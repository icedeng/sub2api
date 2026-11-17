//go:build unit

package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type toolOutputCallIDUpstream struct {
	service.HTTPUpstream
	bodies [][]byte
}

func TestOpenAIResponses_NormalizesCodexAmbientBrowserContext(t *testing.T) {
	for _, withHistory := range []bool{false, true} {
		name := "first request"
		if withHistory {
			name = "with real tool history"
		}
		t.Run(name, func(t *testing.T) {
			items := []any{pageContextItem("browser_tabs", codexBrowserTabsAmbientContext), pageContextItem("codex_apps_open_page", `{"page_id":null}`)}
			if withHistory {
				items = append(items,
					map[string]any{"type": "function_call", "call_id": "call_real", "name": "run", "arguments": "{}"},
					map[string]any{"type": "function_call_output", "call_id": "call_real", "output": "done"})
			}
			body, err := json.Marshal(map[string]any{"model": "gpt-5.1", "stream": false, "input": items})
			require.NoError(t, err)
			upstream := &toolOutputCallIDUpstream{}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream)
			c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
			c.Request = httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(string(body)))
			c.Request.Header.Set("Content-Type", "application/json")
			h.Responses(c)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Len(t, upstream.bodies, 1)
			var ambientMessages, calls, outputs int
			for _, item := range gjson.GetBytes(upstream.bodies[0], "input").Array() {
				if strings.Contains(item.Get("content.0.text").String(), "<external_browser_tabs>") {
					ambientMessages++
					require.Equal(t, "user", item.Get("role").String())
					require.Contains(t, item.Get("content.0.text").String(), codexBrowserTabsAmbientContext)
				}
				if item.Get("type").String() == "function_call" {
					calls++
				}
				if item.Get("type").String() == "function_call_output" {
					outputs++
					require.NotEmpty(t, item.Get("call_id").String())
				}
			}
			require.Equal(t, 1, ambientMessages)
			if withHistory {
				require.Equal(t, 1, calls)
				require.Equal(t, 1, outputs)
			} else {
				require.Zero(t, calls)
				require.Zero(t, outputs)
			}
		})
	}
}

func TestOpenAIResponses_NormalizesAdditionalCodexContext(t *testing.T) {
	for _, name := range []string{"browser_info", "goal_objective", "untrusted_context"} {
		for _, withHistory := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/history=%v", name, withHistory), func(t *testing.T) {
				items := []any{pageContextItem(name, "external context")}
				if withHistory {
					items = append(items,
						map[string]any{"type": "function_call", "call_id": "call_real", "name": "run", "arguments": "{}"},
						map[string]any{"type": "function_call_output", "call_id": "call_real", "output": "done"})
				}
				body, err := json.Marshal(map[string]any{"model": "gpt-5.1", "stream": false, "input": items})
				require.NoError(t, err)
				upstream := &toolOutputCallIDUpstream{}
				h := newOpenAIResponsesFailoverTestHandler(t, upstream)
				c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Len(t, upstream.bodies, 1)
				item := gjson.GetBytes(upstream.bodies[0], "input.0")
				require.Equal(t, "user", item.Get("role").String())
				require.Equal(t, "<external_"+name+">external context</external_"+name+">", item.Get("content.0.text").String())
				require.False(t, item.Get("call_id").Exists())
			})
		}
	}
}

func (u *toolOutputCallIDUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.bodies = append(u.bodies, body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}, nil
}

func (u *toolOutputCallIDUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

// 直接走 Responses 入口，确认补齐后的正文确实交给上游，歧义请求不产生上游调用。
func TestOpenAIResponses_NormalizesToolOutputCallIDsBeforeForward(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		status      int
	}{
		{name: "sequential full history", input: `[{"type":"function_call","call_id":"a","name":"run","arguments":"{}"},{"type":"function_call_output","output":"first"},{"type":"function_call","call_id":"b","name":"run","arguments":"{}"},{"type":"function_call_output","output":"second"}]`, status: http.StatusOK},
		{name: "ambiguous concurrent calls", input: `[{"type":"function_call","call_id":"a"},{"type":"function_call","call_id":"b"},{"type":"function_call_output","output":"unknown"}]`, status: http.StatusBadRequest},
		{name: "missing context", input: `[{"type":"function_call_output","output":"orphan"}]`, status: http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &toolOutputCallIDUpstream{}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream)
			c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.1","stream":false,"input":`+tt.input+`}`))
			c.Request.Header.Set("Content-Type", "application/json")
			h.Responses(c)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			if tt.status != http.StatusOK {
				require.Empty(t, upstream.bodies)
				return
			}
			require.Len(t, upstream.bodies, 1)
			first := gjson.GetBytes(upstream.bodies[0], "input.0.call_id").String()
			second := gjson.GetBytes(upstream.bodies[0], "input.2.call_id").String()
			require.NotEmpty(t, first)
			require.NotEmpty(t, second)
			require.NotEqual(t, first, second)
			require.Equal(t, first, gjson.GetBytes(upstream.bodies[0], "input.1.call_id").String())
			require.Equal(t, second, gjson.GetBytes(upstream.bodies[0], "input.3.call_id").String())
		})
	}
}

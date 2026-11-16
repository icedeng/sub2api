package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type httpBridgeInterruptUpstream struct {
	mu           sync.Mutex
	bodies       [][]byte
	firstBody    *httpBridgeIsolationBody
	firstRequest context.Context
}

func (u *httpBridgeInterruptUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bodies = append(u.bodies, body)
	var response io.ReadCloser
	if len(u.bodies) == 1 {
		prefix := `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","created_at":123,"status":"in_progress"}}` + "\n\n" +
			`data: {"type":"response.output_item.done","sequence_number":1,"item":{"type":"message","id":"msg_done","role":"assistant","content":[{"type":"output_text","text":"finished item"}]}}` + "\n\n" +
			`data: {"type":"response.output_item.done","sequence_number":2,"item":{"type":"message","id":"msg_partial","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"partial item"}]}}` + "\n\n" +
			`data: {"type":"response.output_text.delta","sequence_number":3,"delta":"partial"}` + "\n\n"
		u.firstRequest = req.Context()
		u.firstBody = &httpBridgeIsolationBody{ctx: req.Context(), prefix: bytes.NewReader([]byte(prefix)), suffix: bytes.NewReader(nil), release: make(chan struct{}), closed: make(chan struct{})}
		response = u.firstBody
	} else {
		response = io.NopCloser(strings.NewReader(fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n", len(u.bodies))))
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: response}, nil
}
func (u *httpBridgeInterruptUpstream) DoWithTLS(req *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, id, n)
}

func TestOpenAIWSHTTPBridgeInterruptAndResume(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 2
	upstream := &httpBridgeInterruptUpstream{}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeHTTPBridge}}
	results := make(chan *OpenAIForwardResult, 3)
	errs := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			errs <- err
			return
		}
		defer conn.CloseNow()
		_, first, err := conn.Read(ctx)
		if err != nil {
			errs <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.Clone(ctx)
		errs <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "test", first, &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
			if err == nil {
				results <- result
			}
		}})
	}))
	defer server.Close()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	send := func(payload string) {
		t.Helper()
		require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(payload)))
	}
	read := func(typ string) []byte {
		t.Helper()
		_, msg, err := conn.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, typ, gjson.GetBytes(msg, "type").String(), string(msg))
		return msg
	}
	send(`{"type":"response.create","model":"gpt-5","input":"first"}`)
	read("response.created")
	read("response.output_item.done")
	read("response.output_item.done")
	read("response.output_text.delta")
	send(`{"type":"response.interrupt","response_id":"resp_wrong","mode":"discard_partial_items"}`)
	read("response.interrupt.failed")
	send(`{"type":"response.interrupt","response_id":"resp_1","mode":"unknown"}`)
	read("response.interrupt.failed")
	upstream.mu.Lock()
	requestCtx := upstream.firstRequest
	upstream.mu.Unlock()
	require.NoError(t, requestCtx.Err())
	send(`{"type":"response.interrupt","response_id":"resp_1","mode":"discard_partial_items"}`)
	read("response.interrupt.accepted")
	interrupted := read("response.incomplete")
	require.Equal(t, "interrupted", gjson.GetBytes(interrupted, "response.incomplete_details.reason").String())
	require.Equal(t, int64(4), gjson.GetBytes(interrupted, "sequence_number").Int())
	require.Equal(t, "finished item", gjson.GetBytes(interrupted, "response.output.0.content.0.text").String())
	require.NotContains(t, gjson.GetBytes(interrupted, "response.output").Raw, "partial")
	select {
	case result := <-results:
		require.True(t, result.ClientInterrupted)
		require.Equal(t, "response.incomplete", result.UpstreamTerminalEvent)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.ErrorIs(t, requestCtx.Err(), context.Canceled)
	upstream.mu.Lock()
	closed := upstream.firstBody.closed
	upstream.mu.Unlock()
	select {
	case <-closed:
	default:
		t.Fatal("中断未关闭上游响应体")
	}
	send(`{"type":"response.create","model":"gpt-5","previous_response_id":"resp_1","input":[{"role":"user","content":"continue"}]}`)
	read("response.completed")
	// 终态之后迟到的中断只确认，不产生第二个终态，也不影响后续轮次。
	send(`{"type":"response.interrupt","response_id":"resp_2","mode":"discard_partial_items"}`)
	read("response.interrupt.accepted")
	send(`{"type":"response.create","model":"gpt-5","input":"new root"}`)
	read("response.completed")
	require.NoError(t, conn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-errs:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Len(t, upstream.bodies, 3)
	require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
	require.Equal(t, "finished item", gjson.GetBytes(upstream.bodies[1], "input.1.content.0.text").String())
	require.NotContains(t, string(upstream.bodies[1]), "partial")
}

func TestOpenAIWSHTTPBridgeTurnPanicReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// 使用未就绪的读取结果，确保此测试只验证转发 goroutine 的异常归还。
	reader := &openAIWSClientReader{pending: make(chan openAIWSClientReadResult)}
	result, err := runOpenAIWSHTTPBridgeControlledTurn(ctx, nil, reader, "gpt-5", func([]byte) error { return nil },
		func(context.Context, func([]byte) error) (*OpenAIForwardResult, error) { panic("test panic") })
	require.Nil(t, result)
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusInternalError, closeErr.StatusCode())
	require.NotContains(t, err.Error(), "test panic", "不要把内部异常正文发给客户端")
}

func TestOpenAIWSHTTPBridgeAccountContextGuardRouting(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	svc := &OpenAIGatewayService{cfg: cfg}
	apiKey := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeHTTPBridge}}
	require.True(t, svc.IsOpenAIWSHTTPBridgeAccount(apiKey))
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = false
	require.False(t, svc.IsOpenAIWSHTTPBridgeAccount(apiKey))
	require.True(t, svc.IsOpenAIWSHTTPBridgeAccount(&Account{Platform: PlatformGrok}))
	require.False(t, svc.IsOpenAIWSHTTPBridgeAccount(nil))
	manager := &PluginManager{}
	manager.route.Store(&pluginRoute{pluginID: 1, rolloutPercent: 100, unavailable: "test"})
	svc.pluginManager = manager
	require.True(t, svc.IsOpenAIWSHTTPBridgeAccount(&Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}))
	require.False(t, svc.IsOpenAIWSHTTPBridgeAccount(apiKey))
}

func TestOpenAIWSHTTPBridgeContextGuardBeforeUpstream(t *testing.T) {
	tests := []struct {
		name, first, second, code string
		wantRequests              int
	}{
		{name: "reconnect rejects text delta", first: `{"type":"response.create","model":"gpt-5","previous_response_id":"resp_unknown","input":"continue"}`, code: "previous_response_not_found"},
		{name: "reconnect rejects orphan tool output", first: `{"type":"response.create","model":"gpt-5","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`, code: "tool_call_context_missing"},
		{name: "stale response cannot use latest history", first: `{"type":"response.create","model":"gpt-5","input":"first"}`, second: `{"type":"response.create","model":"gpt-5","previous_response_id":"resp_other","input":"continue"}`, code: "previous_response_not_found", wantRequests: 1},
		{name: "thread switch rejects previous context", first: `{"type":"response.create","model":"gpt-5","client_metadata":{"thread_id":"thread_a"},"input":"first"}`, second: `{"type":"response.create","model":"gpt-5","client_metadata":{"thread_id":"thread_b"},"previous_response_id":"resp_1","input":"continue"}`, code: "previous_response_not_found", wantRequests: 1},
		{name: "full reconnect input accepted", first: `{"type":"response.create","model":"gpt-5","input":[{"role":"user","content":"first"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`, wantRequests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 2
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n"))}}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeHTTPBridge}}
			errs := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					errs <- err
					return
				}
				defer conn.CloseNow()
				_, first, err := conn.Read(ctx)
				if err != nil {
					errs <- err
					return
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r.Clone(ctx)
				errs <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "test", first, nil)
			}))
			defer server.Close()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer conn.CloseNow()
			require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(tt.first)))
			if tt.second != "" {
				_, message, err := conn.Read(ctx)
				require.NoError(t, err)
				require.Equal(t, "response.completed", gjson.GetBytes(message, "type").String())
				require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(tt.second)))
			}
			_, message, err := conn.Read(ctx)
			require.NoError(t, err)
			if tt.code != "" {
				require.Equal(t, tt.code, gjson.GetBytes(message, "error.code").String())
			} else {
				require.Equal(t, "response.completed", gjson.GetBytes(message, "type").String())
				require.NoError(t, conn.Close(coderws.StatusNormalClosure, "done"))
			}
			select {
			case err := <-errs:
				if tt.code != "" {
					require.True(t, IsOpenAIWSHTTPBridgeContextError(err))
				} else {
					require.NoError(t, err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.Len(t, upstream.requests, tt.wantRequests, "无效上下文不得发送上游请求")
		})
	}
}

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSHTTPBridgeContinuationContext(t *testing.T) {
	history := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"inspect"}`),
		json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"checking"}]}`),
		json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"inspect","arguments":"{}"}`),
	}
	tests := []struct {
		name, payload, previous, wantCode string
		history                           []json.RawMessage
		wantCall                          string
	}{
		{name: "missing call id matched uniquely", previous: "resp_1", history: history, wantCall: "call_1", payload: `{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"function_call_output","output":"ok"}]}`},
		{name: "explicit call id replay", previous: "resp_1", history: history, wantCall: "call_1", payload: `{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`},
		{name: "omitted previous response", previous: "resp_1", history: history, wantCall: "call_1", payload: `{"type":"response.create","input":[{"type":"function_call_output","output":"ok"}]}`},
		{name: "unknown response", previous: "resp_1", history: history, wantCode: "previous_response_not_found", payload: `{"type":"response.create","previous_response_id":"resp_other","input":[{"role":"user","content":"continue"}]}`},
		{name: "reconnect delta", wantCode: "previous_response_not_found", payload: `{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`},
		{name: "reconnect text delta", wantCode: "previous_response_not_found", payload: `{"type":"response.create","previous_response_id":"resp_1","input":"continue"}`},
		{name: "orphan result", wantCode: "tool_call_context_missing", payload: `{"type":"response.create","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`},
		{name: "partial coverage", wantCode: "tool_call_context_missing", payload: `{"type":"response.create","input":[{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"call_other","output":"ok"}]}`},
		{name: "ambiguous missing id", previous: "resp_1", history: append(append([]json.RawMessage(nil), history...), json.RawMessage(`{"type":"function_call","call_id":"call_2","name":"inspect","arguments":"{}"}`)), wantCode: "tool_call_context_missing", payload: `{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"function_call_output","output":"ok"}]}`},
		{name: "already answered", previous: "resp_1", history: append(append([]json.RawMessage(nil), history...), json.RawMessage(`{"type":"function_call_output","call_id":"call_1","output":"first"}`)), wantCode: "tool_call_context_missing", payload: `{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"function_call_output","output":"again"}]}`},
		{name: "unresolved reference", wantCode: "tool_call_context_missing", payload: `{"type":"response.create","input":[{"type":"item_reference","id":"call_1"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`},
		{name: "duplicate association", wantCode: "invalid_request", payload: `{"type":"response.create","input":[{"type":"function_call_output","call_id":"call_1","call_id":"other","output":"ok"}]}`},
		{name: "invalid call id type", wantCode: "invalid_request", payload: `{"type":"response.create","input":[{"type":"function_call_output","call_id":42,"output":"ok"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := json.Marshal(tt.history)
			body, input, exists, err := prepareOpenAIWSHTTPBridgeContinuation([]byte(tt.payload), tt.previous, tt.history, len(tt.history) > 0)
			if tt.wantCode != "" {
				var contextErr *openAIWSHTTPBridgeContextError
				require.ErrorAs(t, err, &contextErr)
				require.True(t, IsOpenAIWSHTTPBridgeContextError(err))
				require.Equal(t, tt.wantCode, contextErr.code)
				require.Nil(t, body)
			} else {
				require.NoError(t, err)
				require.True(t, exists)
				require.Len(t, input, 4)
				require.Equal(t, tt.wantCall, gjson.GetBytes(body, "input.3.call_id").String())
				require.Equal(t, "checking", gjson.GetBytes(body, "input.1.content.0.text").String())
				require.True(t, openAIWSRawItemsHaveToolCallContextForOutputs(input))
			}
			after, _ := json.Marshal(tt.history)
			require.Equal(t, before, after, "不能修改共享历史")
		})
	}
}

func TestOpenAIWSHTTPBridgeReconnectFullInput(t *testing.T) {
	payload := []byte(`{"type":"response.create","previous_response_id":null,"input":[{"role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	body, items, exists, err := prepareOpenAIWSHTTPBridgeContinuation(payload, "", nil, false)
	require.NoError(t, err)
	require.True(t, exists)
	require.Len(t, items, 3)
	require.Equal(t, payload, body)
}

func TestOpenAIWSHTTPBridgeReplayedMissingCallID(t *testing.T) {
	for _, previous := range []string{"", "resp_1"} {
		t.Run("previous="+previous, func(t *testing.T) {
			payload := []byte(`{"type":"response.create","input":[{"role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","output":"ok"}]}`)
			_, history, exists, err := prepareOpenAIWSHTTPBridgeContinuation(payload, "", nil, false)
			require.NoError(t, err)
			require.True(t, exists)
			before, err := json.Marshal(history)
			require.NoError(t, err)
			var replay map[string]any
			require.NoError(t, json.Unmarshal(payload, &replay))
			replay["previous_response_id"] = previous
			replay["input"] = append(replay["input"].([]any), map[string]any{"role": "user", "content": "continue"})
			body, err := json.Marshal(replay)
			require.NoError(t, err)
			original := append([]byte(nil), body...)
			updated, items, exists, err := prepareOpenAIWSHTTPBridgeContinuation(body, "resp_1", history, true)
			require.NoError(t, err)
			require.True(t, exists)
			require.Len(t, items, 4, "历史回放不能重复拼接")
			require.Equal(t, "call_1", gjson.GetBytes(updated, "input.2.call_id").String())
			require.Equal(t, "continue", gjson.GetBytes(updated, "input.3.content").String())
			after, err := json.Marshal(history)
			require.NoError(t, err)
			require.Equal(t, before, after, "不能修改共享历史")
			require.Equal(t, original, body, "不能修改原始输入")
		})
	}
}

func TestOpenAIWSHTTPBridgeReplayCallIDGuards(t *testing.T) {
	history := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"inspect"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call_1","output":"ok"}`),
	}
	pending := json.RawMessage(`{"type":"function_call","call_id":"call_2","name":"inspect","arguments":"{}"}`)
	tests := []struct {
		name, input string
		pending     bool
		wantIDs     []string
		wantError   bool
	}{
		{name: "replay plus new result with identical text", pending: true,
			input: `[{"role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","output":"ok"},{"type":"function_call","call_id":"call_2","name":"inspect","arguments":"{}"},{"type":"function_call_output","output":"ok"}]`, wantIDs: []string{"call_1", "call_2"}},
		{name: "same text alone belongs to pending call", pending: true,
			input: `[{"type":"function_call_output","output":"ok"}]`, wantIDs: []string{"call_1", "call_2"}},
		{name: "same text alone cannot reopen completed call",
			input: `[{"type":"function_call_output","output":"ok"}]`, wantError: true},
		{name: "changed output is not a replay",
			input: `[{"role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","output":"different"}]`, wantError: true},
		{name: "changed arguments are not a replay",
			input: `[{"role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{\"changed\":true}"},{"type":"function_call_output","output":"ok"}]`, wantError: true},
		{name: "changed earlier message is not a replay",
			input: `[{"role":"user","content":"different"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","output":"ok"}]`, wantError: true},
		{name: "explicit result and replayed result",
			input: `[{"role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","output":"ok"},{"type":"function_call","call_id":"call_2","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"call_2","output":"new"}]`, wantIDs: []string{"call_1", "call_2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := append([]json.RawMessage(nil), history...)
			if tt.pending {
				stored = append(stored, pending)
			}
			before, err := json.Marshal(stored)
			require.NoError(t, err)
			_, items, _, err := prepareOpenAIWSHTTPBridgeContinuation([]byte(`{"type":"response.create","input":`+tt.input+`}`), "resp_1", stored, true)
			if tt.wantError {
				var contextErr *openAIWSHTTPBridgeContextError
				require.ErrorAs(t, err, &contextErr)
				require.Equal(t, "tool_call_context_missing", contextErr.code)
			} else {
				require.NoError(t, err)
				var ids []string
				for _, raw := range items {
					if isCodexToolCallOutputItemType(gjson.GetBytes(raw, "type").String()) {
						ids = append(ids, gjson.GetBytes(raw, "call_id").String())
					}
				}
				require.Equal(t, tt.wantIDs, ids)
			}
			after, err := json.Marshal(stored)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestOpenAIWSHTTPBridgeReplayedMissingCallIDAcrossTurns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	upstream := &httpUpstreamRecorder{}
	for _, id := range []string{"resp_1", "resp_2", "resp_3"} {
		upstream.responses = append(upstream.responses, &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n")),
		})
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 2
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeHTTPBridge}}
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
	input := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"inspect"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","output":"ok"}`),
	}
	for turn, id := range []string{"resp_1", "resp_2", "resp_3"} {
		if turn > 0 {
			input = append(input, json.RawMessage(`{"role":"user","content":"continue"}`))
		}
		payload, err := json.Marshal(map[string]any{"type": "response.create", "model": "gpt-5", "input": input})
		require.NoError(t, err)
		require.NoError(t, conn.Write(ctx, coderws.MessageText, payload))
		_, event, err := conn.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String(), string(event))
		require.Equal(t, id, gjson.GetBytes(event, "response.id").String())
	}
	require.NoError(t, conn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-errs:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("等待桥接结束超时")
	}
	require.Len(t, upstream.bodies, 3, "每轮只能转发一次")
	for turn, body := range upstream.bodies {
		require.Len(t, gjson.GetBytes(body, "input").Array(), turn+3)
		require.Equal(t, "call_1", gjson.GetBytes(body, "input.2.call_id").String())
	}
}

// 完整历史中的后续调用尚未发生，不能拿来让前一轮结果产生歧义。
func TestOpenAIWSHTTPBridgeSequentialMissingCallIDs(t *testing.T) {
	payload := []byte(`{"type":"response.create","model":"gpt-5","input":[{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"},{"type":"function_call_output","output":"first"},{"type":"function_call","call_id":"call_2","name":"inspect","arguments":"{}"},{"type":"function_call_output","output":"second"}]}`)
	original := append([]byte(nil), payload...)
	body, items, exists, err := prepareOpenAIWSHTTPBridgeContinuation(payload, "", nil, false)
	require.NoError(t, err)
	require.True(t, exists)
	require.Len(t, items, 4)
	require.Equal(t, "call_1", gjson.GetBytes(body, "input.1.call_id").String())
	require.Equal(t, "call_2", gjson.GetBytes(body, "input.3.call_id").String())
	require.Equal(t, original, payload, "不能修改客户端原始输入")
}

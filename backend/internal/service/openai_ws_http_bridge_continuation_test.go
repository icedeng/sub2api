package service

import (
	"encoding/json"
	"testing"

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

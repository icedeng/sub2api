package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAIToolOutputCallIDs(t *testing.T) {
	for _, tt := range []struct {
		name, payload string
		wantIDs       map[string]string
		wantError     bool
	}{
		{name: "sequential calls in reconnect history", payload: `{"input":[{"type":"function_call","call_id":"call_1","name":"run","arguments":"{}"},{"type":"function_call_output","output":"ok"},{"type":"function_call","call_id":"call_2","name":"run","arguments":"{}"},{"type":"function_call_output","output":"ok"}],"metadata":{"number":9007199254740993}}`, wantIDs: map[string]string{"input.1.call_id": "call_1", "input.3.call_id": "call_2"}},
		{name: "names distinguish concurrent calls", payload: `{"input":[{"type":"function_call","call_id":"a","name":"read"},{"type":"function_call","call_id":"b","name":"write"},{"type":"function_call_output","name":"write","output":"ok"}]}`, wantIDs: map[string]string{"input.2.call_id": "b"}},
		{name: "custom tool output keeps type", payload: `{"input":[{"type":"custom_tool_call","call_id":"custom","name":"exec"},{"type":"custom_tool_call_output","output":"ok"}]}`, wantIDs: map[string]string{"input.1.call_id": "custom"}},
		{name: "explicit result reserves its call", payload: `{"input":[{"type":"function_call","call_id":"a"},{"type":"function_call","call_id":"b"},{"type":"function_call_output","call_id":"b","output":"explicit"},{"type":"function_call_output","output":"remaining"}]}`, wantIDs: map[string]string{"input.2.call_id": "b", "input.3.call_id": "a"}},
		{name: "concurrent anonymous results are ambiguous", payload: `{"input":[{"type":"function_call","call_id":"a"},{"type":"function_call","call_id":"b"},{"type":"function_call_output","output":"one"},{"type":"function_call_output","output":"two"}]}`, wantError: true},
		{name: "future call cannot supply earlier result", payload: `{"input":[{"type":"function_call_output","output":"orphan"},{"type":"function_call","call_id":"future"}]}`, wantError: true},
		{name: "completed call cannot be reopened", payload: `{"input":[{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":"a","output":"done"},{"type":"function_call_output","output":"again"}]}`, wantError: true},
		{name: "conflicting call definitions", payload: `{"input":[{"type":"function_call","call_id":"a","name":"read"},{"type":"function_call_output","output":"ok"},{"type":"function_call","call_id":"a","name":"write"}]}`, wantError: true},
		{name: "different namespace cannot match", payload: `{"input":[{"type":"function_call","call_id":"a","namespace":"one"},{"type":"function_call_output","namespace":"two","output":"ok"}]}`, wantError: true},
		{name: "duplicate association fields", payload: `{"input":[{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":"","call_id":"b","output":"ok"}]}`, wantError: true},
		{name: "invalid call id type", payload: `{"input":[{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":null,"output":"ok"}]}`, wantError: true},
		{name: "owned continuation remains unchanged", payload: `{"previous_response_id":"resp_prior","input":[{"type":"function_call","call_id":"a"},{"type":"function_call_output","output":"ok"}]}`},
		{name: "orphan left to existing HTTP validation", payload: `{"input":[{"type":"function_call_output","output":"orphan"}]}`},
		{name: "explicit IDs remain byte identical", payload: `{"input": [{"type":"function_call","call_id":"a"}, {"type":"function_call_output","call_id":"a","output":"ok"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(tt.payload)
			original := append([]byte(nil), payload...)
			got, changed, err := NormalizeOpenAIToolOutputCallIDs(payload)
			require.Equal(t, original, payload, "输入正文保持不可变")
			if tt.wantError {
				require.Error(t, err)
				require.Nil(t, got)
				require.False(t, changed)
				return
			}
			require.NoError(t, err)
			require.Equal(t, len(tt.wantIDs) > 0, changed)
			if !changed {
				require.Equal(t, payload, got)
			}
			for path, id := range tt.wantIDs {
				require.Equal(t, id, gjson.GetBytes(got, path).String())
			}
			require.Equal(t, gjson.GetBytes(payload, "metadata").Raw, gjson.GetBytes(got, "metadata").Raw)
			again, changedAgain, err := NormalizeOpenAIToolOutputCallIDs(got)
			require.NoError(t, err)
			require.False(t, changedAgain)
			require.Equal(t, got, again)
		})
	}
}

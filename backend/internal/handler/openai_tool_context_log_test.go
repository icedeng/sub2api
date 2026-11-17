package handler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func TestOpenAIToolContextLogFieldsRedactsContentAndBoundsDetails(t *testing.T) {
	items := []string{`{"type":"function_call","call_id":"private-call-id","name":"private-tool-name","arguments":"private-arguments"}`, `{"type":"private-type","content":"private-content"}`}
	for i := 0; i < 20; i++ {
		items = append(items, `{"type":"function_call_output","namespace":"private-namespace","name":"private-name","output":"private-output"}`)
	}
	body := []byte(`{"previous_response_id":"private-response-id","input":[` + strings.Join(items, ",") + `]}`)
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range openAIToolContextLogFields(body) {
		f.AddTo(enc)
	}
	require.EqualValues(t, 22, enc.Fields["input_item_count"])
	require.EqualValues(t, 20, enc.Fields["missing_tool_output_count"])
	require.True(t, enc.Fields["has_previous_response_id"].(bool))
	text := fmt.Sprint(enc.Fields)
	require.NotContains(t, text, "private-")
	require.Contains(t, text, "other:")
	require.Contains(t, text, "other:1")
	missing := enc.Fields["missing_tool_outputs"].([]map[string]any)
	require.Len(t, missing, 16)
	require.Equal(t, 2, missing[0]["index"])
	require.Equal(t, "absent", missing[0]["call_id_state"])
}

func TestOpenAIToolContextLogFieldsKnownContext(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call_output","namespace":"agent","name":"browser_info","call_id":" ","output":[]},{"type":"function_call_output","name":"goal_objective","call_id":null,"output":{}},{"type":"function_call_output","call_id":"real","output":"do not log"}]}`)
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range openAIToolContextLogFields(body) {
		f.AddTo(enc)
	}
	require.EqualValues(t, 2, enc.Fields["missing_tool_output_count"])
	missing := enc.Fields["missing_tool_outputs"].([]map[string]any)
	require.Equal(t, "browser_info", missing[0]["name"])
	require.Equal(t, "agent", missing[0]["namespace"])
	require.Equal(t, "blank", missing[0]["call_id_state"])
	require.Equal(t, "array", missing[0]["output_shape"])
	require.Equal(t, "invalid_type", missing[1]["call_id_state"])
	require.Equal(t, "object", missing[1]["output_shape"])
}

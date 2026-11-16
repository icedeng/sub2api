package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeCodexWSClientInput(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{"delegation", []byte(`{"type":"response.create","model":"gpt-5","input":[{"type":"function_call_output","namespace":"codex_app","name":"create_thread","output":"` + delegationEnvelope + `"}]}`)},
		{"automation", codexAutomationBootstrapBody(t, codexAutomationBootstrap("test", "never", "test"), "")},
		{"page", []byte(`{"type":"response.create","model":"gpt-5","input":[{"type":"function_call_output","namespace":"agent","name":"codex_apps_open_page","output":"{\"page_id\":\"page-1\"}"}]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCodexWSClientInput(tt.payload)
			require.Equal(t, "message", gjson.GetBytes(got, "input.0.type").String())
			require.Equal(t, "user", gjson.GetBytes(got, "input.0.role").String())
			require.Equal(t, got, normalizeCodexWSClientInput(got))
		})
	}
	ordinary := []byte(`{"type":"response.create","input":[{"type":"function_call_output","output":"must not be converted"}]}`)
	require.Equal(t, ordinary, normalizeCodexWSClientInput(ordinary))
}

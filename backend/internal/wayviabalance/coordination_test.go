package wayviabalance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRetryAfterPreservesLongCooldownAndRejectsOverflow(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		value string
		want  time.Time
	}{
		{"90", now.Add(90 * time.Second)},
		{"63072000", now.Add(2 * 365 * 24 * time.Hour)},
		{"Mon, 16 Nov 2026 00:03:00 GMT", now.Add(3 * time.Minute)},
		{"9223372036854775807", time.Time{}},
		{"-1", time.Time{}},
		{"+60", time.Time{}},
		{"garbage", time.Time{}},
	} {
		t.Run(tt.value, func(t *testing.T) {
			require.Equal(t, tt.want, retryAfter(tt.value, now))
		})
	}
}

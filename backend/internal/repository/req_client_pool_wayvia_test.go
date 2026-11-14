package repository

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/wayviaegress"
	"github.com/stretchr/testify/require"
)

func TestGetSharedReqClient_StrictTLSCannotBypassControlledDial(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()
	for _, force := range []bool{false, true} {
		for _, fingerprint := range []bool{false, true} {
			route := wayviaegress.Route{Mode: wayviaegress.Strict, Source: netip.MustParseAddr("2606:4700:abcd:1234::42")}
			client, err := getSharedReqClient(reqClientOptions{Route: &route, Timeout: time.Second, ForceHTTP2: force, Impersonate: fingerprint})
			require.NoError(t, err)
			require.NotNil(t, client.GetTransport().DialTLSContext)
			_, err = client.R().Get(srv.URL)
			require.ErrorContains(t, err, "目标IPv6不允许")
			client.GetTransport().CloseIdleConnections()
		}
	}
	require.Zero(t, requests.Load())
}

package wayviaegress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func TestConfigProbeURLsCompatibility(t *testing.T) {
	base := Config{Prefix: "2606:4700:abcd:1234::/64", Primary: "2606:4700:abcd:1234::1", NodeID: "node-1", Interface: "ens18", MaxAddresses: 1024, AppUID: 1000, AppGID: 1000, Socket: "/run/wayvia/agent.sock", StateDir: "/var/lib/wayvia/app", AgentStateDir: "/var/lib/wayvia/agent", KeyFile: "/etc/wayvia/key", KeyID: "v1"}
	for _, tc := range []struct {
		name   string
		legacy string
		urls   []string
		want   []string
		valid  bool
	}{
		{"legacy", "https://old.example/ipv6", nil, []string{"https://old.example/ipv6"}, true},
		{"ordered_list", "", []string{"https://first.example", "https://second.example"}, []string{"https://first.example", "https://second.example"}, true},
		{"list_takes_precedence", "https://old.example", []string{"https://first.example"}, []string{"https://first.example"}, true},
		{"missing", "", nil, nil, false},
		{"http", "", []string{"http://first.example"}, nil, false},
		{"credentials", "", []string{"https://user:secret@first.example"}, nil, false},
		{"port", "", []string{"https://first.example:8443"}, nil, false},
		{"invalid_second", "", []string{"https://first.example", ""}, nil, false},
		{"duplicate", "", []string{"https://first.example", "https://first.example"}, nil, false},
		{"too_many", "", make([]string, 9), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.ProbeURL, c.ProbeURLs = tc.legacy, tc.urls
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("validation=%v, want valid=%v", err, tc.valid)
			}
			if tc.valid && !reflect.DeepEqual(c.probeURLs(), tc.want) {
				t.Fatalf("URLs=%v, want %v", c.probeURLs(), tc.want)
			}
		})
	}
}

func TestProbeFallbackAndFailClosed(t *testing.T) {
	source := netip.MustParseAddr("2606:4700:abcd:1234::42")
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		err        error
		wantReason string
		wantCalls  int
	}{
		{"connection_failure", 0, "", errors.New("connection refused"), "", 2},
		{"server_failure", 503, "unavailable", nil, "", 2},
		{"invalid_body", 200, "<html>unavailable</html>", nil, "", 2},
		{"oversized_body", 200, strings.Repeat("x", 257), nil, "", 2},
		{"address_mismatch", 200, "2606:4700:abcd:1234::43", nil, "EGRESS_SOURCE_CONFLICT", 1},
		{"ipv4_response", 200, "192.0.2.1", nil, "EGRESS_SOURCE_CONFLICT", 1},
		{"unsafe_destination", 0, "", Error("EGRESS_DESTINATION_DENIED", "denied"), "EGRESS_DESTINATION_DENIED", 1},
		{"local_source_conflict", 0, "", Error("EGRESS_SOURCE_CONFLICT", "wrong source"), "EGRESS_SOURCE_CONFLICT", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hosts []string
			client := newProbeClient(rtFunc(func(req *http.Request) (*http.Response, error) {
				hosts = append(hosts, req.URL.Host)
				if req.Method != http.MethodGet || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
					t.Fatal("probe carried credentials or used wrong method")
				}
				if len(hosts) == 1 {
					if tc.err != nil {
						return nil, tc.err
					}
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(source.String() + "\n")), Header: make(http.Header)}, nil
			}), 2)
			observed, err := probeEndpoints(context.Background(), []string{"https://first.example", "https://second.example", "https://unused.example"}, source, client)
			if apperrors.Reason(err) != tc.wantReason || len(hosts) != tc.wantCalls {
				t.Fatalf("observed=%s err=%v hosts=%v", observed, err, hosts)
			}
			if hosts[0] != "first.example" || tc.wantCalls == 2 && hosts[1] != "second.example" {
				t.Fatalf("wrong configured order: %v", hosts)
			}
			if tc.wantReason == "" && observed != source.String() {
				t.Fatalf("wrong observed address: %s", observed)
			}
		})
	}
}

func TestProbeRedirectNotFollowed(t *testing.T) {
	var hosts []string
	source := netip.MustParseAddr("2606:4700:abcd:1234::42")
	client := newProbeClient(rtFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		if req.URL.Host == "first.example" {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://redirect.example"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(source.String()))}, nil
	}), 2)
	if _, err := probeEndpoints(context.Background(), []string{"https://first.example", "https://second.example"}, source, client); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hosts, []string{"first.example", "second.example"}) {
		t.Fatalf("redirect followed or wrong fallback: %v", hosts)
	}
}

func TestProbeCancellationAndExhaustion(t *testing.T) {
	source := netip.MustParseAddr("2606:4700:abcd:1234::42")
	urls := []string{"https://first.example", "https://second.example"}
	calls := 0
	client := newProbeClient(rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("offline")
	}), len(urls))
	if _, err := probeEndpoints(context.Background(), urls, source, client); apperrors.Reason(err) != "EGRESS_PROBE_FAILED" || calls != 2 {
		t.Fatalf("exhaustion err=%v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	if _, err := probeEndpoints(ctx, urls, source, client); err == nil || calls != 0 {
		t.Fatalf("canceled probe still sent requests: err=%v calls=%d", err, calls)
	}
	if client.Timeout != 5*time.Second || newProbeClient(client.Transport, 1).Timeout != 15*time.Second {
		t.Fatal("per-endpoint timeout or legacy timeout changed")
	}
	client.Timeout = 20 * time.Millisecond
	calls = 0
	client.Transport = rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(source.String()))}, nil
	})
	if _, err := probeEndpoints(context.Background(), urls, source, client); err != nil || calls != 2 {
		t.Fatalf("timeout did not fall back: err=%v calls=%d", err, calls)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	calls = 0
	client.Timeout = time.Second
	client.Transport = rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	if _, err := probeEndpoints(ctx, urls, source, client); err == nil || calls != 1 {
		t.Fatalf("parent timeout started another endpoint: err=%v calls=%d", err, calls)
	}
}

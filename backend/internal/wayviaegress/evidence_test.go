package wayviaegress

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/imroc/req/v3"
)

type testEvidenceTransport func(*http.Request) (*http.Response, error)

func (f testEvidenceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type evidenceConn struct{ net.Conn }

func (*evidenceConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("2606:4700:abcd:1234::42"), Port: 12345}
}
func (*evidenceConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("2606:4700::1"), Port: 443}
}
func (*evidenceConn) Close() error { return nil }
func evidenceFixture(t *testing.T) (*Manager, Route, net.Conn) {
	m := fixtureManager(t)
	p := m.policies[1]
	p.Mode = Strict
	p.Target = Strict
	p.State = "ready"
	p.Identity = "user-1"
	p.Source = "2606:4700:abcd:1234::42"
	p.BindingVersion = 7
	m.policies[1] = p
	m.ready[p.Owner] = true
	m.evidenceQueue = make(chan Evidence, 32)
	r, err := m.Resolve(1, "business")
	if err != nil {
		t.Fatal(err)
	}
	c := &trackedConn{Conn: &evidenceConn{}, manager: m, epoch: r.epoch, id: "connection-1"}
	r.epoch.connections[c] = struct{}{}
	// 外层模拟 TLS 封装，仅转发 net.Conn，不依赖 trackedConn 类型断言。
	return m, r, &struct{ net.Conn }{c}
}
func TestEvidenceHTTPReuseSSEAndNoSensitiveURL(t *testing.T) {
	m, r, c := evidenceFixture(t)
	base := testEvidenceTransport(func(req *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(req.Context()).GotConn(httptrace.GotConnInfo{Conn: c, Reused: true})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: fake\n\n"))}, nil
	})
	req, _ := http.NewRequest("POST", "https://example.invalid/private/token?secret=never-record", nil)
	req.Header.Set("Authorization", "Bearer never-record")
	resp, err := WrapTransport(base, r).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	for _, event := range []string{"response_headers", "body_eof"} {
		e := <-m.evidenceQueue
		if e.Source != r.Source.String() || e.ConnectionID != "connection-1" || !e.Reused || e.Event != event || e.Protocol != "SSE" || e.Host != "example.invalid" {
			t.Fatal(e)
		}
	}
	if len(m.evidenceQueue) != 0 {
		t.Fatal("duplicate EOF event")
	}
}
func TestEvidenceWSMessagesUseOriginalHandshakeConnection(t *testing.T) {
	m, r, c := evidenceFixture(t)
	ctx, capture := WithSocketCapture(context.Background())
	base := testEvidenceTransport(func(req *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(req.Context()).GotConn(httptrace.GotConnInfo{Conn: c})
		return &http.Response{StatusCode: 101, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.invalid/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	resp, err := WrapTransport(base, r).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if e := <-m.evidenceQueue; e.Event != "ws_handshake" {
		t.Fatal(e)
	}
	for _, reused := range []bool{false, true} {
		capture.Sent(r, ctx, nil)
		e := <-m.evidenceQueue
		if e.Event != "ws_message_sent" || e.ConnectionID != "connection-1" || e.Source != r.Source.String() || e.Reused != reused {
			t.Fatal(e)
		}
	}
	capture.Sent(r, WithAccount(ctx, 1, "prewarm"), nil)
	if e := <-m.evidenceQueue; e.Purpose != "prewarm" {
		t.Fatal("internal prewarm counted as business", e)
	}

}
func TestEvidenceMissingTraceNeverClaimsConfiguredSource(t *testing.T) {
	m, r, _ := evidenceFixture(t)
	base := testEvidenceTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	req, _ := http.NewRequest("GET", "https://example.invalid", nil)
	resp, err := WrapTransport(base, r).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	e := <-m.evidenceQueue
	if e.Event != "connection_unavailable" || e.Source != "" || e.ConnectionID != "" {
		t.Fatal("fabricated evidence", e)
	}
}

// 使用真实本机 TLS 连接验证标准传输和 Firefox 指纹传输都会提供复用连接信息。
// 测试仅替换拨号中的公网地址限制；生产 Route.Dial 仍强制批准的公网 IPv6。
func TestEvidenceRealTLSAndFingerprintConnectionReuse(t *testing.T) {
	for _, fingerprint := range []bool{false, true} {
		name := "standard"
		if fingerprint {
			name = "fingerprint"
		}
		t.Run(name, func(t *testing.T) {
			m := fixtureManager(t)
			m.evidenceQueue = make(chan Evidence, 16)
			p := m.policies[1]
			p.Mode, p.Target, p.State, p.Source, p.Identity = Strict, Strict, "ready", "::1", "user-1"
			m.policies[1], m.ready[p.Owner] = p, true
			r, err := m.Resolve(1, "business")
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp6", "[::1]:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("test")) }))
			srv.Listener.Close()
			srv.Listener, srv.EnableHTTP2 = listener, true
			srv.StartTLS()
			defer srv.Close()
			dial := func(ctx context.Context, network, address string) (net.Conn, error) {
				c, e := (&net.Dialer{}).DialContext(ctx, network, address)
				if e != nil {
					return nil, e
				}
				tracked := &trackedConn{Conn: c, manager: m, epoch: r.epoch, id: uuid.NewString()}
				m.mu.Lock()
				r.epoch.connections[tracked] = struct{}{}
				m.mu.Unlock()
				return tracked, nil
			}
			var transport http.RoundTripper
			if fingerprint {
				client := req.C().ImpersonateFirefox().EnableForceHTTP2().SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true}).SetDial(dial)
				client.SetDialTLS(TLSDial(dial, client.GetTLSClientConfig(), client.GetTransport().TLSHandshakeContext))
				transport = client.GetTransport()
				defer client.GetTransport().CloseIdleConnections()
			} else {
				tr := &http.Transport{DialContext: dial, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true}
				transport = tr
				defer tr.CloseIdleConnections()
			}
			client := &http.Client{Transport: WrapTransport(transport, r), Timeout: 5 * time.Second}
			connectionID := ""
			for i := 0; i < 2; i++ {
				resp, e := client.Get(srv.URL)
				if e != nil {
					t.Fatal(e)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				evidence := <-m.evidenceQueue
				if evidence.Source != "::1" || evidence.ConnectionID == "" || evidence.Event != "response_headers" {
					t.Fatal(evidence)
				}
				if i == 1 && (!evidence.Reused || evidence.ConnectionID != connectionID) {
					t.Fatal("reused connection was not attributed", evidence)
				}
				connectionID = evidence.ConnectionID
				<-m.evidenceQueue // 响应流结束与响应头共享请求编号。
			}
		})
	}
}
func TestEvidencePersistenceRetentionAndShadowScope(t *testing.T) {
	m := recoveryFixture(t)
	p := m.policies[1]
	if _, err := m.db.Exec(`INSERT INTO accounts(id,platform,type,parent_account_id) VALUES(3,'openai','oauth',1)`); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	batch := make([]Evidence, 205)
	for i := range batch {
		batch[i] = Evidence{Owner: p.Owner, AccountID: 1, RequestID: uuid.NewString(), OccurredAt: time.Now(), Purpose: "business", Event: "response_headers", Protocol: "HTTP", Source: "2606:4700:abcd:1234::42"}
	}
	batch[204].AccountID = 3
	if err := m.persistEvidence(context.Background(), batch, true); err != nil {
		t.Fatal(err)
	}
	var count int
	m.db.QueryRow(`SELECT count(*) FROM wayvia_egress_evidence`).Scan(&count)
	if count != 200 {
		t.Fatal("owner retention", count)
	}
	page, err := m.Evidence(context.Background(), 3, 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].AccountID != 3 {
		t.Fatal("shadow leaked sibling requests", page, err)
	}
	page, err = m.Evidence(context.Background(), 1, 999)
	if err != nil || len(page.Items) != 50 {
		t.Fatal("limit not enforced", len(page.Items), err)
	}
	old := batch[0]
	old.OccurredAt = time.Now().Add(-8 * 24 * time.Hour)
	if err = m.persistEvidence(context.Background(), []Evidence{old}, true); err != nil {
		t.Fatal(err)
	}
	m.db.QueryRow(`SELECT count(*) FROM wayvia_egress_evidence WHERE occurred_at<now()-interval '7 days'`).Scan(&count)
	if count != 0 {
		t.Fatal("expired records retained")
	}
	m.evidenceQueue = make(chan Evidence, 1)
	m.enqueueEvidence(batch[0])
	m.enqueueEvidence(batch[1])
	if m.evidenceDropped.Load() != 1 {
		t.Fatal("bounded queue did not count loss")
	}
}

package wayviaegress

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureManager(t *testing.T) *Manager {
	t.Helper()
	m := &Manager{accounts: map[int64]Account{1: {ID: 1, Platform: "openai", Type: "oauth", Identity: "user-1", Credentials: map[string]any{"refresh_token": "fake-rt", "chatgpt_user_id": "user-1"}}, 2: {ID: 2, Platform: "openai", Type: "apikey"}, 3: {ID: 3, ParentID: 1, Platform: "openai", Type: "oauth"}}, policies: map[int64]Policy{1: {Owner: "11111111-1111-4111-8111-111111111111", AccountID: 1, Mode: Legacy, Target: Legacy, Version: 1, State: "legacy"}}, epochs: map[string]*epoch{}, blocked: map[string]bool{}, ready: map[string]bool{}, runtime: Runtime{Enabled: true, Initialized: true, Revision: 1}}
	old := current.Swap(m)
	t.Cleanup(func() { current.Store(old) })
	return m
}
func TestCandidateStableDistinctAndBounded(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	prefix := netip.MustParsePrefix("2606:4700:abcd:1234::/64")
	a, e := Candidate(key, prefix, "default", "owner-1", 1, 1, 0)
	if e != nil || !prefix.Contains(a) {
		t.Fatal(a, e)
	}
	b, _ := Candidate(key, prefix, "default", "owner-1", 1, 1, 0)
	if a != b {
		t.Fatal("unstable")
	}
	for _, args := range [][3]string{{"other", "owner-1", ""}, {"default", "owner-2", ""}} {
		b, _ = Candidate(key, prefix, args[0], args[1], 1, 1, 0)
		if a == b {
			t.Fatal("owner/pool collision")
		}
	}
	b, _ = Candidate(key, prefix, "default", "owner-1", 1, 2, 0)
	if a == b {
		t.Fatal("rotation reused address")
	}
	if _, e = Candidate(key[:8], prefix, "default", "owner", 1, 1, 0); e == nil {
		t.Fatal("short key accepted")
	}
	if _, e = Candidate(key, prefix, "default", "owner", 1, 1, 64); e == nil {
		t.Fatal("unbounded retry")
	}
}
func TestMissingPolicyFailClosedAPIKeyUnaffected(t *testing.T) {
	m := fixtureManager(t)
	delete(m.policies, 1)
	m.initError = "database unavailable"
	if _, e := m.Resolve(1, "business"); e == nil {
		t.Fatal("missing policy allowed")
	}
	if r, e := m.Resolve(2, "business"); e != nil || r.Strict() || r.Owner != "" {
		t.Fatal(r, e)
	}
	if _, e := m.Resolve(4, "business"); e == nil {
		t.Fatal("unknown account allowed")
	}
}
func TestShadowUsesOwnerAndPausedNeverLegacy(t *testing.T) {
	m := fixtureManager(t)
	p := m.policies[1]
	p.Mode = Strict
	p.Target = Strict
	p.State = "ready"
	p.Source = "2606:4700:abcd:1234::42"
	p.Identity = "user-1"
	m.policies[1] = p
	m.ready[p.Owner] = true
	r, e := m.Resolve(3, "business")
	if e != nil || r.Owner != p.Owner || r.Source.String() != p.Source {
		t.Fatal(r, e)
	}
	m.runtime.Enabled = false
	if _, e = m.Resolve(3, "query"); e == nil {
		t.Fatal("pause downgraded")
	}
	m.runtime.Enabled = true
	m.policies[1] = Policy{Owner: p.Owner, AccountID: 1, Mode: Strict, Target: Legacy, State: "blocked"}
	if _, e = m.Resolve(1, "business"); e == nil {
		t.Fatal("exit in progress downgraded")
	}
}
func TestClosingBusinessPreservesRefreshUntilSave(t *testing.T) {
	m := fixtureManager(t)
	b, _ := m.Resolve(1, "business")
	r, _ := m.Resolve(1, "refresh")
	bl, _ := m.Begin(context.Background(), b)
	rl, _ := m.Begin(context.Background(), r)
	defer bl.End()
	defer rl.End()
	m.closeOwner(r.Owner, false)
	if bl.Context.Err() == nil {
		select {
		case <-bl.Context.Done():
		case <-time.After(time.Second):
			t.Fatal("business not canceled")
		}
	}
	if rl.Context.Err() != nil {
		t.Fatal("refresh canceled before save")
	}
	if _, e := m.Resolve(1, "refresh"); e == nil {
		t.Fatal("new refresh accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if e := m.waitRefresh(ctx, r.Owner); e == nil {
		t.Fatal("reported drained while refresh lease active")
	}
	rl.End()
	if e := m.waitRefresh(context.Background(), r.Owner); e != nil {
		t.Fatal(e)
	}
	bl.End()
	m.closeOwner(r.Owner, true)
	if e := m.waitBusiness(context.Background(), r.Owner); e != nil {
		t.Fatal(e)
	}
}

type fakeConn struct {
	net.Conn
	local  net.Addr
	closed atomic.Bool
}

func (c *fakeConn) LocalAddr() net.Addr { return c.local }
func (c *fakeConn) Close() error        { c.closed.Store(true); return nil }
func TestOrdinaryGuardBeforeBusinessBytes(t *testing.T) {
	m := fixtureManager(t)
	m.config = Config{Prefix: "2606:4700:abcd:1234::/64", Primary: "2606:4700:abcd:1234::1"}
	c := &fakeConn{local: &net.TCPAddr{IP: net.ParseIP("2606:4700:abcd:1234::2")}}
	dial := GuardDial(func(context.Context, string, string) (net.Conn, error) { return c, nil })
	if _, e := dial(context.Background(), "tcp", "example:443"); e == nil || !c.closed.Load() {
		t.Fatal("dedicated source permitted")
	}
	c = &fakeConn{local: &net.TCPAddr{IP: net.ParseIP(m.config.Primary)}}
	if _, e := dial(context.Background(), "tcp", "example:443"); e != nil {
		t.Fatal(e)
	}
	m.config = Config{}
	m.reserved = map[netip.Addr]bool{netip.MustParseAddr("2606:4700:abcd:1234::2"): true}
	c = &fakeConn{local: &net.TCPAddr{IP: net.ParseIP("2606:4700:abcd:1234::2")}}
	if _, e := dial(context.Background(), "tcp", "example:443"); e == nil {
		t.Fatal("lost topology on config removal")
	}
}
func TestLateDialIsClosedAndNeverRegistered(t *testing.T) {
	m := fixtureManager(t)
	r, _ := m.Resolve(1, "business")
	c := &fakeConn{local: &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := r.Dial(func(context.Context, string, string) (net.Conn, error) { close(started); <-release; return c, nil })(context.Background(), "tcp", "example:443")
		done <- e
	}()
	<-started
	m.closeOwner(r.Owner, true)
	close(release)
	if e := <-done; e == nil || !c.closed.Load() {
		t.Fatal("late socket allowed")
	}
	if len(r.epoch.connections) != 0 {
		t.Fatal("late socket registered")
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type duplex struct{ *bytes.Buffer }

func (*duplex) Close() error { return nil }
func TestLeaseBodyRetainsWebSocketDuplex(t *testing.T) {
	m := fixtureManager(t)
	r, _ := m.Resolve(1, "business")
	body := &duplex{bytes.NewBuffer(nil)}
	tr := WrapTransport(rtFunc(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 101, Body: body}, nil }), r)
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	resp, e := tr.RoundTrip(req)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := resp.Body.(io.Writer); !ok {
		t.Fatal("101 lost io.Writer")
	}
	if len(r.epoch.leases) != 1 {
		t.Fatal("lease ended before body")
	}
	resp.Body.Close()
	if len(r.epoch.leases) != 0 {
		t.Fatal("leaked lease")
	}
}
func TestIPv6DestinationAndBindingJSON(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "::ffff:8.8.8.8", "fd00::1", "fe80::1", "64:ff9b::808:808", "2001:db8::1"} {
		if publicIPv6(netip.MustParseAddr(s)) {
			t.Fatal("unsafe destination", s)
		}
	}
	m := fixtureManager(t)
	b, e := json.Marshal(m.Bindings())
	if e != nil {
		t.Fatal(e)
	}
	var rows []map[string]any
	json.Unmarshal(b, &rows)
	if rows[0]["account_id"] != float64(1) {
		t.Fatal(string(b))
	}
	if _, ok := rows[0]["credentials"]; ok {
		t.Fatal("credentials exposed")
	}
}
func TestAddressReadiness(t *testing.T) {
	raw := []byte(`[{"addr_info":[{"local":"2606:4700::1","prefixlen":64,"preferred_life_time":4294967295,"valid_life_time":4294967295},{"local":"2606:4700::2","prefixlen":128,"preferred_life_time":0,"valid_life_time":4294967295},{"local":"2606:4700::3","prefixlen":128,"tentative":true,"preferred_life_time":0,"valid_life_time":4294967295}]}]`)
	for _, v := range []struct {
		ip            string
		primary, want bool
	}{{"2606:4700::1", true, true}, {"2606:4700::2", false, true}, {"2606:4700::2", true, false}, {"2606:4700::3", false, false}} {
		present, ready, e := inspectAddressJSON(raw, v.ip, v.primary)
		if e != nil || !present || ready != v.want {
			t.Fatal(v, ready, e)
		}
	}
}

func TestControlLeaseRejectsNonTokenEndpointsBeforeSending(t *testing.T) {
	m := fixtureManager(t)
	r, _ := m.Resolve(1, "refresh")
	r.control = true
	var calls int
	tr := WrapTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}), r)
	for _, target := range []string{"http://auth.openai.com/api/accounts/oauth/token", "https://auth.openai.com/oauth/token", "https://chatgpt.com/backend-api/accounts/check", "https://auth.openai.com:444/api/accounts/oauth/token"} {
		req, _ := http.NewRequest("POST", target, nil)
		if _, err := tr.RoundTrip(req); err == nil {
			t.Fatal("unsafe control endpoint", target)
		}
	}
	if calls != 0 {
		t.Fatal("denied request sent")
	}
	req, _ := http.NewRequest("POST", TokenEndpoint, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Fatal(calls)
	}
}

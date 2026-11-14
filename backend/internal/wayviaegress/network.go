package wayviaegress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

type DialFunc func(context.Context, string, string) (net.Conn, error)

// TLSDial 覆盖 req 的 HTTP/2 内部重连及强制 HTTP/2 路径。
// 这些路径可能不调用 SetDial，必须显式设置 SetDialTLS，且在受控 TCP 上完成原有指纹握手。
func TLSDial(base DialFunc, config *tls.Config, handshake func(context.Context, string, net.Conn) (net.Conn, *tls.ConnectionState, error)) DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		raw, err := base(ctx, network, address)
		if err != nil {
			return nil, err
		}
		var conn net.Conn
		if handshake != nil {
			conn, _, err = handshake(ctx, host, raw)
		} else {
			cfg := &tls.Config{}
			if config != nil {
				cfg = config.Clone()
			}
			if cfg.ServerName == "" {
				cfg.ServerName = host
			}
			tlsConn := tls.Client(raw, cfg)
			err = tlsConn.HandshakeContext(ctx)
			conn = tlsConn
		}
		if err != nil {
			raw.Close()
			return nil, err
		}
		return conn, nil
	}
}

// auth.openai.com的OIDC发现端点公布此地址；Token POST禁止依靠重定向重放。
const TokenEndpoint = "https://auth.openai.com/api/accounts/oauth/token"

func publicIPv6(a netip.Addr) bool {
	return a.Is6() && !a.Is4In6() && a.Zone() == "" && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() && !netip.MustParsePrefix("2001:db8::/32").Contains(a) && !netip.MustParsePrefix("64:ff9b::/96").Contains(a)
}

// GuardDial 在代理认证或 TLS 之前检查实际源地址；普通路径不依赖池健康。
func GuardDial(base DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := base(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if m := Default(); m != nil && m.reservedSource(c.LocalAddr()) {
			c.Close()
			return nil, Error("EGRESS_SOURCE_CONFLICT", "普通出口误选账号专用地址")
		}
		return c, nil
	}
}
func (m *Manager) reservedSource(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	a, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return false
	}
	a = a.Unmap()
	m.mu.Lock()
	known := m.reserved[a]
	m.mu.Unlock()
	if known {
		return true
	}
	p, err := netip.ParsePrefix(m.config.Prefix)
	if err != nil || !p.Contains(a) {
		return false
	}
	primary, _ := netip.ParseAddr(m.config.Primary)
	return a != primary
}

func (r Route) Dial(base DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, Error("EGRESS_PATH_UNSUPPORTED", "仅支持TCP")
		}
		if err := r.Check(); err != nil {
			return nil, err
		}
		if r.Strict() && (!r.Source.IsValid() || !publicIPv6(r.Source)) {
			return nil, Error("EGRESS_BINDING_MISSING", "源地址无效")
		}
		dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if r.epoch != nil {
			stop := context.AfterFunc(r.epoch.ctx, cancel)
			defer stop()
		}
		var c net.Conn
		var err error
		if !r.Strict() {
			c, err = GuardDial(base)(dialCtx, network, addr)
		} else {
			if network == "tcp4" {
				return nil, Error("EGRESS_NO_AAAA", "严格出口禁止IPv4")
			}
			host, port, e := net.SplitHostPort(addr)
			if e != nil {
				return nil, e
			}
			var ips []netip.Addr
			if ip, e := netip.ParseAddr(host); e == nil {
				ips = []netip.Addr{ip}
			} else {
				ips, err = net.DefaultResolver.LookupNetIP(dialCtx, "ip6", host)
				if err != nil {
					return nil, Error("EGRESS_NO_AAAA", "AAAA解析失败")
				}
			}
			if len(ips) == 0 {
				return nil, Error("EGRESS_NO_AAAA", "目标没有AAAA记录")
			}
			for _, ip := range ips {
				if !publicIPv6(ip) {
					return nil, Error("EGRESS_DESTINATION_DENIED", "目标IPv6不允许")
				}
			}
			d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(r.Source.AsSlice())}, KeepAlive: 30 * time.Second}
			for _, ip := range ips {
				c, err = d.DialContext(dialCtx, "tcp6", net.JoinHostPort(ip.String(), port))
				if err == nil {
					break
				}
				if dialCtx.Err() != nil {
					break
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if err = r.Check(); err != nil {
			c.Close()
			return nil, err
		}
		if r.Strict() {
			a, ok := c.LocalAddr().(*net.TCPAddr)
			if !ok || !a.IP.Equal(net.IP(r.Source.AsSlice())) {
				c.Close()
				return nil, Error("EGRESS_SOURCE_CONFLICT", "实际源地址不匹配")
			}
		}
		if m := Default(); m != nil && r.epoch != nil {
			m.mu.Lock()
			if r.epoch.closed {
				m.mu.Unlock()
				c.Close()
				return nil, Error("EGRESS_PAUSED", "拨号完成时策略已失效")
			}
			t := &trackedConn{Conn: c, manager: m, epoch: r.epoch, id: uuid.NewString()}
			r.epoch.connections[t] = struct{}{}
			m.mu.Unlock()
			c = t
		}
		return c, nil
	}
}

type roundTripper struct {
	base  http.RoundTripper
	route Route
}

func WrapTransport(base http.RoundTripper, r Route) http.RoundTripper { return &roundTripper{base, r} }
func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.route.Check(); err != nil {
		return nil, err
	}
	if (t.route.control || t.route.drainingRefresh) && (req.URL.Scheme != "https" || req.URL.User != nil || req.Method != http.MethodPost || req.URL.Hostname() != "auth.openai.com" || req.URL.Path != "/api/accounts/oauth/token" || req.URL.Port() != "" && req.URL.Port() != "443") {
		return nil, Error("EGRESS_PATH_UNSUPPORTED", "控制租约仅允许Token端点")
	}
	if t.route.Strict() {
		if req.URL.Scheme != "https" {
			return nil, Error("EGRESS_PATH_UNSUPPORTED", "严格账号只允许HTTPS/WSS")
		}
		if req.URL.User != nil {
			return nil, Error("EGRESS_DESTINATION_DENIED", "URL不允许用户信息")
		}
	}
	m := Default()
	if m == nil || t.route.Owner == "" {
		return t.base.RoundTrip(req)
	}
	l, err := m.Begin(req.Context(), t.route)
	if err != nil {
		return nil, err
	}
	callCtx := l.Context
	var trace *evidenceTrace
	protocol := "HTTP"
	if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		protocol = "WS"
	}
	if t.route.Strict() {
		trace = newEvidenceTrace(m, t.route, req.URL.Hostname())
		callCtx = httptrace.WithClientTrace(callCtx, &httptrace.ClientTrace{GotConn: trace.gotConn})
	}
	resp, err := t.base.RoundTrip(req.Clone(callCtx))
	if err != nil {
		if trace != nil {
			trace.record(protocol, "request_failed", 0, err)
		}
		t.route.ReportFailure(req.Context(), err)
		l.End()
		return nil, err
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		protocol = "SSE"
	}
	if trace != nil {
		event := "response_headers"
		if protocol == "WS" {
			event = "ws_handshake"
		}
		trace.record(protocol, event, resp.StatusCode, nil)
		if resp.StatusCode == http.StatusSwitchingProtocols {
			if capture, ok := req.Context().Value(captureKey{}).(*SocketCapture); ok {
				capture.capture(trace)
			}
		}
	}
	// WebSocket 的 101 body 同时实现 io.Writer，必须保持双工接口。
	b := &leaseBody{ReadCloser: resp.Body, end: l.End}
	if trace != nil && protocol != "WS" {
		b.readDone = func(err error) {
			event := "body_eof"
			if err != io.EOF {
				event = "stream_failed"
				t.route.ReportFailure(req.Context(), err)
			} else {
				err = nil
			}
			trace.record(protocol, event, resp.StatusCode, err)
		}
	}
	if w, ok := resp.Body.(io.Writer); ok {
		resp.Body = &duplexLeaseBody{leaseBody: b, writer: w}
	} else {
		resp.Body = b
	}
	return resp, nil
}

type leaseBody struct {
	io.ReadCloser
	end      func()
	once     sync.Once
	readOnce sync.Once
	readDone func(error)
}

func (b *leaseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && b.readDone != nil {
		b.readOnce.Do(func() { b.readDone(err) })
	}
	return n, err
}
func (b *leaseBody) Close() error { e := b.ReadCloser.Close(); b.once.Do(b.end); return e }

type duplexLeaseBody struct {
	*leaseBody
	writer io.Writer
}

func (b *duplexLeaseBody) Write(p []byte) (int, error) { return b.writer.Write(p) }
func (t *roundTripper) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func (m *Manager) probe(ctx context.Context, p Policy) (string, error) {
	r := Route{Mode: Strict, Source: netip.Addr{}, Purpose: "probe"}
	source, err := netip.ParseAddr(p.Source)
	if err != nil || !publicIPv6(source) {
		return "", Error("EGRESS_BINDING_MISSING", "无效绑定")
	}
	r.Source = source
	tr := &http.Transport{DialContext: r.Dial((&net.Dialer{}).DialContext), TLSHandshakeTimeout: 10 * time.Second, Proxy: nil, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	urls := m.config.probeURLs()
	transport := http.RoundTripper(tr)
	if m.probeTransport != nil {
		transport = m.probeTransport
	}
	return probeEndpoints(ctx, urls, source, newProbeClient(transport, len(urls)))
}

func newProbeClient(transport http.RoundTripper, endpointCount int) *http.Client {
	timeout := 5 * time.Second
	if endpointCount == 1 {
		timeout = 15 * time.Second // 保留旧单地址配置的超时预算。
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("probe redirect denied") }}
}

func probeEndpoints(ctx context.Context, urls []string, source netip.Addr, client *http.Client) (string, error) {
	// 整轮仍受15秒预算约束，不能因增加备用服务拖长管理操作。
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if len(urls) == 0 {
		return "", Error("EGRESS_PROBE_FAILED", "未配置回显地址")
	}
	for i, endpoint := range urls {
		if ctx.Err() != nil {
			return "", Error("EGRESS_PROBE_FAILED", "回显核验已取消或超时")
		}
		observed, err := probeEndpoint(ctx, client, endpoint, source)
		if err == nil {
			return observed, nil
		}
		// 只对服务不可用切换目标。真实地址不匹配等安全错误不得被下一站成功掩盖。
		if apperrors.Reason(err) != "EGRESS_PROBE_FAILED" {
			return "", err
		}
		if i == len(urls)-1 {
			return "", Error("EGRESS_PROBE_FAILED", fmt.Sprintf("%d个回显服务均失败：%s", len(urls), apperrors.Message(err)))
		}
	}
	return "", Error("EGRESS_PROBE_FAILED", "回显服务不可用")
}

func probeEndpoint(ctx context.Context, client *http.Client, endpoint string, source netip.Addr) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", Error("EGRESS_DESTINATION_DENIED", "回显地址无效")
	}
	resp, err := client.Do(req)
	if err != nil {
		switch apperrors.Reason(err) {
		case "EGRESS_SOURCE_CONFLICT", "EGRESS_DESTINATION_DENIED", "EGRESS_BINDING_MISSING", "EGRESS_PAUSED", "EGRESS_PATH_UNSUPPORTED":
			return "", err
		}
		return "", Error("EGRESS_PROBE_FAILED", "回显连接失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", Error("EGRESS_PROBE_FAILED", "回显状态非200")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 257))
	if err != nil || len(body) > 256 {
		return "", Error("EGRESS_PROBE_FAILED", "回显格式不正确")
	}
	if ctx.Err() != nil {
		return "", Error("EGRESS_PROBE_FAILED", "回显核验已取消或超时")
	}
	a, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil {
		return "", Error("EGRESS_PROBE_FAILED", "回显格式不正确")
	}
	if a != source {
		return "", Error("EGRESS_SOURCE_CONFLICT", "回显地址与绑定不匹配")
	}
	return a.String(), nil
}

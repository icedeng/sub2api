package repository

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	"github.com/Wei-Shaw/sub2api/internal/wayviaegress"

	"github.com/imroc/req/v3"
)

// reqClientOptions 定义 req 客户端的构建参数
type reqClientOptions struct {
	Route         *wayviaegress.Route
	TokenExchange bool
	ProxyURL      string        // 代理 URL（支持 http/https/socks5）
	Timeout       time.Duration // 请求超时时间
	Impersonate   bool          // 是否模拟浏览器指纹（当前为 Firefox，Chrome 伪装会被 chatgpt.com 的 Cloudflare 质询）
	ForceHTTP2    bool          // 是否强制使用 HTTP/2
}

// sharedReqClients 存储按配置参数缓存的 req 客户端实例
//
// 性能优化说明：
// 原实现在每次 OAuth 刷新时都创建新的 req.Client：
// 1. claude_oauth_service.go: 每次刷新创建新客户端
// 2. openai_oauth_service.go: 每次刷新创建新客户端
// 3. gemini_oauth_client.go: 每次刷新创建新客户端
//
// 新实现使用 sync.Map 缓存客户端：
// 1. 相同配置（代理+超时+模拟设置）复用同一客户端
// 2. 复用底层连接池，减少 TLS 握手开销
// 3. LoadOrStore 保证并发安全，避免重复创建
var sharedReqClients sync.Map

// getSharedReqClient 获取共享的 req 客户端实例
// 性能优化：相同配置复用同一客户端，避免重复创建
func getSharedReqClient(opts reqClientOptions) (*req.Client, error) {
	key := buildReqClientKey(opts)
	if opts.Route != nil {
		key += "|" + opts.Route.Key()
	}
	if opts.TokenExchange {
		key += "|token"
	}
	if cached, ok := sharedReqClients.Load(key); ok {
		if c, ok := cached.(*req.Client); ok {
			return c, nil
		}
	}

	client := req.C().SetTimeout(opts.Timeout)
	if opts.ForceHTTP2 {
		client = client.EnableForceHTTP2()
	}
	if opts.Impersonate {
		// chatgpt.com 的 Cloudflare 会对 req 内置的 Chrome 伪装（UA 固定为 Chrome/120，
		// 与 sec-ch-ua 等 Client Hints 一起已明显过时）直接返回 403 cf-mitigated=challenge，
		// 导致 accounts/check、subscriptions、隐私设置等 backend-api 调用全部失败，
		// 订阅到期时间因此长期不更新（见 issue #4825）。Firefox 伪装的 UA/头部组合
		// 在同一出口 IP 下稳定通过，故改用 Firefox 指纹。
		client = client.ImpersonateFirefox()
	}
	trimmed, _, err := proxyurl.Parse(opts.ProxyURL)
	if err != nil {
		return nil, err
	}
	if trimmed != "" {
		client.SetProxyURL(trimmed)
	}
	base := wayviaegress.GuardDial((&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext)
	if opts.Route != nil {
		if opts.Route.Strict() && trimmed != "" {
			return nil, wayviaegress.Error("EGRESS_PROXY_CONFLICT", "固定IPv6禁止代理")
		}
		base = opts.Route.Dial(base)
		if opts.Route.Strict() {
			client.SetProxy(nil)
			client.DisableHTTP3()
		}
		route := *opts.Route
		client.GetTransport().WrapRoundTripFunc(func(rt http.RoundTripper) req.HttpRoundTripFunc {
			return wayviaegress.WrapTransport(rt, route).RoundTrip
		})
	}
	client.SetDial(base)
	if opts.Route != nil && opts.Route.Strict() {
		// req 的 HTTP/2 新建/重连可能绕过 SetDial；TLS 入口也必须绑定同一出口。
		client.SetDialTLS(wayviaegress.TLSDial(base, client.GetTLSClientConfig(), client.GetTransport().TLSHandshakeContext))
	}
	if opts.TokenExchange {
		client.SetRedirectPolicy(req.NoRedirectPolicy())
		client.SetCommonRetryCount(0)
	}
	client = instrumentReqClient(client)

	actual, loaded := sharedReqClients.LoadOrStore(key, client)
	if loaded {
		client.GetTransport().CloseIdleConnections()
	} else if opts.Route != nil {
		opts.Route.OnInvalidated(func() { sharedReqClients.CompareAndDelete(key, client); client.GetTransport().CloseIdleConnections() })
	}
	if c, ok := actual.(*req.Client); ok {
		return c, nil
	}
	return client, nil
}

func instrumentReqClient(client *req.Client) *req.Client {
	if client == nil {
		return nil
	}
	client.GetTransport().WrapRoundTripFunc(func(rt http.RoundTripper) req.HttpRoundTripFunc {
		timed := servertiming.WrapRoundTripper(rt)
		return timed.RoundTrip
	})
	return client
}

func buildReqClientKey(opts reqClientOptions) string {
	return fmt.Sprintf("%s|%s|%t|%t",
		strings.TrimSpace(opts.ProxyURL),
		opts.Timeout.String(),
		opts.Impersonate,
		opts.ForceHTTP2,
	)
}

// CreatePrivacyReqClient creates an HTTP client for OpenAI privacy settings API
// This is exported for use by OpenAIPrivacyService
// Uses Chrome TLS fingerprint impersonation to bypass Cloudflare checks
func CreatePrivacyReqClient(proxyURL string) (*req.Client, error) {
	return getSharedReqClient(reqClientOptions{
		ProxyURL:    proxyURL,
		Timeout:     30 * time.Second,
		Impersonate: true, // Enable browser TLS fingerprint impersonation (Firefox, see getSharedReqClient)
	})
}

func CreateAccountPrivacyReqClient(ctx context.Context, proxyURL string) (*req.Client, error) {
	id, purpose := wayviaegress.AccountContext(ctx)
	if id < 0 {
		return nil, wayviaegress.Error("EGRESS_ACCOUNT_CONTEXT_REQUIRED", "查询凭据缺少可信账号上下文")
	}
	if id == 0 {
		return CreatePrivacyReqClient(proxyURL)
	}
	route, err := wayviaegress.Resolve(id, purpose)
	if err != nil {
		return nil, err
	}
	return getSharedReqClient(reqClientOptions{ProxyURL: proxyURL, Timeout: 30 * time.Second, Impersonate: true, Route: &route})
}

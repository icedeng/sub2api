package wayviabalance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

func jitter(maximum time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(maximum))) }

func (m *Manager) target(a *service.Account) (string, error) {
	if strings.TrimSpace(a.GetCredential("api_key")) == "" {
		return "", &queryError{code: "missing_api_key"}
	}
	if a.ProxyID != nil && (a.Proxy == nil || a.Proxy.ID != *a.ProxyID || !a.Proxy.IsActive() || a.Proxy.IsExpired(time.Now())) {
		return "", &queryError{code: "proxy_unavailable"}
	}
	base := a.GetCredential("base_url")
	if a.RoutesProtocolByInbound() && a.IsAdaptiveAPIProtocol() {
		base = a.GetCNProtocolBaseURL(service.APIProtocolChatCompletions)
	}
	root, err := normalizeRoot(base)
	if err != nil {
		return "", &queryError{code: "invalid_base_url"}
	}
	options := urlvalidator.ValidationOptions{}
	if m.cfg != nil && m.cfg.Security.URLAllowlist.Enabled {
		options.AllowedHosts = m.cfg.Security.URLAllowlist.UpstreamHosts
		options.RequireAllowlist = true
		options.AllowPrivate = m.cfg.Security.URLAllowlist.AllowPrivateHosts
	}
	if _, err = urlvalidator.ValidateHTTPSURL(root, options); err != nil {
		return "", &queryError{code: "invalid_base_url"}
	}
	return root, nil
}

func siteKey(root string) string { u, _ := url.Parse(root); return digest(u.Scheme + "://" + u.Host) }

func (m *Manager) metadataKey(a *service.Account, root string) string {
	proxy := ""
	if a.Proxy != nil {
		proxy = a.Proxy.URL()
	}
	// 公开请求只允许 Accept-Language 和 User-Agent 覆写；不复制准入头和任何 Cookie。
	userAgent, _ := a.HeaderOverrideValue("user-agent")
	language, _ := a.HeaderOverrideValue("accept-language")
	return digest(root + "\x00" + proxy + "\x00" + userAgent + "\x00" + language)
}

func (m *Manager) fetch(ctx context.Context, a *service.Account, root, path, token, userID string, public bool) ([]byte, error) {
	release, err := m.acquireSite(ctx, siteKey(root))
	if err != nil {
		return nil, err
	}
	defer release()
	requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	requestCtx = service.WithHTTPUpstreamRedirectsDisabled(requestCtx)
	if m.cfg == nil || !m.cfg.Security.URLAllowlist.Enabled || !m.cfg.Security.URLAllowlist.AllowPrivateHosts {
		requestCtx = service.WithHTTPUpstreamPublicHostsOnly(requestCtx)
	}
	if a.Platform == service.PlatformOpenAI {
		requestCtx = service.WithHTTPUpstreamProfile(requestCtx, service.HTTPUpstreamProfileOpenAI)
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpointURL(root, path), nil)
	if err != nil {
		return nil, &queryError{code: "invalid_base_url"}
	}
	req.Header.Set("Accept", "application/json")
	if public {
		for _, key := range []string{"user-agent", "accept-language"} {
			if value, ok := a.HeaderOverrideValue(key); ok {
				req.Header.Set(key, value)
			}
		}
	} else {
		// 余额探测属于账号后台任务，仅应用静态覆写。HTTP 请求仍使用自己的超时和取消上下文。
		a.ApplyHeaderOverrides(context.Background(), req.Header)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if userID != "" {
			req.Header.Set("New-Api-User", userID)
		}
	}
	proxy := ""
	if a.Proxy != nil {
		proxy = a.Proxy.URL()
	}
	var profile *tlsfingerprint.Profile
	if m.tls != nil {
		profile = m.tls.ResolveTLSProfile(a)
	}
	if m.http == nil {
		return nil, ErrUnavailable
	}
	resp, err := m.http.DoWithTLS(req, proxy, a.ID, a.Concurrency, profile)
	if err != nil {
		return nil, &queryError{code: "request_failed"}
	}
	if resp == nil || resp.Body == nil {
		return nil, ErrInvalidResponse
	}
	defer resp.Body.Close()
	now := time.Now().UTC()
	if resp.StatusCode == 429 || resp.StatusCode >= 500 && resp.StatusCode <= 599 {
		next := now.Add(time.Minute + jitter(15*time.Second))
		if remote := retryAfter(resp.Header.Get("Retry-After"), now); remote.After(next) {
			next = remote
		}
		if err := m.cool(ctx, siteKey(root), next); err != nil {
			return nil, &queryError{code: "coordination_unavailable"}
		}
		code := "upstream_unavailable"
		if resp.StatusCode == 429 {
			code = "rate_limited"
		}
		return nil, &queryError{code: code, retryAt: next}
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, &queryError{code: "auth_failed"}
	}
	if resp.StatusCode == 404 || resp.StatusCode == 405 {
		return nil, ErrUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ErrBusiness
	}
	// 所有候选接口共用整轮预算；额外读取一个字节以识别超大响应。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	if err != nil {
		return nil, &queryError{code: "request_failed"}
	}
	if len(body) > 64*1024 {
		return nil, &queryError{code: "response_too_large"}
	}
	return body, nil
}

func (m *Manager) metadata(ctx context.Context, a *service.Account, root string) (*Metadata, error) {
	key := m.metadataKey(a, root)
	if meta, err := m.cachedMetadata(ctx, key); err != nil {
		return nil, &queryError{code: "coordination_unavailable"}
	} else if meta != nil {
		return meta, nil
	}
	failed, err := m.redis.Exists(ctx, redisPrefix+"metadata-failed:"+key).Result()
	if err != nil {
		return nil, &queryError{code: "coordination_unavailable"}
	}
	if failed > 0 {
		return nil, &queryError{code: "metadata_unavailable"}
	}
	release, ok, err := m.lock(ctx, "metadata-lock:"+key, 30*time.Second)
	if err != nil {
		return nil, &queryError{code: "coordination_unavailable"}
	}
	if !ok {
		return nil, &queryError{code: "metadata_unavailable"}
	}
	defer release()
	if meta, err := m.cachedMetadata(ctx, key); err != nil {
		return nil, &queryError{code: "coordination_unavailable"}
	} else if meta != nil {
		return meta, nil
	}
	failed, err = m.redis.Exists(ctx, redisPrefix+"metadata-failed:"+key).Result()
	if err != nil {
		return nil, &queryError{code: "coordination_unavailable"}
	}
	if failed > 0 {
		return nil, &queryError{code: "metadata_unavailable"}
	}
	body, err := m.fetch(ctx, a, root, "/api/status", "", "", true)
	var meta *Metadata
	if err == nil {
		meta, err = parseMetadata(body)
	}
	if err != nil {
		if cacheErr := m.redis.Set(ctx, redisPrefix+"metadata-failed:"+key, "1", time.Minute).Err(); cacheErr != nil {
			return nil, &queryError{code: "coordination_unavailable"}
		}
		return nil, err
	}
	meta.FetchedAt = time.Now().UTC()
	meta.ExpiresAt = meta.FetchedAt.Add(time.Hour)
	value, err := json.Marshal(meta)
	if err != nil {
		return nil, ErrInvalidResponse
	}
	if err = m.redis.Set(ctx, redisPrefix+"metadata:"+key, value, time.Hour).Err(); err != nil {
		return nil, &queryError{code: "coordination_unavailable"}
	}
	return meta, nil
}

func (m *Manager) query(ctx context.Context, a *service.Account, r *record, root string, rediscover bool) (*Snapshot, string, error) {
	provider := r.Config.Provider
	paths := []struct{ provider, path string }{}
	if !rediscover && provider == "auto" && r.DetectedProvider != "" {
		paths = append(paths, struct{ provider, path string }{r.DetectedProvider, r.DetectedPath})
	}
	if provider == "auto" || provider == "sub2api" {
		paths = append(paths, struct{ provider, path string }{"sub2api", "/v1/usage"}, struct{ provider, path string }{"sub2api", "/usage"})
	}
	if provider == "auto" || provider == "newapi" {
		paths = append(paths, struct{ provider, path string }{"newapi", "/api/usage/token/"})
	}
	seen := map[string]bool{}
	for _, candidate := range paths {
		if seen[candidate.path] {
			continue
		}
		seen[candidate.path] = true
		body, err := m.fetch(ctx, a, root, candidate.path, a.GetCredential("api_key"), "", false)
		if errors.Is(err, ErrUnsupported) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		var snapshot *Snapshot
		partial := ""
		if candidate.provider == "sub2api" {
			snapshot, err = parseSub2API(body)
		} else {
			// 先验证 token 响应结构；不能因错误 JSON 继续请求元数据和钱包。
			snapshot, err = parseNewAPI(body, nil)
			if err == nil {
				meta, metaErr := m.metadata(ctx, a, root)
				if metaErr == nil {
					snapshot, err = parseNewAPI(body, meta)
				} else {
					partial = "metadata_unavailable"
				}
			}
		}
		if errors.Is(err, ErrUnsupported) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		snapshot.Provider = candidate.provider
		snapshot.Path = candidate.path
		if snapshot.Wallet == nil && candidate.provider == "newapi" && r.QueryCiphertext != "" {
			if r.QueryIdentity != accountIdentity(a) {
				partial = "query_token_binding_changed"
				snapshot.WalletUnavailableReason = partial
			} else {
				walletErr := m.supplement(ctx, a, r, root, snapshot)
				if walletErr != nil {
					partial = errorCode(walletErr)
					snapshot.WalletUnavailableReason = partial
				}
			}
		}
		snapshot.ReceivedAt = time.Now().UTC()
		snapshot.FreshUntil = snapshot.ReceivedAt.Add(2 * time.Duration(r.Config.IntervalMinutes) * time.Minute)
		if snapshot.Periods == nil {
			snapshot.Periods = []Period{}
		}
		return snapshot, partial, nil
	}
	return nil, "", ErrUnsupported
}

func (m *Manager) supplement(ctx context.Context, a *service.Account, r *record, root string, snapshot *Snapshot) error {
	if m.cipher == nil || m.cfg == nil || !m.cfg.Totp.EncryptionKeyConfigured {
		return &queryError{code: "query_token_invalid"}
	}
	token, err := m.cipher.Decrypt(r.QueryCiphertext)
	if err != nil {
		return &queryError{code: "query_token_invalid"}
	}
	body, err := m.fetch(ctx, a, root, "/api/user/self", token, r.Config.QueryUserID, false)
	if err != nil {
		if errorCode(err) == "auth_failed" {
			return &queryError{code: "query_token_invalid"}
		}
		return err
	}
	uid, err := extractNewAPIUserID(body)
	if err != nil {
		return err
	}
	if uid != r.Config.QueryUserID {
		return &queryError{code: "query_user_mismatch"}
	}
	wallet, err := parseNewAPIUser(body, snapshot.Metadata)
	if err != nil {
		return err
	}
	snapshot.Wallet = wallet
	snapshot.WalletPath = "/api/user/self"
	snapshot.WalletUnavailableReason = ""
	return nil
}

package wayviabalance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// 假传输只记录请求并返回内存响应，不进行 DNS 查询或真实上游联网。
type balanceQueryHTTP struct {
	mu       sync.Mutex
	requests []*http.Request
	respond  func(*http.Request) (*http.Response, error)
}

func (f *balanceQueryHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req.Clone(req.Context()))
	f.mu.Unlock()
	return f.respond(req)
}

func (f *balanceQueryHTTP) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return f.Do(req, proxy, id, concurrency)
}

func (f *balanceQueryHTTP) calls() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...)
}

func queryHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func newBalanceQueryManager(t *testing.T, respond func(*http.Request) (*http.Response, error)) (*Manager, *balanceQueryHTTP) {
	t.Helper()
	r := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: r.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	transport := &balanceQueryHTTP{respond: respond}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Manager{redis: client, http: transport, ctx: ctx, cancel: cancel, slots: make(chan struct{}, 4), ready: true}, transport
}

func queryAccount(id int64, key string) *service.Account {
	return &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive,
		Credentials: map[string]any{"base_url": "https://example.test/tenant/v1", "api_key": key}}
}

const queryValidMetadata = `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`
const queryValidToken = `{"code":true,"data":{"object":"token_usage","unlimited_quota":false,"total_available":15000000,"user_quota":{"total_available":41365000}}}`

func TestBalanceQueryPublicHeadersAndMetadataReuse(t *testing.T) {
	m, transport := newBalanceQueryManager(t, func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "/tenant/api/status", req.URL.Path)
		return queryHTTPResponse(200, queryValidMetadata), nil
	})
	a := queryAccount(1, "model-secret-one")
	a.Credentials["cookie"] = "session-secret"
	a.Credentials["header_override_enabled"] = true
	a.Credentials["header_overrides"] = map[string]string{"User-Agent": "Balance Test", "Accept-Language": "zh-CN", "Authorization": "Bearer override-secret", "Cookie": "override-cookie", "X-API-Key": "override-key", "X-Custom-Secret": "custom-secret"}
	ctx := context.Background()
	meta, err := m.metadata(ctx, a, "https://example.test/tenant")
	require.NoError(t, err)
	require.Equal(t, "500000", meta.QuotaPerUnit)
	require.InDelta(t, time.Hour.Seconds(), meta.ExpiresAt.Sub(meta.FetchedAt).Seconds(), 1)

	b := queryAccount(2, "different-model-secret")
	b.Credentials["header_override_enabled"] = true
	b.Credentials["header_overrides"] = map[string]string{"User-Agent": "Balance Test", "Accept-Language": "zh-CN"}
	second, err := m.metadata(ctx, b, "https://example.test/tenant")
	require.NoError(t, err)
	require.Equal(t, meta.FetchedAt, second.FetchedAt)
	calls := transport.calls()
	require.Len(t, calls, 1)
	require.Equal(t, "GET", calls[0].Method)
	require.Equal(t, "Balance Test", calls[0].Header.Get("User-Agent"))
	require.Equal(t, "zh-CN", calls[0].Header.Get("Accept-Language"))
	for _, header := range []string{"Authorization", "Cookie", "X-API-Key", "X-Goog-API-Key", "New-Api-User", "X-Custom-Secret"} {
		require.Empty(t, calls[0].Header.Get(header), header)
	}
	require.True(t, service.HTTPUpstreamRedirectsDisabled(calls[0].Context()))
	require.True(t, service.HTTPUpstreamPublicHostsOnly(calls[0].Context()))
}

func TestBalanceQueryMetadataIsolation(t *testing.T) {
	m, transport := newBalanceQueryManager(t, func(*http.Request) (*http.Response, error) { return queryHTTPResponse(200, queryValidMetadata), nil })
	ctx := context.Background()
	a := queryAccount(1, "model-key")
	_, err := m.metadata(ctx, a, "https://example.test/tenant")
	require.NoError(t, err)
	_, err = m.metadata(ctx, a, "https://example.test/other-tenant")
	require.NoError(t, err)
	b := queryAccount(2, "model-key")
	b.Proxy = &service.Proxy{ID: 9, Protocol: "http", Host: "proxy.example.test", Port: 8080, Username: "u", Password: "proxy-secret"}
	_, err = m.metadata(ctx, b, "https://example.test/tenant")
	require.NoError(t, err)
	c := queryAccount(3, "model-key")
	c.Credentials["header_override_enabled"] = true
	c.Credentials["header_overrides"] = map[string]string{"Accept-Language": "en-US"}
	_, err = m.metadata(ctx, c, "https://example.test/tenant")
	require.NoError(t, err)
	require.Len(t, transport.calls(), 4)
	require.NotEqual(t, m.metadataKey(a, "https://example.test/tenant"), m.metadataKey(b, "https://example.test/tenant"))
	// 元数据可按租户隔离，但站点限流不能被切换租户前缀绕过。
	require.Equal(t, siteKey("https://example.test/tenant"), siteKey("https://example.test/other-tenant"))
}

func TestBalanceQueryMetadataFailureBackoff(t *testing.T) {
	m, transport := newBalanceQueryManager(t, func(*http.Request) (*http.Response, error) {
		return queryHTTPResponse(200, `{"success":true,"data":{"quota_per_unit":0}}`), nil
	})
	ctx := context.Background()
	a := queryAccount(1, "first-key")
	meta, err := m.metadata(ctx, a, "https://example.test/tenant")
	require.ErrorIs(t, err, ErrInvalidResponse)
	require.Nil(t, meta)
	meta, err = m.metadata(ctx, queryAccount(2, "second-key"), "https://example.test/tenant")
	require.Equal(t, "metadata_unavailable", errorCode(err))
	require.Nil(t, meta)
	require.Len(t, transport.calls(), 1)
	_, err = m.metadata(ctx, a, "https://example.test/other-tenant")
	require.ErrorIs(t, err, ErrInvalidResponse)
	require.Len(t, transport.calls(), 2)
}

func TestBalanceQueryExpiredMetadataRefetched(t *testing.T) {
	m, transport := newBalanceQueryManager(t, func(*http.Request) (*http.Response, error) { return queryHTTPResponse(200, queryValidMetadata), nil })
	a := queryAccount(1, "model-key")
	root := "https://example.test/tenant"
	old, err := json.Marshal(&Metadata{QuotaPerUnit: "1", DisplayType: "USD", ExpiresAt: time.Now().Add(-time.Minute)})
	require.NoError(t, err)
	require.NoError(t, m.redis.Set(context.Background(), redisPrefix+"metadata:"+m.metadataKey(a, root), old, time.Hour).Err())
	meta, err := m.metadata(context.Background(), a, root)
	require.NoError(t, err)
	require.Equal(t, "500000", meta.QuotaPerUnit)
	require.Len(t, transport.calls(), 1)
}

func TestBalanceQueryRetryAfterBlocksOtherAccountAndPath(t *testing.T) {
	for _, mode := range []string{"seconds", "http-date"} {
		t.Run(mode, func(t *testing.T) {
			start := time.Now().UTC()
			minimum := start.Add(120 * time.Second)
			retryValue := "120"
			if mode == "http-date" {
				minimum = start.Add(180 * time.Second).Truncate(time.Second)
				retryValue = minimum.Format(http.TimeFormat)
			}
			m, transport := newBalanceQueryManager(t, func(*http.Request) (*http.Response, error) {
				resp := queryHTTPResponse(429, `{"error":"limited"}`)
				resp.Header.Set("Retry-After", retryValue)
				return resp, nil
			})
			_, err := m.fetch(context.Background(), queryAccount(1, "key-one"), "https://example.test/tenant", "/v1/usage", "key-one", "", false)
			var failure *queryError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, "rate_limited", failure.code)
			require.False(t, failure.retryAt.Before(minimum))
			_, err = m.fetch(context.Background(), queryAccount(2, "key-two"), "https://example.test/other-tenant", "/api/status", "", "", true)
			require.Equal(t, "rate_limited", errorCode(err))
			require.Len(t, transport.calls(), 1)
		})
	}
}

func TestBalanceQueryAutoDoesNotFallbackOnAuthenticationFailure(t *testing.T) {
	for _, status := range []int{401, 403} {
		m, transport := newBalanceQueryManager(t, func(*http.Request) (*http.Response, error) {
			return queryHTTPResponse(status, `{"error":"denied"}`), nil
		})
		r := &record{Config: Config{Provider: "auto", IntervalMinutes: 5}}
		snapshot, _, err := m.query(context.Background(), queryAccount(1, "model-key"), r, "https://example.test/tenant", false)
		require.Equal(t, "auth_failed", errorCode(err))
		require.Nil(t, snapshot)
		require.Len(t, transport.calls(), 1)
		require.Equal(t, "/tenant/v1/usage", transport.calls()[0].URL.Path)
	}
}

func TestBalanceQueryAutoFallsBackOnlyOnUnsupported(t *testing.T) {
	m, transport := newBalanceQueryManager(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/tenant/v1/usage", "/tenant/usage":
			return queryHTTPResponse(404, `{}`), nil
		case "/tenant/api/usage/token/":
			return queryHTTPResponse(200, queryValidToken), nil
		case "/tenant/api/status":
			return queryHTTPResponse(200, queryValidMetadata), nil
		default:
			t.Fatalf("unexpected query path: %s", req.URL.Path)
			return nil, nil
		}
	})
	snapshot, partial, err := m.query(context.Background(), queryAccount(1, "model-key"), &record{Config: Config{Provider: "auto", IntervalMinutes: 5}}, "https://example.test/tenant", false)
	require.NoError(t, err)
	require.Empty(t, partial)
	require.Equal(t, "newapi", snapshot.Provider)
	require.Equal(t, "82.73", snapshot.Wallet.Amount)
	require.Equal(t, "30", snapshot.KeyQuota.Amount)
	require.Len(t, transport.calls(), 4)
}

func TestBalanceQueryMetadataFailurePreservesRawQuota(t *testing.T) {
	m, transport := newBalanceQueryManager(t, func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/api/status") {
			return queryHTTPResponse(200, `{"success":true,"data":{"quota_per_unit":0}}`), nil
		}
		return queryHTTPResponse(200, queryValidToken), nil
	})
	snapshot, partial, err := m.query(context.Background(), queryAccount(1, "model-key"), &record{Config: Config{Provider: "newapi", IntervalMinutes: 5}}, "https://example.test/tenant", false)
	require.NoError(t, err)
	require.Equal(t, "metadata_unavailable", partial)
	require.Equal(t, "41365000", snapshot.Wallet.RawAmount)
	require.Equal(t, "41365000", snapshot.Wallet.Amount)
	require.Equal(t, "quota", snapshot.Wallet.Unit)
	require.Equal(t, "15000000", snapshot.KeyQuota.Amount)
	require.Nil(t, snapshot.Metadata)
	require.Len(t, transport.calls(), 2)
}

func TestBalanceQueryCorruptMetadataCannotPanicOrInventZero(t *testing.T) {
	m, _ := newBalanceQueryManager(t, func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/api/status") {
			return queryHTTPResponse(200, queryValidMetadata), nil
		}
		return queryHTTPResponse(200, queryValidToken), nil
	})
	a := queryAccount(1, "model-key")
	root := "https://example.test/tenant"
	bad, err := json.Marshal(&Metadata{QuotaPerUnit: "0", DisplayType: "USD", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.NoError(t, m.redis.Set(context.Background(), redisPrefix+"metadata:"+m.metadataKey(a, root), bad, time.Hour).Err())
	require.NotPanics(t, func() {
		snapshot, _, err := m.query(context.Background(), a, &record{Config: Config{Provider: "newapi", IntervalMinutes: 5}}, root, false)
		if err != nil {
			require.Nil(t, snapshot)
			return
		}
		require.NotNil(t, snapshot.Wallet)
		require.NotEqual(t, "0", snapshot.Wallet.Amount)
		require.Equal(t, "41365000", snapshot.Wallet.RawAmount)
	})
}

type queryAccountReader struct{ account *service.Account }

func (r queryAccountReader) GetByID(context.Context, int64) (*service.Account, error) {
	return r.account, nil
}
func (r queryAccountReader) GetByIDs(context.Context, []int64) ([]*service.Account, error) {
	return []*service.Account{r.account}, nil
}

type queryResultStore struct{ initial, saved *record }

func (s *queryResultStore) Load(context.Context, int64) (*record, error)         { return s.initial, nil }
func (s *queryResultStore) Due(context.Context, time.Time, int) ([]int64, error) { return nil, nil }
func (s *queryResultStore) SaveConfig(context.Context, *service.Account, Config, int64, string, string, string, bool) (*record, error) {
	return nil, ErrConfigChanged
}
func (s *queryResultStore) SaveResult(_ context.Context, _ *service.Account, _ int64, _ string, result *record) error {
	s.saved = result
	return nil
}

func TestBalanceQueryMalformedSuccessDoesNotOverwriteSnapshot(t *testing.T) {
	for _, previous := range []*Snapshot{nil, {Provider: "sub2api", Wallet: &Metric{Amount: "40.12", Unit: "USD"}, ReceivedAt: time.Now().Add(-time.Hour)}} {
		m, transport := newBalanceQueryManager(t, func(*http.Request) (*http.Response, error) {
			return queryHTTPResponse(200, `{"mode":"unrestricted","balance":"NaN","unit":"USD"}`), nil
		})
		a := queryAccount(1, "model-key")
		initial := &record{Config: Config{Enabled: true, Provider: "sub2api", IntervalMinutes: 5}, Identity: accountIdentity(a), Revision: 1, Snapshot: previous}
		store := &queryResultStore{initial: initial}
		m.store, m.accounts = store, queryAccountReader{account: a}
		state, err := m.Refresh(context.Background(), a.ID, false)
		require.NoError(t, err)
		require.Equal(t, "invalid_response", state.ErrorCode)
		require.NotNil(t, store.saved)
		require.Same(t, previous, store.saved.Snapshot)
		if previous == nil {
			require.Equal(t, "error", state.Status)
			require.Nil(t, state.Snapshot)
		} else {
			require.Equal(t, "stale", state.Status)
			require.Equal(t, "40.12", state.Snapshot.Wallet.Amount)
			require.Equal(t, previous.ReceivedAt, state.Snapshot.ReceivedAt)
		}
		require.Len(t, transport.calls(), 1)
	}
}

type balanceQueryCipher struct{}

func (balanceQueryCipher) Encrypt(string) (string, error) { return "encrypted-test", nil }
func (balanceQueryCipher) Decrypt(string) (string, error) { return "supplemental-test-secret", nil }

func TestBalanceQuerySupplementKeepsIdentityAndCredentialBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name, userID, binding, partial string
		wantWallet                     bool
	}{
		{"same user", "42", "current", "", true},
		{"different user", "43", "current", "query_user_mismatch", false},
		{"changed binding", "42", "old-identity", "query_token_binding_changed", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, transport := newBalanceQueryManager(t, func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/tenant/api/usage/token/":
					require.Equal(t, "Bearer model-test-secret", req.Header.Get("Authorization"))
					return queryHTTPResponse(200, `{"code":true,"data":{"object":"token_usage","unlimited_quota":false,"total_available":15000000}}`), nil
				case "/tenant/api/status":
					require.Empty(t, req.Header.Get("Authorization"))
					return queryHTTPResponse(200, queryValidMetadata), nil
				case "/tenant/api/user/self":
					require.Equal(t, "Bearer supplemental-test-secret", req.Header.Get("Authorization"))
					require.Equal(t, "42", req.Header.Get("New-Api-User"))
					return queryHTTPResponse(200, `{"success":true,"data":{"id":`+tt.userID+`,"quota":41365000}}`), nil
				default:
					t.Fatalf("unexpected query path: %s", req.URL.Path)
					return nil, ErrInvalidResponse
				}
			})
			m.cfg = &config.Config{}
			m.cfg.Totp.EncryptionKeyConfigured = true
			m.cipher = balanceQueryCipher{}
			a := queryAccount(1, "model-test-secret")
			binding := tt.binding
			if binding == "current" {
				binding = accountIdentity(a)
			}
			r := &record{Config: Config{Provider: "newapi", QueryUserID: "42", IntervalMinutes: 5},
				QueryCiphertext: "encrypted-test", QueryIdentity: binding}
			snapshot, partial, err := m.query(context.Background(), a, r, "https://example.test/tenant", false)
			require.NoError(t, err)
			require.Equal(t, tt.partial, partial)
			require.Equal(t, "30", snapshot.KeyQuota.Amount)
			if tt.wantWallet {
				require.Equal(t, "82.73", snapshot.Wallet.Amount)
				require.Equal(t, "/api/user/self", snapshot.WalletPath)
			} else {
				require.Nil(t, snapshot.Wallet)
				require.Empty(t, snapshot.WalletPath)
			}
			if tt.binding != "current" {
				require.Len(t, transport.calls(), 2, "a token bound to an old identity must not be sent")
			} else {
				require.Len(t, transport.calls(), 3)
			}
		})
	}
}

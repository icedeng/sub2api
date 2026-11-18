package wayviabalance

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type managerTestAccounts struct{ account *service.Account }

func (r managerTestAccounts) GetByID(context.Context, int64) (*service.Account, error) {
	return r.account, nil
}

func (r managerTestAccounts) GetByIDs(context.Context, []int64) ([]*service.Account, error) {
	return []*service.Account{r.account}, nil
}

type managerTestStore struct {
	record     *record
	saveConfig func(Config, int64, string, string, string, bool) (*record, error)
	saveResult func(int64, string, *record) error
}

func (s *managerTestStore) Load(context.Context, int64) (*record, error) {
	r := *s.record
	return &r, nil
}

func (s *managerTestStore) SaveConfig(_ context.Context, _ *service.Account, cfg Config, revision int64, identity, ciphertext, secretIdentity string, reset bool) (*record, error) {
	if s.saveConfig == nil {
		return nil, ErrUnavailable
	}
	return s.saveConfig(cfg, revision, identity, ciphertext, secretIdentity, reset)
}

func (s *managerTestStore) SaveResult(_ context.Context, _ *service.Account, revision int64, identity string, result *record) error {
	if s.saveResult == nil {
		return ErrUnavailable
	}
	return s.saveResult(revision, identity, result)
}

func (*managerTestStore) Due(context.Context, time.Time, int) ([]int64, error) { return nil, nil }

func (*managerTestStore) AlertWallets(context.Context, string, *int64) ([]alertWallet, error) {
	return nil, nil
}

type managerTestHTTP struct {
	calls atomic.Int32
	do    func(*http.Request) (*http.Response, error)
}

func (h *managerTestHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	h.calls.Add(1)
	if h.do != nil {
		return h.do(req)
	}
	return nil, ErrUnavailable
}

func (h *managerTestHTTP) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(req, proxy, id, concurrency)
}

func newManagerTest(t *testing.T) (*Manager, *managerTestStore, *managerTestHTTP, *service.Account) {
	t.Helper()
	a := balanceAccount()
	a.Credentials["base_url"] = "https://example.com"
	cfg := defaultConfig()
	cfg.Enabled = true
	s := &managerTestStore{record: &record{Config: cfg, Revision: 1, Identity: accountIdentity(a)}}
	h := &managerTestHTTP{}
	redisServer := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{accounts: managerTestAccounts{a}, store: s, redis: rdb, http: h, ready: true,
		ctx: ctx, cancel: cancel, slots: make(chan struct{}, 4)}
	t.Cleanup(m.Close)
	return m, s, h, a
}

func TestManagerScheduledRefreshRechecksAutoRefresh(t *testing.T) {
	m, s, h, a := newManagerTest(t)
	s.record.Config.AutoRefresh = false
	state, err := m.refresh(context.Background(), a.ID, false, true)
	require.NoError(t, err)
	require.False(t, state.Config.AutoRefresh)
	require.Zero(t, h.calls.Load())
}

func TestManagerDisabledMonitoringNeverQueriesUpstream(t *testing.T) {
	m, s, h, a := newManagerTest(t)
	s.record.Config.Enabled = false
	state, err := m.Refresh(context.Background(), a.ID, false)
	require.NoError(t, err)
	require.Equal(t, "disabled", state.Status)
	require.Zero(t, h.calls.Load())
}

func TestManagerSaturatedCapacityReturnsCacheWithoutUpstream(t *testing.T) {
	m, _, h, a := newManagerTest(t)
	for range cap(m.slots) {
		m.slots <- struct{}{}
	}
	state, err := m.Refresh(context.Background(), a.ID, false)
	require.NoError(t, err)
	require.Equal(t, "unavailable", state.Status)
	require.Equal(t, "query_busy", state.ErrorCode)
	require.Zero(t, h.calls.Load())
}

func TestManagerUpdateRejectsStaleRevisionWithoutWriting(t *testing.T) {
	m, s, h, a := newManagerTest(t)
	var writes atomic.Int32
	s.saveConfig = func(Config, int64, string, string, string, bool) (*record, error) {
		writes.Add(1)
		return nil, nil
	}
	_, err := m.Update(context.Background(), a.ID, UpdateRequest{Config: s.record.Config, ExpectedRevision: 0})
	require.ErrorIs(t, err, ErrConfigChanged)
	require.Zero(t, writes.Load())
	require.Zero(t, h.calls.Load())
}

func TestManagerCanDisableInvalidUpstreamConfiguration(t *testing.T) {
	m, s, h, a := newManagerTest(t)
	a.Credentials = map[string]any{}
	cfg := s.record.Config
	cfg.Enabled = false
	var writes int
	s.saveConfig = func(in Config, revision int64, identity, ciphertext, secretIdentity string, reset bool) (*record, error) {
		writes++
		require.Equal(t, int64(1), revision)
		require.True(t, reset)
		require.Empty(t, ciphertext)
		require.Empty(t, secretIdentity)
		return &record{Config: in, Revision: revision + 1, Identity: identity}, nil
	}
	state, err := m.Update(context.Background(), a.ID, UpdateRequest{Config: cfg, ExpectedRevision: 1})
	require.NoError(t, err)
	require.Equal(t, "disabled", state.Status)
	require.Equal(t, 1, writes)
	require.Zero(t, h.calls.Load())
}

func TestManagerStateKeepsFreshPartialDistinctFromStale(t *testing.T) {
	m, s, _, a := newManagerTest(t)
	now := time.Now().UTC()
	for _, tt := range []struct {
		name, status                  string
		received, freshUntil, attempt time.Time
	}{
		{"fresh partial", "partial", now, now.Add(time.Hour), now.Add(-time.Second)},
		{"later failed attempt", "stale", now.Add(-time.Minute), now.Add(time.Hour), now},
		{"expired partial", "stale", now.Add(-time.Hour), now.Add(-time.Minute), now.Add(-2 * time.Hour)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := *s.record
			r.ErrorCode = "metadata_unavailable"
			r.LastAttemptAt = &tt.attempt
			r.Snapshot = &Snapshot{ReceivedAt: tt.received, FreshUntil: tt.freshUntil}
			require.Equal(t, tt.status, m.state(a, &r).Status)
		})
	}
}

func TestManagerSiteCooldownPersistsDeferredRetry(t *testing.T) {
	m, s, h, a := newManagerTest(t)
	previous := time.Now().UTC().Add(-time.Minute)
	s.record.LastAttemptAt = &previous
	until := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Millisecond)
	require.NoError(t, m.cool(context.Background(), siteKey("https://example.com"), until))
	var persisted *record
	s.saveResult = func(revision int64, identity string, result *record) error {
		require.Equal(t, s.record.Revision, revision)
		require.Equal(t, s.record.Identity, identity)
		copy := *result
		persisted = &copy
		return nil
	}
	state, err := m.refresh(context.Background(), a.ID, false, true)
	require.NoError(t, err)
	require.Equal(t, "cooldown", state.Status)
	require.NotNil(t, persisted)
	require.Equal(t, until, *persisted.NextRetryAt)
	require.Equal(t, previous, *persisted.PreviousAttemptAt)
	require.True(t, persisted.LastAttemptAt.After(previous))
	require.Equal(t, "rate_limited", persisted.ErrorCode)
	require.Zero(t, h.calls.Load())
}

func TestManagerCloseWaitsForActualSingleflightExecution(t *testing.T) {
	m, s, h, a := newManagerTest(t)
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h.do = func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-req.Context().Done()
		close(canceled)
		<-release
		return nil, req.Context().Err()
	}
	var writes atomic.Int32
	s.saveResult = func(int64, string, *record) error { writes.Add(1); return nil }
	refreshDone := make(chan error, 1)
	go func() { _, err := m.Refresh(context.Background(), a.ID, false); refreshDone <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not reach the fake transport")
	}
	closeDone := make(chan struct{})
	go func() { m.Close(); close(closeDone) }()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel the active request")
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned before the active transport finished cleanup")
	case <-time.After(30 * time.Millisecond):
	}
	_, err := m.Refresh(context.Background(), a.ID+1, false)
	require.ErrorIs(t, err, ErrUnavailable)
	release <- struct{}{}
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after request cleanup")
	}
	require.ErrorIs(t, <-refreshDone, ErrUnavailable)
	require.Zero(t, writes.Load(), "shutdown must not persist the canceled result")
	require.Equal(t, int32(1), h.calls.Load())
}

func TestManagerResultPreservesPreviousAttemptForWriteCAS(t *testing.T) {
	m, s, h, a := newManagerTest(t)
	previous := time.Now().UTC().Add(-time.Minute)
	s.record.LastAttemptAt = &previous
	h.do = func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`{"mode":"unrestricted","balance":2,"unit":"USD"}`))}, nil
	}
	s.saveResult = func(_ int64, _ string, result *record) error {
		require.Equal(t, previous, *result.PreviousAttemptAt)
		require.True(t, result.LastAttemptAt.After(previous))
		return ErrConfigChanged
	}
	_, err := m.Refresh(context.Background(), a.ID, false)
	require.ErrorIs(t, err, ErrConfigChanged)
	require.Equal(t, int32(1), h.calls.Load())
}

func TestManagerPartialMetadataFailureHonorsRemoteRetryAfter(t *testing.T) {
	m, s, h, a := newManagerTest(t)
	s.record.Config.Provider = "newapi"
	minimum := time.Now().Add(30 * time.Minute)
	h.do = func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/status" {
			resp := queryHTTPResponse(http.StatusTooManyRequests, `{}`)
			resp.Header.Set("Retry-After", "1800")
			return resp, nil
		}
		return queryHTTPResponse(http.StatusOK, queryValidToken), nil
	}
	s.saveResult = func(_ int64, _ string, result *record) error {
		require.NotNil(t, result.NextRetryAt)
		require.False(t, result.NextRetryAt.Before(minimum))
		return nil
	}
	state, err := m.Refresh(context.Background(), a.ID, false)
	require.NoError(t, err)
	require.Equal(t, "partial", state.Status)
	require.Equal(t, "metadata_unavailable", state.ErrorCode)
	require.False(t, state.NextRetryAt.Before(minimum))
	require.Equal(t, "quota", state.Snapshot.Wallet.Unit)
}

package wayviabalance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/singleflight"
)

type accountReader interface {
	GetByID(context.Context, int64) (*service.Account, error)
	GetByIDs(context.Context, []int64) ([]*service.Account, error)
}

type balanceStore interface {
	Load(context.Context, int64) (*record, error)
	SaveConfig(context.Context, *service.Account, Config, int64, string, string, string, bool) (*record, error)
	SaveResult(context.Context, *service.Account, int64, string, *record) error
	Due(context.Context, time.Time, int) ([]int64, error)
	AlertWallets(context.Context, string, *int64) ([]alertWallet, error)
}

// Manager 的缓存、生命周期、并发与财务调度独立，仅依赖宿主只读账号接口和 HTTP 传输。
type Manager struct {
	accounts  accountReader
	store     balanceStore
	redis     *redis.Client
	http      service.HTTPUpstream
	cfg       *config.Config
	tls       *service.TLSFingerprintProfileService
	cipher    service.SecretEncryptor
	ready     bool
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	opMu      sync.Mutex
	closing   bool
	ops       sync.WaitGroup
	slots     chan struct{}
	group     singleflight.Group
}

// Provide 初始化失败只隔离本模块；不改变已有服务的启动或转发策略。
func Provide(db *sql.DB, rdb *redis.Client, accounts service.AccountRepository, upstream service.HTTPUpstream, cfg *config.Config, tls *service.TLSFingerprintProfileService, cipher service.SecretEncryptor) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{accounts: accounts, redis: rdb, http: upstream, cfg: cfg, tls: tls, cipher: cipher, ctx: ctx, cancel: cancel, slots: make(chan struct{}, 4)}
	s := newStore(db)
	m.store = s
	migrateCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	err := s.Migrate(migrateCtx)
	stop()
	if err != nil {
		slog.Error("wayvia_balance_unavailable", "stage", "migration")
		return m
	}
	m.ready = true
	m.wg.Add(1)
	go m.run()
	return m
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		m.opMu.Lock()
		m.closing = true
		m.cancel()
		m.opMu.Unlock()
		m.wg.Wait()
		m.ops.Wait()
	})
}

func (m *Manager) Get(ctx context.Context, id int64) (*State, error) {
	if m == nil || !m.ready {
		return nil, ErrUnavailable
	}
	a, err := m.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	r, err := m.store.Load(ctx, id)
	if err != nil {
		return nil, ErrUnavailable
	}
	return m.state(a, r), nil
}

func (m *Manager) Batch(ctx context.Context, ids []int64) ([]*State, error) {
	if m == nil || !m.ready {
		return nil, ErrUnavailable
	}
	if len(ids) > 100 {
		return nil, ErrInvalidConfig
	}
	accounts, err := m.accounts.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	states := make([]*State, 0, len(accounts))
	for _, a := range accounts {
		r, err := m.store.Load(ctx, a.ID)
		if err != nil {
			return nil, ErrUnavailable
		}
		states = append(states, m.state(a, r))
	}
	return states, nil
}

func eligible(a *service.Account) bool { return a != nil && a.Type == service.AccountTypeAPIKey }

func accountIdentity(a *service.Account) string {
	// 排序稳定的 JSON 仅在内存参与哈希；原凭据不写入模块表、缓存键或日志。
	var proxy any
	if p := a.Proxy; p != nil {
		proxy = struct {
			Protocol, Host, Username, Password, Status, Fallback string
			Port                                                 int
			ExpiresAt                                            *time.Time
			BackupID                                             *int64
		}{p.Protocol, p.Host, p.Username, p.Password, p.Status, p.FallbackMode, p.Port, p.ExpiresAt, p.BackupProxyID}
	}
	value, _ := json.Marshal(struct {
		Platform, Type string
		Credentials    map[string]any
		ProxyID        *int64
		Proxy          any
	}{a.Platform, a.Type, a.Credentials, a.ProxyID, proxy})
	return digest(string(value))
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) state(a *service.Account, r *record) *State {
	s := &State{AccountID: a.ID, Eligible: eligible(a), Revision: r.Revision, Config: r.Config, QueryTokenConfigured: r.QueryCiphertext != "", Status: "never", ErrorCode: r.ErrorCode, LastAttemptAt: r.LastAttemptAt, NextRetryAt: r.NextRetryAt, Snapshot: r.Snapshot}
	if r.Identity != "" && r.Identity != accountIdentity(a) {
		s.Status = "config_changed"
		s.ErrorCode = "config_changed"
		s.Snapshot = nil
		s.QueryTokenConfigured = false
		s.LastAttemptAt = nil
		s.NextRetryAt = nil
	} else if !s.Eligible || !r.Config.Enabled {
		s.Status = "disabled"
	} else if r.ErrorCode != "" {
		s.Status = "error"
		if r.Snapshot != nil {
			s.Status = "stale"
			if r.Snapshot.FreshUntil.After(time.Now()) && r.LastAttemptAt != nil && !r.Snapshot.ReceivedAt.Before(*r.LastAttemptAt) {
				s.Status = "partial"
			}
		}
	} else if r.Snapshot != nil {
		s.Status = "ok"
		if !r.Snapshot.FreshUntil.After(time.Now()) {
			s.Status = "stale"
		}
	}
	return s
}

func validateConfig(in Config) error {
	if in.Provider != "sub2api" && in.Provider != "newapi" && in.Provider != "auto" {
		return ErrInvalidConfig
	}
	if in.IntervalMinutes < 5 || in.IntervalMinutes > 1440 {
		return ErrInvalidConfig
	}
	if in.LowBalanceThreshold != "" {
		if len(in.LowBalanceThreshold) > 40 || strings.ContainsAny(in.LowBalanceThreshold, "eE") {
			return ErrInvalidConfig
		}
		d, err := decimal.NewFromString(in.LowBalanceThreshold)
		if err != nil || d.IsNegative() {
			return ErrInvalidConfig
		}
	}
	if in.QueryUserID != "" {
		id, err := strconv.ParseInt(in.QueryUserID, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != in.QueryUserID {
			return ErrInvalidConfig
		}
	}
	return nil
}

func (m *Manager) Update(ctx context.Context, id int64, in UpdateRequest) (*State, error) {
	if m == nil || !m.ready {
		return nil, ErrUnavailable
	}
	if err := validateConfig(in.Config); err != nil {
		return nil, err
	}
	if in.ExpectedRevision < 0 || in.ClearQueryToken && in.QueryToken != nil {
		return nil, ErrInvalidConfig
	}
	a, err := m.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !eligible(a) {
		return nil, ErrIneligible
	}
	r, err := m.store.Load(ctx, id)
	if err != nil {
		return nil, ErrUnavailable
	}
	identity := accountIdentity(a)
	if r.Revision != in.ExpectedRevision {
		return nil, ErrConfigChanged
	}
	if in.Config.Enabled {
		if _, err := m.target(a); err != nil {
			return nil, err
		}
	}
	ciphertext, secretIdentity := r.QueryCiphertext, r.QueryIdentity
	if r.Identity != identity || r.Config.QueryUserID != in.Config.QueryUserID || in.ClearQueryToken {
		ciphertext = ""
		secretIdentity = ""
	}
	if in.QueryToken != nil {
		token := strings.TrimSpace(*in.QueryToken)
		if len(token) == 0 || len(token) > 8192 || strings.ContainsAny(token, "\r\n\x00") || in.Config.QueryUserID == "" || in.Config.Provider == "sub2api" {
			return nil, ErrInvalidConfig
		}
		if m.cipher == nil || m.cfg == nil || !m.cfg.Totp.EncryptionKeyConfigured {
			return nil, &queryError{code: "encryption_key_required"}
		}
		ciphertext, err = m.cipher.Encrypt(token)
		if err != nil {
			return nil, ErrUnavailable
		}
		secretIdentity = identity
	}
	if in.Config.Provider == "sub2api" {
		ciphertext = ""
		secretIdentity = ""
	}
	reset := r.Identity != identity || r.Config.Provider != in.Config.Provider || r.Config.QueryUserID != in.Config.QueryUserID || in.QueryToken != nil || in.ClearQueryToken
	r, err = m.store.SaveConfig(ctx, a, in.Config, in.ExpectedRevision, identity, ciphertext, secretIdentity, reset)
	if err != nil {
		return nil, err
	}
	return m.state(a, r), nil
}

func (m *Manager) Refresh(ctx context.Context, id int64, rediscover bool) (*State, error) {
	return m.refresh(ctx, id, rediscover, false)
}

func (m *Manager) refresh(ctx context.Context, id int64, rediscover, scheduled bool) (*State, error) {
	if m == nil || !m.ready {
		return nil, ErrUnavailable
	}
	// 同进程合并使用独立可取消等待者，首个浏览器断开不留下无界后台工作。
	result := m.group.DoChan(strconv.FormatInt(id, 10), func() (any, error) {
		m.opMu.Lock()
		if m.closing {
			m.opMu.Unlock()
			return nil, ErrUnavailable
		}
		m.ops.Add(1)
		m.opMu.Unlock()
		defer m.ops.Done()
		return m.refreshOnce(ctx, id, rediscover, scheduled)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case value := <-result:
		if value.Err != nil {
			return nil, value.Err
		}
		return value.Val.(*State), nil
	}
}

func (m *Manager) refreshOnce(parent context.Context, id int64, rediscover, scheduled bool) (*State, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		// 不让大量不同账号的手动请求在后台积压；固定工作者繁忙时只返回缓存。
		return m.transient(ctx, id, "unavailable", "query_busy", time.Time{})
	}
	release, ok, err := m.lock(ctx, "account:"+strconv.FormatInt(id, 10), 30*time.Second)
	if err != nil {
		return m.transient(ctx, id, "unavailable", "coordination_unavailable", time.Time{})
	}
	if !ok {
		return m.transient(ctx, id, "refreshing", "refresh_in_progress", time.Time{})
	}
	defer release()
	a, err := m.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !eligible(a) {
		return nil, ErrIneligible
	}
	r, err := m.store.Load(ctx, id)
	if err != nil {
		return nil, ErrUnavailable
	}
	if !r.Config.Enabled {
		return m.state(a, r), nil
	}
	identity := accountIdentity(a)
	if identity != r.Identity {
		r, err = m.store.SaveConfig(ctx, a, r.Config, r.Revision, identity, "", "", true)
		if err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	if scheduled && (!r.Config.AutoRefresh || !a.IsActive() || r.NextRetryAt != nil && r.NextRetryAt.After(now)) {
		return m.state(a, r), nil
	}
	if r.ErrorCode != "" && r.NextRetryAt != nil && r.NextRetryAt.After(now) {
		s := m.state(a, r)
		s.Status = "cooldown"
		return s, nil
	}
	if r.LastAttemptAt != nil && now.Sub(*r.LastAttemptAt) < 10*time.Second {
		s := m.state(a, r)
		s.Status = "cooldown"
		s.ErrorCode = "manual_cooldown"
		next := r.LastAttemptAt.Add(10 * time.Second)
		s.NextRetryAt = &next
		return s, nil
	}
	root, queryErr := m.target(a)
	if queryErr == nil {
		until, coolErr := m.cooldown(ctx, siteKey(root))
		if coolErr != nil {
			return m.transient(ctx, id, "unavailable", "coordination_unavailable", time.Time{})
		}
		if until.After(now) {
			// 排到冷却结束再调度，避免同站点的到期账号长期占住 Due 页首。
			deferred := *r
			deferred.PreviousAttemptAt = r.LastAttemptAt
			deferred.LastAttemptAt = &now
			deferred.NextRetryAt = &until
			deferred.ErrorCode = "rate_limited"
			if err := m.store.SaveResult(ctx, a, r.Revision, identity, &deferred); err != nil {
				return nil, err
			}
			s := m.state(a, &deferred)
			s.Status = "cooldown"
			s.ErrorCode = "rate_limited"
			s.NextRetryAt = &until
			return s, nil
		}
	}
	var snapshot *Snapshot
	partialCode := ""
	if queryErr == nil {
		snapshot, partialCode, queryErr = m.query(ctx, a, r, root, rediscover)
	}
	result := *r
	result.PreviousAttemptAt = r.LastAttemptAt
	result.LastAttemptAt = &now
	if queryErr != nil {
		result.ErrorCode = errorCode(queryErr)
		result.FailureCount++
		delay := time.Minute * time.Duration(1<<min(result.FailureCount-1, 8))
		if delay > 6*time.Hour {
			delay = 6 * time.Hour
		}
		next := now.Add(delay + jitter(15*time.Second))
		var remote *queryError
		if errors.As(queryErr, &remote) && remote.retryAt.After(next) {
			next = remote.retryAt
		}
		result.NextRetryAt = &next
	} else {
		result.Snapshot = snapshot
		result.ErrorCode = partialCode
		result.FailureCount = 0
		result.DetectedProvider = snapshot.Provider
		result.DetectedPath = snapshot.Path
		next := time.Now().UTC().Add(time.Duration(r.Config.IntervalMinutes)*time.Minute + jitter(30*time.Second))
		// 元数据或补充钱包部分失败也可能收到 Retry-After，不能对外报告更早的下次尝试。
		if until, err := m.cooldown(ctx, siteKey(root)); err == nil && until.After(next) {
			next = until
		}
		result.NextRetryAt = &next
	}
	// 取消后的落库有独立硬上限，仍由账号/配置 CAS 拒绝迟到结果。
	persistCtx, stopPersist := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer stopPersist()
	if m.ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	if err := m.store.SaveResult(persistCtx, a, r.Revision, identity, &result); err != nil {
		return nil, err
	}
	s := m.state(a, &result)
	if queryErr == nil && partialCode != "" {
		s.Status = "partial"
	}
	return s, nil
}

func (m *Manager) transient(ctx context.Context, id int64, status, code string, next time.Time) (*State, error) {
	s, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	s.Status = status
	s.ErrorCode = code
	if !next.IsZero() {
		s.NextRetryAt = &next
	}
	return s, nil
}

func errorCode(err error) string {
	var e *queryError
	if errors.As(err, &e) {
		return e.code
	}
	switch {
	case errors.Is(err, ErrUnsupported):
		return "unsupported_response"
	case errors.Is(err, ErrBusiness):
		return "upstream_business_error"
	case errors.Is(err, ErrInvalidResponse):
		return "invalid_response"
	default:
		return "request_failed"
	}
}

func (m *Manager) run() {
	defer m.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
			ids, err := m.store.Due(ctx, time.Now().UTC(), 100)
			cancel()
			if err != nil {
				slog.Warn("wayvia_balance_due_failed")
				continue
			}
			// 固定四个工作者，不为全部账号创建 goroutine 或积压任务。
			jobs := make(chan int64)
			var workers sync.WaitGroup
			for range 4 {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for id := range jobs {
						if _, err := m.refresh(m.ctx, id, false, true); err != nil && !errors.Is(err, ErrConfigChanged) && m.ctx.Err() == nil {
							slog.Warn("wayvia_balance_refresh_failed", "account_id", id)
						}
					}
				}()
			}
			for _, id := range ids {
				select {
				case jobs <- id:
				case <-m.ctx.Done():
				}
			}
			close(jobs)
			workers.Wait()
		}
	}
}

func parseRetryDate(value string, now time.Time) time.Time {
	at, err := http.ParseTime(value)
	if err == nil && at.After(now) {
		return at.UTC()
	}
	return time.Time{}
}

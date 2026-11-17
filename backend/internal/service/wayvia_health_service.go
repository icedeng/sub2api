package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/wayviaegress"
	"github.com/Wei-Shaw/sub2api/internal/wayviahealth"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type WayviaHealthService struct {
	db          *sql.DB
	accounts    AccountRepository
	tester      *AccountTestService
	rateLimit   *RateLimitService
	concurrency *ConcurrencyService
	tokens      *OpenAITokenProvider
	audit       *AuditLogService
	secret      []byte
	startOnce   sync.Once
	stop        context.CancelFunc
	done        chan struct{}
}

func NewWayviaHealthService(db *sql.DB, accounts AccountRepository, tester *AccountTestService, rateLimit *RateLimitService, concurrency *ConcurrencyService, tokens *OpenAITokenProvider, audit *AuditLogService, secret string) *WayviaHealthService {
	return &WayviaHealthService{db: db, accounts: accounts, tester: tester, rateLimit: rateLimit, concurrency: concurrency, tokens: tokens, audit: audit, secret: []byte(secret), done: make(chan struct{})}
}

func (s *AccountTestService) Health() *WayviaHealthService { return s.health }

func (s *WayviaHealthService) Settings(ctx context.Context) (wayviahealth.Settings, error) {
	v := wayviahealth.Defaults()
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, wayviahealth.SettingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	err = json.Unmarshal([]byte(raw), &v)
	return v, err
}

func (s *WayviaHealthService) SaveSettings(ctx context.Context, v wayviahealth.Settings) (wayviahealth.Settings, error) {
	if err := v.Validate(); err != nil {
		return v, infraerrors.BadRequest("HEALTH_SETTINGS_INVALID", err.Error())
	}
	expected := v.Version
	v.Version++
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	// 同一数据库锁也用于任务认领，关闭策略与新派发具有明确先后顺序。
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(78251601)`); err != nil {
		return v, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) SELECT $1,$2,NOW() WHERE $3::bigint=0
	 ON CONFLICT(key) DO UPDATE SET value=$2,updated_at=NOW() WHERE (settings.value::jsonb->>'version')::bigint=$3`, wayviahealth.SettingsKey, string(raw), expected)
	if err != nil {
		return v, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return v, err
	}
	if n == 0 {
		// INSERT SELECT 在非零版本时没有候选行，单独执行 CAS 更新。
		res, err = tx.ExecContext(ctx, `UPDATE settings SET value=$1,updated_at=NOW() WHERE key=$2 AND (value::jsonb->>'version')::bigint=$3`, string(raw), wayviahealth.SettingsKey, expected)
		if err != nil {
			return v, err
		}
		n, err = res.RowsAffected()
		if err != nil {
			return v, err
		}
	}
	if n == 0 {
		return v, infraerrors.Conflict("HEALTH_VERSION_CONFLICT", "配置已由其他管理员更新，请重新加载")
	}
	_, err = tx.ExecContext(ctx, `UPDATE wayvia_account_test_runs SET state='cancelled',reason='策略配置已更新',finished_at=NOW() WHERE state='queued' AND source IN ('periodic','error_recheck')`)
	if err != nil {
		return v, err
	}
	return v, tx.Commit()
}

func healthPolicy(a *Account) wayviahealth.Policy {
	var p wayviahealth.Policy
	if a != nil {
		b, _ := json.Marshal(a.Extra[wayviahealth.PolicyKey])
		_ = json.Unmarshal(b, &p)
	}
	return p
}

func healthSupported(a *Account) bool {
	return a != nil && a.Platform == PlatformOpenAI && !a.IsOpenAIAgentIdentity() && (a.Type == AccountTypeOAuth || a.Type == AccountTypeAPIKey)
}

func healthProtocol(a *Account) string {
	if a != nil && a.Type == AccountTypeAPIKey && !openai_compat.ShouldUseResponsesAPI(a.Extra) {
		return "chat_completions"
	}
	return "responses"
}

func (s *WayviaHealthService) fingerprint(a, owner *Account) string {
	// HMAC 不暴露凭据；仅纳入配置字段，额度快照等运行状态不影响版本。
	extra := map[string]any{}
	for k, v := range a.Extra {
		if strings.HasPrefix(k, "codex_") || strings.HasPrefix(k, "passive_") || strings.HasPrefix(k, "wayvia_health_") || strings.HasPrefix(k, "model_rate_") {
			continue
		}
		extra[k] = v
	}
	var proxy any
	if a.Proxy != nil {
		proxy = a.Proxy.URL()
	}
	var egress any
	if a.IsOpenAIOAuth() && wayviaegress.Default() != nil {
		route, err := wayviaegress.Resolve(a.ID, "query")
		if err == nil {
			egress = []any{route.Owner, route.Mode, route.Source.String(), route.BindingVersion, route.PolicyVersion}
		}
	}
	data, _ := json.Marshal([]any{egress, a.ID, a.Platform, a.Type, a.Credentials, owner.ID, owner.Credentials, a.ProxyID, proxy, extra})
	h := hmac.New(sha256.New, s.secret)
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *WayviaHealthService) snapshot(ctx context.Context, id int64) (*Account, *Account, string, error) {
	a, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, nil, "", err
	}
	owner, err := resolveCredentialAccount(ctx, s.accounts, a)
	if err != nil {
		return nil, nil, "", err
	}
	return a, owner, s.fingerprint(a, owner), nil
}

type WayviaEffectivePolicy struct {
	Policy           wayviahealth.Policy `json:"policy"`
	Supported        bool                `json:"supported"`
	Eligible         bool                `json:"eligible"`
	IndependentPlans int                 `json:"independent_plans"`
	Model            string              `json:"model"`
	Protocol         string              `json:"protocol"`
	Template         string              `json:"template"`
	ContentSource    string              `json:"content_source"`
	Version          int64               `json:"version"`
	Timezone         string              `json:"timezone"`
}

func healthInScope(a *Account, cfg wayviahealth.Settings) bool {
	if !healthSupported(a) || !slices.Contains(cfg.AccountTypes, a.Type) {
		return false
	}
	if len(cfg.GroupIDs) == 0 {
		return true
	}
	for _, id := range a.GroupIDs {
		if slices.Contains(cfg.GroupIDs, id) {
			return true
		}
	}
	return false
}

func (s *WayviaHealthService) Effective(ctx context.Context, id int64) (*WayviaEffectivePolicy, error) {
	a, owner, _, err := s.snapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	cfg, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	p := healthPolicy(a)
	model, template, source := wayviahealth.Resolve(cfg, a.Platform, a.Type, openai.DefaultTestModel, p, nil, nil)
	var plans int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduled_test_plans WHERE account_id=$1 AND enabled`, id).Scan(&plans)
	if err != nil {
		return nil, err
	}
	return &WayviaEffectivePolicy{Policy: p, Supported: healthSupported(a) && healthSupported(owner), Eligible: healthInScope(a, cfg) && (p.Mode == "inherit" || p.Mode == "override"), IndependentPlans: plans, Model: model, Protocol: healthProtocol(a), Template: template, ContentSource: source, Version: cfg.Version, Timezone: timezone.Location().String()}, nil
}

func (s *WayviaHealthService) SetPolicy(ctx context.Context, id int64, p wayviahealth.Policy) error {
	if err := p.Validate(); err != nil {
		return infraerrors.BadRequest("HEALTH_POLICY_INVALID", err.Error())
	}
	if _, err := s.accounts.GetByID(ctx, id); err != nil {
		return err
	}
	if err := s.accounts.UpdateExtra(ctx, id, map[string]any{wayviahealth.PolicyKey: p}); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE wayvia_account_test_runs SET state='cancelled',reason='账号策略已更新',finished_at=NOW() WHERE account_id=$1 AND state='queued' AND source IN ('periodic','error_recheck')`, id)
	return err
}

type WayviaHealthSelection struct {
	AccountIDs []int64 `json:"account_ids"`
	Filters    *struct {
		Platform    string `json:"platform"`
		Type        string `json:"type"`
		Status      string `json:"status"`
		Search      string `json:"search"`
		Group       string `json:"group"`
		PrivacyMode string `json:"privacy_mode"`
	} `json:"filters"`
}

// Freeze 在服务端一次读取完整筛选结果；预览返回确定集合，确认操作只接受该集合。
func (s *WayviaHealthService) Freeze(ctx context.Context, in WayviaHealthSelection) ([]int64, error) {
	ids := in.AccountIDs
	if in.Filters != nil {
		if len(ids) != 0 {
			return nil, infraerrors.BadRequest("HEALTH_SELECTION_INVALID", "账号集合和筛选条件不能同时传入")
		}
		f := in.Filters
		var group int64
		if f.Group != "" {
			if f.Group == "ungrouped" {
				group = -1
			} else if _, err := fmt.Sscan(f.Group, &group); err != nil {
				return nil, infraerrors.BadRequest("HEALTH_FILTER_INVALID", "分组无效")
			}
		}
		rows, err := s.accounts.ListAllWithFilters(ctx, f.Platform, f.Type, f.Status, f.Search, group, f.PrivacyMode)
		if err != nil {
			return nil, err
		}
		ids = make([]int64, 0, len(rows))
		for _, a := range rows {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) > 5000 {
		return nil, infraerrors.BadRequest("HEALTH_SELECTION_TOO_LARGE", "一次最多 5000 个账号，请缩小筛选范围")
	}
	seen := map[int64]bool{}
	result := []int64{}
	for _, id := range ids {
		if id <= 0 {
			return nil, infraerrors.BadRequest("HEALTH_ACCOUNT_INVALID", "账号 ID 无效")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := s.accounts.GetByID(ctx, id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, nil
}

func (s *WayviaHealthService) Apply(ctx context.Context, ids []int64) (int64, error) {
	if _, err := s.Freeze(ctx, WayviaHealthSelection{AccountIDs: ids}); err != nil {
		return 0, err
	}
	// 只给无标记账号纳入继承，不覆盖 exclude / override / 人工调度开关。
	r, err := s.db.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(COALESCE(extra,'{}'),'{wayvia_health_check}','{"mode":"inherit"}'::jsonb),updated_at=NOW() WHERE id=ANY($1) AND deleted_at IS NULL AND NOT (COALESCE(extra,'{}') ? 'wayvia_health_check')`, pq.Array(ids))
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func (s *WayviaHealthService) Coverage(ctx context.Context) (map[string]int64, error) {
	result := map[string]int64{"inherit": 0, "exclude": 0, "override": 0, "unmanaged": 0, "independent_plans": 0}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(extra->'wayvia_health_check'->>'mode','unmanaged'),COUNT(*) FROM accounts WHERE deleted_at IS NULL GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int64
		if err = rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		result[k] = n
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	var n int64
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT account_id) FROM scheduled_test_plans WHERE enabled`).Scan(&n)
	result["independent_plans"] = n
	return result, err
}

func (s *WayviaHealthService) autoAllowed(ctx context.Context, a, owner *Account, cfg wayviahealth.Settings, source string) bool {
	p := healthPolicy(a)
	if !healthInScope(a, cfg) || (p.Mode != "inherit" && p.Mode != "override") || !a.Schedulable || a.Status != StatusActive || !owner.Schedulable || owner.Status != StatusActive {
		return false
	}
	if source == "periodic" && !cfg.PeriodicEnabled || source == "error_recheck" && !cfg.ErrorRecheck {
		return false
	}
	for _, item := range []*Account{a, owner} {
		if item.RateLimitResetAt != nil && item.RateLimitResetAt.After(time.Now()) || item.OverloadUntil != nil && item.OverloadUntil.After(time.Now()) || item.TempUnschedulableUntil != nil && item.TempUnschedulableUntil.After(time.Now()) {
			return false
		}
		if item.AutoPauseOnExpired && item.ExpiresAt != nil && item.ExpiresAt.Before(time.Now()) {
			return false
		}
	}
	if source == "periodic" {
		rows, err := s.db.QueryContext(ctx, `SELECT model_id FROM scheduled_test_plans WHERE account_id IN ($1,$2) AND enabled`, a.ID, owner.ID)
		if err != nil {
			return false
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			if rows.Scan(&model) != nil || !isOpenAIImageModel(a.GetMappedModel(model)) {
				return false
			}
		}
		if rows.Err() != nil {
			return false
		}
		_ = rows.Close()
	}
	// 出口模块仍是最后的出站闸门，不能在这里改变其开关或代理。
	if wayviaegress.Default() != nil && a.IsOpenAIOAuth() {
		if _, err := wayviaegress.Resolve(a.ID, "query"); err != nil {
			return false
		}
	}
	return true
}

func healthAuto(source string) bool { return source == "periodic" || source == "error_recheck" }

func newHealthID() string { return uuid.NewString() }

func (s *WayviaHealthService) managesAccount(ctx context.Context, id int64, model string) bool {
	a, err := s.accounts.GetByID(ctx, id)
	if err != nil || !healthSupported(a) || isOpenAIImageModel(a.GetMappedModel(model)) {
		return false
	}
	owner, err := resolveCredentialAccount(ctx, s.accounts, a)
	return err == nil && healthSupported(owner)
}

package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/wayviahealth"
)

func (s *WayviaHealthService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.stop = cancel
		go func() {
			defer close(s.done)
			tick := time.NewTicker(5 * time.Second)
			defer tick.Stop()
			var workers sync.WaitGroup
			defer workers.Wait()
			lastSweep := time.Time{}
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					cfg, err := s.Settings(ctx)
					if err != nil {
						slog.Warn("wayvia_health_settings_unavailable")
						continue
					}
					if time.Since(lastSweep) >= time.Minute {
						s.sweep(ctx, cfg)
						lastSweep = time.Now()
					}
					rows, err := s.db.QueryContext(ctx, `SELECT id::text FROM wayvia_account_test_runs WHERE state='queued' AND source IN ('batch','periodic','error_recheck') AND not_before<=NOW() ORDER BY not_before LIMIT 10`)
					if err != nil {
						continue
					}
					ids := []string{}
					for rows.Next() {
						var id string
						if rows.Scan(&id) == nil {
							ids = append(ids, id)
						}
					}
					_ = rows.Close()
					for _, id := range ids {
						r, err := s.claim(ctx, id, cfg.Concurrency, cfg.TimeoutSeconds)
						if err != nil {
							continue
						}
						workers.Add(1)
						go func(r *WayviaHealthRun) { defer workers.Done(); s.executeQueued(ctx, r, cfg) }(r)
					}
				}
			}
		}()
	})
}

func (s *WayviaHealthService) Stop() {
	if s == nil || s.stop == nil {
		return
	}
	s.stop()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
}

func (s *WayviaHealthService) executeQueued(ctx context.Context, r *WayviaHealthRun, cfg wayviahealth.Settings) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	defer func() {
		if recover() != nil {
			r.State = "failed"
			r.ErrorClass = "internal"
			r.Reason = "检测执行异常"
			_ = s.finishRun(context.WithoutCancel(ctx), r)
		}
	}()
	if r.Source == "error_recheck" && r.ErrorClass == "auth" {
		// 普通 401 最多刷新一次；只用现有带锁 API，不整列写回请求开始时的凭据。
		a, owner, version, err := s.snapshot(ctx, r.AccountID)
		if err != nil || version != r.CredentialVersion || !healthSupported(owner) || owner.IsOpenAIAgentIdentity() || owner.GetOpenAIRefreshToken() == "" {
			r.State = "cancelled"
			r.Reason = "凭据已更新或不能自动续期"
			_ = s.finishRun(context.WithoutCancel(ctx), r)
			return
		}
		latest, e := s.Settings(ctx)
		if e != nil || latest.Version != r.ConfigVersion || !s.autoAllowed(ctx, a, owner, latest, r.Source) || s.tokens == nil || s.tokens.refreshAPI == nil || s.tokens.executor == nil {
			r.State = "cancelled"
			r.Reason = "策略已关闭或刷新不可用"
			_ = s.finishRun(context.WithoutCancel(ctx), r)
			return
		}
		refreshCtx := wayviahealth.WithCredentialGuard(ctx, owner.ID, owner.Credentials)
		expected, _ := json.Marshal(owner.Credentials)
		executor := healthRefreshExecutor{OAuthRefreshExecutor: s.tokens.executor, expected: string(expected)}
		result, err := s.tokens.refreshAPI.RefreshIfNeeded(refreshCtx, owner, executor, 365*24*time.Hour)
		if err != nil || result == nil || !result.Refreshed || result.Account == nil {
			r.State = "failed"
			r.ErrorClass = "auth"
			r.Reason = "带锁凭据刷新未成功，不发送复检"
			_ = s.finishRun(context.WithoutCancel(ctx), r)
			return
		}
		expectedAccount := *a
		if a.ID == owner.ID {
			expectedAccount.Credentials = result.Account.Credentials
		}
		expectedVersion := s.fingerprint(&expectedAccount, result.Account)
		_, _, current, e := s.snapshot(ctx, r.AccountID)
		if e != nil || current != expectedVersion {
			r.State, r.Reason = "cancelled", "刷新后凭据或配置已再次变化"
			_ = s.finishRun(context.WithoutCancel(ctx), r)
			return
		}
		r.CredentialVersion = current
		_, err = s.db.ExecContext(ctx, `UPDATE wayvia_account_test_runs SET credential_version=$2 WHERE id=$1 AND state='running'`, r.ID, current)
		if err != nil {
			r.State, r.Reason = "cancelled", "刷新版本无法持久化"
			_ = s.finishRun(context.WithoutCancel(ctx), r)
			return
		}
	}
	if healthAuto(r.Source) && cfg.SkipRecentSuccess {
		since := time.Now().Add(-time.Duration(cfg.IntervalMinutes) * time.Minute)
		if r.Options.FaultAt != nil {
			since = *r.Options.FaultAt
		}
		if s.recentSuccess(ctx, r, since) {
			r.State = "skipped"
			r.Reason = "近期已有同凭据、模型和协议的成功证据"
			_ = s.finishRun(context.WithoutCancel(ctx), r)
			return
		}
	}
	_, _ = s.tester.RunTestBackground(ctx, r.AccountID, r.Model, AccountTestOptions{TextContent: r.Options.TestContent, HealthRunID: r.ID, HealthSource: r.Source})
	finishCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer done()
	if stored, err := s.GetRun(finishCtx, r.ID); err == nil && stored.State == "running" {
		r.State, r.ErrorClass, r.Reason = "cancelled", "configuration", "账号已删除或检测入口已改变，本任务未执行"
		_ = s.finishRun(finishCtx, r)
	}
}

func (s *WayviaHealthService) sweep(ctx context.Context, cfg wayviahealth.Settings) {
	// 租约失效只取消；出站后宕机的任务不能立即重发。
	_, _ = s.db.ExecContext(ctx, `UPDATE wayvia_account_test_runs SET state='cancelled',reason='交互检测未及时认领',finished_at=NOW() WHERE state='queued' AND source IN ('manual','scheduled') AND created_at<NOW()-$1*INTERVAL '1 second'`, cfg.TimeoutSeconds+30)
	_, _ = s.db.ExecContext(ctx, `UPDATE wayvia_account_test_runs SET state='cancelled',reason='执行租约过期；等待冷却后重新评估',finished_at=NOW(),lease_until=NULL WHERE state='running' AND lease_until<NOW()`)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM wayvia_account_test_runs WHERE id IN (SELECT id FROM (SELECT id,created_at,ROW_NUMBER() OVER(PARTITION BY account_id ORDER BY created_at DESC) rn FROM wayvia_account_test_runs WHERE state NOT IN ('queued','running')) t WHERE rn>$1 OR created_at<NOW()-$2*INTERVAL '1 day')`, cfg.RetentionCount, cfg.RetentionDays)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM wayvia_health_evidence WHERE success_at<NOW()-INTERVAL '30 days'`)
	if !cfg.PeriodicEnabled {
		return
	}
	accounts, err := s.accounts.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return
	}
	for i := range accounts {
		a := &accounts[i]
		owner, err := resolveCredentialAccount(ctx, s.accounts, a)
		if err != nil || !s.autoAllowed(ctx, a, owner, cfg, "periodic") {
			continue
		}
		// 同一凭据使用确定的错峰时间；独立计划优先，影子与母账号共用活动约束。
		var last sql.NullTime
		if err = s.db.QueryRowContext(ctx, `SELECT MAX(created_at) FROM wayvia_account_test_runs WHERE credential_account_id=$1 AND source IN ('periodic','error_recheck','manual','batch','scheduled')`, owner.ID).Scan(&last); err != nil {
			continue
		}
		if last.Valid && time.Since(last.Time) < time.Duration(cfg.IntervalMinutes)*time.Minute {
			continue
		}
		r, err := s.newRun(ctx, a.ID, "periodic", "", "", nil)
		if err != nil {
			continue
		}
		r.NotBefore = time.Now().Add(time.Duration(owner.ID%60) * time.Second)
		if cfg.SkipRecentSuccess && s.recentSuccess(ctx, r, time.Now().Add(-time.Duration(cfg.IntervalMinutes)*time.Minute)) {
			r.State = "skipped"
			r.Reason = "周期窗口内已有同维度成功证据"
		}
		_, _ = s.insertRun(ctx, r)
	}
}

// ObserveFailure 记录现有请求的证据；仅显式开启后才创建额外模型请求。
func (s *WayviaHealthService) ObserveFailure(ctx context.Context, a *Account, status int, headers http.Header, body []byte, source, model string, stateChanged ...bool) {
	if s == nil || !healthSupported(a) || ctx.Value(healthObservationKey{}) != nil || ctx.Value(healthSessionKey{}) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	a, owner, version := s.observedRequest(ctx, a)
	if !healthSupported(a) || !healthSupported(owner) {
		return
	}
	cfg, err := s.Settings(ctx)
	if err != nil {
		return
	}
	_, _, current, currentErr := s.snapshot(ctx, a.ID)
	if currentErr != nil || current != version {
		return
	}
	code := extractUpstreamErrorCode(body)
	if !safeHealthCode.MatchString(code) {
		code = ""
	}
	class := healthErrorClass(status, code)
	protocol := healthProtocol(a)
	if request, ok := ctx.Value(healthRequestKey{}).(*healthRequestSnapshot); ok {
		protocol = request.protocol
	}
	if source == "quota" || source == "quota_details" || source == "models" {
		protocol = source
	}
	now := time.Now()
	// 辅助错误只保存类型和 HTTP 状态，不保存完整响应体；按分钟合并重复证据。
	var recent bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM wayvia_account_test_runs WHERE credential_account_id=$1 AND source=$2 AND credential_version=$3 AND error_class=$4 AND created_at>NOW()-INTERVAL '1 minute')`, owner.ID, source, version, class).Scan(&recent)
	if err != nil {
		return
	}
	r := &WayviaHealthRun{AccountID: a.ID, CredentialAccountID: owner.ID, Source: source, ConfigVersion: cfg.Version, CredentialVersion: version, Model: model, Protocol: protocol, State: "failed", HTTPStatus: status, ErrorClass: class, ErrorCode: code, Reason: source + "：" + healthFailureReason(owner, status, code, body), CreatedAt: now}
	if isOpenAIPermanentUnauthorizedResponse(body) {
		r.ErrorClass = "auth_revoked"
	}
	if len(stateChanged) > 0 {
		r.StateChanged = stateChanged[0]
	}
	if !recent {
		_, _ = s.insertRun(ctx, r)
	}
	if status == 429 {
		if seconds, err := strconv.Atoi(headers.Get("Retry-After")); err == nil && seconds > 0 && seconds <= 86400*365 {
			r.NotBefore = now.Add(time.Duration(seconds) * time.Second)
		} else if at, err := http.ParseTime(headers.Get("Retry-After")); err == nil {
			r.NotBefore = at
		}
		if at := parseOpenAIRateLimitResetTime(body); at != nil && time.Unix(*at, 0).After(r.NotBefore) {
			r.NotBefore = time.Unix(*at, 0)
		}
	}
	s.scheduleRecheck(ctx, r, cfg)
}

func (s *WayviaHealthService) scheduleRecheck(ctx context.Context, failed *WayviaHealthRun, cfg wayviahealth.Settings) {
	if !cfg.ErrorRecheck || (failed.ErrorClass != "network" && failed.ErrorClass != "upstream" && failed.ErrorClass != "rate_limit" && failed.ErrorClass != "auth") {
		return
	}
	a, owner, version, err := s.snapshot(ctx, failed.AccountID)
	if err != nil || !healthSupported(a) || !healthSupported(owner) || version != failed.CredentialVersion {
		return
	}
	latest, err := s.Settings(ctx)
	if err != nil || latest.Version != cfg.Version || !latest.ErrorRecheck {
		return
	}
	if failed.ErrorClass == "auth" && (owner.Type != AccountTypeOAuth || owner.GetOpenAIRefreshToken() == "") {
		return
	}
	p := healthPolicy(a)
	if !healthInScope(a, cfg) || (p.Mode != "inherit" && p.Mode != "override") || !a.Schedulable || a.Status != StatusActive || !owner.Schedulable || owner.Status != StatusActive {
		return
	}
	// 故障协议与当前检测协议不一致时不能用另一种能力冒充恢复。
	if failed.Protocol != "quota" && failed.Protocol != "quota_details" && failed.Protocol != "models" && failed.Protocol != healthProtocol(a) {
		return
	}
	var last sql.NullTime
	if err = s.db.QueryRowContext(ctx, `SELECT MAX(created_at) FROM wayvia_account_test_runs WHERE credential_account_id=$1 AND source='error_recheck'`, owner.ID).Scan(&last); err != nil {
		return
	}
	if last.Valid && time.Since(last.Time) < time.Duration(cfg.CooldownMinutes)*time.Minute {
		return
	}
	r, err := s.newRun(ctx, a.ID, "error_recheck", "", failed.Model, nil)
	if err != nil {
		return
	}
	now := time.Now()
	r.Options.FaultAt = &now
	r.HTTPStatus, r.ErrorCode, r.ErrorClass = failed.HTTPStatus, failed.ErrorCode, failed.ErrorClass
	r.Reason = "已有请求失败后按策略复检"
	r.NotBefore = now.Add(time.Duration(cfg.CooldownMinutes) * time.Minute)
	if failed.NotBefore.After(r.NotBefore) {
		r.NotBefore = failed.NotBefore
	}
	for _, account := range []*Account{a, owner} {
		for _, at := range []*time.Time{account.RateLimitResetAt, account.TempUnschedulableUntil, account.OverloadUntil} {
			if at != nil && at.After(r.NotBefore) {
				r.NotBefore = *at
			}
		}
	}
	_, _ = s.insertRun(ctx, r)
}

func (s *WayviaHealthService) retryRun(ctx context.Context, r *WayviaHealthRun, cfg wayviahealth.Settings) {
	if r.HTTPStatus == 401 || r.ErrorClass != "network" && r.ErrorClass != "upstream" && r.ErrorClass != "rate_limit" {
		return
	}
	backoff := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 60 * time.Minute}
	if r.Attempt >= len(backoff) {
		return
	}
	latest, err := s.Settings(ctx)
	if err != nil || !latest.ErrorRecheck || latest.Version != cfg.Version {
		return
	}
	next := *r
	next.ID = newHealthID()
	next.State = "queued"
	next.RenderedContent = ""
	next.Attempt++
	next.NotBefore = time.Now().Add(backoff[r.Attempt])
	next.Reason = "临时故障有限退避复检"
	if fresh, err := s.accounts.GetByID(ctx, r.CredentialAccountID); err == nil {
		for _, at := range []*time.Time{fresh.RateLimitResetAt, fresh.TempUnschedulableUntil, fresh.OverloadUntil} {
			if at != nil && at.After(next.NotBefore) {
				next.NotBefore = *at
			}
		}
	}
	_, _ = s.insertRun(ctx, &next)
}

// ObserveSuccess 只接收完整成功的转发结果；WS、工具和专项协议保持各自维度。
func (s *WayviaHealthService) ObserveSuccess(ctx context.Context, a *Account, result *OpenAIForwardResult, completed ...bool) {
	if len(completed) == 0 || !completed[0] || s == nil || !healthSupported(a) || result == nil || result.ClientDisconnect || result.ClientInterrupted || result.OpenAIWSMode || result.ImageCount > 0 {
		return
	}
	if result.UpstreamTerminalEvent != "" && result.UpstreamTerminalEvent != "response.completed" && result.UpstreamTerminalEvent != "response.done" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	a, owner, expected := s.observedRequest(ctx, a)
	if !healthSupported(a) || !healthSupported(owner) {
		return
	}
	_, _, version, err := s.snapshot(ctx, a.ID)
	if err != nil || version != expected {
		return
	}
	protocol := healthProtocol(a)
	if result.UpstreamEndpoint != "" {
		switch result.UpstreamEndpoint {
		case "/v1/responses", "/responses":
			protocol = "responses"
		case "/v1/chat/completions", "/chat/completions":
			protocol = "chat_completions"
		default:
			return
		}
	}
	_ = s.saveSuccess(ctx, &WayviaHealthRun{CredentialAccountID: owner.ID, CredentialVersion: version, Model: result.Model, Protocol: protocol})
}

// 保留设置对象的 JSON 类型校验，供接口与队列复用。
func decodeHealthSettings(raw []byte) (wayviahealth.Settings, error) {
	v := wayviahealth.Defaults()
	err := json.Unmarshal(raw, &v)
	if err == nil {
		err = v.Validate()
	}
	return v, err
}

// 只刷新本次失败对应的快照；锁内重读若已被其他刷新/重新授权替换则结束本任务。
type healthRefreshExecutor struct {
	OAuthRefreshExecutor
	expected string
}

func (e healthRefreshExecutor) matches(a *Account) bool {
	if a == nil {
		return false
	}
	b, _ := json.Marshal(a.Credentials)
	return string(b) == e.expected
}
func (e healthRefreshExecutor) CanRefresh(a *Account) bool {
	return e.matches(a) && e.OAuthRefreshExecutor.CanRefresh(a)
}
func (e healthRefreshExecutor) NeedsRefresh(a *Account, window time.Duration) bool {
	return e.matches(a) && e.OAuthRefreshExecutor.NeedsRefresh(a, window)
}
func (e healthRefreshExecutor) Refresh(ctx context.Context, a *Account) (map[string]any, error) {
	if !e.matches(a) {
		return nil, wayviahealth.ErrStaleCredentials
	}
	return e.OAuthRefreshExecutor.Refresh(ctx, a)
}

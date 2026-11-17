package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/wayviahealth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type healthSessionKey struct{}
type healthObservationKey struct{}
type healthRequestKey struct{}
type healthRequestSnapshot struct {
	account, owner *Account
	version        string
	protocol       string
}

// 请求开始时冻结凭据；结束后不能用刚重新授权的 owner 给旧结果背书。
func (s *WayviaHealthService) requestContext(ctx context.Context, a, owner *Account, protocol string) context.Context {
	state := &healthRequestSnapshot{protocol: protocol}
	if healthSupported(a) {
		if owner == nil {
			owner, _ = resolveCredentialAccount(ctx, s.accounts, a)
		}
		if healthSupported(owner) {
			clone := func(a *Account) *Account {
				b, _ := json.Marshal(a)
				var copy Account
				_ = json.Unmarshal(b, &copy)
				return &copy
			}
			state.account, state.owner = clone(a), clone(owner)
			state.version = s.fingerprint(state.account, state.owner)
		}
	}
	return context.WithValue(ctx, healthRequestKey{}, state)
}

func (s *WayviaHealthService) observedRequest(ctx context.Context, a *Account) (*Account, *Account, string) {
	if v, ok := ctx.Value(healthRequestKey{}).(*healthRequestSnapshot); ok {
		return v.account, v.owner, v.version
	}
	if session, ok := ctx.Value(healthSessionKey{}).(*healthSession); ok {
		return session.account, session.owner, session.run.CredentialVersion
	}
	// 非影子账号自身就是请求快照；影子缺少请求期 owner 时不采信迟到结果。
	if healthSupported(a) && !a.IsCredentialShadow() {
		return a, a, s.fingerprint(a, a)
	}
	return nil, nil, ""
}

type healthSession struct {
	run            *WayviaHealthRun
	account, owner *Account
	complete       bool
}

// HealthTestManaged 区分统一文本检测与保留的专项测试恢复流程。
func HealthTestManaged(ctx context.Context) bool { return ctx.Value(healthSessionKey{}) != nil }

func (s *WayviaHealthService) beforeSend(ctx context.Context) error {
	session, _ := ctx.Value(healthSessionKey{}).(*healthSession)
	if session == nil {
		return nil
	}
	r := session.run
	a, owner, version, err := s.snapshot(ctx, r.AccountID)
	if err != nil || version != r.CredentialVersion {
		r.ErrorClass, r.Reason = "cancelled", "凭据、代理或模型配置已更新，取消旧检测"
		return healthConfigError(r.Reason)
	}
	if healthAuto(r.Source) {
		cfg, err := s.Settings(ctx)
		if err != nil {
			return err
		}
		if cfg.Version != r.ConfigVersion || !sameHealthPolicy(healthPolicy(a), r.Options.Policy) || !s.autoAllowed(ctx, a, owner, cfg, r.Source) {
			r.ErrorClass, r.Reason = "cancelled", "自动策略已改变，取消尚未出站的检测"
			return healthConfigError(r.Reason)
		}
	}
	return nil
}

func (s *WayviaHealthService) prepare(c *gin.Context, a *Account, model, prompt string, opts AccountTestOptions) (string, string, func(error), error) {
	ctx := c.Request.Context()
	content := opts.TextContent
	if content == nil && prompt != "" {
		content = &prompt
	}
	cfg, err := s.Settings(ctx)
	if err != nil {
		return "", "", nil, err
	}
	var r *WayviaHealthRun
	if opts.HealthRunID != "" {
		r, err = s.GetRun(ctx, opts.HealthRunID)
		if err != nil {
			return "", "", nil, err
		}
		if r.AccountID != a.ID || r.State != "running" {
			return "", "", nil, healthConfigError("任务不处于可执行状态")
		}
	} else {
		source := opts.HealthSource
		if source == "" {
			source = "manual"
		}
		r, err = s.newRun(ctx, a.ID, source, "", model, content)
		if err != nil {
			return "", "", nil, err
		}
		r.Options.PlanID = opts.HealthPlanID
		r.Options.ScheduleKey = opts.HealthScheduleKey
		r, err = s.insertRun(ctx, r)
		if err != nil {
			return "", "", nil, err
		}
		if r.State != "queued" {
			return "", "", nil, healthConfigError(r.Reason)
		}
		createdID := r.ID
		r, err = s.claim(ctx, r.ID, cfg.Concurrency, cfg.TimeoutSeconds)
		if err != nil {
			_, _ = s.db.ExecContext(ctx, `UPDATE wayvia_account_test_runs SET state='skipped',reason='检测未获得执行名额',finished_at=NOW() WHERE id=$1 AND state='queued'`, createdID)
			return "", "", nil, err
		}
	}
	// 任何后续失败都结束已认领任务，避免配置错误遗留活动租约。
	fail := func(err error) (string, string, func(error), error) {
		r.State = "cancelled"
		r.Reason = err.Error()
		_ = s.finishRun(context.WithoutCancel(ctx), r)
		return "", "", nil, err
	}
	a, owner, version, err := s.snapshot(ctx, a.ID)
	if err != nil {
		return fail(err)
	}
	if !healthSupported(a) || !healthSupported(owner) {
		return fail(healthConfigError("账号类型已不支持统一文本检测"))
	}
	if version != r.CredentialVersion {
		return fail(healthConfigError("凭据、模型或出口配置已更新"))
	}
	if healthAuto(r.Source) && (!s.autoAllowed(ctx, a, owner, cfg, r.Source) || cfg.Version != r.ConfigVersion || !sameHealthPolicy(healthPolicy(a), r.Options.Policy)) {
		return fail(healthConfigError("自动策略已关闭、账号已暂停或配置已变化"))
	}
	plan, err := s.planContent(ctx, r.Options.PlanID)
	if err != nil {
		return fail(err)
	}
	resolvedModel, template, source := wayviahealth.Resolve(cfg, a.Platform, a.Type, openai.DefaultTestModel, healthPolicy(a), plan, r.Options.TestContent)
	if err := wayviahealth.Validate(template); err != nil {
		return fail(err)
	}
	rendered, err := wayviahealth.Render(template, timezone.Now(), nil)
	if err != nil {
		return fail(err)
	}
	r.Model = resolvedModel
	if r.Options.ModelOverride != "" {
		r.Model = r.Options.ModelOverride
	}
	r.HTTPStatus, r.ErrorCode, r.ErrorClass, r.Reason, r.StateChanged = 0, "", "", "", false
	r.RenderedContent = rendered.Content
	r.ContentSource = source
	r.ConfigVersion = cfg.Version
	if !a.IsModelSupported(r.Model) || isOpenAIImageModel(a.GetMappedModel(r.Model)) {
		return fail(healthConfigError("配置模型不受支持或不是文本模型"))
	}
	r.Protocol = healthProtocol(a)
	_, err = s.db.ExecContext(ctx, `UPDATE wayvia_account_test_runs SET rendered_content=$2,content_source=$3,config_version=$4,model=$5,protocol=$6 WHERE id=$1 AND state='running'`, r.ID, r.RenderedContent, r.ContentSource, r.ConfigVersion, r.Model, r.Protocol)
	if err != nil {
		return fail(err)
	}
	release := func() {}
	if s.concurrency != nil {
		slot, err := s.concurrency.AcquireAccountSlot(ctx, owner.ID, owner.Concurrency)
		if err != nil {
			return fail(err)
		}
		if !slot.Acquired {
			return fail(healthConfigError("账号业务并发已满，检测已跳过"))
		}
		release = slot.ReleaseFunc
	}
	testCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	session := &healthSession{run: r, account: a, owner: owner}
	testCtx = context.WithValue(testCtx, healthSessionKey{}, session)
	c.Request = c.Request.WithContext(testCtx)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	s.tester.sendEvent(c, TestEvent{Type: "health_config", Data: map[string]any{"run_id": r.ID, "source": source, "version": cfg.Version, "content": rendered.Content, "timezone": rendered.Timezone, "warnings": rendered.Warnings}})
	return r.Model, rendered.Content, func(testErr error) {
		defer cancel()
		defer release()
		finishCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		r.State = "success"
		if testErr != nil || !session.complete {
			r.State = "failed"
			if r.ErrorClass == "" {
				r.ErrorClass, r.Reason = healthClassifyError(testErr)
				if !session.complete && testErr == nil {
					r.ErrorClass = "incomplete"
					r.Reason = "未收到完整成功终止事件"
				}
			}
		}
		if r.ErrorClass == "cancelled" {
			r.State = "cancelled"
		}
		fresh, freshOwner, current, readErr := s.snapshot(finishCtx, a.ID)
		if readErr != nil || current != version {
			r.State = "cancelled"
			r.Reason = "账号已删除或凭据、模型、出口已更新"
		} else if r.State == "success" {
			_ = s.saveSuccess(finishCtx, r)
			latest, settingsErr := s.Settings(finishCtx)
			if settingsErr == nil && (latest.AutoRecover || opts.HealthAutoRecover) && latest.Version == cfg.Version && sameHealthPolicy(healthPolicy(fresh), r.Options.Policy) && (!healthAuto(r.Source) || s.autoAllowed(finishCtx, fresh, freshOwner, latest, r.Source)) {
				s.recoverOwnedState(finishCtx, r, fresh, freshOwner)
			}
		}
		if r.State == "failed" && (r.ErrorClass == "network" || r.ErrorClass == "upstream") && readErr == nil && current == version {
			latest, e := s.Settings(finishCtx)
			if e == nil && (!healthAuto(r.Source) || (latest.Version == cfg.Version && sameHealthPolicy(healthPolicy(fresh), r.Options.Policy) && s.autoAllowed(finishCtx, fresh, freshOwner, latest, r.Source))) {
				s.markOwnedFailure(finishCtx, r, fresh, freshOwner)
			}
		}
		if err := s.finishRun(finishCtx, r); err != nil {
			_ = c.Error(err)
		}
		if r.State == "failed" && r.Source == "error_recheck" {
			s.retryRun(finishCtx, r, cfg)
		} else if r.State == "failed" && r.ErrorClass != "incomplete" && r.ErrorClass != "model" && r.ErrorClass != "configuration" {
			// 活动约束释放后才安排按需复检；不把一次手动/计划检测扩成并行请求。
			s.scheduleRecheck(finishCtx, r, cfg)
		}
	}, nil
}

func sameHealthPolicy(a, b wayviahealth.Policy) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}

var safeHealthCode = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`)

func healthErrorClass(status int, code string) string {
	switch {
	case code == "token_revoked" || code == "token_invalidated":
		return "auth_revoked"
	case status == 401:
		return "auth"
	case status == 403:
		return "permission"
	case status == 429:
		return "rate_limit"
	case status >= 500:
		return "upstream"
	case status == 0:
		return "network"
	default:
		return "configuration"
	}
}

func healthClassifyError(err error) (string, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "network", "检测请求超时"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled", "检测请求已取消"
	}
	var network net.Error
	if errors.As(err, &network) {
		return "network", "网络连接失败"
	}
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "response.incomplete") || strings.Contains(msg, "before response.completed") || strings.Contains(msg, "before [done]") {
			return "incomplete", "上游响应不完整"
		}
		if strings.Contains(msg, "request failed") || strings.Contains(msg, "stream read") || strings.Contains(msg, "timeout") {
			return "network", "网络请求或流读取失败"
		}
	}
	return "model", "模型请求未完整成功"
}

// HandleFailure 只在请求快照仍有效时调用现有分类器，绝不回写旧 RT。
func (s *WayviaHealthService) HandleFailure(ctx context.Context, a *Account, status int, headers http.Header, body []byte, source, model string) {
	if !healthSupported(a) {
		return
	}
	a, owner, expected := s.observedRequest(ctx, a)
	if !healthSupported(a) || !healthSupported(owner) {
		return
	}
	fresh, freshOwner, version, err := s.snapshot(ctx, a.ID)
	if err != nil || version != expected {
		return
	}
	code := extractUpstreamErrorCode(body)
	if !safeHealthCode.MatchString(code) {
		code = ""
	}
	session, _ := ctx.Value(healthSessionKey{}).(*healthSession)
	if session != nil {
		source = session.run.Source
		r := session.run
		r.HTTPStatus, r.ErrorCode, r.ErrorClass, r.Reason = status, code, healthErrorClass(status, code), healthFailureReason(owner, status, code, body)
		if isOpenAIPermanentUnauthorizedResponse(body) {
			r.ErrorClass = "auth_revoked"
		}
		// 已出站自动任务在关闭/改策略后只记录；明确撤销仍走统一安全规则。
		if healthAuto(source) && code != "token_revoked" && code != "token_invalidated" && !isOpenAIPermanentUnauthorizedResponse(body) {
			cfg, e := s.Settings(ctx)
			if e != nil || cfg.Version != r.ConfigVersion || !sameHealthPolicy(healthPolicy(fresh), r.Options.Policy) || !s.autoAllowed(ctx, fresh, freshOwner, cfg, source) {
				return
			}
		}
	}
	safeBody := healthFailureEvidence(fresh, status, body)
	stateCtx := context.WithValue(ctx, healthObservationKey{}, true)
	stateCtx = wayviahealth.WithCredentialGuard(stateCtx, freshOwner.ID, freshOwner.Credentials)
	if fresh.ID != freshOwner.ID {
		stateCtx = wayviahealth.WithCredentialGuard(stateCtx, fresh.ID, fresh.Credentials)
	}
	before := healthState(fresh, freshOwner)
	if s.rateLimit != nil {
		// 模型权限判断使用原始证据，但该分支只持久化固定原因，不保存响应正文。
		if !s.rateLimit.HandleUpstreamModelNotFound(stateCtx, fresh, model, status, body) {
			s.rateLimit.HandleUpstreamError(stateCtx, fresh, status, headers, safeBody, model)
		}
	}
	after, afterOwner, _, readErr := s.snapshot(ctx, a.ID)
	changed := readErr == nil && before != healthState(after, afterOwner)
	if session != nil {
		session.run.StateChanged = session.run.StateChanged || changed
		return
	}
	s.ObserveFailure(ctx, fresh, status, headers, body, source, model, changed)
}

// 保留分类所需的结构与已命中的规则关键词，不让上游回显的模板/凭据进入日志。
func healthFailureEvidence(a *Account, status int, body []byte) []byte {
	if isHTMLResponse(body) {
		return []byte(`<html>upstream access challenge</html>`)
	}
	if isCloudflareBotBlockResponse(body) {
		return []byte(`{"cloudflare_error":true,"code":1010}`)
	}
	errorFields := map[string]any{"message": fmt.Sprintf("upstream HTTP %d", status)}
	for _, key := range []string{"code", "type", "plan_type"} {
		value := gjson.GetBytes(body, "error."+key).String()
		if safeHealthCode.MatchString(value) {
			errorFields[key] = value
		}
	}
	if code := extractUpstreamErrorCode(body); safeHealthCode.MatchString(code) {
		errorFields["code"] = code
	}
	if reset := parseOpenAIRateLimitResetTime(body); reset != nil {
		errorFields["resets_at"] = *reset
	}
	message := strings.ToLower(extractUpstreamErrorMessage(body))
	for _, known := range []string{"organization has been disabled", "identity verification is required"} {
		if strings.Contains(message, known) {
			errorFields["message"] = known
		}
	}
	result := map[string]any{"error": errorFields}
	if isOpenAIPermanentUnauthorizedResponse(body) {
		result["detail"] = "Unauthorized"
	} else if gjson.GetBytes(body, "detail.code").String() == "deactivated_workspace" {
		result["detail"] = map[string]string{"code": "deactivated_workspace"}
	}
	keywords := []string{}
	for _, match := range matchTempUnschedulableRules(a, status, body) {
		keywords = append(keywords, match.matchedKeyword)
	}
	result["policy_evidence"] = keywords
	safe, _ := json.Marshal(result)
	return safe
}

func healthFailureReason(owner *Account, status int, code string, body []byte) string {
	switch {
	case code == "token_revoked" || code == "token_invalidated" || isOpenAIPermanentUnauthorizedResponse(body):
		return "认证已明确失效，需要重新授权"
	case status == 401 && owner.Type == AccountTypeOAuth && owner.GetOpenAIRefreshToken() == "":
		return "OAuth 认证失败且缺少刷新令牌，等待更新凭据"
	case status == 401 && owner.Type == AccountTypeOAuth:
		return "OAuth 认证未通过，按统一规则等待凭据续期"
	case status == 401:
		return "模型认证未通过，请检查 API Key"
	case status == 403 && (isHTMLResponse(body) || isCloudflareBotBlockResponse(body)):
		return "边缘或代理拦截，不作为账号认证失效证据"
	case status == 429:
		return "上游限流或额度不足，等待冷却及重置"
	default:
		return fmt.Sprintf("上游 HTTP %d", status)
	}
}

func healthState(accounts ...*Account) string {
	states := []any{}
	for _, a := range accounts {
		if a != nil {
			states = append(states, []any{a.Status, a.Schedulable, a.ErrorMessage, a.TempUnschedulableUntil, a.TempUnschedulableReason, a.RateLimitResetAt, a.OverloadUntil, a.Extra["model_rate_limits"]})
		}
	}
	b, _ := json.Marshal(states)
	return string(b)
}

func (s *WayviaHealthService) recoverOwnedState(ctx context.Context, r *WayviaHealthRun, a, owner *Account) {
	// 本模块只恢复自己写入的临时标记；不清认证 error、额度、模型限流、辅助查询状态或人工开关。
	if a.Status != StatusActive || !a.Schedulable || !strings.HasPrefix(a.TempUnschedulableReason, "wayvia_health:") {
		return
	}
	blocked, err := s.GetRun(ctx, strings.TrimPrefix(a.TempUnschedulableReason, "wayvia_health:"))
	if err != nil || blocked.Model != r.Model || blocked.Protocol != r.Protocol || blocked.CredentialVersion != r.CredentialVersion || r.StartedAt == nil || !r.StartedAt.After(blocked.CreatedAt) {
		return
	}
	credentials, _ := json.Marshal(a.Credentials)
	ownerCredentials, _ := json.Marshal(owner.Credentials)
	result, err := s.db.ExecContext(ctx, `WITH changed AS (UPDATE accounts SET temp_unschedulable_until=NULL,temp_unschedulable_reason=NULL,updated_at=NOW() WHERE id=$1 AND status='active' AND schedulable AND temp_unschedulable_reason=$2 AND credentials=$4::jsonb AND EXISTS(SELECT 1 FROM accounts owner WHERE owner.id=$5 AND owner.credentials=$6::jsonb AND owner.deleted_at IS NULL) RETURNING id) INSERT INTO scheduler_outbox(event_type,account_id) SELECT $3,id FROM changed`, a.ID, a.TempUnschedulableReason, SchedulerOutboxEventAccountChanged, string(credentials), owner.ID, string(ownerCredentials))
	if err != nil {
		return
	}
	n, _ := result.RowsAffected()
	r.StateChanged = n > 0
	if n > 0 && s.audit != nil {
		s.audit.Record(&AuditLog{ActorRole: "system", Action: "admin.account_health.auto_recover", StatusCode: 200, Extra: map[string]any{"account_id": a.ID, "operation_id": r.ID}})
	}
}

func (s *WayviaHealthService) markOwnedFailure(ctx context.Context, r *WayviaHealthRun, a, owner *Account) {
	if a == nil || a.Status != StatusActive || !a.Schedulable {
		return
	}
	credentials, _ := json.Marshal(a.Credentials)
	ownerCredentials, _ := json.Marshal(owner.Credentials)
	// 仅创建自己的短期标记，不替换已有的人工、认证、额度或模型限流原因。
	result, err := s.db.ExecContext(ctx, `WITH changed AS (UPDATE accounts SET temp_unschedulable_until=NOW()+INTERVAL '1 minute',temp_unschedulable_reason=$2,updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL AND status='active' AND schedulable AND credentials=$3::jsonb AND EXISTS(SELECT 1 FROM accounts owner WHERE owner.id=$5 AND owner.credentials=$6::jsonb AND owner.deleted_at IS NULL) AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until<=NOW()) AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at<=NOW()) AND (overload_until IS NULL OR overload_until<=NOW()) RETURNING id) INSERT INTO scheduler_outbox(event_type,account_id) SELECT $4,id FROM changed`, a.ID, "wayvia_health:"+r.ID, string(credentials), SchedulerOutboxEventAccountChanged, owner.ID, string(ownerCredentials))
	if err == nil {
		n, _ := result.RowsAffected()
		r.StateChanged = r.StateChanged || n > 0
	}
}

func isOpenAIPermanentUnauthorizedResponse(body []byte) bool {
	return gjson.GetBytes(body, "detail").String() == "Unauthorized"
}

func (s *AccountTestService) handleHealthTestFailure(ctx context.Context, a *Account, status int, headers http.Header, body []byte, model string) {
	if s.health != nil && HealthTestManaged(ctx) {
		s.health.HandleFailure(ctx, a, status, headers, body, "manual", model)
		return
	}
	if s.openaiGatewayService != nil {
		s.openaiGatewayService.handleOpenAIAccountUpstreamError(ctx, a, status, headers, body, model)
	}
}

// HTTP 200 内的结构化流错误仍按明确错误码归类，未知业务失败不升级成认证失效。
func (s *AccountTestService) handleHealthStreamFailure(c *gin.Context, data map[string]any) {
	if s.health == nil {
		return
	}
	session, ok := c.Request.Context().Value(healthSessionKey{}).(*healthSession)
	if !ok {
		return
	}
	errValue := data["error"]
	if response, ok := data["response"].(map[string]any); ok {
		errValue = response["error"]
	}
	body, _ := json.Marshal(map[string]any{"error": errValue})
	code := extractUpstreamErrorCode(body)
	status := 0
	switch code {
	case "token_revoked", "token_invalidated", "invalid_api_key", "invalid_authentication":
		status = 401
	case "rate_limit_exceeded", "usage_limit_reached", "insufficient_quota":
		status = 429
	case "model_not_found":
		status = 404
	case "server_error", "upstream_error":
		status = 502
	}
	if status != 0 {
		s.health.HandleFailure(c.Request.Context(), session.account, status, http.Header{}, body, session.run.Source, session.run.Model)
		session.run.HTTPStatus = 200
		session.run.Reason = "上游流错误：" + session.run.Reason
	}
}

package wayviaegress

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"
)

func (m *Manager) ownerLock(ctx context.Context, owner string) (func(), error) {
	v, _ := m.refreshLocks.LoadOrStore(owner, make(chan struct{}, 1))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Refresh 保持 owner 锁和请求租约直到 CAS 提交。业务取消不丢弃新 Token。
func (m *Manager) Refresh(ctx context.Context, id int64, exchange func(context.Context, Account) (map[string]any, error)) (map[string]any, error) {
	return m.refresh(ctx, id, exchange, false)
}

// enrollment 仅由持有 Control 锁的启用流程使用，普通刷新不绕过新账号门控。
func (m *Manager) refresh(ctx context.Context, id int64, exchange func(context.Context, Account) (map[string]any, error), enrollment bool) (map[string]any, error) {
	if !ControlledRefresh(id) {
		return nil, Conflict("该账号不支持受控刷新")
	}
	r, err := m.refreshRoute(id, enrollment)
	if err != nil {
		return nil, err
	}
	unlock, err := m.ownerLock(ctx, r.Owner)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 120*time.Second)
	defer cancel()
	r, err = m.refreshRoute(id, enrollment)
	if err != nil {
		return nil, err
	}
	lease, err := m.Begin(ctx, r)
	if err != nil {
		return nil, err
	}
	defer lease.End()
	r.drainingRefresh = true
	m.mu.Lock()
	a, p, err := m.rootLocked(id)
	window := m.refreshWindow
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var old []byte
	err = m.db.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id=$1 AND deleted_at IS NULL`, a.ID).Scan(&old)
	if err != nil {
		return nil, err
	}
	a.Credentials = nil // 账号快照中的map不可原地修改。
	if err = json.Unmarshal(old, &a.Credentials); err != nil {
		return nil, err
	}
	a.Identity, _ = a.Credentials["chatgpt_user_id"].(string)
	if p.Identity != "" && p.Identity != a.Identity {
		return nil, Conflict("刷新前账号身份与原owner不一致，请关联授权")
	}
	// RT 刷新不能要求旧 AT/ID Token 有效，否则到期账号永远无法恢复。
	// 启用流程等待 owner 锁后再检查最新 AT，避免重复使用已经轮换的 RT。
	if enrollment {
		raw, _ := a.Credentials["access_token"].(string)
		if token, verifyErr := VerifyAccessToken(ctx, raw); verifyErr == nil && !accessTokenNeedsRefresh(a, token, window) {
			return a.Credentials, nil
		}
	}
	marker := filepath.Join(m.config.StateDir, "refresh-"+p.Owner+".json")
	if err = durableJSON(marker, map[string]any{"owner_id": p.Owner, "account_id": a.ID, "state": "in_flight"}); err != nil {
		return nil, err
	}
	newCredentials, err := exchange(context.WithValue(WithAccount(lease.Context, a.ID, "refresh"), routeContextKey{}, r), a)
	if err != nil {
		m.uncertain(p)
		return nil, Error("EGRESS_CREDENTIAL_UNCERTAIN", "刷新结果不确定，已停止自动重试")
	}
	if token, _ := newCredentials["access_token"].(string); token == "" {
		m.uncertain(p)
		return nil, Error("EGRESS_CREDENTIAL_UNCERTAIN", "刷新响应缺少访问令牌")
	}
	if idValue, _ := newCredentials["chatgpt_user_id"].(string); idValue != "" && (p.Identity != "" && idValue != p.Identity || a.Identity != "" && idValue != a.Identity) {
		m.uncertain(p)
		return nil, Error("EGRESS_CREDENTIAL_UNCERTAIN", "刷新返回身份不一致")
	}
	for k, v := range a.Credentials {
		if _, ok := newCredentials[k]; !ok {
			newCredentials[k] = v
		}
	}
	newCredentials["_token_version"] = time.Now().UnixMilli()
	next, err := json.Marshal(newCredentials)
	if err != nil {
		m.uncertain(p)
		return nil, err
	}
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer saveCancel()
	tx, err := m.db.BeginTx(saveCtx, nil)
	if err != nil {
		m.uncertain(p)
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(saveCtx, `SELECT set_config('wayvia.credential_write','controlled',true)`)
	if err == nil {
		result, e := tx.ExecContext(saveCtx, `UPDATE accounts SET credentials=$2::jsonb,updated_at=now() WHERE id=$1 AND credentials=$3::jsonb AND deleted_at IS NULL AND EXISTS(SELECT 1 FROM wayvia_account_egress_policies WHERE owner_id=$4 AND root_account_id=$1 AND (identity_key IS NULL OR identity_key=NULLIF($5,'')))`, a.ID, string(next), string(old), p.Owner, a.Identity)
		err = e
		if err == nil {
			n, e := result.RowsAffected()
			if e != nil {
				err = e
			} else if n != 1 {
				err = Conflict("刷新凭据CAS冲突")
			}
		}
	}
	if err == nil {
		err = execOne(saveCtx, tx, `UPDATE wayvia_account_egress_policies SET identity_key=COALESCE(identity_key,NULLIF($2,'')) WHERE owner_id=$1 AND (identity_key IS NULL OR identity_key=NULLIF($2,''))`, p.Owner, a.Identity)
	}
	if err == nil {
		err = publishCredentialChange(saveCtx, tx, a.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		tx.Rollback()
		m.uncertain(p)
		return nil, Error("EGRESS_CREDENTIAL_UNCERTAIN", "刷新结果保存未确认")
	}
	// 提交成功但快照发布失败仍须关门，不能继续使用缓存中的旧 Refresh Token。
	if err = m.Reload(saveCtx); err != nil {
		m.uncertain(p)
		return nil, Error("EGRESS_CREDENTIAL_UNCERTAIN", "凭据已提交但快照发布未确认")
	}
	if err = removeDurable(marker); err != nil {
		m.uncertain(p)
		return nil, err
	}
	return newCredentials, nil
}

func (m *Manager) refreshRoute(id int64, enrollment bool) (Route, error) {
	if !enrollment {
		return m.Resolve(id, "refresh")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, p, err := m.rootLocked(id)
	if err != nil {
		return Route{}, err
	}
	// 已启用账号仍走原固定地址和正常就绪检查，禁止回退普通出口。
	if p.Mode == Strict {
		return m.routeLocked(id, "refresh")
	}
	if m.initError != "" || m.closing || m.config.StateDir == "" || m.lock == nil || !m.runtime.Enabled {
		return Route{}, Error("EGRESS_PAUSED", "出口模块未就绪，无法刷新启用凭据")
	}
	if a.ID != id || !a.OrdinaryOAuth() || a.ProxyID != 0 || (p.State != "legacy" && p.State != "pending_enrollment" && p.State != "blocked") {
		return Route{}, Conflict("账号状态不允许启用前刷新")
	}
	r := Route{AccountID: id, Owner: p.Owner, Mode: Legacy, PolicyVersion: p.Version, BindingVersion: p.BindingVersion, Revision: m.runtime.Revision, Purpose: "refresh", control: true}
	e := m.epochs[r.Key()]
	if e == nil || e.ctx.Err() != nil {
		ectx, cancel := context.WithCancel(context.Background())
		e = &epoch{ctx: ectx, cancel: cancel, connections: map[*trackedConn]struct{}{}, leases: map[*Lease]struct{}{}}
		m.epochs[r.Key()] = e
	}
	r.epoch = e
	return r, r.Check()
}
func (m *Manager) uncertain(p Policy) {
	m.closeOwner(p.Owner, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = m.db.ExecContext(ctx, `UPDATE wayvia_account_egress_policies SET state='credential_uncertain',last_error='EGRESS_CREDENTIAL_UNCERTAIN' WHERE owner_id=$1`, p.Owner)
	_ = m.Reload(ctx)
}

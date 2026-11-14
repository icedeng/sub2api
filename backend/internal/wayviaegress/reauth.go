package wayviaegress

import (
	"context"
	"encoding/json"
	"net/netip"
	"path/filepath"
	"time"
)

type routeContextKey struct{}

func RouteForContext(ctx context.Context) (Route, error) {
	if r, ok := ctx.Value(routeContextKey{}).(Route); ok {
		return r, r.Check()
	}
	id, purpose := AccountContext(ctx)
	return Resolve(id, purpose)
}

// ReauthVersion 只创建授权会话，不放行业务或刷新。
func (m *Manager) ReauthVersion(id int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.initError != "" || m.config.StateDir == "" || m.lock == nil {
		return 0, Error("EGRESS_NOT_CONFIGURED", "出口模块配置不可用")
	}
	if m.closing {
		return 0, Error("EGRESS_PAUSED", "服务正在停止")
	}
	a, p, err := m.rootLocked(id)
	if err != nil {
		return 0, err
	}
	if a.ID != id || !a.OrdinaryOAuth() {
		return 0, Conflict("请选择普通 OpenAI OAuth 母账号")
	}
	if p.State == "preparing" || p.State == "pausing" {
		return 0, Conflict("先等待当前操作结束")
	}
	if p.State == "retired" {
		return 0, Conflict("先显式恢复已删除账号")
	}
	return p.Version, nil
}

// Reauthorize 只授予一次token端点控制租约，保存后仍保持blocked，必须显式恢复核验。
func (m *Manager) Reauthorize(ctx context.Context, id, version int64, exchange func(context.Context) (map[string]any, error)) (map[string]any, error) {
	Control.Lock()
	defer Control.Unlock()
	if err := m.Reload(ctx); err != nil {
		return nil, err
	}
	var active bool
	if err := m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM wayvia_egress_operations WHERE phase IN ('accepted','draining','preparing'))`).Scan(&active); err != nil {
		return nil, err
	}
	if active {
		return nil, Conflict("等待当前出口操作完成")
	}
	v, err := m.ReauthVersion(id)
	if err != nil || v != version {
		if err != nil {
			return nil, err
		}
		return nil, Conflict("授权会话策略版本已失效")
	}
	m.mu.Lock()
	_, p, _ := m.rootLocked(id)
	m.mu.Unlock()
	unlock, err := m.ownerLock(ctx, p.Owner)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// 先持久化关门意图，再递增策略版本；任何崩溃点都保持不可调度。
	marker := filepath.Join(m.config.StateDir, "refresh-"+p.Owner+".json")
	if err = durableJSON(marker, map[string]any{"owner_id": p.Owner, "account_id": id, "state": "reauth"}); err != nil {
		return nil, err
	}
	m.closeOwner(p.Owner, true)
	if err = execOne(ctx, m.db, `UPDATE wayvia_account_egress_policies SET policy_version=policy_version+1,state='credential_uncertain' WHERE owner_id=$1 AND policy_version=$2`, p.Owner, p.Version); err != nil {
		m.uncertain(p)
		return nil, err
	}
	p.Version++
	if err = m.Reload(ctx); err != nil {
		m.uncertain(p)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 120*time.Second)
	defer cancel()
	// 首次启用失败可能只有 target=strict、尚无可用绑定；此时仍允许授权控制流程。
	// 已真正启用过固定出口的账号始终使用原地址，不得退回普通出口交换凭据。
	useFixedRoute := p.Mode == Strict
	if useFixedRoute {
		err = m.validateBinding(ctx, p)
		if err == nil {
			err = m.agent(ctx, "check", p)
		}
		if err == nil {
			_, err = m.probe(ctx, p)
		}
		if err != nil {
			m.setBlocked(p, "EGRESS_REAUTH_PRECHECK_FAILED")
			return nil, err
		}
	}
	if err = m.waitBusiness(ctx, p.Owner); err != nil {
		return nil, err
	}
	var old []byte
	if err = m.db.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&old); err != nil {
		m.uncertain(p)
		return nil, err
	}
	ectx, ecancel := context.WithCancel(context.Background())
	e := &epoch{ctx: ectx, cancel: ecancel, connections: map[*trackedConn]struct{}{}, leases: map[*Lease]struct{}{}}
	m.mu.Lock()
	r := Route{AccountID: id, Owner: p.Owner, Mode: p.Mode, Source: netip.Addr{}, PolicyVersion: p.Version, BindingVersion: p.BindingVersion, Revision: m.runtime.Revision, Purpose: "refresh", epoch: e, control: true}
	if useFixedRoute {
		r.Mode = Strict
		r.Source, _ = netip.ParseAddr(p.Source)
	}
	m.epochs[r.Key()] = e
	lctx, lcancel := context.WithCancel(ctx)
	l := &Lease{Route: r, Context: lctx, cancel: lcancel, manager: m}
	e.leases[l] = struct{}{}
	m.mu.Unlock()
	defer l.End()
	defer ecancel()
	defer func() {
		m.closeOwner(p.Owner, true)
		m.mu.Lock()
		now := m.policies[id]
		if !now.Strict() && now.State == "legacy" {
			delete(m.blocked, p.Owner)
		}
		m.mu.Unlock()
	}()
	// 保持控制锁直到CAS和租约清理完成；网络有总超时。
	next, err := exchange(context.WithValue(WithAccount(lctx, id, "reauth"), routeContextKey{}, r))
	if err != nil {
		m.uncertain(p)
		return nil, Error("EGRESS_CREDENTIAL_UNCERTAIN", "重新授权交换或验签未完成，请重新生成授权会话")
	}
	identity, _ := next["chatgpt_user_id"].(string)
	if token, _ := next["access_token"].(string); token == "" {
		m.uncertain(p)
		return nil, Error("EGRESS_CREDENTIAL_UNCERTAIN", "授权响应缺少访问令牌")
	}
	if identity == "" || p.Identity != "" && identity != p.Identity {
		m.uncertain(p)
		return nil, Conflict("重新授权身份必须与原owner一致")
	}
	var original map[string]any
	if err = json.Unmarshal(old, &original); err != nil {
		m.uncertain(p)
		return nil, err
	}
	if refresh, _ := next["refresh_token"].(string); refresh == "" {
		m.uncertain(p)
		return nil, Error("EGRESS_CREDENTIAL_UNCERTAIN", "重新授权未返回Refresh Token，不能沿用待确认旧值")
	}
	for k, v := range original {
		if _, ok := next[k]; !ok {
			next[k] = v
		}
	}
	next["_token_version"] = time.Now().UnixMilli()
	raw, _ := json.Marshal(next)
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
		res, e := tx.ExecContext(saveCtx, `UPDATE accounts SET credentials=$2::jsonb,updated_at=now() WHERE id=$1 AND credentials=$3::jsonb AND deleted_at IS NULL`, id, string(raw), string(old))
		err = e
		if err == nil {
			n, e := res.RowsAffected()
			if e != nil {
				err = e
			} else if n != 1 {
				err = Conflict("重新授权凭据CAS失败")
			}
		}
	}
	if err == nil {
		err = execOne(saveCtx, tx, `UPDATE wayvia_account_egress_policies SET state=CASE WHEN current_mode='legacy' AND target_mode='legacy' THEN CASE WHEN require_ipv6 THEN 'pending_enrollment' ELSE 'legacy' END ELSE 'blocked' END,identity_key=COALESCE(identity_key,$3),last_error='' WHERE owner_id=$1 AND policy_version=$2`, p.Owner, p.Version, identity)
	}
	if err == nil {
		err = publishCredentialChange(saveCtx, tx, id)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		tx.Rollback()
		m.uncertain(p)
		return nil, err
	}
	if err = m.Reload(saveCtx); err != nil {
		m.uncertain(p)
		return nil, Error("EGRESS_CREDENTIAL_UNCERTAIN", "重新授权已提交但快照发布未确认")
	}
	if err = removeDurable(marker); err != nil {
		m.uncertain(p)
		return nil, err
	}
	if !p.Strict() {
		m.mu.Lock()
		delete(m.blocked, p.Owner)
		m.mu.Unlock()
	}
	return next, nil
}

package wayviaegress

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

type Change struct {
	Kind              string `json:"kind"`
	AccountID         int64  `json:"account_id"`
	Mode              string `json:"mode"`
	Enabled           bool   `json:"enabled"`
	ExpectedVersion   int64  `json:"expected_policy_version"`
	ExpectedRevision  int64  `json:"expected_revision"`
	IdempotencyKey    string `json:"idempotency_key"`
	Acknowledge       bool   `json:"acknowledge_stream_interrupt"`
	AcknowledgeLegacy bool   `json:"acknowledge_legacy_egress"`
	BindingID         string `json:"binding_id,omitempty"`
	EnableScheduling  bool   `json:"enable_scheduling,omitempty"`
	automatic         bool
	recovering        bool
}
type Operation struct {
	ID         string     `json:"operation_id"`
	Owner      string     `json:"owner_id,omitempty"`
	Kind       string     `json:"kind"`
	Target     string     `json:"target"`
	Phase      string     `json:"phase"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
type pending struct {
	Operation       Operation `json:"operation"`
	Owners          []string  `json:"owners"`
	EnableAccountID int64     `json:"enable_account_id,omitempty"`
}

func (m *Manager) Operation(ctx context.Context, id string) (Operation, error) {
	var op Operation
	err := m.db.QueryRowContext(ctx, `SELECT id::text,COALESCE(owner_id::text,''),kind,target,phase,error,created_at,finished_at FROM wayvia_egress_operations WHERE id=$1`, id).Scan(&op.ID, &op.Owner, &op.Kind, &op.Target, &op.Phase, &op.Error, &op.CreatedAt, &op.FinishedAt)
	return op, err
}

func (m *Manager) Change(ctx context.Context, c Change, actor string) (Operation, error) {
	Control.Lock()
	defer Control.Unlock()
	m.mu.Lock()
	closing := m.closing
	m.mu.Unlock()
	if closing {
		return Operation{}, Error("EGRESS_PAUSED", "服务正在停止")
	}
	if m.initError != "" {
		return Operation{}, Error("EGRESS_POLICY_UNKNOWN", m.initError)
	}
	if m.config.StateDir == "" || m.lock == nil {
		return Operation{}, Error("EGRESS_NOT_CONFIGURED", "需先完成宿主辅助服务及共享持久目录配置")
	}
	if len(c.IdempotencyKey) < 8 || len(c.IdempotencyKey) > 128 {
		return Operation{}, Conflict("需要8至128字符幂等键")
	}
	b, _ := json.Marshal(c)
	hash := fmt.Sprintf("%x", sha256.Sum256(b))
	var existing, oldHash string
	err := m.db.QueryRowContext(ctx, `SELECT id::text,request_hash FROM wayvia_egress_operations WHERE idempotency_key=$1`, c.IdempotencyKey).Scan(&existing, &oldHash)
	if err == nil {
		if hash != oldHash {
			return Operation{}, Conflict("幂等键已用于不同请求")
		}
		return m.Operation(ctx, existing)
	}
	if err != sql.ErrNoRows {
		return Operation{}, err
	}
	if err = m.Reload(ctx); err != nil {
		return Operation{}, err
	}
	var active string
	if err := m.db.QueryRowContext(ctx, `SELECT id::text FROM wayvia_egress_operations WHERE phase IN ('accepted','draining','preparing') LIMIT 1`).Scan(&active); err == nil {
		return Operation{}, Conflict("等待当前出口操作完成: " + active)
	} else if err != sql.ErrNoRows {
		return Operation{}, err
	}
	m.mu.Lock()
	runtime := m.runtime
	if c.Kind == "restore-account" {
		m.mu.Unlock()
		return m.restoreAccount(ctx, c, actor, hash)
	}
	var p Policy
	var a Account
	if c.Kind != "runtime" && c.AccountID <= 0 {
		m.mu.Unlock()
		return Operation{}, Conflict("需要账号ID")
	}
	if c.AccountID > 0 {
		var e error
		a, p, e = m.rootLocked(c.AccountID)
		if e != nil {
			m.mu.Unlock()
			return Operation{}, e
		}
		if a.ID != c.AccountID || !a.Eligible() {
			m.mu.Unlock()
			return Operation{}, Conflict("仅允许配置OpenAI OAuth母账号")
		}
		if c.automatic && (!p.RequireIPv6 || p.Mode != Legacy || a.Status != "active" || a.Schedulable || p.State == "credential_uncertain" || p.State == "retired") {
			m.mu.Unlock()
			return Operation{}, Error("EGRESS_AUTO_ENROLLMENT_CHANGED", "账号已不属于待自动启用范围")
		}
	}
	if c.Kind == "recover" && (!c.recovering || p.Mode != Strict || p.Target != Strict || p.State != "blocked" || p.BindingID == "" || p.RecoveryState != "running") {
		m.mu.Unlock()
		return Operation{}, Error("EGRESS_RECOVERY_CHANGED", "恢复任务已失效")
	}
	if c.recovering && (a.Status != "active" || !a.Schedulable || !runtime.Enabled) {
		m.mu.Unlock()
		return Operation{}, Error("EGRESS_RECOVERY_DEFERRED", "账号或固定出口已暂停，等待恢复")
	}
	var owners []string
	if c.Kind == "runtime" {
		if runtime.Revision != c.ExpectedRevision {
			m.mu.Unlock()
			return Operation{}, Conflict("全局版本已变化")
		}
		for _, v := range m.policies {
			// 首次自动启用仍由队列完成核验和开放调度；全局恢复不能将它提前收尾。
			if v.Mode == Legacy && v.RequireIPv6 && v.AutoEnrollment != "" && v.AutoEnrollment != "completed" && v.AutoEnrollment != "cancelled" {
				continue
			}
			if v.Strict() && v.State != "retired" {
				if c.Enabled && v.State == "credential_uncertain" {
					m.mu.Unlock()
					return Operation{}, Error("EGRESS_CREDENTIAL_UNCERTAIN", "请先完成待确认账号的关联授权")
				}
				owners = append(owners, v.Owner)
			}
		}
	} else {
		if c.ExpectedVersion != p.Version {
			m.mu.Unlock()
			return Operation{}, Conflict("策略版本已变化")
		}
		owners = []string{p.Owner}
	}
	// active operation查询已串行化执行中任务；失败遗留preparing允许显式重试。
	if p.State == "retired" {
		m.mu.Unlock()
		return Operation{}, Conflict("已退役owner只能恢复绑定")
	}
	if p.State == "credential_uncertain" && c.Kind != "restore-binding" {
		m.mu.Unlock()
		return Operation{}, Error("EGRESS_CREDENTIAL_UNCERTAIN", "先完成账号关联重新授权")
	}
	plugin := m.pluginEnabled
	m.mu.Unlock()
	if !c.Acknowledge && c.Kind != "probe" {
		return Operation{}, Conflict("请确认会中断现有流式请求")
	}
	target := c.Mode
	if c.EnableScheduling && (c.Kind != "mode" || target != Strict) {
		return Operation{}, Conflict("只有启用固定 IPv6 操作可以同时开放账号调度")
	}
	switch c.Kind {
	case "runtime":
		if c.Enabled {
			target = "enabled"
		} else {
			target = "paused"
		}
	case "mode":
		if target != Legacy && target != Strict {
			return Operation{}, Conflict("未知模式")
		}
		if target == Legacy && p.RequireIPv6 {
			return Operation{}, Conflict("此账号必须使用固定 IPv6，不能切回原有出口；可暂停账号调度")
		}
		if target == Legacy && !c.AcknowledgeLegacy {
			return Operation{}, Conflict("退出后恢复原出口，必须显式确认")
		}
	case "retire":
		target = "retired"
	case "rotate", "probe", "restore-binding", "recover":
		if !p.Strict() {
			return Operation{}, Conflict("账号尚未启用固定 IPv6")
		}
		target = Strict
	default:
		return Operation{}, Conflict("未知操作")
	}
	if target == Strict || c.Kind == "runtime" && c.Enabled {
		var persistedPlugin bool
		if err := m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_plugin_bindings WHERE enabled=true AND platform='openai' AND account_type='oauth')`).Scan(&persistedPlugin); err != nil {
			return Operation{}, err
		}
		if persistedPlugin || plugin != nil && plugin() {
			return Operation{}, Error("EGRESS_PLUGIN_CONFLICT", "先停用OpenAI OAuth Transport插件")
		}
		if c.AccountID > 0 {
			if a.ProxyID != 0 {
				return Operation{}, Error("EGRESS_PROXY_CONFLICT", "请先取消账号代理")
			}
			if !runtime.Enabled && c.Kind != "restore-binding" {
				return Operation{}, Error("EGRESS_PAUSED", "请先打开全局开关")
			}
			if !a.OrdinaryOAuth() {
				return Operation{}, Conflict("该认证方式尚不支持")
			}
			for _, other := range m.Bindings() {
				if other.Account.ID != a.ID && !other.Inherited && other.Account.Identity == a.Identity && a.Identity != "" {
					return Operation{}, Conflict("存在同身份独立账号，请先合并为影子账号")
				}
			}
			if a.Identity == "" {
				return Operation{}, Conflict("缺少可信chatgpt_user_id，请通过账号关联授权验证")
			}
			// 从本次数据库快照读取 AT；必要时受控刷新，ID Token 不参与出口判断。
			if !c.recovering {
				a, err = m.prepareAccountAccessToken(ctx, a, p)
				if err != nil {
					return Operation{}, err
				}
			}
			if p.Identity != "" && a.Identity != p.Identity {
				return Operation{}, Conflict("账号身份与owner不一致")
			}
		}
	}
	if c.Kind == "probe" && !runtime.Enabled {
		return Operation{}, Error("EGRESS_PAUSED", "暂停时禁止普通探测")
	}
	if c.Kind == "probe" {
		var recent bool
		if err = m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM wayvia_egress_operations WHERE owner_id=$1 AND kind='probe' AND created_at>now()-interval '30 seconds')`, p.Owner).Scan(&recent); err != nil {
			return Operation{}, err
		}
		if recent {
			return Operation{}, Conflict("同账号探测间隔至少30秒")
		}
	}
	op := Operation{ID: uuid.NewString(), Owner: p.Owner, Kind: c.Kind, Target: target, Phase: "accepted", CreatedAt: time.Now()}
	mark := pending{Operation: op, Owners: owners}
	if c.EnableScheduling {
		mark.EnableAccountID = c.AccountID
	}
	if err = durableJSON(m.pendingPath(op.ID), mark); err != nil {
		return Operation{}, err
	}
	for _, owner := range owners {
		m.closeOwner(owner, false)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	// 验签后到提交前可能发生凭据/代理编辑；锁定账号并复核同一份资料。
	if c.AccountID > 0 {
		raw, e := json.Marshal(a.Credentials)
		if e != nil {
			return Operation{}, e
		}
		var matches bool
		e = tx.QueryRowContext(ctx, `SELECT platform=$2 AND type=$3 AND parent_account_id IS NULL AND COALESCE(proxy_id,0)=$4 AND credentials=$5::jsonb FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, a.ID, a.Platform, a.Type, a.ProxyID, string(raw)).Scan(&matches)
		if e != nil {
			return Operation{}, e
		}
		if !matches {
			return Operation{}, Conflict("账号资料在校验期间发生变化，请重试")
		}
		if target == Strict {
			rows, e := tx.QueryContext(ctx, `SELECT proxy_id IS NULL AND (credentials-'model_mapping'-'compact_model_mapping')='{}'::jsonb FROM accounts WHERE parent_account_id=$1 AND deleted_at IS NULL FOR UPDATE`, a.ID)
			if e != nil {
				return Operation{}, e
			}
			valid := true
			for rows.Next() {
				var ok bool
				if e = rows.Scan(&ok); e != nil {
					rows.Close()
					return Operation{}, e
				}
				valid = valid && ok
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return Operation{}, e
			}
			if !valid {
				return Operation{}, Conflict("影子账号存在独立凭据或代理，请先清理")
			}
		}
	}
	if c.Kind == "restore-binding" {
		if _, e := uuid.Parse(c.BindingID); e != nil {
			return Operation{}, Conflict("需要有效历史绑定ID")
		}
		if err = execOne(ctx, tx, `UPDATE wayvia_account_egress_policies SET active_binding_id=$2 WHERE owner_id=$1 AND EXISTS(SELECT 1 FROM wayvia_account_egress_bindings WHERE id=$2 AND owner_id=$1 AND state IN ('active','retained'))`, p.Owner, c.BindingID); err != nil {
			return Operation{}, err
		}
	}
	var ownerArg any
	if p.Owner != "" {
		ownerArg = p.Owner
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO wayvia_egress_operations(id,idempotency_key,request_hash,owner_id,kind,target,expected_version,actor) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, op.ID, c.IdempotencyKey, hash, ownerArg, c.Kind, target, c.ExpectedVersion, actor); err != nil {
		return Operation{}, err
	}
	if c.Kind == "runtime" {
		runtime.Enabled = c.Enabled
		runtime.Revision++
		runtime.OperationID = op.ID
		raw, _ := json.Marshal(runtime)
		_, err = tx.ExecContext(ctx, `UPDATE settings SET value=$1,updated_at=now() WHERE key=$2`, string(raw), settingKey)
		if err != nil {
			return Operation{}, err
		}
	}
	for _, owner := range owners {
		state := "preparing"
		if target == "paused" || target == Legacy || target == "retired" {
			state = "pausing"
		}
		mode := Strict
		if target == "retired" {
			mode = p.Target
		}
		if target == Legacy {
			mode = Legacy
		}
		_, err = tx.ExecContext(ctx, `UPDATE wayvia_account_egress_policies SET target_mode=$2,policy_version=policy_version+1,state=CASE WHEN state='credential_uncertain' THEN state ELSE $3 END,operation_id=$4,last_error=CASE WHEN state='credential_uncertain' THEN last_error ELSE '' END,identity_key=COALESCE(identity_key,NULLIF($5,'')),updated_at=now() WHERE owner_id=$1`, owner, mode, state, op.ID, a.Identity)
		if err != nil {
			return Operation{}, err
		}
	}
	if err != nil {
		return Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Operation{}, err
	}
	if err = m.Reload(ctx); err != nil {
		m.failOperation(mark, "EGRESS_RELOAD_FAILED")
		return Operation{}, err
	}
	m.operationWG.Add(1)
	go func() { defer m.operationWG.Done(); m.execute(mark) }()
	return op, nil
}

func (m *Manager) execute(mark pending) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var runErr error
	_, runErr = m.db.ExecContext(ctx, `UPDATE wayvia_egress_operations SET phase='draining' WHERE id=$1`, mark.Operation.ID)
	for _, owner := range mark.Owners {
		if runErr != nil {
			break
		}
		runErr = m.waitRefresh(ctx, owner)
		if runErr != nil {
			break
		}
		m.closeOwner(owner, true)
		runErr = m.waitBusiness(ctx, owner)
		if runErr != nil {
			break
		}
		m.mu.Lock()
		var p Policy
		for _, v := range m.policies {
			if v.Owner == owner {
				p = v
				break
			}
		}
		m.mu.Unlock()
		if p.State == "credential_uncertain" && mark.Operation.Target != "paused" && mark.Operation.Kind != "restore-binding" {
			runErr = Error("EGRESS_CREDENTIAL_UNCERTAIN", "刷新结果待确认")
			break
		}
		switch mark.Operation.Target {
		case "retired":
			if p.Source != "" {
				runErr = m.agent(ctx, "retire", p)
			}
			if runErr == nil {
				runErr = execOne(ctx, m.db, `UPDATE wayvia_account_egress_policies SET state='retired',last_error='' WHERE owner_id=$1 AND operation_id=$2`, owner, mark.Operation.ID)
			}
		case Legacy:
			_, runErr = m.db.ExecContext(ctx, `UPDATE wayvia_account_egress_bindings SET state='retained' WHERE owner_id=$1`, owner)
			if runErr != nil {
				break
			}
			runErr = execOne(ctx, m.db, `UPDATE wayvia_account_egress_policies SET current_mode='legacy',target_mode='legacy',state='legacy',last_error='' WHERE owner_id=$1 AND operation_id=$2`, owner, mark.Operation.ID)
		case "paused":
			runErr = execOne(ctx, m.db, `UPDATE wayvia_account_egress_policies SET state=CASE WHEN state='credential_uncertain' THEN state ELSE 'paused' END,last_error=CASE WHEN state='credential_uncertain' THEN last_error ELSE '' END WHERE owner_id=$1 AND operation_id=$2`, owner, mark.Operation.ID)
		default:
			_, runErr = m.db.ExecContext(ctx, `UPDATE wayvia_egress_operations SET phase='preparing' WHERE id=$1`, mark.Operation.ID)
			if runErr != nil {
				break
			}
			if p.Mode == Strict && p.BindingID == "" {
				runErr = Error("EGRESS_BINDING_MISSING", "固定 IPv6 账号的地址绑定丢失，必须恢复数据库")
				break
			}
			if p.BindingID == "" || mark.Operation.Kind == "rotate" {
				p, runErr = m.allocate(ctx, p)
			}
			if runErr == nil {
				runErr = m.validateBinding(ctx, p)
			}
			if runErr == nil {
				action := "prepare"
				if mark.Operation.Kind == "restore-binding" {
					action = "restore"
				}
				runErr = m.agent(ctx, action, p)
			}
			var observed string
			if runErr == nil {
				observed, runErr = m.probe(ctx, p)
			}
			if runErr == nil {
				tx, e := m.db.BeginTx(ctx, nil)
				if e != nil {
					runErr = e
					break
				}
				// 与账号编辑保持相同锁顺序，避免策略锁与账号锁互相等待。
				if mark.EnableAccountID == p.AccountID || mark.Operation.Kind == "recover" {
					_, e = tx.ExecContext(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, p.AccountID)
				}
				if e == nil && mark.Operation.Kind == "recover" {
					var allowed bool
					e = tx.QueryRowContext(ctx, `SELECT status='active' AND schedulable AND deleted_at IS NULL FROM accounts WHERE id=$1`, p.AccountID).Scan(&allowed)
					if e == nil && !allowed {
						e = Error("EGRESS_RECOVERY_DEFERRED", "账号已暂停，保留原绑定等待恢复")
					}
				}
				if e == nil {
					_, e = tx.ExecContext(ctx, `UPDATE wayvia_account_egress_bindings SET state=CASE WHEN id=$2 THEN 'active' ELSE 'retained' END WHERE owner_id=$1`, owner, p.BindingID)
				}
				if e == nil {
					e = execOne(ctx, tx, `UPDATE wayvia_account_egress_policies SET current_mode='strict_ipv6',target_mode='strict_ipv6',state=CASE WHEN state='credential_uncertain' THEN state ELSE 'ready' END,observed_ipv6=$3,observed_at=now(),last_error=CASE WHEN state='credential_uncertain' THEN last_error ELSE '' END WHERE owner_id=$1 AND operation_id=$2`, owner, mark.Operation.ID, observed)
				}
				if e == nil {
					e = enableSchedulingAfterReady(ctx, tx, mark, p)
				}
				if e == nil {
					e = tx.Commit()
				} else {
					tx.Rollback()
				}
				runErr = e
			}
		}
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	// 终态提交和内存放行必须与下一个控制操作原子串行，旧任务不能打开新任务的门控。
	Control.Lock()
	defer Control.Unlock()
	if runErr != nil {
		message := safeError(runErr)
		for _, owner := range mark.Owners {
			_, _ = m.db.ExecContext(finishCtx, `UPDATE wayvia_account_egress_policies SET state=CASE WHEN state='credential_uncertain' THEN state ELSE 'blocked' END,last_error=$2 WHERE owner_id=$1 AND operation_id=$3`, owner, message, mark.Operation.ID)
		}
		_, _ = m.db.ExecContext(finishCtx, `UPDATE wayvia_egress_operations SET phase='failed',error=$2,finished_at=now() WHERE id=$1`, mark.Operation.ID, message)
		_ = m.Reload(finishCtx)
		return
	}
	if err := m.Reload(finishCtx); err != nil {
		m.failOperation(mark, "EGRESS_RELOAD_FAILED")
		return
	}
	if err := m.clearOwnerMarkers(mark); err != nil {
		m.failOperation(mark, "EGRESS_MARKER_PERSISTENCE_FAILED")
		return
	}
	if _, err := m.db.ExecContext(finishCtx, `UPDATE wayvia_egress_operations SET phase='completed',finished_at=now() WHERE id=$1`, mark.Operation.ID); err != nil {
		m.failOperation(mark, "EGRESS_OPERATION_COMMIT_FAILED")
		return
	}
	m.mu.Lock()
	for _, owner := range mark.Owners {
		for key, e := range m.epochs {
			if strings.HasPrefix(key, owner+"/") && e.closed && len(e.leases) == 0 && len(e.connections) == 0 {
				delete(m.epochs, key)
			}
		}
		delete(m.blocked, owner)
		for _, p := range m.policies {
			if p.Owner == owner && p.State == "ready" && !m.closing {
				m.ready[owner] = true
			}
		}
	}
	m.mu.Unlock()
}

// 与核验结果同事务开放母账号，禁用账号和影子账号的手动暂停状态保持不变。
func enableSchedulingAfterReady(ctx context.Context, tx *sql.Tx, mark pending, p Policy) error {
	if mark.EnableAccountID == 0 || mark.EnableAccountID != p.AccountID {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET schedulable=true,updated_at=now() WHERE id=$1 AND status='active' AND deleted_at IS NULL AND schedulable=false AND EXISTS(SELECT 1 FROM wayvia_account_egress_policies WHERE root_account_id=$1 AND owner_id=$2 AND operation_id=$3 AND state='ready' AND current_mode='strict_ipv6' AND target_mode='strict_ipv6')`, p.AccountID, p.Owner, mark.Operation.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	return publishCredentialChange(ctx, tx, p.AccountID)
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	if reason := apperrors.Reason(err); strings.HasPrefix(reason, "EGRESS_") {
		return reason + ": " + apperrors.Message(err)
	}
	s := err.Error()
	if strings.HasPrefix(s, "EGRESS_") && len(s) < 256 {
		return s
	}
	return "EGRESS_OPERATION_FAILED: 操作失败；请检查服务端脱敏日志与部署配置"
}

func (m *Manager) allocate(ctx context.Context, p Policy) (Policy, error) {
	key, err := os.ReadFile(m.config.KeyFile)
	if err != nil || len(key) < 32 {
		return p, Error("EGRESS_KEY_UNAVAILABLE", "分配密钥不可用，已有绑定不受影响")
	}
	prefix, _ := netip.ParsePrefix(m.config.Prefix)
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO wayvia_ipv6_pools(id,cidr,node_id,interface) VALUES('default',$1,$2,$3) ON CONFLICT(id) DO NOTHING`, m.config.Prefix, m.config.NodeID, m.config.Interface)
	if err != nil {
		return p, err
	}
	var cidr, node, iface string
	var allocation int64
	err = tx.QueryRowContext(ctx, `SELECT cidr::text,node_id,interface,allocation_version FROM wayvia_ipv6_pools WHERE id='default' FOR UPDATE`).Scan(&cidr, &node, &iface, &allocation)
	if err != nil {
		return p, err
	}
	if cidr != m.config.Prefix || node != m.config.NodeID || iface != m.config.Interface {
		return p, Conflict("数据库池与批准配置不一致，禁止原地修改")
	}
	var version int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(binding_version),0)+1 FROM wayvia_account_egress_bindings WHERE owner_id=$1`, p.Owner).Scan(&version)
	if err != nil {
		return p, err
	}
	addrs, _ := net.InterfaceAddrs()
	existing := map[netip.Addr]bool{}
	for _, v := range addrs {
		if pre, e := netip.ParsePrefix(v.String()); e == nil {
			existing[pre.Addr()] = true
		}
	}
	for attempt := 0; attempt < 64; attempt++ {
		a, e := Candidate(key, prefix, "default", p.Owner, allocation, version, attempt)
		if e != nil {
			return p, e
		}
		if m.config.excluded(a) || existing[a] {
			continue
		}
		var id string
		e = tx.QueryRowContext(ctx, `INSERT INTO wayvia_account_egress_bindings(owner_id,pool_id,ipv6,binding_version,allocation_key_id) VALUES($1,'default',$2,$3,$4) ON CONFLICT(ipv6) DO NOTHING RETURNING id::text`, p.Owner, a.String(), version, m.config.KeyID).Scan(&id)
		if e == sql.ErrNoRows {
			continue
		}
		if e != nil {
			return p, e
		}
		_, e = tx.ExecContext(ctx, `UPDATE wayvia_account_egress_policies SET active_binding_id=$2 WHERE owner_id=$1`, p.Owner, id)
		if e != nil {
			return p, e
		}
		if e = tx.Commit(); e != nil {
			return p, e
		}
		p.BindingID = id
		p.Source = a.String()
		p.BindingVersion = version
		return p, nil
	}
	return p, Error("EGRESS_POOL_EXHAUSTED", "64次候选分配均不可用")
}
func (m *Manager) recoverPending(ctx context.Context) error {
	// 标记清理与终态提交之间也可能崩溃；未结束的数据库操作一律恢复为阻断。
	if _, err := m.db.ExecContext(ctx, `UPDATE wayvia_account_egress_policies SET state=CASE WHEN state IN ('credential_uncertain','retired') THEN state ELSE 'blocked' END,last_error='EGRESS_RESTART_RECONCILE_REQUIRED' WHERE operation_id IN (SELECT id FROM wayvia_egress_operations WHERE phase IN ('accepted','draining','preparing'))`); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE wayvia_egress_operations SET phase='failed',error='EGRESS_RESTART_RECONCILE_REQUIRED',finished_at=now() WHERE phase IN ('accepted','draining','preparing')`); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(m.config.StateDir, "pending-*.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		b, e := os.ReadFile(f)
		if e != nil {
			return e
		}
		var mark pending
		if e = json.Unmarshal(b, &mark); e != nil {
			return fmt.Errorf("pending标记损坏，禁止开放OAuth")
		}
		for _, owner := range mark.Owners {
			m.closeOwner(owner, true)
		}
		// 响应可能在崩溃前已接收。保留阻断，由管理员重试恢复，绝不重放Token。
		_, e = m.db.ExecContext(ctx, `UPDATE wayvia_egress_operations SET phase='failed',error='EGRESS_RESTART_RECONCILE_REQUIRED',finished_at=now() WHERE id=$1 AND phase<>'completed'`, mark.Operation.ID)
		if e != nil {
			return e
		}
		for _, owner := range mark.Owners {
			// 恢复原账号的事务可能尚未提交；保留墓碑状态，才能继续从原owner入口恢复。
			_, e = m.db.ExecContext(ctx, `UPDATE wayvia_account_egress_policies SET state=CASE WHEN state IN ('credential_uncertain','retired') THEN state ELSE 'blocked' END,last_error='EGRESS_RESTART_RECONCILE_REQUIRED' WHERE owner_id=$1`, owner)
			if e != nil {
				return e
			}
		}
	}
	refreshFiles, err := filepath.Glob(filepath.Join(m.config.StateDir, "refresh-*.json"))
	if err != nil {
		return err
	}
	for _, f := range refreshFiles {
		b, e := os.ReadFile(f)
		if e != nil {
			return e
		}
		var mark struct {
			Owner string `json:"owner_id"`
		}
		if e = json.Unmarshal(b, &mark); e != nil {
			return e
		}
		m.closeOwner(mark.Owner, true)
		if _, e = m.db.ExecContext(ctx, `UPDATE wayvia_account_egress_policies SET state=CASE WHEN state='retired' THEN state ELSE 'credential_uncertain' END,last_error='EGRESS_CREDENTIAL_UNCERTAIN' WHERE owner_id=$1`, mark.Owner); e != nil {
			return e
		}
	}
	return m.Reload(ctx)
}
func (m *Manager) recoverReady(parent context.Context) {
	m.mu.Lock()
	var ps []Policy
	if m.runtime.Enabled && m.initError == "" && !m.closing {
		for _, p := range m.policies {
			if p.Strict() && p.State == "ready" && !m.blocked[p.Owner] {
				ps = append(ps, p)
			}
		}
	}
	m.mu.Unlock()
	for _, p := range ps {
		if parent.Err() != nil {
			return
		}
		Control.Lock()
		if !m.isCurrent(p) {
			Control.Unlock()
			continue
		}
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		err := m.validateBinding(ctx, p)
		if err == nil {
			err = m.agent(ctx, "check", p)
		}
		var observed string
		if err == nil {
			observed, err = m.probe(ctx, p)
		}
		if err == nil {
			_, err = m.db.ExecContext(ctx, `UPDATE wayvia_account_egress_policies SET observed_ipv6=$2,observed_at=now() WHERE owner_id=$1`, p.Owner, observed)
		}
		if err == nil {
			err = m.Reload(ctx)
		}
		cancel()
		if err == nil {
			m.mu.Lock()
			if now, ok := m.policies[p.AccountID]; ok && now.Version == p.Version && !m.blocked[p.Owner] && m.runtime.Enabled && !m.closing {
				m.ready[p.Owner] = true
			}
			m.mu.Unlock()
		} else if parent.Err() == nil {
			if e := m.blockForRecovery(parent, p, safeError(err)); e != nil {
				m.rememberFault(p, safeError(err))
			}
		}
		Control.Unlock()
	}
}

func (m *Manager) clearOwnerMarkers(mark pending) error {
	files, err := filepath.Glob(filepath.Join(m.config.StateDir, "pending-*.json"))
	if err != nil {
		return err
	}
	owners := map[string]bool{}
	for _, o := range mark.Owners {
		owners[o] = true
	}
	for _, f := range files {
		b, e := os.ReadFile(f)
		if e != nil {
			return e
		}
		var old pending
		if e = json.Unmarshal(b, &old); e != nil {
			return e
		}
		remaining := make([]string, 0, len(old.Owners))
		for _, o := range old.Owners {
			if !owners[o] {
				remaining = append(remaining, o)
			}
		}
		if old.Operation.ID == mark.Operation.ID || len(old.Owners) > 0 && len(remaining) == 0 {
			if e = removeDurable(f); e != nil {
				return e
			}
		} else if len(remaining) < len(old.Owners) {
			old.Owners = remaining
			if e = durableJSON(f, old); e != nil {
				return e
			}
		}
	}
	return nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func execOne(ctx context.Context, db execer, q string, args ...any) error {
	r, e := db.ExecContext(ctx, q, args...)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return Conflict("操作状态或版本已变化")
	}
	return nil
}
func (m *Manager) failOperation(mark pending, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, o := range mark.Owners {
		m.closeOwner(o, true)
		_, _ = m.db.ExecContext(ctx, `UPDATE wayvia_account_egress_policies SET state=CASE WHEN state='credential_uncertain' THEN state ELSE 'blocked' END,last_error=$2 WHERE owner_id=$1`, o, reason)
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE wayvia_egress_operations SET phase='failed',error=$2,finished_at=now() WHERE id=$1`, mark.Operation.ID, reason)
	_ = m.Reload(ctx)
}
func (m *Manager) validateBinding(ctx context.Context, p Policy) error {
	if err := m.config.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	a, ok := m.accounts[p.AccountID]
	m.mu.Unlock()
	if !ok || !a.OrdinaryOAuth() || a.ProxyID != 0 || a.Identity != p.Identity || p.Identity == "" {
		return Conflict("账号类型、身份或代理与绑定不一致")
	}
	var plugin bool
	if err := m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_plugin_bindings WHERE enabled AND platform='openai' AND account_type='oauth')`).Scan(&plugin); err != nil {
		return err
	}
	if plugin {
		return Error("EGRESS_PLUGIN_CONFLICT", "OAuth Transport插件冲突")
	}
	var prefix, node, iface string
	if err := m.db.QueryRowContext(ctx, `SELECT cidr::text,node_id,interface FROM wayvia_ipv6_pools WHERE id='default'`).Scan(&prefix, &node, &iface); err != nil {
		return err
	}
	if prefix != m.config.Prefix || node != m.config.NodeID || iface != m.config.Interface {
		return Conflict("批准池配置发生变化")
	}
	return nil
}

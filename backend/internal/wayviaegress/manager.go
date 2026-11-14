package wayviaegress

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Control 串行化固定出口和插件启用。网络准备不占用此锁。
var Control sync.Mutex

type Manager struct {
	probeTransport      http.RoundTripper // 默认使用固定源地址 TCP；隔离测试可注入回显响应。
	db                  *sql.DB
	config              Config
	mu                  sync.Mutex
	reloadMu            sync.Mutex
	stop                context.CancelFunc
	done                chan struct{}
	autoDone            chan struct{}
	faults              map[string]runtimeFault
	evidenceQueue       chan Evidence
	evidenceStop        context.CancelFunc
	evidenceDone        chan struct{}
	evidenceDropped     atomic.Uint64
	evidenceUnavailable atomic.Bool
	reserved            map[netip.Addr]bool
	runtime             Runtime
	accounts            map[int64]Account
	policies            map[int64]Policy
	epochs              map[string]*epoch
	blocked             map[string]bool
	ready               map[string]bool
	refreshLocks        sync.Map
	initError           string
	lock                *os.File
	pluginEnabled       func() bool
	clearAccounts       func([]int64)
	refreshExchange     func(context.Context, Account) (map[string]any, error)
	refreshWindow       time.Duration
	closing             bool
	operationWG         sync.WaitGroup
}

// Initialize 在官方迁移后、后台任务启动前安装门控。失败保持可诊断阻断。
func Initialize(ctx context.Context, db *sql.DB) *Manager {
	m := &Manager{db: db, accounts: map[int64]Account{}, policies: map[int64]Policy{}, epochs: map[string]*epoch{}, blocked: map[string]bool{}, ready: map[string]bool{}}
	current.Store(m)
	if err := m.loadAccounts(ctx); err != nil {
		m.initError = "账号快照不可用"
	}
	if path := os.Getenv("WAYVIA_EGRESS_CONFIG"); path != "" {
		c, err := ReadConfig(path)
		m.config = c // 即使校验失败也保留可解析前缀，普通源保护不得丢失批准范围。
		if err != nil {
			m.initError = err.Error()
		} else {
			m.config = c
			if err = m.acquireLock(); err != nil {
				m.initError = err.Error()
			}
		}
	}
	// 配置或数据库损坏时仍尽力保留专用地址拓扑，普通请求不能误选历史地址。
	if rows, err := db.QueryContext(ctx, `SELECT host(ipv6) FROM wayvia_account_egress_bindings`); err == nil {
		m.reserved = map[netip.Addr]bool{}
		for rows.Next() {
			var raw string
			if rows.Scan(&raw) == nil {
				if a, e := netip.ParseAddr(raw); e == nil {
					m.reserved[a] = true
				}
			}
		}
		rows.Close()
	}
	if err := Migrate(ctx, db); err != nil {
		m.initError = "扩展数据库迁移失败"
		slog.Error("wayvia migration failed", "error", err)
		return m
	}
	if err := m.Reload(ctx); err != nil {
		m.initError = "扩展快照加载失败"
		slog.Error("wayvia snapshot failed", "error", err)
		return m
	}
	if m.lock != nil {
		if err := m.recoverPending(ctx); err != nil {
			m.initError = err.Error()
		}
	}
	monitorCtx, cancel := context.WithCancel(context.Background())
	m.stop = cancel
	m.done = make(chan struct{})
	m.autoDone = make(chan struct{})
	evidenceCtx, evidenceCancel := context.WithCancel(context.Background())
	m.evidenceQueue = make(chan Evidence, 512)
	m.evidenceDone = make(chan struct{})
	m.evidenceStop = evidenceCancel
	go func() { defer close(m.evidenceDone); m.evidenceLoop(evidenceCtx) }()
	go func() { defer close(m.done); m.monitor(monitorCtx) }()
	go func() { defer close(m.autoDone); m.autoEnrollLoop(monitorCtx) }()
	return m
}

type accountQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readAccounts(ctx context.Context, db accountQuery) (map[int64]Account, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,name,platform,type,COALESCE(parent_account_id,0),COALESCE(proxy_id,0),credentials,status,schedulable FROM accounts WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	a := map[int64]Account{}
	for rows.Next() {
		var v Account
		var b []byte
		if err = rows.Scan(&v.ID, &v.Name, &v.Platform, &v.Type, &v.ParentID, &v.ProxyID, &b, &v.Status, &v.Schedulable); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &v.Credentials); err != nil {
			return nil, err
		}
		v.Identity, _ = v.Credentials["chatgpt_user_id"].(string)
		a[v.ID] = v
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return a, nil
}
func (m *Manager) loadAccounts(ctx context.Context) error {
	a, err := readAccounts(ctx, m.db)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.accounts = a
	m.mu.Unlock()
	return nil
}
func (m *Manager) Reload(ctx context.Context) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	accounts, err := readAccounts(ctx, tx)
	if err != nil {
		return err
	}
	var raw string
	var runtime Runtime
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, settingKey).Scan(&raw); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(raw), &runtime); err != nil {
		return err
	}
	if !runtime.Initialized {
		return Error("EGRESS_POLICY_UNKNOWN", "初始化标记缺失")
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.owner_id::text,COALESCE(p.root_account_id,0),COALESCE(p.identity_key,''),p.current_mode,p.target_mode,p.policy_version,p.state,COALESCE(p.active_binding_id::text,''),COALESCE(host(b.ipv6),''),COALESCE(b.binding_version,0),COALESCE(b.state,''),COALESCE(p.operation_id::text,''),COALESCE(NULLIF(j.last_error,''),p.last_error),COALESCE(host(p.observed_ipv6),''),p.observed_at,p.require_ipv6,COALESCE(j.state,''),CASE WHEN j.state='retry' THEN j.next_attempt_at END,COALESCE(r.state,''),COALESCE(r.attempts,0),r.next_attempt_at,r.recovered_at,COALESCE(r.last_error,'') FROM wayvia_account_egress_policies p LEFT JOIN wayvia_account_egress_bindings b ON b.id=p.active_binding_id LEFT JOIN wayvia_egress_enrollments j ON j.owner_id=p.owner_id LEFT JOIN wayvia_egress_recoveries r ON r.owner_id=p.owner_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	policies := map[int64]Policy{}
	for rows.Next() {
		var p Policy
		if err = rows.Scan(&p.Owner, &p.AccountID, &p.Identity, &p.Mode, &p.Target, &p.Version, &p.State, &p.BindingID, &p.Source, &p.BindingVersion, &p.BindingState, &p.OperationID, &p.LastError, &p.ObservedIP, &p.ObservedAt, &p.RequireIPv6, &p.AutoEnrollment, &p.AutoRetryAt, &p.RecoveryState, &p.RecoveryAttempts, &p.RecoveryRetryAt, &p.RecoveredAt, &p.RecoveryError); err != nil {
			return err
		}
		if p.AccountID > 0 {
			policies[p.AccountID] = p
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	sources, err := tx.QueryContext(ctx, `SELECT host(ipv6) FROM wayvia_account_egress_bindings`)
	if err != nil {
		return err
	}
	reserved := map[netip.Addr]bool{}
	for sources.Next() {
		var raw string
		if err = sources.Scan(&raw); err != nil {
			sources.Close()
			return err
		}
		if a, e := netip.ParseAddr(raw); e == nil {
			reserved[a] = true
		}
	}
	err = sources.Err()
	sources.Close()
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	m.mu.Lock()
	m.accounts = accounts
	if m.reserved == nil {
		m.reserved = map[netip.Addr]bool{}
	}
	for a := range reserved {
		m.reserved[a] = true
	}
	m.runtime = runtime
	m.policies = policies
	m.mu.Unlock()
	return nil
}
func (m *Manager) rootLocked(id int64) (Account, Policy, error) {
	seen := map[int64]bool{}
	for id > 0 {
		a, ok := m.accounts[id]
		if !ok {
			return a, Policy{}, Error("EGRESS_POLICY_UNKNOWN", "账号快照缺失")
		}
		if seen[id] {
			return a, Policy{}, Error("EGRESS_POLICY_UNKNOWN", "影子账号循环")
		}
		seen[id] = true
		if !a.Eligible() {
			if len(seen) > 1 {
				return a, Policy{}, Error("EGRESS_POLICY_UNKNOWN", "母账号类型不匹配")
			}
			return a, Policy{}, nil
		}
		if a.ParentID == 0 {
			p, ok := m.policies[id]
			if !ok {
				return a, p, Error("EGRESS_POLICY_UNKNOWN", "OAuth出口策略缺失")
			}
			return a, p, nil
		}
		id = a.ParentID
	}
	return Account{}, Policy{}, nil
}
func (m *Manager) routeLocked(id int64, purpose string) (Route, error) {
	if id < 0 {
		return Route{}, Error("EGRESS_ACCOUNT_CONTEXT_REQUIRED", "无法确定凭据所属账号")
	}
	a, p, err := m.rootLocked(id)
	r := Route{AccountID: id, Mode: Legacy, Purpose: purpose}
	if err != nil {
		return r, err
	}
	if !a.Eligible() {
		return r, nil
	}
	if m.initError != "" {
		return r, Error("EGRESS_POLICY_UNKNOWN", m.initError)
	}
	if m.closing {
		return r, Error("EGRESS_PAUSED", "服务正在停止")
	}
	if p.State == "retired" {
		return r, Error("EGRESS_RETIRED", "账号已退役")
	}
	if p.RequireIPv6 && p.Mode != Strict || p.State == "pending_enrollment" {
		return r, Error("EGRESS_ENROLLMENT_REQUIRED", "账号尚未完成固定 IPv6 自动配置与核验，调度保持暂停")
	}
	if m.config.StateDir != "" && a.OrdinaryOAuth() && a.Identity == "" {
		return r, Error("EGRESS_IDENTITY_UNVERIFIED", "身份未知账号需先完成关联授权")
	}
	if p.State == "credential_uncertain" {
		return r, Error("EGRESS_CREDENTIAL_UNCERTAIN", "凭据状态待确认，请重新授权")
	}
	if !p.Strict() && p.State != "legacy" {
		return r, Error("EGRESS_POLICY_UNKNOWN", "原出口尚未完成受控恢复")
	}
	if m.blocked[p.Owner] {
		return r, Error("EGRESS_PREPARING", "出口转换尚未完成")
	}
	r.Owner = p.Owner
	r.PolicyVersion = p.Version
	if p.Strict() {
		r.Revision = m.runtime.Revision
	}
	r.BindingVersion = p.BindingVersion
	if p.Strict() {
		if !a.OrdinaryOAuth() {
			return r, Error("EGRESS_PATH_UNSUPPORTED", "该认证类型暂不支持固定 IPv6")
		}
		r.Mode = Strict
		r.Source, _ = netip.ParseAddr(p.Source)
		if p.State == "credential_uncertain" {
			return r, Error("EGRESS_CREDENTIAL_UNCERTAIN", "刷新凭据状态待确认，请重新授权")
		}
		if !m.runtime.Enabled {
			return r, Error("EGRESS_PAUSED", "固定 IPv6 账号已暂停")
		}
		if p.State != "ready" || !m.ready[p.Owner] {
			return r, Error("EGRESS_SOURCE_UNREADY", "绑定尚未完成本次就绪核验")
		}
		if a.Identity != p.Identity || p.Identity == "" {
			return r, Error("EGRESS_IDENTITY_UNVERIFIED", "身份与owner不一致")
		}
		if a.ProxyID != 0 {
			return r, Error("EGRESS_PROXY_CONFLICT", "固定IPv6与外部代理冲突")
		}
		if !r.Source.Is6() || r.Source.Is4In6() {
			return r, Error("EGRESS_BINDING_MISSING", "IPv6绑定缺失")
		}
		if purpose != "business" && purpose != "query" && purpose != "refresh" && purpose != "prewarm" && purpose != "reauth" {
			return r, Error("EGRESS_PATH_UNSUPPORTED", "该出站能力尚未接入")
		}
	}
	key := r.Key()
	e := m.epochs[key]
	if e == nil {
		ctx, cancel := context.WithCancel(context.Background())
		e = &epoch{ctx: ctx, cancel: cancel, connections: map[*trackedConn]struct{}{}, leases: map[*Lease]struct{}{}}
		m.epochs[key] = e
	}
	r.epoch = e
	return r, r.Check()
}
func (m *Manager) Resolve(id int64, purpose string) (Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routeLocked(id, purpose)
}
func (m *Manager) Begin(ctx context.Context, r Route) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := r.Check(); err != nil {
		return nil, err
	}
	if r.Owner != "" && !r.control && !r.drainingRefresh {
		fresh, err := m.routeLocked(r.AccountID, r.Purpose)
		if err != nil {
			return nil, err
		}
		if fresh.Key() != r.Key() {
			return nil, Error("EGRESS_PAUSED", "出口版本变化")
		}
	}
	c, cancel := context.WithCancel(ctx)
	l := &Lease{Route: r, Context: c, cancel: cancel}
	if r.epoch != nil {
		l.manager = m
		l.stop = context.AfterFunc(r.epoch.ctx, cancel)
		r.epoch.leases[l] = struct{}{}
	}
	return l, nil
}
func (m *Manager) HasStrict() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.policies {
		if p.Strict() && p.State != "retired" {
			return true
		}
	}
	return false
}
func (m *Manager) IsStrict(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, p, _ := m.rootLocked(id)
	return p.Strict()
}
func (m *Manager) SetHooks(plugin func() bool, clear func([]int64)) {
	m.mu.Lock()
	m.pluginEnabled = plugin
	m.clearAccounts = clear
	m.mu.Unlock()
}

func (m *Manager) closeOwner(owner string, includeRefresh bool) {
	m.mu.Lock()
	m.blocked[owner] = true
	delete(m.ready, owner)
	var conns []*trackedConn
	for key, e := range m.epochs {
		if !strings.HasPrefix(key, owner+"/") {
			continue
		}
		if !includeRefresh && strings.HasSuffix(key, "/refresh") {
			continue
		}
		e.closed = true
		e.cancel()
		for c := range e.connections {
			conns = append(conns, c)
		}
		// 等待请求租约释放后才删除epoch，避免操作提前宣布完成。
	}

	clear := m.clearAccounts
	var ids []int64
	for id := range m.accounts {
		_, p, _ := m.rootLocked(id)
		if p.Owner == owner {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	if clear != nil {
		clear(ids)
	}
}
func (m *Manager) waitRefresh(ctx context.Context, owner string) error {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for {
		m.mu.Lock()
		n := 0
		for key, e := range m.epochs {
			if len(key) > len(owner) && key[:len(owner)+1] == owner+"/" && len(key) >= 8 && key[len(key)-8:] == "/refresh" {
				n += len(e.leases)
			}
		}
		m.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
func (m *Manager) monitor(ctx context.Context) {
	// 重启先保留阻断，再在后台核对原绑定；旧观测不作为 ready 凭据。
	m.recoverReady(ctx)
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		var ps []Policy
		for _, p := range m.policies {
			if p.Strict() && m.ready[p.Owner] {
				ps = append(ps, p)
			}
		}
		m.mu.Unlock()
		for _, p := range ps {
			if ctx.Err() != nil {
				return
			}
			Control.Lock()
			callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := m.agent(callCtx, "check", p)
			cancel()
			if err != nil && m.isCurrent(p) {
				if e := m.blockForRecovery(ctx, p, safeError(err)); e != nil {
					slog.Error("wayvia recovery enqueue failed", "reason", safeError(e))
					m.rememberFault(p, safeError(err))
				}
			}
			Control.Unlock()
		}
	}
}
func (m *Manager) setBlocked(p Policy, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(ctx, `UPDATE wayvia_account_egress_policies SET state=CASE WHEN state='credential_uncertain' THEN state ELSE 'blocked' END,last_error=$2 WHERE owner_id=$1 AND policy_version=$3`, p.Owner, message, p.Version)
	if err != nil {
		slog.Error("wayvia block persistence failed", "owner", p.Owner)
	}
	_ = m.Reload(ctx)
}
func (m *Manager) pendingPath(id string) string {
	return filepath.Join(m.config.StateDir, "pending-"+id+".json")
}
func (m *Manager) String() string { return fmt.Sprintf("%s node=%s", Capability, m.config.NodeID) }

func (m *Manager) waitBusiness(ctx context.Context, owner string) error {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		m.mu.Lock()
		n := 0
		for key, e := range m.epochs {
			if strings.HasPrefix(key, owner+"/") && !strings.HasSuffix(key, "/refresh") {
				n += len(e.leases) + len(e.connections)
				if len(e.leases) == 0 && len(e.connections) == 0 && e.closed {
					delete(m.epochs, key)
				}
			}
		}
		m.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (m *Manager) isCurrent(p Policy) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.policies[p.AccountID]
	return ok && v.Version == p.Version && v.BindingID == p.BindingID
}

// Close 先停止新请求，再等待已开始的刷新保存；未完成标记留给重启恢复。
func (m *Manager) Close() {
	if m == nil {
		return
	}
	Control.Lock()
	m.mu.Lock()
	m.closing = true
	m.mu.Unlock()
	Control.Unlock()
	if m.stop != nil {
		m.stop()
		<-m.done
		if m.autoDone != nil {
			<-m.autoDone
		}
	}
	m.mu.Lock()
	owners := []string{}
	for _, p := range m.policies {
		owners = append(owners, p.Owner)
	}
	m.mu.Unlock()
	for _, o := range owners {
		m.closeOwner(o, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 125*time.Second)
	defer cancel()
	for _, o := range owners {
		_ = m.waitRefresh(ctx, o)
		m.closeOwner(o, true)
	}
	m.operationWG.Wait()
	if m.evidenceStop != nil {
		m.evidenceStop()
		<-m.evidenceDone
	}
}

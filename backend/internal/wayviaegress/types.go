// Package wayviaegress 管理单机 OAuth 固定出口；不依赖 service 或 repository。
package wayviaegress

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const Capability = "wayvia-egress-v1"
const Legacy = "legacy"
const Strict = "strict_ipv6"
const settingKey = "wayvia_ipv6_egress"

func Error(code, message string) error {
	status := 503
	if strings.Contains(code, "CONFLICT") {
		status = 409
	}
	if code == "EGRESS_PATH_UNSUPPORTED" || code == "EGRESS_ALLOCATION_INVALID" {
		status = 400
	}
	return apperrors.New(status, code, message)
}
func Conflict(message string) error { return apperrors.New(409, "EGRESS_CONFLICT", message) }

type Account struct {
	ID          int64          `json:"-"`
	ParentID    int64          `json:"parent_account_id,omitempty"`
	Name        string         `json:"name"`
	Platform    string         `json:"platform"`
	Type        string         `json:"type"`
	ProxyID     int64          `json:"-"`
	Identity    string         `json:"-"`
	Credentials map[string]any `json:"-"`
	Status      string         `json:"account_status"`
	Schedulable bool           `json:"schedulable"`
}

func (a Account) Eligible() bool { return a.Platform == "openai" && a.Type == "oauth" }

type Runtime struct {
	Enabled     bool   `json:"enabled"`
	Revision    int64  `json:"revision"`
	Initialized bool   `json:"initialized"`
	OperationID string `json:"operation_id,omitempty"`
}
type Policy struct {
	Owner            string     `json:"owner_id"`
	AccountID        int64      `json:"account_id"`
	Identity         string     `json:"-"`
	Mode             string     `json:"current_mode"`
	Target           string     `json:"target_mode"`
	Version          int64      `json:"policy_version"`
	State            string     `json:"state"`
	RequireIPv6      bool       `json:"require_ipv6"`
	BindingID        string     `json:"binding_id,omitempty"`
	Source           string     `json:"source_ipv6,omitempty"`
	BindingVersion   int64      `json:"binding_version"`
	BindingState     string     `json:"binding_state,omitempty"`
	OperationID      string     `json:"operation_id,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	ObservedIP       string     `json:"observed_ipv6,omitempty"`
	ObservedAt       *time.Time `json:"observed_at,omitempty"`
	AutoEnrollment   string     `json:"auto_enrollment,omitempty"`
	AutoRetryAt      *time.Time `json:"auto_retry_at,omitempty"`
	RecoveryState    string     `json:"recovery_state,omitempty"`
	RecoveryAttempts int        `json:"recovery_attempts"`
	RecoveryRetryAt  *time.Time `json:"recovery_retry_at,omitempty"`
	RecoveredAt      *time.Time `json:"recovered_at,omitempty"`
	RecoveryError    string     `json:"recovery_error,omitempty"`
}

func (p Policy) Strict() bool { return p.Mode == Strict || p.Target == Strict }

type Route struct {
	AccountID       int64
	Owner           string
	Mode            string
	Source          netip.Addr
	BindingVersion  int64
	PolicyVersion   int64
	Revision        int64
	Purpose         string
	epoch           *epoch
	control         bool
	drainingRefresh bool
}

func (r Route) Key() string {
	return fmt.Sprintf("%s/%d/%s/%s/%d/%d/%d/%t/%s", r.Owner, r.AccountID, r.Mode, r.Source, r.BindingVersion, r.PolicyVersion, r.Revision, r.control, r.Purpose)
}
func (r Route) Strict() bool { return r.Mode == Strict }
func (r Route) Check() error {
	if r.epoch != nil {
		select {
		case <-r.epoch.ctx.Done():
			return Error("EGRESS_PAUSED", "出口策略已变化，请重新选择账号")
		default:
		}
	}
	return nil
}

type epoch struct {
	ctx         context.Context
	cancel      context.CancelFunc
	connections map[*trackedConn]struct{}
	leases      map[*Lease]struct{}
	closed      bool
}
type Lease struct {
	Route   Route
	Context context.Context
	cancel  context.CancelFunc
	stop    func() bool
	manager *Manager
	ended   atomic.Bool
}

func (l *Lease) End() {
	if l == nil || l.ended.Swap(true) {
		return
	}
	l.cancel()
	if l.stop != nil {
		l.stop()
	}
	if l.manager != nil {
		l.manager.mu.Lock()
		delete(l.Route.epoch.leases, l)
		l.manager.mu.Unlock()
	}
}

type trackedConn struct {
	id string
	net.Conn
	manager *Manager
	epoch   *epoch
	closed  atomic.Bool
}

func (c *trackedConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	err := c.Conn.Close()
	c.manager.mu.Lock()
	delete(c.epoch.connections, c)
	c.manager.mu.Unlock()
	return err
}

type contextKey struct{}
type contextAccount struct {
	ID      int64
	Purpose string
}

func WithAccount(ctx context.Context, id int64, purpose string) context.Context {
	return context.WithValue(ctx, contextKey{}, contextAccount{id, purpose})
}
func AccountContext(ctx context.Context) (int64, string) {
	if v, ok := ctx.Value(contextKey{}).(contextAccount); ok {
		return v.ID, v.Purpose
	}
	return 0, "query"
}

var current atomic.Pointer[Manager]

func Default() *Manager { return current.Load() }
func Resolve(id int64, purpose string) (Route, error) {
	if m := Default(); m != nil {
		return m.Resolve(id, purpose)
	}
	return Route{AccountID: id, Mode: Legacy, Purpose: purpose}, nil
}
func HasStrict() bool {
	if m := Default(); m != nil {
		return m.HasStrict()
	}
	return false
}
func IsStrict(id int64) bool {
	if m := Default(); m != nil {
		return m.IsStrict(id)
	}
	return false
}
func CheckAccount(id int64) error { _, err := Resolve(id, "business"); return err }

// ControlledRefresh 将首次启用固定 IPv6 前的刷新也纳入同一凭据提交协议。
func ControlledRefresh(id int64) bool {
	m := Default()
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, p, err := m.rootLocked(id)
	if err != nil {
		return false
	}
	token, _ := a.Credentials["refresh_token"].(string)
	return a.OrdinaryOAuth() && strings.TrimSpace(token) != "" && (p.Strict() || m.config.StateDir != "" && m.lock != nil)
}

// OnInvalidated 用于同步关闭并移除按策略版本缓存的客户端。
func (r Route) OnInvalidated(fn func()) {
	if r.epoch != nil {
		context.AfterFunc(r.epoch.ctx, fn)
	}
}

func (a Account) OrdinaryOAuth() bool {
	if !a.Eligible() {
		return false
	}
	for _, k := range []string{"auth_mode", "openai_auth_mode"} {
		v, _ := a.Credentials[k].(string)
		if v != "" && v != "oauth" {
			return false
		}
	}
	return true
}

type adminActorKey struct{}

func WithAdminActor(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, adminActorKey{}, id)
}
func AdminActor(ctx context.Context) int64 { v, _ := ctx.Value(adminActorKey{}).(int64); return v }

func Configured() bool { m := Default(); return m != nil && m.config.StateDir != "" && m.lock != nil }
func WithResolvedRoute(ctx context.Context, r Route) context.Context {
	return context.WithValue(WithAccount(ctx, r.AccountID, r.Purpose), routeContextKey{}, r)
}

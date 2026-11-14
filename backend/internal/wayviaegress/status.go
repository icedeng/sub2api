package wayviaegress

import (
	"context"
	"sort"
	"strings"
)

type BindingView struct {
	Account
	Policy
	Inherited         bool   `json:"inherited"`
	Ready             bool   `json:"ready"`
	CanEnroll         bool   `json:"can_enroll"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
}
type Status struct {
	Capability string         `json:"capability"`
	Runtime    Runtime        `json:"runtime"`
	Configured bool           `json:"configured"`
	Error      string         `json:"error,omitempty"`
	Prefix     string         `json:"prefix"`
	NodeID     string         `json:"node_id"`
	Interface  string         `json:"interface"`
	Counts     map[string]int `json:"counts"`
	Operations []Operation    `json:"operations"`
	Supported  []string       `json:"supported_purposes"`
}

func (m *Manager) Bindings() []BindingView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []BindingView{}
	for id, a := range m.accounts {
		if !a.Eligible() {
			continue
		}
		_, p, err := m.rootLocked(id)
		if err != nil {
			p = Policy{AccountID: id, State: "blocked", LastError: "EGRESS_POLICY_UNKNOWN"}
		}
		p.AccountID = id
		reason := ""
		if a.ParentID != 0 {
			reason = "影子继承母账号"
		} else if !a.OrdinaryOAuth() {
			reason = "仅支持普通 OpenAI OAuth，不支持其他认证方式"
		} else if a.ProxyID != 0 {
			reason = "请先取消外部代理"
		} else if at, _ := a.Credentials["access_token"].(string); strings.TrimSpace(at) == "" {
			if rt, _ := a.Credentials["refresh_token"].(string); strings.TrimSpace(rt) == "" {
				reason = "缺少 Access Token 和 Refresh Token，请补充账号凭据"
			}
		}
		out = append(out, BindingView{Account: a, Policy: p, Inherited: a.ParentID != 0, Ready: m.ready[p.Owner] && !m.blocked[p.Owner] && m.runtime.Enabled && p.State == "ready", CanEnroll: reason == "", UnsupportedReason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account.ID < out[j].Account.ID })
	return out
}
func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	s := Status{Capability: Capability, Runtime: m.runtime, Configured: m.lock != nil, Error: m.initError, Prefix: m.config.Prefix, NodeID: m.config.NodeID, Interface: m.config.Interface, Counts: map[string]int{}, Operations: []Operation{}, Supported: []string{"HTTP/SSE", "HTTP Bridge", "WS", "query", "refresh"}}
	m.mu.Unlock()
	for _, v := range m.Bindings() {
		s.Counts[v.State]++
	}
	rows, err := m.db.QueryContext(ctx, `SELECT id::text,COALESCE(owner_id::text,''),kind,target,phase,error,created_at,finished_at FROM wayvia_egress_operations ORDER BY created_at DESC LIMIT 20`)
	if err != nil {
		s.Error = "EGRESS_STATUS_UNAVAILABLE"
		return s
	}
	defer rows.Close()
	for rows.Next() {
		var op Operation
		if err = rows.Scan(&op.ID, &op.Owner, &op.Kind, &op.Target, &op.Phase, &op.Error, &op.CreatedAt, &op.FinishedAt); err != nil {
			s.Error = "EGRESS_STATUS_UNAVAILABLE"
			break
		}
		s.Operations = append(s.Operations, op)
	}
	if rows.Err() != nil {
		s.Error = "EGRESS_STATUS_UNAVAILABLE"
	}
	return s
}

func WithCredential(ctx context.Context, token string) context.Context {
	if id, _ := AccountContext(ctx); id > 0 {
		return ctx
	}
	m := Default()
	if m == nil || token == "" {
		return ctx
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, a := range m.accounts {
		if a.ParentID == 0 && a.Eligible() && a.Credentials["access_token"] == token {
			return WithAccount(ctx, id, "query")
		}
	}
	if m.config.StateDir != "" {
		return WithAccount(ctx, -1, "query")
	}
	return ctx
}
func RejectDuplicateIdentity(identity string) error {
	if m := Default(); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, p := range m.policies {
			if identity != "" && p.Identity == identity {
				return Conflict("该身份已绑定固定出口，请使用原账号重新授权入口")
			}
		}
	}
	return nil
}

func (m *Manager) Account(id int64) (Account, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	return a, ok
}

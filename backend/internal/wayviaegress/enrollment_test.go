package wayviaegress

import (
	"context"
	"strings"
	"testing"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func newPendingAccount(t *testing.T, m *Manager) {
	t.Helper()
	var schedulable bool
	err := m.db.QueryRow(`INSERT INTO accounts(id,name,platform,type,schedulable,credentials) VALUES(10,'new OAuth','openai','oauth',true,'{"refresh_token":"rt10","chatgpt_user_id":"user-10"}') RETURNING schedulable`).Scan(&schedulable)
	if err != nil || schedulable {
		t.Fatalf("new account must return paused: %v %v", schedulable, err)
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewOAuthRequiresIPv6BeforeAnyRequest(t *testing.T) {
	m := fixtureDB(t)
	newPendingAccount(t, m)
	if p := m.policies[10]; !p.RequireIPv6 || p.State != "pending_enrollment" || m.accounts[10].Schedulable {
		t.Fatal(p, m.accounts[10].Schedulable)
	}
	stateDir := m.config.StateDir
	for _, configured := range []bool{false, true} {
		m.config.StateDir = stateDir
		for _, enabled := range []bool{false, true} {
			m.runtime.Enabled = enabled
			if !configured {
				m.config.StateDir = ""
			}
			for _, purpose := range []string{"business", "query", "prewarm", "refresh", "images", "search", "live"} {
				if _, err := m.Resolve(10, purpose); apperrors.Reason(err) != "EGRESS_ENROLLMENT_REQUIRED" {
					t.Fatalf("pending bypass: configured=%v enabled=%v purpose=%s err=%v", configured, enabled, purpose, err)
				}
			}
		}
	}
	for _, id := range []int64{1, 2} {
		if r, err := m.Resolve(id, "business"); err != nil || r.Strict() {
			t.Fatalf("existing legacy/API Key changed: id=%d %v %v", id, r, err)
		}
	}
	// 即便状态被错误地恢复为 legacy，持久要求仍然拒绝普通出口。
	p := m.policies[10]
	p.State = "legacy"
	m.policies[10] = p
	if _, err := m.Resolve(10, "business"); err == nil {
		t.Fatal("require_ipv6 ignored")
	}
}

func TestPendingOAuthDatabaseBypassesRejected(t *testing.T) {
	m := fixtureDB(t)
	newPendingAccount(t, m)
	for _, q := range []string{
		`UPDATE accounts SET schedulable=true WHERE id=10`,
		`UPDATE accounts SET schedulable=true WHERE id IN (1,10)`,
		`UPDATE accounts SET type='apikey' WHERE id=10`,
		`UPDATE accounts SET parent_account_id=1 WHERE id=10`,
		`UPDATE wayvia_account_egress_policies SET state='legacy' WHERE root_account_id=10`,
		`UPDATE wayvia_account_egress_policies SET require_ipv6=false WHERE root_account_id=10`,
		`INSERT INTO accounts(id,platform,type,parent_account_id) VALUES(11,'openai','oauth',10)`,
		`INSERT INTO accounts(id,platform,type,parent_account_id) VALUES(12,'openai','oauth',1)`,
	} {
		if _, err := m.db.Exec(q); err == nil || !strings.Contains(err.Error(), "EGRESS_") {
			t.Fatalf("guard failed: %s %v", q, err)
		}
	}
	// 批量导入同样暂停；从 API Key 转为 OAuth 也不能先开放。
	if _, err := m.db.Exec(`INSERT INTO accounts(id,platform,type) VALUES(20,'openai','oauth'),(21,'openai','oauth');UPDATE accounts SET type='oauth' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := m.db.QueryRow(`SELECT count(*) FROM accounts WHERE id IN (2,20,21) AND schedulable=false`).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	if _, err := m.db.Exec(`UPDATE accounts SET name='renamed' WHERE id=10; UPDATE accounts SET deleted_at=now() WHERE id=20`); err != nil {
		t.Fatal("pending edit/delete blocked", err)
	}
}

func TestPendingReauthorizationDoesNotOpenBusiness(t *testing.T) {
	m := fixtureDB(t)
	newPendingAccount(t, m)
	ctx := context.Background()
	_, err := m.Reauthorize(ctx, 10, m.policies[10].Version, func(callCtx context.Context) (map[string]any, error) {
		r, err := RouteForContext(callCtx)
		if err != nil || !r.control {
			t.Fatal("authorization control lease missing", err)
		}
		return map[string]any{"access_token": "at10", "refresh_token": "new-rt10", "chatgpt_user_id": "user-10"}, nil
	})
	if err != nil || m.policies[10].State != "pending_enrollment" || m.accounts[10].Schedulable {
		t.Fatal("reauth released account", err, m.policies[10])
	}
	if _, err := m.Resolve(10, "business"); err == nil {
		t.Fatal("reauth bypassed IPv6")
	}
	if _, err := m.Change(ctx, Change{Kind: "mode", AccountID: 10, Mode: Legacy, ExpectedVersion: m.policies[10].Version, IdempotencyKey: "pending-must-not-exit", Acknowledge: true, AcknowledgeLegacy: true}, "test"); err == nil {
		t.Fatal("explicit legacy escape allowed")
	}
	// 首次准备失败且未生成绑定时，过期凭据仍能重新授权，但不能开放业务。
	if _, err := m.db.Exec(`UPDATE wayvia_account_egress_policies SET target_mode='strict_ipv6',state='blocked' WHERE root_account_id=10`); err != nil {
		t.Fatal(err)
	}
	_, err = m.Reauthorize(ctx, 10, m.policies[10].Version, func(callCtx context.Context) (map[string]any, error) {
		r, err := RouteForContext(callCtx)
		if err != nil || !r.control || r.Strict() {
			t.Fatal("first enrollment authorization must remain possible", r, err)
		}
		return map[string]any{"access_token": "at11", "refresh_token": "new-rt11", "chatgpt_user_id": "user-10"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(10, "business"); err == nil || m.accounts[10].Schedulable {
		t.Fatal("failed enrollment reauth reopened business", err)
	}
}

func TestSchedulingOnlyOpensAfterReadyCommit(t *testing.T) {
	for _, scenario := range []string{"ready", "blocked", "disabled", "no-consent", "outbox-failure"} {
		t.Run(scenario, func(t *testing.T) {
			m := fixtureDB(t)
			newPendingAccount(t, m)
			ctx := context.Background()
			const opID = "77777777-7777-4777-8777-777777777777"
			if _, err := m.db.Exec(`INSERT INTO wayvia_ipv6_pools(id,cidr,node_id,interface) VALUES('default','2606:4700:abcd:1234::/64','n','eth0');INSERT INTO wayvia_account_egress_bindings(owner_id,pool_id,ipv6,binding_version,allocation_key_id,state) SELECT owner_id,'default','2606:4700:abcd:1234::10',1,'k','active' FROM wayvia_account_egress_policies WHERE root_account_id=10;UPDATE settings SET value='{"enabled":true,"initialized":true,"revision":1}' WHERE key='wayvia_ipv6_egress'`); err != nil {
				t.Fatal(err)
			}
			if scenario == "disabled" {
				if _, err := m.db.Exec(`UPDATE accounts SET status='disabled' WHERE id=10`); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "outbox-failure" {
				if _, err := m.db.Exec(`DROP TABLE scheduler_outbox`); err != nil {
					t.Fatal(err)
				}
			}
			state := "ready"
			if scenario == "blocked" {
				state = "blocked"
			}
			p := m.policies[10]
			mark := pending{Operation: Operation{ID: opID}, EnableAccountID: 10}
			if scenario == "no-consent" {
				mark.EnableAccountID = 0
			}
			tx, err := m.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			_, err = tx.Exec(`UPDATE wayvia_account_egress_policies SET current_mode='strict_ipv6',target_mode='strict_ipv6',state=$1,operation_id=$2,identity_key='user-10',active_binding_id=(SELECT id FROM wayvia_account_egress_bindings LIMIT 1) WHERE root_account_id=10`, state, opID)
			if err != nil {
				t.Fatal(err)
			}
			err = enableSchedulingAfterReady(ctx, tx, mark, p)
			if scenario == "outbox-failure" {
				if err == nil {
					t.Fatal("outbox failure ignored")
				}
				tx.Rollback()
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if err = m.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			if m.accounts[10].Schedulable != (scenario == "ready") {
				t.Fatal("unexpected scheduling state", scenario)
			}
			if scenario == "ready" {
				if _, err := m.Resolve(10, "business"); err == nil {
					t.Fatal("database ready bypassed process verification")
				}
				m.ready[p.Owner] = true
				r, err := m.Resolve(10, "business")
				if err != nil || !r.Strict() || r.Source.String() != "2606:4700:abcd:1234::10" {
					t.Fatal(r, err)
				}
				if _, err := m.db.Exec(`INSERT INTO accounts(id,platform,type,parent_account_id) VALUES(11,'openai','oauth',10)`); err != nil {
					t.Fatal("ready shadow creation", err)
				}
				if _, err := m.db.Exec(`UPDATE wayvia_account_egress_policies SET target_mode='legacy' WHERE root_account_id=10`); err == nil {
					t.Fatal("required owner downgraded")
				}
			}
		})
	}
}

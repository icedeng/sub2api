package wayviaegress

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func recoveryFixture(t *testing.T) *Manager {
	t.Helper()
	m := fixtureDB(t)
	m.config.Prefix, m.config.Primary = "2606:4700:abcd:1234::/64", "2606:4700:abcd:1234::1"
	m.config.NodeID, m.config.Interface, m.config.KeyID = "test-node", "eth0", "test-key"
	m.config.AppUID, m.config.AppGID, m.config.MaxAddresses = 1000, 1000, 100
	m.config.KeyFile = filepath.Join(t.TempDir(), "key")
	m.config.AgentStateDir = t.TempDir()
	m.config.ProbeURL = "https://example.invalid/ip"
	if err := os.WriteFile(m.config.KeyFile, []byte(strings.Repeat("x", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	// macOS 的 Unix socket 路径长度受限，使用单独的短目录。
	dir, err := os.MkdirTemp("", "wvr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	m.config.Socket = filepath.Join(dir, "a.sock")
	l, err := net.Listen("unix", m.config.Socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q AgentRequest
		if json.NewDecoder(r.Body).Decode(&q) != nil || q.Address != "2606:4700:abcd:1234::42" || q.Version != 7 {
			http.Error(w, "ownership conflict", 409)
			return
		}
		w.WriteHeader(200)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	m.probeTransport = testEvidenceTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("2606:4700:abcd:1234::42")), Header: http.Header{}}, nil
	})
	for _, q := range []string{
		`INSERT INTO wayvia_ipv6_pools(id,cidr,node_id,interface) VALUES('default','2606:4700:abcd:1234::/64','test-node','eth0')`,
		`UPDATE wayvia_account_egress_policies SET identity_key='user-1',current_mode='strict_ipv6',target_mode='strict_ipv6',state='ready',operation_id='11111111-1111-4111-8111-111111111111' WHERE root_account_id=1`,
		`INSERT INTO wayvia_account_egress_bindings(owner_id,pool_id,ipv6,binding_version,allocation_key_id,state) SELECT owner_id,'default','2606:4700:abcd:1234::42',7,'test-key','active' FROM wayvia_account_egress_policies WHERE root_account_id=1`,
		`UPDATE wayvia_account_egress_policies SET active_binding_id=(SELECT id FROM wayvia_account_egress_bindings LIMIT 1) WHERE root_account_id=1`,
		`UPDATE settings SET value='{"enabled":true,"initialized":true,"revision":1}' WHERE key='wayvia_ipv6_egress'`,
	} {
		if _, err = m.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err = m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.ready[m.policies[1].Owner] = true
	m.SetAccessTokenRefresh(func(context.Context, Account) (map[string]any, error) {
		t.Error("runtime recovery must not refresh tokens")
		return nil, nil
	}, time.Minute)
	t.Cleanup(func() { m.operationWG.Wait() })
	return m
}

func queueRecovery(t *testing.T, m *Manager) Policy {
	t.Helper()
	p := m.policies[1]
	Control.Lock()
	err := m.blockForRecovery(context.Background(), p, "EGRESS_SOURCE_UNREADY")
	Control.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.db.Exec(`UPDATE wayvia_egress_recoveries SET next_attempt_at=now()`); err != nil {
		t.Fatal(err)
	}
	return p
}
func runRecovery(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.recoverRuntimeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.operationWG.Wait()
	if err := m.recoverRuntimeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeRecoveryKeepsBindingAndRestoresGate(t *testing.T) {
	m := recoveryFixture(t)
	before := queueRecovery(t, m)
	if _, err := m.Resolve(1, "business"); err == nil {
		t.Fatal("failed account was allowed")
	}
	runRecovery(t, m)
	after := m.policies[1]
	if after.Source != before.Source || after.BindingID != before.BindingID || after.BindingVersion != before.BindingVersion || after.RecoveryState != "completed" || after.State != "ready" {
		t.Fatal(after)
	}
	if _, err := m.Resolve(1, "business"); err != nil {
		t.Fatal("recovery did not reopen gate", err)
	}
	var count int
	if err := m.db.QueryRow(`SELECT count(*) FROM wayvia_account_egress_bindings`).Scan(&count); err != nil || count != 1 {
		t.Fatal("recovery allocated an address", count, err)
	}
}
func TestRuntimeRecoveryKeepsPauseAndRetriesTransientFailure(t *testing.T) {
	m := recoveryFixture(t)
	queueRecovery(t, m)
	if _, err := m.db.Exec(`UPDATE accounts SET schedulable=false WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	runRecovery(t, m)
	if p := m.policies[1]; p.RecoveryState != "completed" || p.State != "ready" || m.accounts[1].Schedulable {
		t.Fatal("paused account recovery did not preserve scheduling state", p, m.accounts[1])
	}
	if _, err := m.db.Exec(`UPDATE accounts SET schedulable=true,status='disabled' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`UPDATE wayvia_egress_recoveries SET state='retry',attempts=0,next_attempt_at=now(),last_error='EGRESS_SOURCE_UNREADY' WHERE owner_id=$1`, m.policies[1].Owner); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`UPDATE wayvia_account_egress_policies SET state='blocked',last_error='EGRESS_SOURCE_UNREADY' WHERE owner_id=$1`, m.policies[1].Owner); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE accounts SET status='disabled'`, `UPDATE accounts SET status='active';UPDATE settings SET value='{"enabled":false,"initialized":true,"revision":1}' WHERE key='wayvia_ipv6_egress'`} {
		if _, err := m.db.Exec(q); err != nil {
			t.Fatal(err)
		}
		runRecovery(t, m)
		if m.policies[1].RecoveryAttempts != 0 {
			t.Fatal("disabled account or paused service attempted recovery")
		}
	}
	if _, err := m.db.Exec(`UPDATE settings SET value='{"enabled":true,"initialized":true,"revision":1}' WHERE key='wayvia_ipv6_egress'`); err != nil {
		t.Fatal(err)
	}
	good := m.probeTransport
	m.probeTransport = testEvidenceTransport(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp6", Err: io.ErrUnexpectedEOF}
	})
	runRecovery(t, m)
	if p := m.policies[1]; p.RecoveryState != "retry" || p.RecoveryAttempts != 1 || p.RecoveryRetryAt == nil || !p.RecoveryRetryAt.After(time.Now()) {
		t.Fatal(p)
	}
	// 模拟重启接续持久任务，不能新建绑定或使用 RT。
	if err := m.recoverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.probeTransport = good
	m.db.Exec(`UPDATE wayvia_egress_recoveries SET next_attempt_at=now()`)
	runRecovery(t, m)
	if m.policies[1].RecoveryState != "completed" {
		t.Fatal(m.policies[1])
	}
}
func TestRuntimeRecoveryRejectsMismatchAndLatePause(t *testing.T) {
	for _, kind := range []string{"mismatch", "pause", "uncertain"} {
		t.Run(kind, func(t *testing.T) {
			m := recoveryFixture(t)
			queueRecovery(t, m)
			m.probeTransport = testEvidenceTransport(func(*http.Request) (*http.Response, error) {
				ip := "2606:4700:abcd:1234::42"
				switch kind {
				case "mismatch":
					ip = "2606:4700:abcd:1234::99"
				case "pause":
					m.db.Exec(`UPDATE accounts SET schedulable=false WHERE id=1`)
				case "uncertain":
					m.uncertain(m.policies[1])
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(ip))}, nil
			})
			runRecovery(t, m)
			if kind == "pause" {
				if m.accounts[1].Schedulable || m.policies[1].RecoveryState != "completed" || m.policies[1].State != "ready" {
					t.Fatal("manual pause lost during recovery", m.accounts[1], m.policies[1])
				}
				if _, err := m.Resolve(1, "business"); err != nil {
					t.Fatal("paused account recovery did not reopen the fixed IPv6 gate", err)
				}
			}
			if kind != "pause" && m.policies[1].RecoveryState != "blocked" {
				t.Fatal(m.policies[1])
			}
		})
	}
}
func TestRuntimeRecoveryCancelsStaleBindingAndResumesClaim(t *testing.T) {
	m := recoveryFixture(t)
	p := queueRecovery(t, m)
	key := "recover-test-" + uuid.NewString()
	m.db.Exec(`UPDATE wayvia_egress_recoveries SET state='running',idempotency_key=$1`, key)
	if err := m.reconcileRecoveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	m.db.QueryRow(`SELECT state FROM wayvia_egress_recoveries`).Scan(&state)
	if state != "retry" {
		t.Fatal("crash before operation did not retry", state)
	}
	m.db.Exec(`UPDATE wayvia_account_egress_policies SET policy_version=policy_version+1 WHERE owner_id=$1`, p.Owner)
	if err := m.reconcileRecoveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.db.QueryRow(`SELECT state FROM wayvia_egress_recoveries`).Scan(&state)
	if state != "cancelled" {
		t.Fatal("stale job was not cancelled", state)
	}
}
func TestRecoveryFailureClassification(t *testing.T) {
	m := fixtureManager(t)
	p := m.policies[1]
	p.Mode = Strict
	p.Target = Strict
	p.State = "ready"
	p.Identity = "user-1"
	p.Source = "2606:4700:abcd:1234::42"
	m.policies[1] = p
	m.ready[p.Owner] = true
	r, err := m.Resolve(1, "business")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.ReportFailure(ctx, io.ErrUnexpectedEOF)
	r.ReportFailure(context.Background(), Error("EGRESS_CREDENTIAL_UNCERTAIN", "no retry"))
	if len(m.faults) != 0 {
		t.Fatal("non-network error queued recovery")
	}
	r.ReportFailure(context.Background(), &net.OpError{Op: "read", Net: "tcp6", Err: io.ErrUnexpectedEOF})
	if len(m.faults) != 1 {
		t.Fatal("network fault missing")
	}
}

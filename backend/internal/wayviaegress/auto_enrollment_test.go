package wayviaegress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func TestATOnlyAccountCanEnrollWithoutRefresh(t *testing.T) {
	m := fixtureManager(t)
	sign := accessTokenSigner(t)
	a := m.accounts[1]
	delete(a.Credentials, "refresh_token")
	m.SetAccessTokenRefresh(func(context.Context, Account) (map[string]any, error) {
		t.Fatal("AT-only account attempted refresh")
		return nil, nil
	}, 3*time.Minute)
	for _, lifetime := range []time.Duration{time.Hour, time.Minute} {
		a.Credentials["access_token"] = sign("user-1", time.Now().Add(lifetime), nil)
		if !a.OrdinaryOAuth() {
			t.Fatal("AT-only ordinary OAuth rejected")
		}
		if _, err := m.prepareAccountAccessToken(context.Background(), a, m.policies[1]); err != nil {
			t.Fatal("valid AT rejected", err)
		}
		if !m.Bindings()[0].CanEnroll || ControlledRefresh(a.ID) {
			t.Fatal("AT-only enrollment and refresh eligibility conflated")
		}
	}
	a.Credentials["access_token"] = sign("user-1", time.Now().Add(-time.Hour), nil)
	if _, err := m.prepareAccountAccessToken(context.Background(), a, m.policies[1]); apperrors.Reason(err) != "EGRESS_ACCESS_TOKEN_EXPIRED" {
		t.Fatal("expired AT accepted without RT", err)
	}
	a.Credentials["access_token"] = ""
	if _, err := m.prepareAccountAccessToken(context.Background(), a, m.policies[1]); apperrors.Reason(err) != "EGRESS_ACCESS_TOKEN_MISSING" {
		t.Fatal("missing AT accepted without RT", err)
	}
	if m.Bindings()[0].CanEnroll || !strings.Contains(m.Bindings()[0].UnsupportedReason, "缺少") {
		t.Fatal("missing credentials did not produce actionable status")
	}
}

func autoEnrollmentFixture(t *testing.T) *Manager {
	t.Helper()
	m := fixtureDB(t)
	newPendingAccount(t, m)
	sign := accessTokenSigner(t)
	future := time.Now().Add(time.Hour)
	setTestAccessToken(t, m, 10, sign("user-10", future, nil), future)
	if _, err := m.db.Exec(`UPDATE accounts SET credentials=credentials-'refresh_token' WHERE id=10;
	 UPDATE settings SET value='{"enabled":true,"initialized":true,"revision":1}' WHERE key='wayvia_ipv6_egress'`); err != nil {
		t.Fatal(err)
	}
	m.config.Prefix, m.config.Primary = "2606:4700:abcd:1234::/64", "2606:4700:abcd:1234::1"
	m.config.NodeID, m.config.Interface, m.config.KeyID = "test-node", "eth0", "test-key"
	m.config.AppUID, m.config.AppGID, m.config.MaxAddresses = 1000, 1000, 100
	m.config.KeyFile = filepath.Join(t.TempDir(), "key")
	m.config.Socket, m.config.AgentStateDir = filepath.Join(t.TempDir(), "missing.sock"), t.TempDir()
	m.config.ProbeURL = "https://example.invalid/ip"
	if err := os.WriteFile(m.config.KeyFile, []byte(strings.Repeat("x", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	m.SetAccessTokenRefresh(func(context.Context, Account) (map[string]any, error) {
		t.Error("valid AT-only account attempted refresh")
		return nil, Error("EGRESS_REFRESH_UNAVAILABLE", "unexpected test refresh")
	}, 3*time.Minute)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.operationWG.Wait() })
	return m
}

func TestAutomaticEnrollmentQueueFollowsImportTransaction(t *testing.T) {
	m := fixtureDB(t)
	newPendingAccount(t, m)
	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO accounts(id,platform,type) VALUES(11,'openai','oauth')`); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if _, err = m.db.Exec(`INSERT INTO accounts(id,platform,type) VALUES(12,'openai','oauth'),(13,'openai','oauth')`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = m.db.QueryRow(`SELECT count(*) FROM wayvia_egress_enrollments WHERE account_id IN (10,12,13) AND state='queued'`).Scan(&count); err != nil || count != 3 {
		t.Fatal("single and bulk imports were not queued", count, err)
	}
	if err = m.db.QueryRow(`SELECT count(*) FROM wayvia_egress_enrollments WHERE account_id IN (1,2,11)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("legacy, API key or rolled-back import was queued", count, err)
	}
	if err = Migrate(context.Background(), m.db); err != nil {
		t.Fatal("migration was not repeatable", err)
	}
}

func TestAutomaticEnrollmentWaitsForGlobalAndDisabledAccounts(t *testing.T) {
	m := autoEnrollmentFixture(t)
	for _, query := range []string{
		`UPDATE settings SET value='{"enabled":false,"initialized":true,"revision":2}' WHERE key='wayvia_ipv6_egress'`,
		`UPDATE settings SET value='{"enabled":true,"initialized":true,"revision":3}' WHERE key='wayvia_ipv6_egress';UPDATE accounts SET status='disabled' WHERE id=10`,
	} {
		if _, err := m.db.Exec(query); err != nil {
			t.Fatal(err)
		}
		if err := m.autoEnrollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		var attempts int
		if err := m.db.QueryRow(`SELECT attempts FROM wayvia_egress_enrollments WHERE account_id=10`).Scan(&attempts); err != nil || attempts != 0 {
			t.Fatal("paused service or disabled account was processed", attempts, err)
		}
	}
}

func TestAutomaticEnrollmentUsesExistingChangeAndReusesBindingOnRetry(t *testing.T) {
	m := autoEnrollmentFixture(t)
	ctx := context.Background()
	var source string
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt == 2 {
			if _, err := m.db.Exec(`UPDATE wayvia_egress_enrollments SET next_attempt_at=now() WHERE account_id=10`); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.autoEnrollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		m.operationWG.Wait()
		if err := m.autoEnrollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		var state, message, actor, address string
		var attempts, count int
		if err := m.db.QueryRow(`SELECT state,attempts,last_error FROM wayvia_egress_enrollments WHERE account_id=10`).Scan(&state, &attempts, &message); err != nil {
			t.Fatal(err)
		}
		if state != "retry" || attempts != attempt || !strings.Contains(message, "EGRESS_SOURCE_UNREADY") {
			t.Fatal("temporary helper failure was not scheduled for retry", state, attempts, message)
		}
		if err := m.db.QueryRow(`SELECT actor FROM wayvia_egress_operations ORDER BY created_at DESC LIMIT 1`).Scan(&actor); err != nil || actor != "system:auto-ipv6" {
			t.Fatal("automatic operation actor missing", actor, err)
		}
		if err := m.db.QueryRow(`SELECT count(*),min(host(ipv6)) FROM wayvia_account_egress_bindings`).Scan(&count, &address); err != nil || count != 1 || attempt == 2 && source != address {
			t.Fatal("retry allocated another address", count, address, err)
		}
		source = address
		if m.accounts[10].Schedulable {
			t.Fatal("failed preparation opened scheduling")
		}
		if _, err := m.Resolve(10, "business"); err == nil {
			t.Fatal("failed preparation allowed ordinary egress")
		}
	}
}

func TestAutomaticEnrollmentReportsMissingCredentialsAndRetriesAfterUpdate(t *testing.T) {
	m := autoEnrollmentFixture(t)
	ctx := context.Background()
	if _, err := m.db.Exec(`UPDATE accounts SET credentials=credentials-'access_token' WHERE id=10`); err != nil {
		t.Fatal(err)
	}
	if err := m.autoEnrollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if p := m.policies[10]; p.AutoEnrollment != "blocked" || !strings.Contains(p.LastError, "EGRESS_ACCESS_TOKEN_MISSING") || m.accounts[10].Schedulable {
		t.Fatal("preflight failure was hidden", p)
	}
	if _, err := m.db.Exec(`UPDATE accounts SET credentials=credentials||'{"access_token":"updated"}' WHERE id=10`); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := m.db.QueryRow(`SELECT state FROM wayvia_egress_enrollments WHERE account_id=10`).Scan(&state); err != nil || state != "queued" {
		t.Fatal("credential update did not requeue", state, err)
	}
}

func TestAutomaticEnrollmentNeverRetriesUncertainCredentials(t *testing.T) {
	m := autoEnrollmentFixture(t)
	ctx := context.Background()
	if _, err := m.db.Exec(`UPDATE wayvia_egress_enrollments SET state='running',attempts=1,idempotency_key='crashed-before-submit' WHERE account_id=10;
	 UPDATE wayvia_account_egress_policies SET state='credential_uncertain',last_error='EGRESS_CREDENTIAL_UNCERTAIN' WHERE root_account_id=10`); err != nil {
		t.Fatal(err)
	}
	if err := m.autoEnrollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if p := m.policies[10]; p.AutoEnrollment != "blocked" || m.accounts[10].Schedulable {
		t.Fatal("uncertain credentials retried", p)
	}
	if _, err := m.db.Exec(`UPDATE wayvia_account_egress_policies SET state='pending_enrollment',last_error='' WHERE root_account_id=10`); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := m.db.QueryRow(`SELECT state FROM wayvia_egress_enrollments WHERE account_id=10`).Scan(&state); err != nil || state != "queued" {
		t.Fatal("confirmed reauthorization did not requeue", state, err)
	}
}

func TestAutomaticEnrollmentUpgradeQueuesExistingPendingOnly(t *testing.T) {
	m := fixtureDB(t)
	newPendingAccount(t, m)
	// 退到第二条迁移的测试结构，验证升级会接续此前已导入的待启用账号。
	if _, err := m.db.Exec(`DROP TRIGGER wayvia_queue_new_enrollment ON wayvia_account_egress_policies;
	 DROP TRIGGER wayvia_requeue_updated_enrollment ON accounts;
	 DROP FUNCTION wayvia_queue_enrollment(); DROP FUNCTION wayvia_requeue_enrollment();
	 DROP TABLE wayvia_egress_enrollments;
	 DELETE FROM wayvia_schema_migrations WHERE filename='003_automatic_enrollment.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), m.db); err != nil {
		t.Fatal(err)
	}
	var count int
	var id int64
	if err := m.db.QueryRow(`SELECT count(*),min(account_id) FROM wayvia_egress_enrollments`).Scan(&count, &id); err != nil || count != 1 || id != 10 {
		t.Fatal("upgrade touched historical accounts or missed pending account", count, id, err)
	}
}

func TestAutomaticEnrollmentResumesUnsubmittedJobAfterRestart(t *testing.T) {
	m := autoEnrollmentFixture(t)
	if _, err := m.db.Exec(`UPDATE wayvia_egress_enrollments SET state='running',attempts=1,idempotency_key='crashed-before-operation' WHERE account_id=10`); err != nil {
		t.Fatal(err)
	}
	if err := m.autoEnrollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := m.policies[10]; p.AutoEnrollment != "retry" || !strings.Contains(p.LastError, "EGRESS_AUTO_ENROLLMENT_INTERRUPTED") || m.accounts[10].Schedulable {
		t.Fatal("durable job not recovered safely", p)
	}
}

func TestAutomaticEnrollmentCompletionPreservesLaterManualPause(t *testing.T) {
	m := autoEnrollmentFixture(t)
	ctx := context.Background()
	if err := m.autoEnrollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	m.operationWG.Wait()
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	p := m.policies[10]
	mark := pending{Operation: Operation{ID: p.OperationID}, EnableAccountID: 10}
	// 以已完成网络核验的事务入口验证自动任务同样提交 ready、调度与 outbox。
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE wayvia_account_egress_bindings SET state='active';
	 UPDATE wayvia_account_egress_policies SET current_mode='strict_ipv6',target_mode='strict_ipv6',state='ready',last_error='' WHERE root_account_id=10`); err != nil {
		t.Fatal(err)
	}
	if err = enableSchedulingAfterReady(ctx, tx, mark, p); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = m.Reload(ctx); err != nil || !m.accounts[10].Schedulable {
		t.Fatal("ready transaction did not open scheduling", err)
	}
	if _, err = m.Resolve(10, "business"); err == nil {
		t.Fatal("database commit bypassed in-process verification")
	}
	if _, err = m.db.Exec(`UPDATE wayvia_egress_operations SET phase='completed',error='' WHERE id=$1`, p.OperationID); err != nil {
		t.Fatal(err)
	}
	if err = m.autoEnrollOnce(ctx); err != nil || m.policies[10].AutoEnrollment != "completed" {
		t.Fatal("completed operation did not close automatic job", err)
	}
	if _, err = m.db.Exec(`UPDATE accounts SET schedulable=false WHERE id=10`); err != nil {
		t.Fatal(err)
	}
	if err = m.autoEnrollOnce(ctx); err != nil || m.accounts[10].Schedulable {
		t.Fatal("completed automatic job reopened manual pause", err)
	}
}

func TestAutomaticEnrollmentContinuesAfterGlobalPauseAndResume(t *testing.T) {
	m := autoEnrollmentFixture(t)
	ctx := context.Background()
	if err := m.autoEnrollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	m.operationWG.Wait()
	if err := m.autoEnrollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first := m.policies[10]
	if first.AutoEnrollment != "retry" || first.Mode != Legacy || first.Target != Strict {
		t.Fatal("expected failed first enrollment", first)
	}
	for _, enabled := range []bool{false, true} {
		op, err := m.Change(ctx, Change{Kind: "runtime", Enabled: enabled, ExpectedRevision: m.runtime.Revision,
			IdempotencyKey: "test-runtime-" + time.Now().Format("150405.000000000"), Acknowledge: true}, "test-admin")
		if err != nil {
			t.Fatal(err)
		}
		m.operationWG.Wait()
		op, err = m.Operation(ctx, op.ID)
		if err != nil || op.Phase != "completed" {
			t.Fatal("runtime change consumed unfinished enrollment", op, err)
		}
		p := m.policies[10]
		if p.Mode != Legacy || p.State != "blocked" || p.OperationID != first.OperationID || m.accounts[10].Schedulable {
			t.Fatal("runtime change bypassed automatic enrollment", p)
		}
	}
	if _, err := m.db.Exec(`UPDATE wayvia_egress_enrollments SET next_attempt_at=now() WHERE account_id=10`); err != nil {
		t.Fatal(err)
	}
	if err := m.autoEnrollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	m.operationWG.Wait()
	var attempts int
	if err := m.db.QueryRow(`SELECT attempts FROM wayvia_egress_enrollments WHERE account_id=10`).Scan(&attempts); err != nil || attempts != 2 {
		t.Fatal("automatic enrollment did not resume", attempts, err)
	}
}

package wayviaegress

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// 仅使用显式测试DSN，每个用例使用独立schema，绝不运行应用主库迁移。
func fixtureDB(t *testing.T) *Manager {
	t.Helper()
	dsn := os.Getenv("WAYVIA_TEST_DSN")
	if dsn == "" {
		t.Skip("set WAYVIA_TEST_DSN for isolated PostgreSQL tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	schema := fmt.Sprintf("wayvia_test_%d", time.Now().UnixNano())
	if _, err = db.Exec(`CREATE SCHEMA ` + schema + `; SET search_path TO ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); db.Close() })
	_, err = db.Exec(`CREATE TABLE accounts(status text NOT NULL DEFAULT 'active',schedulable boolean NOT NULL DEFAULT true,id bigint PRIMARY KEY,name text NOT NULL DEFAULT '',platform text NOT NULL,type text NOT NULL,parent_account_id bigint REFERENCES accounts(id),proxy_id bigint,credentials jsonb NOT NULL DEFAULT '{}',deleted_at timestamptz,updated_at timestamptz NOT NULL DEFAULT now());CREATE TABLE settings(key text PRIMARY KEY,value text NOT NULL,updated_at timestamptz NOT NULL DEFAULT now());CREATE TABLE sub2api_plugin_bindings(enabled boolean,platform text,account_type text);INSERT INTO accounts(id,name,platform,type,credentials) VALUES(1,'oauth','openai','oauth','{"refresh_token":"fake-old-rt","access_token":"fake-old-at","chatgpt_user_id":"user-1"}'),(2,'api','openai','apikey','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE scheduler_outbox(event_type text,account_id bigint,group_id bigint,payload jsonb)`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	m := fixtureManager(t)
	m.db = db
	m.config = Config{StateDir: t.TempDir()}
	f, err := os.Create(filepath.Join(m.config.StateDir, "test-lock"))
	if err != nil {
		t.Fatal(err)
	}
	m.lock = f
	t.Cleanup(func() { f.Close() })
	if err = m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestDatabaseMigrationAndAccountGuards(t *testing.T) {
	m := fixtureDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, m.db); err != nil {
		t.Fatal("repeat migration", err)
	}
	statements := []string{
		`INSERT INTO accounts(id,platform,type,credentials) VALUES(3,'openai','oauth','{"chatgpt_user_id":"user-3","refresh_token":"rt3"}')`,
		`INSERT INTO wayvia_ipv6_pools(id,cidr,node_id,interface) VALUES('default','2606:4700:abcd:1234::/64','n','eth0')`,
		`UPDATE wayvia_account_egress_policies SET identity_key='user-1',current_mode='strict_ipv6',target_mode='strict_ipv6',state='ready' WHERE root_account_id=1`,
		`INSERT INTO wayvia_account_egress_bindings(owner_id,pool_id,ipv6,binding_version,allocation_key_id,state) SELECT owner_id,'default','2606:4700:abcd:1234::1',2,'k','active' FROM wayvia_account_egress_policies WHERE root_account_id=1`,
		`UPDATE wayvia_account_egress_policies SET active_binding_id=(SELECT id FROM wayvia_account_egress_bindings LIMIT 1) WHERE root_account_id=1`,
		`UPDATE settings SET value='{"enabled":true,"initialized":true,"revision":1}' WHERE key='wayvia_ipv6_egress'`,
		`INSERT INTO accounts(id,platform,type,parent_account_id,credentials) VALUES(4,'openai','oauth',1,'{"model_mapping":{}}')`,
	}
	for _, q := range statements {
		if _, e := m.db.Exec(q); e != nil {
			t.Fatal(q, e)
		}
	}
	bad := []string{
		`UPDATE accounts SET credentials='{"refresh_token":"stale"}' WHERE id=1`,
		`UPDATE accounts SET proxy_id=99 WHERE id=1`,
		`UPDATE accounts SET type='apikey' WHERE id=1`,
		`DELETE FROM accounts WHERE id=1`,
		`INSERT INTO accounts(id,platform,type,credentials) VALUES(5,'openai','oauth','{"chatgpt_user_id":"user-1"}')`,
		`INSERT INTO accounts(id,platform,type,parent_account_id,proxy_id) VALUES(6,'openai','oauth',1,9)`,
		`INSERT INTO accounts(id,platform,type,parent_account_id) VALUES(7,'openai','oauth',2)`,
		`UPDATE accounts SET parent_account_id=1 WHERE id=3`,
		`INSERT INTO wayvia_account_egress_bindings(owner_id,pool_id,ipv6,binding_version,allocation_key_id) SELECT owner_id,'default','2606:4700:abcd:1235::1',1,'k' FROM wayvia_account_egress_policies WHERE root_account_id=1`,
	}
	for _, q := range bad {
		if _, e := m.db.Exec(q); e == nil {
			t.Fatal("guard accepted", q)
		}
	}
	_, e := m.db.Exec(`INSERT INTO wayvia_account_egress_bindings(owner_id,pool_id,ipv6,binding_version,allocation_key_id) SELECT owner_id,'default','2606:4700:abcd:1234::2',1,'k' FROM wayvia_account_egress_policies WHERE root_account_id=1`)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.db.Exec(`UPDATE wayvia_account_egress_policies SET active_binding_id=(SELECT id FROM wayvia_account_egress_bindings LIMIT 1) WHERE root_account_id=3`); e == nil {
		t.Fatal("cross-owner binding accepted")
	}
	if _, e = m.db.Exec(`UPDATE wayvia_schema_migrations SET checksum='tampered'`); e != nil {
		t.Fatal(e)
	}
	if e = Migrate(ctx, m.db); e == nil {
		t.Fatal("checksum mismatch accepted")
	}
}
func TestRefreshSurvivesBusinessCancelAndFirstEnrollment(t *testing.T) {
	m := fixtureDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := m.Refresh(ctx, 1, func(callCtx context.Context, a Account) (map[string]any, error) {
			close(started)
			<-release
			if callCtx.Err() != nil {
				return nil, callCtx.Err()
			}
			if a.Credentials["refresh_token"] != "fake-old-rt" {
				return nil, fmt.Errorf("did not re-read credentials")
			}
			return map[string]any{"access_token": "fake-new-at", "refresh_token": "fake-new-rt", "chatgpt_user_id": "user-1"}, nil
		})
		done <- err
	}()
	<-started
	cancel()
	p := m.policies[1]
	m.closeOwner(p.Owner, false)
	if _, e := m.db.Exec(`UPDATE wayvia_account_egress_policies SET identity_key='user-1',target_mode='strict_ipv6',state='preparing',policy_version=policy_version+1 WHERE root_account_id=1`); e != nil {
		t.Fatal(e)
	}
	if e := m.Reload(context.Background()); e != nil {
		t.Fatal(e)
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	var credentials []byte
	var state string
	if e := m.db.QueryRow(`SELECT credentials,p.state FROM accounts a JOIN wayvia_account_egress_policies p ON p.root_account_id=a.id WHERE a.id=1`).Scan(&credentials, &state); e != nil {
		t.Fatal(e)
	}
	var got map[string]any
	json.Unmarshal(credentials, &got)
	if got["refresh_token"] != "fake-new-rt" || state != "preparing" {
		t.Fatal(string(credentials), state)
	}
	files, _ := filepath.Glob(filepath.Join(m.config.StateDir, "refresh-*.json"))
	if len(files) != 0 {
		t.Fatal("saved result retained uncertain marker")
	}
	if _, e := m.Resolve(1, "business"); e == nil {
		t.Fatal("refresh reopened paused owner")
	}
}
func TestRefreshCASConflictAndRestartAreUncertain(t *testing.T) {
	m := fixtureDB(t)
	_, err := m.Refresh(context.Background(), 1, func(context.Context, Account) (map[string]any, error) {
		_, e := m.db.Exec(`UPDATE accounts SET credentials=credentials||'{"access_token":"concurrent"}' WHERE id=1`)
		return map[string]any{"access_token": "new-at", "refresh_token": "late"}, e
	})
	if err == nil {
		t.Fatal("CAS conflict accepted")
	}
	if m.policies[1].State != "credential_uncertain" {
		t.Fatal(m.policies[1])
	}
	if _, err = m.Resolve(1, "refresh"); err == nil {
		t.Fatal("uncertain retried")
	}
	if err = m.recoverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.policies[1].State != "credential_uncertain" {
		t.Fatal("restart cleared uncertainty")
	}
}
func TestRuntimePausePreservesCredentialUncertainty(t *testing.T) {
	m := fixtureDB(t)
	_, err := m.db.Exec(`UPDATE wayvia_account_egress_policies SET current_mode='strict_ipv6',target_mode='strict_ipv6',identity_key='user-1',state='credential_uncertain' WHERE root_account_id=1`)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.initError = ""
	op, err := m.Change(context.Background(), Change{Kind: "runtime", Enabled: false, ExpectedRevision: 1, IdempotencyKey: "pause-uncertain", Acknowledge: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second * 3)
	for {
		op, err = m.Operation(context.Background(), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Phase == "completed" {
			break
		}
		if op.Phase == "failed" || time.Now().After(deadline) {
			t.Fatal(op)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m.policies[1].State != "credential_uncertain" {
		t.Fatal("pause erased uncertainty")
	}
	if _, err = m.Change(context.Background(), Change{Kind: "runtime", Enabled: true, ExpectedRevision: 2, IdempotencyKey: "resume-uncertain", Acknowledge: true}, "test"); err == nil {
		t.Fatal("resume bypassed uncertainty")
	}
}
func TestPendingCrashNeverBecomesLegacy(t *testing.T) {
	m := fixtureDB(t)
	p := m.policies[1]
	mark := pending{Operation: Operation{ID: "22222222-2222-4222-8222-222222222222", Target: Strict}, Owners: []string{p.Owner}}
	if e := durableJSON(m.pendingPath(mark.Operation.ID), mark); e != nil {
		t.Fatal(e)
	}
	if e := m.recoverPending(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Resolve(1, "business"); e == nil {
		t.Fatal("missing DB operation reopened legacy")
	}
}

func TestPartialRecoveryKeepsOnlyUnrecoveredOwners(t *testing.T) {
	m := fixtureDB(t)
	mark := pending{Operation: Operation{ID: "33333333-3333-4333-8333-333333333333"}, Owners: []string{"owner-a", "owner-b"}}
	if err := durableJSON(m.pendingPath(mark.Operation.ID), mark); err != nil {
		t.Fatal(err)
	}
	if err := m.clearOwnerMarkers(pending{Owners: []string{"owner-a"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(m.pendingPath(mark.Operation.ID))
	if err != nil {
		t.Fatal(err)
	}
	var got pending
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Owners) != 1 || got.Owners[0] != "owner-b" {
		t.Fatal(string(raw))
	}
}

func TestRestoreDeletedAccountPreservesOwnerAndBlocksCredentials(t *testing.T) {
	m := fixtureDB(t)
	ctx := context.Background()
	owner := m.policies[1].Owner
	if _, err := m.db.Exec(`UPDATE wayvia_account_egress_policies SET identity_key='user-1',current_mode='strict_ipv6',target_mode='strict_ipv6',state='retired' WHERE root_account_id=1; UPDATE accounts SET deleted_at=now() WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	retired, err := m.RetiredAccounts(ctx)
	if err != nil || len(retired) != 1 {
		t.Fatal(retired, err)
	}
	op, err := m.Change(ctx, Change{Kind: "restore-account", AccountID: 1, ExpectedVersion: retired[0].Version, IdempotencyKey: "restore-original-owner", Acknowledge: true}, "tester")
	if err != nil || op.Phase != "completed" {
		t.Fatal(op, err)
	}
	if p := m.policies[1]; p.Owner != owner || p.State != "credential_uncertain" || p.Mode != Strict {
		t.Fatal(p)
	}
	var status string
	var sched bool
	if err = m.db.QueryRow(`SELECT status,schedulable FROM accounts WHERE id=1 AND deleted_at IS NULL`).Scan(&status, &sched); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || sched {
		t.Fatal(status, sched)
	}
	if _, err = m.Resolve(1, "business"); err == nil {
		t.Fatal("restoration allowed business")
	}
	if _, err = m.Reauthorize(ctx, 1, retired[0].Version, func(context.Context) (map[string]any, error) {
		t.Fatal("stale reauth reached exchange")
		return nil, nil
	}); err == nil {
		t.Fatal("stale version accepted")
	}
}

func TestRestoreRollbackRemainsRecoverableAfterRestart(t *testing.T) {
	m := fixtureDB(t)
	ctx := context.Background()
	owner := m.policies[1].Owner
	if _, err := m.db.Exec(`UPDATE wayvia_account_egress_policies SET identity_key='user-1',current_mode='strict_ipv6',target_mode='strict_ipv6',state='retired' WHERE root_account_id=1; UPDATE accounts SET deleted_at=now() WHERE id=1; DROP TABLE scheduler_outbox`); err != nil {
		t.Fatal(err)
	}
	retired, err := m.RetiredAccounts(ctx)
	if err != nil || len(retired) != 1 {
		t.Fatal(retired, err)
	}
	c := Change{Kind: "restore-account", AccountID: 1, ExpectedVersion: retired[0].Version, IdempotencyKey: "restore-rollback-retry", Acknowledge: true}
	if _, err = m.Change(ctx, c, "tester"); err == nil {
		t.Fatal("outbox failure did not roll back restore")
	}
	files, err := filepath.Glob(filepath.Join(m.config.StateDir, "pending-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	if err = m.recoverPending(ctx); err != nil {
		t.Fatal(err)
	}
	retired, err = m.RetiredAccounts(ctx)
	if err != nil || len(retired) != 1 || retired[0].Owner != owner || retired[0].Version != c.ExpectedVersion {
		t.Fatal("restore entry lost after restart", retired, err)
	}
	if _, err = m.Resolve(1, "business"); err == nil {
		t.Fatal("deleted account reopened")
	}
	if _, err = m.db.Exec(`CREATE TABLE scheduler_outbox(event_type text,account_id bigint,group_id bigint,payload jsonb)`); err != nil {
		t.Fatal(err)
	}
	op, err := m.Change(ctx, c, "tester")
	if err != nil || op.Phase != "completed" || m.policies[1].State != "credential_uncertain" {
		t.Fatal(op, err, m.policies[1])
	}
	files, err = filepath.Glob(filepath.Join(m.config.StateDir, "pending-*.json"))
	if err != nil || len(files) != 0 {
		t.Fatal("recovered markers retained", files, err)
	}
}

func TestLegacyReauthPublishesCredentialsAndOutbox(t *testing.T) {
	m := fixtureDB(t)
	ctx := context.Background()
	version := m.policies[1].Version
	next, err := m.Reauthorize(ctx, 1, version, func(context.Context) (map[string]any, error) {
		return map[string]any{"access_token": "new-at", "refresh_token": "new-rt", "chatgpt_user_id": "user-1"}, nil
	})
	if err != nil || next["refresh_token"] != "new-rt" {
		t.Fatal(next, err)
	}
	if _, err = m.Resolve(1, "business"); err != nil {
		t.Fatal(err)
	}
	var events int
	if err = m.db.QueryRow(`SELECT count(*) FROM scheduler_outbox WHERE account_id=1`).Scan(&events); err != nil || events != 1 {
		t.Fatal(events, err)
	}
	if _, err = m.Reauthorize(ctx, 1, version, func(context.Context) (map[string]any, error) { t.Fatal("reused reauth version"); return nil, nil }); err == nil {
		t.Fatal("stale version accepted")
	}
}

func TestCredentialPublicationFailureRetainsRecoveryMarker(t *testing.T) {
	m := fixtureDB(t)
	// token结果已返回，但同一事务的调度outbox失败，必须回滚凭据并阻断。
	if _, err := m.db.Exec(`DROP TABLE scheduler_outbox`); err != nil {
		t.Fatal(err)
	}
	_, err := m.Refresh(context.Background(), 1, func(context.Context, Account) (map[string]any, error) {
		return map[string]any{"access_token": "new-at", "refresh_token": "new-rt"}, nil
	})
	if err == nil || m.policies[1].State != "credential_uncertain" {
		t.Fatal(err, m.policies[1])
	}
	files, _ := filepath.Glob(filepath.Join(m.config.StateDir, "refresh-*.json"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	var token string
	if err = m.db.QueryRow(`SELECT credentials->>'refresh_token' FROM accounts WHERE id=1`).Scan(&token); err != nil || token != "fake-old-rt" {
		t.Fatal(token, err)
	}
}

func TestMetadataCannotOverwriteNewRefreshToken(t *testing.T) {
	m := fixtureDB(t)
	ctx := context.Background()
	expected := map[string]any{"refresh_token": "fake-old-rt", "access_token": "fake-old-at", "chatgpt_user_id": "user-1"}
	if _, err := m.db.Exec(`UPDATE accounts SET credentials=credentials||'{"refresh_token":"new-rt"}' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveMetadata(1, expected, map[string]any{"plan_type": "plus", "refresh_token": "injected"}); err == nil {
		t.Fatal("stale metadata replaced token")
	}
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	actual := m.accounts[1].Credentials
	next, err := m.SaveMetadata(1, actual, map[string]any{"plan_type": "plus", "refresh_token": "injected"})
	if err != nil {
		t.Fatal(err)
	}
	if next["refresh_token"] != "new-rt" || next["plan_type"] != "plus" {
		t.Fatal(next)
	}
}

func TestOperationWithoutMarkerRestartsBlocked(t *testing.T) {
	m := fixtureDB(t)
	ctx := context.Background()
	if _, err := m.db.Exec(`INSERT INTO wayvia_egress_operations(id,idempotency_key,request_hash,kind,target,expected_version,actor,phase) VALUES('44444444-4444-4444-8444-444444444444','reconcile-no-file','x','mode','strict_ipv6',1,'test','preparing');UPDATE wayvia_account_egress_policies SET operation_id='44444444-4444-4444-8444-444444444444',state='ready',current_mode='strict_ipv6',target_mode='strict_ipv6' WHERE root_account_id=1`); err != nil {
		t.Fatal(err)
	}
	if err := m.recoverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if m.policies[1].State != "blocked" {
		t.Fatal(m.policies[1])
	}
	op, err := m.Operation(ctx, "44444444-4444-4444-8444-444444444444")
	if err != nil || op.Phase != "failed" {
		t.Fatal(op, err)
	}
}

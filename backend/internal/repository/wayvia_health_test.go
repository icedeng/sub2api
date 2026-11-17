package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/wayviahealth"
)

func healthRepositoryDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("WAYVIA_HEALTH_TEST_DSN")
	if dsn == "" {
		t.Skip("set WAYVIA_HEALTH_TEST_DSN for isolated PostgreSQL checks")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("health_repo_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, _ := url.Parse(dsn)
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); admin.Close() })
	_, err = db.Exec(`CREATE TABLE accounts(id BIGINT PRIMARY KEY,platform TEXT DEFAULT 'openai',type TEXT DEFAULT 'oauth',credentials JSONB DEFAULT '{}',extra JSONB DEFAULT '{}',status TEXT DEFAULT 'active',schedulable BOOLEAN DEFAULT TRUE,error_message TEXT,updated_at TIMESTAMPTZ DEFAULT NOW(),deleted_at TIMESTAMPTZ,rate_limited_at TIMESTAMPTZ,rate_limit_reset_at TIMESTAMPTZ,overload_until TIMESTAMPTZ,temp_unschedulable_until TIMESTAMPTZ,temp_unschedulable_reason TEXT);
CREATE TABLE scheduled_test_plans(id BIGSERIAL PRIMARY KEY,account_id BIGINT,model_id TEXT,cron_expression TEXT,enabled BOOLEAN,max_results INT,auto_recover BOOLEAN,last_run_at TIMESTAMPTZ,next_run_at TIMESTAMPTZ,created_at TIMESTAMPTZ,updated_at TIMESTAMPTZ);
CREATE TABLE scheduler_outbox(event_type TEXT,account_id BIGINT,group_id BIGINT,payload JSONB);
INSERT INTO accounts(id,credentials) VALUES(1,'{"access_token":"new"}'),(2,'{}');`)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/251_wayvia_account_health.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestWayviaHealthCredentialGuardAllMutations(t *testing.T) {
	db := healthRepositoryDB(t)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	r := &accountRepository{client: client, sql: db}
	ctx := wayviahealth.WithCredentialGuard(context.Background(), 1, map[string]any{"access_token": "old"})
	ctx = wayviahealth.WithCredentialGuard(ctx, 2, map[string]any{})
	until := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name  string
		apply func() error
	}{
		{"error", func() error { return r.SetError(ctx, 1, "old result") }},
		{"rate", func() error { return r.SetRateLimited(ctx, 1, until) }},
		{"overload", func() error { return r.SetOverloaded(ctx, 1, until) }},
		{"temp", func() error { return r.SetTempUnschedulable(ctx, 1, until, "old result") }},
		{"model", func() error { return r.SetModelRateLimit(ctx, 2, "test", until) }},
		{"extra", func() error { return r.UpdateExtra(ctx, 2, map[string]any{"codex_primary_used_percent": 100}) }},
		{"credentials", func() error { return r.UpdateCredentials(ctx, 1, map[string]any{"access_token": "old-written-back"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.apply()
			if tc.name == "temp" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, wayviahealth.ErrStaleCredentials) && !errors.Is(err, service.ErrAccountNotFound) {
				t.Fatalf("unexpected mutation error: %v", err)
			}
			var untouched bool
			err = db.QueryRow(`SELECT bool_and(status='active' AND schedulable AND rate_limit_reset_at IS NULL AND overload_until IS NULL AND temp_unschedulable_until IS NULL AND extra='{}'::jsonb AND (id<>1 OR credentials='{"access_token":"new"}'::jsonb)) FROM accounts`).Scan(&untouched)
			if err != nil || !untouched {
				t.Fatal("stale mutation reached current account", err)
			}
		})
	}
	current := wayviahealth.WithCredentialGuard(context.Background(), 1, map[string]any{"access_token": "new"})
	if err := r.SetError(current, 1, "current failure"); err != nil {
		t.Fatal("current credential guard rejected", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM accounts WHERE id=1`).Scan(&status); err != nil || status != "error" {
		t.Fatal(status, err)
	}

}

func TestWayviaHealthPlanContentTransaction(t *testing.T) {
	db := healthRepositoryDB(t)
	r := &scheduledTestPlanRepository{db: db}
	ctx := context.Background()
	initial := "original"
	p, err := r.Create(ctx, &service.ScheduledTestPlan{AccountID: 1, ModelID: "model-1", CronExpression: "0 * * * *", TestContent: &initial})
	if err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("x", 16385)
	p.ModelID = "should-rollback"
	p.TestContent = &oversized
	if _, err = r.Update(ctx, p); err == nil {
		t.Fatal("expected content constraint failure")
	}
	var model, content string
	err = db.QueryRow(`SELECT p.model_id,c.test_content FROM scheduled_test_plans p JOIN wayvia_health_plan_content c ON p.id=c.plan_id WHERE p.id=$1`, p.ID).Scan(&model, &content)
	if err != nil || model != "model-1" || content != "original" {
		t.Fatal("partial plan update", model, content, err)
	}
	p.TestContent = nil
	p.ModelID = "model-2"
	if _, err = r.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	var inherited bool
	db.QueryRow(`SELECT test_content IS NULL FROM wayvia_health_plan_content WHERE plan_id=$1`, p.ID).Scan(&inherited)
	if !inherited {
		t.Fatal("null did not restore inheritance")
	}
}

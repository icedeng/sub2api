package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/wayviahealth"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

// 独立 schema + 显式测试 DSN，绝不读取应用配置或接触生产账号。
func healthFixture(t *testing.T) (*WayviaHealthService, *healthFixtureRepo) {
	t.Helper()
	dsn := os.Getenv("WAYVIA_HEALTH_TEST_DSN")
	if dsn == "" {
		t.Skip("set WAYVIA_HEALTH_TEST_DSN for isolated PostgreSQL health checks")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("health_test_%d", time.Now().UnixNano())
	if _, err = db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	adminDB := db
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, _ := url.Parse(dsn)
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(6)
	t.Cleanup(func() { db.Close(); adminDB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); adminDB.Close() })
	_, err = db.Exec(`CREATE TABLE accounts(id BIGINT PRIMARY KEY,extra JSONB NOT NULL DEFAULT '{}',credentials JSONB NOT NULL DEFAULT '{}',deleted_at TIMESTAMPTZ,updated_at TIMESTAMPTZ DEFAULT NOW(),status TEXT DEFAULT 'active',schedulable BOOLEAN DEFAULT TRUE,temp_unschedulable_until TIMESTAMPTZ,temp_unschedulable_reason TEXT,rate_limit_reset_at TIMESTAMPTZ,overload_until TIMESTAMPTZ); CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at TIMESTAMPTZ DEFAULT NOW()); CREATE TABLE scheduled_test_plans(id BIGINT PRIMARY KEY,account_id BIGINT NOT NULL,model_id TEXT NOT NULL DEFAULT '',enabled BOOLEAN DEFAULT TRUE); CREATE TABLE scheduler_outbox(event_type TEXT,account_id BIGINT); INSERT INTO accounts(id) VALUES(1),(2),(3)`)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/251_wayvia_account_health.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = db.Exec(string(migration)); err != nil {
			t.Fatal("idempotent migration", err)
		}
	}
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "fake-key", "base_url": "https://example.invalid"}, Extra: map[string]any{wayviahealth.PolicyKey: map[string]any{"mode": "inherit"}}}
	b := *a
	b.ID = 2
	c := *a
	c.ID = 3
	repo := &healthFixtureRepo{db: db, accounts: map[int64]*Account{1: a, 2: &b, 3: &c}}
	for _, item := range repo.accounts {
		credentials, _ := json.Marshal(item.Credentials)
		extra, _ := json.Marshal(item.Extra)
		if _, err = db.Exec(`UPDATE accounts SET credentials=$2,extra=$3 WHERE id=$1`, item.ID, string(credentials), string(extra)); err != nil {
			t.Fatal(err)
		}
	}
	s := NewWayviaHealthService(db, repo, nil, nil, nil, nil, nil, "test-only-key")
	tester := &AccountTestService{health: s, accountRepo: repo, cfg: &config.Config{}}
	s.tester = tester
	return s, repo
}

type healthFixtureRepo struct {
	AccountRepository
	db       *sql.DB
	accounts map[int64]*Account
}

func (r *healthFixtureRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	a := r.accounts[id]
	if a == nil {
		return nil, fmt.Errorf("missing account")
	}
	copy := *a
	return &copy, nil
}
func (r *healthFixtureRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	b, _ := json.Marshal(updates)
	_, err := r.db.ExecContext(ctx, `UPDATE accounts SET extra=extra || $2::jsonb WHERE id=$1`, id, string(b))
	if err == nil {
		copy := map[string]any{}
		for k, v := range r.accounts[id].Extra {
			copy[k] = v
		}
		for k, v := range updates {
			copy[k] = v
		}
		r.accounts[id].Extra = copy
	}
	return err
}
func (r *healthFixtureRepo) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	out := []Account{}
	for _, a := range r.accounts {
		out = append(out, *a)
	}
	return out, nil
}

func TestWayviaHealthSettingsCASAndNoAutomaticRequests(t *testing.T) {
	s, _ := healthFixture(t)
	ctx := context.Background()
	cfg, err := s.Settings(ctx)
	if err != nil || cfg.PeriodicEnabled || cfg.ErrorRecheck || cfg.Version != 0 {
		t.Fatal(cfg, err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	success := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		go func() { defer wg.Done(); _, err := s.SaveSettings(ctx, cfg); success <- err == nil }()
	}
	wg.Wait()
	close(success)
	n := 0
	for ok := range success {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("CAS accepted %d concurrent writes", n)
	}
	cfg, err = s.Settings(ctx)
	if err != nil || cfg.Version != 1 {
		t.Fatal(cfg, err)
	}
	cfg.TestContent = "{{date}} {{rand:1-3}}"
	cfg, err = s.SaveSettings(ctx, cfg)
	if err != nil || cfg.Version != 2 {
		t.Fatal(cfg, err)
	}
	s.sweep(ctx, cfg)
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM wayvia_account_test_runs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("save/default sweep created probes", count, err)
	}
}

func TestWayviaHealthQueueConcurrencyAndRestart(t *testing.T) {
	s, _ := healthFixture(t)
	ctx := context.Background()
	r1, err := s.newRun(ctx, 1, "batch", "batch", "gpt-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	r1, err = s.insertRun(ctx, r1)
	if err != nil {
		t.Fatal(err)
	}
	dup := *r1
	dup.ID = newHealthID()
	if _, err = s.insertRun(ctx, &dup); !healthRunBusy(err) {
		t.Fatalf("duplicate not blocked: %v", err)
	}
	r2, err := s.newRun(ctx, 2, "batch", "batch", "gpt-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	r2, err = s.insertRun(ctx, r2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(ctx, r1.ID, 1, 45); err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(ctx, r2.ID, 1, 45); !healthRunBusy(err) {
		t.Fatal("global concurrency not enforced", err)
	}
	_, err = s.db.Exec(`UPDATE wayvia_account_test_runs SET lease_until=NOW()-INTERVAL '1 minute' WHERE id=$1`, r1.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.sweep(ctx, wayviahealth.Defaults())
	recovered, err := s.GetRun(ctx, r1.ID)
	if err != nil || recovered.State != "cancelled" {
		t.Fatal("expired lease replayed", recovered, err)
	}
	if _, err = s.claim(ctx, r2.ID, 1, 45); err != nil {
		t.Fatal(err)
	}
}

func TestWayviaHealthApplyPreservesExistingPolicyAndPause(t *testing.T) {
	s, _ := healthFixture(t)
	ctx := context.Background()
	_, err := s.db.Exec(`UPDATE accounts SET extra='{"other":1,"wayvia_health_check":{"mode":"exclude"}}',schedulable=false WHERE id=1; UPDATE accounts SET extra='{"other":2}' WHERE id=2`)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Apply(ctx, []int64{1, 2})
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	var mode string
	var paused bool
	var other int
	err = s.db.QueryRow(`SELECT extra->'wayvia_health_check'->>'mode',schedulable,(extra->>'other')::int FROM accounts WHERE id=1`).Scan(&mode, &paused, &other)
	if err != nil || mode != "exclude" || paused || other != 1 {
		t.Fatal(mode, paused, other, err)
	}
	err = s.db.QueryRow(`SELECT extra->'wayvia_health_check'->>'mode',(extra->>'other')::int FROM accounts WHERE id=2`).Scan(&mode, &other)
	if err != nil || mode != "inherit" || other != 2 {
		t.Fatal(mode, other, err)
	}
}

func TestWayviaHealthStaleCredentialAndDisabledPolicyCancel(t *testing.T) {
	s, repo := healthFixture(t)
	ctx := context.Background()
	cfg := wayviahealth.Defaults()
	cfg.PeriodicEnabled = true
	cfg, err := s.SaveSettings(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.newRun(ctx, 1, "periodic", "", "gpt-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.insertRun(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.claim(ctx, r.ID, 1, 45); err != nil {
		t.Fatal(err)
	}
	repo.accounts[1].Credentials = map[string]any{"api_key": "changed"}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/", nil)
	_, _, _, err = s.prepare(c, repo.accounts[1], "gpt-test", "", AccountTestOptions{HealthRunID: r.ID})
	if err == nil {
		t.Fatal("accepted stale credentials")
	}
	result, err := s.GetRun(ctx, r.ID)
	if err != nil || result.State != "cancelled" {
		t.Fatal(result, err)
	}
	r, err = s.newRun(ctx, 2, "periodic", "", "gpt-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.insertRun(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PeriodicEnabled = false
	if _, err = s.SaveSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	result, err = s.GetRun(ctx, r.ID)
	if err != nil || result.State != "cancelled" {
		t.Fatal("disabled policy left queued probe", result, err)
	}
}

type healthFakeUpstream struct {
	content string
	body    string
	calls   int
}

func (u *healthFakeUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	u.body = string(b)
	u.calls++
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(u.content))}, nil
}
func (u *healthFakeUpstream) DoWithTLS(req *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, id, n)
}

func TestWayviaHealthUnifiedTextAndIncompleteStreams(t *testing.T) {
	s, repo := healthFixture(t)
	ctx := context.Background()
	cfg := wayviahealth.Defaults()
	cfg.TestContent = "configured {{rand:8-8}}"
	if _, err := s.SaveSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	up := &healthFakeUpstream{content: "data: {\"choices\":[{\"delta\":{\"content\":\"anything\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"}
	s.tester.httpUpstream = up
	repo.accounts[1].Extra[openai_compat.ExtraKeyResponsesSupported] = false
	result, err := s.tester.RunTestBackground(ctx, 1, "gpt-test")
	if err != nil || result.Status != "success" {
		t.Fatal(result, err)
	}
	if up.calls != 1 || !strings.Contains(up.body, "configured 8") {
		t.Fatal("template was not sent", up.calls, up.body)
	}
	up.content = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: [DONE]\n\n"
	result, err = s.tester.RunTestBackground(ctx, 1, "gpt-test")
	if err != nil || result.Status != "failed" {
		t.Fatal("truncated stream passed", result, err)
	}
	rows, err := s.Runs(ctx, 1, "", "", 100, nil)
	if err != nil || len(rows) != 2 || rows[0].State != "failed" || rows[0].ErrorClass != "incomplete" {
		t.Fatal(rows, err)
	}
}

func TestWayviaHealthResponsesPayloadAndTermination(t *testing.T) {
	payload := createOpenAITestPayload("test", true, "configured text")
	b, _ := json.Marshal(payload)
	if !strings.Contains(string(b), "configured text") || payload["store"] != false || payload["instructions"] == nil {
		t.Fatal(payload)
	}
	for _, event := range []string{`{"type":"response.incomplete"}`, `{"type":"response.done","response":{"status":"failed"}}`, `{"type":"response.failed","response":{"error":{"message":"no"}}}`} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/", nil)
		if err := (&AccountTestService{}).processOpenAIStream(c, strings.NewReader("data: "+event+"\n\n")); err == nil {
			t.Fatalf("accepted %s", event)
		}
	}
}

func (r *healthFixtureRepo) SetError(ctx context.Context, id int64, message string) error {
	a := r.accounts[id]
	a.Status = StatusError
	a.Schedulable = false
	a.ErrorMessage = message
	return nil
}
func (r *healthFixtureRepo) SetTempUnschedulable(ctx context.Context, id int64, at time.Time, reason string) error {
	a := r.accounts[id]
	a.TempUnschedulableUntil = &at
	a.TempUnschedulableReason = reason
	return nil
}
func (r *healthFixtureRepo) SetRateLimited(ctx context.Context, id int64, at time.Time) error {
	r.accounts[id].RateLimitResetAt = &at
	return nil
}

func TestWayviaHealthFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantStatus string
		temp, rate bool
	}{
		{"plain OAuth 401", 401, `{"error":{"message":"secret-prompt"}}`, StatusActive, true, false},
		{"revoked", 401, `{"error":{"code":"token_revoked","message":"secret-prompt"}}`, StatusError, false, false},
		{"html403", 403, `<html>secret-prompt</html>`, StatusActive, false, false},
		{"cloudflare1010", 403, `{"cloudflare_error":true,"code":1010,"message":"secret-prompt"}`, StatusActive, false, false},
		{"429 reset", 429, fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d,"message":"secret-prompt"}}`, time.Now().Add(time.Hour).Unix()), StatusActive, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo := healthFixture(t)
			a := repo.accounts[1]
			a.Type = AccountTypeOAuth
			a.Credentials = map[string]any{"access_token": "fake-at", "refresh_token": "fake-rt"}
			s.rateLimit = &RateLimitService{accountRepo: repo, cfg: &config.Config{}}
			ctx := s.requestContext(context.Background(), a, a, "quota")
			s.HandleFailure(ctx, a, tc.status, http.Header{}, []byte(tc.body), "quota", "")
			if a.Status != tc.wantStatus || (a.TempUnschedulableUntil != nil) != tc.temp || (a.RateLimitResetAt != nil) != tc.rate {
				t.Fatalf("unexpected state: %s temp=%v rate=%v", a.Status, a.TempUnschedulableUntil, a.RateLimitResetAt)
			}
			if strings.Contains(a.ErrorMessage+a.TempUnschedulableReason, "secret-prompt") {
				t.Fatal("raw response persisted")
			}
			runs, err := s.Runs(ctx, 1, "", "", 100, nil)
			if err != nil || len(runs) != 1 || !runs[0].StateChanged && (tc.temp || tc.rate || tc.wantStatus == StatusError) {
				t.Fatal(runs, err)
			}
			if a.GetOpenAIRefreshToken() != "fake-rt" {
				t.Fatal("RT changed")
			}
		})
	}
}

func TestWayviaHealthStaleRequestAndDisabledInFlight(t *testing.T) {
	s, repo := healthFixture(t)
	a := repo.accounts[1]
	s.rateLimit = &RateLimitService{accountRepo: repo, cfg: &config.Config{}}
	ctx := s.requestContext(context.Background(), a, a, "responses")
	a.Credentials = map[string]any{"api_key": "new-key"}
	s.HandleFailure(ctx, a, 401, http.Header{}, []byte(`{"error":{"code":"token_revoked"}}`), "manual", "gpt-test")
	if a.Status != StatusActive {
		t.Fatal("stale result disabled new credentials")
	}
	s.ObserveSuccess(ctx, a, &OpenAIForwardResult{Model: "gpt-test", UpstreamEndpoint: "/v1/responses"}, true)
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM wayvia_health_evidence`).Scan(&n)
	if n != 0 {
		t.Fatal("stale success recorded")
	}
	cfg := wayviahealth.Defaults()
	cfg.AccountTypes = []string{"apikey"}
	cfg.PeriodicEnabled = true
	cfg, err := s.SaveSettings(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.newRun(context.Background(), 1, "periodic", "", "gpt-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(context.Background(), healthSessionKey{}, &healthSession{run: r, account: a, owner: a})
	cfg.PeriodicEnabled = false
	if _, err = s.SaveSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	s.HandleFailure(ctx, a, 401, http.Header{}, []byte(`{"error":{"message":"expired"}}`), "manual", "gpt-test")
	if a.Status != StatusActive || r.HTTPStatus != 401 || r.StateChanged {
		t.Fatal("closed policy mutated state", a.Status, r)
	}
	s.HandleFailure(ctx, a, 401, http.Header{}, []byte(`{"error":{"code":"token_revoked"}}`), "manual", "gpt-test")
	if a.Status != StatusError {
		t.Fatal("closed policy ignored explicit revocation")
	}
}

func TestWayviaHealthScheduleDedupAndBatchAtomicity(t *testing.T) {
	s, _ := healthFixture(t)
	ctx := context.Background()
	r, err := s.newRun(ctx, 1, "scheduled", "", "gpt-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Options.ScheduleKey = "plan-1:cycle-1"
	r, err = s.insertRun(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	r.State = "success"
	if err = s.finishRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.ID = newHealthID()
	r.State = "queued"
	if _, err = s.insertRun(ctx, r); !healthRunBusy(err) {
		t.Fatal("same cycle repeated", err)
	}
	_, err = s.db.Exec(`CREATE FUNCTION fail_second_batch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.account_id=2 AND NEW.source='batch' THEN RAISE EXCEPTION 'test-only failure'; END IF; RETURN NEW; END $$;CREATE TRIGGER fail_second_batch BEFORE INSERT ON wayvia_account_test_runs FOR EACH ROW EXECUTE FUNCTION fail_second_batch()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Batch(ctx, WayviaHealthSelection{AccountIDs: []int64{1, 2}}, "gpt-test", nil); err == nil {
		t.Fatal("expected batch failure")
	}
	var n int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM wayvia_account_test_runs WHERE source='batch'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("partial batch left behind", n, err)
	}
}

func TestWayviaHealthRecheckBoundaries(t *testing.T) {
	s, repo := healthFixture(t)
	ctx := context.Background()
	cfg := wayviahealth.Defaults()
	cfg.ErrorRecheck = true
	cfg.AccountTypes = []string{"apikey"}
	cfg, err := s.SaveSettings(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := repo.accounts[1]
	s.ObserveFailure(s.requestContext(ctx, a, a, "websocket"), a, 503, nil, nil, "gateway", "gpt-test")
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM wayvia_account_test_runs WHERE source='error_recheck'`).Scan(&n)
	if n != 0 {
		t.Fatal("WS failure scheduled HTTP probe")
	}
	s.ObserveFailure(s.requestContext(ctx, a, a, healthProtocol(a)), a, 503, nil, nil, "gateway", "gpt-test")
	s.db.QueryRow(`SELECT COUNT(*) FROM wayvia_account_test_runs WHERE source='error_recheck'`).Scan(&n)
	if n != 1 {
		t.Fatal("eligible failure did not schedule one recheck", n)
	}
	s.ObserveFailure(ctx, a, 503, nil, nil, "gateway", "gpt-test")
	s.db.QueryRow(`SELECT COUNT(*) FROM wayvia_account_test_runs WHERE source='error_recheck'`).Scan(&n)
	if n != 1 {
		t.Fatal("cooldown did not merge failures", n)
	}
}

func TestWayviaHealthOwnedRecoveryScope(t *testing.T) {
	s, repo := healthFixture(t)
	ctx := context.Background()
	a := repo.accounts[1]
	blocked, _ := s.newRun(ctx, 1, "manual", "", "gpt-test", nil)
	blocked.State = "failed"
	blocked, _ = s.insertRun(ctx, blocked)
	reason := "wayvia_health:" + blocked.ID
	a.TempUnschedulableReason = reason
	_, err := s.db.Exec(`UPDATE accounts SET temp_unschedulable_reason=$1,temp_unschedulable_until=NOW()+INTERVAL '10 minutes',rate_limit_reset_at=NOW()+INTERVAL '1 hour' WHERE id=1`, reason)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	passed := *blocked
	passed.StartedAt = &now
	passed.Model = "other-model"
	s.recoverOwnedState(ctx, &passed, a, a)
	var saved string
	s.db.QueryRow(`SELECT temp_unschedulable_reason FROM accounts WHERE id=1`).Scan(&saved)
	if saved != reason {
		t.Fatal("other model cleared marker")
	}
	passed.Model = blocked.Model
	a.Schedulable = false
	s.recoverOwnedState(ctx, &passed, a, a)
	s.db.QueryRow(`SELECT temp_unschedulable_reason FROM accounts WHERE id=1`).Scan(&saved)
	if saved != reason {
		t.Fatal("manual pause bypassed")
	}
	a.Schedulable = true
	s.recoverOwnedState(ctx, &passed, a, a)
	var cleared, quotaKept bool
	s.db.QueryRow(`SELECT temp_unschedulable_reason IS NULL,rate_limit_reset_at>NOW() FROM accounts WHERE id=1`).Scan(&cleared, &quotaKept)
	if !cleared || !quotaKept {
		t.Fatal("recovery touched wrong scope", cleared, quotaKept)
	}
}

func TestWayviaHealthBusinessTerminalEvidence(t *testing.T) {
	for _, tc := range []struct {
		body, event string
		want        bool
	}{
		{`{"type":"response.completed","response":{"status":"completed"}}`, "response.completed", true},
		{`{"type":"response.incomplete","response":{"status":"incomplete"}}`, "response.incomplete", false},
		{`{"type":"response.completed","response":{"status":"failed"}}`, "response.completed", false},
		{`{"choices":[{"index":0,"finish_reason":"stop"}]}`, "", true},
		{`{"choices":[{"index":0,"finish_reason":"length"}]}`, "", false},
		{`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`, "", false},
	} {
		observer := &upstreamResponseModelObserver{}
		observer.ObserveOpenAI([]byte(tc.body), tc.event)
		e := observer.healthTerminal
		if got := !e.failed && (e.responsesCompleted || e.chatFinished); got != tc.want {
			t.Fatal(tc.body, got)
		}
	}
}

func TestWayviaHealthQuotaAuthenticationLink(t *testing.T) {
	for _, details := range []bool{false, true} {
		t.Run(fmt.Sprint("details=", details), func(t *testing.T) {
			s, repo := healthFixture(t)
			a := repo.accounts[1]
			a.Type = AccountTypeOAuth
			a.Credentials = map[string]any{"access_token": "fake-token", "refresh_token": "fake-rt", "chatgpt_account_id": "fake-org"}
			s.rateLimit = &RateLimitService{accountRepo: repo, cfg: &config.Config{}}
			cache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(a): "fake-token"}}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if details && r.URL.Path == "/backend-api/wham/usage" {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":{"code":"token_revoked","message":"fake-private-response"}}`))
			}))
			defer srv.Close()
			quota := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), newQuotaRedirectingFactory(srv), nil)
			quota.health = s
			_, err := quota.QueryUsage(context.Background(), 1)
			if (err != nil) == details || a.Status != StatusError {
				t.Fatal("quota auth failure not classified", a.Status, err)
			}
			wantCalls := 1
			if details {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatal("unexpected extra request", calls)
			}
			rows, err := s.Runs(context.Background(), 1, "", "", 100, nil)
			if err != nil || len(rows) != 1 {
				t.Fatal(rows, err)
			}
			wantSource := "quota"
			if details {
				wantSource = "quota_details"
			}
			if rows[0].Source != wantSource || rows[0].ErrorClass != "auth_revoked" {
				t.Fatal(rows[0])
			}
		})
	}
}

func TestWayviaHealthCompositeHistoryCursor(t *testing.T) {
	s, _ := healthFixture(t)
	ctx := context.Background()
	batch, err := s.Batch(ctx, WayviaHealthSelection{AccountIDs: []int64{1, 2, 3}}, "gpt-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Runs(ctx, 0, batch.BatchID, "", 2, nil)
	if err != nil || len(first) != 2 {
		t.Fatal(first, err)
	}
	last := first[1]
	next, err := s.Runs(ctx, 0, batch.BatchID, "", 2, &last.CreatedAt, last.ID)
	if err != nil || len(next) != 1 || next[0].ID == last.ID {
		t.Fatal("same timestamp skipped rows", next, err)
	}
}

func TestWayviaHealthQueuedAccountTypeChangeCancels(t *testing.T) {
	s, repo := healthFixture(t)
	ctx := context.Background()
	cfg := wayviahealth.Defaults()
	r, _ := s.newRun(ctx, 1, "batch", "", "gpt-test", nil)
	r, err := s.insertRun(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.claim(ctx, r.ID, 1, 45)
	if err != nil {
		t.Fatal(err)
	}
	repo.accounts[1].Platform = PlatformAnthropic
	s.executeQueued(ctx, r, cfg)
	saved, err := s.GetRun(ctx, r.ID)
	if err != nil || saved.State != "cancelled" {
		t.Fatal("queued task fell into another adapter", saved, err)
	}
}

func TestWayviaHealthStreamAuthenticationError(t *testing.T) {
	s, repo := healthFixture(t)
	a := repo.accounts[1]
	ctx := context.Background()
	s.rateLimit = &RateLimitService{accountRepo: repo, cfg: &config.Config{}}
	r, _ := s.newRun(ctx, 1, "manual", "", "gpt-test", nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/", nil).WithContext(context.WithValue(ctx, healthSessionKey{}, &healthSession{run: r, account: a, owner: a}))
	err := s.tester.processOpenAIStream(c, strings.NewReader(`data: {"type":"response.failed","response":{"error":{"code":"token_revoked","message":"private-response"}}}`+"\n\n"))
	if err == nil || a.Status != StatusError || r.ErrorClass != "auth_revoked" || r.HTTPStatus != 200 || strings.Contains(w.Body.String(), "private-response") {
		t.Fatal("stream error not safely classified", a.Status, r, err)
	}
}

func TestWayviaHealthQueuedBatchUsesLatestInheritedModel(t *testing.T) {
	s, repo := healthFixture(t)
	ctx := context.Background()
	first, second := "model-before", "model-after"
	cfg := wayviahealth.Defaults()
	cfg.Platforms = []wayviahealth.PlatformPolicy{{Platform: "openai", AccountType: "apikey", Override: wayviahealth.Override{Model: &first}}}
	cfg, err := s.SaveSettings(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.Batch(ctx, WayviaHealthSelection{AccountIDs: []int64{1}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Platforms[0].Model = &second
	cfg, err = s.SaveSettings(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.claim(ctx, batch.Runs[0].ID, 1, 45)
	if err != nil {
		t.Fatal(err)
	}
	_ = repo
	up := &healthFakeUpstream{content: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"}
	s.tester.httpUpstream = up
	s.executeQueued(ctx, r, cfg)
	saved, err := s.GetRun(ctx, r.ID)
	if err != nil || saved.Model != second || saved.ConfigVersion != cfg.Version || !strings.Contains(up.body, second) {
		t.Fatal("batch kept stale default model", saved, up.body, err)
	}
}

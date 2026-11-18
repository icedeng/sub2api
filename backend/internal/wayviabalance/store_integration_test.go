package wayviabalance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 只接受显式测试 DSN；每次使用独立 schema，绝不读取应用或环境的通用数据库配置。
func postgresBalanceStore(t *testing.T) (*store, *service.Account) {
	t.Helper()
	dsn := os.Getenv("WAYVIA_BALANCE_TEST_DSN")
	if dsn == "" {
		t.Skip("set WAYVIA_BALANCE_TEST_DSN to run the isolated PostgreSQL integration test")
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, admin.PingContext(ctx))
	schema := "wayvia_balance_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, cleanupErr := admin.ExecContext(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`)
		require.NoError(t, cleanupErr)
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, parseErr := url.Parse(dsn)
		require.NoError(t, parseErr)
		query := u.Query()
		query.Set("search_path", schema)
		u.RawQuery = query.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `
		CREATE TABLE proxies (
			id bigint PRIMARY KEY, protocol text, host text, port integer,
			username text, password text, status text, expires_at timestamptz,
			fallback_mode text, backup_proxy_id bigint, deleted_at timestamptz
		);
		CREATE TABLE accounts (
			id bigint PRIMARY KEY, type text, platform text, credentials jsonb,
			proxy_id bigint REFERENCES proxies(id), status text,
			parent_account_id bigint, deleted_at timestamptz
		);
		CREATE TABLE account_groups (account_id bigint, group_id bigint);
		INSERT INTO proxies (id, protocol, host, port, password, status)
		VALUES (4, 'http', 'proxy.test', 8080, 'current-secret', 'active');
	`)
	require.NoError(t, err)
	a := balanceAccount()
	credentials, err := json.Marshal(a.Credentials)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO accounts (id,type,platform,credentials,status) VALUES ($1,$2,$3,$4::jsonb,$5)`,
		a.ID, a.Type, a.Platform, string(credentials), a.Status)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO accounts (id,type,platform,credentials,status,proxy_id)
		VALUES (19,'apikey','openai',$1::jsonb,'active',4)`, string(credentials))
	require.NoError(t, err)
	// 任何意外账号表写入都会直接使待测存储方法失败。
	_, err = db.ExecContext(ctx, `
		CREATE FUNCTION reject_account_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'balance module must not write accounts'; END;
		$$;
		CREATE TRIGGER balance_account_write_guard BEFORE INSERT OR UPDATE OR DELETE ON accounts
		FOR EACH ROW EXECUTE FUNCTION reject_account_write();
	`)
	require.NoError(t, err)
	return newStore(db), a
}

func TestStorePostgresIntegration(t *testing.T) {
	s, a := postgresBalanceStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.Migrate(ctx))
	var count int
	var checksum string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*), min(checksum) FROM wayvia_balance_schema_migrations`).Scan(&count, &checksum))
	require.Equal(t, 1, count)
	migration, err := balanceMigrations.ReadFile("migrations/001_balance.sql")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(migration)), checksum)

	cfg := defaultConfig()
	cfg.Enabled = true
	cfg.LowBalanceThreshold = "13"
	r, err := s.SaveConfig(ctx, a, cfg, 0, "identity", "encrypted-secret", "identity", true)
	require.NoError(t, err)
	require.Equal(t, int64(1), r.Revision)
	_, err = s.SaveConfig(ctx, a, cfg, 0, "identity", "", "", true)
	require.ErrorIs(t, err, ErrConfigChanged)

	firstAttempt := time.Now().UTC().Truncate(time.Microsecond)
	next := firstAttempt.Add(5 * time.Minute)
	first := &record{LastAttemptAt: &firstAttempt, NextRetryAt: &next,
		Snapshot: &Snapshot{Provider: "sub2api", Wallet: &Metric{Amount: "12.34", Unit: "USD"},
			ReceivedAt: firstAttempt, FreshUntil: next},
		DetectedProvider: "sub2api", DetectedPath: "/v1/usage"}
	require.NoError(t, s.SaveResult(ctx, a, 1, "identity", first))

	groupID := int64(9)
	_, err = s.db.ExecContext(ctx, `INSERT INTO account_groups VALUES ($1, $2)`, a.ID, groupID)
	require.NoError(t, err)
	wallets, err := s.AlertWallets(ctx, a.Platform, &groupID)
	require.NoError(t, err)
	require.Len(t, wallets, 1)
	wallet := wallets[0]
	wallet.identity = accountIdentity(a)
	low, known := wallet.belowThreshold(a, firstAttempt)
	require.True(t, known)
	require.True(t, low)
	_, known = wallet.belowThreshold(a, next)
	require.False(t, known)
	wallet.errorCode = "request_failed"
	_, known = wallet.belowThreshold(a, firstAttempt)
	require.False(t, known)
	otherGroup := groupID + 1
	wallets, err = s.AlertWallets(ctx, a.Platform, &otherGroup)
	require.NoError(t, err)
	require.Empty(t, wallets)

	cfg.IntervalMinutes = 10
	r, err = s.SaveConfig(ctx, a, cfg, 1, "identity", "encrypted-secret", "identity", false)
	require.NoError(t, err)
	require.Equal(t, int64(2), r.Revision)
	require.Equal(t, "12.34", r.Snapshot.Wallet.Amount)
	require.True(t, firstAttempt.Equal(*r.LastAttemptAt))
	_, err = s.SaveConfig(ctx, a, cfg, 1, "identity", "", "", true)
	require.ErrorIs(t, err, ErrConfigChanged)
	require.ErrorIs(t, s.SaveResult(ctx, a, 1, "identity", first), ErrConfigChanged)

	// 两个查询读取同一配置和 LastAttemptAt；较新查询先落库，旧租约查询不得覆盖它。
	newAttempt := firstAttempt.Add(time.Minute)
	newer := *first
	newer.PreviousAttemptAt = r.LastAttemptAt
	newer.LastAttemptAt = &newAttempt
	newer.Snapshot = &Snapshot{Provider: "sub2api", Wallet: &Metric{Amount: "15.67", Unit: "USD"}}
	require.NoError(t, s.SaveResult(ctx, a, 2, "identity", &newer))
	oldAttempt := firstAttempt.Add(30 * time.Second)
	late := *first
	late.PreviousAttemptAt = r.LastAttemptAt
	late.LastAttemptAt = &oldAttempt
	late.Snapshot = &Snapshot{Wallet: &Metric{Amount: "999", Unit: "USD"}}
	require.ErrorIs(t, s.SaveResult(ctx, a, 2, "identity", &late), ErrConfigChanged)
	r, err = s.Load(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "15.67", r.Snapshot.Wallet.Amount)
	require.True(t, newAttempt.Equal(*r.LastAttemptAt))

	cfg.Enabled = false
	r, err = s.SaveConfig(ctx, a, cfg, 2, "identity", "encrypted-secret", "identity", false)
	require.NoError(t, err)
	late.PreviousAttemptAt = r.LastAttemptAt
	require.ErrorIs(t, s.SaveResult(ctx, a, 2, "identity", &late), ErrConfigChanged)
	require.ErrorIs(t, s.SaveResult(ctx, a, 3, "identity", &late), ErrConfigChanged)

	cfg.Enabled = true
	r, err = s.SaveConfig(ctx, a, cfg, 3, "identity", "encrypted-secret", "identity", false)
	require.NoError(t, err)
	require.ErrorIs(t, s.SaveResult(ctx, a, 4, "other-identity", &late), ErrConfigChanged)
	staleAccount := *a
	staleAccount.Credentials = map[string]any{"api_key": "old-key", "base_url": "https://example.test"}
	require.ErrorIs(t, s.SaveResult(ctx, &staleAccount, 4, "identity", &late), ErrConfigChanged)
	proxyID := int64(4)
	staleProxyAccount := *a
	staleProxyAccount.ID = 19
	staleProxyAccount.ProxyID = &proxyID
	staleProxyAccount.Proxy = &service.Proxy{ID: 4, Protocol: "http", Host: "proxy.test", Port: 8080,
		Password: "old-secret", Status: service.StatusActive}
	_, err = s.SaveConfig(ctx, &staleProxyAccount, cfg, 0, "proxy-identity", "", "", false)
	require.ErrorIs(t, err, ErrConfigChanged)

	r, err = s.SaveConfig(ctx, a, cfg, 4, "new-identity", "", "", true)
	require.NoError(t, err)
	require.Nil(t, r.Snapshot)
	require.Nil(t, r.LastAttemptAt)
	require.Nil(t, r.NextRetryAt)
	require.Empty(t, r.QueryCiphertext)
	require.Empty(t, r.DetectedPath)
	ids, err := s.Due(ctx, time.Now().UTC(), 100)
	require.NoError(t, err)
	require.Equal(t, []int64{a.ID}, ids)

	// 校验只篡改私有测试 schema 内的模块账本，不触碰宿主迁移。
	_, err = s.db.ExecContext(ctx, `UPDATE wayvia_balance_schema_migrations SET checksum = 'changed' WHERE filename = '001_balance.sql'`)
	require.NoError(t, err)
	require.ErrorContains(t, s.Migrate(ctx), "checksum mismatch")
	var persistedCredentials string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT credentials ->> 'api_key' FROM accounts WHERE id = $1`, a.ID).Scan(&persistedCredentials))
	require.Equal(t, "test-key", persistedCredentials)
}

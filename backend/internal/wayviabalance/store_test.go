package wayviabalance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func mockBalanceStore(t *testing.T) (*store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	})
	return newStore(db), mock
}

func balanceAccount() *service.Account {
	return &service.Account{ID: 17, Type: service.AccountTypeAPIKey, Platform: service.PlatformOpenAI,
		Status: service.StatusActive, Credentials: map[string]any{"api_key": "test-key", "base_url": "https://example.test"}}
}

func expectAccountIdentity(mock sqlmock.Sqlmock, a *service.Account, matches bool) {
	encoded, _ := json.Marshal(a.Credentials)
	rows := sqlmock.NewRows([]string{"id"})
	if matches {
		rows.AddRow(a.ID)
	}
	mock.ExpectQuery(`(?s)SELECT id FROM accounts.*credentials.*proxy_id.*status.*parent_account_id.*FOR SHARE`).
		WithArgs(a.ID, a.Type, a.Platform, string(encoded), a.ProxyID, a.Status, a.ParentAccountID).
		WillReturnRows(rows)
}

func balanceRecordRows(cfg Config, revision int64, snapshot string) *sqlmock.Rows {
	encoded, _ := json.Marshal(cfg)
	var snapshotValue any
	if snapshot != "" {
		snapshotValue = []byte(snapshot)
	}
	return sqlmock.NewRows(strings.Fields(strings.ReplaceAll(recordColumns, ",", ""))).
		AddRow(encoded, revision, "identity", "encrypted-secret", "secret-identity", snapshotValue,
			nil, nil, "", 0, "sub2api", "/v1/usage")
}

func TestStoreLoadWithoutConfigurationIsDisabled(t *testing.T) {
	s, mock := mockBalanceStore(t)
	mock.ExpectQuery(`SELECT .* FROM wayvia_upstream_balances WHERE account_id = \$1`).
		WithArgs(int64(17)).WillReturnError(sql.ErrNoRows)
	r, err := s.Load(context.Background(), 17)
	require.NoError(t, err)
	require.Zero(t, r.Revision)
	require.False(t, r.Config.Enabled)
	require.Equal(t, defaultConfig(), r.Config)
	require.Nil(t, r.Snapshot)
}

func TestStoreSaveConfigPreservesLatestDatabaseSnapshot(t *testing.T) {
	s, mock := mockBalanceStore(t)
	a := balanceAccount()
	cfg := defaultConfig()
	cfg.Enabled = true
	encoded, _ := json.Marshal(cfg)
	mock.ExpectBegin()
	expectAccountIdentity(mock, a, true)
	mock.ExpectQuery(`(?s)UPDATE wayvia_upstream_balances SET.*snapshot = CASE WHEN \$7 THEN NULL ELSE snapshot END.*WHERE account_id = \$1 AND revision = \$6 RETURNING`).
		WithArgs(a.ID, string(encoded), "identity", "encrypted-secret", "secret-identity", int64(1), false).
		WillReturnRows(balanceRecordRows(cfg, 2, `{"provider":"sub2api","wallet":{"amount":"20.03","unit":"USD"}}`))
	mock.ExpectCommit()
	r, err := s.SaveConfig(context.Background(), a, cfg, 1, "identity", "encrypted-secret", "secret-identity", false)
	require.NoError(t, err)
	require.Equal(t, int64(2), r.Revision)
	require.Equal(t, "20.03", r.Snapshot.Wallet.Amount)
}

func TestStoreSaveConfigRejectsStaleRevision(t *testing.T) {
	s, mock := mockBalanceStore(t)
	a := balanceAccount()
	mock.ExpectBegin()
	expectAccountIdentity(mock, a, true)
	mock.ExpectQuery(`(?s)UPDATE wayvia_upstream_balances SET.*WHERE account_id = \$1 AND revision = \$6 RETURNING`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	_, err := s.SaveConfig(context.Background(), a, defaultConfig(), 1, "identity", "", "", true)
	require.ErrorIs(t, err, ErrConfigChanged)
}

func TestStoreFirstConfigurationDoesNotOverwriteConcurrentCreation(t *testing.T) {
	s, mock := mockBalanceStore(t)
	a := balanceAccount()
	mock.ExpectBegin()
	expectAccountIdentity(mock, a, true)
	mock.ExpectQuery(`(?s)INSERT INTO wayvia_upstream_balances.*ON CONFLICT \(account_id\) DO NOTHING RETURNING`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	_, err := s.SaveConfig(context.Background(), a, defaultConfig(), 0, "identity", "", "", true)
	require.ErrorIs(t, err, ErrConfigChanged)
}

func TestStoreResultRequiresEnabledAndMatchingRevision(t *testing.T) {
	for _, affected := range []int64{0, 1} {
		t.Run(fmt.Sprintf("affected_%d", affected), func(t *testing.T) {
			s, mock := mockBalanceStore(t)
			a := balanceAccount()
			attempt := time.Now().UTC()
			next := attempt.Add(5 * time.Minute)
			result := &record{Snapshot: &Snapshot{Provider: "sub2api", Wallet: &Metric{Amount: "0", Unit: "USD"}},
				LastAttemptAt: &attempt, NextRetryAt: &next, DetectedProvider: "sub2api", DetectedPath: "/v1/usage"}
			encoded, err := json.Marshal(result.Snapshot)
			require.NoError(t, err)
			mock.ExpectBegin()
			expectAccountIdentity(mock, a, true)
			mock.ExpectExec(`(?s)UPDATE wayvia_upstream_balances SET.*WHERE account_id = \$1 AND revision = \$2 AND identity = \$3.*config -> 'enabled' = 'true'::jsonb.*last_attempt_at IS NOT DISTINCT FROM \$11`).
				WithArgs(a.ID, int64(2), "identity", string(encoded), attempt, next, "", 0, "sub2api", "/v1/usage", nil).
				WillReturnResult(sqlmock.NewResult(0, affected))
			if affected == 1 {
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			err = s.SaveResult(context.Background(), a, 2, "identity", result)
			if affected == 1 {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrConfigChanged)
			}
		})
	}
}

func TestStoreRejectsChangedAccountIdentityBeforeSnapshotWrite(t *testing.T) {
	s, mock := mockBalanceStore(t)
	a := balanceAccount()
	mock.ExpectBegin()
	expectAccountIdentity(mock, a, false)
	mock.ExpectRollback()
	err := s.SaveResult(context.Background(), a, 2, "identity", &record{})
	require.ErrorIs(t, err, ErrConfigChanged)
}

func TestStoreRejectsChangedOrMissingProxyBeforeSnapshotWrite(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_%t", missing), func(t *testing.T) {
			s, mock := mockBalanceStore(t)
			a := balanceAccount()
			id := int64(4)
			a.ProxyID = &id
			mock.ExpectBegin()
			if !missing {
				a.Proxy = &service.Proxy{ID: id, Protocol: "http", Host: "proxy.test", Port: 8080,
					Username: "user", Password: "secret", Status: service.StatusActive}
				mock.ExpectQuery(`(?s)SELECT id FROM proxies.*expires_at.*fallback_mode.*backup_proxy_id.*FOR SHARE`).
					WithArgs(id, "http", "proxy.test", 8080, "user", "secret", service.StatusActive, nil, service.FallbackModeNone, nil).
					WillReturnError(sql.ErrNoRows)
			}
			mock.ExpectRollback()
			err := s.SaveResult(context.Background(), a, 2, "identity", &record{})
			require.ErrorIs(t, err, ErrConfigChanged)
		})
	}
}

func TestStoreDueIsBoundedAndOnlyReturnsEligibleAutoRefreshAccounts(t *testing.T) {
	s, mock := mockBalanceStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`(?s)SELECT b.account_id.*config -> 'enabled' = 'true'::jsonb.*config -> 'auto_refresh' = 'true'::jsonb.*a.deleted_at IS NULL AND a.type = 'apikey' AND a.status = 'active'.*ORDER BY b.next_retry_at NULLS FIRST, b.account_id LIMIT \$2`).
		WithArgs(now, 100).WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(17).AddRow(25))
	ids, err := s.Due(context.Background(), now, 500)
	require.NoError(t, err)
	require.Equal(t, []int64{17, 25}, ids)
}

func TestBalanceMigrationAppliesOnceAndChecksChecksum(t *testing.T) {
	body, err := balanceMigrations.ReadFile("migrations/001_balance.sql")
	require.NoError(t, err)
	checksum := fmt.Sprintf("%x", sha256.Sum256(body))
	for _, mode := range []string{"new", "existing", "changed"} {
		t.Run(mode, func(t *testing.T) {
			s, mock := mockBalanceStore(t)
			mock.ExpectBegin()
			mock.ExpectExec(regexp.QuoteMeta(`SELECT pg_advisory_xact_lock(829174113508)`)).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS wayvia_balance_schema_migrations`).WillReturnResult(sqlmock.NewResult(0, 0))
			q := mock.ExpectQuery(`SELECT checksum FROM wayvia_balance_schema_migrations WHERE filename = \$1`).WithArgs("001_balance.sql")
			switch mode {
			case "new":
				q.WillReturnError(sql.ErrNoRows)
				mock.ExpectExec(regexp.QuoteMeta(string(body))).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec(`INSERT INTO wayvia_balance_schema_migrations`).WithArgs("001_balance.sql", checksum).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			case "existing":
				q.WillReturnRows(sqlmock.NewRows([]string{"checksum"}).AddRow(checksum))
				mock.ExpectCommit()
			case "changed":
				q.WillReturnRows(sqlmock.NewRows([]string{"checksum"}).AddRow("old-checksum"))
				mock.ExpectRollback()
			}
			err := s.Migrate(context.Background())
			if mode == "changed" {
				require.ErrorContains(t, err, "checksum mismatch")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

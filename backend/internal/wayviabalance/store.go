package wayviabalance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type store struct{ db *sql.DB }

func newStore(db *sql.DB) *store { return &store{db: db} }

const recordColumns = `config, revision, identity, query_ciphertext, query_identity,
	snapshot, last_attempt_at, next_retry_at, error_code, failure_count, detected_provider, detected_path`

type rowScanner interface{ Scan(...any) error }

func scanRecord(row rowScanner) (*record, error) {
	var r record
	var configJSON, snapshotJSON []byte
	if err := row.Scan(&configJSON, &r.Revision, &r.Identity, &r.QueryCiphertext, &r.QueryIdentity,
		&snapshotJSON, &r.LastAttemptAt, &r.NextRetryAt, &r.ErrorCode, &r.FailureCount,
		&r.DetectedProvider, &r.DetectedPath); err != nil {
		return nil, err
	}
	r.Config = defaultConfig()
	if err := json.Unmarshal(configJSON, &r.Config); err != nil {
		return nil, err
	}
	if len(snapshotJSON) != 0 {
		if err := json.Unmarshal(snapshotJSON, &r.Snapshot); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

func (s *store) Load(ctx context.Context, id int64) (*record, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	r, err := scanRecord(s.db.QueryRowContext(ctx,
		`SELECT `+recordColumns+` FROM wayvia_upstream_balances WHERE account_id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return &record{Config: defaultConfig()}, nil
	}
	return r, err
}

// SaveConfig 只修改配置拥有的字段；不重写客户端读取后已经更新的余额快照。
func (s *store) SaveConfig(ctx context.Context, a *service.Account, cfg Config, expectedRevision int64,
	identity, ciphertext, secretIdentity string, reset bool,
) (*record, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	if expectedRevision < 0 {
		return nil, ErrConfigChanged
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockAccountIdentity(ctx, tx, a); err != nil {
		return nil, err
	}
	var row *sql.Row
	if expectedRevision == 0 {
		row = tx.QueryRowContext(ctx, `INSERT INTO wayvia_upstream_balances
			(account_id, config, revision, identity, query_ciphertext, query_identity)
			VALUES ($1, $2::jsonb, 1, $3, $4, $5)
			ON CONFLICT (account_id) DO NOTHING RETURNING `+recordColumns,
			a.ID, string(configJSON), identity, ciphertext, secretIdentity)
	} else {
		row = tx.QueryRowContext(ctx, `UPDATE wayvia_upstream_balances SET
			config = $2::jsonb, revision = revision + 1, identity = $3,
			query_ciphertext = $4, query_identity = $5,
			snapshot = CASE WHEN $7 THEN NULL ELSE snapshot END,
			last_attempt_at = CASE WHEN $7 THEN NULL ELSE last_attempt_at END,
			next_retry_at = CASE WHEN $7 THEN NULL ELSE next_retry_at END,
			error_code = CASE WHEN $7 THEN '' ELSE error_code END,
			failure_count = CASE WHEN $7 THEN 0 ELSE failure_count END,
			detected_provider = CASE WHEN $7 THEN '' ELSE detected_provider END,
			detected_path = CASE WHEN $7 THEN '' ELSE detected_path END,
			updated_at = now()
			WHERE account_id = $1 AND revision = $6 RETURNING `+recordColumns,
			a.ID, string(configJSON), identity, ciphertext, secretIdentity, expectedRevision, reset)
	}
	r, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConfigChanged
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

// SaveResult 在同一事务内锁定网络身份，并校验配置版本和上次尝试，拒绝迟到响应。
// 此路径不更新 accounts、账号倍率、调度 outbox 或调度缓存。
func (s *store) SaveResult(ctx context.Context, a *service.Account, expectedRevision int64, identity string, result *record) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	if result == nil || expectedRevision <= 0 || result.FailureCount < 0 {
		return ErrConfigChanged
	}
	var snapshot any
	if result.Snapshot != nil {
		encoded, err := json.Marshal(result.Snapshot)
		if err != nil {
			return err
		}
		snapshot = string(encoded)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockAccountIdentity(ctx, tx, a); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE wayvia_upstream_balances SET
		snapshot = $4::jsonb, last_attempt_at = $5, next_retry_at = $6,
		error_code = $7, failure_count = $8, detected_provider = $9, detected_path = $10,
		updated_at = now()
		WHERE account_id = $1 AND revision = $2 AND identity = $3
		AND config -> 'enabled' = 'true'::jsonb
		AND last_attempt_at IS NOT DISTINCT FROM $11`,
		a.ID, expectedRevision, identity, snapshot, result.LastAttemptAt, result.NextRetryAt,
		result.ErrorCode, result.FailureCount, result.DetectedProvider, result.DetectedPath, result.PreviousAttemptAt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConfigChanged
	}
	return tx.Commit()
}

// lockAccountIdentity 先锁代理再锁账号，与代理更新路径保持一致；只读行锁不会触发调度。
func lockAccountIdentity(ctx context.Context, tx *sql.Tx, a *service.Account) error {
	if a == nil || a.ID <= 0 || a.Type != service.AccountTypeAPIKey {
		return ErrConfigChanged
	}
	if a.ProxyID != nil {
		p := a.Proxy
		if p == nil || p.ID != *a.ProxyID {
			return ErrConfigChanged
		}
		fallback := p.FallbackMode
		if fallback == "" {
			fallback = service.FallbackModeNone
		}
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM proxies WHERE id = $1 AND deleted_at IS NULL
			AND protocol = $2 AND host = $3 AND port = $4
			AND COALESCE(username, '') = $5 AND COALESCE(password, '') = $6 AND status = $7
			AND expires_at IS NOT DISTINCT FROM $8
			AND COALESCE(NULLIF(fallback_mode, ''), 'none') = $9
			AND backup_proxy_id IS NOT DISTINCT FROM $10 FOR SHARE`,
			p.ID, p.Protocol, p.Host, p.Port, p.Username, p.Password, p.Status,
			p.ExpiresAt, fallback, p.BackupProxyID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConfigChanged
		}
		if err != nil {
			return err
		}
	}
	credentials := a.Credentials
	if credentials == nil {
		credentials = map[string]any{}
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE id = $1 AND deleted_at IS NULL
		AND type = $2 AND platform = $3 AND COALESCE(credentials, '{}'::jsonb) = $4::jsonb
		AND proxy_id IS NOT DISTINCT FROM $5 AND status = $6
		AND parent_account_id IS NOT DISTINCT FROM $7 FOR SHARE`,
		a.ID, a.Type, a.Platform, string(encoded), a.ProxyID, a.Status, a.ParentAccountID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConfigChanged
	}
	return err
}

// Due 返回有界到期账号；未查询账号优先，已尝试失败的账号由 next_retry_at 退避。
func (s *store) Due(ctx context.Context, now time.Time, limit int) ([]int64, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	if limit < 1 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT b.account_id FROM wayvia_upstream_balances b
		JOIN accounts a ON a.id = b.account_id
		WHERE b.config -> 'enabled' = 'true'::jsonb AND b.config -> 'auto_refresh' = 'true'::jsonb
		AND (b.next_retry_at IS NULL OR b.next_retry_at <= $1)
		AND a.deleted_at IS NULL AND a.type = 'apikey' AND a.status = 'active'
		ORDER BY b.next_retry_at NULLS FIRST, b.account_id LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

package wayviabalance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
)

//go:embed migrations/*.sql
var balanceMigrations embed.FS

// Migrate 使用独立账本，避免占用上游迁移编号；调用方隔离本模块初始化失败。
func (s *store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// 固定模块锁防止多个副本同时执行首次迁移，提交或回滚时自动释放。
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(829174113508)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS wayvia_balance_schema_migrations (
		filename text PRIMARY KEY, checksum text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := balanceMigrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := balanceMigrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		checksum := fmt.Sprintf("%x", sha256.Sum256(body))
		var applied string
		err = tx.QueryRowContext(ctx,
			`SELECT checksum FROM wayvia_balance_schema_migrations WHERE filename = $1`, entry.Name()).Scan(&applied)
		if err == nil {
			if applied != checksum {
				return fmt.Errorf("balance migration checksum mismatch: %s", entry.Name())
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("balance migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO wayvia_balance_schema_migrations (filename, checksum) VALUES ($1, $2)`, entry.Name(), checksum); err != nil {
			return err
		}
	}
	return tx.Commit()
}

package wayviaegress

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(829174113507)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS wayvia_schema_migrations(filename text PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, e := range entries {
		body, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(body))
		var old string
		err = tx.QueryRowContext(ctx, `SELECT checksum FROM wayvia_schema_migrations WHERE filename=$1`, e.Name()).Scan(&old)
		if err == nil {
			if old != sum {
				return fmt.Errorf("wayvia migration checksum mismatch: %s", e.Name())
			}
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("wayvia migration %s: %w", e.Name(), err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO wayvia_schema_migrations(filename,checksum) VALUES($1,$2)`, e.Name(), sum); err != nil {
			return err
		}
	}
	return tx.Commit()
}

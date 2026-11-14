package wayviaegress

import (
	"context"
	"encoding/json"
	"time"
)

// SaveMetadata 只补全展示字段；CAS失败时丢弃补全，绝不覆盖并发刷新后的令牌。
func (m *Manager) SaveMetadata(id int64, expected, fields map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.mu.Lock()
	_, p, err := m.rootLocked(id)
	m.mu.Unlock()
	if err != nil {
		return expected, err
	}
	unlock, err := m.ownerLock(ctx, p.Owner)
	if err != nil {
		return expected, err
	}
	defer unlock()
	next := make(map[string]any, len(expected)+len(fields))
	for k, v := range expected {
		next[k] = v
	}
	for _, k := range []string{"email", "plan_type", "subscription_expires_at", "privacy_mode"} {
		if v, ok := fields[k].(string); ok && v != "" {
			next[k] = v
		}
	}
	old, err := json.Marshal(expected)
	if err != nil {
		return expected, err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return expected, err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return expected, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('wayvia.credential_write','controlled',true)`); err != nil {
		return expected, err
	}
	if err = execOne(ctx, tx, `UPDATE accounts SET credentials=$2::jsonb,updated_at=now() WHERE id=$1 AND deleted_at IS NULL AND credentials=$3::jsonb`, id, string(raw), string(old)); err != nil {
		return expected, err
	}
	if err = publishCredentialChange(ctx, tx, id); err != nil {
		return expected, err
	}
	if err = tx.Commit(); err != nil {
		return expected, err
	}
	if err = m.Reload(ctx); err != nil {
		return next, err
	}
	return next, nil
}

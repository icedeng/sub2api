package wayviaegress

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ReplaceImportTokens 在原母账号上替换导入的令牌。未找到时返回 0，交由原导入流程处理。
// identity 仅用来查找候选账号，实际写入要求新 AT 验签后的身份与原账号一致。
func (m *Manager) ReplaceImportTokens(ctx context.Context, identity string, credentials map[string]any, includeUnbound bool) (int64, error) {
	if identity == "" {
		return 0, nil
	}
	rows, err := m.db.QueryContext(ctx, `
        SELECT p.owner_id::text, COALESCE(a.id, 0), p.state = 'retired' OR a.deleted_at IS NOT NULL
        FROM wayvia_account_egress_policies p LEFT JOIN accounts a ON a.id = p.root_account_id
        WHERE p.identity_key = $1 OR (
            $2 AND p.identity_key IS NULL AND a.credentials->>'chatgpt_user_id' = $1
            AND a.deleted_at IS NULL AND a.parent_account_id IS NULL
            AND a.platform = 'openai' AND a.type = 'oauth'
            AND COALESCE(a.credentials->>'auth_mode', '') IN ('', 'oauth')
            AND COALESCE(a.credentials->>'openai_auth_mode', '') IN ('', 'oauth')
        ) LIMIT 2`, identity, includeUnbound)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var owner string
	var id int64
	var retired bool
	if !rows.Next() {
		return 0, rows.Err()
	}
	if err = rows.Scan(&owner, &id, &retired); err != nil {
		return 0, err
	}
	if rows.Next() {
		return 0, Conflict("该身份匹配多个母账号，请核对账号归属")
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	if retired || id == 0 {
		return 0, Conflict("该身份的原账号已退役，请先恢复原账号")
	}
	access, _ := credentials["access_token"].(string)
	token, err := VerifyAccessToken(ctx, access)
	if err != nil {
		return 0, err
	}
	if token.UserInfo.ChatGPTUserID != identity {
		return 0, Conflict("新 Access Token 的身份与原账号不一致")
	}
	patch := map[string]any{"access_token": access, "expires_at": token.ExpiresAt.UTC().Format(time.RFC3339)}
	for _, key := range []string{"id_token", "refresh_token"} {
		if v, _ := credentials[key].(string); strings.TrimSpace(v) != "" && v != "__missing_refresh_token__" {
			patch[key] = v
		}
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return 0, err
	}
	Control.Lock()
	defer Control.Unlock()
	unlock, err := m.ownerLock(ctx, owner)
	if err != nil {
		return 0, err
	}
	defer unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// 与账号编辑、退役使用相同行锁顺序；拿锁后重新核对身份和工作区。
	var workspace string
	err = tx.QueryRowContext(ctx, `
        SELECT COALESCE(a.credentials->>'chatgpt_account_id', '')
        FROM accounts a JOIN wayvia_account_egress_policies p ON p.root_account_id = a.id
        WHERE a.id = $1 AND p.owner_id = $2 AND a.deleted_at IS NULL AND a.parent_account_id IS NULL
          AND a.platform = 'openai' AND a.type = 'oauth'
          AND COALESCE(a.credentials->>'auth_mode', '') IN ('', 'oauth')
          AND COALESCE(a.credentials->>'openai_auth_mode', '') IN ('', 'oauth')
          AND a.credentials->>'chatgpt_user_id' = $3 AND (p.identity_key = $3 OR ($4 AND p.identity_key IS NULL))
          AND p.state NOT IN ('preparing', 'pausing', 'retired')
        FOR UPDATE OF a, p`, id, owner, identity, includeUnbound).Scan(&workspace)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, Conflict("原账号身份或出口状态已变化，请重新导入")
	}
	if err != nil {
		return 0, err
	}
	if workspace != "" && workspace != token.UserInfo.ChatGPTAccountID {
		return 0, Conflict("新 Access Token 的工作区与原账号不一致")
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('wayvia.credential_write','controlled',true)`); err != nil {
		return 0, err
	}
	if err = execOne(ctx, tx, `
        UPDATE accounts SET credentials = credentials || $2::jsonb || jsonb_build_object(
            '_token_version', GREATEST($3::bigint, COALESCE((credentials->>'_token_version')::bigint, 0) + 1)
        ), updated_at = now() WHERE id = $1`, id, string(raw), time.Now().UnixMilli()); err != nil {
		return 0, err
	}
	if err = publishCredentialChange(ctx, tx, id); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.Reload(reloadCtx); err != nil {
		return id, Error("EGRESS_SNAPSHOT_RELOAD_FAILED", "凭据已保存，出口快照刷新失败")
	}
	return id, nil
}

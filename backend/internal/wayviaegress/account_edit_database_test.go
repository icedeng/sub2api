package wayviaegress

import (
	"context"
	"strings"
	"testing"
)

func TestStrictOAuthAccountEditSettings(t *testing.T) {
	m := fixtureDB(t)
	if _, err := m.db.Exec(`UPDATE wayvia_account_egress_policies SET
		identity_key='user-1',current_mode='strict_ipv6',target_mode='strict_ipv6',state='ready'
		WHERE root_account_id=1`); err != nil {
		t.Fatal(err)
	}
	// 与编辑表单一致：只调整配置，不持有或替换任何 OAuth 令牌。
	settings := `{"model_mapping":{"gpt-5":"gpt-5"},"compact_model_mapping":{"gpt-5":"gpt-5-mini"},"plan_type":"plus","intercept_warmup_requests":true,"account_scheduling_threshold":80,"temp_unschedulable_enabled":true,"temp_unschedulable_rules":[{"error_code":429,"keywords":["quota"],"duration_minutes":5,"scope":"model"}]}`
	// 在同一个隔离库恢复已发布的保护函数，先复现，再通过正式迁移入口升级。
	oldMigration, err := migrations.ReadFile("migrations/002_require_ipv6_for_new_accounts.sql")
	if err != nil {
		t.Fatal(err)
	}
	old := string(oldMigration)
	start := strings.Index(old, "CREATE OR REPLACE FUNCTION wayvia_account_policy_guard()")
	end := strings.Index(old, "CREATE FUNCTION wayvia_required_policy_guard()")
	if start < 0 || end <= start {
		t.Fatal("旧版保护函数未找到")
	}
	if _, err = m.db.Exec(old[start:end]); err != nil {
		t.Fatal(err)
	}
	if _, err = m.db.Exec(`DELETE FROM wayvia_schema_migrations WHERE filename='005_account_edit_settings.sql'`); err != nil {
		t.Fatal(err)
	}
	if _, err = m.db.Exec(`UPDATE accounts SET credentials=credentials || $1::jsonb WHERE id=1`, settings); err == nil || !strings.Contains(err.Error(), "EGRESS_CONTROLLED_CREDENTIAL_WRITE_REQUIRED") {
		t.Fatal("旧版未复现实际保存错误", err)
	}
	const snapshotSQL = `SELECT json_build_object('account',row_to_json(a),'policy',row_to_json(p))::text
		FROM accounts a JOIN wayvia_account_egress_policies p ON p.root_account_id=a.id WHERE a.id=1`
	var before, after string
	if err = m.db.QueryRow(snapshotSQL).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(context.Background(), m.db); err != nil {
		t.Fatal("增量迁移失败", err)
	}
	if err = m.db.QueryRow(snapshotSQL).Scan(&after); err != nil || before != after {
		t.Fatal("迁移不应修改账号或出口数据", err)
	}
	if _, err := m.db.Exec(`UPDATE accounts SET credentials=credentials || $1::jsonb WHERE id=1`, settings); err != nil {
		t.Fatal("普通配置编辑应成功，OAuth 令牌保持不变", err)
	}
	var unchanged bool
	if err := m.db.QueryRow(`SELECT credentials->>'access_token'='fake-old-at'
		AND credentials->>'refresh_token'='fake-old-rt' AND credentials->>'chatgpt_user_id'='user-1'
		AND credentials->>'plan_type'='plus' FROM accounts WHERE id=1`).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("配置未保存或凭据被修改", err)
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "chatgpt_user_id", "chatgpt_account_id", "expires_at", "base_url", "unknown_future_auth_field"} {
		t.Run("拒绝更改_"+key, func(t *testing.T) {
			_, err := m.db.Exec(`UPDATE accounts SET credentials=credentials || jsonb_build_object($1::text,'changed') WHERE id=1`, key)
			if err == nil || !strings.Contains(err.Error(), "EGRESS_CONTROLLED_CREDENTIAL_WRITE_REQUIRED") {
				t.Fatalf("认证字段必须继续受控: %v", err)
			}
		})
	}
	for _, raw := range []string{`{}`, `[]`, `null`} {
		if _, err := m.db.Exec(`UPDATE accounts SET credentials=$1::jsonb WHERE id=1`, raw); err == nil {
			t.Fatal("不能通过清空凭据或非对象载荷绕过保护", raw)
		}
	}
	// 刷新已经落库后，旧表单不能用旧令牌覆盖新令牌，即使同时修改白名单配置。
	tx, err := m.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT set_config('wayvia.credential_write','controlled',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE accounts SET credentials=credentials || '{"access_token":"new-at","refresh_token":"new-rt"}' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = m.db.Exec(`UPDATE accounts SET credentials=credentials || '{"access_token":"fake-old-at","refresh_token":"fake-old-rt","plan_type":"pro"}' WHERE id=1`); err == nil {
		t.Fatal("旧表单覆盖了刷新后的令牌")
	}
	// 删除配置也必须允许，且不依赖受控刷新事务的临时开关。
	if _, err = m.db.Exec(`UPDATE accounts SET credentials=credentials - ARRAY[
		'model_mapping','compact_model_mapping','plan_type','intercept_warmup_requests',
		'account_scheduling_threshold','temp_unschedulable_enabled','temp_unschedulable_rules'] WHERE id=1`); err != nil {
		t.Fatal("关闭普通配置失败", err)
	}
	if err = m.db.QueryRow(`SELECT credentials='{"access_token":"new-at","refresh_token":"new-rt","chatgpt_user_id":"user-1"}'::jsonb FROM accounts WHERE id=1`).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("关闭配置后凭据不正确", err)
	}
}

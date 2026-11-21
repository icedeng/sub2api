package wayviaegress

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestReplaceImportTokensUsesOriginalOwner(t *testing.T) {
	m := fixtureDB(t)
	sign := accessTokenSigner(t)
	ctx := context.Background()
	_, err := m.db.Exec(`UPDATE accounts SET credentials=credentials||'{"chatgpt_account_id":"account-test","plan_type":"plus"}' WHERE id=1;
 UPDATE wayvia_account_egress_policies SET current_mode='strict_ipv6',target_mode='strict_ipv6',state='ready',identity_key='user-1' WHERE root_account_id=1`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`INSERT INTO accounts(id,platform,type,credentials) VALUES(3,'openai','oauth','{"chatgpt_user_id":"user-1"}')`); err == nil || !strings.Contains(err.Error(), "EGRESS_DUPLICATE_IDENTITY_USE_ORIGINAL_OWNER") {
		t.Fatal("duplicate guard", err)
	}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	access := sign("user-1", expires, nil)
	if id, err := m.ReplaceImportTokens(ctx, "user-1", map[string]any{"access_token": access, "refresh_token": "new-rt", "id_token": "new-id", "plan_type": "injected"}, true); id != 1 || err != nil {
		t.Fatal(id, err)
	}
	got := m.accounts[1].Credentials
	if got["access_token"] != access || got["refresh_token"] != "new-rt" || got["id_token"] != "new-id" || got["expires_at"] != expires.Format(time.RFC3339) || got["plan_type"] != "plus" || m.policies[1].State != "ready" {
		t.Fatal("credential replacement changed other account settings or missed tokens")
	}
	if _, err := m.ReplaceImportTokens(ctx, "user-1", map[string]any{"access_token": sign("user-2", expires, nil)}, true); err == nil {
		t.Fatal("accepted different user")
	}
	if id, err := m.ReplaceImportTokens(ctx, "user-1", map[string]any{"access_token": sign("user-1", expires.Add(time.Hour), nil)}, false); id != 1 || err != nil || m.accounts[1].Credentials["refresh_token"] != "new-rt" {
		t.Fatal("AT-only replacement", id, err)
	}
	if id, err := m.ReplaceImportTokens(ctx, "new-user", nil, true); id != 0 || err != nil {
		t.Fatal("new account", id, err)
	}
	var count int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=1 AND event_type='account_changed'`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

package wayviaegress

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func setTestAccessToken(t *testing.T, m *Manager, id int64, token string, expires time.Time) {
	t.Helper()
	fields, err := json.Marshal(map[string]any{"access_token": token, "expires_at": expires.Format(time.RFC3339), "id_token": "expired-or-invalid-id-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.db.Exec(`UPDATE accounts SET credentials=credentials||$2::jsonb WHERE id=$1`, id, string(fields)); err != nil {
		t.Fatal(err)
	}
}

func TestPendingEnrollmentRefreshUsesATAndKeepsBusinessClosed(t *testing.T) {
	for _, newerAlreadySaved := range []bool{false, true} {
		name := "expired"
		if newerAlreadySaved {
			name = "newer-already-saved"
		}
		t.Run(name, func(t *testing.T) {
			m := fixtureDB(t)
			newPendingAccount(t, m)
			sign := accessTokenSigner(t)
			past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
			setTestAccessToken(t, m, 10, sign("user-10", past, nil), past)
			if _, err := m.db.Exec(`UPDATE settings SET value='{"enabled":true,"initialized":true,"revision":1}' WHERE key='wayvia_ipv6_egress'`); err != nil {
				t.Fatal(err)
			}
			if err := m.Reload(context.Background()); err != nil {
				t.Fatal(err)
			}
			a, p := m.accounts[10], m.policies[10]
			newAT := sign("user-10", future, nil)
			if newerAlreadySaved {
				// 保持快照过期，模拟等待 owner 锁前已有另一次刷新提交。
				setTestAccessToken(t, m, 10, newAT, future)
			}
			calls := 0
			m.SetAccessTokenRefresh(func(ctx context.Context, account Account) (map[string]any, error) {
				calls++
				r, err := RouteForContext(ctx)
				if err != nil || !r.control || r.Strict() || account.Credentials["refresh_token"] != "rt10" {
					t.Fatal("enrollment refresh did not use scoped control route", err)
				}
				for _, purpose := range []string{"business", "query", "prewarm", "refresh"} {
					if _, err := m.Resolve(10, purpose); err == nil {
						t.Fatal("pending account opened during refresh", purpose)
					}
				}
				return map[string]any{"access_token": newAT, "refresh_token": "rotated-rt", "chatgpt_user_id": "user-10", "expires_at": future.Format(time.RFC3339)}, nil
			}, 3*time.Minute)
			Control.Lock()
			prepared, err := m.prepareAccountAccessToken(context.Background(), a, p)
			Control.Unlock()
			if err != nil || prepared.Credentials["access_token"] != newAT {
				t.Fatal("latest AT was not used", err)
			}
			wantCalls := 1
			if newerAlreadySaved {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatal("unexpected RT exchange count", calls)
			}
			if err := m.Reload(context.Background()); err != nil {
				t.Fatal(err)
			}
			if m.accounts[10].Schedulable || m.policies[10].State != "pending_enrollment" || m.policies[10].Owner != p.Owner {
				t.Fatal("refresh changed scheduling or owner")
			}
			if !newerAlreadySaved && m.accounts[10].Credentials["refresh_token"] != "rotated-rt" {
				t.Fatal("rotated RT not persisted")
			}
			if m.accounts[10].Credentials["id_token"] != "expired-or-invalid-id-token" {
				t.Fatal("legacy ID Token was unexpectedly required or rewritten")
			}
		})
	}
}

func TestRefreshIgnoresExpiredATAndIDTokenBeforeExchange(t *testing.T) {
	m := fixtureDB(t)
	sign := accessTokenSigner(t)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	setTestAccessToken(t, m, 1, sign("user-1", past, nil), past)
	m.config.Prefix = "2606:4700:abcd:1234::/64"
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := m.Refresh(context.Background(), 1, func(_ context.Context, a Account) (map[string]any, error) {
		called = true
		if a.Credentials["refresh_token"] != "fake-old-rt" {
			t.Fatal("latest RT not read")
		}
		return map[string]any{"access_token": sign("user-1", future, nil), "refresh_token": "rotated-rt", "chatgpt_user_id": "user-1", "expires_at": future.Format(time.RFC3339)}, nil
	})
	if err != nil || !called || m.accounts[1].Credentials["refresh_token"] != "rotated-rt" {
		t.Fatal("expired old token blocked refresh", err)
	}
}

func TestFirstRefreshRejectsDifferentAccountIdentity(t *testing.T) {
	m := fixtureDB(t)
	_, err := m.Refresh(context.Background(), 1, func(context.Context, Account) (map[string]any, error) {
		return map[string]any{"access_token": "new-at", "refresh_token": "new-rt", "chatgpt_user_id": "other-user"}, nil
	})
	if err == nil || m.policies[1].State != "credential_uncertain" || m.accounts[1].Credentials["refresh_token"] != "fake-old-rt" {
		t.Fatal("changed identity accepted without existing binding", err)
	}
}

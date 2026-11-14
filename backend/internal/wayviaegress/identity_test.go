package wayviaegress

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/golang-jwt/jwt/v5"
)

// 使用本地测试密钥，所有 AT 回归均不访问真实 JWKS 或 OAuth 服务。
func accessTokenSigner(t *testing.T) func(string, time.Time, jwt.MapClaims) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksCache.Lock()
	keys, expires, fetched := jwksCache.keys, jwksCache.expires, jwksCache.fetched
	jwksCache.keys = map[string]*rsa.PublicKey{"test-at": &key.PublicKey}
	jwksCache.expires, jwksCache.fetched = time.Now().Add(time.Hour), time.Now()
	jwksCache.Unlock()
	t.Cleanup(func() {
		jwksCache.Lock()
		jwksCache.keys, jwksCache.expires, jwksCache.fetched = keys, expires, fetched
		jwksCache.Unlock()
	})
	return func(user string, expiry time.Time, overrides jwt.MapClaims) string {
		claims := jwt.MapClaims{
			"iss": "https://auth.openai.com", "aud": []string{"https://api.openai.com/v1"}, "exp": expiry.Unix(),
			"https://api.openai.com/auth":    map[string]any{"chatgpt_user_id": user, "chatgpt_account_id": "account-test", "chatgpt_plan_type": "plus"},
			"https://api.openai.com/profile": map[string]any{"email": "fixture@example.invalid"},
		}
		for k, v := range overrides {
			if v == nil {
				delete(claims, k)
			} else {
				claims[k] = v
			}
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test-at"
		raw, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
}

func TestVerifyAccessTokenClaims(t *testing.T) {
	sign := accessTokenSigner(t)
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, tt := range []struct {
		name   string
		claims jwt.MapClaims
		code   string
	}{
		{"valid", nil, ""},
		{"expired", jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}, "EGRESS_ACCESS_TOKEN_EXPIRED"},
		{"wrong-issuer", jwt.MapClaims{"iss": "https://other.invalid"}, "EGRESS_ACCESS_TOKEN_INVALID"},
		{"id-token-audience", jwt.MapClaims{"aud": []string{"oauth-client-id"}}, "EGRESS_ACCESS_TOKEN_INVALID"},
		{"expired-wrong-audience", jwt.MapClaims{"aud": "oauth-client-id", "exp": time.Now().Add(-time.Hour).Unix()}, "EGRESS_ACCESS_TOKEN_INVALID"},
		{"missing-expiry", jwt.MapClaims{"exp": nil}, "EGRESS_ACCESS_TOKEN_INVALID"},
		{"not-yet-valid", jwt.MapClaims{"nbf": future.Unix()}, "EGRESS_ACCESS_TOKEN_INVALID"},
		{"missing-identity", jwt.MapClaims{"https://api.openai.com/auth": nil}, "EGRESS_IDENTITY_UNVERIFIED"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			verified, err := VerifyAccessToken(context.Background(), sign("user-1", future, tt.claims))
			if apperrors.Reason(err) != tt.code {
				t.Fatalf("unexpected validation result: %v", err)
			}
			if tt.code == "" && (verified.UserInfo.ChatGPTUserID != "user-1" || verified.UserInfo.Email != "fixture@example.invalid" || !verified.ExpiresAt.Equal(future)) {
				t.Fatal("AT metadata not extracted")
			}
		})
	}
	parts := strings.Split(sign("user-1", future, nil), ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sig[0] ^= 1
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)
	if _, err := VerifyAccessToken(context.Background(), strings.Join(parts, ".")); apperrors.Reason(err) != "EGRESS_ACCESS_TOKEN_INVALID" {
		t.Fatal("invalid signature accepted", err)
	}
	if _, err := VerifyAccessToken(context.Background(), ""); apperrors.Reason(err) != "EGRESS_ACCESS_TOKEN_MISSING" {
		t.Fatal(err)
	}
}

func TestSigningKeyNetworkFailureRemainsRetryable(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		code string
	}{
		{"connection", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, "EGRESS_JWKS_UNAVAILABLE"},
		{"timeout", context.DeadlineExceeded, "EGRESS_JWKS_UNAVAILABLE"},
		{"upstream-unavailable", errSigningKeysUnavailable, "EGRESS_JWKS_UNAVAILABLE"},
		{"unknown-key", errors.New("unknown signing key"), "EGRESS_ACCESS_TOKEN_INVALID"},
		{"invalid-signature", jwt.ErrTokenSignatureInvalid, "EGRESS_ACCESS_TOKEN_INVALID"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// jwt 包包装 keyfunc 错误后，仍须区分联网失败和凭据验证失败。
			_, err := accessTokenDetails(&accessTokenClaims{}, fmt.Errorf("%w: %w", jwt.ErrTokenUnverifiable, signingKeyFetchError(tt.err)))
			if apperrors.Reason(err) != tt.code || enrollmentRetryable(safeError(err)) != (tt.code == "EGRESS_JWKS_UNAVAILABLE") {
				t.Fatal("incorrect automatic retry classification", err)
			}
		})
	}
}

func TestEnrollmentUsesValidAccessTokenRegardlessOfIDToken(t *testing.T) {
	sign := accessTokenSigner(t)
	m := fixtureManager(t)
	a := m.accounts[1]
	a.Credentials["access_token"] = sign("user-1", time.Now().Add(time.Hour), nil)
	m.SetAccessTokenRefresh(func(context.Context, Account) (map[string]any, error) {
		t.Fatal("valid AT unnecessarily refreshed")
		return nil, nil
	}, 3*time.Minute)
	for _, oldID := range []any{nil, "", "expired-or-malformed-id-token"} {
		a.Credentials["id_token"] = oldID
		if _, err := m.prepareAccountAccessToken(context.Background(), a, m.policies[1]); err != nil {
			t.Fatal("ID Token blocked valid AT", err)
		}
	}
	a.Credentials["access_token"] = sign("other-user", time.Now().Add(time.Hour), nil)
	if _, err := m.prepareAccountAccessToken(context.Background(), a, m.policies[1]); err == nil {
		t.Fatal("another account's AT accepted")
	}
}

func TestAccessTokenRefreshWindowUsesCompatibleExpiryFormats(t *testing.T) {
	now := time.Now()
	token := &VerifiedAccessToken{ExpiresAt: now.Add(time.Hour)}
	soon := now.Add(time.Minute).Unix()
	for _, stored := range []any{time.Unix(soon, 0).Format(time.RFC3339), strconv.FormatInt(soon, 10), soon, int(soon), float64(soon), json.Number(strconv.FormatInt(soon, 10))} {
		a := Account{Credentials: map[string]any{"expires_at": stored}}
		if !accessTokenNeedsRefresh(a, token, 3*time.Minute) {
			t.Fatalf("refresh window missed for %T", stored)
		}
	}
	for _, stored := range []any{nil, "invalid", now.Add(2 * time.Hour).Format(time.RFC3339)} {
		a := Account{Credentials: map[string]any{"expires_at": stored, "id_token": "expired"}}
		if accessTokenNeedsRefresh(a, token, 3*time.Minute) {
			t.Fatal("valid AT incorrectly expired")
		}
	}
	if !accessTokenNeedsRefresh(Account{}, &VerifiedAccessToken{ExpiresAt: now.Add(time.Minute)}, 3*time.Minute) {
		t.Fatal("signed AT expiry ignored")
	}
}

func TestEnrollmentRefreshNeverFallsBackFromFixedRoute(t *testing.T) {
	m := fixtureManager(t)
	p := m.policies[1]
	p.Mode, p.Target, p.State, p.Identity, p.Source = Strict, Strict, "ready", "user-1", "2606:4700:abcd:1234::1"
	m.policies[1] = p
	m.ready[p.Owner] = true
	r, err := m.refreshRoute(1, true)
	if err != nil || !r.Strict() || r.Source.String() != p.Source {
		t.Fatal("fixed refresh route lost", err)
	}
	m.runtime.Enabled = false
	if _, err = m.refreshRoute(1, true); err == nil {
		t.Fatal("paused strict account fell back to legacy")
	}
}

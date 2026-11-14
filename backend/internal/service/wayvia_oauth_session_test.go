package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/wayviaegress"
	"github.com/golang-jwt/jwt/v5"
)

func TestWayviaTokenResponseDoesNotFallBackToIDToken(t *testing.T) {
	for _, idToken := range []string{"", "expired-or-invalid-id-token"} {
		_, err := openAITokenInfoFromResponse(&openai.TokenResponse{RefreshToken: "rt", IDToken: idToken, ExpiresIn: 3600}, "client", true)
		if apperrors.Reason(err) != "EGRESS_ACCESS_TOKEN_MISSING" {
			t.Fatal("ID Token affected AT validation", err)
		}
	}
}

func TestWayviaTokenResponseUsesATIdentityWithoutIDTokenOrJWKS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).Unix()
	claims := jwt.MapClaims{
		"iss": "https://auth.openai.com", "aud": []string{"https://api.openai.com/v1"}, "exp": expiry,
		"https://api.openai.com/auth":    map[string]any{"chatgpt_user_id": "at-user", "chatgpt_account_id": "at-account", "chatgpt_plan_type": "plus"},
		"https://api.openai.com/profile": map[string]any{"email": "at@example.invalid"},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "local-response-key-not-in-official-jwks"
	at, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, idToken := range []string{"", "expired-or-invalid-id-token"} {
		info, err := openAITokenInfoFromResponse(&openai.TokenResponse{AccessToken: at, RefreshToken: "new-rt", IDToken: idToken, ExpiresIn: 60}, "client", true)
		if err != nil {
			t.Fatal("trusted Token response should not fetch JWKS or require ID Token", err)
		}
		if info.ChatGPTUserID != "at-user" || info.ChatGPTAccountID != "at-account" || info.Email != "at@example.invalid" || info.ExpiresAt != expiry {
			t.Fatal("identity or expiry did not come from AT")
		}
		creds := (&OpenAIOAuthService{}).BuildAccountCredentials(info)
		if creds["refresh_token"] != "new-rt" || creds["access_token"] != at {
			t.Fatal("new credentials were lost")
		}
		if _, exists := creds["id_token"]; idToken == "" && exists {
			t.Fatal("missing ID Token should not overwrite the retained value")
		}
	}
}

func TestWayviaOAuthSessionRejectsWrongActorBeforeNetwork(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	s := NewOpenAIOAuthService(nil, client)
	defer s.Stop()
	s.sessionStore.Set("linked", &openai.OAuthSession{AccountID: 7, ActorID: 9, State: "state", CreatedAt: time.Now()})
	for _, actor := range []int64{0, 8} {
		_, err := s.ExchangeCode(wayviaegress.WithAdminActor(context.Background(), actor), &OpenAIExchangeCodeInput{SessionID: "linked", State: "state", Code: "fake"})
		if err == nil {
			t.Fatal("unauthorized actor accepted")
		}
	}
	if atomic.LoadInt32(&client.exchangeCalled) != 0 {
		t.Fatal("unauthorized exchange sent")
	}
}

func TestWayviaOAuthSessionTakeIsOneTime(t *testing.T) {
	s := NewOpenAIOAuthService(nil, &openaiOAuthClientStateStub{})
	defer s.Stop()
	s.sessionStore.Set("linked", &openai.OAuthSession{AccountID: 7, ActorID: 9, CreatedAt: time.Now()})
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.sessionStore.Take("linked"); ok {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal(winners.Load())
	}
}

package wayviaegress

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/golang-jwt/jwt/v5"
)

var jwksCache struct {
	sync.Mutex
	keys    map[string]*rsa.PublicKey
	expires time.Time
	fetched time.Time
}

var errSigningKeysUnavailable = errors.New("signing keys temporarily unavailable")

// VerifiedAccessToken 来自已验签的 AT 或可信 Token 端点响应，不依赖 ID Token。
type VerifiedAccessToken struct {
	UserInfo  *openai.UserInfo
	ExpiresAt time.Time
}

type accessTokenClaims struct {
	jwt.RegisteredClaims
	Email   string                   `json:"email"`
	Auth    *openai.OpenAIAuthClaims `json:"https://api.openai.com/auth"`
	Profile struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
}

// VerifyAccessToken 使用 API 的 audience，而非 ID Token 使用的 OAuth client_id。
func VerifyAccessToken(ctx context.Context, raw string) (*VerifiedAccessToken, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, Error("EGRESS_ACCESS_TOKEN_MISSING", "缺少 Access Token，请先刷新账号凭据")
	}
	claims := &accessTokenClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return identityKey(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer("https://auth.openai.com"), jwt.WithAudience("https://api.openai.com/v1"), jwt.WithExpirationRequired())
	return accessTokenDetails(claims, err)
}

// AccessTokenFromResponse 仅供服务层解析已通过 HTTPS Token 端点取得的响应。
// 响应来源已受信，不能在 RT 可能已轮换后再次联网取 JWKS 而丢弃新凭据。
// 管理员导入/数据库中尚未确认的 AT 必须使用 VerifyAccessToken，不能调用此入口。
func AccessTokenFromResponse(raw string) (*VerifiedAccessToken, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, Error("EGRESS_ACCESS_TOKEN_MISSING", "Token 响应缺少 Access Token")
	}
	claims := &accessTokenClaims{}
	token, _, err := jwt.NewParser().ParseUnverified(raw, claims)
	if err == nil && token.Method.Alg() != "RS256" {
		err = jwt.ErrTokenSignatureInvalid
	}
	if err == nil {
		err = jwt.NewValidator(jwt.WithIssuer("https://auth.openai.com"), jwt.WithAudience("https://api.openai.com/v1"), jwt.WithExpirationRequired()).Validate(claims)
	}
	return accessTokenDetails(claims, err)
}

func accessTokenDetails(claims *accessTokenClaims, err error) (*VerifiedAccessToken, error) {
	if err != nil {
		if errors.Is(err, errSigningKeysUnavailable) {
			return nil, Error("EGRESS_JWKS_UNAVAILABLE", "签名密钥服务暂不可用，将重试验证 Access Token")
		}
		if errors.Is(err, jwt.ErrTokenExpired) && !errors.Is(err, jwt.ErrTokenInvalidIssuer) && !errors.Is(err, jwt.ErrTokenInvalidAudience) && !errors.Is(err, jwt.ErrTokenNotValidYet) && !errors.Is(err, jwt.ErrTokenSignatureInvalid) && !errors.Is(err, jwt.ErrTokenRequiredClaimMissing) {
			return nil, Error("EGRESS_ACCESS_TOKEN_EXPIRED", "Access Token 已过期，需要刷新")
		}
		return nil, Error("EGRESS_ACCESS_TOKEN_INVALID", "Access Token 签名、签发者、受众或有效期检查失败")
	}
	if claims.Auth == nil || strings.TrimSpace(claims.Auth.ChatGPTUserID) == "" {
		return nil, Error("EGRESS_IDENTITY_UNVERIFIED", "Access Token 缺少 chatgpt_user_id")
	}
	// 复用字段映射，不读取或解析任何 ID Token。
	email := claims.Profile.Email
	if email == "" {
		email = claims.Email
	}
	info := (&openai.IDTokenClaims{Email: email, OpenAIAuth: claims.Auth}).GetUserInfo()
	return &VerifiedAccessToken{UserInfo: info, ExpiresAt: claims.ExpiresAt.Time}, nil
}
func identityKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	jwksCache.Lock()
	defer jwksCache.Unlock()
	if time.Now().Before(jwksCache.expires) {
		if k := jwksCache.keys[kid]; k != nil {
			return k, nil
		}
		// 支持签名密钥轮换，同时限制未知kid触发的远端刷新频率。
		if time.Since(jwksCache.fetched) < time.Minute {
			return nil, fmt.Errorf("unknown signing key")
		}
	}
	tr := &http.Transport{DialContext: GuardDial((&net.Dialer{Timeout: 5 * time.Second}).DialContext), TLSHandshakeTimeout: 5 * time.Second}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("JWKS redirect denied") }}
	r, err := http.NewRequestWithContext(ctx, "GET", "https://auth.openai.com/.well-known/jwks.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(r)
	if err != nil {
		return nil, signingKeyFetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, errSigningKeysUnavailable
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("JWKS status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			KID string `json:"kid"`
			KTY string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(&doc); err != nil {
		return nil, signingKeyFetchError(err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.KTY != "RSA" || k.Alg != "RS256" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, e1 := base64.RawURLEncoding.DecodeString(k.N)
		e, e2 := base64.RawURLEncoding.DecodeString(k.E)
		if e1 != nil || e2 != nil || len(e) > 4 || len(n) < 256 {
			continue
		}
		ei := 0
		for _, v := range e {
			ei = ei*256 + int(v)
		}
		if ei < 3 {
			continue
		}
		keys[k.KID] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: ei}
	}
	jwksCache.keys = keys
	jwksCache.fetched = time.Now()
	jwksCache.expires = time.Now().Add(15 * time.Minute)
	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key")
}

// 仅网络暂时失败允许自动重试；无效密钥、证书或签名仍保持阻断。
func signingKeyFetchError(err error) error {
	var op *net.OpError
	var dns *net.DNSError
	var timeout net.Error
	if errors.As(err, &op) || errors.As(err, &dns) || errors.As(err, &timeout) && timeout.Timeout() || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errSigningKeysUnavailable
	}
	return err
}

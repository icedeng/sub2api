package service

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/wayviaegress"
)

// OpenAIOAuthService handles OpenAI OAuth authentication flows
type OpenAIOAuthService struct {
	sessionStore         *openai.SessionStore
	proxyRepo            ProxyRepository
	oauthClient          OpenAIOAuthClient
	privacyClientFactory PrivacyClientFactory // 用于调用 chatgpt.com/backend-api（ImpersonateChrome）
}

// NewOpenAIOAuthService creates a new OpenAI OAuth service
func NewOpenAIOAuthService(proxyRepo ProxyRepository, oauthClient OpenAIOAuthClient) *OpenAIOAuthService {
	return &OpenAIOAuthService{
		sessionStore: openai.NewSessionStore(),
		proxyRepo:    proxyRepo,
		oauthClient:  oauthClient,
	}
}

// SetPrivacyClientFactory 注入 ImpersonateChrome 客户端工厂，
// 用于调用 chatgpt.com/backend-api 获取账号信息（plan_type 等）。
func (s *OpenAIOAuthService) SetPrivacyClientFactory(factory PrivacyClientFactory) {
	s.privacyClientFactory = factory
}

// OpenAIAuthURLResult contains the authorization URL and session info
type OpenAIAuthURLResult struct {
	AuthURL   string `json:"auth_url"`
	SessionID string `json:"session_id"`
}

// GenerateAuthURL generates an OpenAI OAuth authorization URL
func (s *OpenAIOAuthService) GenerateAuthURL(ctx context.Context, proxyID *int64, redirectURI, platform string) (*OpenAIAuthURLResult, error) {
	// Generate PKCE values
	state, err := openai.GenerateState()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_STATE_FAILED", "failed to generate state: %v", err)
	}

	codeVerifier, err := openai.GenerateCodeVerifier()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_VERIFIER_FAILED", "failed to generate code verifier: %v", err)
	}

	codeChallenge := openai.GenerateCodeChallenge(codeVerifier)

	// Generate session ID
	sessionID, err := openai.GenerateSessionID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_SESSION_FAILED", "failed to generate session ID: %v", err)
	}

	// Get proxy URL if specified
	var proxyURL string
	if proxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_NOT_FOUND", "proxy not found: %v", err)
		}
		if proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	// Use default redirect URI if not specified
	if redirectURI == "" {
		redirectURI = openai.DefaultRedirectURI
	}
	normalizedPlatform := normalizeOpenAIOAuthPlatform(platform)
	clientID, _ := openai.OAuthClientConfigByPlatform(normalizedPlatform)

	accountID, _ := wayviaegress.AccountContext(ctx)
	var policyVersion int64
	if accountID > 0 {
		if wayviaegress.AdminActor(ctx) <= 0 || wayviaegress.Default() == nil {
			return nil, wayviaegress.Conflict("请使用出口管理页关联授权入口")
		}
		var err error
		policyVersion, err = wayviaegress.Default().ReauthVersion(accountID)
		if err != nil {
			return nil, err
		}
		if proxyID != nil {
			return nil, wayviaegress.Conflict("固定 IPv6 账号重新授权禁止使用代理")
		}
	}
	// Store session
	session := &openai.OAuthSession{
		AccountID:     accountID,
		ActorID:       wayviaegress.AdminActor(ctx),
		PolicyVersion: policyVersion,
		State:         state,
		CodeVerifier:  codeVerifier,
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		ProxyURL:      proxyURL,
		CreatedAt:     time.Now(),
	}
	s.sessionStore.Set(sessionID, session)

	// Build authorization URL
	authURL := openai.BuildAuthorizationURLForPlatform(state, codeChallenge, redirectURI, normalizedPlatform)

	return &OpenAIAuthURLResult{
		AuthURL:   authURL,
		SessionID: sessionID,
	}, nil
}

// OpenAIExchangeCodeInput represents the input for code exchange
type OpenAIExchangeCodeInput struct {
	SessionID   string
	Code        string
	State       string
	RedirectURI string
	ProxyID     *int64
}

// OpenAITokenInfo represents the token information for OpenAI
type OpenAITokenInfo struct {
	WayviaPersisted       bool `json:"-"`
	wayviaCredentials     map[string]any
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	IDToken               string `json:"id_token,omitempty"`
	ExpiresIn             int64  `json:"expires_in"`
	ExpiresAt             int64  `json:"expires_at"`
	ClientID              string `json:"client_id,omitempty"`
	AuthMode              string `json:"auth_mode,omitempty"`
	Email                 string `json:"email,omitempty"`
	ChatGPTAccountID      string `json:"chatgpt_account_id,omitempty"`
	ChatGPTUserID         string `json:"chatgpt_user_id,omitempty"`
	ChatGPTAccountFedRAMP bool   `json:"chatgpt_account_is_fedramp,omitempty"`
	OrganizationID        string `json:"organization_id,omitempty"`
	PlanType              string `json:"plan_type,omitempty"`
	SubscriptionExpiresAt string `json:"subscription_expires_at,omitempty"`
	PrivacyMode           string `json:"privacy_mode,omitempty"`
}

// ExchangeCode exchanges authorization code for tokens
func (s *OpenAIOAuthService) ExchangeCode(ctx context.Context, input *OpenAIExchangeCodeInput) (*OpenAITokenInfo, error) {
	// Get session
	session, ok := s.sessionStore.Get(input.SessionID)
	if !ok {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_SESSION_NOT_FOUND", "session not found or expired")
	}
	if input.State == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_STATE_REQUIRED", "oauth state is required")
	}
	if subtle.ConstantTimeCompare([]byte(input.State), []byte(session.State)) != 1 {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_STATE", "invalid oauth state")
	}
	if wayviaegress.AdminActor(ctx) > 0 && session.AccountID <= 0 {
		return nil, wayviaegress.Conflict("专用入口只允许账号关联会话")
	}

	if session.AccountID > 0 {
		if session.ActorID <= 0 || session.ActorID != wayviaegress.AdminActor(ctx) {
			return nil, wayviaegress.Conflict("授权会话须由同一管理员经二次认证兑换")
		}
		if input.ProxyID != nil || input.RedirectURI != "" && input.RedirectURI != session.RedirectURI {
			return nil, wayviaegress.Conflict("关联授权禁止覆盖代理或回调")
		}
		claimed, ok := s.sessionStore.Take(input.SessionID)
		if !ok {
			return nil, wayviaegress.Conflict("授权会话已使用")
		}
		var result *OpenAITokenInfo
		creds, err := wayviaegress.Default().Reauthorize(ctx, claimed.AccountID, claimed.PolicyVersion, func(callCtx context.Context) (map[string]any, error) {
			proxyURL := ""
			if a, ok := wayviaegress.Default().Account(claimed.AccountID); ok && a.ProxyID != 0 {
				p, err := s.proxyRepo.GetByID(callCtx, a.ProxyID)
				if err != nil {
					return nil, err
				}
				if p == nil {
					return nil, wayviaegress.Conflict("代理不存在")
				}
				proxyURL = p.URL()
			}
			token, err := s.oauthClient.ExchangeCode(callCtx, input.Code, claimed.CodeVerifier, claimed.RedirectURI, proxyURL, claimed.ClientID)
			if err != nil {
				return nil, err
			}
			result, err = openAITokenInfoFromResponse(token, claimed.ClientID, true)
			if err != nil {
				return nil, err
			}
			return s.BuildAccountCredentials(result), nil
		})
		if err != nil {
			return nil, err
		}
		result.WayviaPersisted = true
		result.wayviaCredentials = creds
		return result, nil
	}
	// Get proxy URL: prefer input.ProxyID, fallback to session.ProxyURL
	proxyURL := session.ProxyURL
	if input.ProxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *input.ProxyID)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_NOT_FOUND", "proxy not found: %v", err)
		}
		if proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	// Use redirect URI from session or input
	redirectURI := session.RedirectURI
	if input.RedirectURI != "" {
		redirectURI = input.RedirectURI
	}
	clientID := strings.TrimSpace(session.ClientID)
	if clientID == "" {
		clientID = openai.ClientID
	}

	// Exchange code for token
	tokenResp, err := s.oauthClient.ExchangeCode(ctx, input.Code, session.CodeVerifier, redirectURI, proxyURL, clientID)
	if err != nil {
		return nil, err
	}

	tokenInfo, err := openAITokenInfoFromResponse(tokenResp, clientID, wayviaegress.Configured())
	if err != nil {
		return nil, err
	}

	if id, _ := wayviaegress.AccountContext(ctx); !wayviaegress.ControlledRefresh(id) {
		if err := wayviaegress.RejectDuplicateIdentity(tokenInfo.ChatGPTUserID); err != nil {
			return nil, err
		}
		if !wayviaegress.Configured() {
			s.enrichTokenInfo(ctx, tokenInfo, proxyURL)
		}
	}
	// 身份校验及查重成功后消费授权会话。
	s.sessionStore.Delete(input.SessionID)

	return tokenInfo, nil
}

// RefreshToken refreshes an OpenAI OAuth token
func (s *OpenAIOAuthService) RefreshToken(ctx context.Context, refreshToken string, proxyURL string) (*OpenAITokenInfo, error) {
	return s.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, "")
}

// RefreshTokenWithClientID refreshes an OpenAI OAuth token with optional client_id.
func (s *OpenAIOAuthService) RefreshTokenWithClientID(ctx context.Context, refreshToken string, proxyURL string, clientID string) (*OpenAITokenInfo, error) {
	id, _ := wayviaegress.AccountContext(ctx)
	if id == 0 && (wayviaegress.HasStrict() || wayviaegress.Configured()) {
		return nil, wayviaegress.Error("EGRESS_ACCOUNT_CONTEXT_REQUIRED", "存在固定 IPv6 账号，请使用账号关联刷新入口")
	}
	tokenResp, err := s.oauthClient.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, clientID)
	if err != nil {
		return nil, err
	}

	tokenInfo, err := openAITokenInfoFromResponse(tokenResp, strings.TrimSpace(clientID), wayviaegress.Configured())
	if err != nil {
		return nil, err
	}

	if id, _ := wayviaegress.AccountContext(ctx); !wayviaegress.ControlledRefresh(id) {
		if err := wayviaegress.RejectDuplicateIdentity(tokenInfo.ChatGPTUserID); err != nil {
			return nil, err
		}
		s.enrichTokenInfo(ctx, tokenInfo, proxyURL)
	}

	return tokenInfo, nil
}

// 固定出口的身份和到期时间均从 AT 读取；ID Token 仅原样兼容保存，不参与判断。
// 未配置扩展时保留原项目的信息解析行为。
func openAITokenInfoFromResponse(token *openai.TokenResponse, clientID string, useAccessToken bool) (*OpenAITokenInfo, error) {
	info := &OpenAITokenInfo{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, IDToken: token.IDToken, ExpiresIn: int64(token.ExpiresIn), ExpiresAt: time.Now().Unix() + int64(token.ExpiresIn), ClientID: clientID}
	var user *openai.UserInfo
	if useAccessToken {
		verified, err := wayviaegress.AccessTokenFromResponse(token.AccessToken)
		if err != nil {
			return nil, err
		}
		user, info.ExpiresAt = verified.UserInfo, verified.ExpiresAt.Unix()
	} else if token.IDToken != "" {
		claims, err := openai.ParseIDToken(token.IDToken)
		if err != nil {
			slog.Warn("openai_oauth_id_token_parse_failed", "error", err)
		} else {
			user = claims.GetUserInfo()
		}
	}
	if user != nil {
		info.Email, info.ChatGPTAccountID, info.ChatGPTUserID = user.Email, user.ChatGPTAccountID, user.ChatGPTUserID
		info.OrganizationID, info.PlanType = user.OrganizationID, user.PlanType
	}
	return info, nil
}

// enrichTokenInfo 通过 ChatGPT backend-api 补全 tokenInfo 并设置隐私（best-effort）。
// 从 accounts/check 获取最新 plan_type、subscription_expires_at、email，
// 然后尝试关闭训练数据共享。适用于所有获取/刷新 token 的路径。
func (s *OpenAIOAuthService) enrichTokenInfo(ctx context.Context, tokenInfo *OpenAITokenInfo, proxyURL string) {
	if tokenInfo.AccessToken == "" || s.privacyClientFactory == nil {
		return
	}

	// 从 access_token JWT 中提取 orgID（poid），用于匹配正确的账号
	orgID := tokenInfo.OrganizationID
	if orgID == "" {
		if atClaims, err := openai.DecodeIDToken(tokenInfo.AccessToken); err == nil && atClaims.OpenAIAuth != nil {
			orgID = atClaims.OpenAIAuth.POID
		}
	}
	// accounts/check 命中的记录不属于个人账号时，必须改用个人订阅端点拿到期时间，
	// 否则会把 workspace 权益的 expires_at 当成个人订阅到期日展示。
	forcePersonalSubscriptionLookup := false
	if info := fetchChatGPTAccountInfo(ctx, s.privacyClientFactory, tokenInfo.AccessToken, proxyURL, orgID); info != nil {
		// chatgpt_plan_type from the ID token is the canonical personal-plan value.
		// accounts/check is a multi-account/workspace endpoint; inactive team or
		// business workspaces can otherwise overwrite Pro/Free with internal
		// workspace billing plan names such as self_serve_business_usage_based.
		appliedAccountInfoPlanType := shouldApplyChatGPTAccountInfoPlanType(tokenInfo.PlanType, info.PlanType)
		if appliedAccountInfoPlanType {
			tokenInfo.PlanType = info.PlanType
		}
		// plan_type 与 subscription_expires_at 必须描述同一份订阅。套餐取自
		// accounts/check 时，到期时间跟着取同一条记录；套餐保留了 JWT 里的个人值时，
		// 只有该记录确实就是个人账号才能用它的 entitlement.expires_at——poid 指向的
		// 默认 Personal workspace 与 chatgpt_account_id 可以是两个不同的标识，
		// 混用会显示成「个人 Pro + workspace 到期时间」。
		if info.SubscriptionExpiresAt != "" {
			if appliedAccountInfoPlanType || chatGPTAccountInfoBelongsToTokenAccount(tokenInfo, info) {
				tokenInfo.SubscriptionExpiresAt = info.SubscriptionExpiresAt
			} else {
				forcePersonalSubscriptionLookup = true
			}
		}
		if tokenInfo.Email == "" && info.Email != "" {
			tokenInfo.Email = info.Email
		}
	}
	if forcePersonalSubscriptionLookup || strings.TrimSpace(tokenInfo.SubscriptionExpiresAt) == "" {
		if expiresAt := fetchChatGPTSubscriptionExpiresAt(ctx, s.privacyClientFactory, tokenInfo.AccessToken, proxyURL, resolveChatGPTSubscriptionAccountID(tokenInfo, orgID)); expiresAt != "" {
			tokenInfo.SubscriptionExpiresAt = expiresAt
		}
	}

	// 尝试设置隐私（关闭训练数据共享），best-effort
	tokenInfo.PrivacyMode = disableOpenAITraining(ctx, s.privacyClientFactory, tokenInfo.AccessToken, proxyURL)
}

func shouldApplyChatGPTAccountInfoPlanType(current, candidate string) bool {
	return strings.TrimSpace(candidate) != "" && strings.TrimSpace(current) == ""
}

// chatGPTAccountInfoBelongsToTokenAccount 判断 accounts/check 命中的那条记录是不是
// token 自己的个人 ChatGPT 账号。两侧任一缺 ID 时无法区分，返回 true 保持既有行为。
func chatGPTAccountInfoBelongsToTokenAccount(tokenInfo *OpenAITokenInfo, info *ChatGPTAccountInfo) bool {
	personalID := strings.TrimSpace(tokenInfo.ChatGPTAccountID)
	sourceID := strings.TrimSpace(info.AccountID)
	if personalID == "" || sourceID == "" {
		return true
	}
	return strings.EqualFold(personalID, sourceID)
}

func resolveChatGPTSubscriptionAccountID(tokenInfo *OpenAITokenInfo, orgID string) string {
	for _, candidate := range []string{
		tokenInfo.ChatGPTAccountID,
		tokenInfo.OrganizationID,
		orgID,
	} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// RefreshAccountToken refreshes token for an OpenAI OAuth account
func (s *OpenAIOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (*OpenAITokenInfo, error) {
	if account != nil {
		ctx = wayviaegress.WithAccount(ctx, account.ID, "refresh")
		if err := wayviaegress.CheckAccount(account.ID); err != nil {
			return nil, err
		}
		if wayviaegress.ControlledRefresh(account.ID) && !account.IsOpenAIPersonalAccessToken() && !account.IsOpenAIAgentIdentity() {
			return s.refreshWayviaAccount(ctx, account)
		}
	}
	if account.Platform != PlatformOpenAI {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_ACCOUNT", "account is not an OpenAI account")
	}
	if account.Type != AccountTypeOAuth {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_ACCOUNT_TYPE", "account is not an OAuth account")
	}

	var proxyURL string
	if account.ProxyID != nil && s.proxyRepo != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID)
		if err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	accessToken := account.GetCredential("access_token")
	if account.IsOpenAIPersonalAccessToken() {
		if accessToken == "" {
			return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_CODEX_PAT_REQUIRED", "access token is required")
		}
		return s.ValidateCodexPersonalAccessToken(ctx, accessToken, proxyURL)
	}

	refreshToken := account.GetCredential("refresh_token")
	if refreshToken == "" {
		if accessToken != "" {
			tokenInfo := &OpenAITokenInfo{
				AccessToken:           accessToken,
				RefreshToken:          "",
				IDToken:               account.GetCredential("id_token"),
				ClientID:              account.GetCredential("client_id"),
				Email:                 account.GetCredential("email"),
				ChatGPTAccountID:      account.GetCredential("chatgpt_account_id"),
				ChatGPTUserID:         account.GetCredential("chatgpt_user_id"),
				OrganizationID:        account.GetCredential("organization_id"),
				PlanType:              account.GetCredential("plan_type"),
				SubscriptionExpiresAt: account.GetCredential("subscription_expires_at"),
			}
			if expiresAt := account.GetCredentialAsTime("expires_at"); expiresAt != nil {
				tokenInfo.ExpiresAt = expiresAt.Unix()
				tokenInfo.ExpiresIn = int64(time.Until(*expiresAt).Seconds())
			}
			if id, _ := wayviaegress.AccountContext(ctx); !wayviaegress.ControlledRefresh(id) {
				if err := wayviaegress.RejectDuplicateIdentity(tokenInfo.ChatGPTUserID); err != nil {
					return nil, err
				}
				s.enrichTokenInfo(ctx, tokenInfo, proxyURL)
			}
			return tokenInfo, nil
		}
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_NO_REFRESH_TOKEN", "no refresh token available")
	}

	clientID := account.GetCredential("client_id")
	return s.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, clientID)
}

// BuildAccountCredentials builds credentials map from token info
func (s *OpenAIOAuthService) BuildAccountCredentials(tokenInfo *OpenAITokenInfo) map[string]any {
	if tokenInfo.WayviaPersisted {
		return shallowCopyMap(tokenInfo.wayviaCredentials)
	}
	creds := map[string]any{
		"access_token": tokenInfo.AccessToken,
	}
	if tokenInfo.ExpiresAt > 0 {
		creds["expires_at"] = time.Unix(tokenInfo.ExpiresAt, 0).Format(time.RFC3339)
	}
	// 仅在刷新响应返回了新的 refresh_token 时才更新，防止用空值覆盖已有令牌
	if strings.TrimSpace(tokenInfo.RefreshToken) != "" {
		creds["refresh_token"] = tokenInfo.RefreshToken
	}

	if tokenInfo.IDToken != "" {
		creds["id_token"] = tokenInfo.IDToken
	}
	if tokenInfo.Email != "" {
		creds["email"] = tokenInfo.Email
	}
	if tokenInfo.ChatGPTAccountID != "" {
		creds["chatgpt_account_id"] = tokenInfo.ChatGPTAccountID
	}
	if tokenInfo.ChatGPTUserID != "" {
		creds["chatgpt_user_id"] = tokenInfo.ChatGPTUserID
	}
	if tokenInfo.OrganizationID != "" {
		creds["organization_id"] = tokenInfo.OrganizationID
	}
	if tokenInfo.PlanType != "" {
		creds["plan_type"] = tokenInfo.PlanType
	}
	if tokenInfo.SubscriptionExpiresAt != "" {
		creds["subscription_expires_at"] = tokenInfo.SubscriptionExpiresAt
	}
	if strings.TrimSpace(tokenInfo.ClientID) != "" {
		creds["client_id"] = strings.TrimSpace(tokenInfo.ClientID)
	}
	if tokenInfo.AuthMode == OpenAIAuthModePersonalAccessToken {
		creds[openAIAuthModeCredentialKey] = OpenAIAuthModePersonalAccessToken
		creds[openAIAuthModeLegacyCredentialKey] = "personal_access_token"
		creds["token_type"] = "Bearer"
		creds["chatgpt_account_is_fedramp"] = tokenInfo.ChatGPTAccountFedRAMP
	} else if tokenInfo.ChatGPTAccountFedRAMP {
		creds["chatgpt_account_is_fedramp"] = true
	}

	return NormalizeOpenAIPersonalAccessTokenCredentials(nil, tokenInfo, creds)
}

// Stop stops the session store cleanup goroutine
func (s *OpenAIOAuthService) Stop() {
	s.sessionStore.Stop()
}

func normalizeOpenAIOAuthPlatform(platform string) string {
	return openai.OAuthPlatformOpenAI
}

// 所有严格账号刷新入口在服务层收敛，成功返回前凭据已经CAS持久化。
func (s *OpenAIOAuthService) refreshWayviaAccount(ctx context.Context, account *Account) (*OpenAITokenInfo, error) {
	var result *OpenAITokenInfo
	var proxyURL string
	credentials, err := wayviaegress.Default().Refresh(ctx, account.ID, func(callCtx context.Context, a wayviaegress.Account) (map[string]any, error) {
		var err error
		result, proxyURL, err = s.exchangeWayviaRefresh(callCtx, a)
		if err != nil {
			return nil, err
		}
		return s.BuildAccountCredentials(result), nil
	})
	if err != nil {
		return nil, err
	}
	result.WayviaPersisted = true
	result.wayviaCredentials = credentials
	// query租约重新检查；暂停期间不再补全联网。
	if _, err := wayviaegress.Resolve(account.ID, "query"); err == nil {
		queryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s.enrichTokenInfo(wayviaegress.WithAccount(queryCtx, account.ID, "query"), result, proxyURL)
		cancel()
		fields := map[string]any{"email": result.Email, "plan_type": result.PlanType, "subscription_expires_at": result.SubscriptionExpiresAt, "privacy_mode": result.PrivacyMode}
		if enriched, err := wayviaegress.Default().SaveMetadata(account.ID, credentials, fields); err == nil {
			result.wayviaCredentials = enriched
		}
	}
	return result, nil
}

// 启用前刷新与正常刷新共用同一个 Token 客户端；凭据始终由出口模块重新读取并 CAS 保存。
func (s *OpenAIOAuthService) exchangeWayviaRefresh(ctx context.Context, a wayviaegress.Account) (*OpenAITokenInfo, string, error) {
	refresh, _ := a.Credentials["refresh_token"].(string)
	client, _ := a.Credentials["client_id"].(string)
	proxyURL := ""
	if a.ProxyID != 0 {
		if s.proxyRepo == nil {
			return nil, "", wayviaegress.Conflict("代理仓储不可用")
		}
		p, err := s.proxyRepo.GetByID(ctx, a.ProxyID)
		if err != nil {
			return nil, "", err
		}
		if p == nil {
			return nil, "", wayviaegress.Conflict("代理不存在")
		}
		proxyURL = p.URL()
	}
	info, err := s.RefreshTokenWithClientID(ctx, refresh, proxyURL, client)
	return info, proxyURL, err
}

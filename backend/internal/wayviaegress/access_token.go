package wayviaegress

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// SetAccessTokenRefresh 复用服务层的 RT 交换与提前刷新窗口，出口模块只负责门控和提交。
func (m *Manager) SetAccessTokenRefresh(exchange func(context.Context, Account) (map[string]any, error), window time.Duration) {
	m.mu.Lock()
	m.refreshExchange, m.refreshWindow = exchange, window
	m.mu.Unlock()
}

// accessTokenNeedsRefresh 兼容原项目 expires_at 的时间格式；AT 自带的签名期限兜底。
func accessTokenNeedsRefresh(a Account, token *VerifiedAccessToken, window time.Duration) bool {
	expires := token.ExpiresAt
	var stored time.Time
	switch v := a.Credentials["expires_at"].(type) {
	case string:
		stored, _ = time.Parse(time.RFC3339, v)
		if stored.IsZero() {
			if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
				stored = time.Unix(ts, 0)
			}
		}
	case float64:
		stored = time.Unix(int64(v), 0)
	case int64:
		stored = time.Unix(v, 0)
	case int:
		stored = time.Unix(int64(v), 0)
	case json.Number:
		if ts, err := v.Int64(); err == nil {
			stored = time.Unix(ts, 0)
		}
	}
	if !stored.IsZero() && stored.Before(expires) {
		expires = stored
	}
	return time.Until(expires) <= window
}

// prepareAccountAccessToken 由人工或自动启用流程在出口转换前置检查后调用。
// 新账号的刷新使用仅能访问 Token 端点的控制租约，不开放业务、查询或预热。
func (m *Manager) prepareAccountAccessToken(ctx context.Context, a Account, p Policy) (Account, error) {
	m.mu.Lock()
	exchange, window := m.refreshExchange, m.refreshWindow
	m.mu.Unlock()
	raw, _ := a.Credentials["access_token"].(string)
	token, err := VerifyAccessToken(ctx, raw)
	if err != nil && apperrors.Reason(err) != "EGRESS_ACCESS_TOKEN_EXPIRED" && apperrors.Reason(err) != "EGRESS_ACCESS_TOKEN_MISSING" {
		return a, err
	}
	if err == nil && (token.UserInfo.ChatGPTUserID != a.Identity || p.Identity != "" && token.UserInfo.ChatGPTUserID != p.Identity) {
		return a, Conflict("Access Token 身份与账号或原出口绑定不一致")
	}
	refreshToken, _ := a.Credentials["refresh_token"].(string)
	if strings.TrimSpace(refreshToken) == "" {
		// AT-only 账号可在签名有效期内使用；没有 RT 不代表不能绑定 IPv6。
		if err != nil {
			return a, Error(apperrors.Reason(err), "Access Token 缺失或已过期，且没有 Refresh Token；请更新账号凭据")
		}
		return a, nil
	}
	if err != nil || accessTokenNeedsRefresh(a, token, window) {
		if exchange == nil {
			return a, Error("EGRESS_REFRESH_UNAVAILABLE", "Access Token 需要刷新，但刷新服务尚未就绪")
		}
		credentials, refreshErr := m.refresh(ctx, a.ID, exchange, true)
		if refreshErr != nil {
			return a, refreshErr
		}
		a.Credentials = credentials
		a.Identity, _ = credentials["chatgpt_user_id"].(string)
		raw, _ = credentials["access_token"].(string)
		token, err = VerifyAccessToken(ctx, raw)
		if err != nil {
			return a, err
		}
	}
	if token.UserInfo.ChatGPTUserID != a.Identity || p.Identity != "" && token.UserInfo.ChatGPTUserID != p.Identity {
		return a, Conflict("Access Token 身份与账号或原出口绑定不一致")
	}
	return a, nil
}

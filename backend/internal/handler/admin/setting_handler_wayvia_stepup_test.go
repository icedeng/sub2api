package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type wayviaStepUpUserRepo struct {
	service.UserRepository
	user  *service.User
	calls int
}

func (r *wayviaStepUpUserRepo) GetByID(_ context.Context, _ int64) (*service.User, error) {
	r.calls++
	return r.user, nil
}

func (r *wayviaStepUpUserRepo) GetUserAvatar(_ context.Context, _ int64) (*service.UserAvatar, error) {
	return nil, nil
}

type wayviaStepUpCache struct {
	service.TotpCache
	grants     map[string]bool
	err        error
	calls      int
	lastUserID int64
}

func (c *wayviaStepUpCache) HasStepUpGrant(_ context.Context, userID int64, sessionKey string) (bool, error) {
	c.calls++
	c.lastUserID = userID
	return c.grants[sessionKey], c.err
}

func doWayviaStepUp(h *SettingHandler, method string, userID int64, sessionKey string) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth_method", method)
		if userID > 0 {
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID})
		}
		c.Set(middleware.ContextKeySessionID, sessionKey)
	}, h.RequireWayviaStepUp)
	router.PUT("/mode", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/mode", nil))
	return rec
}

func TestRequireWayviaStepUpFollowsSystemSwitch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		totp       bool
		grant      bool
		grantErr   error
		wantStatus int
		wantCode   string
	}{
		{name: "disabled_without_totp", wantStatus: http.StatusNoContent},
		{name: "disabled_without_grant", totp: true, wantStatus: http.StatusNoContent},
		{name: "enabled_requires_totp", enabled: true, wantStatus: http.StatusForbidden, wantCode: "STEP_UP_TOTP_NOT_ENABLED"},
		{name: "enabled_requires_grant", enabled: true, totp: true, wantStatus: http.StatusForbidden, wantCode: "STEP_UP_REQUIRED"},
		{name: "enabled_reuses_session_grant", enabled: true, totp: true, grant: true, wantStatus: http.StatusNoContent},
		{name: "enabled_cache_failure_blocks", enabled: true, totp: true, grantErr: errors.New("cache unavailable"), wantStatus: http.StatusServiceUnavailable, wantCode: "STEP_UP_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newStepUpSwitchTestHandler(t, map[string]string{service.SettingKeyStepUpEnabled: strconv.FormatBool(tc.enabled)})
			users := &wayviaStepUpUserRepo{user: &service.User{ID: 7, TotpEnabled: tc.totp}}
			cache := &wayviaStepUpCache{grants: map[string]bool{"session-a": tc.grant}, err: tc.grantErr}
			h.SetStepUpDeps(service.NewTotpService(users, nil, cache, h.settingService, nil, nil), service.NewUserService(users, nil, nil, nil))

			rec := doWayviaStepUp(h, service.AuditAuthMethodJWT, 7, "session-a")
			require.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantCode != "" {
				require.Contains(t, rec.Body.String(), tc.wantCode)
			}
			if !tc.enabled {
				require.Zero(t, users.calls)
				require.Zero(t, cache.calls)
			} else if tc.totp {
				require.Equal(t, 1, cache.calls)
				require.Equal(t, int64(7), cache.lastUserID)
			}
		})
	}
}

func TestRequireWayviaStepUpReadsUpdatedSwitchAndCurrentSession(t *testing.T) {
	h, settings := newStepUpSwitchTestHandler(t, map[string]string{service.SettingKeyStepUpEnabled: "false"})
	users := &wayviaStepUpUserRepo{user: &service.User{ID: 7, TotpEnabled: true}}
	cache := &wayviaStepUpCache{grants: map[string]bool{}}
	h.SetStepUpDeps(service.NewTotpService(users, nil, cache, h.settingService, nil, nil), service.NewUserService(users, nil, nil, nil))
	request := func(session string) *httptest.ResponseRecorder {
		return doWayviaStepUp(h, service.AuditAuthMethodJWT, 7, session)
	}
	require.Equal(t, http.StatusNoContent, request("session-a").Code)
	settings.values[service.SettingKeyStepUpEnabled] = "true"
	require.Equal(t, http.StatusForbidden, request("session-a").Code)
	cache.grants["session-a"] = true
	require.Equal(t, http.StatusNoContent, request("session-a").Code)
	require.Equal(t, http.StatusForbidden, request("session-b").Code)
	// 会话授权失效后重新要求验证码；关闭系统开关后立即停止要求。
	delete(cache.grants, "session-a")
	require.Equal(t, http.StatusForbidden, request("session-a").Code)
	settings.values[service.SettingKeyStepUpEnabled] = "false"
	require.Equal(t, http.StatusNoContent, request("session-a").Code)
}

func TestRequireWayviaStepUpPreservesIdentityRestrictions(t *testing.T) {
	for _, enabled := range []string{"false", "true"} {
		for _, method := range []string{service.AuditAuthMethodAdminAPIKey, service.AuditAuthMethodMCPOAuth, "future-machine"} {
			t.Run(enabled+"/"+method, func(t *testing.T) {
				h, _ := newStepUpSwitchTestHandler(t, map[string]string{service.SettingKeyStepUpEnabled: enabled})
				h.SetStepUpDeps(&service.TotpService{}, &service.UserService{})
				rec := doWayviaStepUp(h, method, 7, "session-a")
				require.Equal(t, http.StatusForbidden, rec.Code)
				require.Contains(t, rec.Body.String(), "FORBIDDEN")
			})
		}
		t.Run(enabled+"/missing-subject", func(t *testing.T) {
			h, _ := newStepUpSwitchTestHandler(t, map[string]string{service.SettingKeyStepUpEnabled: enabled})
			h.SetStepUpDeps(&service.TotpService{}, &service.UserService{})
			rec := doWayviaStepUp(h, service.AuditAuthMethodJWT, 0, "session-a")
			require.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestRequireWayviaStepUpRejectsMissingDependencies(t *testing.T) {
	for _, dependency := range []string{"settings", "totp", "users"} {
		t.Run(dependency, func(t *testing.T) {
			h, _ := newStepUpSwitchTestHandler(t, map[string]string{service.SettingKeyStepUpEnabled: "false"})
			h.SetStepUpDeps(&service.TotpService{}, &service.UserService{})
			switch dependency {
			case "settings":
				h.settingService = nil
			case "totp":
				h.totpService = nil
			case "users":
				h.userService = nil
			}
			require.Equal(t, http.StatusServiceUnavailable, doWayviaStepUp(h, service.AuditAuthMethodJWT, 7, "session-a").Code)
		})
	}
}

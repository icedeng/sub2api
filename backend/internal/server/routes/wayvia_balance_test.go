package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/wayviabalance"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWayviaBalanceRoutesProtectAllEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	endpoints := []struct{ method, path string }{
		{http.MethodGet, "/accounts?ids=1"},
		{http.MethodGet, "/accounts/1"},
		{http.MethodPut, "/accounts/1"},
		{http.MethodPost, "/accounts/1/refresh"},
	}
	for _, endpoint := range endpoints {
		for _, access := range []string{"unauthenticated", "restricted", "full"} {
			t.Run(endpoint.method+endpoint.path+"/"+access, func(t *testing.T) {
				audited := false
				router := gin.New()
				auth := middleware.AdminAuthMiddleware(func(c *gin.Context) {
					if access == "unauthenticated" {
						c.AbortWithStatus(http.StatusUnauthorized)
						return
					}
					c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{
						Policy: adminaccess.Policy{FullAccess: access == "full"},
					}))
					c.Next()
				})
				audit := middleware.AuditLogMiddleware(func(c *gin.Context) { audited = true; c.Next() })
				RegisterWayviaBalanceRoutes(router.Group("/api/v1"), nil, auth, audit, nil, nil, nil)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(endpoint.method, "/api/v1/admin/wayvia-balance"+endpoint.path, nil))
				switch access {
				case "unauthenticated":
					require.Equal(t, http.StatusUnauthorized, w.Code)
					require.False(t, audited)
				case "restricted":
					require.Equal(t, http.StatusForbidden, w.Code)
					require.True(t, audited)
				case "full":
					require.Equal(t, http.StatusServiceUnavailable, w.Code)
					require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
					require.True(t, audited)
				}
			})
		}
	}
}

func TestWayviaBalanceWritesRequireStepUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stepUpConfigured := range []bool{false, true} {
		for _, method := range []string{http.MethodPut, http.MethodPost} {
			router := gin.New()
			pass := func(c *gin.Context) { c.Next() }
			var stepUp middleware.StepUpAuthMiddleware
			called := false
			if stepUpConfigured {
				stepUp = func(c *gin.Context) { called = true; c.AbortWithStatus(http.StatusForbidden) }
			}
			RegisterWayviaBalanceRoutes(router.Group("/api/v1"), &wayviabalance.Manager{}, middleware.AdminAuthMiddleware(pass), middleware.AuditLogMiddleware(pass), stepUp, nil, nil)
			path := "/api/v1/admin/wayvia-balance/accounts/1"
			if method == http.MethodPost {
				path += "/refresh"
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader("{}")))
			require.Equal(t, http.StatusForbidden, w.Code)
			require.Equal(t, stepUpConfigured, called)
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		}
	}
}

package routes

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/wayviabalance"
	"github.com/gin-gonic/gin"
)

// RegisterWayviaBalanceRoutes 独立注册以便合并上游；沿用管理认证、审计及二次验证。
func RegisterWayviaBalanceRoutes(v1 *gin.RouterGroup, manager *wayviabalance.Manager, auth middleware.AdminAuthMiddleware, audit middleware.AuditLogMiddleware, stepUp middleware.StepUpAuthMiddleware, settings *service.SettingService, limiter *middleware.PanelRateLimiter) {
	g := v1.Group("/admin/wayvia-balance", gin.HandlerFunc(auth))
	if limiter != nil {
		g.Use(limiter.Global())
	}
	g.Use(gin.HandlerFunc(audit), middleware.AdminComplianceGuard(settings), func(c *gin.Context) {
		if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
			response.Forbidden(c, "受限管理密钥无上游余额权限")
			c.Abort()
			return
		}
		c.Header("Cache-Control", "no-store")
		if manager == nil {
			balanceError(c, wayviabalance.ErrUnavailable)
			c.Abort()
			return
		}
		c.Next()
	})
	g.GET("/accounts", func(c *gin.Context) {
		parts := strings.Split(c.Query("ids"), ",")
		if len(parts) == 0 || len(parts) > 100 {
			response.BadRequest(c, "每次需提供1至100个账号")
			return
		}
		ids := make([]int64, 0, len(parts))
		seen := map[int64]bool{}
		for _, part := range parts {
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				response.BadRequest(c, "无效账号ID")
				return
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		states, err := manager.Batch(c.Request.Context(), ids)
		if err != nil {
			balanceError(c, err)
			return
		}
		response.Success(c, gin.H{"items": states})
	})
	g.GET("/accounts/:id", func(c *gin.Context) {
		id, ok := balanceAccountID(c)
		if !ok {
			return
		}
		state, err := manager.Get(c.Request.Context(), id)
		if err != nil {
			balanceError(c, err)
			return
		}
		response.Success(c, state)
	})
	writeGate := func(c *gin.Context) {
		if stepUp == nil {
			response.Forbidden(c, "二次验证不可用")
			c.Abort()
			return
		}
		gin.HandlerFunc(stepUp)(c)
	}
	g.PUT("/accounts/:id", writeGate, func(c *gin.Context) {
		id, ok := balanceAccountID(c)
		if !ok {
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
		var in wayviabalance.UpdateRequest
		if err := c.ShouldBindJSON(&in); err != nil {
			response.BadRequest(c, "无效余额查询配置")
			return
		}
		middleware.SetAuditExtra(c, map[string]any{"account_id": id})
		state, err := manager.Update(c.Request.Context(), id, in)
		if err != nil {
			balanceError(c, err)
			return
		}
		response.Success(c, state)
	})
	g.POST("/accounts/:id/refresh", writeGate, func(c *gin.Context) {
		id, ok := balanceAccountID(c)
		if !ok {
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
		var in struct {
			Rediscover bool `json:"rediscover"`
		}
		if err := c.ShouldBindJSON(&in); err != nil && !errors.Is(err, io.EOF) {
			response.BadRequest(c, "无效刷新请求")
			return
		}
		middleware.SetAuditExtra(c, map[string]any{"account_id": id})
		state, err := manager.Refresh(c.Request.Context(), id, in.Rediscover)
		if err != nil {
			balanceError(c, err)
			return
		}
		response.Success(c, state)
	})
}

func balanceAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "无效账号ID")
		return 0, false
	}
	return id, true
}

func balanceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, wayviabalance.ErrConfigChanged):
		response.ErrorWithDetails(c, 409, "配置已变化，请重新加载", "CONFIG_CHANGED", nil)
	case errors.Is(err, wayviabalance.ErrIneligible), errors.Is(err, wayviabalance.ErrInvalidConfig):
		response.BadRequest(c, "无效余额查询配置或账号类型")
	case errors.Is(err, service.ErrAccountNotFound):
		response.NotFound(c, "账号不存在")
	default:
		response.ErrorWithDetails(c, 503, "余额查询暂不可用，请检查配置或稍后重试", "BALANCE_UNAVAILABLE", nil)
	}
}

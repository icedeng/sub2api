package routes

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/wayviaegress"
	"github.com/gin-gonic/gin"
)

func registerWayviaEgressRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	g := admin.Group("/wayvia-egress")
	g.Use(func(c *gin.Context) {
		if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
			response.Forbidden(c, "受限管理密钥无IPv6出口权限")
			c.Abort()
			return
		}
		if wayviaegress.Default() == nil {
			response.ErrorFrom(c, wayviaegress.Error("EGRESS_POLICY_UNKNOWN", "IPv6出口模块未初始化"))
			c.Abort()
			return
		}
		c.Next()
	})
	g.GET("/status", func(c *gin.Context) { response.Success(c, wayviaegress.Default().Status(c.Request.Context())) })
	g.GET("/retired-accounts", func(c *gin.Context) {
		rows, err := wayviaegress.Default().RetiredAccounts(c.Request.Context())
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, rows)
	})
	g.GET("/accounts/:id/evidence", func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "无效账号")
			return
		}
		limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
		if err != nil || limit < 1 || limit > 100 {
			response.BadRequest(c, "记录数量需为1至100")
			return
		}
		result, err := wayviaegress.Default().Evidence(c.Request.Context(), id, limit)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, result)
	})
	g.GET("/accounts/:id/history", func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "无效账号")
			return
		}
		rows, err := wayviaegress.Default().History(c.Request.Context(), id)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, rows)
	})
	g.GET("/bindings", func(c *gin.Context) {
		rows := wayviaegress.Default().Bindings()
		filtered := make([]wayviaegress.BindingView, 0, len(rows))
		q := strings.ToLower(c.Query("q"))
		state := c.Query("state")
		idFilter, _ := strconv.ParseInt(c.Query("account_id"), 10, 64)
		ids := map[int64]bool{}
		if raw := c.Query("account_ids"); raw != "" {
			parts := strings.Split(raw, ",")
			if len(parts) > 200 {
				response.BadRequest(c, "最多200个账号")
				return
			}
			for _, part := range parts {
				id, err := strconv.ParseInt(part, 10, 64)
				if err != nil || id <= 0 {
					response.BadRequest(c, "无效账号ID")
					return
				}
				ids[id] = true
			}
		}
		for _, v := range rows {
			if len(ids) > 0 && !ids[v.Account.ID] {
				continue
			}
			if idFilter > 0 && v.Account.ID != idFilter {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(v.Name+v.Source), q) {
				continue
			}
			if state != "" && v.State != state {
				continue
			}
			filtered = append(filtered, v)
		}
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		size, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
		if page < 1 {
			page = 1
		}
		if size < 1 || size > 1000 {
			size = 50
		}
		start := len(filtered)
		if page-1 <= len(filtered)/size {
			start = min((page-1)*size, len(filtered))
		}
		end := min(start+size, len(filtered))
		response.Success(c, gin.H{"items": filtered[start:end], "total": len(filtered), "page": page, "page_size": size})
	})
	g.GET("/operations/:id", func(c *gin.Context) {
		op, err := wayviaegress.Default().Operation(c.Request.Context(), c.Param("id"))
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "操作不存在")
			return
		}
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, op)
	})
	mutate := func(kind string) gin.HandlerFunc {
		return func(c *gin.Context) {
			var in wayviaegress.Change
			if err := c.ShouldBindJSON(&in); err != nil {
				response.BadRequest(c, "无效请求")
				return
			}
			in.Kind = kind
			in.AccountID = 0
			if kind != "runtime" {
				id, err := strconv.ParseInt(c.Param("id"), 10, 64)
				if err != nil || id <= 0 {
					response.BadRequest(c, "无效账号")
					return
				}
				in.AccountID = id
			}
			subject, ok := middleware.GetAuthSubjectFromContext(c)
			if !ok {
				response.Forbidden(c, "缺少管理员身份")
				return
			}
			middleware.SetAuditExtra(c, map[string]any{"account_id": in.AccountID})
			op, err := wayviaegress.Default().Change(c.Request.Context(), in, fmt.Sprint(subject.UserID))
			if err != nil {
				response.ErrorFrom(c, err)
				return
			}
			middleware.SetAuditExtra(c, map[string]any{"operation_id": op.ID})
			c.JSON(202, gin.H{"code": 0, "message": "success", "data": op})
		}
	}
	reauth := g.Group("/reauth", h.Admin.Setting.RequireWayviaStepUp, func(c *gin.Context) {
		subject, ok := middleware.GetAuthSubjectFromContext(c)
		if !ok {
			response.Forbidden(c, "缺少管理员身份")
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(wayviaegress.WithAdminActor(c.Request.Context(), subject.UserID))
		c.Next()
	})
	reauth.POST("/generate-auth-url", h.Admin.OpenAIOAuth.GenerateAuthURL)
	reauth.POST("/exchange-code", h.Admin.OpenAIOAuth.ExchangeCode)
	g.PUT("/runtime", h.Admin.Setting.RequireWayviaStepUp, mutate("runtime"))
	g.PUT("/accounts/:id/mode", h.Admin.Setting.RequireWayviaStepUp, mutate("mode"))
	g.POST("/accounts/:id/rotate", h.Admin.Setting.RequireWayviaStepUp, mutate("rotate"))
	g.POST("/accounts/:id/probe", h.Admin.Setting.RequireWayviaStepUp, mutate("probe"))
	g.POST("/accounts/:id/restore-binding", h.Admin.Setting.RequireWayviaStepUp, mutate("restore-binding"))
	g.POST("/accounts/:id/restore-account", h.Admin.Setting.RequireWayviaStepUp, mutate("restore-account"))
	g.POST("/accounts/:id/retire", h.Admin.Setting.RequireWayviaStepUp, mutate("retire"))
}

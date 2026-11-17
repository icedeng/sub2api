package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

func registerWayviaHealthRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	g := admin.Group("/account-health")
	g.GET("/settings", h.Admin.Account.HealthSettings)
	g.PUT("/settings", h.Admin.Setting.RequireWayviaStepUp, h.Admin.Account.HealthSettings)
	g.POST("/preview", h.Admin.Account.HealthPreview)
	g.POST("/apply", h.Admin.Setting.RequireWayviaStepUp, h.Admin.Account.HealthApply)
	g.POST("/batch-tests/preview", h.Admin.Account.HealthBatchPreview)
	g.POST("/batch-tests", h.Admin.Setting.RequireWayviaStepUp, h.Admin.Account.HealthBatch)
	g.GET("/test-runs", h.Admin.Account.HealthRuns)
	g.GET("/summaries", h.Admin.Account.HealthSummaries)
	admin.GET("/accounts/:id/health-policy", h.Admin.Account.HealthPolicy)
	admin.PUT("/accounts/:id/health-policy", h.Admin.Setting.RequireWayviaStepUp, h.Admin.Account.HealthPolicy)
}

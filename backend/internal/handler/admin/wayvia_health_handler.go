package admin

import (
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/wayviahealth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (h *AccountHandler) healthService(c *gin.Context) *service.WayviaHealthService {
	if adminaccess.RestrictedFromContext(c.Request.Context()) != nil {
		response.Forbidden(c, "账号健康配置及实际检测内容仅开放给完整账号管理权限的管理员")
		return nil
	}
	if h.accountTestService == nil || h.accountTestService.Health() == nil {
		response.InternalError(c, "账号健康模块不可用")
		return nil
	}
	return h.accountTestService.Health()
}

func (h *AccountHandler) HealthSettings(c *gin.Context) {
	s := h.healthService(c)
	if s == nil {
		return
	}
	if c.Request.Method == "PUT" {
		v := wayviahealth.Defaults()
		if err := c.ShouldBindJSON(&v); err != nil {
			response.BadRequest(c, "无效配置")
			return
		}
		result, err := s.SaveSettings(c.Request.Context(), v)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		middleware.SetAuditExtra(c, map[string]any{"config_version": result.Version})
		response.Success(c, result)
		return
	}
	v, err := s.Settings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	coverage, err := s.Coverage(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"settings": v, "coverage": coverage, "timezone": timezone.Location().String()})
}

func (h *AccountHandler) HealthPreview(c *gin.Context) {
	if h.healthService(c) == nil {
		return
	}
	var in struct {
		TestContent string `json:"test_content"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, "无效内容")
		return
	}
	if err := wayviahealth.Validate(in.TestContent); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	r, err := wayviahealth.Render(in.TestContent, timezone.Now(), nil)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, r)
}

func healthAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		response.BadRequest(c, "无效账号 ID")
		return 0, false
	}
	return id, true
}

func (h *AccountHandler) HealthPolicy(c *gin.Context) {
	s := h.healthService(c)
	if s == nil {
		return
	}
	id, ok := healthAccountID(c)
	if !ok {
		return
	}
	if c.Request.Method == "PUT" {
		var p wayviahealth.Policy
		if err := c.ShouldBindJSON(&p); err != nil {
			response.BadRequest(c, "无效策略")
			return
		}
		if err := s.SetPolicy(c.Request.Context(), id, p); err != nil {
			response.ErrorFrom(c, err)
			return
		}
	}
	v, err := s.Effective(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, v)
}

func (h *AccountHandler) HealthApply(c *gin.Context) {
	s := h.healthService(c)
	if s == nil {
		return
	}
	var in struct {
		service.WayviaHealthSelection
		Confirm bool `json:"confirm"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, "无效选择")
		return
	}
	if in.Confirm && in.Filters != nil {
		response.BadRequest(c, "请先预览，再提交已冻结的账号集合")
		return
	}
	ids, err := s.Freeze(c.Request.Context(), in.WayviaHealthSelection)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	eligible := []int64{}
	for _, id := range ids {
		v, err := s.Effective(c.Request.Context(), id)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if v.Policy.Mode == "" {
			eligible = append(eligible, id)
		}
	}
	if !in.Confirm {
		response.Success(c, gin.H{"account_ids": eligible, "count": len(eligible), "preserved": len(ids) - len(eligible)})
		return
	}
	n, err := s.Apply(c.Request.Context(), eligible)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"matched_count": len(eligible), "requested_count": len(ids)})
	response.Success(c, gin.H{"applied": n})
}

func (h *AccountHandler) HealthBatchPreview(c *gin.Context) {
	s := h.healthService(c)
	if s == nil {
		return
	}
	var in service.WayviaHealthSelection
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, "无效选择")
		return
	}
	ids, err := s.Freeze(c.Request.Context(), in)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	cfg, err := s.Settings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"account_ids": ids, "count": len(ids), "concurrency": cfg.Concurrency, "timeout_seconds": cfg.TimeoutSeconds})
}

func (h *AccountHandler) HealthBatch(c *gin.Context) {
	s := h.healthService(c)
	if s == nil {
		return
	}
	var in struct {
		service.WayviaHealthSelection
		Model       string  `json:"model"`
		TestContent *string `json:"test_content"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, "无效批量请求")
		return
	}
	result, err := s.Batch(c.Request.Context(), in.WayviaHealthSelection, in.Model, in.TestContent)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"operation_id": result.BatchID, "requested_count": len(result.Runs)})
	response.Success(c, result)
}

func (h *AccountHandler) HealthRuns(c *gin.Context) {
	s := h.healthService(c)
	if s == nil {
		return
	}
	id, err := strconv.ParseInt(c.DefaultQuery("account_id", "0"), 10, 64)
	if err != nil || id < 0 {
		response.BadRequest(c, "无效账号 ID")
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	var before *time.Time
	if raw := c.Query("before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			response.BadRequest(c, "无效分页时间")
			return
		}
		before = &parsed
	}
	if value := c.Query("before_id"); value != "" {
		if _, err := uuid.Parse(value); err != nil {
			response.BadRequest(c, "无效分页标识")
			return
		}
	}
	result, err := s.Runs(c.Request.Context(), id, c.Query("batch_id"), c.Query("source"), limit, before, c.Query("before_id"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AccountHandler) HealthSummaries(c *gin.Context) {
	s := h.healthService(c)
	if s == nil {
		return
	}
	ids := []int64{}
	for _, part := range strings.Split(c.Query("account_ids"), ",") {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id < 1 {
			response.BadRequest(c, "无效账号 ID")
			return
		}
		ids = append(ids, id)
	}
	result, err := s.Summaries(c.Request.Context(), ids)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

package admin

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/gin-gonic/gin"
)

func TestWayviaHealthRestrictedAdminCannotReadContentOrTrigger(t *testing.T) {
	h := &AccountHandler{}
	for _, handler := range []gin.HandlerFunc{h.HealthSettings, h.HealthPreview, h.HealthPolicy, h.HealthApply, h.HealthBatchPreview, h.HealthBatch, h.HealthRuns, h.HealthSummaries} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/", nil)
		c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{Policy: adminaccess.Policy{FullAccess: false}}))
		handler(c)
		if w.Code != 403 {
			t.Fatalf("restricted admin returned %d", w.Code)
		}
	}
}

package repository

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/lib/pq"
)

func TestTranslateControlledCredentialWriteConflict(t *testing.T) {
	err := fmt.Errorf("update account: %w", &pq.Error{Code: "P0001", Message: "EGRESS_CONTROLLED_CREDENTIAL_WRITE_REQUIRED"})
	translated := translatePersistenceError(err, nil, nil)
	code, status := infraerrors.ToHTTP(translated)
	if code != http.StatusConflict || status.Reason != "EGRESS_CONTROLLED_CREDENTIAL_WRITE_REQUIRED" || !strings.Contains(status.Message, "重新打开编辑窗口") {
		t.Fatalf("凭据冲突应返回可处理的 409 提示: %d %+v", code, status)
	}
}

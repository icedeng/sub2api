package wayviaegress

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// BeforeDelete 停流和持久退役成功后，才允许官方删除事务继续。
func (m *Manager) BeforeDelete(ctx context.Context, id int64) error {
	m.mu.Lock()
	a, p, err := m.rootLocked(id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if !a.Eligible() || p.State == "retired" {
		return nil
	}
	if a.ID != id {
		if p.Strict() {
			return Conflict("固定 IPv6 影子账号需随母账号一并删除，以保留原固定出口策略")
		}
		return nil
	}
	if !p.Strict() && !ControlledRefresh(id) {
		return nil
	}
	op, err := m.Change(ctx, Change{Kind: "retire", AccountID: id, ExpectedVersion: p.Version, IdempotencyKey: "delete-" + uuid.NewString(), Acknowledge: true}, "account-delete")
	if err != nil {
		return err
	}
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return Error("EGRESS_OPERATION_PENDING", "退役已接受，请查询操作后重试删除")
		case <-t.C:
			next, e := m.Operation(ctx, op.ID)
			if e != nil {
				return e
			}
			if next.Phase == "completed" {
				return nil
			}
			if next.Phase == "failed" {
				return Error("EGRESS_RETIRE_FAILED", next.Error)
			}
		}
	}
}

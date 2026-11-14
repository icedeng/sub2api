package wayviaegress

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 导入事务已经关门并写入队列；此任务只调用既有受控启用流程，不直接打开调度。
// 手动出口变更继续使用管理员二次验证，后台任务以独立系统 actor 记录审计。
func (m *Manager) autoEnrollLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.recoverRuntimeOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Error("wayvia runtime recovery failed", "reason", safeError(err))
			}
			if err := m.autoEnrollOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Error("wayvia automatic enrollment failed", "reason", safeError(err))
			}
		}
	}
}

type enrollmentJob struct {
	AccountID int64
	Owner     string
	Attempts  int
	Key       string
	Version   int64
}

func (m *Manager) autoEnrollOnce(ctx context.Context) error {
	m.mu.Lock()
	available := !m.closing && m.initError == "" && m.lock != nil && m.config.StateDir != "" && m.refreshExchange != nil
	m.mu.Unlock()
	if !available {
		return nil
	}
	if err := m.reconcileAutoEnrollments(ctx); err != nil {
		return err
	}
	if err := m.Reload(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	enabled := m.runtime.Enabled
	m.mu.Unlock()
	if !enabled {
		return nil
	}
	var job enrollmentJob
	// 数据库队列覆盖单个导入、批量导入和重启；不依赖浏览器停留在导入页面。
	// 一次只领取一个任务，与既有出口操作串行约束保持一致。
	err := m.db.QueryRowContext(ctx, `WITH candidate AS (
	 SELECT j.account_id,p.policy_version FROM wayvia_egress_enrollments j
	 JOIN wayvia_account_egress_policies p ON p.owner_id=j.owner_id AND p.root_account_id=j.account_id
	 JOIN accounts a ON a.id=j.account_id
	 WHERE j.state IN ('queued','retry') AND j.next_attempt_at<=now()
	 AND a.deleted_at IS NULL AND a.status='active' AND NOT a.schedulable
	 AND a.parent_account_id IS NULL AND p.require_ipv6 AND p.current_mode='legacy'
	 AND p.state IN ('pending_enrollment','blocked')
	 AND NOT EXISTS(SELECT 1 FROM wayvia_egress_operations WHERE phase IN ('accepted','draining','preparing'))
	 ORDER BY j.next_attempt_at,j.account_id LIMIT 1 FOR UPDATE OF j SKIP LOCKED
	) UPDATE wayvia_egress_enrollments j SET state='running',attempts=j.attempts+1,
	 idempotency_key=$1,last_error='',updated_at=now()
	 FROM candidate c WHERE j.account_id=c.account_id
	 RETURNING j.account_id,j.owner_id::text,j.attempts,j.idempotency_key,c.policy_version`, "auto-ipv6-"+uuid.NewString()).Scan(&job.AccountID, &job.Owner, &job.Attempts, &job.Key, &job.Version)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = m.Change(ctx, Change{
		Kind: "mode", AccountID: job.AccountID, Mode: Strict, ExpectedVersion: job.Version,
		IdempotencyKey: job.Key, Acknowledge: true, EnableScheduling: true, automatic: true,
	}, "system:auto-ipv6")
	if err != nil {
		// Change 接受前的凭据错误也必须可见，不能只留下一个禁用按钮。
		if saveErr := m.finishEnrollmentFailure(ctx, job, safeError(err)); saveErr != nil {
			return saveErr
		}
	}
	return m.Reload(ctx)
}

func (m *Manager) reconcileAutoEnrollments(ctx context.Context) error {
	// 已成功启用的账号永远不因队列恢复而再次打开调度（包括用户后来暂停的账号）。
	if _, err := m.db.ExecContext(ctx, `UPDATE wayvia_egress_enrollments j SET state='completed',last_error='',updated_at=now()
	 FROM wayvia_account_egress_policies p WHERE p.owner_id=j.owner_id AND j.state NOT IN ('completed','cancelled')
	 AND p.current_mode='strict_ipv6' AND p.state='ready'
	 AND EXISTS(SELECT 1 FROM wayvia_egress_operations o WHERE o.id=p.operation_id AND o.phase='completed')`); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE wayvia_egress_enrollments j SET state='cancelled',last_error='',updated_at=now()
	 FROM wayvia_account_egress_policies p WHERE p.owner_id=j.owner_id AND j.state NOT IN ('completed','cancelled') AND p.state='retired'`); err != nil {
		return err
	}
	rows, err := m.db.QueryContext(ctx, `SELECT j.account_id,j.owner_id::text,j.attempts,j.idempotency_key,
	 p.state,COALESCE(o.phase,''),COALESCE(NULLIF(o.error,''),p.last_error)
	 FROM wayvia_egress_enrollments j JOIN wayvia_account_egress_policies p ON p.owner_id=j.owner_id
	 LEFT JOIN wayvia_egress_operations o ON o.idempotency_key=j.idempotency_key WHERE j.state='running'`)
	if err != nil {
		return err
	}
	type result struct {
		job                   enrollmentJob
		state, phase, message string
	}
	var results []result
	for rows.Next() {
		var r result
		if err = rows.Scan(&r.job.AccountID, &r.job.Owner, &r.job.Attempts, &r.job.Key, &r.state, &r.phase, &r.message); err != nil {
			rows.Close()
			return err
		}
		results = append(results, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range results {
		switch r.phase {
		case "accepted", "draining", "preparing":
			continue
		case "completed":
			_, err = m.db.ExecContext(ctx, `UPDATE wayvia_egress_enrollments SET state='completed',last_error='',updated_at=now() WHERE account_id=$1 AND idempotency_key=$2 AND state='running'`, r.job.AccountID, r.job.Key)
		default:
			if r.state == "credential_uncertain" {
				r.message = "EGRESS_CREDENTIAL_UNCERTAIN: 凭据刷新结果待确认，已停止自动重试"
			} else if r.phase == "" && r.message == "" {
				// 崩溃发生在持久领取后、创建操作前，且没有待确认的凭据标记。
				r.message = "EGRESS_AUTO_ENROLLMENT_INTERRUPTED: 自动配置尚未提交，将重新处理"
			} else if r.message == "" {
				r.message = "EGRESS_OPERATION_FAILED: 自动配置失败，请查看出口状态"
			}
			err = m.finishEnrollmentFailure(ctx, r.job, r.message)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func enrollmentRetryable(message string) bool {
	code, _, _ := strings.Cut(message, ":")
	switch code {
	case "EGRESS_PROBE_FAILED", "EGRESS_SOURCE_UNREADY", "EGRESS_NO_AAAA", "EGRESS_NOT_CONFIGURED",
		"EGRESS_PAUSED", "EGRESS_REFRESH_UNAVAILABLE", "EGRESS_JWKS_UNAVAILABLE", "EGRESS_AUTO_ENROLLMENT_INTERRUPTED", "EGRESS_RESTART_RECONCILE_REQUIRED":
		return true
	case "EGRESS_CONFLICT":
		return strings.Contains(message, "等待当前出口操作完成") || strings.Contains(message, "策略版本已变化") || strings.Contains(message, "账号资料在校验期间发生变化")
	}
	return false
}

func (m *Manager) finishEnrollmentFailure(ctx context.Context, job enrollmentJob, message string) error {
	state := "blocked"
	if enrollmentRetryable(message) {
		state = "retry"
	}
	delay := min(5*time.Second*time.Duration(1<<min(max(job.Attempts-1, 0), 6)), 5*time.Minute)
	_, err := m.db.ExecContext(ctx, `UPDATE wayvia_egress_enrollments SET state=$3,last_error=$4,next_attempt_at=$5,updated_at=now()
	 WHERE account_id=$1 AND idempotency_key=$2 AND state='running'`, job.AccountID, job.Key, state, message, time.Now().Add(delay))
	return err
}

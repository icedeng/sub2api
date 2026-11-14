package wayviaegress

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
)

type runtimeFault struct {
	route Route
	code  string
}

// ReportFailure 只报告连接故障；取消请求、HTTP 错误和凭据错误不触发换路或 RT 重放。
func (r Route) ReportFailure(ctx context.Context, err error) {
	if !r.Strict() || r.control || r.Purpose == "refresh" || r.Purpose == "reauth" || err == nil || ctx.Err() != nil || r.Check() != nil {
		return
	}
	code := strings.SplitN(safeError(err), ":", 2)[0]
	var ne net.Error
	if code != "EGRESS_SOURCE_CONFLICT" && code != "EGRESS_NO_AAAA" {
		if !errors.As(err, &ne) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return
		}
		code = "EGRESS_NETWORK_UNREADY"
	}
	if m := Default(); m != nil {
		m.mu.Lock()
		if m.faults == nil {
			m.faults = map[string]runtimeFault{}
		}
		if !m.closing {
			old, ok := m.faults[r.Owner]
			if !ok || old.route.PolicyVersion != r.PolicyVersion || recoveryRetryable(old.code) {
				m.faults[r.Owner] = runtimeFault{r, code}
			}
		}
		m.mu.Unlock()
	}
}

func (m *Manager) rememberFault(p Policy, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.faults == nil {
		m.faults = map[string]runtimeFault{}
	}
	m.faults[p.Owner] = runtimeFault{Route{AccountID: p.AccountID, Owner: p.Owner, PolicyVersion: p.Version, BindingVersion: p.BindingVersion}, reason}
}

func recoveryRetryable(message string) bool {
	code, _, _ := strings.Cut(message, ":")
	switch code {
	case "EGRESS_SOURCE_UNREADY", "EGRESS_NETWORK_UNREADY", "EGRESS_NO_AAAA", "EGRESS_PROBE_FAILED",
		"EGRESS_RESTART_PRECHECK_FAILED", "EGRESS_RESTART_RECONCILE_REQUIRED", "EGRESS_RECOVERY_DEFERRED", "EGRESS_PAUSED":
		return true
	case "EGRESS_CONFLICT":
		return strings.Contains(message, "等待当前出口操作完成") || strings.Contains(message, "账号资料在校验期间发生变化")
	}
	return false
}

// 调用方持有 Control；失败立即关门，队列只允许恢复同一 owner / binding。
func (m *Manager) blockForRecovery(ctx context.Context, p Policy, reason string) error {
	if !m.isCurrent(p) {
		return nil
	}
	m.closeOwner(p.Owner, false)
	state := "blocked"
	if recoveryRetryable(reason) {
		state = "retry"
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = execOne(ctx, tx, `UPDATE wayvia_account_egress_policies SET state='blocked',last_error=$3
	 WHERE owner_id=$1 AND policy_version=$2 AND state IN ('ready','blocked') AND current_mode='strict_ipv6' AND target_mode='strict_ipv6'`, p.Owner, p.Version, reason); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO wayvia_egress_recoveries(owner_id,binding_id,policy_version,state,last_error,next_attempt_at)
	 VALUES($1,$2,$3,$4,$5,now()+interval '5 seconds') ON CONFLICT(owner_id) DO UPDATE
	 SET binding_id=EXCLUDED.binding_id,policy_version=EXCLUDED.policy_version,state=EXCLUDED.state,
	 attempts=0,idempotency_key=NULL,last_error=EXCLUDED.last_error,next_attempt_at=EXCLUDED.next_attempt_at,updated_at=now()`, p.Owner, p.BindingID, p.Version, state, reason)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return m.Reload(ctx)
}

func (m *Manager) recoverRuntimeOnce(ctx context.Context) error {
	Control.Lock()
	m.mu.Lock()
	available := !m.closing && m.initError == "" && m.lock != nil
	faults := m.faults
	m.faults = nil
	m.mu.Unlock()
	if !available {
		Control.Unlock()
		return nil
	}
	if err := m.Reload(ctx); err != nil {
		m.mu.Lock()
		if m.faults == nil {
			m.faults = map[string]runtimeFault{}
		}
		for owner, f := range faults {
			m.faults[owner] = f
		}
		m.mu.Unlock()
		Control.Unlock()
		return err
	}
	for _, fault := range faults {
		m.mu.Lock()
		_, p, e := m.rootLocked(fault.route.AccountID)
		valid := e == nil && p.State == "ready" && p.Version == fault.route.PolicyVersion && p.BindingVersion == fault.route.BindingVersion
		m.mu.Unlock()
		if valid {
			if err := m.blockForRecovery(ctx, p, fault.code); err != nil {
				m.rememberFault(p, fault.code)
				Control.Unlock()
				return err
			}
		}
	}
	err := m.reconcileRecoveries(ctx)
	Control.Unlock()
	if err != nil {
		return err
	}
	var job recoveryJob
	err = m.db.QueryRowContext(ctx, `WITH candidate AS (
	 SELECT j.owner_id FROM wayvia_egress_recoveries j
	 JOIN wayvia_account_egress_policies p ON p.owner_id=j.owner_id
	 JOIN accounts a ON a.id=p.root_account_id
	 WHERE j.state='retry' AND j.next_attempt_at<=now() AND j.policy_version=p.policy_version AND j.binding_id=p.active_binding_id
	 AND p.current_mode='strict_ipv6' AND p.target_mode='strict_ipv6' AND p.state='blocked'
	 AND a.status='active' AND a.schedulable AND a.deleted_at IS NULL
	 AND EXISTS(SELECT 1 FROM settings WHERE key='wayvia_ipv6_egress' AND (value::jsonb->>'enabled')::boolean)
	 AND NOT EXISTS(SELECT 1 FROM wayvia_egress_operations WHERE phase IN ('accepted','draining','preparing'))
	 ORDER BY j.next_attempt_at LIMIT 1 FOR UPDATE OF j SKIP LOCKED
	) UPDATE wayvia_egress_recoveries j SET state='running',attempts=attempts+1,idempotency_key=$1,updated_at=now()
	 FROM candidate c WHERE j.owner_id=c.owner_id RETURNING j.owner_id::text,j.policy_version,j.attempts,j.idempotency_key,
	 (SELECT root_account_id FROM wayvia_account_egress_policies WHERE owner_id=j.owner_id)`, "recover-ipv6-"+uuid.NewString()).Scan(&job.Owner, &job.Version, &job.Attempts, &job.Key, &job.AccountID)
	if err == sql.ErrNoRows {
		return m.Reload(ctx)
	}
	if err != nil {
		return err
	}
	_, err = m.Change(ctx, Change{Kind: "recover", AccountID: job.AccountID, ExpectedVersion: job.Version, IdempotencyKey: job.Key, Acknowledge: true, recovering: true}, "system:recover-ipv6")
	if err != nil {
		if e := m.finishRecoveryFailure(ctx, job, safeError(err)); e != nil {
			return e
		}
	}
	return m.Reload(ctx)
}

type recoveryJob struct {
	Owner     string
	Version   int64
	Attempts  int
	Key       string
	AccountID int64
}

func (m *Manager) finishRecoveryFailure(ctx context.Context, job recoveryJob, message string) error {
	state := "blocked"
	if recoveryRetryable(message) {
		state = "retry"
	}
	delay := min(5*time.Second*time.Duration(1<<min(max(job.Attempts-1, 0), 6)), 5*time.Minute)
	_, err := m.db.ExecContext(ctx, `UPDATE wayvia_egress_recoveries j SET state=$3,last_error=$4,next_attempt_at=$5,
	 policy_version=p.policy_version,updated_at=now() FROM wayvia_account_egress_policies p
	 WHERE j.owner_id=$1 AND j.idempotency_key=$2 AND j.state='running' AND p.owner_id=j.owner_id
	 AND p.active_binding_id=j.binding_id AND p.state NOT IN ('credential_uncertain','retired')
 AND (p.policy_version=j.policy_version OR EXISTS(SELECT 1 FROM wayvia_egress_operations o WHERE o.id=p.operation_id AND o.idempotency_key=j.idempotency_key AND o.kind='recover'))`, job.Owner, job.Key, state, message, time.Now().Add(delay))
	return err
}

func (m *Manager) reconcileRecoveries(ctx context.Context) error {
	// 人工更换地址/模式、重新授权及其他操作取得所有权后，旧任务只能取消。
	_, err := m.db.ExecContext(ctx, `UPDATE wayvia_egress_recoveries j SET state=CASE WHEN p.state='credential_uncertain' THEN 'blocked' ELSE 'cancelled' END,
	 last_error=CASE WHEN p.state='credential_uncertain' THEN 'EGRESS_CREDENTIAL_UNCERTAIN' ELSE '' END,updated_at=now()
	 FROM wayvia_account_egress_policies p WHERE p.owner_id=j.owner_id AND j.state IN ('retry','running','blocked','completed') AND
	 (p.active_binding_id IS DISTINCT FROM j.binding_id OR p.current_mode<>'strict_ipv6' OR p.target_mode<>'strict_ipv6' OR p.state IN ('retired','credential_uncertain')
	 OR (p.policy_version<>j.policy_version AND NOT EXISTS(SELECT 1 FROM wayvia_egress_operations o WHERE o.id=p.operation_id AND o.idempotency_key=j.idempotency_key AND o.kind='recover')))`)
	if err != nil {
		return err
	}
	rows, err := m.db.QueryContext(ctx, `SELECT j.owner_id::text,j.policy_version,j.attempts,j.idempotency_key,COALESCE(o.phase,''),COALESCE(o.error,'')
	 FROM wayvia_egress_recoveries j LEFT JOIN wayvia_egress_operations o ON o.idempotency_key=j.idempotency_key WHERE j.state='running'`)
	if err != nil {
		return err
	}
	type result struct {
		job            recoveryJob
		phase, message string
	}
	var results []result
	for rows.Next() {
		var r result
		if err = rows.Scan(&r.job.Owner, &r.job.Version, &r.job.Attempts, &r.job.Key, &r.phase, &r.message); err != nil {
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
			_, err = m.db.ExecContext(ctx, `UPDATE wayvia_egress_recoveries SET state='completed',last_error='',recovered_at=now(),updated_at=now() WHERE owner_id=$1 AND idempotency_key=$2 AND state='running'`, r.job.Owner, r.job.Key)
		default:
			if r.message == "" {
				r.message = "EGRESS_RESTART_RECONCILE_REQUIRED"
			}
			err = m.finishRecoveryFailure(ctx, r.job, r.message)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

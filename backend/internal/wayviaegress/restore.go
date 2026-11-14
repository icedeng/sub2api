package wayviaegress

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

type HistoricalBinding struct {
	ID      string `json:"binding_id"`
	Source  string `json:"source_ipv6"`
	Version int64  `json:"binding_version"`
	State   string `json:"state"`
}
type RetiredAccount struct {
	ID        int64  `json:"account_id"`
	Name      string `json:"name"`
	Owner     string `json:"owner_id"`
	Version   int64  `json:"policy_version"`
	BindingID string `json:"binding_id"`
}

// History 只读历史地址；永远不回收其他owner的地址。
func (m *Manager) History(ctx context.Context, id int64) ([]HistoricalBinding, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT b.id::text,host(b.ipv6),b.binding_version,b.state FROM wayvia_account_egress_bindings b JOIN wayvia_account_egress_policies p ON p.owner_id=b.owner_id WHERE p.root_account_id=$1 ORDER BY b.binding_version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoricalBinding{}
	for rows.Next() {
		var b HistoricalBinding
		if err = rows.Scan(&b.ID, &b.Source, &b.Version, &b.State); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (m *Manager) RetiredAccounts(ctx context.Context) ([]RetiredAccount, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT a.id,a.name,p.owner_id::text,p.policy_version,COALESCE(p.active_binding_id::text,'') FROM accounts a JOIN wayvia_account_egress_policies p ON p.root_account_id=a.id WHERE p.state='retired' AND a.deleted_at IS NOT NULL AND p.identity_key IS NOT NULL ORDER BY a.id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RetiredAccount{}
	for rows.Next() {
		var a RetiredAccount
		if err = rows.Scan(&a.ID, &a.Name, &a.Owner, &a.Version, &a.BindingID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// 调用方持有Control。恢复原软删除记录，保留owner/绑定；硬删除必须用一致备份恢复。
// 不自动恢复影子、分组或调度，凭据仍需关联授权确认。
func (m *Manager) restoreAccount(ctx context.Context, c Change, actor, hash string) (Operation, error) {
	if !c.Acknowledge || c.AccountID <= 0 {
		return Operation{}, Conflict("恢复需要账号ID及影响确认")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	var deleted sql.NullTime
	var platform, kind string
	var parent sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT platform,type,parent_account_id,deleted_at FROM accounts WHERE id=$1 FOR UPDATE`, c.AccountID).Scan(&platform, &kind, &parent, &deleted); err != nil {
		return Operation{}, err
	}
	if !deleted.Valid || platform != "openai" || kind != "oauth" || parent.Valid {
		return Operation{}, Conflict("只恢复原软删除OpenAI OAuth母账号")
	}
	var owner, state, identity string
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT owner_id::text,state,policy_version,COALESCE(identity_key,'') FROM wayvia_account_egress_policies WHERE root_account_id=$1 FOR UPDATE`, c.AccountID).Scan(&owner, &state, &version, &identity); err != nil {
		return Operation{}, err
	}
	if state != "retired" || version != c.ExpectedVersion || identity == "" {
		return Operation{}, Conflict("原owner不可恢复或策略版本已变化")
	}
	op := Operation{ID: uuid.NewString(), Owner: owner, Kind: "restore-account", Target: "credential_uncertain", Phase: "completed", CreatedAt: time.Now()}
	mark := pending{Operation: op, Owners: []string{owner}}
	if err = durableJSON(m.pendingPath(op.ID), mark); err != nil {
		return Operation{}, err
	}
	m.closeOwner(owner, true)
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET deleted_at=NULL,status='disabled',schedulable=false,updated_at=now() WHERE id=$1`, c.AccountID); err != nil {
		return Operation{}, err
	}
	if err = execOne(ctx, tx, `UPDATE wayvia_account_egress_policies SET state='credential_uncertain',policy_version=policy_version+1,operation_id=$2,last_error='恢复账号后必须重新授权' WHERE owner_id=$1`, owner, op.ID); err != nil {
		return Operation{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO wayvia_egress_operations(id,idempotency_key,request_hash,owner_id,kind,target,expected_version,actor,phase,finished_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'completed',now())`, op.ID, c.IdempotencyKey, hash, owner, op.Kind, op.Target, version, actor); err != nil {
		return Operation{}, err
	}
	if err = publishCredentialChange(ctx, tx, c.AccountID); err != nil {
		return Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Operation{}, err
	}
	if err = m.Reload(ctx); err != nil {
		return Operation{}, err
	}
	// credential_uncertain已持久化，即使清理失败也不会开放。
	if err = m.clearOwnerMarkers(mark); err != nil {
		return Operation{}, err
	}
	return m.Operation(ctx, op.ID)
}

func publishCredentialChange(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload) VALUES('account_changed',$1,NULL,NULL)`, id)
	return err
}

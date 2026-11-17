package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/wayviahealth"
	"github.com/lib/pq"
)

type WayviaHealthRun struct {
	ID                  string           `json:"id"`
	BatchID             string           `json:"batch_id"`
	AccountID           int64            `json:"account_id"`
	CredentialAccountID int64            `json:"credential_account_id"`
	Source              string           `json:"source"`
	ConfigVersion       int64            `json:"config_version"`
	CredentialVersion   string           `json:"-"`
	Model               string           `json:"model"`
	Protocol            string           `json:"protocol"`
	ContentSource       string           `json:"content_source"`
	RenderedContent     string           `json:"rendered_content"`
	State               string           `json:"state"`
	ErrorClass          string           `json:"error_class"`
	ErrorCode           string           `json:"error_code"`
	HTTPStatus          int              `json:"http_status"`
	Reason              string           `json:"reason"`
	StateChanged        bool             `json:"state_changed"`
	Attempt             int              `json:"attempt"`
	Options             healthRunOptions `json:"-"`
	CreatedAt           time.Time        `json:"created_at"`
	StartedAt           *time.Time       `json:"started_at"`
	FinishedAt          *time.Time       `json:"finished_at"`
	LatencyMs           int64            `json:"latency_ms"`
	NotBefore           time.Time        `json:"not_before"`
}

type healthRunOptions struct {
	ModelOverride string              `json:"model_override,omitempty"`
	ScheduleKey   string              `json:"schedule_key,omitempty"`
	TestContent   *string             `json:"test_content,omitempty"`
	PlanID        int64               `json:"plan_id,omitempty"`
	FaultAt       *time.Time          `json:"fault_at,omitempty"`
	Policy        wayviahealth.Policy `json:"policy"`
}

const healthRunColumns = `id::text,batch_id,account_id,credential_account_id,source,config_version,credential_version,model,protocol,content_source,rendered_content,state,error_class,error_code,http_status,reason,state_changed,attempt,options,created_at,started_at,finished_at,latency_ms,not_before`

func scanHealthRun(row interface{ Scan(...any) error }) (*WayviaHealthRun, error) {
	r := &WayviaHealthRun{}
	var options []byte
	err := row.Scan(&r.ID, &r.BatchID, &r.AccountID, &r.CredentialAccountID, &r.Source, &r.ConfigVersion, &r.CredentialVersion, &r.Model, &r.Protocol, &r.ContentSource, &r.RenderedContent, &r.State, &r.ErrorClass, &r.ErrorCode, &r.HTTPStatus, &r.Reason, &r.StateChanged, &r.Attempt, &options, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.LatencyMs, &r.NotBefore)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(options, &r.Options)
	return r, err
}

func (s *WayviaHealthService) GetRun(ctx context.Context, id string) (*WayviaHealthRun, error) {
	return scanHealthRun(s.db.QueryRowContext(ctx, `SELECT `+healthRunColumns+` FROM wayvia_account_test_runs WHERE id=$1`, id))
}

func (s *WayviaHealthService) Runs(ctx context.Context, accountID int64, batch, source string, limit int, before *time.Time, beforeIDs ...string) ([]*WayviaHealthRun, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	var beforeID *string
	if len(beforeIDs) > 0 && beforeIDs[0] != "" {
		beforeID = &beforeIDs[0]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+healthRunColumns+` FROM wayvia_account_test_runs WHERE ($1::bigint=0 OR account_id=$1) AND ($2='' OR batch_id=$2) AND ($3='' OR source=$3) AND ($5::timestamptz IS NULL OR created_at<$5 OR (created_at=$5 AND id<$6::uuid)) ORDER BY created_at DESC,id DESC LIMIT $4`, accountID, batch, source, limit, before, beforeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*WayviaHealthRun{}
	for rows.Next() {
		r, err := scanHealthRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *WayviaHealthService) Summaries(ctx context.Context, ids []int64) ([]*WayviaHealthRun, error) {
	if len(ids) > 200 {
		return nil, infraerrors.BadRequest("HEALTH_SELECTION_TOO_LARGE", "摘要一次最多 200 个账号")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+healthRunColumns+` FROM (SELECT DISTINCT ON(account_id) * FROM wayvia_account_test_runs WHERE account_id=ANY($1) ORDER BY account_id,created_at DESC) t`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*WayviaHealthRun{}
	for rows.Next() {
		r, err := scanHealthRun(rows)
		if err != nil {
			return nil, err
		}
		r.RenderedContent = ""
		out = append(out, r)
	}
	return out, rows.Err()
}

type healthRunDB interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *WayviaHealthService) insertRun(ctx context.Context, r *WayviaHealthRun) (*WayviaHealthRun, error) {
	return insertHealthRun(ctx, s.db, r)
}

func insertHealthRun(ctx context.Context, db healthRunDB, r *WayviaHealthRun) (*WayviaHealthRun, error) {
	options, err := json.Marshal(r.Options)
	if err != nil {
		return nil, err
	}
	if r.ID == "" {
		r.ID = newHealthID()
	}
	if r.NotBefore.IsZero() {
		r.NotBefore = time.Now()
	}
	row := db.QueryRowContext(ctx, `INSERT INTO wayvia_account_test_runs(id,batch_id,account_id,credential_account_id,source,config_version,credential_version,model,protocol,content_source,rendered_content,state,error_class,error_code,http_status,reason,attempt,options,not_before,finished_at,state_changed)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,CASE WHEN $12 NOT IN ('queued','running') THEN NOW() ELSE NULL END,$20) ON CONFLICT DO NOTHING RETURNING `+healthRunColumns, r.ID, r.BatchID, r.AccountID, r.CredentialAccountID, r.Source, r.ConfigVersion, r.CredentialVersion, r.Model, r.Protocol, r.ContentSource, r.RenderedContent, r.State, r.ErrorClass, r.ErrorCode, r.HTTPStatus, r.Reason, r.Attempt, string(options), r.NotBefore, r.StateChanged)
	result, err := scanHealthRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		var existing string
		_ = db.QueryRowContext(ctx, `SELECT id::text FROM wayvia_account_test_runs WHERE credential_account_id=$1 AND state IN ('queued','running') LIMIT 1`, r.CredentialAccountID).Scan(&existing)
		return nil, infraerrors.Conflict("HEALTH_TEST_ACTIVE", "同一凭据或同一计划周期已有检测任务："+existing)
	}
	return result, err
}

func (s *WayviaHealthService) newRun(ctx context.Context, id int64, source, batch, model string, content *string) (*WayviaHealthRun, error) {
	a, owner, version, err := s.snapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	cfg, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	resolved, _, _ := wayviahealth.Resolve(cfg, a.Platform, a.Type, openai.DefaultTestModel, healthPolicy(a), nil, content)
	if model != "" {
		resolved = model
	}
	r := &WayviaHealthRun{ID: newHealthID(), BatchID: batch, AccountID: id, CredentialAccountID: owner.ID, Source: source, ConfigVersion: cfg.Version, CredentialVersion: version, Model: resolved, Protocol: healthProtocol(a), State: "queued", Options: healthRunOptions{ModelOverride: model, TestContent: content, Policy: healthPolicy(a)}}
	if !healthSupported(a) || !healthSupported(owner) {
		r.State = "skipped"
		r.ErrorClass = "unsupported"
		r.Reason = "不支持该检测类型：首版仅支持普通 OpenAI OAuth / API Key 文本检测"
	}
	if isOpenAIImageModel(a.GetMappedModel(resolved)) || !a.IsModelSupported(resolved) {
		r.State = "skipped"
		r.ErrorClass = "configuration"
		r.Reason = "配置模型不受账号支持或不是文本模型"
	}
	return r, nil
}

type WayviaHealthBatchResult struct {
	BatchID string             `json:"batch_id"`
	Runs    []*WayviaHealthRun `json:"runs"`
}

func (s *WayviaHealthService) Batch(ctx context.Context, selection WayviaHealthSelection, model string, content *string) (*WayviaHealthBatchResult, error) {
	if content != nil {
		if err := wayviahealth.Validate(*content); err != nil {
			return nil, infraerrors.BadRequest("HEALTH_CONTENT_INVALID", err.Error())
		}
	}
	ids, err := s.Freeze(ctx, selection)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, infraerrors.BadRequest("HEALTH_SELECTION_EMPTY", "请选择账号")
	}
	result := &WayviaHealthBatchResult{BatchID: newHealthID(), Runs: []*WayviaHealthRun{}}
	// 先检查每个账号的读取权限和参数，再创建任务。
	for _, id := range ids {
		r, err := s.newRun(ctx, id, "batch", result.BatchID, model, content)
		if err != nil {
			return nil, err
		}
		result.Runs = append(result.Runs, r)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i, r := range result.Runs {
		created, err := insertHealthRun(ctx, tx, r)
		if err != nil {
			if infraerrors.Reason(err) != "HEALTH_TEST_ACTIVE" {
				return nil, err
			}
			r.State = "skipped"
			r.ErrorClass = "deduplicated"
			r.Reason = "同一凭据已有活动任务，本批不重复发送"
			created, err = insertHealthRun(ctx, tx, r)
			if err != nil {
				return nil, err
			}
		}
		result.Runs[i] = created
	}
	return result, tx.Commit()
}

// claim 使用数据库事务串行计算总并发；所有入口共享，不依赖单进程互斥锁。
func (s *WayviaHealthService) claim(ctx context.Context, id string, limit, timeout int) (*WayviaHealthRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(78251601)`); err != nil {
		return nil, err
	}
	// 与设置保存共用锁，不能按保存前的并发快照认领新任务。
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, wayviahealth.SettingsKey).Scan(&raw); err == nil {
		cfg, decodeErr := decodeHealthSettings([]byte(raw))
		if decodeErr != nil {
			return nil, decodeErr
		}
		limit, timeout = cfg.Concurrency, cfg.TimeoutSeconds
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM wayvia_account_test_runs WHERE state='running'`).Scan(&count); err != nil {
		return nil, err
	}
	if count >= limit {
		return nil, infraerrors.Conflict("HEALTH_CONCURRENCY_BUSY", "检测并发已满，请稍后重试")
	}
	r, err := scanHealthRun(tx.QueryRowContext(ctx, `UPDATE wayvia_account_test_runs SET state='running',started_at=NOW(),lease_until=NOW()+$2*INTERVAL '1 second' WHERE id=$1 AND state='queued' AND not_before<=NOW() RETURNING `+healthRunColumns, id, timeout+30))
	if err != nil {
		return nil, err
	}
	return r, tx.Commit()
}

func (s *WayviaHealthService) finishRun(ctx context.Context, r *WayviaHealthRun) error {
	_, err := s.db.ExecContext(ctx, `UPDATE wayvia_account_test_runs SET state=$2,error_class=$3,error_code=$4,http_status=$5,reason=$6,state_changed=$7,finished_at=NOW(),latency_ms=CASE WHEN started_at IS NULL THEN 0 ELSE GREATEST(0,EXTRACT(EPOCH FROM (NOW()-started_at))*1000)::bigint END,lease_until=NULL WHERE id=$1 AND state IN ('queued','running')`, r.ID, r.State, r.ErrorClass, r.ErrorCode, r.HTTPStatus, r.Reason, r.StateChanged)
	return err
}

func (s *WayviaHealthService) planContent(ctx context.Context, id int64) (*string, error) {
	if id == 0 {
		return nil, nil
	}
	var content *string
	err := s.db.QueryRowContext(ctx, `SELECT test_content FROM wayvia_health_plan_content WHERE plan_id=$1`, id).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return content, err
}

func (s *WayviaHealthService) recentSuccess(ctx context.Context, r *WayviaHealthRun, since time.Time) bool {
	var at time.Time
	err := s.db.QueryRowContext(ctx, `SELECT success_at FROM wayvia_health_evidence WHERE credential_account_id=$1 AND credential_version=$2 AND model=$3 AND protocol=$4`, r.CredentialAccountID, r.CredentialVersion, r.Model, r.Protocol).Scan(&at)
	return err == nil && at.After(since)
}

func (s *WayviaHealthService) saveSuccess(ctx context.Context, r *WayviaHealthRun) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO wayvia_health_evidence(credential_account_id,credential_version,model,protocol,success_at) VALUES($1,$2,$3,$4,NOW()) ON CONFLICT(credential_account_id,model,protocol) DO UPDATE SET credential_version=EXCLUDED.credential_version,success_at=EXCLUDED.success_at WHERE wayvia_health_evidence.success_at<EXCLUDED.success_at`, r.CredentialAccountID, r.CredentialVersion, r.Model, r.Protocol)
	return err
}

func healthRunBusy(err error) bool {
	return err != nil && (infraerrors.Reason(err) == "HEALTH_TEST_ACTIVE" || infraerrors.Reason(err) == "HEALTH_CONCURRENCY_BUSY" || errors.Is(err, sql.ErrNoRows))
}

func healthConfigError(message string) error {
	return infraerrors.BadRequest("HEALTH_CONFIGURATION", fmt.Sprint(message))
}

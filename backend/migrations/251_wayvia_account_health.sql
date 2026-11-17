-- 仅新增能力；不修改旧账号、不创建计划、不启用主动探测。
CREATE TABLE IF NOT EXISTS wayvia_account_test_runs (
    id UUID PRIMARY KEY,
    batch_id TEXT NOT NULL DEFAULT '',
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    credential_account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    config_version BIGINT NOT NULL DEFAULT 0,
    credential_version TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    protocol TEXT NOT NULL DEFAULT 'responses',
    content_source TEXT NOT NULL DEFAULT '',
    rendered_content TEXT NOT NULL DEFAULT '' CHECK (octet_length(rendered_content) <= 2048),
    state TEXT NOT NULL CHECK (state IN ('queued','running','success','failed','skipped','cancelled')),
    error_class TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    http_status INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    state_changed BOOLEAN NOT NULL DEFAULT FALSE,
    attempt INTEGER NOT NULL DEFAULT 0,
    options JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    latency_ms BIGINT NOT NULL DEFAULT 0,
    not_before TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_until TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS wayvia_health_active_credential ON wayvia_account_test_runs(credential_account_id) WHERE state IN ('queued','running');
CREATE INDEX IF NOT EXISTS wayvia_health_due ON wayvia_account_test_runs(not_before) WHERE state = 'queued';
CREATE INDEX IF NOT EXISTS wayvia_health_history ON wayvia_account_test_runs(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS wayvia_health_batch ON wayvia_account_test_runs(batch_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS wayvia_health_scheduled_once ON wayvia_account_test_runs((options->>'schedule_key')) WHERE source='scheduled' AND options->>'schedule_key' IS NOT NULL;
CREATE TABLE IF NOT EXISTS wayvia_health_plan_content (
    plan_id BIGINT PRIMARY KEY REFERENCES scheduled_test_plans(id) ON DELETE CASCADE,
    test_content TEXT CHECK (octet_length(test_content) <= 16384)
);
-- 业务成功证据按凭据、模型、协议保存，不从 last_used_at 推断。
CREATE TABLE IF NOT EXISTS wayvia_health_evidence (
    credential_account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    credential_version TEXT NOT NULL,
    model TEXT NOT NULL,
    protocol TEXT NOT NULL,
    success_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (credential_account_id, model, protocol)
);

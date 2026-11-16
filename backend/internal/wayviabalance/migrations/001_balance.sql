-- 余额数据独立于 accounts.extra，避免刷新余额触发账号调度缓存和旧账号表单覆盖。
CREATE TABLE wayvia_upstream_balances (
    account_id bigint PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    config jsonb NOT NULL CHECK (jsonb_typeof(config) = 'object'),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    identity text NOT NULL,
    query_ciphertext text NOT NULL DEFAULT '',
    query_identity text NOT NULL DEFAULT '',
    snapshot jsonb CHECK (snapshot IS NULL OR jsonb_typeof(snapshot) = 'object'),
    last_attempt_at timestamptz,
    next_retry_at timestamptz,
    error_code text NOT NULL DEFAULT '',
    failure_count integer NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
    detected_provider text NOT NULL DEFAULT '',
    detected_path text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX wayvia_upstream_balances_due_idx
    ON wayvia_upstream_balances (next_retry_at NULLS FIRST, account_id)
    WHERE config -> 'enabled' = 'true'::jsonb
      AND config -> 'auto_refresh' = 'true'::jsonb;

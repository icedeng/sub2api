-- 运行恢复只引用原绑定；领取和退避状态在重启后保留。
CREATE TABLE wayvia_egress_recoveries (
 owner_id uuid PRIMARY KEY REFERENCES wayvia_account_egress_policies(owner_id),
 binding_id uuid NOT NULL REFERENCES wayvia_account_egress_bindings(id),
 policy_version bigint NOT NULL,
 state text NOT NULL CHECK(state IN ('retry','running','blocked','completed','cancelled')),
 attempts integer NOT NULL DEFAULT 0,
 idempotency_key text,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 last_error text NOT NULL DEFAULT '',
 recovered_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX wayvia_recovery_queue ON wayvia_egress_recoveries(next_attempt_at) WHERE state='retry';

-- 仅接续旧版明确由运行检查阻断的账号，不接管人工操作失败或凭据不确定状态。
INSERT INTO wayvia_egress_recoveries(owner_id,binding_id,policy_version,state,last_error)
 SELECT owner_id,active_binding_id,policy_version,'retry',last_error
 FROM wayvia_account_egress_policies WHERE current_mode='strict_ipv6' AND target_mode='strict_ipv6'
 AND state='blocked' AND active_binding_id IS NOT NULL
 AND last_error IN ('EGRESS_SOURCE_UNREADY','EGRESS_RESTART_PRECHECK_FAILED');

-- 有界连接证据，既不保存 Token，也不保存请求路径、参数或正文。
CREATE TABLE wayvia_egress_evidence (
 id bigserial PRIMARY KEY,
 owner_id uuid NOT NULL REFERENCES wayvia_account_egress_policies(owner_id),
 account_id bigint NOT NULL,
 occurred_at timestamptz NOT NULL,
 request_id uuid NOT NULL,
 connection_id text NOT NULL,
 purpose text NOT NULL,
 protocol text NOT NULL,
 event text NOT NULL,
 source_ipv6 text NOT NULL,
 remote_address text NOT NULL,
 target_host text NOT NULL,
 reused boolean NOT NULL,
 policy_version bigint NOT NULL,
 binding_version bigint NOT NULL,
 http_status integer NOT NULL,
 error_code text NOT NULL
);
CREATE INDEX wayvia_evidence_owner_recent ON wayvia_egress_evidence(owner_id,id DESC);

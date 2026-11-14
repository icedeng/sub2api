-- 导入事务内写入持久任务；后台只处理首次固定出口，不重新打开已有账号的手动暂停。
CREATE TABLE wayvia_egress_enrollments (
 account_id bigint PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
 owner_id uuid UNIQUE NOT NULL REFERENCES wayvia_account_egress_policies(owner_id) ON DELETE CASCADE,
 state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','running','retry','blocked','completed','cancelled')),
 attempts integer NOT NULL DEFAULT 0,
 idempotency_key text,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 last_error text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX wayvia_enrollment_queue ON wayvia_egress_enrollments(next_attempt_at,account_id)
 WHERE state IN ('queued','retry');

CREATE FUNCTION wayvia_queue_enrollment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.require_ipv6 AND NEW.root_account_id IS NOT NULL AND NEW.current_mode='legacy'
  AND NEW.state='pending_enrollment' AND NEW.active_binding_id IS NULL THEN
  INSERT INTO wayvia_egress_enrollments(account_id,owner_id) VALUES(NEW.root_account_id,NEW.owner_id)
   ON CONFLICT(account_id) DO NOTHING;
 END IF;
 IF TG_OP='UPDATE' AND NEW.require_ipv6 AND NEW.current_mode='legacy'
  AND OLD.state='credential_uncertain' AND NEW.state='pending_enrollment' THEN
  UPDATE wayvia_egress_enrollments SET state='queued',attempts=0,next_attempt_at=now(),last_error='',updated_at=now()
   WHERE owner_id=NEW.owner_id AND state IN ('blocked','retry');
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER wayvia_queue_new_enrollment AFTER INSERT OR UPDATE ON wayvia_account_egress_policies
 FOR EACH ROW EXECUTE FUNCTION wayvia_queue_enrollment();

-- 修正凭据或取消代理后自动重试；不会重新使用结果不确定的 RT，也不修改已完成任务。
CREATE FUNCTION wayvia_requeue_enrollment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.deleted_at IS NULL AND NEW.status='active' AND
  (NEW.credentials IS DISTINCT FROM OLD.credentials OR NEW.proxy_id IS DISTINCT FROM OLD.proxy_id
   OR NEW.status IS DISTINCT FROM OLD.status) THEN
  UPDATE wayvia_egress_enrollments j SET state='queued',attempts=0,next_attempt_at=now(),last_error='',updated_at=now()
   FROM wayvia_account_egress_policies p WHERE j.account_id=NEW.id AND p.owner_id=j.owner_id
   AND p.current_mode='legacy' AND p.state NOT IN ('credential_uncertain','retired')
   AND j.state IN ('blocked','retry');
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER wayvia_requeue_updated_enrollment AFTER UPDATE OF credentials,proxy_id,status ON accounts
 FOR EACH ROW EXECUTE FUNCTION wayvia_requeue_enrollment();

-- 接续升级前已导入但尚未开始启用的账号；不触碰旧 legacy 账号或已有固定绑定。
INSERT INTO wayvia_egress_enrollments(account_id,owner_id)
 SELECT root_account_id,owner_id FROM wayvia_account_egress_policies
 WHERE require_ipv6 AND root_account_id IS NOT NULL AND current_mode='legacy'
 AND state='pending_enrollment' AND active_binding_id IS NULL;

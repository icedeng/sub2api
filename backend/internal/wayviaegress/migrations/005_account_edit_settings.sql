-- 普通账号编辑可调整配置，不能替换 OAuth 认证数据。
-- 保留 001–004 的校验和；只收窄凭据比较范围，其余身份/出口/调度保护与 002 一致。
-- 基于数据库当前行比较，旧表单携带的过期令牌仍会被拒绝，不开启 controlled 旁路。
CREATE OR REPLACE FUNCTION wayvia_account_policy_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE p wayvia_account_egress_policies%ROWTYPE;
 parent accounts%ROWTYPE;
 protected boolean := false;
 new_oauth boolean := false;
 -- 仅普通编辑表单的配置字段；未知字段默认按认证数据保护。
 -- Token、身份、过期时间和网络地址仍只能由受控流程修改。
 editable_fields constant text[] := ARRAY[
  'model_mapping','compact_model_mapping','plan_type','intercept_warmup_requests',
  'account_scheduling_threshold','temp_unschedulable_enabled','temp_unschedulable_rules'
 ];
BEGIN
 IF TG_OP <> 'INSERT' THEN
  SELECT * INTO p FROM wayvia_account_egress_policies WHERE root_account_id=COALESCE(OLD.parent_account_id,OLD.id) FOR UPDATE;
  protected := FOUND AND (p.current_mode='strict_ipv6' OR p.target_mode='strict_ipv6' OR p.state='credential_uncertain');
  IF TG_OP='UPDATE' AND p.require_ipv6 AND (
   NEW.platform IS DISTINCT FROM OLD.platform OR NEW.type IS DISTINCT FROM OLD.type
   OR NEW.parent_account_id IS DISTINCT FROM OLD.parent_account_id) THEN
   RAISE EXCEPTION 'EGRESS_FIXED_IPV6_REQUIRED';
  END IF;
  IF protected THEN
   IF TG_OP='DELETE' THEN
    IF p.state<>'retired' THEN RAISE EXCEPTION 'EGRESS_RETIRE_REQUIRED'; END IF;
   ELSE
    IF NEW.deleted_at IS DISTINCT FROM OLD.deleted_at AND p.state<>'retired' THEN RAISE EXCEPTION 'EGRESS_RETIRE_REQUIRED'; END IF;
    IF NEW.platform IS DISTINCT FROM OLD.platform OR NEW.type IS DISTINCT FROM OLD.type
     OR NEW.parent_account_id IS DISTINCT FROM OLD.parent_account_id OR NEW.proxy_id IS DISTINCT FROM OLD.proxy_id
     THEN RAISE EXCEPTION 'EGRESS_IDENTITY_LOCKED'; END IF;
    IF NEW.credentials IS DISTINCT FROM OLD.credentials AND OLD.parent_account_id IS NULL
     AND (
      jsonb_typeof(NEW.credentials) IS DISTINCT FROM 'object'
      OR jsonb_typeof(OLD.credentials) IS DISTINCT FROM 'object'
      OR (NEW.credentials - editable_fields) IS DISTINCT FROM (OLD.credentials - editable_fields)
     )
     AND COALESCE(current_setting('wayvia.credential_write',true),'') <> 'controlled' THEN
     RAISE EXCEPTION 'EGRESS_CONTROLLED_CREDENTIAL_WRITE_REQUIRED';
    END IF;
    IF p.identity_key IS NOT NULL AND NEW.parent_account_id IS NULL
     AND NEW.credentials->>'chatgpt_user_id' IS DISTINCT FROM p.identity_key THEN RAISE EXCEPTION 'EGRESS_IDENTITY_LOCKED'; END IF;
   END IF;
  END IF;
 END IF;
 IF TG_OP='DELETE' THEN
  UPDATE wayvia_account_egress_policies SET state='retired',policy_version=policy_version+1 WHERE root_account_id=OLD.id;
  RETURN OLD;
 END IF;
 IF NEW.platform='openai' AND NEW.type='oauth' AND NEW.deleted_at IS NULL THEN
  new_oauth := TG_OP='INSERT';
  IF TG_OP='UPDATE' THEN
   new_oauth := OLD.platform<>'openai' OR OLD.type<>'oauth' OR NEW.parent_account_id IS DISTINCT FROM OLD.parent_account_id;
  END IF;
  IF NEW.parent_account_id IS NOT NULL THEN
   SELECT * INTO parent FROM accounts WHERE id=NEW.parent_account_id AND deleted_at IS NULL FOR SHARE;
   IF NOT FOUND OR parent.id=NEW.id OR parent.parent_account_id IS NOT NULL OR parent.platform<>'openai' OR parent.type<>'oauth' THEN
    RAISE EXCEPTION 'EGRESS_INVALID_PARENT';
   END IF;
   SELECT * INTO p FROM wayvia_account_egress_policies WHERE root_account_id=parent.id FOR UPDATE;
   IF NOT FOUND THEN RAISE EXCEPTION 'EGRESS_POLICY_UNKNOWN'; END IF;
   IF new_oauth THEN
    IF p.current_mode<>'strict_ipv6' OR p.target_mode<>'strict_ipv6' OR p.state<>'ready' OR p.active_binding_id IS NULL THEN
     RAISE EXCEPTION 'EGRESS_PARENT_NOT_READY';
    END IF;
    -- 新影子随母账号固定出口，之后不能通过母账号退回普通出口。
    UPDATE wayvia_account_egress_policies SET require_ipv6=true WHERE owner_id=p.owner_id;
    p.require_ipv6 := true;
   END IF;
   IF p.current_mode='strict_ipv6' OR p.target_mode='strict_ipv6' THEN
    IF NEW.proxy_id IS NOT NULL OR (NEW.credentials - 'model_mapping' - 'compact_model_mapping')<>'{}'::jsonb THEN RAISE EXCEPTION 'EGRESS_SHADOW_CONFLICT'; END IF;
    IF TG_OP='UPDATE' AND NEW.parent_account_id IS DISTINCT FROM OLD.parent_account_id THEN RAISE EXCEPTION 'EGRESS_EXIT_REQUIRED'; END IF;
   END IF;
  ELSE
   IF EXISTS(SELECT 1 FROM wayvia_account_egress_policies WHERE identity_key=NULLIF(NEW.credentials->>'chatgpt_user_id','') AND root_account_id IS DISTINCT FROM NEW.id) THEN
    RAISE EXCEPTION 'EGRESS_DUPLICATE_IDENTITY_USE_ORIGINAL_OWNER';
   END IF;
   IF new_oauth THEN
    INSERT INTO wayvia_account_egress_policies(root_account_id,require_ipv6,state)
     VALUES(NEW.id,true,'pending_enrollment')
     ON CONFLICT(root_account_id) DO UPDATE SET require_ipv6=true,
      state=CASE WHEN wayvia_account_egress_policies.current_mode='legacy' AND wayvia_account_egress_policies.target_mode='legacy' THEN 'pending_enrollment' ELSE wayvia_account_egress_policies.state END;
    NEW.schedulable := false;
   END IF;
   SELECT * INTO p FROM wayvia_account_egress_policies WHERE root_account_id=NEW.id FOR UPDATE;
   IF NOT FOUND THEN RAISE EXCEPTION 'EGRESS_POLICY_UNKNOWN'; END IF;
  END IF;
  -- 只拦截开启调度，不妨碍暂停/核验中的凭据保存、元数据编辑和删除。
  IF p.require_ipv6 AND NEW.schedulable AND (TG_OP='INSERT' OR NOT OLD.schedulable OR new_oauth) THEN
   IF p.current_mode<>'strict_ipv6' OR p.target_mode<>'strict_ipv6' OR p.state<>'ready'
    OR NOT EXISTS(SELECT 1 FROM wayvia_account_egress_bindings WHERE id=p.active_binding_id AND owner_id=p.owner_id AND state='active')
    OR NOT EXISTS(SELECT 1 FROM settings WHERE key='wayvia_ipv6_egress' AND (value::jsonb->>'enabled')::boolean=true) THEN
    RAISE EXCEPTION 'EGRESS_ENROLLMENT_REQUIRED';
   END IF;
  END IF;
 END IF;
 IF TG_OP='UPDATE' AND NEW.deleted_at IS NOT NULL THEN
  UPDATE wayvia_account_egress_policies SET state='retired',policy_version=policy_version+1 WHERE root_account_id=OLD.id;
 END IF;
 RETURN NEW;
END $$;


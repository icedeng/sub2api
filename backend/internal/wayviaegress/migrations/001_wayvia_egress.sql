CREATE TABLE wayvia_ipv6_pools (
 id text PRIMARY KEY CHECK (id='default'), cidr cidr NOT NULL CHECK(family(cidr)=6 AND masklen(cidr)=64),
 node_id text NOT NULL, interface text NOT NULL, allocation_version bigint NOT NULL DEFAULT 1,
 config_version bigint NOT NULL DEFAULT 1, state text NOT NULL DEFAULT 'approved'
);
CREATE TABLE wayvia_account_egress_policies (
 owner_id uuid PRIMARY KEY DEFAULT gen_random_uuid(), root_account_id bigint UNIQUE REFERENCES accounts(id) ON DELETE SET NULL,
 identity_key text UNIQUE, current_mode text NOT NULL DEFAULT 'legacy' CHECK(current_mode IN ('legacy','strict_ipv6')),
 target_mode text NOT NULL DEFAULT 'legacy' CHECK(target_mode IN ('legacy','strict_ipv6')),
 policy_version bigint NOT NULL DEFAULT 1, state text NOT NULL DEFAULT 'legacy', active_binding_id uuid,
 operation_id uuid, last_error text NOT NULL DEFAULT '', observed_ipv6 inet, observed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE wayvia_account_egress_bindings (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), owner_id uuid NOT NULL REFERENCES wayvia_account_egress_policies(owner_id) ON DELETE RESTRICT,
 pool_id text NOT NULL REFERENCES wayvia_ipv6_pools(id) ON DELETE RESTRICT,
 ipv6 inet UNIQUE NOT NULL CHECK(family(ipv6)=6 AND masklen(ipv6)=128 AND NOT ipv6 <<= '::ffff:0:0/96'::cidr),
 binding_version bigint NOT NULL CHECK(binding_version>0), allocation_key_id text NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','active','retained')),
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(owner_id,binding_version), UNIQUE(id,owner_id)
);
ALTER TABLE wayvia_account_egress_policies ADD CONSTRAINT wayvia_binding_owner_fk
 FOREIGN KEY(active_binding_id,owner_id) REFERENCES wayvia_account_egress_bindings(id,owner_id) ON DELETE RESTRICT;
CREATE TABLE wayvia_egress_operations (
 id uuid PRIMARY KEY, idempotency_key text UNIQUE NOT NULL, request_hash text NOT NULL,
 owner_id uuid REFERENCES wayvia_account_egress_policies(owner_id), kind text NOT NULL,
 target text NOT NULL, expected_version bigint NOT NULL, actor text NOT NULL,
 phase text NOT NULL DEFAULT 'accepted', error text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz
);
CREATE FUNCTION wayvia_binding_in_pool() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM wayvia_ipv6_pools WHERE id=NEW.pool_id AND NEW.ipv6 <<= cidr) THEN
  RAISE EXCEPTION 'EGRESS_ADDRESS_OUTSIDE_POOL';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER wayvia_binding_pool_check BEFORE INSERT OR UPDATE ON wayvia_account_egress_bindings FOR EACH ROW EXECUTE FUNCTION wayvia_binding_in_pool();

-- 首次迁移仅给现存根账号建立显式 legacy；后续启动绝不补齐丢失策略。
INSERT INTO wayvia_account_egress_policies(root_account_id)
 SELECT id FROM accounts WHERE platform='openai' AND type='oauth' AND parent_account_id IS NULL AND deleted_at IS NULL;
INSERT INTO settings(key,value,updated_at) VALUES ('wayvia_ipv6_egress','{"enabled":false,"revision":1,"initialized":true}',now());

-- 账号写入口包含批量导入和 Ent；事务触发器保证建号与策略不会半成功。
CREATE FUNCTION wayvia_account_policy_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE p wayvia_account_egress_policies%ROWTYPE;
 parent accounts%ROWTYPE;
 protected boolean := false;
BEGIN
 IF TG_OP <> 'INSERT' THEN
  SELECT * INTO p FROM wayvia_account_egress_policies WHERE root_account_id=COALESCE(OLD.parent_account_id,OLD.id) FOR UPDATE;
  protected := FOUND AND (p.current_mode='strict_ipv6' OR p.target_mode='strict_ipv6' OR p.state='credential_uncertain');
  IF protected THEN
   IF TG_OP='DELETE' THEN
    IF p.state<>'retired' THEN RAISE EXCEPTION 'EGRESS_RETIRE_REQUIRED'; END IF;
   ELSE
    IF NEW.deleted_at IS DISTINCT FROM OLD.deleted_at AND p.state<>'retired' THEN RAISE EXCEPTION 'EGRESS_RETIRE_REQUIRED'; END IF;
    IF NEW.platform IS DISTINCT FROM OLD.platform OR NEW.type IS DISTINCT FROM OLD.type
     OR NEW.parent_account_id IS DISTINCT FROM OLD.parent_account_id OR NEW.proxy_id IS DISTINCT FROM OLD.proxy_id
     THEN RAISE EXCEPTION 'EGRESS_IDENTITY_LOCKED'; END IF;
    IF NEW.credentials IS DISTINCT FROM OLD.credentials AND OLD.parent_account_id IS NULL
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
  IF NEW.parent_account_id IS NOT NULL THEN
   SELECT * INTO parent FROM accounts WHERE id=NEW.parent_account_id AND deleted_at IS NULL FOR SHARE;
   IF NOT FOUND OR parent.id=NEW.id OR parent.parent_account_id IS NOT NULL OR parent.platform<>'openai' OR parent.type<>'oauth' THEN
    RAISE EXCEPTION 'EGRESS_INVALID_PARENT';
   END IF;
   SELECT * INTO p FROM wayvia_account_egress_policies WHERE root_account_id=parent.id;
   IF NOT FOUND THEN RAISE EXCEPTION 'EGRESS_POLICY_UNKNOWN'; END IF;
   IF p.current_mode='strict_ipv6' OR p.target_mode='strict_ipv6' THEN
    IF NEW.proxy_id IS NOT NULL OR (NEW.credentials - 'model_mapping' - 'compact_model_mapping')<>'{}'::jsonb THEN RAISE EXCEPTION 'EGRESS_SHADOW_CONFLICT'; END IF;
    IF TG_OP='UPDATE' AND NEW.parent_account_id IS DISTINCT FROM OLD.parent_account_id THEN RAISE EXCEPTION 'EGRESS_EXIT_REQUIRED'; END IF;
   END IF;
  ELSE
   IF EXISTS(SELECT 1 FROM wayvia_account_egress_policies WHERE identity_key=NULLIF(NEW.credentials->>'chatgpt_user_id','') AND root_account_id IS DISTINCT FROM NEW.id) THEN
    RAISE EXCEPTION 'EGRESS_DUPLICATE_IDENTITY_USE_ORIGINAL_OWNER';
   END IF;
   IF TG_OP='INSERT' OR OLD.platform<>'openai' OR OLD.type<>'oauth' OR OLD.parent_account_id IS NOT NULL THEN
    INSERT INTO wayvia_account_egress_policies(root_account_id) VALUES(NEW.id) ON CONFLICT(root_account_id) DO NOTHING;
   END IF;
  END IF;
 END IF;
 IF TG_OP='UPDATE' AND NEW.deleted_at IS NOT NULL THEN
  UPDATE wayvia_account_egress_policies SET state='retired',policy_version=policy_version+1 WHERE root_account_id=OLD.id;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER wayvia_account_create AFTER INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION wayvia_account_policy_guard();
CREATE TRIGGER wayvia_account_update BEFORE UPDATE OR DELETE ON accounts FOR EACH ROW EXECUTE FUNCTION wayvia_account_policy_guard();

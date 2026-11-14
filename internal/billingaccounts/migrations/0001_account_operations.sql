-- Explicit offline migration: no roles, accounts, funds or enrollment are created.
CREATE SCHEMA kaana_billing_accounts;
REVOKE ALL ON SCHEMA kaana_billing_accounts FROM PUBLIC;
CREATE TABLE kaana_billing_accounts.operations (
 account_id text NOT NULL REFERENCES kaana_billing.accounts(id),
 operation_key text NOT NULL,
 fingerprint text NOT NULL,
 evidence_id text NOT NULL,
 config_digest text NOT NULL CHECK(config_digest ~ '^[0-9a-f]{64}$'),
 receipt jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(account_id,operation_key)
);
CREATE TRIGGER immutable_operation BEFORE UPDATE OR DELETE ON kaana_billing_accounts.operations
 FOR EACH ROW EXECUTE FUNCTION kaana_billing.immutable();

-- Snapshot is independent of timezone/JSON rendering. Exact balances and active
-- budget usage participate, so a concurrent financial change invalidates review.
CREATE FUNCTION kaana_billing_accounts.snapshot(aid text) RETURNS jsonb
LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
 SELECT jsonb_build_object('accountId',a.id,'currency',c.code,'enabled',a.enabled,
 'creditLimit',a.credit_limit::text,'promotional',a.promotional::text,
 'purchased',a.purchased::text,'invoiced',a.invoiced::text,'reserved',a.reserved::text,
 'budget',CASE WHEN b.id IS NULL THEN NULL ELSE jsonb_build_object('id',b.id,
 'startsAtUnix',extract(epoch FROM b.starts_at)::text,'endsAtUnix',extract(epoch FROM b.ends_at)::text,
 'amount',b.amount::text,'held',b.held::text,'spent',b.spent::text) END)
 FROM kaana_billing.accounts a JOIN kaana_billing.currencies c ON c.id=a.currency_id
 LEFT JOIN kaana_billing.budgets b ON b.id=a.active_budget_id WHERE a.id=aid;
$$;
CREATE FUNCTION kaana_billing_accounts.inspect(owner_ref text, currency_code text, aid text)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE value jsonb;
BEGIN
 PERFORM 1 FROM kaana_billing.accounts a JOIN kaana_billing.currencies c ON c.id=a.currency_id
 WHERE a.id=aid AND a.oxy_owner=owner_ref AND c.code=currency_code FOR UPDATE OF a;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KB003',MESSAGE='scoped account not found'; END IF;
 value:=kaana_billing_accounts.snapshot(aid);
 RETURN value || jsonb_build_object('revision',kaana_billing.fingerprint(value));
END;
$$;
CREATE FUNCTION kaana_billing_accounts.provision(owner_ref text, currency_code text, op text, evidence text, config_hash text)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE aid text; fp text; previous kaana_billing_accounts.operations; result jsonb;
BEGIN
 PERFORM kaana_billing.key(op); PERFORM kaana_billing.key(evidence);
 IF config_hash IS NULL OR config_hash !~ '^[0-9a-f]{64}$' THEN RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='reviewed config required'; END IF;
 aid:=kaana_billing.create_account(owner_ref,currency_code)->>'id';
 PERFORM 1 FROM kaana_billing.accounts WHERE id=aid FOR UPDATE;
 fp:=kaana_billing.fingerprint(jsonb_build_array('provision',owner_ref,currency_code,evidence,config_hash));
 SELECT * INTO previous FROM kaana_billing_accounts.operations WHERE account_id=aid AND operation_key=op;
 IF FOUND THEN
  IF previous.fingerprint<>fp THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='account operation conflict'; END IF;
  RETURN previous.receipt;
 END IF;
 IF EXISTS(SELECT 1 FROM kaana_billing.accounts WHERE id=aid AND(enabled OR active_budget_id IS NOT NULL OR credit_limit<>0 OR promotional<>0 OR purchased<>0 OR invoiced<>0 OR reserved<>0)) THEN
  RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='provision requires an empty disabled account';
 END IF;
 result:=jsonb_build_object('action','provision','key',op,'evidenceId',evidence,'configSha256',config_hash,
 'snapshot',kaana_billing_accounts.inspect(owner_ref,currency_code,aid));
 INSERT INTO kaana_billing_accounts.operations VALUES(aid,op,fp,evidence,config_hash,result,clock_timestamp());
 RETURN result;
END;
$$;
CREATE FUNCTION kaana_billing_accounts.configure(owner_ref text, currency_code text, aid text, op text,
 evidence text, config_hash text, expected_revision text, budget_key text, starts timestamptz, ends timestamptz,
 amount text, credit text, active boolean) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE fp text; previous kaana_billing_accounts.operations; current_snapshot jsonb; bid text; result jsonb;
BEGIN
 PERFORM kaana_billing.key(op); PERFORM kaana_billing.key(evidence); PERFORM kaana_billing.key(budget_key);
 IF config_hash IS NULL OR config_hash !~ '^[0-9a-f]{64}$' OR expected_revision IS NULL OR expected_revision !~ '^[0-9a-f]{64}$'
 OR active IS NULL THEN RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='explicit review required'; END IF;
 current_snapshot:=kaana_billing_accounts.inspect(owner_ref,currency_code,aid);
 fp:=kaana_billing.fingerprint(jsonb_build_array('configure',owner_ref,currency_code,evidence,config_hash,expected_revision,
 budget_key,extract(epoch FROM starts),extract(epoch FROM ends),kaana_billing.amount(amount),kaana_billing.amount(credit),active));
 SELECT * INTO previous FROM kaana_billing_accounts.operations WHERE account_id=aid AND operation_key=op;
 IF FOUND THEN
  IF previous.fingerprint<>fp THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='account operation conflict'; END IF;
  -- Exact replay acknowledges the original commit, never reapplies old settings.
  RETURN previous.receipt;
 END IF;
 IF current_snapshot->>'revision'<>expected_revision THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='account review changed'; END IF;
 bid:=kaana_billing.configure_budget(aid,budget_key,starts,ends,amount);
 PERFORM kaana_billing.configure_account(aid,bid,credit,active);
 result:=jsonb_build_object('action','configure','key',op,'evidenceId',evidence,'configSha256',config_hash,
 'snapshot',kaana_billing_accounts.inspect(owner_ref,currency_code,aid));
 INSERT INTO kaana_billing_accounts.operations VALUES(aid,op,fp,evidence,config_hash,result,clock_timestamp());
 RETURN result;
END;
$$;
REVOKE ALL ON ALL TABLES IN SCHEMA kaana_billing_accounts FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA kaana_billing_accounts FROM PUBLIC;
GRANT USAGE ON SCHEMA kaana_billing_accounts TO kaana_billing_account_admin;
GRANT EXECUTE ON FUNCTION kaana_billing_accounts.inspect(text,text,text),
 kaana_billing_accounts.provision(text,text,text,text,text),
 kaana_billing_accounts.configure(text,text,text,text,text,text,text,text,timestamptz,timestamptz,text,text,boolean)
 TO kaana_billing_account_admin;
-- Metadata-only startup verification, no write authority on either ledger.
GRANT SELECT ON public.kaana_billing_migrations,public.kaana_billing_account_migrations TO kaana_billing_account_admin;

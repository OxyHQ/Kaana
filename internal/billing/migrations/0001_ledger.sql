-- Explicit offline migration. No account, currency, credit or enrollment is seeded.
CREATE SCHEMA kaana_billing;
REVOKE ALL ON SCHEMA kaana_billing FROM PUBLIC;

CREATE TABLE kaana_billing.currencies (
 id text PRIMARY KEY DEFAULT ('kcur_' || replace(gen_random_uuid()::text,'-','')),
 code text NOT NULL UNIQUE CHECK (code ~ '^[A-Z]{3}$')
);
CREATE TABLE kaana_billing.accounts (
 id text PRIMARY KEY DEFAULT ('kba_' || replace(gen_random_uuid()::text,'-','')),
 oxy_owner text NOT NULL CHECK (length(oxy_owner) BETWEEN 1 AND 256),
 currency_id text NOT NULL REFERENCES kaana_billing.currencies(id),
 enabled boolean NOT NULL DEFAULT false,
 credit_limit numeric(38,12) NOT NULL DEFAULT 0 CHECK (credit_limit>=0),
 active_budget_id text,
 promotional numeric(38,12) NOT NULL DEFAULT 0 CHECK (promotional>=0),
 purchased numeric(38,12) NOT NULL DEFAULT 0 CHECK (purchased>=0),
 invoiced numeric(38,12) NOT NULL DEFAULT 0 CHECK (invoiced>=0),
 reserved numeric(38,12) NOT NULL DEFAULT 0 CHECK (reserved>=0),
 UNIQUE(oxy_owner,currency_id)
);
CREATE TABLE kaana_billing.budgets (
 id text PRIMARY KEY DEFAULT ('kbud_' || replace(gen_random_uuid()::text,'-','')),
 account_id text NOT NULL REFERENCES kaana_billing.accounts(id),
 operation_key text NOT NULL,
 fingerprint text NOT NULL,
 starts_at timestamptz NOT NULL,
 ends_at timestamptz NOT NULL CHECK (ends_at>starts_at),
 amount numeric(38,12) NOT NULL CHECK (amount>=0),
 held numeric(38,12) NOT NULL DEFAULT 0 CHECK (held>=0),
 spent numeric(38,12) NOT NULL DEFAULT 0 CHECK (spent>=0),
 UNIQUE(account_id,operation_key), UNIQUE(id,account_id),
 CHECK (held+spent<=amount)
);
ALTER TABLE kaana_billing.accounts ADD CONSTRAINT account_budget_owner
 FOREIGN KEY(active_budget_id,id) REFERENCES kaana_billing.budgets(id,account_id);
CREATE TABLE kaana_billing.funding (
 id text PRIMARY KEY DEFAULT ('kfund_' || replace(gen_random_uuid()::text,'-','')),
 account_id text NOT NULL REFERENCES kaana_billing.accounts(id),
 operation_key text NOT NULL,
 fingerprint text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('promotional','purchased')),
 evidence_id text NOT NULL UNIQUE,
 amount numeric(38,12) NOT NULL CHECK (amount>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(account_id,operation_key)
);
CREATE TABLE kaana_billing.price_snapshots (
 id text PRIMARY KEY,
 digest text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
 UNIQUE(id,digest)
);
CREATE TABLE kaana_billing.holds (
 id text PRIMARY KEY DEFAULT ('khold_' || replace(gen_random_uuid()::text,'-','')),
 account_id text NOT NULL REFERENCES kaana_billing.accounts(id),
 budget_id text NOT NULL,
 operation_key text NOT NULL,
 fingerprint text NOT NULL,
 admission_id text NOT NULL UNIQUE,
 price_snapshot_id text NOT NULL,
 price_digest text NOT NULL CHECK (price_digest ~ '^[0-9a-f]{64}$'),
 amount numeric(38,12) NOT NULL CHECK (amount>=0),
 promotional numeric(38,12) NOT NULL CHECK (promotional>=0),
 purchased numeric(38,12) NOT NULL CHECK (purchased>=0),
 invoiced numeric(38,12) NOT NULL CHECK (invoiced>=0),
 state text NOT NULL DEFAULT 'held' CHECK (state IN ('held','settled','expired')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 UNIQUE(account_id,operation_key), UNIQUE(id,account_id),
 FOREIGN KEY(budget_id,account_id) REFERENCES kaana_billing.budgets(id,account_id),
 FOREIGN KEY(price_snapshot_id,price_digest) REFERENCES kaana_billing.price_snapshots(id,digest),
 CHECK (promotional+purchased+invoiced=amount),
 CHECK (expires_at>created_at AND expires_at<=created_at+interval '15 minutes')
);
CREATE INDEX billing_due_holds ON kaana_billing.holds(expires_at,id) WHERE state='held';
CREATE TABLE kaana_billing.receipts (
 id text PRIMARY KEY DEFAULT ('kreceipt_' || replace(gen_random_uuid()::text,'-','')),
 account_id text NOT NULL REFERENCES kaana_billing.accounts(id),
 hold_id text NOT NULL UNIQUE,
 operation_key text NOT NULL,
 fingerprint text NOT NULL,
 evidence_id text NOT NULL UNIQUE,
 evidence_digest text NOT NULL CHECK (evidence_digest ~ '^[0-9a-f]{64}$'),
 outcome text NOT NULL CHECK (outcome IN ('completed','partial','failed','cancelled')),
 has_usage boolean NOT NULL,
 rated_amount numeric(38,12) NOT NULL CHECK (rated_amount>=0),
 charged numeric(38,12) NOT NULL CHECK (charged>=0),
 promotional numeric(38,12) NOT NULL CHECK (promotional>=0),
 purchased numeric(38,12) NOT NULL CHECK (purchased>=0),
 invoiced numeric(38,12) NOT NULL CHECK (invoiced>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(account_id,operation_key), UNIQUE(id,account_id),
 FOREIGN KEY(hold_id,account_id) REFERENCES kaana_billing.holds(id,account_id),
 CHECK (promotional+purchased+invoiced=charged),
 CHECK ((outcome IN ('completed','partial') AND has_usage) OR charged=0)
);
CREATE TABLE kaana_billing.refunds (
 id text PRIMARY KEY DEFAULT ('krefund_' || replace(gen_random_uuid()::text,'-','')),
 account_id text NOT NULL REFERENCES kaana_billing.accounts(id),
 receipt_id text NOT NULL,
 operation_key text NOT NULL,
 fingerprint text NOT NULL,
 evidence_id text NOT NULL UNIQUE,
 amount numeric(38,12) NOT NULL CHECK (amount>0),
 promotional numeric(38,12) NOT NULL CHECK (promotional>=0),
 purchased numeric(38,12) NOT NULL CHECK (purchased>=0),
 invoiced numeric(38,12) NOT NULL CHECK (invoiced>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(account_id,operation_key),
 FOREIGN KEY(receipt_id,account_id) REFERENCES kaana_billing.receipts(id,account_id),
 CHECK (promotional+purchased+invoiced=amount)
);
-- A transfer encodes both equal legs in ONE row: debit(source), credit(destination).
-- It is structurally balanced, including split holds with several transfers.
CREATE TABLE kaana_billing.journal (
 id text PRIMARY KEY DEFAULT ('kj_' || replace(gen_random_uuid()::text,'-','')),
 account_id text NOT NULL REFERENCES kaana_billing.accounts(id),
 operation_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('funding','hold','charge','release','expiry','refund')),
 source text NOT NULL CHECK (source IN ('promotional_issuance','external_settlement','promotional','purchased','invoice_credit','reserved','revenue')),
 destination text NOT NULL CHECK (destination IN ('promotional','purchased','invoice_credit','reserved','revenue')),
 amount numeric(38,12) NOT NULL CHECK (amount>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (source<>destination), UNIQUE(operation_id,kind,source,destination)
);
CREATE VIEW kaana_billing.postings AS
 SELECT id AS journal_id,account_id,source AS bucket,-amount AS amount FROM kaana_billing.journal
 UNION ALL SELECT id,account_id,destination,amount FROM kaana_billing.journal;

CREATE FUNCTION kaana_billing.immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION USING ERRCODE='KB001',MESSAGE='immutable billing record'; END;
$$;
CREATE TRIGGER immutable_currency BEFORE UPDATE OR DELETE ON kaana_billing.currencies FOR EACH ROW EXECUTE FUNCTION kaana_billing.immutable();
CREATE TRIGGER immutable_price_snapshot BEFORE UPDATE OR DELETE ON kaana_billing.price_snapshots FOR EACH ROW EXECUTE FUNCTION kaana_billing.immutable();
CREATE TRIGGER immutable_funding BEFORE UPDATE OR DELETE ON kaana_billing.funding FOR EACH ROW EXECUTE FUNCTION kaana_billing.immutable();
CREATE TRIGGER immutable_receipt BEFORE UPDATE OR DELETE ON kaana_billing.receipts FOR EACH ROW EXECUTE FUNCTION kaana_billing.immutable();
CREATE TRIGGER immutable_refund BEFORE UPDATE OR DELETE ON kaana_billing.refunds FOR EACH ROW EXECUTE FUNCTION kaana_billing.immutable();
CREATE TRIGGER immutable_journal BEFORE UPDATE OR DELETE ON kaana_billing.journal FOR EACH ROW EXECUTE FUNCTION kaana_billing.immutable();
CREATE FUNCTION kaana_billing.account_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.id<>OLD.id OR NEW.oxy_owner<>OLD.oxy_owner OR NEW.currency_id<>OLD.currency_id THEN
  RAISE EXCEPTION USING ERRCODE='KB001',MESSAGE='immutable billing identity';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER immutable_account_identity BEFORE UPDATE ON kaana_billing.accounts FOR EACH ROW EXECUTE FUNCTION kaana_billing.account_identity();
CREATE FUNCTION kaana_billing.hold_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION USING ERRCODE='KB001',MESSAGE='immutable hold'; END IF;
 IF (to_jsonb(NEW)-'state') IS DISTINCT FROM (to_jsonb(OLD)-'state') OR OLD.state<>'held' OR NEW.state NOT IN ('settled','expired') THEN
  RAISE EXCEPTION USING ERRCODE='KB001',MESSAGE='immutable hold';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER immutable_hold BEFORE UPDATE OR DELETE ON kaana_billing.holds FOR EACH ROW EXECUTE FUNCTION kaana_billing.hold_identity();
CREATE FUNCTION kaana_billing.budget_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION USING ERRCODE='KB001',MESSAGE='immutable budget'; END IF;
 IF (to_jsonb(NEW)-'held'-'spent') IS DISTINCT FROM (to_jsonb(OLD)-'held'-'spent') THEN
  RAISE EXCEPTION USING ERRCODE='KB001',MESSAGE='immutable budget';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER immutable_budget BEFORE UPDATE OR DELETE ON kaana_billing.budgets FOR EACH ROW EXECUTE FUNCTION kaana_billing.budget_identity();

CREATE FUNCTION kaana_billing.amount(value text) RETURNS numeric LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
 IF value IS NULL OR value !~ '^(0|[1-9][0-9]{0,23})(\.[0-9]{1,12})?$' THEN
  RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='invalid billing amount';
 END IF;
 RETURN value::numeric;
END;
$$;
CREATE FUNCTION kaana_billing.key(value text) RETURNS text LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
 IF value IS NULL OR length(value) NOT BETWEEN 1 AND 256 THEN
  RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='invalid billing key';
 END IF;
 RETURN value;
END;
$$;
CREATE FUNCTION kaana_billing.fingerprint(value jsonb) RETURNS text LANGUAGE sql IMMUTABLE AS $$
 SELECT encode(sha256(convert_to(value::text,'UTF8')),'hex');
$$;
CREATE FUNCTION kaana_billing.scope(aid text) RETURNS jsonb LANGUAGE sql STABLE AS $$
 SELECT jsonb_build_array(id,currency_id,oxy_owner) FROM kaana_billing.accounts WHERE id=aid;
$$;
CREATE FUNCTION kaana_billing.post(a text, op text, k text, src text, dst text, n numeric)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 IF n>0 THEN INSERT INTO kaana_billing.journal(account_id,operation_id,kind,source,destination,amount) VALUES(a,op,k,src,dst,n); END IF;
END;
$$;
CREATE FUNCTION kaana_billing.hold_view(h kaana_billing.holds) RETURNS jsonb LANGUAGE sql STABLE AS $$
 SELECT jsonb_build_object('id',h.id,'accountId',h.account_id,'budgetId',h.budget_id,'state',h.state,'amount',h.amount::text,'expiresAt',h.expires_at);
$$;
CREATE FUNCTION kaana_billing.receipt_view(r kaana_billing.receipts) RETURNS jsonb LANGUAGE sql STABLE AS $$
 SELECT jsonb_build_object('id',r.id,'accountId',r.account_id,'holdId',r.hold_id,'evidenceId',r.evidence_id,'charged',r.charged::text,'outcome',r.outcome);
$$;

-- Admin-only provisioning. Oxy owner is an opaque attribute, never the PK.
CREATE FUNCTION kaana_billing.create_account(owner_ref text, currency_code text)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c text; a kaana_billing.accounts;
BEGIN
 PERFORM kaana_billing.key(owner_ref);
 INSERT INTO kaana_billing.currencies(code) VALUES(currency_code) ON CONFLICT(code) DO NOTHING;
 SELECT id INTO c FROM kaana_billing.currencies WHERE code=currency_code;
 INSERT INTO kaana_billing.accounts(oxy_owner,currency_id) VALUES(owner_ref,c) ON CONFLICT(oxy_owner,currency_id) DO NOTHING;
 SELECT * INTO a FROM kaana_billing.accounts WHERE oxy_owner=owner_ref AND currency_id=c;
 RETURN jsonb_build_object('id',a.id,'currencyId',a.currency_id,'enabled',a.enabled);
END;
$$;
CREATE FUNCTION kaana_billing.configure_budget(aid text, op text, starts timestamptz, ends timestamptz, cap text)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE b kaana_billing.budgets; fp text; n numeric;
BEGIN
 n:=kaana_billing.amount(cap); PERFORM kaana_billing.key(op);
 IF starts IS NULL OR ends IS NULL OR ends<=starts OR NOT isfinite(starts) OR NOT isfinite(ends) THEN RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='invalid budget window'; END IF;
 PERFORM 1 FROM kaana_billing.accounts WHERE id=aid FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KB003',MESSAGE='account not found'; END IF;
 fp:=kaana_billing.fingerprint(jsonb_build_array(kaana_billing.scope(aid),extract(epoch FROM starts),extract(epoch FROM ends),n));
 SELECT * INTO b FROM kaana_billing.budgets WHERE account_id=aid AND operation_key=op;
 IF FOUND THEN
  IF b.fingerprint<>fp THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='billing idempotency conflict'; END IF;
  RETURN b.id;
 END IF;
 INSERT INTO kaana_billing.budgets(account_id,operation_key,fingerprint,starts_at,ends_at,amount) VALUES(aid,op,fp,starts,ends,n) RETURNING * INTO b;
 RETURN b.id;
END;
$$;
CREATE FUNCTION kaana_billing.configure_account(aid text, budget text, credit text, active boolean)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE n numeric;
BEGIN
 n:=kaana_billing.amount(credit);
 IF active IS NULL OR (active AND budget IS NULL) THEN RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='budget required'; END IF;
 UPDATE kaana_billing.accounts SET active_budget_id=budget,credit_limit=n,enabled=active WHERE id=aid;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KB003',MESSAGE='account not found'; END IF;
END;
$$;
CREATE FUNCTION kaana_billing.fund(aid text, op text, bucket text, value text, evidence text)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE f kaana_billing.funding; n numeric; fp text;
BEGIN
 n:=kaana_billing.amount(value); PERFORM kaana_billing.key(op); PERFORM kaana_billing.key(evidence);
 IF n<=0 OR bucket NOT IN ('promotional','purchased') THEN RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='invalid funding'; END IF;
 PERFORM 1 FROM kaana_billing.accounts WHERE id=aid FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KB003',MESSAGE='account not found'; END IF;
 fp:=kaana_billing.fingerprint(jsonb_build_array(kaana_billing.scope(aid),bucket,n,evidence));
 SELECT * INTO f FROM kaana_billing.funding WHERE account_id=aid AND operation_key=op;
 IF FOUND THEN
  IF f.fingerprint<>fp THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='billing idempotency conflict'; END IF;
  RETURN f.id;
 END IF;
 INSERT INTO kaana_billing.funding(account_id,operation_key,fingerprint,kind,evidence_id,amount) VALUES(aid,op,fp,bucket,evidence,n) RETURNING * INTO f;
 UPDATE kaana_billing.accounts SET promotional=promotional+CASE WHEN bucket='promotional' THEN n ELSE 0 END,purchased=purchased+CASE WHEN bucket='purchased' THEN n ELSE 0 END WHERE id=aid;
 PERFORM kaana_billing.post(aid,f.id,'funding',CASE WHEN bucket='promotional' THEN 'promotional_issuance' ELSE 'external_settlement' END,bucket,n);
 RETURN f.id;
END;
$$;

CREATE FUNCTION kaana_billing.reserve(aid text, op text, admission text, price_id text, price_hash text, maximum text, ttl_seconds integer)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE a kaana_billing.accounts; b kaana_billing.budgets; h kaana_billing.holds; n numeric; fp text; p numeric; q numeric; i numeric; now_at timestamptz;
BEGIN
 n:=kaana_billing.amount(maximum); PERFORM kaana_billing.key(op); PERFORM kaana_billing.key(admission); PERFORM kaana_billing.key(price_id);
 IF ttl_seconds IS NULL OR ttl_seconds NOT BETWEEN 1 AND 900 OR price_hash IS NULL OR price_hash !~ '^[0-9a-f]{64}$' THEN RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='invalid reservation'; END IF;
 SELECT * INTO a FROM kaana_billing.accounts WHERE id=aid FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KB003',MESSAGE='account not found'; END IF;
 fp:=kaana_billing.fingerprint(jsonb_build_array(kaana_billing.scope(aid),admission,price_id,price_hash,n,ttl_seconds));
 SELECT * INTO h FROM kaana_billing.holds WHERE account_id=aid AND operation_key=op;
 IF FOUND THEN
  IF h.fingerprint<>fp THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='billing idempotency conflict'; END IF;
  RETURN kaana_billing.hold_view(h) || jsonb_build_object('replay',true);
 END IF;
 IF NOT a.enabled THEN RAISE EXCEPTION USING ERRCODE='KB005',MESSAGE='billing account disabled'; END IF;
 now_at:=clock_timestamp();
 SELECT * INTO b FROM kaana_billing.budgets WHERE id=a.active_budget_id;
 IF NOT FOUND OR now_at<b.starts_at OR now_at>=b.ends_at OR b.held+b.spent+n>b.amount THEN RAISE EXCEPTION USING ERRCODE='KB006',MESSAGE='billing budget exceeded'; END IF;
 INSERT INTO kaana_billing.price_snapshots(id,digest) VALUES(price_id,price_hash) ON CONFLICT(id) DO NOTHING;
 IF NOT EXISTS(SELECT 1 FROM kaana_billing.price_snapshots WHERE id=price_id AND digest=price_hash) THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='price snapshot conflict'; END IF;
 p:=least(a.promotional,n); q:=least(a.purchased,n-p); i:=n-p-q;
 IF i>greatest(0,a.credit_limit-a.invoiced) THEN RAISE EXCEPTION USING ERRCODE='KB007',MESSAGE='insufficient billing funds'; END IF;
 INSERT INTO kaana_billing.holds(account_id,budget_id,operation_key,fingerprint,admission_id,price_snapshot_id,price_digest,amount,promotional,purchased,invoiced,created_at,expires_at)
 VALUES(aid,b.id,op,fp,admission,price_id,price_hash,n,p,q,i,now_at,now_at+make_interval(secs=>ttl_seconds)) RETURNING * INTO h;
 UPDATE kaana_billing.accounts SET promotional=promotional-p,purchased=purchased-q,invoiced=invoiced+i,reserved=reserved+n WHERE id=aid;
 UPDATE kaana_billing.budgets SET held=held+n WHERE id=b.id;
 PERFORM kaana_billing.post(aid,h.id,'hold','promotional','reserved',p);
 PERFORM kaana_billing.post(aid,h.id,'hold','purchased','reserved',q);
 PERFORM kaana_billing.post(aid,h.id,'hold','invoice_credit','reserved',i);
 RETURN kaana_billing.hold_view(h);
END;
$$;
-- Only called with the account row already locked. No role may call it directly.
CREATE FUNCTION kaana_billing.release(h kaana_billing.holds, k text, op text, p numeric, q numeric, i numeric)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 UPDATE kaana_billing.accounts SET promotional=promotional+p,purchased=purchased+q,invoiced=invoiced-i,reserved=reserved-h.amount WHERE id=h.account_id;
 UPDATE kaana_billing.budgets SET held=held-h.amount,spent=spent+h.amount-p-q-i WHERE id=h.budget_id;
 PERFORM kaana_billing.post(h.account_id,op,k,'reserved','promotional',p);
 PERFORM kaana_billing.post(h.account_id,op,k,'reserved','purchased',q);
 PERFORM kaana_billing.post(h.account_id,op,k,'reserved','invoice_credit',i);
END;
$$;
CREATE FUNCTION kaana_billing.settle(aid text, hid text, op text, evidence text, evidence_hash text, outcome_name text, usage_present boolean, rated text)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE h kaana_billing.holds; r kaana_billing.receipts; n numeric; charge numeric; fp text; p numeric; q numeric; i numeric;
BEGIN
 n:=kaana_billing.amount(rated); PERFORM kaana_billing.key(op); PERFORM kaana_billing.key(evidence);
 IF evidence_hash IS NULL OR evidence_hash !~ '^[0-9a-f]{64}$' OR outcome_name IS NULL OR outcome_name NOT IN ('completed','partial','failed','cancelled') OR usage_present IS NULL OR (NOT usage_present AND n<>0) THEN RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='invalid settlement evidence'; END IF;
 PERFORM 1 FROM kaana_billing.accounts WHERE id=aid FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KB003',MESSAGE='account not found'; END IF;
 fp:=kaana_billing.fingerprint(jsonb_build_array(kaana_billing.scope(aid),hid,evidence,evidence_hash,outcome_name,usage_present,n));
 SELECT * INTO r FROM kaana_billing.receipts WHERE account_id=aid AND operation_key=op;
 IF FOUND THEN
  IF r.fingerprint<>fp THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='billing idempotency conflict'; END IF;
  RETURN jsonb_build_object('state','settled','receipt',kaana_billing.receipt_view(r));
 END IF;
 SELECT * INTO h FROM kaana_billing.holds WHERE id=hid AND account_id=aid;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KB003',MESSAGE='hold not found'; END IF;
 IF h.state='settled' THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='hold already settled with another key'; END IF;
 IF h.state='expired' THEN RETURN jsonb_build_object('state','expired'); END IF;
 IF h.expires_at<=clock_timestamp() THEN
  PERFORM kaana_billing.release(h,'expiry',h.id,h.promotional,h.purchased,h.invoiced);
  UPDATE kaana_billing.holds SET state='expired' WHERE id=h.id;
  RETURN jsonb_build_object('state','expired');
 END IF;
 charge:=CASE WHEN outcome_name IN ('completed','partial') AND usage_present THEN n ELSE 0 END;
 IF charge>h.amount THEN RAISE EXCEPTION USING ERRCODE='KB008',MESSAGE='settlement exceeds hold'; END IF;
 p:=least(h.promotional,charge); q:=least(h.purchased,charge-p); i:=charge-p-q;
 INSERT INTO kaana_billing.receipts(account_id,hold_id,operation_key,fingerprint,evidence_id,evidence_digest,outcome,has_usage,rated_amount,charged,promotional,purchased,invoiced)
 VALUES(aid,h.id,op,fp,evidence,evidence_hash,outcome_name,usage_present,n,charge,p,q,i) RETURNING * INTO r;
 PERFORM kaana_billing.post(aid,r.id,'charge','reserved','revenue',charge);
 PERFORM kaana_billing.release(h,'release',r.id,h.promotional-p,h.purchased-q,h.invoiced-i);
 UPDATE kaana_billing.holds SET state='settled' WHERE id=h.id;
 RETURN jsonb_build_object('state','settled','receipt',kaana_billing.receipt_view(r));
END;
$$;
CREATE FUNCTION kaana_billing.expire(hid text) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE h kaana_billing.holds; aid text;
BEGIN
 SELECT account_id INTO aid FROM kaana_billing.holds WHERE id=hid;
 IF NOT FOUND THEN RETURN false; END IF;
 PERFORM 1 FROM kaana_billing.accounts WHERE id=aid FOR UPDATE;
 SELECT * INTO h FROM kaana_billing.holds WHERE id=hid;
 IF h.state<>'held' OR h.expires_at>clock_timestamp() THEN RETURN false; END IF;
 PERFORM kaana_billing.release(h,'expiry',h.id,h.promotional,h.purchased,h.invoiced);
 UPDATE kaana_billing.holds SET state='expired' WHERE id=h.id;
 RETURN true;
END;
$$;
CREATE FUNCTION kaana_billing.refund(aid text, rid text, op text, value text, evidence text)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r kaana_billing.receipts; f kaana_billing.refunds; n numeric; fp text; p numeric; q numeric; i numeric; used_p numeric; used_q numeric; used_i numeric; budget text;
BEGIN
 n:=kaana_billing.amount(value); PERFORM kaana_billing.key(op); PERFORM kaana_billing.key(evidence);
 IF n<=0 THEN RAISE EXCEPTION USING ERRCODE='KB002',MESSAGE='invalid refund'; END IF;
 PERFORM 1 FROM kaana_billing.accounts WHERE id=aid FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KB003',MESSAGE='account not found'; END IF;
 fp:=kaana_billing.fingerprint(jsonb_build_array(kaana_billing.scope(aid),rid,n,evidence));
 SELECT * INTO f FROM kaana_billing.refunds WHERE account_id=aid AND operation_key=op;
 IF FOUND THEN
  IF f.fingerprint<>fp THEN RAISE EXCEPTION USING ERRCODE='KB004',MESSAGE='billing idempotency conflict'; END IF;
  RETURN f.id;
 END IF;
 SELECT * INTO r FROM kaana_billing.receipts WHERE id=rid AND account_id=aid;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KB003',MESSAGE='receipt not found'; END IF;
 SELECT coalesce(sum(promotional),0),coalesce(sum(purchased),0),coalesce(sum(invoiced),0) INTO used_p,used_q,used_i FROM kaana_billing.refunds WHERE receipt_id=rid;
 IF n>r.charged-used_p-used_q-used_i THEN RAISE EXCEPTION USING ERRCODE='KB009',MESSAGE='refund exceeds charge'; END IF;
 i:=least(r.invoiced-used_i,n); q:=least(r.purchased-used_q,n-i); p:=n-i-q;
 INSERT INTO kaana_billing.refunds(account_id,receipt_id,operation_key,fingerprint,evidence_id,amount,promotional,purchased,invoiced)
 VALUES(aid,rid,op,fp,evidence,n,p,q,i) RETURNING * INTO f;
 UPDATE kaana_billing.accounts SET promotional=promotional+p,purchased=purchased+q,invoiced=invoiced-i WHERE id=aid;
 SELECT budget_id INTO budget FROM kaana_billing.holds WHERE id=r.hold_id;
 UPDATE kaana_billing.budgets SET spent=spent-n WHERE id=budget;
 PERFORM kaana_billing.post(aid,f.id,'refund','revenue','invoice_credit',i);
 PERFORM kaana_billing.post(aid,f.id,'refund','revenue','purchased',q);
 PERFORM kaana_billing.post(aid,f.id,'refund','revenue','promotional',p);
 RETURN f.id;
END;
$$;

REVOKE ALL ON ALL TABLES IN SCHEMA kaana_billing FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA kaana_billing FROM PUBLIC;
GRANT USAGE ON SCHEMA kaana_billing TO kaana_billing_runtime,kaana_billing_admin;
GRANT SELECT ON ALL TABLES IN SCHEMA kaana_billing TO kaana_billing_runtime,kaana_billing_admin;
GRANT EXECUTE ON FUNCTION kaana_billing.reserve(text,text,text,text,text,text,integer),kaana_billing.settle(text,text,text,text,text,text,boolean,text),kaana_billing.expire(text) TO kaana_billing_runtime;
GRANT EXECUTE ON FUNCTION kaana_billing.create_account(text,text),kaana_billing.configure_budget(text,text,timestamptz,timestamptz,text),kaana_billing.configure_account(text,text,text,boolean),kaana_billing.fund(text,text,text,text,text),kaana_billing.refund(text,text,text,text,text) TO kaana_billing_admin;

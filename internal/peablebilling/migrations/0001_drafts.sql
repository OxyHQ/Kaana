-- Explicit offline migration under kaana_billing_migrator; no seller/customer/enrollment seeded.
CREATE SCHEMA kaana_peable_billing;
REVOKE ALL ON SCHEMA kaana_peable_billing FROM PUBLIC;
CREATE TABLE kaana_peable_billing.customers (
 account_id text PRIMARY KEY REFERENCES kaana_billing.accounts(id),
 merchant_id text NOT NULL CHECK(length(merchant_id) BETWEEN 1 AND 128),
 customer_id text NOT NULL CHECK(length(customer_id) BETWEEN 1 AND 128),
 UNIQUE(merchant_id,customer_id)
);
CREATE TABLE kaana_peable_billing.drafts (
 id text PRIMARY KEY DEFAULT ('kpd_'||replace(gen_random_uuid()::text,'-','')),
 account_id text NOT NULL REFERENCES kaana_peable_billing.customers(account_id),
 operation_key text NOT NULL CHECK(length(operation_key) BETWEEN 1 AND 128),
 receipt_ids text[] NOT NULL,
 subtotal numeric(38,12) NOT NULL CHECK(subtotal>0),
 minor_exponent integer NOT NULL CHECK(minor_exponent IN(0,2)),
 amount_minor numeric(38,0) NOT NULL CHECK(amount_minor>0 AND amount_minor<1e31),
 rounding_adjustment numeric(38,12) NOT NULL,
 days_until_due integer NOT NULL CHECK(days_until_due BETWEEN 1 AND 365),
 invoice_attempted boolean NOT NULL DEFAULT false,
 invoice_id text UNIQUE,
 item_attempted boolean NOT NULL DEFAULT false,
 item_id text UNIQUE,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(account_id,operation_key)
);
CREATE TABLE kaana_peable_billing.draft_receipts (
 receipt_id text PRIMARY KEY REFERENCES kaana_billing.receipts(id),
 draft_id text NOT NULL REFERENCES kaana_peable_billing.drafts(id),
 amount numeric(38,12) NOT NULL CHECK(amount>=0)
);
CREATE FUNCTION kaana_peable_billing.bind_customer(a text,m text,c text) RETURNS void
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,kaana_peable_billing AS $$
DECLARE old kaana_peable_billing.customers;
BEGIN
 PERFORM 1 FROM kaana_billing.accounts WHERE id=a FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='billing account missing'; END IF;
 SELECT * INTO old FROM kaana_peable_billing.customers WHERE account_id=a;
 IF FOUND THEN
  IF old.merchant_id<>m OR old.customer_id<>c THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='customer binding conflict'; END IF;
  RETURN;
 END IF;
 INSERT INTO kaana_peable_billing.customers VALUES(a,m,c);
END $$;
CREATE FUNCTION kaana_peable_billing.draft_view(d kaana_peable_billing.drafts) RETURNS jsonb
 LANGUAGE sql STABLE SET search_path=pg_catalog,kaana_peable_billing AS $$
 SELECT jsonb_build_object('id',d.id,'accountId',d.account_id,'merchantId',m.merchant_id,'customerId',m.customer_id,
 'currency',c.code,'amountMinor',d.amount_minor::text,'subtotal',d.subtotal::text,'roundingAdjustment',d.rounding_adjustment::text,
 'minorExponent',d.minor_exponent,'daysUntilDue',d.days_until_due,'invoiceAttempted',d.invoice_attempted,'invoiceId',coalesce(d.invoice_id,''),
 'itemAttempted',d.item_attempted,'itemId',coalesce(d.item_id,''))
 FROM kaana_peable_billing.customers m JOIN kaana_billing.accounts a ON a.id=m.account_id
 JOIN kaana_billing.currencies c ON c.id=a.currency_id WHERE m.account_id=d.account_id
$$;
CREATE FUNCTION kaana_peable_billing.prepare_draft(a text,k text,ids text[],days integer) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,kaana_peable_billing AS $$
DECLARE d kaana_peable_billing.drafts; sorted text[]; total numeric; n integer; exp integer; code text;
BEGIN
 IF cardinality(ids)<1 OR cardinality(ids)>1000 OR days NOT BETWEEN 1 AND 365 THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='invalid draft'; END IF;
 SELECT array_agg(x ORDER BY x),count(DISTINCT x) INTO sorted,n FROM unnest(ids) x;
 IF n<>cardinality(ids) THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='duplicate receipt'; END IF;
 SELECT c.code INTO code FROM kaana_billing.accounts a0 JOIN kaana_billing.currencies c ON c.id=a0.currency_id WHERE a0.id=a FOR UPDATE OF a0;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='account missing'; END IF;
 IF NOT EXISTS(SELECT 1 FROM kaana_peable_billing.customers WHERE account_id=a) THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='customer not bound'; END IF;
 SELECT * INTO d FROM kaana_peable_billing.drafts WHERE account_id=a AND operation_key=k;
 IF FOUND THEN
  IF d.receipt_ids<>sorted OR d.days_until_due<>days THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='draft conflict'; END IF;
  RETURN kaana_peable_billing.draft_view(d);
 END IF;
 exp:=CASE WHEN code='JPY' THEN 0 WHEN code IN('USD','EUR','GBP','CHF','CAD','AUD') THEN 2 ELSE NULL END;
 IF exp IS NULL THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='unreviewed currency'; END IF;
 SELECT count(*),sum(r.invoiced-coalesce((SELECT sum(f.invoiced) FROM kaana_billing.refunds f WHERE f.receipt_id=r.id),0)) INTO n,total
 FROM kaana_billing.receipts r WHERE r.account_id=a AND r.id=ANY(sorted);
 IF n<>cardinality(sorted) OR total IS NULL OR total<=0 OR round(total*power(10::numeric,exp))<=0 THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='no invoiceable amount'; END IF;
 INSERT INTO kaana_peable_billing.drafts(account_id,operation_key,receipt_ids,subtotal,minor_exponent,amount_minor,rounding_adjustment,days_until_due)
 VALUES(a,k,sorted,total,exp,round(total*power(10::numeric,exp)),round(total,exp)-total,days) RETURNING * INTO d;
 INSERT INTO kaana_peable_billing.draft_receipts(receipt_id,draft_id,amount)
 SELECT r.id,d.id,r.invoiced-coalesce((SELECT sum(f.invoiced) FROM kaana_billing.refunds f WHERE f.receipt_id=r.id),0)
 FROM kaana_billing.receipts r WHERE r.id=ANY(sorted);
 RETURN kaana_peable_billing.draft_view(d);
END $$;
CREATE FUNCTION kaana_peable_billing.read_draft(did text) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,kaana_peable_billing AS $$
DECLARE d kaana_peable_billing.drafts;
BEGIN
 SELECT * INTO d FROM kaana_peable_billing.drafts WHERE id=did;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='draft missing'; END IF;
 IF EXISTS(SELECT 1 FROM kaana_peable_billing.draft_receipts x JOIN kaana_billing.receipts r ON r.id=x.receipt_id
 WHERE x.draft_id=did AND x.amount<>r.invoiced-coalesce((SELECT sum(f.invoiced) FROM kaana_billing.refunds f WHERE f.receipt_id=r.id),0))
 THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='compensation requires reconciliation'; END IF;
 RETURN kaana_peable_billing.draft_view(d);
END $$;
CREATE FUNCTION kaana_peable_billing.claim_step(did text,step text) RETURNS boolean
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,kaana_peable_billing AS $$
DECLARE d kaana_peable_billing.drafts;
BEGIN
 SELECT * INTO d FROM kaana_peable_billing.drafts WHERE id=did FOR UPDATE;
 PERFORM kaana_peable_billing.read_draft(did);
 IF step='invoice' AND NOT d.invoice_attempted THEN
  UPDATE kaana_peable_billing.drafts SET invoice_attempted=true WHERE id=did;RETURN true;
 ELSIF step='item' AND d.invoice_id IS NOT NULL AND NOT d.item_attempted THEN
  UPDATE kaana_peable_billing.drafts SET item_attempted=true WHERE id=did;RETURN true;
 ELSIF step NOT IN('invoice','item') THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='invalid step';
 END IF;
 RETURN false;
END $$;
CREATE FUNCTION kaana_peable_billing.record_step(did text,step text,remote text) RETURNS void
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,kaana_peable_billing AS $$
DECLARE d kaana_peable_billing.drafts;
BEGIN
 IF remote IS NULL OR length(remote) NOT BETWEEN 1 AND 128 THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='invalid external reference'; END IF;
 SELECT * INTO d FROM kaana_peable_billing.drafts WHERE id=did FOR UPDATE;
 PERFORM kaana_peable_billing.read_draft(did);
 IF step='invoice' AND d.invoice_attempted AND (d.invoice_id IS NULL OR d.invoice_id=remote) THEN
  UPDATE kaana_peable_billing.drafts SET invoice_id=remote WHERE id=did;
 ELSIF step='item' AND d.item_attempted AND d.invoice_id IS NOT NULL AND (d.item_id IS NULL OR d.item_id=remote) THEN
  UPDATE kaana_peable_billing.drafts SET item_id=remote WHERE id=did;
 ELSE RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='external reference conflict'; END IF;
END $$;
REVOKE ALL ON ALL TABLES IN SCHEMA kaana_peable_billing FROM PUBLIC;
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA kaana_peable_billing FROM PUBLIC;
GRANT USAGE ON SCHEMA kaana_peable_billing TO kaana_peable_billing_admin,kaana_peable_billing_runtime;
GRANT EXECUTE ON FUNCTION kaana_peable_billing.bind_customer(text,text,text),kaana_peable_billing.prepare_draft(text,text,text[],integer) TO kaana_peable_billing_admin;
GRANT EXECUTE ON FUNCTION kaana_peable_billing.read_draft(text),kaana_peable_billing.claim_step(text,text),kaana_peable_billing.record_step(text,text,text) TO kaana_peable_billing_runtime;

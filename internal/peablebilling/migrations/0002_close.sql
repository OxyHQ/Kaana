-- Explicit separate migration. Does not enable an operator, select receipts or issue an invoice.
CREATE TABLE kaana_peable_billing.close_attempts (
 id text PRIMARY KEY DEFAULT ('kpc_'||replace(gen_random_uuid()::text,'-','')),
 draft_id text NOT NULL REFERENCES kaana_peable_billing.drafts(id),
 operation_key text NOT NULL CHECK(length(operation_key) BETWEEN 1 AND 128),
 revision text NOT NULL CHECK(revision ~ '^[a-f0-9]{64}$'),
 state text NOT NULL DEFAULT 'attempted' CHECK(state IN('attempted','closed')),
 evidence jsonb,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 closed_at timestamptz,
 UNIQUE(draft_id,operation_key),
 CHECK ((state='closed')=(evidence IS NOT NULL AND closed_at IS NOT NULL))
);
CREATE UNIQUE INDEX one_unresolved_close ON kaana_peable_billing.close_attempts(draft_id);
ALTER TABLE kaana_billing.journal DROP CONSTRAINT journal_kind_check;
ALTER TABLE kaana_billing.journal ADD CONSTRAINT journal_kind_check CHECK(kind IN('funding','hold','charge','release','expiry','refund','invoice_rounding'));

CREATE FUNCTION kaana_peable_billing.close_view(c kaana_peable_billing.close_attempts) RETURNS jsonb
 LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
 SELECT jsonb_build_object('id',c.id,'draftId',c.draft_id,'key',c.operation_key,'revision',c.revision,'state',c.state,'evidence',c.evidence)
$$;
CREATE FUNCTION kaana_peable_billing.read_close(did text,k text) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c kaana_peable_billing.close_attempts;
BEGIN
 SELECT * INTO c FROM kaana_peable_billing.close_attempts WHERE draft_id=did AND operation_key=k;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='close attempt missing'; END IF;
 RETURN kaana_peable_billing.close_view(c);
END $$;
CREATE FUNCTION kaana_peable_billing.claim_close(did text,k text,rev text) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d kaana_peable_billing.drafts; c kaana_peable_billing.close_attempts; fresh boolean:=false;
BEGIN
 -- Same account-first order as refund: no compensation can cross the durable claim.
 PERFORM 1 FROM kaana_billing.accounts a JOIN kaana_peable_billing.drafts x ON x.account_id=a.id WHERE x.id=did FOR UPDATE OF a;
 SELECT * INTO d FROM kaana_peable_billing.drafts WHERE id=did FOR UPDATE;
 IF NOT FOUND OR d.invoice_id IS NULL OR d.item_id IS NULL THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='delivered draft required'; END IF;
 PERFORM kaana_peable_billing.read_draft(did);
 SELECT * INTO c FROM kaana_peable_billing.close_attempts WHERE draft_id=did AND operation_key=k;
 IF FOUND THEN
  IF c.revision<>rev THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='close revision conflict'; END IF;
 ELSE
  INSERT INTO kaana_peable_billing.close_attempts(draft_id,operation_key,revision) VALUES(did,k,rev) RETURNING * INTO c;
  fresh:=true;
 END IF;
 RETURN jsonb_build_object('fresh',fresh,'attempt',kaana_peable_billing.close_view(c));
END $$;
CREATE FUNCTION kaana_peable_billing.refund_close_guard() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 -- The existing refund function holds this lock; also protect any privileged writer.
 PERFORM 1 FROM kaana_billing.accounts WHERE id=NEW.account_id FOR UPDATE;
 IF NEW.invoiced>0 AND EXISTS(SELECT 1 FROM kaana_peable_billing.draft_receipts r
 JOIN kaana_peable_billing.close_attempts c ON c.draft_id=r.draft_id
 WHERE r.receipt_id=NEW.receipt_id)
 THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='issued or ambiguous invoice requires compensation reconciliation'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER refund_invoice_close BEFORE INSERT ON kaana_billing.refunds FOR EACH ROW EXECUTE FUNCTION kaana_peable_billing.refund_close_guard();
CREATE FUNCTION kaana_peable_billing.record_close(did text,k text,ev jsonb) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d kaana_peable_billing.drafts; c kaana_peable_billing.close_attempts; base numeric; netbase numeric; tax numeric; total numeric;
BEGIN
 PERFORM 1 FROM kaana_billing.accounts a JOIN kaana_peable_billing.drafts x ON x.account_id=a.id WHERE x.id=did FOR UPDATE OF a;
 SELECT * INTO d FROM kaana_peable_billing.drafts WHERE id=did FOR UPDATE;
 SELECT * INTO c FROM kaana_peable_billing.close_attempts WHERE draft_id=did AND operation_key=k FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='close attempt unavailable'; END IF;
 IF c.state='closed' THEN
  IF c.evidence<>ev THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='close evidence conflict'; END IF;
  RETURN kaana_peable_billing.close_view(c);
 END IF;
 PERFORM kaana_peable_billing.read_draft(did);
 IF jsonb_typeof(ev)<>'object' OR (SELECT count(*) FROM jsonb_object_keys(ev))<>13
 OR ev->>'invoiceId' IS DISTINCT FROM d.invoice_id OR ev->>'revision' IS DISTINCT FROM c.revision
 OR ev->>'status' NOT IN('open','paid') OR length(ev->>'number') NOT BETWEEN 1 AND 256
 OR ev->>'digest' !~ '^[a-f0-9]{64}$'
 OR ev->>'baseMinor' !~ '^(0|[1-9][0-9]{0,30})$' OR ev->>'taxMinor' !~ '^(0|[1-9][0-9]{0,30})$' OR ev->>'totalMinor' !~ '^(0|[1-9][0-9]{0,31})$'
 OR ev->>'netBaseMinor' !~ '^(0|[1-9][0-9]{0,30})$'
 OR ev->>'startingBalanceMinor' !~ '^(0|-?[1-9][0-9]{0,31})$' OR ev->>'endingBalanceMinor' !~ '^(0|-?[1-9][0-9]{0,31})$'
 OR ev->>'amountDueMinor' !~ '^(0|[1-9][0-9]{0,31})$' OR ev->>'amountPaidMinor' !~ '^(0|[1-9][0-9]{0,31})$'
 OR EXISTS(SELECT 1 FROM jsonb_each(ev) v WHERE jsonb_typeof(v.value)<>'string')
 OR NOT ev ?& ARRAY['invoiceId','revision','status','number','digest','baseMinor','netBaseMinor','taxMinor','totalMinor','startingBalanceMinor','endingBalanceMinor','amountDueMinor','amountPaidMinor']
 THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='invalid close evidence'; END IF;
 base:=(ev->>'baseMinor')::numeric;netbase:=(ev->>'netBaseMinor')::numeric;tax:=(ev->>'taxMinor')::numeric;total:=(ev->>'totalMinor')::numeric;
 IF base<>d.amount_minor OR netbase>base OR total<base OR total<>netbase+tax THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='invoice amount mismatch'; END IF;
 -- Issue is not payment. Only the already-reviewed currency-boundary adjustment changes debt.
 UPDATE kaana_billing.accounts SET invoiced=invoiced+d.rounding_adjustment WHERE id=d.account_id;
 IF d.rounding_adjustment>0 THEN
  PERFORM kaana_billing.post(d.account_id,c.id,'invoice_rounding','invoice_credit','revenue',d.rounding_adjustment);
 ELSIF d.rounding_adjustment<0 THEN
  PERFORM kaana_billing.post(d.account_id,c.id,'invoice_rounding','revenue','invoice_credit',-d.rounding_adjustment);
 END IF;
 UPDATE kaana_peable_billing.close_attempts SET state='closed',evidence=ev,closed_at=clock_timestamp() WHERE id=c.id RETURNING * INTO c;
 RETURN kaana_peable_billing.close_view(c);
END $$;
REVOKE ALL ON kaana_peable_billing.close_attempts FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION kaana_peable_billing.close_view(kaana_peable_billing.close_attempts),kaana_peable_billing.read_close(text,text),kaana_peable_billing.claim_close(text,text,text),kaana_peable_billing.refund_close_guard(),kaana_peable_billing.record_close(text,text,jsonb) FROM PUBLIC;
GRANT USAGE ON SCHEMA kaana_peable_billing TO kaana_peable_billing_closer;
GRANT EXECUTE ON FUNCTION kaana_peable_billing.read_draft(text),kaana_peable_billing.read_close(text,text),kaana_peable_billing.claim_close(text,text,text),kaana_peable_billing.record_close(text,text,jsonb) TO kaana_peable_billing_closer;

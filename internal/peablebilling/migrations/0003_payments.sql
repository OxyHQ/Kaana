-- Explicit reconciliation only; no processor calls, funds, roles or activation.
CREATE TABLE kaana_peable_billing.invoice_payments (
 id text PRIMARY KEY DEFAULT ('kpp_'||replace(gen_random_uuid()::text,'-','')),
 draft_id text NOT NULL UNIQUE REFERENCES kaana_peable_billing.drafts(id),
 close_id text NOT NULL UNIQUE REFERENCES kaana_peable_billing.close_attempts(id),
 merchant_id text NOT NULL,
 payment_intent_id text NOT NULL,
 charge_id text NOT NULL,
 principal numeric(38,12) NOT NULL CHECK(principal>0),
 evidence jsonb NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(merchant_id,payment_intent_id),
 UNIQUE(merchant_id,charge_id)
);
CREATE TRIGGER immutable_invoice_payment BEFORE UPDATE OR DELETE ON kaana_peable_billing.invoice_payments FOR EACH ROW EXECUTE FUNCTION kaana_billing.immutable();
ALTER TABLE kaana_billing.journal DROP CONSTRAINT journal_kind_check;
ALTER TABLE kaana_billing.journal ADD CONSTRAINT journal_kind_check CHECK(kind IN('funding','hold','charge','release','expiry','refund','invoice_rounding','invoice_payment'));

CREATE FUNCTION kaana_peable_billing.payment_view(p kaana_peable_billing.invoice_payments) RETURNS jsonb
 LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
 SELECT jsonb_build_object('id',p.id,'draftId',p.draft_id,'principal',p.principal::text,'evidence',p.evidence)
$$;
CREATE FUNCTION kaana_peable_billing.record_payment(did text, ev jsonb) RETURNS jsonb
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE d kaana_peable_billing.drafts; c kaana_peable_billing.close_attempts; p kaana_peable_billing.invoice_payments; principal numeric; debt numeric; identity jsonb;
BEGIN
 -- Serialize with admission, issuance rounding and all compensation on this account.
 SELECT a.invoiced INTO debt FROM kaana_billing.accounts a JOIN kaana_peable_billing.drafts x ON x.account_id=a.id WHERE x.id=did FOR UPDATE OF a;
 SELECT * INTO d FROM kaana_peable_billing.drafts WHERE id=did FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='invoice draft absent'; END IF;
 identity:=kaana_peable_billing.draft_view(d);
 SELECT * INTO c FROM kaana_peable_billing.close_attempts WHERE draft_id=did AND state='closed' FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='durable accepted invoice close required'; END IF;
 IF ev IS NULL OR jsonb_typeof(ev)<>'object' THEN RAISE EXCEPTION USING ERRCODE='KP001',MESSAGE='invalid payment evidence'; END IF;
 IF (SELECT count(*) FROM jsonb_object_keys(ev))<>10
 OR NOT ev ?& ARRAY['invoiceId','revision','merchantId','customerId','currency','paymentIntentId','chargeId','totalMinor','baseMinor','digest']
 OR EXISTS(SELECT 1 FROM jsonb_each(ev) v WHERE jsonb_typeof(v.value)<>'string')
 OR ev->>'invoiceId' IS DISTINCT FROM d.invoice_id
 OR ev->>'revision' IS DISTINCT FROM c.revision
 OR ev->>'merchantId' IS DISTINCT FROM identity->>'merchantId'
 OR ev->>'customerId' IS DISTINCT FROM identity->>'customerId'
 OR ev->>'currency' IS DISTINCT FROM identity->>'currency'
 OR ev->>'totalMinor' IS DISTINCT FROM c.evidence->>'totalMinor'
 OR ev->>'baseMinor' IS DISTINCT FROM d.amount_minor::text
 OR ev->>'digest' !~ '^[a-f0-9]{64}$'
 OR length(ev->>'paymentIntentId') NOT BETWEEN 1 AND 256
 OR length(ev->>'chargeId') NOT BETWEEN 1 AND 256
 THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='payment proof does not match closed invoice'; END IF;
 SELECT * INTO p FROM kaana_peable_billing.invoice_payments WHERE draft_id=did;
 IF FOUND THEN
  IF p.evidence<>ev THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='payment evidence conflict'; END IF;
  RETURN kaana_peable_billing.payment_view(p);
 END IF;
 -- Tax is Peable's fiscal liability, not an inference-credit balance. Only the
 -- already-rounded Kaana principal retires its receivable; never mint prepaid funds.
 principal:=d.subtotal+d.rounding_adjustment;
 IF principal<=0 OR debt<principal THEN RAISE EXCEPTION USING ERRCODE='KP002',MESSAGE='invoice receivable cannot be reconciled'; END IF;
 INSERT INTO kaana_peable_billing.invoice_payments(draft_id,close_id,merchant_id,payment_intent_id,charge_id,principal,evidence)
 VALUES(d.id,c.id,identity->>'merchantId',ev->>'paymentIntentId',ev->>'chargeId',principal,ev) RETURNING * INTO p;
 UPDATE kaana_billing.accounts SET invoiced=invoiced-principal WHERE id=d.account_id;
 PERFORM kaana_billing.post(d.account_id,p.id,'invoice_payment','external_settlement','invoice_credit',principal);
 RETURN kaana_peable_billing.payment_view(p);
END $$;
REVOKE ALL ON kaana_peable_billing.invoice_payments FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION kaana_peable_billing.payment_view(kaana_peable_billing.invoice_payments),kaana_peable_billing.record_payment(text,jsonb) FROM PUBLIC;
GRANT USAGE ON SCHEMA kaana_peable_billing TO kaana_peable_billing_reconciler;
GRANT EXECUTE ON FUNCTION kaana_peable_billing.read_draft(text),kaana_peable_billing.read_close(text,text),kaana_peable_billing.record_payment(text,jsonb) TO kaana_peable_billing_reconciler;

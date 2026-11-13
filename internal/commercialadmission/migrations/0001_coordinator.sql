CREATE SCHEMA kaana_commercial;
REVOKE ALL ON SCHEMA kaana_commercial FROM PUBLIC;
CREATE TABLE kaana_commercial.admissions (
 admission_id text PRIMARY KEY REFERENCES kaana_admission.admissions(id),
 account_id text NOT NULL REFERENCES kaana_billing.accounts(id),
 hold_id text NOT NULL UNIQUE,
 policy_digest text NOT NULL CHECK (policy_digest ~ '^[0-9a-f]{64}$'),
 price_digest text NOT NULL CHECK (price_digest ~ '^[0-9a-f]{64}$'),
 FOREIGN KEY(hold_id,account_id) REFERENCES kaana_billing.holds(id,account_id)
);
CREATE TABLE kaana_commercial.outcomes (
 admission_id text PRIMARY KEY REFERENCES kaana_commercial.admissions(admission_id),
 evidence_digest text NOT NULL CHECK (evidence_digest ~ '^[0-9a-f]{64}$'),
 status text NOT NULL CHECK (status IN ('charged','zero_charge','rating_failed','hold_expired')),
 receipt_id text REFERENCES kaana_billing.receipts(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK ((status IN ('charged','zero_charge')) = (receipt_id IS NOT NULL))
);
CREATE FUNCTION kaana_commercial.immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'immutable commercial coordination record'; END;
$$;
CREATE TRIGGER immutable_commercial_admission BEFORE UPDATE OR DELETE ON kaana_commercial.admissions FOR EACH ROW EXECUTE FUNCTION kaana_commercial.immutable();
CREATE TRIGGER immutable_commercial_outcome BEFORE UPDATE OR DELETE ON kaana_commercial.outcomes FOR EACH ROW EXECUTE FUNCTION kaana_commercial.immutable();
REVOKE ALL ON ALL TABLES IN SCHEMA kaana_commercial FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA kaana_commercial FROM PUBLIC;
GRANT USAGE ON SCHEMA kaana_commercial TO kaana_billing_runtime;
GRANT SELECT,INSERT ON kaana_commercial.admissions,kaana_commercial.outcomes TO kaana_billing_runtime;

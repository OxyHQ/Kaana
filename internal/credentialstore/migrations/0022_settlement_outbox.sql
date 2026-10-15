-- The settlement outbox (Kaana ADR 0002, decision 3). Every request Kaana
-- serves on https://kaana.ai/v1 under an Oxy grant settles exactly once: when
-- it ends (success, error, cancellation or the close of a realtime session) its
-- settlement is written here, in the same transaction as its provider cost
-- events, and a worker delivers it to Oxy signed with Kaana's settlement key.
--
-- A row holds the exact JSON bytes delivered, so every redelivery is the same
-- message under a fresh signature. It holds units and identities, never
-- money, a prompt, an output or a credential. Oxy settles idempotently on
-- grant_id and reconciles by reading this table through
-- kaana_read_settlements, oldest first.
SET LOCAL lock_timeout = '5s';

CREATE TABLE inference_settlement_outbox (
    outbox_seq BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    settlement_id TEXT PRIMARY KEY CHECK (length(settlement_id) BETWEEN 1 AND 128),
    -- One grant settles once: a second settlement for the same grant is the
    -- same settlement or a conflict, never a second row.
    grant_id TEXT NOT NULL UNIQUE CHECK (length(grant_id) BETWEEN 1 AND 128),
    request_id TEXT NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
    body TEXT NOT NULL CHECK (length(body) BETWEEN 2 AND 1048576),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- Delivery state. A row is due when it is undelivered and its lease
    -- (next_attempt_at) has passed; claiming one pushes the lease forward so
    -- two tasks never deliver it at once.
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    last_failure TEXT CHECK (last_failure IS NULL OR last_failure ~ '^[a-z][a-z0-9_]{0,63}$'),
    delivered_at TIMESTAMPTZ,
    delivered_status TEXT CHECK (delivered_status IN ('settled', 'duplicate')),
    CHECK ((delivered_at IS NULL) = (delivered_status IS NULL)),
    CHECK (jsonb_typeof(body::JSONB) = 'object')
);
REVOKE ALL ON inference_settlement_outbox FROM PUBLIC;

CREATE INDEX inference_settlement_outbox_due_idx
    ON inference_settlement_outbox (next_attempt_at)
    WHERE delivered_at IS NULL;

-- Enqueue one settlement. Called in the transaction that records the
-- request's provider cost events. Returns 'enqueued' for a new row, 'replayed'
-- for the identical row already present (a retried transaction), and refuses
-- anything else that claims the same settlement or grant.
CREATE FUNCTION kaana_enqueue_settlement(
    p_settlement_id TEXT,
    p_grant_id TEXT,
    p_request_id TEXT,
    p_body TEXT
) RETURNS TEXT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_enqueue_settlement$
DECLARE
    existing public.inference_settlement_outbox%ROWTYPE;
    parsed JSONB;
BEGIN
    IF p_settlement_id IS NULL OR length(p_settlement_id) NOT BETWEEN 1 AND 128
       OR p_grant_id IS NULL OR length(p_grant_id) NOT BETWEEN 1 AND 128
       OR p_request_id IS NULL OR length(p_request_id) NOT BETWEEN 1 AND 128
       OR p_body IS NULL OR length(p_body) NOT BETWEEN 2 AND 1048576 THEN
        RAISE EXCEPTION 'settlement is invalid';
    END IF;
    parsed := p_body::JSONB;
    IF jsonb_typeof(parsed) <> 'object'
       OR parsed->>'settlementId' IS DISTINCT FROM p_settlement_id
       OR parsed->>'grantId' IS DISTINCT FROM p_grant_id
       OR parsed->>'requestId' IS DISTINCT FROM p_request_id THEN
        RAISE EXCEPTION 'settlement identity does not match its body';
    END IF;
    INSERT INTO public.inference_settlement_outbox (settlement_id, grant_id, request_id, body)
    VALUES (p_settlement_id, p_grant_id, p_request_id, p_body)
    ON CONFLICT DO NOTHING;
    IF FOUND THEN
        RETURN 'enqueued';
    END IF;
    SELECT * INTO existing FROM public.inference_settlement_outbox
     WHERE settlement_id = p_settlement_id OR grant_id = p_grant_id
     LIMIT 1;
    IF existing.settlement_id = p_settlement_id AND existing.grant_id = p_grant_id
       AND existing.request_id = p_request_id AND existing.body = p_body THEN
        RETURN 'replayed';
    END IF;
    RAISE EXCEPTION 'settlement identity conflict';
END
$kaana_enqueue_settlement$;

-- Claim up to p_limit due settlements for delivery, oldest first, leasing each
-- for p_lease. SKIP LOCKED lets every task run a worker without delivering one
-- row twice at the same time; the lease covers a worker that dies mid-delivery.
CREATE FUNCTION kaana_claim_settlements(
    p_limit INTEGER,
    p_lease INTERVAL
) RETURNS TABLE (
    settlement_id TEXT,
    body TEXT,
    attempts INTEGER
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_claim_settlements$
BEGIN
    IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 100
       OR p_lease IS NULL OR p_lease < INTERVAL '1 second' OR p_lease > INTERVAL '10 minutes' THEN
        RAISE EXCEPTION 'settlement claim is invalid';
    END IF;
    RETURN QUERY
        WITH due AS (
            SELECT o.settlement_id
              FROM public.inference_settlement_outbox o
             WHERE o.delivered_at IS NULL AND o.next_attempt_at <= clock_timestamp()
             ORDER BY o.next_attempt_at, o.outbox_seq
             LIMIT p_limit
               FOR UPDATE SKIP LOCKED
        )
        UPDATE public.inference_settlement_outbox o
           SET attempts = o.attempts + 1,
               next_attempt_at = clock_timestamp() + p_lease
          FROM due
         WHERE o.settlement_id = due.settlement_id
        RETURNING o.settlement_id, o.body, o.attempts;
END
$kaana_claim_settlements$;

-- Record Oxy's acknowledgement. Delivered once is delivered: a later
-- acknowledgement of the same row is a replay and changes nothing.
CREATE FUNCTION kaana_mark_settlement_delivered(
    p_settlement_id TEXT,
    p_status TEXT
) RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_mark_settlement_delivered$
BEGIN
    IF p_status IS NULL OR p_status NOT IN ('settled', 'duplicate') THEN
        RAISE EXCEPTION 'settlement acknowledgement is invalid';
    END IF;
    UPDATE public.inference_settlement_outbox
       SET delivered_at = clock_timestamp(), delivered_status = p_status, last_failure = NULL
     WHERE settlement_id = p_settlement_id AND delivered_at IS NULL;
    RETURN FOUND;
END
$kaana_mark_settlement_delivered$;

-- Record a failed delivery and when to try again. The failure is a closed,
-- content-free class (e.g. oxy_unavailable), never a response body.
CREATE FUNCTION kaana_defer_settlement(
    p_settlement_id TEXT,
    p_retry_at TIMESTAMPTZ,
    p_failure TEXT
) RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_defer_settlement$
BEGIN
    IF p_retry_at IS NULL OR p_failure IS NULL OR p_failure !~ '^[a-z][a-z0-9_]{0,63}$' THEN
        RAISE EXCEPTION 'settlement deferral is invalid';
    END IF;
    UPDATE public.inference_settlement_outbox
       SET next_attempt_at = p_retry_at, last_failure = p_failure
     WHERE settlement_id = p_settlement_id AND delivered_at IS NULL;
    RETURN FOUND;
END
$kaana_defer_settlement$;

-- What Oxy reconciles against: every settlement after a position, delivered or
-- not, oldest first. A row is visible only once its writing transaction is
-- safely in the past, so a reader can never pass a position a late commit
-- would then fill.
CREATE FUNCTION kaana_read_settlements(
    p_after_position BIGINT,
    p_limit INTEGER
) RETURNS TABLE (
    outbox_seq BIGINT,
    body TEXT
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $kaana_read_settlements$
BEGIN
    IF p_after_position IS NULL OR p_after_position < 0 OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 500 THEN
        RAISE EXCEPTION 'settlement page is invalid';
    END IF;
    RETURN QUERY
        SELECT o.outbox_seq, o.body
          FROM public.inference_settlement_outbox o
         WHERE o.outbox_seq > p_after_position
           AND o.created_at < clock_timestamp() - INTERVAL '15 seconds'
         ORDER BY o.outbox_seq
         LIMIT p_limit;
END
$kaana_read_settlements$;

REVOKE ALL ON FUNCTION kaana_enqueue_settlement(TEXT, TEXT, TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_claim_settlements(INTEGER, INTERVAL) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_mark_settlement_delivered(TEXT, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_defer_settlement(TEXT, TIMESTAMPTZ, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_read_settlements(BIGINT, INTEGER) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kaana_enqueue_settlement(TEXT, TEXT, TEXT, TEXT) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_claim_settlements(INTEGER, INTERVAL) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_mark_settlement_delivered(TEXT, TEXT) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_defer_settlement(TEXT, TIMESTAMPTZ, TEXT) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_read_settlements(BIGINT, INTEGER) TO kaana_runtime;

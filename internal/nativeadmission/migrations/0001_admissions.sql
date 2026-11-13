CREATE SCHEMA kaana_admission;
REVOKE ALL ON SCHEMA kaana_admission FROM PUBLIC;

CREATE TABLE kaana_admission.enrollments (
    id TEXT PRIMARY KEY,
    application_id TEXT NOT NULL,
    environment TEXT NOT NULL CHECK (environment IN ('production','staging','development')),
    enabled BOOLEAN NOT NULL DEFAULT false,
    active_policy_id TEXT,
    activation_at TIMESTAMPTZ,
    legacy_fence_complete BOOLEAN NOT NULL DEFAULT false,
    legacy_fence_id TEXT,
    legacy_fence_digest TEXT CHECK (legacy_fence_digest ~ '^[0-9a-f]{64}$'),
    legacy_fence_activated_at TIMESTAMPTZ,
    UNIQUE(application_id, environment),
    CHECK (NOT enabled OR (activation_at IS NOT NULL AND legacy_fence_complete AND active_policy_id IS NOT NULL))
);

CREATE TABLE kaana_admission.policy_versions (
    id TEXT PRIMARY KEY,
    enrollment_id TEXT NOT NULL REFERENCES kaana_admission.enrollments(id),
    policy_key TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version>0),
    digest TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    document JSONB NOT NULL,
    UNIQUE(id,enrollment_id),
    UNIQUE(policy_key,version),
    UNIQUE(enrollment_id,digest)
);

ALTER TABLE kaana_admission.enrollments ADD CONSTRAINT active_policy_same_enrollment
    FOREIGN KEY(active_policy_id,id) REFERENCES kaana_admission.policy_versions(id,enrollment_id);

CREATE TABLE kaana_admission.daily_windows (
    enrollment_id TEXT NOT NULL REFERENCES kaana_admission.enrollments(id),
    utc_day DATE NOT NULL,
    admitted BIGINT NOT NULL CHECK (admitted>=0),
    PRIMARY KEY(enrollment_id,utc_day)
);

-- A cutover imports the previous authority's still-retryable keys before
-- enabling the enrollment. A legacy retry never becomes a native execution.
CREATE TABLE kaana_admission.legacy_fences (
    id TEXT PRIMARY KEY,
    enrollment_id TEXT NOT NULL REFERENCES kaana_admission.enrollments(id),
    idempotency_scope TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    -- Old edge admissions predate body hashes; an unknown hash still fences execution.
    body_digest TEXT CHECK (body_digest ~ '^[0-9a-f]{64}$'),
    legacy_admission_id TEXT NOT NULL,
    legacy_request_id TEXT,
    UNIQUE(idempotency_scope,idempotency_key)
);

-- Each reviewed roster/export refresh is immutable audit evidence. Tombstones
-- remain in legacy_fences; a new manifest may only extend their coverage.
CREATE TABLE kaana_admission.fence_imports (
    id TEXT PRIMARY KEY,
    enrollment_id TEXT NOT NULL REFERENCES kaana_admission.enrollments(id),
    policy_id TEXT NOT NULL REFERENCES kaana_admission.policy_versions(id),
    manifest_digest TEXT NOT NULL CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
    fence_id TEXT NOT NULL,
    activated_at TIMESTAMPTZ NOT NULL,
    exported_at TIMESTAMPTZ NOT NULL,
    credential_ids JSONB NOT NULL,
    entry_count BIGINT NOT NULL CHECK (entry_count>=0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(enrollment_id,manifest_digest)
);

CREATE TABLE kaana_admission.admissions (
    id TEXT PRIMARY KEY,
    enrollment_id TEXT NOT NULL REFERENCES kaana_admission.enrollments(id),
    policy_id TEXT NOT NULL REFERENCES kaana_admission.policy_versions(id),
    request_id TEXT NOT NULL UNIQUE,
    generation_id TEXT UNIQUE,
    authority TEXT NOT NULL DEFAULT 'kaana' CHECK (authority='kaana'),
    principal JSONB NOT NULL,
    delegated_user_id TEXT,
    operation TEXT NOT NULL,
    body_digest TEXT NOT NULL CHECK (body_digest ~ '^[0-9a-f]{64}$'),
    idempotency_scope TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    plan JSONB NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('executing','pending_settlement','settled','abandoned')),
    admitted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    lease_until TIMESTAMPTZ NOT NULL,
    UNIQUE(idempotency_scope,idempotency_key)
);
CREATE INDEX native_active_admissions ON kaana_admission.admissions(enrollment_id,lease_until) WHERE state='executing';

-- A provider completion is durable before a public terminal event. Rating and
-- provider cost persistence can resume after a worker crash without inference.
CREATE TABLE kaana_admission.completions (
    admission_id TEXT PRIMARY KEY REFERENCES kaana_admission.admissions(id),
    digest TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    report JSONB NOT NULL,
    provider_events JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE kaana_admission.settlements (
    id TEXT PRIMARY KEY,
    admission_id TEXT NOT NULL UNIQUE REFERENCES kaana_admission.admissions(id),
    digest TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    report JSONB,
    generation JSONB,
    outcome TEXT NOT NULL CHECK (outcome IN ('completed','partial','failed','cancelled')),
    rating_failure TEXT CHECK (rating_failure IN ('model_not_priced','unpriced_unit','admission_quote_exceeded')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION kaana_admission.reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'native admission immutable record';
END;
$$;
CREATE TRIGGER immutable_native_policy BEFORE UPDATE OR DELETE ON kaana_admission.policy_versions FOR EACH ROW EXECUTE FUNCTION kaana_admission.reject_mutation();
CREATE TRIGGER immutable_native_completion BEFORE UPDATE OR DELETE ON kaana_admission.completions FOR EACH ROW EXECUTE FUNCTION kaana_admission.reject_mutation();
CREATE TRIGGER immutable_native_settlement BEFORE UPDATE OR DELETE ON kaana_admission.settlements FOR EACH ROW EXECUTE FUNCTION kaana_admission.reject_mutation();
CREATE TRIGGER immutable_fence_import BEFORE UPDATE OR DELETE ON kaana_admission.fence_imports FOR EACH ROW EXECUTE FUNCTION kaana_admission.reject_mutation();
CREATE TRIGGER immutable_legacy_fence BEFORE UPDATE OR DELETE ON kaana_admission.legacy_fences FOR EACH ROW EXECUTE FUNCTION kaana_admission.reject_mutation();
-- Row locking needs UPDATE privilege. Keep that privilege behind a fixed,
-- read-only function rather than letting inference workers edit enrollment.
CREATE FUNCTION kaana_admission.lock_enrollment(application TEXT, environment_name TEXT)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM 1 FROM kaana_admission.enrollments
    WHERE application_id=application AND environment=environment_name FOR UPDATE;
END;
$$;
REVOKE ALL ON ALL TABLES IN SCHEMA kaana_admission FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA kaana_admission FROM PUBLIC;
GRANT USAGE ON SCHEMA kaana_admission TO kaana_admission_runtime,kaana_admission_admin;
GRANT EXECUTE ON FUNCTION kaana_admission.lock_enrollment(TEXT,TEXT) TO kaana_admission_runtime;
GRANT SELECT ON ALL TABLES IN SCHEMA kaana_admission TO kaana_admission_runtime;
GRANT INSERT ON kaana_admission.admissions TO kaana_admission_runtime;
GRANT UPDATE(state,generation_id) ON kaana_admission.admissions TO kaana_admission_runtime;
GRANT INSERT,UPDATE ON kaana_admission.daily_windows TO kaana_admission_runtime;
GRANT INSERT ON kaana_admission.settlements,kaana_admission.completions TO kaana_admission_runtime;
GRANT SELECT,INSERT ON kaana_admission.enrollments,kaana_admission.policy_versions,kaana_admission.legacy_fences,kaana_admission.fence_imports TO kaana_admission_admin;
GRANT UPDATE ON kaana_admission.enrollments TO kaana_admission_admin;
GRANT SELECT ON kaana_admission.admissions TO kaana_admission_admin;
GRANT USAGE ON SCHEMA public TO kaana_admission_runtime;
GRANT EXECUTE ON FUNCTION public.kaana_record_provider_attempt_events(JSONB) TO kaana_admission_runtime;
DO $$ BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO kaana_admission_runtime,kaana_admission_admin',current_database());
END $$;

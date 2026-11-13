-- Preserve the application's already-consumed UTC-day quota during authority
-- transfer. The component is separate from the combined runtime counter so a
-- reviewed refresh can add only its delta without replacing native admissions.
CREATE TABLE kaana_admission.legacy_daily_usage (
 enrollment_id text NOT NULL REFERENCES kaana_admission.enrollments(id),
 utc_day date NOT NULL,
 admitted bigint NOT NULL CHECK (admitted >= 0 AND admitted <= 9007199254740991),
 max_requests_per_utc_day bigint NOT NULL CHECK (max_requests_per_utc_day > 0),
 manifest_digest text NOT NULL CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
 exported_at timestamptz NOT NULL,
 PRIMARY KEY(enrollment_id,utc_day)
);
REVOKE ALL ON kaana_admission.legacy_daily_usage FROM PUBLIC;
GRANT SELECT ON kaana_admission.legacy_daily_usage TO kaana_admission_runtime;
GRANT SELECT,INSERT,UPDATE ON kaana_admission.legacy_daily_usage,kaana_admission.daily_windows TO kaana_admission_admin;
ALTER TABLE kaana_admission.fence_imports ADD COLUMN daily_usage jsonb;

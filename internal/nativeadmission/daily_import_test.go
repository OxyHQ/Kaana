package nativeadmission

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func dailyManifest(t *testing.T, p Policy, admitted int64, day time.Time) ([]byte, ImportReview) {
	t.Helper()
	raw, review := testLegacyManifest(t, p)
	var m LegacyManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.ExportedAt = day.UTC().Format(time.RFC3339Nano)
	m.DailyUsage.UTCDay = day.UTC().Format("2006-01-02")
	m.DailyUsage.Admitted = admitted
	raw = manifestBytes(t, m)
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	review.ManifestSHA256 = m.SHA256
	return raw, review
}

func TestLegacyManifestV2ActualOxyFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/inference-fence-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	review := interopReview()
	review.ManifestSHA256 = "1236569dd36b002d16bca61529ebe24cfb12642b0e51a5e1ca425e1d55abf872"
	m, err := DecodeLegacyManifest(raw, review, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
	if err != nil || m.DailyUsage.Admitted != 1 || m.DailyUsage.MaxRequestsPerUTCDay != 5000 {
		t.Fatal(m.DailyUsage, err)
	}
}

func TestLegacyManifestDailyEvidenceAndApprovedRoster(t *testing.T) {
	raw, review := testLegacyManifest(t, policy())
	var base LegacyManifest
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	base.Population.CredentialIDs = append(base.Population.CredentialIDs, "workload-unapproved")
	// Historical secret credentials are tombstones, never newly eligible actors.
	for i := range base.Entries {
		base.Entries[i].CredentialID = "retired-secret"
	}
	base.DailyUsage.Admitted = int64(review.Policy.MaxPerUTCDay) + 1
	raw = manifestBytes(t, base)
	_ = json.Unmarshal(raw, &base)
	review.ManifestSHA256 = base.SHA256
	if _, err := DecodeLegacyManifest(raw, review, time.Now()); err != nil {
		t.Fatal("eligible superset/historical alias/over-limit usage refused", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*LegacyManifest)
	}{
		{"missing scope", func(m *LegacyManifest) { m.DailyUsage.Scope = "" }},
		{"wrong day", func(m *LegacyManifest) { m.DailyUsage.UTCDay = "2026-01-01" }},
		{"no effective cap", func(m *LegacyManifest) { m.DailyUsage.MaxRequestsPerUTCDay = 0 }},
		{"different cap", func(m *LegacyManifest) { m.DailyUsage.MaxRequestsPerUTCDay++ }},
		{"old version", func(m *LegacyManifest) { m.SchemaVersion = 1 }},
		{"unreviewed credential", func(m *LegacyManifest) { m.Population.CredentialIDs = []contract.CredentialID{"unknown"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := clone(base)
			tc.mutate(&m)
			data := manifestBytes(t, m)
			_ = json.Unmarshal(data, &m)
			r := review
			r.ManifestSHA256 = m.SHA256
			if _, err := DecodeLegacyManifest(data, r, time.Now()); err == nil {
				t.Fatal("invalid reviewed evidence accepted")
			}
		})
	}
}

func TestPostgresDailyCarryRefreshAndReplayPreserveNativeUsage(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	p.MaxPerUTCDay = 5
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := dailyManifest(t, p, 2, time.Now().Add(-time.Second))
	if _, err := s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(ctx, storeRequest(t, p, "native-before-refresh", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	assertDaily := func(total, legacy int64) {
		t.Helper()
		var gotTotal, gotLegacy int64
		if err := pool.QueryRow(ctx, `SELECT d.admitted,l.admitted FROM kaana_admission.daily_windows d JOIN kaana_admission.legacy_daily_usage l USING(enrollment_id,utc_day)`).Scan(&gotTotal, &gotLegacy); err != nil || gotTotal != total || gotLegacy != legacy {
			t.Fatal("daily totals", gotTotal, gotLegacy, err)
		}
	}
	assertDaily(3, 2)
	p.Version = 2
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	refreshed, next := dailyManifest(t, p, 3, time.Now())
	if _, err := s.ImportLegacyFence(ctx, refreshed, next); err != nil {
		t.Fatal(err)
	}
	assertDaily(4, 3)
	if _, err := s.Admit(ctx, storeRequest(t, p, "last-remaining", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(ctx, storeRequest(t, p, "over-capacity", nil)); !errors.Is(err, ErrCapacity) {
		t.Fatal("legacy usage did not consume capacity", err)
	}
	if _, err := s.ImportLegacyFence(ctx, refreshed, next); err != nil {
		t.Fatal(err)
	}
	assertDaily(5, 3)
	p.Version = 3
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	decreased, bad := dailyManifest(t, p, 2, time.Now())
	if _, err := s.ImportLegacyFence(ctx, decreased, bad); !errors.Is(err, ErrConflict) {
		t.Fatal("legacy counter decreased", err)
	}
	assertDaily(5, 3)
	var version int
	if err := pool.QueryRow(ctx, `SELECT p.version FROM kaana_admission.enrollments e JOIN kaana_admission.policy_versions p ON p.id=e.active_policy_id`).Scan(&version); err != nil || version != 2 {
		t.Fatal("failed refresh changed policy", version, err)
	}
}

func TestPostgresDailyOverLimitAndOldWindowDoNotReset(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	p.MaxPerUTCDay = 2
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := dailyManifest(t, p, 3, time.Now())
	if _, err := s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(ctx, storeRequest(t, p, "over-legacy-cap", nil)); !errors.Is(err, ErrCapacity) {
		t.Fatal("over-limit import allowed execution", err)
	}
	// Previous UTC windows must not consume today's quota. Simulate the prior
	// completed day through persisted dates, without changing the production clock.
	if _, err := pool.Exec(ctx, `UPDATE kaana_admission.daily_windows SET utc_day=utc_day-1; UPDATE kaana_admission.legacy_daily_usage SET utc_day=utc_day-1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(ctx, storeRequest(t, p, "first-new-day", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	var total int64
	if err := pool.QueryRow(ctx, `SELECT admitted FROM kaana_admission.daily_windows WHERE utc_day=(clock_timestamp() AT TIME ZONE 'UTC')::date`).Scan(&total); err != nil || total != 1 {
		t.Fatal("replay rewrote new day", total, err)
	}
}

func TestPostgresDailyStaleInitialAndHistoricalExactReplay(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := dailyManifest(t, p, 4, time.Now().Add(-24*time.Hour))
	if _, err := s.ImportLegacyFence(ctx, raw, review); err == nil {
		t.Fatal("stale initial quota accepted")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM kaana_admission.legacy_fences`).Scan(&count); err != nil || count != 0 {
		t.Fatal("stale import partially persisted", count, err)
	}
	var m LegacyManifest
	_ = json.Unmarshal(raw, &m)
	daily, _ := json.Marshal(m.DailyUsage)
	// Seed a previously completed import to model reconnect after midnight.
	if _, err := pool.Exec(ctx, `INSERT INTO kaana_admission.fence_imports(id,enrollment_id,policy_id,manifest_digest,fence_id,activated_at,exported_at,credential_ids,entry_count,daily_usage) SELECT 'historical',e.id,p.id,$1,$2,$3,$4,'[]',0,$5 FROM kaana_admission.enrollments e JOIN kaana_admission.policy_versions p ON p.enrollment_id=e.id`, m.SHA256, m.FenceID, m.ActivatedAt, m.ExportedAt, daily); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kaana_admission.enrollments e SET active_policy_id=p.id,legacy_fence_id=$1,legacy_fence_digest=$2,legacy_fence_activated_at=$3,legacy_fence_complete=true FROM kaana_admission.policy_versions p WHERE p.enrollment_id=e.id`, m.FenceID, m.SHA256, m.ActivatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal("historical exact replay not acknowledged", err)
	}
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM kaana_admission.enrollments`).Scan(&enabled); err != nil || enabled {
		t.Fatal("replay reenabled stopped enrollment", enabled, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM kaana_admission.daily_windows`).Scan(&count); err != nil || count != 0 {
		t.Fatal("replay wrote quota", count, err)
	}
}

func TestPostgresDailyImportAdminRuntimeIsolationAndMigrationLedger(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("migration replay", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.kaana_admission_migrations`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	rolePool := func(role string) *pgxpool.Pool {
		t.Helper()
		config := pool.Config().Copy()
		config.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
			return err
		}
		p, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	admin := NewStore(rolePool("kaana_admission_admin"))
	p := policy()
	if err := admin.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := dailyManifest(t, p, 1, time.Now())
	if _, err := admin.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal("admin cannot import", err)
	}
	runtimePool := rolePool("kaana_admission_runtime")
	if _, err := runtimePool.Exec(ctx, `UPDATE kaana_admission.legacy_daily_usage SET admitted=0`); err == nil {
		t.Fatal("runtime edited legacy usage")
	}
	if _, err := NewStore(runtimePool).ImportLegacyFence(ctx, raw, review); err == nil {
		t.Fatal("runtime imported its own capacity")
	}
	if _, err := NewStore(runtimePool).Admit(ctx, storeRequest(t, p, "runtime-positive", nil)); err != nil {
		t.Fatal("runtime admission positive control", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE public.kaana_admission_migrations SET checksum='changed' WHERE version='0002'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("migration checksum ignored")
	}
}

func TestPostgresDailyMigrationUpgradesPublishedSchema(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	// Reconstruct the exact published 0001 schema/ledger, without changing 0001.
	if _, err := pool.Exec(ctx, `DROP TABLE kaana_admission.legacy_daily_usage; ALTER TABLE kaana_admission.fence_imports DROP COLUMN daily_usage; DELETE FROM public.kaana_admission_migrations WHERE version='0002'`); err != nil {
		t.Fatal(err)
	}
	p := policy()
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO kaana_admission.daily_windows SELECT id,(clock_timestamp() AT TIME ZONE 'UTC')::date,7 FROM kaana_admission.enrollments`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("published schema upgrade failed", err)
	}
	raw, review := dailyManifest(t, p, 3, time.Now())
	if _, err := s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := pool.QueryRow(ctx, `SELECT admitted FROM kaana_admission.daily_windows`).Scan(&total); err != nil || total != 10 {
		t.Fatal("upgrade lost existing native counter", total, err)
	}
	var checksum string
	if err := pool.QueryRow(ctx, `SELECT checksum FROM public.kaana_admission_migrations WHERE version='0001'`).Scan(&checksum); err != nil || checksum != digest([]byte(admissionMigration)) {
		t.Fatal("published migration changed", err)
	}
}

func TestPostgresAdmissionWaitsForLegacyDailyImport(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	p.MaxPerUTCDay = 1
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := dailyManifest(t, p, 1, time.Now())
	if _, err := pool.Exec(ctx, `CREATE FUNCTION kaana_admission.test_block_daily() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_advisory_xact_lock(93746212); RETURN NEW; END$$; CREATE TRIGGER test_daily_barrier BEFORE INSERT ON kaana_admission.legacy_daily_usage FOR EACH ROW EXECUTE FUNCTION kaana_admission.test_block_daily()`); err != nil {
		t.Fatal(err)
	}
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err = gate.Exec(ctx, `SELECT pg_advisory_xact_lock(93746212)`); err != nil {
		t.Fatal(err)
	}
	imported := make(chan error, 1)
	go func() { _, err := s.ImportLegacyFence(ctx, raw, review); imported <- err }()
	wait := func(query string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var ready bool
			if err := pool.QueryRow(ctx, query).Scan(&ready); err != nil {
				t.Fatal(err)
			}
			if ready {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("expected lock not observed")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE 'INSERT INTO kaana_admission.legacy_daily_usage%')`)
	admitted := make(chan error, 1)
	r := storeRequest(t, p, "after-import", nil)
	go func() { _, err := s.Admit(ctx, r); admitted <- err }()
	wait(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT kaana_admission.lock_enrollment%')`)
	select {
	case err := <-admitted:
		t.Fatal("admission passed incomplete import", err)
	default:
	}
	if err = gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-imported; err != nil {
		t.Fatal(err)
	}
	if err = <-admitted; !errors.Is(err, ErrCapacity) {
		t.Fatal("admission ignored imported capacity", err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM kaana_admission.admissions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("over-limit admission persisted", count, err)
	}
}

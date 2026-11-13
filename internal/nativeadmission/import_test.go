package nativeadmission

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func interopReview() ImportReview {
	p := policy()
	p.ApplicationID = "application-test"
	p.OwnerAccountID = "owner-test"
	p.CredentialIDs = []contract.CredentialID{"workload-handle-test"}
	return ImportReview{Policy: p, PolicyDigest: policyDigest(p), ManifestSHA256: "16b2a1a47aa61785150b6e3fce72e8e10aa306c5f7fca842f4b4d9fca65fe5c8", FenceID: "01900000-0000-7000-8000-000000000001", ActivatedAt: "2026-11-13T00:00:00.000Z"}
}
func TestLegacyManifestMatchesActualOxyCanonicalBytes(t *testing.T) {
	raw, err := os.ReadFile("testdata/legacy-interop.json")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("testdata/legacy-interop.payload")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	delete(value, "sha256")
	canonical, err := canonicalManifest(value)
	if err != nil || !bytes.Equal(canonical, expected) {
		t.Fatal("canonical bytes differ from actual Oxy serializer")
	}
	review := interopReview()
	manifest, err := DecodeLegacyManifest(raw, review, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Entries[0].Key != "caller:❤️:<>&:\u2028" {
		t.Fatal("opaque caller key changed")
	}
	for _, mutate := range []func(*ImportReview){func(r *ImportReview) { r.ManifestSHA256 = string(bytes.Repeat([]byte("0"), 64)) }, func(r *ImportReview) { r.FenceID = "other" }, func(r *ImportReview) { r.ActivatedAt = "2026-11-12T00:00:00.000Z" }, func(r *ImportReview) { r.Policy.OwnerAccountID = "different"; r.PolicyDigest = policyDigest(r.Policy) }, func(r *ImportReview) {
		r.Policy.CredentialIDs = []contract.CredentialID{"secret-backed-service"}
		r.PolicyDigest = policyDigest(r.Policy)
	}, func(r *ImportReview) { r.Policy.MaxConcurrent++ }} {
		changed := review
		changed.Policy = clone(review.Policy)
		mutate(&changed)
		if _, err = DecodeLegacyManifest(raw, changed, time.Now().Add(24*time.Hour)); err == nil {
			t.Fatal("review mismatch accepted")
		}
	}
}

func manifestBytes(t *testing.T, m LegacyManifest) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	delete(value, "sha256")
	canonical, err := canonicalManifest(value)
	if err != nil {
		t.Fatal(err)
	}
	m.SHA256 = digest(canonical)
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func testLegacyManifest(t *testing.T, p Policy) ([]byte, ImportReview) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Second)
	m := LegacyManifest{SchemaVersion: 2, Authority: "oxy", FenceID: "fence-reviewed", ActivatedAt: "2026-01-01T00:00:00.000Z", ExportedAt: now.Format(time.RFC3339Nano), DailyUsage: LegacyDailyUsage{Scope: "application_environment", UTCDay: now.Format("2006-01-02"), MaxRequestsPerUTCDay: p.MaxPerUTCDay}, Entries: []LegacyEntry{
		{CredentialID: p.CredentialIDs[0], KeyKind: "idempotency_key", Key: "caller:<>&:❤️:\u2028", LegacyAdmissionID: "metered:old", RequestID: "req-old", DigestDomain: "unknown"},
		{CredentialID: p.CredentialIDs[0], KeyKind: "request_id", Key: "req-old", LegacyAdmissionID: "metered:old", RequestID: "req-old", DigestDomain: "unknown"},
	}}
	m.Population.ApplicationID = p.ApplicationID
	m.Population.OwnerAccountID = p.OwnerAccountID
	m.Population.Environment = p.Environment
	m.Population.CredentialKind = "application"
	m.Population.CredentialIDs = clone(p.CredentialIDs)
	slices.SortFunc(m.Population.CredentialIDs, func(a, b contract.CredentialID) int { return utf16Compare(string(a), string(b)) })
	raw := manifestBytes(t, m)
	var signed LegacyManifest
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	return raw, ImportReview{Policy: p, PolicyDigest: policyDigest(p), ManifestSHA256: signed.SHA256, FenceID: m.FenceID, ActivatedAt: m.ActivatedAt}
}
func TestManifestRejectsUndrainedMissingAndUnorderedEvidence(t *testing.T) {
	raw, review := testLegacyManifest(t, policy())
	var base LegacyManifest
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*LegacyManifest){func(m *LegacyManifest) { m.Unresolved.Holds = 1 }, func(m *LegacyManifest) { m.Entries[0].BodyDigest = ptr("not-a-comparable-hash") }, func(m *LegacyManifest) { slices.Reverse(m.Entries) }, func(m *LegacyManifest) {
		m.Population.CredentialIDs = append(m.Population.CredentialIDs, m.Population.CredentialIDs[0])
	}, func(m *LegacyManifest) { m.Entries[1].Key = "different-request" }} {
		changed := clone(base)
		mutate(&changed)
		bad := manifestBytes(t, changed)
		var signed LegacyManifest
		_ = json.Unmarshal(bad, &signed)
		review.ManifestSHA256 = signed.SHA256
		if _, err := DecodeLegacyManifest(bad, review, time.Now()); err == nil {
			t.Fatal("invalid manifest accepted even with matching reviewed hash")
		}
	}
}

func TestPostgresFenceImportIsAtomicAndPreservesPolicyEpoch(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := testLegacyManifest(t, p)
	result, err := s.ImportLegacyFence(ctx, raw, review)
	if err != nil {
		t.Fatal(err)
	}
	if result.Entries != 2 {
		t.Fatal(result)
	}
	if _, err = s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal("same manifest not idempotent", err)
	}
	key := contract.IdempotencyKey("caller:<>&:❤️:\u2028")
	for _, r := range []AdmissionRequest{storeRequest(t, p, "req-fresh-old-key", &key), storeRequest(t, p, "req-old", nil)} {
		if _, err = s.Admit(ctx, r); !errors.Is(err, ErrLegacy) {
			t.Fatal("legacy alias executed", err)
		}
	}
	admitted, err := s.Admit(ctx, storeRequest(t, p, "req-native-v1", nil))
	if err != nil {
		t.Fatal(err)
	}
	v2 := p
	v2.Version = 2
	v2.MaxConcurrent = 1
	if err = s.RegisterPolicy(ctx, v2); err != nil {
		t.Fatal(err)
	}
	review.Policy = v2
	review.PolicyDigest = policyDigest(v2)
	if _, err = s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Admit(ctx, storeRequest(t, p, "req-stale-v1", nil)); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal("stale policy admitted", err)
	}
	if err = s.Settle(ctx, admitted.ID, reportFor(admitted), nil); err != nil {
		t.Fatal("old admitted snapshot cannot settle", err)
	}
	if _, err = s.Admit(ctx, storeRequest(t, v2, "req-new-v2", nil)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM kaana_admission.legacy_fences`).Scan(&count); err != nil || count != 2 {
		t.Fatal("tombstones duplicated", count, err)
	}
}
func TestPostgresFenceConflictRollsBackEveryTombstoneAndActivation(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := testLegacyManifest(t, p)
	if _, err := pool.Exec(ctx, `INSERT INTO kaana_admission.legacy_fences(id,enrollment_id,idempotency_scope,idempotency_key,legacy_admission_id,legacy_request_id) SELECT 'kif-conflict',id,'request','req-old','metered:different','req-old' FROM kaana_admission.enrollments`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLegacyFence(ctx, raw, review); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting old receipt accepted", err)
	}
	var count int
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled,(SELECT count(*) FROM kaana_admission.legacy_fences) FROM kaana_admission.enrollments`).Scan(&enabled, &count); err != nil || enabled || count != 1 {
		t.Fatal("partial cutover persisted", count, enabled, err)
	}
}

func TestCanonicalManifestSortsAstralBeforeBMPAndPreservesEscapes(t *testing.T) {
	got, err := canonicalManifest(map[string]any{"\ue000": "BMP", "😀": "astral", "literal": "\\u2028", "controls": "\b\f\n\r\t\x00"})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"controls\":\"\\b\\f\\n\\r\\t\\u0000\",\"literal\":\"\\\\u2028\",\"😀\":\"astral\",\"\ue000\":\"BMP\"}"
	if string(got) != want {
		t.Fatalf("canonical UTF16/escaping differs: %q", got)
	}
}
func TestPostgresReviewedRosterRefreshCannotDropCoverageOrRollBack(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := testLegacyManifest(t, p)
	if _, err := s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	var refreshed LegacyManifest
	if err := json.Unmarshal(raw, &refreshed); err != nil {
		t.Fatal(err)
	}
	refreshed.ExportedAt = time.Now().UTC().Format(time.RFC3339Nano)
	refreshed.Population.CredentialIDs = append(refreshed.Population.CredentialIDs, "workload-new")
	newRaw := manifestBytes(t, refreshed)
	_ = json.Unmarshal(newRaw, &refreshed)
	next := review
	next.ManifestSHA256 = refreshed.SHA256
	if _, err := s.ImportLegacyFence(ctx, newRaw, next); !errors.Is(err, ErrConflict) {
		t.Fatal("unversioned roster refresh accepted", err)
	}
	p2 := p
	p2.Version = 2
	p2.CredentialIDs = append(clone(p.CredentialIDs), "workload-new")
	if err := s.RegisterPolicy(ctx, p2); err != nil {
		t.Fatal(err)
	}
	next.Policy = p2
	next.PolicyDigest = policyDigest(p2)
	omitted := clone(refreshed)
	omitted.Entries = omitted.Entries[:1]
	missingRaw := manifestBytes(t, omitted)
	_ = json.Unmarshal(missingRaw, &omitted)
	missingReview := next
	missingReview.ManifestSHA256 = omitted.SHA256
	if _, err := s.ImportLegacyFence(ctx, missingRaw, missingReview); !errors.Is(err, ErrConflict) {
		t.Fatal("prior tombstone omitted", err)
	}
	if _, err := s.ImportLegacyFence(ctx, newRaw, next); err != nil {
		t.Fatal("reviewed roster refresh failed", err)
	}
	if _, err := s.ImportLegacyFence(ctx, raw, review); !errors.Is(err, ErrConflict) {
		t.Fatal("policy epoch rolled back", err)
	}
	var imports int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM kaana_admission.fence_imports`).Scan(&imports); err != nil || imports != 2 {
		t.Fatal("import evidence was replaced", imports, err)
	}
}
func TestPostgresLateImportFailureRollsBackEarlierRows(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := testLegacyManifest(t, p)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION kaana_admission.test_reject_second() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.idempotency_scope='request' THEN RAISE EXCEPTION 'injected late write failure'; END IF; RETURN NEW; END$$; CREATE TRIGGER test_second BEFORE INSERT ON kaana_admission.legacy_fences FOR EACH ROW EXECUTE FUNCTION kaana_admission.test_reject_second()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLegacyFence(ctx, raw, review); err == nil {
		t.Fatal("late database failure ignored")
	}
	var fences, imports int
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled,(SELECT count(*) FROM kaana_admission.legacy_fences),(SELECT count(*) FROM kaana_admission.fence_imports) FROM kaana_admission.enrollments`).Scan(&enabled, &fences, &imports); err != nil || enabled || fences != 0 || imports != 0 {
		t.Fatal("partial transaction persisted", enabled, fences, imports, err)
	}
}

func TestPostgresRefreshWaitsForAdmittedPolicyTransaction(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := testLegacyManifest(t, p)
	if _, err := s.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	p2 := p
	p2.Version = 2
	p2.MaxConcurrent = 1
	if err := s.RegisterPolicy(ctx, p2); err != nil {
		t.Fatal(err)
	}
	refreshRaw, next := dailyManifest(t, p2, 2, time.Now())
	if _, err := pool.Exec(ctx, `CREATE FUNCTION kaana_admission.test_block_insert() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_advisory_xact_lock(93746211); IF NEW.policy_id IS DISTINCT FROM (SELECT active_policy_id FROM kaana_admission.enrollments WHERE id=NEW.enrollment_id) THEN RAISE EXCEPTION 'stale policy crossed activation'; END IF; RETURN NEW; END$$; CREATE TRIGGER test_admission_barrier BEFORE INSERT ON kaana_admission.admissions FOR EACH ROW EXECUTE FUNCTION kaana_admission.test_block_insert()`); err != nil {
		t.Fatal(err)
	}
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err = gate.Exec(ctx, `SELECT pg_advisory_xact_lock(93746211)`); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan error, 1)
	r := storeRequest(t, p, "req-concurrent-old", nil)
	go func() { _, err := s.Admit(ctx, r); admitted <- err }()
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
				t.Fatal("expected PostgreSQL lock wait not observed")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event='advisory' AND query LIKE 'INSERT INTO kaana_admission.admissions%')`)
	imported := make(chan error, 1)
	go func() { _, err := s.ImportLegacyFence(ctx, refreshRaw, next); imported <- err }()
	wait(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE 'SELECT e.id,p.id,p.digest%')`)
	select {
	case err := <-imported:
		t.Fatal("refresh passed still-running admission", err)
	default:
	}
	if err = gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-admitted; err != nil {
		t.Fatal("admission before activation failed", err)
	}
	if err = <-imported; err != nil {
		t.Fatal("refresh failed", err)
	}
	var total, legacy int
	if err = pool.QueryRow(ctx, `SELECT d.admitted,l.admitted FROM kaana_admission.daily_windows d JOIN kaana_admission.legacy_daily_usage l USING(enrollment_id,utc_day)`).Scan(&total, &legacy); err != nil || total != 3 || legacy != 2 {
		t.Fatal("concurrent refresh lost native consumption", total, legacy, err)
	}
	if _, err = s.Admit(ctx, storeRequest(t, p, "req-after-refresh", nil)); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal("old plan admitted after activation", err)
	}
}

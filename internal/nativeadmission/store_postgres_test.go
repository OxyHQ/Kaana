package nativeadmission

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testDatabase(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test PostgreSQL configuration")
	}
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	name := newID("kaana_native_")
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(), `DROP DATABASE `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	own := config.Copy()
	own.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = credentialstore.MigrateDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store, pool, ctx
}

func enableTestPolicy(t *testing.T, s *Store, pool *pgxpool.Pool, ctx context.Context, p Policy) {
	t.Helper()
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kaana_admission.enrollments SET enabled=true,activation_at=clock_timestamp(),legacy_fence_complete=true`); err != nil {
		t.Fatal(err)
	}
}

func storeRequest(t *testing.T, p Policy, id string, key *contract.IdempotencyKey) AdmissionRequest {
	t.Helper()
	identity := principal()
	planner, err := NewPlanner([]Policy{p}, []Model{model("medium")})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Plan(identity, request())
	if err != nil {
		t.Fatal(err)
	}
	return AdmissionRequest{RequestID: contract.RequestID(id), Principal: identity, Operation: contract.GrantResponses, BodyDigest: digest([]byte(id)), IdempotencyKey: key, Plan: plan}
}

func TestPostgresNativeAdmissionIsDisabledAndVersioned(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	request := storeRequest(t, p, "req-disabled", nil)
	if err := s.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(ctx, request); !errors.Is(err, ErrDisabled) {
		t.Fatalf("unfenced enrollment admitted: %v", err)
	}
	p.MaxConcurrent++
	if err := s.RegisterPolicy(ctx, p); !errors.Is(err, ErrConflict) {
		t.Fatalf("rewritten policy accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kaana_admission.policy_versions SET version=99`); err == nil {
		t.Fatal("immutable policy changed")
	}
	if _, err := pool.Exec(ctx, `SET ROLE kaana_admission_runtime; UPDATE kaana_admission.enrollments SET enabled=true`); err == nil {
		t.Fatal("runtime enabled its own policy")
	}
}

func TestPostgresConcurrentNativeAdmissionAndPermanentKeys(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	p.MaxConcurrent = 1
	enableTestPolicy(t, s, pool, ctx, p)
	key := contract.IdempotencyKey("same-key")
	r := storeRequest(t, p, "req-one", &key)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Admit(ctx, r); results <- err }()
	}
	wg.Wait()
	close(results)
	fresh, replays := 0, 0
	for err := range results {
		if err == nil {
			fresh++
		} else if errors.Is(err, ErrReplay) {
			replays++
		} else {
			t.Fatal(err)
		}
	}
	if fresh != 1 || replays != 7 {
		t.Fatalf("fresh=%d replays=%d", fresh, replays)
	}
	next := storeRequest(t, p, "req-two", nil)
	if _, err := s.Admit(ctx, next); !errors.Is(err, ErrCapacity) {
		t.Fatalf("parallel capacity exceeded: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kaana_admission.admissions SET lease_until=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Expire(ctx); err != nil || n != 1 {
		t.Fatalf("lease recovery %d %v", n, err)
	}
	if _, err := s.Admit(ctx, r); !errors.Is(err, ErrReplay) {
		t.Fatalf("expired execution repeated: %v", err)
	}
	changed := r
	user := contract.UserID("another-person")
	changed.UserID = &user
	if _, err := s.Admit(ctx, changed); !errors.Is(err, ErrReplay) {
		t.Fatalf("attribution changed idempotency namespace: %v", err)
	}
	changed.BodyDigest = digest([]byte("different"))
	if _, err := s.Admit(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed body accepted: %v", err)
	}
	if _, err := s.Admit(ctx, next); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresLegacyFenceAndDailyCapacity(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	p.MaxPerUTCDay = 1
	p.MaxConcurrent = 2
	enableTestPolicy(t, s, pool, ctx, p)
	key := contract.IdempotencyKey("legacy")
	r := storeRequest(t, p, "req-legacy", &key)
	scope, k := idempotencyIdentity(r.Principal.CredentialID, &key, r.RequestID)
	if _, err := pool.Exec(ctx, `INSERT INTO kaana_admission.legacy_fences(id,enrollment_id,idempotency_scope,idempotency_key,body_digest,legacy_admission_id) SELECT $1,id,$2,$3,$4,'old-grant' FROM kaana_admission.enrollments`, newID("kif_"), scope, k, r.BodyDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(ctx, r); !errors.Is(err, ErrLegacy) {
		t.Fatalf("legacy grant executed: %v", err)
	}
	if _, err := s.Admit(ctx, storeRequest(t, p, "req-new", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kaana_admission.admissions SET lease_until=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Admit(ctx, storeRequest(t, p, "req-more", nil)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("daily limit exceeded after lease expiry: %v", err)
	}
}

func reportFor(a *Admission) contract.UsageReport {
	return contract.UsageReport{SchemaVersion: contract.UsageReportSchemaVersion, RequestID: a.RequestID, Attribution: a.Attribution(), Outcome: contract.OutcomeCompleted, UsageSource: contract.UsageProviderReported, Units: []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 3}}, ResolvedModelReference: "fixture/medium@v1", ServingProvider: "test", DeploymentID: "dep-test", StartedAt: contract.NewTimestamp(a.AdmittedAt), CompletedAt: contract.NewTimestamp(a.AdmittedAt.Add(time.Second))}
}

func TestPostgresNativeSettlementAndGenerationOwnership(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	p.MaxConcurrent = 1
	enableTestPolicy(t, s, pool, ctx, p)
	key := contract.IdempotencyKey("settlement")
	r := storeRequest(t, p, "req-settle", &key)
	a, err := s.Admit(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	report := reportFor(a)
	if err = s.Settle(ctx, a.ID, report, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(ctx, a.ID, report, nil); err != nil {
		t.Fatalf("same settlement: %v", err)
	}
	changed := report
	changed.Units = []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 4}}
	if err = s.Settle(ctx, a.ID, changed, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("settlement rewritten: %v", err)
	}
	body, err := s.Generation(ctx, a.Principal, nil, &key, nil)
	if err != nil {
		t.Fatal(err)
	}
	var record contract.MeteredGenerationRecord
	if err = json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if record.Kind != "metered_usage" || record.CustomerCharge.Status != "not_charged" || record.Tariff.Status != "quoted" {
		t.Fatalf("invalid native generation: %+v", record)
	}
	foreign := a.Principal
	foreign.OwnerAccountID = "another-owner"
	if _, err = s.Generation(ctx, foreign, nil, &key, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign owner read generation: %v", err)
	}
	if _, err = s.Admit(ctx, storeRequest(t, p, "req-after", nil)); err != nil {
		t.Fatalf("settlement did not release capacity: %v", err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM inference_settlement_outbox`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("native settlement reached Oxy outbox: %d %v", count, err)
	}
}

func TestPostgresCostsAndNativeSettlementCommitTogether(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	enableTestPolicy(t, s, pool, ctx, p)
	a, err := s.Admit(ctx, storeRequest(t, p, "req-cost", nil))
	if err != nil {
		t.Fatal(err)
	}
	report := reportFor(a)
	event := providercost.Event{RequestID: a.RequestID, AttemptIndex: 0, Provider: "test", KeyID: "native-key", DeploymentID: "dep-test", ModelReference: report.ResolvedModelReference, Source: providercost.SourceUnknown, Served: true, OccurredAt: a.AdmittedAt.Add(time.Second), Units: report.Units, Telemetry: providercost.AttemptTelemetry{StartedAt: a.AdmittedAt, Latency: time.Second, Outcome: providercost.AttemptSucceeded}}
	if err = s.Settle(ctx, a.ID, report, []providercost.Event{event}); err == nil {
		t.Fatal("cost with missing key was accepted")
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM kaana_admission.settlements`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed cost left settlement: %d %v", count, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position) VALUES('test','native-key','\x01','arn:aws:kms:us-west-2:123456789012:key/11111111-1111-1111-1111-111111111111',1)`); err != nil {
		t.Fatal(err)
	}
	roleConfig := pool.Config().Copy()
	roleConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE kaana_admission_runtime`)
		return err
	}
	runtimePool, err := pgxpool.NewWithConfig(ctx, roleConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	runtime := NewStore(runtimePool)
	if _, err = runtime.Admit(ctx, storeRequest(t, p, "req-runtime-role", nil)); err != nil {
		t.Fatalf("runtime admission: %v", err)
	}
	for _, query := range []string{
		`UPDATE kaana_admission.enrollments SET enabled=false`,
		`INSERT INTO kaana_admission.legacy_fences(id,enrollment_id,idempotency_scope,idempotency_key,body_digest,legacy_admission_id) SELECT 'forged',id,'credential:forged','key',repeat('a',64),'old' FROM kaana_admission.enrollments`,
		`UPDATE kaana_admission.policy_versions SET version=99`,
		`SELECT encrypted_secret FROM provider_credentials`,
	} {
		if _, err = runtimePool.Exec(ctx, query); err == nil {
			t.Fatal("runtime crossed policy/credential boundary")
		}
	}
	if err = runtime.Settle(ctx, a.ID, report, []providercost.Event{event}); err != nil {
		t.Fatalf("runtime settlement: %v", err)
	}

	if err = s.Settle(ctx, a.ID, report, []providercost.Event{event}); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM provider_cost_events WHERE request_id=$1`, a.RequestID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("cost count: %d %v", count, err)
	}
	event.AttemptIndex = 1
	if err = s.Settle(ctx, a.ID, report, []providercost.Event{event}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed cost replay accepted: %v", err)
	}
}

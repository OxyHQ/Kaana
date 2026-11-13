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
	if _, err := pool.Exec(ctx, `UPDATE kaana_admission.enrollments e SET enabled=true,activation_at=clock_timestamp(),legacy_fence_complete=true,active_policy_id=p.id FROM kaana_admission.policy_versions p WHERE p.enrollment_id=e.id AND p.policy_key=$1 AND p.version=$2`, p.ID, p.Version); err != nil {
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
	unknownKey := contract.IdempotencyKey("old:edge:key")
	unknownRequest := storeRequest(t, p, "req-legacy-unknown", &unknownKey)
	unknownScope, unknownValue := idempotencyIdentity(unknownRequest.Principal.CredentialID, &unknownKey, unknownRequest.RequestID)
	if _, err := pool.Exec(ctx, `INSERT INTO kaana_admission.legacy_fences(id,enrollment_id,idempotency_scope,idempotency_key,body_digest,legacy_admission_id) SELECT $1,id,$2,$3,NULL,'old-edge' FROM kaana_admission.enrollments`, newID("kif_"), unknownScope, unknownValue); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{unknownRequest.BodyDigest, digest([]byte("different-body"))} {
		unknownRequest.BodyDigest = body
		if _, err := s.Admit(ctx, unknownRequest); !errors.Is(err, ErrLegacy) {
			t.Fatalf("unknown legacy digest reran or invented conflict: %v", err)
		}
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
	foreign.OwnerAccountID = ptr(contract.AccountID("another-owner"))
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
		`UPDATE provider_credentials SET enabled=false`,
		`UPDATE kaana_admission.admissions SET plan='{}'::jsonb`,
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

func TestPostgresActivePolicyRejectsOldPlansAndPreservesOldSettlements(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	old := policy()
	enableTestPolicy(t, s, pool, ctx, old)
	admitted, err := s.Admit(ctx, storeRequest(t, old, "req-policy-v1", nil))
	if err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.Version++
	newer.MaxConcurrent = 1
	newer.MaxPerUTCDay = 2
	enableTestPolicy(t, s, pool, ctx, newer)
	if _, err = s.Admit(ctx, storeRequest(t, old, "req-stale-policy", nil)); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("stale version remained executable: %v", err)
	}
	if _, err = s.Admit(ctx, storeRequest(t, newer, "req-policy-v2", nil)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("old admission disappeared from new capacity: %v", err)
	}
	if err = s.Settle(ctx, admitted.ID, reportFor(admitted), nil); err != nil {
		t.Fatalf("old snapshot no longer settles: %v", err)
	}
	if _, err = s.Admit(ctx, storeRequest(t, newer, "req-policy-v2", nil)); err != nil {
		t.Fatalf("active version refused: %v", err)
	}
}

func TestPostgresRealtimeAdmissionUsesItsBoundedSessionLease(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	operation := terms(p, "0.25")
	operation.MaxInput = 126976
	operation.MaxSessionDuration = 30 * time.Minute
	operation.MaxSessionResponses = 500
	operation.SessionKinds = []contract.RealtimeSessionKind{"conversation"}
	p.Realtime = &operation
	voice := model("realtime")
	voice.RealtimeSessionKinds = operation.SessionKinds
	voice.InputModalities = []string{"text", "audio"}
	voice.OutputModalities = []string{"text", "audio"}
	voice.Price = nonTextPrice(voice.ID, UnitPrice{contract.UnitAudioInputMilliseconds, "0.08", 60000, "USD"}, UnitPrice{contract.UnitAudioOutputMilliseconds, "0.08", 60000, "USD"}, UnitPrice{contract.UnitSessionMilliseconds, "0.08", 60000, "USD"}, UnitPrice{contract.UnitRequests, "0.004", 1, "USD"})
	request := Request{Operation: contract.GrantRealtimeSession, Target: ptr(contract.ModelTarget("fixture/realtime")), Facts: contract.GrantRequestFacts{EstimatedInputTokens: ptr(1000), RealtimeSessionKind: ptr(contract.RealtimeSessionKind("conversation"))}, SessionMaxDuration: 30 * time.Minute, SessionMaxResponses: 500}
	plan, err := planner(t, p, voice).Plan(principal(), request)
	if err != nil {
		t.Fatal(err)
	}
	enableTestPolicy(t, s, pool, ctx, p)
	admissionRequest := AdmissionRequest{RequestID: "req-native-realtime", Principal: principal(), Operation: request.Operation, BodyDigest: digest([]byte("session-frame")), Plan: plan}
	admitted, err := s.Admit(ctx, admissionRequest)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.LeaseUntil.Sub(admitted.AdmittedAt) != request.SessionMaxDuration {
		t.Fatal("session lease used text timeout")
	}
	forged := admissionRequest
	forged.RequestID = "req-long-session"
	forged.Plan.SessionMaxDuration = 31 * time.Minute
	forged.Plan.ExecutionTimeout = 31 * time.Minute
	if _, err = s.Admit(ctx, forged); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("unbounded session lease accepted: %v", err)
	}
	report := reportFor(admitted)
	report.ResolvedModelReference = "fixture/realtime@v1"
	report.Units = []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 60000}, {Unit: contract.UnitRequests, Quantity: 1}}
	if err = s.Settle(ctx, admitted.ID, report, nil); err != nil {
		t.Fatal(err)
	}
	var amount string
	if err = pool.QueryRow(ctx, `SELECT generation->'tariff'->>'amount' FROM kaana_admission.settlements WHERE admission_id=$1`, admitted.ID).Scan(&amount); err != nil || amount != "0.084000000000" {
		t.Fatalf("session usage rating=%s err=%v", amount, err)
	}
}

func TestPostgresCompletionSurvivesRestartAndCannotBeRewritten(t *testing.T) {
	s, pool, ctx := testDatabase(t)
	p := policy()
	p.MaxConcurrent = 1
	enableTestPolicy(t, s, pool, ctx, p)
	admission, err := s.Admit(ctx, storeRequest(t, p, "req-durable-completion", nil))
	if err != nil {
		t.Fatal(err)
	}
	report := reportFor(admission)
	event := providercost.Event{RequestID: admission.RequestID, AttemptIndex: 0, Provider: "test", KeyID: "event-key-not-yet-imported", DeploymentID: "dep-test", ModelReference: report.ResolvedModelReference, Source: providercost.SourceUnknown, Served: true, OccurredAt: admission.AdmittedAt.Add(time.Second), Units: report.Units, Telemetry: providercost.AttemptTelemetry{StartedAt: admission.AdmittedAt, Latency: time.Second, Outcome: providercost.AttemptSucceeded}}
	if err = s.StageCompletion(ctx, admission.ID, report, []providercost.Event{event}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Admit(ctx, storeRequest(t, p, "req-after-provider-ended", nil)); err != nil {
		t.Fatalf("durable completion retained execution capacity: %v", err)
	}
	changed := report
	changed.Units = []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 9}}
	if err = s.StageCompletion(ctx, admission.ID, changed, []providercost.Event{event}); !errors.Is(err, ErrConflict) {
		t.Fatalf("staged evidence changed: %v", err)
	}
	if err = s.Settle(ctx, admission.ID, changed, []providercost.Event{event}); !errors.Is(err, ErrConflict) {
		t.Fatalf("settlement ignored staged evidence: %v", err)
	}
	ownConfig := pool.Config().Copy()
	pool.Close()
	recoveredPool, err := pgxpool.NewWithConfig(ctx, ownConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer recoveredPool.Close()
	recovered := NewStore(recoveredPool)
	if n, err := recovered.ReplayCompletions(ctx, 100); err == nil || n != 0 {
		t.Fatalf("missing provider key falsely settled: %d %v", n, err)
	}
	var pending int
	if err = recoveredPool.QueryRow(ctx, `SELECT count(*) FROM kaana_admission.completions`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending evidence lost: %d %v", pending, err)
	}
	if _, err = recoveredPool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position) VALUES('test','event-key-not-yet-imported','\x01','arn:aws:kms:us-west-2:123456789012:key/11111111-1111-1111-1111-111111111111',1)`); err != nil {
		t.Fatal(err)
	}
	if n, err := recovered.ReplayCompletions(ctx, 100); err != nil || n != 1 {
		t.Fatalf("restart replay: %d %v", n, err)
	}
	if n, err := recovered.ReplayCompletions(ctx, 100); err != nil || n != 0 {
		t.Fatalf("completion replay duplicated usage: %d %v", n, err)
	}
	if err = recoveredPool.QueryRow(ctx, `SELECT count(*) FROM provider_cost_events WHERE request_id=$1`, admission.RequestID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("provider cost replay count: %d %v", pending, err)
	}
	if err = recoveredPool.QueryRow(ctx, `SELECT count(*) FROM inference_settlement_outbox`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("native replay crossed economic authority: %d %v", pending, err)
	}
}

package commercialadmission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func pointer[T any](v T) *T { return &v }
func testID(prefix string) string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

type fixture struct {
	ctx           context.Context
	db, runtimeDB *pgxpool.Pool
	native        *nativeadmission.Store
	coordinator   *Coordinator
	admin, ledger *billing.Store
	account       billing.Account
	policy        nativeadmission.Policy
	model         nativeadmission.Model
	principal     contract.IdentityPrincipal
}

func database(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	root, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := root.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-test-role-provision'))`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kaana_billing_migrator", "kaana_billing_admin", "kaana_billing_runtime", "kaana_commercial_test_runtime"} {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			if _, err = tx.Exec(ctx, `CREATE ROLE `+pgx.Identifier{name}.Sanitize()+` NOLOGIN INHERIT`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = tx.Exec(ctx, `GRANT kaana_admission_runtime,kaana_runtime,kaana_billing_runtime TO kaana_commercial_test_runtime`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	name := testID("kaana_commercial_test_")
	if _, err = root.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := root.Exec(context.Background(), `DROP DATABASE `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
		root.Close()
		if err != nil {
			t.Error(err)
		}
	})
	local := cfg.Copy()
	local.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = credentialstore.MigrateDatabase(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = nativeadmission.NewStore(db).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `GRANT CREATE ON DATABASE `+pgx.Identifier{name}.Sanitize()+` TO kaana_billing_migrator; GRANT USAGE,CREATE ON SCHEMA public TO kaana_billing_migrator; GRANT USAGE ON SCHEMA kaana_admission TO kaana_billing_migrator; GRANT REFERENCES ON kaana_admission.admissions TO kaana_billing_migrator`); err != nil {
		t.Fatal(err)
	}
	rolePool := func(role string) *pgxpool.Pool {
		config := local.Copy()
		config.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, `SET ROLE `+pgx.Identifier{role}.Sanitize())
			return err
		}
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	migrator := rolePool("kaana_billing_migrator")
	if err = billing.NewStore(migrator).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = New(migrator).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	runtime := rolePool("kaana_commercial_test_runtime")
	coordinator := New(runtime)
	f := &fixture{ctx: ctx, db: db, runtimeDB: runtime, coordinator: coordinator, native: nativeadmission.NewStoreWithEconomics(runtime, coordinator), admin: billing.NewStore(rolePool("kaana_billing_admin")), ledger: billing.NewStore(runtime)}
	f.principal = contract.IdentityPrincipal{OwnerAccountID: pointer(contract.AccountID("opaque-owner")), ApplicationID: "app", CredentialID: "credential", Environment: contract.EnvironmentProduction, Lane: contract.IdentityLaneService, Authority: contract.IdentityAuthorityFirstParty, ApplicationClass: "internal", ApplicationIsInternal: true}
	f.policy = nativeadmission.Policy{EconomicTreatment: nativeadmission.Commercial, ID: "commercial-fixture", Version: 1, Enabled: true, CredentialIDs: []contract.CredentialID{f.principal.CredentialID}, OwnerAccountID: *f.principal.OwnerAccountID, ApplicationID: f.principal.ApplicationID, Environment: f.principal.Environment, Lane: f.principal.Lane, Authority: contract.AuthorityFirstParty, ApplicationClass: f.principal.ApplicationClass, ApplicationIsInternal: true, Audience: contract.AudiencePlatformInternal, AllowedLevels: []contract.PowerLevel{contract.PowerAuto, contract.PowerMedium}, AllowedModels: []contract.ModelID{"fixture/medium"}, Routes: nativeadmission.RouteScope{Mode: "exact", Deployments: []nativeadmission.RoutePermission{{DeploymentID: "fixture-deployment", ModelReference: "fixture/medium@v1", Provider: "fixture"}}}, Limits: contract.GrantStepLimits{MaxPricePerRequest: &contract.Money{Amount: "0.05", Currency: "USD"}, MaxFundingTier: pointer(contract.FundingTierCredit), RequireZeroDataRetention: true, ProhibitTrainingOnCustomerData: true}, MaxInputTokens: 126976, MaxOutputTokens: 8192, DefaultOutputTokens: 8192, MaxTools: 128, MaxConcurrent: 32, MaxPerUTCDay: 5000, ExecutionTimeout: time.Minute}
	f.model = nativeadmission.Model{ID: "fixture/medium", PowerClass: "medium", Audience: contract.AudiencePlatformInternal, Active: true, InputModalities: []string{"text"}, OutputModalities: []string{"text"}, Tools: true, StructuredOutput: true, JSONMode: true, Streaming: true, MaxContextTokens: 131072, MaxOutputTokens: 16384, LicenseID: "reviewed", CommercialUseAllowed: true, ZeroDataRetentionAvailable: true, Price: nativeadmission.ModelPrice{ModelLine: "fixture/medium", PriceVersionID: "fixture-price-v1", Currency: "USD", UnitPrices: []nativeadmission.UnitPrice{{Unit: contract.UnitCachedInputTokens, Amount: "0.1", Per: 1000000, Currency: "USD"}, {Unit: contract.UnitReasoningTokens, Amount: "2", Per: 1000000, Currency: "USD"}, {Unit: contract.UnitInputTokens, Amount: "1", Per: 1000000, Currency: "USD"}, {Unit: contract.UnitOutputTokens, Amount: "2", Per: 1000000, Currency: "USD"}, {Unit: contract.UnitRequests, Amount: "0", Per: 1, Currency: "USD"}}}}
	f.activate(t, f.policy)
	f.account, err = f.admin.CreateAccount(ctx, string(*f.principal.OwnerAccountID), "USD")
	if err != nil {
		t.Fatal(err)
	}
	budget, err := f.admin.ConfigureBudget(ctx, billing.BudgetRequest{AccountID: f.account.ID, Key: "test-budget", Amount: "1", StartsAt: time.Now().Add(-time.Hour), EndsAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.admin.ConfigureAccount(ctx, f.account.ID, budget, "0", true); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) activate(t *testing.T, p nativeadmission.Policy) {
	t.Helper()
	if err := nativeadmission.NewStore(f.db).RegisterPolicy(f.ctx, p); err != nil {
		t.Fatal(err)
	}
	_, err := f.db.Exec(f.ctx, `UPDATE kaana_admission.enrollments e SET enabled=true,legacy_fence_complete=true,activation_at=clock_timestamp(),active_policy_id=p.id FROM kaana_admission.policy_versions p WHERE p.enrollment_id=e.id AND p.policy_key=$1 AND p.version=$2`, p.ID, p.Version)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) fund(t *testing.T) {
	t.Helper()
	_, err := f.admin.Fund(f.ctx, billing.FundingRequest{AccountID: f.account.ID, Key: "test-verified-payment", Kind: "purchased", Amount: "1", EvidenceID: "fixture-only-verified-payment"})
	if err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) request(t *testing.T, key string) nativeadmission.AdmissionRequest {
	t.Helper()
	planner, err := nativeadmission.NewPlanner([]nativeadmission.Policy{f.policy}, []nativeadmission.Model{f.model})
	if err != nil {
		t.Fatal(err)
	}
	target := contract.LevelTarget(contract.PowerAuto)
	plan, err := planner.Plan(f.principal, nativeadmission.Request{Operation: contract.GrantResponses, Target: &target, StructuredOutput: true, Facts: contract.GrantRequestFacts{EstimatedInputTokens: pointer(500), MaxOutputTokens: pointer(8192), Stream: true}})
	if err != nil {
		t.Fatal(err)
	}
	return nativeadmission.AdmissionRequest{RequestID: contract.RequestID(testID("req-")), Principal: f.principal, Operation: contract.GrantResponses, BodyDigest: strings.Repeat("a", 64), IdempotencyKey: pointer(contract.IdempotencyKey(key)), Plan: plan}
}
func report(a *nativeadmission.Admission) contract.UsageReport {
	return contract.UsageReport{SchemaVersion: contract.UsageReportSchemaVersion, RequestID: a.RequestID, Attribution: a.Attribution(), Outcome: contract.OutcomeCompleted, UsageSource: contract.UsageProviderReported, Units: []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 3}}, ResolvedModelReference: "fixture/medium@v1", ServingProvider: "fixture", DeploymentID: "fixture-deployment", StartedAt: contract.NewTimestamp(a.AdmittedAt), CompletedAt: contract.NewTimestamp(a.AdmittedAt.Add(time.Second))}
}
func (f *fixture) count(t *testing.T, table string, want int) {
	t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != want {
		t.Fatalf("%s count %d want%d: %v", table, n, want, err)
	}
}

func TestPostgresCommercialReserveIsAtomicAndOwnershipBound(t *testing.T) {
	f := database(t)
	req := f.request(t, "same-key")
	if _, err := f.native.Admit(f.ctx, req); !errors.Is(err, billing.ErrFunds) {
		t.Fatalf("unfunded admitted %v", err)
	}
	f.count(t, "kaana_admission.admissions", 0)
	f.count(t, "kaana_admission.daily_windows", 0)
	f.count(t, "kaana_billing.holds", 0)
	f.fund(t)
	for _, treatment := range []nativeadmission.EconomicTreatment{nativeadmission.InternalMetered, ""} {
		altered := req
		altered.Plan.EconomicTreatment = treatment
		if _, err := f.native.Admit(f.ctx, altered); !errors.Is(err, nativeadmission.ErrNotEnrolled) {
			t.Fatalf("caller changed immutable economic treatment %q: %v", treatment, err)
		}
	}
	f.count(t, "kaana_admission.admissions", 0)
	f.count(t, "kaana_billing.holds", 0)
	foreign := req
	foreign.Principal.OwnerAccountID = pointer(contract.AccountID("delegated-owner"))
	if _, err := f.native.Admit(f.ctx, foreign); !errors.Is(err, nativeadmission.ErrNotEnrolled) {
		t.Fatalf("foreign owner %v", err)
	}
	req.UserID = pointer(contract.UserID("delegated-person-not-payer"))
	a, err := f.native.Admit(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	f.count(t, "kaana_admission.admissions", 1)
	f.count(t, "kaana_billing.holds", 1)
	f.count(t, "kaana_commercial.admissions", 1)
	b, err := f.ledger.Balance(f.ctx, f.account.ID)
	if err != nil || b.Reserved != string(a.Plan.Quote.Amount) {
		t.Fatalf("execution before exact hold %#v quote%s %v", b, a.Plan.Quote.Amount, err)
	}
	if _, err = f.native.Admit(f.ctx, req); !errors.Is(err, nativeadmission.ErrReplay) {
		t.Fatalf("retry re-executed %v", err)
	}
	req.BodyDigest = strings.Repeat("b", 64)
	if _, err = f.native.Admit(f.ctx, req); !errors.Is(err, nativeadmission.ErrConflict) {
		t.Fatalf("body conflict %v", err)
	}
	// The actual serving role cannot seed funds or activate billing itself.
	if _, err = f.ledger.Fund(f.ctx, billing.FundingRequest{AccountID: f.account.ID, Key: "forbidden", Kind: "purchased", Amount: "1", EvidenceID: "not-verified"}); err == nil {
		t.Fatal("combined runtime minted credit")
	}
}

func TestPostgresCommercialRecoveryCannotBypassHooksOrDoubleCharge(t *testing.T) {
	f := database(t)
	f.fund(t)
	req := f.request(t, "recover")
	noHooks := nativeadmission.NewStore(f.runtimeDB)
	if _, err := noHooks.Admit(f.ctx, req); !errors.Is(err, nativeadmission.ErrDisabled) {
		t.Fatalf("commercial admitted without hook %v", err)
	}
	a, err := f.native.Admit(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	r := report(a)
	if err = f.native.StageCompletion(f.ctx, a.ID, r, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = noHooks.ReplayCompletions(f.ctx, 10); !errors.Is(err, nativeadmission.ErrDisabled) {
		t.Fatalf("restart without hooks %v", err)
	}
	f.count(t, "kaana_billing.receipts", 0)
	// A crash after staged evidence is recovered without invoking a provider.
	recovery, err := f.coordinator.Recover(f.ctx, f.native, 10)
	if err != nil || recovery.Completed != 1 {
		t.Fatalf("recovery %#v %v", recovery, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() { errs <- f.native.Settle(f.ctx, a.ID, r, nil) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.count(t, "kaana_billing.receipts", 1)
	f.count(t, "kaana_admission.settlements", 1)
	f.count(t, "kaana_commercial.outcomes", 1)
	body, err := f.native.Generation(f.ctx, a.Principal, nil, req.IdempotencyKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	var receipt contract.ChargedGenerationRecord
	if err = json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	if err = receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	if receipt.Kind != "charge" || receipt.BilledAmount != "0.000006000000" || receipt.PriceSnapshot.PriceVersionID != "fixture-price-v1" {
		t.Fatalf("incorrect commercial receipt %#v", receipt)
	}
	if strings.Contains(string(body), "not_charged") {
		t.Fatal("debit mislabeled as metering")
	}
	foreign := a.Principal
	foreign.OwnerAccountID = pointer(contract.AccountID("foreign"))
	if _, err = f.native.Generation(f.ctx, foreign, nil, req.IdempotencyKey, nil); !errors.Is(err, nativeadmission.ErrNotFound) {
		t.Fatalf("foreign receipt %v", err)
	}
	changed := r
	changed.Units = []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 4}}
	if err = f.native.Settle(f.ctx, a.ID, changed, nil); !errors.Is(err, nativeadmission.ErrConflict) {
		t.Fatalf("changed settlement %v", err)
	}
}

func TestPostgresCommercialLedgerAndNativeReceiptRollbackTogether(t *testing.T) {
	f := database(t)
	f.fund(t)
	a, err := f.native.Admit(f.ctx, f.request(t, "atomic"))
	if err != nil {
		t.Fatal(err)
	}
	r := report(a)
	if err = f.native.StageCompletion(f.ctx, a.ID, r, nil); err != nil {
		t.Fatal(err)
	}
	_, err = f.db.Exec(f.ctx, `CREATE FUNCTION kaana_admission.inject_settlement_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture after charge'; END $$; CREATE TRIGGER fixture_failure BEFORE INSERT ON kaana_admission.settlements FOR EACH ROW EXECUTE FUNCTION kaana_admission.inject_settlement_failure()`)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.native.Settle(f.ctx, a.ID, r, nil); err == nil {
		t.Fatal("injected failure absent")
	}
	f.count(t, "kaana_billing.receipts", 0)
	f.count(t, "kaana_commercial.outcomes", 0)
	f.count(t, "kaana_admission.settlements", 0)
	b, err := f.ledger.Balance(f.ctx, f.account.ID)
	if err != nil || b.Reserved != string(a.Plan.Quote.Amount) {
		t.Fatalf("hold released before native commit %#v %v", b, err)
	}
	if _, err = f.db.Exec(f.ctx, `DROP TRIGGER fixture_failure ON kaana_admission.settlements`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.ReplayCompletions(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	f.count(t, "kaana_billing.receipts", 1)
}

func TestPostgresCommercialOutcomeAndRatingFailure(t *testing.T) {
	f := database(t)
	f.fund(t)
	for _, kind := range []string{"partial", "failed", "failed-empty", "cancelled", "zero", "unpriced", "overquote"} {
		req := f.request(t, kind)
		a, err := f.native.Admit(f.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		r := report(a)
		switch kind {
		case "partial":
			r.Outcome = contract.OutcomePartial
		case "failed":
			r.Outcome = contract.OutcomeFailed
		case "failed-empty":
			r.Outcome = contract.OutcomeFailed
			r.Units = nil
		case "cancelled":
			r.Outcome = contract.OutcomeCancelled
		case "zero":
			r.Units = []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 0}}
		case "unpriced":
			r.Units = []contract.UsageQuantity{{Unit: contract.UnitImages, Quantity: 1}}
		case "overquote":
			r.Units = []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 100000}}
		}
		if err = f.native.StageCompletion(f.ctx, a.ID, r, nil); err != nil {
			t.Fatal(err)
		}
		if err = f.native.Settle(f.ctx, a.ID, r, nil); err != nil {
			t.Fatal(err)
		}
		body, err := f.native.Generation(f.ctx, a.Principal, nil, req.IdempotencyKey, nil)
		if kind == "unpriced" || kind == "overquote" {
			if !errors.Is(err, nativeadmission.ErrNotFound) {
				t.Fatalf("rating failure became free receipt %s %s %v", kind, body, err)
			}
			var status string
			if err = f.db.QueryRow(f.ctx, `SELECT status FROM kaana_commercial.outcomes WHERE admission_id=$1`, a.ID).Scan(&status); err != nil || status != "rating_failed" {
				t.Fatalf("failure status %s %v", status, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var receipt contract.ChargedGenerationRecord
		if err = json.Unmarshal(body, &receipt); err != nil {
			t.Fatal(err)
		}
		want := "0.000000000000"
		if kind == "partial" {
			want = "0.000006000000"
		}
		if string(receipt.BilledAmount) != want {
			t.Fatalf("%s charge %s want%s", kind, receipt.BilledAmount, want)
		}
	}
	f.count(t, "kaana_billing.receipts", 5)
}

func (f *fixture) ageHolds(t *testing.T) {
	t.Helper()
	// Advance only this disposable fixture's financial clock. Production roles
	// cannot rewrite hold identity; restore its immutable trigger before recovery.
	_, err := f.db.Exec(f.ctx, `BEGIN; ALTER TABLE kaana_billing.holds DISABLE TRIGGER immutable_hold; UPDATE kaana_billing.holds SET created_at=created_at-interval '2 hours',expires_at=expires_at-interval '2 hours'; ALTER TABLE kaana_billing.holds ENABLE TRIGGER immutable_hold; UPDATE kaana_admission.admissions SET lease_until=clock_timestamp()-interval '1 second'; COMMIT`)
	if err != nil {
		t.Fatal(err)
	}
}
func TestPostgresCommercialUnknownExpiryAndLateEvidence(t *testing.T) {
	f := database(t)
	f.fund(t)
	req := f.request(t, "unknown")
	a, err := f.native.Admit(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	f.ageHolds(t)
	recovery, err := f.coordinator.Recover(f.ctx, f.native, 10)
	if err != nil || recovery.Abandoned != 1 || recovery.Released != 1 || recovery.Completed != 0 {
		t.Fatalf("unknown recovery %#v %v", recovery, err)
	}
	f.count(t, "kaana_billing.receipts", 0)
	f.count(t, "kaana_admission.completions", 0)
	r := report(a)
	if err = f.native.StageCompletion(f.ctx, a.ID, r, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.ReplayCompletions(f.ctx, 10); err != nil {
		t.Fatal(err)
	}
	f.count(t, "kaana_billing.receipts", 0)
	var status string
	if err = f.db.QueryRow(f.ctx, `SELECT status FROM kaana_commercial.outcomes WHERE admission_id=$1`, a.ID).Scan(&status); err != nil || status != "hold_expired" {
		t.Fatalf("late debit %s %v", status, err)
	}
	if _, err = f.native.Generation(f.ctx, a.Principal, nil, req.IdempotencyKey, nil); !errors.Is(err, nativeadmission.ErrNotFound) {
		t.Fatalf("expired hold receipt %v", err)
	}
	b, err := f.ledger.Balance(f.ctx, f.account.ID)
	if err != nil || b.Purchased != "1.000000000000" || b.Reserved != "0.000000000000" {
		t.Fatalf("expiry balance %#v %v", b, err)
	}
}

func TestPostgresCommercialLifetimeAndLegacyFenceBeforeExecution(t *testing.T) {
	f := database(t)
	f.fund(t)
	f.policy.ExecutionTimeout = 15 * time.Minute
	f.policy.Version = 2
	f.activate(t, f.policy)
	if _, err := f.native.Admit(f.ctx, f.request(t, "too-long")); !errors.Is(err, ErrHoldLifetime) {
		t.Fatalf("insufficient lifetime admitted %v", err)
	}
	f.count(t, "kaana_admission.admissions", 0)
	f.count(t, "kaana_billing.holds", 0)
	f.policy.ExecutionTimeout = time.Minute
	f.policy.Version = 3
	f.activate(t, f.policy)
	_, err := f.db.Exec(f.ctx, `INSERT INTO kaana_admission.legacy_fences(id,enrollment_id,idempotency_scope,idempotency_key,legacy_admission_id) SELECT 'fixture-fence',id,'credential:credential','legacy','old-authority' FROM kaana_admission.enrollments`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.Admit(f.ctx, f.request(t, "legacy")); !errors.Is(err, nativeadmission.ErrLegacy) {
		t.Fatalf("legacy key reached new ledger %v", err)
	}
	f.count(t, "kaana_billing.holds", 0)
	// A fresh key is the positive control; the fence is not a blanket refusal.
	if _, err = f.native.Admit(f.ctx, f.request(t, "fresh")); err != nil {
		t.Fatal(err)
	}
}

// Mutating a hook's returned record after its financial writes must roll back
// those writes as well. The native store remains the receipt contract boundary.
type corruptReceipt struct {
	*Coordinator
	mutate func(*contract.ChargedGenerationRecord)
}

func (c corruptReceipt) Settle(ctx context.Context, tx pgx.Tx, rated nativeadmission.RatedCompletion) (nativeadmission.EconomicSettlement, error) {
	result, err := c.Coordinator.Settle(ctx, tx, rated)
	if err == nil && result.Charge != nil {
		c.mutate(result.Charge)
	}
	return result, err
}
func TestPostgresCommercialRejectsInvalidReceiptAndRollsBackDebit(t *testing.T) {
	f := database(t)
	f.fund(t)
	cases := []struct {
		name   string
		mutate func(*contract.ChargedGenerationRecord)
	}{
		{"schema", func(r *contract.ChargedGenerationRecord) { r.Currency = "not-a-currency" }},
		{"owner", func(r *contract.ChargedGenerationRecord) { r.ApplicationID = "other-app" }},
		{"amount", func(r *contract.ChargedGenerationRecord) { r.BilledAmount = "0" }},
		{"price", func(r *contract.ChargedGenerationRecord) { r.PriceSnapshot.PriceVersionID = "other-price" }},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := f.native.Admit(f.ctx, f.request(t, tc.name))
			if err != nil {
				t.Fatal(err)
			}
			r := report(a)
			broken := nativeadmission.NewStoreWithEconomics(f.runtimeDB, corruptReceipt{f.coordinator, tc.mutate})
			if err = broken.Settle(f.ctx, a.ID, r, nil); err == nil {
				t.Fatal("invalid commercial receipt accepted")
			}
			f.count(t, "kaana_billing.receipts", index)
			f.count(t, "kaana_commercial.outcomes", index)
			f.count(t, "kaana_admission.settlements", index)
			if err = f.native.Settle(f.ctx, a.ID, r, nil); err != nil {
				t.Fatal(err)
			}
			f.count(t, "kaana_billing.receipts", index+1)
		})
	}
}

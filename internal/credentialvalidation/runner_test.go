package credentialvalidation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/oxyvalidation"
	"github.com/OxyHQ/Kaana/internal/provider"
)

type memoryRepository struct {
	mu       sync.Mutex
	rows     map[string]credentialstore.CustomerCredentialValidationOutcome
	attempts map[string]int
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{
		rows:     make(map[string]credentialstore.CustomerCredentialValidationOutcome),
		attempts: make(map[string]int),
	}
}

func (r *memoryRepository) ClaimCustomerValidation(_ context.Context, operation credentialstore.CustomerCredentialValidationOperation) (credentialstore.CustomerCredentialValidationOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, found := r.rows[operation.OperationID]
	if found {
		if stored.Operation != operation {
			return credentialstore.CustomerCredentialValidationOutcome{State: credentialstore.CustomerCredentialValidationConflict}, nil
		}
		if stored.State == credentialstore.CustomerCredentialValidationExecute {
			return credentialstore.CustomerCredentialValidationOutcome{Operation: operation, State: credentialstore.CustomerCredentialValidationPending}, nil
		}
		return stored, nil
	}
	r.attempts[operation.OperationID]++
	executing := credentialstore.CustomerCredentialValidationOutcome{
		Operation: operation, State: credentialstore.CustomerCredentialValidationExecute,
		LeaseGeneration: int64(r.attempts[operation.OperationID]),
	}
	r.rows[operation.OperationID] = executing
	return executing, nil
}

func (r *memoryRepository) CompleteCustomerValidation(_ context.Context, outcome credentialstore.CustomerCredentialValidationOutcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, found := r.rows[outcome.Operation.OperationID]
	if !found || stored.Operation != outcome.Operation || stored.State != credentialstore.CustomerCredentialValidationExecute ||
		stored.LeaseGeneration != outcome.LeaseGeneration {
		return errors.New("operation was not executing")
	}
	outcome.LeaseGeneration = 0
	r.rows[outcome.Operation.OperationID] = outcome
	return nil
}

type exactResolver struct {
	want                credentialstore.CustomerCredentialScope
	calls               int
	waitForCancellation bool
}

func (r *exactResolver) ResolveForInference(ctx context.Context, scope credentialstore.CustomerCredentialScope) ([]byte, error) {
	r.calls++
	if scope != r.want {
		return nil, fmt.Errorf("unexpected exact scope: %+v", scope)
	}
	if r.waitForCancellation {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return nil, errors.New("credential resolver did not receive a bounded context")
		}
	}
	return []byte("customer-secret"), nil
}

type probeAdapter struct {
	err      error
	calls    int
	requests int
}

func (*probeAdapter) Provider() contract.ProviderSlug { return "stub" }

func (a *probeAdapter) Translate(request *contract.Request, route provider.Route) (*provider.Call, error) {
	a.requests++
	if request.Stream || request.MaxOutputTokens == nil || *request.MaxOutputTokens != 1 ||
		len(request.Input.Messages) != 1 || len(request.Input.Messages[0].Content) != 1 ||
		request.Input.Messages[0].Content[0].Text == nil || *request.Input.Messages[0].Content[0].Text != "." {
		return nil, errors.New("probe was not the fixed one-token request")
	}
	if route.DeploymentID != "dep_exact" || route.Provider != "stub" {
		return nil, errors.New("probe did not use the exact deployment")
	}
	return &provider.Call{Route: route}, nil
}

func (a *probeAdapter) Stream(_ context.Context, call *provider.Call, out provider.Emitter, keys *provider.KeyPool) (provider.Outcome, error) {
	a.calls++
	key, ok := keys.Begin().Next(time.Now())
	if !ok || !key.CustomerOwned() || key.Secret() != "customer-secret" {
		return provider.Outcome{}, errors.New("probe did not receive the exact customer key")
	}
	if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
		return provider.Outcome{}, err
	}
	if err := out.Delta(0, contract.ChannelOutputText, "must-be-discarded"); err != nil {
		return provider.Outcome{}, err
	}
	return provider.Outcome{}, a.err
}

func (a *probeAdapter) Health(context.Context) provider.Health { return provider.Health{} }

type recordingReporter struct{ verdicts []oxyvalidation.Verdict }

func (r *recordingReporter) Submit(verdict oxyvalidation.Verdict) {
	r.verdicts = append(r.verdicts, verdict)
}

func validationTask(operationID string) contract.KaanaCredentialValidationTask {
	return contract.KaanaCredentialValidationTask{
		SchemaVersion: 1, OperationID: contract.KaanaCredentialOperationID(operationID),
		ApplicationID: "app_exact", Provider: "stub", OwnerAccountID: "acc_exact",
		ConnectionID: "conn_exact", Environment: contract.EnvironmentProduction,
		CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz", CredentialRevision: 7,
		DeploymentID: "dep_exact",
	}
}

func validationRunner(t *testing.T, repository *memoryRepository, adapter *probeAdapter, reporter *recordingReporter) (*Runner, *exactResolver) {
	t.Helper()
	issued := contract.NewTimestamp(time.Now())
	document := fmt.Sprintf(`{"snapshotId":"snap_validation","issuedAt":%q,"deployments":[{"deploymentId":"dep_exact","provider":"stub","modelReference":"stub/model@2026-09-01","upstreamModelId":"model","regions":[],"current":true}]}`, issued)
	location := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(location, []byte(document), 0o600); err != nil {
		t.Fatalf("writing inventory: %v", err)
	}
	store, err := inventory.NewStore(inventory.Config{Path: location, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatalf("provider registry: %v", err)
	}
	task := validationTask("op_exact")
	resolver := &exactResolver{want: operationFromTask(task).CustomerCredentialScope}
	runner, err := New(Config{
		Repository: repository, Resolver: resolver, Inventory: store,
		Providers: registry, Reporter: reporter, ProbeTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	return runner, resolver
}

func TestProbeClassifiesOnlyAuthenticationAsInvalid(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		state       string
		failureCode string
	}{
		{name: "success", state: "valid"},
		{name: "authentication", err: provider.ErrCustomerCredential{Code: contract.CodeProviderCredentialInvalid}, state: "invalid", failureCode: "unauthorized"},
		{name: "billing", err: provider.ErrCustomerCredential{Code: contract.CodeProviderBillingRefused}, state: "inconclusive", failureCode: "forbidden"},
		{name: "quota", err: provider.ErrUpstream{Code: contract.CodeRateLimited}, state: "inconclusive", failureCode: "rate_limited"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			repository := newMemoryRepository()
			adapter := &probeAdapter{err: testCase.err}
			reporter := &recordingReporter{}
			runner, resolver := validationRunner(t, repository, adapter, reporter)
			outcome, err := runner.Validate(context.Background(), validationTask("op_exact"))
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if string(outcome.State) != testCase.state || validationFailure(outcome) != testCase.failureCode {
				t.Fatalf("outcome = %+v", outcome)
			}
			if adapter.calls != 1 || adapter.requests != 1 || resolver.calls != 1 {
				t.Fatalf("probe calls: stream=%d translate=%d resolver=%d", adapter.calls, adapter.requests, resolver.calls)
			}
			if len(reporter.verdicts) != 1 || reporter.verdicts[0].OperationID != "op_exact" || reporter.verdicts[0].DeploymentID != "dep_exact" {
				t.Fatalf("exact outcome was not reported: %#v", reporter.verdicts)
			}
		})
	}
}

func TestTerminalReplayDoesNotRepeatProviderSpendAndReemitsCallback(t *testing.T) {
	repository := newMemoryRepository()
	adapter := &probeAdapter{}
	reporter := &recordingReporter{}
	runner, _ := validationRunner(t, repository, adapter, reporter)
	task := validationTask("op_replay")

	first, err := runner.Validate(context.Background(), task)
	if err != nil || first.State != "valid" {
		t.Fatalf("first validation = %+v, %v", first, err)
	}
	second, err := runner.Validate(context.Background(), task)
	if err != nil || !reflect.DeepEqual(second, first) {
		t.Fatalf("replay = %+v, %v; want %+v", second, err, first)
	}
	if adapter.calls != 1 || len(reporter.verdicts) != 2 {
		t.Fatalf("replay spent provider calls=%d or callbacks=%d", adapter.calls, len(reporter.verdicts))
	}
}

func TestBillingRecoveryUsesANewOperationWithoutRotatingTheGeneration(t *testing.T) {
	repository := newMemoryRepository()
	adapter := &probeAdapter{err: provider.ErrCustomerCredential{Code: contract.CodeProviderBillingRefused}}
	reporter := &recordingReporter{}
	runner, resolver := validationRunner(t, repository, adapter, reporter)

	first, err := runner.Validate(context.Background(), validationTask("op_before_topup"))
	if err != nil || first.State != "inconclusive" || validationFailure(first) != "forbidden" {
		t.Fatalf("billing outcome = %+v, %v", first, err)
	}
	adapter.err = nil
	second, err := runner.Validate(context.Background(), validationTask("op_after_topup"))
	if err != nil || second.State != "valid" {
		t.Fatalf("post-top-up outcome = %+v, %v", second, err)
	}
	if adapter.calls != 2 || resolver.calls != 2 || second.CredentialHandle != first.CredentialHandle || second.CredentialRevision != first.CredentialRevision {
		t.Fatalf("same generation was not explicitly revalidated: first=%+v second=%+v calls=%d/%d", first, second, adapter.calls, resolver.calls)
	}
}

func TestOperationIDCannotBeReboundToDifferentSelectors(t *testing.T) {
	repository := newMemoryRepository()
	adapter := &probeAdapter{}
	reporter := &recordingReporter{}
	runner, _ := validationRunner(t, repository, adapter, reporter)
	task := validationTask("op_bound")
	if _, err := runner.Validate(context.Background(), task); err != nil {
		t.Fatalf("first validation: %v", err)
	}
	task.DeploymentID = "dep_other"
	if _, err := runner.Validate(context.Background(), task); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("selector rebinding error = %v", err)
	}
	if adapter.calls != 1 {
		t.Fatalf("selector conflict reached provider %d times", adapter.calls)
	}
}

func TestProbeTimeoutCoversCredentialResolution(t *testing.T) {
	repository := newMemoryRepository()
	adapter := &probeAdapter{}
	reporter := &recordingReporter{}
	runner, resolver := validationRunner(t, repository, adapter, reporter)
	runner.probeTimeout = 20 * time.Millisecond
	resolver.waitForCancellation = true

	started := time.Now()
	outcome, err := runner.Validate(context.Background(), validationTask("op_timeout"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("credential resolution ignored the full-operation timeout: %v", elapsed)
	}
	if outcome.State != "inconclusive" || validationFailure(outcome) != "network" {
		t.Fatalf("timeout outcome = %+v", outcome)
	}
	if resolver.calls != 1 || adapter.requests != 0 || adapter.calls != 0 {
		t.Fatalf("timed-out resolution reached later stages: resolver=%d translate=%d stream=%d", resolver.calls, adapter.requests, adapter.calls)
	}
}

func TestProbeTimeoutMustFitInsideTheDatabaseLease(t *testing.T) {
	repository := newMemoryRepository()
	adapter := &probeAdapter{}
	reporter := &recordingReporter{}
	runner, resolver := validationRunner(t, repository, adapter, reporter)
	_, err := New(Config{
		Repository: repository, Resolver: resolver, Inventory: runner.inventory,
		Providers: runner.providers, Reporter: reporter, ProbeTimeout: maxProbeTimeout + time.Nanosecond,
	})
	if err == nil {
		t.Fatal("a probe timeout that can outlive the PostgreSQL lease was accepted")
	}
}

func TestExplicitNegativeProbeTimeoutIsRejected(t *testing.T) {
	repository := newMemoryRepository()
	reporter := &recordingReporter{}
	runner, resolver := validationRunner(t, repository, &probeAdapter{}, reporter)
	if _, err := New(Config{
		Repository: repository, Resolver: resolver, Inventory: runner.inventory,
		Providers: runner.providers, Reporter: reporter, ProbeTimeout: -time.Second,
	}); err == nil {
		t.Fatal("an explicitly negative probe timeout was silently replaced by the default")
	}
}

func TestProbeHonorsAnAlreadyCanceledParentBeforeResolution(t *testing.T) {
	repository := newMemoryRepository()
	adapter := &probeAdapter{}
	reporter := &recordingReporter{}
	runner, resolver := validationRunner(t, repository, adapter, reporter)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	outcome, err := runner.Validate(ctx, validationTask("op_parent_canceled"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if outcome.State != "inconclusive" || validationFailure(outcome) != "network" {
		t.Fatalf("canceled outcome = %+v", outcome)
	}
	if resolver.calls != 0 || adapter.requests != 0 || adapter.calls != 0 {
		t.Fatalf("canceled probe reached work: resolver=%d translate=%d stream=%d", resolver.calls, adapter.requests, adapter.calls)
	}
}

func validationFailure(outcome contract.KaanaCredentialValidationOutcome) string {
	if outcome.FailureCode == nil {
		return ""
	}
	return string(*outcome.FailureCode)
}

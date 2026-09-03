package kaana_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/customerlimit"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/oxyvalidation"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

// oneDeployment is the single-route inventory most of these tests run against.
// Failover needs two, and builds its own; see failover_test.go.
const oneDeployment = `{
  "deploymentId":"dep_test","provider":"stub",
  "modelReference":"stub/model@2026-05-01","upstreamModelId":"model",
  "regions":["test-region"],"current":true}`

/* -------------------------------------------------------------------------- */
/*  Adapters the tests script                                                 */
/* -------------------------------------------------------------------------- */

type scriptedAdapter struct {
	slug   contract.ProviderSlug
	stream func(ctx context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error)
	// translate, when set, replaces the pass-through translation.
	translate func(request *contract.Request, route provider.Route) (*provider.Call, error)
	// credentials observes the request-scoped override before the scripted
	// stream runs. nil is the platform-pool path.
	credentials func(*provider.KeyPool)

	mutex sync.Mutex
	calls int
}

func (s *scriptedAdapter) Provider() contract.ProviderSlug {
	if s.slug == "" {
		return "stub"
	}
	return s.slug
}

func (s *scriptedAdapter) Translate(request *contract.Request, route provider.Route) (*provider.Call, error) {
	if s.translate != nil {
		return s.translate(request, route)
	}
	return &provider.Call{Route: route, Stream: request.Stream}, nil
}

func (s *scriptedAdapter) Stream(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
	s.mutex.Lock()
	s.calls++
	s.mutex.Unlock()
	if s.credentials != nil {
		s.credentials(credentials)
	}
	return s.stream(ctx, call, out)
}

type scriptedCustomerResolver struct {
	want      credentialstore.CustomerCredentialScope
	plaintext []byte
	err       error
	calls     int
}

type recordingValidationReporter struct {
	mutex    sync.Mutex
	verdicts []oxyvalidation.Verdict
}

func (r *recordingValidationReporter) Submit(verdict oxyvalidation.Verdict) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.verdicts = append(r.verdicts, verdict)
}

func (r *recordingValidationReporter) all() []oxyvalidation.Verdict {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return append([]oxyvalidation.Verdict(nil), r.verdicts...)
}

func (r *scriptedCustomerResolver) ResolveForInference(_ context.Context, scope credentialstore.CustomerCredentialScope) ([]byte, error) {
	r.calls++
	if scope != r.want {
		return nil, fmt.Errorf("unexpected customer credential scope: %+v", scope)
	}
	if r.err != nil {
		return nil, r.err
	}
	return append([]byte(nil), r.plaintext...), nil
}

func (s *scriptedAdapter) Health(context.Context) provider.Health {
	return provider.Health{Provider: s.Provider(), Status: provider.HealthOK, CheckedAt: contract.NewTimestamp(time.Now())}
}

// attempts is how many times this adapter's Stream was entered. Failover tests
// turn on it: "the request was retried elsewhere" and "the request was retried
// on the same deployment twice" produce the same stream and different counts.
func (s *scriptedAdapter) attempts() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.calls
}

/* -------------------------------------------------------------------------- */
/*  Harness                                                                   */
/* -------------------------------------------------------------------------- */

// harness builds an executor over a written inventory snapshot, the way the
// binary does. The snapshot is a real file because the store's whole purpose is
// re-reading one, and a test that bypassed it would exercise a wiring nothing
// runs.
type harness struct {
	deployments         string
	adapters            []provider.Adapter
	rotation            *rotation.Registry
	costs               *providercost.Cards
	customerCredentials kaana.CustomerCredentialResolver
	validationReporter  oxyvalidation.Submitter
	customerLimits      *customerlimit.Registry
	issuedAt            time.Time
	now                 func() time.Time
	// routes are the exact destinations Oxy authorized for tests that exercise
	// failover. nil exercises the additive contract's no-list default.
	routes []contract.AuthorizedRoute
}

func (h harness) build(t *testing.T) *kaana.Executor {
	t.Helper()

	issuedAt := h.issuedAt
	if issuedAt.IsZero() {
		issuedAt = time.Now()
	}
	document := fmt.Sprintf(`{"snapshotId":"snap_kaana_test","issuedAt":%q,"deployments":[%s]}`,
		contract.NewTimestamp(issuedAt), h.deployments)

	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("writing the inventory: %v", err)
	}
	store, err := inventory.NewStore(inventory.Config{
		Path:   path,
		Now:    h.now,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("building the inventory store: %v", err)
	}

	registry, err := provider.NewRegistry(h.adapters...)
	if err != nil {
		t.Fatalf("registering: %v", err)
	}
	rotationRegistry := h.rotation
	if rotationRegistry == nil {
		rotationRegistry = rotation.NewRegistry(rotation.Policy{}, h.now)
	}
	executor, err := kaana.NewExecutor(kaana.Config{
		Inventory:           store,
		Providers:           registry,
		Rotation:            rotationRegistry,
		Costs:               h.costs,
		CustomerCredentials: h.customerCredentials,
		ValidationReporter:  h.validationReporter,
		CustomerLimits:      h.customerLimits,
		Now:                 h.now,
	})
	if err != nil {
		t.Fatalf("building the executor: %v", err)
	}
	return executor
}

func (h harness) run(t *testing.T, request *contract.Request) ([]contract.StreamEvent, kaana.Result) {
	t.Helper()
	if h.routes != nil {
		copy := *request
		copy.AuthorizedRoutes = append([]contract.AuthorizedRoute(nil), h.routes...)
		request = &copy
	}
	var events []contract.StreamEvent
	result := h.build(t).Execute(context.Background(), request, func(event contract.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	return events, result
}

func execute(t *testing.T, adapter provider.Adapter, request *contract.Request) ([]contract.StreamEvent, kaana.Result) {
	t.Helper()
	return harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}}.run(t, request)
}

func baseRequest() *contract.Request {
	reference := contract.ModelReference("stub/model@2026-05-01")
	text := "hi"
	return &contract.Request{
		SchemaVersion: contract.RequestEnvelopeVersion,
		Attribution: contract.Attribution{
			Principal: contract.AuthenticatedPrincipal{
				Billing:         contract.BillingPrincipal{AccountID: "acc_test"},
				ApplicationID:   "app_test",
				CredentialID:    "cred_test",
				Environment:     contract.EnvironmentDevelopment,
				InferenceScopes: []contract.Scope{contract.ScopeInvoke},
			},
			RequestID: "req_test",
		},
		Target:   contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference},
		Modality: contract.ModalityText,
		Input: contract.Input{
			Format: contract.InputMessages,
			Messages: []contract.Message{{
				Role:    contract.RoleUser,
				Content: []contract.ContentPart{{Type: contract.ContentPartText, Text: &text}},
			}},
		},
		Stream: true,
		Client: contract.ClientRequestMetadata{
			APIFormat:  contract.APIFormatResponses,
			Endpoint:   "/v1/responses",
			ReceivedAt: contract.NewTimestamp(time.Now()),
		},
		RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: "rp", PolicyVersion: 1},
	}
}

func happyAdapter() *scriptedAdapter {
	return &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "hello"); err != nil {
			return provider.Outcome{}, err
		}
		units := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}
		if err := out.Usage(units, contract.UsageProviderReported); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{Units: units, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}, nil
	}}
}

/* -------------------------------------------------------------------------- */
/*  Refusals                                                                  */
/* -------------------------------------------------------------------------- */

// TestARoutingProfileWithoutAuthorizedRoutesIsRefusedWithTheFieldNamed pins the
// default-deny half of the contract. A profile names no model, so only Oxy's
// signed list can give Kaana a destination; inventory is never a substitute.
func TestARoutingProfileWithoutAuthorizedRoutesIsRefusedWithTheFieldNamed(t *testing.T) {
	request := baseRequest()
	profileID := contract.RoutingProfileID("rpf_exact")
	request.Target = contract.RoutingTarget{Kind: contract.TargetRoutingProfileID, RoutingProfileID: &profileID}

	events, result := execute(t, happyAdapter(), request)

	if result.Failure == nil {
		t.Fatal("a routing-profile target was served")
	}
	if result.Failure.Code != contract.CodeInvalidRequest {
		t.Errorf("refused with %q", result.Failure.Code)
	}
	if result.Failure.Retryable {
		t.Error("the refusal is retryable, but an identical envelope still names no authorized destination")
	}
	if result.Failure.Param == nil || *result.Failure.Param != "authorizedRoutes" {
		t.Errorf("the refusal names %v as the field at fault", result.Failure.Param)
	}
	if len(events) != 1 || events[0].EventType() != contract.EventError {
		t.Errorf("the refusal produced %d events", len(events))
	}
}

func TestExecutorTransitionAcceptsV1OnlyForADirectModel(t *testing.T) {
	direct := baseRequest()
	direct.SchemaVersion = contract.LegacyRequestEnvelopeVersion
	adapter := happyAdapter()
	events, result := execute(t, adapter, direct)
	if result.Failure != nil {
		t.Fatalf("the transitional direct-model envelope was refused: %v", result.Failure)
	}
	if adapter.attempts() != 1 || len(events) == 0 {
		t.Fatalf("the transitional direct model made %d attempts and %d events", adapter.attempts(), len(events))
	}

	profile := baseRequest()
	profile.SchemaVersion = contract.LegacyRequestEnvelopeVersion
	profileID := contract.RoutingProfileID("rpf_exact")
	profile.Target = contract.RoutingTarget{Kind: contract.TargetRoutingProfileID, RoutingProfileID: &profileID}
	profile.AuthorizedRoutes = []contract.AuthorizedRoute{{
		Substitution:   contract.SubstitutionSameModel,
		DeploymentID:   "dep_test",
		ModelReference: "stub/model@2026-05-01",
		Provider:       "stub",
		Regions:        []contract.Region{"test-region"},
	}}
	blocked := happyAdapter()
	_, refused := execute(t, blocked, profile)
	if refused.Failure == nil || refused.Failure.Code != contract.CodeInvalidRequest {
		t.Fatalf("the v1 profile target refusal = %+v", refused.Failure)
	}
	if blocked.attempts() != 0 {
		t.Fatalf("the v1 profile target reached the adapter %d times", blocked.attempts())
	}
}

func TestCustomerCredentialBindingResolvesExactlyAndNeverUsesThePlatformPool(t *testing.T) {
	request := baseRequest()
	binding := &contract.CustomerProviderCredential{
		CredentialHandle:   "kcred_abcdefghijklmnopqrstuvwxyz",
		CredentialRevision: 4,
		OwnerAccountID:     "acc_customer",
		ConnectionID:       "pcx_exact",
		Environment:        contract.EnvironmentProduction,
	}
	request.AuthorizedRoutes = []contract.AuthorizedRoute{{
		Substitution:               contract.SubstitutionSameModel,
		DeploymentID:               "dep_test",
		ModelReference:             "stub/model@2026-05-01",
		Provider:                   "stub",
		Regions:                    []contract.Region{"test-region"},
		CustomerProviderCredential: binding,
	}}
	wantScope := credentialstore.CustomerCredentialScope{
		CustomerCredentialIdentity: credentialstore.CustomerCredentialIdentity{
			Provider: "stub", OwnerAccountID: "acc_customer", ConnectionID: "pcx_exact",
			Environment: contract.EnvironmentProduction,
		},
		CredentialHandle: string(binding.CredentialHandle),
		Revision:         binding.CredentialRevision,
	}
	const customerSecret = "customer-secret-never-log"
	resolver := &scriptedCustomerResolver{want: wantScope, plaintext: []byte(customerSecret)}
	reporter := &recordingValidationReporter{}
	adapter := happyAdapter()
	var requestPool *provider.KeyPool
	adapter.credentials = func(pool *provider.KeyPool) {
		requestPool = pool
		if pool == nil {
			t.Fatal("the route fell back to the platform credential pool")
		}
		key, ok := pool.Begin().Next(time.Now())
		if !ok || key.Secret() != customerSecret || !key.CustomerOwned() {
			t.Fatalf("the upstream pool received key=%q customerOwned=%v leased=%v", key.Secret(), key.CustomerOwned(), ok)
		}
	}

	_, result := harness{
		deployments:         oneDeployment,
		adapters:            []provider.Adapter{adapter},
		customerCredentials: resolver,
		validationReporter:  reporter,
	}.run(t, request)
	if result.Failure != nil || resolver.calls != 1 || adapter.attempts() != 1 {
		t.Fatalf("the exact BYOK route produced result=%+v resolverCalls=%d attempts=%d", result.Failure, resolver.calls, adapter.attempts())
	}
	if len(result.UpstreamCost.Attempts) != 1 || !result.UpstreamCost.Attempts[0].AttemptUsage.ProviderBilledCustomer || len(result.UpstreamCost.Totals) != 0 {
		t.Fatalf("the BYOK provider expense was misattributed: %+v", result.UpstreamCost)
	}
	if requestPool == nil {
		t.Fatal("the adapter did not receive a request-scoped customer pool")
	}
	if _, ok := requestPool.Begin().Next(time.Now()); ok {
		t.Fatal("the decrypted customer credential remained reachable after execution")
	}
	wantVerdict := oxyvalidation.Verdict{
		ConnectionID: binding.ConnectionID, CredentialHandle: string(binding.CredentialHandle),
		CredentialRevision: binding.CredentialRevision, Environment: binding.Environment,
		State: oxyvalidation.StateValid,
	}
	if verdicts := reporter.all(); len(verdicts) != 1 || verdicts[0] != wantVerdict {
		t.Fatalf("validation verdicts = %#v, expected %#v", verdicts, wantVerdict)
	}
}

func TestCustomerCredentialPoolIsDestroyedWhenAdapterPanics(t *testing.T) {
	request := baseRequest()
	binding, scope := customerBinding("stub")
	request.AuthorizedRoutes = []contract.AuthorizedRoute{{
		Substitution: contract.SubstitutionSameModel, DeploymentID: "dep_test",
		ModelReference: "stub/model@2026-05-01", Provider: "stub",
		Regions: []contract.Region{"test-region"}, CustomerProviderCredential: binding,
	}}
	resolver := &scriptedCustomerResolver{want: scope, plaintext: []byte("customer-secret")}
	var requestPool *provider.KeyPool
	adapter := &scriptedAdapter{
		credentials: func(pool *provider.KeyPool) { requestPool = pool },
		stream: func(context.Context, *provider.Call, provider.Emitter) (provider.Outcome, error) {
			panic("scripted adapter panic")
		},
	}
	executor := harness{
		deployments: oneDeployment, adapters: []provider.Adapter{adapter}, customerCredentials: resolver,
	}.build(t)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		executor.Execute(context.Background(), request, func(contract.StreamEvent) error { return nil })
	}()
	if recovered == nil {
		t.Fatal("the scripted adapter did not panic, so cleanup was not exercised")
	}
	if requestPool == nil {
		t.Fatal("the panicking adapter did not receive a request-scoped pool")
	}
	if requestPool.Configured() {
		t.Fatal("the decrypted customer credential remained configured after adapter panic")
	}
	if _, ok := requestPool.Begin().Next(time.Now()); ok {
		t.Fatal("the decrypted customer credential remained leasable after adapter panic")
	}
}

func TestUnavailableCustomerCredentialFailsClosedBeforeTheUpstream(t *testing.T) {
	request := baseRequest()
	binding := &contract.CustomerProviderCredential{
		CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz", CredentialRevision: 1,
		OwnerAccountID: "acc_customer", ConnectionID: "pcx_exact", Environment: contract.EnvironmentProduction,
	}
	request.AuthorizedRoutes = []contract.AuthorizedRoute{{
		Substitution: contract.SubstitutionSameModel, DeploymentID: "dep_test",
		ModelReference: "stub/model@2026-05-01", Provider: "stub",
		Regions: []contract.Region{"test-region"}, CustomerProviderCredential: binding,
	}}
	resolver := &scriptedCustomerResolver{
		want: credentialstore.CustomerCredentialScope{
			CustomerCredentialIdentity: credentialstore.CustomerCredentialIdentity{
				Provider: "stub", OwnerAccountID: "acc_customer", ConnectionID: "pcx_exact", Environment: contract.EnvironmentProduction,
			}, CredentialHandle: string(binding.CredentialHandle), Revision: 1,
		},
		err: credentialstore.ErrCustomerCredentialUnavailable,
	}
	adapter := happyAdapter()
	_, result := harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, customerCredentials: resolver}.run(t, request)
	if result.Failure == nil || result.Failure.Code != contract.CodeBYOKCredentialInvalid {
		t.Fatalf("the unavailable BYOK generation produced %+v", result.Failure)
	}
	if adapter.attempts() != 0 || result.Report != nil || len(result.UpstreamCost.Attempts) != 0 {
		t.Fatalf("the failed binding reached an upstream or settlement: attempts=%d result=%+v", adapter.attempts(), result)
	}
}

func TestMalformedStoredCustomerCredentialFailsBeforeTransportOrBreaker(t *testing.T) {
	request := baseRequest()
	binding := &contract.CustomerProviderCredential{
		CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz", CredentialRevision: 1,
		OwnerAccountID: "acc_customer", ConnectionID: "pcx_exact", Environment: contract.EnvironmentProduction,
	}
	request.AuthorizedRoutes = []contract.AuthorizedRoute{{
		Substitution: contract.SubstitutionSameModel, DeploymentID: "dep_test",
		ModelReference: "stub/model@2026-05-01", Provider: "stub", Regions: []contract.Region{"test-region"},
		CustomerProviderCredential: binding,
	}}
	resolver := &scriptedCustomerResolver{
		want: credentialstore.CustomerCredentialScope{
			CustomerCredentialIdentity: credentialstore.CustomerCredentialIdentity{
				Provider: "stub", OwnerAccountID: "acc_customer", ConnectionID: "pcx_exact", Environment: contract.EnvironmentProduction,
			}, CredentialHandle: string(binding.CredentialHandle), Revision: binding.CredentialRevision,
		},
		plaintext: []byte{'v', 'a', 'l', 'i', 'd', 0, 'x'},
	}
	adapter := happyAdapter()
	breakers := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 1}, nil)
	_, result := harness{
		deployments: oneDeployment, adapters: []provider.Adapter{adapter},
		rotation: breakers, customerCredentials: resolver,
	}.run(t, request)
	if result.Failure == nil || result.Failure.Code != contract.CodeBYOKCredentialInvalid {
		t.Fatalf("the malformed stored customer credential produced %+v", result.Failure)
	}
	if adapter.attempts() != 0 || result.Report != nil || len(result.UpstreamCost.Attempts) != 0 {
		t.Fatalf("the malformed stored credential reached transport or settlement: attempts=%d result=%+v", adapter.attempts(), result)
	}
	health := breakers.Project([]contract.DeploymentID{"dep_test"})[0]
	if health.ConsecutiveFailures != 0 || health.State != rotation.StateClosed {
		t.Fatalf("the malformed customer credential damaged the shared breaker: %+v", health)
	}
}

func TestCustomerCredentialRefusalDoesNotTripTheSharedBreakerOrFailOver(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		providerCode contract.ErrorCode
		wantInvalid  bool
	}{
		{name: "authentication", providerCode: contract.CodeProviderCredentialInvalid, wantInvalid: true},
		{name: "billing", providerCode: contract.CodeProviderBillingRefused, wantInvalid: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := routingProfileRequest()
			binding := &contract.CustomerProviderCredential{
				CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz", CredentialRevision: 3,
				OwnerAccountID: "acc_customer", ConnectionID: "pcx_exact", Environment: contract.EnvironmentProduction,
			}
			request.AuthorizedRoutes[0].CustomerProviderCredential = binding
			resolver := &scriptedCustomerResolver{
				want: credentialstore.CustomerCredentialScope{
					CustomerCredentialIdentity: credentialstore.CustomerCredentialIdentity{
						Provider: "stub", OwnerAccountID: "acc_customer", ConnectionID: "pcx_exact", Environment: contract.EnvironmentProduction,
					}, CredentialHandle: string(binding.CredentialHandle), Revision: binding.CredentialRevision,
				},
				plaintext: []byte("customer-secret"),
			}
			primary := failingAdapter("stub", provider.ErrCustomerCredential{Code: testCase.providerCode}, nil)
			backup := succeedingAdapter("backup", 1)
			breakers := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 1}, nil)
			reporter := &recordingValidationReporter{}
			_, result := harness{
				deployments: routingProfileDeployments, adapters: []provider.Adapter{primary, backup},
				rotation: breakers, customerCredentials: resolver, validationReporter: reporter,
			}.run(t, request)
			if result.Failure == nil || result.Failure.Code != contract.CodeBYOKCredentialInvalid {
				t.Fatalf("the customer refusal produced %+v", result.Failure)
			}
			if primary.attempts() != 1 || backup.attempts() != 0 {
				t.Fatalf("the customer refusal attempted primary=%d backup=%d", primary.attempts(), backup.attempts())
			}
			health := breakers.Project([]contract.DeploymentID{"dep_a"})[0]
			if health.ConsecutiveFailures != 0 || health.State != rotation.StateClosed {
				t.Fatalf("the customer refusal damaged the shared deployment breaker: %+v", health)
			}
			verdicts := reporter.all()
			if !testCase.wantInvalid {
				if len(verdicts) != 0 {
					t.Fatalf("billing refusal incorrectly disabled a revalidatable key: %#v", verdicts)
				}
				return
			}
			wantVerdict := oxyvalidation.Verdict{
				ConnectionID: binding.ConnectionID, CredentialHandle: string(binding.CredentialHandle),
				CredentialRevision: binding.CredentialRevision, Environment: binding.Environment,
				State: oxyvalidation.StateInvalid, FailureCode: oxyvalidation.FailureUnauthorized,
			}
			if len(verdicts) != 1 || verdicts[0] != wantVerdict {
				t.Fatalf("customer authentication validation verdicts = %#v", verdicts)
			}
		})
	}
}

func TestCustomerCredentialThrottleDoesNotTripTheSharedBreakerOrFailOver(t *testing.T) {
	request := routingProfileRequest()
	binding, scope := customerBinding("stub")
	request.AuthorizedRoutes[0].CustomerProviderCredential = binding
	resolver := &scriptedCustomerResolver{want: scope, plaintext: []byte("customer-secret")}
	pool, err := provider.NewCustomerKeyPool("stub", []byte("customer-secret"))
	if err != nil {
		t.Fatalf("building the customer pool: %v", err)
	}
	key, leased := pool.Begin().Next(time.Now())
	if !leased {
		t.Fatal("the customer pool leased no credential")
	}
	throttle := provider.CustomerCredentialFailure(key, provider.ErrUpstream{
		Code: contract.CodeRateLimited, Category: contract.UpstreamRateLimit,
		Detail: "the customer provider account is throttled", RetryAfterMs: 1700,
	})
	primary := failingAdapter("stub", throttle, nil)
	backup := succeedingAdapter("backup", 1)
	breakers := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 1}, nil)
	reporter := &recordingValidationReporter{}

	executor := harness{
		deployments: routingProfileDeployments, adapters: []provider.Adapter{primary, backup},
		rotation: breakers, customerCredentials: resolver, validationReporter: reporter,
	}.build(t)
	_, result := runOn(executor, request)
	if result.Failure == nil || result.Failure.Code != contract.CodeRateLimited || result.Failure.RetryAfterMs == nil || *result.Failure.RetryAfterMs != 1700 {
		t.Fatalf("the customer throttle produced %+v", result.Failure)
	}
	if primary.attempts() != 1 || backup.attempts() != 0 {
		t.Fatalf("the customer throttle attempted primary=%d backup=%d", primary.attempts(), backup.attempts())
	}
	health := breakers.Project([]contract.DeploymentID{"dep_a"})[0]
	if health.ConsecutiveFailures != 0 || health.State != rotation.StateClosed {
		t.Fatalf("the customer throttle damaged the shared deployment breaker: %+v", health)
	}
	_, blocked := runOn(executor, request)
	if blocked.Failure == nil || blocked.Failure.Code != contract.CodeRateLimited || blocked.Failure.RetryAfterMs == nil || *blocked.Failure.RetryAfterMs <= 0 {
		t.Fatalf("the exact credential throttle was not retained: %+v", blocked.Failure)
	}
	if primary.attempts() != 1 || resolver.calls != 1 || backup.attempts() != 0 {
		t.Fatalf("the throttled credential reached resolver/upstream again: primary=%d resolver=%d backup=%d", primary.attempts(), resolver.calls, backup.attempts())
	}
	if verdicts := reporter.all(); len(verdicts) != 0 {
		t.Fatalf("a transient throttle produced disabling validation: %#v", verdicts)
	}
}

func TestPlatformAndCustomerCredentialBreakerLanesCannotInterfere(t *testing.T) {
	breakers := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 1, Cooldown: time.Hour}, nil)
	resolver := &scriptedCustomerResolver{plaintext: []byte("customer-secret")}
	adapter := &scriptedAdapter{slug: "stub"}
	adapter.stream = func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if call.Route.CustomerProviderCredential == nil {
			return provider.Outcome{}, provider.ErrUpstream{
				Code: contract.CodeProviderCredentialInvalid, Category: contract.UpstreamAuthentication,
				Detail: "the platform credential is refused",
			}
		}
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{
			Units:        []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}},
			UsageSource:  contract.UsageProviderReported,
			FinishReason: contract.FinishStop,
		}, nil
	}
	executor := harness{
		deployments: oneDeployment, adapters: []provider.Adapter{adapter},
		rotation: breakers, customerCredentials: resolver,
	}.build(t)

	_, platformResult := runOn(executor, baseRequest())
	if platformResult.Failure == nil || platformResult.Failure.Code != contract.CodeProviderCredentialInvalid {
		t.Fatalf("the platform credential control request produced %+v", platformResult.Failure)
	}
	healthAfterPlatformFailure := breakers.Project([]contract.DeploymentID{"dep_test"})[0]
	if healthAfterPlatformFailure.State != rotation.StateOpen || healthAfterPlatformFailure.ConsecutiveFailures != 1 {
		t.Fatalf("the platform credential did not open its own breaker: %+v", healthAfterPlatformFailure)
	}

	byokRequest := baseRequest()
	binding, scope := customerBinding("stub")
	binding.CredentialRevision = 1
	scope.Revision = 1
	resolver.want = scope
	byokRequest.AuthorizedRoutes = []contract.AuthorizedRoute{{
		Substitution: contract.SubstitutionSameModel, DeploymentID: "dep_test",
		ModelReference: "stub/model@2026-05-01", Provider: "stub", Regions: []contract.Region{"test-region"},
		CustomerProviderCredential: binding,
	}}
	_, byokResult := runOn(executor, byokRequest)
	if byokResult.Failure != nil {
		t.Fatalf("the open platform breaker blocked healthy BYOK: %+v", byokResult.Failure)
	}
	healthAfterBYOKSuccess := breakers.Project([]contract.DeploymentID{"dep_test"})[0]
	if healthAfterBYOKSuccess.State != rotation.StateOpen || healthAfterBYOKSuccess.ConsecutiveFailures != 1 {
		t.Fatalf("the BYOK success rehabilitated the platform breaker: %+v", healthAfterBYOKSuccess)
	}
}

const routingProfileDeployments = `
  {"deploymentId":"dep_a","provider":"stub","modelReference":"stub/model@2026-05-01",
   "upstreamModelId":"model-a","regions":["r1"],"current":true},
  {"deploymentId":"dep_b","provider":"backup","modelReference":"backup/other@2026-06-01",
   "upstreamModelId":"model-b","regions":["r2"],"current":true},
  {"deploymentId":"dep_c","provider":"unlisted","modelReference":"unlisted/third@2026-07-01",
   "upstreamModelId":"model-c","regions":["r3"],"current":true}`

func routingProfileRequest() *contract.Request {
	request := baseRequest()
	profileID := contract.RoutingProfileID("rpf_exact")
	authorized := true
	request.Target = contract.RoutingTarget{Kind: contract.TargetRoutingProfileID, RoutingProfileID: &profileID}
	request.AuthorizedRoutes = []contract.AuthorizedRoute{
		{
			Substitution:   contract.SubstitutionSameModel,
			DeploymentID:   "dep_a",
			ModelReference: "stub/model@2026-05-01",
			Provider:       "stub",
			Regions:        []contract.Region{"r1"},
		},
		{
			Substitution:       contract.SubstitutionCrossModel,
			DeploymentID:       "dep_b",
			ModelReference:     "backup/other@2026-06-01",
			Provider:           "backup",
			Regions:            []contract.Region{"r2"},
			AuthorizedByPolicy: &authorized,
		},
	}
	return request
}

func TestRoutingProfileFailsOverOnlyWithinItsAuthorizedRouteList(t *testing.T) {
	primary := failingAdapter("stub", overloaded("stub"), nil)
	secondary := succeedingAdapter("backup", 9)
	unlisted := succeedingAdapter("unlisted", 99)

	events, result := harness{
		deployments: routingProfileDeployments,
		adapters:    []provider.Adapter{primary, secondary, unlisted},
	}.run(t, routingProfileRequest())

	if result.Failure != nil {
		t.Fatalf("the profile's authorized alternate was not served: %v", result.Failure)
	}
	if primary.attempts() != 1 || secondary.attempts() != 1 {
		t.Fatalf("authorized attempts were primary=%d alternate=%d", primary.attempts(), secondary.attempts())
	}
	if unlisted.attempts() != 0 {
		t.Fatalf("an inventory route absent from authorizedRoutes was attempted %d times", unlisted.attempts())
	}
	if result.Report == nil || result.Report.ResolvedModelReference != "backup/other@2026-06-01" {
		t.Fatalf("the settled route is %+v", result.Report)
	}

	switches := eventsOfType(events, contract.EventRouteSwitch)
	if len(switches) != 1 {
		t.Fatalf("the cross-model failover emitted %d route switches", len(switches))
	}
	detail := switches[0].(*contract.StreamRouteSwitchEvent).Detail
	if detail.Scope != contract.SwitchScopeModel || detail.AuthorizedByPolicy == nil || !*detail.AuthorizedByPolicy {
		t.Fatalf("the cross-model switch is not reported as policy-authorized: %+v", detail)
	}
	if detail.RequestedModelID == nil || *detail.RequestedModelID != "stub/model" ||
		detail.FromModelReference == nil || *detail.FromModelReference != "stub/model@2026-05-01" ||
		detail.ToModelReference == nil || *detail.ToModelReference != "backup/other@2026-06-01" {
		t.Errorf("the cross-model switch reports the wrong path: %+v", detail)
	}
}

func TestAuthorizedRouteMetadataMustMatchInventoryExactly(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*contract.AuthorizedRoute)
	}{
		{
			name: "deployment is not inventoried for the reference",
			mutate: func(route *contract.AuthorizedRoute) {
				route.DeploymentID = "dep_missing"
			},
		},
		{
			name: "model reference disagrees",
			mutate: func(route *contract.AuthorizedRoute) {
				route.ModelReference = "backup/other@2026-06-01"
			},
		},
		{
			name: "provider disagrees",
			mutate: func(route *contract.AuthorizedRoute) {
				route.Provider = "backup"
			},
		},
		{
			name: "regions disagree",
			mutate: func(route *contract.AuthorizedRoute) {
				route.Regions = []contract.Region{"r9"}
			},
		},
		{
			name: "an empty signed set disagrees with attested inventory",
			mutate: func(route *contract.AuthorizedRoute) {
				route.Regions = []contract.Region{}
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			primary := succeedingAdapter("stub", 1)
			backup := succeedingAdapter("backup", 1)
			unlisted := succeedingAdapter("unlisted", 1)
			request := routingProfileRequest()
			request.AuthorizedRoutes = request.AuthorizedRoutes[:1]
			testCase.mutate(&request.AuthorizedRoutes[0])

			_, result := harness{
				deployments: routingProfileDeployments,
				adapters:    []provider.Adapter{primary, backup, unlisted},
			}.run(t, request)

			if result.Failure == nil || result.Failure.Code != contract.CodeInvalidRequest {
				t.Fatalf("mismatched metadata was reported as %v", result.Failure)
			}
			if result.Failure.Param == nil || *result.Failure.Param != "authorizedRoutes[0]" {
				t.Errorf("the refusal names %v as the offending field", result.Failure.Param)
			}
			if primary.attempts()+backup.attempts()+unlisted.attempts() != 0 {
				t.Fatal("an adapter was reached before the authorized route list was validated whole")
			}
		})
	}

	// Positive control: the same first entry executes when every signed field
	// agrees, so the table above is not passing because this fixture never runs.
	primary := succeedingAdapter("stub", 1)
	request := routingProfileRequest()
	request.AuthorizedRoutes = request.AuthorizedRoutes[:1]
	if _, result := (harness{
		deployments: routingProfileDeployments,
		adapters:    []provider.Adapter{primary, succeedingAdapter("backup", 1), succeedingAdapter("unlisted", 1)},
	}).run(t, request); result.Failure != nil {
		t.Fatalf("matching route metadata was refused: %v", result.Failure)
	}
	if primary.attempts() != 1 {
		t.Fatal("matching metadata still did not reach its authorized adapter")
	}
}

func TestAuthorizedRouteListIsValidatedWholeBeforeItsPrimaryExecutes(t *testing.T) {
	primary := succeedingAdapter("stub", 1)
	backup := succeedingAdapter("backup", 1)
	unlisted := succeedingAdapter("unlisted", 1)
	request := routingProfileRequest()
	request.AuthorizedRoutes[1].Provider = "unlisted"

	_, result := harness{
		deployments: routingProfileDeployments,
		adapters:    []provider.Adapter{primary, backup, unlisted},
	}.run(t, request)

	if result.Failure == nil || result.Failure.Code != contract.CodeInvalidRequest {
		t.Fatalf("a bad second route was reported as %v", result.Failure)
	}
	if result.Failure.Param == nil || *result.Failure.Param != "authorizedRoutes[1]" {
		t.Errorf("the refusal names %v as the offending field", result.Failure.Param)
	}
	if primary.attempts()+backup.attempts()+unlisted.attempts() != 0 {
		t.Fatal("the authorized route list was executed before every entry was validated")
	}
}

func TestConcreteTargetRejectsAnAuthorizedCrossModelEntry(t *testing.T) {
	primary := succeedingAdapter("stub", 1)
	backup := succeedingAdapter("backup", 1)
	unlisted := succeedingAdapter("unlisted", 1)
	request := routingProfileRequest()
	unpinned := contract.ModelReference("stub/model")
	request.Target = contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &unpinned}

	_, result := harness{
		deployments: routingProfileDeployments,
		adapters:    []provider.Adapter{primary, backup, unlisted},
	}.run(t, request)

	if result.Failure == nil || result.Failure.Code != contract.CodeInvalidRequest {
		t.Fatalf("a concrete cross-model entry was reported as %v", result.Failure)
	}
	if result.Failure.Param == nil || *result.Failure.Param != "authorizedRoutes[1]" {
		t.Errorf("the refusal names %v as the offending field", result.Failure.Param)
	}
	if primary.attempts()+backup.attempts()+unlisted.attempts() != 0 {
		t.Fatal("a concrete cross-model list reached an adapter")
	}
}

func TestAnEmptySignedRegionSetMatchesUnattestedInventory(t *testing.T) {
	const deploymentWithoutRegion = `
  {"deploymentId":"dep_a","provider":"stub","modelReference":"stub/model@2026-05-01",
   "upstreamModelId":"model-a","current":true}`
	primary := succeedingAdapter("stub", 1)
	request := routingProfileRequest()
	request.AuthorizedRoutes = request.AuthorizedRoutes[:1]
	request.AuthorizedRoutes[0].Regions = []contract.Region{}

	_, result := harness{
		deployments: deploymentWithoutRegion,
		adapters:    []provider.Adapter{primary},
	}.run(t, request)

	if result.Failure != nil {
		t.Fatalf("matching empty unattested region sets were refused: %v", result.Failure)
	}
	if primary.attempts() != 1 {
		t.Fatal("matching empty unattested region sets did not reach the adapter")
	}
}

func TestASignedRegionCannotMatchUnattestedInventory(t *testing.T) {
	const deploymentWithoutRegion = `
  {"deploymentId":"dep_a","provider":"stub","modelReference":"stub/model@2026-05-01",
   "upstreamModelId":"model-a","current":true}`
	primary := succeedingAdapter("stub", 1)
	request := routingProfileRequest()
	request.AuthorizedRoutes = request.AuthorizedRoutes[:1]

	_, result := harness{
		deployments: deploymentWithoutRegion,
		adapters:    []provider.Adapter{primary},
	}.run(t, request)

	if result.Failure == nil || result.Failure.Code != contract.CodeInvalidRequest {
		t.Fatalf("a signed region with no inventory attestation was reported as %v", result.Failure)
	}
	if result.Failure.Param == nil || *result.Failure.Param != "authorizedRoutes[0]" {
		t.Errorf("the refusal names %v as the offending field", result.Failure.Param)
	}
	if !strings.Contains(result.Failure.Message, "do not match inventory regions") {
		t.Errorf("the refusal does not explain the mismatched authority: %q", result.Failure.Message)
	}
	if primary.attempts() != 0 {
		t.Fatal("the route executed without inventory attestation for its signed region")
	}
}

func TestAnEnvelopeWithoutTheInvokeScopeIsRefused(t *testing.T) {
	request := baseRequest()
	request.Attribution.Principal.InferenceScopes = []contract.Scope{contract.ScopeModelsRead}

	_, result := execute(t, happyAdapter(), request)

	if result.Failure == nil {
		t.Fatal("an envelope without inference:invoke was served")
	}
	if result.Failure.Code != contract.CodeInsufficientScope {
		t.Errorf("refused with %q", result.Failure.Code)
	}
}

func TestAnUnroutableModelIsRefusedAsNotFound(t *testing.T) {
	request := baseRequest()
	reference := contract.ModelReference("stub/other@2026-05-01")
	request.Target.ModelReference = &reference

	_, result := execute(t, happyAdapter(), request)

	if result.Failure == nil {
		t.Fatal("an unroutable model was served")
	}
	if result.Failure.Code != contract.CodeModelNotFound {
		t.Errorf("refused with %q", result.Failure.Code)
	}
	if result.Failure.Retryable {
		t.Error("model_not_found was reported retryable; no identical retry makes a route appear")
	}
}

/* -------------------------------------------------------------------------- */
/*  Framing the emitter enforces                                              */
/* -------------------------------------------------------------------------- */

func TestTheEmitterRefusesAStreamTheContractCannotDescribe(t *testing.T) {
	cases := []struct {
		name   string
		stream func(ctx context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error)
		expect string
	}{
		{
			name: "output before the stream started",
			stream: func(_ context.Context, _ *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				return provider.Outcome{}, out.Delta(0, contract.ChannelOutputText, "hello")
			},
			expect: "precedes the stream's start event",
		},
		{
			name: "two start events",
			stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
					return provider.Outcome{}, err
				}
				return provider.Outcome{}, out.Start(call.Route.ModelReference, time.Now())
			},
			expect: "second start event",
		},
		{
			name: "a start event naming an unpinned model",
			stream: func(_ context.Context, _ *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				return provider.Outcome{}, out.Start("stub/model", time.Now())
			},
			expect: "not revision-pinned",
		},
		{
			name: "a usage event carrying no units",
			stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
					return provider.Outcome{}, err
				}
				return provider.Outcome{}, out.Usage(nil, contract.UsageProviderReported)
			},
			expect: "at least one unit",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			adapter := &scriptedAdapter{stream: func(ctx context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				err := func() error {
					_, err := testCase.stream(ctx, call, out)
					return err
				}()
				if err == nil {
					t.Fatal("the emitter accepted an event it should have refused")
				}
				if !strings.Contains(err.Error(), testCase.expect) {
					t.Errorf("the emitter refused with %q, expected it to mention %q", err, testCase.expect)
				}
				// Returned so the executor treats this as a failed request,
				// which is what it is.
				return provider.Outcome{}, err
			}}
			_, result := execute(t, adapter, baseRequest())
			if result.Failure == nil {
				t.Error("a stream the contract cannot describe was reported as a success")
			}
		})
	}
}

// TestAnAdapterThatCompletesWithoutStartingIsAFailure covers the shape that
// would otherwise hand settlement a receipt for output nobody saw.
func TestAnAdapterThatCompletesWithoutStartingIsAFailure(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(context.Context, *provider.Call, provider.Emitter) (provider.Outcome, error) {
		return provider.Outcome{
			Units:       []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 500}},
			UsageSource: contract.UsageProviderReported,
		}, nil
	}}

	_, result := execute(t, adapter, baseRequest())

	if result.Failure == nil {
		t.Fatal("an adapter that never started a stream was reported as a success")
	}
	if result.Report == nil || result.Report.Outcome != contract.OutcomeFailed {
		t.Errorf("the report says %v", result.Report)
	}
}

/* -------------------------------------------------------------------------- */
/*  Settlement                                                                */
/* -------------------------------------------------------------------------- */

func TestASuccessfulRequestProducesASettleableReport(t *testing.T) {
	events, result := execute(t, happyAdapter(), baseRequest())

	if result.Failure != nil {
		t.Fatalf("the request failed: %v", result.Failure)
	}
	if result.Report == nil {
		t.Fatal("no usage report was produced")
	}
	if err := result.Report.Validate(); err != nil {
		t.Fatalf("the report would be rejected by the contract: %v", err)
	}
	if result.Report.Outcome != contract.OutcomeCompleted {
		t.Errorf("the report says %q", result.Report.Outcome)
	}
	if result.Report.GenerationID == nil || *result.Report.GenerationID == "" {
		t.Error("no generation id was allocated, so the request has no receipt handle")
	}
	if last := events[len(events)-1]; last.EventType() != contract.EventDone {
		t.Errorf("the stream ends with %q", last.EventType())
	}
	// The data plane measures; the control plane prices. A receipt id on a
	// done event would be Kaana quoting a settlement it did not compute.
	done := events[len(events)-1].(*contract.StreamDoneEvent)
	if done.ReceiptID != nil {
		t.Error("the done event carries a receipt id, which only settlement can produce")
	}
}

// TestAnAdapterThatReportsNothingGetsAnEstimatedCompletedReport covers the
// report-v2 rule without turning a provider's otherwise valid answer into an
// internal error. The estimate comes from the normalized request and emitted
// output, never from a price.
func TestAnAdapterThatReportsNothingGetsAnEstimatedCompletedReport(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "answer"); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{FinishReason: contract.FinishStop}, nil
	}}
	cards, err := providercost.Parse([]byte(`{"rateCards":[{"deploymentId":"dep_test","currency":"XTS","rates":[
		{"unit":"requests","amountPerUnit":100},
		{"unit":"input_tokens","amountPerUnit":10},
		{"unit":"output_tokens","amountPerUnit":20}
	]}]}`))
	if err != nil {
		t.Fatalf("building fallback rate-card fixture: %v", err)
	}

	events, result := (harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, costs: cards}).run(t, baseRequest())

	if result.Failure != nil {
		t.Fatalf("provider completion failed because usage was absent: %+v", result.Failure)
	}
	if result.Report == nil {
		t.Fatal("provider completion has no estimated usage report")
	}
	if result.Report.Outcome != contract.OutcomeCompleted || result.Report.UsageSource != contract.UsageEstimated {
		t.Fatalf("estimated completion report = %+v", result.Report)
	}
	reported := make(map[contract.UsageUnit]int, len(result.Report.Units))
	for _, quantity := range result.Report.Units {
		reported[quantity.Unit] = quantity.Quantity
	}
	if reported[contract.UnitRequests] != 1 || reported[contract.UnitInputTokens] <= 0 || reported[contract.UnitOutputTokens] <= 0 {
		t.Fatalf("estimated units = %v", result.Report.Units)
	}
	if len(events) < 2 || events[len(events)-2].EventType() != contract.EventUsage || events[len(events)-1].EventType() != contract.EventDone {
		t.Fatalf("estimated usage must immediately precede done; got %d events", len(events))
	}
	if len(result.UpstreamCost.Attempts) != 1 || len(result.UpstreamCost.Totals) != 1 {
		t.Fatalf("estimated units did not reach upstream-cost accounting: %+v", result.UpstreamCost)
	}
	wantCost := int64(100 + reported[contract.UnitInputTokens]*10 + reported[contract.UnitOutputTokens]*20)
	if got := result.UpstreamCost.Totals[0].Amount; got != wantCost {
		t.Fatalf("estimated upstream cost = %d, expected %d from the existing test rate card", got, wantCost)
	}
}

func TestBrokenSinkKeepsEstimatedPartialUsageWithoutAnotherWrite(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "delivered"); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "not delivered"); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{FinishReason: contract.FinishStop}, nil
	}}
	cards, err := providercost.Parse([]byte(`{"rateCards":[{"deploymentId":"dep_test","currency":"XTS","rates":[
		{"unit":"requests","amountPerUnit":100},
		{"unit":"input_tokens","amountPerUnit":10},
		{"unit":"output_tokens","amountPerUnit":20}
	]}]}`))
	if err != nil {
		t.Fatalf("building sink-failure rate-card fixture: %v", err)
	}
	executor := (harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, costs: cards}).build(t)

	var deltaWrites, usageWrites int
	result := executor.Execute(context.Background(), baseRequest(), func(event contract.StreamEvent) error {
		switch event.EventType() {
		case contract.EventDelta:
			deltaWrites++
			if deltaWrites == 2 {
				return kaana.ErrClientGone
			}
		case contract.EventUsage:
			usageWrites++
		}
		return nil
	})

	if result.Report == nil || result.Report.Outcome != contract.OutcomeCancelled || result.Report.UsageSource != contract.UsageEstimated {
		t.Fatalf("broken-sink settlement = report=%+v failure=%+v", result.Report, result.Failure)
	}
	reported := make(map[contract.UsageUnit]int, len(result.Report.Units))
	for _, quantity := range result.Report.Units {
		reported[quantity.Unit] = quantity.Quantity
	}
	if reported[contract.UnitRequests] != 1 || reported[contract.UnitOutputTokens] <= 0 {
		t.Fatalf("partial estimated units = %v", result.Report.Units)
	}
	if usageWrites != 0 {
		t.Fatalf("executor attempted %d usage writes after the sink failed", usageWrites)
	}
	if len(result.UpstreamCost.Attempts) != 1 || len(result.UpstreamCost.Attempts[0].Units) == 0 {
		t.Fatalf("partial estimate did not reach provider-cost accounting: %+v", result.UpstreamCost)
	}
}

func TestPreOutputFailureDoesNotEstimateFromInputAlone(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(_ context.Context, _ *provider.Call, _ provider.Emitter) (provider.Outcome, error) {
		return provider.Outcome{}, provider.ErrUpstream{
			Code: contract.CodeRateLimited, Category: contract.UpstreamRateLimit, Detail: "pre-output refusal",
		}
	}}

	_, result := execute(t, adapter, baseRequest())
	if result.Report == nil || result.Report.Outcome != contract.OutcomeFailed {
		t.Fatalf("pre-output failure report = %+v", result.Report)
	}
	if len(result.Report.Units) != 0 {
		t.Fatalf("pre-output failure was estimated from input alone: %v", result.Report.Units)
	}
	if len(result.UpstreamCost.Attempts) != 1 || len(result.UpstreamCost.Attempts[0].Units) != 0 {
		t.Fatalf("pre-output failure reached cost units: %+v", result.UpstreamCost)
	}
}

func TestPartialProviderEstimateKeepsExactInputAndAddsDeliveredOutput(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "a delivered answer fragment"); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{
				Units: []contract.UsageQuantity{
					{Unit: contract.UnitRequests, Quantity: 1},
					{Unit: contract.UnitInputTokens, Quantity: 37},
					{Unit: contract.UnitOutputTokens, Quantity: 0},
				},
				UsageSource: contract.UsageEstimated,
			}, provider.ErrUpstream{
				Code: contract.CodeProviderError, Category: contract.UpstreamUnknown,
				Detail: "the provider stopped after partial output",
			}
	}}

	_, result := execute(t, adapter, baseRequest())
	if result.Report == nil || result.Report.Outcome != contract.OutcomePartial || result.Report.UsageSource != contract.UsageEstimated {
		t.Fatalf("partial provider estimate = report=%+v failure=%+v", result.Report, result.Failure)
	}
	quantities := usageQuantitiesByUnit(result.Report.Units)
	if quantities[contract.UnitInputTokens] != 37 {
		t.Fatalf("provider input count was replaced by fallback: %v", result.Report.Units)
	}
	if quantities[contract.UnitOutputTokens] <= 0 {
		t.Fatalf("delivered output was absent from partial settlement: %v", result.Report.Units)
	}
	if len(result.UpstreamCost.Attempts) != 1 || usageQuantitiesByUnit(result.UpstreamCost.Attempts[0].Units)[contract.UnitOutputTokens] <= 0 {
		t.Fatalf("delivered output was absent from provider-cost reconciliation: %+v", result.UpstreamCost)
	}
}

func usageQuantitiesByUnit(units []contract.UsageQuantity) map[contract.UsageUnit]int {
	quantities := make(map[contract.UsageUnit]int, len(units))
	for _, quantity := range units {
		quantities[quantity.Unit] = quantity.Quantity
	}
	return quantities
}

// TestAReportTheContractWouldRejectIsNotReturnedAsIfItWereFine: an invalid
// usage report is not a lost log line — it is the record settlement runs
// against, so a request that executed and cannot be settled must say so.
func TestAReportTheContractWouldRejectIsNotReturnedAsIfItWereFine(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{
			// One unit reported twice: the contract's usage report refuses it,
			// because a unit is settled once, as a total.
			Units: []contract.UsageQuantity{
				{Unit: contract.UnitOutputTokens, Quantity: 10},
				{Unit: contract.UnitOutputTokens, Quantity: 20},
			},
			UsageSource:  contract.UsageProviderReported,
			FinishReason: contract.FinishStop,
		}, nil
	}}

	_, result := execute(t, adapter, baseRequest())

	if result.Report != nil {
		t.Fatal("a report the contract would reject was returned as if it were settleable")
	}
	if result.Failure == nil || result.Failure.Code != contract.CodeInternalError {
		t.Fatalf("the unsettleable request reported %v", result.Failure)
	}
}

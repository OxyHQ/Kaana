package kaana

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

type scopedClaimFixture struct {
	mu    sync.Mutex
	seen  map[string]bool
	calls int
	fail  bool
}

func (f *scopedClaimFixture) ClaimScopedAttempt(_ context.Context, claim credentialstore.ScopedAttemptClaim) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return false, errors.New("synthetic uncertain database result")
	}
	if f.seen[claim.PermitID] {
		return false, nil
	}
	f.seen[claim.PermitID] = true
	return true, nil
}

type scopedAdapterFixture struct {
	pool    *provider.KeyPool
	sends   int
	claims  *scopedClaimFixture
	failure error
}

func (f *scopedAdapterFixture) Provider() contract.ProviderSlug { return "openrouter" }
func (f *scopedAdapterFixture) APIFormats() []contract.APIFormat {
	return []contract.APIFormat{contract.APIFormatDecisions}
}
func (f *scopedAdapterFixture) PlatformCredentials() *provider.KeyPool { return f.pool }
func (f *scopedAdapterFixture) Health(context.Context) provider.Health { return provider.Health{} }
func (f *scopedAdapterFixture) Translate(r *contract.Request, route provider.Route) (*provider.Call, error) {
	return &provider.Call{Route: route, Decisions: r.Input.Decisions}, nil
}
func (f *scopedAdapterFixture) Stream(ctx context.Context, call *provider.Call, _ provider.Emitter, pool *provider.KeyPool) (provider.Outcome, error) {
	outcome, key, err := provider.WalkAttempts(ctx, pool, call, func(_ context.Context, _ provider.Key) (provider.Outcome, provider.CredentialedAttempt) {
		f.claims.mu.Lock()
		claimed := f.claims.calls > 0
		f.claims.mu.Unlock()
		if !claimed {
			panic("upstream reached before durable claim")
		}
		f.sends++
		if f.failure != nil {
			return provider.Outcome{}, provider.CredentialedAttempt{Transport: true, Failure: f.failure}
		}
		probability := 0.75
		return provider.Outcome{Decisions: []contract.DecisionAnswer{{ID: "q", Kind: "noul", Probability: &probability}}, Units: []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 1}, {Unit: contract.UnitOutputTokens, Quantity: 0}}, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}, provider.CredentialedAttempt{Accepted: true}
	})
	outcome.KeyID = key.ID
	return outcome, err
}
func scopedExecutorFixture(t *testing.T) (*Executor, *contract.Request, *scopedAdapterFixture, *scopedClaimFixture) {
	t.Helper()
	return scopedExecutorFixtureAt(t, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), 2*time.Minute)
}
func scopedExecutorFixtureAt(t *testing.T, at time.Time, sourceLifetime time.Duration) (*Executor, *contract.Request, *scopedAdapterFixture, *scopedClaimFixture) {
	t.Helper()
	audience := contract.ScopedExecutionAudience{PermitID: "synthetic-permit-" + strings.ReplaceAll(t.Name(), "/", "-"), IdempotencyKey: "idem", FixtureSHA256: strings.Repeat("a", 64), ExpiresAt: at.Add(sourceLifetime).Format(time.RFC3339), Principal: contract.ScopedExecutionPrincipal{AccountID: "account", ApplicationID: "app", CredentialID: "credential", Environment: contract.EnvironmentProduction}, Policy: contract.RoutingPolicyReference{RoutingPolicyID: "policy", PolicyVersion: 1}, DeploymentID: "dep-private", Provider: "openrouter", KeyID: "exact-key", ModelReference: "typesafe/jev-1.13@2026-09-17", UpstreamModelID: "typesafe/jev-1.13-20260917", PriceVersionID: "oxy-price", ProviderRateCardVersionID: "card", ProviderSourceVersion: "source", MaxCostUSD: "0.01"}
	deployment := inventory.Deployment{ScopedExecution: &audience, DeploymentID: audience.DeploymentID, Provider: audience.Provider, ModelReference: audience.ModelReference, UpstreamModelID: audience.UpstreamModelID, Regions: []contract.Region{"test-region"}}
	raw, err := json.Marshal(map[string]any{"snapshotId": "snapshot", "issuedAt": contract.NewTimestamp(at), "deployments": []inventory.Deployment{deployment}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := inventory.NewStore(inventory.Config{Path: path, Now: func() time.Time { return at }, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	claims := &scopedClaimFixture{seen: map[string]bool{}}
	pool, err := provider.NewKeyPool("openrouter", []provider.KeyDeclaration{{KeyID: "exact-key", Secret: "synthetic-only"}, {KeyID: "other-key", Secret: "synthetic-other"}}, provider.KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &scopedAdapterFixture{pool: pool, claims: claims}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.ReplaceGeneration([]provider.CredentialBinding{{DeploymentID: audience.DeploymentID, Provider: audience.Provider, KeyID: audience.KeyID}}, adapter); err != nil {
		t.Fatal(err)
	}
	cards, err := providercost.Parse([]byte(`{"schemaVersion":1,"rateCardVersionId":"card","source":"operator","sourceVersion":"source","observedAt":"2026-10-02T00:00:00Z","effectiveAt":"2026-10-02T00:00:00Z","rateCards":[{"deploymentId":"dep-private","currency":"USD","rates":[{"unit":"input_tokens","amountPerUnit":42000},{"unit":"output_tokens","amountPerUnit":0}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewExecutor(Config{Inventory: store, Providers: registry, Rotation: rotation.NewRegistry(rotation.Policy{}, func() time.Time { return at }), Costs: cards, ScopedAttemptClaims: claims, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	// Internal synthetic fixtures cannot activate the production source manifest.
	e.scopedSource = func() *contract.ScopedExecutionAudience { return &audience }
	idem := audience.IdempotencyKey
	reference := audience.ModelReference
	request := &contract.Request{SchemaVersion: contract.ScopedRequestEnvelopeVersion, Attribution: contract.Attribution{RequestID: "request-1", Principal: contract.AuthenticatedPrincipal{Billing: contract.BillingPrincipal{AccountID: "account"}, ApplicationID: "app", CredentialID: "credential", Environment: contract.EnvironmentProduction, InferenceScopes: []contract.Scope{contract.ScopeInvoke}}}, Target: contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference}, Modality: contract.ModalityText, Input: contract.Input{Format: contract.InputDecisions, Decisions: &contract.DecisionInput{State: "synthetic", Questions: []contract.DecisionQuestion{{ID: "q", Kind: "noul", Question: "synthetic?"}}}}, Client: contract.ClientRequestMetadata{APIFormat: contract.APIFormatDecisions, Endpoint: "/v1/decisions", ReceivedAt: contract.NewTimestamp(at)}, RoutingPolicy: audience.Policy, IdempotencyKey: &idem, AuthorizedRoutes: []contract.AuthorizedRoute{{Substitution: contract.SubstitutionSameModel, DeploymentID: audience.DeploymentID, ModelReference: audience.ModelReference, Provider: audience.Provider, Regions: []contract.Region{"test-region"}}}, ScopedExecution: &contract.ScopedExecution{ScopedExecutionAudience: audience, RequestID: "request-1", SnapshotID: "snapshot", CatalogueEvidenceHash: strings.Repeat("b", 64)}}
	return e, request, adapter, claims
}
func TestScopedExecutorClaimsBeforeSendAndRefusesFreshRequestReplay(t *testing.T) {
	e, request, adapter, claims := scopedExecutorFixture(t)
	result := e.Execute(context.Background(), request, func(contract.StreamEvent) error { return nil })
	if result.Failure != nil || adapter.sends != 1 || claims.calls != 1 {
		t.Fatalf("positive failed: %+v sends=%d claims=%d", result.Failure, adapter.sends, claims.calls)
	}
	request.Attribution.RequestID = "request-2"
	request.ScopedExecution.RequestID = "request-2"
	replay := e.Execute(context.Background(), request, func(contract.StreamEvent) error { return nil })
	if replay.Failure == nil || adapter.sends != 1 || claims.calls != 2 {
		t.Fatalf("stable permit replay admitted: %+v sends=%d claims=%d", replay.Failure, adapter.sends, claims.calls)
	}
}
func TestScopedExecutorRefusesBeforeClaimWhenAuthorityEvidenceDiffers(t *testing.T) {
	cases := map[string]func(*Executor, *contract.Request, *scopedAdapterFixture, *scopedClaimFixture){
		"nil source": func(e *Executor, _ *contract.Request, _ *scopedAdapterFixture, _ *scopedClaimFixture) {
			e.scopedSource = nil
		},
		"no claim storage": func(e *Executor, _ *contract.Request, _ *scopedAdapterFixture, _ *scopedClaimFixture) {
			e.scopedClaims = nil
		},
		"missing invoke": func(_ *Executor, r *contract.Request, _ *scopedAdapterFixture, _ *scopedClaimFixture) {
			r.Attribution.Principal.InferenceScopes = nil
		},
		"principal mismatch": func(_ *Executor, r *contract.Request, _ *scopedAdapterFixture, _ *scopedClaimFixture) {
			r.Attribution.Principal.ApplicationID = "other"
		},
		"snapshot mismatch": func(_ *Executor, r *contract.Request, _ *scopedAdapterFixture, _ *scopedClaimFixture) {
			r.ScopedExecution.SnapshotID = "other"
		},
		"actual card mismatch": func(e *Executor, _ *contract.Request, _ *scopedAdapterFixture, _ *scopedClaimFixture) { e.costs = nil },
		"wrong existing key binding": func(e *Executor, _ *contract.Request, a *scopedAdapterFixture, _ *scopedClaimFixture) {
			if err := e.registry.ReplaceGeneration([]provider.CredentialBinding{{DeploymentID: "dep-private", Provider: "openrouter", KeyID: "other-key"}}, a); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, alter := range cases {
		t.Run(name, func(t *testing.T) {
			e, r, a, c := scopedExecutorFixture(t)
			alter(e, r, a, c)
			result := e.Execute(context.Background(), r, func(contract.StreamEvent) error { return nil })
			if result.Failure == nil || a.sends != 0 || c.calls != 0 {
				t.Fatalf("refusal failed: %+v sends=%d claims=%d", result.Failure, a.sends, c.calls)
			}
		})
	}
}
func TestScopedExecutorUnknownClaimAndUncertainSendNeverRetry(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		t.Run(map[bool]string{true: "unknown claim", false: "uncertain send"}[unknown], func(t *testing.T) {
			e, r, a, c := scopedExecutorFixture(t)
			c.fail = unknown
			if !unknown {
				a.failure = errors.New("synthetic uncertain exchange")
			}
			result := e.Execute(context.Background(), r, func(contract.StreamEvent) error { return nil })
			want := 1
			if unknown {
				want = 0
			}
			if result.Failure == nil || a.sends != want || c.calls != 1 {
				t.Fatalf("unexpected replay: %+v sends=%d claims=%d", result.Failure, a.sends, c.calls)
			}
		})
	}
}

func TestScopedExecutorUsesExactDeploymentObservationAcrossTwoCards(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		source  string
		allowed bool
	}{
		{"own card", "card", "source", true},
		{"foreign card version", "foreign-card", "source", false},
		{"own version with mismatched source", "card", "different-source", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, request, adapter, claims := scopedExecutorFixture(t)
			// Two genuinely distinct immutable documents, loaded by the production API.
			// The fixture clock remains unchanged; the added card cannot become evidence
			// for the approved private deployment.
			folder := t.TempDir()
			documents := []string{
				`{"schemaVersion":1,"rateCardVersionId":"card","source":"operator","sourceVersion":"source","observedAt":"2026-10-02T00:00:00Z","effectiveAt":"2026-10-02T00:00:00Z","rateCards":[{"deploymentId":"dep-private","currency":"USD","rates":[{"unit":"input_tokens","amountPerUnit":42000},{"unit":"output_tokens","amountPerUnit":0}]}]}`,
				`{"schemaVersion":1,"rateCardVersionId":"foreign-card","source":"provider_documentation","sourceVersion":"foreign-source","observedAt":"2026-09-30T00:00:00Z","effectiveAt":"2026-09-30T00:00:00Z","rateCards":[{"deploymentId":"foreign-deployment","currency":"USD","rates":[{"unit":"input_tokens","amountPerUnit":99999}]}]}`,
			}
			paths := []string{filepath.Join(folder, "own.json"), filepath.Join(folder, "foreign.json")}
			for i, document := range documents {
				if err := os.WriteFile(paths[i], []byte(document), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cards, err := providercost.Load(paths...)
			if err != nil {
				t.Fatal(err)
			}
			e.costs = cards
			// Match the signed/source/snapshot audience to isolate the actual-card check.
			audience := request.ScopedExecution.ScopedExecutionAudience
			audience.ProviderRateCardVersionID = tc.version
			audience.ProviderSourceVersion = tc.source
			request.ScopedExecution.ScopedExecutionAudience = audience
			e.scopedSource = func() *contract.ScopedExecutionAudience { return &audience }
			result := e.Execute(context.Background(), request, func(contract.StreamEvent) error { return nil })
			if tc.allowed {
				if result.Failure != nil || adapter.sends != 1 || claims.calls != 1 {
					t.Fatalf("two-card positive failed: %+v sends=%d claims=%d", result.Failure, adapter.sends, claims.calls)
				}
				if result.UpstreamCost.Attempts[0].RateCardVersionID != "card" {
					t.Fatal("cost cites foreign observation")
				}
			} else if result.Failure == nil || adapter.sends != 0 || claims.calls != 0 {
				t.Fatalf("foreign evidence accepted: %+v sends=%d claims=%d", result.Failure, adapter.sends, claims.calls)
			}
		})
	}
}

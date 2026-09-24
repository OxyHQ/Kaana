package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// durableRuntimeFake refuses exactly what the database functions refuse
// (0011_provider_credential_runtime.sql): a retirement that does not end after
// it began, and a recovery claim for no provider. A fake that accepted them
// is how a zero-length retirement shipped: every unit test passed, and the
// first 402 against PostgreSQL surfaced as provider_error.
type durableRuntimeFake struct {
	credentialRuntimeFake
	claimedFor []contract.ProviderSlug
}

func (r *durableRuntimeFake) ClaimCredentialRecovery(ctx context.Context, slug contract.ProviderSlug, keyID string, at, until time.Time) (CredentialRecoveryDecision, error) {
	if !slug.Valid() || keyID == "" || !until.After(at) {
		return "", errors.New("credential recovery lease is invalid")
	}
	r.claimedFor = append(r.claimedFor, slug)
	return r.credentialRuntimeFake.ClaimCredentialRecovery(ctx, slug, keyID, at, until)
}

func (r *durableRuntimeFake) RecordCredentialAttempt(ctx context.Context, attempt CredentialAttempt) error {
	retiring := attempt.Outcome == "rejected" || attempt.Outcome == "exhausted"
	if !attempt.Provider.Valid() || (retiring && !attempt.RetiredUntil.After(attempt.OccurredAt)) || (!retiring && !attempt.RetiredUntil.IsZero()) {
		return errors.New("provider credential attempt is invalid")
	}
	return r.credentialRuntimeFake.RecordCredentialAttempt(ctx, attempt)
}

// refusingSender answers every call with one provider refusal.
type refusingSender struct {
	status int
	code   contract.ErrorCode
	cat    contract.UpstreamErrorCategory
}

func (s refusingSender) Send(context.Context, *Call, Key) (*http.Response, error) {
	return &http.Response{StatusCode: s.status, Header: make(http.Header), Body: http.NoBody}, nil
}

func (s refusingSender) Refuse(response *http.Response, _ Key) error {
	_ = response.Body.Close()
	return upstreamFailure(s.code, s.cat)
}

func (refusingSender) TransportFailure(_ context.Context, err error) error { return err }

// TestABoundViewRetiresWithItsPoolsPolicy is the schema-0013 regression. A
// platform deployment executes on a Bind view — its exact binding, or its
// provider's only key — and the walk over that view recorded the retirement
// with the VIEW's policy. The view was built empty, so a billing refusal or a
// credential rejection recorded retired_until = occurred_at, the durable
// runtime refused it, and the customer got an unclassified provider_error
// instead of the provider's own verdict, with nothing recorded against a key
// that kept being tried.
func TestABoundViewRetiresWithItsPoolsPolicy(t *testing.T) {
	const retirement = 7 * time.Minute
	refusals := []struct {
		name    string
		sender  refusingSender
		outcome string
	}{
		{"billing refusal", refusingSender{http.StatusPaymentRequired, contract.CodeProviderBillingRefused, contract.UpstreamQuota}, "exhausted"},
		{"credential rejection", refusingSender{http.StatusUnauthorized, contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication}, "rejected"},
	}
	resolutions := []struct {
		name       string
		declared   []string
		bindings   func(slug contract.ProviderSlug) []CredentialBinding
		deployment contract.DeploymentID
		keyID      string
	}{
		{
			name:     "exact binding",
			declared: []string{"bound", "other"},
			bindings: func(slug contract.ProviderSlug) []CredentialBinding {
				return []CredentialBinding{{DeploymentID: "dep_bound", Provider: slug, KeyID: "bound"}}
			},
			deployment: "dep_bound", keyID: "bound",
		},
		{
			name:       "provider default",
			declared:   []string{"only"},
			bindings:   func(contract.ProviderSlug) []CredentialBinding { return nil },
			deployment: "dep_unbound", keyID: "only",
		},
	}
	for _, resolution := range resolutions {
		for _, refusal := range refusals {
			t.Run(resolution.name+"/"+refusal.name, func(t *testing.T) {
				const slug contract.ProviderSlug = "cerebras"
				runtime := &durableRuntimeFake{credentialRuntimeFake: credentialRuntimeFake{decision: CredentialRecoveryClaimed}}
				declarations := make([]KeyDeclaration, 0, len(resolution.declared))
				for index, keyID := range resolution.declared {
					declarations = append(declarations, KeyDeclaration{KeyID: keyID, Secret: fmt.Sprintf("%s-%d", firstCredential, index), Runtime: runtime})
				}
				pool, err := NewKeyPool(slug, declarations, KeyPolicy{Retirement: retirement, OnSeparateAccounts: true}, nil)
				if err != nil {
					t.Fatal(err)
				}
				adapter := credentialRegistryAdapter{registryAdapter: registryAdapter{slug: slug}, pool: pool}
				registry, err := NewRegistry(adapter)
				if err != nil {
					t.Fatal(err)
				}
				if err := registry.ReplaceGeneration(resolution.bindings(slug), adapter); err != nil {
					t.Fatal(err)
				}
				_, view, err := registry.ResolveExecution(resolution.deployment, slug, true)
				if err != nil {
					t.Fatal(err)
				}
				if !view.Begin().AllowThrottleRotation() {
					t.Error("the view lost the pool's separate-accounts declaration")
				}

				call := &Call{RequestID: "req_bound", Route: Route{Provider: slug, DeploymentID: resolution.deployment}}
				response, key, err := Walk(context.Background(), view, call, refusal.sender)
				closeRefused(t, response)
				var upstream ErrUpstream
				if !errors.As(err, &upstream) || upstream.Code != refusal.sender.code {
					t.Fatalf("walk = %v; want the provider's own %s, not an unclassified failure", err, refusal.sender.code)
				}
				if key.ID != resolution.keyID {
					t.Fatalf("walk used key %q, want %q", key.ID, resolution.keyID)
				}
				if len(runtime.attempts) != 1 {
					t.Fatalf("recorded attempts = %+v", runtime.attempts)
				}
				recorded := runtime.attempts[0]
				if recorded.KeyID != resolution.keyID || recorded.Outcome != refusal.outcome ||
					!recorded.RetiredUntil.Equal(recorded.OccurredAt.Add(retirement)) {
					t.Fatalf("recorded %+v; want %s on %q retired for the pool's %s", recorded, refusal.outcome, resolution.keyID, retirement)
				}

				// Once the retirement has passed, recovery is claimed for this
				// provider, not for the view's empty slug.
				pool.Retire(key, KeyRejected, time.Now().Add(-2*retirement), time.Now().Add(-time.Minute))
				runtime.attempts = nil
				response, _, err = Walk(context.Background(), view, call, refusal.sender)
				closeRefused(t, response)
				if !errors.As(err, &upstream) {
					t.Fatalf("recovery walk = %v", err)
				}
				if len(runtime.claimedFor) != 1 || runtime.claimedFor[0] != slug {
					t.Fatalf("recovery claimed for %v, want [%s]", runtime.claimedFor, slug)
				}
			})
		}
	}
}

// closeRefused fails a test whose refusal came back with a response to read.
func closeRefused(t *testing.T, response *http.Response) {
	t.Helper()
	if response != nil {
		_ = response.Body.Close()
		t.Fatal("a refused walk returned a response")
	}
}

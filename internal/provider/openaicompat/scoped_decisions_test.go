package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestScopedDecisionsRequireExactPrivateSourceAndOneClaim(t *testing.T) {
	request, route := decisionFixture("openrouter")
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		if r.URL.Path != "/api/v1/systemone" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+fakeAPIKey {
			t.Error("wrong scoped wire identity")
		}
		var body systemOneRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		encoded, err := json.Marshal(body.Provider.MaxPrice)
		if err != nil || string(encoded) != `{"prompt":0.042,"completion":0}` {
			t.Error("actual decimal ceiling lost", string(encoded), err)
		}
		if body.Provider.AllowFallbacks || !body.Provider.ZDR || body.Provider.DataCollection != "deny" || len(body.Provider.Only) != 1 || body.Provider.Only[0] != "TypeSafe" {
			t.Error("private policy widened")
		}
		_, _ = io.WriteString(w, decisionResponse(route.UpstreamModelID))
	}))
	defer server.Close()
	a, err := New(Config{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatal(err)
	}
	keyID, _ := a.PlatformCredentials().SoleKeyID()
	principal := request.Attribution.Principal
	request.RoutingPolicy = contract.RoutingPolicyReference{RoutingPolicyID: "fixture-policy", PolicyVersion: 1}
	idem := contract.IdempotencyKey("fixture-idempotency")
	request.IdempotencyKey = &idem
	audience := contract.ScopedExecutionAudience{PermitID: "fixture-permit", IdempotencyKey: idem, FixtureSHA256: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano), Principal: contract.ScopedExecutionPrincipal{AccountID: string(principal.Billing.AccountID), ApplicationID: string(principal.ApplicationID), CredentialID: string(principal.CredentialID), Environment: principal.Environment}, Policy: request.RoutingPolicy, DeploymentID: route.DeploymentID, Provider: route.Provider, KeyID: keyID, ModelReference: route.ModelReference, UpstreamModelID: route.UpstreamModelID, PriceVersionID: "fixture-price", ProviderRateCardVersionID: "fixture-card", ProviderSourceVersion: "fixture-source", MaxCostUSD: "0.01"}
	request.SchemaVersion = contract.ScopedRequestEnvelopeVersion
	request.ScopedExecution = &contract.ScopedExecution{ScopedExecutionAudience: audience, RequestID: request.Attribution.RequestID, SnapshotID: "fixture-snapshot", CatalogueEvidenceHash: strings.Repeat("b", 64)}
	route.ScopedExecution = &audience
	prompt, err := providercost.ParseDecisionTokenPrice("0.042")
	if err != nil {
		t.Fatal(err)
	}
	route.ScopedDecisionPriceLimit = &providercost.DecisionPriceLimit{Prompt: prompt}
	if _, err = a.Translate(request, route); err == nil {
		t.Fatal("nil production source approval accepted")
	}
	// Private package fixtures alone supply reviewed synthetic source and wire transport.
	a.scopedSource = func() *contract.ScopedExecutionAudience { return &audience }
	a.scopedHTTPClient = func() *http.Client { return identityBoundFakeClient(t, server.URL) }
	call, err := a.Translate(request, route)
	if err != nil {
		t.Fatal(err)
	}
	if a.decisions.approved() {
		t.Fatal("global decisions gates opened")
	}
	if _, err = a.Stream(context.Background(), call, nil, nil); err == nil {
		t.Fatal("missing claim accepted")
	}
	claimed := false
	call.ScopedAttempt = &provider.ScopedCredentialAttempt{KeyID: keyID, Claim: func(context.Context) error {
		if claimed {
			return errors.New("already consumed")
		}
		claimed = true
		return nil
	}}
	bound, err := a.PlatformCredentials().Bind(keyID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := a.Stream(context.Background(), call, nil, bound)
	if err != nil || len(outcome.Decisions) != 3 || !claimed || sends.Load() != 1 {
		t.Fatal("approved fixture did not execute once", err, sends.Load())
	}
	if _, err = a.Stream(context.Background(), call, nil, bound); err == nil || sends.Load() != 1 {
		t.Fatal("consumed permit repeated", err, sends.Load())
	}
	changed := audience
	changed.Principal.AccountID = "different"
	request.ScopedExecution.ScopedExecutionAudience = changed
	if _, err = a.Translate(request, route); err == nil {
		t.Fatal("wrong principal audience accepted")
	}
}

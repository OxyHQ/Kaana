package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

// RerankScenario names what a fake rerank upstream does with the one
// request the suite sends it (RerankRequest).
type RerankScenario string

const (
	// RerankAnswered scores every document of the request and reports
	// RerankSubject.InputTokens when it reports usage at all.
	RerankAnswered RerankScenario = "answered"
	// RerankEmpty answers 200 with an empty list of results.
	RerankEmpty RerankScenario = "empty"
	// RerankRateLimited refuses with a transient rate limit.
	RerankRateLimited RerankScenario = "rate_limited"
	// RerankServerError refuses with the provider's own 500.
	RerankServerError RerankScenario = "server_error"
	// RerankRequestRefused refuses the request itself (400) after accepting
	// the credential.
	RerankRequestRefused RerankScenario = "request_refused"
	// RerankHeld reads the request and answers nothing until its caller
	// goes away.
	RerankHeld RerankScenario = "held"
)

// RerankUpstream is a running fake rerank provider.
type RerankUpstream struct {
	URL string
	// Requests is how many HTTP requests it received.
	Requests func() int
	// Disconnected reports whether it saw its caller go away mid-request.
	Disconnected func() bool
	// Arrived is closed when the upstream has read a request.
	Arrived <-chan struct{}
}

// RerankSubject is what a rerank adapter author supplies.
type RerankSubject struct {
	Name            string
	Provider        contract.ProviderSlug
	ModelReference  contract.ModelReference
	UpstreamModelID string
	// Documents is how many documents RerankRequest sends (at least three,
	// so the cap of TopN is visible in the answer).
	Documents int
	// TopN is the cap RerankRequest asks for (at least 1, below Documents).
	TopN int
	// InputTokens is what the fake reports; zero for a provider that reports
	// no tokens for a rerank.
	InputTokens int
	// NewAdapter builds the adapter pointed at a fake upstream.
	NewAdapter func(t *testing.T, upstreamURL string) provider.Adapter
	// StartUpstream starts a fake speaking the provider's real wire. It
	// asserts the request it receives is the translation of RerankRequest.
	StartUpstream func(t *testing.T, scenario RerankScenario) *RerankUpstream
}

// RerankQuery is the query every subject is asked.
const RerankQuery = "which synthetic document is about lighthouses"

// RerankDocument is the text of the document at an index in RerankRequest.
func RerankDocument(index int) string { return fmt.Sprintf("synthetic document number %d", index) }

// RerankRequest is the request every subject is sent: the query, the
// documents and the cap the envelope carries.
func RerankRequest(subject RerankSubject) *contract.Request {
	reference := subject.ModelReference
	query, topN := RerankQuery, subject.TopN
	documents := make([]string, subject.Documents)
	for index := range documents {
		documents[index] = RerankDocument(index)
	}
	return &contract.Request{
		SchemaVersion: contract.RequestEnvelopeVersion,
		Attribution: contract.Attribution{
			Principal: contract.AuthenticatedPrincipal{
				Billing: contract.BillingPrincipal{AccountID: "acc_conformance"}, ApplicationID: "app_conformance",
				CredentialID: "cred_conformance", Environment: contract.EnvironmentDevelopment,
				InferenceScopes: []contract.Scope{contract.ScopeInvoke},
			},
			RequestID: "req_conformance_rerank",
		},
		Target:   contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference},
		Modality: contract.ModalityText,
		Input:    contract.Input{Format: contract.InputText, Text: &query},
		Rerank:   &contract.RerankParameters{Documents: documents, TopN: &topN},
		Client: contract.ClientRequestMetadata{
			APIFormat: contract.APIFormatRerank, Endpoint: "/v1/rerank",
			ReceivedAt: contract.NewTimestamp(time.Now()),
		},
		RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: "rp_conformance", PolicyVersion: 1},
		AuthorizedRoutes: []contract.AuthorizedRoute{{
			Substitution: contract.SubstitutionSameModel, DeploymentID: "dep_conformance",
			ModelReference: reference, Provider: subject.Provider, Regions: []contract.Region{"test-region"},
		}},
	}
}

// RunRerank holds a rerank adapter to what the platform depends on,
// driven through the real executor on one deployment: a complete answer with
// its ranking and units; an answer that scored no document, which is the deployment's
// failure and bills nothing; the provider's throttle, fault and refusal
// classified as such; and a cancellation that reaches the provider.
func RunRerank(t *testing.T, subject RerankSubject) {
	t.Helper()
	if subject.Documents < 3 || subject.TopN < 1 || subject.TopN >= subject.Documents || subject.NewAdapter == nil || subject.StartUpstream == nil || !subject.ModelReference.Pinned() {
		t.Fatal("a rerank subject needs at least three documents, a smaller top_n, an adapter, a fake upstream and a pinned model")
	}

	t.Run("serves the ranking with its units", func(t *testing.T) {
		r := executeRerank(t, subject, RerankAnswered, nil)
		if r.result.Failure != nil || r.result.Rerank == nil || r.result.Report == nil {
			t.Fatalf("an answered request failed: %+v", r.result.Failure)
		}
		ranking := r.result.Rerank.Results
		if len(ranking) != subject.TopN {
			t.Errorf("%d results served, top_n asked for %d", len(ranking), subject.TopN)
		}
		seen := map[int]bool{}
		for position, ranked := range ranking {
			if ranked.Index < 0 || ranked.Index >= subject.Documents || seen[ranked.Index] {
				t.Errorf("result %d indexes document %d of %d (or repeats one)", position, ranked.Index, subject.Documents)
			}
			seen[ranked.Index] = true
			if position > 0 && ranked.RelevanceScore > ranking[position-1].RelevanceScore {
				t.Errorf("result %d scores above the one before it: not most relevant first", position)
			}
		}
		report := r.result.Report
		if report.Outcome != contract.OutcomeCompleted {
			t.Errorf("report = %s from %s", report.Outcome, report.UsageSource)
		}
		if report.ResolvedModelReference != subject.ModelReference {
			t.Errorf("the report names %q, the deployment serves %q", report.ResolvedModelReference, subject.ModelReference)
		}
		units := map[contract.UsageUnit]int{}
		for _, unit := range report.Units {
			units[unit.Unit] = unit.Quantity
		}
		if units[contract.UnitRequests] != 1 || units[contract.UnitInputTokens] != subject.InputTokens || len(units) > 2 {
			t.Errorf("units = %v; want one request and %d input tokens", units, subject.InputTokens)
		}
		if r.result.Rerank.InputTokens != subject.InputTokens {
			t.Errorf("the result reports %d input tokens, the upstream said %d", r.result.Rerank.InputTokens, subject.InputTokens)
		}
		if len(r.result.UpstreamCost.Attempts) != 1 || !r.result.UpstreamCost.Attempts[0].Served {
			t.Errorf("cost attempts = %+v; want one served attempt", r.result.UpstreamCost.Attempts)
		}
		if r.upstream.Requests() != 1 {
			t.Errorf("one answer took %d upstream requests", r.upstream.Requests())
		}
		if len(r.events) != 0 {
			t.Errorf("a rerank answer is a whole response, yet %d stream events were emitted", len(r.events))
		}
	})

	t.Run("an answer that scored nothing is a failure that bills nothing", func(t *testing.T) {
		r := executeRerank(t, subject, RerankEmpty, nil)
		if r.result.Rerank != nil {
			t.Error("an empty answer was served")
		}
		failure := r.result.Failure
		if failure == nil || failure.Code != contract.CodeProviderError || !failure.Retryable {
			t.Fatalf("failure = %+v; an attempt that scored no document is a retryable provider error", failure)
		}
		assertRerankBilledNothing(t, r)
	})

	t.Run("a throttle is a rate limit", func(t *testing.T) {
		r := executeRerank(t, subject, RerankRateLimited, nil)
		if r.result.Failure == nil || r.result.Failure.Code != contract.CodeRateLimited || r.result.Rerank != nil {
			t.Fatalf("failure = %+v", r.result.Failure)
		}
		assertRerankBilledNothing(t, r)
	})

	t.Run("a provider fault is a provider error", func(t *testing.T) {
		r := executeRerank(t, subject, RerankServerError, nil)
		if r.result.Failure == nil || r.result.Failure.Code != contract.CodeProviderError || r.result.Rerank != nil {
			t.Fatalf("failure = %+v", r.result.Failure)
		}
		assertRerankBilledNothing(t, r)
	})

	t.Run("a refused request is the caller's, not retryable", func(t *testing.T) {
		r := executeRerank(t, subject, RerankRequestRefused, nil)
		if r.result.Failure == nil || r.result.Failure.Retryable || r.result.Failure.Code == contract.CodeProviderError || r.result.Rerank != nil {
			t.Fatalf("failure = %+v", r.result.Failure)
		}
		assertRerankBilledNothing(t, r)
		if r.upstream.Requests() != 1 {
			t.Errorf("a refusal every key would repeat took %d upstream requests", r.upstream.Requests())
		}
	})

	t.Run("cancelling reaches the provider and ends as a cancellation", func(t *testing.T) {
		r := executeRerank(t, subject, RerankHeld, func(upstream *RerankUpstream, cancel context.CancelFunc) {
			select {
			case <-upstream.Arrived:
				cancel()
			case <-time.After(10 * time.Second):
				cancel()
			}
		})
		if r.result.Failure == nil || r.result.Failure.Code != contract.CodeCancelled || r.result.Rerank != nil {
			t.Fatalf("failure = %+v", r.result.Failure)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !r.upstream.Disconnected() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !r.upstream.Disconnected() {
			t.Error("the provider never saw its caller go away")
		}
	})
}

func assertRerankBilledNothing(t *testing.T, r *rerankRun) {
	t.Helper()
	report := r.result.Report
	if report == nil || report.Outcome != contract.OutcomeFailed || len(report.Units) != 0 {
		t.Errorf("report = %+v; a failed rerank attempt reports no units", report)
	}
	for _, attempt := range r.result.UpstreamCost.Attempts {
		if attempt.Served {
			t.Errorf("attempt %d is marked served", attempt.AttemptIndex)
		}
	}
}

type rerankRun struct {
	result   kaana.Result
	events   []contract.StreamEvent
	upstream *RerankUpstream
	cost     providercost.Record
}

func executeRerank(t *testing.T, subject RerankSubject, scenario RerankScenario, act func(*RerankUpstream, context.CancelFunc)) *rerankRun {
	t.Helper()
	upstream := subject.StartUpstream(t, scenario)
	adapter := subject.NewAdapter(t, upstream.URL)
	inventoryJSON := fmt.Sprintf(`{"snapshotId":"snap_conformance","issuedAt":%q,"deployments":[{"deploymentId":"dep_conformance","provider":%q,"modelReference":%q,"upstreamModelId":%q,"regions":["test-region"],"current":true}]}`,
		contract.NewTimestamp(time.Now()), subject.Provider, subject.ModelReference, subject.UpstreamModelID)
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(path, []byte(inventoryJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := inventory.NewStore(inventory.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := kaana.NewExecutor(kaana.Config{
		Inventory: store, Providers: registry, Rotation: rotation.NewRegistry(rotation.Policy{}, nil),
		Retry: &kaana.RetryPolicy{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &rerankRun{upstream: upstream}
	if act != nil {
		go act(upstream, cancel)
	}
	run.result = executor.Execute(ctx, RerankRequest(subject), func(event contract.StreamEvent) error {
		run.events = append(run.events, event)
		return nil
	})
	run.cost = run.result.UpstreamCost
	return run
}

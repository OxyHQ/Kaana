package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

// DecisionsScenario names what a fake decisions upstream does with the one
// request the suite sends it (DecisionsRequest).
type DecisionsScenario string

const (
	// DecisionsAnswered answers every question with DecisionsAnswers, in the
	// provider's own order and spelling, and reports DecisionsSubject's
	// InputTokens.
	DecisionsAnswered DecisionsScenario = "answered"
	// DecisionsRefused declines to answer, the provider's refusal, and still
	// reports InputTokens.
	DecisionsRefused DecisionsScenario = "refused"
	// DecisionsMalformed answers with a body that parses, reports InputTokens
	// and carries an answer the request did not ask for (a probability above
	// one).
	DecisionsMalformed DecisionsScenario = "malformed"
	// DecisionsCut accepts the request and closes the connection before any
	// body.
	DecisionsCut DecisionsScenario = "cut"
	// DecisionsFirstKeyRejected refuses the first key with the provider's own
	// authentication error (401) and answers the second.
	DecisionsFirstKeyRejected DecisionsScenario = "first_key_rejected"
	// DecisionsFirstKeyUnpaid refuses the first key with the provider's own
	// billing error (402) and answers the second.
	DecisionsFirstKeyUnpaid DecisionsScenario = "first_key_unpaid"
	// DecisionsHeld reads the request and answers nothing until its caller
	// goes away.
	DecisionsHeld DecisionsScenario = "held"
)

// DecisionsUpstream is a running fake decisions provider.
type DecisionsUpstream struct {
	URL string
	// Requests is how many HTTP requests it received.
	Requests func() int
	// Disconnected reports whether it saw its caller go away mid-request.
	Disconnected func() bool
}

// DecisionsSubject is what a decisions adapter author supplies.
type DecisionsSubject struct {
	Name            string
	Provider        contract.ProviderSlug
	ModelReference  contract.ModelReference
	UpstreamModelID string
	// APIKeys are the two credentials NewAdapter configures, in order.
	APIKeys []string
	// InputTokens is what the fake reports having read.
	InputTokens int
	// RefusalInputTokens is what the fake's refusal reports having read;
	// zero for a provider whose refusal is unbilled and measures nothing.
	RefusalInputTokens int
	// NewAdapter builds the adapter pointed at a fake upstream.
	NewAdapter func(t *testing.T, upstreamURL string) provider.Adapter
	// StartUpstream starts a fake speaking the provider's real wire. It
	// asserts the request it receives is DecisionsRequest's translation.
	StartUpstream func(t *testing.T, scenario DecisionsScenario) *DecisionsUpstream
}

// DecisionsInput is the input every subject is asked: one choice, one score
// and one noul (a yes/no proposition).
func DecisionsInput() contract.DecisionInput {
	instructions := "Judge the post."
	return contract.DecisionInput{
		State:        "A synthetic post about a cat.",
		Instructions: &instructions,
		Questions: []contract.DecisionQuestion{
			{ID: "topic", Kind: "choice", Question: "What is it about?", Options: []string{"pets", "cars"}},
			{ID: "quality", Kind: "score", Question: "How good is it?", Levels: []string{"low", "mid", "high"}},
			{ID: "spam", Kind: "noul", Question: "Is it spam?"},
		},
	}
}

// DecisionsAnswers is what an answered fake must mean, in contract order: a
// fake states it in its provider's own order and spelling, and the suite
// requires it back exactly like this.
var DecisionsAnswers = struct {
	Topic, TopicConfidence float64 // probability of "pets", and its confidence
	Quality                []float64
	QualityConfidence      float64
	Spam                   float64
}{Topic: 0.8, TopicConfidence: 0.9, Quality: []float64{0.1, 0.2, 0.7}, QualityConfidence: 0.6, Spam: 0.1}

// RunDecisions holds a decisions adapter to what the platform depends on,
// driven through the real executor on one priced deployment: a complete
// answer in contract order; a refusal and an invalid answer that keep what
// the provider measured; a cut connection that measures nothing; a key walk
// that re-sends only after a definitive 401 or 402; a cancellation that
// reaches the provider; and a route without a price that is never called.
// Every exchange is sent at most once per key and never moves to another
// route.
func RunDecisions(t *testing.T, subject DecisionsSubject) {
	t.Helper()
	if len(subject.APIKeys) < 2 || subject.NewAdapter == nil || subject.StartUpstream == nil || subject.InputTokens <= 0 || !subject.ModelReference.Pinned() {
		t.Fatal("a decisions subject needs two keys, an adapter, a fake upstream, measured input tokens and a pinned model")
	}

	t.Run("answers every question in contract order", func(t *testing.T) {
		r := executeDecisions(t, subject, DecisionsAnswered, true, nil)
		if r.result.Failure != nil || r.result.Decisions == nil {
			t.Fatalf("an answered request failed: %+v", r.result.Failure)
		}
		if r.result.Decisions.Model != subject.ModelReference {
			t.Errorf("the decisions name %q, the deployment serves %q", r.result.Decisions.Model, subject.ModelReference)
		}
		assertDecisionAnswers(t, r.result.Decisions.Data)
		assertDecisionsReport(t, r, contract.OutcomeCompleted, subject.InputTokens)
		if r.upstream.Requests() != 1 {
			t.Errorf("one answer took %d upstream requests", r.upstream.Requests())
		}
	})

	t.Run("a refusal is a content filter that keeps what the provider measured", func(t *testing.T) {
		r := executeDecisions(t, subject, DecisionsRefused, true, nil)
		assertDecisionsFailure(t, r, contract.CodeUpstreamContentFiltered)
		if subject.RefusalInputTokens > 0 {
			assertDecisionsReport(t, r, contract.OutcomeFailed, subject.RefusalInputTokens)
		} else if r.result.Report == nil || len(r.result.Report.Units) != 0 {
			t.Errorf("an unbilled refusal was measured: %+v", r.result.Report)
		}
	})

	t.Run("an invalid answer is refused and keeps what was measured", func(t *testing.T) {
		r := executeDecisions(t, subject, DecisionsMalformed, true, nil)
		assertDecisionsFailure(t, r, contract.CodeProviderError)
		assertDecisionsReport(t, r, contract.OutcomeFailed, subject.InputTokens)
	})

	t.Run("a cut connection measures nothing and is never re-sent", func(t *testing.T) {
		r := executeDecisions(t, subject, DecisionsCut, true, nil)
		if r.result.Failure == nil || r.result.Decisions != nil {
			t.Fatal("a cut connection produced decisions")
		}
		if r.upstream.Requests() != 1 {
			t.Errorf("a cut exchange may have been accepted and billed, yet it was sent %d times", r.upstream.Requests())
		}
		if r.result.Report == nil || len(r.result.Report.Units) != 0 {
			t.Errorf("a cut connection was measured: %+v", r.result.Report)
		}
	})

	for _, scenario := range []DecisionsScenario{DecisionsFirstKeyRejected, DecisionsFirstKeyUnpaid} {
		t.Run("a definitive refusal of the first key is answered by the next: "+string(scenario), func(t *testing.T) {
			r := executeDecisions(t, subject, scenario, true, nil)
			if r.result.Failure != nil || r.result.Decisions == nil {
				t.Fatalf("the second key did not answer: %+v", r.result.Failure)
			}
			assertDecisionAnswers(t, r.result.Decisions.Data)
			if r.upstream.Requests() != 2 {
				t.Errorf("a refused key and an answering one took %d requests", r.upstream.Requests())
			}
			assertNoKey(t, subject, r.result)
		})
	}

	t.Run("a cancellation reaches the provider", func(t *testing.T) {
		r := executeDecisions(t, subject, DecisionsHeld, true, func(upstream *DecisionsUpstream, cancel context.CancelFunc) {
			deadline := time.Now().Add(3 * time.Second)
			for upstream.Requests() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		})
		assertDecisionsFailure(t, r, contract.CodeCancelled)
		deadline := time.Now().Add(3 * time.Second)
		for !r.upstream.Disconnected() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !r.upstream.Disconnected() {
			t.Error("the caller went away and the provider never saw it")
		}
	})

	t.Run("a route without a price is never called", func(t *testing.T) {
		r := executeDecisions(t, subject, DecisionsAnswered, false, nil)
		assertDecisionsFailure(t, r, contract.CodeNoRouteAvailable)
		if r.upstream.Requests() != 0 {
			t.Errorf("an unpriced decisions route was sent %d requests", r.upstream.Requests())
		}
	})
}

type decisionsRun struct {
	result   kaana.Result
	upstream *DecisionsUpstream
}

// executeDecisions runs DecisionsInput through the real executor on one
// deployment, priced (input tokens only) when priced is set. act, when given,
// runs beside the request with its cancel function.
func executeDecisions(t *testing.T, subject DecisionsSubject, scenario DecisionsScenario, priced bool, act func(*DecisionsUpstream, context.CancelFunc)) decisionsRun {
	t.Helper()
	upstream := subject.StartUpstream(t, scenario)
	registry, err := provider.NewRegistry(subject.NewAdapter(t, upstream.URL))
	if err != nil {
		t.Fatalf("registering the adapter: %v", err)
	}
	const deployment = contract.DeploymentID("dep_decisions_conformance")
	path := filepath.Join(t.TempDir(), "inventory.json")
	document := fmt.Sprintf(`{"snapshotId":"snap_decisions","issuedAt":%q,"deployments":[{"deploymentId":%q,"provider":%q,"modelReference":%q,"upstreamModelId":%q,"regions":[],"current":true}]}`,
		contract.NewTimestamp(time.Now()), deployment, subject.Provider, subject.ModelReference, subject.UpstreamModelID)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := inventory.NewStore(inventory.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	var cards *providercost.Cards
	if priced {
		cards, err = providercost.Parse([]byte(fmt.Sprintf(`{"schemaVersion":1,"rateCardVersionId":"rc_decisions_conformance","source":"provider_api","sourceVersion":"conformance","observedAt":"2026-10-01T00:00:00Z","effectiveAt":"2026-10-01T00:00:00Z","rateCards":[{"deploymentId":%q,"currency":"USD","rates":[{"unit":"input_tokens","amountPerUnit":100000},{"unit":"output_tokens","amountPerUnit":0}]}]}`, deployment)))
		if err != nil {
			t.Fatal(err)
		}
	}
	executor, err := kaana.NewExecutor(kaana.Config{Inventory: store, Providers: registry, Rotation: rotation.NewRegistry(rotation.Policy{}, nil), Costs: cards, Retry: &kaana.RetryPolicy{}})
	if err != nil {
		t.Fatal(err)
	}

	input := DecisionsInput()
	reference := subject.ModelReference
	requestID := contract.RequestID("req_decisions_conformance")
	request := &contract.Request{
		SchemaVersion: contract.RequestEnvelopeVersion,
		Attribution: contract.Attribution{
			RequestID: requestID,
			Principal: contract.AuthenticatedPrincipal{
				Billing: contract.BillingPrincipal{AccountID: "acc_conformance"}, ApplicationID: "app_conformance",
				CredentialID: "cred_conformance", Environment: contract.EnvironmentDevelopment, InferenceScopes: []contract.Scope{contract.ScopeInvoke},
			},
		},
		Target:   contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference},
		Modality: contract.ModalityText,
		Input:    contract.Input{Format: contract.InputDecisions, Decisions: &input},
		Client: contract.ClientRequestMetadata{
			APIFormat: contract.APIFormatDecisions, Endpoint: "/v1/decisions", ReceivedAt: contract.NewTimestamp(time.Now()),
		},
		RoutingPolicy:    contract.RoutingPolicyReference{RoutingPolicyID: "rp_conformance", PolicyVersion: 1},
		AuthorizedRoutes: []contract.AuthorizedRoute{{Substitution: contract.SubstitutionSameModel, DeploymentID: deployment, ModelReference: reference, Provider: subject.Provider, Regions: []contract.Region{}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if act != nil {
		go act(upstream, cancel)
	}
	return decisionsRun{result: executor.Execute(ctx, request, func(contract.StreamEvent) error { return nil }), upstream: upstream}
}

func assertDecisionAnswers(t *testing.T, answers []contract.DecisionAnswer) {
	t.Helper()
	want := DecisionsAnswers
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }
	if len(answers) != 3 || answers[0].ID != "topic" || answers[1].ID != "quality" || answers[2].ID != "spam" {
		t.Fatalf("answers are not the questions in contract order: %+v", answers)
	}
	topic, quality, spam := answers[0], answers[1], answers[2]
	if topic.Reply == nil || topic.Reply.Label == nil || *topic.Reply.Label != "pets" || topic.Confidence == nil || !near(*topic.Confidence, want.TopicConfidence) ||
		len(topic.Probabilities) != 2 || !near(topic.Probabilities[0], want.Topic) || !near(topic.Probabilities[1], 1-want.Topic) {
		t.Errorf("the choice is not the provider's, in option order: %+v", topic)
	}
	mean := 0.0
	for i, p := range want.Quality {
		mean += float64(i) * p
	}
	if len(quality.Distribution) != 3 || quality.Mean == nil || !near(*quality.Mean, mean) || quality.Confidence == nil || !near(*quality.Confidence, want.QualityConfidence) {
		t.Errorf("the score is not the provider's, in level order: %+v", quality)
	} else {
		for i, p := range want.Quality {
			if !near(quality.Distribution[i], p) {
				t.Errorf("level %d has probability %v, want %v", i, quality.Distribution[i], p)
			}
		}
	}
	if spam.Probability == nil || !near(*spam.Probability, want.Spam) {
		t.Errorf("the noul is not the provider's: %+v", spam)
	}
}

func assertDecisionsFailure(t *testing.T, r decisionsRun, code contract.ErrorCode) {
	t.Helper()
	if r.result.Failure == nil || r.result.Decisions != nil {
		t.Fatalf("expected a %s failure, got decisions %+v", code, r.result.Decisions)
	}
	if r.result.Failure.Code != code {
		t.Errorf("failed as %q, want %q (%s)", r.result.Failure.Code, code, r.result.Failure.Message)
	}
	if code != contract.CodeNoRouteAvailable && r.upstream.Requests() > 1 {
		t.Errorf("a decisions exchange that may have been billed was sent %d times", r.upstream.Requests())
	}
}

// assertDecisionsReport requires the settlement to state what the provider
// measured, its input tokens and the one request, under a completed outcome
// exactly when the request completed (a measured failure settles partial).
func assertDecisionsReport(t *testing.T, r decisionsRun, outcome contract.RequestOutcome, inputTokens int) {
	t.Helper()
	report := r.result.Report
	if report == nil {
		t.Fatal("an exchange that reached the provider produced no settlement report")
	}
	settled := report.Outcome == contract.OutcomeCompleted
	if settled != (outcome == contract.OutcomeCompleted) || report.UsageSource != contract.UsageProviderReported {
		t.Errorf("settled as %q from %q, want %q from the provider", report.Outcome, report.UsageSource, outcome)
	}
	measured := map[contract.UsageUnit]int{}
	for _, unit := range report.Units {
		measured[unit.Unit] = unit.Quantity
	}
	if measured[contract.UnitInputTokens] != inputTokens || measured[contract.UnitRequests] != 1 {
		t.Errorf("the provider measured %d input tokens and one request; the report says %v", inputTokens, report.Units)
	}
	if len(r.result.UpstreamCost.Attempts) != 1 {
		t.Errorf("one exchange recorded %d cost attempts", len(r.result.UpstreamCost.Attempts))
	}
}

func assertNoKey(t *testing.T, subject DecisionsSubject, result kaana.Result) {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Decisions *contract.DecisionResult
		Failure   *contract.Error
	}{result.Decisions, result.Failure})
	if err != nil {
		t.Fatal(err)
	}
	for position, key := range subject.APIKeys {
		if strings.Contains(string(encoded), key) {
			t.Fatalf("the credential at position %d reached the caller", position+1)
		}
	}
}

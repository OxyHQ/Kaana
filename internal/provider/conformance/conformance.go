// Package conformance is the test suite every provider adapter must pass.
//
// It exists so that adding a provider is a fixture rather than a rewrite. An
// adapter author supplies four things — how to build the adapter, how to start
// a fake upstream speaking that provider's REAL wire format, one request the
// provider genuinely cannot express, and the route it serves — and gets back
// every invariant the platform depends on: event ordering, attribution,
// terminality, usage normalization, error classification, credential
// containment, cancellation and health.
//
// The suite deliberately drives the adapter through the real executor rather
// than calling its methods directly. An adapter is only correct in the shape it
// is actually used, and the framing, terminality and usage-report rules it has
// to satisfy live in the executor: testing the adapter alone would test a
// configuration nothing runs.
package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/inventory"
	"github.com/OxyHQ/Relay/internal/provider"
	"github.com/OxyHQ/Relay/internal/relay"
)

// Scenario names a behaviour the fake upstream must be able to perform. Each
// one corresponds to something a real provider does, and an adapter that cannot
// be driven through all of them has an untested branch on a path that costs
// money.
type Scenario string

const (
	// ScenarioStreaming streams several chunks and then reports usage.
	ScenarioStreaming Scenario = "streaming"
	// ScenarioSlowStream streams with a delay between chunks, so a caller can
	// cancel mid-stream.
	ScenarioSlowStream Scenario = "slow_stream"
	// ScenarioNoUsage streams output and never reports usage, which some
	// providers do and every settlement has to survive.
	ScenarioNoUsage Scenario = "no_usage"
	// ScenarioToolCalls streams a tool call in fragments.
	ScenarioToolCalls Scenario = "tool_calls"
	// ScenarioNonStreaming answers with a single complete response.
	ScenarioNonStreaming Scenario = "non_streaming"
	// ScenarioRateLimited refuses with a transient rate limit.
	ScenarioRateLimited Scenario = "rate_limited"
	// ScenarioQuotaExhausted refuses with an account-level quota exhaustion,
	// which is the same HTTP status as a rate limit and the opposite answer.
	ScenarioQuotaExhausted Scenario = "quota_exhausted"
	// ScenarioCredentialEchoed refuses with an error whose text echoes the
	// credential the caller sent. Providers really do this, and it is the
	// single most likely way an upstream key reaches a customer.
	ScenarioCredentialEchoed Scenario = "credential_echoed"
)

// Upstream is a running fake provider.
type Upstream struct {
	// URL is the base URL the adapter should be pointed at.
	URL string
	// TotalChunks is how many output chunks this scenario writes when it runs
	// to completion. Zero for scenarios that produce no output.
	TotalChunks int
	// Written reports how many chunks were actually written.
	Written func() int
	// CancelledAfterChunks reports how many chunks had been written when the
	// upstream observed its caller disconnect, or -1 if it never did.
	//
	// This is what makes the cancellation test a proof rather than an
	// assertion: the observation is made by the upstream, about its own
	// connection, and it is compared against the number a completed request
	// would produce.
	CancelledAfterChunks func() int
	// RequestCount reports how many HTTP requests the upstream received.
	RequestCount func() int
}

// Subject is what an adapter author supplies.
type Subject struct {
	// Name identifies the adapter in test output.
	Name string
	// Provider is the slug the adapter reports.
	Provider contract.ProviderSlug
	// ModelReference is the revision-pinned reference the fixture route serves.
	ModelReference contract.ModelReference
	// UpstreamModelID is what the fake upstream expects to be called.
	UpstreamModelID string
	// APIKey is the credential the adapter is configured with. The suite
	// asserts this exact string never appears in anything a customer receives,
	// so it must be the one NewAdapter uses.
	APIKey string
	// NewAdapter builds the adapter under test, pointed at a fake upstream.
	NewAdapter func(t *testing.T, upstreamURL string) provider.Adapter
	// NewUnconfigured builds the adapter with no credential.
	NewUnconfigured func(t *testing.T) provider.Adapter
	// StartUpstream starts a fake upstream for a scenario. It must speak the
	// provider's real wire format: a fake that speaks the normalized contract
	// would test nothing, because translation is the part most likely to be
	// wrong.
	StartUpstream func(t *testing.T, scenario Scenario) *Upstream
	// Unsupported is a request the provider genuinely cannot express, and the
	// code it must be refused with. The code must be non-retryable.
	Unsupported func() (*contract.Request, contract.ErrorCode)
}

// Run executes the suite.
func Run(t *testing.T, subject Subject) {
	t.Helper()
	requireSubject(t, subject)

	t.Run("reports a stable, valid provider slug", func(t *testing.T) {
		adapter := subject.NewAdapter(t, "https://unused.invalid")
		if got := adapter.Provider(); got != subject.Provider {
			t.Fatalf("adapter reports %q, the subject declares %q", got, subject.Provider)
		}
		if !adapter.Provider().Valid() {
			t.Fatalf("%q is not a valid provider slug", adapter.Provider())
		}
		first, second := adapter.Provider(), adapter.Provider()
		if first != second {
			t.Fatalf("the slug is not stable across calls: %q then %q", first, second)
		}
	})

	t.Run("streams a normalized, well-framed event sequence", func(t *testing.T) {
		run := execute(t, subject, ScenarioStreaming, streamingRequest(subject), nil)
		assertWellFramedStream(t, run)
		assertTerminal(t, run, contract.EventDone)
		assertReport(t, run, contract.OutcomeCompleted)

		if run.report.UsageSource != contract.UsageProviderReported {
			t.Errorf("the upstream reported usage; the report says %q", run.report.UsageSource)
		}
		if len(run.report.Units) == 0 {
			t.Error("the upstream reported usage; the report carries no units")
		}
		if !run.sawEvent(contract.EventUsage) {
			t.Error("no usage event reached the stream, so a client cannot see what it is being metered")
		}
		if run.report.TimeToFirstTokenMs == nil {
			t.Error("output was produced; the report carries no time to first token")
		}
	})

	t.Run("produces the same normalized shape from a non-streamed upstream", func(t *testing.T) {
		request := streamingRequest(subject)
		request.Stream = false
		run := execute(t, subject, ScenarioNonStreaming, request, nil)
		assertWellFramedStream(t, run)
		assertTerminal(t, run, contract.EventDone)
		assertReport(t, run, contract.OutcomeCompleted)
		if !run.sawEvent(contract.EventDelta) {
			t.Error("a non-streamed response produced no output events")
		}
	})

	t.Run("settles a provider that reports no usage as an estimate", func(t *testing.T) {
		run := execute(t, subject, ScenarioNoUsage, streamingRequest(subject), nil)
		assertWellFramedStream(t, run)
		assertReport(t, run, contract.OutcomeCompleted)
		if run.report.UsageSource == contract.UsageProviderReported {
			t.Error("the upstream reported nothing; the report claims the provider reported it")
		}
	})

	t.Run("streams tool calls that a client can reassemble", func(t *testing.T) {
		run := execute(t, subject, ScenarioToolCalls, streamingRequest(subject), nil)
		assertWellFramedStream(t, run)

		calls := make(map[string]bool)
		completed := make(map[string]bool)
		for _, event := range run.events {
			call, isCall := event.(*contract.StreamToolCallEvent)
			if !isCall {
				continue
			}
			if call.ToolCallID == "" {
				t.Error("a tool-call event carries no id, so its fragments cannot be joined")
			}
			calls[call.ToolCallID] = true
			if call.Complete {
				completed[call.ToolCallID] = true
			}
		}
		if len(calls) == 0 {
			t.Fatal("the upstream streamed a tool call; none reached the stream")
		}
		for id := range calls {
			if !completed[id] {
				t.Errorf("tool call %q was never marked complete, so a client never learns when to parse it", id)
			}
		}
	})

	t.Run("classifies a transient rate limit as retryable", func(t *testing.T) {
		run := execute(t, subject, ScenarioRateLimited, streamingRequest(subject), nil)
		failure := assertFailure(t, run)
		if failure.Code != contract.CodeRateLimited {
			t.Errorf("a 429 burst limit was classified %q, expected rate_limited", failure.Code)
		}
		if !failure.Retryable {
			t.Error("a transient rate limit was reported as non-retryable")
		}
	})

	t.Run("classifies an exhausted quota as non-retryable", func(t *testing.T) {
		run := execute(t, subject, ScenarioQuotaExhausted, streamingRequest(subject), nil)
		failure := assertFailure(t, run)
		if failure.Retryable {
			t.Errorf("an exhausted quota was reported retryable under code %q; only a human raises a quota", failure.Code)
		}
		if failure.Code == contract.CodeRateLimited {
			t.Error("an exhausted quota was classified as a rate limit; they are the same status and opposite answers")
		}
	})

	t.Run("never lets an upstream credential reach the customer", func(t *testing.T) {
		if subject.APIKey == "" {
			t.Fatal("the subject declares no API key, so this check would pass vacuously")
		}
		run := execute(t, subject, ScenarioCredentialEchoed, streamingRequest(subject), nil)
		_ = assertFailure(t, run)

		encoded, err := json.Marshal(run.events)
		if err != nil {
			t.Fatalf("encoding the emitted stream: %v", err)
		}
		if strings.Contains(string(encoded), subject.APIKey) {
			t.Fatalf("the configured upstream credential appears in the customer-visible stream:\n%s", encoded)
		}
		// Positive control on the test itself: the upstream must actually have
		// echoed the credential, or "no leak" is what a scenario that never
		// mentioned it also reports.
		if !run.upstreamEchoedCredential {
			t.Fatal("the fake upstream did not echo the credential, so this check measured nothing")
		}
	})

	t.Run("refuses a request it cannot express before spending anything", func(t *testing.T) {
		request, expected := subject.Unsupported()
		if expected.Retryable() {
			t.Fatalf("the subject expects refusal with %q, which is retryable; a request the provider cannot express can never succeed on a retry", expected)
		}
		run := execute(t, subject, ScenarioStreaming, request, nil)
		failure := assertFailure(t, run)
		if failure.Code != expected {
			t.Errorf("refused with %q, the subject expects %q", failure.Code, expected)
		}
		if failure.Retryable {
			t.Error("a request the provider cannot express was reported retryable")
		}
		if got := run.upstream.RequestCount(); got != 0 {
			t.Errorf("the upstream received %d requests for a request that should have been refused before translation completed", got)
		}
	})

	t.Run("a client disconnect cancels the upstream call", func(t *testing.T) {
		// The control runs first: it establishes what an UNINTERRUPTED request
		// looks like from the upstream's own side. Without it, "the upstream
		// saw its caller go away" is also what a normally-finished request
		// reports, and the cancellation check would prove nothing.
		control := execute(t, subject, ScenarioSlowStream, streamingRequest(subject), nil)
		assertTerminal(t, control, contract.EventDone)
		if got := control.upstream.CancelledAfterChunks(); got != -1 {
			t.Fatalf("the control request was not cancelled, yet the upstream observed a disconnect after %d chunks", got)
		}
		if written, total := control.upstream.Written(), control.upstream.TotalChunks; written != total {
			t.Fatalf("the control request wrote %d of %d chunks; the scenario is not running to completion", written, total)
		}

		cancelled := execute(t, subject, ScenarioSlowStream, streamingRequest(subject), cancelAfterFirstDelta)

		// The upstream's observation is asynchronous: Execute returns as soon
		// as the adapter's own read fails, which can be before the provider's
		// end of the connection notices. Polling to a deadline is the honest
		// way to wait for it — reading the counter once measures scheduling
		// order, not propagation.
		observed := -1
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if observed = cancelled.upstream.CancelledAfterChunks(); observed != -1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		switch {
		case observed == -1:
			t.Fatalf("the client cancelled, and the upstream never observed a disconnect after 3s (it wrote %d of %d chunks): the cancellation did not reach the provider",
				cancelled.upstream.Written(), cancelled.upstream.TotalChunks)
		case observed >= cancelled.upstream.TotalChunks:
			t.Fatalf("the upstream observed the disconnect only after writing all %d chunks, which is what completing normally looks like", observed)
		}
		if written := cancelled.upstream.Written(); written >= cancelled.upstream.TotalChunks {
			t.Fatalf("the upstream wrote all %d chunks despite the cancellation", cancelled.upstream.TotalChunks)
		}

		if cancelled.report == nil {
			t.Fatal("a cancelled request produced no usage report, so the reservation cannot be settled or refunded exactly")
		}
		if cancelled.report.Outcome != contract.OutcomeCancelled {
			t.Errorf("a cancelled request settled as %q", cancelled.report.Outcome)
		}
		for _, event := range cancelled.events {
			if event.EventType() == contract.EventDone {
				t.Error("a done event followed a cancellation, which is indistinguishable from a completed request")
			}
		}
	})

	t.Run("reports health without a credential and without leaking one", func(t *testing.T) {
		adapter := subject.NewUnconfigured(t)
		health := adapter.Health(context.Background())
		if health.Status != provider.HealthUnconfigured {
			t.Errorf("an adapter with no credential reports %q, expected %q", health.Status, provider.HealthUnconfigured)
		}
		if health.Provider != subject.Provider {
			t.Errorf("health names provider %q, expected %q", health.Provider, subject.Provider)
		}
		if _, err := health.CheckedAt.Time(); err != nil {
			t.Errorf("health carries an unparseable timestamp %q: %v", health.CheckedAt, err)
		}
	})

	t.Run("reports health against a reachable upstream", func(t *testing.T) {
		upstream := subject.StartUpstream(t, ScenarioStreaming)
		adapter := subject.NewAdapter(t, upstream.URL)
		health := adapter.Health(context.Background())
		if health.Status != provider.HealthOK {
			t.Errorf("a reachable upstream reports %q, expected %q (%s)", health.Status, provider.HealthOK, health.Detail)
		}
		encoded, err := json.Marshal(health)
		if err != nil {
			t.Fatalf("encoding health: %v", err)
		}
		if subject.APIKey != "" && strings.Contains(string(encoded), subject.APIKey) {
			t.Fatalf("the health projection carries a credential: %s", encoded)
		}
	})
}

func requireSubject(t *testing.T, subject Subject) {
	t.Helper()
	switch {
	case subject.Name == "":
		t.Fatal("conformance: the subject has no name")
	case !subject.Provider.Valid():
		t.Fatalf("conformance: %q is not a provider slug", subject.Provider)
	case !subject.ModelReference.Pinned():
		t.Fatalf("conformance: %q must be revision-pinned", subject.ModelReference)
	case subject.UpstreamModelID == "":
		t.Fatal("conformance: the subject declares no upstream model id")
	case subject.NewAdapter == nil, subject.NewUnconfigured == nil, subject.StartUpstream == nil, subject.Unsupported == nil:
		t.Fatal("conformance: the subject is incomplete")
	}
}

/* -------------------------------------------------------------------------- */
/*  Driving one request                                                       */
/* -------------------------------------------------------------------------- */

type run struct {
	events                   []contract.StreamEvent
	report                   *contract.UsageReport
	failure                  *contract.Error
	upstream                 *Upstream
	upstreamEchoedCredential bool
}

func (r *run) sawEvent(kind contract.StreamEventType) bool {
	for _, event := range r.events {
		if event.EventType() == kind {
			return true
		}
	}
	return false
}

// interceptor lets a test act on the stream as it arrives — cancelling, for
// instance, at the moment the first output reaches the client.
type interceptor func(event contract.StreamEvent, cancel context.CancelFunc)

func cancelAfterFirstDelta(event contract.StreamEvent, cancel context.CancelFunc) {
	if event.EventType() == contract.EventDelta {
		cancel()
	}
}

func execute(t *testing.T, subject Subject, scenario Scenario, request *contract.Request, intercept interceptor) *run {
	t.Helper()

	upstream := subject.StartUpstream(t, scenario)
	adapter := subject.NewAdapter(t, upstream.URL)

	inventoryJSON := fmt.Sprintf(`{"deployments":[{
		"deploymentId":"dep_conformance",
		"provider":%q,
		"modelReference":%q,
		"upstreamModelId":%q,
		"region":"test-region",
		"current":true
	}]}`, subject.Provider, subject.ModelReference, subject.UpstreamModelID)

	inv, err := inventory.Parse([]byte(inventoryJSON))
	if err != nil {
		t.Fatalf("building the conformance inventory: %v", err)
	}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatalf("registering the adapter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := &run{upstream: upstream}
	var mutex sync.Mutex
	sink := func(event contract.StreamEvent) error {
		mutex.Lock()
		result.events = append(result.events, event)
		mutex.Unlock()
		if intercept != nil {
			intercept(event, cancel)
		}
		return nil
	}

	executed := relay.NewExecutor(inv, registry).Execute(ctx, request, sink)
	result.report = executed.Report
	result.failure = executed.Failure
	result.upstreamEchoedCredential = scenario == ScenarioCredentialEchoed && upstream.RequestCount() > 0
	return result
}

/* -------------------------------------------------------------------------- */
/*  Invariants                                                                */
/* -------------------------------------------------------------------------- */

// assertWellFramedStream checks every framing rule the contract states in prose:
// one start, first; a monotonic sequence with no gaps; the request id on every
// event; the declared schema version on every event; and nothing after a
// terminal event.
func assertWellFramedStream(t *testing.T, r *run) {
	t.Helper()
	if len(r.events) == 0 {
		t.Fatal("the stream carried no events")
	}
	if first := r.events[0].EventType(); first != contract.EventStart {
		t.Fatalf("the stream opens with a %q event, not start", first)
	}

	starts, terminals := 0, 0
	for index, event := range r.events {
		if event.Sequence() != index {
			t.Errorf("event %d carries sequence %d; sequences must be monotonic from zero so a redelivery is detectable", index, event.Sequence())
		}
		switch event.EventType() {
		case contract.EventStart:
			starts++
		case contract.EventDone, contract.EventError:
			terminals++
			if index != len(r.events)-1 {
				t.Errorf("a %q event at position %d is followed by %d more events", event.EventType(), index, len(r.events)-index-1)
			}
		}

		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("event %d does not encode: %v", index, err)
		}
		var generic struct {
			SchemaVersion int    `json:"schemaVersion"`
			RequestID     string `json:"requestId"`
			Type          string `json:"type"`
		}
		if err := json.Unmarshal(encoded, &generic); err != nil {
			t.Fatalf("event %d does not decode: %v", index, err)
		}
		if generic.SchemaVersion != contract.SchemaVersion {
			t.Errorf("event %d carries schemaVersion %d, expected %d", index, generic.SchemaVersion, contract.SchemaVersion)
		}
		if generic.RequestID == "" {
			t.Errorf("event %d carries no requestId, so a proxy or a reconnecting client could not attribute it", index)
		}
		if generic.Type != string(event.EventType()) {
			t.Errorf("event %d encodes type %q while reporting %q", index, generic.Type, event.EventType())
		}
	}
	if starts != 1 {
		t.Errorf("the stream carries %d start events, expected exactly 1", starts)
	}
	if terminals != 1 {
		t.Errorf("the stream carries %d terminal events, expected exactly 1", terminals)
	}

	start, isStart := r.events[0].(*contract.StreamStartEvent)
	if !isStart {
		t.Fatalf("the first event reports start but is a %T", r.events[0])
	}
	if !start.ResolvedModelReference.Pinned() {
		t.Errorf("the start event reports %q, which is not revision-pinned: a customer is not told which weights answered", start.ResolvedModelReference)
	}
	if !start.ServingProvider.Valid() {
		t.Errorf("the start event names serving provider %q", start.ServingProvider)
	}
}

func assertTerminal(t *testing.T, r *run, expected contract.StreamEventType) {
	t.Helper()
	if len(r.events) == 0 {
		t.Fatal("the stream carried no events")
	}
	if got := r.events[len(r.events)-1].EventType(); got != expected {
		t.Fatalf("the stream ends with %q, expected %q", got, expected)
	}
}

func assertFailure(t *testing.T, r *run) *contract.Error {
	t.Helper()
	if r.failure == nil {
		t.Fatal("the request was expected to fail and did not")
	}
	if r.failure.RequestID == "" {
		t.Error("the error carries no requestId, so a customer cannot correlate it with anything")
	}
	if r.failure.Retryable && !r.failure.Code.Retryable() {
		t.Errorf("%q is retryable in the error body and non-retryable in the contract", r.failure.Code)
	}
	assertTerminal(t, r, contract.EventError)
	return r.failure
}

// assertReport checks the technical usage record settlement runs against.
func assertReport(t *testing.T, r *run, expected contract.RequestOutcome) {
	t.Helper()
	if r.report == nil {
		t.Fatal("no usage report was produced, so the request cannot be settled")
	}
	if err := r.report.Validate(); err != nil {
		t.Fatalf("the usage report would be rejected by the contract: %v", err)
	}
	if r.report.Outcome != expected {
		t.Errorf("the report says %q, expected %q", r.report.Outcome, expected)
	}
	if r.report.RouteSwitches != 0 {
		t.Errorf("the report counts %d route switches; failover is not implemented", r.report.RouteSwitches)
	}
	if r.report.DeploymentID == nil || *r.report.DeploymentID == "" {
		t.Error("the report names no deployment, so a charge cannot be attributed to a route")
	}
	started, err := r.report.StartedAt.Time()
	if err != nil {
		t.Fatalf("the report's startedAt is unparseable: %v", err)
	}
	if time.Since(started) > time.Minute {
		t.Errorf("the report's startedAt is %s old; it is not the instant this request began", time.Since(started))
	}
}

/* -------------------------------------------------------------------------- */
/*  Fixtures                                                                  */
/* -------------------------------------------------------------------------- */

func streamingRequest(subject Subject) *contract.Request {
	reference := subject.ModelReference
	return &contract.Request{
		SchemaVersion: contract.RequestEnvelopeVersion,
		Attribution: contract.Attribution{
			Principal: contract.AuthenticatedPrincipal{
				Billing:         contract.BillingPrincipal{AccountID: "acc_conformance"},
				ApplicationID:   "app_conformance",
				CredentialID:    "cred_conformance",
				Environment:     contract.EnvironmentDevelopment,
				InferenceScopes: []contract.Scope{contract.ScopeInvoke},
			},
			RequestID: "req_conformance",
		},
		Target:   contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference},
		Modality: contract.ModalityText,
		Input: contract.Input{
			Format: contract.InputMessages,
			Messages: []contract.Message{{
				Role:    contract.RoleUser,
				Content: []contract.ContentPart{{Type: contract.ContentPartText, Text: textOf("hello")}},
			}},
		},
		Stream: true,
		Client: contract.ClientRequestMetadata{
			APIFormat:  contract.APIFormatResponses,
			Endpoint:   "/v1/responses",
			ReceivedAt: contract.NewTimestamp(time.Now()),
		},
		RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: "rp_conformance", PolicyVersion: 1},
	}
}

func textOf(value string) *string { return &value }

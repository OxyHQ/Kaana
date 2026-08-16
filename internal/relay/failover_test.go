package relay_test

import (
	"context"
	"testing"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/provider"
	"github.com/OxyHQ/Relay/internal/providercost"
	"github.com/OxyHQ/Relay/internal/relay"
	"github.com/OxyHQ/Relay/internal/rotation"
)

// twoDeploymentsOfOneRevision is the failover set: one revision of one model,
// served by two providers. Everything below turns on the fact that these two
// rows name the SAME modelReference — that is what makes a switch between them
// same-model rather than a substitution.
const twoDeploymentsOfOneRevision = `
  {"deploymentId":"dep_a","provider":"stub","modelReference":"stub/model@2026-05-01",
   "upstreamModelId":"model-a","region":"r1","current":true},
  {"deploymentId":"dep_b","provider":"backup","modelReference":"stub/model@2026-05-01",
   "upstreamModelId":"model-b","region":"r2","current":true}`

func succeedingAdapter(slug contract.ProviderSlug, tokens int) *scriptedAdapter {
	return &scriptedAdapter{slug: slug, stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "served by "+string(slug)); err != nil {
			return provider.Outcome{}, err
		}
		units := []contract.UsageQuantity{
			{Unit: contract.UnitRequests, Quantity: 1},
			{Unit: contract.UnitOutputTokens, Quantity: tokens},
		}
		return provider.Outcome{Units: units, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}, nil
	}}
}

// failingAdapter fails before emitting anything, which is what an upstream that
// refuses a request with a status code looks like from here.
func failingAdapter(slug contract.ProviderSlug, failure error, units []contract.UsageQuantity) *scriptedAdapter {
	return &scriptedAdapter{slug: slug, stream: func(context.Context, *provider.Call, provider.Emitter) (provider.Outcome, error) {
		return provider.Outcome{Units: units, UsageSource: contract.UsageProviderReported}, failure
	}}
}

func overloaded(slug contract.ProviderSlug) provider.ErrUpstream {
	return provider.ErrUpstream{
		Code:        contract.CodeProviderOverloaded,
		Category:    contract.UpstreamOverloaded,
		Detail:      string(slug) + " is overloaded",
		Passthrough: &contract.ProviderErrorPassthrough{Provider: slug},
	}
}

func customerFault(slug contract.ProviderSlug) provider.ErrUpstream {
	return provider.ErrUpstream{
		Code:        contract.CodeInvalidRequest,
		Category:    contract.UpstreamInvalidReq,
		Detail:      string(slug) + " rejected the request",
		Passthrough: &contract.ProviderErrorPassthrough{Provider: slug},
	}
}

func runOn(executor *relay.Executor, request *contract.Request) ([]contract.StreamEvent, relay.Result) {
	var events []contract.StreamEvent
	result := executor.Execute(context.Background(), request, func(event contract.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	return events, result
}

func eventsOfType(events []contract.StreamEvent, kind contract.StreamEventType) []contract.StreamEvent {
	matched := make([]contract.StreamEvent, 0)
	for _, event := range events {
		if event.EventType() == kind {
			matched = append(matched, event)
		}
	}
	return matched
}

/* -------------------------------------------------------------------------- */
/*  The policy this build is not sent                                         */
/* -------------------------------------------------------------------------- */

// TestWithoutARoutingPolicyRelayNeverChoosesAmongDeployments pins the default,
// and it is the most important test in this file.
//
// The published routingFallbackPolicySchema gives the customer `disabled` and
// `sameModelDeployment` — two booleans that govern exactly this feature — and
// routingPolicySchema adds allowedRegions and deniedRegions. The envelope
// carries a routing policy REFERENCE and none of those values. So a Relay that
// failed over by default would silently override a control the platform
// advertises to customers, for every customer who switched it off.
//
// With the default in place a reference resolves to its declared primary and
// nowhere else, which is exactly how this build behaved before failover
// existed. Every other test in this file sets the authorisation explicitly; if
// this one ever passes for the wrong reason, they all become vacuous.
func TestWithoutARoutingPolicyRelayNeverChoosesAmongDeployments(t *testing.T) {
	primary := failingAdapter("stub", overloaded("stub"), nil)
	secondary := succeedingAdapter("backup", 7)

	events, result := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters:    []provider.Adapter{primary, secondary},
		// failoverAuthorized deliberately left false: this is the shipped default.
	}.run(t, baseRequest())

	if primary.attempts() != 1 {
		t.Errorf("the declared primary was attempted %d times", primary.attempts())
	}
	if secondary.attempts() != 0 {
		t.Errorf("a second deployment was used %d times without a policy authorising the choice", secondary.attempts())
	}
	if len(eventsOfType(events, contract.EventRouteSwitch)) != 0 {
		t.Error("a route switch was announced without a policy authorising it")
	}
	if result.Failure == nil || result.Failure.Code != contract.CodeProviderOverloaded {
		t.Fatalf("the customer was told %v; the declared primary failed and nothing else was tried", result.Failure)
	}
	if result.Report == nil || result.Report.RouteSwitches != 0 {
		t.Errorf("the report counts %v route switches", result.Report)
	}

	// The control: the identical fixture DOES fail over once the policy value
	// says it may, so the refusal above is the authorisation and not a broken
	// fixture.
	authorized := failingAdapter("stub", overloaded("stub"), nil)
	authorizedSecondary := succeedingAdapter("backup", 7)
	if _, result := (harness{
		deployments:        twoDeploymentsOfOneRevision,
		adapters:           []provider.Adapter{authorized, authorizedSecondary},
		failoverAuthorized: true,
	}).run(t, baseRequest()); result.Failure != nil {
		t.Fatalf("with failover authorised the same fixture still failed: %v", result.Failure)
	}
	if authorizedSecondary.attempts() != 1 {
		t.Fatal("with failover authorised the second deployment was still not used, so the check above measures nothing")
	}
}

/* -------------------------------------------------------------------------- */
/*  Same-model failover                                                       */
/* -------------------------------------------------------------------------- */

func TestAFailedDeploymentFailsOverToAnotherServingTheSameRevision(t *testing.T) {
	primary := failingAdapter("stub", overloaded("stub"), nil)
	secondary := succeedingAdapter("backup", 12)

	events, result := harness{
		deployments:        twoDeploymentsOfOneRevision,
		adapters:           []provider.Adapter{primary, secondary},
		failoverAuthorized: true,
	}.run(t, baseRequest())

	if result.Failure != nil {
		t.Fatalf("a request with a healthy second deployment failed: %v", result.Failure)
	}
	if primary.attempts() != 1 {
		t.Errorf("the failing deployment was attempted %d times", primary.attempts())
	}
	if secondary.attempts() != 1 {
		t.Fatalf("the second deployment was attempted %d times; the request was not failed over", secondary.attempts())
	}

	if result.Report == nil {
		t.Fatal("no usage report was produced")
	}
	if err := result.Report.Validate(); err != nil {
		t.Fatalf("the report would be rejected by the contract: %v", err)
	}
	if result.Report.RouteSwitches != 1 {
		t.Errorf("the report counts %d route switches", result.Report.RouteSwitches)
	}
	if result.Report.ServingProvider != "backup" {
		t.Errorf("the report attributes the work to %q", result.Report.ServingProvider)
	}
	if result.Report.DeploymentID == nil || *result.Report.DeploymentID != "dep_b" {
		t.Errorf("the report names deployment %v", result.Report.DeploymentID)
	}

	// The switch is announced to the customer, before the start event, because
	// that is when it happened: nothing had been streamed yet.
	switches := eventsOfType(events, contract.EventRouteSwitch)
	if len(switches) != 1 {
		t.Fatalf("%d route-switch events reached the customer", len(switches))
	}
	if events[0].EventType() != contract.EventRouteSwitch {
		t.Errorf("the stream opens with %q", events[0].EventType())
	}
	start, isStart := events[1].(*contract.StreamStartEvent)
	if !isStart {
		t.Fatalf("the event after the switch is a %T", events[1])
	}
	if start.ServingProvider != "backup" {
		t.Errorf("the start event names %q as the serving provider", start.ServingProvider)
	}
	for index, event := range events {
		if event.Sequence() != index {
			t.Errorf("event %d carries sequence %d; a switch must not break the monotonic sequence", index, event.Sequence())
		}
	}
}

// TestFailoverNeverCrossesToADifferentModel is the assertion the whole feature
// is fenced by. The platform forbids serving a different model than the one
// asked for, and a failover that quietly crossed models would look exactly like
// a successful request.
//
// The structural half of the guarantee is elsewhere: an inventory.Endpoint
// carries no model reference of its own, so every candidate is the route set's
// one reference paired with a different address. What this checks is that the
// event Relay emits says so too, and cannot be read as a substitution.
func TestFailoverNeverCrossesToADifferentModel(t *testing.T) {
	events, result := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters: []provider.Adapter{
			failingAdapter("stub", overloaded("stub"), nil),
			succeedingAdapter("backup", 3),
		},
		failoverAuthorized: true,
	}.run(t, baseRequest())

	requested := contract.ModelReference("stub/model@2026-05-01")
	switches := eventsOfType(events, contract.EventRouteSwitch)
	if len(switches) != 1 {
		t.Fatalf("%d route-switch events were emitted, so this check has nothing to inspect", len(switches))
	}
	switched := switches[0].(*contract.StreamRouteSwitchEvent)

	if switched.Detail.Scope != contract.SwitchScopeDeployment {
		t.Errorf("the switch is scoped %q; only a deployment-scoped switch serves the same weights", switched.Detail.Scope)
	}
	if switched.Detail.ModelReference == nil || *switched.Detail.ModelReference != requested {
		t.Errorf("the switch names model %v, the request asked for %q", switched.Detail.ModelReference, requested)
	}
	// The fields that describe a model substitution must be absent. A
	// deployment switch that filled them in would be a cross-model fallback
	// wearing the wrong scope.
	if switched.Detail.RequestedModelID != nil ||
		switched.Detail.FromModelReference != nil ||
		switched.Detail.ToModelReference != nil ||
		switched.Detail.AuthorizedByPolicy != nil {
		t.Errorf("the switch carries model-substitution detail: %+v", switched.Detail)
	}

	// And the customer is told the same weights answered as were asked for.
	start := events[1].(*contract.StreamStartEvent)
	if start.ResolvedModelReference != requested {
		t.Errorf("after a failover the start event reports %q", start.ResolvedModelReference)
	}
	if result.Report.ResolvedModelReference != requested {
		t.Errorf("after a failover the usage report reports %q", result.Report.ResolvedModelReference)
	}
}

// TestFailoverStopsOnceOutputHasBeenEmitted: a retry after the customer has
// tokens in hand would deliver the beginning of one answer and the whole of
// another. The emitter refuses the switch, which is what makes this a property
// of the code rather than of the executor remembering to check.
func TestFailoverStopsOnceOutputHasBeenEmitted(t *testing.T) {
	primary := &scriptedAdapter{slug: "stub", stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "half an answer"); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{
			Units:       []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 4}},
			UsageSource: contract.UsageProviderReported,
		}, overloaded("stub")
	}}
	secondary := succeedingAdapter("backup", 99)

	events, result := harness{
		deployments:        twoDeploymentsOfOneRevision,
		adapters:           []provider.Adapter{primary, secondary},
		failoverAuthorized: true,
	}.run(t, baseRequest())

	if secondary.attempts() != 0 {
		t.Error("the request was retried elsewhere after output had already reached the customer")
	}
	if len(eventsOfType(events, contract.EventRouteSwitch)) != 0 {
		t.Error("a route switch was emitted after output had begun")
	}
	if result.Failure == nil {
		t.Fatal("the request was reported successful")
	}
	if result.Report == nil || result.Report.Outcome != contract.OutcomePartial {
		t.Errorf("a stream that produced output and then failed settled as %v", result.Report)
	}
	if last := events[len(events)-1]; last.EventType() != contract.EventError {
		t.Errorf("the stream ends with %q", last.EventType())
	}
}

/* -------------------------------------------------------------------------- */
/*  What must never be retried                                                */
/* -------------------------------------------------------------------------- */

// TestARequestTheProviderCannotExpressIsNotRetriedElsewhere: the refusal is
// about the request. Retrying would make what a request MEANS depend on which
// route happened to be healthy, so an identical envelope would be refused now
// and served in five minutes.
func TestARequestTheProviderCannotExpressIsNotRetriedElsewhere(t *testing.T) {
	primary := succeedingAdapter("stub", 1)
	primary.translate = func(*contract.Request, provider.Route) (*provider.Call, error) {
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeInvalidRequest,
			Param:  "sampling.topK",
			Detail: "this protocol has no top_k parameter",
		}
	}
	secondary := succeedingAdapter("backup", 1)

	_, result := harness{
		deployments:        twoDeploymentsOfOneRevision,
		adapters:           []provider.Adapter{primary, secondary},
		failoverAuthorized: true,
	}.run(t, baseRequest())

	if secondary.attempts() != 0 {
		t.Errorf("a refusal to translate was retried on %d other deployments", secondary.attempts())
	}
	if result.Failure == nil || result.Failure.Code != contract.CodeInvalidRequest {
		t.Fatalf("the refusal reported %v", result.Failure)
	}
	if result.Failure.Param == nil || *result.Failure.Param != "sampling.topK" {
		t.Errorf("the refusal names %v as the field at fault", result.Failure.Param)
	}
}

// TestACustomerFaultIsNotRetriedElsewhereAndCostsNoDeploymentItsPlace: an
// upstream that rejected the request would reject it everywhere, so a retry
// spends a second time to be refused a second time — and counting it against
// the deployment would take a healthy route out of rotation for everybody.
func TestACustomerFaultIsNotRetriedElsewhereAndCostsNoDeploymentItsPlace(t *testing.T) {
	primary := failingAdapter("stub", customerFault("stub"), nil)
	secondary := succeedingAdapter("backup", 1)
	rotationRegistry := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 2}, nil)

	executor := harness{
		deployments:        twoDeploymentsOfOneRevision,
		adapters:           []provider.Adapter{primary, secondary},
		rotation:           rotationRegistry,
		failoverAuthorized: true,
	}.build(t)

	for range 5 {
		_, result := runOn(executor, baseRequest())
		if result.Failure == nil || result.Failure.Code != contract.CodeInvalidRequest {
			t.Fatalf("a request the provider rejected reported %v", result.Failure)
		}
	}

	if secondary.attempts() != 0 {
		t.Errorf("a request the provider rejected was retried on %d other deployments", secondary.attempts())
	}
	if primary.attempts() != 5 {
		t.Errorf("the first deployment was attempted %d times across 5 requests, so it left rotation", primary.attempts())
	}
	projected := rotationRegistry.Project([]contract.DeploymentID{"dep_a"})[0]
	if projected.State != rotation.StateClosed {
		t.Errorf("five customer-fault failures left the deployment %q", projected.State)
	}
}

/* -------------------------------------------------------------------------- */
/*  Breakers in the routing decision                                          */
/* -------------------------------------------------------------------------- */

// TestAnOpenDeploymentIsNoLongerTriedFirst: once a deployment is out of
// rotation the request goes straight to a healthy one. Nothing was attempted on
// the open route, so this is route SELECTION and not a switch, and the receipt
// must not count it as one.
func TestAnOpenDeploymentIsNoLongerTriedFirst(t *testing.T) {
	primary := failingAdapter("stub", overloaded("stub"), nil)
	secondary := succeedingAdapter("backup", 5)
	rotationRegistry := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 2, Cooldown: time.Hour}, nil)

	executor := harness{
		deployments:        twoDeploymentsOfOneRevision,
		adapters:           []provider.Adapter{primary, secondary},
		rotation:           rotationRegistry,
		failoverAuthorized: true,
	}.build(t)

	// Two failures open dep_a's breaker. Each of these requests fails over and
	// is served, so the customer never sees the provider's trouble.
	for range 2 {
		if _, result := runOn(executor, baseRequest()); result.Failure != nil {
			t.Fatalf("a request with a healthy second deployment failed: %v", result.Failure)
		}
	}
	attemptsBefore := primary.attempts()

	events, result := runOn(executor, baseRequest())

	if primary.attempts() != attemptsBefore {
		t.Errorf("the open deployment was attempted again (%d attempts, was %d)", primary.attempts(), attemptsBefore)
	}
	if result.Failure != nil {
		t.Fatalf("the request failed: %v", result.Failure)
	}
	if result.Report.RouteSwitches != 0 {
		t.Errorf("the report counts %d route switches for a request that was only ever sent to one deployment", result.Report.RouteSwitches)
	}
	if len(eventsOfType(events, contract.EventRouteSwitch)) != 0 {
		t.Error("a route switch was announced for a deployment that was never attempted")
	}
	if result.Report.ServingProvider != "backup" {
		t.Errorf("the report attributes the work to %q", result.Report.ServingProvider)
	}
}

// TestNoSwitchIsAnnouncedToADeploymentThatIsNeverTried covers the case the
// ordering above hides.
//
// A route switch is announced at the top of the attempt that REPLACES the
// failed one, not at the moment of failure — because the replacement's own
// breaker may refuse it. Announcing on failure would tell a customer their
// request moved to a deployment that never received it, and put a switch on the
// receipt that never happened.
//
// The fixture forces it: the healthy-looking candidate fails, and the only
// other one is already out of rotation.
func TestNoSwitchIsAnnouncedToADeploymentThatIsNeverTried(t *testing.T) {
	primary := failingAdapter("stub", overloaded("stub"), nil)
	secondary := failingAdapter("backup", overloaded("backup"), nil)
	rotationRegistry := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 1, Cooldown: time.Hour}, nil)

	executor := harness{
		deployments:        twoDeploymentsOfOneRevision,
		adapters:           []provider.Adapter{primary, secondary},
		rotation:           rotationRegistry,
		failoverAuthorized: true,
	}.build(t)

	// The first request tries both and opens both breakers. It legitimately
	// switches once, which is the control: the assertions below are about a
	// switch that must NOT be announced, and a build that announced none at all
	// would pass them vacuously.
	events, _ := runOn(executor, baseRequest())
	if len(eventsOfType(events, contract.EventRouteSwitch)) != 1 {
		t.Fatalf("the control request announced %d switches, expected exactly 1", len(eventsOfType(events, contract.EventRouteSwitch)))
	}

	// Now dep_b is open. dep_a is open too, so nothing is attempted at all and
	// the request is refused before any provider is reached.
	if _, result := runOn(executor, baseRequest()); result.Failure.Code != contract.CodeDeploymentUnavailable {
		t.Fatalf("with both breakers open the request reported %q", result.Failure.Code)
	}

	// Let dep_a back in, on its own, and leave dep_b out. dep_a fails, and
	// there is nowhere to go.
	rotationRegistry.Retain([]contract.DeploymentID{"dep_b"})
	attemptsBefore := secondary.attempts()

	events, result := runOn(executor, baseRequest())

	if secondary.attempts() != attemptsBefore {
		t.Errorf("the out-of-rotation deployment was attempted (%d attempts, was %d)", secondary.attempts(), attemptsBefore)
	}
	if switches := eventsOfType(events, contract.EventRouteSwitch); len(switches) != 0 {
		t.Errorf("%d route switches were announced to a deployment that was never tried", len(switches))
	}
	if result.Failure == nil {
		t.Fatal("the request was reported successful")
	}
	if result.Failure.Code != contract.CodeProviderOverloaded {
		t.Errorf("the customer was told %q, and what happened is that the deployment that ran failed", result.Failure.Code)
	}
	if result.Report == nil {
		t.Fatal("the attempt that ran produced no usage report")
	}
	if result.Report.RouteSwitches != 0 {
		t.Errorf("the report counts %d route switches", result.Report.RouteSwitches)
	}
	if result.Report.DeploymentID == nil || *result.Report.DeploymentID != "dep_a" {
		t.Errorf("the report names deployment %v; the attempt that ran was dep_a", result.Report.DeploymentID)
	}
}

// TestEveryRouteOutOfRotationRefusesWithARealRetryHint: the hint is the moment
// the earliest breaker will admit its next trial, which is a fact this process
// holds rather than a number chosen to look reasonable.
func TestEveryRouteOutOfRotationRefusesWithARealRetryHint(t *testing.T) {
	cooldown := 30 * time.Second
	rotationRegistry := rotation.NewRegistry(rotation.Policy{FailuresToOpen: 1, Cooldown: cooldown}, nil)

	executor := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters: []provider.Adapter{
			failingAdapter("stub", overloaded("stub"), nil),
			failingAdapter("backup", overloaded("backup"), nil),
		},
		rotation:           rotationRegistry,
		failoverAuthorized: true,
	}.build(t)

	// The first request tries both, fails over once, and opens both breakers.
	if _, result := runOn(executor, baseRequest()); result.Failure == nil {
		t.Fatal("a request whose every deployment failed was reported successful")
	}

	events, result := runOn(executor, baseRequest())

	if result.Failure == nil {
		t.Fatal("a request with no route in rotation was served")
	}
	if result.Failure.Code != contract.CodeDeploymentUnavailable {
		t.Errorf("refused with %q", result.Failure.Code)
	}
	if !result.Failure.Retryable {
		t.Error("the refusal is non-retryable, but the breakers clear on their own")
	}
	if result.Failure.RetryAfterMs == nil {
		t.Fatal("the refusal carries no retry hint, and this process knows exactly when the next probe is")
	}
	if *result.Failure.RetryAfterMs <= 0 || *result.Failure.RetryAfterMs > int(cooldown.Milliseconds()) {
		t.Errorf("the refusal advises retrying in %dms, and the cooldown is %s", *result.Failure.RetryAfterMs, cooldown)
	}
	// Nothing executed, so there is nothing to settle.
	if result.Report != nil {
		t.Errorf("a request that never reached a provider produced a usage report: %+v", result.Report)
	}
	if len(events) != 1 || events[0].EventType() != contract.EventError {
		t.Errorf("the refusal produced %d events", len(events))
	}
}

/* -------------------------------------------------------------------------- */
/*  What a failed attempt costs, and who pays for it                          */
/* -------------------------------------------------------------------------- */

// TestAFailedAttemptIsOffTheCustomersReceiptAndOnRelaysCost is the asymmetry
// that makes provider cost a separate measurement rather than a field on the
// usage report.
//
// The customer never received the failed attempt's output, so charging for it
// would be wrong. The provider will invoice for it regardless, so dropping it
// entirely would leave Relay reconciling against a number that is short by
// exactly its own failover traffic.
func TestAFailedAttemptIsOffTheCustomersReceiptAndOnRelaysCost(t *testing.T) {
	burned := []contract.UsageQuantity{
		{Unit: contract.UnitRequests, Quantity: 1},
		{Unit: contract.UnitOutputTokens, Quantity: 40},
	}
	cards, err := providercost.Parse([]byte(`{"rateCards":[
		{"deploymentId":"dep_a","currency":"XTS","rates":[
			{"unit":"requests","amountPerUnit":1000},{"unit":"output_tokens","amountPerUnit":10}]},
		{"deploymentId":"dep_b","currency":"XTS","rates":[
			{"unit":"requests","amountPerUnit":1000},{"unit":"output_tokens","amountPerUnit":10}]}]}`))
	if err != nil {
		t.Fatalf("building rate cards: %v", err)
	}

	_, result := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters: []provider.Adapter{
			failingAdapter("stub", overloaded("stub"), burned),
			succeedingAdapter("backup", 4),
		},
		costs:              cards,
		failoverAuthorized: true,
	}.run(t, baseRequest())

	if result.Failure != nil {
		t.Fatalf("the request failed: %v", result.Failure)
	}

	// The receipt carries the serving attempt's units, and only those.
	for _, unit := range result.Report.Units {
		if unit.Unit == contract.UnitOutputTokens && unit.Quantity != 4 {
			t.Errorf("the customer's receipt carries %d output tokens; the served attempt produced 4", unit.Quantity)
		}
	}

	// The cost carries both.
	if len(result.UpstreamCost.Attempts) != 2 {
		t.Fatalf("the cost record holds %d attempts, expected the failed one and the served one", len(result.UpstreamCost.Attempts))
	}
	if !result.UpstreamCost.Complete {
		t.Errorf("the cost record is incomplete: %+v", result.UpstreamCost)
	}
	if len(result.UpstreamCost.Totals) != 1 {
		t.Fatalf("the cost record totals %d currencies", len(result.UpstreamCost.Totals))
	}
	// 1×1000 + 40×10 burned, then 1×1000 + 4×10 served.
	const expected = 1000 + 400 + 1000 + 40
	if result.UpstreamCost.Totals[0].Amount != expected {
		t.Errorf("the request cost %d upstream, expected %d (the failed attempt's units are part of it)",
			result.UpstreamCost.Totals[0].Amount, expected)
	}

	served := 0
	for _, attempt := range result.UpstreamCost.Attempts {
		if attempt.Served {
			served++
		}
	}
	if served != 1 {
		t.Errorf("%d attempts are marked as having served the customer", served)
	}
}

/* -------------------------------------------------------------------------- */
/*  Serving from a stale snapshot                                             */
/* -------------------------------------------------------------------------- */

// TestAStaleSnapshotServesPinnedTargetsAndRefusesUnpinnedOnes is the
// control-plane outage behaviour, read through the errors a customer receives.
func TestAStaleSnapshotServesPinnedTargetsAndRefusesUnpinnedOnes(t *testing.T) {
	// The snapshot was issued two hours ago and nothing has re-issued it. The
	// default horizon is an hour.
	stale := harness{
		deployments: twoDeploymentsOfOneRevision,
		adapters: []provider.Adapter{
			succeedingAdapter("stub", 1),
			succeedingAdapter("backup", 1),
		},
		issuedAt: time.Now().Add(-2 * time.Hour),
	}

	pinned := baseRequest()
	if _, result := stale.run(t, pinned); result.Failure != nil {
		t.Fatalf("a pinned reference was refused from a stale snapshot: %v", result.Failure)
	}

	unpinned := baseRequest()
	reference := contract.ModelReference("stub/model")
	unpinned.Target.ModelReference = &reference

	_, result := stale.run(t, unpinned)
	if result.Failure == nil {
		t.Fatal("an unpinned reference resolved from a snapshot nobody is re-issuing")
	}
	if result.Failure.Code != contract.CodeServiceUnavailable {
		t.Errorf("refused with %q", result.Failure.Code)
	}
	if !result.Failure.Retryable {
		t.Error("the refusal is non-retryable, but it clears the moment the control plane publishes again")
	}

	// The control: the same unpinned request is served from a snapshot inside
	// the horizon, so the refusal above is the staleness and not the reference.
	fresh := stale
	fresh.issuedAt = time.Now()
	if _, result := fresh.run(t, unpinned); result.Failure != nil {
		t.Fatalf("a fresh snapshot refused an unpinned reference: %v", result.Failure)
	}
}

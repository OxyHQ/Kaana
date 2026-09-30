package kaana

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// A realtime session is opened here and run elsewhere (internal/realtime).
//
// Opening is the half that is routing: the signed authorizedRoutes are
// resolved against the inventory by the same function a request uses, each is
// admitted by its deployment breaker, resolved to its exact platform
// credential, and attempted in order until one OPENS. Once one has, the
// session belongs to that deployment for its whole life — the conversation
// state is upstream, so there is no failover after this point, and nothing
// here is consulted again until settlement.
//
// Settlement is the other half that is the executor's: pricing every attempt
// the session made, the opened one included, and writing that operator record
// exactly once.

// SessionOpening is what opening a realtime session produced.
type SessionOpening struct {
	// Upstream is the open provider session. Nil when no route opened.
	Upstream provider.RealtimeUpstream
	// Route is the deployment that opened, or, when none did, the last route
	// attempted (the first signed one when none was), so a report can still
	// name what it refused.
	Route provider.Route
	// Failure is why no route opened. Non-nil exactly when Upstream is nil.
	Failure *contract.Error

	requestID    contract.RequestID
	attribution  contract.Attribution
	generationID *contract.GenerationID
	startedAt    time.Time
	// attempts are the failed open attempts, in order; the opened one is
	// appended at settlement, when its latency and units are known.
	attempts []providercost.AttemptUsage
	keyID    string
	keyClass provider.KeyClass
	// openStartedAt and openClock time the opened attempt: the executor's
	// clock for when it began, the monotonic clock for how long it ran.
	openStartedAt time.Time
	openClock     time.Time
}

// Opened reports whether a route opened.
func (o *SessionOpening) Opened() bool { return o.Upstream != nil }

// StartedAt is when the session request was received.
func (o *SessionOpening) StartedAt() time.Time { return o.startedAt }

// OpenSession tries the session's signed routes in order until one opens.
//
// ctx is the SESSION's lifetime, not the connection's: a session outlives the
// connection that asked for it (a dropped client may resume), so the caller
// owns the open upstream and ends it with Upstream.Close. Cancelling ctx while
// a route is opening abandons the open.
func (e *Executor) OpenSession(ctx context.Context, request *contract.RealtimeSessionRequest) *SessionOpening {
	requestID := request.Attribution.RequestID
	opening := &SessionOpening{
		requestID: requestID, attribution: request.Attribution,
		generationID: generationFor(request.Attribution), startedAt: e.now(),
	}
	if len(request.AuthorizedRoutes) > 0 {
		first := request.AuthorizedRoutes[0]
		opening.Route = provider.Route{DeploymentID: first.DeploymentID, Provider: first.Provider, ModelReference: first.ModelReference, Regions: first.Regions}
	}
	refuse := func(failure *contract.Error) *SessionOpening {
		opening.Failure = failure
		return opening
	}

	if err := request.Validate(); err != nil {
		return refuse(contract.NewError(requestID, contract.CodeInvalidRequest, err.Error()))
	}
	if !request.Attribution.HasScope(contract.ScopeInvoke) {
		return refuse(contract.NewError(requestID, contract.CodeInsufficientScope, "the session request does not carry inference:invoke"))
	}
	for index, route := range request.AuthorizedRoutes {
		if route.CustomerProviderCredential != nil {
			// BYOK custody is request-scoped: the decrypted key lives for one
			// upstream call and is destroyed with it. A session would hold it
			// for up to an hour, which is a custody decision nobody has made,
			// so a customer credential is refused rather than kept.
			return refuse(contract.NewError(requestID, contract.CodeInvalidRequest,
				"a customer provider credential cannot serve a realtime session").
				WithParam(fmt.Sprintf("authorizedRoutes[%d].customerProviderCredential", index)))
		}
	}
	candidates, failure := resolveAuthorizedRoutes(e.inventory.Current(), requestID, request.AuthorizedRoutes, opening.startedAt)
	if failure != nil {
		return refuse(failure)
	}

	var (
		skipped []contract.DeploymentID
		lastErr error
	)
	for _, authorized := range candidates {
		route := authorized.route
		permit, admitted := e.rotation.Admit(route.DeploymentID)
		if !admitted {
			skipped = append(skipped, route.DeploymentID)
			continue
		}
		adapter, credentials, err := e.registry.ResolveRealtimeExecution(route.DeploymentID, route.Provider)
		if err != nil {
			permit.NotAttributable()
			if errors.Is(err, provider.ErrNotASessionAdapter) {
				// The signed route names a deployment whose adapter executes
				// requests. That is a fact about the route, identical on every
				// retry, so it is a refusal rather than a skip.
				return refuse(contract.NewError(requestID, contract.CodeUnsupportedModality,
					fmt.Sprintf("the %s deployment does not hold realtime sessions", route.Provider)).WithParam("authorizedRoutes"))
			}
			// No adapter, or no exact credential binding: a configuration gap
			// that says nothing about the provider's health.
			skipped = append(skipped, route.DeploymentID)
			continue
		}
		if !provider.OpensRealtime(adapter, request.Kind) {
			permit.NotAttributable()
			return refuse(contract.NewError(requestID, contract.CodeUnsupportedModality,
				fmt.Sprintf("the %s deployment does not hold %s sessions", route.Provider, request.Kind)).WithParam("kind"))
		}

		opening.Route = route
		attemptStartedAt, attemptClock := e.now(), time.Now()
		upstream, opened, err := adapter.Open(ctx, provider.RealtimeOpenRequest{
			RequestID: requestID, Route: route, Kind: request.Kind, Config: request.Config,
		}, credentials)
		if err == nil {
			permit.Succeeded()
			opening.Upstream = upstream
			opening.keyID, opening.keyClass = opened.KeyID, opened.KeyClass
			opening.openStartedAt, opening.openClock = attemptStartedAt, attemptClock
			return opening
		}
		var unsupported provider.ErrUnsupported
		if errors.As(err, &unsupported) {
			// Refused before anything was dialled: the request is what the
			// provider cannot express, identical everywhere.
			permit.NotAttributable()
			return refuse(translationFailure(requestID, err))
		}
		cancelled := isCancellation(ctx, err)
		opening.attempts = append(opening.attempts, providercost.AttemptUsage{
			AttemptIndex: len(opening.attempts), DeploymentID: route.DeploymentID, Provider: route.Provider,
			ModelReference: route.ModelReference, OccurredAt: e.now(), KeyID: opened.KeyID, KeyClass: string(opened.KeyClass),
			Telemetry: attemptTelemetry(ctx, requestID, route.Provider, err, attemptStartedAt, attemptClock, time.Time{}),
		})
		lastErr = err
		switch {
		case cancelled:
			permit.NotAttributable()
			return refuse(contract.NewError(requestID, contract.CodeCancelled, "the session was abandoned while it opened"))
		case !provider.DeploymentAttributable(err):
			permit.NotAttributable()
			return refuse(upstreamFailure(requestID, route.Provider, err))
		default:
			// A failure another deployment could survive, and nothing has been
			// sent to the client yet: the next signed route is attempted.
			permit.Failed()
		}
	}
	if lastErr != nil {
		return refuse(upstreamFailure(requestID, opening.Route.Provider, lastErr))
	}
	return refuse(e.everyRouteOutOfRotation(requestID, len(candidates), skipped, opening.startedAt))
}

// SessionEnd is what a session measured about itself when it ended.
type SessionEnd struct {
	ClosedAt time.Time
	// Units are the session's totals, each unit once.
	Units []contract.UsageQuantity
	// UsageSource is where the units came from: the provider's reports, or
	// Kaana's own measurement for a provider billed by it
	// (provider.RealtimeMeter). Empty means provider-reported.
	UsageSource contract.UsageSource
	// FirstOutputAt is when the first output reached the session; zero when
	// none did.
	FirstOutputAt time.Time
	// Outcome and FailureCode are how the opened attempt ended.
	Outcome     providercost.AttemptOutcome
	FailureCode contract.ErrorCode
	// ReportOutcome is how the request ended for settlement.
	ReportOutcome contract.RequestOutcome
}

// SessionSettlement is a settled session: the usage report the client is
// owed, and the operator cost of every attempt the session made.
type SessionSettlement struct {
	Report          *contract.UsageReport
	UpstreamCost    providercost.Record
	CostRecordError error
	// ReportError is set when the report failed its own validation, in which
	// case Report is nil: an unsettleable report is not handed over as if it
	// were fine.
	ReportError error
}

// SettleSession builds the session's usage report and prices and records the
// upstream cost of every attempt it made. The caller settles a session once.
func (e *Executor) SettleSession(ctx context.Context, opening *SessionOpening, end SessionEnd) SessionSettlement {
	usage := append([]providercost.AttemptUsage(nil), opening.attempts...)
	units := append([]contract.UsageQuantity{}, end.Units...)
	// Every failed open before the served (or last) attempt is a route the
	// session moved away from.
	switches := max(len(opening.attempts)-1, 0)
	if opening.Opened() {
		switches = len(opening.attempts)
		// The session's latency is its whole life, on the monotonic clock the
		// open attempt started on.
		latency := time.Since(opening.openClock)
		telemetry := providercost.AttemptTelemetry{StartedAt: opening.openStartedAt, Latency: latency, Outcome: end.Outcome, FailureCode: end.FailureCode}
		if !end.FirstOutputAt.IsZero() && !end.FirstOutputAt.Before(opening.openStartedAt) {
			telemetry.TimeToFirstOutput = min(end.FirstOutputAt.Sub(opening.openStartedAt), latency)
		}
		usage = append(usage, providercost.AttemptUsage{
			AttemptIndex: len(usage), DeploymentID: opening.Route.DeploymentID, Provider: opening.Route.Provider,
			ModelReference: opening.Route.ModelReference, OccurredAt: end.ClosedAt, KeyID: opening.keyID,
			KeyClass: string(opening.keyClass), Served: true, Units: units, Telemetry: telemetry,
		})
	} else {
		units = []contract.UsageQuantity{}
	}
	settlement := SessionSettlement{UpstreamCost: e.costs.MeasureRequest(opening.requestID, usage)}

	source := end.UsageSource
	if source == "" {
		source = contract.UsageProviderReported
	}
	report := &contract.UsageReport{
		SchemaVersion: contract.UsageReportSchemaVersion, RequestID: opening.requestID, GenerationID: opening.generationID,
		Attribution: withGeneration(opening.attribution, opening.generationID), Outcome: end.ReportOutcome,
		Units: units, UsageSource: source,
		ResolvedModelReference: opening.Route.ModelReference, ServingProvider: opening.Route.Provider,
		DeploymentID: opening.Route.DeploymentID, RouteSwitches: min(switches, 100),
		StartedAt: contract.NewTimestamp(opening.startedAt), CompletedAt: contract.NewTimestamp(end.ClosedAt),
	}
	if end.ClosedAt.Before(opening.startedAt) {
		report.CompletedAt = report.StartedAt
	}
	if !end.FirstOutputAt.IsZero() {
		milliseconds := int(end.FirstOutputAt.Sub(opening.startedAt).Milliseconds())
		if milliseconds > 0 {
			report.TimeToFirstTokenMs = &milliseconds
		}
	}
	if err := report.Validate(); err != nil {
		settlement.ReportError = fmt.Errorf("kaana: the session usage report is not settleable: %w", err)
	} else {
		settlement.Report = report
	}

	if e.costRecorder != nil && len(settlement.UpstreamCost.Attempts) > 0 {
		recordContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerCostPersistenceTimeout)
		defer cancel()
		settlement.CostRecordError = e.costRecorder.Record(recordContext, settlement.UpstreamCost)
	}
	return settlement
}

// Package relay executes one normalized inference request: it resolves a route,
// hands the request to an adapter, frames what the adapter reports as a
// normalized event stream, and produces the technical usage record settlement
// runs against.
//
// Everything a customer could be told "no" about was already decided at the Oxy
// edge — scopes, account access, credential status, spend. Relay does not
// re-derive any of it; the envelope it receives is an already-authorized
// instruction (ADR 0006).
package relay

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/inventory"
	"github.com/OxyHQ/Relay/internal/provider"
)

// Executor turns an envelope into a stream and a usage report.
type Executor struct {
	inventory *inventory.Inventory
	registry  *provider.Registry
	now       func() time.Time
}

// NewExecutor wires the inventory and the adapter registry together.
func NewExecutor(inv *inventory.Inventory, registry *provider.Registry) *Executor {
	return &Executor{inventory: inv, registry: registry, now: time.Now}
}

// Result is what one execution produced.
//
// Report is present whenever the request reached an adapter, including when it
// was cancelled or failed part-way: a partial stream is a settlement case, so
// the absence of a report is what makes a refund impossible rather than
// approximate.
type Result struct {
	Report *contract.UsageReport
	// Failure is the terminal error, if the request ended in one. It has been
	// emitted on the stream, except when the client cancelled: writing to a
	// client that withdrew is pointless, and a cancelled request whose events
	// kept flowing would be indistinguishable from one that completed.
	Failure *contract.Error
}

// ErrClientGone reports that the sink stopped accepting events. The executor
// treats it as a cancellation rather than a failure, because the customer
// withdrawing is not the provider failing.
var ErrClientGone = errors.New("relay: the event sink is gone")

// Execute runs the request, emitting normalized events to sink.
//
// Cancelling ctx cancels the upstream call. That is not a convention the
// adapters are asked to honour politely: the context is the only handle they
// are given for the HTTP request, so an adapter that ignores it cannot make an
// upstream call at all.
func (e *Executor) Execute(ctx context.Context, request *contract.Request, sink Sink) Result {
	requestID := request.Attribution.RequestID
	generationID := e.generationID(request)
	startedAt := e.now()

	route, adapter, failure := e.resolve(request)
	if failure != nil {
		emit := newEmitter(sink, requestID, generationID, "", startedAt)
		_ = emit.finishWithError(failure)
		return Result{Failure: failure}
	}

	emit := newEmitter(sink, requestID, generationID, route.Provider, startedAt)

	call, err := adapter.Translate(request, route)
	if err != nil {
		failure := translationFailure(requestID, err)
		_ = emit.finishWithError(failure)
		return Result{Failure: failure}
	}

	outcome, streamErr := adapter.Stream(ctx, call, emit)
	completedAt := e.now()

	report := &contract.UsageReport{
		SchemaVersion:          contract.SchemaVersion,
		RequestID:              requestID,
		GenerationID:           generationID,
		Attribution:            withGeneration(request.Attribution, generationID),
		Units:                  outcome.Units,
		UsageSource:            outcome.UsageSource,
		ResolvedModelReference: route.ModelReference,
		ServingProvider:        route.Provider,
		DeploymentID:           &route.DeploymentID,
		// Same-model failover and cross-model fallback are out of scope for this
		// build, so no route can switch and the count is structurally zero
		// rather than merely unobserved.
		RouteSwitches: 0,
		StartedAt:     contract.NewTimestamp(startedAt),
		CompletedAt:   contract.NewTimestamp(completedAt),
	}
	if ttft := emit.timeToFirstToken(); ttft > 0 {
		milliseconds := int(ttft.Milliseconds())
		report.TimeToFirstTokenMs = &milliseconds
	}
	if report.UsageSource == "" {
		// An adapter that measured nothing still has to say how it knows. The
		// honest answer is that the number is a reconstruction, and marking it
		// so is what lets settlement apply its estimation policy knowingly
		// instead of treating a guess as a provider's count.
		report.UsageSource = contract.UsageEstimated
	}

	switch {
	case streamErr == nil && !emit.started:
		// An adapter that returns success without ever emitting a start event
		// has produced a stream the contract cannot describe. Reporting it as
		// completed would hand settlement a receipt for output nobody saw.
		report.Outcome = contract.OutcomeFailed
		failure := contract.NewError(requestID, contract.CodeInternalError,
			fmt.Sprintf("the %s adapter completed without starting a stream", route.Provider))
		_ = emit.finishWithError(failure)
		return e.finalize(report, failure, nil)
	case streamErr == nil:
		report.Outcome = contract.OutcomeCompleted
		if err := emit.finishWithDone(finishReasonOr(outcome.FinishReason, contract.FinishStop), completedAt); err != nil {
			return e.finalize(report, nil, err)
		}
	case isCancellation(ctx, streamErr):
		report.Outcome = contract.OutcomeCancelled
		// The stream is not written to again: the client that cancelled is not
		// listening, and a cancelled request whose events kept flowing would be
		// indistinguishable from one that completed.
		return e.finalize(report, contract.NewError(requestID, contract.CodeCancelled, "the client cancelled the request"), nil)
	default:
		if len(outcome.Units) > 0 {
			report.Outcome = contract.OutcomePartial
		} else {
			report.Outcome = contract.OutcomeFailed
		}
		failure := upstreamFailure(requestID, route.Provider, streamErr)
		if emitErr := emit.finishWithError(failure); emitErr != nil {
			return e.finalize(report, failure, emitErr)
		}
		return e.finalize(report, failure, nil)
	}
	return e.finalize(report, nil, nil)
}

// finalize validates the usage report before returning it.
//
// A report Oxy's parse rejects is not a lost log line: it is the record
// settlement runs against, so emitting an invalid one produces a request that
// executed, cost money upstream, and can never be settled or refunded. When the
// report is unusable the executor says so rather than handing back something
// that looks fine.
func (e *Executor) finalize(report *contract.UsageReport, failure *contract.Error, sinkErr error) Result {
	if err := report.Validate(); err != nil {
		return Result{
			Failure: contract.NewError(report.RequestID, contract.CodeInternalError,
				fmt.Sprintf("the usage report for this request is not settleable: %v", err)),
		}
	}
	if sinkErr != nil && failure == nil {
		// The events could not be delivered. The work still happened and is
		// still owed, so the report stands; only the stream is lost.
		failure = contract.NewError(report.RequestID, contract.CodeCancelled, "the event stream could not be delivered")
	}
	return Result{Report: report, Failure: failure}
}

// resolve turns the envelope's target into a concrete route and adapter.
func (e *Executor) resolve(request *contract.Request) (provider.Route, provider.Adapter, *contract.Error) {
	requestID := request.Attribution.RequestID

	if err := request.Validate(); err != nil {
		return provider.Route{}, nil, contract.NewError(requestID, contract.CodeInvalidRequest, err.Error())
	}
	if !request.Attribution.HasScope(contract.ScopeInvoke) {
		// Not a customer authorization decision — the edge already made that.
		// An envelope arriving without the invoke scope is a malformed
		// instruction from the edge, and serving it would mean Relay deciding
		// something it has no basis to decide.
		return provider.Route{}, nil, contract.NewError(requestID, contract.CodeInsufficientScope,
			"the envelope does not carry inference:invoke")
	}

	if request.Target.Kind == contract.TargetRoutingProfile {
		// Resolving a profile needs its candidate list, which lives in the Oxy
		// catalogue and is not in the envelope: it carries a routing policy
		// REFERENCE, not a snapshot. Choosing a model here would be exactly the
		// silent substitution the platform forbids, so the request is refused
		// with the field named. See README, "What Oxy still has to decide".
		return provider.Route{}, nil, contract.NewError(requestID, contract.CodeInvalidRequest,
			"this build serves concrete model targets only: the envelope carries a routing policy reference, not the snapshot a profile would have to be resolved against",
		).WithParam("target.routingProfile")
	}

	route, err := e.inventory.Resolve(*request.Target.ModelReference)
	if err != nil {
		var noRoute inventory.ErrNoRoute
		if errors.As(err, &noRoute) {
			return provider.Route{}, nil, contract.NewError(requestID, contract.CodeModelNotFound,
				fmt.Sprintf("no deployment serves %q", noRoute.Reference)).WithParam("target.modelReference")
		}
		return provider.Route{}, nil, contract.NewError(requestID, contract.CodeInternalError, err.Error())
	}

	adapter, found := e.registry.Lookup(route.Provider)
	if !found {
		// The inventory routes somewhere this process cannot reach. The server
		// refuses to start in this state, so reaching it means the inventory
		// changed under a running process.
		return provider.Route{}, nil, contract.NewError(requestID, contract.CodeDeploymentUnavailable,
			fmt.Sprintf("no adapter is loaded for provider %q", route.Provider))
	}
	return route, adapter, nil
}

// generationID allocates the id for this generation.
//
// Relay allocates it; Oxy allocates requestId. The contract's identifiers
// module says the data plane generates both, but requestId is REQUIRED on the
// inbound envelope, so it cannot be. See README, "What Oxy still has to decide".
func (e *Executor) generationID(request *contract.Request) *contract.GenerationID {
	if existing := request.Attribution.GenerationID; existing != nil && *existing != "" {
		return existing
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		// A generation id is a correlation handle, not a security boundary. If
		// the system's entropy source is failing, the request is in far more
		// trouble than an unnamed generation, and refusing here would turn a
		// degraded host into a total outage.
		return nil
	}
	id := contract.GenerationID("gen_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(entropy[:])))
	return &id
}

func withGeneration(attribution contract.Attribution, generationID *contract.GenerationID) contract.Attribution {
	attribution.GenerationID = generationID
	return attribution
}

func finishReasonOr(reason, fallback contract.FinishReason) contract.FinishReason {
	if reason == "" {
		return fallback
	}
	return reason
}

// isCancellation distinguishes the customer withdrawing from the provider
// failing. They settle differently — one is a refund reason, the other is an
// upstream failure — so conflating them would put the wrong reason on a
// reversal.
func isCancellation(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, ErrClientGone) ||
		errors.Is(ctx.Err(), context.Canceled)
}

// translationFailure maps an adapter's refusal to translate.
func translationFailure(requestID contract.RequestID, err error) *contract.Error {
	var unsupported provider.ErrUnsupported
	if errors.As(err, &unsupported) {
		failure := contract.NewError(requestID, unsupported.Code, unsupported.Detail)
		if unsupported.Param != "" {
			failure = failure.WithParam(unsupported.Param)
		}
		return failure
	}
	return contract.NewError(requestID, contract.CodeInvalidRequest, err.Error())
}

// upstreamFailure maps an adapter's execution failure. An adapter that
// classified the failure itself is believed; anything else is an unclassified
// provider error, which is non-retryable by the contract's rule that a failure
// nobody can classify is not safe to retry.
func upstreamFailure(requestID contract.RequestID, slug contract.ProviderSlug, err error) *contract.Error {
	var upstream provider.ErrUpstream
	if errors.As(err, &upstream) {
		failure := contract.NewError(requestID, upstream.Code, upstream.Detail)
		if upstream.RetryAfterMs > 0 {
			failure = failure.WithRetryAfter(upstream.RetryAfterMs)
		}
		return failure.WithUpstream(upstream.Category, upstream.Passthrough)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return contract.NewError(requestID, contract.CodeProviderTimeout,
			fmt.Sprintf("%s did not respond in time", slug)).
			WithUpstream(contract.UpstreamTimeout, nil)
	}
	return contract.NewError(requestID, contract.CodeProviderError, err.Error()).
		WithUpstream(contract.UpstreamUnknown, &contract.ProviderErrorPassthrough{Provider: slug})
}

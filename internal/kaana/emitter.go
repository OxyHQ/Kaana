package kaana

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// Sink receives normalized events in order. Returning an error stops the
// stream, which is how a vanished downstream client cancels the upstream call.
type Sink func(contract.StreamEvent) error

// emitter stamps the framing every stream event carries and enforces the
// ordering rules the contract states in prose.
//
// Adapters never touch requestId, sequence or schemaVersion, and never decide
// terminality. That is not tidiness: it removes the class of bug where one
// provider's events arrive unattributable, repeat a sequence, or follow an
// error with a done — none of which any adapter's own tests would catch,
// because each adapter would be internally consistent with itself.
type emitter struct {
	sink         Sink
	requestID    contract.RequestID
	generationID *contract.GenerationID
	// provider is the deployment currently being attempted. It changes when the
	// executor fails over, so that the start event names the provider that
	// actually answered rather than the first one tried.
	provider   contract.ProviderSlug
	deployment contract.DeploymentID
	sequence   int
	// started means the customer-visible start event has been WRITTEN: the
	// request is committed to the attempt that wrote it and can no longer move.
	// It becomes true at the attempt's first delivered output (or at done, for
	// an answer with no output), never at the adapter's Start call — see
	// "Deferred start" on Start.
	started bool
	// attemptStarted means the attempt being served has called Start. It is
	// what the adapter-facing ordering rules check, and it resets with serving.
	attemptStarted bool
	// pendingStart and pending hold what the current attempt reported before
	// it delivered anything: its start and any usage or empty delta events. They
	// are written, in order and with fresh sequence numbers, when the attempt
	// commits, and discarded unwritten when it fails first.
	pendingStart  *contract.StreamStartEvent
	pending       []contract.StreamEvent
	terminated    bool
	sinkFailed    bool
	firstOutputAt time.Time
	// attemptOutputAt is when the attempt currently being served produced its
	// first output. serving resets it, so a failover measures its own attempt
	// rather than inheriting the one it replaced.
	attemptOutputAt time.Time
	// admittedAt is when Kaana began executing, NOT when the upstream started
	// answering. Time to first token measured from the upstream's response
	// headers would exclude connection and queueing time, which is most of what
	// the number exists to expose.
	admittedAt time.Time
	estimate   *usageEstimate
}

func newEmitter(sink Sink, request *contract.Request, generationID *contract.GenerationID, admittedAt time.Time) *emitter {
	return &emitter{
		sink:         sink,
		requestID:    request.Attribution.RequestID,
		generationID: generationID,
		admittedAt:   admittedAt,
		estimate:     newUsageEstimate(request),
	}
}

// serving names the deployment about to be attempted, and discards anything a
// previous attempt reported without delivering: that attempt did not serve, so
// its start must never reach the customer.
func (e *emitter) serving(slug contract.ProviderSlug, deployment contract.DeploymentID) {
	e.provider = slug
	e.deployment = deployment
	e.attemptOutputAt = time.Time{}
	e.attemptStarted = false
	e.pendingStart = nil
	e.pending = nil
}

// markOutput records the first output of the request and of the attempt.
func (e *emitter) markOutput() {
	now := time.Now()
	if e.firstOutputAt.IsZero() {
		e.firstOutputAt = now
	}
	if e.attemptOutputAt.IsZero() {
		e.attemptOutputAt = now
	}
}

func (e *emitter) next() int {
	sequence := e.sequence
	e.sequence++
	return sequence
}

func (e *emitter) send(event contract.StreamEvent) error {
	if e.terminated {
		return fmt.Errorf("kaana: %s event follows a terminal event", event.EventType())
	}
	err := e.sink(event)
	if err != nil {
		e.sinkFailed = true
	}
	return err
}

// sendOrHold writes a non-output event once the stream is committed, and holds
// it behind the pending start until then. Its sequence number is stamped when
// it is written, so a discarded attempt leaves no gap in the sequence.
func (e *emitter) sendOrHold(event contract.StreamEvent) error {
	if e.started {
		stampSequence(event, e.next())
		return e.send(event)
	}
	e.pending = append(e.pending, event)
	return nil
}

// commit writes the held start event and everything held behind it. It runs
// before the attempt's first delivered output and before done, and it is the
// moment the request stops being movable.
func (e *emitter) commit() error {
	if e.started {
		return nil
	}
	if e.pendingStart == nil {
		// requireStarted runs first on every caller; this is the invariant, not
		// a path an adapter can reach.
		return fmt.Errorf("kaana: an attempt committed without a start event")
	}
	e.started = true
	start, held := e.pendingStart, e.pending
	e.pendingStart, e.pending = nil, nil
	start.Seq = e.next()
	if err := e.send(start); err != nil {
		return err
	}
	for _, event := range held {
		stampSequence(event, e.next())
		if err := e.send(event); err != nil {
			return err
		}
	}
	return nil
}

// stampSequence sets the sequence of an event that may have been held. Only
// the kinds sendOrHold is given can be held.
func stampSequence(event contract.StreamEvent, sequence int) {
	switch held := event.(type) {
	case *contract.StreamUsageEvent:
		held.Seq = sequence
	case *contract.StreamDeltaEvent:
		held.Seq = sequence
	}
}

// Start implements provider.Emitter.
//
// # Deferred start
//
// The start event is validated here and HELD, not written. It is written when
// the attempt delivers its first output — a non-empty delta, a tool call, audio
// — or completes without any (done). Until then nothing the customer can act on
// exists, so the request can still be retried on the same deployment or moved
// to the next authorized one: an upstream that answers 200 and then reports a
// rate limit inside the stream has delivered nothing, and having had its
// headers read must not pin the request to it. The customer still sees start
// first, exactly once, naming the route that actually served.
func (e *emitter) Start(resolved contract.ModelReference, at time.Time) error {
	if e.started || e.attemptStarted {
		return fmt.Errorf("kaana: an adapter emitted a second start event")
	}
	if !resolved.Pinned() {
		// The contract requires the start event to name a revision-pinned
		// reference. An unpinned one here means the route resolution reported
		// a model line rather than the weights that answered, and a customer
		// would be told less than the contract promises.
		return fmt.Errorf("kaana: the resolved model reference %q is not revision-pinned", resolved)
	}
	e.attemptStarted = true
	e.pendingStart = &contract.StreamStartEvent{
		SchemaVersion:          contract.SchemaVersion,
		Type:                   contract.EventStart,
		RequestID:              e.requestID,
		GenerationID:           e.generationID,
		ResolvedModelReference: resolved,
		ServingProvider:        e.provider,
		StartedAt:              contract.NewTimestamp(at),
	}
	return nil
}

// Delta implements provider.Emitter.
func (e *emitter) Delta(outputIndex int, channel contract.DeltaChannel, text string) error {
	if err := e.requireStarted(contract.EventDelta); err != nil {
		return err
	}
	if outputIndex < 0 {
		return fmt.Errorf("kaana: output index %d is negative", outputIndex)
	}
	event := &contract.StreamDeltaEvent{
		SchemaVersion: contract.SchemaVersion,
		Type:          contract.EventDelta,
		RequestID:     e.requestID,
		OutputIndex:   outputIndex,
		Channel:       channel,
		Text:          text,
	}
	if text == "" {
		// An empty delta delivers nothing, so it does not commit the request.
		return e.sendOrHold(event)
	}
	e.markOutput()
	if err := e.commit(); err != nil {
		return err
	}
	event.Seq = e.next()
	err := e.send(event)
	if err == nil {
		e.estimate.addDelta(channel, text)
	}
	return err
}

// ToolCall implements provider.Emitter.
func (e *emitter) ToolCall(call provider.ToolCallDelta) error {
	if err := e.requireStarted(contract.EventToolCall); err != nil {
		return err
	}
	if call.ID == "" {
		return fmt.Errorf("kaana: a tool-call event carries no tool call id")
	}
	// Every tool-call event is model output the customer can act on, including
	// the id-only opening and closing frames, so each one commits.
	if err := e.commit(); err != nil {
		return err
	}
	event := &contract.StreamToolCallEvent{
		SchemaVersion: contract.SchemaVersion,
		Type:          contract.EventToolCall,
		RequestID:     e.requestID,
		Seq:           e.next(),
		ToolCallID:    call.ID,
		Complete:      call.Complete,
	}
	if call.Name != "" {
		event.Name = &call.Name
	}
	if call.ArgumentsDelta != "" {
		event.ArgumentsDelta = &call.ArgumentsDelta
	}
	err := e.send(event)
	if err == nil {
		e.estimate.addToolCall(call.Name, call.ArgumentsDelta)
	}
	return err
}

func (e *emitter) hasDeliveredOutput() bool { return e.estimate.hasDeliveredOutput() }

// Usage implements provider.Emitter.
func (e *emitter) Usage(units []contract.UsageQuantity, source contract.UsageSource) error {
	if err := e.requireStarted(contract.EventUsage); err != nil {
		return err
	}
	if len(units) == 0 {
		// The contract requires at least one unit. An empty usage event is not
		// "no usage yet" — it is an unparseable message, and Oxy would drop the
		// whole stream frame rather than the empty list.
		return fmt.Errorf("kaana: a usage event must carry at least one unit")
	}
	// Usage is progress, not output: it is held with the start until the
	// attempt delivers something. An attempt that fails first keeps its units
	// in its Outcome, and so in the operator cost record, without telling the
	// customer about work that never reached them.
	return e.sendOrHold(&contract.StreamUsageEvent{
		SchemaVersion: contract.StreamUsageEventSchemaVersion,
		Type:          contract.EventUsage,
		RequestID:     e.requestID,
		DeploymentID:  e.deployment,
		Units:         append([]contract.UsageQuantity(nil), units...),
		UsageSource:   source,
	})
}

// errSwitchTooLate reports that the stream has already committed — output
// reached the customer — so the request can no longer be moved.
//
// It is a value rather than a formatted error because the executor's failover
// loop matches on it: this is the difference between "there is nowhere left to
// go" — an ordinary outcome — and a stream that could not be written to, which
// is a delivery failure.
var errSwitchTooLate = errors.New("kaana: a route switch follows the stream's start event; retrying now would duplicate output the customer already has")

// routeSwitch reports that an attempt failed and the request is being retried
// on the next route Oxy authorized.
//
// Two rules are enforced here rather than by the caller, because both are the
// kind that a future change would breach without noticing:
//
//  1. **It refuses once the stream has committed.** A switch after output has
//     begun would re-run a request whose first tokens the customer already has,
//     and the second attempt would emit a second start event describing a
//     stream that had already been described. So failover is possible exactly
//     while nothing has been delivered — the failed attempt's own start was
//     held, never written — and that is why this event PRECEDES the start
//     event rather than amending it: the switch really did happen before
//     anything was streamed, and saying so in order is the honest framing. The
//     contract specifies event shapes and not their order; see README.
//
//  2. **It can report a model switch only with the signed list's primary model
//     line.** The executor supplies that value after resolving every list entry
//     against inventory. A switch between two revisions of one line is refused:
//     neither the deployment-scoped nor model-scoped contract shape can report
//     that truthfully.
func (e *emitter) routeSwitch(
	reason contract.RouteSwitchReason,
	requestedModelID contract.ModelID,
	from, to provider.Route,
	at time.Time,
) error {
	if e.started {
		return errSwitchTooLate
	}
	if from.ModelReference == to.ModelReference {
		reference := to.ModelReference
		deployment := to.DeploymentID
		return e.send(&contract.StreamRouteSwitchEvent{
			SchemaVersion: contract.SchemaVersion,
			Type:          contract.EventRouteSwitch,
			RequestID:     e.requestID,
			Seq:           e.next(),
			Reason:        reason,
			Detail: contract.RouteSwitchDetail{
				Scope:          contract.SwitchScopeDeployment,
				ToProvider:     to.Provider,
				ModelReference: &reference,
				ToDeploymentID: &deployment,
			},
			OccurredAt: contract.NewTimestamp(at),
		})
	}
	if from.ModelReference.ModelID() == to.ModelReference.ModelID() {
		return fmt.Errorf("kaana: a route switch from %q to %q changes revision inside one model line, which the stream contract cannot report truthfully",
			from.ModelReference, to.ModelReference)
	}
	if !requestedModelID.Valid() {
		return fmt.Errorf("kaana: a cross-model route switch has no valid primary model line")
	}
	authorized := true
	fromReference := from.ModelReference
	toReference := to.ModelReference
	return e.send(&contract.StreamRouteSwitchEvent{
		SchemaVersion: contract.SchemaVersion,
		Type:          contract.EventRouteSwitch,
		RequestID:     e.requestID,
		Seq:           e.next(),
		Reason:        reason,
		Detail: contract.RouteSwitchDetail{
			Scope:              contract.SwitchScopeModel,
			ToProvider:         to.Provider,
			RequestedModelID:   &requestedModelID,
			FromModelReference: &fromReference,
			ToModelReference:   &toReference,
			AuthorizedByPolicy: &authorized,
		},
		OccurredAt: contract.NewTimestamp(at),
	})
}

// finishWithDone emits the successful terminal event.
func (e *emitter) finishWithDone(reason contract.FinishReason, at time.Time) error {
	if err := e.requireStarted(contract.EventDone); err != nil {
		return err
	}
	// An answer with no output commits here: the attempt completed, so it is
	// the one that served, and its start and usage precede done.
	if err := e.commit(); err != nil {
		return err
	}
	event := &contract.StreamDoneEvent{
		SchemaVersion: contract.SchemaVersion,
		Type:          contract.EventDone,
		RequestID:     e.requestID,
		Seq:           e.next(),
		GenerationID:  e.generationID,
		FinishReason:  reason,
		CompletedAt:   contract.NewTimestamp(at),
	}
	err := e.send(event)
	e.terminated = true
	return err
}

// finishWithError emits the terminal error event.
//
// It does not require a prior start: a request that failed during translation
// or route resolution never started, and the customer still needs the error.
// Nor does it write a held start: an attempt that failed before delivering
// anything did not serve, and the customer is told only that the request
// failed.
func (e *emitter) finishWithError(failure *contract.Error) error {
	if e.terminated {
		return fmt.Errorf("kaana: an error event follows a terminal event")
	}
	event := &contract.StreamErrorEvent{
		SchemaVersion: contract.SchemaVersion,
		Type:          contract.EventError,
		RequestID:     e.requestID,
		Seq:           e.next(),
		Error:         *failure,
	}
	err := e.send(event)
	e.terminated = true
	return err
}

func (e *emitter) requireStarted(kind contract.StreamEventType) error {
	if !e.attemptStarted {
		return fmt.Errorf("kaana: a %s event precedes the stream's start event", kind)
	}
	return nil
}

// timeToFirstToken reports how long the first non-empty output took, measured
// from the moment Kaana admitted the request. Zero when no output was produced.
func (e *emitter) timeToFirstToken() time.Duration {
	if e.firstOutputAt.IsZero() || e.admittedAt.IsZero() {
		return 0
	}
	return e.firstOutputAt.Sub(e.admittedAt)
}

// Audio stamps one bounded audio event and records only delivery, never content.
func (e *emitter) Audio(outputIndex int, mediaType string, data []byte) error {
	if err := e.requireStarted(contract.EventAudio); err != nil {
		return err
	}
	if outputIndex < 0 || len(data) == 0 || len(data) > provider.MaxAudioChunkBytes {
		return fmt.Errorf("kaana: invalid audio index or chunk size")
	}
	switch mediaType {
	case "audio/mpeg", "audio/wav", "audio/ogg", "audio/aac", "audio/flac", "audio/pcm":
	default:
		return fmt.Errorf("kaana: unsupported audio media type")
	}
	e.markOutput()
	if err := e.commit(); err != nil {
		return err
	}
	err := e.send(&contract.StreamAudioEvent{SchemaVersion: contract.SchemaVersion,
		Type: contract.EventAudio, RequestID: e.requestID, Seq: e.next(), OutputIndex: outputIndex,
		MediaType: contract.AudioMediaType(mediaType), Data: base64.StdEncoding.EncodeToString(data)})
	if err == nil {
		e.estimate.outputDelivered = true
	}
	return err
}

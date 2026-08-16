package relay

import (
	"fmt"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/provider"
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
	sink          Sink
	requestID     contract.RequestID
	generationID  *contract.GenerationID
	provider      contract.ProviderSlug
	sequence      int
	started       bool
	terminated    bool
	firstOutputAt time.Time
	// admittedAt is when Relay began executing, NOT when the upstream started
	// answering. Time to first token measured from the upstream's response
	// headers would exclude connection and queueing time, which is most of what
	// the number exists to expose.
	admittedAt time.Time
}

func newEmitter(sink Sink, requestID contract.RequestID, generationID *contract.GenerationID, slug contract.ProviderSlug, admittedAt time.Time) *emitter {
	return &emitter{
		sink:         sink,
		requestID:    requestID,
		generationID: generationID,
		provider:     slug,
		admittedAt:   admittedAt,
	}
}

func (e *emitter) next() int {
	sequence := e.sequence
	e.sequence++
	return sequence
}

func (e *emitter) send(event contract.StreamEvent) error {
	if e.terminated {
		return fmt.Errorf("relay: %s event follows a terminal event", event.EventType())
	}
	return e.sink(event)
}

// Start implements provider.Emitter.
func (e *emitter) Start(resolved contract.ModelReference, at time.Time) error {
	if e.started {
		return fmt.Errorf("relay: an adapter emitted a second start event")
	}
	if !resolved.Pinned() {
		// The contract requires the start event to name a revision-pinned
		// reference. An unpinned one here means the route resolution reported
		// a model line rather than the weights that answered, and a customer
		// would be told less than the contract promises.
		return fmt.Errorf("relay: the resolved model reference %q is not revision-pinned", resolved)
	}
	e.started = true
	return e.send(&contract.StreamStartEvent{
		SchemaVersion:          contract.SchemaVersion,
		Type:                   contract.EventStart,
		RequestID:              e.requestID,
		Seq:                    e.next(),
		GenerationID:           e.generationID,
		ResolvedModelReference: resolved,
		ServingProvider:        e.provider,
		StartedAt:              contract.NewTimestamp(at),
	})
}

// Delta implements provider.Emitter.
func (e *emitter) Delta(outputIndex int, channel contract.DeltaChannel, text string) error {
	if err := e.requireStarted(contract.EventDelta); err != nil {
		return err
	}
	if outputIndex < 0 {
		return fmt.Errorf("relay: output index %d is negative", outputIndex)
	}
	if e.firstOutputAt.IsZero() && text != "" {
		e.firstOutputAt = time.Now()
	}
	return e.send(&contract.StreamDeltaEvent{
		SchemaVersion: contract.SchemaVersion,
		Type:          contract.EventDelta,
		RequestID:     e.requestID,
		Seq:           e.next(),
		OutputIndex:   outputIndex,
		Channel:       channel,
		Text:          text,
	})
}

// ToolCall implements provider.Emitter.
func (e *emitter) ToolCall(call provider.ToolCallDelta) error {
	if err := e.requireStarted(contract.EventToolCall); err != nil {
		return err
	}
	if call.ID == "" {
		return fmt.Errorf("relay: a tool-call event carries no tool call id")
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
	return e.send(event)
}

// Usage implements provider.Emitter.
func (e *emitter) Usage(units []contract.UsageQuantity, source contract.UsageSource) error {
	if err := e.requireStarted(contract.EventUsage); err != nil {
		return err
	}
	if len(units) == 0 {
		// The contract requires at least one unit. An empty usage event is not
		// "no usage yet" — it is an unparseable message, and Oxy would drop the
		// whole stream frame rather than the empty list.
		return fmt.Errorf("relay: a usage event must carry at least one unit")
	}
	return e.send(&contract.StreamUsageEvent{
		SchemaVersion: contract.SchemaVersion,
		Type:          contract.EventUsage,
		RequestID:     e.requestID,
		Seq:           e.next(),
		Units:         units,
		UsageSource:   source,
	})
}

// finishWithDone emits the successful terminal event.
func (e *emitter) finishWithDone(reason contract.FinishReason, at time.Time) error {
	if err := e.requireStarted(contract.EventDone); err != nil {
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
func (e *emitter) finishWithError(failure *contract.Error) error {
	if e.terminated {
		return fmt.Errorf("relay: an error event follows a terminal event")
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
	if !e.started {
		return fmt.Errorf("relay: a %s event precedes the stream's start event", kind)
	}
	return nil
}

// timeToFirstToken reports how long the first non-empty output took, measured
// from the moment Relay admitted the request. Zero when no output was produced.
func (e *emitter) timeToFirstToken() time.Duration {
	if e.firstOutputAt.IsZero() || e.admittedAt.IsZero() {
		return 0
	}
	return e.firstOutputAt.Sub(e.admittedAt)
}

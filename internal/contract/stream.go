package contract

// The stream is one discriminated union of seven whole messages, each carrying
// its own schemaVersion, requestId and monotonic sequence.
//
// Unlike the request's inline unions, this one's variants are named shapes in
// the contract, so they are named types here too and StreamEvent is the
// interface they satisfy. A consumer meeting an unknown event fails at the
// parse instead of falling into a default branch that treats it as output; the
// same rule applies on this side, which is why there is no generic event type.

// StreamEventType discriminates a normalized stream event.
type StreamEventType string

const (
	EventStart       StreamEventType = "start"
	EventDelta       StreamEventType = "delta"
	EventToolCall    StreamEventType = "tool_call"
	EventUsage       StreamEventType = "usage"
	EventRouteSwitch StreamEventType = "route_switch"
	EventError       StreamEventType = "error"
	EventDone        StreamEventType = "done"
)

var streamEventTypeValues = []StreamEventType{
	EventStart, EventDelta, EventToolCall, EventUsage, EventRouteSwitch, EventError, EventDone,
}

// StreamEvent is any event a normalized stream can carry.
type StreamEvent interface {
	// EventType is the value of the event's `type` discriminator.
	EventType() StreamEventType
	// Sequence is the event's position in the stream. Terminality and ordering
	// are the emitter's to enforce, and it reads this to enforce them.
	Sequence() int
}

// UsageQuantity is a metered quantity of one unit. Never money: a quantity
// carries no price and no currency, so a consumer cannot mistake a token count
// for an amount owed.
type UsageQuantity struct {
	Unit     UsageUnit `json:"unit"`
	Quantity int       `json:"quantity"`
}

// StreamStartEvent is the first event of every stream: what was actually
// resolved. It carries no deployment health, no route id and no upstream cost.
type StreamStartEvent struct {
	SchemaVersion          int             `json:"schemaVersion"`
	Type                   StreamEventType `json:"type"`
	RequestID              RequestID       `json:"requestId"`
	Seq                    int             `json:"sequence"`
	GenerationID           *GenerationID   `json:"generationId,omitempty"`
	ResolvedModelReference ModelReference  `json:"resolvedModelReference"`
	ServingProvider        ProviderSlug    `json:"servingProvider"`
	StartedAt              Timestamp       `json:"startedAt"`
}

func (e *StreamStartEvent) EventType() StreamEventType { return EventStart }
func (e *StreamStartEvent) Sequence() int              { return e.Seq }

// StreamDeltaEvent is a chunk of output.
type StreamDeltaEvent struct {
	SchemaVersion int             `json:"schemaVersion"`
	Type          StreamEventType `json:"type"`
	RequestID     RequestID       `json:"requestId"`
	Seq           int             `json:"sequence"`
	OutputIndex   int             `json:"outputIndex"`
	Channel       DeltaChannel    `json:"channel"`
	Text          string          `json:"text"`
}

func (e *StreamDeltaEvent) EventType() StreamEventType { return EventDelta }
func (e *StreamDeltaEvent) Sequence() int              { return e.Seq }

// StreamToolCallEvent is a tool call being streamed. ArgumentsDelta
// accumulates; Complete marks the call finished so a client knows when the
// accumulated JSON text is worth parsing.
type StreamToolCallEvent struct {
	SchemaVersion  int             `json:"schemaVersion"`
	Type           StreamEventType `json:"type"`
	RequestID      RequestID       `json:"requestId"`
	Seq            int             `json:"sequence"`
	ToolCallID     string          `json:"toolCallId"`
	Name           *string         `json:"name,omitempty"`
	ArgumentsDelta *string         `json:"argumentsDelta,omitempty"`
	Complete       bool            `json:"complete"`
}

func (e *StreamToolCallEvent) EventType() StreamEventType { return EventToolCall }
func (e *StreamToolCallEvent) Sequence() int              { return e.Seq }

// StreamUsageEvent reports metered units for the request so far. Units only —
// what a customer is charged is the ledger's answer, and a cost quoted by the
// data plane would be a second, unauthoritative answer to the same question.
type StreamUsageEvent struct {
	SchemaVersion int             `json:"schemaVersion"`
	Type          StreamEventType `json:"type"`
	RequestID     RequestID       `json:"requestId"`
	Seq           int             `json:"sequence"`
	Units         []UsageQuantity `json:"units"`
	UsageSource   UsageSource     `json:"usageSource"`
}

func (e *StreamUsageEvent) EventType() StreamEventType { return EventUsage }
func (e *StreamUsageEvent) Sequence() int              { return e.Seq }

// RouteSwitchScope discriminates what kind of re-route happened.
type RouteSwitchScope string

const (
	// SwitchScopeDeployment is same-model failover: the same revision, served
	// somewhere else.
	SwitchScopeDeployment RouteSwitchScope = "deployment"
	// SwitchScopeModel is a substitution, expressible only with
	// AuthorizedByPolicy true.
	SwitchScopeModel RouteSwitchScope = "model"
)

var routeSwitchScopeValues = []RouteSwitchScope{SwitchScopeDeployment, SwitchScopeModel}

// RouteSwitchDetail describes a re-route.
//
// RequestedModelID is the UNPINNED model line the customer asked for. A request
// that pinned a revision asked for exactly those weights and is served or
// refused, never substituted — so for such a request there is no value that
// satisfies the field, and a model-scope switch cannot be constructed at all.
type RouteSwitchDetail struct {
	Scope              RouteSwitchScope `json:"scope"`
	ToProvider         ProviderSlug     `json:"toProvider"`
	ModelReference     *ModelReference  `json:"modelReference,omitempty"`
	ToDeploymentID     *DeploymentID    `json:"toDeploymentId,omitempty"`
	RequestedModelID   *ModelID         `json:"requestedModelId,omitempty"`
	FromModelReference *ModelReference  `json:"fromModelReference,omitempty"`
	ToModelReference   *ModelReference  `json:"toModelReference,omitempty"`
	AuthorizedByPolicy *bool            `json:"authorizedByPolicy,omitempty"`
}

// StreamRouteSwitchEvent is the customer-visible notice that an allowed route
// switch occurred, emitted in-stream rather than only on the receipt.
type StreamRouteSwitchEvent struct {
	SchemaVersion int               `json:"schemaVersion"`
	Type          StreamEventType   `json:"type"`
	RequestID     RequestID         `json:"requestId"`
	Seq           int               `json:"sequence"`
	Reason        RouteSwitchReason `json:"reason"`
	Detail        RouteSwitchDetail `json:"detail"`
	OccurredAt    Timestamp         `json:"occurredAt"`
}

func (e *StreamRouteSwitchEvent) EventType() StreamEventType { return EventRouteSwitch }
func (e *StreamRouteSwitchEvent) Sequence() int              { return e.Seq }

// StreamErrorEvent is a terminal error. The stream ends here; no done event
// follows, so a client that saw an error never also has to reconcile a success.
type StreamErrorEvent struct {
	SchemaVersion int             `json:"schemaVersion"`
	Type          StreamEventType `json:"type"`
	RequestID     RequestID       `json:"requestId"`
	Seq           int             `json:"sequence"`
	Error         Error           `json:"error"`
}

func (e *StreamErrorEvent) EventType() StreamEventType { return EventError }
func (e *StreamErrorEvent) Sequence() int              { return e.Seq }

// StreamDoneEvent is the successful terminal event.
//
// ReceiptID is present once settlement has produced one. Pensara never sets it:
// settlement is the control plane's, and a data plane that filled the field in
// would be quoting a charge it did not compute.
type StreamDoneEvent struct {
	SchemaVersion int             `json:"schemaVersion"`
	Type          StreamEventType `json:"type"`
	RequestID     RequestID       `json:"requestId"`
	Seq           int             `json:"sequence"`
	GenerationID  *GenerationID   `json:"generationId,omitempty"`
	FinishReason  FinishReason    `json:"finishReason"`
	ReceiptID     *string         `json:"receiptId,omitempty"`
	CompletedAt   Timestamp       `json:"completedAt"`
}

func (e *StreamDoneEvent) EventType() StreamEventType { return EventDone }
func (e *StreamDoneEvent) Sequence() int              { return e.Seq }

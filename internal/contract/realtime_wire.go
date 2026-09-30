// Code in this file mirrors the realtime session family of @oxy.so/contracts
// (contract set 3.2.0). See descriptor_test.go for the gate that holds it to
// the published shapes.

package contract

/* -------------------------------------------------------------------------- */
/*  Client commands                                                           */
/* -------------------------------------------------------------------------- */

// Every command carries the session's request id and a client-chosen command
// id, unique within the session. The server acknowledges a command before it
// applies it, and a command id it has already accepted is never applied again.

// RealtimeCommandType is the discriminator of a client command.
type RealtimeCommandType string

const (
	RealtimeCommandSessionUpdateType    RealtimeCommandType = "session.update"
	RealtimeCommandItemCreateType       RealtimeCommandType = "conversation.item.create"
	RealtimeCommandItemDeleteType       RealtimeCommandType = "conversation.item.delete"
	RealtimeCommandItemTruncateType     RealtimeCommandType = "conversation.item.truncate"
	RealtimeCommandInputAudioAppendType RealtimeCommandType = "input_audio.append"
	RealtimeCommandInputAudioCommitType RealtimeCommandType = "input_audio.commit"
	RealtimeCommandInputAudioClearType  RealtimeCommandType = "input_audio.clear"
	RealtimeCommandResponseCreateType   RealtimeCommandType = "response.create"
	RealtimeCommandResponseCancelType   RealtimeCommandType = "response.cancel"
	RealtimeCommandSessionResumeType    RealtimeCommandType = "session.resume"
	RealtimeCommandSessionCloseType     RealtimeCommandType = "session.close"
)

var realtimeCommandTypeValues = []RealtimeCommandType{
	RealtimeCommandSessionUpdateType,
	RealtimeCommandItemCreateType,
	RealtimeCommandItemDeleteType,
	RealtimeCommandItemTruncateType,
	RealtimeCommandInputAudioAppendType,
	RealtimeCommandInputAudioCommitType,
	RealtimeCommandInputAudioClearType,
	RealtimeCommandResponseCreateType,
	RealtimeCommandResponseCancelType,
	RealtimeCommandSessionResumeType,
	RealtimeCommandSessionCloseType,
}

// RealtimeCommand is one decoded client command.
type RealtimeCommand interface {
	CommandType() RealtimeCommandType
	Identity() (RequestID, RealtimeCommandID)
}

type RealtimeSessionUpdateCommand struct {
	SchemaVersion int                         `json:"schemaVersion"`
	RequestID     RequestID                   `json:"requestId"`
	CommandID     RealtimeCommandID           `json:"commandId"`
	Type          string                      `json:"type"`
	Config        RealtimeSessionConfigUpdate `json:"config"`
}

func (*RealtimeSessionUpdateCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandSessionUpdateType
}
func (c *RealtimeSessionUpdateCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeItemCreateCommand struct {
	SchemaVersion  int                      `json:"schemaVersion"`
	RequestID      RequestID                `json:"requestId"`
	CommandID      RealtimeCommandID        `json:"commandId"`
	Type           string                   `json:"type"`
	PreviousItemID *RealtimeItemID          `json:"previousItemId,omitempty"`
	Item           RealtimeConversationItem `json:"item"`
}

func (*RealtimeItemCreateCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandItemCreateType
}
func (c *RealtimeItemCreateCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeItemDeleteCommand struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     RequestID         `json:"requestId"`
	CommandID     RealtimeCommandID `json:"commandId"`
	Type          string            `json:"type"`
	ItemID        RealtimeItemID    `json:"itemId"`
}

func (*RealtimeItemDeleteCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandItemDeleteType
}
func (c *RealtimeItemDeleteCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeItemTruncateCommand struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     RequestID         `json:"requestId"`
	CommandID     RealtimeCommandID `json:"commandId"`
	Type          string            `json:"type"`
	ItemID        RealtimeItemID    `json:"itemId"`
	ContentIndex  int               `json:"contentIndex"`
	AudioEndMs    int               `json:"audioEndMs"`
}

func (*RealtimeItemTruncateCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandItemTruncateType
}
func (c *RealtimeItemTruncateCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeInputAudioAppendCommand struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     RequestID         `json:"requestId"`
	CommandID     RealtimeCommandID `json:"commandId"`
	Type          string            `json:"type"`
	Data          string            `json:"data"`
}

func (*RealtimeInputAudioAppendCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandInputAudioAppendType
}
func (c *RealtimeInputAudioAppendCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeInputAudioCommitCommand struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     RequestID         `json:"requestId"`
	CommandID     RealtimeCommandID `json:"commandId"`
	Type          string            `json:"type"`
}

func (*RealtimeInputAudioCommitCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandInputAudioCommitType
}
func (c *RealtimeInputAudioCommitCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeInputAudioClearCommand struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     RequestID         `json:"requestId"`
	CommandID     RealtimeCommandID `json:"commandId"`
	Type          string            `json:"type"`
}

func (*RealtimeInputAudioClearCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandInputAudioClearType
}
func (c *RealtimeInputAudioClearCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeResponseCreateCommand struct {
	SchemaVersion int                         `json:"schemaVersion"`
	RequestID     RequestID                   `json:"requestId"`
	CommandID     RealtimeCommandID           `json:"commandId"`
	Type          string                      `json:"type"`
	Response      *RealtimeResponseParameters `json:"response,omitempty"`
}

func (*RealtimeResponseCreateCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandResponseCreateType
}
func (c *RealtimeResponseCreateCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeResponseCancelCommand struct {
	SchemaVersion int                 `json:"schemaVersion"`
	RequestID     RequestID           `json:"requestId"`
	CommandID     RealtimeCommandID   `json:"commandId"`
	Type          string              `json:"type"`
	ResponseID    *RealtimeResponseID `json:"responseId,omitempty"`
}

func (*RealtimeResponseCancelCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandResponseCancelType
}
func (c *RealtimeResponseCancelCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeSessionResumeCommand struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     RequestID         `json:"requestId"`
	CommandID     RealtimeCommandID `json:"commandId"`
	Type          string            `json:"type"`
	AfterSequence int               `json:"afterSequence"`
}

func (*RealtimeSessionResumeCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandSessionResumeType
}
func (c *RealtimeSessionResumeCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

type RealtimeSessionCloseCommand struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     RequestID         `json:"requestId"`
	CommandID     RealtimeCommandID `json:"commandId"`
	Type          string            `json:"type"`
}

func (*RealtimeSessionCloseCommand) CommandType() RealtimeCommandType {
	return RealtimeCommandSessionCloseType
}
func (c *RealtimeSessionCloseCommand) Identity() (RequestID, RealtimeCommandID) {
	return c.RequestID, c.CommandID
}

/* -------------------------------------------------------------------------- */
/*  Server events                                                             */
/* -------------------------------------------------------------------------- */

// RealtimeEventType is the discriminator of a server event.
type RealtimeEventType string

const (
	RealtimeEventSessionCreatedType      RealtimeEventType = "session.created"
	RealtimeEventSessionUpdatedType      RealtimeEventType = "session.updated"
	RealtimeEventSessionResumedType      RealtimeEventType = "session.resumed"
	RealtimeEventCommandAcceptedType     RealtimeEventType = "command.accepted"
	RealtimeEventItemAddedType           RealtimeEventType = "conversation.item.added"
	RealtimeEventItemDoneType            RealtimeEventType = "conversation.item.done"
	RealtimeEventItemDeletedType         RealtimeEventType = "conversation.item.deleted"
	RealtimeEventItemTruncatedType       RealtimeEventType = "conversation.item.truncated"
	RealtimeEventSpeechStartedType       RealtimeEventType = "input_audio.speech_started"
	RealtimeEventSpeechStoppedType       RealtimeEventType = "input_audio.speech_stopped"
	RealtimeEventInputAudioCommittedType RealtimeEventType = "input_audio.committed"
	RealtimeEventInputAudioClearedType   RealtimeEventType = "input_audio.cleared"
	RealtimeEventResponseCreatedType     RealtimeEventType = "response.created"
	RealtimeEventOutputAudioDeltaType    RealtimeEventType = "output_audio.delta"
	RealtimeEventOutputAudioDoneType     RealtimeEventType = "output_audio.done"
	RealtimeEventTranscriptDeltaType     RealtimeEventType = "transcript.delta"
	RealtimeEventTranscriptDoneType      RealtimeEventType = "transcript.done"
	RealtimeEventTextDeltaType           RealtimeEventType = "text.delta"
	RealtimeEventToolCallType            RealtimeEventType = "tool_call"
	RealtimeEventResponseDoneType        RealtimeEventType = "response.done"
	RealtimeEventErrorType               RealtimeEventType = "error"
	RealtimeEventSessionClosedType       RealtimeEventType = "session.closed"
)

var realtimeEventTypeValues = []RealtimeEventType{
	RealtimeEventSessionCreatedType,
	RealtimeEventSessionUpdatedType,
	RealtimeEventSessionResumedType,
	RealtimeEventCommandAcceptedType,
	RealtimeEventItemAddedType,
	RealtimeEventItemDoneType,
	RealtimeEventItemDeletedType,
	RealtimeEventItemTruncatedType,
	RealtimeEventSpeechStartedType,
	RealtimeEventSpeechStoppedType,
	RealtimeEventInputAudioCommittedType,
	RealtimeEventInputAudioClearedType,
	RealtimeEventResponseCreatedType,
	RealtimeEventOutputAudioDeltaType,
	RealtimeEventOutputAudioDoneType,
	RealtimeEventTranscriptDeltaType,
	RealtimeEventTranscriptDoneType,
	RealtimeEventTextDeltaType,
	RealtimeEventToolCallType,
	RealtimeEventResponseDoneType,
	RealtimeEventErrorType,
	RealtimeEventSessionClosedType,
}

// RealtimeEventHeader is what every server event carries: the session, its
// position in the session's one monotonic sequence, and the command it
// answers, when it answers one.
type RealtimeEventHeader struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
}

// RealtimeServerEvent is one event the data plane sends a session's client.
// Stamp sets the header and the discriminator; the session numbers events,
// never the code that produced them.
type RealtimeServerEvent interface {
	EventType() RealtimeEventType
	Stamp(header RealtimeEventHeader)
	Header() RealtimeEventHeader
}

type RealtimeSessionCreatedEvent struct {
	SchemaVersion          int                   `json:"schemaVersion"`
	RequestID              RequestID             `json:"requestId"`
	Sequence               int                   `json:"sequence"`
	CommandID              *RealtimeCommandID    `json:"commandId,omitempty"`
	Type                   string                `json:"type"`
	ResolvedModelReference ModelReference        `json:"resolvedModelReference"`
	ServingProvider        ProviderSlug          `json:"servingProvider"`
	DeploymentID           DeploymentID          `json:"deploymentId"`
	Kind                   RealtimeSessionKind   `json:"kind"`
	Config                 RealtimeSessionConfig `json:"config"`
	Limits                 RealtimeSessionLimits `json:"limits"`
	ResumeWindowMs         int                   `json:"resumeWindowMs"`
	StartedAt              Timestamp             `json:"startedAt"`
	ExpiresAt              Timestamp             `json:"expiresAt"`
}

func (*RealtimeSessionCreatedEvent) EventType() RealtimeEventType {
	return RealtimeEventSessionCreatedType
}
func (e *RealtimeSessionCreatedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventSessionCreatedType)
}
func (e *RealtimeSessionCreatedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeSessionUpdatedEvent struct {
	SchemaVersion int                   `json:"schemaVersion"`
	RequestID     RequestID             `json:"requestId"`
	Sequence      int                   `json:"sequence"`
	CommandID     *RealtimeCommandID    `json:"commandId,omitempty"`
	Type          string                `json:"type"`
	Config        RealtimeSessionConfig `json:"config"`
}

func (*RealtimeSessionUpdatedEvent) EventType() RealtimeEventType {
	return RealtimeEventSessionUpdatedType
}
func (e *RealtimeSessionUpdatedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventSessionUpdatedType)
}
func (e *RealtimeSessionUpdatedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeSessionResumedEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	AfterSequence int                `json:"afterSequence"`
}

func (*RealtimeSessionResumedEvent) EventType() RealtimeEventType {
	return RealtimeEventSessionResumedType
}
func (e *RealtimeSessionResumedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventSessionResumedType)
}
func (e *RealtimeSessionResumedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeCommandAcceptedEvent struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     RequestID         `json:"requestId"`
	Sequence      int               `json:"sequence"`
	CommandID     RealtimeCommandID `json:"commandId"`
	Type          string            `json:"type"`
	Duplicate     bool              `json:"duplicate"`
}

func (*RealtimeCommandAcceptedEvent) EventType() RealtimeEventType {
	return RealtimeEventCommandAcceptedType
}
func (e *RealtimeCommandAcceptedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, string(RealtimeEventCommandAcceptedType)
	if h.CommandID != nil {
		e.CommandID = *h.CommandID
	}
}
func (e *RealtimeCommandAcceptedEvent) Header() RealtimeEventHeader {
	commandID := e.CommandID
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: &commandID}
}

type RealtimeItemAddedEvent struct {
	SchemaVersion  int                      `json:"schemaVersion"`
	RequestID      RequestID                `json:"requestId"`
	Sequence       int                      `json:"sequence"`
	CommandID      *RealtimeCommandID       `json:"commandId,omitempty"`
	Type           string                   `json:"type"`
	ItemID         RealtimeItemID           `json:"itemId"`
	PreviousItemID *RealtimeItemID          `json:"previousItemId,omitempty"`
	Item           RealtimeConversationItem `json:"item"`
}

func (*RealtimeItemAddedEvent) EventType() RealtimeEventType { return RealtimeEventItemAddedType }
func (e *RealtimeItemAddedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventItemAddedType)
}
func (e *RealtimeItemAddedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeItemDoneEvent struct {
	SchemaVersion int                      `json:"schemaVersion"`
	RequestID     RequestID                `json:"requestId"`
	Sequence      int                      `json:"sequence"`
	CommandID     *RealtimeCommandID       `json:"commandId,omitempty"`
	Type          string                   `json:"type"`
	ItemID        RealtimeItemID           `json:"itemId"`
	Item          RealtimeConversationItem `json:"item"`
}

func (*RealtimeItemDoneEvent) EventType() RealtimeEventType { return RealtimeEventItemDoneType }
func (e *RealtimeItemDoneEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventItemDoneType)
}
func (e *RealtimeItemDoneEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeItemDeletedEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	ItemID        RealtimeItemID     `json:"itemId"`
}

func (*RealtimeItemDeletedEvent) EventType() RealtimeEventType { return RealtimeEventItemDeletedType }
func (e *RealtimeItemDeletedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventItemDeletedType)
}
func (e *RealtimeItemDeletedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeItemTruncatedEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	ItemID        RealtimeItemID     `json:"itemId"`
	ContentIndex  int                `json:"contentIndex"`
	AudioEndMs    int                `json:"audioEndMs"`
}

func (*RealtimeItemTruncatedEvent) EventType() RealtimeEventType {
	return RealtimeEventItemTruncatedType
}
func (e *RealtimeItemTruncatedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventItemTruncatedType)
}
func (e *RealtimeItemTruncatedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeSpeechStartedEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	ItemID        RealtimeItemID     `json:"itemId"`
	AudioStartMs  int                `json:"audioStartMs"`
}

func (*RealtimeSpeechStartedEvent) EventType() RealtimeEventType {
	return RealtimeEventSpeechStartedType
}
func (e *RealtimeSpeechStartedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventSpeechStartedType)
}
func (e *RealtimeSpeechStartedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeSpeechStoppedEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	ItemID        RealtimeItemID     `json:"itemId"`
	AudioEndMs    int                `json:"audioEndMs"`
}

func (*RealtimeSpeechStoppedEvent) EventType() RealtimeEventType {
	return RealtimeEventSpeechStoppedType
}
func (e *RealtimeSpeechStoppedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventSpeechStoppedType)
}
func (e *RealtimeSpeechStoppedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeInputAudioCommittedEvent struct {
	SchemaVersion  int                `json:"schemaVersion"`
	RequestID      RequestID          `json:"requestId"`
	Sequence       int                `json:"sequence"`
	CommandID      *RealtimeCommandID `json:"commandId,omitempty"`
	Type           string             `json:"type"`
	ItemID         RealtimeItemID     `json:"itemId"`
	PreviousItemID *RealtimeItemID    `json:"previousItemId,omitempty"`
}

func (*RealtimeInputAudioCommittedEvent) EventType() RealtimeEventType {
	return RealtimeEventInputAudioCommittedType
}
func (e *RealtimeInputAudioCommittedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventInputAudioCommittedType)
}
func (e *RealtimeInputAudioCommittedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeInputAudioClearedEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
}

func (*RealtimeInputAudioClearedEvent) EventType() RealtimeEventType {
	return RealtimeEventInputAudioClearedType
}
func (e *RealtimeInputAudioClearedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventInputAudioClearedType)
}
func (e *RealtimeInputAudioClearedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeResponseCreatedEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	ResponseID    RealtimeResponseID `json:"responseId"`
}

func (*RealtimeResponseCreatedEvent) EventType() RealtimeEventType {
	return RealtimeEventResponseCreatedType
}
func (e *RealtimeResponseCreatedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventResponseCreatedType)
}
func (e *RealtimeResponseCreatedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeOutputAudioDeltaEvent struct {
	SchemaVersion int                 `json:"schemaVersion"`
	RequestID     RequestID           `json:"requestId"`
	Sequence      int                 `json:"sequence"`
	CommandID     *RealtimeCommandID  `json:"commandId,omitempty"`
	Type          string              `json:"type"`
	ResponseID    RealtimeResponseID  `json:"responseId"`
	ItemID        RealtimeItemID      `json:"itemId"`
	ContentIndex  int                 `json:"contentIndex"`
	Format        RealtimeAudioFormat `json:"format"`
	Data          string              `json:"data"`
}

func (*RealtimeOutputAudioDeltaEvent) EventType() RealtimeEventType {
	return RealtimeEventOutputAudioDeltaType
}
func (e *RealtimeOutputAudioDeltaEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventOutputAudioDeltaType)
}
func (e *RealtimeOutputAudioDeltaEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeOutputAudioDoneEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	ResponseID    RealtimeResponseID `json:"responseId"`
	ItemID        RealtimeItemID     `json:"itemId"`
	ContentIndex  int                `json:"contentIndex"`
}

func (*RealtimeOutputAudioDoneEvent) EventType() RealtimeEventType {
	return RealtimeEventOutputAudioDoneType
}
func (e *RealtimeOutputAudioDoneEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventOutputAudioDoneType)
}
func (e *RealtimeOutputAudioDoneEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeTranscriptDeltaEvent struct {
	SchemaVersion int                      `json:"schemaVersion"`
	RequestID     RequestID                `json:"requestId"`
	Sequence      int                      `json:"sequence"`
	CommandID     *RealtimeCommandID       `json:"commandId,omitempty"`
	Type          string                   `json:"type"`
	Source        RealtimeTranscriptSource `json:"source"`
	ItemID        RealtimeItemID           `json:"itemId"`
	ContentIndex  int                      `json:"contentIndex"`
	ResponseID    *RealtimeResponseID      `json:"responseId,omitempty"`
	Text          string                   `json:"text"`
}

func (*RealtimeTranscriptDeltaEvent) EventType() RealtimeEventType {
	return RealtimeEventTranscriptDeltaType
}
func (e *RealtimeTranscriptDeltaEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventTranscriptDeltaType)
}
func (e *RealtimeTranscriptDeltaEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeTranscriptDoneEvent struct {
	SchemaVersion int                      `json:"schemaVersion"`
	RequestID     RequestID                `json:"requestId"`
	Sequence      int                      `json:"sequence"`
	CommandID     *RealtimeCommandID       `json:"commandId,omitempty"`
	Type          string                   `json:"type"`
	Source        RealtimeTranscriptSource `json:"source"`
	ItemID        RealtimeItemID           `json:"itemId"`
	ContentIndex  int                      `json:"contentIndex"`
	ResponseID    *RealtimeResponseID      `json:"responseId,omitempty"`
	Transcript    string                   `json:"transcript"`
}

func (*RealtimeTranscriptDoneEvent) EventType() RealtimeEventType {
	return RealtimeEventTranscriptDoneType
}
func (e *RealtimeTranscriptDoneEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventTranscriptDoneType)
}
func (e *RealtimeTranscriptDoneEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeTextDeltaEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	ResponseID    RealtimeResponseID `json:"responseId"`
	ItemID        RealtimeItemID     `json:"itemId"`
	ContentIndex  int                `json:"contentIndex"`
	Text          string             `json:"text"`
}

func (*RealtimeTextDeltaEvent) EventType() RealtimeEventType { return RealtimeEventTextDeltaType }
func (e *RealtimeTextDeltaEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventTextDeltaType)
}
func (e *RealtimeTextDeltaEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeToolCallEvent struct {
	SchemaVersion  int                `json:"schemaVersion"`
	RequestID      RequestID          `json:"requestId"`
	Sequence       int                `json:"sequence"`
	CommandID      *RealtimeCommandID `json:"commandId,omitempty"`
	Type           string             `json:"type"`
	ResponseID     RealtimeResponseID `json:"responseId"`
	ItemID         RealtimeItemID     `json:"itemId"`
	ToolCallID     string             `json:"toolCallId"`
	Name           *string            `json:"name,omitempty"`
	ArgumentsDelta *string            `json:"argumentsDelta,omitempty"`
	Complete       bool               `json:"complete"`
}

func (*RealtimeToolCallEvent) EventType() RealtimeEventType { return RealtimeEventToolCallType }
func (e *RealtimeToolCallEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventToolCallType)
}
func (e *RealtimeToolCallEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeResponseDoneEvent struct {
	SchemaVersion int                    `json:"schemaVersion"`
	RequestID     RequestID              `json:"requestId"`
	Sequence      int                    `json:"sequence"`
	CommandID     *RealtimeCommandID     `json:"commandId,omitempty"`
	Type          string                 `json:"type"`
	ResponseID    RealtimeResponseID     `json:"responseId"`
	Status        RealtimeResponseStatus `json:"status"`
	FinishReason  *FinishReason          `json:"finishReason,omitempty"`
	DeploymentID  DeploymentID           `json:"deploymentId"`
	Units         []UsageQuantity        `json:"units"`
	UsageSource   UsageSource            `json:"usageSource"`
}

func (*RealtimeResponseDoneEvent) EventType() RealtimeEventType { return RealtimeEventResponseDoneType }
func (e *RealtimeResponseDoneEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventResponseDoneType)
}
func (e *RealtimeResponseDoneEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeErrorEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	Fatal         bool               `json:"fatal"`
	Error         Error              `json:"error"`
}

func (*RealtimeErrorEvent) EventType() RealtimeEventType { return RealtimeEventErrorType }
func (e *RealtimeErrorEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventErrorType)
}
func (e *RealtimeErrorEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

type RealtimeSessionClosedEvent struct {
	SchemaVersion int                        `json:"schemaVersion"`
	RequestID     RequestID                  `json:"requestId"`
	Sequence      int                        `json:"sequence"`
	CommandID     *RealtimeCommandID         `json:"commandId,omitempty"`
	Type          string                     `json:"type"`
	Reason        RealtimeSessionCloseReason `json:"reason"`
	DeploymentID  *DeploymentID              `json:"deploymentId,omitempty"`
	Units         []UsageQuantity            `json:"units"`
	UsageSource   UsageSource                `json:"usageSource"`
	ClosedAt      Timestamp                  `json:"closedAt"`
}

func (*RealtimeSessionClosedEvent) EventType() RealtimeEventType {
	return RealtimeEventSessionClosedType
}
func (e *RealtimeSessionClosedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventSessionClosedType)
}
func (e *RealtimeSessionClosedEvent) Header() RealtimeEventHeader {
	return RealtimeEventHeader{SchemaVersion: e.SchemaVersion, RequestID: e.RequestID, Sequence: e.Sequence, CommandID: e.CommandID}
}

package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Realtime sessions (contract set 3.2.0) are the one inference family that is
// not a request: a signed session request opens a long-lived WebSocket
// conversation, the client sends a closed set of commands, and the server sends
// a closed set of events numbered by one monotonic sequence. A session is one
// request for attribution, metering and settlement; it is never substituted,
// and once it has opened on a deployment it stays there.

// RealtimeSchemaVersion is the version every realtime session request,
// command and event declares.
const RealtimeSchemaVersion = 1

// Published bounds.
const (
	// MaxRealtimeAudioFrameBase64Length bounds one audio frame: 48 KiB of
	// audio, about one second of 24 kHz mono PCM16.
	MaxRealtimeAudioFrameBase64Length = 65_536
	// MaxRealtimeSessionDurationMs is the longest a session may be signed for.
	MaxRealtimeSessionDurationMs = 3_600_000
	// MaxRealtimeSessionAudioBytes is the most audio a session may be signed
	// for in each direction.
	MaxRealtimeSessionAudioBytes = 172_800_000
	// MaxRealtimeResumeWindowMs is the longest a dropped connection may be
	// resumed after.
	MaxRealtimeResumeWindowMs = 60_000

	maxRealtimeInstructions = 32_768
)

// RealtimeCommandID is a client-chosen command identity, unique within one
// session.
type RealtimeCommandID string

// RealtimeItemID is a conversation item's identity within one session.
type RealtimeItemID string

// RealtimeResponseID is a response's identity within one session.
type RealtimeResponseID string

// RealtimeSessionKind is what a session does.
type RealtimeSessionKind string

const (
	RealtimeConversation  RealtimeSessionKind = "conversation"
	RealtimeTranscription RealtimeSessionKind = "transcription"
	RealtimeTranslation   RealtimeSessionKind = "translation"
)

var realtimeSessionKindValues = []RealtimeSessionKind{RealtimeConversation, RealtimeTranscription, RealtimeTranslation}

// Valid reports whether the kind is one the contract declares.
func (k RealtimeSessionKind) Valid() bool { return isMember(k, realtimeSessionKindValues) }

// RealtimeSessionTransport is how a session is carried.
type RealtimeSessionTransport string

const RealtimeWebSocket RealtimeSessionTransport = "websocket"

var realtimeSessionTransportValues = []RealtimeSessionTransport{RealtimeWebSocket}

// RealtimeAudioFormat is a session's audio encoding, named by what it is
// because a realtime stream has no container to say so.
type RealtimeAudioFormat string

const (
	RealtimePCM16 RealtimeAudioFormat = "pcm16_24khz"
	RealtimeULaw  RealtimeAudioFormat = "g711_ulaw"
	RealtimeALaw  RealtimeAudioFormat = "g711_alaw"
)

var realtimeAudioFormatValues = []RealtimeAudioFormat{RealtimePCM16, RealtimeULaw, RealtimeALaw}

// RealtimeOutputModality is what a session's responses produce.
type RealtimeOutputModality string

const (
	RealtimeOutputText  RealtimeOutputModality = "text"
	RealtimeOutputAudio RealtimeOutputModality = "audio"
)

var realtimeOutputModalityValues = []RealtimeOutputModality{RealtimeOutputText, RealtimeOutputAudio}

// RealtimeTranscriptSource names which audio a transcript describes.
type RealtimeTranscriptSource string

const (
	RealtimeTranscriptInput  RealtimeTranscriptSource = "input_audio"
	RealtimeTranscriptOutput RealtimeTranscriptSource = "output_audio"
)

var realtimeTranscriptSourceValues = []RealtimeTranscriptSource{RealtimeTranscriptInput, RealtimeTranscriptOutput}

// RealtimeResponseStatus is how a response ended.
type RealtimeResponseStatus string

const (
	RealtimeResponseCompleted  RealtimeResponseStatus = "completed"
	RealtimeResponseCancelled  RealtimeResponseStatus = "cancelled"
	RealtimeResponseIncomplete RealtimeResponseStatus = "incomplete"
	RealtimeResponseFailed     RealtimeResponseStatus = "failed"
)

var realtimeResponseStatusValues = []RealtimeResponseStatus{
	RealtimeResponseCompleted, RealtimeResponseCancelled, RealtimeResponseIncomplete, RealtimeResponseFailed,
}

// RealtimeSessionCloseReason is why a session ended.
type RealtimeSessionCloseReason string

const (
	RealtimeClosedByClient   RealtimeSessionCloseReason = "client_closed"
	RealtimeMaxDuration      RealtimeSessionCloseReason = "max_duration"
	RealtimeIdleTimeout      RealtimeSessionCloseReason = "idle_timeout"
	RealtimeLimitExceeded    RealtimeSessionCloseReason = "limit_exceeded"
	RealtimeResumeExpired    RealtimeSessionCloseReason = "resume_expired"
	RealtimeUpstreamClosed   RealtimeSessionCloseReason = "upstream_closed"
	RealtimeUpstreamError    RealtimeSessionCloseReason = "upstream_error"
	RealtimeNoRouteAvailable RealtimeSessionCloseReason = "no_route_available"
	RealtimeServerShutdown   RealtimeSessionCloseReason = "server_shutdown"
)

var realtimeSessionCloseReasonValues = []RealtimeSessionCloseReason{
	RealtimeClosedByClient, RealtimeMaxDuration, RealtimeIdleTimeout, RealtimeLimitExceeded, RealtimeResumeExpired,
	RealtimeUpstreamClosed, RealtimeUpstreamError, RealtimeNoRouteAvailable, RealtimeServerShutdown,
}

/* -------------------------------------------------------------------------- */
/*  Session configuration                                                     */
/* -------------------------------------------------------------------------- */

// RealtimeTurnDetectionType discriminates RealtimeTurnDetection.
type RealtimeTurnDetectionType string

const (
	TurnDetectionNone        RealtimeTurnDetectionType = "none"
	TurnDetectionServerVAD   RealtimeTurnDetectionType = "server_vad"
	TurnDetectionSemanticVAD RealtimeTurnDetectionType = "semantic_vad"
)

var realtimeTurnDetectionTypeValues = []RealtimeTurnDetectionType{TurnDetectionNone, TurnDetectionServerVAD, TurnDetectionSemanticVAD}

// RealtimeVADEagerness is how readily semantic VAD ends a turn.
type RealtimeVADEagerness string

var realtimeVADEagernessValues = []RealtimeVADEagerness{"low", "medium", "high", "auto"}

// RealtimeTurnDetection is how the end of a user's turn is detected: the
// flattened form of the published union. `none` carries nothing; both VAD forms
// state CreateResponse and InterruptResponse explicitly.
type RealtimeTurnDetection struct {
	Type              RealtimeTurnDetectionType `json:"type"`
	Threshold         *float64                  `json:"threshold,omitempty"`
	PrefixPaddingMs   *int                      `json:"prefixPaddingMs,omitempty"`
	SilenceDurationMs *int                      `json:"silenceDurationMs,omitempty"`
	Eagerness         *RealtimeVADEagerness     `json:"eagerness,omitempty"`
	CreateResponse    *bool                     `json:"createResponse,omitempty"`
	InterruptResponse *bool                     `json:"interruptResponse,omitempty"`
}

func (t RealtimeTurnDetection) validate() error {
	switch t.Type {
	case TurnDetectionNone:
		if t.Threshold != nil || t.PrefixPaddingMs != nil || t.SilenceDurationMs != nil || t.Eagerness != nil ||
			t.CreateResponse != nil || t.InterruptResponse != nil {
			return fmt.Errorf("turnDetection none carries no parameters")
		}
	case TurnDetectionServerVAD:
		if t.Eagerness != nil {
			return fmt.Errorf("server_vad has no eagerness")
		}
		if t.Threshold != nil && (*t.Threshold < 0 || *t.Threshold > 1) {
			return fmt.Errorf("server_vad threshold is between 0 and 1")
		}
		if t.PrefixPaddingMs != nil && (*t.PrefixPaddingMs < 0 || *t.PrefixPaddingMs > 5_000) {
			return fmt.Errorf("server_vad prefixPaddingMs is between 0 and 5000")
		}
		if t.SilenceDurationMs != nil && (*t.SilenceDurationMs < 0 || *t.SilenceDurationMs > 10_000) {
			return fmt.Errorf("server_vad silenceDurationMs is between 0 and 10000")
		}
		if t.CreateResponse == nil || t.InterruptResponse == nil {
			return fmt.Errorf("server_vad states createResponse and interruptResponse")
		}
	case TurnDetectionSemanticVAD:
		if t.Threshold != nil || t.PrefixPaddingMs != nil || t.SilenceDurationMs != nil {
			return fmt.Errorf("semantic_vad takes only eagerness")
		}
		if t.Eagerness == nil || !isMember(*t.Eagerness, realtimeVADEagernessValues) {
			return fmt.Errorf("semantic_vad needs an eagerness")
		}
		if t.CreateResponse == nil || t.InterruptResponse == nil {
			return fmt.Errorf("semantic_vad states createResponse and interruptResponse")
		}
	default:
		return fmt.Errorf("%q is not a turn detection", t.Type)
	}
	return nil
}

// createsOrInterrupts reports whether turn detection answers or cuts off a
// response on its own.
func (t RealtimeTurnDetection) createsOrInterrupts() bool {
	return (t.CreateResponse != nil && *t.CreateResponse) || (t.InterruptResponse != nil && *t.InterruptResponse)
}

// RealtimeInputTranscription enables transcription of the caller's audio.
type RealtimeInputTranscription struct {
	Language *string `json:"language,omitempty"`
	Prompt   *string `json:"prompt,omitempty"`
}

func (t RealtimeInputTranscription) validate() error {
	if !boundedText(t.Language, 2, 35) {
		return fmt.Errorf("inputAudioTranscription.language is a BCP 47 tag")
	}
	if !boundedText(t.Prompt, 0, 4_096) {
		return fmt.Errorf("inputAudioTranscription.prompt is at most 4096 characters")
	}
	return nil
}

// RealtimeTranslationTarget is the target of a translation session.
type RealtimeTranslationTarget struct {
	TargetLanguage string `json:"targetLanguage"`
}

// RealtimeSessionConfig is a session's full configuration.
type RealtimeSessionConfig struct {
	Instructions            *string                     `json:"instructions,omitempty"`
	OutputModalities        []RealtimeOutputModality    `json:"outputModalities,omitempty"`
	Voice                   *string                     `json:"voice,omitempty"`
	InputAudioFormat        RealtimeAudioFormat         `json:"inputAudioFormat"`
	OutputAudioFormat       *RealtimeAudioFormat        `json:"outputAudioFormat,omitempty"`
	TurnDetection           RealtimeTurnDetection       `json:"turnDetection"`
	InputAudioTranscription *RealtimeInputTranscription `json:"inputAudioTranscription,omitempty"`
	Translation             *RealtimeTranslationTarget  `json:"translation,omitempty"`
	Tools                   []ToolDefinition            `json:"tools,omitempty"`
	ToolChoice              *ToolChoice                 `json:"toolChoice,omitempty"`
	Temperature             *float64                    `json:"temperature,omitempty"`
	MaxOutputTokens         *int                        `json:"maxOutputTokens,omitempty"`
}

// RealtimeSessionConfigUpdate is what session.update may change. Kind, audio
// formats and voice are fixed at open, so they are not fields here.
type RealtimeSessionConfigUpdate struct {
	Instructions            *string                     `json:"instructions,omitempty"`
	OutputModalities        []RealtimeOutputModality    `json:"outputModalities,omitempty"`
	TurnDetection           *RealtimeTurnDetection      `json:"turnDetection,omitempty"`
	InputAudioTranscription *RealtimeInputTranscription `json:"inputAudioTranscription,omitempty"`
	Tools                   []ToolDefinition            `json:"tools,omitempty"`
	ToolChoice              *ToolChoice                 `json:"toolChoice,omitempty"`
	Temperature             *float64                    `json:"temperature,omitempty"`
	MaxOutputTokens         *int                        `json:"maxOutputTokens,omitempty"`
}

// RealtimeSessionLimits are the ceilings Oxy signed the session for. The data
// plane closes the session rather than exceeding any of them.
type RealtimeSessionLimits struct {
	MaxDurationMs       int `json:"maxDurationMs"`
	IdleTimeoutMs       int `json:"idleTimeoutMs"`
	MaxInputAudioBytes  int `json:"maxInputAudioBytes"`
	MaxOutputAudioBytes int `json:"maxOutputAudioBytes"`
	MaxResponses        int `json:"maxResponses"`
}

// RealtimeClientMetadata is what the edge records about the session call.
type RealtimeClientMetadata struct {
	Endpoint        string            `json:"endpoint"`
	ClientSessionID *string           `json:"clientSessionId,omitempty"`
	ReceivedAt      Timestamp         `json:"receivedAt"`
	Labels          map[string]string `json:"labels,omitempty"`
}

// RealtimeSessionRequest is the signed first frame that opens a session.
type RealtimeSessionRequest struct {
	SchemaVersion    int                      `json:"schemaVersion"`
	Attribution      Attribution              `json:"attribution"`
	ModelReference   ModelReference           `json:"modelReference"`
	Kind             RealtimeSessionKind      `json:"kind"`
	Transport        RealtimeSessionTransport `json:"transport"`
	Config           RealtimeSessionConfig    `json:"config"`
	Limits           RealtimeSessionLimits    `json:"limits"`
	Client           RealtimeClientMetadata   `json:"client"`
	RoutingPolicy    RoutingPolicyReference   `json:"routingPolicy"`
	AuthorizedRoutes []AuthorizedRoute        `json:"authorizedRoutes"`
}

// RealtimeResponseParameters overrides the session's configuration for one
// response.
type RealtimeResponseParameters struct {
	Instructions     *string                  `json:"instructions,omitempty"`
	OutputModalities []RealtimeOutputModality `json:"outputModalities,omitempty"`
	MaxOutputTokens  *int                     `json:"maxOutputTokens,omitempty"`
	ToolChoice       *ToolChoice              `json:"toolChoice,omitempty"`
}

/* -------------------------------------------------------------------------- */
/*  Conversation items                                                        */
/* -------------------------------------------------------------------------- */

// RealtimeContentPartType discriminates RealtimeContentPart.
type RealtimeContentPartType string

const (
	RealtimeInputTextPart   RealtimeContentPartType = "input_text"
	RealtimeInputAudioPart  RealtimeContentPartType = "input_audio"
	RealtimeOutputTextPart  RealtimeContentPartType = "output_text"
	RealtimeOutputAudioPart RealtimeContentPartType = "output_audio"
)

var realtimeContentPartTypeValues = []RealtimeContentPartType{
	RealtimeInputTextPart, RealtimeInputAudioPart, RealtimeOutputTextPart, RealtimeOutputAudioPart,
}

// RealtimeContentPart is one part of a conversation message, flattened.
type RealtimeContentPart struct {
	Type       RealtimeContentPartType `json:"type"`
	Text       *string                 `json:"text,omitempty"`
	Format     *RealtimeAudioFormat    `json:"format,omitempty"`
	Data       *string                 `json:"data,omitempty"`
	Transcript *string                 `json:"transcript,omitempty"`
}

// RealtimeItemType discriminates RealtimeConversationItem.
type RealtimeItemType string

const (
	RealtimeMessageItem            RealtimeItemType = "message"
	RealtimeFunctionCallItem       RealtimeItemType = "function_call"
	RealtimeFunctionCallOutputItem RealtimeItemType = "function_call_output"
)

var realtimeItemTypeValues = []RealtimeItemType{RealtimeMessageItem, RealtimeFunctionCallItem, RealtimeFunctionCallOutputItem}

// RealtimeItemRole is a message's author.
type RealtimeItemRole string

var realtimeItemRoleValues = []RealtimeItemRole{"system", "user", "assistant"}

// RealtimeConversationItem is one item of a session's conversation, flattened.
type RealtimeConversationItem struct {
	Type      RealtimeItemType      `json:"type"`
	ItemID    *RealtimeItemID       `json:"itemId,omitempty"`
	Role      *RealtimeItemRole     `json:"role,omitempty"`
	Content   []RealtimeContentPart `json:"content,omitempty"`
	CallID    *string               `json:"callId,omitempty"`
	Name      *string               `json:"name,omitempty"`
	Arguments *string               `json:"arguments,omitempty"`
	Output    *string               `json:"output,omitempty"`
}

// Validate applies the published per-variant rules.
func (i RealtimeConversationItem) Validate() error {
	if i.ItemID != nil && !boundedID(string(*i.ItemID)) {
		return fmt.Errorf("item.itemId is 1 to 128 characters")
	}
	switch i.Type {
	case RealtimeMessageItem:
		if i.Role == nil || !isMember(*i.Role, realtimeItemRoleValues) || len(i.Content) == 0 || len(i.Content) > 64 ||
			i.CallID != nil || i.Name != nil || i.Arguments != nil || i.Output != nil {
			return fmt.Errorf("a message item has a role and 1 to 64 content parts, and nothing else")
		}
		for index, part := range i.Content {
			if err := part.validate(*i.Role); err != nil {
				return fmt.Errorf("item.content[%d]: %w", index, err)
			}
		}
	case RealtimeFunctionCallItem:
		if i.Role != nil || i.Content != nil || i.Output != nil || i.CallID == nil || !boundedID(*i.CallID) ||
			i.Name == nil || !boundedID(*i.Name) || i.Arguments == nil {
			return fmt.Errorf("a function_call item has a callId, name and arguments, and nothing else")
		}
	case RealtimeFunctionCallOutputItem:
		if i.Role != nil || i.Content != nil || i.Name != nil || i.Arguments != nil || i.CallID == nil ||
			!boundedID(*i.CallID) || i.Output == nil || utf16Length(*i.Output) > 1_048_576 {
			return fmt.Errorf("a function_call_output item has a callId and output, and nothing else")
		}
	default:
		return fmt.Errorf("%q is not a conversation item", i.Type)
	}
	return nil
}

func (p RealtimeContentPart) validate(role RealtimeItemRole) error {
	input := p.Type == RealtimeInputTextPart || p.Type == RealtimeInputAudioPart
	if (role == "assistant") == input {
		return fmt.Errorf("a %s message cannot carry %s", role, p.Type)
	}
	if role == "system" && p.Type != RealtimeInputTextPart {
		return fmt.Errorf("a system message is text")
	}
	switch p.Type {
	case RealtimeInputTextPart, RealtimeOutputTextPart:
		if p.Text == nil || p.Format != nil || p.Data != nil || p.Transcript != nil {
			return fmt.Errorf("a text part carries text only")
		}
		if p.Type == RealtimeInputTextPart && utf16Length(*p.Text) > 1_048_576 {
			return fmt.Errorf("an input_text part is at most 1048576 characters")
		}
	case RealtimeInputAudioPart, RealtimeOutputAudioPart:
		if p.Text != nil || p.Format == nil || !isMember(*p.Format, realtimeAudioFormatValues) {
			return fmt.Errorf("an audio part names its format")
		}
		if p.Type == RealtimeOutputAudioPart && p.Data != nil {
			return fmt.Errorf("an output_audio part never carries audio inline")
		}
		if p.Data != nil {
			if err := ValidateRealtimeAudioFrame(*p.Data); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%q is not a content part", p.Type)
	}
	return nil
}

/* -------------------------------------------------------------------------- */
/*  Validation                                                                */
/* -------------------------------------------------------------------------- */

// ValidateRealtimeAudioFrame checks one base64 audio frame against the
// published bound and padded-base64 grammar, and returns nothing else: the
// decoded size is DecodedRealtimeAudioBytes.
func ValidateRealtimeAudioFrame(data string) error {
	if len(data) < 4 || len(data) > MaxRealtimeAudioFrameBase64Length || len(data)%4 != 0 {
		return fmt.Errorf("an audio frame is 4 to %d characters of padded base64", MaxRealtimeAudioFrameBase64Length)
	}
	if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))); err != nil {
		return fmt.Errorf("an audio frame is padded base64")
	}
	return nil
}

// DecodedRealtimeAudioBytes is the number of audio bytes a valid frame holds,
// which is what a session's audio ceilings are counted in.
func DecodedRealtimeAudioBytes(data string) int {
	return base64.StdEncoding.DecodedLen(len(data)) - strings.Count(data[max(0, len(data)-2):], "=")
}

func (c RealtimeSessionConfig) validate() error {
	if !boundedText(c.Instructions, 0, maxRealtimeInstructions) {
		return fmt.Errorf("config.instructions is at most 32768 characters")
	}
	if err := validateOutputModalities(c.OutputModalities); err != nil {
		return err
	}
	if !boundedText(c.Voice, 1, 64) {
		return fmt.Errorf("config.voice is 1 to 64 characters")
	}
	if !isMember(c.InputAudioFormat, realtimeAudioFormatValues) ||
		(c.OutputAudioFormat != nil && !isMember(*c.OutputAudioFormat, realtimeAudioFormatValues)) {
		return fmt.Errorf("config audio formats are pcm16_24khz, g711_ulaw or g711_alaw")
	}
	if err := c.TurnDetection.validate(); err != nil {
		return fmt.Errorf("config.turnDetection: %w", err)
	}
	if c.InputAudioTranscription != nil {
		if err := c.InputAudioTranscription.validate(); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	if c.Translation != nil && !boundedText(&c.Translation.TargetLanguage, 2, 35) {
		return fmt.Errorf("config.translation.targetLanguage is a BCP 47 tag")
	}
	return validateSharedConfig(c.Tools, c.ToolChoice, c.Temperature, c.MaxOutputTokens)
}

func validateOutputModalities(modalities []RealtimeOutputModality) error {
	if modalities == nil {
		return nil
	}
	if len(modalities) < 1 || len(modalities) > 2 {
		return fmt.Errorf("outputModalities names one or two modalities")
	}
	seen := make(map[RealtimeOutputModality]bool, 2)
	for _, modality := range modalities {
		if !isMember(modality, realtimeOutputModalityValues) || seen[modality] {
			return fmt.Errorf("outputModalities names text and audio at most once each")
		}
		seen[modality] = true
	}
	return nil
}

func validateSharedConfig(tools []ToolDefinition, choice *ToolChoice, temperature *float64, maxOutputTokens *int) error {
	if err := validateTuning(tools, temperature, maxOutputTokens); err != nil {
		return err
	}
	return validateToolDeclarations(tools, choice, "session")
}

// validateTuning holds the ranges a session configuration and a session update
// share. The update schema has no tool-declaration refinement, so it stops here.
func validateTuning(tools []ToolDefinition, temperature *float64, maxOutputTokens *int) error {
	if len(tools) > 128 {
		return fmt.Errorf("config.tools holds at most 128 tools")
	}
	if temperature != nil && (*temperature < 0 || *temperature > 2) {
		return fmt.Errorf("config.temperature is between 0 and 2")
	}
	if maxOutputTokens != nil && *maxOutputTokens <= 0 {
		return fmt.Errorf("config.maxOutputTokens must be positive")
	}
	return nil
}

// Validate carries the published refinements of the session request: the
// per-kind configuration rules, the limit ordering and the rule that a session
// is never substituted.
func (r *RealtimeSessionRequest) Validate() error {
	if r.SchemaVersion != 1 {
		return fmt.Errorf("contract: realtime session schemaVersion must be 1")
	}
	if r.Attribution.RequestID == "" || r.Attribution.Principal.Billing.AccountID == "" {
		return fmt.Errorf("contract: realtime session attribution names its request and billing account")
	}
	if !r.ModelReference.Valid() {
		return fmt.Errorf("contract: realtime session modelReference is invalid")
	}
	if !isMember(r.Kind, realtimeSessionKindValues) || !isMember(r.Transport, realtimeSessionTransportValues) {
		return fmt.Errorf("contract: realtime session kind or transport is invalid")
	}
	if err := r.Config.validate(); err != nil {
		return fmt.Errorf("contract: realtime session %w", err)
	}
	limits := r.Limits
	if limits.MaxDurationMs < 1_000 || limits.MaxDurationMs > MaxRealtimeSessionDurationMs ||
		limits.IdleTimeoutMs < 1_000 || limits.IdleTimeoutMs > MaxRealtimeSessionDurationMs ||
		limits.MaxInputAudioBytes < 1 || limits.MaxInputAudioBytes > MaxRealtimeSessionAudioBytes ||
		limits.MaxOutputAudioBytes < 1 || limits.MaxOutputAudioBytes > MaxRealtimeSessionAudioBytes ||
		limits.MaxResponses < 1 || limits.MaxResponses > 10_000 {
		return fmt.Errorf("contract: realtime session limits are out of range")
	}
	if limits.IdleTimeoutMs > limits.MaxDurationMs {
		return fmt.Errorf("contract: an idle timeout cannot outlast the session")
	}
	if r.Client.Endpoint == "" || len(r.Client.Endpoint) > 256 || r.Client.ReceivedAt == "" {
		return fmt.Errorf("contract: realtime session client metadata is incomplete")
	}

	config := r.Config
	producesAudio := isMember(RealtimeOutputAudio, config.OutputModalities)
	switch r.Kind {
	case RealtimeConversation:
		switch {
		case config.OutputModalities == nil:
			return fmt.Errorf("contract: a conversation names what its responses produce")
		case producesAudio && (config.Voice == nil || config.OutputAudioFormat == nil):
			return fmt.Errorf("contract: spoken responses need a voice and an output audio format")
		case config.Translation != nil:
			return fmt.Errorf("contract: only a translation session translates")
		}
	case RealtimeTranscription:
		switch {
		case config.InputAudioTranscription == nil:
			return fmt.Errorf("contract: a transcription session transcribes its input")
		case config.OutputModalities != nil || config.Voice != nil || config.OutputAudioFormat != nil ||
			config.Translation != nil || config.Tools != nil || config.ToolChoice != nil || config.MaxOutputTokens != nil:
			return fmt.Errorf("contract: a transcription session never responds")
		case config.TurnDetection.createsOrInterrupts():
			return fmt.Errorf("contract: a transcription session has no response to create or interrupt")
		}
	case RealtimeTranslation:
		if config.Translation == nil || config.OutputAudioFormat == nil {
			return fmt.Errorf("contract: a translation session names its target language and output audio format")
		}
		if config.Tools != nil || config.ToolChoice != nil {
			return fmt.Errorf("contract: a translation session calls no tools")
		}
	}

	if err := validateRouteList(r.AuthorizedRoutes); err != nil {
		return err
	}
	line := r.ModelReference.ModelID()
	for index, route := range r.AuthorizedRoutes {
		if route.Substitution != SubstitutionSameModel || route.ModelReference.ModelID() != line {
			return fmt.Errorf("contract: authorizedRoutes[%d]: every route of a session serves the model it named", index)
		}
		if r.ModelReference.Pinned() && route.ModelReference != r.ModelReference {
			return fmt.Errorf("contract: authorizedRoutes[%d].modelReference: a pinned session is served on exactly the revision it pinned", index)
		}
	}
	return nil
}

/* -------------------------------------------------------------------------- */
/*  Decoding client commands                                                  */
/* -------------------------------------------------------------------------- */

// realtimeCommandConstructors maps each command discriminator to the type it
// decodes into.
var realtimeCommandConstructors = map[RealtimeCommandType]func() RealtimeCommand{
	RealtimeCommandSessionUpdateType:    func() RealtimeCommand { return &RealtimeSessionUpdateCommand{} },
	RealtimeCommandItemCreateType:       func() RealtimeCommand { return &RealtimeItemCreateCommand{} },
	RealtimeCommandItemDeleteType:       func() RealtimeCommand { return &RealtimeItemDeleteCommand{} },
	RealtimeCommandItemTruncateType:     func() RealtimeCommand { return &RealtimeItemTruncateCommand{} },
	RealtimeCommandInputAudioAppendType: func() RealtimeCommand { return &RealtimeInputAudioAppendCommand{} },
	RealtimeCommandInputAudioCommitType: func() RealtimeCommand { return &RealtimeInputAudioCommitCommand{} },
	RealtimeCommandInputAudioClearType:  func() RealtimeCommand { return &RealtimeInputAudioClearCommand{} },
	RealtimeCommandResponseCreateType:   func() RealtimeCommand { return &RealtimeResponseCreateCommand{} },
	RealtimeCommandResponseCancelType:   func() RealtimeCommand { return &RealtimeResponseCancelCommand{} },
	RealtimeCommandSessionResumeType:    func() RealtimeCommand { return &RealtimeSessionResumeCommand{} },
	RealtimeCommandSessionCloseType:     func() RealtimeCommand { return &RealtimeSessionCloseCommand{} },
}

// DecodeRealtimeCommand decodes and validates one client text frame. Unknown
// fields are refused, as the published strict shapes refuse them, and every
// command must name the session it belongs to.
func DecodeRealtimeCommand(frame []byte, session RequestID) (RealtimeCommand, error) {
	var probe struct {
		Type RealtimeCommandType `json:"type"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		return nil, fmt.Errorf("contract: a realtime frame is one JSON command")
	}
	construct, known := realtimeCommandConstructors[probe.Type]
	if !known {
		return nil, fmt.Errorf("contract: %q is not a realtime command", probe.Type)
	}
	command := construct()
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(command); err != nil {
		return nil, fmt.Errorf("contract: realtime %s command: %w", probe.Type, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("contract: a realtime frame is exactly one command")
	}
	if err := validateRealtimeCommand(command, session); err != nil {
		return nil, err
	}
	return command, nil
}

func validateRealtimeCommand(command RealtimeCommand, session RequestID) error {
	requestID, commandID := command.Identity()
	if requestID != session {
		return fmt.Errorf("contract: the command names another session")
	}
	if !boundedID(string(commandID)) {
		return fmt.Errorf("contract: commandId is 1 to 128 characters")
	}
	schemaVersion := 0
	switch c := command.(type) {
	case *RealtimeSessionUpdateCommand:
		schemaVersion = c.SchemaVersion
		update := c.Config
		if update.Instructions == nil && update.OutputModalities == nil && update.TurnDetection == nil &&
			update.InputAudioTranscription == nil && update.Tools == nil && update.ToolChoice == nil &&
			update.Temperature == nil && update.MaxOutputTokens == nil {
			return fmt.Errorf("contract: a session update changes at least one field")
		}
		if !boundedText(update.Instructions, 0, maxRealtimeInstructions) {
			return fmt.Errorf("contract: config.instructions is at most 32768 characters")
		}
		if err := validateOutputModalities(update.OutputModalities); err != nil {
			return fmt.Errorf("contract: %w", err)
		}
		if update.TurnDetection != nil {
			if err := update.TurnDetection.validate(); err != nil {
				return fmt.Errorf("contract: config.turnDetection: %w", err)
			}
		}
		if update.InputAudioTranscription != nil {
			if err := update.InputAudioTranscription.validate(); err != nil {
				return fmt.Errorf("contract: %w", err)
			}
		}
		if err := validateTuning(update.Tools, update.Temperature, update.MaxOutputTokens); err != nil {
			return fmt.Errorf("contract: %w", err)
		}
	case *RealtimeItemCreateCommand:
		schemaVersion = c.SchemaVersion
		if c.PreviousItemID != nil && !boundedID(string(*c.PreviousItemID)) {
			return fmt.Errorf("contract: previousItemId is 1 to 128 characters")
		}
		if err := c.Item.Validate(); err != nil {
			return fmt.Errorf("contract: %w", err)
		}
	case *RealtimeItemDeleteCommand:
		schemaVersion = c.SchemaVersion
		if !boundedID(string(c.ItemID)) {
			return fmt.Errorf("contract: itemId is 1 to 128 characters")
		}
	case *RealtimeItemTruncateCommand:
		schemaVersion = c.SchemaVersion
		if !boundedID(string(c.ItemID)) || c.ContentIndex < 0 || c.AudioEndMs < 0 {
			return fmt.Errorf("contract: a truncation names an item, a content index and a non-negative audio end")
		}
	case *RealtimeInputAudioAppendCommand:
		schemaVersion = c.SchemaVersion
		if err := ValidateRealtimeAudioFrame(c.Data); err != nil {
			return fmt.Errorf("contract: %w", err)
		}
	case *RealtimeInputAudioCommitCommand:
		schemaVersion = c.SchemaVersion
	case *RealtimeInputAudioClearCommand:
		schemaVersion = c.SchemaVersion
	case *RealtimeResponseCreateCommand:
		schemaVersion = c.SchemaVersion
		if response := c.Response; response != nil {
			if !boundedText(response.Instructions, 0, maxRealtimeInstructions) {
				return fmt.Errorf("contract: response.instructions is at most 32768 characters")
			}
			if err := validateOutputModalities(response.OutputModalities); err != nil {
				return fmt.Errorf("contract: %w", err)
			}
			if response.MaxOutputTokens != nil && *response.MaxOutputTokens <= 0 {
				return fmt.Errorf("contract: response.maxOutputTokens must be positive")
			}
		}
	case *RealtimeResponseCancelCommand:
		schemaVersion = c.SchemaVersion
		if c.ResponseID != nil && !boundedID(string(*c.ResponseID)) {
			return fmt.Errorf("contract: responseId is 1 to 128 characters")
		}
	case *RealtimeSessionResumeCommand:
		schemaVersion = c.SchemaVersion
		if c.AfterSequence < -1 {
			return fmt.Errorf("contract: afterSequence is -1 or a sequence")
		}
	case *RealtimeSessionCloseCommand:
		schemaVersion = c.SchemaVersion
	}
	if schemaVersion != 1 {
		return fmt.Errorf("contract: realtime commands are schemaVersion 1")
	}
	return nil
}

func boundedID(value string) bool { return value != "" && utf16Length(value) <= 128 }

// utf16Length is a string's length as the published schemas measure it, in
// UTF-16 code units, counted without allocating.
func utf16Length(value string) int {
	length := 0
	for _, r := range value {
		length += utf16.RuneLen(r)
	}
	return length
}

// boundedText reports whether an optional string is within a UTF-16 length.
func boundedText(value *string, minimum, maximum int) bool {
	if value == nil {
		return true
	}
	length := utf16Length(*value)
	return length >= minimum && length <= maximum
}

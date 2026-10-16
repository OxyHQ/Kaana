package contract

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
)

// The public API of https://kaana.ai/v1 (Kaana ADR 0002, decisions 7–10;
// `@kaana.ai/contracts`, src/public). These are the shapes internal/publicapi
// writes to a client: they name what went wrong and what served a request by
// model, never by provider, operator, key or deployment.

/* -------------------------------------------------------------------------- */
/*  Errors                                                                    */
/* -------------------------------------------------------------------------- */

// PublicErrorCode is the public error vocabulary: the internal one minus the
// three codes that describe Kaana's own relationship with a provider.
type PublicErrorCode string

var publicErrorCodeValues = []PublicErrorCode{
	"invalid_request", "authentication_failed", "permission_denied", "insufficient_scope",
	"model_not_found", "unsupported_modality", "context_length_exceeded", "request_too_large",
	"output_limit_exceeded", "idempotency_conflict", "insufficient_balance", "spending_limit_exceeded",
	"quota_exceeded", "byok_credential_invalid", "policy_violation", "commercial_permission_denied",
	"no_route_available", "upstream_content_filtered", "cancelled", "rate_limited",
	"provider_error", "provider_timeout", "provider_overloaded", "service_unavailable", "internal_error",
}

// publicErrorHTTPStatus is PUBLIC_ERROR_HTTP_STATUS: the status of each code
// when it is decided before the first byte.
var publicErrorHTTPStatus = map[PublicErrorCode]int{
	"invalid_request": http.StatusBadRequest, "authentication_failed": http.StatusUnauthorized,
	"permission_denied": http.StatusForbidden, "insufficient_scope": http.StatusForbidden,
	"model_not_found": http.StatusNotFound, "unsupported_modality": http.StatusBadRequest,
	"context_length_exceeded": http.StatusBadRequest, "request_too_large": http.StatusRequestEntityTooLarge,
	"output_limit_exceeded": http.StatusBadRequest, "idempotency_conflict": http.StatusConflict,
	"insufficient_balance": http.StatusPaymentRequired, "spending_limit_exceeded": http.StatusPaymentRequired,
	"quota_exceeded": http.StatusTooManyRequests, "byok_credential_invalid": http.StatusBadRequest,
	"policy_violation": http.StatusForbidden, "commercial_permission_denied": http.StatusForbidden,
	"no_route_available": http.StatusServiceUnavailable, "upstream_content_filtered": http.StatusBadRequest,
	"cancelled": 499, "rate_limited": http.StatusTooManyRequests,
	"provider_error": http.StatusBadGateway, "provider_timeout": http.StatusGatewayTimeout,
	"provider_overloaded": http.StatusServiceUnavailable, "service_unavailable": http.StatusServiceUnavailable,
	"internal_error": http.StatusInternalServerError,
}

// HTTPStatus is the status this code is answered with before the first byte.
func (c PublicErrorCode) HTTPStatus() int {
	if status, known := publicErrorHTTPStatus[c]; known {
		return status
	}
	return http.StatusInternalServerError
}

// Valid reports whether the code is public.
func (c PublicErrorCode) Valid() bool { return slices.Contains(publicErrorCodeValues, c) }

// PublicError is the error body every /v1 surface returns and every public
// stream's terminal error event carries.
type PublicError struct {
	SchemaVersion    int                    `json:"schemaVersion"`
	Code             PublicErrorCode        `json:"code"`
	Message          string                 `json:"message"`
	Retryable        bool                   `json:"retryable"`
	RequestID        RequestID              `json:"requestId"`
	RetryAfterMs     *int                   `json:"retryAfterMs,omitempty"`
	Param            *string                `json:"param,omitempty"`
	UpstreamCategory *UpstreamErrorCategory `json:"upstreamCategory,omitempty"`
}

// NewPublicError builds a public error whose retryability follows its code,
// as the contract's refinement requires.
func NewPublicError(requestID RequestID, code PublicErrorCode, message string) *PublicError {
	return &PublicError{
		SchemaVersion: SchemaVersion, Code: code, Message: SafeErrorText(message),
		Retryable: ErrorCode(code).Retryable(), RequestID: requestID,
	}
}

// WithParam names the request field at fault.
func (e *PublicError) WithParam(param string) *PublicError {
	e.Param = &param
	return e
}

// WithRetryAfter records how long to wait, only for a retryable code.
func (e *PublicError) WithRetryAfter(milliseconds int) *PublicError {
	if e.Retryable && milliseconds >= 0 {
		e.RetryAfterMs = &milliseconds
	}
	return e
}

// Error implements error.
func (e *PublicError) Error() string {
	return fmt.Sprintf("%s: %s (requestId=%s)", e.Code, e.Message, e.RequestID)
}

// publicMessages is the fixed text of each code when the failure came from
// Kaana's execution rather than from the request's own validation. An
// adapter's or provider's message can name the provider that served a request,
// which the public surface never does (ADR 0002, decision 7), so it is replaced
// rather than scrubbed: a pattern that missed one spelling would leak it.
var publicMessages = map[PublicErrorCode]string{
	"invalid_request":              "the request cannot be served as asked",
	"authentication_failed":        "the credential was not accepted; applications and keys are managed in Oxy Console (https://console.oxy.so)",
	"permission_denied":            "the credential is not permitted to make this request; see Oxy Console (https://console.oxy.so)",
	"insufficient_scope":           "the credential lacks the scope this request needs; see Oxy Console (https://console.oxy.so)",
	"model_not_found":              "no such model is served",
	"unsupported_modality":         "the model does not serve this kind of request",
	"context_length_exceeded":      "the request exceeds the model's context window",
	"request_too_large":            "the request is larger than this endpoint accepts",
	"output_limit_exceeded":        "the requested output exceeds the model's limit",
	"idempotency_conflict":         "the idempotency key was already used for a different request",
	"insufficient_balance":         "the account has insufficient balance; billing is managed in Oxy Console (https://console.oxy.so)",
	"spending_limit_exceeded":      "a spending limit would be exceeded; limits are managed in Oxy Console (https://console.oxy.so)",
	"quota_exceeded":               "a quota for this credential is exhausted",
	"byok_credential_invalid":      "the customer provider credential is unavailable; check it in Oxy Console (https://console.oxy.so)",
	"policy_violation":             "the request is outside the application's inference policy",
	"commercial_permission_denied": "the model may not be used commercially under this application's terms",
	"no_route_available":           "no route can serve this request now",
	"upstream_content_filtered":    "the request or its output was filtered",
	"cancelled":                    "the request was cancelled",
	"rate_limited":                 "the request rate limit was reached; retry later",
	"provider_error":               "the model failed to serve the request",
	"provider_timeout":             "the model did not respond in time",
	"provider_overloaded":          "the model is overloaded; retry later",
	"service_unavailable":          "the service is temporarily unavailable; retry later",
	"internal_error":               "an internal error occurred",
}

// PublicMessage is the fixed public text of a code.
func PublicMessage(code PublicErrorCode) string {
	if message, known := publicMessages[code]; known {
		return message
	}
	return publicMessages["internal_error"]
}

// PublicErrorFrom projects an internal error onto the public surface: an
// internal-only code becomes service_unavailable (Kaana retired that key or
// route and nothing else was left), the provider passthrough is dropped, and
// the message is the code's fixed public text, because an internal message can
// name a provider. The upstream category stays: it is provider-neutral.
func PublicErrorFrom(failure *Error) *PublicError {
	if failure == nil {
		return nil
	}
	code := PublicErrorCode(failure.Code)
	switch failure.Code {
	case CodeProviderCredentialInvalid, CodeProviderBillingRefused, CodeDeploymentUnavailable:
		code = "service_unavailable"
	}
	if !code.Valid() {
		code = "internal_error"
	}
	public := NewPublicError(failure.RequestID, code, PublicMessage(code))
	if failure.Param != nil && code == "invalid_request" {
		public = public.WithParam(*failure.Param)
		public.Message = SafeErrorText(PublicMessage(code) + " (" + *failure.Param + ")")
	}
	if failure.RetryAfterMs != nil {
		public = public.WithRetryAfter(*failure.RetryAfterMs)
	}
	public.UpstreamCategory = failure.UpstreamCategory
	if !public.Retryable {
		public.RetryAfterMs = nil
	}
	return public
}

/* -------------------------------------------------------------------------- */
/*  Speech                                                                    */
/* -------------------------------------------------------------------------- */

// SpeechStreamFormat is how a streamed speech response is carried.
type SpeechStreamFormat string

const (
	SpeechStreamAudio SpeechStreamFormat = "audio"
	SpeechStreamSSE   SpeechStreamFormat = "sse"
)

var speechStreamFormatValues = []SpeechStreamFormat{SpeechStreamAudio, SpeechStreamSSE}

// SpeechRequest is POST /v1/audio/speech, in the shape a stock OpenAI client
// sends.
type SpeechRequest struct {
	Model          ModelReference      `json:"model"`
	Input          string              `json:"input"`
	Voice          string              `json:"voice"`
	ResponseFormat *SpeechFormat       `json:"response_format,omitempty"`
	Speed          *float64            `json:"speed,omitempty"`
	Stream         *bool               `json:"stream,omitempty"`
	StreamFormat   *SpeechStreamFormat `json:"stream_format,omitempty"`
	User           *string             `json:"user,omitempty"`
}

// SpeechAudioDeltaEvent is one chunk of streamed speech.
type SpeechAudioDeltaEvent struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

// SpeechAudioDoneEvent ends a successful speech stream.
type SpeechAudioDoneEvent struct {
	Type      string          `json:"type"`
	RequestID RequestID       `json:"requestId"`
	Model     ModelReference  `json:"model"`
	Usage     []UsageQuantity `json:"usage"`
}

// SpeechStreamErrorEvent ends a failed speech stream.
type SpeechStreamErrorEvent struct {
	Type  string      `json:"type"`
	Error PublicError `json:"error"`
}

/* -------------------------------------------------------------------------- */
/*  Transcription                                                             */
/* -------------------------------------------------------------------------- */

// TranscriptionTimestamps is the timing detail a transcription asks for.
type TranscriptionTimestamps string

var transcriptionTimestampsValues = []TranscriptionTimestamps{"none", "segment", "word"}

// Valid reports whether the value is one the contract names.
func (t TranscriptionTimestamps) Valid() bool {
	return slices.Contains(transcriptionTimestampsValues, t)
}

// AudioInputMediaType is the media type of an uploaded audio file.
type AudioInputMediaType string

var audioInputMediaTypeValues = []AudioInputMediaType{
	"audio/mpeg", "audio/mp4", "audio/wav", "audio/ogg", "audio/webm", "audio/flac", "audio/aac", "audio/pcm",
}

// Valid reports whether the media type is one the contract accepts.
func (t AudioInputMediaType) Valid() bool { return slices.Contains(audioInputMediaTypeValues, t) }

// LanguageTag is a BCP 47 language tag.
type LanguageTag string

// languageTagPattern is the contract's grammar for a tag (languageTagSchema):
// validated by its shape only; the catalogue is the authority on which tags a
// model accepts.
var languageTagPattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)

// Valid reports whether the tag matches the contract's grammar and length.
func (t LanguageTag) Valid() bool {
	return len(t) <= 35 && languageTagPattern.MatchString(string(t))
}

// TranscriptionRequest is the text fields of the multipart form.
type TranscriptionRequest struct {
	Model      ModelReference           `json:"model"`
	Language   *LanguageTag             `json:"language,omitempty"`
	Prompt     *string                  `json:"prompt,omitempty"`
	Timestamps *TranscriptionTimestamps `json:"timestamps,omitempty"`
}

// TranscriptionAudio is the inline audio of the JSON form.
type TranscriptionAudio struct {
	MediaType AudioInputMediaType `json:"mediaType"`
	Data      string              `json:"data"`
}

// TranscriptionJSONRequest is the JSON form of a transcription upload.
type TranscriptionJSONRequest struct {
	Model      ModelReference           `json:"model"`
	Language   *LanguageTag             `json:"language,omitempty"`
	Prompt     *string                  `json:"prompt,omitempty"`
	Timestamps *TranscriptionTimestamps `json:"timestamps,omitempty"`
	Audio      TranscriptionAudio       `json:"audio"`
}

// TranscriptionSegment is one timed span of text.
type TranscriptionSegment struct {
	StartMs int    `json:"startMs"`
	EndMs   int    `json:"endMs"`
	Text    string `json:"text"`
}

// TranscriptionWord is one timed word.
type TranscriptionWord struct {
	StartMs int    `json:"startMs"`
	EndMs   int    `json:"endMs"`
	Word    string `json:"word"`
}

// Transcription is the response to a transcription request.
type Transcription struct {
	SchemaVersion int                    `json:"schemaVersion"`
	RequestID     RequestID              `json:"requestId"`
	Model         ModelReference         `json:"model"`
	Text          string                 `json:"text"`
	Language      *LanguageTag           `json:"language,omitempty"`
	DurationMs    *int                   `json:"durationMs,omitempty"`
	Segments      []TranscriptionSegment `json:"segments,omitempty"`
	Words         []TranscriptionWord    `json:"words,omitempty"`
	Usage         []UsageQuantity        `json:"usage"`
}

/* -------------------------------------------------------------------------- */
/*  The response stream                                                       */
/* -------------------------------------------------------------------------- */

// ResponseStreamStartEvent opens a public response stream: the model that
// serves it, never the provider.
type ResponseStreamStartEvent struct {
	SchemaVersion int             `json:"schemaVersion"`
	Type          StreamEventType `json:"type"`
	RequestID     RequestID       `json:"requestId"`
	Seq           int             `json:"sequence"`
	GenerationID  *GenerationID   `json:"generationId,omitempty"`
	Model         ModelReference  `json:"model"`
	StartedAt     Timestamp       `json:"startedAt"`
}

// ResponseStreamUsageEvent reports units so far, without the deployment.
type ResponseStreamUsageEvent struct {
	SchemaVersion int             `json:"schemaVersion"`
	Type          StreamEventType `json:"type"`
	RequestID     RequestID       `json:"requestId"`
	Seq           int             `json:"sequence"`
	Units         []UsageQuantity `json:"units"`
	UsageSource   UsageSource     `json:"usageSource"`
}

// ResponseStreamErrorEvent is the terminal failure of a public stream.
type ResponseStreamErrorEvent struct {
	SchemaVersion int             `json:"schemaVersion"`
	Type          StreamEventType `json:"type"`
	RequestID     RequestID       `json:"requestId"`
	Seq           int             `json:"sequence"`
	Error         PublicError     `json:"error"`
}

/* -------------------------------------------------------------------------- */
/*  Realtime, as a public client speaks it                                    */
/* -------------------------------------------------------------------------- */

// RealtimeOpenLimits are the ceilings a client may ask for at open, each at
// most what Kaana signs a session for.
type RealtimeOpenLimits struct {
	MaxDurationMs       *int `json:"maxDurationMs,omitempty"`
	IdleTimeoutMs       *int `json:"idleTimeoutMs,omitempty"`
	MaxInputAudioBytes  *int `json:"maxInputAudioBytes,omitempty"`
	MaxOutputAudioBytes *int `json:"maxOutputAudioBytes,omitempty"`
	MaxResponses        *int `json:"maxResponses,omitempty"`
	MaxTextItems        *int `json:"maxTextItems,omitempty"`
}

// RealtimeSessionOpen is the first frame of GET /v1/realtime.
type RealtimeSessionOpen struct {
	Type            string                    `json:"type"`
	Kind            RealtimeSessionKind       `json:"kind"`
	Transport       *RealtimeSessionTransport `json:"transport,omitempty"`
	Config          RealtimeSessionConfig     `json:"config"`
	Limits          *RealtimeOpenLimits       `json:"limits,omitempty"`
	ClientSessionID *string                   `json:"clientSessionId,omitempty"`
	Labels          RequestLabels             `json:"labels,omitempty"`
}

// RealtimePublicSessionCreatedEvent opens a public session: the model that
// serves it, never the provider or deployment.
type RealtimePublicSessionCreatedEvent struct {
	SchemaVersion  int                   `json:"schemaVersion"`
	RequestID      RequestID             `json:"requestId"`
	Sequence       int                   `json:"sequence"`
	CommandID      *RealtimeCommandID    `json:"commandId,omitempty"`
	Type           string                `json:"type"`
	Model          ModelReference        `json:"model"`
	Kind           RealtimeSessionKind   `json:"kind"`
	Config         RealtimeSessionConfig `json:"config"`
	Limits         RealtimeSessionLimits `json:"limits"`
	ResumeWindowMs int                   `json:"resumeWindowMs"`
	StartedAt      Timestamp             `json:"startedAt"`
	ExpiresAt      Timestamp             `json:"expiresAt"`
}

// EventType implements RealtimeServerEvent.
func (*RealtimePublicSessionCreatedEvent) EventType() RealtimeEventType {
	return RealtimeEventSessionCreatedType
}

// Stamp implements RealtimeServerEvent.
func (e *RealtimePublicSessionCreatedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventSessionCreatedType)
}

// RealtimePublicResponseDoneEvent ends one response, without the deployment.
type RealtimePublicResponseDoneEvent struct {
	SchemaVersion int                    `json:"schemaVersion"`
	RequestID     RequestID              `json:"requestId"`
	Sequence      int                    `json:"sequence"`
	CommandID     *RealtimeCommandID     `json:"commandId,omitempty"`
	Type          string                 `json:"type"`
	ResponseID    RealtimeResponseID     `json:"responseId"`
	Status        RealtimeResponseStatus `json:"status"`
	FinishReason  *FinishReason          `json:"finishReason,omitempty"`
	Units         []UsageQuantity        `json:"units"`
	UsageSource   UsageSource            `json:"usageSource"`
}

// EventType implements RealtimeServerEvent.
func (*RealtimePublicResponseDoneEvent) EventType() RealtimeEventType {
	return RealtimeEventResponseDoneType
}

// Stamp implements RealtimeServerEvent.
func (e *RealtimePublicResponseDoneEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventResponseDoneType)
}

// RealtimePublicErrorEvent carries a public error.
type RealtimePublicErrorEvent struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     RequestID          `json:"requestId"`
	Sequence      int                `json:"sequence"`
	CommandID     *RealtimeCommandID `json:"commandId,omitempty"`
	Type          string             `json:"type"`
	Fatal         bool               `json:"fatal"`
	Error         PublicError        `json:"error"`
}

// EventType implements RealtimeServerEvent.
func (*RealtimePublicErrorEvent) EventType() RealtimeEventType { return RealtimeEventErrorType }

// Stamp implements RealtimeServerEvent.
func (e *RealtimePublicErrorEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventErrorType)
}

// RealtimePublicSessionClosedEvent ends a public session with its totals.
type RealtimePublicSessionClosedEvent struct {
	SchemaVersion int                        `json:"schemaVersion"`
	RequestID     RequestID                  `json:"requestId"`
	Sequence      int                        `json:"sequence"`
	CommandID     *RealtimeCommandID         `json:"commandId,omitempty"`
	Type          string                     `json:"type"`
	Reason        RealtimeSessionCloseReason `json:"reason"`
	Units         []UsageQuantity            `json:"units"`
	UsageSource   UsageSource                `json:"usageSource"`
	ClosedAt      Timestamp                  `json:"closedAt"`
}

// EventType implements RealtimeServerEvent.
func (*RealtimePublicSessionClosedEvent) EventType() RealtimeEventType {
	return RealtimeEventSessionClosedType
}

// Stamp implements RealtimeServerEvent.
func (e *RealtimePublicSessionClosedEvent) Stamp(h RealtimeEventHeader) {
	e.SchemaVersion, e.RequestID, e.Sequence, e.CommandID, e.Type = h.SchemaVersion, h.RequestID, h.Sequence, h.CommandID, string(RealtimeEventSessionClosedType)
}

// PublicRealtimeEvent projects one internal realtime event onto the public
// surface. The four variants that name a provider or deployment, or carry an
// internal error, are rebuilt; every other event is the same shape on both
// surfaces and passes unchanged.
func PublicRealtimeEvent(event RealtimeServerEvent) RealtimeServerEvent {
	switch e := event.(type) {
	case *RealtimeSessionCreatedEvent:
		return &RealtimePublicSessionCreatedEvent{
			Model: e.ResolvedModelReference, Kind: e.Kind, Config: e.Config, Limits: e.Limits,
			ResumeWindowMs: e.ResumeWindowMs, StartedAt: e.StartedAt, ExpiresAt: e.ExpiresAt,
		}
	case *RealtimeResponseDoneEvent:
		return &RealtimePublicResponseDoneEvent{
			ResponseID: e.ResponseID, Status: e.Status, FinishReason: e.FinishReason,
			Units: nonNilUnits(e.Units), UsageSource: e.UsageSource,
		}
	case *RealtimeErrorEvent:
		return &RealtimePublicErrorEvent{Fatal: e.Fatal, Error: *PublicErrorFrom(&e.Error)}
	case *RealtimeSessionClosedEvent:
		return &RealtimePublicSessionClosedEvent{
			Reason: e.Reason, Units: nonNilUnits(e.Units), UsageSource: e.UsageSource, ClosedAt: e.ClosedAt,
		}
	}
	return event
}

func nonNilUnits(units []UsageQuantity) []UsageQuantity {
	if units == nil {
		return []UsageQuantity{}
	}
	return units
}

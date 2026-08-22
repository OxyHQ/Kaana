package anthropic

import "encoding/json"

// The upstream wire shapes, exactly as the Anthropic Messages API defines them.
// They are separate from the contract types on purpose: this file is the only
// place in Kaana that knows what this provider's JSON looks like, and the moment
// one of its field names leaks past the adapter the boundary is gone.
//
// Every field is a pointer or a slice where the upstream may omit it, because
// the difference between "zero tokens" and "no usage reported" is the
// difference between a settled receipt and an estimated one.

// messagesRequest is the body of POST /v1/messages.
//
// MaxTokens is a plain int rather than a pointer because this protocol REQUIRES
// it. The contract makes `maxOutputTokens` optional, so the adapter refuses a
// request that omits it rather than choosing a ceiling on the customer's
// behalf — a silent cap changes what the model returns while reporting success.
type messagesRequest struct {
	Model      string           `json:"model"`
	Messages   []messageParam   `json:"messages"`
	MaxTokens  int              `json:"max_tokens"`
	Stream     bool             `json:"stream"`
	System     []systemBlock    `json:"system,omitempty"`
	Tools      []toolParam      `json:"tools,omitempty"`
	ToolChoice *toolChoiceParam `json:"tool_choice,omitempty"`

	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`
}

// systemBlock carries the system prompt, which this protocol hoists out of the
// message list entirely rather than carrying as a message with a role.
type systemBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messageParam struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type textBlockParam struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imageBlockParam struct {
	Type   string      `json:"type"`
	Source imageSource `json:"source"`
}

// imageSource is a union on `type`: `base64` carries the bytes, `url` carries a
// location the provider fetches itself.
type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type documentBlockParam struct {
	Type   string         `json:"type"`
	Source documentSource `json:"source"`
	Title  *string        `json:"title,omitempty"`
}

type documentSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// toolUseBlockParam is how an assistant's tool call is sent BACK to the
// provider. Input is a parsed object here, not the JSON text the contract
// carries, which is the one place this translation can fail on a model's own
// output — see translateToolCall.
type toolUseBlockParam struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// toolResultBlockParam answers a tool call. This protocol has no `tool` role:
// a tool result is a USER message carrying this block.
type toolResultBlockParam struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   []any  `json:"content"`
}

type toolParam struct {
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// toolChoiceParam is `{"type":"auto"|"any"|"none"|"tool", "name": …}`. The
// contract's `required` mode is this protocol's `any`.
type toolChoiceParam struct {
	Type string  `json:"type"`
	Name *string `json:"name,omitempty"`
}

/* -------------------------------------------------------------------------- */
/*  Responses                                                                 */
/* -------------------------------------------------------------------------- */

// message is a non-streamed response.
type message struct {
	Content    []contentBlock `json:"content"`
	StopReason *string        `json:"stop_reason"`
	Usage      *usage         `json:"usage"`
}

// contentBlock is one block of a complete response. `thinking` and `text` are
// different blocks carrying different channels, which is why the normalized
// stream can separate reasoning from the answer without guessing.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text"`
	Thinking *string         `json:"thinking"`
	ID       *string         `json:"id"`
	Name     *string         `json:"name"`
	Input    json.RawMessage `json:"input"`
}

/* -------------------------------------------------------------------------- */
/*  Stream events                                                             */
/* -------------------------------------------------------------------------- */

// The stream is a sequence of NAMED events rather than one repeated frame
// shape, and there is no terminal sentinel: `message_stop` ends it. Every event
// repeats its name inside the JSON, and this adapter reads the JSON `type`
// rather than the SSE `event:` name — the two always agree, and the one inside
// the payload is the one that survives a proxy that rewrites frame names.
const (
	eventMessageStart      = "message_start"
	eventContentBlockStart = "content_block_start"
	eventContentBlockDelta = "content_block_delta"
	eventContentBlockStop  = "content_block_stop"
	eventMessageDelta      = "message_delta"
	eventMessageStop       = "message_stop"
	eventPing              = "ping"
	eventError             = "error"
)

// streamEvent is every event this adapter reads, flattened.
//
// Flattened rather than decoded per name because the alternative is decoding
// each frame twice: once to learn its type and once to read it.
type streamEvent struct {
	Type string `json:"type"`

	// message_start
	Message *streamMessage `json:"message"`

	// content_block_start / _delta / _stop
	Index        *int          `json:"index"`
	ContentBlock *contentBlock `json:"content_block"`
	Delta        *streamDelta  `json:"delta"`

	// message_delta carries the stop reason and the CUMULATIVE output count.
	Usage *usage `json:"usage"`

	// error, which arrives after a 200 and therefore has no status to carry it.
	Error *errorDetail `json:"error"`
}

type streamMessage struct {
	StopReason *string `json:"stop_reason"`
	Usage      *usage  `json:"usage"`
}

// streamDelta is the union of every delta this protocol emits, plus the
// message-level delta that carries the stop reason.
type streamDelta struct {
	Type string `json:"type"`
	// text_delta
	Text *string `json:"text"`
	// thinking_delta
	Thinking *string `json:"thinking"`
	// input_json_delta: a FRAGMENT of a tool call's arguments, not valid JSON
	// on its own.
	PartialJSON *string `json:"partial_json"`
	// signature_delta carries the encrypted thinking signature. Kaana reads it
	// so it is not mistaken for output, and emits nothing: no contract event
	// has a field for provider-opaque block metadata. See README.
	Signature *string `json:"signature"`
	// message_delta
	StopReason *string `json:"stop_reason"`
}

const (
	deltaText      = "text_delta"
	deltaThinking  = "thinking_delta"
	deltaInputJSON = "input_json_delta"
	deltaSignature = "signature_delta"
	blockText      = "text"
	blockThinking  = "thinking"
	blockRedacted  = "redacted_thinking"
	blockToolUse   = "tool_use"
)

// usage is this protocol's token accounting, and its nesting is NOT the one an
// OpenAI-compatible provider uses.
//
//   - InputTokens EXCLUDES both cache fields: the published definition is
//     "number of input tokens which were not read from or used to create a
//     cache", and the documented total is
//     `cache_read + cache_creation + input_tokens`.
//   - OutputTokens INCLUDES the thinking tokens broken out in
//     OutputTokensDetails: "output_tokens remains the inclusive, authoritative
//     total used for billing".
//
// So one of the contract's two normalising subtractions applies here and the
// other must NOT — see normalizeUsage.
type usage struct {
	InputTokens              *int                 `json:"input_tokens"`
	OutputTokens             *int                 `json:"output_tokens"`
	CacheReadInputTokens     *int                 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int                 `json:"cache_creation_input_tokens"`
	OutputTokensDetails      *outputTokensDetails `json:"output_tokens_details"`
}

type outputTokensDetails struct {
	ThinkingTokens *int `json:"thinking_tokens"`
}

// errorBody is the envelope every failure arrives in, over HTTP and inside the
// stream alike.
type errorBody struct {
	Type  string      `json:"type"`
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// The error `type` values this protocol defines, per status. They are read
// rather than inferred from the status because the status alone cannot separate
// a throttle from an exhausted account.
const (
	errorRateLimit       = "rate_limit_error"
	errorBilling         = "billing_error"
	errorAuthentication  = "authentication_error"
	errorPermission      = "permission_error"
	errorNotFound        = "not_found_error"
	errorRequestTooLarge = "request_too_large"
	errorInvalidRequest  = "invalid_request_error"
	errorOverloaded      = "overloaded_error"
	errorTimeout         = "timeout_error"
	errorAPI             = "api_error"
)

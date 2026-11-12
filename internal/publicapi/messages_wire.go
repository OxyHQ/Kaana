package publicapi

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// The request and response bodies of POST /v1/messages, the
// Anthropic-Messages-compatible surface, as `@kaana.ai/contracts`
// (src/public/anthropicMessages.ts) spells them. Union-typed fields are decoded
// by hand, and every body Kaana writes is parsed by that file's Zod schemas in
// the wire-fixture gate (TestWritePublicWireFixtures, tools/contract/validate.mjs).

// messagesBlocks is a string or a list of content blocks: a message's content,
// `system`, and a tool result's content.
type messagesBlocks struct {
	text   *string
	blocks []messagesBlock
}

func (b *messagesBlocks) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		b.text = &text
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&b.blocks); err != nil {
		return err
	}
	if b.blocks == nil {
		return fieldError{"content", "content is a string or a list of content blocks"}
	}
	return nil
}

// messagesBlock is every content block of a request in one struct: which
// members a block of each type may carry is checked where it is translated.
type messagesBlock struct {
	Type      string               `json:"type"`
	Text      *string              `json:"text,omitempty"`
	Source    *messagesImageSource `json:"source,omitempty"`
	ID        *string              `json:"id,omitempty"`
	Name      *string              `json:"name,omitempty"`
	Input     json.RawMessage      `json:"input,omitempty"`
	ToolUseID *string              `json:"tool_use_id,omitempty"`
	Content   *messagesBlocks      `json:"content,omitempty"`
	IsError   *bool                `json:"is_error,omitempty"`
	// Thinking, Signature and Data are the members of an assistant's thinking
	// and redacted_thinking blocks.
	Thinking  *string `json:"thinking,omitempty"`
	Signature *string `json:"signature,omitempty"`
	Data      *string `json:"data,omitempty"`
}

// UnmarshalJSON reads the six block types Kaana carries strictly. A block of
// any other type (document, a server tool's) keeps only its type, so
// it is refused by that type at its own path rather than as an unknown member;
// its content is never kept.
func (b *messagesBlock) UnmarshalJSON(data []byte) error {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	switch head.Type {
	case "text", "image", "tool_use", "tool_result", "thinking", "redacted_thinking":
		type strict messagesBlock
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		return decoder.Decode((*strict)(b))
	}
	*b = messagesBlock{Type: head.Type}
	return nil
}

type messagesImageSource struct {
	Type      string  `json:"type"`
	MediaType *string `json:"media_type,omitempty"`
	Data      *string `json:"data,omitempty"`
	URL       *string `json:"url,omitempty"`
}

type messagesMessage struct {
	Role    string          `json:"role"`
	Content *messagesBlocks `json:"content"`
}

type messagesTool struct {
	Type        *string        `json:"type,omitempty"`
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
	Strict      *bool          `json:"strict,omitempty"`
}

// messagesToolChoice is auto, any, none or a named tool.
type messagesToolChoice struct{ choice contract.ToolChoice }

func (c *messagesToolChoice) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type string  `json:"type"`
		Name *string `json:"name,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	mode := func(mode contract.ToolChoiceMode) { c.choice = contract.ToolChoice{Mode: &mode} }
	switch {
	case wire.Type == "auto" && wire.Name == nil:
		mode(contract.ToolChoiceAuto)
	case wire.Type == "any" && wire.Name == nil:
		mode(contract.ToolChoiceRequired)
	case wire.Type == "none" && wire.Name == nil:
		mode(contract.ToolChoiceNone)
	case wire.Type == "tool" && wire.Name != nil && *wire.Name != "":
		c.choice = contract.ToolChoice{Function: &contract.ToolChoiceFunction{Type: "function", Name: *wire.Name}}
	default:
		return fieldError{"tool_choice", "tool_choice is auto, any, none or {type: tool, name}"}
	}
	return nil
}

// messagesThinking is the request's `thinking`: disabled (nothing asked),
// adaptive (reasoning with the route's own sizing) or enabled with a token
// budget (the contract's reasoning.maxTokens).
type messagesThinking struct {
	enabled bool
	budget  *int
}

func (t *messagesThinking) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type         string `json:"type"`
		BudgetTokens *int   `json:"budget_tokens,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return fieldError{"thinking", "thinking is {type: disabled}, {type: adaptive} or {type: enabled, budget_tokens}"}
	}
	switch {
	case wire.Type == "disabled" && wire.BudgetTokens == nil:
	case wire.Type == "adaptive" && wire.BudgetTokens == nil:
		t.enabled = true
	case wire.Type == "enabled" && wire.BudgetTokens != nil && *wire.BudgetTokens > 0:
		t.enabled, t.budget = true, wire.BudgetTokens
	default:
		return fieldError{"thinking", "thinking is {type: disabled}, {type: adaptive} or {type: enabled, budget_tokens} with a positive budget"}
	}
	return nil
}

type messagesOutputConfig struct {
	Effort *contract.ReasoningEffort `json:"effort,omitempty"`
}

func (c *messagesOutputConfig) UnmarshalJSON(data []byte) error {
	var wire struct {
		Effort *string `json:"effort,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if wire.Effort == nil {
		return nil
	}
	switch effort := contract.ReasoningEffort(*wire.Effort); effort {
	case contract.ReasoningEffortLow, contract.ReasoningEffortMedium, contract.ReasoningEffortHigh:
		c.Effort = &effort
		return nil
	}
	return fieldError{"output_config.effort", "output_config.effort is low, medium or high"}
}

type messagesMetadata struct {
	UserID *string `json:"user_id,omitempty"`
}

// messagesRequest is anthropicMessagesRequestSchema.
type messagesRequest struct {
	Model         modelTarget           `json:"model"`
	MaxTokens     *int                  `json:"max_tokens"`
	Messages      []messagesMessage     `json:"messages"`
	System        *messagesBlocks       `json:"system,omitempty"`
	Temperature   *float64              `json:"temperature,omitempty"`
	TopP          *float64              `json:"top_p,omitempty"`
	TopK          *int                  `json:"top_k,omitempty"`
	StopSequences []string              `json:"stop_sequences,omitempty"`
	Stream        *bool                 `json:"stream,omitempty"`
	Tools         []messagesTool        `json:"tools,omitempty"`
	ToolChoice    *messagesToolChoice   `json:"tool_choice,omitempty"`
	Thinking      *messagesThinking     `json:"thinking,omitempty"`
	OutputConfig  *messagesOutputConfig `json:"output_config,omitempty"`
	Metadata      *messagesMetadata     `json:"metadata,omitempty"`
}

/* -------------------------------------------------------------------------- */
/*  Responses                                                                 */
/* -------------------------------------------------------------------------- */

// messagesUsage is the usage block, from measured units only.
type messagesUsage struct {
	InputTokens          int  `json:"input_tokens"`
	OutputTokens         int  `json:"output_tokens"`
	CacheReadInputTokens *int `json:"cache_read_input_tokens,omitempty"`
	// CacheCreationInputTokens is present only when the route measured a cache
	// write AND the grant prices it as its own unit; otherwise the tokens are
	// in InputTokens (kaana.FoldCacheWrite).
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
}

// messagesUsageOf derives the block from the measured units. Output includes
// reasoning tokens, as Anthropic counts them; the cache read is reported only
// when the route measured one, and a cache creation only when the grant prices
// cache_write_input_tokens as its own unit.
func messagesUsageOf(units []contract.UsageQuantity) messagesUsage {
	var usage messagesUsage
	for _, quantity := range units {
		switch quantity.Unit {
		case contract.UnitInputTokens:
			usage.InputTokens += quantity.Quantity
		case contract.UnitOutputTokens, contract.UnitReasoningTokens:
			usage.OutputTokens += quantity.Quantity
		case contract.UnitCachedInputTokens:
			read := quantity.Quantity
			if usage.CacheReadInputTokens != nil {
				read += *usage.CacheReadInputTokens
			}
			usage.CacheReadInputTokens = &read
		case contract.UnitCacheWriteInputTokens:
			written := quantity.Quantity
			if usage.CacheCreationInputTokens != nil {
				written += *usage.CacheCreationInputTokens
			}
			usage.CacheCreationInputTokens = &written
		}
	}
	return usage
}

// messagesOutputBlock is a thinking, text or tool_use block of an answer.
type messagesOutputBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature *string         `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// messagesAnswer is anthropicMessageSchema.
type messagesAnswer struct {
	ID           string                `json:"id"`
	Type         string                `json:"type"`
	Role         string                `json:"role"`
	Model        string                `json:"model"`
	Content      []messagesOutputBlock `json:"content"`
	StopReason   string                `json:"stop_reason"`
	StopSequence *string               `json:"stop_sequence"`
	Usage        messagesUsage         `json:"usage"`
}

// messagesStopReason maps the finish reason Kaana measured. stop_sequence is
// never produced: the finish reason does not say which sequence ended the turn.
func messagesStopReason(reason contract.FinishReason) string {
	switch reason {
	case contract.FinishLength:
		return "max_tokens"
	case contract.FinishToolCalls:
		return "tool_use"
	case contract.FinishContentFilter, contract.FinishRefusal:
		return "refusal"
	}
	return "end_turn"
}

// messagesError is anthropicErrorSchema.
type messagesError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

// messagesErrorOf renders a public error in Anthropic's idiom. The HTTP status
// is the public code's own; only the type name is Anthropic's.
func messagesErrorOf(failure *contract.PublicError) messagesError {
	var body messagesError
	body.Type, body.RequestID = "error", string(failure.RequestID)
	body.Error.Message = failure.Message
	if failure.Param != nil {
		body.Error.Message = fmt.Sprintf("%s: %s", *failure.Param, failure.Message)
	}
	switch failure.Code {
	case "authentication_failed":
		body.Error.Type = "authentication_error"
	case "permission_denied", "insufficient_scope", "policy_violation", "commercial_permission_denied":
		body.Error.Type = "permission_error"
	case "model_not_found":
		body.Error.Type = "not_found_error"
	case "request_too_large":
		body.Error.Type = "request_too_large"
	case "insufficient_balance", "spending_limit_exceeded":
		body.Error.Type = "billing_error"
	case "rate_limited", "quota_exceeded":
		body.Error.Type = "rate_limit_error"
	case "provider_timeout":
		body.Error.Type = "timeout_error"
	case "provider_overloaded":
		body.Error.Type = "overloaded_error"
	case "provider_error", "service_unavailable", "no_route_available", "internal_error", "cancelled":
		body.Error.Type = "api_error"
	default:
		// invalid_request, unsupported_modality, context_length_exceeded,
		// output_limit_exceeded, byok_credential_invalid, upstream_content_filtered,
		// idempotency_conflict: the caller's request, whatever the status.
		body.Error.Type = "invalid_request_error"
	}
	return body
}

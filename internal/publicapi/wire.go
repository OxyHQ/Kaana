package publicapi

import (
	"bytes"
	"encoding/json"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// The request and response bodies of the text endpoints, as
// `@kaana.ai/contracts` spells them. Their union-typed fields (a model or a
// level, a string or messages, a string or a list) are decoded by hand; every
// response is parsed by the contract's own Zod schema in the wire-fixture gate
// (TestWritePublicWireFixtures, tools/contract/validate.mjs).

// fieldError is a custom decoder's refusal of one field, which decodeRequest
// reports as invalid_request on that param.
type fieldError struct {
	field   string
	message string
}

func (e fieldError) Error() string { return e.message }

// modelTarget is modelTargetSchema: a model reference or a power level.
type modelTarget struct {
	reference *contract.ModelReference
	level     *contract.PowerLevel
}

func (t *modelTarget) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fieldError{"model", "model is a model reference or a power level"}
	}
	if level, ok := contract.ParsePowerLevel(value); ok {
		t.level = &level
		return nil
	}
	reference := contract.ModelReference(value)
	if !reference.Valid() {
		return fieldError{"model", "model is publisher/model[@revision] or a power level"}
	}
	t.reference = &reference
	return nil
}

// grantTarget is the target Oxy is asked to grant.
func (t *modelTarget) grantTarget() *contract.GrantTarget {
	switch {
	case t == nil:
		return nil
	case t.level != nil:
		target := contract.LevelTarget(*t.level)
		return &target
	case t.reference != nil:
		target := contract.ModelTarget(*t.reference)
		return &target
	}
	return nil
}

// responsesInput is a string or a list of messages.
type responsesInput struct {
	messages []contract.Message
}

func (i *responsesInput) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return fieldError{"input", "input is a non-empty string or a list of messages"}
		}
		i.messages = []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Type: contract.ContentPartText, Text: &text}}}}
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&i.messages); err != nil || len(i.messages) == 0 {
		return fieldError{"input", "input is a non-empty string or a list of messages"}
	}
	return nil
}

// responsesRequest is responsesRequestSchema.
type responsesRequest struct {
	Model            *modelTarget                  `json:"model,omitempty"`
	Input            responsesInput                `json:"input"`
	MaxOutputTokens  *int                          `json:"maxOutputTokens,omitempty"`
	Stream           *bool                         `json:"stream,omitempty"`
	Temperature      *float64                      `json:"temperature,omitempty"`
	TopP             *float64                      `json:"topP,omitempty"`
	TopK             *int                          `json:"topK,omitempty"`
	FrequencyPenalty *float64                      `json:"frequencyPenalty,omitempty"`
	PresencePenalty  *float64                      `json:"presencePenalty,omitempty"`
	Seed             *int                          `json:"seed,omitempty"`
	StopSequences    []string                      `json:"stopSequences,omitempty"`
	Tools            []contract.ToolDefinition     `json:"tools,omitempty"`
	ToolChoice       *contract.ToolChoice          `json:"toolChoice,omitempty"`
	ResponseFormat   *contract.ResponseFormat      `json:"responseFormat,omitempty"`
	Reasoning        *contract.ReasoningParameters `json:"reasoning,omitempty"`
	Labels           contract.RequestLabels        `json:"labels,omitempty"`
	ClientRequestID  *string                       `json:"clientRequestId,omitempty"`
}

// response is responseSchema.
type response struct {
	SchemaVersion int                             `json:"schemaVersion"`
	RequestID     contract.RequestID              `json:"requestId"`
	GenerationID  *contract.GenerationID          `json:"generationId,omitempty"`
	Model         contract.ModelReference         `json:"model"`
	FinishReason  contract.FinishReason           `json:"finishReason"`
	Output        []contract.Message              `json:"output"`
	Usage         []contract.UsageQuantity        `json:"usage"`
	RoutingPolicy contract.RoutingPolicyReference `json:"routingPolicy"`
	LatencyMs     int                             `json:"latencyMs"`
}

/* -------------------------------------------------------------------------- */
/*  Chat completions (OpenAI-compatible)                                      */
/* -------------------------------------------------------------------------- */

// chatContent is a message's content: a string, a list of parts, or null.
type chatContent struct {
	parts []contract.ContentPart
}

func (c *chatContent) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case bytes.Equal(trimmed, []byte("null")):
		return nil
	case len(trimmed) > 0 && trimmed[0] == '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		c.parts = []contract.ContentPart{{Type: contract.ContentPartText, Text: &text}}
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&c.parts)
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role       contract.MessageRole `json:"role"`
	Content    *chatContent         `json:"content,omitempty"`
	Name       *string              `json:"name,omitempty"`
	ToolCallID *string              `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall       `json:"tool_calls,omitempty"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description *string        `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters"`
		Strict      *bool          `json:"strict,omitempty"`
	} `json:"function"`
}

// chatToolChoice is a mode or {type: function, function: {name}}.
type chatToolChoice struct{ choice contract.ToolChoice }

func (c *chatToolChoice) UnmarshalJSON(data []byte) error {
	var mode contract.ToolChoiceMode
	if json.Unmarshal(data, &mode) == nil {
		c.choice = contract.ToolChoice{Mode: &mode}
		return nil
	}
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&named); err != nil || named.Type != "function" || named.Function.Name == "" {
		return fieldError{"tool_choice", "tool_choice is auto, none, required or a named function"}
	}
	c.choice = contract.ToolChoice{Function: &contract.ToolChoiceFunction{Type: "function", Name: named.Function.Name}}
	return nil
}

// chatStop is a string or a list of strings.
type chatStop struct{ sequences []string }

func (s *chatStop) UnmarshalJSON(data []byte) error {
	var single string
	if json.Unmarshal(data, &single) == nil {
		s.sequences = []string{single}
		return nil
	}
	if err := json.Unmarshal(data, &s.sequences); err != nil || len(s.sequences) > 8 {
		return fieldError{"stop", "stop is a string or at most 8 strings"}
	}
	return nil
}

type chatResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string         `json:"name"`
		Schema map[string]any `json:"schema"`
		Strict *bool          `json:"strict,omitempty"`
	} `json:"json_schema,omitempty"`
}

type chatAudio struct {
	Voice  string `json:"voice"`
	Format string `json:"format"`
}

// chatRequest is chatCompletionsRequestSchema.
type chatRequest struct {
	Model               modelTarget               `json:"model"`
	Messages            []chatMessage             `json:"messages"`
	MaxTokens           *int                      `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                      `json:"max_completion_tokens,omitempty"`
	Temperature         *float64                  `json:"temperature,omitempty"`
	TopP                *float64                  `json:"top_p,omitempty"`
	FrequencyPenalty    *float64                  `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64                  `json:"presence_penalty,omitempty"`
	Seed                *int                      `json:"seed,omitempty"`
	Stop                *chatStop                 `json:"stop,omitempty"`
	Stream              *bool                     `json:"stream,omitempty"`
	Tools               []chatTool                `json:"tools,omitempty"`
	ToolChoice          *chatToolChoice           `json:"tool_choice,omitempty"`
	ResponseFormat      *chatResponseFormat       `json:"response_format,omitempty"`
	ReasoningEffort     *contract.ReasoningEffort `json:"reasoning_effort,omitempty"`
	Modalities          []string                  `json:"modalities,omitempty"`
	Audio               *chatAudio                `json:"audio,omitempty"`
	User                *string                   `json:"user,omitempty"`
}

type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int  `json:"cached_tokens"`
		AudioTokens  *int `json:"audio_tokens,omitempty"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int  `json:"reasoning_tokens"`
		AudioTokens     *int `json:"audio_tokens,omitempty"`
	} `json:"completion_tokens_details"`
}

type chatOutputToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatChoiceMessage struct {
	Role      string               `json:"role"`
	Content   *string              `json:"content"`
	ToolCalls []chatOutputToolCall `json:"tool_calls,omitempty"`
}

type chatChoice struct {
	Index        int               `json:"index"`
	Message      chatChoiceMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

// chatCompletion is chatCompletionSchema.
type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

type chatDeltaToolCall struct {
	Index    int     `json:"index"`
	ID       *string `json:"id,omitempty"`
	Type     *string `json:"type,omitempty"`
	Function struct {
		Name      *string `json:"name,omitempty"`
		Arguments *string `json:"arguments,omitempty"`
	} `json:"function"`
}

type chatDelta struct {
	Role      *string             `json:"role,omitempty"`
	Content   *string             `json:"content,omitempty"`
	Refusal   *string             `json:"refusal,omitempty"`
	ToolCalls []chatDeltaToolCall `json:"tool_calls,omitempty"`
}

type chatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

// chatChunk is chatCompletionChunkSchema.
type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *chatUsage        `json:"usage,omitempty"`
}

// chatError is chatCompletionErrorSchema.
type chatError struct {
	Error struct {
		Message string                   `json:"message"`
		Type    string                   `json:"type"`
		Param   *string                  `json:"param"`
		Code    contract.PublicErrorCode `json:"code"`
	} `json:"error"`
}

func chatErrorOf(failure *contract.PublicError) chatError {
	var body chatError
	body.Error.Message, body.Error.Param, body.Error.Code = failure.Message, failure.Param, failure.Code
	body.Error.Type = "invalid_request_error"
	switch {
	case failure.Code == "authentication_failed":
		body.Error.Type = "authentication_error"
	case failure.Code == "rate_limited" || failure.Code == "quota_exceeded":
		body.Error.Type = "rate_limit_error"
	case failure.Retryable || failure.Code.HTTPStatus() >= 500:
		body.Error.Type = "api_error"
	case failure.Code.HTTPStatus() == 403 || failure.Code.HTTPStatus() == 402:
		body.Error.Type = "permission_error"
	}
	return body
}

/* -------------------------------------------------------------------------- */
/*  Embeddings                                                                */
/* -------------------------------------------------------------------------- */

type embeddingsInput struct{ texts []string }

func (i *embeddingsInput) UnmarshalJSON(data []byte) error {
	var single string
	if json.Unmarshal(data, &single) == nil {
		if single == "" {
			return fieldError{"input", "input is a non-empty string"}
		}
		i.texts = []string{single}
		return nil
	}
	if err := json.Unmarshal(data, &i.texts); err != nil || len(i.texts) == 0 || len(i.texts) > 2048 {
		return fieldError{"input", "input is a string or 1 to 2048 strings"}
	}
	for _, text := range i.texts {
		if text == "" {
			return fieldError{"input", "every input is a non-empty string"}
		}
	}
	return nil
}

// embeddingsRequest is embeddingsRequestSchema.
type embeddingsRequest struct {
	Model          contract.ModelReference `json:"model"`
	Input          embeddingsInput         `json:"input"`
	EncodingFormat *string                 `json:"encoding_format,omitempty"`
	Dimensions     *int                    `json:"dimensions,omitempty"`
	User           *string                 `json:"user,omitempty"`
}

type embeddingVector struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// embeddingsResponse is embeddingsResponseSchema.
type embeddingsResponse struct {
	Object string                  `json:"object"`
	Data   []embeddingVector       `json:"data"`
	Model  contract.ModelReference `json:"model"`
	Usage  struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

/* -------------------------------------------------------------------------- */
/*  Decisions                                                                 */
/* -------------------------------------------------------------------------- */

// decisionRequest is decisionRequestSchema.
type decisionRequest struct {
	Model        *contract.ModelReference    `json:"model,omitempty"`
	State        string                      `json:"state"`
	Instructions *string                     `json:"instructions,omitempty"`
	Questions    []contract.DecisionQuestion `json:"questions"`
	Effort       *contract.DecisionEffort    `json:"effort,omitempty"`
}

// decisionSuccess is decisionSuccessSchema.
type decisionSuccess struct {
	SchemaVersion int                             `json:"schemaVersion"`
	RequestID     contract.RequestID              `json:"requestId"`
	Model         contract.ModelReference         `json:"model"`
	Data          []contract.DecisionAnswer       `json:"data"`
	Usage         []contract.UsageQuantity        `json:"usage"`
	RoutingPolicy contract.RoutingPolicyReference `json:"routingPolicy"`
}

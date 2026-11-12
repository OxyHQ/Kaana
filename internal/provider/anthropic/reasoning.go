package anthropic

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/reasoningblock"
)

// minThinkingBudget is the smallest `budget_tokens` this protocol accepts.
const minThinkingBudget = 1024

// Block kinds inside a sealed Anthropic payload.
const (
	sealedThinking = "thinking"
	sealedRedacted = "redacted"
)

// sealedBlock is what a reasoning block's continuation token carries for this
// provider family: the block exactly as the provider issued it. Every field
// is customer data and none is logged.
type sealedBlock struct {
	Kind      string `json:"t"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// sealThinking seals a finished thinking block. A block with no signature
// cannot be continued (the provider verifies the signature), so it yields no
// token and the block stays display text only.
func sealThinking(thinking, signature string) (string, bool) {
	if signature == "" {
		return "", false
	}
	token, err := reasoningblock.Seal(reasoningblock.FamilyAnthropic, sealedBlock{Kind: sealedThinking, Thinking: thinking, Signature: signature})
	return token, err == nil
}

// sealRedacted seals a redacted_thinking block, whose only content is `data`.
func sealRedacted(data string) (string, bool) {
	if data == "" {
		return "", false
	}
	token, err := reasoningblock.Seal(reasoningblock.FamilyAnthropic, sealedBlock{Kind: sealedRedacted, Data: data})
	return token, err == nil
}

// thinkingBlocks returns this protocol's blocks among an assistant message's
// reasoning parts, unmodified and in their original order. A part another
// provider family issued, one that cannot be read, and one with no token (text
// only) are dropped: only a block this provider signed can be sent back to it.
func thinkingBlocks(parts []contract.ContentPart) []any {
	var tokens []string
	for _, part := range parts {
		if part.Type == contract.ContentPartReasoning && part.Opaque != nil {
			tokens = append(tokens, *part.Opaque)
		}
	}
	var blocks []any
	for _, payload := range reasoningblock.Payloads(reasoningblock.FamilyAnthropic, tokens) {
		var sealed sealedBlock
		if json.Unmarshal(payload, &sealed) != nil {
			continue
		}
		switch sealed.Kind {
		case sealedThinking:
			blocks = append(blocks, thinkingBlockParam{Type: "thinking", Thinking: sealed.Thinking, Signature: sealed.Signature})
		case sealedRedacted:
			blocks = append(blocks, redactedThinkingBlockParam{Type: "redacted_thinking", Data: sealed.Data})
		}
	}
	return blocks
}

// continuable reports whether every assistant turn of the conversation's
// CURRENT tool loop (the messages after the last user message) that called a
// tool carries thinking blocks this adapter can send. Earlier turns need none:
// the provider drops the thinking of turns before the last user message.
func continuable(messages []contract.Message) bool {
	start := 0
	for index, message := range messages {
		if message.Role == contract.RoleUser {
			start = index + 1
		}
	}
	for _, message := range messages[start:] {
		if message.Role == contract.RoleAssistant && len(message.ToolCalls) > 0 && len(thinkingBlocks(message.Content)) == 0 {
			return false
		}
	}
	return true
}

// thinkingBudget is the `thinking` object for a caller's `reasoning.maxTokens`,
// or nil when the caller named none.
//
// The budget form is refused, by name, for every model not documented to take
// it (noThinkingBudget): current models reject it outright, and a model nobody
// reviewed is given no benefit of the doubt. The protocol's own bounds are
// refused the same way rather than letting the provider answer a 400 after the
// request has crossed the network.
func thinkingBudget(request *contract.Request, route provider.Route) (*thinkingConfig, error) {
	if request.Reasoning == nil || request.Reasoning.MaxTokens == nil {
		return nil, nil
	}
	budget := *request.Reasoning.MaxTokens
	accepted, stated := AcceptedParameters(route.UpstreamModelID)
	if !stated || !slices.Contains(accepted, provider.ParameterReasoningMaxTokens) {
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeInvalidRequest,
			Param:  "reasoning.maxTokens",
			Detail: "this model does not take a thinking token budget; use reasoning.effort, or omit reasoning to use the model's own behaviour",
		}
	}
	if budget < minThinkingBudget {
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeInvalidRequest,
			Param:  "reasoning.maxTokens",
			Detail: fmt.Sprintf("this model needs a thinking budget of at least %d tokens", minThinkingBudget),
		}
	}
	if request.MaxOutputTokens != nil && budget >= *request.MaxOutputTokens {
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeInvalidRequest,
			Param:  "reasoning.maxTokens",
			Detail: "the thinking budget must be less than maxOutputTokens",
		}
	}
	return &thinkingConfig{Type: "enabled", BudgetTokens: budget}, nil
}

package publicapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// The reasoning continuity flows (ADR 0009 decision 2), end to end through the
// real executor with a provider that reasons in two blocks: the opt-in is what
// delivers reasoning, a request that never asked keeps its old answer, and what
// a caller sends back reaches the provider in order.

type reasoningAnswer struct {
	Output []struct {
		Content []map[string]any `json:"content"`
	} `json:"output"`
}

func responsesAnswer(t *testing.T, response *reply) []map[string]any {
	t.Helper()
	body := readAll(t, response)
	var answer reasoningAnswer
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &answer) != nil || len(answer.Output) != 1 {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	return answer.Output[0].Content
}

func TestAResponseCarriesReasoningBlocksInOrderOnlyToACallerThatAskedForReasoning(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	asked := responsesAnswer(t, h.post("/v1/responses", validKey, map[string]any{"model": "stub/model", "input": "hola", "reasoning": map[string]any{}}))
	if len(asked) != 3 || asked[0]["type"] != "reasoning" || asked[0]["opaque"] != "opaque-a" || asked[0]["text"] != "think a " ||
		asked[1]["type"] != "reasoning" || asked[1]["opaque"] != "opaque-b" || asked[2]["type"] != "text" {
		t.Fatalf("content = %v", asked)
	}

	// Positive control: the same request without `reasoning` is the answer it
	// always was. A reader of an older contract refuses a part type it does not
	// know, so reasoning must never reach it unasked.
	unasked := responsesAnswer(t, h.post("/v1/responses", validKey, map[string]any{"model": "stub/model", "input": "hola"}))
	if len(unasked) != 1 || unasked[0]["type"] != "text" {
		t.Errorf("a request that never asked for reasoning got %v", unasked)
	}

	excluded := responsesAnswer(t, h.post("/v1/responses", validKey, map[string]any{"model": "stub/model", "input": "hola", "reasoning": map[string]any{"exclude": true}}))
	if len(excluded) != 1 || excluded[0]["type"] != "text" {
		t.Errorf("excluded reasoning was returned: %v", excluded)
	}
}

func TestAStreamedResponseDeliversEachBlocksTokenOnlyWhenAskedFor(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	opaque := func(reasoning any) (tokens []string, reasoningText string) {
		body := map[string]any{"model": "stub/model", "input": "hola", "stream": true}
		if reasoning != nil {
			body["reasoning"] = reasoning
		}
		for _, frame := range readSSE(t, h.post("/v1/responses", validKey, body)) {
			var event struct {
				Type    string  `json:"type"`
				Channel string  `json:"channel"`
				Text    string  `json:"text"`
				Opaque  *string `json:"opaque"`
			}
			if json.Unmarshal(frame.data, &event) != nil || event.Type != "delta" || event.Channel != "reasoning" {
				continue
			}
			reasoningText += event.Text
			if event.Opaque != nil {
				tokens = append(tokens, *event.Opaque)
			}
		}
		return tokens, reasoningText
	}
	if tokens, text := opaque(map[string]any{}); strings.Join(tokens, ",") != "opaque-a,opaque-b" || text != "think a think b " {
		t.Errorf("tokens = %v, text = %q", tokens, text)
	}
	if tokens, _ := opaque(nil); len(tokens) != 0 {
		t.Errorf("a request that never asked for reasoning was sent continuation tokens: %v", tokens)
	}
	if tokens, text := opaque(map[string]any{"exclude": true}); len(tokens) != 0 || text != "" {
		t.Errorf("excluded reasoning was streamed: %v %q", tokens, text)
	}
}

func TestAnAssistantReasoningPartReachesTheProviderInOrder(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/responses", validKey, map[string]any{"model": "stub/model", "reasoning": map[string]any{}, "input": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "q"}}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "reasoning", "text": "first", "opaque": "tok-1"},
			map[string]any{"type": "reasoning", "opaque": "tok-2"},
			map[string]any{"type": "text", "text": "answer"},
		}},
	}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, response.Body)
	}
	assistant := h.adapter.lastCall().Input.Messages[1]
	if len(assistant.Content) != 3 || assistant.Content[0].Type != contract.ContentPartReasoning || *assistant.Content[0].Opaque != "tok-1" ||
		assistant.Content[1].Type != contract.ContentPartReasoning || *assistant.Content[1].Opaque != "tok-2" || assistant.Content[2].Type != contract.ContentPartText {
		t.Errorf("assistant content = %+v", assistant.Content)
	}
}

func TestReasoningThatTheContractRefusesIsRefusedByName(t *testing.T) {
	for name, tc := range map[string]struct {
		body  map[string]any
		param string
	}{
		"an effort beside a budget": {map[string]any{"model": "stub/model", "input": "x", "reasoning": map[string]any{"effort": "low", "maxTokens": 2048}}, "reasoning.maxTokens"},
		"a zero budget":             {map[string]any{"model": "stub/model", "input": "x", "reasoning": map[string]any{"maxTokens": 0}}, "reasoning.maxTokens"},
		"an unknown member":         {map[string]any{"model": "stub/model", "input": "x", "reasoning": map[string]any{"summary": "auto"}}, "summary"},
		"reasoning on a user message": {map[string]any{"model": "stub/model", "input": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "reasoning", "text": "x"}}}}}, "input[0].content[0].type"},
		"a reasoning part with nothing": {map[string]any{"model": "stub/model", "input": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "reasoning"}}}}}, "input[0].content[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			response := h.post("/v1/responses", validKey, tc.body)
			body := readAll(t, response)
			if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), tc.param) {
				t.Errorf("%d %s, want a 400 naming %s", response.StatusCode, body, tc.param)
			}
			if h.adapter.called() != 0 {
				t.Error("a refused request reached the provider")
			}
		})
	}
}

// Chat completions has no standard field for reasoning, and a non-standard one
// would break the clients that parse it: a reasoning part is refused, and the
// answer never carries one.
func TestChatCompletionsCarryNoReasoningContinuity(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/chat/completions", validKey, map[string]any{"model": "stub/model", "messages": []any{
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "reasoning", "opaque": "tok"}}}}})
	body := readAll(t, response)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "/v1/responses and /v1/messages") {
		t.Errorf("%d %s", response.StatusCode, body)
	}
	answered := h.post("/v1/chat/completions", validKey, map[string]any{"model": "stub/model", "reasoning_effort": "low", "messages": []any{
		map[string]any{"role": "user", "content": "hi"}}})
	if text := string(readAll(t, answered)); strings.Contains(text, "opaque") || strings.Contains(text, "think a") {
		t.Errorf("a chat completion carried reasoning: %s", text)
	}
}

type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Text      string `json:"text"`
}

func TestMessagesReturnThinkingBlocksWithTheTokenAsSignatureAndAcceptThemBack(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/messages", validKey, messagesBody(map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 2048}}))
	body := readAll(t, response)
	var answer struct {
		Content []thinkingBlock `json:"content"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &answer) != nil || len(answer.Content) != 3 {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if answer.Content[0] != (thinkingBlock{Type: "thinking", Thinking: "think a ", Signature: "opaque-a"}) ||
		answer.Content[1] != (thinkingBlock{Type: "thinking", Thinking: "think b ", Signature: "opaque-b"}) || answer.Content[2].Type != "text" {
		t.Errorf("content = %+v", answer.Content)
	}
	reasoning := h.adapter.lastCall().Reasoning
	if reasoning == nil || reasoning.MaxTokens == nil || *reasoning.MaxTokens != 2048 || reasoning.HasEffort() {
		t.Errorf("reasoning = %+v", reasoning)
	}

	// Continue the conversation with the blocks exactly as they were returned,
	// and a redacted one.
	sent := messagesBody(map[string]any{"thinking": map[string]any{"type": "adaptive"}, "messages": []any{
		map[string]any{"role": "user", "content": "q"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "think a ", "signature": "opaque-a"},
			map[string]any{"type": "redacted_thinking", "data": "opaque-r"},
			map[string]any{"type": "text", "text": "ok"},
		}},
		map[string]any{"role": "user", "content": "go on"},
	}})
	if again := h.post("/v1/messages", validKey, sent); again.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", again.StatusCode, again.Body)
	}
	call := h.adapter.lastCall()
	assistant := call.Input.Messages[1]
	if len(assistant.Content) != 3 || *assistant.Content[0].Text != "think a " || *assistant.Content[0].Opaque != "opaque-a" ||
		assistant.Content[1].Text != nil || *assistant.Content[1].Opaque != "opaque-r" || assistant.Content[2].Type != contract.ContentPartText {
		t.Errorf("assistant content = %+v", assistant.Content)
	}
	if call.Reasoning == nil || call.Reasoning.MaxTokens != nil || call.Reasoning.HasEffort() {
		t.Errorf("adaptive thinking should opt in with the route's own sizing, got %+v", call.Reasoning)
	}
}

func TestStreamedMessagesOpenAThinkingBlockPerReasoningBlockBeforeTheText(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	frames := readSSE(t, h.post("/v1/messages", validKey, messagesBody(map[string]any{"stream": true, "thinking": map[string]any{"type": "adaptive"}})))
	var trace []string
	for _, frame := range frames {
		var event struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type      string `json:"type"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
			} `json:"delta"`
		}
		if json.Unmarshal(frame.data, &event) != nil {
			t.Fatalf("frame %s", frame.data)
		}
		switch event.Type {
		case "content_block_start":
			trace = append(trace, "start:"+event.ContentBlock.Type)
		case "content_block_delta":
			switch event.Delta.Type {
			case "thinking_delta":
				trace = append(trace, "thinking:"+strings.TrimSpace(event.Delta.Thinking))
			case "signature_delta":
				trace = append(trace, "signature:"+event.Delta.Signature)
			}
		case "content_block_stop":
			trace = append(trace, "stop")
		}
	}
	want := "start:thinking,thinking:think a,signature:opaque-a,stop,start:thinking,thinking:think b,signature:opaque-b,stop,start:text,stop"
	if got := strings.Join(trace, ","); got != want {
		t.Errorf("trace = %s\nwant    %s", got, want)
	}
}

func TestMessagesWithoutThinkingNeverReturnAThinkingBlock(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for name, body := range map[string]map[string]any{
		"nothing asked":     messagesBody(nil),
		"thinking disabled": messagesBody(map[string]any{"thinking": map[string]any{"type": "disabled"}}),
	} {
		response := h.post("/v1/messages", validKey, body)
		if text := string(readAll(t, response)); strings.Contains(text, "thinking") {
			t.Errorf("%s: a thinking block was returned: %s", name, text)
		}
	}
}

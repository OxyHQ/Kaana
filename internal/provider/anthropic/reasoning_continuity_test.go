package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/reasoningblock"
)

// Reasoning continuity (ADR 0009 decision 2) against a fake speaking the real
// Messages wire: the signature and the redacted payload are captured, sealed
// and forwarded unmodified, ahead of the tool_use they preceded.

// blockRecorder keeps the order of everything an adapter reports about
// reasoning.
type blockRecorder struct {
	quietEmitter
	events []string
	tokens []string
}

func (r *blockRecorder) Delta(_ int, channel contract.DeltaChannel, text string) error {
	if channel == contract.ChannelReasoning {
		r.events = append(r.events, "text:"+text)
	}
	return nil
}

func (r *blockRecorder) ReasoningBlock(_ int, opaque string) error {
	r.events = append(r.events, "block")
	r.tokens = append(r.tokens, opaque)
	return nil
}

func sealed(t *testing.T, block sealedBlock) string {
	t.Helper()
	token, err := reasoningblock.Seal(reasoningblock.FamilyAnthropic, block)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func payloadOf(t *testing.T, token string) sealedBlock {
	t.Helper()
	family, payload, err := reasoningblock.Open(token)
	if err != nil || family != reasoningblock.FamilyAnthropic {
		t.Fatalf("token = %q %v", family, err)
	}
	var block sealedBlock
	if err := json.Unmarshal(payload, &block); err != nil {
		t.Fatal(err)
	}
	return block
}

// serve runs one request against a fake upstream that answers with the given
// body, and returns what the upstream received and what the adapter reported.
func serve(t *testing.T, request *contract.Request, route provider.Route, contentType, response string) (map[string]json.RawMessage, *blockRecorder) {
	t.Helper()
	received := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(upstream.Close)
	adapter, err := New(Config{BaseURL: upstream.URL + "/v1", Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	call, err := adapter.Translate(request, route)
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	recorder := &blockRecorder{}
	if _, err := adapter.Stream(context.Background(), call, recorder, nil); err != nil {
		t.Fatalf("streaming: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(<-received, &wire); err != nil {
		t.Fatal(err)
	}
	return wire, recorder
}

const streamedThinking = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG-1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"REDACTED-2"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"done"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`

func requestFor(stream bool) *contract.Request {
	request := baseRequest("req_reasoning")
	request.Stream = stream
	request.Reasoning = &contract.ReasoningParameters{}
	return request
}

func TestAStreamedThinkingBlockIsSealedWithItsSignatureAtItsEnd(t *testing.T) {
	_, recorder := serve(t, requestFor(true), testRoute(), "text/event-stream", streamedThinking)
	if got, want := strings.Join(recorder.events, "|"), "text:let me |text:think|block|block"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	first, second := payloadOf(t, recorder.tokens[0]), payloadOf(t, recorder.tokens[1])
	if first != (sealedBlock{Kind: sealedThinking, Thinking: "let me think", Signature: "SIG-1"}) {
		t.Errorf("thinking block = %+v", first)
	}
	if second != (sealedBlock{Kind: sealedRedacted, Data: "REDACTED-2"}) {
		t.Errorf("redacted block = %+v", second)
	}
	for _, token := range recorder.tokens {
		if strings.Contains(token, "SIG-1") {
			t.Error("the token is a plain concatenation, not an opaque encoding")
		}
	}
}

func TestAWholeThinkingResponseIsSealedTheSameWay(t *testing.T) {
	response := `{"id":"m","type":"message","role":"assistant","model":"claude-fake","content":[
	  {"type":"thinking","thinking":"plan","signature":"SIG-A"},
	  {"type":"redacted_thinking","data":"RED-B"},
	  {"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`
	_, recorder := serve(t, requestFor(false), testRoute(), "application/json", response)
	if got, want := strings.Join(recorder.events, "|"), "text:plan|block|block"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if payloadOf(t, recorder.tokens[0]) != (sealedBlock{Kind: sealedThinking, Thinking: "plan", Signature: "SIG-A"}) ||
		payloadOf(t, recorder.tokens[1]) != (sealedBlock{Kind: sealedRedacted, Data: "RED-B"}) {
		t.Errorf("tokens = %v", recorder.tokens)
	}
}

func TestAThinkingBlockWithNoSignatureYieldsNoToken(t *testing.T) {
	response := `{"content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
	_, recorder := serve(t, requestFor(false), testRoute(), "application/json", response)
	if len(recorder.tokens) != 0 {
		t.Errorf("a block the provider cannot verify was sealed: %v", recorder.tokens)
	}
}

func part(text, opaque string) contract.ContentPart {
	p := contract.ContentPart{Type: contract.ContentPartReasoning}
	if text != "" {
		p.Text = &text
	}
	if opaque != "" {
		p.Opaque = &opaque
	}
	return p
}

func textOf(text string) contract.ContentPart {
	return contract.ContentPart{Type: contract.ContentPartText, Text: &text}
}

// toolTurn is a conversation whose assistant turn reasoned, said something and
// called a tool, the shape Anthropic requires its thinking blocks back for.
func toolTurn(parts ...contract.ContentPart) []contract.Message {
	question := "q"
	return []contract.Message{
		{Role: contract.RoleUser, Content: []contract.ContentPart{textOf(question)}},
		{Role: contract.RoleAssistant, Content: parts, ToolCalls: []contract.ToolCall{{ID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`}}},
		{Role: contract.RoleTool, ToolCallID: ptr("call_1"), Content: []contract.ContentPart{textOf("42")}},
	}
}

func ptr(value string) *string { return &value }

func assistantContent(t *testing.T, body messagesRequest) []map[string]any {
	t.Helper()
	for _, message := range body.Messages {
		if message.Role == "assistant" {
			encoded, _ := json.Marshal(message.Content)
			var blocks []map[string]any
			if err := json.Unmarshal(encoded, &blocks); err != nil {
				t.Fatal(err)
			}
			return blocks
		}
	}
	t.Fatal("no assistant message was sent")
	return nil
}

func types(blocks []map[string]any) string {
	var names []string
	for _, block := range blocks {
		names = append(names, block["type"].(string))
	}
	return strings.Join(names, ",")
}

func TestReasoningBlocksAreForwardedUnmodifiedAheadOfTheToolUse(t *testing.T) {
	request := requestFor(false)
	request.Input.Messages = toolTurn(
		textOf("calling"),
		part("shown", sealed(t, sealedBlock{Kind: sealedThinking, Thinking: "plan", Signature: "SIG-1"})),
		part("", sealed(t, sealedBlock{Kind: sealedRedacted, Data: "RED-2"})),
	)
	body, _ := translate(t, request)
	blocks := assistantContent(t, body)
	if got := types(blocks); got != "thinking,redacted_thinking,text,tool_use" {
		t.Fatalf("assistant blocks = %s", got)
	}
	if blocks[0]["thinking"] != "plan" || blocks[0]["signature"] != "SIG-1" || blocks[1]["data"] != "RED-2" {
		t.Errorf("a block was modified on the way back: %v %v", blocks[0], blocks[1])
	}
}

func TestAForeignOrUnreadableBlockIsDroppedAndNeverSent(t *testing.T) {
	foreign, _ := reasoningblock.Seal(reasoningblock.FamilyOpenRouter, map[string]string{"type": "reasoning.text"})
	request := requestFor(false)
	request.Input.Messages = toolTurn(
		part("x", foreign),
		part("y", "not-a-token"),
		part("text only, nothing to continue", ""),
		part("", sealed(t, sealedBlock{Kind: sealedThinking, Thinking: "mine", Signature: "SIG"})),
		textOf("hi"),
	)
	body, _ := translate(t, request)
	blocks := assistantContent(t, body)
	if got := types(blocks); got != "thinking,text,tool_use" {
		t.Fatalf("assistant blocks = %s", got)
	}
	if blocks[0]["signature"] != "SIG" {
		t.Errorf("blocks = %v", blocks)
	}
}

func TestAnAssistantTurnOfOnlyForeignReasoningIsDroppedLikeAnEmptyMessage(t *testing.T) {
	foreign, _ := reasoningblock.Seal(reasoningblock.FamilyOpenRouter, 1)
	request := requestFor(false)
	request.Input.Messages = []contract.Message{
		{Role: contract.RoleUser, Content: []contract.ContentPart{textOf("a")}},
		{Role: contract.RoleAssistant, Content: []contract.ContentPart{part("", foreign)}},
		{Role: contract.RoleUser, Content: []contract.ContentPart{textOf("b")}},
	}
	body, _ := translate(t, request)
	for _, message := range body.Messages {
		if message.Role == "assistant" {
			t.Errorf("an assistant message with nothing to say was sent: %+v", message)
		}
	}
}

func budgetRoute(model string) provider.Route {
	route := testRoute()
	route.UpstreamModelID = model
	return route
}

func budgetRequest(tokens int, messages []contract.Message) *contract.Request {
	request := requestFor(false)
	request.Reasoning = &contract.ReasoningParameters{MaxTokens: &tokens}
	limit := 4096
	request.MaxOutputTokens = &limit
	if messages != nil {
		request.Input.Messages = messages
	}
	return request
}

func thinkingOf(t *testing.T, request *contract.Request, model string) *thinkingConfig {
	t.Helper()
	call, err := newTestAdapter(t).Translate(request, budgetRoute(model))
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	var body messagesRequest
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Thinking
}

func TestAThinkingBudgetIsSentOnlyToAModelDocumentedToTakeIt(t *testing.T) {
	for _, model := range []string{"claude-haiku-4-5-20251001", "claude-opus-4-5-20251101", "claude-opus-4-6", "claude-sonnet-4-6"} {
		thinking := thinkingOf(t, budgetRequest(2048, nil), model)
		if thinking == nil || thinking.Type != "enabled" || thinking.BudgetTokens != 2048 {
			t.Errorf("%s: thinking = %+v", model, thinking)
		}
	}
	for _, model := range []string{"claude-opus-4-7", "claude-opus-4-8", "claude-opus-5", "claude-opus-5-5", "claude-sonnet-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1", "claude-unreviewed-9"} {
		_, err := newTestAdapter(t).Translate(budgetRequest(2048, nil), budgetRoute(model))
		var unsupported provider.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Param != "reasoning.maxTokens" || unsupported.Code != contract.CodeInvalidRequest {
			t.Errorf("%s: err = %v, want a refusal naming reasoning.maxTokens", model, err)
		}
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "anthropic") {
			t.Errorf("%s: the refusal names the provider: %v", model, err)
		}
	}
}

func TestAThinkingBudgetOutsideTheProtocolsBoundsIsRefusedBeforeTheCall(t *testing.T) {
	for name, tokens := range map[string]int{"below the minimum": 512, "not below max tokens": 4096} {
		_, err := newTestAdapter(t).Translate(budgetRequest(tokens, nil), budgetRoute("claude-haiku-4-5-20251001"))
		var unsupported provider.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Param != "reasoning.maxTokens" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Anthropic rejects a thinking-enabled request whose current tool turn lacks
// its own thinking blocks. After a failover or a model switch those blocks are
// another provider's, so the budget is left off for that request and the
// conversation continues rather than failing.
func TestThinkingIsLeftOffRatherThanSendAToolTurnWithoutItsBlocks(t *testing.T) {
	foreign, _ := reasoningblock.Seal(reasoningblock.FamilyOpenRouter, 1)
	const model = "claude-haiku-4-5-20251001"

	withOwn := toolTurn(part("", sealed(t, sealedBlock{Kind: sealedThinking, Thinking: "t", Signature: "S"})), textOf("hi"))
	if thinking := thinkingOf(t, budgetRequest(2048, withOwn), model); thinking == nil {
		t.Error("control: a tool turn with its own blocks lost the budget")
	}
	for name, parts := range map[string][]contract.ContentPart{
		"foreign blocks": {part("", foreign), textOf("hi")},
		"no blocks":      {textOf("hi")},
	} {
		if thinking := thinkingOf(t, budgetRequest(2048, toolTurn(parts...)), model); thinking != nil {
			t.Errorf("%s: thinking was enabled for a tool turn with nothing to send: %+v", name, thinking)
		}
	}

	// An earlier loop's turn needs none: the provider ignores thinking before the
	// last user message.
	earlier := append(toolTurn(textOf("old")), contract.Message{Role: contract.RoleUser, Content: []contract.ContentPart{textOf("next")}})
	if thinking := thinkingOf(t, budgetRequest(2048, earlier), model); thinking == nil {
		t.Error("a finished tool loop without blocks disabled thinking for the next question")
	}
}

func TestAnEffortOnlyRequestNeverSynthesisesThinking(t *testing.T) {
	request := requestFor(false)
	request.Reasoning = &contract.ReasoningParameters{Effort: contract.EffortPtr(contract.ReasoningEffortLow)}
	if thinking := thinkingOf(t, request, "claude-opus-5"); thinking != nil {
		t.Errorf("thinking = %+v", thinking)
	}
}

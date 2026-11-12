package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/reasoningblock"
)

// Reasoning continuity (ADR 0009 decision 2) for OpenRouter's ordered
// `reasoning_details`: read from a stream or a whole message, sealed one block
// per entry, and forwarded unmodified and in order on the assistant message of
// the next turn.

type detailsRecorder struct {
	events []string
	tokens []string
}

func (r *detailsRecorder) Start(contract.ModelReference, time.Time) error { return nil }
func (r *detailsRecorder) Delta(_ int, channel contract.DeltaChannel, text string) error {
	if channel == contract.ChannelReasoning {
		r.events = append(r.events, "text:"+text)
	}
	return nil
}
func (r *detailsRecorder) ReasoningBlock(_ int, opaque string) error {
	r.events = append(r.events, "block")
	r.tokens = append(r.tokens, opaque)
	return nil
}
func (r *detailsRecorder) ToolCall(provider.ToolCallDelta) error                      { return nil }
func (r *detailsRecorder) Usage([]contract.UsageQuantity, contract.UsageSource) error { return nil }

func detailOf(t *testing.T, token string) map[string]any {
	t.Helper()
	family, payload, err := reasoningblock.Open(token)
	if err != nil || family != reasoningblock.FamilyOpenRouter {
		t.Fatalf("token family = %q, %v", family, err)
	}
	var entry map[string]any
	if err := json.Unmarshal(payload, &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

func runOpenRouter(t *testing.T, request *contract.Request, handler http.HandlerFunc) (*detailsRecorder, []byte) {
	t.Helper()
	var sent []byte
	adapter, _ := newOpenRouterAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		handler(w, r)
	})
	call, err := adapter.Translate(request, openRouterGeminiRoute(nil))
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	recorder := &detailsRecorder{}
	if _, err := adapter.Stream(context.Background(), call, recorder, nil); err != nil {
		t.Fatalf("streaming: %v", err)
	}
	return recorder, sent
}

const streamedDetails = "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"let \",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"let \",\"index\":0,\"format\":\"anthropic-claude-v1\"}]}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"think\",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"think\",\"index\":0}]}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_details\":[{\"type\":\"reasoning.text\",\"signature\":\"SIG-1\",\"index\":0},{\"type\":\"reasoning.encrypted\",\"data\":\"ENC-2\",\"index\":1,\"id\":\"rs_2\"}]}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func streamingRequest() *contract.Request {
	request := requestWith([]contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("think")}}})
	request.Reasoning = &contract.ReasoningParameters{}
	return request
}

func TestStreamedReasoningDetailsAreJoinedPerEntryAndSealedAtTheEnd(t *testing.T) {
	request := streamingRequest()
	request.Stream = true
	recorder, _ := runOpenRouter(t, request, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamedDetails))
	})
	if got, want := strings.Join(recorder.events, "|"), "block|block"; got != want {
		// The text arrives on the `reasoning` field of the delta, which this
		// recorder sees as text events before the blocks.
		if !strings.HasSuffix(got, want) {
			t.Fatalf("events = %s", got)
		}
	}
	if len(recorder.tokens) != 2 {
		t.Fatalf("tokens = %d", len(recorder.tokens))
	}
	first, second := detailOf(t, recorder.tokens[0]), detailOf(t, recorder.tokens[1])
	if first["text"] != "let think" || first["signature"] != "SIG-1" || first["format"] != "anthropic-claude-v1" || first["type"] != "reasoning.text" {
		t.Errorf("first entry = %v", first)
	}
	if second["type"] != "reasoning.encrypted" || second["data"] != "ENC-2" || second["id"] != "rs_2" {
		t.Errorf("second entry = %v", second)
	}
}

func TestWholeReasoningDetailsAreSealedInOrder(t *testing.T) {
	whole := streamingRequest()
	whole.Stream = false
	recorder, _ := runOpenRouter(t, whole, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok","reasoning":"plan",
		 "reasoning_details":[{"type":"reasoning.summary","summary":"s","index":0},{"type":"reasoning.encrypted","data":"E","index":1}]},"finish_reason":"stop"}],
		 "usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	})
	if got, want := strings.Join(recorder.events, "|"), "text:plan|block|block"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if detailOf(t, recorder.tokens[0])["type"] != "reasoning.summary" || detailOf(t, recorder.tokens[1])["data"] != "E" {
		t.Errorf("tokens = %v", recorder.tokens)
	}
}

func sealedDetail(t *testing.T, entry map[string]any) string {
	t.Helper()
	token, err := reasoningblock.Seal(reasoningblock.FamilyOpenRouter, entry)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func reasoningPartOf(opaque string) contract.ContentPart {
	return contract.ContentPart{Type: contract.ContentPartReasoning, Opaque: &opaque}
}

func answered(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
}

func TestReasoningDetailsAreForwardedUnmodifiedAndInOrderOnTheAssistantMessage(t *testing.T) {
	foreign, _ := reasoningblock.Seal(reasoningblock.FamilyAnthropic, map[string]string{"t": "thinking"})
	first := map[string]any{"type": "reasoning.text", "text": "plan", "signature": "SIG", "index": float64(0), "format": "anthropic-claude-v1"}
	second := map[string]any{"type": "reasoning.encrypted", "data": "ENC", "index": float64(1)}
	request := streamingRequest()
	request.Stream = false
	request.Input.Messages = []contract.Message{
		{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("q")}},
		{Role: contract.RoleAssistant,
			Content:   []contract.ContentPart{reasoningPartOf(sealedDetail(t, first)), reasoningPartOf(foreign), reasoningPartOf("not-a-token"), reasoningPartOf(sealedDetail(t, second)), textPartOf("calling")},
			ToolCalls: []contract.ToolCall{{ID: "call_1", Name: "lookup", Arguments: "{}"}}},
		{Role: contract.RoleTool, ToolCallID: &[]string{"call_1"}[0], Content: []contract.ContentPart{textPartOf("42")}},
	}
	_, sent := runOpenRouter(t, request, answered)
	var wire struct {
		Messages []struct {
			Role             string           `json:"role"`
			Content          any              `json:"content"`
			ReasoningDetails []map[string]any `json:"reasoning_details"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(sent, &wire); err != nil {
		t.Fatal(err)
	}
	assistant := wire.Messages[1]
	if len(assistant.ReasoningDetails) != 2 {
		t.Fatalf("reasoning_details = %v", assistant.ReasoningDetails)
	}
	for index, want := range []map[string]any{first, second} {
		got, _ := json.Marshal(assistant.ReasoningDetails[index])
		expected, _ := json.Marshal(want)
		if string(got) != string(expected) {
			t.Errorf("entry %d changed on the way back: %s != %s", index, got, expected)
		}
	}
	if assistant.Content != "calling" {
		t.Errorf("a reasoning part leaked into the content: %v", assistant.Content)
	}
	if strings.Contains(string(sent), "not-a-token") || strings.Contains(string(sent), `"type":"reasoning"`) {
		t.Errorf("a reasoning part or an unreadable token reached the upstream: %s", sent)
	}
}

func TestADirectProviderNeverReceivesReasoningDetailsOrAReasoningPart(t *testing.T) {
	entry := sealedDetail(t, map[string]any{"type": "reasoning.text", "text": "x"})
	request := streamingRequest()
	request.Reasoning = nil
	request.Stream = false
	request.Input.Messages = []contract.Message{
		{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("q")}},
		{Role: contract.RoleAssistant, Content: []contract.ContentPart{reasoningPartOf(entry), textPartOf("a")}},
	}
	wire := sendThroughRealWire(t, "openai", request)
	if strings.Contains(string(wire["messages"]), "reasoning") {
		t.Errorf("a direct provider received reasoning: %s", wire["messages"])
	}
}

func TestOpenRouterTakesAThinkingBudgetAsItsMaxTokensAndDirectProvidersRefuseIt(t *testing.T) {
	budget := 2048
	request := streamingRequest()
	request.Stream = false
	request.Reasoning = &contract.ReasoningParameters{MaxTokens: &budget}
	wire := sendThroughRealWire(t, "openrouter", request)
	if string(wire["reasoning"]) != `{"max_tokens":2048}` {
		t.Errorf("reasoning on the OpenRouter wire = %s", wire["reasoning"])
	}
	if _, leaked := wire["reasoning_effort"]; leaked {
		t.Error("an effort was invented for a budget")
	}
	for _, slug := range []contract.ProviderSlug{"openai", "groq", "xai"} {
		adapter, err := New(Config{Provider: slug, BaseURL: "https://upstream.invalid", Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
		if err != nil {
			continue
		}
		_, err = adapter.Translate(request, testRoute())
		var unsupported provider.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Param != "reasoning.maxTokens" {
			t.Errorf("%s: err = %v, want a refusal naming reasoning.maxTokens", slug, err)
		}
	}
}

func TestAnOptInWithoutAnEffortOrBudgetSendsNoReasoningFieldAnywhere(t *testing.T) {
	for _, slug := range []contract.ProviderSlug{"openrouter", "openai", "groq", "together"} {
		request := streamingRequest()
		request.Stream = false
		wire := sendThroughRealWire(t, slug, request)
		for _, field := range []string{"reasoning", "reasoning_effort"} {
			if _, present := wire[field]; present {
				t.Errorf("%s: %s was invented for a bare opt-in", slug, field)
			}
		}
	}
}

func TestAWireBudgetIsReadAsTheBudgetControlAndNotAsAnEffort(t *testing.T) {
	budget := []provider.RequestParameter{provider.ParameterReasoningMaxTokens}
	if got := wireReasoningParameters(json.RawMessage(`{"max_tokens":2048}`)); len(got) != 1 || got[0] != budget[0] {
		t.Errorf("a budget = %v", got)
	}
	if got := wireReasoningParameters(json.RawMessage(`{"effort":"low"}`)); len(got) != 1 || got[0] != provider.ParameterReasoningEffort {
		t.Errorf("an effort = %v", got)
	}
}

package publicapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

func messagesBody(extra map[string]any) map[string]any {
	body := map[string]any{"model": "stub/model", "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Hola, mundo secreto"}}}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

var lookupTool = map[string]any{"name": "lookup", "description": "Look a thing up.", "input_schema": map[string]any{"type": "object"}}

type anthropicAnswer struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Role       string `json:"role"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage map[string]int `json:"usage"`
}

type anthropicFailure struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func anthropicRefusal(t *testing.T, response *reply, status int, errorType string) anthropicFailure {
	t.Helper()
	body := readAll(t, response)
	var failure anthropicFailure
	if response.StatusCode != status || json.Unmarshal(body, &failure) != nil || failure.Type != "error" || failure.Error.Type != errorType {
		t.Fatalf("want %d %s, got %d %s", status, errorType, response.StatusCode, body)
	}
	assertNoProviderLeak(t, "an Anthropic error", body)
	return failure
}

// The Anthropic dialect, whole: a message with the revision that served it and
// usage from the measured units; the key is read from x-api-key and from
// Authorization: Bearer alike, and the request is the `responses` operation.
func TestMessagesAreServedWholeUnderBothCredentialHeaders(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	request := messagesBody(map[string]any{"system": "Be brief.", "temperature": 0.5, "top_k": 20, "stop_sequences": []any{"END"}})
	for name, send := range map[string]func() *reply{
		"x-api-key": func() *reply {
			return h.post("/v1/messages", "", request, "x-api-key", validKey, "anthropic-version", "2023-06-01")
		},
		"bearer": func() *reply { return h.post("/v1/messages", validKey, request) },
	} {
		t.Run(name, func(t *testing.T) {
			response := send()
			body := readAll(t, response)
			var answer anthropicAnswer
			if response.StatusCode != http.StatusOK || json.Unmarshal(body, &answer) != nil {
				t.Fatalf("%d %s", response.StatusCode, body)
			}
			assertNoProviderLeak(t, "a message", body)
			if answer.Type != "message" || answer.Role != "assistant" || answer.Model != string(textModel) || !strings.HasPrefix(answer.ID, "msg_") ||
				answer.StopReason != "end_turn" || len(answer.Content) != 1 || answer.Content[0].Type != "text" || answer.Content[0].Text != "chunk 0 chunk 1 chunk 2 " {
				t.Errorf("message = %s", body)
			}
			if answer.Usage["input_tokens"] != 7 || answer.Usage["output_tokens"] != 3 {
				t.Errorf("usage = %v", answer.Usage)
			}
			if _, cached := answer.Usage["cache_read_input_tokens"]; cached {
				t.Errorf("usage claims a cache read the route never measured: %v", answer.Usage)
			}
			if response.Header.Get("Request-Id") == "" || response.Header.Get("Request-Id") != response.Header.Get(publicapi.HeaderRequestID) {
				t.Errorf("request id headers = %v", response.Header)
			}
		})
	}
	if len(h.oxy.authorizationBodies()) != 2 {
		t.Fatalf("%d authorizations", len(h.oxy.authorizationBodies()))
	}
	var asked contract.InferenceAuthorizationRequest
	if err := json.Unmarshal([]byte(h.oxy.authorizationBodies()[0]), &asked); err != nil {
		t.Fatal(err)
	}
	if asked.Operation != contract.GrantResponses || asked.Client.Endpoint != "/v1/messages" || asked.Credential != validKey ||
		asked.Facts.MaxOutputTokens == nil || *asked.Facts.MaxOutputTokens != 64 || strings.Contains(h.oxy.authorizationBodies()[0], "secreto") {
		t.Errorf("authorization = %s", h.oxy.authorizationBodies()[0])
	}
	call := h.adapter.lastCall()
	if call.Client.APIFormat != contract.APIFormatResponses || call.Client.Endpoint != "/v1/messages" || call.Stream ||
		*call.MaxOutputTokens != 64 || *call.Sampling.Temperature != 0.5 || *call.Sampling.TopK != 20 || len(call.Sampling.StopSequences) != 1 {
		t.Errorf("envelope = %+v", call)
	}
	messages := call.Input.Messages
	if len(messages) != 2 || messages[0].Role != contract.RoleSystem || *messages[0].Content[0].Text != "Be brief." ||
		messages[1].Role != contract.RoleUser || *messages[1].Content[0].Text != "Hola, mundo secreto" {
		t.Errorf("messages = %+v", messages)
	}
	h.settle()
	if settlement := h.oxy.settled()["grt_1"]; settlement.Report.DeploymentID != "dep_stub_b" {
		t.Errorf("settlement = %+v", settlement)
	}
}

func TestMessagesAreRefusedWithoutAKeyOrWithARevokedOne(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	anthropicRefusal(t, h.post("/v1/messages", "", messagesBody(nil)), http.StatusUnauthorized, "authentication_error")
	anthropicRefusal(t, h.post("/v1/messages", "", messagesBody(nil), "x-api-key", revokedKey), http.StatusUnauthorized, "authentication_error")
	anthropicRefusal(t, h.post("/v1/messages", revokedKey, messagesBody(nil)), http.StatusUnauthorized, "authentication_error")
	if h.adapter.called() != 0 {
		t.Error("a refused request reached the provider")
	}
}

// A streamed message: Anthropic's event order, every frame typed as its event
// name, the usage on message_delta, and a failure after output is one error
// event and no message_stop.
func TestStreamedMessagesFollowAnthropicsEventOrder(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/messages", "", messagesBody(map[string]any{"stream": true}), "x-api-key", validKey)
	frames := readSSE(t, response)
	var names []string
	for _, frame := range frames {
		assertNoProviderLeak(t, "a stream event", frame.data)
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(frame.data, &head)
		if head.Type != frame.event {
			t.Errorf("frame %q carries type %q", frame.event, head.Type)
		}
		names = append(names, frame.event)
	}
	want := []string{"message_start", "ping", "content_block_start", "content_block_delta", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", names, want)
	}
	var delta struct {
		Delta map[string]any `json:"delta"`
		Usage map[string]int `json:"usage"`
	}
	_ = json.Unmarshal(frames[7].data, &delta)
	if delta.Delta["stop_reason"] != "end_turn" || delta.Usage["input_tokens"] != 7 || delta.Usage["output_tokens"] != 3 {
		t.Errorf("message_delta = %s", frames[7].data)
	}
	if !strings.Contains(string(frames[0].data), `"model":"`+string(textModel)+`"`) {
		t.Errorf("message_start = %s", frames[0].data)
	}

	h.adapter.failAfter = 2
	frames = readSSE(t, h.post("/v1/messages", validKey, messagesBody(map[string]any{"stream": true})))
	last := frames[len(frames)-1]
	var failure anthropicFailure
	if last.event != "error" || json.Unmarshal(last.data, &failure) != nil || failure.Error.Type != "api_error" {
		t.Fatalf("last frame = %s %s", last.event, last.data)
	}
	for _, frame := range frames {
		if frame.event == "message_stop" || frame.event == "message_delta" {
			t.Errorf("a failed stream ended as if it had succeeded: %s", frame.event)
		}
	}
	h.settle()
	if settlement := h.oxy.settled()["grt_2"]; settlement.Report.Outcome != contract.OutcomePartial {
		t.Errorf("a stream cut after output settles partial: %+v", settlement.Report)
	}

	h.adapter.failAfter = 0
	anthropicRefusal(t, h.post("/v1/messages", brokeKey, messagesBody(map[string]any{"stream": true})), http.StatusPaymentRequired, "billing_error")
}

// A tool round trip: the call comes back as a tool_use block with its input as
// an object and stop_reason tool_use; the history carrying that call and its
// result reaches the provider as an assistant tool call and a tool message.
func TestMessagesRoundTripATool(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	response := h.post("/v1/messages", validKey, messagesBody(map[string]any{"tools": []any{lookupTool}, "tool_choice": map[string]any{"type": "any"}}))
	body := readAll(t, response)
	var answer anthropicAnswer
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &answer) != nil {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if answer.StopReason != "tool_use" || len(answer.Content) != 2 || answer.Content[1].Type != "tool_use" || answer.Content[1].ID != "call_1" ||
		answer.Content[1].Name != "lookup" || string(answer.Content[1].Input) != `{"q":"x"}` {
		t.Fatalf("message = %s", body)
	}
	call := h.adapter.lastCall()
	if len(call.Tools) != 1 || call.Tools[0].Type != "function" || call.Tools[0].Name != "lookup" || call.Tools[0].Parameters["type"] != "object" ||
		call.ToolChoice == nil || *call.ToolChoice.Mode != contract.ToolChoiceRequired {
		t.Errorf("tools = %+v choice = %+v", call.Tools, call.ToolChoice)
	}

	followUp := messagesBody(map[string]any{"tools": []any{lookupTool}, "tool_choice": map[string]any{"type": "tool", "name": "lookup"}, "messages": []any{
		map[string]any{"role": "user", "content": "Find x"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "Looking."},
			map[string]any{"type": "tool_use", "id": "call_1", "name": "lookup", "input": map[string]any{"q": "x"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": []any{map[string]any{"type": "text", "text": "found"}}, "is_error": false},
			map[string]any{"type": "text", "text": "Thanks."},
		}},
	}})
	if response := h.post("/v1/messages", validKey, followUp); response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, response.Body)
	}
	call = h.adapter.lastCall()
	messages := call.Input.Messages
	if len(messages) != 4 || messages[1].Role != contract.RoleAssistant || len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].Arguments != `{"q":"x"}` ||
		messages[2].Role != contract.RoleTool || *messages[2].ToolCallID != "call_1" || *messages[2].Content[0].Text != "found" ||
		messages[3].Role != contract.RoleUser || *messages[3].Content[0].Text != "Thanks." {
		t.Errorf("messages = %+v", messages)
	}
	if call.ToolChoice.Function == nil || call.ToolChoice.Function.Name != "lookup" {
		t.Errorf("tool choice = %+v", call.ToolChoice)
	}

	frames := readSSE(t, h.post("/v1/messages", validKey, messagesBody(map[string]any{"stream": true, "tools": []any{lookupTool}})))
	var names []string
	for _, frame := range frames {
		names = append(names, frame.event)
	}
	if !strings.Contains(string(frames[len(frames)-2].data), `"stop_reason":"tool_use"`) ||
		!strings.Contains(strings.Join(names, ","), "content_block_stop,content_block_start,content_block_delta,content_block_stop,message_delta") {
		t.Fatalf("events = %v", names)
	}
	var started, delta struct {
		ContentBlock map[string]any `json:"content_block"`
		Delta        map[string]any `json:"delta"`
	}
	for _, frame := range frames {
		switch {
		case frame.event == "content_block_start" && strings.Contains(string(frame.data), "tool_use"):
			_ = json.Unmarshal(frame.data, &started)
		case frame.event == "content_block_delta" && strings.Contains(string(frame.data), "input_json_delta"):
			_ = json.Unmarshal(frame.data, &delta)
		}
	}
	if started.ContentBlock["id"] != "call_1" || started.ContentBlock["name"] != "lookup" || delta.Delta["partial_json"] != `{"q":"x"}` {
		t.Errorf("tool_use = %v %v", started, delta)
	}
}

// Each public code is answered in Anthropic's idiom with the status the public
// API gives it.
func TestMessagesErrorsKeepTheirStatusAndTakeAnthropicsTypes(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	failure := anthropicRefusal(t, h.post("/v1/messages", validKey, messagesBody(map[string]any{"model": "gpt-4o"})), http.StatusBadRequest, "invalid_request_error")
	if !strings.HasPrefix(failure.Error.Message, "model: ") {
		t.Errorf("message = %q; it names the parameter", failure.Error.Message)
	}
	missing := messagesBody(nil)
	delete(missing, "max_tokens")
	failure = anthropicRefusal(t, h.post("/v1/messages", validKey, missing), http.StatusBadRequest, "invalid_request_error")
	if !strings.HasPrefix(failure.Error.Message, "max_tokens: ") {
		t.Errorf("message = %q", failure.Error.Message)
	}
	anthropicRefusal(t, h.post("/v1/messages", validKey, messagesBody(map[string]any{"service_tier": "auto"})), http.StatusBadRequest, "invalid_request_error")
	anthropicRefusal(t, h.post("/v1/messages", brokeKey, messagesBody(nil)), http.StatusPaymentRequired, "billing_error")

	h.oxy.refusal = refuseWith(http.StatusTooManyRequests, "application/json; charset=utf-8", oxyMachineLaneRefusal, "Retry-After", "42")
	limited := h.post("/v1/messages", validKey, messagesBody(nil))
	anthropicRefusal(t, limited, http.StatusTooManyRequests, "rate_limit_error")
	if limited.Header.Get("Retry-After") != "42" {
		t.Errorf("Retry-After = %q", limited.Header.Get("Retry-After"))
	}
	h.oxy.refusal = nil

	h.adapter.failDeployment = "dep_stub_b"
	h.adapter.failAfter = 0
	// Overloaded on the first route, served on the second: still a message.
	if response := h.post("/v1/messages", validKey, messagesBody(nil)); response.StatusCode != http.StatusOK {
		t.Errorf("failover = %d %s", response.StatusCode, response.Body)
	}
}

// cache_control is refused wherever the dialect puts it, naming the path; a key
// of that name inside a tool's own schema is data, not a marker (the control).
func TestMessagesRefuseCacheControlByName(t *testing.T) {
	marker := map[string]any{"type": "ephemeral"}
	for name, tc := range map[string]struct {
		body  map[string]any
		param string
	}{
		"a text block": {messagesBody(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "x", "cache_control": marker}}}}}), "messages[0].content[0].cache_control"},
		"a tool result block": {messagesBody(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "a", "content": []any{map[string]any{"type": "text", "text": "x", "cache_control": marker}}}}}}}), "messages[0].content[0].content[0].cache_control"},
		"system":      {messagesBody(map[string]any{"system": []any{map[string]any{"type": "text", "text": "x", "cache_control": marker}}}), "system[0].cache_control"},
		"a tool":      {messagesBody(map[string]any{"tools": []any{map[string]any{"name": "a", "input_schema": map[string]any{}, "cache_control": marker}}}), "tools[0].cache_control"},
		"the request": {messagesBody(map[string]any{"cache_control": marker}), "cache_control"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			failure := anthropicRefusal(t, h.post("/v1/messages", validKey, tc.body), http.StatusBadRequest, "invalid_request_error")
			if !strings.HasPrefix(failure.Error.Message, tc.param+": ") || !strings.Contains(failure.Error.Message, "cache") {
				t.Errorf("message = %q, want it to name %s", failure.Error.Message, tc.param)
			}
			if len(h.oxy.authorizationBodies()) != 0 || h.adapter.called() != 0 {
				t.Error("a refused request was authorized or executed")
			}
		})
	}

	h := newHarness(t, harnessOptions{})
	control := messagesBody(map[string]any{"tools": []any{map[string]any{"name": "a", "input_schema": map[string]any{
		"type": "object", "properties": map[string]any{"cache_control": map[string]any{"type": "string"}}}}}})
	if response := h.post("/v1/messages", validKey, control); response.StatusCode != http.StatusOK {
		t.Errorf("a schema property named cache_control was refused: %d %s", response.StatusCode, response.Body)
	}
}

// What the contract cannot carry is refused by name, and what maps cleanly is
// carried: output_config.effort is the reasoning effort.
func TestMessagesRefuseWhatTheContractCannotCarry(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for name, tc := range map[string]struct {
		extra map[string]any
		param string
	}{
		"thinking enabled without a budget": {map[string]any{"thinking": map[string]any{"type": "enabled"}}, "thinking"},
		"thinking with a display":           {map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 2048, "display": "summarized"}}, "thinking"},
		"a budget beside an effort": {map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 2048},
			"output_config": map[string]any{"effort": "low"}}, "thinking.budget_tokens"},
		"a thinking block on a user message": {map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "thinking", "thinking": "hmm", "signature": "s"}}}}}, "messages[0].content[0]"},
		"a thinking block with nothing": {map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "", "signature": ""}}}}}, "messages[0].content[0]"},
		"effort beyond high": {map[string]any{"output_config": map[string]any{"effort": "max"}}, "output_config.effort"},
		"a server tool":      {map[string]any{"tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}}}, "tools[0].type"},
		"an image url": {map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.test/a.png"}}}}}}, "messages[0].content[0].source"},
		"a document": {map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "document", "source": map[string]any{"type": "text", "data": "x"}}}}}}, "messages[0].content[0]"},
		"parallel tool use off": {map[string]any{"tools": []any{lookupTool}, "tool_choice": map[string]any{"type": "auto", "disable_parallel_tool_use": true}}, "disable_parallel_tool_use"},
	} {
		t.Run(name, func(t *testing.T) {
			failure := anthropicRefusal(t, h.post("/v1/messages", validKey, messagesBody(tc.extra)), http.StatusBadRequest, "invalid_request_error")
			if !strings.HasPrefix(failure.Error.Message, tc.param+": ") {
				t.Errorf("message = %q, want it to name %s", failure.Error.Message, tc.param)
			}
		})
	}
	if len(h.oxy.authorizationBodies()) != 0 {
		t.Fatalf("%d authorizations for requests that were refused", len(h.oxy.authorizationBodies()))
	}

	accepted := messagesBody(map[string]any{"thinking": map[string]any{"type": "disabled"}, "output_config": map[string]any{"effort": "high"}, "metadata": map[string]any{"user_id": "u"}})
	if response := h.post("/v1/messages", validKey, accepted); response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, response.Body)
	}
	if reasoning := h.adapter.lastCall().Reasoning; reasoning == nil || !reasoning.HasEffort() || *reasoning.Effort != contract.ReasoningEffortHigh {
		t.Errorf("reasoning = %+v", reasoning)
	}
	if strings.Contains(h.oxy.authorizationBodies()[0], `"u"`) {
		t.Error("metadata.user_id reached Oxy")
	}
}

// Claude Code's built-in tool descriptions run to many thousands of characters,
// so a long description is carried whole; the bound still exists, and a
// description past it is refused naming the parameter.
func TestMessagesCarryALongToolDescriptionAndRefusePastTheBound(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	long := strings.Repeat("d", 12000)
	tool := map[string]any{"name": "lookup", "description": long, "input_schema": map[string]any{"type": "object"}}
	if response := h.post("/v1/messages", validKey, messagesBody(map[string]any{"tools": []any{tool}})); response.StatusCode != http.StatusOK {
		t.Fatalf("a 12,000-character description = %d %s", response.StatusCode, response.Body)
	}
	tools := h.adapter.lastCall().Tools
	if len(tools) != 1 || tools[0].Description == nil || len(*tools[0].Description) != 12000 {
		t.Fatalf("the provider saw %+v, want the whole description", tools)
	}
	tool["description"] = strings.Repeat("d", 32769)
	failure := anthropicRefusal(t, h.post("/v1/messages", validKey, messagesBody(map[string]any{"tools": []any{tool}})), http.StatusBadRequest, "invalid_request_error")
	if !strings.HasPrefix(failure.Error.Message, "tools[0].description: ") {
		t.Errorf("message = %q", failure.Error.Message)
	}
}

// An inline image is carried and counted; a refusal names the caller's own
// location, even after a tool result moved the part to another message.
func TestMessagesCarryInlineImages(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	image := func(mediaType string) map[string]any {
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": pixelPNG}}
	}
	request := messagesBody(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "a", "content": []any{image("image/png")}},
		map[string]any{"type": "text", "text": "look"}, image("image/png")}}}})
	if response := h.post("/v1/messages", validKey, request); response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, response.Body)
	}
	var asked contract.InferenceAuthorizationRequest
	_ = json.Unmarshal([]byte(h.oxy.authorizationBodies()[0]), &asked)
	if asked.Facts.InputImages == nil || *asked.Facts.InputImages != 2 {
		t.Errorf("facts = %+v", asked.Facts)
	}
	if images, _ := contract.CheckImageInput(h.adapter.lastCall().Input.Messages); images != 2 {
		t.Errorf("the provider was sent %d images", images)
	}

	request = messagesBody(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "a", "content": "ok"},
		map[string]any{"type": "text", "text": "look"}, image("image/bmp")}}}})
	failure := anthropicRefusal(t, h.post("/v1/messages", validKey, request), http.StatusBadRequest, "invalid_request_error")
	if !strings.HasPrefix(failure.Error.Message, "messages[0].content[2].source.media_type: ") || strings.Contains(failure.Error.Message, pixelPNG) {
		t.Errorf("message = %q", failure.Error.Message)
	}
}

// A browser client may send the Anthropic headers.
func TestMessagesPreflightAllowsTheAnthropicHeaders(t *testing.T) {
	h := newHarness(t, harnessOptions{origins: []string{"https://app.example"}})
	request, _ := http.NewRequest(http.MethodOptions, h.server.URL+"/v1/messages", nil)
	request.Header.Set("Origin", "https://app.example")
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "x-api-key,anthropic-version,content-type")
	allowed := h.do(request)
	if allowed.StatusCode != http.StatusNoContent || !strings.Contains(allowed.Header.Get("Access-Control-Allow-Headers"), "X-Api-Key") ||
		!strings.Contains(allowed.Header.Get("Access-Control-Allow-Headers"), "Anthropic-Version") {
		t.Errorf("preflight = %d %v", allowed.StatusCode, allowed.Header)
	}
}

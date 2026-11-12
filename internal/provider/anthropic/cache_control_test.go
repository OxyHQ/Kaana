package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// The prompt-cache breakpoint against a fake that speaks the real Messages wire:
// `cache_control: {"type":"ephemeral"}` appears on exactly the blocks, system
// block and tool that were marked, never anywhere else and never with a `ttl`,
// at most four of them, and a cache write the provider reports comes back as the
// cache-write unit through the path every other request uses.

// usageRecorder keeps the units the adapter reports.
type usageRecorder struct {
	quietEmitter
	units []contract.UsageQuantity
}

func (r *usageRecorder) Usage(units []contract.UsageQuantity, _ contract.UsageSource) error {
	r.units = append(r.units, units...)
	return nil
}

func (*usageRecorder) ReasoningBlock(int, string) error { return nil }

const cachedAnswer = `{"id":"msg_fake","type":"message","role":"assistant","model":"claude-fake",
  "content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,
  "usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":40,"cache_read_input_tokens":0,
  "cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":0}}}`

// sendMarked runs the request against the fake and returns the body the
// upstream received and the units the adapter reported.
func sendMarked(t *testing.T, request *contract.Request) (any, string, []contract.UsageQuantity) {
	t.Helper()
	received := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cachedAnswer))
	}))
	t.Cleanup(upstream.Close)
	adapter, err := New(Config{BaseURL: upstream.URL + "/v1", Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	call, err := adapter.Translate(request, testRoute())
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	recorder := &usageRecorder{}
	if _, err := adapter.Stream(context.Background(), call, recorder, nil); err != nil {
		t.Fatalf("streaming: %v", err)
	}
	raw := string(<-received)
	var wire any
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatal(err)
	}
	return wire, raw, recorder.units
}

// cacheControlPaths lists every place the key appears, as a path of object keys
// and array indexes, and the value found there, in document order.
func cacheControlPaths(node any, path string, found map[string]string) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			here := path + "." + key
			if key == "input" || key == "input_schema" {
				// A tool call's input and a tool's schema are the caller's own
				// data, where a key of this name is not a marker.
				continue
			}
			if key == "cache_control" {
				encoded, _ := json.Marshal(child)
				found[strings.TrimPrefix(path, ".")] = string(encoded)
				continue
			}
			cacheControlPaths(child, here, found)
		}
	case []any:
		for index, child := range value {
			cacheControlPaths(child, fmt.Sprintf("%s.%d", path, index), found)
		}
	}
}

func breakpoint() *bool { yes := true; return &yes }

func markable(text string, marked bool) contract.ContentPart {
	part := textPart(text)
	if marked {
		part.CacheBreakpoint = breakpoint()
	}
	return part
}

func markedRequest(requestID contract.RequestID) *contract.Request {
	request := baseRequest(requestID)
	image := contract.ContentPart{Type: contract.ContentPartImage, Source: &contract.ContentSource{Kind: contract.ContentSourceInline, MediaType: new("image/png"), Data: new("AAAA")}}
	call := "call_1"
	request.Tools = []contract.ToolDefinition{
		{Type: "function", Name: "plain", Parameters: map[string]any{"type": "object"}},
		{Type: "function", Name: "lookup", Parameters: map[string]any{"type": "object"}, CacheBreakpoint: breakpoint()},
	}
	request.Input.Messages = []contract.Message{
		{Role: contract.RoleSystem, Content: []contract.ContentPart{markable("rules", false), markable("more rules", true)}},
		{Role: contract.RoleUser, Content: []contract.ContentPart{markable("first", false), markable("second", true), image, markable("fourth", false)}},
		{Role: contract.RoleAssistant, Content: []contract.ContentPart{markable("thinking aloud", false)},
			ToolCalls: []contract.ToolCall{{ID: call, Name: "lookup", Arguments: `{"cache_control":"mine"}`}}},
		{Role: contract.RoleTool, ToolCallID: &call, Content: []contract.ContentPart{markable("one", false), markable("two", true)}},
		{Role: contract.RoleUser, Content: []contract.ContentPart{markable("last", false)}},
	}
	return request
}

// The key appears on exactly the marked tool, system block, content block and
// tool result and nowhere else, with the value {"type":"ephemeral"} and no ttl;
// the caller's own cache_control key inside a tool call's input is untouched
// data and is not among them.
func TestCacheControlAppearsExactlyOnTheMarkedBlocks(t *testing.T) {
	_, raw, units := sendMarked(t, markedRequest("req_marked"))
	var wire any
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	cacheControlPaths(wire, "", found)

	// Message 0 is the user turn (first, second*, image, fourth); message 2 is
	// the user message carrying the tool_result block, which takes the marker
	// of its marked part.
	want := map[string]string{
		"tools.1":              `{"type":"ephemeral"}`,
		"system.0":             `{"type":"ephemeral"}`,
		"messages.0.content.1": `{"type":"ephemeral"}`,
		"messages.2.content.0": `{"type":"ephemeral"}`,
	}
	if fmt.Sprint(sortedPairs(found)) != fmt.Sprint(sortedPairs(want)) {
		t.Errorf("cache_control placed at\n  %v\nwant exactly\n  %v\nbody: %s", sortedPairs(found), sortedPairs(want), raw)
	}
	if strings.Contains(raw, `"ttl"`) {
		t.Errorf("a ttl was sent: only the default 5-minute lifetime is priced: %s", raw)
	}
	if !strings.Contains(raw, `"input":{"cache_control":"mine"}`) {
		t.Errorf("the caller's own cache_control key in a tool call's input was lost: %s", raw)
	}
	if got := reported(units); got[contract.UnitCacheWriteInputTokens] != 40 || got[contract.UnitInputTokens] != 10 {
		t.Errorf("units = %v: the provider reported 40 tokens written to the cache and 10 uncached", got)
	}
}

func sortedPairs(found map[string]string) []string {
	pairs := make([]string, 0, len(found))
	for path, value := range found {
		pairs = append(pairs, path+"="+value)
	}
	slices.Sort(pairs)
	return pairs
}

// With no breakpoint nothing is sent: the control that the test above measures
// the marker and not a default.
func TestNoCacheControlIsSentWithoutABreakpoint(t *testing.T) {
	request := markedRequest("req_unmarked")
	request.Tools[1].CacheBreakpoint = nil
	for index := range request.Input.Messages {
		for part := range request.Input.Messages[index].Content {
			request.Input.Messages[index].Content[part].CacheBreakpoint = nil
		}
	}
	wire, _, _ := sendMarked(t, request)
	found := map[string]string{}
	cacheControlPaths(wire, "", found)
	if len(found) != 0 {
		t.Errorf("cache_control found at %v for a request without a breakpoint", sortedPairs(found))
	}
}

// Anthropic refuses a fifth breakpoint. The adapter sends the first four in
// prompt order (tools, system, messages) and the request still succeeds.
func TestAtMostFourBreakpointsAreSentAndTheRequestStillSucceeds(t *testing.T) {
	request := baseRequest("req_many")
	request.Tools = []contract.ToolDefinition{{Type: "function", Name: "lookup", Parameters: map[string]any{"type": "object"}, CacheBreakpoint: breakpoint()}}
	request.Input.Messages = []contract.Message{
		{Role: contract.RoleSystem, Content: []contract.ContentPart{markable("rules", true)}},
		{Role: contract.RoleUser, Content: []contract.ContentPart{markable("a", true), markable("b", true), markable("c", true), markable("d", true)}},
	}
	wire, raw, _ := sendMarked(t, request)
	found := map[string]string{}
	cacheControlPaths(wire, "", found)
	want := []string{
		`messages.0.content.0={"type":"ephemeral"}`, `messages.0.content.1={"type":"ephemeral"}`,
		`system.0={"type":"ephemeral"}`, `tools.0={"type":"ephemeral"}`,
	}
	if got := sortedPairs(found); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("cache_control at %v, want the first four in prompt order %v\nbody: %s", got, want, raw)
	}
}

// A breakpoint on a part Anthropic cannot cache is dropped rather than sent
// (it rejects cache_control on an empty text block) and the request succeeds.
func TestABreakpointOnAnEmptyTextBlockIsNotSent(t *testing.T) {
	request := baseRequest("req_empty")
	request.Input.Messages = []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{markable("", true), markable("real", true)}}}
	wire, _, _ := sendMarked(t, request)
	found := map[string]string{}
	cacheControlPaths(wire, "", found)
	if got := sortedPairs(found); len(got) != 1 {
		t.Errorf("cache_control at %v: only the non-empty block may carry one", got)
	}
}

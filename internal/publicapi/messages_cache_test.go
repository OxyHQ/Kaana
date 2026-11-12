package publicapi_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

// pricedCatalogue is the declared catalogue with stub/model pricing
// cache_write_input_tokens at the price version the harness's grants admit, so
// a request for it is one whose cache write is priced.
func pricedCatalogue(t *testing.T) *publicapi.PublicCatalogue {
	t.Helper()
	priced := strings.Replace(declaredCatalogue,
		`"unitPrices":[{"unit":"output_tokens","amount":"0.000002","per":1,"currency":"USD"}]`,
		`"unitPrices":[{"unit":"output_tokens","amount":"0.000002","per":1,"currency":"USD"},{"unit":"cache_write_input_tokens","amount":"0.0000025","per":1,"currency":"USD"}]`, 1)
	if priced == declaredCatalogue {
		t.Fatal("the stub line was not repriced: the fixture no longer matches")
	}
	catalogue, err := publicapi.ParsePublicCatalogue([]byte(priced))
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

// breakpointsOf names every breakpoint a request carries as the adapter would
// see it, in prompt order: "tool:<name>" and "message<i>.<role>.part<j>".
func breakpointsOf(request *contract.Request) []string {
	var found []string
	for _, tool := range request.Tools {
		if tool.CacheBreakpoint != nil && *tool.CacheBreakpoint {
			found = append(found, "tool:"+tool.Name)
		}
	}
	for index, message := range request.Input.Messages {
		for part, content := range message.Content {
			if content.CacheBreakpoint != nil && *content.CacheBreakpoint {
				found = append(found, fmt.Sprintf("message%d.%s.part%d", index, message.Role, part))
			}
		}
	}
	return found
}

var (
	ephemeral  = map[string]any{"type": "ephemeral"}
	ephemeral5 = map[string]any{"type": "ephemeral", "ttl": "5m"}
	ephemeral1 = map[string]any{"type": "ephemeral", "ttl": "1h"}
)

func text(content string, marker map[string]any) map[string]any {
	block := map[string]any{"type": "text", "text": content}
	if marker != nil {
		block["cache_control"] = marker
	}
	return block
}

func userBlocks(blocks ...any) []any {
	return []any{map[string]any{"role": "user", "content": blocks}}
}

func markedTool(name string, marker map[string]any) map[string]any {
	return map[string]any{"name": name, "description": "d", "input_schema": map[string]any{"type": "object"}, "cache_control": marker}
}

func sameList(got, want []string) bool { return fmt.Sprint(got) == fmt.Sprint(want) }

// On a route whose granted price version prices the cache write, a cache_control
// marker becomes the contract's breakpoint on exactly the part, system message
// or tool that carried it, and reaches the adapter's call; an unmarked sibling
// does not. The header stays empty: nothing was ignored.
func TestMessagesForwardCacheControlWhereTheWriteIsPriced(t *testing.T) {
	body := messagesBody(map[string]any{
		"system":   []any{text("rules", nil), text("more rules", ephemeral)},
		"tools":    []any{markedTool("lookup", ephemeral5), map[string]any{"name": "plain", "input_schema": map[string]any{"type": "object"}}},
		"messages": userBlocks(text("first", nil), text("second", ephemeral), text("third", nil)),
	})
	h := newHarness(t, harnessOptions{catalogue: pricedCatalogue(t)})
	response := h.post("/v1/messages", validKey, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, response.Body)
	}
	if got := response.Header.Get(publicapi.HeaderIgnoredParameters); got != "" {
		t.Errorf("%s = %q, want none: every marker was forwarded", publicapi.HeaderIgnoredParameters, got)
	}
	want := []string{"tool:lookup", "message0.system.part1", "message1.user.part1"}
	if got := breakpointsOf(h.adapter.lastCall()); !sameList(got, want) {
		t.Errorf("breakpoints at the adapter = %v, want exactly %v", got, want)
	}
	if strings.Contains(h.oxy.authorizationBodies()[0], "ephemeral") {
		t.Error("the marker reached Oxy")
	}

	// The control: the same request on a route that does not price the write
	// reaches the adapter with no breakpoint, and says so.
	unpriced := newHarness(t, harnessOptions{})
	response = unpriced.post("/v1/messages", validKey, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, response.Body)
	}
	if got := breakpointsOf(unpriced.adapter.lastCall()); len(got) != 0 {
		t.Errorf("an unpriced route was sent breakpoints: %v", got)
	}
	if got := response.Header.Get(publicapi.HeaderIgnoredParameters); got != "cache_control" {
		t.Errorf("%s = %q, want cache_control", publicapi.HeaderIgnoredParameters, got)
	}
}

// A streamed request decides the same way, and its header is set before the
// first frame.
func TestMessagesStreamForwardCacheControlWhereTheWriteIsPriced(t *testing.T) {
	body := messagesBody(map[string]any{"stream": true, "messages": userBlocks(text("hi", ephemeral))})
	h := newHarness(t, harnessOptions{catalogue: pricedCatalogue(t)})
	response := h.post("/v1/messages", validKey, body)
	if response.StatusCode != http.StatusOK || response.Header.Get(publicapi.HeaderIgnoredParameters) != "" {
		t.Fatalf("%d, %s = %q", response.StatusCode, publicapi.HeaderIgnoredParameters, response.Header.Get(publicapi.HeaderIgnoredParameters))
	}
	if got := breakpointsOf(h.adapter.lastCall()); !sameList(got, []string{"message0.user.part0"}) {
		t.Errorf("breakpoints = %v", got)
	}
}

// A one-hour marker cannot be priced (one cache-write unit, the 5-minute rate),
// so it stays ignored and named even where the write is priced; a 5-minute one
// beside it is still forwarded, and the header still names the one that was not.
func TestMessagesIgnoreAnHourLongCacheControlEvenWherePriced(t *testing.T) {
	h := newHarness(t, harnessOptions{catalogue: pricedCatalogue(t)})
	only := h.post("/v1/messages", validKey, messagesBody(map[string]any{"messages": userBlocks(text("a", ephemeral1))}))
	if only.StatusCode != http.StatusOK || only.Header.Get(publicapi.HeaderIgnoredParameters) != "cache_control" {
		t.Fatalf("%d, header %q", only.StatusCode, only.Header.Get(publicapi.HeaderIgnoredParameters))
	}
	if got := breakpointsOf(h.adapter.lastCall()); len(got) != 0 {
		t.Errorf("a 1h marker was forwarded: %v", got)
	}

	mixed := h.post("/v1/messages", validKey, messagesBody(map[string]any{"messages": userBlocks(text("a", ephemeral1), text("b", ephemeral5))}))
	if mixed.StatusCode != http.StatusOK || mixed.Header.Get(publicapi.HeaderIgnoredParameters) != "cache_control" {
		t.Fatalf("%d, header %q", mixed.StatusCode, mixed.Header.Get(publicapi.HeaderIgnoredParameters))
	}
	if got := breakpointsOf(h.adapter.lastCall()); !sameList(got, []string{"message0.user.part1"}) {
		t.Errorf("breakpoints = %v, want only the 5m one", got)
	}

	// A marker the contract has no home for: another type, a member this
	// endpoint does not know, a request-level one.
	for name, extra := range map[string]map[string]any{
		"another type":   {"messages": userBlocks(text("a", map[string]any{"type": "persistent"}))},
		"another member": {"messages": userBlocks(text("a", map[string]any{"type": "ephemeral", "scope": "global"}))},
		"the request":    {"cache_control": ephemeral},
	} {
		response := h.post("/v1/messages", validKey, messagesBody(extra))
		if response.StatusCode != http.StatusOK || response.Header.Get(publicapi.HeaderIgnoredParameters) != "cache_control" {
			t.Errorf("%s: %d, header %q", name, response.StatusCode, response.Header.Get(publicapi.HeaderIgnoredParameters))
		}
		if got := breakpointsOf(h.adapter.lastCall()); len(got) != 0 {
			t.Errorf("%s was forwarded: %v", name, got)
		}
	}
}

// Anthropic takes four breakpoints. More than four are served, the earliest in
// prompt order (tools, system, messages) are kept, and the rest are named as
// ignored: the request does not fail.
func TestMessagesCapCacheControlAtFourBreakpoints(t *testing.T) {
	blocks := []any{}
	for index := range 5 {
		blocks = append(blocks, text(fmt.Sprintf("part %d", index), ephemeral))
	}
	body := messagesBody(map[string]any{
		"system":   []any{text("rules", ephemeral)},
		"tools":    []any{markedTool("lookup", ephemeral)},
		"messages": userBlocks(blocks...),
	})
	h := newHarness(t, harnessOptions{catalogue: pricedCatalogue(t)})
	response := h.post("/v1/messages", validKey, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, response.Body)
	}
	want := []string{"tool:lookup", "message0.system.part0", "message1.user.part0", "message1.user.part1"}
	if got := breakpointsOf(h.adapter.lastCall()); !sameList(got, want) {
		t.Errorf("breakpoints = %v, want the first four in prompt order %v", got, want)
	}
	if got := response.Header.Get(publicapi.HeaderIgnoredParameters); got != "cache_control" {
		t.Errorf("%s = %q: the three dropped markers must be named", publicapi.HeaderIgnoredParameters, got)
	}

	// The control: exactly four are all forwarded and nothing is named.
	exact := messagesBody(map[string]any{"messages": userBlocks(text("a", ephemeral), text("b", ephemeral), text("c", ephemeral), text("d", ephemeral))})
	response = h.post("/v1/messages", validKey, exact)
	if got := breakpointsOf(h.adapter.lastCall()); len(got) != 4 || response.Header.Get(publicapi.HeaderIgnoredParameters) != "" {
		t.Errorf("four markers: %v, header %q", got, response.Header.Get(publicapi.HeaderIgnoredParameters))
	}
}

// A marker on a tool_result block (or on a block inside its content) is the
// breakpoint of the tool message that result became.
func TestMessagesForwardCacheControlOnAToolResult(t *testing.T) {
	conversation := func(result map[string]any) map[string]any {
		return messagesBody(map[string]any{"tools": []any{lookupTool}, "messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "a", "name": "lookup", "input": map[string]any{}}}},
			map[string]any{"role": "user", "content": []any{result}},
		}})
	}
	h := newHarness(t, harnessOptions{catalogue: pricedCatalogue(t)})
	for name, result := range map[string]map[string]any{
		"on the block":  {"type": "tool_result", "tool_use_id": "a", "cache_control": ephemeral, "content": []any{text("one", nil), text("two", nil)}},
		"inside it":     {"type": "tool_result", "tool_use_id": "a", "content": []any{text("one", nil), text("two", ephemeral)}},
		"a text result": {"type": "tool_result", "tool_use_id": "a", "cache_control": ephemeral, "content": "plain"},
	} {
		response := h.post("/v1/messages", validKey, conversation(result))
		if response.StatusCode != http.StatusOK || response.Header.Get(publicapi.HeaderIgnoredParameters) != "" {
			t.Errorf("%s: %d, header %q: %s", name, response.StatusCode, response.Header.Get(publicapi.HeaderIgnoredParameters), response.Body)
			continue
		}
		got := breakpointsOf(h.adapter.lastCall())
		want := []string{"message1.tool.part1"}
		if name == "a text result" {
			want = []string{"message1.tool.part0"}
		}
		if !sameList(got, want) {
			t.Errorf("%s: breakpoints = %v, want %v", name, got, want)
		}
	}
}

// A key named cache_control inside a tool's schema or a tool call's input is the
// caller's data: it is never read as a marker, on a priced route either.
func TestMessagesLeaveACacheControlKeyInTheCallersOwnDataAlone(t *testing.T) {
	h := newHarness(t, harnessOptions{catalogue: pricedCatalogue(t)})
	body := messagesBody(map[string]any{
		"tools": []any{map[string]any{"name": "a", "input_schema": map[string]any{
			"type": "object", "properties": map[string]any{"cache_control": map[string]any{"type": "string"}}}}},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "a", "name": "a", "input": map[string]any{"cache_control": map[string]any{"type": "ephemeral"}}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "a", "content": "ok"}}},
		},
	})
	response := h.post("/v1/messages", validKey, body)
	if response.StatusCode != http.StatusOK || response.Header.Get(publicapi.HeaderIgnoredParameters) != "" {
		t.Fatalf("%d, header %q: %s", response.StatusCode, response.Header.Get(publicapi.HeaderIgnoredParameters), response.Body)
	}
	call := h.adapter.lastCall()
	if got := breakpointsOf(call); len(got) != 0 {
		t.Errorf("a caller's own cache_control key became a breakpoint: %v", got)
	}
	if !strings.Contains(string(mustJSON(t, call.Tools[0].Parameters)), "cache_control") {
		t.Error("the caller's cache_control schema property was removed")
	}
	if !strings.Contains(call.Input.Messages[0].ToolCalls[0].Arguments, "cache_control") {
		t.Errorf("the caller's cache_control tool input was removed: %s", call.Input.Messages[0].ToolCalls[0].Arguments)
	}
}

// The executor withholds a breakpoint from a route whose price version does not
// price the write whatever endpoint carried it: a chat completion that names the
// contract's own member is served, and the unpriced route is never sent it (the
// control is the priced route, which is).
func TestTheExecutorWithholdsABreakpointFromAnUnpricedRoute(t *testing.T) {
	chat := map[string]any{"model": "stub/model", "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "hi", "cacheBreakpoint": true}}}}}
	priced := newHarness(t, harnessOptions{catalogue: pricedCatalogue(t)})
	if response := priced.post("/v1/chat/completions", validKey, chat); response.StatusCode != http.StatusOK {
		t.Fatalf("priced: %d %s", response.StatusCode, response.Body)
	}
	if got := breakpointsOf(priced.adapter.lastCall()); !sameList(got, []string{"message0.user.part0"}) {
		t.Errorf("a priced route was not sent the breakpoint: %v", got)
	}
	unpriced := newHarness(t, harnessOptions{})
	if response := unpriced.post("/v1/chat/completions", validKey, chat); response.StatusCode != http.StatusOK {
		t.Fatalf("unpriced: %d %s", response.StatusCode, response.Body)
	}
	if got := breakpointsOf(unpriced.adapter.lastCall()); len(got) != 0 {
		t.Errorf("an unpriced route was sent the breakpoint: %v", got)
	}
}

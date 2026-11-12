package publicapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestTheProviderPreferenceIsAcceptedNarrowsAndNeverWidens drives the routing
// preference through the real handlers (ADR 0008): a satisfiable preference is
// served, a preference the grant cannot satisfy is refused rather than widened,
// and the controls Kaana cannot honour are refused rather than ignored.
func TestTheProviderPreferenceIsAcceptedNarrowsAndNeverWidens(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	served := h.post("/v1/responses", validKey, responsesBody(map[string]any{
		"provider":       map[string]any{"only": []string{"stub"}, "allow_fallbacks": true, "sort": "latency"},
		"promptCacheKey": "conversation-42",
	}))
	if body := readAll(t, served); served.StatusCode != http.StatusOK {
		t.Fatalf("a satisfiable preference was refused: %d %s", served.StatusCode, body)
	}

	chat := h.post("/v1/chat/completions", validKey, map[string]any{
		"model": "stub/model", "messages": []any{map[string]any{"role": "user", "content": "hola"}},
		"provider":         map[string]any{"order": []string{"stub"}},
		"prompt_cache_key": "conversation-42",
	})
	if body := readAll(t, chat); chat.StatusCode != http.StatusOK {
		t.Fatalf("chat completions refused the preference: %d %s", chat.StatusCode, body)
	}

	before := h.adapter.called()
	narrowed := h.post("/v1/responses", validKey, responsesBody(map[string]any{
		"provider": map[string]any{"only": []string{"somebody-else"}},
	}))
	body := readAll(t, narrowed)
	if narrowed.StatusCode < 400 || !strings.Contains(string(body), "no_route_available") {
		t.Errorf("a preference no admitted route satisfies was %d %s", narrowed.StatusCode, body)
	}
	if h.adapter.called() != before {
		t.Error("a request the preference left with no route was still attempted")
	}

	for name, provider := range map[string]map[string]any{
		"sorting by price":          {"sort": "price"},
		"a price ceiling":           {"max_price": map[string]any{"prompt": 1}},
		"a data policy":             {"zdr": true},
		"a percentile object":       {"preferred_max_latency": map[string]any{"p90": 3}},
		"a made up quantization":    {"quantizations": []string{"fp3"}},
		"an empty allow-list":       {"only": []string{}},
		"a provider name with caps": {"order": []string{"Stub"}},
	} {
		response := h.post("/v1/responses", validKey, responsesBody(map[string]any{"provider": provider}))
		if body := readAll(t, response); response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s was %d %s, want 400", name, response.StatusCode, body)
		}
	}
}

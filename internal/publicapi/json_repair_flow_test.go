package publicapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

// Opt-in JSON repair (ADR 0009 decision 5, docs/json-repair.md), end to end
// through the real executor with a provider that answers a scripted text.

const fencedAnswer = "```json\n{\"name\": \"Ada\", \"tags\": [\"a\", \"b\",],}\n```"
const repairedAnswer = `{"name": "Ada", "tags": ["a", "b"]}`

func jsonRequest(repair any) map[string]any {
	format := map[string]any{"type": "json_object"}
	if repair != nil {
		format["repair"] = repair
	}
	return map[string]any{"model": "stub/model", "input": "give json", "responseFormat": format}
}

func answerText(t *testing.T, response *reply) string {
	t.Helper()
	parts := responsesAnswer(t, response)
	if len(parts) != 1 || parts[0]["type"] != "text" {
		t.Fatalf("content = %v", parts)
	}
	return parts[0]["text"].(string)
}

func TestRepairTrueRepairsAStoppedJSONAnswerAndSaysSo(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.adapter.answer = fencedAnswer
	response := h.post("/v1/responses", validKey, jsonRequest(true))
	header := response.Header.Get(publicapi.HeaderRepaired)
	if text := answerText(t, response); text != repairedAnswer || header != "true" {
		t.Fatalf("text = %q, %s = %q", text, publicapi.HeaderRepaired, header)
	}

	// Already valid JSON is returned as it is, with no marker.
	h.adapter.answer = "{\"a\": 1}\n"
	valid := h.post("/v1/responses", validKey, jsonRequest(true))
	if got := valid.Header.Get(publicapi.HeaderRepaired); got != "" {
		t.Errorf("a valid answer carries %s = %q", publicapi.HeaderRepaired, got)
	}
	if text := answerText(t, valid); text != "{\"a\": 1}\n" {
		t.Errorf("a valid answer became %q", text)
	}

	// Text no repair makes valid is returned untouched, with no marker.
	h.adapter.answer = "I cannot do that."
	refused := h.post("/v1/responses", validKey, jsonRequest(true))
	if refused.Header.Get(publicapi.HeaderRepaired) != "" || answerText(t, refused) != "I cannot do that." {
		t.Error("unrepairable text was touched or marked")
	}
}

func TestRepairIsNeverDefaultOn(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.adapter.answer = fencedAnswer
	for name, body := range map[string]map[string]any{
		"absent":    jsonRequest(nil),
		"false":     jsonRequest(false),
		"no format": {"model": "stub/model", "input": "give json"},
	} {
		response := h.post("/v1/responses", validKey, body)
		if got := response.Header.Get(publicapi.HeaderRepaired); got != "" {
			t.Errorf("%s: %s = %q", name, publicapi.HeaderRepaired, got)
		}
		if text := answerText(t, response); text != fencedAnswer {
			t.Errorf("%s: the answer was rewritten to %q", name, text)
		}
	}
}

func TestRepairNeverTouchesAnAnswerTheModelDidNotFinish(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	truncated := `{"name": "Ada", "tags": ["a", "b"`
	for _, finish := range []contract.FinishReason{contract.FinishLength, contract.FinishContentFilter, contract.FinishRefusal} {
		h.adapter.answer, h.adapter.finish = truncated, finish
		response := h.post("/v1/responses", validKey, jsonRequest(true))
		if got := response.Header.Get(publicapi.HeaderRepaired); got != "" {
			t.Errorf("finish %s: %s = %q", finish, publicapi.HeaderRepaired, got)
		}
		if text := answerText(t, response); text != truncated {
			t.Errorf("finish %s: the answer was rewritten to %q", finish, text)
		}
	}

	// The control: the same text with a normal stop is the forgotten-closers
	// case, which is repaired.
	h.adapter.answer, h.adapter.finish = truncated, contract.FinishStop
	response := h.post("/v1/responses", validKey, jsonRequest(true))
	if text := answerText(t, response); text != truncated+"]}" || response.Header.Get(publicapi.HeaderRepaired) != "true" {
		t.Errorf("a stopped answer was %q, %s = %q", text, publicapi.HeaderRepaired, response.Header.Get(publicapi.HeaderRepaired))
	}
}

func TestRepairIsRefusedWhereItCannotBeHonoured(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	streamed := jsonRequest(true)
	streamed["stream"] = true
	text := map[string]any{"model": "stub/model", "input": "x", "responseFormat": map[string]any{"type": "text", "repair": true}}
	notBoolean := jsonRequest("yes")
	for name, body := range map[string]map[string]any{"a streamed request": streamed, "a text format": text, "a non-boolean": notBoolean} {
		response := h.post("/v1/responses", validKey, body)
		raw := string(readAll(t, response))
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(raw, "invalid_request") || !strings.Contains(raw, "repair") {
			t.Errorf("%s: %d %s", name, response.StatusCode, raw)
		}
	}
	if h.adapter.called() != 0 {
		t.Errorf("a refused request reached the provider %d times", h.adapter.called())
	}

	// repair: false with a stream is the old request and still streams.
	off := jsonRequest(false)
	off["stream"] = true
	if response := h.post("/v1/responses", validKey, off); response.StatusCode != http.StatusOK {
		t.Errorf("repair: false with stream: %d", response.StatusCode)
	}
}

func TestRepairStaysKaanaSideAndNeverReachesTheProvider(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.adapter.answer = fencedAnswer
	if response := h.post("/v1/responses", validKey, jsonRequest(true)); response.StatusCode != http.StatusOK {
		t.Fatalf("%d", response.StatusCode)
	}
	call := h.adapter.lastCall()
	if call == nil || call.ResponseFormat == nil || call.ResponseFormat.Type != contract.ResponseFormatJSONObject {
		t.Fatalf("the provider saw %+v", call)
	}
	encoded, err := json.Marshal(call)
	if err != nil || strings.Contains(string(encoded), "repair") {
		t.Errorf("the signed request carries repair: %s", encoded)
	}
	for _, body := range h.oxy.authorizationBodies() {
		if strings.Contains(body, "repair") {
			t.Errorf("Oxy was sent repair: %s", body)
		}
	}
}

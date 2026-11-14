package publicapi_test

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

// catalogueBySetPath is GET /v1/models as this build renders it for a reader
// on each set a pinned release states (PinnedReaderSets), recorded for
// packages/sdk/src/__tests__/compatibility.test.ts: there each published
// release lists what it is sent, and contracts 8.2.0 refuses what a reader on
// 8.16.0 is sent, so the recording carries every value an older reader would
// refuse.
const catalogueBySetPath = "../../packages/sdk/src/__tests__/compat/catalogue-by-set.generated.json"

// catalogueWithEveryAddition is the harness's declared catalogue with every
// value of CATALOGUE_ADDITIONS: unreported capabilities and a cache-write
// price on stub/model, and a realtime line that is the default `live` line.
func catalogueWithEveryAddition(t *testing.T) *publicapi.PublicCatalogue {
	t.Helper()
	var file struct {
		SchemaVersion int              `json:"schemaVersion"`
		Models        []map[string]any `json:"models"`
	}
	if err := json.Unmarshal([]byte(declaredCatalogue), &file); err != nil {
		t.Fatal(err)
	}
	for _, model := range file.Models {
		capabilities := model["capabilities"].(map[string]any)
		switch model["modelId"] {
		case "stub/model":
			capabilities["unreported"] = []any{"parallelToolCalls", "promptCaching"}
			pricing := model["pricing"].(map[string]any)
			pricing["unitPrices"] = append(pricing["unitPrices"].([]any),
				map[string]any{"unit": "cache_write_input_tokens", "amount": "0.00000375", "per": 1, "currency": "USD"})
		case "openai/gpt-realtime-2.1":
			capabilities["realtime"] = map[string]any{"transports": []string{"websocket"},
				"sessionKinds": []string{"conversation", "live"}, "defaultKinds": []string{"live"}}
		}
	}
	raw, err := json.Marshal(map[string]any{"schemaVersion": 1, "models": file.Models})
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := publicapi.ParsePublicCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

// liveObservation is a realtime deployment that holds conversation and live
// sessions.
const liveObservation = `{"realtime":{"transports":["websocket"],"sessionKinds":["conversation","live"],"inputAudioTranscription":true,
  "audioFormats":{"input":["pcm16_24khz"],"output":["pcm16_24khz"]}},
  "audio":{"languages":[],"voices":[{"id":"marin","name":"Marin","languages":[]}]}}`

// TestTheCatalogueIsRenderedInTheReadersContractSet: GET /v1/models sends a
// value of CATALOGUE_ADDITIONS only to a request whose Kaana-Contract-Version
// is that value's set or later; a request that states none, or one that does
// not parse, is sent none. A deployed SDK pinned to an older set therefore
// keeps parsing the whole list when the catalogue gains a value, and switching
// the line that serves a kind stays a catalogue change.
func TestTheCatalogueIsRenderedInTheReadersContractSet(t *testing.T) {
	h := newHarness(t, harnessOptions{catalogue: catalogueWithEveryAddition(t), realtimeObservation: json.RawMessage(liveObservation)})
	list := func(path, stated string) []byte {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
		request.Header.Set("Authorization", "Bearer "+validKey)
		if stated != "" {
			request.Header.Set(publicapi.HeaderContractVersion, stated)
		}
		listed := h.do(request)
		if listed.StatusCode != http.StatusOK {
			t.Fatalf("%s for %q: %d %s", path, stated, listed.StatusCode, listed.Body)
		}
		return listed.Body
	}
	additions := map[string]string{
		"unreported":  `"unreported":["parallelToolCalls","promptCaching"]`,
		"cache-write": `"unit":"cache_write_input_tokens"`,
		"live":        `"sessionKinds":["conversation","live"]`,
		"defaults":    `"defaultKinds":["live"]`,
	}
	for _, reader := range []struct {
		stated string
		sent   []string
	}{
		{"", nil},
		{"not-a-set", nil},
		{"4.4.0", nil},
		{"8.2.0", nil},
		{"8.3.9", nil},
		{"8.4.0", []string{"unreported", "cache-write"}},
		{"8.15.0", []string{"unreported", "cache-write"}},
		{"8.16.0", []string{"unreported", "cache-write", "live", "defaults"}},
		{contract.ContractVersion, []string{"unreported", "cache-write", "live", "defaults"}},
		{"9.0.0", []string{"unreported", "cache-write", "live", "defaults"}},
	} {
		body := string(list("/v1/models", reader.stated))
		for name, text := range additions {
			if sent := strings.Contains(body, text); sent != slices.Contains(reader.sent, name) {
				t.Errorf("a reader on %q was sent %s: %v\n%s", reader.stated, name, sent, body)
			}
		}
		// What is left out is the addition, never the line or its other values.
		for _, kept := range []string{`"modelId":"openai/gpt-realtime-2.1"`, `"sessionKinds":["conversation`, `"unit":"output_tokens"`, `"modelId":"stub/model"`} {
			if !strings.Contains(body, kept) {
				t.Errorf("a reader on %q was not sent %s:\n%s", reader.stated, kept, body)
			}
		}
	}
	if one := string(list("/v1/models/openai/gpt-realtime-2.1", "8.2.0")); strings.Contains(one, "live") || !strings.Contains(one, `"sessionKinds":["conversation"]`) {
		t.Errorf("GET /v1/models/{model} is not rendered in the reader's set: %s", one)
	}

	recorded := make(map[string]json.RawMessage, len(publicapi.PinnedReaderSets))
	for _, set := range publicapi.PinnedReaderSets {
		recorded[publicapi.PinnedReaderKey(set)] = list("/v1/models", set)
	}
	encoded, err := json.MarshalIndent(recorded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if os.Getenv("KAANA_UPDATE_COMPAT_FIXTURES") == "1" {
		if err := os.WriteFile(catalogueBySetPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(catalogueBySetPath)
	if err != nil || !reflect.DeepEqual(masked(t, committed), masked(t, encoded)) {
		t.Fatalf("%s is not what Kaana sends; regenerate it with KAANA_UPDATE_COMPAT_FIXTURES=1 go test ./internal/publicapi -run TestTheCatalogueIsRenderedInTheReadersContractSet", catalogueBySetPath)
	}
}

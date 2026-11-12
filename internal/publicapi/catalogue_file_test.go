package publicapi_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/publicapi"
)

const catalogueFilePath = "../../configs/public-catalogue.json"

// The checked-in declared catalogue is the one Kaana serves and relays to Oxy
// (ADR 0002, amendment 2026-10-15): it must parse, and what Oxy needs to rate a
// grant must be in it.
func TestTheDeclaredCatalogueFileParsesAndCarriesListPrices(t *testing.T) {
	raw, err := os.ReadFile(catalogueFilePath)
	if err != nil {
		t.Fatalf("reading the declared catalogue: %v", err)
	}
	catalogue, err := publicapi.ParsePublicCatalogue(raw)
	if err != nil {
		t.Fatalf("the declared catalogue does not parse: %v", err)
	}

	var document struct {
		SchemaVersion int `json:"schemaVersion"`
		Models        []struct {
			ModelID  string `json:"modelId"`
			Audience string `json:"audience"`
			Pricing  *struct {
				UnitPrices []struct {
					Unit   string `json:"unit"`
					Amount string `json:"amount"`
					Per    int64  `json:"per"`
				} `json:"unitPrices"`
			} `json:"pricing"`
		} `json:"models"`
	}
	if err := json.Unmarshal(catalogue.Document(), &document); err != nil {
		t.Fatalf("the internal document does not decode: %v", err)
	}
	// Floor: the one-time migration of Oxy's catalogue was 252 models and the
	// transcription lines were added to it. An empty or truncated file would
	// otherwise parse and price nothing.
	if document.SchemaVersion != 1 || len(document.Models) < 255 {
		t.Fatalf("the document has schemaVersion %d and %d models", document.SchemaVersion, len(document.Models))
	}
	priced := map[string][]string{}
	for _, model := range document.Models {
		if model.Audience == "" {
			t.Errorf("%s carries no audience in the internal document", model.ModelID)
		}
		if model.Pricing != nil {
			for _, price := range model.Pricing.UnitPrices {
				if price.Per <= 0 || price.Amount == "" {
					t.Errorf("%s prices %s with amount %q per %d", model.ModelID, price.Unit, price.Amount, price.Per)
				}
				priced[model.ModelID] = append(priced[model.ModelID], price.Unit)
			}
		}
	}
	want := map[string]string{
		"openai/gpt-4o-transcribe":                 "input_tokens",
		"openai/gpt-4o-mini-transcribe-2025-12-15": "output_tokens",
		"x-ai/grok-voice-3.0":                      "session_milliseconds",
		"x-ai/grok-voice-transcribe-2.0":           "audio_input_milliseconds",
	}
	for model, unit := range want {
		found := false
		for _, priceUnit := range priced[model] {
			found = found || priceUnit == unit
		}
		if !found {
			t.Errorf("%s does not price %s; has %v", model, unit, priced[model])
		}
	}
}

// A declared model describes the MODEL. The fields that name a route would put
// an operator on the customer surface, so the file may not carry them.
func TestTheDeclaredCatalogueRefusesFieldsThatDescribeARoute(t *testing.T) {
	raw, err := os.ReadFile(catalogueFilePath)
	if err != nil {
		t.Fatalf("reading the declared catalogue: %v", err)
	}
	var file struct {
		SchemaVersion int                          `json:"schemaVersion"`
		Models        []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, field := range []string{"servingProviders", "availabilityScope", "commercialPermission", "currentRevision"} {
		model := map[string]json.RawMessage{}
		for name, value := range file.Models[0] {
			model[name] = value
		}
		model[field] = json.RawMessage(`[]`)
		mutated, err := json.Marshal(map[string]any{"schemaVersion": 1, "models": []any{model}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := publicapi.ParsePublicCatalogue(mutated); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("a declared model carrying %s was accepted or refused for another reason: %v", field, err)
		}
	}
	// Positive control: the unmodified first model parses on its own.
	clean, _ := json.Marshal(map[string]any{"schemaVersion": 1, "models": []any{file.Models[0]}})
	if _, err := publicapi.ParsePublicCatalogue(clean); err != nil {
		t.Errorf("the unmodified model was refused: %v", err)
	}
}

// The five capability booleans whose `false` was never evidence, in the order
// the contract lists them (UNREPORTED_CAPABILITY_KEYS, contracts 8.4.0).
var unreportableCapabilities = []string{"tools", "parallelToolCalls", "structuredOutput", "jsonMode", "promptCaching"}

// TestTheDeclaredCatalogueSaysWhichCapabilitiesItDoesNotKnow holds the
// evidence rule for the five unreportable booleans (docs/operating.md, "The
// declared catalogue"): a `false` is "not recorded" and is listed in
// `capabilities.unreported` unless testdata/capability_evidence.json cites a
// source that the capability is absent; a `true` must be in that file, with its
// source, so a new unevidenced true cannot be added unnoticed. A listed key is
// false, listed once, in the contract's order.
func TestTheDeclaredCatalogueSaysWhichCapabilitiesItDoesNotKnow(t *testing.T) {
	raw, err := os.ReadFile(catalogueFilePath)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Models []struct {
			ModelID      string                     `json:"modelId"`
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	evidenceRaw, err := os.ReadFile("testdata/capability_evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		// Evidence[model][key] is the source of a true value: a URL, or
		// "migration-2026-10-15" for a value grandfathered from Oxy's export.
		Evidence map[string]map[string]string `json:"evidence"`
		// Absent[model][key] is a source stating the capability is absent, which
		// is the only thing that lets a false stay unlisted.
		Absent map[string]map[string]string `json:"absent"`
	}
	if err := json.Unmarshal(evidenceRaw, &evidence); err != nil {
		t.Fatal(err)
	}
	position := map[string]int{}
	for index, key := range unreportableCapabilities {
		position[key] = index
	}
	seen := map[string]bool{}
	for _, model := range document.Models {
		seen[model.ModelID] = true
		var listed []string
		if encoded, ok := model.Capabilities["unreported"]; ok {
			if err := json.Unmarshal(encoded, &listed); err != nil {
				t.Errorf("%s: unreported does not decode: %v", model.ModelID, err)
				continue
			}
			if len(listed) == 0 {
				t.Errorf("%s: an empty unreported list is omitted, not written", model.ModelID)
			}
		}
		inList := map[string]bool{}
		last := -1
		for _, key := range listed {
			at, known := position[key]
			if !known {
				t.Errorf("%s: unreported lists %q, which is not one of the five", model.ModelID, key)
				continue
			}
			if inList[key] {
				t.Errorf("%s: unreported lists %s twice", model.ModelID, key)
			}
			if at <= last {
				t.Errorf("%s: unreported is not in the contract's order: %v", model.ModelID, listed)
			}
			inList[key], last = true, at
			if string(model.Capabilities[key]) != "false" {
				t.Errorf("%s: %s is listed as unreported but is %s", model.ModelID, key, model.Capabilities[key])
			}
		}
		for _, key := range unreportableCapabilities {
			switch string(model.Capabilities[key]) {
			case "true":
				if evidence.Evidence[model.ModelID][key] == "" {
					t.Errorf("%s: %s is true with no source in testdata/capability_evidence.json", model.ModelID, key)
				}
			case "false":
				if !inList[key] && evidence.Absent[model.ModelID][key] == "" {
					t.Errorf("%s: %s is false, not listed as unreported and has no source that it is absent", model.ModelID, key)
				}
				if inList[key] && evidence.Absent[model.ModelID][key] != "" {
					t.Errorf("%s: %s cites a source that it is absent but is still listed as unreported", model.ModelID, key)
				}
			default:
				t.Errorf("%s: %s is %s, not a boolean", model.ModelID, key, model.Capabilities[key])
			}
		}
	}
	for _, sources := range []map[string]map[string]string{evidence.Evidence, evidence.Absent} {
		for modelID := range sources {
			if !seen[modelID] {
				t.Errorf("testdata/capability_evidence.json names %s, which the catalogue does not declare", modelID)
			}
		}
	}
}

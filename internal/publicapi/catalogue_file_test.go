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
		"x-ai/grok-voice-think-fast-2.0":           "session_milliseconds",
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

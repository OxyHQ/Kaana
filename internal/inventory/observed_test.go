package inventory_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/inventory"
)

func deploymentWith(id, provider, observed string) string {
	return fmt.Sprintf(`{"deploymentId":%q,"provider":%q,"modelReference":"openai/gpt-oss-120b@observed-2026-08-01",
	  "upstreamModelId":"gpt-oss-120b","current":true,"observed":%s}`, id, provider, observed)
}

func TestParseRefusesObservedMetadataItCouldNotRepresent(t *testing.T) {
	now := time.Now()
	for name, observed := range map[string]string{
		"a zero context window":         `{"contextTokens":0}`,
		"a negative output limit":       `{"maxOutputTokens":-1}`,
		"an empty modality list":        `{"inputModalities":[]}`,
		"an unsorted modality list":     `{"inputModalities":["text","image"]}`,
		"a free-text modality":          `{"outputModalities":["Text output"]}`,
		"an effort outside the vocab":   `{"reasoningEfforts":["extreme"]}`,
		"a repeated effort":             `{"reasoningEfforts":["low","low"]}`,
		"an untrimmed name":             `{"displayName":" x "}`,
		"a control character in name":   `{"displayName":"a\u0007b"}`,
		"a creation that is not a time": `{"createdAt":"yesterday"}`,
		"a negative list price":         `{"listPrice":{"currency":"USD","input":"-1","output":"1"}}`,
		"a non-canonical list price":    `{"listPrice":{"currency":"USD","input":"1.50","output":"1"}}`,
	} {
		if _, err := inventory.Parse(issued(now, deploymentWith("d", "openrouter", observed)), time.Hour); err == nil || !strings.Contains(err.Error(), "observed") {
			t.Errorf("%s: accepted or refused for another reason: %v", name, err)
		}
	}
	// Control: every field populated and valid is accepted.
	valid := `{"displayName":"OpenAI: gpt-oss-120b","createdAt":"2025-08-05T17:17:11Z","contextTokens":131072,
	  "maxOutputTokens":32768,"inputModalities":["image","text"],"outputModalities":["text"],"supportsTools":true,
	  "reasoningEfforts":["low","medium","high"],"listPrice":{"currency":"USD","input":"0.072","output":"0.28"}}`
	if _, err := inventory.Parse(issued(now, deploymentWith("d", "openrouter", valid)), time.Hour); err != nil {
		t.Fatalf("control: a fully observed deployment was refused: %v", err)
	}
}

func TestCatalogueNarrowsCapabilitiesAcrossReportingDeploymentsOnly(t *testing.T) {
	document := issued(time.Now(), strings.Join([]string{
		deploymentWith("dep_a", "openrouter", `{"displayName":"A name","contextTokens":131072,"maxOutputTokens":32768,
		  "inputModalities":["image","text"],"outputModalities":["text"],"supportsTools":true,
		  "reasoningEfforts":["low","medium","high"],"listPrice":{"currency":"USD","input":"0.072","output":"0.28"}}`),
		deploymentWith("dep_b", "cheaperinference", `{"displayName":"B name","contextTokens":65536,
		  "inputModalities":["text"],"supportsTools":false,"reasoningEfforts":["high"],
		  "listPrice":{"currency":"USD","input":"0.05","output":"0.2"}}`),
		// Silent: abstains on every field rather than erasing the others.
		`{"deploymentId":"dep_c","provider":"groq","modelReference":"openai/gpt-oss-120b@observed-2026-08-01","upstreamModelId":"x","current":true}`,
	}, ","))
	entry := parse(t, document).Catalogue()[0]
	encoded, _ := json.Marshal(entry)

	if entry.DisplayName == nil || *entry.DisplayName != "A name" {
		t.Errorf("displayName follows deployment-id order: %s", encoded)
	}
	if *entry.ContextTokens != 65536 || *entry.MaxOutputTokens != 32768 {
		t.Errorf("limits are the smallest reported: %s", encoded)
	}
	if strings.Join(entry.InputModalities, ",") != "text" || strings.Join(entry.OutputModalities, ",") != "text" {
		t.Errorf("modalities are the reporting intersection: %s", encoded)
	}
	if entry.SupportsTools == nil || *entry.SupportsTools {
		t.Errorf("one reporting deployment without tools must make the line tool-less: %s", encoded)
	}
	if entry.ReasoningEfforts == nil || len(*entry.ReasoningEfforts) != 1 || (*entry.ReasoningEfforts)[0] != "high" {
		t.Errorf("efforts are the reporting intersection: %s", encoded)
	}
	if len(entry.ListPrices) != 2 || entry.ListPrices[0].DeploymentID != "dep_a" || entry.ListPrices[1].Output != "0.2" {
		t.Errorf("each provider's price stays its own observation: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"listPrices":[{"deploymentId":"dep_a","provider":"openrouter","currency":"USD","input":"0.072","output":"0.28"}`) {
		t.Errorf("the wire shape of a list price moved: %s", encoded)
	}
}

func TestCatalogueSaysNothingWhereNoProviderSaidAnything(t *testing.T) {
	entry := parse(t, issued(time.Now(), twoDeploymentsOfOneRevision)).Catalogue()[0]
	encoded, _ := json.Marshal(entry)
	for _, field := range []string{"displayName", "createdAt", "contextTokens", "maxOutputTokens", "inputModalities",
		"outputModalities", "supportsTools", "reasoningEfforts", "listPrices"} {
		if strings.Contains(string(encoded), `"`+field+`"`) {
			t.Errorf("%s was invented for a line no provider described: %s", field, encoded)
		}
	}
}

func TestAnExplicitNoEffortReportIsKeptDistinctFromSilence(t *testing.T) {
	entry := parse(t, issued(time.Now(), deploymentWith("d", "openrouter", `{"reasoningEfforts":[]}`))).Catalogue()[0]
	encoded, _ := json.Marshal(entry)
	if !strings.Contains(string(encoded), `"reasoningEfforts":[]`) {
		t.Errorf("a model reported to take no effort control must say [] rather than nothing: %s", encoded)
	}
}

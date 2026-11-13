package publicapi

import (
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const cacheWriteCatalogue = `{"schemaVersion":1,"models":[
 {"modelId":"anthropic/priced","publisher":{},"displayName":"x","capabilities":{},"license":{},"provenance":{},"dataPolicy":{},"deprecation":{"status":"active"},
  "pricing":{"priceVersionId":"pv-priced","currency":"USD","unitPrices":[{"unit":"cache_write_input_tokens","amount":"1.250000000000","per":1000000,"currency":"USD"},{"unit":"input_tokens","amount":"1.000000000000","per":1000000,"currency":"USD"}]}},
 {"modelId":"anthropic/plain","publisher":{},"displayName":"x","capabilities":{},"license":{},"provenance":{},"dataPolicy":{},"deprecation":{"status":"active"},
  "pricing":{"priceVersionId":"pv-plain","currency":"USD","unitPrices":[{"unit":"input_tokens","amount":"1.000000000000","per":1000000,"currency":"USD"}]}},
 {"modelId":"anthropic/unpriced","publisher":{},"displayName":"x","capabilities":{},"license":{},"provenance":{},"dataPolicy":{},"deprecation":{"status":"active"}}
]}`

func grantAdmitting(lines ...contract.AdmittedModelLine) *contract.Grant {
	return &contract.Grant{Steps: []contract.GrantStep{{Limits: contract.GrantStepLimits{AdmittedModelLines: lines}}}}
}

// The decision is the grant's: a line prices the unit for a request only when
// the price version the grant ADMITTED is the declared one and it carries the
// unit. Anything else folds, because Oxy rejects a settled unit its price list
// does not carry.
func TestTheCacheWriteIsPricedOnlyAtTheAdmittedPriceVersionThatCarriesIt(t *testing.T) {
	catalogue, err := ParsePublicCatalogue([]byte(cacheWriteCatalogue))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{catalogue: catalogue}

	cases := []struct {
		name  string
		grant *contract.Grant
		model contract.ModelReference
		want  bool
	}{
		{"admitted at the declared version that prices it", grantAdmitting(contract.AdmittedModelLine{ModelLine: "anthropic/priced", PriceVersionID: "pv-priced"}), "anthropic/priced@2026-01-01", true},
		{"a revision of the same line", grantAdmitting(contract.AdmittedModelLine{ModelLine: "anthropic/priced", PriceVersionID: "pv-priced"}), "anthropic/priced", true},
		{"admitted at an older version", grantAdmitting(contract.AdmittedModelLine{ModelLine: "anthropic/priced", PriceVersionID: "pv-old"}), "anthropic/priced@2026-01-01", false},
		{"a line whose price list lacks the unit", grantAdmitting(contract.AdmittedModelLine{ModelLine: "anthropic/plain", PriceVersionID: "pv-plain"}), "anthropic/plain@2026-01-01", false},
		{"a line with no pricing", grantAdmitting(contract.AdmittedModelLine{ModelLine: "anthropic/unpriced", PriceVersionID: "pv-x"}), "anthropic/unpriced@2026-01-01", false},
		{"a line the file does not declare", grantAdmitting(contract.AdmittedModelLine{ModelLine: "anthropic/other", PriceVersionID: "pv-priced"}), "anthropic/other@2026-01-01", false},
		{"a line the grant did not admit", grantAdmitting(contract.AdmittedModelLine{ModelLine: "anthropic/plain", PriceVersionID: "pv-plain"}), "anthropic/priced@2026-01-01", false},
		{"admitted at two versions in one grant", grantAdmitting(
			contract.AdmittedModelLine{ModelLine: "anthropic/priced", PriceVersionID: "pv-priced"},
			contract.AdmittedModelLine{ModelLine: "anthropic/priced", PriceVersionID: "pv-old"}), "anthropic/priced@2026-01-01", false},
	}
	for _, tc := range cases {
		if got := server.cacheWritePriced(tc.grant)(provider.Route{ModelReference: tc.model}); got != tc.want {
			t.Errorf("%s: priced = %v, want %v", tc.name, got, tc.want)
		}
	}

	var absent *PublicCatalogue
	if absent.PricesUnit("anthropic/priced", "pv-priced", contract.UnitCacheWriteInputTokens) {
		t.Error("a missing catalogue priced a unit")
	}
}

// The wire dialects show a cache write the way their own clients read one, and
// the prompt total a client sees is the same whether or not the grant prices
// the unit apart.
func TestUsageDialectsCarryTheCacheWrite(t *testing.T) {
	folded := []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 10}, {Unit: contract.UnitOutputTokens, Quantity: 3}}
	split := []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 4}, {Unit: contract.UnitCacheWriteInputTokens, Quantity: 6}, {Unit: contract.UnitOutputTokens, Quantity: 3}}

	if a, b := chatUsageOf(folded), chatUsageOf(split); a.PromptTokens != 10 || b.PromptTokens != 10 || a.TotalTokens != b.TotalTokens {
		t.Errorf("chat prompt tokens: folded %d, split %d, expected 10 both", a.PromptTokens, b.PromptTokens)
	}
	if got := messagesUsageOf(folded); got.InputTokens != 10 || got.CacheCreationInputTokens != nil {
		t.Errorf("folded messages usage = %+v", got)
	}
	got := messagesUsageOf(split)
	if got.InputTokens != 4 || got.CacheCreationInputTokens == nil || *got.CacheCreationInputTokens != 6 {
		t.Errorf("split messages usage = %+v, expected input 4 and cache_creation_input_tokens 6", got)
	}
}

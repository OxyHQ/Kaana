package publicapi_test

import (
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
)

// retiredPriceVersions are price versions that were published before the
// cache-write price was added to the line. A published version never changes
// (docs/operating.md), so these ids must not come back on a line that now
// prices the unit: Oxy would find the same id with different prices and price
// the line at nothing (`price_version_changed`).
var retiredPriceVersions = []string{
	"01a12f09-fdd9-7e11-ae30-c6d08ae809c3", // anthropic/claude-fable-5
	"01a12f09-fdfa-7502-b65b-e7e3bd4b0e32", // anthropic/claude-fable-5.1
	"01a12f09-fe19-77f8-9a5d-2343ffbb46aa", // anthropic/claude-haiku-5.5
	"01a0d808-57e6-7ec7-b269-eee66ed823e5", // anthropic/claude-opus-4.1
	"01a0d808-5864-799e-8876-c5cdad30719f", // anthropic/claude-sonnet-4
	"01a0d808-5878-7002-aad4-ac5b6b87f7e7", // anthropic/claude-sonnet-4.5
	// Retired 2026-11-12 when the cache-read price of these three was corrected
	// from the full input price to Anthropic's documented 0.1x.
	"01a1ce39-1a62-79cf-865a-394fe0346631", // anthropic/claude-opus-4.1
	"01a1ce39-1a62-739c-8b45-b4bedf5a6404", // anthropic/claude-sonnet-4
	"01a1ce39-1a62-7b7a-90ee-a8407a2c05a4", // anthropic/claude-sonnet-4.5
}

type cataloguePrice struct {
	Unit   string `json:"unit"`
	Amount string `json:"amount"`
	Per    int64  `json:"per"`
}

func catalogueModelPrices(t *testing.T) (map[string][]cataloguePrice, map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(catalogueFilePath)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Models []struct {
			ModelID string `json:"modelId"`
			Pricing *struct {
				PriceVersionID string           `json:"priceVersionId"`
				UnitPrices     []cataloguePrice `json:"unitPrices"`
			} `json:"pricing"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	prices := map[string][]cataloguePrice{}
	versions := map[string]string{}
	for _, model := range file.Models {
		if model.Pricing != nil {
			prices[model.ModelID] = model.Pricing.UnitPrices
			versions[model.ModelID] = model.Pricing.PriceVersionID
		}
	}
	return prices, versions
}

// The cache-write price is declared only where the provider documents it, as
// exactly 1.25x the line's own base input price, and under a price version no
// earlier price of the line used.
func TestOnlyAnthropicLinesPriceTheCacheWriteAtOneAndAQuarterTimesInput(t *testing.T) {
	prices, versions := catalogueModelPrices(t)
	seen := map[string]string{}
	for model, version := range versions {
		if other, duplicate := seen[version]; duplicate {
			t.Errorf("%s and %s share price version %s", model, other, version)
		}
		seen[version] = model
		for _, retired := range retiredPriceVersions {
			if version == retired {
				t.Errorf("%s reuses the retired price version %s with different prices", model, retired)
			}
		}
	}

	pricing := 0
	for model, list := range prices {
		var write, input *cataloguePrice
		for index := range list {
			switch list[index].Unit {
			case "cache_write_input_tokens":
				write = &list[index]
			case "input_tokens":
				input = &list[index]
			}
		}
		if write == nil {
			continue
		}
		pricing++
		if !strings.HasPrefix(model, "anthropic/") {
			t.Errorf("%s prices cache_write_input_tokens without primary evidence for its provider", model)
			continue
		}
		if input == nil || input.Per != write.Per {
			t.Errorf("%s prices a cache write per %d and its input otherwise", model, write.Per)
			continue
		}
		base, okBase := new(big.Rat).SetString(input.Amount)
		got, okWrite := new(big.Rat).SetString(write.Amount)
		if !okBase || !okWrite {
			t.Errorf("%s: amounts %q and %q are not exact decimals", model, input.Amount, write.Amount)
			continue
		}
		if want := new(big.Rat).Mul(base, big.NewRat(5, 4)); got.Cmp(want) != 0 {
			t.Errorf("%s prices a cache write at %s, and 1.25 x %s is %s", model, write.Amount, input.Amount, want.FloatString(12))
		}
	}
	if pricing != 6 {
		t.Errorf("%d lines price cache_write_input_tokens, expected the six Anthropic lines that carry a price", pricing)
	}
}

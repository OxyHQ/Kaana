package publicapi_test

import (
	"math/big"
	"strings"
	"testing"
)

// anthropicCacheReadFractions is Anthropic's documented cache-hit price as a
// fraction of the base input price
// (https://platform.claude.com/docs/en/about-claude/pricing, "Cache hits and
// refreshes", read 2026-11-12): 0.1x for every priced line here except Fable
// 5.1, which is 0.025x.
var anthropicCacheReadFractions = map[string]*big.Rat{
	"anthropic/claude-fable-5":    big.NewRat(1, 10),
	"anthropic/claude-fable-5.1":  big.NewRat(1, 40),
	"anthropic/claude-haiku-5.5":  big.NewRat(1, 10),
	"anthropic/claude-opus-4.1":   big.NewRat(1, 10),
	"anthropic/claude-sonnet-4":   big.NewRat(1, 10),
	"anthropic/claude-sonnet-4.5": big.NewRat(1, 10),
}

func unitAmount(t *testing.T, model string, list []cataloguePrice, unit string) *big.Rat {
	t.Helper()
	for _, price := range list {
		if price.Unit == unit {
			amount, ok := new(big.Rat).SetString(price.Amount)
			if !ok {
				t.Fatalf("%s: %s amount %q is not an exact decimal", model, unit, price.Amount)
			}
			return amount
		}
	}
	return nil
}

// An Anthropic line's cached_input_tokens price is the documented cache-hit
// price, not its full input price (which over-billed every cache read tenfold).
func TestAnthropicLinesPriceTheCacheReadAtTheDocumentedFractionOfInput(t *testing.T) {
	prices, _ := catalogueModelPrices(t)
	checked := 0
	for model, list := range prices {
		if !strings.HasPrefix(model, "anthropic/") {
			continue
		}
		fraction, known := anthropicCacheReadFractions[model]
		if !known {
			t.Errorf("%s is a priced Anthropic line with no documented cache-read fraction in this test", model)
			continue
		}
		input, read := unitAmount(t, model, list, "input_tokens"), unitAmount(t, model, list, "cached_input_tokens")
		if input == nil || read == nil {
			t.Errorf("%s lacks an input_tokens or cached_input_tokens price", model)
			continue
		}
		checked++
		if want := new(big.Rat).Mul(input, fraction); read.Cmp(want) != 0 {
			t.Errorf("%s prices a cache read at %s, and %s x %s is %s", model, read.FloatString(12), fraction.RatString(), input.FloatString(12), want.FloatString(12))
		}
	}
	if checked != len(anthropicCacheReadFractions) {
		t.Errorf("checked %d Anthropic lines, expected %d", checked, len(anthropicCacheReadFractions))
	}
}

// Control: the rule is Anthropic's alone. Other providers declare their own
// cache-read prices and at least one is not 0.1x its input, so the
// Anthropic-only scope is load-bearing.
func TestOtherProvidersCacheReadPricesAreNotHeldToTheAnthropicFraction(t *testing.T) {
	prices, _ := catalogueModelPrices(t)
	tenth := big.NewRat(1, 10)
	differing := 0
	for model, list := range prices {
		if strings.HasPrefix(model, "anthropic/") {
			continue
		}
		input, read := unitAmount(t, model, list, "input_tokens"), unitAmount(t, model, list, "cached_input_tokens")
		if input == nil || read == nil {
			continue
		}
		if read.Cmp(new(big.Rat).Mul(input, tenth)) != 0 {
			differing++
		}
	}
	if differing == 0 {
		t.Error("no non-Anthropic line declares a cache-read price other than 0.1x input; the control has lost its meaning")
	}
}

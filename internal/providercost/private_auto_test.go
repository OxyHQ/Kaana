package providercost

import "testing"

func TestPrivateAutoControlledQuoteExactBound(t *testing.T) {
	price, _ := ParseDecisionTokenPrice("0.042")
	limit := DecisionPriceLimit{Prompt: price}
	if !PrivateDecisionQuoteWithin(limit, 8192, "0.001000000000") {
		t.Fatal("reviewed price denied")
	}
	for _, bad := range []string{"0.000344063999", "0", "-1", "NaN"} {
		if PrivateDecisionQuoteWithin(limit, 8192, bad) {
			t.Fatal("ceiling exceeded", bad)
		}
	}
	if !PrivateDecisionQuoteWithin(limit, 8192, "0.000344064000") {
		t.Fatal("exact decimal boundary denied")
	}
	output, _ := ParseDecisionTokenPrice("0.01")
	limit.Completion = output
	if PrivateDecisionQuoteWithin(limit, 1, "0.001") {
		t.Fatal("output charge generalized")
	}
}

package nativeadmission

import (
	"errors"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func TestPartitionedUsageHasExactImmutablePrice(t *testing.T) {
	p := price("fixture/medium")
	amount, err := RateUnits(p, []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 100}, {Unit: contract.UnitCachedInputTokens, Quantity: 200}, {Unit: contract.UnitCacheWriteInputTokens, Quantity: 300}, {Unit: contract.UnitOutputTokens, Quantity: 400}, {Unit: contract.UnitReasoningTokens, Quantity: 500}, {Unit: contract.UnitRequests, Quantity: 1}})
	if err != nil || amount.Amount != "0.002295000000" {
		t.Fatalf("partitioned usage: %+v %v", amount, err)
	}
	quote, err := textQuote(p, 600, 900)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := moneyRat(amount)
	bound, _ := moneyRat(quote)
	if actual.Cmp(bound) > 0 {
		t.Fatal("actual partition exceeds quoted bound")
	}
	if quote.Amount != "0.002550000000" {
		t.Fatal(quote)
	}
}
func TestUnknownUnitsAreNeverFree(t *testing.T) {
	p := price("fixture/medium")
	for _, units := range [][]contract.UsageQuantity{{{Unit: contract.UnitImages, Quantity: 0}}, {{Unit: contract.UnitInputTokens, Quantity: -1}}, {{Unit: contract.UnitInputTokens, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 2}}} {
		if _, err := RateUnits(p, units); !errors.Is(err, ErrUnpriced) {
			t.Fatal(err)
		}
	}
	p.UnitPrices = append(p.UnitPrices, p.UnitPrices[0])
	if _, err := RateUnits(p, nil); !errors.Is(err, ErrUnpriced) {
		t.Fatal("duplicate price accepted")
	}
}
func TestNoFloatingPointAndConservativeFractionBoundary(t *testing.T) {
	p := ModelPrice{ModelLine: "fixture/tiny", PriceVersionID: "tiny1", Currency: "USD", UnitPrices: []UnitPrice{{Unit: contract.UnitRequests, Amount: "0.000000000001", Per: 3, Currency: "USD"}}}
	amount, err := RateUnits(p, []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}})
	if err != nil || amount.Amount != "0.000000000001" {
		t.Fatalf("fraction rounded down: %+v %v", amount, err)
	}
	p.UnitPrices[0].Amount = "NaN"
	if _, err = RateUnits(p, nil); err == nil {
		t.Fatal("nondecimal price accepted")
	}
}

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
	for _, units := range [][]contract.UsageQuantity{{{Unit: contract.UnitImages, Quantity: 1}}, {{Unit: contract.UnitInputTokens, Quantity: -1}}, {{Unit: contract.UnitInputTokens, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 2}}} {
		if _, err := RateUnits(p, units); !errors.Is(err, ErrUnpriced) {
			t.Fatal(err)
		}
	}
	p.UnitPrices = append(p.UnitPrices, p.UnitPrices[0])
	if _, err := RateUnits(p, nil); !errors.Is(err, ErrUnpriced) {
		t.Fatal("duplicate price accepted")
	}
}
func TestNoFloatingPointAndActualChargeFractionBoundary(t *testing.T) {
	p := ModelPrice{ModelLine: "fixture/tiny", PriceVersionID: "tiny1", Currency: "USD", UnitPrices: []UnitPrice{{Unit: contract.UnitRequests, Amount: "0.000000000001", Per: 3, Currency: "USD"}}}
	amount, err := RateUnits(p, []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}})
	if err != nil || amount.Amount != "0.000000000000" {
		t.Fatalf("actual charge did not round to nearest: %+v %v", amount, err)
	}
	p.UnitPrices[0].Amount = "NaN"
	if _, err = RateUnits(p, nil); err == nil {
		t.Fatal("nondecimal price accepted")
	}
}

func TestTextQuoteRequiresEveryPotentialPartitionPrice(t *testing.T) {
	for _, unit := range []contract.UsageUnit{contract.UnitInputTokens, contract.UnitCachedInputTokens, contract.UnitOutputTokens, contract.UnitReasoningTokens, contract.UnitRequests} {
		p := price("fixture/medium")
		kept := []UnitPrice{}
		for _, candidate := range p.UnitPrices {
			if candidate.Unit != unit {
				kept = append(kept, candidate)
			}
		}
		p.UnitPrices = kept
		if _, err := textQuote(p, 100, 100); !errors.Is(err, ErrUnpriced) {
			t.Fatalf("missing %s was admitted", unit)
		}
	}
}

// Shadow vectors for PostgreSQL numeric round(sum(amount*q/per),12).
func TestActualChargeShadowRoundingVectors(t *testing.T) {
	for _, v := range []struct {
		amount   string
		per      int64
		quantity int
		want     string
	}{
		{"0.000000000001", 3, 1, "0.000000000000"},
		{"0.000000000001", 2, 1, "0.000000000001"},
		{"0.000000000001", 3, 2, "0.000000000001"},
		{"0.000000000005", 2, 1, "0.000000000003"},
		{"1", 3, 1, "0.333333333333"},
		{"1", 3, 2, "0.666666666667"},
	} {
		p := ModelPrice{ModelLine: "fixture/tiny", PriceVersionID: "shadow", Currency: "USD", UnitPrices: []UnitPrice{{Unit: contract.UnitRequests, Amount: v.amount, Per: v.per, Currency: "USD"}}}
		units := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: v.quantity}, {Unit: contract.UnitImages, Quantity: 0}}
		got, err := RateUnits(p, units)
		if err != nil || string(got.Amount) != v.want {
			t.Fatalf("%+v: %+v %v", v, got, err)
		}
		quoted, err := quoteUnits(p, units)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := moneyRat(got)
		q, _ := moneyRat(quoted)
		if q.Cmp(a) < 0 {
			t.Fatal("quote below actual rounded charge")
		}
	}
	p := ModelPrice{ModelLine: "fixture/tiny", PriceVersionID: "aggregate", Currency: "USD", UnitPrices: []UnitPrice{
		{Unit: contract.UnitInputTokens, Amount: "0.000000000001", Per: 3, Currency: "USD"},
		{Unit: contract.UnitOutputTokens, Amount: "0.000000000001", Per: 3, Currency: "USD"},
	}}
	got, err := RateUnits(p, []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 1}, {Unit: contract.UnitOutputTokens, Quantity: 1}})
	if err != nil || got.Amount != "0.000000000001" {
		t.Fatalf("aggregate rounded per axis: %+v %v", got, err)
	}
}

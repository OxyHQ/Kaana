package providercost_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/providercost"
)

const twoCards = `{"rateCards":[
  {"deploymentId":"dep_a","currency":"XTS","rates":[
    {"unit":"requests","amountPerUnit":1000},
    {"unit":"input_tokens","amountPerUnit":2},
    {"unit":"output_tokens","amountPerUnit":10}]},
  {"deploymentId":"dep_b","currency":"XTS","rates":[
    {"unit":"requests","amountPerUnit":500},
    {"unit":"output_tokens","amountPerUnit":25}]}]}`

func parse(t *testing.T, document string) *providercost.Cards {
	t.Helper()
	cards, err := providercost.Parse([]byte(document))
	if err != nil {
		t.Fatalf("parsing rate cards: %v", err)
	}
	return cards
}

func TestPricesTheUnitsAnAttemptConsumed(t *testing.T) {
	measured := parse(t, twoCards).Measure("dep_a", []contract.UsageQuantity{
		{Unit: contract.UnitRequests, Quantity: 1},
		{Unit: contract.UnitInputTokens, Quantity: 300},
		{Unit: contract.UnitOutputTokens, Quantity: 20},
	})

	if !measured.Complete() {
		t.Fatalf("a fully priced attempt reports incomplete: %+v", measured)
	}
	const expected = 1000 + 300*2 + 20*10
	if measured.Cost.Amount != expected {
		t.Errorf("the attempt cost %d, expected %d", measured.Cost.Amount, expected)
	}
	if measured.Cost.Currency != "XTS" {
		t.Errorf("the attempt is priced in %q", measured.Cost.Currency)
	}
}

// TestAnUnknownCostIsNotAZeroCost is the shape this package exists for. Summing
// an unknown as zero produces a reconciliation that looks complete and is
// quietly short by exactly the traffic nobody priced.
func TestAnUnknownCostIsNotAZeroCost(t *testing.T) {
	cards := parse(t, twoCards)

	unpriced := cards.Measure("dep_nobody_priced", []contract.UsageQuantity{
		{Unit: contract.UnitOutputTokens, Quantity: 1000},
	})
	if unpriced.Priced {
		t.Error("a deployment with no rate card reports itself priced")
	}
	if unpriced.Complete() {
		t.Error("a deployment with no rate card reports a complete measurement")
	}
	if unpriced.Cost.Currency != "" {
		t.Errorf("an unpriced attempt carries currency %q", unpriced.Cost.Currency)
	}

	// A card that prices SOME of what was measured is the more dangerous case:
	// it produces a plausible number that is short by whatever it missed.
	partial := cards.Measure("dep_b", []contract.UsageQuantity{
		{Unit: contract.UnitOutputTokens, Quantity: 4},
		{Unit: contract.UnitReasoningTokens, Quantity: 9000},
	})
	if !partial.Priced {
		t.Fatal("a deployment with a rate card reports itself unpriced")
	}
	if partial.Complete() {
		t.Error("an attempt with an unpriced unit reports a complete measurement")
	}
	if len(partial.UnpricedUnits) != 1 || partial.UnpricedUnits[0] != contract.UnitReasoningTokens {
		t.Errorf("the measurement names %v as unpriced", partial.UnpricedUnits)
	}
	if partial.Cost.Amount != 4*25 {
		t.Errorf("the priced part came to %d, expected %d", partial.Cost.Amount, 4*25)
	}
}

// TestNoRateCardsAtAllIsASupportedState: cost measurement is optional, and a
// build without it must say "not measured" rather than "free".
func TestNoRateCardsAtAllIsASupportedState(t *testing.T) {
	var absent *providercost.Cards

	measured := absent.Measure("dep_a", []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}})
	if measured.Priced || measured.Complete() {
		t.Errorf("an unconfigured meter reported %+v", measured)
	}
	if absent.Priced("dep_a") {
		t.Error("an unconfigured meter claims to price a deployment")
	}

	record := absent.MeasureRequest("req_x", []providercost.AttemptUsage{
		{DeploymentID: "dep_a", Units: []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}},
	})
	if record.Complete {
		t.Error("an unconfigured meter produced a complete cost record")
	}
	if len(record.Totals) != 0 {
		t.Errorf("an unconfigured meter totalled %v", record.Totals)
	}
}

// TestTwoCurrenciesAreNotAddedTogether: two providers serving one model line
// can invoice in different currencies, and adding them would be a conversion
// Relay has no rate for.
func TestTwoCurrenciesAreNotAddedTogether(t *testing.T) {
	cards := parse(t, `{"rateCards":[
	  {"deploymentId":"dep_a","currency":"XTS","rates":[{"unit":"requests","amountPerUnit":100}]},
	  {"deploymentId":"dep_b","currency":"XXX","rates":[{"unit":"requests","amountPerUnit":700}]}]}`)

	record := cards.MeasureRequest("req_x", []providercost.AttemptUsage{
		{DeploymentID: "dep_a", Units: []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}},
		{DeploymentID: "dep_b", Served: true, Units: []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}},
	})

	if len(record.Totals) != 2 {
		t.Fatalf("two currencies produced %d totals: %v", len(record.Totals), record.Totals)
	}
	if record.Totals[0].Currency != "XTS" || record.Totals[0].Amount != 100 {
		t.Errorf("the first total is %v", record.Totals[0])
	}
	if record.Totals[1].Currency != "XXX" || record.Totals[1].Amount != 700 {
		t.Errorf("the second total is %v", record.Totals[1])
	}
	if !record.Complete {
		t.Error("a fully priced request across two currencies reports incomplete")
	}
}

func TestParseRefusesACardThatWouldProduceAPlausibleWrongNumber(t *testing.T) {
	cases := []struct {
		name     string
		document string
		expect   string
	}{
		{
			name:     "no cards at all",
			document: `{"rateCards":[]}`,
			expect:   "no rate cards declared",
		},
		{
			name:     "a card with no rates",
			document: `{"rateCards":[{"deploymentId":"d","currency":"XTS","rates":[]}]}`,
			expect:   "price every request at zero",
		},
		{
			name:     "a currency that is not a currency",
			document: `{"rateCards":[{"deploymentId":"d","currency":"dollars","rates":[{"unit":"requests","amountPerUnit":1}]}]}`,
			expect:   "not a currency code",
		},
		{
			name:     "a unit the contract does not declare",
			document: `{"rateCards":[{"deploymentId":"d","currency":"XTS","rates":[{"unit":"gpu_seconds","amountPerUnit":1}]}]}`,
			expect:   "not a usage unit",
		},
		{
			name:     "a negative rate",
			document: `{"rateCards":[{"deploymentId":"d","currency":"XTS","rates":[{"unit":"requests","amountPerUnit":-1}]}]}`,
			expect:   "negatively",
		},
		{
			name:     "one unit priced twice",
			document: `{"rateCards":[{"deploymentId":"d","currency":"XTS","rates":[{"unit":"requests","amountPerUnit":1},{"unit":"requests","amountPerUnit":2}]}]}`,
			expect:   "prices requests twice",
		},
		{
			name: "two cards for one deployment",
			document: `{"rateCards":[
			  {"deploymentId":"d","currency":"XTS","rates":[{"unit":"requests","amountPerUnit":1}]},
			  {"deploymentId":"d","currency":"XTS","rates":[{"unit":"requests","amountPerUnit":2}]}]}`,
			expect: "two rate cards price",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := providercost.Parse([]byte(testCase.document))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), testCase.expect) {
				t.Errorf("refused with %q, expected it to mention %q", err, testCase.expect)
			}
		})
	}
}

// TestTheContractCannotReachAnAmount is the boundary gate.
//
// `internal/contract` is the wire, and ADR 0006 puts every customer-facing
// amount in Oxy. The moment the contract package can see this one, a money
// field on a produced shape is one import away — so the import direction is
// asserted here rather than left to review.
//
// The scan carries its own positive control: it must find an import it knows is
// there, or "no import of providercost" is also what a parser that read nothing
// reports.
func TestTheContractCannotReachAnAmount(t *testing.T) {
	const contractDir = "../contract"
	entries, err := os.ReadDir(contractDir)
	if err != nil {
		t.Fatalf("reading %s: %v", contractDir, err)
	}

	scanned, imports := 0, map[string]bool{}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, filepath.Join(contractDir, entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", entry.Name(), err)
		}
		scanned++
		for _, spec := range parsed.Imports {
			imports[strings.Trim(spec.Path.Value, `"`)] = true
		}
	}

	if scanned < 5 {
		t.Fatalf("the scan read %d files of internal/contract; it is not reading the package", scanned)
	}
	if !imports["regexp"] {
		t.Fatal("the scan found no import of regexp, which internal/contract certainly has: it is not reading imports")
	}
	for path := range imports {
		if strings.Contains(path, "providercost") {
			t.Errorf("internal/contract imports %q; the wire contract must not be able to name an amount", path)
		}
		if strings.HasPrefix(path, "github.com/OxyHQ/Relay/") {
			t.Errorf("internal/contract imports %q; it imports nothing of Relay's, which is what lets the drift gate compare it against the published package with nothing in between", path)
		}
	}
}

func TestMoneyRendersForAnOperator(t *testing.T) {
	if got := (providercost.Money{Currency: "XTS", Amount: 2_500_000_000_000}).String(); got != "XTS 2.500000000000" {
		t.Errorf("an amount renders as %q", got)
	}
	if got := (providercost.Money{}).String(); got != "unmeasured" {
		t.Errorf("an unmeasured amount renders as %q", got)
	}
}

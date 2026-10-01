package providercost

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecisionReportedCost(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		amount int64
	}{
		{"0", 0}, {"0.0", 0}, {"0e5", 0}, {"0.000000000123", 123}, {"0.5", 500000000000},
		{"1.5e-7", 150000}, {"1.5E-7", 150000}, {"123e-12", 123}, {"1e-12", 1}, {"2e+0", 2000000000000},
		{"0.000000000123000", 123}, {"1.20e1", 12000000000000},
	} {
		m := (DecisionUsage{Cost: json.RawMessage(tc.raw)}).ReportedCost()
		if m == nil || m.Currency != "USD" || m.Amount != tc.amount {
			t.Errorf("%s: got %v, want %d", tc.raw, m, tc.amount)
		}
	}
	// Unknown is nil, never zero and never rounded.
	for _, raw := range []string{
		"", "null", `"0.5"`, "true", "{}", "-1", "-0", "0.0000000000001", "1e-13", "1.5e-13", "1e-20", "0.00000000000000001", "1.0000000000001", "123e-14",
		"1e30", "99999999999", "1e999999999999999999", "01", "1.", ".5", "1e", "NaN",
	} {
		if m := (DecisionUsage{Cost: json.RawMessage(raw)}).ReportedCost(); m != nil {
			t.Errorf("%q became a known amount %v", raw, m)
		}
	}
}

func TestDecisionPriceLimitExactDecimal(t *testing.T) {
	for _, value := range []string{"0", "0.042", "0.042000000000000000000000001", "1", "12.500"} {
		price, err := ParseDecisionTokenPrice(value)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(DecisionPriceLimit{Prompt: price})
		if err != nil || string(encoded) != `{"prompt":`+value+`,"completion":0}` {
			t.Fatalf("%s: %s %v", value, encoded, err)
		}
	}
	encoded, err := json.Marshal(DecisionPriceLimit{})
	if err != nil || string(encoded) != `{"prompt":0,"completion":0}` {
		t.Fatalf("zero ceiling: %s %v", encoded, err)
	}
	for _, value := range []string{"", "-0", "-0.042", "NaN", "Infinity", "+Inf", "1e999", "0.042e0", ".042", "01", "1.", " 0.042", `"0.042"`, strings.Repeat("1", 257)} {
		if _, err := ParseDecisionTokenPrice(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	if _, err := json.Marshal(DecisionTokenPrice{decimal: "NaN"}); err == nil {
		t.Fatal("invalid internal value serialized")
	}
}

func TestDecisionTokenPriceRoundTripAndInvalidReceiver(t *testing.T) {
	price, err := ParseDecisionTokenPrice("0.042000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(price)
	if err != nil {
		t.Fatal(err)
	}
	var decoded DecisionTokenPrice
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != price {
		t.Fatal("round trip changed exact decimal")
	}
	for _, invalid := range []string{`null`, `"0.042"`, `-0.042`, `1e999`, `true`, `{}`} {
		if err := json.Unmarshal([]byte(invalid), &decoded); err == nil {
			t.Errorf("decoded invalid %s", invalid)
		}
		if decoded != price {
			t.Fatal("invalid input changed prior ceiling")
		}
	}
	for _, value := range []string{"0", "0.000"} {
		zero, err := ParseDecisionTokenPrice(value)
		if err != nil || !zero.IsZero() {
			t.Fatal("exact zero not recognized")
		}
	}
	if (DecisionTokenPrice{}).IsZero() != true || price.IsZero() {
		t.Fatal("zero ceiling predicate incorrect")
	}
}

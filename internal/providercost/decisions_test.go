package providercost

import (
	"encoding/json"
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

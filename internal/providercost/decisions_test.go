package providercost

import (
	"encoding/json"
	"testing"
)

func TestDecisionReportedCost(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		amount int64
		valid  bool
	}{
		{"0", 0, true}, {"0.000000000123", 123, true}, {"0.5", 500000000000, true},
		{"-1", 0, false}, {"null", 0, false}, {`"0.5"`, 0, false}, {"0.0000000000001", 0, false},
	} {
		u := DecisionUsage{Cost: json.RawMessage(tc.raw)}
		m, err := u.ReportedCost()
		if tc.valid {
			if err != nil || m == nil || m.Amount != tc.amount {
				t.Fatalf("%s: %v %v", tc.raw, m, err)
			}
		} else if err == nil {
			t.Fatalf("invalid amount accepted: %s", tc.raw)
		}
	}
	m, err := (DecisionUsage{}).ReportedCost()
	if err != nil || m != nil {
		t.Fatal("unknown cost became zero")
	}
}

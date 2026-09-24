package providercost

import "testing"

func TestListPriceFromPerTokenIsAnExactDecimalShift(t *testing.T) {
	for _, testCase := range []struct {
		input, output         string
		wantInput, wantOutput string
	}{
		{"0.000003", "0.000015", "3", "15"},
		{"0.0000000375", "0.00000015", "0.0375", "0.15"},
		{"0", "0", "0", "0"},
		{"0.00000125000", "0.00001", "1.25", "10"},
		{"2", "0.000000000000000001", "2000000", "0.000000000001"},
	} {
		price, err := ListPriceFromPerToken("USD", testCase.input, testCase.output)
		if err != nil {
			t.Fatalf("%s/%s: %v", testCase.input, testCase.output, err)
		}
		if price.Currency != "USD" || price.Input != testCase.wantInput || price.Output != testCase.wantOutput {
			t.Errorf("%s/%s became %+v, want %s/%s", testCase.input, testCase.output, price, testCase.wantInput, testCase.wantOutput)
		}
	}
}

func TestListPriceRefusesWhatNobodyQuoted(t *testing.T) {
	for _, bad := range [][3]string{
		{"USD", "-1", "-1"},                   // a router's "price depends on the route"
		{"USD", "0.000003", ""},               // half a price is not a price
		{"USD", "1e-6", "0.000001"},           // exponent notation never enters as a number
		{"USD", "0.0000000000000000001", "0"}, // beyond the exact scale: refused, not rounded
		{"usd", "0.000001", "0.000001"},       // not a currency code
		{"USD", " 0.000001", "0.000001"},      // whitespace-normalized
		{"USD", "0.", "0"},
	} {
		if price, err := ListPriceFromPerToken(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("%q was accepted as %+v", bad, price)
		}
	}
	if err := (ListPrice{Currency: "USD", Input: "3.50", Output: "15"}).Validate(); err == nil {
		t.Error("a non-canonical spelling was accepted, so one amount could have two snapshot byte forms")
	}
	if err := (ListPrice{Currency: "USD", Input: "3.5", Output: "15"}).Validate(); err != nil {
		t.Errorf("control: a canonical list price was refused: %v", err)
	}
}

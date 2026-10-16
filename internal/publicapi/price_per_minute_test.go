package publicapi

import (
	"math/big"
	"testing"
)

// A minute's price is rounded up to the contract's twelve decimals, so an
// estimate reserved with it never falls short of what the minute costs.
func TestCeilDecimalNeverRoundsDown(t *testing.T) {
	for _, c := range []struct {
		value *big.Rat
		want  string
	}{
		{big.NewRat(2, 3000), "0.000666666667"},
		{big.NewRat(8, 100), "0.08"},
		{big.NewRat(1, 1), "1.0"},
		{big.NewRat(0, 1), "0.0"},
		{big.NewRat(1, 1_000_000_000_000), "0.000000000001"},
		{big.NewRat(1, 10_000_000_000_000), "0.000000000001"},
	} {
		if got := ceilDecimal(c.value, 12); got != c.want {
			t.Errorf("ceilDecimal(%s) = %s, want %s", c.value.RatString(), got, c.want)
		}
	}
}

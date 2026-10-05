package providercost

import "math/big"

// PrivateDecisionQuoteWithin bounds controlled input by its UTF-8 byte ceiling
// (one token per byte conservatively), at the exact observed per-million rate.
// This is a pre-send quote bound, never an assertion about a provider invoice.
func PrivateDecisionQuoteWithin(limit DecisionPriceLimit, controlledBytes int, ceiling string) bool {
	if controlledBytes < 0 || limit.Prompt.IsZero() || !limit.Completion.IsZero() {
		return false
	}
	price, ok := new(big.Rat).SetString(limit.Prompt.decimal)
	if !ok {
		return false
	}
	bound, ok := new(big.Rat).SetString(ceiling)
	if !ok || bound.Sign() <= 0 {
		return false
	}
	quote := new(big.Rat).Mul(price, big.NewRat(int64(controlledBytes), 1_000_000))
	return quote.Cmp(bound) <= 0
}

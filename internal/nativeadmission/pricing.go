package nativeadmission

import (
	"fmt"
	"math/big"
	"regexp"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// Prices are Kaana's declared customer tariff, never a provider cost estimate.
type UnitPrice struct {
	Unit     contract.UsageUnit    `json:"unit"`
	Amount   string                `json:"amount"`
	Per      int64                 `json:"per"`
	Currency contract.CurrencyCode `json:"currency"`
}
type ModelPrice struct {
	ModelLine      contract.ModelID      `json:"modelLine"`
	PriceVersionID string                `json:"priceVersionId"`
	Currency       contract.CurrencyCode `json:"currency"`
	UnitPrices     []UnitPrice           `json:"unitPrices"`
}

var decimalPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,29})(\.[0-9]{1,12})?$`)

func decimalRat(value string) (*big.Rat, error) {
	if !decimalPattern.MatchString(value) {
		return nil, fmt.Errorf("%w: invalid decimal", ErrUnpriced)
	}
	n, ok := new(big.Rat).SetString(value)
	if !ok || n.Sign() < 0 {
		return nil, ErrUnpriced
	}
	return n, nil
}
func moneyRat(m contract.Money) (*big.Rat, error) {
	if m.Currency != "USD" {
		return nil, fmt.Errorf("%w: unsupported currency", ErrUnpriced)
	}
	return decimalRat(string(m.Amount))
}
func unitRates(p ModelPrice) (map[contract.UsageUnit]*big.Rat, error) {
	if !p.ModelLine.Valid() || p.PriceVersionID == "" || p.Currency != "USD" || len(p.UnitPrices) == 0 {
		return nil, ErrUnpriced
	}
	rates := make(map[contract.UsageUnit]*big.Rat, len(p.UnitPrices))
	for _, u := range p.UnitPrices {
		if !u.Unit.Valid() || u.Per <= 0 || u.Currency != p.Currency {
			return nil, ErrUnpriced
		}
		if _, exists := rates[u.Unit]; exists {
			return nil, fmt.Errorf("%w: duplicate price unit", ErrUnpriced)
		}
		amount, err := decimalRat(u.Amount)
		if err != nil {
			return nil, err
		}
		rates[u.Unit] = amount.Quo(amount, new(big.Rat).SetInt64(u.Per))
	}
	return rates, nil
}

// Round upward once at the money boundary, to 12 decimal places. Never use a
// binary float; quotes must bound every valid partition of the measured units.
func roundedMoney(r *big.Rat) contract.Money {
	scale := big.NewInt(1_000_000_000_000)
	scaled := new(big.Int).Mul(r.Num(), scale)
	q, remainder := new(big.Int).QuoRem(scaled, r.Denom(), new(big.Int))
	if remainder.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	amount := new(big.Rat).SetFrac(q, scale).FloatString(12)
	return contract.Money{Amount: contract.ExactDecimal(amount), Currency: "USD"}
}

// RateUnits refuses unknown/duplicate units, even a zero quantity. Missing
// prices never mean free. A price snapshot cannot be replaced at settlement.
func RateUnits(price ModelPrice, units []contract.UsageQuantity) (contract.Money, error) {
	rates, err := unitRates(price)
	if err != nil {
		return contract.Money{}, err
	}
	total := new(big.Rat)
	seen := make(map[contract.UsageUnit]bool)
	for _, u := range units {
		rate, ok := rates[u.Unit]
		if !ok || seen[u.Unit] || u.Quantity < 0 {
			return contract.Money{}, ErrUnpriced
		}
		seen[u.Unit] = true
		total.Add(total, new(big.Rat).Mul(rate, new(big.Rat).SetInt64(int64(u.Quantity))))
	}
	return roundedMoney(total), nil
}

func textQuote(price ModelPrice, input, output int) (contract.Money, error) {
	rates, err := unitRates(price)
	if err != nil {
		return contract.Money{}, err
	}
	in, ok := rates[contract.UnitInputTokens]
	if !ok {
		return contract.Money{}, ErrUnpriced
	}
	out, ok := rates[contract.UnitOutputTokens]
	if !ok {
		return contract.Money{}, ErrUnpriced
	}
	requests, ok := rates[contract.UnitRequests]
	if !ok {
		return contract.Money{}, ErrUnpriced
	}
	// Input/output units are partitions, not overlapping totals. Cache writes
	// can cost more than uncached input; reasoning can cost more than visible text.
	for _, unit := range []contract.UsageUnit{contract.UnitCachedInputTokens, contract.UnitCacheWriteInputTokens} {
		if rate, ok := rates[unit]; ok && rate.Cmp(in) > 0 {
			in = rate
		}
	}
	if rate, ok := rates[contract.UnitReasoningTokens]; ok && rate.Cmp(out) > 0 {
		out = rate
	}
	total := new(big.Rat).Set(requests)
	total.Add(total, new(big.Rat).Mul(in, new(big.Rat).SetInt64(int64(input))))
	total.Add(total, new(big.Rat).Mul(out, new(big.Rat).SetInt64(int64(output))))
	return roundedMoney(total), nil
}

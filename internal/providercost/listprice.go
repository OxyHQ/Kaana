package providercost

import (
	"fmt"
	"strings"
)

// ListPrice is what a provider's own public model catalogue says one million
// tokens cost, at the moment the inventory publisher read it.
//
// It is an OBSERVATION of somebody else's published catalogue, not a cost and
// not a price:
//
//   - it is not what Kaana pays for a request — that is a Measurement, derived
//     from a versioned rate card or the provider's exact per-request receipt;
//   - it is not what a customer is charged — that is Oxy's, always.
//
// It lives in this package because this is the only package allowed to hold an
// amount of money, so the decimal rules below are the same ones a rate card
// obeys. It crosses exactly one boundary: the signed operator catalogue
// `GET /internal/v1/models`, whose only caller is Oxy, which uses it as input to
// its own pricing. It never enters an inference stream event, a usage report,
// an error body or any contract shape.
//
// Amounts are exact decimal strings in the currency's major unit per million
// tokens. They never pass through a floating-point number, and one that could
// not be represented at this package's Scale is refused rather than rounded.
type ListPrice struct {
	Currency string `json:"currency"`
	// Input is the published price of one million input (prompt) tokens.
	Input string `json:"input"`
	// Output is the published price of one million output (completion) tokens.
	Output string `json:"output"`
}

// tokensPerQuotedUnitExponent is the unit a ListPrice quotes, 10^6 tokens: the
// unit every provider catalogue Kaana reads quotes in its own UI.
const tokensPerQuotedUnitExponent = 6

// ListPriceFromPerToken converts a catalogue that publishes a per-TOKEN decimal
// price (OpenRouter's `pricing.prompt` / `pricing.completion`) into a per-
// million-token ListPrice by an exact decimal shift.
//
// A negative value is how some catalogues say "not a fixed price" (a router
// whose price depends on where it routes). It is refused, so the caller leaves
// the observation absent instead of publishing a number nobody quoted.
func ListPriceFromPerToken(currency, inputPerToken, outputPerToken string) (ListPrice, error) {
	input, err := shiftDecimal(inputPerToken, tokensPerQuotedUnitExponent)
	if err != nil {
		return ListPrice{}, fmt.Errorf("providercost: input list price: %w", err)
	}
	output, err := shiftDecimal(outputPerToken, tokensPerQuotedUnitExponent)
	if err != nil {
		return ListPrice{}, fmt.Errorf("providercost: output list price: %w", err)
	}
	price := ListPrice{Currency: currency, Input: input, Output: output}
	if err := price.Validate(); err != nil {
		return ListPrice{}, err
	}
	return price, nil
}

// Validate refuses a list price this package could not hold exactly.
func (p ListPrice) Validate() error {
	if _, err := ParseDecimal(p.Currency, p.Input); err != nil {
		return fmt.Errorf("providercost: list price input: %w", err)
	}
	if _, err := ParseDecimal(p.Currency, p.Output); err != nil {
		return fmt.Errorf("providercost: list price output: %w", err)
	}
	if canonicalDecimal(p.Input) != p.Input || canonicalDecimal(p.Output) != p.Output {
		return fmt.Errorf("providercost: list price amounts must be canonical decimals (no leading or trailing zeros)")
	}
	return nil
}

// shiftDecimal multiplies a non-negative decimal string by 10^exponent without
// floating point, returning the canonical decimal.
func shiftDecimal(value string, exponent int) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
		return "", fmt.Errorf("%q is not a non-negative decimal amount", value)
	}
	whole, fraction, _ := strings.Cut(value, ".")
	if whole == "" || strings.Contains(fraction, ".") || (strings.Contains(value, ".") && fraction == "") {
		return "", fmt.Errorf("%q is not a decimal amount", value)
	}
	for _, digit := range whole + fraction {
		if digit < '0' || digit > '9' {
			return "", fmt.Errorf("%q is not a decimal amount", value)
		}
	}
	for len(fraction) < exponent {
		fraction += "0"
	}
	return canonicalDecimal(whole + fraction[:exponent] + "." + fraction[exponent:]), nil
}

// canonicalDecimal strips leading zeros of the whole part and trailing zeros of
// the fraction, so one amount has one spelling and a snapshot's bytes do not
// move because a provider reformatted the same number.
func canonicalDecimal(value string) string {
	whole, fraction, _ := strings.Cut(value, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction == "" {
		return whole
	}
	return whole + "." + fraction
}

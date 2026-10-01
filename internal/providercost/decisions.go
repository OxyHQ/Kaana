package providercost

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// DecisionUsage owns the provider's optional usage.cost alongside its counters.
// Decode cost separately so a malformed amount never erases measured work.
type DecisionUsage struct {
	InputTokens  *int            `json:"input_tokens"`
	OutputTokens *int            `json:"output_tokens"`
	Cost         json.RawMessage `json:"cost"`
}

// jsonNumber is the JSON number grammar, captured as sign, whole, fraction and
// exponent so the amount is scaled exactly rather than through a float.
var jsonNumber = regexp.MustCompile(`^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

// ReportedCost returns the exact amount the provider billed, or nil when that
// amount is unknown. Unknown covers an absent or null cost, a non-number, a
// negative amount, and a value that cannot be held exactly at Scale: such a
// value is neither rounded nor replaced with zero. An unknown amount never
// fails the answer it accompanies; the provider has already been paid for it.
func (u DecisionUsage) ReportedCost() *Money {
	match := jsonNumber.FindStringSubmatch(string(u.Cost))
	if match == nil || match[1] == "-" {
		return nil
	}
	digits := strings.TrimLeft(match[2]+match[3], "0")
	if digits == "" {
		return &Money{Currency: "USD"}
	}
	exponent := 0
	if match[4] != "" {
		parsed, err := strconv.Atoi(match[4])
		if err != nil {
			return nil
		}
		exponent = parsed
	}
	// value = digits × 10^shift, in units of 10^-Scale.
	shift := exponent - len(match[3]) + Scale
	if shift < 0 {
		if -shift > len(digits) || strings.Trim(digits[len(digits)+shift:], "0") != "" {
			return nil
		}
		digits = digits[:len(digits)+shift]
	} else {
		if len(digits)+shift > 19 {
			return nil
		}
		digits += strings.Repeat("0", shift)
	}
	if digits == "" {
		return nil
	}
	amount, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return nil
	}
	return &Money{Currency: "USD", Amount: amount}
}

// DecisionPriceLimit defaults to zero spend. No reviewed price authorization
// exists for these dormant routes; enabling them requires a separately reviewed
// immutable ceiling, held here rather than in an inference contract.
type DecisionPriceLimit struct {
	Prompt     DecisionTokenPrice `json:"prompt"`
	Completion DecisionTokenPrice `json:"completion"`
}

// DecisionTokenPrice is an exact USD-per-million-token ceiling. Its zero value
// is zero spend. Values are constructed from plain decimal strings, never floats.
// Scientific notation is deliberately refused to keep the ceiling inspectable.
type DecisionTokenPrice struct{ decimal string }

var decisionDecimal = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// ParseDecisionTokenPrice refuses negative, non-finite and malformed prices.
// The original decimal is retained exactly; it is neither rounded nor scaled.
func ParseDecisionTokenPrice(value string) (DecisionTokenPrice, error) {
	if len(value) > 256 || !decisionDecimal.MatchString(value) {
		return DecisionTokenPrice{}, errors.New("decision token price: expected a nonnegative plain decimal of at most 256 bytes")
	}
	return DecisionTokenPrice{decimal: value}, nil
}

// MarshalJSON emits a JSON number, not a string. No output is possible from an
// invalid internal value; the uninitialized value preserves zero-spend policy.
func (p DecisionTokenPrice) MarshalJSON() ([]byte, error) {
	if p.decimal == "" {
		return []byte("0"), nil
	}
	if _, err := ParseDecisionTokenPrice(p.decimal); err != nil {
		return nil, err
	}
	return []byte(p.decimal), nil
}

// UnmarshalJSON accepts only an exact nonnegative numeric decimal.
func (p *DecisionTokenPrice) UnmarshalJSON(data []byte) error {
	parsed, err := ParseDecisionTokenPrice(string(data))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// IsZero reports whether the exact ceiling allows no spend.
func (p DecisionTokenPrice) IsZero() bool {
	return strings.Trim(p.decimal, "0.") == ""
}

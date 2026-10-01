package providercost

import (
	"encoding/json"
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
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
}

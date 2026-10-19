package providercost

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
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

// DecisionPriceLimit is a decisions deployment's per-million-token price
// ceiling, derived only from its reviewed rate card (Cards.DecisionPriceLimit)
// and held here rather than in an inference contract. Its zero value is zero
// spend; a route without a card is refused, never sent with it.
type DecisionPriceLimit struct {
	Prompt     DecisionTokenPrice `json:"prompt"`
	Completion DecisionTokenPrice `json:"completion"`
}

// DecisionPriceLimit is the price ceiling of one decisions deployment, derived
// from its current loaded USD card and from nothing else: not an Oxy customer
// price, not a provider default. Every decisions route needs one (decisions
// bill input tokens only, so the card is a positive input rate and a zero
// output rate); a deployment without such a card answers false and is
// refused, never priced at zero. OpenRouter's systemone call sends it as
// `max_price`; every adapter refuses a decisions call without it.
func (c *Cards) DecisionPriceLimit(id contract.DeploymentID, at time.Time) (DecisionPriceLimit, bool) {
	if c == nil {
		return DecisionPriceLimit{}, false
	}
	observation, ok := c.ObservationForDeployment(id)
	if !ok || at.Before(observation.EffectiveAt) || (observation.ExpiresAt != nil && !at.Before(*observation.ExpiresAt)) {
		return DecisionPriceLimit{}, false
	}
	card, ok := c.byDeployment[id]
	if !ok || card.Currency != "USD" || len(card.Rates) != 2 {
		return DecisionPriceLimit{}, false
	}
	var limit DecisionPriceLimit
	input, output := false, false
	for _, rate := range card.Rates {
		if rate.AmountPerUnit < 0 {
			return DecisionPriceLimit{}, false
		}
		price, err := ParseDecisionTokenPrice(fmt.Sprintf("%d.%06d", rate.AmountPerUnit/1_000_000, rate.AmountPerUnit%1_000_000))
		if err != nil {
			return DecisionPriceLimit{}, false
		}
		switch rate.Unit {
		case contract.UnitInputTokens:
			if input {
				return DecisionPriceLimit{}, false
			}
			input = true
			limit.Prompt = price
		case contract.UnitOutputTokens:
			if output {
				return DecisionPriceLimit{}, false
			}
			output = true
			limit.Completion = price
		default:
			return DecisionPriceLimit{}, false
		}
	}
	return limit, input && output && !limit.Prompt.IsZero() && limit.Completion.IsZero()
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

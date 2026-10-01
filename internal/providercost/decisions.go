package providercost

import (
	"encoding/json"
	"fmt"
)

// DecisionUsage owns the provider's optional usage.cost alongside its counters.
// Decode cost separately so a malformed amount never erases measured work.
type DecisionUsage struct {
	InputTokens  *int            `json:"input_tokens"`
	OutputTokens *int            `json:"output_tokens"`
	Cost         json.RawMessage `json:"cost"`
}

func (u DecisionUsage) ReportedCost() (*Money, error) {
	if len(u.Cost) == 0 {
		return nil, nil
	}
	amount, err := ParseDecimal("USD", string(u.Cost))
	if err != nil {
		return nil, fmt.Errorf("providercost: invalid decisions billed cost")
	}
	return &amount, nil
}

// DecisionPriceLimit defaults to zero spend. No reviewed price authorization
// exists for these dormant routes; enabling them requires a separately reviewed
// immutable ceiling, held here rather than in an inference contract.
type DecisionPriceLimit struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
}

package providercost

import (
	"fmt"
	"github.com/OxyHQ/Kaana/internal/contract"
	"time"
)

// MatchesPublishedTokenPrice binds one actual loaded card to the exact observed
// per-million prices. No floats, multiplication overflow or invented zero rate.
func (c *Cards) MatchesPublishedTokenPrice(id contract.DeploymentID, price ListPrice, at time.Time) bool {
	if c == nil || price.Validate() != nil {
		return false
	}
	observation, ok := c.ObservationForDeployment(id)
	if !ok || at.Before(observation.EffectiveAt) || (observation.ExpiresAt != nil && !at.Before(*observation.ExpiresAt)) {
		return false
	}
	card, ok := c.byDeployment[id]
	if !ok || card.Currency != price.Currency || len(card.Rates) != 2 {
		return false
	}
	input, err := ParseDecimal(price.Currency, price.Input)
	if err != nil {
		return false
	}
	output, err := ParseDecimal(price.Currency, price.Output)
	if err != nil {
		return false
	}
	if input.Amount%1_000_000 != 0 || output.Amount%1_000_000 != 0 {
		return false
	}
	seenInput, seenOutput := false, false
	for _, rate := range card.Rates {
		switch rate.Unit {
		case contract.UnitInputTokens:
			seenInput = rate.AmountPerUnit == input.Amount/1_000_000
		case contract.UnitOutputTokens:
			seenOutput = rate.AmountPerUnit == output.Amount/1_000_000
		default:
			return false
		}
	}
	return seenInput && seenOutput
}

// ScopedDecisionPriceLimit derives the wire ceiling from the actual current
// loaded USD card, not from an Oxy customer price or provider defaults.
func (c *Cards) ScopedDecisionPriceLimit(id contract.DeploymentID, at time.Time) (DecisionPriceLimit, bool) {
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

// ScopedDecisionPublishedPrice derives the canonical per-million quote from
// the actual valid immutable card. It is not a guessed/customer price, and a
// private candidate still has to match authenticated upstream discovery.
func (c *Cards) ScopedDecisionPublishedPrice(id contract.DeploymentID, at time.Time) (ListPrice, bool) {
	limit, ok := c.ScopedDecisionPriceLimit(id, at)
	if !ok {
		return ListPrice{}, false
	}
	price := ListPrice{Currency: "USD", Input: canonicalDecimal(limit.Prompt.decimal), Output: canonicalDecimal(limit.Completion.decimal)}
	return price, price.Validate() == nil
}

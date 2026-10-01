package providercost

import (
	"github.com/OxyHQ/Kaana/internal/contract"
	"time"
)

// MatchesPublishedTokenPrice binds one actual loaded card to the exact observed
// per-million prices. No floats, multiplication overflow or invented zero rate.
func (c *Cards) MatchesPublishedTokenPrice(id contract.DeploymentID, price ListPrice, at time.Time) bool {
	if c == nil || price.Validate() != nil {
		return false
	}
	observation, ok := c.Observation()
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

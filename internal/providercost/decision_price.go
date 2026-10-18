package providercost

import (
	"fmt"
	"github.com/OxyHQ/Kaana/internal/contract"
	"time"
)

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

package publicapi

import (
	"encoding/json"
	"maps"
	"math/big"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/provider"
)

type nativeLinePrices []nativeadmission.ModelPrice

func (prices nativeLinePrices) LinePrice(line contract.ModelID, version string) linePrice {
	for _, price := range prices {
		if price.ModelLine != line || price.PriceVersionID != version || price.Currency != "USD" {
			continue
		}
		perMillion := func(unit contract.UsageUnit) *big.Rat {
			for _, u := range price.UnitPrices {
				if u.Unit == unit && u.Per > 0 {
					amount, ok := new(big.Rat).SetString(u.Amount)
					if ok {
						return amount.Mul(amount, big.NewRat(1000000, u.Per))
					}
				}
			}
			return nil
		}
		return linePrice{Input: perMillion(contract.UnitInputTokens), Output: perMillion(contract.UnitOutputTokens)}
	}
	return linePrice{}
}
func (s *Server) cacheWritePricedAdmission(a *admission) kaana.CacheWritePriced {
	if a.native == nil {
		return s.cacheWritePricedSteps(a.steps())
	}
	return func(route provider.Route) bool {
		price, err := a.native.Plan.PriceForReport(contract.UsageReport{ResolvedModelReference: route.ModelReference, ServingProvider: route.Provider, DeploymentID: route.DeploymentID})
		return err == nil && slices.ContainsFunc(price.UnitPrices, func(u nativeadmission.UnitPrice) bool { return u.Unit == contract.UnitCacheWriteInputTokens })
	}
}

// A line with provider-dependent tariffs has no honest single aggregate price.
// Only the authenticated population's rendered copy changes; global declarations
// and their immutable bytes remain untouched.
func nativePricedDeclaration(model declaredModel, line contract.ModelID, book []nativeadmission.RoutePrice) declaredModel {
	var entries []nativeadmission.RoutePrice
	for _, entry := range book {
		if entry.Price.ModelLine == line {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return model
	}
	model.fields = maps.Clone(model.fields)
	delete(model.fields, "pricing")
	if len(entries) == 1 {
		p := entries[0].Price
		model.fields["pricing"], _ = json.Marshal(contract.PriceSnapshot{PriceVersionID: p.PriceVersionID, Currency: p.Currency, UnitPrices: toContractPrices(p.UnitPrices)})
	}
	return model
}
func toContractPrices(prices []nativeadmission.UnitPrice) []contract.UnitPrice {
	result := make([]contract.UnitPrice, 0, len(prices))
	for _, p := range prices {
		result = append(result, contract.UnitPrice{Unit: p.Unit, Amount: contract.ExactDecimal(p.Amount), Per: p.Per, Currency: p.Currency})
	}
	return result
}

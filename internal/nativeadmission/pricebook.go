package nativeadmission

import (
	"errors"
	"slices"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// RoutePrice is an immutable customer tariff for one reviewed deployment tuple.
// It never changes the public catalogue or another population's tariff.
type RoutePrice struct {
	Route                RoutePermission `json:"route"`
	Price                ModelPrice      `json:"price"`
	SourceArtifactSHA256 string          `json:"sourceArtifactSha256"`
	SourcePriceVersionID string          `json:"sourcePriceVersionId"`
	SourceEvidenceRef    string          `json:"sourceEvidenceRef"`
}

func (p Policy) validatePriceBook() error {
	seen := map[RoutePermission]bool{}
	versions := map[string]ModelPrice{}
	for _, entry := range p.PriceBook {
		if entry.Route.DeploymentID == "" || !entry.Route.ModelReference.Valid() || !strings.Contains(string(entry.Route.ModelReference), "@") || !entry.Route.Provider.Valid() || entry.Route.ModelReference.ModelID() != entry.Price.ModelLine || seen[entry.Route] || len(entry.SourceArtifactSHA256) != 64 || entry.SourcePriceVersionID == "" || entry.SourceEvidenceRef == "" {
			return errors.New("nativeadmission: invalid scoped price evidence")
		}
		for _, c := range entry.SourceArtifactSHA256 {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return ErrUnpriced
			}
		}
		if _, err := unitRates(entry.Price); err != nil {
			return err
		}
		if old, ok := versions[entry.Price.PriceVersionID]; ok && !samePrice(old, entry.Price) {
			return ErrUnpriced
		}
		seen[entry.Route] = true
		versions[entry.Price.PriceVersionID] = entry.Price
	}
	return nil
}

type pricedModel struct {
	model      Model
	routePrice *RoutePrice
}

func (p Policy) pricedModels(models []Model, scope RouteScope) []pricedModel {
	var result []pricedModel
	for _, model := range models {
		scoped := false
		for _, entry := range p.PriceBook {
			if entry.Price.ModelLine != model.ID {
				continue
			}
			scoped = true
			if !scope.Allows(entry.Route.DeploymentID, entry.Route.ModelReference, entry.Route.Provider, true) {
				continue
			}
			candidate := model
			candidate.Price = clone(entry.Price)
			result = append(result, pricedModel{candidate, ptrRoutePrice(entry)})
		}
		if !scoped {
			result = append(result, pricedModel{model, nil})
		}
	}
	return result
}
func ptrRoutePrice(p RoutePrice) *RoutePrice { copy := clone(p); return &copy }

// PriceForReport requires the actual served tuple when a scoped tariff exists.
// Missing identity never chooses a cheaper provider or falls back to a line price.
func (p Plan) PriceForReport(r contract.UsageReport) (ModelPrice, error) {
	scoped := false
	for _, entry := range p.RoutePrices {
		if entry.Price.ModelLine != r.ResolvedModelReference.ModelID() {
			continue
		}
		scoped = true
		if entry.Route.DeploymentID == r.DeploymentID && entry.Route.Provider == r.ServingProvider && entry.Route.ModelReference == r.ResolvedModelReference {
			return clone(entry.Price), nil
		}
	}
	if scoped {
		return ModelPrice{}, ErrUnpriced
	}
	var found *ModelPrice
	for _, price := range p.Prices {
		if price.ModelLine != r.ResolvedModelReference.ModelID() {
			continue
		}
		if found != nil && found.PriceVersionID != price.PriceVersionID {
			return ModelPrice{}, ErrUnpriced
		}
		copy := price
		found = &copy
	}
	if found == nil {
		return ModelPrice{}, ErrUnpriced
	}
	return clone(*found), nil
}

func (p Policy) admitsPriceBook(plan Plan) bool {
	if len(plan.StepRoutes) != len(plan.Steps) || (len(plan.StepGroups) != 0 && len(plan.StepGroups) != len(plan.Steps)) {
		return false
	}
	for i, group := range plan.StepGroups {
		if group < 0 || i > 0 && group < plan.StepGroups[i-1] {
			return false
		}
	}

	for _, entry := range plan.RoutePrices {
		if !slices.ContainsFunc(p.PriceBook, func(approved RoutePrice) bool { return sameJSON(approved, entry) }) {
			return false
		}
	}
	for index, step := range plan.Steps {
		for _, line := range step.Limits.AdmittedModelLines {
			scoped := slices.ContainsFunc(p.PriceBook, func(entry RoutePrice) bool { return entry.Price.ModelLine == line.ModelLine })
			if scoped && !slices.ContainsFunc(plan.RoutePrices, func(entry RoutePrice) bool {
				return entry.Price.ModelLine == line.ModelLine && entry.Price.PriceVersionID == line.PriceVersionID && plan.StepRoutes[index].Mode == "exact" && len(plan.StepRoutes[index].Deployments) == 1 && plan.StepRoutes[index].Deployments[0] == entry.Route && slices.ContainsFunc(plan.Prices, func(price ModelPrice) bool { return sameJSON(price, entry.Price) })
			}) {
				return false
			}
		}
	}
	return true
}

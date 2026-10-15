package publicapi

import (
	"slices"
	"sort"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// plan is a grant resolved into the executor's terms: the ordered routes Kaana
// may attempt, and which step of the grant each one belongs to.
type plan struct {
	target contract.RoutingTarget
	routes []contract.AuthorizedRoute
	steps  map[contract.DeploymentID]int
}

// stepOf is the position, in the grant, of the step a deployment served.
func (p *plan) stepOf(deployment contract.DeploymentID) *int {
	if p == nil {
		return nil
	}
	step, known := p.steps[deployment]
	if !known {
		return nil
	}
	return &step
}

// resolveGrant turns the grant's ordered steps into routes (Kaana ADR 0002,
// decisions 4 and 5).
//
// For each step, in order: the model lines the step admits (only the target
// line for a model step, at its pinned revision if the caller pinned one; every
// admitted line for a level step), at the revision the inventory serves now;
// every deployment serving them that the step's provider and region limits
// admit and whose key pool holds a key within the step's funding ceiling;
// ordered by the cheapest such key (free, credit, paid, unstated), then the
// grant's line order, then the inventory's. A deployment a previous step
// already authorized is not repeated: a request moves to the next step only
// when the previous one has no admissible route left, which the executor
// decides attempt by attempt and only before any output.
//
// A step with `sameModelFailover: false` keeps only the best-ranked deployment
// of each admitted line (one provider; its key pool still walks free → credit →
// paid inside that deployment, ADR 0001), so the request never moves to another
// provider of the same line within the step.
//
// Kaana never adds a model line Oxy did not admit and never widens a limit.
// What it cannot verify from its inventory (zero data retention, training,
// licences) is the policy Oxy applied when it computed admittedModelLines.
func resolveGrant(grant *contract.Grant, snapshot *inventory.Inventory, funding FundingRanker, at time.Time) (*plan, *contract.PublicError) {
	resolved := &plan{steps: make(map[contract.DeploymentID]int)}
	revisionOf := make(map[contract.ModelID]contract.ModelReference)
	for stepIndex, step := range grant.Steps {
		type candidate struct {
			route    provider.Route
			rank     int
			line     int
			position int
		}
		var candidates []candidate
		for lineIndex, admitted := range step.Limits.AdmittedModelLines {
			reference := contract.ModelReference(admitted.ModelLine)
			if step.Target.Kind == contract.GrantTargetModel {
				if admitted.ModelLine != *step.Target.ModelLine {
					continue
				}
				reference = step.Target.Reference()
			}
			if pinned, seen := revisionOf[admitted.ModelLine]; seen {
				// One execution never switches a line's revision silently.
				reference = pinned
			}
			set, err := snapshot.Resolve(reference, at)
			if err != nil {
				continue
			}
			for position, route := range set.Candidates() {
				if !admitsRoute(step.Limits, route) {
					continue
				}
				if _, taken := resolved.steps[route.DeploymentID]; taken {
					continue
				}
				ceiling := provider.FundingTierNone
				if step.Limits.MaxFundingTier != nil {
					ceiling = provider.FundingTier(*step.Limits.MaxFundingTier)
				}
				rank := fundingRankUnknown
				if grant.CustomerProviderCredential == nil && funding != nil {
					cheapest, admissible := funding.CheapestTier(route, ceiling)
					if !admissible {
						continue
					}
					rank = cheapest
				}
				candidates = append(candidates, candidate{route: route, rank: rank, line: lineIndex, position: position})
			}
		}
		sort.SliceStable(candidates, func(a, b int) bool {
			if candidates[a].rank != candidates[b].rank {
				return candidates[a].rank < candidates[b].rank
			}
			if candidates[a].line != candidates[b].line {
				return candidates[a].line < candidates[b].line
			}
			return candidates[a].position < candidates[b].position
		})
		// With sameModelFailover false each admitted line is served by ONE
		// deployment (one provider) in this step: the best-ranked admissible
		// one, with no other provider of the line behind it.
		servedInStep := make(map[contract.ModelID]bool)
		for _, chosen := range candidates {
			route := chosen.route
			line := route.ModelReference.ModelID()
			if pinned, seen := revisionOf[line]; seen && pinned != route.ModelReference {
				continue
			}
			if !step.Limits.SameModelFailoverAllowed() {
				if servedInStep[line] {
					continue
				}
				servedInStep[line] = true
			}
			revisionOf[line] = route.ModelReference
			authorized := contract.AuthorizedRoute{
				Substitution:               contract.SubstitutionSameModel,
				DeploymentID:               route.DeploymentID,
				ModelReference:             route.ModelReference,
				Provider:                   route.Provider,
				Regions:                    append([]contract.Region{}, route.Regions...),
				MaxFundingTier:             step.Limits.MaxFundingTier,
				CustomerProviderCredential: grant.CustomerProviderCredential,
			}
			if len(resolved.routes) > 0 && line != resolved.routes[0].ModelReference.ModelID() {
				// Oxy admitted this line in the grant: the substitution is
				// authorized by the customer's policy, as the grant states.
				authorizedByPolicy := true
				authorized.Substitution = contract.SubstitutionCrossModel
				authorized.AuthorizedByPolicy = &authorizedByPolicy
			}
			resolved.routes = append(resolved.routes, authorized)
			resolved.steps[route.DeploymentID] = stepIndex
		}
	}
	if len(resolved.routes) == 0 {
		return nil, contract.NewPublicError(grant.RequestID, "no_route_available", contract.PublicMessage("no_route_available"))
	}
	first := grant.Steps[0].Target
	switch {
	case first.Kind == contract.GrantTargetModel && allOnLine(resolved.routes, *first.ModelLine):
		reference := first.Reference()
		resolved.target = contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference}
	case first.Kind == contract.GrantTargetModel:
		// A model target whose later steps admit other lines (a fallback the
		// customer's policy allows): the executor's model target forbids a
		// substitute, so the request is carried as the policy's target.
		profile := contract.RoutingProfileID("model:" + string(*first.ModelLine))
		resolved.target = contract.RoutingTarget{Kind: contract.TargetRoutingProfileID, RoutingProfileID: &profile}
	default:
		profile := contract.RoutingProfileID("power-" + string(*first.Level))
		resolved.target = contract.RoutingTarget{Kind: contract.TargetRoutingProfileID, RoutingProfileID: &profile}
	}
	return resolved, nil
}

func allOnLine(routes []contract.AuthorizedRoute, line contract.ModelID) bool {
	for _, route := range routes {
		if route.ModelReference.ModelID() != line {
			return false
		}
	}
	return true
}

// admitsRoute applies the step limits Kaana can verify from its inventory:
// providers and regions. A region limit admits only a route with a stated
// region set that satisfies it; a route with no regional attestation is
// refused under any region limit, because "unknown" is not "allowed".
func admitsRoute(limits contract.GrantStepLimits, route provider.Route) bool {
	if len(limits.ProviderAllowlist) > 0 && !slices.Contains(limits.ProviderAllowlist, route.Provider) {
		return false
	}
	if slices.Contains(limits.ProviderDenylist, route.Provider) {
		return false
	}
	if len(limits.AllowedRegions) > 0 || len(limits.DeniedRegions) > 0 {
		if len(route.Regions) == 0 {
			return false
		}
		for _, region := range route.Regions {
			if len(limits.AllowedRegions) > 0 && !slices.Contains(limits.AllowedRegions, region) {
				return false
			}
			if slices.Contains(limits.DeniedRegions, region) {
				return false
			}
		}
	}
	return true
}

package publicapi

import (
	"math/big"
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
// grant's line order, then recent time to first output in coarse bands
// (latencyBand), then the inventory's. A deployment a previous step
// already authorized is not repeated: a request moves to the next step only
// when the previous one has no admissible route left, which the executor
// decides attempt by attempt and only before any output.
//
// A step with `sameModelFailover: false` keeps only the best-ranked deployment
// of each admitted line (one provider; its key pool still walks free → credit →
// paid inside that deployment, ADR 0001), so the request never moves to another
// provider of the same line within the step.
//
// A request's `needs` (the controls it carries, see carriedParameters, and its
// size) drop a deployment whose published accepted-parameter set refuses one or
// whose observed window it cannot fit; a deployment with no such statement is
// kept, as absence is "nobody said".
//
// Kaana never adds a model line Oxy did not admit and never widens a limit.
// What it cannot verify from its inventory (zero data retention, training,
// licences) is the policy Oxy applied when it computed admittedModelLines.
func resolveGrant(grant *contract.Grant, snapshot *inventory.Inventory, funding FundingRanker, performance PerformanceReader, serves func(provider.Route) bool, needs requestNeeds, at time.Time) (*plan, *contract.PublicError) {
	if needs.any() {
		// Prefer routes whose published accepted-parameter set takes every
		// control the request carries and whose window it fits (ADR 0008). When
		// none does, resolve as before: the attempt then refuses with the exact
		// parameter named, which is a better answer than "no route". The
		// caller's hard preferences (`only`, `ignore`, `quantizations`,
		// `require_parameters`) apply in both passes.
		if resolved, failure := resolveGrantFor(grant, snapshot, funding, performance, serves, needs, at); failure == nil {
			return resolved, nil
		}
	}
	return resolveGrantFor(grant, snapshot, funding, performance, serves, needs.hardOnly(), at)
}

// routeAuthorization is the internal execution boundary shared by verified legacy
// grants and Kaana's native admissions. It is never an invented Oxy grant.
type routeAuthorization struct {
	requestID          contract.RequestID
	steps              []contract.GrantStep
	customerCredential *contract.CustomerProviderCredential
	permits            func(int, provider.Route) bool
}

func resolveGrantFor(grant *contract.Grant, snapshot *inventory.Inventory, funding FundingRanker, performance PerformanceReader, serves func(provider.Route) bool, needs requestNeeds, at time.Time) (*plan, *contract.PublicError) {
	return resolveAuthorizedFor(routeAuthorization{requestID: grant.RequestID, steps: grant.Steps, customerCredential: grant.CustomerProviderCredential}, snapshot, funding, performance, serves, needs, at)
}

func resolveAuthorizedFor(authorization routeAuthorization, snapshot *inventory.Inventory, funding FundingRanker, performance PerformanceReader, serves func(provider.Route) bool, needs requestNeeds, at time.Time) (*plan, *contract.PublicError) {
	resolved := &plan{steps: make(map[contract.DeploymentID]int)}
	revisionOf := make(map[contract.ModelID]contract.ModelReference)
	for stepIndex, step := range authorization.steps {
		type candidate struct {
			route provider.Route
			rank  int
			line  int
			// providerRank, slow, sortBand and affine are the caller's own
			// preferences (provider.order, preferred_*, provider.sort and
			// prompt_cache_key); band is Kaana's recent-latency tie-break.
			providerRank int
			slow         bool
			sortBand     int
			priceKey     *big.Rat
			affine       bool
			band         int
			position     int
		}
		var candidates []candidate
		for lineIndex, admitted := range step.Limits.AdmittedModelLines {
			var price linePrice
			if needs.prices != nil && (needs.prefs.hasCeiling() || needs.prefs.sortsByPrice()) {
				price = needs.prices.LinePrice(admitted.ModelLine, admitted.PriceVersionID)
			}
			if !needs.prefs.within(price) {
				// A line over the caller's ceiling, or one whose price the
				// catalogue does not state, is not used: the preference only
				// narrows what the grant admitted.
				continue
			}
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
				if !admitsRoute(step.Limits, route) || (serves != nil && !serves(route)) || (authorization.permits != nil && !authorization.permits(stepIndex, route)) {
					continue
				}
				if needs.refusedBy(route) || needs.prefs.excludes(route, needs.parameters) {
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
				if authorization.customerCredential == nil && funding != nil {
					cheapest, admissible := funding.CheapestTier(route, ceiling)
					if !admissible {
						continue
					}
					rank = cheapest
				}
				candidates = append(candidates, candidate{
					route: route, rank: rank, line: lineIndex, position: position,
					providerRank: needs.prefs.providerRank(route.Provider),
					slow:         needs.prefs.slow(performance, route.DeploymentID),
					sortBand:     needs.prefs.sortBand(performance, route.DeploymentID),
					priceKey:     price.blended(),
					affine:       needs.affine != "" && route.DeploymentID == needs.affine,
					band:         latencyBand(performance, route.DeploymentID),
				})
			}
		}
		sort.SliceStable(candidates, func(a, b int) bool {
			if candidates[a].rank != candidates[b].rank {
				return candidates[a].rank < candidates[b].rank
			}
			if needs.prefs.sortsByPrice() {
				if byPrice := comparePrice(candidates[a].priceKey, candidates[b].priceKey); byPrice != 0 {
					return byPrice < 0
				}
			}
			if candidates[a].line != candidates[b].line {
				return candidates[a].line < candidates[b].line
			}
			if candidates[a].providerRank != candidates[b].providerRank {
				return candidates[a].providerRank < candidates[b].providerRank
			}
			if candidates[a].slow != candidates[b].slow {
				return !candidates[a].slow
			}
			if candidates[a].sortBand != candidates[b].sortBand {
				return candidates[a].sortBand < candidates[b].sortBand
			}
			if candidates[a].affine != candidates[b].affine {
				return candidates[a].affine
			}
			if candidates[a].band != candidates[b].band {
				return candidates[a].band < candidates[b].band
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
				CustomerProviderCredential: authorization.customerCredential,
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
	if needs.prefs.keepsOneProvider() {
		resolved.routes = keepOneProvider(resolved.routes, needs.prefs)
	}
	if len(resolved.routes) == 0 {
		return nil, contract.NewPublicError(authorization.requestID, "no_route_available", contract.PublicMessage("no_route_available"))
	}
	first := authorization.steps[0].Target
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

// carriedParameters is the set of caller controls a text request carries, in
// the accepted-parameter vocabulary, as the upstream wire would send them: a
// `text` response format sends nothing, `auto` tool choice is every provider's
// default, and a tool choice is a value only when it is none, required or a
// named function (openaicompat.wireToolChoiceValue reads the same way).
func carriedParameters(maxOutput *int, sampling contract.SamplingParameters, tools int, choice *contract.ToolChoice, format *contract.ResponseFormat, reasoning *contract.ReasoningParameters) []provider.RequestParameter {
	var carried []provider.RequestParameter
	add := func(present bool, parameter provider.RequestParameter) {
		if present {
			carried = append(carried, parameter)
		}
	}
	add(maxOutput != nil, provider.ParameterMaxOutputTokens)
	// `exclude` and an empty `reasoning` need nothing from a route: Kaana
	// delivers or withholds reasoning itself.
	add(reasoning.HasEffort(), provider.ParameterReasoningEffort)
	add(reasoning != nil && reasoning.MaxTokens != nil, provider.ParameterReasoningMaxTokens)
	add(format != nil && format.Type != contract.ResponseFormatText, provider.ParameterResponseFormat)
	add(sampling.FrequencyPenalty != nil, provider.ParameterFrequencyPenalty)
	add(sampling.PresencePenalty != nil, provider.ParameterPresencePenalty)
	add(sampling.Seed != nil, provider.ParameterSeed)
	add(len(sampling.StopSequences) > 0, provider.ParameterStopSequences)
	add(sampling.Temperature != nil, provider.ParameterTemperature)
	add(sampling.TopP != nil, provider.ParameterTopP)
	add(tools > 0, provider.ParameterTools)
	if choice != nil {
		switch {
		case choice.Function != nil:
			carried = append(carried, provider.ParameterToolChoiceFunction)
		case choice.Mode != nil && *choice.Mode == contract.ToolChoiceNone:
			carried = append(carried, provider.ParameterToolChoiceNone)
		case choice.Mode != nil && *choice.Mode == contract.ToolChoiceRequired:
			carried = append(carried, provider.ParameterToolChoiceRequired)
		}
	}
	return carried
}

// latencyBandRatio is how much slower than the band below a deployment must be
// to fall into the next band. Bands, not a comparison with a margin, because
// "within 25%" is not transitive and a sort over it is not well defined; bands
// also keep two deployments of near-equal speed in inventory order instead of
// swapping places on noise.
const latencyBandRatio = 1.5

// latencyBandFloor is the time to first output below which every deployment is
// in band 1: under 100 ms the difference is not one a caller can feel.
const latencyBandFloor = 100 * time.Millisecond

// latencyBand ranks a deployment by recent time to first output. An unobserved
// deployment is in band 0, ahead of every observed one: unknown is not slow,
// and a cold deployment must get traffic to be measured at all. It only orders
// routes that are equal on funding and line, never excludes one, and is the
// "latency and health signals" ADR 0001 decision 3 already named (ADR 0008).
func latencyBand(performance PerformanceReader, deployment contract.DeploymentID) int {
	if performance == nil {
		return 0
	}
	observed, ok := performance.FirstOutput(deployment)
	if !ok {
		return 0
	}
	band := 1
	for limit := latencyBandFloor; observed > limit; limit = time.Duration(float64(limit) * latencyBandRatio) {
		band++
	}
	return band
}

// requestNeeds is what a request asks of a route beyond where it may go: the
// controls it carries and how much it needs to read and write. It stays in
// Kaana and is never sent to Oxy.
type requestNeeds struct {
	parameters []provider.RequestParameter
	// prefs are the caller's `provider` preferences, and affine the deployment
	// that last served its prompt_cache_key; both only narrow or reorder.
	prefs  *providerPreferences
	affine contract.DeploymentID
	// prices reads a model line's declared list price for `provider.max_price`
	// and `provider.sort: "price"`; nil states no price.
	prices LinePricer
	// inputTokens is the content-free estimate Oxy sized the hold from; zero
	// when the request has none.
	inputTokens     int
	maxOutputTokens *int
}

func (n requestNeeds) any() bool {
	return len(n.parameters) > 0 || n.inputTokens > 0 || n.maxOutputTokens != nil
}

// hardOnly is the needs with the soft preferences dropped: the request's
// controls and size no longer steer route choice, but the caller's hard
// preferences stay, and `require_parameters` keeps the controls it checks.
func (n requestNeeds) hardOnly() requestNeeds {
	kept := requestNeeds{prefs: n.prefs, affine: n.affine, prices: n.prices}
	if n.prefs != nil && n.prefs.RequireParameters != nil && *n.prefs.RequireParameters {
		kept.parameters = n.parameters
	}
	return kept
}

// refusedBy reports whether the route's published statements say it cannot
// serve this request: a control it does not accept, an input larger than its
// context window, or an output limit above its own. Unknown statements refuse
// nothing, and the estimate is rough, so only a clear excess refuses.
func (n requestNeeds) refusedBy(route provider.Route) bool {
	if _, refused := route.UnacceptedParameter(n.parameters...); refused {
		return true
	}
	if route.ContextTokens != nil && n.inputTokens > *route.ContextTokens {
		return true
	}
	return route.MaxOutputTokens != nil && n.maxOutputTokens != nil && *n.maxOutputTokens > *route.MaxOutputTokens
}

// throughputBandFloor is the output rate at or above which every deployment is
// in the first throughput band; each 1.5x slower is the next band.
const throughputBandFloor = 100.0

// sortBand is the band `provider.sort` ranks a route by: the recent latency
// band for `latency`, the recent throughput band for `throughput`, and 0 for
// no sort. An unobserved deployment is in band 0, ahead of observed ones, for
// the reason latencyBand gives.
func (p *providerPreferences) sortBand(performance PerformanceReader, id contract.DeploymentID) int {
	if p == nil || p.Sort == nil || performance == nil {
		return 0
	}
	if *p.Sort == "latency" {
		return latencyBand(performance, id)
	}
	observed, ok := performance.Throughput(id)
	if !ok {
		return 0
	}
	band := 1
	for floor := throughputBandFloor; observed < floor && band < 64; floor /= latencyBandRatio {
		band++
	}
	return band
}

// keepOneProvider applies `allow_fallbacks: false`: with `order`, the routes of
// the listed providers (in their ranked order); without it, the single
// best-ranked route. The request is never moved to another provider.
func keepOneProvider(routes []contract.AuthorizedRoute, prefs *providerPreferences) []contract.AuthorizedRoute {
	if len(prefs.Order) == 0 {
		return routes[:min(len(routes), 1)]
	}
	kept := routes[:0:0]
	for _, route := range routes {
		if slices.Contains(prefs.Order, string(route.Provider)) {
			kept = append(kept, route)
		}
	}
	return kept
}

// comparePrice orders two lines by blended list price, cheaper first; a line
// whose price is not stated ranks after every priced one.
func comparePrice(a, b *big.Rat) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return a.Cmp(b)
}

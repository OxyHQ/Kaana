package publisher

import (
	"fmt"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"time"
)

type privateAutoPublicationPermit struct {
	Approval       contract.PrivateAutoSourceApproval
	PublishedPrice providercost.ListPrice
	Cards          *providercost.Cards
	Eligibility    *Decider
}

func privateAutoPermitForApproval(audience *contract.PrivateAutoSourceApproval, cards *providercost.Cards, eligibility *Decider, at time.Time) (*privateAutoPublicationPermit, error) {
	if audience == nil || !audience.NotExpired(at) {
		return nil, nil
	}
	if audience.Validate() != nil || eligibility == nil || !eligibility.now.Equal(at) {
		return nil, fmt.Errorf("publisher: reviewed private authority lacks fresh canonical eligibility")
	}
	observation, loaded := cards.ObservationForDeployment(audience.DeploymentID)
	price, priced := cards.ScopedDecisionPublishedPrice(audience.DeploymentID, at)
	if !loaded || !priced || observation.VersionID != audience.ProviderRateCardVersionID || observation.SourceVersion != audience.ProviderSourceVersion {
		return nil, fmt.Errorf("publisher: reviewed private authority lacks its actual immutable card")
	}
	return &privateAutoPublicationPermit{Approval: *audience, PublishedPrice: price, Cards: cards, Eligibility: eligibility}, nil
}

func privateAutoCandidate(discoveries []Discovery, permit *privateAutoPublicationPermit, at time.Time, withhold Withhold, reportOnly bool) (*snapshotDeployment, error) {
	if permit == nil {
		return nil, nil
	}
	scope := permit.Approval
	if scope.Validate() != nil || scope.Provider != "openrouter" || scope.UpstreamModelID != "typesafe/jev-1.13-20260917" || withhold == nil || reportOnly {
		return nil, fmt.Errorf("publisher: scoped publication is not eligible")
	}
	if !scope.NotExpired(at) {
		return nil, fmt.Errorf("publisher: scoped publication expired")
	}
	if permit.Eligibility == nil || !permit.Eligibility.now.Equal(at) {
		return nil, fmt.Errorf("publisher: fresh publication eligibility unavailable")
	}
	key, bound := permit.Eligibility.resolve(scope.DeploymentID, scope.Provider, permit.Eligibility.evidence.Keys[scope.Provider])
	if !bound || key.KeyID != scope.KeyID {
		return nil, fmt.Errorf("publisher: scoped exact execution key is not eligible")
	}
	if _, blocked := permit.Eligibility.Decide(scope.DeploymentID, scope.Provider); blocked {
		return nil, nil
	}
	observation, ok := permit.Cards.ObservationForDeployment(scope.DeploymentID)
	if !ok || observation.VersionID != scope.ProviderRateCardVersionID || observation.SourceVersion != scope.ProviderSourceVersion || !permit.Cards.MatchesPublishedTokenPrice(scope.DeploymentID, permit.PublishedPrice, at) {
		return nil, fmt.Errorf("publisher: scoped actual card identity or price differs")
	}
	var candidate *snapshotDeployment
	for _, discovery := range discoveries {
		if discovery.Provider.APIKey == "" || discovery.Provider.Slug != scope.Provider || discovery.Provider.CredentialKeyID != scope.KeyID {
			continue
		}
		for _, model := range discovery.Models {
			if model.CanonicalSlug != scope.UpstreamModelID || model.UpstreamModelID != "typesafe/jev-1.13" || model.Unservable != "" || model.Observed == nil || model.Observed.ListPrice == nil || *model.Observed.ListPrice != permit.PublishedPrice {
				continue
			}
			if candidate != nil {
				return nil, fmt.Errorf("publisher: scoped discovery is ambiguous")
			}
			audience := scope
			candidate = &snapshotDeployment{PrivateAutoSourceApproval: &audience, DeploymentID: scope.DeploymentID, Provider: scope.Provider, ModelReference: scope.ModelReference, UpstreamModelID: scope.UpstreamModelID, Regions: append([]contract.Region(nil), discovery.Provider.Regions...), Current: false, Observed: model.Observed}
		}
	}
	if candidate == nil {
		return nil, fmt.Errorf("publisher: scoped authenticated catalogue binding absent")
	}
	if !sameAutoRegions(scope.Regions, candidate.Regions) {
		return nil, fmt.Errorf("publisher: private Auto regions differ")
	}
	if _, withheld := withhold(scope.DeploymentID, scope.Provider); withheld {
		return nil, nil
	}
	return candidate, nil
}

func sameAutoRegions(a, b []contract.Region) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[contract.Region]bool{}
	for _, r := range a {
		seen[r] = true
	}
	for _, r := range b {
		if !seen[r] {
			return false
		}
		delete(seen, r)
	}
	return len(seen) == 0
}
func (p *Publisher) privateAutoPermitForCycle(approval *contract.PrivateAutoSourceApproval, eligibility *Decider, at time.Time) *privateAutoPublicationPermit {
	permit, err := privateAutoPermitForApproval(approval, p.cards, eligibility, at)
	if err != nil {
		p.logger.Warn("private Auto omitted; ordinary refresh continues", "reason", "private_auto_prerequisites_unavailable")
		return nil
	}
	return permit
}

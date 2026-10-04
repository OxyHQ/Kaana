package publisher

import (
	"fmt"
	"github.com/OxyHQ/Kaana/internal/contract"

	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/scopedpermit"
	"time"
)

// privatePublicationPermit is source review, never an environment/config switch.
// It only narrows an authenticated discovery and normal withholding decision.
type privatePublicationPermit struct {
	Audience       contract.ScopedExecutionAudience
	PublishedPrice providercost.ListPrice
	Cards          *providercost.Cards
	Eligibility    *Decider
}

// Only compiled source approval can activate private publication. Actual cards
// and the same-cycle database Decider are dependencies, never caller authority.
func sourceReviewedPrivatePermit(cards *providercost.Cards, eligibility *Decider, at time.Time) (*privatePublicationPermit, error) {
	return privatePermitForAudience(scopedpermit.SourceReviewedAudience(), cards, eligibility, at)
}

func privatePermitForAudience(audience *contract.ScopedExecutionAudience, cards *providercost.Cards, eligibility *Decider, at time.Time) (*privatePublicationPermit, error) {
	if audience == nil || !audience.NotExpired(at) {
		return nil, nil
	}
	if !scopedpermit.Matches(audience, audience, at) || eligibility == nil || !eligibility.now.Equal(at) {
		return nil, fmt.Errorf("publisher: reviewed private authority lacks fresh canonical eligibility")
	}
	observation, loaded := cards.Observation()
	price, priced := cards.ScopedDecisionPublishedPrice(audience.DeploymentID, at)
	if !loaded || !priced || observation.VersionID != audience.ProviderRateCardVersionID || observation.SourceVersion != audience.ProviderSourceVersion {
		return nil, fmt.Errorf("publisher: reviewed private authority lacks its actual immutable card")
	}
	return &privatePublicationPermit{Audience: *audience, PublishedPrice: price, Cards: cards, Eligibility: eligibility}, nil
}

func scopedCandidate(discoveries []Discovery, permit *privatePublicationPermit, at time.Time, withhold Withhold, reportOnly bool) (*snapshotDeployment, error) {
	if permit == nil {
		return nil, nil
	}
	scope := permit.Audience
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
	observation, ok := permit.Cards.Observation()
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
			candidate = &snapshotDeployment{ScopedExecution: &audience, DeploymentID: scope.DeploymentID, Provider: scope.Provider, ModelReference: scope.ModelReference, UpstreamModelID: scope.UpstreamModelID, Regions: append([]contract.Region(nil), discovery.Provider.Regions...), Current: false, Observed: model.Observed}
		}
	}
	if candidate == nil {
		return nil, fmt.Errorf("publisher: scoped authenticated catalogue binding absent")
	}
	if _, withheld := withhold(scope.DeploymentID, scope.Provider); withheld {
		return nil, nil
	}
	return candidate, nil
}

// A missing private prerequisite withdraws that row, never ordinary inventory.
// No previous private permit is carried forward and logs contain no authority.
func (p *Publisher) privatePermitForCycle(audience *contract.ScopedExecutionAudience, eligibility *Decider, at time.Time) *privatePublicationPermit {
	permit, err := privatePermitForAudience(audience, p.cards, eligibility, at)
	if err != nil {
		p.logger.Warn("private deployment omitted; ordinary inventory refresh continues", "reason", "private_prerequisites_unavailable")
		return nil
	}
	return permit
}

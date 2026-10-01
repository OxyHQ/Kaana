package publisher

import (
	"fmt"
	"github.com/OxyHQ/Kaana/internal/contract"

	"github.com/OxyHQ/Kaana/internal/providercost"
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

// No reviewed principal/funding/privacy/immutable route publication exists yet.
func sourceReviewedPrivatePermit() *privatePublicationPermit { return nil }

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

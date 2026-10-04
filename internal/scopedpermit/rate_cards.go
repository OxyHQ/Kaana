package scopedpermit

import (
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

const ordinaryRateCardPath = "/etc/kaana-rates/provider-rates.json"
const privateRateCardPath = "/etc/kaana-rates/provider-rates-jev-scoped.json"

// LoadRateCards preserves optional ordinary pricing while compiled authority is
// absent. Only that source authority appends the baked private observation;
// environment configuration cannot activate private pricing or publication.
func LoadRateCards(ordinaryPath string) (*providercost.Cards, error) {
	auto := SourceReviewedPrivateAutoApproval()
	if auto == nil {
		return loadRateCards(ordinaryPath, ordinaryRateCardPath, privateRateCardPath, SourceReviewedAudience())
	}
	if auto.Validate() != nil {
		return nil, fmt.Errorf("scopedpermit: invalid private Auto source")
	}
	if ordinaryPath == "" {
		ordinaryPath = ordinaryRateCardPath
	}
	cards, err := providercost.Load(ordinaryPath, privateRateCardPath)
	if err != nil {
		return nil, err
	}
	observation, ok := cards.ObservationForDeployment(auto.DeploymentID)
	if !ok || observation.VersionID != auto.ProviderRateCardVersionID || observation.SourceVersion != auto.ProviderSourceVersion {
		return nil, fmt.Errorf("scopedpermit: private Auto actual card differs")
	}
	return cards, nil
}

func loadRateCards(ordinaryPath, defaultPath, privatePath string, audience *contract.ScopedExecutionAudience) (*providercost.Cards, error) {
	if audience == nil {
		if ordinaryPath == "" {
			return nil, nil
		}
		return providercost.Load(ordinaryPath)
	}
	if ordinaryPath == "" {
		ordinaryPath = defaultPath
	}
	cards, err := providercost.Load(ordinaryPath, privatePath)
	if err != nil {
		return nil, err
	}
	observation, ok := cards.ObservationForDeployment(audience.DeploymentID)
	if !ok || observation.VersionID != audience.ProviderRateCardVersionID || observation.SourceVersion != audience.ProviderSourceVersion {
		return nil, fmt.Errorf("scopedpermit: baked rate card does not match reviewed audience")
	}
	return cards, nil
}

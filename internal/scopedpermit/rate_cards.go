package scopedpermit

import (
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

const ordinaryRateCardPath = "/etc/kaana-rates/provider-rates.json"
const privateRateCardPath = "/etc/kaana-rates/provider-rates-jev-scoped.json"
const privateAutoRateCardPath = "/etc/kaana-rates/provider-rates-jev-private-auto.json"

// LoadRateCards loads each private observation only for its independent compiled
// authority. Auto never substitutes for, or bypasses, the scoped observation.
func LoadRateCards(ordinaryPath string) (*providercost.Cards, error) {
	return loadRateCards(ordinaryPath, ordinaryRateCardPath, privateRateCardPath, privateAutoRateCardPath,
		SourceReviewedAudience(), SourceReviewedPrivateAutoApproval())
}

func loadRateCards(ordinaryPath, defaultPath, scopedPath, autoPath string,
	audience *contract.ScopedExecutionAudience, auto *contract.PrivateAutoSourceApproval) (*providercost.Cards, error) {
	if audience == nil && auto == nil {
		if ordinaryPath == "" {
			return nil, nil
		}
		return providercost.Load(ordinaryPath)
	}
	if auto != nil && auto.Validate() != nil {
		return nil, fmt.Errorf("scopedpermit: invalid private Auto source")
	}
	if ordinaryPath == "" {
		ordinaryPath = defaultPath
	}
	paths := []string{ordinaryPath}
	if audience != nil {
		paths = append(paths, scopedPath)
	}
	if auto != nil {
		paths = append(paths, autoPath)
	}
	cards, err := providercost.Load(paths...)
	if err != nil {
		return nil, err
	}
	if audience != nil {
		observation, ok := cards.ObservationForDeployment(audience.DeploymentID)
		if !ok || observation.VersionID != audience.ProviderRateCardVersionID || observation.SourceVersion != audience.ProviderSourceVersion {
			return nil, fmt.Errorf("scopedpermit: baked rate card does not match reviewed audience")
		}
	}
	if auto != nil {
		observation, ok := cards.ObservationForDeployment(auto.DeploymentID)
		if !ok || observation.VersionID != auto.ProviderRateCardVersionID || observation.SourceVersion != auto.ProviderSourceVersion {
			return nil, fmt.Errorf("scopedpermit: private Auto actual card differs")
		}
	}
	return cards, nil
}

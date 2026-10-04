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
	return loadRateCards(ordinaryPath, ordinaryRateCardPath, privateRateCardPath, SourceReviewedAudience())
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

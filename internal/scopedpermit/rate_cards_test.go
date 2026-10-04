package scopedpermit

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

func TestAbsentAuthorityNeverLoadsPrivateCard(t *testing.T) {
	if SourceReviewedAudience() != nil {
		t.Fatal("this inactive preparation must not carry a source permit")
	}
	cards, err := LoadRateCards("")
	if err != nil || cards != nil {
		t.Fatalf("ordinary optional pricing changed: %v", err)
	}
	ordinary := "../../configs/provider-rates.json"
	cards, err = loadRateCards(ordinary, "absent-default", "absent-private", nil)
	if err != nil || len(cards.Observations()) != 1 {
		t.Fatalf("absent authority tried to load private pricing: %v", err)
	}
}

func TestReviewedCardAppendPreservesEachObservation(t *testing.T) {
	ordinary := "../../configs/provider-rates.json"
	private := "../../configs/provider-rates-jev-scoped.json"
	raw, err := os.ReadFile(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != "5a094d5d27005f7182c99cb19129d328f48576147940db91bbb5fc0008219968" {
		t.Fatal("historical xAI observation bytes changed")
	}
	baseline, err := providercost.Load(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", ordinary} {
		t.Run(fmt.Sprintf("configured=%t", path != ""), func(t *testing.T) {
			cards, err := loadRateCards(path, ordinary, private, cardBindingFixture())
			if err != nil || len(cards.Observations()) != 2 {
				t.Fatalf("separate observations not loaded: %v", err)
			}
			xai := contract.DeploymentID("dep_xai_realtime_grok_voice_think_fast_2_0_observed_2026_09_30")
			before, _ := baseline.ObservationForDeployment(xai)
			after, _ := cards.ObservationForDeployment(xai)
			if !reflect.DeepEqual(before, after) || after.ExpiresAt != nil || after.ObservedAt.Format(time.RFC3339) != "2026-09-30T00:00:00Z" {
				t.Fatal("append changed xAI identity/time/expiry")
			}
			usage := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}
			if !reflect.DeepEqual(baseline.Measure(xai, usage), cards.Measure(xai, usage)) {
				t.Fatal("append changed ordinary measurement")
			}
			jev, ok := cards.ObservationForDeployment(cardBindingFixture().DeploymentID)
			if !ok || jev.VersionID != cardBindingFixture().ProviderRateCardVersionID || jev.SourceVersion != cardBindingFixture().ProviderSourceVersion || jev.ObservedAt.Format(time.RFC3339Nano) != "2026-10-04T13:12:32.36019Z" {
				t.Fatal("private observation identity was replaced")
			}
			if !cards.MatchesPublishedTokenPrice(cardBindingFixture().DeploymentID, providercost.ListPrice{Currency: "USD", Input: "0.042", Output: "0"}, time.Date(2026, 10, 4, 14, 0, 0, 0, time.UTC)) {
				t.Fatal("independent card does not match exact published price")
			}
		})
	}
}

func TestPrivateAppendRefusesMissingOrMismatchedEvidence(t *testing.T) {
	ordinary := "../../configs/provider-rates.json"
	private := "../../configs/provider-rates-jev-scoped.json"
	for _, field := range []string{"deployment", "version", "source"} {
		t.Run(field, func(t *testing.T) {
			a := cardBindingFixture()
			switch field {
			case "deployment":
				a.DeploymentID = "foreign-deployment"
			case "version":
				a.ProviderRateCardVersionID = "foreign-card"
			case "source":
				a.ProviderSourceVersion = "foreign-observation"
			}
			if _, err := loadRateCards(ordinary, ordinary, private, a); err == nil {
				t.Fatal("foreign card evidence admitted")
			}
		})
	}
	for _, paths := range [][2]string{{"absent-ordinary", private}, {ordinary, "absent-private"}} {
		if _, err := loadRateCards(paths[0], ordinary, paths[1], cardBindingFixture()); err == nil {
			t.Fatal("missing configured card silently ignored")
		}
	}
	malformed := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(malformed, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRateCards(ordinary, ordinary, malformed, cardBindingFixture()); err == nil {
		t.Fatal("malformed private card ignored")
	}
}

// Only the observation binding is exercised here. This incomplete synthetic
// value is not a valid execution audience and never enters the exported getter.
func cardBindingFixture() *contract.ScopedExecutionAudience {
	return &contract.ScopedExecutionAudience{
		DeploymentID:              "dep_openrouter_typesafe_jev_1_13_scoped_2026_10_04",
		ProviderRateCardVersionID: "rc_openrouter_jev_2026_10_04",
		ProviderSourceVersion:     "openrouter-api/2026-10-04/typesafe/jev-1.13-20260917/556fab0c5da201c07d4eeafd32b48250fb3fa297b69ed0f9e62a7225ca8511ba",
	}
}

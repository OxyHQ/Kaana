package scopedpermit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

func TestPrivateAutoSourceMatchesExactReviewedApproval(t *testing.T) {
	raw, err := os.ReadFile("../../docs/audits/2026-10-05-private-auto-exact-activation/source-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != "48cc94c95a4d2c0987facb9c5f1bbe9afaee559d3c3b1d733f91f3ebfcb51d1e" {
		t.Fatal("reviewed input changed")
	}
	var expected contract.PrivateAutoSourceApproval
	if err = json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	actual := SourceReviewedPrivateAutoApproval()
	if actual == nil || !reflect.DeepEqual(actual, &expected) {
		t.Fatal("compiled source differs from exact reviewed approval")
	}
	expiry, err := time.Parse(time.RFC3339, expected.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if !actual.NotExpired(expiry.Add(-time.Millisecond)) || actual.NotExpired(expiry) || actual.NotExpired(expiry.Add(time.Millisecond)) {
		t.Fatal("source/evidence expiry boundary changed")
	}
	changed := SourceReviewedPrivateAutoApproval()
	changed.Principal.ApplicationID = "foreign"
	changed.Review.InternalUseAllowed = false
	if SourceReviewedPrivateAutoApproval().Equal(changed) {
		t.Fatal("caller mutated source authority")
	}
	if SourceReviewedAudience().ExpiresAt != "2026-10-05T01:28:26Z" {
		t.Fatal("Mention singleton expiry changed")
	}
}

func TestPrivateAutoActualThirdCardRetainsOriginalObservations(t *testing.T) {
	ordinary, scoped, autoPath := "../../configs/provider-rates.json", "../../configs/provider-rates-jev-scoped.json", "../../configs/provider-rates-jev-private-auto.json"
	baseline, err := providercost.Load(ordinary, scoped)
	if err != nil {
		t.Fatal(err)
	}
	actual := SourceReviewedPrivateAutoApproval()
	if actual == nil {
		t.Fatal("missing reviewed Auto source")
	}
	cards, err := loadRateCards("", ordinary, scoped, autoPath, SourceReviewedAudience(), actual)
	if err != nil || cards == nil || len(cards.Observations()) != 3 {
		t.Fatalf("actual three-card load: %v", err)
	}
	for _, previous := range baseline.Observations() {
		found := false
		for _, current := range cards.Observations() {
			if reflect.DeepEqual(previous, current) {
				found = true
			}
		}
		if !found {
			t.Fatal("original price/source/date/expiry changed")
		}
	}
	observed, ok := cards.ObservationForDeployment(actual.DeploymentID)
	if !ok || observed.VersionID != actual.ProviderRateCardVersionID || observed.SourceVersion != actual.ProviderSourceVersion {
		t.Fatal("actual Auto card/source mismatch")
	}
	for _, field := range []string{"deployment", "card", "source"} {
		changed := SourceReviewedPrivateAutoApproval()
		switch field {
		case "deployment":
			changed.DeploymentID = "foreign"
		case "card":
			changed.ProviderRateCardVersionID = "foreign"
		case "source":
			changed.ProviderSourceVersion = "foreign"
		}
		if _, err := loadRateCards("", ordinary, scoped, autoPath, SourceReviewedAudience(), changed); err == nil {
			t.Fatalf("foreign %s admitted", field)
		}
	}
	docker, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	ignore, err := os.ReadFile("../../.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(docker), "configs/provider-rates.json configs/provider-rates-jev-scoped.json configs/provider-rates-jev-private-auto.json /out/etc/kaana-rates/") || !strings.Contains(string(ignore), "\n!configs/provider-rates-jev-private-auto.json\n") || strings.Contains(string(ignore), "\n!configs/**\n") {
		t.Fatal("exact third card missing from closed Docker context/copy")
	}
}

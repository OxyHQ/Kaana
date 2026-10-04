package scopedpermit

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

func autoCardFixture(t *testing.T) (string, *contract.PrivateAutoSourceApproval) {
	t.Helper()
	raw, err := os.ReadFile("../contract/testdata/private-auto/golden-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	var auto contract.PrivateAutoSourceApproval
	if err = json.Unmarshal(raw, &auto); err != nil {
		t.Fatal(err)
	}
	auto.DeploymentID = "dep_synthetic_private_auto"
	auto.ProviderRateCardVersionID = "rc_synthetic_private_auto"
	auto.ProviderSourceVersion = "synthetic-private-auto-source"
	if err = auto.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("../../configs/provider-rates-jev-scoped.json")
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err = json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	card["rateCardVersionId"] = auto.ProviderRateCardVersionID
	card["sourceVersion"] = auto.ProviderSourceVersion
	card["rateCards"].([]any)[0].(map[string]any)["deploymentId"] = auto.DeploymentID
	raw, err = json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic-auto.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, &auto
}

func TestSeparateAutoCardRetainsThreeImmutableObservations(t *testing.T) {
	ordinary, scoped := "../../configs/provider-rates.json", "../../configs/provider-rates-jev-scoped.json"
	autoPath, auto := autoCardFixture(t)
	beforeBytes := map[string][32]byte{}
	for _, path := range []string{ordinary, scoped} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		beforeBytes[path] = sha256.Sum256(raw)
	}
	baseline, err := providercost.Load(ordinary, scoped)
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range []string{"", ordinary} {
		cards, err := loadRateCards(configured, ordinary, scoped, autoPath, cardBindingFixture(), auto)
		if err != nil || cards == nil || len(cards.Observations()) != 3 {
			t.Fatalf("three cards not loaded: %v", err)
		}
		for _, old := range baseline.Observations() {
			found := false
			for _, now := range cards.Observations() {
				if reflect.DeepEqual(old, now) {
					found = true
				}
			}
			if !found {
				t.Fatal("retained observation identity/time/expiry changed")
			}
		}
		observed, ok := cards.ObservationForDeployment(auto.DeploymentID)
		if !ok || observed.VersionID != auto.ProviderRateCardVersionID || observed.SourceVersion != auto.ProviderSourceVersion {
			t.Fatal("Auto observation differs")
		}
	}
	for path, want := range beforeBytes {
		raw, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(raw) != want {
			t.Fatal("old card bytes changed")
		}
	}
}

func TestStandaloneAutoDoesNotReadAbsentScopedCard(t *testing.T) {
	path, auto := autoCardFixture(t)
	ordinary := "../../configs/provider-rates.json"
	cards, err := loadRateCards("", ordinary, "absent-scoped", path, nil, auto)
	if err != nil || cards == nil || len(cards.Observations()) != 2 {
		t.Fatalf("standalone Auto not loaded: %v", err)
	}
	cards, err = loadRateCards(ordinary, "absent-default", "absent-scoped", "absent-auto", nil, nil)
	if err != nil || len(cards.Observations()) != 1 {
		t.Fatalf("absent authorities loaded a private card: %v", err)
	}
}

func TestAutoCannotBypassRetainedScopedCardValidation(t *testing.T) {
	path, auto := autoCardFixture(t)
	for _, field := range []string{"deployment", "version", "source"} {
		t.Run(field, func(t *testing.T) {
			scoped := cardBindingFixture()
			switch field {
			case "deployment":
				scoped.DeploymentID = "foreign"
			case "version":
				scoped.ProviderRateCardVersionID = "foreign"
			case "source":
				scoped.ProviderSourceVersion = "foreign"
			}
			if _, err := loadRateCards("", "../../configs/provider-rates.json", "../../configs/provider-rates-jev-scoped.json", path, scoped, auto); err == nil {
				t.Fatal("Auto bypassed scoped validation")
			}
		})
	}
}

func TestAutoCardFailsClosedForEveryBindingOrMissingFile(t *testing.T) {
	path, auto := autoCardFixture(t)
	for _, field := range []string{"deployment", "version", "source", "shape", "missing", "old-scoped-instead"} {
		t.Run(field, func(t *testing.T) {
			changed := *auto
			selected := path
			switch field {
			case "deployment":
				changed.DeploymentID = "foreign"
			case "version":
				changed.ProviderRateCardVersionID = "foreign"
			case "source":
				changed.ProviderSourceVersion = "foreign"
			case "shape":
				changed.Principal.Environment = "development"
			case "missing":
				selected = "absent-auto"
			case "old-scoped-instead":
				selected = "../../configs/provider-rates-jev-scoped.json"
			}
			if _, err := loadRateCards("", "../../configs/provider-rates.json", "absent-scoped", selected, nil, &changed); err == nil {
				t.Fatalf("Auto %s admitted", field)
			}
		})
	}
}

func TestAutoSourceUsesReviewedThirdCardPath(t *testing.T) {
	if source := SourceReviewedPrivateAutoApproval(); source == nil || source.ApprovalID != "alia-private-auto-internal-20261005-01" {
		t.Fatal("exact reviewed Auto source missing")
	}
	if privateAutoRateCardPath != "/etc/kaana-rates/provider-rates-jev-private-auto.json" {
		t.Fatalf("unexpected Auto path %q", privateAutoRateCardPath)
	}
}

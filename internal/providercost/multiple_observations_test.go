package providercost_test

import (
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const separateJev = `{"schemaVersion":1,"rateCardVersionId":"rc_jev_fixture","source":"provider_api","sourceVersion":"jev-api-oct4","observedAt":"2026-10-04T13:12:32Z","effectiveAt":"2026-10-04T13:12:32Z","expiresAt":"2026-10-04T13:17:32Z","rateCards":[{"deploymentId":"dep_jev_fixture","currency":"USD","rates":[{"unit":"input_tokens","amountPerUnit":42000},{"unit":"output_tokens","amountPerUnit":0}]}]}`

func loadSeparate(t *testing.T, second string) (*providercost.Cards, *providercost.Cards) {
	t.Helper()
	original, err := providercost.Load("../../configs/provider-rates.json")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "jev.json")
	if err := os.WriteFile(file, []byte(second), 0600); err != nil {
		t.Fatal(err)
	}
	combined, err := providercost.Load("../../configs/provider-rates.json", file)
	if err != nil {
		t.Fatal(err)
	}
	return original, combined
}

func TestSeparateObservationsPreserveOldXaiAndOwnJevWindow(t *testing.T) {
	old, combined := loadSeparate(t, separateJev)
	original, _ := old.Observation()
	xai := contract.DeploymentID("dep_xai_realtime_grok_voice_think_fast_2_0_observed_2026_09_30")
	retained, ok := combined.ObservationForDeployment(xai)
	if !ok || !reflect.DeepEqual(original, retained) {
		t.Fatal("xAI observation re-dated, re-versioned or changed")
	}
	if _, ok := combined.Observation(); ok {
		t.Fatal("multiple documents collapsed into a false single observation")
	}
	observations := combined.Observations()
	if len(observations) != 2 || observations[0].VersionID != "rc_jev_fixture" || observations[1].VersionID != original.VersionID {
		t.Fatal("independent registration records missing")
	}
	for _, at := range []time.Time{time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 4, 13, 13, 0, 0, time.UTC), time.Date(2026, 10, 4, 14, 0, 0, 0, time.UTC)} {
		usage := []providercost.AttemptUsage{{DeploymentID: xai, OccurredAt: at, Units: []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 2}}}}
		if !reflect.DeepEqual(old.MeasureRequest("xai", usage), combined.MeasureRequest("xai", usage)) {
			t.Fatal("new card changed old xAI cost/window/provenance")
		}
	}
	for _, tc := range []struct {
		at     time.Time
		priced bool
	}{
		{time.Date(2026, 10, 4, 13, 12, 31, 0, time.UTC), false},
		{time.Date(2026, 10, 4, 13, 13, 0, 0, time.UTC), true},
		{time.Date(2026, 10, 4, 13, 17, 32, 0, time.UTC), false},
	} {
		record := combined.MeasureRequest("jev", []providercost.AttemptUsage{{DeploymentID: "dep_jev_fixture", OccurredAt: tc.at, Units: []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 10}, {Unit: contract.UnitOutputTokens, Quantity: 0}}}})
		if record.Complete != tc.priced {
			t.Fatalf("Jev own window: %+v", record)
		}
		if tc.priced && (record.Attempts[0].RateCardVersionID != "rc_jev_fixture" || record.Attempts[0].Cost.Amount != 420000) {
			t.Fatal("Jev uses old/global observation or wrong price")
		}
		_, quoted := combined.ScopedDecisionPublishedPrice("dep_jev_fixture", tc.at)
		if quoted != tc.priced {
			t.Fatal("scoped price ignores own temporal observation")
		}
	}
	if _, ok := combined.ObservationForDeployment("foreign"); ok {
		t.Fatal("unpriced deployment assigned evidence")
	}
}

func TestMultipleFilesRejectAmbiguousVersionOrDeploymentAndMissingFile(t *testing.T) {
	folder := t.TempDir()
	a := filepath.Join(folder, "a.json")
	b := filepath.Join(folder, "b.json")
	if err := os.WriteFile(a, []byte(separateJev), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := providercost.Load(); err == nil {
		t.Fatal("empty file list accepted")
	}
	if _, err := providercost.Load(a, b); err == nil {
		t.Fatal("missing configured file ignored")
	}
	for _, document := range []string{separateJev, strings.Replace(separateJev, "rc_jev_fixture", "another-version", 1), strings.Replace(separateJev, "dep_jev_fixture", "another-deployment", 1)} {
		if err := os.WriteFile(b, []byte(document), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := providercost.Load(a, b); err == nil {
			t.Fatal("ambiguous version/deployment replaced immutable evidence")
		}
	}
}

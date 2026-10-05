package scopedpermit

import (
	"encoding/json"
	"github.com/OxyHQ/Kaana/internal/contract"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestReviewedMentionCommissioningSourceBindsExactAudienceAndExpires(t *testing.T) {
	raw, err := os.ReadFile("../../docs/audits/2026-10-05-mention-third-source/audience.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected contract.ScopedExecutionAudience
	if err = json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	audience := SourceReviewedAudience()
	if !reflect.DeepEqual(audience, &expected) {
		t.Fatal("compiled audience differs from root freeze")
	}
	if audience == nil || audience.Validate() != nil || audience.PermitID != "jev-mention-native-en-onepost-20261005-03" || audience.FixtureSHA256 != "0c12c43c70a269a40ca0e856af98c48db8d20573b2e1b7d75f9bd6f9468a73da" || audience.Principal.ApplicationID != "6a2f851751b784a86fd0e916" || audience.MaxCostUSD != "0.01" {
		t.Fatal("reviewed source tuple was not compiled exactly")
	}
	at := time.Date(2026, 10, 5, 7, 9, 11, 0, time.UTC)
	expires := time.Date(2026, 10, 5, 7, 9, 12, 0, time.UTC)
	if audience.ExpiresAt != "2026-10-05T07:09:12Z" || !Matches(audience, SourceReviewedAudience(), at) || Matches(nil, audience, at) || Matches(audience, nil, at) || Matches(audience, audience, expires) || Matches(audience, audience, expires.Add(time.Second)) {
		t.Fatal("absent or expired audience admitted, or exact positive refused")
	}

	previousRaw, err := os.ReadFile("../../docs/audits/2026-10-05-mention-distinct-source/audience.json")
	if err != nil {
		t.Fatal(err)
	}
	var previous contract.ScopedExecutionAudience
	if err = json.Unmarshal(previousRaw, &previous); err != nil {
		t.Fatal(err)
	}
	if Matches(&previous, audience, at) {
		t.Fatal("prior Mention source revision admitted")
	}
	for _, change := range []func(){
		func() { audience.Principal.ApplicationID = "6a2f851751b784a86fd0e934" },
		func() { audience.Principal.CredentialID = "foreign" },
		func() { audience.KeyID = "foreign" },
		func() { audience.FixtureSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" },
		func() { audience.Policy.PolicyVersion++ },
		func() { audience.PriceVersionID = "foreign" },
		func() { audience.ProviderSourceVersion = "foreign" },
		func() { audience.IdempotencyKey = "new-operation" },
		func() { audience.PermitID = previous.PermitID },
		func() { audience.IdempotencyKey = previous.IdempotencyKey },
		func() { audience.FixtureSHA256 = previous.FixtureSHA256 },
		func() { audience.DeploymentID = previous.DeploymentID },
		func() { audience.ProviderRateCardVersionID = previous.ProviderRateCardVersionID },
	} {
		audience = SourceReviewedAudience()
		change()
		if Matches(audience, SourceReviewedAudience(), at) {
			t.Fatal("changed audience admitted")
		}
	}
	// Returning a fresh value keeps consumers from mutating compiled approval.
	if SourceReviewedAudience().IdempotencyKey != "mention_jev_native_en_9aebfe5269e8fbe9e8c3961a93ef71d3" {
		t.Fatal("source approval was mutable")
	}
}

func TestReviewedMentionSourceLoadsIndependentCardWithoutReDatingXai(t *testing.T) {
	cards, err := loadRateCards("../../configs/provider-rates.json", "unused", "../../configs/provider-rates-jev-scoped.json", "absent-auto", SourceReviewedAudience(), nil)
	if err != nil || len(cards.Observations()) != 2 {
		t.Fatal("reviewed source did not load exactly two independent observations", err)
	}
	observation, ok := cards.ObservationForDeployment(SourceReviewedAudience().DeploymentID)
	if !ok || observation.VersionID != SourceReviewedAudience().ProviderRateCardVersionID || observation.SourceVersion != SourceReviewedAudience().ProviderSourceVersion {
		t.Fatal("private card/source binding mismatch")
	}
}

func TestDistinctMentionCardMatchesFrozenBytesAndRetainsObservation(t *testing.T) {
	actual, err := os.ReadFile("../../configs/provider-rates-jev-scoped.json")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("../../docs/audits/2026-10-05-mention-third-source/provider-card.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(expected) {
		t.Fatal("baked card differs from exact reviewed bytes")
	}
	var card map[string]any
	if err := json.Unmarshal(actual, &card); err != nil {
		t.Fatal(err)
	}
	if card["observedAt"] != "2026-10-04T22:51:12.367282Z" || card["effectiveAt"] != "2026-10-04T22:51:12.367282Z" {
		t.Fatal("old observation was redated")
	}
}

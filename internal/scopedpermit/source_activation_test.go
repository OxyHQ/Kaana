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
	raw, err := os.ReadFile("../../docs/audits/2026-10-05-mention-native-source-activation/audience.json")
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
	if audience == nil || audience.Validate() != nil || audience.PermitID != "jev-mention-native-en-onepost-20261005-01" || audience.FixtureSHA256 != "a962e5ed49a962db7934684c62834dd25b04aba94162ad9d07140c9a2abeb3b2" || audience.Principal.ApplicationID != "6a2f851751b784a86fd0e916" || audience.MaxCostUSD != "0.01" {
		t.Fatal("reviewed source tuple was not compiled exactly")
	}
	at := time.Date(2026, 10, 5, 1, 28, 25, 0, time.UTC)
	expires := time.Date(2026, 10, 5, 1, 28, 26, 0, time.UTC)
	if audience.ExpiresAt != "2026-10-05T01:28:26Z" || !Matches(audience, SourceReviewedAudience(), at) || Matches(nil, audience, at) || Matches(audience, nil, at) || Matches(audience, audience, expires) || Matches(audience, audience, expires.Add(time.Second)) {
		t.Fatal("absent or expired audience admitted, or exact positive refused")
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
	} {
		audience = SourceReviewedAudience()
		change()
		if Matches(audience, SourceReviewedAudience(), at) {
			t.Fatal("changed audience admitted")
		}
	}
	// Returning a fresh value keeps consumers from mutating compiled approval.
	if SourceReviewedAudience().IdempotencyKey != "mention_jev_native_en_8d04b9d17510fe89d7ae084039ee4231" {
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

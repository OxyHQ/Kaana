package scopedpermit

import (
	"testing"
	"time"
)

func TestReviewedAliaCommissioningSourceBindsExactAudienceAndExpires(t *testing.T) {
	audience := SourceReviewedAudience()
	if audience == nil || audience.Validate() != nil || audience.PermitID != "jev-internal-synthetic-20261004-01" || audience.FixtureSHA256 != "023ad0e07ce8ad36de7b6c8b3a3e6817833e606b98a25a898e471fb1537d0125" || audience.Principal.ApplicationID != "6a2f851751b784a86fd0e934" || audience.MaxCostUSD != "0.01" {
		t.Fatal("reviewed source tuple was not compiled exactly")
	}
	at := time.Date(2026, 10, 4, 20, 30, 0, 0, time.UTC)
	expires := time.Date(2026, 10, 5, 2, 30, 0, 0, time.UTC)
	if audience.ExpiresAt != "2026-10-05T02:30:00.000Z" || !Matches(audience, SourceReviewedAudience(), at) || Matches(nil, audience, at) || Matches(audience, nil, at) || Matches(audience, audience, expires) || Matches(audience, audience, expires.Add(time.Second)) {
		t.Fatal("absent or expired audience admitted, or exact positive refused")
	}
	for _, change := range []func(){
		func() { audience.Principal.ApplicationID = "foreign-Mention" },
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
	if SourceReviewedAudience().IdempotencyKey != "jev-internal-synthetic-20261004-01" {
		t.Fatal("source approval was mutable")
	}
}

func TestReviewedAliaSourceLoadsIndependentCardWithoutReDatingXai(t *testing.T) {
	cards, err := loadRateCards("../../configs/provider-rates.json", "unused", "../../configs/provider-rates-jev-scoped.json", "absent-auto", SourceReviewedAudience(), nil)
	if err != nil || len(cards.Observations()) != 2 {
		t.Fatal("reviewed source did not load exactly two independent observations", err)
	}
	observation, ok := cards.ObservationForDeployment(SourceReviewedAudience().DeploymentID)
	if !ok || observation.VersionID != SourceReviewedAudience().ProviderRateCardVersionID || observation.SourceVersion != SourceReviewedAudience().ProviderSourceVersion {
		t.Fatal("private card/source binding mismatch")
	}
}

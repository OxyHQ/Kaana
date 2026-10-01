package publisher

import (
	"encoding/json"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"testing"
	"time"
)

const publicationAudienceFixture = `{"permitId":"permit-fixture","idempotencyKey":"idem-fixture","fixtureSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expiresAt":"2099-01-01T00:00:00Z","principal":{"accountId":"account","applicationId":"app","credentialId":"credential","environment":"production"},"policy":{"routingPolicyId":"policy","policyVersion":1},"deploymentId":"dep-private","provider":"openrouter","keyId":"exact-key","modelReference":"typesafe/jev-1.13@2026-09-17","upstreamModelId":"typesafe/jev-1.13-20260917","priceVersionId":"oxy-price","providerRateCardVersionId":"provider-card","providerSourceVersion":"source","maxCostUsd":"0.01"}`

func privateFixture(t *testing.T) ([]Discovery, *privatePublicationPermit) {
	t.Helper()
	var scope contract.ScopedExecutionAudience
	if err := json.Unmarshal([]byte(publicationAudienceFixture), &scope); err != nil {
		t.Fatal(err)
	}
	cards, err := providercost.Parse([]byte(`{"schemaVersion":1,"rateCardVersionId":"provider-card","source":"provider_api","sourceVersion":"source","observedAt":"2026-10-01T00:00:00Z","effectiveAt":"2026-10-01T00:00:00Z","rateCards":[{"deploymentId":"dep-private","currency":"USD","rates":[{"unit":"input_tokens","amountPerUnit":42000},{"unit":"output_tokens","amountPerUnit":0}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	price := providercost.ListPrice{Currency: "USD", Input: "0.042", Output: "0"}
	discoveries := []Discovery{{Provider: Provider{Slug: "openrouter", CredentialKeyID: "exact-key", APIKey: "synthetic-test-key"}, Models: []DiscoveredModel{{UpstreamModelID: "typesafe/jev-1.13", CanonicalSlug: scope.UpstreamModelID, Observed: &inventory.Observed{ListPrice: &price}}}}}
	return discoveries, &privatePublicationPermit{Audience: scope, PublishedPrice: price, Cards: cards, Eligibility: NewDecider(Evidence{Keys: map[contract.ProviderSlug][]KeyEvidence{"openrouter": {{KeyID: "exact-key"}}}}, DefaultWithholdPolicy(), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))}
}
func TestPrivatePublicationRequiresNormalEvidenceAndCannotEnableGeneralJev(t *testing.T) {
	discoveries, permit := privateFixture(t)
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	allowed := func(contract.DeploymentID, contract.ProviderSlug) (Withholding, bool) { return Withholding{}, false }
	candidate, err := scopedCandidate(discoveries, permit, at, allowed, false)
	if err != nil || candidate == nil {
		t.Fatal("positive private candidate", err)
	}
	if candidate.Current || candidate.ScopedExecution == nil {
		t.Fatal("private candidate broadened")
	}
	if sourceReviewedPrivatePermit() != nil || executable(discoveries[0].Provider, permit.Audience.UpstreamModelID) {
		t.Fatal("global Jev publication enabled")
	}
	for _, tc := range []struct {
		name   string
		mutate func()
	}{{"auth", func() { discoveries[0].Provider.APIKey = "" }}, {"evidence", func() { permit.Eligibility = nil }}, {"actual-key", func() { permit.Eligibility.evidence.Keys["openrouter"] = []KeyEvidence{{KeyID: "other"}} }}, {"key", func() { discoveries[0].Provider.CredentialKeyID = "other" }}, {"canonical", func() { discoveries[0].Models[0].CanonicalSlug = "typesafe/jev-router" }}, {"price", func() { discoveries[0].Models[0].Observed.ListPrice.Input = "0.043" }}, {"unservable", func() { discoveries[0].Models[0].Unservable = "no ZDR" }}, {"card", func() { permit.Audience.ProviderRateCardVersionID = "invented" }}} {
		discoveries, permit = privateFixture(t)
		tc.mutate()
		if got, err := scopedCandidate(discoveries, permit, at, allowed, false); err == nil || got != nil {
			t.Error(tc.name, "did not refuse")
		}
	}
	discoveries, permit = privateFixture(t)
	for _, mode := range []bool{false, true} {
		if got, err := scopedCandidate(discoveries, permit, at, nil, mode); err == nil || got != nil {
			t.Fatal("missing withholding accepted")
		}
	}
	withheld := func(contract.DeploymentID, contract.ProviderSlug) (Withholding, bool) { return Withholding{}, true }
	if got, err := scopedCandidate(discoveries, permit, at, withheld, false); err != nil || got != nil {
		t.Fatal("withheld route published")
	}
}
func TestScopedSnapshotHashChangesOnlyForScopedEvidence(t *testing.T) {
	discoveries, permit := privateFixture(t)
	candidate, err := scopedCandidate(discoveries, permit, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), func(contract.DeploymentID, contract.ProviderSlug) (Withholding, bool) { return Withholding{}, false }, false)
	if err != nil {
		t.Fatal(err)
	}
	original := contentID([]snapshotDeployment{*candidate})
	candidate.ScopedExecution.PriceVersionID = "changed"
	if contentID([]snapshotDeployment{*candidate}) == original {
		t.Fatal("scoped card audience not hashed")
	}
	candidate.ScopedExecution = nil
	legacy := contentID([]snapshotDeployment{*candidate})
	candidate.Observed = nil
	if contentID([]snapshotDeployment{*candidate}) != legacy {
		t.Fatal("legacy observed fields entered hash")
	}
}

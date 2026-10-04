package publisher

import (
	"context"
	"encoding/json"
	"errors"
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
	sourcePermit, sourceErr := sourceReviewedPrivatePermit(permit.Cards, permit.Eligibility, at)
	if sourceErr != nil || sourcePermit != nil || executable(discoveries[0].Provider, permit.Audience.UpstreamModelID) {
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

func TestPrivateFactoryUsesActualCardAndSameCycleEligibility(t *testing.T) {
	discoveries, fixture := privateFixture(t)
	at := fixture.Eligibility.now
	permit, err := privatePermitForAudience(&fixture.Audience, fixture.Cards, fixture.Eligibility, at)
	if err != nil || permit == nil || permit.Cards != fixture.Cards || permit.Eligibility != fixture.Eligibility || permit.PublishedPrice != fixture.PublishedPrice {
		t.Fatalf("actual dependency positive: permit=%+v err=%v", permit, err)
	}
	built, err := buildSnapshotWithholding(discoveries, testAttribution(t), Observations{}, at, fixture.Eligibility.Decide, false, permit)
	if err != nil {
		t.Fatalf("actual snapshot construction: %v", err)
	}
	var snapshot snapshotFile
	if err := json.Unmarshal(built.Body, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Deployments) != 1 || snapshot.Deployments[0].Current || snapshot.Deployments[0].ScopedExecution == nil || !snapshot.Deployments[0].ScopedExecution.Equal(&fixture.Audience) {
		t.Fatal("private route was missing, broadened, or changed")
	}
	if _, err := BuildSnapshotWithholding(discoveries, testAttribution(t), Observations{}, at, fixture.Eligibility.Decide, false); err == nil {
		t.Fatal("public snapshot builder admitted private authority")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*privatePublicationPermit)
	}{
		{"missing-card", func(p *privatePublicationPermit) { p.Cards = nil }},
		{"missing-evidence", func(p *privatePublicationPermit) { p.Eligibility = nil }},
		{"previous-cycle", func(p *privatePublicationPermit) { p.Eligibility.now = at.Add(-time.Nanosecond) }},
		{"card-version", func(p *privatePublicationPermit) { p.Audience.ProviderRateCardVersionID = "other-card" }},
		{"source-version", func(p *privatePublicationPermit) { p.Audience.ProviderSourceVersion = "other-source" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, f := privateFixture(t)
			tc.mutate(f)
			if got, err := privatePermitForAudience(&f.Audience, f.Cards, f.Eligibility, at); err == nil || got != nil {
				t.Fatal("missing actual source dependency accepted")
			}
		})
	}
	expired := fixture.Audience
	expired.ExpiresAt = string(contract.NewTimestamp(at))
	if got, err := privatePermitForAudience(&expired, fixture.Cards, fixture.Eligibility, at); err != nil || got != nil {
		t.Fatal("expired approval did not disappear")
	}
	if got, err := privatePermitForAudience(nil, nil, nil, at); err != nil || got != nil {
		t.Fatal("inert source approval changed legacy publication")
	}
}

func TestFailedDatabaseEvidenceCannotBecomePrivateEligibility(t *testing.T) {
	discoveries, f := privateFixture(t)
	at := f.Eligibility.now
	evidence := &fakeEvidence{evidence: f.Eligibility.evidence}
	p := &Publisher{evidence: evidence, withholding: DefaultWithholdPolicy(), logger: quietLogger()}
	withhold, decider := p.withholdFor(context.Background(), discoveries, nil, false, at)
	if withhold == nil || decider == nil || !decider.now.Equal(at) {
		t.Fatal("fresh actual evidence lost its same-cycle Decider")
	}
	if got, err := privatePermitForAudience(&f.Audience, f.Cards, decider, at); err != nil || got == nil {
		t.Fatal("fresh private positive failed", err)
	}
	evidence.set(f.Eligibility.evidence, errors.New("synthetic SQL unavailable"))
	carry, missing := p.withholdFor(context.Background(), discoveries, nil, false, at)
	if carry == nil || missing != nil {
		t.Fatal("legacy fallback unexpectedly supplied private eligibility")
	}
	if got, err := privatePermitForAudience(&f.Audience, f.Cards, missing, at); err == nil || got != nil {
		t.Fatal("private publication accepted carried fallback")
	}
}

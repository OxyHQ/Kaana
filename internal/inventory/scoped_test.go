package inventory

import (
	"encoding/json"
	"github.com/OxyHQ/Kaana/internal/contract"
	"testing"
	"time"
)

const privateAudienceFixture = `{"permitId":"permit-fixture","idempotencyKey":"idem-fixture","fixtureSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expiresAt":"2099-01-01T00:00:00Z","principal":{"accountId":"account","applicationId":"app","credentialId":"credential","environment":"production"},"policy":{"routingPolicyId":"policy","policyVersion":1},"deploymentId":"dep-private","provider":"openrouter","keyId":"exact-key","modelReference":"typesafe/jev-1.13@2026-09-17","upstreamModelId":"typesafe/jev-1.13-20260917","priceVersionId":"oxy-price","providerRateCardVersionId":"provider-card","providerSourceVersion":"source","maxCostUsd":"0.01"}`

func TestScopedRoutesAreNeverPublicAndMatchAllAudienceFields(t *testing.T) {
	var audience contract.ScopedExecutionAudience
	if err := json.Unmarshal([]byte(privateAudienceFixture), &audience); err != nil {
		t.Fatal(err)
	}
	deployment := Deployment{ScopedExecution: &audience, DeploymentID: audience.DeploymentID, Provider: audience.Provider, ModelReference: audience.ModelReference, UpstreamModelID: audience.UpstreamModelID}
	raw, err := json.Marshal(map[string]any{"snapshotId": "private", "issuedAt": "2026-10-02T00:00:00Z", "deployments": []Deployment{deployment}})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := Parse(raw, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	if _, err := inv.Resolve(audience.ModelReference, at); err == nil {
		t.Fatal("private route publicly resolved")
	}
	if _, err := inv.Deployment(audience.DeploymentID); err == nil {
		t.Fatal("private deployment publicly resolved")
	}
	if len(inv.Catalogue()) != 0 || len(inv.DeploymentDescriptors()) != 0 || len(inv.PublicDeployments()) != 0 || len(inv.PinnedOnlyReferences()) != 0 {
		t.Fatal("private projection leaked")
	}
	if len(inv.CatalogueScoped(at)) != 1 || len(inv.DeploymentDescriptorsScoped()) != 1 {
		t.Fatal("positive scoped projection missing")
	}
	route, err := inv.DeploymentScoped(audience.DeploymentID, at, &audience)
	if err != nil || !route.ScopedExecution.Equal(&audience) {
		t.Fatal("exact audience missing", err)
	}
	route.ScopedExecution.KeyID = "mutated"
	if _, err := inv.DeploymentScoped(audience.DeploymentID, at, &audience); err != nil {
		t.Fatal("returned route aliases inventory", err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(privateAudienceFixture), &fields); err != nil {
		t.Fatal(err)
	}
	for field := range fields {
		changed := audience
		switch field {
		case "principal":
			changed.Principal.AccountID = "other"
		case "policy":
			changed.Policy.PolicyVersion++
		default:
			mutated := map[string]any{}
			for k, v := range fields {
				mutated[k] = v
			}
			mutated[field] = "other"
			data, _ := json.Marshal(mutated)
			if json.Unmarshal(data, &changed) != nil {
				continue
			}
		}
		if _, err := inv.ResolveScoped(audience.ModelReference, at, &changed); err == nil {
			t.Errorf("wrong %s audience admitted", field)
		}
	}
	if _, err := inv.ResolveScoped(audience.ModelReference, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), &audience); err == nil {
		t.Fatal("expiry boundary admitted")
	}
	deployment.Current = true
	raw, _ = json.Marshal(map[string]any{"snapshotId": "private", "issuedAt": "2026-10-02T00:00:00Z", "deployments": []Deployment{deployment}})
	if _, err := Parse(raw, time.Hour); err == nil {
		t.Fatal("scoped deployment marked current")
	}
}

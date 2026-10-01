package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func scopedAudienceFixture() string {
	return `{"permitId":"permit-fixture","idempotencyKey":"idem-fixture","fixtureSha256":"` + strings.Repeat("a", 64) + `","expiresAt":"2099-01-01T00:00:00Z","principal":{"accountId":"account","applicationId":"app","credentialId":"credential","environment":"production"},"policy":{"routingPolicyId":"policy","policyVersion":1},"deploymentId":"dep-fixture","provider":"openrouter","keyId":"exact-key","modelReference":"typesafe/jev-1.13@2026-09-17","upstreamModelId":"typesafe/jev-1.13-20260917","priceVersionId":"oxy-price","providerRateCardVersionId":"provider-card","providerSourceVersion":"source","maxCostUsd":"0.01"}`
}

func TestScopedAudienceStrictParityAndCeiling(t *testing.T) {
	raw := scopedAudienceFixture()
	var positive ScopedExecutionAudience
	if err := json.Unmarshal([]byte(raw), &positive); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(raw, `"permitId":"permit-fixture"`, `"permitId":"permit-fixture","permitId":"other"`, 1),
		strings.Replace(raw, `"permitId":`, `"PermitID":`, 1),
		strings.Replace(raw, `"principal":{`, `"principal":{"extra":true,`, 1),
		strings.Replace(raw, `"credentialId":"credential"`, `"credentialId":null`, 1),
		strings.Replace(raw, `"maxCostUsd":"0.01"`, `"maxCostUsd":"0.010000000001"`, 1),
		strings.Replace(raw, `"maxCostUsd":"0.01"`, `"maxCostUsd":"0"`, 1),
		strings.Replace(raw, `"maxCostUsd":"0.01"`, `"maxCostUsd":0.001`, 1),
		strings.Replace(raw, `"production"`, `"staging"`, 1),
	} {
		var audience ScopedExecutionAudience
		if err := json.Unmarshal([]byte(bad), &audience); err == nil {
			t.Fatal("ambiguous/invalid audience accepted", bad)
		}
	}
}
func TestScopedExecutionRetainsEnvelopeFieldsAndExactRawFixture(t *testing.T) {
	input := `{"decisions":{"state":"<synthetic>"},"format":"decisions"}`
	digest := sha256.Sum256([]byte(input))
	raw := strings.TrimSuffix(scopedAudienceFixture(), "}") + `,"requestId":"actual-trace","snapshotId":"snap-fixture","catalogueEvidenceHash":"` + strings.Repeat("b", 64) + `"}`
	var scope ScopedExecution
	if err := json.Unmarshal([]byte(raw), &scope); err != nil {
		t.Fatal(err)
	}
	if scope.RequestID != "actual-trace" || scope.PermitID != "permit-fixture" || scope.SnapshotID != "snap-fixture" {
		t.Fatal("envelope fields lost")
	}
	scope.FixtureSHA256 = hex.EncodeToString(digest[:])
	request := Request{ScopedExecution: &scope}
	if err := request.ValidateScopedInputBytes([]byte(`{"input":` + input + `}`)); err != nil {
		t.Fatal(err)
	}
	if err := request.ValidateScopedInputBytes([]byte(`{"input":` + strings.Replace(input, "<", `\u003c`, 1) + `}`)); err == nil {
		t.Fatal("raw byte change normalized away")
	}
	for _, version := range []int{1, 2} {
		request.SchemaVersion = version
		if request.validateScopedExecution() == nil {
			t.Fatal("legacy envelope accepted restriction")
		}
	}
	if (&Request{SchemaVersion: ScopedRequestEnvelopeVersion}).validateScopedExecution() == nil {
		t.Fatal("v3 without restriction accepted")
	}
}

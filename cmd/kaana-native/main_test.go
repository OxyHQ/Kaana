package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
)

func pointer[T any](v T) *T { return &v }
func TestValidateUsesReviewedArtifactsWithoutOpeningDatabase(t *testing.T) {
	p := nativeadmission.Policy{EconomicTreatment: nativeadmission.InternalMetered, ID: "cli-reviewed", Version: 1, Enabled: true, CredentialIDs: []contract.CredentialID{"workload-handle-test"}, OwnerAccountID: "owner-test", ApplicationID: "application-test", Environment: contract.EnvironmentProduction, Lane: contract.IdentityLaneService, Authority: contract.AuthorityFirstParty, ApplicationClass: "internal", ApplicationIsInternal: true, Audience: contract.AudiencePlatformInternal, AllowedModels: []contract.ModelID{"fixture/model"}, Routes: nativeadmission.RouteScope{Mode: "exact", Deployments: []nativeadmission.RoutePermission{{DeploymentID: "fixture-route", ModelReference: "fixture/model@v1", Provider: "fixture"}}}, Limits: contract.GrantStepLimits{MaxPricePerRequest: &contract.Money{Amount: "0.05", Currency: "USD"}, MaxFundingTier: pointer(contract.FundingTierCredit)}, MaxInputTokens: 126976, MaxOutputTokens: 8192, DefaultOutputTokens: 8192, MaxTools: 128, MaxConcurrent: 32, MaxPerUTCDay: 5000, ExecutionTimeout: time.Minute}
	policyHash, err := nativeadmission.PolicyDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(nativeadmission.PolicyFile{SchemaVersion: 1, Policies: []nativeadmission.Policy{p}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(t.TempDir(), "reviewed.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile("../../internal/nativeadmission/testdata/legacy-interop.json")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"validate", "--policy-file", path, "--policy-file-sha256", hash, "--policy-id", p.ID, "--policy-version", "1", "--policy-sha256", policyHash, "--manifest-sha256", "7a85bf15e61fa6e9ad56a1326053f2c8bd8e4a9b5edd99478d7594eb3c81cf3d", "--fence-id", "01900000-0000-7000-8000-000000000001", "--activated-at", "2026-11-13T00:00:00.000Z"}
	var out bytes.Buffer
	if err = run(args, bytes.NewReader(manifest), &out, func(string) string { t.Fatal("validation accessed environment/database"); return "" }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"valid":true`)) {
		t.Fatal("validation proof absent")
	}
	args[len(args)-1] = "2026-11-12T00:00:00.000Z"
	if err = run(args, bytes.NewReader(manifest), io.Discard, func(string) string { t.Fatal("invalid review accessed database"); return "" }); err == nil {
		t.Fatal("changed fence accepted")
	}
}

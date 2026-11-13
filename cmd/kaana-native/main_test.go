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
func validateFixture(t *testing.T) ([]string, []byte) {
	t.Helper()
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
	args := []string{"validate", "--policy-file", path, "--policy-file-sha256", hash, "--policy-id", p.ID, "--policy-version", "1", "--policy-sha256", policyHash, "--manifest-sha256", "16b2a1a47aa61785150b6e3fce72e8e10aa306c5f7fca842f4b4d9fca65fe5c8", "--fence-id", "01900000-0000-7000-8000-000000000001", "--activated-at", "2026-11-13T00:00:00.000Z"}
	return args, manifest
}
func TestValidateUsesReviewedArtifactsWithoutOpeningDatabase(t *testing.T) {
	args, manifest := validateFixture(t)
	var err error
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

type forbiddenManifestStdin struct{}

func (forbiddenManifestStdin) Read([]byte) (int, error) { panic("file mode read stdin") }
func TestValidateManifestFileBeforeAnyDatabaseAccess(t *testing.T) {
	args, manifest := validateFixture(t)
	path := filepath.Join(t.TempDir(), "manifest.json")
	// The same reviewed canonical manifest can be larger than the ECS override
	// limit; only its local path travels in argv, never its payload.
	expanded := append(append([]byte{}, manifest...), bytes.Repeat([]byte(" "), 16384)...)
	if err := os.WriteFile(path, expanded, 0600); err != nil {
		t.Fatal(err)
	}
	fileArgs := append(append([]string{}, args...), "--manifest-file", path)
	var output bytes.Buffer
	if err := run(fileArgs, forbiddenManifestStdin{}, &output, func(string) string { t.Fatal("validation opened DB or environment"); return "" }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"valid":true`)) {
		t.Fatal("validation evidence missing")
	}
	if bytes.Contains(output.Bytes(), []byte("caller:")) {
		t.Fatal("manifest payload leaked")
	}
	for _, operation := range []string{"validate", "import"} {
		bad := append([]string{}, fileArgs...)
		bad[0] = operation
		if err := os.WriteFile(path, []byte(`{"sha256":"unreviewed"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(bad, forbiddenManifestStdin{}, io.Discard, func(string) string { t.Fatal("invalid artifact reached DB"); return "" }); err == nil {
			t.Fatal("changed manifest accepted")
		}
	}
}
func TestManifestFileRejectsMissingDirectoryAndOversize(t *testing.T) {
	dir := t.TempDir()
	oversized := filepath.Join(dir, "large.json")
	file, err := os.OpenFile(oversized, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(64*1024*1024 + 1); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(dir, "public.json")
	if err := os.WriteFile(public, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(public, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing"), dir, oversized, public, link} {
		if _, err := readManifestArtifact(path, forbiddenManifestStdin{}); err == nil {
			t.Fatal("unbounded/nonregular artifact accepted")
		}
	}
	if err := run([]string{"migrate", "--manifest-file", oversized}, forbiddenManifestStdin{}, io.Discard, func(string) string { t.Fatal("invalid flags reached environment"); return "" }); err == nil {
		t.Fatal("manifest silently ignored")
	}
}

func TestInspectReusesPolicyReviewAndRejectsArbitraryInputs(t *testing.T) {
	args, _ := validateFixture(t)
	args = append([]string{"inspect"}, args[1:11]...)
	var output bytes.Buffer
	if err := run(args, forbiddenManifestStdin{}, &output, func(key string) string {
		if key != "DATABASE_URL" {
			t.Fatal("unexpected environment input")
		}
		return "private malformed database value"
	}); err == nil || output.Len() != 0 || bytes.Contains([]byte(err.Error()), []byte("private malformed")) {
		t.Fatal("invalid DSN accepted or exposed")
	}
	for _, extra := range [][]string{{"--sql", "select 1"}, {"--manifest-file", "private.json"}, {"--policy-sha256", "changed"}} {
		altered := append(append([]string{}, args...), extra...)
		if err := run(altered, forbiddenManifestStdin{}, io.Discard, func(string) string { t.Fatal("invalid review reached database"); return "" }); err == nil {
			t.Fatal("invalid inspection arguments accepted")
		}
	}
}

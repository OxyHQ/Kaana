package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const (
	previousProductionManifest = "../../configs/cutovers/production-bindings-snap_dfd6904a99d6313b.json"
	cutoverProductionManifest  = "../../configs/cutovers/production-bindings-snap_ebf19b144959bbb8.json"
	currentProductionManifest  = "../../configs/cutovers/production-bindings-snap_37548e4f1f8ec610.json"
)

var reviewedPrimaryKeys = map[string]string{
	"cerebras":   "43405cea-a7d1-49c2-ba73-5a84536d3abf",
	"groq":       "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1",
	"openrouter": "b8090dce-82f2-4077-9fc1-fd831a53ca27",
	"xai":        "1d72d527-81ca-41e5-9644-2d81a4b126ec",
}

func requireReviewedKeys(t *testing.T, rows []bindingManifestAssignment) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, row := range rows {
		key, ok := reviewedPrimaryKeys[string(row.Provider)]
		if !ok || row.KeyID != key {
			t.Fatalf("unreviewed provider/key assignment: %+v", row)
		}
		counts[string(row.Provider)]++
	}
	return counts
}

func TestReviewedProductionBindingManifestIsCompleteAndExact(t *testing.T) {
	manifest, err := readBindingManifest(previousProductionManifest)
	if err != nil {
		t.Fatalf("reviewed manifest: %v", err)
	}
	if manifest.Inventory.SnapshotID != "snap_dfd6904a99d6313b" || manifest.Inventory.ContentSHA256 != "18ef56a8240a4dd749759cd8783ba402464e7f0402412252acdae6cfa496df17" || len(manifest.Assignments) != 340 || len(manifest.RetainedBindings) != 0 {
		t.Fatalf("production provenance/count drifted: %+v", manifest.Inventory)
	}
	counts := requireReviewedKeys(t, manifest.Assignments)
	if counts["cerebras"] != 2 || counts["groq"] != 7 || counts["openrouter"] != 324 || counts["xai"] != 7 {
		t.Fatalf("provider cardinality drifted: %v", counts)
	}
}

// A successor manifest must describe the database its predecessor produced:
// every deployment still published keeps the exact row (and operation ID) it
// already has, each applied row the publisher withdrew is retained verbatim, and
// only a genuinely new deployment takes a new, unused operation ID. It returns
// the new assignments.
func requireSuccessorManifest(t *testing.T, previousPath, currentPath string) []bindingManifestAssignment {
	t.Helper()
	previous, err := readBindingManifest(previousPath)
	if err != nil {
		t.Fatalf("previous manifest: %v", err)
	}
	current, err := readBindingManifest(currentPath)
	if err != nil {
		t.Fatalf("current manifest: %v", err)
	}
	requireReviewedKeys(t, current.RetainedBindings)

	appliedRows := append(append([]bindingManifestAssignment{}, previous.Assignments...), previous.RetainedBindings...)
	applied := make(map[contract.DeploymentID]bindingManifestAssignment, len(appliedRows))
	usedOperations := make(map[string]struct{}, len(appliedRows))
	for _, row := range appliedRows {
		applied[row.DeploymentID] = row
		usedOperations[row.OperationID] = struct{}{}
	}
	var fresh []bindingManifestAssignment
	for _, row := range current.Assignments {
		if old, found := applied[row.DeploymentID]; found {
			if old != row {
				t.Fatalf("deployment %q changed its applied row: %+v -> %+v", row.DeploymentID, old, row)
			}
			delete(applied, row.DeploymentID)
			continue
		}
		if _, reused := usedOperations[row.OperationID]; reused {
			t.Fatalf("new deployment %q reuses applied operation %q", row.DeploymentID, row.OperationID)
		}
		fresh = append(fresh, row)
	}
	if len(applied) != len(current.RetainedBindings) {
		t.Fatalf("%d applied rows are neither assigned nor retained", len(applied)-len(current.RetainedBindings))
	}
	for _, row := range current.RetainedBindings {
		if applied[row.DeploymentID] != row {
			t.Fatalf("retained binding is not the exact applied row: %+v", row)
		}
	}
	return fresh
}

// snap_ebf19b144959bbb8 is the manifest the schema 0013 cutover applied and
// verified on 2026-09-24. It stays as the record of the row it created.
func TestCutoverProductionBindingManifestExtendsTheFirstOne(t *testing.T) {
	current, err := readBindingManifest(cutoverProductionManifest)
	if err != nil {
		t.Fatalf("cutover manifest: %v", err)
	}
	if current.Inventory.SnapshotID != "snap_ebf19b144959bbb8" || current.Inventory.S3VersionID != "hZsP6lsuSFMehoKgem9HF6NHZr43_0bd" || current.Inventory.ContentSHA256 != "49534e10561d66eb651afb39860762e7c25abbe6064232d11532eef61361d017" || len(current.Assignments) != 333 || len(current.RetainedBindings) != 8 {
		t.Fatalf("production provenance/count drifted: %+v", current.Inventory)
	}
	counts := requireReviewedKeys(t, current.Assignments)
	if counts["cerebras"] != 1 || counts["groq"] != 6 || counts["openrouter"] != 319 || counts["xai"] != 7 {
		t.Fatalf("provider cardinality drifted: %v", counts)
	}
	fresh := requireSuccessorManifest(t, previousProductionManifest, cutoverProductionManifest)
	if len(fresh) != 1 || fresh[0].DeploymentID != "dep_openrouter_z_ai_glm_5_2_free_observed_2026_09_01" || fresh[0].OperationID != "kdb_00000000000000000000000000000341" {
		t.Fatalf("unexpected new assignments: %+v", fresh)
	}
}

// snap_37548e4f1f8ec610 is the first snapshot the speech-discovering publisher
// issued. Its only new deployment is xAI text-to-speech, bound to xAI's single
// enabled key under the first unused operation ID.
func TestCurrentProductionBindingManifestExtendsTheCutoverOne(t *testing.T) {
	current, err := readBindingManifest(currentProductionManifest)
	if err != nil {
		t.Fatalf("current manifest: %v", err)
	}
	if current.Inventory.SnapshotID != "snap_37548e4f1f8ec610" || current.Inventory.S3VersionID != "bB84mMzvBNUKtjKdmurcgBn5vIUCBNPJ" || current.Inventory.ContentSHA256 != "e73ea428e95d0957e4773d0e89629450848289688aa0e31ef3474ea06e667c02" || len(current.Assignments) != 334 || len(current.RetainedBindings) != 8 {
		t.Fatalf("production provenance/count drifted: %+v", current.Inventory)
	}
	counts := requireReviewedKeys(t, current.Assignments)
	if counts["cerebras"] != 1 || counts["groq"] != 6 || counts["openrouter"] != 319 || counts["xai"] != 8 {
		t.Fatalf("provider cardinality drifted: %v", counts)
	}
	fresh := requireSuccessorManifest(t, cutoverProductionManifest, currentProductionManifest)
	if len(fresh) != 1 || fresh[0].DeploymentID != "dep_xai_tts_observed_2026_09_24" || fresh[0].OperationID != "kdb_00000000000000000000000000000342" {
		t.Fatalf("unexpected new assignments: %+v", fresh)
	}
}

func TestBindingManifestRefusesARetainedRowThatIsAlsoAssigned(t *testing.T) {
	document, err := os.ReadFile(currentProductionManifest)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(document, &raw); err != nil {
		t.Fatal(err)
	}
	var assignments []bindingManifestAssignment
	if err := json.Unmarshal(raw["assignments"], &assignments); err != nil {
		t.Fatal(err)
	}
	overlap := assignments[0]
	overlap.OperationID = "kdb_ffffffffffffffffffffffffffffffff"
	raw["retainedBindings"], _ = json.Marshal([]bindingManifestAssignment{overlap})
	mutated, _ := json.Marshal(raw)
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBindingManifest(path); err == nil || !strings.Contains(err.Error(), "both assigns and retains") {
		t.Fatalf("overlapping retained binding accepted: %v", err)
	}
}

type fakeBindingRepository struct {
	rows  map[contract.DeploymentID]credentialstore.DeploymentBindingMetadata
	calls []string
}

func (f *fakeBindingRepository) BindDeployment(_ context.Context, operationID string, binding provider.CredentialBinding, actor string) (string, error) {
	f.calls = append(f.calls, operationID)
	if existing, found := f.rows[binding.DeploymentID]; found && existing.OperationID == operationID {
		// The database's replay check compares the actor too, and a new
		// workflow run is always a new actor.
		if existing.OperationActor != actor {
			return "conflict", credentialstore.ErrDeploymentBindingConflict
		}
		return "replayed", nil
	}
	f.rows[binding.DeploymentID] = credentialstore.DeploymentBindingMetadata{DeploymentID: binding.DeploymentID, Provider: binding.Provider, KeyID: binding.KeyID, OperationID: operationID, OperationActor: actor}
	return "applied", nil
}

func (f *fakeBindingRepository) ListDeploymentBindings(context.Context) ([]credentialstore.DeploymentBindingMetadata, error) {
	result := make([]credentialstore.DeploymentBindingMetadata, 0, len(f.rows))
	for _, row := range f.rows {
		result = append(result, row)
	}
	return result, nil
}

func row(deployment, operation, actor string) credentialstore.DeploymentBindingMetadata {
	return credentialstore.DeploymentBindingMetadata{DeploymentID: contract.DeploymentID(deployment), Provider: "xai", KeyID: "1d72d527-81ca-41e5-9644-2d81a4b126ec", OperationID: operation, OperationActor: actor}
}

func assignment(deployment, operation string) bindingManifestAssignment {
	return bindingManifestAssignment{DeploymentID: contract.DeploymentID(deployment), Provider: "xai", KeyID: "1d72d527-81ca-41e5-9644-2d81a4b126ec", OperationID: operation}
}

func TestApplyBindingManifestBindsOnlyWhatIsNotAlreadyExact(t *testing.T) {
	const kept, added, withdrawn = "kdb_00000000000000000000000000000001", "kdb_00000000000000000000000000000003", "kdb_00000000000000000000000000000002"
	repository := &fakeBindingRepository{rows: map[contract.DeploymentID]credentialstore.DeploymentBindingMetadata{
		"dep_kept":      row("dep_kept", kept, "github-actions:OxyHQ/Kaana:1"),
		"dep_withdrawn": row("dep_withdrawn", withdrawn, "github-actions:OxyHQ/Kaana:1"),
	}}
	manifest := bindingManifest{
		Assignments:      []bindingManifestAssignment{assignment("dep_kept", kept), assignment("dep_added", added)},
		RetainedBindings: []bindingManifestAssignment{assignment("dep_withdrawn", withdrawn)},
	}
	// A second run, as a different actor, must converge instead of conflicting.
	for _, actor := range []string{"github-actions:OxyHQ/Kaana:2", "github-actions:OxyHQ/Kaana:3"} {
		if err := applyBindingManifest(context.Background(), repository, manifest, actor); err != nil {
			t.Fatalf("apply as %s: %v", actor, err)
		}
	}
	if len(repository.calls) != 1 || repository.calls[0] != added {
		t.Fatalf("apply mutated more than the one missing row: %v", repository.calls)
	}
	readback, _ := repository.ListDeploymentBindings(context.Background())
	if err := verifyBindingManifest(readback, manifest); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Positive control: the comparison is still exact in both directions.
	unlisted := manifest
	unlisted.RetainedBindings = nil
	if err := verifyBindingManifest(readback, unlisted); err == nil {
		t.Fatal("verify accepted a readback row the manifest does not name")
	}
	repository.rows["dep_withdrawn"] = row("dep_withdrawn", "kdb_00000000000000000000000000000009", "x")
	readback, _ = repository.ListDeploymentBindings(context.Background())
	if err := verifyBindingManifest(readback, manifest); err == nil {
		t.Fatal("verify accepted a retained row with a different operation")
	}
}

func TestApplyBindingManifestRebindsADifferentKey(t *testing.T) {
	repository := &fakeBindingRepository{rows: map[contract.DeploymentID]credentialstore.DeploymentBindingMetadata{
		"dep_a": {DeploymentID: "dep_a", Provider: "xai", KeyID: "ad05516d-e2d2-4be4-8735-5e69c9bff41c", OperationID: "kdb_00000000000000000000000000000001", OperationActor: "a"},
	}}
	manifest := bindingManifest{Assignments: []bindingManifestAssignment{assignment("dep_a", "kdb_00000000000000000000000000000001")}}
	err := applyBindingManifest(context.Background(), repository, manifest, "b")
	if !errors.Is(err, credentialstore.ErrDeploymentBindingConflict) {
		t.Fatalf("a changed key under a reused operation must reach the database's conflict check, got %v", err)
	}
}

// Every place that names the production manifest must name the same file:
// the image bakes it, the build context admits it, the workflow proves its
// inventory provenance, and the operations mount it for apply and verify.
func TestProductionManifestIsNamedConsistently(t *testing.T) {
	const name = "production-bindings-snap_37548e4f1f8ec610.json"
	for path, want := range map[string]string{
		"../../Dockerfile":                               "cp configs/cutovers/" + name + " /out/etc/kaana-cutovers/",
		"../../.dockerignore":                            "!configs/cutovers/" + name + "\n",
		"../../.github/workflows/credential-admin.yml":   "manifest=configs/cutovers/" + name + "\n",
		"../../.github/credential-admin-operations.json": `"/etc/kaana-cutovers/` + name + `"`,
	} {
		document, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(document), want) {
			t.Errorf("%s does not name the current production manifest", path)
		}
		if strings.Count(string(document), "production-bindings-snap_") != strings.Count(string(document), name) {
			t.Errorf("%s names another production manifest", path)
		}
	}
}

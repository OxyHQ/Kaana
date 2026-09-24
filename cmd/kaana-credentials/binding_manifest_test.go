package main

import (
	"context"
	"errors"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const (
	previousProductionManifest = "../../configs/cutovers/production-bindings-snap_dfd6904a99d6313b.json"
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

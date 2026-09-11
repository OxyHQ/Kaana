package main

import (
	"os"
	"testing"
)

func TestReviewedProductionBindingManifestIsCompleteAndExact(t *testing.T) {
	const path = "../../configs/cutovers/production-bindings-snap_dfd6904a99d6313b.json"
	manifest, err := readBindingManifest(path)
	if err != nil {
		t.Fatalf("reviewed manifest: %v", err)
	}
	if manifest.Inventory.SnapshotID != "snap_dfd6904a99d6313b" || manifest.Inventory.ContentSHA256 != "18ef56a8240a4dd749759cd8783ba402464e7f0402412252acdae6cfa496df17" || len(manifest.Assignments) != 340 {
		t.Fatalf("production provenance/count drifted: %+v", manifest.Inventory)
	}
	wantKeys := map[string]string{
		"cerebras":   "43405cea-a7d1-49c2-ba73-5a84536d3abf",
		"groq":       "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1",
		"openrouter": "b8090dce-82f2-4077-9fc1-fd831a53ca27",
		"xai":        "1d72d527-81ca-41e5-9644-2d81a4b126ec",
	}
	counts := map[string]int{}
	for _, assignment := range manifest.Assignments {
		key, ok := wantKeys[string(assignment.Provider)]
		if !ok || assignment.KeyID != key {
			t.Fatalf("unreviewed provider/key assignment: %+v", assignment)
		}
		counts[string(assignment.Provider)]++
	}
	if counts["cerebras"] != 2 || counts["groq"] != 7 || counts["openrouter"] != 324 || counts["xai"] != 7 {
		t.Fatalf("provider cardinality drifted: %v", counts)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

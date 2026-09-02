package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

func TestExactDeploymentDescriptorSelectionRejectsAbsenceAndAmbiguity(t *testing.T) {
	descriptors := []inventory.DeploymentDescriptor{
		{
			DeploymentID:   "dep_exact",
			ModelReference: "publisher/model@revision-a",
			Provider:       "provider-a",
			Regions:        []contract.Region{},
		},
		{
			DeploymentID:   "dep_exact",
			ModelReference: "publisher/model@revision-b",
			Provider:       "provider-b",
			Regions:        []contract.Region{"region-b"},
		},
	}

	if _, err := selectExactDeploymentDescriptor(descriptors, "dep_missing"); !errors.Is(err, errDeploymentDescriptorNotFound) {
		t.Fatalf("an absent exact id was refused with %v", err)
	}
	if _, err := selectExactDeploymentDescriptor(descriptors, "dep_exac"); !errors.Is(err, errDeploymentDescriptorNotFound) {
		t.Fatalf("a prefix was treated as an exact identity: %v", err)
	}
	if _, err := selectExactDeploymentDescriptor(descriptors, "dep_exact"); !errors.Is(err, errDeploymentDescriptorAmbiguous) {
		t.Fatalf("a duplicate exact id was refused with %v", err)
	}
	if err := validateUniqueDeploymentDescriptors(descriptors); !errors.Is(err, errDeploymentDescriptorAmbiguous) {
		t.Fatalf("the list accepted a duplicate exact id: %v", err)
	}
}

func TestExactDeploymentDescriptorSelectionReturnsOneUnchangedIdentity(t *testing.T) {
	want := inventory.DeploymentDescriptor{
		DeploymentID:   "dep_exact",
		ModelReference: "publisher/model@revision-a",
		Provider:       "provider-a",
		Regions:        []contract.Region{},
	}

	selected, err := selectExactDeploymentDescriptor([]inventory.DeploymentDescriptor{want}, "dep_exact")
	if err != nil {
		t.Fatalf("the exact id was refused: %v", err)
	}
	if len(selected) != 1 {
		t.Fatalf("the exact id selected %d descriptors", len(selected))
	}
	got := selected[0]
	if got.DeploymentID != want.DeploymentID || got.ModelReference != want.ModelReference || got.Provider != want.Provider {
		t.Fatalf("the exact lookup changed identity: got %+v want %+v", got, want)
	}
	if got.Regions == nil || len(got.Regions) != 0 {
		t.Fatalf("the exact lookup changed explicit empty regions to %#v", got.Regions)
	}
	if err := validateUniqueDeploymentDescriptors([]inventory.DeploymentDescriptor{want}); err != nil {
		t.Fatalf("a unique descriptor list was refused: %v", err)
	}
}

func TestExactDeploymentDescriptorBatchRejectsPartialAndKeepsOnlyExactIDs(t *testing.T) {
	descriptors := []inventory.DeploymentDescriptor{
		{DeploymentID: "dep_a", ModelReference: "publisher/a@revision", Provider: "provider-a", Regions: []contract.Region{}},
		{DeploymentID: "dep_extra", ModelReference: "publisher/extra@revision", Provider: "provider-b", Regions: []contract.Region{"region-extra"}},
		{DeploymentID: "dep_z", ModelReference: "publisher/z@revision", Provider: "provider-c", Regions: []contract.Region{"region-z"}},
	}

	selected, err := selectExactDeploymentDescriptors(descriptors, []contract.DeploymentID{"dep_z", "dep_a"})
	if err != nil {
		t.Fatalf("the exact batch was refused: %v", err)
	}
	if len(selected) != 2 || selected[0].DeploymentID != "dep_a" || selected[1].DeploymentID != "dep_z" {
		t.Fatalf("the batch returned an extra, omitted an exact id, or changed stable order: %+v", selected)
	}
	if selected[0].Regions == nil || len(selected[0].Regions) != 0 {
		t.Fatalf("the batch changed an unattested region set to %#v", selected[0].Regions)
	}
	if _, err := selectExactDeploymentDescriptors(
		descriptors, []contract.DeploymentID{"dep_a", "dep_missing"},
	); !errors.Is(err, errDeploymentDescriptorNotFound) {
		t.Fatalf("a partial batch was refused with %v", err)
	}
	if _, err := selectExactDeploymentDescriptors(
		descriptors, []contract.DeploymentID{"dep_a", "dep_a"},
	); !errors.Is(err, errDeploymentDescriptorAmbiguous) {
		t.Fatalf("a duplicate requested identity was refused with %v", err)
	}
}

func TestDeploymentDescriptorBatchParserEnforcesBound(t *testing.T) {
	ids := make([]string, MaxDeploymentDescriptorQueryIDs+1)
	for index := range ids {
		ids[index] = fmt.Sprintf("dep_%03d", index)
	}
	body, err := json.Marshal(map[string]any{"deploymentIds": ids})
	if err != nil {
		t.Fatalf("encoding the oversized query: %v", err)
	}
	if _, err := parseDeploymentDescriptorQuery(body); err == nil {
		t.Fatalf("the parser accepted %d ids, above the %d-id bound", len(ids), MaxDeploymentDescriptorQueryIDs)
	}
}

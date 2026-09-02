package httpapi

import (
	"errors"
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

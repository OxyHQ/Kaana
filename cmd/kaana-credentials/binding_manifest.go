package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/provider"
)

type bindingManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Authority     string `json:"authority"`
	Inventory     struct {
		Bucket          string `json:"bucket"`
		Key             string `json:"key"`
		SnapshotID      string `json:"snapshotId"`
		IssuedAt        string `json:"issuedAt"`
		S3VersionID     string `json:"s3VersionId"`
		ETag            string `json:"etag"`
		ContentSHA256   string `json:"contentSha256"`
		DeploymentCount int    `json:"deploymentCount"`
	} `json:"inventory"`
	CredentialReadback struct {
		TaskARN                    string         `json:"taskArn"`
		LogStream                  string         `json:"logStream"`
		ObservedEnabledCardinality map[string]int `json:"observedEnabledCardinality"`
	} `json:"credentialReadback"`
	Assignments []bindingManifestAssignment `json:"assignments"`
	// RetainedBindings are exact rows the database keeps for deployments that
	// are no longer in the reviewed inventory. There is no unbind mutation, so a
	// deployment the publisher withdrew keeps its audited binding row forever.
	// Such a row is inert: serving checks only the deployments its mounted
	// inventory names. Listing each one here keeps the readback comparison
	// complete and exact instead of tolerating any row the manifest omits.
	RetainedBindings []bindingManifestAssignment `json:"retainedBindings,omitempty"`
}

type bindingManifestAssignment struct {
	OperationID  string                `json:"operationId"`
	DeploymentID contract.DeploymentID `json:"deploymentId"`
	Provider     contract.ProviderSlug `json:"provider"`
	KeyID        string                `json:"keyId"`
}

func readBindingManifest(path string) (bindingManifest, error) {
	var manifest bindingManifest
	if path == "" {
		return manifest, errors.New("binding manifest path is required")
	}
	document, err := os.ReadFile(path)
	if err != nil {
		return manifest, fmt.Errorf("reading binding manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decoding binding manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return manifest, errors.New("binding manifest contains trailing JSON")
	}
	if manifest.SchemaVersion != 1 || manifest.Authority != "reviewed-exact-kaana-deployment-credential-assignment" || manifest.Inventory.Bucket == "" || manifest.Inventory.Key == "" || manifest.Inventory.SnapshotID == "" || manifest.Inventory.S3VersionID == "" || len(manifest.Inventory.ContentSHA256) != 64 || manifest.Inventory.DeploymentCount != len(manifest.Assignments) || len(manifest.Assignments) == 0 {
		return manifest, errors.New("binding manifest identity or count is invalid")
	}
	deployments := make(map[contract.DeploymentID]struct{}, len(manifest.Assignments))
	operations := make(map[string]struct{}, len(manifest.Assignments))
	for _, assignment := range manifest.Assignments {
		if assignment.DeploymentID == "" || !assignment.Provider.Valid() || assignment.KeyID == "" || len(assignment.OperationID) != 36 || assignment.OperationID[:4] != "kdb_" {
			return manifest, errors.New("binding manifest contains an invalid assignment")
		}
		if _, duplicate := deployments[assignment.DeploymentID]; duplicate {
			return manifest, fmt.Errorf("binding manifest repeats deployment %q", assignment.DeploymentID)
		}
		if _, duplicate := operations[assignment.OperationID]; duplicate {
			return manifest, fmt.Errorf("binding manifest repeats operation %q", assignment.OperationID)
		}
		deployments[assignment.DeploymentID] = struct{}{}
		operations[assignment.OperationID] = struct{}{}
	}
	for _, retained := range manifest.RetainedBindings {
		if retained.DeploymentID == "" || !retained.Provider.Valid() || retained.KeyID == "" || len(retained.OperationID) != 36 || retained.OperationID[:4] != "kdb_" {
			return manifest, errors.New("binding manifest contains an invalid retained binding")
		}
		if _, duplicate := deployments[retained.DeploymentID]; duplicate {
			return manifest, fmt.Errorf("binding manifest both assigns and retains deployment %q", retained.DeploymentID)
		}
		if _, duplicate := operations[retained.OperationID]; duplicate {
			return manifest, fmt.Errorf("binding manifest repeats operation %q", retained.OperationID)
		}
		deployments[retained.DeploymentID] = struct{}{}
		operations[retained.OperationID] = struct{}{}
	}
	return manifest, nil
}

type bindingManifestRepository interface {
	BindDeployment(context.Context, string, provider.CredentialBinding, string) (string, error)
	ListDeploymentBindings(context.Context) ([]credentialstore.DeploymentBindingMetadata, error)
}

// applyBindingManifest binds every assignment the database does not already
// hold exactly. A row already bound to the same provider, key and operation ID
// is skipped rather than replayed: the database treats a replay by a different
// actor as a conflict, and every workflow run is a different actor, so
// replaying would make a manifest that is already applied impossible to apply
// again and a partially applied one impossible to finish.
func applyBindingManifest(ctx context.Context, repository bindingManifestRepository, manifest bindingManifest, actor string) error {
	current, err := repository.ListDeploymentBindings(ctx)
	if err != nil {
		return err
	}
	bound := make(map[contract.DeploymentID]credentialstore.DeploymentBindingMetadata, len(current))
	for _, item := range current {
		bound[item.DeploymentID] = item
	}
	for _, assignment := range manifest.Assignments {
		if existing, found := bound[assignment.DeploymentID]; found && existing.Provider == assignment.Provider && existing.KeyID == assignment.KeyID && existing.OperationID == assignment.OperationID {
			continue
		}
		binding := provider.CredentialBinding{DeploymentID: assignment.DeploymentID, Provider: assignment.Provider, KeyID: assignment.KeyID}
		if _, err := repository.BindDeployment(ctx, assignment.OperationID, binding, actor); err != nil {
			return fmt.Errorf("applying deployment %q: %w", assignment.DeploymentID, err)
		}
	}
	return nil
}

func verifyBindingManifest(metadata []credentialstore.DeploymentBindingMetadata, manifest bindingManifest) error {
	required := len(manifest.Assignments) + len(manifest.RetainedBindings)
	if len(metadata) != required {
		return fmt.Errorf("binding readback has %d rows, manifest requires %d", len(metadata), required)
	}
	byDeployment := make(map[contract.DeploymentID]credentialstore.DeploymentBindingMetadata, len(metadata))
	for _, item := range metadata {
		byDeployment[item.DeploymentID] = item
	}
	expectedRows := append(append([]bindingManifestAssignment(nil), manifest.Assignments...), manifest.RetainedBindings...)
	for _, expected := range expectedRows {
		actual, found := byDeployment[expected.DeploymentID]
		if !found || actual.Provider != expected.Provider || actual.KeyID != expected.KeyID || actual.OperationID != expected.OperationID {
			return fmt.Errorf("binding readback differs at deployment %q", expected.DeploymentID)
		}
	}
	return nil
}

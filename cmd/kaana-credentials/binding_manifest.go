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
	return manifest, nil
}

func applyBindingManifest(ctx context.Context, repository *credentialstore.Postgres, manifest bindingManifest, actor string) error {
	for _, assignment := range manifest.Assignments {
		binding := provider.CredentialBinding{DeploymentID: assignment.DeploymentID, Provider: assignment.Provider, KeyID: assignment.KeyID}
		if _, err := repository.BindDeployment(ctx, assignment.OperationID, binding, actor); err != nil {
			return fmt.Errorf("applying deployment %q: %w", assignment.DeploymentID, err)
		}
	}
	return nil
}

func verifyBindingManifest(metadata []credentialstore.DeploymentBindingMetadata, manifest bindingManifest) error {
	if len(metadata) != len(manifest.Assignments) {
		return fmt.Errorf("binding readback has %d rows, manifest requires %d", len(metadata), len(manifest.Assignments))
	}
	byDeployment := make(map[contract.DeploymentID]credentialstore.DeploymentBindingMetadata, len(metadata))
	for _, item := range metadata {
		byDeployment[item.DeploymentID] = item
	}
	for _, expected := range manifest.Assignments {
		actual, found := byDeployment[expected.DeploymentID]
		if !found || actual.Provider != expected.Provider || actual.KeyID != expected.KeyID || actual.OperationID != expected.OperationID {
			return fmt.Errorf("binding readback differs at deployment %q", expected.DeploymentID)
		}
	}
	return nil
}

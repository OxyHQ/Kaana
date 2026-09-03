package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type credentialOperationManifest struct {
	SchemaVersion           int                 `json:"schemaVersion"`
	AWSRegion               string              `json:"awsRegion"`
	AccountID               string              `json:"accountId"`
	Cluster                 string              `json:"cluster"`
	TaskDefinitionFamily    string              `json:"taskDefinitionFamily"`
	ContainerName           string              `json:"containerName"`
	Image                   string              `json:"image"`
	TaskRoleARN             string              `json:"taskRoleArn"`
	ExecutionRoleARN        string              `json:"executionRoleArn"`
	DatabaseURLParameterARN string              `json:"databaseUrlParameterArn"`
	KMSKeyARN               string              `json:"kmsKeyArn"`
	SubnetIDs               []string            `json:"subnetIds"`
	SecurityGroupID         string              `json:"securityGroupId"`
	Operations              map[string][]string `json:"operations"`
}

func TestCredentialAdminWorkflowHasOnlyReviewedOperations(t *testing.T) {
	manifestBytes, err := os.ReadFile("../../.github/credential-admin-operations.json")
	if err != nil {
		t.Fatalf("reading credential operation manifest: %v", err)
	}
	var manifest credentialOperationManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("parsing credential operation manifest: %v", err)
	}

	expectedOperations := map[string][]string{
		"list": {"list"},
		"import-relay-groq": {
			"import-ssm", "--provider", "groq", "--key-id", "relay-groq-20260902",
			"--position", "2", "--parameter", "/oxy/relay/RELAY_PROVIDER_GROQ_API_KEY", "--class", "paid",
		},
		"import-relay-openrouter": {
			"import-ssm", "--provider", "openrouter", "--key-id", "relay-openrouter-20260902",
			"--position", "2", "--parameter", "/oxy/relay/RELAY_PROVIDER_OPENROUTER_API_KEY", "--class", "paid",
		},
		"import-relay-xai": {
			"import-ssm", "--provider", "xai", "--key-id", "relay-xai-20260902",
			"--position", "2", "--parameter", "/oxy/relay/RELAY_PROVIDER_XAI_API_KEY", "--class", "paid",
		},
	}
	if !reflect.DeepEqual(manifest.Operations, expectedOperations) {
		t.Fatalf("credential operations drifted: %#v", manifest.Operations)
	}

	expectedIdentity := credentialOperationManifest{
		SchemaVersion:           1,
		AWSRegion:               "us-west-2",
		AccountID:               "237343248947",
		Cluster:                 "oxy-cluster",
		TaskDefinitionFamily:    "oxy-kaana-credential-admin",
		ContainerName:           "kaana-credential-admin",
		Image:                   "237343248947.dkr.ecr.us-west-2.amazonaws.com/oxy/kaana@sha256:fabc92349aedb245da44cb368e6f09a0ea243a44c36dca707dd80140e3442955",
		TaskRoleARN:             "arn:aws:iam::237343248947:role/oxy-kaana-credential-admin",
		ExecutionRoleARN:        "arn:aws:iam::237343248947:role/oxy-kaana-credential-admin-execution",
		DatabaseURLParameterARN: "arn:aws:ssm:us-west-2:237343248947:parameter/oxy/kaana/CREDENTIAL_ADMIN_DATABASE_URL",
		KMSKeyARN:               "arn:aws:kms:us-west-2:237343248947:key/d4ca87d9-f773-4409-8ae7-e96d7f3438c5",
		SubnetIDs:               []string{"subnet-08f5cc132b3cab15c", "subnet-0bfb367f29d1fd375"},
		SecurityGroupID:         "sg-0a4e4450d15996cdf",
	}
	manifest.Operations = nil
	if !reflect.DeepEqual(manifest, expectedIdentity) {
		t.Fatalf("credential task identity drifted: %#v", manifest)
	}

	workflowBytes, err := os.ReadFile("../../.github/workflows/credential-admin.yml")
	if err != nil {
		t.Fatalf("reading credential workflow: %v", err)
	}
	workflow := string(workflowBytes)
	for _, required := range []string{
		"type: choice",
		"refs/heads/main",
		"arn:aws:iam::237343248947:role/oxy-kaana-github-deploy",
		"aws ecs run-task",
		"aws ecs wait tasks-stopped",
		"credential-admin-operations.json",
		"AUDIT_ACTOR: github-actions:OxyHQ/Kaana:${{ github.run_id }}",
		"^github-actions:OxyHQ/Kaana:[0-9]+$",
		`environment: [{name: "KAANA_CREDENTIAL_ACTOR", value: $audit_actor}]`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("credential workflow lost required boundary %q", required)
		}
	}
	for _, forbidden := range []string{
		"aws ssm get-parameter",
		"aws ssm put-parameter",
		"aws ecs update-service",
		"aws ecs register-task-definition",
		"type: string",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("credential workflow contains forbidden capability %q", forbidden)
		}
	}
}

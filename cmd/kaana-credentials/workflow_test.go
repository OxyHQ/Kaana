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
	SourceCommit            string              `json:"sourceCommit"`
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
		"list":    {"list"},
		"migrate": {"migrate"},
		"deduplicate-groq": {
			"deduplicate", "--operation-id", "kop_0af8007d9fdddd88d2622eabff99aeb9",
			"--provider", "groq", "--duplicate-key-id", "relay-groq-20260902",
			"--keep-key-id", "legacy-alia-20260901",
		},
		"deduplicate-openrouter": {
			"deduplicate", "--operation-id", "kop_64722ac4d450f4ac2d5c6b6bd0fe0a15",
			"--provider", "openrouter", "--duplicate-key-id", "relay-openrouter-20260902",
			"--keep-key-id", "legacy-alia-20260901",
		},
		"deduplicate-xai": {
			"deduplicate", "--operation-id", "kop_29de63b8cd98855b8e0a440d9db7aef3",
			"--provider", "xai", "--duplicate-key-id", "relay-xai-20260902",
			"--keep-key-id", "legacy-alia-20260901",
		},
		"rekey-cerebras-primary": {
			"rekey-id", "--operation-id", "kop_5b4f96c394a7a288754a1388fed0c5b2",
			"--provider", "cerebras", "--old-key-id", "cerebras-relay-main",
			"--new-key-id", "43405cea-a7d1-49c2-ba73-5a84536d3abf",
		},
		"rekey-groq-primary": {
			"rekey-id", "--operation-id", "kop_3ac18ed3ab6c6bf97862b03193ef4357",
			"--provider", "groq", "--old-key-id", "legacy-alia-20260901",
			"--new-key-id", "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1",
			"--requires-operation-id", "kop_0af8007d9fdddd88d2622eabff99aeb9",
		},
		"rekey-openrouter-primary": {
			"rekey-id", "--operation-id", "kop_eb9b5b291df58b7573633e92f5eb8ad4",
			"--provider", "openrouter", "--old-key-id", "legacy-alia-20260901",
			"--new-key-id", "b8090dce-82f2-4077-9fc1-fd831a53ca27",
			"--requires-operation-id", "kop_64722ac4d450f4ac2d5c6b6bd0fe0a15",
		},
		"rekey-xai-primary": {
			"rekey-id", "--operation-id", "kop_6f4d191e8834c4410049904de37952a6",
			"--provider", "xai", "--old-key-id", "legacy-alia-20260901",
			"--new-key-id", "1d72d527-81ca-41e5-9644-2d81a4b126ec",
			"--requires-operation-id", "kop_29de63b8cd98855b8e0a440d9db7aef3",
		},
		"rekey-elevenlabs-primary": {
			"rekey-id", "--operation-id", "kop_b5d7eca4d16b7162529ab4688042efae",
			"--provider", "elevenlabs", "--old-key-id", "legacy-alia-20260901",
			"--new-key-id", "6e4abb22-af03-46fb-95d9-b2e4286657f2",
		},
		"rekey-groq-secondary-if-different": {
			"rekey-id", "--operation-id", "kop_c1b4d87bf4e2a5a6d815dc1a1b0460a3",
			"--provider", "groq", "--old-key-id", "relay-groq-20260902",
			"--new-key-id", "f0c4e09f-a5f8-4af8-86b4-960e2d637ce1",
			"--requires-operation-id", "kop_0af8007d9fdddd88d2622eabff99aeb9",
			"--requires-outcome", "different",
		},
		"rekey-openrouter-secondary-if-different": {
			"rekey-id", "--operation-id", "kop_0418afb5cc61a79a8ff2db4ddcd5b809",
			"--provider", "openrouter", "--old-key-id", "relay-openrouter-20260902",
			"--new-key-id", "2bdf7141-fdf6-4cbf-8332-3ea98202f52f",
			"--requires-operation-id", "kop_64722ac4d450f4ac2d5c6b6bd0fe0a15",
			"--requires-outcome", "different",
		},
		"rekey-xai-secondary-if-different": {
			"rekey-id", "--operation-id", "kop_49a92662d24e3190eaa25e0396780e29",
			"--provider", "xai", "--old-key-id", "relay-xai-20260902",
			"--new-key-id", "ad05516d-e2d2-4be4-8735-5e69c9bff41c",
			"--requires-operation-id", "kop_29de63b8cd98855b8e0a440d9db7aef3",
			"--requires-outcome", "different",
		},
	}
	if !reflect.DeepEqual(manifest.Operations, expectedOperations) {
		t.Fatalf("credential operations drifted: %#v", manifest.Operations)
	}
	runbookBytes, err := os.ReadFile("../../docs/provider-credential-id-cutover.md")
	if err != nil {
		t.Fatalf("reading credential ID cutover runbook: %v", err)
	}
	runbook := string(runbookBytes)
	operationIDs := make(map[string]string)
	canonicalIDs := make(map[string]struct{})
	for operationName, command := range manifest.Operations {
		joined := strings.Join(command, " ")
		for _, forbidden := range []string{"--position", "--value", "import-ssm"} {
			if operationName != "list" && strings.Contains(joined, forbidden) {
				t.Errorf("credential operation %q contains forbidden authority/transport %q", operationName, forbidden)
			}
		}
		if operationName == "list" || operationName == "migrate" {
			continue
		}
		if !strings.Contains(runbook, joined) {
			t.Errorf("runbook does not contain exact command %q", joined)
		}
		for index := 0; index < len(command)-1; index++ {
			switch command[index] {
			case "--operation-id":
				if previous, duplicate := operationIDs[command[index+1]]; duplicate {
					t.Errorf("operation id %q is reused by %q and %q", command[index+1], previous, operationName)
				}
				operationIDs[command[index+1]] = operationName
			case "--new-key-id":
				canonicalIDs[command[index+1]] = struct{}{}
			}
		}
	}
	if len(operationIDs) != 11 {
		t.Fatalf("exact operation id count = %d, want 11", len(operationIDs))
	}
	expectedCanonicalIDs := map[string]struct{}{
		"43405cea-a7d1-49c2-ba73-5a84536d3abf": {},
		"8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1": {},
		"b8090dce-82f2-4077-9fc1-fd831a53ca27": {},
		"1d72d527-81ca-41e5-9644-2d81a4b126ec": {},
		"6e4abb22-af03-46fb-95d9-b2e4286657f2": {},
		"f0c4e09f-a5f8-4af8-86b4-960e2d637ce1": {},
		"2bdf7141-fdf6-4cbf-8332-3ea98202f52f": {},
		"ad05516d-e2d2-4be4-8735-5e69c9bff41c": {},
	}
	if !reflect.DeepEqual(canonicalIDs, expectedCanonicalIDs) {
		t.Fatalf("canonical provider credential IDs drifted: %#v", canonicalIDs)
	}

	expectedIdentity := credentialOperationManifest{
		SchemaVersion:           1,
		AWSRegion:               "us-west-2",
		AccountID:               "237343248947",
		Cluster:                 "oxy-cluster",
		TaskDefinitionFamily:    "oxy-kaana-credential-admin",
		ContainerName:           "kaana-credential-admin",
		Image:                   "237343248947.dkr.ecr.us-west-2.amazonaws.com/oxy/kaana@sha256:e9536dff531f3581754b7b24eee30532a6dfce7b544ec6e8e80268e7c6021ada",
		SourceCommit:            "728cf22b18042e3f8e7e9d68d6fb44a18756d6b4",
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
	expectedOperationChoices := `        options:
          - list
          - migrate
          - deduplicate-groq
          - deduplicate-openrouter
          - deduplicate-xai
          - rekey-cerebras-primary
          - rekey-groq-primary
          - rekey-openrouter-primary
          - rekey-xai-primary
          - rekey-elevenlabs-primary
          - rekey-groq-secondary-if-different
          - rekey-openrouter-secondary-if-different
          - rekey-xai-secondary-if-different`
	if !strings.Contains(workflow, expectedOperationChoices) {
		t.Errorf("credential workflow operation choices drifted")
	}
	for _, required := range []string{
		"type: choice",
		"refs/heads/main",
		"arn:aws:iam::237343248947:role/oxy-kaana-github-deploy",
		"aws ecs run-task",
		"aws ecs wait tasks-stopped",
		"aws ecr describe-images",
		`--image-ids "imageDigest=$digest"`,
		`--image-ids "imageTag=$source_commit"`,
		"aws ecs register-task-definition",
		"file:///tmp/credential-admin-task-definition.json",
		`item["image"] = config["image"]`,
		"if task != expected:",
		"the registered task definition differs from the derived image-only revision",
		"credential-admin image digest did not survive registration",
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
		"type: string",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("credential workflow contains forbidden capability %q", forbidden)
		}
	}
}

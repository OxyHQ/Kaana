package main

import (
	"os"
	"strings"
	"testing"
)

func TestAWSDeployRequiresMainAndTheExactCredentialIDCutover(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../.github/workflows/deploy-aws.yml")
	if err != nil {
		t.Fatalf("reading the AWS deploy workflow: %v", err)
	}
	workflow := string(workflowBytes)
	gate := `    if: >-
      github.ref == 'refs/heads/main' &&
      vars.KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE == 'true'`
	if strings.Count(workflow, gate) != 1 {
		t.Fatalf("the AWS deploy job does not have the one exact fail-closed main/cutover gate")
	}
	if !strings.Contains(workflow, "  push:\n    branches: [main]") || !strings.Contains(workflow, "  workflow_dispatch:") {
		t.Fatal("the gated workflow no longer covers both main pushes and manual dispatch")
	}
	for _, bypass := range []string{
		"KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE != 'false'",
		"KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE ||",
		"github.event_name == 'workflow_dispatch' ||",
	} {
		if strings.Contains(workflow, bypass) {
			t.Fatalf("the AWS deploy workflow contains cutover bypass %q", bypass)
		}
	}
}

package main

import (
	"os"
	"strings"
	"testing"
)

func TestAWSDeployBuildsOnlyFromMainAndGatesECSDeployment(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../.github/workflows/deploy-aws.yml")
	if err != nil {
		t.Fatalf("reading the AWS deploy workflow: %v", err)
	}
	workflow := string(workflowBytes)
	mainGate := `    if: github.ref == 'refs/heads/main'`
	if strings.Count(workflow, mainGate) != 1 {
		t.Fatal("the AWS image build does not have the one exact main-only gate")
	}
	dispatch := `  workflow_dispatch:
    inputs:
      mode:
        description: Build the immutable image only, or deploy it after every cutover gate passes
        type: choice
        required: true
        default: build-only
        options:
          - build-only
          - deploy`
	if strings.Count(workflow, dispatch) != 1 || !strings.Contains(workflow, "  push:\n    branches: [main]") {
		t.Fatal("the workflow no longer exposes the exact safe build-only/deploy modes on main")
	}
	build := strings.Index(workflow, "      - name: Build and push (linux/arm64)")
	deploy := strings.Index(workflow, "      - name: Deploy to ECS (skipped until a service exists)")
	if build < 0 || deploy < 0 || build >= deploy {
		t.Fatal("the immutable image must be built before the gated ECS step")
	}
	deployGate := `        if: >-
          vars.KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE == 'true' &&
          (github.event_name == 'push' || inputs.mode == 'deploy')`
	if strings.Count(workflow, deployGate) != 1 {
		t.Fatal("the ECS step does not require both the exact cutover gate and an explicit deploy-capable event")
	}
	between := workflow[build:deploy]
	if strings.Contains(between, "KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE") || strings.Contains(between, "inputs.mode") {
		t.Fatal("the immutable build is incorrectly hidden behind the deployment gate")
	}
	for _, bypass := range []string{
		"KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE != 'false'",
		"KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE ||",
		"github.event_name == 'workflow_dispatch' ||",
		"inputs.mode == 'build-only' ||",
	} {
		if strings.Contains(workflow, bypass) {
			t.Fatalf("the AWS deploy workflow contains cutover bypass %q", bypass)
		}
	}
}

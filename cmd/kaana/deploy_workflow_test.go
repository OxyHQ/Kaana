package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPeerDeploymentNetworkDiscovery(t *testing.T) {
	command := exec.Command("python3", "../../.github/scripts/test_realtime_peer_vpc.py")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("private peer network discovery: %v\n%s", err, output)
	}
	workflow, err := os.ReadFile("../../.github/workflows/deploy-aws.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`if [ "$service" = "$APP" ]; then`,
		`PEER_CIDR=$(python3 .github/scripts/realtime-peer-vpc.py --cluster "$CLUSTER" --service "$service")`,
		`--arg peer "$PEER_CIDR"`,
		`map(select(.name != "KAANA_REALTIME_PEER_VPC_CIDR"))`,
		`{name:"KAANA_REALTIME_PEER_VPC_CIDR",value:$peer}`,
	} {
		if !strings.Contains(string(workflow), required) {
			t.Errorf("lost peer configuration boundary %s", required)
		}
	}
}

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
          vars.KAANA_CREDENTIAL_RUNTIME_SCHEMA_0011_COMPLETE == 'true' &&
          vars.KAANA_CREDENTIAL_RUNTIME_SCHEMA_0012_COMPLETE == 'true' &&
          vars.KAANA_CREDENTIAL_RUNTIME_SCHEMA_0013_COMPLETE == 'true' &&
          (github.event_name == 'push' || inputs.mode == 'deploy')`
	if strings.Count(workflow, deployGate) != 1 {
		t.Fatal("the ECS step does not require both the exact cutover gate and an explicit deploy-capable event")
	}
	migrate := strings.Index(workflow, "      - name: Apply the candidate's credential schema before serving it")
	if migrate < 0 || build >= migrate || migrate >= deploy {
		t.Fatal("the candidate's migrator must run after the build and before any service moves")
	}
	buildStep := workflow[build:migrate]
	if strings.Contains(buildStep, "KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE") ||
		strings.Contains(buildStep, "KAANA_CREDENTIAL_RUNTIME_SCHEMA_0011_COMPLETE") ||
		strings.Contains(buildStep, "KAANA_CREDENTIAL_RUNTIME_SCHEMA_0012_COMPLETE") || strings.Contains(buildStep, "inputs.mode") {
		t.Fatal("the immutable build is incorrectly hidden behind the deployment gate")
	}
	// Every run that may deploy migrates first; a build-only run touches no
	// schema, except while the historical 0013 gate is open.
	migrateStep := workflow[migrate:deploy]
	for _, migrationBoundary := range []string{
		`        if: >-
          vars.KAANA_CREDENTIAL_RUNTIME_SCHEMA_0013_COMPLETE != 'true' ||
          github.event_name == 'push' || inputs.mode == 'deploy'`,
		`command:["migrate"]`,
		`if [ "$exit_code" != 0 ]; then`,
		"the candidate's migrator exited $exit_code; no service was updated",
	} {
		if !strings.Contains(migrateStep, migrationBoundary) {
			t.Errorf("the pre-deploy migration lost boundary %q", migrationBoundary)
		}
	}
	if strings.Contains(migrateStep, "aws ecs update-service") {
		t.Fatal("the migration step can change serving traffic")
	}
	for _, bypass := range []string{
		"KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE != 'false'",
		"KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE ||",
		"KAANA_CREDENTIAL_RUNTIME_SCHEMA_0011_COMPLETE != 'false'",
		"KAANA_CREDENTIAL_RUNTIME_SCHEMA_0011_COMPLETE ||",
		"KAANA_CREDENTIAL_RUNTIME_SCHEMA_0012_COMPLETE != 'false'",
		"KAANA_CREDENTIAL_RUNTIME_SCHEMA_0012_COMPLETE ||",
		"KAANA_CREDENTIAL_RUNTIME_SCHEMA_0013_COMPLETE != 'false'",
		"KAANA_CREDENTIAL_RUNTIME_SCHEMA_0013_COMPLETE ||",
		"github.event_name == 'workflow_dispatch' ||",
		"inputs.mode == 'build-only' ||",
	} {
		if strings.Contains(workflow, bypass) {
			t.Fatalf("the AWS deploy workflow contains cutover bypass %q", bypass)
		}
	}
}

func TestCandidateCanaryIsIsolatedBoundedAndAlwaysCleanedUp(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../.github/workflows/candidate-canary.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(workflowBytes)
	for _, required := range []string{
		"if: github.ref == 'refs/heads/main'",
		"oxy-kaana-candidate",
		`source_digest" != "$DIGEST`,
		"aws ecs wait tasks-running",
		`--started-by "gh-kaana-candidate-${GITHUB_RUN_ID}"`,
		"isolated candidate start failure (operator-safe)",
		"stoppedReason",
		"containers:[.containers[]|{name,lastStatus,exitCode,reason}]",
		`--log-stream-name "$log_prefix/kaana/$task_id"`,
		`select(.msg=="kaana could not start")|{msg,error}`,
		`'{startupError:$startupError}'`,
		"privateIPv4Address",
		`observed_digest" = "$DIGEST`,
		"trap cleanup EXIT",
		"aws ecs stop-task",
		"aws ecs wait tasks-stopped",
		"aws ecs deregister-task-definition",
		"OxyOperation,value=KaanaCandidateCanary",
		"KAANA_CANDIDATE_MAX_LIFETIME",
		"assignPublicIp' <<<\"$network\")\" != DISABLED",
		"/usr/local/bin/kaana-probe",
		"candidate loopback /livez probe failed",
		"candidatePrivateIp=$private_ip",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("candidate workflow lost boundary %q", required)
		}
	}
	if strings.Contains(workflow, "aws ecs update-service") || strings.Contains(workflow, "assignPublicIp: ENABLED") {
		t.Fatal("candidate workflow can change serving traffic or request a public address")
	}
}

func TestPublisherDeployCarriesOnlyTheReviewedDiscoveryCredentialIDs(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../.github/workflows/deploy-aws.yml")
	if err != nil {
		t.Fatalf("reading the AWS deploy workflow: %v", err)
	}
	workflow := string(workflowBytes)
	for _, required := range []string{
		"      - '.github/credential-admin-operations.json'",
		".discoveryCredentialIds |",
		`keys == ["anthropic", "cerebras", "elevenlabs", "groq", "openrouter", "siliconflow", "xai", "xai-realtime"]`,
		`if [ "$service" = "$PUBLISHER_SERVICE" ]; then`,
		`--argjson ids "$DISCOVERY_CREDENTIALS"`,
		"REGISTERED_DISCOVERY=",
		"EXPECTED_DISCOVERY=",
		"did not preserve the exact eight publisher discovery credential IDs",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("publisher deployment lost required exact-ID boundary %q", required)
		}
	}
	for _, variable := range []string{
		"KAANA_PROVIDER_ANTHROPIC_DISCOVERY_KEY_ID",
		"KAANA_PROVIDER_CEREBRAS_DISCOVERY_KEY_ID",
		"KAANA_PROVIDER_ELEVENLABS_DISCOVERY_KEY_ID",
		"KAANA_PROVIDER_GROQ_DISCOVERY_KEY_ID",
		"KAANA_PROVIDER_OPENROUTER_DISCOVERY_KEY_ID",
		"KAANA_PROVIDER_SILICONFLOW_DISCOVERY_KEY_ID",
		"KAANA_PROVIDER_XAI_DISCOVERY_KEY_ID",
		"KAANA_PROVIDER_XAI_REALTIME_DISCOVERY_KEY_ID",
	} {
		if count := strings.Count(workflow, variable); count != 3 {
			t.Errorf("publisher discovery variable %q occurs %d times, want exact remove/add/readback coverage", variable, count)
		}
	}
	register := strings.Index(workflow, "ARN=$(aws ecs register-task-definition")
	readback := strings.Index(workflow, "REGISTERED_DISCOVERY=$(")
	update := strings.Index(workflow, "aws ecs update-service")
	if register < 0 || readback <= register || update <= readback {
		t.Fatal("publisher discovery IDs are not registered and read back before the service update")
	}
}

func TestCredentialControlServicesDeployOnlyAfterTerraformCreatesThem(t *testing.T) {
	workflowBytes, err := os.ReadFile("../../.github/workflows/deploy-aws.yml")
	if err != nil {
		t.Fatalf("reading the AWS deploy workflow: %v", err)
	}
	workflow := string(workflowBytes)
	for _, required := range []string{
		"CREDENTIAL_CONTROL_SERVICE: kaana-credential-control",
		"CREDENTIAL_CONTROL_FAMILY: oxy-kaana-credential-control",
		"PLATFORM_CREDENTIAL_CONTROL_SERVICE: kaana-platform-credential-control",
		"PLATFORM_CREDENTIAL_CONTROL_FAMILY: oxy-kaana-platform-credential-control",
		`STATUS=$(aws ecs describe-services --cluster "$CLUSTER" --services "$service"`,
		`if [ "$STATUS" != "ACTIVE" ]; then`,
		`return 0`,
		`BASE=$(aws ecs describe-task-definition --task-definition "$family" --query 'taskDefinition')`,
		`REGISTERED_IMAGE=$(aws ecs describe-task-definition --task-definition "$ARN"`,
		`if [ "$REGISTERED_IMAGE" != "$IMAGE" ]; then`,
		`deploy_one "$CREDENTIAL_CONTROL_SERVICE" "$CREDENTIAL_CONTROL_FAMILY" "$CREDENTIAL_CONTROL_SERVICE"`,
		`deploy_one "$PLATFORM_CREDENTIAL_CONTROL_SERVICE" "$PLATFORM_CREDENTIAL_CONTROL_FAMILY" "$PLATFORM_CREDENTIAL_CONTROL_SERVICE"`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("credential-control digest deployment lost %q", required)
		}
	}
	status := strings.Index(workflow, `STATUS=$(aws ecs describe-services --cluster "$CLUSTER" --services "$service"`)
	family := strings.Index(workflow, `BASE=$(aws ecs describe-task-definition --task-definition "$family"`)
	register := strings.Index(workflow, `ARN=$(aws ecs register-task-definition`)
	update := strings.Index(workflow, `aws ecs update-service --cluster "$CLUSTER" --service "$service" --task-definition "$ARN"`)
	if status < 0 || family <= status || register <= family || update <= register {
		t.Fatal("service existence, Terraform family, digest registration and service update are not ordered safely")
	}
	for _, forbidden := range []string{
		"aws ecs create-service",
		"aws iam create-role",
		"aws ssm put-parameter",
		"KAANA_PLATFORM_CREDENTIAL_CONTROL_PRIVATE_KEY",
		"KAANA_PROVIDER_KEY",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("deploy workflow contains forbidden bootstrap authority %q", forbidden)
		}
	}
}

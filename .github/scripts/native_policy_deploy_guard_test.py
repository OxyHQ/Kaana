import hashlib
import json
import importlib.util
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("guard", ROOT / ".github/scripts/native-policy-deploy-guard.py")
GUARD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARD)


def task(pair=None):
    return {"containerDefinitions": [{"name": "kaana", "environment": [{"name": k, "value": v} for k, v in (pair or {}).items()]}]}


class NativePolicyDeployTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        folder = self.root / "configs/native-admission"
        folder.mkdir(parents=True)
        (folder / "reviewed.json").write_bytes(b'{"policies":[]}\n')
        self.pair = {GUARD.PATH: "/etc/kaana-native/reviewed.json", GUARD.SHA: hashlib.sha256((folder / "reviewed.json").read_bytes()).hexdigest()}

    def test_inert_deployment_remains_allowed(self):
        self.assertFalse(GUARD.validate(task(), task(), self.root))

    def test_first_configuration_and_preservation_are_allowed(self):
        self.assertTrue(GUARD.validate(task(), task(self.pair), self.root))
        self.assertTrue(GUARD.validate(task(self.pair), task(self.pair), self.root, expected=task(self.pair)))

    def test_reviewed_version_change_uses_new_image_bytes(self):
        old = dict(self.pair, KAANA_NATIVE_POLICY_SHA256="0" * 64)
        self.assertTrue(GUARD.validate(task(old), task(self.pair), self.root))

    def test_configured_authority_cannot_disappear(self):
        with self.assertRaisesRegex(ValueError, "cannot remove"):
            GUARD.validate(task(self.pair), task(), self.root)

    def test_partial_empty_duplicate_and_secret_bindings_are_refused(self):
        variants = [task({GUARD.PATH: self.pair[GUARD.PATH]}), task({GUARD.SHA: self.pair[GUARD.SHA]}), task(dict(self.pair, KAANA_NATIVE_POLICY_PATH=""))]
        duplicate = task(self.pair)
        duplicate["containerDefinitions"][0]["environment"].append({"name": GUARD.SHA, "value": self.pair[GUARD.SHA]})
        variants.append(duplicate)
        secret = task(self.pair)
        secret["containerDefinitions"][0]["secrets"] = [{"name": GUARD.PATH, "valueFrom": "opaque-reference"}]
        variants.append(secret)
        for bad in variants:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                GUARD.validate(task(), bad, self.root)
            with self.subTest(live=bad), self.assertRaises(ValueError):
                GUARD.validate(bad, task(self.pair), self.root)

    def test_missing_changed_or_outside_image_artifact_is_refused(self):
        for path, sha in [("/etc/kaana-native/absent.json", self.pair[GUARD.SHA]), (self.pair[GUARD.PATH], "0" * 64), ("/etc/kaana-native/../outside.json", self.pair[GUARD.SHA]), (self.pair[GUARD.PATH], "NOT_SHA")]:
            with self.subTest(path=path, sha=sha), self.assertRaises(ValueError):
                GUARD.validate(task(), task({GUARD.PATH: path, GUARD.SHA: sha}), self.root)

    def test_readback_cannot_diverge_from_reviewed_candidate(self):
        with self.assertRaisesRegex(ValueError, "registered"):
            GUARD.validate(task(), task(), self.root, expected=task(self.pair))

    def test_exact_container_required(self):
        duplicate = task()
        duplicate["containerDefinitions"] *= 2
        for bad in [{}, duplicate, {"containerDefinitions": [{"name": "publisher"}]}]:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                GUARD.validate(task(), bad, self.root)

    def test_workflow_checks_before_registration_and_after_readback(self):
        workflow = (ROOT / ".github/workflows/deploy-aws.yml").read_text()
        deploy = workflow[workflow.index('BASE=$(aws ecs describe-task-definition --task-definition "$family"'):]
        first = deploy.index("python3 .github/scripts/native-policy-deploy-guard.py")
        register = deploy.index("ARN=$(aws ecs register-task-definition")
        second = deploy.index("python3 .github/scripts/native-policy-deploy-guard.py", first + 1)
        update = deploy.index("aws ecs update-service")
        self.assertLess(first, register)
        self.assertLess(register, second)
        self.assertLess(second, update)
        self.assertIn('--expected "/tmp/task-definition-$service.json"', deploy)
        self.assertIn('--task-definition "$family"', deploy)
        for source in [ROOT / "Makefile", ROOT / ".github/workflows/ci.yml"]:
            self.assertIn("native_policy_deploy_guard_test.py", source.read_text())



class NativeMigrationWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.workflow = (ROOT / ".github/workflows/deploy-aws.yml").read_text()
        path = ROOT / "configs/native-admission/alia-kaana-v2.json"
        self.pair = {GUARD.PATH: "/etc/kaana-native/alia-kaana-v2.json", GUARD.SHA: hashlib.sha256(path.read_bytes()).hexdigest()}

    def test_live_and_candidate_config_drive_only_native_opt_in(self):
        import shlex
        import subprocess
        start = self.workflow.index('          native_configured=false')
        end = self.workflow.index("          profile=$(jq -ce '.taskProfiles.migrator'", start)
        fragment = "\n".join(line[10:] for line in self.workflow[start:end].splitlines())
        for live, candidate, expected in [(task(), task(), "false"), (task(self.pair), task(self.pair), "true"), (task(), task(self.pair), "true"), (task(self.pair), task(), None), (task(), task({GUARD.PATH: self.pair[GUARD.PATH]}), None)]:
            with self.subTest(expected=expected), tempfile.TemporaryDirectory() as folder:
                isolated = fragment.replace("/tmp/kaana-migration-", folder + "/kaana-migration-")
                prefix = "set -euo pipefail\nCLUSTER=fixture\nAPP=kaana\nFAMILY=oxy-kaana\nLIVE=" + shlex.quote(json.dumps(live)) + "\nCANDIDATE=" + shlex.quote(json.dumps(candidate)) + "\n"
                prefix += r'''aws(){
 case "$*" in
  *services\[0\].status*) printf '%s' ACTIVE ;;
  *services\[0\].taskDefinition*) printf '%s' live ;;
  *'--task-definition live'*) printf '%s' "$LIVE" ;;
  *'--task-definition oxy-kaana'*) printf '%s' "$CANDIDATE" ;;
  *) return 99 ;;
 esac
}
'''
                result = subprocess.run(["bash", "-c", prefix + isolated + '\nprintf "%s" "$native_configured"'], cwd=ROOT, text=True, capture_output=True)
                if expected is None:
                    self.assertNotEqual(result.returncode, 0)
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout, expected)

    def test_task_command_and_serving_barrier_are_exact(self):
        import subprocess
        start = self.workflow.index('          overrides=$(jq -cn --arg container "$container" --argjson native')
        end = self.workflow.index('          result=$(aws ecs run-task', start)
        fragment = "\n".join(line[10:] for line in self.workflow[start:end].splitlines())
        for configured in ("false", "true"):
            result = subprocess.run(["bash", "-c", 'set -euo pipefail\ncontainer=migrator\nnative_configured=' + configured + '\n' + fragment + '\nprintf "%s" "$overrides"'], text=True, capture_output=True, check=True)
            self.assertEqual(json.loads(result.stdout)["containerOverrides"][0]["command"], ["migrate", "--native"] if configured == "true" else ["migrate"])
        start = self.workflow.index('              if [ "$(jq -r')
        end = self.workflow.index('\n              fi', start) + len('\n              fi')
        fragment = "\n".join(line[14:] for line in self.workflow[start:end].splitlines())
        for configured, ready, refused in [(True, "true", False), (True, "false", True), (True, "", True), (False, "false", False)]:
            result = subprocess.run(["bash", "-c", 'NATIVE_POLICY=\'' + json.dumps({"configured": configured}) + '\'\nNATIVE_SCHEMA_READY=' + ready + '\ngate(){\n' + fragment + '\n}\ngate'], text=True, capture_output=True)
            self.assertEqual(result.returncode != 0, refused)
        self.assertLess(self.workflow.index('if [ "$exit_code" != 0 ]'), self.workflow.index('echo "native_ready=$native_configured"'))
        self.assertLess(self.workflow.index('native configuration appeared after schema preparation'), self.workflow.index('aws ecs update-service'))
        self.assertIn('NATIVE_SCHEMA_READY: ${{ steps.schema.outputs.native_ready }}', self.workflow)


if __name__ == "__main__":
    unittest.main()

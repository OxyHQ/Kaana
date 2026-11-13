import hashlib
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


if __name__ == "__main__":
    unittest.main()

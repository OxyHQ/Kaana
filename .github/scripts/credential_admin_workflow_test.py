"""Execute the real workflow's image/command fragments with synthetic AWS responses."""
import pathlib
import json
import shlex
import tempfile
import subprocess
import unittest

SOURCE = (pathlib.Path(__file__).parents[1] / "workflows/credential-admin.yml").read_text()

class ReviewedMigrationWorkflow(unittest.TestCase):
    def test_image_selection_is_operation_scoped(self):
        start = SOURCE.index('          if [[ "$OPERATION" == *production-deployment-bindings || "$OPERATION" == migrate ]]; then')
        end = SOURCE.index('          printf', start)
        fragment = "\n".join(line[10:] for line in SOURCE[start:end].splitlines())
        for operation, current in [("migrate", True), ("apply-production-deployment-bindings", True), ("verify-production-deployment-bindings", True), ("list", False), ("rekey-openrouter-primary", False), ("deduplicate-groq", False)]:
            with self.subTest(operation=operation):
                with tempfile.TemporaryDirectory() as folder:
                    path = pathlib.Path(folder) / "config.json"
                    old = {"image": "registry/kaana@sha256:old", "sourceCommit": "old"}
                    path.write_text(json.dumps(old))
                    prefix = 'OPERATION=' + operation + '\nGITHUB_SHA=currentmain\nCONFIG_PATH=' + shlex.quote(str(path)) + '\nconfig=' + shlex.quote(json.dumps(old)) + '\naws(){ printf "%s" sha256:current; }\n'
                    result = subprocess.run(['bash', '-c', prefix + fragment + '\nprintf "%s" "$config"'], capture_output=True, text=True, check=True)
                    expected = {"image": "registry/kaana@sha256:current", "sourceCommit": "currentmain"} if current else old
                    self.assertEqual(json.loads(result.stdout), expected)
        self.assertIn("image=$(jq -r '.image' /tmp/credential-operation-resolved-config.json)", SOURCE)
        self.assertIn("source_commit=$(jq -r '.sourceCommit' /tmp/credential-operation-resolved-config.json)", SOURCE)
        self.assertIn('config=$(cat /tmp/credential-operation-resolved-config.json)', SOURCE)
        self.assertEqual(SOURCE.count('imageTag=$GITHUB_SHA'), 1)

    def test_migration_phase_executes_exact_guarded_command(self):
        start = SOURCE.index('          if [ "$OPERATION" = \'migrate\' ]; then')
        end = SOURCE.index('          if [ "$OPERATION" = \'bind-deployment\' ]; then', start)
        fragment = "\n".join(line[10:] for line in SOURCE[start:end].splitlines())
        for phase, expected in [('inspect', '["migrate","--reviewed-0020","--inspect-only"]'), ('apply', '["migrate","--reviewed-0020"]')]:
            result = subprocess.run(['bash', '-c', 'OPERATION=migrate\nMIGRATION_PHASE=' + phase + '\n' + fragment + '\nprintf "%s" "$command"'], capture_output=True, text=True, check=True)
            self.assertEqual(result.stdout, expected)
        rejected = subprocess.run(['bash', '-c', 'OPERATION=migrate\nMIGRATION_PHASE=wrong\n' + fragment], capture_output=True, text=True)
        self.assertNotEqual(rejected.returncode, 0)
        self.assertIn('default: inspect', SOURCE)

if __name__ == '__main__':
    unittest.main()

"""Offline workflow/supply-chain regressions; no container or registry access."""
from pathlib import Path
import re
import subprocess
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
MAIN_ONLY = "github.event_name == 'push' && github.ref == 'refs/heads/main'"


class CanonicalContract(unittest.TestCase):
    def setUp(self):
        self.text = (ROOT / '.github/workflows/canonical-image.yml').read_text()
        # BaseLoader preserves YAML 1.2 Actions 'on' rather than YAML 1.1 bool.
        self.workflow = yaml.load(self.text, Loader=yaml.BaseLoader)

    def test_triggers_and_publish_authority(self):
        workflow = self.workflow
        self.assertEqual(set(workflow['on']), {'pull_request', 'push', 'schedule', 'workflow_dispatch'})
        self.assertEqual(workflow['on']['push']['branches'], ['main'])
        paths = ['images/canonical/**', '.github/workflows/canonical-image.yml', 'scripts/verify-source']
        for trigger in ('push', 'pull_request'):
            self.assertEqual(workflow['on'][trigger]['paths'], paths)
        self.assertEqual(workflow['on']['schedule'], [{'cron': '23 4 * * 1'}])
        self.assertFalse(workflow['on']['workflow_dispatch'])
        self.assertEqual(workflow['permissions'], {'contents': 'read'})
        self.assertEqual(workflow['env'], {'IMAGE': 'ghcr.io/r-agatsuma/mcp-workspace-base'})
        self.assertEqual(set(workflow['jobs']), {'validate', 'publish'})
        validate, publish = (workflow['jobs'][name] for name in ('validate', 'publish'))
        self.assertNotIn('permissions', validate)
        self.assertNotIn('if', validate)
        self.assertEqual(publish['permissions'], {'contents': 'read', 'packages': 'write'})
        self.assertEqual(publish['if'], MAIN_ONLY)
        self.assertEqual(publish['needs'], 'validate')
        for job in (validate, publish):
            self.assertEqual(job['runs-on'], 'ubuntu-24.04-arm')
            self.assertIn('timeout-minutes', job)
        commands = '\n'.join(step.get('run', '') for step in validate['steps'])
        self.assertIn('docker build --platform linux/arm64 --no-cache --provenance=false --sbom=false', commands)
        self.assertIn('--build-arg "SOURCE_REVISION=$SOURCE_REVISION"', commands)
        self.assertIn('images/canonical/smoke.py --revision "$SOURCE_REVISION"', commands)
        self.assertNotRegex(commands, r'docker (push|login)|gh |ansible')
        for step in validate['steps']:
            if 'canonical.tar' in str(step) or step.get('with', {}).get('name') == 'canonical-validated-image':
                self.assertEqual(step['if'], MAIN_ONLY)
        publish_commands = '\n'.join(step.get('run', '') for step in publish['steps'])
        self.assertIn('docker image load --input "$RUNNER_TEMP/canonical.tar"', publish_commands)
        self.assertIn('docker push "$IMAGE:sha-$SOURCE_REVISION"', publish_commands)
        self.assertIn('"$IMAGE@$DIGEST"', publish_commands)
        self.assertIn('has("manifests") | not', publish_commands)
        # No branch checkout or rebuild in the privileged publication job.
        self.assertNotRegex(publish_commands, r'docker (build|run)\b|git |gh |ansible')
        self.assertNotIn('actions/checkout', str(publish))
        self.assertEqual(re.findall(r'secrets\.([A-Za-z_0-9]+)', self.text), ['GITHUB_TOKEN'])
        self.assertNotIn('secrets.', str(validate))
        self.assertNotIn('id-token', self.text)
        self.assertNotIn('pull_request_target', self.text)

    def test_actions_and_shell_syntax(self):
        allowed = {'actions/checkout', 'actions/upload-artifact', 'actions/download-artifact'}
        self.assertEqual(set(self.workflow), {'name', 'on', 'permissions', 'env', 'jobs'})
        for job in self.workflow['jobs'].values():
            self.assertLessEqual(set(job), {'runs-on', 'timeout-minutes', 'steps', 'permissions', 'if', 'needs'})
            for step in job['steps']:
                self.assertLessEqual(set(step), {'name', 'uses', 'with', 'run', 'env', 'if'})
                self.assertNotEqual('uses' in step, 'run' in step)
                if 'uses' in step:
                    name, sha = step['uses'].split('@')
                    self.assertIn(name, allowed)
                    self.assertRegex(sha, r'^[0-9a-f]{40}$')
                    if name == 'actions/checkout':
                        self.assertEqual(step['with']['persist-credentials'], 'false')
                if 'run' in step:
                    subprocess.run(['bash', '-n'], input=step['run'], text=True, check=True)

    def test_no_context_credentials_and_consistent_base(self):
        directory = ROOT / 'images/canonical'
        self.assertEqual([line for line in (directory / '.dockerignore').read_text().splitlines()
                          if line and not line.startswith('#')], ['**'])
        dockerfile = (directory / 'Dockerfile').read_text()
        base = re.search(r'FROM (\S+)', dockerfile)[1].split('@')[1]
        self.assertIn("BASE = '" + base + "'", (directory / 'smoke.py').read_text())
        # Bounded committed-material scan, independent of registry access.
        files = list(directory.glob('*')) + [ROOT / '.github/workflows/canonical-image.yml']
        for path in files:
            if not path.is_file():
                continue
            self.assertNotRegex(path.read_text(), r'-----BEGIN (?:OPENSSH |RSA |EC )?PRIVATE KEY-----|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|AKIA[0-9A-Z]{16}')


if __name__ == '__main__':
    unittest.main()

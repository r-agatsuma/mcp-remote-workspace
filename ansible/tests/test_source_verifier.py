"""Availability regression for #113; no dependency installs or host execution."""
import builtins
import contextlib
import io
from pathlib import Path
import runpy
import tempfile
import unittest
from unittest.mock import patch

import yaml

ROOT = Path(__file__).resolve().parents[2]


class Availability(unittest.TestCase):
    def test_yaml_runs_without_ansible_package_or_executable(self):
        verifier = runpy.run_path(str(ROOT / 'scripts/verify-source'))
        check = verifier['verify_deployment_source']
        real_import = builtins.__import__

        def missing_ansible(name, *args, **kwargs):
            if name == 'ansible':
                raise ImportError('synthetic unavailable Ansible')
            return real_import(name, *args, **kwargs)

        with tempfile.TemporaryDirectory(prefix='s3f-yaml-') as tmp:
            root = Path(tmp)
            path = root / 'example.yml'
            for missing in ('package', 'executable'):
                with self.subTest(missing=missing):
                    path.write_text('valid: [placeholder]\n')
                    output = io.StringIO()
                    importer = missing_ansible if missing == 'package' else real_import
                    with patch.dict(check.__globals__, ROOT=root), \
                            patch('builtins.__import__', side_effect=importer), \
                            patch.object(verifier['shutil'], 'which', return_value=None), \
                            patch.dict(check.__globals__, run=lambda *args: self.fail('Ansible must not execute')), \
                            contextlib.redirect_stdout(output):
                        check()
                    self.assertIn('PASS: repository YAML parsing', output.getvalue())
                    self.assertIn('UNSUPPORTED / NOT EXECUTED:', output.getvalue())
                    self.assertRegex(output.getvalue(), 'Ansible|ansible-playbook')
                    # Available YAML parse errors must propagate as a failure,
                    # even while Ansible is unavailable.
                    path.write_text('invalid: [\n')
                    with patch.dict(check.__globals__, ROOT=root), \
                            patch('builtins.__import__', side_effect=importer), \
                            patch.object(verifier['shutil'], 'which', return_value=None), \
                            self.assertRaises(yaml.YAMLError):
                        check()

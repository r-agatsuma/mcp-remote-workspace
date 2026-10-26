"""#152 deterministic local evidence. Synthetic identities; never contact hosts."""
import base64
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from deployment import manifest as schema

DEPLOY = runpy.run_path(str(ROOT / 'scripts/deploy'))
BOOTSTRAP = {
    'mcp_source_revision': 'a' * 40,
    'mcp_target_ssh_host_key_sha256': 'SHA256:' + base64.b64encode(bytes(32)).decode().rstrip('='),
    'mcp_data_device_id': '/dev/disk/by-id/scsi-SYNTHETIC_TEST_DISK',
    'mcp_data_expected_serial': 'SYNTHETIC_TEST_DISK',
    'mcp_data_expected_sector_bytes': 4096,
}
FINAL = BOOTSTRAP | {
    'mcp_data_uuid': '12345678-1234-1234-1234-123456789abc',
    'mcp_runner_subuid_start': 200000, 'mcp_runner_subgid_start': 300000,
    'mcp_host_public_ips': ['192.0.2.10', '2001:db8::10'],
    'mcp_tunnel_id': 'tunnel_' + '0' * 32,
}
LOCATOR = {'host': 'target.example.invalid', 'user': 'operator', 'port': 22}
OVERRIDES = {
    'mcp_data_expected_bytes': 1, 'mcp_runner_subid_count': 100000,
    'mcp_public_dns': ['9.9.9.9'], 'mcp_canonical_image': 'example.invalid/test:latest',
    'mcp_workspaced_config': {'idle-ttl': '1h', 'memory-bytes': 1},
    'mcp_production_endpoint': {'address': '127.0.0.1', 'port': 9999},
}


def dump(value):
    return yaml.safe_dump(value).encode()


class Manifest(unittest.TestCase):
    def test_valid_stages_and_distinct_byte_hashes(self):
        self.assertEqual(schema.validate(dump(BOOTSTRAP), 'bootstrap'), BOOTSTRAP)
        self.assertEqual(schema.validate(dump(FINAL), 'final'), FINAL)
        self.assertNotEqual(DEPLOY['sha'](dump(BOOTSTRAP)), DEPLOY['sha'](dump(FINAL)))
        # Comments/format are bound too; accepted semantics do not erase bytes.
        raw = dump(BOOTSTRAP)
        self.assertEqual(schema.validate(b'# comment\n' + raw, 'bootstrap'), BOOTSTRAP)
        self.assertNotEqual(DEPLOY['sha'](raw), DEPLOY['sha'](b'# comment\n' + raw))

    def test_structural_rejection_in_both_stages(self):
        cases = [b'', b'null', b'[]', b'scalar', b'{}\n---\n{}', b'a: 1\na: 2',
                 b'a:\n  b: 1\n  b: 2', b'a: {b: 1, b: 2}', b'<<: {a: 1}',
                 b'a: {"<<": {b: 1}}', b'a: &x 1', b'a: *x', b'a: !custom text',
                 b'a: !!str text', b'!<tag:yaml.org,2002:map> {}', b'a: [', b'a: 012',
                 b'a: 0x10', b'a: 1_000', b'a: 1:20', b'a: 1.0', b'a: true',
                 b'a: 2026-01-01', b'%YAML 1.1\n---\na: b', b'\xff', b'a: "{{ lookup }}"',
                 b'a: "{% x %}"', b'a: "line\\nbreak"', b'\xef\xbb\xbfa: b',
                 b'{? [a, b]: c}', b'a: {1: value}']
        for stage in ('bootstrap', 'final'):
            for raw in cases:
                with self.subTest(stage=stage, raw=raw), self.assertRaises(ValueError):
                    schema.validate(raw, stage)

    def test_nested_duplicates_are_rejected_before_unknown_key_schema(self):
        for raw in (b'unknown: {nested: 1, nested: 2}', b'unknown:\n  - {nested: 1, nested: 2}'):
            with self.assertRaisesRegex(ValueError, 'duplicate mapping key'):
                schema.parse(raw)

    def test_stage_allowlists_and_every_missing_field(self):
        for stage, good in [('bootstrap', BOOTSTRAP), ('final', FINAL)]:
            for key in good:
                changed = dict(good)
                del changed[key]
                with self.subTest(stage=stage, key=key), self.assertRaises(ValueError):
                    schema.validate(dump(changed), stage)
        for key in schema.FINAL - schema.BOOTSTRAP:
            with self.subTest(key=key), self.assertRaises(ValueError):
                schema.validate(dump(BOOTSTRAP | {key: FINAL[key]}), 'bootstrap')
        with self.assertRaises(ValueError):
            schema.validate(dump(BOOTSTRAP), 'final')

    def test_lexical_types(self):
        bad = {
            'mcp_source_revision': ['A' * 40, 'a' * 39, 'main', 1],
            'mcp_target_ssh_host_key_sha256': ['SHA256:' + 'a' * 43, 'SHA256:' + 'A' * 43 + '=', 'MD5:abc', 1],
            'mcp_data_device_id': ['/dev/sdb', '/dev/disk/by-id/disk-part1', '/dev/disk/by-id/..', '/dev/disk/by-id/a/b'],
            'mcp_data_expected_serial': ['', ' leading', 123, '{{ secret }}'],
            'mcp_data_expected_sector_bytes': [0, 1024, '512', True, 512.0],
            'mcp_data_uuid': ['bad', '12345678-1234-1234-1234-123456789ABC'],
            'mcp_runner_subuid_start': [0, -1, True, '200000', 2**32 - 65536],
            'mcp_runner_subgid_start': [0, -1, 1.5, 2**32],
            'mcp_host_public_ips': [[], '192.0.2.10', ['example.invalid'], ['192.0.2.01'],
                                    ['::ffff:192.0.2.10'], ['fe80::1%eth0'], ['192.0.2.10', '192.0.2.10']],
            'mcp_tunnel_id': ['tunnel_' + 'A' * 32, 'tunnel_123', 1],
        }
        for key, values in bad.items():
            for value in values:
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    schema.validate(dump(FINAL | {key: value}), 'final')

    def test_exhaustive_policy_and_secret_rejection(self):
        keys = OVERRIDES | {'api_key': 'FORBIDDEN_FIELD', 'mcp_tunnel_api_key': 'FORBIDDEN_FIELD',
                            'mcp_data_bootstrap_enabled': True, 'ansible_ssh_common_args': 'options',
                            'vars_files': ['alternate.yml'], 'unknown': 1}
        for key, value in keys.items():
            for stage, good in [('bootstrap', BOOTSTRAP), ('final', FINAL)]:
                with self.subTest(stage=stage, key=key), self.assertRaises(ValueError):
                    schema.validate(dump(good | {key: value}), stage)

    def test_locator_has_no_variable_or_connection_escape_hatch(self):
        self.assertEqual(schema.locator(dump(LOCATOR)), LOCATOR)
        for key, value in OVERRIDES.items():
            with self.assertRaises(ValueError):
                schema.locator(dump(LOCATOR | {key: value}))
        for change in ({'ansible_connection': 'local'}, {'host': '-oProxyCommand=bad'},
                       {'host': '{{ value }}'}, {'port': '22'}, {'user': 'root; command'},
                       {'all': {'vars': OVERRIDES}}, {'vars_files': ['alternate.yml']}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                schema.locator(dump(LOCATOR | change))


class Invocation(unittest.TestCase):
    def test_regular_private_file_boundary(self):
        with tempfile.TemporaryDirectory(prefix='private-input-') as temporary:
            root = Path(temporary)
            path = root / 'input.yml'
            path.write_bytes(dump(BOOTSTRAP))
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                DEPLOY['read_input'](path, private=True)
            path.chmod(0o600)
            self.assertEqual(DEPLOY['read_input'](path, private=True), dump(BOOTSTRAP))
            link = root / 'link'
            link.symlink_to(path)
            with self.assertRaises(OSError):
                DEPLOY['read_input'](link)

    def test_no_arbitrary_extra_vars_or_flags(self):
        base = ['validate', 'bootstrap', '--manifest', 'synthetic.yml', '--inventory', 'synthetic-locator.yml']
        for args in [['-e', 'x=y'], ['--extra-vars', '@anything'], ['--vars-file', 'anything'],
                     ['--tags', 's3e'], ['--skip-tags', 'gate'], ['--manifest-sha', 'a' * 64],
                     ['--', '--extra-vars', 'x=y']]:
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                DEPLOY['arguments'](base + args)
        for args in [['--final-observation', 'observation.yml']]:
            with self.assertRaises(ValueError):
                DEPLOY['arguments'](base + args)
        with self.assertRaises(ValueError):
            DEPLOY['arguments'](['run', *base[1:]])

    def test_actual_git_revision_cleanliness_and_snapshot_isolation(self):
        with tempfile.TemporaryDirectory(prefix='manifest-git-') as temporary:
            root = Path(temporary) / 'repo'
            root.mkdir()
            def git(*args):
                return subprocess.check_output(['git', '-C', str(root), *args], stderr=subprocess.DEVNULL)
            git('init')
            (root / 'source').write_text('reviewed\n')
            git('add', 'source')
            git('-c', 'user.name=Source Test', '-c', 'user.email=test@example.invalid', 'commit', '-m', 'fixture')
            revision = git('rev-parse', 'HEAD').decode().strip()
            DEPLOY['source_binding'](root, revision)
            with self.assertRaisesRegex(ValueError, 'HEAD differs'):
                DEPLOY['source_binding'](root, 'f' * 40)
            (root / 'source').write_text('dirty\n')
            with self.assertRaisesRegex(ValueError, 'dirty'):
                DEPLOY['source_binding'](root, revision)
            git('add', 'source')
            with self.assertRaisesRegex(ValueError, 'dirty'):
                DEPLOY['source_binding'](root, revision)
            # Archive uses committed bytes even when a caller has untracked vars.
            (root / 'host_vars').mkdir()
            (root / 'host_vars/anything').write_text('override')
            snapshot = Path(temporary) / 'snapshot'
            DEPLOY['snapshot'](root, revision, snapshot)
            self.assertEqual((snapshot / 'source').read_text(), 'reviewed\n')
            self.assertFalse((snapshot / 'host_vars').exists())

    def test_wrong_stage_and_changed_bytes_stop_before_network(self):
        with tempfile.TemporaryDirectory(prefix='manifest-input-') as temporary:
            root = Path(temporary)
            manifest = root / 'manifest.yml'
            inventory = root / 'locator.yml'
            manifest.write_bytes(dump(BOOTSTRAP))
            inventory.write_bytes(dump(LOCATOR))
            main = DEPLOY['main']
            with patch.dict(main.__globals__, source_binding=lambda *args: None), \
                    patch.object(DEPLOY['socket'], 'getaddrinfo') as network:
                for stage, extra in [('final', []), ('bootstrap', ['--manifest-sha256', 'f' * 64]),
                                     ('bootstrap', ['--inventory-sha256', 'f' * 64])]:
                    with self.assertRaises(ValueError):
                        main(['validate', stage, '--manifest', str(manifest), '--inventory', str(inventory), *extra])
                network.assert_not_called()

    def test_grant_names_exact_candidate_and_device(self):
        grant = {'authorize': 'format-blank-data', 'source_revision': BOOTSTRAP['mcp_source_revision'],
                 'manifest_sha256': 'a' * 64, 'inventory_sha256': 'b' * 64,
                 'host_key_sha256': BOOTSTRAP['mcp_target_ssh_host_key_sha256'],
                 'data_device_id': BOOTSTRAP['mcp_data_device_id']}
        DEPLOY['bootstrap_grant'](dump(grant), BOOTSTRAP, 'a' * 64, 'b' * 64)
        for key in grant:
            with self.subTest(key=key), self.assertRaises(ValueError):
                DEPLOY['bootstrap_grant'](dump(grant | {key: 'WRONG'}), BOOTSTRAP, 'a' * 64, 'b' * 64)

    def test_final_observation_binding_and_set_equality(self):
        record = {'manifest_sha256': 'a' * 64, 'inventory_sha256': 'b' * 64,
                  'host_key_sha256': FINAL['mcp_target_ssh_host_key_sha256'],
                  'public_ips': list(reversed(FINAL['mcp_host_public_ips'])),
                  'observed_at': '2026-01-01T00:00:00Z',
                  'provider_evidence_sha256': 'c' * 64, 'guest_evidence_sha256': 'd' * 64}
        DEPLOY['final_observation'](dump(record), FINAL, 'a' * 64, 'b' * 64)
        for change in ({'public_ips': ['192.0.2.11']}, {'manifest_sha256': 'c' * 64},
                       {'host_key_sha256': 'wrong'}, {'observed_at': '2999-01-01T00:00:00Z'},
                       {'observed_at': '2026-01-40T00:00:00Z'}, {'guest_evidence_sha256': ''}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                DEPLOY['final_observation'](dump(record | change), FINAL, 'a' * 64, 'b' * 64)

    def test_pin_is_authenticated_identity_not_locator_or_tofu(self):
        blob = base64.b64encode(b'synthetic public host key').decode()
        expected = 'SHA256:' + base64.b64encode(hashlib.sha256(b'synthetic public host key').digest()).decode().rstrip('=')
        scan = '192.0.2.10 ssh-ed25519 ' + blob + '\n'
        self.assertEqual(DEPLOY['pinned_key'](scan, expected), 'ssh-ed25519 ' + blob)
        with self.assertRaises(ValueError):
            DEPLOY['pinned_key'](scan, BOOTSTRAP['mcp_target_ssh_host_key_sha256'])
        options = DEPLOY['connection_options'](Path('/synthetic/known_hosts'))
        for option in ('StrictHostKeyChecking=yes', 'ControlMaster=no', 'ControlPath=none',
                       'GlobalKnownHostsFile=/dev/null', 'UpdateHostKeys=no', 'VerifyHostKeyDNS=no',
                       'IdentityFile=none', 'ForwardAgent=no'):
            self.assertIn(option, options)
        self.assertEqual(options[:2], ['-F', '/dev/null'])

    def test_formal_run_uses_bound_snapshot_inputs_and_replaced_environment(self):
        # Mock only external execution, not input validation/normalization or
        # grant checks. This is invocation construction evidence, not SSH/E2E.
        blob = base64.b64encode(b'synthetic public host key').decode()
        expected = 'SHA256:' + base64.b64encode(hashlib.sha256(b'synthetic public host key').digest()).decode().rstrip('=')
        manifest = BOOTSTRAP | {'mcp_target_ssh_host_key_sha256': expected}
        raw = dump(manifest)
        mh, ih = DEPLOY['sha'](raw), DEPLOY['sha'](dump(LOCATOR))
        grant = {'authorize': 'format-blank-data', 'source_revision': manifest['mcp_source_revision'],
                 'manifest_sha256': mh, 'inventory_sha256': ih, 'host_key_sha256': expected,
                 'data_device_id': manifest['mcp_data_device_id']}
        with tempfile.TemporaryDirectory(prefix='run-construction-') as temporary:
            root = Path(temporary)
            for name, content in [('manifest.yml', raw), ('locator.yml', dump(LOCATOR)), ('grant.yml', dump(grant))]:
                DEPLOY['private_write'](root / name, content)
            def snapshot(source, revision, destination):
                (destination / 'ansible').mkdir(parents=True)
            calls = []
            def execute(command, **kwargs):
                calls.append(command[0])
                env = kwargs['env']
                for key in ('ANSIBLE_INVENTORY', 'ANSIBLE_SSH_ARGS', 'ANSIBLE_CALLBACK_PLUGINS', 'PYTHONPATH'):
                    self.assertNotIn(key, env)
                self.assertNotEqual(env['HOME'], os.environ.get('HOME'))
                if command[0] == 'ssh-keyscan':
                    # Changing original files after reading cannot affect invocation.
                    (root / 'manifest.yml').write_bytes(dump(manifest | OVERRIDES))
                    return subprocess.CompletedProcess(command, 0, stdout='192.0.2.10 ssh-ed25519 ' + blob + '\n')
                if command[0] == 'ssh':
                    self.assertIn('StrictHostKeyChecking=yes', command)
                else:
                    self.assertEqual(command[0], '/synthetic/ansible-playbook')
                    self.assertEqual(command[3], 'bootstrap-data.yml')
                    variables = json.loads(Path(command[-1][1:]).read_bytes())
                    self.assertEqual(variables, manifest | {'mcp_data_bootstrap_enabled': True})
                    inv = json.loads(Path(command[2]).read_bytes())
                    target = inv['all']['children']['mcp_hosts']['hosts']['mcp_target']
                    self.assertEqual(target['ansible_host'], '192.0.2.10')
                    self.assertEqual(target['ansible_connection'], 'ssh')
                return subprocess.CompletedProcess(command, 0)
            main = DEPLOY['main']
            previous_umask = os.umask(0o077)
            try:
                with patch.dict(main.__globals__, source_binding=lambda *args: None, snapshot=snapshot), \
                        patch.object(DEPLOY['shutil'], 'which', return_value='/synthetic/ansible-playbook'), \
                        patch.object(DEPLOY['socket'], 'getaddrinfo', return_value=[(2, 1, 6, '', ('192.0.2.10', 22))]), \
                        patch.object(DEPLOY['subprocess'], 'run', side_effect=execute), \
                        patch.dict(os.environ, ANSIBLE_INVENTORY='bad', ANSIBLE_SSH_ARGS='bad',
                                   ANSIBLE_CALLBACK_PLUGINS='bad', PYTHONPATH='bad'), \
                        contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(main(['run', 'bootstrap', '--manifest', str(root / 'manifest.yml'),
                                           '--inventory', str(root / 'locator.yml'), '--manifest-sha256', mh,
                                           '--inventory-sha256', ih, '--bootstrap-grant', str(root / 'grant.yml'),
                                           '--evidence-dir', str(root / 'evidence')]), 0)
            finally:
                os.umask(previous_umask)
            self.assertEqual(calls, ['ssh-keyscan', 'ssh', '/synthetic/ansible-playbook'])
            self.assertEqual((root / 'evidence/manifest.yml').read_bytes(), raw)
            self.assertEqual(json.loads((root / 'evidence/result.json').read_bytes()), {'exit_code': 0})

    def test_final_run_cannot_bypass_operator_freshness_with_noninteractive_input(self):
        with tempfile.TemporaryDirectory(prefix='freshness-') as temporary:
            root = Path(temporary)
            mh, ih = DEPLOY['sha'](dump(FINAL)), DEPLOY['sha'](dump(LOCATOR))
            record = {'manifest_sha256': mh, 'inventory_sha256': ih,
                      'host_key_sha256': FINAL['mcp_target_ssh_host_key_sha256'],
                      'public_ips': FINAL['mcp_host_public_ips'], 'observed_at': '2026-01-01T00:00:00Z',
                      'provider_evidence_sha256': 'c' * 64, 'guest_evidence_sha256': 'd' * 64}
            for name, value in [('manifest.yml', FINAL), ('locator.yml', LOCATOR), ('observation.yml', record)]:
                DEPLOY['private_write'](root / name, dump(value))
            main = DEPLOY['main']
            with patch.dict(main.__globals__, source_binding=lambda *args: None), \
                    patch.object(DEPLOY['sys'].stdin, 'isatty', return_value=False), \
                    patch.object(DEPLOY['socket'], 'getaddrinfo') as network:
                with self.assertRaisesRegex(ValueError, 'consumption-time'):
                    main(['run', 'final', '--manifest', str(root / 'manifest.yml'),
                          '--inventory', str(root / 'locator.yml'), '--manifest-sha256', mh,
                          '--inventory-sha256', ih, '--final-observation', str(root / 'observation.yml'),
                          '--evidence-dir', str(root / 'evidence')])
                network.assert_not_called()


class SourceBoundary(unittest.TestCase):
    def test_stage_preflight_precedes_roles_and_preserves_destructive_separation(self):
        for filename, stage in [('site.yml', 'final'), ('bootstrap-data.yml', 'bootstrap')]:
            play = yaml.safe_load((ROOT / 'ansible' / filename).read_text())[0]
            self.assertEqual(play['vars']['mcp_deployment_stage'], stage)
            self.assertEqual(play['pre_tasks'][0]['ansible.builtin.import_tasks'], 'tasks/deployment_preflight.yml')
        defaults = yaml.safe_load((ROOT / 'ansible/group_vars/all.yml').read_text())
        self.assertEqual(defaults['mcp_data_expected_bytes'], 107374182400)
        self.assertEqual(defaults['mcp_runner_subid_count'], 65536)
        self.assertEqual(defaults['mcp_data_expected_sector_bytes'], 0)
        self.assertEqual(defaults['mcp_public_dns'], ['8.8.8.8', '8.8.4.4'])
        self.assertEqual(defaults['mcp_canonical_image'], 'ghcr.io/r-agatsuma/mcp-workspace-base@sha256:8af2148405bfa66bfd522d820145ccd7a19ca9f9cfe39ee609861d082b1680d8')
        for path, stage in [('bootstrap-manifest.example.yml', 'bootstrap'), ('deployment-manifest.example.yml', 'final')]:
            raw = (ROOT / 'ansible/inventory' / path).read_bytes()
            self.assertEqual(set(schema.parse(raw)), schema.BOOTSTRAP if stage == 'bootstrap' else schema.FINAL)
            with self.assertRaises(ValueError):
                schema.validate(raw, stage)

    def test_rendered_evidence_hashes_only_explicit_nonsecret_files(self):
        tasks = yaml.safe_load((ROOT / 'ansible/tasks/deployment_configuration_evidence.yml').read_text())
        action = tasks[0]['ansible.builtin.stat']
        self.assertEqual(action['checksum_algorithm'], 'sha256')
        self.assertIs(action['follow'], False)
        paths = tasks[0]['loop']
        self.assertIn('/etc/mcp-tunnel/tunnel.yaml', paths)
        self.assertNotIn('/etc/mcp-tunnel/control-plane-api-key', paths)
        self.assertTrue(all('*' not in path and '{{' not in path for path in paths))

    def test_effective_extra_var_policy_overrides_fail_assertion(self):
        from test_s3e_contract import ansible
        gate = yaml.safe_load((ROOT / 'ansible/tasks/deployment_preflight.yml').read_text())[0]
        values = yaml.safe_load((ROOT / 'ansible/group_vars/all.yml').read_text())
        with tempfile.TemporaryDirectory(prefix='policy-assert-') as temporary:
            root = Path(temporary)
            play = [{'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
                     'vars': values, 'tasks': [gate]}]
            result = ansible(play, root)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            for key in list(OVERRIDES)[:4]:
                extra = root / 'extra.json'
                extra.write_text(json.dumps({key: OVERRIDES[key]}))
                result = subprocess.run(['ansible-playbook', '-i', 'localhost,', str(root / 'render.yml'),
                                         '--extra-vars', '@' + str(extra)], cwd=ROOT / 'ansible',
                                        capture_output=True, text=True, timeout=30)
                self.assertNotEqual(result.returncode, 0, key)
                self.assertIn('source-owned v0 reference policy', result.stdout)

    def test_reference_policy_renders_through_actual_consumers(self):
        from test_s3e_contract import ansible, fixture
        values = fixture()
        defaults = yaml.safe_load((ROOT / 'ansible/group_vars/all.yml').read_text())
        values.update({key: defaults[key] for key in ('mcp_canonical_image', 'mcp_public_dns',
                                                      'mcp_data_expected_bytes', 'mcp_runner_subid_count')})
        values.update(mcp_public_dns_validated=defaults['mcp_public_dns'],
                      mcp_data_uuid=FINAL['mcp_data_uuid'], mcp_data_expected_sector_bytes=4096)
        with tempfile.TemporaryDirectory(prefix='policy-render-') as temporary:
            root = Path(temporary)
            tasks = [
                {'ansible.builtin.import_tasks': str(ROOT / 'ansible/roles/s3c/tasks/interface.yml')},
                {'ansible.builtin.import_tasks': str(ROOT / 'ansible/roles/s3e/tasks/configuration.yml')},
                {'ansible.builtin.copy': {'content': '{{ mcp_workspaced_argv | to_json }}', 'dest': str(root / 'argv.json')}},
                {'ansible.builtin.template': {'src': str(ROOT / 'ansible/roles/s3b/templates/data-verify.py.j2'),
                                              'dest': str(root / 'data-verify')}}]
            result = ansible([{'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
                               'vars': values, 'tasks': tasks}], root)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            argv = json.loads((root / 'argv.json').read_text())
            self.assertEqual(argv[argv.index('-image') + 1], defaults['mcp_canonical_image'])
            self.assertEqual([argv[index + 1] for index, arg in enumerate(argv) if arg == '-public-dns'], defaults['mcp_public_dns'])
            self.assertIn('107374182400', (root / 'data-verify').read_text())


if __name__ == '__main__':
    unittest.main()

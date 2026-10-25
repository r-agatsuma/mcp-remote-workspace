"""Source/component evidence only. No credential content or real service operation."""
import ast
from copy import deepcopy
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import stat
import subprocess
import tempfile
import types
import unittest
from unittest.mock import Mock, patch
import zipfile
from contextlib import ExitStack

import yaml

ROOT = Path(__file__).resolve().parents[1]
ROLE = ROOT / 'roles/s3e'
TEMPLATES = ROLE / 'templates'


def fixture(uid=947, port=8080):
    values = yaml.safe_load((ROOT / 'group_vars/all.yml').read_text())
    runtime = next(task['ansible.builtin.set_fact']['mcp_runtime_environment']
                   for task in yaml.safe_load((ROOT / 'roles/s3b/tasks/main.yml').read_text())
                   if 'mcp_runtime_environment' in task.get('ansible.builtin.set_fact', {}))
    values.update(mcp_runner_account='mcp-runner', mcp_tunnel_account='mcp-tunnel',
                  mcp_runner_uid=uid, mcp_runner_primary_gid=uid + 1,
                  mcp_runtime_environment=runtime,
                  mcp_data_mount_unit=r'srv-mcp\x2dpodman.mount',
                  mcp_data_ready_unit='mcp-data-ready.service',
                  mcp_public_dns_validated=['9.9.9.9', '2606:4700:4700::1111'],
                  mcp_canonical_image='example.invalid/workspace@sha256:' + 'a' * 64,
                  mcp_tunnel_id='tunnel_' + '0' * 32,
                  mcp_production_endpoint={'address': '127.0.0.1', 'port': port})
    return values


def ansible(play, directory):
    path = Path(directory) / 'render.yml'
    path.write_text(yaml.safe_dump(play, sort_keys=False))
    return subprocess.run(['ansible-playbook', '-i', 'localhost,', str(path)],
                          cwd=ROOT, env={**os.environ, 'ANSIBLE_NOCOLOR': '1'},
                          text=True, capture_output=True, timeout=180)


def module(path):
    value = types.ModuleType(path.stem)
    exec(compile(path.read_text(), str(path), 'exec'), value.__dict__)
    return value


@unittest.skipUnless(shutil.which('ansible-playbook'), 'ansible-playbook unavailable')
class RenderedContract(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix='s3e-render-')
        cls.directory = Path(cls.temp.name)
        plays = []
        for uid, port in [(947, 8080), (20481, 18080)]:
            output = cls.directory / str(uid)
            output.mkdir()
            tasks = [
                {'ansible.builtin.import_tasks': str(ROOT / 'roles/s3c/tasks/interface.yml')},
                {'ansible.builtin.import_tasks': str(ROLE / 'tasks/configuration.yml')},
                {'ansible.builtin.copy': {'content': '{{ mcp_workspaced_config | to_json }}',
                                         'dest': str(output / 'config.json')}},
                {'ansible.builtin.template': {'src': str(TEMPLATES / '{{ item }}'),
                                              'dest': str(output / "{{ item | regex_replace('\\.j2$', '') }}")},
                 'loop': sorted(p.name for p in TEMPLATES.glob('*.j2'))}]
            values = fixture(uid, port)
            if uid == 20481:
                values['mcp_workspaced_config'].update({'memory-bytes': 3221225472, 'cpu-weight': 200,
                                                       'max-generations': 3, 'idle-ttl': '45m'})
            plays.append({'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
                          'become': False, 'vars': values, 'tasks': tasks})
        result = ansible(plays, cls.directory)
        if result.returncode:
            raise AssertionError(result.stdout + result.stderr)

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    def test_shared_config_complete_and_daemon_recovery_equal(self):
        cli = (ROOT.parent / 'cmd/mcp-workspaced/main.go').read_text()
        for uid, port in [(947, 8080), (20481, 18080)]:
            output = self.directory / str(uid)
            recovery = module(output / 'recover.py')
            config = json.loads((output / 'config.json').read_text())
            unit = (output / 'mcp-workspaced.service').read_text()
            command = shlex.split(next(l.removeprefix('ExecStart=') for l in unit.splitlines()
                                       if l.startswith('ExecStart=')))
            self.assertEqual(command[:4], ['/usr/local/bin/mcp-workspaced', '-daemon',
                                          '-mcp-listen', f'127.0.0.1:{port}'])
            self.assertEqual(command[4:], recovery.SHARED_ARGV)
            effective = {}
            for flag, value in zip(command[4::2], command[5::2]):
                effective.setdefault(flag[1:], []).append(value)
            self.assertEqual(effective, {key: value if isinstance(value, list) else [str(value)]
                                         for key, value in config.items()})
            for key in effective:
                self.assertIn('"' + key + '"', cli)
            self.assertEqual(recovery.ENVIRONMENT['XDG_RUNTIME_DIR'], f'/run/user/{uid}')
            self.assertEqual(recovery.ENVIRONMENT['DBUS_SESSION_BUS_ADDRESS'], f'unix:path=/run/user/{uid}/bus')
            self.assertIn(f'BindsTo=srv-mcp\\x2dpodman.mount mcp-data-ready.service user@{uid}.service netfilter-persistent.service', unit)
            self.assertIn('User=mcp-runner\nGroup=mcp-runner', unit)
            self.assertIn('Slice=system.slice', unit)
            self.assertIn('StandardOutput=null\nStandardError=null', unit)
            self.assertIn('StartLimitBurst=3', unit)
            self.assertIn('TimeoutStopSec=15s', unit)

    def canonical(self, uid=947):
        return module(self.directory / str(uid) / 'canonical-image.py')

    def canonical_execution(self, helper, state, calls):
        """Podman process double only; all convergence decisions use rendered source."""
        def execute(command, **kwargs):
            calls.append(command)
            self.assertEqual(kwargs['env'], helper.ENVIRONMENT)
            self.assertEqual(kwargs['stdin'], subprocess.DEVNULL)
            if command == ['/usr/bin/python3', '-I', '/usr/local/lib/mcp-workspaced/service-prerequisites']:
                if state.get('prerequisite_failed'):
                    raise subprocess.CalledProcessError(1, command)
                return types.SimpleNamespace(stdout='', returncode=0)
            self.assertEqual(command[:2], ['/usr/bin/podman', '--remote=false'])
            args = command[2:]
            if args == ['info', '--format=json']:
                info = {'host': {'security': {'rootless': True}}, 'store': {
                    'graphRoot': '/srv/mcp-podman/runner/.local/share/containers/storage',
                    'runRoot': helper.ENVIRONMENT['XDG_RUNTIME_DIR'] + '/containers',
                    'configFile': '/srv/mcp-podman/runner/.config/containers/storage.conf'}}
                info['store'].update(state.get('store', {}))
                if state.get('rootful'):
                    info['host']['security']['rootless'] = False
                return types.SimpleNamespace(stdout=json.dumps(info), returncode=0)
            if args == ['image', 'exists', '--', helper.IMAGE]:
                return types.SimpleNamespace(stdout='', returncode=state.get('exists_rc', 0 if state['present'] else 1))
            if args[0] == 'pull':
                self.assertEqual(args[-2:], ['--', helper.IMAGE])
                self.assertEqual(len(args), 6)
                authfile = Path(args[1].removeprefix('--authfile='))
                certs = Path(args[2].removeprefix('--cert-dir='))
                with authfile.open() as stream:
                    self.assertEqual(json.load(stream), {'auths': {}})
                self.assertEqual(stat.S_IMODE(authfile.stat().st_mode), 0o600)
                self.assertEqual(stat.S_IMODE(authfile.parent.stat().st_mode), 0o700)
                self.assertEqual(certs.parent, authfile.parent)
                self.assertEqual(list(certs.iterdir()), [])
                self.assertEqual(stat.S_IMODE(certs.stat().st_mode), 0o700)
                self.assertEqual(args[3], '--tls-verify=true')
                self.assertEqual(kwargs['timeout'], 1800)
                if state.get('pull_failed'):
                    raise subprocess.CalledProcessError(1, command)
                state['present'] = True
                return types.SimpleNamespace(stdout='b' * 64, returncode=0)
            self.assertEqual(args[:3], ['image', 'inspect', '--'])
            self.assertIn(args[3], [helper.IMAGE, 'sha256:' + 'b' * 64])
            if state.get('inspect_failed'):
                raise subprocess.CalledProcessError(125, command)
            if 'inspection_raw' in state:
                return types.SimpleNamespace(stdout=state['inspection_raw'], returncode=0)
            record = {'Id': 'b' * 64, 'Digest': 'sha256:' + 'a' * 64, 'RepoDigests': [helper.IMAGE]}
            record.update(state.get('inspection', {}))
            if args[3].startswith('sha256:'):
                record.update(state.get('by_id', {}))
            return types.SimpleNamespace(stdout=json.dumps([record]), returncode=0)
        return execute

    def canonical_context(self, helper, state, calls, directory):
        stack = ExitStack()
        for name, value in [('getuid', helper.UID), ('geteuid', helper.UID), ('getgid', helper.GID),
                            ('getgroups', [helper.GID])]:
            stack.enter_context(patch.object(helper.os, name, return_value=value))
        stack.enter_context(patch.dict(helper.ENVIRONMENT, TMPDIR=directory))
        stack.enter_context(patch.object(helper.subprocess, 'run',
                                        side_effect=self.canonical_execution(helper, state, calls)))
        # Registry routing files only. Opening any ambient auth file is forbidden.
        stack.enter_context(patch.object(helper.Path, 'read_text', side_effect=FileNotFoundError))
        stack.enter_context(patch.object(helper.Path, 'exists', return_value=False))
        return stack

    def test_canonical_present_and_absent_then_repeat_are_idempotent(self):
        for uid in [947, 20481]:
            for initially_present in [True, False]:
                helper = self.canonical(uid)
                state, calls = {'present': initially_present}, []
                with tempfile.TemporaryDirectory() as directory, \
                     self.canonical_context(helper, state, calls, directory), \
                     patch.dict(os.environ, {'HOME': '/root', 'REGISTRY_AUTH_FILE': '/ambient/auth',
                                             'DOCKER_CONFIG': '/ambient/docker', 'CONTAINER_HOST': 'ssh://other',
                                             'HTTPS_PROXY': 'http://other'}):
                    first = helper.converge()
                    self.assertEqual(first, {'changed': not initially_present, 'reference': helper.IMAGE,
                                             'image_id': 'sha256:' + 'b' * 64, 'digest': 'sha256:' + 'a' * 64})
                    second = helper.converge()
                    self.assertFalse(second['changed'])
                    self.assertEqual(sum('pull' in c for c in calls), int(not initially_present))
                    for ref in [helper.IMAGE, first['image_id']]:
                        self.assertEqual(sum(c[2:] == ['image', 'inspect', '--', ref] for c in calls), 2)
                    self.assertEqual(list(Path(directory).iterdir()), [])

    def test_canonical_failure_states_stop_without_acquisition_fallback(self):
        helper = self.canonical()
        cases = [
            {'prerequisite_failed': True}, {'rootful': True}, {'exists_rc': 125},
            {'store': {'graphRoot': '/root/.local/share/containers/storage'}},
            {'store': {'runRoot': '/run/user/1/containers'}},
            {'store': {'configFile': '/etc/containers/storage.conf'}},
            {'inspect_failed': True}, {'inspection_raw': 'not JSON'}, {'inspection_raw': '[]'},
            {'inspection_raw': '[{}, {}]'}, {'inspection_raw': '{}'},
            {'inspection': {'Id': 'short'}}, {'inspection': {'Digest': ''}},
            {'inspection': {'RepoDigests': []}}, {'inspection': {'RepoDigests': ['other@sha256:' + 'a' * 64]}},
            {'by_id': {'Id': 'c' * 64}}, {'by_id': {'Digest': 'sha256:' + 'c' * 64}},
            {'by_id': {'RepoDigests': []}},
        ]
        for absent in [False, True]:
            for failure in cases + ([{'pull_failed': True}] if absent else []):
                with self.subTest(absent=absent, failure=failure), tempfile.TemporaryDirectory() as directory:
                    state, calls = {'present': not absent, **failure}, []
                    with self.canonical_context(helper, state, calls, directory), \
                         self.assertRaises((ValueError, subprocess.CalledProcessError)):
                        helper.converge()
                    self.assertLessEqual(sum('pull' in c for c in calls), int(absent))
                    if set(failure) & {'prerequisite_failed', 'rootful', 'exists_rc', 'store'}:
                        self.assertFalse(any('pull' in c for c in calls))

    def test_canonical_rejects_wrong_identity_and_ambiguous_inputs_before_podman(self):
        helper = self.canonical()
        for identity in [('getuid', 0), ('geteuid', 0), ('getgid', 0), ('getgroups', [helper.GID, 0])]:
            with tempfile.TemporaryDirectory() as directory:
                calls = []
                with self.canonical_context(helper, {'present': True}, calls, directory), \
                     patch.object(helper.os, identity[0], return_value=identity[1]), self.assertRaises(ValueError):
                    helper.converge()
                self.assertEqual(calls, [])
        for reference in ['example.invalid/workspace:latest', 'workspace@sha256:' + 'a' * 64,
                          'ns/workspace@sha256:' + 'a' * 64, helper.IMAGE + '\n', helper.IMAGE.upper(),
                          'docker://' + helper.IMAGE, 'dir:/tmp/image@sha256:' + 'a' * 64,
                          'oci-archive:/tmp/image@sha256:' + 'a' * 64]:
            with patch.object(helper, 'IMAGE', reference), patch.object(helper.subprocess, 'run') as run, \
                 self.assertRaises(ValueError):
                helper.repository_digest()
            run.assert_not_called()

    def test_canonical_multiarch_index_and_optional_tag_keep_exact_digest_binding(self):
        helper = self.canonical()
        index = helper.IMAGE
        expected = helper.repository_digest()
        with patch.object(helper, 'IMAGE', index.replace('@', ':policy@')):
            self.assertEqual(helper.repository_digest(), expected)
        with tempfile.TemporaryDirectory() as directory:
            state = {'present': True, 'inspection': {'Digest': 'sha256:' + 'c' * 64,
                     'RepoDigests': [index, index.split('@')[0] + '@sha256:' + 'c' * 64]}}
            with self.canonical_context(helper, state, [], directory):
                self.assertEqual(helper.converge()['digest'], 'sha256:' + 'c' * 64)

    def test_canonical_credential_helpers_fail_closed_without_reading_auth(self):
        helper = self.canonical()
        config_paths = ['/etc/containers/registries.conf',
                        '/srv/mcp-podman/runner/.config/containers/registries.conf',
                        '/etc/containers/registries.conf.d/custom.conf',
                        '/srv/mcp-podman/runner/.config/containers/registries.conf.d/custom.conf']
        for target in config_paths:
            for config, rejects in [('', False), ('credential-helpers = []', False),
                                     ('credential-helpers = ["containers-auth.json"]', False),
                                     ('credential-helpers = ["secretservice"]', True), ('not toml', True)]:
                def read(path):
                    self.assertIn(str(path), config_paths)
                    return config if str(path) == target else ''
                with self.subTest(target=target, config=config), \
                     patch.object(helper.Path, 'read_text', read), \
                     patch.object(helper.Path, 'exists', return_value=True), \
                     patch.object(helper.Path, 'glob', lambda p, pattern: [p / 'custom.conf']):
                    if rejects:
                        with self.assertRaises(ValueError):
                            helper.verify_credential_helpers()
                    else:
                        helper.verify_credential_helpers()
        with tempfile.TemporaryDirectory() as directory:
            calls = []
            with self.canonical_context(helper, {'present': False}, calls, directory), \
                 patch.object(helper, 'verify_credential_helpers', side_effect=ValueError('credentials')), \
                 self.assertRaises(ValueError):
                helper.converge()
            self.assertFalse(any('pull' in c for c in calls))

    def test_canonical_effective_credential_helper_keys_gate_acquisition(self):
        # Exercise the rendered production converge() path, including its actual
        # TOML gate. Only host observations and Podman processes are doubled.
        config_paths = ['/etc/containers/registries.conf',
                        '/srv/mcp-podman/runner/.config/containers/registries.conf',
                        '/etc/containers/registries.conf.d/custom.conf',
                        '/srv/mcp-podman/runner/.config/containers/registries.conf.d/custom.conf']
        cases = [('', False), ('credential-helpers = []', False),
                 ('credential-helpers = ["containers-auth.json"]', False)]
        for key in ['credential-helpers', 'Credential-Helpers', 'CREDENTIAL-HELPERS',
                    'CrEdEnTiAl-HeLpErS', '"credential-helper\u017f"']:
            for value in ['["secretservice"]', '["pass"]', '["containers-auth.json", "pass"]',
                          '["secretservice", "containers-auth.json"]', '"containers-auth.json"',
                          'false', '[1]', '[[]]', '{ helper = "pass" }']:
                cases.append((key + ' = ' + value, True))
            if key != 'credential-helpers':
                # Reject noncanonical spellings even with otherwise safe values;
                # there must be no choice of precedence between equivalent keys.
                cases.extend([(key + ' = []', True), (key + ' = ["containers-auth.json"]', True)])
        cases.extend([
            ('credential-helpers = []\nCredential-Helpers = ["secretservice"]', True),
            ('Credential-Helpers = ["secretservice"]\ncredential-helpers = []', True),
            ('credential-helpers = []\nCREDENTIAL-HELPERS = []', True),
            ('credential-helpers = []\ncredential-helpers = []', True),
            ('credential-helpers = [', True), ('Credential-Helpers = [', True),
            ('[credential-helpers]\nhelper = "pass"', True),
        ])
        for uid in [947, 20481]:
            helper = self.canonical(uid)
            for target in config_paths:
                for config, rejects in cases:
                    with self.subTest(uid=uid, target=target, config=config), \
                         tempfile.TemporaryDirectory() as directory:
                        state, calls, reads = {'present': False}, [], []
                        def read(path):
                            self.assertIn(str(path), config_paths, 'must not read credential contents')
                            reads.append(str(path))
                            return config if str(path) == target else ''
                        with self.canonical_context(helper, state, calls, directory), \
                             patch.object(helper.Path, 'read_text', read), \
                             patch.object(helper.Path, 'exists', return_value=True), \
                             patch.object(helper.Path, 'glob', lambda p, pattern: [p / 'custom.conf']):
                            if rejects:
                                with self.assertRaises(ValueError):
                                    helper.converge()
                                self.assertFalse(any('pull' in c for c in calls))
                                self.assertFalse(state['present'])
                                self.assertEqual(list(Path(directory).iterdir()), [])
                            else:
                                self.assertTrue(helper.converge()['changed'])
                                self.assertEqual(sum('pull' in c for c in calls), 1)
                            self.assertIn(target, reads)

    def test_tunnel_config_and_fail_closed_start_sequence(self):
        for uid, port in [(947, 8080), (20481, 18080)]:
            output = self.directory / str(uid)
            config = yaml.safe_load((output / 'tunnel.yaml').read_text())
            self.assertEqual(config['control_plane']['api_key'], 'file:/etc/mcp-tunnel/control-plane-api-key')
            self.assertEqual(config['mcp']['server_urls'],
                             [{'channel': 'main', 'url': f'http://127.0.0.1:{port}/mcp'}])
            self.assertEqual(config['mcp']['startup_wait_timeout'], '60s')
            self.assertEqual(config['health']['listen_addr'], '127.0.0.1:0')
            self.assertFalse(config['admin_ui']['allow_remote'])
            self.assertFalse(config['admin_ui']['open_browser'])
            unit = (output / 'mcp-tunnel.service').read_text()
            self.assertIn('User=mcp-tunnel\nGroup=mcp-tunnel', unit)
            self.assertIn('After=mcp-workspaced.service\nBindsTo=mcp-workspaced.service', unit)
            commands = [shlex.split(l.split('=', 1)[1]) for l in unit.splitlines() if l.startswith('ExecStart')]
            self.assertEqual(len(commands), 3)
            self.assertTrue(commands[0][-1].endswith('/preflight'))
            self.assertEqual(commands[1][-3:], ['doctor', '--config', '/etc/mcp-tunnel/tunnel.yaml'])
            self.assertEqual(commands[2][-3:], ['run', '--config', '/etc/mcp-tunnel/tunnel.yaml'])
            for command in commands:
                self.assertEqual(command[:3], ['/usr/bin/env', '-i', 'PATH=/usr/bin:/bin'])
            self.assertNotIn('ExecStartPre=-', unit)
            self.assertNotIn('control-plane-api-key', unit)
            self.assertNotIn('embedded', unit)

    def test_recovery_exclusivity_modes_and_exact_exec(self):
        recovery = module(self.directory / '947/recover.py')
        status = 'LoadState=loaded\nActiveState=inactive\nMainPID=0\nJob=\n'
        for args in [['inspect'], ['storage'], ['backend', 'a' * 64], ['cleanup', 'b' * 64]]:
            with patch.object(recovery.os, 'getuid', return_value=947), \
                 patch.object(recovery.os, 'geteuid', return_value=947), \
                 patch.object(recovery.os, 'getgid', return_value=948), \
                 patch.object(recovery.subprocess, 'run', return_value=types.SimpleNamespace(stdout=status)) as run, \
                 patch.object(recovery.os, 'execve') as execute:
                recovery.launch(args)
                call = execute.call_args.args
                self.assertEqual(call[0], '/usr/local/bin/mcp-workspaced')
                self.assertEqual(call[1], [call[0], *recovery.recovery_args(args), *recovery.SHARED_ARGV])
                self.assertEqual(call[2], recovery.ENVIRONMENT)
                self.assertEqual(run.call_count, 2)
        for args in [[], ['daemon'], ['inspect', 'x'], ['storage', 'a' * 64],
                     ['backend'], ['cleanup', '-image'], ['backend', 'a' * 63], ['cleanup', 'a' * 64, '-daemon']]:
            with self.assertRaises(ValueError):
                recovery.recovery_args(args)
        for bad in ['ActiveState=active', 'ActiveState=activating', 'MainPID=123', 'Job=42', 'LoadState=not-found']:
            prop = bad.split('=')[0]
            altered = '\n'.join(bad if line.startswith(prop + '=') else line for line in status.splitlines())
            with patch.object(recovery.os, 'getuid', return_value=947), \
                 patch.object(recovery.os, 'geteuid', return_value=947), \
                 patch.object(recovery.os, 'getgid', return_value=948), \
                 patch.object(recovery.subprocess, 'run', return_value=types.SimpleNamespace(stdout=altered)), \
                 patch.object(recovery.os, 'execve') as execute:
                with self.assertRaises(ValueError):
                    recovery.launch(['inspect'])
                execute.assert_not_called()
        with patch.object(recovery.os, 'getuid', return_value=0), self.assertRaises(ValueError):
            recovery.launch(['inspect'])

    def test_metadata_gate_never_opens_key_and_rejects_wrong_state(self):
        preflight = module(self.directory / '947/tunnel-preflight.py')
        account = types.SimpleNamespace(pw_uid=949, pw_gid=950)
        directory = types.SimpleNamespace(st_mode=stat.S_IFDIR | 0o750, st_uid=0, st_gid=950)
        key = types.SimpleNamespace(st_mode=stat.S_IFREG | 0o600, st_uid=949, st_gid=950)
        with patch.object(preflight.pwd, 'getpwnam', return_value=account), \
             patch.object(preflight.os, 'getuid', return_value=949), \
             patch.object(preflight.os, 'geteuid', return_value=949), \
             patch.object(preflight.os, 'getgid', return_value=950), \
             patch.object(preflight.os, 'getgroups', return_value=[950]), \
             patch.object(preflight.os, 'access', return_value=True) as access, \
             patch.object(preflight.Path, 'lstat', side_effect=[directory, key]), \
             patch('builtins.open', side_effect=AssertionError('content must not be opened')):
            preflight.secret_metadata()
            access.assert_called_once()
        for prop, bad in [('st_mode', stat.S_IFLNK | 0o600), ('st_mode', stat.S_IFDIR | 0o600),
                          ('st_mode', stat.S_IFREG | 0o640), ('st_uid', 947), ('st_gid', 948)]:
            wrong = deepcopy(key)
            setattr(wrong, prop, bad)
            with patch.object(preflight.pwd, 'getpwnam', return_value=account), \
                 patch.object(preflight.os, 'getuid', return_value=949), \
                 patch.object(preflight.os, 'geteuid', return_value=949), \
                 patch.object(preflight.os, 'getgid', return_value=950), \
                 patch.object(preflight.os, 'getgroups', return_value=[950]), \
                 patch.object(preflight.Path, 'lstat', side_effect=[directory, wrong]), self.assertRaises(ValueError):
                preflight.secret_metadata()

    def test_service_prerequisites_reject_drift_without_mutation(self):
        prerequisites = module(self.directory / '947/prerequisites.py')
        reads = {
            '/proc/self/cgroup': '0::/system.slice/mcp-workspaced.service\n',
            '/sys/fs/cgroup' + prerequisites.CGROUP + '/cgroup.controllers': 'cpu memory pids',
            '/etc/resolv.conf': '# deployment owned\nnameserver 9.9.9.9\nnameserver 2606:4700:4700::1111\n',
            '/proc/sys/net/ipv4/ping_group_range': '948 948',
        }
        metadata = {
            '/run/user/947': types.SimpleNamespace(st_mode=stat.S_IFDIR | 0o700, st_uid=947),
            '/run/user/947/bus': types.SimpleNamespace(st_mode=stat.S_IFSOCK | 0o600, st_uid=947),
            '/etc/resolv.conf': types.SimpleNamespace(st_mode=stat.S_IFREG | 0o644, st_uid=0),
        }
        def run(command, **kwargs):
            self.assertEqual(kwargs['env'], prerequisites.ENVIRONMENT)
            self.assertEqual(command[0], '/usr/bin/systemctl')
            return types.SimpleNamespace(stdout=prerequisites.CGROUP if '--user' in command else 'active')
        with patch.object(prerequisites.os, 'getuid', return_value=947), \
             patch.object(prerequisites.os, 'geteuid', return_value=947), \
             patch.object(prerequisites.os, 'getgid', return_value=948), \
             patch.object(prerequisites.os, 'getgroups', return_value=[948]), \
             patch.object(prerequisites.os.path, 'ismount', return_value=True), \
             patch.object(prerequisites.Path, 'read_text', lambda p: reads[str(p)]), \
             patch.object(prerequisites.Path, 'lstat', lambda p: metadata[str(p)]), \
             patch.object(prerequisites.subprocess, 'run', side_effect=run):
            prerequisites.verify()
            for path, wrong in [('/proc/self/cgroup', '0::/mcpws123.slice/build.service'),
                                 ('/etc/resolv.conf', 'nameserver 127.0.0.53'),
                                 ('/proc/sys/net/ipv4/ping_group_range', '0 2147483647'),
                                 ('/sys/fs/cgroup' + prerequisites.CGROUP + '/cgroup.controllers', 'cpu pids')]:
                original = reads[path]
                reads[path] = wrong
                with self.assertRaises(ValueError):
                    prerequisites.verify()
                reads[path] = original
            with patch.object(prerequisites.os.path, 'ismount', return_value=False), self.assertRaises(ValueError):
                prerequisites.verify()
            with patch.object(prerequisites.subprocess, 'run', return_value=types.SimpleNamespace(stdout='inactive')), \
                 self.assertRaises(ValueError):
                prerequisites.verify()

    def test_bounded_wait_delayed_listener_timeout_and_no_error_fallback(self):
        preflight = module(self.directory / '947/tunnel-preflight.py')
        with patch.object(preflight.socket, 'create_connection', side_effect=[ConnectionRefusedError(), Mock()]) as connect, \
             patch.object(preflight.time, 'sleep'), \
             patch.object(preflight.time, 'monotonic', side_effect=[0, 0, 0, 1]):
            connect.side_effect = [ConnectionRefusedError(), unittest.mock.MagicMock()]
            preflight.wait_local()
            self.assertEqual(connect.call_count, 2)
            self.assertEqual(connect.call_args.args[0], ('127.0.0.1', 8080))
        with patch.object(preflight.socket, 'create_connection') as connect, \
             patch.object(preflight.time, 'monotonic', side_effect=[0, 61]), self.assertRaises(TimeoutError):
            preflight.wait_local()
        connect.assert_not_called()
        with patch.object(preflight.socket, 'create_connection', side_effect=OSError('other failure')) as connect, \
             self.assertRaises(OSError):
            preflight.wait_local()
        self.assertEqual(connect.call_count, 1)

    @unittest.skipUnless(shutil.which('systemd-analyze'), 'systemd-analyze unavailable')
    def test_units_parse_in_synthetic_systemd_root(self):
        with tempfile.TemporaryDirectory(prefix='s3e-unit-root-') as directory:
            root = Path(directory)
            units = root / 'etc/systemd/system'
            units.mkdir(parents=True)
            for name in ['mcp-workspaced.service', 'mcp-tunnel.service']:
                shutil.copyfile(self.directory / '947' / name, units / name)
            for name in ['sysinit.target', 'basic.target', 'shutdown.target', 'multi-user.target']:
                (units / name).write_text('[Unit]\nDescription=Synthetic dependency\n')
            for name in ['mcp-data-ready.service', 'user@947.service', 'netfilter-persistent.service', 'systemd-sysctl.service']:
                (units / name).write_text('[Service]\nType=oneshot\nExecStart=/usr/bin/true\nRemainAfterExit=yes\n')
            (units / r'srv-mcp\x2dpodman.mount').write_text('[Mount]\nWhat=/dev/synthetic\nWhere=/srv/mcp-podman\nType=ext4\n')
            for path in ['usr/bin/true', 'usr/bin/env', 'usr/bin/python3', 'usr/local/bin/mcp-workspaced']:
                target = root / path
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text('#!/bin/sh\nexit 0\n')
                target.chmod(0o755)
            result = subprocess.run(['systemd-analyze', 'verify', '--root=' + directory,
                                     '--generators=no', '--man=no', 'mcp-workspaced.service', 'mcp-tunnel.service'],
                                    text=True, capture_output=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


class SourceContract(unittest.TestCase):
    @unittest.skipUnless(shutil.which('ansible-playbook'), 'ansible-playbook unavailable')
    def test_canonical_task_changed_reporting_and_failure_gate(self):
        original = yaml.safe_load((ROLE / 'tasks/canonical_image.yml').read_text())[0]
        self.assertIs(original['become'], True)
        self.assertEqual(original['become_user'], 'mcp-runner')
        self.assertEqual(original['ansible.builtin.command']['argv'],
                         ['/usr/bin/env', '-i', '/usr/bin/python3', '-I',
                          '/usr/local/lib/mcp-workspaced/canonical-image'])
        self.assertEqual(original['ansible.builtin.command']['chdir'], '{{ mcp_runner_home }}')
        self.assertNotIn('failed_when', original)
        self.assertNotIn('ignore_errors', original)
        with tempfile.TemporaryDirectory(prefix='s3e-image-gate-') as directory:
            plays = []
            for name, output, rc in [('present', '{"changed":false}', 0),
                                     ('absent', '{"changed":true}', 0),
                                     ('failed', '', 1), ('malformed', 'not JSON', 0)]:
                script = Path(directory) / (name + '.py')
                script.write_text('import sys\nprint(' + repr(output) + ')\nsys.exit(' + str(rc) + ')\n')
                task = deepcopy(original)
                task['become'] = False
                task['ansible.builtin.command'] = {'argv': ['/usr/bin/python3', '-I', str(script)]}
                failed = name in ['failed', 'malformed']
                plays.append({'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
                              'become': False, 'tasks': [
                    {'ansible.builtin.set_fact': {'activated': False, 'blocked': False}},
                    {'block': [task, {'ansible.builtin.set_fact': {'activated': True}}],
                     'rescue': [{'ansible.builtin.set_fact': {'blocked': True}}]},
                    {'ansible.builtin.assert': {'that': [
                        'blocked == ' + str(failed), 'activated == ' + str(not failed),
                        'mcp_canonical_image_convergence.changed == ' + str(name == 'absent')]
                        if not failed else ['blocked', 'not activated']}}]})
            result = ansible(plays, directory)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    @unittest.skipUnless(shutil.which('ansible-playbook'), 'ansible-playbook unavailable')
    def test_production_configuration_rejects_invalid_inputs(self):
        cases = []
        def reject(name, mutate):
            values = fixture()
            mutate(values)
            cases.append((name, values))
        reject('nonloopback', lambda v: v['mcp_production_endpoint'].update(address='0.0.0.0'))
        reject('boolean port', lambda v: v['mcp_production_endpoint'].update(port=True))
        reject('floating image', lambda v: v.update(mcp_canonical_image='example.invalid/workspace:latest'))
        reject('second canonical source', lambda v: v['mcp_workspaced_config'].update(
            image='example.invalid/another@sha256:' + 'b' * 64))
        reject('image line break', lambda v: v.update(mcp_canonical_image=v['mcp_canonical_image'] + '\n'))
        reject('missing policy', lambda v: v['mcp_workspaced_config'].pop('nofile'))
        reject('arbitrary flag', lambda v: v['mcp_workspaced_config'].update(daemon=True))
        reject('wrong Data', lambda v: v['mcp_workspaced_config'].update({'data-dir': '/tmp'}))
        reject('different DNS', lambda v: v['mcp_workspaced_config'].update({'public-dns': ['8.8.8.8']}))
        reject('bad migration policy', lambda v: v['mcp_workspaced_config'].update({'migration-tasks-max': 512}))
        reject('bad memory type', lambda v: v['mcp_workspaced_config'].update({'memory-bytes': '4294967296'}))
        reject('bad duration', lambda v: v['mcp_workspaced_config'].update({'idle-ttl': '0s'}))
        reject('missing Tunnel input', lambda v: v.update(mcp_tunnel_id='REPLACE_WITH_TUNNEL_ID'))
        plays = []
        for name, values in cases:
            plays.append({'name': name, 'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
                          'become': False, 'vars': values, 'tasks': [
                {'ansible.builtin.set_fact': {'case_rejected': False, 'assertion_failure': False}},
                {'ansible.builtin.import_tasks': str(ROOT / 'roles/s3c/tasks/interface.yml')},
                {'block': [{'ansible.builtin.import_tasks': str(ROLE / 'tasks/configuration.yml')}],
                 'rescue': [{'ansible.builtin.set_fact': {'case_rejected': True,
                             'assertion_failure': "{{ ansible_failed_result.assertion is defined or (ansible_failed_result.results | default([]) | selectattr('assertion', 'defined') | list | length > 0) }}"}}]},
                {'ansible.builtin.assert': {'that': ['case_rejected', 'assertion_failure']}}]})
        with tempfile.TemporaryDirectory(prefix='s3e-invalid-') as directory:
            result = ansible(plays, directory)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_archive_install_idempotence_and_digest_failure_before_writes(self):
        installer = module(ROLE / 'files/install-tunnel.py')
        # Synthetic public artifact bytes, no credential fixtures. Root metadata
        # is simulated; this does not install anything outside the temporary tree.
        with tempfile.TemporaryDirectory(prefix='s3e-assets-') as directory:
            root = Path(directory)
            archive = root / 'tunnel-client-v0.0.16-linux-arm64.zip'
            with zipfile.ZipFile(archive, 'w') as bundle:
                for name in installer.MEMBERS:
                    bundle.writestr(name, 'synthetic public artifact: ' + name)
            for name in ['SHA256SUMS.txt', 'tunnel-client-v0.0.16-provenance.sigstore.json']:
                (root / name).write_text('synthetic public verification material')
            pins = {name: hashlib.sha256((root / name).read_bytes()).hexdigest() for name in installer.PINS}
            original_lstat = Path.lstat
            def as_root(path):
                actual = original_lstat(path)
                return types.SimpleNamespace(st_mode=actual.st_mode, st_uid=0)
            with patch.object(installer, 'PINS', pins), patch.object(Path, 'lstat', as_root):
                self.assertTrue(installer.install(root))
                self.assertFalse(installer.install(root))
                for name, mode in installer.MEMBERS.items():
                    self.assertEqual(stat.S_IMODE((root / name).stat().st_mode), mode)
                for name in pins:
                    original = (root / name).read_bytes()
                    (root / name).write_bytes(b'wrong artifact')
                    with patch.object(installer.os, 'replace') as replace, self.assertRaises(ValueError):
                        installer.install(root)
                    replace.assert_not_called()
                    (root / name).write_bytes(original)

    def test_secret_tasks_are_metadata_only_and_install_pins_exact(self):
        secret = yaml.safe_load((ROLE / 'tasks/secret.yml').read_text())
        for task in secret:
            self.assertFalse(set(task) & {'ansible.builtin.copy', 'ansible.builtin.template', 'ansible.builtin.slurp'})
            if 'ansible.builtin.stat' in task:
                value = task['ansible.builtin.stat']
                for key in ['follow', 'get_checksum', 'get_mime', 'get_attributes']:
                    self.assertIs(value[key], False)
        text = (ROLE / 'tasks/secret.yml').read_text()
        for value in ["stat.mode == '0750'", "stat.mode == '0600'", "stat.pw_name == 'mcp-tunnel'",
                      "stat.gr_name == 'mcp-tunnel'", 'stat.readable', 'mcp_runner_secret_access.rc != 1']:
            self.assertIn(value, text)
        installer = module(ROLE / 'files/install-tunnel.py')
        tasks = yaml.safe_load((ROLE / 'tasks/main.yml').read_text())
        download = next(t for t in tasks if 'ansible.builtin.get_url' in t)
        self.assertEqual(download['ansible.builtin.get_url']['url'],
                         'https://github.com/openai/tunnel-client/releases/download/v0.0.16/{{ item.name }}')
        self.assertEqual({a['name']: a['sha256'] for a in download['loop']}, installer.PINS)
        self.assertEqual(installer.PINS['tunnel-client-v0.0.16-linux-arm64.zip'],
                         '963d0384aaa7c798778479c45673f9051a4039dd891aef8f8978d2a1a628b74f')
        self.assertEqual(installer.PINS['SHA256SUMS.txt'],
                         'fcce864a5935655534cc3d981257509579dbb5c71dbaec64c17a157ca9d65e56')
        self.assertEqual(installer.PINS['tunnel-client-v0.0.16-provenance.sigstore.json'],
                         'adae10fe6a877d05bee25f4d5fc2c0623c76189ac5af7387efa1c0152e16d3ab')
        self.assertLess(next(i for i, t in enumerate(tasks) if t.get('ansible.builtin.import_tasks') == 'secret.yml'),
                        next(i for i, t in enumerate(tasks) if 'ansible.builtin.systemd_service' in t))

    def test_no_bypass_or_new_go_dependency_and_scripts_parse(self):
        sources = '\n'.join(p.read_text() for p in ROLE.rglob('*') if p.is_file())
        for prohibited in ['podman prune', 'buildah prune', 'journal.json', 'CAP_NET_ADMIN',
                           'sudoers', 'latest/download', '.bashrc', '.profile']:
            self.assertNotIn(prohibited, sources)
        for path in (ROLE / 'files').glob('*.py'):
            ast.parse(path.read_text())


if __name__ == '__main__':
    unittest.main()

"""Composition evidence from production tasks/templates; no target operations.

Only account/host observations are synthetic. Expected paths and identities
below independently anchor the frozen deployment contract, not renderer output.
"""
import ast
import json
import re
from pathlib import Path
import shlex
import shutil
import tempfile
import unittest
from unittest.mock import patch

import yaml
import test_s3d_contract as s3d
from test_s3e_contract import ansible, module

ROOT = Path(__file__).resolve().parents[1]


def tasks(role):
    return yaml.safe_load((ROOT / f'roles/{role}/tasks/main.yml').read_text())


def reachable_tasks(path):
    """Walk static task edges and blocks; refuse edges we cannot inspect."""
    seen, active = set(), set()

    def walk_file(path):
        path = path.resolve()
        if path in active:
            raise AssertionError(f'cyclic task import: {path}')
        if path in seen:
            return
        active.add(path)
        yield from walk_tasks(path, yaml.safe_load(path.read_text()))
        active.remove(path)
        seen.add(path)

    def walk_tasks(path, entries):
        if not isinstance(entries, list):
            raise AssertionError(f'expected task list: {path}')
        for task in entries:
            if not isinstance(task, dict):
                raise AssertionError(f'expected task mapping: {path}')
            yield path, task
            for key, value in task.items():
                action = key.rsplit('.', 1)[-1]
                if action in ('include_role', 'import_role', 'include', 'action', 'local_action'):
                    raise AssertionError(f'uninspectable task/role edge: {path}: {key}')
                if action in ('import_tasks', 'include_tasks'):
                    target = value.get('file') if isinstance(value, dict) else value
                    if (not isinstance(target, str) or not target
                            or any(token in target for token in ('{{', '{%', '{#'))):
                        raise AssertionError(f'dynamic task edge: {path}: {value}')
                    yield from walk_file(path.parent / target)
                if key in ('block', 'rescue', 'always'):
                    yield from walk_tasks(path, value)

    yield from walk_file(path)


def assert_manager_ordering(test, unit, manager):
    for relation in ('Requires', 'After'):
        entries = [line.split('=', 1)[1].split() for line in unit.splitlines()
                   if line.startswith(relation + '=')]
        test.assertEqual(len(entries), 1, relation)
        test.assertIn(manager, entries[0], relation)


def fact_task(role, key):
    return next(t for t in tasks(role) if key in t.get('ansible.builtin.set_fact', {}))


def play(values, actions):
    return [{'hosts': 'localhost', 'connection': 'local', 'gather_facts': False,
             'become': False, 'vars': values, 'tasks': actions}]


@unittest.skipUnless(shutil.which('ansible-playbook'), 'ansible-playbook unavailable')
class Composition(unittest.TestCase):
    def test_actual_identity_storage_manager_network_to_services(self):
        for uid, gid, port, dns in [(947, 952, 8080, s3d.DNS),
                                    (20481, 31002, 18443, list(reversed(s3d.DNS)))]:
            with self.subTest(uid=uid), tempfile.TemporaryDirectory(prefix='s3f-') as tmp:
                directory = Path(tmp)
                values = yaml.safe_load((ROOT / 'group_vars/all.yml').read_text())
                runner = ['x', str(uid), str(gid), '', values['mcp_runner_home'], '/usr/sbin/nologin']
                tunnel = ['x', str(uid + 2), str(gid + 2), '', '/nonexistent', '/usr/sbin/nologin']
                groups = {'mcp-runner': ['x', str(gid), ''], 'mcp-tunnel': ['x', str(gid + 2), '']}
                values.update(mcp_current_runner_passwd=runner, mcp_current_tunnel_passwd=tunnel,
                              mcp_current_runner_group=groups['mcp-runner'],
                              mcp_current_tunnel_group=groups['mcp-tunnel'],
                              mcp_current_passwd_records={'mcp-runner': runner, 'mcp-tunnel': tunnel},
                              mcp_current_group_records=groups,
                              mcp_runner_groups={'stdout': str(gid)}, mcp_tunnel_groups={'stdout': str(gid + 2)},
                              mcp_public_dns=dns, mcp_host_public_ips=s3d.HOSTS,
                              mcp_production_endpoint={'address': '127.0.0.1', 'port': port},
                              mcp_canonical_image='example.invalid/workspace@sha256:' + 'a' * 64,
                              mcp_tunnel_id='tunnel_' + '0' * 32)
                network_task = next(t for t in tasks('s3d') if 'mcp_network_plan' in t)
                keys = ['mcp_runner_uid', 'mcp_runner_primary_gid', 'mcp_runner_account',
                        'mcp_tunnel_account', 'mcp_runtime_environment', 'mcp_service_environment',
                        'mcp_data_mount_unit', 'mcp_data_ready_unit', 'mcp_user_manager_unit',
                        'mcp_user_manager_cgroup']
                export = {k: '{{ ' + k + ' }}' for k in keys}
                export['network_args'] = network_task['mcp_network_plan']
                actions = [
                    {'ansible.builtin.import_tasks': str(ROOT / 'roles/s3a/tasks/assert_created_identities.yml')},
                    fact_task('s3b', 'mcp_runtime_environment'),
                    {'ansible.builtin.import_tasks': str(ROOT / 'roles/s3c/tasks/interface.yml')},
                    {'ansible.builtin.copy': {'content': '{{ export | to_json }}',
                                             'dest': str(directory / 'interfaces.json')}}]
                result = ansible(play({**values, 'export': export}, actions), directory)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                facts = json.loads((directory / 'interfaces.json').read_text())
                args = facts.pop('network_args')
                self.assertEqual(args, dict(uid=uid, gid=gid, resolvers=dns,
                                            host_public_ips=s3d.HOSTS, endpoint=values['mcp_production_endpoint']))
                # Execute the actual S3D planner under its existing read-only host
                # doubles, including its independent pwd lookup and parser calls.
                with patch.object(s3d, 'DNS', args['resolvers']):
                    planned = s3d.Planner().run_plan(uid=args['uid'], gid=args['gid'], endpoint=args['endpoint'])
                self.assertEqual(planned['ping_sysctl'], f'net.ipv4.ping_group_range = {gid} {gid}\n')
                for family in ('v4', 'v6'):
                    self.assertIn(f'--uid-owner {uid} ', planned['rules'][family])
                values.update(facts, mcp_network_plan=planned)
                actions = [fact_task('s3d', 'mcp_public_dns_validated'),
                           {'ansible.builtin.import_tasks': str(ROOT / 'roles/s3e/tasks/configuration.yml')}]
                for role, name in [('s3b', 'runtime.env'), ('s3b', 'storage.conf'), ('s3b', 'containers.conf'),
                                   ('s3b', 'data.mount'), ('s3e', 'mcp-workspaced.service'),
                                   ('s3e', 'mcp-tunnel.service'), ('s3e', 'recover.py'), ('s3e', 'tunnel.yaml'),
                                   ('s3e', 'canonical-image.py')]:
                    actions.append({'ansible.builtin.template': {
                        'src': str(ROOT / f'roles/{role}/templates/{name}.j2'), 'dest': str(directory / name)}})
                result = ansible(play(values, actions), directory)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                unit = (directory / 'mcp-workspaced.service').read_text()
                command = shlex.split(next(l.split('=', 1)[1] for l in unit.splitlines() if l.startswith('ExecStart=')))
                install = next(t['ansible.builtin.copy'] for t in tasks('s3a')
                               if t.get('register') == 'mcp_application_install')
                self.assertEqual(install['dest'], '/usr/local/bin/mcp-workspaced')
                self.assertEqual((install['owner'], install['group'], install['mode']), ('root', 'root', '0755'))
                self.assertEqual(command[:4], [install['dest'], '-daemon', '-mcp-listen', f'127.0.0.1:{port}'])
                recovery = module(directory / 'recover.py')
                canonical = module(directory / 'canonical-image.py')
                self.assertEqual(canonical.ENVIRONMENT, recovery.ENVIRONMENT)
                self.assertEqual((canonical.UID, canonical.GID), (uid, gid))
                self.assertEqual(canonical.GRAPHROOT, '/srv/mcp-podman/runner/.local/share/containers/storage')
                self.assertEqual(canonical.IMAGE, values['mcp_canonical_image'])
                self.assertEqual(canonical.PODMAN, ['/usr/bin/podman', '--remote=false'])
                self.assertEqual(command[4:], recovery.SHARED_ARGV)
                flags = {}
                for flag, value in zip(command[4::2], command[5::2]):
                    flags.setdefault(flag, []).append(value)
                self.assertEqual(flags['-public-dns'], dns)
                self.assertEqual([l.split()[1] for l in planned['resolv_conf'].splitlines() if l.startswith('nameserver ')], dns)
                self.assertIn(s3d.reply_rule(port), planned['rules']['v4'])
                self.assertEqual(flags['-state-dir'], ['/srv/mcp-podman/runner/.local/state/mcp-workspaced'])
                self.assertEqual(flags['-data-dir'], ['/srv/mcp-podman/workspaces'])
                self.assertEqual(flags['-podman'], ['/usr/bin/podman'])
                runtime = dict(l.split('=', 1) for l in (directory / 'runtime.env').read_text().splitlines()
                               if l and not l.startswith('#'))
                self.assertEqual(recovery.ENVIRONMENT, {**runtime, 'DBUS_SESSION_BUS_ADDRESS': f'unix:path=/run/user/{uid}/bus'})
                self.assertEqual(runtime['HOME'], '/srv/mcp-podman/runner')
                self.assertEqual(runtime['TMPDIR'], '/srv/mcp-podman/tmp')
                self.assertEqual(runtime['XDG_RUNTIME_DIR'], f'/run/user/{uid}')
                self.assertIn('Where=/srv/mcp-podman', (directory / 'data.mount').read_text())
                for key in ('graphroot', 'rootless_storage_path'):
                    self.assertIn(f'{key} = "/srv/mcp-podman/runner/.local/share/containers/storage"',
                                  (directory / 'storage.conf').read_text())
                containers = (directory / 'containers.conf').read_text()
                for key, expected in {
                    'static_dir': '/srv/mcp-podman/runner/.local/share/containers/storage/libpod',
                    'volume_path': '/srv/mcp-podman/runner/.local/share/containers/storage/volumes',
                    'image_copy_tmp_dir': runtime['TMPDIR'],
                    'events_logfile_path': '/srv/mcp-podman/events/events.log',
                    'tmp_dir': f'/run/user/{uid}/libpod/tmp',
                    'cgroup_manager': 'systemd',
                }.items():
                    self.assertIn(f'{key} = "{expected}"', containers)
                # The path-creation module is another independent consumer;
                # inspect its actual constants without invoking host operations.
                tree = ast.parse((ROOT / 'library/mcp_data_paths.py').read_text())
                paths = next(ast.literal_eval(node.value) for node in tree.body
                             if isinstance(node, ast.Assign)
                             and any(isinstance(t, ast.Name) and t.id == 'RUNNER_PATHS' for t in node.targets))
                for path in (flags['-state-dir'][0], flags['-data-dir'][0], runtime['TMPDIR'],
                             '/srv/mcp-podman/runner/.local/share/containers/storage'):
                    self.assertIn(str(Path(path).relative_to('/srv/mcp-podman')), paths)
                for line in runtime.items():
                    self.assertIn('Environment="' + '='.join(line) + '"', unit)
                self.assertIn(f'BindsTo=srv-mcp\\x2dpodman.mount mcp-data-ready.service user@{uid}.service netfilter-persistent.service', unit)
                assert_manager_ordering(self, unit, facts['mcp_user_manager_unit'])
                # Each relationship must independently detect removal or drift
                # in the actual rendered unit, using the S3C-derived fact.
                for relation in ('Requires', 'After'):
                    for replacement in ('', 'user@1.service'):
                        altered = '\n'.join(
                            line.replace(facts['mcp_user_manager_unit'], replacement)
                            if line.startswith(relation + '=') else line
                            for line in unit.splitlines())
                        with self.assertRaises(AssertionError):
                            assert_manager_ordering(self, altered, facts['mcp_user_manager_unit'])
                self.assertEqual(facts['mcp_user_manager_cgroup'], f'/user.slice/user-{uid}.slice/user@{uid}.service')
                self.assertIn('User=' + facts['mcp_runner_account'] + '\nGroup=mcp-runner', unit)
                self.assertIn('User=' + facts['mcp_tunnel_account'] + '\nGroup=mcp-tunnel',
                              (directory / 'mcp-tunnel.service').read_text())
                tunnel_config = yaml.safe_load((directory / 'tunnel.yaml').read_text())
                self.assertEqual(tunnel_config['mcp']['server_urls'], [{'channel': 'main', 'url': 'http://' + command[3] + '/mcp'}])
                self.assertEqual(tunnel_config['control_plane']['api_key'], 'file:/etc/mcp-tunnel/control-plane-api-key')
                self.assertEqual(set(tunnel_config['control_plane']), {'api_key', 'tunnel_id'})
                for action in ('inspect', 'storage'):
                    self.assertEqual(recovery.recovery_args([action]), ['-recover', action])
                for action in ('backend', 'cleanup'):
                    self.assertEqual(recovery.recovery_args([action, 'a' * 64]), ['-recover', action, '-recovery-selection', 'a' * 64])
                for action in ('reset', 'prune', 'daemon'):
                    with self.assertRaises(ValueError):
                        recovery.recovery_args([action])


class SourceBoundary(unittest.TestCase):
    def test_canonical_convergence_is_reachable_before_all_production_activation(self):
        sequence = list(reachable_tasks(ROOT / 'roles/s3e/tasks/main.yml'))
        converge = next(i for i, (path, task) in enumerate(sequence)
                        if path.name == 'canonical_image.yml' and 'ansible.builtin.command' in task)
        render = next(i for i, (_, task) in enumerate(sequence) if task.get('register') == 'mcp_service_render')
        self.assertLess(render, converge)
        render_task = sequence[render][1]
        install = next(v for v in render_task['loop'] if v['src'] == 'canonical-image.py.j2')
        self.assertEqual(install, {'src': 'canonical-image.py.j2',
                                  'dest': '/usr/local/lib/mcp-workspaced/canonical-image', 'mode': '0755'})
        self.assertEqual((render_task['ansible.builtin.template']['owner'],
                          render_task['ansible.builtin.template']['group']), ('root', 'root'))
        for i, (_, task) in enumerate(sequence):
            self.assertFalse(set(task) & {'rescue', 'ignore_errors', 'ignore_unreachable'})
            if 'ansible.builtin.systemd_service' in task:
                self.assertLess(converge, i)
        helper = (ROOT / 'roles/s3e/templates/canonical-image.py.j2').read_text()
        self.assertIn('/usr/local/lib/mcp-workspaced/service-prerequisites', helper)
        self.assertNotIn('socket', helper)
        self.assertNotIn('resolv.conf', helper)
        runtime = (ROOT.parent / 'internal/workspace/podman.go').read_text()
        self.assertIn('"container", "create", "--pull=never"', runtime)

    def test_only_landed_roles_and_no_hidden_bootstrap_import(self):
        site = yaml.safe_load((ROOT / 'site.yml').read_text())
        self.assertEqual(site[0]['roles'], ['s3a', 's3b', 's3c', 's3d', 's3e'])
        for role in site[0]['roles']:
            for path, task in reachable_tasks(ROOT / f'roles/{role}/tasks/main.yml'):
                for forbidden in ('mcp_data_bootstrap', 'data_bootstrap', 'bootstrap-data', 'mkfs', 'parted', 'wipefs --all'):
                    self.assertNotIn(forbidden, yaml.safe_dump(task), str(path))
        bootstrap = tasks('data_bootstrap')
        self.assertIn('mcp_data_bootstrap_enabled | default(false) is sameas true',
                      bootstrap[0]['ansible.builtin.assert']['that'])
        defaults = yaml.safe_load((ROOT / 'group_vars/all.yml').read_text())
        self.assertNotIn('mcp_data_bootstrap_enabled', defaults)
        self.assertEqual(defaults['mcp_public_dns'], [])

    def test_service_authority_inventory_and_no_deployment_runner(self):
        # Independent inventory: any new installed service/template requires an
        # explicit source review instead of silently becoming a second authority.
        units = sorted(str(p.relative_to(ROOT)) for p in (ROOT / 'roles').rglob('*.service.j2'))
        self.assertEqual(units, [
            'roles/s3b/templates/data-ready.service.j2',
            'roles/s3b/templates/data-verify.service.j2',
            'roles/s3e/templates/mcp-tunnel.service.j2',
            'roles/s3e/templates/mcp-workspaced.service.j2'])
        self.assertEqual(sorted(p.name for p in (ROOT / 'roles').iterdir()),
                         ['data_bootstrap', 's3a', 's3b', 's3c', 's3d', 's3e'])
        # This slice deliberately adds no workflow or runner. Fail closed on a
        # future workflow until its source-only execution boundary is reviewed.
        workflows = ROOT.parent / '.github/workflows'
        self.assertFalse(workflows.exists() and any(workflows.iterdir()))
        for role in ('s3a', 's3b', 's3c', 's3d', 's3e'):
            for path in (ROOT / f'roles/{role}').rglob('*'):
                if not path.is_file() or '__pycache__' in path.parts:
                    continue
                source = path.read_text()
                for forbidden in ('podman system prune', 'buildah prune', 'mcp-probe', 'echo-server'):
                    self.assertNotIn(forbidden, source, str(path))
        # S3D owns writes; S3E preflight observes, never repairs these authorities.
        preflight = (ROOT / 'roles/s3e/templates/prerequisites.py.j2').read_text()
        self.assertNotIn('write_text', preflight)
        self.assertNotIn('sysctl -w', preflight)
        self.assertNotIn('iptables', preflight)

    def test_single_authority_tasks_and_credential_file_reference(self):
        services = []
        for role in ('s3a', 's3b', 's3c', 's3d', 's3e'):
            for path, task in reachable_tasks(ROOT / f'roles/{role}/tasks/main.yml'):
                service = task.get('ansible.builtin.systemd_service', {})
                if 'name' in service:
                    services.append((role, service['name']))
                for kind in ('ansible.builtin.copy', 'ansible.builtin.template', 'ansible.builtin.lineinfile'):
                    action = task.get(kind, {})
                    destination = action.get('dest', action.get('path', ''))
                    if any(part in destination for part in ('/etc/iptables/', '/etc/sysctl.d/', '/etc/resolv.conf')):
                        self.assertEqual(role, 's3d')
                    self.assertNotIn('/sys/fs/cgroup', destination)
        self.assertEqual(services, [
            ('s3b', r'srv-mcp\x2dpodman.mount'), ('s3b', 'mcp-data-ready.service'),
            ('s3c', '{{ mcp_user_manager_unit }}'), ('s3c', '{{ mcp_user_manager_unit }}'),
            ('s3c', 'dbus.socket'), ('s3d', 'netfilter-persistent.service'),
            ('s3e', 'mcp-workspaced.service'), ('s3e', 'mcp-tunnel.service')])
        # Production configuration may contain exactly the file reference, never
        # an inline key or environment-based credential. No real key fixture.
        key_lines = []
        for path in ROOT.rglob('*'):
            if not path.is_file() or 'tests' in path.parts or '__pycache__' in path.parts:
                continue
            if path.suffix not in ('.yml', '.yaml', '.j2'):
                continue
            for line in path.read_text().splitlines():
                if re.match(r'\s*api_key\s*:', line):
                    key_lines.append(line.strip())
                self.assertNotRegex(line, r'(?i)(?:OPENAI_API_KEY|CONTROL_PLANE_API_KEY)\s*[=:]')
        self.assertEqual(key_lines, ['api_key: file:/etc/mcp-tunnel/control-plane-api-key'])


class TraversalRegression(unittest.TestCase):
    def test_imported_authority_regressions_are_rejected(self):
        # Mutate a disposable source copy, never the production checkout/host.
        with tempfile.TemporaryDirectory(prefix='s3f-authority-') as tmp:
            root = Path(tmp) / 'ansible'
            shutil.copytree(ROOT, root, ignore=shutil.ignore_patterns('__pycache__'))
            imported = root / 'roles/s3c/tasks/interface.yml'
            original = imported.read_text()
            cases = [
                {'ansible.builtin.systemd_service': {'name': 'nftables.service', 'state': 'started'}},
                {'ansible.builtin.copy': {'dest': '/etc/resolv.conf', 'content': 'second authority'}},
                {'ansible.builtin.copy': {'dest': '/etc/iptables/rules.v4', 'content': 'second authority'}},
                {'ansible.builtin.copy': {'dest': '/sys/fs/cgroup/second/cgroup.subtree_control', 'content': '+cpu'}},
            ]
            for task in cases:
                with self.subTest(task=task), patch(__name__ + '.ROOT', root):
                    imported.write_text(original + yaml.safe_dump([{'block': [task]}]))
                    with self.assertRaises(AssertionError):
                        SourceBoundary().test_single_authority_tasks_and_credential_file_reference()

    def test_static_includes_blocks_and_uninspectable_edges(self):
        with tempfile.TemporaryDirectory(prefix='s3f-edges-') as tmp:
            root = Path(tmp)
            main = root / 'main.yml'
            (root / 'child.yml').write_text('- name: reached\n  ansible.builtin.debug: {msg: synthetic}\n')
            for edge in ('ansible.builtin.import_tasks', 'include_tasks'):
                for branch in ('block', 'rescue', 'always'):
                    main.write_text(yaml.safe_dump([{branch: [{edge: {'file': 'child.yml'}}]}]))
                    self.assertTrue(any(task.get('name') == 'reached' for _, task in reachable_tasks(main)))
            for task in [
                {'include_tasks': '{{ selected }}.yml'},
                {'ansible.builtin.import_tasks': {'file': '{% if selection %}child.yml{% endif %}'}},
                {'ansible.builtin.include_role': {'name': '{{ role }}'}},
                {'import_role': {'name': 'uninspected'}},
                {'action': 'include_tasks child.yml'},
            ]:
                with self.subTest(task=task):
                    main.write_text(yaml.safe_dump([{'block': [task]}]))
                    with self.assertRaises(AssertionError):
                        list(reachable_tasks(main))

"""Source/synthetic evidence only; never provision or address a host manager."""
import base64
from copy import deepcopy
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]
TASKS = ROOT / 'roles/s3c/tasks'


def encoded(value):
    return {'content': base64.b64encode(value.encode()).decode()}


def fixture(uid):
    runtime = f'/run/user/{uid}'
    cgroup = f'/user.slice/user-{uid}.slice/user@{uid}.service'
    environment = {'HOME': '/srv/mcp-podman/runner', 'PATH': '/usr/bin:/bin',
                   'XDG_RUNTIME_DIR': runtime}
    return dict(
        mcp_runner_account='mcp-runner', mcp_runner_uid=uid,
        mcp_runner_primary_gid=uid + 1, mcp_runtime_environment=environment,
        expected_environment={**environment, 'DBUS_SESSION_BUS_ADDRESS': f'unix:path={runtime}/bus'},
        expected_uid=uid, mcp_cgroup_filesystem={'stdout': 'cgroup2fs'},
        mcp_host_controllers=encoded('cpu io memory pids\n'),
        mcp_user_controllers=encoded('cpu memory pids\n'),
        mcp_runner_linger={'stdout': 'yes'},
        mcp_user_manager={'status': dict(LoadState='loaded', ActiveState='active',
                                        User=str(uid), ControlGroup=cgroup,
                                        Delegate='yes', DelegateControllers='cpu memory pids')},
        mcp_user_runtime_paths={'results': [
            {'stat': dict(isdir=True, islnk=False, uid=uid, mode='0700')},
            {'stat': dict(issock=True, islnk=False, uid=uid)}]},
        mcp_user_root_cgroup={'stdout': cgroup})


class SourceContract(unittest.TestCase):
    def test_only_existing_unprivileged_manager_and_no_new_unit_or_helper(self):
        site = yaml.safe_load((ROOT / 'site.yml').read_text())
        self.assertEqual(site[0]['roles'], ['s3a', 's3b', 's3c'])
        tasks = yaml.safe_load((TASKS / 'main.yml').read_text())
        modules = []
        for path in TASKS.glob('*.yml'):
            for task in yaml.safe_load(path.read_text()):
                modules.extend(key for key in task if key.startswith('ansible.'))
        self.assertLessEqual(set(modules), {
            'ansible.builtin.import_tasks', 'ansible.builtin.assert',
            'ansible.builtin.set_fact', 'ansible.builtin.apt',
            'ansible.builtin.command', 'ansible.builtin.systemd_service',
            'ansible.builtin.stat', 'ansible.builtin.slurp'})
        commands = [t['ansible.builtin.command']['argv'] for t in tasks
                    if 'ansible.builtin.command' in t]
        self.assertEqual(commands, [
            ['/usr/bin/stat', '-f', '--format=%T', '/sys/fs/cgroup'],
            ['/usr/bin/loginctl', '--no-ask-password', 'enable-linger', '{{ mcp_runner_account }}'],
            ['/usr/bin/loginctl', 'show-user', '{{ mcp_runner_account }}', '--property=Linger', '--value'],
            ['/usr/bin/systemctl', '--user', 'show', '--property=ControlGroup', '--value', '--', '-.slice'],
            ['/usr/bin/systemd-run', '--version']])
        services = [t for t in tasks if 'ansible.builtin.systemd_service' in t]
        self.assertEqual([t['ansible.builtin.systemd_service'] for t in services], [
            {'name': '{{ mcp_user_manager_unit }}', 'state': 'started'},
            {'name': '{{ mcp_user_manager_unit }}'},
            {'name': 'dbus.socket', 'scope': 'user', 'enabled': True, 'state': 'started'}])
        # Query after start: systemd_service's activation result can contain the
        # pre-start status. Never use it as post-start authority evidence.
        self.assertEqual(services[1]['register'], 'mcp_user_manager')
        self.assertNotIn('register', services[0])
        self.assertEqual(services[2]['become_user'], '{{ mcp_runner_account }}')
        self.assertEqual(services[2]['environment'], '{{ mcp_service_environment }}')
        for task in tasks:
            if task.get('become_user'):
                self.assertEqual(task['become_user'], '{{ mcp_runner_account }}')
            if 'ansible.builtin.command' in task:
                argv = task['ansible.builtin.command']['argv']
                if argv[0] in ['/usr/bin/systemctl', '/usr/bin/systemd-run']:
                    self.assertEqual(task['environment'], '{{ mcp_service_environment }}')
                    self.assertEqual(task['become_user'], '{{ mcp_runner_account }}')
        linger = next(t for t in tasks if 'enable-linger' in t.get('ansible.builtin.command', {}).get('argv', []))
        self.assertEqual(linger['ansible.builtin.command']['creates'],
                         '/var/lib/systemd/linger/{{ mcp_runner_account }}')
        self.assertEqual(list((ROOT / 'roles/s3c').iterdir()), [TASKS])
        identity_source = (ROOT / 'roles/s3a/tasks/assert_created_identities.yml').read_text()
        self.assertIn('mcp_runner_uid: "{{ mcp_current_runner_passwd[1] | int }}"', identity_source)

    @unittest.skipUnless(shutil.which('ansible-playbook'), 'ansible-playbook unavailable')
    def test_production_assertions_with_synthetic_identities_and_rejections(self):
        main = yaml.safe_load((TASKS / 'main.yml').read_text())
        prerequisite_assertions = [task for task in main if 'ansible.builtin.assert' in task]
        cases = [('valid system UID', fixture(947), False),
                 ('valid different UID', fixture(20481), False)]

        def reject(name, mutate):
            value = deepcopy(fixture(947))
            mutate(value)
            cases.append((name, value, True))

        reject('root identity', lambda v: v.update(mcp_runner_uid=0))
        reject('wrong account', lambda v: v.update(mcp_runner_account='other'))
        reject('wrong S3B runtime', lambda v: v['mcp_runtime_environment'].update(XDG_RUNTIME_DIR='/run/user/1000'))
        reject('legacy filesystem', lambda v: v['mcp_cgroup_filesystem'].update(stdout='tmpfs'))
        reject('no linger', lambda v: v['mcp_runner_linger'].update(stdout='no'))
        for prop, wrong in [('LoadState', 'not-found'), ('ActiveState', 'inactive'),
                            ('User', '1000'), ('Delegate', 'no'),
                            ('ControlGroup', '/system.slice/user@947.service')]:
            reject('wrong manager ' + prop,
                   lambda v, p=prop, w=wrong: v['mcp_user_manager']['status'].update({p: w}))
        for controller in ['cpu', 'memory', 'pids']:
            remaining = ' '.join(c for c in ['cpu', 'memory', 'pids'] if c != controller)
            reject('host missing ' + controller,
                   lambda v, r=remaining: v.update(mcp_host_controllers=encoded(r)))
            reject('manager not delegated ' + controller,
                   lambda v, r=remaining: v['mcp_user_manager']['status'].update(DelegateControllers=r))
            reject('unavailable below manager ' + controller,
                   lambda v, r=remaining: v.update(mcp_user_controllers=encoded(r)))
        for index, prop, wrong in [(0, 'uid', 1000), (0, 'mode', '0755'), (0, 'islnk', True),
                                    (0, 'isdir', False), (1, 'uid', 1000),
                                    (1, 'issock', False), (1, 'islnk', True)]:
            reject(f'wrong runtime metadata {index} {prop}',
                   lambda v, i=index, p=prop, w=wrong: v['mcp_user_runtime_paths']['results'][i]['stat'].update({p: w}))
        reject('user command reaches wrong manager', lambda v: v['mcp_user_root_cgroup'].update(stdout='/other'))

        tasks = []
        for name, values, should_reject in cases:
            tasks.append({'name': name + ' fixture', 'ansible.builtin.set_fact': {
                **values, 'case_rejected': False, 'failed_assertion': ''}})
            tasks.append({'name': name, 'block': [
                {'ansible.builtin.import_tasks': str(TASKS / 'interface.yml')},
                *prerequisite_assertions,
                {'ansible.builtin.import_tasks': str(TASKS / 'assert_manager.yml')},
                {'ansible.builtin.import_tasks': str(TASKS / 'assert_interface.yml')},
                {'ansible.builtin.assert': {'that': [
                    "mcp_user_manager_unit == 'user@' + (expected_uid | string) + '.service'",
                    "mcp_user_runtime_unit == 'user-runtime-dir@' + (expected_uid | string) + '.service'",
                    "mcp_user_manager_cgroup == '/user.slice/user-' + (expected_uid | string) + '.slice/user@' + (expected_uid | string) + '.service'",
                    'mcp_service_environment == expected_environment',
                    "mcp_user_manager_required_controllers == ['cpu', 'memory', 'pids']"]}}],
                'rescue': [{'ansible.builtin.set_fact': {
                    'case_rejected': True,
                    'failed_assertion': "{{ ansible_failed_result.assertion | default('') }}"}}]})
            tasks.append({'name': name + ' expected outcome', 'ansible.builtin.assert': {'that': [
                'case_rejected == ' + ('true' if should_reject else 'false'),
                'not case_rejected or failed_assertion | length > 0']}})
        play = [{'name': 'S3C synthetic production assertions only', 'hosts': 'localhost',
                 'connection': 'local', 'gather_facts': False, 'become': False, 'tasks': tasks}]
        with tempfile.TemporaryDirectory(prefix='s3c-synthetic-') as directory:
            path = Path(directory) / 'contract.yml'
            path.write_text(yaml.safe_dump(play, sort_keys=False))
            result = subprocess.run(['ansible-playbook', '-i', 'localhost,', str(path)],
                                    env={**os.environ, 'ANSIBLE_NOCOLOR': '1'},
                                    text=True, capture_output=True, timeout=120)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()

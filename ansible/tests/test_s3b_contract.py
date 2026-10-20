"""Source and synthetic checks only: never open a device or run host tools."""
import ast
import importlib.util
import json
from pathlib import Path
import sys
import types
import unittest
from unittest.mock import Mock, patch
import configparser
import shutil
import subprocess
import tempfile
import tomllib

import yaml
from jinja2 import Environment, StrictUndefined

ROOT = Path(__file__).resolve().parents[1]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


data = load('ansible.module_utils.mcp_data', ROOT / 'module_utils/mcp_data.py')
bootstrap = load('mcp_data_bootstrap_test', ROOT / 'library/mcp_data_bootstrap.py')
subids = load('mcp_subids_test', ROOT / 'library/mcp_subids_verify.py')
paths_module = load('mcp_paths_test', ROOT / 'library/mcp_data_paths.py')
IDENTITY = '/dev/disk/by-id/scsi-SYNTHETIC_DATA'
SERIAL = 'SYNTHETIC_DATA'
SIZE = 100 * 1024**3
UUID = '12345678-1234-1234-1234-123456789abc'


def snapshot():
    return dict(identity=IDENTITY, serial=SERIAL, target='/dev/synthetic-data', dev='8:16', size=SIZE,
                sector=512, type='disk', partitions=[], slaves=[], holders=[], swap=False,
                root_known=True, boot_ancestry=['8:0', '8:1'], mounts=[], foreign_mounts=[], signatures=[])


class Gates(unittest.TestCase):
    def validate(self, value, blank=True):
        return data.validate(value, IDENTITY, SERIAL, SIZE, 512, blank)

    def test_blank_synthetic_target_passes_pure_gate(self):
        self.validate(snapshot())

    def test_stop_cases(self):
        cases = {'Boot/root ancestry': {'boot_ancestry': ['8:16']},
                 'unknown root': {'root_known': False},
                 'identity mismatch': {'identity': '/dev/disk/by-id/other'},
                 'serial mismatch': {'serial': 'OTHER'}, 'size': {'size': SIZE - 512},
                 'sector': {'sector': 4096}, 'partition target': {'type': 'part'},
                 'partition table topology': {'partitions': ['synthetic-data1']},
                 'slave': {'slaves': ['dm-1']}, 'holder': {'holders': ['dm-2']},
                 'swap': {'swap': True}, 'mounted': {'mounts': [{'target': data.MOUNT}]},
                 'foreign mount': {'foreign_mounts': [{'target': '/elsewhere'}]}}
        for name, changed in cases.items():
            with self.subTest(name=name):
                value = snapshot()
                value.update(changed)
                with self.assertRaisesRegex(ValueError, 'STOP'):
                    self.validate(value)
        for signature in ['ext4', 'xfs', 'LVM2_member', 'zfs_member', 'gpt', 'dos', 'unknown']:
            with self.subTest(signature=signature):
                value = snapshot()
                value['signatures'] = [{'type': signature}]
                with self.assertRaisesRegex(ValueError, 'signatures'):
                    self.validate(value)

    def test_no_volatile_or_partition_authority(self):
        for identity in ['/dev/sdb', '/dev/nvme1n1', '/dev/disk/by-uuid/' + UUID,
                         IDENTITY + '-part1', '/dev/disk/by-id/../sdb', '']:
            with self.subTest(identity=identity), self.assertRaises(ValueError):
                data.inputs(identity, SERIAL, SIZE, 512)

    def test_missing_evidence_is_not_accepted(self):
        for key in snapshot():
            value = snapshot()
            del value[key]
            if key in ('target',):
                continue  # verified by probe/open code, not the pure gate
            with self.subTest(key=key), self.assertRaises((ValueError, KeyError)):
                self.validate(value)

    def test_disabled_bootstrap_does_not_probe_or_open(self):
        with patch.object(bootstrap, 'inspect') as probe, patch.object(bootstrap.os, 'open') as opened:
            for enabled in [False, None, 'true', 1]:
                with self.assertRaisesRegex(ValueError, 'default-disabled'):
                    bootstrap.bootstrap(enabled, IDENTITY, SERIAL, SIZE, 512)
            probe.assert_not_called()
            opened.assert_not_called()

    def test_bad_target_does_not_open_or_format(self):
        value = snapshot()
        value['holders'] = ['active']
        with patch.object(bootstrap, 'inspect', return_value=value), patch.object(bootstrap.os, 'open') as opened, \
                patch.object(bootstrap.subprocess, 'run') as execute:
            with self.assertRaises(ValueError):
                bootstrap.bootstrap(True, IDENTITY, SERIAL, SIZE, 512)
            opened.assert_not_called()
            execute.assert_not_called()

    def test_pinned_device_mismatch_and_changed_evidence_stop(self):
        for rdev, change in [(0, {}), (os_rdev(), {'serial': 'CHANGED'})]:
            before, after = snapshot(), snapshot()
            after.update(change)
            with self.subTest(rdev=rdev), patch.object(bootstrap, 'inspect', side_effect=[before, after]), \
                    patch.object(bootstrap.os, 'open', return_value=42), \
                    patch.object(bootstrap.os, 'close'), \
                    patch.object(bootstrap.os, 'fstat', return_value=types.SimpleNamespace(st_mode=0o60600, st_rdev=rdev)), \
                    patch.object(bootstrap.subprocess, 'run') as execute:
                with self.assertRaises(ValueError):
                    bootstrap.bootstrap(True, IDENTITY, SERIAL, SIZE, 512)
                execute.assert_not_called()

    def test_formatter_failure_has_no_force_retry_or_repair(self):
        with patch.object(bootstrap, 'inspect', return_value=snapshot()), \
                patch.object(bootstrap.os, 'open', side_effect=[42, 43]) as opened, \
                patch.object(bootstrap.os, 'close') as closed, \
                patch.object(bootstrap.os, 'fstat', return_value=types.SimpleNamespace(st_mode=0o60600, st_rdev=os_rdev())), \
                patch.object(bootstrap.subprocess, 'run', side_effect=RuntimeError('synthetic failure')) as execute:
            with self.assertRaisesRegex(RuntimeError, 'synthetic failure'):
                bootstrap.bootstrap(True, IDENTITY, SERIAL, SIZE, 512)
            self.assertEqual(execute.call_count, 1)
            argv = execute.call_args.args[0]
            self.assertEqual(argv[:3], ['/usr/sbin/mkfs.ext4', '-m', '10'])
            self.assertNotIn('-F', argv)
            self.assertEqual(argv[-1], '/proc/self/fd/42')
            self.assertEqual(execute.call_args.kwargs['pass_fds'], (42,))
            self.assertTrue(opened.call_args_list[0].args[1] & bootstrap.os.O_NOFOLLOW)
            self.assertTrue(opened.call_args_list[1].args[1] & bootstrap.os.O_EXCL)
            self.assertEqual(closed.call_args_list[0].args, (43,))
            self.assertEqual(closed.call_args_list[-1].args, (42,))

    def test_ext4_reserve_root_only_and_uuid(self):
        value = snapshot()
        value['signatures'] = [{'type': 'ext4', 'uuid': UUID}]
        header = ('Filesystem UUID: ' + UUID + '\nBlock count: 100001\nReserved block count: 10000\n'
                  'Reserved blocks uid: 0 (user root)\nReserved blocks gid: 0 (group root)\n')
        with patch.object(data, 'command', side_effect=[value['target'] + '\n', header]):
            data.filesystem(value, UUID)
        for old, new in [('10000\n', '9999\n'), ('0 (user root)', '1001 (user runner)'),
                         ('0 (group root)', '1001 (group runner)'), (UUID, 'wrong')]:
            with patch.object(data, 'command', side_effect=[value['target'] + '\n', header.replace(old, new)]), self.assertRaises(ValueError):
                data.filesystem(value, UUID)
        value['signatures'].append({'type': 'gpt'})
        with patch.object(data, 'command', return_value=value['target']), self.assertRaisesRegex(ValueError, 'signature'):
            data.filesystem(value, UUID)
        value['signatures'].pop()
        for matches in ['', '/dev/other', value['target'] + '\n/dev/clone']:
            with patch.object(data, 'command', return_value=matches), self.assertRaisesRegex(ValueError, 'UUID'):
                data.filesystem(value, UUID)

    def test_boot_guard_stops_on_wrong_mode_owner_content_or_mount(self):
        valid = types.SimpleNamespace(st_mode=0o40000, st_uid=0, st_gid=0)
        with patch.object(data.os, 'lstat', return_value=valid), patch.object(data.os, 'listdir', return_value=[]):
            data.check_guard('/synthetic/guard')
        for changed in [dict(st_mode=0o40755, st_uid=0, st_gid=0),
                        dict(st_mode=0o40000, st_uid=1001, st_gid=0),
                        dict(st_mode=0o120000, st_uid=0, st_gid=0)]:
            with patch.object(data.os, 'lstat', return_value=types.SimpleNamespace(**changed)), self.assertRaises(ValueError):
                data.check_guard('/synthetic/guard')
        with patch.object(data.os, 'lstat', return_value=valid), \
                patch.object(data.os, 'listdir', return_value=['unexpected']), self.assertRaises(ValueError):
            data.check_guard('/synthetic/guard')
        parent = types.SimpleNamespace(st_mode=0o40755, st_uid=0)
        with patch.object(data.os, 'lstat', return_value=parent), \
                patch.object(data, 'command', return_value=data.MOUNT), self.assertRaisesRegex(ValueError, 'wrong device'):
            data.guard(snapshot())
        with patch.object(data.os, 'lstat', return_value=parent), \
                patch.object(data, 'command', return_value=''), \
                patch.object(data.os.path, 'lexists', return_value=False), \
                patch.object(data.os, 'mkdir') as mkdir:
            with self.assertRaisesRegex(ValueError, 'absent'):
                data.guard(snapshot())
            mkdir.assert_not_called()

    def test_symlink_directory_is_rejected_without_chown(self):
        with tempfile.TemporaryDirectory(prefix='s3b-path-fixture-') as directory:
            pool = Path(directory) / 'pool'
            pool.mkdir()
            outside = Path(directory) / 'outside'
            outside.mkdir()
            (pool / 'runner').symlink_to(outside, target_is_directory=True)
            base = types.SimpleNamespace(st_uid=0, st_gid=0, st_mode=0o40755, st_dev=2)
            with patch.object(paths_module, 'MOUNT', str(pool)), \
                    patch.object(paths_module, 'ROOT_PATHS', ['runner']), \
                    patch.object(paths_module, 'RUNNER_PATHS', []), \
                    patch.object(paths_module.os.path, 'ismount', return_value=True), \
                    patch.object(paths_module.os, 'fstat', return_value=base), \
                    patch.object(paths_module.os, 'stat', return_value=types.SimpleNamespace(st_dev=1)), \
                    patch.object(paths_module.os, 'fchown') as chown:
                with self.assertRaises(OSError):
                    paths_module.provision(1234, 2345)
                chown.assert_not_called()
            self.assertEqual(list(outside.iterdir()), [])

    def test_subordinate_ranges(self):
        subids.check('operator:100000:65536\n', 'mcp-runner', 200000, 65536, [0, 1000, 1001])
        subids.check('mcp-runner:200000:65536\n', 'mcp-runner', 200000, 65536, [0, 1001])
        for records, start, ids in [('operator:200000:65536', 200000, [0]),
                                    ('mcp-runner:100000:65536', 200000, [0]),
                                    ('', 0, [0]), ('', 200000, [200001]),
                                    ('mcp-runner:200000:65536\nmcp-runner:300000:65536', 200000, [0])]:
            with self.subTest(records=records, start=start), self.assertRaises(ValueError):
                subids.check(records, 'mcp-runner', start, 65536, ids)


def os_rdev():
    return bootstrap.os.makedev(8, 16)


class SourceContract(unittest.TestCase):
    def setUp(self):
        self.values = yaml.safe_load((ROOT / 'group_vars/all.yml').read_text())
        self.values.update(mcp_runner_uid=1234, mcp_runner_primary_gid=2345,
                           mcp_data_uuid=UUID, mcp_data_expected_bytes=SIZE)
        self.env = Environment(undefined=StrictUndefined)
        self.env.filters['to_json'] = json.dumps

    def render(self, name):
        return self.env.from_string((ROOT / 'roles/s3b/templates' / name).read_text()).render(self.values)

    def test_yaml_and_python_parse(self):
        for path in sorted(ROOT.rglob('*.yml')):
            yaml.safe_load(path.read_text())
        for path in [*ROOT.glob('library/*.py'), *ROOT.glob('module_utils/*.py')]:
            ast.parse(path.read_text())
        compile(self.render('data-verify.py.j2'), 'rendered-data-verify', 'exec')

    def test_entry_graph_and_destructive_isolation(self):
        # Walk all role task includes, not just top-level list-tasks output.
        visited = set()
        modules = set()

        def tasks(path):
            path = path.resolve()
            if path in visited:
                return
            visited.add(path)
            for task in yaml.safe_load(path.read_text()):
                for key, value in task.items():
                    if key in ('ansible.builtin.import_tasks', 'ansible.builtin.include_tasks'):
                        self.assertNotIn('{{', value)
                        tasks(path.parent / value)
                    elif key in ('block', 'rescue', 'always'):
                        self.fail('Extend traversal before introducing a task block')
                    elif key not in ('name', 'when', 'loop', 'loop_control', 'register', 'changed_when',
                                     'failed_when', 'args', 'environment', 'become_user'):
                        modules.add(key)

        site = yaml.safe_load((ROOT / 'site.yml').read_text())
        self.assertEqual(site[0]['roles'], ['s3a', 's3b'])
        for role in site[0]['roles']:
            tasks(ROOT / 'roles' / role / 'tasks/main.yml')
        all_source = '\n'.join(p.read_text() for p in visited)
        self.assertNotIn('mcp_data_bootstrap', modules)
        self.assertNotIn('data_bootstrap', all_source)
        self.assertNotRegex(all_source, r'(?i)\b(mkfs|sfdisk|parted|fdisk|tune2fs|pvcreate|wipefs)\b')
        for module in modules:
            if module.startswith('mcp_'):
                source = (ROOT / 'library' / (module + '.py')).read_text()
                self.assertNotIn('mcp_data_bootstrap', source)
                self.assertNotIn('mkfs', source)
        # The only storage-signature command reachable from normal convergence
        # is explicitly read-only (the probe never calls wipefs without no-act).
        shared = (ROOT / 'module_utils/mcp_data.py').read_text()
        self.assertIn("['/usr/sbin/wipefs', '--no-act', '--json', target]", shared)
        self.assertNotRegex(shared, r'\b(mkfs|tune2fs|sfdisk|parted)\b')
        bootstrap_tasks = yaml.safe_load((ROOT / 'roles/data_bootstrap/tasks/main.yml').read_text())
        self.assertIn('mcp_data_bootstrap_enabled | default(false) is sameas true',
                      bootstrap_tasks[0]['ansible.builtin.assert']['that'])
        self.assertNotIn('mcp_data_bootstrap_enabled', self.values)
        self.assertEqual(yaml.safe_load((ROOT / 'bootstrap-data.yml').read_text())[0]['roles'], ['data_bootstrap'])

    def test_rendered_persistent_paths_and_same_store(self):
        storage = tomllib.loads(self.render('storage.conf.j2'))['storage']
        engine_conf = tomllib.loads(self.render('containers.conf.j2'))
        engine = engine_conf['engine']
        self.assertEqual(storage['driver'], 'overlay')
        self.assertEqual(storage['options']['overlay']['mount_program'], '')
        self.assertEqual(storage['graphroot'], storage['rootless_storage_path'])
        for path in [storage['graphroot'], storage['rootless_storage_path'], engine['static_dir'],
                     engine['volume_path'], engine['image_copy_tmp_dir'], engine['events_logfile_path'],
                     self.values['mcp_runner_home'], self.values['mcp_workspace_data_dir'],
                     self.values['mcp_workspace_state_dir']]:
            self.assertTrue(path.startswith('/srv/mcp-podman/'), path)
        self.assertTrue(storage['runroot'].startswith('/run/user/1234/'))
        self.assertTrue(engine['tmp_dir'].startswith('/run/user/1234/'))
        self.assertEqual(engine['events_logfile_max_size'], '10m')
        self.assertEqual(engine_conf['containers']['log_size_max'], 10485760)
        journal = configparser.ConfigParser(interpolation=None)
        journal.read_string(self.render('journal-bounds.conf.j2'))
        self.assertEqual(journal['Journal']['SystemMaxUse'], '64M')
        self.assertEqual(journal['Journal']['SystemMaxFileSize'], '8M')
        env = dict(line.split('=', 1) for line in self.render('runtime.env.j2').splitlines() if '=' in line)
        for key in ['HOME', 'TMPDIR', 'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME', 'XDG_STATE_HOME']:
            self.assertTrue(env[key].startswith('/srv/mcp-podman/'))
        tasks = (ROOT / 'roles/s3b/tasks/main.yml').read_text()
        self.assertIn('/usr/bin/buildah', tasks)
        self.assertIn('buildah=1.33.7+ds1-*', tasks)
        self.assertIn('uidmap=1:4.13+*', tasks)
        self.assertIn('passt=0.0~git20240220.1e6f92b-*', tasks)
        self.assertIn('mcp_runtime_environment:', tasks)
        self.assertIn('install_recommends: false', tasks)
        self.assertIn('not item.stat.exists', tasks)
        self.assertIn('/usr/bin/fuse-overlayfs', tasks)
        self.assertEqual(env['PATH'], '/usr/bin:/bin')
        self.assertIn('mcp_runner_home', (ROOT / 'roles/s3a/tasks/main.yml').read_text())
        self.assertLess(tasks.index('Require mounted verified Data'), tasks.index('mcp_data_paths:'))
        paths = (ROOT / 'library/mcp_data_paths.py').read_text()
        self.assertIn('os.O_NOFOLLOW', paths)
        self.assertIn('dir_fd=parent', paths)
        self.assertIn('os.fchown(leaf, owner, group)', paths)
        self.assertIn("os.stat('.has-mount-program', dir_fd=leaf, follow_symlinks=False)", paths)
        self.assertIn('st.st_dev == base.st_dev', paths)

    @unittest.skipUnless(shutil.which('systemd-analyze'), 'systemd-analyze unavailable')
    def test_systemd_units_in_synthetic_root(self):
        # verify parses/loads dependencies but never starts units. Dummy
        # executables satisfy existence checks in a disposable synthetic root.
        with tempfile.TemporaryDirectory(prefix='s3b-static-units-') as directory:
            root = Path(directory)
            units = root / 'etc/systemd/system'
            units.mkdir(parents=True)
            for target in ['sysinit', 'basic', 'shutdown', 'local-fs', 'local-fs-pre', 'umount']:
                (units / (target + '.target')).write_text('[Unit]\nDefaultDependencies=no\n')
            for exe in ['usr/local/lib/mcp-workspaced/data-verify', 'bin/true']:
                path = root / exe
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('#!/bin/sh\nexit 0\n')
                path.chmod(0o755)
            (units / 'systemd-udev-settle.service').write_text(
                '[Unit]\nDefaultDependencies=no\n[Service]\nType=oneshot\nExecStart=/bin/true\n')
            names = []
            for template, name in [('data.mount.j2', 'srv-mcp\\x2dpodman.mount'),
                                   ('data-verify.service.j2', 'mcp-data-verify.service'),
                                   ('data-ready.service.j2', 'mcp-data-ready.service')]:
                (units / name).write_text(self.render(template))
                names.append(name)
            result = subprocess.run(['systemd-analyze', 'verify', '--root=' + directory,
                                     '--generators=no', '--man=no', *names],
                                    text=True, capture_output=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_mount_guard_and_ordering(self):
        mount = configparser.ConfigParser(interpolation=None)
        mount.read_string(self.render('data.mount.j2'))
        self.assertEqual(mount['Mount']['What'], '/dev/disk/by-uuid/' + UUID)
        self.assertEqual(mount['Mount']['Type'], 'ext4')
        self.assertEqual(mount['Unit']['Requires'], 'mcp-data-verify.service')
        self.assertEqual(mount['Unit']['After'], 'mcp-data-verify.service')
        unit = self.render('data-verify.service.j2')
        ready = self.render('data-ready.service.j2')
        self.assertIn('BindsTo=srv-mcp\\x2dpodman.mount', ready)
        self.assertIn('data-verify --mounted', ready)
        self.assertIn('RequiredBy=local-fs.target', ready)
        self.assertIn('Type=oneshot', unit)
        self.assertIn('Before=srv-mcp\\x2dpodman.mount', unit)
        self.assertIn('filesystem(snapshot,', self.render('data-verify.py.j2'))
        self.assertIn('guard(snapshot)', self.render('data-verify.py.j2'))
        shared = (ROOT / 'module_utils/mcp_data.py').read_text()
        self.assertIn('os.mkdir(MOUNT, 0o000)', shared)
        self.assertIn('st.st_uid == 0 and st.st_gid == 0', shared)
        self.assertIn('stat.S_IMODE(st.st_mode) == 0', shared)
        self.assertIn("command(['/usr/bin/mount', '--bind', '/srv', view])", shared)


if __name__ == '__main__':
    unittest.main()

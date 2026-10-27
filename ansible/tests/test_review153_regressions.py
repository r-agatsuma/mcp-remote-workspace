"""Bounded RV-153-01/02/03 counterexamples; no target or provider operations."""
from contextlib import contextmanager
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import types
import unittest
from unittest.mock import mock_open, patch

import yaml
import test_deployment_contract as deployment
import test_s3b_contract as storage

DEPLOY = deployment.DEPLOY


class RawSourceIdentity(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix='review153-git-')
        self.addCleanup(directory.cleanup)
        self.directory = Path(directory.name)
        self.repo = self.directory / 'repo'
        self.repo.mkdir()
        self.git('init', '--template=')
        self.files = {
            'ansible/policy.yml': (b'idle-ttl: 30m\n', 0o644),
            'ansible/roles/data_bootstrap/tasks/main.yml': (b'- name: reviewed bootstrap\n', 0o644),
            'scripts/deploy': (b'#!/bin/sh\nexit 0\n', 0o755),
            'binary.fixture': (bytes(range(256)), 0o644),
        }
        for name, (content, mode) in self.files.items():
            path = self.repo / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)
            path.chmod(mode)
        self.git('add', '.')
        self.git('commit', '-m', 'reviewed A')
        self.reviewed = self.git('rev-parse', 'HEAD').decode().strip()

    def git(self, *args):
        return subprocess.check_output(
            ['git', '-C', str(self.repo), '-c', 'user.name=Regression Test',
             '-c', 'user.email=test@example.invalid', '-c', 'commit.gpgSign=false',
             '-c', 'core.hooksPath=/dev/null', *args], stderr=subprocess.DEVNULL)

    def assert_raw_reviewed_snapshot(self, destination):
        expected_dirs = {str(parent) for name in self.files for parent in Path(name).parents
                         if str(parent) != '.'}
        members = {str(path.relative_to(destination)): path for path in destination.rglob('*')}
        self.assertEqual(set(members), set(self.files) | expected_dirs)
        for name in expected_dirs:
            self.assertTrue(stat.S_ISDIR(members[name].lstat().st_mode))
        for name, (content, mode) in self.files.items():
            path = members[name]
            self.assertTrue(stat.S_ISREG(path.lstat().st_mode))
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), mode)
            self.assertEqual(path.read_bytes(), content, name)

    def test_replace_ref_never_supplies_replacement_content(self):
        (self.repo / 'ansible/policy.yml').write_text('idle-ttl: 1h\n')
        self.git('add', '.')
        self.git('commit', '-m', 'changed B')
        replacement = self.git('rev-parse', 'HEAD').decode().strip()
        self.git('checkout', '--detach', self.reviewed)
        self.git('replace', self.reviewed, replacement)
        # This disposable checkout is clean under replacement semantics.
        self.git('read-tree', '--reset', '-u', self.reviewed)
        self.assertEqual(self.git('rev-parse', 'HEAD').decode().strip(), self.reviewed)
        self.assertEqual(self.git('status', '--porcelain', '--untracked-files=no'), b'')
        rejected = False
        try:
            DEPLOY['source_binding'](self.repo, self.reviewed)
        except ValueError:
            rejected = True
        destination = self.directory / 'snapshot'
        DEPLOY['snapshot'](self.repo, self.reviewed, destination)
        # Independent content oracle, even if binding rejects the checkout.
        self.assertEqual((destination / 'ansible/policy.yml').read_bytes(), b'idle-ttl: 30m\n')
        self.assert_raw_reviewed_snapshot(destination)
        self.assertTrue(rejected, 'replacement checkout must differ from the raw reviewed tree')

    def test_info_attributes_cannot_omit_a_committed_deployment_file(self):
        ignored = 'ansible/roles/data_bootstrap/tasks/main.yml'
        (self.repo / '.git/info').mkdir(exist_ok=True)
        (self.repo / '.git/info/attributes').write_text(ignored + ' export-ignore\n')
        self.assertIn(b'export-ignore: set', self.git('check-attr', 'export-ignore', '--', ignored))
        DEPLOY['source_binding'](self.repo, self.reviewed)
        destination = self.directory / 'snapshot'
        DEPLOY['snapshot'](self.repo, self.reviewed, destination)
        self.assertTrue((destination / ignored).is_file(), 'raw reviewed member must not disappear')
        self.assert_raw_reviewed_snapshot(destination)

    def test_materialized_member_type_mode_and_content_discrepancies_are_rejected(self):
        expected = {}
        for record in self.git('ls-tree', '-r', '-t', '-z', self.reviewed).split(b'\0'):
            if not record:
                continue
            header, name = record.split(b'\t', 1)
            mode, kind, oid = header.split()
            expected[Path(os.fsdecode(name))] = (b'40000' if kind == b'tree' else mode, oid.decode())
        for case in ('extra', 'missing', 'mode', 'content', 'type'):
            with self.subTest(case=case):
                destination = self.directory / case
                DEPLOY['snapshot'](self.repo, self.reviewed, destination)
                path = destination / 'ansible/policy.yml'
                if case == 'extra':
                    (destination / 'unexpected').write_text('unreviewed')
                elif case == 'missing':
                    path.unlink()
                elif case == 'mode':
                    path.chmod(0o755)
                elif case == 'content':
                    path.write_text('idle-ttl: 1h\n')
                else:
                    path.unlink()
                    path.symlink_to(destination / 'scripts/deploy')
                with self.assertRaisesRegex(ValueError, 'source snapshot'):
                    DEPLOY['verify_snapshot'](destination, expected)

    def test_unmaterializable_source_stops_formal_run_before_dns(self):
        (self.repo / 'linked').symlink_to('ansible/policy.yml')
        self.git('add', 'linked')
        self.git('commit', '-m', 'unsupported source type')
        revision = self.git('rev-parse', 'HEAD').decode().strip()
        manifest = deployment.BOOTSTRAP | {'mcp_source_revision': revision}
        mh, ih = DEPLOY['sha'](deployment.dump(manifest)), DEPLOY['sha'](deployment.dump(deployment.LOCATOR))
        grant = {'authorize': 'format-blank-data', 'source_revision': revision,
                 'manifest_sha256': mh, 'inventory_sha256': ih,
                 'host_key_sha256': manifest['mcp_target_ssh_host_key_sha256'],
                 'data_device_id': manifest['mcp_data_device_id']}
        for name, value in [('manifest.yml', manifest), ('locator.yml', deployment.LOCATOR), ('grant.yml', grant)]:
            DEPLOY['private_write'](self.directory / name, deployment.dump(value))
        main = DEPLOY['main']
        previous_umask = os.umask(0o077)
        try:
            with patch.dict(main.__globals__, ROOT=self.repo), \
                    patch.object(DEPLOY['shutil'], 'which', return_value='/synthetic/ansible-playbook'), \
                    patch.object(DEPLOY['socket'], 'getaddrinfo') as dns:
                with self.assertRaisesRegex(ValueError, 'unsupported'):
                    main(['run', 'bootstrap', '--manifest', str(self.directory / 'manifest.yml'),
                          '--inventory', str(self.directory / 'locator.yml'), '--manifest-sha256', mh,
                          '--inventory-sha256', ih, '--bootstrap-grant', str(self.directory / 'grant.yml'),
                          '--evidence-dir', str(self.directory / 'evidence')])
                dns.assert_not_called()
        finally:
            os.umask(previous_umask)


class EarlyDataMountpoint(unittest.TestCase):
    @contextmanager
    def observations(self, occupants):
        """Only OS observations are doubled; inspect/validate/filesystem/check run."""
        target = '/dev/synthetic-data'
        root_mount = {'target': '/', 'maj:min': '8:0', 'source': '/dev/synthetic-boot',
                      'fstype': 'ext4', 'options': 'rw'}
        header = ('Filesystem UUID: ' + storage.UUID + '\nBlock count: 100001\n'
                  'Reserved block count: 10000\nReserved blocks uid: 0 (user root)\n'
                  'Reserved blocks gid: 0 (group root)\n')

        def command(argv):
            if argv[0] == '/usr/bin/lsblk':
                devices = [{'maj:min': '8:0'}] if '--inverse' in argv else [
                    {'type': 'disk', 'size': storage.SIZE, 'log-sec': 512}]
                return json.dumps({'blockdevices': devices})
            if argv[0] == '/usr/bin/udevadm':
                return 'ID_SERIAL=' + storage.SERIAL + '\n'
            if argv[0] == '/usr/bin/findmnt':
                return json.dumps({'filesystems': [root_mount, *occupants]})
            if argv[0] == '/usr/sbin/wipefs' and '--no-act' in argv:
                return json.dumps({'signatures': [{'type': 'ext4', 'uuid': storage.UUID}]})
            if argv[0] == '/usr/sbin/blkid':
                return target + '\n'
            if argv[0] == '/usr/sbin/dumpe2fs':
                return header
            raise AssertionError('unexpected command, including any mutation: ' + str(argv))

        block = types.SimpleNamespace(st_mode=stat.S_IFBLK | 0o600, st_rdev=os.makedev(8, 16))
        with patch.object(storage.data, 'command', side_effect=command), \
                patch.object(storage.data.os.path, 'islink', side_effect=lambda path: path == storage.IDENTITY), \
                patch.object(storage.data.os.path, 'realpath', side_effect=lambda path: target if path == storage.IDENTITY else path), \
                patch.object(storage.data.os.path, 'exists', return_value=True), \
                patch.object(storage.data.os, 'stat', return_value=block), \
                patch.object(storage.data.os, 'listdir', return_value=[]), \
                patch('builtins.open', mock_open(read_data='Filename Type Size Used Priority\n')):
            yield

    def occupant(self, device):
        return {'target': storage.data.MOUNT, 'maj:min': device,
                'source': '/dev/synthetic-occupant', 'fstype': 'ext4', 'options': 'rw,nosuid,nodev'}

    def check(self):
        return storage.preflight.check(storage.IDENTITY, storage.SERIAL, storage.SIZE, 512, 'final', storage.UUID)

    def test_wrong_occupant_stops_in_the_gate_before_s3a_mutation(self):
        # Real Ansible expansion establishes the production gate's position;
        # no production task is executed by --list-tasks.
        listing = subprocess.check_output(
            ['ansible-playbook', '-i', 'localhost,', '--list-tasks', 'site.yml'],
            cwd=storage.ROOT, text=True)
        gate = 'Reobserve the stage-specific guest Data identity before any mutation'
        first_mutation = 's3a : Install distro-provided Git and Go bootstrap'
        self.assertLess(listing.index(gate), listing.index(first_mutation))
        play = yaml.safe_load((storage.ROOT / 'site.yml').read_text())[0]
        tasks = yaml.safe_load((storage.ROOT / 'tasks/deployment_preflight.yml').read_text())
        for item in [play, *play['pre_tasks'], *tasks]:
            self.assertFalse(any(key in item for key in ('ignore_errors', 'failed_when', 'rescue', 'when')))
        with self.observations([self.occupant('8:32')]):
            selected = storage.data.inspect(storage.IDENTITY)
            self.assertEqual(selected['mounts'], [])
            self.assertEqual(selected['foreign_mounts'], [])
            storage.data.filesystem(selected, storage.UUID)  # B itself is valid.
            with self.assertRaisesRegex(ValueError, 'Data mountpoint'):
                self.check()

    def test_unused_and_correctly_mounted_final_states_remain_allowed(self):
        for occupants in ([], [self.occupant('8:16')]):
            with self.subTest(occupants=occupants), self.observations(occupants):
                self.assertEqual(self.check()['mounts'], occupants)

    def test_ambiguous_or_uninspectable_backing_identity_is_rejected(self):
        for occupants in ([self.occupant('8:16')] * 2, [self.occupant(None)]):
            with self.subTest(occupants=occupants), self.observations(occupants):
                with self.assertRaisesRegex(ValueError, 'Data mountpoint'):
                    self.check()


class YAMLDirectives(unittest.TestCase):
    def test_unknown_directives_rejected_by_parse_with_valid_stage_bodies(self):
        for stage, value in [('bootstrap', deployment.BOOTSTRAP), ('final', deployment.FINAL)]:
            raw = deployment.dump(value)
            self.assertEqual(deployment.schema.validate(raw, stage), value)
            for prefix in (b'%FOO arbitrary\n---\n', b'%FOO arbitrary\n%FOO repeated\n---\n'):
                document = prefix + raw
                # Establish a lexically valid unknown-directive counterexample,
                # then test parse directly: no schema rejection can mask it.
                directives = [token for token in yaml.scan(document) if isinstance(token, yaml.tokens.DirectiveToken)]
                self.assertEqual(len(directives), prefix.count(b'%FOO'))
                with self.subTest(stage=stage, prefix=prefix):
                    with self.assertRaisesRegex(ValueError, '^YAML directives are forbidden$'):
                        deployment.schema.parse(document)
                    with self.assertRaisesRegex(ValueError, '^YAML directives are forbidden$'):
                        deployment.schema.validate(document, stage)

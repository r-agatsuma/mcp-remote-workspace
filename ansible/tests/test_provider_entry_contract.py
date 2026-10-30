"""#189 synthetic provider/guest fixtures, never provider or target evidence."""
import base64
import contextlib
from copy import deepcopy
from datetime import datetime, timezone
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from test_deployment_contract import DEPLOY, FINAL, LOCATOR, dump

# This is a synthetic grammar fixture, not an infrastructure identity.
RECORD = {'schema': 'mcp-oci-observation/1', 'instance_ocid': 'ocid1.instance.synthetic_fixture',
          'observed_at': '2026-01-01T00:00:00Z', 'lifecycle_state': 'RUNNING',
          'vnic_attachment_count': 1, 'private_ip_count': 1, 'ipv6_count': 1,
          'primary_private_ipv4': '10.0.0.5', 'public_ipv4': '11.12.13.14', 'gua_ipv6': '2600:1234::5'}
FINAL_LOCATOR = {key: value for key, value in LOCATOR.items() if key != 'host'}
GUEST = [{'ifname': 'eth0', 'flags': ['BROADCAST', 'UP'], 'addr_info': [
    {'family': 'inet', 'local': RECORD['primary_private_ipv4'], 'scope': 'global'},
    {'family': 'inet6', 'local': RECORD['gua_ipv6'], 'scope': 'global'}]}]


def encoded(value):
    return (json.dumps(value) + '\n').encode()


class ProviderRecord(unittest.TestCase):
    def test_valid_record_and_rfc1918_boundaries(self):
        self.assertEqual(DEPLOY['provider_record'](encoded(RECORD)), RECORD)
        for address in ['10.0.0.0', '10.255.255.255', '172.16.0.0', '172.31.255.255',
                        '192.168.0.0', '192.168.255.255']:
            DEPLOY['provider_record'](encoded(RECORD | {'primary_private_ipv4': address}))

    def test_each_missing_and_extra_key(self):
        for key in RECORD:
            bad = dict(RECORD)
            del bad[key]
            with self.subTest(missing=key), self.assertRaises(ValueError):
                DEPLOY['provider_record'](encoded(bad))
            with self.subTest(extra=key), self.assertRaises(ValueError):
                DEPLOY['provider_record'](encoded(RECORD | {key + '_extra': RECORD[key]}))
        for value in [[], None, 'record', 1]:
            with self.assertRaises(ValueError):
                DEPLOY['provider_record'](encoded(value))

    def test_wrong_types_values_counts_lifecycles_addresses_and_time(self):
        cases = {
            'schema': ['', 'mcp-oci-observation/2', 1, None],
            'instance_ocid': ['ocid1.instance.', 'ocid1.vnic.synthetic', '-argument',
                              'ocid1.instance.UPPER', 'ocid1.instance.a\n', 1],
            'lifecycle_state': ['STOPPED', 'running', None],
            'observed_at': ['2999-01-01T00:00:00Z', '2026-02-30T00:00:00Z',
                            '2026-01-01T24:00:00Z', '2026-01-01T00:00:00+00:00',
                            '2026-1-01T00:00:00Z', '2026-01-01T00:00:00.0Z', 1, None],
            'primary_private_ipv4': ['11.0.0.1', '172.15.255.255', '172.32.0.0', '192.169.0.0',
                                     '127.0.0.1', '169.254.0.1', '100.64.0.1', 'fd00::1',
                                     '10.0.0.01', '10.0.0.1/32', 'host.invalid', None],
            'public_ipv4': ['10.0.0.1', '192.0.2.1', '127.0.0.1', '224.0.0.1',
                            '168.63.129.16', '2600:1234::5', '::ffff:11.12.13.14', [], None],
            'gua_ipv6': ['11.12.13.14', 'fd00::1', 'fe80::1', '2001:db8::1',
                         '2002::1', '3fff::1', '4000::1', '2600:1234::5%eth0', None],
        }
        for key in ('vnic_attachment_count', 'private_ip_count', 'ipv6_count'):
            cases[key] = [0, 2, -1, True, False, 1.0, '1', None]
        for key, values in cases.items():
            for value in values:
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    DEPLOY['provider_record'](encoded(RECORD | {key: value}))

    def test_json_parsing_discipline_and_exact_size_boundary(self):
        good = encoded(RECORD)
        for raw in [b'', b'{', b'\xff', b'\xef\xbb\xbf' + good, good + b'{}',
                    good[:-2] + b',"schema":"mcp-oci-observation/1"}',
                    b'{"nested":{"x":1,"x":2}}', b'{"x":NaN}', b'{"x":Infinity}',
                    b'[' * 2000 + b']' * 2000, good + b' ' * 65536]:
            with self.subTest(raw=raw[:50]), self.assertRaises(ValueError):
                DEPLOY['provider_record'](raw)
        DEPLOY['provider_record'](good + b' ' * (65536 - len(good)))
        with self.assertRaises(ValueError):
            DEPLOY['provider_record'](good + b' ' * (65537 - len(good)))


class GuestJoin(unittest.TestCase):
    def test_exact_sets_across_interfaces_and_loopback_exclusion(self):
        links = [GUEST[0] | {'addr_info': GUEST[0]['addr_info'][:1]},
                 GUEST[0] | {'ifname': 'eth1', 'addr_info': GUEST[0]['addr_info'][1:]},
                 {'ifname': 'lo', 'flags': ['LOOPBACK'], 'addr_info': [
                     {'family': 'inet', 'local': '127.0.0.1', 'scope': 'global'}]}]
        DEPLOY['guest_join'](encoded(links), RECORD)
        # ip may include non-global entries; these cannot satisfy the global set.
        extra = {'family': 'inet6', 'local': 'fe80::1', 'scope': 'link'}
        DEPLOY['guest_join'](encoded([GUEST[0] | {'addr_info': GUEST[0]['addr_info'] + [extra]}]), RECORD)

    def test_extra_missing_ipv4_ipv6_and_malformed_data_stop(self):
        cases = [b'{', b'\xff', encoded({}), encoded([]), encoded([{}]), encoded([None])]
        for index, address in [(0, '10.0.0.6'), (1, '2600:1234::6')]:
            missing = deepcopy(GUEST)
            del missing[0]['addr_info'][index]
            cases.append(encoded(missing))
            extra = deepcopy(GUEST)
            extra[0]['addr_info'].append(extra[0]['addr_info'][index] | {'local': address})
            cases.append(encoded(extra))
            for change in [{'local': 'bad'}, {'family': 'other'}, {'scope': 'link'}]:
                bad = deepcopy(GUEST)
                bad[0]['addr_info'][index].update(change)
                cases.append(encoded(bad))
        for field in ['flags', 'ifname', 'addr_info']:
            bad = deepcopy(GUEST)
            del bad[0][field]
            cases.append(encoded(bad))
        for raw in cases:
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                DEPLOY['guest_join'](raw, RECORD)


class ConfirmationAndCLI(unittest.TestCase):
    def test_only_exact_y_continues_and_display_contains_required_facts(self):
        stdout = io.StringIO()
        with patch.object(DEPLOY['sys'].stdin, 'isatty', return_value=True), \
                patch('builtins.input', return_value='y'), contextlib.redirect_stdout(stdout):
            result = DEPLOY['confirm'](RECORD, FINAL['mcp_target_ssh_host_key_sha256'])
        self.assertEqual(result['answer'], 'y')
        self.assertLessEqual(datetime.strptime(result['confirmed_at'], '%Y-%m-%dT%H:%M:%SZ')
                             .replace(tzinfo=timezone.utc), datetime.now(timezone.utc))
        for fact in [*map(str, RECORD.values()), FINAL['mcp_target_ssh_host_key_sha256'],
                     'Guest join: PASS', 'every Security List', 'every NSG', 'protocol TCP or All',
                     'IPv6 source', 'same instance']:
            self.assertIn(fact, stdout.getvalue())
        for answer in ['', 'Y', 'yes', 'n', ' y', 'y ', 'y\n', 'y\x00']:
            stderr = io.StringIO()
            with self.subTest(answer=answer), patch.object(DEPLOY['sys'].stdin, 'isatty', return_value=True), \
                    patch('builtins.input', return_value=answer), contextlib.redirect_stdout(io.StringIO()), \
                    contextlib.redirect_stderr(stderr), self.assertRaisesRegex(ValueError, 'not confirmed'):
                DEPLOY['confirm'](RECORD, 'fingerprint')
            self.assertEqual(stderr.getvalue(), 'STOP: not confirmed\n')
        with patch.object(DEPLOY['sys'].stdin, 'isatty', return_value=True), \
                patch('builtins.input', side_effect=EOFError), contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()), self.assertRaisesRegex(ValueError, 'not confirmed'):
            DEPLOY['confirm'](RECORD, 'fingerprint')

    def test_non_tty_stops_without_prompt_or_display(self):
        with patch.object(DEPLOY['sys'].stdin, 'isatty', return_value=False), \
                patch('builtins.input') as prompt, patch('builtins.print') as display:
            with self.assertRaisesRegex(ValueError, 'tty'):
                DEPLOY['confirm'](RECORD, 'fingerprint')
            prompt.assert_not_called()
            display.assert_not_called()

    def test_stage_specific_locator(self):
        locator = DEPLOY['locator']
        self.assertEqual(locator(dump(LOCATOR), 'bootstrap'), LOCATOR)
        self.assertEqual(locator(dump(FINAL_LOCATOR), 'final'), FINAL_LOCATOR)
        for stage, value in [('final', LOCATOR), ('bootstrap', FINAL_LOCATOR), ('other', FINAL_LOCATOR)]:
            with self.subTest(stage=stage), self.assertRaises(ValueError):
                locator(dump(value), stage)
        for stage, good in [('bootstrap', LOCATOR), ('final', FINAL_LOCATOR)]:
            for key in good:
                bad = dict(good)
                del bad[key]
                with self.assertRaises(ValueError):
                    locator(dump(bad), stage)

    def test_final_hashes_optional_provider_required_bootstrap_unchanged(self):
        base = ['run', 'final', '--manifest', 'manifest', '--inventory', 'locator',
                '--evidence-dir', 'evidence']
        self.assertIsNone(DEPLOY['arguments'](base + ['--provider-record', 'record']).manifest_sha256)
        with self.assertRaises(ValueError):
            DEPLOY['arguments'](base)
        with self.assertRaises(ValueError):
            DEPLOY['arguments']([*base[:1], 'bootstrap', *base[2:], '--provider-record', 'record'])
        # Removed CLI input is accepted nowhere; this is solely a rejection test.
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            DEPLOY['arguments'](base + ['--final-observation', 'record'])


class FinalInvocation(unittest.TestCase):
    def run_final(self, failure=None, hashes=False):
        blob = base64.b64encode(b'synthetic public host key').decode()
        fingerprint = 'SHA256:' + base64.b64encode(hashlib.sha256(b'synthetic public host key').digest()).decode().rstrip('=')
        manifest = FINAL | {'mcp_target_ssh_host_key_sha256': fingerprint}
        provider_raw = b'  ' + encoded(RECORD)  # Preserve consumed bytes, not re-serialized JSON.
        guest_raw = encoded(GUEST)
        calls = []
        with tempfile.TemporaryDirectory(prefix='final-invocation-') as temporary:
            root = Path(temporary)
            for name, content in [('manifest.yml', dump(manifest)), ('locator.yml', dump(FINAL_LOCATOR)),
                                  ('record.json', provider_raw)]:
                DEPLOY['private_write'](root / name, content)

            def snapshot(source, revision, destination):
                (destination / 'ansible').mkdir(parents=True)

            def execute(command, **kwargs):
                calls.append(command)
                if command[0] == 'ssh-keyscan':
                    self.assertEqual(command[-1], RECORD['public_ipv4'])
                    # The entry must not reopen the record or locator after validation.
                    (root / 'record.json').write_bytes(b'{}')
                    (root / 'locator.yml').write_bytes(b'{}')
                    if failure == 'pin':
                        return subprocess.CompletedProcess(command, 0, stdout='')
                    return subprocess.CompletedProcess(command, 0, stdout='host ssh-ed25519 ' + blob + '\n')
                if command[0] == 'ssh':
                    self.assertEqual(command[-2], RECORD['public_ipv4'])
                    self.assertTrue(kwargs['check'])
                    if command[-1] == '/usr/bin/true':
                        if failure == 'possession':
                            raise subprocess.CalledProcessError(1, command)
                        return subprocess.CompletedProcess(command, 0)
                    self.assertEqual(command[-1], '/usr/sbin/ip -j addr show scope global')
                    self.assertEqual(command[1:-1], calls[1][1:-1])
                    if failure == 'guest-command':
                        raise subprocess.CalledProcessError(1, command)
                    return subprocess.CompletedProcess(command, 0, stdout=b'{}' if failure == 'guest-parse' else guest_raw)
                self.assertEqual(command[0], '/synthetic/ansible-playbook')
                self.assertEqual(command[3], 'site.yml')
                variables = json.loads(Path(command[-1][1:]).read_bytes())
                self.assertEqual(variables, manifest | {'mcp_host_public_ipv4': RECORD['public_ipv4'],
                                                       'mcp_host_gua_ipv6': RECORD['gua_ipv6']})
                self.assertNotIn(RECORD['instance_ocid'], json.dumps(variables))
                return subprocess.CompletedProcess(command, 0)

            argv = ['run', 'final', '--manifest', str(root / 'manifest.yml'),
                    '--inventory', str(root / 'locator.yml'), '--provider-record', str(root / 'record.json'),
                    '--evidence-dir', str(root / 'evidence')]
            if hashes or failure in ('manifest-hash', 'locator-hash'):
                argv += ['--manifest-sha256', 'f' * 64 if failure == 'manifest-hash' else DEPLOY['sha'](dump(manifest)),
                         '--inventory-sha256', 'f' * 64 if failure == 'locator-hash' else DEPLOY['sha'](dump(FINAL_LOCATOR))]
            previous_umask = os.umask(0o077)
            try:
                with patch.dict(DEPLOY['main'].__globals__, source_binding=lambda *args: None, snapshot=snapshot), \
                        patch.object(DEPLOY['shutil'], 'which', return_value='/synthetic/ansible-playbook'), \
                        patch.object(DEPLOY['socket'], 'getaddrinfo') as dns, \
                        patch.object(DEPLOY['subprocess'], 'run', side_effect=execute), \
                        patch.object(DEPLOY['sys'].stdin, 'isatty', return_value=failure != 'non-tty'), \
                        patch('builtins.input', return_value='n' if failure == 'answer' else 'y'), \
                        contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                    if failure:
                        with self.assertRaises((ValueError, subprocess.CalledProcessError)):
                            DEPLOY['main'](argv)
                    else:
                        self.assertEqual(DEPLOY['main'](argv), 0)
                    dns.assert_not_called()
            finally:
                os.umask(previous_umask)
            if failure:
                self.assertNotIn('/synthetic/ansible-playbook', [command[0] for command in calls])
                if failure in ('manifest-hash', 'locator-hash'):
                    self.assertEqual(calls, [])
                if failure in ('pin', 'possession'):
                    self.assertFalse(any(command[-1] == '/usr/sbin/ip -j addr show scope global' for command in calls))
                return
            evidence = root / 'evidence'
            self.assertEqual((evidence / 'provider-record.json').read_bytes(), provider_raw)
            self.assertEqual((evidence / 'guest-join.json').read_bytes(), guest_raw)
            binding = json.loads((evidence / 'binding.json').read_bytes())
            self.assertEqual(binding['provider_record_sha256'], DEPLOY['sha'](provider_raw))
            self.assertEqual(binding['guest_join_sha256'], DEPLOY['sha'](guest_raw))
            confirmation = json.loads((evidence / 'confirmation.json').read_bytes())
            self.assertEqual(confirmation['answer'], 'y')
            self.assertEqual(evidence.stat().st_mode & 0o777, 0o700)
            for path in evidence.iterdir():
                self.assertEqual(path.stat().st_mode & 0o777, 0o600, path)
            for name in ['validated-layer2.json', 'effective-inventory.json']:
                self.assertNotIn(RECORD['instance_ocid'], (evidence / name).read_text())
            self.assertEqual([c[0] for c in calls], ['ssh-keyscan', 'ssh', 'ssh', '/synthetic/ansible-playbook'])

    def test_final_invocation_binding_evidence_and_injected_variables(self):
        self.run_final()
        self.run_final(hashes=True)

    def test_all_external_gates_stop_before_mutation(self):
        for failure in ['pin', 'possession', 'guest-command', 'guest-parse', 'answer', 'non-tty',
                        'manifest-hash', 'locator-hash']:
            with self.subTest(failure=failure):
                self.run_final(failure)


if __name__ == '__main__':
    unittest.main()

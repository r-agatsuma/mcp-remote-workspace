"""#189 OCI CLI JSON fixtures are synthetic, never live provider evidence."""
import contextlib
from copy import deepcopy
import io
import json
import runpy
import subprocess
import unittest
from unittest.mock import patch

from test_deployment_contract import ROOT, DEPLOY
from test_provider_entry_contract import RECORD

OBSERVE = runpy.run_path(str(ROOT / 'scripts/oci-observe'))
# Grammar-only synthetic IDs, not infrastructure identities.
INSTANCE = RECORD['instance_ocid']
COMPARTMENT, VNIC, PRIVATE = 'ocid1.compartment.synthetic', 'ocid1.vnic.synthetic', 'ocid1.privateip.synthetic'
FIXTURES = [
    {'lifecycle-state': 'RUNNING', 'compartment-id': COMPARTMENT},
    [{'lifecycle-state': 'ATTACHED', 'vnic-id': VNIC}],
    {'lifecycle-state': 'AVAILABLE'},
    [{'is-primary': True, 'id': PRIVATE, 'ip-address': RECORD['primary_private_ipv4']}],
    {'private-ip-id': PRIVATE, 'ip-address': RECORD['public_ipv4']},
    [{'lifecycle-state': 'AVAILABLE', 'ip-address': RECORD['gua_ipv6']}],
]
COMMANDS = [
    ['oci', 'compute', 'instance', 'get', '--instance-id', INSTANCE],
    ['oci', 'compute', 'vnic-attachment', 'list', '--compartment-id', COMPARTMENT,
     '--instance-id', INSTANCE, '--all'],
    ['oci', 'network', 'vnic', 'get', '--vnic-id', VNIC],
    ['oci', 'network', 'private-ip', 'list', '--vnic-id', VNIC, '--all'],
    ['oci', 'network', 'public-ip', 'get', '--private-ip-id', PRIVATE],
    ['oci', 'network', 'ipv6', 'list', '--vnic-id', VNIC, '--all'],
]


class Observation(unittest.TestCase):
    def invoke(self, fixtures=None, fail_at=None, raw_at=None, argv=None, error=None):
        data = FIXTURES if fixtures is None else fixtures
        calls = []

        def runner(command):
            index = len(calls)
            calls.append(command)
            self.assertEqual(command, COMMANDS[index])
            if index == fail_at:
                raise error or subprocess.CalledProcessError(1, command, stderr=b'PRIVATE CLI STDERR')
            if raw_at is not None and index == raw_at[0]:
                return raw_at[1]
            return json.dumps({'data': data[index]}).encode()

        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = OBSERVE['main']([INSTANCE] if argv is None else argv, runner=runner)
        return result, stdout.getvalue(), stderr.getvalue(), calls

    def assert_stop(self, **kwargs):
        result, stdout, stderr, calls = self.invoke(**kwargs)
        self.assertNotEqual(result, 0)
        self.assertEqual(stdout, '')
        self.assertTrue(stderr.startswith('OBSERVE STOP: '), stderr)
        self.assertEqual(len(stderr.splitlines()), 1)
        self.assertNotIn('PRIVATE CLI STDERR', stderr)
        self.assertNotIn(INSTANCE, stderr)
        return calls

    def test_canonical_record_and_exact_read_only_paginated_command_sequence(self):
        result, stdout, stderr, calls = self.invoke()
        self.assertEqual(result, 0)
        self.assertEqual(stderr, '')
        self.assertEqual(calls, COMMANDS)
        record = DEPLOY['provider_record'](stdout.encode())
        self.assertEqual(record, RECORD | {'observed_at': record['observed_at']})
        self.assertEqual(stdout, json.dumps(record, sort_keys=True, separators=(',', ':')) + '\n')
        self.assertEqual(stdout.count('\n'), 1)

    def test_exactly_one_explicit_ocid_no_selection_or_flags(self):
        for argv in [[], [INSTANCE, INSTANCE], ['--help'], ['ocid1.instance.'],
                     ['ocid1.vnic.synthetic'], ['ocid1.instance.UPPER'], ['ocid1.instance.a\n']]:
            with self.subTest(argv=argv):
                self.assertEqual(self.assert_stop(argv=argv), [])

    def test_every_cli_failure_is_stop_without_stdout_or_retry(self):
        for index in range(6):
            for error in [subprocess.CalledProcessError(1, COMMANDS[index], stderr=b'PRIVATE CLI STDERR'),
                          FileNotFoundError('PRIVATE CLI STDERR'), subprocess.TimeoutExpired(COMMANDS[index], 1)]:
                with self.subTest(index=index, error=type(error)):
                    calls = self.assert_stop(fail_at=index, error=error)
                    self.assertEqual(calls, COMMANDS[:index + 1])

    def test_every_malformed_json_missing_data_or_wrong_container_is_stop(self):
        for index in range(6):
            for raw in [b'{', b'\xff', b'\xef\xbb\xbf{}', b'null', b'[]', b'{}',
                        b'{"data":{},"data":{}}', b'{"data":NaN}', b'{"data":Infinity}',
                        b'[' * 2000 + b']' * 2000]:
                with self.subTest(index=index, raw=raw):
                    self.assert_stop(raw_at=(index, raw))
            for data in [None, 1, 'bad', {}, []]:
                fixtures = deepcopy(FIXTURES)
                fixtures[index] = data
                with self.subTest(index=index, data=data):
                    self.assert_stop(fixtures=fixtures)

    def test_every_required_field_missing_is_stop(self):
        for index, value in enumerate(FIXTURES):
            item = value[0] if type(value) is list else value
            for key in item:
                fixtures = deepcopy(FIXTURES)
                target = fixtures[index][0] if type(value) is list else fixtures[index]
                del target[key]
                with self.subTest(index=index, missing=key):
                    self.assert_stop(fixtures=fixtures)

    def test_list_multiplicity_nonobject_members_and_attachment_other_states_stop(self):
        for index in [1, 3, 5]:
            for value in [[], FIXTURES[index] * 2, [None], [1], ['bad']]:
                fixtures = deepcopy(FIXTURES)
                fixtures[index] = value
                with self.subTest(index=index, value=value):
                    self.assert_stop(fixtures=fixtures)
        fixtures = deepcopy(FIXTURES)
        fixtures[1].append({'lifecycle-state': 'DETACHED', 'vnic-id': VNIC})
        self.assert_stop(fixtures=fixtures)

    def test_each_lifecycle_and_primary_flag_and_identifiers_stop(self):
        for index in [0, 1, 2, 5]:
            for state in ['TERMINATED', '', None, 1]:
                fixtures = deepcopy(FIXTURES)
                item = fixtures[index][0] if index in [1, 5] else fixtures[index]
                item['lifecycle-state'] = state
                with self.subTest(index=index, state=state):
                    self.assert_stop(fixtures=fixtures)
        for primary in [False, 1, 'true', None]:
            fixtures = deepcopy(FIXTURES)
            fixtures[3][0]['is-primary'] = primary
            self.assert_stop(fixtures=fixtures)
        for index, key in [(0, 'compartment-id'), (1, 'vnic-id'), (3, 'id')]:
            for value in ['', '--flag', 'ocid1.BAD', None, 1]:
                fixtures = deepcopy(FIXTURES)
                item = fixtures[index][0] if index in [1, 3] else fixtures[index]
                item[key] = value
                with self.subTest(index=index, key=key, value=value):
                    self.assert_stop(fixtures=fixtures)
        fixtures = deepcopy(FIXTURES)
        fixtures[4]['private-ip-id'] = 'ocid1.privateip.different'
        self.assert_stop(fixtures=fixtures)

    def test_private_public_gua_type_family_range_failures_stop(self):
        for index, addresses in [
            (3, ['11.0.0.1', '100.64.0.1', '127.0.0.1', 'fd00::1', '172.32.0.0']),
            (4, ['10.0.0.1', '192.0.2.1', '168.63.129.16', RECORD['gua_ipv6']]),
            (5, [RECORD['public_ipv4'], 'fd00::1', '4000::1', '2001:db8::1', '3fff::1']),
        ]:
            for address in addresses + ['bad', None, [], 1, 'fe80::1%eth0', '::ffff:11.12.13.14']:
                fixtures = deepcopy(FIXTURES)
                item = fixtures[index][0] if index in [3, 5] else fixtures[index]
                item['ip-address'] = address
                with self.subTest(index=index, address=address):
                    self.assert_stop(fixtures=fixtures)

    def test_default_runner_uses_cli_defaults_without_auth_files_or_mutation(self):
        command = COMMANDS[0]
        with patch.object(OBSERVE['subprocess'], 'run', return_value=subprocess.CompletedProcess(command, 0, stdout=b'{}')) as run:
            self.assertEqual(OBSERVE['cli'](command), b'{}')
        run.assert_called_once_with(command, capture_output=True, check=True)


if __name__ == '__main__':
    unittest.main()

"""Source/synthetic S3D evidence. No host firewall, sysctl, or DNS mutation."""
import importlib.util
import ipaddress
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

import yaml

ROOT = Path(__file__).resolve().parents[1]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


network = load('ansible.module_utils.mcp_network', ROOT / 'module_utils/mcp_network.py')
planner = load('mcp_network_plan_test', ROOT / 'library/mcp_network_plan.py')
HOSTS = ['11.12.13.14', '2600:1234::5']  # Synthetic host identities, no network use.
DNS = ['9.9.9.9', '2606:4700:4700::1111', '8.8.4.4']
BASE = '''*filter
:INPUT DROP [12:34]
:FORWARD DROP [0:0]
:OUTPUT ACCEPT [0:0]
:OPERATOR - [0:0]
-A INPUT -i lo -j ACCEPT
-A INPUT -p tcp --dport 22 -j ACCEPT
-A FORWARD -j OPERATOR
-A OUTPUT -j ACCEPT
-A OPERATOR -j DROP
COMMIT
*nat
:PREROUTING ACCEPT [0:0]
:INPUT ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
:POSTROUTING ACCEPT [0:0]
COMMIT
'''
BAD = [
    '', 'dns.example', '9.9.9.9:53', ' 9.9.9.9', '0.0.0.0', '0.1.2.3',
    '127.0.0.53', '10.0.0.1', '172.16.0.1', '192.168.1.1', '100.64.0.1',
    '169.254.169.254', '169.254.0.2', '100.100.100.200', '168.63.129.16',
    '192.0.0.1', '192.0.2.1', '192.88.99.1', '198.18.0.1', '198.51.100.1',
    '203.0.113.1', '224.0.0.1', '240.0.0.1', '255.255.255.255',
    '::', '::1', '::ffff:9.9.9.9', '::ffff:192.168.1.1', 'fd00::1',
    'fd00:ec2::254', 'fe80::1', 'fe80::1%eth0', '2606:4700:4700::1111%eth0',
    'fec0::1', 'ff02::1', '64:ff9b::a00:1', '64:ff9b:1::1', '100::1',
    '100:0:0:1::1', '2001::1', '2001:2::1', '2001:5::1', '2001:10::1',
    '2001:20::1', '2001:db8::1', '2002:a00:1::1', '3fff::1', '5f00::1',
]


def evaluate(rendered, address, uid, local=False, broadcast=False):
    """Small strict interpreter for rendered owner/destination/terminal rules.

    This is rule-order inspection, NOT packet/Netfilter evidence. Unknown
    generated syntax fails the test instead of being treated as an allow/deny.
    """
    lines = network.parse_save(rendered)['filter']
    output = [shlex.split(line) for line in lines if line.startswith('-A OUTPUT ')]
    hook = output[0]
    assert hook == ['-A', 'OUTPUT', '-m', 'owner', '--uid-owner', str(uid),
                    '-m', 'comment', '--comment', network.MARKER, '-j', network.CHAIN]
    target = ipaddress.ip_address(address)
    rules = [shlex.split(line)[2:] for line in lines if line.startswith('-A ' + network.CHAIN + ' ')]
    for rule in rules:
        if rule == ['-m', 'addrtype', '--dst-type', 'LOCAL', '-j', 'DROP']:
            if local:
                return 'DROP'
        elif rule == ['-m', 'addrtype', '--dst-type', 'BROADCAST', '-j', 'DROP']:
            if broadcast:
                return 'DROP'
        elif rule[:2] == ['!', '-d']:
            assert rule[3:] == ['-j', 'DROP']
            if target not in ipaddress.ip_network(rule[2]):
                return 'DROP'
        elif rule[0] == '-d':
            assert rule[2:] == ['-j', 'DROP']
            if target in ipaddress.ip_network(rule[1]):
                return 'DROP'
        else:
            assert rule == ['-j', 'ACCEPT']
            return 'ACCEPT'
    raise AssertionError('missing terminal decision')


class Inputs(unittest.TestCase):
    def test_ordered_public_literals_and_length_one_to_three(self):
        for count in [1, 2, 3]:
            self.assertEqual(network.inputs(947, 952, DNS[:count], HOSTS), (DNS[:count], HOSTS))
        self.assertEqual(network.inputs(20481, 31002, [DNS[1]], HOSTS)[0], [DNS[1]])

    def test_prohibited_literals_and_no_fallback(self):
        for address in BAD:
            with self.subTest(address=address), self.assertRaises(ValueError):
                network.inputs(947, 952, [address], HOSTS)

    def test_bad_lists_duplicates_self_and_identity(self):
        for values in [None, '', DNS[0], [], DNS + ['1.0.0.1'], [DNS[0], DNS[0]],
                       [DNS[1], '2606:4700:4700:0:0:0:0:1111'], [None], [42], [HOSTS[0]]]:
            with self.subTest(values=values), self.assertRaises(ValueError):
                network.inputs(947, 952, values, HOSTS)
        for hosts in [[], [HOSTS[0]] * 2, ['10.0.0.1'], ['host.example']]:
            with self.assertRaises(ValueError):
                network.inputs(947, 952, DNS, hosts)
        for uid, gid in [(0, 952), (947, 0), (-1, 952), (True, 952), ('947', 952),
                         (947, 2**31), (2**32, 952)]:
            with self.assertRaises(ValueError):
                network.inputs(uid, gid, DNS, HOSTS)


class Rendering(unittest.TestCase):
    def test_multiple_actual_identities_sysctl_and_rerun(self):
        for uid, gid in [(947, 952), (20481, 31002)]:
            for family in [4, 6]:
                rendered = network.render_rules(BASE, BASE, uid, family, HOSTS)
                self.assertEqual(network.render_rules(rendered, rendered, uid, family, HOSTS), rendered)
                self.assertEqual(network.render_rules(rendered, rendered, uid + 1, family, HOSTS).count(
                    f'--uid-owner {uid + 1} '), 1)
                self.assertEqual(network.unowned(network.parse_save(rendered)),
                                 network.unowned(network.parse_save(BASE)))
            self.assertEqual(network.ping_sysctl(uid, gid), f'net.ipv4.ping_group_range = {gid} {gid}\n')

    def test_public_native_traffic_and_special_use_are_not_overbroad(self):
        public = ['1.0.0.1', '8.8.8.8', '9.9.9.9', '100.63.255.254', '100.128.0.1',
                  '168.63.129.17', '172.32.0.1', '192.31.196.1', '192.52.193.1',
                  '192.175.48.1', '2001:4860:4860::8888', '2606:4700:4700::1111',
                  '2001:1::1', '2001:1::2', '2001:1::3', '2001:3::1',
                  '2001:4:112::1', '2001:30::1', '2620:4f:8000::1']
        for address in public:
            family = ipaddress.ip_address(address).version
            with self.subTest(address=address):
                rendered = network.render_rules(BASE, None, 947, family, HOSTS)
                self.assertEqual(evaluate(rendered, address, 947), 'ACCEPT')
                self.assertEqual(evaluate(rendered, address, 947, local=True), 'DROP')
                if family == 4:
                    self.assertEqual(evaluate(rendered, address, 947, broadcast=True), 'DROP')
        # No protocol/port filter restricts required public TCP/UDP/ICMP/ICMPv6.
        for family in [4, 6]:
            _, rules = network.runner_rules(947, family, HOSTS)
            self.assertTrue(all(' -p ' not in line and '--dport' not in line for line in rules))

    def test_every_prohibited_destination_precedes_generic_accept(self):
        for address in BAD + HOSTS:
            try:
                parsed = ipaddress.ip_address(address)
            except ValueError:
                continue
            if '%' in address:
                continue
            rendered = network.render_rules(BASE, BASE, 947, parsed.version, HOSTS)
            with self.subTest(address=address):
                self.assertEqual(evaluate(rendered, address, 947), 'DROP')
        for family in [4, 6]:
            _, rules = network.runner_rules(947, family, HOSTS)
            self.assertEqual(sum(line.endswith(' ACCEPT') for line in rules), 1)
            self.assertTrue(rules[-1].endswith(' ACCEPT'))
            self.assertNotIn('--dport 53', '\n'.join(rules))
            self.assertNotIn('ESTABLISHED', '\n'.join(rules))

    def test_policy_drift_incomplete_files_or_chain_collision_fail_closed(self):
        for value in [BASE.replace(':INPUT DROP', ':INPUT ACCEPT'),
                      BASE.replace('-A FORWARD -j OPERATOR\n', ''),
                      BASE.replace('COMMIT\n*nat', '*nat'),
                      BASE.replace(':FORWARD DROP [0:0]\n', ''),
                      '*filter\n:INPUT ACCEPT [0:0]\n', 'flush ruleset\n',
                      BASE.replace(':OPERATOR', ':MCP_S3D_OUTPUT'), '']:
            with self.subTest(value=value), self.assertRaises(ValueError):
                network.render_rules(BASE, value, 947, 4, HOSTS)
        # A successful empty live save means no explicit tables on a clean host.
        clean = network.render_rules('', None, 947, 6, HOSTS)
        self.assertEqual(evaluate(clean, DNS[1], 947), 'ACCEPT')
        # Preserve quoted operator rule data and exact non-owned rule ordering.
        quoted = BASE.replace('-A INPUT -i lo', '-A INPUT -m comment --comment "operator accepted input" -i lo')
        rendered = network.render_rules(quoted, quoted, 947, 4, HOSTS)
        self.assertIn('--comment "operator accepted input" -i lo', rendered)
        self.assertEqual(network.unowned(network.parse_save(rendered)),
                         network.unowned(network.parse_save(quoted)))
        ipv6 = network.render_rules(BASE, BASE, 947, 6, HOSTS)
        self.assertIn(f'-A {network.CHAIN} ! -d 2000::/3 -j DROP\n', ipv6)
        self.assertNotIn("'!'", ipv6)

    def test_direct_host_view_same_order_and_no_daemon_or_stub(self):
        dns, _ = network.inputs(947, 952, DNS, HOSTS)
        text = network.direct_resolv_conf(dns)
        self.assertEqual([line.split()[1] for line in text.splitlines() if line.startswith('nameserver ')], dns)
        self.assertNotIn('127.0.0.53', text)
        self.assertEqual(len(text.splitlines()), 4)
        for address in BAD:
            with self.assertRaises(ValueError):
                network.direct_resolv_conf([address])

    def test_persistent_loader_defaults_cannot_accumulate_or_override(self):
        source = '# operator comment\nIPTABLES_SKIP_SAVE=yes\nFLUSH_ON_STOP=1\nexport FLUSH_ON_STOP="2"\nIPTABLES_RESTORE_NOFLUSH=yes\n'
        rendered = network.persistence_defaults(source)
        self.assertIn('# operator comment\nIPTABLES_SKIP_SAVE=yes\n', rendered)
        self.assertEqual(rendered.count('FLUSH_ON_STOP='), 1)
        self.assertIn('FLUSH_ON_STOP=0\n', rendered)
        self.assertIn('IPTABLES_RESTORE_NOFLUSH=no\n', rendered)
        self.assertIn('IP6TABLES_RESTORE_NOFLUSH=no\n', rendered)
        self.assertIn('IPTABLES_TEST_RULESET=no\n', rendered)
        self.assertIn('IP6TABLES_TEST_RULESET=no\n', rendered)
        self.assertEqual(network.persistence_defaults(rendered), rendered)
        for bad in ['. /root/other.conf', 'FLUSH_ON_STOP=$(echo 1)', 'echo failure',
                    'PATH=/other', 'FLUSH_ON_STOP=0; echo failure', 'export', 'FLUSH_ON_STOP="${VALUE}"']:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                network.persistence_defaults(bad)


class Planner(unittest.TestCase):
    def run_plan(self, uid=947, gid=952, account=None, nss='hosts: files dns\n', assigned=None, saved=None):
        def read(path, optional=False):
            if path == '/etc/nsswitch.conf':
                return nss
            return None if path == '/etc/default/netfilter-persistent' else saved

        def execute(argv, data=None):
            if argv[0] == '/usr/sbin/ip':
                import json
                return json.dumps([{'addr_info': [{'local': value} for value in (
                    assigned if assigned is not None else ['127.0.0.1', '::1'])]}])
            if argv[-1] == '--version':
                return 'iptables v1.8.10 (nf_tables)'
            if argv[0].endswith('-legacy-save'):
                return ''
            if argv[0].endswith('-save'):
                return BASE
            if argv[-1] == '--test':
                self.assertIn('--uid-owner ' + str(uid), data)
                return ''
            self.fail('unexpected command: ' + repr(argv))

        with patch.object(planner.pwd, 'getpwnam', return_value=account or types.SimpleNamespace(pw_uid=uid, pw_gid=gid)), \
                patch.object(planner, 'trusted_read', side_effect=read), \
                patch.object(planner.os.path, 'realpath', return_value='/usr/sbin/xtables-nft-multi'), \
                patch.object(planner, 'command', side_effect=execute) as calls:
            result = planner.plan(uid, gid, DNS, HOSTS)
            self.assertEqual([call.args[0] for call in calls.call_args_list if call.args[0][-1] == '--test'], [
                ['/usr/sbin/iptables-nft-restore', '--test'], ['/usr/sbin/ip6tables-nft-restore', '--test']])
            return result

    def test_actual_account_check_and_both_families_parser_before_any_write(self):
        for uid, gid in [(947, 952), (20481, 31002)]:
            result = self.run_plan(uid, gid)
            self.assertEqual(result['public_dns'], DNS)
            self.assertEqual(result['ping_sysctl'], network.ping_sysctl(uid, gid))
        with self.assertRaisesRegex(ValueError, 'actual account'):
            self.run_plan(account=types.SimpleNamespace(pw_uid=1000, pw_gid=1001))
        with self.assertRaisesRegex(ValueError, 'assigned host'):
            self.run_plan(assigned=[DNS[0]])
        with self.assertRaisesRegex(ValueError, 'host address evidence'):
            self.run_plan(assigned=[])
        for nss in ['hosts: files resolve dns\n', 'hosts: files mdns4_minimal dns\n', '']:
            with self.assertRaisesRegex(ValueError, 'NSS'):
                self.run_plan(nss=nss)


class Source(unittest.TestCase):
    @unittest.skipUnless(shutil.which('ansible-playbook'), 'ansible-playbook unavailable')
    def test_real_ansible_identity_input_assertion_and_downstream_fact(self):
        production = yaml.safe_load((ROOT / 'roles/s3d/tasks/main.yml').read_text())
        gate = production[0]
        export = next(task for task in production if 'ansible.builtin.set_fact' in task)
        tasks = []
        for uid, gid in [(947, 952), (20481, 31002)]:
            tasks.extend([
                {'ansible.builtin.set_fact': dict(mcp_runner_account='mcp-runner', mcp_runner_uid=uid,
                    mcp_runner_primary_gid=gid, mcp_public_dns=DNS, mcp_host_public_ips=HOSTS,
                    mcp_network_plan={'public_dns': DNS})}, gate, export,
                {'ansible.builtin.assert': {'that': ['mcp_public_dns_validated == mcp_public_dns']}}])
        play = [{'name': 'S3D source-only Ansible fact interface', 'hosts': 'localhost',
                 'connection': 'local', 'gather_facts': False, 'become': False, 'tasks': tasks}]
        with tempfile.TemporaryDirectory(prefix='s3d-synthetic-') as directory:
            path = Path(directory) / 'contract.yml'
            path.write_text(yaml.safe_dump(play, sort_keys=False))
            result = subprocess.run(['ansible-playbook', '-i', 'localhost,', str(path)],
                                    env={**os.environ, 'ANSIBLE_NOCOLOR': '1'},
                                    text=True, capture_output=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_trust_chain_and_scope(self):
        tasks = yaml.safe_load((ROOT / 'roles/s3d/tasks/main.yml').read_text())
        plan = next(task for task in tasks if 'mcp_network_plan' in task)
        self.assertEqual(plan['mcp_network_plan'], dict(uid='{{ mcp_runner_uid }}', gid='{{ mcp_runner_primary_gid }}',
            resolvers='{{ mcp_public_dns }}', host_public_ips='{{ mcp_host_public_ips }}'))
        identity = (ROOT / 'roles/s3a/tasks/assert_created_identities.yml').read_text()
        for fact, index in [('mcp_runner_uid', 1), ('mcp_runner_primary_gid', 2)]:
            self.assertIn(f'{fact}: "{{{{ mcp_current_runner_passwd[{index}] | int }}}}"', identity)
        export = next(task['ansible.builtin.set_fact'] for task in tasks if 'ansible.builtin.set_fact' in task)
        self.assertEqual(export, {'mcp_public_dns_validated': '{{ mcp_network_plan.public_dns }}'})
        copies = [task['ansible.builtin.copy'] for task in tasks if 'ansible.builtin.copy' in task]
        self.assertEqual([copy['content'] for copy in copies], [
            '{{ mcp_network_plan.persistence_defaults }}', '{{ mcp_network_plan.rules[item] }}',
            '{{ mcp_network_plan.ping_sysctl }}', '{{ mcp_network_plan.resolv_conf }}'])
        self.assertTrue(all(copy['owner'] == copy['group'] == 'root' for copy in copies))
        self.assertEqual(copies[-1]['dest'], '/etc/resolv.conf')
        self.assertIs(copies[-1]['follow'], False)
        services = [task['ansible.builtin.systemd_service'] for task in tasks if 'ansible.builtin.systemd_service' in task]
        self.assertEqual(services, [{'name': 'netfilter-persistent.service', 'enabled': True, 'state': 'started'}])
        commands = [task['ansible.builtin.command']['argv'] for task in tasks if 'ansible.builtin.command' in task]
        self.assertEqual(commands, [['/usr/sbin/netfilter-persistent', 'start'],
                                   ['/usr/lib/systemd/systemd-sysctl', '--prefix=net/ipv4/ping_group_range']])
        self.assertFalse(any(task.get('become_user') for task in tasks))
        defaults = yaml.safe_load((ROOT / 'group_vars/all.yml').read_text())
        self.assertEqual(defaults['mcp_public_dns'], [])
        self.assertEqual(defaults['mcp_host_public_ips'], [])
        self.assertNotIn('mcp_runner_uid', defaults)
        self.assertNotIn('mcp_runner_primary_gid', defaults)


if __name__ == '__main__':
    unittest.main()

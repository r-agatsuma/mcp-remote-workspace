"""Source/synthetic S3D evidence. No host firewall, sysctl, or DNS mutation."""
import hashlib
import importlib.util
import ipaddress
import os
from pathlib import Path
import shlex
import shutil
import stat
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
ENDPOINT = yaml.safe_load((ROOT / 'group_vars/all.yml').read_text())['mcp_production_endpoint']
EVIDENCE = ROOT / 'tests/fixtures/issue55-grant6115658421'
HISTORICAL_LIVE = {family: (EVIDENCE / f'live.v{family}.exact').read_text() for family in (4, 6)}
HISTORICAL_PERSISTENT = {family: (EVIDENCE / f'rules.v{family}.exact').read_text() for family in (4, 6)}
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


def reply_rule(port):
    return (f'-A MCP_S3D_OUTPUT -s 127.0.0.1/32 -d 127.0.0.1/32 -o lo -p tcp -m conntrack '
            f'--ctorigsrc 127.0.0.1 --ctorigdst 127.0.0.1 --ctorigdstport {port} --ctdir REPLY -j ACCEPT')


def evaluate(rendered, address, uid, local=False, broadcast=False):
    """Small strict interpreter for ordinary (non-MCP-reply) destination policy.

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
        if rule == shlex.split(reply_rule(ENDPOINT['port']))[2:]:
            # The exact tuple/REPLY exception is checked separately. Ordinary
            # destination probes here do not model that TCP reply flow.
            continue
        elif rule == ['-m', 'addrtype', '--dst-type', 'LOCAL', '-j', 'DROP']:
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
    def test_shared_production_endpoint_reference_and_port_boundaries(self):
        self.assertEqual(ENDPOINT, {'address': '127.0.0.1', 'port': 8080})
        self.assertEqual(network.production_endpoint(ENDPOINT), ENDPOINT)
        for port in [1, 65535]:
            endpoint = dict(ENDPOINT, port=port)
            self.assertEqual(network.production_endpoint(endpoint), endpoint)

    def test_endpoint_fails_closed_without_coercion(self):
        invalid = [None, [], '127.0.0.1:8080', {}, {'address': '127.0.0.1'},
                   {'port': 8080}, dict(ENDPOINT, extra=True)]
        invalid += [dict(ENDPOINT, address=address) for address in
                    ['127.0.0.2', '127.1', '127.0.0.0/8', '0.0.0.0', '::1',
                     'localhost', '127.0.0.1/32', ' 127.0.0.1', None, 2130706433]]
        invalid += [dict(ENDPOINT, port=port) for port in
                    [None, True, False, '8080', '1', 8080.0, 1.5, 0, -1, 65536, [], {}]]
        for endpoint in invalid:
            with self.subTest(endpoint=endpoint):
                with self.assertRaises(ValueError):
                    network.production_endpoint(endpoint)
                # No valid family silently ignores invalid shared deployment input.
                for family in [4, 6]:
                    with self.assertRaises(ValueError):
                        network.render_rules(BASE, BASE, 947, family, HOSTS, endpoint)

    def test_ipv4_protocol_anycast_is_not_a_trusted_dns_default(self):
        # Egress eligibility does not imply deployment-default DNS eligibility.
        # Keep the stricter #111 resolver contract, including PCP/TURN anycast.
        for address in ['192.0.0.9', '192.0.0.10', '192.0.0.8', '192.0.0.170', '192.0.0.171']:
            with self.subTest(address=address):
                with self.assertRaises(ValueError):
                    network.inputs(947, 952, [address], HOSTS)
                with self.assertRaises(ValueError):
                    network.direct_resolv_conf([address])

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
    def test_exact_ipv4_exception_order_and_deployment_port(self):
        for uid in [947, 20481]:
            for port in [ENDPOINT['port'], 18443, 1, 65535]:
                endpoint = dict(ENDPOINT, port=port)
                rendered = network.render_rules(BASE, BASE, uid, 4, HOSTS, endpoint)
                lines = network.parse_save(rendered)['filter']
                hook = (f'-A OUTPUT -m owner --uid-owner {uid} -m comment '
                        '--comment mcp-s3d-output-v1 -j MCP_S3D_OUTPUT')
                expected = reply_rule(port)
                first = lines.index(hook)
                self.assertEqual(lines[first:first + 4], [hook, expected,
                    '-A MCP_S3D_OUTPUT -m addrtype --dst-type LOCAL -j DROP',
                    '-A MCP_S3D_OUTPUT -m addrtype --dst-type BROADCAST -j DROP'])
                self.assertEqual(rendered.count(expected + '\n'), 1)
                # Reconcile old owned policy when the trusted port changes.
                updated = network.render_rules(rendered, rendered, uid, 4, HOSTS,
                                               dict(endpoint, port=port % 65535 + 1))
                self.assertNotIn(expected + '\n', updated)

    def test_only_owned_additions_and_no_broad_accept_anywhere(self):
        # Existing operator policy may legitimately contain these constructs.
        base = BASE.replace('-A OUTPUT -j ACCEPT\n',
            '-A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n'
            '-A OUTPUT -p tcp --dport 8080 -j ACCEPT\n-A OUTPUT -j ACCEPT\n')
        for family in [4, 6]:
            rendered = network.render_rules(base, base, 947, family, HOSTS, ENDPOINT)
            parsed = network.parse_save(rendered)
            # Equality covers every table and ordered unowned rule, so a new
            # broad rule outside the reserved chain cannot evade these checks.
            self.assertEqual(network.unowned(parsed), network.unowned(network.parse_save(base)))
            hook, rules = network.runner_rules(947, family, HOSTS, ENDPOINT)
            base_rules = [line for values in network.parse_save(base).values() for line in values
                          if line.startswith('-A ')]
            additions = [line for values in parsed.values() for line in values
                         if line.startswith('-A ') and line not in base_rules]
            self.assertEqual(additions, [hook] + rules)
            self.assertEqual(hook, '-A OUTPUT -m owner --uid-owner 947 -m comment '
                             '--comment mcp-s3d-output-v1 -j MCP_S3D_OUTPUT')
            accepts = [line for line in additions if line.endswith(' -j ACCEPT')]
            self.assertEqual(accepts, ([reply_rule(ENDPOINT['port'])] if family == 4 else []) +
                             ['-A MCP_S3D_OUTPUT -j ACCEPT'])
            for token in ['ESTABLISHED', 'RELATED', '--ctstate', ' -m state ', '--sport', '--dport']:
                self.assertNotIn(token, '\n'.join(additions))
            for line in additions:
                self.assertNotIn('::1', [token.split('/')[0] for token in shlex.split(line)])
            remaining = rules[1:] if family == 4 else rules
            self.assertTrue(all(line.endswith(' -j DROP') for line in remaining[:-1]))
            self.assertNotIn('conntrack', '\n'.join(remaining))

    def test_landed_policy_unchanged_except_exact_ipv4_insertion(self):
        # SHA-256 of complete BASE/HOSTS/UID=947 render at landed #111 baseline
        # 0a60115dd290c4e54e940d549f6e806a1efeb9b1, captured before this edit.
        # This checks all tables, owned policy and ordering, including IPv6.
        baseline = {4: 'bbd68a915d1d1c7166762837e0e3dd1d3357bb21e7af4251c0445df25e2a6a52',
                    6: '0619405b7c0b46648cc2169d847e9a22c8363760f0bb50b01d1d2ef023e87127'}
        for family in [4, 6]:
            rendered = network.render_rules(BASE, BASE, 947, family, HOSTS, ENDPOINT)
            if family == 4:
                rendered = rendered.replace(reply_rule(ENDPOINT['port']) + '\n', '', 1)
            else:
                _, owned = network.runner_rules(947, 6, HOSTS, ENDPOINT)
                self.assertEqual(owned[0], '-A MCP_S3D_OUTPUT -m addrtype --dst-type LOCAL -j DROP')
                self.assertNotIn('conntrack', rendered)
                for line in owned:
                    self.assertNotIn('::1', [token.split('/')[0] for token in shlex.split(line)])
                self.assertEqual(network.render_rules(BASE, BASE, 947, 6, HOSTS,
                                 dict(ENDPOINT, port=18443)), rendered)
            self.assertEqual(hashlib.sha256(rendered.encode()).hexdigest(), baseline[family])

    def test_ipv4_protocol_suballocation_iana_oracle(self):
        # Independent expected outcomes from IANA, not production prefix lists:
        # https://www.iana.org/assignments/iana-ipv4-special-registry
        # PCP .9 and TURN .10 are Globally Reachable=True more-specific entries;
        # a blanket DROP of their non-public /24 parent is incorrect (P1-01).
        cases = {
            '192.0.0.9': 'ACCEPT', '192.0.0.10': 'ACCEPT',
            '192.0.0.8': 'DROP', '192.0.0.170': 'DROP', '192.0.0.171': 'DROP',
            '10.0.0.1': 'DROP', '169.254.0.2': 'DROP', '127.0.0.1': 'DROP',
            '169.254.169.254': 'DROP', '168.63.129.16': 'DROP',
            '192.0.2.1': 'DROP', '240.0.0.1': 'DROP', HOSTS[0]: 'DROP',
        }
        for uid in [947, 20481]:
            rendered = network.render_rules(BASE, BASE, uid, 4, HOSTS, ENDPOINT)
            for address, expected in cases.items():
                with self.subTest(uid=uid, address=address):
                    self.assertEqual(evaluate(rendered, address, uid), expected)
            # Audit the complete remainder independently of its prefix encoding.
            for last in range(256):
                with self.subTest(uid=uid, last=last):
                    expected = 'ACCEPT' if last in (9, 10) else 'DROP'
                    self.assertEqual(evaluate(rendered, f'192.0.0.{last}', uid), expected)

    def test_ipv4_protocol_anycast_cannot_bypass_independent_security_denies(self):
        for address in ['192.0.0.9', '192.0.0.10']:
            rendered = network.render_rules(BASE, BASE, 947, 4, HOSTS, ENDPOINT)
            with self.subTest(address=address):
                self.assertEqual(evaluate(rendered, address, 947, local=True), 'DROP')
                self.assertEqual(evaluate(rendered, address, 947, broadcast=True), 'DROP')
                host_rendered = network.render_rules(BASE, BASE, 947, 4, HOSTS + [address], ENDPOINT)
                self.assertEqual(evaluate(host_rendered, address, 947), 'DROP')
                owned = [shlex.split(line) for line in network.parse_save(host_rendered)['filter']
                         if line.startswith('-A ' + network.CHAIN + ' ')]
                self.assertEqual(owned[-1], ['-A', network.CHAIN, '-j', 'ACCEPT'])
                self.assertEqual(owned[0], shlex.split(reply_rule(ENDPOINT['port'])))
                self.assertTrue(all(rule[-2:] == ['-j', 'DROP'] for rule in owned[1:-1]))

    def test_multiple_actual_identities_sysctl_and_rerun(self):
        for uid, gid in [(947, 952), (20481, 31002)]:
            for family in [4, 6]:
                rendered = network.render_rules(BASE, BASE, uid, family, HOSTS, ENDPOINT)
                self.assertEqual(network.render_rules(rendered, rendered, uid, family, HOSTS, ENDPOINT), rendered)
                self.assertEqual(network.render_rules(rendered, rendered, uid + 1, family, HOSTS, ENDPOINT).count(
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
                rendered = network.render_rules(BASE, None, 947, family, HOSTS, ENDPOINT)
                self.assertEqual(evaluate(rendered, address, 947), 'ACCEPT')
                self.assertEqual(evaluate(rendered, address, 947, local=True), 'DROP')
                if family == 4:
                    self.assertEqual(evaluate(rendered, address, 947, broadcast=True), 'DROP')
        # Outside the exact reply, no protocol/port filter restricts public traffic.
        for family in [4, 6]:
            _, rules = network.runner_rules(947, family, HOSTS, ENDPOINT)
            remaining = rules[1:] if family == 4 else rules
            self.assertTrue(all(' -p ' not in line and '--dport' not in line for line in remaining))

    def test_every_prohibited_destination_precedes_generic_accept(self):
        for address in BAD + HOSTS:
            try:
                parsed = ipaddress.ip_address(address)
            except ValueError:
                continue
            if '%' in address:
                continue
            rendered = network.render_rules(BASE, BASE, 947, parsed.version, HOSTS, ENDPOINT)
            with self.subTest(address=address):
                self.assertEqual(evaluate(rendered, address, 947), 'DROP')
        for family in [4, 6]:
            _, rules = network.runner_rules(947, family, HOSTS, ENDPOINT)
            self.assertEqual(sum(line.endswith(' ACCEPT') for line in rules), 2 if family == 4 else 1)
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
                network.render_rules(BASE, value, 947, 4, HOSTS, ENDPOINT)
        # A successful empty live save means no explicit tables on a clean host.
        clean = network.render_rules('', None, 947, 6, HOSTS, ENDPOINT)
        self.assertEqual(evaluate(clean, DNS[1], 947), 'ACCEPT')
        # Preserve quoted operator rule data and exact non-owned rule ordering.
        quoted = BASE.replace('-A INPUT -i lo', '-A INPUT -m comment --comment "operator accepted input" -i lo')
        rendered = network.render_rules(quoted, quoted, 947, 4, HOSTS, ENDPOINT)
        self.assertIn('--comment "operator accepted input" -i lo', rendered)
        self.assertEqual(network.unowned(network.parse_save(rendered)),
                         network.unowned(network.parse_save(quoted)))
        ipv6 = network.render_rules(BASE, BASE, 947, 6, HOSTS, ENDPOINT)
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


class BaselineGate(unittest.TestCase):
    def render(self, live=HISTORICAL_LIVE, saved=HISTORICAL_PERSISTENT, uid=947, endpoint=ENDPOINT):
        return network.render_rule_pair(live, saved, uid, HOSTS, endpoint)

    def test_independent_archived_fixture_hashes_and_reviewed_identities(self):
        # Recorded on #131, independently verified against archived manifest and
        # baseline.json before copying these exact bytes. No production generator.
        raw = {
            'rules.v4.exact': '02bef6508e4f471a2d6ffffe849c3ad04ad87c9a101ab4785c18054d041d63b2',
            'rules.v6.exact': '8bc66a914076cc16f945fbef6ab0aa196ea0dd47380d1aada79205e48d0a0dd7',
            'live.v4.exact': '1a5568a30ca9c3e40c5b13c03f842c7a039a5491e0eec6921676c2fefccccfb5',
            'live.v6.exact': 'bc48341838e6dedf65cf9f15488ded8cfeee52bddc370f482aff4b76123d796f',
        }
        structural = {
            'rules.v4.exact': 'c310c48e912e93934bf74bda7bc2ec2757867334e6cc07c6bd3b74f5c4b596db',
            'live.v4.exact': '48475b2dde3592a726cdfeff06cf232043bbc3dfa94fd52340bda01009db9824',
            'live.v6.exact': '4851ec3ce356f13692cf237a40e28daf951feb5bc054716a24f28d403fa89e6d',
        }
        for filename, expected in raw.items():
            data = (EVIDENCE / filename).read_bytes()
            self.assertEqual(hashlib.sha256(data).hexdigest(), expected)
            if filename in structural:
                self.assertEqual(network.save_identity(network.parse_save(data.decode())), structural[filename])
        self.assertEqual(network.BASELINE_PERSISTENT4, structural['rules.v4.exact'])
        self.assertEqual(network.BASELINE_PERSISTENT6_RAW, raw['rules.v6.exact'])
        self.assertEqual(network.BASELINE_LIVE, {family: structural[f'live.v{family}.exact'] for family in (4, 6)})

    def test_original_landed_strict_comparator_rejects_measured_pair(self):
        self.assertIn('-p udp --dport 123 ', HISTORICAL_PERSISTENT[4])
        self.assertIn('-p udp -m udp --dport 123 ', HISTORICAL_LIVE[4])
        self.assertNotEqual(network.unowned(network.parse_save(HISTORICAL_PERSISTENT[4])),
                            network.unowned(network.parse_save(HISTORICAL_LIVE[4])))
        # render_rules retains the actual landed strict-equality code path.
        with self.assertRaisesRegex(ValueError, 'live/persistent unrelated policy differs'):
            network.render_rules(HISTORICAL_LIVE[4], HISTORICAL_PERSISTENT[4], 947, 4, HOSTS, ENDPOINT)
        with self.assertRaisesRegex(ValueError, 'non-save command'):
            network.parse_save(HISTORICAL_PERSISTENT[6])
        with self.assertRaisesRegex(ValueError, 'non-save command'):
            network.render_rules(HISTORICAL_LIVE[6], HISTORICAL_PERSISTENT[6], 947, 6, HOSTS, ENDPOINT)

    def test_exact_reviewed_pair_composes_only_from_live_and_keeps_hook_order(self):
        rendered = self.render()
        for family in (4, 6):
            canonical = network.unowned(network.parse_save(HISTORICAL_LIVE[family]))
            self.assertEqual(network.unowned(network.parse_save(rendered[family])), canonical)
            self.assertEqual(rendered[family], network.render_rules(
                HISTORICAL_LIVE[family], HISTORICAL_LIVE[family], 947, family, HOSTS, ENDPOINT))
            output = [line for line in network.parse_save(rendered[family])['filter']
                      if line.startswith('-A OUTPUT ')]
            self.assertEqual(output, [f'-A OUTPUT -m owner --uid-owner 947 -m comment '
                f'--comment {network.MARKER} -j {network.CHAIN}'] +
                [line for line in canonical['filter'] if line.startswith('-A OUTPUT ')])
            self.assertNotIn('\n-F ', rendered[family])
        self.assertIn('-p udp -m udp --dport 123 ', rendered[4])
        self.assertNotIn('-p udp --dport 123 ', rendered[4])

    def test_strict_canonical_rerun_owned_recomposition_and_partial_convergence(self):
        rendered = self.render()
        self.assertEqual(self.render(rendered, rendered), rendered)
        # Known original files plus already-owned live state are also bounded.
        self.assertEqual(self.render(rendered), rendered)
        for family in (4, 6):
            partial = dict(HISTORICAL_PERSISTENT)
            partial[family] = rendered[family]
            self.assertEqual(self.render(rendered, partial), rendered)
        updated = self.render(rendered, rendered, uid=20481, endpoint=dict(ENDPOINT, port=18443))
        for family in (4, 6):
            self.assertEqual(updated[family].count('--uid-owner 20481 '), 1)
            self.assertNotIn('--uid-owner 947 ', updated[family])
            self.assertEqual(network.unowned(network.parse_save(updated[family])),
                             network.unowned(network.parse_save(HISTORICAL_LIVE[family])))
        self.assertIn(reply_rule(18443), updated[4])
        self.assertNotIn(reply_rule(ENDPOINT['port']), updated[4])
        self.assertNotIn('--ctorigdstport', updated[6])

    def test_unknown_policy_presence_order_and_syntax_stop_even_if_both_sides_agree(self):
        for family in (4, 6):
            source = HISTORICAL_LIVE[family]
            rule = next(line for line in source.splitlines() if line.startswith('-A INPUT '))
            mutations = {
                'built-in policy': source.replace(':OUTPUT ACCEPT', ':OUTPUT DROP'),
                'addition': source.replace('COMMIT', '-A OUTPUT -p tcp --dport 443 -j ACCEPT\nCOMMIT'),
                'removal': source.replace(rule + '\n', ''),
                'order': source.replace(rule + '\n', '').replace('COMMIT', rule + '\nCOMMIT'),
                'unexpected chain': source.replace(':OUTPUT ACCEPT', ':FOREIGN - [0:0]\n:OUTPUT ACCEPT'),
                'unexpected table': source + '*raw\n:OUTPUT ACCEPT [0:0]\nCOMMIT\n',
                'incomplete': source.replace('COMMIT\n', ''),
                'restore command': source.replace('COMMIT', '-F INPUT\nCOMMIT'),
                'equivalent option order': source.replace('-A INPUT -i lo -j ACCEPT', '-A INPUT -j ACCEPT -i lo'),
            }
            if family == 4:
                mutations.update({
                    'destination': source.replace('169.254.0.2/32', '169.254.0.3/32', 1),
                    'protocol': source.replace('-p icmp', '-p udp', 1),
                    'port': source.replace('--dport 123 ', '--dport 124 '),
                    # UDP/123 without -m udp is the reviewed persistent identity;
                    # UDP/53 without it is a distinct, unreviewed variation.
                    'optional module': source.replace('-p udp -m udp --dport 53', '-p udp --dport 53'),
                })
            else:
                mutations.update({
                    'destination': source.replace('-A INPUT -i lo ', '-A INPUT -d ::1/128 -i lo ', 1),
                    'protocol': source.replace('-p udp -m udp', '-p tcp -m tcp'),
                    'port': source.replace('--dport 546 ', '--dport 545 '),
                    'optional module': source.replace('-p udp -m udp', '-p udp'),
                })
            for name, changed in mutations.items():
                self.assertNotEqual(changed, source, name)
                live = dict(HISTORICAL_LIVE)
                live[family] = changed
                saved = dict(HISTORICAL_PERSISTENT)
                saved[family] = changed
                for side, inputs in [('live', (live, HISTORICAL_PERSISTENT)),
                                     ('persistent', (HISTORICAL_LIVE, saved)), ('both', (live, saved))]:
                    with self.subTest(family=family, mutation=name, side=side), self.assertRaises(ValueError):
                        self.render(*inputs)

    def test_historical_persistent_drift_and_ipv6_raw_identity_are_exact(self):
        for family in (4, 6):
            source = HISTORICAL_PERSISTENT[family]
            variants = [source.replace(':OUTPUT ACCEPT', ':OUTPUT DROP'),
                        source.replace('COMMIT', '-A OUTPUT -j ACCEPT\nCOMMIT'), '', None]
            if family == 4:
                variants += [source.replace('--dport 123 ', '--dport 124 '),
                             source.replace('-p udp --dport 123', '-p udp -m udp -m udp --dport 123')]
            else:
                variants += [source + '# unreviewed raw bytes\n', source.replace('-F INPUT\n', ''),
                             source.replace('-F INPUT\n-F FORWARD', '-F FORWARD\n-F INPUT'),
                             source.replace('-F INPUT', '-F  INPUT'), source.replace('\n', '\r\n')]
            for changed in variants:
                saved = dict(HISTORICAL_PERSISTENT)
                saved[family] = changed
                with self.subTest(family=family, changed=changed), self.assertRaises(ValueError):
                    self.render(saved=saved)

    def test_reserved_chain_and_hook_collisions_in_either_family_or_side_stop(self):
        rendered = self.render()
        for family in (4, 6):
            source = HISTORICAL_LIVE[family]
            collisions = [source.replace('COMMIT', ':MCP_S3D_OUTPUT - [0:0]\nCOMMIT'),
                          source.replace('COMMIT', '-A OUTPUT -j MCP_S3D_OUTPUT\nCOMMIT'),
                          rendered[family].replace('--comment mcp-s3d-output-v1', '--comment foreign'),
                          rendered[family].replace(':MCP_S3D_OUTPUT - ', ':MCP_S3D_OUTPUT DROP '),
                          rendered[family].replace('COMMIT', '-A INPUT -j MCP_S3D_OUTPUT\nCOMMIT'),
                          source + '*nat\n:MCP_S3D_OUTPUT - [0:0]\nCOMMIT\n']
            for collision in collisions:
                for side in ('live', 'persistent'):
                    live, saved = dict(HISTORICAL_LIVE), dict(HISTORICAL_PERSISTENT)
                    (live if side == 'live' else saved)[family] = collision
                    with self.subTest(family=family, side=side), self.assertRaises(ValueError):
                        self.render(live, saved)

    def test_both_family_gates_precede_composition_and_no_unknown_bootstrap(self):
        for family in (4, 6):
            for source in ['', BASE]:
                live = dict(HISTORICAL_LIVE)
                saved = dict(HISTORICAL_PERSISTENT)
                live[family] = saved[family] = source
                with patch.object(network, 'render_rules') as compose:
                    with self.assertRaises(ValueError):
                        self.render(live, saved)
                    compose.assert_not_called()
            with self.assertRaises(ValueError):
                self.render({family: HISTORICAL_LIVE[family]}, HISTORICAL_PERSISTENT)

    def test_only_existing_save_counter_and_file_comment_normalization(self):
        live = {family: source.replace('[0:0]', '[42:1234]') + '# counter snapshot\n'
                for family, source in HISTORICAL_LIVE.items()}
        saved = dict(HISTORICAL_PERSISTENT)
        saved[4] = saved[4].replace('[0:0]', '[10:20]') + '# persistent comment\n'
        self.assertEqual(self.render(live, saved), self.render())


class Planner(unittest.TestCase):
    def test_trusted_read_preserves_exact_ipv6_bytes_including_line_endings(self):
        with tempfile.TemporaryDirectory(prefix='s3d-baseline-read-') as directory:
            path = Path(directory) / 'rules.v6'
            for source in [HISTORICAL_PERSISTENT[6], HISTORICAL_PERSISTENT[6].replace('\n', '\r\n')]:
                path.write_bytes(source.encode('utf-8'))
                # Synthetic root ownership metadata; no privileged file operation.
                info = types.SimpleNamespace(st_uid=0, st_gid=0, st_mode=stat.S_IFREG | 0o640)
                with patch.object(planner.os, 'fstat', return_value=info):
                    saved = planner.trusted_read(str(path))
                self.assertEqual(saved.encode('utf-8'), path.read_bytes())
                if '\r' in saved:
                    with self.assertRaises(ValueError):
                        network.render_rule_pair(HISTORICAL_LIVE,
                            {4: HISTORICAL_PERSISTENT[4], 6: saved}, 947, HOSTS, ENDPOINT)

    def run_plan(self, uid=947, gid=952, account=None, nss='hosts: files dns\n', assigned=None,
                 saved=HISTORICAL_PERSISTENT, endpoint=ENDPOINT, live=HISTORICAL_LIVE, restore_calls=None):
        def read(path, optional=False):
            if path == '/etc/nsswitch.conf':
                return nss
            return None if path == '/etc/default/netfilter-persistent' else saved[int(path[-1])]

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
                return live[6 if 'ip6tables' in argv[0] else 4]
            if argv[-1] == '--test':
                if restore_calls is not None:
                    restore_calls.append(argv)
                self.assertIn('--uid-owner ' + str(uid), data)
                return ''
            self.fail('unexpected command: ' + repr(argv))

        with patch.object(planner.pwd, 'getpwnam', return_value=account or types.SimpleNamespace(pw_uid=uid, pw_gid=gid)), \
                patch.object(planner, 'trusted_read', side_effect=read), \
                patch.object(planner.os.path, 'realpath', return_value='/usr/sbin/xtables-nft-multi'), \
                patch.object(planner, 'command', side_effect=execute) as calls:
            result = planner.plan(uid, gid, DNS, HOSTS, endpoint)
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

    def test_either_family_baseline_failure_prevents_all_restore_tests(self):
        for family in (4, 6):
            live = dict(HISTORICAL_LIVE)
            live[family] = live[family].replace(':OUTPUT ACCEPT', ':OUTPUT DROP')
            saved = dict(HISTORICAL_PERSISTENT)
            saved[family] = saved[family].replace(':OUTPUT ACCEPT', ':OUTPUT DROP')
            for kwargs in [dict(live=live), dict(saved=saved)]:
                calls = []
                with self.subTest(family=family, side=next(iter(kwargs))), self.assertRaises(ValueError):
                    self.run_plan(**kwargs, restore_calls=calls)
                self.assertEqual(calls, [])

    def test_shared_endpoint_reaches_renderer_and_invalid_input_stops_before_host_reads(self):
        for port in [1, ENDPOINT['port'], 18443, 65535]:
            endpoint = dict(ENDPOINT, port=port)
            with patch.object(planner, 'production_endpoint', wraps=network.production_endpoint) as validate:
                result = self.run_plan(endpoint=endpoint)
                validate.assert_called_once_with(endpoint)
            self.assertIn(reply_rule(port) + '\n', result['rules']['v4'])
            self.assertNotIn('--ctorigdstport', result['rules']['v6'])
        for endpoint in [dict(ENDPOINT, address='127.0.0.2'), dict(ENDPOINT, port='8080'),
                         dict(ENDPOINT, port=True), dict(ENDPOINT, port=65536), None]:
            with patch.object(planner, 'trusted_read') as read, patch.object(planner, 'command') as command:
                with self.assertRaises(ValueError):
                    planner.plan(947, 952, DNS, HOSTS, endpoint)
                read.assert_not_called()
                command.assert_not_called()

    def test_ansible_module_preserves_endpoint_member_types(self):
        # Run the real argument parser: nested type coercion must not turn a
        # string, boolean or float port into an accepted integer.
        from ansible.module_utils import basic
        for port in [ENDPOINT['port'], '8080', True, 8080.0]:
            endpoint = dict(ENDPOINT, port=port)
            params = dict(uid=947, gid=952, resolvers=DNS, host_public_ips=HOSTS, endpoint=endpoint)
            with patch.object(basic, '_load_params', return_value=params), \
                    patch.object(planner, 'plan', return_value={}) as plan, \
                    patch.object(basic.AnsibleModule, 'exit_json'), \
                    patch.object(basic.AnsibleModule, 'fail_json') as fail:
                planner.main()
                fail.assert_not_called()
                received = plan.call_args.args[-1]
                self.assertEqual(received, endpoint)
                self.assertIs(type(received['port']), type(port))
                if type(port) is not int:
                    with self.assertRaises(ValueError):
                        network.production_endpoint(received)


class Source(unittest.TestCase):
    def test_endpoint_preflight_precedes_every_host_mutation(self):
        tasks = yaml.safe_load((ROOT / 'roles/s3d/tasks/main.yml').read_text())
        index = next(i for i, task in enumerate(tasks)
                     if task['name'] == 'Validate trusted production MCP endpoint before host mutation')
        # All tasks through the unconditional preflight are assertion-only;
        # no module, import, block or conditional can mutate/bypass this prefix.
        for task in tasks[:index + 1]:
            self.assertEqual(set(task), {'name', 'ansible.builtin.assert'})
        first_mutation = next(i for i, task in enumerate(tasks) if 'ansible.builtin.debconf' in task)
        self.assertLess(index, first_mutation)
        self.assertTrue(all(set(task) <= {'name', 'ansible.builtin.assert', 'ansible.builtin.service_facts',
                                         'loop', 'when'} for task in tasks[:first_mutation]))

    @unittest.skipUnless(shutil.which('ansible-playbook'), 'ansible-playbook unavailable')
    def test_real_ansible_endpoint_preflight_rejects_before_continuation(self):
        production = yaml.safe_load((ROOT / 'roles/s3d/tasks/main.yml').read_text())
        preflight = next(task for task in production
                         if task['name'] == 'Validate trusted production MCP endpoint before host mutation')
        cases = [('missing endpoint', {}, True)]

        def case(name, endpoint, reject):
            cases.append((name, {'mcp_production_endpoint': endpoint}, reject))

        for port in [1, ENDPOINT['port'], 18443, 65535]:
            case(f'valid integer port {port}', dict(ENDPOINT, port=port), False)
        for index, endpoint in enumerate([None, [], '127.0.0.1:8080', {}, {'address': '127.0.0.1'},
                                          {'port': 8080}, dict(ENDPOINT, extra=True),
                                          {'address': '127.0.0.1', 'other': 8080}]):
            case(f'invalid mapping {index}', endpoint, True)
        for address in ['127.0.0.2', '127.1', '127.0.0.0/8', '0.0.0.0', '::1', 'localhost',
                        '127.0.0.1/32', ' 127.0.0.1', None, 2130706433]:
            case(f'invalid address {address!r}', dict(ENDPOINT, address=address), True)
        for port in [None, True, False, '8080', '1', 8080.0, 1.5, 0, -1, 65536, [], {}]:
            case(f'invalid port {port!r}', dict(ENDPOINT, port=port), True)

        plays = []
        for name, values, reject in cases:
            plays.append({'name': name, 'hosts': 'localhost', 'connection': 'local',
                'gather_facts': False, 'become': False, 'vars': values, 'tasks': [
                    {'ansible.builtin.set_fact': {'preflight_rejected': False, 'after_preflight': False}},
                    {'block': [preflight, {'ansible.builtin.set_fact': {'after_preflight': True}}],
                     'rescue': [{'ansible.builtin.set_fact': {
                         'preflight_rejected': True,
                         'failed_task': '{{ ansible_failed_task.name }}',
                         'failed_message': '{{ ansible_failed_result.msg }}'}}]},
                    {'ansible.builtin.assert': {'that': [
                        'preflight_rejected == ' + ('true' if reject else 'false'),
                        'after_preflight == ' + ('false' if reject else 'true'),
                        'not preflight_rejected or failed_task == expected_task',
                        'not preflight_rejected or failed_message == expected_message'],
                     }, 'vars': {'expected_task': preflight['name'],
                                 'expected_message': preflight['ansible.builtin.assert']['fail_msg']}}]})
        with tempfile.TemporaryDirectory(prefix='s3d-endpoint-preflight-') as directory:
            path = Path(directory) / 'contract.yml'
            path.write_text(yaml.safe_dump(plays, sort_keys=False))
            result = subprocess.run(['ansible-playbook', '-i', 'localhost,', str(path)],
                                    env={**os.environ, 'ANSIBLE_NOCOLOR': '1'},
                                    text=True, capture_output=True, timeout=60)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

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
            resolvers='{{ mcp_public_dns }}', host_public_ips='{{ mcp_host_public_ips }}', endpoint='{{ mcp_production_endpoint }}'))
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

    def test_endpoint_single_deployment_authority_and_downstream_interface(self):
        definitions = []
        for path in ROOT.rglob('*.yml'):
            for line in path.read_text().splitlines():
                if line.lstrip().startswith('mcp_production_endpoint:'):
                    definitions.append(path.relative_to(ROOT).as_posix())
        self.assertEqual(definitions, ['group_vars/all.yml'])
        for path in [ROOT / 'module_utils/mcp_network.py', ROOT / 'library/mcp_network_plan.py']:
            self.assertNotIn('8080', path.read_text())
        documentation = (ROOT / 'README.md').read_text()
        for consumer in ['mcp_production_endpoint', '#112', 'mcp-workspaced', 'Secure MCP Tunnel']:
            self.assertIn(consumer, documentation)


if __name__ == '__main__':
    unittest.main()

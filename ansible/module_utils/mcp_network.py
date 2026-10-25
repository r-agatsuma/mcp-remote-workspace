"""Pure S3D input validation and iptables-save composition; no host mutations.

Trusted inputs preserve current build_dns.go's native public resolver exclusions.
General egress separately preserves globally reachable IPv4 suballocations.
Do not use ipaddress.is_global: its special-use table varies by Python version.
"""
import hashlib
import ipaddress
import re
import shlex

CHAIN = 'MCP_S3D_OUTPUT'
MARKER = 'mcp-s3d-output-v1'
# Issue #130 / accepted #131 evidence: Issue #55, grant 6115658421.
# Save identities hash the ordered parse_save representation serialized as
# *table\n + lines joined by \n + \nCOMMIT\n, with no table/option sorting.
# Independent exact evidence and provenance live in tests/fixtures/.
BASELINE_LIVE = {
    4: '48475b2dde3592a726cdfeff06cf232043bbc3dfa94fd52340bda01009db9824',
    6: '4851ec3ce356f13692cf237a40e28daf951feb5bc054716a24f28d403fa89e6d',
}
BASELINE_PERSISTENT4 = 'c310c48e912e93934bf74bda7bc2ec2757867334e6cc07c6bd3b74f5c4b596db'
# IPv6's historical restore-only file is recognized by exact UTF-8 bytes,
# never parsed/interpreted as save syntax and never used for composition.
BASELINE_PERSISTENT6_RAW = '8bc66a914076cc16f945fbef6ab0aa196ea0dd47380d1aada79205e48d0a0dd7'
RESOLVER_DENY4 = (
    '0.0.0.0/8', '10.0.0.0/8', '100.64.0.0/10', '127.0.0.0/8',
    '169.254.0.0/16', '172.16.0.0/12', '192.0.0.0/24', '192.0.2.0/24',
    '192.88.99.0/24', '192.168.0.0/16', '198.18.0.0/15',
    '198.51.100.0/24', '203.0.113.0/24', '224.0.0.0/4', '240.0.0.0/4',
    # Azure platform virtual IP: not ordinary public Internet or a DNS exception.
    '168.63.129.16/32',
)
# IANA IPv4 special-purpose registry: PCP .9/32 and TURN .10/32 are globally
# reachable despite their non-public 192.0.0.0/24 parent. Deny only the remainder
# for general egress; trusted resolver defaults still reject the entire /24.
# No early ACCEPT: independent LOCAL/BROADCAST/host/platform denies still win.
# https://www.iana.org/assignments/iana-ipv4-special-registry
EGRESS_DENY4 = tuple(prefix for prefix in RESOLVER_DENY4 if prefix != '192.0.0.0/24') + (
    '192.0.0.0/29', '192.0.0.8/32', '192.0.0.11/32', '192.0.0.12/30',
    '192.0.0.16/28', '192.0.0.32/27', '192.0.0.64/26', '192.0.0.128/25',
)
# Native public IPv6 is restricted to 2000::/3 before these exclusions.
# This also denies unspecified/loopback, mapped IPv4, NAT64, discard, ULA,
# deprecated site-local, link-local, multicast and unallocated address space.
DENY6 = (
    '2001::/32', '2001:2::/48', '2001:10::/28', '2001:20::/28',
    '2001:db8::/32', '2002::/16', '3fff::/20',
)
# IANA's 2001::/23 parent is not globally reachable except more-specific
# allocations. Retain the globally reachable allocations also permitted by the
# current runtime contract. Generate only DROP prefixes for the remainder:
# these are public address classifications, never DNS-specific ACCEPT rules.
PUBLIC_PROTOCOL6 = ('2001:1::1/128', '2001:1::2/128', '2001:1::3/128',
                    '2001:3::/32', '2001:4:112::/48', '2001:30::/28')


def protocol_deny6():
    remaining = [ipaddress.ip_network('2001::/23')]
    for prefix in PUBLIC_PROTOCOL6:
        allowed = ipaddress.ip_network(prefix)
        remaining = [part for network in remaining
                     for part in (network.address_exclude(allowed) if allowed.subnet_of(network) else [network])]
    return tuple(str(network) for network in sorted(remaining))


DENY6 = tuple(str(network) for network in ipaddress.collapse_addresses(
    ipaddress.ip_network(prefix) for prefix in DENY6 + protocol_deny6()))


def require(condition, message):
    if not condition:
        raise ValueError('STOP: ' + message)


def identity(uid, gid):
    require(type(uid) is int and 0 < uid < 2**31, 'invalid actual runner UID')
    require(type(gid) is int and 0 < gid < 2**31, 'invalid actual primary GID')


def literal(value):
    require(isinstance(value, str) and '%' not in value, 'unscoped literal IP required')
    try:
        address = ipaddress.ip_address(value)
    except ValueError as exc:
        raise ValueError('STOP: literal IP required') from exc
    # Reject mapped addresses rather than letting family-dependent rendering
    # select a different policy. No rewriting/fallback on invalid input.
    require(not (address.version == 6 and address.ipv4_mapped), 'native literal IP required')
    return address


def public(address):
    ranges = RESOLVER_DENY4 if address.version == 4 else DENY6
    return (address.version == 4 or address in ipaddress.ip_network('2000::/3')) and not any(
        address in ipaddress.ip_network(prefix) for prefix in ranges)


def public_list(values, minimum, maximum, name):
    require(type(values) is list and minimum <= len(values) <= maximum, 'invalid ' + name + ' list length')
    addresses = [literal(value) for value in values]
    require(all(public(address) for address in addresses), 'prohibited ' + name + ' destination')
    result = [str(address) for address in addresses]
    require(len(set(result)) == len(result), 'duplicate ' + name + ' destination')
    return result


def inputs(uid, gid, resolvers, host_public_ips):
    identity(uid, gid)
    # The dedicated OCI host has public NAT addresses that need not appear on
    # an interface. Inventory must enumerate them; addrtype LOCAL covers all
    # currently assigned addresses independently, including later IPv6 changes.
    hosts = public_list(host_public_ips, 1, 64, 'host public IP')
    dns = public_list(resolvers, 1, 3, 'public resolver')
    require(not set(dns).intersection(hosts), 'resolver is the host itself')
    return dns, hosts


def production_endpoint(value):
    require(type(value) is dict and set(value) == {'address', 'port'}, 'invalid production MCP endpoint')
    require(value['address'] == '127.0.0.1', 'production MCP address must be exactly 127.0.0.1')
    require(type(value['port']) is int and 1 <= value['port'] <= 65535,
            'production MCP port must be an integer in 1..65535')
    return value


def runner_rules(uid, family, hosts, endpoint):
    require(type(uid) is int and 0 < uid < 2**31, 'invalid actual runner UID')
    require(family in (4, 6), 'invalid address family')
    endpoint = production_endpoint(endpoint)
    start = f'-A OUTPUT -m owner --uid-owner {uid} -m comment --comment {MARKER} -j {CHAIN}'
    rules = []
    if family == 4:
        address = endpoint['address']
        rules.append(f'-A {CHAIN} -s {address}/32 -d {address}/32 -o lo -p tcp -m conntrack '
                     f'--ctorigsrc {address} --ctorigdst {address} --ctorigdstport {endpoint["port"]} '
                     '--ctdir REPLY -j ACCEPT')
    rules.append(f'-A {CHAIN} -m addrtype --dst-type LOCAL -j DROP')
    if family == 4:
        # Directed broadcasts depend on assigned interface prefixes, not only
        # the fixed 255.255.255.255 limited-broadcast address.
        rules.append(f'-A {CHAIN} -m addrtype --dst-type BROADCAST -j DROP')
    if family == 6:
        rules.append(f'-A {CHAIN} ! -d 2000::/3 -j DROP')
    for prefix in (EGRESS_DENY4 if family == 4 else DENY6):
        rules.append(f'-A {CHAIN} -d {prefix} -j DROP')
    for host in hosts:
        address = literal(host)
        if address.version == family:
            rules.append(f'-A {CHAIN} -d {address}/{address.max_prefixlen} -j DROP')
    # Outside the exact production MCP reply above, all deny decisions precede
    # this family-neutral public protocol allowance.
    rules.append(f'-A {CHAIN} -j ACCEPT')
    return start, rules


def parse_save(source):
    """Accept only declarative save syntax; restore --test is the host parser.

    Counters/comments are deliberately excluded from the semantic comparison.
    Unrelated declarations and rule ordering are retained, including other tables.
    """
    tables = {}
    current = None
    for raw in source.splitlines():
        line = raw.strip()
        if not line or line.startswith('#'):
            continue
        if line.startswith('*'):
            require(current is None and re.fullmatch(r'\*[a-z]+', line), 'malformed table')
            current = line[1:]
            require(current not in tables, 'duplicate table')
            tables[current] = []
        elif line == 'COMMIT':
            require(current is not None, 'unexpected COMMIT')
            current = None
        else:
            require(current is not None, 'rule outside table')
            if line.startswith(':'):
                require(re.fullmatch(r':[A-Za-z0-9_.:+-]+ (ACCEPT|DROP|-) \[\d+:\d+\]', line), 'invalid chain declaration')
                line = re.sub(r'\[\d+:\d+\]$', '[0:0]', line)
            else:
                tokens = shlex.split(line)
                require(len(tokens) >= 4 and tokens[0] == '-A', 'non-save command in rule file')
                # Preserve the save grammar verbatim: restore is not a shell
                # parser. In particular shlex.join would introduce single
                # quotes around ! and comments, changing restore semantics.
            tables[current].append(line)
    require(current is None, 'incomplete rule file')
    return tables


def unowned(tables):
    """Strip only the reserved S3D chain and its single marked OUTPUT hook."""
    result = {table: list(lines) for table, lines in tables.items()}
    lines = result.setdefault('filter', [':INPUT ACCEPT [0:0]', ':FORWARD ACCEPT [0:0]', ':OUTPUT ACCEPT [0:0]'])
    declarations = [line for line in lines if line.startswith(':' + CHAIN + ' ')]
    refs = [line for line in lines if not line.startswith(':') and CHAIN in shlex.split(line)]
    if declarations or refs:
        require(declarations == [f':{CHAIN} - [0:0]'], 'reserved chain collision')
        hooks = [line for line in refs if shlex.split(line)[1] != CHAIN]
        require(len(hooks) == 1 and re.fullmatch(
            rf'-A OUTPUT -m owner --uid-owner [1-9][0-9]* -m comment --comment {MARKER} -j {CHAIN}', hooks[0]),
            'foreign reference to reserved chain')
    kept = [line for line in lines if line not in declarations and line not in refs]
    for chain in ('INPUT', 'FORWARD', 'OUTPUT'):
        require(len([line for line in kept if line.startswith(':' + chain + ' ')]) == 1,
                'incomplete filter built-in chains')
    require(len([line.split()[0] for line in kept if line.startswith(':')]) ==
            len(set(line.split()[0] for line in kept if line.startswith(':'))), 'duplicate chain')
    result['filter'] = kept
    # Another table referencing the reserved name is never owned by this slice.
    require(not any(CHAIN in shlex.split(line) or line.startswith(':' + CHAIN + ' ')
                    for table, values in result.items() if table != 'filter' for line in values),
            'reserved chain outside filter')
    return result


def render_rules(live, persistent, uid, family, hosts, endpoint):
    """Strict save composer; deployment must first pass render_rule_pair's gate."""
    base = unowned(parse_save(live))
    if persistent is not None:
        parsed = parse_save(persistent)
        require(bool(parsed), 'empty persistent rule file')
        require(base == unowned(parsed), 'live/persistent unrelated policy differs; operator reconciliation required')
    hook, owned = runner_rules(uid, family, hosts, endpoint)
    lines = base['filter']
    declarations = [line for line in lines if line.startswith(':')]
    rules = [line for line in lines if not line.startswith(':')]
    # First OUTPUT rule: even an existing broad ACCEPT cannot bypass the boundary.
    base['filter'] = declarations + [f':{CHAIN} - [0:0]', hook] + owned + rules
    return ''.join('*' + table + '\n' + '\n'.join(values) + '\nCOMMIT\n'
                   for table, values in base.items())


def save_identity(tables):
    representation = ''.join('*' + table + '\n' + '\n'.join(lines) + '\nCOMMIT\n'
                             for table, lines in tables.items())
    return hashlib.sha256(representation.encode('utf-8')).hexdigest()


def render_rule_pair(live, persistent, uid, hosts, endpoint):
    """Gate both families against reviewed identities before composing either.

    Historical persistent input and canonical live save are separate identities,
    not equivalent syntax. Only counters and file comments in save grammar have
    the existing parser's normalization. In particular no UDP or -F rewriting
    occurs here. Strict reruns must still retain the reviewed unowned baseline;
    matching unknown live/persistent state is not authority to adopt it.
    """
    require(set(live) == set(persistent) == {4, 6}, 'both firewall families required')
    for family in (4, 6):
        base = unowned(parse_save(live[family]))
        require(save_identity(base) == BASELINE_LIVE[family],
                f'unreviewed IPv{family} live baseline; operator reconciliation required')
        saved = persistent[family]
        require(saved is not None, f'missing IPv{family} persistent baseline')
        if family == 6 and hashlib.sha256(saved.encode('utf-8')).hexdigest() == BASELINE_PERSISTENT6_RAW:
            continue
        parsed = parse_save(saved)
        if family == 4 and save_identity(parsed) == BASELINE_PERSISTENT4:
            continue
        # Ordinary post-convergence reconciliation retains the strict comparator
        # and reserved-chain checks, bounded to the same canonical live baseline.
        require(bool(parsed) and base == unowned(parsed),
                f'unreviewed IPv{family} persistent baseline; operator reconciliation required')
    return {family: render_rules(live[family], live[family], uid, family, hosts, endpoint)
            for family in (4, 6)}


def direct_resolv_conf(resolvers):
    dns = public_list(resolvers, 1, 3, 'public resolver')
    return '# Managed by S3D: deployment-owned direct resolver view.\n' + ''.join(
        'nameserver ' + address + '\n' for address in dns)


def ping_sysctl(uid, gid):
    identity(uid, gid)
    return f'net.ipv4.ping_group_range = {gid} {gid}\n'


def persistence_defaults(source):
    """Compose the loader's shell assignments without executable overrides.

    Replace every owned assignment, including duplicate/exported forms. Keep
    comments and the two unrelated save controls; refuse uninspectable shell
    code/unknown configuration rather than executing it during provisioning.
    """
    owned = dict(FLUSH_ON_STOP='0', IPTABLES_RESTORE_NOFLUSH='no',
                 IP6TABLES_RESTORE_NOFLUSH='no', IPTABLES_TEST_RULESET='no',
                 IP6TABLES_TEST_RULESET='no')
    kept = []
    for line in (source or '').splitlines():
        words = shlex.split(line, comments=True)
        if not words:
            kept.append(line)
            continue
        if words[0] == 'export':
            words = words[1:]
        require(len(words) == 1 and re.fullmatch(r'[A-Z][A-Z0-9_]*=[A-Za-z0-9_./:-]*', words[0]),
                'non-declarative netfilter-persistent defaults')
        key, _ = words[0].split('=', 1)
        require(key in owned or key in ('IPTABLES_SKIP_SAVE', 'IP6TABLES_SKIP_SAVE'),
                'unknown netfilter-persistent default: ' + key)
        if key not in owned:
            kept.append(line)
    return '\n'.join(kept + [key + '=' + value for key, value in owned.items()]) + '\n'

"""Pure controller rendering. Complete *filter ownership is explicitly gated.

Other tables are never supplied to restore. The reviewed host baseline retains
INPUT/FORWARD and unrelated OUTPUT rules; the runner jump is first in OUTPUT.
"""
import ipaddress
import re


def normalize(text):
    lines = [re.sub(r'\[\d+:\d+\]', '[0:0]', line.strip())
             for line in text.splitlines()
             if line.strip() and not line.startswith('#')]
    # Save emits rules grouped by chain. Group ordering is not semantics, but
    # order WITHIN each chain is. Preserve that order for drift detection.
    declarations = sorted(line for line in lines if line.startswith(':'))
    chains = {}
    other = []
    for line in lines:
        if line.startswith('-A '):
            chains.setdefault(line.split()[1], []).append(line)
        elif not line.startswith(':'):
            other.append(line)
    if other != ['*filter', 'COMMIT']:
        raise ValueError('Only one complete filter table is supported')
    ordered = ['*filter'] + declarations
    for name in sorted(chains):
        ordered.extend(chains[name])
    return '\n'.join(ordered + ['COMMIT']) + '\n'


def render(base, family, uid, self_ips, prohibited, public_protocol):
    base = normalize(base)
    lines = base.splitlines()
    if lines[0] != '*filter' or lines[-1] != 'COMMIT' or lines.count('COMMIT') != 1:
        raise ValueError('Require reviewed filter-only iptables-save baseline')
    if ':OUTPUT ACCEPT [0:0]' not in lines:
        raise ValueError('Reviewed baseline must permit ordinary public egress')
    if any('MCP_RUNNER' in line or line.startswith(('-I ', '-D ', '-F ', '*nat'))
           for line in lines):
        raise ValueError('Unexpected baseline mutation or existing product chain')
    uid = int(uid)
    assert uid >= 1000 and family in (4, 6)
    chain = 'MCP_RUNNER'
    reject = 'REJECT --reject-with ' + ('icmp-port-unreachable' if family == 4 else 'icmp6-port-unreachable')
    rules = [f'-A OUTPUT -m owner --uid-owner {uid} -j {chain}',
             # Reply direction only: no broad established-connection bypass.
             f'-A {chain} -m conntrack --ctstate RELATED,ESTABLISHED --ctdir REPLY -j RETURN',
             f'-A {chain} -m addrtype --dst-type LOCAL -j {reject}']
    for address in self_ips:
        ip = ipaddress.ip_address(address)
        if ip.version == family:
            rules.append(f'-A {chain} -d {ip}/{ip.max_prefixlen} -j {reject}')
    for address in public_protocol:
        network = ipaddress.ip_network(address)
        assert network.version == family
        rules.append(f'-A {chain} -d {network} -j RETURN')
    for address in prohibited:
        network = ipaddress.ip_network(address)
        assert network.version == family
        rules.append(f'-A {chain} -d {network} -j {reject}')
    rules.append(f'-A {chain} -j RETURN')
    # Declarations must precede rules. Product jump precedes every host OUTPUT
    # acceptance rule, while the host baseline can still impose stricter policy.
    index = next((i for i, line in enumerate(lines) if line.startswith('-A ')), len(lines)-1)
    lines.insert(index, f':{chain} - [0:0]')
    lines[index+1:index+1] = rules
    return '\n'.join(lines) + '\n'


class FilterModule:
    def filters(self):
        return {'mcp_firewall': render, 'mcp_rules_normalize': normalize}

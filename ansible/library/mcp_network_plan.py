#!/usr/bin/python3
"""Read-only deployment planner; shipped transiently by Ansible, never installed."""
import json
import os
import pwd
import stat
import subprocess

from ansible.module_utils.basic import AnsibleModule
from ansible.module_utils.mcp_network import (
    direct_resolv_conf, inputs, literal, parse_save, persistence_defaults, ping_sysctl,
    production_endpoint, render_rules, require,
)


def trusted_read(path, optional=False):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        require(optional, 'missing trusted file: ' + path)
        return None
    with os.fdopen(fd, encoding='utf-8') as source:
        info = os.fstat(source.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and info.st_gid == 0
                and not info.st_mode & 0o022, 'unsafe trusted file: ' + path)
        value = source.read(1024 * 1024 + 1)
        require(len(value) <= 1024 * 1024, 'oversized trusted file: ' + path)
        return value


def command(argv, data=None):
    return subprocess.run(argv, input=data, text=True, capture_output=True, check=True).stdout


def plan(uid, gid, resolvers, hosts, endpoint):
    dns, hosts = inputs(uid, gid, resolvers, hosts)
    endpoint = production_endpoint(endpoint)
    account = pwd.getpwnam('mcp-runner')
    require((account.pw_uid, account.pw_gid) == (uid, gid), 'S3A facts differ from actual account')
    nss = trusted_read('/etc/nsswitch.conf')
    host_lines = [line.split('#', 1)[0].split() for line in nss.splitlines()
                  if line.split('#', 1)[0].strip().startswith('hosts:')]
    require(host_lines == [['hosts:', 'files', 'dns']],
            'direct host resolver requires NSS hosts: files dns; no alternate resolver path')
    addresses = json.loads(command(['/usr/sbin/ip', '-json', 'address', 'show']))
    assigned = [str(literal(entry['local'])) for link in addresses for entry in link['addr_info']]
    require(bool(assigned), 'missing host address evidence')
    require(not set(dns).intersection(assigned), 'resolver is an assigned host address')
    defaults = persistence_defaults(trusted_read('/etc/default/netfilter-persistent', optional=True))
    # Persistent plugins call generic names. Refuse a legacy alternative rather
    # than silently changing the existing host's firewall authority.
    for family in ('iptables', 'ip6tables'):
        for suffix in ('', '-save', '-restore'):
            generic = '/usr/sbin/' + family + suffix
            selected = '/usr/sbin/' + family + '-nft' + suffix
            require(os.path.realpath(generic) == os.path.realpath(selected), 'nft alternatives required: ' + generic)
        require('nf_tables' in command(['/usr/sbin/' + family + '-nft', '--version']), 'nft backend required')
        legacy = parse_save(command(['/usr/sbin/' + family + '-legacy-save']))
        require(all(all(line.startswith(':') and ' ACCEPT ' in line for line in lines)
                    for lines in legacy.values()), 'existing legacy firewall policy')
    files = {}
    for family, name in ((4, 'iptables'), (6, 'ip6tables')):
        path = '/etc/iptables/rules.v' + str(family)
        saved = trusted_read(path, optional=True)
        live = command(['/usr/sbin/' + name + '-nft-save'])
        rendered = render_rules(live, saved, uid, family, hosts, endpoint)
        # Both families must parse before either persistent file is changed.
        command(['/usr/sbin/' + name + '-nft-restore', '--test'], rendered)
        files['v' + str(family)] = rendered
    return dict(public_dns=dns, rules=files, resolv_conf=direct_resolv_conf(dns),
                ping_sysctl=ping_sysctl(uid, gid), persistence_defaults=defaults)


def main():
    module = AnsibleModule(argument_spec=dict(
        uid=dict(type='int', required=True), gid=dict(type='int', required=True),
        resolvers=dict(type='list', elements='str', required=True),
        host_public_ips=dict(type='list', elements='str', required=True),
        # Preserve raw member types: integer coercion would accept string/bool ports.
        endpoint=dict(type='dict', required=True)), supports_check_mode=True)
    try:
        result = plan(module.params['uid'], module.params['gid'], module.params['resolvers'],
                      module.params['host_public_ips'], module.params['endpoint'])
        module.exit_json(changed=False, **result)
    except Exception as exc:
        module.fail_json(msg='STOP: ' + str(exc))


if __name__ == '__main__':
    main()

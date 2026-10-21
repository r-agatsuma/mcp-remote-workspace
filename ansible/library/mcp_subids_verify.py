#!/usr/bin/python3
"""Validate explicit subordinate-ID ranges without reallocating existing state."""
import grp
import pwd
from ansible.module_utils.basic import AnsibleModule
from ansible.module_utils.mcp_data import require


def check(records, user, start, count, host_ids):
    require(start > 0 and count >= 65536 and start + count <= 2**32 - 1, 'invalid subordinate range')
    require(not any(start <= n < start + count for n in host_ids), 'subordinate range overlaps host identities')
    owned = []
    for line in records.splitlines():
        if not line or line.startswith('#'):
            continue
        name, lower, length = line.split(':')
        lower, length = int(lower), int(length)
        require(lower > 0 and length > 0, 'invalid existing subordinate-ID record')
        if name == user:
            owned.append((lower, length))
        else:
            require(start + count <= lower or lower + length <= start, 'subordinate range overlaps another allocation')
    require(not owned or owned == [(start, count)], 'existing runner subordinate ranges differ; no reassignment')


def main():
    module = AnsibleModule(argument_spec=dict(uid_start=dict(type='int', required=True),
        gid_start=dict(type='int', required=True), count=dict(type='int', required=True)), supports_check_mode=True)
    try:
        for path, start, ids in [('/etc/subuid', module.params['uid_start'], [p.pw_uid for p in pwd.getpwall()]),
                                 ('/etc/subgid', module.params['gid_start'], [g.gr_gid for g in grp.getgrall()])]:
            with open(path, encoding='utf-8') as f:
                check(f.read(), 'mcp-runner', start, module.params['count'], ids)
        module.exit_json(changed=False)
    except Exception as exc:
        module.fail_json(msg='STOP: ' + str(exc))


if __name__ == '__main__':
    main()

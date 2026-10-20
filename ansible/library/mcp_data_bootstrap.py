#!/usr/bin/python3
"""Separate first-provision entry. Never imported by normal convergence."""
import os
import stat
import subprocess
import uuid
from ansible.module_utils.basic import AnsibleModule
from ansible.module_utils.mcp_data import argument_spec, filesystem, inputs, inspect, require, validate


def bootstrap(enabled, identity, serial, size, sector):
    # Validate authority before even probing the device. Only a real boolean true
    # is accepted by the task assertions as well as this independent module gate.
    require(enabled is True, 'destructive bootstrap is default-disabled')
    inputs(identity, serial, size, sector)
    before = validate(inspect(identity), identity, serial, size, sector, True)
    # Pin the selected block device against symlink retargeting. Separately
    # probe exclusive availability; mke2fs performs its own exclusive busy check
    # and must not inherit our exclusive claim (which would make it self-busy).
    fd = os.open(before['target'], os.O_RDWR | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        st = os.fstat(fd)
        require(stat.S_ISBLK(st.st_mode) and '%d:%d' % (os.major(st.st_rdev), os.minor(st.st_rdev)) == before['dev'],
                'device changed before busy check')
        busy_fd = os.open('/proc/self/fd/%d' % fd, os.O_RDONLY | os.O_EXCL | os.O_CLOEXEC)
        os.close(busy_fd)
        after = validate(inspect(identity), identity, serial, size, sector, True)
        require(before == after, 'device evidence changed before formatting')
        created_uuid = str(uuid.uuid4())
        # No force, partition, erase-signature, retry or repair option exists.
        subprocess.run(['/usr/sbin/mkfs.ext4', '-m', '10', '-U', created_uuid,
                        '-E', 'root_owner=0:0', '/proc/self/fd/%d' % fd],
                       check=True, capture_output=True, text=True, pass_fds=(fd,), timeout=300,
                       env={'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LC_ALL': 'C'})
        result = validate(inspect(identity), identity, serial, size, sector, False)
        filesystem(result, created_uuid)
        return created_uuid
    finally:
        os.close(fd)


def main():
    spec = argument_spec()
    spec['enabled'] = dict(type='bool', default=False)
    module = AnsibleModule(argument_spec=spec, supports_check_mode=False)
    try:
        created_uuid = bootstrap(**module.params)
        module.exit_json(changed=True, uuid=created_uuid,
                         msg='Record this UUID as mcp_data_uuid; normal convergence mounts it without formatting.')
    except Exception as exc:
        module.fail_json(msg='STOP (no retry/repair): ' + str(exc))


if __name__ == '__main__':
    main()

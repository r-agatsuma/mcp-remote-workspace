#!/usr/bin/python3
"""Create known runtime directories through pinned Data directory descriptors."""
import os
import stat
from ansible.module_utils.basic import AnsibleModule
from ansible.module_utils.mcp_data import MOUNT, require

# Root controls the config ancestry across later template tasks. Writable runtime
# state has private runner-owned leaves; guests receive no host bind mounts.
ROOT_PATHS = ['runner', 'runner/.local', 'runner/.local/share', 'runner/.local/state',
    'runner/.local/share/containers', 'runner/.config', 'runner/.config/containers',
    'runner/.config/environment.d']
RUNNER_PATHS = ['runner/.local/state/mcp-workspaced', 'runner/.cache',
    'runner/.local/share/containers/storage', 'runner/.local/share/containers/storage/libpod',
    'runner/.local/share/containers/storage/volumes', 'tmp', 'events', 'workspaces']


def provision(uid, gid):
    require(uid > 0 and gid > 0, 'non-root runner required')
    require(os.path.ismount(MOUNT), 'Data is not mounted')
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
    root = os.open(MOUNT, flags)
    changed = False
    try:
        base = os.fstat(root)
        require(base.st_uid == 0 and base.st_gid == 0 and not base.st_mode & 0o022,
                'Data root must remain protected root:root')
        require(base.st_dev != os.stat('/srv').st_dev, 'Data must be separate from Boot')
        paths = sorted(ROOT_PATHS + RUNNER_PATHS, key=lambda path: (path.count('/'), path))
        for rel in paths:
            owner, group, mode = (0, 0, 0o755) if rel in ROOT_PATHS else (uid, gid, 0o700)
            parent = os.dup(root)
            try:
                components = rel.split('/')
                for part in components[:-1]:
                    child = os.open(part, flags, dir_fd=parent)
                    os.close(parent)
                    parent = child
                    require(os.fstat(parent).st_dev == base.st_dev, 'runtime ancestor leaves Data')
                created = False
                try:
                    os.mkdir(components[-1], mode, dir_fd=parent)
                    created = True
                    changed = True
                except FileExistsError:
                    pass
                leaf = os.open(components[-1], flags, dir_fd=parent)
                try:
                    if created:
                        os.fchown(leaf, owner, group)
                    if rel == 'runner/.local/share/containers/storage':
                        try:
                            os.stat('.has-mount-program', dir_fd=leaf, follow_symlinks=False)
                        except FileNotFoundError:
                            pass
                        else:
                            raise ValueError('STOP: existing store used a mount program; no conversion/repair')
                    st = os.fstat(leaf)
                    require(st.st_dev == base.st_dev and st.st_uid == owner and st.st_gid == group
                            and stat.S_IMODE(st.st_mode) == mode,
                            'unexpected existing runtime path ownership/mode/type: ' + rel)
                finally:
                    os.close(leaf)
            finally:
                os.close(parent)
        return changed
    finally:
        os.close(root)


def main():
    module = AnsibleModule(argument_spec=dict(uid=dict(type='int', required=True), gid=dict(type='int', required=True)))
    try:
        module.exit_json(changed=provision(**module.params))
    except Exception as exc:
        module.fail_json(msg='STOP: ' + str(exc))


if __name__ == '__main__':
    main()

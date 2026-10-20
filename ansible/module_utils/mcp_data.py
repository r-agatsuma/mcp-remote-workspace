"""One-shot Ansible storage inspection. No formatting or repair operations."""
import json
import os
import re
import stat
import subprocess
import tempfile

MOUNT = '/srv/mcp-podman'


def require(ok, reason):
    if not ok:
        raise ValueError('STOP: ' + reason)


def command(argv):
    result = subprocess.run(argv, check=True, capture_output=True, text=True,
                            timeout=30, env={'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LC_ALL': 'C'})
    return result.stdout


def inputs(identity, serial, size, sector):
    require(isinstance(identity, str) and re.fullmatch(r'/dev/disk/by-id/[A-Za-z0-9_.:+-]+', identity)
            and not re.search(r'-part[0-9]+$', identity), 'whole-device stable by-id identity required')
    require(isinstance(serial, str) and serial.strip() == serial and bool(serial), 'expected ID_SERIAL required')
    require(type(size) is int and size > 0, 'exact positive expected size in bytes required')
    require(sector in (512, 4096), 'expected logical sector size must be 512 or 4096')


def flatten(nodes):
    result = []
    for node in nodes:
        result.append(node)
        result.extend(flatten(node.get('children', [])))
    return result


def validate(snapshot, identity, serial, size, sector, blank):
    """Pure gate shared by the real probe and synthetic contract tests."""
    inputs(identity, serial, size, sector)
    require(snapshot['identity'] == identity and snapshot['serial'] == serial, 'target identity mismatch')
    require(snapshot['size'] == size and snapshot['sector'] == sector, 'unexpected target size/sector topology')
    require(snapshot['type'] == 'disk' and not snapshot['partitions']
            and not snapshot['slaves'], 'whole independent disk required')
    require(snapshot['root_known'] and snapshot['dev'] not in snapshot['boot_ancestry'], 'Boot/root ancestry or unknown root')
    require(not snapshot['holders'] and not snapshot['swap'], 'holder/active dependency')
    require(not snapshot['foreign_mounts'], 'mounted/active target outside Data mount')
    if blank:
        require(not snapshot['mounts'], 'bootstrap target is mounted')
        require(snapshot['signatures'] == [], 'unexpected existing storage signatures')
    return snapshot


def inspect(identity):
    require(os.path.islink(identity), 'stable by-id input is not a symlink')
    target = os.path.realpath(identity)
    st = os.stat(target)
    require(stat.S_ISBLK(st.st_mode), 'target is not a block device')
    dev = '%d:%d' % (os.major(st.st_rdev), os.minor(st.st_rdev))
    sys = '/sys/dev/block/' + dev
    devices = json.loads(command(['/usr/bin/lsblk', '--json', '--bytes', '--nodeps',
                                 '--output', 'TYPE,SIZE,LOG-SEC', target]))['blockdevices']
    require(len(devices) == 1, 'ambiguous target topology')
    properties = dict(line.split('=', 1) for line in command(
        ['/usr/bin/udevadm', 'info', '--query=property', '--name', target]).splitlines() if '=' in line)
    mounts = flatten(json.loads(command(['/usr/bin/findmnt', '--json', '--output',
                                         'TARGET,SOURCE,MAJ:MIN,FSTYPE,OPTIONS']))['filesystems'])
    ancestry = set()
    root_known = False
    for mount in mounts:
        if mount['target'] in ('/', '/boot', '/boot/efi'):
            mm = mount['maj:min']
            require(os.path.exists('/sys/dev/block/' + mm), 'Boot/root is not an inspectable block device')
            ancestors = flatten(json.loads(command(['/usr/bin/lsblk', '--inverse', '--json',
                '--output', 'MAJ:MIN', '/dev/block/' + mm]))['blockdevices'])
            ancestry.update(node['maj:min'] for node in ancestors)
            root_known |= mount['target'] == '/'
    signatures = json.loads(command(['/usr/sbin/wipefs', '--no-act', '--json', target]))['signatures']
    swap_devs = []
    with open('/proc/swaps', encoding='utf-8') as swaps:
        swap_lines = swaps.read().splitlines()[1:]
    for line in swap_lines:
        path = line.split()[0]
        swap_st = os.stat(path)
        # Includes swapfiles located on the selected filesystem.
        number = swap_st.st_rdev if stat.S_ISBLK(swap_st.st_mode) else swap_st.st_dev
        swap_devs.append('%d:%d' % (os.major(number), os.minor(number)))
    selected = [m for m in mounts if m['maj:min'] == dev]
    return dict(identity=identity, target=target, dev=dev, serial=properties.get('ID_SERIAL', ''),
                size=int(devices[0]['size']), sector=int(devices[0]['log-sec']), type=devices[0]['type'],
                partitions=[p for p in os.listdir(sys) if os.path.exists(sys + '/' + p + '/partition')],
                holders=os.listdir(sys + '/holders'), slaves=os.listdir(sys + '/slaves'),
                root_known=root_known, boot_ancestry=sorted(ancestry), swap=dev in swap_devs,
                mounts=selected, foreign_mounts=[m for m in selected if m['target'] != MOUNT],
                signatures=signatures)


def filesystem(snapshot, uuid):
    require(re.fullmatch(r'[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}', uuid or ''), 'explicit ext4 UUID required')
    matches = command(['/usr/sbin/blkid', '-c', '/dev/null', '-t', 'UUID=' + uuid, '-o', 'device']).splitlines()
    require(len(matches) == 1 and os.path.realpath(matches[0]) == snapshot['target'],
            'filesystem UUID is absent, duplicated or resolves to the wrong device')
    signatures = snapshot['signatures']
    require(len(signatures) == 1 and signatures[0]['type'] == 'ext4'
            and signatures[0]['uuid'] == uuid, 'selected UUID/ext4 signature mismatch or extra signature')
    header = command(['/usr/sbin/dumpe2fs', '-h', snapshot['target']])
    fields = dict(line.split(':', 1) for line in header.splitlines() if ':' in line)
    require(fields['Filesystem UUID'].strip() == uuid, 'ext4 UUID mismatch')
    require(int(fields['Reserved block count']) == int(fields['Block count']) // 10,
            'ext4 reserve is not 10 percent; no automatic tune/repair')
    require(fields['Reserved blocks uid'].strip().startswith('0 ')
            and fields['Reserved blocks gid'].strip().startswith('0 '), 'reserved blocks must remain root-only')
    for mount in snapshot['mounts']:
        require(mount['target'] == MOUNT and mount['fstype'] == 'ext4' and 'rw' in mount['options'].split(','),
                'wrong Data mount state')


def argument_spec():
    return dict(identity=dict(type='str', required=True), serial=dict(type='str', required=True),
                size=dict(type='int', required=True), sector=dict(type='int', required=True))


def check_guard(path):
    st = os.lstat(path)
    require(stat.S_ISDIR(st.st_mode) and st.st_uid == 0 and st.st_gid == 0
            and stat.S_IMODE(st.st_mode) == 0, 'Boot-side guard must be root:root mode 000')
    require(not os.listdir(path), 'Boot-side guard contains unexpected state')


def guard(snapshot, create=False):
    # A non-recursive parent bind exposes the Boot directory underneath an
    # already-mounted Data filesystem, without unmounting Data or copying it.
    parent = os.lstat('/srv')
    require(stat.S_ISDIR(parent.st_mode) and parent.st_uid == 0
            and not parent.st_mode & 0o022, '/srv must be a protected real directory')
    all_mounts = command(['/usr/bin/findmnt', '--raw', '--noheadings', '--output', 'TARGET']).splitlines()
    mounted = MOUNT in all_mounts
    require(mounted == bool(snapshot['mounts']), 'Data mount is backed by the wrong device')
    if mounted:
        with tempfile.TemporaryDirectory(prefix='mcp-data-guard-', dir='/run') as view:
            command(['/usr/bin/mount', '--bind', '/srv', view])
            try:
                check_guard(view + '/mcp-podman')
            finally:
                command(['/usr/bin/umount', view])
        return False
    if not os.path.lexists(MOUNT):
        require(create, 'Boot-side guard is absent')
        os.mkdir(MOUNT, 0o000)
        return True
    check_guard(MOUNT)
    return False

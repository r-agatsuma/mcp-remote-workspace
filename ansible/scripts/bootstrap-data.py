#!/usr/bin/python3
"""DESTRUCTIVE first-use provisioning only. Never called by ordinary deploy."""
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import uuid


def output(*args):
    return subprocess.check_output(args, text=True)


def require(condition):
    if not condition:
        raise ValueError('unsafe device precondition')


def main(device, identity, serial, size):
    require(str(uuid.UUID(identity)) == identity)
    require(device.startswith('/dev/disk/by-id/'))
    actual = Path(device).resolve(strict=True)
    require(stat.S_ISBLK(actual.stat().st_mode))
    require(not Path('/dev/disk/by-uuid', identity).exists())
    disks = json.loads(output('lsblk', '--json', '--bytes', '--output',
                              'PATH,TYPE,SIZE,SERIAL,MOUNTPOINTS', str(actual)))['blockdevices']
    require(len(disks) == 1)
    disk = disks[0]
    require(disk['type'] == 'disk' and not disk.get('children'))
    require(int(disk['size']) == int(size) and disk['serial'] == serial)
    require(not any(disk['mountpoints']))
    require(not list(Path('/sys/class/block', actual.name, 'holders').iterdir()))
    require(str(actual) not in Path('/proc/swaps').read_text())
    signatures = json.loads(output('wipefs', '--no-act', '--json', str(actual)))['signatures']
    require(signatures == [])
    # Linux exclusive block-device open refuses a mounted/in-use device.
    fd = os.open(actual, os.O_RDONLY | os.O_EXCL)
    os.close(fd)
    # Maintenance precondition: no concurrent disk/partition/mount changes.
    # No signature wiping, repartitioning, automatic rerun, or pool reset.
    subprocess.run(['mkfs.ext4', '-F', '-m', '10', '-U', identity, str(actual)], check=True)


if __name__ == '__main__':
    try:
        main(*sys.argv[1:])
    except (AssertionError, OSError, ValueError, KeyError, TypeError,
            subprocess.SubprocessError):
        raise SystemExit('Bootstrap refused or incomplete; inspect device, never retry blindly')

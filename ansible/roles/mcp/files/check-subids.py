#!/usr/bin/python3
"""Provisioning-only read-only subordinate-ID conflict check."""
from pathlib import Path
import pwd
import grp
import sys

def require(condition):
    if not condition:
        raise SystemExit('Unsafe or overlapping subordinate-ID assignment')


start = int(sys.argv[1])
end = start + 65536
require(100000 <= start < end <= 2**32 - 1)
ids = [int(v) for v in sys.argv[2:]]
ids += [p.pw_uid for p in pwd.getpwall()]
ids += [g.gr_gid for g in grp.getgrall()]
require(not any(start <= ident < end for ident in ids))
for file in ('/etc/subuid', '/etc/subgid'):
    entries = Path(file).read_text().splitlines() if Path(file).exists() else []
    runner = 0
    for line in entries:
        if not line.strip() or line.startswith('#'):
            continue
        name, lower, count = line.split(':')
        lower, count = int(lower), int(count)
        if name == 'mcp-runner':
            runner += 1
            require(lower == start and count == 65536 and runner == 1)
        else:
            require(end <= lower or lower + count <= start)

#!/usr/bin/python3
"""Provisioning-only inspection; no sysctl writes."""
from pathlib import Path
import re

files = [Path('/etc/sysctl.conf')]
for directory in ('/etc/sysctl.d', '/run/sysctl.d', '/usr/local/lib/sysctl.d',
                  '/usr/lib/sysctl.d'):
    files.extend(Path(directory).glob('*.conf'))
for file in files:
    if str(file) == '/etc/sysctl.d/90-mcp-runner.conf' or not file.exists():
        continue
    if re.search(r'^\s*net[./]ipv4[./]ping_group_range\s*=', file.read_text(), re.M):
        raise SystemExit('Competing ping_group_range source; operator review required')

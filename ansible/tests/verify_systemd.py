#!/usr/bin/env python3
"""Offline unit parser/ordering check in a disposable synthetic root.

Dependency units and executable placeholders are fixtures, never started. This
is not verification of Ubuntu runtime behavior, permissions or namespaces.
"""
from pathlib import Path
import shutil
import subprocess
import tempfile

from test_deployment import render_all


def put(root, path, content, mode=0o644):
    target = root / path.lstrip('/')
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content)
    target.chmod(mode)


def main():
    if not shutil.which('systemd-analyze'):
        print('SKIPPED — tooling unavailable: systemd-analyze')
        return
    with tempfile.TemporaryDirectory(prefix='mcp-unit-verify-') as directory:
        root = Path(directory)
        rendered = render_all(root)
        units = '/etc/systemd/system/'
        names = ['mcp-workspaced.service', 'mcp-image-prepare.service',
                 'mcp-tunnel.service', 'srv-mcp\\x2dpodman.mount']
        for name in names:
            put(root, units + name, rendered[name])
        put(root, units + 'user@2100.service.d/90-mcp.conf', rendered['runner-manager.conf'])
        for service in ('user@2100.service', 'netfilter-persistent.service',
                        'systemd-sysctl.service', 'systemd-journald@mcp.service',
                        'systemd-journald-sync@mcp.service'):
            put(root, units + service,
                '[Unit]\nDefaultDependencies=no\n[Service]\nExecStart=/usr/bin/true\n')
        for target in ('sysinit', 'basic', 'shutdown', 'local-fs', 'local-fs-pre',
                       'network-online', 'multi-user', 'sockets', 'swap'):
            put(root, units + target + '.target', '[Unit]\nDefaultDependencies=no\n')
        for suffix in ('.socket', '-varlink@.socket'):
            name = 'systemd-journald@mcp.socket' if suffix == '.socket' else 'systemd-journald-varlink@mcp.socket'
            put(root, units + name,
                '[Unit]\nDefaultDependencies=no\n[Socket]\n'
                f'ListenStream=/run/{name}\nService=systemd-journald@mcp.service\n')
        for executable in ('/usr/bin/true', '/usr/bin/systemctl', '/bin/mount',
                           '/bin/umount', '/usr/local/libexec/mcp-data-check',
                           '/usr/local/libexec/mcp-store-check', '/usr/local/libexec/mcp-launch',
                           '/usr/local/libexec/mcp-image-prepare', '/usr/local/libexec/tunnel-client'):
            put(root, executable, '#!/bin/sh\nexit 0\n', 0o755)
        subprocess.run(['systemd-analyze', '--root=' + directory, '--man=no',
                        'verify'] + [units + name for name in names] +
                       [units + 'user@2100.service'], check=True)
        print('PASS — offline unit syntax/dependency fixtures only; no services started')


if __name__ == '__main__':
    main()

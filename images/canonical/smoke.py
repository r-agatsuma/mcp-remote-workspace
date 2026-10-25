#!/usr/bin/env python3
"""Image-only checks; accepts docker or a host-required prefix such as sudo podman."""
import argparse
import json
from pathlib import Path
import re
import subprocess

SOURCE = 'https://github.com/r-agatsuma/mcp-remote-workspace'
BASE = 'sha256:18b33ff3829707f31ada654a963b9ee35f2e6f0241b964bd17a22a9b330cb63d'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--revision', required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('runtime_and_image', nargs='+')
    args = parser.parse_args()
    if not re.fullmatch('[0-9a-f]{40}', args.revision) or len(args.runtime_and_image) < 2:
        parser.error('require exact source SHA and runtime command followed by image')
    runtime, image = args.runtime_and_image[:-1], args.runtime_and_image[-1]
    args.evidence.mkdir(parents=True, exist_ok=True)

    def run(*command):
        return subprocess.check_output([*runtime, *command], text=True)

    metadata = json.loads(run('image', 'inspect', image))[0]
    config = metadata['Config']
    assert (metadata['Os'], metadata['Architecture']) == ('linux', 'arm64'), metadata
    assert config['User'] == '0', config
    assert config['WorkingDir'] == '/workspace', config
    assert not config.get('Entrypoint'), config
    assert not config.get('Volumes') and not config.get('ExposedPorts'), config
    assert not config.get('OnBuild'), config
    labels = config['Labels']
    assert labels['org.opencontainers.image.source'] == SOURCE, labels
    assert labels['org.opencontainers.image.revision'] == args.revision, labels
    assert labels['org.opencontainers.image.base.name'] == 'docker.io/library/ubuntu:24.04', labels
    assert labels['org.opencontainers.image.base.digest'] == BASE, labels
    assert all(value.split('=', 1)[0] in ('PATH', 'LANG', 'LC_ALL')
               for value in config.get('Env', [])), config
    (args.evidence / 'image-inspect.json').write_text(json.dumps(metadata, indent=2) + '\n')

    # Override both image ENTRYPOINT and CMD, as the landed canonical runtime does.
    container = run('run', '--detach', '--network=none', '--entrypoint=/usr/bin/sleep',
                    image, 'infinity').strip()
    try:
        assert json.loads(run('container', 'inspect', container))[0]['State']['Running']
        output = run('exec', container, '/bin/sh', '-ec', r'''
test "$(id -u)" = 0
test "$PWD" = /workspace
test -x /bin/sh && test -x /bin/bash && test -x /usr/bin/sleep
/usr/bin/sleep 0
for cmd in cat head git curl jq rg fd python3 cc c++ make file less ssh patch ps pip3 rsync tree unzip wget zip; do
    command -v "$cmd"
done
git lfs version
fd --version
rg --version
test ! -S /var/run/docker.sock && test ! -S /run/podman/podman.sock
python3 -m pip --version
python3 -m venv /tmp/canonical-venv
/tmp/canonical-venv/bin/python -c 'import sys; assert sys.prefix != sys.base_prefix'
test "$(printf 'canonical\nsecond\n' | cat | head -n 1)" = canonical
# Demonstrate the intended writable userspace semantics without package-network access.
touch /usr/local/canonical-smoke /etc/canonical-smoke
for cmd in systemd sshd sudo docker podman codex iro gh node tmux fzf; do
    if command -v "$cmd"; then echo "Unexpected command: $cmd" >&2; exit 1; fi
done
for pkg in systemd systemd-sysv openssh-server sudo docker.io podman; do
    if dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q 'install ok installed'; then
        echo "Unexpected installed package: $pkg" >&2; exit 1
    fi
done
# Bounded credential locations: no imported user/registry auth or SSH private keys.
found=$(find /root /home /etc /usr/local -xdev -type f \( \
    -name .git-credentials -o -name .netrc -o -name .npmrc -o \
    -path '*/.docker/config.json' -o -path '*/containers/auth.json' -o \
    -name 'id_rsa*' -o -name 'id_ed25519*' -o -name 'ssh_host_*_key' \) -print)
test -z "$found"
test -f /usr/share/doc/base-files/copyright
printf 'Canonical image smoke: PASS\n'
''')
        print(output, end='')
        (args.evidence / 'smoke.txt').write_text(output)
        (args.evidence / 'packages.tsv').write_text(run(
            'exec', container, 'dpkg-query', '-W', '-f=${binary:Package}\t${Version}\t${Architecture}\n'))
        (args.evidence / 'copyright-sha256.txt').write_text(run(
            'exec', container, '/bin/sh', '-ec',
            'find /usr/share/doc -type f -name copyright -exec sha256sum {} +'))
    finally:
        # Only the exact smoke container created above is removed.
        run('container', 'rm', '--force', container)


if __name__ == '__main__':
    main()

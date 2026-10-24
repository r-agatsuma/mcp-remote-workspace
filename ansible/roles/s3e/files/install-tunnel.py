#!/usr/bin/python3
"""Install only exact non-secret official assets; never execute a Tunnel client."""
import hashlib
import os
from pathlib import Path
import stat
import tempfile
import zipfile

DIRECTORY = Path('/usr/local/lib/mcp-tunnel/v0.0.16')
PINS = {
    'tunnel-client-v0.0.16-linux-arm64.zip': '963d0384aaa7c798778479c45673f9051a4039dd891aef8f8978d2a1a628b74f',
    'SHA256SUMS.txt': 'fcce864a5935655534cc3d981257509579dbb5c71dbaec64c17a157ca9d65e56',
    'tunnel-client-v0.0.16-provenance.sigstore.json': 'adae10fe6a877d05bee25f4d5fc2c0623c76189ac5af7387efa1c0152e16d3ab',
}
MEMBERS = {'tunnel-client': 0o755, 'LICENSE': 0o644, 'NOTICE': 0o644,
           'tunnel-client-v0.0.16-linux-arm64-licenses.txt': 0o644}


def install(directory):
    for name, expected in PINS.items():
        path = directory / name
        meta = path.lstat()
        if not stat.S_ISREG(meta.st_mode) or meta.st_uid != 0 or meta.st_mode & 0o022:
            raise ValueError('unsafe release asset')
        with path.open('rb') as stream:
            if hashlib.file_digest(stream, 'sha256').hexdigest() != expected:
                raise ValueError('pinned release digest mismatch')
    changed = False
    with zipfile.ZipFile(directory / 'tunnel-client-v0.0.16-linux-arm64.zip') as archive:
        for name, mode in MEMBERS.items():
            data = archive.read(name)
            target = directory / name
            if target.exists() or target.is_symlink():
                meta = target.lstat()
                if not stat.S_ISREG(meta.st_mode) or meta.st_uid != 0 or meta.st_mode & 0o022:
                    raise ValueError('unsafe installed asset')
                if target.read_bytes() == data and stat.S_IMODE(meta.st_mode) == mode:
                    continue
            fd, temporary = tempfile.mkstemp(prefix='.install-', dir=directory)
            try:
                with os.fdopen(fd, 'wb') as stream:
                    stream.write(data)
                    os.fchmod(stream.fileno(), mode)
                    os.fsync(stream.fileno())
                os.replace(temporary, target)
            finally:
                if os.path.exists(temporary):
                    os.unlink(temporary)
            changed = True
    return changed


if __name__ == '__main__':
    print('installed' if install(DIRECTORY) else 'unchanged')

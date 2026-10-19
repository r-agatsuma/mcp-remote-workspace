#!/bin/bash
# Trusted provisioning only; no product/workspace credentials or runtime work.
set -euo pipefail
revision=$1
toolchain_sha=$2
cd "/usr/local/src/mcp-workspaced/$revision"
export PATH=/opt/mcp-toolchain/go/bin:/usr/bin:/bin
export GOTOOLCHAIN=go1.26.9
export LC_ALL=C
go mod download
go mod verify
go build -mod=readonly -trimpath -buildvcs=false -o mcp-workspaced ./cmd/mcp-workspaced
go version -m mcp-workspaced > build-info.txt
go list -mod=readonly -m all > modules.txt
file mcp-workspaced > linkage.txt
readelf -l -d mcp-workspaced >> linkage.txt
# Inspect actual loader resolution, not an assumption about CGO/static linkage.
if ldd mcp-workspaced > ldd.txt 2>&1; then
  :
elif grep -Eq 'not a dynamic executable|statically linked' ldd.txt; then
  :
else
  exit 1
fi
cat ldd.txt >> linkage.txt
if grep -q 'not found' linkage.txt; then exit 1; fi
python3 - "$revision" "$toolchain_sha" <<'PY'
import hashlib
import json
from pathlib import Path
import sys
sha = lambda p: hashlib.sha256(Path(p).read_bytes()).hexdigest()
record = dict(source_revision=sys.argv[1], go_toolchain='go1.26.9',
              go_archive_sha256=sys.argv[2], binary_sha256=sha('mcp-workspaced'),
              source_archive_sha256=sha('source.tar'), go_sum_sha256=sha('go.sum'),
              build_info=Path('build-info.txt').read_text(),
              modules=Path('modules.txt').read_text(),
              linkage=Path('linkage.txt').read_text(), cgo_enabled=True,
              target='Ubuntu 24.04 linux/arm64')
Path('provenance.json').write_text(json.dumps(record, indent=2)+'\n')
PY

# mcp-remote-workspace

A Go MCP service for disposable Linux development workspaces orchestrated by
ChatGPT/OpenAI. The binary exposes **stdio only**, using the official
`modelcontextprotocol/go-sdk`. It opens no network listener.

The v0 tool contract is implemented and validated. `workspace_create` creates an
empty workspace using local rootless Podman; `workspace_destroy` removes it.
`exec`, `write_text`, and `read_text` operate inside that managed container.
Public Git bootstrap and the Git change backend remain
unimplemented and return an MCP tool error with `isError: true` and structured content:

```json
{"error":{"code":"not_implemented","message":"workspace backend is not implemented"}}
```

Schema/type violations use MCP invalid-argument handling. Invalid workspace paths
return `invalid_path`. Public Git URLs with embedded credentials are rejected.

| Tool | Purpose |
| --- | --- |
| `workspace_create` | Create an empty workspace (the default); public Git returns `not_implemented`. |
| `exec` | Run one process inside an existing workspace with explicit argv. |
| `write_text` | Replace a complete UTF-8 text file. |
| `read_text` | Read a complete UTF-8 text file without silent truncation. |
| `workspace_changes` | Return Git change metadata relative to a baseline. |
| `workspace_destroy` | Permanently remove a disposable workspace. |

All input `path` and `cwd` values are POSIX paths relative to `/workspace`, such
as `.` or `internal/foo.go`; absolute paths and all `..` components are rejected.
File and cwd resolution opens each component with `O_NOFOLLOW` through pinned
directory descriptors. All symlinks are rejected, including links within the
workspace, dangling links, and a symlink replacing `/workspace`; special files
are also rejected by text I/O. This checks the effective target rather than only
cleaning the input string, and avoids following a link substituted during lookup.
`exec.cwd` defaults to `.`. Shell use requires explicit argv such as
`["bash", "-lc", "..."]`. Environment values are string overrides, and timeout
seconds must be positive (default **60**, maximum **600**). Overrides apply to the
requested process, never to Podman or the helper. stdin for that process is closed.

Execution uses an embedded Python 3 helper launched via `podman exec`, with JSON
requests on stdin and no implicit shell. It drains stdout and stderr separately,
returns the exit code (including normal nonzero exits), and retains at most
**256 KiB per stream** (`workspace.MaxStreamBytes`). Each stream has an explicit
truncation flag. Invalid output UTF-8 is replaced, and the returned UTF-8 strings
are bounded by the same byte limit. The Linux helper becomes a child subreaper
before launching the command. On timeout it sends SIGKILL to its children,
repeatedly adopts and terminates orphaned descendants (including processes that
use `setsid` or double-fork), and reaps them all before returning `timed_out: true`
with `exit_code: null` and captured output. Cleanup also occurs on normal exit;
commands should not leave background processes running. The deadline remains
active when pipes close early or descendants keep pipes open. The host also
bounds the serialized helper response and diagnostic buffers.

`write_text` encodes the supplied UTF-8 string directly to bytes, fsyncs a new
temporary file in the target directory, and atomically replaces the target entry.
Parent directories **must already exist**; create them explicitly with `exec` if
needed. New/replacement files use mode 0644 subject to the container umask.
`read_text` accepts regular UTF-8 files without NUL bytes (the binary-file check).
Its default maximum is **1 MiB**, configurable at daemon startup with
`-max-text-file-bytes N` (positive bytes), or through `workspace.Options` in Go.
An oversized file returns `size_limit`, and invalid UTF-8 or NUL-containing files
return `non_utf8`; reads never truncate content. Both file tools return the
normalized relative path, byte size, and SHA-256 of the exact file bytes.

The typed requests/responses and reflected schemas in `internal/mcpserver` define
the v0 contract. IDs are opaque, timestamps are RFC3339 UTC, and file hashes are
lowercase SHA-256 of exact bytes. Returned paths are normalized and relative.
`workspace_changes` compares the baseline to the current filesystem, including
local commits, staged/unstaged changes, and untracked files; results will be sorted
by path. Without an explicit `base_ref`, it uses the recorded source baseline or
returns `base_ref_required` once that backend is implemented.

The intended production host is Debian 13 (Trixie) Minimal. Tunnel setup and
Ansible deployment belong to separate issues.

Workspace operations require a prepared dedicated non-root service account with
local Podman 5.x, crun, conmon, slirp4netns, netavark, the distribution seccomp
profile at `/usr/share/containers/seccomp.json`, and rootless storage. The host
must provide a systemd user bus at `/run/user/UID/bus`, cgroup v2 with delegated
CPU/memory/pids controllers, and subordinate UID/GID ranges large enough for
65536 IDs per concurrent workspace. The service checks rootless mode and the
required controllers on startup and fails if they are unavailable. It does not
install packages, configure the host, enable user lingering, or pull images.

Build the project image explicitly as the service user before starting the daemon:

```sh
podman --remote=false build -t localhost/mcp-remote-workspace:v0 \
  -f container/Containerfile container
```

The image contains ca-certificates, curl, git, jq, ripgrep, patch, build-essential,
and python3. It supplies a writable `/workspace` and contains no service
credentials. The fixed local image tag is resolved once at startup; creation uses
the resulting immutable image ID with `--pull=never`. Keep that image available
and do not replace its tag while workspaces exist: recovery rejects containers
whose image no longer matches the fixed local image.

The v0 runtime permits **2 CPUs** (200000 µs quota per 100000 µs period), **2 GiB
memory with no additional swap**, and **256 processes**. Callers cannot change
these values. Containers run as UID/GID 0 inside their own automatically allocated
user namespace, drop all capabilities, enable no-new-privileges and seccomp, and
use private PID, IPC, UTS, and cgroup namespaces. The network is slirp4netns with
outbound Internet and host loopback forwarding disabled. There are no user bind
mounts, host devices, engine sockets, secrets, or published ports. `/workspace`
lives in the container's writable layer; destroying a workspace loses its data.

`internal/workspace/containers.conf` is embedded in the binary and materialized
as private application configuration under `/run/user/UID/mcp-workspaced-v0`.
Existing files must match the embedded configuration and the hook directory must
be empty; conflicts fail startup without overwriting files. This configuration
persists across daemon exits because conmon may need it for later container exit
cleanup. Podman receives `CONTAINERS_CONF` pointing only to this file, an explicit
empty hooks directory, `--default-mounts-file=/dev/null`, and `--remote=false`.
System/user containers.conf files, config overrides, and remote connections do
not participate in workspace creation. This relies on Podman's documented
[CONTAINERS_CONF semantics](https://github.com/containers/common/blob/v0.62.3/docs/containers.conf.5.md#environment-variables).

The Podman process environment is constructed from fixed PATH/LANG, the service
account's home from the account database, the UID-derived runtime directory,
the project config directory, and a UID-derived D-Bus address required by systemd
cgroup management. Host `NOTIFY_SOCKET`, `LISTEN_*`, engine connection/socket,
proxy, credential, loader, and session environment variables are not inherited.
The container receives only fixed PATH, `HOME=/root`, `LANG=C.UTF-8`, and
`HOSTNAME=mcp-workspace`, with the hostname also fixed to `mcp-workspace`;
`--unsetenv-all`, disabled proxy forwarding, and `--sdnotify=ignore` enforce this.
Post-create inspection verifies the saved auto user namespace option and allocated
UID/GID mappings; Podman adds the effective private user namespace at start.
Post-start and recovery inspection assert the isolation and resource profile,
including the pre-runtime state of interrupted creations, before returning success.
The Debian host, prepared configuration, daemon,
local Podman, and the dedicated user's project-owned engine/storage configuration
are trusted; container code and downloaded content are untrusted.

Workspace IDs contain 32 cryptographically random bytes and are opaque handles,
independent of container IDs. Ownership, workspace ID, UTC creation time, and
profile version are persisted under `io.github.r-agatsuma.mcp-remote-workspace.*`
Podman labels. Startup discovers managed containers, including stopped containers
from interrupted transactions, validates their labels/profile, rejects duplicate
IDs, and restores the registry without restarting compute or changing the IDs.
Malformed labels or profile drift fail startup without modifying containers and
require operator investigation using those ownership labels.

Creation rolls back partial failures using a separate bounded cleanup context.
If the create command fails before returning an ID, cleanup discovers the exact
container by ownership/workspace labels and verifies its creation timestamp. If
cleanup fails, the registry and labels remain and the error supplies the opaque
workspace ID for an explicit `workspace_destroy` retry. Destroy uses force removal
to stop compute and remove its writable layer/runtime state. Failed removal keeps
the mapping; successful repeated destroy is idempotent within that daemon's
lifetime. Unknown IDs, including destroyed IDs after a restart, return
`workspace_not_found`.

This service is not a generic remote shell. Host process execution, GitHub
authentication/commit/PR operations, private repository cloning, arbitrary runtime
images/options/mounts/devices/capabilities/network modes, HTTP serving, and binary
file transfer are outside the v0 contract. GitHub operations remain outside this
service.

Build and run with Go 1.24 or newer (SDK v1.4.0 is pinned):

```sh
go build -o mcp-workspaced ./cmd/mcp-workspaced
./mcp-workspaced
```

After startup validation and recovery, the process accepts newline-delimited MCP
JSON on stdin, reserves stdout for MCP, and exits cleanly on stdin EOF, SIGINT, or
SIGTERM. Daemon exit preserves workspaces for later recovery.

```sh
go test ./...
go vet ./...
gofmt -l cmd internal
```

Normal tests mock Podman and require no container host. Operation tests use
Python 3 to run the embedded helper against temporary test directories, covering
argv/environment behavior, UTF-8 round trips, repeated timeout/descendant cleanup
(including separate sessions, double-forks and zombie rejection),
output truncation, size limits, traversal, symlinks, and special-file rejection.
On a prepared dedicated
rootless account with the fixed image already built, opt into the integration
test (it creates and removes one test workspace, checks actual resource limits,
writes/reads/executes a program, checks timeout/output/file limits and path
security, probes isolation and outbound HTTPS, and tests rediscovery):

```sh
MCP_WORKSPACE_INTEGRATION=1 go test ./internal/workspace \
  -run '^TestRootlessPodmanIntegration$' -count=1
```

# mcp-remote-workspace

A Go MCP service for disposable Linux development workspaces orchestrated by
ChatGPT/OpenAI. The binary exposes **stdio only**, using the official
`modelcontextprotocol/go-sdk`. It opens no network listener.

The v0 tool contract is implemented and validated. `workspace_create` creates an
empty workspace using local rootless Podman; `workspace_destroy` removes it.
`exec`, `write_text`, and `read_text` operate inside the managed container.
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
as `.` or `internal/foo.go`; absolute paths and root escapes are rejected.
`exec.cwd` defaults to `.`. Shell use requires explicit argv such as
`["bash", "-lc", "..."]`. Environment values are string overrides, and timeout
seconds must be positive. The default execution timeout is 60 seconds and the
maximum is 300 seconds; callers cannot disable these limits.

The typed requests/responses and reflected schemas in `internal/mcpserver` define
the v0 contract. IDs are opaque, timestamps are RFC3339 UTC, and file hashes are
lowercase SHA-256 of exact bytes. Returned paths will be normalized and relative.
Execution reports separate stdout/stderr, each limited to **256 KiB** of returned
UTF-8 text, with explicit truncation flags. Invalid output bytes are replaced
with U+FFFD while preserving the returned byte limit. A normal nonzero exit is
a successful tool result. Timeout returns `timed_out: true` and a null exit code
after successful process cleanup.

`read_text` and `write_text` default to a **1 MiB** file/content limit. The operator
can set `MCP_MAX_TEXT_BYTES` (1–67108864 bytes); tool callers cannot change it.
Reads never truncate: oversized files return `size_limit`. Non-UTF-8 files,
files containing NUL bytes, and nonregular files return `non_utf8`.
Paths reject every `..` component, absolute paths, and **all symlinks**, even
symlinks targeting another location inside `/workspace`. Directory descriptor
walking and no-follow opens check effective targets. File operations are
serialized with execution, so no process from a previous managed execution can
replace a checked path during I/O. Mutation by trusted operators outside this
operation path is outside the v0 threat model.

Writes send UTF-8 bytes through JSON stdin to the static helper, with no shell
interpolation. Missing parents are created with mode 0755 (subject to umask).
A new mode-0644 file is written and synced in the target directory, then renamed
over the target atomically; the directory is synced too. Replacement does not
preserve the old inode or its permissions. A cancelled/failed write has an
uncertain outcome (old or complete new target); an interrupted helper can leave
a temporary `.mcp-write-*` file. Hashes and sizes describe exact UTF-8 bytes.
`workspace_changes` compares the baseline to the current filesystem, including
local commits, staged/unstaged changes, and untracked files; results will be sorted
by path. Without an explicit `base_ref`, it uses the recorded source baseline or
returns `base_ref_required`.

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

Build and install the immutable control runtime as an operator before starting
the daemon (Go 1.24 or newer). Installation is an explicit host preparation step;
the daemon does not build or repair this artifact:

```sh
CGO_ENABLED=0 go build -o mcp-control ./cmd/mcp-control
sudo install -d -m 0755 /usr/local/lib/mcp-workspaced
sudo install -m 0755 mcp-control /usr/local/lib/mcp-workspaced/mcp-control
```

The host path is fixed and service-owned. The daemon verifies the Go program
identity, `CGO_ENABLED=0`, absence of an ELF interpreter/shared-library imports,
and executable file/parent permissions. Paths cannot be symlinks or writable by
other users, and must belong to root or the service user. Keep the artifact
available and unchanged while workspaces exist.
The daemon alone mounts it read-only at `/run/mcp-control`; callers cannot choose
or replace it. No mutable Python runtime, dynamic linker, startup hooks or shell
is used for trusted file operations or PID 1. The normal rootfs remains writable:
development code can install packages or break `/usr`, `/etc`, Python, or sleep
without compromising trusted file I/O or stop/start recovery.

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
is a separate service-owned writable local Podman volume named from the opaque
workspace ID. Its ownership labels and local driver with no driver options are
verified. Destroying a workspace removes both its container and that volume.

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
IDs, and restores the registry without changing IDs. Only after validating the
complete container/volume registry does it reset recovered containers; leftovers
from a previous daemon are never assumed safe. Orphan volumes from interrupted
creation are recovered as blocked handles that can be explicitly destroyed.
Malformed labels or profile drift fail startup without modifying containers and
require operator investigation using those ownership labels.
Workspaces from the earlier sleep/writable-layer profile are incompatible with
this profile. Destroy them with the earlier daemon before upgrading; the new
daemon does not migrate or silently recreate them.

`exec`, `read_text`, `write_text`, and destruction share an operation lock per
workspace. The lock remains held through all failure cleanup. Execution is an
ephemeral job API: persistent/background services are unsupported. A successful
helper response is not proof of cleanup because untrusted code has the same
container identity and can kill or interfere with the helper. **Every operation**,
including successful file I/O, therefore stops the container with timeout zero,
verifies it is stopped, starts the same container with `/run/mcp-control init`
as PID 1, and verifies the running profile before releasing the lock. Killing
the private PID namespace covers new sessions/groups, double forks, daemonized
children and zombies. The immutable PID 1 also reaps adopted children.

Timeout, cancellation, transport/helper failure and malformed responses all take
this same recovery path using an independent 45-second cleanup context.
The existing container ID, mutable writable layer and `/workspace` volume survive
successful reset. Containers are never automatically recreated. If reset cannot
be verified, the tool returns `backend_error` and the workspace is blocked until
explicit `workspace_destroy`. Private host-side block markers under the service
account's `~/.local/share/mcp-workspaced-v0` preserve blocks across daemon restarts;
that account's `~/.local/share` parent must already exist. Blocked workspaces are
not automatically retried or reset on startup; destroy remains available.

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

Normal tests mock Podman and require no container host. On a prepared dedicated
rootless account with the fixed image and static control runtime installed, opt into the integration
test (it creates and removes one test workspace, checks actual resource limits,
writes/reads/executes a trivial program, checks timeouts/output limits and path
escapes, exercises daemonized descendants, helper death and cancelled file
transport recovery, tampers with Python/startup hooks/sleep, probes isolation
and outbound HTTPS, and tests rediscovery):

```sh
MCP_WORKSPACE_INTEGRATION=1 go test ./internal/workspace \
  -run '^TestRootlessPodmanIntegration$' -count=1
```

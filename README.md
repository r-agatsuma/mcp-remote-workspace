# mcp-remote-workspace

A Go MCP service for disposable Linux development workspaces orchestrated by
ChatGPT/OpenAI. The binary exposes **stdio only**, using the official
`modelcontextprotocol/go-sdk`. It opens no network listener.

`workspace_create` starts an empty workspace in one local rootless Podman
container, and `workspace_destroy` removes it. Public Git bootstrap and the
remaining tool backends are not implemented; those requests return an MCP tool
error with `isError: true` and structured content:

```json
{"error":{"code":"not_implemented","message":"workspace backend is not implemented"}}
```

Schema/type violations use MCP invalid-argument handling. Invalid workspace paths
return `invalid_path`. Public Git URLs with embedded credentials are rejected.

| Tool | Purpose |
| --- | --- |
| `workspace_create` | Create an empty workspace (the default); public Git bootstrap is not implemented. |
| `exec` | Run one process inside an existing workspace with explicit argv. |
| `write_text` | Replace a complete UTF-8 text file. |
| `read_text` | Read a complete UTF-8 text file without silent truncation. |
| `workspace_changes` | Return Git change metadata relative to a baseline. |
| `workspace_destroy` | Permanently remove a disposable workspace. |

All input `path` and `cwd` values are POSIX paths relative to `/workspace`, such
as `.` or `internal/foo.go`; absolute paths and root escapes are rejected.
`exec.cwd` defaults to `.`. Shell use requires explicit argv such as
`["bash", "-lc", "..."]`. Environment values are string overrides, and timeout
seconds must be positive; future backend limits cannot be disabled by callers.

The typed requests/responses and reflected schemas in `internal/mcpserver` define
the v0 contract. IDs are opaque, timestamps are RFC3339 UTC, and file hashes are
lowercase SHA-256 of exact bytes. Returned paths will be normalized and relative.
Execution will report truncation explicitly, and a normal nonzero exit will be
a successful tool result. Reads will reject oversized and non-UTF-8 files.
`workspace_changes` compares the baseline to the current filesystem, including
local commits, staged/unstaged changes, and untracked files; results will be sorted
by path. Without an explicit `base_ref`, it uses the recorded source baseline or
returns `base_ref_required`. Future filesystem backends must also reject effective
symlink escapes and document a deterministic parent-directory creation policy.

The intended production host is Debian 13 (Trixie) Minimal. Workspace lifecycle
uses rootless Podman; OpenAI Secure MCP Tunnel setup and Ansible deployment
belong to separate issues.

The service requires local Podman 5.4 or newer, slirp4netns, subordinate UID/GID
ranges for the service user, and cgroups v2 with CPU, memory and pids controllers
delegated to that user. It checks Podman's rootless status and discovers managed
containers before accepting MCP requests; initialization failure exits with a
diagnostic on stderr. It never invokes sudo or connects to a remote Podman
service. Build the project image as the same unprivileged user before starting:

```sh
podman --remote=false build --http-proxy=false \
  -t localhost/mcp-remote-workspace:dev -f container/Containerfile container
```

The fixed development image includes ca-certificates, curl, git, jq, ripgrep,
patch, build-essential and python3, and has a writable `/workspace` directory.
Creation uses only this local image (`--pull=never`), with a fixed profile of
2 CPUs, 1 GiB memory and 256 pids. It uses a private user namespace with UID 0
inside, private PID/IPC namespaces, and slirp4netns for outbound networking.
It drops all capabilities and enables no-new-privileges. No host directories,
devices, engine sockets or credentials are passed into the container; automatic
mounts from mounts.conf and host proxy environment forwarding are disabled.
Each Podman command uses a service-owned, last-loaded containers.conf override
that clears default devices, including configurations with array appending
enabled. Other host engine settings are preserved. This prevents rootless
device bind mounts that ordinary container inspect does not expose.
See the [containers.conf documentation](https://github.com/containers/common/blob/v0.62.2/docs/containers.conf.5.md)
for configuration precedence and array replacement.
The effective container configuration is inspected before starting and on
recovery; incompatible host defaults cause an error instead of weakening the
profile. See the [Podman create documentation](https://docs.podman.io/en/v5.4.2/markdown/podman-create.1.html)
for the runtime options.

Creation returns a cryptographically random `ws_` handle, UTC `created_at`, and
`source.type: "empty"`. The handle and timestamp are persisted in project-specific
Podman labels (`io.github.r-agatsuma.mcp-remote-workspace.*`), so restarting the
MCP service preserves existing workspaces. Shutdown leaves containers available
for recovery. The service owns no workspace bind mounts or separate workspace
files; removal deletes the container's writable layer and runtime metadata.
Successful duplicate destroys are idempotent during one server lifetime.
After restart, IDs of already removed workspaces, and other unknown IDs, return
`workspace_not_found`. Podman operation failures return `backend_error`; a failed
destroy keeps its mapping so a later request can try again. A failed inspection
or start removes only the container created by that request. An interrupted
create or invalid container ID output also triggers cleanup, discovering the
container by the request's exact labels with a context independent of request
cancellation. Rollback failure reports the workspace handle and retains its
mapping so a later destroy can finish discovery and removal.

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

The process accepts newline-delimited MCP JSON on stdin, reserves stdout for MCP,
and exits cleanly on stdin EOF, SIGINT, or SIGTERM.

```sh
go test ./...
go vet ./...
gofmt -l cmd internal
```

Normal tests mock Podman commands and cover recovery, unknown IDs, duplicate
destroy, isolation policy and command failures. On a prepared rootless Podman
host with the image already built, run the opt-in integration test to verify
actual creation, recovery, outbound HTTPS, writable storage, installed tools,
host home/socket isolation and removal:

```sh
MCP_WORKSPACE_PODMAN_TEST=1 go test ./internal/workspace -run TestRootlessPodmanIntegration -v
```

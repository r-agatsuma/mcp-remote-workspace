# mcp-remote-workspace

A Go MCP service for disposable Linux development workspaces orchestrated by
ChatGPT/OpenAI. The binary exposes **stdio only**, using the official
`modelcontextprotocol/go-sdk`. It opens no network listener.

The v0 tool contract is implemented and validated, but **all workspace backends
are currently non-functional**. Valid requests return an MCP tool error with
`isError: true` and structured content:

```json
{"error":{"code":"not_implemented","message":"workspace backend is not implemented"}}
```

Schema/type violations use MCP invalid-argument handling. Invalid workspace paths
return `invalid_path`. Public Git URLs with embedded credentials are rejected.

| Tool | Purpose |
| --- | --- |
| `workspace_create` | Create an empty workspace (the default) or bootstrap public Git. |
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

The intended production host is Debian 13 (Trixie) Minimal, with rootless Podman
for workspaces and OpenAI Secure MCP Tunnel for connectivity. Those backends,
tunnel setup, and Ansible deployment belong to separate issues.

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

# Dependency inventory

`go.mod` pins selected versions and `go.sum` records downloaded module checksums.
There are no replacements. The SDK remains the MCP implementation; P5 adds focused
Dockerfile syntax/image-reference parsing and imports Go's Linux syscall bindings
directly. No build engine, daemon or network authority is embedded.

## MCP dependency decision

- **Required capability:** official MCP client/server types, JSON-RPC transport,
  protocol negotiation, tool schemas/discovery/dispatch and Streamable HTTP.
  The standard library provides HTTP/JSON primitives, but no MCP implementation.
- **Maintainer/provenance:** Model Context Protocol Go SDK maintainers, maintained
  in collaboration with Google; [official upstream](https://github.com/modelcontextprotocol/go-sdk),
  [pinned release](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0).
  The Go module download identifies tag `v1.8.0` at upstream revision
  `3f3b699b2b67e1ed033a63d6651671dab53c2d32`.
- **License:** upstream reports Apache-2.0 for new contributions and MIT for
  existing code; see the [pinned LICENSE](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/LICENSE).
- **Adoption evidence:** the command constructs a real SDK server/client and
  exchanges MCP messages over the SDK's in-memory JSON transport. Local tests
  additionally connect over Streamable HTTP, negotiate MCP `2025-11-25` and
  `2026-07-28`, discover an inferred input schema and invoke a test-only echo tool.
  This exercises the baseline server/protocol surface instead of just importing
  the SDK. The echo tool exists only in test source.
- **Protocol boundary:** Ping was removed in MCP `2026-07-28`, as documented in
  the [pinned SDK protocol guide](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/docs/protocol.md#ping).
  The initial HTTP probe confirmed that modern HTTP Ping is rejected, including
  with explicit request metadata. The smoke operation explicitly uses
  `2025-11-25` for its Ping check. HTTP tests exercise Ping on `2025-11-25` and
  discovery/tool calls on both revisions. This is a local probe configuration;
  it does not select a production protocol revision or add a compatibility layer.
- **Decision boundary:** this evidence supports adoption for this baseline. It
  does not establish every future MCP feature, production transport/auth design,
  remote runtime behavior or full protocol conformance. A later concrete gap
  requiring another dependency/product decision must be reported before adding it.

## P5 parsing and Linux dependencies

| Direct module/version | Capability and rationale | Provenance/license |
| --- | --- | --- |
| `github.com/moby/buildkit v0.34.0` | Only `frontend/dockerfile/parser` and `shell` are imported. Maintained parsing handles stages, continuation, heredocs and quoted label keys without a custom Dockerfile frontend. Service code retains source/isolation policy. | [Moby BuildKit](https://github.com/moby/buildkit/tree/v0.34.0/frontend/dockerfile), Apache-2.0 |
| `github.com/distribution/reference v0.6.0` | Standard image-reference syntax, including short names and digests; avoids a custom registry grammar. No image fetching or engine integration. | [Distribution](https://github.com/distribution/reference/tree/v0.6.0), Apache-2.0 |
| `golang.org/x/sys v0.48.0` | Direct Linux openat2, fd-relative removal and fchmodat2 bindings for narrow exact Data cleanup; replaces hand-written syscall ABI code. Previously indirect. | [Go project](https://go.googlesource.com/sys/+/refs/tags/v0.48.0), BSD-3-Clause |

The parser raises the Go module minimum to 1.26.8; the preferred toolchain remains
1.26.9. The imported BuildKit packages provide syntax and diagnostic types only;
this service still invokes host Podman/Buildah. The module graph includes BuildKit's
other engine/cloud dependencies, but those packages are not command dependencies.
The following indirect modules occur in the command's package graph after tidy:

| Module/version | Purpose |
| --- | --- |
| `github.com/containerd/typeurl/v2 v2.3.0` | BuildKit parser diagnostic type registration |
| `github.com/google/jsonschema-go v0.4.3` | MCP schema validation |
| `github.com/opencontainers/go-digest v1.0.0` | Image-reference digest syntax |
| `github.com/pkg/errors v0.9.1` | Parser error context |
| `github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10` | Generated BuildKit diagnostic serialization |
| `github.com/segmentio/asm v1.1.3` | SDK encoding support |
| `github.com/segmentio/encoding v0.5.4` | SDK JSON encoding |
| `github.com/yosida95/uritemplate/v3 v3.0.2` | MCP URI templates |
| `golang.org/x/oauth2 v0.35.0` | SDK transport auth |
| `golang.org/x/sync v0.23.0` | SDK request concurrency; version selected by BuildKit |
| `golang.org/x/time v0.16.0` | SDK rate limiting; version selected by BuildKit |
| `google.golang.org/protobuf v1.36.12` | BuildKit diagnostic types |

To audit the complete selected module graph separately from command package
imports, and verify cached module checksums:

```sh
go list -m all
go list -deps -f '{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}' ./cmd/mcp-workspaced
go mod verify
```

# Dependency inventory

The direct non-standard-library modules are
**`github.com/modelcontextprotocol/go-sdk v1.8.0`** and **`golang.org/x/sys v0.41.0`**.
`go.mod` pins selected versions
and `go.sum` records downloaded module checksums. There are no replacements or
additional convenience/framework dependencies.

## Direct dependency decision

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

## Transitive modules selected by this baseline

These eight modules were brought in by the approved SDK. The existing pinned
`golang.org/x/sys` is now also imported directly for Linux exact-generation Data
cleanup: `openat2` constrains traversal and `fchmodat2(AT_EMPTY_PATH)` changes only
verified directory descriptors. The standard library does not expose both of
these controls. No module/version or transitive dependency was added by this use;
the other seven remain `// indirect` requirements. Inclusion in a package dependency
graph does not establish which code survives binary linking.

| Exact module/version | SDK dependency purpose | Maintainer/provenance |
| --- | --- | --- |
| `github.com/google/jsonschema-go v0.4.3` | Tool schema inference and validation | Google, [upstream](https://github.com/google/jsonschema-go) |
| `github.com/segmentio/encoding v0.5.4` | SDK internal JSON encoding/decoding | Segmentio, [upstream](https://github.com/segmentio/encoding) |
| `github.com/segmentio/asm v1.1.3` | Low-level support for Segmentio encoding | Segmentio, [upstream](https://github.com/segmentio/asm) |
| `github.com/yosida95/uritemplate/v3 v3.0.2` | MCP resource URI templates | yosida95, [upstream](https://github.com/yosida95/uritemplate) |
| `golang.org/x/oauth2 v0.35.0` | SDK auth package used by MCP transports | Go project, [upstream](https://go.googlesource.com/oauth2) |
| `golang.org/x/sync v0.20.0` | SDK multi-round-trip request concurrency | Go project, [upstream](https://go.googlesource.com/sync) |
| `golang.org/x/sys v0.41.0` | Platform primitives used by Segmentio assembly support; direct Linux cleanup syscalls | Go project, [upstream](https://go.googlesource.com/sys) |
| `golang.org/x/time v0.15.0` | SDK logging rate limiting | Go project, [upstream](https://go.googlesource.com/time) |

The complete `go list -m all` graph also includes the following modules, absent
from this command's package dependency graph. They are recorded to distinguish
SDK/test/subpackage module requirements from command dependencies:

| Exact module/version | Reason in module graph | Maintainer/provenance |
| --- | --- | --- |
| `github.com/google/go-cmp v0.7.0` | Upstream test comparisons | Google, [upstream](https://github.com/google/go-cmp) |
| `github.com/golang-jwt/jwt/v5 v5.3.1` | SDK OAuth extension tests | golang-jwt maintainers, [upstream](https://github.com/golang-jwt/jwt) |
| `golang.org/x/tools v0.42.0` | SDK conformance-test archive utilities | Go project, [upstream](https://go.googlesource.com/tools) |
| `cloud.google.com/go/compute/metadata v0.3.0` | OAuth2 module's Google-specific subpackage dependency | Google Cloud Go maintainers, [upstream](https://github.com/googleapis/google-cloud-go/tree/main/compute/metadata) |

To audit the selected graph and command package dependencies locally:

```sh
go list -m all
go list -deps -f '{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}' ./cmd/mcp-workspaced
go mod verify
```

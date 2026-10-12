# mcp-remote-workspace

Go source baseline for `mcp-workspaced`. The module path is
`github.com/r-agatsuma/mcp-remote-workspace` and the command lives at
`cmd/mcp-workspaced`.

This slice contains a build skeleton and a bounded local MCP smoke check.
Running without arguments prints usage and exits; `-smoke` connects an SDK client
and server in memory using MCP `2025-11-25`, checks server identity/capabilities,
pings, verifies an empty
tool list, closes the sessions, and exits. The whole operation has a five-second
deadline and returns a nonzero status on failure. It opens no network listener.
There are no workspace lifecycle operations or product tools.

## Go baseline

`go.mod` records Go language/module minimum **1.25.0** (required by the pinned SDK)
and preferred toolchain **go1.26.9**. Use `GOTOOLCHAIN=go1.26.9` for exact-toolchain
validation; the `toolchain` directive alone permits an already newer Go toolchain.
The Go command can download this toolchain when needed. Initial dependency and
toolchain downloads require network access; later builds can use the local cache.

## Local build and validation contract

Run from the repository root with the pinned toolchain:

```sh
export GOTOOLCHAIN=go1.26.9
gofmt -w cmd/mcp-workspaced/*.go
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/mcp-workspaced
./mcp-workspaced -smoke
git diff --check
```

The build writes `./mcp-workspaced`, which is ignored by Git. Race tests require
a local Go-supported race platform and C compiler. Tests exercise the empty
skeleton over the SDK's JSON transport and use a loopback HTTP server plus a
test-only echo tool to check Streamable HTTP, protocol negotiation, schema
inference, tool discovery and dispatch for MCP `2025-11-25` and `2026-07-28`.
The HTTP test configuration is an SDK suitability probe, not a production
transport or authentication contract.

Inspect the locally built artifact with `go version -m ./mcp-workspaced` if needed.
These checks establish local source/build behavior only. Target-host execution,
linkage, CGO/shared-library behavior and portability require separate verification
of the delivered revision in Issue #68.

See [the dependency inventory](docs/dependencies.md) for versions, provenance and
the scope of the SDK adoption evidence.

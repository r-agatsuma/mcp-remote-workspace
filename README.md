# mcp-remote-workspace

Go MCP baseline and host-side lifecycle core for `mcp-workspaced`. The module path is
`github.com/r-agatsuma/mcp-remote-workspace` and the command lives at
`cmd/mcp-workspaced`.

The command includes a daemon skeleton and a bounded local MCP smoke check.
Running without arguments prints usage and exits; `-smoke` connects an SDK client
and server in memory using MCP `2025-11-25`, checks server identity/capabilities,
pings, verifies an empty
tool list, closes the sessions, and exits. The whole operation has a five-second
deadline and returns a nonzero status on failure. It opens no network listener.
The lifecycle core implements trusted conversation identity, durable exact-generation
create/destroy, singleton ownership and startup reconciliation. `-daemon` owns and
reconciles local state until shutdown; it opens no network listener and exposes no
product tools. See [the lifecycle contract](docs/lifecycle.md) for configuration,
trust boundaries and failure behavior. Reusable generation, process, backend,
TTL and shared-pool storage controls are described in [the admission contract](docs/admission.md).
Embedded servers can register `exec`, `read_text` and `write_text` with a trusted
admission callback; see [the tool contract](docs/tools.md) for request/result limits,
uncertain completion and runtime capability requirements. The landed Podman adapter
implements a trusted Linux process/cgroup probe and rejects unverifiable state.
They are tested with local fakes; real-host verification remains separate.

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
gofmt -w cmd/mcp-workspaced/*.go internal/workspace/*.go
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
Lifecycle tests use fake runtimes, stub CLI responses and a local stub executable;
they do not invoke Podman. They cover identity, fresh generations, idempotency,
partial failures, stale callbacks, durable recovery, process-lock conflicts and
reconciliation admission gates.
These checks establish local source/build behavior only. Target-host execution,
linkage, CGO/shared-library behavior and portability require separate verification
of the delivered revision in external verification Issues (#68 for the baseline,
#69 for this lifecycle slice).

See [the dependency inventory](docs/dependencies.md) for versions, provenance and
the scope of the SDK adoption evidence.

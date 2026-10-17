# mcp-remote-workspace

Go MCP baseline and host-side lifecycle core for `mcp-workspaced`. The module path is
`github.com/r-agatsuma/mcp-remote-workspace` and the command lives at
`cmd/mcp-workspaced`.

The command includes a loopback MCP daemon and a bounded local MCP smoke check.
Running without arguments prints usage and exits; `-smoke` connects an SDK client
and server in memory using MCP `2025-11-25`, checks server identity/capabilities,
pings, verifies an empty
tool list, closes the sessions, and exits. The whole operation has a five-second
deadline and returns a nonzero status on failure. It opens no network listener.
The lifecycle core implements trusted conversation identity, durable exact-generation
create/destroy, singleton ownership and startup reconciliation. `-daemon` owns and
reconciles local state before serving the five product tools at the explicit
`-mcp-listen 127.0.0.1:8080` endpoint, path `/mcp`. See
[production ingress](docs/ingress.md) for trusted-host prerequisites and bounds, and
[the lifecycle contract](docs/lifecycle.md) for configuration,
trust boundaries and failure behavior. Reusable generation, process, backend,
TTL and shared-pool storage controls are described in [the admission contract](docs/admission.md).
Embedded servers can register `workspace_create`, `workspace_destroy`, `exec`,
`read_text` and `write_text` with a trusted
admission callback; see [the tool contract](docs/tools.md) for request/result limits,
uncertain completion and runtime capability requirements. The landed Podman adapter
implements a trusted Linux process/cgroup probe and rejects unverifiable state.
They are tested with local fakes; real-host verification remains separate.
Optional Dockerfile text creates one asynchronous exact-generation candidate.
Builds use the existing lifecycle, journal and admission controls, with rootless
Podman/Buildah, pasta, a bounded systemd user slice and retained Data diagnostics.
See [Dockerfile creation](docs/lifecycle.md#dockerfile-creation-p5) for behavior,
host prerequisites and the separate canonical-host verification boundary.
Trusted operators can use exclusive `-recover inspect|backend|storage|cleanup`
mode with the normal singleton and durable state. Backend recovery requires
product-gathered evidence that the recorded kernel BootID differs from the
current boot; residue still requires exact cleanup. See [trusted recovery](docs/recovery.md)
for the terminal-evidence map, storage latch, pressure fallback and bounded
stdout/stderr failure observations.
Production startup requires one to three explicit `-public-dns <literal-IP>`
options; no resolver addresses are compiled defaults. Build RUN and both final
image paths receive this deployment-owned list. Final workspaces select independent
rootless pasta namespaces with forwarding and gateway mapping disabled, plus the
same service-controlled image/runtime controls. See the
[trusted runtime profile](docs/lifecycle.md#trusted-runtime-and-dns-profile).
Host-fetch resolver configuration and root-managed OUTPUT policy remain deployment
responsibilities; actual network/DNS effectiveness is deferred to #95.

## Deployment source

[Canonical v0 deployment and operations](docs/deployment.md) describes the
repository-owned Ansible/systemd tree, exact-source provenance, separated
credentials, Data bootstrap safety, trusted DNS/firewall and exclusive recovery.
See `ansible/inventory/example` for required inputs. These are source/static
checks only: OCI convergence and real runtime/Tunnel behavior remain #95/#96.

## Go baseline

`go.mod` records Go language/module minimum **1.26.8** (required by the pinned Dockerfile parser)
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
go build ./...
go build ./cmd/mcp-workspaced
./mcp-workspaced -smoke
git diff --check
```

The build writes `./mcp-workspaced`, which is ignored by Git. Race tests require
a local Go-supported race platform and C compiler. Tests exercise the empty
skeleton over the SDK's JSON transport and use a loopback HTTP server plus a
test-only echo tool to check Streamable HTTP, protocol negotiation, schema
inference, tool discovery and dispatch for MCP `2025-11-25` and `2026-07-28`.
Additional component tests construct the production ingress over local HTTP with
an in-memory Runtime, covering identity admission, exact-generation dispatch,
startup ordering, cancellation, shutdown and resource bounds. These tests do not
prove real OpenAI metadata delivery or Tunnel compatibility.

Inspect the locally built artifact with `go version -m ./mcp-workspaced` if needed.
Lifecycle tests use fake runtimes, stub CLI responses and a local stub executable;
they do not invoke Podman. They cover identity, fresh generations, idempotency,
partial failures, stale callbacks, durable recovery, process-lock conflicts and
reconciliation admission gates.
These checks establish local source/build behavior only. Target-host execution,
linkage, CGO/shared-library behavior and portability require separate verification
of the delivered revision under #90: #95 dirty-OCI integration and #96 clean
ChatGPT/Tunnel E2E. Independent source review is also required before landing.

See [the dependency inventory](docs/dependencies.md) for versions, provenance and
the scope of the SDK adoption evidence.

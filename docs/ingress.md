# Production loopback MCP ingress

`mcp-workspaced -daemon` requires an explicit `-mcp-listen` literal loopback
IP:port, for example `127.0.0.1:8080` (IPv6 `[::1]:8080` is also accepted).
The only HTTP endpoint is `/mcp`. Empty configuration, hostnames, wildcard or
non-loopback addresses, scoped addresses, malformed ports and port zero fail
before Service initialization. There is no fallback or separate probe listener.
The existing image, journal, Podman, resource and TTL flags still apply.
One to three `-public-dns <literal-IP>` options are also required; values are
validated and copied at service startup. This trusted deployment configuration
reaches build RUN and final workspace defaults, never request identity or tool
arguments. See the [runtime profile](lifecycle.md#trusted-runtime-and-dns-profile)
for host-fetch DNS separation and the required external enforcement boundary.

The daemon acquires the existing singleton through `workspace.Open`, initializes
the runtime and reconciles the journal before binding the socket. The same
Service handles TTL and the existing `RegisterTools` registration. Initialize
and tools/list require no workspace identity. Exactly five public tools exist:
`workspace_create`, `workspace_destroy`, `exec`, `read_text`, `write_text`.
The SDK remains pinned at v1.8.0; no module was added or upgraded.

## Trust and identity

This is the #20 dedicated trusted-host / Tunnel-to-loopback boundary. The host
OS, host control plane and local clients are trusted. Deployment must keep
untrusted workspace/build code from reaching host services, including this
listener, and preserve separate Tunnel credentials/users. A literal loopback
peer check and loopback Host check reject off-host traffic and DNS rebinding;
Origin and Sec-Fetch-Site requests are rejected because browser access is not the
Tunnel path. Forwarded headers grant no trust. These checks do not authenticate
one local Unix user or distinguish the Tunnel process from another trusted host
process. There is no new token, signature or user-authentication architecture.
Do not expose/reverse-proxy this endpoint to untrusted clients.

Only after that transport boundary, a tools/call must carry both
`_meta["openai/subject"]` and `_meta["openai/session"]` as non-empty strings,
each at most 1024 decoded UTF-8 bytes. Invalid UTF-8, replacement characters
(including malformed surrogate escapes), malformed/non-object metadata and
duplicate keys at the envelope/params/metadata levels fail closed. Metadata is
at most 16 KiB of wire JSON, allowing escaped identity fields. Rejection returns
a fixed HTTP error before tool dispatch or workspace mutation.

Only this validated pair is passed to `IdentityFromTrustedMeta`. The admission
adapter removes both raw fields before SDK parsing and passes the derived
Identity through private request context. Its tool callback does not read wire
metadata, arguments, headers, connection state or `Mcp-Session-Id` for identity.
The transport session header is removed before SDK handling, including when an
SDK compatibility environment switch is set. Other metadata needed for protocol
negotiation remains available. Raw identity is not logged, returned, or used in
object names. SDK and HTTP wire diagnostics are disabled; daemon messages cover
fixed startup/shutdown outcomes rather than request data.

#43 provides feasibility evidence for OpenAI metadata delivery, not proof that
arbitrary wire metadata is trustworthy. Local tests simulate the trusted host
side of this boundary. Real metadata delivery and final Tunnel compatibility
remain #96 system verification; host/network enforcement is #92/#94/#95 work.

## Resource limits

| Resource | Production bound |
| --- | --- |
| HTTP body, including chunked bodies | 9 MiB, enough for existing escaped tool arguments |
| HTTP header configuration | 16 KiB `MaxHeaderBytes` (stdlib permits its additional 4 KiB read allowance) |
| Header read and idle connection time | 5 seconds |
| Body read / total HTTP request lifetime | 45 seconds |
| HTTP write deadline | 46 seconds |
| Accepted connections, including pre-header and idle | 32 |
| Concurrent HTTP admissions / SDK ephemeral sessions | 8; excess receives 503 without waiting |
| JSON-RPC requests per HTTP admission | One; batches rejected |
| Tool-call metadata | 16 KiB encoded; each identity field 1024 decoded bytes |
| Method / tool names | 128 decoded bytes |
| Daemon shutdown / drain | 10 seconds total |

These constants live in `cmd/mcp-workspaced/ingress.go`; deployment consumes the
explicit listen flag and documented bounds. They are finite operator policy,
not new public tool options. Tool calls also retain the existing `-tool-timeout`
(default 30 seconds); the HTTP deadline caps a larger configured tool timeout.

The SDK uses stateless Streamable HTTP and JSON responses. No persistent session
map, event replay store or standing GET/SSE stream is enabled. Every admitted
request keeps its concurrency slot until its inner handler actually exits, even
if the outer stdlib timeout handler has already returned a bounded HTTP outcome.
Backend permits remain governed by the existing Service ledger. The connection
cap includes slow/pre-header/idle connections; excess sockets are closed.

## Shutdown and cancellation

SIGINT/SIGTERM closes new admission and cancels TTL. HTTP shutdown closes
admission again, cancels active request contexts, drains within the shared
10-second budget, then force-closes sockets if necessary. The daemon waits for
TTL to finish and calls the existing `Service.Close`, which cancels/waits for
owned build workers before releasing singleton ownership. TTL runs in one
separate goroutine against that same Service so a lifecycle wait cannot postpone
the start of the shutdown budget.

The SDK's old-protocol detached context is explicitly linked back to the HTTP
request lifetime. Caller disconnect/deadline therefore bounds control-plane
waiting for both locally tested protocol families. No operation is replayed.
Cancellation does not prove backend terminality: unknown work retains exact
ledger ownership until trusted completion, and accepted asynchronous builds keep
the existing Service-owned lifetime. Shutdown does not retarget work.

If a trusted runtime adapter fails to unwind within the shutdown budget, the
daemon returns a fixed error and main exits nonzero. It does not unlock the
singleton early to permit another live daemon; OS process exit releases the
lock and subsequent startup reconciliation owns remaining durable intent. This
is bounded process shutdown, not a claim that cancellation terminated backend
work or completed recovery. #93 owns further terminality/recovery behavior.

## Local evidence and remaining gates

Tests instantiate the production constructor and HTTP server with an in-memory
Runtime, negotiate MCP 2025-11-25 and 2026-07-28, discover exactly five tools and
exercise Service dispatch. Negative cases cover identity shape/bounds, duplicate
metadata, argument/header/session overrides, malformed requests, browser/rebinding
requests, batch/body/concurrency bounds, startup reconciliation and shutdown.
Cancellation tests check one submission and retained uncertain accounting before
late completion. Lifecycle fixtures check PRESENT/ABSENT/BUILDING/FINALIZING,
contention, closed admission and replacement fencing for all three exec/file APIs.

Independent source review is required separately. No real Podman, systemd,
Secure MCP Tunnel, ChatGPT, OCI or remote integration verification is performed
by these tests. #92 supplies the source runtime/network/DNS profile; #93 recovery,
#94 deployment, #95 dirty-OCI integration (I1), and #96 clean
reconstruction/ChatGPT/Tunnel E2E (I2) remain deferred.

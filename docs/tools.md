# Workspace tools

`Service.Exec`, `ReadText` and `WriteText` require a trusted `Identity` and the
already admitted `Exact` PRESENT generation. They never create, repair, restart,
retarget or retry a generation. `RegisterTools(server, service, admit)` installs
the public MCP `workspace_create`, `workspace_destroy`, `exec`, `read_text` and
`write_text` tools on an embedded server.
The host-supplied admission callback must attest identity and bind the exact
generation; caller arguments and unverified wire `_meta` cannot supply authority.
For create/destroy the callback attests identity and may return a zero Exact;
the lifecycle service selects the exact current candidate itself.
Registration itself opens no listener. The command's smoke server remains
tool-free, while the daemon supplies the production loopback MCP ingress described
below and in [production ingress](ingress.md).

## Requests and results

`workspace_create` takes optional `dockerfile` text and returns
`{"status":"PROCESSING"}` for an accepted asynchronous candidate or
`{"status":"PRESENT"}` for the existing/finalized generation. Omission retains
canonical creation; empty/null supplied text does not. Polling BUILDING/FINALIZING
or creating against PRESENT ignores newly supplied text. A definite terminal
failure returns bounded `build_failed` and drives exact cleanup; replacement
requires a later create after ABSENT. Uncertainty returns `outcome_unknown`,
preserving the exact candidate for recovery. No workspace or job ID is exposed.
`workspace_destroy` takes no arguments and returns `{"status":"ABSENT"}` only
after exact cleanup. During cancellation it may return uncertainty until trusted
terminal handling completes. See [the lifecycle contract](lifecycle.md#dockerfile-creation-p5).

`exec` takes `argv` (1–256 strings), optional absolute container `cwd` (default
`/`), string-to-string `env` overlay, UTF-8 `stdin` and boolean `detach` (default
false). There is no implicit shell parsing: `['/bin/sh', '-lc', '...']` is explicit
shell usage. argv/cwd/env/stdin reject NUL. Environment names must be nonempty
without `=`. Paths reject relative forms and lexical `..` components before
normalization. The argv+env limit is 256 KiB of encoded JSON
`{"argv":[...],"env":{...}}` (empty/nil env may be omitted); stdin is at most
1 MiB of UTF-8 bytes. MCP decoding also has an 8 MiB wire argument bound covering
worst-case escaping, rejects null/type mismatches and lone UTF-16 surrogate
escapes, and never reflects raw decoder diagnostics.

Foreground completion returns `exit_status`, and separate `stdout` / `stderr`
objects containing `text`, original `total_bytes`, `truncated` and `invalid_utf8`.
Each stream drains all output while retaining at most 64 KiB, half head and half
tail. Text concatenates the retained head/tail without an inserted marker;
`truncated` indicates the gap. Invalid UTF-8 uses ASCII `?` replacement without
expanding that limit. Source UTF-8 validity is scanned across chunk boundaries,
including discarded output. JSON escaping can expand each stream by at most six
times its retained byte limit. Nonzero workload exit status remains a completed
result. A runtime error or uncertain completion is a tool error instead.

Detached success is exactly `{"status":"started"}` after native acceptance,
without a job ID, retained workload output, liveness promise or status API. The
runtime submission permit covers acceptance, not the detached workload lifetime.
Acceptance/completion does not refresh TTL beyond the admitted request activity.

`read_text` takes `path` and returns `{"text":"..."}`. It rejects files over
1 MiB and invalid UTF-8 with bounded errors. `write_text` takes `path` and UTF-8
`content` of at most 1 MiB and returns `{"status":"written"}`. File text may
include UTF-8 NUL characters; the exec request's NUL prohibition is separate.
Writes create absent targets or truncate writable existing non-directory
targets, with no append, parent creation or atomic replacement guarantee.
Failed or uncertain writes may leave partial content.

## Injection, completion and runtime adapters

Every operation uses the landed `Service.Inject` PID/cgroup gate and backend
ledger, including both file helpers. The trusted optional `ExecRuntime.StartExec`
capability receives the freshly attested snapshot and targets its immutable
container object ID. It must submit promptly once, honor cancellation, stream
output without unbounded buffering and provide one terminal `ExecOutcome`.
Only definite completion resolves exact-operation accounting. Submission errors,
panic, lost completion channel, cancellation and timeout conservatively retain
uncertain accounting; no internal replay occurs. Late trusted terminal evidence
can resolve the original operation, independently of the caller lifetime.

`Config.ToolTimeout` is an operator-owned finite control-plane lifetime for exec
and file calls, including admission/probe/submission waits. Zero selects 30 seconds;
negative values are invalid. The command exposes positive `-tool-timeout`.
MCP registration starts the deadline before the host admission callback and
passes that same deadline through to the Service operation. The callback must
honor cancellation and must not submit runtime work itself. The handler bounds
its wait even if the callback stalls, and discards late admission results without
dispatching a tool. Go cannot forcibly stop a callback that ignores cancellation;
its worker remains until the callback returns.
Deadline expiry while admission/submission is in flight returns uncertainty;
a context already canceled before admission returns cancellation. Trusted recovery
continues to use P4a `PendingBackend` / `RecoverBackend`; this adds no durable job
system or lifecycle state.

`ToolError` supplies fixed `code` / `message` and, for uncertainty,
`outcome_unknown: true`. Arbitrary runtime/decoder errors, filenames, argv, content,
stdout/stderr and raw error chains are excluded from error responses. The typed
Go error retains only bounded sentinel causes. Unknown completion explicitly warns
that side effects may have begun and must not be replayed automatically.

The Podman streaming adapter invokes the local CLI with `--remote=false`, literal
argv, sorted per-process env flags, stdin and separate bounded output sinks. It
uses the snapshot's full immutable ID, never a workspace/container name. Killing
the local CLI does not prove injected workload completion; cancellation, pipe loss
and reserved Podman failure statuses 125–127 retain uncertain accounting.
The production Podman adapter implements `ProcessProbe` on Linux and consumes
the existing admission substrate. It freshly discovers exact ownership, inspects
the full immutable ID, and checks running state, PID and start time against pinned
procfs directories. It binds the actual PID membership to the reported resource
cgroup, whose generated `libpod-<full-ID>.scope`, `libpod-<full-ID>` or
`libpod-payload-<full-ID>` name must match the immutable object. PID membership
may be in a descendant; `pids.current` is read at the resource root to count its
entire task subtree. Limits are read from the actual member group through its
ancestors, and CPU weight is read at the resource root. `cpu.max` bandwidth is
normalized to Podman's 100 ms default period. Actual NOFILE soft/hard limits come
from `/proc/<pid>/limits`, rather than desired runtime configuration.

The probe opens only fixed host `/proc` and `/sys/fs/cgroup` roots, verifies their
kernel filesystem types and a full unified cgroup mount, and bounds every read.
It pins process/cgroup directory identities, repeats runtime inspection and
process/resource observations, and rejects PID reuse, membership migration,
cgroup replacement, changed limits, missing/ambiguous data or cancellation.
The larger of both observed subtree task counts reaches the existing gate.
Non-Linux hosts, partial cgroup mounts, unavailable controller files, frozen
groups and mismatched policies fail closed without injection. Read-only probe
failure attests no submitted workload and releases the existing reservation;
uncertain exec completion retains it. No host path argument or workload mount
is added, and this slice does not claim live Podman execution evidence.

File operations run fixed `/bin/sh` helpers inside the container: bounded
`head -c 1048577 -- "$1"` for reads and `cat > "$1"` for writes. Paths are
positional arguments and content travels only through stdin. The image must
provide these utilities. Symlinks resolve entirely inside the container filesystem
namespace; file content operations introduce no host mounts or host file I/O.

## Local evidence

Tests use fake runtimes/process snapshots, fault injection, in-memory MCP
connections, a local stub runtime executable and temporary file-helper fixtures.
They cover exact-generation replacement and absence, all injection/backend gates,
timeout/cancellation and late recovery, detached acceptance, bounded head/tail
output and encoding errors, strict public request decoding, file UTF-8/size/path
and create/truncate/symlink behavior, partial writes, no replay and persisted
unknown accounting. Procfs/cgroup fixtures and fault injection also exercise the
ordinary production `*Podman` probe and all three tool paths, including inherited
limits, subtree membership, PID/cgroup reuse and fail-closed behavior. Fixture helper/probe execution is not OCI validation. Composed dirty-OCI
verification of the delivered exact revision is owned by Issue #95 under the #90
execution graph; clean reconstruction and real ChatGPT/Secure MCP Tunnel E2E are
owned by Issue #96.

## Production admission

The daemon supplies the `RegisterTools` callback from its loopback HTTP admission
boundary; see [production ingress](ingress.md). `Service.CurrentGeneration` reads
the exact current PRESENT intent under the existing lifecycle lock. It fails
promptly on contention or a non-PRESENT candidate, and never creates, probes,
refreshes activity or caches a target. An absent workspace returns
`generation_unavailable`; BUILDING/FINALIZING returns `not_ready`. Outstanding
uncertain lifecycle work remains fail-closed. The subsequent existing `Inject`
rechecks the same W+G, so replacement after lookup rejects the old operation
rather than selecting the new generation. Create/destroy retain Service-owned
generation selection. This adds no public identity or generation parameter.

## Failed Dockerfile observation

The existing `workspace_create` error result may carry a `build_diagnostic`
object with separate `stdout` / `stderr` stream results (at most 64 KiB each),
optional `exit_status`, and `available`. It is prepared only for the exact known
terminal failed candidate. Cleanup uncertainty can accompany that diagnostic
with `outcome_unknown: true`; a replacement is still forbidden. The observation
call ignores supplied replacement text. Only a later create after confirmed
ABSENT allocates fresh G. Unknown build completion returns outcome-unknown,
without claiming terminal build failure. See [trusted recovery](recovery.md)
for retention, cleanup and operator-only backend/storage recovery.

# Exec and text tools (P3a)

`Service.Exec`, `ReadText` and `WriteText` require a trusted `Identity` and the
already admitted `Exact` PRESENT generation. They never create, repair, restart,
retarget or retry a generation. `RegisterTools(server, service, admit)` installs
the public MCP `exec`, `read_text` and `write_text` tools on an embedded server.
The host-supplied admission callback must attest identity and bind the exact
generation; caller arguments and unverified wire `_meta` cannot supply authority.
Registration opens no listener. The command's smoke server remains tool-free,
and the daemon still supplies no product ingress.

## Requests and results

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
The landed Podman adapter **still lacks `ProcessProbe`**, so it fails closed at
the injection gate until a trusted host process probe is supplied. This slice
does not change that admission substrate or claim live Podman execution evidence.

File operations run fixed `/bin/sh` helpers inside the container: bounded
`head -c 1048577 -- "$1"` for reads and `cat > "$1"` for writes. Paths are
positional arguments and content travels only through stdin. The image must
provide these utilities. Symlinks resolve entirely inside the container filesystem
namespace; production code introduces no host mounts or host file I/O for tools.

## Local evidence

Tests use fake runtimes/process snapshots, fault injection, in-memory MCP
connections, a local stub runtime executable and temporary file-helper fixtures.
They cover exact-generation replacement and absence, all injection/backend gates,
timeout/cancellation and late recovery, detached acceptance, bounded head/tail
output and encoding errors, strict public request decoding, file UTF-8/size/path
and create/truncate/symlink behavior, partial writes, no replay and persisted
unknown accounting. Fixture helper execution is not OCI validation. Real Podman
verification of the delivered exact revision remains external Issue #71 scope.

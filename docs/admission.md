# Admission controls (P4a)

The lifecycle service now owns generation, backend, process injection, TTL and
shared-pool storage admission. P4a supplies host-side primitives for exec/build
consumers without additional ingress. [P3a tools](tools.md) consume this substrate.
All policy, probes, callbacks and recovery APIs are trusted operator/runtime code,
never caller-provided tool arguments. No scheduler or database is added: the daemon
uses a TTL timer and admission state uses the existing singleton lifecycle journal.

## Operator policy and resources

`ReferencePolicy()` centralizes canonical reference-host defaults:

| Setting | Default |
| --- | --- |
| Memory per resource-bearing generation | 4 GiB |
| Resource-bearing generations | 2 |
| Swap per generation | 0 |
| CPU hard quota | none (`-1`) |
| Effective cgroup v2 CPU weight | 100 |
| `pids.max` | 512 |
| Bounded raced migration observation | 513 tasks |
| NOFILE soft / hard | 1024 / 1024 |
| Live or outcome-uncertain backend operations | 4 |
| Retained completed operation metadata | 16 entries |
| Idle / absolute TTL | 30 minutes / 24 hours |
| Available bytes / free inode safety minimums | 1 GiB / 1024 |

TTL and storage defaults are local operator defaults; the Issue does not prescribe
those values. Positive TTL and storage thresholds are required; callers cannot
extend or disable them. `Config.Policy` is copied on open. The daemon exposes
`-memory-bytes`, `-max-generations`, `-swap-bytes`, `-cpu-quota`, `-cpu-weight`,
`-pids-max`, `-migration-tasks-max`, `-nofile`, `-max-backend-operations`,
`-idle-ttl`, `-absolute-ttl`, `-min-available-bytes` and `-min-free-inodes`.
Migration allowance must equal the configured task ceiling plus one.

The Podman adapter applies memory, memory-plus-swap, CPU quota, cgroup v2 CPU
weight, PID limit and equal NOFILE soft/hard limits to creation. The daemon gives
`NewPodmanWithPolicy` and the service the same policy. Embedded hosts must do the
same. These flags are locally tested with stub commands; their real-host behavior
is external validation scope.

P5 Dockerfile candidates use those same generation slots and backend ledger.
Acceptance reserves a create operation and a build-control operation so stopping
an uncertain build remains accounted even when it overlaps live work. Definite
build completion releases the control reservation; sequential finalization uses
the original create reservation. Diagnostics alone retain no live permit.
Generation cleanup remains required before freeing its slot. Build parents use
the configured 512 task ceiling, never the 513-task migration observation allowance.

## Generations and process injection

Durable PRESENT intent consumes a generation slot, including missing runtime
objects, incomplete creates and uncertain destroys. Check, allocation, durable
intent and runtime creation share lifecycle serialization. Only confirmed removal
with no pending backend work frees the slot. There is no automatic replacement
of a missing or stopped generation.

`Service.Inject` is the process injection gate. An optional `ProcessProbe`
capability of the trusted runtime must freshly validate the exact W/G, immutable
container ID, live PID and start identity, cgroup path and identity, subtree task
count, and effective limits. It must validate actual PID/cgroup membership, never
return cached desired settings. The callback receives that snapshot immediately
after validation under the lifecycle lock. It must target the immutable runtime
object and exact generation, preserve the PID/cgroup identities when using them,
submit at most one injection, return promptly, and never reenter the service.

Counts below the task ceiling permit submission. At 512 or the tolerated raced
513, submission is rejected until usage falls. Counts beyond the configured
migration allowance, uncertain probes, missing capabilities or mismatched limits
fail closed. No cleanup or killing is inferred solely from a task observation.
There is at most one unfinished injection per exact generation, including across
restart. Outcome-uncertain lifecycle mutations also block injection and new backend
work for that generation. Stale exact state and stale snapshots cannot authorize a replacement.

P3a adds [exec/text tools](tools.md) consuming this gate, a streaming Podman exec
adapter and a production Linux `ProcessProbe`. The probe binds a full immutable
container ID to fresh runtime state and actual procfs/cgroup v2 observations.
Missing, mismatched or raced evidence fails closed. Runtime paths without this
capability still cannot inject. Deterministic fixtures exercise the production
probe; real PID/cgroup/OCI validation remains external evidence.

## Backend completion and recovery

`BackendLedger` is reusable independently; service paths share one ledger for
create/destroy, injection, and later `BeginBackend` consumers. Reservations are
persisted before submission. `BeginBackend` returns the exact `BackendOperation`
identity, including its trusted kernel BootID; live host completion calls
`RecoverBackend(operation, DefinitelyCompleted)`.
It must also be used for definitely failed work or an unsubmitted reservation.
The operation kind is bounded host metadata. Injection reservations must go
through `Inject`, not `BeginBackend`.

Request cancellation/deadlines and ordinary runtime errors do not prove backend
completion. Unknown work retains its global permit and injection reservation;
backend recovery and restart do not infer completion from momentary absence.
Runtime success or trusted `CompletedBackendError` attests terminal completion;
partial side effects still retain lifecycle intent. The synchronous CLI adapter
attests a terminal failure only after its local command returns without
cancellation or explicit uncertainty. Runtime Initialize/List are synchronous
read-only probes and must leave no backend work alive on return.

`PendingBackend` provides exact operation identities for recovery. The exclusive
[operator recovery mode](recovery.md) gathers fresh kernel boot evidence and can
release a selected prior-boot operation; current-boot and missing historical
BootID remain fail-closed. Startup does not release permits. Reboot does not
establish generation/residue absence. Incorrect
W/G, ID or kind cannot release another operation's permit. Durable monotonic IDs
prevent stale recovery from resolving a later operation, including after restart.
Completed diagnostics are bounded metadata without workload output and consume
no live permits. Neither completion nor diagnostics refresh idle TTL. Destroy
fails closed while exact backend completion is unresolved, preventing a late
operation from recreating a retired generation.

## TTL

Creation and last admitted MCP activity are journaled per exact generation.
`AdmitActivity` is for admitted MCP requests; idempotent create and process
submission also refresh idle activity. Status reads, detached workload activity,
backend completion/recovery and runtime probes alone do not refresh it. Absolute
creation time never changes. Process submission also rechecks absolute expiry
after its probe. `BeginBackend` rejects idle- or absolute-expired generations
before reserving a permit and does not refresh activity itself. Consumers must
record admitted MCP activity separately with `AdmitActivity`. Expiry does not
release existing backend permits or prevent their exact-operation recovery.

`ExpiredGenerations` returns hints only. `CleanupExpired` compares exact current
intent and rereads current activity under the lifecycle lock before cleanup.
A refreshed idle hint or replacement generation is rejected. Storage-pressure
cleanup uses the same serialized exact TTL eligibility. The daemon checks TTL
periodically (at most one minute between scans, or half an operator-selected
short idle TTL); benign stale hints are ignored. An unresolved cleanup error
stops the daemon with intent preserved, requiring host recovery.

Journal version 2 persists chronology, pending operations, the operation counter,
and the storage-recovery latch. Its lifetime entries may additionally carry optional
exact build phase/image intent; no other persistence authority is introduced.
Empty version 1 journals are accepted. Nonempty
version 1 journals have no trusted creation/activity time and fail closed before
runtime access. Operators must resolve prior lifecycle intent with the prior
version before upgrading, or supply independently verified chronology as an
explicit host recovery. The service never guesses timestamps or silently grants
a fresh absolute lifetime. A startup clock older than saved activity also fails
closed. Failed journal durability closes lifecycle admission.

## Shared disposable Data pool

`Config.DataDir` / `-data-dir` identifies the shared disposable pool filesystem.
It defaults to the state-directory filesystem; the operator must point it at the
actual Data pool if that is elsewhere. The pool must already exist. Sampling
uses Linux statfs normal-user `Bavail` bytes (excluding reserved blocks) and free
inodes. No per-workspace quota, reservation or arbitrary file deletion is added.

Fresh creation requires both margins to be at least their configured safety
minimums. Unsafe margins allow cleanup only of independently TTL-eligible exact
generations, then require a fresh safe sample. Pressure supplies no eligibility
for active workspaces. Failed probes, cleanup failures, or remaining unsafe margins
latch MCP activity, creation/backend/injection and caller destroy admission closed
for operator recovery. Existing intent remains inspectable; operator
`DestroyGeneration` remains exact-generation fenced.

Exactly zero available blocks bypass all automatic pressure/TTL cleanup. The
operator must restore both safe margins, then invoke host-only `RecoverStorage`;
a safe sample or restart alone does not clear a persisted latch. Recovery only
samples and persists state, never guesses files to delete. If exhausted storage
also prevents journaling the latch, the daemon closes admission; after operator
space recovery, startup reconciliation is required before further activity.

## Local evidence

Fake runtimes, cgroup/process snapshots, injected clocks, storage samples and
thresholds exercise concurrent ceilings, unknown outcomes, cancellation, stale
callbacks, durable restart accounting, TTL hint revalidation, byte/inode pressure,
and operator recovery. Stub CLI tests cover reference and overridden resource
arguments. These tests do not access OCI or exhaust a real filesystem.

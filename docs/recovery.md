# Trusted recovery

Issue #93 implements source-level terminality and recovery. The Human-selected
Linux boot epoch supplies restart-verifiable backend terminal evidence for v0.
No Design Authority #20 text is changed. Local tests use fake runtime commands,
procfs/cgroup fixtures, and simulated previous boot records. Actual saturation,
user-scope killing, control-plane survival, reboot recovery and resource bounds
on OCI remain #95 evidence. Deployment is #94; clean ChatGPT/Tunnel E2E is #96.

The [deployment operations runbook](deployment.md#operations-and-exclusive-recovery)
provides the same CLI with the configured account, resolver namespace, user bus,
image, journal/Data paths and policy. Stop admission first; no journal edits,
global prune or whole-user kills are part of that procedure.

## Terminal-evidence map

Each backend reservation is durably recorded before submission with operation ID,
exact opaque W+G, kind, and the trusted kernel BootID. Linux reads BootID directly
from `/proc/sys/kernel/random/boot_id` and verifies procfs and UUID syntax; no
flag, environment variable, tool argument or operator assertion supplies it.
A process cannot survive a host reboot, so the admission epoch is fixed for that
Service lifetime. Recovery takes a fresh kernel sample.

BootID is an additive field in the existing version-2 journal. New reservations
always record it; old valid records without it remain readable and reserved.
Reconciliation never backfills missing epochs or upgrades them into evidence.

| Persisted kind | Owned work and state | Live completion evidence | Recovery after restart |
| --- | --- | --- | --- |
| `create`, canonical | Exact candidate; local create/start commands and possible container | Trusted adapter terminal return; generation remains until exact cleanup | Different recorded BootID proves prior execution terminal; container existence/absence alone does not |
| `create`, Dockerfile | Exact build phase/image/Data/parent, followed by finalization under the same reservation | Terminal build and drains, exact parent termination, then terminal finalization | Different BootID releases the selected reservation only; build result stays unknown and residue stays owned |
| `build-control` | Reserved capacity for exact parent/control operations | Terminal build/control return | Different BootID; parent absence alone cannot prove an outside submitter/control command finished |
| `destroy` | Exact container removal and optional Buildah/image/parent/Data cleanup | Definite pre-submission failure, or completed exact cleanup and independent absence checks; failure retains generation intent | Different BootID proves backend terminality; cleanup must still establish absence separately |
| `injection` | Exact exec/read/write submission; one unfinished injection per generation | Trusted execution completion/native detached acceptance, with terminal transport | Different BootID; current container/PID observations do not reconstruct the old execution result |
| Other `BeginBackend` kinds | Bounded exact host-side operation metadata; no current product callers | Consumer-supplied trusted terminal evidence | Same boot-epoch rule for v0 local work; no inferred kind-specific probe |

The CLI deliberately refuses current-boot uncertain operations. Existing live
operation-specific completion paths remain usable inside the owning Service.
Missing historical BootID stays fail-closed even after reboot; it cannot be
retrospectively supplied by an operator. Malformed nonempty BootID rejects startup.
Neither startup, elapsed time, log contents, a successful kill command, a safe
storage sample, nor momentary container/cgroup absence releases backend permits.
There is no automatic replay or startup permit recovery.

Releasing a prior-boot permit proves terminal execution only. It does not establish
success, definite build failure, or workspace/runtime/build absence. Exact
cleanup must remove all required W+G state before the generation slot is released.
The original unknown build is never published or restarted.

## Exclusive command mode

Run recovery as the same service account with the same image, state/Data paths,
Podman executable, DNS and resource/storage policy configuration as the daemon.
The normal daemon must relinquish lifecycle ownership first. Recovery uses the
same `workspace.Open`, reconciliation and account-wide singleton; a competing
process fails before runtime discovery. This mode has no listener and is mutually
exclusive with `-daemon` and `-smoke`. It is absent from the five-tool MCP surface.

Example inspection (use the deployment's actual values and policy flags):

```sh
mcp-workspaced -recover inspect \
  -state-dir "$STATE_DIR" -data-dir "$DATA_DIR" \
  -image "$CANONICAL_IMAGE" -public-dns "$PUBLIC_DNS_PRIMARY"
```

Inspection emits bounded JSON containing only journal-owned backend operations,
opaque W/G correlation, selection tokens, terminal-evidence classification,
owned generation cleanup selections, and storage-latch state. It exposes no raw
OpenAI subject/session, workload output or arbitrary runtime inventory.

Use the displayed `operations[].selection` for one backend recovery:

```sh
mcp-workspaced -recover backend -recovery-selection "$BACKEND_SELECTION" \
  -state-dir "$STATE_DIR" -data-dir "$DATA_DIR" \
  -image "$CANONICAL_IMAGE" -public-dns "$PUBLIC_DNS_PRIMARY"
```

The selection binds the displayed ID, W+G, kind and BootID. A stale or mismatched
selection fails. The product obtains evidence itself and calls exact ledger
recovery only for a nonempty valid BootID different from the current kernel boot.
There is no `--completed`, trust switch, journal-edit workflow or lock bypass.
Reboot is an external operator/deployment action; this command never initiates it.

Each release updates exact injection bookkeeping and durably commits the journal
under lifecycle serialization before admission can proceed. An unsuccessful or
uncertain commit closes Service admission. A subsequent open reconciles the
actual durable state; it does not guess whether the previous commit succeeded.

After backend/storage recovery, explicit exact cleanup is available using
`generations[].selection`:

```sh
mcp-workspaced -recover cleanup -recovery-selection "$GENERATION_SELECTION" \
  -state-dir "$STATE_DIR" -data-dir "$DATA_DIR" \
  -image "$CANONICAL_IMAGE" -public-dns "$PUBLIC_DNS_PRIMARY"
```

Cleanup uses the normal exact destroy path, refuses pending backend work, and
never allocates a replacement. A later public create obtains fresh G only after
confirmed absence and durable removal of old intent. Command success for
`backend` means one permit was recovered; it does not mean cleanup finished.
Commands have a two-minute control deadline. Deadline/failure supplies no terminal
evidence and may require another explicit inspection.

## Storage latch

Free bytes/inodes returning, inspection and ordinary daemon restart do not clear
a persisted storage latch. After restoring capacity through separately authorized
operator action, invoke `-recover storage` with the same configuration and safety
thresholds as the daemon. It samples current normal-user available bytes and free
inodes and requires both `MinAvailableBytes` and `MinFreeInodes` before durably
clearing the latch. Unsafe, failed or canceled samples leave it set. Failed commit
closes admission. This action deletes no files and never lowers thresholds.
Backend recovery and storage recovery remain distinct actions and evidence checks.

## Final workspace removal under PID pressure

Normal destroy first resolves exact W+G to the immutable container ID and attempts
`podman container rm --force -- <ID>` with a five-second bound. Success is followed
by an independent exact absence check.

A synchronous validation/discovery failure before the first removal submission
releases that invocation's backend permit, but retains exact generation intent
for a later destroy attempt. It does not establish ABSENT. After removal has
been submitted, ambiguous errors retain both backend and generation ownership.

Fallback requires expiration of that internal ordinary-removal bound while the
outer request is still live, plus fresh attestation that the same container is at
its configured PID ceiling (including the accepted one-task migration allowance).
An arbitrary Podman error, caller cancellation/deadline, missing object, malformed
inspection or uncertain ownership cannot authorize a kill.

The fallback reuses the injection probe's full W+G, immutable ID, live inspection,
PID/start-time, actual membership, cgroup inode and effective-limit checks. It
requires the exact conventional rootless path:

```text
/user.slice/user-UID.slice/user@UID.service/user.slice/libpod-FULL_ID.scope
```

It pins that inode, checks the user manager's ControlGroup, and repeats live
runtime/proc/cgroup attestation immediately before:

```text
/usr/bin/systemctl --user kill --kill-whom=all --signal=SIGKILL -- libpod-FULL_ID.scope
```

Only this exact scope is targeted. Recovery then waits within a ten-second bound
for the pinned subtree inode to be unlinked or report `populated=0` and
`pids.current=0`. Path disappearance while that inode remains linked is refused.
Only then does it retry bounded ordinary removal by immutable ID and independently
re-list exact absence. The Service also reconciles absence before committing
lifecycle removal. Ambiguity retains backend/generation ownership. Unsupported
platforms/layouts fail closed. The user-manager hierarchy must be provisioned by
#94; this source does not install units or exercise real scope killing.

## Failed-build observation

Before build submission, separate exact-generation `stdout.log` and `stderr.log`
files are created under the existing Data build directory. Each retains at most
512 KiB of prefix output, preserving the previous 1 MiB aggregate log budget.
Both OS pipes continuously drain after saturation and after either log writer
fails. No retention buffer grows with emitted output. Small result metadata
(exact ownership, emitted-byte totals and available subprocess exit status) is
synced with the logs; it never supplies terminal authority.

After definite terminal failure, the next permitted `workspace_create` prepares
an exact-generation `build_failed` diagnostic before attempting cleanup. Its
`build_diagnostic` contains distinct stdout/stderr, at most 64 KiB each, byte totals,
truncation/UTF-8 information, optional exit status and availability. Returned text
is a bounded head/tail view of the retained prefix, not a claim to retain every
emitted byte. Service Data/journal paths and absolute path tokens in engine
output are redacted. This also hides image-local absolute paths because mixed
engine/RUN output cannot reliably distinguish them. Arbitrary Go error chains
are never returned. Pre-submission failures, damaged logs and historical
combined logs can yield an unavailable diagnostic with the fixed failure message.

That observation ignores replacement Dockerfile text. Cleanup uncertainty returns
the prepared failure diagnostic with `outcome_unknown` for cleanup, retains exact
recovery ownership, and blocks fresh creation. Diagnostics survive engine/parent
cleanup failure; Data removal is attempted only after the observation is prepared
and exact runtime/build-object absence has been checked. There is no persistent
public FAILED state. A later create gets fresh G only after exact cleanup and the
durable ABSENT transition. TTL may abandon failed candidates through the existing
serialized exact cleanup path. Caller disconnect does not discard an accepted
candidate; cancellation, lost drains and restart do not invent a definite failure
or replay the build.

# Issue #230: bounded destroy regression evidence

## Authority and observed production facts

The 2026-11-07 Human dispatch authorizes this source correction and isolated
GreenCloud experiments. #20 remains design authority. Production facts are the
Human-reported reconstruction in #217 comment 6597043307, not a live inspection:
TTL expiry, daemon exit 1, unresolved destroy ownership, a stopping container,
and live conmon/sleep with StopTimeout=10 and SIGTERM. No production connection
or mutation was performed. I1 remains blocked.

Source baseline: b52aa0b8d3eb3864817e1ed93432e56387bb986f.
At preparation, origin/main was d443f6d (the only later commit is #229's unrelated
S3B overlay marker correction). This candidate is based on the production pin.

## Confirmed source defect and correction

`Podman.Destroy` bounds ordinary force removal to 5 seconds, inherited from the
caller, but omits Podman's stop time. The default 10-second stop wait can exceed
that budget for canonical `/usr/bin/sleep infinity` PID 1. Set `--time=0` on
ordinary removal and the existing conditional pressure-recovery removal. Destroy
discards the complete generation, so no graceful shutdown requirement is added.
The Go deadline is unchanged; shorter caller deadlines/cancellation still win.

Discovery still selects one exact W+G and immutable container ID. Success still
requires independent List/absence confirmation. Errors after submission remain
ErrOutcomeUnknown; backend permit/journal ownership remains retained. The scoped
PID-pressure attestation and recovery conditions are unchanged. No unconditional
retry or expanded delete scope is introduced.

TTL CleanupExpired calls destroy with the daemon context. runTTL returns cleanup
errors, and runDaemon propagates them; main exits on error. That behavior is
unchanged. Public tool and HTTP budgets (defaults 30s and 45s) do not justify
extending the adapter deadline. Pressure recovery has its existing separate
10-second child budget, still bounded by caller cancellation.

## GreenCloud real rootless environment

Existing vm-04 SSH alias and known host keys verified with StrictHostKeyChecking;
no host key enrollment. Existing mcp-runner UID 1001, subordinate IDs
165536:65536, rootless=true. Debian 13.7, amd64, kernel 6.12.113+deb13-amd64,
Podman 5.4.2, crun 1.21, conmon 2.1.12, cgroup v2, overlay/extfs.
Approximately 30GiB free on a 35GiB filesystem. No existing containers or images
in that account at preflight. Explicit cgroupfs manager was used because the
runuser session lacked a systemd user bus; no systemd/user/OS settings changed.
Production is Ubuntu 24.04/aarch64/systemd: this is not equivalent evidence.

Image: docker.io/library/debian:bookworm-slim, resolved RepoDigest
`docker.io/library/debian@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc`.
Second reported platform RepoDigest:
`sha256:c67e92c8099ba22853ab4cfda7cfb494d253d6012ca82dee98ba8976772af018`.
Final repeat used the first digest explicitly. Network none, no volumes,
`--stop-signal=SIGTERM --stop-timeout=10`, command `/usr/bin/sleep infinity`.

## Before/after measurements (2026-11-07 JST)

| Case / unique name | Exact container ID | Result |
| --- | --- | --- |
| before / mcp230-before-20261107-0320 | 2a54f75dff84acbab43c861966868e1e174e9a22c6998a22e795024f83f72e71 | 5.003181741s, signal killed, context deadline exceeded; stopping, Running=false, FinishedAt unset; conmon 29519 and sleep 29521 live |
| after / mcp230-after-20261107-0322 | 043b8442eba7d40e9232781e930235bd0ef9ff257dd9fff1878bb2e787f3e29e | --time=0: 108.922450ms, exit 0, context nil; exists exit 1 |
| repeat / mcp230-repeat-20261107-0324 | a417b762702a1e5b641c7b89b60b6881221a05a98ca973284ef719c1cf523ea5 | --time=0: 98.970974ms, exit 0, context nil; exists exit 1 |

The before container remained stopping on a later read-only inspection. Its
known exact ID was cleaned once using --time=0: 122.208271ms, exit 0, exists exit
1; both recorded PIDs gone. Final account container list empty. No global prune,
existing workload mutation, broad kill, or production access occurred. The pulled
test image remains cached; no image pruning was performed.

The measurement harness uses Go exec.CommandContext, 5-second context,
WaitDelay=1s, separate bytes.Buffer stdout/stderr (not inherited terminal pipes),
and `/usr/bin/podman --remote=false --cgroup-manager=cgroupfs container rm
--force [--time=0] -- EXACT_ID`. It prints elapsed time, Run error, ctx.Err and
stdout/stderr. It was cross-compiled linux/amd64 with Go 1.26.9, CGO_ENABLED=0.
This measures the real CLI interruption behavior, not shell timeout. The harness
mirrors the production command path; it does not execute the full Service,
journal, TTL scheduler or production Podman profile. Its small output buffers
are not the production 8MiB bounded-output wrapper. Source tests cover adapter
ownership and lifecycle behavior. All test containers use the required mcp230-
prefix; no canonical mcpws- names were created.

## Causality and remaining boundaries

This confirms the stop-window/deadline defect and reproduces the observed
stopping/live-process pattern. It does not prove the unique production root
cause, production signal delivery, or production recovery. Different Podman,
kernel, architecture, systemd, pressure, storage or runtime conditions may still
cause deadline expiry, correctly remaining fail-closed. Immediate SIGKILL can
interrupt workload writes; full-generation destroy already discards that state.

Independent source review must bind its disposition to the candidate SHA.
Human landing, separately authorized OCI recovery/deployment and affected I1
regression plus remaining A–G checks are required. The daemon-wide TTL error
policy is a separate Human design question, not changed by this patch.

## Source verification

Final `./scripts/verify-source`: exit 0, PASS. Includes module verification,
gofmt, `go test ./...`, `go test -race ./...`, `go vet ./...`, build, daemon
smoke, artifact readelf inspection, Ansible local syntax/render/component checks
(138 Python tests), and canonical source checks (3 Python tests). No live
inventory or production deployment was run. The script's generic NOT EXECUTED
footer refers to its own source-only boundary; the isolated Podman measurements
above were executed separately.

Updated exact-argv tests require --time=0 for ordinary and conditional removal.
Added TestDestroyImmediateStopPreservesDeadlineBudget checks the default 5s
adapter budget and a shorter 1s caller budget without sleeping. Existing pressure
and lifecycle tests exercise cancellation, retained unknown backend ownership,
no false terminality, absence confirmation and later-generation exclusion.

# Workspace lifecycle

`internal/workspace` contains the host-side lifecycle service and a narrow Podman
CLI adapter. The MCP smoke server remains tool-free; the production daemon
exposes the five tools through [loopback ingress](ingress.md). P4a adds reusable
TTL and resource/backend/storage admission on this substrate; see
[the admission contract](admission.md) for current policy and recovery behavior.
[Tools](tools.md) provide embedded lifecycle and exec/text registration using that gate.

## Identity and authority

`IdentityFromTrustedMeta` must receive host-attested `_meta["openai/subject"]` and
`_meta["openai/session"]`. Both must be nonempty strings. Unverified wire metadata
must never be passed directly to this boundary; the production ingress validates
the trusted-host request before deriving identity. Caller workspace/user/session
arguments and
`Mcp-Session-Id` do not contribute to identity.

W is a domain-separated SHA-256 hash of the length-prefixed subject/session pair.
The raw values are discarded. G is a fresh cryptographically random 256-bit
identifier for each candidate. `WorkspaceKey` and `Generation` are distinct Go
types. `Exact{Workspace, Generation}` is required by every runtime create/destroy
and delayed cleanup/publication operation.

Container names have the exact form `mcpws-v1-<W>-<G>`. Ownership requires matching
`io.mcp-workspaced.owner`, `.workspace`, `.generation` and `.image` labels, a full
container ID and the configured digest-pinned image, or the journaled immutable
candidate-built image ID. Unknown labels in the service
namespace, inconsistent names/labels, unrecorded generations and a different
canonical image cause reconciliation to fail closed. Unrelated containers are
ignored; service lookalikes are never adopted or cleaned up automatically.

## State and failure behavior

The only public states are ABSENT and PRESENT. The durable journal records exact
generation intent before create can mutate the runtime. For canonical creation, a partial, failed or
unconfirmed create conservatively retains PRESENT, including across restart.
Create on PRESENT returns the existing generation and records admitted idle activity
within the operator absolute TTL, without runtime inspection,
repair, restart or replacement. An explicit successful destroy is needed before
another candidate can be allocated.

Destroy first validates discovery against the journal, invokes exact-generation
removal, then independently confirms absence. Only a successful removal and
confirmed absence permit durable removal of intent. A runtime error (even after
the container disappeared), an unconfirmed removal or a discovery error retains
PRESENT. A journal write/sync error closes admission because the durable result
may be uncertain; startup must reconcile again.

Lifecycle operations are serialized with generation comparison and publication.
`DestroyGeneration` rejects stale cleanup. `WithGeneration` rejects stale
completions/publications before executing a host-side callback under the same
lock; callbacks must not reenter the service. Closing the service waits for active
operations before releasing singleton ownership.

Startup acquires an OS-managed nonblocking Linux `flock` before constructing the
runtime. The fixed lock is under the effective service account's OS-recorded home
at `.local/state/mcp-workspaced/daemon.lock`, independent of `HOME` overrides and
the configured journal directory. It remains on disk and is never unlinked;
closing the descriptor or process exit releases ownership. Lock/state directories
and files must be private and owned by the effective account. Non-Linux startup
fails closed.

Before admitting operations, startup validates the journal, verifies rootless
runtime mode and reconciles all service-owned containers with saved exact intent.
It does not mutate runtime objects. Recorded generations whose containers are
missing or stopped still remain PRESENT. Any uncertain or unrecognized discovery
fails startup and releases the lock. A new empty journal cannot silently adopt
existing service containers. Changing the configured canonical image while an
existing journal uses another image fails closed; image migration is outside this
slice.

## Daemon configuration and runtime boundary

The optional `-daemon` mode requires `-image` with a canonical `@sha256:<64 lowercase
hex characters>` reference already installed in the service account's local image
store. `-state-dir` optionally selects an absolute private journal directory;
the default is `.local/state/mcp-workspaced` under the OS-recorded account home.
`-podman` selects an absolute host executable (default `/usr/bin/podman`). Startup
reconciliation finishes before the daemon announces readiness and waits for
SIGINT/SIGTERM. `-mcp-listen` explicitly supplies the literal loopback IP and port;
normal admission starts only after reconciliation. See [production ingress](ingress.md)
for the request boundary and bounded shutdown behavior.

The adapter runs without a shell, explicitly uses `--remote=false`, drops remote
connection environment variables and checks `info --format=json` for rootless
mode. Production execution requires a non-root service account. Discovery uses
JSON list/inspect output. Final-container create uses the canonical image or an
exact candidate-built image ID, never pulls, does
not mount host paths or engine sockets, disables implicit image volumes and uses
no privileged/nested engine. Both canonical and custom final containers use the
same service-controlled runtime profile described below, drop container
capabilities, enable no-new-privileges and apply the configured P4a resource limits.
Process injection remains a separate trusted runtime capability; see the admission contract.
Start/remove operate on full immutable container IDs resolved and validated for
exact W/G; neither logical-key deletion nor bulk/prune operations exist. Output
is bounded and commands honor context cancellation. No Podman/libpod SDK is used.

## Trusted runtime and DNS profile

Production configuration requires one to three `-public-dns <literal-IP>` options,
in resolver preference order. There is no compiled resolver-address fallback.
For example, deployment supplies its selected addresses as:

```sh
mcp-workspaced -daemon -mcp-listen 127.0.0.1:8080 \
  -image "$CANONICAL_IMAGE" -public-dns "$PUBLIC_DNS_PRIMARY" \
  -public-dns "$PUBLIC_DNS_SECONDARY"
```

The daemon passes these values through `workspace.Config.PublicDNS`. `Open`
validates, normalizes and copies the list before runtime initialization; the
Podman adapter keeps its own copy. Literal public IPs are required. Mapped IPv4
is normalized to IPv4; scoped addresses, hostnames, loopback, private, CGNAT,
link-local, ULA, documentation and reserved/special-use destinations are rejected.
Configuration is deployment-owned, never a tool argument, metadata field,
workspace environment or Dockerfile option. Public address classification does
not replace deployment's denial of the host's own addresses.

Final canonical and custom containers each select an independent rootless pasta
namespace using this explicit profile:

```text
pasta:--no-map-gw,-t,none,-u,none,-T,none,-U,none
```

It disables automatic TCP/UDP forwarding in both directions and gateway mapping.
No ports are published, no shared workspace network is selected, and no engine
socket is passed. The build profile retains its separate exact-generation DHCP
hostname token solely for Buildah residue attribution. Neither pasta profile
is the destination enforcement boundary: root/operator-managed numeric-mcp-runner
UID OUTPUT policy must enforce the accepted IPv4/IPv6 public-egress policy.
The application installs no firewall rules and gains no host network privilege.

Both final image paths ignore image volumes, disable healthchecks, host proxy
injection and automatic exposed-port publication, and explicitly select private
PID/IPC/UTS/cgroup namespaces. Image commands cannot trigger Podman's automatic
systemd mount changes; image labels cannot select a seccomp profile. Ordinary
image commands, guest environment and writable root filesystems remain usable.
`--no-hosts` suppresses Podman's host-alias injection; guest `/etc/hosts` content
cannot create a host route or override OUTPUT policy. No host devices, bind
mounts, capabilities or security overrides can be supplied through tool input.

At initialization the service creates bounded empty hook/default-mount
configuration under `StateDir/runtime-profile` (private directories and a private
empty file). Nonempty, symlinked or non-private entries fail startup. Both final
paths explicitly select these locations, suppressing ambient OCI hooks and
subscription/default mounts. These locations are never mounted into guests.
Deployment must keep the remaining host Podman configuration trusted.

The same configured resolver list reaches build RUN and final-container `--dns`
arguments, with no inherited DNS search domains. Host-side image fetch is a
separate context: it uses deployment's `/etc/resolv.conf`, not these `--dns`
arguments. The existing host-resolver validation remains in the build path.
Deployment must arrange the correct resolver view for the service/user-manager
and Podman namespaces; #80 showed that an existing pause namespace can retain a
stale stub-resolver view. This source change does not provision that view.
Untrusted code may still query other public DNS/DoH endpoints. There is no DNS
exclusivity rule, DNS proxy or prohibited-destination exception.

These are source-level configuration and command-construction guarantees.
Deployment reproduction belongs to #94; actual pasta connectivity, OUTPUT denial,
resolver propagation and uncached fetch/package usability remain #95 evidence.
#96 owns clean reconstruction and ChatGPT/Tunnel E2E. No real-host PASS follows
from deterministic local tests.

## Dockerfile creation (P5)

`CreateWorkspace(ctx, identity, dockerfile)` accepts optional UTF-8 Dockerfile text
(at most 1 MiB). Omitted text retains canonical creation. Empty/invalid supplied
text fails instead of selecting the canonical image. From ABSENT, acceptance
journals one exact W+G candidate and returns `PROCESSING`. Repeated create while
BUILDING/FINALIZING observes that candidate and ignores replacement text. PRESENT
create remains idempotent. Exec/read/write reject candidates promptly as not ready.

The existing lifetime journal carries only build phase and immutable final image
ID. A cancellable worker performs build and final-container creation outside the
lifecycle lock. Admission, transitions, revocation, cleanup and publication remain
under that lock. Publication requires terminal subprocess/drains, confirmed exact
image/container ownership, current candidate authority and a durable journal write.
Autonomous progress does not refresh TTL. Restart converts unfinished candidates
to recovery-only intent without replay or publication.

Definite build/finalization failure releases backend permits and retains bounded
diagnostics. The next permitted create returns `build_failed` and attempts exact
cleanup; that call never accepts replacement text. Only a later create after
confirmed ABSENT can obtain fresh G. Destroy/eligible TTL revoke authority before
cancelling the worker. Uncertain termination or cleanup retains ownership and
backend accounting for trusted `PendingBackend`/`RecoverBackend` recovery. Existing
TTL handles abandoned failures; there is no failure-specific timer or scheduler.

BuildKit's Dockerfile parser supplies syntax; service code classifies external
`FROM`, `COPY --from` and RUN mount image sources. Same-Dockerfile named/numeric
stages are allowed. Normal short names, registry names, tags and digests are
accepted. Missing external images are pulled with an empty service-owned authfile,
then inspected before consumption; build uses `--pull=never`. Service-generation
image names and images carrying service labels are prohibited as external input.
Inherited ONBUILD, dynamic image expressions/RUN mount flags and FROM platform overrides are
unsupported because pre-submission inspection cannot safely classify them.
Dockerfile LABEL keys cannot override the reserved service namespace or use
dynamic keys. Caller network/security/device overrides and secret/SSH RUN mounts
are rejected. Local-context COPY may fail normally: callers supply only text.

The rootless adapter requires the existing mcp-runner systemd user manager,
cgroup v2 controllers, `/usr/bin/systemctl`, `/usr/bin/systemd-run`, Buildah and
pasta. It creates an exact transient slice using `TasksMax=Policy.PIDsMax` (512
by default), memory/swap/CPU-weight properties, then validates the expected user
cgroup path and kernel limits before submission. Fetch/build workers run below
that slice with NOFILE 1024; `podman build --cgroup-parent` selects that exact
parent for OCI RUN scopes. The daemon, pipe drains and stop/recovery controls stay
outside it. The implementation expects the conventional
`/user.slice/user-UID.slice/user@UID.service/` user-manager hierarchy and fails
closed if it cannot attest it. There is no unbounded or privileged fallback.

Build RUN explicitly uses pasta, no gateway mapping and no port forwarding,
with the trusted `Config.PublicDNS` resolver list. Host-side pulls require deployment
`/etc/resolv.conf` to contain public-routable resolvers. The existing root-managed
numeric-mcp-runner-UID OUTPUT boundary must already enforce prohibited destinations
for both paths. This feature changes no firewall or deployment configuration.
Untrusted code may use other public DNS/DoH endpoints; resolver exclusivity is not
required. Final containers retain the trusted runtime profile, disable image
volumes/healthchecks and use empty service-owned hook configuration.

Engine GraphRoot must be on the configured Data filesystem. Context, TMPDIR,
authfile, image-ID file and logs live under `DataDir/builds/mcpws-v1-W-G`.
Both output streams continuously drain bounded OS pipes to a combined 1 MiB
diagnostic prefix, discarding excess even after write failure. Drain/write failure
prevents publication. Final and intermediate images carry exact ownership labels;
deletion uses independently inspected immutable IDs and `--no-prune`.
Buildah external-storage residue is attributed by its saved exact pasta network
NamespaceOption and immutable ContainerID. Ambiguous evidence fails closed;
NamespaceOptions is this adapter's attribution choice, not additional architecture
authority. Cleanup stops/removes the exact parent, removes exact engine objects,
independently rereads absence and removes exact Data state. Linux openat2 prevents
symlink/mount traversal; pinned, service-owned directories can gain only the owner
permissions needed to remove restrictive 0100/0000 state. This requires kernel
support for openat2 and fchmodat2/AT_EMPTY_PATH. No global prune or chmod is used.

Local tests cover lifecycle transitions, accounting, command construction, source
policy, streaming bounds and exact cleanup decisions. Separate delivered-HEAD
canonical-host verification must establish actual pasta/DNS/OUTPUT enforcement,
Buildah NamespaceOptions and residue removal, RUN cgroup ancestry and effective
512-task ceiling, saturation/control survival, restrictive Data cleanup, large
output RSS/storage bounds and cancellation/TTL/restart/failure recovery. Local
checks do not establish runtime PASS.

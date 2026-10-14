# Dockerfile-backed candidates (P5a)

`Service.CreateDockerfile(ctx, identity, dockerfile)` is the trusted host-side
creation entry point. Its only caller build input is a UTF-8 Dockerfile of at
most 1 MiB. It accepts no build context path, host/workspace file, bind, engine
socket, device, capability, secret, SSH material or runtime option. This slice
adds no public MCP create/build/job/status tool and no product transport.

The result contains `PROCESSING`, `PRESENT` or `FAILED`; the exact generation is
available to trusted Go ingress only and is excluded from JSON. Polling an
accepted BUILDING/FINALIZING candidate returns that same generation and ignores
new contents. PRESENT also ignores a new Dockerfile. FAILED retains its generation
until exact cleanup; polling never rebuilds. The original canonical `Create`
continues to work and returns the existing exact candidate with `ErrNotReady`
when it is not PRESENT. Exec/read/write cannot inject into a candidate and return
bounded not-ready errors without creating or waiting for publication.

## Parsing and image sources

The local compiler parses instructions and continuations, resolves image-position
variables and same-file stage names/indices, validates references, and emits
normalized literal image positions. It does not delegate unchecked source to an
alternate frontend and does not use a raw-regex scan as policy enforcement.

External FROM/COPY sources require an explicit distribution registry host and a
tag or SHA-256 digest. Domain hosts, host:port and bracketed IPv6 are supported.
Short names, default-tag search, transport/URL/archive/storage inputs, localhost,
loopback and legacy numeric loopback spellings are rejected. `scratch` is a FROM
special case. A COPY name is internal only when an already declared same-file
stage resolves it. Unknown or forward stage names undergo external source policy.
The service reserves the `mcpws-generation-v1` repository component everywhere.
Known service image aliases, manifest/config digests and local image name history
are also denied, including owned candidates and old/failed/stale images in storage.

The supported grammar is intentionally narrower than all Dockerfile frontends:

- ARG defaults and simple `$NAME` / `${NAME}` substitutions must be fully known.
  FROM uses global ARG scope; COPY uses explicitly declared stage ARG/ENV scope.
  ENV supports one unquoted `NAME=value` assignment. Unknown values, automatic
  platform variables, shell modifiers and quoted image positions fail closed.
- FROM supports an image or earlier stage, with optional `AS name`. COPY supports
  `--from=`, `--chown=` and `--chmod=`. Stage aliases are unique and normalized.
- RUN shell/JSON forms and ordinary CMD/ENTRYPOINT/WORKDIR/USER/EXPOSE/VOLUME/
  STOPSIGNAL/SHELL/HEALTHCHECK instructions are passed through. RUN options,
  frontend/escape directives, heredocs, ONBUILD, ADD and caller LABEL are rejected.
  These restrictions prevent hidden image/context/mount inputs and ownership
  metadata changes. General frontend compatibility is outside this slice.

The adapter rechecks service image inventory before fetching. Each checked
external source is pulled once with verified TLS, inspected for service ownership
and inherited ONBUILD triggers, then substituted with its immutable local image
ID in the trusted build text. Inherited ONBUILD is rejected before executing the
build. Podman builds with `--pull=never`; source resolution cannot subsequently
fall back to an unchecked tag, local alias or short-name search.

This source policy is separate from destination enforcement. An explicit registry
reference does not grant a private/ULA/link-local/metadata/host/other-workspace
exception. Registry fetches and rootless build networking inherit the existing
root-managed host Netfilter OUTPUT policy selected by the numeric `mcp-runner`
UID, at actual destinations including DNS-resolved and redirected connections.
There is no DNS pre-resolution boundary, redirect parser, proxy, firewall helper
or extra privilege in this implementation. The adapter selects `--isolation=rootless`
and explicit `--network=pasta:...` for an independent rootless network namespace,
disables inherited proxy settings and drops capabilities. It explicitly disables
TCP/UDP forwarding in both directions (`-t/-u/-T/-U none`) and gateway-to-host
mapping (`--no-map-gw`). The service module clears inherited `pasta_options`,
including array-append settings, before these fixed arguments are applied.

## Trusted build defaults

`Config.BuildDNS` / `PodmanBuildConfig.DNS` accepts up to eight public IP literals
from service/deployment configuration. The daemon accepts the same setting with
`-build-dns=IP,IP`. No resolver address is built into production defaults. Empty
configuration leaves default resolution to the trusted runtime/deployment. Explicit
settings become separate `podman build --dns=IP` arguments; they affect build RUN
containers, not the host resolver used by image pulls. Loopback, private, link-local,
ULA, metadata and other non-public address classes are rejected. The inherited
UID OUTPUT policy still denies prohibited actual destinations, including service
host and other-workspace addresses; this setting creates no DNS exception.

Dockerfile ARG/ENV/RUN values cannot change these defaults or host configuration.
RUN code retains the existing public-Internet capability, including other public
DNS/DoH endpoints. Host-side resolver provisioning and the eventual systemd/Ansible
mechanism remain deployment responsibilities.

Embedded hosts configure `NewPodmanWithBuildConfig` with the service's exact
`DataDir`, resource policy and trusted settings. `Config.BuildahPath` / daemon
`-buildah` selects an absolute local Buildah executable (default `/usr/bin/buildah`)
under the same non-root account, environment and storage as Podman. This is a
storage inspection/removal tool, with no remote transport or additional privilege.
DataDir must not contain commas, NUL, CR or LF: commas delimit pasta options and
cannot safely represent part of the candidate metadata path in that grammar.

## Ownership, completion and cleanup

ABSENT creation allocates fresh G, acquires the shared generation/backend budgets,
and durably records BUILDING before creating Data state or submitting build work.
The Data path is `builds/mcpws-v1-<W>-<G>` beneath operator DataDir. Its `context`
directory contains only the compiled `Dockerfile`; bounded stdout/stderr (64 KiB
head/tail per stream) and operational temporary state are outside that context.
Outside `context`, the adapter creates a private `build-container.conf` module
that resets pasta options using `podman --module`. The fixed network argument
contains `--pid,<DataDir>/builds/mcpws-v1-<W>-<G>/pasta.pid`. Buildah saves this
argument in its network `NamespaceOptions` when initializing each working
container, before RUN. This also applies to external COPY source containers.
Attribution uses that saved setting, even if pasta has not started and no PID
file or final image exists. The PID value is never process/deletion authority.
Caller Dockerfile instructions cannot supply or change this namespace setting.
No arbitrary local files can become caller COPY inputs. Ordinary context COPY
instructions have only Dockerfile available, and typically fail when other files
are requested. Same-file stage COPY remains supported.

The service owns a bounded local worker, independently of polling request
cancellation. Trusted `BuildRuntime` methods use exact W/G and synchronous
read-only inventory or terminal backend evidence. Successful completion reenters
the lifecycle lock, revalidates exact candidate authority, journals FINALIZING,
and uses backend admission to create the exact container from
`localhost/mcpws-generation-v1/<W>:<G>`. Container creation runs outside the lock
so polling and cancellation can proceed. Publication reenters serialization,
confirms exact container ownership/image and rechecks authority and TTL before
journaling PRESENT. Failure cannot publish PRESENT. Completion does not refresh
idle or absolute TTL.

Destroy/TTL/recovery revoke publication before cancellation or permit recovery.
Deleting candidates retain generation ownership. Late success can clean up G but
cannot publish it or touch another generation. Definite backend evidence releases
only the original operation; an active local worker also retains cleanup ownership
until its return, even after trusted permit recovery. Shutdown closes admission,
cancels workers and retains singleton ownership until local workers exit.

Journal version 3 extends the existing exact-generation lifecycle intent with
phase/image metadata; valid older canonical journals are migrated. Restart fences
unfinished BUILDING/FINALIZING records as FAILED and preserves uncertain backend
accounting. It neither resumes builds nor adopts a leftover container as PRESENT. Orphan
service-owned images or Data build directories without exact journal intent fail
startup, including when the operator selects a different StateDir.
Trusted `PendingBackend` / `RecoverBackend` evidence is required to resolve
uncertain engine work before cleanup. Local CLI cancellation or an abnormal build
exit alone does not prove that Buildah work stopped.

Cleanup separates ordinary Podman objects from external storage objects using
bounded inventories. External objects use `buildah inspect --type=container` and
full immutable IDs; only one saved non-host network namespace with the complete
expected pasta argument and exact Data/W/G PID-file path establishes attribution.
Missing, duplicated or altered settings fail closed. Names, base-image IDs, inventory deltas and
image labels alone do not grant external deletion authority. `buildah rm -- ID`
removes only attributed G objects. Startup includes these objects in generation
reconciliation even when no custom image was committed.

Cleanup removes the exact container, exact owned custom/intermediate images and
owned external build containers, independently reads back storage/runtime absence,
and removes only
G's Data subtree before ABSENT. Image deletion uses immutable IDs and `--no-prune`;
there is no global/wildcard prune. Other generations and unrelated Data remain.
Missing/inconsistent ownership, unidentified external engine containers or dangling images, retained
objects, partial cleanup or uncertain completion keep intent/admission closed for
that generation instead of guessing ownership. Such uncertainty requires trusted
host recovery; cleanup cannot promise automatic recovery from an engine crash.

## Validation boundary

Local tests use fake builders/runtimes, stub commands, local executables and
network-denial decision errors. They cover reference parsing, variable/stage/COPY
resolution, inventory aliases, inherited triggers, pinned build inputs, repeated
polling, failures, cancellation/late container creation, TTL, recovery, restart,
cleanup ownership, shared admission and bounded logs/tools. These are local source
and adapter-contract evidence. Real host Podman builds/image fetches and effective
network/cgroup enforcement are the implementation-time verification responsibility
of Issue #72 against the delivered exact revision.

The adapter targets Podman 4.9.3 / Buildah 1.33.5's explicit pasta path and inspect
`Type`/`ContainerID`/`NamespaceOptions` shape. The former `default_mounts_file`
module setting is not TOML-configurable in the canonical containers/common
version and is no longer used for attribution. Existing residue without the new
saved namespace setting remains ambiguous and cannot be adopted. Real support,+cancellation cleanup and effective RUN networking/resources still require #72
verification against the revised exact source revision.

The propagation contract follows the versioned official sources:
[Podman build options](https://github.com/containers/podman/blob/v4.9.3/cmd/podman/common/build.go),
[Buildah stage initialization](https://github.com/containers/buildah/blob/v1.33.5/imagebuildah/stage_executor.go),
[builder state persistence](https://github.com/containers/buildah/blob/v1.33.5/new.go),
[Buildah inspect projection](https://github.com/containers/buildah/blob/v1.33.5/buildah.go),
[pasta setup](https://github.com/containers/common/blob/v0.57.4/libnetwork/pasta/pasta.go),
[configuration fields](https://github.com/containers/common/blob/v0.57.4/pkg/config/config.go)
and [configuration array overrides](https://github.com/containers/common/blob/v0.57.4/internal/attributedstring/slice.go).

The primary build-log sink finding remains unresolved: output is currently
captured in memory and the bounded retained files are written after Build returns.
Pre-opened ordinary Data file descriptors plus a strict growth bound need an
agreed enforcement mechanism. A process-wide file-size limit also constrains
engine/build file writes; a monitoring cutoff permits transient overshoot and
cannot bound writes by uncertain surviving workers. Neither behavior has been
introduced as an implicit change to the build contract.

The CLI options and image inspection shape follow the official
[Podman build documentation](https://docs.podman.io/en/latest/markdown/podman-build.1.html)
and [image inspect documentation](https://docs.podman.io/en/latest/markdown/podman-image-inspect.1.html).

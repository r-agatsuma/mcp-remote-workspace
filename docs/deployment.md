# Canonical v0 deployment — source contract (#94)

Authority: frozen #20, bounded #94, and landed application interfaces at
`f1f5f7567e5cc955dbff1f96d54897b9d2565926`. This tree does not change #91/#92/#93.
No dirty OCI host was consulted or modified to produce it.

**#94 establishes deployment source and static/component checks, not a working
host.** Ansible convergence/idempotency, startup, resolver namespaces, native
storage interoperability, cgroup ancestry, firewall effectiveness, reboot and
pressure recovery are #95 evidence. Clean reconstruction, real OpenAI identity,
Tunnel READY and ChatGPT E2E remain #96 (with composed Tunnel checks in #95).
`active` is not application readiness. No independent source review is claimed.

## Tree and inputs

`ansible/playbooks/deploy.yml` imports the single `mcp` role, split into explicit
preflight, Data, accounts, source build, network, services and Tunnel tasks.
`bootstrap-data.yml` is a **separate destructive entrypoint**, never imported by
deploy. `ansible/tests/test_deployment.py` renders synthetic fixtures only.
There is no default inventory, automatic production CD or self-hosted runner.

| Kind | Inputs / ownership |
| --- | --- |
| Fixed v0 architecture/mechanisms | Ubuntu 24.04 Minimal AArch64, rootless Podman + standalone Buildah + crun + pasta, existing user manager/cgroup v2, ext4 whole device and 10% reservation, root-managed iptables-nft/ip6tables-nft/netfilter-persistent, separated `mcp-runner`/`mcp-tunnel` |
| Fixed delivery values | `go1.26.9`, Tunnel `v0.0.15`, `/srv/mcp-podman`, `http://127.0.0.1:8080/mcp` |
| Human-selected deployment defaults | Explicit `mcp_public_dns: [8.8.8.8, 8.8.4.4]`; resource/TTL/storage values in role defaults (same as landed ReferencePolicy) |
| Required host inputs | Exact source commit and controller repository path; immutable image; whole-device by-id path, expected UUID; stable distinct UID/GIDs and nonoverlapping subordinate range; reviewed firewall baseline files and explicit filter ownership acknowledgment; all external/public NAT self IPs; existing Tunnel ID |
| Secret input | `mcp_tunnel_environment_file`: protected controller file containing only `CONTROL_PLANE_API_KEY=<runtime key>`; runtime principal needs Tunnels **Read + Use**, not management authority |
| Bootstrap-only inputs | Exact device serial and size, UUID, and `mcp_bootstrap_confirm` matching `INITIALIZE <UUID> ON <by-id device>` |

Copy `ansible/inventory/example` outside the repository. All required placeholders
must be intentionally replaced. Do not put credentials or live infrastructure IDs
in tracked examples. Choose unused stable service UID/GIDs (>=1000), a dedicated
65536-ID subordinate range, and retain these values across all upgrades. Existing
named accounts with different IDs or overlapping subids fail instead of migrating
runtime ownership. No sudo or host network capabilities are granted.

The 2-OCPU/12-GiB reference values are not universal resource guarantees:
4 GiB/generation, two generations, zero swap, CPU quota -1, weight 100, 512 tasks,
513 migration observation, NOFILE 1024, four backend operations, idle TTL 30m,
absolute TTL 24h, 1 GiB available-byte and 1024 free-inode thresholds, tool timeout
30s. Retained completed-operation metadata uses the landed default of 16; build
logs retain 512 KiB per stream and returned diagnostics 64 KiB per stream.

## Controller workflow (later authorized execution only)

These commands are instructions for #95/#96, **not commands executed by #94**:

```sh
python3 -m venv .venv-deploy
.venv-deploy/bin/pip install -r ansible/requirements-dev.txt
cd ansible
# Before either apply: confirm host, provider ingress, device and inputs manually.
ansible-playbook -i /protected/inventory/hosts.yml playbooks/deploy.yml --syntax-check
ansible-playbook -i /protected/inventory/hosts.yml playbooks/deploy.yml --diff
```

Controller prerequisites: Python 3.11+, Git, the exact commit already present in
the local repository, SSH host-key trust and an independently authorized operator
connection/become path. No target GitHub credential is installed. Package and
artifact downloads need ordinary trusted host HTTPS; runtime image acquisition
uses the separately enforced runner path. Never use `--start-at-task` or partial
tags to bypass safety gates. Check mode/syntax are not convergence proof.

The target must use signed supported Ubuntu noble package sources, not PPAs or
hand-installed runtime replacements. The role installs Podman, **`/usr/bin/buildah`**,
crun, passt (`/usr/bin/pasta`), uidmap, dbus-user-session, systemd, e2fsprogs,
iptables + persistence plugins, CA certificates, util-linux, Python, ACL tooling,
unzip and native build/linkage inspection tools. Reference evidence is Podman
4.9.3, Buildah 1.33.7 and crun 1.14.1; Ubuntu package patch revisions are not
fabricated pins. `/etc/mcp-deployment/packages.txt` records the exact resolved
versions. #95 must compare those versions and kernel/feature availability with
accepted evidence; an incompatible mechanism is a STOP, not permission to
substitute rootful runtime, fuse overlay, another network backend or broader
sysctls. Ubuntu's supported rootless/AppArmor policies must work as shipped;
this role does not globally disable AppArmor or user-namespace restrictions.

### Explicit first-time Data bootstrap

Normal deploy only inspects existing identity/type/reservation and mounts it.
It never formats, wipes signatures, repartitions or resets the pool.

For a newly attached, genuinely blank dedicated whole device, inspect by-id
identity, serial, byte size, mount state, holders, partition/signature absence,
and confirm it is not Boot. Arrange exclusive operator maintenance (no concurrent
mount/partition changes). Supply the exact destructive acknowledgment, then run
`playbooks/bootstrap-data.yml` separately. Its script repeats those checks,
requires an exclusive block-device open, and invokes one `mkfs.ext4 -F -m 10 -U`.
One `-F` permits the deliberately selected whole device, not a mounted-device
force (`-FF` is never used). An initialized device fails this entrypoint; do not
blindly retry a partially failed format. No automatic whole-pool reset exists.

First deploy requires Data **unmounted**, so it can establish the root-owned
mode-000 Boot mountpoint guard and record that fact. It refuses to adopt an
already-mounted pool with unknown guard history; it does not unmount it for you.
Later normal reruns verify the filesystem before writing anything beneath it.
The mount unit uses `/dev/disk/by-uuid/<expected>` and ext4 with
`nodev,nosuid,noatime`. The unprivileged `mcp-data-check` verifies actual mount,
block-device identity, filesystem, options, Boot/Data separation and required
Data paths before product startup and recovery. No guard has privilege to mount,
repair or delete anything. Mount absence/failure never authorizes Boot fallback.

Layout:

```text
/var/lib/mcp-workspaced/                 private bounded lifecycle journal/profile
/srv/mcp-podman/                        dedicated mounted filesystem, root 0711
  runner/                              root-controlled parent
    home/                              account home; fixed singleton beneath .local/state
    config/containers/                 root-owned shared storage/engine configuration
    storage/                           graphroot, images/metadata/writable layers
    data/builds/                       exact-generation contexts/temp/bounded logs
    cache/ tmp/ events/                 runtime-induced persistent sinks
  journal/                             only product journald namespace, bounded
/run/user/<UID>/                        volatile runroot, libpod temp and user bus
/etc/mcp-deployment/                    operator-owned trusted config/provenance
/etc/mcp-tunnel/                        root-controlled, tunnel-group traversal only
```

The mounted root is traversable but service directories are private. All ordinary
persistent runtime growth is on Data. Podman and standalone Buildah consume the
same XDG config, overlay graphroot and volatile runroot. Native overlay has no
`mount_program` fallback. Startup's unprivileged `mcp-store-check` checks both
CLI-reported stores; real interoperability remains #95. Caller image metadata
cannot inject mounts/hooks/volumes because the landed application explicitly
suppresses them. No engine socket or additional volume is exposed.

The product's existing journald namespace is bound to Data, with a 32 MiB
archive-use target, 8 MiB individual file bound, 8 MiB volatile fallback bound,
and rate limiting. Active journal files can add to the archive-use target; it is
not represented as an exact 32-MiB total hard quota. Only product logs use this
namespace; ordinary host/system journals stay where the OS owns them. Build and
container logs have their separate landed/engine bounds. Runtime growth does not
inherit an unbounded Boot log sink.

The 10% root reserve is recovery capacity, **not** a per-workspace quota or inode
reservation. At exact-zero normal bytes Podman destruction may fail. Escalate for
separately authorized operator capacity restoration; never guess arbitrary files
to remove or reproduce the historical fill-file experiment as recovery.

## Exact source, toolchain and artifact provenance

The controller verifies the full 40-hex commit and uses `git archive <commit>`;
uncommitted working files and branches are not deployment authority. The target
retains the source archive, source tree, go.sum and modules under
`/usr/local/src/mcp-workspaced/<commit>`. Builds use `GOTOOLCHAIN=go1.26.9`,
`-mod=readonly`, checksum-database validation and `go mod verify`; native CGO is
explicitly enabled. `file`, `readelf` and `ldd` inspect the **actual artifact** and
missing dynamic dependencies fail the build. No static-portability claim is made.

The binary is atomically installed root:root 0755 at
`/usr/local/libexec/mcp-workspaced`. `/etc/mcp-deployment/provenance.json` binds
source revision, source archive SHA-256, Go archive/version, go.sum SHA-256,
installed binary SHA-256, module/build information and linkage. The installed
hash is checked against that record. This is inspectable provenance, not a promise
of byte-identical builds across arbitrary C compiler/package revisions.

Official artifact provenance consulted for this delivery:

* Go archive: `https://go.dev/dl/go1.26.9.linux-arm64.tar.gz`, SHA-256
  `4a97373d49fcacdcf3694fea368a500b00ee3e963974f3e7514132717632f052`, from
  `https://go.dev/dl/?mode=json&include=all`.
* Tunnel release: [official v0.0.15](https://github.com/openai/tunnel-client/releases/tag/v0.0.15),
  full-client Linux arm64 ZIP SHA-256
  `c51bfd883fc22e3445494a03c0179875176564bde470661b308fd83af5d01abb`, checked
  against official release metadata/SHA256SUMS and downloaded bytes. Release tag
  resolves to `a390c168ff1b2d14e73a95991c186c6aba3ff5a0`; the binary reports
  `0.0.15+a390c168ff1b2d14e73a95991c186c6aba3ff5a0`.
  A signed upstream provenance bundle exists. Author-host `gh` lacks
  `attestation`, so Sigstore verification was **SKIPPED — tooling unavailable**;
  no signature-verification claim is made. HTTPS official checksum provenance is
  the input used here. #95 may additionally verify the published attestation.

## Manager, resolver and runtime topology

The selected bounded implementation is a root-defined **system service with
`User=mcp-runner`**, connected explicitly to `/run/user/<UID>/bus`. Linger keeps
that existing unprivileged manager available without a login. This is the
smallest unattended mechanism used here, not a universal architecture invariant.
The manager delegates cpu/memory/pids; source creates exact finite transient
build slices with `TasksMax=Policy.PIDsMax` (512), not the 513 migration allowance.
Daemon/drains/recovery remain outside those slices. There is no privileged cgroup
helper, alternate cgroup daemon or whole-user kill mechanism.

Resolver choice: a deployment-owned `/etc/mcp-deployment/resolv.conf` is bound
read-only over `/etc/resolv.conf` in **both** the root-launched product/image unit
and the root-launched `user@<UID>.service`. Descendants of the manager, including
independently launched transient pull/build workers, inherit the manager's
resolver namespace. This deliberately does not assume inheritance from the
product alone, nor alter the host's resolver for other users. Podman pause
namespaces must originate under this view; a live manager/resolver change is
refused to avoid silently adopting stale namespace state. Application
`-public-dns` separately supplies both Dockerfile RUN and final workspace DNS.

The configured addresses are **8.8.8.8 and 8.8.4.4**, not compiled universal
constants. They remain explicit trusted inventory values for future intentional
Human changes. No stub/link-local DNS exception, DNS daemon, or public DNS/DoH
exclusivity rule exists. #95 must verify all three paths with both rendered
addresses, including uncached external pulls and namespaces after reboot.

For an intentional resolver/manager configuration change: stop product/Tunnel,
resolve lifecycle work as appropriate, disable runner linger, and perform an
independently authorized reboot before applying while the manager is inactive.
Then deploy reinstalls/enables linger and starts the new namespace. Do not stop
or kill the whole user scope as a shortcut, reuse stale pause namespaces, or
silently apply a top-level-only resolver change. No restart-manager handler exists.

Conservative runner units deliberately avoid guessed sandbox directives that
would break setuid newuidmap/newgidmap, rootless mounts, procfs, cgroup inspection
or the user bus. Trusted host-side runner code is inside #20's trust boundary.
The Tunnel has a separate modest sandbox and no runner runtime ownership.

## Firewall and sysctl ownership

Only iptables-nft/ip6tables-nft and netfilter-persistent are authorities. UFW,
firewalld or enabled native nftables.service cause refusal, not silent disabling.
The dedicated-host operator supplies reviewed **filter-only** save snapshots for
both families, preserving INPUT/FORWARD/operator-access and unrelated OUTPUT
semantics. Initial live tables must exactly match these baselines (counters and
chain serialization order excluded). Later live tables must match the previously
installed deployment-owned tables; unreviewed drift fails rather than resets.
The product owns the complete relevant filter tables explicitly, not arbitrary
other tables. Both candidates are syntax-tested before installation; changed
policy stops product admission, then loads through netfilter-persistent. An
unchanged rerun does not reload/flush rules. Boot orders manager/product behind
that authority. Operators must not run a competing save/restore manager.

The numeric runner UID jump precedes baseline OUTPUT acceptance. Rules allow
only **reply-direction** ESTABLISHED/RELATED handling for trusted responses
(including MCP responses to the separately owned Tunnel), then deny LOCAL/self,
all supplied external NAT/public self IPs, private/link-local/ULA/metadata and
accepted non-public/special-use classes. An unrestricted established-connection
exception is intentionally absent. Public allocations within protocol-assignment
parents remain routable; the baseline must permit ordinary public egress.

`roles/mcp/vars/main.yml` spells out #20/#56/#55 classes using the
[IANA IPv4 registry](https://www.iana.org/assignments/iana-ipv4-special-registry/)
and [IPv6 registry](https://www.iana.org/assignments/iana-ipv6-special-registry/)
(the primary references also used in #55). This is a reviewable fixed list, not a
runtime-updated registry policy. It includes shared, this-network,
protocol-assignment, documentation, benchmark, deprecated relay, discard/dummy,
translation-local, site-local, multicast and reserved classes. Public protocol
allocations are not DNS exceptions. `--dst-type LOCAL` covers assigned self
addresses; inventory must additionally enumerate public NAT self addresses not
assigned to guest interfaces. #95 must inspect both rendered files and replay the
accepted matrix, not infer denial from these templates.

`/etc/sysctl.d/90-mcp-runner.conf` supplies exactly
`net.ipv4.ping_group_range = <primary-GID> <primary-GID>`; competing sources fail.
No host CAP_NET_ADMIN or general raw-socket capability is added. Landed pasta
profiles disable all automatic forwarding and gateway mapping. Provider ingress
must retain the accepted no-new-unsolicited-IPv6-TCP policy. This is the accepted
layered inbound guarantee, not provider-independent guest-only denial.

## Tunnel profile, credentials and readiness

The exact v0.0.15 binary's `--version` and `run --help`, plus its
[pinned configuration reference](https://github.com/openai/tunnel-client/blob/v0.0.15/docs/configuration.md),
confirm the rendered interface:

```text
tunnel-client run --profile-file /etc/mcp-tunnel/profile.yml
```

The deterministic profile binds `main` to `http://127.0.0.1:8080/mcp` and references
`env:CONTROL_PLANE_API_KEY`. Systemd reads `/etc/mcp-tunnel/runtime.env` (0600,
mcp-tunnel owner, inaccessible parent to runner); the key is never argv, inventory
content, a template value, or Ansible diff/log output. Secret-copy tasks have
`no_log: true`, `diff: false`, and `force: false`. Routine deploy preserves the
existing credential, even if the controller source differs. Rotation is a
separately authorized operator action, not a rerun side effect. Runtime input
must contain only the required runtime key, not admin keys or debug settings.
No secret was read, staged or rotated during #94.

No probe, embedded stub, management credential, Tunnel recreation or Cloudflared
companion is run. Health binds ephemeral loopback to avoid conflicting with 8080;
its resolved URL is private under `/run/mcp-tunnel/health-url`. Unsafe raw HTTP
logging is disabled. The client supports a 60s listener startup wait; upstream
explicitly says expiry resumes polling but leaves readiness non-ready. This is
not a readiness guarantee or replay mechanism. The source reconciles before
opening MCP, and there is no invented sd_notify. Both units use bounded stop,
10s restart delay and three starts per five minutes. A failure needs inspection,
not endless retry or a manufactured READY assertion.

## Operations and exclusive recovery

Run only with separately authorized target access. Normal control:

```sh
sudo systemctl start mcp-workspaced.service mcp-tunnel.service
sudo systemctl stop mcp-tunnel.service mcp-workspaced.service
sudo systemctl restart mcp-workspaced.service
```

Stop Tunnel and daemon before recovery. The root-owned `mcp-launch` is only an
unprivileged configuration adapter: it executes the **existing binary/CLI**, same
journal/image/DNS/policy and singleton, without a listener. Run it through the
existing user manager to preserve resolver view and runtime environment:

```sh
# UID must be the resolved inventory UID; do not guess it.
RUNNER_UID=$(id -u mcp-runner)
# Define a local operator convenience function, not an installed privileged helper.
recover() {
  sudo -u mcp-runner env XDG_RUNTIME_DIR="/run/user/$RUNNER_UID" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$RUNNER_UID/bus" \
    systemd-run --user --wait --pipe --collect --unit=mcp-operator-recovery \
    /usr/local/libexec/mcp-launch "$@"
}
recover -recover inspect
recover -recover backend -recovery-selection "$BACKEND_SELECTION"
recover -recover storage
recover -recover cleanup -recovery-selection "$GENERATION_SELECTION"
```

Use one command at a time. Select `operations[].selection` or
`generations[].selection` from a **fresh** inspection, never a guessed W/G/token.
These commands run outside finite build slices. Their two-minute product deadline
and singleton remain authoritative. No lock is removed or bypassed.

* Backend uncertainty: inspection must show a recorded prior kernel BootID.
  Current-boot or missing historical BootID is refused. If an independently
  authorized reboot is required, stop services first, preserve journal/Data,
  reboot, stop admission again, inspect, and recover each exact prior-boot
  operation. Reboot proves terminality, not absence or operation success.
* Generation cleanup is a separate exact selection **after** backend terminality
  and safe storage. Successful backend recovery does not imply cleanup completed.
* Storage latch: restore both byte/inode safety margins by separately authorized
  operator action, then `-recover storage`. Restart or a safe sample alone does
  not clear a persisted latch. Exact-zero requires operator escalation, not an
  automatic generic deletion algorithm. Inspection is not deletion authority.
* Missing/wrong Data mount: leave admission stopped, inspect device identity and
  the mount unit; restore only the intended mount. Never chmod the Boot guard to
  make startup pass or create a replacement journal to adopt old objects.
* After recovery, inspect again, then start product and Tunnel. Verify actual MCP
  admission and Tunnel readiness independently; never equate command exit or
  unit `active` with a complete E2E result.

See [the landed recovery contract](recovery.md) for exact terminal-evidence and
pressure-scope rules. **Prohibited normal operations:** journal hand editing,
blind rm/chmod, global Podman/Buildah prune, whole-user cgroup kill, manual backend
“mark complete”, arbitrary workspace-file deletion, and secret printing. A
Human-authorized emergency whole-pool discard would require a separate operation
and exact lifecycle/absence resolution; this tree does not implement it.

### Upgrade / reboot / diagnostics

Before upgrading, stop admission and inspect/recover with the **old** binary and
configuration where needed. Intentionally select a new full source commit in
inventory, review the diff and reapply. The exact new artifact/config change is
visible in Ansible results; bounded restart reconciles durable state. Journal,
Data and Tunnel credential are preserved. No format, global prune, identity
change, or deletion of recovery state occurs. Changing canonical image with a
nonempty journal is unsupported by landed source; resolve old intent using the
old configuration first. Policy changes likewise require handling existing
resource-bearing generations under their original policy, not merely changing
flags and assuming old cgroups migrated. No automatic production CD is present.

For ordinary reboot, stop admission, collect recovery inspection, preserve state,
reboot under operator authorization, and verify mounted Data, persistent firewall,
user manager/bus, source provenance and readiness. Boot never releases pending
permits automatically; use prior-boot recovery and separate exact cleanup.

Collect only bounded operator evidence: unit status (without environments),
`/etc/mcp-deployment/provenance.json`, `inputs.json`, `packages.txt`, protected
firewall rules and recovery inspection. Product journal files are under
`/srv/mcp-podman/journal/<machine-id>.mcp`; use root
`journalctl --directory=<that-exact-directory> --lines=100 --no-pager`.
Root's ordinary host journal records unit exit/start failures separately.
Treat opaque W/G, Tunnel ID, addresses and build diagnostics as sensitive; redact
before publishing. Never collect `/proc/*/environ`, environment dumps, the secret
file, raw HTTP logs, raw OpenAI identity or arbitrary workspace output.

## Source validation and #95 handoff

From repository root, using the pinned development requirements:

```sh
.venv-deploy/bin/python ansible/tests/test_deployment.py
cd ansible
../.venv-deploy/bin/ansible-playbook -i inventory/example/hosts.yml playbooks/deploy.yml --syntax-check
../.venv-deploy/bin/ansible-playbook -i inventory/example/hosts.yml playbooks/bootstrap-data.yml --syntax-check
../.venv-deploy/bin/ansible-lint --offline
cd ..
GOTOOLCHAIN=go1.26.9 go test ./...
GOTOOLCHAIN=go1.26.9 go test -race ./...
GOTOOLCHAIN=go1.26.9 go vet ./...
GOTOOLCHAIN=go1.26.9 go build ./...
GOTOOLCHAIN=go1.26.9 go mod verify
git diff --check
```

Tests cover the 23 requested source assertions, YAML/Python parsing, negative
firewall inputs, numeric UID/GID direction, endpoint/secret separation, resolver
placement and launch-shell syntax. Synthetic unit verification uses `systemd-analyze`
when available; fixture executables/dependencies are **not runtime tests**.

#95 must retain and report:

1. Exact PR head, `mcp_source_revision`, installed binary SHA-256 and provenance
   JSON (do not substitute a mutable branch name).
2. `inputs.json` (all resolved trusted non-secret inputs), package versions,
   actual kernel/systemd/Podman/Buildah/crun/pasta features and target OS/arch.
3. Immutable canonical image reference and actual rootless image/store evidence.
4. Data device/UUID/options/reservation, Boot guard, all growth sinks, numeric
   accounts/subids and actual user manager/cgroup/bus/resolver topology.
5. Rendered both-family firewall files, exact primary-GID sysctl, self-address
   coverage, DNS `8.8.8.8, 8.8.4.4`, endpoint and all resource/TTL/storage policy.
6. Tunnel version/checksum/provenance, profile and credential **name/path only**:
   `CONTROL_PLANE_API_KEY`, controller `mcp_tunnel_environment_file`, target
   `/etc/mcp-tunnel/runtime.env`. No values in evidence.
7. Entrypoint `ansible/playbooks/deploy.yml`, protected inventory and separately
   acknowledged bootstrap if needed, plus this recovery runbook.
8. First apply and unchanged second-apply results; real startup, uncached
   host-fetch/RUN/final DNS, firewall matrix, native storage interoperability,
   cgroup ancestry/build PID ceiling, exact pressure kill, storage and reboot
   recovery, resource/log bounds and Tunnel readiness. #96 must still establish
   clean reconstruction and real OpenAI/ChatGPT identity/E2E.

No runtime PASS follows from a rendered configuration, syntax check or this list.

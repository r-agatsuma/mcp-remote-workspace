# S3 deployment source (S3A–S3F)

Start with the [operator runbook](../docs/operations.md) for supported procedures,
recovery, secret staging and evidence boundaries. Run `./scripts/verify-source`
from the repository root for the consolidated local source/static checks.

This tree targets Ubuntu 24.04 on arm64. It creates the unprivileged
`mcp-runner` and `mcp-tunnel` system accounts, then installs the application
from an explicitly selected repository commit. S3B adds the verified Data mount
and rootless runtime placement described below. S3C provisions the unattended
user manager and checks its cgroup authority. S3D adds the root-owned persistent
destination policy and direct trusted resolver view. S3E adds the production
services, shared trusted recovery configuration and pinned Tunnel installation.

Before a deployment, copy `inventory/hosts.example.yml` and replace its
documentation-only address and operator account. Set `mcp_source_revision` in
`group_vars/all.yml` to the exact 40-character commit to deploy. The example
value intentionally fails the revision check. The repository URL, source path,
toolchain, and account names are visible in the variables and tasks; do not set
the revision to a branch name or tag.

Run from this directory with an installed Ansible controller:

```sh
ansible-playbook --syntax-check site.yml
ansible-playbook --list-tasks site.yml
ansible-inventory --list
ansible-playbook -i localhost, tests/revision_contract.yml
```

These commands validate and inspect source-side Ansible configuration without
connecting to a deployment host. The local revision contract playbook exercises
the production identity and source-path assertions using synthetic facts.
Ansible check mode is a remote execution mode: it still connects to the selected
hosts and runs supported tasks in check mode. It is not a controller-only
source/static validation step and is not evidence of real-host convergence.

The role installs Ubuntu's `git`, `golang-go`, and `binutils` packages. The Go
package is only the bootstrap for the selected toolchain; `binutils` supplies
the binary linkage inspection tool.
Every Go operation sets `GOTOOLCHAIN=go1.26.9`, which asks the Go command to
select/download that exact toolchain. It verifies module checksums, records the
toolchain version and embedded build metadata, and builds from the checked-out
commit. The installed executable is `/usr/local/bin/mcp-workspaced`, owned by
`root:root` with mode `0755`. Before installation, the role requires
`/usr/local/bin` itself to be root-owned and not group/world writable.

After account creation, `mcp_runner_uid` and `mcp_runner_primary_gid` are
derived only after full passwd/group records pass the UID/GID separation checks.
`mcp_runner_account` and `mcp_tunnel_account` expose the account names. Before
any mutation, the role rejects account-name overrides, root identities, shared
UID/GID values, non-private primary groups, and supplementary memberships on
pre-existing service accounts. It never reassigns IDs to repair existing
accounts. Later roles can consume the verified facts directly; S3A does not
assign fixed numeric IDs. The account records use `/usr/sbin/nologin`, have
locked passwords, and do not create home directories. S3B selects the runner passwd home on Data
through `mcp_runner_home`; the S3A task defaults to `/nonexistent` when that input
is absent. UID/GID fact interfaces are unchanged.

Before invoking Git and again before building, the role requires the source
checkout path and its `/usr/local/src` parent chain to be root-owned and not
group/world writable. It checks every checkout entry and tests effective
writability as both service users, including ACL effects. Unsafe pre-existing
paths fail closed; S3A does not chmod/chown them into compliance. It creates
`/usr/local/src` as `root:root` mode `0755` only if that parent is absent.

The role records binary build metadata with `go version -m` and program/dynamic
headers with `readelf`; it makes no static linkage or portability claim. Source
checks do not prove host convergence or runtime behavior.


## S3B Data and rootless runtime contract

`site.yml` runs S3A, S3B, S3C, S3D, then S3E. It never imports `bootstrap-data.yml`, the
`data_bootstrap` role or `mcp_data_bootstrap` module. Ordinary convergence
contains no partition/format/signature-erasure/tune/repair action. The shared
inspector invokes `wipefs --no-act --json` only to read signatures. Missing,
wrong, ambiguous or exhausted Data is an operator condition; no automatic
repair, global prune, chmod recovery or replacement filesystem is attempted.

The reference is a dedicated whole disk, ext4, at `/srv/mcp-podman`. The expected
size is an explicit byte count, not an exact workspace entitlement. Both entry
paths require trusted per-host inputs in place of the failing example values:

| Input | Meaning |
| --- | --- |
| `mcp_data_device_id` | Whole-device `/dev/disk/by-id/...` symlink; no partition suffix or volatile kernel name |
| `mcp_data_expected_serial` | Independently supplied expected udev `ID_SERIAL` for that volume |
| `mcp_data_expected_bytes` | Exact expected whole-device size in bytes |
| `mcp_data_expected_sector_bytes` | Expected logical sector size, 512 or 4096 |
| `mcp_data_uuid` | ext4 UUID reported after authorized bootstrap; ordinary convergence only |
| `mcp_runner_subuid_start`, `mcp_runner_subgid_start` | Explicit non-overlapping host allocations, outside real host IDs |
| `mcp_runner_subid_count` | At least 65536; default 65536 |

No real device identity, host address, fixed universal service UID/GID or secret
is committed. Unexpected pre-existing subordinate-ID allocations fail rather
than being reassigned. These are deployment inputs, not product request fields.

### Separate first provision

The destructive entry is **only** `bootstrap-data.yml`. It is disabled when
`mcp_data_bootstrap_enabled` is absent or false; the enable is intentionally not
stored in example inventory. An operator must explicitly supply a boolean true
for that run, together with the trusted identity/serial/size/sector inputs.
Bootstrap refuses check mode. Do not use ordinary deployment to initialize Data.

Before any formatting, the module resolves the stable identity and independently
checks udev serial, exact byte/sector size, whole-disk topology with no partitions,
Boot/root ancestry (including `/boot` and `/boot/efi`), all visible mounts, swap,
sysfs holders/slaves and read-only signature inspection. Uninspectable root,
missing evidence, any existing filesystem/partition/LVM/ZFS/unknown signature,
or any active/ambiguous state is STOP. It pins the selected block device, probes
exclusive availability, releases that probe and repeats the evidence check. It
then performs one `mkfs.ext4` call on
the inherited device fd with `-m 10` and a fresh UUID. There is no force flag,
retry, erase or fallback. The pinned fd remains open; the exclusive probe is
released before the formatter because [e2fsprogs 1.47.0](https://raw.githubusercontent.com/tytso/e2fsprogs/v1.47.0/misc/util.c)
performs its own exclusive busy check and rejects a device already exclusively
held by the bootstrap itself. Failure remains STOP, including failure after a
partially completed format. Bootstrap is not an idempotent repair entry.

The result reports the UUID; the operator records it in `mcp_data_uuid` for the
ordinary path. Bootstrap does not mount Data or launch the application. Neither
bootstrap nor any real device operation was executed as Author validation.

### Ordinary mount and guard

Ordinary convergence verifies ext4, the exact UUID and exactly floor(total
blocks / 10) reserved blocks, with reserved-block UID/GID both zero. It never
changes the reserve or grants runner access to it. It rejects partitions,
foreign mounts, holders/slaves, identity/topology mismatch and competing fstab
Data entries. Storage tools come from bounded Noble packages (`e2fsprogs
1.47.0-*`, `util-linux 2.39.3-*`, `udev 255.4-*`).

The Boot-side mountpoint is created empty as root:root mode `000` only when
absent. Unexpected existing ownership, permissions, symlinks or files fail
without chmod/chown repair. On reruns with Data mounted, a temporary
non-recursive `/srv` bind under `/run` exposes and validates the hidden Boot
mountpoint, then is unmounted in a finally block. Data itself is not unmounted.

The native `srv-mcp\x2dpodman.mount` uses `/dev/disk/by-uuid/<UUID>`, ext4 and
`rw,nosuid,nodev`, with no `nofail`. It requires and orders after
`mcp-data-verify.service`, a root-owned one-shot verifier that rechecks the
selected device/filesystem/guard at mount activation, including after reboot.
UUID scans reject missing, duplicated or differently resolved filesystem identity.
`mcp-data-ready.service` requires and follows the mount, rechecks actual mounted
identity, and is required by `local-fs.target`. It binds its lifetime to the mount.
The installed verifier contains only the read-only inspector, not the bootstrap
module. No privileged long-running helper is introduced. UUID disappearance
fails the mount; there is no alternate source. Runtime directory creation
requires mounted verified Data and uses pinned directory fds with `O_NOFOLLOW`.

S3B exports `mcp_data_mount_unit`, `mcp_data_ready_unit` and
`mcp_runtime_environment`. Later service wiring must require/order after the
Data-ready gate and mount (and bind service lifetime to their availability), and consume the same environment and `mcp_workspace_state_dir` /
`mcp_workspace_data_dir`. S3B does not install daemon/user-manager units or
claim that a running service has those dependencies yet.

### Runtime packages and placement

The role installs `state: present` with bounded Noble version patterns, never
`latest`, and records resolved versions using `dpkg-query` at deployment time.
No package repository is added. The following package families are selected:

| Package | Allowed version pattern | Purpose |
| --- | --- | --- |
| podman | `4.9.3+ds1-*` | Rootless container engine, including pasta build RUN |
| buildah | `1.33.7+ds1-*` | Explicit standalone `/usr/bin/buildah` residue inspection/removal |
| passt | `0.0~git20240220.1e6f92b-*` | `/usr/bin/pasta` |
| crun | `1.14.1-*` | Selected OCI runtime |
| netavark / aardvark-dns | `1.4.0-*` | Distribution engine network dependencies |
| uidmap | `1:4.13+*` | `newuidmap` / `newgidmap` |
| dbus-user-session | `1.14.*` | User-session prerequisite; user-manager authority remains S3C |

The package versions are documented by the [Ubuntu Noble package index](https://packages.ubuntu.com/noble/podman),
[Buildah index](https://packages.ubuntu.com/noble/buildah),
[passt index](https://packages.ubuntu.com/noble/passt) and
[crun index](https://packages.ubuntu.com/noble/crun).
[Podman 4.9.3 build documentation](https://docs.podman.io/en/v4.9.3/markdown/podman-build.1.html)
includes pasta for RUN. Ubuntu supported update revisions within each bounded
pattern are allowed; unavailable constraints or missing capabilities fail instead
of obtaining another runtime/repository. Transitive distro dependencies remain
APT-managed. Actual resolved target versions are unverified at this checkpoint.

Kernel >=6.8, loaded native overlay and enabled unprivileged user namespaces are
required; the role checks rather than changes sysctl policy. Ubuntu's distro
runtime/AppArmor integration is retained. No AppArmor disable, host capability,
sudo grant, reserved-block access or extra privileged service is added. Real
namespace/AppArmor behavior is not established by source inspection.

| Persistent state | Effective Data location |
| --- | --- |
| OS-recorded runner home | `/srv/mcp-podman/runner` |
| Config and launch environment | `runner/.config/containers` and `runner/.config/environment.d` |
| Cache and local data/state | `runner/.cache`, `runner/.local/share`, `runner/.local/state` |
| GraphRoot: shared/image/writable layers | `runner/.local/share/containers/storage` |
| Podman persistent DB | GraphRoot `libpod` |
| Built-in volumes | GraphRoot `volumes` |
| Default account-wide singleton and journal | `runner/.local/state/mcp-workspaced` |
| `-data-dir` and generation build/context/auth/IID/diagnostics | `/srv/mcp-podman/workspaces` and its `builds` children |
| General image-copy temporary data | `/srv/mcp-podman/tmp` |
| Runtime file events | `/srv/mcp-podman/events/events.log`, rotation threshold 10 MiB |
| Container logs | Within GraphRoot, 10 MiB per-container limit |

Relative locations above are under `/srv/mcp-podman`. Home/config ancestry is
root-owned mode 0755; writable runtime leaves are runner-owned mode 0700.
Guests receive no host home/config bind mounts. Rootless Podman and standalone
Buildah read the same HOME/XDG-selected `storage.conf`: native overlay, identical
graphroot/rootless_storage_path and UID-derived `/run/user/<UID>/containers`
runroot. Podman DB/volumes are explicit. Empty mount_program selects native
overlay. Recommended packages are disabled for runtime installation; a fixed
`PATH=/usr/bin:/bin`, rejection of fuse-overlayfs in that PATH, and rejection of
an existing `<GraphRoot>/overlay/.has-mount-program` marker prevent an implicit
fuse fallback/store conversion. A missing overlay directory is valid for a new
store. An existing overlay directory is opened relative to the pinned GraphRoot
fd with `O_NOFOLLOW`; its identity, Data device, mount ID, runner ownership and
non-group/world-writable mode must be verifiable. Symlinks, foreign mounts and
untrusted state fail closed. Markers and stores are never removed or migrated.
The [shared containers configuration](https://raw.githubusercontent.com/containers/common/v0.57.1/docs/containers.conf.5.md)
supports the engine's Data image-copy temp and event settings. Build workers also
receive the source-owned generation-specific Data TMPDIR via systemd-run.

Only volatile runroot/libpod runtime state is under `/run/user/<UID>` (tmpfs on
the reference host). Fixed deployment configuration, account/subid records,
package metadata and verifier code are host/control state with size determined
by deployment, not caller output. Runtime events and container output avoid the
host persistent journal. Application diagnostics remain the existing bounded
Data files. S3B does not configure or establish host journald retention bounds.
The later production-service owner must place caller-induced service
stdout/stderr/control-plane logs on Data or independently hard-bound their
persistent retention, and provide source/static evidence of that placement or
bound before production-service acceptance. S3B does not implement those service
units or choose a host-wide logging policy. Exact-zero behavior remains fail
closed/operator recovery; no new recovery procedure is supplied here.

### Source/static validation

From the repository root:

```sh
python3 -m unittest discover -s ansible/tests -p 'test_*.py' -v
GOTOOLCHAIN=go1.26.9 go mod verify
GOTOOLCHAIN=go1.26.9 go test ./...
GOTOOLCHAIN=go1.26.9 go vet ./...
GOTOOLCHAIN=go1.26.9 go build ./...
git diff --check
```

From `ansible/`, also run the S3A checks above plus:

```sh
ansible-playbook --syntax-check bootstrap-data.yml
ansible-playbook --list-tasks bootstrap-data.yml
```

The Python suite parses YAML/Python, walks normal task imports, renders TOML,
mount/verification units and the verifier, and checks placement/reserve/package
contracts. When available, it runs `systemd-analyze verify --root=<temporary
synthetic root> --generators=no --man=no` against the three rendered units,
with synthetic dependency units/executable placeholders; no units are started. Its pure gate and mocked formatter cases cover identity/size/topology,
Boot/root, mounted/holder/slave/swap, signatures, disabled enable, changed device
evidence, UUID/reserve mismatch and no retry after failure. The tests never open
block devices, run mkfs, contact a target, or claim real formatting/mount/reboot,
rootless overlay, Podman/Buildah store alignment/NamespaceOptions, user-manager,
network or end-to-end PASS. Those remain environment-dependent later evidence.

## S3C unattended user-manager contract

S3C consumes the verified S3A `mcp_runner_account`, `mcp_runner_uid` and
`mcp_runner_primary_gid` facts and S3B `mcp_runtime_environment`. It installs the
Ubuntu 24.04 `systemd` / `libpam-systemd` `255.4-*` prerequisites; S3B already
installs `dbus-user-session`. No package repository or Go dependency is added.

On this platform, `loginctl --no-ask-password enable-linger mcp-runner` is the
smallest supported persistent boot/logout mechanism: logind starts the user
manager at boot and keeps it after logout, without any interactive login.
Only loginctl creates its persistent marker; reruns skip that command when the
marker exists and verify `loginctl show-user ... --property=Linger --value`.
S3C also explicitly starts the distribution `user@<actual UID>.service` now.
It keeps the account's locked password and nologin shell. The invariant is
**unattended mcp-runner user-manager availability**; linger is this Ubuntu
implementation, not a universal architecture requirement. See the
[systemd 255 loginctl documentation](https://www.freedesktop.org/software/systemd/man/255/loginctl.html).

The standard [systemd 255 user-manager unit](https://github.com/systemd/systemd/blob/v255/units/user%40.service.in)
delegates `pids memory cpu`. S3C checks the loaded unit's `Delegate=yes`,
`DelegateControllers`, actual numeric `User`, active state and exact ControlGroup
instead of adding an override, helper, daemon or second cgroup authority.
It requires a unified cgroup-v2 filesystem, all three host controllers and all
three controllers readable by the runner at the manager's `cgroup.controllers`.
The [systemd delegation interface](https://www.freedesktop.org/software/systemd/man/255/systemd.resource-control.html#Delegate=)
lets the existing user manager enable these controllers for resource-controlled
user units. Missing/disabled controllers or unexpected hierarchy fail STOP;
S3C never writes cgroupfs, relaxes host policy or silently repairs delegation.
These convergence checks inspect prerequisites; they do not submit a build or
prove kernel limits/pressure behavior. The application retains its own exact
finite transient parent creation, limit attestation and stop/revert semantics.

The following deterministic facts are available to later S3E tasks in the same
play, derived afresh from the actual identity on every convergence:

| Fact | Value / downstream use |
| --- | --- |
| `mcp_user_manager_unit` | `user@<UID>.service`; require, order after and bind application lifetime to this prerequisite |
| `mcp_user_runtime_unit` | `user-runtime-dir@<UID>.service`, managed by the distribution user-manager unit |
| `mcp_user_manager_cgroup` | `/user.slice/user-<UID>.slice/user@<UID>.service` |
| `mcp_user_manager_environment` | `XDG_RUNTIME_DIR=/run/user/<UID>` and `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<UID>/bus` |
| `mcp_user_manager_required_controllers` | `cpu`, `memory`, `pids` |
| `mcp_service_environment` | S3B runtime environment combined with the explicit S3C runtime/bus environment |

The [distribution runtime-directory service](https://www.freedesktop.org/software/systemd/man/255/user%40.service.html)
creates `/run/user/<UID>`; S3C does not create/chown that directory itself.
It starts/enables the packaged user `dbus.socket` with the explicit environment,
checks the runtime directory is UID-owned mode 0700 and the bus is a UID-owned
socket, then checks `systemctl --user show ... -.slice` returns the expected
manager ControlGroup. `/usr/bin/systemd-run --version` checks executable access.
No shell startup file, login, ambient bus address or interactive authentication
is required. The same `mcp_service_environment` must be explicitly supplied by
later service execution and host control/recovery commands as `mcp-runner`.
Setting environment does not change process cgroup membership.

S3E must consume this prerequisite and environment alongside S3B's Data gates;
it owns production service placement, lifecycle/restart wiring and bounded logs.
Daemon/control/recovery processes must stay outside every finite `mcpws<W><G>.slice`
build parent. S3C creates no application/Tunnel unit, moves no process, changes
no Go source or application cgroup semantics, and grants no sudo or host capability.

The Python suite additionally runs the production S3C fact/assertion tasks through
local Ansible using synthetic identities, manager properties, controller lists
and runtime/bus metadata. It checks multiple UIDs, wrong ancestry/identity,
missing controllers/delegation/linger, and unsafe runtime/bus interfaces. It also
inspects the provisioning graph for login-free boot persistence and absence of
new units/helpers/cgroup writes. These tests never run provisioning tasks,
loginctl, systemctl or systemd-run, and never contact a deployment host.
Actual boot/logout availability, user bus operation, controller delegation,
transient-parent creation/stopping, OCI ancestry/effective limits and pressure /
saturation/control survival remain unverified real-host behavior at this Author
checkpoint. Independent source review is still required.

## S3D host destination policy and trusted DNS

S3D consumes `mcp_runner_uid` and `mcp_runner_primary_gid` from S3A, then compares
both with `pwd.getpwnam('mcp-runner')` at planning time. It never allocates or
reassigns identities. Supply these root/operator-owned inventory inputs:

| Input | Contract |
| --- | --- |
| `mcp_public_dns` | Ordered list of 1–3 distinct native public-routable literal IPs; no hostnames, zones, mapped IPv6 or fallback |
| `mcp_host_public_ips` | Complete list of the dedicated host's public IPs, including provider NAT/floating addresses absent from NICs; 1–64 distinct public literals |
| `mcp_production_endpoint` | Shared production endpoint mapping in `group_vars/all.yml`: `address: 127.0.0.1`, `port: 8080` for the reference deployment; address must be exactly `127.0.0.1` and port an integer in `1..65535` |

The endpoint is the single trusted deployment source for S3D and later #112's
`mcp-workspaced` listener and Secure MCP Tunnel local target. Those consumers
must use the same mapping directly, without defining another endpoint default.
The planner rejects malformed mappings, other addresses and non-integer ports
(including strings and booleans), without coercion or fallback. Python rendering
has no independent production port default.

Both example IP lists are empty and intentionally fail. No resolver is a compiled
or example deployment default. Reconcile the host list whenever provider address
assignments change. Actual interface addresses are additionally denied through
Netfilter's `addrtype LOCAL`, independent of inventory; a resolver matching a
listed or currently assigned host address is rejected. These lists are trusted
deployment state, never workspace/Dockerfile input.

The transient read-only Ansible planner validates inputs, snapshots
`iptables-nft-save` / `ip6tables-nft-save`, and composes `/etc/iptables/rules.v4`
and `rules.v6`. Each filter table gets exactly one marked numeric-UID OUTPUT hook
before existing OUTPUT rules, and one reserved `MCP_S3D_OUTPUT` chain. IPv4 starts
with exactly one production MCP reply exception derived from that endpoint:

```text
-A MCP_S3D_OUTPUT -s 127.0.0.1/32 -d 127.0.0.1/32 -o lo -p tcp -m conntrack --ctorigsrc 127.0.0.1 --ctorigdst 127.0.0.1 --ctorigdstport <configured-port> --ctdir REPLY -j ACCEPT
```

It immediately precedes the ordinary LOCAL DROP and existing destination policy.
It matches an exact endpoint/tuple within the trusted host/control-plane boundary;
it does not authenticate a PID, executable or specifically the Tunnel process.
IPv6 gets no exception and still begins with LOCAL DROP. All other host/self
and destination DROP rules precede the chain's terminal ACCEPT. IPv4 route-type
broadcast destinations are also denied, including directed broadcasts. No generic
stateful, loopback, DNS, gateway or port-only acceptance is added. Public
TCP/UDP/ICMP/ICMPv6 are not restricted by a protocol whitelist. No port is
published, no host network privilege is granted, and pasta remains connectivity
and translation only.

IPv4 exclusions include this-network, RFC1918, CGNAT, loopback, link-local,
protocol-assignment, documentation, deprecated relay, benchmark, multicast and
reserved ranges, plus the public-numbered Azure platform VIP. OCI metadata and
provider resolvers fall within link-local; other represented metadata classes
include shared space and IPv6 ULA. IPv6 requires native `2000::/3`, excludes
Teredo, benchmark, ORCHID, documentation, 6to4 and the non-public remainder of
IANA's protocol-assignment block. Globally reachable protocol suballocations
permitted by the current runtime contract remain public; they get no early
ACCEPT. General IPv4 egress uses finer DROP prefixes for `192.0.0.0/24` that
leave IANA's globally reachable PCP `192.0.0.9/32` and TURN `192.0.0.10/32`
eligible for the terminal public ACCEPT. Independent host/self, LOCAL/BROADCAST
and platform denies still take precedence. Trusted resolver validation remains
stricter and rejects the entire special-purpose `/24`, including `.9/.10`;
egress eligibility does not make an address a trusted DNS default. The two
IPv4 range sets share the other exclusions explicitly in
`module_utils/mcp_network.py`, independent of controller/host Python `is_global`
table versions. Trusted inputs retain current `build_dns.go` exclusions and
additionally reject the platform VIP and IANA's non-public protocol remainder.
IPv6 classification and application Go behavior are unchanged.

Unrelated chain declarations, INPUT/FORWARD policies and rule order, other
OUTPUT rules and other tables are retained; only packet counters and save-file
comments are normalized. A successful empty live save on a fresh host has no
explicit filter table, so the planner materializes its implicit ACCEPT baseline.
An existing empty/incomplete persistent file is rejected. Unrelated live and
persistent policy must agree before either file is written. Drift, reserved-chain
collisions, legacy policy, unexpected persistence plugins or competing active /
boot-enabled firewall services fail rather than being overwritten. Provisioning
must have exclusive control of the firewall during convergence; this is not a
coordinator for concurrent root/operator rule writers.

The role installs Noble `iptables=1.8.10-*`, `iptables-persistent=1.0.20` and
`netfilter-persistent=1.0.20` without installer autosave, package removal or
service activation. Generic persistence executables must already resolve to
the nft variants; the role does not switch alternatives. Both rendered families
undergo `*-nft-restore --test` before persistence writes at deployment time.
Only `netfilter-persistent start` loads the rules, with its boot service enabled.
The loader defaults disable additive/no-flush restore and stop-time flushing.
Every owned setting is written once; comments and save-control settings are
preserved, while unknown/non-declarative shell configuration fails. The plugin's
optional test mode is disabled because it returns success on parser failure;
the planner explicitly tests both families and restore errors propagate. Failure
can leave partial convergence and is never converted into success or a fallback.

`/etc/sysctl.d/99-mcp-network.conf` contains exactly
`net.ipv4.ping_group_range = <actual primary GID> <actual primary GID>` as
root:root 0644. Only that key is applied through systemd-sysctl's effective
configuration; a conflicting effective override is rejected by reading the key
back. No raw-socket capability or wider group range is added.

The host resolver implementation is one root-owned 0644 regular
`/etc/resolv.conf`, with only the validated ordered `nameserver` values. A known
systemd-resolved symlink is replaced atomically, without changing its generated
target. The planner requires the existing NSS `hosts: files dns` path, refusing
alternate NSS resolver modules rather than changing host NSS policy. Thus the
selected host/Podman path reads direct upstreams instead of the stub or a DHCP
provider list. This supported external-management mode is an Ubuntu mechanism,
not an architecture invariant. It does not reconfigure systemd-resolved's D-Bus
clients, add a daemon, promise cache behavior or force untrusted public DNS/DoH
exclusivity. There is no resolved.conf DNS list that could diverge from inventory.

`mcp_public_dns_validated` exports the normalized, validated list in operator
order for later S3E repeated `-public-dns` arguments. Both it and the host file
come from the same planner result derived from `mcp_public_dns`. S3D does not
render daemon arguments, service/Tunnel/recovery units, provider policy or any
S3E/S3F implementation. Existing namespaces may retain stale resolver views;
their lifecycle and real-runtime propagation remain later integration work.

Material mechanism references:

- [Ubuntu Noble resolved resolver-file modes and NSS interfaces](https://manpages.ubuntu.com/manpages/noble/man8/systemd-resolved.service.8.html)
- [Ubuntu Noble iptables extensions: owner and addrtype](https://manpages.ubuntu.com/manpages/noble/man8/iptables-extensions.8.html)
- [Upstream iptables 1.8.10 source](https://www.netfilter.org/projects/iptables/files/iptables-1.8.10.tar.xz): restore's double-quote parser is not shell quoting; preserve existing rule text
- [Noble persistence package version](https://packages.ubuntu.com/source/noble/iptables-persistent) and its [IPv4](https://sources.debian.org/src/iptables-persistent/1.0.20/plugins/15-ip4tables/) / [IPv6 loader source](https://sources.debian.org/src/iptables-persistent/1.0.20/plugins/25-ip6tables/)
- [IANA IPv4](https://www.iana.org/assignments/iana-ipv4-special-registry) / [IPv6 special-purpose registries](https://www.iana.org/assignments/iana-ipv6-special-registry)
- [Azure platform VIP classification](https://learn.microsoft.com/en-us/azure/virtual-network/what-is-ip-address-168-63-129-16)

`python3 -m unittest discover -s ansible/tests -p 'test_*.py' -v` includes pure
input/rule-order/preservation/rerender tests, mocked read-only planner cases and
a local Ansible fact-interface test with multiple synthetic identities. No test
applies firewall/sysctl/resolver configuration. The strict synthetic rule
interpreter is not the Netfilter parser or a packet test. The deployment parser
calls above are encoded but require supported target tools and privileges.
Actual filtering, pasta composition, workspace isolation, IPv6 routing, metadata
blocking, reboot persistence, Podman/image-fetch DNS, cache and namespace
behavior, daemon public-DNS runtime behavior and OCI E2E remain unverified at
this Author/source checkpoint. Independent source review is a later checkpoint.

## S3E production services and trusted recovery

S3E installs `mcp-workspaced.service` as `mcp-runner` and `mcp-tunnel.service`
as `mcp-tunnel`. Set `mcp_canonical_image` to the canonical image's exact
`@sha256:<64 lowercase hex>` reference and `mcp_tunnel_id` to the deployment's
`tunnel_<32 lowercase hex>` ID. The checked-in placeholders deliberately fail.
The image must already exist in the runner's local store; S3E does not pull it
or create a Tunnel object. The non-secret `mcp_production_endpoint` mapping
supplies S3D's exact reply rule, the daemon listener and the Tunnel `main`
channel. No second production endpoint default exists.

`group_vars/all.yml` owns one `mcp_workspaced_config` mapping, with all exposed
shared CLI policy values explicitly selected from the application's current
reference policy. It derives state/Data paths from S3B and ordered DNS from
S3D. Configuration tasks validate this complete mapping and generate exactly
one `mcp_workspaced_argv` vector. Both the daemon unit and the root-owned recovery
launcher render that vector; neither maintains a second flag/value list.
The same S3B/S3C environment supplies HOME, XDG placement, TMPDIR, PATH and the
actual-UID runtime/bus address. Unsupported/missing fields, unpinned images,
alternate paths and invalid policy fail before service installation.

The daemon runs `/usr/local/bin/mcp-workspaced -daemon -mcp-listen ...` in
`system.slice`, outside finite build parents. It requires and binds its lifetime
to the S3B Data mount/verification unit, actual-UID user manager and
`netfilter-persistent.service`, and orders after `systemd-sysctl.service`.
Its unprivileged read-only preflight additionally verifies prerequisite active
states, mounted Data, runtime/bus metadata, user-manager cgroup/controllers,
the exact trusted resolver view and exact primary-GID ping range. It neither
repairs host state nor installs another network/cgroup authority. Root-managed
firewall loading remains the S3D distribution service's responsibility.

Both services have a 15-second stop limit, a 10-second failure restart delay,
and at most three starts per 300-second interval. `Type=exec` establishes only
process supervision. It does not assert MCP or Tunnel readiness. Application
replacement is registered by S3A so a changed binary also restarts the consumers.
No rootless-runtime-breaking `NoNewPrivileges` setting is imposed on the runner;
the Tunnel service has no capabilities and uses `NoNewPrivileges` and a read-only
filesystem view.

Service stdout/stderr persistence is **prohibited** (`StandardOutput=null`,
`StandardError=null`) rather than becoming a caller-induced Boot-volume sink.
This intentionally gives up persistent process-output diagnostics. Application
generation diagnostics remain the existing bounded Data files, trusted recovery
inspection still writes to the operator's terminal, and systemd retains unit
state/exit status. The Tunnel UI's in-memory event buffer is limited to 100 events;
raw HTTP logging is disabled and no log file is configured. No host-wide journald
retention policy or extra logging daemon is introduced.

### Bounded recovery entry

After explicitly stopping the daemon, an authorized operator invokes the
root-owned `/usr/local/lib/mcp-workspaced/recover` launcher as `mcp-runner`:

```sh
sudo systemctl stop mcp-workspaced.service
sudo -u mcp-runner /usr/local/lib/mcp-workspaced/recover inspect
sudo -u mcp-runner /usr/local/lib/mcp-workspaced/recover backend "$BACKEND_SELECTION"
sudo -u mcp-runner /usr/local/lib/mcp-workspaced/recover storage
sudo -u mcp-runner /usr/local/lib/mcp-workspaced/recover cleanup "$GENERATION_SELECTION"
```

These are separate operator-selected actions, not a script to run in sequence.
Selections come only from the application's inspection output. The launcher
accepts no alternate flags, configuration path, environment override or command.
It requires the runner's actual UID/GID, a loaded inactive/failed daemon with
zero MainPID and no queued job, and the same service prerequisites. It never
stops a daemon implicitly or grants the runner sudo. The operator's existing
authority performs the user switch; the launcher itself has no privilege.

Recovery execs the same application with the shared arguments and a replaced
explicit environment. The application's two-minute recovery deadline,
account-wide singleton, reconciliation, boot-epoch evidence and exact selection
checks remain authoritative. A racing start fails at that singleton; the launcher
does not bypass or replace it. No journal edits, arbitrary deletion, global prune
or automatic lifecycle repair are added. Stopping the daemon also stops its
bound Tunnel; later restart is an explicit operator action.

### Fixed Tunnel artifact and metadata-only credential boundary

The role downloads these exact
[official v0.0.16 release assets](https://github.com/openai/tunnel-client/releases/tag/v0.0.16):

| Asset | SHA-256 |
| --- | --- |
| `tunnel-client-v0.0.16-linux-arm64.zip` | `963d0384aaa7c798778479c45673f9051a4039dd891aef8f8978d2a1a628b74f` |
| `SHA256SUMS.txt` | `fcce864a5935655534cc3d981257509579dbb5c71dbaec64c17a157ca9d65e56` |
| `tunnel-client-v0.0.16-provenance.sigstore.json` | `adae10fe6a877d05bee25f4d5fc2c0623c76189ac5af7387efa1c0152e16d3ab` |

Ansible `get_url` checks every digest. A Python-standard-library installer checks
all three again before extracting the client and license material into the
root-owned `/usr/local/lib/mcp-tunnel/v0.0.16/` directory. It does not execute the
binary, install the bundled Cloudflare companion, resolve `latest`, or add a
signature tool. This verifies the task's pinned provenance **bytes**, not Sigstore
signature/identity semantics. A version or pin change requires another explicit
deployment revision.

Human must separately stage `/etc/mcp-tunnel/control-plane-api-key` as a regular
`mcp-tunnel:mcp-tunnel` mode `0600` file. Its directory is `root:mcp-tunnel` mode
`0750`. Ansible may establish the non-secret directory, but never creates or
transports the key. Missing/wrong metadata stops deployment before any S3E
service operation. The `stat` tasks use `follow: false`, `get_checksum: false`,
`get_mime: false` and `get_attributes: false`; only metadata is registered.
Readability is checked as `mcp-tunnel`, and `test -r` must deny `mcp-runner` without
opening the file. S3A's separate UID/GID and private-group checks remain required.
No ACL or supplementary group grants the runner access.

The root-owned `0640` non-secret `/etc/mcp-tunnel/tunnel.yaml` uses exactly
`control_plane.api_key: file:/etc/mcp-tunnel/control-plane-api-key`.
Each Tunnel start repeats the metadata-only checks as `mcp-tunnel`. Only the
official client resolves the runtime secret at execution. Its environment is
cleared with `env -i`; ambient API-key, proxy and configuration values cannot
override the file-based configuration. There is no secret value in deployment
variables, argv, shell startup files, templates or tests.

### Local readiness and remaining evidence

The [v0.0.16 configuration contract](https://github.com/openai/tunnel-client/blob/v0.0.16/docs/configuration.md)
supports the file reference and `mcp.startup_wait_timeout`. It also explicitly
states that polling resumes after the native wait times out. Therefore the
60-second native wait alone is insufficient for S3E's fail-closed startup.

Before `run`, an unprivileged preflight waits at most 60 seconds for the exact
local TCP listener, retrying only connection refusal. The landed application
binds that socket only after open/reconciliation. The supported v0.0.16
[`doctor` command](https://github.com/openai/tunnel-client/blob/v0.0.16/cmd/client/doctor_command.go)
then must exit successfully: it checks local reachability/configuration,
optional OAuth metadata and health-listener availability without polling the
Tunnel control plane. Both are mandatory `ExecStartPre` commands under a
90-second systemd start limit. A failed wait, failed doctor or timeout prevents
`run` entirely. The native 60-second wait remains configured, and the unit is
bound to the daemon's lifetime. Neither an early process spawn nor a failed
startup probe is accepted as local readiness.

The health/admin listener explicitly uses `127.0.0.1:0`, avoiding collision with
the deployment-owned MCP port; remote UI and browser launch are disabled. Its
ephemeral port is an operator-local diagnostic surface, not an MCP endpoint or
a remote readiness assertion.

The Python suite invokes only configuration/render/assertion Ansible tasks on
localhost with synthetic facts. It compares every effective daemon/recovery
argument, two UIDs/endpoints and changed policies; rejects malformed inputs;
tests recovery gating and exact exec with mocked process interfaces; tests
secret metadata without content fixtures; tests delayed/refused/timed-out local
startup with mocked sockets; checks pinned installation idempotence and corrupt
asset rejection in a temporary tree; and parses rendered units with
`systemd-analyze verify` using a synthetic root. It never starts a real unit,
runs the Tunnel client, contacts OCI, or stages/opens a real credential.

Run the source checks listed above, including `site.yml` / `bootstrap-data.yml`
syntax checks, local `tests/revision_contract.yml`, the full Python suite,
`go mod verify`, Go tests/vet/build, and `git diff --check`. Actual systemd
startup/restart, target permissions, runtime cgroups/network/DNS, Data pressure,
installed-client execution and Tunnel/ChatGPT interoperability remain I1/I2.
This source checkpoint and any synthetic result do not establish Tunnel READY
or real-host PASS. Independent source review and later runtime acceptance remain
separate checkpoints.

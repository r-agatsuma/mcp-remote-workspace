# S3 deployment source (S3A + S3B)

This tree targets Ubuntu 24.04 on arm64. It creates the unprivileged
`mcp-runner` and `mcp-tunnel` system accounts, then installs the application
from an explicitly selected repository commit. S3B adds the verified Data mount
and rootless runtime placement described below. User-manager/cgroup authority,
network policy, production services and Tunnel wiring remain later slices.

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

`site.yml` runs S3A then S3B. It never imports `bootstrap-data.yml`, the
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
an existing GraphRoot `.has-mount-program` marker prevent an implicit fuse
fallback/store conversion. Unexpected packages/stores are not removed. The [shared containers configuration](https://raw.githubusercontent.com/containers/common/v0.57.1/docs/containers.conf.5.md)
supports the engine's Data image-copy temp and event settings. Build workers also
receive the source-owned generation-specific Data TMPDIR via systemd-run.

Only volatile runroot/libpod runtime state is under `/run/user/<UID>` (tmpfs on
the reference host). Fixed deployment configuration, account/subid records,
package metadata and verifier code are host/control state with size determined
by deployment, not caller output. Runtime events and container output avoid the
host persistent journal. The native journald drop-in independently bounds
control-plane metadata: SystemMaxUse=64M, SystemMaxFileSize=8M, RuntimeMaxUse=16M,
RuntimeMaxFileSize=4M. These apply to the host journal as a whole and restart
journald only when changed; retention includes the fixed active-file overhead
allowed by journald, so 64 MiB is not a strict total-filesystem-byte ceiling. Application diagnostics remain the existing bounded
Data files. Exact-zero behavior remains fail closed/operator recovery; no new
recovery procedure is supplied here.

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

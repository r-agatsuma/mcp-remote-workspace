# S3 operator runbook

This is the operational entry point for the landed S3 deployment. The detailed
[source contracts](../ansible/README.md), [ingress](ingress.md),
[lifecycle](lifecycle.md), [admission](admission.md) and [recovery](recovery.md)
explain its boundaries. Issue #113 owns consolidation, not new production
behavior. Commands below are procedures for a separately authorized operator;
source review does not authorize executing them on a host.

## Evidence and execution gates

| Evidence | What it establishes | What it does not establish |
| --- | --- | --- |
| Source fact | Roles, configured paths, account names, unit dependencies, bounded launcher actions | Installed host state |
| Static/rendered checks | Synthetic inputs flow through production task/configuration code consistently; local tests pass | Real service or runtime effectiveness |
| Operational expectation/inference | A successful prerequisite chain should permit daemon reconciliation and local Tunnel startup | Measured boot, readiness, packet or credential behavior |
| Later #95 dirty-host integration | Separately authorized composed-product measurements on the selected host | Clean reconstruction or real ChatGPT E2E by itself |
| Later #96 clean-host / real Tunnel / ChatGPT E2E | Separately authorized clean deployment and external acceptance | A property measured by this source checkpoint |

No source result is real systemd, Podman/Buildah, Netfilter, DNS, cgroup, OCI,
credential-behavior, Tunnel READY or ChatGPT E2E PASS. Ansible `--check` still
connects to hosts; it is not a substitute for local source checks.

## Prerequisites and trusted inputs

Use the dedicated Ubuntu 24.04 arm64 target and a trusted Ansible controller.
The operator must have existing host administration authority; neither service
account gains sudo or an interactive shell. Use a reviewed repository revision.
Keep host-specific inputs outside Git. All variable names in the shell examples
are placeholders for explicitly selected local paths or inspection selections;
never substitute a credential into a command.

Record the exact source/deployment commit, target platform, package provenance,
and non-secret input revision in the authorized deployment record. Supply a
private inventory based on `ansible/inventory/hosts.example.yml` and a non-secret
variables file with these inputs (see `ansible/group_vars/all.yml`):

| Input | Required meaning |
| --- | --- |
| `mcp_source_revision` | Exact 40-hex reviewed application commit, never a branch or tag |
| `mcp_data_device_id`, `mcp_data_expected_serial` | Independently verified whole-device stable by-id identity and udev serial |
| `mcp_data_expected_bytes`, `mcp_data_expected_sector_bytes` | Exact independently verified geometry |
| `mcp_data_uuid` | UUID from separately authorized initial bootstrap, checked on ordinary convergence |
| `mcp_runner_subuid_start`, `mcp_runner_subgid_start`, `mcp_runner_subid_count` | Explicit non-overlapping subordinate allocations |
| `mcp_public_dns` | Ordered one-to-three trusted public literal resolvers; no built-in production list |
| `mcp_host_public_ips` | All public host addresses, including provider NAT/floating addresses |
| `mcp_canonical_image` | Approved digest-pinned reference, already installed in the runner's local store |
| `mcp_tunnel_id` | Operator-provisioned Tunnel identifier; separate from the key |
| `mcp_production_endpoint` | Fixed `127.0.0.1` and selected port; one source for firewall, listener and Tunnel |

The canonical image already being present is an application prerequisite;
`site.yml` does not populate it or create a Tunnel object. Do not invent a pull,
image migration or control-plane provisioning procedure here. If those trusted
inputs are unavailable, STOP and return to the responsible provisioning task.
Kernel/runtime, native overlay, NSS and existing firewall prerequisites are
checked by the landed roles; conflicts are STOP, not permission to replace
unrelated policy. Keep the complete shared `mcp_workspaced_config` mapping from
source; do not construct alternate daemon or recovery flags.

## First provision: separately authorized destructive Data bootstrap

This is a distinct, default-disabled operation, not a deployment retry or a
recovery procedure. Confirm Human authorization for the specific blank Data
volume and independently verify its stable identity, serial, size and sector
size. Never use a bare volatile device name as destructive authority. Boot/root
ancestry, existing signatures, mounted/active dependencies, identity mismatch,
or ambiguous observations require STOP.

Only in that separately authorized first-provision task, from `ansible/`:

```sh
ansible-playbook -i "$INVENTORY_PATH" bootstrap-data.yml \
  -e "@$NONSECRET_INPUTS_PATH" -e '{"mcp_data_bootstrap_enabled": true}'
```

Do not persist the enable in inventory. This path checks the device and issues
one ext4 format with 10% root reserve; it does not mount or start services.
Record the returned UUID as `mcp_data_uuid` for normal convergence. A failed or
partially completed format is STOP; do not retry blindly, erase signatures or
reformat. Check mode is refused. No actual bootstrap was performed as S3F evidence.

## Human staging of the Tunnel runtime key

The runtime key stays outside Git, Ansible, command arguments, shell history,
shell startup files and diagnostic bundles. Ansible establishes the non-secret
directory but deliberately stops before service operation if key metadata is
missing or wrong. Human stages the key separately through an approved protected
secret-delivery channel directly to the following final target; this runbook
never prints or transports its contents:

| Path | Type | Owner:group | Mode |
| --- | --- | --- | --- |
| `/etc/mcp-tunnel` | Real directory, not a symlink | `root:mcp-tunnel` | `0750` |
| `/etc/mcp-tunnel/control-plane-api-key` | Regular file, not a symlink | `mcp-tunnel:mcp-tunnel` | `0600` |

On initial convergence the metadata gate may stop after the directory/accounts
exist. Complete authorized Human staging, then rerun ordinary convergence. Wrong
existing state must be investigated through that staging process; do not use
blind permission repair or read the key to diagnose it. Verify metadata without
following links or opening contents:

```sh
sudo stat --format='%F %U:%G %a' /etc/mcp-tunnel
sudo stat --format='%F %U:%G %a' /etc/mcp-tunnel/control-plane-api-key
sudo -u mcp-tunnel /usr/bin/test -r /etc/mcp-tunnel/control-plane-api-key
sudo -u mcp-runner /usr/bin/test -r /etc/mcp-tunnel/control-plane-api-key
```

Expected: exact types/ownership/modes above, tunnel readability exit 0, runner
readability exit 1. Any other result is STOP. These are metadata/access checks,
not proof of valid credential content or control-plane acceptance. Never cat,
echo, checksum, diff, back up or attach the key. Only the official runtime
consumes `file:/etc/mcp-tunnel/control-plane-api-key` in the non-secret
`/etc/mcp-tunnel/tunnel.yaml`.

## Normal non-destructive convergence

Run source verification first on the selected controller checkout. Then, only
under the separate host-operation authorization, from `ansible/`:

```sh
ansible-playbook -i "$INVENTORY_PATH" site.yml -e "@$NONSECRET_INPUTS_PATH"
```

The fixed sequence is S3A application/identities, S3B verified Data/runtime,
S3C unattended user manager, S3D network/DNS, then S3E services. This entry never
imports destructive bootstrap. It verifies the selected filesystem/UUID and
mounts Data; missing or wrong Data never falls back to formatting or Boot.
Application installation uses `/usr/local/bin/mcp-workspaced` as root:root 0755.
Production services are enabled and started, or restarted when the binary or
service configuration changes. This is a host mutation, not a static command.

A failed task may leave partial convergence. Preserve the failure evidence and
identify the failed prerequisite before another attempt. Do not treat all
failures as harmless idempotent retries. No tags, task skipping, ignored errors,
or manual alternate service launch is a supported shortcut around the gates.

## Normal service operation and observation

On the authorized host:

```sh
sudo systemctl start mcp-workspaced.service
sudo systemctl start mcp-tunnel.service
sudo systemctl show mcp-workspaced.service mcp-tunnel.service \
  --property=LoadState,ActiveState,SubState,MainPID,Result,ExecMainStatus,Job
```

For planned stop, stop ingress first, then its daemon:

```sh
sudo systemctl stop mcp-tunnel.service
sudo systemctl stop mcp-workspaced.service
```

For an authorized restart:

```sh
sudo systemctl stop mcp-tunnel.service
sudo systemctl restart mcp-workspaced.service
sudo systemctl start mcp-tunnel.service
```

The daemon binds to the Data mount/ready gate, actual-UID user manager and
netfilter-persistent, and orders after systemd-sysctl. The Tunnel binds to the
daemon; stopping the daemon also stops the Tunnel. Starting the daemon again
alone does not promise the Tunnel restarts: start it explicitly. Units have
15-second stop limits, a 10-second failure restart delay and three starts per
300 seconds. Repeated failure or rate limiting requires diagnosis, not an
unbounded restart loop. Stop the daemon before exclusive recovery and ensure no
operator/deployment process is concurrently starting it.

`Type=exec`, ActiveState and MainPID are supervision observations, not readiness.
The application binds the configured loopback socket only after singleton
acquisition and startup reconciliation. The deployed Tunnel startup first checks
credential metadata and waits at most 60 seconds for that exact TCP listener,
then requires the pinned client's local `doctor` command to succeed before
`run`; the whole startup has a 90-second systemd limit. Inspect the two units'
state/exit status above to distinguish failed pre-start from a running process.
There is no separate application health endpoint or public workspace status API.
For a fresh bounded local listener observation, use the same installed preflight
as the Tunnel unit (metadata/access checks only; it never opens the key):

```sh
sudo -u mcp-tunnel /usr/bin/env -i PATH=/usr/bin:/bin \
  /usr/bin/python3 -I /usr/local/lib/mcp-tunnel/preflight
```

Exit 0 establishes that the metadata gate and exact local TCP connection succeeded
at that observation. A nonzero exit or timeout is STOP for local readiness; do
not substitute a process-status check for it. This does not send a product tool
call or start another Tunnel client.

The local doctor/listener gates give only bounded local startup evidence. They
do not establish real control-plane READY, real credential validity or ChatGPT
identity delivery. The Tunnel health/admin listener uses an ephemeral loopback
port; do not assume a fixed health URL or expose it remotely. Do not run a second
Tunnel client for observation. Real readiness validation belongs to #96.

## Exclusive trusted recovery

Use only the installed S3E launcher. It uses the same rendered shared arguments,
Data paths, runtime environment and DNS/resource/storage policy as the daemon.
Do not retype flags or pass an alternate config/environment. First stop both
services as above and observe the daemon's loaded, inactive/failed state,
MainPID 0 and empty Job. The launcher enforces those conditions and prerequisites;
the application singleton remains the final arbiter against a racing start.
Never unlink its lock or bypass reconciliation.

Inspect as the actual service account:

```sh
sudo -u mcp-runner /usr/local/lib/mcp-workspaced/recover inspect
```

The output contains journal-owned operations, opaque generation selections,
terminal-evidence classification and storage latch state. Select only an exact
token from this inspection. The following commands are separate decisions, not
a sequence to execute automatically:

```sh
sudo -u mcp-runner /usr/local/lib/mcp-workspaced/recover backend "$BACKEND_SELECTION"
sudo -u mcp-runner /usr/local/lib/mcp-workspaced/recover storage
sudo -u mcp-runner /usr/local/lib/mcp-workspaced/recover cleanup "$GENERATION_SELECTION"
```

`backend` uses `operations[].selection`; only a valid recorded BootID different
from the current kernel boot can release that prior-boot reservation. Current
boot or missing historical evidence remains blocked. Reboot, process absence or
a kill result alone does not establish generation absence. A released permit
does not prove success or remove residue.

`storage` only samples and clears the persisted storage latch when both configured
byte/inode margins are safe. Restore capacity only through a separately authorized,
evidence-based operator recovery task first. Exact-zero R2 or whole-pool reset
has no generic safe deletion procedure in this runbook: return to Human, preserving
ownership evidence. No threshold reduction or reserved-block access is granted.

`cleanup` uses `generations[].selection` and exact generation fencing. Pending
backend work blocks cleanup. It must establish required residue absence and a
durable transition before any fresh generation can be admitted. Never guess a
container, build object, workspace or generation from names/logs.

The application's recovery control deadline is two minutes. Failure, timeout,
or uncertain persistence is not completion. Inspect again only if prerequisites
and exclusive ownership remain valid, and return unresolved uncertainty to Human.
After successful, understood recovery, explicitly start daemon then Tunnel and
repeat local observations. Do not replay an uncertain user operation.

## Missing/wrong Data and prerequisite failure

Normal convergence and boot gates reject wrong/missing Data; the Boot-side
mountpoint remains root-owned mode 000, and persistent runtime state belongs on
`/srv/mcp-podman`. STOP ingress/service operation and record unit state and the
failed deployment assertion. Check the trusted input record against the intended
volume identity through the separately authorized storage investigation.
Recovery itself requires verified mounted Data and the same user-manager/network
prerequisites. It is not an escape hatch for a failed mount or lost journal.

Never hand-edit the journal, blindly chmod/chown/rm Data/runtime state, globally
prune Podman/Buildah, delete unknown state to clear a failure, or bypass singleton
ownership. Do not broaden firewall/DNS/cgroup privilege or add another authority
for convenience. Do not use bootstrap to repair an existing filesystem. Unknown
ownership, missing journal, ambiguous residue, unsafe capacity or prerequisite
drift requires Human/coordinator disposition.

## Application and Tunnel upgrades; rollback

For an application/deployment upgrade, review the exact repository change and
source checks, record old/new commits and non-secret inputs, plan the outage,
stop Tunnel then daemon, and confirm compatibility with existing durable state
and canonical image. Set `mcp_source_revision` to the exact accepted commit and
run ordinary convergence from the reviewed deployment checkout. Observe local
startup again. No implicit journal migration or canonical-image switch is
supported. Existing generations referring to another canonical image fail closed.

Tunnel upgrades are explicit repository changes: update the pinned release,
Linux arm64 archive, checksum/provenance digests, installer verification,
versioned paths, unit/config contract checks and documentation together under a
new authorized task and review. Verify official release material and supported
file-reference/readiness behavior. Current code verifies pinned provenance bytes,
not Sigstore signature semantics. Never resolve `latest` at startup, replace the
binary in place manually or introduce a new verification dependency implicitly.

Rollback is a Human decision after checking application/journal/image and
configuration compatibility. Record partial deployment and unknown outcomes;
stop services and preserve evidence if compatibility cannot be established.
Do not automatically downgrade packages, revert the journal, restore a copied
runtime tree, or retry an uncertain operation. A prior source revision alone is
not proof that rollback is safe.

## Reboot expectations and later checks

Source encodes stable-UUID Data mounting/verification, unattended runner linger,
user-manager runtime/bus, persistent network/sysctl configuration and enabled
production services. These imply intended unattended startup; they are not a
measured boot result. A separately authorized reboot check must record actual
Data identity/placement, user-manager controllers/bus, network/DNS behavior,
service ordering/local readiness and reconciliation. Perform the same unit-state
observations after reboot. Unfinished backend reservations still require exact
inspection/recovery; reboot does not replay builds or prove cleanup. Real-runtime
checks belong to #95 and clean boot/Tunnel/ChatGPT acceptance to #96.

## Diagnostics and redaction

Collect the exact code/deployment revision, non-secret input revision, failed
assertion, package/build metadata and selected unit-state fields. Use the bounded
`systemctl show` command above; service stdout/stderr are deliberately sent to
null, so journald is not a retained application/Tunnel-output source. Do not enable
raw HTTP logging or broaden persistence for convenience. Product generation
logs on Data are bounded retained prefixes, not complete output or terminality
evidence; treat workload text as untrusted and potentially secret-bearing.

Recovery inspection is exclusive and bounded, not an arbitrary runtime inventory.
Keep originals in authorized restricted evidence storage. Before sharing, redact
credentials, Tunnel IDs, device identities, private/host/resolver addresses, raw
OpenAI identities, workload secrets, request contents and exact selection tokens.
Retain only necessary correlation in a restricted record. Never attach the
credential, raw inventory, whole environment, runtime tree or unreviewed logs.

## One source/static verification entry point

From the repository root:

```sh
./scripts/verify-source
```

This uses the existing Go, Python unittest, Ansible/PyYAML and available readelf /
systemd-analyze tooling. It does not install dependencies or contact an inventory.
It runs Go module verification, read-only gofmt checks, tests, conditional race
tests, vet/build, local smoke and artifact metadata, repository YAML parsing,
Ansible syntax/task-list and example-inventory inspection, localhost revision
assertions, the entire S3 component /
render suite and working/index `git diff --check`. Initial Go toolchain/module
downloads may need public network access.

Exit 0 means all supported source checks completed successfully with no skips;
1 is failure; 2 is partial with explicit UNSUPPORTED / NOT EXECUTED reasons.
A failed race-capability probe is reported, never labeled PASS. An available
race toolchain followed by failed repository tests is a failure. Synthetic
systemd unit parsing is not real systemd execution.

`ansible/tests/test_s3f_contract.py` connects production identity derivation,
S3B environment, S3C manager, S3D planner outputs and S3E service/recovery/Tunnel
renders using two synthetic identities, ports and resolver orders. Independent
fixed path/account anchors and the actual install task guard against shared
mistakes; existing S3B–S3E tests cover negative gates, secret metadata and bounded
recovery actions. Source inventory checks protect bootstrap separation and the
single authority/service/runner boundary. These are drift checks, not an exhaustive
secret detector or a replacement for independent source review and manual
redaction review of changed docs/configuration/examples.

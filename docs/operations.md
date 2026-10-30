# S3 operator runbook

This is the operational entry point for the landed S3 deployment. The detailed
[source contracts](../ansible/README.md), [ingress](ingress.md),
[lifecycle](lifecycle.md), [admission](admission.md) and [recovery](recovery.md)
explain its boundaries. Commands below are procedures for a separately
authorized operator;
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

## Three deployment authorities and controller prerequisites

The supported procedure is:

```text
reviewed source-owned policy
+ validated stage-appropriate operator manifest
+ separately staged secret where applicable
        -> scripts/deploy formal invocation
```

Use a trusted controller with Python 3.12 or newer, the existing Ansible/PyYAML
controller dependencies, Git, OpenSSH and an authorized SSH authentication
agent. The controller need not be a Mac; macOS is not architecture. Neither
service account gains sudo or interactive login. The selected management account
must already have the authorized noninteractive become access required by the
playbooks. No password/private-key/credential file is a wrapper input.

Layer 1 is the reviewed repository. `ansible/group_vars/all.yml` owns the v0
reference size (100 GiB), subordinate range length, Google resolver pair and
exact canonical image digest, together with the endpoint, paths, storage,
service topology, toolchain/runtime/package, resource/TTL/backend-operation and
Tunnel artifact policy. These are reference-deployment choices, not universal
product invariants. Change them only through a reviewed source revision.

Layer 2 is operator-controlled, non-secret, per-deployment state. Keep each real
manifest as a durable regular mode `0600` file outside every public checkout and
outside `/tmp`. Do not put infrastructure identities into Git. The public
[bootstrap schema](../ansible/inventory/bootstrap-manifest.example.yml) and
[final schema](../ansible/inventory/deployment-manifest.example.yml) contain
placeholders only. The wrapper enforces the exact stage-specific key sets:

| Input | Bootstrap | Final |
| --- | --- | --- |
| `mcp_source_revision` (40 lowercase hex) | Required | Required |
| `mcp_target_ssh_host_key_sha256` (canonical unpadded OpenSSH SHA256 fingerprint) | Required | Required |
| `mcp_data_device_id`, `mcp_data_expected_serial` | Selected whole-device stable by-id and exact udev serial | Same identities, freshly checked |
| `mcp_data_expected_sector_bytes` (integer 512 or 4096) | Required observation, no effective default | Required observation |
| `mcp_data_uuid` | Forbidden | Independently verified ext4 UUID |
| `mcp_runner_subuid_start`, `mcp_runner_subgid_start` | Forbidden | Positive, host-verified allocations |
| `mcp_tunnel_id` | Forbidden | Selected `tunnel_<32 lowercase hex>` |

Layer 3 is only the independently staged runtime secret described below. The
manifest cannot contain API-key/credential fields, source-policy duplicates,
bootstrap enable, nested policy, alternate vars files or unknown keys.

Manifest and locator YAML must be UTF-8, exactly one document and one top-level
mapping. Duplicate keys at every depth, merge keys, anchors, aliases, explicit
or custom tags, directives, malformed YAML and template expressions are rejected
unconditionally. Only strings, canonical unsigned decimal integers, lists and
string-key mappings are interpreted; quote strings which YAML would otherwise
interpret as numbers, booleans or timestamps. This narrow schema deliberately
rejects alternate numeric spellings, timestamps and parser fallback. Input is
bounded to 64 KiB. A hash is reported only after deterministic interpretation,
schema validation and source binding succeed.

## Formal invocation and target identity

Create a separate private locator file from
[`locator.example.yml`](../ansible/inventory/locator.example.yml), containing
exactly `host`, integer `port` and `user` for bootstrap. For final, remove `host`:
only integer `port` and `user` are accepted; the provider record supplies the
SSH public IPv4. It is not a general Ansible inventory.
SSH config aliases/options, vars plugins, inventory scripts and adjacent
`host_vars`/`group_vars` are not consumed. Resolve any SSH-config alias into an
explicit DNS/IP locator and account for bootstrap. That locator must resolve to
exactly one address for the invocation; ambiguity requires operator re-selection.
Final does not resolve a DNS locator.

SSH config / DNS / resolved IP is a **locator**, not identity authority. Human
must accept the expected host-key fingerprint through the authorized selection
process before creating the manifest. `ssh-keyscan` retrieves an untrusted public
key; only a key whose SHA256 equals that accepted fingerprint is used. The
wrapper then authenticates an SSH connection with strict checking, an isolated
known-hosts file, no global/user SSH config, no DNS host-key authority, no
connection reuse and no automatic key update. It never accepts unattended TOFU.
Mismatch is STOP / Human re-selection, even if the locator stayed unchanged.
A new clean host needs a new accepted fingerprint and new lifecycle artifacts.

From the exact reviewed, clean controller checkout, perform local-only validation:

```sh
./scripts/deploy validate bootstrap --manifest "$BOOTSTRAP_MANIFEST" --inventory "$LOCATOR"
./scripts/deploy validate final --manifest "$FINAL_MANIFEST" --inventory "$FINAL_LOCATOR"
```

These are separate stage examples, not a sequence that requires a final manifest
before bootstrap. Record the returned stage, source revision, exact manifest
SHA256 and exact locator-input SHA256 in restricted deployment evidence.
Controller HEAD must equal `mcp_source_revision`; any tracked change (including
staged changes) fails. Bootstrap `run` requires those accepted hashes. Final
`run` accepts them optionally and checks any supplied hash against the consumed
bytes. Each input is read once. It passes normalized validated Layer-2 JSON
to Ansible, retaining both that JSON and the exact original bytes and hashes.
It exports only the committed source into an isolated execution tree from raw commit/tree/blob objects with
replacement semantics disabled. Local archive attributes cannot alter that
snapshot; member set, file types/modes and blob contents are verified against
the raw reviewed objects before DNS/SSH contact. It replaces the Ansible environment/HOME,
and creates the effective single-host inventory itself. Untracked/ignored
variable files and ambient `ANSIBLE_*` configuration cannot alter the invocation.
No CLI pass-through, extra-vars escape hatch, tags, task skipping, alternate
playbook or alternate vars file is supported. Raw `ansible-playbook` mutation is
outside the formal procedure. Syntax/list/component checks below remain local.

Every mutating entry rechecks stage-specific guest Data identity before role
mutation, then the existing runtime gates check their own consumption boundaries.
The final preflight also checks the actual Data mountpoint's backing identity:
an unused mountpoint is allowed, but a foreign or ambiguous occupant stops before
S3A. This read-only check does not create the guard or perform bind mounts; the
later S3B guard remains in place.
Storage inspection tools must be present for the initial read-only check; absent
or uninspectable evidence is STOP, never permission to install/repair first.
Boot/root ancestry, topology, signatures and UUID/state remain guest-local
identity authority. Provider object IDs are not universal deployment identity.
No provider discovery or object selection occurs in the wrapper.

Use a new restricted evidence directory outside Git for each run. It contains
input snapshots, effective inventory, accepted public host key binding, resolved
locator observation, validated Layer-2 JSON, Ansible output and exit status.
Bootstrap output records the exact rendered bootstrap configuration and its
SHA256; final output records SHA256 for every explicit non-secret rendered
configuration file and an ordered aggregate before service activation. Guest
Data evidence and its SHA256 are also recorded. Configuration hashing never
walks secret directories or includes the key path. Preserve partial/failed output
as partial evidence. Only sanitized hashes/counts/provenance belong in public
Issues; the original identities and rendered configuration remain restricted.
All `run` input files (manifest, locator and grant/provider record) must be
operator-owned regular files with mode `0600` outside the controller checkout;
symlinks are rejected. Keep these durable records outside temporary storage.

## First provision: separately authorized destructive Data bootstrap

The lifecycle is explicitly:

```text
bootstrap manifest -> validate/hash -> exact Human destructive grant
-> bootstrap-data.yml -> generated UUID -> independent UUID verification
-> Human/operator records UUID -> complete final manifest
-> validate final manifest -> provider record/guest join/confirmation -> ordinary site.yml
```

Bootstrap is a distinct, default-disabled operation, never a deployment retry or
recovery procedure. Confirm Human authorization for the exact selected blank
Data volume, its pinned host key, by-id, serial and geometry. The reference size
comes from source. Absence of a UUID does not authorize formatting. Bootstrap
does not require final public addresses, Tunnel ID or subordinate-ID state.
Provider evidence is required only if needed externally to disambiguate the
selected destructive target; an ambiguous provider/guest attribution is STOP.
Do not invent an additional bootstrap public-address field or equality gate.

The Human-issued grant is a separate non-secret YAML file with exactly:

```yaml
authorize: format-blank-data
source_revision: REPLACE_WITH_MANIFEST_SOURCE_REVISION
manifest_sha256: REPLACE_WITH_ACCEPTED_BOOTSTRAP_MANIFEST_HASH
inventory_sha256: REPLACE_WITH_ACCEPTED_LOCATOR_HASH
host_key_sha256: REPLACE_WITH_ACCEPTED_MANIFEST_HOST_KEY
data_device_id: REPLACE_WITH_EXACT_SELECTED_MANIFEST_DEVICE
```

This must record an actual Human grant, not an agent-generated authorization.
Only after obtaining it, under the separate host-operation authorization:

```sh
./scripts/deploy run bootstrap --manifest "$BOOTSTRAP_MANIFEST" --inventory "$LOCATOR" \
  --manifest-sha256 "$BOOTSTRAP_SHA256" --inventory-sha256 "$LOCATOR_SHA256" \
  --bootstrap-grant "$HUMAN_GRANT" --evidence-dir "$NEW_BOOTSTRAP_EVIDENCE"
```

The wrapper requires exact grant/candidate/target equality and supplies only the
existing boolean destructive enable in addition to validated Layer-2 values.
The existing bootstrap verifier rechecks blank inactive whole-disk identity and
performs one ext4 format with 10% root reserve. It does not mount or start
services. Independently verify the generated UUID on the authenticated host's
selected device; Human/operator then records it in a **new final manifest** and
completes the remaining instance inputs. Different manifest bytes produce a new
SHA256. Failure, partial completion, missing/ambiguous UUID output or unknown
outcome is STOP for fresh read-only investigation, never automatic retry, repair,
signature erasure or reformat. Check mode is refused.

## Final-stage Cloud Shell observation and guest join

Before every final invocation, use the selected instance's existing OCI Cloud
Shell console session. Do not install OCI credentials or an OCI CLI on the
controller. The helper uses only the console CLI's default authentication and
six read-only calls; each list uses `--all`. It requires one RUNNING instance,
one ATTACHED VNIC attachment, an AVAILABLE VNIC, one primary RFC 1918 private
IPv4, its bound public IPv4 and one AVAILABLE public GUA IPv6. Unexpected state,
multiplicity, missing data or CLI failure emits `OBSERVE STOP` and no record.
These source checks and synthetic tests are not provider/runtime evidence.

1. On the reviewed controller checkout, obtain the exact reviewed source SHA and
   SHA-256 identities for the helper and its repository-local, standard-library
   public-address predicate. Transfer these **non-secret source identities** to
   Cloud Shell through the approved channel; do not derive trust from downloaded
   code alone:

   ```sh
   git rev-parse HEAD
   sha256sum scripts/oci-observe ansible/module_utils/mcp_network.py
   ```

   In Cloud Shell, obtain the source at that exact SHA (the variables below are
   those reviewed identities, not a branch tip):

   ```sh
   git clone --no-checkout https://github.com/r-agatsuma/mcp-remote-workspace.git "$OBSERVE_SOURCE"
   git -C "$OBSERVE_SOURCE" checkout --detach "$REVIEWED_SHA"
   cd "$OBSERVE_SOURCE"
   test "$(git rev-parse HEAD)" = "$REVIEWED_SHA"
   printf '%s  scripts/oci-observe\n%s  ansible/module_utils/mcp_network.py\n' \
     "$REVIEWED_HELPER_SHA256" "$REVIEWED_NETWORK_SHA256" | sha256sum --check --strict -
   ```

   Stop on any mismatch; run the helper only after these checks pass. The helper
   must stay in this source tree so it can import the pure predicate. It requires
   Python 3's standard library only, with no controller/Ansible dependencies.
2. Still in Cloud Shell, run it for the explicitly selected instance:

   ```sh
   ./scripts/oci-observe "$INSTANCE_OCID"
   ```

   A successful stdout is exactly one canonical provider-record v1 JSON object.
   A failure is STOP, never permission to select another instance or add credentials.
3. Relay that exact successful stdout, including its trailing LF, to a durable
   private **0600** regular file `$PROVIDER_RECORD` on the controller, outside
   every checkout and temporary storage. Use an approved private channel; the
   OCID and addresses must not enter Git, public Issues or diagnostic bundles.
   The parser rejects unknown/missing keys, duplicate keys, non-UTF-8, BOMs,
   inputs over 64 KiB, invalid addresses/counts/lifecycles and future UTC times.
   No signature or universal TTL is claimed.
4. From the clean reviewed controller checkout, with a final locator containing
   only `port` and `user`, run:

   ```sh
   ./scripts/deploy run final --manifest "$FINAL_MANIFEST" --inventory "$FINAL_LOCATOR" \
     --provider-record "$PROVIDER_RECORD" --evidence-dir "$NEW_FINAL_EVIDENCE"
   ```

   The wrapper connects directly to the record's public IPv4, matches the
   accepted SSH fingerprint and proves possession with `/usr/bin/true`. Over
   those same pinned options it runs the read-only command
   `/usr/sbin/ip -j addr show scope global`. Across all non-loopback interfaces,
   the global IPv4 set must equal the record's primary private IPv4 singleton,
   and the global IPv6 set its GUA singleton. Any extra/missing address,
   malformed JSON or command failure stops before Ansible mutation.
5. Read the record facts, matched SSH fingerprint and guest-join result shown
   by the interactive tty prompt. In the OCI console, for the VNIC of **this same
   instance**, check **every Security List of its subnet and every NSG attached
   to the VNIC**: there must be no ingress rule admitting TCP (protocol TCP or
   All) from an IPv6 source. Only after this console check, answer exactly `y`.
   Every other answer, EOF or non-tty stdin stops before mutation. Stop if the
   record or attribution is no longer current; timestamps/hashes do not grant authority.

The manifest forbids address inputs. The entry alone injects `mcp_host_public_ipv4`
and `mcp_host_gua_ipv6` into Ansible after the join/confirmation. Private evidence
adds the exact consumed `provider-record.json`, exact guest response
`guest-join.json`, and `confirmation.json` (answer and UTC time). `binding.json`
contains `provider_record_sha256` and `guest_join_sha256` as audit identities of
those bytes, not authority. The OCID appears only in private evidence and the
local confirmation display; it is not passed to Ansible.

No target/provider/Cloud Shell execution or deployment is authorized by source
validation. Runtime proof belongs to #95 (I1) and #190 (I2); each requires its own
execution contract and authorization.

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

The v0 credential order is: confirm `/etc/mcp-tunnel` is a real directory
`root:mcp-tunnel 0750` → the Human stages the new credential → first final entry
→ same-entry reapply (with a new observation and evidence directory). If the
directory/account prerequisite is absent, STOP for the separately authorized
prerequisite work; do not use a failed first entry to establish the staging
order. Wrong existing state must be investigated through that staging process;
do not use blind permission repair or read the key to diagnose it. Verify metadata without
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
under the separate host-operation authorization and the provider/guest/console
confirmation gate above, from the reviewed repository root:

```sh
./scripts/deploy run final --manifest "$FINAL_MANIFEST" --inventory "$FINAL_LOCATOR" \
  --provider-record "$PROVIDER_RECORD" --evidence-dir "$NEW_FINAL_EVIDENCE"
```

The fixed sequence is S3A application/identities, S3B verified Data/runtime,
S3C unattended user manager, S3D network/DNS, then S3E canonical-image convergence
and services. This entry never
imports destructive bootstrap. It verifies the selected filesystem/UUID and
mounts Data; missing or wrong Data never falls back to formatting or Boot.
Application installation uses `/usr/local/bin/mcp-workspaced` as root:root 0755.
Production services are enabled and started, or restarted when the binary or
service configuration changes. This is a host mutation, not a static command.

S3E checks the configured exact image locally as the final runner, using the
same Data HOME/XDG, runtime/bus and TMPDIR as the service. It verifies rootless
store paths and immutable inspected identity before any service start/restart.
Present state does not pull and reports no acquisition change. Absent state
acquires only the configured digest, verifies its repository/digest association
and immutable local ID, and reports changed; repeat convergence verifies locally.
Old canonical images, external base images and shared cache are retained.

The acquisition context uses an explicit empty authfile and client certificate
directory; inherited auth/proxy/remote overrides are discarded. Ambient registry
credential helpers are rejected. The existing S3D resolver/egress boundary is
used without exceptions. Acquisition/inspection failure stops before service
activation. Private-registry credentials or policy weakening require a separate
Human decision; never bypass this gate with login, a proxy or runtime pulling.
See [S3E mechanism and identity checks](../ansible/README.md#s3e-production-services-and-trusted-recovery).

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
and canonical image. Set `mcp_source_revision` in the final manifest to the exact
accepted commit, validate/re-hash the candidate, refresh final-stage observations
and run the formal ordinary entry from the matching clean reviewed deployment
checkout. Observe local startup again. No implicit journal migration or canonical-image switch is
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
S3B environment, S3C manager, S3D planner outputs and S3E image/service/recovery/Tunnel
renders using two synthetic identities, ports and resolver orders. Independent
fixed path/account anchors and the actual install task guard against shared
mistakes; existing S3B–S3E tests cover negative gates, secret metadata and bounded
recovery actions. Source inventory checks protect bootstrap separation and the
single authority/service/runner boundary. These are drift checks, not an exhaustive
secret detector or a replacement for independent source review and manual
redaction review of changed docs/configuration/examples.

The #141 image tests use mocked Podman responses and synthetic Ansible command
results. They establish source behavior for present/absent/repeat convergence,
identity/store/credential rejection and service-task ordering. They do not
establish real image fetches, registry credential isolation, Podman multiarch
inspection behavior or deployed network/DNS effectiveness. After independent
review and landing, separately authorized #95 must verify the composed path on
the dirty target: exact image absent in the final runner store, reviewed Ansible
acquisition through deployed S3D DNS/egress, immutable identity verification,
service activation, canonical `workspace_create()` without a runtime pull and
the intended container image binding. Repeat convergence must show no reacquisition.

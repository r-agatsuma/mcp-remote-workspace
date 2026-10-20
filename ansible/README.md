# S3A Ansible foundation

This tree targets Ubuntu 24.04 on arm64. It creates the unprivileged
`mcp-runner` and `mcp-tunnel` system accounts, then installs the application
from an explicitly selected repository commit. It does not configure runtime
storage, user managers, networking, a production service, or Tunnel credentials.

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
locked passwords, and do not create home directories.

Before invoking Git and again before building, the role requires the source
checkout path and its `/usr/local/src` parent chain to be root-owned and not
group/world writable. It checks every checkout entry and tests effective
writability as both service users, including ACL effects. Unsafe pre-existing
paths fail closed; S3A does not chmod/chown them into compliance. It creates
`/usr/local/src` as `root:root` mode `0755` only if that parent is absent.

The role records binary build metadata with `go version -m` and program/dynamic
headers with `readelf`; it makes no static linkage or portability claim. Source
checks do not prove host convergence or runtime behavior.

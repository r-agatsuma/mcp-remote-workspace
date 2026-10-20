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
ansible-playbook -i inventory/hosts.example.yml site.yml --check --diff
```

The second command is a controller-side dry run only when an inventory has
been configured to reach an appropriate host; it is not required for source
validation and does not establish real-host convergence.

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
derived from the created account's passwd record. `mcp_runner_account` and
`mcp_tunnel_account` expose the account names. Later roles can consume these
facts directly; S3A does not assign fixed numeric IDs. The account records use
`/usr/sbin/nologin`, have locked passwords, and do not create home directories.

The role records binary build metadata with `go version -m` and program/dynamic
headers with `readelf`; it makes no static linkage or portability claim. Source
checks do not prove host convergence or runtime behavior.

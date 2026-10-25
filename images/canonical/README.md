# Canonical workspace image

This is the #148 candidate-image source for
`ghcr.io/r-agatsuma/mcp-workspace-base`. #144 owns production digest selection;
neither this directory nor its workflow changes Ansible inputs.

## Image contract and provenance

- Supported platform: **linux/arm64 only**, built/tested natively. No amd64 or
  emulation prerequisite is introduced.
- Base: Docker Official Image `docker.io/library/ubuntu:24.04`, pinned directly
  to arm64/v8 manifest
  `sha256:18b33ff3829707f31ada654a963b9ee35f2e6f0241b964bd17a22a9b330cb63d`.
  At selection, its upstream index was
  `sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac`.
  The Dockerfile uses the platform manifest, not the index or tag resolution.
  [Official Ubuntu image source](https://github.com/docker-library/official-images/blob/master/library/ubuntu),
  [Ubuntu OCI source](https://git.launchpad.net/cloud-images/+oci/ubuntu-base).
- Final image metadata explicitly sets user **`0`** and workdir **`/workspace`**.
  No custom ENTRYPOINT/CMD is added. Inherited Ubuntu CMD is not liveness
  authority: the landed runtime explicitly supplies `/usr/bin/sleep infinity`.
- Distro packages use `--no-install-recommends`; `fd` is an alias for `fdfind`.
  No service manager, SSH server, sudo or nested container engine is installed.
  Package versions follow Ubuntu repositories at build time; pinning the base
  does not make apt installation reproducible. Every build records its package
  versions and copyright-file hashes.
- Repository source is Apache-2.0: see [LICENSE](../../LICENSE). Ubuntu and its
  packages have their own licenses, including GPL/LGPL and other licenses;
  Apache-2.0 does not describe the aggregate image. The image retains
  `/usr/share/common-licenses` and `/usr/share/doc/*/copyright`. Inspect those
  files for the actual package versions selected by #144. See also the
  [upstream Ubuntu image licensing information](https://hub.docker.com/_/ubuntu)
  and [Canonical policy](https://canonical.com/legal/intellectual-property-policy).
  No distro copyright/license files are stripped.

## Workflow modes and evidence

`canonical-image.yml` uses native `ubuntu-24.04-arm` runners. Its Actions are
SHA-pinned; checkout does not persist credentials.

| Event | Behavior | GHCR authority |
| --- | --- | --- |
| Relevant pull request | Offline source checks, build, smoke | None |
| Relevant push to main | Build, smoke, transfer exact tested image, publish candidate | Publish job only: `GITHUB_TOKEN`, `packages: write` |
| Weekly Monday 04:23 UTC | Fresh uncached build, same smoke, discard image | None |
| Manual dispatch | Build, smoke, discard image | None |

Only main push transfers a Docker archive to the dependent publish job (one-day
retention). No privileged job checks out PR code. Discovery tags are
`sha-<source revision>` and are not production identity. Published artifacts are
single arm64 platform manifests, not multi-platform indexes. BuildKit SBOM and
provenance attestations are disabled to avoid adding an unvalidated index shape.

The run summary and `canonical-validation-evidence` / `canonical-publication-evidence`
artifacts record source/run identity, exact Dockerfile/hash, pinned base,
user/workdir/labels/platform, package versions, copyright hashes, smoke result,
push output, registry manifest and exact GHCR digest. Metadata evidence is retained
30 days; #144 must preserve the accepted run/digest evidence in its decision record.
No registry token or Docker auth directory is uploaded or baked into the image.

## Local validation

`./scripts/verify-source` includes offline Dockerfile-parser, workflow YAML,
trigger/permission, shell syntax, package-baseline and bounded secret-material
regressions, plus existing deployment digest-identity checks. It does not access
GHCR or require a container engine. `actionlint .github/workflows/canonical-image.yml`
can additionally check the full Actions schema if actionlint is installed.

On the arm64 developer VM, use the required **host** rootful Podman invocation:

```sh
revision=$(git rev-parse HEAD)
sudo podman build --platform linux/arm64 --no-cache \
  --build-arg "SOURCE_REVISION=$revision" \
  --tag localhost/mcp-workspace-base:issue-148 images/canonical
python3 -B images/canonical/smoke.py --revision "$revision" \
  --evidence /tmp/issue-148-image-evidence -- sudo podman localhost/mcp-workspace-base:issue-148
```

The smoke runs with network disabled, checks the runtime-supplied sleep process,
tools, package absence, bounded credential locations, metadata and writable
`/usr`/`/etc`. It removes only its own exact smoke container. A rootful local image
build/smoke does **not** establish production rootless isolation, UID mapping,
network/cgroup/security behavior or #95 composed-runtime PASS.

## First publication and Human checkpoint

This branch/PR cannot publish. After independent review and separately authorized
landing, the main workflow publishes the first candidate with `GITHUB_TOKEN`.
The source OCI label associates it with this repository. A failed package-name,
repository-linking or token authorization must be returned to the coordinator;
do not substitute a PAT, package name, registry or administration authority.

The Human package administrator must make the initially private package **Public**.
Then verify exact-digest anonymous retrieval without login/helper state in a
separately bounded local validation (not ordinary source checks):

```sh
# Replace only with the exact candidate digest recorded by the main workflow.
candidate='ghcr.io/r-agatsuma/mcp-workspace-base@sha256:<64 lowercase hex>'
# An explicitly empty auth file excludes local registry credentials/helpers.
authdir=$(mktemp -d /tmp/canonical-anonymous.XXXXXX)
printf '{"auths":{}}\n' > "$authdir/auth.json"
sudo podman pull --authfile "$authdir/auth.json" --platform linux/arm64 "$candidate"
```

Record the candidate, run, Public setting and anonymous retrieval result before
#144 accepts it. Do not select production identity here. No #95, target-host
mutation, production pull, Tunnel staging or merge is authorized by these checks.

# Lifecycle core (P2a)

`internal/workspace` contains the host-side lifecycle service and a narrow Podman
CLI adapter. The existing MCP smoke server remains tool-free. No product ingress,
authentication transport, exec/files or build is introduced. P4a adds reusable
TTL and resource/backend/storage admission on this substrate; see
[the admission contract](admission.md) for current policy and recovery behavior.

## Identity and authority

`IdentityFromTrustedMeta` must receive host-attested `_meta["openai/subject"]` and
`_meta["openai/session"]`. Both must be nonempty strings. Unverified wire metadata
must never be passed directly to this boundary; there is deliberately no listener
or tool handler doing so. Caller workspace/user/session arguments and
`Mcp-Session-Id` do not contribute to identity.

W is a domain-separated SHA-256 hash of the length-prefixed subject/session pair.
The raw values are discarded. G is a fresh cryptographically random 256-bit
identifier for each candidate. `WorkspaceKey` and `Generation` are distinct Go
types. `Exact{Workspace, Generation}` is required by every runtime create/destroy
and delayed cleanup/publication operation.

Container names have the exact form `mcpws-v1-<W>-<G>`. Ownership requires matching
`io.mcp-workspaced.owner`, `.workspace`, `.generation` and `.image` labels, a full
container ID and the configured digest-pinned image. Unknown labels in the service
namespace, inconsistent names/labels, unrecorded generations and a different
canonical image cause reconciliation to fail closed. Unrelated containers are
ignored; service lookalikes are never adopted or cleaned up automatically.

## State and failure behavior

The only public states are ABSENT and PRESENT. The durable journal records exact
generation intent before create can mutate the runtime. A partial, failed or
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
SIGINT/SIGTERM. There is no network admission in this skeleton.

The adapter runs without a shell, explicitly uses `--remote=false`, drops remote
connection environment variables and checks `info --format=json` for rootless
mode. Production execution requires a non-root service account. Discovery uses
JSON list/inspect output. Create uses only the canonical image, never pulls, does
not mount host paths or engine sockets, disables implicit image volumes and uses
no privileged/nested engine. This slice uses no network, drops container
capabilities and enables no-new-privileges, and applies the configured P4a resource limits. Process injection remains a
separate trusted runtime capability; see the admission contract.
Start/remove operate on full immutable container IDs resolved and validated for
exact W/G; neither logical-key deletion nor bulk/prune operations exist. Output
is bounded and commands honor context cancellation. No Podman/libpod SDK is used.

Local tests exercise fake runtimes and stub commands only. Actual rootless Podman
behavior, CLI compatibility and the bounded pre-P4 host envelope are the external
verification scope of Issue #69.

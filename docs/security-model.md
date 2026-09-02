# Security Model

The project intentionally separates the unprivileged sandbox sidecar from the
privileged node mount path.

## Trust Boundary

The mounter sidecar is treated as an untrusted client:

- it carries a projected service account token;
- it sends requests over a Unix domain socket;
- it does not receive `SYS_ADMIN`;
- it is not privileged;
- it does not mount host `/proc`;
- it does not access the upstream CSI socket.

The wrapper DaemonSet is the only component allowed to perform node-side mount
operations. It must run with the node permissions needed by the selected mount
namespace implementation.

## Socket Exposure

The Unix domain socket is not a pod-wide network endpoint. It is a filesystem
object and should only be mounted into the mounter sidecar. Workload containers
should not receive:

- the wrapper socket hostPath volume;
- the projected token with the wrapper audience;
- generated mount configuration that contains PV names or mount targets.

With that layout, a workload container cannot call the wrapper just by knowing
the socket path. It has no mount for that path and no matching projected token.
The wrapper checks below still exist as defense in depth for configuration
mistakes, sidecar bugs, and compromised low-privilege clients.

## Request Validation

For every mount request the wrapper:

1. requires the configured `driver_name`;
2. validates the bearer token with Kubernetes TokenReview and the configured
   audience;
3. requires the TokenReview identity to be the live Pod's service account and
   requires the token's bound Pod name and UID to match the request exactly;
4. fetches the live Pod and checks namespace, name, UID, node, phase, and target
   container;
5. fetches the PV and checks `spec.csi.driver`;
6. evaluates the PV's namespace and service account annotation allowlists;
7. validates the NFS source, request subpath, and target path denylist;
8. requires an additional capability key if the effective source is the NFS
   export root and the wrapper was started with root-key protection; and
9. refuses to continue when the target path is already a mount point.

The request body does not contain NFS server credentials. NFS mount source data
is derived from the Kubernetes PV object on the wrapper side. Workload
containers should not be able to read PV objects through Kubernetes RBAC, and
the mounter should receive mount configuration from the sandbox runtime CSI
invocation instead of accepting arbitrary PV choices from the workload.

The wrapper does not fetch a PVC and does not use the PV's `spec.claimRef` as
an authorization boundary. This is intentional so one PV can be shared across
namespaces, but it also means a bound PV is not protected by its claim
namespace. Per-PV access is provided only by the annotation policy described
below, in addition to the socket, exact-Pod token, driver, node, container, and
path checks.

For the dynamic path, the sandbox runtime invokes the mounter with a CSI
`NodePublishVolumeRequest`. The workload container does not receive the wrapper
socket or projected wrapper token.

## Exact Pod-Bound Token

The configured audience prevents a token intended for another recipient, such
as the Kubernetes API server, from being replayed to the wrapper. Audience is
not storage authorization by itself. The wrapper additionally requires
TokenReview to return exactly one non-empty
`authentication.kubernetes.io/pod-name` and
`authentication.kubernetes.io/pod-uid` value, and both values must match the
requested and live target Pod. The token username must also match that Pod's
namespace and service account.

This prevents one Pod from using a token belonging to another Pod that happens
to use the same service account. A legacy or otherwise unbound service account
token that does not expose the bound Pod name and UID is rejected.

## PV Annotation Policy

The wrapper recognizes two PV annotations:

```yaml
kary.dev/allow-namespace: "sandbox-a, sandbox-b"
kary.dev/allow-serviceaccount: "runtime, workspace-agent"
```

Each annotation is an independent comma-separated allowlist. Entries are
trimmed and compared exactly and case-sensitively with the live Pod namespace
or bare `spec.serviceAccountName`. When both annotations are present, both must
match. A missing annotation makes only that dimension unrestricted; if both are
missing, the PV policy itself is unrestricted.

An explicitly empty value, an empty item such as `a,,b` or `a,`, and `*` are
invalid and cause authorization to fail. There is deliberately no wildcard;
operators express an unrestricted dimension by omitting its annotation.

Because missing annotations default to unrestricted and `claimRef` is ignored,
every existing PV without these annotations becomes selectable across
namespaces by any caller that passes the other wrapper checks. Kubernetes RBAC
for creating or patching PVs and these annotations is therefore part of the
storage security boundary.

Annotation changes apply to future mounts. They do not actively revoke or
unmount a mount that already exists in an unchanged container namespace. If the
container ID changes, reconciliation reloads the live PV and applies the current
annotation policy before restoring the mount. Explicit unmount uses exact-Pod
authentication and a matching saved desired-mount record, but intentionally
does not re-evaluate current PV annotations, so revocation cannot prevent
cleanup.

## NFS Export-Root Capability

`source_sub_path` is relative to the PV root, while the PV CSI `subDir` is
relative to the configured NFS share. The wrapper treats a request as an NFS
export-root mount only when both values normalize to empty:

| PV CSI `subDir` | Request `source_sub_path` | Effective source | Additional key check |
| --- | --- | --- | --- |
| empty | empty | NFS export root | only when the wrapper key is configured |
| empty | non-empty | request directory below export root | no |
| non-empty | empty | PV root below the export | no |
| non-empty | non-empty | request directory below the PV root | no |

This is a lexical policy over the trusted PV CSI fields. It cannot detect an
NFS-server-side symlink or alias whose non-empty `subDir` resolves back to the
export root. Treat control of PV `server`/`share`/`subDir` and the NFS namespace
as storage-administrator authority; the capability key is not a defense against
a malicious PV or NFS administrator.

The wrapper reads the capability from `WRAPPER_EXPORT_ROOT_KEY_FILE` or
`--export-root-key-file` at startup. The trimmed value must contain 32 through
4096 visible ASCII characters and should be generated randomly. The authorizer
retains only its SHA-256 hash in memory for constant-time comparison; the
plaintext is not added to mount state.
If the option is absent, the wrapper applies no additional root-key check:
export-root mounts still require all exact-Pod, live-Pod, PV annotation,
driver, container, and path checks. If the option is present, a root request
with a missing or different key is denied. The key never bypasses another
authorization check. Changing the file requires restarting the wrapper because
it is a startup credential.

An authorized mounter uses `EXPORT_ROOT_KEY_FILE` or
`--export-root-key-file`; the Go SDK uses `Config.ExportRootKeyFile`. The client
reads the file for each mount and places the value only in the
`X-Kary-Export-Root-Key` HTTP header over the Unix socket. The key is neither a
JSON request field nor an unmount credential, and clients must not log it.

For Helm deployments, `wrapper.exportRootKeySecret.name` and
`wrapper.exportRootKeySecret.key` select an existing Secret in the wrapper
DaemonSet namespace (the release namespace unless `namespace` overrides it).
The chart mounts that key read-only into the wrapper with mode `0400`; it does
not create the Secret, read one from another namespace, or distribute it to
mounter clients. An empty Secret name configures
annotation-only root authorization. When a Secret is selected, only clients
explicitly trusted to mount the NFS export root should receive the same value.

## Source SubPath Creation

The wrapper normally requires a non-empty `source_sub_path` to exist. v0.0.2
adds an opt-in node policy, `WRAPPER_CREATE_MISSING_SUBPATHS=true`, that creates
missing directory components below the staged PV. It is disabled by default, so
upgrading does not add NFS write behavior unless an operator enables it.

The switch is global to a wrapper process. Every caller that passes the
existing Pod and PV annotation checks can create any valid relative path inside
that PV. v0.0.2 does not enforce an allowed subpath prefix or a separate
"may create" permission, and a misspelled path can leave a persistent empty
directory. Only trusted runtimes or sidecars should receive both the wrapper
socket and projected token.

Creation does not relax path validation. The path must remain relative, cannot
contain `..` or NUL, and every existing component is opened without following
symlinks and must be a directory. Newly created components request
`WRAPPER_CREATED_SUBPATH_MODE` (default `0770`) through `mkdirat`, subject to
the wrapper process umask and filesystem or NFS default ACLs. Existing
components keep their owner and mode, and the wrapper does not `chmod` or
`chown` new components.

This policy adds a write to the NFS export and therefore needs an explicit
permissions review. With `root_squash`, node-side root may be mapped to an
anonymous UID/GID: creation can fail, or a successfully created directory can
have ownership that prevents the workload from entering it. Pre-provision the
directory with the intended UID/GID when ownership is part of the security
boundary. Avoid widening the mode merely to bypass an unverified export,
umask, default ACL, owner, or group configuration.

## Container Restart Reconciliation

After the initial authenticated mount succeeds, the wrapper stores only the
normalized Pod/PV/container/target mount intent and the mounted container ID in
its node-local state directory. In state format v2,
`export_root_authorized` means an export-root intent passed the complete policy
in effect at mount time. Annotation-only root mounts set that marker with an
empty fingerprint; key-protected root mounts additionally store the
authorizing key's SHA-256 fingerprint. Each desired mount has its own `0600`
file, so updates do not rewrite every Pod's state. The state does not persist
the projected bearer token, plaintext export-root key, NFS credentials, or a
caller-supplied NFS server.

Legacy state v1 has no root/non-root classification and is represented as
unknown in memory. When a replacement container triggers reconciliation, a
wrapper without a key uses the current live PV plan and policy to backfill the
classification and may restore the intent. With a wrapper key, a legacy intent
whose current live plan is the export root fails closed; the exact Pod must
call `Unmount` and then issue a valid keyed `Mount`. A legacy intent whose live
plan is non-root does not need the key. For
ordinary state v2, a changed mount intent or root/non-root classification for
the same target fails closed and requires `Unmount` followed by `Mount`.

The state format remains v2, so v1.1.0 can parse files written by v1.1.1.
However, v1.1.0 requires a fingerprint for every authorized root intent and
therefore rejects, and may remove during reconciliation, v1.1.1
annotation-only root state. Explicitly unmount those intents before a
downgrade; do not rely on rollback to preserve their automatic restoration.

In key-protected mode, reconciliation restores an export-root intent only when
its state is authorized and its fingerprint matches the current wrapper key.
An older annotation-only root intent therefore does not automatically restore
after key protection is enabled. A trusted caller holding the current key can
repeat the same mount request to upgrade or refresh state without stacking a
new mount. Removing the wrapper key switches back to annotation-only mode;
after live authorization succeeds, the wrapper can restore an authorized root
intent and clears any stale fingerprint. Adding, rotating, or removing the key
does not actively unmount an existing Linux mount. Wrapper and client Secret
updates must be coordinated because the wrapper reads only at startup while
clients read before each mount.

The wrapper runs one `SharedIndexInformer` filtered by its own node name. The
informer owns LIST/WATCH cache synchronization, resource-version handling, and
reconnection; the wrapper does not issue periodic GET requests for every saved
Pod. When an informer event reports a different container ID for the same Pod
UID and container name, the wrapper validates the live Pod, PV, driver, and PV
annotation policy again before mounting into the replacement container
namespace. An export-root plan follows the current wrapper mode:
annotation-only mode requires the saved root-authorization marker but no
fingerprint, while key-protected mode additionally requires a fingerprint
matching the current key. A deleted, terminal, UID-mismatched, or different-node
Pod makes the saved intent stale and removes it.

Explicit unmount first requires exact-Pod authentication and a saved intent
whose Pod UID, container, target, and PV match. It does not fetch the PV or
re-evaluate annotations, so a later annotation revocation cannot block cleanup.
The saved container ID must also match the live target container. If the
container has changed, the wrapper removes only the stale intent and does not
touch a same-path mount in the replacement namespace.
The wrapper deletes the intent before unmounting and restores it if the node
operation fails, preventing the reconciler from recreating a successfully
removed mount. If no matching intent exists, unmount is idempotently successful
and does not touch an unrelated mount point.

Reconciliation is event-driven but still eventually consistent. The target path
is absent between container creation and handling its Pod status event, so
applications that require the mount before their first instruction still need a
runtime startup gate or retry behavior.

## Driver Name

The default driver name is:

```text
csi.nfs.zhida
```

It is configured through `DRIVER_NAME` or the chart value `driverName`. Do not
hard-code environment-specific driver names in chart, demo, mounter, or wrapper
code.

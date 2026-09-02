# kruise-agents-nfs-csi

`kruise-agents-nfs-csi` adds low-privilege, on-demand NFS mounts to
OpenKruise runtimes and other trusted Kubernetes sidecars while preserving the
upstream `nfs.csi.k8s.io` provisioning behavior.

A trusted mounter client sends an authenticated request over a Unix domain
socket. The node wrapper validates the projected service account token and the
exact live Pod identity, applies the selected PV's annotation policy, then
performs the mount in the target container's mount namespace. The client itself
does not mount filesystems, receive `SYS_ADMIN`, mount host `/proc`, or access
the CSI socket.

The default CSI driver name is `csi.nfs.zhida`. Set the same `DRIVER_NAME` in
the wrapper and mounter when a deployment uses a different name.

## What's new in v1.1.0

- Dynamic mounts no longer require a PVC or use the PV `claimRef` as an
  authorization boundary. PVs can instead restrict access with optional
  namespace and service-account annotation allowlists.
- Projected service account tokens must be bound to the exact target Pod name
  and UID, in addition to matching the configured audience.
- Mounting an effective NFS export root requires an optional startup capability
  key. The key is sent only over the wrapper UDS and is never persisted.
- Desired-mount state now preserves export-root authorization safely across
  container restarts, and unmount avoids touching an unregistered or replaced
  container target.
- PV `subDir` values are normalized and validated before mount planning.

## What's new in v0.0.2

- The supported integration surfaces are documented as a direct mounter binary
  call, an OpenKruise CSI `NodePublishVolumeRequest` call, and a Go SDK.
- The wrapper can optionally create a missing `source_sub_path` safely before
  mounting it. This is disabled by default for backward compatibility.
- Created directory mode is configurable. The default mode passed to `mkdirat`
  is `0770`; the effective mode is still restricted by the wrapper process
  umask, filesystem or NFS default ACLs, and server policy.

## Architecture

- `cmd/wrapper` runs on each Linux node. It owns TokenReview, Kubernetes object
  validation, path checks, mount namespace operations, and desired-mount state.
- `cmd/mounter` is the `kruise-nfs-mounter` low-privilege command-line client.
- `mounter` is the public Go SDK for trusted runtime and sidecar integrations.
- `internal/node` contains the Linux mount implementation. Non-Linux builds
  return an explicit unsupported error for mount operations.

The wrapper stages an NFS PV only long enough to move or clone the selected
mount into the target container namespace. By default it then unmounts the
staging source (`WRAPPER_UNSTAGE_AFTER_MOUNT=true`). Successful mounts are
stored as root-only node state without bearer tokens or NFS credentials. If a
container ID changes for the same Pod UID, a node-filtered informer triggers
live revalidation and restores the mount in the replacement namespace. State
format v2 records whether an NFS export-root mount was authorized and the
SHA-256 fingerprint of the authorizing key; the key itself is never persisted.

## Prerequisites

All three integration styles require:

- a Linux node running the wrapper and the upstream NFS CSI components;
- a CSI PV whose driver name matches the wrapper configuration and whose PV
  annotations permit the target Pod;
- a trusted runtime or sidecar with the wrapper socket mounted;
- a projected service account token with the configured audience, bound to the
  exact target Pod name and UID;
- Pod namespace, name, and UID from the Downward API; and
- the target container name, or an unambiguous target container.

The wrapper does not require a PVC and does not use `spec.claimRef` for
authorization. A bound PV can therefore be selected from another namespace
when its annotation policy allows the target Pod.

Do not mount the wrapper socket or its projected token into untrusted workload
code. See [Security Model](docs/security-model.md) for the full trust boundary.

## Usage

### 1. Call the mounter binary directly

Use direct mode when a trusted sidecar already knows the PV, optional subpath,
and target path:

```sh
kruise-nfs-mounter mount \
  --driver csi.nfs.zhida \
  --namespace "${POD_NAMESPACE}" \
  --pod-name "${POD_NAME}" \
  --pod-uid "${POD_UID}" \
  --pv "${PV_NAME}" \
  --sub-path users/alice/workspace \
  --target /workspace/data \
  --container main
```

The socket and token default to
`/var/lib/kruise-agents-nfs-csi/wrapper.sock` and
`/var/run/secrets/kruise-agents-nfs-csi/token`. Override them with
`--socket-path` and `--token-file`, or with `WRAPPER_SOCKET_PATH` and
`PROJECTED_TOKEN_FILE`.

See [Standalone mounter](docs/standalone-mounter.md) for the Pod wiring and
unmount example.

### 2. Let OpenKruise invoke the mounter

An OpenKruise runtime can pass its CSI `NodePublishVolumeRequest` protobuf as
base64:

```sh
kruise-nfs-mounter mount \
  --driver csi.nfs.zhida \
  --config "${NODE_PUBLISH_VOLUME_REQUEST_BASE64}" \
  --container main
```

The mounter derives the PV name, target path, and optional source subpath from
the CSI request, then calls the same wrapper API as direct mode. The value of
`--config` is a base64-encoded protobuf message, not JSON. Pod identity should
be injected through `POD_NAMESPACE`, `POD_NAME`, and `POD_UID` or their
projected-file fallbacks.

See [User API](docs/api.md#运行时命令) for accepted CSI context keys and the
matching unmount command.

### 3. Use the Go SDK

Trusted Go runtimes can avoid a child process:

```go
client, err := mounter.NewClient(mounter.Config{
    DriverName:  "csi.nfs.zhida",
    SocketPath:  "/var/lib/kruise-agents-nfs-csi/wrapper.sock",
    TokenFile:   "/var/run/secrets/kruise-agents-nfs-csi/token",
    HTTPTimeout: 15 * time.Second,
})
if err != nil {
    return err
}
defer client.CloseIdleConnections()

result, err := client.Mount(ctx, mounter.MountRequest{
    Namespace:     podNamespace,
    PodName:       podName,
    PodUID:        podUID,
    PVName:        pvName,
    SourceSubPath: "users/alice/workspace",
    TargetPath:    "/workspace/data",
    ContainerName: "main",
})
```

Install the SDK with:

```sh
go get github.com/silver-chard/kruise-agents-nfs-csi/mounter@v1.1.0
```

The SDK uses the same UDS protocol, token rotation behavior, validation, and
reconciliation as the binary. Keep the SDK and node wrapper on the same release
version. See [Go SDK guide](docs/sdk.md) for a complete example and error
handling.

## PV annotation authorization

PV access is controlled independently along the namespace and service account
dimensions:

```yaml
metadata:
  annotations:
    kary.dev/allow-namespace: "sandbox-a, sandbox-b"
    kary.dev/allow-serviceaccount: "runtime, workspace-agent"
```

Each value is a comma-separated allowlist. Surrounding whitespace is ignored,
but matching is otherwise exact and case-sensitive. Service account entries are
bare names, not `namespace/name` values. If both annotations are present, both
must allow the live target Pod. Omitting one annotation leaves that dimension
unrestricted; omitting both allows every namespace and service account that
passes the socket, projected-token, live-Pod, node, driver, container, and path
checks.

An annotation that is present but empty is not the same as an omitted
annotation. Empty list items, including a trailing comma, and `*` are invalid
and fail closed. Use omission, not a wildcard, to make a dimension unrestricted.

This policy replaces the previous PVC/`claimRef` authorization. The wrapper
does not fetch the PVC and does not compare the PV's `claimRef` namespace or
UID. Consequently, an existing PV without either allowlist is available across
namespaces to every otherwise authorized caller. Treat permission to create or
patch these PV annotations as storage-access administration.

Changing an annotation does not immediately unmount an established mount. The
new policy is applied to new mount requests and when a changed container ID
requires reconciliation. Explicit unmount remains available to the exact Pod
for a matching saved desired-mount record even when the PV annotations have
subsequently revoked that Pod.

## NFS export-root capability

The effective NFS source is the export root only when both the PV CSI `subDir`
and request `source_sub_path` normalize to empty. That operation requires the
wrapper's export-root capability key. A PV with a non-empty `subDir` already
represents a directory below the NFS export, so mounting that PV's root does not
require the key. A non-empty request `source_sub_path` likewise selects a path
below the export and does not require it.

This boundary is lexical and assumes the PV and NFS namespace are trusted. The
wrapper evaluates the PV's `server`, `share`, and normalized `subDir`; it cannot
detect an NFS-server-side symlink or alias whose non-empty `subDir` resolves
back to the share root. Anyone who can modify those PV fields or the NFS
namespace must therefore be treated as a storage administrator. Here,
"export root" means the root of the PV's configured `server` + `share`, not the
NFS server's filesystem `/`. To request the PV root, leave `source_sub_path`
empty; `/` remains invalid because request subpaths must be relative.

Configure the wrapper with a key file at startup:

```sh
WRAPPER_EXPORT_ROOT_KEY_FILE=/var/run/secrets/kruise-agents-nfs-csi-export-root/key
# equivalent flag:
kruise-nfs-wrapper --export-root-key-file=/var/run/secrets/kruise-agents-nfs-csi-export-root/key
```

The trimmed key must contain 32 through 4096 visible ASCII characters; use a
random value rather than a human-readable password. The wrapper reads and
hashes it at startup; rotating the file therefore requires a wrapper restart.
If no wrapper key file is configured, all export-root mount requests are
denied.

Clients that are intentionally allowed to mount the export root configure the
same value as a file, rather than placing it in command arguments or request
JSON:

```sh
EXPORT_ROOT_KEY_FILE=/var/run/secrets/kruise-agents-nfs-csi/export-root-key \
  kruise-nfs-mounter mount ...

# equivalent client flag:
kruise-nfs-mounter mount \
  --export-root-key-file /var/run/secrets/kruise-agents-nfs-csi/export-root-key \
  ...
```

The Go SDK uses `Config.ExportRootKeyFile`, for example:

```go
mounter.Config{
    ExportRootKeyFile: "/var/run/secrets/kruise-agents-nfs-csi/export-root-key",
}
```

The mounter and SDK read the file for each mount request and send the key only
in the `X-Kary-Export-Root-Key` HTTP header over the Unix socket. They do not
send it for unmount. The key is not a mount request JSON field and is never
written to desired-mount state. State v2 persists an
`export_root_authorized` boolean and the key's SHA-256 fingerprint.
Reconciliation of an export-root mount requires that fingerprint to match the
wrapper's current startup key. After rotating the key and restarting the
wrapper, an existing mount remains mounted, but its automatic remount is
disabled until a trusted caller repeats `Mount` with the new key; an idempotent
call refreshes the fingerprint without stacking another mount.

The Helm chart consumes an existing Secret; it does not generate one:

```yaml
wrapper:
  exportRootKeySecret:
    name: kruise-nfs-export-root-key
    key: export-root-key
```

The named Secret must exist in the chart release namespace before installation
or upgrade. Its selected value is mounted read-only into the wrapper with mode
`0400`. A trusted mounter that needs this capability must separately mount the
same Secret and set `EXPORT_ROOT_KEY_FILE`; clients that only mount NFS
subdirectories should not receive it.

## Optional creation of a missing SourceSubPath

By default, a non-empty `source_sub_path` must already be a directory in the
PV. v0.0.2 adds an opt-in wrapper policy that creates missing path components
before the bind mount:

| Setting | Environment variable | Wrapper flag | Helm value | Default |
| --- | --- | --- | --- | --- |
| Enable creation | `WRAPPER_CREATE_MISSING_SUBPATHS` | `--create-missing-subpaths` | `wrapper.createMissingSubPaths` | `false` |
| Requested directory mode | `WRAPPER_CREATED_SUBPATH_MODE` | `--created-subpath-mode` | `wrapper.createdSubPathMode` | `0770` |

Example wrapper configuration:

```sh
WRAPPER_CREATE_MISSING_SUBPATHS=true
WRAPPER_CREATED_SUBPATH_MODE=0770
```

The mode is parsed strictly as octal in the range `0001` through `07777`;
`0000` or an invalid value prevents the wrapper from starting. Each `mkdirat`
call uses the requested mode, while the effective mode is subject to the
wrapper process umask and filesystem or NFS default ACLs. Existing components
keep their current owner and mode. The wrapper does not `chmod` or `chown` new
or existing directories, and successfully created parent components are not
rolled back if a later component fails.

Path validation is unchanged: the path must be relative, cannot contain `..`
or NUL, and no existing component may be a symlink or non-directory. A missing
path while creation is disabled, or any unsafe path, returns HTTP `400`. After
authorization succeeds, storage failures such as `EACCES`, `EROFS`, `ENOSPC`,
or `EDQUOT` during creation are node-operation failures and return HTTP `500`.

This is a wrapper-wide policy, not a per-request permission. Every trusted
client that passes the Pod and PV annotation checks can create any valid
relative path in that PV; there is no `allowedSubPathPrefix` authorization in
v0.0.2. A typo can
therefore leave a persistent empty directory. Keep the socket and projected
token limited to trusted runtimes or sidecars.

NFS export permissions still decide whether creation succeeds. With
`root_squash`, the wrapper's root identity is commonly mapped to an anonymous
UID/GID, so creation may be denied or the resulting owner may not match the
workload. Mode `0770` can then leave the workload unable to enter the directory.
Prefer pre-provisioning directories with the intended UID/GID when ownership
matters, or deliberately align export policy, anonymous identity, process
umask, default ACLs, group ownership, and requested mode before enabling
automatic creation.

## Build and validate

Build the two binaries directly from source:

```sh
mkdir -p bin
go build -o bin/kruise-nfs-wrapper ./cmd/wrapper
go build -o bin/kruise-nfs-mounter ./cmd/mounter
```

Or run `scripts/build.sh` to format, test, vet, and build static Linux/amd64
binaries into `dist/linux-amd64`.

Run the project checks:

```sh
gofmt -w ./cmd ./internal ./mounter
go test ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./cmd/wrapper ./cmd/mounter
helm lint charts/kruise-agents-nfs-csi
```

For managed NFS export permissions and `root_squash` troubleshooting, see
[GCP Filestore setup](docs/gcp-filestore.md).

## API and security references

- [User-facing commands and wrapper API](docs/api.md)
- [Go SDK guide](docs/sdk.md)
- [Standalone mounter](docs/standalone-mounter.md)
- [Wrapper protocol](docs/protocol.md)
- [Security model](docs/security-model.md)

# kruise-agents-nfs-csi

This repository starts a lightweight wrapper around upstream
`nfs.csi.k8s.io` for OpenKruise sandbox dynamic mounts.

The design keeps the upstream NFS CSI driver behavior in place and adds a
small node-side agent beside it. The OpenKruise agent-runtime sidecar runs a
low-privilege mounter client. The client talks to the node wrapper through a
Unix domain socket. It never mounts filesystems, never gets `SYS_ADMIN`, never
mounts host `/proc`, and never talks to the CSI socket directly.

Default CSI driver name:

```text
csi.nfs.zhida
```

The driver name is configurable through the same `DRIVER_NAME` setting in the
wrapper, mounter, Docker entrypoint, and demo chart.

The wrapper stages each NFS PV on the node only long enough to clone it into
the target container mount namespace. By default it then unmounts that staging
source (`WRAPPER_UNSTAGE_AFTER_MOUNT=true`). This keeps node mount tables from
growing with the total number of PVs that have ever been dynamically mounted on
the node. Set it to `false` only when intentionally trading node mount-table
growth for repeated-mount reuse of the same staged PV.

## Components

- `cmd/wrapper`: node DaemonSet agent that listens on a Unix socket, performs
  TokenReview, validates live Pod/PV state, rejects dangerous target paths, and
  delegates mount work to the node-side mount implementation.
- `cmd/mounter`: low-privilege sidecar client invoked by sandbox runtime. It
  reads a projected service account token and calls the wrapper socket API.
- `internal/api`: stable JSON request and response types.
- `internal/kube`: minimal in-cluster Kubernetes REST client using only the Go
  standard library.
- `internal/node`: node mount interface. Linux builds contain the initial NFS
  stage and mount-namespace bind implementation; non-Linux builds return an
  explicit unsupported error.
- `demo`: SandboxSet, sandbox inject ConfigMap, PV/PVC, and chart demo files.

## Security Model

The dangerous operations are concentrated in the node DaemonSet wrapper:

- the mounter is not privileged;
- the mounter does not need `SYS_ADMIN`;
- the mounter does not mount host `/proc`;
- the mounter is only an API client over UDS;
- the wrapper validates projected service account tokens with TokenReview;
- the wrapper re-checks Pod and PV state from the apiserver;
- the wrapper re-checks the live PVC bound by the PV claimRef;
- the wrapper only allows the configured CSI driver;
- the wrapper rejects dangerous target paths such as `/`, `/proc`, `/sys`,
  `/dev`, and Kubernetes secret paths;
- if the target path is already a mount point, the wrapper returns an error and
  does not unmount it.

See [docs/security-model.md](docs/security-model.md) for details.

## Cloud NFS Setup

For GCP Filestore or any managed NFS endpoint, see the Chinese setup checklist
in [docs/gcp-filestore.md](docs/gcp-filestore.md). The key requirement is that
the configured NFS share must be writable by the CSI controller node so the
upstream NFS CSI driver can create PVC subdirectories during dynamic
provisioning.

## Local Validation

```sh
gofmt -w ./cmd ./internal
go test ./...
go vet ./...
go build ./cmd/wrapper ./cmd/mounter
```

`internal/node` performs real mount operations only on Linux. Local macOS builds
compile the same wrapper API but return `node mount is only supported on linux`
if the wrapper receives a mount request.

## Images

The wrapper image is built from the upstream NFS CSI image so it keeps the
original `nfsplugin` binary and adds the wrapper agent:

```sh
REGISTRY=iregistry.baidu-int.com/cnap-cluster \
VERSION=0.0.1-beta.12 \
BASE_IMAGE=iregistry.baidu-int.com/baidu-base/ubuntu:resolute \
UPSTREAM_NFS_CSI_IMAGE=iregistry.baidu-int.com/cnap-cluster/nfsplugin:v4.13.2 \
scripts/build.sh
```

The mounter sidecar can be built separately:

```sh
PUSH=1 scripts/build.sh
```

## API

For the user-facing command and wrapper API contract, see
[docs/api.md](docs/api.md). For non-Kruise workloads that call the mounter
directly, see [docs/standalone-mounter.md](docs/standalone-mounter.md).

The wrapper listens on a Unix socket, by default:

```text
/var/lib/kruise-agents-nfs-csi/wrapper.sock
```

Mount requests use JSON over HTTP on UDS:

```json
{
  "api_version": "kruise-agents-nfs-csi.zhida/v1alpha1",
  "driver_name": "csi.nfs.zhida",
  "namespace": "example",
  "pod_name": "sandbox-demo-0",
  "pod_uid": "00000000-0000-0000-0000-000000000000",
  "pv_name": "pv-sandbox-nfs-demo",
  "source_sub_path": "users/alice/workspace",
  "target_path": "/workspace/data",
  "container_name": "main"
}
```

Responses use the project convention:

```json
{"data":{"mounted":true}}
```

or:

```json
{"error":"target path /proc/data is not allowed"}
```

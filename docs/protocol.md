# Wrapper Protocol

Transport: HTTP over Unix domain socket.

Default socket:

```text
/var/lib/kruise-agents-nfs-csi/wrapper.sock
```

## Runtime Mount Command

The sandbox runtime calls the mounter through the CSI-compatible command shape:

```text
kruise-nfs-mounter mount --driver csi.nfs.zhida --config <base64 NodePublishVolumeRequest>
```

The mounter decodes the CSI `NodePublishVolumeRequest`, derives `pv_name`,
optional `source_sub_path`, and `target_path`, then forwards a mount request to
the wrapper over the Unix socket.
Pod identity is provided by injected Downward API environment variables. The
business container does not receive the wrapper socket or projected wrapper
token.

Authentication header:

```text
Authorization: Bearer <projected-service-account-token>
```

The projected token must be bound to the exact target Pod name and UID. A
client that is allowed to mount the NFS export root can additionally send:

```text
X-Kary-Export-Root-Key: <export-root-capability-key>
```

The capability key is an optional HTTP header, not a JSON field. It is ignored
for effective NFS subpaths and required only when both the PV CSI `subDir` and
the request `source_sub_path` normalize to empty. Clients must not log it.

Unmount uses the same exact-Pod identity model, but does not send or require the
export-root key:

```text
kruise-nfs-mounter unmount --driver csi.nfs.zhida --config <base64 NodePublishVolumeRequest>
```

## POST /v1/mount

Request:

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

Success:

```json
{
  "data": {
    "mounted": true,
    "driver_name": "csi.nfs.zhida",
    "pv_name": "pv-sandbox-nfs-demo",
    "source_sub_path": "users/alice/workspace",
    "target_path": "/workspace/data",
    "container_name": "main"
  }
}
```

Failure:

```json
{
  "error": "pv pv-sandbox-nfs-demo uses driver nfs.csi.k8s.io, expected csi.nfs.zhida"
}
```

## POST /v1/unmount

Request:

```json
{
  "api_version": "kruise-agents-nfs-csi.zhida/v1alpha1",
  "driver_name": "csi.nfs.zhida",
  "namespace": "example",
  "pod_name": "sandbox-demo-0",
  "pod_uid": "00000000-0000-0000-0000-000000000000",
  "pv_name": "pv-sandbox-nfs-demo",
  "target_path": "/workspace/data",
  "container_name": "main"
}
```

Success:

```json
{
  "data": {
    "unmounted": true,
    "driver_name": "csi.nfs.zhida",
    "pv_name": "pv-sandbox-nfs-demo",
    "target_path": "/workspace/data",
    "container_name": "main"
  }
}
```

Unmount only acts on a matching saved desired mount. It removes that state
before touching the target namespace. The saved and live container IDs must
match; after a container change, unmount removes only the stale state and does
not touch a same-path mount in the replacement namespace. If the desired mount
or target is already absent, the operation still succeeds without unmounting an
unrelated mount point. PV annotation changes do not block this cleanup path.

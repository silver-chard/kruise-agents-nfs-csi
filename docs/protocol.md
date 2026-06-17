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

The mounter decodes the CSI `NodePublishVolumeRequest`, derives `pv_name` and
`target_path`, then forwards a mount request to the wrapper over the Unix socket.
Pod identity is provided by injected Downward API environment variables. The
business container does not receive the wrapper socket or projected wrapper
token.

Authentication header:

```text
Authorization: Bearer <projected-service-account-token>
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

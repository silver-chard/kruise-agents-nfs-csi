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
3. fetches the live Pod and checks namespace, name, UID, and service account;
4. fetches the PV and checks `spec.csi.driver`;
5. checks that the PV claimRef belongs to the requesting Pod namespace;
6. fetches the live PVC referenced by the PV claimRef and checks name,
   namespace, and UID when present;
7. validates the target path denylist;
8. refuses to continue when the target path is already a mount point.

The request body does not contain NFS server credentials. NFS mount source data
is derived from the Kubernetes PV object on the wrapper side. Workload
containers should not be able to read PV/PVC objects through Kubernetes RBAC,
and the mounter should receive mount configuration from the sandbox runtime CSI
invocation instead of accepting arbitrary PV/PVC choices from the workload.
The wrapper does not read higher-level claim objects. PVC identity is always
derived from the live PV `claimRef`.

For the dynamic path, the sandbox runtime invokes the mounter with a CSI
`NodePublishVolumeRequest`. The workload container does not receive the wrapper
socket or projected wrapper token.

## Driver Name

The default driver name is:

```text
csi.nfs.zhida
```

It is configured through `DRIVER_NAME` or the chart value `driverName`. Do not
hard-code environment-specific driver names in chart, demo, mounter, or wrapper
code.

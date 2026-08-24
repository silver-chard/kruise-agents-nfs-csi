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

## Source SubPath Creation

The wrapper normally requires a non-empty `source_sub_path` to exist. v0.0.2
adds an opt-in node policy, `WRAPPER_CREATE_MISSING_SUBPATHS=true`, that creates
missing directory components below the staged PV. It is disabled by default, so
upgrading does not add NFS write behavior unless an operator enables it.

The switch is global to a wrapper process. Every caller that passes the
existing Pod, namespace, PV, and PVC checks can create any valid relative path
inside that PV. v0.0.2 does not enforce an allowed subpath prefix or a separate
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
its node-local state directory. Each desired mount has its own `0600` file, so
updates do not rewrite every Pod's state. It does not persist the projected bearer token, NFS
credentials, or a caller-supplied NFS server.

The wrapper runs one `SharedIndexInformer` filtered by its own node name. The
informer owns LIST/WATCH cache synchronization, resource-version handling, and
reconnection; the wrapper does not issue periodic GET requests for every saved
Pod. When an informer event reports a different container ID for the same Pod
UID and container name, the wrapper validates the live Pod, PV, PV claimRef, and
PVC again before mounting into the replacement container namespace. A deleted,
terminal, UID-mismatched, or different-node Pod makes the saved intent stale and
removes it. Explicit authenticated unmount removes the intent before unmounting
so the reconciler cannot recreate it.

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

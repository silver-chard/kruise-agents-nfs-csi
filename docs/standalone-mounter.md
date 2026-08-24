# 在非 OpenKruise Pod 中使用 mounter

本文说明如何在普通 Kubernetes Pod 中由可信 sidecar 调用
`kruise-nfs-mounter`，把一个已绑定的 NFS CSI PV 动态挂载到业务容器。

这条链路仍然使用节点 wrapper 的鉴权和挂载实现。mounter 只读取 projected
service account token 并通过 Unix domain socket 发送请求，不需要 privileged、
`SYS_ADMIN`、host `/proc` 或 CSI socket。

## 前置条件

- 每个目标 Linux 节点上已经运行 wrapper 和上游 NFS CSI node 组件。
- PVC 已经 Bound，PV 的 `spec.csi.driver` 与 wrapper `DRIVER_NAME` 一致。
- PV CSI attributes 包含有效的 `server` 和 `share`。
- wrapper socket 目录默认是
  `/var/lib/kruise-agents-nfs-csi`，并且只挂载给可信 mounter sidecar。
- projected token 的 audience 与 wrapper `TOKEN_AUDIENCE` 一致。
- wrapper socket 的 group/mode 与 sidecar 的 `runAsGroup` 匹配。

业务 Pod 不需要把这个 PVC 声明到 `spec.volumes`。PVC/PV 在这里提供存储身份和
NFS 参数，真正的节点挂载由 wrapper 完成。

## mounter 配置

| 配置 | 默认值 | 说明 |
| --- | --- | --- |
| `DRIVER_NAME` | `csi.nfs.zhida` | 必须与 wrapper、CSIDriver 和 PV `spec.csi.driver` 一致。 |
| `WRAPPER_SOCKET_PATH` | `/var/lib/kruise-agents-nfs-csi/wrapper.sock` | sidecar 内可见的 wrapper socket。 |
| `PROJECTED_TOKEN_FILE` | `/var/run/secrets/kruise-agents-nfs-csi/token` | projected service account token 文件。 |
| `POD_NAMESPACE` | 文件 fallback | 当前 Pod namespace，建议使用 Downward API。 |
| `POD_NAME` | 文件或 `/etc/hostname` fallback | 当前 Pod name，建议使用 Downward API。 |
| `POD_UID` | 文件 fallback | 当前 Pod UID，必须使用 Downward API。 |
| `CONTAINER_NAME` | 空 | 目标业务容器名；多容器 Pod 建议显式配置。 |
| `MOUNTER_HTTP_TIMEOUT` | `15s` | mounter 到本节点 wrapper 的 HTTP 超时。 |

对应命令行参数 `--driver`、`--socket-path`、`--token-file`、
`--namespace`、`--pod-name`、`--pod-uid` 和 `--container` 会覆盖默认配置。

## 最小 Pod 配置

下面示例使用占位镜像
`ghcr.io/example/kruise-agents-nfs-csi-mounter:v0.0.2`。部署前必须替换为
包含 v0.0.2 `kruise-nfs-mounter` 二进制、并且集群可以拉取的镜像。

示例假设 wrapper socket group 是 `2000`，token audience 是
`kruise-agents-nfs-csi.zhida/sandbox-mounter`。如果部署值不同，请同时修改
`fsGroup`、`runAsGroup` 和 token `audience`。示例中的 `nfs-csi`
StorageClass 也应替换为集群中由同一个 driver 提供的 StorageClass。

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: dynamic-nfs-demo
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: dynamic-nfs-mounter
  namespace: dynamic-nfs-demo
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: workspace-data
  namespace: dynamic-nfs-demo
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs-csi
  resources:
    requests:
      storage: 20Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: dynamic-nfs-workload
  namespace: dynamic-nfs-demo
spec:
  serviceAccountName: dynamic-nfs-mounter
  automountServiceAccountToken: false
  securityContext:
    fsGroup: 2000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: main
      image: busybox:1.36
      command: ["sh", "-c", "mkdir -p /workspace/data && sleep 360000"]

    - name: nfs-mounter
      image: ghcr.io/example/kruise-agents-nfs-csi-mounter:v0.0.2
      command: ["sh", "-c", "sleep 360000"]
      env:
        - name: DRIVER_NAME
          value: csi.nfs.zhida
        - name: WRAPPER_SOCKET_PATH
          value: /var/lib/kruise-agents-nfs-csi/wrapper.sock
        - name: PROJECTED_TOKEN_FILE
          value: /var/run/secrets/kruise-agents-nfs-csi/token
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: POD_UID
          valueFrom:
            fieldRef:
              fieldPath: metadata.uid
        - name: CONTAINER_NAME
          value: main
      securityContext:
        runAsUser: 1000
        runAsGroup: 2000
        privileged: false
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: wrapper-socket
          mountPath: /var/lib/kruise-agents-nfs-csi
          readOnly: true
        - name: mounter-token
          mountPath: /var/run/secrets/kruise-agents-nfs-csi
          readOnly: true
  volumes:
    - name: wrapper-socket
      hostPath:
        path: /var/lib/kruise-agents-nfs-csi
        type: Directory
    - name: mounter-token
      projected:
        defaultMode: 0440
        sources:
          - serviceAccountToken:
              path: token
              audience: kruise-agents-nfs-csi.zhida/sandbox-mounter
              expirationSeconds: 3600
          - downwardAPI:
              items:
                - path: namespace
                  fieldRef:
                    fieldPath: metadata.namespace
                - path: pod_name
                  fieldRef:
                    fieldPath: metadata.name
                - path: pod_uid
                  fieldRef:
                    fieldPath: metadata.uid
```

这个 ServiceAccount 不需要额外的 Kubernetes RBAC。TokenReview 和 Pod/PV/PVC
查询由 wrapper 的 ServiceAccount 执行。

## 直接 mount

先从已绑定 PVC 取得 PV 名：

```sh
kubectl -n dynamic-nfs-demo wait pvc/workspace-data \
  --for=jsonpath='{.status.phase}'=Bound \
  --timeout=120s

PV_NAME="$(kubectl -n dynamic-nfs-demo get pvc workspace-data \
  -o jsonpath='{.spec.volumeName}')"
```

然后在可信 sidecar 中调用 mounter：

```sh
kubectl -n dynamic-nfs-demo exec pod/dynamic-nfs-workload -c nfs-mounter -- \
  kruise-nfs-mounter mount \
    --pv "${PV_NAME}" \
    --sub-path users/alice/workspace \
    --target /workspace/data \
    --container main
```

如果不传 `--sub-path`，wrapper 会挂载整个 PV。`--sub-path` 必须是 PV 内的
相对目录，不能是绝对路径，不能包含 `..` 或 symlink 组件。

验证目标业务容器：

```sh
kubectl -n dynamic-nfs-demo exec pod/dynamic-nfs-workload -c main -- \
  sh -c 'mount | grep /workspace/data && touch /workspace/data/.mount-check'
```

## Unmount

显式 unmount 会先删除节点上的期望挂载状态，再卸载目标，防止容器重协调恢复一个
已经取消的挂载：

```sh
kubectl -n dynamic-nfs-demo exec pod/dynamic-nfs-workload -c nfs-mounter -- \
  kruise-nfs-mounter unmount \
    --pv "${PV_NAME}" \
    --target /workspace/data \
    --container main
```

目标已经不存在时，unmount 仍可幂等成功。

## 缺失 SourceSubPath

默认情况下，`--sub-path` 指向的目录必须已经存在。v0.0.2 可以在 wrapper 侧
显式开启自动创建：

```text
WRAPPER_CREATE_MISSING_SUBPATHS=true
WRAPPER_CREATED_SUBPATH_MODE=0770
```

对应 wrapper flags 是 `--create-missing-subpaths` 和
`--created-subpath-mode`，Helm values 是 `wrapper.createMissingSubPaths` 和
`wrapper.createdSubPathMode`。开关默认是 `false`。

新建的每一级目录把 mode `0770` 传给 `mkdirat`，实际 mode 还会受到 wrapper
进程 umask 和文件系统/NFS default ACL 影响；wrapper 不会 `chmod` 或 `chown`。
NFS `root_squash` 可能把 wrapper root 映射为匿名 UID/GID，导致创建失败，或目录
所有者和业务容器不匹配。所有权重要时，应提前按目标 UID/GID 创建目录，而不是依赖
自动创建。

## CSI config 模式

如果自定义 runtime 已经生成 CSI `NodePublishVolumeRequest` protobuf，可以调用：

```sh
kruise-nfs-mounter mount \
  --driver csi.nfs.zhida \
  --config "${NODE_PUBLISH_VOLUME_REQUEST_BASE64}" \
  --container main
```

`--config` 是 base64 编码后的 protobuf，不是 JSON。完整字段提取规则见
[用户调用 API](api.md#运行时命令)。

## 常见问题

### 为什么 sidecar 不需要 privileged？

mounter 只是 UDS API client。真正的 NFS stage、mount namespace 切换和 bind mount
都由节点 wrapper 完成。

### 能否把 socket 和 token 挂给业务容器？

不建议。拿到二者的代码可以请求 wrapper 对通过现有 namespace、PV、PVC 校验的卷
执行挂载；v0.0.2 也没有单独的 subPath 前缀授权。应让可信 runtime 或专用 sidecar
持有它们。

### 目标路径已经是 mount point 怎么办？

wrapper 会返回错误，不会自动覆盖或卸载现有 mount。调用方应先显式 unmount，或选择
新的目标路径。

### 业务容器重启后挂载会丢失吗？

挂载属于具体容器的 mount namespace。wrapper 会观察同一 Pod UID 的 container ID
变化，重新校验 Pod/PV/PVC 后恢复挂载。恢复是最终一致的；如果应用第一条指令就依赖
该目录，runtime 仍需实现启动门禁或重试。

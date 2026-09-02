# 在非 OpenKruise Pod 中使用 mounter

本文说明如何在普通 Kubernetes Pod 中由可信 sidecar 调用
`kruise-nfs-mounter`，把一个 NFS CSI PV 动态挂载到业务容器。

这条链路仍然使用节点 wrapper 的鉴权和挂载实现。mounter 只读取 projected
service account token 并通过 Unix domain socket 发送请求，不需要 privileged、
`SYS_ADMIN`、host `/proc` 或 CSI socket。

## 前置条件

- 每个目标 Linux 节点上已经运行 wrapper 和上游 NFS CSI node 组件。
- PV 的 `spec.csi.driver` 与 wrapper `DRIVER_NAME` 一致。
- PV CSI attributes 包含有效的 `server` 和 `share`。
- PV 的可选 `kary.dev/allow-namespace` 和
  `kary.dev/allow-serviceaccount` annotation 允许当前 Pod。
- wrapper socket 目录默认是
  `/var/lib/kruise-agents-nfs-csi`，并且只挂载给可信 mounter sidecar。
- projected token 的 audience 与 wrapper `TOKEN_AUDIENCE` 一致，并且 token
  绑定当前 Pod。
- wrapper socket 的 group/mode 与 sidecar 的 `runAsGroup` 匹配。
- 如果有效 NFS 路径是 `share` root，wrapper 和调用方还需共享一个
  export root capability key 文件；非 root mount 不需要。

PV 不需要 PVC 或 `claimRef`。wrapper 不读取它们做 mount 授权；PV 提供
NFS 参数，实时 Pod identity 和 PV annotation 提供授权边界，真正的节点挂载由
wrapper 完成。

## mounter 配置

| 配置 | 默认值 | 说明 |
| --- | --- | --- |
| `DRIVER_NAME` | `csi.nfs.zhida` | 必须与 wrapper、CSIDriver 和 PV `spec.csi.driver` 一致。 |
| `WRAPPER_SOCKET_PATH` | `/var/lib/kruise-agents-nfs-csi/wrapper.sock` | sidecar 内可见的 wrapper socket。 |
| `PROJECTED_TOKEN_FILE` | `/var/run/secrets/kruise-agents-nfs-csi/token` | projected service account token 文件。 |
| `EXPORT_ROOT_KEY_FILE` | 空 | 可选 export root capability key 文件；非 root mount 保持为空。 |
| `POD_NAMESPACE` | 文件 fallback | 当前 Pod namespace，建议使用 Downward API。 |
| `POD_NAME` | 文件或 `/etc/hostname` fallback | 当前 Pod name，建议使用 Downward API。 |
| `POD_UID` | 文件 fallback | 当前 Pod UID，必须使用 Downward API。 |
| `CONTAINER_NAME` | 空 | 目标业务容器名；多容器 Pod 建议显式配置。 |
| `MOUNTER_HTTP_TIMEOUT` | `15s` | mounter 到本节点 wrapper 的 HTTP 超时。 |

对应命令行参数 `--driver`、`--socket-path`、`--token-file`、`--export-root-key-file`、
`--namespace`、`--pod-name`、`--pod-uid` 和 `--container` 会覆盖默认配置。

## 最小 Pod 配置

下面示例使用占位镜像
`ghcr.io/example/kruise-agents-nfs-csi-mounter:VERSION`。部署前必须替换为
包含与节点 wrapper 同版本 `kruise-nfs-mounter` 二进制、并且集群可以拉取的镜像。

示例假设 wrapper socket group 是 `2000`，token audience 是
`kruise-agents-nfs-csi.zhida/sandbox-mounter`。如果部署值不同，请同时修改
`fsGroup`、`runAsGroup` 和 token `audience`。

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
      image: ghcr.io/example/kruise-agents-nfs-csi-mounter:VERSION
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

这个 ServiceAccount 不需要额外的 Kubernetes RBAC。TokenReview 和 Pod/PV
查询由 wrapper 的 ServiceAccount 执行。必须使用上面的
`serviceAccountToken` projected volume；默认
`/var/run/secrets/kubernetes.io/serviceaccount/token` 通常面向 Kubernetes API，
不能替代这个专用 audience token。wrapper 还要求 TokenReview 返回的
`authentication.kubernetes.io/pod-name` 和
`authentication.kubernetes.io/pod-uid` 各只有一个值，并与请求和实时 Pod
精确匹配。

## 直接 mount

由可信 runtime 配置要挂载的 PV name，例如：

```sh
PV_NAME=shared-workspace-pv
```

建议在 PV 上明确限制允许使用它的 namespace 和 ServiceAccount：

```yaml
metadata:
  annotations:
    kary.dev/allow-namespace: "dynamic-nfs-demo"
    kary.dev/allow-serviceaccount: "dynamic-nfs-mounter"
```

每个 annotation key 完全缺失表示该维度不限制；存在时值是逗号分隔的
精确 allowlist，每项会 `TrimSpace`，匹配区分大小写。两个 key 同时存在时按
AND。空值、空项和 `*` 都是非法配置并 fail closed；要取消某维限制应删除
该 key。两个 key 都缺失时，任何能通过 exact Pod token、socket、driver 和
路径检查的可信 caller 都可以请求该 PV。

然后在可信 sidecar 中调用 mounter：

```sh
kubectl -n dynamic-nfs-demo exec pod/dynamic-nfs-workload -c nfs-mounter -- \
  kruise-nfs-mounter mount \
    --pv "${PV_NAME}" \
    --sub-path users/alice/workspace \
    --target /workspace/data \
    --container main
```

`--sub-path` 必须是 PV 内的相对目录，不能是绝对路径，不能包含 `..` 或
symlink 组件。上例的有效路径在 NFS share root 之下，因此不需要 export root
key。如果不传 `--sub-path`，wrapper 会挂载 PV root；是否等于 export root
还取决于 PV 的 `volumeAttributes["subDir"]`。

验证目标业务容器：

```sh
kubectl -n dynamic-nfs-demo exec pod/dynamic-nfs-workload -c main -- \
  sh -c 'mount | grep /workspace/data && touch /workspace/data/.mount-check'
```

## 有效 NFS 路径与 export root key

wrapper 用 PV CSI `volumeAttributes["subDir"]`（兼容 `subdir`）和请求
`--sub-path` 共同判定有效 NFS 路径：

| PV `subDir` | `--sub-path` | 有效位置 | 需要 key |
| --- | --- | --- | --- |
| 空 | 空 | NFS `share` root | 是 |
| 空 | `users/alice` | `share/users/alice` | 否 |
| `tenants/team-a` | 空 | `share/tenants/team-a` | 否 |
| `tenants/team-a` | `workspace` | `share/tenants/team-a/workspace` | 否 |

PV `subDir` 归一化后为空、仅 `/` 或仅 `.` 都表示 share root；包含 NUL 或任意
`..` 路径段会被拒绝。`--sub-path` 空或归一化为 `.` 表示 PV root；绝对路径、
NUL 和 `..` 会被拒绝。只有两者归一化后都为空才需要 key。

wrapper 通过 `WRAPPER_EXPORT_ROOT_KEY_FILE` / `--export-root-key-file` 配置
服务端 key；未配置时只拒绝 export root mount，不影响非 root mount。调用方
把相同 Secret 只读挂入 sidecar，然后在 root mount 上指定：

```sh
kruise-nfs-mounter mount \
  --pv "${PV_NAME}" \
  --target /workspace/data \
  --container main \
  --export-root-key-file /var/run/secrets/kruise-agents-nfs-csi-root/key
```

也可以设置 `EXPORT_ROOT_KEY_FILE`。key 只通过
`X-Kary-Export-Root-Key` header 发送，不进入 JSON 或持久化 state；state 保存
该 mount 已通过 root 授权的布尔值和 key 的 SHA-256 fingerprint，不保存 key
原文。不要把 key 放进环境变量值、PV annotation 或日志。wrapper 在启动时读取
key，替换文件后需重启 wrapper；mounter 每次 mount 都重新读取调用方文件。
轮换后，已有 Linux mount 不会被主动卸载；可信 caller 应用新 key 重复同一个
mount 请求，以无叠加方式刷新 state fingerprint，否则下一次容器重启不会自动
恢复旧 key 授权的 export root mount。

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

unmount 只发送专用 audience 的 Pod-bound token，不读取或发送 export root key，
也不重新获取 PV 或检查 PV annotation。wrapper 只会按精确 Pod、container 和
target 清理此前登记的 state，并要求 state 中 PV 与请求一致；没有匹配 state 时
幂等成功，不会卸载未受 wrapper 管理的 mount。state container ID 已经不是实时
容器时只删除旧 state，不操作新容器 namespace 中的同路径 mount。

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

不建议。拿到二者的代码可以用当前 Pod 身份请求所有被 PV annotation 允许的卷；
annotation 完全缺失时该 PV 不做 namespace/ServiceAccount 限制。当前也没有单独的
subPath 前缀授权。应让可信 runtime 或专用 sidecar 持有 socket 和 token。

### 目标路径已经是 mount point 怎么办？

wrapper 会返回错误，不会自动覆盖或卸载现有 mount。调用方应先显式 unmount，或选择
新的目标路径。

### 业务容器重启后挂载会丢失吗？

挂载属于具体容器的 mount namespace。wrapper 会观察同一 Pod UID 的 container ID
变化，重新校验实时 Pod、PV driver 和 annotation 后恢复挂载。节点 state 不保存
token、export root key 或 NFS 凭据；export root mount 只会在已登记授权和 key
fingerprint 仍匹配 wrapper 当前启动 key 时恢复。
恢复是最终一致的；如果应用第一条指令就依赖该目录，runtime 仍需实现启动门禁或重试。

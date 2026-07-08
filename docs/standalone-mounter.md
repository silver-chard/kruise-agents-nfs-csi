# 非 Kruise 场景使用 mounter 动态挂载

本文说明客户不接入 Kruise，只使用 CSI wrapper 和 `kruise-nfs-mounter`，
在 Pod 启动后按需把 NFS PV 动态挂载到业务容器内。

## 1. 说明

动态挂载链路如下：

1. 集群安装 CSI wrapper。安装后每个节点上会有一个 wrapper 进程监听 Unix domain socket。
2. 业务 Pod 内放置一个可信的 mounter 调用方。它可以是独立 sidecar，也可以是业务镜像内置的二进制。
3. mounter 通过 projected service account token 证明自己属于当前 Pod。
4. mounter 通过节点上的 wrapper socket 发起 mount 请求。
5. wrapper 校验 token、Pod、PV、PVC、目标容器和目标路径后，把 PV bind mount 到目标业务容器的 mount namespace。

这个能力不要求业务 Pod 把 PVC 写进 `spec.volumes` 和 `volumeMounts`。PVC/PV 的作用是提供
Kubernetes 侧的存储身份和 NFS 参数，实际挂载动作由 wrapper 在节点上完成。

## 2. CSI 及 wrapper 安装

以下以内部 chart repo `appspace-crd/cnap-kruise-agents` 为准，只安装 NFS CSI wrapper，
不安装 sandbox controller、sandbox manager 和 cfs。

### 2.1 只安装 wrapper，不创建 StorageClass

如果集群已经有 provisioner 为 `csi.nfs.cnap.com` 的 StorageClass/PV，或者客户会手工
创建同 driver 的 PV，可以不让 chart 创建 StorageClass。此时不需要传 NFS endpoint。

```sh
helm upgrade --install cnap-csi-wrapper \
  appspace-crd/cnap-kruise-agents \
  --version 0.3.3 \
  -n kube-system \
  --create-namespace \
  --kube-context <context> \
  --set agents-sandbox-controller.enabled=false \
  --set agents-sandbox-manager.enabled=false \
  --set cfs.enabled=false \
  --set sandboxInjectionConfig.enabled=false \
  --set kruise-agents-nfs-csi.enabled=true \
  --set kruise-agents-nfs-csi.csi-driver-nfs.storageClass.create=false \
  --set kruise-agents-nfs-csi.csi-driver-nfs.kubeletDir=/home/cce/kubelet
```

`kubeletDir` 取决于集群：

| 集群类型 | `kubeletDir` |
| --- | --- |
| BCE-CCE | `/home/cce/kubelet` |
| 标准 Kubernetes | `/var/lib/kubelet` |

注意：shell 的 `\` 后面不能再跟注释。不要写成
`--set ...kubeletDir=/home/cce/kubelet \ # comment`，这会让命令解析失败。

### 2.2 如果需要 chart 创建 StorageClass

如果客户希望用这个 CSI driver 动态创建 PVC/PV，需要打开 StorageClass，并传入真实 NFS
endpoint：

```sh
helm upgrade --install cnap-csi-wrapper \
  appspace-crd/cnap-kruise-agents \
  --version 0.3.3 \
  -n kube-system \
  --create-namespace \
  --kube-context <context> \
  --set agents-sandbox-controller.enabled=false \
  --set agents-sandbox-manager.enabled=false \
  --set cfs.enabled=false \
  --set sandboxInjectionConfig.enabled=false \
  --set kruise-agents-nfs-csi.enabled=true \
  --set kruise-agents-nfs-csi.csi-driver-nfs.kubeletDir=/home/cce/kubelet \
  --set kruise-agents-nfs-csi.csi-driver-nfs.storageClass.create=true \
  --set kruise-agents-nfs-csi.csi-driver-nfs.storageClass.name=nfs-csi \
  --set kruise-agents-nfs-csi.csi-driver-nfs.storageClass.parameters.server=<nfs-server> \
  --set kruise-agents-nfs-csi.csi-driver-nfs.storageClass.parameters.share=<nfs-export-path>
```

`server` 和 `share` 示例：

| 参数 | 示例 | 说明 |
| --- | --- | --- |
| `server` | `nfs.example.internal` | NFS server 地址，必须是 Pod 所在节点可访问的地址。 |
| `share` | `/exports/workloads` | NFS export 根路径。PVC 对应的子目录会在该 export 下创建。 |

### 2.3 安装后检查

```sh
helm status cnap-csi-wrapper -n kube-system --kube-context <context>

kubectl --context <context> -n kube-system rollout status deploy/cnap-nfs-csi-controller
kubectl --context <context> -n kube-system rollout status ds/cnap-nfs-csi-node
kubectl --context <context> get csidriver csi.nfs.cnap.com
```

确认 node wrapper 监听的 socket 目录存在：

```sh
kubectl --context <context> -n kube-system get ds cnap-nfs-csi-node \
  -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"="}{.image}{"\n"}{end}'
```

wrapper 默认在每个节点监听：

```text
/var/lib/kruise-agents-nfs-csi/wrapper.sock
```

这个 socket 不是 Kubernetes Service。mounter 所在容器必须通过 `hostPath` 把节点上的
`/var/lib/kruise-agents-nfs-csi` 挂进来。

## 3. 动态挂载使用

### 3.1 mounter 使用条件

mounter 需要满足这些条件：

- 能访问本节点 wrapper socket：默认 `/var/lib/kruise-agents-nfs-csi/wrapper.sock`。
- 能读取 projected service account token：默认 `/var/run/secrets/kruise-agents-nfs-csi/token`。
- 能提供当前 Pod 的 namespace、name 和 UID。
- 能明确目标业务容器：通过 `--container`、`CONTAINER_NAME`，或业务容器上的 `SANDBOX_MAIN_CONTAINER=true`。
- 要挂载的 PVC/PV 已经 Bound，且 PV 的 `spec.csi.driver` 等于 `csi.nfs.cnap.com`。
- PV CSI attributes 中有 NFS `server` 和 `share`。
- 如果只挂 PV 内的某个目录，`--sub-path` 指向的目录必须已经存在，且不能包含 symlink 组件。
- 目标路径是业务容器内的绝对路径，且当前不是已有 mount point。

### 3.2 mounter 容器参数和依赖

下面表格按“容器怎么配置”和“二进制怎么消费”拆开说明。`mounter container 使用`
描述 YAML 中应该怎么配；`二进制使用` 描述 `kruise-nfs-mounter` 启动后会如何使用该值。

| 参数类型 | 变量名 | mounter container 使用 | 二进制使用 | 说明 |
| --- | --- | --- | --- | --- |
| 环境变量 | `DRIVER_NAME` | 必填。设置为已安装 CSI driver name，例如 `csi.nfs.cnap.com`。 | 不传 `--driver-name` 或 `mount --driver` 时，用它作为请求里的 driver name。 | 这个值必须同时等于 CSIDriver 名称、StorageClass `provisioner`、PV `spec.csi.driver`。不一致时 wrapper 会拒绝挂载。 |
| 环境变量 | `WRAPPER_SOCKET_PATH` | 必填。设置为 mounter 容器内可见的 socket 文件，例如 `/var/lib/kruise-agents-nfs-csi/wrapper.sock`。 | 不传 `--socket-path` 时，二进制用它连接 wrapper。 | 它必须落在 wrapper socket hostPath 的挂载目录里；如果路径错，会报连接 Unix socket 失败。 |
| 环境变量 | `PROJECTED_TOKEN_FILE` | 必填。设置为 projected token 在容器内的文件路径，例如 `/var/run/secrets/kruise-agents-nfs-csi/token`。 | 不传 `--token-file` 时，二进制读取该文件，并把内容放到 `Authorization: Bearer`。 | 文件内容是短期 service account token。token 的 audience 必须和 wrapper chart 的 `tokenAudience` 一致。 |
| 环境变量 | `POD_NAMESPACE` | 必填。建议用 Downward API `metadata.namespace` 注入。 | 不传 `--namespace` 时，作为请求 Pod namespace。 | wrapper 会用它查 Pod、PVC，并校验 token 是否属于同一个 namespace。 |
| 环境变量 | `POD_NAME` | 必填。建议用 Downward API `metadata.name` 注入。 | 不传 `--pod-name` 时，作为请求 Pod name。 | wrapper 会按这个名字读取实时 Pod，并确认该 Pod 正在运行。 |
| 环境变量 | `POD_UID` | 必填。建议用 Downward API `metadata.uid` 注入。 | 不传 `--pod-uid` 时，作为请求 Pod UID。 | 防止同名 Pod 删除重建后，旧 mounter 请求误挂到新 Pod 上。 |
| 环境变量 | `CONTAINER_NAME` | 多容器 Pod 建议配置，例如 `main`。也可以不设，调用时传 `--container main`。 | 不传 `--container` 时，作为目标业务容器名。 | wrapper 最终会把 PV 挂到这个容器的 mount namespace。多容器 Pod 不指定时，容易无法唯一确定目标容器。 |
| 环境变量 | `NAMESPACE_FILE` | 可选。默认 `/var/run/secrets/kubernetes.io/serviceaccount/namespace`。 | 当 `POD_NAMESPACE` 为空时，二进制读取该文件。 | 正常 YAML 已注入 `POD_NAMESPACE`，一般不用配置。 |
| 环境变量 | `POD_NAME_FILE` | 可选。默认 `/var/run/secrets/kruise-agents-nfs-csi/pod_name`。 | 当 `POD_NAME` 为空时，二进制读取该文件；再不行会读 `/etc/hostname`。 | 对应 projected downwardAPI 里的 `pod_name` 文件。 |
| 环境变量 | `POD_UID_FILE` | 可选。默认 `/var/run/secrets/kruise-agents-nfs-csi/pod_uid`。 | 当 `POD_UID` 为空时，二进制读取该文件。 | 对应 projected downwardAPI 里的 `pod_uid` 文件。 |
| 环境变量 | `MOUNTER_HTTP_TIMEOUT` | 可选。默认 `15s`，例如可设为 `30s`。 | 控制二进制调用 wrapper socket 的 HTTP client 超时。 | 只影响 mounter 到本机 wrapper 的请求，不影响 NFS mount 本身的超时。 |
| 环境变量 | `TOKEN_AUDIENCE` | 不建议配置。mounter 容器真正需要配置的是 projected token 的 `audience`。 | 当前二进制不会把该值发送给 wrapper。 | wrapper 校验的是 token 自身的 audience，不是 mounter 环境变量里的 `TOKEN_AUDIENCE`。 |
| Volume | `wrapper-socket` | 必填。把宿主机 `/var/lib/kruise-agents-nfs-csi` 以 `hostPath` 只读挂到 mounter 容器。 | 二进制通过 `WRAPPER_SOCKET_PATH` 或 `--socket-path` 使用其中的 `wrapper.sock`。 | 该目录来自节点 DaemonSet。不要挂给不可信业务进程，否则它也能调用 wrapper。 |
| Volume | `mounter-token` | 必填。用 `projected.serviceAccountToken` 投射 token，并挂到 `PROJECTED_TOKEN_FILE` 所在目录。 | 二进制读取 token 后调用 wrapper。 | `serviceAccountToken.audience` 必须匹配 wrapper chart 的 `tokenAudience`，否则 TokenReview 会失败。 |
| Volume | Downward API files | 建议配置。projected volume 中放入 `namespace`、`pod_name`、`pod_uid`。 | 当对应环境变量为空时，作为 Pod 身份 fallback。 | 同时配置环境变量和文件 fallback，能降低客户改 YAML 时漏配身份字段的风险。 |
| Pod 配置 | `serviceAccountName` | 必填。Pod 使用的 service account 必须和 projected token 对应。 | 二进制不直接访问 Kubernetes API；wrapper 会校验 token subject 和 Pod service account。 | mounter service account 不需要额外 Kubernetes RBAC；TokenReview 和 Pod/PV/PVC 查询由节点 wrapper 的 service account 完成。 |
| Pod 配置 | `automountServiceAccountToken` | 建议设为 `false`。 | 二进制只读取 `PROJECTED_TOKEN_FILE` 指定的 token。 | 避免容器里出现默认 service account token，减少误用。 |
| Pod 配置 | `SANDBOX_MAIN_CONTAINER` | 可选。设置在业务容器上，例如 `SANDBOX_MAIN_CONTAINER=true`。 | 当请求里没有 `--container` / `CONTAINER_NAME` 时，wrapper 用它查找目标容器。 | 如果已经显式传 `--container main`，这个标记不是必需的。 |
| 镜像/文件 | `kruise-nfs-mounter` | 必填。可以来自 mounter sidecar 镜像，也可以内置在客户业务镜像。 | 执行该二进制发起 mount 请求。 | 如果把二进制放进业务容器，该容器也必须同时挂载 wrapper socket 和 projected token。 |
| Kubernetes 对象 | PVC/PV | 必填。PVC 必须 Bound，且能取到 `.spec.volumeName`。 | 直接参数模式下通过 `--pv <pv-name>` 传入。 | Pod 不需要声明 PVC volumeMount；wrapper 会校验 PV claimRef 是否指向同 namespace 的 PVC。 |
| 调用参数 | `--sub-path` | mounter 容器不配置为环境变量，调用时按需传入。 | 指定 PV 内要挂载的目录；不传时挂整个 PV。 | 仅支持目录 subPath，例如 `users/alice/workspace`。必须是相对路径，不能包含 `..`、绝对路径或 symlink 组件。 |
| 调用参数 | `--target` | mounter 容器不配置为环境变量，调用时传入。 | 指定业务容器内最终挂载路径。 | 必须是绝对路径，例如 `/workspace/data`；不能是 `/proc`、`/sys`、`/dev`、secret、kubelet 等敏感目录。 |

### 3.3 最小可运行 YAML

下面示例假设：

- CSI driver name 是 `csi.nfs.cnap.com`；
- mounter 镜像是 `iregistry.baidu-int.com/cnap-cluster/kruise-agents-nfs-csi-mounter:0.0.3`；
- wrapper socket 在节点上的目录是 `/var/lib/kruise-agents-nfs-csi`；
- projected token audience 是 `kruise-agents-nfs-csi.zhida/sandbox-mounter`；
- 已经存在可让 PVC Bound 的 StorageClass `nfs-csi`。如果安装时设置了
  `storageClass.create=false`，需要客户提前创建同 driver 的 StorageClass/PV，或把 PVC
  改成客户已有的同 driver StorageClass。

#### ServiceAccount

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: standalone-mounter
  namespace: customer-demo
```

#### PVC

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: workspace-data
  namespace: customer-demo
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs-csi
  resources:
    requests:
      storage: 20Gi
```

等待 PVC Bound，并取出 PV 名：

```sh
kubectl -n customer-demo wait pvc/workspace-data \
  --for=jsonpath='{.status.phase}'=Bound \
  --timeout=120s

PV_NAME="$(kubectl -n customer-demo get pvc workspace-data -o jsonpath='{.spec.volumeName}')"
```

#### Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: customer-workload
  namespace: customer-demo
spec:
  serviceAccountName: standalone-mounter
  automountServiceAccountToken: false
  containers:
    - name: main
      image: busybox:1.36
      command: ["sh", "-c", "mkdir -p /workspace/data && sleep 360000"]
      env:
        - name: SANDBOX_MAIN_CONTAINER
          value: "true"

    - name: nfs-mounter
      image: iregistry.baidu-int.com/cnap-cluster/kruise-agents-nfs-csi-mounter:0.0.3
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "sleep 360000"]
      env:
        - name: DRIVER_NAME
          value: csi.nfs.cnap.com
        - name: WRAPPER_SOCKET_PATH
          value: /var/lib/kruise-agents-nfs-csi/wrapper.sock
        - name: PROJECTED_TOKEN_FILE
          value: /var/run/secrets/kruise-agents-nfs-csi/token
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        - name: POD_UID
          valueFrom:
            fieldRef:
              fieldPath: metadata.uid
        - name: CONTAINER_NAME
          value: main
      securityContext:
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

## 4. mounter 调用方式

### 4.1 直接参数模式

推荐不接 Kruise 的客户使用直接参数模式。调用时传 PV 名和目标路径：

```sh
kubectl -n customer-demo exec pod/customer-workload -c nfs-mounter -- \
  kruise-nfs-mounter \
    --driver-name csi.nfs.cnap.com \
    --pv "${PV_NAME}" \
    --sub-path users/alice/workspace \
    --target /workspace/data \
    --container main
```

如果 `DRIVER_NAME`、`POD_NAMESPACE`、`POD_NAME`、`POD_UID`、`CONTAINER_NAME`
都已经通过环境变量注入，可以简化为：

```sh
kubectl -n customer-demo exec pod/customer-workload -c nfs-mounter -- \
  kruise-nfs-mounter \
    --pv "${PV_NAME}" \
    --sub-path users/alice/workspace \
    --target /workspace/data
```

成功输出示例：

```json
{"data":{"mounted":true,"driver_name":"csi.nfs.cnap.com","pv_name":"pvc-xxxx","source_sub_path":"users/alice/workspace","target_path":"/workspace/data","container_name":"main"}}
```

验证业务容器内可见：

```sh
kubectl -n customer-demo exec pod/customer-workload -c main -- \
  sh -c 'mount | grep /workspace/data && touch /workspace/data/.mounter-check && ls -l /workspace/data/.mounter-check'
```

### 4.2 参数说明

| 参数 | 是否必填 | 说明 |
| --- | --- | --- |
| `--driver-name` | 否 | 默认来自 `DRIVER_NAME`。必须匹配 CSIDriver 和 PV `spec.csi.driver`。 |
| `--pv` | 是 | 要动态挂载的 PV 名，通常从 PVC `.spec.volumeName` 获取。 |
| `--sub-path` | 否 | PV 内的目录 subPath。不传时挂整个 PV；传入时必须是已存在的相对目录路径。 |
| `--target` | 是 | 目标业务容器内的绝对路径，例如 `/workspace/data`。 |
| `--container` | 建议显式传 | 目标业务容器名。多容器 Pod 建议显式传，避免 wrapper 无法唯一判断。 |
| `--namespace` | 否 | 默认来自 `POD_NAMESPACE`，或 fallback 文件。 |
| `--pod-name` | 否 | 默认来自 `POD_NAME`，或 fallback 文件。 |
| `--pod-uid` | 否 | 默认来自 `POD_UID`，或 fallback 文件。 |
| `--socket-path` | 否 | 默认来自 `WRAPPER_SOCKET_PATH`。 |
| `--token-file` | 否 | 默认来自 `PROJECTED_TOKEN_FILE`。 |

### 4.3 CSI config 模式

如果客户已有自己的 runtime，能够生成 CSI `NodePublishVolumeRequest` protobuf，可以使用
`mount --config` 模式：

```sh
kruise-nfs-mounter mount \
  --driver csi.nfs.cnap.com \
  --config "${NODE_PUBLISH_VOLUME_REQUEST_BASE64}" \
  --container main
```

`NODE_PUBLISH_VOLUME_REQUEST_BASE64` 必须是 base64 编码后的 protobuf，不是 JSON。
mounter 会从 `NodePublishVolumeRequest` 中读取：

| 字段 | 用途 |
| --- | --- |
| `target_path` | 目标业务容器内路径。 |
| `volume_context["source_sub_path"]`、`volume_context["sourceSubPath"]` | 可选，PV 内目录 subPath。 |
| `volume_context["sub_path"]`、`volume_context["subPath"]` | 可选，PV 内目录 subPath fallback。 |
| `publish_context["source_sub_path"]`、`publish_context["sourceSubPath"]` | 可选，PV 内目录 subPath fallback。 |
| `publish_context["sub_path"]`、`publish_context["subPath"]` | 可选，PV 内目录 subPath fallback。 |
| `volume_context["csi.storage.k8s.io/pv/name"]` | 优先 PV 名。 |
| `volume_context["pvName"]`、`volume_context["pv_name"]`、`volume_context["persistentVolumeName"]` | PV 名 fallback。 |
| `publish_context["csi.storage.k8s.io/pv/name"]`、`publish_context["pvName"]` | PV 名 fallback。 |
| `volume_id` | 最后 fallback；如果带 `-` 加 6 位小写字母或数字后缀，会去掉该后缀。 |

## 5. wrapper 侧校验要求

一次 mount 请求必须同时满足：

- bearer token 通过 TokenReview，且 audience 等于 chart 的 `tokenAudience`；
- token service account 属于请求 namespace；
- token service account 等于 Pod 的 `spec.serviceAccountName`；
- 请求中的 Pod name、namespace、UID 与实时 Pod 一致；
- Pod 不是 `Succeeded` 或 `Failed`；
- PV 是 CSI PV，且 `spec.csi.driver` 等于 `DRIVER_NAME`；
- PV claimRef 指向同 namespace 的 PVC；
- PVC 实时存在，且 UID 与 PV claimRef 一致时必须匹配；
- 目标容器存在并有非空 container ID；
- PV CSI `volumeAttributes` 中有 `server` 和 `share`；
- `source_sub_path` 为空，或是 PV 内已存在的目录；路径必须是相对路径，不能包含 `..`、绝对路径或 symlink 组件；
- 目标路径是绝对路径，且当前还不是 mount point。

## 6. 常见问题

### 业务 Pod 是否必须声明 PVC volumeMount？

不必须。mounter 的动态挂载不是 kubelet 的普通 `volumeMount` 流程。PVC/PV 用来提供
Kubernetes 存储身份和 NFS 参数，真正挂载动作由节点 wrapper 完成。

### 为什么 mounter sidecar 不需要 privileged？

mounter 只是一个低权限 API client：读取 token、连接本机 wrapper socket、发送 JSON 请求。
真实 mount、`setns`、`open_tree`、`move_mount` 都在节点 wrapper 容器中执行。

### 能不能把 wrapper socket 和 token 挂给业务容器？

技术上可以，但不推荐。拿到 socket 和 token 的容器可以请求 wrapper 挂载它 namespace
内已绑定的 PV。更安全的做法是让可信 sidecar 或客户自己的 runtime/agent 持有这两个挂载。

### `storageClass.create=false` 时还能用动态挂载吗？

可以，但前提是客户自己准备好 Bound 的 PVC/PV。PV 必须是 CSI PV，
`spec.csi.driver` 必须等于 `csi.nfs.cnap.com`，`volumeAttributes` 中必须包含
`server` 和 `share`。

### 目标路径已经挂载怎么办？

wrapper 会返回错误，不会主动覆盖已有 mount point。调用方需要选择新的目标路径，
或由自己的生命周期逻辑先清理旧挂载。

### unmount staging 会影响业务容器里的 subPath mount 吗？

不会。wrapper 会先把 `stagePath/subPath` clone/move 到业务容器的 mount namespace，
再清理节点上的 staging mount。业务容器里的 mount 已经是独立 mount 引用，不依赖
staging 路径继续存在。

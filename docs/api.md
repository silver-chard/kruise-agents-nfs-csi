# 用户调用 API

本文梳理动态 NFS mount 能直接给用户或运行时集成方调用的接口。

推荐的用户调用面是 `kruise-nfs-mounter` 命令。wrapper 的 HTTP API 是更底层的
Unix Domain Socket API，只应该挂载给 mounter sidecar，不应该直接暴露给业务容器。

## API 总览

| 层次 | 调用方 | 接口 | 用途 |
| --- | --- | --- | --- |
| 运行时命令 | Sandbox runtime | `kruise-nfs-mounter mount --driver <driver> --config <base64 NodePublishVolumeRequest>` | 推荐集成方式。mounter 解析 CSI 输入后调用 wrapper，可选 PV 内目录 subPath。 |
| 直接命令 | 可信 sidecar 或排障会话 | `kruise-nfs-mounter --pv <pv> --sub-path <dir> --target <path>` | 调用方已经知道 PV、可选 subPath 和目标路径时使用，适合调试或封装更上层接口。 |
| 健康检查 | 节点本地检查器 | `GET /healthz` over wrapper UDS | 确认 wrapper 进程正在服务，并返回当前 driver name。 |
| Mount API | mounter sidecar | `POST /v1/mount` over wrapper UDS | 底层 JSON API，需要 projected service account bearer token。 |

所有请求和响应 JSON 字段都使用 `snake_case`。

## 调用链路

1. Sandbox runtime 调用 `kruise-nfs-mounter`。
2. mounter 从参数、环境变量或 Downward API projected 文件读取 Pod 身份。
3. mounter 从 `PROJECTED_TOKEN_FILE` 读取 projected service account token。
4. mounter 通过节点 wrapper Unix socket 发送 `POST /v1/mount`。
5. wrapper 校验 token、实时 Pod、PV、PVC、目标容器、driver name 和目标路径。
6. wrapper 在节点上临时 stage NFS PV，并把整个 PV 或 PV 内指定目录 bind-mount 到目标容器的 mount namespace。

业务容器不应该拿到 wrapper socket、projected wrapper token，或可任意选择 PV 的配置。

## 运行时命令

正常集成优先调用：

```sh
kruise-nfs-mounter mount \
  --driver csi.nfs.zhida \
  --config "${NODE_PUBLISH_VOLUME_REQUEST_BASE64}" \
  --sub-path users/alice/workspace \
  --container main
```

`--config` 必须是 base64 编码的 CSI `NodePublishVolumeRequest` protobuf。mounter
会按下面规则解析：

| 来源 | 用途 |
| --- | --- |
| `target_path` | 业务容器内的目标挂载路径。 |
| `volume_context["source_sub_path"]`、`volume_context["sourceSubPath"]` | 可选，PV 内目录 subPath。 |
| `volume_context["sub_path"]`、`volume_context["subPath"]` | 可选，PV 内目录 subPath fallback。 |
| `publish_context["source_sub_path"]`、`publish_context["sourceSubPath"]` | 可选，PV 内目录 subPath fallback。 |
| `publish_context["sub_path"]`、`publish_context["subPath"]` | 可选，PV 内目录 subPath fallback。 |
| `volume_context["csi.storage.k8s.io/pv/name"]` | 优先作为 PV 名。 |
| `volume_context["pvName"]` | PV 名 fallback。 |
| `volume_context["pv_name"]` | PV 名 fallback。 |
| `volume_context["persistentVolumeName"]` | PV 名 fallback。 |
| `publish_context["csi.storage.k8s.io/pv/name"]` | PV 名 fallback。 |
| `publish_context["pvName"]` | PV 名 fallback。 |
| `volume_id` | 最后 fallback。如果以 `-` 加 6 位小写字母或数字结尾，会去掉该后缀。 |

命令参数：

| 参数 | 是否必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `--driver` | 否 | `DRIVER_NAME` 或 `csi.nfs.zhida` | wrapper 和 PV 期望的 CSI driver name。 |
| `--config` | 是 | 无 | base64 CSI `NodePublishVolumeRequest`。 |
| `--sub-path` | 否 | `NodePublishVolumeRequest` 中的 subPath 或空 | PV 内目录 subPath。不传时挂整个 PV；传入时只支持已存在目录。 |
| `--container` | 视情况 | `CONTAINER_NAME` | 目标业务容器。Pod 多容器且没有 `SANDBOX_MAIN_CONTAINER=true` 标记时必须传。 |
| `--namespace` | 否 | `POD_NAMESPACE`、`NAMESPACE_FILE` 或 service account namespace 文件 | 请求 Pod namespace。 |
| `--pod-name` | 否 | `POD_NAME`、`POD_NAME_FILE` 或 `/etc/hostname` | 请求 Pod name。 |
| `--pod-uid` | 否 | `POD_UID` 或 `POD_UID_FILE` | 请求 Pod UID。 |
| `--socket-path` | 否 | `WRAPPER_SOCKET_PATH` 或 `/var/lib/kruise-agents-nfs-csi/wrapper.sock` | wrapper Unix socket 路径。 |
| `--token-file` | 否 | `PROJECTED_TOKEN_FILE` 或 `/var/run/secrets/kruise-agents-nfs-csi/token` | projected service account token 文件。 |

成功时 stdout：

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

失败时 stderr：

```json
{
  "error": "wrapper rejected mount request: pv pv-a uses driver other.csi.driver, expected csi.nfs.zhida"
}
```

## 直接命令

直接命令跳过 CSI protobuf 解析，但仍然会走 wrapper 的鉴权和 mount 逻辑：

```sh
kruise-nfs-mounter \
  --driver-name csi.nfs.zhida \
  --namespace openkruise-sandbox-demo \
  --pod-name sandbox-demo-0 \
  --pod-uid 00000000-0000-0000-0000-000000000000 \
  --pv pv-sandbox-nfs-demo \
  --sub-path users/alice/workspace \
  --target /workspace/data \
  --container main
```

只建议在可信 sidecar 或受控排障会话中使用。调用环境仍然需要 wrapper socket 和
projected token。

## Wrapper Socket API

默认 socket：

```text
/var/lib/kruise-agents-nfs-csi/wrapper.sock
```

### GET /healthz

不需要鉴权。

响应：

```json
{
  "data": {
    "status": "ok",
    "driver_name": "csi.nfs.zhida"
  }
}
```

### POST /v1/mount

请求头：

```text
Content-Type: application/json
Accept: application/json
Authorization: Bearer <projected-service-account-token>
```

请求体：

```json
{
  "api_version": "kruise-agents-nfs-csi.zhida/v1alpha1",
  "driver_name": "csi.nfs.zhida",
  "namespace": "openkruise-sandbox-demo",
  "pod_name": "sandbox-demo-0",
  "pod_uid": "00000000-0000-0000-0000-000000000000",
  "pv_name": "pv-sandbox-nfs-demo",
  "source_sub_path": "users/alice/workspace",
  "target_path": "/workspace/data",
  "container_name": "main"
}
```

字段契约：

| 字段 | 是否必填 | 说明 |
| --- | --- | --- |
| `api_version` | 是 | 必须是 `kruise-agents-nfs-csi.zhida/v1alpha1`。 |
| `driver_name` | 是 | 必须匹配 wrapper `DRIVER_NAME` 和 `pv.spec.csi.driver`。 |
| `namespace` | 是 | 请求 Pod namespace。token service account 和 PV claimRef 都必须属于该 namespace。 |
| `pod_name` | 是 | 请求 Pod name。 |
| `pod_uid` | 是 | 请求 Pod UID，用于防止 Pod name 复用导致误授权。 |
| `pv_name` | 是 | 要挂载的 PersistentVolume。wrapper 从实时 PV 对象读取 NFS source 信息。 |
| `source_sub_path` | 否 | PV 内目录 subPath。为空时挂整个 PV；不为空时必须是相对目录路径，且不能包含 `..`、绝对路径或 symlink 组件。 |
| `target_path` | 是 | 目标业务容器内的绝对路径。wrapper 会用 `path.Clean` 归一化并拒绝敏感路径。 |
| `container_name` | 否 | 目标容器名。为空时优先选择带 `SANDBOX_MAIN_CONTAINER=true` 的容器；没有标记时只接受单容器 Pod。 |

未知 JSON 字段会被拒绝。请求体超过 1 MiB 会被拒绝。

成功响应：

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

失败响应：

```json
{
  "error": "target path /proc/data is not allowed"
}
```

状态码：

| 状态码 | 含义 |
| --- | --- |
| `200` | mount 请求已完成。 |
| `400` | JSON 非法、缺少必填字段、`api_version` 不匹配、`driver_name` 不匹配、source subPath/目标路径非法，或目标容器选择非法。 |
| `401` | 缺少 bearer token、Authorization 格式错误，或 token 为空。 |
| `403` | Token、Pod、PV、PVC、driver、namespace、claimRef 或节点 mount 校验失败。 |
| `405` | HTTP method 不支持。 |
| `503` | `WRAPPER_ENABLE_MOUNT=false`，节点 mount 操作被禁用。 |

排障时可以用 `curl` 直接调用 UDS：

```sh
token="$(tr -d '\n' < /var/run/secrets/kruise-agents-nfs-csi/token)"
curl --unix-socket /var/lib/kruise-agents-nfs-csi/wrapper.sock \
  -H "Content-Type: application/json" \
  -H "Accept: application/json" \
  -H "Authorization: Bearer ${token}" \
  --data @mount-request.json \
  http://unix/v1/mount
unset token
```

不要把真实 token 打印到日志或写进文档。正常用户集成优先使用
`kruise-nfs-mounter` 命令封装。

## 鉴权与校验

wrapper 只有在下面检查全部通过时才会执行 mount：

| 检查项 | 要求 |
| --- | --- |
| TokenReview | bearer token 必须通过 Kubernetes TokenReview，并包含配置的 `TOKEN_AUDIENCE`。 |
| Namespace | token service account 必须属于请求的 namespace。 |
| Pod 身份 | 实时 Pod namespace、name、UID 和 service account 必须匹配请求与 token。 |
| Pod phase | `Succeeded` 和 `Failed` Pod 会被拒绝。 |
| PV driver | `pv.spec.csi.driver` 必须匹配 `driver_name`。 |
| PV claimRef | PV 必须有 claimRef，且 claimRef namespace 必须是请求 Pod namespace。 |
| PVC 身份 | wrapper 会读取 PV claimRef 指向的实时 PVC，并校验 namespace、name 和存在时的 UID。 |
| Container | 目标容器必须出现在 Pod status 中，并且 container ID 不能为空。 |
| NFS source | PV CSI `volumeAttributes` 必须包含 `server` 和 `share`；`subDir` 可选。 |
| Source subPath | `source_sub_path` 为空时挂整个 PV；不为空时只能指向 PV 内已存在的目录。 |
| Target path | 目标路径必须是绝对路径，且不能指向敏感系统路径或 secret 路径。 |
| Existing mount | 如果目标路径已经是 mount point，wrapper 返回错误，不会主动 unmount。 |

## Source SubPath 规则

`source_sub_path` 是 PV 内部的目录路径，不是业务容器内路径。wrapper 会拒绝：

- 绝对路径；
- 包含 NUL byte 的路径；
- 任何路径段为 `..` 的路径；
- 不存在的目录；
- 任意路径组件是 symlink 的目录。

空值表示挂载整个 PV。当前只支持目录 subPath，不支持文件 subPath。

## Target Path 规则

wrapper 会拒绝：

- `/`、`/proc`、`/sys`、`/dev`；
- `/proc/`、`/sys/`、`/dev/` 下的路径；
- `/run/secrets/` 和 `/var/run/secrets/` 下的 service account 或 Kubernetes secret 路径；
- `/etc/kubernetes/`、`/etc/ssl/private/`、`/root/.kube/`；
- `/var/lib/kubelet/` 和 `/var/lib/kruise-agents-nfs-csi/`；
- `/etc/passwd`、`/etc/shadow`、`/etc/group`；
- 包含 NUL byte 的路径；
- 相对路径。

## Sidecar 集成清单

mounter sidecar 需要：

- `kruise-nfs-mounter` 二进制；
- `DRIVER_NAME`，并且与已安装 driver 和 PV 匹配；
- 从节点 wrapper state 目录挂载的 `WRAPPER_SOCKET_PATH`；
- audience 匹配 `TOKEN_AUDIENCE` 的 `PROJECTED_TOKEN_FILE`；
- 来自 Downward API 环境变量或 projected 文件的 Pod namespace、name 和 UID；
- 非 privileged，且不需要 `SYS_ADMIN`。

wrapper DaemonSet 需要：

- 相同的 `DRIVER_NAME`；
- 相同的 `TOKEN_AUDIENCE`；
- host 上的 wrapper state 目录和 kubelet pod 目录；
- Linux 节点 mount 能力；
- 用于 TokenReview、Pod、PV、PVC 读取的 Kubernetes RBAC。

目标 workload 需要：

- Pod service account 与 mounter projected token 对应；
- PV 已绑定到同 namespace 的 PVC；
- 目标路径是绝对路径且当前不是 mount point；
- 单容器 Pod、`SANDBOX_MAIN_CONTAINER=true` 标记，或显式传入 `container_name` 三者至少满足一个。

## 配置参考

wrapper 环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DRIVER_NAME` | `csi.nfs.zhida` | 请求和 PV 期望的 CSI driver name。 |
| `WRAPPER_SOCKET_PATH` | `/var/lib/kruise-agents-nfs-csi/wrapper.sock` | UDS 路径。 |
| `WRAPPER_SOCKET_MODE` | `0660` | UDS 文件权限。 |
| `TOKEN_AUDIENCE` | `kruise-agents-nfs-csi.zhida/sandbox-mounter` | TokenReview audience。 |
| `WRAPPER_STAGING_ROOT` | `/var/lib/kruise-agents-nfs-csi/staging` | 节点上按 PV staging 的根目录。 |
| `WRAPPER_ENABLE_MOUNT` | `true` | 设置为 `false` 时只验证 API 链路，不做真实 mount。 |
| `WRAPPER_UNSTAGE_AFTER_MOUNT` | `true` | 每次成功 bind mount 后，卸载并删除该 PV 的 staging source。 |
| `WRAPPER_REQUEST_TIMEOUT` | `30s` | Kubernetes 与 mount 操作的单请求超时。 |

mounter 环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DRIVER_NAME` | `csi.nfs.zhida` | 请求中发送的 driver name。 |
| `WRAPPER_SOCKET_PATH` | `/var/lib/kruise-agents-nfs-csi/wrapper.sock` | wrapper UDS 路径。 |
| `PROJECTED_TOKEN_FILE` | `/var/run/secrets/kruise-agents-nfs-csi/token` | projected service account token。 |
| `POD_NAMESPACE` | 空 | Pod namespace。 |
| `POD_NAME` | 空 | Pod name。 |
| `POD_UID` | 空 | Pod UID。 |
| `CONTAINER_NAME` | 空 | 可选目标业务容器。 |
| `MOUNTER_HTTP_TIMEOUT` | `15s` | wrapper HTTP client 超时。 |

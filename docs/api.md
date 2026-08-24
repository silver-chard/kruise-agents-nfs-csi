# 用户调用 API

本文梳理动态 NFS mount 能直接给用户或运行时集成方调用的接口。

推荐的用户调用面是 `kruise-nfs-mounter` 命令，或
`github.com/silver-chard/kruise-agents-nfs-csi/mounter` Go SDK。两者都是
wrapper Unix Domain Socket API 的低权限客户端。底层 socket 只应该挂载给可信
runtime 或 mounter sidecar，不应该直接暴露给不可信业务容器。

## API 总览

| 层次 | 调用方 | 接口 | 用途 |
| --- | --- | --- | --- |
| 运行时命令 | OpenKruise runtime | `kruise-nfs-mounter mount --driver <driver> --config <base64 NodePublishVolumeRequest>` | 推荐集成方式。mounter 解析 CSI 输入后调用 wrapper，可选 PV 内目录 subPath。 |
| 直接命令 | 可信 sidecar 或排障会话 | `kruise-nfs-mounter mount --pv <pv> --sub-path <dir> --target <path>` | 调用方已经知道 PV、可选 subPath 和目标路径时使用，适合调试或封装更上层接口。 |
| Go SDK | 可信 runtime 或 sidecar 中的 Go 代码 | `mounter.NewClient(...).Mount(...)` | 不启动子进程，直接复用与 mounter 命令相同的低权限 UDS 协议、鉴权和重协调语义。 |
| 健康检查 | 节点本地检查器 | `GET /healthz` over wrapper UDS | 确认 wrapper 进程正在服务，并返回当前 driver name。 |
| Mount API | mounter sidecar | `POST /v1/mount` over wrapper UDS | 底层 JSON API，需要 projected service account bearer token。 |
| Unmount API | mounter sidecar | `POST /v1/unmount` over wrapper UDS | 删除期望挂载并卸载当前目标，使用与 mount 相同的鉴权模型。 |

所有请求和响应 JSON 字段都使用 `snake_case`。

## v0.0.2 wrapper 启动配置

v0.0.2 新增缺失 SourceSubPath 的可选创建策略。这个策略属于 wrapper
部署配置，不是单次 mount 请求字段：

| 环境变量 | Wrapper flag | Helm value | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| `WRAPPER_CREATE_MISSING_SUBPATHS` | `--create-missing-subpaths` | `wrapper.createMissingSubPaths` | `false` | 是否逐级创建缺失的 `source_sub_path`。默认关闭，保持原有“目录必须已存在”的行为。 |
| `WRAPPER_CREATED_SUBPATH_MODE` | `--created-subpath-mode` | `wrapper.createdSubPathMode` | `0770` | 新建每一级目录传给 `mkdirat` 的八进制 mode。实际 mode 仍受进程 umask 和文件系统/NFS default ACL 影响。 |

mode 必须是 `0001` 到 `07777` 范围内的八进制权限字符串；`0000` 或非法值会
导致 wrapper 启动失败。wrapper 不会为新建或已有目录执行 `chown`，也不会修改
已有目录的 mode。

## 调用链路

1. 可信 runtime 选择调用 `kruise-nfs-mounter`，或在 Go 进程中调用 `mounter.Client`。
2. 调用方从参数、环境变量或 Downward API projected 文件取得 Pod 身份；SDK
   调用方把 namespace、Pod name 和 Pod UID 填入请求。
3. 命令或 SDK 从配置的 token 文件读取 projected service account token。
4. 命令或 SDK 通过节点 wrapper Unix socket 发送 `POST /v1/mount`。
5. wrapper 校验 token、实时 Pod、PV、PVC、目标容器、driver name 和目标路径。
6. wrapper 在节点上临时 stage NFS PV，并把整个 PV 或 PV 内指定目录 bind-mount 到目标容器的 mount namespace。
7. wrapper 在节点持久化不含 token 的期望挂载；同一 Pod UID 的目标容器 ID 变化后，重新校验实时 Pod/PV/PVC 并挂入新的 mount namespace。

业务容器不应该拿到 wrapper socket、projected wrapper token，或可任意选择 PV 的配置。

每个节点 wrapper 只维护一个按本节点过滤的 Pod `SharedIndexInformer`，由 client-go 处理 LIST/WATCH、本地 cache、resourceVersion 和断线重连，不会按 Pod 周期轮询。容器重启恢复仍是最终一致的：从容器创建到对应 Pod status 事件处理完成之间，目标路径会有一个短暂未挂载窗口。需要在进程第一条指令前保证挂载的业务，仍应由 runtime 增加启动门禁或重试。

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
| `--sub-path` | 否 | `NodePublishVolumeRequest` 中的 subPath 或空 | PV 内目录 subPath。不传时挂整个 PV；默认只支持已存在目录，wrapper 显式开启创建策略后可以安全创建缺失目录。 |
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
kruise-nfs-mounter mount \
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

## Go SDK

Go 程序可以直接引用：

```go
import "github.com/silver-chard/kruise-agents-nfs-csi/mounter"
```

SDK 公开的主要接口是：

```go
func NewClient(Config) (*Client, error)
func (*Client) Mount(context.Context, MountRequest) (*MountResult, error)
func (*Client) Unmount(context.Context, UnmountRequest) (*UnmountResult, error)
func (*Client) Health(context.Context) (*HealthResult, error)
func (*Client) CloseIdleConnections()
```

`Config` 字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `DriverName` | `string` | 请求使用的 CSI driver name，必须与 wrapper 和 PV 一致。 |
| `SocketPath` | `string` | 当前容器内可见的 wrapper Unix socket。 |
| `TokenFile` | `string` | projected service account token 文件。SDK 在每次 mount/unmount 时重新读取，以兼容 token 轮换。 |
| `HTTPTimeout` | `time.Duration` | UDS HTTP 请求超时；为 `0` 时使用 15 秒默认值。 |
| `DisableHTTPTimeout` | `bool` | 显式关闭 client 级请求超时；通常不应启用，并应为每次调用传入有界 context。 |

`MountRequest` 包含 `Namespace`、`PodName`、`PodUID`、`PVName`、
`SourceSubPath`、`TargetPath` 和 `ContainerName`。`UnmountRequest`
包含相同的身份和目标字段，但不包含 `SourceSubPath`。

SDK 只是现有 mounter 的进程内客户端封装：

- 不直接执行 mount；
- 不访问 CSI socket 或 host `/proc`；
- 不需要 privileged 或 `SYS_ADMIN`；
- 仍然需要 wrapper socket、projected token 和 Downward API Pod identity；
- 所有 mount/unmount 仍由 wrapper 执行 TokenReview，并重新检查实时
  Pod、PV、PVC、目标容器、driver 和目标路径；
- 成功挂载仍由节点 wrapper 持久化期望状态并负责容器重启后的重协调。

不要为了使用 SDK 把 wrapper socket 和 projected token 交给不可信业务代码。
SDK 应运行在可信 runtime 或专用 sidecar 中。完整配置、可编译风格示例和错误处理
见 [Go SDK 使用说明](sdk.md)。

SDK 会自动使用当前模块的 wrapper API 版本，wrapper 会严格校验该版本。SDK 依赖
与节点 wrapper 镜像应固定到同一个发布版本或 commit，并一起升级；健康检查不提供
协议版本协商。

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
| `source_sub_path` | 否 | PV 内目录 subPath。为空时挂整个 PV；不为空时必须是相对目录路径，且不能包含 `..`、绝对路径或 symlink 组件。目录默认必须存在；是否创建缺失目录由 wrapper 全局策略决定。 |
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
| `400` | JSON 非法、缺少必填字段、`api_version` 不匹配、`driver_name` 不匹配、source subPath/目标路径非法、缺失 subPath 且创建策略关闭，或目标容器选择非法。 |
| `401` | 缺少 bearer token、Authorization 格式错误，或 token 为空。 |
| `403` | Token、Pod、PV、PVC、driver、namespace 或 claimRef 鉴权/实时资源校验失败。 |
| `405` | HTTP method 不支持。 |
| `500` | 鉴权通过后节点 mount/unmount 操作失败，包括自动创建目录时的 `EACCES`、`EROFS`、`ENOSPC` 或 `EDQUOT` 等存储错误。 |
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
| Source subPath | `source_sub_path` 为空时挂整个 PV；不为空时必须是安全的相对目录。默认必须已存在，只有 wrapper 显式开启创建策略后才会创建缺失目录。 |
| Target path | 目标路径必须是绝对路径，且不能指向敏感系统路径或 secret 路径。 |
| Existing mount | 如果目标路径已经是 mount point，wrapper 返回错误，不会主动 unmount。 |

## Source SubPath 规则

`source_sub_path` 是 PV 内部的目录路径，不是业务容器内路径。无论是否启用
自动创建，wrapper 都会拒绝：

- 绝对路径；
- 包含 NUL byte 的路径；
- 任何路径段为 `..` 的路径；
- 任意已有路径组件是 symlink；
- 任意已有路径组件不是目录。

空值表示挂载整个 PV。当前只支持目录 subPath，不支持文件 subPath。默认配置
`WRAPPER_CREATE_MISSING_SUBPATHS=false` 下，任意组件不存在也会被拒绝。

启用 `WRAPPER_CREATE_MISSING_SUBPATHS=true` 后，wrapper 使用逐级、禁止跟随 symlink
的方式创建缺失组件。每一级请求使用 `WRAPPER_CREATED_SUBPATH_MODE`（默认
`0770`）传给 `mkdirat`，但实际 mode 会受到 wrapper 进程 umask 和文件系统/NFS
default ACL 约束。已经存在的目录不会被 `chmod` 或 `chown`；新目录也不会被
`chmod` 或 `chown`。如果中间创建失败，之前成功创建的父目录不会回滚。

NFS export 权限仍然是最终约束。启用 `root_squash` 时，wrapper 的 root 身份通常
会映射成匿名 UID/GID，因此创建可能失败，或者新目录 owner 与业务容器不匹配。
mode `0770` 在 group 不一致时仍会阻止业务访问。对 owner/group 有明确要求时，应
预先按目标 UID/GID 创建目录，或先对 export policy、匿名身份、umask、default ACL、
group 和 mode 做完整验证。

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

## Sidecar 与 SDK 集成清单

mounter sidecar 或 SDK 调用方需要：

- 命令模式需要 `kruise-nfs-mounter` 二进制；SDK 模式需要 Go 包
  `github.com/silver-chard/kruise-agents-nfs-csi/mounter`；
- `DRIVER_NAME`，并且与已安装 driver 和 PV 匹配；
- 从节点 wrapper state 目录挂载的 `WRAPPER_SOCKET_PATH`；
- audience 匹配 `TOKEN_AUDIENCE` 的 `PROJECTED_TOKEN_FILE`；
- 来自 Downward API 环境变量或 projected 文件的 Pod namespace、name 和 UID；
- 非 privileged，且不需要 `SYS_ADMIN`。

wrapper DaemonSet 需要：

- 相同的 `DRIVER_NAME`；
- 相同的 `TOKEN_AUDIENCE`；
- host 上的 wrapper state 目录和 kubelet pod 目录；
- `WRAPPER_MOUNT_STATE_DIR` 必须持久化到节点，并且只允许 wrapper 写入；
- 如需自动创建缺失 subPath，显式设置
  `WRAPPER_CREATE_MISSING_SUBPATHS=true`，并确认
  `WRAPPER_CREATED_SUBPATH_MODE`、进程 umask 和 NFS export 权限；
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
| `WRAPPER_SOCKET_MODE` | `0660` | UDS 文件权限；chart 默认让 socket group 可写，mounter 容器需要使用相同 group。 |
| `TOKEN_AUDIENCE` | `kruise-agents-nfs-csi.zhida/sandbox-mounter` | TokenReview audience。 |
| `WRAPPER_STAGING_ROOT` | `/var/lib/kruise-agents-nfs-csi/staging` | 节点上按 PV staging 的根目录。 |
| `WRAPPER_MOUNT_STATE_DIR` | `/var/lib/kruise-agents-nfs-csi/mounts` | 节点持久化期望挂载状态；每个挂载独立写一个 `0600` 文件，不保存 token。 |
| `WRAPPER_NODE_NAME` | `NODE_ID`、`KUBE_NODE_NAME` 或空 | wrapper 所在 Kubernetes 节点名，用于建立仅过滤本节点 Pod 的 informer；chart 通过 Downward API 注入。为空时禁用重协调。 |
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

Go SDK 不隐式读取上述 mounter 环境配置。调用方通过 `mounter.Config` 设置
`DriverName`、`SocketPath`、`TokenFile` 和 `HTTPTimeout`，并把从
Downward API 获得的 Pod namespace、name 和 UID 填入每个 mount/unmount 请求。

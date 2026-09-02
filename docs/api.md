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

## Wrapper 启动配置

缺失 SourceSubPath 创建策略和可选的 NFS export root key 都属于 wrapper
部署配置，不是 mount JSON 请求字段：

| 环境变量 | Wrapper flag | Helm value | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| `WRAPPER_CREATE_MISSING_SUBPATHS` | `--create-missing-subpaths` | `wrapper.createMissingSubPaths` | `false` | 是否逐级创建缺失的 `source_sub_path`。默认关闭，保持原有“目录必须已存在”的行为。 |
| `WRAPPER_CREATED_SUBPATH_MODE` | `--created-subpath-mode` | `wrapper.createdSubPathMode` | `0770` | 新建每一级目录传给 `mkdirat` 的八进制 mode。实际 mode 仍受进程 umask 和文件系统/NFS default ACL 影响。 |
| `WRAPPER_EXPORT_ROOT_KEY_FILE` | `--export-root-key-file` | `wrapper.exportRootKeySecret.name` / `key` | 空 | 可选的 wrapper 侧 NFS export root capability key 文件。不配置时 root mount 只受 annotation 等既有规则限制；配置后 root mount 额外要求相同 key。 |

mode 必须是 `0001` 到 `07777` 范围内的八进制权限字符串；`0000` 或非法值会
导致 wrapper 启动失败。wrapper 不会为新建或已有目录执行 `chown`，也不会修改
已有目录的 mode。

配置 export root key 时，去掉首尾空白后的内容必须是 32 到 4096 个可见
ASCII 字符（不含空格）。wrapper 启动时读取并保存其 SHA-256 摘要；替换文件后
需要重启 wrapper 才能生效。配置文件路径但文件不存在、为空或格式非法会导致
wrapper 启动失败，而完全不配置表示不启用额外 root-key 限制。不要把 key 写进
PV annotation、mount JSON、日志或节点持久化 mount state。

## 调用链路

1. 可信 runtime 选择调用 `kruise-nfs-mounter`，或在 Go 进程中调用 `mounter.Client`。
2. 调用方从参数、环境变量或 Downward API projected 文件取得 Pod 身份；SDK
   调用方把 namespace、Pod name 和 Pod UID 填入请求。
3. 命令或 SDK 从配置的 token 文件读取专用 audience 的、绑定当前
   Pod 的 projected service account token。
4. 命令或 SDK 通过节点 wrapper Unix socket 发送 `POST /v1/mount`。
5. wrapper 通过 TokenReview 校验 audience、ServiceAccount 和 token 绑定的精确
   Pod name/UID，再校验实时 Pod、PV annotation、目标容器、driver name
   和目标路径。
6. wrapper 在节点上临时 stage NFS PV，并把整个 PV 或 PV 内指定目录 bind-mount 到目标容器的 mount namespace。
7. wrapper 在节点持久化不含 token、export root key 或 NFS 凭据的期望
   挂载；同一 Pod UID 的目标容器 ID 变化后，重新校验实时
   Pod、PV driver 和 annotation，再挂入新的 mount namespace。

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
| `--export-root-key-file` | 否 | `EXPORT_ROOT_KEY_FILE` 或空 | NFS export root capability key 文件。仅 wrapper 已启用该限制且有效路径是 export root 时需要。 |

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
专用 audience 的 Pod-bound projected token。如果该 PV 的有效 NFS 路径是
export root，且 wrapper 已配置 root key，还需通过 `--export-root-key-file` 提供
相同 key；非 root 挂载以及未启用该限制的 wrapper 不需要。

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
| `ExportRootKeyFile` | `string` | 可选 NFS export root capability key 文件。配置后 SDK 仅在 `Mount` 时重新读取并放入 HTTP header；`Unmount` 不读取、不发送。 |
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
- 所有 mount/unmount 仍由 wrapper 执行 TokenReview，严格匹配 token 绑定的
  Pod name/UID 与实时 Pod；
- mount 还会检查实时 PV、PV annotation、目标容器、driver、路径，以及
  wrapper 已配置时的 export root key（仅 root 挂载）；key 不绕过其他检查；
- unmount 不发送 export root key，也不重新核验 PV annotation，只会清理
  该精确 Pod/container/target 已登记的 mount state 和对应 mount；
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
X-Kary-Export-Root-Key: <export-root-key>
```

`X-Kary-Export-Root-Key` 不是 JSON 字段；只有 wrapper 配置了 root key 且
请求有效 NFS export root 时才要求它。有效 NFS 路径在 export root 之下时应
省略；mounter/SDK 配置了 key 文件时可能仍会发送该 header，wrapper 会对非
root mount 忽略它。未配置 wrapper key 时，root mount 不依赖该 header。

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
| `namespace` | 是 | 请求 Pod namespace。必须与 token ServiceAccount namespace、实时 Pod 和 token Pod-bound identity 一致。 |
| `pod_name` | 是 | 请求 Pod name，必须同时匹配实时 Pod 和 TokenReview 返回的 bound Pod name。 |
| `pod_uid` | 是 | 请求 Pod UID，必须同时匹配实时 Pod 和 TokenReview 返回的 bound Pod UID。 |
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
| `403` | Token/audience/bound Pod、PV driver/annotation 或 export root key 鉴权失败。 |
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

上例挂载了非空 `source_sub_path`，因此不需要 export root key。调试已启用
root-key 限制的 export root mount 时，必须额外从文件读取 key 并发送
`X-Kary-Export-Root-Key` header。不要把 key 写入 shell history、请求文件或
日志；正常集成优先使用 CLI 的 `--export-root-key-file` 或 SDK
`ExportRootKeyFile`。

不要把真实 token 打印到日志或写进文档。正常用户集成优先使用
`kruise-nfs-mounter` 命令封装。

### POST /v1/unmount

unmount 请求只发送 `Authorization: Bearer <projected-service-account-token>`，
不发送 `X-Kary-Export-Root-Key`。wrapper 会再次校验专用 audience、token
绑定的精确 Pod、实时 Pod/node 和目标容器，然后按
`(pod_uid, container_name, target_path)` 查找持久化 state：

- 没有已登记 state 时幂等返回成功，不卸载未受 wrapper 管理的 mount；
- state 的 PV 必须与请求 `pv_name` 一致；
- state 的 container ID 必须与实时目标容器一致；若容器已经换代，只删除旧
  state，不触碰新容器 namespace 中的同路径 mount；
- 完全匹配时先删除 state，再卸载当前容器 namespace 中的目标；
- 不重新获取 PV，不重新校验 PV annotation，也不需要 export root key。

这个收敛式语义保证管理员收紧 PV annotation 或轮换 export root key 后，
原 Pod 仍能清理之前成功登记的挂载。

## 鉴权与校验

wrapper 只有在下面检查全部通过时才会执行 mount：

| 检查项 | 要求 |
| --- | --- |
| TokenReview | bearer token 必须通过 Kubernetes TokenReview，并包含配置的 `TOKEN_AUDIENCE`。 |
| Namespace | token service account 必须属于请求的 namespace。 |
| Exact Pod token | TokenReview `status.user.extra` 必须各返回且只返回一个 `authentication.kubernetes.io/pod-name` 和 `authentication.kubernetes.io/pod-uid`，并与请求和实时 Pod 精确匹配。未绑定 Pod 的 ServiceAccount token 会被拒绝。 |
| Pod 身份 | 实时 Pod namespace、name、UID 和 service account 必须匹配请求与 token，且 Pod 必须在当前 wrapper 节点。 |
| Pod phase | `Succeeded` 和 `Failed` Pod 会被拒绝。 |
| PV driver | `pv.spec.csi.driver` 必须匹配 `driver_name`。 |
| PV namespace allowlist | PV 存在 `kary.dev/allow-namespace` 时，实时 Pod namespace 必须在逗号分隔的 allowlist 中；annotation 缺失时该维度不限制。 |
| PV ServiceAccount allowlist | PV 存在 `kary.dev/allow-serviceaccount` 时，实时 Pod `spec.serviceAccountName` 必须在 allowlist 中；annotation 缺失时该维度不限制。 |
| Container | 目标容器必须出现在 Pod status 中，并且 container ID 不能为空。 |
| NFS source | PV CSI `volumeAttributes` 必须包含 `server` 和 `share`；`subDir` 可选。 |
| Export root | 归一化后的 PV `subDir` 和请求 `source_sub_path` 都为空时属于 export root。wrapper 未配置 key 时只执行其他授权规则；配置 key 时必须额外提供相同 key，且 key 不绕过 annotation。state v2 对通过完整策略的 root intent 保存授权标志，并只在 key 模式保存 fingerprint。 |
| Source subPath | `source_sub_path` 为空时挂整个 PV；不为空时必须是安全的相对目录。默认必须已存在，只有 wrapper 显式开启创建策略后才会创建缺失目录。 |
| Target path | 目标路径必须是绝对路径，且不能指向敏感系统路径或 secret 路径。 |
| Existing mount | 如果目标路径已经是 mount point，wrapper 返回错误，不会主动 unmount。 |

### PV annotation 授权

PV 不需要 PVC 或 `claimRef` 才能被 wrapper 挂载。wrapper 不使用 PV
`claimRef` 或 PVC 作为 mount 授权；无论 PV 是否已绑定，都只按实时
Pod identity、PV driver 和下面两个可选 annotation 决定访问：

```yaml
metadata:
  annotations:
    kary.dev/allow-namespace: "team-a, team-b"
    kary.dev/allow-serviceaccount: "runtime-a, runtime-b"
```

- annotation key 完全缺失表示该维度 unrestricted；两个 key 都缺失时，
  任何能通过 exact Pod token、socket、driver 和路径检查的 Pod 都可请求该 PV。
- key 存在时，值是逗号分隔的精确 allowlist；每项会执行
  `TrimSpace`，匹配区分大小写。
- 空值、空项（包括开头/结尾逗号或连续逗号）和 `*` 都是非法
  配置并 fail closed；如果需要 unrestricted，应删除该 key。
- 两个 key 都存在时使用 AND：namespace 和 ServiceAccount name 必须
  同时命中。ServiceAccount annotation 只写 name，namespace 由另一维度限制。

因此，不要把“不写 annotation”理解为默认隔离。它表示对所有能同时
拿到 wrapper socket 和当前 Pod 专用 token 的可信 caller 开放该 PV。

## 有效 NFS 路径与 export root

wrapper 使用 PV CSI `volumeAttributes["subDir"]`（兼容小写
`subdir`）和请求 `source_sub_path` 一起判定有效 NFS 路径。这里的
export root 是 PV `server` + `share` 指定的 NFS share root，不是节点或
NFS server 的文件系统 `/`。

| 归一化 PV `subDir` | 归一化 `source_sub_path` | 有效位置 | 额外 key 校验 |
| --- | --- | --- | --- |
| 空 | 空 | `share` root | 仅 wrapper 配置 key 时 |
| 空 | `users/alice` | `share/users/alice` | 否 |
| `tenants/team-a` | 空 | `share/tenants/team-a` | 否 |
| `tenants/team-a` | `workspace` | `share/tenants/team-a/workspace` | 否 |

PV `subDir` 会去掉首尾空白并归一化；空、仅 `/` 或仅 `.` 的路径
都表示 share root，包含 NUL 或任何 `..` 路径段会被拒绝。
`source_sub_path` 空或归一化为 `.` 时表示 PV root；绝对路径、NUL 和
`..` 会被拒绝。只要两者任一归一化后非空，有效路径就在
export root 之下，不使用 key。

wrapper 没有配置 `WRAPPER_EXPORT_ROOT_KEY_FILE` 时，export root mount 只受
exact Pod token、实时 Pod、PV annotation、driver、container 和路径等既有规则
限制。配置 key 后，mount client 必须通过 `X-Kary-Export-Root-Key` header 提供
相同值；持有 key 仍不能绕过 annotation 或任何其他检查。

state v2 的 `export_root_authorized` 表示 root intent 已通过当时的完整策略。
annotation-only root mount 也设置该标志，但 fingerprint 为空；key 模式还保存
authorizing key 的 SHA-256 fingerprint。state 不保存 key 原文、header、token
或可重放凭据。旧 state v2 可以直接复用。state v1 没有 root/非 root 分类，加载
后以内存 unknown 标记：目标容器换代触发重协调时，wrapper 未配置 key 会按当前
live PV plan 和 policy 补齐分类并可恢复；wrapper 配置 key 且当前 plan 是
export root 时 fail closed，需由精确 Pod 身份 `Unmount` 后再携带有效 key 重新
`Mount`。当前 plan 为非 root 时不需要 key。

wrapper 只在启动时读取 key，而 SDK/mounter 每次 mount 都读取客户端 key 文件。
新增或轮换 key 后，旧的无 fingerprint 或旧 fingerprint root intent 不会在 key
模式自动恢复；持有当前 key 的可信 caller 可重复相同 mount 请求，无叠加地升级
或刷新 state。移除 wrapper key 后，annotation-only 模式可以在重新校验现有规则
后恢复 root intent，并清除旧 fingerprint。模式或 key 变化都不会主动卸载已有
Linux mount。

普通 state v2 的同一 target 若要切换 mount intent 或 root/非 root 分类，会 fail
closed，必须先 `Unmount` 再 `Mount`；幂等重复请求只用于刷新完全相同的已登记
intent。

v1.1.1 继续写 state v2，v1.1.0 可以解析该格式，但会把“已授权且 fingerprint
为空”的 annotation-only root state 作为 fail closed 处理，并可能在重协调时
清理它。回滚到 v1.1.0 前应显式 unmount 这类 intent。

## Source SubPath 规则

`source_sub_path` 是 PV 内部的目录路径，不是业务容器内路径。无论是否启用
自动创建，wrapper 都会拒绝：

- 绝对路径；
- 包含 NUL byte 的路径；
- 任何路径段为 `..` 的路径；
- 任意已有路径组件是 symlink；
- 任意已有路径组件不是目录。

空值表示挂载整个 PV；如果 PV `subDir` 也归一化为空，这是 export root mount，
并在 wrapper 已配置 key 时额外通过 root key 鉴权。当前只支持目录 subPath，
不支持文件 subPath。默认配置
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
- audience 匹配 `TOKEN_AUDIENCE`、且由当前 Pod 的
  `serviceAccountToken` projected volume 签发的 `PROJECTED_TOKEN_FILE`；TokenReview
  必须能返回与当前 Pod 一致的 bound Pod name/UID extra；
- 来自 Downward API 环境变量或 projected 文件的 Pod namespace、name 和 UID；
- 只在 wrapper 已启用 root-key 限制且调用方需要 export root mount 时挂载相同
  key 文件，并为 CLI 设置 `EXPORT_ROOT_KEY_FILE` / `--export-root-key-file`，或
  为 SDK 设置 `Config.ExportRootKeyFile`；
- 非 privileged，且不需要 `SYS_ADMIN`。

wrapper DaemonSet 需要：

- 相同的 `DRIVER_NAME`；
- 相同的 `TOKEN_AUDIENCE`；
- 如需为 export root 增加共享 key 限制，使用 `WRAPPER_EXPORT_ROOT_KEY_FILE` /
  `--export-root-key-file` 或 Helm `wrapper.exportRootKeySecret` 挂载 key；不配置
  表示 root mount 只受 annotation 等既有规则限制；
- host 上的 wrapper state 目录和 kubelet pod 目录；
- `WRAPPER_MOUNT_STATE_DIR` 必须持久化到节点，并且只允许 wrapper 写入；
- 如需自动创建缺失 subPath，显式设置
  `WRAPPER_CREATE_MISSING_SUBPATHS=true`，并确认
  `WRAPPER_CREATED_SUBPATH_MODE`、进程 umask 和 NFS export 权限；
- Linux 节点 mount 能力；
- 用于 TokenReview、Pod 和 PV 读取的 Kubernetes RBAC。

目标 workload 需要：

- Pod service account 与 mounter projected token 对应；
- PV 的可选 `kary.dev/allow-namespace` 和
  `kary.dev/allow-serviceaccount` 允许该 Pod；PV 不需要 PVC 或 `claimRef`；
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
| `WRAPPER_CREATE_MISSING_SUBPATHS` | `false` | 是否逐级创建缺失的 `source_sub_path`。 |
| `WRAPPER_CREATED_SUBPATH_MODE` | `0770` | 创建缺失 subPath 时请求的八进制 mode。 |
| `WRAPPER_EXPORT_ROOT_KEY_FILE` | 空 | 可选 wrapper 侧 export root key 文件。空值表示不增加 root-key 检查；其他授权规则仍然生效。 |

mounter 环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DRIVER_NAME` | `csi.nfs.zhida` | 请求中发送的 driver name。 |
| `WRAPPER_SOCKET_PATH` | `/var/lib/kruise-agents-nfs-csi/wrapper.sock` | wrapper UDS 路径。 |
| `PROJECTED_TOKEN_FILE` | `/var/run/secrets/kruise-agents-nfs-csi/token` | projected service account token。 |
| `EXPORT_ROOT_KEY_FILE` | 空 | 可选 export root key 文件；仅需要访问 key-protected export root 的 caller 配置。 |
| `POD_NAMESPACE` | 空 | Pod namespace。 |
| `POD_NAME` | 空 | Pod name。 |
| `POD_UID` | 空 | Pod UID。 |
| `CONTAINER_NAME` | 空 | 可选目标业务容器。 |
| `MOUNTER_HTTP_TIMEOUT` | `15s` | wrapper HTTP client 超时。 |

Go SDK 不隐式读取上述 mounter 环境配置。调用方通过 `mounter.Config` 设置
`DriverName`、`SocketPath`、`TokenFile`、可选 `ExportRootKeyFile` 和
`HTTPTimeout`，并把从
Downward API 获得的 Pod namespace、name 和 UID 填入每个 mount/unmount 请求。

# 项目上下文与专利背景材料

本文用于整理 `kruise-agents-nfs-csi` 项目的技术背景、当前实现、设计演进和仍待确认的问题。它不是法律意见，也不替代正式专利交底书。

标注规则：

- 【代码事实】：当前仓库中可以直接从代码、Chart、demo 或提交历史确认的内容。
- 【设计规划】：已经在设计中明确表达，但当前代码尚未完整实现的内容。
- 【推测/待确认】：从问题背景或线程记忆推导而来，需要后续技术或法律评审确认的内容。

敏感信息处理：本文不记录 token、密钥、内部访问地址、客户数据、临时集群对象、节点 IP、内部镜像仓库地址或个人证件信息。涉及部署环境的 driver name、镜像仓库和集群对象均以“部署时覆盖值”“内部测试环境”等中性描述表达。

## 1. 项目解决的业务问题

【代码事实】本项目实现一套围绕 upstream NFS CSI driver 的轻量 wrapper。仓库 README 明确说明：保留 upstream NFS CSI driver 行为，在节点侧增加 wrapper agent，并提供低权限 mounter client；mounter 通过 Unix domain socket 调用 wrapper，不直接执行 mount，也不获得 `SYS_ADMIN` 或 host `/proc` 访问能力。

【设计背景】目标场景是沙箱或类似多租户工作负载已经启动后，需要把一个 NFS/PV 目录动态挂载到目标业务容器内的任意业务路径。业务容器镜像、启动命令和挂载路径可能不可控，因此不能依赖业务镜像预装 NFS 工具、不能要求业务容器自行 mount，也不能把高危权限下放给业务容器。

【设计背景】传统做法如果让 sidecar 或业务容器完成 NFS mount，通常需要至少满足一部分条件：`SYS_ADMIN`、privileged、可见 host `/proc`、可见 CSI socket、可直接接触 NFS helper 或 kubelet 插件路径。这些条件会扩大业务 Pod 的权限面。本项目选择把 mount、bind mount、进入目标容器 mount namespace 的能力集中在节点 DaemonSet wrapper 中，沙箱侧只保留 API client 能力。

【代码事实】driver name 是配置项，默认值在 `internal/config/config.go:10` 附近定义为 `csi.nfs.zhida`，mounter、wrapper、entrypoint、Chart 和 demo 都通过同一类配置传递 driver name，而不是在代码调用链中写死某个部署环境名称。

## 2. 现有 Kubernetes CSI 挂载流程及其限制

【设计背景】标准 Kubernetes CSI 流程通常由 kubelet 的 volume manager 根据 Pod spec 中声明的 volume 触发。典型路径是：

1. Controller sidecar 处理 PVC/PV 创建、扩容等控制面动作。
2. kubelet 在节点侧调用 CSI Node 服务。
3. CSI Node 服务执行 `NodeStageVolume` / `NodePublishVolume`，把卷发布到 kubelet 管理的 Pod volume 目录。
4. kubelet/container runtime 在创建容器时把这些 volume 作为容器 mount 传入。

【设计背景】这个流程适合 Pod 创建前已在 Pod spec 中声明的 volume，但不适合以下需求：

- Pod 已经运行后，再把某个 PV 动态挂到目标容器的任意路径。
- 目标路径不是 kubelet volumeMount 中预声明的路径。
- 业务容器不可改镜像、不可加 mount helper、不可加 `SYS_ADMIN`。
- 不希望 runtime sidecar 直接拿到 CSI socket 或 host `/proc`。
- NFS 实际 mount 依赖节点网络、hostNetwork、kubelet 插件目录或节点级 helper，放在 Pod namespace 内不稳定。

【代码事实】本项目并没有替换标准 CSI 控制面能力。Chart 的 controller Deployment 仍包含 CSI provisioner/resizer/liveness probe 和 upstream `nfsplugin` 形态的容器，见 `charts/kruise-agents-nfs-csi/templates/controller.yaml`。节点 DaemonSet 的 `nfs` 容器同时运行 upstream `nfsplugin` 和 wrapper agent，见 `build/wrapper/entrypoint.sh:13` 与 `build/wrapper/entrypoint.sh:35`。

## 3. 当前方案的完整系统架构

【代码事实】当前实现包含以下组件：

- mounter：`cmd/mounter/main.go`，运行在 runtime sidecar 中，是低权限 CLI/API client。
- wrapper agent：`cmd/wrapper/main.go` 与 `internal/wrapper`，运行在节点 DaemonSet 中，通过 Unix socket 暴露 `/v1/mount`。
- upstream NFS CSI driver：通过 wrapper 镜像保留 `/nfsplugin`，entrypoint 默认启动 upstream `nfsplugin`。
- Kubernetes REST client：`internal/kube/client.go`，用于 TokenReview、Pod、PV、PVC 查询。
- 节点 mount 执行器：`internal/node/mounter_linux.go`，负责 staging NFS、查找目标容器 PID、进入 mount namespace、执行 move mount。
- 安全校验：`internal/security/targetpath.go` 与 `internal/wrapper/authz.go`。

【代码事实】当前通信协议是 HTTP over Unix domain socket。wrapper 监听 socket 的逻辑在 `cmd/wrapper/main.go:90` 的 `listenUnix`；mounter 通过自定义 `http.Transport.DialContext` 连接 UDS，见 `cmd/mounter/main.go:217` 的 `callWrapper`。

架构图：

```text
Sandbox/Workload Pod
  ├─ target container
  │    └─ receives final bind mount at requested path
  │
  └─ runtime sidecar
       ├─ projected service account token, dedicated audience
       ├─ Downward API pod name/namespace/uid
       └─ kruise-nfs-mounter
            │ HTTP+JSON over UDS, no TCP port
            ▼
Node DaemonSet
  └─ wrapper nfs container
       ├─ upstream nfsplugin keeps normal CSI Node behavior
       ├─ wrapper agent validates token + live Pod/PV/PVC
       ├─ mounts NFS PV to node staging path
       ├─ finds target container mount namespace
       └─ clones/moves mount into target container path

Kubernetes API
  ├─ TokenReview
  ├─ Pod
  ├─ PersistentVolume
  └─ PersistentVolumeClaim
```

【代码事实】Chart 中 node DaemonSet 的 wrapper/nfs 容器是 privileged 并增加 `SYS_ADMIN`，同时挂载 kubelet pods 目录和 wrapper state 目录；mounter demo 则配置为 `privileged: false`、`allow_privilege_escalation: false`、`read_only_root_filesystem: true`、drop all capabilities，见 `charts/kruise-agents-nfs-csi/templates/node.yaml` 与 `demo/sandbox-inject-configmap.yaml`。

## 4. 各组件职责边界

### Controller

【代码事实】本仓库没有实现新的业务 Controller，也没有实现自定义 CRD controller。当前 controller 相关 YAML 是 upstream CSI 控制面组件编排：provisioner、resizer、liveness probe 和 upstream `nfsplugin`，见 `charts/kruise-agents-nfs-csi/templates/controller.yaml`。

【设计规划】外部 sandbox/runtime controller 的职责是：在 Pod/runtime sidecar 中注入 mounter、projected token、Downward API 信息、wrapper socket hostPath；在动态挂载时向 mounter 传递 CSI 风格的 `NodePublishVolumeRequest` 或等价配置。这个 controller 不应直接持有节点 mount 权限。

### Wrapper CSI / Wrapper Agent

【代码事实】wrapper agent 不是一个完整替代 upstream CSI 的 CSI server。它是节点 DaemonSet 中额外启动的 HTTP/UDS agent，提供 `/v1/mount` API；upstream `nfsplugin` 仍负责原始 CSI 能力。入口脚本通过 `WRAPPER_AGENT_ENABLED` 决定是否启动 wrapper agent，然后再执行 `/nfsplugin`。

【代码事实】wrapper agent 的核心职责：

- 解析 UDS 请求：`internal/wrapper/server.go:59` `handleMount`。
- 校验请求结构和 driver：`internal/wrapper/authz.go:16` `validateRequestShape`。
- TokenReview：`internal/wrapper/server.go:96` `mount` 调用 `ReviewToken`。
- 校验 Pod、PV、PVC、container status：`internal/wrapper/authz.go:40`、`:59`、`:89`、`:111`。
- 构造节点 mount plan：`internal/wrapper/authz.go:151` `buildMountPlan`。
- 调用节点 mount executor：`internal/node/mounter_linux.go:34` `Mount`。

### CSI Driver

【代码事实】upstream NFS CSI driver 的二进制 `/nfsplugin` 被复制进 wrapper 镜像，entrypoint 默认执行 `/nfsplugin --endpoint=... --nodeid=... --drivername=...`。因此普通 PVC provision/publish 仍沿用 upstream driver 的控制面和节点能力。

【代码事实】`Dockerfile.wrapper` 还复制 NFS helper 和运行时依赖文件，以保证 wrapper 镜像不仅有 Go wrapper 二进制，也能保留 upstream NFS mount 所需 helper。

### Runtime Agent / Mounter

【代码事实】mounter 是 client。它支持两种调用形态：

- runtime 风格：`kruise-nfs-mounter mount --driver <driver> --config <base64 NodePublishVolumeRequest>`，解析逻辑在 `cmd/mounter/main.go:86`。
- direct/debug 风格：命令行直接传 `--pv`、`--target`、`--pod-name` 等。

【代码事实】mounter 从环境变量或文件补齐 Pod identity，见 `cmd/mounter/main.go:133` `completePodIdentity`；读取 projected token 后调用 wrapper socket，见 `cmd/mounter/main.go:217` `callWrapper`。mounter 不执行 mount syscall。

### CRI、containerd/CRI-O

【代码事实】当前 wrapper 不直接调用 CRI socket，也没有使用 containerd/CRI-O SDK。它从 Kubernetes Pod status 读取 `containerID`，再扫描节点 `/proc/<pid>/cgroup`，用 Pod UID 和 normalized container ID 匹配目标进程，见 `internal/node/mounter_linux.go:194` `findContainerPID`。

【代码事实】`normalizeContainerID` 支持去掉 `containerd://`、`cri-o://` 这类前缀；如果 cgroup 中包含 Pod UID 和 container ID 或其 12 位短 ID，则认为找到了目标容器进程。

【设计规划】如果不同 CRI/cgroup 版本导致 cgroup 扫描不稳定，后续可以引入 CRI RuntimeService 查询作为替代或补充，但当前代码尚未实现。

### 目标容器

【代码事实】目标容器不需要知道 wrapper socket、projected token 或 CSI socket；它只在自身 mount namespace 内看到最终挂载结果。目标容器选择逻辑优先使用请求中的 `container_name`；如果未指定，则查找 Pod spec 中设置了 `SANDBOX_MAIN_CONTAINER=true` 的容器；如果仍未确定且 Pod 只有一个容器，则使用唯一容器，见 `internal/wrapper/authz.go:111`。

## 5. Pod 启动后动态挂载与卸载调用时序

### 动态挂载时序

【代码事实】当前已实现的动态挂载时序如下：

1. 外部 runtime/controller 在 Pod 中注入 mounter sidecar、projected service account token、Downward API Pod 信息，以及只给 mounter 使用的 wrapper socket 路径。
2. Pod 已运行后，runtime 调用 mounter，例如 `mount --driver ... --config <base64 NodePublishVolumeRequest>`。
3. mounter 解码 CSI `NodePublishVolumeRequest`，从 `VolumeContext`、`PublishContext` 或 `VolumeId` 得到 `pv_name`，从 `TargetPath` 得到目标路径，见 `cmd/mounter/main.go:86` 和 `cmd/mounter/main.go:157`。
4. mounter 读取 projected token，向 wrapper UDS 发送 `POST /v1/mount`，请求结构定义在 `internal/api/types.go:5`。
5. wrapper 校验请求 JSON、API version、driver name、Pod identity、target path，见 `internal/wrapper/server.go:59` 与 `internal/security/targetpath.go:35`。
6. wrapper 调 Kubernetes TokenReview，确认 token 已认证且 audience 匹配，见 `internal/kube/client.go:55` 与 `internal/wrapper/authz.go:40`。
7. wrapper 重新读取 live Pod，校验 namespace/name/UID、Pod service account 与 token service account 一致，见 `internal/wrapper/authz.go:59`。
8. wrapper 读取 PV 与 PVC，校验 PV 是 CSI PV、driver 匹配、PV claimRef 指向请求 Pod 所在 namespace 的 PVC，且 PVC identity 与 claimRef 匹配，见 `internal/wrapper/authz.go:89`。
9. wrapper 选择目标容器 status，取得 container ID，见 `internal/wrapper/authz.go:111`。
10. wrapper 从 PV CSI volumeAttributes 中取 NFS `server`、`share`、`subDir/subdir`，构造 `node.MountPlan`，见 `internal/wrapper/authz.go:151`。
11. node mounter 对同一 PV 使用分段锁，创建 staging path，必要时执行 `mount -t nfs` 到 staging path，见 `internal/node/mounter_linux.go:34` 与 `mountNFS`。
12. node mounter 根据 Pod UID 和 container ID 在 host `/proc` 中找目标 PID，打开 `/proc/<pid>/ns/mnt`，进入目标容器 mount namespace，使用 `open_tree` 克隆 staging mount，再用 `move_mount` 挂到目标路径，见 `internal/node/mounter_linux.go:194` 与 `internal/node/mounter_linux.go:230`。
13. 如果 `UnstageAfterMount` 开启，wrapper 在完成动态 bind 后清理 staging mount 和 staging 目录，见 `internal/node/mounter_linux.go:53` 与 `internal/node/mounter_linux.go:90`。
14. wrapper 返回 `{"data": {"mounted": true, ...}}`；mounter 把结果输出给调用方。

### 动态卸载时序

【代码事实】当前实现提供认证后的 `/v1/unmount` 和 `kruise-nfs-mounter unmount`。卸载时序为：

- 重复 TokenReview、Pod/PV/PVC、driver、target path 和目标容器校验。
- 先从节点状态目录删除对应的期望挂载文件，避免 informer 重协调把目标重新挂回。
- 重新定位当前目标容器 PID 和 mount namespace；目标仍为 mount point 时进入该 namespace 卸载。
- 目标已经不存在时返回成功，使容器重启窗口内的 unmount 也能取消期望挂载。
- unmount 失败时恢复原期望状态，避免状态与实际挂载静默分叉。

【代码事实】staging cleanup 仍包含两条独立路径：每次 mount 后按 `UnstageAfterMount` 清理，以及 wrapper 启动时调用 `node.CleanupStagingRoot` 清理旧进程遗留的 staging mounts。

【设计规划】当前 mountinfo 检查只能确认 target 是 mount point；如需对无本地状态的历史挂载做更强卸载校验，还应核对实际 mount source 与请求 PV/source 一致。

## 6. 容器身份校验和 mount namespace 获取方法

### 容器身份校验

【代码事实】请求身份链条由三部分组成：

- mounter 提供 Pod namespace/name/UID、driver name、PV name、target path、可选 container name，结构体为 `internal/api/types.go:5` 的 `MountRequest`。
- wrapper 用 TokenReview 校验 bearer token 和 audience，见 `internal/wrapper/authz.go:40`。
- wrapper 读取 live Pod/PV/PVC 再做二次校验，见 `internal/wrapper/server.go:96` 的 `mount`。

【代码事实】当前 PV 授权粒度是“请求 Pod namespace 与 PV claimRef namespace 一致，并且 PV/PVC/driver identity 匹配”。测试名 `TestAuthorizePVForPodAllowsNamespaceScopedDynamicMount` 也反映了这是 namespace-scoped dynamic mount，而不是逐 Pod spec volume 绑定校验。

【设计规划】更强的逐 Pod 授权可以通过 controller 签发不可篡改的 mount grant、Pod annotation、PVC/PV label policy 或 runtime 内部授权记录实现，但这些机制当前没有落在仓库代码中。

### mount namespace 获取方法

【代码事实】wrapper 运行在节点 DaemonSet 中，当前配置要求它能看到 host `/proc`，默认 `HostProcRoot` 是 `/proc`。节点 Chart 使用 `hostPID: true`，见 `charts/kruise-agents-nfs-csi/values.yaml`。

【代码事实】`findContainerPID` 扫描 `${HostProcRoot}/*/cgroup`，同时匹配 Pod UID 和 container ID；找到 PID 后，`bindMountIntoContainerNamespace` 打开：

- `${HostProcRoot}/<pid>/mountinfo`：检查 target 是否已经是 mount point。
- `${HostProcRoot}/<pid>/root/<target>`：创建或打开目标路径。
- `${HostProcRoot}/<pid>/ns/mnt`：进入目标 mount namespace。

【代码事实】进入 namespace 后，代码使用 Linux `setns(CLONE_NEWNS)`、`open_tree(OPEN_TREE_CLONE)` 和 `move_mount` 完成挂载移动，相关逻辑在 `internal/node/mounter_linux.go:230`。

## 7. 原子提交、幂等、回滚和状态重协调机制

### 原子提交

【代码事实】当前实现把 NFS 源先挂到节点 staging path，再用 `open_tree` 克隆已挂载的 mount tree，最后在目标容器 mount namespace 中用一次 `move_mount` 提交到目标路径。这个路径减少了“半挂载到业务路径”的中间状态。

【代码事实】代码在创建目标目录前后两次检查目标路径是否已经是 mount point；如果已经是 mount point，直接报错，不自动 unmount，见 `internal/node/mounter_linux.go:230`。

【推测/待确认】如果另一个进程在第二次 mountpoint 检查之后、`move_mount` 之前并发占用目标路径，当前实现依赖内核 syscall 返回错误，但没有更高层的 per-target 锁或事务记录。

### 幂等

【代码事实】对同一 PV 的 staging 操作有 128 条 stripe lock，避免同一 PV 并发 staging/unstaging 互相干扰，见 `internal/node/mounter_linux.go:22` 与 `stagingLock`。

【代码事实】如果 staging path 已经是 mount point，当前实现复用 staging mount。wrapper 会持久化 `(pod_uid, container_name, target_path)` 对应的期望挂载和最近成功的 container ID；同一挂载意图、同一 container ID 且目标仍为 mount point 时，重复请求直接返回成功。未知的已有 mount point 仍由 node mounter 显式拒绝，不会覆盖。

### 回滚

【代码事实】如果 NFS staging mount、PID 查找、namespace 打开或 `move_mount` 之前失败，目标容器路径不会被挂载；若配置 `UnstageAfterMount`，defer 会尝试清理 staging path。

【代码事实】如果 `move_mount` 成功但后续 staging cleanup 失败，当前实现只向 stderr 打 warning；目标容器内挂载仍保留。代码没有把 cleanup 失败转换为 mount 失败。

【设计规划】完整回滚机制需要记录目标 mount 成功状态，并在后续步骤失败时选择是否卸载目标路径；当前未实现。

### 状态重协调

【代码事实】wrapper 使用节点本地状态目录记录不含 token 的期望挂载，每个挂载独立写入一个 `0600` 文件，并为本节点 Pod 建立一个按 `spec.nodeName` 过滤的 `SharedIndexInformer`。当目标容器的 `containerID` 变化时，wrapper 重新校验 live Pod/PV/PVC 并挂入新 mount namespace；Pod 删除、终态、UID 不匹配或离开当前节点时删除过期记录。PID/cgroup 短暂不可见时，只对失败挂载做固定 worker 数量的有界退避重试，不按 Pod 周期扫描。

## 8. 与其他方案的区别

### 与普通 CSI NodePublishVolume 的区别

【代码事实】普通 CSI 能力仍由 upstream `nfsplugin` 保留；本项目新增的是 Pod 运行后的 UDS mount API。

区别：

- 普通 CSI 由 kubelet 根据 Pod spec 中预声明 volume 调用。
- 本方案由 runtime sidecar 在 Pod 已运行后触发 wrapper。
- 普通 CSI 发布到 kubelet 管理路径，再由 kubelet带入容器。
- 本方案直接进入目标容器 mount namespace，把 PV mount 克隆到任意经过校验的业务路径。

### 与 Sidecar 挂载的区别

区别：

- Sidecar 挂载通常要求 sidecar 自身具备 mount 权限和 helper，甚至需要可见目标容器 rootfs 或进程。
- 本方案的 sidecar/mounter 不 privileged、不需要 `SYS_ADMIN`、不挂 host `/proc`，只是 API client。
- 危险能力集中在节点 DaemonSet wrapper，便于 RBAC、审计和部署约束。

### 与 mount propagation 的区别

区别：

- mount propagation 需要预先设计 shared/bidirectional volume 拓扑，且目标路径通常仍受 Pod spec 和共享目录限制。
- 本方案不要求业务容器提前暴露通用共享目录；目标路径由 runtime 请求给出，并由 wrapper 校验后进入目标容器 namespace 绑定。
- mount propagation 不能替代 token、PV/PVC、目标容器身份校验。

### 与 LXD 运行时目录注入的区别

区别：

- LXD 目录/设备注入依赖 LXD runtime 语义，不是 Kubernetes CSI 标准路径。
- 本方案保持在 Kubernetes PV/PVC/CSI 资源模型内，并以 CRI 容器的 mount namespace 为目标。
- 本方案不要求业务容器运行在 LXD，也不要求控制 LXD profile/device。

### 与直接 nsenter + bind mount 的区别

区别：

- 直接 `nsenter` 脚本通常只解决“怎么挂进去”，缺少 TokenReview、Pod/PV/PVC 重新校验、driver 限制、target denylist、socket 边界和 mounter 最小权限设计。
- 本方案把 nsenter/mount namespace 操作封装在 wrapper agent 中，并把请求协议、认证、授权和 staging cleanup 放在同一服务边界内。
- 当前实现仍使用 setns/bind/move mount 等底层技术，但不是把这些能力直接暴露给业务 Pod 或 sidecar 脚本。

## 9. 已实现、正在实现、计划实现、仅为设想的内容

### 已实现

【代码事实】

- Go module、wrapper、mounter、共享 API、Kubernetes REST client。
- HTTP over UDS 的 `/v1/mount`。
- projected token + TokenReview + audience 校验。
- live Pod/PV/PVC 校验。
- driver name 可配置，默认 `csi.nfs.zhida`。
- target path denylist。
- 已挂载 target path 拒绝。
- NFS staging mount。
- 通过 host `/proc` + cgroup 扫描获取目标容器 PID。
- `setns` + `open_tree` + `move_mount` 动态挂载到目标容器 mount namespace。
- staging path 挂载后清理和 wrapper 启动时 stale staging cleanup。
- 节点本地期望挂载状态、目标容器重启后的 informer 事件重协调和有界重试。
- 认证后的 `/v1/unmount`，并在卸载前删除期望状态以阻止复挂。
- Chart、demo、Dockerfile、`scripts/build.sh`。
- 单元测试覆盖 mounter request 解析、PV 授权基础规则、target path denylist、主容器选择。

### 正在实现 / 当前工作树中已出现但需继续固化

【代码事实】当前工作树存在未提交改动，包含 staging cleanup、wrapper 配置、Chart values、node mount 类型等修改。本文按当前工作树读取，但这些内容需要后续 commit、review 和 release 固化。

【设计规划】需要继续固化的点：

- 当前 `UnstageAfterMount` 相关能力与 Chart/entrypoint 的一致性。
- 不同 Linux 内核、containerd/CRI-O、cgroup v1/v2 的兼容性验证。
- informer cache、目标容器重启恢复和失败重试的实际集群规模验证。

### 计划实现

【设计规划】

- 更细粒度的 PV/PVC 授权策略，避免仅以 namespace 作为授权粒度。
- 可观测性：结构化日志、metrics、mount latency、失败原因分类。
- 支持 dry-run 或 validate-only 请求，用于 controller 预检查。

### 仅为设想

【推测/待确认】

- MountGrant CRD 或 controller-signed grant。
- 使用非对称签名绑定 Pod/PV/target path/container identity，减少 wrapper 对外部 controller 时序的依赖。
- CRI RuntimeService 直接查询 container PID，替代 `/proc/*/cgroup` 扫描。
- 对多种远端文件系统复用同一 wrapper 协议，而不仅是 NFS。

## 10. 关键源代码文件、函数和数据结构位置

| 类型 | 位置 | 说明 |
| --- | --- | --- |
| API 数据结构 | `internal/api/types.go:5` `MountRequest` | wrapper/mounter 共用 JSON 请求结构 |
| mounter runtime 解析 | `cmd/mounter/main.go:86` `parseRuntimeMountRequest` | 解码 CSI `NodePublishVolumeRequest` |
| mounter 调 wrapper | `cmd/mounter/main.go:217` `callWrapper` | HTTP over UDS，携带 bearer token |
| wrapper 启动 | `cmd/wrapper/main.go` | 加载配置、启动 kube client、node mounter、UDS server |
| UDS 监听 | `cmd/wrapper/main.go:90` `listenUnix` | 创建 socket 并设置权限 |
| HTTP handler | `internal/wrapper/server.go:59` `handleMount` | 接收 `/v1/mount` |
| mount 主流程 | `internal/wrapper/server.go:96` `mount` | 请求校验、TokenReview、Pod/PV/PVC 查询、调用 node mounter |
| token 校验 | `internal/wrapper/authz.go:40` `authorizeToken` | 校验 authenticated、namespace、audience |
| Pod 校验 | `internal/wrapper/authz.go:59` `authorizePod` | 校验 Pod identity、service account、phase |
| PV/PVC 校验 | `internal/wrapper/authz.go:89` `authorizePVForPod` | 校验 CSI driver、claimRef、PVC identity |
| 目标容器选择 | `internal/wrapper/authz.go:111` `selectContainerStatus` | container_name / SANDBOX_MAIN_CONTAINER / single-container fallback |
| mount plan | `internal/wrapper/authz.go:151` `buildMountPlan` | 从 PV attributes 构造 NFS source |
| target path 安全 | `internal/security/targetpath.go:35` `ValidateTargetPath` | 拒绝 `/`、`/proc`、secret、kubelet 等危险路径 |
| node mount | `internal/node/mounter_linux.go:34` `Mount` | staging、PID 查找、namespace bind |
| staging cleanup | `internal/node/mounter_linux.go:107` `CleanupStagingRoot` | wrapper 启动时清理旧 staging mount |
| 容器 PID 查找 | `internal/node/mounter_linux.go:194` `findContainerPID` | 扫描 host `/proc/*/cgroup` |
| mount namespace bind | `internal/node/mounter_linux.go:230` `bindMountIntoContainerNamespace` | `setns` + `open_tree` + `move_mount` |
| Kubernetes client | `internal/kube/client.go` | TokenReview、Pod、PV、PVC REST 调用 |
| wrapper entrypoint | `build/wrapper/entrypoint.sh` | 同时启动 wrapper agent 和 upstream `nfsplugin` |
| node Chart | `charts/kruise-agents-nfs-csi/templates/node.yaml` | DaemonSet、hostPath、privileged wrapper |
| demo 注入 | `demo/sandbox-inject-configmap.yaml` | 低权限 mounter sidecar 示例 |

## 11. 首次构思、首次实现、评审、上线和对外披露时间线

【设计背景】以下时间线来自当前 git 历史和相关 Codex 线程记忆；没有从仓库确认的事项明确标为未确认。

- 2026-06-10：线程记忆显示，项目从现有 sandbox CSI injection 调试中演进出“wrap upstream NFS CSI、节点 wrapper DS、sidecar mounter 低权限 API client”的架构方向；同时明确拒绝让 sandbox/业务容器访问 host `/proc`。
- 2026-06-11 22:10 +08:00：git 提交 `070b7de init project`，仓库初始提交。
- 2026-06-15：线程记忆显示，已经完成 repo skeleton、wrapper/mounter 协议、TokenReview、target path denylist、Linux mount namespace staging/bind 逻辑、docs/demo/build.sh，并做过 `go test`、`go vet`、linux/amd64 cross build 等验证。
- 2026-06-17 23:54 +08:00：git 提交 `06b98c4 wrapper csi for dynamic mount`，一次性加入核心代码、Chart、demo、docs 和 build 脚本。
- 2026-06-24：线程记忆显示，staging unmount 行为在内部测试环境验证通过，包括 mount 后 staging 清理和容器内读写验证。具体临时对象和地址不记录在本文中。
- 技术评审：已发生多轮线程内设计评审和实现取舍讨论；未从仓库确认正式代码评审记录。
- 上线：未从当前仓库确认生产上线。仅能确认有内部测试/验证记录。
- 对外披露：未从当前仓库确认公开发布、公开演示或专利外部披露记录。

## 12. 各参与人的具体技术贡献

【代码事实】当前 git 历史显示两个提交：`init project` 和 `wrapper csi for dynamic mount`。提交作者信息存在于 git log 中；本文不记录邮箱。

【设计背景】参与贡献可按角色划分：

- 项目发起人与需求方：定义核心约束，包括业务容器不可控、不能给业务容器或 mounter `SYS_ADMIN`、不能让 sandbox/业务容器访问 host `/proc`、不直接暴露 CSI socket、driver name 必须可配置、优先用轻量 wrapper 保留 upstream NFS CSI 能力。
- 主要工程实现者 / 仓库提交作者：完成仓库初始化和核心实现提交，包括 Go 代码、Chart、Dockerfile、demo、docs、build 脚本。
- Codex coding agent：根据多轮对话辅助完成架构拆解、代码实现、测试验证、staging cleanup 修复、文档整理和风险边界归纳。该角色是否作为发明贡献主体，需要由项目方和法律评审进一步确认。
- upstream Kubernetes CSI、NFS CSI、OpenKruise 相关项目：提供基础接口、运行时机制和依赖能力；它们是被复用的开源基础，不应混同为本项目新增发明点。

【推测/待确认】如果后续要进入专利流程，需要由项目负责人补充：正式发明人名单、每个人对“动态挂载授权模型、节点 wrapper mount namespace 操作、原子/回滚机制、runtime 注入协议”等具体权利要求的创造性贡献。

## 13. 仍未解决的技术问题和备选方案

### 未解决问题

【代码事实 / 设计规划】

- 当前 PV 授权粒度是 namespace + PV/PVC/driver identity，不是严格逐 Pod 声明的 volume 授权。
- 当前目标容器 PID 查找依赖 host `/proc/*/cgroup` 字符串匹配，可能受 CRI、cgroup v2、节点发行版影响。
- 当前 target path denylist 是静态规则，后续可能需要按业务策略配置。
- 当前没有完整 metrics、审计事件、结构化错误码。

### 备选方案

【设计规划 / 推测】

- 引入 MountGrant CRD：由可信 controller 根据 Pod/PV/PVC/target 生成一次性或短期 grant，wrapper 验 grant 后执行 mount。优点是授权更精确；缺点是 controller 时序会影响 mount 成功率。
- 使用签名 mount request：controller 或 runtime 对 Pod UID、PV name、target path、container name、过期时间签名，wrapper 用公钥验签。优点是抗篡改；缺点是密钥轮换和签发路径复杂。
- 使用 CRI API 查 PID：wrapper 通过 CRI RuntimeService 查询 sandbox/container 状态，替代 `/proc` 扫描。优点是更语义化；缺点是需要访问 CRI socket 并适配 runtime 差异。
- 状态 CRD + reconciler：记录动态 mount 状态，支持重试、卸载、Pod 删除清理和审计。优点是系统化；缺点是实现复杂度和权限面增加。
- 限制 target path 模板：只允许挂到 runtime 管控的一组路径。优点是安全简单；缺点是不能满足“任意业务路径”诉求。
- 保持当前最小实现：继续用 UDS + TokenReview + live Pod/PV/PVC 校验 + node wrapper mount namespace 操作。优点是简单、低侵入；缺点是授权粒度和重协调能力需要由部署边界补强。

## 结论

【代码事实】当前仓库已经实现“低权限 mounter 通过 UDS 请求节点 wrapper，wrapper 校验 token/Pod/PV/PVC 后进入目标容器 mount namespace 动态挂载 NFS PV”的核心闭环。该闭环的关键代码集中在 `cmd/mounter`、`internal/wrapper` 和 `internal/node`。

【设计规划】完整产品化还需要补齐逐 Pod 授权、runtime/CRI 兼容性、规模化重协调验证和审计可观测能力。

【推测/待确认】如果进入专利撰写，建议把潜在发明点集中在“节点 wrapper 保留 upstream CSI 能力并额外提供动态 namespace mount API”“低权限 runtime mounter 与 TokenReview/live-resource 校验组合”“staging clone + move_mount 的提交路径”“Pod 运行后面向目标容器任意路径的受控挂载”这几类技术组合上，同时避免把尚未实现的 grant 机制写成既成事实。

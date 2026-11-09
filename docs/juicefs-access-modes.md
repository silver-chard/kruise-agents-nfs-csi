# JuiceFS publish target、source 复用与逐次读写控制

本文说明可选的 JuiceFS 后端如何经 CSI 发布源目录、复用 source，并提供逐目标 RO/RW 控制。NFS 保持既有默认路径；JuiceFS 只有配置可信 CSI socket 后才启用。独立 Mount Pod 模式的部署前提和升级边界见 [Mount Pod backend](juicefs-mountpod.md)。

## by-process 与三层路径

JuiceFS CSI 的 `--by-process=true` 表示由 CSI Node 容器启动 JuiceFS 客户端进程完成 FUSE 挂载。它与使用独立 Mount Pod 的运行模式不同，也不表示“每个目录启动一个进程”。当前核对的驱动版本为 v0.33.0。

在默认配置下，下面三个路径属于不同层次：

| 层次 | 示例 | 作用 |
| --- | --- | --- |
| CSI 基础 FUSE 挂载 | `/var/lib/jfs/personal-handle` | CSI 按 volumeHandle 建立文件系统访问入口 |
| wrapper publish target | `/var/lib/kruise-agents-nfs-csi/juicefs-publish/<hash-A>` | CSI `NodePublishVolumeRequest.TargetPath`，wrapper 从这里选择目录 |
| 用户容器 target | `/mnt/personal` | 用户程序真正访问的目录 |

这里是 **publish target**（发布卷的目标路径）。默认 publish 根目录可通过 `WRAPPER_JUICEFS_PUBLISH_ROOT` 配置。`<hash-A>` 是示意，真实值为 `sha256(PodUID + "\x00" + PVName)` 的十六进制结果。

例如 Pod A 使用 `personal-pv`，该 PV 的 `volumeHandle=personal-handle`。mounter 经 `wrapper.sock` 发 HTTP/JSON 请求，wrapper 校验身份和授权，再经配置的 CSI UDS 发 gRPC/protobuf `NodePublishVolume` 请求：

```text
VolumeId  = personal-handle
TargetPath = /var/lib/kruise-agents-nfs-csi/juicefs-publish/<hash-A>
Readonly   = 源挂载模式，由 PV 和既有 lease 决定
```

CSI 将基础 FUSE 中该卷所暴露的目录 bind 到 publish target；wrapper 再将 `<hash-A>/user/alice` bind 到容器 `/mnt/personal`。若 PV 配有 `subdir`、`subPath`，publish target 所见根目录还受这些配置限制，不能一概当成整个 JuiceFS 文件系统根目录。

本文所说 **source** 是 wrapper 提供子目录的源挂载（publish target）及其背后的 FUSE 连接。它不是新的 Kubernetes 资源、数据副本或镜像内目录。

## 发现相同 source 后的行为

| 场景 | 实际行为 |
| --- | --- |
| 同 Pod、PV、容器 target，完整请求相同且挂载健康 | wrapper 快速返回成功，不重复调用 CSI、不重新 bind |
| 同 Pod + PV，新增 target 或子目录 | 复用同一个 lease 和 publish target，增加 consumer；调用幂等 NodePublish，目标已挂载时 CSI 直接返回，再 bind 本次子目录 |
| 同一 target 改 subPath 或 RO/RW | 拒绝，要求先卸载既有 target |
| source 失联 | 拒绝破坏性重建，避免拆掉其他 consumer 仍在使用的源 |
| 不同 Pod 使用同一个 PV | PodUID 不同，publish target 不同；不保证基础 FUSE 跨 Pod 复用 |

例如同一 Pod 内，`personal-pv` 的 `user/alice` 挂到 `/mnt/personal`，同一子目录还可按另一条授权挂到 `/mnt/personal-copy`；两者共享 source，各有 consumer。只卸载其中一个不会调用 NodeUnpublish；最后一个 consumer 释放后才释放这个 publish target。

v0.33.0 的 by-process 路径遇到一个新 publish target，会再次进入基础挂载流程；已有基础 mountPath 时还可能先执行卸载。因此不能由“两个 PV”推导出“集群永远只有两个 FUSE 挂载”。节点、Pod、volumeHandle、挂载参数和 CSI 运行模式共同决定复用及生命周期。本次没有解决跨 Pod 同 handle 的基础挂载竞争。

## 可信 Manager 授权本次 RO/RW

wrapper 不解析业务 Manager 的 profile、PVC 路由或用户名。可信控制面负责选择
PV 和子目录、检查业务权限，再向目标 Pod 写入下面的授权 annotation：

```yaml
metadata:
  annotations:
    drive.juicefs.com/allowed-mounts: >-
      [{"pv_name":"personal-pv","source_sub_path":"user/alice","target_path":"/mnt/personal","container_name":"main","read_only":false}]
```

annotation 是授权，不是挂载触发器。沙箱 ServiceAccount 和不可信业务容器不得拥有
修改它、Pod 或 PV 的权限。wrapper 同时校验 projected token 的 audience、精确 Pod
身份、节点、容器、PV 的 namespace/ServiceAccount 授权以及路径安全。

mounter 接收已经由控制面选定的参数（Pod 身份由 Downward API/环境提供）：

```sh
# 可写：必须匹配上面的 read_only:false 授权，且 PV 本身允许写入。
kruise-nfs-mounter --driver csi.juicefs.com --pv personal-pv \
  --sub-path user/alice --target /mnt/personal --container main

# 只读：控制面先写入相同路径、read_only:true 的授权，再调用：
kruise-nfs-mounter --driver csi.juicefs.com --pv personal-pv \
  --sub-path user/alice --target /mnt/personal --container main --read-only

# 仅卸载 wrapper 管理的动态挂载。
kruise-nfs-mounter unmount --driver csi.juicefs.com --pv personal-pv \
  --target /mnt/personal --container main
```

上面 RO/RW 是替代示例，不应对同一活跃 target 直接切换模式。先卸载后重新授权/挂载。
wrapper 的底层 JSON 字段是 snake_case；请求 read_only 省略时为 false，仍须匹配
显式 read_only:false 的授权。业务 API 如果默认只读，必须由控制面显式传递 true。
**授权里的 read_only 不能省略或为 null**，也不能被 JSON omitempty 丢弃。

JuiceFS 子目录必须预先存在；wrapper 不创建这些业务目录。已有 NFS 接口和目录
创建策略不受这套 JuiceFS grant 机制影响。

PV 的 nodePublishSecretRef 必须出现在 wrapper 的 namespace/name Secret 白名单中；
wrapper 读取后经 CSI gRPC 传递，Secret 数据不写入 wrapper 的 lease 或期望挂载状态。
Kubernetes RBAC 是另一层独立限制，白名单不能替代 RBAC。

## source 模式与用户视图模式

本次实现没有为 RO 和 RW 分别创建两套 source key。PV 决定新 source 的模式，Manager grant 决定 consumer 模式：

| 源模式 | 本次 consumer | 结果 |
| --- | --- | --- |
| RO | RO | 允许 |
| RO | RW | 拒绝，不能提升 PV 权限 |
| RW | RW | 允许，仍受目录 POSIX 权限限制 |
| RW | RO | 支持，要求安全地在挂入容器前设置只读 |

PV `spec.csi.readOnly`、`spec.mountOptions` 和 `volumeAttributes.mountOptions` 中的 `ro` / `read-only` 会限制源为 RO。遇到冲突时保守使用 RO。其他挂载参数保留 CSI 的顺序：attributes 在前、spec.mountOptions 在后。`ReadWriteMany` 等 accessModes 是卷能力/匹配声明，不能代替实际挂载只读标志。

CSI 可能将只读请求传给客户端，但发布一个内核标志仍为 RW 的 bind。wrapper 会在注入前将自己管理的 RO source bind 收紧为只读。核验通过安全打开目录的 FD 获取 `mnt_id`，匹配实际挂载并检查 `Fstatfs`；同路径可能有多个 mountinfo 记录，不能选首条、末条或最大 mount ID。

新建 source 若 PV 允许写入，即使第一个 consumer 只读，也会建立 RW source，方便后续混合 RO/RW consumer。**没有 `mount_setattr` 的节点不能使用这种 RW source → RO consumer 组合**：通常需要主线 Linux 5.12+ 或厂商回移；只看版本号不如实际验证系统调用能力。旧内核只允许已经 RO 的源走 RO 兼容挂载。需要保留旧内核只读接入时，应使用明确配置为 RO 的 PV，不能仅依赖请求的只读标志。

新 source lease 为 v2，记录源模式及每个 consumer 模式。旧 v1 lease 以 RO 解释，保留原 key；不会自动变为 RW。旧 RO source 收到 RW 请求会拒绝。改变既有目标模式仍需先卸载后挂载，更新 metadata 不会原地 remount。

挂载中途失败后使用普通卸载回滚；如果目标忙碌等原因使清理失败，保留 source 引用和容器挂载记录，供显式卸载或授权撤回继续处理。不会把 lazy detach 当作撤回进程已打开文件描述符的写权限。授权撤回同样可能被忙碌目标阻塞；要立即停止所有存量写操作，需要关闭相关文件或结束消费容器。

## 升级与验收边界

1. 先核对 PV 模式、节点系统调用能力和实际 CSI 运行方式，再配置兼容的 wrapper/mounter 与控制面。无 mount_setattr 的内核可使用明确配置 RO 的独立 PV，不应从 RW source 派生 RO target。
2. 旧 v1 RO source、同 handle 的其他 publish target 和基础 FUSE 都要纳入维护计划。仅清空某个 Pod 的 consumer 或删除 lease 文件不能证明源已正常释放；不要手工删除活跃 lease。
3. by-process 的 FUSE 进程属于 CSI Node 容器。重建该容器可能使依赖它的业务挂载出现 Transport endpoint is not connected。独立更新 wrapper 不必然停止 CSI 中的 FUSE，但重建包含二者的 Pod 会停止它。
4. mountpod 是显式选项，要求匹配的外部 CSI wrapper-target 扩展。独立 Mount Pod 保持运行时，CSI Node/wrapper 更新可与已有 FUSE 生命周期分离；不代表 process 原地迁移、Mount Pod 重建、节点重启或后端故障无损。
5. wrapper 只管理自己登记的动态 target。kubelet 通过 Pod spec 建立的静态 volumeMount 不会自动纳入 wrapper 状态；对未登记的 target 调用 unmount 是幂等 no-op，不能作为静态挂载已移除的证据。控制面应在动态操作前拒绝静态 target 冲突。
6. 单元测试不代替真实内核、CSI、挂载传播和故障恢复验收。本仓库没有包含实际集群的 Secret、PV/PVC 或部署快照。

源码入口：`internal/node/juicefs_linux.go` 管理 key/lease/CSI 调用；`internal/node/readonly_linux.go` 注入和核验模式；`internal/wrapper/juicefs.go` 校验 grant；`internal/wrapper/server.go` 执行请求幂等和卸载。

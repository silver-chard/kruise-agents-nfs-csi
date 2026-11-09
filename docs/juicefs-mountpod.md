# JuiceFS Mount Pod backend

## 作用与依赖

默认 `process` 模式仍使用原 hashed publish target。选择 `mountpod` 后，wrapper
使用带真实业务 PodUID 的 publish target，配合 CSI Node 创建/复用独立 Mount Pod。
FUSE 客户端由 Mount Pod 持有，不由 wrapper 或 CSI Node 容器直接持有。

**不能只切换这个选项就使用未修改的上游驱动。** 本实现需要基于 JuiceFS CSI
v0.33.0 的外部 wrapper-target 扩展：

- CSI 必须显式信任一个独立的 `JUICEFS_WRAPPER_PUBLISH_ROOT`，与 wrapper 的
  publish root 相同；不能把该目录当成真正的 kubelet Pod volume 目录。
- 必须支持从下列路径及 NodePublishVolume Pod 上下文识别真实业务 Pod，并在
  Mount Pod manager 中维护引用。不是使用 wrapper Pod 的 UID。
- wrapper target 在 CSI Node 重启、NodeUnpublishVolume 和业务 Pod 删除时，必须
  与原生 kubelet target 一样得到正确的引用恢复/释放，不能提前回收仍在使用的 FUSE。
- CSI Node 的 Mount Pod 模式和 manager 必须开启；它不同于动态供给用的
  Controller Deployment。预建静态 PV 不要求启用动态供给 Controller。

```text
<publish-root>/pods/<business-pod-uid>/volumes/kubernetes.io~csi/<pv-name>/mount
```

本仓库的 PR 仅提供 mounter/wrapper 一侧实现。**外部 CSI 扩展、CSI 镜像、外层部署
Chart 和业务 Manager 不包含在这里。** 仓库内既有 NFS Helm chart 不会自动部署
JuiceFS 或增加完整的 JuiceFS hostPath/RBAC 配置。

## wrapper 配置

以下是示意环境变量，真实 hostPath/挂载传播/CSI socket 路径需按集群配置：

```sh
WRAPPER_JUICEFS_CSI_SOCKET=/var/lib/kubelet/csi-plugins/csi.juicefs.com/csi.sock
WRAPPER_JUICEFS_MOUNT_MODE=mountpod
WRAPPER_JUICEFS_PUBLISH_ROOT=/var/lib/kruise-agents-nfs-csi/juicefs-publish
WRAPPER_JUICEFS_STATE_DIR=/var/lib/kruise-agents-nfs-csi/juicefs-state
WRAPPER_JUICEFS_SECRET_REFS=example/juicefs-metadata
WRAPPER_JUICEFS_REQUEST_TIMEOUT=180s
```

- CSI socket 非空才启用 JuiceFS；NFS driver 和 socket 协议保持原样。
- publish root 必须是干净的绝对路径、不能是 `/` 或 kubelet 的 pods 子树。
  wrapper 与 CSI 必须看到同一节点目录，并正确配置挂载传播。
- lease 和 desired mount 状态都必须落在持久节点目录；wrapper 更新时不能清空。
- Secret 白名单为逗号分隔的 `namespace/name`。PV 的 `nodePublishSecretRef` 指向
  该 Secret；wrapper ServiceAccount 另需对应 namespace 中限定 Secret 名称的
  `get` 权限。不要给业务沙箱开放 Secret 读取或 grant/PV 修改权限。
- CSI Node 另需上游驱动所需的 Mount Pod、运行 Secret 及节点等 RBAC。
- 180s 请求预算只用于 JuiceFS mountpod 操作，不改变 NFS 或 Kubernetes API 超时。
  mounter CLI 对 JuiceFS 的默认 HTTP timeout 也是 180s；显式设置
  `MOUNTER_HTTP_TIMEOUT` 时以调用方设置为准。Manager/网关超时也要覆盖首次拉镜像。

Manager grant、PV 只读上限和内核要求见 [access modes](juicefs-access-modes.md)。

## 复用、清理与升级

- source lease 按真实业务 PodUID + PVName 持久化。同一 Pod/PV 的多个目录共享该
  publish source；跨业务 Pod 的底层 FUSE 是否复用由 CSI 的节点/options 等决定。
- 最后一个 consumer 释放后调用 NodeUnpublishVolume。业务 Pod 删除时也会释放
  source；暂时失败保留状态并允许后续重试。
- wrapper 重启不会主动卸载已有 source。健康 consumer 不重绑；RO source 的
  视图可以在核验后收紧，不把活跃 RW consumer 静默升级/降级。
- 存在不兼容 lease 时，启动检查拒绝切换 mount mode/publish root。process 挂载
  不能原地当作 mountpod 接管，必须先正常释放旧引用；不能删 lease 绕过检查。
- CSI Node/wrapper 更新与 FUSE 生命周期解耦的前提是独立 Mount Pod 保持运行、
  CSI 引用管理兼容、hostPath 和 socket 配置不变。Mount Pod 自身删除/重建/驱逐/OOM、
  客户端镜像替换、节点重启和后端故障不在无中断保证范围内。
- static Pod volumeMount 和 wrapper 动态 target 的归属不因 Mount Pod 模式改变。
  动态 unmount 不能接管 kubelet 静态挂载。

## 构建与验证

需要 `go.mod` 声明的 Go 版本（当前 Go 1.26.3 或更新版本）。

```sh
umask 022
go test ./...
go test -race ./...
go vet ./...
sh -n build/wrapper/entrypoint.sh
sh build/test-entrypoint.sh

mkdir -p dist/linux-amd64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/linux-amd64/kruise-nfs-wrapper ./cmd/wrapper
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/linux-amd64/kruise-nfs-mounter ./cmd/mounter
```

Dockerfile 中的 `BASE_IMAGE` 由构建者显式提供，不含环境专用私有镜像默认值：

| 文件 | 用途 |
| --- | --- |
| `build/Dockerfile.agent` | Debian-compatible 基础镜像上安装 NFS 工具并放入双驱动二进制 |
| `build/Dockerfile.combined` | 在已有 `/nfsplugin` 的兼容镜像上更新 wrapper/mounter 和监督脚本 |
| `build/Dockerfile.mountpod-agent` | 在已有兼容 mounter runtime 镜像上仅替换二进制，保留原启动配置 |
| `build/Dockerfile.binaries` | scratch 格式的二进制交付产物，不是可直接运行的挂载组件 |
| `build/Dockerfile.combined-offline` | 离线测试用，另需经核验的本地 nfsplugin；不提交 vendor 二进制 |

本地单元测试不会验证真实 FUSE 挂载或 CSI 扩展。上线前应在隔离节点验证 RO/RW、
引用复用、卸载/Pod 删除、wrapper/CSI Node 重启和文件连续读写，再逐步灰度。

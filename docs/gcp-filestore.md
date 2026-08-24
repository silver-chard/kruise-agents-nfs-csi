# GCP Filestore 配置说明

本文说明如何把 GCP Filestore 配成一个可被本项目动态 provision 的标准 NFS share。

本项目包装的是 upstream `nfs.csi.k8s.io` 行为。它不会调用 GCP Filestore API，也不识别 GCP Filestore CSI 的 `ip`、`volume`、`protocol` 字段。它只按普通 NFS 语义工作：

```yaml
parameters:
  server: <filestore-ip>
  share: /<file-share-name>
```

动态创建 PVC 时，CSI controller 会执行类似下面的动作：

```text
mount <filestore-ip>:/<file-share-name>
mkdir pvc-...
chmod pvc-...
```

因此 Filestore 的 file share 必须允许 CSI controller 所在的 GKE 节点写入和创建目录。

## GCP Console 配置步骤

1. 打开 Google Cloud Console。
2. 进入 `Filestore`。
3. 进入 `Instances`。
4. 选择目标 Filestore instance。
5. 确认 file share 名称，例如 `agent_code`。
6. 点击 `Edit`。
7. 找到 `IP-based access control`、`Access control rules` 或 `NFS export options` 区域。
8. 修改或新增一条访问规则。

建议先用下面配置跑通：

```text
File share:   agent_code
IP range:     <GKE node primary CIDR>
Access mode:  READ_WRITE
Squash mode:  NO_ROOT_SQUASH
```

`IP range` 必须覆盖 GKE node primary IP，不是 Pod IP。CSI controller 通常以 `hostNetwork` 方式运行，NFS 服务端看到的来源地址是节点主网卡 IP。

如果页面中已有多条重叠规则，要确认最精确命中的规则也是 `READ_WRITE`。例如下面这种配置会导致节点仍然只读：

```text
192.0.2.0/24     READ_WRITE
192.0.2.128/25   READ_ONLY
```

当节点 IP 在示例网段 `192.0.2.128/25` 内时，更精确的 `READ_ONLY` 会优先生效。

## StorageClass 配置

对应的 StorageClass 应该使用 upstream NFS CSI 参数：

```yaml
provisioner: csi.nfs.example.com
parameters:
  server: <filestore-ip>
  share: /agent_code
  mountPermissions: "0777"
mountOptions:
  - nfsvers=4.1
  - hard
  - nordirplus
```

不要把 GCP Filestore CSI 的静态 PV 字段放到这个 driver 上：

```yaml
volumeAttributes:
  ip: <filestore-ip>
  volume: agent_code
  protocol: NFSv4.1
```

这组字段属于 GCP Filestore CSI driver，不属于 upstream NFS CSI driver。

## 验证方法

创建一个临时 PVC 后，观察 PVC 事件和 CSI controller 日志。

成功时应看到 PVC 进入 `Bound`，并自动创建 PV。

如果仍然失败，常见错误和含义如下：

```text
failed to make subdirectory: ... read-only file system
```

说明 Filestore 访问规则或目录权限仍然让 CSI controller 节点只读。

```text
permission denied
```

说明 export 规则、root squash、POSIX 目录权限或客户端来源 IP 不满足写入要求。

```text
server is a required parameter
share is a required parameter
```

说明 StorageClass 使用了错误参数。请确认是 `server` 和 `share`，不是 `ip` 和 `volume`。

## 最小排查清单

- `share` 是否是真实 file share 路径，例如 `/agent_code`，而不是 `/`。
- Filestore access rule 是否覆盖 GKE node primary CIDR。
- 命中的最精确规则是否是 `READ_WRITE`。
- `NO_ROOT_SQUASH` 是否已启用，或 root squash 后的匿名用户是否有写权限。
- `mountPermissions` 是否适合业务容器写入。先跑通建议用 `"0777"`。

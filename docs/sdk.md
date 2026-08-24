# Go SDK 使用说明

`github.com/silver-chard/kruise-agents-nfs-csi/mounter` 为可信 Go runtime
和 sidecar 提供与 `kruise-nfs-mounter` 命令等价的进程内调用方式。

SDK 只是现有低权限 mounter 的 Unix Domain Socket 客户端：

- 不直接执行 mount；
- 不内嵌 node mounter 或 wrapper；
- 不访问 CSI socket 或 host `/proc`；
- 不需要 privileged 或 `SYS_ADMIN`；
- mount、unmount、TokenReview、实时 Kubernetes 对象校验和容器重启重协调
  仍由节点 wrapper 完成。

SDK 调用进程会持有 wrapper socket 和 projected service account token，因此必须是
可信 runtime 或专用 sidecar。不要把这些能力直接交给不可信业务代码。

## 使用条件

调用 SDK 的容器需要：

1. 能访问本节点 wrapper socket，例如
   `/var/lib/kruise-agents-nfs-csi/wrapper.sock`。
2. 能读取 audience 与 wrapper `TOKEN_AUDIENCE` 一致的 projected service
   account token。
3. 通过 Downward API 获得当前 Pod 的 namespace、name 和 UID。
4. 知道要挂载的 PV、目标业务容器和容器内目标路径。
5. 使用与 wrapper 和 PV `spec.csi.driver` 相同的 driver name。

`MountRequest.SourceSubPath` 非空时，目录默认必须已经存在。v0.0.2 可以在
wrapper 启动时通过 `WRAPPER_CREATE_MISSING_SUBPATHS=true` 开启缺失目录创建；
这是节点级策略，不是 SDK 的单次请求选项。

SDK 本身不需要 Kubernetes RBAC。TokenReview 以及 Pod、PV、PVC 查询由节点
wrapper 的 service account 完成。

下面是调用容器需要的关键 Pod 配置片段。socket group 应与 wrapper 部署配置一致：

```yaml
env:
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
```

## 安装与配置

```sh
go get github.com/silver-chard/kruise-agents-nfs-csi/mounter
```

创建客户端：

```go
client, err := mounter.NewClient(mounter.Config{
    DriverName:  "csi.nfs.zhida",
    SocketPath:  "/var/lib/kruise-agents-nfs-csi/wrapper.sock",
    TokenFile:   "/var/run/secrets/kruise-agents-nfs-csi/token",
    HTTPTimeout: 15 * time.Second,
})
```

`Config` 字段：

| 字段 | 是否必填 | 说明 |
| --- | --- | --- |
| `DriverName` | 是 | 必须与 wrapper 配置和 PV CSI driver 一致。 |
| `SocketPath` | 是 | SDK 容器内可见的 wrapper Unix socket。 |
| `TokenFile` | 是 | projected service account token 文件。 |
| `HTTPTimeout` | 否 | UDS HTTP 请求超时；为 `0` 时默认 15 秒，不能为负数。 |
| `DisableHTTPTimeout` | 否 | 显式关闭 client 级超时；通常应保持为 `false`，并为每次调用传入有界 context。 |

`NewClient` 会拒绝空的 `DriverName`、`SocketPath` 或 `TokenFile`。
`Mount` 和 `Unmount` 会在访问 token 或 wrapper 前检查 namespace、Pod name、
Pod UID、PV name 和 target path 是否为空。
`Mount` 和 `Unmount` 每次调用都会重新读取 `TokenFile`，因此长生命周期
进程可以使用 Kubernetes 自动轮换后的 projected token。`Health` 不需要读取
token。

## 版本兼容

SDK 会自动发送当前模块实现的 wrapper API 版本，调用方不能覆盖。wrapper 会严格
校验该版本，因此 SDK 依赖与节点 wrapper 镜像应来自同一个发布版本或同一 commit，
并一起升级。`Health` 只检查服务状态，不协商 API 版本；不要单独滚动升级 SDK 后
长期搭配旧 wrapper 使用。

## 完整示例

下面的程序从 Downward API 环境变量读取 Pod identity，并使用
`PV_NAME`、`TARGET_PATH`、可选的 `SOURCE_SUB_PATH` 和
`CONTAINER_NAME` 执行 `health`、`mount` 或 `unmount`。

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "log"
    "os"
    "time"

    "github.com/silver-chard/kruise-agents-nfs-csi/mounter"
)

func main() {
    if err := run(); err != nil {
        var responseError *mounter.ResponseError
        switch {
        case errors.Is(err, context.DeadlineExceeded):
            log.Printf("operation timed out; its final outcome may be unknown: %v", err)
        case errors.Is(err, context.Canceled):
            log.Printf("operation canceled: %v", err)
        case errors.As(err, &responseError):
            log.Printf("wrapper rejected operation=%s status=%d: %s",
                responseError.Operation, responseError.StatusCode, responseError.Message)
        default:
            log.Printf("operation failed: %v", err)
        }
        os.Exit(1)
    }
}

func run() error {
    if len(os.Args) != 2 {
        return fmt.Errorf("usage: %s health|mount|unmount", os.Args[0])
    }

    client, err := mounter.NewClient(mounter.Config{
        DriverName:  envOr("DRIVER_NAME", "csi.nfs.zhida"),
        SocketPath:  envOr("WRAPPER_SOCKET_PATH", "/var/lib/kruise-agents-nfs-csi/wrapper.sock"),
        TokenFile:   envOr("PROJECTED_TOKEN_FILE", "/var/run/secrets/kruise-agents-nfs-csi/token"),
        HTTPTimeout: 15 * time.Second,
    })
    if err != nil {
        return fmt.Errorf("create mounter client: %w", err)
    }
    defer client.CloseIdleConnections()

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    switch os.Args[1] {
    case "health":
        result, err := client.Health(ctx)
        if err != nil {
            return fmt.Errorf("check wrapper health: %w", err)
        }
        log.Printf("wrapper status=%s driver=%s", result.Status, result.DriverName)
        return nil

    case "mount":
        namespace, podName, podUID, err := podIdentity()
        if err != nil {
            return err
        }
        pvName, err := requiredEnv("PV_NAME")
        if err != nil {
            return err
        }
        targetPath, err := requiredEnv("TARGET_PATH")
        if err != nil {
            return err
        }

        result, err := client.Mount(ctx, mounter.MountRequest{
            Namespace:     namespace,
            PodName:       podName,
            PodUID:        podUID,
            PVName:        pvName,
            SourceSubPath: os.Getenv("SOURCE_SUB_PATH"),
            TargetPath:    targetPath,
            ContainerName: os.Getenv("CONTAINER_NAME"),
        })
        if err != nil {
            return fmt.Errorf("mount pv %s at %s: %w", pvName, targetPath, err)
        }
        log.Printf("mounted=%t pv=%s target=%s container=%s",
            result.Mounted, result.PVName, result.TargetPath, result.ContainerName)
        return nil

    case "unmount":
        namespace, podName, podUID, err := podIdentity()
        if err != nil {
            return err
        }
        pvName, err := requiredEnv("PV_NAME")
        if err != nil {
            return err
        }
        targetPath, err := requiredEnv("TARGET_PATH")
        if err != nil {
            return err
        }

        result, err := client.Unmount(ctx, mounter.UnmountRequest{
            Namespace:     namespace,
            PodName:       podName,
            PodUID:        podUID,
            PVName:        pvName,
            TargetPath:    targetPath,
            ContainerName: os.Getenv("CONTAINER_NAME"),
        })
        if err != nil {
            return fmt.Errorf("unmount pv %s at %s: %w", pvName, targetPath, err)
        }
        log.Printf("unmounted=%t pv=%s target=%s container=%s",
            result.Unmounted, result.PVName, result.TargetPath, result.ContainerName)
        return nil

    default:
        return fmt.Errorf("unsupported operation %q", os.Args[1])
    }
}

func podIdentity() (namespace, podName, podUID string, err error) {
    namespace, err = requiredEnv("POD_NAMESPACE")
    if err != nil {
        return "", "", "", err
    }
    podName, err = requiredEnv("POD_NAME")
    if err != nil {
        return "", "", "", err
    }
    podUID, err = requiredEnv("POD_UID")
    if err != nil {
        return "", "", "", err
    }
    return namespace, podName, podUID, nil
}

func requiredEnv(name string) (string, error) {
    value := os.Getenv(name)
    if value == "" {
        return "", fmt.Errorf("%s is required", name)
    }
    return value, nil
}

func envOr(name, fallback string) string {
    if value := os.Getenv(name); value != "" {
        return value
    }
    return fallback
}
```

示例运行方式：

```sh
go run . health

PV_NAME=pv-workspace TARGET_PATH=/workspace/data SOURCE_SUB_PATH=users/alice CONTAINER_NAME=main go run . mount

PV_NAME=pv-workspace TARGET_PATH=/workspace/data CONTAINER_NAME=main go run . unmount
```

真实 Pod 中的 `POD_NAMESPACE`、`POD_NAME` 和 `POD_UID` 应由 Downward
API 注入，不应手工伪造。

## 方法语义

### Mount

`Mount` 会把请求编码为 `POST /v1/mount`，并携带当前
`TokenFile` 中的 bearer token。wrapper 成功完成实时校验和节点挂载后返回
`MountResult`。

`SourceSubPath` 必须是 PV 内安全的相对目录路径。默认情况下它必须已存在；wrapper
开启 `WRAPPER_CREATE_MISSING_SUBPATHS` 后可以逐级创建缺失目录，请求 mode 由
`WRAPPER_CREATED_SUBPATH_MODE` 控制（默认 `0770`）并传给 `mkdirat`。实际 mode
受 wrapper 进程 umask、文件系统/NFS default ACL 和 export policy 影响，wrapper
不会 `chmod` 或 `chown`。在启用
`root_squash` 的 export 上，应先验证匿名 UID/GID 是否能创建目录，并确认业务
容器能访问创建后的 owner/group/mode。

重复发送相同 Pod、container、target 和 PV 的请求用于处理调用方未收到成功响应的
情况。不要用不同 PV 或 subPath 隐式替换同一个 target；应先显式 unmount。

### Unmount

`Unmount` 使用与 mount 相同的 TokenReview 和实时资源校验。wrapper 会先删除
期望挂载状态，再卸载当前容器 namespace 中的目标，避免重协调重新创建已明确取消的
挂载。目标已经不存在时，调用仍可幂等成功。

### Health

`Health` 调用 wrapper UDS 上的 `GET /healthz`，返回 `Status` 和
`DriverName`。它只说明 wrapper 进程正在提供 socket API，不代替一次真实
mount 验证。

### CloseIdleConnections

一个进程通常复用一个 `Client`。进程退出或不再使用 Client 时调用
`CloseIdleConnections`，释放 UDS HTTP transport 中的空闲连接。

## 错误处理

`NewClient` 返回本地配置错误；`Mount` 和 `Unmount` 还可能返回：

- token 文件读取失败或内容为空；
- wrapper socket 不存在、权限不足或连接失败；
- context 取消或 HTTP 超时；
- wrapper 返回的请求格式、TokenReview、Pod/PV/PVC、driver、container 或路径校验错误；
- 节点 mount/unmount 操作失败。

wrapper 返回非 2xx 状态时，SDK 返回 `*mounter.ResponseError`。调用方可以通过
`errors.As` 读取 `Operation`、`StatusCode` 和不含 token 的 `Message`，
区分输入或鉴权错误与服务不可用。

建议：

- 始终给操作传入有界 `context.Context`；
- 使用 `%w` 包装错误，保留 `errors.Is` 对 context 错误的判断能力；
- 连接中断或超时发生在请求发出之后时，最终结果可能不确定；使用完全相同的请求重试，
  不要改成另一个 PV 或 target；
- 鉴权、driver、Pod UID 或路径校验错误应先修正输入和部署配置，不要无界重试；
- 不在日志、错误包装或 telemetry 中记录 token 内容。

## 与容器重启重协调的关系

SDK 进程不运行 informer，也不保存 node mount 状态。首次 mount 成功后，节点
wrapper 会把不含 token 和 NFS 凭据的期望挂载写入节点状态目录。目标业务容器获得
新的 container ID 时，wrapper 重新检查实时 Pod、PV 和 PVC，并把挂载恢复到新的
mount namespace。

恢复是最终一致的。容器创建和 informer 处理完成之间，目标路径可能暂时未挂载。
需要在应用第一条指令前保证挂载存在时，应由可信 runtime 增加启动门禁或重试。

底层 JSON/HTTP 契约见 [用户调用 API](api.md)，完整信任边界见
[Security Model](security-model.md)。

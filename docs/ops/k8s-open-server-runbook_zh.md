# K8s 开服操作手册

本手册仅适用于生产环境 Kubernetes 开服操作。
不涵盖 Docker Compose 或本地进程启动方式。

## 1. 前置条件

1. `kubectl` 可访问目标集群，且具有命名空间操作权限。
2. Linux 运行时产物已准备就绪（不含 `.exe` 二进制文件）。
3. 目标镜像仓库可写。
4. 区服规划已就绪：区服名称、区服 ID、副本数量。

建议快速检查：

```powershell
kubectl version --short
kubectl get nodes
```

## 2. 准备运行时产物

将 Linux 二进制文件和资源复制到 `deploy/k8s/runtime/linux`：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-stage-runtime `
  -BinarySourceRoot D:/linux-build/bin `
  -ZoneInfoSource bin/zoneinfo `
  -TableSource generated/tables
```

运行预检以阻止不完整的运行时输入：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-image-preflight
```

## 3. 构建并推送运行时镜像

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-build-image `
  -ImageRepository ghcr.io/luyuancpp/mmorpg-node `
  -ImageTag v1

pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-push-image `
  -ImageRepository ghcr.io/luyuancpp/mmorpg-node `
  -ImageTag v1
```

## 4. 暴露方式检查

在生产开服前，执行 dry-run 暴露检查，确保所选 profile 和 Service 类型在命令输出中明确显示。

预期结果：

1. `custom` 未显式指定 `GateServiceType` 时解析为 `NodePort`。
2. `managed-cloud` 解析为 `LoadBalancer`。
3. `custom + LoadBalancer` 会打印警告，让运维人员明确确认集群具备 LB 能力。

```powershell
pwsh -NoProfile -Command "Set-Location 'F:\work\mmorpg'; Write-Host '--- CASE1 custom default ---'; pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -DryRun -ZoneName testa -ZoneId 201; Write-Host '--- CASE2 managed-cloud ---'; pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -DryRun -ZoneName testb -ZoneId 202 -OpsProfile managed-cloud; Write-Host '--- CASE3 custom + LoadBalancer ---'; pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -DryRun -ZoneName testc -ZoneId 203 -OpsProfile custom -GateServiceType LoadBalancer"
```

如果输出与预期的 Service 类型解析不匹配，请在部署前停止操作，并修正命令的 profile 或 Service 类型。

## 5. 开启单个区服

托管云 profile 示例：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName yesterday `
  -ZoneId 101 `
  -OpsProfile managed-cloud `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:v1 `
  -WaitReady
```

裸金属 profile 示例：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName yesterday `
  -ZoneId 101 `
  -OpsProfile bare-metal `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:v1 `
  -WaitReady
```

部署后检查：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-status -ZoneName yesterday
```

## 6. 开启全部区服

使用 YAML 区服配置以提高运维可读性：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-up `
  -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml `
  -OpsProfile managed-cloud `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:v1 `
  -WaitReady
```

部署后检查：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-status `
  -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml
```

## 7. 发布快捷命令

单区服一键发布：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-release-zone `
  -ZoneName yesterday `
  -ZoneId 101 `
  -OpsProfile managed-cloud `
  -ImageRepository ghcr.io/luyuancpp/mmorpg-node `
  -ImageTag v1 `
  -WaitReady
```

全区服一键发布：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-release-all `
  -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml `
  -OpsProfile managed-cloud `
  -ImageRepository ghcr.io/luyuancpp/mmorpg-node `
  -ImageTag v1 `
  -WaitReady
```

## 8. 回滚

使用上一个已知正常的镜像标签重新部署。

单区服回滚：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName yesterday `
  -ZoneId 101 `
  -OpsProfile managed-cloud `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:v0 `
  -WaitReady
```

全区服回滚：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-up `
  -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml `
  -OpsProfile managed-cloud `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:v0 `
  -WaitReady
```

紧急关服：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName yesterday
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-down -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml
```

## 9. 故障排查

常用检查命令：

```powershell
kubectl get ns
kubectl get pods -n mmorpg-zone-yesterday -o wide
kubectl describe pod <pod-name> -n mmorpg-zone-yesterday
kubectl logs <pod-name> -n mmorpg-zone-yesterday --tail=200
kubectl get svc -n mmorpg-zone-yesterday
```

C++ 节点的业务日志**不在 stdout 上**。对 C++ Pod 跑 `kubectl logs`，除了 gRPC / abseil /
librdkafka 自己打的 stderr，就只剩 gate 的 `[gate_version] ...` 启动行 —— 而这行是 **gate 专有**的：
scene / battle 连启动行都没有，它们的 stdout 上只有上述 stderr。业务日志被 muduo 写进共享卷
`node-logs` 上的 `/app/bin/logs/cpp_nodes/` 文件里。要看它们有两条路：

1. 查 Loki。Alloy sidecar 把这些文件送进了 infra namespace 里的 Loki，
   port-forward 方式与标签约定见 [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §6。
2. 直接进 Pod 看文件：

```powershell
# zone 内的节点（gate 与 scene 各池）在 zone namespace 里
kubectl exec <pod-name> -n mmorpg-zone-yesterday -c <节点容器> -- ls /app/bin/logs/cpp_nodes
# battle 是全局池，部署在 infra namespace，不在任何 zone namespace 里
kubectl exec <pod-name> -n mmorpg-infra -c battle -- ls /app/bin/logs/cpp_nodes
```

`<节点容器>` 只在**单 scene 池**的档位下等于节点名（`gate` / `scene`）；scene 角色拆分档下池名是
`scene-world` / `scene-instance`，**容器名取的就是池名**，`-c scene` 会报 `container not found`。
拿不准先查一下：`kubectl get pod <pod-name> -n <namespace> -o jsonpath='{.spec.containers[*].name}'`。
battle 部署在 infra namespace（默认 `mmorpg-infra`），它不属于任何 zone，所以 Loki 里它的
`zone` 标签值是 `global`，不是某个区服名。

文件**只保留最近 8 个滚动分片**，更早的已被容器内的清理循环删掉 ——
muduo 每 8MiB 滚一个新文件且从不删旧文件。比这 8 个更早的日志只能去 Loki 查。

另外，每个 C++ Pod 现在多一个名为 `log-sidecar` 的**原生 sidecar**（写在 `initContainers` 里，带 `restartPolicy: Always`），
它计入 Pod Ready，所以 `kubectl get pod` 看到的容器数比以前多一个（gate 的 Pod 是 `2/2`）。
**它拉不到镜像时业务容器根本不会启动**：Pod 的 STATUS 是 `Init:ErrImagePull` 或
`Init:ImagePullBackOff`（按这两个字符串搜才搜得到），`kubectl describe pod` 的报错在
**Init Containers** 一节，gate / scene / battle 进程一次都没跑起来。kind 需要先把
`grafana/alloy:v1.10.0` 载入节点。想排除 sidecar 干扰就带 `-NoCppLogSidecar` 重新部署 ——
关掉后消失的只有 sidecar 容器、它的两个卷（`cpp-log-sidecar-config` / `cpp-log-sidecar-state`）
和 pod 模板上的 `mmorpg.io/cpp-log-sidecar-config-hash` 注解（scene 的 Agones Fleet 上还会少一行 `container: <池名>`）；
业务容器里的日志清理循环、`node-logs` 的 `sizeLimit: 2Gi`、`snowflake-cache` 的 64Mi 都是**无条件**渲染的，
关掉 sidecar 不会把它们变回去。

> 现状（2026-09-19）：上面这套形态**已落码，并在本机 kind 集群的临时 namespace 里端到端跑通过**
> （假 C++ 节点写 muduo 格式行 + 真 Loki），但**从未在真实 zone / infra namespace 上 apply 过**。
> 验证范围与仍未验证项见 [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §7。

如果 `WaitReady` 失败：

1. 检查镜像拉取错误和仓库凭据。
2. 检查 ConfigMap 数据和挂载的运行时路径。
3. 检查 Gate Service 暴露模式是否与集群类型匹配。
4. 验证基础设施 Pod（`etcd`、`redis`、`kafka`）是否健康。

## 10. 变更记录模板

每次操作保留简短的发布记录：

1. 日期时间和操作人。
2. 执行的命令。
3. 镜像标签。
4. 受影响的区服。
5. 验证结果。
6. 回滚决策和最终状态。

# K8s 开服操作手册

> 2026-10-15 按当前部署脚本修订。本文面向集群外玩家的生产部署;命令仅作操作说明,本次文档修订没有执行部署、推送或压测。中英文旧副本的操作步骤统一按本修订执行。

## 1. 前置条件

在仓库根目录操作,先确认目标集群、区服清单、镜像与入口规划。完整参数见 [部署目录说明](../../deploy/k8s/README.md),发布门禁见 [发布清单](release-checklist.md),入口与切换约束见 [集群外客户端入口](../design/k8s-client-entry.md)。

- 集群已具备 Ingress controller、DNS / TLS、可用的存储类,并已安装本例使用的 Agones。生产 battle 用 Agones 动态端口;普通 Deployment + hostPort 只作验证与回退。
- C++、全部启用的 Go 服务与 Java gateway 镜像都已构建、验证,并由有权限的发布者推到目标仓库。AI 不执行推送。
- 使用明确的 `ReleaseProfile=prod` 或 `staging`;默认 `dev` 不代表生产档。先完成该档位的 `release_preflight.ps1` 门禁。
- 发布环境通过受控方式注入密钥,不把值写进本文、Git 文件、命令历史或日志。部署脚本使用 `MMORPG_GATE_TOKEN_SECRET`、`MMORPG_INTERNAL_AUTH_SECRET`、`MMORPG_BATTLE_TOKEN_SECRET`、`MMORPG_SPECTATE_TOKEN_SECRET`、`MMORPG_LOGIN_QUEUE_TOKEN_SECRET`、`MMORPG_GATEWAY_ADMIN_API_KEY`,以及 `MMORPG_MYSQL_USER/PASSWORD`、`MMORPG_GATEWAY_DB_USER/PASSWORD`、`MMORPG_REDIS_PASSWORD`。HMAC 与管理密钥至少 32 字符,数据库 / Redis 密码至少 12 字符;用途不同的密钥不得复用。数据库账号必须已在目标实例正确创建并授权,各业务服务的其他专用凭据按其配置准备。
- `ZonesConfigPath` 要包含实际区服,不能把示例区服直接当成已审批规划;`ClusterId` 是部署级常量,已有环境不得随意改值。

`-DryRun` 会输出部分 ConfigMap / Secret 的渲染内容;只用不含真实密钥的离线输入检查结构,不要把真实密钥的 dry-run 输出归档。

## 2. 准备 C++ 运行时产物

这些必须是 Linux 可执行文件,包括 gate、scene、battle,不能用 Windows `.exe`。暂存目录与构建路径见 [runtime 说明](../../deploy/k8s/runtime/README.md)。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-stage-runtime `
  -BinarySourceRoot "<Linux二进制目录>" `
  -ZoneInfoSource bin/zoneinfo -TableSource generated/tables
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-image-preflight
```

## 3. 镜像与版本

正式发布按 [发布清单](release-checklist.md) 出包并记录不可变 tag / digest。以下部署参数使用已推送、已验证的完整镜像引用。C++、Go、Java 的 tag 可以不同,但必须来自同一份已核对的发布清单。

`k8s-release-zone / k8s-release-all` 是 C++ 镜像构建 / 推送与节点发布包装,**不会部署 Go / Java 服务**。全栈发布使用下列 `k8s_deploy.ps1` 入口;不能只跑包装命令就宣称 login、router 与 gateway 已发布。

## 4. 明确本次部署参数

托管云示例使用每个 gate 序号各自的 LoadBalancer 与 DNS 名。替换全部占位后再运行:

```powershell
$ErrorActionPreference = "Stop"
$Deploy = @{
  KubeContext = "<目标 context>"
  KubeConfig = "<kubeconfig 绝对路径>"
  ReleaseProfile = "prod"
  OpsProfile = "managed-cloud"
  InfraNamespace = "mmorpg-infra"
  ZonesConfigPath = "deploy/k8s/zones.ops-recommended.yaml"
  ClusterId = 0
  NodeImage = "<镜像仓库>/mmorpg-node:<不可变 tag>"
  GoSvcRegistry = "<Go 镜像仓库前缀>"
  GoSvcTag = "<不可变 Go tag>"
  JavaSvcRegistry = "<Java 镜像仓库前缀>"
  JavaSvcTag = "<不可变 Java tag>"
  ClientEntryMode = "external"
  GateRouterMode = "1"
  BattleOrchestrator = "agones"
  GateClientHostTemplate = "gate-{ordinal}.{zone}.example.com"
  GatewayIngressHost = "gateway-{zone}.example.com"
  GatewayIngressClassName = "nginx"
  GatewayIngressTlsSecret = "<zone 内已创建的 TLS Secret 名>"
  GatewayTrustedProxies = "<Ingress controller 的可信 CIDR>"
  WaitReady = $true
}
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig get nodes
```

裸金属改为 `OpsProfile="bare-metal"`,gate 使用每序号 NodePort。每个 zone 的 `gateNodePortBase` 须在区服清单显式分配,范围不能重叠;DNS 模板必须让每个序号解析到实际可达的节点 / 独立 L4 入口,不能用随机分流到全部 gate 的单一入口代替。gate 票据绑定具体节点。

只设置 `OpsProfile` 或 `GateServiceType` 不会自动启用集群外入口。默认 `ClientEntryMode=podip` 只适合集群内 robot;podip 下 gate 副本数为 2 时连单一 `gate-entry` Service 都不会生成。生产示例必须显式保留 `external` 与每序号地址规划。

## 5. 先部署共享 infra 与全局服务

```powershell
& ./tools/scripts/k8s_deploy.ps1 -Command infra-up @Deploy
```

本步骤生成 MySQL 初始化 ConfigMap、预建审计 / 命令 topic,并在 infra namespace 部署全局 Go 服务(包括 client-rpc-router)与 battle。必须给出 `GoSvcRegistry` 且不带 `SkipGoSvc`;留空会跳过 Go 服务,不是自动复用节点镜像。上例 `Gateway*` 参数在 infra-up 只会提示被忽略,它们在下一步 zone-up 随 Java gateway 生效。

先确认共享存储、topic 预建 Job、全局 router 与 battle 就绪,再发布 zone。gate 路由模式 1 依赖 router;只重跑 zone-up 不会补齐缺失的全局服务。存量环境的 etcd / Redis / Kafka 形态切换有单独迁移要求,先按部署目录说明审查,不得把第一次部署命令当成无影响升级。

## 6. 发布单个或全部区服

单区示例(名称、ID 必须与区服清单一致):

```powershell
& ./tools/scripts/k8s_deploy.ps1 -Command zone-up `
  -ZoneName yesterday -ZoneId 101 @Deploy
```

全部区服:已完成上一节 infra 时加 `SkipInfra`,避免无意再次更新共享资源。

```powershell
& ./tools/scripts/k8s_deploy.ps1 -Command all-up -SkipInfra @Deploy
```

`GoSvcRegistry / JavaSvcRegistry` 分别控制 zone 内 Go 服务与 Java gateway 是否部署;缺任一个都会跳过相应服务。生产档默认打开限流与登录排队,所需 queue token 密钥必须已注入。不要为绕过门禁改用 dev 档或 `SkipPreflight`。

## 7. 部署后验收

```powershell
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-status `
  -ZoneName yesterday -KubeContext $Deploy.KubeContext -KubeConfig $Deploy.KubeConfig
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig `
  -n mmorpg-infra get pods
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig `
  -n mmorpg-zone-yesterday get statefulset,service,ingress
```

核对 gate-0、gate-1 的独立 Service 与 DNS,以及 gateway Ingress / TLS;用授权测试账号验证 server-list、assign-gate、gate 长连和 battle 直连。确认下发的是客户端可达地址而非 PodIP。管理接口不经公开 Ingress,验收从集群内进行。具体场景与运行证据按对应冒烟文档记录,`WaitReady` 只证明 Kubernetes 就绪条件,不替代业务验收。

## 8. 回滚与停止服务

回滚要从上一份发布清单取出 C++ / Go / Java 三类镜像身份,先审查数据库迁移与协议兼容。保持本次所有 context、namespace、`external`、DNS、Ingress、可信代理与路由模式参数,修改 `$Deploy` 中相应镜像后,按上述顺序部署;只回滚 zone 不更新 infra 中的全局 router / battle。

这些参数不粘滞。漏传可能退回 podip,脚本会拒绝会踢人的形态或地址切换;只有确认维护窗口与迁移方案后才使用 `AllowDisruptiveSwitch`。

紧急关服使用明确的目标 context 与配置:

```powershell
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-down -ZoneName yesterday `
  -KubeContext $Deploy.KubeContext -KubeConfig $Deploy.KubeConfig
```

zone-down 删除整个 zone namespace。全区关闭见 `all-down`;默认还会删除 infra namespace 及其 PVC,不能把它当成仅停业务进程的命令。只关闭 zone 时使用明确的 `SkipInfra` 与已核对的区服清单。

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

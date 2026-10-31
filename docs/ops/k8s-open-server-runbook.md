# K8s 开服操作手册

> 2026-10-15 按当前部署脚本修订。本文面向集群外玩家的生产部署;命令仅作操作说明,本次文档修订没有执行部署、推送或压测。中英文旧副本的操作步骤统一按本修订执行。

## 1. 前置条件

在仓库根目录操作,先确认目标集群、区服清单、镜像与入口规划。完整参数见 [部署目录说明](../../deploy/k8s/README.md),发布门禁见 [发布清单](release-checklist.md),入口与切换约束见 [集群外客户端入口](../design/k8s-client-entry.md)。

- 集群已具备 Ingress controller、DNS / TLS、可用的存储类,并已安装本例使用的 Agones。生产 battle 用 Agones 动态端口;普通 Deployment + hostPort 只作验证与回退。
- C++、全部启用的 Go 服务与 Java gateway 镜像都已构建、验证,并由有权限的发布者推到目标仓库。AI 不执行推送。
- 使用明确的 `ReleaseProfile=prod` 或 `staging`;默认 `dev` 不代表生产档。先完成该档位的 `release_preflight.ps1` 门禁。
- 发布环境通过受控方式注入密钥,不把值写进本文、Git 文件、命令历史或日志。部署脚本使用 `MMORPG_GATE_TOKEN_SECRET`、`MMORPG_INTERNAL_AUTH_SECRET`、`MMORPG_BATTLE_TOKEN_SECRET`、`MMORPG_SPECTATE_TOKEN_SECRET`、`MMORPG_LOGIN_QUEUE_TOKEN_SECRET`、`MMORPG_GATEWAY_ADMIN_API_KEY`,以及 `MMORPG_MYSQL_USER/PASSWORD`、`MMORPG_GATEWAY_DB_USER/PASSWORD`、`MMORPG_REDIS_PASSWORD`。HMAC 与管理密钥至少 32 字符,数据库 / Redis 密码至少 12 字符;用途不同的密钥不得复用。数据库账号必须已在目标实例正确创建并授权,各业务服务的其他专用凭据按其配置准备。
- `ZonesConfigPath` 要包含实际区服,不能把示例区服直接当成已审批规划;`ClusterId` 是部署级常量,已有环境不得随意改值。
- go/db 认领前按全局库 `player_route` 选库,运行账号要有这张表的 SELECT。`maintenance_drain.ps1 render-roles` 的计划不会自动带它,运维在 `tables` 里写上 `{schema: <全局库>, table: player_route, operations: [SELECT]}`。没有授权时 go/db 启动读路由失败,拒绝启动。
- match 同样直读全局库:它的运行账号在 `tables` 里要有 `player_route` 和 `zone_merged_into` 的 SELECT。没有 `zone_merged_into` 的读权限时 match 拒绝启动。合服工具的写权限在操作员角色 `deploy/sql/authority_operator_role.sql` 里,不进运行账号。

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

全新集群上,全局服务里除 client-rpc-router 以外,本步骤只写 ConfigMap、跑迁移 Job,进程要等第一个 zone 的 data-service 就绪才能起(启动依赖见部署目录说明「全局服务的部署顺序」),输出里有告警 `[GlobalGoSvc]Deferred:`。第一个 zone 就绪之后再跑一次本节命令,它们才起来。第一次部署也可以直接用 `all-up`(不带 `SkipInfra`),顺序由脚本排好。

先确认共享存储、topic 预建 Job、全局 router 与 battle 就绪,再发布 zone。gate 路由模式 1 依赖 router;只重跑 zone-up 不会补齐缺失的全局服务。存量环境的 etcd / Redis / Kafka 形态切换有单独迁移要求,先按部署目录说明审查,不得把第一次部署命令当成无影响升级。

## 6. 发布单个或全部区服

单区示例(名称、ID 必须与区服清单一致):

```powershell
& ./tools/scripts/k8s_deploy.ps1 -Command zone-up `
  -ZoneName yesterday -ZoneId 101 @Deploy
```

全部区服:已完成上一节 infra 时加 `SkipInfra`,避免无意再次更新共享资源。这两条命令都不起全局服务;全新集群上它们会告警 `[GlobalGoSvc]NotStarted:` 并点名没起的服务,这时再跑一次上一节的 `infra-up`。

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

## 9. Troubleshooting

Common checks:

```powershell
kubectl get ns
kubectl get pods -n mmorpg-zone-yesterday -o wide
kubectl describe pod <pod-name> -n mmorpg-zone-yesterday
kubectl logs <pod-name> -n mmorpg-zone-yesterday --tail=200
kubectl get svc -n mmorpg-zone-yesterday
```

C++ node business logs are **not** on stdout. Apart from gRPC / abseil / librdkafka's own
stderr, the only thing `kubectl logs` shows on a C++ Pod is gate's `[gate_version] ...` startup
line — and that line is **gate-specific**: scene and battle print no startup line at all, so their
stdout carries nothing but that stderr. muduo writes the business logs to files under
`/app/bin/logs/cpp_nodes/` on the shared `node-logs` volume. Two ways to read them:

1. Query Loki. An Alloy sidecar ships those files to the Loki in the infra namespace;
   port-forward instructions and label conventions are in
   [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §6.
2. Read the files in the Pod directly:

```powershell
# zone nodes (gate and the scene pools) live in the zone namespace
kubectl exec <pod-name> -n mmorpg-zone-yesterday -c <node-container> -- ls /app/bin/logs/cpp_nodes
# battle is a global pool deployed in the infra namespace, not in any zone namespace
kubectl exec <pod-name> -n mmorpg-infra -c battle -- ls /app/bin/logs/cpp_nodes
```

`<node-container>` equals the node name (`gate` / `scene`) only in the single scene-pool layout.
Under the scene role split the pools are `scene-world` / `scene-instance` and **the container name
is the pool name**, so `-c scene` fails with `container not found`. When in doubt, list them first:
`kubectl get pod <pod-name> -n <namespace> -o jsonpath='{.spec.containers[*].name}'`.
battle is deployed to the infra namespace (`mmorpg-infra` by default) and belongs to no zone, so in
Loki its `zone` label value is `global` rather than a zone name.

Only the 8 most recent rolled files are kept — an in-container cleanup loop deletes the rest,
because muduo rolls every 8MiB and never removes old files. Anything older than those 8 files
exists only in Loki.

Every C++ Pod also runs a **native sidecar** named `log-sidecar` (declared under `initContainers`
with `restartPolicy: Always`). It counts toward Pod readiness, so `kubectl get pod` reports one
container more than it used to (a gate Pod reads `2/2`). **If that image cannot be pulled, the
business container never starts at all**: the Pod STATUS is `Init:ErrImagePull` or
`Init:ImagePullBackOff` (search for those exact strings), the error appears under the
**Init Containers** section of `kubectl describe pod`, and the gate / scene / battle process has not
run even once. On kind, `grafana/alloy:v1.10.0` must be loaded into the node first. To rule the
sidecar out, redeploy with `-NoCppLogSidecar`: that removes only the sidecar container, its two
volumes (`cpp-log-sidecar-config` / `cpp-log-sidecar-state`) and the pod-template annotation
`mmorpg.io/cpp-log-sidecar-config-hash` (plus the `container: <pool-name>` line on the scene Agones
Fleet). The in-container log cleanup loop, the `node-logs` `sizeLimit: 2Gi` and the 64Mi
`snowflake-cache` volume are rendered **unconditionally** and stay exactly as they are.

> Status (2026-09-19): this shape is **committed and passed an end-to-end run in a temporary
> namespace on a local kind cluster** (fake C++ node writing muduo-format lines + real Loki), but it
> has **never been applied to a real zone / infra namespace**. What was and was not verified:
> [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §7.

If `WaitReady` fails:

1. Check image pull errors and registry credentials.
2. Check ConfigMap data and mounted runtime paths.
3. Check gate service exposure mode against cluster type.
4. Verify infra pods (`etcd`, `redis`, `kafka`) are healthy.

## 10. Change Record Template

Keep a short release note per operation:

1. Date/time and operator.
2. Command executed.
3. Image tag.
4. Zones affected.
5. Validation result.
6. Rollback decision and final status.

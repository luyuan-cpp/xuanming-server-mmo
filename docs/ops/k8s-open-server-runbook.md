# K8s 开服操作手册

> 2026-10-15 按当前部署脚本修订。本文面向集群外玩家的生产部署;命令仅作操作说明,本次文档修订没有执行部署、推送或压测。中英文旧副本的操作步骤统一按本修订执行。

## 1. 前置条件

在仓库根目录操作,先确认目标集群、区服清单、镜像与入口规划。完整参数见 [部署目录说明](../../deploy/k8s/README.md),发布门禁见 [发布清单](release-checklist.md),入口与切换约束见 [集群外客户端入口](../design/k8s-client-entry.md)。

- 集群已具备 Ingress controller、DNS / TLS、可用的存储类,并已安装本例使用的 Agones。生产 battle 用 Agones 动态端口;普通 Deployment + hostPort 只作验证与回退。
- C++、全部启用的 Go 服务与 Java gateway 镜像都已构建、验证,并由有权限的发布者推到目标仓库。AI 不执行推送。Go 侧包括 `go_svc_image.ps1` 目录里的两个工具镜像:`mmorpg-writer-authority`(写者 Pod 的 init)、`mmorpg-db-migrate`(`db-migrate` Job)。
- 写者 Pod 回收 CronJob 用官方镜像 `registry.k8s.io/kubectl`,版本钉在 `-SqlWriterReaperImage` 的缺省值。集群拉不到外网时推到内部仓库,部署时给这个参数。
- 使用明确的 `ReleaseProfile=prod` 或 `staging`;默认 `dev` 不代表生产档。先完成该档位的 `release_preflight.ps1` 门禁。
- 发布环境通过受控方式注入密钥,不把值写进本文、Git 文件、命令历史或日志。部署脚本使用 `MMORPG_GATE_TOKEN_SECRET`、`MMORPG_INTERNAL_AUTH_SECRET`、`MMORPG_BATTLE_TOKEN_SECRET`、`MMORPG_SPECTATE_TOKEN_SECRET`、`MMORPG_LOGIN_QUEUE_TOKEN_SECRET`、`MMORPG_GATEWAY_ADMIN_API_KEY`,以及 `MMORPG_MYSQL_USER/PASSWORD`、`MMORPG_GATEWAY_DB_USER/PASSWORD`、`MMORPG_REDIS_PASSWORD`。HMAC 与管理密钥至少 32 字符,数据库 / Redis 密码至少 12 字符;用途不同的密钥不得复用。数据库账号必须已在目标实例正确创建并授权,各业务服务的其他专用凭据按其配置准备。
- `ZonesConfigPath` 要包含实际区服,不能把示例区服直接当成已审批规划;`ClusterId` 是部署级常量,已有环境不得随意改值。
- 写者闸门每个逻辑库一份([设计 §2](../design/horizontal-scale-logical-db.md))。`maintenance_drain.ps1 render-roles` 的计划按逻辑库各写一份:`authoritySchema` 和 `schemaDatabases` 都是这个库,`registrationRequestFile` 是这个库的 REGISTER;每个目录片的计划再加 `directorySchema: <全局库名>_00` 和 `directoryShard: true`(只有第 0 片有 `zone_merged_into`,合服工具的授权由 `directorySchema` 带出;`directoryShard` 带出 schema 账号种 `directory_shard` 的权限;K=1 时第 0 片就是 `<全局库名>`)。写者账号只要自己要写的库的闸门表权限(SELECT,LOCK_TOUCH 档再加 `writer_slot.lock_touch` 的 UPDATE),不再有跨库授权。唯一的例外是 data_service 在存档库的表:`player_store` 读写,`player_centre_database`、`user_accounts` 只读(空路由启动判据)。各服务的 `role_fence` 在本服务的库里,授权跟着本库的业务表走。流水库的计划要列全 `transaction_log`、`player_snapshot`、`rollback_audit_log`、`gm_recall_batch`、`gm_recall_op`、`gm_recall_seq`、`gm_recall_source`、`role_fence`,操作都是 `SELECT,INSERT,UPDATE,DELETE`。少了回收存储就起不来,`Store.Required=true` 时 data_service 拒启;`Recall.Enabled=false` 也一样,护栏要靠这个存储判挂起。
- **授权分两步,顺序不能反**。表级 `GRANT` 对不存在的表报 1146(MySQL、TiDB 都是),空库上只有 schema 账号的授权执行得了:
  1. 迁移之前,每个逻辑库跑一次 `maintenance_drain.ps1 -MaintenanceCommand render-schema-role -PlanPath <计划> -OutputPath <输出>`,把输出的 SQL 执行掉。计划只要 `authoritySchema`、`schemaPrincipal`、`schemaDatabases`,目录片再加 `directoryShard: true`。这时还没有 generation,也就没有 REGISTER 原件,所以不用 `render-roles`。`mmorpg` 的表来自 `gateway_tables.sql`,不走这一步。
  2. 迁移之后(§5),`render-roles` 输出的 JSON 里 `operator`、`runtime` 两段再执行,它们都是表级授权。`schema` 段和第 1 步是同一份,不用再执行。
- 有限名册(`rosterFile`)的每条 entry 是一个 boot:`requests` 里每个要写的逻辑库一份 REGISTER,`authoritySchema` 是这个库,`protectedSchemas` 只有这个库,`writer` 身份和账号相同;`generation` 取这个库 `route_meta` 的值,由 `-migrate` 随机生成,所以要先跑迁移 Job 才能定名册,顺序见 §5。REGISTER 不带 `expectedEpoch`:每次搬库或回滚 epoch 都变,名册绑了它就废了,收据记落地时的实际 epoch。写了这个字段的请求会被拒绝。go/db 要写几个落点库就几份,`gateway` 写 `mmorpg`。注册账号的连接串指哪个库都行,`writer_authority` 按 REGISTER 的库名切换;所有库要在这个连接串的集群上。
- **名册要留余量**。一条 entry 是一个 boot,只能用一次:每次 Pod 启动烧一条,一次滚动发布换一遍副本,写过之后崩溃的写者 Pod 被回收 CronJob 删掉再烧一条(§9),节点驱逐也烧一条。还没写过就退出的进程(依赖没就绪)原地重启,不烧。
  - 怎么算:`k8s_deploy.ps1` 要求条数 ≥ 副本数 × 6,上限 1024,不够就在 apply 之前拒绝。6 = 当前这一代 1 + 一次滚动发布 1 + 每个副本 4 次崩溃或驱逐的余量。这是下限。两次换名册之间预计还有 N 次发布,就再加 副本数 × N;依赖长时间不就绪时进程退出、Pod 被换,同样在烧,崩溃多的环境多给。
  - 用完了会怎样:还活着的 Pod 不受影响。新 Pod 的 init 报 `有限roster全部已由其它明确boot消费`,停在 Init,回收 CronJob 不删它,告警 `SqlWriterRegistrarFailing` 响。在这之前少掉的副本补不回来。
  - 怎么办:写一份新名册(新的 writer 身份和账号,`generation` 不变,账号按上面的第 2 步授权),更新目录的 `rosterFile`、`rosterSha256`,重新部署,这会滚动这个服务。
- go/db 认领前调 data_service 的 `GetPlayerRoute` 选库,不连目录([设计 §7](../design/horizontal-scale-logical-db.md)):配置 `DataServiceRpc` 必填,两边都要有 `MMORPG_ROLE_FENCE_SECRET_DB`,缺了 go/db 拒绝启动。
- **TiDB 档先开 `tidb_enable_check_constraint`,再建库**:`SET GLOBAL tidb_enable_check_constraint = ON;`。`player_route` 的唯一性靠 CHECK,TiDB 默认不执行,建完表才打开不一定补上约束。`data_service -migrate` 建表之前查这个开关,没开就拒绝。开了就不要再关。MySQL 档不用管。
- 目录按 `Directory.LogicalShards` 分片,流水和快照在 `<全局库名>_ledger`([设计 §1、§5.1](../design/horizontal-scale-logical-db.md))。`-DirectoryLogicalShards`:prod 必须显式 64,staging 必须显式(64 的约数),dev 缺省 1。K 上线后不能改。这些库 `-migrate` 之前要建好,要是空库;MySQL 档由 `infra-up` 的 `mysql-init-sql` 预建,TiDB 档由运维预建片库 `<全局库名>_00..63`、`<全局库名>_ledger` 和各业务库。
- data_service 的 boot 名册(`rosterFile`)每条 entry 的 `requests` 要恰好是片库、流水库和 `MMORPG_DB_ALLOWED_DATABASES` 列出的全部存档库,一共 K+1+存档库数份,多一份少一份都拒绝启动。`k8s_deploy.ps1` 在 apply 之前核一次,对不上就拒绝。
- `-Region` 是地域码,写进 ConfigMap 模板的 `IdSegment.Issuer.Region`,同时注入 data-service 主容器的 `MMORPG_ID_ISSUER_EXPECTED_REGION`。常驻进程读的是 Secret 里抄的配置,抄错了 data_service 比对这个变量,拒绝启动。staging、prod 必须显式给 1..63,0 只给 dev。一旦发号不能改,不能和别的地域重复。开新地域见 [开新地域手册](open-new-region-runbook.md)。
- 合服映射只在目录第 0 片,各服务经 `GetMergedZones` 读,不连目录。合服工具的写权限在操作员角色 `deploy/sql/authority_operator_role.sql` 里,不进运行账号。
- **运维预置的每 boot 配置要带上新段**。常驻进程的配置来自 `runtimeConfigSecret`,schema Job 来自 `schemaConfigSecret`,`k8s_deploy.ps1` 渲染的 ConfigMap 是模板:把 ConfigMap 里的 `Directory`、`RoleFence`、`Recall`、`SharedRedis`、`PlayerRedis`、`MethodTimeouts`、`DbPlacement`、`IdSegment.Issuer.Region`、`DataServiceRpc`、`GuildRpc`、`MatchRpc` 抄进去,再填账号。两边不一致时以 Secret 为准,配置缺了服务拒绝启动。staging、prod 的 `schemaConfigSecret` 必须带 `DbPlacement`,没有时 `-migrate` 拒绝运行(`MMORPG_DB_PLACEMENT_REQUIRED`):它靠这一段看库是不是在搬。go/db 的 `db-migrate` Job 同样注入;手工跑 `cmd/migrate -command up` 时自己设这个环境变量,配置文件同样带这一段。
- **schema Job 的账号**。目录条目的 `schemaPermissionSecret` 有两个键,必须是同一个账号:`principal` 所有 Job 都读(`MMORPG_SQL_SCHEMA_PRINCIPAL`);`dsn` 只有 guild、mail、rank 读,这三个服务的连接串走环境变量。两边对不上时 Job 报 `权威SQL受限主体不匹配`。其余服务的账号在 `schemaConfigSecret` 的配置里(chat 是 `chat-authority-secrets` 的 `migration-dsn`,match 是 `domain-service-secrets` 的 `match-migration-user`、`match-migration-password`),同样要等于这个 `principal`。`schemaConfigSecret` 的键名是配置文件名(`db.yaml`、`data_service.yaml` 等),内容从 `-DryRun` 渲染的 ConfigMap 抄。
- **预置 Secret `domain-service-secrets` 的角色护栏密钥**。值由运维生成,不进仓库、不进日志;每把至少 32 字节,互不相同(data_service 拒绝跨用途复用),同名 key 在各 namespace 同值。manifest 都是 `optional: false`,缺一个 key 对应的 Pod 卡在 `CreateContainerConfigError`:

  | key | 环境变量 | 放在哪 | 谁用 |
  |---|---|---|---|
  | `role-fence-coordinator` | `MMORPG_ROLE_FENCE_SECRET_COORDINATOR` | 每个 zone namespace、infra namespace | data-service 调参与者,六个参与者验 |
  | `asset-data-service` | `MMORPG_ASSET_OP_SECRET_DATA_SERVICE` | 每个 zone namespace | GM 批量回收的资产指令签名,data-service 签、scene 验,两边同值。`optional: true`:`-AssetOps off` 时用不到;`-AssetOps on` 时 data-service 缺它拒启 |
  | `login-admin-token` | `MMORPG_LOGIN_ADMIN_TOKEN` | 每个 zone namespace | `LoginAdmin` 的运维口令(`x-admin-token`),至少 16 字节。`optional: true`:没有这个 key 时运维面整体停用 |
  | `role-fence-guild` | `MMORPG_ROLE_FENCE_SECRET_GUILD` | 每个 zone namespace、infra namespace | guild 读权威状态,data-service 验 |
  | `role-fence-trade` | `MMORPG_ROLE_FENCE_SECRET_TRADE` | 同上 | trade |
  | `role-fence-mail` | `MMORPG_ROLE_FENCE_SECRET_MAIL` | 同上 | mail |
  | `role-fence-match` | `MMORPG_ROLE_FENCE_SECRET_MATCH` | 同上 | match |
  | `role-fence-player-inbox` | `MMORPG_ROLE_FENCE_SECRET_PLAYER_INBOX` | 同上 | player-inbox |
  | `role-fence-friend` | `MMORPG_ROLE_FENCE_SECRET_FRIEND` | 同上 | friend |
  | `role-fence-chat` | `MMORPG_ROLE_FENCE_SECRET_CHAT` | 同上 | chat 读权威状态(只读,不是参与者) |
  | `role-fence-db` | `MMORPG_ROLE_FENCE_SECRET_DB` | 每个 zone namespace | go/db 调 `GetPlayerRoute`,data-service 验 |
  | `match-internal-guild` | `MMORPG_MATCH_INTERNAL_SECRET_GUILD` | infra namespace | guild 和 match 同值:开局、占用、读终局都验它 |

- 参与者经 infra namespace 的 K8s Service 直连(`guild.<infra>:50300`、`trade.<infra>:50800`、`mail.<infra>:50900`、`match.<infra>:50500`、`player-inbox.<infra>:50950`、`friend.<infra>:50400`),`k8s_deploy.ps1` 按 `-InfraNamespace` 渲染。`-NetworkPolicies on` 时 guild、match 的放行名单已含 data-service。任何一个参与者不可达,过户和删角一律拒绝,其余功能不受影响。
- 告警装 `deploy/k8s/logical-db-alerts.yaml`,安装方式和抓取前提写在文件头。闸门冻结时长没有指标,要查库。

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
  # prod 必须是 64,上线后不能改。
  DirectoryLogicalShards = 64
  # 地域码 1..63,0 只给 dev。一旦发号不能改,不能和别的地域重复,见开新地域手册。
  Region = <地域码>
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

全新集群上,全局服务里除 client-rpc-router 以外,本步骤只写 ConfigMap、跑迁移 Job,进程要等第一个 zone 的 data-service 就绪才能起(启动依赖见部署目录说明「全局服务的部署顺序」),输出里有告警 `[GlobalGoSvc]Deferred:`。第一个 zone 就绪之后再跑一次本节命令,它们才起来。

### 全新集群的顺序

名册要按迁移 Job 种下的 `route_meta.generation` 写,所以 Job 和进程分两遍跑。Job 重复跑是幂等的:已经齐全的库跳过,不换 generation。

1. 建库(TiDB 档先开 `tidb_enable_check_constraint`,见 §1)。按 §1 给 schema 账号授权(`render-schema-role`),预置 `schemaConfigSecret`、`schemaPermissionSecret`。
2. 写部署目录 `MMORPG_SQL_K8S_BOOT_CATALOG`。这一遍每个写者的条目只要 `schemaConfigSecret`、`schemaPermissionSecret` 两个字段。
3. 跑迁移 Job,不起写者进程:

   ```powershell
   & ./tools/scripts/k8s_deploy.ps1 -Command all-up -SqlSchemaOnly @Deploy
   ```

   它做的是:infra、全局服务的 ConfigMap 与迁移 Job,client-rpc-router 的进程,然后每个 zone 的 `db-migrate`、`data-service-migrate`。`db-migrate` 迁 `MMORPG_DB_ALLOWED_DATABASES` 列出的全部存档库。这一步恒等 Job 结束,失败或超时就中断,看 `kubectl -n <namespace> logs job/<服务>-migrate`。只加一个 zone 时用 `zone-up -ZoneName <zone> -ZoneId <id> -SqlSchemaOnly`。
4. 逐库读 generation,目录各片、流水库、全部存档库、各业务库都要读,`mmorpg` 也要:

   ```sql
   SELECT generation FROM route_meta WHERE id = 1;
   ```

5. 写每个写者的 REGISTER 原件和名册(§1,条数按「名册要留余量」),`render-roles` 之后执行 `operator`、`runtime` 两段,把 boot 的 Secret 预置好,目录条目补全 `requestSecret`、`registrarSecret`、`registrarImage`、`physicalAuthoritySha256`、`runtimeConfigSecret`、`replicas`、`rosterFile`、`rosterSha256`。
6. 起进程,顺序由脚本排好(§6):

   ```powershell
   & ./tools/scripts/k8s_deploy.ps1 -Command all-up -SkipInfra @Deploy
   & ./tools/scripts/k8s_deploy.ps1 -Command infra-up @Deploy
   ```

   第一条跑一遍 Job(都是跳过),再起 zone 里的进程;第二条看到 data-service 就绪,起其余全局服务。也可以不分开,一条不带 `SkipInfra` 的 `all-up`。`@Deploy` 带 `WaitReady` 时第一条会等到 scene 就绪,而 scene 等发号器:等 `deploy/data-service` 就绪之后另开一个终端做第 7 步的初始化,或者这一遍去掉 `WaitReady`。
7. 初始化发号器(§6.1),验收(§7),然后才放流量。

目录里缺名册时,不带 `SqlSchemaOnly` 的 `zone-up` 在写任何东西之前拒绝,报错点名这个服务。

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

### 6.1 初始化发号器

只在全新环境做一次,在第一个 zone 的 data-service 就绪之后、放流量之前。两份预留记录都不存在时发号器拒绝发号,这是刻意的,它分不清首次部署和两套 etcd 同时丢数据。

```powershell
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig -n <zone-namespace> `
  exec deploy/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-issuer-init
```

输出 `both reservation records created` 就完成了,重复执行是安全的。mail、rank 不因为它卡启动,它们的发号器预领失败只记日志。没初始化时建角和发号报错,scene 的号段门禁不放行,所以 zone 就绪不了。初始化之后不用重启任何服务,客户端按退避自动重试。详见[发号器运维手册](id-issuer-runbook.md) §2。

## 7. 部署后验收

```powershell
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-status `
  -ZoneName yesterday -KubeContext $Deploy.KubeContext -KubeConfig $Deploy.KubeConfig
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig `
  -n mmorpg-infra get pods
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig `
  -n mmorpg-zone-yesterday get statefulset,service,ingress
```

核对写者 Pod 实际读到的地域码。ConfigMap 只是模板,常驻进程读 Secret 里抄的配置,下面两个数都要等于这次的 `-Region`:

```powershell
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig -n mmorpg-zone-yesterday `
  exec deploy/data-service -- grep -n 'Region:' /app/etc/private/data_service.yaml
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig -n mmorpg-zone-yesterday `
  exec deploy/data-service -- printenv MMORPG_ID_ISSUER_EXPECTED_REGION
```

核对 `kubectl -n <namespace> get cronjob sql-writer-pod-reaper` 在每个 zone namespace 和 infra namespace 都有。

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

写者 Pod(db、data-service、login、gateway 和 infra namespace 的全局服务):

- **`Init:Error` / `Init:CrashLoopBackOff`**:看 `kubectl logs <pod> -c sql-writer-registrar --previous`。`有限roster全部已由其它明确boot消费` 是名册用完了(§1)。闸门表缺失是迁移 Job 没跑或没覆盖这个库,`generation` 不符是名册按错的库写的。这种 Pod 回收 CronJob 不删,告警是 `SqlWriterRegistrarFailing`。闸门冻结(排空、搬库)时 init 什么也不写,解冻后自己过去。
- **主容器崩溃、被 OOMKill**:写资格一次性,写过之后容器原地重启读到消费标记就拒启,入口脚本以退出码 78 退出,这个 Pod 自己恢复不了。`sql-writer-pod-reaper` 每分钟删一次主容器上一次以 78 退出的写者 Pod,ReplicaSet 建的新 Pod 领名册下一条。不用人工删。还没写过就退出的进程没有消费标记,原地重启,不删。看它删了什么:`kubectl -n <namespace> logs job/<job> -c select-restarted-writers`(删之前)、`-c delete-restarted-writers`(删之后),两边都写明 namespace 和 Pod 名。
- **被删的 Pod 留下一个没有进程的写者**:它在各逻辑库的注册还在,和注册完就崩的 Pod 一样,下一次排空之前按非 run 的 RETIRE 退休([设计 §2](../design/horizontal-scale-logical-db.md))。
- **Job 或 CronJob 起不来**:`sql-writer-pod-reaper` 的镜像拉不到时只是这一分钟没回收,不影响别的 Pod。看 `kubectl -n <namespace> describe job <job>`。

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

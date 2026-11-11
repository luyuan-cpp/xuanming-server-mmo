# 开新地域手册

> **状态**:部署参数和闸已按[设计 §9](../design/horizontal-scale-logical-db.md)落码,**未编译、未测试、未演练**。下面的步骤没有在真实环境走过,第一次按它做之前先在 staging 走一遍。

开一个新地域就是再部署一整套服务器。地域之间不共享数据库,集群不跨洋,30 毫秒的限制不适用。

## 1. 先记住三件事

- **地域码一旦发号不能改。** 地域码写在发号计数器的最高位,打散之后的 ID 上看不出来。发号之后改配置,发号器发现记录里的地域和配置不一致,拒绝发号,建角和发物品全部停下。只有值域见底这类事故才换码,按[发号器运维手册](id-issuer-runbook.md) §4.5 换成下一个可认领的码。换码只换发号码(部署参数 `-IdIssuerRegion`);`-Region` 是这个地域存档库的永久编号,永远不变,换了老玩家就存不了盘。
- **地域码不能和别的地域重复。** 两个地域用同一个码,发出来的 ID 会撞,而且撞的时候没有任何报错。2026-11-05 起由程序把关:全局号源登记处里一个码只绑一个部署环境(K8s 集群的身份,data-service 运行期读)和其中的一对本地 etcd。另一套环境用同一个码初始化会被拒绝(`was already claimed`),拿别人的快照克隆过来也发不了号(`not the ones bound`)。2026-11-11 起,同一份清单部署到另一个 K8s 集群、本地 etcd 集群号完全相同,也照样拒绝([发号器运维手册](id-issuer-runbook.md) §1)。§3 的登记表仍然要维护:登记处整个丢了数据时,要按它补回退役码、定下一个可认领的码。
- **用过的地域码不再分配。** 哪怕那个地域已经下线,它发过的 ID 可能还在日志、备份和对账记录里。登记处按从小到大、一个接一个认领:只能认领下一个可认领的码,填大了拒绝(`region codes are claimed in order`),认领过的码(包括退役的)初始化一律拒绝。

## 2. 一个地域要有什么

| 东西 | 说明 |
|---|---|
| 目录 | `<全局库名>_00` 到 `<全局库名>_63`,共 64 片。K 上线后不能改 |
| 流水库 | `<全局库名>_ledger` |
| 业务库 | `mmorpg`(在区域 MySQL)、`mmorpg_guild`、`mmorpg_trade`、`mmorpg_mail`、`mmorpg_match`、`mmorpg_friend`、`mmorpg_player_inbox`、`mmorpg_chat`、`mmorpg_rank` |
| 存档库 | `zone_<n>_db`、`player_store_<id>_db`,由 `MMORPG_DB_ALLOWED_DATABASES` 列出 |
| Redis | 共享实例和玩家域集群,按 [K8s 开服手册](k8s-open-server-runbook.md) 部署 |
| Kafka | 这个地域自己一套,topic 不跨地域 |
| etcd | **两套**:`etcd` 和 `etcd-id-secondary`,发号器的两份本地预留记录分别放这两套,地址不能有交集 |
| 全局号源登记处 | **全世界共用一套**,不是每个地域一套。新地域要能经私网访问它,并有一张世界 CA 签的客户端证书(每个 zone 命名空间一份 Secret `id-registry-client-tls`);部署时用 `-IdRegistryEtcd` 指过去,见[发号器运维手册](id-issuer-runbook.md) §1、§6 |
| 物理机房号 | `-PhysicalRegion`,全局匹配用,是机房编号,和地域码不是一回事 |

每个地域一份角色列表,和现在一个账号多区多角色是同一个模型。**不做全局账号目录**:客户端的地域列表由网关配置给出,玩家在别的地域有没有角色,这边不知道。

名字按地域唯一,同一个名字在两个地域可以各有一个角色。公会、好友、聊天不跨地域。同地域的所有 zone 互打不变。跨地域对战没有确定玩法之前不做,数据不跨地域写。

## 3. 地域码登记表

分配之前先查这张表,分配之后**先改表再部署**,和部署变更一起评审。生产和 staging 用 1 到 63,共用一个登记处,按从小到大分配。

这张表和登记处是两份记录:登记处在发号前把关,这张表是登记处整个丢了数据时的重建依据(每个用过、没重新登记的码都要补墓碑,下一个可认领的码取表里用过的最大码 +1,[发号器运维手册](id-issuer-runbook.md) §4.3)。两边对不上时停下来查。

登记处只能按顺序认领,所以新地域的码就是表里用过的最大码 +1,不要跳号。一个地域换过发号码(§1)时,新码另起一行,在「地域」一栏写明是哪个地域的新发号码。这个地域的 `-Region`(存档库编号)仍是它第一次分配的码,以后部署照传。

| 地域码 | 地域 | 环境 | 启用日期 | 状态 |
|---|---|---|---|---|
| 0 | dev 缺省,不进生产 | dev | - | 保留 |

0 只给 dev、test。`Region` 是可选配置,漏写就是 0,所以 staging、prod 部署拒绝 `-Region 0`,`Mode` 不是 dev、test 的 data_service 也拒绝启动,漏写不会悄悄变成一个地域。

项目还没有上线,生产地域还没有分配。第一个生产地域用 1,之后按顺序分配。

## 4. 步骤

替换全部占位。部署参数的完整说明见 [K8s 开服手册](k8s-open-server-runbook.md),密钥和预置清单也以那边为准。

### 4.1 选地域码

在 §3 的表里选一个没用过的码,登记。发现表和线上对不上时停下来查,不要往下做。

### 4.2 准备这个地域的基础设施和预置

- 新建 TiDB 集群,地址作为这个地域的 `GlobalAuthorityTiDBHost`。不和别的地域共用集群。
- **建库之前**打开 CHECK 约束:`SET GLOBAL tidb_enable_check_constraint = ON;`。`player_route` 的唯一性靠它,TiDB 默认不执行,建完表才打开不一定补上约束。`data_service -migrate` 建表之前查这个开关,没开就拒绝。
- 按开服手册 §1 预置 `domain-service-secrets`,**每个地域自己生成一套密钥**,不从别的地域复制。
- 用世界 CA 给这个地域签一张登记处客户端证书,在这个地域每个 zone 命名空间建 Secret `id-registry-client-tls`([发号器运维手册](id-issuer-runbook.md) §6)。CA 私钥离线保存,不进集群。
- 建片库、流水库、业务库和存档库,给 schema 账号授权(开服手册 §1 的 `render-schema-role`)。boot 名册这时还写不了,它要迁移 Job 种下的 generation,见 §4.3。

### 4.3 部署

`-Region`、`-DirectoryLogicalShards`、`-PlayerStoreShards` 和 `-IdRegistryEtcd` 在 staging、prod 必须显式给,prod 的目录片数只能是 64。这些参数不粘滞,这个地域以后每次部署都要照传同样的值,回滚也一样。托管登记处的那个集群可以用 `-HostIdRegistry` 代替 `-IdRegistryEtcd`。

新地域的存档库是 `player_store_<1000000 + 地域码×1000 + 0..K-1>_db`(K = `-PlayerStoreShards`,初值 64,上线后不能改),部署前在这个地域的 TiDB 上建好空库,`MMORPG_DB_ALLOWED_DATABASES` 恰好列出这 K 个库外加各区主库 `zone_<n>_db`([总体方案 §7.2](../design/player-data-tidb-scaling-plan.md))。

```powershell
$Deploy = @{
  KubeContext = "<这个地域的 context>"
  ReleaseProfile = "prod"
  Region = <地域码>
  IdRegistryEtcd = "<登记处的 host:port>"
  DirectoryLogicalShards = 64
  PlayerStoreShards = 64
  GlobalAuthorityTiDBHost = "<这个地域的 TiDB 入口>"
  PhysicalRegion = <机房编号>
  # 其余参数见 K8s 开服手册 §4
}
# 第一遍只跑迁移 Job,不起写者进程。
& ./tools/scripts/k8s_deploy.ps1 -Command all-up -SqlSchemaOnly @Deploy
# 读 generation、写名册、补全部署目录(K8s 开服手册 §5)之后再跑一遍。data_service 的名册要恰好是片库、流水库和全部存档库。
& ./tools/scripts/k8s_deploy.ps1 -Command all-up @Deploy
```

入口打印一行,核对地域码和库名:

```text
Logical databases: directory_shards=64 directory=mmorpg_global_00..63 ledger=mmorpg_global_ledger id_region=<地域码> ...
```

紧接着还有一行 `Id issuer: region=<地域码> registry=<登记处地址>`。

ConfigMap 里的 `IdSegment.Issuer` 只是模板,常驻进程读的是运维预置的 Secret,所以要核 Pod 里生效的配置。Secret 里的 `IdSegment.Issuer` 要有 `Region`、`RegistryEtcd` 和 `RegistryTLS`。`RegistryEtcd` 等于部署时的 `-IdRegistryEtcd`;`RegistryTLS` 的 `CAFile`、`CertFile`、`KeyFile` 分别是 `/etc/mmorpg/id-registry-tls/` 下的 `ca.crt`、`tls.crt`、`tls.key`。下面两个数都要等于这个地域的码:

```powershell
kubectl --context <context> -n <zone-namespace> exec statefulset/data-service -- grep -n 'Region:' /app/etc/private/data_service.yaml
kubectl --context <context> -n <zone-namespace> exec statefulset/data-service -- printenv MMORPG_ID_ISSUER_EXPECTED_REGION
```

第二个数是部署脚本从 `-Region` 注入的。data_service 启动时比对它和配置,不一致就拒绝启动,所以 Secret 抄错会在这里暴露,不会悄悄发出别的地域的号。

全部 zone 的 data-service 用同一个地域码。每个 zone 部署时传的 `-Region` 都要一样。

### 4.4 初始化发号器

新地域的码还没在登记处认领,发号器拒绝发号,这是刻意的。整个世界第一次部署时,先按[发号器运维手册](id-issuer-runbook.md) §2.1 托管并开启登记处。在第一个 zone 的 data-service 就绪之后、放流量之前做这一步。在任意一个 data-service Pod 里跑一次:

```bash
kubectl -n <zone-namespace> exec statefulset/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-issuer-init
```

mail、rank 不因为它卡启动,它们的发号器预领失败只记日志。没初始化时建角和发号报错,scene 的号段门禁不放行,所以带 `WaitReady` 的部署要另开一个终端跑这一步,或者第一遍不带 `WaitReady`。初始化之后不用重启任何服务。

输出 `region code claimed in the id registry and both reservation records created` 就完成了。码被别的环境认领过、已退役,或者小于登记处里下一个可认领的码,初始化都会拒绝,不写任何东西。**只在全新地域跑。** 已经发过号的地域丢了记录是 ID 安全事件,按[发号器运维手册](id-issuer-runbook.md) §4 用 `-id-issuer-recover` 处理,不要重新初始化。

### 4.5 接入客户端

在网关配置里把这个地域加进客户端的地域列表,域名和入口按开服手册 §4。

## 5. 验收

逐条核对,任何一条不对都不要放量:

1. `kubectl get pods` 全部就绪,data-service 没有 `does not match` 或写资格相关的拒启日志。
2. 建一个角色,按[发号器运维手册](id-issuer-runbook.md) §5 的办法用 `idsegment.UnmixID` 把 player_id 还原成计数器,右移 57 位等于这个地域的地域码。
3. `data_service_id_issuer_reserve_total{result="ok"}` 在增长,`result="refused"` 不增长。
4. 同一个账号在这个地域看到的是空角色列表,在别的地域的角色不出现。
5. 建一个和别的地域重名的角色,能建成。
6. 过户一个角色,`data_service_role_fence_prepare_total{result="clean"}` 在增长,说明协调者连得上这个地域的全部参与者。
7. 告警规则已安装(`deploy/k8s/logical-db-alerts.yaml`、`deploy/k8s/data-service-route-alerts.yaml`),有抓取。

## 6. 配错了怎么办

| 现象 | 含义 | 处置 |
|---|---|---|
| 部署报 `must pass -Region explicitly` | 没给地域码 | 给,不要猜,查 §3 的表 |
| 部署或 data_service 启动报 `reserved for dev` | 地域码是 0,或者配置漏写 | 查 §3 的表,用分配给这个地域的码 |
| 部署报 `must use -DirectoryLogicalShards 64` | prod 的 K 不是 64 | 改成 64 |
| 部署报 `must pass -PlayerStoreShards explicitly` | 没给本地域的存档库数 | 给这个地域定下的 K(初值 64),以后每次部署照传 |
| 部署报 `must list exactly the <K> player store databases of region <r>` | `MMORPG_DB_ALLOWED_DATABASES` 里的存档库和 `-Region`、`-PlayerStoreShards` 推出来的不一致 | 看报错里的 missing / unexpected:少了就补库名(库要先建好),多了一般是地域码或 K 填错,改参数,不要删库 |
| 日志 `belongs to another region` | 记录里的地域和配置不一致 | 配错了 `-Region`。改回去,重新部署。**不要改记录** |
| data_service 启动报 `IdSegment.Issuer.Region` 和 `MMORPG_ID_ISSUER_EXPECTED_REGION` 不一致 | 运维预置的 Secret 里地域码抄错了,或者 `-Region` 传错了 | 查 §3 的表,改错的那一边 |
| 部署报 `must pass -IdRegistryEtcd` | 没给登记处地址 | 给全世界共用的那个登记处地址,托管它的集群可以带 `-HostIdRegistry` |
| 初始化报 `was already claimed` | 这个码被别的环境认领过(包括另一个 K8s 集群里用同一份清单部署的环境),或者小于登记处里下一个可认领的码 | 查 §3 的表,用下一个可认领的码。登记处只认一个环境,另一个环境发不出号,不会撞号 |
| 初始化报 `region codes are claimed in order` | 配的码大于登记处里下一个可认领的码 | 用表里用过的最大码 +1,`-Region` 和 Secret 一起改对。什么都没写 |
| 初始化或发号超时,日志 `authentication handshake failed` | 到登记处的双向 TLS 握手失败:地址不对、证书不是世界 CA 签的、SAN 缺这个地址或证书过期 | 按[发号器运维手册](id-issuer-runbook.md) §3 的同名一行处置,不要改成信任别的 CA |
| 初始化或发号报 `read the deployment identity` | 读不到本 K8s 集群的身份:data-service 的 ServiceAccount 没有 get `namespaces/kube-system`,或者不在 Pod 里执行 | 在 data-service Pod 里执行;按[发号器运维手册](id-issuer-runbook.md) §1 补 RBAC。什么都没写,补好重跑 |
| 发号日志 `not the ones bound to this region code` | 本部署环境(K8s 集群)或这两套 etcd 不是登记处给这个码绑的那一份:重复部署、克隆了别的环境的快照、灾备到了新的 K8s 集群,或者换了 etcd | 重复部署或克隆:这套环境换成下一个可认领的码。灾备或换了 etcd:按[发号器运维手册](id-issuer-runbook.md) §4.2 rebind |
| 发现两个地域在 2026-11-05 之前就用了同一个码,已经发过号 | ID 可能已经撞了(那时还没有登记处把关) | 事故。先停这两个地域的建角,按事故流程处理,再决定怎么办 |

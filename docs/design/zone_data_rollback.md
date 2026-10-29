# Zone 数据回档方案

> **修订(2026-08-15)**: 数据层迁 TiDB 后,灾难恢复级的"MySQL PITR"须改为 TiDB BR/PiTR(runbook 未改写前**不许迁生产**);Phase 2 单一全局库后 zone 粒度回档改由应用级按 home_zone 承担。见 [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) §D8。

## 概述

Zone 数据回档分两个层面：
1. **应用级回档**（RPC/逻辑骨架已实现，生产执行暂停）：通过 `RollbackZone` RPC，从快照恢复玩家数据
2. **灾难恢复级**（Ops 操作）：MySQL PITR + Redis flush + Kafka offset reset（2026-10-18 更正：语义已裁决为「丢弃 TargetTime 之后的写」，不再 Redis flush，Kafka 拨到日志末端，见 §3「2026-10-18 修订」）

> **当前安全状态（2026-08-03）**：应用级 `RollbackPlayer` / `RollbackZone` /
> `RollbackAll` 在生产 `ServiceContext` 中没有可用的跨服务离线 epoch 栅栏，
> 因此会以 `error_code=16 (NotImplemented)` **fail-closed**，不修改任何玩家
> 数据。这不是“先查一次玩家是否在线”能代替的问题。

---

## 1. 单玩家回档（应用级）

详见 [single_player_rollback.md](single_player_rollback.md)。

- 踢玩家下线 → 找目标时间点之前最近的快照 → 调用各服务 `ImportPlayerData()` 恢复
- 自动校验交易日志防止道具复制（有正常转移记录的物品不恢复）
- 本质是**补偿式**回档，不是时间倒流

---

## 2. 整 Zone 应用级回档（`RollbackZone` RPC）

### 必须先闭合的在线写栅栏

当前 Scene 的后续存盘会经 Kafka 进入 `go/db` 并落 MySQL，而
`data_service` 的应用级回档修改 Redis。如果只在回档前查一次
“当前离线”，会同时存在两个窗口：

- 查完后玩家可以重新登录/Scene 激活（TOCTOU）；
- 旧 Scene epoch 在回档前已发出、但尚未落库的存盘，会在回档后
  再次覆盖结果。

因此 `RollbackFence` 的契约必须是原子的跨服务离线 epoch：

1. 阻止目标 player/zone 的新登录和 Scene 激活；
2. 让当前 Scene 下线并排空它已发出的 Kafka/DB 写；
3. 存盘消息携带 epoch，DB 落库端拒绝旧 epoch；
4. 栅栏覆盖“意图审计 → pre-rollback 安全快照 → 恢复 → 结果审计”
   全过程，持久化完成后才释放。

当前仓库尚没有 login / player_locator / Scene / db 共同实现这个协议。
`data_service` 仅保留了可注入的契约接口；生产不注入实现，所以执行路径
默认禁用。单元测试可注入 fake fence，验证缺栅栏、安全快照失败、
审计失败时均不会静默继续。

### 设计决策：Guild/Friend 不回档

**结论：Guild/Friend 数据不需要回档。**

**原因**：
- Player Redis blob（`player:{id}:player_database`）**不包含** guild_id 或 friend 引用
- Guild 通过 `GuildService` gRPC 独立查询，Friend 通过 `FriendService` gRPC 独立查询
- 两个系统的数据完全解耦，回档 player 数据不会与 guild/friend 产生不一致
- 即使存在时间差异（如玩家回档到加入公会前，但 guild_member 表仍有记录），
  这类不一致通过正常的 guild/friend 操作即可自愈（退出重进、重新申请等）

> **2026-09-28 追加(帮会二期 B5d-2b)**:上面"不需要回档、可自愈"只对**关系**数据(成员、好友)成立,
> **不适用于帮会资产**。捐献 / 帮会商店 / 活动发奖是玩家与帮会两边各记一笔的资产操作:回档把玩家的钱包
> 换回快照,帮会资金、帮贡、限购与领奖记录却不在快照里 —— 捐献方向变成资产复制,商店 / 发奖方向变成
> 玩家白亏,这不会自愈。现在三个 `Rollback*` 在写任何玩家数据之前都要过"帮会资产闸":先问 guild 这些玩家
> 自快照以来有没有已应用的资产操作,有就默认拒绝(`ErrCodeRollbackGuildDivergence`,可带
> `accept_guild_divergence` + `x-admin-token` + reason/operator 放行并留逐行日志供人工补偿),问不到就
> 拒绝且不可放行(`ErrCodeRollbackGuildCheckFailed`),写完 10s 后复查一次
> (`ErrCodeRollbackGuildDivergedAfterWrite`)。三个 `Rollback*` 也因此一律要求 `x-admin-token`。
> 设计与运维手册见 [guild-phase2/07-rollback-fail-closed.md](./guild-phase2/07-rollback-fail-closed.md)。

### 无快照角色处理（fail-closed）

`RollbackZone` **不会再自动删除任何无快照角色**。快照只在 GM、事件和回档
安全点产生，仓库内没有周期性快照任务；因此“目标时间前没有快照”不能证明
“角色是在目标时间后创建的”。把两者等同会删除大量正常老玩家。

| 情况 | 处理 |
|------|------|
| T 之前创建的角色 | 从 player_snapshot 恢复数据 ✅ |
| 目标时间前没有快照的角色 | 仅列入 `orphan_player_ids` 候选名单，不恢复、不删除 |
| 候选角色的 Redis / zone mapping | 保持原样 |
| 候选角色的账号 / guild / friend | 保持原样 |

字段名 `orphan_player_ids` 为保持 wire compatibility 暂不改名；它的当前语义是
“需要人工核对的无快照候选”，**不是已确认的新建角色，更不是已清理结果**。
`orphans_cleaned` 恒为 0。若以后恢复自动清理，必须先引入权威角色创建时间，
并走独立审批、pre-delete 快照和可恢复删除流程。

### 执行流程（栅栏落地后，2 阶段）

```
Preflight: 获取跨服务离线 epoch 栅栏 + 写 STARTED 审计
→ 任一前置失败则停止，零玩家数据变更

Phase 1: 恢复 player 数据
────────────────────────
遍历 zone 内所有有快照的 player
→ 对每个 player: 找 target_time 之前最近的 player_snapshot
→ 创建 pre-rollback 安全快照
→ 用 snapshot 数据覆盖 Redis

Phase 2: 报告无快照候选
──────────────────────
SCAN mapping Redis, 找到该 zone 下所有有 zone mapping 的 player
→ 对比 snapshot 中的 player 列表，得到差集（候选）
→ 将候选写入响应，所有数据和映射保持不变

Finalize: 写 RESULT 审计 → 释放 epoch 栅栏
```

### 代码位置

| 文件 | 作用 |
|------|------|
| `go/data_service/internal/logic/rollback_logic.go` | RollbackZone（player 回档 + 无快照候选报告） |
| `go/data_service/internal/routing/router.go` | GetAllPlayerIDsInZone（SCAN mapping Redis） |
| `go/data_service/internal/store/snapshot_store.go` | player_snapshot + rollback_audit_log CRUD |
| `proto/data_service/data_service.proto` | RollbackZone RPC 定义 |

### 跨服务调用链

```
RollbackZone (data_service)
  ├── RollbackFence: 阻断新会话 + 排空/拒绝旧 epoch 存盘（尚未落地）
  ├── Phase 1: 恢复每个 player 的 Redis 数据（从 snapshot）
  └── Phase 2: 返回无快照候选名单（零删除、零账号变更）
```

> **安全边界**：候选名单只能用于人工核对，调用方不得把它解释成“已经清理”。

---

## 3. 整 Zone 灾难恢复级回档（Ops 操作）

### 架构前提

每个 zone 在 K8s 中是独立 namespace（`mmorpg-zone-{zoneName}`），拥有独立的：
- MySQL 实例（数据库名 `game`）
- Redis 实例（`--appendonly yes`）
- Kafka broker / topic（`db_task_topic`）

写路径：`C++ node → Kafka(db_task_topic) → Go db service → MySQL`，Redis 仅为缓存加速层。

### 操作步骤

```
步骤 1: 关闭 zone（停止所有业务写入）
─────────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <zone>

步骤 2: MySQL Point-in-Time Recovery (PITR)
────────────────────────────────────────────
# 先恢复到影子库验证
mysqlbinlog --stop-datetime="2026-03-26 10:00:00" binlog.000xxx | mysql -h shadow-host -u root -p game

# 确认无误后替换正式库
# 方式 A: 云 RDS 控制台一键 PITR
# 方式 B: 自建 MySQL 用 binlog 手动恢复

步骤 3: Redis 处理
──────────────────
# Redis 是缓存层，C++ 节点启动时会从 MySQL 重建，直接清空即可
redis-cli -h <redis-host> FLUSHDB
# ⚠ 2026-10-12 补:K8s 下各 zone 的玩家缓存与 mapping Redis(player:zone / player:placement / merge:*)、
#   go/db 的游标与重试收据同在共享实例的 DB 0,这里的 FLUSHDB 会清掉**所有 zone** 的这些全局键,
#   不只是本 zone 缓存。k8s_zone_rollback.ps1 已加防护:目标 DB 里有这些键就在停服之前拒绝,
#   除非显式传 -ConfirmFlushSharedRedis(dev_tools 为 -RollbackConfirmFlushSharedRedis)。
#   映射丢失后的恢复见 docs/ops/merge-zone-runbook.md §9.2。
# ⚠ 2026-10-18 补(映射持久化第 1b-1 批,代码未编译、未测试):RouteStore.Mode=db_first 时,步骤 2 的 PITR 会连带回档
#   同一 mysql 实例上的全局库(player_route 等路由表),这时 mapping Redis 才是较新的一边 —— **不要**用
#   -ConfirmFlushSharedRedis 放行这里的 FLUSHDB,恢复改走 freeze → reconcile-from-cache → audit -frozen-recovery
#   → unfreeze(`data_service -f <yaml> -route-tool <子命令>`,runbook §9.4)。上一行的 §9.2 手工重放只用于 redis 模式,
#   或库与 Redis 同时丢失。所有档位当前仍是 redis 模式;k8s_zone_rollback.ps1 加入这四步属于第 1b-3 批。
# ⚠ 2026-10-18 更正(第 1b-1 批修复评审):上一条只说了「不要整库 FLUSHDB」,没说本步骤的目的该怎么完成。db_first 下
#   本步骤仍必须清掉**本 zone** 回档点之后的玩家缓存(否则它们活过 PITR,C++ 节点按比 MySQL 更新的缓存继续运行、回写,
#   回档只生效一半),只是要选择性地清:只清本 zone 的玩家缓存(键名以 go/db 与 C++ 的键名函数为准,不按印象拼 pattern),
#   保留 player:zone:* / player:placement:* / player:maprev:* / merge:* / mapping:route:ready、go/db 的 consumer:applied*
#   游标与 kafka:retry* 收据、账号目录等全局键。K8s 上能做这种选择性清理的载体(k8s_zone_rollback.ps1 不整库 FLUSHDB、
#   按本 zone 处置玩家缓存)登记为第 1b-3 批的前置;**在它落地并验证之前,db_first 下 K8s 的 zone 回档不可执行**。
#   详见 runbook §9.4 恢复流程第 0 步的更正注。
# ⚠ 2026-10-18(回档语义裁决,负责人 2026-10-17 选「丢弃 TargetTime 之后的写」):一键脚本 k8s_zone_rollback.ps1 已不做 FLUSHDB,
#   本步骤改由回档工具 zone_rollback_discard 在 Step 4b 只丢弃本 zone 在 T 之后派生的状态(重试收据隔离、受影响玩家的缓存与证据按
#   PITR 之后的库删除,映射 / 游标 / 账号目录一概不碰),见下方「2026-10-18 修订」。工具尚未落地。

# 或从 AOF/RDB 备份恢复到对应时间点（如果有归档的话）

步骤 4: Kafka 处理
──────────────────
# 清空 db_task_topic，防止回档时间点之后的写入消息被重放
# (2026-10-16 更正,下面两行原文不删:方式 A 不适用于现行管线。topic 名是 db_task_zone_<id>[_g<gen>],分区数是数据顺序协议的一部分,
#  取 go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt(现为 10,不是 5);同名删除重建后 offset 从 0 重新开始,go/db 的永久
#  applied cursor(consumer:applied:<topic>:<key>:<msgType>)仍记着旧 offset,新写会被 orderingForTask 判陈旧(同分区)或不可比(分区数变了)。删除重建等同换代,须按
#  db-task-kafka-partition-contract.md「离线扩容流程」执行;一键脚本 k8s_zone_rollback.ps1 的 Step 5 只用方式 B(-ToDatetime))
# (2026-10-17 更正:kafka_offset_reset.ps1 -DeleteAndRecreateTopic 现在对 db_task topic(db_task_zone_<id>[_g<gen>],判据 lib Test-DbTaskTopicName)
#  一律拒绝,要换一个空 topic 只能把 ServerConfig.Kafka.TopicGeneration 加一换代,不提供同名重建的逃生口。
#  第一行「防止回档时间点之后的写入消息被重放」在 K8s 上也不成立,见下方「2026-10-17 修订」的「回档的 Kafka 语义」)
# (2026-10-18 更正:回档语义已由负责人 2026-10-17 裁决为「丢弃 TargetTime 之后的写」,一键脚本 Step 5 改为 -ToLatest(不再 --to-datetime),
#  方式 A、方式 B 都不再是回档的做法;现行流程见下方「2026-10-18 修订」)
# 方式 A: 删除并重建 topic
kafka-topics.sh --delete --topic db_task_topic --bootstrap-server <broker>
kafka-topics.sh --create --topic db_task_topic --partitions 5 --bootstrap-server <broker>

# 方式 B: 重置 consumer group offset 到指定时间戳
# (2026-10-15 更正:组名是本 zone 的 db_rpc_consumer_group_z<id>,topic 是 db_task_zone_<id>,TopicGeneration ≥ 2 时
#  带 _g<gen> 后缀;重置之后还要删掉 legacy 组 db_rpc_consumer_group 在本 topic 上的位点。一键脚本的做法见下方「2026-10-15 修订」)
kafka-consumer-groups.sh --reset-offsets --to-datetime 2026-03-26T10:00:00.000 \
  --group db_rpc_consumer_group_z<id> --topic db_task_zone_<id> --bootstrap-server <broker> --execute

步骤 5: 重新拉起 zone
─────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up \
  -ZoneName <zone> -ZoneId <id> -NodeImage <image> -WaitReady
```

> **(2026-09-29 更正)步骤 5 的命令不完整,且整套手工步骤已由一键脚本取代**:部署参数不粘滞,`k8s-zone-up` 每次按本次参数重新生成,
> 不从集群读回旧值。只写上面这行会把一个以 `-GateRouterMode 0` 回退、以 `-ClientEntryMode external` 对外、经 Ingress 暴露或 staging / prod 档运行的 zone
> 静默重建成默认形态(路由模式 `"1"`、podip、无 Ingress、dev 档)。重新拉起时必须照传上一次部署的同一组参数,清单与预检见下方「2026-09-29 修订」。
> `-NodeImage` 必须是**回滚目标版本**的不可变 tag,不是当前工作树的版本。

### 关键安全点

| 要点 | 说明 |
|------|------|
| Redis 可安全清空 | 它是缓存层，节点启动后从 MySQL 重建 |
| Kafka 必须处理 | 不清空 topic/offset 会导致旧消息重放覆盖回档数据 |
| MySQL 必须先恢复 | 它是唯一的持久化真相源（source of truth） |
| 串行保证 | Kafka partition key = `player_id`，同玩家写入串行执行 |

> **2026-10-17 更正**：「Kafka 必须处理」一行的说法在现行管线上不完整。go/db 有按 topic 的已落库游标，K8s 上它与各 zone 缓存同在共享 Redis DB0、回档不清；重置位点之后哪些写会重放、哪些被吸收，取决于游标还在不在。现状与两个互斥方案见下方「2026-10-17 修订」的「回档的 Kafka 语义」。
>
> **2026-10-18 更正**：负责人 2026-10-17 已裁决方案 (A)「丢弃 TargetTime 之后的写」，上表前两行都不再成立：Redis **不能**整库清空（共享 DB0，回档改由回档工具只丢弃本 zone 在 T 之后派生的状态）；Kafka 不清 topic、不拨回 TargetTime，而是把 zone 组拨到日志末端（`-ToLatest`），并保留游标。现行流程、各状态族的处理与代价见下方「2026-10-18 修订」。

### 2026-09-29 修订:第 0 步预检与不粘滞的部署参数

一键入口是 `tools/scripts/k8s_zone_rollback.ps1`(`dev_tools.ps1 -Command k8s-zone-rollback` 包装,默认 dry-run,`-Apply` 才执行)。
原先的 7 步(zone-down / Kafka drain / MySQL PITR 暂停 / Redis FLUSHDB / offset reset / zone-up / 验证清单)前面**新增了第 0 步**
(脚本头 `.DESCRIPTION`,`k8s_zone_rollback.ps1:15-27`;Step 6 的不粘滞说明在 `:34-48`)。起因:K8s 默认 `-GateRouterMode` 已翻为 `"1"`(turn-based §22 D75),集群外入口参数也不粘滞;
而 Step 1 会删掉整个 zone namespace,`k8s_deploy.ps1` 自己的集群现状预检到 Step 6 面对的是空 namespace,一条都触发不了。所以核对前移到停服之前。

**第 0 步(只读,先于停服;dry-run 只预告不执行),两部分都过才进 Step 1**:

1. **集群现状核对**(`-Apply` 且下列任一项留空时,一次 `kubectl` 读 zone namespace 里的 gate —— StatefulSet 或 Deployment 两种形态都认 —— 与 Ingress `gateway`):
   - `-GateRouterMode` 留空:gate 模板当前的 `GATE_CLIENT_RPC_ROUTER` 必须等于 `k8s_deploy.ps1` 的默认值(按语法树读出,现为 `"1"`)。
     以 `"0"` 回退运行的 zone **必须显式传 `-GateRouterMode 0`**,否则在停服之前被拒(以前会被 Step 6 静默切回 `"1"`,若路由服仍不可用,gate 卡在依赖门,登录 / 匹配全部 `no_target`)。
   - `-ClientEntryMode` 留空:gate 当前形态(StatefulSet = external,Deployment = podip)必须等于默认 `podip`。以 external 运行的 zone 必须显式传 `-ClientEntryMode external`。
   - `-GatewayIngressHost` 与 `-ConfirmNoGatewayIngress` 都没给:namespace 里不能有 Ingress `gateway`(Step 1 会连它一起删、Step 6 留空不重建),且必须读得到。
   - 不一致或判不出(集群不可达、Forbidden、gate 用 valueFrom 注入)一律在停服之前拒绝,要求显式传参;显式传了的项不核对。
   - **上一次回滚中途放弃、namespace 已被删掉时重跑**:一个 gate 都读不到,三项都判为读不到。必须显式传 `-GateRouterMode` 与 `-ClientEntryMode`,
     并对 Ingress 表态:照传 `-GatewayIngressHost`(及 trusted proxies),或加 `-ConfirmNoGatewayIngress`(`dev_tools.ps1` 侧为 `-RollbackConfirmNoGatewayIngress`)。
   - 地址参数与 `-RequireClientEndpoint` **不逐项核对**:预检只把 gate StatefulSet 现有的地址 env 与 Ingress host 打印出来,供操作者对照部署记录。
2. **静态预检**:把 Step 6 将要传给 `dev_tools.ps1 k8s-zone-up` 的同一份参数加 `-DryRun` 执行一遍(`k8s_deploy.ps1 -DryRun` 只本地渲染、不调 kubectl),非 0 即拒绝。
   参数组合、档位与密钥、发布门禁的规则只在 `k8s_deploy.ps1` 一处,这里不复制;拼错或矛盾的组合在停服之前暴露。要求 PATH 上有 git;staging / prod 档工作树脏会在这里被拒(与 Step 6 一致,只是提前)。

**回滚时必须照传的参数(与该 zone 上一次部署一致)**:

| 场景 | 参数 |
|---|---|
| 以 `"0"` 回退运行 | `-GateRouterMode 0`(回退态不粘滞,见 `k8s_deploy.ps1:157-160` 参数注释;D-12 修订的"回退"条款) |
| 以 external 对外 | `-ClientEntryMode external`、`-GateServiceType`、`-GateNodePortBase`、`-ClientPublicHost` 或 `-GateClientHostTemplate`、`-GateExternalTrafficPolicy`;逐个 zone 切 external 的窗口里还有 `-RequireClientEndpoint` |
| 经 Ingress 对外 | `-GatewayIngressHost`、`-GatewayTrustedProxies`(及 `-GatewayIngressClassName` / `-GatewayIngressTlsSecret`) |
| staging / prod | `-ReleaseProfile`(**留空 = 按 dev 档部署**:login 以 dev 模式、密钥回落占位值拉起,安全方向 fail-open);部署 Java 服务时还需环境变量 `MMORPG_GATEWAY_ADMIN_API_KEY`(Step 6 子进程继承);staging / prod 还需要 `MMORPG_LOGIN_QUEUE_TOKEN_SECRET`(排队缺省开;见 `deploy/k8s/README.md`)(2026-10-15 更正:登录排队合入门槛未清时各档排队都关,不要求该变量;见 `deploy/k8s/README.md`「登录排队的合入门槛」) |
| dev 依赖开发口令登录 | `-LoginDevPasswordAuth`(照常透传到 Step 6,由 `k8s_deploy.ps1` 的 dev 档门禁把关;漏传则 login 没有任何认证配置,Step 7 冒烟登录失败) |
| 已启用微信 / QQ 登录 | 按上次部署照传 `-LoginWeChatAppId` / `-LoginWeChatEndpoint` / `-LoginWeChatAllowOpenId`、`-LoginQQAppId` / `-LoginQQEndpoint` / `-LoginQQAllowOpenId`;微信还需部署机环境变量 `MMORPG_LOGIN_WECHAT_APP_SECRET`。`-Apply` 下第 0 步在停服之前核对 login ConfigMap 与 Secret,会关闭或改动已启用配置、或现状判不出时拒绝;确要变更加 `-ConfirmLoginAuthProviderChange`。存档重跑与确认开关的边界见 [微信 / QQ 沙盒手册](../ops/wechat-qq-sandbox-runbook.md) §B |
| 指定集群 | `-KubeContext` / `-KubeConfig`:第 0 步、Step 1、Step 6 用同一集群。`dev_tools.ps1` 的回滚入口以前静默丢掉它们、回滚落到 kubectl 当前 context,现已转发 |

- `-AllowDisruptiveSwitch` 只转给 Step 6,**不豁免第 0 步**;它只在 namespace 没删净、`k8s_deploy.ps1` 的集群现状预检真的触发时起作用。
- `dev_tools.ps1` 的回滚入口不转发 `-BattleOrchestrator`(zone 回滚不碰 battle 全局池);`dev_tools.ps1:1592-1617` 列出了全部转发项。
- `k8s-release-*` 也接受 `-RequireClientEndpoint`,但经那条路只参与预检、不改写 login / scene_manager 的 ConfigMap。
- 命令示例见 `k8s_zone_rollback.ps1` 头部 `.EXAMPLE`(含 `"0"` 回退与 external + Ingress 的 staging / prod 两例)。

**仍未解决(已登记为独立任务,本批不做)**:Step 6 的 `GoSvcTag` / `JavaSvcTag` 取当前工作树的 sha,不是回滚目标版本;`OpsProfile`、`GateReplicas`、
`SceneReplicas` 等不透传;`-ReleaseProfile` 留空仍按 dev 档部署,只有摘要行提醒,没有与集群核对。

**验证状态**:以上全部**未运行**(未编译、未测试,待 Codex 验证)。Codex 在仓库根执行
`pwsh -NoProfile -File tools/scripts/tests/k8s_zone_rollback_gate_router_mode.tests.ps1`(需要 PATH 上有 git),再跑 `k8s_deploy_contract.tests.ps1`、
`k8s_client_entry_contract.tests.ps1`、`dev_tools_merge_zone_contract.tests.ps1`,通过标准 `fail=0`,失败时保留全部 `[FAIL]` 行。
DryRun 冒烟:`pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-rollback -ZoneName e2e -ZoneId 101 -RollbackTargetTime 2026-05-15T14:23:00Z -NodeImage registry.invalid/test/mmorpg-node:0123456789ab -KubeContext kind-mmorpg -LoginDevPasswordAuth`,
期望 Step 1、预检 kubectl、Step 6 三行都带上 context,Step 6 还带 `-LoginDevPasswordAuth`。

### 2026-10-15 修订:存盘消费组按 zone 拆分后的 Kafka 步骤(删 Step 2、第 0 步 c / d、Step 5 退役 legacy 位点、Step 6 落到 final(Z))

**状态:已落码,未编译、未测试,待 Codex 验证。** 本节按 2026-10-16 工作区的代码写(`tools/scripts/k8s_zone_rollback.ps1`、`tools/scripts/kafka_offset_reset.ps1`、
`tools/scripts/lib/kafka_consumer_groups.ps1`、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/dev_tools.ps1`),与规格不一致处以代码为准,见本节末尾。
设计见 [db-consumer-group-per-zone.md](./db-consumer-group-per-zone.md)(§5 收尾、§6 回档、§11 未核实项)。上文「2026-09-29 修订」里「原先的 7 步」
含 Kafka drain、第 0 步只有两部分,这两处以本节为准(脚本头 `.DESCRIPTION` 已同步改写)。
(2026-10-18 补:本节里第 0 步 d 与 Step 5 的 `-ToDatetime <TargetTime>`、「Step 5 本来就把位点拨回 TargetTime」以及「Redis 已清」的说法,在负责人裁决之后都已改变:
两处一律 `-ToLatest`,回档不再 FLUSHDB。现行流程以下方「2026-10-18 修订」为准;本节其余内容(组名推导、退役 legacy 位点、Step 6 落到 final(Z))仍然有效。)

起因:go/db 的存盘消费组从 K8s 上全 zone 共用的 `db_rpc_consumer_group`(下称 legacy 组)逐 zone 换成 `db_rpc_consumer_group_z<ZoneId>`(下称 zone 组)。
旧脚本 Step 5 重置的是 legacy 组:只要别的 zone 在线,这个组就不是 Empty,重置一定被 Kafka 拒绝。旧 Step 2 在 zone-down 之后等 LAG 排空,
可本 zone 的 db 已经停了,必然等满超时再 `proceeding anyway`。

**组名与分区数**:脚本入口用 `Get-YamlScalar` 现读 `go/db/etc/db.yaml` 的 `ServerConfig.Kafka.GroupID`(基名)与 `ServerConfig.Kafka.PartitionCnt`(必须是正整数),
读不到即拒绝,此时尚未产生任何改动。`-KafkaGroup` 留空推导为 `<基名>_z<ZoneId>`;新增 `-KafkaLegacyGroup`,留空推导为 `<基名>`;两者相同(区分大小写比较)入口即拒。
摘要行 `kafka` 一栏变为 `<bootstrap> topic=<topic> group=<zone 组> retire=<legacy 组> partitions=<N>`。
(2026-10-17 补,R7 复核 scripts [6]:topic 不再只按 `-ZoneId` 拼。入口读完 `PartitionCnt` 之后再读 `ServerConfig.Kafka.TopicGeneration`,必须是非负整数,读不到即拒;用 lib 的 `Get-DbTaskTopicName` 推导,世代 ≤1 为 `db_task_zone_<id>`,否则 `db_task_zone_<id>_g<gen>`。`-KafkaTopic` 留空即用推导值;显式给了却与推导值不同(区分大小写),入口即拒,此时尚未产生任何改动。理由:Step 6 的 `k8s_deploy.ps1` 只按 db.yaml 的世代号读 topic,第 0 步与 Step 5 改的若是另一个 topic,真实 topic 上的位点不会动,全程不报错。`dev_tools.ps1` 的 `-RollbackKafkaTopic` 缺省为空串。)

**第 0 步由两部分扩为 a–d**(上文「第 0 步」的 1 / 2 即脚本里的 a / b;2026-10-17 起再加 e)。a、b、d、e 都过才进 Step 1,c 只告警:

- **c. Kafka LAG 只读快照**(`Show-KafkaTopicLagSnapshot`):只在 `-Apply` 下执行,dry-run 只打印两条 `kafka-consumer-groups.sh --describe --group <组>` 命令。
  - 对 zone 组与 legacy 组各 describe 一次,只打印本 topic 的分区 / current / end / lag 与 LAG 合计,别的 topic 的行不计入;组不存在时打印 `(group does not exist)`。
  - 任何失败只 `Write-Warning`,绝不拒绝。它取代原 Step 2,用途只是停服前看一眼两组在本 topic 上的进度。
- **d. Step 5 预演**(`Invoke-Step5Rehearsal`):只在 `-Apply` 下、Step 1 之前执行。
  - 先 describe zone 组。它在本 topic 上已有数值位点就跳过,打印 `zone group already holds offsets on <topic>; Step 5 rehearsal skipped`。
  - 组不存在或没有位点(从未切过组的 zone)时,以 hashtable splatting 调 `kafka_offset_reset.ps1 -BootstrapServer … -Topic … -Group <zone 组> -ToDatetime <TargetTime> -Partitions <N>`,
    即 dry-run,不带 `-Apply` 与 `-RetireGroup`。计划不完整、抛错或退出码非 0,都在停服之前拒绝。
  - describe 本身失败同样拒绝:判不出就无法证明 Step 5 走得通。
  - 已知局限(未处理):zone 组在本 topic 上没有已提交位点、却仍有活跃成员时(例如以 final(Z) 运行、但从未写入过的 zone),dry-run 重置会因组不是 Empty 被 Kafka 拒绝,回档在停服前被拒。
- **e. Step 6 的 Kafka 读法预检**(`Assert-Step6KafkaFactsReadable`,2026-10-17,R7 复核 scripts [2]):只在 `-Apply` 下、第 0 步 d 之后、Step 1 之前执行;dry-run 只打印两条将要执行的 `kubectl … exec -n <infra ns> kafka-0 -c kafka -- /bin/sh -c "… kafka-consumer-groups.sh --bootstrap-server localhost:9092 --describe --group <组>"`。
  - 起因:Step 6 的 `k8s_deploy.ps1` 在写路径入口经 `kubectl exec` 进 kafka-0 读两组位点,读不到就拒绝部署;以前第 0 步只用本机 CLI,部署账号缺 `pods/exec`、kafka-0 正在重启或容器名不对时,各项预检全过,直到 Step 6 才失败。
  - 与 Step 6 走同一条路径:lib 的同一份命令与分类(`Get-DbGroupDescribeExecArgs` / `Resolve-DbGroupDescribeExecResult`)、同一个 kubectl 出口(`Invoke-ClientEntryKubectl`)、同一组 `-KubeContext` / `-KubeConfig`;infra namespace 按语法树读 `dev_tools.ps1` 的 `-InfraNamespace` 默认值(Step 6 不传它,用的就是这个,现为 `mmorpg-infra`),读不出即拒绝。
  - 对 zone 组与 legacy 组各 exec 一次,exec 失败或输出判不出即在停服之前拒绝;再逐组与本机 CLI(`-KafkaBootstrap`)比对「本 topic 上有没有已提交位点」,两边结论不同(多半是两者指向了不同的 Kafka)同样拒绝。只比有无、不比位点值:停服之前 db 还在消费。本机 describe 失败也拒绝。
- c / d / e 直接调用 PATH 上的 `kafka-consumer-groups.sh`(本脚本没有 `-KafkaBin`),执行机必须装有 Kafka CLI 且连得到 `-KafkaBootstrap`;e 另外要求部署账号对 infra namespace 的 kafka-0 有 `pods/exec` 权限。

**Step 2 已删除**,编号 3–7 不变。`k8s_zone_rollback.ps1` 的 `-KafkaDrainTimeoutSec` 与 `dev_tools.ps1` 的 `-RollbackKafkaDrainTimeoutSec` 一并删除。
理由:Step 5 本来就把位点拨回 TargetTime,排空没有意义。

**Step 5:重置 zone 组,复核通过后退役 legacy 组在本 topic 上的位点。** 脚本以 hashtable splatting 调用
`kafka_offset_reset.ps1 -BootstrapServer <b> -Topic db_task_zone_<id> -Group db_rpc_consumer_group_z<id> -RetireGroup db_rpc_consumer_group -ToDatetime <TargetTime> -Partitions <N> [-Apply]`:

1. 解析 `--reset-offsets` 输出的 GROUP / TOPIC / PARTITION / NEW-OFFSET 计划表(`ConvertFrom-KafkaResetPlan`)。计划必须恰好覆盖 0..N-1,NEW-OFFSET 都是数值;dry-run 与 `-Apply` 都检查。
2. `-Apply` 时,`--execute` 之后再 describe zone 组,逐分区断言 CURRENT-OFFSET 等于计划的 NEW-OFFSET;不符即抛出「重置后复核失败,没有删除 db_rpc_consumer_group 的位点」。
   这个抛出在删除之前。
3. 复核通过才执行 `Remove-RetiredGroupTopicOffsets`:
   - legacy 组在本 topic 上没有数值位点时打印 `nothing to retire`(幂等,可重跑);
   - 否则执行 `kafka-consumer-groups.sh --delete-offsets --group db_rpc_consumer_group --topic db_task_zone_<id>`,输出含 `GROUP_SUBSCRIBED_TO_TOPIC` 即抛出;删完再 describe 一次,仍有数值位点也抛出。
   - 回档是有意回退,这条路不做「新组 ≥ 旧组」检查(那是 `-FinalizeHandoff` 的前置条件)。

- legacy 组即使还有别的 zone 的成员,只要没有成员订阅本 topic,broker 就允许删除。这一点是推断,待 Codex 在钉版的 Kafka 4.3.1 上实测(设计文档 §11 V3(d))。
- 仍有订阅者时(本 zone 的 namespace 没删干净、留有 db 实例),Step 5 失败。这时 zone 已停、MySQL 已回档、Redis 已清:清掉残留实例后,**单独重跑脚本打印的那条
  `kafka_offset_reset.ps1` 命令**(带 `-Apply`;重跑时旧组若已没有本 topic 的位点,退役一步打印 `nothing to retire`),再单独跑 Step 6。
- `-RetireGroup` 必须配 `-Partitions > 0`,不能等于 `-Group`,不能与 `-DeleteAndRecreateTopic` 同用,违反任一条即在入口拒绝。
- (2026-10-17 补,R7 复核 scripts [1] / [5])`kafka_offset_reset.ps1` 的其余参数约束与退出码:
  - `-DeleteAndRecreateTopic` 对 db_task topic(`db_task_zone_<id>` 或带 `_g<gen>`,lib `Test-DbTaskTopicName`)一律拒绝,改走 `TopicGeneration` 换代;对其它 topic 必须显式给 `-ReplicationFactor`(缺省 0 = 没给)且等于现有副本数,删除前先 describe 核对分区数与副本数,读不到或不一致都不删;删除后最多等 30s(单调时钟)确认 topic 已从 `--list` 消失,`--list` 失败或超时即抛出、不 create;create 失败或重建后复核不对,提示先停生产者再手工重建。细节见设计文档 §5。
  - 重置模式的成功路径以 `exit 0` 结束,`Invoke-KafkaToolCaptured` 在 finally 里把 `LASTEXITCODE` 复位为 0。以前「组不存在」的 describe 以非 0 退出、被判为 missing 后正常返回,Step 5 会在 PITR / FLUSHDB 之后把成功判成失败。

**Step 6:显式 `-DbConsumerGroup zone`,真跑时落到最终形状 final(Z)。** `$upArgs.DbConsumerGroup = 'zone'`,第 0 步 b 的静态预检用的是同一份参数。

- 真跑时,`k8s_deploy.ps1` 在写路径入口(`Initialize-DbConsumerGroupPlans`)kubectl exec 进 infra namespace 的 `kafka-0`,读两组在本 topic 上的位点(`Read-DbGroupTopicFacts`)。
  命令经 `/bin/sh -c "KAFKA_HEAP_OPTS='-Xms16m -Xmx64m' /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 --describe --group <组>"` 执行,
  避免 CLI 继承 broker 的大堆参数、把 kafka-0 撑到 OOMKilled。
- Step 5 之后 zone 组有位点、legacy 组在本 topic 上没有,`Resolve-DbConsumerGroupPlan` 落到 `Shape=final`、`Source=explicit-already-authoritative`:
  db ConfigMap 只写 `GroupID: "db_rpc_consumer_group_z<id>"`,不带 `GroupHandoffFrom`,交接闸之前的旧镜像也能起。
  db Pod 模板注解 `mmorpg.io/db-consumer-group` 为 zone 组,`mmorpg.io/db-group-handoff-from` 为 `none`。
- 第 0 步 b 的静态预检带 `-DryRun`、不读集群,渲染的是交接形状的预览(`ConsumerGroup` + `GroupHandoffFrom`,`Source=dryrun-preview`,另有一行告警)。它只校验参数组合,不代表真跑的形状。
- **回档之后,该 zone 一律以 zone 组为权威**:仍在 legacy 组上的 zone 借回档迁到 zone 组。这是行为变化。
- 剩余风险:Kafka 位点直到 Step 6 才读,第 0 步不读。部署账号缺 `pods/exec` 权限或 kafka-0 未就绪时,`k8s_deploy.ps1` 会在任何写操作之前拒绝,但这时 zone 已停服、数据已回档;
  修好之后,按脚本打印的 Step 6 那一行单独重跑 `dev_tools.ps1 -Command k8s-zone-up … -DbConsumerGroup zone`。
  (2026-10-17 更正:这条已在停服前覆盖。第 0 步 e 用 Step 6 的同一条路径预读两组位点,缺 `pods/exec`、kafka-0 未就绪都在 Step 1 之前拒绝。剩下的只是第 0 步 e 与 Step 6 之间的窗口:中间隔着 MySQL PITR,kafka-0 在这段时间里重启仍会让 Step 6 拒绝,处置同上。)
- Step 6 渲染的是最终形状(`Source=explicit-already-authoritative`),不触发 `k8s_deploy.ps1` 2026-10-17 新增的交接闸能力校验(那一项只在要渲染交接形状时才抓 db Pod 的 `/metrics`),所以回档不需要 `pods/proxy` 权限。
- Step 5 的退役没做成(两组在本 topic 上都有位点)时,显式 zone 模式同样拒绝,并打印两种 `-FinalizeHandoff` 收尾命令。回档场景下应重跑 Step 5,不要照那条提示收尾。
- Step 6 必须部署 db:`dev_tools.ps1` 的 `-GoSvcRegistry` 缺省非空。显式 `-DbConsumerGroup` 配上不部署 db 的运行(没给 `-GoSvcRegistry`,或带了 `-SkipGoSvc`)时,`k8s_deploy.ps1` 在写路径入口拒绝。

**`dev_tools.ps1` 入口**:
- `-RollbackKafkaGroup` 缺省改为空串(由回滚脚本推导);新增 `-RollbackKafkaLegacyGroup`。两者都只在非空时转发。
- `k8s-zone-rollback` 显式给了 `-DbConsumerGroup` 就在调用回滚脚本之前拒绝,因为 Step 6 固定用 zone。
- `kafka-offset-reset` 分支改为 hashtable splatting,新增 `-KafkaRetireGroup` / `-KafkaFinalizeHandoff` 透传(都要配 `-KafkaPartitions > 0`)。

**与规格的差异(以代码为准)**:
- 原规格写「新增脚本常量 `$DbConsumerGroupBase`」;代码改为入口现读 db.yaml,没有这个常量。
- 原规格写 Step 6 渲染 zone 的交接形状(`ConsumerGroup` + `GroupHandoffFrom`);代码按修订稿渲染最终形状 final(Z),只有 `-DryRun` 预览才是交接形状。
- 原规格只有第 0 步 c;代码按修订稿另加第 0 步 d(Step 5 预演),并要求 `-Partitions` 参与计划覆盖与 `-Apply` 后复核。
- 原规格风险 8 写「漏删 legacy 位点时由 go/db 的 R7 拦住」;final(Z) 下 go/db 不配 `GroupHandoffFrom`、没有交接闸,实际兜底是 `k8s_deploy.ps1` 显式模式的「两组都有位点即拒绝」。

**未核实项**(待 Codex 在 `apache/kafka:4.3.1` 上实测,设计文档 §11):
- 对不存在的组执行 `--reset-offsets --to-datetime --execute` 能否建出位点(V3(c));
- 组 Stable、但无人订阅本 topic 时 `--delete-offsets` 能否成功(V3(d));
- 不存在组 / Empty 组 describe 的原文,以及 `--execute` 是否打印 NEW-OFFSET 表(V3(b)(f)(h)(i))。

脚本按推定的文案 fail-closed。若 V3(c) 证实建不出位点,回档改动不得发布,交负责人决策。

**Codex 验证**(Claude 未执行):在仓库根逐个执行 `pwsh -NoProfile -NonInteractive -File tools/scripts/tests/<名>.tests.ps1`,名字为
`k8s_zone_rollback_gate_router_mode`、`k8s_zone_rollback_flush_guard`、`kafka_offset_reset_output_guard`、`dev_tools_kafka_offset_reset`、`dev_tools_merge_zone_contract`、
`k8s_deploy_contract`、`k8s_client_entry_contract`,通过标准 `fail=0`,失败时保留全部 `[FAIL]` 行。dry-run 冒烟:
`pwsh -NoProfile -File tools/scripts/k8s_zone_rollback.ps1 -ZoneName t1 -ZoneId 101 -TargetTime 2026-05-15T14:23:00Z -NodeImage ghcr.io/luyuancpp/mmorpg-node:0123456789ab -SkipMySqlPause`,
期望摘要含 `group=db_rpc_consumer_group_z101 retire=db_rpc_consumer_group partitions=10`,Step 5 行含 `-RetireGroup db_rpc_consumer_group` 与 `-Partitions 10`,
Step 6 行含 `-DbConsumerGroup zone`,输出里没有 Step 2 的排空等待。

### 2026-10-17 修订:R7 复核后的脚本修复与回档的 Kafka 语义(已于 2026-10-17 裁决,落码见下方「2026-10-18 修订」)

**状态:已落码,未编译、未测试,待 Codex 验证。** 本节按 2026-10-17 工作区的 `tools/scripts/k8s_zone_rollback.ps1`、`tools/scripts/kafka_offset_reset.ps1`、`tools/scripts/lib/kafka_consumer_groups.ps1`、
`tools/scripts/dev_tools.ps1` 写;回档语义一项当时只改了注释,行为没有改。设计细节见 [db-consumer-group-per-zone.md](./db-consumer-group-per-zone.md) §5、§6。
(2026-10-18 补:下面「回档的 Kafka 语义」一段描述的是**裁决之前**的代码,留作历史;负责人 2026-10-17 选了方案 (A),现行流程以「2026-10-18 修订」为准。)

**已并入上文「2026-10-15 修订」的修复**(那一节里标了「2026-10-17」):
- 第 0 步 e `Assert-Step6KafkaFactsReadable`:停服前按 Step 6 的同一条 `kubectl exec` 路径预读两组位点,并与本机 CLI 比对有无位点(R7 [2]);上文「剩余风险:Kafka 位点直到 Step 6 才读」因此已在停服前覆盖。
- topic 按 `ServerConfig.Kafka.TopicGeneration` 推导,显式 `-KafkaTopic` 与推导值不一致即拒(R7 [6])。
- `kafka_offset_reset.ps1`:`-DeleteAndRecreateTopic` 对 db_task topic 一律拒绝、`-ReplicationFactor` 必填且须与现有一致、删除等待超时即抛出(R7 [1]);重置模式以 `exit 0` 结束、`LASTEXITCODE` 复位(R7 [5])。上文步骤 4 的「方式 A」对 db_task 已不可用,要换空 topic 只能换代。

**回档的 Kafka 语义(R7 复核 scripts [4];负责人 2026-10-17 已裁决 (A) 丢弃 TargetTime 之后的写,下面是裁决之前的现状与两案,留作历史)**

现状(按代码):
- 本文步骤 4 写「清空 db_task_topic,防止回档时间点之后的写入消息被重放」,「关键安全点」写「不清空 topic/offset 会导致旧消息重放覆盖回档数据」,脚本头 Step 5 写「avoids replaying post-target writes」。三处都默认「拨回位点 = 从 TargetTime 起重放,且重放会覆盖回档数据」,在现行管线上都不准确。
- go/db 写任务有按 topic 的已落库游标 `consumer:applied:<topic>:<key>:<msgType>` 与归属 epoch `consumer:applied_epoch:<topic>:<key>:<msgType>`(`go/db/internal/kafka/key_ordered_consumer.go` 的 `appliedSeqKey` / `appliedEpochKey`),seq 不大于游标的写判 stale,不落库。
- K8s 上游标与各 zone 的缓存、映射、重试收据同在共享 Redis DB0。Step 4 的 FLUSHDB 防护(`Get-SharedRedisGuardPatterns` 含 `consumer:applied*`、`kafka:retry*`)在这个库上必拒,能走通的只有两种:不给 `-RedisHost`,Step 4 被跳过,游标不清;带 `-ConfirmFlushSharedRedis`,连所有 zone 的游标、映射与重试收据一起清。
- 游标还在时,Step 5 把 zone 组拨回 TargetTime 之后:
  - TargetTime 之后已经落过库的消息被游标判 stale 吸收,不写 MySQL,所以「从 TargetTime 起重放」在 K8s 上不成立;
  - zone-down 时还没消费的积压,seq 大于游标,照常落库;
  - 共享 DB0 里本 topic 的重试收据(`kafka:retry:queue:<topic>`、`kafka:retry:queue:<topic>:p<N>`、`kafka:retry:processing:<topic>:<实例>` 与旧版的 `kafka:retry:processing:<topic>`)从未推进过游标,Step 6 之后照常重试落库;
  - 后两类是 TargetTime 之后的写,会落进 PITR 之后的 MySQL,部分玩家回到回档前的状态,所以「丢弃」也不完整。
- 游标被清时(本机单库,或带 `-ConfirmFlushSharedRedis`),没有游标的写按 `apply_no_cursor` 全部重新落库(`db_ordering_guard_total{outcome="apply_no_cursor"}`),TargetTime 之后的写全部重放,回档等于没做。

两个方案(互斥,修法与代价):

| | (A) 丢弃 TargetTime 之后的写 | (B) 从 TargetTime 起灾难恢复重放 |
|---|---|---|
| 做法 | Step 5 改为 `-ToLatest`;在 Step 4 / 5 之间盘点并拦截本 topic 的重试收据(上面列的 ready / 按分区 ready / processing 键) | Step 5 之前只清本 topic 的游标:`SCAN` 匹配 `consumer:applied:<topic>:*` 与 `consumer:applied_epoch:<topic>:*`,逐页 `UNLINK`;第 0 步加一条校验:TargetTime 不早于各分区的日志起点,否则重放不全。**现在没有这条校验** |
| 结果 | MySQL 停在 PITR 的 TargetTime | MySQL 先回到 TargetTime,再由 Kafka 追平到 zone-down 时的状态 |
| 适用 | 撤销 TargetTime 之后的错误写(误操作、刷资源、坏版本) | MySQL 丢数据或损坏,要从 Kafka 补回 |
| 代价 | 要新写盘点与拦截收据的工具,并与 go/db 的重试键(`retry_routing.go` / `retry_ownership.go`)和 `go/db/cmd/retry_requeue` 口径一致;zone-down 前最后一批存盘一起丢,回档点完全由 PITR 决定 | 撤销不了错误写,重放会把它们再写一遍;要在共享 DB0 上按 topic 批量删键,pattern 里 topic 之后必须带冒号、topic 先做 glob 转义,逐页执行、任一页出错即停;重放量受保留期与 PVC 限制;「不早于日志起点」的判据还要设计 |

两个方案都要另外核实的一点(推断,没有核对过):`--to-datetime` 按消息时间戳取位点,时间戳早于 TargetTime、但 TargetTime 那一刻还没被消费的积压,既不在 PITR 之后的 MySQL 里,也不在拨回之后的重放范围内。

裁决之前,按现有代码执行回档得到的是上面「现状」的混合语义。(2026-10-18:已裁决并落码,见下方「2026-10-18 修订」;上面「两案都要另外核实的一点」在 (A) 下不再相关 —— Step 5 不再按时间戳取位点。)

**Codex 验证**(Claude 未执行):在仓库根逐个执行 `pwsh -NoProfile -File tools/scripts/tests/<名>.tests.ps1`,名字为 `kafka_offset_reset_output_guard`、`dev_tools_kafka_offset_reset`、
`k8s_zone_rollback_gate_router_mode`(需要 PATH 上有 git)、`k8s_zone_rollback_flush_guard`、`k8s_deploy_contract`、`k8s_migrate_gate`、`k8s_client_entry_contract`,通过标准 `fail=0`,
失败时保留全部 `[FAIL]` 行。

### 2026-10-18 修订:回档语义已裁决 —— 丢弃 TargetTime 之后的写

**状态:已落码(脚本、契约与契约测试),未编译、未测试,待 Codex 验证。** 以 2026-10-18 工作区的 `tools/scripts/k8s_zone_rollback.ps1`(文件头 `.DESCRIPTION` 与「zone_rollback_discard 契约」两段为准)、
`tools/scripts/lib/kafka_consumer_groups.ps1`、`tools/scripts/kafka_offset_reset.ps1`、`tools/scripts/dev_tools.ps1`、`tools/scripts/k8s_deploy.ps1`(只移走一个函数)与
`tools/scripts/tests/` 下的 `k8s_zone_rollback_discard` / `k8s_zone_rollback_gate_router_mode` 两套测试及沙箱 `tests/lib/k8s_zone_rollback_sandbox.ps1` 写;本节与代码不一致时以代码为准。
读 MySQL、写 Redis 的回档工具 `go/db/cmd/zone_rollback_discard` **尚未落地**:在它落地之前 `-Apply` 停在入口校验(要求 `-RollbackDiscardTool`),落地之后 preflight
在 zone 维护围栏(待裁决 #1)落地之前仍以 3 拒绝。**结论:K8s 上的整 zone 回档目前只能 dry-run,`-Apply` 不可执行。** Java 版:不适用(Java 版没有 Kafka 存盘链,同 db-consumer-group-per-zone.md §12)。

**裁决**:负责人 2026-10-17(原话选项「丢弃 T 之后的写」):回档就是「回到 TargetTime 时刻」—— 回到 T 那一刻 MySQL 里已经落库的状态。落法:Step 5 改用 `-ToLatest`
(不再 `--to-datetime`);Step 4 / 5 之间盘点并拦下本 topic 的重试收据;不清游标。
**代价(已接受)**:T 之后的合法存盘一并丢弃,包括 T 之前产生、T 那一刻还在 Kafka 里没落库的存盘(玩家看到的回退点比 T 早一个消费延迟);[T, 停服] 之间的登录、交易、
资产变化对本 zone 的玩家档一律作废。跨服务状态(guild / mail / 收件箱 / trade / data_service 全局库)不随 zone 回退,与以前相同。

**为什么不再 FLUSHDB**:K8s 与本机的 go/db 游标、重试收据、归属映射、账号目录与各 zone 的玩家缓存同在一个 Redis DB。整库清空会清掉所有 zone 的这些键,游标没了之后
重放全部按 `apply_no_cursor` 重新落库,等于事故。`-ConfirmFlushSharedRedis` / `-RedisPassword`(及 dev_tools 的 `-RollbackConfirmFlushSharedRedis` / `-RollbackRedisPassword`)改为入口拒绝。
**为什么不清游标**:go/db 的 seq 是消费时算的 offset + 1。拨到日志末端之后,新消息的 offset 不小于切点 O,旧游标不大于 O,新写恒大于旧游标,不会被误判陈旧;
保留游标还能在以后误投递时吸收重放。前提「offset 单调、不回退」由 5b 的 verify 复核(各分区最大 seq 不大于切点)。**以后对 db_task topic 做 `-ToDatetime` / `-ToEarliest`
都不得越过本次的切点**(`run/logs/zone-rollback/<rid>/kafka-cut.json`),越过即把停服时那批积压重新落库;`kafka_offset_reset.ps1` 目前不读切点,只能靠文档与操作者核对。

**改后的流程**(`-Apply` 才执行;dry-run 照样做入口校验,其余只打印命令):

| 步 | 做什么 | 拒绝时的状态与出路 |
|---|---|---|
| 入口 E1–E4 | TargetTime 只收早于当前的 UTC `yyyy-MM-ddTHH:mm:ss[.fff]Z`,`-PitrRestoredTo` 须等于它;`-Apply` 必须显式给 `-RedisHost`、`-MySqlDsnEnv`(变量非空)、`-RollbackDiscardTool`、`-BarrierProtocol legacy`,`-SkipMySqlPause` 必须配 `-PitrRestoredTo`;口令与 DSN 只给环境变量名;`-BarrierProtocol strict`(#3)与 `-AcceptBlockedPlayers`(#4)入口即拒;生成 rollback-id 与报告目录 `run/logs/zone-rollback/<rid>/` | 什么都没动 |
| 0 a | 留空的 `-GateRouterMode` / `-ClientEntryMode` / Ingress 与集群现状核对。第 0 步全部通过后把 Step 6 的入口决定与读到的集群现状写进 `entry-state.json`;同一 rid 重跑、namespace 已删时沿用存档核对,入口决定与存档不同即拒。(2026-10-22 补,B0-4:`-NodeOpenFileLimitMode` 留空时同一步另核对 gate / scene(含 Agones Fleet)模板里的 `NODE_OPEN_FILE_LIMIT_MODE`,任一为 `warn` 或读不到即拒;现状随集群现状存档(Schema 3),不进入口决定) | 停服之前 |
| 0 b | Step 6 的同一份参数加 `-DryRun` 交给 k8s_deploy 静态预检 | 停服之前 |
| 0 c | 两组在本 topic 上的 LAG 只读快照(只告警) | — |
| 0 d | zone 组没有位点时以 dry-run 预演 Step 5(`-ToLatest`) | 停服之前 |
| 0 e | 按 Step 6 的路径(kubectl exec 进 kafka-0)读两组位点,与本机 CLI 比对 | 停服之前 |
| 0 f | 部署事实(go-svc-db-config / data-service 的非机密字段,存 `deploy-facts.json`):ZoneId / 世代 / 分区数等于本机 db.yaml;db 消费组按 lib `ConvertFrom-LiveDbConsumerGroup` 归形状,只放行 final(L)、final(Z)、handoff(Z←L)、handoff(L←Z);Redis 拓扑同实例同 DB;Fencing.Mode=legacy。随后 kubectl exec 进 infra 的 redis-0 对 go/db 的 Redis 地址读 `INFO server` 的 run_id(之后每次工具调用带 `-expect-run-id`);`<tool> -version` 的构建提交必须以线上 db 镜像 tag 开头且不是脏树;最后 `<tool> -mode preflight` | 停服之前 |
| 1 | `k8s-zone-down`,再确认 namespace 已不存在 | 尚未 PITR;同一 rid 重跑或放弃 |
| 1b | (i) zone 组 `--state` 为 Empty / Dead / 不存在,legacy 组本 topic 无成员;(ii) 隔 `-QuiesceSeconds`(≥300s)两次读日志末端必须相同;(iii) `<tool> -mode stopped`:租约过期、没有人在别处在线,最后一步写停服行指纹基线 `fingerprint-baseline.json`(phase=stopped);(iv) 两组 LAG 与将丢弃的积压条数(zone 组有位点按 zone 组,否则按 legacy 组) | 尚未 PITR,Redis / Kafka 未动;放弃 = 不带 `-DbConsumerGroup` 的 zone-up |
| 3 | 人工 PITR:只回 `zone_<id>_db`(及工具列出的落点库),先恢复到影子库再替换,原库改名保留到 Step 6 冒烟通过;不回全局库 | — |
| 4a | `<tool> -mode inventory`:与停服基线比较行指纹(没有任何玩家变化 = 没做 PITR / 恢复到影子库没换回 / 连错库,拒绝,除非 `-AcknowledgeNoPostTWrites`),归类、blocked 即拒(#4)。通过后再复核一次停服状态:namespace 仍不存在、zone 组仍不活跃、legacy 组本 topic 仍无成员 | Kafka / Redis 未动;中止 = 换回原库 → 不跑 5 / 4b → **不带 `-DbConsumerGroup`** 的 zone-up |
| 5 | zone 组拨到日志末端(`kafka_offset_reset.ps1 -ToLatest`),逐分区复核后删除 legacy 组在本 topic 上的位点;读 zone 组位点作为切点 O,存 `kafka-cut.json` | 从这里起只能向前,按打印的单步命令用同一 rid 重跑 |
| 4b | `<tool> -mode purge -confirm`(与 4a 同一组判定参数):重试收据与死信改名进隔离区,受影响玩家的缓存、证据与屏障按 PITR 之后的库删除 | 只能向前,同一 rid 续跑 |
| 5b | zone 组每个分区 CURRENT == LOG-END == O;`<tool> -mode verify`(同一组判定参数 + `-purge-report`) | Step 6 之前拒绝;查清残留写者后单独重跑 Step 5 / 4b / 5b |
| 6 | `k8s-zone-up -DbConsumerGroup zone`:Step 5 之后只有 zone 组有位点,渲染最终形状 final(Z) | 只重跑 Step 6 |
| 7 | 人工核对清单(purge.json 列为已处理的玩家首登前没有缓存;重试族由 5b 的 verify 核对过,zone 起来之后不再拿 verify 复核) | — |

**各状态族的处理**:

| 状态族 | 处理 | 在哪一步 |
|---|---|---|
| [T, 停服] 之间以及停服时的 Kafka 积压 | 不落库:zone 组拨到日志末端;legacy 组在本 topic 上的位点退役 | 5 |
| go/db 游标 `consumer:applied*` / `consumer:applied_epoch*`、归属计数器 owner_epoch | 保留不动;verify 复核各分区最大 seq 不大于切点 | 5b |
| 本 topic 的重试收据(共享 ready、按分区 ready `:p<n>`、processing、实例登记)与死信 | `RENAMENX` 进 `rollback:quarantine:<zone>:<rid>:<pass>:<原键>`(与 `PEXPIRE` 同一段 Lua),guard 键 `UNLINK`;隔离区永不回注线上队列 | 4b |
| 重试租约 `kafka:retry:instances` | stopped 等到没有未过期租约;verify 确认没有存活租约 | 1b / 5b |
| 受影响玩家的父快照 `PlayerAllData:<pid>`、子表缓存、sync_digest、资产提交证据、屏障 | 每玩家一段 Lua(白名单:home = 本 zone 且落点 = zone_<id>_db,原子复核映射与落点)按 PITR 之后的库删除;legacy 协议下绝不写 `save_barrier`;verify 逐个复核仍不存在 | 4b / 5b |
| 64 个补投桶 | `ZREM` 本人的成员;verify 确认为空 | 4b / 5b |
| 映射(player:zone / placement / maprev)、`account:*`、`merge:*` | 不碰 | — |
| 本 zone 归属、在别的 zone 在线的玩家 | stopped 拒绝(陈旧 location 按 merge_zone 判据剔除,handoff 另等 300s) | 1b |
| blocked 玩家(torn / owner_behind / post_t_candidate / legacy_owned / unclassified) | 4a 拒绝,待裁决 #4 | 4a |
| T 之后合入本 zone 的玩家(`merge:merged_into:*` 的 dst = 本 zone) | preflight 拒绝,待裁决 #6 | 0 f |
| MySQL 全局库(data_service 的 id_segment / player_name / 路由表、guild、mail、收件箱、trade) | 不回退(回退 id_segment 会重复发号) | 3 |

**本轮回档复核(R7 语义复核之后的对抗复核,2026-10-18)的处置**,全部成立并已落码:

| # | 问题 | 处置 |
|---|---|---|
| [0] | 第 0 步 f 拿线上 `GroupID` 原值比本机基名,final(Z) 与交接形状的 zone(包括回档过一次的)一律被拒,文案还让人改 db.yaml | `ConvertFrom-LiveDbConsumerGroup` 从 k8s_deploy.ps1 移到 lib 共用;deploy-facts 记 `KafkaGroup = {Shape, Group, HandoffFrom}`(Schema 2),只放行四种合法形状,拒绝文案改为「线上形状不属于 {L, Z}」 |
| [1] | 行指纹基线在停服前取,go/db 仍在落库,没做 PITR 或连错库也会被判「有变化」 | 基线挪到 Step 1b 的 stopped,且 stopped 排在 Kafka 两项之后、最后一步才写;preflight 不再写基线;同一 rid 已有 stopped 基线不覆盖,非 stopped 阶段的基线以 3 拒绝 |
| [2] | purge 不带 `-target-time` / `-acknowledge-no-post-t-writes`,带了 `-AcknowledgeNoPostTWrites` 的路径上 Step 5 之后 purge 必然退出 3 | inventory / purge / verify 共用一份判定参数 `$judgeArgs`,打印的单步命令用同一份 argv |
| [3] | 同一 rid 续跑与隔离区冲突规则自相矛盾,第二次 4b 必然撞上上一轮的目标键 | 契约写明续跑规则(清单的 rid / zone / topic / 世代一致即属于本次);隔离键名带轮次 `<pass>`,每次 purge 开新一轮 |
| [4] | `-RedisHost` 与 `-KubeContext` 指向的集群没有绑定,另一个环境同 ZoneId 的 Redis 也能过 | 第 0 步 f 经 kubectl exec 读 go/db Redis 的 run_id,工具每个模式按 `-expect-run-id` 核对 |
| [5] | 「工具与线上 go/db 同一提交」只写在注释里,没有核对 | 第 0 步 f 执行 `<tool> -version`,提交必须以线上 db 镜像 tag 开头且两边都不是脏树;digest 部署无从核对即拒;preflight 另行复核 |
| [6] | 5b 只查 Kafka 与重试族,4b 之后写进 Redis 的 T 之后状态发现不了 | verify 逐个复核 purge.json 里处理过的玩家的父快照、子表缓存、sync_digest、资产证据与屏障,并重跑盘点归类 |
| [7] | 契约漏了把删除范围限定在本 topic 的规则 | 契约写明:键名一律出自 go/db 的导出函数;MATCH = escapeRedisGlob(前缀) + ':' / ':p' 边界 + '*';SCAN 到 cursor 0;任一页出错即停、以 1 退出并给出分族计数;purge.json 记分族计数;点名工具测试用例 |
| [8] | PITR 之后到 Step 5 之间不再复核停服状态 | 4a 通过之后、Step 5 之前再执行 namespace 检查与两组消费者检查,不过即按中止路径拒绝 |
| [9] | 中止路径用 Step 6 的 `-DbConsumerGroup zone`,legacy 上的 zone 会被渲染成交接形状、可能起不来 | 中止 / 放弃路径单独组装一份去掉 `DbConsumerGroup` 的 zone-up 参数(keep) |
| [10] | Step 1 之后整脚本重跑只能凭记忆传入口参数 | 入口决定与集群现状存 `entry-state.json`,重跑时沿用并逐项比较 |
| [11] | (iv) 只打印 zone 组 LAG;Step 7 清单口径不对;一处注释仍写 FLUSHDB;dev_tools 不透传两个开关 | (iv) 两组都打印并按口径估算丢弃条数;Step 7 改为「purge.json 列为已处理的玩家」与「重试族由 5b verify 核对」;删掉 `kafka_offset_reset.ps1` 末尾注释里的 FLUSHDB;dev_tools 新增 `-RollbackAllowEmptyTarget` / `-RollbackAcknowledgeNoPostTWrites` |

**残余风险**:
1. 全部是静态结论:没有编译、没有运行任何测试;Kafka CLI 行为(db-consumer-group-per-zone.md §11,其中 V3(c) 现在对应「对不存在的组 `--reset-offsets --to-latest --execute` 能否建出位点」)未在钉版 4.3.1 上实测。
2. 回档工具未落地:契约只写在脚本文件头,工具落地时以它为准、改一边必须同步另一边;沙箱里的假工具只验证脚本的编排与参数,不验证工具的语义。
3. verify 覆盖不到的写:Redis 里不属于盘点归类族的键、5b 的 verify 结束到 Step 6 拉起之间的秒级窗口里写进 Redis 的状态。5b 之后到 Step 6 之间没有再复核。
4. 停服复核只读一次:4a 之后的复核与 Step 5 之间仍有秒级窗口;PITR 期间 namespace 被重建并已写库时,只能按中止路径恢复。
5. Redis 实例身份依赖集群内 infra 的 redis-0(镜像带 redis-cli、Pod 有 `REDIS_PASSWORD`)能连到 go/db 的 Redis 地址;go/db 用集群外托管 Redis、口令不同或 redis-0 不可用时第 0 步 f 拒绝(fail-closed),需要另行设计。回档中途发生主从切换,run_id 变了,工具以 3 拒绝,用同一 rid 重跑。
6. 版本核对要求线上 db 镜像 tag 是 git sha 且干净构建:digest 部署、`-dirty` tag 一律拒绝(dev 档的脏树镜像同样无法回档),没有人工豁免开关。
7. 切点约束只靠文档(见上)。
8. `-QuiesceSeconds` 下限 300s 来自 librdkafka `message.timeout.ms` 缺省值;C++ 生产者以后改了这个值,下限要同步调。
9. 入口决定存档之后,同一 rid 不能换入口;确要换,先人工核对部署记录,再删掉 `entry-state.json`。
10. db_first 路由存储下 PITR 若连带回退同一实例上的全局库,见上方步骤 3 的 2026-10-18 补注(另一批次);本方案只回 `zone_<id>_db`、不碰映射。

**需负责人另行拍板**(脚本对这些路径一律 fail-closed):
- **#1 zone 维护围栏**:回档期间阻断本 zone 归属玩家在任何 zone 登录、激活的围栏。落地之前工具 preflight 必须以 3 拒绝,也就是工具落地之后 `-Apply` 仍走不通。
- **#3 严格屏障协议(`-BarrierProtocol strict`)**:严格提交屏障批次落地之后,工具成为 `save_barrier` 的新写入方,屏障按 PITR 之后的库重建的形状,以及「有 pending、无 ACK、SQL 围栏 (0,0)」这类玩家怎么处置。
- **#4 blocked 玩家**:torn / owner_behind / post_t_candidate / legacy_owned / unclassified 以及 T 之后建角、迁入的玩家,缓存与证据是隔离保留还是删除,T 之后建的角色能否以空骨架继续使用;裁决前 `-AcceptBlockedPlayers` 入口即拒。
- **#6 T 之后合入本 zone 的玩家**:按现有判据会被当成 T 之后建角而清掉,裁决前 preflight 拒绝。
- 回档工具 `go/db/cmd/zone_rollback_discard` 的落地批次。

**Codex 验证**(Claude 未执行;仓库根,PowerShell 7):
- 逐个执行 `pwsh -NoProfile -NonInteractive -File tools/scripts/tests/<名>.tests.ps1`,名字为 `k8s_zone_rollback_discard`、`k8s_zone_rollback_gate_router_mode`(需要 PATH 上有 git)、
  `kafka_offset_reset_output_guard`、`dev_tools_kafka_offset_reset`、`k8s_deploy_contract`(本批把 `ConvertFrom-LiveDbConsumerGroup` 移进 lib,§③ 节的用例经 lib 取它)、
  `k8s_client_entry_contract`、`k8s_migrate_gate`、`dev_tools_merge_zone_contract`。通过标准:每个以 0 退出、`fail=0`;失败时保留全部 `[FAIL]` 行。
  `k8s_zone_rollback_flush_guard.tests.ps1` 已删除(由 `k8s_zone_rollback_discard.tests.ps1` 取代),不要再找它。
- dry-run 冒烟:`pwsh -NoProfile -File tools/scripts/k8s_zone_rollback.ps1 -ZoneName t1 -ZoneId 101 -TargetTime 2026-05-15T14:23:00Z -NodeImage ghcr.io/luyuancpp/mmorpg-node:0123456789ab -SkipMySqlPause`,
  期望退出码 0;Step 5 行含 `-ToLatest True`、不含 `ToDatetime`;输出里有 `Redis identity (read-only): kubectl exec -n mmorpg-infra pod/redis-0`、`Discard tool build:`、
  五个模式的工具命令都带 `-expect-run-id`,inventory / purge / verify 都带 `-target-time 2026-05-15T14:23:00Z -fingerprint-baseline`;没有 `FLUSHDB`。

---

## 3. 当前缺失基建

| 缺失项 | 状态 | 说明 | 优先级 |
|--------|------|------|--------|
| MySQL 定时备份 + binlog 归档 | ✅ **已落地 (2026-05-15)** | `deploy/k8s/manifests/infra/mysql.yaml`(PVC + binlog on)+ `mysql-backup-cronjob.yaml`(每天 03:17 UTC dump + binlog 复制),SOP 见 [`docs/ops/mysql-backup-pitr-runbook.md`](../ops/mysql-backup-pitr-runbook.md) | ~~高~~ |
| Redis RDB/AOF 定期快照归档 | 未做 | 当前 `--appendonly yes` 保证持久化，但无历史时间点恢复能力。**2026-10-18 更正**：回档不再 FLUSHDB（共享 DB0），改由回档工具只丢弃本 zone 在 TargetTime 之后派生的状态，见上文「2026-10-18 修订」 | 低 |
| Kafka offset 回档工具 | ✅ **已落地 (2026-05-15)** | `tools/scripts/kafka_offset_reset.ps1`(支持 to-datetime / to-earliest / to-latest / delete-and-recreate-topic 4 种模式,默认 dry-run),注册为 `dev_tools.ps1 -Command kafka-offset-reset`。**2026-10-17 更正**:delete-and-recreate-topic 对 db_task topic 一律拒绝(改走 TopicGeneration 换代),对其它 topic 要求 `-ReplicationFactor` 与现有一致;另有 2026-10-15 起的 FinalizeHandoff 模式,见上文「2026-10-17 修订」(已落码,未编译、未测试,待 Codex 验证) | ~~中~~ |
| 一键 zone 回档脚本 | ✅ **已落地 (2026-05-15)** | `tools/scripts/k8s_zone_rollback.ps1`(7 步:zone-down / Kafka drain / MySQL PITR 提示暂停 / Redis FLUSHDB / kafka-offset-reset / zone-up / 验证清单),注册为 `dev_tools.ps1 -Command k8s-zone-rollback`,默认 dry-run。**2026-09-29 更正**:现为「第 0 步预检 + 7 步」,见上文「## 3. 整 Zone 灾难恢复级回档」下的「2026-09-29 修订:第 0 步预检与不粘滞的部署参数」(本文有两个「## 3.」标题,指的是前一个)。**2026-10-15 更正**:Step 2(Kafka drain)已删除,第 0 步增加 c(LAG 只读快照)/ d(Step 5 预演),Step 5 重置 zone 组 `db_rpc_consumer_group_z<id>` 并退役 legacy 组在本 topic 上的位点,Step 6 落到 final(Z),见同处「2026-10-15 修订」(已落码,未编译、未测试,待 Codex 验证)。**2026-10-18 更正**:回档语义已裁决为「丢弃 TargetTime 之后的写」,流程改为 第 0 步 a–f → 1 → 1b → 3 PITR → 4a → 5(`-ToLatest`)→ 4b → 5b → 6 → 7,不再 FLUSHDB,读 MySQL / 写 Redis 交给尚未落地的回档工具 `go/db/cmd/zone_rollback_discard`,见上文「2026-10-18 修订」(已落码,未编译、未测试,待 Codex 验证;工具落地之前 `-Apply` 不可执行) | ~~中~~ |
| 备份异地归档(S3/OSS) | 未做 | PVC 只防 pod 重启,不防整个集群 / 机房挂 | 高(生产前必做) |
| 跨服务离线 epoch 回档栅栏 | 未做（应用级回档 fail-closed） | login/player_locator 阻断新会话，Scene 排空存盘，Kafka/db 携带并校验 epoch | P0（重启应用级回档前） |

---

## 4. 生产环境建议

- **云 MySQL（RDS）**：自带自动备份和 PITR，回档操作在控制台点几下即可，**强烈推荐**
- **自建 MySQL**：必须配置 `log-bin`、定期 `mysqldump` CronJob、binlog 归档到对象存储
- **Redis**：zone 回档时直接 FLUSHDB，无需额外备份
  - 2026-10-18 更正：不成立。K8s 与本机的 Redis 都是共享库，回档不做 FLUSHDB，改由回档工具只丢弃本 zone 在 TargetTime 之后派生的状态，见上文「2026-10-18 修订」。
- **Kafka**：回档后最安全的做法是清空 `db_task_topic`，避免旧消息重放
  - 2026-10-17 更正：现行管线不能清空或同名重建 db_task topic（go/db 的已落库游标按 topic 名区分、永不过期，同名重建后的新存盘会被判陈旧丢弃；`kafka_offset_reset.ps1 -DeleteAndRecreateTopic` 已拒绝），回档只拨位点；拨位点之后哪些写会重放，见上文「2026-10-17 修订」的「回档的 Kafka 语义」。
  - 2026-10-18 更正：负责人 2026-10-17 已裁决「丢弃 TargetTime 之后的写」，回档把 zone 组拨到日志末端（`-ToLatest`）、保留游标，见上文「2026-10-18 修订」。

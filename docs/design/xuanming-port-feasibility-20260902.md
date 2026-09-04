# XuanMing-Server → mmorpg/go 移植可行性定谳（2026-09-02）

> 范围：`F:\XuanMing-Server`（下称 **A**，Kratos + go.work，Pandora/玄冥 UE5 后端）→ `D:\luyuan\mmorpg\go`（下称 **B**，go-zero + C++ 场景服）。
> 用户三问：① A 是不是没有单点？② 除 DS 外全搬可以吗？③ B 原来有的模块不搬可以吗？
> 方法：8 维度调查 → 30 组件逐个评估（+ 补评 pkg/config）→ 17 组件对抗证伪 → 3 个独立验证者复核单点清单 → 基于完整输入的跨组件批判（§4.3）→ 每条待拍板问题一份 ADR + 反方挑战（§8）→ 首两批文件级执行清单（§9）。所有结论均附 `file:line`，审计原始产物见 §11。
> **状态：分析阶段，未搬任何代码。** 开工前需用户确认 §10 检查清单。

---

## 0. 结论先行

| 问题 | 答案 |
|---|---|
| ① A 没有单点？ | **不成立，且最硬的单点在应用层。** `deploy/k8s/infra/` 那批 replicas:1 是 **dev-only**（`overlays/online/kustomization.yaml:20-26` 不引用 infra/，`tools/scripts/tests/infra_durability_contract_test.ps1:52-56` 机械断言），生产基础设施拓扑**仓库内未找到**。真实单点见 §2。 |
| ② 除 DS 全搬？ | **不可以。** 四条模型互斥（线协议 / 玩家权威 / proto 工具链 / DDL 纪律），30 个组件里能原样搬的文件合计 **< 700 行**；A 的 `pkg/` 大半是 B 的旧副本（文件头自陈「移植自 mmorpg」），搬回去是回流不是移植。 |
| ③ 已有的不搬？ | **可以，而且必须。** 按此收窄后，真正要做的是「B 没有的 7 个域 + 4 个形态不同的域」，且它们**共用一个前置缺口**：B 的 Go 侧没有「幂等发物入包 / 发经验」接口（§4.1）。 |

---

## 1. 两边的硬事实

| | A（XuanMing） | B（mmorpg） |
|---|---|---|
| 框架 | Kratos v2，单一 `go.work`（35 个 use） | go-zero `zrpc.MustNewServer`，**无 go.work**，`go/` 下 10 个独立 go.mod（全仓 14 个）+ `replace ../proto ../shared`；依赖栈内部两两分叉（go-zero 1.9.2/1.10.0、etcd 3.5.15/3.6.5、sarama/kafka-go、go-zero redis/go-redis v9） |
| Go | 1.26.5 | 1.24.5（仅 db 1.26.5）；CI builder `golang:1.24-alpine` 无 GOTOOLCHAIN |
| 客户端线协议 | Envoy gRPC-Web + jwt_authn，32 条 `/pandora` 路由 | muduo TCP，`len+nameLen+typeName+payload+adler32`，客户端消息 ≤1KB，`proto/message_id.txt` 158 条数字号 |
| 玩家权威 | `services/runtime/owner`：单行 CAS、单调 owner_epoch、两阶段准入、生产强制 TiDB | C++ scene ECS 内存 → `PlayerAllData` 整包写 Redis → Kafka DBTask → go/db REPLACE INTO |
| proto | `pandora/` import 根，**0 个 go_package**（靠 buf managed），ErrCode 枚举 | `proto/` 根，裸 protoc 无 buf，TipInfoMessage；proto2mysql 扩展号 500001/2/6/11/12 与 B `OptionTableName` **同号同 extendee**（同一二进制 protoregistry panic） |
| 错误码 | `pkg/errcode` 481 行手写常量，services 侧 304 个文件 import | 配表生成 Tip 0..129 扁平轴 + Go 私有段 200-259，客户端查文案 |
| 配置表 | proto 为源（`excel_col` 只支持标量）| xlsx 为源，出 Go/C++/Java；item/mission/reward/condition/skill **五表同名不同 schema** |
| DDL | 96 个 golang-migrate SQL 跨 10 库，带外 Job fail-closed | proto2mysql 反射自动建表 + 启动内联 AutoMigrate（`db.go:51` 自陈会自作主张 MODIFY COLUMN） |
| snowflake | Epoch 1781161165 | Epoch 1773446400，与 `cpp snow_flake.h` 绑死；A 的 chat/mail sweep 用 `MinIDAt` 按 epoch 范围删 |
| pkg/config | 集中式 Base + 自定义 `Duration`(Int64 kind) + `omitempty` | 每服务自带 Config；go-zero 只认精确 `time.Duration`，`omitempty` ≠ `,optional`（缺键报 field not set） |

---

## 2. 单点定谳（经 3 个独立验证者复核）

**口径纠正**：29 条初审清单里 7 条（etcd/MySQL/Redis/Kafka/TiDB/edge-envoy/观测栈）证据全指向 `deploy/k8s/infra/`，那是本地 minikube 清单。线上形态本仓无法证实——**既不能说有，也不能说没有**。

**落在生产路径上的真实单点：**

| # | 单点 | 证据 | 玩家侧表现 |
|---|---|---|---|
| 1 | 19/22 业务 Deployment 线上 replicas:1 | `overlays/online/kustomization.yaml:59-62` 只抬 login=2、player-locator=2；`services.yaml` 只有 :727 ds-allocator=2；全集群仅 1 个 PDB；线上 >1 的三个服务里两个零反亲和 | owner 挂=进场 WAIT 全服；hub-allocator 挂=进不了大厅；matchmaker 挂=撮合停 |
| 2 | matchmaker 单写者选举**全环境关闭** | `matchmaker-dev.yaml:152-160` leader 整段注释；pve 连注释都没有；生成器不注入；Deployment 无 strategy | 稳态单点；滚更窗口跨键 saga 交错（单键有 WATCH/CAS） |
| 3 | mail 游标水位只在单发号器内成立 | `mail/cmd/mail/main.go:88-96` 自陈；无机械门禁 | 滚更即触发，玩家永久漏收邮件 |
| 4 | snowflake nodeID 作用域 | `ProvideSnowflakeN` 同进程 n 个 Node 共用 nodeID → **逐位相同**（`provider.go:58-77`）；etcd key 按服务名隔离刻意跨服务复用，只靠 `gen_cluster_config.ps1:1612-1615` 一张 3 条手工 override 表 | instance_id/match_id 重号 |
| 5 | cellroute 分片全环境关闭 | yaml 零命中 | 所有存储单点无横向切分 |
| 6 | **pandora-envoy DS 面网关** :8444 replicas:1 无 PDB（初审漏项） | 所有 DS 回连唯一一跳 | 挂 >20s 全服在场对局被 DS 自我 fencing 踢光 |
| 7 | **pandora-hub-stable Fleet replicas:1**（初审漏项） | 容量 500，无 autoscaler | 整个大厅一个 UE 进程 |
| 8 | 单 Redis `--maxmemory 1gb --maxmemory-policy noeviction`（初审漏项） | 探针只 `redis-cli ping` | 写面全死、探针恒绿 |
| 9 | ds-allocator 2 副本对 etcd 故障同源失效 | `fence.Lost()` 同刻 `os.Exit(1)` | 伪高可用 |
| 10 | killswitch 文件烘进镜像、无挂卷 | k8s 里根本改不了；etcd 源零 blank import | 想关某 RPC 关不了 |
| 11 | 逻辑单点 | account 库全服单 schema；owner 库要求 TiDB（MySQL 异步复制切换会回滚已确认写→双 owner） | 与副本数无关 |
| 12 | fail-closed 契约 | login/player_locator/ds_allocator/hub_allocator/battle_result 失租即 `os.Exit(1)`；13 个发号服务 Acquire 失败即退出 | etcd 不可用=五条链全域拒服 |

另有：DSTicket keyset 单 revision immutable 不可轮换；alloy 采集器单副本 Deployment 非 DaemonSet；etcd key 前缀缺 `<env>` 段（文档规定有）。

---

## 3. 逐组件去留表（31 项）

桶：**缺** = B 完全没有；**壳** = B 只有 proto 桩 / C++ 有码零调用点；**有** = B 有完整实现。结论以对抗验证后的 corrected verdict 为准。

### 3.1 pkg 层（9 项）

| 组件 | A 行数 | 桶 | 结论 | 落点 / 只搬什么 | 人日 |
|---|---|---|---|---|---|
| pkg/configtable + tools/configtable-gen | 7984+2824 | 有（A 自陈移植自 B） | 不搬 | 只按 B 的 manifest 重做**加载端校验**（sha256/整批原子/版本单调）→ `go/shared/configload/` | 12 |
| pkg/auth + passwd + internalrpcauth + sessiongate | 2502 | 有 | 不搬 | 只吃 internalrpcauth 的 Redis ReplayStore + 每 caller 独立密钥 → `go/login/.../callerauth/`（补 B 自己写下的缺口 `callerauth.go:52-56`） | 6 |
| pkg/kafkax | 1364 | 有（A 是 B 的削薄副本） | 不搬 | 只回流 5 个 Kafka 指标 + 消费循环指数退避 → `go/db/internal/kafka` | 3 |
| pkg/middleware + grpcserver + grpcclient + transport + svc + grpcstats + metrics + log | 2463 | 有（17/19 文件 import Kratos） | 不搬 | 只搬 `log/window.go`（75 行）→ `go/shared/logwindow/` | 2 |
| pkg/mysqlx + dbguard + redisx + redislock + cache | 1766 | 部分缺 | 改造 | 直搬 `mysqlx/schema.go`、`backend_check.go`、`redisx/ratelimit.go`；dbguard 重做为 `go/shared/dbcapacity/`（不能叫 dbguard，撞 `go/db/internal/dbguard`）；sharded.go 不搬（与 B 的 TiDB 决策相反）；cache/redislock 是回流件且 B 版更新 | 9 |
| pkg/snowflake + placement + cellroute + leader + killswitch | 2562 | 有（B 全面更强） | 不搬 | 补丁回流 `GenerateInto`/`MinIDAt`/`ProvideSnowflakeN`/txn 自认领；cellroute 条件执行（与 B 的 home_zone 目录路由同职责，先拍板） | 6 |
| pkg/namecheck + playerdisplay + errcode + safego + version + releasetrack + rating + rewardclaim + offlinewatch 等 | 3728 | 混合 | 改造 | version/releasetrack/rating/rewardclaim/offlinewatch.classify 可直搬但 **B 侧零调用点**；namecheck 是唯一「A 留、B 退役」的组件（B 的 C++ 过滤器生产零调用点且非 AC 自动机）；errcode/safego 不搬 | 11 |
| pkg/dsauthfence + dsauthrecord + dsmetadata + battleabort | 4524 | DS 专属 | 跳过 | **D15 已判：writerlease 也不引入**。B 内单写者循环统一 `shared/leader`；mail 漏收的真因是「每 RPC 现取号 + 读侧按 max id 推水位」，修法是发号与提交同事务，与选举无关 | 0 |
| pkg/config | 639 | B 纪律不同 | 改造 | `go/shared/<connconf>/` 薄包只放 MySQL/Redis/Kafka 三个连接配置（`time.Duration` + go-zero tag）；其余每服务内联；`Duration`/`omitempty` 全量换 | 2 |

### 3.2 services 层（21 项）

| 组件 | A 行数 | 桶 | 结论 | 落点 / 只搬什么 | 人日 |
|---|---|---|---|---|---|
| login | 9368 | 有（B 两步登录已完整） | 不搬 | 只加 `login_ratelimit.go` + `redisx/ratelimit.go` | 12 |
| player | 5802 | 有（C++ ECS） | 不搬 | 等级/属性/技能 B 留；A 独有的经验曲线、昵称改名、领奖位图若要做另立项 | — |
| friend | 2491 | 有 | 不搬 | 四块增量：黑名单 / 推荐 / 终态请求 GC / per-player 配额 → 现有 `go/friend` | 18 |
| chat | 1568 | **壳**（proto + message_id 61/28 已烧进 C++，服务端 0 行） | 改造 | 新建 `go/chat`；**协议留 B**（A 的 ChatMessage 字段 1 语义正撞）；扇出改 `gate-{id}`；TEAM/GROUP 频道等 team 服务 | 20 |
| guild | 3784 | 有（role 编码正好反向！） | 不搬 | 五块增量：申请审批 / 踢人 / 转让 / 任命 / 群聊 → 现有 `go/guild` | 28 |
| mail | 2153 | **缺**（B 文档自己预留了独立 Go mail） | 改造 | 新建 `go/mail`；**P0 前置：发物接口**（§4.1）；游标水位单写者问题不能原样带入 | 12 |
| mission | 2742 | 有 C++ 引擎（A 的 Go 引擎本来就移植自 B），但**引擎跑不起来**：无怪物实体、无持久化、发奖 handler 空 | **不搬 + 整体封存**（D14 翻案） | A 的 Go mission 不搬（三条 P0 成立）；B 的 C++ mission 保留不动（零生产者，无副作用）；设三道重开闸后再立项 | 1（封存）|
| dialogue | 942 | **缺**（B NOTES #269 自列未建） | 改造 | 新建 `go/dialogue`；`session.go`/`tree.go` 168 行直搬；软前置 NPC 实体（B 的 `Npc{}` 是空 message） | 12 |
| team | 6518 | **缺**（B 的 go/match 是排队凑单，`party_member_ids` 标注「二期」等 team 喂名单） | 改造 | 新建 `go/team`；Redis 状态机 2940 行可改造；推送/身份/三个出站解析器重写 | 35 |
| matchmaker | 8547 | 有（`go/match` 2927 行已接线到 C++ battle） | 不搬 | 只借 MMR 动态窗口 + 确认期状态机 → `go/match/internal/logic/` | 18 |
| inventory | 8778 | C++ 域（bag 3188 行有码零调用点不持久化；**currency 已接线不可动**） | **不搬核心** | 用户已定：背包核心权威留 C++。只从 A 提取「幂等发物入包」的**接口规格**给 §4.1 用 | — |
| auction | 4536 | **缺** | 重写 | 新建 `go/auction`；`market_router.go`/`shard_topology.go` 直搬；**P0 前置：escrow + 离线改资产** | 24 |
| trade | 1468 | **缺** | 改造 | 新建 `go/trade`；922 行 Redis 订单状态机可改造；结算腿悬空于 §4.1 | 26 |
| push | 2198 | 有（gate-{id} + C++ gate 三方已用） | 不搬 | 只搬投递缓冲 Lua → `go/shared/pushbuffer/`（正是 B 的 TODO #286） | 14 |
| player_locator | 3115 | 有（同名不同物：A 是 DS presence 五态机，B 是会话生命周期权威） | 不搬 | 三块增量：presence fan-out 等 → 现有 `go/player_locator` | 7 |
| leaderboard | 2192 | **缺**（GUILD 榜除外，B guild 已有双 ZSET 榜） | 改造 | 新建 `go/leaderboard`；`board_store.go` ZSET + 两段 Lua 直搬；SettleBoard 发奖半边砍或等 §4.1 | 18 |
| owner | 1841 | 代码零实现，但 `docs/design/scene-owner-reentry-barrier.md §3.3` 已选 **Redis sidecar epoch** 形态 | 不搬 | 按 B 自己的设计做，不引入 A 的 TiDB 单表权威（A 的三根支柱 hub_source_revision / ds_instance_lease / Admit 调用方在 B 都没有生产者） | — |
| data_service | 1037 | 有（同名不同义，B 4811 行） | 不搬 | 无可搬之物 | 1 |
| battle_result | 7691 | B 零战报表/零 outbox/零 rating，但 48% 是 DS 专属 | 重写 | 新建 `go/battle_result` 只做战报落库/幂等/Elo/出箱/保留期；`mmr.go` 60 行直搬；金币/HP 回写留 B 的 C++ | 35 |
| ds_allocator + hub_allocator | 31877 | DS（与 B 的 scene_manager Agones 层同域） | 跳过 | 唯一残留 fencing token 见 pkg/dsauthfence | 8 |

### 3.3 契约与运维（2 项）

| 组件 | 桶 | 结论 | 说明 | 人日 |
|---|---|---|---|---|
| proto 模块 + tools/migrate + dsticketkeys + plannerdb-provisioner + lexicon-import | 工具链互斥 | 重写 | 34 个配置表 proto 方向相反；协议 proto 逐个重写进 B 的 -I 根 + message_id 申号；`tools/migrate` 与 B 的 `go/db/cmd/migrate` 正撞留 B；`lexicon-import` 可直搬 | 28 |
| deploy / Jenkinsfile / robot/stress | 有 | 不搬 | B 有 6 个 GitHub Actions + 44 个 deploy 文件 + 6006 行压测 robot；A 的 CI 建立在 go.work 上 | 17 |

**汇总**：可原样搬的文件 < 700 行；17 个走对抗验证的组件里 11 个至少一条「可直接搬」被证伪（最常见原因：传递依赖到 `pkg/errcode` / `pkg/log` / A 的 proto）。

---

## 4. 跨组件才暴露的问题

### 4.1 一个缺口卡住五条链（最重要）

mail 领附件、mission 发奖、leaderboard 结算、auction 结算、trade 结算——五条链在 A 全部落到 `inventory.GrantItems / GrantInstances` + `player.AddExperience`。（D14 拍板后 mission 留 C++ 进程内发奖、从这条链退出，剩四条；trade 后置。）B 的 Go 侧**没有**这两个接口：物品权威在 C++ ECS（且 bag 不持久化、零调用点），货币权威 `CurrencySystem::AddCurrency(entt::entity)` **要求玩家在线**，经验只有 scene 能写。

这不是「搬一个 inventory 服务」能解决的（用户已定核心权威留 C++），而是要在 B 里新立一个**「幂等发物 / 发经验」入口**，它的形态由权威归属决定：
- 在线：Go 服务 → C++ scene 的 gRPC（B 已有 C++ 侧 gRPC 客户端，反向通道需新增）→ `BagService` / `CurrencySystem` 内存权威 + 正常存盘链；
- 离线：要么把「待领取」持久化在 Go 侧（邮件附件模型，玩家上线后由 scene 拉取认领——A 的 `rewardclaim` 就是这个模式），要么允许 Go 直写 `BagAllData` 快照（会绕过 C++ 不变量，**不推荐**）。

这一项不在 A 的任何组件里，是 B 自己的新增工作，估 15-20 人日，且是 Batch 6/7 的硬前置。

### 4.2 其它共享假设
- **snowflake epoch**：留 B 的；chat/mail 搬入后其 `MinIDAt` 范围删要按 B epoch 重算，A 存量数据不迁。
- **错误码轴**：所有搬入服务改走 B 的 Tip 轴（客户端查文案只能选 B），每域申私有段。
- **message_id**：B 从空洞按 Go map 随机序分配，A 的 213 个 RPC 进来会打散既有号 → 先给 `tools/proto_generator` 加「新增不改旧号」的测试。
- **配置表命名空间**：五张同名表要么改 A 侧表名、要么分 TableManager；A 的 item 表带 UE 资产路径，B 下游是 Unity+FairyGUI。
- **Redis 客户端两套并存**：go-zero `core/stores/redis`（无 WATCH）只在 match / scene_manager / data_service；go-redis/v9 在 login / player_locator / db / guild / friend。A 的 CAS 仓储（matchmaker 7 处、team）落 go-redis/v9 即可，「B 全仓无 WATCH」是错的。

### 4.3 完整性批判补充的共享前置（基于完整输入的第二轮批判，`critic2.json`）

这些不属于任何单个组件，但每个客户端面服务都会撞上：

| # | 前置 | 证据 | 影响 |
|---|---|---|---|
| P0 | **客户端→Go 服务入站通道在 B 自己身上就是断的**。gate 只发现 `bin/etc/base_deploy_config.yaml:39-46` 列出的 7 个前缀（Scene/Gate/Login/Friend/Guild/SceneManager/Battle），再经 `node_util.cpp:9-20` 的 `nodeTypeNameMap` 映射，`service_discovery_manager.cpp:160` 对映射失败的 key 直接跳过 | `MatchNodeService` / `ChatNodeService` / PlayerLocator 都不在表里 → B 自己的 go/match 客户端发现不了；robot 无任何 match handler | 每个客户端面服务要做 5 件事：node.proto 枚举 → nodeTypeNameMap → base_deploy_config 前缀 → `IsZoneScopedNodeType`（`node_util.cpp:101-116`）→ rpc_event_registry 重生成。应先做成一条可复用清单并把 match 修通 |
| P0 | **Go 私有 tip 码段已用尽**。0..129 导表生成，130-199 保留，200-219 guild / 220-239 friend / 240-259 player_locator（各自 `internal/constants`）；`tipcode.go:182` 对 >129 一律 VerdictUnknown | 259 之后没有约定；svc-chat 自提的 260-279 是自造号段 | 11 个新服务每家至少一个私有段，扩段规则要先定 |
| P0 | **message_id 锁步发版**。`tools/proto_generator/protogen/internal/generator/cpp/service_register_info.go:189-204` 对新方法从 `unUseMessageId`（Go map）随机取号 | 各组件计划新增 80+ 方法；逐个搬 = 12 次以上 C++/Unity/robot/Go 四端锁步发版 | 要么一次性批量登记，要么先把 protogen 改成确定性分配（改 B 工具链） |
| P1 | **B 内部就有两套雪花布局**。login 的 player_id 走 bwmarrin（13 node / 9 step / 毫秒，Epoch 1721473263000 ms，`player_id_gen.go:15-25` 明写换布局作废全部存量 PlayerId）；其余 Go 服务与 C++ 走 shared/snowflake（17/15/秒，Epoch 1773446400） | A 的 player_id 是第三套（17/15/秒，Epoch 1781161165） | 存量数据若要迁，按 player_id 建键的一切都要重映射；cellroute `%4096` 在 bwmarrin 布局下只落 8 个 Cell，没有分片价值 |
| P1 | **配置表加载端 Linux 大小写 bug**。`go/shared/generated/table/item_table.go:57` 读 `Item.json`，产物是 `item.json` | 四个服务 `useBinary=false` | 任何新 Go 服务照 friend 的形状 `LoadTables(dir,false)` 在 Linux 容器里必 `log.Fatalf`；go/match 至今没接表管理器 |
| P1 | **服务端身份提取没有共享实现**。读 `x-session-detail-bin` 的只有 login（14 文件）与 match（3 文件）且互不复用；friend/guild 直接信 `req.PlayerId` | `go/shared` 零 proto import，放不进 shared | 9 个客户端面服务会各抄一份 SessionDetails 解析 + 各自一种验签口径；需要一个鉴权样板 |
| P1 | **推送通道「已有」≠「可靠」**。全仓真正调用 `kafkautil.PushToPlayer` 的只有 `go/match/internal/logic/push.go:70`；friend/guild 构造了 builder 零调用；gate 找不到 session 即丢（`gate_event_handler.cpp:311-317`）；#286 投递缓冲仍是 TODO | chat / team / guild / friend / mission / mail / match-confirm 都把 gate-{id} 当可靠通道 | 投递缓冲层要单独立项，不能挂在判「不搬」的 push 组件下被一起划掉 |
| P1 | **建表两套并存**。`go/db/internal/migrate/plan.go:73-78` 明写禁手写并行 DDL（ProtoSource 从 proto 推导）；而 `deploy/mysql-init` 三个文件手写了 9 张表（guild/friend 先例） | 两套账本表都叫 `schema_migrations` 且列不同 | 新服务建表走哪条要拍板 |
| P1 | **friend / guild / match 只是仓库里有代码**。`deploy/k8s/manifests/go-svc` 9 个文件里没有它们；`tools/scripts/go_services.ps1:113` ServiceCatalogue 只 6 项 | 「搬完能不能跑」取决于目标定义 | 11 个新服务各需 manifest / catalogue / 端口 / Dockerfile 参数，不在任何组件人日里 |
| P2 | **offlinewatch 在 B 没有喂料**。它的输入是 A player_locator 的 departure Kafka 事件；B 的 player_locator 只有 7 个 RPC | pkg-misc 判可搬、svc-team 依赖它 | 连起来是「搬一个没有喂料的消费者」 |
| P2 | `go/AGENTS.md:14-20` 列的 contracts / instance / mail / team 桩目录**不存在** | `ls go/` 实测 | 分桶证据以 `ls` 为准，别信那份文档 |

批判还更正了三处组件结论：writerlease 在 A 侧的使用者是 5 个（player / ds_allocator / hub_allocator / **mail** / mission），pkg-ds 漏了 mail；`deploy/mysql-init` 是 3 个文件 9 张表；B 有 `go/shared/generated/table`（friend/guild/login/player_locator/scene_manager 在用），「Go 侧无表管理器」只是 go/match 局部的注释。

---

## 5. 分批计划（按依赖顺序）

> 第二轮综合（基于完整 30 组件 + `critic2.json`）把原来的「0-5 六个小批」合并成一个 **B 侧底座批**：那些不是各服务的工作量，是 14 个组件各自声明的同一批前置。数字沿用各组件评估的 days，pkg 项按收窄后子集重估。

| 批 | 内容 | 人日 | 验收 |
|---|---|---|---|
| **0 B 侧底座（不是移植；8 个新服务共同的前置）** | 按 §8 ADR 落地：① D16 入站通道：**match 已在 HEAD 修通**（6c4021ae5），只做八步清单成文 + 门禁（含放行既有 chat 不一致）+ k8s ConfigMap 缺陷单（2）② ~~D17 tip 登记表~~ **已完成**（改为「段声明进 Tip.xlsx 组头 + 发号器按段分配」，见 tip-code-axis.md；余 Codex 验证）③ D3 proto 封边：protogen 确定性追加 + 墓碑 + 守卫测试 + CI + 4 个补丁（3）④ 客户端面 Go 鉴权样板（5）⑤ **D1 资产幂等投递**（§8.1 换血合成，含 C++ scene 侧）（20 + 3 bag 前置）⑥ D5 配置表：`Reward.exp` + 修 `item_table.go:57` 大小写 + go/match 接表（3）⑦ D4 `go/tools/migrate`（A 的 migrate 收窄移入）+ 两工具互拒台账 + 每服务库（15）⑧ D9 可用性契约 + `shared/noderegistry` + 修启动挂起 + etcd 钉版/PVC（12）⑨ D13 推送契约 + 单出口 seam + **`BuildPushCommand` 预留游标参数**（5）⑩ k8s manifest / ServiceCatalogue / 端口模板（4）⑪ D12 依赖栈模板 + `shared/connconf`（1.5）⑫ `shared/mysqlx` schema/backend_check + `shared/dbcapacity`（5）⑬ 零依赖直搬 version/releasetrack/log.window + D10 `MinIDAt` 补丁（2） | **≈80** | robot 的 JoinQueue 经 gate→match 在 B 集群跑通；「加节点类型」清单成文并被 chat 复用一次；`tipsegments_test` 全绿且 match 不再撞 common 段；protogen 连跑两次 `message_id.txt` diff 为空；`schemamigrate` 对一个新库 up/status 成功且改动已应用脚本被拒；一个幂等投递端到端：离线玩家的 grant 上线后到账且重复投递不重复 |
| **1 冷域三件（已拍板「邮件/交易行/排行走 Go」）** | mail（12，D15：`mail_channel_seq` 同事务发号，不用选举）→ leaderboard（18）→ auction（24） | **54** | mail：附件领取经幂等投递到账，重复 ClaimMail 不重复发放；**2 副本时系统/公会邮件不漏收（不靠单写者）**。leaderboard：C++ scene 有写分调用点；GUILD scope 请求被拒并指向 go/guild；SettleBoard Top-N 落库且发奖经幂等投递。auction：买卖双方均离线时成交仍到账；同 match_id 幂等 |
| **2 社交 / 组队（B 空壳，面向客户端最重）** | chat（20）→ namecheck + lexicon_import（4.5，**条件**：敏感词方案拍板为 Go 侧）→ team（**45**，D7 翻案：进 `go/match` 同进程但协议独立 + `BeginTeamMatch` 改原子事务 + match 加 `team_id`） | **69.5** | chat：PRIVATE/WORLD/GUILD 三频道 Unity/robot 端到端；WORLD 广播走 `BroadcastToAll`（全仓首个真实调用方）；现查失败必须报错不许假成功。team：14 个 RPC 经 gate 可达；JoinQueue 带 `team_id` 整队成局不可拆票；scene 读到 `team:<id>` 后 `aoi.cpp:27-28` 队伍跟随触发 |
| **3 条件项（各有一个未拍板前提，拍板前不开工）** | dialogue（12，前提：NPC 刷点表 / 交互协议 / 距离判定有人做）；battle_result（35，前提：MMR 落点拍板 + 战报表走 D4 的迁移器）。**mission 按 D14 翻案后整体封存**（1 人日：改文档、去工期、C++ 模块保留不动），设三道重开闸：击杀事实有确定归属／发物入口存在（即 D1）／奖励表有 exp 列（D5） | **48** | dialogue：至少一个真实 npc_id 可 StartDialogue→ChooseOption 跨副本成功（会话在 Redis）。battle_result：同 match_id 重复回报不重复入账；eloDeltas 对 `eBattleOutcome` 显式映射（枚举错位一位） |
| **4 可选增量（B 已有模块的补丁，不属移植）** | friend 四块（18）、guild 五块（28）、login 限流/封禁（12）、locator presence/批量（7）、match MMR+确认期（18）、push 投递缓冲 `shared/pushbuffer`（14，D13 准入条件：客户端 last_seen 上报落地 + ≥2 个真实生产者 + 压测证明重连回源是瓶颈）、kafkax 指标/退避（3）、configtable 加载端校验（12）、callerauth Redis ReplayStore（6） | 118 | 逐项以对应组件 target 段为准；不改 B 现有表主键、不改已分配 message_id、新 tip 码进登记表 |

**总人日**：必做（0+1+2）**≈205**；全做（+3）**≈253**；可选（4）另计 118。口径：单人熟悉 B 仓库、代码进仓库且在 B 的 k8s 集群 robot 冒烟；不含 Unity 客户端 UI、压测、B 自身欠账。比上一版（150/225）高出的部分来自 ADR 把底座做实：D4 迁移器 15、D9 可用性契约 12、D1 从 12 抬到 17、D13 推送契约 5、D7 team +10；反向省下的：D16 −5（match 已在 HEAD 修通）、D14 −21（mission 从「B 自补 22」改为封存 1）。若目标只是「代码入库」可减约 10%。

**这些数字还会动**：D17 的号段方案两选一未定；D15 的 `leader.go` 修复（≈40 行 + 3 用例）还没计入任何一批；`owner_epoch` 5 人日（已降级为加固项，可并行）与 D1b 托管原语都在本表之外。

**trade 去哪了**：第一轮判「改造 26」，第二轮按「背包核心留 C++」的拍板复核后改判**后置**——它的订单状态机（922 行纯 Redis）能搬，但结算腿（`inventory.SettlePlayerTrade` 单事务双人对转）在 B 只能由 C++ scene 的两阶段托管 RPC 承担；资产原语落地前它是空壳，落地后再看是 Go 服务还是 C++ 直接做。

---

## 6. 明确不搬（及理由）

| 不搬 | 理由 |
|---|---|
| pkg/configtable、kafkax、cache、redislock、grpcstats、mission 引擎 | 文件头自陈「移植自 mmorpg」，B 版本更新（B 的 cache 修了 singleflight defer 泄漏，A 没修） |
| pkg/middleware 等 Kratos 胶水 | go-zero 1.10 已默认装配 Trace/Recover/Stat/Prometheus/Breaker/Shedding |
| pkg/auth JWT + passwd | B 无 Envoy 边缘无验签端；bcrypt 弱于 B 的 Argon2id |
| pkg/snowflake 整包 | Epoch 与 C++ 绑死；B 有 Fence/HighWaterEpochSec 而 A 没有 |
| pkg/errcode、safego | 撞 B 的 Tip 轴 / B 的 safego 已 58 处调用 |
| pkg/mysqlx/sharded.go、cellroute（默认） | 与 B「TiDB region 自动切分、应用层不做分片」决策相反 |
| inventory 核心、player、data_service、owner | 双权威（用户已定背包核心留 C++；owner 按 B 设计文档做） |
| login、friend、guild、matchmaker、player_locator、push 整服务 | B 已有完整实现，只做增量 |
| ds_allocator、hub_allocator、pkg/dsauthfence | DS 专属，且与 B scene_manager 的 Agones 层同域 |
| A 的 proto 整包、tools/migrate、deploy、Jenkinsfile、robot/stress、147 个 .ps1 | 工具链互斥 / B 已有 |
| A 的 python/（598 个 .py） | 与本次正交，但要先定性它是等价实现还是分叉的第二事实源 |

---

## 7. 待人拍板（收窄后仍阻塞的）

每条都已做成 ADR（§8），带推荐项与反方结论；这里只列问题本身。

1. **D1 资产幂等投递 / 托管原语的契约**：投递到「当前 owner 节点」靠什么判旧（B 的 §3.3 owner_epoch 未落码 vs 复用 session_version + 节点级再入屏障）；离线玩家的 grant 落哪张表、加载时谁应用；挂单冻结是否要求在线。**阻塞第 1 批全部 + 第 3 批。**
2. **D2 / D17 错误码轴与 tip 私有段扩容**：Go 私有段 200-259 已用尽；(a) 扩段改 `tipcode.go` + 每服务 Classifier，还是 (b) 全进 `Tip.xlsx` 走 exporter；A 的 151 个数值码谁映射。
3. **D3 proto 契约封边**：扩展号 500001 系列谁让；go_package 规则；message_id 一次性批量登记 vs 先改 protogen 确定性分配。
4. **D4 建表纪律与库归属**：手写 `deploy/mysql-init`（guild/friend 先例）vs proto2mysql ProtoSource（`plan.go:73-78` 禁手写并行 DDL）；落 mmorpg 库 / zone_N_db / 新库。battle_result 选了 proto 路，mail/mission/auction/leaderboard 选了手写路，必须统一。
5. **D5 配置表改动的单一 owner**：mission（Reward.xlsx 加 exp）、dialogue（新 Dialogue.xlsx）、battle_result（drop 表）都要改 xlsx 并重跑三语言产物；先修 `item_table.go:57` 大小写 bug。
6. **D6 cellroute**：与 home_zone 目录路由互斥，且裸取模在 bwmarrin 布局下只落 8 个 Cell。
7. **D7 team ↔ match 接缝**：独立 go/team 喂 `party_member_ids`，还是 team 内嵌 match，还是 C++ scene 持有。
8. **D8 A 的 python/ 定性**：README 自称 strangler 迁移、与 Go 版并存；若它已是 A 的维护主线，本计划引用的「A 的设计/判据」以哪份为准。
9. **D9 B 自身可用性与 fail-closed 契约**：搬入服务沿不沿用「etcd 失租即 os.Exit(1)」；B 现有服务对 etcd 不可用的行为。
10. **D10 存量数据与雪花**：A 数据迁不迁；A/B/login 三套 player_id 布局。
11. **D11 / D16 客户端目标与入站通道 owner**：Unity+FairyGUI 为目标是否确认；「修通 match + 加节点类型清单」是否由 C++ 侧承担。
12. **D12 新 module 依赖栈口径**：go-zero 1.9.2/1.10.0、etcd 3.5.15/3.6.5、sarama/kafka-go、go-zero redis/go-redis v9、go 1.24.5；`shared/go.mod` 加不加 mysql 驱动。
13. **D13 推送可靠性层**：`shared/pushbuffer` 是否单独立项并提到第 2 批之前；缓冲键按 player_id 还是 session_id。
14. **D14 mission 事实源**：留 C++ mission（B 自补生产者/持久化/发奖）vs 搬 Go mission 并删 C++ 模块。
15. **D15 writerlease 引不引**：与 `shared/leader` 同域；mail watermark / owner 任期 / 出箱发布器 / match 撮合循环各用哪个。
16. **敏感词方案**：B 已有文档（`docs/design/chat-sensitive-word-filter.md`）拍板 C++ ASCII 折叠 + 字节子串并明确否决 AC/NFKC，但零调用点、词表不在仓库；A 的 namecheck 方向相反。二选一决定第 2 批的 4.5 人日。
17. **目标定义**：「代码进仓库」还是「B 上跑起来」——决定 manifest / catalogue / 端口是否计入。
18. **battle_result 的 MMR 落点**（仅当第 3 批含它）：C++ ECS + player_database 加 rating 字段（跨语言）还是恒返 BaseMMR 只做战报持久化。
19. **dialogue 是否本轮做**（仅当第 3 批含它）：B 的 `Npc{}` 是空 message、无刷点表、无交互协议。

---

## 8. 决策记录（ADR）

每条由一个分析 agent 读两仓写成（options ≥2、每项带 file:line 与人日），再由一个反方 agent 尝试推翻。反方结论：**3 条被翻案（D2 / D4 / D7）**，其余 stands 或带纠正；D12–D17 的反方在补跑。**推荐 ≠ 已拍板**，§10 列出需要你确认的。

**翻案摘要**
- **D2 → 并入 D17**：反方证伪了「exporter 取号 append-only、id 终身稳定」——`enum_gen.py:107` 每次导表用 xlsx 当前内容整体重写 state（实证 kSceneTransferFailed=130 在 4889c0ce2 生成、30498fe9d 重生成时被抹掉）；「Tip.xlsx 是客户端文案唯一来源」仓库内零证据；130-199 只有 70 槽装不下 ≥97 个新号；导表器本机从未跑过且要 protoc 恰 35.1。**最终**：Go 私有段发号 + `tiperr` 薄壳 + `gofmt -r` 改写 + go/match 立即迁号。**但号段方案本身还没定**——见下条 D17。
- **D17 的三条机制被推翻，号段方案待选**：① 我上一版写的「exporter ≤199 硬上限 + 需文案时批量同号导入」是**自相矛盾**的：`global_id` 取跨 group 全局 max+1，钉住 ≥200 后下次导表就撞上限 raise；不进 xlsx 又会被整组抹掉。二者只能选一个。② 登记表对跨 module 的 `internal` 包**没有强制力**。③ match 实际是 **20 个码**（含 40-45 六个观战码），20 宽零余量。反方替代方案：`Lo = 1000 * uint32(ENodeType_<Xxx>NodeService)`，每域 1000 槽，新增码零 shared PR。**需你在两套里选一套。**
- **D14 → 封存（原推荐被推翻）**：「留 C++ 自补四块 22 人日」的两条支柱实测是假的——scene 进程**没有怪物也没有 NPC**（全 scene 唯一实体创建是建玩家，怪物只在回合制 battle 节点），填了杀怪事件也命中不了任何目标；`BagService` 在 modules/bag 之外**零调用点**，`player_battle.cpp:470-478` 自陈背包未挂载玩家实体。**最终**：mission 整体封存（1 人日：改文档、去工期、保留 C++ 模块不动），设三道重开闸。批 3 从 73 降到 47。
- **D15 → R1 必须先改**：`shared/leader` 的 `IsLeader()` 没有时间成分，进程冻结 / 长 GC 这一支完全没防护；`Elector` 也不暴露本届锁值，「Lua 属主校验」对它的使用者不可达。反方还指出 ADR 漏掉了唯一已接线 shared/leader 的组件——scene_manager 死节点收尾链，它逐字命中「该引 fencing」的触发条件。**最终**：不引 writerlease 仍可行，但要先给 `leader.go` 加读时截止 + `Current()`（≈40 行 + 3 用例，原 3.5 人日没算这笔），首个消费者是 scene_manager 的强制销毁 Lua。
- **D4 → O2 收窄版**：反方证伪了「加 SQLSource 不动 runner 主流程」——`runner.go:174-208` Up 写死 Baseline + Drift 两条、`plan.go:64-71` Source 接口无「待应用序列」，逐文件台账 + checksum 要重写应用循环；且 go/db runner 零测试。**最终**：A 的 `tools/migrate` 搬进 B 作 `go/tools/migrate`（独立 module，只 require golang-migrate/v4 + mysql 驱动，删 ≈450 行 A 专属代码，保留 targets manifest / expand-only / coverage 门禁 / 4 个门禁测试），只管 database/sql 关系表；proto 消息表继续 `go/db/cmd/migrate`；两工具各加 ~20 行拒绝对方形状的 `schema_migrations`；每搬入服务一库 `mmorpg_<svc>`。mail 读 guild_members 改经 guild gRPC（推荐）或跨库 GRANT，需拍板。
- **D7 → 方案 2「同进程、分协议」**：反方证伪了「match 用 go-zero redis 无 WATCH 所以不能内嵌」——B 家法是 Lua CAS（`session_cas.go` 模板）且 go-redis 直连可并存。**最终**：`go/match` 内新增 `internal/team`，独立 `proto/team/team.proto` + `NODE_TEAM` + 独立 message_id 段，第二次 `noderegistry.Register`；`BeginTeamMatch` 不作 RPC 改为覆盖 `team:{id}` + `match:ticket:{成员}` + `match:queue` 的原子事务；`JoinQueueRequest` 加 `team_id`，`party_member_ids` 标 deprecated。新 module 数 11→≤9。
- **D6 拆两条**：D6a 不搬 cellroute（0.5）；**D6b「home_zone 目录成为 B 唯一归属权威」是 B 自身新项（3-5 人日）**——现状零写入者零读者、三份归属记录零强制。
- **D9 纠正**：层②按 login/C++ 口径（「重夺同 id 否则退出」），不按 friend 的「换 id 继续」；身份层只强制给真发号服务。
- **D10 纠正**：B 的 `MinIDAt` 只是铸造时刻下界（借位 ≤10s、clock<Epoch 钳 0），范围删须减 `MaxTimeFieldLeadSec=12s`；chat/mail 新表改带 `create_time_ms` 列按时间 sweep，`MinIDAt` 只作可选优化。
- **D16 纠正**：HEAD 已把 Match 补进 `nodeTypeNameMap`（2026-09-01），「修 match」部分去掉，只做八步清单 + 三道门禁 ≈5 人日。
- **D12 修订 R3**：允许 `shared/go.mod` 加 `go-sql-driver/mysql v1.9.0`（与 friend/guild 及 go-zero 1.9.2 自带同版，MVS 不抬任何 module）+ `miniredis v2.35.0`，其余 9 个 go.mod 零改动；这与执行清单 Batch 2 一致，但与 D12 原推荐相反，需拍板。

| ID | 问题 | 推荐 | 人日 | 可逆性 | 反方 | 备注 |
|---|---|---|---|---|---|---|
| **D1** | 资产幂等投递（发物、发经验入口） | **换血合成**（§8.1）：设计 1 的骨架（Go 出箱 + 复用既有 `SceneNodeGrpc` 推送，**整条 D16 链一项都不需要**）+ 设计 2 的幂等基底（`applied_watermark` + 位图与资产同 blob 落盘，无时间维度）+ 设计 3 的 bag 落点（`player_database_1` 空闲字段 3，不新建表）。托管/冻结另立 D1b | **20** + 3（bag 前置）；`owner_epoch` 5 单列 | costly（首个带新 proto 字段的 scene 发版后） | 三方案 + 两裁判，裁判胜者相反、综合裁定 | `owner_epoch` **从上线门禁降级为加固项**；不被 D2/D17/D3 阻塞 |
| **D2** | 错误码轴归属 | 搬入服务全部改走 B 的 Tip 轴；A 域码作为新 group 追加进 `Tip.xlsx` 由 exporter 取号（≥130）；A 公共码人工映射到 B common 段；新建 `go/shared/tiperr` 保持 `New/As/NewCause` 调用形状，`gofmt -r` 机械改写复制进 B 的副本（A 仓一字不改） | 15 | costly（id 随客户端包发出后即契约） | 未挑战 | **与 D17 冲突**，见下 |
| **D17** | Go 私有 tip 段扩容 | **方向保留（Go 私有段），三条机制要换**。反方指出：① **exporter ≤199 硬上限与「批量同号导入补文案」互斥**——`enum_gen.py:116-120` 的 `global_id` 是**跨所有 group 的全局 max+1**，把私有号写进 xlsx 钉住 ≥200 后，下次导表取号就 ≥520 撞上限直接 raise；只手改 json 不进 xlsx 则 `:107` 的 `_save_json` 会把不在 xlsx 的 group **整组抹掉**（kSceneTransferFailed=130 就是这么消失的）。② 跨 module + `internal` 包，shared 无法反向校验任何服务的码，登记表只能被自愿查表的服务引用。③ **match 是 20 个码不是 14**（还有 40-45 六个观战码），20 宽号段零余量 | 5 | 高 | **uncertain** | 反方的替代方案：号段从已有唯一标识派生 —— `Lo = 1000 * uint32(ENodeType_<Xxx>NodeService)`，每域 1000 槽，新增码零 shared PR、不触发 10 个 module 重编矩阵。**这条需要你拍板选哪套** |
| | *D2 vs D17 的合并建议* | 先按 D17 走登记表发号（Go 独立、不触发 C++ 重编）；需要客户端文案的码按发布节奏**批量**写入 `tip_enum_ids.json` + `Tip.xlsx` 同号导入（exporter 尊重已有 id，不改号），而不是每个码过一次 exporter。这样同时满足 D2 的「文案唯一来源是 Tip.xlsx」和 D17 的「不让 11 个服务各跑 11 次 C++ 全量重编」 | 5 + 每次发布 0.5 | | | **需你拍板** |
| **D3** | proto 契约封边 | A 缺席让号（`proto2mysql_option.proto` 与 A 的 data_service.proto 永不进 B）；go_package 写 protogen 注入值 `proto/<dir>`；message_id 单空间 **KeyName 排序追加 + 墓碑**，加 protogen 三用例测试 + CI append-only 门禁 | 3 | easy（首批号发出前） | 未挑战 | D11 补充：InitMessageId 对已知键保号，当前 0..157 无洞，只需封死「删/改名造洞被复用」 |
| **D4** | schema 管理归属 | B 的 `go/db/internal/migrate` runner 升格为全仓唯一迁移器（抽成 `go/schemamigrate` 模块），proto 消息表继续 ProtoSource，搬入的关系表走每服务 embed 的版本化 SQLSource，**每个搬入服务独占库 `mmorpg_<svc>`**；关闭 golang-migrate 与 `deploy/mysql-init` 新增业务表两条路 | 15 | costly（首个生产库跑过 up 后台账即契约） | 未挑战 | 移植 A 的 expand-only 门禁与两道 fail-closed 判据，不移植工具 |
| **D5** | 配置表命名空间 | B 的 `data/*.xlsx` 是唯一命名空间；搬入服务读现有 TableManager；首批只给 `Reward.xlsx` 追加 `exp` 列；Item/Mission/Condition/Skill 不改；不建第二 TableManager | 7 | easy（唯一不可逆：RewardTable 字段号 3） | 未挑战 | 决定性理由：发物终点在 C++ BagService，只认 B 的 ItemTableManager |
| **D6** | cellroute vs home_zone | **不搬 cellroute**，6 人日新层也不做 | 0.5 | easy | **stands**，但纠正：home_zone 目录现状**零写入者零读者**，B 有三份归属记录（AccountSimplePlayer.zone_id / player:zone / EnterScene 填 login 节点 ZoneId）且零处强制；C++ 热路径已在按 `playerId % count` 选 SceneManager 副本 | 「home_zone 是唯一权威」要先落成事实 |
| **D7** | team ↔ match 接缝 | **翻案为方案 2「同进程、分协议」**：team 进 `go/match` 内 `internal/team`，但协议独立（`proto/team/team.proto` + `NODE_TEAM` + 独立 message_id 段 + 第二次 `noderegistry.Register`）；`BeginTeamMatch` 不作 RPC，改为覆盖 `team:{id}` + 每成员 `match:ticket` + `match:queue` 的原子事务；`JoinQueueRequest` 加 `team_id`，`party_member_ids` 标 deprecated | **45**（原估 35） | costly（协议归属发货即固定） | **flip** | 原 ADR 不内嵌的硬理由「match 用 go-zero redis 无 WATCH」被证伪：B 家法是 Lua CAS（`session_cas.go` 模板），且 go-redis 可直连并存。新 module 数 11→≤9 |
| **D8** | A 的 python/ 定性 | Go 的同源翻译件，只读参考，不作移植源，不入 B | 2 | easy | 未挑战 | RPC 面 216/216 机械等价，但语义有刻意偏离且 14/21 零并排验证 |
| **D9** | B 自身可用性与 fail-closed 契约 | 四层契约成文（身份租约退出 / 注册重注册 / 开关 fail-open / 领导权 Redis 降级）；抽 `go/shared/noderegistry`；修 friend/guild/player_locator/login 启动挂起；etcd 钉版 + PVC | 12 | 契约 easy / infra costly | **stands**，纠正：第②层「换 id 不退出」只是 friend 一家口径；**login 与 C++ 都是「重夺同 id 否则退出」**（C++ 注册 node_id 即 snowflake worker id），契约应按 login/C++ 口径 | |
| **D10** | 存量数据与 epoch | A 存量**不迁**（A 生产无注册写路径、online 镜像全是占位）；B epoch 不动；B 侧按 B epoch 补 `MinIDAt`；A 各库停服归档 90 天后弃 | 2 | 数据侧 90 天后 one-way | **stands**，纠正：B 的发号器在墙钟早于 Epoch 时钳 0 从高水位继续（A 是 panic），`MinIDAt` 在 B 没有「时间段 == 铸造秒」的构造性保证，范围删前要加 GuardEpochSec 守卫 | |
| **D11** | 客户端目标与 handler 归属 | **Unity 唯一目标**，A 的 UE 客户端不是目标；B 仓 protogen 一次出 message_id + C++ gate 分发表 + Go 常量 + robot 桩；C# pb 与 handler 归客户端仓；纠正 `proto/AGENTS.md:38` 的「UE4 double 精度」误导 | 15 | costly（→双目标需为 B 加 Envoy/JWT 整层） | 未挑战 | 每域 6 处 gate 登记点，先用 Match 补欠账当样板 |
| **D12** | 新 module 依赖栈 | 钉 `go 1.24.5`（无 toolchain 行）/ go-zero v1.10.0 / etcd client v3.5.15 / go-redis v9 直连 / kafka-go 只做生产 / database/sql 按服务直连；禁直接 require proto2mysql（它把 go 指令钉在 1.26.5，builder 是 1.24-alpine + GOTOOLCHAIN=local） | 1.5 | 高 | **stands**，三处纠正 | ① **「shared 加驱动会波及 5-8 个 module」是错的**——自然实验：`shared/go.mod:6` 已直连 require sarama，而 `player_locator` 的 go.mod/go.sum **各 0 行 sarama** 且 CI 全绿。真实规则是「只有真正 import 到的 module 才受影响」，加 mysql 驱动的实际波及面只有 friend/guild 两个（它们自己直连 v1.9.0 会被 MVS 抬到 v1.9.3）。这直接支持 §10 第 9 条按「允许加」拍板。② go-redis 版本应钉 **v9.18.0**（data_service/db/robot 已在用），写 v9.17.3 会让 11 个新 module 出厂就低于现役。③ 「kafka-go 只做生产、不吃 go-zero redis」只是源码风格约定，拿不到「依赖面干净」——`shared/kafkautil` 一个 package 里 kafka-go 与 sarama 混住，任何调 `PushToPlayer` 的新 module 都会把 sarama 拉进自己的 go.mod（go/match 零 sarama import 却有 `// indirect`）|
| **D13** | 推送可靠性层 | 先立项「推送契约 + 单出口 seam」（Batch 0）：`MessageContent.id` 保留给游标、每条 S2C 自带业务 ID/revision、每域必须有全量拉取 RPC；`shared/pushbuffer` 后置到可选批并设准入条件 | 5 | 高 | **stands**，但三处必改 | ① CI 门禁按原文（`grep '"gate-'` 排除 kafkautil 必须为空）**在 HEAD 就红**——login/player_locator/scene_manager 有 6 处控制面 GateCommand 直接写 `gate-{id}`，不可能改走 PushToPlayer；门禁对象应改为「`PushToPlayerEvent`/`BroadcastTo*Event` 构造点只允许在各服务的 `gate_command_builder.go`」。② 「日后插缓冲调用方零改动」不成立：游标要进信封必须改 `GateCommandBuilder.BuildPushCommand` 签名，而 builder 是**每服务逐字复制**（现 3 份，搬完 9 份）——Batch 0 要同时预留接口参数。③ 规则 4 的触发条件客户端不可观测：`EnterGameResponse` 不含 `enter_gs_type`，应改成「任何 EnterGame 成功后全量回源」 |
| **D14** | mission 事实源 | **翻案为「封存」**。原推荐（留 C++、B 自补四块 22 人日）的两条支柱实测都是假的：① **scene 进程里没有怪物也没有 NPC**——全 scene 唯一的实体创建是 `player_lifecycle.cpp:1154` 建玩家，`grep MonsterTable cpp/libs/services/scene cpp/nodes/scene` = 0，怪物只在回合制 battle 节点；填了 `BeKillEvent` 也只在玩家杀玩家时触发，而 29 条 Condition 里 16 条是 category=1（杀怪），目标集合恒空。② **`BagService`/`BagSystem` 在 modules/bag 之外零调用点**，`player_battle.cpp:470-478` 自陈「背包系统尚未挂载玩家实体」——「在线直接用内存权威」只有货币一条成立。且 `BattleSettlementData` 不带击杀明细，(a) 同样要新建跨进程事实契约 | 1（封存）| 高 | **flip** | 重开三闸：击杀事实有确定归属（`BattleSettlementData` 扩 `{monster_table_id,count}` 复用 `player_battle.cpp:489-565` 已有的按 battle_id 幂等）／发物入口存在（即 D1）／奖励表有 exp 列（D5）。A 的 Go mission 仍不搬（原三条 P0 成立） |
| **D15** | writerlease 引不引 | **翻案：R1 必须先改，与引不引无关**。`shared/leader` 的 `IsLeader() bool` **没有时间成分**——2/3 TTL 自我降级只在心跳 goroutine 被调度时生效，覆盖「Redis 单边不可达」，**不覆盖进程冻结 / 长 GC**；且 `Elector` 没有任何 accessor 暴露本届锁值（`val` 是 `Run` 的局部变量），所以「把 token 写进 Lua 做属主校验」对 shared/leader 使用者根本不可达。反方还指出 ADR **漏掉了唯一一个已接线 shared/leader 的组件**：scene_manager 死节点收尾链，它逐字命中 R3 的触发条件（进程内快照 + 屏障延迟 ≥20s + 强制销毁 + Lua 零属主凭据） | 3.5 **+ 未计的 R1 修复**（≈40 行 + 3 用例） | 高 | **flip** | 最小修法可不引 writerlease：给 `leader.go` 加读时截止 `validUntil` + `Current() (lockValue, held)`，首个消费者定为 scene_manager 的 `luaAtomicDestroyInstanceForce` 加锁值等值校验（同一 Redis 内等值校验即可，不需要 etcd 单调号） |
| **D16** | 客户端入站通道 | **翻案**：ADR 要「先修通」的 match 三处缺口已在 HEAD 落码（提交 6c4021ae5，2026-09-02，且已端到端冒烟 BATTLE_SMOKE_OK）。剩下的只是八步清单成文 + 两件轻量物 + 一个独立 k8s 缺陷单 | **1.5–2**（原估 7） | 高 | **flip** | 门禁②按 ADR 原文写会**当场打死 gate**：`chat.proto:49` 已有 `OptionIsClientProtocolService`、28/61 已在 `IsClientMessageId`，但 ChatNodeService 不在白名单 → 断言即 LOG_FATAL。门禁须先放行既有不一致或改成告警 |

### 8.1 D1 关键设计评审：发物 / 发经验入口

三个独立设计 + 两位裁判（正确性 / 工程）+ 综合裁定。**上一轮的「设计 1 默认胜出」作废**——那次两位裁判因 prompt 截断只看到了设计 1。本轮裁判从磁盘整份读完三个设计。

| 设计 | 形态 | 裁判 A（正确性） | 裁判 B（工程） | 致命缺陷 |
|---|---|---|---|---|
| **1 AssetGrant** | Go 出箱表 → 读 `player:{id}:location` → 复用既有 `SceneNodeGrpc` 推给 C++ → C++ 时间戳台账 | 7.4 | 7.3 | **可复现双发**：台账按 7 天裁剪 × Go 侧 PENDING 行不过期。响应丢 + 玩家离线 >7 天 + 重推落在一次周期存盘之后 → 台账条目已被时间裁掉 → 二次入账。另：道具只到 Redis（把 bag 的 MySQL 半边排除在 18 人日之外） |
| **2 GrantLedger** | Go 只记账 → C++ 主动 Pull（租约）→ 同 tick Apply → **位图与资产同 blob 落盘** → Ack | **7.75（胜）** | 6.7 | Pull 条件 `seq > after_seq` 制造孤儿行，天真修法又引入双发（两处须同批改）。且六步 D16 清单**漏了 `cpp/nodes/scene/main.cpp:65` 的 `CanConnectNodeTypeList`**——不加这行 scene 永远不对它建 channel，而 Pull/Ack 正是它全部正确性的载体 |
| **3 ledger-first** | Go 侧资产账本，应收/实收分离，指令流带扣减 + escrow | 5.7 | **7.5（胜）** | **唯一会销毁真实物品的路径**：`bag_service.h` 只有单件 `RemoveItem`，无批量全或无原语；N 件删到第 k+1 件不存在时，前 k 件已消失、游标已推进、行终态、escrow 已关，没有补偿分录。且托管期内 Go 成为资产权威，与「背包核心权威留 C++」正面冲突 |

**两位裁判的胜者恰好是对方的最低分项**，分歧根源是权重轴不同（幂等基底 vs 关键路径长度），对 B 仓事实的核实高度重合、互不矛盾。设计 1 是唯一没有被任何一方排在末位的，两人给的 `fit_with_B` 都是 9。

#### 裁定：换血合成

> **取设计 1 的骨架，把幂等基底整块换成设计 2 的，bag 落点取设计 3 的。**

- **骨架（设计 1）**：Go 出箱表 + 读 location + 复用**既有**的 `SceneNodeGrpc` 通道。整条 D16 发现链、两个节点类型枚举取号**一项都不需要**——因此两位裁判在设计 2/3 上各抓到的一处枚举撞号（`NODE_GRANT=23` 撞 `NODE_LOG`、`NODE_LEDGER=30` 撞 `NODE_BATTLE`）在本方案下直接作废。
- **幂等基底（设计 2）**：`applied_watermark` + 1024 位窗口位图，与资产同在 `PlayerAllData`、同一条 Lua SET、同一条 DBTask 落盘，**不含任何时间维度**。设计 1 那个双发洞连同成因一起消失。
- **bag 落点（设计 3）**：`player_database_1` 的空闲字段 3。该消息今天只有 `player_id=1` / `stress_test_probe=2`，复用既有的两条 DBTask，**不新建表**——绕开设计 2 的新表撞 `runner.go:396-404` checksum 死角（老库上表建不出来，且只 WARN 不报错）。

**三处直接收益**（写明以免后来者退回去）：① 不再需要「按条数裁剪 + floor 回传 + 3d/7d 常量互指」那套补丁；② **`owner_epoch` 从「生产上线门禁」降级为「加固项」**——分区双写下赢的 blob 其位图与资产必然自洽，重复不会发生；③「apply 后崩、存盘前崩」不再靠 3 天补发窗兜底，行只在响应报出持久化位覆盖其 seq 时才终态。

**人日：本体 20**（设计 1 的 18 −1 拆掉时间台账 +2 位图与水位 +1 回执）**+ 3 bag 前置**；`owner_epoch` 5 人日单列、可并行、不阻塞。

#### 前置与依赖

| 类型 | 内容 |
|---|---|
| **P0 前置** | bag 接进 `PlayerAllData`（`player_data_loader.h:11-21` 各加一行 marshal/unmarshal + proto 加 `bag_data=3`）。今天 `grep bag_marshal:: cpp --include=*.cpp \| grep -v /tests/` **只命中三条注释**，生产既不加载也不保存背包 |
| **P0 纪律** | 上一条动手前**必须与 bag 并发编辑者合并**：`cpp/libs/modules/bag/` 下 7 个文件 M + `bag_profile_registry.{h,cpp}` ??，归属不能看 mtime |
| **P0 实测** | 满包 `BagAllData` 入库字节要实测，确认不被 `key_ordered_consumer.go:550-588` 的 blobGuard 拦下——被拦则 Redis 照写而 MySQL 静默停在旧版 |
| **P1 Linux 必修** | `item_table.go:57` 读 `item.json` 而产物是 `Item.json`；grant 是第一个在容器里消费该表的 Go 服务，大小写敏感必 Fatalf |
| **已解耦** | **不被 D2/D17 阻塞**（v1 复用 `bag_error_tip` 的 `kBagAddItemBagFull=106`，不新造私有段）；**不被 D3 阻塞**（只加 5 个新方法且零客户端面，验收改用「连跑两次 protogen，第二次 diff 为空」）；D4 落地前用 `deploy/mysql-init/grant_tables.sql` 过渡 |
| **明确不需要** | 整条 D16 发现链、`node.proto` / `proto_option.proto` 两个枚举、`main.cpp:65` 白名单、`grpc_client` 生成、`rpc_replies` 装配——**一项都不需要**，记下以免被误加 |

#### 托管 / 冻结（auction 挂单、trade 对转）另立 D1b

**硬边界：D1 的指令流只允许增，禁止把任何扣减放进同一条 seq 流。** 因为 B 侧今天没有全或无的批量扣减原语。但 D1 现在要留两个延展点：

1. `GrantItemSpec` 现在就带 `uint64 preset_item_uuid`（v1 强制 0，非 0 返回 INVALID）——托管物回到背包时必须保留原实例 guid，字段号上线后不可复用，不能等 D1b 再切。
2. 「per-player seq + watermark + 位图」这套载体是**方向无关**的：位图只回答「这个 seq 是否已被恰好应用一次」。D1b 的 `ReserveEscrow` 可作同族 RPC 复用同一份组件、同一条 Lua SET、同一套闸，不需要第二套幂等机制。

D1b 必须先补 C++ 的 `Bag::ReserveForBatchRemove` + `BagService::RemoveItems`，口径对齐既有的「所有会拒绝的判断都在 Reserve 阶段完成」。

## 9. 首两批文件级执行清单

这是「开始搬」时第一刀会碰到的东西——底座批里唯一两组**文件级**的清单（对应 §5 批 0 的 ⑬ 与 ⑪⑫）。每一项都由一个 agent 逐文件打开 A 侧源与 B 侧落点核过 import 与行号，再由另一个 agent 复核。**A 仓一字不改**；Batch 1 零 go.mod 改动，Batch 2 只动 `go/shared/go.mod`。两组合计 ≈16 人日、≈46 个文件（26 新建 + 20 修改）。

通用注意：
- 含 `-race` 的验证命令在 Codex 机器上需 CGO_ENABLED=1 + gcc（本机 CGO_ENABLED=0），否则去掉 `-race`。
- Batch 1 的 item 4（rating）随批 3 battle_result、item 5（offlinewatch/classify）随批 2 team **延后**，现在搬进去是死码；item 11（callerauth）等 §10 第 10 条拍板。
- Batch 1 item 10 的 login 侧 `produce_total` 落点与 item 11 正文在核验输入中被截断，交 Codex 前由我补全。
- 两组都不交付：TableBudget 预算数字、保留期 where 条件、backend_check 的 RequireTiDB 消费者、payload.go、sharded.go、cache/redislock（回流件，B 版更新）。

### Batch 1 零依赖直搬 + 补丁回流（定谳 §5 第 0 批⑬ + §3.1 补丁项；A 仓一字不改，全部落 B 既有 shared / db / login 三个 module，不新增 go.mod）

**前置**：B 工作区干净：cd D:/luyuan/mmorpg && git status --porcelain go/shared go/db go/login deploy/k8s/Dockerfile.go-svc 输出为空（AGENTS §10.2 动手前查既有改动；不覆盖他人工作）；本机 go1.26.5（实测 `go version` = go1.26.5 windows/amd64）；go/db/go.mod:3 钉 go 1.26.5、go/shared 与 go/login 钉 1.24.5，本机工具链全部满足；CI 按各模块 go.mod 定版（.github/workflows/go-modules-ci.yml:188-190），shared/db/login 都在自动发现矩阵内（:90-97 fin；基线全绿由 Codex 先复核：cd D:/luyuan/mmorpg/go/shared && go test -count=1 ./...；cd ../db && go test -count=1 ./...；cd ../login && go test -count=1 ./...（CI 注释 go-modules-ci.yml:71-79 称 2026-08-10 全绿；//go:build integration 用例默认不跑；Epoch 红线：go/shared/snowflake/snowflake.go:16 `Epoch uint64 = 1773446400` 与 cpp/libs/engine/core/utils/id/snow_flake.h:25 `kEpoch = 1773446400` 绑死；本批任何改动不得触碰该行，item 8 新增 TestEpochPinnedToCpp 机械断言；D10 已按定谳 §8 推荐执行：B epoch 不动、按 B epoch 补 MinIDAt、A 存量不迁；MinIDAt 范围删必须带 ≥12s 余量（借位 maxBorrowAheadSec=10 snowflake.go:39 + guardLeadSec=2 allocator.go:545）；item 11（callerauth Redis ReplayStore）需用户先推翻 go/login/internal/logic/pkg/callerauth/callerauth.go:52-56 明写的「刻意不上 Redis，避免每 RPC 一次往返抬高登录 P99」的权衡；未拍板前 item 11 不开工；item 11 每 caller 独立密钥要运维给 cpp gate / Java gateway 分发不同密钥（deploy Secret 层，不在本批）；代码默认回落单密钥 InternalAuth（secrets.go:108-110）保持兼容；item 3（version）要改 deploy/k8s/Dockerfile.go-svc:48-50 的 -X 目标，影响 8 个 Go 镜像构建参数（tools/scripts/go_svc_image.ps1:87 传 BUILD_VERSION）；docker 构建不在 Codex 本地验证范围，只做 go build -ldflags 等价验证；shared 模块纪律：零新增第三方依赖（go/shared/leader/leader_test.go:12 「不引入 miniredis，shared 模块零新增依赖」）、零 proto import（critic2 P1「go/shared 零 proto import」）；本批 shared 侧 9 项全部满足；不改 proto、不改 message_id.txt、不新建 module、不改 B 现有表主键（定谳 §5 第 4 批约束同样适用）；AGENTS §5「没写文档 = 没说过」：item 8/9 完成后在 docs/design/snowflake-id-allocation.md 或 snowflake-node-id-lease-recycling.md 追加一节；PROGRESS.md 只追加一条本批流水

**顺序**：1 logwindow（纯 stdlib，最先入库，item 10 的 Errors() 排空日志限流可作其首个消费者） → 2 releasetrack → 3 version（含 Dockerfile.go-svc + login.go 首个接线点） → 4 rating → 5 offlinewatch/classify → 6 safego Recover/Recovered → 7 cache UniversalClient → 8 snowflake GenerateInto + MinIDAt + TestEpochPinnedToCpp → 9 snowflakealloc keyOwnedByLease 自认领 + 冲突熔断 + Handle.NewNodes（依赖 8 只在同 module 编译层面；NewNodes 只用既有 snowflake.NewNode/SetGuardTime） → —— 以上 1-9 同在 go/shared，一次 `go build ./... && go test -count=1 ./...` 收口，再编译 guild/login/match/scene_manager 四个 snowflakealloc 调用方 —— → 10 go/db kafka 消费主循环指数退避 + Errors() 排空 + 5 个指标（独立 module，可与 1-9 并行） → 11 go/login callerauth ReplayStore + 每 caller 独立密钥（独立 module；前置：用户推翻 callerauth.go:52-56 权衡）

| # | 类型 | 源（A） | 落点（B） | 必须的机械改动 | go.mod | 核验 |
|---|---|---|---|---|---|---|
| 1 | copy | F:/XuanMing-Server/pkg/log/window.go（75 行；window.go:3 唯一 import "sync/atomic"，detail_flat:78/:254 两名验证者确认零耦合）+ F:/XuanMing-Server/pkg/log/window_test. | D:/luyuan/mmorpg/go/shared/logwindow/window.go + D:/luyuan/mmorpg/go/shared/logwindow/window_test.go（新建包；ls go/shared 实测无 log/logwindow 目录；B 全仓 grep s | window.go:1 `package log` → `package logwindow`；window_test.go:6 同改；window.go:29-37 用法示例把 `plog.With(ctx).Warnw("msg","dep_failed",...)` / `plog.With(ctx).Infow(...)` 改写为 go-zero 语汇 `logx.WithContext(ctx).Err；window.go:18-19 删去「§16.5 容量边界、§9.18 有界纪律」（A docs/design/infra.md 章节号，B 无对应文档），改引 AGENTS §9「不要把 player_id 当 Prometheus label」同一精神；文件头加一行来源注释：移植自 XuanMing-Server pkg/log/window.go（2026-09），逻辑零改 | none（stdlib sync/atomic；shared/go.mod 不动） | —  |
| 2 | copy | F:/XuanMing-Server/pkg/releasetrack/policy.go（48 行；policy.go:6-12 import 全 stdlib crypto/sha256、encoding/binary、errors、fmt、strconv）+ policy_test.go（30 | D:/luyuan/mmorpg/go/shared/releasetrack/policy.go + policy_test.go（新建包；B 全仓无确定性灰度分桶，detail_flat:118 「唯一一条完全证伪不掉」） | 零逻辑改动；package 名 releasetrack 保留；文件头加来源注释；policy.go:2-3 的「编排层权威回读到的实际 track」注释保留（对 B 同样成立） | none | —  |
| 3 | copy | F:/XuanMing-Server/pkg/version/version.go（59 行；import fmt、runtime） | D:/luyuan/mmorpg/go/shared/version/version.go（新建）+ D:/luyuan/mmorpg/deploy/k8s/Dockerfile.go-svc:41-50（-X 目标）+ D:/luyuan/mmorpg/go/login/login.go main | version.go:6-11 注释里 `-X github.com/luyuancpp/pandora/pkg/version.Version=` 三行改成 `-X shared/version.Version=` / `.Commit=` / `.BuildTime=`（B ；Dockerfile.go-svc:48-50 `-X main.buildVersion=${BUILD_VERSION}` / `-X main.buildCommit=` / `-X main.buildTime=` → `-X shared/version.Version；Go 链接器只对**被链入**的包做 -X：B 现状 grep buildVersion 于 go/ 零命中 ⇒ -X 静默无效（detail_flat:118）；换目标后同理，至少一个 main 必须 import shared/version 并引用，否则包不链入照样无效。首；version.go:34-40 Info 结构 json tag 保留（将来 /version 或 BUILD_INFO 用） | none（stdlib；login 已 replace shared=>../shared，go/login/go.mod:119） | —  |
| 4 | copy | F:/XuanMing-Server/pkg/rating/pool.go（43 行；pool.go:14 唯一 import "strings"） | D:/luyuan/mmorpg/go/shared/rating/pool.go + 新 pool_test.go（新建包） | pool.go:3-7 注释里 A 的服务链（关卡表→matchmaker→ds_allocator→BattleStorageRecord→battle_result→player）改为 B 的未来链（go/match 成局 → go/battle_result（定谳 §5 第；pool.go:26-30 「与 player_mmr.rating_pool 列宽 VARCHAR(32) 一致」改为「B 尚无该表；第 3 批 battle_result 建表时 rating_pool 列宽必须 == MaxPoolLen，并用测试钉住」（detail_fl；删去 §9.22 / §9.24 / §17.1 引用（A 文档章节号）；DefaultPool / MaxPoolLen / Normalize 逻辑零改 | none | —  |
| 5 | copy | F:/XuanMing-Server/pkg/offlinewatch/classify.go（81 行；classify.go:3 唯一 import "time"）+ F:/XuanMing-Server/pkg/offlinewatch/offlinewatch_test.go:133-205 | D:/luyuan/mmorpg/go/shared/offlinewatch/classify.go + classify_test.go（新建包，只放这一个源文件；offlinewatch.go/locatorreader.go 不搬——它们拖 sarama/go-redis/protobuf  | 导出符号（detail_flat:125 「零导出符号，落 B 是编得过的死代码」）：classify.go:10 `type verdict int` → `Verdict`；:15-21 `verdictUnknown/verdictOnline/verdictWaiting；classify.go:41-47 注释里 A 的 RPC 名 `locator BatchGetLocation` / `BatchGetLastSeen` 改为「输入由调用方提供；B 的 player_locator 目前只有 7 个 RPC 且无 last-seen（pro；删 §9.22 引用；文件头加来源注释；逻辑（:60-81）零改：online 优先 → max(lastSeen, hint) → <=0 Unknown → 满阈值 Offline 否则 Waiting | none | —  |
| 6 | patch-into-existing | F:/XuanMing-Server/pkg/safego/safego.go:39-71（Recover(ctx,name) + Recovered(ctx,name,r) bool 两个 defer 原语）+ safego_test.go:11-24（TestRecover_SwallowsPa | D:/luyuan/mmorpg/go/shared/safego/safego.go（插在 Run 之后、Go 之前，即 :96 与 :98 之间）+ D:/luyuan/mmorpg/go/shared/safego/safego_test.go（追加） | 新增 `func Recover(ctx context.Context, point string)`（必须以 `defer safego.Recover(ctx, point)` 直接调用，A :43-44 注释保留）与 `func Recovered(ctx context；实现复用 B 既有 `report(point, recovered)`（safego.go:148-157：register() + panicTotal{point}.Inc() + logx.Errorw(EventGoroutinePanic, point/panic/s；包文档 safego.go:11-17 「本包的两条原语」补第三段：Recover/Recovered 供无法用 Go/Loop 包装的入口（server-stream handler 把 panic 转成 gRPC 错误、既有 defer 链）使用；point 必须是常量点位名；参数名沿 B 叫 point 不叫 name | none（prometheus client_golang v1.21.1、go-zero v1.9.2 已直连 shared/go.mod:7,10） | —  |
| 7 | patch-into-existing | F:/XuanMing-Server/pkg/cache/cache.go:59（`rdb redis.UniversalClient`；A 文件头 :4 自陈「直接复用自 mmorpg/go/shared/cache/」，唯一改进就是这一签名） | D:/luyuan/mmorpg/go/shared/cache/cache.go:61（`rdb *redis.Client` 一行）+ 新 cache_test.go | cache.go:61 `rdb *redis.Client,` → `rdb redis.UniversalClient,`；函数体 :70 rdb.Get / :90 rdb.Set 在接口上同名可用，无需其它改动；**绝不**把 A 的 singleflightDo（A :30-50 旧写法，无 defer 收尾）带回：B :37-50 已修 dbLoader panic 导致 sfCall 永留 map + WaitGroup 永不归零的缺陷，:97-104 comma-ok 断言也比 ；:56-58 文档补一句：接受 *redis.Client / *redis.ClusterClient / *redis.Ring / Sentinel failover client | none（go-redis v9.16.0 已直连 shared/go.mod:8） | —  |
| 8 | patch-into-existing（不能 copy： | F:/XuanMing-Server/pkg/snowflake/snowflake.go:86-144（GenerateInto 契约与分支）、:208-217（MinIDAt）；测试 F:/XuanMing-Server/pkg/snowflake/generate_into_test.go:1 | D:/luyuan/mmorpg/go/shared/snowflake/snowflake.go（GenerateInto 插在 Generate 之后 :265-267；MinIDAt 与 MaxTimeFieldLeadSec 插在 NowEpochSec :292 之后）+ snowflak | 新增 `func (n *Node) GenerateInto(dst []uint64) (filled int, err error)`：先查 fenced（同 :169-171）；持 n.mu；消化 bootGuardPending（:186-187）；循环直到 fille；GenerateInto 注释抄 A :86-102：与循环 Generate 只差开销不差语义；dst 内不保证连续（跨秒跳变）；dst 由调用方分配，批量入口自行限量；B 额外写明：每一段借位都单独过 maxBorrowAheadSec 预算，一批超过 32768 不允许一次；新增 `func MinIDAt(unixSec int64) uint64`（A :212-217 公式原样：unixSec<int64(Epoch) → 0；否则 (uint64(unixSec)-Epoch)<<timeShift）与 `const MaxTimeField；snowflake.go:16 `Epoch uint64 = 1773446400` 一字不动；:13-14 注释旁加「TestEpochPinnedToCpp 机械断言」提示 | none | —  |
| 9 | patch-into-existing | F:/XuanMing-Server/pkg/snowflake/etcdnode/etcdnode.go:211-223（Txn 响应丢失自认领）、:365-383（keyOwnedByLease）、:74-78 + :191-235（maxConsecutiveTxnFailures=5 熔断） | D:/luyuan/mmorpg/go/shared/snowflakealloc/allocator.go（Allocate :115-211；Handle 结构 :247-280；AllocateWithKeepAlive :311-439；NewNode :457-474；advanceGua | 自认领：新增 `keyOwnedByLease(ctx, cli, key string, leaseID clientv3.LeaseID) (bool, error)`（A :365-383，klog→logx）；在复用分支 :154-166（`err == nil && t；熔断：B 的 for 循环（:132-210）只在 `txnResp.Succeeded==false`（并发抢占 :209）时重来、无次数上限；加 `const maxClaimConflicts = 64`（沿 A maxConsecutiveTxnFailures 语义，改；NewNodes：新增 `func (h *Handle) NewNodes(n int) ([]*snowflake.Node, error)`；n<=0 返回错误（A provider.go:85-87）；每个 = snowflake.NewNode(h.WorkerID) ；NewNodes 文档必须抄 A provider.go:65-71 三条：不同 Node 同秒第 K 个号逐位相同（常态）；每个 ID 空间必须独立表 / key 前缀 / 唯一键；跨服务共享的 ID 空间不适用（spof_full 单点 #4、定谳 §2 表第 4 行的根因） | none（clientv3 v3.5.15 已直连 shared/go.mod:12） | —  |
| 10 | patch-into-existing | F:/XuanMing-Server/pkg/kafkax/consumer.go:159-208（consumeBackoffMin=200ms / Max=30s 常量 + Start 主循环：ErrClosedConsumerGroup 退出、ctx 感知 timer、正常返回复位）、:211 | D:/luyuan/mmorpg/go/db/internal/kafka/key_ordered_consumer.go:756-767（Start 内 go func 主循环：`Consume 返错 → logx.Errorf → time.Sleep(1s)` 固定重试）、:1420-1439 | 退避：:757-767 改成 A :178-208 形态——`backoff := consumeBackoffMin`；`Consume` 返错时 `errors.Is(err, sarama.ErrClosedConsumerGroup)` 直接 return；`metric；Errors() 排空：B :632 设 `config.Consumer.Return.Errors = true` 但整文件无 `.Errors()` 读取（grep 实测 0 命中；sarama 满则丢，非阻塞但全部不可见）→ 照 A :211-225 起排空 gorout；指标 5 个落 db/internal/metrics/metrics.go，命名沿 B 的 Subsystem 风格（:36 `subsystem = "db"` → 本组用 Subsystem "kafka"，无 pandora_ 前缀，AGENTS §9 禁 player_；produce_total 的接线点不在 db：唯一生产者是 go/login/internal/kafka/key_ordered_producer.go:301 与 :377 两处 SendMessage（另一 module，login/go.mod:12 有自己的 clie | none（sarama v1.43.1、go-redis v9.18.0、testify v1.11.1、miniredis v2.36.1、client_go | —  |
| 11 | patch-into-existing | F:/XuanMing-Server/pkg/internalrpcauth/redis.go:13-36（RedisReplayStore：SetNX + TTL，key=sha256(caller:nonce)）、auth.go:46-51（ReplayStore 接口）、:140-165（Ne | D:/luyuan/mmorpg/go/login/internal/logic/pkg/callerauth/verifier.go（Options :13-36、Verifier :41-49、NewVerifier :52-80、Verify :93-143、verifyOnly :148-1 | 新增 `type ReplayStore interface { Consume(ctx context.Context, key string, ttl time.Duration) (bool, error) }`（A auth.go:49-51）；`memoryReplay；Verifier（verifier.go:41-49）：`nonces *nonceSet` → `replays ReplayStore`；Options 加 `Replays ReplayStore`（nil → 用 memoryReplayStore 包 newNonceS；每 caller 独立密钥：Options 加 `SecretsByCaller map[string]SecretVerifier`；Verify 在 :94 取到 caller 并过白名单后，先查 SecretsByCaller[caller]，命中用它验签（:135），未命；接线：login.go:270 `newSessionInterceptor(cfg config.Config)` → `newSessionInterceptor(cfg config.Config, rdb *redis.Client)`；:388 调用处传 `ctx.Re | none（go-redis v9.16.0 直连 login/go.mod:13；miniredis v2.35.0 直连 :7；grpc/metadata 已 | —  |

**Codex 验证命令**：
- item 1：cd D:/luyuan/mmorpg/go/shared && go build ./... && go vet ./logwindow/ && go test ./logwindow/ -race -count=1；通过标准：三条 exit 0，最后一条输出含 `ok shared/logwindow`
- item 2：cd D:/luyuan/mmorpg/go/shared && go test ./releasetrack/ -run TestPolicyDeterministicAndBounded -count=1；通过标准：exit 0 且输出 ok
- item 3：pwsh：cd D:/luyuan/mmorpg/go/shared; go test ./version/ -count=1; $env:EXPECT_VERSION='vtest'; go test -ldflags "-X shared/version.Version=vtest" -run TestInjectedViaLdflags -v ./version/ -count=1; Remove-Item Env:EXPECT_VERSION; cd ../login; go build ./...；通过标准：三条 go 命令 exit 0，第二条输出含 `--- PASS: TestInjectedViaLdflags` 
- item 4：cd D:/luyuan/mmorpg/go/shared && go test ./rating/ -count=1；通过标准：exit 0 且输出 ok
- item 5：cd D:/luyuan/mmorpg/go/shared && go test ./offlinewatch/ -run 'TestClassify' -count=1；通过标准：exit 0，输出 ok 且 8 个子测试全 PASS（加 -v 可见）
- item 6：cd D:/luyuan/mmorpg/go/shared && go build ./... && go vet ./safego/ && go test ./safego/ -count=1 && cd ../login && go build ./... && cd ../match && go build ./...；通过标准：全部 exit 0，safego 输出 ok（13 个测试）
- item 7：cd D:/luyuan/mmorpg/go/shared && go build ./... && go vet ./cache/ && go test ./cache/ -count=1；再核零调用点：grep -rn "shared/cache" D:/luyuan/mmorpg/go --include=*.go ／ grep -v "go/shared/cache/" 应无输出（本次实测为空）；通过标准：go 命令 exit 0 且 ok，grep 为空
- item 8：cd D:/luyuan/mmorpg/go/shared && go build ./... && go test ./snowflake/ -run 'TestGenerateInto／TestMinIDAt／TestEpochPinned' -v -count=1 && go test ./snowflake/ -race -count=1 && grep -n "Epoch uint64 = 1773446400" snowflake/snowflake.go；通过标准：go 命令 exit 0、-v 输出 9 个 PASS、全包 ok（≥24 个测试）、grep 命中 snowflake.go:16
- item 9：cd D:/luyuan/mmorpg/go/shared && go build ./... && go vet ./snowflakealloc/ && go test ./snowflakealloc/ -count=1 && cd ../guild && go build ./... && cd ../login && go build ./... && cd ../match && go build ./... && cd ../scene_manager && go build ./...；通过标准：全部 exit 0，snowflakealloc 输出 ok。集成（可选）：确认本机 etcd 在 allocator_i
- item 10：cd D:/luyuan/mmorpg/go/db && go build ./... && go vet ./internal/kafka/ ./internal/metrics/ && go test ./internal/kafka/ -run 'TestNextConsumeBackoff／TestConsumeLoop' -v -count=1 && go test ./... -count=1；通过标准：全部 exit 0，-v 输出 3 个 PASS，全模块 ok（注意 go/db/go.mod:3 钉 go 1.26.5，本机 go1.26.5 满足）。若同批做 login 侧 produce_total：cd ..
- item 11：cd D:/luyuan/mmorpg/go/login && go build ./... && go vet ./internal/logic/pkg/callerauth/ ./internal/config/ && go test ./internal/logic/pkg/callerauth/ -v -count=1 && go test ./... -count=1；再 `grep -rn "\.Verify(md" D:/luyuan/mmorpg/go/login --include=*.go` 应为空；通过标准：go 命令全部 exit 0，callerauth -v 输出含 7 个新 PASS 且既有 13 个 

**核验补漏**：item 10 尾部:tests / touches_go_mod / verify / risk 四段缺失(JSON 在「ConsumeClaim :1427-1437 poison;c」截断)；item 11(callerauth ReplayStore + 每 caller 独立密钥)整项正文缺失,只在 preconditions 与 order 里被提及；item 10 的 login 侧 produce_total 落点未指明:login/internal/kafka/key_ordered_producer.go 不 import prometheus,需指定 login 内声明/注册指标的包；item 8 缺文档项:preconditions 要求 item 8/9 完成后在 docs/design/snowflake-*.md 追加一节并在 PROGRESS.md 追加流水,item 9 edits 有写、item 8 edits 未列(MinIDAt/MaxTimeFieldLeadSec/GenerateInto 契约同样需要落文档)；item 8 缺「MinIDAt 量纲」说明:A 测试注入 Unix 秒,B nowEpoch 是 Epoch 相对秒,B 测试须传 int64(Epoch)+current；item 3 的其余 7 个 main(data_service/db/friend/guild/match/player_locator/scene_manager)接线明确列为不在本批,但 Dockerfile 改后这 7 个镜像的 -X 仍是 no-op,清单应把「TODO 清单」写进 PROGRESS.md 流水而不是只在 risk 里提；verify 通用缺口:所有含 -race 的命令(item 1、item 8)需注明 Codex 机器须 CGO_ENABLED=1 + gcc,否则 exit 2「-race requires cgo」

人日：清单 11，核验后 11

### Batch 2 — 存储护栏 + 配置薄包（定谳文档 §5 第 0 批 ⑪ shared/connconf + ⑫ shared/mysqlx schema/backend_check + shared/dbcapacity；pkg/config 路径 (c)）

**前置**：拍板 D12 冲突：本清单按「shared/go.mod 加 go-sql-driver/mysql v1.9.0」执行，与 ADR D12 推荐「不加」相反；分歧点是 mysqlx.go（TLS 严格连接器，mysqlx.go:32 import 驱动）是否进 shared。若拍板不加驱动，第 5 项 target 改为 go/login/internal/mysqlclient/，第 4 项只剩 miniredis，其余 8 项不变；拍板日志级别映射：go-zero logx 无 Warn（logs.go 只有 Errorf/Errorw/Infof/Slowf/Sloww）；本清单 Warnw→Errorf、Errorw→Errorf、Infow→Infof（B 全仓 Errorf 460 处 / Errorw 5 / Slowf 0）。若 B 后续对 Errorf 建告警，report_only 积压日志会成噪音，可改 Sloww 但全仓零先例；拍板指标命名：pandora_db_* → dbcapacity_*（Subsystem 形状同 shared/killswitch/metrics.go:15-35）；B 的 deploy/ 与 docs 无任何 pandora_db_ 引用，改名无下游；shared/go.mod:3 的 go 指令保持 1.24.5、不加 toolchain 行（CI go-version-file，go-modules-ci.yml:189；镜像 golang:1.24-alpine，Dockerfile.go-svc:15）；本批代码最高只用 atomic.Uint64/泛型，无 1.25+ 特性；Codex 本机：go1.26.5（go version 实测），`go mod tidy -diff` 可用；模块缓存已有 go-sql-driver/mysql v1.9.0、edwards25519 v1.1.0、miniredis v2.35.0（friend/guild/login 在用），tidy 无需外网；开工前 git -C D:/luyuan/mmorpg status --porcelain go/ 为空（当前 HEAD 5ef65b3f8）；D:/luyuan/proto2mysql 存在（db/go.mod:132 replace 目标，否则第 11 步 db 模块 build 必红）；明确本批不交付：TableBudget 预算数字、保留期 where 条件、backend_check 的 RequireTiDB 消费者、payload.go、sharded.go、cache/redislock（回流件，B 版更新）；第 10 项运行期验证需 deploy/docker-compose.yml 的 MySQL 已起且 deploy/mysql-init/guild_friend_tables.sql 已执行（含 :209 CALL migrate_guild_friend_schema）

**顺序**：1 shared/connconf（三结构 + ParseCompression + 测试）—— 后续 mysqlx.go / redisx/client.go 的签名依赖它 → 2 shared/mysqlx/schema.go（纯拷贝） → 3 shared/mysqlx/backend_check.go + 单测部分（纯拷贝） → 4 shared/go.mod：手写加 go-sql-driver/mysql v1.9.0 + miniredis v2.35.0，Codex 跑 go mod tidy，确认只动 shared/go.mod+go.sum → 5 shared/mysqlx/mysqlx.go 改造 + roots_test/tls_test 移植（依赖 1、4） → 6 shared/redisx/ratelimit.go + 测试（依赖 4 的 miniredis） → 7 shared/redisx/client.go 改造 + 测试（依赖 1） → 8 shared/dbcapacity/capacity.go（表级巡检；logx + sync.Once） → 9 shared/dbcapacity/sweep.go + sweep_test.go（共用 8 的 registerOnce） → 10 friend/guild servicecontext 接 CheckTables/CheckColumns/AssertStrictModeStartup（首个消费者；可单独延后） → 11 收尾复核：cd D:/luyuan/mmorpg/go/shared && go build ./... && go vet ./... && go test -count=1 ./...（注意 shared 既有 kafkautil/snowflakealloc 的 integration 用例默认不跑）；再对 data_service/db/friend/guild/login/match/player_locator/scene_manager 八个

| # | 类型 | 源（A） | 落点（B） | 必须的机械改动 | go.mod | 核验 |
|---|---|---|---|---|---|---|
| 1 | new-file-from-spec | F:/XuanMing-Server/pkg/config/config.go:160-181 (MySQLConf) + :186-222 (RedisConf) + :227-246 (KafkaConfig) + :250-265 (KafkaConfig.ParseCompression) | D:/luyuan/mmorpg/go/shared/connconf/connconf.go（新建目录；ls go/shared 实测无 connconf/，12 个既有包中无任何配置类型包） | package connconf；import 只留 time + github.com/IBM/sarama（shared/go.mod:6 已 require v1.43.1，不引新依赖）；三结构全部 Duration→time.Duration（RedisConf 5 字段、MySQLConf 3 字段、KafkaConfig 6 字段），去掉 A 的 `Std()` 语义；依据：go-zero v1.9.2 core/mapping/unmarshaler.go；tag 全部改 go-zero 形状 `json:"PascalCase[,optional]"`，删除全部 yaml tag 与 omitempty（core/mapping/utils.go:23 只认 optional，omitempty 被静默忽略→缺键报 field n；MySQLConf 字段：DataSource string `json:"DataSource"`（键名对齐 friend/guild 既有 MySQLConf.DataSource，friend/internal/config/config.go:33、friend/etc/ | none（sarama v1.43.1 shared/go.mod:6、go-zero v1.9.2 shared/go.mod:10 均已存在） | —  |
| 2 | copy | F:/XuanMing-Server/pkg/mysqlx/schema.go:1-185（CheckTables :28 / CheckColumns :77 / ColumnSpec :116 / CheckColumnSpecs :131） | D:/luyuan/mmorpg/go/shared/mysqlx/schema.go（新建目录 go/shared/mysqlx/） | 代码零改动：import 只有 context/database/sql/fmt/strings（schema.go:10-15），detail_checks 已 confirmed；注释改 3 处 A 专名：:3 「deploy/mysql-init/*.sql 只在首次初始化执行」在 B 同样成立（deploy/mysql-init/guild_friend_tables.sql:79-83 自陈 docker-entrypoint-initdb.d 只在；本批不接 data_service：它自建表（CREATE TABLE IF NOT EXISTS），「表缺失」状态不存在，只有 CheckColumnSpecs 在那有意义（detail_checks 更正） | none（只 import database/sql；驱动注册由同包 mysqlx.go 承担，见第 4 项） | —  |
| 3 | copy | F:/XuanMing-Server/pkg/mysqlx/backend_check.go:1-146（AssertTiDBBackend :26 / IsTiDBVersion :32 / AssertTiDBVersionAtLeast :59 / tidbVersionAtLeast :68 | D:/luyuan/mmorpg/go/shared/mysqlx/backend_check.go | 代码逻辑零改动：import 只有 context/database/sql/errors/fmt/regexp/strconv/strings（:14-22）；文件头 :1-11 的 owner/login/§9.22/§15.5 叙述改为 B 语境（B 的 TiDB 迁移依据是 docs/design/global-data-layer-tidb-decision.md）；错误文案 :46 `require_tidb=true` → ；**本批零消费者，且必须永远挂在配置门后**：B 全线 dev 指向裸 MySQL:3306（friend/etc/friend.yaml:18、go/ 全仓 sql.Open 7 处无 TiDB），无条件调用会 fail-close 掉每次本地启动。A 的形状：owner/cm | none | —  |
| 4 | patch-into-existing | F:/XuanMing-Server/pkg/go.mod:42（github.com/go-sql-driver/mysql v1.8.1）+ 第 6 项 ratelimit_test.go 的 miniredis 依赖 | D:/luyuan/mmorpg/go/shared/go.mod + D:/luyuan/mmorpg/go/shared/go.sum | shared/go.mod 第一个 require 块加 `github.com/go-sql-driver/mysql v1.9.0`（**钉 v1.9.0 不钉 v1.9.3**：friend/go.mod:7、guild/go.mod:7 直接 require v1.9.0；shared/go.mod 加 `github.com/alicebob/miniredis/v2 v2.35.0`（ratelimit_test.go 用；版本取 friend/guild/login 同款 v2.35.0，shared/go.sum:3-4 已有其 h1+go；shared/go.sum 由 Codex 跑 go mod tidy 生成，预期新增：go-sql-driver/mysql v1.9.0（h1 + /go.mod）、filippo.io/edwards25519 v1.1.0（h1 + /go.mod；mysql v1.9.；**波及面实测结论：其余 8 个 module 的 go.mod/go.sum 均不需改**。证据：(1) 只有 data_service/db/friend/guild/login 五个 module 直接 import 驱动且 go.sum 已有对应行（data_servic | D:/luyuan/mmorpg/go/shared/go.mod（+2 require：go-sql-driver/mysql v1.9.0、alicebob | —  |
| 5 | patch-into-existing | F:/XuanMing-Server/pkg/mysqlx/mysqlx.go:1-218（MustNewClient :52 / NewClient :61 / unsafeTLSConfig :157 / loadStrictRootCAs :170） | D:/luyuan/mmorpg/go/shared/mysqlx/mysqlx.go | import :34 `github.com/luyuancpp/pandora/pkg/config` → `shared/connconf`；:52、:61 签名 `config.MySQLConf` → `connconf.MySQLConf`；:62 `c.DSN` → `c.DataSource`（3 处：:62 判空、:70 ParseDSN、:93 sql.Open）；错误文案 :63 `mysql DSN is empty` → `mysql DataSource is empty`；删 `.Std()` 3 处：:131 ConnMaxLifetime、:140 ConnMaxIdleTime、:144 PingTimeout（字段已是 time.Duration）；错误文案 :68 `tls_ca_file and tls_server_name` → `TLSCAFile and TLSServerName`、:99 同改；:87 `tls_server_name` → `TLSServerName`（B 用 PascalCase jso | 依赖第 4 项 shared/go.mod 已加 go-sql-driver/mysql v1.9.0；本项本身不再改 go.mod | —  |
| 6 | copy | F:/XuanMing-Server/pkg/redisx/ratelimit.go:1-136（RLKey :30 / RLKeyString :36 / Cooldown :44 / ClearCooldown :58 / quotaScript :64 / IncrWindow :73 / Q | D:/luyuan/mmorpg/go/shared/redisx/ratelimit.go（新建目录 go/shared/redisx/） | import 只有 context/fmt/time + github.com/redis/go-redis/v9（:21-27），shared/go.mod:8 已 require go-redis v9.16.0；SetNX/PTTL/NewScript 皆基础 API；key 前缀 :31、:37 `pandora:rl:%s:%s:...` → `rl:%s:%s:...`（B 键无产品前缀：player:session: / login_session: / callerauth: 等，grep 实测）；注释 :18、:99 同改；注释 :8-9 `errcode.ErrRateLimited(9…)` → 「拒绝时由调用方映射 B 的 Tip 码（serverbase/tipcode）」；:16 `docs/design/infra.md §3.2` 引用删除（B 无此文档）；不改语义：fail-open 契约、PX 自过期、report-only 无关 | 依赖第 4 项（miniredis v2.35.0 进 shared/go.mod） | —  |
| 7 | patch-into-existing | F:/XuanMing-Server/pkg/redisx/client.go:1-118（NewClient :26 / NewUniversalClient :66 / NewUniversalClientWithCredentials :75 / NewDeadlineUniversalCli | D:/luyuan/mmorpg/go/shared/redisx/client.go | import :8 `github.com/luyuancpp/pandora/pkg/config` → `shared/connconf`；`config.RedisConf` → `connconf.RedisConf` 5 处（:26、:66、:76、:85、:90）；删 `.Std()` 8 处：NewClient :31-33、:37；newUniversalClient :104-106、:110；`int(c.DB)` → `c.DB` 2 处（:30、:103；connconf.RedisConf.DB 已是 int）；注释 :19 `config.RedisConf.MaintNotifications` → `connconf.RedisConf.MaintNotifications` | none（go-redis v9.16.0 已在 shared/go.mod:8） | —  |
| 8 | new-file-from-spec | F:/XuanMing-Server/pkg/dbguard/dbguard.go:92-356（TableBudget :101 / ColumnBudget :111 / Guard :119 / New :128 / AssertStrictMode :140 / AssertStrictMo | D:/luyuan/mmorpg/go/shared/dbcapacity/capacity.go（新建目录；**不能叫 dbguard**：D:/luyuan/mmorpg/go/db/internal/dbguard 已存在且 allowlist.go:7 明写「两道闸都刻意做成模块内自包含，不 | package dbcapacity；import 去掉 :43 `plog "github.com/luyuancpp/pandora/pkg/log"` 与 :44 `github.com/luyuancpp/pandora/pkg/metrics`，加 `errors`、`；**日志 7 处**（go-zero 无 Warn 级别，logx/logs.go 只有 Errorf/Errorw/Infof/Slowf；B 全仓 Errorf 460 处 / Errorw 5 / Infow 0 / Slowf 0，故按 B 多数派用格式化调用）：:202；**指标 6 个**（:52-80）改名去 pandora_ 前缀，用 B 形状 `prometheus.GaugeOpts{Subsystem:"dbcapacity", Name:"table_rows"}` → dbcapacity_table_rows / table_r；**删除 init() :83-90 的 6 个 metrics.Register**，改成 sync.Once 懒注册：照抄 D:/luyuan/mmorpg/go/shared/killswitch/metrics.go:37-53 模板（`registerOnce.Do`  | none（prometheus client_golang v1.21.1 shared/go.mod:7、go-zero v1.9.2 :10 已存在；只 i | —  |
| 9 | new-file-from-spec | F:/XuanMing-Server/pkg/dbguard/sweep.go:1-193（Mode :35 / ParseMode :57 / Outcome :70 / ReportPending :108 / ReportDeleted :122 / execer :134 / SweepTa | D:/luyuan/mmorpg/go/shared/dbcapacity/sweep.go | package dbcapacity；import 去掉 :30 plog、:31 pkg/metrics，加 logx（sync/errors 已由 capacity.go 引入，registerOnce 共用同一个）；**日志 4 处**：:113-118 `plog.With(ctx).Warnw("msg","db_retention_pending_not_deleted",...)` → `logx.WithContext(ctx).Errorf("[dbcapacity] db_re；**指标 2 个**（:86-94）改名 dbcapacity_retention_pending_rows / retention_deleted_rows_total；**删除 init() :97-100 的 2 个 metrics.Register**，并入 capaci；保持零值 ModeReportOnly（:39）与 ParseMode 拒绝未知值（:57-67）——这就是「默认 report_only」；不改 Count/Delete 共用 where 的机制（:155、:175） | none | —  |
| 10 | patch-into-existing | D:/luyuan/mmorpg/go/friend/internal/svc/servicecontext.go:40-46 与 D:/luyuan/mmorpg/go/guild/internal/svc/servicecontext.go:55-61（现状：sql.Open + db.Ping | 同两文件（首个消费者：让 shared/mysqlx + shared/dbcapacity 不是零调用点） | friend servicecontext.go:46 `db.Ping()` 成功之后插入：`ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second); defer cancel(); if ；guild servicecontext.go:61 之后同样插入：CheckTables(...,"guild","guild_member","guild_schema_migration")（sql:6、:24、:35）+ `mysqlx.CheckColumns(ctx,；friend/go.mod、guild/go.mod **不变**：二者已直接 require go-sql-driver/mysql v1.9.0（friend/go.mod:7、guild/go.mod:7）= shared 钉定版本，go.sum 已有 v1.9.0 两行（；不接 backend_check 三个 Assert（B dev 全线裸 MySQL，见第 3 项）；不接 Guard.Check 容量巡检（预算未推导） | none（预期 go mod tidy -diff 为空；若非空即第 4 项版本钉错） | —  |

**Codex 验证命令**：
- item 1：cd D:/luyuan/mmorpg/go/shared && go build ./... && go vet ./connconf/... && go test -count=1 ./connconf/...；通过标准：三条 exit 0，go test 输出含 `ok shared/connconf`；补充静态门禁：grep -rn 'omitempty\／yaml:"' D:/luyuan/mmorpg/go/shared/connconf/ 输出为空
- item 2：cd D:/luyuan/mmorpg/go/shared && go build ./mysqlx/... && go vet ./mysqlx/...；通过标准 exit 0。集成（可选，需 deploy/docker-compose.yml 的 MySQL 已起且跑过 mysql-init）：$env:SHARED_TEST_MYSQL_DSN='root:<pwd>@tcp(127.0.0.1:3306)/mmorpg?parseTime=true'; cd D:/luyuan/mmorpg/go/shared && go test -count=1 -tags integration -run 'TestCheckTabl
- item 3：cd D:/luyuan/mmorpg/go/shared && go test -count=1 -run 'TestIsTiDBVersion／TestTiDBVersionAtLeast' ./mysqlx/...；通过标准 exit 0 且输出含 ok（无需任何数据库）。可选：docker compose -f D:/luyuan/mmorpg/deploy/docker-compose.tidb.yml up -d 后 $env:SHARED_TEST_TIDB_DSN='root@tcp(127.0.0.1:4000)/test'; go test -count=1 -tags integration -run Agai
- item 4：步骤 1（Codex 执行，Claude 只手写 require 行）：cd D:/luyuan/mmorpg/go/shared && go mod tidy && git -C D:/luyuan/mmorpg status --porcelain go/；通过标准：变更文件只有 go/shared/go.mod、go/shared/go.sum 及本批新增源文件，且 `git -C D:/luyuan/mmorpg diff go/shared/go.mod` 只新增 go-sql-driver/mysql、miniredis、edwards25519 三行、go 指令仍为 1.24.5。步骤 2：pwsh: foreach 
- item 5：cd D:/luyuan/mmorpg/go/shared && go build ./... && go vet ./mysqlx/... && go test -count=1 ./mysqlx/...；通过标准：exit 0，输出含 `ok shared/mysqlx`，且 -v 下 TestNewClient* 9 个用例与 TestLoadStrictRootCAs* 全 PASS、无 SKIP 以外的 FAIL；静态门禁 grep -rn 'pkg/config\／\.Std()\／NewShardSet' D:/luyuan/mmorpg/go/shared/mysqlx/ 输出为空
- item 6：cd D:/luyuan/mmorpg/go/shared && go test -count=1 -run 'TestCooldown／TestClearCooldown／TestQuota／TestIncrWindow／TestArmPenalty／TestPenalty／TestActionQuota／TestRLKey' ./redisx/...；通过标准 exit 0 且输出含 `ok shared/redisx`（miniredis 进程内，无外部 Redis）
- item 7：cd D:/luyuan/mmorpg/go/shared && go build ./... && go vet ./redisx/... && go test -count=1 ./redisx/...；通过标准 exit 0 且输出含 `ok shared/redisx`；静态门禁 grep -rn 'pkg/config\／\.Std()\／pandora' D:/luyuan/mmorpg/go/shared/redisx/ 输出为空
- item 8：cd D:/luyuan/mmorpg/go/shared && go build ./... && go vet ./dbcapacity/... && go test -count=1 ./dbcapacity/...；通过标准 exit 0 且输出含 `ok shared/dbcapacity`；静态门禁：grep -rn 'pandora\／pkg/log\／pkg/metrics\／func init()' D:/luyuan/mmorpg/go/shared/dbcapacity/ 输出为空；grep -c 'registerOnce' D:/luyuan/mmorpg/go/shared/dbcapacity/*.go
- item 9：cd D:/luyuan/mmorpg/go/shared && go test -count=1 -run 'TestParseMode／TestOutcomeCleaned' -v ./dbcapacity/...；通过标准：exit 0，输出含 `--- PASS: TestParseMode` 及其子用例「拼错绝不猜成 delete」「零值就是不删」全 PASS
- item 10：静态：cd D:/luyuan/mmorpg/go/friend && go build ./... && go vet ./internal/svc/... && go mod tidy -diff；cd D:/luyuan/mmorpg/go/guild && 同三条；通过标准全部 exit 0 且 tidy -diff 无输出。运行期正向（需 docker compose -f D:/luyuan/mmorpg/deploy/docker-compose.yml up -d 且 mysql-init 已执行）：cd D:/luyuan/mmorpg/go/friend && go run friend.go -f etc/fr

**核验补漏**：第 5 项 edits 漏 mysqlx.go:103 `mysql tls_ca_file %q: %w`→`mysql TLSCAFile %q: %w`;不改则 tls_test.go:297(已改断言)对应的 TestNewClientRejectsMissingOrMalformedCA 三子用例红；第 3 项测试需拆两个文件:backend_check_test.go(纯表驱动)+ backend_check_integration_test.go(`//go:build integration`);build tag 是文件级,合在一起会把 verify 的 -run 目标一起排除；第 8 项 capacity_test (a) 需先给 GaugeVec 设一个 label 子项再 Gather(或改断言 AlreadyRegisteredError);client_golang v1.21.1 Gather 剔除空 family；第 4 项 risk (b) 出路不成立一半:schema/backend_check 的集成测试仍 sql.Open("mysql"),驱动会以测试依赖进 shared/go.mod;要彻底不加驱动须把集成测试也移出 shared；第 1 项 test (b)「缺键报 field X is not set」对 []string(Brokers)不保证:go-zero unmarshaler.go:945-949 slice 分支走 emptyMap 而非 newInitError,文案可能不同;断言只对 DataSource/Host 这类标量成立；第 7 项 redisx/client.go:1、:11 的「Pandora」大写不被静态门禁 `grep 'pandora'` 抓到,建议 edits 顺手改注释或门禁加 -i；引用纠错(不影响落地):guild_friend_tables.sql 的 initdb 注释在 :71-72 不是 :79-83;A owner 路径应为 services/runtime/owner/cmd/owner/main.go:109 与 services/runtime/owner/internal/conf/conf.go:25;A budgets.go 应为 services/s；第 2 项 schema_integration_test 与第 3 项集成测试的 sql.Open("mysql") 依赖同包 mysqlx.go 的驱动 import;若第 5 项后落或按 (b) 移走,测试文件需自带 `_ "github.com/go-sql-driver/mysql"`；shared/go.sum:3-4 已有 miniredis v2.35.0 残留行但 go.mod 无 require;tidy 后 diff 里这两行会保留而非「新增」,verify 步骤 1 的 diff 预期文字需按此理解；清单在第 10 项 verify 中途截断,第 10 项运行期口径及其后可能存在的条目未核实；corrected_total_days 无原估算可对照(输入未给出),下方数字为按 10 项复杂度(第 5/8 项各约 1 天,其余 0.25-0.5 天)独立估算

人日：清单 5.5，核验后 5

## 10. 开工前检查清单

综合者给出的 12 件事；**第 1、9、10、11 不拍则执行清单 Batch 1/2 也只能开一半**。逐条勾选后再开工。

1. **【口径·P0】E2 目标定义**：按「B 上跑起来」（推荐）还是「代码进仓库」。前者 k8s manifest / ServiceCatalogue / 端口 / Dockerfile 参数计入，必做 ≈205 人日；后者 −10%。
2. **【keystone·P0】D1**：拍板 §8.1 的换血合成形态（20 人日），并批准「bag 接进 PlayerAllData」立项（3 人日，`bag_marshal::` 生产零命中）。`owner_epoch` 已**从上线门禁降级为加固项**（5 人日可并行，不再阻塞上线）。**协调 `cpp/libs/modules/bag/*` 的并发编辑者**（`git status` 实测 7 个 M + `bag_profile_registry.{h,cpp}` ??），D1 的 C++ 前置须等其提交。
3. ~~**【翻案·P0】D2→D17**~~ → **已实施，见 [tip-code-axis.md](tip-code-axis.md)**。两案都没选：真正的标准做法是把段声明进 `Tip.xlsx` 的组头行、让发号器按段分配，于是不需要登记表、也不需要那个惹祸的 exporter 上限。顺带查出「文案列从来没有出口」，一并补上。存量 129 个码一次性重排到 1000/2000/…，guild/friend/match 的 36 个手写码进表（14000/15000/16000），「Go 私有段」概念删除。剩下的只需 Codex 跑一次导表器 + 编译验证。
   <details><summary>原始两案（已作废，留档）</summary>
   - **甲案**（原推荐修补版）：`serverbase/tipsegments.go` 登记表 + 每域**加宽到 ≥64**（20 宽装不下 match 的 20 个码）+ **放弃 exporter ≤199 硬上限**（它与「批量导入补文案」互斥，只能留一个）→ 代价是私有码永远没有客户端文案来源。
   - **乙案**（反方提议）：`Lo = 1000 * uint32(ENodeType_<Xxx>NodeService)`，每域 1000 槽，从已有的全仓唯一枚举派生，不建登记表、新增码零 shared PR、不触发 10 个 module 重编矩阵。
   两案共同部分：`tiperr` 薄壳 + `gofmt -r` 改写 + **go/match 立即迁出 20 个撞号的码**（不是 14 个）。这是批 0 ② 与 A-源 proto 重写的前置。
   </details>
4. **【翻案·P0】D4 O2 收窄版**：A 的 `tools/migrate` 搬进 B 作 `go/tools/migrate` 只管关系表；proto 表继续 go/db runner；两工具互拒对方台账形状；每搬入服务一库 `mmorpg_<svc>`。二选一：mail 读 guild_members 经 guild gRPC（推荐）或跨库 GRANT。
5. **【翻案·P1】D7 方案 2**：team 进 `go/match` 同进程、`NODE_TEAM` 独立协议、第二次 noderegistry 注册、`BeginTeamMatch` 改原子事务；批准 C++ 前置「scene 侧 TeamId 写入点」。可延后到批 2 前。
6. **【产品·P0】D11 + D16 一次确认**：Unity 是唯一客户端目标（A 的 UE 不是）；C++ 侧承担「加客户端面节点类型」八步清单（match 已于 6c4021ae5 补通并冒烟，作为范例不再另修）；补第三条工具链规则堵「S2C Notify* 可被客户端直呼」的洞；指定客户端仓 `D:/luyuan/mmorpg-client` 每域 3-4 人日的 owner。**注意**：D16 的门禁②（gate 启动断言「客户端目标类型 ⊆ 白名单∩映射∩前缀」）按原文落地会当场打死 gate——`chat.proto:49` 已声明客户端协议服务、message_id 28/61 已在 `IsClientMessageId`，但 ChatNodeService 不在白名单；门禁必须先放行这批既有不一致，或降为告警直到 chat 落地。
7. **【范围·P1】D14 翻案后**：mission **整体封存**（A 的 Go mission 不搬 + B 的 C++ 模块保留不动，1 人日改文档），不做原计划的「B 自补四块 22 人日」——因为 scene 里没有怪物、`BagService` 零调用点，那 22 人日做完也点不亮。设三道重开闸：击杀事实有确定归属／D1 落地／`Reward.exp` 落地。
   **顺带确认一件事**：这条同时说明 B 的 PVE 玩法链（杀怪→任务→奖励）今天是断的，不只是移植问题。要不要单独立项由你定。
8. **【数据·P2 但 one-way】D10**：确认 A 无线上玩家数据（判据：accounts 表无非 robot/stress 前缀且非 dev 集群的账号；online overlay 仍是 `registry.example.com` 占位）→ 不迁；A 各库停服全量归档、90 天后删除由你在仓库外执行；同意 chat/mail 新表按时间列 sweep。
9. **【go.mod 授权·P1】D12 R3 例外**：允许 `go/shared/go.mod` 加 `go-sql-driver/mysql v1.9.0` + `miniredis v2.35.0`（执行清单 Batch 2 item 4），其余 9 个 go.mod 零改动。**反方已把这条的风险打掉**：`shared/go.mod` 早就直连 require sarama，而 `player_locator` 的 go.mod/go.sum 各 0 行 sarama 且 CI 全绿——「加驱动会波及 5-8 个 module」不成立，实际只波及 friend/guild 两个。同时新 module 模板里的 go-redis 版本请钉 **v9.18.0**（不是 v9.17.3，后者低于现役）。**本期允许改的 go.mod = 仅 `go/shared/go.mod`**；Batch 1 零 go.mod 改动。
10. **【Batch 1 item 11·P1】callerauth**：决定是否推翻 `callerauth.go:52-56`「刻意不上 Redis 避免抬登录 P99」的权衡（每条带 `x-session-detail-bin` 的 RPC 多一次 SETNX）；不推翻则 item 11 不开工，其余不受影响；推翻则运维需给 C++ gate / Java gateway 分发每 caller 独立密钥。
11. **【环境·P0 for Codex】**：Codex 机器补 Python 3（`py -3`，本机 `python` 是 Store 存根）+ openpyxl + jinja2 + **protoc 恰 35.1**（`pb.h` `#if PROTOBUF_VERSION != 7035001`）+ protoc-gen-go（导表器本机从未跑过，PROGRESS.md:1680）；若要 `-race` 需 mingw-w64 + CGO_ENABLED=1（本机 CGO_ENABLED=0）；`D:/luyuan/proto2mysql` 须存在（db/go.mod replace）。
12. **【无默认·P2，可延后到批 2/3 前】** E1 敏感词方案（B 的 C++ ASCII 折叠 + 你提供词表 vs A 的 Go namecheck）；E3 battle_result MMR 落点（C++ ECS 加 rating vs 恒返 BaseMMR）；E4 dialogue 本轮做否（NPC 刷点表 / 交互协议 / 距离判定三前提有人做）。同期 B 自身欠账另列：D6b home_zone 唯一权威（3-5）、D9 3 成员 etcd、D5 导表器 per-table 语言过滤。

**开工前基线命令**（Codex）：
```bash
cd /d/luyuan/mmorpg && git status --porcelain go/ && git log -1 --format='%h %ci'
```
通过标准：`go/` 输出为空（本轮实测 HEAD 5ef65b3f8 时为空）；`cpp/libs/modules/bag/*` 的 M/?? 属并发编辑者，与 Batch 1/2 无关但阻塞 D1 的 C++ 前置。

**残余风险（摘要，全文见工作流 3 报告 residual_risks）**：Codex 本机 CGO_ENABLED=0 → 所有 `-race` 不可用，Batch 1 的并发正确性只靠断言；Reward.exp 是线协议重排（exp=2、reward 2→3）非追加，表与 C++ gate/scene + 5 个 Go 服务 + Java 必须同批重编重发；D3 墓碑空槽在 `game_channel.cpp:434-435` 是可达 UB，补丁前 C++ 收到墓碑号即崩；D11 第三条规则落地前客户端可直呼 20 个 Notify* 号；Batch 1 有 6 项入库即死码（已各指定首个消费者或延后批次）；`go/db` 被 proto2mysql 钉在 go 1.26.5 而 builder 是 1.24-alpine + GOTOOLCHAIN=local，db 镜像构建形态未验证。

## 11. 审计产物

- 工作流 1（60 agent）：`~/.claude/projects/F--work/fe9e8bcf-.../subagents/workflows/wf_bd4b91c1-ad4/journal.jsonl`
- 工作流 2（pkg/config 补评 + 重综合）：同目录 `wf_912a2805-5e0/`
- 工作流 3（标准分析：ADR × 11 + 关键设计评审 + 首两批清单）：同目录 `wf_e78dc3ce-554/`
- 工作流 4（补充 ADR × 6 + 反方）：同目录 `wf_c0d9cf3f-c56/`
- 工作流 5（D1 三方案重评，裁判从磁盘读全部设计）：同目录 `wf_f06b210a-d2d/`
- 中间结果文件（scratchpad）：`adrs_main3.json` / `adrs_supp.json` / `keystone3.json` / `manifests3.json` / `report3.json` / `critic2.json` / `plan2.json`
- 拆分后的中间结果：scratchpad `components.json`（30 组件）、`detail_checks.json`（17 组件对抗验证）、`spof_full.json`（单点 + 3 验证者）、`surveys_full.json`（8 维度）、`pkg_config.json`
- 相关既有文档：`docs/design/scene-owner-reentry-barrier.md`、`docs/design/bag-instance-layout-split.md`、`docs/design/player-async-save-loss-windows.md`、`docs/notes/todo_zh.md #286`

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
| pkg/dsauthfence + dsauthrecord + dsmetadata + battleabort | 4524 | DS 专属 | 跳过 | 唯一残留 `writerlease`（789 行 Chubby 式 fencing token）有条件 → `go/shared/writerlease/`，与 `shared/leader` 并列不替换 | 4 |
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
| mission | 2742 | 有 C++ 引擎（A 的 Go 引擎本来就移植自 B），缺持久化+发奖半边 | 改造 | 新建 `go/mission` 只做 I/O 半边（MySQL 权威/幂等收据/发奖流水/出箱）；**P0 前置：发奖下游**；B 的 ConditionEvent 全仓零生产者 | 26 |
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

mail 领附件、mission 发奖、leaderboard 结算、auction 结算、trade 结算——五条链在 A 全部落到 `inventory.GrantItems / GrantInstances` + `player.AddExperience`。B 的 Go 侧**没有**这两个接口：物品权威在 C++ ECS（且 bag 不持久化、零调用点），货币权威 `CurrencySystem::AddCurrency(entt::entity)` **要求玩家在线**，经验只有 scene 能写。

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
| **0 B 侧底座（不是移植；8 个新服务共同的前置）** | ① 客户端→Go 入站通道：加节点类型清单（node.proto 枚举 / `nodeTypeNameMap` / `base_deploy_config` 前缀 / `IsZoneScopedNodeType` / rpc_event_registry 重生成），先把 go/match 的客户端腿修通 + robot match handler（3）② tip 码轴扩段：改 `tipcode.go` tipDomains + 每服务 Classifier 模板（2）③ message_id：改 protogen 空洞随机分配为确定性，或一次性批量登记（3）④ 客户端面 Go 服务鉴权样板：`x-session-detail-bin` 解析 + 验签口径，一份被 8 个服务复用（5）⑤ **资产幂等投递 + 托管原语**（§4.1，含 C++ scene 侧）（12，待 §8 关键设计评审后校准）⑥ 配置表：修 `item_table.go:57` 大小写 bug + go/match 接表管理器（1）⑦ 建表纪律拍板落地（0.5）⑧ k8s manifest / ServiceCatalogue / 端口模板（4）⑨ `shared/connconf` 薄包（0.5）+ `shared/mysqlx` schema/backend_check + `shared/dbcapacity`（5）⑩ 零依赖直搬：version / releasetrack / log.window（1）+ snowflake `GenerateInto`/`MinIDAt` 补丁（1） | **38** | robot 的 JoinQueue 经 gate→match 在 B 集群跑通（当前 `PickRandomNode` 恒 nullopt）；「加节点类型」清单成文并被 chat 复用一次；`tipcode.go` 扩段后 guild/friend/player_locator 的 constants_test 与新段测试全绿；protogen 连跑两次 `message_id.txt` diff 为空；一个幂等投递端到端：离线玩家的 grant 上线后到账且重复投递不重复 |
| **1 冷域三件（已拍板「邮件/交易行/排行走 Go」）** | mail（12）→ leaderboard（18）→ auction（24） | **54** | mail：附件领取经幂等投递到账，重复 ClaimMail 不重复发放；2 副本时系统/公会邮件不漏收（`shared/leader` 单写者持有 watermark）。leaderboard：C++ scene 有写分调用点；GUILD scope 请求被拒并指向 go/guild；SettleBoard Top-N 落库且发奖经幂等投递。auction：买卖双方均离线时成交仍到账；同 match_id 幂等 |
| **2 社交 / 组队（B 空壳，面向客户端最重）** | chat（20）→ namecheck + lexicon_import（4.5，**条件**：敏感词方案拍板为 Go 侧）→ team（35） | **59.5** | chat：PRIVATE/WORLD/GUILD 三频道 Unity/robot 端到端；WORLD 广播走 `BroadcastToAll`（全仓首个真实调用方）；现查失败必须报错不许假成功。team：14 个 RPC 经 gate 可达；JoinQueue 带 `party_member_ids` 整队成局不可拆票；scene 读到 `team:<id>` 后 `aoi.cpp:27-28` 队伍跟随触发 |
| **3 条件项（各有一个未拍板前提，拍板前不开工）** | mission（26，前提：拍板搬 Go 并删 `cpp/libs/modules/mission`、scene 新增 ConditionEvent 生产者）；dialogue（12，前提：NPC 刷点表 / 交互协议 / 距离判定有人做，否则只能假 npc_id 联调）；battle_result（35，前提：MMR 落点拍板 + 战报表建表纪律） | **73** | mission：`ReportMissionFacts` 收据幂等，发奖流水经幂等投递清零。dialogue：至少一个真实 npc_id 可 StartDialogue→ChooseOption 跨副本成功（会话在 Redis）。battle_result：同 match_id 重复回报不重复入账；eloDeltas 对 `eBattleOutcome` 显式映射（枚举错位一位） |
| **4 可选增量（B 已有模块的补丁，不属移植）** | friend 四块（18）、guild 五块（28）、login 限流/封禁（12）、locator presence/批量（7）、match MMR+确认期（18）、**push 投递缓冲 `shared/pushbuffer`（14，若要求可靠推送应提到第 2 批之前）**、kafkax 指标/退避（3）、configtable 加载端校验（12）、callerauth Redis ReplayStore（6） | 118 | 逐项以对应组件 target 段为准；不改 B 现有表主键、不改已分配 message_id、新 tip 码进扩段 |

**总人日**：必做（0+1+2）**≈150**；全做（+3）**≈225**；可选（4）另计 118。口径：单人熟悉 B 仓库、代码进仓库且在 B 的 k8s 集群 robot 冒烟；不含 Unity 客户端 UI、压测、B 自身欠账（friend/guild manifest、match 客户端腿之外的接线）。若目标只是「代码入库」可减约 10%。

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

（待填：主分析工作流 `wf_e78dc3ce-554` 的 11 份 ADR + 关键设计 D1，补充工作流 `wf_c0d9cf3f-c56` 的 6 份 ADR，均含反方挑战结论。）

## 9. 首两批文件级执行清单

（待填：Batch 底座中「零依赖直搬 + 补丁回流」与「存储护栏 + 配置薄包」两组的 source → target → 机械改动 → 测试 → go.mod → Codex 验证命令。）

## 10. 开工前检查清单

（待填：用户需确认的决策与需提供的仓库外材料。）

## 11. 审计产物

- 工作流 1（60 agent）：`~/.claude/projects/F--work/fe9e8bcf-.../subagents/workflows/wf_bd4b91c1-ad4/journal.jsonl`
- 工作流 2（pkg/config 补评 + 重综合）：同目录 `wf_912a2805-5e0/`
- 工作流 3（标准分析：ADR × 11 + 关键设计评审 + 首两批清单）：同目录 `wf_e78dc3ce-554/`
- 工作流 4（补充 ADR × 6）：同目录 `wf_c0d9cf3f-c56/`
- 拆分后的中间结果：scratchpad `components.json`（30 组件）、`detail_checks.json`（17 组件对抗验证）、`spof_full.json`（单点 + 3 验证者）、`surveys_full.json`（8 维度）、`pkg_config.json`
- 相关既有文档：`docs/design/scene-owner-reentry-barrier.md`、`docs/design/bag-instance-layout-split.md`、`docs/design/player-async-save-loss-windows.md`、`docs/notes/todo_zh.md #286`

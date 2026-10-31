# 全服匹配：按模板分池、逐级扩大范围、每池单写者

## 2026-10-29：人数档真人PVP

新增模式`PVP_TEAM=7`，`team_size`表示每侧1～5人。PVE仍按当前人数直接开战；新PVP即使队伍已经达到目标人数，也必须排队等另一侧真人，不得沿用“满队直接PVE”的分支。旧1V1/5V5编号和含义保留，不引入机器人或新的评分策略。

队伍候选投影及索引增加`mode`，在LIMIT之前按模式、配置和每侧人数隔离；原件中的模式必须与请求一致。匹配单位不可拆，可用散人补齐同侧，两侧人数必须相等。事务提交点原子复核名单、原票、原队伍版本和双方分边；`MatchFormationMember.team_index`持久化分边，首次开局与恢复都读取同一原件，不按名单下标或蛇形评分重建。取消与失败补偿沿用原意图、墓碑和所有权规则。

本节为新契约，实际验证状态以`docs/PROGRESS.md`新增条目为准。

> **状态**：设计已拍板（2026-10-24，负责人）。第一批（纯函数包与两张新表）与第二批（接线：模板目录、按池撮合、提交点复核、肇事者剔除、单人即时成局、机房内按负载选节点）均已落源码，**未编译、未测试，待 Codex 验证**（§12）。实现与原设计不同的地方（池归属改用 etcd 注册表做 HRW、不建租约表等）已在 §2、§5 原位更正，进度见 §11。
> **适用**：Go 版 `go/match` 的全局 TiDB 权威路径（生产路径）。Java 版同批跟进，见 §13。
> **关系**：本文是 [cross-zone-matchmaking.md](cross-zone-matchmaking.md) §12 / §13 的后续实现设计；与之冲突处以本文为准。
> [pve-team-size-matching.md](pve-team-size-matching.md)、[team-backfill-matching.md](team-backfill-matching.md) 只描述 Redis 兼容路径，全局路径的人数档与补位以本文为准。
> 调研与方案评审的证据（file:line）见本会话的调研结论，要点已收进 §1 的"现状缺口"列。

## 0. 一句话

匹配池按 **模式 × 怪物战斗模板（`battle_config_id`）× 人数档** 切分，全服共用。每张票先只和**同 zone** 的人凑，等久了放宽到**同大区**，再等放宽到**全服**，像在树上逐层往上找；物理机房（延迟）仍按 D-16 区域内优先、等满后跨机房。每个池由一个副本独占写，一轮切出多局；单人档入队即开局。提交点仍是 TiDB `claimTx`，正确性不依赖租约。

## 1. 已拍板决策（2026-10-24）

| # | 决策 | 解决的现状缺口 |
|---|---|---|
| D1 | 容量门槛：上线按 S1（入队 833 人/秒，capacity-model-hok-scale.md:317），设计余量 S2（4,167 人/秒）。模式占比未给，压测先按 PVE单人 40% / PVE组队 30% / 1V1 15% / 5V5 15% 的假设，拿到真实占比后替换 | 现状每副本每 500ms 最多成 1 局（global_match.go:298-312），2 副本约 8 局/秒，差两个数量级 |
| D2 | **逐级扩大范围**：同 zone → 同大区 → 全服，适用于所有需要凑人的匹配（PVE 组队人数档、队伍补位、1V1、5V5）。替代 `Team.AllowCrossZone` 对撮合池的 home 过滤；组队社交约束（谁能和谁组队）不变。覆盖 team-system.md:2539「最终队伍同 home」对匹配组的适用范围 | 生产 PVE 组队按 home zone 硬切池（region_candidates.go:160-171,189-191），与"全服"相反 |
| D3 | 单人档（PVE_SOLO、PVE_TEAM 1 人档）以及满员预组队：首选机房没有可分配 battle 节点时，**立即**放到硬延迟内 RTT 最小的可分配机房，不等跨机房阈值（没有同伴可等）。这是 D-16 / §13.1 对"组内没有陌生人"情形的细化 | 单人档首选机房无节点时白等 30 秒（store.go:535-541） |
| D4 | 模板入口用新表 **PlayMode**（白名单 + 人数上限），Dungeon 只描述"打什么"。撤销 Java `MatchProperties.java:28-30`「不能改成查表」的约束 | match 不读表；任意 `battle_config_id` 都能建池（joinqueuelogic.go:70-98）；改模板要停全部副本（policy.go:27-34） |
| D5 | 首领本 Dungeon id3 首发不开放（不进 PlayMode）。开放前先给 Dungeon 加"每人怪物只数 / 固定阵容"列，并让 Java 引擎追平按人数放大 | 表里 max_team_size=10 而引擎上限 5；缺每人只数列 |
| D6 | 战斗期限 = 成局时刻 + 备战预算 + 模板 `time_limit`，上限 3600 秒；PVP 回合打满判平的阈值由 `time_limit` 推导，删掉 `RatingDrawRoundCap*` | claimTx 写死 +300000ms（store.go:543-546），模板时长在生产不生效 |
| D7 | PVP：每个计分模式一行白名单；1V1 与 5V5 分开计分；计分局禁止观战（复用 16018）；赛季首发不做（运维手册禁止调用 `StopRankSource`，它一停写就会卡住结果消费） | 客户端可任填 config 建私有池刷分；评分全服一份；计分局可实时观战 |
| D8 | 错误码：新增"需要重新探测"与"没有机房满足延迟"两个码；16004 只留给权威库不可用并标为 fault。16002 / 16003 分工以 Go 为准，两版与客户端同批改 | 16004 混用四种情况（global_match.go:41-61），客户端无法区分 |
| D9 | Redis 兼容路径冻结：不再加新语义。dev/test 改用本机一次性 TiDB/MySQL 跑全局路径，删除时机另定 | dev 与生产两套算法与失败语义（logic 包 18 个文件有 Authority 分支） |
| D10 | 架构：原位改造（每池单写者 + 一轮多局 + 单人即时成局），不做内存撮合；只在压测满足 §5.6 的切换条件时再评估 | — |

其余实现细节按"最标准的方式"处理（负责人原话），即本文 §3–§10 的规则。

## 2. 术语

- **池（pool）**：`(mode, battle_config_id, team_size)`。同一个池里的票才可能进同一局。不同模板、不同人数档永不混。
- **单位（unit）**：不可拆的撮合单位，一张单排票或一支完整预组队。
- **范围树**：home zone（叶）→ 大区（ZoneGroup 表）→ 全服（根）。
- **范围层级**：`zone`=0、`group`=1、`global`=2。一组人的层级 = 全体成员在树上的最近公共祖先。
- **归属键**：`(mode, battle_config_id)`（`catalog.OwnerKey`）。同一模板的各人数档归同一个副本，为以后跨档合池留接口。
- **写者**：按归属键负责某个池的 match 副本（§5.2，2026-10-30 按实现更正：原"256 个租约分片"不做）。

## 3. 范围树与逐级放宽

实现：`go/match/internal/scope`（纯函数，第一批已落）。

### 3.1 大区从哪来

新表 **ZoneGroup**（`data/schema/zonegroup_table.proto`）：每行一个 zone 号段 `[zone_begin, zone_end]` → `group_id`，同一大区可以多行，号段不重叠。初始数据：大区 1 = zone 1–100，大区 2 = zone 101–200。

- 号段式而不是逐 zone 登记：开新区落在预留号段里不用改表。
- 没有落在任何号段的 zone 视为**未分大区**：放宽时从 zone 层直接到全服。这是匹配质量维度，选择不拒绝入队（可用性优先），风险是新区没登记时少了中间一层。原计划的 `match_scope_ungrouped_total` 未做；成局按层级计数见 `match_pool_claim_total{level}`。
- 只描述玩法分组。data_service 的 `Regions`（zone → 存储集群）是存储布局，二者独立：搬存储不该改变谁和谁匹配（AGENTS.md §11.6「是什么」与「摆在哪」分层）。

### 3.2 放宽规则

- 票据入队时 `zone_id` 写真实 home zone（读 `player_route.home_zone`，不再用当前所在场景的 zone；整队成员取队伍原件里的 home）。**实现更正（2026-10-30）**：不新增 `zone_group` 列，大区由撮合与提交点各自用本副本模板目录里的 ZoneGroup 号段换算（`catalog.ZoneNode`）。滚动换表时不同副本换算出的大区可能短暂不同，只影响这段时间的大区层合并，不影响正确性；提交点另核对票上的 home 仍是玩家当前 home（`requireTicketHomesTx`），迁移或合服后的旧票不能借旧 home 成局。
- 允许层级只看等待（从原始入队时刻起算，重试与回队不重置）：
  - 等待 < `ScopeGroupAfterSeconds`：只同 zone；
  - < `ScopeGlobalAfterSeconds`：可到同大区；
  - 之后：可到全服。
  - 初始值 10 秒 / 20 秒（`CrossRegionWaitSeconds` 仍是 30 秒）。三个值都是待定标的配置，按 D-16 用真实等待、成局、延迟分布校准；进全局策略指纹（各副本必须一致，提交点要用）。
- **双向同意**：一组人跨到哪一层，组里**每个单位**都必须已经等到允许这一层（`scope.Admit`）。刚入队的人不会被别人的长等待拖进更大的范围，他自己的"同 zone 优先"不被打破。
- 预组队成员本来就跨了几层时，这个跨度是队伍自身的范围，不需要再等（`scope.UnitAllowed`）。
- 组里只有一个单位时（单人档、满员预组队）没有陌生人，不受范围约束。

### 3.3 与机房、评分两个维度的关系

三个维度各自按等待放宽，互相独立，提交点各自复核：

| 维度 | 放宽 | 硬约束 |
|---|---|---|
| 范围树（本文） | zone → 大区 → 全服 | 双向同意 |
| 物理机房（D-16） | 全员首选机房相同 → 全员等满 `CrossRegionWaitSeconds` 后可跨机房 | 每名成员到放置机房 RTT ≤ `HardLatencyMs` |
| 评分（仅 1V1 / 5V5） | 容差曲线随锚点等待放宽（现状不变） | 组内评分极差 ≤ 锚点容差 |

搜索顺序：先本机房阶段，再跨机房阶段（区域内优先）；每个阶段内范围从窄到宽。默认阈值下自然形成：0–10 秒同 zone 同机房，10–20 秒同大区同机房，20–30 秒全服同机房，30 秒后再按 zone → 大区 → 全服 依次尝试跨机房。

### 3.4 哪些入口适用

| 入口 | 是否逐级放宽 |
|---|---|
| JoinQueue：PVE_TEAM 人数档 2–5、1V1、5V5 | 是 |
| StartTeamMatch 带目标人数（队伍补位） | 是，队伍作为一个单位 |
| PVE_SOLO / 1 人档、满员整队开战 | 否（没有陌生人），只做机房放置（D3） |
| 切磋、活动开战 | 否（名单固定） |
| 随机观战 | 否（观战会话 2026-10-24 确认暂不接；以后可复用 `scope` 包） |

## 4. 成组规则（`go/match/internal/poolgroup`，第一批已落）

输入一个池的一页单位（写者从 `(state,mode,config_id,team_size,team_id,enqueued_ms,player_id)` 索引读出，已过滤过期与 `not_before`），输出不共用单位、不共用玩家的若干候选组：

1. 单位按排队序（入队时刻、ID）排列，依次做锚点；每个机房阶段最多**真正展开搜索** 256 个锚点（有界）。本阶段 O(1) 就能判定不可能成组的锚点（硬延迟内没有可分配机房、首选机房无节点、未等满跨机房阈值、满员单位已在本机房阶段放置过）不占预算。
2. 满员单位直接放置（D3），只在本机房阶段处理一次。
3. 本机房阶段：候选与锚点首选机房相同，放置在该机房。跨机房阶段：锚点与候选都等满跨机房阈值；在全员可达且可分配的机房里，选组内最大 RTT 最小者，其次 RTT 之和，再次机房号。
4. 每个阶段内范围层级从 zone 到锚点允许的层级逐层尝试；候选必须同属锚点在该层的子树，且自己也已允许该层。
5. 非计分模式按排队序做精确装箱，预组队整体进出；计分模式只接受单排单位，在容差内找评分极差最小的窗口。
6. 每个机房每次尝试最多收集 64 个候选（计分模式 512 个），**先按机房可达过滤再计数**，排在前面但到不了该机房的候选不占名额。
7. 同一玩家出现在页里两个单位中（读页时单排票与整队状态不一致）时，这些单位本轮全部不参加，下一轮重读。因此输出的组之间、组内都不会有重复玩家。
8. 之前没凑成的单位可以被后来的锚点当候选，所以 `Units[0]`（锚点）不一定是组里最早入队的。

它是纯函数：挑错人只会让提交失败，不会成错局。5V5 分边仍由 gather 的蛇形分边完成（现状不变）。

## 5. 架构：每池单写者 + 一轮多局

### 5.1 组件（2026-10-30 按实现更正）

match 副本形态不变（RPC 前端 + 写者 + formation 执行者），共连同一个逻辑 TiDB。**不新增表、进程、Redis 键或内存权威**：

- 池归属：`internal/poolowner`，成员表 = etcd 里注册的 `MatchNodeService` 实例（`svc.MatchNodes` watcher）；
- 撮合循环：`logic/global_pool_matcher.go`（`StartGlobalPoolMatcher`、`claimImmediately`）；读池：`authority/pool_scan.go`；
- 机房内选点：`internal/battleplace`（两次随机取本副本在途较少者 + 节点准入拒绝后 30 秒本地熔断）；
- 模板目录：`internal/catalog`（§7）。

原设计的 `match_pool_shard` / `match_replica_heartbeat` 租约表**不做**。理由：归属只管分工、不承担正确性（§5.5），而 etcd 注册表已是全部副本看到的同一份成员表；在它上面做 HRW 不新增心跳写入，副本增减只迁移原本落在该副本上的池。代价：成员表变化的瞬间可能两个副本同时撮合同一池（只多几次提交冲突），或短暂无人负责（单人档仍有入队即时成局兜底）。

### 5.2 池归属（取代原"租约"）

- 归属键 `(模式, 模板)`，用最高随机权重哈希（rendezvous / HRW）在成员表上选主，结果只取决于成员集合。
- 本实例注册成功后才进入成员表（`SetPoolMember`），关机注销前先退出；不在成员表里的副本不负责任何池。
- 已提交成局的 `Pending` 恢复按同一归属键分给负责副本；成员表为空时全部副本都恢复（退化为旧行为）。恢复的正确性只靠 `AcquireWork` 的工作租约与行锁。
- 原计划的 `recovery_shard` 列与"提交前核对租约"的软围栏随租约表一起不做。

### 5.3 写者一轮（按实现）

1. 取本副本负责的池（`catalog.Pools()` 过滤归属）；读一次数据库时钟；可用机房 = battle 节点快照去掉本地熔断的节点。
2. 每个池读一页单排票（`GlobalMatch.PoolPageSize`，默认 512，按 `(enqueued_ms, player_id)` keyset 翻页）与至多 64 支排队整队；页里已过期的单排票每池每轮至多 64 张按票号 CAS 置 4（`ExpireTickets`）。整队成员票过期仍由队伍维护整单撤队。
3. 调 `poolgroup.Form` 得到候选组。整页一组都没成才翻到下一页，否则下一轮从队首重读，防止一页卡住的票挡住后面的人。
4. 每组先非阻塞占备战槽（`GlobalMatch.GatherSlots`，默认 256），占不到留到下一轮；本轮至多 8 个提交事务并发，给入队请求留数据库连接；整队先 `CheckParty` 复核，失效的整单撤队。
5. 一轮总预算 3 秒（间隔取 `MatcherIntervalMs`，默认 500ms）。全部提交返回后本轮才结束，下一轮读到的已是提交后的状态，所以不需要"在途票"排除集。
6. 未做：撮合前批量在线检查（改为肇事者剔除 + 票 TTL 兜底）；已禁用模板的票不主动撤（提交点拒绝、`CommitProfile` 不再续命，随 TTL 过期）；连接池拆分。

### 5.4 单人档即时成局

Join 事务提交后派一个后台任务（`claimImmediately`，3 秒预算，经 `LaunchRelayTask` 接纳）用**独立事务**立即成局，不阻塞入队回包（与 Java `tryImmediate` 同形态，不合并进 Join 事务）。失败就留在 state=1，由负责该池的写者兜底。即时成局与写者争同一张票时，只有一方在票行复核上成功。适用 PVE_SOLO 与 PVE_TEAM 1 人档；人数档 PVP 的每侧 1 人档仍要等真人对手，不走这里。

### 5.5 提交点与正确性论证

- **唯一互斥依据是 `claimTx` 的玩家锁 + 票行锁 + 代次 / 状态 CAS**（锁序不变：P 升序 → 入场守卫 → 观战 → 票行升序 FOR UPDATE）。池归属不承担正确性：两个副本同时撮合同一池时，同一玩家不可能进两局，满足 D-16"旧 leader 不能让同一玩家进两局"。
- 提交点新增复核（第二批已落，只对排队成局 `RequeueOnFailure=true` 生效；切磋、活动、整队直接开战名单固定，不受约束）：
  - 范围树：用加锁读出的票（home zone、入队时刻）与数据库时钟算 `scope.Admit`，阈值取策略值，不满足回 `ErrScopeWait`；
  - 认领方模板目录里该 `(mode, config)` 仍开放，否则回 `ErrTemplateClosed`；
  - 组内只有一个单位时豁免跨机房等待（D3）；
  - 战斗期限 = 提交时刻 + 120 秒备战预算（一代工作租约）+ 模板 `time_limit`（缺省 180 秒，上限 3600 秒）。
  - **未做**：组内评分极差复核。评分是撮合质量规则，容差配置进全局策略指纹、各副本一致，撮合时已按锚点容差检查；提交点复核只是纵深防御，列为后续。
- 成局失败只剔除**确定由自己导致**的肇事者：不在线或未进场、scene 以业务码（非 fault 码）拒绝备战、配表指纹不一致。其单排票在同一事务里置 4（`FinishFailed`）；肇事者属于整队时整队照常回队，由 `CheckParty` 决定是否撤单，绝不拆队；其余成员保留原入队时刻回队，`not_before` +1 秒。读位置失败、RPC 超时或断连、scene 故障码等结果未知的失败不定罪（`errPrepareRejected`）。
- 建房被节点准入拒绝（`battle_not_allocatable`）时，本副本把该节点熔断 30 秒；该成局原件仍按 UNKNOWN 走唯一 SQL 终局，不因"房间必未建成"提前释放（已持久 Create 原件的失败只能等终局，见 `globalAbortPrepared`）。

### 5.6 什么时候考虑内存撮合

不预先做。R3、R4 完成后按 S1 负载压测（AGENTS.md §6，与 R0 基线二维对比），下列任一持续 10 分钟以上才进入下一步：

1. 某池 `round_seconds` p99 > 0.8 × 轮间隔，队列深度持续上升，且 claim 冲突 < 5%；或实测 claim 上限 < 目标成局率 × 1.5。
2. 读页 p99 > 50ms，或读页占 match 域 TiKV 读 > 30%。
3. 热 PVP 池 `window_overflow` / 轮次 > 5%，或组内评分极差 p95 > 1.5 × 当时容差。
4. 目标上调到 2000 万 PCU 口径，且按机房拆写者后最热池仍超单写者实测上限的 50%。

先做"写者增量缓存"（只读新增尾部，每 2–5 秒全量重读兜底）；仍不够才复审内存撮合方案。

## 6. TiDB 热点治理（与 §5 同批或紧随）

- 投影 `match_ticket_region`：1 人档不写；只写 RTT ≤ 硬延迟的机房；探测刷新改差量更新。
- 票表：读者迁走后删除 `formation_id`、`(state,enqueued_ms,player_id)`、`(state,mode,team_id,…)` 三个索引。
- `match_formation`：改 NONCLUSTERED + SHARD_ROW_ID_BITS + PRE_SPLIT，新增 `(recovery_shard,state,retry_at_ms)`，与在途修复新增的列同批迁移。
- 条件项（有 KeyViz 或压测证据才做）：池索引在 `enqueued_ms` 前插入 `preferred_region`；热池按机房拆写者；投影整表退役。

## 7. 模板目录

新表 **PlayMode**（`data/schema/playmode_table.proto`）：`id, match_mode, battle_config_id, enabled, team_size_max`。初始数据：

| id | 模式 | battle_config_id | 人数上限 |
|---|---|---|---|
| 1 | PVE_SOLO | 1 | 1 |
| 2 | PVE_TEAM | 1 | 5 |
| 3 | 1V1 | 1 | — |
| 4 | 5V5 | 1 | — |
| 5 | 切磋 | 0（默认切磋规则） | — |
| 6 | PVP_TEAM（人数档真人 PVP） | 1 | 每侧 5 |

- 本表是 [configurable-templates.md](configurable-templates.md) §6 所设计 PlayMode 的最小子集，**字段编号以 `data/schema/playmode_table.proto` 为准**。以后扩 kind / rated / rule_set 等列时追加新字段号，不重排现有的 1–5。
- 只开放现在实际在用的组合：PVE 只有副本 1（与原 yaml `PveTeamSizeByConfigId {"1":5}` 一致）；PVP 暂借副本 1 的 `time_limit`（robot 冒烟就填 1）。给 PVP 建专用竞技场 Dungeon 行要客户端同步改，列为后续。
- match 启动时读表编译成不可变目录，校验失败拒启（规则写在 schema 注释里）。
- 入口：JoinQueue、StartTeamMatch（含 PVP_TEAM 整队，`TeamSizeFor(mode, config)`）、ChallengePlayer 不命中或 `enabled=false` 回 16002（PVE_TEAM 模板没有组队行时 JoinQueue 仍回原"未开放组队"码）。人数上限 = min(`team_size_max`, `Dungeon.max_team_size`, 5)，超过的表行在目录编译时就拒启。PVP_TEAM 客户端须带 `battle_config_id=1` 入队（本表第 6 行）。
- 删掉 yaml、K8s ConfigMap 与策略指纹里的 `PveTeamSizeByConfigId`：开新本、调人数不再停全部副本。
- 提交点复核认领方目录（§5.5）；`CommitProfile` 刷新路径也校验目录，禁用模板的票不能靠持续探测续命；写者对"明确禁用"的票系统撤票，对本副本不认识的模板只跳过。
- 战斗期限按 D6。

## 8. PVP

- 白名单按 §7；评分读失败时计分模式 Join 拒绝，不再静默按 1500（global_match.go:45）。
- 评分按 `(player_id, mode)` 分开存。
- 计分局禁止观战（复用 16018，观战列表排除计分局）。
- 1V1 谁在 0 队由 battle_id 决定（splitmix64 终混取一位，`sideFlip`），不再恒归排在前面的锚点；同一局重放结果不变。切磋仍是发起者在 0 队。（第二批已落）
- 评分按模式分开存、平局阈值由 `time_limit` 推导：未做，计分功能上线前补（R5）。
- 不做：组排、Glicko、赛季、5V5 分边枚举。先用"前 N 局大 K"的 Elo 加现有蛇形分边，加一个"期望胜率差"直方图，看数据再定。

## 9. 客户端可见变化（两版与客户端同批）

- 16002 新增拒绝场景：模板不在白名单 / 未开放。
- 新增两个 tip 码（导表器发号，`data/tip/Tip.xlsx`）："需要重新探测"、"没有机房满足延迟"；16004 标 fault。
- 不新增消息号与字段。逐级放宽、单人即时成局对客户端是行为变化，不是协议变化。

## 10. 不做

- 内存撮合、硬围栏、批量 IN FOR UPDATE 的 claim、Join 与 claim 合并成一个事务。
- rule_rev 池隔离、弹性人数档、Glicko、5V5 126 种分边枚举、赛季表、票据寿命与探测解耦。
- PVP 组排、跨模板 / 随机副本合池、AI 补位、战力或等级分层、开放时间列、按模式独立的延迟阈值、3V3。
- 不改 `releaseGlobalFinished`、`HandleQueryBattleCreation` 及 formation 目标失效后的解卡语义（属于"数据覆盖审计"会话）。
- 评分来源单行串行与投榜批量化：结果侧另行立项。

## 11. 分阶段落地与进度

| 阶段 | 内容 | 状态 |
|---|---|---|
| 第一批 | `scope`、`poolgroup` 两个纯函数包与测试；ZoneGroup、PlayMode 两张表的 schema 与 xlsx；本文 | 已落源码，未编译、未测试 |
| R0 | 全局路径低基数指标；robot 加 JoinQueue 压测动作；`stress_snap` 拉 :9170；`stress_summarize` 加匹配段；跑现状基线 | 部分：按池指标已落（`metrics/pool.go`：`pool_round_seconds`、`pool_owned`、`pool_page_units{mode,team_size}`、`pool_claim_total{mode,level,pass,outcome}`、`pool_wait_seconds{mode,level}`、`pool_expired_total{mode}`、`immediate_claim_total{mode,outcome}`）；robot / 压测脚本 / 基线待做 |
| R1 | 票据写真实 home zone；肇事者剔除；过期清扫；机房内按负载选节点（P2C + 熔断），删除"最小 NodeId"；整队先占槽再 claim；单人即时成局；`MatcherIntervalMs` 生效 | 第二批已落源码（2026-10-30），未编译、未测试；在线检查撤票不做（§5.3 第 6 条） |
| R2 | 模板目录接线、白名单（JoinQueue / StartTeamMatch / ChallengePlayer）、期限按模板 | 第二批已落源码，未编译、未测试；PVP 平局阈值推导挪到 R5 |
| R3 | 池归属（HRW，取代分片租约）+ 一轮多局 + 范围树接入撮合与提交点 | 第二批已落源码，未编译、未测试 |
| R4 | §6 TiDB 热点治理（含投影表 `match_ticket_region` / `team_launch_region` 退役：第二批起只写不读） | 待做，按压测证据 |
| R5 | §8 PVP 口径：评分按模式分开、平局阈值、计分局禁观战核对 | 1V1 分边已落；其余计分功能上线前 |
| R6 | battle 上报 `accepting` / `room_count`，按负载放置闭环 | 待做 |

**文件归属（2026-10-24 与并行会话约定）**：

- 「数据覆盖审计交接文档」会话在改 `authority/store.go`（formation 恢复出口、`checkWork`、formation 状态迁移）、`logic/global_match.go`（`globalDestroy` / `releaseGlobalFinished`）、`authority/entry_guard.go`。第二批要等它通知函数名与行号后按段落避开。
  - **2026-10-24 该会话已收尾**（social-match-battle#0，未编译、待 Codex；通知时本会话已结束，故写在这里）。它改过的段落，行号为 10-24 工作区：`authority/store.go` 约 779-806（`ErrAwaitingExpiredOutcome`、`awaitsExpiredOutcome`、`expiredOutcomeSQL`、`CountStrandedExpiredFormations`）、约 824-825（`AcquireWork` 的到期门禁）、约 869 起（`CreateIntentPersisted`）、约 919（`Pending` 的 SQL 加 `AND NOT expiredOutcomeSQL`）；`logic/global_match.go` 约 409-420（`formationDestroyAuthority` 接口与 `globalDestroy` 的无原件短路）、约 530-570（`globalAbortPrepared` 在 `Finish(false)` 之后计数）；`logic/gather.go` 约 177（`globalDestroy(svcCtx.Authority, …)` 调用点）；`logic/matcher.go` 约 138-139（到期巡检 `safego.Loop`）；新文件 `logic/global_destroy_test.go`、`logic/global_expired_audit.go`、`authority/formation_recovery_exit_test.go`、`authority/started_formation_exit_test.go`。`entry_guard.go`、`checkWork` 最终没有改。第二批动这些文件前先重读，按段落避开。
- 「全服观战架构设计」会话在改 `constants/errors.go`、`metrics/metrics.go`、`spectate*.go`、`watchbattlelogic.go`、`global_spectator.go`、`authority/spectator.go`、`entry_guard.go`（只加哨兵错误）、`match_authority.proto` 的 `MatchSpectatorIntentRecord`。它以后的可见性改动要碰 `gather.go` 两处，`gather.go` 由本设计负责，届时在本设计的版本上补。
- `match_authority.proto` 两边改的是不同 message；proto 生成只由 Codex 在两边落稳后统一跑一次。
- **第二批（2026-10-30）动到的他人文件**，并行会话请按段落合并：`logic/pvp_team_test.go` 两处 `newPartySvcCtx` 少接一个返回值（编译错误，改为 `s, mr, _ :=`）；`team/service.go`、`team/sql_launch.go` 的组队人数上限改为 `TeamSizeFor(mode, config)`（PVP_TEAM 也查 PlayMode 表，删掉固定 `Capacity` 分支）；`logic/challengelogic.go` 入口加 PlayMode 白名单；`logic/gather.go` 的 `errPrepareRejected`、肇事者传递、`not_allocatable` 熔断与 1V1 `sideFlip`；`logic/global_match.go` 的 `globalAbortPrepared` 改为带肇事者调用 `FinishFailed`。单测的模板目录替身统一用 `logic/catalog_fixture_test.go` 的 `useTestCatalog`。

## 12. 验证清单（交给 Codex）

Claude 未执行任何编译、测试或导表。第一批与第二批合并验证，按 §12.2 的顺序执行；§12.1 保留第一批的原始口径。

### 12.1 第一批（原清单）

1. 纯函数包（不依赖外部服务），工作目录 `go/match`：
   ```bash
   go vet ./internal/scope/... ./internal/poolgroup/...
   go test ./internal/scope/... ./internal/poolgroup/... -count=1 -v
   ```
   通过标准：全部 PASS，零 SKIP。失败时保留失败用例名与断言输出。
2. 新表：
   - 在仓库根执行 `dev.bat gen`（全量导表，会写 `generated/` 与三端部署目录）；期望：ZoneGroup、PlayMode 两表导出成功，PlayMode 的 `battle_config_id` 外键校验通过（0 视为空引用）。
   - `py tools/data_table_exporter/tools/gen_schema_index.py` 更新 `data/AGENTS.md` 的索引块。
   - **C++ 登记是必做项**：导表后生成的 `cpp/generated/table/code/all_table.cpp` 会 include 每张表的头文件并调用其 `TableManager::Load()`，不登记就会链接失败。C++ 编译前在 `cpp/generated/table/CMakeLists.txt` 与 `table.vcxproj`（含 `.filters`）按既有格式（参照 GuildActivity 的写法）登记：`code/zonegroup_table.cpp`、`code/playmode_table.cpp`、`code/playmode_table_fk.cpp`（PlayMode 带外键）、`proto/zonegroup_table.pb.cc`、`proto/playmode_table.pb.cc`，以及对应 `.h` / `_comp.h` / `_fk.h` / `.pb.h` 的 ClInclude 项；最终以导表器实际产出的文件列表为准。之后 MSBuild 串行 `/m:1` 编 table 库与链接它的节点。
   - 失败时保留导表器完整报错。
3. 通过后，第二批才开始接线（match 读表需要 `go/shared/generated/table` 里生成的 `PlayModeTable` / `ZoneGroupTable`）。

### 12.2 第二批（2026-10-30，按顺序执行）

项目未上线，玩家数据可清空（负责人 2026-10-30）；本批不需要迁移存量数据。

1. **导表与 tip 码**（仓库根）：`dev.bat gen`。期望：
   - `go/shared/generated/table` 生成 `PlayModeTableManagerInstance`、`ZoneGroupTableManagerInstance`（match 的 `internal/catalog` 直接引用）；PlayMode 第 6 行（PVP_TEAM）导出成功；
   - tip：match_error 组新增 `MatchProbeRequired`、`MatchNoRegionWithinLatency`（以及观战会话加的 `MatchSpectateTicketInvalid`），`MatchInternal`(16004) 出现在 `go/shared/generated/tip/faults.go` 与 Java 的 `TipFaults`；
   - 之后执行 `py tools/data_table_exporter/tools/gen_schema_index.py`。
2. **C++ 表登记**：同 §12.1 第 2 步（第一批若已登记则跳过），MSBuild 串行 `/m:1`。
3. **proto**（仓库根，未获客户端授权，用 no-unity 配置）：
   ```bash
   pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
   ```
   本批只改了 `proto/match/match_authority.proto`（票表池索引）与 `proto/team/team_authority.proto`（`team_launch` 池索引）的索引选项，不改字段。
4. **Go 编译与单测**（工作目录 `go/match`）：
   ```bash
   go vet ./...
   go test ./... -count=1
   ```
   通过标准：全部 PASS。重点包：`internal/scope`、`poolgroup`、`catalog`、`poolowner`、`battleplace`、`authority`、`logic`、`team`、`constants`、`config`。失败时保留失败用例名、断言输出与编译错误全文。
5. **真库回归**（隔离的一次性 TiDB，库可清空）：
   ```bash
   MATCH_IT_TIDB=127.0.0.1:<端口> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/authority/... ./internal/team/... -count=1
   ```
   重点：`TestTeamTiDBFinalHomeFiltersWholeUnitsBeforeLimit`（跨 home 整队期望 `ErrScopeWait`、换 home 期望 `ErrTeamRoute`）；`CheckCandidateIndexes` 在新索引缺失时拒启——先用 schemamigrate 按新 proto 建表。
6. **其它受影响的编译**：`go/robot`（只改注释）、`java/gateway_node`（导表器重新生成 `TipFaults` 时）；`tools/scripts/k8s_deploy.ps1` 渲染一次 match ConfigMap，核对出现 `TableDir: /generated/tables` 与 `GlobalMatch` 段的 `ScopeGroupAfterSeconds` / `ScopeGlobalAfterSeconds` / `GatherSlots` / `PoolPageSize`，且不再有 `PveTeamSizeByConfigId`。
7. **冒烟**（本机全局路径，清空 redis / mysql / etcd 后起服）：PVE_SOLO 入队即开局；PVE_TEAM 2 人档两人同 zone 成局；1V1 两人成局；不在 PlayMode 表的 `battle_config_id` 入队回 16002。机器人现有冒烟配置填 `battle_config_id=1`。

## 13. Java 版（AGENTS.md §12）

两版共享的只有客户端契约（tip 码、白名单拒绝语义）与配置表数据。Java 版需要：

- 同样的 PlayMode / ZoneGroup 表（ContractSync 同步数据），撤销 `MatchProperties.java:28-30` 的禁查表约束；
- 逐级放宽范围与双向同意（`GlobalRegionIndex` / `GlobalQueueUnits`）；
- 每池单写者 + 一轮多局（`GlobalMatchService.round()` 现为单轮单局）；
- 肇事者剔除、离线撤票推广到单排与 PVP、拒绝码按 Go 口径对齐；
- Java 已有而 Go 待追的：按 `accepting` + `roomCount` 选节点（R6）、离线撤单位（只写进本仓文档，不往 PARITY 加行）。单人即时成局 Go 第二批已追平。
- 前提：Java 全局匹配路径目前只有源码、没有编译和运行证据（match-spec.md:2418-2447），要先让它跑通。

状态：待做。PARITY.md 第 149、187 行已过期，需按当前源码更正后再登记新行。

## 14. 风险与未决

- 默认阈值（10 / 20 / 30 秒、180ms）都没有数据依据，冷门模板可能等待偏长；R0 指标出来后按模板定标。
- 双向同意让刚入队的人在冷门池里要等自己的阈值；这是"同 zone 优先"的代价。
- 未登记大区的 zone 跳过大区层；开新区要同步维护 ZoneGroup 号段。
- 单写者在 S3 量级可能不够，需要按机房拆写者（§6 条件项）。
- 本文所有吞吐数字都是推算，没有压测证据前不宣称性能提升（AGENTS.md §6）。
- 期限按模板（D6）的代价：已持久 Create 原件之后失败（含节点准入拒绝）的成局只能等唯一 SQL 终局，玩家被占用到"提交时刻 + 120 秒 + 模板时长"。缺省模板仍是 300 秒；开放长模板（如 3600 秒）前要先补"确定未建房即提前终局"的路径，否则一次建房失败会把整组人卡一小时。
- PVP_TEAM 只在 PlayMode 第 6 行（`battle_config_id=1`）开放；客户端若按 0 入队会被白名单拒绝。
- 池归属依赖 etcd 注册：成员表为空（etcd 不可用且本地快照也空）时没有副本撮合排队池，只剩单人档即时成局；`match_pool_owned` 掉到 0 要告警。
- 投影表 `match_ticket_region` / `team_launch_region` 第二批起只写不读，写放大仍在；退役归 R4。
- **共享工作区提醒**：ZoneGroup / PlayMode 的 schema 与 xlsx 已落盘。之后任何会话跑 `dev.bat gen`，都会生成这两张表的三端代码；C++ 编译前必须按 §12 第 2 步补登记，否则 table 库链接失败。

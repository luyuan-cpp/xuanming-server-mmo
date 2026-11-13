# 全服匹配：按模板分池、逐级扩大范围、每池单写者

## 人数档真人 PVP（2026-10-29）与计分组排（2026-11-05）

**模式**：`PVP_TEAM=7`，`team_size` 是每侧人数 1～5。2V2～5V5 就是 `team_size` 为 2～5 的四个独立池；JoinQueue 里那个 `MATCH_MODE_3V3` 仍未开放，3V3 走这里。PVE 仍按当前人数直接开战；PVP 即使队伍已经达到目标人数，也必须排队等另一侧的真人，不走"满队直接开战"。旧 1V1 / 5V5 编号和含义保留，5V5 仍只收单排、按蛇形分边。

**单位与分边**：单排票和整队都是不可拆的单位，可以用单人补齐同一侧，两侧人数必须相等。整队与单排票都按池隔离读出（§5.3），原件中的模式必须与请求一致。提交点原子复核名单、原票、原队伍版本和双方分边（`validatePvpTeamFormation`）；`MatchFormationMember.team_index` 持久化分边，首次开局与恢复都读同一原件，不按名单下标或蛇形评分重建。取消与失败补偿沿用原意图、墓碑和所有权规则。

**计分组排（负责人 2026-11-05 定，Java 版全部记待做）**：PVP_TEAM 从 11-05 起是计分模式，按评分凑人、按结果算 Elo。选人和分边在 `poolgroup/pvp_units.go`（`pickSides`），计分规则在 `ratingrule` 包，提交点复核在 `authority/rated_spread.go`。

1. **评分来源**：单人入队时读评分，读失败拒绝入队（与 1V1 / 5V5 相同）。整队入队时在同一个入队事务里读每个成员的评分，写进成员票的 `rating_centi`（`authority/party_rating.go`），读故障整笔回滚。撮合和提交点都只看票上的这份快照。评分按模式分开存、按模式分榜、平局阈值归 R5（回合制缺口会话）；两边 11-05 约定 PVP_TEAM 按 mode 一条评分轨道，不按每侧人数再分。
2. **队伍分**：一个单位的队伍分 = 成员评分的平均（四舍五入到 0.01 分）+ `GlobalMatch.PartyRatingBonusPerMember` ×（人数 − 1）。缺省每多一人加 25 分，5 人队加 100，代表车队配合的优势，待按车队胜率定标。队伍分只用于凑人和分边；Elo 结算仍按两队成员原分的平均，不做组队修正。
3. **凑人数**：候选已经按机房、范围树、与锚点的分差不超过锚点容差过滤，按排队序最多取 64 个（计分也是 64，不用单排计分池的 512），再按人数分桶。一局的人数组成是 2N 的一种拆分：每份不超过 N，其中一份是锚点；5V5 最多 30 种。每种组成在每个人数桶里取排队最早的单位。计分池再逐个试评分窗口：窗口宽度等于锚点容差，并且包含锚点；窗口下界取候选里落在 [锚点分 − 容差, 锚点分] 区间的每个分值，窗口里同样取排队最早的。所以组内队伍分极差不会超过锚点容差。
4. **车队对等**：两侧最大车队人数之差叫车队差。车队差不超过 1 的立即可成（双排打单人）。更大的差要**吃亏一侧**（最大车队较小的那侧）的每个单位都等够：一个单位能接受的车队差 = 1 + 等待 ÷ `GlobalMatch.PremadeGapStepSeconds`（缺省 10 秒）。按缺省值，5V5 里 3 人队打 5 个单人，要这些单人等满 10 秒；4 人队要等 20 秒，5 人队要等 30 秒。等得够久自然全放开，不会饿死。等待取单位里最短的那名成员，和范围树的双向同意一个道理：刚入队的单人不会被拉去给车队垫背。步长为 0 时不限车队差。
5. **分边**：选中的单位最多 10 个。锚点固定在 0 侧，枚举其余单位的全部分法（最多 512 种），在两侧各 N 人、满足车队对等的分法里，取车队差最小、再取两侧"队伍分 × 人数"之和相差最小的一种。
6. **先选哪一组**：先比成局时机：这组用到的最晚排队位置越靠前越好，也就是先来先成局。同一时机再依次比车队差、两侧分差、组内极差，最后按单位的排队位置定序，结果确定。不计分、也不限车队差时，成局时机和原来的二维 0-1 背包 DP 相同。
7. **提交点**：分边复核不变。新增计分排队局（`RequeueOnFailure` 为真的 1V1、5V5、PVP_TEAM）的组内队伍分极差复核：用加锁读出的票按同一公式算队伍分，极差不得超过组内最长等待者的容差（数据库时钟），否则回 `ErrRatingSpread`。撮合用的是锚点的容差，锚点的等待不超过组内最长等待，容差又随等待单调不减，所以复核只会更宽、不会误拒。复核看的是撮合用过的同一批原票，正常不会触发；一旦触发，说明撮合与提交点的规则或配置不一致，`claimGroup` 记一条 Error 日志（带模式、模板与对局号）。车队对等和分边平衡是质量规则，提交点不复核。三个配置项（组队加成、车队差步长、组队成员评分差上限）显式写在 `go/match/etc/match_service.yaml` 的 `GlobalMatch` 段，K8s ConfigMap 从这里镜像。
8. **指纹**：计分模式集合与组队加成进全局策略指纹（提交点要用）；车队差步长不进。
9. **每侧 5 人不收 4 人队**（负责人 2026-11-05 定，借鉴 LoL 灵活组排）：PVP_TEAM 每侧 5 人的池不接受 4 人队伍开战，免得一个单人被塞进 4 黑；4 人队可以打每侧 4 人的池，或者等第 5 人进队。StartTeamMatch 入口先判（`team.CheckPvpPartySize`），开战原件的提交事务里按锁住的名单再判一次，拒绝回 `TeamPvpPartySizeNotAllowed`。单人入队和其他人数档不受影响。
10. **组队成员评分差上限**（负责人 2026-11-05 定，借鉴 LoL 排位的组队段位限制，防止高分号带低分号刷分）：计分模式的整队开战，在开战原件的同一个事务里按本模式读每个成员的评分，最高与最低之差超过 `GlobalMatch.PartyRatingGapMax`（缺省 500 分，0 = 不限）就整笔回滚，回 `TeamRatingGapTooLarge`，名册和开战锁都不动（`authority.RequirePartyRatingGapTx`）。只在开战时判这一次：已经排上队的队伍不会因为之后评分变化被撤，排队期间队员也打不了别的局。上限是提交点规则，进全局策略指纹。

第 9、10 条的两个码在 `Tip.xlsx` 的 `//team_error` 组末尾追加，由导表器发号，都是业务拒绝（`fault` 留空）；客户端按 id 查文案，不需要新消息或字段。

单侧 1 人的 PVP_TEAM 池也计分，与 1V1 是两条评分轨道。

**未做**：

- Elo 的组队修正：车队赢了照常按两队平均分加分。
- 5V5（`MATCH_MODE_5V5`）的组排：仍只收单排。

本节是 11-05 的新契约。纯单测已由 Codex 跑过，真库回归与冒烟还没跑；验证步骤见 §12.5，逐项状态见 §11 的 R7 行，实际状态以 `docs/PROGRESS.md` 为准。客户端改动见 §9。

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
| D7 | PVP：每个计分模式一行白名单；1V1 与 5V5 分开计分；计分局禁止观战（复用 16018）；赛季首发不做（「停写 → 投完 → 封榜」的编排没有生产调用方；2026-11-06 起停写不再卡住结果消费，见 §8 R5「迟到与重复」） | 客户端可任填 config 建私有池刷分；评分全服一份；计分局可实时观战 |
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
| 评分（1V1 / 5V5 / PVP_TEAM） | 容差曲线随锚点等待放宽 | 组内队伍分极差 ≤ 锚点容差；提交点按组内最长等待者的容差复核 |
| 车队差（仅 PVP_TEAM） | 能接受的差 = 1 + 等待 ÷ 步长，看吃亏一侧每个单位 | 质量规则，提交点不复核 |

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
5. 非计分模式按排队序做精确装箱，预组队整体进出；计分且不分边（1V1 / 5V5）只接受单排单位，在容差内找评分极差最小的窗口；分边模式（PVP_TEAM）按开头「计分组排」第 3～6 条选人和分边。
6. 每个机房每次尝试最多收集 64 个候选（1V1 / 5V5 是 512 个），**先按机房可达过滤再计数**，排在前面但到不了该机房的候选不占名额。
7. 同一玩家出现在页里两个单位中（读页时单排票与整队状态不一致）时，这些单位本轮全部不参加，下一轮重读。因此输出的组之间、组内都不会有重复玩家。
8. 之前没凑成的单位可以被后来的锚点当候选，所以 `Units[0]`（锚点）不一定是组里最早入队的。

它是纯函数：挑错人只会让提交失败，不会成错局。5V5 分边仍由 gather 的蛇形分边完成；PVP_TEAM 的分边在这里算好，写进成局原件。

## 5. 架构：每池单写者 + 一轮多局

### 5.1 组件（2026-10-30 按实现更正）

match 副本形态不变（RPC 前端 + 写者 + formation 执行者），共连同一个逻辑 TiDB。**不新增表、进程、Redis 键或内存权威**：

- 池归属：`internal/poolowner`，成员表 = etcd 里注册的 `MatchNodeService` 实例（`svc.MatchNodes` watcher）；
- 撮合循环：`logic/global_pool_matcher.go`（`StartGlobalPoolMatcher`、`claimImmediately`）；读池：`authority/pool_scan.go`；
- 机房内选点：`internal/battleplace`（随机两选一取负载较低者；节点自报 CLOSED 不选；节点准入拒绝后 30 秒本地熔断。负载与可分配状态的来源见下文 R6）；
- 模板目录：`internal/catalog`（§7）。

原设计的 `match_pool_shard` / `match_replica_heartbeat` 租约表**不做**。理由：归属只管分工、不承担正确性（§5.5），而 etcd 注册表已是全部副本看到的同一份成员表；在它上面做 HRW 不新增心跳写入，副本增减只迁移原本落在该副本上的池。代价：成员表变化的瞬间可能两个副本同时撮合同一池（只多几次提交冲突），或短暂无人负责（单人档仍有入队即时成局兜底）。

**R6：可分配状态与负载（2026-11-05 落码，未编译）。** 不照搬 Java 版的周期写（Java 的 battle 周期把 accepting / roomCount 写进 Redis 目录）：周期写的写放大与读量随进程数平方增长（`docs/handoff/server-gap-audit-20261101.md` 第 4 条）。

- **可分配状态** `NodeInfo.admission`（`proto/common/base/common.proto` 字段 14，只有 battle 填）：etcd 首发前填 CLOSED；建房准入闸打开且 Agones 会放行许可（未排空、不在 Starting / ShuttingDown / Stopped）后改 OPEN；排空、停机、维护关闸改回 CLOSED。battle 的 loop 上 1 秒一拍，比对期望值与 **watch 回读到的自身登记**（battle 本来就 watch 自己的前缀做劫持检测与注册自检），不一致且注册链空闲才 `EtcdManager::UpdateNodeInfo()`；同一个值写出 5 秒后回读仍不一致就补写。稳态零写入；生成的 etcd 客户端在 RPC 失败时不回调，丢掉的 Put 靠回读对账自愈。回读看不到自己的键、租约未确认有效、在重注册或有在途注册 CAS 时不写，因为此刻 Put 会抢在注册链的 `PutIfAbsent` 之前建出服务键、让框架重新分配 node_id；这期间期望值只留在内存的 NodeInfo 里，由首发 / 重注册整份带出去。停机关闸当场再对账一次。实现：`cpp/nodes/battle/battle_admission_advertise.h`（纯决策，单测在 `cpp/nodes/battle/tests/battle_admission_gate_test.cpp`）、`cpp/nodes/battle/main.cpp`。
- **负载**：房间数随 `CreateBattleResponse.room_count`（`proto/battle/battle_node.proto` 字段 5）捎回，不另发流量，也不进注册信息。match 在每个 CreateBattle 成功应答（`logic/gather.go` 开局、`logic/global_match.go` 恢复重放）调 `Placement.Observe`。
- **match 选点**：CLOSED 不可选（`Regions`、`Pick` 都排除）。UNSPECIFIED 视为可选，这是有意的 fail-open：新版 battle 首发前就填 CLOSED，UNSPECIFIED 只来自旧版本；滚动升级时把旧节点判成不可选会让整个机房没有节点、排队成局全部失败；误选的代价有界（battle 在任何副作用之前以 `battle_not_allocatable` 拒绝，本副本熔断它）。负载 = 60 秒内的上报房间数 + 本副本在途；读数过期按未知处理、只看在途，否则一次旧的高读数会让节点一直被避开、读数永远等不到刷新。代价：没有读数的节点显得更闲，会先被多选几次（选中即刷新读数）；混版本期间不上报的旧节点一直显得更闲。
- **指标** `match_battle_nodes{state}`（open / unspecified / closed / cooldown / invalid），每个撮合轮刷新，不负责任何池的副本也报。
- **「可分配」判定全仓一份**：`battle_admission::AcceptsNewRooms`（准入闸为 kOpen 且 `BattleNodeImpl::LifecycleGrantsPermits()`）。§5.5 落库前预检 `rejects_new_battles` 的 battle 侧填值复用它，不另写一份：`BattleNodeImpl::HandleQueryBattleCreation`（`cpp/nodes/battle/handler/grpc/battle_node.cpp` 守护段）读每秒对账写进内存 `NodeInfo.admission` 的期望值，`admission != OPEN` 即为真。这个函数是静态的、拿不到准入闸，所以读已算好的值，代价是最多晚一个对账周期（1 秒）。

### 5.2 池归属（取代原"租约"）

- 归属键是撮合段的键（2026-11-12 起，见本节末「热池拆分」与 §5.2.1；原先一个 `(模式, 模板)` 一个键，编码为 `模式:模板`），用最高随机权重哈希（rendezvous / HRW）在成员表上选主。`internal/poolowner.Owner/Owns` 委托 `shared/workshard`，与 player_inbox、trade 共用打分实现：`SHA-256(成员 + NUL 字节 + 键)` 的前 8 字节按大端序取 `uint64`，分数最高者负责，同分取字典序较小的成员。结果与成员顺序无关；各服务仍各自维护成员表。
- 2026-11-06 替换旧 FNV-1a 打分，修复短数字后缀的归属集中。此次算法切换会重新分配池与后台分片；项目尚未上线，归属不写入持久数据，无需数据迁移。固定新算法后，增成员只把部分键交给新成员，减成员只重分配原属该成员的键；固定样本的分散回归不承诺任意成员集合、任意键都均匀分配。成员生命周期以及未同步、空表时的处理保持下述规则。
- 本实例注册成功后才进入成员表（`SetPoolMember`），关机注销前先退出。
- **一条归属规则管全部后台工作（2026-11-06，未编译；缺口清单第 24 条）**：`svc.OwnsWork(key)`，撮合池与下面几项按片分工的工作共用。成员表为空（watch 还没同步，或 etcd 一直起不来）时全部副本都做，保活优先；成员表非空、但本实例不在其中时什么都不负责。归属只管分工，正确性都在各自的 SQL 提交点。
  - 按片分工的工作（`authority/work_owner.go`，`WorkShards = 64`，片号 = 业务实体 id 取模；发号器的 id 已打散，取模足够均匀；只决定分工，不进主键，改它不用迁移）：
    - **成局推进**（`Pending`）：片号 = `battle_id`，条件在 SQL 里、`LIMIT 32` 之前。原先全局先取 32 条再在内存里按 (模式, 模板) 过滤，一个副本备战槽满了，它的到期成局占满队头，别的副本看不到自己的。
    - **整队维护**（`TeamLaunchesAfter` 的 prepared / queued / forming / completed 四个循环）：片号 = 发起者 `actor_id`，这一列在扫描索引里，不用回表。原先每个副本每 500ms 给每支排队整队开一个锁全员票的事务。
    - **闲置队伍到期**：片号 = `team_id`。
    - **投榜**（`PublishRankOnce`）：每个 (计分模式, 来源片) 一个键。原先每个副本串行投全部 64 片；两个副本偶尔同投一片也安全，rank 按 (榜, 片, 游标) 幂等。
    - **已建房票巡检**（`ReadyAfter`）：同类问题，每个副本都扫全部 state=3 的票、逐张开事务。片号 = `player_id`，键 `workReadyTicket`。2026-11-10 已在 `ready_recovery.go` 的 `ReadyAfter` 里接上（数据覆盖审计批 B5，未编译）：条件在 `ORDER BY` 之前，片参数追加在绑定参数末尾；没绑归属时条件为 `1=1`。这条巡检只是兜底，终局发件箱和 `JoinQueue` 的按需路径照样会释放票，所以某片暂时没有副本负责也不会漏放。回归用例 `TestReadyTiDBReadyAfterReadsOnlyOwnedShards`。
    - **终局发件箱**（`PublishBattleOutcomes`，2026-11-10 落码，未编译；缺口清单第 25 条）：整局结果（`match_battle_completion`，一局一行、没有 `player_id` 列）片号 = `battle_id`，键 `outcome-result`；个人结算（`match_battle_settlement_source`，首投与等待两页共用）片号 = `player_id`，键 `outcome-settlement`。条件在 `LIMIT` 之前，分片列都在扫描索引里。原先每个副本每 2 秒读同一个到期队头、在 `next_retry_ms` 认领 CAS 上空撞。积压读数与按票的 `SettlementLookupDue` 不分片。设计与验证见 [assetop-redelivery-sharding-20261110.md](assetop-redelivery-sharding-20261110.md) §2.5、§8。
  - **不在这条规则里**：到期终局恢复（`RecoverExpiredOutcomeLane`）由数据覆盖审计会话按车道轮转，所有副本都跑，同一成局靠行锁只写一份终局（两套做法互不依赖）。
- 原计划的 `recovery_shard` 列与"提交前核对租约"的软围栏随租约表一起不做；成局推进改用 `battle_id` 取模，不需要新列。
- **热池拆分（2026-11-12 落码，未生成、未编译；缺口清单第 24 条）**：一个池不再只归一个副本。本机房阶段按首选机房拆成机房段，热门模板再按玩家拆成 K 个子池段；跨机房阶段每个池一个跨机房段。每段一个归属键，照旧走 `svc.OwnsWork`。两级都只改「谁读哪一段票」，提交点（`claimTx` 的玩家锁、票行锁与 CAS）不变，§5.5 的正确性论证不变。细化见 §5.2.1，验证见 §12.8。
  - 方向由「缺口清单压测项：统一压测方案与定案」会话 11-11 定（负责人当天要求不等压测、现在定案）：本机房阶段本来就只把首选机房相同的人组在一起（§3.3），按机房拆不损匹配质量，多机房部署时这一阶段的吞吐随机房数扩；单机房部署只按机房拆仍是单写者，不满足「加机器就涨吞吐」，所以热门模板再分 K 个子池，代价是前几秒候选人少 K 倍，只落在配置了 K > 1 的模板上。
  - 细化时与方向稿不同的两处（理由见 §5.2.1 第 5、6 条）：子池号不落列，按 `player_id`（整队取发起者）取模、在索引项上过滤；首选机房列在探测刷新时跟着新原件重算，不是入队后就不再变。
  - 没选的做法：只调大单写者的提交并发（现在 8）是纵向扩，不随机器数涨，只作调参；内存撮合仍按 §5.6 的条件另议。

#### 5.2.1 热池拆分细化（2026-11-12，未生成、未编译）

**1. 撮合段与归属键**（`internal/poolowner/segments.go`：`Segments`、`Segment.Key`、`Segment.Window`）

| 段（指标 role） | 归属键 | 枚举 | 读哪些排队单位 | 跑哪些阶段（`poolgroup.Passes`） |
|---|---|---|---|---|
| 整池段 `pool` | `pool:模式:模板:人数` | 每组只有 1 人的池（PVE_SOLO、PVE_TEAM 1 人档） | 全池 | 先本机房、再跨机房（与拆分前相同） |
| 机房段 `region` | `pool:模式:模板:人数:region:机房` | 其余每个池 × 机房列表里每个机房 | 首选机房列等于该机房；模板配了子池（K > 1）时只读等满合并时长的 | 只跑本机房阶段 |
| 子池段 `sub` | `pool:模式:模板:人数:region:机房:sub:k` | K > 1 的模板：每个池 × 每个机房 × k = 0..K−1 | 首选机房列等于该机房、子池号为 k、入队不满合并时长 | 只跑本机房阶段 |
| 跨机房段 `cross` | `pool:模式:模板:人数:cross` | 每个拆开的池一个 | 不分机房，等满 `CrossRegionWaitSeconds` 的 | 先跑本机房阶段占位（不提交），再跑跨机房阶段，只提交跨机房阶段的组 |

- 每组 1 人的池不拆：组里没有陌生人，写者只是入队即时成局（§5.4）的兜底，即时成局本来就分散在处理入队请求的各副本上。按机房拆反而有坑：首选机房的节点全部下线时没有机房段会读这些票，满员单位「首选机房没节点就立即换机房」（D3）就没人做了。
- 归属键带上人数档。原来按 `(模式, 模板)` 归属，同一模板的各人数档在一个副本上，理由是「为以后跨档合池留接口」（§2）；跨档合池在 §10 里不做，热池又常常就是一个模板的一个人数档，按模板归属会把这个模板的各档压在同一个副本上。`catalog.OwnerKey` 随之删掉。
- 键都以 `pool:` 开头，与 `authority/work_owner.go` 的后台工作键（`formation:`、`team-launch:` 等）分开。

**2. 机房列表**：本副本 stable battle 节点镜像（`BattleNodes`，etcd `BattleNodeService` 注册）里出现的 `physical_region`（非 0），不看可分配状态，也不扣本地熔断。

- 各副本 watch 同一个前缀，看到的是同一份注册信息，与成员表同口径；watch 延迟期间短暂不一致，只会让某一段这一拍没人做或两个副本都做。
- 不用可分配机房（`Placement.Regions`）枚举：它扣掉了本副本的本地熔断，各副本不同；负责某机房段的副本恰好熔断了那个机房的节点，就会整段跳过。成组时照旧只往可分配机房放（`poolgroup.Form` 的 regions 参数）。
- 探测计划（`GetNetworkProbePlan`）从同一份镜像取目标机房，所以票上的首选机房都在列表里。例外是那个机房的节点之后全部下线：它的单位本来就成不了本机房组（本机房阶段要求首选机房可分配，§4 第 3 条），等满跨机房阈值后由跨机房段放到别的机房，与拆分前相同。整队成员首选机房不一致时首选机房列是 0，同理只有跨机房段读它（这样的单位本来也只能参加跨机房阶段，见 `poolgroup.Unit.Preferred`）。
- 列表为空（还没有 battle 节点注册）时只剩跨机房段；这时本来就无处放置。

**3. 两个阶段的切分**：记 W = `CrossRegionWaitSeconds`，M = `SubPoolMergeSeconds`，K = 模板的子池数，now = 本轮读一次的数据库时钟。

- 机房段：K = 1 读首选机房为 R 的全部单位；K > 1 只读 `enqueued_ms ≤ now − M`。子池段：读首选机房为 R、子池号为 k、`enqueued_ms > now − M`。跨机房段：读 `enqueued_ms ≤ now − W`，不分机房。
- 机房段与子池段按入队时刻分开，只在两个副本的本轮时钟不同时，边界上重叠几百毫秒。跨机房段读的老票也在机房段的页里，两段可能同时选中同一张票，由提交点裁决（§5.5），输的一方回滚，多一次冲突。
- 跨机房段先在自己的页上照原顺序跑一遍本机房阶段，成的组不提交，只是把能在首选机房凑成组的老票让给机房段（`poolgroup.PassesCross`），再在剩下的单位上跑跨机房阶段。这保留了拆分前「同一页先本机房、再跨机房」的优先级：跨机房段只提交拆分前也只能跨机房成的组。
- 仍会撞车的情形，以及为什么不另加机制（2026-11-12 定案）：老票 A 只能和机房段页里的新票凑本机房组，同时跨机房段能把 A 和别的机房的老票凑跨机房组。这要求两种机会**在同一轮里同时出现**：A 若早就有本机房的机会，机房段在 A 等满 W 之前就把它组走了；A 若早就有跨机房的机会，跨机房段在它等满 W 的那一轮就组走了。所以只有「新票刚到」与「另一个机房刚有票等满 W」落在同一轮时才撞，谁先提交谁成局、输的一方回滚，不会成错局。拆分前的单写者遇到同一轮的两种机会会先选本机房；拆分后是先提交者赢，只差在这一种同轮巧合上。为它在跨机房段里再读各机房的新票、或给老票加预留写，都会把读量或写量加回去，不值得。撞车的比例看 G4 表（第 8 条）。
- 新票（等待 < W）在跨机房阶段本来就当不了锚点、也当不了候选（§4 第 3 条），跨机房段不读它们不丢任何组。
- 合并时长必须小于 W（配置校验），所以进入跨机房阶段的票都已经回到机房段。

**4. 跨机房段的翻页**：沿用 §5.3 第 2、3 条，按 `(enqueued_ms, player_id)` keyset 翻页，走原池索引 `(state,mode,config_id,team_size,team_id,enqueued_ms,player_id)`，条件是 `enqueued_ms ≤ now − W` 加 `(enqueued_ms, player_id) > 游标`。整页一组都没成才翻到下一页，读到区间尾或成了组就回队首。区间上界每轮随时钟前移，新等满 W 的票接在尾部，不用另记位置。整队照旧从队首读至多 64 支，条件同样是 `enqueued_ms ≤ now − W`：成员票与整队原件是同一个入队时刻（`EnqueueTeamLaunchTx`），单位的等待就是原件的等待。每段各有一个游标，只在本副本内存里，只用于调度。

**5. 子池**

- 配置：`GlobalMatch.SubPools` 按模板列出 `{Mode, ConfigId, Count}`，缺省为空 = 全部不拆；Count 1..64（1 = 不拆），同一模板只能写一行，只能写有多人池的模式（PVE_TEAM、1V1、5V5、PVP_TEAM）。合并时长 `GlobalMatch.SubPoolMergeSeconds` 缺省 5 秒，配了 K > 1 时须 ≥ 1 且小于 `CrossRegionWaitSeconds`。越界在加载配置时拒启；写了模板目录里没有的模板只打 ERROR、不拒启（它不起作用）。
- 都不是提交点规则，不进全局策略指纹。滚动改配置时各副本的 K 可能短暂不同，只影响子池段的枚举：某个子池号一时没人读，这部分新票等满 M 后由机房段接着撮合，不会卡住。
- 放 yaml 不放 PlayMode 表：K 和 M 是按负载调的调度参数；PlayMode 表管的是开放哪些入口、几个人，属于玩法数据。
- 子池号 = 单排票的 `player_id`（整队取发起者 `actor_id`）对 K 取模，读页时在索引项上过滤（`MOD(player_id, K) = k`），**不落列**。与方向稿不同：它是主键的函数，存一份就是第二份真相（AGENTS §11.2 DRY）；不落列时改 K 也不用改数据。player_id 由发号器打散（AGENTS §7 不变量 1），取模足够均匀，与 `work_owner.go` 的 64 片同一个理由。
- 代价：入队后的前 M 秒只能在 1/K 的新票里凑组；范围树前 10 秒只许同 zone（§3.2），这 M 秒里同 zone 的候选少 K 倍。配 K 的只该是候选本来就多的热门模板。
- 只拆本机房阶段：跨机房阶段在 W 之后，早过了合并时长。

**6. 存储**

- 票表 `match_authority_ticket` 加字段 20、整队原件 `team_launch` 加字段 18，列名都是 `pool_region`（首选机房，uint32）。11-05 退役的票字段 17 `preferred_region` 仍是 reserved、不复用，两张投影表也不复活：新列不是投影副本，就在原件那一行上。
- 单排票：`Join` 入队时按刚落库的探测原件算（`PreferredRegion(profile, now)`，与撮合、提交点同一个函数）；`CommitProfile` 刷新排队票的探测原件时，同一条 UPDATE 按新原件重算。
  - 与方向稿「之后探测变了不改列」不同，理由：撮合与提交点判本机房都按当前原件现算的首选机房（§4、§5.5）。票的过期时刻就是回执里最早的过期时刻（`CommitNetworkProbe`），回执在票的有效期内全部有效，所以现算值只在原件被刷新时才变。列若不跟着改，首选机房变了的票会留在旧机房段的页里，和那一页的人都凑不成本机房组，只能等跨机房段；而客户端是在探测快过期时才重新探测的（robot 的 `networkProbeRefreshMargin`），这些恰恰是已经等得最久的票。跟着改只是同一条 UPDATE 多写一列，不多一次写入，列与原件始终一致，没有第二份要对账的副本。
- 整队：`EnqueueTeamLaunchTx` 把原件改成排队态（state=2）的同一条 UPDATE 写入成员首选机房的共同值，成员不一致写 0（与撮合输入 `poolUnitInput` 的 Preferred 同一规则）。整队成员的探测原件入队后固定、不随单人探测刷新（`commitProfileOnce` 只刷 team_id=0 的票），所以值不再变。成员票上的这一列不写：单排读页只读 team_id=0 的票。
- 回队（失败回队 `FinishFailed`、整队维护回队）不碰这一列：原件没变，值仍然对。直接开局（`reserveGroupTx`、`direct_admission.go`）的票在同一个事务里就被认领，从不以排队态可见，不写。
- 这一列只给读页用：Go 侧读票的 `ticketColumns` / `teamLaunchColumns` 不带它（读出来恒为 0），提交点不读它；它也不是占用事实，不进 `TicketOccupiedSQL`。
- 新索引，追加在原有索引之后（已有索引名不变）：
  - 票：`(state,mode,config_id,team_size,team_id,pool_region,enqueued_ms,player_id)`；
  - 整队：`(state,mode,config_id,target_size,pool_region,enqueued_ms,actor_id,request_high,request_low)`。
  - 首选机房在等值前缀里，机房段顺着索引按入队序读。子池号不做前缀：做了前缀，机房段读一个机房的全部子池就要 K 路归并。子池段的 `MOD(player_id, K) = k` 在索引项上过滤（player_id / actor_id 本身是索引列），回表只取命中的行，代价是每轮最多扫「页大小 × K」个索引项，而且只扫入队不满 M 秒的那一截。
  - 原池索引保留，给整池段与跨机房段用。同批删掉票表两条没有读者的二级索引（§6），票表的二级索引从 4 条变 3 条，排队态每变一次少维护一个索引项。
  - `CheckCandidateIndexes` 核两条新索引，缺了拒启。

**7. 提交点**：不变。`claimTx` 的玩家锁、票行锁与代次 / 状态 CAS，跨机房等待复核（`allPreferred`），`ClaimTeamUnits` 的机房与跨区等待校验，都按加锁读出的原票和数据库时钟现算，不读 `pool_region`。§5.5 的论证照旧成立：撮合段只决定谁读哪段票，两个段（或两个副本）同时选中同一张票时，只有一方的票行复核成功。

唯一的配套改动在错误分类：票行复核失败（票已被别的段认领、撤单、回队退避、过期、探测原件刷新，或放置机房已不在硬延迟内）原来回一个普通错误，撮合侧当成结局未知，记一次排空 HOLD（`HoldRelay`）。拆分后跨机房段和机房段撞车是常态，这样每次排空都会停在 HOLD。改为哨兵 `authority.ErrClaimStale`（文案不变），归进 `expectedClaimRejection`：事务在提交之前就回滚了，什么都没写，结局是已知的。

**8. 指标**

- `match_pool_owned{role}`：本副本负责的撮合段数，role 取 pool / region / sub / cross。告警 `MatchPoolsUnowned`（`sum(match_pool_owned) == 0`）不用改；多副本冒烟的核对口径从「之和 = 池数」改成「之和 = 段数」。
- 新增 `match_pool_segment_claim_total{role,outcome}`：按段角色计提交结果，outcome 与 `match_pool_claim_total` 相同。看冲突落在哪类段：cross 段的 conflict 偏高，说明跨机房段与机房段撞车多（第 3 条）。`stress_summarize.ps1` 的 G4 表按 role 出这两项（2026-11-12，钉名测试 `TestSegmentRolesMatchStressSummarize`）。
- `match_pool_page_units{mode,team_size}` 改成本副本各本机房段（pool / region / sub）读到的可撮合单位数之和；跨机房段的页与机房段重叠，不计入。本轮不再负责的标签置 0。
- `match_pool_round_seconds` 不变，一轮包含本副本的全部段。
- 压测复核（[stress-gap-items-verification-20261111.md](../stress/stress-gap-items-verification-20261111.md) 第 24 条）照旧看 G1 的 rd/int 与 conflict%；G1 的 owned 列变成段数。

**9. 混版本与上线前**：项目未上线，玩家数据可清空（负责人 2026-10-30）。

- 新表结构清库重建：旧库缺 `pool_region` 时迁移器会补列（值为 0），但不会给已有表建新索引，`CheckCandidateIndexes` 拒启；照惯例清库后 `match -f <配置> -migrate`。
- 新旧副本不混跑。真混跑了也不会成错局，只会变慢：旧副本不写 `pool_region`，它入队的票都在 0 上，只有跨机房段读得到，要等满 W；旧副本按 `(模式, 模板)` 归属、新副本按段归属，同一个池可能被两边同时撮合，多出提交冲突。

**10. 定案（2026-11-12，负责人要求不留「以后再定」）**

- 写者增量缓存与内存撮合：不做，见 §5.6。
- K 与合并时长：缺省 K = 1（不拆）、M = 5 秒。哪个模板要拆，由运维看 G1 的 rd/int 与 G4 表在 `GlobalMatch.SubPools` 里配，改配置滚动生效、不改代码。这是调度参数，没有对比表之前不宣称吞吐提升（AGENTS §6）。
- 跨机房段与机房段的撞车：不另加机制，推导见第 3 条；由提交点裁决，G4 表看比例。
- 顺带改掉的两处（§6）：票表删两条没有读者的二级索引；整队读页从 `mode IN(5,0)` 改成 `mode=?`。
- Java 版待做，只登记 PARITY（§13）。

### 5.3 写者一轮（按实现）

1. 取本副本负责的撮合段（`poolowner.Segments` 枚举，`svc.OwnsWork` 过滤归属，§5.2.1）；读一次数据库时钟；可用机房 = battle 节点快照去掉本地熔断的节点。
2. 每段读一页单排票（`GlobalMatch.PoolPageSize`，默认 512，按 `(enqueued_ms, player_id)` keyset 翻页，读哪一段见 §5.2.1 第 3 条）与至多 64 支排队整队；页里已过期的单排票每段每轮至多 64 张按票号 CAS 置 4（`ExpireTickets`，两段读到同一张时只有一边命中）。整队成员票过期仍由队伍维护整单撤队。
3. 调 `poolgroup.Form` 得到候选组（机房段、子池段只跑本机房阶段，跨机房段只提交跨机房阶段的组）。整页一组都没成才翻到下一页，否则下一轮从队首重读，防止一页卡住的票挡住后面的人；每段各有一个游标。
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
  - 计分排队局的组内队伍分极差（2026-11-05）：用加锁读出的票按 `ratingrule.UnitCenti` 算每个单位的队伍分，极差不得超过组内最长等待者的容差，否则回 `ErrRatingSpread`（开头「计分组排」第 7 条）。撮合已经按锚点容差挑过，这道复核是纵深防御：撮合和提交点各自按同一份规则判，规则参数进策略指纹。
- 成局失败只剔除**确定由自己导致**的肇事者：不在线或未进场、scene 以业务码（非 fault 码）拒绝备战、配表指纹不一致。其单排票在同一事务里置 4（`FinishFailed`）；肇事者属于整队时整队照常回队，由 `CheckParty` 决定是否撤单，绝不拆队；其余成员保留原入队时刻回队，`not_before` +1 秒。读位置失败、RPC 超时或断连、scene 故障码等结果未知的失败不定罪（`errPrepareRejected`）。
- 建房被节点准入拒绝（`battle_not_allocatable`）时，本副本把该节点熔断 30 秒；该成局原件仍按 UNKNOWN 走唯一 SQL 终局，不因"房间必未建成"提前释放（已持久 Create 原件的失败只能等终局，见 `globalAbortPrepared`）。
- **落库前预检（2026-11-05）**：gather 落 Create 原件之前本来就要调一次 `QueryBattleCreation` 核终局协议能力，应答新增 `rejects_new_battles`（节点此刻会拒绝新建房：准入闸未开或已关、Agones 排空、正在停机）。为真时本局干净失败：原件还没落库，`globalDestroy` 走无原件短路，全员保留原入队时刻回队，并熔断该节点 30 秒。它只收窄"选中节点之后才开始排空"的窗口，CreateBattle 的三道准入仍是唯一判定；缺省 false，旧节点照旧。battle 侧填值复用 R6 的「可分配」判定（全仓只留一份），2026-11-05 已随 R6 落码（未编译）：回显了本实例时按内存 `NodeInfo.admission != OPEN` 填，目标实例不符的空回包不填（§5.1 R6 段）。
- 根治"已落原件、确定没建房还要等到期"的路径归数据覆盖审计会话（2026-11-05 接手评估）：过了原件的 `AdmitBeforeMs` 加时钟余量、且同一目标实例回 ABSENT / ABORTED 时提前判 ABORT。终局协议的规则以那边的落码为准。

### 5.6 内存撮合与写者增量缓存（2026-11-12 定案：都不做）

D10 定的是原位改造、不做内存撮合，只在压测到顶时再评估；11-12 热池拆分（§5.2.1）落码，负责人要求不等压测、按标准做法定案，这两条路都定为不做：

- **写者增量缓存**（只读新增尾部，每 2–5 秒全量重读兜底）：是单写者的纵向优化，还要多一份和库对账的内存状态。拆分之后每段只读自己那一段，读页量与提交并发都随段数扩。
- **内存撮合**：同 D10。权威仍在 SQL 提交点，撮合进程不持有票，副本可以随时增减。

原来进入「下一步」的几条信号，改作调参与扩容的信号（看 G1 / G4 表，按 AGENTS §6 的流程压测）：

1. 某段 `round_seconds` p99 > 0.8 × 轮间隔、队列深度持续上升且冲突 < 5%：给这个模板调大 K（`GlobalMatch.SubPools`），或加 match 副本让段分得更散。
2. 读页 p99 > 50ms，或读页占 match 域 TiKV 读 > 30%：先核读页走的是不是按首选机房的索引（§5.2.1 第 6 条），再调页大小。
3. 热 PVP 池 `window_overflow` / 轮次 > 5%，或组内评分极差 p95 > 1.5 × 当时容差：是匹配质量问题，调容差曲线，不靠写者扩容。
4. cross 段 conflict% 持续偏高：先核是不是同一轮里同时出现本机房与跨机房两种机会（§5.2.1 第 3 条，正常应很少）。

## 6. TiDB 热点治理（与 §5 同批或紧随）

- **机房投影已退役（2026-11-05，未编译）**：`match_ticket_region`、`team_launch_region` 两张表连同写入（入队、成局、回队、取消、整队排队与维护）、只读投影的候选函数、投影修复函数，以及票上只为投影服务的 `preferred_region` 列（proto 字段 17 改 `reserved`）一并删除。第二批起撮合只读池索引，投影只写不读，留着等于每次入队、成局、回队、取消多写最多 16 行、多一套要保持一致的副本（AGENTS §11.5 第 3 条）。首选机房由撮合与提交点从 `network_profile` 现算（`authority.PreferredRegion`）。原先夹在整队投影写入里的整队原件校验（人数一致、票上 home 与原件一致、`requireTeamHomeGroupTx`）改为 `requireQueuedTeamGroupTx`，排队、维护、失败回队三处照旧调用。占用判定 `shared/matchentry` 去掉这一列，其余 14 项不变。旧库里的两张表与列迁移器不会删，清库重建（未上线，负责人已允许清空）。
- **票表二级索引（2026-11-12，未编译）**：删掉没有读者的 `formation_id` 与 `(state,mode,team_id,enqueued_ms,player_id)` 两条（全仓核过：票表的 UPDATE / DELETE 都按主键 `player_id`，不按主键的读只有撮合读页与已建房票巡检）。`(state,enqueued_ms,player_id)` 保留：已建房票巡检 `ReadyAfter` 按它给 state=3 的票翻页。加上 §5.2.1 新增的按首选机房索引，票表二级索引从 4 条变 3 条，排队态每变一次少维护一个索引项。索引名按位置生成（`idx_<表>_<序号>`），删了中间两条后面的序号会变，没有代码按名字引用；旧库清库重建。
- **`match_formation` 主键不改 NONCLUSTERED（2026-11-12 定案）**：NONCLUSTERED + SHARD_ROW_ID_BITS + PRE_SPLIT 是给按时间递增的雪花主键防尾部写热点用的（TiDB 决策 D3）。`battle_id` 现在由发号器发、已经打散（AGENTS §7 不变量 1），聚簇主键的写入本来就散在各个 Region，不需要再打散。原计划的 `recovery_shard` 列随租约表不做（§5.2）。
- **整队读页只认显式模式（2026-11-12，未编译）**：撮合读整队原件的条件从 `mode IN(5,0)` 改成 `mode=?`。`mode=0` 是旧原件按 PVE_TEAM 解释的写法，新原件由 `normalizeTeamMatchMode` 恒写 5；`IN` 两个值会让读页变成两段范围，TiDB 只能把这一段的整队全读出来再排序，读够一页就停不下来。项目未上线、清库重建，库里没有 `mode=0` 的原件；校验路径的 `TeamLaunchMode` 照旧兼容 0。
- 池索引按首选机房前缀、热池按机房与子池拆写者：2026-11-12 已落码（§5.2.1，未编译），没有等 KeyViz 或压测证据（负责人 11-11 改口径「不用等压测数据了，先写完」）。

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
- **评分按 `(player_id, mode)` 分开存**（R5，2026-11-05 已落码，未编译，待 Codex 验证，§12.6）：
  - 哪些模式计分只看 `ratingrule.Rated`（1V1、5V5、PVP_TEAM）。每个计分模式一条评分轨道：`match_rating` 主键 `(player_id, mode)`，分数和局数各算各的，打 1V1 不动 5V5 的分。PVP_TEAM 各档每侧人数共用一条轨道（与 R7 约定），和 1V1 是两条轨道。不计分的模式没有评分行，`Store.Rating`、`ApplyRating`、`StopRankSource` 收到它直接报错，不给缺省分。
  - **mode 同源**：单人入队读分用 JoinQueue 的 `mode`（已过模板白名单），整队入队的快照用队伍原件的 mode（`queueRatingCentiTx`，与单人入队同一条 SQL `authority.ratingCenti`）；5V5 蛇形分队用成局原件的 mode（`gather.teamAssignment`）；结算用结果的 `match_mode`。三处是同一个值：票的 mode → 成局原件 `mode` → `CreateBattleRequest.match_mode` → battle 回显到 `BattleResultEvent.match_mode`；新终局协议在 `CommitBattleOutcome` 提交点还核了结果的 mode 与持久建房原件、成局原件一致（`validateTerminalResult`）。点名成局（切磋、活动、整队直接开战）都不计分，票上的评分快照记 0，不为它读库。
  - **榜按模式分开**：一张榜混进两种模式，批次的 `score_version`（这名玩家在本模式第几局）两边各数各的，1V1 的第 9 局会被 5V5 的第 3 局当成旧版本挡掉，或者把新分盖回旧分。所以每个计分模式一张榜：`match_rank_source` 主键 `(source_epoch, mode, shard)`、`match_rank_outbox` 主键 `(source_epoch, mode, shard, source_cursor)`，每个模式自己的 64 片，各片游标从 1 连续；发布循环按模式找（或打开）各自的榜，一个模式的榜打不开不拦别的模式。榜键 `board_type` 如下（其余固定为 GLOBAL、period `all`、第 1 代），是客户端契约（`GetRankPage` 按它找榜），两版服务器同值。取值登记在 `proto/rank/rank.proto` 的 `RankBoardType`（2026-11-12 起；客户端与服务端都用生成常量，不写数字，已上线的值不改不复用）；哪个模式写哪张榜只看 `authority.rankBoardTypes`，`TestRankBoardTypesCoverExactlyRatedModes` 钉住它与计分模式集合一一对应、每个值都已在枚举里登记：

    | 模式 | board_type | 说明 |
    |---|---|---|
    | 1V1 | 1 | 沿用拆分前唯一那张评分榜的键（robot rank-smoke 读它），打开请求与拆分前逐字节相同 |
    | 5V5 | 2 | 新增 |
    | PVP_TEAM | 3 | 新增，各档每侧人数一张榜 |

  - rank 服务不用改：它按 (board, generation, source_shard) 管游标和幂等，按 (board, generation, entity) 管版本，与生产者开几张榜无关。
  - **封榜按模式**：`StopRankSource(ctx, epoch, mode)` 停一个模式的 64 片，返回该模式各片最终游标之和，就是那张榜封榜时的 `expected_source_cursor`。要停全部计分模式，对每个计分模式各调一次，先后不限、彼此独立：各模式各自封榜，没有跨模式的原子要求，一个模式停了不影响别的模式入账。赛季首发仍不做（D7），「停写 → 投完 → 封榜」的编排没有生产调用方。
  - **迟到与重复**：`match_rating_applied` 仍按 battle_id 幂等，摘要覆盖整条结果（含 match_mode），同一局换个模式重放判冲突，不会记进另一条轨道；重复判定在停写检查之前，已入账的局在停写之后重放照样回「重复」，不卡结果消费。
  - **停写之后的新局（2026-11-06，未编译）**：评分照记、入账标记照写，只是这局不进榜——不写 outbox、不推进来源游标，`StopRankSource` 返回的封榜批次总数不变；`ApplyRating` 回 applied，另计 `match_rating_unpublished_total{mode}`。原先这里报错，结果消费（全部模式共用一条流）会重试到 HOLD、把别的模式和别的局一起卡住（数据覆写审计 r2 §4 第 12 条）。取舍：评分是玩家的实力轨道，榜只是它的快照，封榜不该吞掉已经打完的计分局；新代次怎么开、要不要重置评分，等赛季功能立项时定。
- **回合打满判平**（R5，取代 `RatingDrawRoundCap*`，2026-11-05 已落码，未编译）：引擎的胜负规则对所有模式相同（`TurnBattleEngine::UpdateOutcome`）：一方全灭判另一方胜，同时全灭判平，都没全灭而回合数到了上限判进攻方（0 队）负；整场期限打满由 battle 节点直接盖平局章（`OnBattleDeadline`），结果本来就是 DRAW。结果里没有「终局原因」字段，但完整个人原件（`settlement_format_version=1`）带每名玩家和宝宝的终局存活状态，能直接判出是不是打满，比用回合数反推准（`logic.ratedScoreA`）：
  - DRAW 计 0.5。SIDE_A_WIN 计 1：打满只会判 1 队胜，0 队胜一定是把对面打到全灭，哪怕在最后一回合（旧实现把它也改判平局）。
  - SIDE_B_WIN：0 队还有能行动的单位（玩家未阵亡未逃跑，或他的宝宝未阵亡）就是打满，改判平局；0 队全灭才是真输，最后一回合真打死也分得清（旧实现分不清，一律保守判平）。
  - 只有没有个人原件的旧版结果（version 0）才看回合数：`total_rounds ≥ catalog.RoundLimit(battle_config_id)` 判平，否则算输。`RoundLimit` 与引擎同一换算：`max(1, ceil(time_limit × 1000 / 6000))`，`time_limit` 为 0 或模板不在表里取 300（`turn_battle_constants.h` 的 `kRoundDurationMs` / `kDefaultMaxRounds`），不截断到 3600 秒（截断的是整场期限）。PVP 现在借用的副本 1 是 1800 秒，正好 300 回合，与删掉的 `RatingDrawRoundCap: 300` 相同。
  - 原件证明打满、回合数却没到本副本推导的上限时（battle 与本副本的 Dungeon 表版本不同，或引擎多了胜负规则），仍按原件判平局，记一条 ERROR 日志。指标 `match_rating_round_cap_draw_total{mode,basis}`，basis 取 `survivors` / `round_limit`，只计真正入账的局。
  - 配置 `RatingDrawRoundCap` / `RatingDrawRoundCapByConfigId` 已删（`config.go`、`etc/match_service.yaml`、`k8s_deploy.ps1` 的 match ConfigMap 键清单）。它们不在全局策略指纹里，删除不改指纹。
- **计分局禁止观战（复用 16018，观战列表排除计分局）：已做，2026-11-05 核对**。可见性在建房时按模式定一次，冻结进 `CreateBattleRequest.spectate_visibility`（`logic/spectate_visibility.go` 的 `spectateVisibilityOfMode`：1V1、5V5、PVP_TEAM 都是 PRIVATE），之后各执法点只读冻结值：WatchBattle 落原件之前判 `globalFormationWatchable`，不满足回 16018（`kMatchBattleNotWatchable`，`logic/global_spectator.go` 的 `locateGlobalWatch`）；活跃集合拒收私密局（`logic/spectate.go` 的 `addSpectateActive`），观战列表和随机选场都读不到它；列表读取和运营推荐再各滤一次（`listwatchablebattleslogic.go`、`setspectatefeaturedlogic.go`）；battle 的 AddObserver 对非 PUBLIC 房间一律拒绝（`cpp/nodes/battle/logic/battle_room_manager.cpp` 两处，第二道闸）。`TestRatedModesAreNeverSpectatable` 按 `ratingrule` 遍历全部计分模式断言都是私密，以后加计分模式漏改观战表会红。
- 1V1 谁在 0 队由 battle_id 决定（splitmix64 终混取一位，`sideFlip`），不再恒归排在前面的锚点；同一局重放结果不变。切磋仍是发起者在 0 队。（第二批已落）
- 人数档真人 PVP（PVP_TEAM）2026-11-05 起计分，组排、车队对等与分边枚举见开头「计分组排」。
- 不做：Glicko、赛季、5V5（`MATCH_MODE_5V5`）的组排与分边枚举。5V5 先用"前 N 局大 K"的 Elo 加现有蛇形分边，加一个"期望胜率差"直方图，看数据再定。

## 9. 客户端可见变化（两版与客户端同批）

- 16002 新增拒绝场景：模板不在白名单 / 未开放。
- 新增两个 tip 码（导表器发号，`data/tip/Tip.xlsx`）："需要重新探测"、"没有机房满足延迟"；16004 标 fault。
- 不新增消息号与字段。逐级放宽、单人即时成局对客户端是行为变化，不是协议变化。
- 2026-11-05（R7）：`//team_error` 组新增 `TeamPvpPartySizeNotAllowed`（每侧 5 人的 PVP_TEAM 不收 4 人队）与 `TeamRatingGapTooLarge`（组队成员评分差超过上限），StartTeamMatch 开 PVP_TEAM 时回这两个业务拒绝。车队对等让大车队多等，对客户端是行为变化，不是协议变化。
- 客户端（`../mmorpg-client`，2026-11-11 Claude 写完，未编译）：
  - `TeamClient.DescribeTip` 给这两个码各配一句中文提示，不再落到「组队服务暂未完成请求（4031）」这类兜底文案。
  - 队长开 PVP_TEAM 前，本地按同一条规则先拦「每侧 5 人 + 4 人队」：`TeamClient.PvpPartySizeForbidden`，在 `Precondition` 与 `BattleMatchEntryClient.StartPvp` 里判。服务端开战事务仍以 `team.CheckPvpPartySize` 为准。
  - 战斗排队面板：4 人队的 5v5 档沿用已有的不可点样式；选中的档发不了时退回队伍人数那一档；提示行换成「4 人队可以打 4v4；5v5 需要 1～3 人或满 5 人。」这句文案没有设计稿依据，待设计确认。
  - 评分差上限要读成员的评分，客户端没有这份数据，只靠服务端回码提示。
  - EditMode 新增 `TeamClientTests` 两条：4 人队开 5v5 本地被拒、同一队开 4v4 正常发出；两个码各有自己的文案。
- 2026-11-12（R5 残余收口，Claude 写完，未生成、未编译）：`rank.proto` 新增枚举 `RankBoardType` 登记 `board_type` 取值（见 §8），`RankBoardKey.board_type` 仍是 uint32，线上取值不变。客户端论剑榜按模式分榜：
  - 窗口标题右侧三个页签「PVP 匹配」「PVP 1v1」「PVP 5v5」（board_type 3 / 1 / 2），默认看 PVP_TEAM：客户端「匹配 PVP」入口只排 PVP_TEAM，玩家的评分在这张榜上；1V1 / 5V5 客户端没有排队入口。
  - `RankClient`：快照按榜类型分开存，最后一次意图所在的榜就是选中的榜，翻页、「我的附近」、「最新榜」都跟选中榜走。切榜不作废在途请求（单请求在途与保守隔离契约不变），新榜排在后面；旧榜的迟到回包只进旧榜自己的快照，不出现在新榜上，也不改状态行。回包回显的榜类型与请求不符按读失败处理。超时作废全部榜的快照；换角色、换连接回到默认榜。
  - 某个计分模式还没有一局入账时它的榜还没开（发布循环按模式惰性打开），读它回 `kRankBoardNotFound`，客户端显示「本期排行榜尚未开放。」。1V1 / 5V5 没有客户端入口，开服后这两个页签会一直是这句。
  - **待设计确认**（没有设计稿，现用窗口已有的 `tab_normal` / `tab_selected` 素材，界面验收不能按原图还原宣称通过）：页签的形态与位置（现为标题右侧三个横向页签，原副标题缩成「全服排行 · 同分先达到者在前」放在页签右边）；页签文字（PVP_TEAM 的 1v1 档记在「PVP 匹配」，与「PVP 1v1」容易混）；选中态；默认页签；空榜文案（现为「榜上暂无记录，打一局本模式论剑即可上榜。」）与未开榜文案；1V1 / 5V5 没有排队入口时要不要显示这两个页签。
  - EditMode `RankBoardSelectionTests` 12 条；`RankClientTests` 的回包改在默认榜上。

## 10. 不做

- 内存撮合、硬围栏、批量 IN FOR UPDATE 的 claim、Join 与 claim 合并成一个事务。
- rule_rev 池隔离、弹性人数档、Glicko、5V5 126 种分边枚举、赛季表、票据寿命与探测解耦。
- 5V5（`MATCH_MODE_5V5`）组排、跨模板 / 随机副本合池、AI 补位、战力或等级分层、开放时间列、按模式独立的延迟阈值、`MATCH_MODE_3V3`（3V3 走 PVP_TEAM 每侧 3 人）。
- 不改 `releaseGlobalFinished`、`HandleQueryBattleCreation` 及 formation 目标失效后的解卡语义（属于"数据覆盖审计"会话）。
- 评分来源单行串行与投榜批量化：结果侧另行立项。

## 11. 分阶段落地与进度

| 阶段 | 内容 | 状态 |
|---|---|---|
| 第一批 | `scope`、`poolgroup` 两个纯函数包与测试；ZoneGroup、PlayMode 两张表的 schema 与 xlsx；本文 | 已落源码，未编译、未测试 |
| R0 | 全局路径低基数指标；robot 加 JoinQueue 压测动作；`stress_snap` 拉 :9170；`stress_summarize` 加匹配段；跑现状基线 | 按池指标已落（`metrics/pool.go`：`pool_round_seconds`、`pool_owned`、`pool_page_units{mode,team_size}`、`pool_claim_total{mode,level,pass,outcome}`（outcome 含 `party_ended`）、`pool_wait_seconds{mode,level}`、`pool_expired_total{mode}`、`immediate_claim_total{mode,outcome}`）；压测抓取、汇总段、robot JoinQueue 动作 2026-11-05 已落码（缺口清单第 10 条，未运行）：`stress_snap.ps1` 缺省抓 :9170（`t<N>m_match.txt`）；`stress_summarize.ps1` 的 G1（总览，含轮耗时 p99 ÷ 轮间隔、冲突率）/ G2（按 level 的成局与等待 p50 / p95）/ G3（按模式）；robot `match-stress` 档的 `join_queue` 动作（探测 → 入队 → 排满 `dwell_seconds` 撤单，成局后等结束再排），样例 `robot/etc/robot.match-stress-100.yaml`；基线待压测 |
| R1 | 票据写真实 home zone；肇事者剔除；过期清扫；机房内按负载选节点（P2C + 熔断），删除"最小 NodeId"；整队先占槽再 claim；单人即时成局；`MatcherIntervalMs` 生效 | 第二批已落源码（2026-10-30），未编译、未测试；在线检查撤票不做（§5.3 第 6 条） |
| R2 | 模板目录接线、白名单（JoinQueue / StartTeamMatch / ChallengePlayer）、期限按模板 | 第二批已落源码，未编译、未测试；PVP 平局阈值推导挪到 R5 |
| R3 | 池归属（HRW，取代分片租约）+ 一轮多局 + 范围树接入撮合与提交点 | 第二批已落源码，未编译、未测试 |
| R4 | §6 TiDB 热点治理 | 投影表退役已落源码（2026-11-05，未编译、未测试）；其余按压测证据 |
| R5 | §8 PVP 口径：评分按模式分开、平局阈值、计分局禁观战核对 | 1V1 分边已落；评分按模式、榜按模式、平局判定已落码（2026-11-05，回合制缺口会话），未编译，待 Codex 验证（§12.6）：`match_rating` 主键 `(player_id, mode)`，投榜来源与 outbox 按 `(epoch, mode, shard)`，每个计分模式一张榜（board_type 1V1=1、5V5=2、PVP_TEAM=3），`StopRankSource` 按模式停；回合打满先看个人原件里 0 队是否全灭，旧版结果按 `catalog.RoundLimit`（模板 `time_limit` 换算），删 `RatingDrawRoundCap*`；计分局禁观战已做（位置见 §8）；Java 版待做 |
| R6 | battle 上报可分配状态与房间数，match 按负载放置 | 已落码（2026-11-05，回合制缺口会话），未编译，待 Codex 验证（§12.4）：`NodeInfo.admission`（14）只在变化时写 etcd、按 watch 回读对账；`CreateBattleResponse.room_count`（5）随成功应答捎回；battleplace 跳过 CLOSED、UNSPECIFIED 视为可选，按「60 秒内的上报房间数 + 本副本在途」比较；指标 `match_battle_nodes{state}`（§5.1）。「可分配」判定已落为 `battle_admission::AcceptsNewRooms` + `BattleNodeImpl::LifecycleGrantsPermits`；match 侧落库前预检 `rejects_new_battles` 已落（§5.5），battle 侧填值复用这一判定，那一行仍待补 |
| R7 | 人数档真人 PVP 计分组排（开头一节）：PVP_TEAM 计分、整队入队读分、按人数分桶与拆分模板选人、车队对等、分边平衡、提交点组内极差复核、每侧 5 人不收 4 人队、组队成员评分差上限 | 已落码（2026-11-05）。Codex 11-06 跑过纯单测，全过，4 条 R7 真库用例因没有 TiDB 跳过；11-10 跑过 robot 编译、vet、单测与夹具工具单测，全过。真库回归与冒烟都没跑过；11-11 改了冒烟判据、夹具工具与客户端（§9），这三处未编译（§12.5）。评分按模式分开存由 R5 负责；Java 版待做 |
| R8 | 后台工作按片分工（缺口清单第 24 条）：成局推进、整队维护、闲置队伍到期、投榜与撮合池共用一条 `OwnsWork` 归属规则（§5.2）；评级来源停写后评分照记、不进榜、不卡结果消费（§8 R5「迟到与重复」） | 已落源码（2026-11-06，全服匹配方案讨论会话），未编译、未测试，待 Codex 验证（§12.7）；热池拆分见 R9 |
| R9 | 热池拆分（缺口清单第 24 条，§5.2.1）：撮合段（整池 / 机房 / 子池 / 跨机房）与按段归属键、票与整队原件的 `pool_region` 列与新索引、`GlobalMatch.SubPools` / `SubPoolMergeSeconds`、`poolgroup.Passes`、`authority.ErrClaimStale`、按段指标 | 已落码（2026-11-12，「缺口第 24 条：热池按机房与子池拆写者」会话），未生成、未编译、未测试，待 Codex 验证（§12.8）；Java 版待做 |

**文件归属（2026-10-24 与并行会话约定）**：

- 「数据覆盖审计交接文档」会话在改 `authority/store.go`（formation 恢复出口、`checkWork`、formation 状态迁移）、`logic/global_match.go`（`globalDestroy` / `releaseGlobalFinished`）、`authority/entry_guard.go`。第二批要等它通知函数名与行号后按段落避开。
  - **2026-10-24 该会话已收尾**（social-match-battle#0，未编译、待 Codex；通知时本会话已结束，故写在这里）。它改过的段落，行号为 10-24 工作区：`authority/store.go` 约 779-806（`ErrAwaitingExpiredOutcome`、`awaitsExpiredOutcome`、`expiredOutcomeSQL`、`CountStrandedExpiredFormations`）、约 824-825（`AcquireWork` 的到期门禁）、约 869 起（`CreateIntentPersisted`）、约 919（`Pending` 的 SQL 加 `AND NOT expiredOutcomeSQL`）；`logic/global_match.go` 约 409-420（`formationDestroyAuthority` 接口与 `globalDestroy` 的无原件短路）、约 530-570（`globalAbortPrepared` 在 `Finish(false)` 之后计数）；`logic/gather.go` 约 177（`globalDestroy(svcCtx.Authority, …)` 调用点）；`logic/matcher.go` 约 138-139（到期巡检 `safego.Loop`）；新文件 `logic/global_destroy_test.go`、`logic/global_expired_audit.go`、`authority/formation_recovery_exit_test.go`、`authority/started_formation_exit_test.go`。`entry_guard.go`、`checkWork` 最终没有改。第二批动这些文件前先重读，按段落避开。
- 「全服观战架构设计」会话在改 `constants/errors.go`、`metrics/metrics.go`、`spectate*.go`、`watchbattlelogic.go`、`global_spectator.go`、`authority/spectator.go`、`entry_guard.go`（只加哨兵错误）、`match_authority.proto` 的 `MatchSpectatorIntentRecord`。它以后的可见性改动要碰 `gather.go` 两处，`gather.go` 由本设计负责，届时在本设计的版本上补。
- `match_authority.proto` 两边改的是不同 message；proto 生成只由 Codex 在两边落稳后统一跑一次。
- **第二批（2026-10-30）动到的他人文件**，并行会话请按段落合并：`logic/pvp_team_test.go` 两处 `newPartySvcCtx` 少接一个返回值（编译错误，改为 `s, mr, _ :=`）；`team/service.go`、`team/sql_launch.go` 的组队人数上限改为 `TeamSizeFor(mode, config)`（PVP_TEAM 也查 PlayMode 表，删掉固定 `Capacity` 分支）；`logic/challengelogic.go` 入口加 PlayMode 白名单；`logic/gather.go` 的 `errPrepareRejected`、肇事者传递、`not_allocatable` 熔断与 1V1 `sideFlip`；`logic/global_match.go` 的 `globalAbortPrepared` 改为带肇事者调用 `FinishFailed`。单测的模板目录替身统一用 `logic/catalog_fixture_test.go` 的 `useTestCatalog`。
- **计分组排（R7，2026-11-05）与 R5 的分工**（R5 在回合制缺口会话）：R7 改 `logic/rating.go` 里 `isRatedMode`、`ratingToleranceFor`、`ratingToleranceMaxWaitFor`、`anchorToleranceFor` 四个函数体（改成委托 `ratingrule`），新包 `ratingrule`，`poolgroup` 整包，`logic/global_pool_matcher.go` 的 `poolPolicy` / `poolUnitInput` / `expectedClaimRejection`，`authority` 的 `claimTx` 复核段、`policy.go` 指纹段、`team_queue.go` 的 `EnqueueTeamLaunchTx`、新文件 `rated_spread.go` / `party_rating.go`，以及 `config.GlobalMatchConf` 的两个字段。R5 改 `authority/rating.go`、`loadRating*`、`ApplyBattleResult` 的平局判定、`global_match.go` 的入队读分、`gather.go` 的 `loadRatings`、`RatingDrawRoundCap*`、榜单与建表。R5 已把 `queueRatingCentiTx` 改成调用 `rating.go` 的 `ratingCenti`（按 `(player_id, mode)` 读）。R7 补充的 `RequirePartyRatingGapTx` 也走它；真库用例 `TestAuthorityTiDBQueueRatingSnapshot`、`TestAuthorityTiDBPartyRatingGap` 按 `match_rating(player_id, mode, rating_centi, games, updated_ms, score_changed_ms)` 插数据。以后改 `ratingCenti` 的签名或这张表的结构，要连这几处一起改。（2026-11-05 R5 已改：`queueRatingCentiTx` 改走 `authority/rating.go` 的 `ratingCenti`，按 `(player_id, mode)` 读；对应真库用例 `rated_spread_test.go` 的 `TestAuthorityTiDBQueueRatingSnapshot` 本来就按 `(player_id, mode)` 插数，与新主键一致，不用改。）

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

### 12.3 第三批（2026-11-05：投影退役、落库前预检）

Claude 只跑过 `gofmt`，没有编译、测试或生成。第二批在 Codex 11-02 第二轮里 `go build` / `go vet` / `go test ./...`（`go/match`，不含真库）已是 0 退出（`docs/handoff/player-save-direct-commit-20261030/codex-results.md` 的 `03-*-go-match`）；本批在它之上。

1. **proto**（仓库根，no-unity）：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。本批改了 `proto/battle/battle_node.proto`（`QueryBattleCreationResponse.rejects_new_battles = 5`）、`proto/match/match_authority.proto`（删 `MatchTicketRegionRecord`，票的字段 17 改 `reserved`）、`proto/team/team_authority.proto`（删 `TeamLaunchRegionRecord`）。期望：生成后 `go/proto/match`、`go/proto/team` 里不再有这两个 message 和 `PreferredRegion` 字段；C++ 侧只多一个字段，battle 节点要重新编译（`/m:1`）。
2. **Go 单测**：
   ```bash
   cd go/shared && go vet ./matchentry/... && go test ./matchentry/... -count=1
   cd go/match && go vet ./... && go test ./... -count=1
   ```
   通过标准：全部 PASS。失败保留编译错误全文、失败用例名与断言原文。
3. **真库回归**（隔离的一次性 TiDB，清库后用 `match -f <配置> -migrate` 建表）：
   ```bash
   cd go/match
   MATCH_IT_TIDB=127.0.0.1:<端口> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/authority/... ./internal/team/... ./internal/logic/... -count=1
   ```
   重点用例：`TestTeamTiDBCrossHomeUnitsShareThePoolAndWaitForScope`、`TestTeamTiDBPoolTicketsReadSoloUnitsAcrossHomes`、`TestTeamTiDBFailedFormationRequeuesWholeOriginalUnit`、`TestTeamTiDBWholeUnitsCrossWaitUnknownAndRecovery`、`TestTeamTiDBUnmergeLeavesInFlightUnitsRunning`、`TestEntryTiDBOccupiedSQLMatchesAllFourteenProtoFieldsAndRejectsNull`、`occupancy_native_test.go` 全部。第二轮里 team 包大面积报「权威SQL结果未定（业务发送后），禁止重放」，那是各服务共有的 SQL 写者环境问题，不是本批引入；若仍出现，单独记录、不归因本批。
   旧库（第二批之前建的）多出 `match_ticket_region`、`team_launch_region` 两张表和票表的 `preferred_region` 列，迁移器不会自动删，可能被判漂移拒启：清库重建，不要手改断言。
4. **battle 侧**：`rejects_new_battles` 的填值已随 R6 落在 `battle_node.cpp` 的 `HandleQueryBattleCreation` 守护段（2026-11-05，未编译），随 §12.4 的 battle 构建一起编译；冒烟时停机或 Agones 排空的节点上，`QueryBattleCreation` 回显本实例的应答应为 `rejects_new_battles=true`。

### 12.4 R6（2026-11-05：battle 可分配状态与房间数）

Claude 未执行任何编译、测试或生成。协议生成可与 §12.3 合并成一次。

1. **proto**（仓库根，no-unity）：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。本批改 `proto/common/base/common.proto`（新增枚举 `NodeAdmission`，`NodeInfo.admission = 14`）与 `proto/battle/battle_node.proto`（`CreateBattleResponse.room_count = 5`）。期望：`go/proto/common/base` 有 `NodeAdmission_NODE_ADMISSION_*` 与 `(*NodeInfo).GetAdmission`，`go/proto/battle` 有 `(*CreateBattleResponse).GetRoomCount`；C++ 生成物有 `NodeInfo::admission()` / `set_admission()` 与 `CreateBattleResponse::set_room_count()`。
2. **C++**（MSBuild 串行 `/m:1`，Debug x64）：common.proto 变了，先串行重编生成的 proto 库与链接它的库（按惯例重编 `game.sln`），再编 `cpp/nodes/battle/battle.vcxproj` 与 `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`（battle 的头文件单测编在这里，只有 Debug|x64）。测试工程链接预编好的 `lib/*.lib`，改了库必须先编库，不要只跑 `run_cpp_tests.ps1 -Build`。通过标准：0 个 `error C` / `LNK`；测试程序 `--gtest_filter=BattleAdmission*` 全部 PASS（既有 `BattleAdmissionGate.*` 7 个 + 新增 `BattleAdmissionAdvertise.*` 10 个）。
3. **Go**（`go/match`）：`go build ./... && go vet ./... && go test ./internal/battleplace/... ./internal/discovery/... ./internal/logic/... -count=1`。重点用例：`TestPickByAdmissionAndLoad`（12 个子用例）、`TestCensusCountsEveryState`、既有 4 个 battleplace 用例、`TestParseReadsAdmissionByNameOrNumber`、`TestParseSkipsNonRegistrationsAndUnusableValues`、`TestCreateBattleClassifiesReply`、`TestOutcomeCreateResponseRequiresOriginalInstanceAndAdoptedVersion`。`go/shared/nodeinfo` 只改了注释。
4. **冒烟**（本机，清空 redis / mysql / etcd 后起服）：battle 开闸后约 1 秒内，`etcdctl get --prefix BattleNodeService.rpc/zone/` 里该节点的值出现 `"admission":"NODE_ADMISSION_OPEN"`，battle 日志有一条 `battle admission advertised to etcd: admission=open observed=closed`；优雅停机（或维护关闸）时日志有一条 `admission=closed`，租约撤销之前 etcd 里变成 `NODE_ADMISSION_CLOSED`；稳态下不再出现 `battle admission` 日志，该键的 `mod_revision`（`etcdctl get <键> -w json`）不再增长，证明不是周期写。match 的 `/metrics` 里 `match_battle_nodes{state="open"}` 等于在线 battle 数。

### 12.5 R7（2026-11-05：人数档真人 PVP 计分组排）

Claude 只跑过 `gofmt -l`（改动的文件全部无输出），没有编译、测试或生成。本批不改 proto、不改配置表；开头第 9、10 条在 `Tip.xlsx` 的 `//team_error` 组末尾加了两个码，要先导表，再编译。评分按模式分开存（R5，回合制缺口会话）同时在改 `authority/rating.go`、`logic/rating.go` 等文件，编译失败时先确认出错的是哪一批的文件；`match_rating` 的建表变化以 R5 的清单为准，真库用例要在 R5 落地之后跑。

0. **导表与 tip 码**（仓库根）：`dev.bat gen`。期望：`go/shared/generated/pb/table` 里出现 `TeamError_kTeamPvpPartySizeNotAllowed` 与 `TeamError_kTeamRatingGapTooLarge`，号码落在 team 段 [4000, 5000)，state 只新增这两个名字、已有名字的号不变；`go/shared/generated/tip/faults.go` 不变（两个都不是故障）。C++ 与 Java 的 TeamError 生成物各多两个枚举值，C++ 的 table 库与链接它的节点照常串行 `/m:1` 重编。失败时保留导表器完整报错。
1. **编译与静态检查**（工作目录 `go/match`）：
   ```bash
   go build ./...
   go vet ./...
   ```
   通过标准：0 退出。失败时保留编译错误全文。
2. **单测**（工作目录 `go/match`，不依赖外部服务）：
   ```bash
   go test ./internal/ratingrule/... ./internal/poolgroup/... ./internal/config/... ./internal/logic/... ./internal/authority/... -count=1
   ```
   重点用例：`ratingrule` 包全部；`poolgroup` 的 `TestPickSides*`、`TestCompositionsForCountsPartitions`、`TestFormRatedSides*`、`TestFormSidesPremadeGapUsesUnitWait`，以及原有的 `TestSides*`、`TestRated*`；`authority` 的 `TestRequireRatedSpread*`；`logic` 的 `TestPoolUnitInputScoresPartiesWithBonus`、`TestPoolPolicyRatesPvpTeamPools`、`TestRatedModeIncludesPvpTeam`、`TestRatingSpreadIsExpectedClaimRejection`、`TestPoolUnitInputIntersectsMemberLatency`（整队队伍分的断言已改），以及 `rating_match_test.go` / `rating_review_fix_test.go`（容差曲线改成委托 `ratingrule`，数值不变）；`team` 的 `TestCheckPvpPartySize` 与 `errors_test.go` 的 tip 码护栏（新增两个码）；`config` 的 `TestPvpPartyRuleDefaults`。通过标准：全部 PASS。没有 TiDB 时 `TestAuthorityTiDB*` 跳过属正常，如实报告为未运行。失败时保留用例名与断言原文。
3. **真库回归**（隔离的一次性 TiDB，清库后用 `match -f <配置> -migrate` 建表；旧库里 `match_global_policy` 的摘要会让新进程拒启，见 §14）：
   ```bash
   cd go/match
   MATCH_IT_TIDB=127.0.0.1:<端口> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/authority/... ./internal/team/... -count=1
   ```
   重点用例：`TestAuthorityTiDBQueueRatingSnapshot`（整队入队按模式读分、没有记录按 1500、非计分模式为 0）；`TestAuthorityTiDBPartyRatingGap`（成员分差超过上限回 `ErrPartyRatingGap`，恰好等于上限、上限为 0、非计分模式都放行）；`TestAuthorityTiDBGlobalPolicyCannotBeOverwrittenByWaitHardLimitSecretOrRules`（新增 `party-bonus`、`party-gap` 子用例，改车队差步长仍应准入）；team 包的整队入队与成局用例（PVE_TEAM 整队票的 `rating_centi` 仍为 0），以及 `pvp_party_rules_integration_test.go` 的 `TestTeamTiDBPvpFiveSideRejectsFourStack`（4 人队开 5V5 被拒、名册与开战锁不动，同一队开 4V4 直接进队列）与 `TestTeamTiDBPvpRatingGapRejectsBeforeLaunch`（队员分差 600 被拒、不留开战原件；差 500 放行，成员票带本模式评分快照）。team 夹具的建表列表加了 `match_rating`。
4. **robot 与夹具工具的编译、静态检查和单测**（在第 0 步导表之后）：robot 用 `-mod=vendor` 引 `go/shared`，先同步 vendor，再检查全部包，并生成下一步使用的 `robot.exe`。在仓库的 `robot/` 目录执行：
   ```powershell
   go mod vendor
   go build ./...
   go build -o robot.exe .
   go vet ./...
   go test . ./config/... -count=1
   ```
   然后切到仓库的 `go/match/` 目录执行：
   ```powershell
   go test ./cmd/ratingfixture/... -count=1
   ```
   重点用例：robot 的 `TestPvpSmoke*`（情况表自洽、评分夹具覆盖全部参战账号并跟着配置走、核对结算的情况每种结果都改分、本局榜条目的判定、夹具核对、Elo 预言、等待窗口、夹具读写）与 `config` 的 `TestPvpTeamSmokeConfig*`；`ratingfixture` 的 `TestParseFixture*`、`TestWriteFixtureRecordsAppliedMs`、`TestCheckTargetOnlyAllowsDirectDevOrTestDatabases`。通过标准：各命令退出码为 0、全部用例 PASS，并产出 `robot/robot.exe`。任一步失败即停止后续步骤，保留完整输出，不用旧二进制继续冒烟。
5. **冒烟：robot `pvp-team-smoke`，覆盖开头第 1～10 条的全部情况**（本机全局路径，清空 redis / mysql / etcd 后起服，match 用缺省的计分组排参数；前置条件全文在 `robot/etc/pvp_team_smoke.yaml` 文件头：rank 链路就绪、gate 路由服模式、MessageLimiter 有 team 与 rank 额度、PVP_TEAM 的池里没有别人）。三步：第一轮按顺序做全部三步；重跑只重做第二、三步，不用清库；换库或改了规则参数才重做第一步。
   第一步在仓库的 `robot/` 目录执行（yaml 保持 `phase: prepare`）：
   ```powershell
   .\robot.exe -c etc/pvp_team_smoke.yaml
   ```
   登录全部 82 个参战账号（首次登录即建角），日志出现 `PVP_TEAM_SMOKE_PREPARED fixture=… ratings=82`，写出 `robot/logs/pvp_team_smoke_fixture.json`：每个账号的赛前评分，情况指定的照写、其余 1500，`applied_ms` 为 0。
   第二步在仓库的 `go/match/` 目录执行；`-f` 必须指向正在运行的 match 实例所用配置，沿用它的数据库口令环境变量。下面的路径只适用于实例确实使用仓库默认配置的情况；本地启动脚本派生 MySQL 配置时，改填该派生配置路径：
   ```powershell
   go run ./cmd/ratingfixture -f etc/match_service.yaml -in ../../robot/logs/pvp_team_smoke_fixture.json
   ```
   在一个事务里把夹具写进 `match_rating`（只认 dev / test 配置；已有行只改分数与时刻，局数不动），再把写库时刻（数据库时钟）写回夹具的 `applied_ms`，输出 `applied 82 ratings for mode 7 to …/… at applied_ms=…`。写回失败时退出码 1，评分已在库里，原命令重跑即可。
   第三步把 yaml 里的 `phase` 改成 `run`，切回 `robot/` 目录，再跑一次第一步的命令。run 开跑时核对夹具已写库（`applied_ms` 非 0）、每个账号的 player_id 与评分对得上，随即把 `applied_ms` 清零：一次写库只够跑一轮，忘了重做第二步的重跑会以 `PVP_TEAM_SMOKE_FAIL case=fixture` 直接失败。通过标准：日志出现 `PVP_TEAM_SMOKE_OK cases=14 battles=12`，退出码 0；失败时是 `PVP_TEAM_SMOKE_FAIL case=… reason=…`、退出码 1，保留整份 robot 日志与 match 日志。14 种情况（单测之外再跑一遍真链路）：
   - 1 人档：`1v1`；`1v1-rating-window`（1500 对 1700，前 3 秒不成局，锚点等满 5 秒后成局）；
   - 2 人档：`2v2-duo-plus-solos`（双排 1500 × 2 同侧、两个单人 1550；打完全员在 PVP_TEAM 榜 board_type 3 上按 Elo 结算，1V1 榜上没有他们）、`2v2-duo-vs-duo`、`2v2-party-too-big`（`TeamSizeExceeded`）、`2v2-rating-gap`（1200 / 1800 回 `TeamRatingGapTooLarge`；1200 / 1700 可以排队；排队中队员离队，队长收到整队撤单 `TeamStateChanged`）、`2v2-rated-balance`（1500 / 1600 / 1400 / 1500 等满一档成局，两个 1500 同侧）；
   - 3 人档：`3v3-duo-solo-vs-solos`（立即成局）、`3v3-trio-waits-for-solos`（单人入队后前 8 秒不成局，10 秒后成局）；
   - 4 人档：`4v4-four-stack`（开 5V5 回 `TeamPvpPartySizeNotAllowed`、队伍保持空闲；开 4V4 排队，前 18 秒不成局，20 秒后成局）；
   - 5 人档：`5v5-trio-duo-vs-trio-duo`（每侧一个 3 人队加一支双排）、`5v5-duos-split`（两支双排分在两侧）、`5v5-stack-vs-stack`、`5v5-stack-waits-for-solos`（前 28 秒不成局，30 秒后成局）。
   每种成局的情况都核对全员进同一局、每支队伍整体同侧、两侧各 N 人，再全员直连自动战斗打到终局。不同人数档并发跑，整轮约 5 分钟。
   结算核对口径（2026-11-11 按 Codex 11-10 指出的两处边界改）：
   - 全部 82 个参战账号每轮由夹具重置赛前评分，不清库重跑的撮合条件与第一轮相同。
   - 结算预言的赛前评分取夹具，不读榜：榜上是上一轮结算后的分数。
   - 榜条目要同时满足两条才算本局入账：`achieved_ms` 晚于夹具的写库时刻；分数是本局可能的结果之一，即 battle 判的胜负，或 match 按回合上限改判的平局。夹具写库之前的旧条目、上一轮迟到的投榜批次都早于写库时刻，不会被当成本局。
   - match 改了分才把 `achieved_ms` 记成结算时刻，分数不变时沿用写库时刻，判不成本局。所以 `2v2-duo-plus-solos` 让两侧平均分不等（双排 1500、单人高半个起始容差），胜、负、平都会改分；单测按所有可能的分边与结果核对这一点，运行时再查一次，改不了分就直接失败。不再有「分数没变也放行」的分支。
   - 剩余边界：客户端榜单仍没有局数或结算版本，判据依赖 match 结算与夹具工具读的是同一个数据库时钟，TiDB 节点之间的时钟差要远小于写库到开战的间隔（实际是秒级以上）。
   另核对 match 日志里没有 `Rating spread rejected at commit`：撮合与提交点按同一份规则判同一批票，出现就说明两边的规则或配置不一致，保留日志上报。
   第一轮通过后，只重做第二、三步再跑一轮，不清库：通过标准相同。这一轮验证夹具重置全部账号之后重跑仍然确定。
6. **客户端**（`../mmorpg-client` 仓库根，§9 的改动）。两个码的 C# 枚举 `KTeamPvpPartySizeNotAllowed` / `KTeamRatingGapTooLarge` 已在客户端生成物里，不用重新生成。先做离线编译体检（约 2 秒，退出码 0 为绿）：
   ```powershell
   pwsh -File tools/client_compile_check.ps1 -ShowErrors 40
   ```
   再在隔离工程里用 Unity batchmode 跑 EditMode。这个脚本复制全部 `Assets/Scripts` 与 `Assets/Tests/EditMode/Tianyong/Team*Tests.cs`，不打开主工程：
   ```powershell
   pwsh -File tools/verify_team_hud.ps1
   ```
   重点用例：`TeamClientTests` 的 `StartPvpMatch_FourStackCannotQueueFiveSide`、`DescribeTip_PvpPartyRulesHaveTheirOwnText`，以及补了两个码的 `DescribeTip_CoversTableAndUnknown`。通过标准：编译体检退出码 0；`team-tests.xml` 失败数为 0，Unity 退出码 0。证据保留在脚本打印的证据目录，主要是 `team-tests.xml`、`unity.log`、`native-run.json`。客户端工作区还有别的会话的未提交改动，红在别处时先分清是哪批的。

### 12.6 R5（2026-11-05：评分按模式分开、榜按模式、平局判定）

Claude 未执行任何编译、测试或生成。项目未上线，库可清空，不写迁移。与 §12.5 共用一个工作区，同包有 R7 与观战、数据覆盖审计会话的在途改动，红在别处时先分清是哪边的。

1. **proto**（仓库根，no-unity，可与 §12.3～§12.5 合并成一次）：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。本批只改 `proto/match/match_authority.proto` 的三个表记录：`MatchRatingRecord` 加 `mode = 6`、主键 `player_id,mode`；`MatchRankSourceRecord` 加 `mode = 7`、主键 `source_epoch,mode,shard`；`MatchRankOutboxRecord` 加 `mode = 6`、主键 `source_epoch,mode,shard,source_cursor`。期望：`go/proto/match` 里这三个 message 有 `GetMode()`。C++ 生成物（`match_authority.pb.cc`）也会变，没有 C++ 代码引用这三个 message，随下次 C++ 编译即可。
2. **建表**（清空 match 库与 rank 库）：TiDB 用迁移账号执行 `match -f <配置> -migrate`；本机 MySQL 档 `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-migrate -LocalMySQL -GoServices match`。新库：三张表按新主键建出。旧库（本批之前建的）：迁移器会补 `mode` 列，但主键不符记人工项（`primary key mismatch: table match_rating has PRIMARY KEY (player_id)…`，ExitCode 4），常驻进程的 `requireCurrentSchema` 拒绝接流。这是预期的 fail-closed，不要手改主键或断言，清库重建；rank 库要一起清，否则旧榜上 (shard, cursor) 的收据会和新来源从 1 起的游标撞成冲突，投榜卡住。
3. **Go**（cwd `go/match`，`GOTOOLCHAIN=local`）：`go build ./... && go vet ./... && go test ./... -count=1`。通过标准：全部 PASS。重点用例：
   - `internal/logic`：`TestRatedScoreAJudgesRoundCapByOriginals`（13 个子用例）、`TestOldResultsDrawAtTemplateRoundLimit`、`TestRatedModesAreNeverSpectatable`，既有 `TestApplyBattleResultIgnoresUnratedResults`、`TestSpectateVisibilityPolicy`；
   - `internal/catalog`：`TestRoundLimitMirrorsEngine`；
   - `internal/authority`（不连库也跑）：`TestRankBoardTypesCoverExactlyRatedModes`、`TestOrderRankShardsByBacklogServesLargestFirst`；
   - `internal/config`、根包 `match_service_test.go`：删配置项后照常 PASS。
   失败时保留编译错误全文、失败用例名和断言原文。
4. **真库回归**（隔离的一次性 TiDB，清库；cwd `go/match`）：
   ```bash
   MATCH_IT_TIDB=127.0.0.1:<端口> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/authority/... -count=1 -run 'Rating|RankBoard|QueueRatingSnapshot'
   MATCH_IT_TIDB=127.0.0.1:<端口> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/authority/... ./internal/logic/... ./internal/team/... -count=1
   ```
   重点：`TestRatingTiDBModesKeepSeparateRatingsSourcesAndBoards`（同一玩家两种模式的分数、局数、score_version、同一片的游标、榜号互不相干，一局换模式重放判冲突，按模式停写）、`TestRatingTiDBRejectsUnratedModeWithoutSideEffects`、R7 的 `TestAuthorityTiDBQueueRatingSnapshot`（整队入队按模式读分），既有 `TestRatingTiDBShardedSourceCursorsVersionsAndStopTotal`、`OneShardFailureDoesNotBlockOthers`、`RoundBudgetExhaustedStopsWithoutError`、`CursorAheadFromOtherReplicaIsNotAFailure`、`AchievedMsFollowsScoreChangeNotGames`、`TransactionOutboxReplayAndStopBarrier`（新增断言：停写后已入账的局重放回「重复」、不报错）。整链 `TestTiDBRatingRankMailChainRecoversAfterCommittedReplyLoss` 要 `RANK_MAIL_CHAIN_IT_TIDB` 和预编译的 mail / rank 夹具（`RANK_MAIL_CHAIN_MAIL_EXE`、`RANK_MAIL_CHAIN_RANK_EXE`），有环境才跑；它只打 1V1，期望不变。
5. **ConfigMap**：`tools/scripts/k8s_deploy.ps1` 渲染一次 match ConfigMap，核对不再有 `RatingDrawRoundCap`，其余评分键照旧。
6. **冒烟**（本机，清空 redis / mysql / etcd 后起服）：机器人打一局 1V1、一局 PVP_TEAM（或 5V5）后，`SELECT player_id,mode,rating_centi,games FROM match_rating` 每人每个模式一行；`match_rank_source` 出现两个 mode，各自的 `board_id`；rank 库 `rank_board` 有 `board_type` 1 与 3（或 2）两行；robot rank-smoke（读 `board_type` 1）照常通过。打满回合的 PVP 局 `match_rating_round_cap_draw_total{basis="survivors"}` 增长，`basis="round_limit"` 只在旧版结果出现。

### 12.7 第四批（2026-11-06：后台工作按片分工、评级停写不卡消费）

Claude 只跑过 `gofmt`，没有编译、测试或生成。本批不改 proto，不需要重新生成。

1. **Go**（工作目录 `go/match`）：
   ```bash
   go vet ./...
   go test ./internal/authority/... ./internal/svc/... ./internal/logic/... ./internal/team/... ./internal/metrics/... -count=1
   ```
   新增纯单测 `TestWorkShardFilterKeepsOnlyOwnedShards`（不需要真库）。通过标准：全部 PASS；失败保留编译错误全文、用例名与断言原文。
2. **真库**（一次性 TiDB，清库后 `match -f <配置> -migrate`）：
   ```bash
   MATCH_IT_TIDB=127.0.0.1:<端口> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/authority/... ./internal/team/... -count=1 -run 'Work|Rating|FormationExit|TeamTiDB'
   ```
   重点：`TestWorkTiDBPendingReturnsOnlyOwnedShards`、`TestWorkTiDBRankPublisherSendsOnlyOwnedShards`、`TestTeamTiDBMaintenanceReadsOnlyOwnedShards`。评级停写改了 4 处既有断言，从"停写即报错"改成"评分照记、不进榜、封榜批次总数不变"：`TestRatingTiDBShardedSourceCursorsVersionsAndStopTotal`、`TestRatingTiDBTransactionOutboxReplayAndStopBarrier`、`TestRatingTiDBModesKeepSeparateRatingsSourcesAndBoards`、整链 `TestTiDBRatingRankMailChainRecoversAfterCommittedReplyLoss`（要 `RANK_MAIL_CHAIN_*` 环境，有才跑）。
3. **部署合同**：`pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`（`match-alerts.yaml` 的 `MatchPoolsUnowned` 只改了描述）。
4. **多副本冒烟**（本机起 2 个 match 副本，清空数据后）：两个副本的 `match_pool_owned` 之和等于目录里的池数；造几局失败成局，两边日志里推进的 battle_id 不重复；停掉一个副本，约一个 etcd 租约周期后另一个副本接手它的片（`match_pool_owned` 涨到全部）。（R9 之后 `match_pool_owned` 按 role 计撮合段数，「之和 = 池数」改按 §12.8 第 5 步核。）

### 12.8 R9（2026-11-12：热池拆分）

Claude 只跑过 `gofmt -l`（改动与新增的 Go 文件都无输出），没有生成、编译或测试。本批改了 proto，先生成再编译；项目未上线，库清空重建。match 同时有「战斗剧本系统」会话的未提交改动（`global_pool_matcher.go` 的 `claimGroup` 成员段、`store.go` 的 `claimTx` 成员段、`gather.go`），本批避开了这些段落；红在别处先分清是哪批的。

1. **proto**（仓库根，no-unity，可与别的批次合并成一次）：
   ```bash
   pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
   ```
   本批改 `proto/match/match_authority.proto`（`MatchAuthorityTicketRecord.pool_region = 20`；OptionIndex 追加按首选机房的一条，同时删掉没有读者的 `formation_id` 与 `state,mode,team_id,…` 两条，剩 3 条，§6）与 `proto/team/team_authority.proto`（`TeamLaunchRecord.pool_region = 18`，OptionIndex 末尾追加一条；`mode` 字段只改注释）。期望：`go/proto/match`、`go/proto/team` 的这两个 message 多出 `PoolRegion` 与 `GetPoolRegion()`；C++ 生成物（`match_authority.pb.*`、`team_authority.pb.*`）随之变化，没有 C++ 代码引用这两个字段，随下次 C++ 串行编译（`/m:1`）即可。robot 的 vendor 不引用这两个字段，下次 `go mod vendor` 时同步。
2. **Go 编译、静态检查与单测**（工作目录 `go/match`）：
   ```bash
   go build ./... && go vet ./... && go test ./... -count=1
   ```
   通过标准：全部 PASS；没有 TiDB 时 `*TiDB*` 用例跳过属正常，如实报为未运行。新增纯单测：`poolgroup` 的 `TestPassesLocalSkipsCrossStage`、`TestPassesCrossEmitsOnlyCrossGroups`、`TestPassesZeroValueRunsBothStages`；`poolowner` 的 `TestSegmentsKeepSinglePlayerPoolsWhole`、`TestSegmentsSplitByRegionThenSubPool`、`TestSegmentsWithoutRegionsLeaveOnlyCross`、`TestSegmentKeysAreDistinctAndStable`、`TestSegmentWindowFollowsRole`、`TestRoleLabels`、`TestSegmentRolesMatchStressSummarize`（读 `tools/scripts/stress_summarize.ps1` 的 `$matchSegmentRoles`）；`config` 的 `TestSubPoolsValidation`、`TestSubPoolCountDefaultsToOne`（既有 `TestMatchServiceYamlLoads` 要能加载多出的 `SubPoolMergeSeconds`）；`logic` 的 `TestSegmentSliceReadsTheSegmentWindow`、`TestSegmentPassesFollowRole`、`TestRegisteredRegionsIgnoreAdmission`、`TestExpectedClaimRejectionCoversStaleClaims`；`authority` 的 `TestUnitPoolRegionNeedsEveryMemberToAgree`、`TestPoolSliceFilterBindsOnlySetConditions`。删掉了 `catalog` 的 `TestOwnerKeyGroupsSizesOfOneTemplate`（`catalog.OwnerKey` 已删）。失败时保留编译错误全文、用例名与断言原文。
3. **真库**（隔离的一次性 TiDB，库可清空；夹具自己按新 proto 建表，不要复用本批之前建的库；cwd `go/match`）：
   ```bash
   MATCH_IT_TIDB=127.0.0.1:<端口> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/authority/... ./internal/team/... -count=1 -run 'PoolSlice|QueuedLaunchRecordsCommonPreferredRegion|ReplayAndClaimAtomicity|CancelledMemberDoesNotPartiallyClaim'
   MATCH_IT_TIDB=127.0.0.1:<端口> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/authority/... ./internal/team/... ./internal/logic/... -count=1
   ```
   重点：`TestPoolSliceTiDBJoinRecordsPreferredRegion`（入队写首选机房列、机房段只读本机房、整池段读全部、`CheckCandidateIndexes` 通过）、`TestPoolSliceTiDBSubPoolAndWindows`（子池段按 player_id 取模与入队时刻、拆了子池的机房段只读等满合并时长的、跨机房段不分机房）、`TestPoolSliceTiDBProfileRefreshMovesTicket`（探测刷新把票移到新机房段，票号与最初等待不变）、`TestPoolSliceTiDBTeamLaunchSlices`（整队按自己的列与发起者取模读，列为 0 的只有不分机房的段读到）、`TestPoolSliceTiDBSecondClaimOfSameTicketsIsStale`（第二次认领回 `ErrClaimStale`、整笔回滚），team 包的 `TestTeamTiDBQueuedLaunchRecordsCommonPreferredRegion`（成员一致写共同机房、不一致写 0）。`role_identity_native_test.go` 按文案断言票行复核失败，文案没变，应照常通过。通过标准：全部 PASS，Skip 不算。旧库（本批之前建的）：迁移器会补 `pool_region` 列，但不给已有表建新索引，`CheckCandidateIndexes` 报缺索引、常驻进程拒启，这是预期的 fail-closed：清库重建，不要手补索引或改断言。
4. **部署合同与汇总脚本**：`pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`（match 的 yaml 只多了 `SubPoolMergeSeconds: 5` 与注释，通过标准与改动前相同）；`pwsh -File tools/scripts/test_stress_summarize.ps1`（夹具的 `match_pool_owned` 改成按 role 的三条、新增 `match_pool_segment_claim_total`，G1 的 owned 仍是 4；新增 G4 表的表头与 5 行断言）。通过标准：两个脚本退出码 0。
5. **多副本冒烟**（本机起 2 个 match 副本，清空 redis / mysql / etcd 后起服）：
   - 两个副本的 `match_pool_owned` 按 role 求和：pool = 每组 1 人的池数，region = 多人池数 × 机房数（battle 节点注册里的不同机房），cross = 多人池数，sub = 0（缺省不拆）。
   - 1V1 两人成局，`match_pool_segment_claim_total{role="region",outcome="committed"}` 增长。跨机房段的成局在本机不好造（两人首选机房要不同），由纯单测与真库用例覆盖。
   - 给 PVE_TEAM 模板 1 配 `SubPools: [{Mode: 5, ConfigId: 1, Count: 2}]` 后重启：`match_pool_owned{role="sub"}` 之和 = 多人 PVE_TEAM 池数 × 机房数 × 2；两人档两人同 zone 入队照常成局（计在 sub 或 region）；日志里没有 `has no effect`。
   - 停掉一个副本，约一个 etcd 租约周期后另一个副本的各 role 段数涨到全部。
   通过标准：以上各项成立，冒烟期间没有 `Failed to read pool tickets segment=`；优雅停机的排空报告不因成局冲突停在 HOLD。

## 13. Java 版（AGENTS.md §12）

两版共享的只有客户端契约（tip 码、白名单拒绝语义）与配置表数据。Java 版需要：

- 同样的 PlayMode / ZoneGroup 表（ContractSync 同步数据），撤销 `MatchProperties.java:28-30` 的禁查表约束；
- 逐级放宽范围与双向同意（`GlobalRegionIndex` / `GlobalQueueUnits`）；
- 每池单写者 + 一轮多局（`GlobalMatchService.round()` 现为单轮单局）；
- 肇事者剔除、离线撤票推广到单排与 PVP、拒绝码按 Go 口径对齐；
- Java 已有而 Go 待追的：离线撤单位（只写进本仓文档，不往 PARITY 加行）。单人即时成局 Go 第二批已追平。按可分配状态 + 房间数选节点（R6）Go 已于 2026-11-05 追平（未编译）：Java 版 battle 周期把 accepting / roomCount 写进 Redis 目录，Go 版只在变化时写 etcd、房间数随建房应答捎回（§5.1）。两版服务端内部实现不同（AGENTS §12 允许），对外语义相同：match 只选可分配的节点、按房间数比较负载。Java 已有等价，不往 Java 仓 PARITY.md 加行。
- 前提：Java 全局匹配路径目前只有源码、没有编译和运行证据（match-spec.md:2418-2447），要先让它跑通。
- 人数档真人 PVP 计分组排（开头一节，R7，2026-11-05）：Java 版待做，已登记 Java 仓 `PARITY.md`「2026-11-05 人数档真人 PVP 计分组排（Java 待做）」。Java 版人数档 PVP 的撮合本身也还没有（11-05 核对：game-match 主代码里 `PVP_TEAM` 只出现在观战可见性表），要一起做。客户端契约多两个 team_error 码（第 9、10 条），表数据两版共用，Java 版开战入口要回同样的码；没有新消息和字段。
- 评分按模式分开、榜按模式、回合打满判平（§8，R5，2026-11-05）：Java 版待做，已登记 Java 仓 `PARITY.md`「2026-11-05 评分按模式分开、榜按模式、回合打满判平（R5，Java 待做）」。11-05 核对：Java 的 `match_rating` 每人一行，`RatingRankOutbox` 只开 `board_type` 1 一张榜。`board_type` 1 / 2 / 3（1V1 / 5V5 / PVP_TEAM）是客户端契约，两版同值；计分局禁观战 Java 已有（`SpectateVisibilityPolicy`）。2026-11-12 取值登记进 `rank.proto` 的 `RankBoardType`、客户端按模式分页签：Java 待做（同步 proto 枚举、开榜用生成常量），已登记 Java 仓 `PARITY.md`「2026-11-12 榜类型登记进 rank.proto、客户端论剑榜按模式分页签（Java 待做）」。
- 后台工作按片分工、评级停写不卡消费（§5.2、§8 R5「停写之后的新局」，R8，2026-11-06）：Java 版待做，2026-11-10 补登 Java 仓 `PARITY.md`「2026-11-10 全服匹配第四批：后台工作按片分工、评级停写不卡消费」。这是服务端内部改动，没有客户端契约。
- 热池拆分（§5.2.1，R9，2026-11-12）：Java 版待做，已登记 Java 仓 `PARITY.md`「2026-11-12 全服匹配热池拆分：按首选机房与子池拆写者」。服务端内部改动，没有客户端契约；Java 的全局匹配还是单轮单局，要先追平「每池单写者 + 一轮多局」再做这一项。

状态：Java 版全部待做（负责人 11-05 起定 Java 留作 todo，只登记、不写 Java 代码）。对应记录都在 Java 仓 `PARITY.md`：表格里「全服匹配：按怪物战斗模板分池…」一行（第一至第三批）、上面各条注明的补充条目，以及 2026-11-06「后台分片归属哈希修复与收件箱对账分工」（三个服务共用的打分实现）。表格里 2026-10-15「全球匹配区域优先与等待扩区」一行记的是本文之前的 Go 现状，2026-11-10 已把它的状态列改成指向「全服匹配」一行。这里一律按标题引用 PARITY 记录，不按行号：行号会随别的会话插行失效，原先写的「第 149、187 行」就是这样失效的。

## 14. 风险与未决

- 默认阈值（10 / 20 / 30 秒、180ms）都没有数据依据，冷门模板可能等待偏长；R0 指标出来后按模板定标。
- 双向同意让刚入队的人在冷门池里要等自己的阈值；这是"同 zone 优先"的代价。
- 未登记大区的 zone 跳过大区层；开新区要同步维护 ZoneGroup 号段。
- 单写者在 S3 量级可能不够：2026-11-12 已按首选机房与子池拆写者（§5.2.1），效果没有压测数据；跨机房段与机房段会争同一批老票，冲突率看 `match_pool_segment_claim_total{role="cross"}`。
- 本文所有吞吐数字都是推算，没有压测证据前不宣称性能提升（AGENTS.md §6）。
- 期限按模板（D6）的代价：已持久 Create 原件之后失败（含节点准入拒绝）的成局只能等唯一 SQL 终局，玩家被占用到"提交时刻 + 120 秒 + 模板时长"。缺省模板仍是 300 秒；开放长模板（如 3600 秒）前要先有"确定未建房即提前终局"，否则一次建房失败会把整组人卡一小时。2026-11-05 起分三层收窄：R6 让 match 不选正在排空的节点；落库前预检 `rejects_new_battles` 收窄"选中之后才排空"的窗口（battle 侧填值待 R6）；提前终局由数据覆盖审计会话评估落码。三者都落地之前不开放长模板。
- PVP_TEAM 只在 PlayMode 第 6 行（`battle_config_id=1`）开放；客户端若按 0 入队会被白名单拒绝。
- 计分组排（R7）的缺省值（组队加成每人 25 分、车队差步长 10 秒、组队成员评分差上限 500 分）没有数据依据，要按车队胜率、PVP_TEAM 的等待分布和被评分差挡下的开战次数定标。车队对等会让大车队在冷门时段多等，最长约 3 个步长（5V5 的 5 人队打单人）。
- R7 改了全局策略指纹（计分模式集合、组队加成、组队成员评分差上限）：已有库里 `match_global_policy` 的旧摘要会让新进程拒启，本地与 dev 清库重建；上线后改这两项要停全部副本受控调整。
- R7 开启前已在排队的 PVP_TEAM 票没有评分快照（`rating_centi=0`），与新票的分差超过容差，只能等终态兜底；上线前本来就清库，不单独迁移。
- Elo 不做组队修正，车队的评分会比同水平单排涨得快；要不要修正看 PVP_TEAM 上线后的胜率数据。
- R6 的残余：① battle 写可分配状态的前提是「回读看得到自己的键、注册链空闲」；初始引导时服务键 CAS 失败或超时后，注册链在 1 秒退避里没有在途 CAS，这个窗口里的写仍可能抢先建出服务键（与 gate 每 10 秒写人数同一类既有残余），只在注册失败重试时出现。② 负载读数只在本副本选中某台建房时刷新，各副本看到的读数不同，没有读数的节点会先被多选；均衡效果要按 AGENTS §6 压测验证，没有对比表前不宣称改善。③ 混版本期间不上报的旧 battle 显得更闲、多分到成局。④ 排空标签每 5 秒读一次、对账 1 秒一拍，排空开始到 match 看到 CLOSED 最长约 6 秒加 watch 延迟，期间选中的节点仍由 §5.5 的落库前预检或准入闸拒绝、熔断。
- 池归属依赖 etcd 注册：成员表为空（etcd 不可用且本地快照也空）时没有副本撮合排队池，只剩单人档即时成局；`match_pool_owned` 掉到 0 要告警。
- 投影表 `match_ticket_region` / `team_launch_region` 已于 2026-11-05 退役（§6）；本地与 dev 的旧库要清库重建，迁移器不删旧表、旧列。
- **共享工作区提醒**：ZoneGroup / PlayMode 的 schema 与 xlsx 已落盘。之后任何会话跑 `dev.bat gen`，都会生成这两张表的三端代码；C++ 编译前必须按 §12 第 2 步补登记，否则 table 库链接失败。

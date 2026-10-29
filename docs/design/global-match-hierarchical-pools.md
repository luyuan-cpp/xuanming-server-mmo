# 全服匹配：按模板分池、逐级扩大范围、每池单写者

> **状态**：设计已拍板（2026-10-24，负责人），分批落码中。第一批（纯函数包与两张新表）已落源码，**未编译、未测试，待 Codex 验证**；第二批（接线）等并行会话的在途改动落稳后再动，见 §11。
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
- **分片（shard）**：池按 `H(mode, battle_config_id) mod 256` 归到 256 个租约分片之一，同一模板的各人数档落在同一分片，为以后跨档合池留接口。
- **写者**：持有某分片租约的 match 副本，负责该分片内全部池的撮合。

## 3. 范围树与逐级放宽

实现：`go/match/internal/scope`（纯函数，第一批已落）。

### 3.1 大区从哪来

新表 **ZoneGroup**（`data/schema/zonegroup_table.proto`）：每行一个 zone 号段 `[zone_begin, zone_end]` → `group_id`，同一大区可以多行，号段不重叠。初始数据：大区 1 = zone 1–100，大区 2 = zone 101–200。

- 号段式而不是逐 zone 登记：开新区落在预留号段里不用改表。
- 没有落在任何号段的 zone 视为**未分大区**：放宽时从 zone 层直接到全服，记指标 `match_scope_ungrouped_total`（第二批）。这是匹配质量维度，选择不拒绝入队（可用性优先），风险是新区没登记时少了中间一层。
- 只描述玩法分组。data_service 的 `Regions`（zone → 存储集群）是存储布局，二者独立：搬存储不该改变谁和谁匹配（AGENTS.md §11.6「是什么」与「摆在哪」分层）。

### 3.2 放宽规则

- 票据入队时写入 home zone 与大区号（第二批：`match_authority_ticket` 新增 `zone_group`；`zone_id` 改为真实 home zone，读 `player_route.home_zone`，不再用当前所在场景的 zone）。之后撮合与提交点都读票上冻结的值，不再查表。滚动换表时不同副本写入的大区号可能短暂不同，只影响这段时间的大区层合并，不影响正确性。
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

输入一个池的一页单位（写者从 `(state,mode,config_id,team_size,enqueued_ms)` 索引读出，已过滤过期与 `not_before`），输出不共用单位、不共用玩家的若干候选组：

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

### 5.1 组件

match 副本形态不变（RPC 前端 + 写者 + formation 执行者），共连同一个逻辑 TiDB。新增两张小表：

- `match_pool_shard`：256 行，`shard_id, owner_instance, epoch, lease_until_ms`。
- `match_replica_heartbeat`：每副本一行，用于算公平份额。

不新增进程、Redis 键或内存权威；不用 etcd、Kafka 分区或 TiCDC 做租约。

### 5.2 租约

- 每副本每 2 秒续约一次；本地有效期 = 续约语句发出前的单调时刻 + 5 秒，过期立即停止在该分片上开新 claim（失租即停）。
- 份额 = ceil(256 / 存活副本数)，超额主动释放、不足去抢过期分片，易主时 epoch+1；优雅退出前释放全部分片。仓内先例：`rank_mail_reward` 的 `lease_until_ms`。
- 分片数与哈希**不进**全局策略指纹：租约只管调度，混跑期间映射不一致只会多些冲突，不会成错局，调分片数不用全停。

### 5.3 写者一轮

1. 对持有分片里的每个池：一次索引 seek 判空；非空则读队首一页（默认 256 行，按需自适应到 2048），页后过滤 `team_id=0`；整队 launch 走 `team_launch` 新加的池索引。
2. 撮合前批量检查成员在线状态：已下线 / 找不到的票由系统撤（state=4）；重连中的本轮跳过。
3. 调 `poolgroup.Form` 得到候选组；维护本地"在途票"排除集，避免下一轮再选中还没提交完的组。
4. 先占 gather 槽，再并发 claim；连接池拆成 rpc / matcher / gather 三个，gather 槽可配，默认 256。
5. 整轮截止时间 = 0.8 × 轮间隔（`MatcherIntervalMs` 在全局路径生效）。
6. 维护（每 5 秒、有上限）：过期票置 4；已禁用模板的票由写者撤；投影修复失败不再阻断成局。

### 5.4 单人档即时成局

Join 事务提交后，在同一 RPC 里用**独立事务**立即 claim（与 Java `tryImmediate` 同形态，不合并进 Join 事务）。失败就留在 state=1，由写者兜底。Join 即时 claim 与写者争同一张票时，只有一方在票行复核上成功。

### 5.5 提交点与正确性论证

- **唯一互斥依据是 `claimTx` 的玩家锁 + 票行锁 + 代次 / 状态 CAS**（store.go:491-574，锁序不变：P 升序 → 入场守卫 → 观战 → 票行升序 FOR UPDATE）。租约不承担正确性。
- 软围栏：在 `claimTx` **提交前最后一句**做非锁定读，核对分片 owner / epoch / `lease_until_ms > 数据库时间`；claim 的截止时间不晚于本地租约截止。失租后迟到的提交最多一次，且已经过提交点全部复核，同一玩家不可能进两局，满足 D-16"旧 leader 不能让同一玩家进两局"。不锁租约行（会让同分片的 claim 串行）。
- 提交点新增复核（第二批）：
  - 范围树：用加锁读出的票（home zone、大区号、入队时刻）与数据库时钟算 `scope.Admit`，阈值取策略值；
  - 组内评分极差 ≤ 组内**等待最长成员**的容差（计分模式）。撮合时用的是锚点容差（§4 第 8 条），而容差随等待单调不减，最长等待者的容差不小于锚点容差，所以提交点复核只会更宽、不会误拒撮合出的组，也不需要把锚点身份随 claim 传下去；
  - 认领方模板目录里该 `(mode, config)` 仍开放、人数在上限内；
  - 组内只有一个单位时豁免跨机房等待（D3）。
- 成局失败（PrepareBattle 确定性拒绝：离线、不在场景、已在战）时只剔除肇事者，其票置 4；预组队整体撤；其余成员保留原入队时刻回队，`not_before` +1 秒。RPC 结果未知不定罪。
- formation 记录 `recovery_shard`；`Pending` 与超期恢复只扫本副本持有的分片，另加低频孤儿兜底，替代全副本抢同一个队头。

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

- 本表是 [configurable-templates.md](configurable-templates.md) §6 所设计 PlayMode 的最小子集，**字段编号以 `data/schema/playmode_table.proto` 为准**。以后扩 kind / rated / rule_set 等列时追加新字段号，不重排现有的 1–5。
- 只开放现在实际在用的组合：PVE 只有副本 1（与原 yaml `PveTeamSizeByConfigId {"1":5}` 一致）；PVP 暂借副本 1 的 `time_limit`（robot 冒烟就填 1）。给 PVP 建专用竞技场 Dungeon 行要客户端同步改，列为后续。
- match 启动时读表编译成不可变目录，校验失败拒启（规则写在 schema 注释里）。
- 入口：JoinQueue、StartTeamMatch、ChallengePlayer 不命中或 `enabled=false` 回 16002。人数上限 = min(`team_size_max`, `Dungeon.max_team_size`, 5)。
- 删掉 yaml、K8s ConfigMap 与策略指纹里的 `PveTeamSizeByConfigId`：开新本、调人数不再停全部副本。
- 提交点复核认领方目录（§5.5）；`CommitProfile` 刷新路径也校验目录，禁用模板的票不能靠持续探测续命；写者对"明确禁用"的票系统撤票，对本副本不认识的模板只跳过。
- 战斗期限按 D6。

## 8. PVP

- 白名单按 §7；评分读失败时计分模式 Join 拒绝，不再静默按 1500（global_match.go:45）。
- 评分按 `(player_id, mode)` 分开存。
- 计分局禁止观战（复用 16018，观战列表排除计分局）。
- 1V1 先后手由 formation 种子决定，不再锚点恒为先手侧（gather.go:755-758）。
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
| R0 | 全局路径低基数指标（`queue_depth{mode,team_size}`、`wait_seconds{mode,pass}`、`claim_total{mode,pass,outcome}`、`round_seconds`、`gather_inflight`、`expired_swept_total`、`culprit_total`、`window_overflow_total`、`scope_level_total{level}`）；robot 加 JoinQueue 压测动作；`stress_snap` 拉 :9170；`stress_summarize` 加匹配段；跑现状基线 | 待做（`metrics.go` 正被观战会话修改） |
| R1 | 票据写真实 home zone 与大区号；肇事者剔除；在线检查撤票；过期清扫；机房内按负载选节点（P2C + 熔断），删除"最小 NodeId"；整队先占槽再 claim；单人即时成局；`MatcherIntervalMs` 生效 | 待做（等在途改动） |
| R2 | 模板目录接线、白名单、期限按模板、PVP 平局阈值推导 | 待做 |
| R3 | 分片租约 + 一轮多局 + 范围树接入撮合与提交点 | 待做（依赖 R1） |
| R4 | §6 TiDB 热点治理 | 与 R3 同批或紧随 |
| R5 | §8 PVP 口径 | 计分功能上线前 |
| R6 | battle 上报 `accepting` / `room_count`，按负载放置闭环 | 待做 |

**文件归属（2026-10-24 与并行会话约定）**：

- 「数据覆盖审计交接文档」会话在改 `authority/store.go`（formation 恢复出口、`checkWork`、formation 状态迁移）、`logic/global_match.go`（`globalDestroy` / `releaseGlobalFinished`）、`authority/entry_guard.go`。第二批要等它通知函数名与行号后按段落避开。
  - **2026-10-24 该会话已收尾**（social-match-battle#0，未编译、待 Codex；通知时本会话已结束，故写在这里）。它改过的段落，行号为 10-24 工作区：`authority/store.go` 约 779-806（`ErrAwaitingExpiredOutcome`、`awaitsExpiredOutcome`、`expiredOutcomeSQL`、`CountStrandedExpiredFormations`）、约 824-825（`AcquireWork` 的到期门禁）、约 869 起（`CreateIntentPersisted`）、约 919（`Pending` 的 SQL 加 `AND NOT expiredOutcomeSQL`）；`logic/global_match.go` 约 409-420（`formationDestroyAuthority` 接口与 `globalDestroy` 的无原件短路）、约 530-570（`globalAbortPrepared` 在 `Finish(false)` 之后计数）；`logic/gather.go` 约 177（`globalDestroy(svcCtx.Authority, …)` 调用点）；`logic/matcher.go` 约 138-139（到期巡检 `safego.Loop`）；新文件 `logic/global_destroy_test.go`、`logic/global_expired_audit.go`、`authority/formation_recovery_exit_test.go`、`authority/started_formation_exit_test.go`。`entry_guard.go`、`checkWork` 最终没有改。第二批动这些文件前先重读，按段落避开。
- 「全服观战架构设计」会话在改 `constants/errors.go`、`metrics/metrics.go`、`spectate*.go`、`watchbattlelogic.go`、`global_spectator.go`、`authority/spectator.go`、`entry_guard.go`（只加哨兵错误）、`match_authority.proto` 的 `MatchSpectatorIntentRecord`。它以后的可见性改动要碰 `gather.go` 两处，`gather.go` 由本设计负责，届时在本设计的版本上补。
- `match_authority.proto` 两边改的是不同 message；proto 生成只由 Codex 在两边落稳后统一跑一次。

## 12. 第一批验证清单（交给 Codex）

Claude 未执行任何编译、测试或导表。

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

## 13. Java 版（AGENTS.md §12）

两版共享的只有客户端契约（tip 码、白名单拒绝语义）与配置表数据。Java 版需要：

- 同样的 PlayMode / ZoneGroup 表（ContractSync 同步数据），撤销 `MatchProperties.java:28-30` 的禁查表约束；
- 逐级放宽范围与双向同意（`GlobalRegionIndex` / `GlobalQueueUnits`）；
- 每池单写者 + 一轮多局（`GlobalMatchService.round()` 现为单轮单局）；
- 肇事者剔除、离线撤票推广到单排与 PVP、拒绝码按 Go 口径对齐；
- Java 已有而 Go 待追的：单人即时成局（`tryImmediate`）、按 `accepting` + `roomCount` 选节点、离线撤单位（只写进本仓文档，不往 PARITY 加行）。
- 前提：Java 全局匹配路径目前只有源码、没有编译和运行证据（match-spec.md:2418-2447），要先让它跑通。

状态：待做。PARITY.md 第 149、187 行已过期，需按当前源码更正后再登记新行。

## 14. 风险与未决

- 默认阈值（10 / 20 / 30 秒、180ms）都没有数据依据，冷门模板可能等待偏长；R0 指标出来后按模板定标。
- 双向同意让刚入队的人在冷门池里要等自己的阈值；这是"同 zone 优先"的代价。
- 未登记大区的 zone 跳过大区层；开新区要同步维护 ZoneGroup 号段。
- 单写者在 S3 量级可能不够，需要按机房拆写者（§6 条件项）。
- 本文所有吞吐数字都是推算，没有压测证据前不宣称性能提升（AGENTS.md §6）。
- **共享工作区提醒**：ZoneGroup / PlayMode 的 schema 与 xlsx 已落盘。之后任何会话跑 `dev.bat gen`，都会生成这两张表的三端代码；C++ 编译前必须按 §12 第 2 步补登记，否则 table 库链接失败。

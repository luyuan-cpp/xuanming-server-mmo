# 「战斗抽出去」这套做法能套到哪些玩法:MOBA / MMO / SLG 定谳

**Created:** 2026-09-06
**状态:** 架构定谳(判据与适用边界已拍板;各玩法的实现进度以各自文档为准,本文不记录完成度)
**关联:** [moba-battle-target-architecture.md](./moba-battle-target-architecture.md)(会话制目标形态)、[turn-based-battle-server.md](./turn-based-battle-server.md)(D1-D4 原始决策)、[battle-transport-decision.md](./battle-transport-decision.md)(传输选型)、[moba-ds-server-interview-qa.md](./moba-ds-server-interview-qa.md)(DS 内部设计)、[scene-creation-architecture.md](./scene-creation-architecture.md)(scene 编排)、[player-async-save-loss-windows.md](./player-async-save-loss-windows.md)(scene 存盘链)、[slg-server-complete-framework.md](./slg-server-complete-framework.md) / [slg-server-framework-complete.md](./slg-server-framework-complete.md) / [slg-march-system-complete.md](./slg-march-system-complete.md)(SLG 设计稿)

> **一句话:能不能「像 battle 那样抽出去」,看的不是玩法类型,而是玩法里有没有「会话形状」的段——有始有终、入口一份快照、出口一份结果 DTO、中间态外界不需要。**
> MOBA 整局就是会话,能。MMO 和 SLG 各自一半能一半不能:会话段(副本对局、竞技场、SLG 战斗结算、跨服战场)能;常驻世界段(MMO 主世界、SLG 大地图、城建)按定义不能「可丢」,它们的上限是「随时可杀但不回档」,那是另一套手段。

---

## 0. 本文回答什么

回合制 battle 已按会话制形态抽成独立节点([turn-based-battle-server.md](./turn-based-battle-server.md))。随之而来的问题是:MOBA 显然可以照做,那 **MMO 和 SLG 能不能也这么做?**

本文先把「battle 抽出去到底靠什么」这把尺子校正一遍(§1),再用它逐个量 MMO(§2)、SLG(§3)、MOBA(§4)。§5 是三张表的汇总与待办。

**本文不是**:新的实施计划,也不推翻任何既有决策。它只补一件既有文档没写的事——**判据的适用边界**。

---

## 1. 尺子:battle 抽出去靠的到底是什么

### 1.1 顶层判据是「可丢」,零写权是它的必要条件而不是它的因

[moba-battle-target-architecture.md](./moba-battle-target-architecture.md) 的一句话判据(§开头)是「战斗进程能不能被随时 kill 掉而不损坏任何持久数据」。角色表把「有没有玩家库连接」与「状态可不可以丢」列为**并列两列**,三条存储红线并列服务于同一个判据。

所以因果方向是:**「写玩家库 ⇒ 不可丢」**(零写权是维持可丢的必要条件),不是「零写权 ⇒ 可丢」。四条契约并列共同兑现 kill 判据,少任何一条都不成立:

| 契约 | 出处 | 少了它会怎样 |
|---|---|---|
| battle 对玩家权威数据零写权、不连玩家库 | D1 / 不变量 4 / 红线 1 | 崩溃即损坏持久数据 |
| 结果在 battle 之外先落地一次并幂等应用 | 契约③ / 不变量 2 | 崩溃后奖励丢失或重复发放 |
| 局中闸(`InBattleComp`)+ 结算串行化 | D4 / 不变量 3 / 红线 2 | 快照与权威分叉,残血带不出战斗 |
| 超时 → 解冻/作废补偿(scene reaper) | §3.2 | battle 被 kill 后玩家**永久**冻结,进不了下一场 |

最后一条最容易被漏掉。只保留零写权、拿掉 reaper:battle 被 kill 后结算永不到达,`InBattleComp` 与 `battle:lock` 永不摘除,玩家永久进不了战斗——这本身就是「持久协调态被损坏」。**验收判据里的 kill -9 一项明写「该局判 abandoned、补偿结算」,不只是「不损坏数据」。**

### 1.2 「可丢」≠「无状态」

battle 仍然是**有状态、房间钉定**的游戏服务器:一局只活在一个进程内存里,票据签给具体实例(`battle_node_id` + 实例 UUID),客户端拿到的是 ip:port 不是服务名,换版只能排空不能滚动,扩缩容按房间数装箱([battle-transport-decision.md](./battle-transport-decision.md) §4)。

这组部署性质派生自「局内状态在内存且允许丢」这个**实现选择**,不派生自 single-writer。判「某玩法能不能抽」时不要拿「它有状态」当否定理由——battle 也有状态。

### 1.3 真正决定「抽得动」的是:单元是不是一段会话

上面四条契约之所以做得起,根子在于**被抽出去的单元是一段会话**:

- **有始有终**——生命周期由外部编排者(match)开启和终结,不是「一直在那儿」;
- **入口是一份值快照**——不是引用,不是可回查的句柄,进程内查不到 db/data_service 连接(契约②);
- **出口是一份可序列化的结果 DTO**——不是活对象;
- **中间态外界不需要**——所以才允许丢。

**主世界没有这个形状。** 它没有终点,状态即产品,中间态就是外界要的东西。所以判一个玩法能不能抽,正确的问法是「**这个玩法里哪一段是会话**」,而不是「这个玩法属于哪个品类」。

---

## 2. MMO:三层要分开判

### 2.1 主世界 scene:范畴错误,不是能力问题

**判定:不能做 battle 式「可丢」。可达目标是「有界 RPO + 可重启恢复」。**

主世界的状态就是产品本身,「打完丢掉」没有意义。[moba-ds-server-interview-qa.md](./moba-ds-server-interview-qa.md) Q1 的对照表已写清:DS 是「结束后状态丢弃」,Scene 是「状态持久化到 DB」。把主世界往「可丢」推是范畴错配。

**现状(核实过,别重查):**

- scene 是玩家权威数据的唯一 writer([cross_server_architecture_principle_zh.md](./cross_server_architecture_principle_zh.md) 规则 #8、D1),直连 Redis 异步 Save(hiredis),另发 Kafka DBTask 由 Go db 服务写 MySQL——**所以「不连库」在主世界不成立**;
- 持久化 = 每玩家默认 300s 的分摊周期存盘(`SCENE_PLAYER_SAVE_INTERVAL_SECONDS` 可调)+ 退出/停机 drain;`dirty` 只用于 proto-compare 跳过未变更的写,**不触发即时存盘**;
- 硬 kill(SIGKILL/OOM)回档到上次成功周期存盘,最多丢一个周期;**已持久化快照不被污染**(`PlayerLastPersistedSnapshotComp` 只在成功后更新);优雅停机走有界 drain;
- 跨节点交接没有存盘屏障,生产以 `AllowUnsafeCrossNodeHandoff=false` fail-closed 封住([player-async-save-loss-windows.md](./player-async-save-loss-windows.md) §4.3);
- C++ `player_migrate` 协议存在但只迁 7 个 ECS 组件,不完整。

**要往前走,是两件正交的事,都不要求把 writer 搬出 scene:**

1. **压 RPO** —— 保持 writer 在 scene,缩短快照间隔 + 把 op 级日志/WAL 扩展到可恢复操作并在重连时 replay(仓内已有同类设计:`cross-server-rollback-gap-fixes` 的 transaction_log + PostCrashReplay、[guild-actor-architecture.md](./guild-actor-architecture.md) 的 Kafka WAL 先写后改内存),或走 [player-async-save-loss-windows.md](./player-async-save-loss-windows.md) §4.1 结尾给的 durable outbox / 从 MySQL 分表重建;
2. **单写者安全** —— per-player 交接 epoch / fencing,解决「旧节点仍活着还能写 Redis 而 Go 已改派」的分区双写(见 [scene-owner-reentry-barrier.md](./scene-owner-reentry-barrier.md):owner_epoch 设计成塞进 scene 现有存盘 Lua 的原子 CAS,writer 仍留在 scene)。

> **纠正一个常见误判:**「要把主世界变成 battle 式,必须把玩家数据 writer 移到外部数据服务」——**不成立**。规则 #8 只禁止多个场景服并发写同一玩家,不要求 scene 是物理持久化者;`data_service` 是跨区路由代理,不是为持久化/可丢而设。

### 2.2 副本 / 镜像:两条宿主路径,性质由选型决定

**判定:能,但要看走哪条路径;现有的 instance 路径不是 battle 式。**

| 路径 | 模型 | 玩家态 | 场景态 |
|---|---|---|---|
| `SceneTypeInstance` 副本(现有) | WoW 式副本服:玩家仍是节点内 entt 实体,writer 随人走,周期存盘 | 可恢复(同主世界) | 0 人 300s 回收;镜像 30s | 
| battle 房间(D1「人不动、数据动」) | scene 保持 writer,房间只持派生快照,吐一条结算事件 | 权威留在 scene | 可丢 |

**现有 instance 路径的生产事实:**已有位置的玩家做跨节点交接被 `AllowUnsafeCrossNodeHandoff=false` 拒绝,所以生产里可达的副本只剩「同物理节点」——与主世界同进程、同 kill 域;独立的 `scene-instance` Fleet 对已有位置玩家不可达,只对首次落点或 dev 开关开放。**镜像应从「副本」里剔出单独说**:它刻意与源场景共置在 `scene-world` 节点上复用驻留数据,不进 instance 池、不触发跨节点交接,玩家态命运与主世界完全相同。

另外注意措辞:普通副本(dungeon run)的 300s 闲置期是**刻意保活**以撑过掉线重试窗,其场景态「丢了就没了」是被迫接受的失败模式,**不是** battle 那种「按设计可丢」。

**要让一段副本玩法拿到 battle 性质**,把它建成 battle 房间即可(快照入场 + 零写权 + 结算事件回 scene 幂等应用),这条路本仓已经现成。代价是:**当前 battle 内核是回合制逻辑房间**,D2 明写「不需要 AOI/Movement/20FPS tick」,实时空间型副本要落到 battle 节点还需要新增 tick/移动内核。

### 2.3 竞技场 / 战场类会话玩法

**判定:回合制/逻辑房间型 —— 能,已落码;实时空间型 —— 无决策。**

- **回合制/逻辑房间型**(匹配遇怪、场景切磋 PK、回合 5v5):路径 = D1「人不动、数据动」+ D2 全局池 battle 节点;scene 侧 `InBattleComp` 冻结清单 + `battle:lock` + 离线 pending + deadline reaper 已落码。其 kill 安全性是**一致性保证**(不双花、不丢权威数据),**不是可恢复保证**——中局 kill 即整局作废。
- **实时空间型战场/竞技场**:D2 明确 battle 节点不做空间模拟;仓内 scene 代码**没有任何「战场」实现**(唯一命中是 `node.pb.go` 把「跨服战场」注在 `CrossServerNodeService=21` 上);[agones-scene-node-high-density.md](./agones-scene-node-high-density.md) 里 Fleet 表提到的「战场」只是承载规划,Agones 三阶段从未上过集群。**规划落点未定,不能说「今天跑着的战场是 instance scene」。**

---

## 3. SLG:三层要分开判(注意:全部只有设计稿,零实现)

> 前置声明:本仓的 `slg-*.md` 是设计/面试稿,没有对应代码。下面判的是**设计形态**,不是现状。

### 3.1 BattleService:比本仓 battle 更好抽

**判定:能,且比本仓 battle 节点更彻底——但整条链是否 kill-safe 取决于消费方。**

SLG 的战斗是**无状态请求/响应 worker**:`BattleReport = SimulateBattle(attacker_snapshot, defender_snapshot, seed, terrain)`,单场 < 1ms,不持有任何地图/玩家/连接数据,客户端只拿战报本地回放。

于是本仓 battle 那一整套**客户端面基建在 SLG 没有挂靠点**:没有客户端直连、没有票据、没有房间生命周期、没有 tick/心跳、没有重连追帧。能搬过去的只有两条契约:契约②「吃快照吐结果、进程内不出现 db 连接」和契约③「回写幂等」。

**但契约③要按 SLG 形态重写,不能照搬:**

- SLG worker 是带 seed 的确定性纯函数,重投/重算天然等价;战报已按 `battle_report_id`(snowflake)落库,**落地去重已有**;
- 缺的是 **MapService 消费 `battle_result` 时的显式判重**。正解是**实体状态门**:仅当 march `state == BATTLING` 且 `battle_id` 匹配才应用,应用后翻 `RETURNING`/销毁,并与 Redis 状态写入在同一原子步之后再 ack。
- **不要**在 MapService 前面再加一个同步 `put_unique` 的外置结算服——那会往 <1ms / 大规模攻城的热路径塞一次同步 DB 往返,与文档既定的「Redis 为准 + 异步 db-write」模型相悖。
- **另一条更危险且文档完全没覆盖的**:崩溃恢复流程会「补发到达事件」,已打过但结果未落 Redis 的行军会被**再次触发碰撞、再开一场战斗**。恢复前必须查该 march 是否已有 `battle_id` 落地。
- MOBA 契约③ 的「心跳超时 → abandoned 补偿」半段在 SLG 无对应物(worker 无会话),不移植。

MapService 同时扮演 MOBA 模型里的 Allocator(打包快照)与结算服(应用结果)——这在 SLG 里**不是角色混淆**,SLG 本就没有分配语义,世界服在内存应用结果是通行做法。

### 3.2 MapService:不是 battle 的反面,只是粒度不同

**判定:一个赛季内的一张图不可拆;但在赛季/战场粒度上,它本身就是 battle 形态。**

常见误判是把 MapService 说成「可恢复不可丢,与 battle 相反」。按判据表自己的口径,**「重连/重排,可恢复」就是「可丢」**——只有持久层丢才算事故。MapService 的内存态:

- 行军 = Redis 里 `{path, start_time, speed}` 的**物化视图**,先写 Redis 再懒计算位置,崩溃后三个值读出即完美恢复。这是教科书式的 write-ahead + 确定性派生;
- 格子 = dirty Region 定期 flush 的**写回缓存**,崩溃回滚到上次 flush,设计上接受有界丢失(Redis checkpoint 口径 ≤10s)。

**真正的差别是三条,都不是「可丢性」:**

1. **单元粒度** —— battle 一局一房间;MapService 一张图 / 一个赛季 / 一个跨服战场一实例。两者都是「单元内不可拆、单元间水平扩」。**跨服战场就是标准的 battle 形态**:快照进、独立跑、MQ 出结果;赛季管理(新赛季起新实例、旧赛季关闭保留 DB、一台机跑多个)也是同一形状。
2. **持久层写入路径** —— battle 红线是不写玩家库;MapService 必须持续写回(它是格子/行军状态的唯一写方)。它对应 MOBA 文档里与 battle **并列的 scene 角色**,不是「玩家持久化数据唯一权威」的业务服。
3. **恢复代价** —— 补发到达事件、重建 occupant / 定时器 / tile_occupants 索引、重跑碰撞检测。

「不可水平扩」仅指**单张图在一个赛季内**不可拆分(跨进程行军/碰撞要分布式事务,代价过高),不是「不能抽出去」。

### 3.3 Build / Army / Alliance:业务服角色,不走这条路

**判定:能拆成独立进程,但拆出来的不是 battle 那种可丢节点。**

- 对应判据表的**业务服**角色(持久层写方,状态不可整体丢);
- **部署形态两份文档口径相反,引用时必须指明出处**:[slg-server-complete-framework.md](./slg-server-complete-framework.md) §2.4 建议 Map+Build+Army 合并为一进程、Build/Army/Alliance 单实例、仅 Chat/Rank/Alliance 可选独立;而 [slg-server-framework-complete.md](./slg-server-framework-complete.md) §2.1/2.2 把它们画成经内部 gRPC/Kafka 连接的独立业务微服务并标注「可按玩家/联盟 ID 分片」。后者正是业界通行的「大地图服单实例 + 玩家服按 uid 分片」。
- 状态性质是**进程内存权威 + write-behind 异步落盘**(MMO 世界服同型),不是 DB 事务权威。
- **建筑定时器是从 `finish_time` 派生的缓存,允许丢;不允许丢的是 `finish_time` 本身**。恢复 = 按 finish_time 重挂或惰性结算,不需要行军那种碰撞注册重建。
- Build 该不该与 Map 同进程,真判据是「建造完成回写 Tile + 视野推送」这个耦合,不是「它有状态」。

---

## 4. MOBA:对回合制的真实增量比想象的小

**判定:能,骨架直接继承;但回合制的几项是短局退化形态,长局必须改。**

### 4.1 真新增只有两项

| 项 | 判定 |
|---|---|
| **定 tick 实时帧调度** | ✅ 真新增,回合制没有(D2 明写不需要 20FPS tick) |
| **实时快照/增量广播** | ✅ 真新增 |
| 强确定性 | ❌ **不是新增**——D7 已把引擎定为「快照+指令流+种子 → 纯函数」,不变量 5 写明所有随机只走种子 RNG,观战(D8)已在消费它 |
| 录像/回放 | ❌ 不是新增——D7「回放 = 免费」,回放持久化是二期预留;判据表还把 replay 列在「丢了只影响回放功能」的可丢层 |
| 断线追帧 | ⚠️ **取决于未拍板的同步模型**:状态同步下重连 = 最新全量快照 + delta(回合制 D7 已是这个形态),**不需要覆盖重连窗口的指令缓冲**;只有锁步帧同步才需要 `frames[]`/`keyframes[]` 与全端强确定性 |

> **顺带清一个当下的缺陷**:`table_expression.h` 在 exprtk 里注册了基于 `rand()` 的 `random` 函数,策划在伤害表达式里一用就绕过引擎的种子 RNG。它经 `table_battle_data_provider` → `SkillTableManager::GetDamage` **在回合制今天就可达**,是**现行**违反「禁 rand()」的缺陷,应立即清理,不是「MOBA 化前置项」。

### 4.2 客户端面:是「叠加」不是「换」

[battle-transport-decision.md](./battle-transport-decision.md) §5 写的是**条件项**——「battle 变成实时 MOBA → 客户端面换 KCP/UDP,**先实测 muduo TCP 边的单房 tick 成本再决定**」。目标架构文档全文零处提到 KCP/UDP,ARCH §2 硬约束还把 MOBA 归在 TCP 链路里。

按通行做法,更可能的落地形态是**在既有 muduo TCP + ProtobufCodec 客户端面之上叠加一条 KCP/UDP 通道**:操作指令走 unreliable 档,票据入场/结算/断线/心跳留在 TCP(Q3 自己写明「结算/断线/心跳可以走 TCP」),UDP 不通时整体回落。换 UDP 还附带准入控制(反射放大)重做,不是纯换 socket。

「控制面 gRPC 不动」应限定为 **match→battle 的 5 个 unary 命令边**不动;battle→结算这条承重边本来就是 Kafka。

### 4.3 长局下必须改的四处(回合制的短局退化形态)

1. **结算模型**。现在是 D4 的「每玩家最多一单在途,幂等去重退化为每人记最近一个 battle_id」+ deadline reaper。MOBA 15~40 分钟长局下,deadline 必须 ≥ 最长时长,battle 第 2 分钟崩掉则**全体玩家被冻结到 deadline**(不能排队/交易/切场景);且 reaper 与迟到的真结算竞态,reaper 先到则真结算被当「重复」丢弃、**无账本可重放**。改为判据表自己写的标准:`match_results` 按 `match_id` put_unique 先落地 + 逐人 `grant_once` + battle 心跳(15s)超时判 abandoned **提前解冻**。
2. **控制面补存活/排空边**,并把 D26「签不出票不阻断开局、全程走 gate 中继」的 fail-open 改为 fail-closed。
3. **直连收缩**:删 Kafka→gate 回落(其合理性来自「每回合一条消息」,实时下不成立),并先修 RECONNECT/REPLACE 场景下重绑断链的问题。
4. **路由表**:在共享 Redis 增设 `battle_id → 落点` 契约 key(带 TTL)。现在只有 match 私有的 `spectate:*` 观战索引 + 写在 scene 快照里的路由,不满足红线 3「路由表进共享存储」。

### 4.4 落点粒度不需要重拍

一进程 N 房是**有理由的自觉分叉**,不是与 DS 文档的冲突:

- [moba-ds-server-interview-qa.md](./moba-ds-server-interview-qa.md) 是通用面试 Q&A;其 Q8 原话是「看资源密度和隔离需求」并把「一进程多局」列为正式方案,「主流商业 MOBA 选一进程一局」是对**重型引擎 DS**(省 200MB+/进程)的描述而非禁令;
- Q9 的「严禁重置状态复用同一进程」禁的是**串行复用**(打完一局清空再接下一局),不是**并发 N 房**;
- 「PRNG 污染」在本仓不成立:每个引擎实例持有自己的 `std::mt19937_64(seed)`,房间是纯内存对象结束即销毁;
- 目标文档自己说「标准的核心是角色边界,不是进程数量」。

**待办不是重拍粒度,而是三件两份文档本就一致的事:**(1) 实测单房 tick/内存成本;(2) 补每房 try/catch + 单房超时熔断;(3) 落实「rooms 归零才 Shutdown」的世代排空(按版本/房次上限停接单 → 排空 → 退出),在进程世代粒度兑现「灰度按局、碎片有界」。仅当实测显示实时房间内存达几十 MB 以上、或崩溃形态以 SIGSEGV/OOM 为主(try/catch 挡不住),才切回一局一进程。

> **文案待更新**:目标架构 §四 仍写「未实测单房 tick 成本前不拍死」,落后于 §六 已按代码事实记的「一进程 N 房」;§六「已有(2026-08-31 原文,保留作历史对照)」段里的「一局一进程定位」已被 09-05 现状段覆盖,建议加一句指向以免被再次引用。

---

## 5. 汇总与待办

### 5.1 一张表

| 对象 | 能否 battle 式抽出 | 实际性质 | 本仓现状 |
|---|---|---|---|
| MOBA 对局 | ✅ 能 | 可丢会话 | 目标形态已定,骨架来自回合制 |
| MMO 副本/竞技场对局 | ✅ 能(建成 battle 房间) | 可丢会话(人不动、数据动) | 回合制房间已落码;实时空间型无内核 |
| MMO 主世界 scene | ❌ 范畴错误 | 持久权威;目标 = 有界 RPO + 可重启 | 每玩家默认 300s 周期存盘 |
| MMO 现有 instance 副本 | ⚠️ 不是 battle 式 | 场景态可丢、玩家态可恢复 | 生产里实际同节点同 kill 域 |
| SLG BattleService | ✅ 能,更彻底 | 无状态纯函数 worker | 仅设计稿 |
| SLG MapService | ⚠️ 一季内不可拆;赛季/战场粒度上就是 battle 形态 | 可恢复(= 判据表口径的可丢) | 仅设计稿 |
| SLG 城建/武将/联盟 | ❌ 不走这条路 | 业务服角色 | 仅设计稿,两份文档部署口径相反 |

### 5.2 由本文产生的待办

| # | 待办 | 归属 |
|---|---|---|
| 1 | 清理 `table_expression.h` 的 exprtk `random`(基于 `rand()`,绕过种子 RNG)——**回合制现行缺陷** | 回合制 |
| 2 | 补每房 try/catch + 单房超时熔断;世代排空 | battle 节点 |
| 3 | 实测单房 tick / 内存成本(落点粒度与传输选型都挂在它后面) | battle 节点 |
| 4 | 共享 Redis 增设 `battle_id → 落点` 契约 key(红线 3) | battle / match |
| 5 | 长局化时:结算改 put_unique + grant_once + 心跳 abandoned;D26 改 fail-closed | MOBA 化前置 |
| 6 | 目标架构 §四 / §六历史段的文案同步 | 文档 |
| 7 | (若做 SLG)MapService 消费 `battle_result` 的状态门 + 恢复补发前查 `battle_id` | SLG 设计稿 |

### 5.3 引用本文时必须一并说明的前提

**回合制「已抽出去」本身只过了编译 / 单测 / 静态评审**:整栈冒烟未跑(缺本机基础设施)、[moba-battle-target-architecture.md](./moba-battle-target-architecture.md) §七验收判据零打勾、gate 仍中继、battle 无 K8s manifest。上述对 MMO/SLG 的推演,是照着一个**尚未整栈验证**的样板。

---

## 6. 本文的核查方式

4 个视角(判据 / MMO / SLG / MOBA)各出 ≤4 条带行号引用的主张,每条主张配 2 个反驳者:一个**证据镜头**(逐条核对引用是否支持结论、并在全 docs/design grep 矛盾记载,不确定则默认推翻),一个**架构实践镜头**(须给出具体反例设计才算推翻)。共 36 个 agent。

16 条主张中 14 条被判「引用全对但结论夸大」——**本文采用的是反驳者给出的修正表述**,不是原始主张。被推翻的典型是:把「丢进度」说成「坏数据」、把一份文档的观点说成「文档口径」、把「可恢复」与「可丢」对立、把通用面试 Q&A 当作项目共识、把「必须移 writer」当作唯一出路。这些说法在本文里已逐一改正,**再遇到时不必重新论证**。

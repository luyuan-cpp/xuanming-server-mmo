# 回合制战斗服设计(turn-based battle server)

> 状态:一期已落地(2026-08-15 起实现,2026-08-17 全栈冒烟通);二期实现中(2026-08-31 起,
> 见 §10 观战、§11 自动战斗/5v5/队伍上限)。本文是回合制战斗(问道/梦幻式)的架构决策与
> 模块规格,是 battle 节点、battle 引擎、scene 集成、match 服务四个模块的实现依据。
> 未编译声明:二期代码为"待 Codex 生成+编译验证"状态,见 §14。
>
> **(2026-09-29 更正)客户端战斗链路已被 §22 收缩取代**:§2–§14 里凡是「客户端战斗消息经 gate 中继到 battle」
> 「`BindBattleEvent` / `UnbindBattleEvent` 会话绑定」「S2C 经 Kafka→gate 下发战斗帧」「scene RECONNECT 重发 Bind」的描述
> (如 D6、D10 / D11 的理由、§3.1、§3.2、§4.3、§10.2、§11 的 `SetAutoBattle` 路由、§14 验收项),都是收缩前的形态,原文保留作判据链。
> 现状:战斗上行与战斗帧只走客户端直连 battle(§22 D66 / D68),Bind / Unbind 契约已删、事件号墓碑(D67),
> scene 换会话只推重连提示(D72)。未编译、未测试,待 Codex 验证(§22.7)。

## 1. 背景与一期范围

现有战斗是实时 MMO 模式(20FPS `World::Update`,AOI 广播,挂钟定时器驱动技能相位)。
本设计新增一套**回合制战斗**(问道/梦幻西游式),与实时战斗并存、互不侵入:

- **一期(本轮)**:匹配遇怪(PVE solo 即配 / PVE 组队 FIFO / PVP 1v1)、**场景发起 PK
  (切磋,点名成局)**、独立 battle 节点、确定性回合引擎、scene 冻结/快照/结算闭环、结算串行化。
- **二期(明确不做,仅留接口)**:观战、ready check、跨服转播、回放持久化、宠物/召唤兽、
  robot 压测动作、客户端 UI(客户端仓另行开发,UI 一律 UGUI)。

## 2. 核心架构决策

| # | 决策 | 理由 |
|---|------|------|
| D1 | **人不动、数据动**:开战不做玩家跨节点交接,战斗服只接收"战斗快照"(属性/技能/buff/道具副本),打完回传一条结算事件 | 规避 `AllowUnsafeCrossNodeHandoff=false` 的持久化屏障缺失;保持 single-writer 不变量(cross_server_architecture_principle.md 规则 #8):场景服全程是玩家权威数据唯一 writer,战斗服只拥有可丢弃的派生副本。*(2026-09-20 标注:「持久化屏障缺失」是决策当时的事实;该屏障已由 cross-zone-scene-travel.md CZ-4 的换手门(`owner_epoch` + `player:{id}:handoff` 落盘标记)补上,代码已进 main、尚未编译 / 测试。D1 结论不因此改变:single-writer 这半条理由仍成立,开战也不值得为一次交接付出冻结 + 存盘 + 重新落点的代价)* |
| D2 | **新增独立节点类型 `BattleNodeService = 28`**(gRPC 协议,不进 `IsZoneScopedNodeType` → 全局池) | 回合制战斗是逻辑房间不是空间场景,不需要 AOI/Movement/20FPS tick;全局池让跨 zone 匹配/观战免客户端换 gate;etcd 三阶段注册/Kafka topic(`battle-{id}`)由框架自动派生 |
| D3 | **所有战斗统一由 match 服务编排开局**,两个入口:①队列匹配(系统凑单);②场景发起 PK(点名成局,challenge 应战后成局)。两入口汇入同一条 gather 管线;`battle_id`/`challenge_id` 由 match 服务用 shared/snowflake(17-bit worker 布局)生产 | 用户产品决策:匹配遇怪 + 场景可点名切磋;单一编排者让 gather/补偿逻辑只存在一份;SnowFlake 节点隔离不变量:battle_id 只能由 match 节点生产 |
| D4 | **结算串行化**:`InBattleComp` 摘除条件 = 场景服已应用结算;摘除前不得再排队/开战 | 残血带出战斗要求下一场快照必须反映上一场结果;每玩家最多一单在途,幂等去重退化为"每人记最近一个 battle_id" |
| D5 | **技能/buff 沿用现有表与管线,时间换算成回合**:复用 `SkillTable`/`BuffTable`/`CooldownTable`/`SkillPermission` 及 damage/bonus_damage 表达式、effect[]→buff 语义,把挂钟 timer 全部替换为回合计数;回合制战斗代码是**全新独立模块**,不改实时战斗代码 | 用户指示:技能参照原来那套、把 timer 弄掉弄成回合;新开回合制战斗模块 |
| D6 | **【已被 §22 D66 / D68 / D67 取代(2026-09-29):gate 两种模式都不中继战斗;战斗帧只走直连、无直连即丢弃;经 Kafka→gate 回落的只剩大厅公告 `NotifyBattleAssigned` / `NotifyBattleStart`;原文保留】** **上行走 gate→gRPC,下行走 Kafka**:客户端战斗消息经 gate 按 `OptionFileDefaultNode=NODE_BATTLE` 路由(gRPC + `x-session-detail-bin` metadata);battle 推 S2C 走 Kafka `gate-{gate_id}` 的 `PushToPlayerEvent`/`BroadcastToPlayersEvent`(既有已实现路径)。**2026-09-05 起降级为回落路径**:客户端凭票据直连 battle 节点,有直连即直发,见 §18 D23 | battle 不需要 muduo TCP server 全套;回合制每回合一条消息,Kafka 延迟完全可接受;不用管 battle↔gate 连接拓扑(全局池连全服 gate 的问题消失)。§18 之后:gate 中继只在直连未建立 / 断开时兜底 |
| D7 | **确定性引擎**:引擎输入 = 快照 + 指令流 + 随机种子,输出纯函数;战斗事件流(每回合一条 `TurnResult`)是一等公民 | 观战 = 转发事件流;断线重连 = 补发状态快照;回放 = 免费;单测可穷打 |
| D35 | **传输选型定谳(2026-09-05):按平面拆传输,不按节点选传输。** 客户端 ↔ battle = **muduo TCP 直连**(照搬 gate 客户端面,票据入场,§18);match(Go)→ battle 的控制面(CreateBattle / DestroyBattle / AddObserver / RemoveObserver / IssueBattleTicket)= **gRPC unary**;battle → 结算 = Kafka 幂等;battle → gate **不再有这条边**。三方案排序:①客户端面 muduo + 控制面 gRPC(本决策)> ②全 muduo(前提:`GameRpcMessage` 加 request_id + 生产级 Go RPC0 客户端)> ③全 gRPC(D6 原形态)。**battle 保持 C++**,gRPC ≠ Go。D6 的 gate 中继与 Kafka 回落定为过渡路径,收缩条件见 §18.7。完整论证、gRPC 在本仓的实测成本、RPC0 缺口清单见 [battle-transport-decision.md](./battle-transport-decision.md) | 战斗流量九成五在客户端边,muduo 单线程零拷贝完胜;gRPC 在本仓每节点起手 17 线程、≤8 poller 各阻塞在 promise/future、应答等 5ms CQ 轮询,不该进热路径。控制面每局一两次调用,gRPC 成本可忽略,而 RPC0 无请求关联 id、无 deadline、无 Go 客户端,补齐等于重写三成 gRPC。D6 原论据「不用管 battle↔gate 拓扑」不成立:gate 白名单已含 Battle,每 gate 对每 battle 建 channel + CQ |

## 3. 生命周期与数据流

### 3.1 正常流程

> **【下图中的 `BindBattleEvent` / `UnbindBattleEvent` 与「gate(gRPC,按绑定)」两步已被 §22 D66 / D67 取代(2026-09-29)】**:
> 现在 battle 建房后推 `NotifyBattleAssigned`(票据)+ `NotifyBattleStart`,客户端凭票直连 battle,`SubmitBattleAction` 只走直连;
> 房间销毁时关闭其全部直连。原图保留。

```
客户端 JoinQueue ──gate(gRPC)──► match 服务
  match: 咨询性检查 battle:lock:{player_id} → 入 Redis 队列(solo 即配)
  match: 凑单成功 → battle_id = snowflake
  match ──gRPC──► 各参与者所在 scene 节点 PrepareBattle(player_id, battle_id, battle_node…)
    scene: 挂 InBattleComp{battle_id, deadline} + SET battle:lock:{player_id}
    scene: 返回 BattlePlayerSnapshot(属性/技能/buff/道具副本 + session/gate/scene 路由信息)
  match ──gRPC──► battle 节点 CreateBattle(battle_id, snapshots[], battle_config, seed)
    battle: 查 Dungeon/Monster 表生成怪物侧 → 建 BattleRoom
    battle ──Kafka gate-{gid}──► BindBattleEvent(把 session 的 BattleNodeService 绑到本节点)
    battle ──Kafka gate-{gid}──► PushToPlayerEvent(BattleStartS2C)
  回合循环:
    客户端 SubmitBattleAction ──gate(gRPC,按绑定)──► battle
    battle: 全员就绪或回合超时(默认普攻)→ 引擎结算 → 广播 TurnResultS2C
    胜负已分 → BattleEndS2C
  battle ──Kafka scene-{nid}──► BattleSettlementEvent(每参与者一条,key=player_id)
  battle ──Kafka gate-{gid}──► UnbindBattleEvent → 销毁 BattleRoom
    scene: 校验 InBattleComp.battle_id 匹配 → 应用结算 → 摘 InBattleComp → DEL battle:lock
```

### 3.1b 场景发起 PK(切磋)入口

```
A 在场景点 B ──gate──► match.ChallengePlayer
  match: 咨询性检查双方 battle:lock → challenge_id = snowflake
         → Redis challenge:{id}{challenger, target, config, expires} TTL 60s
         → 推 B NotifyChallengeInvite(经 Kafka gate PushToPlayerEvent)
B 应战 ──gate──► match.RespondChallenge(accept=true)
  match: challenge 未过期校验 + 双方 battle:lock 权威性复查
         → 推双方 NotifyChallengeResult → 进入 §3.1 标准 gather(mode=PVP_CHALLENGE)
B 拒绝或 60s 超时:作废 challenge,NotifyChallengeResult(accepted=false) 推发起者
```

要点:发起时刻只做咨询性检查(不冻结任何人);冻结仍发生在 gather 的 PrepareBattle,
所以"发起挑战后立刻去排队/被别人挑战"不会产生状态泄漏 —— 先到先得,后到的 gather 拒绝。
一期不做同场景/距离校验(客户端从 AOI 拿 target_player_id 天然近距),需要时后续加在
PrepareBattle 的 scene 侧校验里。切磋结算规则(是否死亡惩罚)一期与普通战斗一致,产品细则二期。

### 3.2 失败补偿矩阵

| 故障点 | 表现 | 补偿 |
|--------|------|------|
| PrepareBattle 部分失败(某参与者掉线/已在战斗) | gather 凑不齐 | match 对已冻结者逐个调 `CancelBattlePrepare` 解冻;其余人回队首(todo #7) |
| CreateBattle 失败 | 战斗没建起来 | match 调各 scene `CancelBattlePrepare`;battle 若半建则 `DestroyBattle` |
| battle 节点崩溃 | 结算永远不来 | scene 侧 reaper:`InBattleComp.deadline_ms` 过期 → 解冻 + DEL lock + 记 metric(战斗作废) |
| 结算重复投递(Kafka at-least-once) | 二次应用 | scene 按 `InBattleComp.battle_id` 匹配才应用;摘除后再来的同 id 结算丢弃并记日志 |
| 结算到达时玩家已下线(lease 过期被 LeaveScene) | 场景无实体可应用 | scene 写 `battle:settlement:pending:{player_id}`(Redis,TTL 7 天);玩家下次登录加载完成后**先应用挂起结算再放开排队** |
| 玩家战斗中掉线 | 指令不再提交 | 【「重发 BindBattleEvent」已被 §22 D72 取代(2026-09-29):scene 换会话(RECONNECT 或 REPLACE)只推 `BattleReconnectS2C`,客户端经 `MatchService.RequestBattleTicket` 补签后重建直连再 `GetBattleState`;原文保留】战斗照打,回合超时按默认行动(普攻);重连(RECONNECT)进场时 scene 检查 InBattleComp → 向 gate 重发 BindBattleEvent + 推 BattleReconnectS2C,客户端用 GetBattleState 补拉 |
| match 服务崩溃(gather 中途) | 冻结无人收尾 | 同 battle 崩溃:scene reaper 按 deadline 解冻;队列状态在 Redis,match 重启可续 |
| 挑战应答时发起者已进入其它战斗/下线 | 成局失败 | RespondChallenge 复查 battle:lock 拒绝,推双方失败通知;challenge 记录 TTL 自然过期 |

> 所有补偿都是"超时 → 解冻/作废"型;没有任何路径需要恢复"在途的权威数据"。

## 4. proto 契约

### 4.1 节点枚举

- `proto/common/base/node.proto`:`eNodeType` 加 `BattleNodeService = 28`。
- `proto/db/proto_option.proto`:`NodeType` 加 `NODE_BATTLE = 30`(29 已被 PLAYER_LOCATOR 占用)。

### 4.2 新文件

| 文件 | 内容 | 路由 |
|------|------|------|
| `proto/battle/battle_data.proto` | `BattlePlayerSnapshot` / `BattleActorState` / `BattleAction` / `BattleEventItem` / `BattleSettlementData` 等共享数据结构 | 无(纯数据) |
| `proto/battle/player_battle.proto` | 客户端协议:`SubmitBattleAction` / `GetBattleState` + S2C(`BattleStartS2C` / `TurnResultS2C` / `BattleEndS2C` / `BattleReconnectS2C`);service 挂 `OptionIsClientProtocolService` | `OptionFileDefaultNode = NODE_BATTLE` |
| `proto/battle/battle_node.proto` | 内部 gRPC:`BattleNode` service,`CreateBattle` / `DestroyBattle`(match 调用) | `OptionFileDefaultNode = NODE_BATTLE` |
| `proto/common/event/battle_event.proto` | `BattleSettlementEvent`(经 SceneCommand DispatchEvent 进 scene)、`BattlePrepareTimeoutEvent`(scene 内部 reaper 用,可选) | 事件注册表自动收录 |
| `proto/common/component/battle_comp.proto` | `InBattleComp{battle_id, battle_node_id, deadline_ms, state}`(scene 玩家实体挂载) | 无 |

### 4.3 改动文件

- `proto/contracts/kafka/gate_event.proto`:加 `BindBattleEvent{session_id, battle_node_id, battle_id}`、
  `UnbindBattleEvent{session_id, battle_id}`(gate 侧修改 `SessionInfo` 的 BattleNodeService 绑定)。
- `proto/match/match_service.proto`:`MatchMode` 加 `MATCH_MODE_PVE_SOLO = 4` / `MATCH_MODE_PVE_TEAM = 5`;
  `JoinQueueRequest` 加 `uint32 battle_config_id = 6`(对应 DungeonTable id);service 挂
  `OptionIsClientProtocolService` + 文件挂 `OptionFileDefaultNode = NODE_MATCH`(客户端直接 JoinQueue)。
- `proto/scene/scene.proto`:`service Scene` 加 `PrepareBattle` / `CancelBattlePrepare` RPC
  (match→scene,gRPC;响应携带 `BattlePlayerSnapshot`)。
- `proto/common/component/actor_comp.proto`:`BaseAttributesComp` 加 `uint64 speed = 8`(出手序;
  同步补进 CLAUDE.md §4 类型约束清单)。

### 4.4 类型纪律(沿用宪法 §4)

battle_id / player_id / scene_id / skill_id(实例)/ buff_id 一律 `uint64`;
session_id / 各类 table_id / 回合数 `uint32`;时间戳 `uint64` 毫秒;属性 `uint64`。
引擎内部状态一律用 proto 消息承载(宪法 §3:不手写与 proto 重复的并行 struct)。

## 5. 模块规格

### 5.1 回合引擎 `cpp/libs/services/battle/`(纯逻辑,零网络/零 ECS 依赖)

**结构镜像现有技能/buff 代码**(`combat/skill`、`combat/buff` 的静态 System + proto Comp 风格),
但为独立模块,不 include 任何 `services/scene` 头文件。复用点:

- `SkillTableManager` / `BuffTableManager` / `CooldownTable` / `SkillPermission` 表与
  damage / bonus_damage / health_regeneration 表达式(`SetDamageParam({casterLevel})` 两步调用照旧);
- 校验链照搬:ValidateTarget / CheckCooldown / CheckPlayerLevel / CheckBuff(SkillPermission)/ CheckState,
  **去掉** CheckCasting / CheckRecovery / CheckChannel(相位概念随 timer 一起删除);
- 伤害公式与实时技能共用纯规则 `system/combat_damage_rules.h`(2026-09-13 起比例减伤,常驻减伤封顶 60%;
  非 PVE 对局再 × `kPvpDamageScale` = 0.3(2026-09-14 由 0.2 重标定);`critchance/100` 概率 ×2)。公式细节见 `player-attribute-allocation.md` §3.2;
  改前的"攻击减防御"口径(`base*(1+strength*0.1) - armor`,`*(1-resistance*0.01)`)已废弃;
- buff 语义照搬:max_layer 叠层 / immune_tag / dispel_tag / sub_buff / target_sub_buff / interval_effect。

**时间→回合换算**(v1 策略,后续可在 Excel 加回合列覆盖):
`rounds = max(1, ceil(duration_ms / kRoundDurationMs))`,`kRoundDurationMs = 6000` 常量。
冷却、buff 持续、周期 tick(interval)全部按此换算,战斗内只走回合计数,无任何 timer。

**回合规则 v1**:
- 回合结构:收集全员行动(带 `action_deadline`)→ 按 `speed` 降序结算(同速按 actor_id 稳定序)
  → 逐个执行(死亡单位跳过)→ 回合末 buff tick(周期效果/持续减一/到期移除)→ 胜负判定;
- 行动类型:`ATTACK`(普攻)/ `SKILL` / `DEFEND`(本回合受伤减半)/ `FLEE`(逃跑,成功率基于速度差,
  PVE 可逃,PVP 一期不可逃)/ `ITEM`(从快照道具副本扣,结算回写;效果读 `ItemTable.battle_*` 列,
  可指向同队存活单位,PVP 每人每场限 `kMaxItemUsesPerBattlePvp` 次 —— 2026-09-17 D44/D45/D47,见 §20);
- 技能只收回合制可施放的类型(剔除被动 / 开关 / 持续施法);快照带来的 buff 进引擎前先清洗
  (剔除控制类与瞬时类、`caster_id` 域改写)—— D49/D50,见 §20;
- 超时/掉线默认行动:普攻(随机存活敌方目标,用引擎 RNG 保确定性);
- 胜负:一方全灭;`max_rounds`(DungeonTable.time_limit 换算或默认 30 回合)打满 → 进攻方判负;
- RNG:`std::mt19937_64(seed)`,所有随机(暴击/默认目标/逃跑)只走引擎 RNG,禁 `tlsRandom`/`rand()`。

**引擎 API**(battle 节点按此对接):

```cpp
namespace turnbattle {
class TurnBattleEngine {
 public:
  // 快照 + 怪物侧 + 配置初始化;返回是否合法
  bool Initialize(const CreateBattleRequest& request);
  // 收行动;全员就绪返回 true
  bool SubmitAction(uint64_t actorId, const BattleAction& action);
  // 未提交者填默认行动并结算本回合
  TurnResultS2C ResolveCurrentRound();
  eBattleOutcome Outcome() const;         // ONGOING / SIDE_A_WIN / SIDE_B_WIN / DRAW(proto 生成枚举名)
  BattleSettlementData BuildSettlement(uint64_t playerId) const;
  BattleStateS2C BuildStateSnapshot() const;  // 重连补拉
};
}
```

单元测试(独立测试工程,照 `cpp/tests/` 现有工程组织):确定性(同种子同指令→同事件流)、
速度序、超时默认、buff 回合语义、胜负边界、结算数值。

### 5.2 battle 节点 `cpp/nodes/battle/`

- `_template/main.with_context.cpp.example` 起步;`RunSimpleNodeMainWithOwnedContext`;
  gRPC 注册 `BattleNodeGrpcImpl`(CreateBattle/DestroyBattle);无 World::Update 帧循环,
  回合超时用 muduo timer(每 room 一个 deadline);
- `BattleRoomManager`(手写类):battle_id → BattleRoom{engine, participants 路由信息, timer};
- 客户端消息(SubmitBattleAction/GetBattleState)经 gate gRPC 进来,session metadata 解出 player_id;
- 出站全走 Kafka producer:S2C 用 `GateCommand{PushToPlayerEvent/BroadcastToPlayersEvent}`
  (`target_instance_id` 填 gate 实例 UUID,防僵尸不变量);结算用
  `SceneCommand{DispatchEvent, event_id=BattleSettlementEvent}`(key=player_id);
  绑定用 `BindBattleEvent`/`UnbindBattleEvent`;
- 路由信息(gate_node_id / gate_instance_id / scene_node_id / scene_instance_id / session_id)
  全部由 `BattlePlayerSnapshot` 携带,battle 节点不查 etcd 定位对端。

### 5.3 scene 集成 `cpp/libs/services/scene/battle/` + handler 守护段

`PlayerBattleSystem`(手写,静态方法风格):
- `PrepareBattle`:校验(在线/未在战斗/未冻结)→ 挂 `InBattleComp` → SET `battle:lock:{player_id}`
  (EX = 战斗最大时长+60s)→ 组 `BattlePlayerSnapshot`(属性含 speed、技能列表、参战 buff、
  可战斗道具副本、session/gate/scene 路由信息)→ 返回;
- `CancelBattlePrepare` / reaper 过期:摘 comp + DEL lock;
- `ApplySettlement`:battle_id 匹配校验 → 应用 HP/MP/经验/金钱/道具 delta(道具扣除按实际持有
  校验,不足按 0 处理并记日志,防刷)→ 摘 comp + DEL lock → 推客户端通知;玩家不在 → 写
  pending(§3.2);
- 登录链路挂钩:加载完成后应用 pending 结算;RECONNECT 且有 InBattleComp → 重发 BindBattleEvent;
- reaper:低频扫描(挂 timer 或并入既有慢频系统),`deadline_ms` 过期即作废解冻;
- 冻结清单(InBattleComp 存在时拒绝):再次排队/开战、交易、使用改属性道具、切场景、跨 zone 迁移、
  **实时技能(施法者与目标两侧)、实时 buff 挂载、移动上报与位移积分、背包整理、GM 回滚类写操作**(2026-09-17 D48);
  放行:聊天、邮件收取(不动战斗属性部分)、好友、任务领奖(只增不减,方向安全)。
  **闸只放客户端入口层与实时战斗落点,不得下沉到 `BagService` / `CurrencySystem`** ——
  结算入账时 `InBattleComp` 还挂着,下沉会把结算自己挡住。

### 5.4 match 服务 `go/match/`

go-zero,结构照抄 `go/scene_manager`(config/etc yaml/internal/{logic,svc,server}/noderegistry):
- 队列:Redis(`match:queue:{mode}:{battle_config_id}` list + ticket hash),JoinQueue 前
  咨询性查 `battle:lock:{player_id}`;solo 即配,组队/1v1 FIFO 凑单;
- matcher loop:凑单 → battle_id(shared/snowflake,17-bit 布局,match 节点专属生产)→
  gather(§3.1)→ 失败补偿(§3.2);玩家定位读 `player:{id}:location`(scene_manager 维护,
  本文档即该 key 的共享契约记录);
- battle 节点发现:etcd list-watch `BattleNodeService.rpc/` 前缀(照 LoadReporter 模式),
  v1 随机选,负载上报二期;
- 无状态、可水平扩,多实例用 Redis 锁保护 matcher loop;**全服跨 zone 匹配与 Redis Cluster
  形态见 §16 / `cross-zone-matchmaking.md`**(2026-09-02:队列 key 改为 `{mq}` hash tag 同 slot +
  注册集取代 SCAN,match 私有 key 与跨运行时契约 key 分成 MatchRedis / SharedRedis 双存储);
- **挑战模块(场景发起 PK)**:ChallengePlayer/RespondChallenge(§3.1b),challenge 记录
  `challenge:{id}` Redis TTL 60s + `challenge:target:{player_id}` 反查(同一目标同时只挂一个
  待应答挑战,后来者拒绝);S2C 弹窗/结果经 Kafka gate PushToPlayerEvent 推送(目标玩家的
  session/gate 定位照 friend/chat 等 Go 服务的既有推送模式);应战成功复用 gather,
  mode=MATCH_MODE_PVP_CHALLENGE。

### 5.5 框架注册点(散点清单)

- `node_util.cpp`:`nodeTypeNameMap` 加 BattleNodeService;`IsTcpNodeType` **不加**(gRPC 节点);
  `IsZoneScopedNodeType` **不加**(全局池);
- `bin/etc/base_deploy_config.yaml`:`service_discovery_prefixes` 加 `BattleNodeService.rpc`
  (gate 需发现 battle 节点建 gRPC channel;match 的 Go 侧自行 watch);
- gate 事件 handler(生成守护段):BindBattleEvent/UnbindBattleEvent → 改 `SessionInfo`
  的 BattleNodeService 绑定(照 RoutePlayerEvent 模式,`SetEntityId`);
- `dev.bat` / 部署脚本:加 battle 节点条目;
- `CLAUDE.md` §4:BaseAttributesComp uint64 清单补 `speed`。

## 6. Redis key 契约(新增)

| key | 写者 | 读者 | 语义 |
|-----|------|------|------|
| `battle:lock:{player_id}` = battle_id | scene(Prepare 时 SET EX / 结算与作废时 DEL) | match(JoinQueue 咨询性检查) | 战斗串行化锁,权威判定仍在 scene 的 InBattleComp |
| `battle:settlement:pending:{player_id}` = 序列化 BattleSettlementEvent,TTL 7d | scene(玩家离线时) | scene(登录加载后) | 离线结算暂存 |
| `match:{mq}:index` / `match:{mq}:queue:*` / `match:{mq}:lock:*` | match | match | 队列注册集 / 队列 / 凑单锁,hash tag `{mq}` 同 slot(Lua 原子入队;§16)|
| `match:ticket:{player_id}` | match | match | 排队票据(含 zone_id / queue_key;matched 态短 TTL 自愈,§16)|

**存储归属(2026-09-02,§16)**:上表 `battle:*` 与 `player:*` 是跨运行时契约 key,留在共享 Redis
(SharedRedis,match 只读);`match:*` / `challenge:*` / `spectate:*` 是 match 私有 key,走 MatchRedis
(可配 Redis Cluster)。`MatchRedis` 未配置时两者为同一实例。

## 7. 新增不变量(并入宪法 §7 语义)

1. `battle_id` 只能由 match 节点生产(shared/snowflake 17-bit 布局,可用 `ParseGuid` 解);
2. 结算事件按 `player_id` 做 Kafka key;应用以 `InBattleComp.battle_id` 匹配为准,不匹配丢弃并告警;
3. `InBattleComp` 摘除 = 结算已应用或作废,是玩家再次进战斗的唯一放行条件;
4. 战斗服对玩家权威数据零写权:结算只能以事件形式交由 scene 应用,禁止 battle 直写 DB/Redis 玩家数据;
5. 引擎内所有随机只走种子 RNG(确定性,观战/回放依赖)。

二期新增(2026-08-31):

6. 观众对战斗状态零影响:AddObserver/RemoveObserver/StopWatch 不触碰引擎;观众不能是本场参战者;
   观众永远收不到结算事件(BattleRouting 的 scene 字段留 0 即是这条不变量的实现);
7. `spectate:*` Redis key 只有 match 读写(battle 节点保持零持久化);
8. 观战与排队/战斗互斥:进 gather 前 match 必须清退该玩家的观战绑定(D11);
9. 移动链路与战斗触发零耦合:任何"遇怪"入口只能经 match(D16,禁止随机遇怪)。

## 8. 部署形态

battle 为独立进程池(全局,不分 zone),K8s 单独 Deployment(照 scene-world/scene-instance
第三池模式);战斗房间为纯内存对象,节点无持久状态,崩溃即战斗作废(§3.2 补偿);
水平扩容 = 加实例,match 按发现列表分配。Agones/rooms Counter 一期不接。

match 同为**全局池**(MatchNodeService 不在 `IsZoneScopedNodeType`,gate 连全 zone 的 match 实例随机路由):
K8s 部署到 infra namespace 一次、replicas ≥ 2;排队/票据/挑战/观战索引放 Redis Cluster(§16)。

## 9. 二期清单

**本轮实现(2026-08-31)**:观战 + 观战匹配(§10)、自动战斗 + 5v5 + 队伍上限 5(§11)。
**仍预留**:ready check、转播 relay、回放持久化(事件流落盘)、宠物参战(快照加 pet 段)、
robot battle 动作、匹配负载均衡、Excel 回合列(替代 kRoundDurationMs 换算)、
观战事件流延迟推送(反侦察,见 §10.6)、预组队入队(party_member_ids)。

## 10. 观战系统(二期,2026-08-31)

### 10.1 决策

| # | 决策 | 理由 |
|---|------|------|
| D8 | **观战 = 转发确定性事件流**(D7 的直接兑现):观众首帧收 `BuildStateSnapshot()` 全量状态,之后逐回合收与参战者相同的 `TurnResultS2C`(不同消息号) | 引擎零改动;观众对战斗状态零影响(零写权,同宪法不变量 4 的精神) |
| D9 | **观战匹配由 match 编排**(D3 的延伸):match 在 gather 开局成功后把战斗登记进 Redis 活跃索引;`WatchBattle(battle_id=0)` 随机挑一场,`ListWatchableBattles` 出列表。battle 节点不碰 Redis,索引清理靠 TTL + 懒剔除 | 只有 match 知道"哪些战斗存在、在哪个 battle 节点";battle 节点保持纯内存、零持久化(§8) |
| D10 | **【已被 §22 D66 / D67 / D69 取代(2026-09-29):观众不再经 gate 绑定;AddObserver 后推观众 `NotifyBattleAssigned`,观众凭票直连,首帧随直连握手下发,`StopWatchBattle` 只走直连;原文保留】** **观众复用参战者的会话绑定机制**:AddObserver 成功后 battle 节点发既有 `BindBattleEvent` 把观众 session 绑到本节点,退出观战的上行(`StopWatchBattle`)按绑定路由 | 不新增 gate 机制;Kafka 同 key(player_id)保证 gate 先处理绑定再下发首帧 |
| D11 | **观战与排队/战斗互斥**:`WatchBattle` 拒绝持有 match ticket 或 `battle:lock` 的玩家;已在观战的玩家由 `WatchBattle` 先清退旧场(RemoveObserver+DEL 标记)再接入新场;任何玩家进入 gather(排队凑单/切磋成局)时 match 先把他从观战中清退(RemoveObserver,尽力而为) | 观众绑定和参战绑定共用 SessionInfo 的 BattleNodeService 槽位,互斥杜绝绑定被覆盖的竞态 |

### 10.2 数据流

```
客户端 WatchBattle(battle_id|0) ──gate(gRPC,无状态路由)──► match
  match: 互斥检查(ticket / battle:lock;重复 WatchBattle 不拒绝,懒清退旧场后放行,战斗已收尾则仅删标记)
       → battle_id=0 时从 spectate:battles:active 随机挑一场
       → 读 spectate:battle:{id} 得 SpectateBattleRecord(含 battle_node_id)
       → 读 player:session:{player_id} 组观众 BattleRouting(session/gate/zone,scene 字段留 0)
       → SETNX+EX spectate:watching:{player_id} = battle_id(TTL 同索引;并发 WatchBattle 抢占失败即拒绝)
       → gRPC battle.AddObserver(battle_id, observer, routing)
  battle: 房间校验(存在/观众未满 kMaxObserversPerRoom/非参战者)→ 加入 room.observers
       → Kafka gate-{gid}: BindBattleEvent(观众 session → 本节点)
       → Kafka gate-{gid}: PushToPlayerEvent(NotifySpectateState 首帧,含 observer_count)
  回合循环:每次 ResolveRound 广播 TurnResultS2C 给参战者(NotifyTurnResult)
       同时广播给观众(NotifySpectateTurnResult,同 payload 不同消息号)
  战斗结束/作废:观众收 NotifySpectateEnd(outcome + reason)→ UnbindBattleEvent
  观众主动退出:StopWatchBattle ──gate(按绑定)──► battle:移出 observers + Unbind
  AddObserver 报房间不存在:match 懒剔除索引(DEL 记录 + ZREM)后对随机模式换一场重试一次
```

### 10.3 proto 契约(已落盘)

- `proto/battle/player_battle.proto`:`SpectateStateS2C` / `SpectateEndS2C`(含 `eSpectateEndReason`)、
  `StopWatchBattle(Request/Response)`;service 新增 `StopWatchBattle` /
  `NotifySpectateState` / `NotifySpectateTurnResult` / `NotifySpectateEnd`;
- `proto/battle/battle_node.proto`:`AddObserver` / `RemoveObserver`(match→battle 内部 gRPC);
- `proto/match/match_service.proto`:`WatchBattle` / `ListWatchableBattles` +
  `BattleWatchSummary`(客户端摘要)+ `SpectateBattleRecord`(Redis 内部记录,含 battle_node_id)。

### 10.4 Redis key 契约(新增,全部 match 读写)

| key | 类型 | 语义 |
|-----|------|------|
| `spectate:battle:{battle_id}` | string = SpectateBattleRecord pb,TTL = BattleMaxDurationSeconds + 60s | 活跃战斗登记(gather 成功后写) |
| `spectate:battles:active` | ZSET member=battle_id score=created_at_ms | 随机/列表索引;懒剔除 + 定期按 score 清过期 |
| `spectate:watching:{player_id}` | string = battle_id,TTL 同上 | 观战互斥标记(gather 清退时反查用);重复 WatchBattle 懒清退旧场(战斗已收尾则仅删标记) |

### 10.5 房间侧规格(battle 节点)

- `BattleRoom` 加 `std::map<uint64_t, ::BattleRouting> routingByObserver`(有序,遍历稳定)
  + 常量 `kMaxObserversPerRoom = 20`;
- 观众推送与参战者共用既有 Kafka 出站助手(key=player_id,target_instance_id 防僵尸不变量照守);
- `HandleGetBattleState` 放行观众(参战者或观众都可补拉,重进观战画面用);
- `AbortAllRooms` / deadline 强制收尾 / `FinishBattle` 都要给观众发 `SpectateEndS2C` + 解绑;
- `HandleDestroyBattle`(match 回滚路径)同样清观众;
- 观众数上限满 / 观众是参战者 / 房间不存在 → AddObserver 回对应 tip 错误。

### 10.6 一期观战明确不做

事件流延迟推送(防"开小号观战偷看对手指令"):v1 观众与参战者同步收流。回合制信息量
有限且当前无排位利益,延迟缓冲(N 回合环形缓冲 + 首帧快照回退 N 回合)留到有竞技需求时做。

## 11. 自动战斗 / 5v5 / 队伍上限(二期,2026-08-31)

### 11.1 决策

| # | 决策 | 理由 |
|---|------|------|
| D12 | **自动战斗是服务端状态**:`SetAutoBattle` 落在引擎 `BattleActorState.is_auto`;auto 单位在 `AllPlayersReady()` 中视为已就绪,`ResolveCurrentRound()` 的默认行动路径(普攻随机存活敌人)替他出手 | 挂机玩家掉线/切后台照打;引擎确定性不受影响(默认行动本来就走引擎 RNG);状态进快照,重连/观战免费可见 |
| D13 | **全自动房间按固定节奏推进**:装填回合时若 `AllPlayersReady()` 立即为真(全员挂机),回合 timer 改用 `kAutoRoundIntervalMs = 2000` 而非整个行动窗口 | 既不空转刷回合(观众/客户端跟得上),也不傻等 30s 行动窗口 |
| D14 | **队伍上限 5 双侧强制**:引擎 `Initialize` 校验每队玩家 ≤ `kMaxBattleTeamSize = 5`(超限拒绝建房);match 侧 PVE_TEAM 凑满人数按 `min(配置值, 5)` 收口 | 产品口径"队伍上限五个人";Dungeon 表存在 max_team_size=10 的历史行,代码收口比改表重导安全 |
| D15 | **5v5 PVP 启用既有枚举**:`MATCH_MODE_5V5` 走 FIFO 凑 10 人,弹出序前 5 人 team 0、后 5 人 team 1 | 复用 matcher/gather 全部管线,只改 requiredPlayers/teamIndexFor 两个开关点 |
| D16 | **跑图不遇怪是既定架构,明文化**:场景内没有怪物实体、没有任何移动/碰撞触发战斗的逻辑;PVE 进战斗只有两条路——JoinQueue 匹配(solo 即配/组队凑单)与场景点名切磋。移动链路(MoveStart/MoveSync/导航裁决)与战斗系统零耦合,今后加"场景可见怪物"也必须走"点击怪物 → match 入口",禁止随机遇怪 | 用户产品决策(2026-08-31):跑路不遇怪 |

### 11.2 自动战斗数据流

```
客户端「自动」开关 → SetAutoBattle{battle_id, enabled} ──gate(按绑定)──► battle
  battle: 校验参战者/未死/未逃 → engine.SetActorAuto(player_id, enabled)
        → enabled 且 AllPlayersReady() → 立即 ResolveRound(同 SubmitAction 就绪路径)
  引擎: is_auto 单位不进 pending_actor_ids;ResolveCurrentRound 对未提交者
        (含 auto)FillDefaultActions 普攻
  客户端: BattleStateS2C.actors[].is_auto 渲染开关状态;本地记忆开关,
        下一场 BattleStart 后自动重发 SetAutoBattle(连续挂机体验)
「连续战斗」为纯客户端开关:BattleEnd 结算面板收起后自动按上一次的
  mode/battle_config_id 重新 JoinQueue。
```

### 11.3 触点清单

- 引擎:`SetActorAuto` / `AllPlayersReady` 跳过 auto / `BuildStateSnapshot.pending_actor_ids`
  排除 auto / `Initialize` 队伍人数校验;常量 `kMaxBattleTeamSize=5`、`kAutoRoundIntervalMs=2000`
  进 `constants/turn_battle_constants.h`;
- battle 节点:`HandleSetAutoBattle`(手写 BattleClientPlayerGrpcImpl 新方法)+
  `ArmRoundTimer` 全自动检测(D13);
- match:`requiredPlayers`/`JoinQueue` 开放 5v5(=10 人)、PVE_TEAM 人数 `min(cfg,5)`、
  `teamIndexFor` 按 `memberIndex < required/2` 分队(1v1/切磋语义不变);
- 客户端:战斗屏「自动」按钮、排队面板「连续战斗」开关、5v5 与 PVE 组队入口。



## 12. Codex 验证清单(总执行顺序,2026-08-15 首轮实现后固化)

> 各模块的细化步骤见 PROGRESS.md 对应条目;本节是跨模块的**总顺序**,乱序必失败。
> 全部 C++ MSBuild 必须 `/m:1` 串行(并发报假 C1041/LNK1104)。

**阶段 0:proto 重生成(一切之先)** — `cd go && build.bat`(必要时再走 `dev_tools.ps1 -Command proto-gen-run`)。验收:
- `cpp/generated/proto/battle/{battle_data,battle_node,player_battle}.pb.*` 及 `{battle_node,player_battle}.grpc.pb.*`(两 proto 均已去掉 `cc_generic_services`);
- `node.pb.h` 含 `BattleNodeService=28`;`gate_event.pb.h` 含 `BindBattleEvent/UnbindBattleEvent`;`battle_comp.pb.h`(InBattleComp);`battle_event.pb.*`;
- `scene.pb.h` 含 `PrepareBattle*/CancelBattlePrepare*`;`scene_node_service.grpc.pb.h` 的 `SceneNodeGrpc` 新增同名两 rpc(消息共用 scene.proto 一份);
- `rpc/service_metadata/player_battle_service_metadata.h` 的 6 个 `BattleClientPlayer*MessageId`;`contracts_kafka_gate_event_event_id.h` 的 `ContractsKafka(Un)BindBattleEventEventId`;`common_event_battle_event_event_id.h` 的 `BattleSettlementEventEventId`;
- `message_id.txt` 出现 `BattleClientPlayer*` / `MatchService*` 13 个键;
- 生成骨架:`cpp/nodes/battle/handler/grpc/battle_node.{h,cpp}`、`cpp/nodes/scene/handler/event/battle_event_handler.{h,cpp}`、`scene_handler` 与 `scene_node_service` 新方法。
- 任何常量名/路径与代码引用不符 → **只改引用侧**(`battle_room_manager.cpp` / `challengelogic.go` / `gate_command_builder.go` / 客户端 `BattleClient.cs`),不改生成器语义。

**阶段 0.5:守护段填充(见 §13)+ 已知陷阱**:`scene_node_service.cpp` 的 CreateScene Agones
前置检查(AcquireCreatePermitBlocking 块)在守护段之外,重生成**必丢**,必须 `git diff` 比对恢复。

**阶段 1:C++ 工程接入与编译(串行)**:①`generated/proto` 新 .cc 入 `proto.vcxproj`;
②`battle.vcxproj`(libs)与 `turn_battle_engine_test.vcxproj`、`cpp/nodes/battle/battle.vcxproj` 入 sln
(GUID 见 PROGRESS.md);③编译顺序 proto → services/battle → services/scene 链 → gate → nodes/battle →
测试;④跑 `turn_battle_engine_test.exe`,17 用例全绿。

**阶段 2:Go**:`go/proto` 视需 `go mod tidy`;`go/match` `go mod tidy && go build ./...`(首次生成 go.sum);可选 `go vet`。

**阶段 3:客户端**(E:/work/mmorpg-client):`gen_proto.ps1 -ProtoRoot E:/work/xuanming-server-mmo` →
`gen_messageids.ps1`(无 missing 警告、13 常量)→ Unity 6000.0.32f1 编译零 CS 错 →
EditMode `MmorpgClient.Tests.EditMode.Battle` 20 用例绿。

**阶段 4:本地冒烟**:`dev.bat start-cpp`(含 1 battle)+ `go_services.ps1`(match 已入 catalogue,端口 50500/metrics 9170)→
etcdctl 验 `BattleNodeService.rpc` 记录 `protocolType=PROTOCOL_GRPC` → 登录进场景 → 排队 PVE 单人 →
回合战斗 → 结算回场景;切磋:A 输入 B 的 player_id 发起 → B 弹窗应战 → PVP 开局。

## 13. 生成后守护段施工清单(一次性,贴完可删本节)

重生成出骨架后,把下列代码贴进对应文件的 `///<<< BEGIN/END WRITING YOUR CODE` 守护段。
全部为一行委托,业务在手写类中。

**A. `cpp/nodes/battle/handler/grpc/battle_node.cpp`**(骨架若未生成,照 `scene_node_service.h/.cpp` 形态手建,类 `BattleNodeImpl : public BattleNode::Service`,runInLoop+promise 投递):
- 文件头守护段:`#include "logic/battle_room_manager.h"` 与 `#include "muduo/base/Logging.h"`
- `HandleCreateBattle` 守护段:`BattleRoomManager::Instance().HandleCreateBattle(*request, *response);`
- `HandleDestroyBattle` 守护段(响应 Empty 无形参):`BattleRoomManager::Instance().HandleDestroyBattle(*request);`

**B. `cpp/nodes/scene/handler/grpc/scene_node_service.cpp`**(gRPC 面,match 走这条):
- 文件头守护段加:`#include "battle/system/player_battle.h"`
- `HandlePrepareBattle` 守护段:`PlayerBattleSystem::PrepareBattle(*request, *response);`
- `HandleCancelBattlePrepare` 守护段:`PlayerBattleSystem::CancelBattlePrepare(*request);`
- ⚠️ 同文件守护段外的 Agones 前置检查重生成会丢,先 git diff 恢复再继续。

**C. `cpp/nodes/scene/handler/rpc/scene_handler.cpp`**(muduo 面,C++ 节点间备用,同款两行委托,
方法签名以生成器输出为准):`PrepareBattle`/`CancelBattlePrepare` 守护段同 B 的两行。

**D. `cpp/nodes/scene/handler/event/battle_event_handler.cpp`**:
- 文件头守护段:`#include "battle/system/player_battle.h"`
- `BattleSettlementEventHandler` 守护段:`PlayerBattleSystem::ApplySettlement(event);`
- 若同目录 `event_handler.cpp` 聚合器未自动补 `BattleEventHandler::Register/UnRegister`,手工补两行。

**E. `cpp/nodes/gate/handler/event/gate_event_handler.cpp`**(顶部守护段已含
`#include "battle_binding_helper.h"`,重生成保留):
- `BindBattleEventHandler` 守护段:`gate_battle_binding::HandleBindBattle(event);`
- `UnbindBattleEventHandler` 守护段:`gate_battle_binding::HandleUnbindBattle(event);`

## 14. 二期 Codex 验证清单(观战 + 自动战斗 + 5v5,2026-08-31)

> 总顺序与 §12 相同:proto 重生成 → C++ 串行编译 → go/match → 客户端 → 冒烟。
> C++ MSBuild 一律 `/m:1`。

**阶段 0:proto 重生成** — `cd go && build.bat`(必要时 `dev_tools.ps1 -Command proto-gen-run`)。验收:
- `battle_data.pb.h`:`BattleActorState` 含 `is_auto`;
- `player_battle.pb.h`:`SpectateStateS2C`/`SpectateEndS2C`/`eSpectateEndReason`/
  `StopWatchBattle*`/`SetAutoBattle*`;`player_battle_service_metadata.h` 新增 5 个
  `BattleClientPlayer*MessageId`(StopWatchBattle/SetAutoBattle/NotifySpectateState/
  NotifySpectateTurnResult/NotifySpectateEnd);
- `battle_node.pb.h` + `battle_node.grpc.pb.h`:`AddObserver`/`RemoveObserver`;
- `match_service.pb.go` 等 Go 产物:`WatchBattle`/`ListWatchableBattles`/
  `BattleWatchSummary`/`SpectateBattleRecord`;`message_id.txt` 新增
  `MatchServiceWatchBattle`/`MatchServiceListWatchableBattles` 与 5 个 BattleClientPlayer 键;
- ⚠️ 老陷阱:`scene_node_service.cpp` 守护段外的 Agones 块重生成必丢,git diff 恢复。
- 任何常量名与代码引用不符 → 只改引用侧(battle_room_manager.cpp / spectate.go /
  客户端 BattleClient.cs、SpectateClient.cs),不改生成器。

**阶段 1:C++(串行)**:proto.vcxproj(新 .cc)→ `cpp/libs/services/battle`(引擎)→
`cpp/nodes/battle` → `turn_battle_engine_test.exe`(新增 auto/人数上限用例全绿,含既有 17 例)。
scene/gate 无代码改动,受 proto 重生成影响需重编。

**阶段 2:Go**:`cd go/match && go mod tidy && go build ./... && go vet ./...`。

**阶段 3:客户端**(E:/work/mmorpg-client):`gen_proto.ps1 -ProtoRoot E:/work/xuanming-server-mmo`
→ `gen_messageids.ps1`(白名单新增 7 键无 missing 警告)→ 编译零 CS 错 →
EditMode Battle 测试全绿(含新增 SpectateClient / 自动战斗用例)。

**阶段 4:冒烟**:两客户端(dev_demo + 第二个 dev 账号):A 排 PVE solo 开战 →
B 主界面「观战」→ 随机观战 → 收到首帧与逐回合事件 → A 开「自动」挂机到结算 →
B 收到观战结束推送;B 观战中点排队 → 观战被清退(NotifySpectateEnd reason=REMOVED)。

## 15. PVE 数据化(2026-09-02):怪物属性 / 副本怪物组 / 奖励 / 玩家初始属性

引擎早已支持带属性怪物/多怪/多回合(单测反证),此前 PVE"一回合秒杀"纯因**表空**
(MonsterTable 只有 id 列)+ **玩家新号 0 属性**(建角只写账号级 class_id,不建属性数据)。
本轮补数据 + 三处接线,PVE 变为大厂标准多回合对战。

### 15.1 策划表加列(走 tools/data_table_exporter 重导)
- `Monster.xlsx`:health/strength/armor/resistance/critchance/speed(uint64)+ exp_reward/gold_reward;16 只怪分级。
  **2026-09-13 重定**(取代从未导表的 2026-09-11 那版):按加点百分比公式 + 比例减伤重算 1-16 的 health / strength / speed
  (护甲 / 抗性 / 暴击 / 奖励不变);新手教学怪 / 普通怪 / 首领三档目标、参照等级与新值见 `player-attribute-allocation.md` §8。
  **2026-09-14 再重定**:删相性 / 仙魔后参照号物伤下降、速度单位 ×12(逃跑系数同除 12),1-16 按同样三档目标重算,
  新值同见 §8;09-13 那版同样从未导表。
  同日防御单位 ×12:`armor` 列 3~35 → 36~420,与等级系数 360 + 120 × 等级(`combat_damage_rules.h`)配套,减伤比例不变。
- `Dungeon.xlsx`:`monster`(repeated fk:Monster)怪物组;副本1=[1,2]/副本2=[6,7]/副本3=[11,12,16]。
- `Class.xlsx`:init_health/mana/strength/armor/resistance/critchance/speed(职业初始属性)。
- **导表 PATH 必须同时含 protoc 与 protoc-gen-go/grpc**,否则 Go 侧 proto 静默不重生成。

### 15.2 引擎接线(cpp/libs/services/battle)
- `AppendMonsterActor` 优先读 `FindMonster(id)` 表属性,缺行(兜底怪 id=0)回退常量。
- `TableBattleDataProvider::GetDungeonMonsterIds` 读 `DungeonTable.monster`。
- `BuildSettlement` 玩家侧(team0)胜时累加击杀怪物 exp_reward/gold_reward → exp_gain/gold_gain;败/逃/亡不发奖。

### 15.3 玩家初始属性 + 复活(cpp/libs/services/scene/player/player_database_loader.cpp)
- `ApplyClassInitialAttributesOrRevive`:加载时 BaseAttributesComp 全 0(新号)→ 按 ClassTable 首行 init_* 赋全属性;
  已初始化但 health=0(阵亡)→ 恢复满血满蓝(基础复活,防永久卡死)。残血带出战斗(D4)在同会话 battle→battle 保持,不受影响(此函数只在 DB 加载/登录跑)。

### 15.4 已知缺口
- ~~经验/道具落地~~:**道具与掉落已于 2026-09-17 落地**(引擎摇掉落 → scene 真扣真发,见 §20 与
  [turn-battle-gap-closure.md](./turn-battle-gap-closure.md));**经验仍未入账** —— 全仓没有经验值组件与升级
  结算系统(只有 `LevelComp` 与 `PlayerUpgradeEvent` 事件壳),结算里仍只记日志。
- 怪物 AI 用技能未做(只普攻);技能 damage 表达式对低级 PVE 偏大,配怪技能前要重平衡。
- class_id 未随 PlayerAllData 下发 scene,玩家初始属性/技能暂全职业统一(取 Class 首行)。
- 完整死亡/复活流程(复活点/惩罚/道具)待产品细化。**基线(2026-09-02)**:战斗结算把玩家打到 0 血
  (含离线挂起结算登录后补应用)即按 ClassTable 首行满血满蓝复活(`ReviveBaseAttributesIfDead`,与登录
  加载复活同一规则);`PrepareBattle` 拒绝 0 血玩家。否则阵亡玩家可再次排队、被快照进新局、引擎开局即判负
  (跨 zone 冒烟复跑时实测)。产品定稿死亡流程时替换此基线。

## 16. 全服跨 zone 匹配 + 水平扩展 + Redis Cluster(2026-09-02)

完整设计见 `docs/design/cross-zone-matchmaking.md`。要点:

- **链路本来就通**:快照模型玩家不搬家(绕开 `cross-zone-readiness-audit.md` 的实体迁移致命项);
  `(node_type, node_id)` 由 etcd CAS **全局**分配(Go `noderegistry` allocationKey 不带 zone、C++
  `etcd_service.cpp` "lost the race globally"),`gate-{id}` / `scene-{id}` topic 跨 zone 不撞;
  `player:{id}:location` 带 zone_id,match watcher 覆盖全 zone 的 scene 前缀;battle / match 都是全局池。
- **真正补的洞**:①Redis Cluster 就绪 —— `SCAN match:queue:*` 在集群只扫一个分片、挑战记录双 key
  `DEL` 跨 slot、C++ hiredis 无集群;②实例崩溃后 `matched` 票据 6h 不能重排;③多实例 metrics 端口撞。
- **决策**:全局池不分 zone、无优先级、无降级开关;MatchRedis(私有 key,可集群)/ SharedRedis(契约 key,
  只读)双存储;队列三类 key 同 `{mq}` slot + Lua 原子入队 + 注册集;`matched` 短 TTL(默认 30s);
  票据记 zone_id / queue_key;`gather_zone_mix_total{mode,mix}` 指标;客户端零改动。
- **补充(实施中发现)**:gate 两处随机路由硬过滤同 zone → `NodeUtils::IsGlobalPoolNodeType` 豁免
  Match/Battle(D11);`shared/snowflakealloc` 同主机亲和复用会抢活进程的 worker id(本地起第二个 login
  时 zone1 login 自杀)→ 只在 lease 已死或有 `released` 标记时接管,否则派生键分配(D5b)。
- **验证**:go/match 单测(miniredis);本地 `redis-cluster` compose profile + `dev-start-zones 1,2` +
  robot `battle-smoke` `cross_zone: true` 输出 `CROSS_ZONE_MATCH_OK`。

## 17. 战斗表现(问道式演出)与二期扩展索引(2026-09-03)

- 表现规格与美术契约:`turn-battle-presentation.md`(录像观察、协议增量 D1-D5、客户端表现层架构、验证)、
  `battle-art-prompts.md`(生图提示词包、资源路径契约、程序化生成清单)。
- 二期服务端:prepare deadline + `BattleConfirmedEvent`、配表指纹(`BattlePlayerSnapshot.table_fingerprint` /
  `CreateBattleRequest.table_fingerprint`)、`contracts.kafka.BattleResultEvent`(battle → match,评分回流)—— 见
  `cross-zone-matchmaking.md` §10/§11。

## 18. 客户端直连 battle 节点:票据入场,战斗流量零字节经 gate(2026-09-05,已落码待验证)

> 状态:**已落码;2026-09-05 静态评审 22 条已修;proto-gen / C++ Debug 全量 0 error / Go / robot / 两份 gtest 全绿;整栈冒烟(§18.8 第 5-6 步)已于 2026-09-05(3) 通过——旧模式与路由模式两轮 `BATTLE_SMOKE_OK`,`a_direct_turns == 总回合数`,战斗帧零回落 gate(PROGRESS.md 同日条目)。Unity 客户端 2026-09-06 提交 `b5cf6ef` 接入(`BattleDirectLink` + `DirectRoutingBattleTransport`),Unity 实机对真 battle 的端到端直连尚未跑过。收缩阶段决策见 §19(2026-09-16);收缩已于 2026-09-29 一次做完,见 §22(D65–D75,未编译待验证)。** 目标形态与判据见
> [moba-battle-target-architecture.md](./moba-battle-target-architecture.md)(会话制对局标准形态:
> 大厅一条连接走 gate,战斗另一条连接直连 battle,票据入场,battle 可随时 kill)。
> 本节只写"改了什么、契约是什么、怎么验",不重复目标文档的论证。

### 18.1 决策

| # | 决策 | 理由 |
|---|------|------|
| D23 | **【已被 §22 D66 / D68 取代(2026-09-29):gate 两种模式都不中继战斗,战斗帧无直连即丢弃、不再回落 gate;原文保留作判据链】** **双通道并存、直连优先(expand 阶段)**:battle 节点在自身 TCP 端口(`NodeInfo.endpoint`,框架已分配并发布到 etcd)开客户端监听;出站 S2C 有已验证直连即直发,否则回落既有 Kafka→gate 路径;上行经 gate gRPC 的路径保留。gate / scene / match **零改动** | 旧客户端零改动行为不变(AGENTS.md §11.3 向后兼容:expand→migrate→contract);战斗流量不再挤 gate 这条共享通道;收缩阶段(删 gate 中继)另起决策,须先确认全部客户端已切直连 |
| D24 | **票据由 battle 节点自己签发并校验**:HMAC-SHA256,独立密钥 `BaseDeployConfig.battle_token_secret`(全部 battle 实例共享,与 `gate_token_secret` 分域);签名 = hex(HMAC(secret, `BattleTicketPayload` 序列化字节)),与 gate 令牌完全同口径 | battle 是唯一知道房间名单的一方;不给 match 发密钥(少一处持密者);"本地验签、不回问大厅"仍成立;客户端 / robot 两条连接复用同一套握手代码。备选"match 签发"被否:match 与 battle 各自 Kafka 生产者,分配包与开战包跨生产者无序,而 battle 自签能在同 key 上保证"先分配再开战" |
| D25 | **票据寿命 = 房间作废期限 `deadline_ms`,可重复用于重连,不做 jti 一次性**。验签之后仍须:签给本节点(`battle_node_id` + 实例 UUID `battle_instance_id`)、未过期、角色合法、房间仍存在且该玩家按角色确在名单。客户端丢票(冷启动)经大厅通道 `MatchService.RequestBattleTicket` 补签:match 从会话 metadata 取权威 player_id、按观战索引 `spectate:battle:{battle_id}` 定位房间所在 battle 节点,再调内部 `BattleNode.IssueBattleTicket{battle_id, player_id}` 由 battle 核对名单自签,裁决原样透传(原 gate→battle 的 `BattleClientPlayer.RequestBattleTicket` 已删,改道理由见 [client-rpc-router.md](./client-rpc-router.md) D33:路由服不转发 battle 消息) | 房间销毁即全部票据失效,一次性 jti 表没有增益;实例 UUID 防节点重启后 node_id 复用让旧票复活;补签走已鉴权的大厅会话,票据发放只有两条通道(开局推送 / 会话补签),都由会话身份背书;match 不持票据密钥、不复制名单 |
| D26 | **【部分已被 §22 取代(2026-09-29):「签不出票只记 WARN 不阻断开局、全程走 gate 中继」→ §22 D70 fail-closed;观众「先推 Assigned 再推 `SpectateStateS2C`」→ §22 D69 首帧随直连握手下发;「先 Assigned 再 Start、同 key 保序」仍有效】** **投递顺序**:CreateBattle 成功后每参战者先推 `BattleAssignedS2C{battle_id, host, port, token_payload, token_signature, expire_at_ms, role}` 再推 `BattleStartS2C`(同 Kafka key=player_id 保序);观众 AddObserver 成功后先推 Assigned(role=OBSERVER)再推 `SpectateStateS2C`。签不出票(见 D27 空密钥 / endpoint 未就绪)只记 WARN 不阻断开局,该玩家全程走 gate 中继 | 客户端拿到票据就能建连,开战包此刻还没直连、照旧经 gate;两条路径都合法,客户端对战斗消息来自哪条连接不敏感 |
| D27 | **直连安全闸(逐条镜像 gate)**:并发上限 `battle_max_connections`(0 仅 dev/test);空密钥处置由 `BATTLE_RUN_MODE` 决定(dev/test 跳过签名比对但载荷 / 节点 / 期限 / 名单照常校验,prod 拒绝启动 + 拒连);未验证连接 10s 握手期限;验证前一切非握手消息即关;`ClientRequest` 体 ≤1KB;每消息号限速(`MessageLimiter`,与 gate 同一张表);消息号白名单 = `BattleClientPlayer` 的四条客户端 RPC;非法包累计达阈值即关(阈值与 gate 同源:`IllegalPacketCounter`,默认 50,`GATE_ILLEGAL_PACKET_THRESHOLD` 可调、0 = 只计数不踢);输出缓冲 2MB 高水位断连;启动门禁除非空外还查密钥 ≥32 字节且 ≠ `GateTokenSecret`(prod 拒启,dev/test WARN) | 直连面是第二个对公网开放的端口,必须与 gate 同等设防;运行模式变量与 gate 分开(`BATTLE_RUN_MODE`):同机 gate=dev 不能顺带放行 battle |
| D28 | **会话身份合成**:直连消息进 `BattleRoomManager` 时合成 `SessionDetails{player_id, session_id=路由快照里的 gate 会话号}`,四个 Handle* **零改动**(复用既有防串房 / 防观众冒充校验) | 权威身份来自已验证票据而非请求体,与 gate 注入 metadata 的形态一致;`session_id` 只用于日志对齐 |

### 18.2 线协议与握手(客户端接入契约;Unity 客户端在独立仓库,按此实现)

- **帧格式与 gate 完全一致**:ProtobufCodec(`len | nameLen | typeName | protobuf | adler32`);上行 `ClientRequest{id, message_id, body}`,下行 `MessageContent{message_id, serialized_message, id, error_message}`。客户端复用大厅连接的编解码与按 `message_id` 分发的 handler 表,**不需要第二套协议栈**。
- **握手**:TCP 连上 `BattleAssignedS2C.host:port` 后,**首包必须**是 `BattleTokenVerifyRequest{payload=token_payload, signature=token_signature}`(两字段原样透传,客户端不解析 payload);服务端回 `BattleTokenVerifyResponse{success, error, battle_id}`。失败后服务端主动断开;客户端不得重试同一张票超过 1 次,应回大厅通道 `MatchService.RequestBattleTicket` 补签。
- **握手之后**:只允许 `SubmitBattleAction / GetBattleState / StopWatchBattle / SetAutoBattle` 四个消息号;应答 `MessageContent.message_id` = 请求消息号、`id` = 请求 id。别的消息号回信封错误 `kInvalidParameter` 并计非法包。
- **S2C**:握手成功后本玩家的 `NotifyTurnResult / NotifyBattleEnd / NotifySpectateState / NotifySpectateTurnResult / NotifySpectateEnd` 全部改从直连到达;`NotifyBattleStart`、`NotifyBattleAssigned`、`NotifyBattleReconnect` 仍从大厅连接到达(它们发生在直连建立之前)。客户端两条连接的 S2C 走同一个 handler 表即可。
- **生命周期**:战斗结束 / 作废 / 观众被清退时服务端先推终局包再 `shutdown`(FIN 在输出缓冲排空之后),客户端收到 FIN 即视为本局直连结束,不重连;战斗中直连意外断开 → 若手里的票据未过期直接重连握手(同票可重用,D25),否则大厅通道 `MatchService.RequestBattleTicket(battle_id)` 取新票再连。`NotifyBattleReconnect`(scene 在 RECONNECT 链路推)到达时按同样流程建直连。
- **【已被 §22 D66 / D68 / D74 取代(2026-09-29):收缩后没有回落,直连建不起来 = 本局不能出手,服务端每回合超时替玩家默认出手;客户端处置见 §22.3「Unity」;原文保留】** **回落**:直连建不起来(网络策略 / 端口不可达)时客户端可以继续经大厅连接收发战斗消息 —— 服务端两条路径都在(D23)。客户端应打点上报"直连失败率",这是收缩阶段(删 gate 中继)的前置数据。
- **消息号**:`BattleClientPlayerNotifyBattleAssignedMessageId` / `MatchServiceRequestBattleTicketMessageId` 由 proto-gen 分配(`proto/message_id.txt`;补签消息号挂在 MatchService 下,`BattleClientPlayerRequestBattleTicket` 已删),客户端 / robot 用生成常量,不写数字。

### 18.3 服务端改动集

| 文件 | 改动 |
|---|---|
| `proto/battle/player_battle.proto` / `proto/match/match_service.proto` / `proto/battle/battle_node.proto` | `player_battle.proto` 新增 `eBattleTicketRole` / `BattleTicketPayload` / `BattleTokenVerifyRequest` / `BattleTokenVerifyResponse` / `BattleAssignedS2C` / `RequestBattleTicketRequest` / `RequestBattleTicketResponse`,service 加 `NotifyBattleAssigned`(`RequestBattleTicket` 已从本服务删除);补签入口挂在 `MatchService.RequestBattleTicket`(客户端协议,复用上述请求 / 响应消息);`battle_node.proto` 加内部 `BattleNode.IssueBattleTicket(IssueBattleTicketRequest{battle_id, player_id}) returns (IssueBattleTicketResponse{error_message, assignment})` |
| `go/match`:`internal/server/matchserviceserver.go`、`internal/logic/requestbattleticketlogic.go`(+ `requestbattleticket_test.go`)、`internal/metrics` | `MatchService.RequestBattleTicket`:player_id 只取会话 metadata → `spectate:battle:{battle_id}` 取 `battle_node_id`(不存在 = 房间已结束,kInvalidParameter)→ `BattleNodes.EndpointOfNode`(未发现 = kServiceUnavailable)→ `BattleNode.IssueBattleTicket`(3s 超时,RPC 缝 `issueBattleTicketFn` 可替换)→ battle 裁决原样透传;计数 `match_request_battle_ticket_total{outcome}` |
| `proto/common/base/config.proto` + `cpp/libs/engine/config/config.cpp` + `bin/etc/base_deploy_config.yaml` | `battle_token_secret = 16`(yaml `BattleTokenSecret`)、`battle_max_connections = 17`(yaml `BattleMaxConnections`;**代码无默认值**:缺键即 proto3 零值 0,prod 拒绝启动、dev/test 按硬上限 65535 放行 —— 本地 yaml 显式配 4096,部署 ConfigMap 必须显式带这一项,与 `GateMaxConnections` 同纪律) |
| `cpp/libs/engine/core/security/token_security.h`(新) | HMAC / 常数时间比较 / RunMode / ClassifyTokenSecret 的唯一实现;`cpp/nodes/gate/gate_security.h` 改为 using 别名(API 不变,GM 鉴权部分原地不动;`gate/tests/gate_security_test.cpp` 编译命令多一个 `-I cpp/libs/engine/core`) |
| `cpp/nodes/battle/battle_security.h`(新) | `BATTLE_RUN_MODE`、`SignTicket` / `VerifyTicketSignature`(字节级)、`ClassifyTicketFields`(与 proto 解耦的字段判定) |
| `cpp/nodes/battle/client/battle_client_edge.{h,cpp}`(新) | 客户端直连面:装到 `Node::GetTcpServer()`(与 gate 同时机 `SetAfterStart`),握手 / 派发 / 下行 / D27 五道闸 |
| `cpp/nodes/battle/logic/battle_room_manager.{h,cpp}` | 【`PushToPlayer` 已被 §22 D68 拆成 `PushBattleFrame`(只走直连)与 `PushLobbyAnnouncement`(只用于 Assigned / Start,可回落);签不出票已按 D70 fail-closed(2026-09-29);原文保留】`directConnByPlayer`;`PushToPlayer`(直连优先、Kafka 回落)替换全部 `PushMessageToPlayer`(改名 `PushMessageViaGate`);`BuildAssignment` / `PushAssignment` / `AttachDirectConnection` / `DetachDirectConnection` / `CloseDirectConnection(s)` / `HandleIssueBattleTicket`(player_id 来自 match 请求,battle 只核对名单;原 `HandleRequestBattleTicket` 去掉会话入参);开局 / 观战接入先推分配包;结束 / 作废 / 销毁 / 观众清退关直连 |
| `cpp/nodes/battle/handler/grpc/battle_node.{h,cpp}` | 【`battle_client_player_service.{h,cpp}` 已随 §22 D66 删除,`handler/grpc` 只剩控制面 `battle_node.{h,cpp}`(2026-09-29);原文保留】`IssueBattleTicket`(match → battle 内部 gRPC,runInLoop + promise 委托 `HandleIssueBattleTicket`,status 恒 OK、错误经 tip);`battle_client_player_service.{h,cpp}` 不再覆写补签(生成基类已无该方法) |
| `cpp/nodes/battle/main.cpp` | `ValidateBattleClientEdgeConfigOrDie` 启动门禁;装配 edge;停机断开全部直连 |
| `cpp/nodes/battle/{battle.vcxproj,battle.vcxproj.filters,CMakeLists.txt}` | 新源文件入构建 |
| `cpp/nodes/battle/tests/battle_ticket_test.cpp`(新) | 独立 gtest(18 条):签名往返 / 篡改 / 分域 / 字段判定顺序 / 期限闭区间 / 空密钥处置 / 密钥强度(≥32 字节、≠ gate) |
| `tools/proto_generator/protogen/go.sum` | `luyuancpp/proto2mysql@v0.1.0` 校验和更正为上游实际值(否则 `proto-gen-build` 拒建,见 §18.8 第 1 步) |
| `robot/`:`pkg/client.go`(`VerifyBattleToken`)、`battle_direct_conn.go`(新)、`logic/gameobject/player.go`、`logic/handler/battle_client_player_notify_battle_assigned.go`(新)、`logic/handler/match_service_responses.go`(`MatchServiceRequestBattleTicketHandler` + `init()` 登记:生成的分发表只收 `ClientPlayer` / `GamePlayer` 命名的服务)、`battle_smoke_scenario.go`、`battle_smoke_cross_zone_scenario.go`、`config/config.go`、`etc/battle_smoke.yaml` | 机器人凭票据直连并断言回合结果 / 终局包从直连到达;`battle_smoke.skip_direct_connect=true` 走回落路径(**该开关已被 §22 D73 删除**) |

**【本段已被 §22 D66 / D67 / D72 取代(2026-09-29):gate 两种模式都不中继、Bind/Unbind 契约已删,scene 只推 `BattleReconnectS2C`;原文保留】** **没改的**:gate 旧模式(继续中继 + Bind/Unbind;路由模式见 §18.7)、scene(RECONNECT 仍重发 BindBattleEvent + BattleReconnectS2C)、match 的 gather(只新增 `RequestBattleTicket` 补签入口)、Kafka 契约、结算 / 确认 / 结果回流链路。

### 18.4 Redis / Kafka 契约变化

无。票据不落 Redis(D25:房间存在 + 名单即权威);直连面不产生任何 Kafka 消息。

### 18.5 新增不变量(并入 §7 语义)

10. 直连面放行的唯一凭据是本节点签发的票据;票据只经两条已鉴权通道发放(开局 / 观战接入的 `NotifyBattleAssigned` 推送,大厅会话经 `MatchService.RequestBattleTicket → BattleNode.IssueBattleTicket` 的补签 —— player_id 由 match 从会话取,battle 只核对名单),battle 不提供任何不验票的客户端入口,也不提供任何绕过 match 的补签入口;
11. 直连消息的玩家身份只来自票据(`SessionDetails.player_id`),请求体里的 `battle_id` 只用于查房,不用于身份;既有 Handle* 的名单校验对直连同样生效;
12. 同一玩家同一房间最多一条有效直连;重连即替换,旧连接的迟到 FIN 不得摘掉新连接(按连接身份比对);
13. 房间结束 / 作废 / 销毁必须关闭其全部直连;不允许存在"房间已不存在、直连仍挂着"的状态。

### 18.6 部署与运维

> **(2026-09-29 更正)本节的部署部分已被 §22 D75 与集群外入口 D76–D93 取代**:battle 的 K8s 形态、客户端寻址与运维手册以
> [k8s-client-entry.md](./k8s-client-entry.md) 为准。下文「部署链尚未接入这两个键」「battle 目前连 manifest 都没有」两句已过时:
> `k8s_deploy.ps1` 已有 `Apply-BattlePool`,`MMORPG_BATTLE_TOKEN_SECRET` 在生成 battle ConfigMap 时经 `Resolve-InjectedSecret`
> 注入并校验 ≥32 字节、≠ gate 密钥(不是本节设想的 `Initialize-InjectedSecrets`);`release_preflight.ps1` 的 battle 目标至今仍未做。
> 「配置」「观测」两条仍有效。原文保留。

- 配置:`BattleTokenSecret`(生产必须与 `GateTokenSecret` 不同且 ≥32 字节 —— 两条都由 battle 启动门禁强制,prod 违反即 LOG_FATAL;进 Secret 不进 git,本地 `base_deploy_config.yaml` 里的 `local-dev-` 值只用于开发)、`BattleMaxConnections`(无代码默认值,必须显式配);环境变量 `BATTLE_RUN_MODE=dev|test|prod`(默认 prod)。
- **部署链尚未接入这两个键(与 battle manifest 同批补,缺一项 battle Pod 就 CrashLoopBackOff)**:`tools/scripts/k8s_deploy.ps1` 的 `Initialize-InjectedSecrets` 加 `MMORPG_BATTLE_TOKEN_SECRET`(`-MinLength 32`,并断言 ≠ `MMORPG_GATE_TOKEN_SECRET`)、node ConfigMap 模板追加 `BattleTokenSecret` / `BattleMaxConnections`(后者 `Get-AuthoritativeScalar` 取自 `base_deploy_config.yaml`)、`release_preflight.ps1` 加 battle 目标、`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 镜像 GateTokenSecret 那条非空断言。现在不改:battle 没有 manifest,而 `Initialize-InjectedSecrets` 在 prod 档位缺环境变量会 throw,提前加会把现有 gate/scene 发布一起打死。
- **【已被 §22 D70 / D76–D93 取代,见 [k8s-client-entry.md](./k8s-client-entry.md):票据地址改由 `client_endpoint::ClientFacing` 选出(拿不到客户端可达地址即签票失败、按 D70 拒绝);原文保留】** 寻址:客户端连的是 `NodeInfo.endpoint`(`NODE_IP` 解析出的注册地址 + 框架分配的 TCP 端口)。本地 `dev_tools.ps1 -NodeIp <LAN ip>` 已能让局域网客户端连到;**K8s 上 battle Pod 需要客户端可达的入口(hostPort / NodePort,同 gate 的做法)—— C 档待办,battle 目前连 manifest 都没有(PROGRESS 2026-09-04)**。
- 观测:`battle 客户端直连面已就绪` / `battle 直连握手成功` / `battle 直连拒绝(采样) reason=…` / `battle 关闭房间全部直连` 四类日志;拒绝原因枚举:`at_capacity / token_secret_not_configured / handshake_timeout / ticket_hmac_mismatch / ticket_payload_parse_failed / empty_identity / node_mismatch / instance_mismatch / expired / role_invalid / ticket_not_in_roster / request_before_verify / unknown_message_type`。

### 18.7 明确不做 / 后续

- **【已被 §22 D65 / D66 / D73 / D75 取代(2026-09-29):收缩已一次做完,gate 两种模式都不中继战斗、不处理 Bind/Unbind(契约已删),K8s 默认已翻 "1",`skip_direct_connect` 已删;原文保留】** **收缩阶段 = `GATE_CLIENT_RPC_ROUTER` 路由模式**(见 [client-rpc-router.md](./client-rpc-router.md) D33 / D34):gate 设该环境变量后白名单只剩 `{Scene(TCP), ClientRpcRouter}`,不再中继任何 battle 消息、忽略 Bind/Unbind 事件,直连成为唯一战斗通路;票据补签因此改道 `MatchService.RequestBattleTicket → BattleNode.IssueBattleTicket`(D25 已按此口径更新),`skip_direct_connect: true` 的回落路径在路由模式下预期失败。默认仍是旧模式(D34),翻转前提不变:客户端直连失败率数据 + scene 的 RECONNECT 链路换成推 `BattleAssignedS2C`;
- **【已被 §22 D65–D67 取代:已删;但本条把 `SetIfEmptyHandler` 桥接列入删码清单说宽了——它(连同 `sResponseTypeToMsgId`)是所有 gRPC 回包的通用出口,**不删**(§22 D66、§22.8 勘误第 6 行)】** **收缩(contract)**:默认翻转到路由模式后再删 gate 的 battle 中继代码(四类白名单、Bind/Unbind 事件、`SetIfEmptyHandler` 桥接);
- **已修(2026-09-16,见 §19 D38;原文保留作判据链)**:`RedirectToGate`(跨区 / gate 迁移)后战斗既收不到重绑也收不到重连提示。判据链:`DecideEnterGame` 只在旧会话 `State==StateDisconnecting` 时判 `ShortReconnect`,而 gate 迁移时旧会话通常仍是 `StateOnline` → 判 `ReplaceLogin` → `enter_gs_type=LOGIN_REPLACE`;此时 `player_battle.cpp` 的 `OnPlayerEnterScene` 两步守卫都不触发 —— 第 1 步 `RestoreBattleFreezeOnLogin` 被 `!any_of<InBattleComp>` 挡住(同区迁移实体还在),第 2 步要求 `enterGsType == LOGIN_RECONNECT`。后果:`BindBattleEvent` 不重发 → 新 gate 上没有该会话的 battle 绑定 → **gate 中继路径同样断**(`requiresSessionBinding` 命中 BattleNodeService),`BattleReconnectS2C` 也不推。修法:第 2 步的条件改成「有 `InBattleComp` 且本次是换会话类登录(RECONNECT **或** REPLACE)」,或在 `RestoreBattleFreezeOnLogin` 之外单列一条「会话换了就重绑」的路径。**客户端已先自愈**(`GameClient.RedirectFlow` 在重定向成功后用捕获的 battle_id 主动走 `MatchService.RequestBattleTicket` 补签重建直连,该链路经 match 随机路由、不依赖 gate 绑定),所以路由模式下不受影响;但旧模式下服务端不修就仍是断的;
- 每消息 HMAC(`hmac-message-signing.md` 的 slice B)—— 直连面与 gate 同样只有 adler32,两处一起做;
- 票据吊销(踢人 / 封号中途):目前靠房间销毁;需要时加 `RevokeTicket` 走 match→battle gRPC;
- 观众直连上限单独配置(现在与参战者共享 `battle_max_connections`)。

### 18.8 Codex 验证清单(按序;任一步红即停)

1. **重生成 proto**(仓库根):`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。期望:`proto/message_id.txt` 末尾新增 `BattleClientPlayerNotifyBattleAssigned` / `BattleNodeIssueBattleTicket` / `MatchServiceRequestBattleTicket`(`BattleClientPlayerRequestBattleTicket` 已删,不再出现);`cpp/generated/rpc/service_metadata/{player_battle,battle_node,match_service}_service_metadata.h` 出现同名 `*MessageId` 常量;`robot/generated/pb/game/message_id.go` 同;`robot/logic/handler/message_body_handler.go` 的 map 只多一行 `BattleClientPlayerNotifyBattleAssigned`(生成器 `robot_case.go` 只收服务名含 `ClientPlayer` / `GamePlayer` 的服务,`MatchService*` 应答由 `match_service_responses.go` 的 `init()` 自行登记,handler 函数已手写);`client/unity/Assets/Scripts/Net/Generated/` 多两个 handler 桩(客户端仓的事,不在本仓验)。**第 1 步之前 C++ / robot 编译不过是预期的。**
   **2026-09-05 实跑踩到的三个坑(照此做,否则第 3 步链接必红):**
   - **先 `proto-gen-build` 再 `proto-gen-run`**:仓里的 `proto-gen.exe` 是 08-11 的旧二进制(未跟踪),它对**没有 `package` 的 proto**(battle 域全部)把 sender 前置声明写成 `namespace {void SendBattleNode…}`(匿名命名空间),gate / scene / battle 链接时 17 个 `LNK2019`;生成器源码已修(`service_register_info.go` 空 package 走全局声明),只是二进制没重建。
   - **`protoc` 必须在 PATH**:`proto-gen-run` 的 C++ 序列化步骤直接 exec `protoc`,本机装在 `third_party/grpc/install_vs2026_dbg/bin`(含 `grpc_cpp_plugin.exe`),不在 PATH 时 fatal 于 `rpc_message.proto`、registry 根本走不到。
   - **`tools/proto_generator/protogen/go.sum` 里 `luyuancpp/proto2mysql@v0.1.0` 的校验和是陈旧的**(代理与 GitHub 直连拿到的都是 `h1:JrtGCOG…`,go.sum 记的是 `h1:da7YQBr…`),`proto-gen-build` 会以 SECURITY ERROR 拒建;本次已把 go.sum 更新为上游实际内容的校验和(API 兼容,构建通过)。
   - 重生成的副产物里 **`cpp/generated/grpc_client/grpc_init_client.cpp` 多出 `messageId == 176u || 177u`** 是必需的(PlayerBattle 完成队列分派,少了 gate 收不到这两个 RPC 的回包);而本机 `protoc-gen-go v1.36.10` / `protoc-gen-go-grpc v1.6.0` 比生成 HEAD 时用的版本旧,会把 4 个无关 go pb(`player_attribute*` / `match_event`)改成纯版本注释噪音,**已还原,不要把它们带进提交**。
2. **Go**:`cd go/proto && go build ./...`;`cd go/match && go build ./... && go test ./...`(含 `RequestBattleTicket` 逻辑的 6 条单测:成功透传 / 无会话 / 索引缺失 / 节点未发现 / RPC 出错 / battle 拒签透传);**robot 走 vendor 构建**(`robot/vendor/modules.txt` 存在,`go build` 默认 `-mod=vendor`,读的是 `robot/vendor/proto/battle/*.pb.go` 而不是 `go/proto/`):先 `cd robot && go mod vendor` 刷新(需要能拉私有模块 `github.com/luyuancpp/muduoclient`;拉不到时退而求其次:把 `go/proto/battle/*.pb.go` 原样复制到 `robot/vendor/proto/battle/`,`shared` 未改不用动),再 `go build ./... && go vet ./...`;刷新后的 `robot/vendor/proto/**` 随本次提交。上次 robot 侧改 proto 就漏过这一步(PROGRESS 2026-09-03「go mod vendor 刷新」)。
3. **C++ 全解决方案串行编译**:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(仓库根;`/m:1` 必须,并发会报假 C1041 / LNK1104)。受影响工程:`proto` / `rpc` / `grpc_client`(重生成)、`config`(config.cpp)、`gate`(gate_security.h 改 include)、`battle`(全部新文件)。期望 0 error。 **2026-09-05 实测:重建 proto-gen 并重生成后,gate / scene / battle 三个 exe 均链接成功并复制到 `bin/`;不要用 Release 配置(third_party 全部是 Debug/MDd 静态库,Release 会报成片 `LNK2038` 运行库不匹配,与本改动无关)。**
4. **独立单测**(Linux 或带 g++ 的容器,仓库根,`GT=third_party/grpc/third_party/googletest/googletest`;**Windows 无 g++ 时用 MSVC**:`vcvars64` 后 `cl /std:c++latest /EHsc /MDd /utf-8 /I cpp
odesattle /I cpp\libs\engine\core /I third_party\grpc\install_vs2026_dbg\include /I %GT%\include <test.cpp> %GT%\src\gtest_main.cc /link /LIBPATH:lib gtest.lib crypto.lib ssl.lib ws2_32.lib advapi32.lib` —— `lib/gtest_main.lib` 链不出 `main`,要直接编 `gtest_main.cc`;2026-09-05 实测 battle 18/18、gate 22/22 全绿):
   - `g++ -std=c++23 -I cpp/nodes/battle -I cpp/libs/engine/core -I "$GT/include" -I "$GT" cpp/nodes/battle/tests/battle_ticket_test.cpp "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc" -lssl -lcrypto -lpthread -o /tmp/battle_ticket_test && /tmp/battle_ticket_test` → 18 条全绿;
   - `g++ -std=c++23 -I cpp/nodes/gate -I cpp/libs/engine/core -I "$GT/include" -I "$GT" cpp/nodes/gate/tests/gate_security_test.cpp "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc" -lssl -lcrypto -lpthread -o /tmp/gate_security_test && /tmp/gate_security_test` → 原有用例全绿(回归:gate_security.h 改为 using 别名后 API 不变)。
5. **本地整栈冒烟**(`pwsh -File tools/scripts/dev_tools.ps1 -Command dev-start`,`BATTLE_RUN_MODE` 不设即 prod,`base_deploy_config.yaml` 已配 `BattleTokenSecret`):
   - battle 日志出现 `battle 客户端直连面已就绪: endpoint=<ip>:<port> max_connections=4096 run_mode=prod` 与 `Battle direct-connect ticket verification ENFORCED`;
   - `cd robot && .
obot.exe -c etc/battle_smoke.yaml` → `BATTLE_SMOKE_OK … a_direct_turns>=1 b_direct_spectate_turns>=1`,battle 日志有两条 `battle 直连握手成功 … signature_checked=1`(role 各一)与 `battle 关闭房间全部直连`;
   - **【已被 §22 D73 取代:`skip_direct_connect` 已删、没有回落路径,现行冒烟见 §22.7 第 9 步;原文保留】** 把 `etc/battle_smoke.yaml` 里 `battle_smoke.skip_direct_connect: true` 打开再跑一次 → `BATTLE_SMOKE_OK … a_direct_turns=-1 b_direct_spectate_turns=-1`(D23 回落路径完整);
   - `dev-start-zones 1,2` + `.
obot.exe -c etc/battle_smoke_cross_zone.yaml` → `CROSS_ZONE_MATCH_OK … a_direct_turns>=1 b_direct_turns>=1`(两侧连同一个 battle 进程)。
6. **负向**:
   - 临时把 `BattleTokenSecret` 改成另一个值只重启 battle(robot 侧票据由 battle 签、battle 验,改密钥不影响正向;要造 `ticket_hmac_mismatch` 需在 robot 里把 `token_signature` 改一个字节后握手,期望 `BattleTokenVerifyResponse.success=false error="invalid ticket signature"` 且连接被关);
   - `BattleTokenSecret: ""` + 不设 `BATTLE_RUN_MODE` 启动 battle → 进程 FATAL `Refusing to start: battle_token_secret is empty while run_mode=prod`;加 `BATTLE_RUN_MODE=dev` → 启动成功并打 `SECURITY WARNING`,冒烟仍过(`signature_checked=0`);
   - 用 `nc`/裸 TCP 连 battle TCP 端口不发任何东西 → 10s 后被关(`handshake_timeout` 采样日志)。
7. 以上任一步失败:保留 battle 日志(`run/logs/cpp_nodes/battle*`)、robot 日志与失败 step 名,不重试、不改判据。

## 19. 收缩阶段决策 + 换会话重绑修复(2026-09-16)

> 背景:用户 2026-09-16 拍板"先把客户端直连 battle 做完"。核对结果:§18 的服务端、Unity 客户端(`b5cf6ef`)、robot 冒烟三边在 9 月 5-6 日已全部落地并通过,本机一键启动(`start_game.ps1 -GateRouterMode` 默认 `'1'`)已跑在路由模式,gate 已不中继战斗。所以"做完"= 收缩阶段的几件收尾,而不是重做。本节只记决策与本轮改动;K8s 对外入口由并行会话(microservice-zone-contract §17 "完整 K8s Battle 验收")负责,不在此重复。

### 19.1 决策

| # | 决策 | 理由 |
|---|------|------|
| D36 | **【已被 §22 D65 / D75 取代(2026-09-29):前提被豁免、事后补验;K8s `-GateRouterMode` 默认已翻 "1"】** **翻转默认到路由模式的前提改为"K8s 上以路由模式跑通 battle-smoke"一条**;§18.7 / D34 原前提之一"客户端直连失败率数据"作废 | 那条前提是为"老客户端仍走 gate 中继"的过渡期准备的;项目未上线、没有老客户端(memory:xuanming-prelaunch-no-legacy-data),expand→migrate→contract 里的 migrate 窗口不存在。本地已翻;K8s `k8s_deploy.ps1 -GateRouterMode` 默认 `"0"` 待并行会话的 K8s battle 验收通过后翻,C++ 默认值按 D-12 不改 |
| D37 | **【已被 §22 D65–D67 取代(2026-09-29):不等 K8s 实跑,已一次删完;删码清单以 §22.3 为准(反查桥不删)】** **gate 中继代码与 Kafka Bind/Unbind 契约在 K8s 路由模式 battle-smoke 通过后同批删**,不提前删 | 路由模式是 chat / guild / trade 等新服务共同的唯一形态,直连模式代码是它们目前唯一的回退面(D-12);在 K8s 未实跑前删码等于拆掉所有人的回退。删码清单沿用 battle-transport-decision.md D6 修订 + §18.7 收缩条 |
| D38 | **【部分已被 §22 D72 取代(2026-09-29):触发条件(RECONNECT 或 REPLACE)与「不新增 scene→battle gRPC」仍有效;「`BindBattleEvent` 继续发」已删,scene 只推重连提示】** **scene 换会话重绑:RECONNECT 与 REPLACE 都触发,只推 `BattleReconnectS2C`,不新增 scene→battle gRPC** | 客户端收到重连提示后已按 §18.2 走 `MatchService.RequestBattleTicket` 补签重建直连(`GameClient.RedirectFlow` 也如此自愈),服务端不需要拿票据;新增 scene→battle 同步调用会打破 D6 "没有 C++ 节点同步调 battle"。`BindBattleEvent` 继续发(旧模式还在用),随 D37 一并删 |
| D39 | **【已被 §22 D68 取代(2026-09-29,已落码):battle 的回落白名单只有 `NotifyBattleAssigned` / `NotifyBattleStart`;`NotifyBattleReconnect` 由 scene 经大厅会话发,不归 battle 回落管辖;不再等 D37 先后】** **Kafka→gate 的 S2C 回落收缩后只保留 `NotifyBattleAssigned` / `NotifyBattleStart` / `NotifyBattleReconnect`**(它们发生在直连建立前,必须经大厅);战斗帧(`NotifyTurnResult` / `NotifyBattleEnd` / `NotifySpectate*`)在无直连时**不再回落、直接不发**,客户端靠重连后 `GetBattleState` 补拉 | 直连是唯一战斗通路(D33);保留战斗帧回落等于维持两条代码路径。代价:连不上 battle 端口的客户端不能战斗——与 LoL / 王者要求 UDP 可达同一口径,写进客户端提示 |
| D40 | **`SubmitBattleAction` 加回合号做幂等**列为收缩后的第一项后续,不在本轮 | 收缩后直连抖动丢一手 = 该回合默认普攻;有回合号客户端才能安全重发。改 proto,受 regen 解冻约束 |

### 19.2 本轮改动

- `cpp/libs/services/scene/battle/system/player_battle.cpp` `OnPlayerEnterScene` 第 2 步:条件由 `enterGsType == LOGIN_RECONNECT` 改为 `LOGIN_RECONNECT || LOGIN_REPLACE`(D38)。判据链见 §18.7 原文:跨 gate 重定向后旧会话通常仍 `Online`,login 判 `REPLACE`,原条件漏掉这条路径。`LOGIN_FIRST` 不进该分支(首登实体新建、无 `InBattleComp`,由第 1 步按锁 + ctx 重建)。
- 文档:§18 状态行改为已冒烟通过;§18.7 待修项标记已修;[moba-battle-target-architecture.md](./moba-battle-target-architecture.md) §六 过时缺口更正。

### 19.3 未编译,待 Codex 验证(按序)

1. **C++ scene 串行编译**(仓库根):`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64 /t:scene`;期望 0 error,`bin/scene.exe` 更新。并行会话正在改 `cpp/nodes/scene/handler/grpc/scene_node_service.*`,若因其未完成改动报错,只保留错误摘要、不改判据。
2. **【已被 §22.7 第 9 步取代(2026-09-29):`skip_direct_connect` 回落已删,两种路由模式都只验直连】** **整栈冒烟回归(当前 HEAD)**:§18.8 第 5 步三条(直连 / `skip_direct_connect` 回落 / 跨 zone)。注意路由模式下 `skip_direct_connect: true` 预期失败(client-rpc-router.md §7.5),只在 `GATE_CLIENT_RPC_ROUTER=0` 下跑回落那条。
3. **【已被 §22 D72 取代(2026-09-29):`BindBattleEvent` 已删,判据「目标 gate 日志出现 BindBattleEvent 处理记录」作废;现行判据 = 换会话后客户端收到 `NotifyBattleReconnect` → 补签 → 直连就绪 → `GetBattleState`,robot 未覆盖(§22.5 第 9 条),由 Unity 实机验(§22.7 第 10 步)】** **D38 专项(旧模式)**:双 gate 起服,robot 战斗中触发 `RedirectToGate`(或用 `dev_tools.ps1` 把玩家的 gate 迁移),期望:目标 gate 日志出现 `BindBattleEvent` 处理记录、客户端收到 `NotifyBattleReconnect`(消息号 `BattleClientPlayerNotifyBattleReconnectMessageId`)、随后 `GetBattleState` 成功、战斗继续到 `BATTLE_SMOKE_OK`;改动前同一场景战斗断掉(对照)。
4. **Unity 实机**(客户端仓 `tools/run_crosszone_pair.ps1`):`DevAutoPilot` 的 `BattleEnd` 行 `direct_turns == turns`,battle 日志有 `battle 直连握手成功 … signature_checked=1` 与 `battle 关闭房间全部直连`。
5. 任一步失败:保留 scene / gate / battle 日志与失败 step 名,不重试、不改判据。

## 20. 回合制战斗缺口收口 G1–G9 索引(2026-09-17)

- 详细设计、决策 D41–D50、改动集与 Codex 验证清单:[turn-battle-gap-closure.md](./turn-battle-gap-closure.md)。
- 一句话:掉落(Monster 表加 `drop` 槽 + 引擎终局摇点 + scene 真入包)、战斗内用药(Item 表加三列、
  可给队友、PVP 限次、提交回错误码)、消耗按实际持有夹紧扣除并逐条落流水、局中闸补五处、
  快照 buff 与技能进引擎前清洗、战斗状态按收信人裁剪并回传本人道具余量、Item 表并入配表指纹。
- **契约影响**:配表指纹变更(scene 与 battle 必须同版本同批替换);`BattleStateS2C` 新增
  `self_items = 7`(需 regen);tip 零新增。
- G9(结算路由固定在开局)已由 2026-09-08 的 R07 结算出箱修掉主体,本轮只更正文档口径,
  残留是首投最多约 10s 的延迟。§3.2 / §6 / §7 对 `battle:settlement:pending:*` 写者的描述仍是
  R07 之前的旧口径(写者不只 scene,battle 节点也先写),待一并订正。

## 21. 直连收缩现状核对(2026-09-29,静态阅读)

> **(2026-09-29 更正)本节是 §22 收缩之前的现状快照,原文保留不改,已不代表现状。** 经 5 路对抗核验,
> 部分表述有误或不全(§21.1 三行的连接数、§21.2 反查桥的归属、§21.4「相等」的口径、§21.5 第 1/3/4/5/6 条等),
> 逐条勘误见 **§22.8**。§21.6 的待决策项已裁决:D26 → §22 D70(fail-closed),D39 → §22 D68(与删码同批,D65),
> 集群外入口 → D76–D93([k8s-client-entry.md](./k8s-client-entry.md));§21.5 第 1、2 条本批不做(§22.6)。
> **(2026-09-29 再更正)§21.5 第 2 条已由集群外入口 2b 第 9 条修掉**:match 改为在 CreateBattle **之前**写观战记录(fail-closed),
> 写不进去就不开这局(`outcome=index_failed`),见 §22.5 第 4 条与 k8s-client-entry D82;收缩批本身仍未改这条。第 1 条仍未做。

> 用户 2026-09-29 问「battle 为什么不经 gate、客户端有几条连接」,本节是顺带核对出的现状快照。
> 原理与问答见 [battle-transport-decision.md §8](./battle-transport-decision.md)。
> 没有编译,没有运行,没有读客户端仓。行号以 2026-09-29 工作树为准。

### 21.1 三处默认值不一样

开关是 `GATE_CLIENT_RPC_ROUTER`。

| 环境 | 开关默认 | gate 怎么处理战斗消息 | 客户端实际连接数 |
|---|---|---|---|
| C++ 裸跑(未设环境变量) | 关,即旧模式(`gate_router_mode.h:34-54`) | 按 `BindBattleEvent` 建立的绑定,经 gRPC 中继给 battle | 直连成功时 2 条;直连失败时回落 gate 中继,1 条 |
| 本地 `start_game.ps1` | `'1'`,即路由模式(`start_game.ps1:20`) | 直接回 `kServiceUnavailable`(`client_message_processor.cpp:812-821`);路由服同样拒绝(`forwardlogic.go:133-137`) | 战斗期间 2 条,而且只能走直连 |
| K8s `k8s_deploy.ps1` | `"0"`,即旧模式(`k8s_deploy.ps1:153`) | 中继 | 【推】battle 对外通告的是 POD_IP(`k8s_deploy.ps1:4027-4028`),集群外连不上;直连失败后回落 gate 中继,实际只有 1 条 |

### 21.2 D37 收缩清单:一项都没删

battle-transport-decision.md §6 和 D37 要删的东西全部还在,也都参与编译,只是被开关隔开了:

- `cpp/nodes/gate/handler/event/battle_binding_helper.cpp`(Bind / Unbind / `ClearBattleRecord`),以及 `gate_event_handler.cpp` 对 Bind/UnbindBattleEvent 的订阅;
- gate 出站白名单里旧模式分支的 `BattleNodeService`(`gate/main.cpp:214-217`);
- `HandleGrpcNodeMessage` 对 Battle 的「必须先绑定」规则;
- `ClearBattleRecord` 的两个调用点:断线时一处,`OnNodeRemove` 的 battle 分支一处;
- battle 侧的 `BattleClientPlayerGrpcImpl`(读 `x-session-detail-bin`),以及 gate 端按应答类型反查消息号的桥接;
- scene 换会话时经 Kafka 重发 `BindBattleEvent`(`player_battle.cpp`)。

删码门槛仍是 D36/D37:「K8s 上以路由模式跑通 battle-smoke」。PROGRESS 里没有通过记录。

### 21.3 D39 没落码,由此出现的不对称

- `BattleRoomManager::PushToPlayer`(`battle_room_manager.cpp:1267-1289`)对**所有**消息号都是「有直连就直发,否则经 Kafka → gate 回落」,没有按 D39 收窄。
- 路由模式下,没建成直连的玩家处于不对称状态:
  - 下行战斗帧仍能经 gate 收到;
  - 上行出手被 gate 拒绝,每回合只能等超时后默认普攻。
- 以下两条在路由模式下都不成立,只在旧模式下成立,结果同上:
  - D26「签不出票只记 WARN,该玩家全程走 gate 中继」;
  - §18.2 的「回落」条款。
- D26 要不要改成 fail-closed(签不出票就拒绝开局),待决策。

### 21.4 Unity 客户端(只依据本仓文档)

- 客户端提交 `b5cf6ef` 已接入 `BattleDirectLink` 和 `DirectRoutingBattleTransport`。09-17 的 EditMode 回归里,BattleDirectLink 有 38 条通过(PROGRESS)。
- Unity 实机连真 battle 的端到端直连**没跑过**(§19.3 第 4 步)。09-03 的 Unity 双播放器实测早于直连落码,当时战斗走的是 gate 中继。
- 真正端到端验证过直连的只有 robot:09-05 跑了两轮 `BATTLE_SMOKE_OK`,`a_direct_turns == 总回合数`。

### 21.5 调研中发现的风险(待核实或待决策)

1. **停机路径可能丢终局包。**
   - 顺序:`SetBeforeShutdown` 先调 `AbortAllRooms`,把房间直连的 shutdown 推迟一轮;紧接着同步调 `DisconnectAll`,把仍处于 `connected()` 的房间直连 `forceClose`;排队的延迟 shutdown 随后被跳过。
   - 后果:终局包如果还积压在用户态输出缓冲里,可能丢失。
   - `DisconnectAll` 的注释「这些连接 connected()==false」已经过时(`battle/main.cpp:224-230`)。
2. **补签依赖观战索引。**
   - `spectate:battle:{id}` 在 CreateBattle 之后才写,写失败只打日志,理由是「只影响可观战」。
   - 但 `RequestBattleTicket` 也靠这个索引定位节点。索引缺失时,补签会回「战斗不存在或已结束」(`go/match/internal/logic/spectate.go`、`requestbattleticketlogic.go`)。
3. **§11.7 回调绑定。**
   - `BattleClientEdge::Install` 往 `TcpServer` 的回调里绑的是裸 `this`,edge 比 TcpServer 活得长只靠 node_entry 里的声明顺序保证。
   - `BattleRoomManager::edge_` 是裸指针。事故审计建议在 before-shutdown 里调 `SetClientEdge(nullptr)`,目前没做。
4. **两个冒烟场景的战斗步骤可能被拒。**
   - `team-smoke` 要求路由模式,但它的 `SetAutoBattle` 故意经 gate 发;`features-smoke` 的战斗也走 gate 连接。
   - 按路由模式的规则,这些上行会被拒。PROGRESS 里也找不到 `TEAM_SMOKE_OK` / `FEATURES_BATTLE_OK` 的通过记录。
5. **路由模式下,战斗中的路由自愈不会触发。**
   - `RefreshRoutingFromSession` 只采信带 `gate_instance_id` 的会话身份,而直连面合成的 `SessionDetails` 不带这个字段。
   - 战斗中换 gate 之后,Kafka 回落推送会投到旧会话,恢复只能靠客户端补签后重连。D38 认可这种做法。
6. **本地端口冲突。** scene 和 battle 共用端口基址 20000,分配器的重试路径会以 fail-closed 方式死循环,本地是给 battle 设 `RPC_PORT=20010` 绕过去的。

### 21.6 待用户决策

- K8s 上 battle 的集群外入口形态:hostPort / NodePort / 外部 L4 / Agones。
- D26 要不要改成 fail-closed。
- D39 要不要先于 D37 单独落地。
- §21.5 第 1、2 条要不要修。

## 22. 直连收缩一次做完(2026-09-29)

> 状态:**已落码;未编译、未测试、未上集群,待 Codex 验证(AGENTS §10.1)。** 用户 2026-09-29 拍板「一次全做,事后验」:
> 删 gate 战斗中继、D39 回落收窄、D26 改 fail-closed、删 Kafka Bind/Unbind 契约、robot 与 Unity 改为直连唯一,一批落完,
> 不再等 D36 / D37 的「K8s 路由模式 battle-smoke 先跑通」。
> 本节**取代** §18.1 D23 / D26、§18.2「回落」条款、§18.6 部署寻址、§18.7 收缩条、§18.8 第 5 步 `skip_direct_connect` 回落、
> §19 D36–D39、§19.3 第 2–3 步(各处已加标注,原文保留作判据链)。§21 是收缩前的现状快照,勘误见 §22.8。
> 编号:D51–D64 已被 [client-access-band-routing.md](./client-access-band-routing.md) 占用;本节 D65–D75;
> 集群外入口 D76–D93 见 [k8s-client-entry.md](./k8s-client-entry.md)。
> 口径:以本批落码契约为起点,**以磁盘代码为准**,实现偏离契约处在决策表里逐条写明。行号以 2026-09-29 工作树为准;
> 共用工作树有他人并发改动,行号可能漂移,以函数名为锚。

### 22.1 收缩后的连接形态

| 连接 | 何时存在 | 承载 |
|---|---|---|
| gate 长连接(大厅) | 登录后常驻 | Login / CreatePlayer / EnterGame;scene 的全部客户端消息;Go 服务:路由模式("1")经 `client_rpc_router`,直连模式("0")由 gate 直调 login / scene_manager / match(白名单 `{Scene, Login, SceneManager, Match}`,`gate/main.cpp:216-218`;chat / guild / friend / trade 仅路由模式可达)。补签 `MatchService.RequestBattleTicket` 与 `WatchBattle` 两种模式都能走通;battle 的大厅公告 `NotifyBattleAssigned` / `NotifyBattleStart`(无活直连时经 Kafka→gate 回落,D68);scene 自己下发的 `NotifyBattleReconnect` 与结算后的 `NotifyBattleEnd`。**不承载战斗上行**:四条战斗 RPC 经 gate 一律回 `kServiceUnavailable`(1003),两种路由模式相同(D66) |
| battle 直连 | 参战 / 观战期间,每局一条 | 握手应答;四条战斗 RPC(`SubmitBattleAction` / `GetBattleState` / `StopWatchBattle` / `SetAutoBattle`)及其应答;握手后的全部战斗帧(`NotifyTurnResult` / `NotifyBattleEnd` / `NotifySpectateState` / `NotifySpectateTurnResult` / `NotifySpectateEnd`);观众首帧 = 紧跟握手应答的 `NotifySpectateState`(D69)。**直连是战斗唯一通路**:战斗帧无活直连即丢弃,不回落 gate(D68) |
| HTTP 短请求(Java 网关) | 登录前后 | `GET /api/server-list`、可选 `POST /api/login`、`POST /api/assign-gate`(排队时轮询 `POST /api/queue-status`)、`POST /api/refresh-token`。这些接口**都不校验 access token**,access token 在 gate TCP 的 `Login` 上出示。不算长连接 |

- 平时 1 条长连接;参战或观战时 2 条。跨 zone 传送是**替换** gate 连接(先连新 gate 再关旧),连接数不增加。
- 直连建不起来 = 本局不能出手(服务端每回合超时替玩家默认出手),客户端显示「无法连接战斗服务器」横幅 + 重新连接(D74)。
  与 §19 D39 原理由同一口径:要求客户端能到达 battle 端口,如同 LoL / 王者要求 UDP 可达。
- 同一玩家同一时刻只保留一条 battle 直连:客户端单条 `BattleDirectLink`,battle 房间 `directConnByPlayer` 按 player_id 单槽;
  观战与参战互斥(D11)。

### 22.2 决策 D65–D75

| # | 决策 | 理由 |
|---|------|------|
| D65 | **收缩一次做完**:豁免 D36 / D37 / D-12 的前提①「K8s 上以路由模式跑通 battle-smoke」,事后补验 | 项目未上线、没有老客户端,expand→migrate→contract 的 migrate 窗口不存在;分两批删码要一直维持两条战斗通路,§21.3 的「下行能收、上行被拒」不对称就是这样来的。代价:K8s 路由模式 battle-smoke 待事后补验(谁跑、在哪个 kind namespace 待用户定);集群外入口落地前,K8s 集群外客户端既连不上 battle 也连不上 gate(§22.8 勘误第 3 行) |
| D66 | **gate 两种模式都不中继战斗**。目标为 `BattleNodeService` 的客户端消息在 `RpcClientSessionHandler::DispatchClientRpcMessage` 统一拒绝:位置在 `IsClientMessageId`、`ValidateClientMessage`(体积 + 限速)与 GM 闸之后、按协议分派之前;`LOG_DEBUG` + `SendTipToClient(conn, kServiceUnavailable)` + `return`,**不计非法包、不断连**(`client_message_processor.cpp:937-950`)。`HandleRouterForward` 里原有的 battle 拒绝删除(已不可达);出站白名单两种模式都不含 `BattleNodeService`(`gate/main.cpp:199` 起的注释与白名单);`HandleGrpcNodeMessage` 的 `requiresSessionBinding` 只剩 Scene。**保留**:`gate/main.cpp` 的 `sResponseTypeToMsgId` 反查桥;`gate_router_mode.h` 的函数与默认值(D-12,只改注释);`node_util.cpp` 的 `IsGrpcOnlyNodeType(BattleNodeService)` 逻辑(battle 以 `PROTOCOL_GRPC` 注册,TCP 端口专供客户端直连);`base_deploy_config.yaml` 的 `BattleNodeService.rpc` 发现前缀(battle 自身注册自检与劫持检测依赖它);生成器为 `BattleClientPlayer` 产出的 gate 侧 gRPC 客户端桩留作死代码(gate 仍 watch 该前缀,但 `service_discovery_manager.cpp:125-127` 在连接前按白名单返回,不建 channel) | 一处拦截同时覆盖直连模式与路由模式;这组号仍是合法协议号(同时是直连面与 Notify 下行的号,不能摘 `OptionIsClientProtocolService`),踢线会把大厅连接一起拆掉;反查桥是所有 gRPC 回包的通用出口,不是 battle 专属(§22.8 勘误第 6 行),删了会断 login / match / scene_manager 等回包;按服务过滤生成桩是另一件事(KISS,§22.6) |
| D67 | **删除 Kafka `BindBattleEvent` / `UnbindBattleEvent` 契约,事件号不复用**:从 `proto/contracts/kafka/gate_event.proto` 删掉两个 message(`:98-101` 留说明注释);event_id 生成器新增**自动墓碑**:`proto/event_id.txt` 里出现过、而 proto 里已不存在的事件改写为 `N=reserved:<原名>` 永久保留,分配新号时跳过全部墓碑号,墓碑不生成任何 C++ / Go 常量或分派 case(机制见 §22.3「proto 与生成器」) | proto 字段 `reserved N;` 在事件号上的对应物,满足 AGENTS §7.4「编号上线后不复用」;事件号是 Kafka 线上契约,滚动升级窗口里旧 scene 发来的 43 号不会被新 gate 误解析成别的事件 |
| D68 | **D39 落码:下行拆成两个显式出口**。`PushBattleFrame`:只走直连,无活直连即丢弃并打按 messageId 采样的 `LOG_INFO metric=battle_frame_dropped_no_direct`;用于 TurnResult / BattleEnd / SpectateState / SpectateTurnResult / SpectateEnd 及直连建立后的一切战斗帧。`PushLobbyAnnouncement`:有活直连直发,否则经 Kafka→gate 回落,**只**用于 `NotifyBattleAssigned` / `NotifyBattleStart`。判定抽成不依赖 protobuf 的纯函数 `battle_push_policy::Decide`(`cpp/nodes/battle/battle_push_policy.h`),配独立 gtest。`PushMessageViaGate` 保留,唯一调用方是 `PushLobbyAnnouncement`。scene 发起的 `NotifyBattleReconnect` 与 scene 结算后的 `NotifyBattleEnd` 是 scene 自己经大厅会话的下行,不归 D39 管;客户端两条连接都挂 `NotifyBattleEnd` 并按 battle_id 幂等 | 回落是白名单,不是默认:保留战斗帧回落等于维持第二条战斗通路;哪条消息属于哪一类写在调用点上,一眼可见;battle 没有 Prometheus 端点,丢弃计数靠日志 `metric=` 提取,采样是为了防止掉线玩家每回合一条把日志放大 |
| D69 | **观众快照随直连下发(snapshot on connect)**。契约写的是「`AttachDirectConnection` 成功且角色为 OBSERVER 时推当前 `SpectateStateS2C`」。**落码偏离一**:触发点不在 `AttachDirectConnection` 里,而是 `BattleClientEdge::OnTokenVerify` 在 `SendVerifyReply` **之后**调新方法 `BattleRoomManager::OnDirectConnectionVerified`(`battle_client_edge.cpp:351-361`、`battle_room_manager.cpp:1576-1593`),线上顺序固定为 `BattleTokenVerifyResponse → NotifySpectateState`;同一连接的重复握手(幂等分支)不再推。**落码偏离二**:`AddObserver` 原先的首帧推送只在**同会话幂等重试**这一条路径保留(`PushSpectateState` → `PushBattleFrame`,有活直连照常直发,否则丢弃,`battle_room_manager.cpp:840-848`);新观众路径与会话已变路径**不推首帧**(`:856-862`、`:888-890`)。参战者握手时不推快照,由客户端在直连就绪时 `GetBattleState` 补拉(robot 与 Unity 同口径) | 偏离一:`AttachDirectConnection` 在握手应答写出之前执行,而 robot 的 `VerifyBattleToken` 要求读到的第一个包是应答,快照抢先会被当成应答吞掉。偏离二:这两条路径结构上不可能有活直连(新观众没有 `directConnByPlayer` 条目;换会话路径刚 `CloseDirectConnectionOf`),照字面调 `PushBattleFrame` 必然丢弃,每次观战接入都给 `battle_frame_dropped_no_direct` 添一条预期内的丢弃,会淹没真信号。可观察行为与字面实现相同(首帧不送达,由握手快照补上),只少一条日志 |
| D70 | **D26 改 fail-closed**。`HandleCreateBattle` 在插表、装定时器、推送、发确认事件之前为全体参战者预签 `BuildAssignment`;任一失败 → `LOG_ERROR metric=battle_ticket_issue_failed … role=participant` + 回 `kServiceUnavailable`,不建房(`battle_room_manager.cpp:569-585`)。`HandleAddObserver`:新观众在登记前预签,失败不登记(`:873-883`,`path=new`);两条幂等路径先重签,失败 → 从 `routingByObserver` / `observerNames` 摘除、`CloseDirectConnectionOf`、回 `kServiceUnavailable`(`:823-838`,`path=idempotent_same_session` / `idempotent_session_changed`)。`PushAssignment` 只下发调用方预签好的 `BattleAssignedS2C`(`:1500-1507`)。`HandleIssueBattleTicket` 本就 fail-closed,不改。拒绝码复用 `kServiceUnavailable`(1003,已是 fault 码),tip 零新增。match 侧**零逻辑改动**(仅指收缩批范围;集群外入口批另改了 match 的 `gather.go` / `spectate.go` / `queue.go` / `watchbattlelogic.go`,见 k8s-client-entry D82 与 §22.5 第 4、5 条):通用补偿已覆盖(`DestroyBattle` 缺房幂等成功 → 解冻 / 回队首 / 删票;`WatchBattle` 收到任何非缺房 tip 即 DEL 观战标记),只补回归测试。与集群外入口的接缝:`BuildAssignment` 的地址改取 `client_endpoint::ClientFacing`(D76/D78,`:1452-1465`),拿不到客户端可达地址即签票失败,走同一条 fail-closed 路径。建房准入共五步(`battle_node.cpp:56-124` 的 `BattleNodeImpl::CreateBattle`):① 准入闸(gRPC 线程,`:68`)→ ② 分配许可(D82,gRPC 线程,`:83`)→ ③ loop 内复核准入闸(`:107`)→ ④ 预签 `BuildAssignment`(loop 内,`HandleCreateBattle`)→ ⑤ 插表。①–③ 任一道不过 → gRPC `UNAVAILABLE` + `battle_not_allocatable`,match 跳过 `DestroyBattle` 换节点重试一次;④ 失败 → gRPC OK + tip `kServiceUnavailable`(本决策),两类拒绝在 match 侧走不同分支 | 直连是唯一通路:签不出票的玩家连不上这局,放进来只会全程挂机被默认普攻;旧日志「玩家将全程走 gate 中继」在收缩后不成立。预签时房间还是局部对象,拒绝等于什么都没发生;观众侧「摘除 + 关直连」与 match 的回滚口径一致,避免出现「match 认为他已不在观战、房间里却还挂着他」 |
| D71 | **删除 `RefreshRoutingFromSession`**:直连合成的 `SessionDetails` 不带 `gate_instance_id`,该函数恒为空操作。`BattleRouting` 只在 CreateBattle 快照 / AddObserver 时写入;四个 `Handle*` 保持 `SessionDetails` 入参(D28 零改动) | 空操作函数会让人以为「路由能自愈」;收缩后的恢复通路是 scene 重连提示 → 客户端补签重建直连(D72)。代价:大厅公告的路由固定在快照时刻(§22.5 第 3 条) |
| D72 | **scene 换会话只推重连提示**:`RebindBattleOnReconnect` 改名 `NotifyBattleReconnectToClient`,函数体只剩 `try_get<InBattleComp>` → 组 `BattleReconnectS2C{battle_id}` → `SendMessageToClientViaGate` → 日志(`player_battle.cpp:1912-1932`);删掉 BindBattleEvent / GateCommand / Kafka / `gate_instance_id` 检查及专用 include,删死参数 `rebindGate`。两个调用点:`OnPlayerEnterScene` 第 2 步(RECONNECT 或 REPLACE 且有 `InBattleComp`,`:2008-2015`,D38 的条件不变);登录重建回调在 FIGHTING 时**总推**,不再要求 `battle_node_id != 0`(`:1536-1542`)。`InBattleComp.battle_node_id`、`PrepareBattleRequest.battle_node_id` 字段保留,只改注释(日志 / 排障用) | gate 已没有绑定可重建;客户端凭 battle_id 经 `MatchService.RequestBattleTicket` 补签,用不到节点号,ctx 缺失的降级重建同样要提示;删字段要动 proto 与持久化,留着零成本 |
| D73 | **robot:直连是战斗唯一通路**。删 `skip_direct_connect`;`battle_direct_conn.go` 拆成 `dialBattleDirect(account, playerId, assigned, stats, onMessage)` + 保留包装 `openBattleDirectConn`;battle-smoke 的 A 在 `WaitBattleStart` 后立即建直连(不等观战屏障),B 维持「等 Assigned(OBSERVER) → 直连 → 由握手快照拿首帧」;跨 zone 两侧无条件直连;team-smoke 每场按 battle_id 找 Assigned 后建直连、在直连上 `SetAutoBattle`,直连推送并入同一个 pushes 列表;features-smoke 战斗段改走直连(`callDirect`,与 `call` 共用 `callVia`)。落码补充:`dialBattleDirect` 在参战者握手成功后自动经直连发一条 `GetBattleState` 就绪补拉(D69 的参战者口径);握手超时分支改为异步 `Close` | robot 是唯一跑过端到端直连的参考客户端,必须与服务端同口径;A 若等观战屏障后才建直连,屏障期间的 TurnResult 会被 D68 丢弃;异步 `Close` 是因为黑洞地址下 vendored muduo 的 `Close` 要等 `net.Dial` 的 OS 超时(Windows 约 21s,Linux 约 127s),同步关会让 10s 预算失效 |
| D74 | **Unity 客户端**:新增窄接口 `IBattleChannel`(不扩 `IBattleTransport`);四条战斗 RPC 直连未就绪时本地快速失败(`NotReadyError = "link: not ready"`),绝不走大厅;删 `IsRetriableOnLobby`、大厅重发与 `RoutesDirect`;`SendOneWay` 遇战斗号未就绪即丢弃并记日志;`BattleDirectLink` 关闭原因结构化为 `BattleLinkCloseKind { Ended, HostClosed, BattleGone, Unreachable, Superseded }`(`Superseded` 是评审后追加的),补签错误回调带 tipId;新增 `Retry` / `EnsureBattle` / `Abandon`;`BattleClient` 直连就绪时 `GetBattleState` 补拉并补发自动战斗记忆,`SubmitAction` 返回 bool;`SpectateClient` 首帧由握手快照驱动(D69),`StopWatch` 未就绪时本地收敛 + `Abandon`;补签预算、自动重拉、UI 口径见 §22.3「Unity」。大厅断线仍拆直连(本批不改);提示文案是客户端本地字符串,`kServiceUnavailable` / `kInvalidParameter` 用已生成的 `CommonErrorTip`,客户端无需 regen | 窄接口让战斗分流层可替换、可单测,不污染大厅传输接口;大厅重发在收缩后必被 gate 回 1003,留着只会把「直连没好」伪装成「服务端拒绝」;结构化关闭原因让 UI 分得清「本局已结束」与「连不上」 |
| D75 | **K8s 默认路由模式**:`k8s_deploy.ps1 -GateRouterMode` 默认改 `"1"`(`k8s_deploy.ps1:164`);C++ 默认值与 `gate_security_test` 不改(D-12 修订,见 [xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md));`dev_tools.ps1` / `k8s_image.ps1` 增加 `-GateRouterMode`,留空即不覆盖、非空才透传;契约测试新增用例。收尾补丁:`k8s_zone_rollback.ps1` 增加同形参数,`-Apply` 且留空时在停服前读集群里 gate 当前的模式,读不到或与 `k8s_deploy.ps1` 默认值不一致即拒绝,要求显式传 0 / 1;`dev_tools.ps1 -Command k8s-zone-rollback` 已透传(`dev_tools.ps1:1603`) | gate 不再中继战斗后,"0" 模式也失去了战斗回退的意义;chat / friend / trade 只经路由服可达。回退态**不粘滞**:以 "0" 回退运行的 zone,每次重新部署(含灾备回滚、合服后的 zone-up)都必须显式再传 `-GateRouterMode 0`,否则静默落回 "1"。集群外部署须与 `-ClientEntryMode external` 在同一窗口启用(上线顺序见 [k8s-client-entry.md](./k8s-client-entry.md)) |

### 22.3 改动集

**gate**(`cpp/nodes/gate`)

- `handler/rpc/client_message_processor.cpp`:`DispatchClientRpcMessage` 统一拒绝战斗消息(`:937-950`,日志
  `拒绝经 gate 的战斗消息(战斗只走客户端直连)`,逐条 DEBUG);`HandleRouterForward` 删 battle 分支与不再使用的 `rpcHandlerMeta`;
  `HandleGrpcNodeMessage` 的 `requiresSessionBinding` 只剩 Scene;断线路径与 `OnNodeRemove` battle 分支里的 `ClearBattleRecord` 调用删除。
- 删除 `handler/event/battle_binding_helper.{h,cpp}`,`CMakeLists.txt` / `gate.vcxproj` / `gate.vcxproj.filters` 同步去登记。
- `handler/event/gate_event_handler.cpp` 是生成物:只清守护段(全局 include 与 `BindBattleEventHandler` / `UnbindBattleEventHandler` 里的委托)。
  regen 前这两个空 handler 仍按旧 pb 类型编译,regen 后随 proto 一起消失。`gate_kafka_command_router.cpp` 是生成物,不手改。
- `main.cpp`:出站白名单两种模式都不含 `BattleNodeService`,启动日志 `出站白名单=` 可直接验收;`sResponseTypeToMsgId` 反查桥不动。
- 只改注释:`gate_router_mode.h`、`handler/event/scene_route_helper.h`、`handler/event/scene_entry_dispatch.h`。
- 引擎公共注释(逻辑一字不改):`node_util.cpp` / `node_util.h`(battle 的 TCP 端口挂客户端直连面 `BattleClientEdge`,不是空闲占位、不可回收,
  没有任何 C++ 节点以 RpcCodec 拨它;节点间控制面只剩 Go match 经 `grpc_endpoint`);`node.cpp:530-532`(C++ 发现方 `node_connector`
  按 `protocol_type` 分派,battle 若标成 TCP 会让它们用 RpcCodec 拨 battle 的 TCP 端口;Go match 只认 `grpc_endpoint`)。

**battle**(`cpp/nodes/battle`)

- 新增 `battle_push_policy.h`:`battle_push_policy::Decide(PushCategory{kLobbyAnnouncement, kBattleFrame}, hasLiveDirect)`
  → `PushRoute{kDirect, kViaGate, kDrop}`,纯 `constexpr`;有活直连一律直发,无活直连时只有大厅公告回落(`:42-49`);
  在 `battle.vcxproj` / `.filters` 登记为 `ClInclude`。
- 新增 `tests/battle_push_policy_test.cpp`:6 条 gtest,**不进** vcxproj / CMakeLists,文件头写了独立编译命令。
- `logic/battle_room_manager.{h,cpp}`:
  - `PushToPlayer` 拆为 `PushBattleFrame`(`:1391-1408`)与 `PushLobbyAnnouncement`(`:1410-1430`);两条防御分支(战斗帧判出 `kViaGate`
    按丢弃处理、大厅公告判出 `kDrop` 打 `LOG_ERROR` 契约破坏)不可达。`PushMessageViaGate`(`:103-118`)只剩一个调用方。
    丢弃日志 `LogBattleFrameDroppedSampled`(`:120-136`):每个 messageId 首次必打,之后每 1024 次一行。
  - `HandleCreateBattle` 预签(`:569-585`);插表后逐人先 `PushAssignment` 再 `PushLobbyAnnouncement(NotifyBattleStart)`(`:611-626`)。
    `HandleAddObserver` 的三条签票路径(`:812-890`);`PushAssignment` 只下发(`:1500-1507`);新增 `OnDirectConnectionVerified`(`:1576-1593`)。
  - 改名 `NotifySpectateEndAndUnbind` → `NotifySpectateEndAndClose`(`:1297`)。删除 `SendBindBattle` / `SendUnbindBattle` / `SelfNodeId` /
    `RefreshRoutingFromSession` 及全部调用点。battle 仍依赖 `contracts_kafka_gate_event_event_id.h`(大厅公告回落要用 `PushToPlayerEvent` 的事件号)。
- `client/battle_client_edge.{h,cpp}`:`OnTokenVerify` 在 `SendVerifyReply` 之后调 `OnDirectConnectionVerified`(`:357-361`)。
- 删除 `handler/grpc/battle_client_player_service.{h,cpp}`(`BattleClientPlayerGrpcImpl`,即 gate 中继客户端消息在 battle 侧的入口),构建清单同步。
  `handler/grpc` 只剩 match→battle 控制面 `battle_node.{h,cpp}`;这些文件不是生成器输出,regen 不会让被删文件回来。
  `main.cpp` / `handler/event/event_handler.cpp` / `battle_security.h` 只改注释。
- 新日志 metric(battle 无 Prometheus 端点,由日志侧按 `metric=` 提取):
  - `metric=battle_frame_dropped_no_direct message_id=… dropped_total=… latest_battle_id=… latest_player_id=…`(`LOG_INFO`,按 messageId 采样);
  - `metric=battle_ticket_issue_failed battle_id=… player_id=… role=participant|observer`,观众另带
    `path=new|idempotent_same_session|idempotent_session_changed`(`LOG_ERROR`,逐条;正常运行应为 0)。

**scene**(`cpp/libs/services/scene`)

- `battle/system/player_battle.{h,cpp}`:`RebindBattleOnReconnect` → `NotifyBattleReconnectToClient`(D72),删 5 个只为 Bind 服务的 include
  (`RdKafka`、`contracts::`、`ResolveCommandRoute`、`NodeConfigManager` 等在文件内已无引用)与死参数 `rebindGate`;
  登录重建回调在 FIGHTING 时总推提示。日志:`[PlayerBattle] 战斗中换会话,已推重连提示`。
- `player/system/player_lifecycle.cpp`:只改第 6 步注释(已随 `ba8621a75` 提交,该文件当前没有未提交改动)。

**proto 与生成器**

- `proto/contracts/kafka/gate_event.proto`:删 `BindBattleEvent` / `UnbindBattleEvent`。`proto/battle/player_battle.proto`、
  `proto/battle/battle_node.proto`、`proto/common/component/battle_comp.proto`、`proto/scene/scene.proto`:只改注释
  (regen 后 `cpp/generated/proto/**`、`go/proto/**`、`generated/proto/**`、`robot/vendor/proto/**` 只有注释漂移)。
- **事件号墓碑机制**(D67,`tools/proto_generator/protogen/internal/generator/cpp/event_id.go`):
  - `proto/event_id.txt` 的行有两种:活事件 `N=<IdName>`,墓碑 `N=reserved:<原 IdName>`。前缀常量的唯一权威是
    `internal/message_id.go:26` 的 `ReservedEventIdPrefix`。
  - 事件从 proto 消失 → 旧号转墓碑、永久占位(日志 WARN `Event no longer in proto; its ID is now reserved and will never be reused`);
    分配新号跳过全部墓碑。**事件改名**按「旧名删除 + 新名新增」处理:旧号转墓碑,新名拿新号(改前旧号会进空洞池被复用)。
  - 墓碑不生成任何常量或 case:C++ 头文件与 `rpc_event_registry` 分派从 proto 描述符生成,天然跳过;Go 常量生成器
    `ConstantsGenerator.GenerateWithSuffix` 显式跳过墓碑行(`message_id.go:64`),否则 11 个 Go 服务的
    `generated/pb/game/event_id.go` 会出现 `const reserved:…` 这种非法标识符。`EventIdLen` 计入墓碑号,`kMaxEventCount` 不缩。
  - 解析闸:同一号出现两次(含「既是活号又是墓碑」)、活名重复 → Fatal;Kafka 事件目录读不到 → Fatal(原为 Warn 跳过)。
  - 批量闸 `checkEventIdInputComplete`(纯函数,`event_id.go:358`):`contracts_kafka` 配成空、而文件里仍有 `ContractsKafka*` 活事件 → Fatal;
    单轮新增墓碑 > `maxNewTombstonesPerRun = 4`(`event_id.go:37`)→ Fatal,除非进程环境显式设 `PROTOGEN_ALLOW_MASS_EVENT_TOMBSTONE=1`
    (只认 `1`)。Fatal 在写 `event_id.txt` 之前,不会落墓碑;但流水线里更早的并行组可能已写出部分生成物,中止后先查工作树再重跑。
  - **纪律**:墓碑行随生成物一起提交,**永不手删**;提交 `event_id.txt` 前逐行审 diff 与 WARN。本批预期恰好两行:
    `43=reserved:ContractsKafkaBindBattleEvent`、`44=reserved:ContractsKafkaUnbindBattleEvent`,文件仍 49 行(0..48)。
    放行变量一旦设置,任意数量的墓碑都会通过。
  - 新增单测:`internal/generator/cpp/event_id_test.go`(`TestParseEventIdLine`、`TestEventIdFileStateRejectsDuplicates`、
    `TestAssignEventIds*`、`TestRenderEventIdFileWritesTombstonesBackVerbatim`、`TestCheckEventIdInputComplete*`、
    `TestEventIdLenCountsTombstonedIds`);`internal/message_id_test.go`(`TestGenerateWithSuffixSkipsReservedEventIds`)。
- 生成器为 `BattleClientPlayer` 产出的 gate 侧 gRPC 桩(`player_battle_grpc_client.*`、`grpc_init_client` 的 battle 分支、
  `rpc_event_registry` 的 sender)保留为死代码,不改生成器过滤(§22.6)。

**match**(`go/match`)

- 收缩批范围内**零逻辑改动**(D70 由通用补偿覆盖)。同一工作树里集群外入口批另有 match 逻辑改动(建房前写观战记录、
  准入拒绝换节点重试一次、`outcome=not_allocatable` / `index_failed`、matched TTL 公式、WatchBattle 建房窗口不剔除),
  见 k8s-client-entry D82 与 §22.5 第 4、5 条;不要把当前 match 当成收缩前的行为来读。收缩批新增回归:`internal/logic/gather_create_reject_test.go`
  (`TestGatherCreateBattleServiceUnavailableRequeuesAllMembers`、`TestRunTeamGatherCreateBattleServiceUnavailableDeletesAllTickets`、
  `TestRunChallengeGatherCreateBattleServiceUnavailableUnfreezesBoth`);`spectate_test.go` 的 `TestWatchBattleServiceUnavailableRollsBackMark`
  (「新观众」「同场重看走幂等重推」两个子用例,断言 1003 拒绝后 match 不补发 `RemoveObserver`,依赖 D70 的房间侧摘除);
  fixture 新增 `createTip` / `createRejected`(被拒请求不进 `created`,原断言语义不变)。
- 只改注释:`gather.go` / `spectate.go` / `watchbattlelogic.go` / `requestbattleticketlogic.go` / `internal/constants/errors.go`。
  口径:同一玩家同一时刻只保留一条 battle 直连(客户端单条链路,battle 房间 `directConnByPlayer` 按 player_id 单槽),观战与参战互斥;
  `ErrSpectateOffline` = 观众尚无直连时 `NotifyBattleAssigned` 要经 gate 回落,不在线就没有可路由的会话;
  持票断线可凭原票重连(D25),丢票或同票重试失败后,补签是回到本局的唯一通路,依赖观战索引;
  battle 明确拒绝 CreateBattle 时,`DestroyBattle` 送达则照常解冻 / 回队首 / 删票,`DestroyBattle` 本身失败仍走保守分支(§22.5 第 5 条)。

**robot**

- `battle_direct_conn.go`:`dialBattleDirect` / `openBattleDirectConn`(D73);参战者握手成功后自动经直连 `GetBattleState`;
  每条直连按类计数(`state_replies` / `turn_results` / `battle_ends` / `spectate_states` 等);`battleDirectConnTimeout = 10s`,
  包装层等 Assigned 与 `dialBattleDirect` 内建连 + 握手各一份 10s 预算。
- `battle_smoke_scenario.go`:A 开战即直连;B 先 `WaitBattleAssigned`(预算 `battleSmokeSpectateTimeout` = 15s)、校验 role 与 battle_id,
  再直连并断言首帧来自握手快照。`battle_smoke_cross_zone_scenario.go`:两侧无条件直连。
- `team_smoke_scenario.go`:`finishBattle` 拆成 `connectBattle(since, battleId)` + `finishBattleOn(direct, since, battleId)`;
  S8 在 `startSoloBattle` 返回后**立即**建直连,再做 A 的开战被拒断言(`TeamMemberInBattle`),最后在直连上 `SetAutoBattle` 打到终局
  (与 battle-smoke A「开战即直连」同口径;评审发现原顺序最长 20s 的断言重试会让 6s 一回合的 PVE_SOLO 在拨号前打完)。
- `features_battle_smoke.go` / `features_smoke_scenario.go`:战斗段走 `callDirect`;`config/config.go`、`etc/battle_smoke.yaml`、
  `etc/team_smoke.yaml`:删开关与旧注释;`logic/handler/player_feature_battle_response.go`(信封拒绝文案改为 envelope rejected)、
  `logic/handler/battle_client_player_notify_battle_assigned.go`。
- 断言口径:直连计数恒 ≥1,不再有 `a_direct_turns=-1` 这类回落输出;A 与跨 zone 两侧的直连须 `state_replies>=1 && turn_results>=1 && battle_ends>=1`;
  B 的直连 `spectate_states>=1`。
- 新失败步骤名(输出为 `BATTLE_SMOKE_FAIL step=… reason=…` / `TEAM_SMOKE_FAIL step=…`):
  - `b-wait-battle-assigned`:B 在 15s 内没收到观战票据(`NotifyBattleAssigned`);
  - `b-assigned-role`:观众票据 role ≠ `BATTLE_TICKET_ROLE_OBSERVER`;
  - `b-assigned-battle-id`:观众票据 battle_id ≠ A 的对局;
  - `b-direct-snapshot`:首帧 `NotifySpectateState` 不是经直连到达(D69 的机器校验);
  - `b-spectate-battle-id`:**语义收窄**为握手快照的 battle_id 不符(`b-wait-spectate-state` 相应改为等握手快照);
  - `S8-b-solo-connect`:team-smoke 里 B 单人开战后按 battle_id 找参战 Assigned 或建直连失败。
- 未覆盖:`logic/handler/battle_client_player_notify_battle_reconnect.go` 仍是空函数,D72「重连提示 → 补签 → 重建直连」不在 robot 冒烟范围内。

**Unity 客户端**(独立仓 `../mmorpg-client`,本批已授权修改。以下依据该仓工作树与实现记录;改动一部分被他人会话的
`f86a3b6` / `727450d` 顺带提交,其余仍未提交,审阅时读整文件)

- 分流层:`Assets/Scripts/Game/Battle/IBattleChannel.cs`(新,窄接口)、`DirectRoutingBattleTransport.cs`(未就绪快速失败、`SendOneWay` 丢弃并记日志;
  Battle 与 Spectate 共用一个实例)、`GameClient.cs` 接线。
- `Assets/Scripts/Net/BattleDirectLink.cs`:
  - `BattleLinkCloseKind { Ended, HostClosed, BattleGone, Unreachable, Superseded }`。`Superseded` = 链路改去服务另一局而旧局未终结
    (旧局从此收不到战斗帧);`SpectateClient` 对自己那局收到 `Superseded` 即收敛回 None,`BattleClient` 忽略它(不是权威终局信号)。
  - 建连期限 `ConnectTimeoutSeconds = 10`,调用超时 `CallTimeoutSeconds = 15`;同票重连最多 1 次(`MaxSameTicketRetries`,§18.2),等待 1s。
  - **补签预算**:每局 `MaxReissuesPerBattle = 3`。宿主入口(`HandleReconnectHint` / `Retry` / `EnsureBattle`)重置预算并**立即**补签,算第 1 次;
    自动恢复路径(握手被拒、同票重连用尽)第 k 次补签前等 `1s × 2^(k-1) × (1 ± 20%)`,即 1s / 2s / 4s。
    退避以票据 `expire_at_ms` 的剩余时间封顶(仅剩余 > 0 时);票据已过期时,每轮预算只允许**一次**立即补签,之后照常退避——
    不凭本地时钟判 `BattleGone`,防止客户端时钟偏快把进行中的战斗误判收场。补签结果分四类(`BattleDirectLink.cs` 的 `SendReissue`,`:736-790`):
    补签回包为空或 battle_id 不符 → **立即** `Unreachable`(`reissue_mismatch`),不退避、不消耗剩余预算;
    回包缺 host / port → 按一次补签失败走退避(`HandleReissueFailed(0, "incomplete assignment")`);
    补签通道不可用(`TicketReissuer` 返回 false,如大厅未就绪)→ 按一次失败计;没有 `TicketReissuer`(或 battle_id 为 0)→ 直接 `Unreachable`。
    按失败计的各类在预算用完后 → `Unreachable`。
  - **只有**补签回 `common_error.kInvalidParameter` 才判 `BattleGone`;`kServiceUnavailable` 等其余失败继续退避。
  - 未就绪错误串 `NotReadyError = "link: not ready"`(前缀 `TransportErrorPrefix = "link: "`)。
- `Assets/Scripts/Game/Battle/BattleClient.cs`:
  - 直连就绪 → `GetBattleState` 补拉(同局在途去重,迟到结果作废)+ 补发自动战斗记忆。
  - **自动重拉**:补拉以传输错误失败、而同一局直连仍就绪时(如链路 Verified 时的 `link: rpc timeout`),按 `MaxStatePullRetries = 3`、
    `1s / 2s / 4s ±20%` 由 `Tick` 退避重拉;换局 / 收尾 / 断线 / 下次就绪 / 显式补拉 / 补拉成功都作废计划并把预算归零。
    预算用完由主城入口「返回战斗」或下一次就绪兜底。服务端拒绝(非传输错误)照常 `OnError`,不重拉。
  - 公开 API:`IsBattleChannelReady` / `IsBattleChannelFailed` / `OnBattleChannelReady` / `OnBattleChannelFailed` / `RetryBattleChannel()` /
    `HasActiveBattle`;`SubmitAction` 返回 bool,未就绪时按是否已判失败提示「连接中」或「连不上」。文案常量
    `BattleChannelConnectingText`「正在连接战斗服务器,请稍候…」、`BattleChannelFailedText`「无法连接战斗服务器,本局将由系统自动出手,可点击重新连接」。
  - tip 映射:`SubmitBattleAction` 的 `kInvalidParameter` 显示「行动无效」(引擎对 PVP 逃跑、未拥有的技能、道具限次等普通非法行动也回这个码);
    只有 `SetAutoBattle` 把 `kInvalidParameter` 映射为「战斗不存在或已结束」;`kServiceUnavailable` 全局映射。
  - 已按权威信号收场的战斗仍接受晚到的 scene 结算 `NotifyBattleEnd`(两条连接按 battle_id 幂等),UI 可能在战斗屏收起后再收到一次 `OnBattleEnd`。
- `SpectateClient.cs`:首帧由握手快照驱动;有通道时首帧超时也通知服务端退出;直连未就绪时 `StopWatch` 本地收敛 + `Abandon`,
  若分配包晚到、直连随后就绪,在 Ready(OBSERVER) 时补发一次 `StopWatchBattle`。
- UI(`Assets/Scripts/UI/Ugui/Battle/`):
  - 战斗屏横幅:「正在连接战斗服务器…」有 1.5s 宽限期(`BattleHudLogic.ChannelConnectingBannerDelaySeconds`,手动重连跳过宽限);
    判定连不上时显示「无法连接战斗服务器」横幅 + 「重新连接」按钮(调 `RetryBattleChannel`);横幅矩形避开 toast 带与左列;
    出手按钮按就绪置灰;连接状态每帧从 `BattleClient` 轮询(`IBattleChannel` 没有「掉线恢复中」事件);
    补拉到新状态时 `BattleHudLogic.NeedsScreenResync` → `BattleScreen.ApplyState` 重刷已打开的战斗屏(回合号变化时复位已提交态并退出目标选择)。
  - **主城入口四档**(`BattleUiRoot.DecideEntryMode`,public static 纯函数,只由 `BattleClient` 实时状态推出,UI 不另存标记):
    1. 「战斗」:战斗屏已开,或没有进行中的对局;点了开排队面板。只有这一档显示观战入口。
    2. 「重新连接战斗」(可点):`IsBattleChannelFailed`;点了 `RetryBattleChannel`,回到「连接战斗中…」。
    3. 「连接战斗中…」(不可点):`HasActiveBattle && !IsBattleChannelReady`,覆盖大厅重连后到首次就绪 / 失败之间,以及从入口发起的重连。
    4. 「返回战斗」(可点):有对局、直连已就绪而相位仍是 None(就绪补拉在途,或自动重拉预算已用完);点了 `RequestState`(同局在途去重)。
  - `App/DevAutoPilot.cs`:订阅 `OnBattleChannelFailed` 立即 `Fail("battle_direct", …)`;`BattleEnd` 时断言 `direct_turns == turns`。
  - `tools/run_crosszone_pair.ps1`:要求每侧 `direct_turns == turns`(比 robot 的 ≥1 严),缺 `direct_turns` 字段视为播放器过旧、判失败。
    `tools/gen_proto.ps1` / `gen_messageids.ps1`:服务端仓默认路径改为同级 `../mmorpg`。README 与 `.gitignore` 同步到同级仓口径。

### 22.4 新增不变量(并入 §7 语义,接 §18.5 编号)

14. 战斗上行只从 battle 直连进;gate 对 `BattleNodeService` 的客户端消息一律回 `kServiceUnavailable`,不中继、不绑定,两种路由模式相同。
15. battle 下行按类别路由:战斗帧只走直连,无活直连即丢弃;无活直连时经 Kafka→gate 回落的只有大厅公告
    `NotifyBattleAssigned` / `NotifyBattleStart`。回落是白名单,新增类别默认丢弃(`battle_push_policy::Decide`)。
16. 签不出票 = 不建房 / 不登记新观众 / 摘除已登记观众并关其直连,回 `kServiceUnavailable`;拒绝路径不得有任何副作用(插表、定时器、推送、确认事件)。
17. 观众首帧只随直连握手下发,且必须排在握手应答之后(客户端握手读到的第一个包必须是 `BattleTokenVerifyResponse`)。
18. `proto/event_id.txt` 的 `N=reserved:<原名>` 墓碑行永久保留、不得手删;事件删除或改名都烧掉旧号;
    单轮新增墓碑 > 4 必须显式 `PROTOGEN_ALLOW_MASS_EVENT_TOMBSTONE=1` 并逐行审 WARN。

### 22.5 风险

1. **K8s 未实跑(D65 豁免)**:K8s 上从没以路由模式跑过含 gate 的链路。默认翻 "1" 后 gate 硬依赖 infra 里的 client-rpc-router:
   `infra-up -SkipGoSvc`、没给 `-GoSvcRegistry`、只对旧 infra 跑 zone-up、以及 `k8s_image.ps1` 发布路径(从不部署 Go 服务)
   都会让 gate 卡在依赖门,登录与匹配全部 no_target;目前只有注释说明,没有运行时拦截。集群外入口(D76–D93)落地前,
   集群外客户端不能登录也不能战斗。
2. **不向后兼容**:没有直连的客户端收不到 battle 的 TurnResult / BattleEnd / SpectateState / SpectateEnd;参战者只在 scene 结算后
   经大厅收到一份 BattleEnd;被 match 清退且没有直连的观众收不到 SpectateEnd,只能靠自身超时或 FIN 收敛。
   滚动升级窗口里旧 scene 还在发 `GateCommand(event_id=43)` 时,新 gate 每条打一行 WARN(不会误解析);旧 gate 配新 battle / scene 则
   完全没有 BindBattle。建议 gate / scene / battle 同批停换。
3. **大厅公告路由固定在快照时刻(D71)**:玩家在开房到收到 Assigned / Start 之间换 gate,这两个包会投到旧会话;恢复只能靠
   scene 重连提示 → 补签,而补签依赖观战索引(第 4 条)。PREPARING 期间换会话同样会推重连提示,此时房间未建成,补签回
   `kInvalidParameter`,客户端按 `BattleGone` 收敛;若开局包随后投到旧会话,这一局就拿不到票据(改造前同样存在的缺口)。
4. **补签依赖观战索引(§21.5 第 2 条;收缩批不做,已由集群外入口 2b 第 9 条修掉失败路径)**:
   - 原风险「SETEX 写失败 → 开了局却补不了签」**已不存在**。match 现在在 CreateBattle **之前**写 `spectate:battle:{id}`
     (`gather.go:308-321`;`writeSpectateRecord`,`spectate.go:120-145`):有界重试共 2 次,每次 ctx 截止 1s(go-redis 读超时下实际上限 3s),
     退避 100ms,最坏约 6.1s(`spectate.go:61-82`);仍写不进去就**不建房**,干净失败并记 `gather_total{outcome=index_failed}`。
     D82 换节点前先改写一次(`gather.go:339-343`,失败同样 `index_failed`);开局成功后 `publishSpectateBattle` best-effort 同值补写并 ZADD(`gather.go:389`)。
     CreateBattle 与 DestroyBattle 都失败时记录保留(房间可能活着,补签靠它)。建房窗口里按 battle_id 观战不会懒剔除记录
     (`watchbattlelogic.go` 的 `roomMayBeCreating`)。口径与 [k8s-client-entry.md](./k8s-client-entry.md) D82「建房前写(fail-closed)」一致。
   - 残留风险:补签仍依赖这条记录(TTL = 最长战斗时长 + 60s,`spectateTTLSeconds`);持票断线可凭原票重连,不依赖记录。
     客户端把 `kInvalidParameter` 当作唯一的 BattleGone 信号,match 若在别的失败上误回这个码,客户端会把进行中的战斗收敛回 None。
   - 以上 match 改动未编译、未测试,待 Codex 验证。
5. **match 侧**:`createBattle` 不区分 RPC 失败与 tip≠0 明确拒绝,明确拒绝后若 `DestroyBattle` 也失败,会走 `create_failed_room_alive`
   保守分支(不解冻,冻结等 scene 的 `prepare_deadline_ms` 清掉、票据等 matched TTL 自愈);`PickRandom` 不排除坏节点,签票失败的节点
   可能被反复选中,造成 Prepare / Cancel 空转;`gather_total{outcome=create_failed}` 分不出两类。`WatchBattle` 对 battle 的 1003
   一律回 `ErrBattleNotWatchable`,客户端分不出「战斗服暂不可用」。
   **(2026-09-29 补充)** 节点级准入拒绝(gRPC `UNAVAILABLE` + `battle_not_allocatable`)已由集群外入口 D82 单独处理:跳过 `DestroyBattle`,
   用 `PickRandomExcept` 排除已试节点、换节点重试**一次**(`gather.go:324-357`、`discovery/node_watcher.go:317`),仍被拒记
   `outcome=not_allocatable`(`metrics.go:43`)。上面「不排除坏节点、记 `create_failed`」只对 tip≠0 的明确拒绝(如 D70 签票失败回 1003)
   和 RPC 失败仍然成立。未编译、未测试,待 Codex 验证。
6. **停机路径未改(§21.5 第 1 条)**:`AbortAllRooms` 之后 `DisconnectAll` 立即 forceClose 房间直连;没有 gate 兜底后,
   停机时积压在用户态缓冲里的观众 `SpectateEnd` 丢了就无法补回。
7. **D70 没有 C++ 单测**:CreateBattle 预签拒绝与 AddObserver 三条签票失败路径只由代码评审、match 回归测试与冒烟覆盖。
   幂等路径签票失败时直接关观众直连,不先推 `SpectateEnd(REMOVED)`,客户端靠 FIN 收敛。
8. **D69 依赖客户端不吞第二个包**:已核 robot 的 `VerifyBattleToken` 只读一个包,随后 `RecvLoop` 从同一连接接着读;
   若某客户端的握手读法吞掉紧随的快照,观众就没有首帧(仍可 `GetBattleState` 恢复)。
9. **robot**:冒烟必须在 battle / gate / scene / proto 同批落地并全量重编后跑,旧 battle 二进制会让 B 按设计失败在 `b-direct-snapshot`;
   battle 客户端端口从 robot 所在机器不可达时,全部战斗冒烟失败在 `*-direct-connect`;team-smoke S8 若 A 的拒绝断言因过渡态
   重试太久,B 的 PVE_SOLO 可能先打完、`battle:lock` 释放,`S8-in-battle-reject` 失败(收缩前就有);muduo 客户端每条直连 Close 后
   泄漏 RecvLoop goroutine,握手超时路径另泄漏握手 goroutine(冒烟进程随即退出,可接受);终局包可能收两份(直连一份、scene 结算后
   大厅一份),各场景已按 battle_id 幂等。D72 的「重连提示 → 补签」链路 robot 不覆盖。
10. **Unity**:SYN 黑洞下从开局到判定 `Unreachable` 最长约 90s(4 张票,每张两次 10s 建连,中间含 1 次同票重连、间隔 1s;
    每张新票都重置同票重连次数,`BattleDirectLink.cs:257`、`ScheduleRecovery` `:621-632`;补签前退避 1 / 2 / 4s ±20%;合计 4×(10+1+10) + 1+2+4 ≈ 91s,另加补签往返),期间出手按钮置灰、
    服务端每回合替玩家默认出手;陈旧的观众分配包晚于参战分配包到达,会让链路切走本局(`BattleClient` 忽略 `Superseded`,显示「连接中」
    直到回合超时或终局;Kafka 按 player_id 保序,极少见);大厅断线仍拆直连,战斗中大厅抖动等同直连中断;
    「已提交 → 断直连 → 补拉到新回合 → 按钮复位」的 UI 接线只有人工验证覆盖(EditMode 下 `Object.Destroy` 会报错);
    新增的两个 `.meta` 是手写的,以 Unity 导入结果为准。
11. **生成与共用工作树**:proto-gen-run 会一并带入他人未提交的 guild / config / data_service 等 proto 改动,并重写有他人未提交修改的
    `rpc_event_registry.{h,cpp}`、`grpc_init_client.cpp`,须与相关会话约定时段;`enable_unity_client: true` 会改写客户端仓生成物。
    墓碑机制不可逆:proto 输入不完整时运行生成器、又绕过批量闸,会永久烧掉大量事件号。
12. **死配置**:`bin/etc/base_deploy_config.yaml` 的 `GrpcClient.CallDeadlineMs.BattleNodeService: 5000`(注释仍写「gate 直连模式」)已无 C++ 调用方,
    待删或改注释(该文件有他人未提交改动,只许精确 Edit)。

### 22.6 明确不做 / 后续

- 停机路径 `DisconnectAll` 立即 forceClose 房间直连(§21.5 第 1 条):简单去掉 forceClose 会让连接停在 kDisconnecting,析构时撞
  `TcpConnection.cc:71` 的 assert(09-13 gate 事故同款,AGENTS §11.7);要保住终局包,必须同时保证 quit 前每条连接都到 kDisconnected,另行设计。
- 补签依赖观战索引(§21.5 第 2 条):另起任务做独立的 `battle_id → 落点` 路由键。**(2026-09-29 更正)** 「开了局却补不了签」的失败路径
  已由集群外入口 2b 第 9 条修掉(建房前写、fail-closed,见 §22.5 第 4 条);独立路由键降为可选后续。
- D40 `SubmitBattleAction` 回合号幂等(收缩后直连抖动丢一手 = 该回合默认普攻)。
- 生成器按服务过滤 gate 侧 gRPC 客户端桩。
- match:`createBattle` 区分明确拒绝与 RPC 失败(明确拒绝时跳过 `DestroyBattle`,新增 `outcome=create_rejected`);`WatchBattle` 是否透传 battle 的 1003。
- robot 覆盖 D72 链路(`NotifyBattleReconnect` → 补签 → 重建直连);Unity 战斗屏级 EditMode / PlayMode 用例(见第 10 条风险)。
- `k8s_deploy.ps1` 在 `GateRouterMode=1` 且 `-SkipGoSvc` / 无 `-GoSvcRegistry` 时告警或 fail-fast;`k8s_image.ps1` 发布路径不部署 Go 服务的存量缺口。
- 集群外入口 D76–D93 及其登记为独立任务的后续项,见 [k8s-client-entry.md](./k8s-client-entry.md)。
- 本节不改 AGENTS.md:§4.1 的 proto 重生命令写法已过时(见 §22.7 第 3 步),改它需用户确认。

### 22.7 Codex 验证清单(按依赖顺序;任一步红即停)

> 全部**未执行**:Claude 未编译、未测试(AGENTS §10.1)。本清单合并收缩批各工作包的验证命令;集群外入口(D76–D93)的
> kind 端到端验证见 [k8s-client-entry.md](./k8s-client-entry.md),与本清单共用第 3 步 proto 重生。
> 工作目录默认仓库根 `D:\luyuan\wuxingqitan\mmorpg`。Go 模块下载失败时进程级设 `$env:GOPROXY='https://goproxy.cn,direct'` 重试一次。
> 任一步失败:保留 battle / gate / scene 日志、robot 的 `*_FAIL step=…` 行、Unity 的 results.xml 与 unity.log 首个错误,不重试、不改判据。

0. **静态核对(随时可跑)**:
   `rg -n "PushToPlayer\(|SendBindBattle|SendUnbindBattle|SelfNodeId|RefreshRoutingFromSession|NotifySpectateEndAndUnbind|RebindBattleOnReconnect|rebindGate|ContractsKafka(Bind|Unbind)BattleEvent" cpp/nodes/battle cpp/libs/services/scene`
   → 0 命中;`rg -n "battle_binding_helper|ClearBattleRecord" cpp/nodes/gate` → 0 命中;
   `rg -n "skip_direct_connect" robot -g '!vendor/**'` → 只剩 `config/config.go` 的说明注释。
1. **event_id 生成器单测**(`tools/proto_generator/protogen`):`go vet ./internal/generator/cpp/`;
   `go test ./internal/generator/cpp/ -count=1 -v -run "EventId|ParseEventIdLine|AssignEventIds|RenderEventIdFile|CheckEventIdInputComplete"`。
   通过:vet 无输出,`TestAssignEventIds*`、`TestCheckEventIdInputComplete*`、`TestEventIdLenCountsTombstonedIds` 等全 PASS。
2. **message_id(Go 常量)生成器单测**(同目录):`go vet ./internal/`;
   `go test ./internal/ -count=1 -v -run GenerateWithSuffixSkipsReservedEventIds`;再 `go test ./internal/... ./cmd/... -count=1`。
   通过:全绿、无 import cycle。**第 1、2 步未绿不得进入第 3 步**(否则 regen 会给 11 个 Go 服务写入非法标识符)。
3. **proto 重生**。前置:第 1–2 步绿;gate / battle / scene 收缩改动全部落盘;与有未提交 proto 改动的会话约定时段;
   `PROTOGEN_ALLOW_MASS_EVENT_TOMBSTONE` **不设**。注意**不是** `cd go && build.bat`(它只对 db / login 跑 goctl,不产出 pb;末尾还有 `pause`)。
   1. `$env:PATH = "$PWD\third_party\grpc\install_vs2026_dbg\bin;$env:PATH"`,`protoc --version` 必须是 `libprotoc 35.1`
      (C++ 生成物断言 `PROTOBUF_VERSION == 7035001`);`protoc-gen-go` / `protoc-gen-go-grpc` 要在 PATH 上且与现有产物头部一致
      (v1.36.10 / v1.6.0),不一致即停下报告。
   2. 客户端门禁:`proto_gen.yaml` 设了 `enable_unity_client: true`,会改写 `../mmorpg-client` 的生成物。先确认
      `../mmorpg-client/tools/gen_proto.ps1` 收录了 `proto/team/team.proto`、`proto/trade/jubaozhai.proto`、`proto/friend/friend.proto` 三行;
      缺任何一行,就复制一份只把 `enable_unity_client` 改成 `false` 的配置副本,经 `-ConfigPath` 传入(原配置不改)。
   3. `pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`(失败即停;不可省,09-20 出过旧二进制吞守护段的事故)。
   4. `pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools/proto_generator/protogen/etc/proto_gen.yaml`
      (或上面的副本)。**不信退出码**(复制失败只打 Warn,脚本也不检查生成器退出码),先查输出里没有 Fatal / ERROR,再按 grep 验收:
      - 日志恰有两条 WARN `Event no longer in proto…`,event_id 为 43、44;若 Fatal `Event proto input looks incomplete; refusing to tombstone event IDs`,
        **不许**设放行变量,保留日志报告;
      - `Select-String proto/event_id.txt -Pattern '^(43|44)='` 恰为 `43=reserved:ContractsKafkaBindBattleEvent`、`44=reserved:ContractsKafkaUnbindBattleEvent`,
        文件仍 49 行(0..48),其余号不变;`cpp/generated/rpc/service_metadata/rpc_event_registry.h` 仍是 `kMaxEventCount = 49`;
      - `rg -n "reserved|BindBattle|UnbindBattle"` 在 `go/*/generated/pb/game/event_id.go`、
        `cpp/generated/rpc/service_metadata/contracts_kafka_gate_event_event_id.h`、`cpp/generated/rpc/service_metadata/rpc_event_registry.cpp`、
        `cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp`、`cpp/nodes/gate/handler/event/gate_event_handler.{h,cpp}` 里 0 命中;
      - 同批集群外入口的 proto 改动一起生成:`cpp/generated/proto/common/base/common.pb.h` 含 `client_endpoint`;
      - 再跑一次 proto-gen-run:没有新墓碑 WARN,`proto/event_id.txt` 没有新 diff(不动点)。
   5. robot vendor:`cd robot && go mod vendor`;拉不到私有模块时只把 `go/proto` 下本次涉及的 pb.go 复制进 `robot/vendor/proto`,
      不带入 protoc-gen-go 版本差异造成的噪音。
   6. 提交时 `proto/event_id.txt` 的墓碑行随生成物一起提交。
4. **Go 各模块**(第 3 步之后):
   - `cd go/proto && go build ./...`;11 个服务 `go/{battle,chat,client_rpc_router,data_service,friend,guild,login,match,scene_manager,team,trade}`
     各自 `go build ./...`,全部退出码 0(确认生成的 `event_id.go` 没有非法标识符)。
   - match(`go/match`):`gofmt -l ./internal/logic` 为空;`go vet ./internal/logic/... ./internal/constants/...`;
     `go test ./internal/logic/ -count=1 -v -run 'ServiceUnavailable|TestGather|TestRunTeamGather|TestRunChallengeGather|TestWatchBattle|TestStopWatching'`
     → 三条 `*CreateBattleServiceUnavailable*` 与 `TestWatchBattleServiceUnavailableRollsBackMark` 两个子用例 PASS,既有用例无回归;
     再 `go test ./... -count=1` 全绿。
   - robot(`robot/`,vendor 模式不需要网络):`gofmt -l`(vendor 除外;`config/config.go` 默认配置字面量里 `TradeSmoke:` 一行(约 :463)的对齐差异是既存的);
     `go build ./... && go vet ./...`;`go test -count=1 -run "TestDialBattleDirect|TestBattleDirectRecord|TestFeatureBattle" .`;
     `go test -count=1 -run TestFeatureBattle ./logic/handler/`;`go test -count=1 ./...`。`battle_direct_conn_test.go` 用 127.0.0.1 真 TCP,
     沙箱禁止监听时报的是环境失败而非逻辑失败。
5. **C++ 串行编译**(Debug x64,**必须 `/m:1`**,并发会报假的 C1041 / LNK1104;不要用 Release,third_party 全是 Debug 库):
   - **只有 scene lib 可在 regen 之前单独编**:`msbuild cpp\libs\services\scene\scene.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`
     (已核:scene lib 源码不引用 `client_endpoint`,工程无 ProjectReference)。
   - **(2026-09-29 更正)battle 不能在 regen 之前编**:收缩批当时的结论「battle 不依赖 regen」在当前工作树已不成立。集群外入口 D76 / D78
     在 battle 与 engine core 里用了新 proto 字段 `NodeInfo.client_endpoint`(=11,`proto/common/base/common.proto:33`):
     `battle_room_manager.cpp:1462`(`BuildAssignment`)、`battle/main.cpp:300`、`core/.../node/node.cpp:623`、`client_endpoint.cpp:305`;
     而现有 `cpp/generated/proto/common/base/common.pb.h` 里还没有这个字段。regen 前编 battle 必然报编译错误,**不是**收缩改动出了问题。
     battle 挪到第 3 步之后:`msbuild cpp\nodes\battle\battle.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`,或直接跑下面的全量。
   - 第 3 步之后全量:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。通过:proto / rpc / grpc_client / engine core / infra / scene lib / scene / gate /
     battle 0 error(`/WX` 开着),没有 `C1083 battle_binding_helper.h`,没有未解析的 `gate_battle_binding::*`。失败时保留每个工程的第一段错误(前 50 行);
     若 `player_battle.cpp` 因删 include 报错,只补回真正需要的那一个并回报。可选 Linux:`bash tools/scripts/build_linux.sh`(GCC `-Wall -Wextra`)。
6. **独立 gtest**(不进 vcxproj):
   - `battle_push_policy_test`,Linux / 带 g++ 的容器:
     `GT=third_party/grpc/third_party/googletest/googletest; g++ -std=c++23 -I cpp/nodes/battle -I "$GT/include" -I "$GT" cpp/nodes/battle/tests/battle_push_policy_test.cpp "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc" -lpthread -o /tmp/battle_push_policy_test && /tmp/battle_push_policy_test`
     → 6 条 PASSED。Windows(Developer PowerShell):`$GT='third_party\grpc\third_party\googletest\googletest'` 后
     `cl /std:c++latest /EHsc /utf-8 /I cpp\nodes\battle /I $GT\include /I $GT cpp\nodes\battle\tests\battle_push_policy_test.cpp $GT\src\gtest-all.cc $GT\src\gtest_main.cc /Fe:build\battle_push_policy_test.exe`,运行后同一判据。
   - 回归 `gate_security_test`(D-12:测试一字未改,C++ 默认仍是直连模式)与 `battle_ticket_test`:按 §18.8 第 4 步的命令编译运行,全绿。
7. **PowerShell 契约测试(D75;不依赖 regen,可与任一步并行)**:`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`
   (本批新增用例:gate 默认以路由模式部署、`-GateRouterMode 0` 回退仍可生成、负向只收 "0" / "1"、发现前缀、dev_tools / k8s_image 留空不透传);
   `k8s_zone_rollback_gate_router_mode.tests.ps1`、`dev_tools_merge_zone_contract.tests.ps1`、`start_game_command_contract.tests.ps1`。
   通过:退出码 0、fail=0。集群外入口批对同一批脚本和测试另有改动与期望,冲突时以 k8s-client-entry.md 的验证清单为准。
8. **客户端**(`D:\luyuan\wuxingqitan\mmorpg-client`):
   - 运行时程序集:`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40` → 退出码 0,skipped missing references 为空。
   - EditMode(先关闭打开该工程的 Unity 编辑器,`Temp/UnityLockfile` 不存在;或按 `tools/run_city_tiles_tests.ps1` 用隔离副本):
     `New-Item -ItemType Directory -Force "$env:TEMP\mmorpg-client-editmode" | Out-Null; & "C:\Program Files\Unity\Hub\Editor\6000.6.0f1\Editor\Unity.exe" -batchmode -nographics -projectPath "D:\luyuan\wuxingqitan\mmorpg-client" -runTests -testPlatform EditMode -assemblyNames MmorpgClient.Tests.EditMode.Battle -testResults "$env:TEMP\mmorpg-client-editmode\results.xml" -logFile "$env:TEMP\mmorpg-client-editmode\unity.log"`
     (Unity 路径以本机安装为准)→ results.xml 根节点 `failed="0"`,unity.log 没有 `error CS`。重点:`BattleDirectLinkTests`、`DirectRoutingBattleTransportTests`、
     `BattleClientStateMachineTests`、`BattleClientAutoBattleTests`、`SpectateClientStateMachineTests`、`BattleUiLayoutTests`、`BattleHudLogicTests`。
     再用同样方式跑 `MmorpgClient.Tests.EditMode.Net` / `.Guild` / `.Social` / `.Jubaozhai`,确认 `IBattleTransport` 未变、其余假实现不受影响。
   - 脚本语法(只解析不执行;在已打开的 pwsh 会话里直接执行,**不要**套 `pwsh -Command "…"`,外层 shell 会先展开 `$`):
     `foreach ($rel in 'tools/run_crosszone_pair.ps1','tools/gen_proto.ps1','tools/gen_messageids.ps1') { $t = $null; $e = $null; [void][System.Management.Automation.Language.Parser]::ParseFile((Join-Path 'D:\luyuan\wuxingqitan\mmorpg-client' $rel), [ref]$t, [ref]$e); "$rel errors=$($e.Count)" }`
     → 三行都是 `errors=0`。
9. **本地整栈冒烟**(第 1–6 步全绿、全量重编、重启全部进程之后。前置清理:`pwsh tools/scripts/dev_tools.ps1 -Command kafka-offset-reset`
   + `redis-cli FLUSHALL`;在 `robot/` 下执行):
   1. battle-smoke,`start_game.ps1 -GateRouterMode 1`(默认)与 `-GateRouterMode 0` 各起一次栈、各跑一次 `.\robot.exe -c etc/battle_smoke.yaml`
      → `BATTLE_SMOKE_OK … a_direct_turns>=1 b_direct_spectate_turns>=1`;battle 日志两条 `battle 直连握手成功 … signature_checked=1`
      (role 为 PARTICIPANT 与 OBSERVER 各一);A 的直连 `state_replies>=1`,B 的 `spectate_states>=1`;`metric=battle_ticket_issue_failed` 不出现;
      `metric=battle_frame_dropped_no_direct` 只允许零星采样;两种模式下 gate 启动日志 `出站白名单=` 都不含 `BattleNodeService`,
      gate 日志没有 BindBattle / `No node bound`,gate 不连 battle 的 gRPC 端口。
   2. 负向(D66,可选,需临时改 robot 或调试客户端):经 gate 发 `BattleClientPlayer` 上行(如 `SetAutoBattle`)超过
      `GATE_ILLEGAL_PACKET_THRESHOLD`(默认 50)次 → 每次都收到 tip 1003,大厅连接不被踢。
      **(2026-09-29 更正)发送节奏必须低于该消息号的限速**:gate 的 `ValidateClientMessage`(体积 + `MessageLimiter` 限速)排在战斗拒绝之前
      (`client_message_processor.cpp:888` 先于 `:944`),超速回 `kRateLimitExceeded` 而不是 1003,且**计入非法包**,累计到阈值即踢线
      (`CheckMessageLimit`,`:300-330`)。限速取值:MessageLimiter 表有该号的行按表,否则默认每 1s 窗口 3 条(`message_limiter.{h,cpp}`)。
      连发同号会出假红;按低于限速的节奏发(如每条间隔 ≥1s:限速按整秒时间戳判窗,`message_limiter.cpp:13, 34`,实际窗口可跨两个整秒),或只断言「未触发限速的每条都收到 1003,且 gate 日志没有 illegal-packet 踢线」。
   3. 跨 zone:`dev-start-zones 1,2` 后 `.\robot.exe -c etc/battle_smoke_cross_zone.yaml` → `CROSS_ZONE_MATCH_OK … a_direct_turns>=1 b_direct_turns>=1`,
      两侧直连 `state_replies>=1`。
   4. team-smoke(路由模式):`.\robot.exe -c etc/team_smoke.yaml` → `TEAM_SMOKE_OK`,每场(含 S8)都有 `[team-smoke] battle direct-connect delivery`
      且 `turn_results>=1`。
   5. features-smoke:用 `mode: features-smoke` 的配置选一个已有账号 / 角色,`features_smoke` 段配 `battle_config_id: 1`、
      `accept_mission_id: 12`、`claim_mission_id: 12`、`verify_relogin: true` → `FEATURES_BATTLE_OK` 与 `FEATURES_SMOKE_OK`。
   6. K8s 路由模式 battle-smoke 的事后补验(D65)由用户指定环境后执行,判据同第 1 条。
10. **Unity 实机**(第 9 步绿之后,在客户端仓):`pwsh -File tools/build_crosszone_player.ps1 -UnityExe "C:\Program Files\Unity\Hub\Editor\6000.6.0f1\Editor\Unity.exe"`
    重出播放器。**必须显式传 `-UnityExe`**:脚本默认值指向 6000.5.8f1(`build_crosszone_player.ps1:16`),而工程是 6000.6.0f1
    (`ProjectSettings/ProjectVersion.txt`,与第 8 步同一版本);路径以执行机实际安装为准(本机 `C:\Program Files\Unity\Hub\Editor` 下没有编辑器)。双 zone 路由模式起栈后
    `pwsh -File tools/run_crosszone_pair.ps1` → 退出码 0;PASS 行 `a_turns == a_direct_turns`、`b_turns == b_direct_turns`;两侧日志都有
    `[battle-direct] verified` 与 `battle_direct ready`;battle 日志 `signature_checked=1`;观战侧没有「观战首帧超时」。
    负向:阻断 battle 客户端端口后再跑 → `RESULT=FAIL stage=battle_direct`(RST 约 10s,SYN 黑洞最长约 90s),而不是 `stage=battle` 超时。
    人工目测:失败横幅在 toast 下方、不叠字;已提交后断直连、恢复时服务端已进下一回合 → 屏上回合 / HP / 倒计时刷新、按钮可点;
    主城入口四档切换符合 §22.3「Unity」;换会话(大厅重连)后收到 `NotifyBattleReconnect` → 补签 → 直连就绪 → `GetBattleState` 恢复战斗(D72)。

### 22.8 §21 勘误表

依据:09-29 对 §21 的 5 路对抗核验(partial / 遗漏条目逐条给出替换口径),加上事后发现的 K8s 连接数推断错误。「依据」列的行号是核验时
(收缩改动之前)的工作树行号,收缩后多已变动,仅作出处追溯。§21 原文不改。

| # | §21 位置 | 原表述 | 更正 | 依据 |
|---|---|---|---|---|
| 1 | §21.1 表「C++ 裸跑」行 | 直连成功时 2 条;直连失败时回落 gate 中继,1 条 | 服务端旧模式下两条路径确实都在,但参考客户端 robot **不会自动回落**:10s 内建不成直连即判冒烟失败,只有预设 `skip_direct_connect: true` 才全程走中继;Unity 当时未核。收缩后两种模式 gate 都不中继,直连失败 = 本局不能出手(D66) | `robot/battle_direct_conn.go:28-37`、`battle_smoke_scenario.go:323-331`、`etc/battle_smoke.yaml:34-37`(核验时) |
| 2 | §21.1 表「start_game」行 | 战斗期间 2 条,而且只能走直连 | 只有**上行**只能走直连;下行在没有直连时仍经 Kafka→gate 回落(`PushToPlayer` 不分模式,gate 的 `PushToPlayerEventHandler` 也不分),直连没建成时只剩大厅 1 条,处于 §21.3 的不对称状态。收缩后下行战斗帧也只走直连(D68) | `battle_room_manager.cpp:1271-1288`、`gate_event_handler.cpp:274-292`(核验时) |
| 3 | §21.1 表「K8s」行 | 【推】battle 对外通告的是 POD_IP,集群外连不上;直连失败后回落 gate 中继,实际只有 1 条 | 「通告 POD_IP」不是推断,代码可证:票据 host 取 `NodeInfo.endpoint`,`ResolveNodeIp` 优先 `POD_IP`(Downward API 注入),端口 20000,脚本没给 battle 配 hostPort / NodePort。「回落 gate 中继、只剩 1 条」**不成立**:K8s 上 gate 同样通告 POD_IP,集群外客户端 gate 与 battle **都连不上**,集群内 robot 两者都能连;robot 也不会自动回落。现状已被 D75(默认 "1")与集群外入口 D76–D93 取代 | `battle_room_manager.cpp:1308-1313, 1339-1340`、`node.cpp:282-293`、`k8s_deploy.ps1:1061-1064, 4038-4039`(核验时);gate 同样通告 POD_IP 由集群外入口批核实,见 [k8s-client-entry.md](./k8s-client-entry.md) |
| 4 | §21.1(漏项) | 只列了三处环境 | 漏了本机 `dev_tools.ps1 dev-start` / `dev-start-zones`:它不设 `GATE_CLIENT_RPC_ROUTER`,继承父 shell,未设即旧模式;09-05 两轮冒烟就是在父 shell 里手动切换的模式 | 09-29 核验遗漏项(现状类) |
| 5 | §21.2 | 要删的东西全部还在、参与编译,只是被开关隔开了 | 「全部还在、参与编译」成立,但**开关只在 gate**:battle 照常注册 `BattleClientPlayerGrpcImpl`、照常发 Bind/Unbind,scene 照常重发 Bind,`SetIfEmptyHandler` 无条件安装;它们在路由模式下无效,是因为 gate 忽略绑定事件、拒绝上行、白名单不含 battle,而不是自己被开关关掉。收缩后已删(D66 / D67 / D72) | `battle/main.cpp:199-201`、`battle_room_manager.cpp:567, 1255`、`player_battle.cpp:2069-2073`、`battle_binding_helper.cpp:24-39`(核验时) |
| 6 | §21.2 第 5 条 | 以及 gate 端按应答类型反查消息号的桥接 | 桥接(`SetIfEmptyHandler` + `sResponseTypeToMsgId`)是**所有 gRPC 回包的通用出口**:旧模式下 login / match / scene_manager 的应答、路由模式下 Forward 回的 `MessageContent` 都经它下发,不是 battle 专属、不受开关控制,**不能删**。§18.7 收缩条把它列进删码清单本身就说宽了;D66 保留它 | `gate/main.cpp:268-313`(核验时) |
| 7 | §21.2(漏项) | 清单只列 gate 侧与 scene 重发 | 漏了 D37 点名的 Kafka Bind/Unbind 契约的生产端(battle 的 `SendBindBattle` / `SendUnbindBattle`)和分派端(`gate_kafka_command_router.cpp`),以及 §6 点名的回落 `PushMessageViaGate`。收缩后:生产端与契约删除(D67),分派端随 regen 消失,`PushMessageViaGate` 只服务大厅公告(D68) | `battle_room_manager.cpp:102-140`、`gate_kafka_command_router.cpp:94-107`(核验时) |
| 8 | §21.3(漏项) | 没给 D26 fail-open 的代码位置 | 位置是 `PushAssignment` 签不出票时 `LOG_WARN` 后 return、开局照常;其日志原文「玩家将全程走 gate 中继」在路由模式下不成立。已由 D70 改 fail-closed,该日志随之删除。另应注明 robot 直连失败不会自动回落(同第 1 行) | `battle_room_manager.cpp:1297-1313, 1348-1358`(核验时) |
| 9 | §21.4 第 3 条 | 真正端到端验证过直连的只有 robot:09-05 两轮 `BATTLE_SMOKE_OK`,`a_direct_turns == 总回合数` | 09-05 两轮(旧模式 14/14、路由模式 13/13)属实,但「相等」是实测值,robot 只断言 ≥1;之后 09-22、09-28 在 `start_game.ps1` 路由模式下还各有一次 `BATTLE_SMOKE_OK`,robot 缺省走直连、直连失败即判失败,但记录没写配置与直连计数 | `PROGRESS.md` 09-05 / 09-22 / 09-28 条目;`battle_smoke_scenario.go:360-368`(核验时) |
| 10 | §21.5 第 1 条 | 终局包如果还积压在用户态输出缓冲里,可能丢失;`DisconnectAll` 的注释已经过时(`battle/main.cpp:224-230`) | 受影响的只有观众的 `SpectateEndS2C`:停机作废路径对参战者本就不推直连终局包;丢的也只是内核没收下、积压在用户态 `outputBuffer_` 的那一截,通常只发生在慢客户端。**修复约束**:不能简单去掉 `DisconnectAll` 的 forceClose(gRPC drain ≤2s 后两跳 `quit()`,延迟关闭来不及触发,连接停在 kDisconnecting,撞 `TcpConnection.cc:71` assert)。注释实际在 `battle_client_edge.cpp:101-103` / `.h:67-68`(`main.cpp:224-230` 只是调用点),且与延迟 shutdown 同一提交 `c149b7c57` 引入,从一开始就不符,不是「过时」 | `battle_room_manager.cpp:1253-1260`、muduo `TcpConnection.cc:71, 150-174, 334-345`、`node.cpp:48, 1035-1063`(核验时) |
| 11 | §21.5 第 2 条(补充) | 补签依赖观战索引 | 补充:`spectate.go` 函数注释「索引缺失只是该场不可观战」自 D25 / D33 起就不成立(本批已改注释);ZADD 失败不影响补签,只有 SETEX 失败会。**(已由集群外入口 2b 第 9 条改为建房前写,fail-closed)**:SETEX 写不进去现在是不建房、记 `index_failed`,不再出现「开了局却补不了签」,见 §22.5 第 4 条 | 09-29 核验遗漏项(风险类);`go/match/internal/logic/spectate.go`;现状 `gather.go:308-321`、`spectate.go:120-145` |
| 12 | §21.5 第 3 条 | `BattleRoomManager::edge_` 是裸指针,事故审计建议在 before-shutdown 里调 `SetClientEdge(nullptr)`,目前没做 | 事实都对,但把审计意见说重了:审计判它**不可达**(只在推送路径读,loop 退出后不会再读),处置是「记录;**可**在 before-shutdown 置空」,属可选卫生项;审计引的 `main.cpp:197` 当时已漂到 `:206`。另:审计把 `BattleClientEdge` 归为「进程级单例 + 回调只在 loop 运行期触发」,两条前提都不准确(edge 在 context 里;`~TcpServer` 在 loop 退出后仍会经 `connectDestroyed` 回调),真正起作用的只有 `node_entry.h` 的声明顺序 | `battle_room_manager.h:63, 293`、`incident-gate-tcpconnection-dtor-assert-2026-09-13.md:502`(核验时) |
| 13 | §21.5 第 4 条 | 两个冒烟场景的战斗步骤可能被拒 | 两者后果不同:features-smoke 的 `SetAutoBattle` 要等同号回包,gate 回的是 tip、robot 不处理,路由模式下**必定** 15s 超时失败;team-smoke 的 `send` 不检查拒绝,战斗靠 6s 超时默认行动推进、下行经 gate 回落照样收到,**大概率仍输出 `TEAM_SMOKE_OK`**,把拒绝掩盖了,不能当战斗上行的证据;其文件头「全程经 gate 中继(D23 回落)」在路由模式下不成立。收缩后两者都改走直连(D73) | `features_smoke_scenario.go:24, 417-447`、`team_smoke_scenario.go:33-36, 659-667, 874-892`(核验时) |
| 14 | §21.5 第 5 条 | 战斗中换 gate 之后,Kafka 回落推送会投到旧会话,恢复只能靠客户端补签后重连 | 换 gate 本身**不会**断开通往 battle 的直连;投到旧会话的只是「当时没有活直连」的那部分下行。补签加重连只能恢复直连,**不会刷新路由**(`HandleIssueBattleTicket` / `AttachDirectConnection` 都不改 routing),旧路由一直保持到战斗结束。收缩后 `RefreshRoutingFromSession` 已删(D71),战斗帧不再回落,残留影响只剩大厅公告(§22.5 第 3 条) | `battle_room_manager.cpp:1267-1289, 1362-1412, 1728-1775`(核验时) |
| 15 | §21.5 第 6 条 | 本地端口冲突,给 battle 设 `RPC_PORT=20010` 绕过 | 补出处与现状:见 `PROGRESS.md:4201`;根因是 `etcd_service.cpp:275`(端口 CAS 失败后重试时不清零端口)加 `node_allocator.cpp:227`(把残留端口当预设端口),代码**未修**;绕法只在 `start_game.ps1:585-586`(当前工作树;核验时为 :583-584),`cpp_nodes.ps1` / `dev_tools.ps1 cpp-node-start` 不设 `RPC_PORT`,仍会踩到 | 09-29 核验遗漏项(风险类) |

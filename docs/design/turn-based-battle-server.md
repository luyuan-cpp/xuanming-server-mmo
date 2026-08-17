# 回合制战斗服设计(turn-based battle server)

> 状态:一期实现中(2026-08-15 起)。本文是回合制战斗(问道/梦幻式)的架构决策与模块规格,
> 是 battle 节点、battle 引擎、scene 集成、match 服务四个模块的实现依据。
> 未编译声明:本轮所有代码均为"待 Codex 生成+编译验证"状态,见 §12。

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
| D1 | **人不动、数据动**:开战不做玩家跨节点交接,战斗服只接收"战斗快照"(属性/技能/buff/道具副本),打完回传一条结算事件 | 规避 `AllowUnsafeCrossNodeHandoff=false` 的持久化屏障缺失;保持 single-writer 不变量(cross_server_architecture_principle.md 规则 #8):场景服全程是玩家权威数据唯一 writer,战斗服只拥有可丢弃的派生副本 |
| D2 | **新增独立节点类型 `BattleNodeService = 28`**(gRPC 协议,不进 `IsZoneScopedNodeType` → 全局池) | 回合制战斗是逻辑房间不是空间场景,不需要 AOI/Movement/20FPS tick;全局池让跨 zone 匹配/观战免客户端换 gate;etcd 三阶段注册/Kafka topic(`battle-{id}`)由框架自动派生 |
| D3 | **所有战斗统一由 match 服务编排开局**,两个入口:①队列匹配(系统凑单);②场景发起 PK(点名成局,challenge 应战后成局)。两入口汇入同一条 gather 管线;`battle_id`/`challenge_id` 由 match 服务用 shared/snowflake(17-bit worker 布局)生产 | 用户产品决策:匹配遇怪 + 场景可点名切磋;单一编排者让 gather/补偿逻辑只存在一份;SnowFlake 节点隔离不变量:battle_id 只能由 match 节点生产 |
| D4 | **结算串行化**:`InBattleComp` 摘除条件 = 场景服已应用结算;摘除前不得再排队/开战 | 残血带出战斗要求下一场快照必须反映上一场结果;每玩家最多一单在途,幂等去重退化为"每人记最近一个 battle_id" |
| D5 | **技能/buff 沿用现有表与管线,时间换算成回合**:复用 `SkillTable`/`BuffTable`/`CooldownTable`/`SkillPermission` 及 damage/bonus_damage 表达式、effect[]→buff 语义,把挂钟 timer 全部替换为回合计数;回合制战斗代码是**全新独立模块**,不改实时战斗代码 | 用户指示:技能参照原来那套、把 timer 弄掉弄成回合;新开回合制战斗模块 |
| D6 | **上行走 gate→gRPC,下行走 Kafka**:客户端战斗消息经 gate 按 `OptionFileDefaultNode=NODE_BATTLE` 路由(gRPC + `x-session-detail-bin` metadata);battle 推 S2C 走 Kafka `gate-{gate_id}` 的 `PushToPlayerEvent`/`BroadcastToPlayersEvent`(既有已实现路径) | battle 不需要 muduo TCP server 全套;回合制每回合一条消息,Kafka 延迟完全可接受;不用管 battle↔gate 连接拓扑(全局池连全服 gate 的问题消失) |
| D7 | **确定性引擎**:引擎输入 = 快照 + 指令流 + 随机种子,输出纯函数;战斗事件流(每回合一条 `TurnResult`)是一等公民 | 观战 = 转发事件流;断线重连 = 补发状态快照;回放 = 免费;单测可穷打 |

## 3. 生命周期与数据流

### 3.1 正常流程

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
| 玩家战斗中掉线 | 指令不再提交 | 战斗照打,回合超时按默认行动(普攻);重连(RECONNECT)进场时 scene 检查 InBattleComp → 向 gate 重发 BindBattleEvent + 推 BattleReconnectS2C,客户端用 GetBattleState 补拉 |
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
- 伤害公式照搬 `CalculateFinalDamage` 语义:`base*(1+strength*0.1) - armor`,`*(1-resistance*0.01)`,
  `critchance/100` 概率 ×2,饱和到 0;
- buff 语义照搬:max_layer 叠层 / immune_tag / dispel_tag / sub_buff / target_sub_buff / interval_effect。

**时间→回合换算**(v1 策略,后续可在 Excel 加回合列覆盖):
`rounds = max(1, ceil(duration_ms / kRoundDurationMs))`,`kRoundDurationMs = 6000` 常量。
冷却、buff 持续、周期 tick(interval)全部按此换算,战斗内只走回合计数,无任何 timer。

**回合规则 v1**:
- 回合结构:收集全员行动(带 `action_deadline`)→ 按 `speed` 降序结算(同速按 actor_id 稳定序)
  → 逐个执行(死亡单位跳过)→ 回合末 buff tick(周期效果/持续减一/到期移除)→ 胜负判定;
- 行动类型:`ATTACK`(普攻)/ `SKILL` / `DEFEND`(本回合受伤减半)/ `FLEE`(逃跑,成功率基于速度差,
  PVE 可逃,PVP 一期不可逃)/ `ITEM`(从快照道具副本扣,结算回写);
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
- 冻结清单(InBattleComp 存在时拒绝):再次排队/开战、交易、使用改属性道具、切场景、跨 zone 迁移;
  放行:聊天、邮件收取(不动战斗属性部分)、好友。

### 5.4 match 服务 `go/match/`

go-zero,结构照抄 `go/scene_manager`(config/etc yaml/internal/{logic,svc,server}/noderegistry):
- 队列:Redis(`match:queue:{mode}:{battle_config_id}` list + ticket hash),JoinQueue 前
  咨询性查 `battle:lock:{player_id}`;solo 即配,组队/1v1 FIFO 凑单;
- matcher loop:凑单 → battle_id(shared/snowflake,17-bit 布局,match 节点专属生产)→
  gather(§3.1)→ 失败补偿(§3.2);玩家定位读 `player:{id}:location`(scene_manager 维护,
  本文档即该 key 的共享契约记录);
- battle 节点发现:etcd list-watch `BattleNodeService.rpc/` 前缀(照 LoadReporter 模式),
  v1 随机选,负载上报二期;
- 无状态、可水平扩,多实例用 Redis 锁保护 matcher loop(或 v1 单实例部署 + 文档标注);
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
| `match:queue:*` / `match:ticket:{player_id}` | match | match | 队列状态,重启可续 |

## 7. 新增不变量(并入宪法 §7 语义)

1. `battle_id` 只能由 match 节点生产(shared/snowflake 17-bit 布局,可用 `ParseGuid` 解);
2. 结算事件按 `player_id` 做 Kafka key;应用以 `InBattleComp.battle_id` 匹配为准,不匹配丢弃并告警;
3. `InBattleComp` 摘除 = 结算已应用或作废,是玩家再次进战斗的唯一放行条件;
4. 战斗服对玩家权威数据零写权:结算只能以事件形式交由 scene 应用,禁止 battle 直写 DB/Redis 玩家数据;
5. 引擎内所有随机只走种子 RNG(确定性,观战/回放依赖)。

## 8. 部署形态

battle 为独立进程池(全局,不分 zone),K8s 单独 Deployment(照 scene-world/scene-instance
第三池模式);战斗房间为纯内存对象,节点无持久状态,崩溃即战斗作废(§3.2 补偿);
水平扩容 = 加实例,match 按发现列表分配。Agones/rooms Counter 一期不接。

## 9. 二期清单(预留接口)

观战(BattleRoom.observers + WatchBattle RPC + 事件流延迟推送)、ready check、转播 relay、
回放持久化(事件流落盘)、宠物参战(快照加 pet 段)、robot battle 动作、匹配负载均衡、
Excel 回合列(替代 kRoundDurationMs 换算)。

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

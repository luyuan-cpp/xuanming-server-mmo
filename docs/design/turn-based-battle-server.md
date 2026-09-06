# 回合制战斗服设计(turn-based battle server)

> 状态:一期已落地(2026-08-15 起实现,2026-08-17 全栈冒烟通);二期实现中(2026-08-31 起,
> 见 §10 观战、§11 自动战斗/5v5/队伍上限)。本文是回合制战斗(问道/梦幻式)的架构决策与
> 模块规格,是 battle 节点、battle 引擎、scene 集成、match 服务四个模块的实现依据。
> 未编译声明:二期代码为"待 Codex 生成+编译验证"状态,见 §14。

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
| D6 | **上行走 gate→gRPC,下行走 Kafka**:客户端战斗消息经 gate 按 `OptionFileDefaultNode=NODE_BATTLE` 路由(gRPC + `x-session-detail-bin` metadata);battle 推 S2C 走 Kafka `gate-{gate_id}` 的 `PushToPlayerEvent`/`BroadcastToPlayersEvent`(既有已实现路径)。**2026-09-05 起降级为回落路径**:客户端凭票据直连 battle 节点,有直连即直发,见 §18 D23 | battle 不需要 muduo TCP server 全套;回合制每回合一条消息,Kafka 延迟完全可接受;不用管 battle↔gate 连接拓扑(全局池连全服 gate 的问题消失)。§18 之后:gate 中继只在直连未建立 / 断开时兜底 |
| D7 | **确定性引擎**:引擎输入 = 快照 + 指令流 + 随机种子,输出纯函数;战斗事件流(每回合一条 `TurnResult`)是一等公民 | 观战 = 转发事件流;断线重连 = 补发状态快照;回放 = 免费;单测可穷打 |
| D35 | **传输选型定谳(2026-09-05):按平面拆传输,不按节点选传输。** 客户端 ↔ battle = **muduo TCP 直连**(照搬 gate 客户端面,票据入场,§18);match(Go)→ battle 的控制面(CreateBattle / DestroyBattle / AddObserver / RemoveObserver / IssueBattleTicket)= **gRPC unary**;battle → 结算 = Kafka 幂等;battle → gate **不再有这条边**。三方案排序:①客户端面 muduo + 控制面 gRPC(本决策)> ②全 muduo(前提:`GameRpcMessage` 加 request_id + 生产级 Go RPC0 客户端)> ③全 gRPC(D6 原形态)。**battle 保持 C++**,gRPC ≠ Go。D6 的 gate 中继与 Kafka 回落定为过渡路径,收缩条件见 §18.7。完整论证、gRPC 在本仓的实测成本、RPC0 缺口清单见 [battle-transport-decision.md](./battle-transport-decision.md) | 战斗流量九成五在客户端边,muduo 单线程零拷贝完胜;gRPC 在本仓每节点起手 17 线程、≤8 poller 各阻塞在 promise/future、应答等 5ms CQ 轮询,不该进热路径。控制面每局一两次调用,gRPC 成本可忽略,而 RPC0 无请求关联 id、无 deadline、无 Go 客户端,补齐等于重写三成 gRPC。D6 原论据「不用管 battle↔gate 拓扑」不成立:gate 白名单已含 Battle,每 gate 对每 battle 建 channel + CQ |

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
| D10 | **观众复用参战者的会话绑定机制**:AddObserver 成功后 battle 节点发既有 `BindBattleEvent` 把观众 session 绑到本节点,退出观战的上行(`StopWatchBattle`)按绑定路由 | 不新增 gate 机制;Kafka 同 key(player_id)保证 gate 先处理绑定再下发首帧 |
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
- 经验/道具落地:引擎已产出 exp_gain/items_gained,scene 侧金币真入账,经验/掉落需先建经验/等级/背包系统。
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

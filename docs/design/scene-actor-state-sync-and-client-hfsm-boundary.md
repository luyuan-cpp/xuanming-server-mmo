# 主角状态互斥即时同步与客户端 UnityHFSM 接入边界

> 关联契约与实现：服务端 `proto/scene/client_player_common.proto`、`cpp/libs/modules/combat_ai/state_mutex_matrix.h`、`cpp/libs/services/scene/actor/action_state/`、`cpp/libs/services/scene/combat_state/`、Java `game-scene/.../skill/`；客户端对应记录见 `mmorpg-client/Docs/ClientProtocolCompatibility.md`「主角权威状态互斥即时同步与未来 UnityHFSM 接入边界」。

## 1. 背景与现状分层

- **服务端状态权威层已落地**：C++、Go、Java 三端共用同一套 64 位 O(1) `StateMutexMatrix` + `Fsm` + 退出钩子语义（规范化 `ActorActionState` 与 `ActorActionCombatState` 表，模式统一为 `0=Block / 1=Allow / 2=Interrupt`）。
- **为什么 66 同步不够**：原有的 66（`ActorBaseAttributesS2C`，`actor_state_attribute_sync.cpp`）以每 2 帧（10Hz）节流扇出给 AOI `visibleWatchers`，既不即时也不保主角本人必达，且不包含 `ActorActionState`（`Combat / TeamFollow / Mounted`）。
- **现行分工**：
  - **旁观者（AOI）**：继续走现有 66 属性状态节流同步与 `SkillUsed`/`SkillInterrupted` 广播，不把主角高频私有状态全量推给视野周围玩家。
  - **主角本人（即时单播）**：新增 `SceneClientPlayerCommon.NotifySelfActorStateFlags`（消息号 `440`，`CLIENT_RPC_PUSH`），仅在行为位图或战斗异常位图发生真实变化时立刻单播给主角本人。

## 2. 权威协议与位图语义

消息定义在 [proto/scene/client_player_common.proto](../../proto/scene/client_player_common.proto)：

```protobuf
message SelfActorStateFlagsS2C {
  uint64 action_state_mask = 1; // 位 i = eActorState(i)
  uint64 combat_state_mask = 2; // 位 i = eActorCombatState(i)
  uint64 state_seq = 3;         // 单次进场内单调递增序号(仅位变化或重推时递增,防乱序覆盖)
}
```

- **`action_state_mask`（`eActorState`）**：`bit 0 = Combat (0)`、`bit 1 = TeamFollow (1)`、`bit 2 = Mounted (2)`。
- **`combat_state_mask`（`eActorCombatState`）**：`bit 1 = Silence (1)`；采用来源 Buff 引用计数（仅 `0 -> 1` 与 `1 -> 0` 时翻位，叠加同类 Buff 不重复推包）。
- **合并与静默规则**：
  - `TryPerformAction` 内部多条打断 + 退出钩子 + 新入态通过 `SelfSyncSuppressGuard` 合并为至多 1 次 `SelfActorStateFlagsS2C` 单播。
  - 重复加减相同未变化位图不推包；NPC、离线托管、无有效网关会话，以及跨节点/跨区交接冻结期间（`PlayerFrozenComp` / `player.frozen()`）不推包。
  - 每次进场 / 换场景落地（`EnterScene` 之后），客户端先将本地状态位清零，服务端将上次推送基线重置为 `0`，若当前存在非零状态立刻补推 1 次（首次进场全 0 不发多余空包）。

## 3. 客户端 `Inspiaaa/UnityHFSM` 选型与强制边界

### 3.1 结论：适合未来实时 MMO，暂不预埋依赖

- 当前客户端仅为移动、服务端驱动跟随（`TeamFollowDriven`）与回合制战斗演出（`BattleSequencer`），**尚未构建实时 MMO 角色动作与技能连招演出状态机**，现阶段不提前引入 `UnityHFSM` 包壳依赖。
- 当后续正式开发实时 MMO 大世界角色动作 / 技能演出时，**适合接入 `Inspiaaa/UnityHFSM`**（建议以本地 `.tgz` 或源码 vendor 方式引入，先核对 MIT 许可）：
  1. **契合动作演出语义**：内置层级状态（`Locomotion` / `Mounted` / `Combat` / `Controlled`）、`needsExitTime`（前摇/后摇/收刀自然过渡）与 `ForceRequestStateChange`（受控/打断强切）。
  2. **满足双端平台红线**：纯 C#、单线程 `Update()` 驱动、无 `Thread`/`Task.Run`/`System.Reflection.Emit`，兼容微信/抖音小游戏 WebGL2 单线程与 iOS/Android IL2CPP。

### 3.2 必须守住的架构红线：「表现层归客户端、状态权威归服务端」

1. **客户端 HFSM 只负责表现与本地交互预判**：动画状态过渡、武器挂点、特效节奏与技能栏/上马按钮本地置灰防误触。
2. **禁止客户端自行裁决互斥与打断结果**：施法打断关系、行为互斥、沉默禁言/禁技、冷却与伤害计算，**唯一权威在服务端**（`StateMutexMatrix` + `SkillSystem` + `ActorActionStateSystem` + `CombatStateSystem`），一切以服务端 RPC 应答与推送为准。
3. **订阅 `OnSelfActorStateChanged` 单向收敛**：客户端 HFSM 必须订阅 `GameClient.OnSelfActorStateChanged(prev, next)` 并读取 `GameClient.SelfActorState`；当 `IsSilenced` / `IsInCombat` / `IsTeamFollowing` / `IsMounted` 被服务端强制改写或打断时，表现层通过 `ForceRequestStateChange` 立即收敛到服务端当前状态，不得拿本地动画未播完拒绝服务端纠偏。
4. **单线程与生命周期清零**：仅在 Unity 主线程推进状态机，严禁后台线程或并发回调改写状态；随 `NotifyEnterScene` / 断线 `ResetSelfActorStateFlags()` 对称退订与复位。

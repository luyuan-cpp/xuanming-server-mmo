# 《论剑 / 人宠会武》V5 扩展设计：临阵唤灵（单局限 1 次替补灵宠召唤 / 临场换宠）

> 日期：2026-11-26  
> 状态：V5 扩展实施设计（建立在 V1–V4 双服与客户端闭环之上，复用 Codex 已交付的 `ranked-play-v4-tactical-ui-20261125/ui/summon-*.png` 6 枚透明切片，零新美术缺口）  
> 适用仓库：`xuanming-server-mmo-cpp`（C++ 权威服）、`xuanming-server-mmo`（Java 独立服同步）、`mmorpg-client`（Unity 客户端与 UGUI）  
> 关联设计：[ranked-play-v3-tactical-prep.md](ranked-play-v3-tactical-prep.md) · [ranked-play-v4-team-tactics-and-highlights.md](ranked-play-v4-team-tactics-and-highlights.md) · [wuxing-gameplay-expansions-v1.md](wuxing-gameplay-expansions-v1.md)（严格零重叠、零冲突）

---

## 一、需求分析

论剑 V1–V4 已完整落地跨规模单分排位、6 套 4 技能娱乐轮换预设、凝锋 `+20%`、战意渐盛、第 5 回合体面认输、战前 15 秒明牌变阵（换赛季印/换首发宠）、队内一键战术标点、本局战术复盘徽记与连胜破势。

当前正式自养论剑（1V1–5V5）与群仙问道赛仍存在一处非常真实、体感极强的局内玩法断层：
- V3 已由场景服将玩家最多 5 只合规满血蓝备选灵宠（`standby_pets`）打包进战斗房间，但开打第 1 回合后，这 5 只备选灵宠立刻被丢弃；
- 一旦首发灵宠在第 2–3 回合被对手用「凝锋 `+20%`」集火秒杀，玩家在剩余 10 余回合内只能孤身应战，缺少回合制人宠对战最核心的「中盘拍出二宠逆转 / 针对换宠」博弈高潮，也浪费了玩家培养第 2～6 只功能宠的策略价值。

目标：在不增加任何肝度、不拉长单局回合数、不新增数值付费、**零新 RPC / 零新消息号 / 零新库表 / 零新配表**的前提下，落地 **「临阵唤灵」**，且直接接入 Codex 已交付的 6 枚 `summon-*.png` 帮会风控件实现开箱闭环。

---

## 二、设计分析

### 2.1 生效范围与核心规则
1. **生效模式**：仅正式自养竞技模式生效（与 V3 战前变阵同口径）：
   - 正式人宠会武排位：`match_mode == MATCH_MODE_PVP_TEAM && ranked_context.season_id != 0 && !ranked_context.use_free_preset`；
   - 全服赛季战队锦标赛：`match_mode == MATCH_MODE_SEASON_TOURNAMENT && !ranked_context.use_free_preset`。
   - PVE、首领试炼、瑞兽迎春、五行幻境、免费预设娱乐场、切磋与帮战不开启，零额外打扰。
2. **备选灵宠池构建与 V3 联动**：
   - 开局（或 V3 `FinalizePrepPhase()` 锁阵后），为每名非 AI 真人玩家冻结本局中盘备选池 `standbyPetPoolByPlayer[playerId]` = 建房快照全部合规灵宠（原首发宠 + `standby_pets`，去重、复核归属与 `pet_cultivation`、排除 `competitive_temporary`）**减去最终实际首发上场的灵宠**。
   - 若玩家在 V3 战前 15 秒将首发从 A 宠换为 B 宠，则亮牌后 A 宠自动进入中盘备选池、B 宠移出备选池，玩家中盘仍可唤回 A 宠。
3. **单局严格限 1 次 + 第 2 回合解锁（反拖回合、反逼氪）**：
   - `summon_pet_unlock_round = 2`（第 1 回合首发由 V3 战前 15 秒决定，第 2 回合起解锁临阵唤灵）；
   - 每名玩家整局至多成功唤灵 **1 次**（备选池为空则为 0 次）；
   - **养 2 只宠即享 100% 满配次数**，零逼氪；最多 1 次/人，绝不演化成数十回合无限招宠拉锯；
   - 天然联动「每 3 回合人宠五行相生合击」：可在第 2/5/8 回合临场唤出与主角五行相生的备选宠，于次轮触发合击共鸣 `+10%`。
4. **回合结算与出手顺序规则**：
   - 复用既有 `SubmitBattleAction`：新增 `eBattleActionType::BATTLE_ACTION_SUMMON_PET = 8` 与 `BattleAction.summon_pet_id = 5`；
   - 消耗主角本人本回合正常行动（宠物不可提交；变阵期禁止）；
   - 当前首发宠**已阵亡、被劝退或仍存活**均允许唤灵（阵亡补位或存活战术换宠；被替换旧宠退出本局战场，不再回备选池）；
   - 若主角在本回合轮到出手前被击杀或受硬控（眩晕/冰冻/昏睡/混乱）未能执行，**不扣除唤灵次数**，下回合仍可再次使用；
   - 执行唤灵时：以原宠 `formation_slot`（若无首发宠则分配同队下一阵位）与**全新局内 `actor_id = kPetActorIdBase + nextPetActorIndex++`** 构建满血满蓝新灵宠，原地替换（或追加）进 `actors`，初始化新宠 `runtime_skills` 与开场被动/光环，发射 `BATTLE_EVENT_SUMMON_PET = 24`（`source = 主角`，`target = 新宠 actor_id`，`skill_table_id = 新宠 pet_table_id`，`success = true`）；
   - **新唤出的灵宠不在当回合插入行动**（从下一回合开始正常按速度排表出手），保证出手序、单回合预算与确定性回放 100% 稳定。

### 2.2 协议增量（零新 RPC / 零新消息号 / 零新 Tip）
- `proto/battle/battle_data.proto`：
  - `eBattleActionType` 末尾追加 `BATTLE_ACTION_SUMMON_PET = 8;`
  - `BattleAction` 末尾追加 `uint64 summon_pet_id = 5;`
  - `eBattleEventType` 末尾追加 `BATTLE_EVENT_SUMMON_PET = 24;`
- `proto/battle/player_battle.proto`：
  - `BattleStateS2C` 末尾追加（复用既有 `BattlePrepPetOption`）：
    - `repeated BattlePrepPetOption self_standby_pets = 36;`（私有：仅本人可见，对手与观众清空）
    - `uint32 self_summon_pet_remaining = 37;`（私有：本人剩余次数 0/1，对手与观众清空）
    - `uint32 summon_pet_unlock_round = 38;`（公开规则：正式自养竞技为 2，其余模式为 0）

---

## 三、思想分析

1. **最小增量复用（KISS / Ponytail）**：不新增任何 RPC、消息号、错误码、配置表或数据库列；直接复用 V3 已入房的 `standby_pets`、`BuildPetActor`、`BattlePrepPetOption` 以及现有 `SubmitBattleAction` / `BattleStateS2C` / `TurnResultS2C` 通道，天然覆盖断线重连（`GetBattleState`）、分级观战与战报回放。
2. **全新局内 `actor_id` 替换策略**：新唤出灵宠保留原宠 `formation_slot` 但分配新 `actor_id`，一举消除四大隐患：① 旧宠 `require_living_caster` 光环不会因新宠复用旧 ID 而误续命；② 旧宠若在本回合排在主角之后，其原 `pendingActions` 不会误施加到新宠身上；③ Unity `BattleScreen` 的 `structural` 判定自动销毁旧宠节点并按新宠物种/头像/血条重建视图；④ V4 `BattleCombatHighlight` 与五行相生均按 `owner_player_id` / 实时 `pet_table_id` 计算，自然无缝衔接。

---

## 四、漏洞分析与防线

| 潜在漏洞 | 防护措施 |
|---|---|
| 对手或观众通过 `BattleStateS2C` 窥探己方剩余备选宠 | C++ `RedactStateForViewer` 与 Java `BattleViews.redact` 强制清空 `self_standby_pets` 与 `self_summon_pet_remaining`，仅对本人 `viewerPlayerId` 回填 |
| 伪造 `summon_pet_id`（他人宠物、临时宠、当前已在场宠或重复召唤） | 校验与执行两层均严格查表 `standbyPetPoolByPlayer[playerId]` 白名单，且执行成功后立即扣零 `summonPetRemaining` 并清空该玩家剩余备选池 |
| 主角第 1 回合抢跑、变阵期抢跑、或灵宠冒充主人提交唤灵 | `ValidateAction` / `ActionChecks` 强校验 `!prepActive`、`actor_type == PLAYER`、`IsSummonPetEnabled()`、`roundIndex >= 2`、`remaining > 0` |
| 被替下旧宠的「需施法者存活」光环在新宠复用 ID 后继续残留 | 新宠强制分配全新 `actor_id = kPetActorIdBase + nextPetActorIndex++`，旧宠从在场表移除，`FindActor(oldId) == nullptr` 自动终止旧宠绑定光环 |
| 主角先手唤宠后，旧宠同回合已提交技能被新宠执行或新宠同回合白嫖出手 | 新宠 `actor_id` 不在冻结的 `lastActionOrder` 中且不在 `pendingActions` 中，严格次轮起行动 |
| 主角提交唤灵但出手前被击杀/硬控，导致唯一一次机会白白蒸发 | 仅在 `ExecuteSummonPet` 实际落地时扣减 `summonPetRemaining`，提交期不扣次 |

---

## 五、步骤分析与执行步骤

1. **协议增量与双端同步**：更新 `proto/battle/{battle_data,player_battle}.proto`，运行 `dev_tools.ps1 -Command proto-gen-run`、Java `ContractSync.java`（仅 battle proto 同步或核验）及 Unity `gen_proto.ps1` / `gen_messageids.ps1`。
2. **C++ 权威服实现**：
   - `turn_battle_constants.h` 追加 `kPvpSummonPetUnlockRound = 2`；
   - `configured_battle_effects.cpp` 抽出单单位初始化 `InitializeConfiguredAbilitiesForActor`；
   - `turn_battle_engine.{h,cpp}` 实现备选池冻结、V3 亮牌衔接、`BATTLE_ACTION_SUMMON_PET` 校验与执行、`FillSummonStateForViewer`；
   - `battle_room_manager.cpp` 在 `RedactStateForViewer` 清空私有字段并在 `FillSelfItems` 回填；
   - `ranked_battle_test.cpp` 新增 V5 临阵唤灵专项单测（第 1 回合拒、第 2 回合补召/换宠、单局限 1 次、视角隐私、硬控不扣次、与 V3 换首发联动）。
3. **Java 独立服同构实现**：
   - `BattleConstants`、`BattleContext`、`ConfiguredEffects`、`ActionChecks`、`TurnBattleEngine`、`BattleViews`、`BattleRoomServiceImpl` 对齐 C++ 全部规则，补齐 `RankedBattleEngineTest` / `BattleViewsTest` 单测并更新 `PARITY.md`。
4. **Unity 美术导入与客户端接入**：
   - 从 `D:\work\image\designs\ranked-play-v4-tactical-ui-20261125\ui\` 导入 6 枚 `summon-*.png` + `.meta` 至 `Assets/Resources/UI/Ugui/RankedV4Tactical/ui/`（校验 SHA-256 与 GUID 零冲突）；
   - 更新 `SeasonEconomyUiArt.cs`、`BattleFidelityArt.cs`、`BattleClient.cs`、`BattleScreen.cs`（底部「临阵唤灵」按钮、弹层选宠玉匣、事件播报与自动收起）与 `BattleClientAutoBattleTests.cs`。
5. **全端编译与单测验收**：执行 C++ 静态预检 + MSBuild + GTest、Go `internal/ranked` 单测、Java Maven 单测与 Unity 全仓编译 + 离线 NUnit 单测。

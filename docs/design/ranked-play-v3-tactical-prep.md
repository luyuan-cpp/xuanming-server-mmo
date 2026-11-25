# 《论剑 / 人宠会武》V3 扩展设计：战前 15 秒明牌变阵（换赛季印 / 换首发宠）

> 日期：2026-11-25  
> 状态：V3 扩展实施设计（建立在 V1+V2 双端闭环基础上）  
> 适用仓库：`xuanming-server-mmo-cpp`（C++ `scene` / `battle` 权威源）、`mmorpg-client`（Unity 客户端与 UGUI）、`xuanming-server-mmo`（Java 独立版对账跟进）

---

## 1. 需求分析

论剑 V1+V2 已实现 1V1–5V5 跨规模单分排位、6 套 4 技能娱乐轮换预设、凝锋 +20%、战意渐盛、校准五段位与 6 胜「锋芒」永久称号、PVP 第 5 回合体面认输，以及防挂机三端闭环。

当前正式自养论剑（1V1–5V5）仍存在一个高频策略痛点：
- 匹配成局后直接盲打，玩家带进场的单枚赛季印（如破盾印 vs 减伤守印）和单只首发灵宠完全靠猜；
- 玩家培养的第 2～6 只备选灵宠（肉盾石灵、高速破甲金猊、毒法灵狐等）与当季免费的 4 枚赛季印，缺少“看对面阵容临场针对”的博弈高光时刻。

目标：在不增加任何肝度、不引入任何数值付费、不拉长单局节奏的前提下，为正式自养论剑与赛季战队锦标赛加入 **「战前 15 秒明牌变阵」**，让每场 PVP 开局前多出一轮高频、低心智负担、极强成就感的临场博弈。

---

## 2. 设计分析

### 2.1 生效范围与触发条件
- **仅对以下两种正式竞技自养局生效**：
  1. 正式人宠会武排位：`match_mode == MATCH_MODE_PVP_TEAM` 且 `ranked_context.season_id != 0` 且 `!ranked_context.use_free_preset`；
  2. 全服赛季战队锦标赛：`match_mode == MATCH_MODE_SEASON_TOURNAMENT` 且 `!ranked_context.use_free_preset`。
- **静默跳过兜底（零旧测回归）**：上述模式下，仅当全场至少存在一名非 AI 真人玩家携带可切换备选（`available_season_seals.size() > 1` 或 `standby_pets.size() > 0`）时才进入 15 秒战前变阵；否则直接进第 1 回合。
- **永不打扰模式**：PVE、首领试炼、瑞兽迎春、切磋、护卫救援、物法易势 5V5（免费预设）与帮战均不触发，零额外等待。

### 2.2 核心玩法规则（明牌初阵 + 同队互见 + 敌方盲选）
1. **入场明牌初阵**：建房成功进入战斗舞台，双方立即看到彼此入场初阵（敌我人物职业/等级/名字/外观 + 当前出战首发宠种类/名字/等级）。
2. **15 秒同步临场变阵**：
   - **换赛季印**：可在本季全部有效赛季印（S1 共 4 枚：`101 破阵锐印`、`102 破阵玄印`、`103 固阵守印`、`104 聚息援印`）中免费临场切换；仅切换机制槽 `equipment_effects.season_seal`，轻度气血/防御万分比仍只由赛季进度决定，不重算人物基础面板。
   - **换首发宠**：可在本人名下全部合规灵宠（当前首发宠 + 至多 5 只合规备选宠，合共至多 6 只；均满足主人等级限制、有效表行、满血满蓝隔离态）中一键切换首发。零宠或仅 1 只合规宠的玩家依然可以切换赛季印并点「锁定出战」。
3. **同队实时协同 / 对手严密盲选**：
   - 同队队友通过 `NotifyBattlePrepUpdate` 实时看到彼此的赛季印、首发宠与「已锁定」状态，3V3/5V5 可瞬间分工；
   - **对手与观众在 15 秒内绝对看不到己方实时改动与备选池**，彻底杜绝 0.1 秒卡点互切拉扯。
4. **提前开打与超时保底**：
   - 未锁定前可反复切换赛季印与首发宠；点击「锁定出战」后本局变阵锁死，不可再改；
   - 全场非 AI 真人均锁定，或 15 秒倒计时结束，服务端立刻执行 `FinalizePrepPhase()`：同步亮出双方最终赛季印与首发宠，录制回放开局起点，启动第 1 回合 25 秒行动窗，并广播正式开局帧（`prep_state.active == false`）。
   - 超时未操作保留原本携带的赛季印与首发宠，**绝不判逃跑、绝不卡房**。
5. **零场外污染**：变阵仅对本场临时快照生效，战后不改动场外默认赛季印与默认出战灵宠，也不回写任何宠物血蓝。

### 2.3 协议设计（复用战斗直连通道，零新配置表/零新 Tip）
- `proto/battle/battle_data.proto`：
  - `BattlePlayerSnapshot` 追加：
    - `repeated BattlePetSnapshot standby_pets = 26;`
    - `repeated SeasonSealEffect available_season_seals = 27;`
  - 新增紧凑战前视图消息：`BattlePrepSealOption`、`BattlePrepPetOption`、`BattlePrepPlayerState`、`BattlePrepState`。
- `proto/battle/player_battle.proto`：
  - `BattleStateS2C` 追加 `BattlePrepState prep_state = 26;`
  - `BattleClientPlayer` 新增 2 条战斗直连 RPC：
    - `UpdateBattlePrep (UpdateBattlePrepRequest) returns (UpdateBattlePrepResponse)`
    - `NotifyBattlePrepUpdate (BattlePrepUpdateS2C) returns (Empty)`（`CLIENT_RPC_PUSH`，仅同队单播）

---

## 3. 思想分析

- **引擎无时钟、房间持定时**：`TurnBattleEngine` 只维护确定性的 `prepActive`、白名单选项表、实时选择表与 `FinalizePrepPhase()` 纯状态迁移，100% 离线可测；15 秒截止时间戳与 `prepTimer` 由 `BattleRoomManager` 单 loop 线程托管，与 25 秒回合定时完全同构。
- **单一真相源与脱敏门禁**：所有下发状态（开局、补拉、观战、推送）统一经 `RedactStateForViewer` 清空 `prep_state`，再由 `FillSelfItems` 按 `viewerPlayerId` 精确投影「本人可选列表 + 本队实时状态」，从根源杜绝情报泄漏。
- **回放零格式破坏**：开启变阵的房间在 `FinalizePrepPhase()` 亮牌后再初始化 `BattleReplayRecorder`，确保回放从真实第 1 回合终态开演，不膨胀回放体积。

---

## 4. 漏洞分析与防线

| 潜在漏洞 | 防护措施 |
|---|---|
| 变阵期伪造 `SubmitBattleAction` / `SetAutoBattle` / `SubmitSurrenderVote` 抢跑或提前结算 | 引擎在 `prepActive == true` 时一律拒绝并返回 `kFeatureUnavailable` |
| 伪造对手 `pet_id` 或非法 `seal_id` 越权换装/换宠 | 服务端严格只认建房时冻结在 `prepOptionsByPlayer[playerId]` 内的白名单；`scene` 与 `battle` 双层复核归属、等级、`pet_cultivation` 与 `ValidSealEffect` |
| 对手抓包偷看己方备选宠或实时切换意图 | `RedactStateForViewer` 强制清空 `prep_state`；`NotifyBattlePrepUpdate` 仅按 `team_index` 同队单播；观众视图仅露倒计时与初阵 |
| 替换首发宠残留旧属性/旧被动或破坏阵位与试炼志事实 | 抽出统一 `BuildPetActor` 整只重建 `BattleActorState`（原地保阵位与局内号）、重跑宝宝被动初始化，并在 `FinalizePrepPhase()` 后冻结试炼志 `used_pet_table_id` / `used_season_seal_id` |
| 玩家变阵期断线或直连未建导致永久卡死 | 15 秒 `prepTimer` 兜底自动收口并经 `PushLobbyAnnouncement`（直连优先/大厅回落）发亮牌开战帧；重连 `GetBattleState` 无缝还原剩余倒计时与同队选择 |
| 双端并行开发期间强跑全量 `ContractSync` 破坏 Java 编译 | 不触碰 `.xlsx` / `Tip.xlsx` / `go/match`；权威 C++ 仓与 Unity 端先行完整闭环并跑绿测试，Java 仓待并行 GuildWar/PetTier/SkillLoadout 协议合入后再经 `ContractSync` 同步落账 |

---

## 5. 步骤分析与执行步骤

1. **协议与绑定**：更新 `proto/battle/battle_data.proto` 与 `proto/battle/player_battle.proto`，执行 `dev.bat proto` 生成 C++/Go/Unity 绑定与消息号。
2. **Scene 快照打包**：
   - `player_pet.{h,cpp}` 新增 `BuildStandbyBattleSnapshots(player, activePetId, maxCount, out)`，复用单只宝宝构建逻辑打包最多 5 只合规备选宠；
   - `player_season.{h,cpp}` 新增 `CollectAvailableSeals(nowMs, out)`，按表序打包当季全部合法赛季印；
   - `player_battle.cpp` 在正式论剑 / 锦标赛隔离快照中填充 `standby_pets`（满血满蓝）与 `available_season_seals`。
3. **Battle 引擎与房间**：
   - `turn_battle_constants.h` 增加 `kTacticalPrepTimeoutMs = 15000`；
   - `turn_battle_engine.{h,cpp}` 实现变阵初始化、`UpdateBattlePrep`、`FinalizePrepPhase`、`FillPrepStateForViewer` 及变阵期操作门禁；
   - `battle_room_manager.{h,cpp}` 与 `battle_client_edge.cpp` 接入 15 秒 `prepTimer`、`HandleUpdateBattlePrep`、同队单播推送、全员锁定/超时自动开打与重连补拉；
   - `ranked_battle_test.cpp` 补齐 V3 变阵专项单测。
4. **Unity 客户端与 UGUI**：
   - 同步 `gen_proto.ps1`、`gen_messageids.ps1`（白名单加入 `UpdateBattlePrep` / `NotifyBattlePrepUpdate`）；
   - `DirectRoutingBattleTransport.cs`、`RpcGuard.cs`、`BattleClient.cs` 接入 `IsInPrepPhase`、`CurrentPrepState`、`UpdateBattlePrep(...)`、`OnBattlePrepChanged`；
   - `BattleScreen.cs` 接入可操作战前 15 秒明牌变阵浮层（敌方初阵速览 + 赛季印切换 + 首发宠切换 + 队友就绪条 + 倒计时 + 「锁定出战」）；
   - `BattleClientAutoBattleTests.cs` 补齐变阵协议流转与状态机单测。
5. **验收与交接**：执行 C++ 静态门禁 + MSBuild + GTest 与 Unity 编译/NUnit 验收，并在聊天中直接交付完整自包含的 Codex UI 视觉精修提示词。

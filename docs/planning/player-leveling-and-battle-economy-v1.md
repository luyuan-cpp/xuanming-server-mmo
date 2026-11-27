# 《五行奇谈》G1：角色平滑升级曲线与全玩法经验/银两产出闭环设计（V1）

> 日期：2026-11-25
> 状态：**设计定稿落盘，待工作区并行会话提交清理后分批实施**（本轮仅落盘设计文档，未改动任何代码、协议、配表或并行会话未提交文件）
> 上位红线：[项目设定](project-vision.md) · [核心循环 V1](core-gameplay-loop.md) · [全局玩法统一收口总账](gameplay-unified-design-v1.md) · [经济与官方 7:3 总纲](equipment-economy-charter.md) · [角色属性加点系统](../design/player-attribute-allocation.md)
> 适用仓库：`xuanming-server-mmo-cpp`（C++ Scene/Battle + 共享表/协议）、`xuanming-server-mmo`（Java 独立服同步对齐）、`mmorpg-client`（Unity 非 UI 模型 + UGUI 展示）
>
> **2026-11-26 反「上班」升级覆盖（详见 [anti-chore-remediation-v1.md](anti-chore-remediation-v1.md)）**：常规战斗银两周上限已由“周一 05:00 清零”升级为**3 周滚存蓄水池**（每周一 05:00 补充 `+30,000` 全额软上限 / `+45,000` 硬封顶，未赚取额度滚存累计至多 **3 周 = 90,000 软上限 / 135,000 硬封顶**；新号首周维持 30k/45k 防脚本爆发，抗时钟回拨）。

---

## 一、需求分析

### 1.1 现役源码核实暴露的 4 个核心断层
直接核对三仓现役代码，当前角色成长与银两闭环存在四处阻塞“新手玩 30 分钟真正爽起来”的真实缺口：

1. **有累计经验无升级曲线，新号永远卡 1 级**：
   - `proto/common/component/actor_comp.proto` 的 `LevelComp` 已有 `level` 与 `total_exp`，`player_battle.cpp` 也已把 `settlement.exp_gain()` 累加到 `total_exp`；
   - 但 `player_level_rules.h`（第 18 行）明确写明升级曲线未接通，当前唯一改级入口是 dev/test 的 `GmSetPlayerLevel`，导致正常新号打完 20 场试炼仍是 1 级，永远摸不到 10/20/30/40 级技能（`skill.json`）、10 级宠物（`pet.json`）与 10–80 级装备档（`item.json`）。
2. **正式会武 / 娱乐轮换 / 五行幻境 / 锦标赛在结算层被资产隔离短路，经验和银两恒为 0**：
   - `turn_battle_engine.cpp` 的 `RollRewards()` 仅对普通 PVE 胜局生效，且显式写死 `settlement.set_gold_gain(0)`；
   - `player_battle.cpp`（第 2854 行）对 `competitive_ranked || use_free_preset || isFestivalBeast || isWuxingTower || isGuildWar` 统一走隔离早退，直接传空 `SettlementCurrencyGains{}` 且跳过了经验与银两入账。结果玩家打正式 1V1/3V3 会武、护卫救援、物法易势 5V5、锦标赛与五行幻境既不涨级也不产银两。
3. **银两消费点密集但常规核心战斗缺稳定、受控产出**：
   - 词条重铸（20 银两/次）、5 档品相打磨（20 银两/次）、普通宠物商店（`1,000 × quality × unlock_level`）、属性洗点、技能修习、神秘商人、帮战追杀令均消耗 `kCurrencyGold`（银两）；
   - 若无温和受控的战斗银两产出，免费玩家会陷入“有重铸砂却凑不齐银两”的卡手；若不做周平滑软上限，又会给 24 小时单人脚本留出刷爆银两通胀的口子。
4. **双服（C++ vs Java）结算口径存在现存分叉，且客户端缺经验条与升级即时爽感反馈**：
   - Java 仓 `BattleSettlementService.java`（第 149、225 行）当前仅读旧 `settlement.getGoldGain()`（漏读 `settlement.getCurrencyGainsList()`）并直接 `metrics.expIgnored()` 忽略经验，与 C++ 行为不一致；
   - Unity 客户端 `AttributePanel` 与 `CoreSettlementRewardSection` 当前只展示等级和材料，没有经验条、本局修为/银两入账明细、以及升级后「获得新属性点 → 一键前往加点」的正反馈闭环。

### 1.2 上位红线准入四问自检（[project-vision.md](project-vision.md)）
1. **普通零氪玩家**：每天玩 10–30 分钟或隔周上线，首场试炼就能从 1 级连升到 4 级并拿到属性点，半小时到 10 级，打齐首套 660 材料时自然到达 55–60 级、银两充裕不卡手，无每日打卡清零。**通过**。
2. **个人开发者收入**：本系统只产角色经验与绑定角色钱包的常规银两，不发灵石、不发可交易核心材料，反而让新号顺畅走到 10/30/60 级解锁技能、装备和外观展示，直接托举灵石百宝阁、永久衣柜与仙途轻享卡转化。**通过**。
3. **物价与长期循环**：战斗银两设「每周 30,000 全额 + 超额 20% 衰减 + 每周 45,000 硬封顶」（沿用 UTC+8 周一 05:00 切周契约），帮战 V2 银两不重复发放、切磋与瑞兽迎春恒为 0、PVP 负/平走四重防挂机门槛，彻底封死脚本无限印钞。**通过**。
4. **防骗与资产安全**：全部走既有 `CurrencySystem::AddCurrency` 与产出封禁检查（被封禁币种跳过且留痕审计），与收件箱永久水位、近期战果同帧同事务提交，零崩溃双发与客户端伪造空间。**通过**。

---

## 二、设计分析

### 2.1 权威策划表设计（两张新表，全数值进表、零硬编码）

#### 表 1：`PlayerLevelExp`（`data/schema/playerlevelexp_table.proto` + `data/PlayerLevelExp.xlsx`）
双端共享（`common` owner，客户端可直接读表或消费服务端权威下发字段；仍以服务端计算结果为准，客户端零公式分叉）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `level` | `uint32`（主键，1..85，连续无空洞） | 目标角色等级；`level = 1` 时 `required_total_exp` 必须为 `0` |
| `required_total_exp` | `uint64` | 达到该等级所需的**累计总经验**（严格单调递增：`req[L] > req[L-1]`） |

**1→85 级平滑曲线与里程碑锚点（锚定仓库真实解锁节点：宠 1/10、技能 1/10/20/30/40、装备 1/10/20/30/40/50/60/70/80、上限 85）**：

| 等级段 | 升级规则（单级增量） | 到达段末累计经验 | 折合普通胜场（1,000/场） | 玩家体验锚点 |
|---|---|---:|---:|---|
| `1 → 4` | `200 / 300 / 400` | `900` | **1 场（约 5 分钟）** | **首战即连升 3 级，立得 +15 属性点**，立刻体验加点变强 |
| `4 → 10` | `500 / 550 / 600 / 650 / 650 / 650` | `4,500` | **4–5 场（约 25–30 分钟）** | **首日半小时必到 10 级**：解锁 10 级装备/技能/宠物与商店买宠 |
| `10 → 30` | `600 + 18 × (L - 10)`（均 ~775/级） | `20,000` | **15–20 场（约 1.5–2.5 小时）** | 稳定解锁 20/30 级装备与中阶核心技能 |
| `30 → 60` | `1,000 + 46 × (L - 30)`（均 ~1,667/级） | `70,000` | **55–70 场（约 5–7 小时）** | **与 660 首套装备同步毕业**：40 级全部技能解锁完毕 |
| `60 → 80` | `3,200 + 400 × (L - 60)`（均 7,000/级） | `210,000` | 累计 `160–210` 场 | 阶段二周 150、试炼志 V2.1、赛季会武期间自然触达 80 级满档装 |
| `80 → 85` | `14k / 16k / 18k / 20k / 22k` | `300,000` | 累计 `230–300` 场 | 温和满级收官；满 85 后 `total_exp` 继续记录累计修为不封死 |

#### 表 2：`BattleProgressionReward`（`data/schema/battleprogressionreward_table.proto` + `data/BattleProgressionReward.xlsx`）
服务端专用（`server` owner，C++ 与 Java 通过 `ContractSync` 共用同一权威表），统一管理各战斗模式的基础经验、基础银两与周银两软/硬上限参数：

| `id` | `match_mode` | `difficulty` | `outcome_tier` | `exp_gain` | `silver_gain` | 适用玩法说明 |
|---:|---|---|---|---:|---:|---|
| `1` | `PVE_SOLO` / `PVE_TEAM` | `0/1`（普通/常规） | 胜 | `1,000` | `600` | 普通首领试炼 / 常规 PVE / 护卫救援（`use_free_preset` PVE 胜） |
| `2` | `PVE_SOLO` / `PVE_TEAM` | `2`（困难） | 胜 | `1,300` | `1,000` | 困难首领试炼胜（高于普通 30% 经验、67% 银两） |
| `3` | `PVP_TEAM` / `POLARITY_5V5` / `SEASON_TOURNAMENT` | `0` | 胜 | `1,300` | `1,000` | 正式会武 / 物法易势 5V5 / 群仙问道赛胜局 |
| `4` | `PVP_TEAM` / `POLARITY_5V5` / `SEASON_TOURNAMENT` | `0` | 达标平/负 | `800` | `500` | 须同时满足四重防挂机门槛（未逃离 + `≥3 回合` + `≥2 手操` + 有效贡献） |
| `5` | `WUXING_TOWER` | `0` | 胜 | `1,000` | `600` | 五行幻境周常爬塔单层首通/胜局 |
| `6` | `GUILD_WAR_PVP` | `0` | 胜 / 达标平负 | `800` / `500` | `0` | 帮战真人局：**银两由 `go/guild` 独立发，此处恒为 0 防双发** |
| `7` | `GUILD_WAR_SIEGE` | `0` | 胜 | `500` | `0` | 帮战守旗踢馆：**银两由 `go/guild` 独立发，此处恒为 0 防双发** |
| — | `FESTIVAL_BEAST` / `DUEL` / `CUSTOM_DUEL` / 逃离 / 0 回合 / `VOID` / PVE 负 | — | — | `0` | `0` | **显式零产出**（契约要求与防互刷） |

- **全局银两周软上限规则行（同表或单行 `BattleProgressionRule`）**：
  - `weekly_silver_soft_cap = 30,000`（每周全额池，UTC+8 周一 05:00 重置，直接复用 `PlayerEquipmentMaterialRewardSystem::ResolveWeek` 抗时钟回拨）；
  - `diminished_rate_permyriad = 2,000`（超软上限后按 `20%` 衰减发放，向下取整）；
  - `weekly_silver_hard_cap = 45,000`（触及周硬上限后银两 `+0`，但经验、材料、评分与试炼志照常 100% 发放）。
  - **跨边界精确拆分**：若本周已获 `29,600`、本场基准 `1,000`，则前 `400` 按 100% 算、后 `600` 按 20% 得 `120`，本场实得 `520` 并标记 `battle_silver_diminished = true`。

---

### 2.2 持久化设计（零新 DB 表、零新 DB 列，严格同事务原子提交）

1. **C++ 权威服（`xuanming-server-mmo-cpp`）**：
   - `LevelComp`（`proto/common/component/actor_comp.proto`）：**原样复用**现有 `level = 1` 与 `total_exp = 2`，不改表结构。
   - `ClientRequestLedgerComp`（`proto/common/component/client_request_ledger_comp.proto`，归属 `WALLET` 组，与 `CurrencyComp`、`EquipmentMaterialRewardLedger` 同行落盘）：追加 `BattleEconomyLedger battle_economy = 5;`：
     ```proto
     message BattleEconomyLedger {
       uint64 week_starts_at_ms = 1;       // 当前游戏周起点（UTC+8 周一 05:00，抗时钟回拨）
       uint64 battle_silver_this_week = 2; // 本周通过战斗结算累计入账的银两（含衰减段）
       uint64 battle_exp_this_week = 3;    // 本周战斗累计经验（仅统计监控用，不作硬截断）
     }
     ```
2. **Java 独立服（`xuanming-server-mmo`）**：
   - 在 `game-player-store/src/main/proto/game/storage/player_state.proto`：
     - 新增 `PlayerProgressionState progression = 23;`（保存 `uint64 total_exp = 1; uint64 week_starts_at_ms = 2; uint64 battle_silver_this_week = 3; uint64 battle_exp_this_week = 4;`），角色 `level` 继续单一事实源保存在 `player` 表列中；
     - 通过 `PersistentSchemaTool` 追加最新持久 schema 世代台账并补未知字段往返单测。

---

### 2.3 协议契约扩展（全量末尾追加，零新 RPC、零新消息号）

| Proto 文件 | 目标消息 | 追加字段（只增不改） | 用途 |
|---|---|---|---|
| `proto/scene/player_attribute.proto` | `AttributePanelInfo`（当前最大号 `12`） | `uint64 total_exp = 13;`<br>`uint64 current_level_exp = 14;`<br>`uint64 next_level_exp = 15;`<br>`bool is_max_level = 16;` | 服务端统一算出本级内已获经验与升级所需经验，客户端零公式推算 |
| `proto/common/component/player_battle_result_comp.proto` | `PlayerBattleResult`（当前最大号 `39`） | `uint32 level_before = 40;`<br>`uint32 level_after = 41;`<br>`uint64 total_exp_after = 42;`<br>`uint64 next_level_total_exp = 43;`<br>`uint64 battle_silver_weekly_used = 44;`<br>`uint64 battle_silver_weekly_soft_cap = 45;`<br>`bool battle_silver_diminished = 46;` | 持久战果恢复（重登/补弹）与现场结算同源展示 |
| `proto/battle/player_battle.proto` | `BattleEndS2C`（当前最大号 `13`） | `uint32 level_before = 14;`<br>`uint32 level_after = 15;`<br>`uint64 exp_gain = 16;`<br>`uint64 silver_gain = 17;`<br>`uint64 total_exp_after = 18;`<br>`uint64 next_level_total_exp = 19;`<br>`bool battle_silver_diminished = 20;` | 现场结算实时回显最终实得修为、银两与升级突破 |

---

### 2.4 服务端核心规则与调用链（C++ 与 Java 同构纯函数 + Scene 单一真相）

#### 1. 纯规则模块（零 ECS / 零 I/O，双服 100% 同构单测）
- **C++**：扩展 `cpp/libs/services/scene/player/system/player_level_rules.h`（及新增 `battle_progression_rules.h`）；
- **Java**：扩展 `game-scene/.../player/PlayerLevels.java`（及新增 `BattleProgressionRules.java`）。
- **核心纯函数**：
  1. `ResolveLevelForExp(storedLevel, currentTotalExp, expGain, expTable) -> LevelUpDelta`:
     - 溢出保护：`expGain > UINT64_MAX - currentTotalExp` → 返回错误拒绝应用（整笔延后）；
     - `newTotalExp = currentTotalExp + expGain`；
     - 若 `storedLevel >= kMaxLevel`：**`newStoredLevel = storedLevel`（绝不降级携带的超上限等级！）**；
     - 否则在有序 `expTable` 上二分查找满足 `required_total_exp <= newTotalExp` 的最大 `targetLevel`（封顶 `kMaxLevel`）；
     - **只升不降保护**：`newStoredLevel = std::max(storedLevel, targetLevel)`（保护 GM 调高过等级的角色，绝不因获得少量经验而回退等级）。
  2. `EvaluateBattleProgression(ledger, settlement, nowMs, explicitExp, explicitGold, table) -> ProgressionDelta`:
     - 优先识别显式排除模式（`VOID` / `0 回合` / `fled` / `isFestivalBeast` / `DUEL` / `CUSTOM_DUEL` / PVE 败 / PVP 负平未达四重门槛）→ 产出 `0 exp, 0 silver`；
     - 查表取得 `(baseExp, baseSilver)`；
     - 兼容历史 PVE `BattleRewardProfile`：`finalExp = max(explicitExp, baseExp)`；`rawSilver = (isGuildWar ? 0 : max(explicitGold, baseSilver))`；
     - 按 `ResolveWeek(nowMs)`（抗时钟回拨：`nowWeekStart > ledger.week_starts_at_ms` 时才重置周计数）对 `rawSilver` 施加软/硬上限拆分，产出 `creditedSilver` 与 `newLedgerCandidate`。

#### 2. `PlayerAttributeSystem::OnLevelChanged` 统一副作用收口
将原散落在 `PlayerAttributeSystem::GmSetLevel`（`player_attribute.cpp:1279–1286`）中的升级连带逻辑抽成共用函数 `ApplyLevelChangeEffects(player, oldLevel, newLevel, bool requestSaveNow)`：
1. `Recalculate(player, RecalcReason::kLevelChanged)`（提升上限并按增量补足当前血蓝）；
2. `PetSystem::RecalculateAll(player, kLevelChanged)`；
3. 按跨越的每一级（`oldLevel + 1 .. newLevel`）推进 `PlayerMissionSystem::OnCondition(kConditionLevelUp, lvl)`（确保 1 级直跳 4 级时不会漏掉“升到 2 级 / 3 级”的顺序任务）；
4. 仅当 `requestSaveNow == true`（GM 调用）时当场推面板/宠物列表并调 `RequestKeyProgressSave`；**当在战斗收件箱结算链路内调用时，待 SQL 存盘确认后在 `CompleteInboxSettlement` 统一推送 `BattleEndS2C`、`AttributePanelChangedS2C` 与 `PetListChangedS2C`**，严守“未确认落盘不对客户端提前公开收益”的铁律！

#### 3. 修复 `player_battle.cpp` 隔离分支与 Java `BattleSettlementService`
- **C++ `player_battle.cpp`**：
  - 将 `PrepareBattleProgression(...)`（含经验计算 + 银两预检 `CanCreditWholeBattleCurrency` + 封禁检查）提升到**隔离分支与常规分支之前**统一预检；
  - 隔离分支（`competitive_ranked || use_free_preset || isWuxingTower || isGuildWar`，不含 `isFestivalBeast`）仍严格隔离血蓝回写、道具消耗与宠物战损，但**原子入账本局经验（含升级重算）与银两（经 `CurrencySystem::AddCurrency(..., TX_BATTLE_REWARD, battleId)`）**，并将真实实得记入 `RecordBattleResult` 与 `BattleEndS2C`；
  - 常规分支按同一原语完成经验升级与银两入账，并在 `ApplySettlementToEntity` 内与收件箱水位同帧修改、同一次 `SavePlayer` 落盘。
  - **更新 `tools/scripts/tests/test_cpp_level_comp_reads.py`**：在 `MARKER_LABEL` 与 `ALLOWED_MARKERS` 中显式登记 `player_attribute.cpp`（或 `player_progression.cpp`）的 `直读存储等级:经验升级` 白名单。
- **Java `BattleSettlementService.java`**：
  - 修复第 149 行：合并读取 `settlement.getGoldGain()` 与 `settlement.getCurrencyGainsList()` 中 `currency_type == GOLD`，与 C++ 完全同源；
  - 接入 `PlayerProgressionService`，在隔离分支与常规分支同步完成经验升级、银两软上限入账、属性/宠物重算、任务触发与 `PARITY.md` 登记，彻底消除 `metrics.expIgnored()` 占位。

---

### 2.5 Unity 客户端非 UI 与 UGUI 设计（`mmorpg-client`，严守 WebGL2 + IL2CPP 双形态红线）

#### 1. 纯 C# 领域模型（零 `UnityEngine` 依赖，可独立跑 EditMode 单测）
- 新增 `Assets/Scripts/Game/CoreLoop/PlayerProgressionModels.cs`（含 `.meta`）：
  - `PlayerExpSnapshot`（不可变值对象：`uint Level`, `ulong TotalExp`, `ulong CurrentLevelExp`, `ulong NextLevelExp`, `bool IsMaxLevel`, `float Progress01`）；
  - `SettlementProgressionView`（从 `BattleEndS2C` 或恢复的 `PlayerBattleResult` 统一构建：`ulong ExpGain`, `ulong SilverGain`, `uint LevelBefore`, `uint LevelAfter`, `bool LeveledUp`, `uint NewAttributePointsHint`, `bool SilverDiminished`, `string SummaryLine`）；
  - 全程使用 `ulong` 承载经验与银两，严禁窄化为 `int`/`float` 运算。
- 扩展 `PlayerFeaturesClient` / `AttributeClient` 与 `CoreGameplayLoopClient`：缓存最新 `PlayerExpSnapshot`，在 `AttributePanelChangedS2C` 到达时经不可变事件广播给 HUD 与属性面板。

#### 2. UGUI 界面落点（只描述功能结构与状态，绝不定义美术风格或配色）
> 遵循 [project-vision.md](project-vision.md) §三.3 协作红线：本节与后续任何发给 Codex / GPT 的作图清单**只写做什么 UI、功能语义、状态、尺寸与切图槽位，严禁指定任何画风词、风格词、材质词、装饰画法或配色/色号**。

1. **结算页修为与银两反馈条**（嵌入 `mmorpg-client/Assets/Scripts/UI/Ugui/CoreLoop/CoreSettlementRewardSection.cs` 与 `mmorpg-client/Assets/Scripts/UI/Ugui/Ranked/RankedSettlementPanel.cs`）：
   - 当 `exp_gain > 0 || silver_gain > 0` 时，渲染紧凑横条显示 `+1,000 修为`、`+600 银两`（若 `silver_diminished == true` 附次级状态说明 `本周银两已入衰减期`）；
   - 当 `level_after > level_before` 时，展开升级高亮提示条：**`升级！Lv.{before} → Lv.{after} · 获得新属性点`**，右侧放次级按钮 **`前往加点`**（直跳 `AttributeUiRoot`，不阻挡关闭/再战）；
   - 若本局为瑞兽迎春或切磋（`exp_gain == 0 && silver_gain == 0`），自动收起该条，零空白占位。
2. **角色属性页经验条**（嵌入 `mmorpg-client/Assets/Scripts/UI/Ugui/Attribute/AttributePanel.cs` 左栏等级下方）：
   - 显示 `Lv.{level}` + 经验进度条 + `当前经验 / 升级所需`（满 85 级显示 `已达满级 · 累计修为 {total_exp}`）；
   - 仅读 `AttributePanelInfo` 权威字段，不做客户端查表推算。
3. **主城轻量角色状态条**（嵌入 `CoreMainCityHubPanel` 顶部/侧栏，适配 `Screen.safeArea` 与小游戏右上角 140px 胶囊避让）：
   - 展示 `Lv.{level}`、经验进度条与当前银两余额；未分配属性点 `> 0` 时在「角色/加点」入口显示轻提示角标（绝不弹窗打断、不造每日催办红点）。
4. **生命周期与双形态红线**：
   - 所有事件在 `OnDisable` / `OnDestroy` 对称退订，断线/切号清空旧快照并校核 `(PlayerId, ConnectionGeneration)`；
   - 纯主线程更新，零 `Task.Run` / `Thread` / `System.Reflection.Emit`。

*(可选新素材槽位：G1 首版直接复用客户端已有 UGUI 控件即可运行，不阻塞联调；如后续确需让 Codex 补专属底图，仅按功能需求给无风格词的纯清单——如升级提示底条 `level_up_banner.png` 与经验条滑轨/填充图，按既有可选槽位加载。)*

---

## 三、思想分析

1. **为什么坚持“零新 DB 表、零新 DB 列、零新 RPC”？**
   `LevelComp` 已经持久化了 `level` 和 `total_exp`，`ClientRequestLedgerComp` 已经处于 `WALLET` 存盘组，结算与面板已经存在 `BattleEndS2C`、`PlayerBattleResult` 与 `AttributePanelInfo`。把周银两账本与升级字段直接追加在现有载体上，不仅消灭了 `go/db` DDL 迁移风险与新消息号限流配置，更在物理层保证了「等级 + 经验 + 银两 + 材料 + 近期战果 + 收件箱水位」**100% 落入同一笔原子存盘事务**。
2. **为什么把核心战斗的经验/银两计算收口在 `Scene`，而不是分散在 `TurnBattleEngine`、`go/match`、`go/guild`？**
   战斗服（`battle`）是无状态的回合计算器，不知道玩家本周银两软上限水位、不知道账号/角色封禁状态、也无法处理跨节点重连延迟结算的实时周切。沿用已验证稳定的 `PlayerEquipmentMaterialRewardSystem` 架构范式：**战斗服只冻结客观战斗事实（模式、难度、胜负、回合数、手操回合、有效贡献），由 `Scene` 在收件箱首次应用时单一真相裁决经验升级与银两入账**，代码路径最短、防刷最严、双服最好对齐。
3. **为什么用「周平滑软上限 30,000 + 20% 衰减 + 45,000 硬顶」而不是“每日银两上限”？**
   彻底贯彻 [project-vision.md](project-vision.md) 的“真不肝”红线：周末集中玩 3 小时的上班族能一次拿满全周银两，不因周一到周五没上线而亏欠；同时 45,000 的周硬上限让 24 小时挂机脚本单号每周最多只能印出固定封顶银两，守住经济通胀底线。

---

## 四、漏洞分析与权威防线清单

| # | 潜在漏洞 / 边界场景 | 风险后果 | 权威硬防线 |
|---|---|---|---|
| **V1** | 混版本回滚存档携带超上限等级（如 `stored_level = 90 > kMaxLevel = 85`）获得新经验 | 若按 `CappedLevel + 1` 或直接 `set_level(targetLevel)` 会永久把 90 级降回 85 级，破坏存档 | `ResolveLevelForExp` 显式校验 `if (storedLevel >= kMaxLevel) return storedLevel;` 且始终取 `max(storedLevel, targetLevel)`；同时在 `test_cpp_level_comp_reads.py` 注册白名单守卫 |
| **V2** | 开发/测试期 GM 调到 60 级但 `total_exp = 0`，随后打赢一场战斗 | 若按 `total_exp` 反算覆盖等级，会把 60 级角色强行打回 4 级 | 升级单调不减约束 `newStoredLevel = max(storedLevel, targetLevel)`，永不降级；并补充专门单元测试验证 `GmSetLevel(60) + GainExp(1000)` 保持 60 级 |
| **V3** | 新增直读 `LevelComp.level()` 触发 CI 契约测试拦截 | `test_cpp_level_comp_reads.py` 报错阻断构建与提交 | 严格只在经验升级唯一写入口直读，同行标注 `// 直读存储等级:经验升级` 并同步更新 `test_cpp_level_comp_reads.py` 的 `MARKER_LABEL`、`ALLOWED_MARKERS` 及脚本自测用例 |
| **V4** | 一次获得大额经验跨多级（如 1 级首战连跳至 4 级） | 只通知 `level = 4` 会漏掉“升到 2 级 / 3 级”的顺序任务条件 | `ApplyLevelChangeEffects` 按 `for (lvl = oldLevel + 1; lvl <= newLevel; ++lvl)` 逐级触发 `kConditionLevelUp`（或确认任务比较语义） |
| **V5** | 隔离模式（会武/娱乐场/爬塔）结算触发升级时写坏玩家局外血蓝 | 隔离模式严禁把局内残血写回局外；若顺序错乱会把局外血蓝改错 | 隔离分支只调 `Recalculate(kLevelChanged)`（按新旧上限增量上调当前血蓝，不碰 `settlement.health()`/`mana()`），零战损契约 100% 保持 |
| **V6** | 存档提交失败或场景崩溃回滚，却已提前向客户端推送升级与银两提示 | 客户端弹出「升到 4 级」，重登后回到 1 级造成假成功信任事故 | `ApplySettlementToEntity` 期间只改内存组件不推客户端消息，统一由 `CompleteInboxSettlement` 在 TiDB/MySQL 存盘确认后发布 `BattleEndS2C` + `AttributePanelChangedS2C` + `PetListChangedS2C` |
| **V7** | 帮战 V2 同时经 `go/guild` 和本结算通道发双份银两 | 帮战单局变成 4,000+ 银两且绕过 30 分钟同对手递减 | `BattleProgressionReward` 中 `GUILD_WAR_PVP` 与 `GUILD_WAR_SIEGE` 的 `silver_gain` 强制配置为 `0` 并在表校验中硬断言 `== 0` |
| **V8** | 玩家建小号约战切磋或反复打瑞兽迎春刷经验/银两 | 零成本无限刷满 85 级或绕过瑞兽活动零掉落契约 | `DUEL`、`CUSTOM_DUEL`、`FESTIVAL_BEAST`、逃离、0 回合、`VOID`、PVE 失败、未达四重门槛的 PVP 负/平在纯规则入口首行即短路返回 `{0, 0}` |
| **V9** | 玩家银两达到货币封顶、溢出或被 GM 产出封禁 | 整笔战斗结算卡死永久 DEFER，或封禁账号继续领银两 | 严格复用既有 `CanCreditWholeBattleCurrency` 与 `IsSettlementCurrencyGainBlocked`：封禁仅跳过银两并写结构化 WARN 日志、其余经验/材料正常入账；溢出整笔 DEFER 不丢进度 |
| **V10** | 时钟短暂回拨导致本周银两软上限被意外重置 | 节点 NTP 抖动触发多刷一轮 30,000 银两 | 沿用 `PlayerEquipmentMaterialRewardSystem::ResolveWeek` 的单调比较 `if (weekStart > ledger.week_starts_at_ms)` 才重置，回拨不清空已用额 |
| **V11** | `PlayerLevelExp` 策划热更出错（如等级断号、非单调、`level=1` 门槛非 0） | 导致玩家经验异常跳级或热更崩溃 | 在 `SceneNodeHooks::TableLoadHandler::Validate`（C++）与 `TableValidation`（Java）注册强校验：必须覆盖 `1..kMaxLevel`、`req[1]==0`、严格递增；校验失败拒绝热更（fail-closed） |
| **V12** | 双服（C++ vs Java）行为漂移 | Java 端仍忽略经验、漏读 `currency_gains` 导致双服结果不一致 | 同批经 `ContractSync.java` 同步两张新表与三处 proto 字段，修复 `BattleSettlementService.java` 银两汇总并补齐同口径边界单测与 `PARITY.md` 登记 |

---

## 五、步骤分析（并行会话提交后分 3 批落地，每批严控 `< 20` 文件）

| 批次 | 目标与涉及仓库 | 预计手改文件数 | 前置条件 |
|---|---|---:|---|
| **批次 1 · 表、协议与 C++ 核心闭环** | `xuanming-server-mmo-cpp`：新增 `PlayerLevelExp` + `BattleProgressionReward` 表与 schema、追加 3 处 proto 字段、实现 `player_level_rules.h` / `battle_progression_rules.h` / `player_attribute.cpp` / `player_battle.cpp` / `test_cpp_level_comp_reads.py` 与 GoogleTest 单测 | ~14 文件 | 并行会话已提交/清理共享 `proto/`、`data/`、`player_battle.cpp` |
| **批次 2 · Java 独立服全量对齐** | `xuanming-server-mmo`：`ContractSync` 同步契约、`player_state.proto` + `PersistentSchemaTool` 新世代、`PlayerLevels` / `BattleProgressionRules` / `PlayerProgressionService` / `AttributeService` / `BattleSettlementService` 修复 + Maven 单元测试 + 更新 `PARITY.md` | ~11 文件 | 批次 1 已通过导表、C++ 预检、MSBuild 与单测 |
| **批次 3 · Unity 客户端非 UI + UGUI** | `mmorpg-client`：`.\gen.bat client` 同步表与协议、新增 `PlayerProgressionModels.cs`（+ EditMode 单测）、扩展 `PlayerFeaturesClient` / `CoreGameplayLoopClient` / `CoreSettlementRewardSection` / `RankedSettlementPanel` / `AttributePanel` / `CoreMainCityHubPanel` 并跑 `client_compile_check.ps1` | ~10 文件 | 批次 1 导表与 proto 已就绪，且客户端并行会话已收口 |

---

## 六、执行步骤与验收标准（开工操作手册）

### 6.1 批次 1 执行命令（`xuanming-server-mmo-cpp`）
1. **配表与协议生成**：
   - 新增 `data/schema/playerlevelexp_table.proto`、`data/schema/battleprogressionreward_table.proto` 及对应 `.xlsx`；
   - 更新 `proto/common/component/client_request_ledger_comp.proto`、`proto/scene/player_attribute.proto`、`proto/common/component/player_battle_result_comp.proto`、`proto/battle/player_battle.proto`；
   - 运行 `dev.bat gen` 与 `python tools/data_table_exporter/tools/gen_schema_index.py`。
2. **C++ 实现与静态预检**：
   - 完成规则头、场景升级副作用统一收口、战斗结算隔离/常规双分支接入、`CompleteInboxSettlement` 落盘后推面板/宠表及单测；
   - 必跑两项只读静态预检：
     ```powershell
     python tools/scripts/tests/test_cpp_no_raw_pointer_member.py
     python tools/scripts/tests/test_cpp_level_comp_reads.py -v
     ```
3. **串行编译与单测（必须 `/m:1`）**：
   - 按 `table` → `proto` → `scene`（lib）→ `scene`（node）→ `bag_test` → `turn_battle_engine_test` 顺序串行构建并运行：
     - 验证 1 级首战普通首领直跳 4 级（`total_exp=1000`, `level=4`, `remaining_points=20`）；
     - 验证 1V1 会武胜局（隔离模式）正常得 `1,300 EXP + 1,000 银两` 且场外血蓝零战损；
     - 验证未达防挂机门槛的 PVP 负局、瑞兽迎春、切磋均给 `0 EXP, 0 银两`；
     - 验证帮战 V2 仅发经验、银两为 `0`（不与 `go/guild` 双发）；
     - 验证跨 30,000 周软上限（按 20% 精确拆分）与 45,000 周硬顶截断、跨周一 05:00 重置及抗时钟回拨；
     - 验证 `stored_level = 90` 混版本存档与 `GmSetLevel(60)` 账号获得经验后均**绝不降级**；
     - 验证重投同一 `battle_id` 幂等不双发经验或银两。

### 6.2 批次 2 执行命令（`xuanming-server-mmo`）
1. 同步契约并检查：
   ```powershell
   java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp
   java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp --check
   ```
2. 登记 `player_state.proto` 新世代台账，完成 Java 逻辑与同口径边界单测，更新 `PARITY.md`；
3. 运行全量构建与定向测试：
   ```powershell
   ./mvnw -B clean install -DskipTests
   ./mvnw -B -pl game-player-store,game-scene -am test
   ```

### 6.3 批次 3 执行命令（`mmorpg-client`）
1. 同步表与协议并做编译体检：
   ```powershell
   .\gen.bat client
   powershell -NoProfile -ExecutionPolicy Bypass -File tools/client_compile_check.ps1
   ```
2. 运行纯 C# EditMode 单元测试（验证 `PlayerProgressionModels` 经验进度、升级属性点提示、银两衰减提示、`ulong` 大数无溢出、切号隔离）并在编辑器核查 `1920×1080`、异形屏 `Screen.safeArea` 与小游戏右上角胶囊避让。

### 6.4 索引补刷说明
因当前工作区存在并行会话未提交修改 `docs/README.md` 与 `docs/PROGRESS.md`，本轮未触碰上述两份文件；待并行会话提交后，执行一次：
```powershell
python tools/scripts/gen_docs_index.py
```
即可将本文自动纳入 `docs/README.md` 索引。

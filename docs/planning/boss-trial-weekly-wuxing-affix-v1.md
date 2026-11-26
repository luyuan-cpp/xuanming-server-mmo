# 首领试炼「周常五行词缀」V1 设计文档

- **日期**：2026-11-25
- **适用仓**：`xuanming-server-mmo-cpp`（C++ 战斗引擎 & 共享 proto）、`xuanming-server-mmo`（Java 战斗引擎 & `PARITY.md`）、`mmorpg-client`（Unity 备战卡片 & 战斗顶栏只读 HUD）
- **基线依赖**：[boss-trial-planning-ui.md](boss-trial-planning-ui.md)、[core-gameplay-loop.md](core-gameplay-loop.md)、[project-vision.md](project-vision.md)

---

## 0. 立项四问护栏（`project-vision.md`）

1. **增加的是玩法，还是只增加肝度？**
   - 纯玩法策略变化，**零新增肝度**：不新增周常任务/体力/次数门槛，与既有 10/15 份装备材料奖励池 100% 共享，打赢同样局数即可拿满本周材料。
2. **让小额买爽还是让零氪变痛？**
   - **零付费点、零门槛**：不售词缀重置券/抗性符/增益药；5 条周常词缀均为「正向激励 + 反向考验」对称设计，任何职业与单人带宠都能打通，只是鼓励玩家按本周词缀在自己已经免费养成的 3 套人物技能方案 + 3 套灵宠词条方案 + 5 套免费阵法之间切换。
3. **有没有给交易、市场或账号流动埋雷？**
   - **零资产表/账本改动**：完全不改奖励配额、绑定状态、掉落或经济流向。
4. **有没有破坏既有稳定核心循环？**
   - **零侵入非首领试炼模式**：仅在真实首领试炼（`IsBossTrialKind(boss_trial_kind) && !has_tower_context()`）生效；常规 PVE、五行幻境爬塔（`tower_context` 复用 101–107 副本）、护卫救援（109）、物法易势 5V5（110）、瑞兽迎春（111）、论剑排位、帮会帮战、赛季锦标赛、道友切磋全部保持 `boss_weekly_affix_id == 0`，逐字节零漂移。

---

## 1. 需求分析

- **核心问题**：玩家养成 3 套人物技能预设与 3 套出战宝宝方案后，首领试炼（破阵回春/护势/灵障/玄毒/双卫同阵）每场机制相对固定，容易一套通用方案打整个赛季，副套与辅助/驱散/蓄力/追击流派利用率偏低。
- **目标**：
  1. 每周（UTC+8 周一 `05:00` 切周，与全服游戏周历基准 `FIRST_MONDAY_05_UTC8_MS = 334,800,000` 一致）为首领试炼确定性轮换 **1 条「五行周常词缀」**（5 周一轮：`1 金 / 2 木 / 3 水 / 4 火 / 5 土`）。
  2. 每条词缀严格包含 **1 条正向激励**（高爽感回馈，对应盘活某一类人物/宝宝流派）+ **1 条反向考验**（温和可读的战术考题，复用既有护盾/蓄势/破绽/二阶段机制，绝不劝退单系玩家）。
  3. 局内权威由战斗引擎从建房原件 `CreateBattleRequest` 确定性冻结并写入 `BattleStateS2C.boss_weekly_affix_id`，确保跨周一 `05:00` 边界、断线重连、观战、回放与 C++/Java 双服逐回合完全一致。
  4. 客户端在首领试炼备战页（`BossTrialPrepPanel`）展示本周词缀横条（五行元素 + 正向激励 + 反向考验 + 流派建议 + 距下周一 `05:00` 轮换倒计时），并在战斗顶栏（`BattleCoreHudOverlay`）展示只读周常词缀标签与触发播报。

---

## 2. 设计分析

### 2.1 周次映射与确定性冻结

- 共享常量（C++ `boss_trial_weekly_affix_rules.h`、Java `BossTrialWeeklyAffixRules.java`、Unity `CoreGameplayLoopModels.cs` 逐字一致）：
  - `kDayMs = 86_400_000`
  - `kWeekMs = 7 * kDayMs = 604_800_000`
  - `kFirstMonday05Utc8Ms = 3 * kDayMs + 21 * 3_600_000 = 334_800_000`（`1970-01-05 05:00:00 UTC+8`）
  - 有效时间戳（`nowMs >= kFirstMonday05Utc8Ms`）下的周常词缀 ID：
    $$\text{affixId} = \left(\left\lfloor \frac{\text{nowMs} - 334{,}800{,}000}{604{,}800{,}000} \right\rfloor \bmod 5\right) + 1 \in \{1,2,3,4,5\}$$
- **建房冻结策略（零改 match 服务、零破存量单测）**：
  - `CreateBattleRequest` 新增可选字段 `uint32 boss_weekly_affix_id = 22`（`0` = 自动推导；`1..5` = 显式指定用于测试/活动固定；`> 5` 建房拒绝防脏输入）。
  - `BattleStateS2C` 新增权威回显字段 `uint32 boss_weekly_affix_id = 34`（非首领试炼或未开启时恒为 `0`）。
  - 引擎 `Initialize` 解析顺序：
    1. 若 `!IsBossTrialKind(bossTrialKind) || hasTowerContext` → 恒为 `0`。
    2. 若 `request.boss_weekly_affix_id` 在 `1..5` → 冻结为该显式值（`> 5` 拒绝建房）。
    3. 否则若 `request.created_at_ms >= kFirstMonday05Utc8Ms` → 按 `created_at_ms` 确定性推导 `1..5`（生产对局 Go `match` / Java `game-match` 均在建房原件冻结 `created_at_ms`）。
    4. 否则（历史单测默认 `created_at_ms == 0`）→ `0`（不激活，存量所有单测与黄金回放逐字节不变）。

### 2.2 五周五行词缀定义（正向激励 + 反向考验）

| ID | 五行 / 名称 | 正向激励（盘活流派） | 反向考验（温和考题） | 建议切换方案 |
|---|---|---|---|---|
| `1` | **金 · 锋芒破阵周** | 攻击处于「破绽」状态的敌方时，我方暴击率 `+15%`，且单点/主目标聚火直伤 `+10%`。 | 敌方全员开局获得 `8%` 最大气血金甲护盾（先吸收伤害再扣血）。 | 破绽爆发/破甲人物 + 碎甲单点宠 |
| `2` | **木 · 灵脉涤尘周** | 我方成功驱散敌方增益（含护势/蓄势）或净化友方减益（含破军破甲）时，施法者立刻回复 `12%` 最大法力（每人每回合限 1 次）并令全队本回合与下回合全伤害 `+10%`。 | 每逢第 `2/5/8/11…` 回合末，存活且未被控、未受断疗压制的首领额外回复 `4%` 最大气血。 | 驱散 `A34` / 断脉 `A35` / 净化辅助 + 灵愈宠 |
| `3` | **水 · 蓄势聚灵周** | 我方明牌蓄力技（`PrepareCast` 兑现）与持续毒火（`Periodic` 周期与引爆）伤害 `+20%`；释放带蓄力或持续毒火效果的技能法力消耗 `-20%`（原耗蓝 > 0 至少 1 点）。 | 敌方首领每逢偶数回合末（未被控锁时）额外凝聚 `+1` 层「蓄势」（沿用 3 层上限、可驱散）。 | 蓄力爆发（`E01`/`E02`）/ 毒火持续流 + 驱散副手 |
| `4` | **火 · 烈焰连携周** | 我方出战灵宠直接攻击与追击技（`FollowUp`/`PURSUED`）伤害 `+18%`；我方攻击命中「破绽」敌方时额外附加目标 `3%` 最大气血烈焰灼伤（单次封顶 `1200`）。 | 首领进入二阶段「决战姿态（破釜）」的血线门槛由 `≤ 50%` 提前至 `≤ 60%`。 | 猛攻/连击灵宠预设 + 追击爆发人物 |
| `5` | **土 · 守御反震周** | 我方成功化劲化解首领慢速重劈（触发 `kBossMechanicCleaveParryExpose`）时，我方全体存活单位立刻获得 `8%` 最大气血厚土护盾，且全队下一回合全伤害 `+12%`。 | 首领慢速重劈与灵障强法重击基础伤害额外 `+12%`。 | 守御/双罩/反震人物 + 护主铁壁宠 |

### 2.3 机制事件码与定点数口径

- 新增 `BATTLE_EVENT_BOSS_MECHANIC` 子事件码 `kBossMechanicWeeklyAffixTrigger = 10`（`value = bossWeeklyAffixId * 100000 + detailValue` 或直传关键数值：木周回蓝传实际回蓝量、土周加盾传人数、金/水/火由对应伤害/回血/蓄势事件与 HUD 清晰呈现；为避免编码位魔数，明确：`mechanic_kind = 10` 为「周常词缀触发」，`value` 约定如下：
  - 木周正向回蓝振奋：`source = 施法者`，`target = 施法者`，`value = 实际回蓝量`（附带标准 `BATTLE_EVENT_MANA`）；
  - 木周反向生机反哺：`source = 首领`，`target = 首领`，`value = 实际回血量`（附带标准 `BATTLE_EVENT_HEAL`）；
  - 土周正向化劲护盾：`source = 首领`，`target = 首领`，`value = 获盾友方人数`（每名友方附带标准 `BATTLE_EVENT_SHIELD`）。
  - 水周反向沧澜蓄势：直接复用既有 `kBossMechanicMomentumGain (6)`，`value = 当前蓄势层数`，客户端零新增 UI 即可立即亮起蓄势角标！
  - 火周反向提前破釜：直接复用既有 `kBossMechanicPhaseTwoEnrage (5)`，`value = 2`。
  - 金周反向开局金甲护盾：直接写入 `leylineEarthShieldByActor`（C++）/ `BattleUnit.addTournamentEarthShield`（Java），受击自动走既有护盾吸收！
- 全部缩放采用无符号整数定点数（`* (100 + pct) / 100`），C++ 与 Java 逐项一致、零跨平台浮点漂移。

---

## 3. 思想分析

1. **做减法、借现有舞台**：不发明新的副货币、不造新的周常打卡、不添复杂新图标；所有正向与反向都直接挂接首领试炼 V2 已经让玩家看懂的四个核心动词——**破绽、驱散、蓄力/毒火、化劲防御**。
2. **备战期决策优先**：在 `BossTrialPrepPanel` 顶部一眼看清本周五行词缀与推荐流派，玩家进本前顺手切换技能方案/灵宠方案，把「养成多套方案」真正转化为每周 2 分钟就能感受到的新鲜感。
3. **双端同源单点真源**：周一 `05:00` 锚点与 `1..5` 映射全部写成零依赖纯规则（C++ header / Java final class / Unity pure C#），客户端战前仅用服务端校准时间预览本周词缀，进战后 100% 只认权威 `BattleStateS2C.boss_weekly_affix_id`。

---

## 4. 漏洞分析与防线

| 潜在漏洞 | 防线设计 |
|---|---|
| **跨周一 05:00 切周导致战中重连/回放词缀跳变** | 词缀 ID 在 `TurnBattleEngine::Initialize` 时由冻结的 `CreateBattleRequest.created_at_ms`（或显式 `boss_weekly_affix_id`）一次性算出并固化，整局永不重算，回放与重连完全一致。 |
| **五行幻境爬塔复用 101–107 副本导致词缀误叠** | 强制门禁 `IsBossTrialKind(bossTrialKind) && !hasTowerContext`；爬塔局 `bossWeeklyAffixId` 恒为 `0`。 |
| **木周群驱散/多目标驱散单回合无限刷满蓝** | 记录 `weeklyAffixWoodManaUsedActors`（每回合清空），同一施法者每回合至多触发 1 次 `12%` 驱散回蓝，杜绝多目标技能单回合瞬间回满 60% 蓝量。 |
| **火周破绽灼伤触发二段护盾/反击死循环或秒杀高血首领** | 灼伤采用 `min(1200, max(1, target.max_health * 3 / 100))` 封顶并在 `ApplyEncounterDamageModifiers` 后/扣血路径中作为附加直伤一次性计入，不重入 `ResolveConfiguredDamage` 或受击监听。 |
| **水周 `-20%` 耗蓝导致技能耗蓝被压成 0 无限白嫖** | 规定原耗蓝 `> 0` 时减免后 `max(1, cost * 80 / 100)`，保留最低 1 点法力校验；且 `CheckSkillCost` 与 `ConsumeSkillMana` 共用同一份 `SkillManaCost` 算法，杜绝「校验放行但扣蓝不足」错位。 |
| **非法 `boss_weekly_affix_id > 5` 注入建房** | C++ `Initialize` 与 Java `TurnBattleEngine.start` 均对 `request.boss_weekly_affix_id > 5` 返回建房拒绝（fail-closed）。 |
| **影响存量单测与历史黄金回放** | 历史单测未填 `created_at_ms`（默认为 `0`）且未填 `boss_weekly_affix_id`（默认为 `0`），解析结果为 `0`（关闭），保证存量数百条测试零漂移。 |

---

## 5. 步骤分析与执行步骤

1. **契约层（`xuanming-server-mmo-cpp`）**：
   - `proto/battle/battle_node.proto`：`CreateBattleRequest` 追加 `uint32 boss_weekly_affix_id = 22;`。
   - `proto/battle/player_battle.proto`：`BattleStateS2C` 追加 `uint32 boss_weekly_affix_id = 34;`。
2. **C++ 纯规则与战斗引擎（`xuanming-server-mmo-cpp`）**：
   - 新增 `cpp/libs/services/battle/constants/boss_trial_weekly_affix_rules.h`（周历映射、5 词缀常数、零裸指针）。
   - `turn_battle_constants.h` 追加 `kBossMechanicWeeklyAffixTrigger = 10`。
   - `turn_battle_engine.{h,cpp}` & `configured_battle_effects.cpp` 接入 5 词缀正向激励与反向考验、回合清理与 `BuildStateSnapshot` 回填。
   - 新增 GoogleTest `BossTrialWeeklyAffixTest` 覆盖全部 5 条词缀正反机制、爬塔/非试炼隔离、跨周冻结确定性与非法 ID 拒绝。
3. **Java 对等实现（`xuanming-server-mmo`）**：
   - 同步 proto 契约，新增 `BossTrialWeeklyAffixRules.java`，在 `BattleConstants.java`、`BattleUnit.java`、`BattleContext.java`、`EncounterMechanics.java`、`ConfiguredBattle.java`、`ConfiguredEffects.java`、`ConfiguredImpact.java`、`ActionChecks.java`、`ClassSkillExecution.java`、`TurnBattleEngine.java` 完成 1:1 对等接入。
   - 新增 `BossTrialWeeklyAffixBattleEngineTest.java` 逐项验证 5 条词缀与隔离性，更新 `PARITY.md`。
4. **Unity 客户端接入（`mmorpg-client`）**：
   - 同步 proto 生成与消息号。
   - `CoreGameplayLoopModels.cs` 新增纯 C# `BossTrialWeeklyAffixCatalog` / `BossTrialWeeklyAffixView`，`BattleCoreHudView` 新增 `BossWeeklyAffixId` 及展示文案，`DescribeBossMechanicEvent` 新增 `mechanic_kind == 10` 播报。
   - `BossTrialPrepPanel.cs` 在简介下方紧凑展示本周五行词缀卡（复用既有玉简条与五行色，零重画、零遮挡 5 张首领卡）。
   - `BattleCoreHudOverlay.cs` 在首领试炼顶栏状态条并入本周词缀徽签（`raycastTarget = false`，遵守 `Screen.safeArea` 与小游戏右上胶囊避让）。
   - `CoreGameplayLoopTests.cs` 增补周历锚点、5 词缀目录与 HUD 派生单测。
5. **三端生成、编译与测试回归**：
   - 交由低档执行 worker 运行 `proto-gen-run`、C++ 裸指针/英文日志检查与 `/m:1` 编译测试、Java `ContractSync` 与 Maven 测试、Unity `gen_proto.ps1` / `gen_messageids.ps1` / `client_compile_check.ps1` 与 EditMode NUnit。

---

## 6. 给异机 Codex 的可选美术增量说明（非阻断）

当前客户端已通过既有 `hud-strip` / 五行色标与文案完整跑通备战与战斗展示，**无需等待美术即可联机上线**。若后续希望进一步提升视觉辨识度，仅需可选补充 5 枚 64×64 透明底周常五行词缀角标至 `Assets/Resources/UI/Ugui/CoreLoopV2/`：
- `icon_weekly_affix_metal.png`（金·锋芒破阵）
- `icon_weekly_affix_wood.png`（木·灵脉涤尘）
- `icon_weekly_affix_water.png`（水·蓄势聚灵）
- `icon_weekly_affix_fire.png`（火·烈焰连携）
- `icon_weekly_affix_earth.png`（土·守御反震）

# 率土式叠加成长战法 V2 扩展设计（《兵法叠势卷》6 项永久成长被动）

> 日期：2026-11-26  
> 适用仓库：权威服务端 `xuanming-server-mmo-cpp`（C++ 战斗引擎 + 权威配表）、Java 独立服务端 `xuanming-server-mmo`（`game-battle-engine` + 配置表同步）、Unity 客户端 `mmorpg-client`（配置表与图标键接入 + 异机 Codex 图标交接）  
> 上位衔接：[装备品相与率土式制书 V1](equipment-quality-and-skill-crafting-v1.md) · [战斗战法深度与宝宝养成全量扩展](battle-skill-and-pet-cultivation-expansion.md) · [技能总览](skill-planning/README.md) · [项目设定](project-vision.md)

---

## 1. 需求分析

### 1.1 背景与空白生态位

此前卷 3《兵法成长卷》（`scroll_id = 3`）已落地 3 项每回合自动叠层至战斗结束的人物被动（`12020 愈战愈勇`、`12021 深谋远虑`、`12022 步步为营`），但在实际构筑生态中仍存在两类明显缺口：

1. **缺少“行为/交互驱动”的率土式叠层战法**：纯回合开始自动 `+1` 层已有物/法/防三件套，若继续堆纯时间白板叠层容易同质化；真正能拉开流派博弈与反制深度的是“靠受击、主动命中或持续伤害触发叠层至战斗结束”的事件型成长战法：
   - 反击/援护流（`P09 反戈一击`、`P19 法术反击`、`A36 同袍共济`）缺受击成长核；
   - 中后期面对 `G03 步步为营 + A14 灵鼎护元` 的高防铁桶阵缺专属叠层破甲矛；
   - 毒爆/毒火流（`A19 蚀骨毒`、`A29 蔓毒术`、`A30 焚心炎`、`A31 毒火交攻`）完全吃不到 `G01/G02/O01` 的直伤成长红利。
2. **空白职业与主体生态位尚未补齐**：
   - 净化位与慢速蓄力核（`E01/E02`）除开局 2 回合防先手的 `E08 净心护阵` 外，缺少中后期自身抗控成长轴；
   - 独立护盾体系（`A40 太清灵盾`、`S08 护命灵障`）自撤回 `P11` 后尚无专属人物被动成长轴；
   - 宝宝目前 `0` 项成长被动（`G01–G03` 全为人物专属），缺少高风险高回报的养宠成长机制来放大清宠、护宠与 `B09 游说退敌` 劝宠博弈。

### 1.2 硬边界（严禁触碰）

- **严守 4 条永久禁区**：不做叠加至战斗结束的**速度、控制命中、全队减伤、全队自动回血**。
- **硬层数封顶**：全部 6 项均锁死 `stack_limit = 5`，适配 10–15 回合战斗节奏，禁止无限叠层。
- **分卷宗零稀释与稀世占比红线**：冻结既有卷 1–6，新开常规卷宗 `scroll_id = 7`《兵法叠势卷》（`rarity_tier = 0`，6 项均为常规可拓印/可拆页技能），全永久池稀世占比从 `8/89 (8.99%)` 降至 `8/95 (8.42%) <= 10%`。
- **零新协议字段 / 零配表 Schema 变更**：复用既有 `SkillTable` / `BuffTable` / `SkillScrollCraftTable` 列与 `BattleBuffEntry.stacks`，不修改任何 `.proto` 线协议。

---

## 2. 设计分析

### 2.1 新增 6 项永久成长战法完整规格（`scroll_id = 7`《兵法叠势卷》）

| 表 ID | 追溯码 | 名称 | 主体 / 槽位 | Buff ID | 触发方式与每回合上限 | 单层与封顶效果（持续至战斗结束） | 阵亡 / 劝退 / 驱散规则 |
|---:|---|---|---|---:|---|---|---|
| `12023` | `G04` | **承锋蓄势** | 人物 · 被动 | `120230` | 受敌方直接伤害或 `A36` 援护分担扣血后叠 1 层（每轮限 1 层） | 自身直接伤害 `+6%` / 层，上限 5 层（`+30%`） | 阵亡暂停、复活保留层数；不可普通驱散 |
| `12024` | `G05` | **循隙破甲** | 人物 · 被动 | `120240` | 自身主动攻击（普攻或主动攻击技）命中敌方扣血后叠 1 层（每轮限 1 层） | 自身攻击无视目标护甲与防御 `+4%` / 层，上限 5 层（`20%` 穿防） | 阵亡暂停、复活保留层数；不可普通驱散 |
| `12025` | `G06` | **蚀骨愈深** | 人物 · 被动 | `120250` | 自己施加的中毒/灼烧造成实际扣血后叠 1 层（每轮限 1 层） | 自身持续伤害（毒/灼烧）`+6%` / 层，上限 5 层（`+30%`） | 阵亡暂停、复活保留层数；不可普通驱散 |
| `12026` | `G07` | **磐石道心** | 人物 · 被动 | `120260` | 成为敌方行动控制目标时（命中、抵抗或护阵抵挡皆计）叠 1 层（每轮限 1 层） | 自身全行动控制抵抗（封主/封法/封物/混乱/昏睡/冰冻）`+5%` / 层，上限 5 层（`+25%`） | 阵亡暂停、复活保留层数；不可普通驱散 |
| `12027` | `G08` | **厚泽蓄灵** | 人物 · 被动 | `120270` | 主动施放治疗、护盾或回生印技能时叠 1 层（每轮限 1 层） | 自身施放护盾容量 `+5%` / 层，上限 5 层（`+25%`） | 阵亡暂停、复活保留层数；不可普通驱散 |
| `12028` | `G09` | **兽威渐盛** | 宝宝 · 被动 | `120280` | 宝宝主动攻击（普攻或主动攻击技）命中敌方扣血后叠 1 层（每轮限 1 层） | 宝宝自身直接伤害 `+6%` / 层，上限 5 层（`+30%`） | **阵亡或被劝退即清零层数**（保留被动本体，复活/重召后可从 0 再叠）；不可普通驱散 |

*(说明：候选 #5 在“护盾容量 vs 治疗效能”中最终选定**护盾容量**，原因：治疗已有常驻被动 `P05 悬壶济世` 而独立护盾 `A40/S08` 尚无专属被动，且护盾有 2 回合时效、不恢复已损血量、可被 `A34` 驱散或毒火绕盾，绝不制造拖平膀胱局。)*

### 2.2 统一数据驱动引擎扩展（最小增量、零技能 ID 硬编码）

遵循 `AGENTS.md §11.1 / §11.2`，沿用既有 `BattleEffectOp::GrowingAura (25)` 统一承载“时间轴叠层”与“事件触发叠层”，无需新增 `BattleEffectOp`：

1. **枚举扩充（C++ `battle_effect_constants.h` 与 Java `BattleEffect.java` 严格对齐）**：
   - `BattleTrigger` 新增 3 项事件触发相位：
     - `AfterDirectAttack = 7`（同时匹配普攻命中 `AfterBasic(1)` 与主动技能命中 `AfterSkill(2)`，受 `per_round_limit = 1` 统一限频，服务 `G05` 人物与 `G09` 宝宝）；
     - `AfterControlAttempt = 8`（敌方施放 `state_kind ∈ [1..6]` 行动控制时触发，命中、抵抗或被 `E08 净心护阵` 抵挡皆计，每轮限 1 层，服务 `G07 磐石道心`）；
     - `AfterSupportCast = 9`（主动施放含 `Heal`/`Shield`/`ReviveMark` 效果的技能后触发，每轮限 1 层，服务 `G08 厚泽蓄灵`）。
   - `BattleModifier` 新增 4 项正交修正轴（避免与 `O01 OutgoingDamage(5)` 或 `P16 ControlResist(12)` 撞槽触发“同类取强不相加”）：
     - `SelfGrowingDirectDamage = 25`（服务 `G04` 人物与 `G09` 宝宝，与 `O01`、`G01/G02` 同层加算不复利；覆盖普通直伤、追击及 `P09/P19` 镜像反击）
     - `DefensePenetration = 26`（在 `DamageBeforeCriticalRoll` 按 `1 - clamp(pen, 0, 0.5)` 同步缩减目标护甲与有效防御，再进入 60% 常驻减伤封顶公式；0 时与旧公式逐位一致）
     - `PeriodicDamageBonus = 27`（在 `ResolveConfiguredDamage` 的 `BattleDamageOrigin::Periodic` 分支实时放大，同时覆盖回合末跳伤与 `A31 毒火交攻` 引爆，且受 `require_living_caster` 约束）
     - `AllControlResist = 28`（在 `EquipmentControlRoll` 对全部 6 类行动控制 `kind ∈ [1..6]` 生效，与仅管混乱/昏睡的 `ControlResist(12)` 完全隔离，不误增强宝宝 `P16 凝心守神`）
2. **统一 `GrowingAura` 触发边界**：
   - `BeginConfiguredRound` 仅推进 `trigger_event == RoundStart (6)` 的 `GrowingAura`；
   - `TriggerConfiguredBuffs` 新增 `BattleEffectOp::GrowingAura` 分支：校验敌我阵营（受击/命中仅响应敌方，防混乱自打或友伤刷层），每回合按 `per_round_limit` 叠 1 层至 `stack_limit`；
   - `A36 同袍共济`（`BattleDamageOrigin::Redirect`）分担扣血后仅放行 `GrowingAura` 类型的 `AfterTake` 监听，严禁触发 `MirrorSkill` / `ReflectDamage` / `ConditionalMana` 防止递归或规则漂移。
3. **易失层数清零机制（零 Schema 新增字段）**：
   - 复用 `BuffTable.keep_on_death`：作为被动监听（`IsConfiguredListenerBuff == true`），`HandleDeath` 始终保留被动本体，但当 `battle_op == GrowingAura && keep_on_death == 0` 时，在 `HandleDeath` 与 `RetreatPet` 成功退场两处调用 `ResetVolatileGrowingStacks(actor)` 将 `stacks` 清零（`120280 兽威渐盛` 配 `keep_on_death = 0`，`120200..120270` 配 `keep_on_death = 1`）。

---

## 3. 思想分析

1. **为什么把新卷 7 的全部 6 项统一做成行为/交互触发，同时保留老卷 3 回合稳叠？**  
   老卷 3（`愈战愈勇 / 深谋远虑 / 步步为营`）承接率土经典“时间轴保底成长”，适合稳健型单核；新卷 7（`G04–G09`）则 100% 做到零站桩白嫖——受击（`G04`）、主动命中（`G05`/`G09`）、DOT 跳伤（`G06`）、成为敌方控场目标（`G07`）、主动施放疗盾（`G08`）全部与战场交互挂钩，对手可通过绕火/改毒、封控断出手、净化毒火、转控别的目标、封灵断辅或秒宠/劝宠主动打断成长曲线。
2. **为什么用独立正交 `BattleModifier` 而不是硬塞进旧枚举？**  
   引擎 `ConfiguredModifier` 遵循 `ModifierSelection`“同轴取最强正项”规则；独立正交轴保证 `G04` 能与全队光环 `O01` 加算不互相吞掉、保证 `G07` 不意外把 `P16 凝心守神` 的抗混/抗睡偷偷改成全抗、保证 `G05` 穿防准确作用于防御/护甲层而非错误绕过 60% 常驻减伤红线。
3. **为什么不改动任何 `.proto`？**  
   `BattleBuffEntry.stacks` 已由战斗快照实时同步客户端，`SkillScrollCraft` 与 `SkillTable` 均为全表驱动下发；不改协议、不增新 RPC 即可完成三端无缝接入，最小化上线与双服对账成本。

---

## 4. 漏洞分析与防线总表

| 潜在漏洞 / 边界 | 破坏后果 | 防护设计 |
|---|---|---|
| **事件型 `GrowingAura` 在回合开始也被自动叠层** | `G04/G05/G06/G09` 站着不动也会每回合涨层 | `BeginConfiguredRound` 强制校验 `trigger_event == BattleTrigger::RoundStart (6)`。 |
| **群攻命中 5 人、连击或多段毒火单回合瞬间叠满 5 层** | 第 1 回合就吃满 `+30%` 破坏前期节奏 | 全部事件型 Buff 均配 `per_round_limit = 1`，由 `TriggerConfiguredBuffs` 按 `(last_trigger_round, trigger_count)` 严格限频每回合至多 1 层。 |
| **`A36 同袍共济` 援护转伤意外触发反击/反伤或回蓝** | 破坏 `combat-rules.md §8` 不递归铁律 | `Redirect` 路径进入 `AfterTake` 时仅放行 `BattleEffectOp::GrowingAura`，其余 op 一律跳过。 |
| **混乱打队友 / 友方自伤白刷 `G04` 或 `G05/G09` 层数** | 利用混乱或自残机制套利刷层 | `GrowingAura` 响应 `AfterTake / AfterBasic / AfterSkill / AfterDirectAttack` 时强制校验 `owner.team_index() != other.team_index()`，且 `Confusion` 来源不进入 `AfterSkill`/`AfterBasic`。 |
| **穿防叠层导致目标防御变负或突破 60% 减伤边界** | 出现负防御/NaN 或伤害异常 | `DefensePenetration` 钳制于 `[0.0, 0.50]`，缩减的是 `armor` 与 `EffectiveDefense`（非负整数向下取整），仍经标准 `ReceivedRatio` 计算。 |
| **宝宝 `G09` 阵亡或被 `B09` 劝退后，被动被误删或层数未清零** | 复活/重召后再也叠不了，或死后仍保留满层 `+30%` | 凭 `IsConfiguredListenerBuff` 保留被动本体不删除，同时在 `HandleDeath` 与 `RetreatPet` 成功处统一调用 `ResetVolatileGrowingStacks` 清零 `keep_on_death == 0` 的 `stacks`。 |
| **全控抗性误改 `BattleModifier::ControlResist(12)` 导致 `P16` 越权增强** | 宝宝 `P16 凝心守神` 变成全控抵抗，破坏既有单测与平衡 | 新增独立 `AllControlResist = 28` 仅服务 `G07`，`ControlResist = 12` 保持仅作用于 `kind == 4 || kind == 5`；且 `AllControlResist` 仅作用于 `kind ∈ [1..6]`，不影响中毒(7)。 |
| **稀世占比超标或旧卷宗池被稀释** | 破坏制书保值承诺或触发 `ValidateCraftTables` 拒启 | 新开 `scroll_id = 7`《兵法叠势卷》（常规），6 项均为 `rarity_tier = 0`，稀世占比 `8/95 = 8.42% <= 10%`。 |
| **客户端缺新图标 PNG 时 UI 报错或白块** | 未合入 Codex 图标前影响列表/制书体验 | `SkillBookMapper` 与 `SkillCraftWindow` 均具备缺图回退机制；新增 `12023..12028` 图标键及 `scroll_7` 卷宗图标交接单，图到位即自动生效。 |

---

## 5. 步骤分析与执行步骤

- [x] **步骤 1**：落盘本设计文档与异机 Codex 纯功能图标交接单 [growing-passive-skills-v2-ui-handoff.md](ui-handoff/growing-passive-skills-v2-ui-handoff.md)，同步更新 `docs/README.md`。
- [x] **步骤 2**：向权威源表 `data/Skill.xlsx`（`12023..12028`）、`data/Buff.xlsx`（`120230..120280`）与 `data/SkillScrollCraft.xlsx`（`id = 7`）写入数据，执行全量导表（`run.py` + 仓内 `libprotoc 35.1`）并刷新 `data/AGENTS.md` 索引。
- [x] **步骤 3**：实现 C++ 战斗引擎（`battle_effect_constants.h`、`configured_battle_effects.cpp`、`equipment_battle_effects.cpp`、`turn_battle_engine.{h,cpp}`），更新 `modern_skill_test.cpp` 目录基线至 95 项并扩展 `growing_passive_skill_test.cpp` 覆盖全部 6 项及边界条件。
- [x] **步骤 4**：执行 C++ 静态守卫（`test_cpp_no_raw_pointer_member.py`、`check_log_text_english.py`）、串行 MSBuild `/m:1` 与 `turn_battle_engine_test.exe`（521/521 PASS）回归验收。
- [x] **步骤 5**：同步 Java 独立服 `xuanming-server-mmo` 的 `skill.pb` / `buff.pb` / `skillscrollcraft.pb` 与 `manifest.json`，在 `game-battle-engine` 镜像实现同等机制并扩展 `GrowingPassiveSkillTest.java` 与 `ConfiguredBattleCatalogTest.java`，运行 Maven 单测（`game-battle-engine` 37/37 + `RankedBattleEngineTest` 11/11 + `game-scene` `EquipQualityAndSkillCraftTest` 5/5）全绿验收。
- [x] **步骤 6**：同步 Unity 客户端 `mmorpg-client` 配置表（`sync_tables.ps1` 81/81 一致），复用既有全数据驱动 `UI/Skills/{skillId}` 与 `UI/Ugui/Skills/Scrolls/scroll_{scrollId}` 路径（缺图安全回退，零硬编码 C# 改动），执行 `client_compile_check.ps1`（781/781 0 错误）。
- [x] **步骤 7**：更新 `docs/PROGRESS.md` 与 `xuanming-server-mmo/PARITY.md` 对账台账，并在交付回复中提供可直接复制给异机 Codex 的纯功能图标制作清单。

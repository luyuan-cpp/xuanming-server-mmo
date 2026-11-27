# 战斗战法深度与宝宝养成全量扩展设计（扩展包 A+B+C+D）

> **日期**：2026-11-25  
> **适用仓库**：权威服务端 `xuanming-server-mmo-cpp`（C++ `battle` / `scene` + 权威配表 + 协议）、Unity 客户端 `mmorpg-client`（纯 C# 逻辑层 + Gemini UI 交接规范）；独立 Java 服务端 `xuanming-server-mmo` 按负责人指令记录至 `PARITY.md` 待同步 TODO。  
> **前置共识**：项目未上线、玩家数据可清空、所有改动直接在 `main` 推进；按最高工程标准实现强一致、可水平扩展、数据驱动且易于维护的契约，拒绝按技能 ID 或策划编号写死运行时分支。

> **2026-11-27 用户讨论记录（非现行实现规格）**：用户希望人物、宝宝属性点洗点约 5 元一次；宝宝洗资质与成长约 5 元一次，最多 5 次养满，全付费最多 25 元。用户还希望免费玩家可从 PVE/PVP 获得培养资源，困难 PVE 单场收益高一些，并按每天玩约 30 分钟测算免费养满所需时间。这些是本次对话中尚未写入策划的需求记录；装备打造、强化、材料产出与商城继续沿用已定的 [核心循环](core-gameplay-loop.md)、[强化材料经济](season-pvp-pve-economy-unified-v1.md) 和 [官方商品](spirit-shop-and-starter-packs-v1.md)，不恢复旧表格的每日 20 份或 185 元测算。
>
> **当前状态**：人物与宝宝四维洗点仍按配表扣游戏金币，低等级按既有规则免费；宝宝条目点战外免费重分。宝宝资质当前出生随机、实例内不变；成长率由四维资质派生。洗资质/洗成长、对应付费商品、免费洗练资源及免费养满时长均未在当前版本定案或实现。本记录不改变现行玩法，也不为尚未定案的洗练指定操作方式、产出数值或商城规则。

---

## 一、需求分析

在现有 69 项现代回合技能、72 条 Buff、宝宝 21 项独立条目点与五阶归属已接通的基础上，负责人批准确认一次落地四套玩法深度扩展（A+B+C+D），并正式命名 A40（`10040`）：

1. **A40 正式定名**：将技能 `10040` 由占位名「独立护盾（名称待定）」定名为 **「太清灵盾」**。
2. **扩展包 A — 策略定向新技能（人物 14 招 `S01–S14` + 宝宝 2 招 `SP01–SP02`，共 16 招）**：
   - 将现有群攻/群封/群辅“副目标纯随机”升级为战术定向选人：主目标仍由玩家指定，其余名额按策略排序补选（敌方物攻最高、法攻最高、当前气血数值最低、友方气血比例最低、物防最低、法防最低、本回合排速速度最低）。
   - 新增单次消耗型攻防印记（截锋令/遏灵令削弱下一次物/法攻击；授锋战印/聚灵法印强化下一次物/法主动攻击）。
3. **扩展包 B — 率土式构筑战法（`E01–E08`，共 8 招，撑出 6 套全新反制流派）**：
   - `E01 蓄锋破阵` / `E02 蓄雷天刑`：1 回合明牌蓄力、次轮高额群攻爆发；对手可在蓄力窗口内通过击杀、硬控、对应封攻、驱散或开盾反制。
   - `E03 灵法专修` / `E04 兵锋专修`：永久被动互斥专精——彻底禁用物理（普攻/物理技/物理反击）换取 `+22%` 法伤，或彻底禁用魔法（法术技/法术反击）换取 `+22%` 物伤，不误封治疗与辅助。
   - `E05 截灵追击` / `E06 封锋追击`：人物与宝宝共用追击战法，普攻后按概率与条目尝试封法，法术主动命中后尝试封物（每轮至多触发 1 次，走完整控制抗性与免控护阵判定）。
   - `E07 连击破绽`：开场为敌方至多 5 人挂载可驱散易伤弱点，每次直接攻击/追击命中叠 1 层（同一行动 `group_id` 每目标最多叠 1 层，至多 5 层 `+30%` 承伤），人宠接力滚雪球。
   - `E08 净心护阵`：开场为友方至多 7 人提供 1 次行动控制抵挡（2 回合、可被敌方驱散），反制一速先手群控。
4. **扩展包 C — 宝宝条目 3 方案一键切换 + 五阶真养成闭环**：
   - **3 套条目预设方案（槽位 `1..3`）**：战外免费一键切换，每槽位带枚举用途标签（`0 默认 / 1 输出 / 2 控场 / 3 辅助`，无自由文本输入），切换走乐观锁 + 幂等键，同时实时同步当前生效的 `entry_allocated`。
   - **五阶真分化与灵阶飞升**：常规宠种按定位合理划分初始阶位（普通 1 阶 / 变异 2 阶 / 灵兽 3 阶，活动神兽 `101 春禧兽` 维持 4 阶神兽），并开放 `AscendPetTier` 灵阶飞升（消耗玩法产出 `3406 悟道残页` + 银两 + 宝宝等级门槛，从当前阶逐阶升至 5 阶圣兽），每升一阶按 `PetTier.xlsx` 表驱动增加 `+2` 点独立条目预算（20→22→24→26→28，单项上限不变，不引入失控全局属性倍率）。
5. **扩展包 D — 人物战法构筑装配槽 + 3 套流派方案一键切换**：
   - 遵循**实例与布局分层**（`AGENTS.md §11.6`）：已学技能及重数留在 `skill_list`（实例层），构筑装配槽与 3 套流派方案留在 `loadout_schemes`（布局层）。
   - 槽位预算：**开场 1 槽 + 被动 3 槽 + 追击 1 槽**，主动技能全量可用；同 `exclusive_group` 互斥（如 E03 与 E04 不可同装）；提供 `SaveSkillLoadout` 与 `SwitchSkillLoadout` 两个强一致幂等 RPC，并支持未启用自定义方案时的向后兼容模式（保证既有单测与新号体验零破坏）。

---

## 二、设计分析

### 1. 数据驱动与模式选型（遵循 `AGENTS.md §11.1 / §11.2`）

- **策略模式（Strategy Pattern）用于副目标选人**：在 `SkillTable` 新增 `secondary_target_policy`（field 61，枚举 0..7），引擎 `SelectConfiguredTargets` 通过单一分发函数按策略计算候选度量值并在并列子集内用引擎确定性 RNG 打破平局。新增任何策略只增一个枚举与比较分支，不按技能 ID 分支。
- **表驱动互斥组（`exclusive_group`）**：在 `SkillTable` 新增 `exclusive_group`（field 62，`0` = 无互斥，`> 0` = 同组互斥），同时作用于场景装配校验（`skill_loadout_rules.h`）与战斗开局第二道防线（`TurnBattleEngine::InitPlayers`）。
- **状态/操作枚举扩充而非特判**：
  - `BattleEffectOp` 新增 `PrepareCast = 27`、`HitVulnerability = 28`、`ControlImmunity = 29`；
  - `BattleModifier` 新增 `NextPhysicalAttack = 21`、`NextMagicAttack = 22`、`ForbidPhysical = 23`、`ForbidMagic = 24`。
- **分卷宗零稀释（遵循 `equipment-quality-and-skill-crafting-v1.md`）**：既有卷宗 `1 五行本源卷`、`2 首发绝学卷`、`3 兵法成长卷` 保持原封不动；新增 `4 兵谋定向卷`（16 策略技，常规）、`5 奇策战法卷`（6 率土常规技）、`6 奇策绝学卷`（2 率土稀世开场技 E07/E08），新技能稀世占比 `2/24 = 8.3% <= 10%`。
- **存储零 DDL 扩展与水平扩展性**：所有新增持久化字段均位于已有 `player_database` 内的 `PlayerPetComp` 与 `PlayerSkillListComp` Protobuf blob 中，不改 MySQL 表结构、不引入跨服共享状态、不新开线程，完全兼容多 scene 节点水平扩展与跨 zone 迁移。

### 2. 协议与数据契约设计

| 模块 | 新增 / 变更源 | 核心字段 / RPC |
|---|---|---|
| **配表 Schema** | `skill_table.proto` | `uint32 secondary_target_policy = 61;`、`uint32 exclusive_group = 62;` |
| **配表 Schema** | `pettier_table.proto` | `uint32 ascend_cost_item_id = 5;`、`uint32 ascend_cost_item_count = 6;`、`uint64 ascend_cost_gold = 7;`、`uint32 ascend_min_pet_level = 8;`、`uint32 bonus_entry_points = 9;` |
| **持久化组件** | `player_pet_comp.proto` | `PetEntryPreset { slot_id, tag, allocated }`、`active_entry_preset_slot = 19`、`entry_presets = 20`、`tier_last_request_id = 21` |
| **持久化组件** | `player_skill_comp.proto` | `SkillLoadoutScheme { scheme_id, role_tag, passive_skill_ids, pursuit_skill_ids, opening_skill_ids }`、`active_loadout_scheme_id = 2`、`loadout_schemes = 3`、`loadout_state_version = 4`、`loadout_last_request_id = 5` |
| **战斗数据** | `battle_data.proto` | `BattleActorState.charging_skill_id = 45;`（公开蓄力技 ID，供 UI 头顶展示） |
| **宝宝协议** | `scene/player_pet.proto` | `SwitchPetEntryPreset`、`AscendPetTier`；`PetEntriesInfo` 增加 `active_preset_slot`/`presets`；`PetInfo` 增加 `next_tier` 飞升预览 |
| **技能协议** | `scene/player_skill.proto` | `SaveSkillLoadout`、`SwitchSkillLoadout`；`SkillCatalogEntry` 增加 `secondary_target_policy`/`exclusive_group`/`equipped`；`ListSkillsResponse` 增加 `loadout` 完整快照 |

---

## 三、思想分析

1. **为什么把 A/B/C/D 放在一起做成闭环**：
   - 仅有新技能（A+B）而无装配取舍（D），后期玩家会全部点满导致“所有角色同质化”；有了 `1 开场 + 3 被动 + 1 追击` 与 `exclusive_group` 互斥后，**纯法蓄力核、纯物连击核、开场免控反手队、人宠破绽接力队**才会真正分化成不同流派。
   - 宝宝五阶过去只有外观预览与活动单宠，加上 **图鉴初始分化 + 玩法材料灵阶飞升 + 3 套免费条目方案** 后，玩家打论剑切控场套、打首领试炼切输出/护盾套，且每只喜爱的宠都能一路养成到圣兽，彻底盘活既有美术资产。
2. **为什么不引入自由文本方案名**：
   - 按 `mmorpg-client/AGENTS.md` 规定，所有自由输入文本必须走异步 `content_guard` 审核；预设方案使用有限枚举 `role_tag`（`0 默认 / 1 输出 / 2 控场 / 3 辅助`）既满足一键辨识，又消除异步审核状态机与 UGC 违规面，符合 KISS 原则。

---

## 四、漏洞分析（安全 / 一致性 / 边界条件）

| 潜在漏洞 / 边界 | 防护设计 |
|---|---|
| **单次消耗型印记（S01/S02/S09/S10）在群攻中只对第 1 个目标生效就被提前消耗** | 在 `ExecuteConfiguredSkill` / `ExecuteAttack` 行动粒度统一快照并应用，整次行动至少命中 1 次直接伤害后于行动末尾一次性摘除标记，避免群攻后半段丢失加成或减益。 |
| **蓄力技（E01/E02）被复制（A41）/ 镜像（P09/P19）或蓄力期被控后白白触发** | 表配 `forbid_copy = 1`；蓄力状态 `keep_on_death = 0` 且可被 `A34` 驱散；次轮结算前若处于冰冻/昏睡/混乱/封主动/对应封物或封法，立即打断并销毁蓄力状态，不释放伤害也不退蓝。 |
| **纯物/纯法专精（E03/E04）借普攻、装备反击或镜像反击绕过禁用代价** | `ForbidPhysical` 同时拦截普攻提交与执行、`kActionTagPhysical` 主动技、物理 `TryCounterAttack` 与物理 `MirrorSkill`；`ForbidMagic` 同理拦截 `kActionTagMagic` 主动技与法术 `MirrorSkill`；两者均不拦截治疗、护盾、净化、防御、道具。 |
| **连击破绽（E07）被多段连击、毒火或反伤瞬间叠满 5 层** | 仅响应 `Direct` 与 `Pursuit` 来源且伤害 `> 0`；每个 `HitVulnerability` 实例记录 `(last_trigger_round, secondary_value = currentGroupId)`，同一行动 `group_id` 对同一目标最多叠 1 层；毒火、反伤、分担转伤、资源代价不叠层。 |
| **净心护阵（E08）拦截中毒/灼烧或绕过驱动驱散** | 仅拦截 `BattleEffectOp::Control` 且 `state_kind ∈ [1..6]`（封主动/封法/封物/混乱/昏睡/冰冻），不挡毒/灼烧；正常标记为可驱散 `Buff`，可被 `A34` 驱散；触发抵挡后扣减 `remaining_hits` 并归零移除，发出 `BATTLE_EVENT_RESIST` 供客户端演出。 |
| **宝宝条目方案切换在途并发 / 跨版本预算超限** | 方案切换与保存共用 `entry_state_version` 乐观锁与 `request_id` 幂等回执，在战内/冻结/交接窗口一律 `fail-closed` 拒绝；切到历史方案若因版本降预算导致超额，复用既有 `effective = false` 保护，绝不静默丢点。 |
| **宝宝灵阶飞升扣道具成功但扣银两失败导致资产损失** | `AscendPetTier` 先完整预检（等级、阶位、乐观 `expected_tier`、背包物品数、银两余额、幂等键），先扣银两再扣道具；任一步失败立即原样回滚已扣资产（或先查后在同事件循环原子扣减并检查返回值），成功才推进 `cultivation_tier` 与 `entry_state_version` 并请求关键存盘。 |
| **人物战法装配方案破坏既有单测 / 老号技能生效** | `active_loadout_scheme_id == 0` 维持全量非互斥兼容态；一旦调用 `SaveSkillLoadout` / `SwitchSkillLoadout` 启用方案（`1..3`），scene 派生属性（`CollectPassiveBonus`）、辅助来源（`PlayerSupportEffectSystem::Build`）与战斗快照（`FillBattleSnapshot`）三处共用同一 `PlayerSkillSystem::IsSkillEquipped` 真源，杜绝面板与战内分叉；开局 `TurnBattleEngine::InitPlayers` 再对 `exclusive_group` 做 fail-closed 二次复核。 |
| **宝宝五阶飞升后条目预算变更破坏 `PetEntriesTest` 既有断言** | 1 阶（普通）`bonus_entry_points = 0`（总预算维持 20），2/3/4/5 阶依次 `+2/+4/+6/+8`（总预算 22/24/26/28），既保证既有 1 阶单测 100% 不变，又让进阶成长清晰可感知。 |
| **Gate 限流拦截新 RPC（`server tip=1008`）** | 在 `data/MessageLimiter.xlsx` 仅增量追加新 RPC 对应限流配置（`500=10 / 501=5 / 502=10 / 503=10`，窗口 1s，tip=1000），绝不覆盖其他会话对 `MessageLimiter.xlsx` 的未提交改动。 |

---

## 五、步骤分析与执行步骤

1. **落盘设计文档与索引**：保存本设计文档并更新 `docs/README.md`。
2. **更新 Schema、配表与 Tip**：
   - `data/schema/skill_table.proto` 加 `secondary_target_policy`、`exclusive_group`；
   - `data/schema/pettier_table.proto` 加 5 个灵阶飞升列；
   - 更新 `data/Skill.xlsx`（`10040` 定名「太清灵盾」+ 24 项新技能）、`data/Buff.xlsx`（新增 28 条对应 Buff）、`data/SkillScrollCraft.xlsx`（新增卷宗 4/5/6）、`data/PetTier.xlsx`（补齐 5 阶飞升参数）、`data/Pet.xlsx`（合理分化 2/3 阶常规宠种，保留 `1 灵狐 = 1`、`101 春禧兽 = 4`）、`data/tip/Tip.xlsx`（新增灵阶飞升与战法装配 tip 错误码）。
3. **更新服务端 Proto 契约**：修改 `player_pet_comp.proto`、`player_skill_comp.proto`、`battle_data.proto`、`scene/player_pet.proto`、`scene/player_skill.proto`。
4. **实现战斗引擎（扩展包 A + B）**：
   - `battle_effect_constants.h` 扩展操作与修正枚举；
   - `turn_battle_engine.{h,cpp}`：冻结回合初速度 `roundStartSpeeds`、实现蓄力触发/打断与 `charging_skill_id` 投影、专精禁攻校验、`InitPlayers` 互斥组复核、`ResolveBasicAttackHit` 与 `ExecuteAction` 单次消耗印记钩子；
   - `configured_battle_effects.cpp` & `equipment_battle_effects.cpp`：实现 7 种副目标策略排序、单次物/法攻印记、蓄力准备/释放、连击破绽叠层、净心护阵控场拦截。
5. **实现场景宝宝模块（扩展包 C）**：
   - `player_pet_entries.cpp`：条目预算支持 `PetRule.entry_total_points + PetTier.bonus_entry_points`，支持 `entry_presets`（1..3 槽）同步写回与 `SwitchEntryPreset`；
   - `player_pet.cpp` & `player_pet.h` & `player_pet_handler.{h,cpp}`：实现 `FillTierAscension` 与 `AscendTier` 强一致扣费扣材飞升，暴露新 RPC 入口。
6. **实现场景技能装配模块（扩展包 D）**：
   - 新增纯规则头 `cpp/libs/services/scene/player/system/skill_loadout_rules.h`（不依赖 ECS，方便单测）；
   - `player_skill.{h,cpp}` & `player_skill_handler.{h,cpp}`：实现 `SaveLoadout`、`SwitchLoadout`、`IsSkillEquipped`、接入 `BuildSkillBook`、`CollectPassiveBonus`、`FillBattleSnapshot`、`UpgradeSkill` 自动补槽。
7. **实现 Unity 客户端纯 C# 适配层与双端单测**：
   - `PetClient.Entries.cs`（`SwitchEntryPreset`）与 `PetClient.cs`（`AscendTier` + 新 Tip 映射）；
   - `SkillBookClient.cs` / `SkillBookMapper.cs`（`SaveLoadout`、`SwitchLoadout`、新目录字段与副目标策略展示文本）；
   - `tools/gen_messageids.ps1` 白名单登记 4 个新客户端 RPC；
   - 服务端 C++ 单测（`modern_skill_test.cpp` 同步目录总数至 89 并新增策略定向/蓄力/专精/追击封控/连击破绽/净心护阵测试、`pet_entries_test.cpp` 新增方案切换与五阶飞升测试、新增 `player_skill_loadout_test.cpp`）与客户端 NUnit EditMode 单测。
8. **只读静态预检与 Gemini 交接文档**：
   - 执行 `test_cpp_no_raw_pointer_member.py`、`test_cpp_thread_sources.py`、`check_log_text_english.py`、xlsx schema 静态校验；
   - 产出 `mmorpg-client/Docs/battle-skill-and-pet-expansion-ui-handoff.md` 供 Gemini 完成编译测试与新 UI 接线，并追加 `docs/PROGRESS.md` 与 `xuanming-server-mmo/PARITY.md` TODO。

---

## 六、最终设计方案摘要（技能总表）

### 1. 策略定向 16 招（`scroll_id = 4 兵谋定向卷`，常规）

| 表 ID | 追溯码 | 名称 | 主体 | 目标与上限 | 副目标策略（`secondary_target_policy`） | 效果与首测参数 |
|---:|---|---|---|---|---|---|
| 10101 | S01 | 截锋令 | 人物 | 敌 3 | 1 敌方物攻最高 | 施加 2 回合标记，削弱下一次物理攻击 `-25%`，触发后消耗（CD2，蓝 7%，概率 85%） |
| 10102 | S02 | 遏灵令 | 人物 | 敌 3 | 2 敌方法攻最高 | 施加 2 回合标记，削弱下一次法术攻击 `-25%`，触发后消耗（CD2，蓝 7%，概率 85%） |
| 10103 | S03 | 断刃锁心 | 人物 | 敌 2 | 1 敌方物攻最高 | 封锁物理攻击 2 回合（CD3，蓝 8%，基础 70%） |
| 10104 | S04 | 缚咒锁灵 | 人物 | 敌 2 | 2 敌方法攻最高 | 封锁法术攻击 2 回合（CD3，蓝 8%，基础 70%） |
| 10105 | S05 | 逐命斩 | 人物 | 敌 3 | 3 当前气血数值最低 | 物理直伤（倍率 1.0，CD1，蓝 6%） |
| 10106 | S06 | 追魄雷 | 人物 | 敌 3 | 3 当前气血数值最低 | 法术直伤（倍率 1.0，CD1，蓝 6%） |
| 10107 | S07 | 扶危甘霖 | 人物 | 友 4 | 4 气血比例最低 | 即时治疗（`per_level=20`，不复活，CD1，蓝 7%） |
| 10108 | S08 | 护命灵障 | 人物 | 友 3 | 4 气血比例最低 | 独立吸收护盾 2 回合（`per_level=16`，CD3，蓝 8%） |
| 10109 | S09 | 授锋战印 | 人物 | 友 3 | 1 友方物攻最高 | 强化下一次物理主动攻击 `+30%`，2 回合，触发消耗（CD2，蓝 7%） |
| 10110 | S10 | 聚灵法印 | 人物 | 友 3 | 2 友方法攻最高 | 强化下一次法术主动攻击 `+30%`，2 回合，触发消耗（CD2，蓝 7%） |
| 10111 | S11 | 寻隙破阵 | 人物 | 敌 2 | 5 敌方物防最低 | 物理直伤（倍率 1.05，CD1，蓝 6%） |
| 10112 | S12 | 穿隙灵雷 | 人物 | 敌 2 | 6 敌方法防最低 | 法术直伤（倍率 1.05，CD1，蓝 6%） |
| 10113 | S13 | 掠影追锋 | 人物 | 敌 3 | 7 本回合排速最低 | 物理直伤（倍率 1.0，CD1，蓝 6%） |
| 10114 | S14 | 流光落星 | 人物 | 敌 3 | 7 本回合排速最低 | 法术直伤（倍率 1.0，CD1，蓝 6%） |
| 11101 | SP01 | 猎血撕咬 | 宝宝 | 敌 2 | 3 当前气血数值最低 | 宝宝物理直伤（倍率 0.90，CD1，蓝 6%） |
| 11102 | SP02 | 追魂灵弹 | 宝宝 | 敌 2 | 3 当前气血数值最低 | 宝宝法术直伤（倍率 0.90，CD1，蓝 6%） |

### 2. 率土式构筑 8 招（`scroll_id = 5 奇策战法卷` / `scroll_id = 6 奇策绝学卷`）

| 表 ID | 追溯码 | 名称 | 主体 / 类型 | 卷宗 / 稀有度 | 核心机制与首测参数 |
|---:|---|---|---|---|---|
| 10115 | E01 | 蓄锋破阵 | 人物 · 主动 | 卷 5 · 常规 | 本轮蓄力（公开可见、可驱散/控断），下轮正常行动自动释放敌 5 物理爆发（倍率 2.15，CD4，蓝 14%，禁复制） |
| 10116 | E02 | 蓄雷天刑 | 人物 · 主动 | 卷 5 · 常规 | 与 E01 对称，下轮自动释放敌 5 法术爆发（倍率 2.15，CD4，蓝 14%，禁复制） |
| 12101 | E03 | 灵法专修 | 人物 · 被动 | 卷 5 · 常规 | `exclusive_group=1`；禁用普攻/物理主动/物理反击，法术伤害 `+22%` |
| 12102 | E04 | 兵锋专修 | 人物 · 被动 | 卷 5 · 常规 | `exclusive_group=1`；禁用法术主动/法术反击，物理伤害 `+22%` |
| 13101 | E05 | 截灵追击 | 共用 · 追击 | 卷 5 · 常规 | 普攻命中后 45% 触发（每轮 1 次），按 65% 基础率与条目尝试封法 1 回合 |
| 13102 | E06 | 封锋追击 | 共用 · 追击 | 卷 5 · 常规 | 法术主动命中后 45% 触发（每轮 1 次），按 65% 基础率与条目尝试封物 1 回合 |
| 14101 | E07 | 连击破绽 | 人物 · 开场 | 卷 6 · 稀世 | 开场给敌 5 挂 4 回合弱点（可驱散）；每次直接攻击/追击命中 +1 层（同行动每目标至多 1 层，上限 5 层，每层承伤 `+6%`） |
| 14102 | E08 | 净心护阵 | 人物 · 开场 | 卷 6 · 稀世 | 开场给友 7 挂 2 回合护阵（可驱散），抵挡首次行动控制（封主动/封法/封物/混乱/昏睡/冰冻）后消耗 |

# A41 玄镜映法（回合制敌方技能复制）设计方案与 UI 交接

更新：2026-11-23。返回 [技能策划总览](README.md) · [人物主动技能](active-character.md) · [战斗公用规则](combat-rules.md) · [首测参数](first-test-parameters.md)。

架构参考：kasan [《如何实现一个强大的 MMO 技能系统——技能》](https://zhuanlan.zhihu.com/p/149704315) 与 [《如何实现一个强大的 MMO 技能系统——BUFF》](https://zhuanlan.zhihu.com/p/150812545)。

---

## 1. 需求分析

- **核心玩法需求**：新增一个主动技能，本回合指定一名敌方单位，复制该敌方单位的技能并以**施法者自身属性**立即释放。
- **用户已确认规则**：
  1. **Boss 技能不可复制**：任何 Boss 单位及其专属技能都不能被复制；客户端在选目标阶段直接将 Boss 单位置灰/不可选，服务端同步强校验拦截。
  2. **按 kasan《技能》《BUFF》数据驱动架构落地**：技能本身只负责施法壳子、目标筛选、耗蓝与冷却，不按技能 ID 或 Boss ID 硬编码；复制执行与免疫规则全部沉入 Buff 操作（`BattleEffectOp`）、标签免疫（`tag` / `immune_tag`）与配表字段（`forbid_copy` / `is_boss`）。
  3. **目标本回合未选可复制技能时的兜底（方案 A）**：若目标本回合选择普攻、防御、道具、逃跑、捕捉、召回、被控或选了禁复制技能，则从目标已学的可复制主动技能池中确定性随机抽取一招释放；若目标完全没有可复制主动技能，则产出免疫/无效反馈事件（`BATTLE_EVENT_RESIST`）。
  4. **平衡参数由程序按现有 61 项体系定首测试值**：纳入正式配置目录（总数由 61 项增至 62 项），首测数值标记为待实战平衡。
  5. **UI 与美术由 Codex 承接**：本文同时给出完整的客户端协议、演出事件、UI 交互与出图规格。

---

## 2. 设计分析

### 2.1 平衡参数设计（`A41 玄镜映法`）

| 项目 | 配置值 | 平衡依据 |
|---|---|---|
| 策划编号 / Skill ID / Buff ID | `A41` / `10041` / `100410` | 沿用 `A=10000+N`、`Buff=Skill*10+idx` 约定，避开全部退役 ID |
| 技能名称 | `玄镜映法` | 贴合道家玄冥语境（对应太虚澄心、灵鼎护元、五色返照） |
| 主体资格 (`caster_mask`) | `1`（人物专属） | 避免 5 人 + 5 宠同时高频复制高价值群攻/群控导致回合失衡；人物施放共享技能时按人物上限（敌5/友7），复制到宝宝专属技按该技自身 `max_targets`（1或2） |
| 技能类型 (`battle_model` / `ability_kind`) | `1` / `1`（现代主动） | 接入统一事件化战斗模型，受 `A20 封灵咒` 限制 |
| 目标阵营与数量 (`target_team` / `max_targets`) | `1`（敌方）/ `1` | 单体指向敌方目标；禁止选 Boss（`target.is_boss()`） |
| 行动标签 (`action_tags`) | `8`（`kActionTagSupport`） | 归入辅助策略，不会被混乱（`A23`）选中误打队友 |
| 耗蓝 (`mana_percent`) | `0.10`（最大法力 10%） | 高于常规 5%–8% 技能，低于 15% 大爆发，反映灵活借招的策略成本 |
| 冷却 (`cooldown_rounds`) | `3`（R1 施放、R4 再用） | 对标 `A20`/`A23`/`A26`/`A38`/`A40`，防止连续回合复制爆发/复活/冻结 |
| 禁复制标记 (`forbid_copy`) | `1`（本技能自身禁复制） | 同步给 `B13 舍命破灵`、`B15 移灵养元`、`B16 舍身救主` 标 `forbid_copy = 1` |
| 复制效果 Buff (`100410`) | `battle_op = 26` (`CopyTargetSkill`), `ratio = 0.85`, `tag = ["SkillCopy"]` | 伤害、治疗、护盾、回蓝、抽蓝、持续伤害、回生印均按 **85%** 折算，避免以 10% 蓝/CD3 满额白嫖 15% 蓝/CD4 爆发技；重数按施法者**玄镜映法自身重数**结算，保证升级收益 |

### 2.2 数据契约与表结构增量（向后兼容）

1. **`proto/battle/battle_data.proto` (`BattleActorState`)**：
   - 新增 `bool is_boss = 43;`（默认 `false`）。引擎在 `AppendMonsterActor` 中依据 `MonsterTable.is_boss != 0` 填充，客户端选目标阶段直接读该字段置灰 Boss。
2. **`data/schema/monster_table.proto` & `data/Monster.xlsx`**：
   - 新增 `uint32 is_boss = 13;`（`0` 普通怪，`1` 首领）。将副本 3 首领（`Monster.id = 16`，见 `official_scripts.json`）标为 `1`，其余 `1..15` 为 `0`。
3. **`data/schema/skill_table.proto` & `data/Skill.xlsx`**：
   - 新增 `uint32 forbid_copy = 57;`（`0` 允许复制，`1` 禁止复制）。正向布尔语义无双重否定，存量行缺省 `0` 自动兼容。
4. **`cpp/libs/services/battle/constants/battle_effect_constants.h`**：
   - `BattleEffectOp` 末尾（`kCount` 前）新增 `CopyTargetSkill = 26`。

### 2.3 运行时结算流程

1. **提交期校验（`ValidateAction` / `CheckConfiguredSkill`）**：
   - 在 `ConfiguredTargetAllowed` 中与 `RetreatPet` 并列检查：若技能 `effect[]` 含 `BattleEffectOp::CopyTargetSkill` 且 `target.is_boss()`，返回 `false`（`kSkillInvalidTarget`）。
   - 若敌方场上仅剩 Boss，连 `target_id = 0` 也判不可施放，避免白费行动与法力。
2. **执行期（`ApplyConfiguredEffect` -> `CopyTargetSkill`）**：
   - `CopyTargetSkill` 属于即时非持久操作（`Persistent(op) == false`），不滞留于敌方 Buff 列表。
   - **双保险免疫校验**：检查 `receiver.is_boss() || IsImmuneToBuff(receiver, row)`（支持未来任意单位挂 `immune_tag = ["SkillCopy"]` Buff 免疫复制），命中则发 `BATTLE_EVENT_RESIST` 并返回。
   - **技能选择（`SelectCopiedSkillId`）**：
     1. 优先读取 `pendingActions[receiver.actor_id()]`：若为 `BATTLE_ACTION_SKILL` 且满足 `IsSkillCopyableFor(caster, *planned)`，直接复制该技能（无论施法者速度快于还是慢于目标）。
     2. 兜底方案 A：遍历 `receiver.skill_table_ids()`，筛选满足 `IsSkillCopyableFor(caster, *candidate)` 的技能，按 `skill_id` 升序去重后用引擎确定性 RNG 抽 1 招（仅 1 招时不消耗 RNG）。
     3. 若仍无候选，发 `BATTLE_EVENT_RESIST`（`buff_table_id = 100410`）反馈。
   - **可复制资格（`IsSkillCopyableFor`）**：
     - `battle_model == 1 && ability_kind == 1`
     - `forbid_copy == 0 && health_cost_mode == 0 && max_battle_uses == 0`
     - `effect[]` 中不含 `CopyTargetSkill`（结构防递归）且不含依赖已付气血/已付法力（`value_basis == 5 || 6`）的空壳转换
     - `HasValidConfiguredTarget(caster, row)` 为真（例如敌方无宝宝时跳过 `B09 游说退敌`）
   - **目标重映射与镜像施放**：
     - 复制到敌方攻击/控制/驱散（`target_team == 1`）：主目标优先设为被复制的 `receiver`（若 `receiver` 不合法如劝退对人物，则传 `0` 交由引擎选合法敌方），其余目标按施法者主体上限补选。
     - 复制到友方治疗/护盾/增益/净化/回生印（`target_team == 2`）或自身技（`target_team == 3`）：主目标切回 `caster` 自身（其余按己方阵营随机补齐），绝不反手资敌。
     - 调用 `ExecuteConfiguredSkill(caster, copiedAction, *copiedSkill, result, BattleDamageOrigin::Mirror, scale, context.rank)`：
       - 使用**施法者自身属性**与**玄镜映法重数 (`rankOverride`)**；
       - 伤害与 `ConfiguredValue`（治疗、护盾、回蓝、抽蓝、持续伤害、回生印、临时气血）统一乘 `scale`（`0.85`）；`scale == 1.0` 时不执行额外浮点乘法，老回放逐位不变；
       - 被复制技能本体 0 耗蓝、不进施法者冷却、不扣施法者每战次数；
       - `BattleDamageOrigin::Mirror` 阻止递归触发追击与 `P09`/`P19` 反击。
3. **被动镜像同步收口**：
   - `BattleEffectOp::MirrorSkill`（`P09 反戈一击` / `P19 法术反击`）同步补齐 `other.is_boss() || (incoming != nullptr && incoming->forbid_copy() != 0)` 拦截，确保全引擎不存在任何路径可以复制 Boss 技能或禁复制技能。
4. **自动战斗（`SelectAutoConfiguredSkill`）**：
   - 自动选技仅在目标非 Boss、未免疫 `SkillCopy` 且拥有至少 1 个可复制主动技能时认为 `A41` 有用，避免挂机对无技能野怪空放。

---

## 3. 思想分析

- **严格遵循 kasan《技能》+《BUFF》数据驱动模型**：技能（`SkillTable`）只定义施法门槛、资源代价与效果组合，`CopyTargetSkill` 作为原子 `BattleEffectOp` 接入 Buff 执行器；“不可复制”由单位标签免疫（`BuffImmuneTag` 含 `"SkillCopy"`）、单位属性（`MonsterTable.is_boss` -> `BattleActorState.is_boss`）和技能数据字段（`SkillTable.forbid_copy`）共同表达，实现**开闭原则（OCP）**——后续新增 Boss 或禁复制技能仅改表，不改 C++ 代码。
- **策略与上下文解耦（GoF 策略 / 上下文传递思想）**：沿用引擎现有 `BattleEffectOp` 分发与 `EffectContext` 上下文（新增 `rankOverride` 参数替代临时篡改 `actor.skill_ranks()`，避免状态污染与异常残留），复用了 `ExecuteConfiguredSkill` 整条镜像管线，改动面极小且单一事实源（DRY）。
- **确定性优先**：兜底候选集按 `skill_id` 排序去重后再调用引擎成员 `RandIndex`，且 `scale == 1.0` 短路浮点乘法，确保所有既有同种子回放逐字节不变。

---

## 4. 漏洞分析（边界与防滥用）

| 潜在漏洞/边界 | 风险 | 防护措施 |
|---|---|---|
| **Boss 技能被复制** | 玩家复制高倍首领大招击穿数值平衡 | 三层拦截：① 客户端读 `BattleActorState.is_boss` 选目标置灰；② 服务端 `ConfiguredTargetAllowed` 提交期拒收 `kSkillInvalidTarget`；③ 执行期 `CopyTargetSkill` 与被动 `P09/P19` `MirrorSkill` 同步拦 `is_boss`、`forbid_copy` 与 `ImmuneTag("SkillCopy")` |
| **双镜套娃 / 无限递归** | 双方互放 `A41` 触发栈溢出或无限事件流 | ① `A41` 表配 `forbid_copy = 1`；② `IsSkillCopyableFor` 结构拒绝含 `CopyTargetSkill` 的技能；③ `CopyTargetSkill` 入口检查 `origin == Mirror` 直接返回；④ `Mirror` 源不触发 `AfterSkill` / `AfterTake` 监听 |
| **无代价白嫖牺牲/资源转换技** | 镜像 0 耗蓝复制 `B16 舍身救主` 免费 60% 治疗+3 净化，或复制 `B13`/`B15` 因 `paidHealth/paidMana == 0` 产出空效果 | `B13`/`B15`/`B16` 配 `forbid_copy = 1`，且 `IsSkillCopyableFor` 结构拦截 `health_cost_mode != 0`、`max_battle_uses != 0`、`value_basis ∈ {5, 6}` |
| **复制敌方辅助技能反向资敌** | 直接沿用选中的敌方 `target_id` 给敌人加血/套盾/复活 | `target_team == 2 || 3` 时强制把主目标切回施法者己方（`caster.actor_id()`），再由 `SelectConfiguredTargets` 补齐己方其余目标 |
| **复制技能在低重数下白板或抢占原技能 CD** | 玩家没学敌方技能导致按 1 重结算无升级动力，或误把敌方技能写进自己冷却 | `ExecuteConfiguredSkill` 支持显式 `rankOverride` 透传 `A41` 自身重数；`BattleDamageOrigin::Mirror` 跳过二次耗蓝、冷却与每战计数 |
| **出手先后导致读不到敌方本回合技能** | 施法者比敌人快时敌人还没放技能 | `ResolveCurrentRound` 在排速前已完成全员 `pendingActions` 填充，统一读 `pendingActions[target]`，快慢手行为完全一致 |
| **客户端提前偷看对手指令/技能表** | 若客户端展示“即将复制哪招”会泄露对手隐藏行动 | 不下发任何对手隐藏指令，只在回合结算结果 `TurnResultS2C` 事件流里正常下发两段 `BATTLE_EVENT_SKILL` |
| **无合法目标（如全 Boss 或抄 `B09` 对无宠阵容）** | 浪费蓝与行动或空技能事件 | 全 Boss 时 `ValidateAction` 直接拒绝；兜底选技前用 `HasValidConfiguredTarget` 过滤当前无合法目标的技能；无任何可复制技能时发 `BATTLE_EVENT_RESIST` |

---

## 5. 步骤分析

1. **文档与目录更新**：落盘本设计文档，更新 `active-character.md`、`README.md` 与 `tools/scripts/skill_catalog.py`（总原型 61 -> 62，人物可用 40 -> 41），运行 `gen_docs_index.py`。
2. **契约与配表变更**：更新 `proto/battle/battle_data.proto`、`data/schema/skill_table.proto`、`data/schema/monster_table.proto` 与对应源表 `Skill.xlsx`、`Buff.xlsx`、`Monster.xlsx`，重生成 `data/AGENTS.md` 索引。
3. **C++ 引擎实现**：扩展 `BattleEffectOp::CopyTargetSkill`、`AppendMonsterActor` 写 `is_boss`、`ConfiguredTargetAllowed`、`ApplyConfiguredEffect`、`MirrorSkill` 禁 Boss 技能与 `SelectAutoConfiguredSkill`。
4. **单元测试补齐**：更新 `modern_skill_test.cpp` 与 `player_skill_book_test.cpp` 计数基线，并新增完整机制与边界单测（本回合技能复制、方案 A 随机兜底、重数与 85% 折算、友方技能阵营重映射、Boss 拦截与 `is_boss` 下发、Buff `ImmuneTag("SkillCopy")`、`forbid_copy`/防套娃/防 `B16` 牺牲技、被动 `P09`/`P19` 不复制 Boss 技能、自动战斗过滤、同种子确定性）。
5. **生成物同步与验证**：执行 `dev.bat gen`、`mmorpg-client/gen.bat client`、`test_cpp_no_raw_pointer_member.py`、`skill_catalog.py check`、`verify_schema_parity.py`，串行 `/m:1` 编译受影响 C++ 工程并跑通 `turn_battle_engine_test` 与 `skill_test`。
6. **多端同步说明**：Java 版（`xuanming-server-mmo`）与 Python 版（`xuanming-server-mmo-python`）需后续按本设计同步 `CopyTargetSkill` / `is_boss` / `forbid_copy`；Unity UI 与美术资产按第 7 节交由 Codex 执行。

---

## 6. 执行步骤

1. 写入本设计文档，更新 `docs/planning/skill-planning/active-character.md`、`docs/planning/skill-planning/README.md`。
2. 修改 `proto/battle/battle_data.proto`、`data/schema/skill_table.proto`、`data/schema/monster_table.proto`。
3. 更新 `tools/scripts/skill_catalog.py`，并通过幂等补丁写入 `data/Skill.xlsx`、`data/Buff.xlsx`、`data/Monster.xlsx`，重建 `docs/planning/skill-planning/first-test-parameters.md` 与 `data/AGENTS.md`。
4. 实现 C++ 引擎代码：`battle_effect_constants.h`、`turn_battle_engine.h`、`turn_battle_engine.cpp`、`configured_battle_effects.cpp`。
5. 补齐 `modern_skill_test.cpp` 与 `player_skill_book_test.cpp` 单测。
6. 运行只读预检、导表、协议生成（含同级 `mmorpg-client` 同步）、C++ 串行构建（`/m:1`）及单测，如实汇总结果。

---

## 7. 给 Codex 的 Unity UI 与美术交接清单

服务端本次已同步 `BattleActorState.is_boss` 与最新配置表到同级 `mmorpg-client`（`Proto/Generated/BattleData.cs`、`StreamingAssets/Tables/*.pb`），Codex 直接按以下清单制作美术与 UI 即可：

### 7.1 美术出图清单（按项目既有的统一 UI 美术风格、不另定画风/色号；PNG-32 透明底、严禁带文字、无压缩无 mipmap）

| 资产 | 存放路径与规格 | 说明 |
|---|---|---|
| 技能图标 | `Assets/Resources/UI/Skills/10041.png`（与现有 `1101.png`~`1202.png` 同规格） | `A41 玄镜映法` 图标（建议意象：八卦玄镜 / 灵影映照） |
| 复制汲取特效（必做） | `Assets/Resources/Battle/Fx/mirror_copy_cast_strip.png`（8 帧横条、左→右、每帧 `384x384`、pivot `(0.5, 0.5)`） | 第一拍在被复制敌方身上浮现镜面/符印汲取光效 |
| 镜像光环特效（推荐） | `Assets/Resources/Battle/Fx/mirror_cast_ring_strip.png`（8 帧横条、`384x384`、地面型 pivot `(0.5, 0.1)`） | 第二拍释放复制技能时在施法者脚下叠加银青/紫金镜像阵，同时复用于 `P09`/`P19` |
| 灵光折返弹道（选做） | `Assets/Resources/Battle/Fx/mirror_copy_travel_strip.png`（8 帧横条、`384x384`） | 从目标飞回施法者的灵光轨迹（复用 `BattleFxPlayer.PlayTravel`） |

同步更新 `Assets/Resources/Battle/ART_MANIFEST.json`、`Assets/Resources/Battle/skill_presentation.json`（为 `skill_id: 10041` 绑定 `mirror_copy_cast`）与 `Assets/Resources/Skills/catalog.json`（登记 `10041 玄镜映法`）。

### 7.2 客户端 UI 与演出接线（`mmorpg-client`）

1. **选目标阶段灰掉 Boss（`BattleScreen.cs`）**：
   - 选择 `10041`（或含 `CopyTargetSkill` 语义的技能）进入选目标模式时，`IsTargetable` 检查 `actor.IsBoss`，为 `true` 时不可选中并置灰（或叠“首领不可复制”提示），选目标提示栏显示 `请为【玄镜映法】选择敌方目标（首领不可复制）`。
2. **补齐事件解码（`TurnPlan.cs`）**：
   - `TurnEventInput` 与 `Beat` 新增 `HitKind`（读 `ev.HitKind`）与 `PrimaryTargetId`（读 `ev.TargetId`）；
   - `BattleEventCodes` 补 `Resist = 15`，`TargetEffect` 补 `Resist`，并入当前 `Cast` 拍。
3. **两拍演出与战报（`BattlePresenter.cs`）**：
   - 第一拍（`SkillId == 10041`）：无伤害但有 `PrimaryTargetId` 时在目标处播 `mirror_copy_cast`；若该拍带 `TargetEffect.Resist`，目标头顶用 TMP 飘字 `不可复制/免疫` 并写入战报。
   - 第二拍（`HitKind == eBattleHitKind.BattleHitMirror`）：复用 `BattleArtCatalog.ResolveSkillFx(beat.SkillId, false)` 播被复制技能原特效 + 脚下叠 `mirror_cast_ring` + 施法者头顶飘字 `【复制】<技能名>`；同步把战报 `施放 技能{beat.SkillId}` 改为读取 `SkillCatalog.Name(beat.SkillId)`，镜像时写 `复制施放【技能名】`。
4. **EditMode 单测（`Assets/Tests/EditMode/Battle/TurnPlanTests.cs` 等）**：
   - 验证 `SKILL(10041) + SKILL(目标技, BATTLE_HIT_MIRROR)` 拆为两拍并保留 `HitKind`；
   - 验证 `SKILL(10041) + RESIST` 正确进入 `Beat` 目标结果；
   - 验证选目标阶段 `actor.IsBoss == true` 不可选且提示明确；
   - 验证缺特效图时走 `BattleArtCatalog` 缺图降级不报错。

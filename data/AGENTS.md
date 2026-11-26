# CONFIG TABLE KNOWLEDGE BASE

## OVERVIEW
`data/` 放策划配置表。**schema 的事实源是 `data/schema/<Sheet>_table.proto`，不是 xlsx 表头**
（2026-09-03 迁移完成）。xlsx 只提供列名与数据；类型、owner、键、索引、外键、位序、注释
全部写在权威 schema 里，用 `data/schema/cfg_options.proto` 定义的自定义 option 表达。

导表器在 `tools/data_table_exporter/`，把这两者合起来生成 JSON / `.pb` / `.proto` /
C++ · Go · Java 的表管理器、常量、table-id 枚举、bit_index 映射。

## STRUCTURE
```text
data/
├── schema/                     # 权威 schema（进 git，走 code review）
│   ├── cfg_options.proto       # option 词表。导表器运行时解析它，没有第二份
│   └── <sheet>_table.proto     # 每张表一份
├── <Sheet>.xlsx                # 源表：第1行列名 / 第2-4行空 / 第5行中文说明 / 第6行起数据
├── operator/Operator.xlsx      # 走 enum_gen 另一条路径，格式与主表不同
└── tip/Tip.xlsx                # 同上，tip 错误码轴
```

## WHERE TO LOOK
| 想知道 | 去哪 |
|------|------|
| 某张表有哪些列、什么类型 | `data/schema/<sheet>_table.proto` |
| 有哪些 option 可以写、什么含义 | `data/schema/cfg_options.proto` |
| 哪张表有主键/索引/外键/位序 | 本文件下方的**配置表索引** |
| 生成出来的查表接口长什么样 | `generated/code/{cpp,go,java}/<sheet>_table.*` |
| 表怎么被读进来的 | `tools/data_table_exporter/core/table_source.py` |
| 加一列/加一张表怎么做 | 本文件 CONVENTIONS |
| tip 错误码 | 根 `AGENTS.md` 的 tip 段；码轴在 `data/tip/Tip.xlsx` |

## CONVENTIONS

### 加一列
1. 策划在 xlsx **任意位置**插一列，第 1 行写英文列名（插在哪都行，绑定按列名不按列位）。
2. 程序在 `data/schema/<sheet>_table.proto` 加一个字段，**号取当前最大号 + 1**，
   非标量必须写 `(cfg_slots)`。
3. `dev.bat gen`。对不上会当场报错并指出是哪一列，产出一个字节都不落盘。

### 加一张表
1. 建 `data/<Sheet>.xlsx`，第 1 行列名（首列必须是 `id`），第 6 行起数据。
2. 写 `data/schema/<sheet>_table.proto`：抄一份现成的改，`(cfg_sheet)` 填 sheet 名。
3. `dev.bat gen`，然后跑 `gen_schema_index.py` 更新本文件的索引。

### 主键可重复的表

有些表同一个 `id` 会有多行。在**主键列**上写 `(cfg_multi)` 声明它：

```proto
message DropTable {
  option (cfg_sheet) = "Drop";
  uint32 id = 1 [(cfg_multi) = true];   // 这张表的 id 允许重复
  ...
}
```

后果：

- 生成 **`FindAllById`**（Go 返回切片 / C++ 返回零拷贝 range / Java 返回 List），
  **不生成 `FindById`** —— 把可重复主键当单值用会在编译期断，而不是悄悄只看第一行。
- **不允许再有 `(cfg_bit_index)`**（互斥，会报错）：位序是「ID → 存档位图下标」，
  重复 ID 没有唯一下标。
- 没声明 `(cfg_multi)` 的表出现重复 `id` = **导表失败，整批不产出**。
  从前这种情况是静默的：id 索引是单值 map，`FindById` 只拿到其中一行、`Count` 少算，
  而 `FindAll` / JSON / `.pb` 里那些行都还在——数据没丢，丢的是索引，且零报错。

### 热更：只存 id，不存指针

`Load()` 整批建好新快照再换掉旧的。**C++ 侧旧快照当场析构**，任何存着的
`const XxxTable*` 立刻变野指针，而且不会有任何报错；Go/Java 有 GC 不会崩，
但会永远拿到热更前的旧值。

```
✅ 存 id，用的时候现查
❌ 把行指针/引用存进成员、容器、闭包、协程帧
```

生成的头文件里写着同一条契约，改动那段注释前先想清楚。

### 行 id 只增不删（`(cfg_ids_permanent)`，2026-11-11）

玩家存档、在途的资产通道指令按 id 引用的表（目前是 `Item`、`Pet`），在主消息上写表级 option：

```proto
message ItemTable {
  option (cfg_sheet)         = "Item";
  option (cfg_ids_permanent) = true;
  ...
}
```

- 导表器把这张表出现过的全部 id 记在台账 `tools/data_table_exporter/state/permanent_ids/<表名小写>_ids.txt`（进 git），
  表里少了台账里的 id 就**整批不产出**（`core/permanent_ids.py`）。新 id 在导表时自动追加，台账与产物同一次提交。
- 不再产出某个物品 / 宝宝：去掉掉落、商店、奖励里对它的引用，**表行保留**，已持有的照常生效。
- 只在开发期存在、从未发布过的行要删：在同一次提交里从台账删掉这个 id，提交说明写明理由。
- 为什么：scene 遇到本节点表里查不到的 id 时，只有表行永不删除，才能判定它是更新版本或尚未热更到的行、
  回 RETRY 等它出现，而不是把引用它的资产当坏数据拒掉（`docs/design/mixed-version-persist-field-protection.md` §13 第 10 条）。

### 位序存储（`(cfg_bit_index) = true`）与删 ID 红线
- 玩家针对表 ID 的纯布尔完成/领取/解锁状态优先用位图存，在主键 `id` 列打 `(cfg_bit_index) = true`（与 `(cfg_multi)` 互斥），由导表器经 `tools/data_table_exporter/state/mapping/table_index_mapping/<表名>_mapping.json` 生成紧凑位序。
- **策划红线**：上线后**绝不复用已删除/废弃的旧 ID**（旧 ID 在 `*_mapping.json` 永久占坑不回收，复用会让老玩家的历史 `1` 位静默串味到新内容）；有进行中/待领奖老玩家的表优先下架/禁用而非物理删行，物理删行前先与程序确认清理及计数口径。
- **程序红线**：严禁手改/删除 `*_mapping.json` 或对已发布表用 `--bitindex-bootstrap`；查表判空容错、不拿 `bitset.count()` 充当有效完成总数、混跑高位未知 bit 原样回写。
- 完整机制、风险示例与程序/策划红线清单见 → [docs/rules/bit-storage-and-table-ids.md](../docs/rules/bit-storage-and-table-ids.md)。

### 改字段号 / 删字段
- **字段号是 wire 契约**。`.pb` 是入库的二进制，改号 = 让所有存量数据那一段被错读，
  而且 `ParseFromString` 不会报错。
- 删字段必须写 `reserved <号>;`，复用退休号会被解析器拒绝。
- 改号会在 `generated/code/proto/*.proto` 里显示成一行 diff——review 时盯住它。

### 中文说明
写在权威 schema 的 `//` 注释里。xlsx 第 5 行的中文是**投影**，由
`strip_header_rows.py --decorate` 从 schema 生成，**在表里改它不生效**，重跑会被覆盖。

## ANTI-PATTERNS
- ❌ 在 xlsx 第 2~4 行写类型/owner/选项——那套 5 行表头格式已经退休，写了也没人读。
- ❌ 手改 `generated/` 下的任何东西：那是产物，下次导表就没了。
- ❌ 复用退休的字段号，或为了「让号连续」而重排字段号。
- ❌ 改了表不跑 `gen_schema_index.py`：腐坏的索引比没有索引更坏。
- ❌ **把表行的指针/引用存起来**——热更那一刻就是野指针（C++），或永久陈旧（Go/Java）。只存 id。
- ❌ 删 `(cfg_ids_permanent)` 表（`Item`、`Pet`）的行：导表会失败。不再产出就去掉引用，表行保留。
- ❌ 给可重复主键的表加 `(cfg_bit_index)`：位序对重复 ID 没有定义，会被拒。
- ❌ 把枚举定义粘进注释（历史上 `Buff.buff_type` 就是一整段 C++ enum）——proto 里可以直接写 `enum`。

## COMMANDS
```bash
# 全量导表（会写 generated/ 与三端部署目录）
dev.bat gen

# 沙盒导表 + 与仓内产物逐字节比对（对仓库只读，改导表器后的验收判据）
py tools/data_table_exporter/tools/sandbox_export.py --out /tmp/after --compare

# schema 语法 lint + 迁移期等价对拍
py tools/data_table_exporter/tools/verify_schema_parity.py

# 重生本文件的索引块（改表后必跑；--check 用于 CI）
py tools/data_table_exporter/tools/gen_schema_index.py

# 单元测试
cd tools/data_table_exporter && py -m pytest -q
```

## NOTES
- `GlobalVariable.xlsx` 的 `to_uint32`/`to_int32`/`to_string`/`to_float`/`to_double`
  五列 owner 为空，**整列被丢弃、表里有数据也进不了产物**。这是历史坏数据，
  导表时每列会打一条 WARNING。修它会给 `GlobalVariableTable` 加 5 个字段，属独立决策。
- 复合键 `(cfg_composite)` 词表与模板都支持，但当前 **0 张表在用**，第一次使用需单独验证。
- `Operator.xlsx` / `Tip.xlsx` 表头格式与主表**不同**，走 `enum_gen.py`，不吃权威 schema。

## 配置表索引

<!-- BEGIN GENERATED: schema-index -->

<!-- 由 tools/data_table_exporter/tools/gen_schema_index.py 生成，不要手改 -->

| 表 | 源表 | 权威 schema | 列 | 字段 | 行 | 唯一键 | 多值键 | 索引 | 外键 | 位序 | 表达式 | 子消息 |
|---|---|---|---:|---:|---:|---|---|---|---|---|---|---|
| **ActivitySchedule** | `ActivitySchedule.xlsx` | `activityschedule_table.proto` | 11 | 11 | 3 | — | — | `id` | `id→Mission.id` | — | — | — |
| **ActivityVitalityReward** | `ActivityVitalityReward.xlsx` | `activityvitalityreward_table.proto` | 4 | 4 | 0 | — | — | `rule_id` `reward_id` | `rule_id→ActivityVitalityRule.id` `reward_id→Reward.id` | — | — | — |
| **ActivityVitalityRule** | `ActivityVitalityRule.xlsx` | `activityvitalityrule_table.proto` | 6 | 6 | 0 | — | — | — | — | — | — | — |
| **ActorActionCombatState** | `ActorActionCombatState.xlsx` | `actoractioncombatstate_table.proto` | 5 | 4 | 8 | — | — | — | — | — | — | — |
| **ActorActionState** | `ActorActionState.xlsx` | `actoractionstate_table.proto` | 5 | 4 | 12 | — | — | — | — | — | — | — |
| **AfkPassRule** | `AfkPassRule.xlsx` | `afkpassrule_table.proto` | 7 | 7 | 1 | — | — | — | — | — | — | — |
| **AppearanceRule** | `AppearanceRule.xlsx` | `appearancerule_table.proto` | 7 | 7 | 1 | — | — | `card_item_id` | `card_item_id→Item.id` | — | — | — |
| **AppearanceSeries** | `AppearanceSeries.xlsx` | `appearanceseries_table.proto` | 9 | 9 | 3 | — | — | — | — | — | — | — |
| **AttributeAllocRatio** | `AttributeAllocRatio.xlsx` | `attributeallocratio_table.proto` | 10 | 10 | 8 | — | — | `dimension_id` `class_id` | `dimension_id→AttributeDimension.id` | — | — | — |
| **AttributeAutoPlan** | `AttributeAutoPlan.xlsx` | `attributeautoplan_table.proto` | 14 | 6 | 6 | — | — | `class_id` `pool_id` | `pool_id→AttributePool.id` `dimension→AttributeDimension.id` | — | — | — |
| **AttributeDimension** | `AttributeDimension.xlsx` | `attributedimension_table.proto` | 12 | 12 | 8 | — | — | `pool_id` | `pool_id→AttributePool.id` | — | — | — |
| **AttributePool** | `AttributePool.xlsx` | `attributepool_table.proto` | 10 | 10 | 2 | — | — | `owner_type` | — | — | — | — |
| **AttributeRule** | `AttributeRule.xlsx` | `attributerule_table.proto` | 7 | 7 | 1 | — | — | — | — | — | — | — |
| **BaseScene** | `BaseScene.xlsx` | `basescene_table.proto` | 5 | 5 | 21 | — | — | — | — | — | — | — |
| **BattleProgressionReward** | `BattleProgressionReward.xlsx` | `battleprogressionreward_table.proto` | 6 | 6 | 20 | — | — | — | — | — | — | — |
| **BattleProgressionRule** | `BattleProgressionRule.xlsx` | `battleprogressionrule_table.proto` | 4 | 4 | 1 | — | — | — | — | — | — | — |
| **Buff** | `Buff.xlsx` | `buff_table.proto` | 64 | 54 | 134 | — | — | — | — | — | `health_regeneration:double` `bonus_damage:double` | — |
| **CharacterAppearance** | `CharacterAppearance.xlsx` | `characterappearance_table.proto` | 14 | 14 | 9 | — | — | `series_id` | `series_id→AppearanceSeries.id` | `id` | — | — |
| **Class** | `Class.xlsx` | `class_table.proto` | 11 | 9 | 9 | — | — | — | — | — | — | — |
| **Condition** | `Condition.xlsx` | `condition_table.proto` | 20 | 10 | 29 | — | — | — | — | — | — | — |
| **Cooldown** | `Cooldown.xlsx` | `cooldown_table.proto` | 2 | 2 | 9 | — | — | — | — | — | — | — |
| **Dungeon** | `Dungeon.xlsx` | `dungeon_table.proto` | 11 | 9 | 31 | — | — | `scene_id` | `scene_id→BaseScene.id` `monster→Monster.id` | — | — | — |
| **EquipAffixPool** | `EquipAffixPool.xlsx` | `equipaffixpool_table.proto` | 4 | 4 | 29 | — | — | `pool_id` `attr_id` | `attr_id→EquipAttribute.id` | — | — | — |
| **EquipAffixQuality** | `EquipAffixQuality.xlsx` | `equipaffixquality_table.proto` | 6 | 6 | 5 | — | — | — | — | — | — | — |
| **EquipAffixRule** | `EquipAffixRule.xlsx` | `equipaffixrule_table.proto` | 5 | 5 | 1 | — | — | — | — | — | — | — |
| **EquipArtifact** | `EquipArtifact.xlsx` | `equipartifact_table.proto` | 9 | 9 | 8 | — | — | — | — | — | — | — |
| **EquipAttribute** | `EquipAttribute.xlsx` | `equipattribute_table.proto` | 6 | 6 | 26 | — | — | — | — | — | — | — |
| **EquipAttributeCap** | `EquipAttributeCap.xlsx` | `equipattributecap_table.proto` | 5 | 5 | 125 | — | — | `attr_id` | `attr_id→EquipAttribute.id` | — | — | — |
| **EquipEnhance** | `EquipEnhance.xlsx` | `equipenhance_table.proto` | 10 | 10 | 23 | — | — | `material_id` `profile_id` | `material_id→Item.id` | — | — | — |
| **EquipGem** | `EquipGem.xlsx` | `equipgem_table.proto` | 4 | 4 | 12 | — | — | `id` `attr_id` | `id→Item.id` `attr_id→EquipAttribute.id` | — | — | — |
| **EquipGemSocket** | `EquipGemSocket.xlsx` | `equipgemsocket_table.proto` | 5 | 5 | 6 | — | — | `equip_kind` | — | — | — | — |
| **EquipSlot** | `EquipSlot.xlsx` | `equipslot_table.proto` | 3 | 3 | 11 | — | — | — | — | — | — | — |
| **EquipV1Affix** | `EquipV1Affix.xlsx` | `equipv1affix_table.proto` | 10 | 10 | 36 | — | — | `pool` `material_id` | `material_id→Item.id` | — | — | — |
| **FestivalBeastActivity** | `FestivalBeastActivity.xlsx` | `festivalbeastactivity_table.proto` | 20 | 20 | 1 | — | — | `battle_config_id` `reward_pet_table_id` | `battle_config_id→Dungeon.id` `reward_pet_table_id→Pet.id` | — | — | — |
| **FestivalBeastTier** | `FestivalBeastTier.xlsx` | `festivalbeasttier_table.proto` | 8 | 8 | 8 | — | — | — | — | — | — | — |
| **GlobalVariable** | `GlobalVariable.xlsx` | `globalvariable_table.proto` | 8 | 1 | 16 | — | — | — | — | — | — | — |
| **GuildActivity** | `GuildActivity.xlsx` | `guildactivity_table.proto` | 15 | 15 | 3 | — | — | `reward_id` `dungeon_id` | `reward_id→Reward.id` `dungeon_id→Dungeon.id` | — | — | — |
| **GuildDonate** | `GuildDonate.xlsx` | `guilddonate_table.proto` | 8 | 8 | 3 | — | — | — | — | — | — | — |
| **GuildLevel** | `GuildLevel.xlsx` | `guildlevel_table.proto` | 4 | 4 | 10 | — | — | — | — | — | — | — |
| **GuildRule** | `GuildRule.xlsx` | `guildrule_table.proto` | 13 | 13 | 1 | — | — | — | — | — | — | — |
| **GuildShop** | `GuildShop.xlsx` | `guildshop_table.proto` | 9 | 9 | 17 | — | — | `item_id` | `item_id→Item.id` | — | — | — |
| **GuildWarMilestone** | `GuildWarMilestone.xlsx` | `guildwarmilestone_table.proto` | 9 | 9 | 4 | — | — | `reward_item1_id` `reward_item2_id` | `reward_item1_id→Item.id` `reward_item2_id→Item.id` | — | — | — |
| **GuildWarRule** | `GuildWarRule.xlsx` | `guildwarrule_table.proto` | 31 | 31 | 1 | — | — | `battle_config_id` | `battle_config_id→Dungeon.id` | — | — | — |
| **Item** | `Item.xlsx` | `item_table.proto` | 23 | 18 | 145 | — | — | `affix_rule` `forge_material_id` `artifact_id` | `affix_rule→EquipAffixRule.id` `forge_material_id→Item.id` `artifact_id→EquipArtifact.id` | — | — | `base_attr` |
| **MessageLimiter** | `MessageLimiter.xlsx` | `messagelimiter_table.proto` | 4 | 4 | 118 | — | — | — | — | — | — | — |
| **Mirror** | `Mirror.xlsx` | `mirror_table.proto` | 3 | 3 | 2 | — | — | `scene_id` `main_scene_id` | `scene_id→BaseScene.id` `main_scene_id→World.id` | — | — | — |
| **Mission** | `Mission.xlsx` | `mission_table.proto` | 15 | 9 | 17 | — | — | `reward_id` | `reward_id→Reward.id` `condition_id→Condition.id(组)` | `id` | — | — |
| **Monster** | `Monster.xlsx` | `monster_table.proto` | 19 | 13 | 57 | — | — | `capture_pet_table_id` | `capture_pet_table_id→Pet.id` | — | — | `drop` |
| **MysteryMerchantGoods** | `MysteryMerchantGoods.xlsx` | `mysterymerchantgoods_table.proto` | 8 | 8 | 12 | — | — | `item_id` | `item_id→Item.id` | — | — | — |
| **MysteryMerchantRule** | `MysteryMerchantRule.xlsx` | `mysterymerchantrule_table.proto` | 9 | 9 | 1 | — | — | — | — | — | — | — |
| **MysteryMerchantSpot** | `MysteryMerchantSpot.xlsx` | `mysterymerchantspot_table.proto` | 6 | 6 | 15 | — | — | `scene_config_id` | — | — | — | — |
| **Pet** | `Pet.xlsx` | `pet_table.proto` | 28 | 19 | 23 | — | — | — | — | — | — | — |
| **PetCosmetic** | `PetCosmetic.xlsx` | `petcosmetic_table.proto` | 14 | 14 | 3 | — | — | — | — | `id` | — | — |
| **PetEntry** | `PetEntry.xlsx` | `petentry_table.proto` | 10 | 10 | 21 | — | — | — | — | — | — | — |
| **PetRule** | `PetRule.xlsx` | `petrule_table.proto` | 9 | 9 | 1 | — | — | — | — | — | — | — |
| **PetTier** | `PetTier.xlsx` | `pettier_table.proto` | 9 | 9 | 5 | — | — | — | — | — | — | — |
| **PlayMode** | `PlayMode.xlsx` | `playmode_table.proto` | 10 | 6 | 42 | — | — | `battle_config_id` | `battle_config_id→Dungeon.id` | — | — | — |
| **PlayerLevelExp** | `PlayerLevelExp.xlsx` | `playerlevelexp_table.proto` | 2 | 2 | 85 | — | — | — | — | — | — | — |
| **RankedPreset** | `RankedPreset.xlsx` | `rankedpreset_table.proto` | 20 | 15 | 6 | — | — | `pet_table_id` | `pet_table_id→Pet.id` `skill_ids→Skill.id` `pet_entry_ids→PetEntry.id` | — | — | — |
| **RankedRule** | `RankedRule.xlsx` | `rankedrule_table.proto` | 23 | 23 | 1 | — | — | `battle_config_id` `reference_class_id` | `battle_config_id→Dungeon.id` `reference_class_id→Class.id` | — | — | — |
| **Reward** | `Reward.xlsx` | `reward_table.proto` | 5 | 2 | 7 | — | — | — | — | `id` | — | `reward` |
| **RewardedAdSlot** | `RewardedAdSlot.xlsx` | `rewardedadslot_table.proto` | 11 | 11 | 1 | — | — | — | — | — | — | — |
| **RoleNameRule** | `RoleNameRule.xlsx` | `rolenamerule_table.proto` | 11 | 11 | 1 | — | — | — | — | — | — | — |
| **SeasonRule** | `SeasonRule.xlsx` | `seasonrule_table.proto` | 17 | 17 | 2 | — | — | — | — | — | — | — |
| **SeasonSeal** | `SeasonSeal.xlsx` | `seasonseal_table.proto` | 12 | 12 | 8 | — | — | `season_id` | `season_id→SeasonRule.id` | — | — | — |
| **Skill** | `Skill.xlsx` | `skill_table.proto` | 87 | 62 | 149 | — | — | — | — | — | `damage:double` | `cost_resource` `required_item` `required_resource` |
| **SkillPermission** | `SkillPermission.xlsx` | `skillpermission_table.proto` | 7 | 2 | 3 | — | — | — | — | — | — | — |
| **SkillScrollCraft** | `SkillScrollCraft.xlsx` | `skillscrollcraft_table.proto` | 16 | 16 | 7 | — | — | `fragment_item_id` | `fragment_item_id→Item.id` | — | — | — |
| **SkillVfxCosmetic** | `SkillVfxCosmetic.xlsx` | `skillvfxcosmetic_table.proto` | 16 | 16 | 3 | — | — | `skill_table_id` | `skill_table_id→Skill.id` | `id` | — | — |
| **SpiritShopGoods** | `SpiritShopGoods.xlsx` | `spiritshopgoods_table.proto` | 21 | 21 | 9 | — | — | `item_id_1` | `item_id_1→Item.id` | — | — | — |
| **SpiritShopLimitGroup** | `SpiritShopLimitGroup.xlsx` | `spiritshoplimitgroup_table.proto` | 4 | 4 | 1 | — | — | — | — | — | — | — |
| **Test** | `Test.xlsx` | `test_table.proto` | 24 | 6 | 6 | — | — | — | — | — | — | `testobj` |
| **TestMultiKey** | `TestMultiKey.xlsx` | `testmultikey_table.proto` | 33 | 14 | 6 | `string_key` `uint32_key` `int32_key` | `m_string_key` `m_uint32_key` `m_int32_key` | `level` `test_ref` | `test_ref→Test.id` `test_refs→Test.id(组)` | — | — | `testobj1` |
| **TournamentReward** | `TournamentReward.xlsx` | `tournamentreward_table.proto` | 18 | 12 | 9 | — | — | `tier_kind` | `item_ids→Item.id` | — | — | — |
| **TournamentRule** | `TournamentRule.xlsx` | `tournamentrule_table.proto` | 31 | 31 | 1 | — | — | `battle_config_id` `salute_item_id` | `battle_config_id→Dungeon.id` `salute_item_id→Item.id` | — | — | — |
| **TravelPassSeason** | `TravelPassSeason.xlsx` | `travelpassseason_table.proto` | 15 | 15 | 1 | — | — | — | — | — | — | — |
| **TravelPassTier** | `TravelPassTier.xlsx` | `travelpasstier_table.proto` | 14 | 14 | 10 | — | — | `pass_id` | `pass_id→TravelPassSeason.id` | — | — | — |
| **TrialJournalEntry** | `TrialJournalEntry.xlsx` | `trialjournalentry_table.proto` | 16 | 16 | 12 | — | — | `chapter_id` | — | — | — | — |
| **TrialJournalMilestone** | `TrialJournalMilestone.xlsx` | `trialjournalmilestone_table.proto` | 15 | 15 | 11 | — | — | `scope_chapter_id` | — | `id` | — | — |
| **TrialJournalSeal** | `TrialJournalSeal.xlsx` | `trialjournalseal_table.proto` | 8 | 8 | 33 | — | — | `entry_id` | `entry_id→TrialJournalEntry.id` | `id` | — | — |
| **World** | `World.xlsx` | `world_table.proto` | 2 | 2 | 16 | — | — | `scene_id` | `scene_id→BaseScene.id` | — | — | — |
| **ZoneGroup** | `ZoneGroup.xlsx` | `zonegroup_table.proto` | 5 | 4 | 2 | — | — | — | — | — | — | — |

合计 **82** 张表、**1036** 个物理列、**883** 个进产物的字段。

<!-- END GENERATED -->

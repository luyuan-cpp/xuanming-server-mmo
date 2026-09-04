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
| **ActorActionCombatState** | `ActorActionCombatState.xlsx` | `actoractioncombatstate_table.proto` | 8 | 2 | 4 | — | — | — | — | — | — | `state` |
| **ActorActionState** | `ActorActionState.xlsx` | `actoractionstate_table.proto` | 8 | 2 | 4 | — | — | — | — | — | — | `state` |
| **BaseScene** | `BaseScene.xlsx` | `basescene_table.proto` | 2 | 2 | 21 | — | — | — | — | — | — | — |
| **Buff** | `Buff.xlsx` | `buff_table.proto` | 32 | 22 | 20 | — | — | — | — | — | `health_regeneration:double` `bonus_damage:double` | — |
| **Class** | `Class.xlsx` | `class_table.proto` | 4 | 2 | 9 | — | — | — | — | — | — | — |
| **Condition** | `Condition.xlsx` | `condition_table.proto` | 20 | 10 | 29 | — | — | — | — | — | — | — |
| **Cooldown** | `Cooldown.xlsx` | `cooldown_table.proto` | 2 | 2 | 9 | — | — | — | — | — | — | — |
| **Dungeon** | `Dungeon.xlsx` | `dungeon_table.proto` | 4 | 4 | 3 | — | — | `scene_id` | `scene_id→BaseScene.id` | — | — | — |
| **EquipSlot** | `EquipSlot.xlsx` | `equipslot_table.proto` | 2 | 2 | 3 | — | — | — | — | — | — | — |
| **GlobalVariable** | `GlobalVariable.xlsx` | `globalvariable_table.proto` | 8 | 1 | 16 | — | — | — | — | — | — | — |
| **Item** | `Item.xlsx` | `item_table.proto` | 3 | 3 | 28 | — | — | — | — | — | — | — |
| **MessageLimiter** | `MessageLimiter.xlsx` | `messagelimiter_table.proto` | 4 | 4 | 4 | — | — | — | — | — | — | — |
| **Mirror** | `Mirror.xlsx` | `mirror_table.proto` | 3 | 3 | 2 | — | — | `scene_id` `main_scene_id` | `scene_id→BaseScene.id` `main_scene_id→World.id` | — | — | — |
| **Mission** | `Mission.xlsx` | `mission_table.proto` | 15 | 9 | 17 | — | — | `reward_id` | `reward_id→Reward.id` `condition_id→Condition.id(组)` | `id` | — | — |
| **Monster** | `Monster.xlsx` | `monster_table.proto` | 2 | 1 | 16 | — | — | — | — | — | — | — |
| **Reward** | `Reward.xlsx` | `reward_table.proto` | 5 | 2 | 7 | — | — | — | — | `id` | — | `reward` |
| **Skill** | `Skill.xlsx` | `skill_table.proto` | 45 | 23 | 13 | — | — | — | — | — | `damage:double` | `cost_resource` `required_item` `required_resource` |
| **SkillPermission** | `SkillPermission.xlsx` | `skillpermission_table.proto` | 7 | 2 | 3 | — | — | — | — | — | — | — |
| **Test** | `Test.xlsx` | `test_table.proto` | 24 | 6 | 6 | — | — | — | — | — | — | `testobj` |
| **TestMultiKey** | `TestMultiKey.xlsx` | `testmultikey_table.proto` | 33 | 14 | 6 | `string_key` `uint32_key` `int32_key` | `m_string_key` `m_uint32_key` `m_int32_key` | `level` `test_ref` | `test_ref→Test.id` `test_refs→Test.id(组)` | — | — | `testobj1` |
| **World** | `World.xlsx` | `world_table.proto` | 2 | 2 | 16 | — | — | `scene_id` | `scene_id→BaseScene.id` | — | — | — |

合计 **21** 张表、**233** 个物理列、**118** 个进产物的字段。

<!-- END GENERATED -->

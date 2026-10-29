# 装备属性系统(问道式:基础属性 + 蓝/粉/黄随机属性)

> 落地日期：2026-10-07 / 08；2026-10-15 按 HEAD `6281e4d27` 与进度记录更新状态。
> 一、二期已在主干完成正式导表和全量 proto-gen，生成产物已入库；2026-10-13 装备定向 robot 测试
> 32 个顶层用例、连同子用例 253 项通过。**相关 C++ 完整构建、C++ 单测与联机冒烟仍待验证**，见 §9。
> 客户端生成同步在当日进度中有记录，本次没有读取独立客户端或核对其当前运行结果，不再把旧隔离工作树、
> 未发消息号或预期 CS0117 当作现状。客户端与 Java 版的当前状态须分别核对其仓库记录。
> **二期（强化 + 镶嵌）见 [equipment-enhance-inlay.md](equipment-enhance-inlay.md)**；两期首次生成已完成，
> 不要再按首次发号流程重新占号。后续协议变更仍按正常生成门禁执行。
> 实现分布:配表 `data/EquipAttribute*.xlsx` / `data/EquipAffix*.xlsx` + `data/Item.xlsx` 新列;
> 协议 `proto/common/component/item_base_comp.proto`、`proto/scene/player_bag.proto`、
> `proto/scene/player_attribute.proto`、`proto/battle/battle_data.proto`;
> 纯规则 `cpp/libs/services/scene/player/system/equip_attribute_rules.h`;
> 编排 `cpp/libs/services/scene/player/system/player_equip.{h,cpp}`;
> 容器原语 `cpp/libs/modules/bag/*`;战斗 `cpp/libs/services/battle/system/turn_battle_engine.cpp`;
> 客户端 `../mmorpg-client` 的 `Game/PlayerFeatures` + `UI/Ugui/Gameplay`。
>
> 本文 §2–§6 是**实现契约**:字段号、表列、函数签名以此为准;实现与本文不一致时改其中一个并在 §10 记录。

2026-10-23 当前数值见 §2.6：四部位补齐十级一档，共 36 件；一级属性、准确与随机伤害上限收紧，气血、防御等预算同步重定。
本批未改协议；已完成独立数值设计镜像及战斗测试工程完整 282 项回归，联机与装备穿脱存盘完整链路尚未验收。
Java 独立仓 `D:/luyuan/xuanming-server-mmo` 在本机不存在，当前共享配表待同步。

## 1. 目标与非目标

### 1.1 用户需求(2026-10-07,四张问道截图)

- 武器可随机到的属性(武器池,12 条):伤害、力量、体质、灵力、敏捷、准确、物理连击率、反击率、
  物理必杀率、所有技能上升、忽视所有抗异常、法术必杀率。
  (相对问道:删 忽视所有抗性 / 所有相性 / 金木水火土相性,加 法术必杀率。)
- 防具可随机到的属性(防具池,17 条):防御、气血、法力、力量、体质、灵力、敏捷、所有属性、反震率、
  抗中毒、抗冰冻、抗昏睡、抗遗忘、抗混乱、所有抗异常、抗法术、抗物理。
  (相对问道:删 金木水火土抗性 / 所有抗性,加 抗法术 / 抗物理。)
- 装备 tooltip 形如问道:名称、角色要求、基础属性(武器「伤害」,鞋「防御」「速度」),
  再按颜色分档列随机属性,每行「属性名 当前值/上限值」。

### 1.2 本次做

1. 属性定义、属性池、上限、掷值规则全部数据化(4 张新表 + Item 表新列)。
2. 装备实例携带随机属性行(蓝 / 粉 / 黄),随存档、跨区、回滚快照走。
3. 创建装备实例时掷属性(掉落、任务奖励、资产通道、GM 发物全覆盖)。
4. 穿上 / 卸下(此前没有任何穿脱能力)。
5. 装备属性作用到角色面板与回合制战斗。
6. 客户端:背包页加装备栏、装备 tooltip、穿 / 脱按钮;属性面板显示装备加成与战斗类属性。
7. GM 发物(dev/test 才开)+ robot 冒烟 + 单测。

### 1.3 本次不做(实例与协议已留位)

| 不做 | 说明 |
|---|---|
| 炼化(重掷蓝 / 粉 / 黄) | 掷值函数 `equiprules::RollAffixes` 已是独立纯函数,炼化时直接复用 |
| 改造(改 N、百分比加成) | 二期已做,即「强化」:见 [equipment-enhance-inlay.md](equipment-enhance-inlay.md)。强化等级放在 `EquipInstanceData` 里,`ItemEntry` 6 号 `enchant_level` 不再启用 |
| 进化星级、套装 / 绿色属性、共鸣 | 颜色档枚举预留 `4 = 绿`,本次不掷、不显示 |
| 限制交易 / 绑定 | `ItemEntry` 9–11 号仍预留 |
| 聚宝斋寄售带属性的装备 | `AssetItemInstance` 未扩;P3 寄售前必须在销毁前抓整份实例(见 §8) |
| 混乱状态本身 | 引擎没有「混乱」buff 类型;抗混乱只贯通到面板与战斗快照,引擎暂不消费 |

### 1.4 用户未明示、按默认值落地的决定(可随时改)

| # | 决定 | 默认值 | 改它要动哪里 |
|---|---|---|---|
| D1 | 粉 / 黄属性从哪来 | 炼化未做之前,创建时按 `EquipAffixRule` 的概率直接掷出 | 只改表(概率填 0 即关闭) |
| D2 | 「准确」的效果 | **只加物理伤害**(本作没有命中 / 闪避判定,做成命中率等于无效属性) | `EquipAttribute` 表该行的 `effect` / `effect_param` |
| D3 | 连击 | 普攻命中后按连击率追加 **1 段**;追加段按普攻公式独立结算(可独立必杀)再 ×50% | `turn_battle_constants.h` |
| D4 | 反击 | 被普攻打中且仍能行动时按反击率还一次普攻;反击不再触发连击 / 反击 / 反震 | 同上 |
| D5 | 反震 | 受到普攻伤害**且仍存活**时按反震率把实扣伤害的 50% 弹回出手者(致死的那一段不反震,不会同归于尽) | 同上 |
| D6 | 必杀倍率 | 沿用引擎现值 ×2;物理 / 法术必杀率分别叠加在角色基础暴击率上 | 同上 |
| D7 | 抗物理 / 抗法术 | 分别叠加在角色基础抗性上,与护甲 / 防御共用 60% 常驻减伤封顶 | `combat_damage_rules.h`(与实时技能共用,慎改) |
| D8 | 昏睡 / 遗忘 | 引擎无同名状态:抗昏睡 → 眩晕(30),抗遗忘 → 沉默(31) | `turn_battle_constants.h` 的映射表 |
| D9 | 全部数值 | 见 §2.6，2026-10-23 当前基线按本作单位重定，已做独立公式镜像验收；原生与联机待验 | 只改表 |
| D10 | 装备的一级属性点 | 走每点固定系数(2026-09-13 已定口径:自然成长与装备加成不走加点公式) | — |

## 2. 配置表

导表流程见 `data/AGENTS.md`。xlsx 由幂等脚本 `tools/scripts/equip_xlsx_patch.py` 生成 / 打补丁
(仿 `guild_b5a_xlsx_patch.py`;多会话共写的 `Item.xlsx` / `EquipSlot.xlsx` / `Tip.xlsx` /
`MessageLimiter.xlsx` 只做 `load → insert → save → 读回核对`,绝不整表写回)。

### 2.1 `EquipAttribute`(新表)—— 一行一个属性

| 列 | 类型 | 含义 |
|---|---|---|
| `id` | uint32 | 属性 id(主键) |
| `name` | string | 显示名;由服务器下发给客户端(客户端零配表) |
| `effect` | uint32 | 效果种类,见下表(C++ 只按它分支,不按 id 分支) |
| `effect_param` | uint32 | 效果参数 |
| `percent` | uint32 | 0 = 点数;1 = 整数百分点(显示成 `N%`) |
| `sort` | uint32 | 显示排序(升序) |

效果种类(`equiprules::EffectKind`,枚举值即表里填的数):

| effect | 含义 | effect_param |
|---|---|---|
| 1 | 一级属性点 | `AttributeDimension.id`(101 体质 / 102 灵力 / 103 力量 / 104 敏捷) |
| 2 | 所有一级属性点(角色池四项各 +N) | 0 |
| 3 | 二级属性平加 | `DerivedStat`:1 气血上限 / 2 法力上限 / 3 物伤 / 4 法伤 / 5 速度 / 6 防御 |
| 4 | 伤害(物伤与法伤各 +N) | 0 |
| 5 | 战斗类属性 | `CombatStat`,数值 **等于** `CombatAttributes` 的字段号(§3.3) |

属性清单(id 一经使用不再改义):

| id | name | effect | param | percent | 池 |
|---:|---|---:|---:|---:|---|
| 1 | 伤害 | 4 | 0 | 0 | 武器 |
| 2 | 准确 | 3 | 3 | 0 | 武器 |
| 3 | 力量 | 1 | 103 | 0 | 武器 / 防具 |
| 4 | 体质 | 1 | 101 | 0 | 武器 / 防具 |
| 5 | 灵力 | 1 | 102 | 0 | 武器 / 防具 |
| 6 | 敏捷 | 1 | 104 | 0 | 武器 / 防具 |
| 7 | 所有属性 | 2 | 0 | 0 | 防具 |
| 8 | 防御 | 3 | 6 | 0 | 防具 |
| 9 | 气血 | 3 | 1 | 0 | 防具 |
| 10 | 法力 | 3 | 2 | 0 | 防具 |
| 11 | 速度 | 3 | 5 | 0 | —(只作鞋的基础属性) |
| 12 | 物理必杀率 | 5 | 1 | 1 | 武器 |
| 13 | 法术必杀率 | 5 | 2 | 1 | 武器 |
| 14 | 物理连击率 | 5 | 3 | 1 | 武器 |
| 15 | 反击率 | 5 | 4 | 1 | 武器 |
| 16 | 反震率 | 5 | 5 | 1 | 防具 |
| 17 | 所有技能上升 | 5 | 6 | 0 | 武器 |
| 18 | 忽视所有抗异常 | 5 | 7 | 1 | 武器 |
| 19 | 抗中毒 | 5 | 8 | 1 | 防具 |
| 20 | 抗冰冻 | 5 | 9 | 1 | 防具 |
| 21 | 抗昏睡 | 5 | 10 | 1 | 防具 |
| 22 | 抗遗忘 | 5 | 11 | 1 | 防具 |
| 23 | 抗混乱 | 5 | 12 | 1 | 防具 |
| 24 | 所有抗异常 | 5 | 13 | 1 | 防具 |
| 25 | 抗法术 | 5 | 14 | 1 | 防具 |
| 26 | 抗物理 | 5 | 15 | 1 | 防具 |

### 2.2 `EquipAttributeCap`(新表)—— 上限按「属性 × 装备等级档」

| 列 | 类型 | 含义 |
|---|---|---|
| `id` | uint32 | 行号(主键,`attr_id * 1000 + level`) |
| `attr_id` | uint32 | `EquipAttribute.id`(外键、索引) |
| `level` | uint32 | 等级档起点(含)。取 `level <= 装备等级` 的最大档;一档都不满足 = 该属性在这件装备上不可用 |
| `min_value` | uint32 | 掷值下限(≥ 1) |
| `cap` | uint32 | 上限;tooltip 的「/上限值」 |

掷值 = `[min_value, cap]` 均匀整数。上限**不入库**,显示与校验时按 (attr_id, 装备等级) 现查。

### 2.3 `EquipAffixPool`(新表)—— 属性池

| 列 | 类型 | 含义 |
|---|---|---|
| `id` | uint32 | 行号(主键,`pool_id * 1000 + attr_id`) |
| `pool_id` | uint32 | 池号(索引):1 = 武器池,2 = 防具池 |
| `attr_id` | uint32 | `EquipAttribute.id`(外键) |
| `weight` | uint32 | 权重(> 0) |

池是数据,不在 C++ 里写 `if 部位 == 武器`(AGENTS §11.6);以后加首饰池只加行。

### 2.4 `EquipAffixRule`(新表)—— 掷几条

| 列 | 类型 | 含义 |
|---|---|---|
| `id` | uint32 | 规则号(主键) |
| `blue_min` / `blue_max` | uint32 | 蓝属性条数区间(均匀),蓝属性之间互不重复 |
| `pink_rate` | uint32 | 掷出 1 条粉属性的概率(万分比);可与蓝重复 |
| `yellow_rate` | uint32 | 掷出 1 条黄属性的概率(万分比);可与蓝 / 粉重复 |

初始 1 行:`id=1, blue 1–3, pink 3000, yellow 1500`。

### 2.5 `Item` 表新列(字段号接在现有 6 之后)

| 号 | 列 | 类型 | 含义 |
|---:|---|---|---|
| 7 | `name` | string | 物品名(此前 `BagItemInfo.name` 恒为空) |
| 8 | `description` | string | 描述 |
| 9 | `icon_key` | string | 图标 Resources 路径(`UI/` 开头),空 = 客户端兜底图标 |
| 10 | `equip_level` | uint32 | 佩戴等级,同时是查上限的装备等级 |
| 11 | `equip_class` | uint32 | 职业要求(`Class.id`),0 = 不限 |
| 12 | `affix_pool` | uint32 | 随机属性池(`EquipAffixPool.pool_id`),0 = 不掷 |
| 13 | `affix_rule` | uint32 | 掷值规则(`EquipAffixRule.id`),0 = 不掷 |
| 14 | `base_attr` | repeated 子消息 ×3 | 基础属性:`base_attr_id`(`EquipAttribute.id`)+ `base_attr_value` |

约束:`equip_kind != 0` 的行必须 `max_stack_size == 1`(可堆叠会把不同属性的实例并成一堆)。
`PlayerEquipSystem::InstallItemInitializer` 启动时逐行校验并打 ERROR;`ItemStore::CanStack` 对
`has_equip()` 的实例一律返回 false 作为兜底。

### 2.6 当前数据（2026-10-23 基线，D9）

**部位与槽位**(`EquipSlot` 追加行;既有 0–2 号槽与部位 1 / 2 是单测夹具,不动):

| 槽 id | equip_kind | name(`EquipSlot` 新列,字段号 3) |
|---:|---:|---|
| 3 | 11 | 武器 |
| 4 | 12 | 帽子 |
| 5 | 13 | 衣服 |
| 6 | 14 | 鞋子 |

**装备档位**：四部位各有 1 / 10 / 20 / 30 / 40 / 50 / 60 / 70 / 80 九档，共 36 件。
旧 20 件保留既有 id（1101–1105 / 1201–1205 / 1301–1305 / 1401–1405），对应 1 / 20 / 40 / 60 / 80 档；
新增 16 件的 id = `equip_kind × 100 + equip_level`，仅用于新增 10 / 30 / 50 / 70 档，例如 1110 为 10 级武器档、1130 为 30 级武器档。
所有装备 `equip_class = 0`、`affix_rule = 1`；武器 `affix_pool = 1`，防具 `affix_pool = 2`。脚本逐档检查唯一装备与基础属性引用。

| 部位 | 基础属性(L = equip_level) |
|---|---|
| 武器(kind 11) | 伤害 `20L + 20` |
| 帽子(kind 12) | 防御 `6L + 6` |
| 衣服(kind 13) | 防御 `12L + 12` |
| 鞋子(kind 14) | 防御 `6L + 6`、速度 `4L + 8` |

**随机属性区间**（`EquipAttributeCap` 仍为 1 / 20 / 40 / 60 / 80 五档，取不超过装备等级的最大档）：
本批修改的属性 id 1–10、12–17 改为 `min_value = max(1, ceil(cap × 0.5))`；
未改的忽视异常、各类抗性 id 18–26 保留 `max(1, ceil(cap × 0.3))`。速度 id 11 只作为鞋子基础属性，没有随机上限行。
下表每格为「下限～上限」，不是固定掷值。

| 属性 | L1 | L20 | L40 | L60 | L80 |
|---|---:|---:|---:|---:|---:|
| 伤害 | 10～20 | 75～150 | 150～300 | 225～450 | 300～600 |
| 准确 | 8～15 | 50～100 | 100～200 | 150～300 | 200～400 |
| 力量 / 体质 / 灵力 / 敏捷 | 1～1 | 1～2 | 2～3 | 2～4 | 3～6 |
| 所有属性 | 1～1 | 1～1 | 1～1 | 1～2 | 1～2 |
| 防御 | 12～24 | 90～180 | 180～360 | 270～540 | 360～720 |
| 气血 | 15～30 | 120～240 | 240～480 | 360～720 | 480～960 |
| 法力 | 10～20 | 80～160 | 160～320 | 240～480 | 320～640 |
| 物理必杀率 / 法术必杀率 / 物理连击率 / 反击率 / 反震率(%) | 1～1 | 1～2 | 2～3 | 2～4 | 3～5 |
| 所有技能上升 | 1～1 | 1～1 | 1～1 | 1～2 | 1～2 |
| 忽视所有抗异常(%) | 1～2 | 2～5 | 3～10 | 5～15 | 6～20 |
| 抗中毒 / 抗冰冻 / 抗昏睡 / 抗遗忘 / 抗混乱(%) | 1～2 | 2～5 | 3～10 | 5～15 | 6～20 |
| 所有抗异常(%) | 1～2 | 2～4 | 3～8 | 4～12 | 5～15 |
| 抗法术 / 抗物理(%) | 1～1 | 1～2 | 1～3 | 2～4 | 2～5 |

参照量（不加点、无装备、被动放大前，L 级）：物伤 50L、法伤 40L、防御 60L、速度 240 + 36L、气血 500 + 50L、法力 800 + 40L。
装备一级点仍走固定系数（D10），例如当前 80 档单条力量最多 +6 = 物伤 +300；破军满投 425 点增加物伤 +5312.5，尚未计被动。
普通装备保留基础武器伤害与防具成长；收紧一级点及准确，避免同名词条轻易盖过完整加点。

独立脚本 `tools/scripts/verify_turn_balance.py` 用实际导出表核对数值。普通装设为四部位可穿最高档基础 + 两条互异蓝词条取 50% 上限（不低于下限），
无强化、粉黄或宝石。85 级普通蓝词条按每项属性选最有利组合，收益占完整 425 点分配收益的 16.36%～44.12%；不代表极品满词条上界。
当前新表 11101 项设计检查、280 状态、172 次重数比较零失败，旧表同一最终脚本 108 项失败；证据与运行命令见 `player-attribute-allocation.md` §7。
此为数值镜像，不证明穿脱、存盘、原生实现或联机通过；本批未复跑这些完整链路。

### 2.7 tip 码(新域 `//equip_error base=28000 width=1000`)

| 码名(表里不带 `k`) | 文案 | fault |
|---|---|---|
| `EquipItemNotFound` | 物品不存在 | |
| `EquipNotEquipment` | 该物品不是装备 | |
| `EquipLevelNotEnough` | 等级不足,无法装备 | |
| `EquipClassMismatch` | 职业不符,无法装备 | |
| `EquipNoSlot` | 没有可用的装备栏位 | |
| `EquipNotEquipped` | 该装备未穿戴 | |
| `EquipBagFull` | 背包已满,无法卸下 | |
| `EquipInBattle` | 战斗中无法更换装备 | |
| `EquipFrozen` | 当前状态无法更换装备 | |
| `EquipGrantInvalid` | 发放参数无效 | |
| `EquipInternalError` | 装备操作失败,请稍后再试 | 1 |

## 3. 协议

### 3.1 实例层(`proto/common/component/item_base_comp.proto`)

```proto
// 一条装备随机属性。实例层只写「是什么」:上限、名称等可由配表重算的量不入库(AGENTS §11.6)。
message EquipAffix {
  uint32 attr_id = 1;  // EquipAttribute.id
  uint32 tier = 2;     // 颜色档:1 蓝 / 2 粉 / 3 黄 / 4 绿(预留)
  uint32 value = 3;    // 当前值
  uint32 seq = 4;      // 档内序号(0 起)。显示顺序 = (tier, seq);repeated 下标不承载语义
}

message EquipInstanceData {
  repeated EquipAffix affixes = 1;
}

message ItemComp {
  ...
  // 仅装备实例有。has_equip() == true 表示「已完成初始化」,即使一条属性都没掷出 ——
  // 搬运(穿脱 / 邮件回流)靠它保证不重掷。
  EquipInstanceData equip = 5;
}
```

`proto/common/database/bag_quest_mail_data.proto`:`ItemEntry` 加 `EquipInstanceData equip = 14;`
(6–12 是 TODO 表预定号,不占;7 号 `affixes` 的注释改为指向 14)。
存盘 / 读盘必须保留 presence:`if (item.has_equip()) entry.mutable_equip()->CopyFrom(item.equip())`,反向同理。

### 3.2 客户端协议(`proto/scene/player_bag.proto`)

```proto
message EquipAttrLineInfo {
  uint32 attr_id = 1;
  string name = 2;    // 服务器下发
  uint32 tier = 3;    // 0 基础属性 / 1 蓝 / 2 粉 / 3 黄 / 4 绿
  uint64 value = 4;
  uint64 cap = 5;     // 上限;基础属性行为 0(不显示「/上限」)
  bool percent = 6;   // true = 显示成 N%
  uint32 seq = 7;
}

message EquipSlotInfo {
  uint32 slot = 1;
  uint32 equip_kind = 2;
  string name = 3;
}

BagItemInfo 追加:
  uint32 equip_level = 9;
  uint32 equip_class = 10;                    // 0 = 不限
  string equip_class_name = 11;               // Class 表有名称列则填,否则空
  repeated EquipAttrLineInfo base_attrs = 12; // 顺序 = 表里的基础属性顺序,seq 递增
  repeated EquipAttrLineInfo affixes = 13;    // 已按 (tier, seq) 排好

BagLayoutInfo 追加:
  repeated EquipSlotInfo equip_slots = 5;     // 仅 bag_type = 2:容量内的槽位定义(含空槽),按 slot 升序;没配部位名的槽只在被占用时下发

message EquipItemRequest { uint64 item_id = 1; }
message EquipItemResponse {
  TipInfoMessage error_message = 1;
  BagInfo bag = 2;        // 人物背包全量
  BagInfo equipment = 3;  // 装备栏全量
}
message UnequipItemRequest { uint64 item_id = 1; }
message UnequipItemResponse { TipInfoMessage error_message = 1; BagInfo bag = 2; BagInfo equipment = 3; }
message GmGrantItemRequest { uint32 config_id = 1; uint32 count = 2; }
message GmGrantItemResponse { TipInfoMessage error_message = 1; BagInfo bag = 2; }

service SceneBagClientPlayer 末尾追加(顺序不可变,CallMethod 按 method index 分发):
  rpc EquipItem (EquipItemRequest) returns (EquipItemResponse);
  rpc UnequipItem (UnequipItemRequest) returns (UnequipItemResponse);
  rpc GmGrantItem (GmGrantItemRequest) returns (GmGrantItemResponse);
```

穿上规则:取接受该部位的槽位(槽号升序)里**第一个空槽**;都占着就**替换槽号最小的那个**,旧装备回背包
(刚腾出一格,必然放得下)。同部位多槽时让玩家指定换哪只,以后给请求追加字段即可。

属性面板(`proto/scene/player_attribute.proto`):

```proto
AttributeDimensionInfo 追加:
  uint32 bonus = 9;   // 装备等外部加成点数,已含在 value 内(客户端显示「170(+15)」)

message CombatAttributeInfo {
  uint32 combat_id = 1;  // CombatStat
  string name = 2;
  uint64 value = 3;      // 终值:必杀率含角色基础暴击率,抗物理 / 抗法术含基础抗性
  bool percent = 4;
  uint32 sort = 5;
}

AttributePanelInfo 追加:
  repeated CombatAttributeInfo combat = 10;  // 表完整时 15 项全量,按 sort 升序;百分比项显示夹到 100
```

### 3.3 运行时派生与战斗(`proto/common/component/actor_attribute_state_comp.proto`、`proto/battle/battle_data.proto`)

```proto
// 战斗类属性(装备等外部来源的**加成部分**,不含角色基础暴击率 / 抗性;运行时派生,不落库)。
// 百分比为整数百分点。字段号 == equiprules::CombatStat == EquipAttribute.effect_param,三者不得错位。
message CombatAttributes {
  uint64 physical_crit_rate = 1;
  uint64 magic_crit_rate = 2;
  uint64 combo_rate = 3;
  uint64 counter_rate = 4;
  uint64 reflect_rate = 5;
  uint64 skill_level_bonus = 6;
  uint64 ignore_ailment_resist = 7;
  uint64 resist_poison = 8;
  uint64 resist_freeze = 9;
  uint64 resist_sleep = 10;
  uint64 resist_forget = 11;
  uint64 resist_confusion = 12;
  uint64 resist_all_ailment = 13;
  uint64 magic_resist = 14;
  uint64 physical_resist = 15;
}

DerivedAttributesComp 追加:  CombatAttributes combat = 7;
BattlePlayerSnapshot 追加:   CombatAttributes combat = 20;
BattleActorState 追加:       CombatAttributes combat = 27;

enum eBattleHitKind { BATTLE_HIT_NORMAL = 0; BATTLE_HIT_COMBO = 1; BATTLE_HIT_COUNTER = 2; BATTLE_HIT_REFLECT = 3; }
BattleEventItem 追加:        eBattleHitKind hit_kind = 14;
eBattleEventType 追加:       BATTLE_EVENT_RESIST = 15;  // 异常状态被抵抗:source = 施加者,target = 抵抗者,buff_table_id = 被抵抗的 buff
```

`battle_data.proto` 需 `import "proto/common/component/actor_attribute_state_comp.proto";`。
全部是追加字段,不新增消息号(RESIST 与 hit_kind 老客户端会忽略)。

## 4. 服务端实现契约

### 4.1 容器层(`cpp/libs/modules/bag/`)—— 不认识「属性」,只认识「整份实例」

- `using ItemInstanceInitializer = std::function<void(ItemComp&)>;`(放 `item_system.h`)。
- `BagService::SetItemInstanceInitializer(ItemInstanceInitializer)`:进程级安装;传空 = 卸载(单测用)。
  `BagService` 三个 `AddItem(s)` 重载**新铸的每一个不可叠加实例**,在进 `ItemStore` 之前调用它一次
  (条件:已安装且 `!item.has_equip()`)。挂点在 `Bag::AddNonStackableItem` 的逐件循环(所有入口的唯一汇聚点),
  由 `BagService` 把回调一路传下去;`Bag` 自己不持有全局状态。回调不许失败(无返回值)。
  还原路径(`InsertItemForRestore`)与搬运原语不调用它。
- `Bag` 新增搬运原语(仅供穿脱编排;不写流水、不过封禁闸、不掷属性):
  - `uint32_t TakeInstance(Guid guid, ItemComp& out);` —— 整份拷出并两层成对移除;不存在返回 bag 域既有的「找不到」码。
  - `uint32_t PutInstance(ItemComp item, std::optional<uint32_t> slot = std::nullopt);` —— 沿用 `item.item_id()`
    (必须有效且本包不存在);函数内把 `acquire_seq` 清 0 让本包重盖;`slot` 为空按部位 / 布局自动找位,
    指定则该槽必须空且(具名槽布局下)接受该部位。
  - `std::vector<uint32_t> SlotsAcceptingKind(uint32_t equipKind) const;` —— 槽位表里接受该部位且在容量内的槽,升序。
  - 按槽取占用者 guid 的只读查询(已有等价接口则复用)。
- `Bag::InsertItemForRestore` 新增收整份 `ItemComp` + `pos` 的重载;旧的位置参数重载保留并转调(测试大量直调)。
- `bag_marshal.cpp`:`ItemComp ↔ ItemEntry` 收成一对函数,固定包 / 动态包共用;`equip` 带 presence 往返。
- `ItemStore::CanStack`:任一方 `has_equip()` 返回 false。
- 红线:`container_layout.{h,cpp}` 不得出现 `config_id / ItemComp / item_table / entt`;`item_store` 不得出现槽位概念。

### 4.2 纯规则(`equip_attribute_rules.h`,namespace `equiprules`,只依赖 std)

```cpp
enum class AffixTier : uint8_t { kBase = 0, kBlue = 1, kPink = 2, kYellow = 3, kGreen = 4, kCount };
enum class EffectKind : uint8_t { kNone = 0, kPrimaryPoint = 1, kAllPrimaryPoints = 2, kDerivedFlat = 3, kBothAttacks = 4, kCombat = 5, kCount };
enum class DerivedStat : uint8_t { kNone = 0, kMaxHealth = 1, kMaxMana = 2, kPhysicalAttack = 3, kMagicAttack = 4, kSpeed = 5, kDefense = 6, kCount };
enum class CombatStat : uint8_t { kNone = 0, kPhysicalCritRate = 1, kMagicCritRate = 2, kComboRate = 3, kCounterRate = 4,
    kReflectRate = 5, kSkillLevelBonus = 6, kIgnoreAilmentResist = 7, kResistPoison = 8, kResistFreeze = 9, kResistSleep = 10,
    kResistForget = 11, kResistConfusion = 12, kResistAllAilment = 13, kMagicResist = 14, kPhysicalResist = 15, kCount };

struct EquipBonus {
    std::map<uint32_t, uint64_t> primaryPoints;   // dimension id -> 点数
    uint64_t allPrimaryPoints{0};
    std::array<uint64_t, static_cast<std::size_t>(DerivedStat::kCount)> derivedFlat{};
    std::array<uint64_t, static_cast<std::size_t>(CombatStat::kCount)> combat{};
};

// 未知 kind / 越界 param 返回 false 且不改 bonus(调用方记 ERROR)。加法饱和,不回绕。
bool ApplyEffect(EquipBonus& bonus, uint32_t effectKind, uint32_t effectParam, uint64_t value);
uint64_t PrimaryPointsFor(const EquipBonus& bonus, uint32_t dimensionId);  // 单项 + 所有属性

struct PoolEntry { uint32_t attrId{0}; uint32_t weight{0}; };
struct ValueRange { uint32_t minValue{0}; uint32_t cap{0}; };   // cap == 0 = 本档不可用
struct RollRule { uint32_t blueMin{0}; uint32_t blueMax{0}; uint32_t pinkRatePermyriad{0}; uint32_t yellowRatePermyriad{0}; };
struct RolledAffix { uint32_t attrId{0}; AffixTier tier{AffixTier::kBlue}; uint32_t value{0}; uint32_t seq{0}; };

// rand(n):返回 [0, n) 均匀整数,n >= 1。rangeOf(attrId):返回该属性在本装备等级档的取值区间。
// 蓝:条数在 [blueMin, blueMax] 均匀,按权重不放回抽取;粉 / 黄:各按概率至多 1 条,按权重放回抽取。
// 池里可用属性不够时蓝属性能掷几条掷几条;任何输入都不抛异常、不死循环。
template <class RandFn, class RangeFn>
std::vector<RolledAffix> RollAffixes(const std::vector<PoolEntry>& pool, const RollRule& rule, RandFn&& rand, RangeFn&& rangeOf);

// levels 里 <= equipLevel 的最大者的下标;没有返回 -1。
int PickLevelTier(const std::vector<uint32_t>& levels, uint32_t equipLevel);
```

### 4.3 编排(`player_equip.{h,cpp}`,`class PlayerEquipSystem`,全静态)

```cpp
static void InstallItemInitializer();   // scene 启动时调一次(幂等);同时做 §2.5 的表校验
static void RollNewInstance(ItemComp& item);   // 生产随机源;非装备 / 无池无规则 = 只 mutable_equip() 置位或不动
static uint32_t Equip(entt::entity player, Guid itemId);
static uint32_t Unequip(entt::entity player, Guid itemId);
static uint32_t GmGrantItem(entt::entity player, uint32_t configId, uint32_t count);   // 走 BagService,TX_GM_GRANT
static void CollectBonus(entt::entity player, equiprules::EquipBonus& out);   // 只读 bags[kEquipment]:基础属性 + 随机属性
static void FillItemDisplay(const ItemComp& item, ::BagItemInfo& info);       // name / 描述 / 图标 / 要求 / 属性行
static void FillEquipSlots(::BagLayoutInfo& layout);   // 前置:调用方已填好 layout.capacity 与 layout.slots(见 §10.7)
struct CombatStatDisplay { std::string name; bool percent{false}; uint32_t sort{0}; };
static CombatStatDisplay DescribeCombatStat(equiprules::CombatStat stat);     // 反查 EquipAttribute 表(effect = 5 且 effect_param = stat 的行)
```

- 写入口(`Equip / Unequip / GmGrantItem`)先过:实体有效、未跨区冻结、不在战斗中(照 `player_pet.cpp` 的 `CheckWritable`)。
- 穿上顺序:全部纯校验 → 源包 `TakeInstance` → (替换时)装备栏 `TakeInstance` → 装备栏 `PutInstance(新, 槽)` →
  (替换时)背包 `PutInstance(旧)`。任何一步失败都按相反顺序放回并返回 `kEquipInternalError`(打 ERROR)。
- 成功后 `PlayerAttributeSystem::Recalculate(player, RecalcReason::kEquipmentChanged)` + `PushPanel(player)`。
- 值的显示夹取:`value` 超过当前表上限时显示 `min(value, cap)`,不改存档(策划下调上限不洗号)。
- 装备「是否穿着」= 它在不在 `bags[kEquipment]`;实例上没有「已装备」标记(AGENTS §11.6)。

### 4.4 属性计算(`player_attribute.cpp`)

- `RecalcReason` 加 `kEquipmentChanged`;它落入「按比例保持当前气血 / 法力」分支(不是补增量,否则脱穿加血装 = 免费回血)。
- `Recalculate` 内 `PlayerEquipSystem::CollectBonus` 现算,**不写** `bonus_values`(那是落库字段,留给丹药)。
  - 一级点:`natural = base_per_level × level + bonus_values[dim] + PrimaryPointsFor(equip, dim)`,只对角色池维度。
  - 二级平加:维度循环之后、取整之前加进累加器(气血 / 法力 / 物伤 / 法伤 / 速度 / 防御)。
  - 战斗类:写进 `DerivedAttributesComp.combat`(整块覆盖,不做增量维护)。
- `StandardBaseAtLevelCap` 不含装备(否则加点收益比例随装备浮动)。
- 加载顺序:`player_database.bag_component` 的还原先于 `InitializeOnLoad`(主路径,当前代码存的档都走这条)。`player_data_loader.h` 里「旧快照只有顶层 `bag_data`」的回退分支在重算之后才还原背包,那条路上装备加成要等下一次重算才计入(见 §8)。
- `BuildPanel`:`AttributeDimensionInfo.bonus` = `bonus_values + 装备点`;`combat` 15 项全量。

### 4.5 战斗(`player_battle.cpp`、`turn_battle_engine.cpp`)

- `BuildBattleSnapshot`:`snapshot.combat = derived.combat`。`InitPlayers` 拷进 `BattleActorState.combat`;怪物 / 宝宝为全 0。
- **所有新掷骰:概率为 0 时短路、不消耗随机数**(怪物、宝宝、无装备玩家的事件流与改动前逐位一致)。
  只用 `Rand01()` / `RandIndex()`。每段普攻的固定顺序:命中 → 必杀 → 落伤害 → 反震 → 连击续段;整次普攻结束后判反击。
- 必杀:`CalculateFinalDamage` 增加伤害类型参数;暴击率 = `critchance + (物理 ? physical_crit_rate : magic_crit_rate)`,夹到 [0, 100]。
- 抗性:`resistance + (物理 ? physical_resist : magic_resist)`,交给 `combat_damage_rules.h` 现有夹取(规则头不改签名)。
- 连击 / 反击 / 反震:只挂普攻(含技能校验失败降级成的普攻);常量 `kComboDamagePercent = 50`、`kMaxComboExtraHits = 1`、
  `kReflectDamagePercent = 50` 放 `turn_battle_constants.h`。事件用 `hit_kind` 标注;反击发 ATTACK + DAMAGE,
  连击同组 `hit_index` 递增,反震发 DAMAGE(source = 受击者,target = 出手者)。出手者被反震 / 反击打死走 `HandleDeath` 并终止本次行动。
- 所有技能上升:技能伤害表达式的等级参数 = `actor.level() + combat.skill_level_bonus`。
- 抗异常:`AddBuffToActor` 免疫判定之后、驱散之前;仅当施加者与目标不同队且 buff 类型在异常映射里。
  有效抵抗率 = `max(0, 单项 + 所有抗异常 − 施加者.忽视所有抗异常)`,夹到 100;0 与 ≥ 100 两端都不掷骰(必不抵抗 / 必抵抗),只有 1..99 掷一次;成功发 `BATTLE_EVENT_RESIST` 并返回。
  映射:中毒 50 → `resist_poison`;冰冻 52 → `resist_freeze`;眩晕 30 → `resist_sleep`;沉默 31 → `resist_forget`;混乱无映射。
- 战斗状态下发给客户端前清掉所有单位的 `combat`(不暴露对手装备概率;自己的看属性面板)。

### 4.6 入口(`player_bag_handler.cpp` 守护段、GM 三道闸)

- 三个新方法只做:参数粗检 → 调 `PlayerEquipSystem` → 失败直接写本次 `response->mutable_error_message()->set_id(...)` → 成功回全量(`PlayerBagSystem::BuildSnapshot`)。
- `GmGrantItem`:`scene_gm_guard::RejectGmClientRpc`、`gate_gm_client_messages.h` 登记、`client_gm_gate_test.cpp` 清单同步、
  `cpp/nodes/gate/SECURITY.md` §3 清单同步。**这是「prod 关掉」不是鉴权。**
- `BuildSnapshot`:每件物品调 `FillItemDisplay`;`bag_type = 2` 时调 `FillEquipSlots`。

## 5. 校验与不变量

1. 装备实例的随机属性只在「新铸」时掷一次;还原、穿脱、回流不重掷(`has_equip()` 守护)。
2. 上限、名称、基础属性不入库;改表立即对存量装备生效(显示夹取,不洗存档)。
3. 装备加成不落库:`bonus_values`、`BaseAttributesComp` 都不写装备数值。唯一例外是既有镜像 `BaseAttributesComp.speed`(出手序读它):`Recalculate` 每次用含装备的速度覆盖它,它随存档落库但从不作为计算输入,加载后第一次重算即被覆盖。
4. 穿脱不能当治疗(§4.4);战斗中、跨区冻结中拒绝穿脱。
5. 穿脱是「同一 guid 换包」:不写获得 / 销毁流水,不过发放封禁闸。
6. 新增战斗掷骰对「概率为 0」的单位零影响(确定性事件流不变)。
7. `equip_kind != 0 ⇒ max_stack_size == 1`。
8. 战斗指纹:Item 表在指纹内,新列有数据后指纹会变,scene 与 battle 必须同批换表;4 张新表只在 scene 读,不进指纹。
9. 导表后所有读配表的 Go 进程(login / scene_manager / player_locator / guild)与 robot 必须用同批生成物重编:
   Go 侧表加载是严格 JSON(不认识的字段即失败)且失败就退出进程 —— 旧 login 读到新 Item 表 = 全服进不去。

## 6. 客户端(Unity uGUI,`../mmorpg-client`)

- `PlayerFeaturesClient`:背包与装备栏分开缓存(原来同一时刻只存一个 `BagInfo`);新增 `RequestEquipment / EquipItem / UnequipItem`,
  回包整体覆盖两份快照;业务 tip 先看响应体 `error_message`。穿脱失败不作废在途读取;「状态未知」的失败
  (超时、缺快照、物品不存在、内部故障)自动重拉两份快照,规则类拒绝(等级 / 职业 / 背包满 / 战斗中 …)不重拉;
  穿脱在途期间的刷新请求收口后补发一次。
- 背包页(`GameplayWindow`):左侧装备栏(按 `equip_slots` 画出服务器下发的槽位,空槽显示部位名),右侧物品格;
  选中装备弹出 tooltip 卡片:名称、角色要求、基础属性、按颜色分档的属性行「名称 当前值/上限值」、物品说明、「装备 / 卸下」按钮。
  颜色:蓝 `#5AA7FF`、粉 `#FF7AD9`、黄 `#FFD84A`、绿 `#52D96A`(预留)、不满足要求红 `#FF5540`。
  tooltip 挂在窗口根而不是每次重建的 `_body` 上。服务器下发的字符串一律纯文本,一行一个 Text,不拼富文本。
- 属性面板:一级属性显示「值(+装备加成)」;新增「战斗属性」侧页签,按服务器下发的 `combat` 列表逐行显示(列表为空时页签不出现)。
- 战斗表现:本次不改(连击 / 反击 / 反震事件沿用现有 ATTACK / DAMAGE 演出;RESIST 老逻辑忽略)。
- 没有新 HUD 入口(右列已排满),全部并进现有「背包」「角色」窗。

## 7. 验证

以下是测试覆盖清单，用例条数为首次落码时的数。C++ 构建、C++ 单测与联机冒烟仍缺验证；
robot 装备定向测试已于 2026-10-13 通过，证据与边界见 §9，不能再统称全部未运行。

| 层 | 位置 | 内容 |
|---|---|---|
| 纯规则 | `cpp/tests/turn_battle_engine_test/equip_attribute_rules_test.cpp` | 33 条:掷值的随机数消费顺序、蓝不重复、粉黄概率、值域夹取、`ApplyEffect` 五种效果与越界、等级档;15 条 `static_assert` 守「`CombatStat` == proto 字段号」 |
| 战斗引擎 | `cpp/tests/turn_battle_engine_test/turn_battle_engine_test.cpp` | 新增 29 条 `Combat*`(`TurnBattleEngineTest.*` 共 89 条):必杀 / 抗性分类、连击、反击、反震、技能等级、抗异常、零概率不耗随机数、掷骰顺序对账、下发清洗 |
| 背包实例层 | `cpp/tests/bag_test/bag_instance_data_test.cpp` | 27 条:搬运原语、回调挂点、不重掷、存档往返(含 presence)、堆叠兜底、装备栏还原落位;另在 `player_feature_persistence_test.cpp`、`cross_zone_test.cpp` 各 1 条往返 |
| 装备编排 | `cpp/tests/bag_test/player_equip_test.cpp` | 47 条:表校验、掷属性、GM 发物、穿上 / 替换 / 卸下与各拒绝码、三条回滚、加成汇总、tooltip 投影、handler |
| 属性接入 | `cpp/tests/bag_test/player_equip_attribute_test.cpp` | 20 条:六项二级属性增量、战斗类 15 项、穿脱不当治疗、面板 bonus / combat、不落库、加点收益不随装备变、存档往返后重算 |
| GM 闸 | `cpp/tests/currency_test/client_gm_gate_test.cpp` | `GmGrantItem` 进清单并被拦 |
| 端到端 | `robot/robot.exe -c etc/equip_smoke.yaml`(账号 `robot_9103`) | 9 步:快照字段 → 槽位 → 等级不足 → 穿上 → 替换 → 穿鞋 → 重登 → 卸下回到原值 → `EQUIP_SMOKE_OK`;连跑两遍第二遍 `granted=0`。纯校验函数另有 14 个 Go 单测 `robot/equip_smoke_scenario_test.go` |
| 客户端 | 见 `../mmorpg-client/Docs/EquipmentUI-20261007.md` | EditMode 用例 + `GameplayUiVerification` 截图 |

## 8. 已知缺口

- 炼化 / 进化 / 套装 / 限制交易未做(§1.3);改造(强化)与镶嵌在二期做了,见 [equipment-enhance-inlay.md](equipment-enhance-inlay.md)。
- 现网 Buff / Skill 表没有任何异常状态,怪物只会普攻:抗异常与忽视抗异常的掷骰点已落码并有单测,但实战要等策划配出带异常状态的技能才有触发机会;抗混乱在引擎里没有消费点。
- 抗物理 / 抗法术与防御共用 60% 封顶:防御已堆满的角色这两条无效(D7)。
- 聚宝斋寄售、流水、回滚 diff 都不带属性:`DestroyedInstance` / `LogItemDestroy` 只有 guid / config / size;临时格先进先出挤掉带属性装备时无法从流水还原属性。
- 数值已按 2026-10-23 基线重定（D9），含普通装备与裸装参照；当前证据仍限独立公式镜像，随机极品装备、技能轮转与真实联机难度尚需验收。
- 客户端不加载配表:名称 / 上限全靠服务器下发,换表不用发客户端。
- GM 发物是「prod 关掉」不是鉴权。
- 旧顶层 `bag_data` 回退分支(`player_data_loader.h`,生成器模板产物)在属性重算之后才还原背包:走到这条路的旧快照,装备加成要到下一次重算才计入。当前代码存的档都带 `bag_component`,不会走到;未上线没有旧档,所以没有改模板。
- 抗物理 / 抗法术的面板值不反映 60% 共用封顶(防御堆满时实际生效值低于面板值);实时技能(非回合制)的暴击只读基础暴击率,不吃装备必杀率。
- 配表热更后在线玩家要到下一次重算(升级 / 加点 / 穿脱 / 重登)才生效;单表热更不重新跑表校验。
- Item 行被删掉的装备会卡在槽里(搬运原语对查不到表的 config 一律拒,fail-closed)。
- **重登 / 离线结算窗口内可以穿脱**:「战斗中禁穿脱」只认 `InBattleComp`,而重登后它的重建与挂起结算的应用要等 1–2 次 Redis 往返;窗口内脱装备 → 结算按当时的上限夹取气血 → 再穿回按比例放大,等于一次小额回血(违反 §5 不变量 4)。窗口很窄,且相关函数(`RestoreBattleFreezeOnLogin` / `ApplyPendingSettlement`)在 main 上刚被改过,所以留到带入 main 之后在 `PlayerBattleSystem::IsInBattle` 一处收口(加点、宝宝同时受益)。
- **入包边界整份信任实例上的 `equip` 段**:只要 `has_equip()` 就原样入库,消费侧只按上限夹单条数值,不校验条数、同档唯一、属性是否属于该装备的池。今天 `equip` 的来源只有服务器自己掷的和读盘,客户端碰不到;做邮件回流 / 寄售交付(P3)之前必须在玩法层补这道校验。
- 没配部位名的槽「不展示但仍可穿」:只有 GM 发夹具物品 1 / 2 才会占到,占着时以空名下发,客户端显示「栏位 N」。
- **Java 版（AGENTS §12）**：2026-10-13 装备整合进度仍登记待做，本次未核对 Java 仓的 `PARITY.md`，不提升完成状态。
  Java 仓现行位置以根 AGENTS.md §12 为准；旧路径不存在不构成 Java 未实现的证据。对齐时需核对本文 §2 / §3
  的客户端可见字段、tip 与配置表，并登记对应版本。

## 9. 已有证据与后续验证（AGENTS §10.1）

### 9.1 已完成：主干整合、正式生成与定向测试

依据 [PROGRESS](../PROGRESS.md) 的“2026-10-13 装备强化与镶嵌并入主干、正式生成与定向验证”条目：

- 一、二期已在主干整合；正式执行两次导表、一次全量 proto-gen，产物随 `0b40715d0` 等提交入库。
- 当次清单为 version 25 / 42 表，42 张源表及 84 份 JSON/PB 的尺寸、哈希和行数逐项一致；
  这是当次生成记录，后续加表应以新清单为准，不把历史版本号当永久断言。
- 一期消息号已入库：`UnequipItem=246`、`GmGrantItem=247`、`EquipItem=249`；二期为
  `InlayGem=250`、`EnhanceEquip=251`、`RemoveGem=252`，既有号未改。业务代码继续引用生成常量。
- 装备 tip 为 28000–28021；正式生成已同步装备表、协议、handler 元数据和限流规则。
- 当次在 `robot` 运行 `go test -mod=mod -run '^TestEquip' -count=1 -timeout=120s -json .`，
  32 个顶层用例、含子用例 253 项通过，0 失败。此证据是定向契约/逻辑测试，不是 C++ 或联机验收。

不再执行旧分支合并、首次占号或指向 `E:\work` 隔离工作树的生成步骤。后续确有协议/表源改动时，
先检查当前工作区与生成落点，按根规范和 `data/AGENTS.md` 重新生成，并保留 handler 守护段。
本次文档修订没有生成、编译或测试。

### 9.2 待验证：C++ 构建与单测

2026-10-13 装备批没有重编 C++ modules / scene / 节点，没有运行 C++ 单测。使用当前主仓与同批
生成物，MSBuild 一律 `/m:1 /p:Configuration=Debug /p:Platform=x64`，先核对既有依赖可用；缺工具
或依赖时按 AGENTS.md §10.2 报告，不自行安装或绕过门禁。

顺序为 table → proto → core → rpc → grpc_client → modules → battle 库 → scene 库 →
gate / scene / battle 节点 → 相关测试工程。**modules 与 scene 必须同批重编**，避免 ItemComp / Bag
布局不一致。工作区还有并行 RPC/ECS 改动，失败需按实际文件归因，不能统称装备问题。

测试目标与通过标准以二期 [§9.2](equipment-enhance-inlay.md#92-c-构建与单测) 为准，至少覆盖
`PlayerEquipTest.*`、`PlayerEquipAttributeTest.*`、`BagInstanceDataTest.*`、战斗 `Equip*`、
`ClientGmGateTest.*`、`CrossZoneBagMarshal.*` 及对应既有回归。先确认 `table_data_directory`
下七张装备表及依赖齐全；缺文件导致的 `LOG_FATAL` / abort 不能算测试通过。

### 9.3 Go 与 robot 验证边界

已执行的装备定向命令见 §9.1。当时采用的 `-mod=mod` 结果不能当作 vendor 验证。后续运行使用
当前模块声明的 Go 版本与 `GOTOOLCHAIN=local`，不得再固定已过时的 Go 1.26.5 安装路径，或仅为
测试重写 go.mod / vendor。受影响 Go 服务需用同批协议与表代码构建，新增验证逐项登记。

### 9.4 待验证：起栈与冒烟

在已授权的 dev 栈使用同一批产物重启受影响服务；scene 与 battle 同批换表，GM 入口只在允许的
开发模式打开。先执行既有登录基线，再在 `robot` 下运行 `etc/equip_smoke.yaml` 两遍，随后按二期
§9.4 运行强化镶嵌场景。装备一期要求两遍均输出 `EQUIP_SMOKE_OK`、退出码 0，第二遍不重复发物。
这不是压测；若要登录压测，另行遵守根 AGENTS.md §6 的清库与基线流程。

失败保留场景失败行及前文、同时间段 scene 的 `[PlayerEquip]` / ERROR、首个编译错误上下文、
失败用例完整输出，不注释断言、不跳过测试。不要为了重跑随意清除真实账号存档。

### 9.5 客户端与双版本

2026-10-13 进度记录了客户端代理同步官方 C# 协议、消息号、表代码及 handler；本次没有检查
客户端仓，不能据此宣称 Unity 测试、截图或当前运行通过。旧隔离检出的占位消息号与 CS0117
仅属于当时的离线测试背景。客户端后续验证在其仓库按规范执行，服务端本次不读写该仓。
Java 版仍以独立仓 `PARITY.md` 为准，不据旧路径或过期交接推断完成度。

## 10. 实现偏离记录

(实现与 §2–§6 不一致之处逐条记在这里。)

### 10.1 配置表地基(§2,2026-10-07)

列、类型、字段号、初始数据、tip 码与段号均与 §2 一致,**没有偏离**。以下是 §2 没写明、落地时补上的决定,
以及导表器能力带来的限制（本节记录 2026-10-07 的实现过程；正式生成已于 10-13 完成，C++ 验证边界见 §9）：

1. **`EquipAttribute.sort` 初值**:§2.1 没给,落地为 `sort = id × 10`(升序即 §2.1 清单顺序,留空档方便插队)。
   消费方只能按 `sort` 排序,不要假设它等于 id。
2. **`Item.base_attr` 的 `base_attr_id` 没有外键**:导表器的外键 / 索引只认主消息上的标量列,写在子消息列上会让
   生成的 `Load()` 去调主消息上不存在的访问器。所以导表期**不校验** `base_attr_id` 是否存在于 `EquipAttribute`;
   `Item.affix_pool`(指向 `EquipAffixPool.pool_id`,不是主键)同样没有外键。这两处填错只能靠
   `PlayerEquipSystem::InstallItemInitializer` 的启动表校验发现 —— 它除了 §2.5 的堆叠约束,还应逐行核对:
   `base_attr_id` 在 `EquipAttribute` 里、`affix_pool != 0` 时 `EquipAffixPool` 里有该池的行。
   有外键(导表期校验)的只有:`EquipAttributeCap.attr_id`、`EquipAffixPool.attr_id` → `EquipAttribute`,
   `Item.affix_rule` → `EquipAffixRule`(0 = 无引用)。
3. **`Item.xlsx` 的物理列序**:新列追加在最右,顺序是 `name … affix_pool, base_attr(6 列), affix_rule` ——
   `affix_rule` 排在 `base_attr` 之后,因为最右一列必须是标量(导表器用 `max_column` 推最后一个字段的列宽)。
   绑定按列名,字段号仍是 §2.5 的 7–14。`base_attr` 用不到的槽**留空**,填 0 会被导成一条 `id = 0` 的基础属性。
4. **既有行不回填**:`Item` 原有 28 行的新列全部留空(名称为空、不掷属性);`EquipSlot` 0–2 号槽的 `name` 留空(这三个是单测夹具槽,没名字的槽不对玩家展示,见 §10.7)。
5. **外键列自动带索引**:`Item.affix_rule`、`EquipAffixPool.attr_id` 因外键自动生成二级索引
   (`GetByAffixRule` / `GetByAttrId`),并新增产物 `item_table_fk.{h,cpp}`(已登记进 `CMakeLists.txt` / `table.vcxproj`)。
6. **tip 段**:28000 段核对为空闲且未被说明行预留,按 §2.7 原样落地;沙盒发号 `kEquipItemNotFound = 28000` …
   `kEquipInternalError = 28010`(枚举 `equip_error`,头文件 `table/proto/tip/equip_error_tip.pb.h`)。
7. **`MessageLimiter` 的生成顺序**：首次实现时需等 proto-gen 发号后按方法名写表；这一步已完成，
   当前装备与 GetBag / SortBag 规则应从源表和生成物核对，不再视为未执行。将来新增方法仍须先发号再写限流表。

### 10.2 背包实例层(§4.1,2026-10-07/08)

接口与 §4.1 一致;以下是落地时多出的约束与补强(27 条新用例 `cpp/tests/bag_test/bag_instance_data_test.cpp`,未编译):

1. **`SlotsAcceptingKind` 只对具名槽布局(装备栏)有结果**,自由格布局恒返回空,并对脏表做了去重。
2. **`PutInstance` 只收 `max_stack_size == 1` 且 `size == 1` 的实例**(与 `TakeInstance` 对称);自动找位不触发淘汰策略(临时格也不挤旧物)。
3. **回调若改了 `item_id / config_id / size / acquire_seq`,容器会钉回去并打 ERROR** —— 回调只许写 `equip`。
4. **按槽查占用者没有新接口**,复用 `bag.Layout().At(slot)`(空槽返回 `kInvalidGuid`)。
5. **装备栏还原优先落回快照槽位**:快照 `pos` 仍被槽位表认可且空着就原样落回,否则才取该部位第一个空槽
   (此前恒取第一个空槽,同部位两槽时重登会换位)。既有用例 `RestoreFollowsConfigNotTheSnapshotPos` 的语义不变。
6. **堆叠兜底不止 `CanStack` 一处**:`MeasureFreeRoomPerConfig` / `HasMergeablePartials` / `MergePartialStacks` 各自按 config 分组,
   也都补了 `has_equip()` 跳过。残留:`ReserveForBatchAdd(vector)` 的批量预检不知道入包那件带不带 equip,
   只在「装备被误配成可堆叠」时偏乐观 —— 不变量 7 的启动校验是第一道闸。
7. **回调槽位是进程级静态变量,无锁**:只支持「启动期装一次、此后只读」;测试里装过的必须在 TearDown 卸掉。
8. `TakeInstance` 对表里已删的 config 返回 `kInvalidTableId`(这种装备脱不下来,fail-closed)。

### 10.3 战斗引擎(§4.5,2026-10-07/08)

29 条新用例(`turn_battle_engine_test.cpp`,`TurnBattleEngineTest.*` 共 89 条;未编译):

1. **连击段独立结算**:D3 已按实现改写 —— 追加段走完整普攻公式(可独立必杀)再 ×50%,向上取整、至少 1、不超过目标当前气血。
2. **必杀满 100 仍掷骰**(保持落地前满暴击单位的随机数消耗);连击 / 反震 / 反击 / 抗异常走 `RollPercent`:0 与 ≥ 100 两端都不耗随机数。
3. **反震要求受击者挨完这一段仍存活**(D5 已改写);反震伤害不吃减伤、不必杀,出手者被弹死则终止本次行动(不再连击、不判反击),击杀归属给受击者。
4. **反击另起事件组**(`group_id + 1`,`hit_index` 归 0);一回合的组数因此可以多于出手数。连击段与反震留在原行动的组里。
5. **一次普攻的随机数消费顺序**:首段必杀 → 首段反震 → 连击续段判定 →(连击段必杀 → 连击段反震)→ 反击判定 →(反击那一下的必杀)。
   由 `CombatFractionalRollsFollowDocumentedRandomOrder` 逐回合对账钉住。
6. **清洗函数在引擎库**:`TurnBattleEngine::StripEngineOnlyState`(public static),battle 节点的 `RedactStateForViewer` 调它;
   开战包 / 回合结果(参战者、观众)/ 补拉 / 观战首帧 5 个出口都经过。节点侧接线没有单测覆盖(只做了走查)。
7. `InitPlayers` 只在快照 `has_combat()` 时建子消息;未知伤害类型两组加成都不吃;`target_sub_buff` 回挂到施法者身上的异常按「施加者 = 原目标」判抵抗。
8. **客户端分拍需确认**:反击组的出手者不在 `action_order` 的对应位置;反震是出手者那组里 `source = 受击者` 的 DAMAGE;连击是同组同目标两条 DAMAGE。

### 10.4 纯规则与装备编排(§4.2 / §4.3 / §4.6,2026-10-08)

1. **`rand` 必须能无损接收 `uint64_t`**(权重和按 64 位累加);返回值 >= n 时夹成 n − 1。生产随机源是 `tlsRandom.Rand<uint64_t>(0, n - 1)`。
2. **掷骰消耗口径**:走到的每个步骤恰好消耗一次(条数区间退化、取值单点、rate >= 10000 都照掷);只有 rate == 0 与候选为空不消耗。池内重复 `attr_id` 合并权重。
3. **`ApplyEffect`**:effect 2 / 4 忽略 param;effect 1 只拒 param == 0(维度是否存在、是否角色池由表校验把关);value == 0 返回 true 且不建键。
4. **`DerivedStat` 与 proto 字段号不同序**(`kSpeed = 5, kDefense = 6`,而 `DerivedAttributesComp` 是 `defense = 5, speed = 6`):消费方按名字取,不许按号对拷。
5. **`PlayerEquipSystem` 多出的公开成员**:`ValidateTables()`(返回问题条数;一行里互不相干的问题各计一条,同一根因只计一次)、`IsEquipment(configId)`、`kGmGrantMaxCount = 99`。
6. **前置检查返回码**:实体无效 `kEntityIsNull`,冻结 `kEquipFrozen`,战斗中 `kEquipInBattle`,没有背包组件 `kEquipInternalError`。
7. **回滚**:失败按相反顺序放回;放回也失败时打带 `[PlayerEquip][INSTANCE_LOST]` 标签的 ERROR(含 guid / config),不用 `LOG_FATAL`(那会中止进程);回滚后装备栏内容与调用前不同就重算并推面板。
8. **加成与 tooltip 同口径**:Item 行 `equip_kind == 0` 的穿着物整件不计;规则层不接受 effect 的属性行既不计入也不下发;无上限档的随机属性以 `value = 0, cap = 0` 下发。`CollectBonus` 不复核佩戴要求(穿上之后降级 / 改表不脱装备)。
9. **`FillEquipSlots` 只下发槽号小于装备栏容量的槽**(没配名字的槽还要「被占用」才下发,见 §10.7);`DescribeCombatStat` 多行命中取 sort 最小,未命中返回空名。
10. **RPC 的 method index 是 2 / 3 / 4**(`GetBag` 0、`SortBag` 1 之后);handler 对 `item_id == 0` 回 `kInvalidParameter`。`player_bag_handler.h` 的手工补丁以 Codex 重生成的为准。
11. **启动安装点**:`cpp/nodes/scene/main.cpp`,表加载之后。`bag_test.vcxproj` 为新用例加了两条 OpenSSL include 目录。
12. `cpp/nodes/gate/SECURITY.md` 里 `GmGrantItem` 的 message_id 是占位,proto-gen 发号后回填。

### 10.5 属性计算接入(§4.4 / §3.2,2026-10-08)

1. **面板一级点的装备部分在 `BuildPanel` 里现算**(再调一次 `CollectBonus`,只读、开窗才调),没有做运行时缓存组件;六项与 combat 读上一次 `Recalculate` 的结果。
2. **combat 表完整时恒 15 项**;某项在 `EquipAttribute` 表里没有显示行就不下发(由表校验报错,见 §10.7)。百分比项面板显示夹到 100,`DerivedAttributesComp.combat` 存未夹取的原值。
3. `derived.mutable_combat()` 每次重算都置位:无装备玩家 `has_combat()` 为真、内容全 0。
4. `RecalcReason` 追加 `kEquipmentChanged`,没有给这个既有枚举加 `kCount`。
5. 累加器出口用 `FloorToU64`(>= 2^64 夹到上限);`bonus` 饱和到 uint32。

### 10.6 robot 冒烟(§7,2026-10-08)

1. 改了任务名单外的 `robot/config/config.go`(mode 白名单加 `equip-smoke`,不加 `config.Load` 直接报 unknown mode)。
2. 冒烟自带 700ms 同号节流;gate 级拒绝(限流 / GM 闸)表现为「全零响应」判红,真实码看紧挨着的 `gate envelope error` 日志。
3. `EquipItem(0)` 认 `kInvalidParameter` 或 `kEquipItemNotFound` 其一,其余判红。
4. 冒烟镜像了少量表值(config 1101 / 1105 / 1401、槽 3–6、基础值 40 / 12 / 12 / 1620、规则行 1 的蓝 1..3、战斗属性 15 行):策划改这些要同步改 `equip_smoke_scenario.go` 顶部常量。
5. 预备段「已有就复用」:服务端有 bug 的那一轮铸出的坏实例不会重掷,修好后要换账号或清 `robot_9103` 的背包存档再跑(失败信息里有 `[reused=…]` 提示)。

### 10.7 整体审查收口(2026-10-08)

跨模块整体审查(全链路贯通 / 既有回归 / 生成器兼容 / 资产与安全 四个视角,11 条发现)之后的统一修复,经独立复核「真实落地、未引入新的编译问题」:

1. **表校验新增「战斗属性 1..15 各有显示行」**:判据就是 `DescribeCombatStat(stat).name.empty()`,与属性面板跳过该行用的是同一个函数。
   计数口径:`effect = 5` 但 `effect_param` 越界的行已按「规则层不接受」报过,它造成的那一项缺行不再另计(被抵掉的项不打日志;修好被拒的行再跑,剩下的会原样报出)。
   `EquipAttribute` 表没加载时 `ValidateTables` 返回 15 而不是 0。
2. **没配部位名的槽不对玩家展示**:`FillEquipSlots` 只下发「容量内、且(有名字或正被占用)」的槽。原因是 `EquipSlot` 0–2 号槽是单测夹具(部位 1 / 2,无名字),
   全量下发的话客户端会多画 3 个没名字的空槽。前置:调用方先填好 `layout.capacity` 与 `layout.slots`(`BuildSnapshot` 是唯一调用点)。
3. **角色池维度判定只留一份**:新增公开静态函数 `PlayerAttributeSystem::IsPlayerDimension(uint32_t dimensionId)`;`player_attribute.cpp` 匿名命名空间里
   按行判定的那个改名 `IsPlayerDimensionRow`(同名会被成员函数的非限定查找先命中);`player_equip.cpp` 的副本与常量已删。
4. **`InstallItemInitializer` 不带「已安装」标志**:每次调用都重装回调并跑表校验;节点侧唯一调用点是 `cpp/nodes/scene/main.cpp` 的表加载完成回调。
5. `container_layout.h` 里 `PlaceAt` 的注释与 `docs/design/bag-instance-layout-split.md` 的还原落位规则已按 §10.2 同步。
6. 两条有意延后的安全项(重登窗口穿脱、入包边界信任 `equip` 段)记在 §8;生成流程上的三个陷阱(发号错位、客户端落点、`match_internal` 未登记)与 Go 进程同批重编写进了 §9。
7. 用例数更新:`PlayerEquipTest` 47、`PlayerEquipAttributeTest` 20。

### 10.8 客户端(§6,2026-10-08)

实现与验证细节在客户端仓 `Docs/EquipmentUI-20261007.md`。与 §6 的差异:

1. **首次实现时的消息号依赖**：2026-10-08 离线验证使用临时占位号，曾因常量缺失出现 CS0117。`MessageIds` 与正式协议后来已生成并入库；该历史现象不能当作当前允许的编译错误，当前客户端状态见 §9.5。
2. **卡片多了一行物品说明**(§6 原清单没有):装备选中后原详情栏不再显示,说明搬到卡片的属性行与按钮之间。
3. **失败后的同步策略**(见 §6 正文):不是「失败就补拉」,而是「失败不作废在途读取 + 只有状态未知的失败才重拉」,避免连点时顶到 `GetBag` 限流。
4. **传输层 / gate 级错误显示成中文文案**(「装备失败:网络异常,请稍后重试」等),原始英文串不上屏。
5. 新增界面辅助类 `EquipBagModel`(槽位视图、选中项跟随、要求文案)与 `GameplayWindow.SetBagPage`(两份快照一次重画)。
6. **协议 C# 的顺带差异**:`BattleData.cs` 重生成时带进了服务端早已存在、客户端一直没重生成的 `BattleActivityContext`(帮会历练),纯新增类型。
7. 客户端 origin/main 基线自带三处与本功能无关的编译错误(`QdaoPetCompanion.cs` 5 个、`PetAppearanceIntegrationTests.cs` 4 个、
   `QdaoArchivedActionsPlayModeTests.cs` 6 个),不修的话 Unity 里整个 Battle 测试程序集编不过;本功能没有动它们。

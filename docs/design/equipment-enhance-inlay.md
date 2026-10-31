# 装备强化与镶嵌(装备属性系统二期)

> 落地日期：2026-10-09 / 10；2026-10-13 已整合主干、正式导表与全量 proto-gen，C++ 编译、C++ / Go 单测、起栈冒烟（一期 + 二期两遍）均有当日运行证据，见 §11。
> 2026-10-15 的文档更新与本次合并保留这些历史证据；它们不代表当前合并后的 RPC / ECS 等改动已复验。后续验证见 §9，首次实现阶段的记录见 §10.8。
> 前置:[equipment-attributes.md](equipment-attributes.md)(一期:随机属性 + 穿脱 + 战斗类属性)。本文只写二期新增的部分,
> 一期的表、协议、函数沿用不重复;两份文档冲突时,二期范围内以本文为准。
> 服务器实现位于当前主干；旧隔离工作树 `E:\work\xuanming-server-mmo-equip` 的 `feat/equip-enhance-inlay` 停在历史改写前，已作废，不要再合：
> 配表 `data/EquipEnhance.xlsx` / `data/EquipGem.xlsx` / `data/EquipGemSocket.xlsx` + `data/Item.xlsx` 新行 + `data/tip/Tip.xlsx` 新码;
> 协议 `proto/common/component/item_base_comp.proto`、`proto/scene/player_bag.proto`、`proto/common/rollback/transaction_log.proto`;
> 纯规则 `cpp/libs/services/scene/player/system/equip_attribute_rules.h`;编排 `cpp/libs/services/scene/player/system/player_equip.{h,cpp}`;
> 容器编排 `cpp/libs/modules/bag/bag_service.{h,cpp}`;入口 `cpp/nodes/scene/handler/rpc/player/player_bag_handler.{h,cpp}`;
> 冒烟 `robot/equip_forge_smoke_scenario.go`。
> 客户端实现涉及 `Game/PlayerFeatures` 与 `UI/Ugui/Gameplay/EquipBagModel.cs`。当日进度记录了正式生成同步，
> 本次没有读取独立客户端或验证其当前运行结果；旧隔离检出与预期编译错误仅作历史背景，见 §6.5、§9.5。
>
> 本文 §2–§6 是**实现契约**:字段号、表列、函数签名以此为准;实现与本文不一致时改其中一个并在 §10 记录。
> 契约经过一轮三视角对抗评审(服务器可行性 / 资产一致性 / 协议与客户端),采纳的修订已并入正文。

## 1. 目标与非目标

### 1.1 需求(2026-10-09)

一期落地后,装备只能「掷属性、穿、脱」。用户要求把装备系统的逻辑代码做完:**强化**(装备 +N,放大基础属性)与
**镶嵌**(宝石嵌进装备孔位提供属性,可取下)。

### 1.2 本次做

1. 强化:消耗金币与材料,按成功率把装备强化等级 +1;等级按万分比放大这件装备的**基础属性**(不放大随机属性与宝石)。
2. 镶嵌 / 取下:宝石是一种可叠加物品;嵌入时消耗 1 颗,记在装备实例上;取下时退回 1 颗。
3. 强化等级与已镶宝石随装备实例走:存档、跨区、穿脱都不丢。
4. 强化与宝石的加成进角色面板与回合制战斗(走一期同一条链:`CollectBonus → Recalculate → 战斗快照`)。
5. 规则全部数据化(3 张新表);客户端零配表,显示所需的一切由服务器下发。
6. 客户端数据层(请求 / 回包 / 快照 / 纯逻辑模型)与用例;robot 冒烟;C++ 单测。

### 1.3 本次不做

| 不做 | 说明 |
|---|---|
| 洗练 / 炼化(重掷蓝 / 粉 / 黄) | 仍按一期 §1.3;`equiprules::RollAffixes` 可直接复用 |
| 宝石合成 / 升级、打孔、孔位解锁 | 孔位由 `EquipGemSocket` 表按部位给定;要按装备档次区分时给该表加列 |
| 强化保护符、祝福值、强化转移 | 失败惩罚只有「掉级」一个旋钮(表列,默认 0) |
| 界面(美术 UI) | 客户端 UI 文件由 Codex 在做;本次只交数据层与纯逻辑模型,入口接线见客户端 `Docs/EquipmentForge-20261009.md` |
| 流水带实例数据 | 一期 §8 已记:流水只有 guid / config / size;带强化 / 宝石的装备被销毁后无法从流水还原 |

### 1.4 用户未明示、按默认值落地的决定(可随时改)

| # | 决定 | 默认值 | 改它要动哪里 |
|---|---|---|---|
| E1 | 强化放大什么 | 只放大这件装备的**基础属性**(武器伤害、防具防御 / 速度);随机属性与宝石不放大 | `player_equip.cpp` 的 `ResolveBaseNow` |
| E2 | 强化上限 | +12(= `EquipEnhance` 表行数) | 只改表 |
| E3 | 强化失败的后果 | 金币与材料照扣,**不掉级**(表列 `fail_level_loss` 全填 0) | 只改表 |
| E4 | 强化消耗 | 金币 + 一种材料(强化石);成败都扣 | 只改表 |
| E5 | 强化 / 镶嵌的对象在哪 | 人物背包里的或身上穿着的都行;仓库 / 临时格里的不行 | `player_equip.cpp` 的 `LocateEquipment` |
| E6 | 孔位怎么来 | 按**部位**给定(武器 2 孔、衣服 2 孔、帽子 / 鞋各 1 孔),每个孔限定宝石类型(0 = 任意) | 只改表 |
| E7 | 孔里已有宝石再嵌 | 拒绝,要先取下(不做「顶替并退回」) | `player_equip.cpp` 的 `InlayGem` |
| E8 | 取下宝石 | 免费,原样退回 1 颗到人物背包;背包放不下就拒绝 | `player_equip.cpp` 的 `RemoveGem` |
| E9 | 同一件装备能否嵌多颗同类宝石 | 能(只受孔位类型限制) | — |
| E10 | 全部数值 | 见 §2,是**占位建议**,未经实测 | 只改表 |
| E11 | 数据落点 | 强化等级与宝石放进 `EquipInstanceData`(随 `ItemComp.equip` / `ItemEntry.equip` 整份搬运),**不启用** `ItemEntry` 预留的 6 号 `enchant_level` / 8 号 `gem_uuids` | 见 §3.1 |
| E12 | 宝石怎么记 | 记宝石的 `config_id`,不记 guid(宝石可叠加,guid 不是稳定身份) | 见 §3.1 |

## 2. 配置表

导表流程见 `data/AGENTS.md`。xlsx 由 `tools/scripts/equip_xlsx_patch.py` 的新子命令生成 / 打补丁(纪律同一期:
多会话共写的 `Item.xlsx` / `Tip.xlsx` 不整表写回,保存后读回核对)。

**不给 `Item` 表加列**:Item 表在战斗配表指纹内,且被所有 Go 进程严格加载;「是不是宝石」「有几个孔」「强化规则」都放在
只有 scene 读的新表里。本次对 Item 表只**加行**(强化石与宝石物品)。

### 2.1 `EquipEnhance`(新表)—— 一行一个强化等级

schema `data/schema/equipenhance_table.proto`(`message EquipEnhanceTable`,sheet `EquipEnhance`,主键 `id`),全标量:

| 号 | 列 | 类型 | 含义 |
|---:|---|---|---|
| 1 | `id` | uint32 | **目标**强化等级(主键,从 1 起连续)。第 N 行 = 「从 +(N−1) 强化到 +N」的规则,以及 +N 的加成 |
| 2 | `bonus_permyriad` | uint32 | 强化到本级后基础属性的**总**加成(万分比,不是相对上一级的增量);随等级单调不减 |
| 3 | `success_rate` | uint32 | 成功率(万分比,1..10000) |
| 4 | `fail_level_loss` | uint32 | 失败时掉几级(0 = 不掉;最多掉到 0) |
| 5 | `cost_gold` | uint64 | 金币消耗(成败都扣);0 = 不收 |
| 6 | `material_id` | uint32 | 材料物品,`[(cfg_fk) = "Item"]`;0 = 不要材料 |
| 7 | `material_count` | uint32 | 材料数量(成败都扣);`material_id == 0` 时必须为 0 |

强化上限 `MaxEnhanceLevel()` = 表里从 1 起**连续**的最大 id(表为空 = 0 = 强化未开放)。

初始 12 行(材料统一 2001 强化石):

| id | bonus_permyriad | success_rate | fail_level_loss | cost_gold | material_count |
|---:|---:|---:|---:|---:|---:|
| 1 | 500 | 10000 | 0 | 1000 | 1 |
| 2 | 1000 | 10000 | 0 | 2000 | 1 |
| 3 | 1500 | 10000 | 0 | 3000 | 2 |
| 4 | 2000 | 9000 | 0 | 5000 | 2 |
| 5 | 2600 | 8000 | 0 | 8000 | 3 |
| 6 | 3200 | 7000 | 0 | 12000 | 3 |
| 7 | 3800 | 6000 | 0 | 18000 | 4 |
| 8 | 4500 | 5000 | 0 | 26000 | 4 |
| 9 | 5200 | 4000 | 0 | 36000 | 5 |
| 10 | 6000 | 3000 | 0 | 50000 | 6 |
| 11 | 7000 | 2500 | 0 | 70000 | 8 |
| 12 | 8000 | 2000 | 0 | 100000 | 10 |

### 2.2 `EquipGem`(新表)—— 一行一种宝石

schema `data/schema/equipgem_table.proto`(`message EquipGemTable`,sheet `EquipGem`,主键 `id`),全标量:

| 号 | 列 | 类型 | 含义 |
|---:|---|---|---|
| 1 | `id` | uint32 | 宝石物品的 `Item.id`(主键,`[(cfg_fk) = "Item"]`)。**「是不是宝石」= 本表有这一行,且对应 Item 行存在、不是装备**(§4.3 `IsGem`) |
| 2 | `gem_type` | uint32 | 宝石类型(≥ 1);孔位按类型接纳 |
| 3 | `attr_id` | uint32 | 提供的属性,`[(cfg_fk) = "EquipAttribute"]`;效果走一期的 `equiprules::ApplyEffect` |
| 4 | `value` | uint32 | 属性值(> 0);百分比属性为整数百分点 |

初始 12 行(类型 1 攻击 / 2 防御 / 3 气血 / 4 迅捷;属性 id 见一期 §2.1):

| id | 名称(Item.name) | gem_type | attr_id | value |
|---:|---|---:|---:|---:|
| 2101 / 2102 / 2103 | 攻击宝石·一级 / 二级 / 三级 | 1 | 1(伤害) | 30 / 60 / 100 |
| 2201 / 2202 / 2203 | 防御宝石·一级 / 二级 / 三级 | 2 | 8(防御) | 12 / 24 / 40 |
| 2301 / 2302 / 2303 | 气血宝石·一级 / 二级 / 三级 | 3 | 9(气血) | 60 / 120 / 200 |
| 2401 / 2402 / 2403 | 迅捷宝石·一级 / 二级 / 三级 | 4 | 11(速度) | 12 / 24 / 40 |

### 2.3 `EquipGemSocket`(新表)—— 一行一个孔位

schema `data/schema/equipgemsocket_table.proto`(`message EquipGemSocketTable`,sheet `EquipGemSocket`,主键 `id`),全标量:

| 号 | 列 | 类型 | 含义 |
|---:|---|---|---|
| 1 | `id` | uint32 | 行号(主键,`equip_kind * 100 + socket`) |
| 2 | `equip_kind` | uint32 | 部位,`[(cfg_index) = true]`;同一部位的全部行 = 该部位装备的孔位 |
| 3 | `socket` | uint32 | 孔位号(0 起,同部位内连续、不重复,< `equiprules::kMaxGemSockets` = 8) |
| 4 | `gem_type` | uint32 | 该孔接纳的宝石类型;0 = 任意类型 |
| 5 | `name` | string | 孔位显示名(下发给客户端;允许留空,客户端空名时显示「孔位 N」) |

孔位是数据(AGENTS §11.6「槽位由配置表枚举」):加孔只加行,不在 C++ 里写 `if 部位 == 武器`。

初始 6 行:

| id | equip_kind | socket | gem_type | name |
|---:|---:|---:|---:|---|
| 1100 | 11(武器) | 0 | 1(攻击) | 攻击孔 |
| 1101 | 11 | 1 | 0(任意) | 通用孔 |
| 1200 | 12(帽子) | 0 | 3(气血) | 气血孔 |
| 1300 | 13(衣服) | 0 | 2(防御) | 防御孔 |
| 1301 | 13 | 1 | 3(气血) | 气血孔 |
| 1400 | 14(鞋子) | 0 | 4(迅捷) | 迅捷孔 |

### 2.4 `Item` 表新行(不加列)

物品 id 段:**2000–2999 = 装备养成材料**(2001 强化石;21xx–24xx 宝石,百位 = 宝石类型,个位 = 等级)。
13 行,全部 `max_stack_size = 999`、`equip_kind = 0`、`battle_usable = 0`,只填 `id / max_stack_size / name / description`,
其余列留空(与一期「留空不填 0」的纪律一致;`equip_kind` / `battle_usable` 两列按既有 15–28 号行的填法):

| id | name | description |
|---:|---|---|
| 2001 | 强化石 | 强化装备时消耗的材料。 |
| 2101–2103 | 攻击宝石·一级 / 二级 / 三级 | 可镶嵌在装备的攻击孔或通用孔,提升伤害。 |
| 2201–2203 | 防御宝石·一级 / 二级 / 三级 | 可镶嵌在装备的防御孔或通用孔,提升防御。 |
| 2301–2303 | 气血宝石·一级 / 二级 / 三级 | 可镶嵌在装备的气血孔或通用孔,提升气血。 |
| 2401–2403 | 迅捷宝石·一级 / 二级 / 三级 | 可镶嵌在装备的迅捷孔或通用孔,提升速度。 |

### 2.5 tip 码(追加在 `//equip_error` 组内,紧跟一期 11 个码之后,顺序如下)

| 码名(表里不带 `k`) | 文案 | fault |
|---|---|---|
| `EquipEnhanceUnavailable` | 该装备无法强化 | |
| `EquipEnhanceMaxLevel` | 已强化到最高等级 | |
| `EquipEnhanceMaterialNotEnough` | 强化材料不足 | |
| `EquipEnhanceGoldNotEnough` | 金币不足,无法强化 | |
| `EquipGemNotGem` | 该物品不是宝石 | |
| `EquipGemNotOwned` | 背包里没有这颗宝石 | |
| `EquipGemSocketInvalid` | 该装备没有这个宝石孔 | |
| `EquipGemSocketOccupied` | 该孔位已镶有宝石,请先取下 | |
| `EquipGemSocketEmpty` | 该孔位没有宝石 | |
| `EquipGemTypeMismatch` | 宝石类型与孔位不符 | |
| `EquipGemBagFull` | 背包已满,无法取下宝石 | |

号由导表器发(发号规则是「已发号按名保留,新名字按表内出现顺序取本段最小空位」,所以**行序就是号序**,预期 28011–28021;
**任何手写文件里都不许出现这些数字**)。一期的 `EquipItemNotFound` / `EquipNotEquipment` / `EquipInBattle` / `EquipFrozen` /
`EquipInternalError` 原样复用。

补丁子命令(建议名 `forge-tip-codes`)的两条路径,幂等判据都是「`equip_error` 组内,这 11 个码紧跟在一期 11 个码之后,
且 (码名, 文案, fault) 逐项一致」:

- (a) `//equip_error` 组最后一个码行之后到表尾全空 → 只写空格(`apply_patches_and_verify`);
- (b) 其后还有别的组(例如别的分支先进 main 的 `//mystery_merchant_error`)→ 在组尾下一行 `ws.insert_rows(at, amount=11)` 再写,
  保存后读回,逐组核对**其它所有组**的 (码名, 文案, fault) 序列不变(照 `tools/scripts/guild_b5a_xlsx_patch.py` 的做法)。

### 2.6 工程登记(手工;导表器不写工程文件)

- `cpp/generated/table/CMakeLists.txt`、`table.vcxproj`、`table.vcxproj.filters` 三处,照一期 `equipaffixpool` / `equipattributecap` 的登记写法各加:
  - `code/equipenhance_table.{h,cpp}`、`code/equipenhance_table_comp.h`、`code/equipenhance_table_fk.{h,cpp}`、`proto/equipenhance_table.pb.{h,cc}`;
  - `code/equipgem_table.{h,cpp}`、`_comp.h`、`_fk.{h,cpp}`、`proto/equipgem_table.pb.{h,cc}`(有外键:`id → Item`、`attr_id → EquipAttribute`);
  - `code/equipgemsocket_table.{h,cpp}`、`_comp.h`、`proto/equipgemsocket_table.pb.{h,cc}`(**没有 `_fk`**:只有索引)。
- `cpp/tests/bag_test/bag_test.vcxproj` 与 `.filters` 加 `bag_remove_exact_test.cpp`(`cpp/tests` 下没有 CMakeLists,只走 vcxproj)。
- `tools/scripts/gen_docs_index.py` 的分类规则认得本文档(`equipment-*`),并重生 `docs/README.md`。

## 3. 协议

### 3.1 实例层(`proto/common/component/item_base_comp.proto`)

```proto
// 一颗已镶嵌的宝石。记 config 不记 guid:宝石是可叠加物品,guid 不是稳定身份。
message EquipGem
{
	uint32 socket = 1;         // 孔位号(0 起)。显式字段,repeated 下标不承载语义(AGENTS §11.6)
	uint32 gem_config_id = 2;  // 宝石的 Item 表 id(= EquipGem 表主键)
}

message EquipInstanceData
{
	repeated EquipAffix affixes = 1;
	uint32 enhance_level = 2;    // 强化等级(+N),0 = 未强化
	repeated EquipGem gems = 3;  // 已镶嵌宝石;同一 socket 至多一条
}
```

`ItemComp.equip`(5)与 `ItemEntry.equip`(14)共用这个消息,`bag_marshal` 是带 presence 的整份 `CopyFrom`,
所以**容器层与存档层零改动**。`proto/common/database/bag_quest_mail_data.proto` 里 `ItemEntry` 的 TODO 表:
6 号 `enchant_level`、8 号 `gem_uuids` 改注为「不再启用,已由字段 14 `equip` 承载,别复用」(同 7 号的写法);
`item_store.h`、`bag_marshal.{h,cpp}` 里说「强化等级 / 镶嵌仍是预留」的注释同步改。

`presence` 语义不变:`has_equip() == true` 表示实例已完成初始化。**强化 / 镶嵌只接受 `has_equip()` 的实例**,
绝不对未初始化的实例调 `mutable_equip()`;对它们的拒绝形态见 §4.3(不是故障码)。

### 3.2 流水类型(`proto/common/rollback/transaction_log.proto`,枚举末尾追加)

```proto
TX_EQUIP_ENHANCE = 27;     // 装备强化:扣金币 + 销毁材料(成败都扣)。correlation_id = 装备 item guid
TX_EQUIP_INLAY = 28;       // 宝石镶嵌:销毁 1 颗宝石物品。correlation_id = 装备 item guid
TX_EQUIP_GEM_REMOVE = 29;  // 取下宝石:发回 1 颗宝石物品。correlation_id = 装备 item guid
```

物品流水的 `extra`(紧凑 JSON,手拼字符串即可):强化扣材料 `{"from":<强化前等级>,"success":true|false}`;
镶嵌 `{"socket":<N>}`;取下 `{"socket":<N>}`。货币流水没有 `extra` 形参,只带 `txType` 与 `correlationId`。

### 3.3 客户端协议(`proto/scene/player_bag.proto`)

```proto
EquipAttrLineInfo 追加:
  uint64 enhance_bonus = 8;       // 仅基础属性行:强化带来的增量,**已含在 value 内**
  uint64 enhance_next_value = 9;  // 仅基础属性行、且该装备带 enhance_next 时:强化到下一级后这一行的 value(预览)

// 装备上的一个宝石孔。
message EquipGemSocketInfo {
  uint32 socket = 1;           // 孔位号
  uint32 gem_type = 2;         // 该孔接纳的宝石类型,0 = 任意
  uint32 gem_config_id = 3;    // 占着这个孔的宝石 config;0 = 空孔。**「是否占用」只看它**
  string gem_name = 4;         // 宝石名(空孔为空;宝石的 Item 行查不到时也为空)
  string gem_icon_key = 5;
  EquipAttrLineInfo attr = 6;  // 宝石**当前生效**的属性。gem_config_id != 0 却不带 attr = 已镶但当前不生效(仍可取下)。tier 恒 0、cap 恒 0、seq 恒 0
  string name = 7;             // 孔位显示名(可为空)
}

// 下一次强化的预览(客户端零配表)。
message EquipEnhancePreview {
  uint32 next_level = 1;
  uint32 success_rate = 2;        // 万分比
  uint64 cost_gold = 3;
  uint32 material_config_id = 4;  // 0 = 不要材料
  string material_name = 5;
  uint32 material_count = 6;
  uint32 bonus_permyriad = 7;     // 升到 next_level 后基础属性的总加成(万分比)
  uint32 fail_level_loss = 8;
  string material_icon_key = 9;
}

BagItemInfo 追加:
  uint32 enhance_level = 14;                     // 装备:当前强化等级(已按表上限夹取)
  uint32 enhance_max_level = 15;                 // 装备:强化上限;0 = 强化未开放
  EquipEnhancePreview enhance_next = 16;         // 装备:下一级预览;已满级 / 未开放 / 实例未初始化 / 下一级的行不可用时不带
  repeated EquipGemSocketInfo gem_sockets = 17;  // 装备:该部位的全部孔位(含空孔),按 socket 升序;实例未初始化时不带
  uint32 gem_type = 18;                          // 宝石物品:宝石类型(≥ 1);不是宝石 = 0
  EquipAttrLineInfo gem_attr = 19;               // 宝石物品:这颗宝石提供的属性(不是宝石、或属性行不可用时不带)。tier / cap / seq 恒 0
  uint32 enhance_bonus_permyriad = 20;           // 装备:当前有效等级的基础属性总加成(万分比);0 级为 0,满级也写

message EnhanceEquipRequest { uint64 item_id = 1; }
message EnhanceEquipResponse {
  TipInfoMessage error_message = 1;
  BagInfo bag = 2;           // 人物背包全量(金币、材料变化)
  BagInfo equipment = 3;     // 装备栏全量
  bool success = 4;          // 本次强化是否成功。**失败不是错误**:error_message 为空、两份快照照常带回
  uint32 enhance_level = 5;  // 操作后的强化等级
}
message InlayGemRequest { uint64 item_id = 1; uint32 socket = 2; uint32 gem_config_id = 3; }
message InlayGemResponse { TipInfoMessage error_message = 1; BagInfo bag = 2; BagInfo equipment = 3; }
message RemoveGemRequest { uint64 item_id = 1; uint32 socket = 2; }
message RemoveGemResponse { TipInfoMessage error_message = 1; BagInfo bag = 2; BagInfo equipment = 3; }

service SceneBagClientPlayer 末尾追加(顺序不可变,CallMethod 按 method index 分发;index 5 / 6 / 7):
  rpc EnhanceEquip (EnhanceEquipRequest) returns (EnhanceEquipResponse);
  rpc InlayGem (InlayGemRequest) returns (InlayGemResponse);
  rpc RemoveGem (RemoveGemRequest) returns (RemoveGemResponse);
```

- 三个响应的 `bag = 2`、`equipment = 3` 与 `EquipItemResponse` 同号同名,handler 复用 `ReplyInventoryAndEquipment` 模板。
- 强化的概率失败**不走 tip**:客户端有 tip 就保留旧快照,而失败时材料已扣,必须带回新快照。
- 镶嵌按宝石的 `config_id` 消耗 1 颗(不指定哪一堆):背包层没有「从指定那一堆扣 k 个并写流水」的入口,也不需要。
- 方法名不以 `Gm` 开头(scene 按前缀拦 GM 指令);请求体无 repeated、无有符号整型(scene 的请求体检查);响应类型名不含 `Empty`。
- **消息号由 proto-gen 发。robot / 客户端 / 测试 / 限流补丁等任何手写文件里都不许出现这三个方法的数字消息号,只引用生成常量**(见 §9)。

## 4. 服务端实现契约

返回值约定同一期:`uint32_t` tip id,**成功是 `kSuccess`(= 1000,不是 0)**,一律写 `err != kSuccess`。

### 4.1 容器编排(`cpp/libs/modules/bag/bag_service.{h,cpp}`)

```cpp
// 按 config 全或无扣除并逐条落销毁流水:任何一种持有量不够就一件不扣,返回 kBagInsufficientItems。
// itemsToRemove 为空,或任一条目 count == 0 → 打 ERROR,返回 kInvalidParameter,一件不扣、不落流水
//   (编程错误:0 数量的条目没有可扣的东西,却会进预检与流水,所以在入口判掉;调用方自己跳过不需要扣的项)。
// 跨 zone 冻结期返回 kInvalidParameter(一件不扣)。不判战斗(D48:闸放在玩法入口层)。
// 扣到 0 的堆会以 size == 0 留在包里,调用方随后调 MergeAndCompact(kMergeOnly) 回收(同战斗结算)。
// 常规的「不够」请调用方先用静默的 Bag::CheckQuantities / CheckItemsAvailable 预检,并区分版本错位与确定不足:
//   本函数内部的不足分支会打 ERROR 与调用栈。计数与扣除只认 ItemStore::IsCountableByConfig 的单位(混版本保护第 6 批)。
// 按 config 抽数量**不看是不是装备**:调用方必须保证被扣的 config 不是装备(§4.3 的写入口在提交前校验)。
static uint32_t RemoveItemsExact(
    entt::entity playerEntity,
    Bag &bag,
    const ItemCountMap &itemsToRemove,
    TransactionType txType,
    uint64_t correlationId = 0,
    const std::string &extra = {});
```

实现照 `RemoveItemsClamped` 的形状:参数检查 → 冻结闸 → `Bag::RemoveItems(itemsToRemove, &drained)`(全或无)→ 每条回执
`LogItemDestroy(playerEntity, guid, configId, amount, correlationId, extra, txType)`(注意 `LogItemDestroy` 的 `txType` 在最后,
与 `LogItemCreate` 的顺序不同)。`modules/bag` 其余文件除注释外零改动(容器层不认识「强化」「宝石」)。

### 4.2 纯规则(`equip_attribute_rules.h`,namespace `equiprules`,只依赖 std)

```cpp
inline constexpr uint32_t kMaxGemSockets = 8;   // 单件装备孔位数上限(表校验用;消费侧也只看实例上前这么多条宝石记录)

// 基础值按万分比放大。bonus == 0 严格恒等;全整数;饱和不回绕。算法写死(实现与用例都照这个):
//   inc    = SaturatingMul(base / 10000, bonus) + (base % 10000) * bonus / 10000   // 第二项最大 9999 * (2^32 - 1),不溢出;两项相加用饱和加
//   result = SaturatingAdd(base, inc)
// 锚点:ScaleByPermyriad(40, 500) == 42;ScaleByPermyriad(12, 500) == 12;ScaleByPermyriad(19999, 5000) == 29998;
//       ScaleByPermyriad(UINT64_MAX, 1) == UINT64_MAX;ScaleByPermyriad(x, 0) == x。
constexpr uint64_t ScaleByPermyriad(uint64_t baseValue, uint32_t bonusPermyriad);

// 强化判定。ratePermyriad == 0:必败且不消耗随机数;其余恰好消耗一次 rand(kPermyriadBase),
// 结果 < ratePermyriad 即成功(ratePermyriad >= kPermyriadBase 必成,但照样消耗一次 —— 与 RollAffixes 的粉 / 黄口径一致)。
template <class RandFn>
bool RollEnhanceSuccess(uint32_t ratePermyriad, RandFn&& rand);

// 强化失败后的等级:level > loss ? level - loss : 0。
constexpr uint32_t LevelAfterFailure(uint32_t level, uint32_t failLevelLoss);

struct GemSocketRule { uint32_t socket{0}; uint32_t gemType{0}; };   // gemType == 0 = 任意
enum class GemFit : uint8_t { kOk = 0, kSocketInvalid = 1, kSocketOccupied = 2, kTypeMismatch = 3, kCount };

// 孔位校验。sockets:该部位的孔位定义;occupied:实例上已占用的孔位号(原始记录,可含重复)。
// 判定顺序写死:① socket 不在 sockets 里 → kSocketInvalid;② socket 在 occupied 里 → kSocketOccupied;
// ③ gemType == 0 || (rule.gemType != 0 && rule.gemType != gemType) → kTypeMismatch;④ kOk。
// sockets 里同一 socket 有多条定义时取第一条。
GemFit CheckGemFit(const std::vector<GemSocketRule>& sockets, const std::vector<uint32_t>& occupied,
                   uint32_t socket, uint32_t gemType);
```

`detail` 里补一个 `constexpr uint64_t SaturatingMul(uint64_t a, uint64_t b)`(与现有 `SaturatingAdd` 并列)。
不往 `AffixTier` / `EffectKind` 里加值(那会放宽存档里随机属性的合法范围,并打破既有 `static_assert`)。

### 4.3 编排(`player_equip.{h,cpp}`,`class PlayerEquipSystem`,新增公开成员)

```cpp
struct EnhanceOutcome { bool success{false}; uint32_t level{0}; };

static uint32_t Enhance(entt::entity player, Guid itemId, EnhanceOutcome& outcome);
static uint32_t InlayGem(entt::entity player, Guid itemId, uint32_t socket, uint32_t gemConfigId);
static uint32_t RemoveGem(entt::entity player, Guid itemId, uint32_t socket);
static uint32_t MaxEnhanceLevel();          // EquipEnhance 表从 1 起连续的最大 id;表空 = 0
static bool IsGem(uint32_t configId);       // EquipGem 表有这一行 && Item 行存在 && equip_kind == 0

// 仅供单测:替换本文件全部掷骰(掷属性与强化判定)用的随机源。rand(n) 返回 [0, n);传空恢复生产随机源(tlsRandom)。
// 进程级静态、无锁:只许在测试的 SetUp / TearDown 里调(同 BagService::SetItemInstanceInitializer 的约定),用完必须复原。
// 没有它,编排层的强化失败分支(照扣、掉级)只能靠概率撞,写不出确定的用例。
static void SetRandomSourceForTest(std::function<uint64_t(uint64_t)> rand);
```

三个写入口都写在 `player_equip.cpp` 里(`CheckWritable` / `OnEquipmentChanged` 是文件内私有函数),骨架同 `Equip`:
**`CheckWritable` → 取 `PlayerBagsComp` → 全部纯校验 → 提交 → 改的是穿着的那件则 `OnEquipmentChanged`**。

**公共:定位装备 `LocateEquipment(bags, itemId)`**(匿名命名空间):先 `bags[kInventory]` 后 `bags[kEquipment]` 按 guid 找,
返回 {所在的 `Bag*`、是否穿着、Item 行、实例是否已初始化(`has_equip()`)}。拒绝码(顺序与 `Equip` 现有写法一致):
找不到 → `kEquipItemNotFound`;Item 行为空 / `equip_kind == 0` / `max_stack_size != 1` → `kEquipNotEquipment`;`size != 1` → `kEquipItemNotFound`。
**未初始化(`!has_equip()`)不在这里拒**:它是一期定义的合法状态(「本来就没有」),由三个入口各自映射成业务码并打 `LOG_WARN`:
强化 → `kEquipEnhanceUnavailable`;镶嵌 → `kEquipGemSocketInvalid`;取下 → `kEquipGemSocketEmpty`(读 `equip()` 的默认实例即可,不必特判)。

**强化 `Enhance`**

| 步 | 动作 | 失败返回 |
|---|---|---|
| 1 | `CheckWritable` | `kEntityIsNull` / `kEquipFrozen` / `kEquipInBattle` |
| 2 | `LocateEquipment`;未初始化 | 见上;`kEquipEnhanceUnavailable` |
| 3 | `max = MaxEnhanceLevel()`;`max == 0` | `kEquipEnhanceUnavailable` |
| 4 | `cur = min(实例.enhance_level, max)`;`cur >= max` | `kEquipEnhanceMaxLevel` |
| 5 | 取 `EquipEnhance` 第 `cur + 1` 行(缺行打 ERROR);`success_rate == 0`(必败的行不收费) | `kEquipInternalError`;`kEquipEnhanceUnavailable` |
| 6 | 要材料(`material_id != 0 && material_count > 0`)时:材料的 Item 行必须存在且 `equip_kind == 0`(否则打 ERROR —— 按 config 扣会把装备当材料销毁) | `kEquipInternalError` |
| 7 | 要材料时 `inventory.CheckQuantities({{material_id, material_count}})`:确定不足;只有算上带更新版本字段的材料才够(版本错位,打 `metric=persist_version_skew where=equip_enhance` WARN,先于金币判定,非终态,升级后可重试) | `kEquipEnhanceMaterialNotEnough`;`kFeatureUnavailable` |
| 8 | `cost_gold > 0` 时 `!CurrencySystem::CanAfford(player, kCurrencyGold, cost)`(`cost_gold > INT64_MAX` 视为付不起) | `kEquipEnhanceGoldNotEnough` |
| —— | **以上全是纯校验,任何一条失败什么都没动** | |
| 9 | `success = equiprules::RollEnhanceSuccess(row.success_rate, RandBelow)`(先掷后扣:流水的 `extra` 要带成败;结果在回包前不外露) | — |
| 10 | `cost_gold > 0` 时 `CurrencySystem::DeductCurrency(player, kCurrencyGold, cost, TX_EQUIP_ENHANCE, itemId)` | 打 ERROR,`kEquipInternalError`(什么都没动) |
| 11 | 要材料时 `BagService::RemoveItemsExact(player, inventory, {{material_id, material_count}}, TX_EQUIP_ENHANCE, itemId, extra)` | 打带 `[PlayerEquip][ENHANCE_PARTIAL]` 标签的 ERROR(金币已扣),`kEquipInternalError` |
| 12 | **重新**按 guid 取实例指针,写 `enhance_level = success ? cur + 1 : LevelAfterFailure(cur, row.fail_level_loss)` | 取不到:打 `[PlayerEquip][INSTANCE_LOST]` ERROR,`kEquipInternalError` |
| 13 | 扣过材料则 `BagService::MergeAndCompact(player, inventory, CompactPolicy::kMergeOnly)` 回收空堆(失败只打 ERROR)。**放在写实例之后**:「已扣」与「已写」之间不夹任何会销毁实例的步骤 | — |
| 14 | 等级变了且装备穿着 → `OnEquipmentChanged(player)` | — |
| 15 | `LOG_INFO`(player、guid、config、前后等级、成败);填 `outcome`;`return kSuccess` | — |

提交顺序排成「后面的步骤不会再失败」:scene 单线程、冻结已被第 1 步挡掉、余额与持有量都问过,所以第 10、11 步在正常情况下
不会失败;第 11 步的失败分支只为留下可对账的日志(没有货币退款原语,`AddCurrency` 会被欠款抵扣与封禁闸改写,不能当回滚用)。
金币与材料都不退(成败都扣是设计,不是故障)。

**镶嵌 `InlayGem`**

| 步 | 动作 | 失败返回 |
|---|---|---|
| 1 | `CheckWritable` | 同上 |
| 2 | `LocateEquipment`;未初始化 | 见上;`kEquipGemSocketInvalid` |
| 3 | `!IsGem(gemConfigId)`(表里没有这一行,或它的 Item 行不存在 / 是装备) | `kEquipGemNotGem` |
| 4 | `CheckGemFit(该部位的孔位定义, 实例 gems 里出现过的**全部** socket(原始记录,不按是否生效过滤;只有 `gem_config_id == 0` 的空壳记录视同不存在,见取下一节), socket, gem.gem_type)` | `kEquipGemSocketInvalid` / `kEquipGemSocketOccupied` / `kEquipGemTypeMismatch` |
| 5 | 实例 `gems_size() >= kMaxGemSockets`(干净数据走不到:孔位已被第 4 步判满;走到 = 脏数据,打 ERROR) | `kEquipInternalError` |
| 6 | `inventory.CheckQuantities({{gemConfigId, 1}})`:一颗都没有;只有带更新版本字段的堆里有(版本错位,打 `metric=persist_version_skew where=equip_inlay` WARN) | `kEquipGemNotOwned`;`kFeatureUnavailable` |
| —— | **以上全是纯校验** | |
| 7 | `BagService::RemoveItemsExact(player, inventory, {{gemConfigId, 1}}, TX_EQUIP_INLAY, itemId, extra)` | 打 ERROR,`kEquipInternalError`(什么都没动) |
| 8 | **重新**取实例指针,`add_gems()` 写 `{socket, gemConfigId}` | 取不到:打 `[PlayerEquip][GEM_LOST]` ERROR(含 gem config),`kEquipInternalError` |
| 9 | `MergeAndCompact(kMergeOnly)`(放在写实例之后,理由同强化) | — |
| 10 | 装备穿着 → `OnEquipmentChanged`;`LOG_INFO`;`return kSuccess` | — |

**取下 `RemoveGem`**

| 步 | 动作 | 失败返回 |
|---|---|---|
| 1–2 | `CheckWritable`、`LocateEquipment` | 同上 |
| 3 | 实例的 `gems`(原始记录)里找 `socket`,取第一条 `gem_config_id != 0` 的;没有(含未初始化的实例) | `kEquipGemSocketEmpty` |
| 3a | 宝石的 Item 行不存在,或被配成了装备(打 ERROR;前者发放必败,后者退回去会铸出一件掷了属性的装备实例) | `kEquipInternalError`(宝石仍在装备上) |
| 4 | 静默预检:`inventory.IsSpaceInsufficient(1)` 且没有哪一堆同种宝石还塞得下(同 config、不带 `equip` 段、`size < max_stack_size`,判据同容器层的并堆) | `kEquipGemBagFull` |
| 5 | `BagService::AddItems(player, inventory, blockList, {{gemConfig, 1}}, TX_EQUIP_GEM_REMOVE, itemId, extra)`。**先发物,成功后才抹宝石**(照 `PlayerMissionSystem::ClaimReward`) | `kBagItemNotStacked` / `kBagAddItemBagFull` → `kEquipGemBagFull`;其余 → 打 ERROR,`kEquipInternalError`(宝石仍在装备上) |
| 6 | **重新**取实例指针(第 5 步是一次 Insert),从 `gems` 里删掉该 `socket` 的**全部**条目;除退回的那一条之外每删一条打一条 ERROR(socket + gem_config_id:那是脏数据里的重复 / 空壳记录,被无偿清掉了) | 取不到:打 `[PlayerEquip][INSTANCE_LOST]` ERROR,`kEquipInternalError` |
| 7 | 装备穿着 → `OnEquipmentChanged`;取下的那颗当时不计入加成的话补一条 `LOG_WARN`(提交成功之后才打);`LOG_INFO`;`return kSuccess` | — |

取下不要求该孔位仍在 `EquipGemSocket` 表里,也不要求宝石仍在 `EquipGem` 表里(策划删孔 / 删宝石行之后,宝石照样取得下来);
宝石的 Item 行被删、或被配成了装备,则退不回去,宝石留在装备上(fail-closed;在第 3a 步就拒,不等到发放失败 —— 否则背包满的玩家
会先收到「背包已满」,腾出格子后才发现根本退不了)。「发回了但装备上还在」的复制路径不存在:每个 `Bag` 各有自己的
`ItemStore`,人物背包满了是整批拒绝、不淘汰。

**`gem_config_id == 0` 的记录视同不存在**(镶嵌的占用判定、取下、`ResolveGemsNow`、tooltip 四处同口径):协议里 0 就是「空孔」,
这种记录没有东西可退 —— 当成占用的话,服务器说「已占」、客户端显示空孔、取下必败,这个孔对玩家永久作废。它只可能来自外来的
`equip` 段或存档损坏(镶嵌入口自己写不出 0);重新嵌一次即自愈(下次取下时空壳记录一并清掉)。

**消费侧(加成与 tooltip 同口径,只从下面几个函数取值)**

```cpp
// 匿名命名空间,与 ResolveAffixNow 并列:
uint32_t EffectiveEnhanceLevel(const ItemComp& item);            // has_equip() ? min(enhance_level, MaxEnhanceLevel()) : 0
uint32_t EnhanceBonusPermyriad(uint32_t level);                  // level == 0 → 0;否则 EquipEnhance 第 level 行的 bonus_permyriad(缺行返回 0)
uint64_t ResolveBaseNow(uint64_t tableValue, uint32_t bonusPermyriad);   // = equiprules::ScaleByPermyriad
struct ResolvedGem { uint32_t socket; uint32_t gemConfigId; const EquipAttributeTable* attr; uint64_t value; };
std::vector<ResolvedGem> ResolveGemsNow(const ItemComp& item, const ItemTable& row);
```

`ResolveGemsNow` 的现查规则(入包边界整份信任 `equip` 段,所以消费侧必须自己校验)。只看实例上**前 `kMaxGemSockets` 条**记录;
**先按 socket 取第一条,再对这一条判**(第一条不合格则该 socket 整个不计,不回退到同 socket 的后一条):
① `EquipGem` 表有该 `gem_config_id`;② 该 `socket` 在这件装备部位的孔位定义里;③ 宝石属性行存在且效果被规则层接受(`IsEffectAccepted`)。
**不**复核宝石类型与孔位类型(类型是嵌入时的规则;策划事后改孔位类型不让已嵌的宝石失效)。结果按 `socket` 升序。
**读路径静默**:它被 `GetBag`(每件物品一次)与每次属性重算调用,不合格的记录只跳过、不打日志(否则策划删一行就是全服日志洪泛);
写入口(镶嵌 / 取下)碰到这类记录时打 `LOG_WARN`(带 item guid / socket / gem config)。

- `AccumulateWornItem`:基础属性循环里把 `base.base_attr_value()` 换成 `ResolveBaseNow(...)`;随机属性循环之后加一段宝石循环,
  逐条 `equiprules::ApplyEffect(bonus, attr.effect, attr.effect_param, value)`。
- `FillItemDisplay`:
  - 非装备:若 `IsGem(config)`,写 `gem_type`(18);宝石属性行存在且被接受时写 `gem_attr`(19);然后照旧返回。不是宝石则与现状逐位一致。
  - 装备:基础属性行 `value = ResolveBaseNow(表值, 当前万分比)`、`enhance_bonus = value − 表值`;
    写 `enhance_level`(14,有效等级)、`enhance_max_level`(15)、`enhance_bonus_permyriad`(20)。
  - `enhance_next`(16):仅当 `has_equip()`、有效等级 < 上限、且下一级的行存在并 `success_rate > 0`(与 `Enhance` 第 5 步同判据)时写;
    同时给每条基础属性行写 `enhance_next_value = ResolveBaseNow(表值, 下一级万分比)`。材料名 / 图标取材料的 Item 行(查不到留空)。
  - `gem_sockets`(17):仅 `has_equip()` 时写。按孔位定义逐孔下发(含空孔);**「占用」按原始记录**:实例 `gems` 里有该 socket 的记录
    (取第一条)→ `gem_config_id` = 记录里的 config、名称 / 图标取宝石的 Item 行(查不到留空);**「生效」按 `ResolveGemsNow`**:计入加成的才带 `attr`。
    这样服务器判占用的口径(`CheckGemFit` 的 `occupied`)与客户端看到的一致。
- `Recalculate` / `BuildPanel` / 战斗快照**不用改**:强化与宝石都经 `CollectBonus` 进 `EquipBonus`。`RecalcReason` 复用
  `kEquipmentChanged`(按比例保持当前气血 / 法力:强化加血装不回血)。`player_attribute.h` 里 `Recalculate` 的调用约定注释扩成
  「装备栏的内容一变,**或穿着的装备的实例数据一变**」。

**表校验(`ValidateTables` 追加)**。口径同一期:逐条 `++problems` + `LOG_ERROR "[PlayerEquip] 表校验: ..."`,不拒启;
**一行里互不相干的问题各计一条,同一个根因只计一次**。下面每个分号隔开的条件「恰好 +1」(用例按这张表逐项注入坏数据):

- `EquipEnhance`(逐行):`id == 0` 或 `id > MaxEnhanceLevel()`(不在从 1 起的连续前缀里);`success_rate` 不在 1..10000;
  连续前缀内 `bonus_permyriad` 小于上一级;`cost_gold > INT64_MAX`;`material_id != 0` 且 Item 行不存在(该行不再查后两项);
  `material_id != 0`、Item 行存在且 `equip_kind != 0`;`material_id != 0`、Item 行存在且 `battle_usable != 0`
  (强化石不能是战斗道具:战斗结算按 config 扣物,与强化扣材料会在重登窗口里交错);`material_id != 0` 且 `material_count == 0`;
  `material_id == 0` 且 `material_count != 0`。
- `EquipGem`(逐行):Item 行不存在(该行不再查后两项);Item 行存在且 `equip_kind != 0`;Item 行存在且 `battle_usable != 0`;
  `gem_type == 0`;属性行不存在;`value == 0`。**属性行存在但 `!IsEffectAccepted` 不计**(`EquipAttribute` 段已报过,同一根因)。
- `EquipGemSocket`:`equip_kind == 0`(逐行;这样的行不属于任何部位,不参加下面按部位的两项);`socket >= kMaxGemSockets`(逐行);
  同部位内重复的 socket,每多出一行 +1;按部位把**界内**的 socket 去重后不是 `0..n−1`,每个部位 +1 —— 但越界的行已逐行报过,
  它原本该占的那个号留下的缺口不另计:只有「缺口数 > 越界行数」才报(缺口数 = 界内最大号 + 1 − 界内不同号的个数)。
- 三张表都必须已加载(`InstallItemInitializer` 的调用点在表加载完成回调里,无需新增调用点)。

校验只打日志不拒启,且单表热更不重跑,所以**写入口对缺行、以及被配成装备的材料 / 宝石行一律 fail-closed**(上表第 5、6 步与 `IsGem`)。

### 4.4 入口(`player_bag_handler.{h,cpp}`)

三个新方法照 `EquipItem` / `UnequipItem`:参数粗检 → 调 `PlayerEquipSystem` → 失败直接写本次 `response->mutable_error_message()->set_id(...)` → 成功 `ReplyInventoryAndEquipment`。

- `EnhanceEquip`:`item_id == 0` → `kInvalidParameter`;成功后另写 `success` 与 `enhance_level`。
- `InlayGem`:`item_id == 0 || gem_config_id == 0` → `kInvalidParameter`(`socket` 可以是 0)。
- `RemoveGem`:`item_id == 0` → `kInvalidParameter`。

自定义逻辑只写在 `.cpp` 的 `///<<< BEGIN WRITING YOUR CODE` 守护段内;`.h` 的声明与 `case 5 / 6 / 7` 是生成物的手工补丁,
以 proto-gen 重生成为准。不需要动 gate 的 GM 名单(不是 `Gm*` 指令)。

限流:`equip_xlsx_patch.py` 的 `LIMITER_NEW_METHODS` 加 `EnhanceEquip: 10`、`InlayGem: 5`、`RemoveGem: 5`(发号之后才能跑
`message-limiter`)。档位语义:限流器按秒级时间戳、差值 ≥ 2 才出窗,档位 N 实际是「相邻两秒合计 N 个」;强化是唯一会被长时间连点的
装备操作,给 10(≈ 5 次 / 秒)。没进限流表之前吃默认档 3(≈ 1.5 次 / 秒),超限计非法包、累计 50 次被 gate 断线 —— 所以客户端
数据层自带发包间隔(§6.1),且 `message-limiter` 执行之前不拿真实客户端验收强化。

## 5. 校验与不变量

1. 强化等级与宝石只存在于 `EquipInstanceData`;穿脱(Take / PutInstance)、存档与跨区(`bag_marshal`)是整份搬运,不丢、不重置。
   回滚执行尚未实现(`player_rollback_handler.cpp` 的 TODO),实现时必须走 `bag_marshal`。
2. 强化与宝石的数值不入库:入库的只有「+几」和「哪个孔嵌了哪种宝石」;改表立即对存量装备生效(等级按表上限夹取,不洗存档)。
3. 加成与 tooltip 同口径:基础属性(含下一级预览值)只从 `ResolveBaseNow` 取,宝石属性只从 `ResolveGemsNow` 取;不计入加成的宝石不显示属性。
   「孔位是否占用」是另一件事,服务器与客户端都只看原始记录。
4. 0 级严格恒等:`ScaleByPermyriad(x, 0) == x`,未强化、未镶嵌的装备与一期逐位一致(一期全部用例与 robot 冒烟的期望值不变)。
5. 强化 / 镶嵌 / 取下与穿脱共用 `CheckWritable`:战斗中、跨区冻结中拒绝。它们是 scene 侧**第一批玩家可触发的背包扣减入口**,
   战斗闸必须在入口层(D48)。
6. 先校验后提交;提交段没有可失败的常规路径。取下宝石「先发物、后抹除」:任何失败都不会凭空少宝石。
7. 不对 `!has_equip()` 的实例调 `mutable_equip()`;不对非装备写 `equip` 段(否则该实例永远不能并堆)。
8. `ItemStore` 返回的实例指针在下一次 Insert / Erase(含整理)后失效:改实例前必须重新取。
9. 被扣的材料 / 宝石 config 必须不是装备:按 config 抽数量不看 `has_equip()`,会把装备实例当材料销毁。表校验报、写入口挡,两道都要有。
10. 强化的概率失败是成功的响应(无 tip,带两份快照,`success = false`)。
11. 导表后的同批重编要求同一期 §5 不变量 8、9(Item 表加了 13 行,战斗指纹会变;三张新表只在 scene 读,不进指纹)。

## 6. 客户端契约（Unity 数据层与纯逻辑）

### 6.1 `PlayerFeaturesClient`(数据层)

```csharp
public readonly struct EquipEnhanceOutcome { public ulong ItemId { get; }  public bool Success { get; }  public uint Level { get; } }

public const int GoldCurrencyIndex = 0;                     // 镜像服务器 currency.h 的 kCurrencyGold
public const long ForgeMinIntervalMs = 700;                 // 强化 / 镶嵌 / 取下 两次发包的最小间隔(按默认限流档 3 留:持续连点要 ≥ 667ms)
public Func<long> NowMs { get; set; }                       // 发包间隔用的时钟;传 null 恢复默认的单调时钟。只给用例替换
public BagInfo CharacterBag => _bags[CharacterBagType];     // 人物背包(Bag 是「当前查看的那一类」,可能是仓库)
public EquipEnhanceOutcome? LastEnhanceOutcome { get; private set; }
public event Action<EquipEnhanceOutcome> OnEnhanceResult;
public void EnhanceEquip(ulong itemId);
public void InlayGem(ulong itemId, uint socket, uint gemConfigId);
public void RemoveGem(ulong itemId, uint socket);
```

- 与穿脱共用同一个在途标志(`BusyEquip`)与请求序号(`_equipActionRequest`):现成的互斥、延后补发、旧连接 / 旧角色回包拒绝全部沿用。
  `PerformEquipAction(ulong itemId, bool equip)` 的 `bool equip` 换成「操作名 + 发包委托」;`Complete` 增加第 4 个参数 `Action applied = null`。
  **穿 / 脱两条路径的行为与文案逐字不变**(既有 70 条用例守着)。
- 操作名(用于「{操作名}:{原因}」):强化 `无法强化`;镶嵌 `镶嵌失败`;取下 `取下失败`。(不用「强化失败」:会与掷骰失败混淆。)
- 成功路径顺序固定:覆盖两份快照 → `applied?.Invoke()`(强化在这里写 `LastEnhanceOutcome`)→ `OnChanged`(仍只触发一次)→
  强化再触发 `OnEnhanceResult`(触发前复核 `IsCurrent` 与请求序号)→ `SendDeferredReads`。
- `LastEnhanceOutcome` 的清理点:每次装备类操作(穿 / 脱 / 强化 / 镶嵌 / 取下)开始时、`ClearSnapshots`;`Reject` 路径不写。`Dispose` 把 `OnEnhanceResult` 置空。
- **发包间隔**:三个新操作距离上一次**新操作发包**不足 `ForgeMinIntervalMs` 的点击按连点丢弃(不报错、不发包;闸紧跟在 `BusyEquip`
  之后、互斥提示与本地拒绝之前)。时钟可注入(公开属性 `NowMs`,默认单调时钟;用例里替换 —— 测试程序集没有 `InternalsVisibleTo`,
  所以不是 internal),不改构造函数签名(`GameClient` 那一处调用是别的会话的在途文件)。数据层不提供「冷却中」状态,界面要反馈就按这个常量自己做按钮冷却。
- **发包前的本地拒绝**(不发包、不重拉,文案走 `OnError`):`itemId == 0`;镶嵌时 `gemConfigId == 0`;
  目标不在人物背包与装备栏两份快照里;强化时已满级(`EnhanceMaxLevel > 0 && EnhanceLevel >= EnhanceMaxLevel`)或未开放(`EnhanceMaxLevel == 0`);
  镶嵌时目标孔不存在 / 已占用(`GemConfigId != 0`);取下时目标孔为空;镶嵌时人物背包里该宝石数量为 0。
  金币 / 材料不足**不在本地拦**(快照可能偏旧,拦了会把人锁死),只给模型置灰用。两份快照任一缺失时不做本地拒绝,直接发包。
- 回包整体覆盖两份快照;业务 tip 先看响应体;传输失败绝不自动重发(强化不是幂等操作),只重拉快照。
- 失败后是否重拉(`IsRuleRejection` 扩充;判据不变:「服务器没动过状态,且本地快照没有被证明过期」才不重拉):

  | 不重拉 | 重拉两份快照 |
  |---|---|
  | `KEquipGemTypeMismatch`、`KEquipGemBagFull`,以及既有的 `KEquipNotEquipment` / `KEquipInBattle` / `KEquipFrozen` / 限流 | `KEquipGemNotOwned`、`KEquipGemSocketOccupied`、`KEquipGemSocketEmpty`、`KEquipGemSocketInvalid`、`KEquipGemNotGem`、`KEquipEnhanceMaxLevel`,以及既有的 `KEquipItemNotFound` / `KEquipInternalError` / 未收录的码 |
  | `KEquipEnhanceMaterialNotEnough` / `KEquipEnhanceGoldNotEnough`:服务器给的码 == 发包那一刻 `EquipBagModel.CheckEnhance` 的结论(本来就知道缺这一样)→ 不重拉 | 同两个码:本地以为够,或以为缺的是另一样 → 快照过期,重拉 |
  | `KEquipEnhanceUnavailable`:本地快照本来就没有下一级预览(`CheckEnhance` 得 `Unavailable`:实例未初始化,或下一级是必败的行 —— 服务器不给预览、也照样拒)→ 不重拉 | 同一个码:本地带着预览、以为能强化 → 快照过期,重拉 |

- 文案(`DescribeEquipTip`)逐字镜像 §2.5,只写枚举名不写数字。

### 6.2 `EquipDisplay`(纯函数排版;只管排版与顺序,不推算任何数值)

逐字格式(全角冒号 `：`、全角括号 `（）`;`percent` 的行数值后带 `%`):

| 场景 | 格式 | 例 |
|---|---|---|
| 基础属性行,`EnhanceBonus == 0` | 与现状逐字相同:`{名称}：{值}` | `伤害：40` |
| 基础属性行,`EnhanceBonus > 0` | `{名称}：{值}（+{增量}）` | `伤害：46（+6）` |
| 基础属性行的下一级预览 | `{名称}：{值} → {EnhanceNextValue}` | `伤害：46 → 48` |
| 名称后缀 | `{名称} +{N}`;`N == 0` 不加 | `青铜剑 +3` |
| 空孔 | `{孔名}：未镶嵌` | `攻击孔：未镶嵌` |
| 已镶且生效 | `{孔名}：{宝石名}（{属性名} +{值}）` | `攻击孔：攻击宝石·一级（伤害 +30）` |
| 已镶但不生效(无 `attr`) | `{孔名}：{宝石名}（未生效）` | |
| 孔名 | `Name` 非空用它;空则 `孔位 {socket + 1}` | `孔位 1` |
| 宝石物品说明行 | `{属性名} +{值}` | `伤害 +30` |
| 成功率 | 万分比 / 100,至多两位小数,去掉末尾的 0,加 `%` | `90%`、`12.5%` |
| 强化消耗 | `金币 {cost}`;有材料再加 `、{材料名} ×{count}`;材料名 / 宝石名为空用 `物品 · {config}`(与背包格子上缺名物品的占位一致) | `金币 5000、强化石 ×2` |

`LinesOf` 的签名与返回不变(只含基础 + 随机属性);孔位行另出函数。`EquipAttrTier` 不加新值。

### 6.3 `EquipBagModel`(纯逻辑模型;函数显式接收人物背包快照)

- 孔位视图:一件装备的 `GemSockets` 按 `Socket` 升序。
- 「这个孔能嵌哪些宝石」:人物背包里 `GemType != 0` 且(孔的 `GemType == 0` 或相等)的物品,**按 `ConfigId` 汇总数量**(同一种宝石可能分几堆;请求按 config 扣)。
- 强化前置检查(用于按钮置灰与 §6.1 的重拉判定):返回 `None / Unavailable / MaxLevel / GoldNotEnough / MaterialNotEnough / Unknown`。
  满级 := `EnhanceMaxLevel > 0 && EnhanceLevel >= EnhanceMaxLevel`;未开放 := `EnhanceMaxLevel == 0`;没有 `EnhanceNext`(且不属于前两种)→ `Unavailable`;
  金币读人物背包 `Currency.Values[GoldCurrencyIndex]`,`Currency` 缺失或下标越界 → `Unknown`(不置灰);材料数量按 `ConfigId` 汇总。

### 6.4 用例(`Assets/Tests/EditMode/Battle/`,新文件各配 `.meta`)

`EquipForgeClientTests.cs`(数据层:三个请求的成功 / 各类拒绝 / 重拉分类 / 本地拒绝 / 发包间隔 / 强化结果发布与清理 / 旧连接与旧角色回包 /
与穿脱整理任务的互斥)、`EquipForgeModelTests.cs`(排版逐字、模型)。照 `EquipClientTests.cs` 的夹具写;`FakeBattleTransport` 是 internal,
只能放这个目录。窗口级用例(`EquipWindowTests.cs`)是别的会话的在途文件,不加。

### 6.5 正式生成与客户端验证边界

三个方法已在服务器正式发号：`InlayGem=250`、`EnhanceEquip=251`、`RemoveGem=252`；11 个新 tip
及 PlayerBag 新字段也已生成。客户端白名单应映射到对应生成常量，不得使用临时占位号。

首次实现时的隔离检出缺这些产物，离线验证曾补临时符号；正式生成后不能继续把 CS0117 当作
“预期可忽略错误”，也不再把“产物未到位所以不能合并”作为当前步骤。2026-10-13 进度记录了
客户端代理同步，本次未读取客户端仓，其编译、Unity 与界面验证分别以客户端记录为准。

界面接线的历史说明见客户端 `Docs/EquipmentForge-20261009.md`；本次不对其当前实现作完成声明。

## 7. 验证

以下为测试覆盖清单。2026-10-13 的 C++ 构建、单测、robot 与联机冒烟证据见 §11；当前合并后的
代码仍须按 §9 复验。客户端离线测试分别见 §10.8（首次实现）与 §11.1（正式生成后），不等于当前 Unity 验收。

| 层 | 位置 | 内容 |
|---|---|---|
| 纯规则 | `cpp/tests/turn_battle_engine_test/equip_attribute_rules_test.cpp` | `ScaleByPermyriad`(恒等、取整、五个锚点、饱和)、`RollEnhanceSuccess`(0 不耗随机数、边界、满值照掷)、`LevelAfterFailure`、`CheckGemFit`(四种结果与判定顺序、`gemType == 0`) |
| 容器编排 | `cpp/tests/bag_test/bag_remove_exact_test.cpp`(新) | `RemoveItemsExact`:全或无、多 config、空表与 0 数量(持有 / 不持有)、流水类型、冻结、扣到 0 留空堆 |
| 实例层 | `cpp/tests/bag_test/bag_instance_data_test.cpp` | 新字段随搬运 / 存档往返(含 presence);`ExpectSameEquip` 覆盖新字段 |
| 装备编排 | `cpp/tests/bag_test/player_equip_test.cpp` | 三张新表的校验(逐项 +1)、强化(成功 / 失败 / 掉级 / 各拒绝码 / 扣费对账 / 空堆回收 / 材料被配成装备)、镶嵌与取下(各拒绝码 / 恰好扣 1 / 先发后抹 / 脏数据)、加成汇总、tooltip 投影(占用与生效两个口径)、handler、method index |
| 属性接入 | `cpp/tests/bag_test/player_equip_attribute_test.cpp` | 穿着时强化 / 镶嵌 / 取下的六项增量、当前气血按比例、存档往返后重算 |
| GM 闸名单 | `cpp/tests/currency_test/client_gm_gate_test.cpp` | 三个新方法进「正常 RPC」名单 |
| 存档 / 跨区一跳 | `cpp/tests/bag_test/player_feature_persistence_test.cpp`、`cpp/tests/cross_zone_test/cross_zone_test.cpp` | 既有的两条 `EquipInstanceData…` 往返用例里,实例带上强化等级与宝石(孔位号故意不等于下标) |
| 端到端 | `robot/robot.exe -c etc/equip_forge_smoke.yaml`(账号 `robot_9104`) | 见下 |
| 客户端 | `Assets/Tests/EditMode/Battle/EquipForgeClientTests.cs`、`EquipForgeModelTests.cs` | 见 §6.4 |

**robot 冒烟 `equip-forge-smoke`**(新 mode;改 `robot/config/config.go` 白名单与报错串、`robot/main.go` 分发、3 个 handler 桩、
`robot/logic/handler/player_features_response.go` 的两个 switch;新文件复用 `equipSmokeSession` 的 `call / getBag / gmGrant / relogin`
与既有 tip 变量,新增的包级标识符一律带 `equipForge` 前缀 —— 同在 `package main`,重名即编不过):

0. 预备:装备全卸;目标 = `item_id` 最小且 `enhance_level < enhance_max_level` 的 1101,没有就发一把(空位不足判红并提示清号);
   目标各孔的宝石先全部取下。金币与材料「缺多少补多少」(`GmAddCurrency` / `GmGrantItem`,单次上限 99 够一级所需)。
1. 显示:目标带 `enhance_max_level == 12`、`gem_sockets` 两个孔(类型 1 / 0、名称非空)、`enhance_next.next_level == enhance_level + 1`;
   背包里的 2101 带 `gem_type == 1` 与 `gem_attr`。
2. 拒绝码(只测能确定造出来的,每条之后断言两份快照不变):`GemNotGem`(`gem_config_id = 2001`)、`GemSocketInvalid`(`socket = 7`)、
   `GemTypeMismatch`(0 号孔嵌 2201)、`GemNotOwned`(0 号孔嵌从不发放的 2103)、`GemSocketEmpty`;`EnhanceEquip(0)` / `InlayGem(…, 0)` 的参数错误。
3. 强化(在背包里强化一次、穿上后再强化一次):对账**以强化前那份快照里的 `enhance_next` 为准** —— 金币差 = `cost_gold`
   (读 `bag.currency.values[0]`)、材料差 = `material_count`、成功则等级 +1、失败则 `max(0, level − fail_level_loss)`;
   `success_rate == 10000` 时必须成功,其余两种结果都接受。基础属性断言 `value − enhance_bonus == 表值`、
   `enhance_bonus == 表值 × enhance_bonus_permyriad / 10000`(向下取整)。只有「目标穿在身上且等级变了」才等属性面板推送。
4. 镶嵌:0 号孔嵌 2101 → 该孔 `gem_config_id == 2101` 且带 `attr`、背包里 2101 少 1;再嵌同孔 → `GemSocketOccupied`;穿着时面板物伤 / 法伤各 + 宝石值。
5. 重登:强化等级与宝石都在。
6. 取下:该孔变空、2101 回到原数量、面板回到镶嵌前。`EQUIP_FORGE_SMOKE_OK`。

每份响应快照都断言没有 `count == 0` 的条目(空堆必须已回收)。同一种宝石的取下次数远小于 50(见 §8 的异常告警阈值)。
强化不可逆且 robot 没有删物手段:目标到 +12 之后下一轮会自动换一把新的 1101;`EnhanceMaxLevel` / `EnhanceUnavailable` / `GemBagFull`
机器人造不出来,由 C++ 单测覆盖。纯校验函数配 `robot/equip_forge_smoke_scenario_test.go`。

## 8. 已知缺口

- 洗练 / 宝石合成 / 打孔 / 强化保护未做(§1.3)。
- 强化石与宝石没有任何产出途径(掉落 / 任务 / 商店都没配),目前只能 GM 发物;装备本身同样(一期遗留)。
- 流水不带实例数据:强化等级与宝石无法从流水还原。按 guid 扣出(`RemoveItemsByGuid`,寄售托管将来靠它)、临时格淘汰、还原落位失败的丢弃
  三条路径都只留 guid / config / size —— 二期之后它们丢的是 +N 与宝石;接寄售托管之前必须让扣出回执带整份 `ItemComp`。
- 宝石的 `item_uuid` 血缘在镶嵌处断开(销毁 G1、取下铸出 G2),两条流水靠「装备 guid + 宝石 config + extra 里的 socket」配对。
- 没有货币退款原语:强化第 11 步(扣材料)若在金币已扣之后失败,只留 `[ENHANCE_PARTIAL]` 日志供人工对账。正常路径走不到。
- 强化不是幂等操作:客户端超时后玩家再点一次会再扣一次。客户端的约定是传输失败只重拉快照、不自动重发。
- 取下免费且走正式发放入口:同一种宝石 10 分钟内取下第 51 次起,每次都会产生一条 item 类异常获得告警(`AnomalyDetector` 默认阈值
  50 次 / 600 秒,无冷却);「嵌 → 取」循环的流水量只受限流与客户端发包间隔约束。要根治就给取下加成本(表列)。
- 宝石 config 被 GM 封禁时取下会失败(`AddItems` 过发放封禁闸,回 `kInvalidParameter`),对玩家表现为 `kEquipInternalError`,且每次点击各打一条 WARN + ERROR。
- 「战斗中禁写」只认 `InBattleComp`:一期 §8 记的重登 / 离线结算窗口同样适用于强化与镶嵌;登录时 Redis 未就绪的话,窗口持续到下次进场。
  核过没有资产复制路径(强化石 / 宝石不是战斗道具,由表校验守着;金币与背包同一条记录落盘)。仍计划在 `PlayerBattleSystem::IsInBattle` 一处收口。
- 入包边界整份信任 `equip` 段(一期 §8):消费侧已对强化等级夹取、对宝石现查并限制遍历条数,但做邮件回流 / 寄售之前仍须在玩法层补入包校验。
- 策划删掉某部位的孔位定义后,该孔里已嵌的宝石不再计入加成、tooltip 也不显示这个孔(仍可用 `RemoveGem` 按孔位号取下,但界面上看不到入口)。
  孔还在而宝石行被删 / 属性行不可用时,孔显示为「已镶但未生效」,可以取下。
- `kEquipInBattle` / `kEquipFrozen` 的文案是「…无法更换装备」,强化 / 镶嵌复用时措辞不完全贴切。
- 配表热更后在线玩家要到下一次重算才生效(一期遗留)。
- **Java 版（AGENTS §12）**：2026-10-13 装备整合进度仍登记待做，本次未核对 Java 仓的 `PARITY.md`，不提升完成状态。
  仓库位置以根 AGENTS.md §12 为准；对齐时核对本文 §2 / §3 的字段、3 个 RPC、11 个 tip、3 张新表与 Item 新行。

## 9. 已有证据与后续验证（AGENTS §10.1）

### 9.0 当前代码与证据来源

实现与生成物已在主干，参考 `6e212616bf` 与 `0b40715d0` 及
[PROGRESS](../PROGRESS.md) 的“2026-10-13 装备强化与镶嵌并入主干、正式生成与定向验证”。
同日后续的 C++ / Go 编译、单测与起栈冒烟证据见 §11 及 PROGRESS 的“2026-10-13 装备强化与镶嵌：主干编译、单测与起栈冒烟通过”。
本节保留 2026-10-15 更新后的复验入口，§11 保留当日源码与产物的结果；历史通过不替代当前合并后的复验。
旧 `E:\work` 工作树、分支合并前提和首次生成准备不再是当前执行入口。构建前先核对当前主仓的
并行改动，不覆盖其他会话的 RPC、ECS 或配置工作。

### 9.1 正式生成与发号已完成

- 正式执行两次导表和一次全量 proto-gen；原消息号 0–249 逐行保留，新增
  `InlayGem=250`、`EnhanceEquip=251`、`RemoveGem=252`。这些号已入库，不重新占号。
- 二期新增 tip 28011–28021，一期与二期合计 28000–28021；Tip 编号事实源仍是源表与发号器。
- 当次导表清单为 version 25 / 42 表；三张新增表分别为 12 / 12 / 6 行，Item 为 61 行。
  42 张源表与 84 份 JSON/PB 的尺寸、哈希、行数核验一致。这是 10-13 的生成基线，后续源表
  修改后须按新清单核对，不能强制恢复旧版本号或行数。
- `MessageLimiter.xlsx` 已按生成的方法号追加强化、镶嵌和取下规则并再次导表；旧“发号之后再
  首次执行 message-limiter”的步骤已完成。运行前仍要确认服务读取同批生成数据。
- handler 守护段与 scene gRPC 自定义实现当次生成前后逐字节一致，Agones 许可块保留。

本次文档修订不重新执行导表或生成。后续确有契约修改时按正常流程生成，并核对既有编号与
守护段，不能按旧分支的空号或冲突约定回退已分配的号。

### 9.2 C++ 构建与单测

2026-10-13 的 C++ 构建与运行证据见 §11.2、§11.3；当前合并后的代码尚未在此记录复验结果。复验按一期 §9.2 的依赖顺序编译，全部 MSBuild 使用
`/m:1 /p:Configuration=Debug /p:Platform=x64`；modules 与 scene 必须同批重编。
当前工作区的 RPC/ECS 修改可能影响构建，失败按实际文件归因，不把源码存在当作编译通过。

读表测试前确认 `bin/etc/base_deploy_config.yaml` 的 `table_data_directory` 中七张装备表与
依赖齐全。文件缺失可能触发 `File2String` 的 `LOG_FATAL`，整个进程 abort，不是某条用例失败。

| 测试程序 | 过滤器或范围 | 通过标准 |
|---|---|---|
| `build\cpp\tests\turn_battle_engine_test.exe` | `Equip*` 及既有引擎回归 | 全部通过，含强化比例与孔位适配 |
| `build\cpp\tests\bag_test.exe` | `PlayerEquipTest.*` | 强化、镶嵌、取下、tooltip、加成、handler 正常与拒绝路径通过 |
| 同上 | `PlayerEquipAttributeTest.*`、`BagInstanceDataTest.*`、`BagRemoveExactTest.*` | 属性、实例往返与精确扣减通过 |
| 同上 | `PlayerFeaturePersistenceTest.EquipInstanceDataRoundTripsThroughDatabaseRecord` 及全量回归 | 强化与宝石保存恢复，既有用例不回归 |
| `build\cpp\tests\currency_test.exe` | `ClientGmGateTest.*` | 全部通过 |
| `build\cpp\tests\cross_zone_test.exe` | `CrossZoneBagMarshal.*` 及既有回归 | 装备实例带强化/宝石往返通过 |

当次实现清单曾列 `PlayerEquipTest` 151、`PlayerEquipAttributeTest` 37、`BagInstanceDataTest` 31、
`BagRemoveExactTest` 10、`Equip*` 56 条；这些是当时的用例数，后续新增用例不应被旧数字挡住。

退出码非 0、缺测试汇总或出现 abort 都不能算通过。保留首个编译错误上下文和失败用例完整输出；
不要注释断言、跳过用例或修改数值期望来绕过失败。

### 9.3 失败定位

1. 表文件或配置目录不匹配：先检查读取路径与同批数据；`RequireForgeRows` 前置失败或
   `ShippedTablesPassValidation` 失败时，保留源表校验输出，不改断言。
2. protobuf、tip、表管理器符号缺失：核对正在构建的源码、生成目录和工程登记是否同批，不能
   简单认定“尚未首次生成”；主干已有正式生成产物。
3. `ScaleByPermyriad anchor:` 等 static_assert 失败：保留完整编译输出，核对算法/字段契约，
   不关闭断言。
4. 强化半提交、丢失实例或宝石：保留 `[ENHANCE_PARTIAL]`、`[INSTANCE_LOST]`、`[GEM_LOST]`
   日志及账号场景上下文，停止该失败流程并报告，不用重试掩盖资产状态。

### 9.4 robot 与联机冒烟

**已有定向证据**：2026-10-13 在 `robot` 运行
`go test -mod=mod -run '^TestEquip' -count=1 -timeout=120s -json .`，32 个顶层用例、含子用例
253 项通过，0 失败；新增强化镶嵌测试在内。该命令不验证 C++、vendor 或真实服务。
同日另有 `-mod=vendor` 构建、静态检查与测试（§11.4），以及一期、二期两遍联机冒烟（§11.7）；均仅证明当日源码与产物。

后续 Go 构建/复测使用当前模块声明的 Go 版本与 `GOTOOLCHAIN=local`，不再固定历史的
Go 1.26.5 安装目录。不要仅为复测改 go.mod、go.sum 或 vendor；缺依赖/工具按根门禁报告。

在已授权 dev 栈用同一批产物重启受影响服务，scene 与 battle 同批换表，GM 闸使用允许的开发
模式。先跑既有登录基线与一期 `etc/equip_smoke.yaml`，再在 `robot` 运行
`.\robot.exe -c etc/equip_forge_smoke.yaml` 两遍（账号 `robot_9104`）。

通过标准：两遍均有 `EQUIP_FORGE_SMOKE_OK` 且退出码 0；新账号第一遍发武器、第二遍复用；
强化成败与当次表中预览及实扣一致；镶嵌前与取下后的物伤相等；重登后强化与宝石恢复，scene
表校验问题数为 0。等级与成功率按运行时表值对账，不把 10-10 的占位数值当永久值。

失败保留 `EQUIP_FORGE_SMOKE_FAIL step=… reason=…` 及前文、同时间段 scene 的 `[PlayerEquip]`
与 ERROR。`prep-*` / `display-*` 先查同批表和节点；超时检查 gate/scene 产物及转发；
`enhance-*-reconcile` 查预览、实扣与等级，`relogin-restored` 查保存恢复链。不要随意清真实账号。

本次没有启动栈、执行联机冒烟或压测。若另做登录压测，须遵守根 AGENTS.md §6。

### 9.5 客户端与 Java 版

2026-10-13 进度记录了客户端代理同步正式 C# 协议、消息号、表代码与 handler；本次没有读取
独立客户端仓，也没有运行其编译、Unity 或截图。旧“19 处预期 CS0117”与临时占位号是正式生成
之前的离线验证背景，不是当前可接受的编译失败。

2026-10-10 的离线 NUnit 244/244 与变异测试记录保留在 §10.8；2026-10-13 正式生成后的离线 Roslyn + NUnit 244/244 记录保留在 §11.1。
这些证据不替代当前客户端的 Unity 与界面验收；当日界面入口仍未做（§11.6）。后续由客户端仓按其规范复核装备页、tooltip、强化镶嵌、失败提示与重登。Java 版以独立
仓 `PARITY.md` 为准，本次不读取或更新，不提升实现状态。

## 10. 实现偏离记录

实现与 §2–§6 不一致、或 §2–§6 没写明而落地时补上的决定。评审后被采纳的修订已并入正文,这里只记要点与出处。

### 10.1 配置表、协议、补丁脚本(§2、§3)

1. `forge-tip-codes` 比 §2.5 多两条「先核对再动手」的中止条件:`//equip_error` 组在一期 11 个码之后已有别的(非本批)码;
   按发号规则推算两期 22 个码的号不是从段首连续。新码的号由行序决定,这两种现状下照写不会报错、但号会错位。
2. Item 新 13 行只写了 `id / max_stack_size / equip_kind(0) / battle_usable(0) / name / description` 六格;`battle_heal_hp / battle_heal_mp` 留空
   (既有 15–28 号行这两格填的是 0)。结果是 `item.json` 里这 13 行没有这两个键,语义等同 0。
3. 一期 `cmd_new_tables` 的循环体原样抽成 `create_scalar_tables`(`new-tables` 与 `forge-tables` 共用);`tip_parse` 的返回多了 `code_rows`。
   一期五个子命令的输出与行为未变(改动前后各 `--dry-run` 一次)。
4. proto 的字段号 / 字段名 / 类型 / rpc 顺序与 §3 一致;长注释按所在文件的风格改成字段上方的前导注释。
   `bag_quest_mail_data.proto` 文件头那段英文说明同步补了一段「6 / 7 / 8 号不再启用」。
5. Claude 做过一次**沙盒导表**(产物只写到会话临时目录,不算正式导表):退出码 0;三张新表 12 / 12 / 6 行、`item.json` 61 行、
   `equip_error` 28000–28021 连续、三处工程登记与实际产出的文件集一一对上。

### 10.2 背包容器编排(§4.1)

1. `RemoveItemsExact` 的用例只断言流水的**条数**(用 txlog 号段计数),没有断言 `tx_type / correlation_id / extra` 的值:
   `TransactionLogSystem` 在单测里没有可读侧(直接交给 Kafka 生产者)。参数顺序靠类型系统守着;「调用方传错 txType 的值」单测发现不了。
2. 多改了两句注释:`bag_service.h` 带整份 `ItemComp` 的 `AddItems` 的信任边界(二期字段同样原样入库不校验);`bag_marshal.cpp` 的
   `ItemToEntry`(新字段随整份 `CopyFrom` 走,别改成逐字段拷)。

### 10.3 纯规则(§4.2)

1. `CheckGemFit` 的定义带 `inline`(头文件里的非模板函数);另加 6 条 `static_assert`(五个锚点 + `GemFit::kCount == 4`)。
2. `RollEnhanceSuccess` 对不守约的随机源(返回值 ≥ n)按 `detail::RollBelow` 的既有口径夹成 n − 1,即除非必成否则算失败。

### 10.4 编排与入口(§4.3、§4.4)

1. `EnhanceOutcome` 是 `PlayerEquipSystem` 的嵌套类型;`Enhance` 开头重置 `outcome`。
2. `ReplyInventoryAndEquipment` 模板的返回类型由 `void` 改为 `bool`(两份快照是否都带上);`EnhanceEquip` 只在它返回 true 时写
   `success / enhance_level` —— 否则快照建不出来的响应会同时带着 tip 与 `success = true`。穿脱两个既有调用点不看返回值,行为不变。
3. 提交点写实例之前的「重新按 guid 取」统一收进 `RefetchInitializedInstance`,它同时复核 `has_equip()`(不变量 7 在提交点也成立)。
4. `[PlayerEquip][ENHANCE_PARTIAL]` 一律带标签,用 `gold_deducted=<实际已扣数>` 说明有没有要对账的金币(该级 `cost_gold` 可以是 0)。
5. `enhance_next` 里的材料字段只在「要材料」(与 `Enhance` 同判据)时才写:`material_id != 0` 但 `material_count == 0` 的脏行按「不要材料」下发。
6. 孔位定义里同一个 socket 有多行(脏表)时只下发排在前面的那一行(与 `CheckGemFit` 取第一条一致);`GetByEquipKind` 的行序 = 表内行序
   (对沙盒生成物核过)。
7. 写入口的 `LOG_WARN` 落在两个点:镶嵌被拒为「已占用」且占着孔的那条记录当前不生效;取下的那条记录当前不生效(照常放行,提交成功后才打)。
8. 表校验:`EquipGem` 的属性行存在但 effect 不被接受时既不计数也不打日志(`EquipAttribute` 段已为那一行打过 ERROR,保持「一条 ERROR 对应一次计数」)。
9. **代码评审后的修订**(都已并入 §4.3 正文):取下新增第 3a 步(宝石的 Item 行不存在或是装备 → 在纯校验段拒);第 4 步的静默预检改成精确的
   「有没有还塞得下的堆」(`HasStackWithRoom`,不动 `modules/bag`);`gem_config_id == 0` 的记录四处统一视同不存在;取下第 6 步按
   「退回的那一条 / 其余」区分日志;`EquipGemSocket` 的不连续计数改成「缺口数 > 越界行数才报」,真正做到同一根因只计一次。

### 10.5 C++ 用例(§7)

1. 「强化未开放 / 下调上限 / 等级中间缺一行」三类场景用 `const_cast` 直接摘 `EquipEnhanceTableManager` 的主键索引(`EditableEnhanceIndex`),
   不是 `Editable` 改行:`MaxEnhanceLevel()` 与取行走的都是主键索引,原地改行上的 id 骗不过它们。`TearDown` 重新 `Load()` 复原。
2. 夹具的金币由 `SetGold` 直接写 `CurrencyComp.values`(`clear_values + add_values`),不走 `CurrencySystem::AddCurrency`
   (那条路要过欠款抵扣、封禁闸与异常获得统计)。扣金币仍走被测代码的正式入口。
3. `PlayerEquipAttributeTest` 的「面板推送发生」靠数一条 WARN 来观察(夹具玩家没有会话组件,`PushPanel` 恰好留下那一条),用到探针的用例
   开头都有一次校准。scene 侧没有「推面板」的可测接缝,这是已知的覆盖方式。它的夹具现在在 `SetUp` 投一段 item 号段(取下要铸号)。
4. 「嵌 → 取」往返后,六项逐位回到原值;当前气血在残血时断言为「不高于原值、且至多少 1 点」(`RescaleCurrent` 两次向下取整)。
   「无净收益」成立,「恰好回到原值」对残血不成立。
5. 没有用例覆盖的契约行为:三张表全空时的校验;下一级缺行 → `kEquipInternalError`;提交段三个带标签的失败分支;实例 `size != 1`;
   脏表同号孔位只下发第一行。前四项从公开接口稳定造不出来。
6. 评审后补强:三处图标 / 名称断言原先两边都是空串(发货表没配图标,夹具物品没有名字),现在先在内存里补值再断言非空;
   新增 `GemRecordWithConfigZeroCountsAsAnEmptySocketAndHealsOnTheNextInlay`;`EnhanceTreatsAnUnpayableGoldCostAsNotEnoughGold` 的注释
   改成只声称它真正验到的对外行为。

### 10.6 robot 冒烟(§7)

1. `relogin` 没有复用一期的:`equipSmokeSession.relogin` 内部把账号写死成 `robot_9103`。新写了 `openEquipForgeSession` / `equipForgeRelogin`
   (约 20 行,只差账号),其余(`call / getBag / gmGrant / throttle / equip / unequip / panelAfter …`)全部复用。
2. 补金币 / 强化石放在每次强化之前(按当时快照里的 `enhance_next` 算缺口),不在预备段;预备段只保证有 1 颗 2101。
3. 多了两条预备断言:人物背包里已有 2103 → 判红(`kEquipGemNotOwned` 靠「从不发放 2103」来造);包里已有 1101 但 `enhance_max_level = 0`
   → 判红且**不再发新装备**(强化未开放时反复重跑不会把这个删不掉东西的账号越跑越脏)。
4. 「背包里强化」恰好把目标推到 +12 的那一轮,「穿上后再强化」改为断言 `kEquipEnhanceMaxLevel` 且两个包不变。
5. 在 §7 列出的断言之外,补了几条由 §3.3 / §4.3 直接推出的断言(成功后 `value == 预览的 enhance_next_value`;面板「恰好」只变物伤 / 法伤;
   三个操作前后目标不换包不挪格;镶嵌后孔位 `attr` 与背包里那颗宝石自报的 `gem_attr` 一致;等等)。对账只比金币这一个数,
   其它币种、欠款、封禁列表不在断言范围。
6. 冒烟镜像的表值集中在 `equip_forge_smoke_scenario.go` 顶部(1101 的伤害表值 40、上限 12、武器两孔类型 1 / 0、2101 / 2201 / 2103 / 2001、
   GM 单次上限 99):策划改这些表必须同步改。

### 10.7 客户端(§6)

1. `NowMs` 是公开可写属性(测试程序集没有 `InternalsVisibleTo`);传 null 恢复默认单调时钟。
2. `ForgeMinIntervalMs` 落地为 **700**(评审指出 500 在默认限流档下持续连点仍会每 4 个包被拒 1 个)。
3. 重拉分类比 §6.1 初稿细:金币 / 材料不足按「服务器给的码是否等于发包时 `CheckEnhance` 的结论」分流;`KEquipEnhanceUnavailable` 同理
   (本地本来就没有预览 → 不重拉,否则每点一次都白拉两份一模一样的快照)。`CheckEnhance` 的检查顺序固定为:未开放 → 满级 → 无预览 → 材料 → 金币。
4. `LastEnhanceOutcome` 的「操作开始」取的是「通过了静默丢弃的闸(在途、发包间隔)之后」:随后被互斥挡下或被本地拒绝的那一次也清空。
5. 取下的本地拒绝只拦「快照里列着这个孔、而且它是空的」;快照里没列出来的孔位号不拦(策划删孔后宝石仍要能按孔位号取下)。
6. 契约没给的几句文案:四句空参数提示;三个新操作回包缺快照时是「服务器未返回最新的背包与装备栏」;本地拒绝的原因复用对应 tip 码的文案。
7. `PlayerBag.cs` 的差异除新增的消息 / 字段外,还有两处文档注释变化(服务器 proto 在基线上已有、客户端上次生成时还没有的改动)。
8. 数据层通过别名引用了 `UI.Ugui.Gameplay` 里的 `EquipBagModel` / `EquipEnhanceCheck`(纯逻辑,同一程序集);以后若拆程序集,这两个类型要跟着数据层走。
9. 没改的:三处既有文案「装备正在更换，请稍候」与界面的「正在更换装备…」对强化 / 镶嵌不贴切(穿脱路径的既有文案,界面文件归 Codex)。

### 10.8 评审与验证记录（首次实现阶段，截至 2026-10-10）

- **设计评审**(契约落码前,三个只读视角:服务器可行性 / 资产一致性 / 协议与客户端):30 条发现,采纳的已并入 §2–§9 正文
  (工程登记、发号前置、Tip 追加的两条路径、表校验计数口径、占用与生效两个口径、材料 / 宝石被配成装备的运行期兜底、
  `RemoveItemsExact` 的空表与 0 数量、`ScaleByPermyriad` 的算法与锚点、未初始化实例的拒绝形态、流水 `extra`、读路径静默、
  强化面板缺的字段、孔位显示名、重拉分类、强化结果的发布顺序、限流与发包间隔、客户端落点、逐字格式表、robot 冒烟设计)。
- **代码评审**(落码后,八个只读视角):编译可行性视角对产品代码逐符号核对(对着沙盒导表生成物与主仓头文件)**零发现**;
  其余视角共 14 条,全部是 minor,处置如下 ——
  已修:取下的日志时机、精确的背包满预检、`gem_config_id == 0` 死孔位、`EquipGemSocket` 计数自洽(以上四条见 §10.4 第 9 条);
  三处空转断言与一条注释(§10.5 第 6 条);robot 强化未开放时不再累积装备、对账注释与代码一致(§10.6);
  客户端 `KEquipEnhanceUnavailable` 的无限重拉与发包间隔(§10.7 第 2、3 条);文档里过时的数字与发号现状(本节与 §9)。
  只登记不改:表文件缺失时测试进程 abort(§9.2 的硬前置);`EnhanceTreatsAnUnpayableGoldCost…` 验不到那道显式上限闸(§10.5 第 6 条)。
- **实际跑过的**:补丁脚本八个子命令(真跑三个新的,`--dry-run` 全部八个);沙盒导表一次;robot 的 `gofmt -l -e`;
  客户端离线编译 + NUnit 244 / 244 + 变异测试;若干只读的静态自查脚本(括号配平、用例重名)。
  **该阶段尚未执行的**：服务器编译、C++ / Go 测试、正式导表、proto-gen、起栈冒烟、Unity。后续 2026-10-13 的正式生成、编译、单测与冒烟证据见 §11；当前合并后的剩余验证见 §9，不能继续沿用此前的未生成结论。

## 11. 主干验证记录(2026-10-13)

本节保留 2026-10-13 的历史运行记录，所述源码、产物、问题和客户端状态均以当日为准，不表示当前合并后的代码已通过复验。

用户 10-13 当面授权 Claude 在**装备这条线**上做「生成 → 并入 main → 串行编译 → 改编译错误 → 跑单测与 robot 冒烟」,
这是 AGENTS §10.1 的一次性例外,范围只到装备线。本节只写**实际跑出来的结果**;没跑的在 §11.6 逐条列出。
证据目录:`E:\work\output\equip-forge-20261013\`(Claude 跑的 C++ / robot 测试日志)、
`E:\work\output\team-follow-20261010\claude-build\run1`、`run2`(C++ 编译,每个工程一份日志 + `results.json`)。

### 11.1 并入与正式生成(Codex 做的,Claude 核对)

- 仓库历史在 10-12 前后被整体改写(提交号全变,另有一次把约 200 个文件的日志字符串英文化)。Codex 把二期的提交重放进 main:
  `6e212616bf`。Claude 把它与原提交逐文件比过:除日志字符串外的行一致,逻辑没有被改动。
- 正式导表(两遍)+ 全量 proto-gen:`0b40715d06`。Claude 在 main 上核对到的结果 ——
  消息号 `SceneBagClientPlayerInlayGem = 250`、`EnhanceEquip = 251`、`RemoveGem = 252`(一期的 246 / 247 / 249 不变);
  tip `kEquipEnhanceUnavailable = 28011` … `kEquipGemBagFull = 28021`,共 11 个,紧跟 `kEquipInternalError = 28010`;
  `equipenhance.json` 12 行、`equipgem.json` 12 行、`equipgemsocket.json` 6 行、`item.json` 61 行;限流表已含三个新方法(10 / 5 / 5)。
- robot 的 vendor 落后于生成物、按默认的 `-mod=vendor` 编不过:`1e3f9e1f68` 只做了 `go mod vendor` 同步。
- 作废的东西:Claude 在隔离工作树里另跑过一次导表与 proto-gen(那一次的号序是 250 = Enhance、251 = Inlay,与 main 不同),
  没有也不会进 main。隔离工作树 `xuanming-server-mmo-equip` 的分支停在旧历史上,**不要合并**。
- 客户端 main `ba60401f` 已合入数据层分支并带上正式的 C# 协议(`MessageIds.cs` 的三个号与上面一致、`EquipErrorTip.cs` 有 11 个新枚举名),
  §6.5 / §9.5 说的 19 处预期 CS0117 已经消失。Claude 在客户端 main `ba60401f` 上做了离线验证(不是 Unity):
  `tools/client_compile_check.ps1` 运行时程序集 429 个文件 0 error;同一套 Roslyn 口径下 Battle 测试程序集 32 个文件 0 error;
  用 Unity 自带的 NUnit 跑装备相关五个用例类 **244 / 244**(`EquipForgeClientTests` 95、`EquipForgeModelTests` 35、`EquipClientTests` 70、
  `EquipBagModelTests` 13、`PlayerFeaturesClientTests` 31)—— 这一次用的是正式生成的消息号与 tip 枚举,不再是占位。
  证据 `E:\work\output\equip-forge-20261013\client-offline\`。

### 11.2 C++ 编译

Debug x64、`VCToolsVersion=14.51.36231`、MSBuild `/m:1 /nr:false`。主仓同一时间只能有一条编译链,所以由「组队跟随」那个会话统一执行
(两条线要的是同一批节点),Claude 只单独重编过 `bag_test.vcxproj`。

| 轮次 | 工程 | 结果 |
|---|---|---|
| run1 | table → proto → core → rpc → grpc_client → modules → battle 库 → scene 库 → gate 节点 | 全部 Exit 0;`modules` / `battle` / `scene` 库在警告即错误下 0 warning |
| run1 | scene 节点 | **链接失败**:21 个 `agones::*` 的 LNK 错误。原因是 `lib\infra.lib` 还是 10-01 的旧产物,编译链里漏了 `infra.vcxproj`;与装备无关 |
| run2 | third_party、muduo、proto_helpers、config、infra、thread_context、session、gate 库、gate / scene / battle 三个节点、`aoi_test` / `bag_test` / `turn_battle_engine_test` / `currency_test` / `cross_zone_test` | 16 个工程全部 Exit 0 |

当时 §9.3 列的「最可能先出错的位置」一条都没有发生:装备的产品代码(`player_equip.cpp`、`equip_attribute_rules.h`、`bag_service.cpp`、
`player_bag_handler.cpp`)与三个大测试文件都是**首次编译即 0 error**,没有为了编过而改任何一行;6 条 `static_assert` 全部成立。

### 11.3 C++ 单测(Claude 跑的,二进制是 run2 的产物)

| exe | 过滤器 | 结果 |
|---|---|---|
| `bag_test.exe` | `PlayerEquipTest.*` | 151 / 151 |
| 同上 | `PlayerEquipAttributeTest.*` | 37 / 37 |
| 同上 | `BagInstanceDataTest.*` | 31 / 31 |
| 同上 | `BagRemoveExactTest.*` | 10 / 10 |
| 同上 | `PlayerFeaturePersistenceTest.EquipInstanceDataRoundTripsThroughDatabaseRecord` | 1 / 1 |
| 同上 | 不带过滤 | 第一次 482 / 488,修了一处测试夹具后 **488 / 488**(见 §11.5) |
| `turn_battle_engine_test.exe` | 不带过滤 | 232 / 232(其中 `Equip*` 56、`TurnBattleEngineTest` 103) |
| `currency_test.exe` | `ClientGmGateTest.*` | 6 / 6 |
| 同上 | 不带过滤 | 144 条里 140 过、3 跳过(要真 Redis / 既有跳过)、**1 失败**:`AssetOpSystemTest.DebitBundleInvalidRecorded`,不属于装备,见 §11.6 |
| `cross_zone_test.exe` | 不带过滤 | 166 / 166(其中 `CrossZoneBagMarshal.*` 6 / 6) |

### 11.4 Go / robot(Claude 跑的)

工作目录 `robot\`,Go 1.26.5,`-mod=vendor`:`go build` 与 `go vet . ./logic/handler` 退出码 0;
`go test -run "TestEquip|TestFeature" . ./logic/handler ./logic/gameobject` 三个包全是 `ok`;`-v` 口径下 253 条 `RUN`、253 条 `PASS`、0 条 `FAIL`
(顶层 `TestEquip*` 32 个,其余是子用例与 `TestFeature*`)。`robot\robot.exe` 是这一批源码编出来的。
Codex 在 `0b40715d06` 上独立跑过同一组,也是 253 通过(见 `docs/PROGRESS.md` 10-13 Codex 那一条)。

### 11.5 顺手修掉的一处测试顺序依赖(`697f5295b1`)

`bag_test.cpp` 里的 `BagSegmentNotReadyTest` 会把物品号段置成「未就绪」,并假定自己最后执行;gtest 按翻译单元排,
`bag_remove_by_guid_test.cpp` 的 7 条用例排在它后面,6 条因为拿不到号段而失败(单独跑是 7 / 7)。
修法只动这一个测试文件:7 个 `TEST` 改 `TEST_F`,夹具 `SetUp` 里重置并投一段物品号段。产品代码没动。
同类的顺序依赖在别的用例组里还有(带 `--gtest_shuffle` 时,种子 7919 / 15838 / 4242 下分别失败 37 / 8 / 7 条,
落在 `BagTest`、`GainBlockServiceTest`、`CompactPolicyTest`、`BagServiceSortTest`、`BagLayoutSwapTest`、`BagPlacementFailureTest`),
**没有处理**;装备相关的用例组在这三个种子下都没有失败。

### 11.6 还没验证的 / 不归本线的

- 三条新日志(`[PlayerEquip] 强化:` / `镶嵌:` / `取下:`)在 main 上仍是中文,而仓库其余日志已在 10-13 统一成英文;不影响功能,
  下次动 `player_equip.cpp` 时顺手改(改了要重编 scene,所以这次没有为它单独动)。
- **客户端**:没有在 Unity 里编过、跑过(EditMode 用例、窗口级用例、实机都没有);证据只有 §11.1 那次离线的 Roslyn + NUnit。
  (10-16 更新:已在真 Unity 6000.6.0f1 里跑过 EditMode —— 副本工程 batchmode,客户端 main `db969ce4`,装备相关八个用例类全过(含真搭 uGUI 的 `EquipWindowTests` 29/29 与新面板逻辑 `EquipForgeUiStateTests` 63/63);同批另有 67 条美术资源用例失败,退回改动前同样失败,属既有问题。仍没有实机点过。)
  强化 / 镶嵌的**界面入口没有做**(按分工是 Codex 的文件,接线说明在客户端 `Docs/EquipmentForge-20261009.md`)——
  所以现在进游戏点不到强化与镶嵌,只能靠 robot 冒烟走通这三条协议。
  (10-16 更新:**按钮逻辑已经接好**,在客户端 main `db969ce4` 的 `EquipForgeUiState`;入口与面板画面交给 Codex,交接文档是客户端
  `Docs/EquipmentForgeUI-handoff-20261015.md`。在 Codex 接上界面之前,进游戏仍然点不到。见 `docs/PROGRESS.md` 10-16 那一条。)
  实机点强化 / 镶嵌之前先补货:`robot\` 下 `.\robot.exe -c etc/equip_forge_topup.yaml`(模式 `equip-forge-topup`,
  缺省给 `robot_9104` 补 40 万银两、60 颗强化石、四种一级宝石各 3 颗,够把一把装备从 +0 强化到满级;只发东西,发完核对到账)。
- `AssetOpSystemTest.DebitBundleInvalidRecorded`:期望 `REJECTED` + `kAssetInvalidBundle`,实际 `RETRY` + 27006 ——
  `asset_op_system.cpp` 在校验 bundle 之前先查了 `PlayerBagsComp`。单独跑也失败,属于通用资产通道(帮会 / 聚宝斋)那条线,本线只登记、没有改。
- §8 的已知缺口不变:强化石、宝石、装备本身都没有产出途径(只能 GM 发);洗练、宝石合成、强化保护没做。
- **Java 版(AGENTS §12)**:待做。

### 11.7 起栈冒烟(Claude 跑的,全部通过)

栈由「组队跟随」会话用正式启动器 `tools/scripts/start_game.ps1` 从主仓起(一区;启动器 11:22:42 报 Server started,
日志 `run/logs/game-launcher/20261013-111614-565/launcher.log`):`bin\gate.exe / scene.exe / battle.exe` 是 run2 的产物(源码 `07f9ec3ca9`),
`bin\go_services` 的 11 个 Go 服务与 Java 网关同日重编。它自己的基线先过了:login-test 23 / 23、`TEAM_SMOKE_OK`。
scene 启动日志:`equipment table validation issues=0`;scene 与 battle 的战斗配表指纹相同(`34a72b7b…`)。

在同一套栈上(工作目录 `robot\`,`robot.exe` 是 §11.4 那一份),三次运行退出码都是 0,日志在 `E:\work\output\equip-forge-20261013\smoke\`:

| 运行 | 结果行 |
|---|---|
| `etc/equip_smoke.yaml`(`robot_9103`) | `EQUIP_SMOKE_OK player=2608 steps=9 … affixes=3/3/1 granted=4 physical_attack=50->201 defense=60->72 speed=276->288` |
| `etc/equip_forge_smoke.yaml` 第 1 遍(`robot_9104`,全新账号) | `EQUIP_FORGE_SMOKE_OK player=2615 steps=6 … granted_weapon=true enhance_level=0->1->2 enhance_in_bag=success enhance_worn=success gem=2101 value=30 physical_attack=102->132->102` |
| 同上第 2 遍 | `EQUIP_FORGE_SMOKE_OK … granted_weapon=false enhance_level=2->3->4 enhance_in_bag=success enhance_worn=success gem=2101 value=30 physical_attack=106->136->106` |

与 §9.4 的通过标准逐项对上:第 1 遍 `granted_weapon=true`、`0->1->2`、两次必成;第 2 遍复用同一把(`target` 相同)、`2->3->4`
(第 4 级成功率 9000,这次掷成了);两遍的 `physical_attack` 第一段与第三段相等(取下宝石后回到镶嵌前)。
第 1 遍的分步日志还给出了:7 条拒绝用例全中、强化后基础物攻 40 → 42、穿着时强化面板 100 → 102、嵌入攻击宝石(+30)后 102 → 132、
**重登之后强化等级 2 与宝石仍在**(面板仍是 132)。
scene 侧同时段的 `[PlayerEquip]` 日志与之吻合:4 次强化(扣金币 1000 / 2000 / 3000 / 5000,强化石 1 / 1 / 2 / 2)、2 次镶嵌、2 次取下;
没有 `[ENHANCE_PARTIAL]` / `[INSTANCE_LOST]` / `[GEM_LOST]` / `Table validation` 的 ERROR(同时段仅有的 ERROR 是别的机器人放技能的
`kSkillUnInterruptible`,与装备无关);gate 与 battle 日志 0 条 ERROR。

没有覆盖到的:概率失败那条分支(4 级以后才可能失败,这两遍都掷成了,失败分支只有单测覆盖);真实客户端(界面入口没做,见 §11.6)。

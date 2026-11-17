// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Item.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  模块依赖:这个头只要 Core + Engine(Engine/DataTable.h)。
//  整套产物要加的模块见 ConfigSubsystem.h 顶部的「Build.cs」一节 —— 那里有可以
//  直接抄进 Build.cs 的一行。
//
//  形态说明(为什么不是 UDataTable):
//    1. UDataTable 的填表 API 是 WITH_EDITOR-only,shipping 包里没有「再 Load 一次」
//       这个动作,而本导表器五门语言的产物全部建立在「Load() 整批建新快照再原子换掉
//       旧的」之上 —— 用 UDataTable 等于让 UE 这一门独家丢掉热更。
//    2. UDataTable 行名唯一,与本导表器的「主键可重复」(table.multi_primary_key)
//       结构性冲突。
//  继承 FTableRowBase 是零成本的:把「将来想丢进 UDataTable 看一眼」的门留着,
//  但运行时一行代码都不依赖它。
//
//  字段名口径(**这是整套 USTRUCT 的命门**):
//    generated/tables/item.json 不是 protojson,是导表器自己 json.dumps 出来的,
//    键就是 proto 字段名本身 —— **snake_case**。
//    FJsonObjectConverter 用 `JsonAttributes.Find(Property->GetName())` 找值,
//    TMap<FString,...> 的比较/哈希大小写不敏感,所以匹配是「忽略大小写的逐字符相等」,
//    **它不做 snake_case <-> camelCase 转换**。因此下面每个 UPROPERTY 的名字都
//    刻意写成与 JSON 键逐字符相同的 snake_case。改成 UE 惯例的 PascalCase 的那一刻,
//    FJsonObjectConverter 会**静默跳过**这一列(找不到的字段不报错),整张表变成默认值。
//
//  UPROPERTY 说明符口径:
//    - Blueprint 能安全承接的列写 `UPROPERTY(BlueprintReadOnly, Category = ...)`;
//    - 其余列写**光板 `UPROPERTY()`**,不写 Category。光有 Category 而没有任何
//      Edit*/Visible*/BlueprintRead* 说明符,UHT 会对每个这种字段报一条警告
//      ("Property has a Category but is not editable or blueprint visible"),
//      21 张表能刷出一屏。光板 UPROPERTY() 一样有反射,
//      FJsonObjectConverter 照样认得 —— 装载不受影响。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"
#include "Engine/DataTable.h"

#include "CfgItemRow.generated.h"

/// Item.base_attr 的子消息行。
USTRUCT(BlueprintType)
struct FCfgItemBaseAttr
{
	GENERATED_BODY()

	/** 基础属性:每槽 (EquipAttribute.id, 数值),最多 3 条;用不到的槽留空,不要填 0 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 base_attr_id = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 base_attr_value = 0;
};

/// Item 的一行。字段名与 item.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:ItemTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgItemRow : public FTableRowBase
{
	GENERATED_BODY()

	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 id = 0;

	/** 最大堆叠数量 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 max_stack_size = 0;

	/** 装备部位:0=不是装备。>0=该部位编号。部位的含义(手镯/头/胸…)以及每个部位有几个槽,全部由数据定义 —— 槽位表 EquipSlot 决定哪个槽接受哪个部位,代码里不写死 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 equip_kind = 0;

	/** 回合制战斗内可用:0=不可用,1=可用。只有它非 0 的物品才会进 BattlePlayerSnapshot.items 消耗副本,引擎也按它拒绝 ITEM 行动 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 battle_usable = 0;

	/** 战斗内使用回复的生命值(0=不回血)。按当前属性单位(血量千级)填 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int64 battle_heal_hp = 0;

	/** 战斗内使用回复的法力值(0=不回蓝)。按当前属性单位(法力 ×4)填 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int64 battle_heal_mp = 0;

	/** 物品名(下发客户端) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	FString name;

	/** 描述(下发客户端 tooltip) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	FString description;

	/** 图标 Resources 路径(UI/ 开头,不带扩展名);空 = 客户端兜底图标 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	FString icon_key;

	/** 佩戴等级,同时是查属性上限(EquipAttributeCap)用的装备等级 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 equip_level = 0;

	/** 职业要求(Class.id);0 = 不限 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 equip_class = 0;

	/** 随机属性池(EquipAffixPool.pool_id,不是那张表的主键,所以不设外键);0 = 不掷 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 affix_pool = 0;

	/** 掷值规则。FK -> EquipAffixRule.id;0 = 不掷 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 affix_rule = 0;

	/** 装备规则版本：0 保留旧随机属性及强化规则，1 首发基础装，2 首发首饰，3 首发法宝。 同一模板的版本不修改；新增版本使用新物品 id，避免静默改变已有实例。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 equip_profile = 0;

	/** 定向打造材料。0 表示本模板未开放打造；材料数量必须同时为 0。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 forge_material_id = 0;

	/** 打造一个本体所需数量；不含后续首次安装词条的费用。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 forge_material_count = 0;

	/** 固定法宝效果。0 表示不是法宝；两件同名法宝仍是两个独立实例。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	int32 artifact_id = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Item")
	TArray<FCfgItemBaseAttr> base_attr;
};

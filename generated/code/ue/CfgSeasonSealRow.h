// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 SeasonSeal.xlsx 生成。**不要手改**。
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
//    generated/tables/seasonseal.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgSeasonSealRow.generated.h"

/// SeasonSeal 的一行。字段名与 seasonseal.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:SeasonSealTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgSeasonSealRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 赛季印唯一 ID（如 101..104） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	int32 id = 0;

	/** 所属赛季 ID（FK -> SeasonRule.id） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	int32 season_id = 0;

	/** 展示名称 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	FString name;

	/** 效果说明 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	FString description;

	/** 客户端图标键（不带 Assets/Resources 前缀与扩展名） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	FString icon_key;

	/** 复用的轻量战斗机制种类（同 EquipArtifact effect_id：1 物理削盾、2 法术削盾、6 开局减伤、7 防御回蓝等） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	int32 effect_id = 0;

	/** 增效、削盾、减伤或回蓝比例（万分比整数） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	int32 bonus_bps = 0;

	/** 以入战最大气血为基准的减伤上限（万分比；0 表示无此限制） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	int32 cap_bps = 0;

	/** 每回合触发次数上限（0 不限） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	int32 per_round_limit = 0;

	/** 每场战斗触发次数上限（0 不限） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	int32 battle_limit = 0;

	/** 仅在前几回合可触发（0 不限） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	int32 active_rounds = 0;

	/** 触发治疗增幅时的气血比例上限（万分比；0 无此条件） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonSeal")
	int32 health_threshold_bps = 0;
};

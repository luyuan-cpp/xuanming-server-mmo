// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 SeasonRule.xlsx 生成。**不要手改**。
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
//    generated/tables/seasonrule.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgSeasonRuleRow.generated.h"

/// SeasonRule 的一行。字段名与 seasonrule.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:SeasonRuleTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgSeasonRuleRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 赛季编号（从 1 起递增，对应 Skill.season_id 与 SeasonSeal.season_id） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int32 id = 0;

	/** 赛季展示名（如 破阵之季） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	FString name;

	/** 赛季开始时刻（UTC 毫秒，左闭；0 表示立即可用） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int64 starts_at_ms = 0;

	/** 赛季结束时刻（UTC 毫秒，右开；0 表示首测未设截止时刻） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int64 ends_at_ms = 0;

	/** 是否启用（同一时刻最多允许 1 条启用且在有效期内的赛季） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	bool enabled = false;

	/** 赛季印满级所需总点数（首测 = 12） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int32 seal_max_points = 0;

	/** 合格正规 PVE 胜利获得的赛季印点数（首测 = 1） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int32 pve_win_points = 0;

	/** 合格正规排位 PVP 胜利获得的赛季印点数（首测 = 2） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int32 pvp_win_points = 0;

	/** 合格正规排位 PVP 平/负获得的赛季印点数（首测 = 1） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int32 pvp_other_points = 0;

	/** 赛季印满级气血万分比上限（首测 = 300 即 3%，服务端硬上限 <= 500） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int32 max_health_bps_cap = 0;

	/** 赛季印满级防御万分比上限（首测 = 300 即 3%，服务端硬上限 <= 500；禁加速度/控命/抵抗） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int32 defense_bps_cap = 0;

	/** 快捷追赶扣除的货币类型（CurrencyType：0 银两 / 1 灵石 / 2 绑定灵石） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int32 catchup_currency_type = 0;

	/** 每补 1 点进度的货币单价（0 = 暂不开放快捷追赶；首测 = 10 灵石） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int64 catchup_price_per_point = 0;

	/** 本季默认佩戴的赛季印 ID（FK -> SeasonSeal.id） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	int32 default_seal_id = 0;

	/** 赛季简介说明 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SeasonRule")
	FString desc;
};

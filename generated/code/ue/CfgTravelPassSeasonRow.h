// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 TravelPassSeason.xlsx 生成。**不要手改**。
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
//    generated/tables/travelpassseason.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgTravelPassSeasonRow.generated.h"

/// TravelPassSeason 的一行。字段名与 travelpassseason.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:TravelPassSeasonTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgTravelPassSeasonRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 期次 ID（上线后只停用不复用旧 ID） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int32 id = 0;

	/** 启用开关：0 = 停用，1 = 启用（全表同一时段至多 1 条启用期次） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int32 enabled = 0;

	/** 期次展示名（如「青云游历手记 · 第一期」） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	FString name;

	/** 期次说明（写明不强制、无日清、先玩后买、解锁永不过期） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	FString description;

	/** 活动中心卡片图标资源键 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	FString icon_key;

	/** 游历手记顶部画卷横幅资源键 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	FString banner_asset_key;

	/** 开始时间（UTC 毫秒，0 表示无起点限制） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int64 starts_at_ms = 0;

	/** 结束时间（UTC 毫秒，0 表示常驻不限时；非 0 时已解锁典藏者永不过期） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int64 ends_at_ms = 0;

	/** 满级总游历印点数（如 300） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int32 max_points = 0;

	/** 合格普通 PVE / 瑞兽迎春 / 帮会胜局加点（如 20） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int32 pve_win_points = 0;

	/** 合格困难首领试炼 / 正式 PVP 胜局加点（如 30） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int32 hard_or_pvp_win_points = 0;

	/** 合格达标 PVP 平局或负局加点（如 15） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int32 pvp_qualified_other_points = 0;

	/** 解锁典藏轨结算币种：CurrencyType（首期统一填 1 = 真实充值灵石，严禁绑定灵石抵扣） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int32 premium_currency_type = 0;

	/** 解锁典藏轨单价（1..1000000000，如 180 灵石 = ¥18） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int64 premium_price = 0;

	/** 配置修订号（改价或调整关键配置时递增，用于报价防呆） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TravelPassSeason")
	int32 config_revision = 0;
};

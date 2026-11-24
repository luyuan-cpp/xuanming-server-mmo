// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 TrialJournalEntry.xlsx 生成。**不要手改**。
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
//    generated/tables/trialjournalentry.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgTrialJournalEntryRow.generated.h"

/// TrialJournalEntry 的一行。字段名与 trialjournalentry.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:TrialJournalEntryTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgTrialJournalEntryRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 关卡格稳定业务 ID（101..104 卷一破阵试炼；201..203 预留卷二三境明雷；301.. 预留卷三季游） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	int32 id = 0;

	/** 所属卷编号（1=卷一·破阵试炼，2=卷二·三境明雷，3=卷三·季游奇谈） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	int32 chapter_id = 0;

	/** 所属赛季编号（0=永久卷，>0=赛季隔离卷，遵循 bit-storage-and-table-ids.md §1） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	int32 season_id = 0;

	/** 玩法来源类型（1=常驻首领试炼，2=野外明雷首领，3=轮换PVE，4=轮换PVP，5=人宠会武） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	int32 source_kind = 0;

	/** 难度档（0=不区分，1=普通，2=困难） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	int32 difficulty = 0;

	/** 护卫或机制变体键（如 rejuvenation / bulwark / wind_eye / water_mirror / tide） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	FString variant_key;

	/** 当前可比规则版本（平衡大改时显式递增，避免旧版成绩与新版混比） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	int32 rule_version = 0;

	/** 当前规则版本展示名（如 首测规则） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	FString rule_version_name;

	/** 是否在当前目录生效（下架置 false，保留旧位与旧档兼容） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	bool enabled = false;

	/** 同卷内展示排序（升序） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	int32 sort_order = 0;

	/** 关卡格展示名（如 普通·回春） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	FString display_name;

	/** 卷名称（如 卷一 · 破阵试炼） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	FString chapter_name;

	/** 客户端图标键（不带 Assets/Resources 前缀与扩展名） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	FString icon_key;

	/** 准入模式（0=基础战斗准入直接开放，1=任意普通破阵格通关印点亮后解锁困难） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	int32 unlock_mode = 0;

	/** 客户端「前往试炼」跳转参数（如 boss_trial:normal:rejuvenation） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	FString jump_target;

	/** 关卡机制简介 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalEntry")
	FString desc;
};

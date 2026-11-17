// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 EquipArtifact.xlsx 生成。**不要手改**。
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
//    generated/tables/equipartifact.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgEquipArtifactRow.generated.h"

/// EquipArtifact 的一行。字段名与 equipartifact.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:EquipArtifactTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgEquipArtifactRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 固定效果种类：1 破阵、2 照影、3 回响、4 济生、5 固元、6 护心、7 归元、8 清心。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|EquipArtifact")
	int32 id = 0;

	/** 增幅、削盾、减伤、回复或概率修正，万分比整数。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|EquipArtifact")
	int32 bonus_bps = 0;

	/** 以入战最大气血为基准的减伤上限，万分比；0 表示无此项限制。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|EquipArtifact")
	int32 cap_bps = 0;

	/** 每件实例每回合的次数限制；0 表示不另设此项上限。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|EquipArtifact")
	int32 per_round_limit = 0;

	/** 每件实例整场次数限制；0 表示不另设此项上限。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|EquipArtifact")
	int32 battle_limit = 0;

	/** 只在前几个全局回合可触发；0 表示没有回合窗口限制。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|EquipArtifact")
	int32 active_rounds = 0;

	/** 触发治疗增幅时的当前气血比例上限，万分比；0 表示没有此条件。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|EquipArtifact")
	int32 health_threshold_bps = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|EquipArtifact")
	FString name;

	UPROPERTY(BlueprintReadOnly, Category = "Config|EquipArtifact")
	FString description;
};

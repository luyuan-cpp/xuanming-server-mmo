// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 FestivalBeastActivity.xlsx 生成。**不要手改**。
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
//    generated/tables/festivalbeastactivity.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgFestivalBeastActivityRow.generated.h"

/// FestivalBeastActivity 的一行。字段名与 festivalbeastactivity.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:FestivalBeastActivityTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgFestivalBeastActivityRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 活动期次实例 ID（如 202601） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 id = 0;

	/** 运营总开关（默认 false，未排期关闭） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	bool enabled = false;

	/** 配置版本号（每次运营修改期次规则递增，发起挑战原样校验） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 config_revision = 0;

	/** 活动展示名（默认“瑞兽迎春”） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	FString activity_name;

	/** 首领/神兽工作名（默认“春禧兽”） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	FString boss_name;

	/** 活动横幅资源引用键 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	FString banner_asset_key;

	/** 活动图标资源引用键 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	FString icon_key;

	/** 开放开始 UTC 毫秒（含） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int64 effective_start_at_ms = 0;

	/** 开放结束 UTC 毫秒（不含；正式期次须为 14 个完整 24 小时） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int64 effective_end_at_ms = 0;

	/** 结束后集中领奖展示天数（默认 7 天） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 claim_showcase_days = 0;

	/** 关联战斗副本配置 ID（Dungeon.id = 111，boss_trial_kind = 7） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 battle_config_id = 0;

	/** 本期累计领奖所需胜场（默认 3） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 required_wins = 0;

	/** 稳定奖励身份（复刻复用，同一角色终身最多领取 1 只） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	FString reward_identity;

	/** 奖励定义版本（达成永久资格时冻结，后续改表不影响已达成权益） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 reward_definition_version = 0;

	/** 固定奖励神兽种类 ID（Pet.id = 101） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 reward_pet_table_id = 0;

	/** 回合上限（默认 12） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 round_limit = 0;

	/** 首领行动循环周期（默认 3） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 cycle_rounds = 0;

	/** 首次重击回合（默认 3，即 R3/6/9/12） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 first_heavy_round = 0;

	/** 预告提前回合数（默认 1，即 R2/5/8/11 预告下一轮重击目标） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 telegraph_lead_rounds = 0;

	/** 重击基础伤害倍率百分比（默认 180） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|FestivalBeastActivity")
	int32 heavy_damage_percent = 0;
};

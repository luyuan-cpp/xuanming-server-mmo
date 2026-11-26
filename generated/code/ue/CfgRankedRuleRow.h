// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 RankedRule.xlsx 生成。**不要手改**。
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
//    generated/tables/rankedrule.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgRankedRuleRow.generated.h"

/// RankedRule 的一行。字段名与 rankedrule.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:RankedRuleTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgRankedRuleRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 规则行 id(固定 1) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 id = 0;

	/** 规则版本号(修改预设或排位参数时递增;组队确认与开局提交点校验它) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 rule_version = 0;

	/** 排位专用战斗模板(FK -> Dungeon.id,首测=5,time_limit=720) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 battle_config_id = 0;

	/** 竞技快照参考职业(FK -> Class.id,首测=1) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 reference_class_id = 0;

	/** 竞技快照统一等级(首测=30) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 competitive_level = 0;

	/** 默认人物预设(FK -> RankedPreset.id,首测=101 破阵) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 default_character_preset_id = 0;

	/** 默认宝宝预设(FK -> RankedPreset.id,首测=203 护主) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 default_pet_preset_id = 0;

	/** 单赛季持续天数(首测=56 即 8 周) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 season_duration_days = 0;

	/** 排队等待提示阈值秒数(首测=120) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 wait_prompt_seconds = 0;

	/** 分差窗口一档时长上限(秒,首测=30) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 spread_tier1_seconds = 0;

	/** 分差窗口一档最大分差(分,首测=100) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 spread_tier1_points = 0;

	/** 分差窗口二档时长上限(秒,首测=90) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 spread_tier2_seconds = 0;

	/** 分差窗口二档最大分差(分,首测=200) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 spread_tier2_points = 0;

	/** 分差窗口三档硬上限(分,首测=300,永不退化为不限) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 spread_tier3_points = 0;

	/** 头像纹样累计有效场次门槛(首测=4) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 milestone_avatar_matches = 0;

	/** 名片底纹累计有效场次门槛(首测=8) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 milestone_card_matches = 0;

	/** 永久青云玉符累计有效场次门槛(首测=12) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 milestone_talisman_matches = 0;

	/** 青锋段位下限(×100,V2=152000;新号1500起步落初试,净胜2场破冰升青锋) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 tier_qingfeng_centi = 0;

	/** 云章段位下限(×100,V2=160000;封榜可留存云章徽章与称号) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 tier_yunzhang_centi = 0;

	/** 天枢段位下限(×100,V2=172000) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 tier_tianshu_centi = 0;

	/** 无双段位下限(×100,V2=185000) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 tier_wushuang_centi = 0;

	/** 规则说明 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	FString desc;

	/** 本季累计胜场锋芒称号门槛(V2=6) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 milestone_win_matches = 0;

	/** 正式论剑每队战前技能禁选槽数（0=关闭，3=每队禁3招；纳入配置指纹） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 skill_ban_slots_per_team = 0;

	/** 正式论剑开启战前技能禁选的全员最低段位分门槛（×100；160000=云章起开，0=全段位开；纳入配置指纹） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RankedRule")
	int32 skill_ban_min_rating_centi = 0;
};

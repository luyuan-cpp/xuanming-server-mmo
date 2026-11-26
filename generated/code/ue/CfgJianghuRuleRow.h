// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 JianghuRule.xlsx 生成。**不要手改**。
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
//    generated/tables/jianghurule.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgJianghuRuleRow.generated.h"

/// JianghuRule 的一行。字段名与 jianghurule.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:JianghuRuleTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgJianghuRuleRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 规则行 ID（固定为 1） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 id = 0;

	/** 总开关：1 = 开放天墉生死状与江湖恩怨，0 = 关闭 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 enabled = 0;

	/** 普通江湖战书消耗银两数（CurrencyType = 0，默认 500） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int64 silver_writ_cost = 0;

	/** 官方金帖生死状道具 ID（FK -> Item.id，默认 3415） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 gold_writ_item_id = 0;

	/** 战书邀请有效期（秒，默认 60） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 invite_ttl_sec = 0;

	/** 目标小队全员确认时间窗（秒，默认 30） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 team_confirm_sec = 0;

	/** 同目标拒绝/过期后的再次下帖冷却（秒，默认 300） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 same_target_cooldown_sec = 0;

	/** 单角色每日未成局战书邀请次数上限（默认 20） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 daily_unmatched_invite_limit = 0;

	/** 天墉生死状单局回合上限（打满双方存活权威判平，默认 30） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 duel_round_limit = 0;

	/** 天墉生死状单局最长期限（秒，超时权威判平，默认 720） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 duel_timeout_sec = 0;

	/** 战败社交印记自然持续时长（秒，离线照常走表，默认 1800） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 defeat_mark_duration_sec = 0;

	/** 普通洗尘消耗银两数（默认 1000） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int64 silver_clear_cost = 0;

	/** 官方清风洗尘符道具 ID（FK -> Item.id，默认 3416） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 clear_item_id = 0;

	/** 清风洗尘符附赠洒脱徽记时长（秒，默认 1800） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 clear_badge_duration_sec = 0;

	/** 个人恩怨录最多保留活跃宿敌人数（默认 50） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 grudge_max_rivals = 0;

	/** 每对宿敌最多保留近期交手战报条数（默认 20） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 grudge_max_logs_per_rival = 0;

	/** 帮派恩怨墙最多保留近期对局记录条数（默认 20） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 guild_wall_max_records = 0;

	/** 触发宿敌连胜/终结连胜高光全服播报的最低连胜场次（默认 3） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|JianghuRule")
	int32 streak_highlight_threshold = 0;
};

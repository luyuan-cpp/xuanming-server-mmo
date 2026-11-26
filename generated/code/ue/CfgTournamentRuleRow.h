// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 TournamentRule.xlsx 生成。**不要手改**。
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
//    generated/tables/tournamentrule.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgTournamentRuleRow.generated.h"

/// TournamentRule 的一行。字段名与 tournamentrule.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:TournamentRuleTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgTournamentRuleRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 规则行 id(固定 1) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 id = 0;

	/** 规则版本号(修改核心参数时递增) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 rule_version = 0;

	/** 总开关(true=开放,false=暂停) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	bool enabled = false;

	/** 锦标赛专用战斗模板(FK -> Dungeon.id,首测=5,time_limit=720) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 battle_config_id = 0;

	/** 单场每侧出战人数(1..5,首测=3) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 battle_team_size = 0;

	/** 战队名册人数上限(battle_team_size..7,首测=4即3首发+1替补) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 roster_max_size = 0;

	/** 参赛最低角色等级(首测=15) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 min_level = 0;

	/** 报名阶段天数(首测=42) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 registration_days = 0;

	/** 海选积分赛天数(首测=7) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 qualifier_days = 0;

	/** 淘汰赛与总决赛天数(首测=2) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 knockout_days = 0;

	/** 封榜展示期天数(首测=5) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 settle_display_days = 0;

	/** 每支战队海选最多场次(首测=6) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 qualifier_max_matches = 0;

	/** 领取参赛保底奖所需完赛场次(首测=2) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 participation_min_matches = 0;

	/** 淘汰赛最大晋级队数(2/4/8/16,首测=16) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 knockout_max_teams = 0;

	/** 单轮淘汰赛超时窗口秒数(首测=1800即30分钟) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 knockout_round_timeout_seconds = 0;

	/** 海选胜场积分(首测=3) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 qualifier_win_points = 0;

	/** 海选平局积分(首测=1) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 qualifier_draw_points = 0;

	/** 海选负场积分(首测=0) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 qualifier_loss_points = 0;

	/** 战队邀请有效秒数(首测=259200即72小时) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 invite_ttl_seconds = 0;

	/** 规则说明 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	FString desc;

	/** 半决赛与总决赛是否启用 BO3 与五行灵脉阵眼轮转(V2,默认 true) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	bool bo3_top4_enabled = false;

	/** BO3 局间换阵与灵脉公示倒计时秒数(V2,默认 60) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 intermission_seconds = 0;

	/** 淘汰赛战报是否向全服公开回放(V2,默认 true) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	bool public_knockout_replay = false;

	/** 每日免费观道致敬绑定银两数量(V2,默认 5000) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 salute_gold_amount = 0;

	/** 每日免费观道致敬绑定道具 ID(V2,FK -> Item.id,默认 20 灵兽口粮) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 salute_item_id = 0;

	/** 每日免费观道致敬绑定道具数量(V2,默认 3) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 salute_item_count = 0;

	/** 报名与海选期是否开放零积分热身约战(V3,默认 true) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	bool warmup_spar_enabled = false;

	/** 淘汰赛阶段是否开放免费全服神机竞猜榜(V3,默认 true) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	bool prediction_enabled = false;

	/** 神机竞猜达标领奖所需最低命中场次(V3,默认 2) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 prediction_reward_min_hits = 0;

	/** 四强及以上淘汰赛是否加赛季军争夺战(V3,默认 true) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	bool third_place_match_enabled = false;

	/** BO3 淘汰赛首局开战前是否开启双方队长五行灵脉禁选(V4,默认 true) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	bool leyline_ban_enabled = false;

	/** 锦标赛每队战前技能禁选槽数（0=关闭，3=每队禁3招，BO3 每小局独立重禁） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TournamentRule")
	int32 skill_ban_slots_per_team = 0;
};

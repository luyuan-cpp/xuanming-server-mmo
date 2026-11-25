// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 GuildWarRule.xlsx 生成。**不要手改**。
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
//    generated/tables/guildwarrule.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgGuildWarRuleRow.generated.h"

/// GuildWarRule 的一行。字段名与 guildwarrule.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:GuildWarRuleTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgGuildWarRuleRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 规则行 id(固定 1) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 id = 0;

	/** 参与帮战与被挂追杀令的最低角色等级 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 min_level = 0;

	/** 从战意切回和平免战所需脱战冷却秒数 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 peace_cooldown_sec = 0;

	/** 战败后不可被强制点战保护秒数 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 post_battle_protect_sec = 0;

	/** 灵脉旗势上限 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 leyline_max_banner_hp = 0;

	/** 易主后初始旗势值 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 leyline_capture_init_hp = 0;

	/** 易主整备护盾秒数(护盾内打真人正常给奖但旗势不掉) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 leyline_shield_sec = 0;

	/** 攻方真人胜扣减旗势 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 pvp_win_banner_damage = 0;

	/** 守方真人胜恢复旗势 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 pvp_defend_banner_heal = 0;

	/** 攻方踢守旗阵胜扣减旗势 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 siege_win_banner_damage = 0;

	/** 本帮守旗阵槽位上限 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 garrison_max_slots = 0;

	/** 单人刷新守旗快照冷却秒数 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 garrison_refresh_cooldown_sec = 0;

	/** 帮战第几回合起启用决胜煞气(复接 escalation_stacks) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 frenzy_start_round = 0;

	/** 普通追杀令发布银两 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int64 bounty_normal_silver = 0;

	/** 重金追杀令发布银两 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int64 bounty_heavy_silver = 0;

	/** 追杀令银两系统销毁税率(万分比,1000=10%) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 bounty_tax_bps = 0;

	/** 帮会公费追杀令消耗帮会资金 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int64 bounty_public_funds = 0;

	/** 帮会每日公费追杀令上限 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 bounty_public_daily_limit = 0;

	/** 追杀令有效期秒数(默认 86400=24h) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 bounty_ttl_sec = 0;

	/** 被追杀销单后免再挂保护秒数(默认 1800=30m) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 bounty_protect_sec = 0;

	/** 目标累计反杀不同追兵几次撕毁追杀令(默认 2) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 bounty_counter_kill_tear = 0;

	/** 每角色每日领取追杀/反杀银两赏金上限(默认 5) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 bounty_daily_payout_limit = 0;

	/** 追杀追踪行踪节流冷却秒数(默认 15) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 track_cooldown_sec = 0;

	/** 同对手收益递减窗口秒数(默认 1800=30m) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 diminish_window_sec = 0;

	/** 灵脉占领周期分红间隔秒数(默认 3600=1h) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 dividend_interval_sec = 0;

	/** 每帮每周灵脉分红上限(默认 24) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 dividend_guild_weekly_cap = 0;

	/** 每角色每周可领灵脉分红上限(默认 12) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 dividend_player_weekly_cap = 0;

	/** 领取周战功宝箱与灵脉分红的最低个人周战功(默认 30) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int64 dividend_min_personal_merit = 0;

	/** 帮战真人/守旗复用对战模板(Dungeon.id) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 battle_config_id = 0;

	/** 当小时金脉(1.5x)帮贡/战功/旗势万分比倍率(默认 15000=1.5x) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 gold_leyline_multiplier_bps = 0;

	/** 本周宿敌额外加成万分比(默认 2000=+20%) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildWarRule")
	int32 rivalry_bonus_bps = 0;
};

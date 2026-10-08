// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 GuildActivity.xlsx 生成。**不要手改**。
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
//    generated/tables/guildactivity.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgGuildActivityRow.generated.h"

/// GuildActivity 的一行。字段名与 guildactivity.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:GuildActivityTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgGuildActivityRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 活动 id */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int32 id = 0;

	/** 界面显示名(非空) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	FString name;

	/** 活动类型:1 元宵灯会 / 2 中秋团圆 / 3 同道历练 决定走哪个 RPC;同一类型同一时刻只允许一行启用 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int32 type = 0;

	/** 是否启用:false = 关闭(界面显示"未开放") */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	bool enabled = false;

	/** 档期开始,UTC Unix 毫秒;与 end_at_ms 同为 0 = 常开(仅开发) 帮会级进度的档期键取这一天的游戏日,改它等于开新一期、旧进度不再计入 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int64 start_at_ms = 0;

	/** 档期结束,UTC Unix 毫秒(不含);非 0 时必须 > start_at_ms */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int64 end_at_ms = 0;

	/** 帮会等级下限(GuildLevel.id),>=1 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int32 min_guild_level = 0;

	/** 每次参与个人所得帮贡(点灯 / 领奖 / 历练胜利) 与计数同一事务写入 guild_member,不会出现次数扣了帮贡没到 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int64 personal_contribution = 0;

	/** 帮会资金:灯会、团圆 = 本档期达阈值时只发一次;历练 = 每个计资金的胜场发一次 0 = 不发资金(团圆默认 0,用户决策 3 只给团圆设个人奖) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int64 guild_funds = 0;

	/** 阈值:灯会 = 本档期点灯人次(>=1);历练 = 每游戏日计资金胜场上限(>=1) 团圆 = 同时在线人数,生效值取"本行值"与 GuildRule.reunion_min_online_members 两者较大(activity.ReunionThreshold) GuildRule 那列是下限:本行填 0 即按它;本行填得比它低也按它,只有填得更高才加码。两者都为 0 拒绝启动 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int32 guild_threshold = 0;

	/** 物品奖励。FK -> Reward.id,0 = 无 非 0 时奖励包必须非空、每项数量 >=1、物品种类 <=16(资产通道单次发放上限) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int32 reward_id = 0;

	/** 仅历练:副本。FK -> Dungeon.id(= battle_config_id);灯会、团圆必须填 0 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int32 dungeon_id = 0;

	/** 仅历练:含发起人的最少人数;灯会、团圆必须填 0。历练要求 2 <= min <= max <= 5 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int32 team_size_min = 0;

	/** 仅历练:含发起人的最多人数;灯会、团圆必须填 0 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int32 team_size_max = 0;

	/** 每人每游戏日可参与(得奖)次数,>=1 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|GuildActivity")
	int32 daily_limit = 0;
};

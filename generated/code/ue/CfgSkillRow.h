// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Skill.xlsx 生成。**不要手改**。
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
//    generated/tables/skill.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgSkillRow.generated.h"

/// Skill.required_item 的子消息行。
USTRUCT(BlueprintType)
struct FCfgSkillRequiredItem
{
	GENERATED_BODY()

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 required_item_type = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int64 required_item_value = 0;
};

/// Skill.required_resource 的子消息行。
USTRUCT(BlueprintType)
struct FCfgSkillRequiredResource
{
	GENERATED_BODY()

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 required_resource_type = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 required_resource_value = 0;
};

/// Skill.cost_resource 的子消息行。
USTRUCT(BlueprintType)
struct FCfgSkillCostResource
{
	GENERATED_BODY()

	/** 资源条件 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 cost_resource_id = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 cost_resource_cost = 0;
};

/// Skill 的一行。字段名与 skill.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:SkillTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
///
/// 表达式列:damage((cfg_expr:double))。
/// 这些列在 Excel 里填的是**公式**(例如 "100*level"),导表器原样搬进 JSON,
/// 所以 UE 侧的字段是 FString,里面躺着公式文本而不是求值结果。
/// 它们刻意**不带 BlueprintReadOnly**:带上就等于在蓝图面板里摆一个看起来是数据、
/// 实际是源码的字段,而 UE 这门没有 ExcelExpression 求值器(C++ 那门用
/// table_expression.h),蓝图侧拿到只会当字符串用错。要用先补 UE 版求值器,
/// 补好之后再决定暴露什么 —— 暴露的应该是**求值结果**,不是公式。
/// 字段名不能改(比如加 _expr 后缀):名字一改就与 JSON 键对不上,
/// FJsonObjectConverter 会静默跳过这一列。
USTRUCT(BlueprintType)
struct FCfgSkillRow : public FTableRowBase
{
	GENERATED_BODY()

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 id = 0;

	/** enum eSkillType : uint32_t { kPassiveSkill = 1 << 0, // 被动技能 kGeneralSkill = 1 << 1, // 普通施法技能 kChannelSkill = 1 << 2, // 持续施法技能 kToggleSkill = 1 << 3, // 开关类技能 kActivateSkill = 1 << 4, // 激活类技能 kBasicAttack = 1 << 5 // 普通攻击技能的单独枚举 }; */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	TArray<int32> skill_type;

	/** 0技能释放时不需要目标即可释放（如群疗，踩地板技能） -> 1 << 1 1技能释放时需要选定目标（单体指向性技能） -> 1 << 2 2技能释放时需要以指定地点为目标（常用于AOE技能） -> 1 << 3 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	TArray<int32> targeting_mode;

	/** 目标 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 require_target = 0;

	/** 目标状态 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 target_status = 0;

	/** CastPoint(毫秒Spell时间点）, 一般为动画抬手到攻击帧时长。比如说播放一个挥刀动画0.3秒后动画到攻击点，这时策划就配置CastPoint为0.3秒。 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	double cast_point = 0.0;

	/** 后摇阶段一般不能被其他技能打断，除非是连招或者强制立即释放类技能。我们的技能系统不需要引入公共CD这个概念，通过前摇后摇阶段的划分就足够满足各种需求了 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	double recovery_time = 0.0;

	/** 技能前摇阶段不允许其他技能释放，除非技能可强制立即释放（bImmediately=true）（如有些游戏要求滚动还有解控技可以立即打断当前技能） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 immediate = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	TArray<int32> effect;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 channel_think = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 channel_finish = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 think_interval = 0;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 channel_time = 0;

	/** 技能范围 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	double range = 0.0;

	/** 最大距离 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	double max_range = 0.0;

	/** 最小距离 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	double min_range = 0.0;

	/** 自身状态 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 self_status = 0;

	/** 所需状态 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 required_status = 0;

	/** 所需状态 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	int32 cooldown_id = 0;

	/** ⚠ 表达式列((cfg_expr:double))。值是**公式文本**(如 "100*level"),不是数;
	 *  刻意不暴露给 Blueprint,见上面结构体注释的「表达式列」。 */
	UPROPERTY()
	FString damage;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	TArray<FCfgSkillRequiredItem> required_item;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	TArray<FCfgSkillRequiredResource> required_resource;

	UPROPERTY(BlueprintReadOnly, Category = "Config|Skill")
	TArray<FCfgSkillCostResource> cost_resource;
};

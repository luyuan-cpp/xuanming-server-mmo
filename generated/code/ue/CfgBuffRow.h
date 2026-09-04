// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Buff.xlsx 生成。**不要手改**。
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
//    generated/tables/buff.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgBuffRow.generated.h"

/// Buff 的一行。字段名与 buff.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:BuffTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
///
/// 表达式列:health_regeneration((cfg_expr:double)), bonus_damage((cfg_expr:double))。
/// 这些列在 Excel 里填的是**公式**(例如 "100*level"),导表器原样搬进 JSON,
/// 所以 UE 侧的字段是 FString,里面躺着公式文本而不是求值结果。
/// 它们刻意**不带 BlueprintReadOnly**:带上就等于在蓝图面板里摆一个看起来是数据、
/// 实际是源码的字段,而 UE 这门没有 ExcelExpression 求值器(C++ 那门用
/// table_expression.h),蓝图侧拿到只会当字符串用错。要用先补 UE 版求值器,
/// 补好之后再决定暴露什么 —— 暴露的应该是**求值结果**,不是公式。
/// 字段名不能改(比如加 _expr 后缀):名字一改就与 JSON 键对不上,
/// FJsonObjectConverter 会静默跳过这一列。
USTRUCT(BlueprintType)
struct FCfgBuffRow : public FTableRowBase
{
	GENERATED_BODY()

	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	int32 id = 0;

	/** 强制没有施法者 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	int32 no_caster = 0;

	/** #pragma once enum eBuffType { // 控制类 Buff kBuffTypeMovementSpeedReduction = 0, // 移动速度减速 kBuffTypeAttackSpeedSlow = 1, // 攻击速度减速 kBuffTypeCastSpeedSlow = 2, // 技能施放速度减速 kBuffTypeGlobalSlow = 3, // 全局减速（移动、攻击、施法等都减速） // 属性增强类 Buff kBuffTypeIncreaseAttack = 10, // 增加攻击力 kBuffTypeIncreaseDefense = 11, // 增加防御力 kBuffTypeIncreaseHealth = 12, // 增加最大生命值 kBuffTypeMovementSpeedBoost = 13, // 提高移动速度 kBuffTypeIncreaseCriticalChance = 14, // 提高暴击率 // 防御类 Buff kBuffTypeDamageReduction = 20, // 减少受到的伤害 kBuffTypeShield = 21, // 获得护盾（吸收伤害） // 特殊类 Buff kBuffTypeStun = 30, // 晕眩，无法行动 kBuffTypeSilence = 31, // 沉默，无法施放技能 kBuffTypeInvincibility = 32, // 无敌，免疫所有伤害 kBuffTypeStealth = 33, // 隐身，无法被敌人发现 kBuffTypeImmunity = 34, // 免疫buff kBuffTypeDispel = 35, // 驱散，移除buff或debuff kBuffTypeNextBasicAttack = 36, // 下一次普攻，造成额外效果 // 持续恢复类 Buff kBuffTypeHealthRegeneration = 40, // 持续恢复生命值 kBuffTypeManaRegeneration = 41, // 持续恢复法力值 kBuffTypeHealthRegenerationBasedOnLostHealth = 42, // 根据已损失生命值的每秒回复 kBuffTypeNoDamageOrSkillHitInLastSeconds = 43, // 若在过去s秒内，没有受到伤害或被技能命中 // Debuff 类 kBuffTypePoison = 50, // 中毒，持续扣除生命值 kBuffTypeBurn = 51, // 燃烧，持续受到火焰伤害 kBuffTypeFreeze = 52, // 冰冻，无法行动 }; */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	int32 buff_type = 0;

	/** Slow:减速 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	TMap<FString, bool> tag;

	/** 免疫tag */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	TMap<FString, bool> immune_tag;

	/** 驱散tag */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	TMap<FString, bool> dispel_tag;

	/** 等级 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	int32 level = 0;

	/** 最大层数 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	int32 max_layer = 0;

	/** 无限时长 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	int32 infinite_duration = 0;

	/** 持续时间 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	double duration = 0.0;

	/** 强制打断 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	int32 force_interrupt = 0;

	/** 间隔时间 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	double interval = 0.0;

	/** 间隔时间 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	int32 interval_count = 0;

	/** 间隔时间 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	TArray<double> interval_effect;

	/** 移动速度加成 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	double movement_speed_boost = 0.0;

	/** 移速降低 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	double movement_speed_reduction = 0.0;

	/** 持续恢复公式 */
	/** ⚠ 表达式列((cfg_expr:double))。值是**公式文本**(如 "100*level"),不是数;
	 *  刻意不暴露给 Blueprint,见上面结构体注释的「表达式列」。 */
	UPROPERTY()
	FString health_regeneration;

	/** 子buff */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	TArray<int32> sub_buff;

	/** 子buff */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	TArray<int32> target_sub_buff;

	/** 若在过去s能命中 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	double combat_idle_seconds = 0.0;

	/** 生效次数 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|Buff")
	int32 time = 0;

	/** 额外物理伤害 */
	/** ⚠ 表达式列((cfg_expr:double))。值是**公式文本**(如 "100*level"),不是数;
	 *  刻意不暴露给 Blueprint,见上面结构体注释的「表达式列」。 */
	UPROPERTY()
	FString bonus_damage;
};

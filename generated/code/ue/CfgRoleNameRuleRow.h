// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 RoleNameRule.xlsx 生成。**不要手改**。
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
//    generated/tables/rolenamerule.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgRoleNameRuleRow.generated.h"

/// RoleNameRule 的一行。字段名与 rolenamerule.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:RoleNameRuleTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgRoleNameRuleRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 规则行 id(固定 1) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RoleNameRule")
	int32 id = 0;

	/** 角色名最少字数(Unicode 码点,去首尾空白后) 按码点数不按字节数:一个汉字 = 1 字;0 会让空串合法,启动校验拒之 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RoleNameRule")
	int32 min_chars = 0;

	/** 角色名最多字数(≤ 32) 上界 32 是代码里的结构上限 playername.StructuralMaxRunes,表里再大也会被启动校验拒 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RoleNameRule")
	int32 max_chars = 0;

	/** 空名时服务端生成名的前缀 只有机器人 / 无 UI 路径会走到(正式客户端建角必填),前缀本身也要满足字数与字符集规则 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RoleNameRule")
	FString generated_prefix;

	/** 生成名随机后缀位数([a-z0-9]) 前缀字数 + 本值必须落在 [min_chars, max_chars] 内;位数越小越容易撞名 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RoleNameRule")
	int32 generated_suffix_len = 0;

	/** 生成名撞名时最多尝试次数 每次尝试都要打一次名字登记表,调大会放大建角延迟;尝试用尽按失败返回,不降级成重名 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RoleNameRule")
	int32 max_generate_attempts = 0;

	/** 说明 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RoleNameRule")
	FString desc;
};

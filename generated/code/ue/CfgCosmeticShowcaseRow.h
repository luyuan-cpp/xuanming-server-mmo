// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 CosmeticShowcase.xlsx 生成。**不要手改**。
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
//    generated/tables/cosmeticshowcase.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgCosmeticShowcaseRow.generated.h"

/// CosmeticShowcase 的一行。字段名与 cosmeticshowcase.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:CosmeticShowcaseTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgCosmeticShowcaseRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 永久表主键 ID(1 起只增不删,经 cfg_bit_index 映射到拥有位图下标) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	int32 id = 0;

	/** 稳定外观标识键(全表唯一非空,如 "formation_skin_metal") */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	FString cosmetic_key;

	/** 槽位种类:1=脚底阵纹,2=入场与胜利演出,3=聊天气泡,4=分享卡底纹,5=个人名片底纹 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	int32 slot_kind = 0;

	/** 中文展示名 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	FString name;

	/** 客户端主视觉资源键(Assets/Resources/<asset_key>.png) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	FString asset_key;

	/** 可选次级视觉资源键(如胜利态/高亮态图,空串表示复用主资源) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	FString secondary_asset_key;

	/** 未拥有时允许消耗的永久解锁券道具 ID(0 = 仅活动/手记/支持者包专属;默认 3417 排面装饰券) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	int32 card_item_id = 0;

	/** 每次解锁消耗券张数(card_item_id > 0 时 >= 1,默认 1) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	int32 card_item_count = 0;

	/** 灵石百宝阁快捷跳转商品 ID(0 = 非百宝阁直售;默认 307) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	int32 spirit_shop_goods_id = 0;

	/** 是否启用:1 = 开放展示与穿戴,0 = 下架(不物理删行) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	int32 enabled = 0;

	/** 界面展示排序(升序) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	int32 sort_order = 0;

	/** 未拥有且非直售时展示的取得说明文案 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	FString acquire_hint;

	/** 外观简介(纯外观描述,零数值) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|CosmeticShowcase")
	FString desc;
};

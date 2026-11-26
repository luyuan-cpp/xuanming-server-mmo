// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 SpiritShopGoods.xlsx 生成。**不要手改**。
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
//    generated/tables/spiritshopgoods.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgSpiritShopGoodsRow.generated.h"

/// SpiritShopGoods 的一行。字段名与 spiritshopgoods.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:SpiritShopGoodsTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgSpiritShopGoodsRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 商品 ID（上线后只停用不复用旧 ID） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 id = 0;

	/** 分页：1 = 首套提速，2 = 精进配套，3 = 外观便利 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 tab_id = 0;

	/** 展示排序（升序） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 sort_order = 0;

	/** 上架开关：0 = 下架/停售，1 = 在售 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 enabled = 0;

	/** 商品展示名（留空时仅允许单物品商品回退物品表名） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	FString name;

	/** 商品简述（明码写清绑定仅自用、不加战力上限等说明） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	FString description;

	/** 图标资源键（留空回退 item_id_1 的 icon_key） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	FString icon_key;

	/** 结算货币：0 银两 / 1 灵石 / 2 绑定灵石（首批核心创收项统一填 1 = 灵石） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 currency_type = 0;

	/** 单份价格（1..1000000000，防乘法溢出） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int64 price = 0;

	/** 单次请求最大购买份数（1..99） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 max_buy_per_request = 0;

	/** 物品 1（必填）：FK -> Item.id */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 item_id_1 = 0;

	/** 物品 1 每份数量（>= 1） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 item_count_1 = 0;

	/** 物品 1 是否绑定：1 = 绑定仅自用，0 = 非绑定 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 item_bound_1 = 0;

	/** 物品 2（可选，0 = 无第二个物品；非 0 时启动/热更强校 Item.Exists） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 item_id_2 = 0;

	/** 物品 2 每份数量（item_id_2 > 0 时必须 >= 1） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 item_count_2 = 0;

	/** 物品 2 是否绑定：1 = 绑定仅自用，0 = 非绑定 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 item_bound_2 = 0;

	/** 单商品个人限购重置周期：0 = 终身，1 = 每周（UTC+8 周一 05:00） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 limit_reset_kind = 0;

	/** 单商品限购份数（0 = 不限购；>0 = 每周期或终身最多买多少份） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 limit_count = 0;

	/** 所属共享限购组 ID（0 = 不参与共享组；>0 时须存在对应 SpiritShopLimitGroup 行） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 limit_group_id = 0;

	/** 买 1 份消耗的共享组权重（limit_group_id > 0 时必须 >= 1） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	int32 limit_group_weight = 0;

	/** 客户端角标标签（如「终身限额」「保底配套」，可为空） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|SpiritShopGoods")
	FString badge_text;
};

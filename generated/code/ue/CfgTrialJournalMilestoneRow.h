// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 TrialJournalMilestone.xlsx 生成。**不要手改**。
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
//    generated/tables/trialjournalmilestone.json 不是 protojson,是导表器自己 json.dumps 出来的,
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

#include "CfgTrialJournalMilestoneRow.generated.h"

/// TrialJournalMilestone 的一行。字段名与 trialjournalmilestone.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:TrialJournalMilestoneTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgTrialJournalMilestoneRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 里程碑永久主键 ID（经 cfg_bit_index 映射到 PlayerTrialJournalComp.claimed_milestone_bits） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	int32 id = 0;

	/** 计数范围（0=全部永久卷汇总，1=卷一，2=卷二，3=卷三赛季隔离） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	int32 scope_chapter_id = 0;

	/** 赛季编号（0=永久里程碑，>0=对应赛季） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	int32 season_id = 0;

	/** 需点亮的有效战法印数量门槛 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	int32 required_seals = 0;

	/** 赠送的仅自用重铸砂数量（Item 3403；0=不发） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	int32 bound_sand_count = 0;

	/** 赠送的仅自用法宝拓印数量（Item 3404；0=不发） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	int32 bound_artifact_voucher_count = 0;

	/** 解锁的名片印戳键（空=无） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	FString badge_key;

	/** 解锁的名片印戳展示名（空=无） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	FString badge_name;

	/** 解锁的留灯院人物短评键（空=无） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	FString commentary_key;

	/** 解锁的人物短评标题（空=无） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	FString commentary_title;

	/** 人物短评正文（仅叙事展示，零数值加成） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	FString commentary_body;

	/** 终极专属永久形象稳定 ID（空=无；非空须为 CharacterAppearance 中启用且 card_unlockable=0 的玩法专属形象） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	FString grant_appearance_id;

	/** 是否启用（下架置 false） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	bool enabled = false;

	/** 里程碑标题 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	FString title;

	/** 里程碑说明 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	FString desc;

	/** 专属灵宠幻化永久 ID（0=无；非 0 须为 PetCosmetic 中启用且 card_item_id=0、mall_sku_id=0、spirit_shop_goods_id=0 的玩法专属幻化） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	int32 grant_pet_cosmetic_id = 0;

	/** 专属技能特效永久 ID（0=无；非 0 须为 SkillVfxCosmetic 中启用且 card_item_id=0、mall_sku_id=0、spirit_shop_goods_id=0 的玩法专属特效） */
	UPROPERTY(BlueprintReadOnly, Category = "Config|TrialJournalMilestone")
	int32 grant_skill_vfx_id = 0;
};

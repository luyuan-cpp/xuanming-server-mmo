// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Item.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgItemRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "ItemTable.h"
#include "EquipAffixRuleTable.h"
#include "EquipArtifactTable.h"
#include "ItemTable.h"

/// 解析 Item.affix_rule -> EquipAffixRule 行。
inline const FCfgEquipAffixRuleRow* GetItemAffixRuleRow(const UConfigSubsystem* Config, const FCfgItemRow& Row)
{
	const UEquipAffixRuleTable* Target = Config != nullptr ? Config->GetEquipAffixRuleTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.affix_rule) : nullptr;
}

/// 同上,按 Item 的 id 取行再解析。
inline const FCfgEquipAffixRuleRow* GetItemAffixRuleRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UItemTable* Source = Config != nullptr ? Config->GetItemTable() : nullptr;
	const FCfgItemRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetItemAffixRuleRow(Config, *Row) : nullptr;
}

/// 解析 Item.forge_material_id -> Item 行。
inline const FCfgItemRow* GetItemForgeMaterialIdRow(const UConfigSubsystem* Config, const FCfgItemRow& Row)
{
	const UItemTable* Target = Config != nullptr ? Config->GetItemTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.forge_material_id) : nullptr;
}

/// 同上,按 Item 的 id 取行再解析。
inline const FCfgItemRow* GetItemForgeMaterialIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UItemTable* Source = Config != nullptr ? Config->GetItemTable() : nullptr;
	const FCfgItemRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetItemForgeMaterialIdRow(Config, *Row) : nullptr;
}

/// 解析 Item.artifact_id -> EquipArtifact 行。
inline const FCfgEquipArtifactRow* GetItemArtifactIdRow(const UConfigSubsystem* Config, const FCfgItemRow& Row)
{
	const UEquipArtifactTable* Target = Config != nullptr ? Config->GetEquipArtifactTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.artifact_id) : nullptr;
}

/// 同上,按 Item 的 id 取行再解析。
inline const FCfgEquipArtifactRow* GetItemArtifactIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UItemTable* Source = Config != nullptr ? Config->GetItemTable() : nullptr;
	const FCfgItemRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetItemArtifactIdRow(Config, *Row) : nullptr;
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:affix_rule == Key 的全部 Item 行。
inline TArray<const FCfgItemRow*> FindItemRowsByAffixRule(const UConfigSubsystem* Config, int32 Key)
{
	const UItemTable* Source = Config != nullptr ? Config->GetItemTable() : nullptr;
	return Source != nullptr ? Source->GetByAffixRule(Key) : TArray<const FCfgItemRow*>();
}

/// 反查:forge_material_id == Key 的全部 Item 行。
inline TArray<const FCfgItemRow*> FindItemRowsByForgeMaterialId(const UConfigSubsystem* Config, int32 Key)
{
	const UItemTable* Source = Config != nullptr ? Config->GetItemTable() : nullptr;
	return Source != nullptr ? Source->GetByForgeMaterialId(Key) : TArray<const FCfgItemRow*>();
}

/// 反查:artifact_id == Key 的全部 Item 行。
inline TArray<const FCfgItemRow*> FindItemRowsByArtifactId(const UConfigSubsystem* Config, int32 Key)
{
	const UItemTable* Source = Config != nullptr ? Config->GetItemTable() : nullptr;
	return Source != nullptr ? Source->GetByArtifactId(Key) : TArray<const FCfgItemRow*>();
}

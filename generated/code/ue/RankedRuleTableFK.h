// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 RankedRule.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgRankedRuleRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "RankedRuleTable.h"
#include "ClassTable.h"
#include "DungeonTable.h"

/// 解析 RankedRule.battle_config_id -> Dungeon 行。
inline const FCfgDungeonRow* GetRankedRuleBattleConfigIdRow(const UConfigSubsystem* Config, const FCfgRankedRuleRow& Row)
{
	const UDungeonTable* Target = Config != nullptr ? Config->GetDungeonTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.battle_config_id) : nullptr;
}

/// 同上,按 RankedRule 的 id 取行再解析。
inline const FCfgDungeonRow* GetRankedRuleBattleConfigIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const URankedRuleTable* Source = Config != nullptr ? Config->GetRankedRuleTable() : nullptr;
	const FCfgRankedRuleRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetRankedRuleBattleConfigIdRow(Config, *Row) : nullptr;
}

/// 解析 RankedRule.reference_class_id -> Class 行。
inline const FCfgClassRow* GetRankedRuleReferenceClassIdRow(const UConfigSubsystem* Config, const FCfgRankedRuleRow& Row)
{
	const UClassTable* Target = Config != nullptr ? Config->GetClassTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.reference_class_id) : nullptr;
}

/// 同上,按 RankedRule 的 id 取行再解析。
inline const FCfgClassRow* GetRankedRuleReferenceClassIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const URankedRuleTable* Source = Config != nullptr ? Config->GetRankedRuleTable() : nullptr;
	const FCfgRankedRuleRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetRankedRuleReferenceClassIdRow(Config, *Row) : nullptr;
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:battle_config_id == Key 的全部 RankedRule 行。
inline TArray<const FCfgRankedRuleRow*> FindRankedRuleRowsByBattleConfigId(const UConfigSubsystem* Config, int32 Key)
{
	const URankedRuleTable* Source = Config != nullptr ? Config->GetRankedRuleTable() : nullptr;
	return Source != nullptr ? Source->GetByBattleConfigId(Key) : TArray<const FCfgRankedRuleRow*>();
}

/// 反查:reference_class_id == Key 的全部 RankedRule 行。
inline TArray<const FCfgRankedRuleRow*> FindRankedRuleRowsByReferenceClassId(const UConfigSubsystem* Config, int32 Key)
{
	const URankedRuleTable* Source = Config != nullptr ? Config->GetRankedRuleTable() : nullptr;
	return Source != nullptr ? Source->GetByReferenceClassId(Key) : TArray<const FCfgRankedRuleRow*>();
}

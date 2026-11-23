// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 RankedPreset.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgRankedPresetRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "RankedPresetTable.h"
#include "PetTable.h"
#include "PetEntryTable.h"
#include "SkillTable.h"

/// 解析 RankedPreset.pet_table_id -> Pet 行。
inline const FCfgPetRow* GetRankedPresetPetTableIdRow(const UConfigSubsystem* Config, const FCfgRankedPresetRow& Row)
{
	const UPetTable* Target = Config != nullptr ? Config->GetPetTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.pet_table_id) : nullptr;
}

/// 同上,按 RankedPreset 的 id 取行再解析。
inline const FCfgPetRow* GetRankedPresetPetTableIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const URankedPresetTable* Source = Config != nullptr ? Config->GetRankedPresetTable() : nullptr;
	const FCfgRankedPresetRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetRankedPresetPetTableIdRow(Config, *Row) : nullptr;
}

/// 解析 RankedPreset.skill_ids[] -> Skill 行。
inline TArray<const FCfgSkillRow*> GetRankedPresetSkillIdsRows(const UConfigSubsystem* Config, const FCfgRankedPresetRow& Row)
{
	TArray<const FCfgSkillRow*> Result;
	const USkillTable* Target = Config != nullptr ? Config->GetSkillTable() : nullptr;
	if (Target == nullptr)
	{
		return Result;
	}
	for (const int32& RefId : Row.skill_ids)
	{
		if (const FCfgSkillRow* Found = Target->FindByIdSilent(RefId))
		{
			Result.Add(Found);
		}
	}
	return Result;
}

/// 同上,按 RankedPreset 的 id 取行再解析。
inline TArray<const FCfgSkillRow*> GetRankedPresetSkillIdsRows(const UConfigSubsystem* Config, int32 TableId)
{
	const URankedPresetTable* Source = Config != nullptr ? Config->GetRankedPresetTable() : nullptr;
	const FCfgRankedPresetRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetRankedPresetSkillIdsRows(Config, *Row) : TArray<const FCfgSkillRow*>();
}

/// 解析 RankedPreset.pet_entry_ids[] -> PetEntry 行。
inline TArray<const FCfgPetEntryRow*> GetRankedPresetPetEntryIdsRows(const UConfigSubsystem* Config, const FCfgRankedPresetRow& Row)
{
	TArray<const FCfgPetEntryRow*> Result;
	const UPetEntryTable* Target = Config != nullptr ? Config->GetPetEntryTable() : nullptr;
	if (Target == nullptr)
	{
		return Result;
	}
	for (const int32& RefId : Row.pet_entry_ids)
	{
		if (const FCfgPetEntryRow* Found = Target->FindByIdSilent(RefId))
		{
			Result.Add(Found);
		}
	}
	return Result;
}

/// 同上,按 RankedPreset 的 id 取行再解析。
inline TArray<const FCfgPetEntryRow*> GetRankedPresetPetEntryIdsRows(const UConfigSubsystem* Config, int32 TableId)
{
	const URankedPresetTable* Source = Config != nullptr ? Config->GetRankedPresetTable() : nullptr;
	const FCfgRankedPresetRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetRankedPresetPetEntryIdsRows(Config, *Row) : TArray<const FCfgPetEntryRow*>();
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:pet_table_id == Key 的全部 RankedPreset 行。
inline TArray<const FCfgRankedPresetRow*> FindRankedPresetRowsByPetTableId(const UConfigSubsystem* Config, int32 Key)
{
	const URankedPresetTable* Source = Config != nullptr ? Config->GetRankedPresetTable() : nullptr;
	return Source != nullptr ? Source->GetByPetTableId(Key) : TArray<const FCfgRankedPresetRow*>();
}

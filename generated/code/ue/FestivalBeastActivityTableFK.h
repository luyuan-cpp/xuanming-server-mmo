// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 FestivalBeastActivity.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgFestivalBeastActivityRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "FestivalBeastActivityTable.h"
#include "DungeonTable.h"
#include "PetTable.h"

/// 解析 FestivalBeastActivity.battle_config_id -> Dungeon 行。
inline const FCfgDungeonRow* GetFestivalBeastActivityBattleConfigIdRow(const UConfigSubsystem* Config, const FCfgFestivalBeastActivityRow& Row)
{
	const UDungeonTable* Target = Config != nullptr ? Config->GetDungeonTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.battle_config_id) : nullptr;
}

/// 同上,按 FestivalBeastActivity 的 id 取行再解析。
inline const FCfgDungeonRow* GetFestivalBeastActivityBattleConfigIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UFestivalBeastActivityTable* Source = Config != nullptr ? Config->GetFestivalBeastActivityTable() : nullptr;
	const FCfgFestivalBeastActivityRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetFestivalBeastActivityBattleConfigIdRow(Config, *Row) : nullptr;
}

/// 解析 FestivalBeastActivity.reward_pet_table_id -> Pet 行。
inline const FCfgPetRow* GetFestivalBeastActivityRewardPetTableIdRow(const UConfigSubsystem* Config, const FCfgFestivalBeastActivityRow& Row)
{
	const UPetTable* Target = Config != nullptr ? Config->GetPetTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.reward_pet_table_id) : nullptr;
}

/// 同上,按 FestivalBeastActivity 的 id 取行再解析。
inline const FCfgPetRow* GetFestivalBeastActivityRewardPetTableIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UFestivalBeastActivityTable* Source = Config != nullptr ? Config->GetFestivalBeastActivityTable() : nullptr;
	const FCfgFestivalBeastActivityRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetFestivalBeastActivityRewardPetTableIdRow(Config, *Row) : nullptr;
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:battle_config_id == Key 的全部 FestivalBeastActivity 行。
inline TArray<const FCfgFestivalBeastActivityRow*> FindFestivalBeastActivityRowsByBattleConfigId(const UConfigSubsystem* Config, int32 Key)
{
	const UFestivalBeastActivityTable* Source = Config != nullptr ? Config->GetFestivalBeastActivityTable() : nullptr;
	return Source != nullptr ? Source->GetByBattleConfigId(Key) : TArray<const FCfgFestivalBeastActivityRow*>();
}

/// 反查:reward_pet_table_id == Key 的全部 FestivalBeastActivity 行。
inline TArray<const FCfgFestivalBeastActivityRow*> FindFestivalBeastActivityRowsByRewardPetTableId(const UConfigSubsystem* Config, int32 Key)
{
	const UFestivalBeastActivityTable* Source = Config != nullptr ? Config->GetFestivalBeastActivityTable() : nullptr;
	return Source != nullptr ? Source->GetByRewardPetTableId(Key) : TArray<const FCfgFestivalBeastActivityRow*>();
}

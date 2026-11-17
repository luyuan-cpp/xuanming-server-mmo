// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Monster.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgMonsterRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "MonsterTable.h"
#include "PetTable.h"

/// 解析 Monster.capture_pet_table_id -> Pet 行。
inline const FCfgPetRow* GetMonsterCapturePetTableIdRow(const UConfigSubsystem* Config, const FCfgMonsterRow& Row)
{
	const UPetTable* Target = Config != nullptr ? Config->GetPetTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.capture_pet_table_id) : nullptr;
}

/// 同上,按 Monster 的 id 取行再解析。
inline const FCfgPetRow* GetMonsterCapturePetTableIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UMonsterTable* Source = Config != nullptr ? Config->GetMonsterTable() : nullptr;
	const FCfgMonsterRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetMonsterCapturePetTableIdRow(Config, *Row) : nullptr;
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:capture_pet_table_id == Key 的全部 Monster 行。
inline TArray<const FCfgMonsterRow*> FindMonsterRowsByCapturePetTableId(const UConfigSubsystem* Config, int32 Key)
{
	const UMonsterTable* Source = Config != nullptr ? Config->GetMonsterTable() : nullptr;
	return Source != nullptr ? Source->GetByCapturePetTableId(Key) : TArray<const FCfgMonsterRow*>();
}

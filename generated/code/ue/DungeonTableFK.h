// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Dungeon.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgDungeonRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "DungeonTable.h"
#include "BaseSceneTable.h"
#include "MonsterTable.h"

/// 解析 Dungeon.scene_id -> BaseScene 行。
inline const FCfgBaseSceneRow* GetDungeonSceneIdRow(const UConfigSubsystem* Config, const FCfgDungeonRow& Row)
{
	const UBaseSceneTable* Target = Config != nullptr ? Config->GetBaseSceneTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.scene_id) : nullptr;
}

/// 同上,按 Dungeon 的 id 取行再解析。
inline const FCfgBaseSceneRow* GetDungeonSceneIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UDungeonTable* Source = Config != nullptr ? Config->GetDungeonTable() : nullptr;
	const FCfgDungeonRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetDungeonSceneIdRow(Config, *Row) : nullptr;
}

/// 解析 Dungeon.monster[] -> Monster 行。
inline TArray<const FCfgMonsterRow*> GetDungeonMonsterRows(const UConfigSubsystem* Config, const FCfgDungeonRow& Row)
{
	TArray<const FCfgMonsterRow*> Result;
	const UMonsterTable* Target = Config != nullptr ? Config->GetMonsterTable() : nullptr;
	if (Target == nullptr)
	{
		return Result;
	}
	for (const int32& RefId : Row.monster)
	{
		if (const FCfgMonsterRow* Found = Target->FindByIdSilent(RefId))
		{
			Result.Add(Found);
		}
	}
	return Result;
}

/// 同上,按 Dungeon 的 id 取行再解析。
inline TArray<const FCfgMonsterRow*> GetDungeonMonsterRows(const UConfigSubsystem* Config, int32 TableId)
{
	const UDungeonTable* Source = Config != nullptr ? Config->GetDungeonTable() : nullptr;
	const FCfgDungeonRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetDungeonMonsterRows(Config, *Row) : TArray<const FCfgMonsterRow*>();
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:scene_id == Key 的全部 Dungeon 行。
inline TArray<const FCfgDungeonRow*> FindDungeonRowsBySceneId(const UConfigSubsystem* Config, int32 Key)
{
	const UDungeonTable* Source = Config != nullptr ? Config->GetDungeonTable() : nullptr;
	return Source != nullptr ? Source->GetBySceneId(Key) : TArray<const FCfgDungeonRow*>();
}

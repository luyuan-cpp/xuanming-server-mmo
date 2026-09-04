// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Mission.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgMissionRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "MissionTable.h"
#include "ConditionTable.h"
#include "RewardTable.h"

/// 解析 Mission.reward_id -> Reward 行。
inline const FCfgRewardRow* GetMissionRewardIdRow(const UConfigSubsystem* Config, const FCfgMissionRow& Row)
{
	const URewardTable* Target = Config != nullptr ? Config->GetRewardTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.reward_id) : nullptr;
}

/// 同上,按 Mission 的 id 取行再解析。
inline const FCfgRewardRow* GetMissionRewardIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UMissionTable* Source = Config != nullptr ? Config->GetMissionTable() : nullptr;
	const FCfgMissionRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetMissionRewardIdRow(Config, *Row) : nullptr;
}

/// 解析 Mission.condition_id[] -> Condition 行(组外键 gfk)。
inline TArray<const FCfgConditionRow*> GetMissionConditionIdRows(const UConfigSubsystem* Config, const FCfgMissionRow& Row)
{
	TArray<const FCfgConditionRow*> Result;
	const UConditionTable* Target = Config != nullptr ? Config->GetConditionTable() : nullptr;
	if (Target == nullptr)
	{
		return Result;
	}
	for (const int32& RefId : Row.condition_id)
	{
		if (const FCfgConditionRow* Found = Target->FindByIdSilent(RefId))
		{
			Result.Add(Found);
		}
	}
	return Result;
}

/// 同上,按 Mission 的 id 取行再解析。
inline TArray<const FCfgConditionRow*> GetMissionConditionIdRows(const UConfigSubsystem* Config, int32 TableId)
{
	const UMissionTable* Source = Config != nullptr ? Config->GetMissionTable() : nullptr;
	const FCfgMissionRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetMissionConditionIdRows(Config, *Row) : TArray<const FCfgConditionRow*>();
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:reward_id == Key 的全部 Mission 行。
inline TArray<const FCfgMissionRow*> FindMissionRowsByRewardId(const UConfigSubsystem* Config, int32 Key)
{
	const UMissionTable* Source = Config != nullptr ? Config->GetMissionTable() : nullptr;
	return Source != nullptr ? Source->GetByRewardId(Key) : TArray<const FCfgMissionRow*>();
}

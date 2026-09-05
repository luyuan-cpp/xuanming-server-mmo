// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 AttributeAutoPlan.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgAttributeAutoPlanRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "AttributeAutoPlanTable.h"
#include "AttributeDimensionTable.h"
#include "AttributePoolTable.h"

/// 解析 AttributeAutoPlan.pool_id -> AttributePool 行。
inline const FCfgAttributePoolRow* GetAttributeAutoPlanPoolIdRow(const UConfigSubsystem* Config, const FCfgAttributeAutoPlanRow& Row)
{
	const UAttributePoolTable* Target = Config != nullptr ? Config->GetAttributePoolTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.pool_id) : nullptr;
}

/// 同上,按 AttributeAutoPlan 的 id 取行再解析。
inline const FCfgAttributePoolRow* GetAttributeAutoPlanPoolIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UAttributeAutoPlanTable* Source = Config != nullptr ? Config->GetAttributeAutoPlanTable() : nullptr;
	const FCfgAttributeAutoPlanRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetAttributeAutoPlanPoolIdRow(Config, *Row) : nullptr;
}

/// 解析 AttributeAutoPlan.dimension[] -> AttributeDimension 行。
inline TArray<const FCfgAttributeDimensionRow*> GetAttributeAutoPlanDimensionRows(const UConfigSubsystem* Config, const FCfgAttributeAutoPlanRow& Row)
{
	TArray<const FCfgAttributeDimensionRow*> Result;
	const UAttributeDimensionTable* Target = Config != nullptr ? Config->GetAttributeDimensionTable() : nullptr;
	if (Target == nullptr)
	{
		return Result;
	}
	for (const int32& RefId : Row.dimension)
	{
		if (const FCfgAttributeDimensionRow* Found = Target->FindByIdSilent(RefId))
		{
			Result.Add(Found);
		}
	}
	return Result;
}

/// 同上,按 AttributeAutoPlan 的 id 取行再解析。
inline TArray<const FCfgAttributeDimensionRow*> GetAttributeAutoPlanDimensionRows(const UConfigSubsystem* Config, int32 TableId)
{
	const UAttributeAutoPlanTable* Source = Config != nullptr ? Config->GetAttributeAutoPlanTable() : nullptr;
	const FCfgAttributeAutoPlanRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetAttributeAutoPlanDimensionRows(Config, *Row) : TArray<const FCfgAttributeDimensionRow*>();
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:pool_id == Key 的全部 AttributeAutoPlan 行。
inline TArray<const FCfgAttributeAutoPlanRow*> FindAttributeAutoPlanRowsByPoolId(const UConfigSubsystem* Config, int32 Key)
{
	const UAttributeAutoPlanTable* Source = Config != nullptr ? Config->GetAttributeAutoPlanTable() : nullptr;
	return Source != nullptr ? Source->GetByPoolId(Key) : TArray<const FCfgAttributeAutoPlanRow*>();
}

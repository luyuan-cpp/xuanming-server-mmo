// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 TestMultiKey.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgTestMultiKeyRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "TestMultiKeyTable.h"
#include "TestTable.h"

/// 解析 TestMultiKey.test_ref -> Test 行。
inline const FCfgTestRow* GetTestMultiKeyTestRefRow(const UConfigSubsystem* Config, const FCfgTestMultiKeyRow& Row)
{
	const UTestTable* Target = Config != nullptr ? Config->GetTestTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.test_ref) : nullptr;
}

/// 解析 TestMultiKey.test_refs[] -> Test 行(组外键 gfk)。
inline TArray<const FCfgTestRow*> GetTestMultiKeyTestRefsRows(const UConfigSubsystem* Config, const FCfgTestMultiKeyRow& Row)
{
	TArray<const FCfgTestRow*> Result;
	const UTestTable* Target = Config != nullptr ? Config->GetTestTable() : nullptr;
	if (Target == nullptr)
	{
		return Result;
	}
	for (const int32& RefId : Row.test_refs)
	{
		if (const FCfgTestRow* Found = Target->FindByIdSilent(RefId))
		{
			Result.Add(Found);
		}
	}
	return Result;
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:test_ref == Key 的全部 TestMultiKey 行。
inline TArray<const FCfgTestMultiKeyRow*> FindTestMultiKeyRowsByTestRef(const UConfigSubsystem* Config, int32 Key)
{
	const UTestMultiKeyTable* Source = Config != nullptr ? Config->GetTestMultiKeyTable() : nullptr;
	return Source != nullptr ? Source->GetByTestRef(Key) : TArray<const FCfgTestMultiKeyRow*>();
}

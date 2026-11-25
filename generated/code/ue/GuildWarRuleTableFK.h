// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 GuildWarRule.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  与 C++ 那门的差别只有一个:C++ 侧每张表是进程级单例(XxxTableManager::Instance()),
//  UE 侧表挂在 UConfigSubsystem(每个 GameInstance 一份)上,所以每个助手的第一个
//  参数是那个 subsystem。传 nullptr 一律返回空结果,不崩。
//
//  返回的 const FCfgGuildWarRuleRow* / const FCfg*Row* 属于**目标表当前快照**,
//  只在目标表下一次 Load 之前有效。调用方只存 id。
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部:
//  PublicDependencyModuleNames 要有 Core / CoreUObject / Engine / Json / JsonUtilities。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"

#include "ConfigSubsystem.h"
#include "GuildWarRuleTable.h"
#include "DungeonTable.h"

/// 解析 GuildWarRule.battle_config_id -> Dungeon 行。
inline const FCfgDungeonRow* GetGuildWarRuleBattleConfigIdRow(const UConfigSubsystem* Config, const FCfgGuildWarRuleRow& Row)
{
	const UDungeonTable* Target = Config != nullptr ? Config->GetDungeonTable() : nullptr;
	return Target != nullptr ? Target->FindByIdSilent(Row.battle_config_id) : nullptr;
}

/// 同上,按 GuildWarRule 的 id 取行再解析。
inline const FCfgDungeonRow* GetGuildWarRuleBattleConfigIdRow(const UConfigSubsystem* Config, int32 TableId)
{
	const UGuildWarRuleTable* Source = Config != nullptr ? Config->GetGuildWarRuleTable() : nullptr;
	const FCfgGuildWarRuleRow* Row = Source != nullptr ? Source->FindByIdSilent(TableId) : nullptr;
	return Row != nullptr ? GetGuildWarRuleBattleConfigIdRow(Config, *Row) : nullptr;
}

// ---------------------------------------------------------------------------
// 反向外键(HasMany):按外键列的值找源表的行
// ---------------------------------------------------------------------------

/// 反查:battle_config_id == Key 的全部 GuildWarRule 行。
inline TArray<const FCfgGuildWarRuleRow*> FindGuildWarRuleRowsByBattleConfigId(const UConfigSubsystem* Config, int32 Key)
{
	const UGuildWarRuleTable* Source = Config != nullptr ? Config->GetGuildWarRuleTable() : nullptr;
	return Source != nullptr ? Source->GetByBattleConfigId(Key) : TArray<const FCfgGuildWarRuleRow*>();
}

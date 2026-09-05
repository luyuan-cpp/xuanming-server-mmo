#pragma once
#include <cstdint>
#include <vector>

// 刻意只**前向声明**,不 include 任何表头。这些助手的签名里目标表只以
// const XTable* / std::vector<const XTable*> 的元素出现,指向不完整类型的指针
// 本身是完整类型,而函数**声明**从不要求返回/参数类型完整。
// 函数体在 <表名>_table_fk.cpp 里,那里才需要完整的 XTableManager。
//
// protoc 自己就在 table/proto/<表名>_table.pb.h 里写了同样的前向声明
// (全局作用域、无 package、无 dllexport_decl),所以这行是合法的重复声明。
class MissionTable;
class ConditionTable;
class RewardTable;

// ---------------------------------------------------------------------------
// Foreign key helpers for MissionTable
// ---------------------------------------------------------------------------

/// Resolve Mission.reward_id -> Reward row.
const RewardTable* GetMissionRewardIdRow(const MissionTable& row);

/// Resolve Mission.reward_id -> Reward row (by Mission id).
const RewardTable* GetMissionRewardIdRow(uint32_t tableId);

/// Resolve Mission.condition_id[] -> Condition rows.
std::vector<const ConditionTable*> GetMissionConditionIdRows(const MissionTable& row);

/// Resolve Mission.condition_id[] -> Condition rows (by Mission id).
std::vector<const ConditionTable*> GetMissionConditionIdRows(uint32_t tableId);

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all Mission rows whose reward_id == key.
const std::vector<const MissionTable*>& FindMissionRowsByRewardId(uint32_t key);

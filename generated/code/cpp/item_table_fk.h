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
class ItemTable;
class EquipAffixRuleTable;
class EquipArtifactTable;
class ItemTable;

// ---------------------------------------------------------------------------
// Foreign key helpers for ItemTable
// ---------------------------------------------------------------------------

/// Resolve Item.affix_rule -> EquipAffixRule row.
const EquipAffixRuleTable* GetItemAffixRuleRow(const ItemTable& row);

/// Resolve Item.affix_rule -> EquipAffixRule row (by Item id).
const EquipAffixRuleTable* GetItemAffixRuleRow(uint32_t tableId);

/// Resolve Item.forge_material_id -> Item row.
const ItemTable* GetItemForgeMaterialIdRow(const ItemTable& row);

/// Resolve Item.forge_material_id -> Item row (by Item id).
const ItemTable* GetItemForgeMaterialIdRow(uint32_t tableId);

/// Resolve Item.artifact_id -> EquipArtifact row.
const EquipArtifactTable* GetItemArtifactIdRow(const ItemTable& row);

/// Resolve Item.artifact_id -> EquipArtifact row (by Item id).
const EquipArtifactTable* GetItemArtifactIdRow(uint32_t tableId);

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all Item rows whose affix_rule == key.
const std::vector<const ItemTable*>& FindItemRowsByAffixRule(uint32_t key);

/// Reverse FK: find all Item rows whose forge_material_id == key.
const std::vector<const ItemTable*>& FindItemRowsByForgeMaterialId(uint32_t key);

/// Reverse FK: find all Item rows whose artifact_id == key.
const std::vector<const ItemTable*>& FindItemRowsByArtifactId(uint32_t key);

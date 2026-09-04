#pragma once
#include <vector>
#include "testmultikey_table.h"

#include "test_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for TestMultiKeyTable
// ---------------------------------------------------------------------------

/// Resolve TestMultiKey.test_ref -> Test row.
inline const TestTable* GetTestMultiKeyTestRefRow(const TestMultiKeyTable& row) {
    auto [ptr, _] = TestTableManager::Instance().FindByIdSilent(row.test_ref());
    return ptr;
}

/// Resolve TestMultiKey.test_refs[] -> Test rows.
inline std::vector<const TestTable*> GetTestMultiKeyTestRefsRows(const TestMultiKeyTable& row) {
    std::vector<const TestTable*> result;
    for (auto id : row.test_refs()) {
        auto [ptr, _] = TestTableManager::Instance().FindByIdSilent(id);
        if (ptr) result.push_back(ptr);
    }
    return result;
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

/// Reverse FK: find all TestMultiKey rows whose test_ref == key.
inline const std::vector<const TestMultiKeyTable*>& FindTestMultiKeyRowsByTestRef(uint32_t key) {
    return TestMultiKeyTableManager::Instance().GetByTestRef(key);
}

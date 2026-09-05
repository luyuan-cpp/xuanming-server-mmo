#include "testmultikey_table_fk.h"
#include "testmultikey_table.h"
#include "test_table.h"

// ---------------------------------------------------------------------------
// Foreign key helpers for TestMultiKeyTable
// ---------------------------------------------------------------------------

/// Resolve TestMultiKey.test_ref -> Test row.
const TestTable* GetTestMultiKeyTestRefRow(const TestMultiKeyTable& row) {
    auto [ptr, _] = TestTableManager::Instance().FindByIdSilent(row.test_ref());
    return ptr;
}

/// Resolve TestMultiKey.test_refs[] -> Test rows.
std::vector<const TestTable*> GetTestMultiKeyTestRefsRows(const TestMultiKeyTable& row) {
    std::vector<const TestTable*> result;
    for (auto id : row.test_refs()) {
        auto [ptr, _] = TestTableManager::Instance().FindByIdSilent(id);
        if (ptr) result.push_back(ptr);
    }
    return result;
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// 只碰本表的 manager,不需要任何目标表的类型。
// ---------------------------------------------------------------------------

/// Reverse FK: find all TestMultiKey rows whose test_ref == key.
const std::vector<const TestMultiKeyTable*>& FindTestMultiKeyRowsByTestRef(uint32_t key) {
    return TestMultiKeyTableManager::Instance().GetByTestRef(key);
}

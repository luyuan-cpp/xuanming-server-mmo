package com.game.table;

import java.util.ArrayList;
import java.util.List;

/**
 * Foreign key helpers for TestMultiKeyTable.
 * DO NOT EDIT -- regenerate from Excel via Data Table Exporter.
 */
public final class TestMultiKeyTableForeignKeys {
    private TestMultiKeyTableForeignKeys() {}

    /** Resolve TestMultiKey.test_ref -> Test row. */
    public static TestTable getTestRefRow(TestMultiKeyTable row) {
        return TestTableManager.getInstance().findById(row.getTestRef());
    }

    /** Resolve TestMultiKey.test_refs[] -> Test rows. */
    public static List<TestTable> getTestRefsRows(TestMultiKeyTable row) {
        List<TestTable> result = new ArrayList<>();
        for (int id : row.getTestRefsList()) {
            TestTable r = TestTableManager.getInstance().findById(id);
            if (r != null) { result.add(r); }
        }
        return result;
    }

    // ---- Reverse FK (HasMany): find source rows by FK column value ----

    /** Reverse FK: find all TestMultiKey rows whose test_ref == key. */
    public static List<TestMultiKeyTable> findRowsByTestRef(int key) {
        return TestMultiKeyTableManager.getInstance().getByTestRef(key);
    }

}

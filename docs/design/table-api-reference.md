# Configuration Table API Reference

配置表生成接口的使用说明。示例使用 C++ 语法；Go 和 Java 有等价 API。

结构定义以 [data/schema](../../data/schema/) 为准，xlsx 第 1 行列名、第 2–4 行空、第 5 行注释投影、第 6 行起数据。字段类型与选项写入 schema proto；完整修改流程见 [data/AGENTS.md](../../data/AGENTS.md)。

---

## Table of Contents

- [1. Global Table Loader](#1-global-table-loader)
- [2. Manager Basic Query](#2-manager-basic-query)
- [3. 二级索引 (cfg_index)](#3-二级索引-cfg_index)
- [4. 复合键](#4-复合键)
- [5. 外键 (cfg_fk / cfg_gfk)](#5-外键-cfg_fk--cfg_gfk)
- [6. Reverse Foreign Key (HasMany)](#6-reverse-foreign-key-hasmany)
- [7. Expression Column](#7-expression-column)
- [8. Utility Methods](#8-utility-methods)
- [9. Hot Reload](#9-hot-reload)
- [10. Schema 选项速查](#10-schema-选项速查)
- [11. Thread Safety](#11-thread-safety)

---

## 1. Global Table Loader

Defined in `table_loader.h` / `table_loader.cpp`.

```cpp
// Serial load: all Load() first, then all LoadSuccess(), then global callback.
void LoadTables();

// Parallel load: each table Load() in its own thread, barrier wait,
// then serial LoadSuccess() for all tables, then global callback.
void LoadTablesAsync();

// Register a global callback invoked after ALL tables finish Load+LoadSuccess.
void OnTablesLoadSuccess(const LoadSuccessCallback& callback);

// Re-load all tables from disk and bump version counter.
// WARNING: old proto data pointers become invalid after this call.
void ReloadTables();

// Monotonically-increasing counter, incremented by ReloadTables().
uint64_t GetTableLoadVersion();
```

### Load Lifecycle

```
Phase 1: Load()        -- parse pb file, build idMap + all indexes (per-table, parallel-safe)
Phase 2: LoadSuccess() -- per-table callback (all tables already loaded, safe for cross-table work)
Phase 3: global callback -- OnTablesLoadSuccess registered callback
```

---

## 2. Manager Basic Query

Every table `XxxTableManager` is a singleton with the following APIs. Examples use `MissionTableManager`.

### FindById / FindByIdSilent

```cpp
// Returns {row_ptr, error_code}. Logs warning if not found.
auto [row, err] = MissionTableManager::Instance().FindById(1001);

// Silent version: no log on miss.
auto [row, err] = MissionTableManager::Instance().FindByIdSilent(1001);
```

### FindAll

```cpp
// Returns the full protobuf data container (MissionTableData).
const auto& allData = MissionTableManager::Instance().FindAll();
for (int i = 0; i < allData.data_size(); ++i) {
    const auto& row = allData.data(i);
}
```

### GetIdMap

```cpp
// Returns unordered_map<uint32_t, const MissionTable*>.
const auto& idMap = MissionTableManager::Instance().GetIdMap();
```

### FindByKey (unique key column)

```cpp
// For columns marked with `key` option in Excel.
auto [row, err] = XxxTableManager::Instance().FindByColumnName(keyValue);
const auto& keyMap = XxxTableManager::Instance().GetColumnNameMap();
```

### FindByIds (batch, like SQL IN)

```cpp
// Returns vector of matching rows. Skips not-found ids silently.
auto rows = MissionTableManager::Instance().FindByIds({1001, 1002, 1003});
```

### Exists

```cpp
bool exists = MissionTableManager::Instance().Exists(1001);

// For unique key columns:
bool exists = XxxTableManager::Instance().ExistsByColumnName(keyValue);
```

### Count

```cpp
std::size_t total = MissionTableManager::Instance().Count();

// For multi-key columns:
std::size_t n = XxxTableManager::Instance().CountByColumnName(keyValue);
```

---

## 3. 二级索引 (cfg_index)

在权威 schema 的字段上写 `(cfg_index) = true`。加载时生成对应的二级索引，xlsx 中只填写列名与数据。

```proto
// data/schema/testmultikey_table.proto 的字段片段
uint32 level = 2 [(cfg_index) = true];
```

### Generated APIs

```cpp
// Get all rows with a given level (returns const reference, zero allocation).
const auto& rows = TestMultiKeyTableManager::Instance().GetByLevel(5);

// Get the full index map.
const auto& levelIndex = TestMultiKeyTableManager::Instance().GetLevelIndex();

// Count rows with a given level.
std::size_t n = TestMultiKeyTableManager::Instance().CountByLevelIndex(5);
```

**Performance**: O(1) lookup, returns `const vector&` -- no allocation per call.

---

## 4. 复合键

参与同一复合键的字段写相同的 `(cfg_composite)`。下面是示意字段，不是要求修改某张现有表的字段号；当前配置表是否使用该能力见 [全表索引](../../data/AGENTS.md#配置表索引)。

```proto
uint32 zone_id = 2 [(cfg_composite) = "ZoneServer"];
uint32 server_id = 3 [(cfg_composite) = "ZoneServer"];
```

### Generated APIs

```cpp
// Lookup by composite key (all columns combined).
const auto* row = XxxTableManager::Instance().FindByZoneServer(zoneId, serverId);

// Check existence.
bool exists = XxxTableManager::Instance().ExistsByZoneServer(zoneId, serverId);
```

---

## 5. 外键 (cfg_fk / cfg_gfk)

在权威 schema 上声明 `(cfg_fk) = "表名"` 或 `"表名.列名"`；组外键使用 `(cfg_gfk)`。repeated 字段还须声明物理容量 `(cfg_slots)`。

```proto
// data/schema/mission_table.proto 的字段片段
uint32 id = 1 [(cfg_bit_index) = true];
repeated uint32 condition_id = 2 [(cfg_slots) = 6, (cfg_gfk) = "Condition"];
uint32 reward_id = 7 [(cfg_fk) = "Reward"];
```

### Generated APIs (C++)

Located in `xxx_table_fk.h`:

```cpp
#include "mission_table_fk.h"

// ----- Scalar FK: reward_id -> Reward -----

// Pass a row reference:
const RewardTable* reward = GetMissionRewardIdRow(missionRow);

// Pass a mission id:
const RewardTable* reward = GetMissionRewardIdRow(missionId);

// ----- Repeated FK (gfk): condition_id[] -> Condition -----

// Pass a row reference:
std::vector<const ConditionTable*> conditions = GetMissionConditionIdRows(missionRow);

// Pass a mission id:
std::vector<const ConditionTable*> conditions = GetMissionConditionIdRows(missionId);
```

### Generated APIs (Go)

Located in `mission_table_fk.go`:

```go
import "table"

// Pass row:
conditions := table.GetMissionConditionIdRows(row)
reward, ok := table.GetMissionRewardIdRow(row)

// Pass id:
conditions := table.GetMissionConditionIdRowsById(missionId)
reward, ok := table.GetMissionRewardIdRowById(missionId)
```

### Generated APIs (Java)

Located in `MissionTableForeignKeys.java`:

```java
// Pass row:
List<ConditionTable> conditions = MissionTableForeignKeys.getConditionIdRows(row);
RewardTable reward = MissionTableForeignKeys.getRewardIdRow(row);

// Pass id:
List<ConditionTable> conditions = MissionTableForeignKeys.getConditionIdRows(tableId);
RewardTable reward = MissionTableForeignKeys.getRewardIdRow(tableId);
```

### FK is Stateless

FK helpers are **inline functions that perform live lookups** against the target table's current Snapshot. No cached data structure is built -- every call queries the target table directly. This means:

- No rebuild needed on hot reload
- Always sees the latest data after `ReloadTables()`
- Thread-safe (reads from immutable Snapshot)

---

## 6. Reverse Foreign Key (HasMany)

Auto-generated for **scalar** FK columns. Builds an `unordered_map<K, vector<const T*>>` index at load time.

```cpp
// Find all Mission rows that reference reward_id == 2001.
const auto& missions = FindMissionRowsByRewardId(2001);

// Go:
missions := table.FindMissionRowsByRewardId(2001)

// Java:
List<MissionTable> missions = MissionTableForeignKeys.findRowsByRewardId(2001);
```

**Note**: Reverse FK is only generated for scalar FK columns, not for repeated/gfk columns (a repeated column would need a multimap, which is less useful).

---

## 7. Expression Column

在 schema 的字符串字段上写 `(cfg_expr_type) = "double"` 或 `"int"`，参数用 `(cfg_expr_param)` 逐项声明。xlsx 列值仍为表达式字符串（如 `"x*2+1"`）。

```cpp
// Evaluate expression for a given row id.
double value = XxxTableManager::Instance().GetColumnName(tableId);

// Set expression parameters before evaluation.
XxxTableManager::Instance().SetColumnNameParam({playerLevel, attackPower});
```

---

## 8. Utility Methods

### Where (filter)

```cpp
// Returns all rows matching predicate.
auto rows = MissionTableManager::Instance().Where([](const MissionTable& row) {
    return row.mission_type() == 1;
});
```

### First

```cpp
// Returns first row matching predicate, or nullptr.
const auto* row = MissionTableManager::Instance().First([](const MissionTable& row) {
    return row.mission_type() == 1;
});
```

### RandOne

```cpp
// Returns a random row, or nullptr if table is empty.
const auto* row = MissionTableManager::Instance().RandOne();
```

### SetLoadSuccessCallback (per-table)

```cpp
// Register a callback invoked during LoadSuccess() phase.
// At this point ALL tables have finished Load(), safe for cross-table work.
MissionTableManager::Instance().SetLoadSuccessCallback([]() {
    // Build custom cross-table data structures here.
});
```

---

## 9. Hot Reload

### How It Works

1. `ReloadTables()` calls `LoadTables()` which rebuilds all Snapshots.
2. Each `Load()` creates a brand-new `Snapshot` (new idMap, new indexes, new data).
3. The old Snapshot is replaced atomically (`unique_ptr` swap).
4. `LoadSuccess()` callbacks fire with all tables loaded.
5. Global `loadSuccessCallback` fires last.
6. Version counter increments.

### What's Safe

| Operation | Effect |
|-----------|--------|
| Add rows | New Snapshot includes new rows + updated indexes |
| Delete rows | New Snapshot excludes deleted rows |
| Modify rows | New Snapshot has modified data |
| FK lookups | Stateless, always queries current Snapshot |
| idx lookups | Index rebuilt in Load(), returns const ref to new data |
| Reverse FK | Index rebuilt in Load(), returns const ref to new data |

### Caveat

After `ReloadTables()`, any **raw pointers** or **string_view** held from the old Snapshot are **dangling**. Always re-query after reload.

---

## 10. Schema 选项速查

选项词表在 [cfg_options.proto](../../data/schema/cfg_options.proto)。下表均写在 schema 字段选项中，不写入 xlsx 表头。

| 选项 | 说明 | 示例 |
|------|------|------|
| `cfg_key` | 唯一键；与 cfg_multi 合用时为多值键 | `(cfg_key) = true` |
| `cfg_multi` | 主键可重复或二级键多值；重复主键不生成 FindById | `(cfg_multi) = true` |
| `cfg_index` | 二级索引 | `(cfg_index) = true` |
| `cfg_bit_index` | 存档领取状态位序，不能用于重复主键表 | `(cfg_bit_index) = true` |
| `cfg_fk` | 外键表或指定列 | `(cfg_fk) = "BaseScene.scene_id"` |
| `cfg_gfk` | 组外键 | `(cfg_gfk) = "Condition"` |
| `cfg_composite` | 复合键组名 | `(cfg_composite) = "ZoneServer"` |
| `cfg_expr_type` | 表达式结果类型 | `(cfg_expr_type) = "double"` |
| `cfg_expr_param` | 表达式参数，可重复声明 | `(cfg_expr_param) = "x"` |
| `cfg_slots` | 非标量字段的物理容量 | `(cfg_slots) = 6` |
| `cfg_owner` | 产物归属；缺省 common | `(cfg_owner) = "server"` |

---

## 11. Thread Safety

- **Load phase**: Each table loads in its own thread (in `LoadTablesAsync`), only accesses its own data -- no cross-table dependency. Safe.
- **Read phase**: All reads go through immutable Snapshot. Multiple threads can read concurrently. Safe.
- **Reload**: `ReloadTables()` must be called from a single thread (the main/loader thread). After swap, readers see new data on next access.
- **FK helpers**: Stateless inline functions reading from current Snapshot. Thread-safe.

---

## Quick Reference: Naming Convention

| Pattern | C++ | Go | Java |
|---------|-----|-----|------|
| Singleton | `XxxTableManager::Instance()` | `XxxTableManagerInstance` | `XxxTableManager.getInstance()` |
| Find by id | `FindById(id)` | `FindById(id)` | `findById(id)` |
| FK (row) | `GetXxxColRow(row)` | `GetXxxColRow(row)` | `getColRow(row)` |
| FK (id) | `GetXxxColRow(id)` | `GetXxxColRowById(id)` | `getColRow(id)` |
| FK repeated (row) | `GetXxxColRows(row)` | `GetXxxColRows(row)` | `getColRows(row)` |
| FK repeated (id) | `GetXxxColRows(id)` | `GetXxxColRowsById(id)` | `getColRows(id)` |
| Reverse FK | `FindXxxRowsByCol(key)` | `FindXxxRowsByCol(key)` | `findRowsByCol(key)` |
| Idx lookup | `GetByCol(key)` | `GetByCol(key)` | `getByCol(key)` |
| Where | `Where(pred)` | -- | -- |
| First | `First(pred)` | -- | -- |
| RandOne | `RandOne()` | -- | -- |

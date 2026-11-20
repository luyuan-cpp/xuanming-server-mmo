# 配置表 API 参考手册

配置表生成接口的使用说明。示例使用 C++ 语法；Go 和 Java 有等价 API。

结构定义以 [data/schema](../../data/schema/) 为准，xlsx 第 1 行列名、第 2–4 行空、第 5 行注释投影、第 6 行起数据。字段类型与选项写入 schema proto；完整修改流程见 [data/AGENTS.md](../../data/AGENTS.md)。

---

## 目录

- [1. 全局表加载器](#1-全局表加载器)
- [2. Manager 基础查询](#2-manager-基础查询)
- [3. 二级索引 (cfg_index)](#3-二级索引-cfg_index)
- [4. 复合键](#4-复合键)
- [5. 外键 (cfg_fk / cfg_gfk)](#5-外键-cfg_fk--cfg_gfk)
- [6. 反向外键 (HasMany)](#6-反向外键-hasmany)
- [7. 表达式列](#7-表达式列)
- [8. 实用方法](#8-实用方法)
- [9. 热更新](#9-热更新)
- [10. Schema 选项速查](#10-schema-选项速查)
- [11. 线程安全](#11-线程安全)

---

## 1. 全局表加载器

定义在 `table_loader.h` / `table_loader.cpp`。

```cpp
// 串行加载：先全部 Load()，再全部 LoadSuccess()，最后全局回调。
void LoadTables();

// 并行加载：每个表在独立线程中 Load()，屏障等待，
// 然后串行调用所有表的 LoadSuccess()，最后全局回调。
void LoadTablesAsync();

// 注册全局回调，在所有表 Load+LoadSuccess 完成后调用。
void OnTablesLoadSuccess(const LoadSuccessCallback& callback);

// 从磁盘重新加载所有表，并递增版本号。
// 警告：旧的 proto 数据指针在此调用后失效。
void ReloadTables();

// 单调递增计数器，每次 ReloadTables() 后加 1。
uint64_t GetTableLoadVersion();
```

### 加载生命周期

```
阶段 1: Load()         -- 解析 pb 文件，构建 idMap + 所有索引（每个表独立，并行安全）
阶段 2: LoadSuccess()  -- 每个表的回调（此时所有表已加载完毕，可安全做跨表组建）
阶段 3: 全局回调        -- OnTablesLoadSuccess 注册的回调
```

---

## 2. Manager 基础查询

每个 `XxxTableManager` 都是单例，提供以下 API。示例使用 `MissionTableManager`。

### FindById / FindByIdSilent

```cpp
// 返回 {行指针, 错误码}。未找到时打印警告日志。
auto [row, err] = MissionTableManager::Instance().FindById(1001);

// 静默版本：未找到时不打日志。
auto [row, err] = MissionTableManager::Instance().FindByIdSilent(1001);
```

### FindAll

```cpp
// 返回完整的 protobuf 数据容器 (MissionTableData)。
const auto& allData = MissionTableManager::Instance().FindAll();
for (int i = 0; i < allData.data_size(); ++i) {
    const auto& row = allData.data(i);
}
```

### GetIdMap

```cpp
// 返回 unordered_map<uint32_t, const MissionTable*>。
const auto& idMap = MissionTableManager::Instance().GetIdMap();
```

### FindByKey（唯一键列）

```cpp
// 对 Excel 中标记了 `key` 选项的列。
auto [row, err] = XxxTableManager::Instance().FindByColumnName(keyValue);
const auto& keyMap = XxxTableManager::Instance().GetColumnNameMap();
```

### FindByIds（批量查询，类似 SQL IN）

```cpp
// 返回匹配的行向量。未找到的 id 静默跳过。
auto rows = MissionTableManager::Instance().FindByIds({1001, 1002, 1003});
```

### Exists

```cpp
bool exists = MissionTableManager::Instance().Exists(1001);

// 对唯一键列：
bool exists = XxxTableManager::Instance().ExistsByColumnName(keyValue);
```

### Count

```cpp
std::size_t total = MissionTableManager::Instance().Count();

// 对多键列：
std::size_t n = XxxTableManager::Instance().CountByColumnName(keyValue);
```

---

## 3. 二级索引 (cfg_index)

在权威 schema 的字段上写 `(cfg_index) = true`。加载时生成对应的二级索引，xlsx 中只填写列名与数据。

```proto
// data/schema/testmultikey_table.proto 的字段片段
uint32 level = 2 [(cfg_index) = true];
```

### 生成的 API

```cpp
// 获取所有 level 等于指定值的行（返回 const 引用，零分配）。
const auto& rows = TestMultiKeyTableManager::Instance().GetByLevel(5);

// 获取完整索引 map。
const auto& levelIndex = TestMultiKeyTableManager::Instance().GetLevelIndex();

// 统计 level 等于指定值的行数。
std::size_t n = TestMultiKeyTableManager::Instance().CountByLevelIndex(5);
```

**性能**: O(1) 查找，返回 `const vector&` -- 每次调用零内存分配。

---

## 4. 复合键

参与同一复合键的字段写相同的 `(cfg_composite)`。下面是示意字段，不是要求修改某张现有表的字段号；当前配置表是否使用该能力见 [全表索引](../../data/AGENTS.md#配置表索引)。

```proto
uint32 zone_id = 2 [(cfg_composite) = "ZoneServer"];
uint32 server_id = 3 [(cfg_composite) = "ZoneServer"];
```

### 生成的 API

```cpp
// 按复合键（多列组合）查找。
const auto* row = XxxTableManager::Instance().FindByZoneServer(zoneId, serverId);

// 检查是否存在。
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

### 生成的 API (C++)

位于 `xxx_table_fk.h`：

```cpp
#include "mission_table_fk.h"

// ----- 标量外键: reward_id -> Reward -----

// 传行引用：
const RewardTable* reward = GetMissionRewardIdRow(missionRow);

// 传 mission id：
const RewardTable* reward = GetMissionRewardIdRow(missionId);

// ----- repeated 外键 (gfk): condition_id[] -> Condition -----

// 传行引用：
std::vector<const ConditionTable*> conditions = GetMissionConditionIdRows(missionRow);

// 传 mission id：
std::vector<const ConditionTable*> conditions = GetMissionConditionIdRows(missionId);
```

### 生成的 API (Go)

位于 `mission_table_fk.go`：

```go
import "table"

// 传行：
conditions := table.GetMissionConditionIdRows(row)
reward, ok := table.GetMissionRewardIdRow(row)

// 传 id：
conditions := table.GetMissionConditionIdRowsById(missionId)
reward, ok := table.GetMissionRewardIdRowById(missionId)
```

### 生成的 API (Java)

位于 `MissionTableForeignKeys.java`：

```java
// 传行：
List<ConditionTable> conditions = MissionTableForeignKeys.getConditionIdRows(row);
RewardTable reward = MissionTableForeignKeys.getRewardIdRow(row);

// 传 id：
List<ConditionTable> conditions = MissionTableForeignKeys.getConditionIdRows(tableId);
RewardTable reward = MissionTableForeignKeys.getRewardIdRow(tableId);
```

### 外键是无状态的

外键 helper 是**内联函数，每次调用实时查询**目标表的当前 Snapshot。不构建缓存数据结构，因此：

- 热更新后无需重建
- `ReloadTables()` 后下一次调用自动看到最新数据
- 线程安全（读取不可变 Snapshot）

---

## 6. 反向外键 (HasMany)

为**标量** FK 列自动生成。加载时构建 `unordered_map<K, vector<const T*>>` 索引。

```cpp
// 查找所有引用 reward_id == 2001 的 Mission 行。
const auto& missions = FindMissionRowsByRewardId(2001);

// Go：
missions := table.FindMissionRowsByRewardId(2001)

// Java：
List<MissionTable> missions = MissionTableForeignKeys.findRowsByRewardId(2001);
```

**注意**: 反向外键仅针对标量 FK 列生成，不针对 repeated/gfk 列。

---

## 7. 表达式列

在 schema 的字符串字段上写 `(cfg_expr_type) = "double"` 或 `"int"`，参数用 `(cfg_expr_param)` 逐项声明。xlsx 列值仍为表达式字符串（如 `"x*2+1"`）。

```cpp
// 对指定行 id 求表达式值。
double value = XxxTableManager::Instance().GetColumnName(tableId);

// 在求值前设置表达式参数。
XxxTableManager::Instance().SetColumnNameParam({playerLevel, attackPower});
```

---

## 8. 实用方法

### Where（条件过滤）

```cpp
// 返回所有满足条件的行。
auto rows = MissionTableManager::Instance().Where([](const MissionTable& row) {
    return row.mission_type() == 1;
});
```

### First

```cpp
// 返回第一个满足条件的行，未找到返回 nullptr。
const auto* row = MissionTableManager::Instance().First([](const MissionTable& row) {
    return row.mission_type() == 1;
});
```

### RandOne

```cpp
// 随机返回一行，表为空时返回 nullptr。
const auto* row = MissionTableManager::Instance().RandOne();
```

### SetLoadSuccessCallback（每个表独立）

```cpp
// 注册在 LoadSuccess() 阶段调用的回调。
// 此时所有表已完成 Load()，可安全做跨表操作。
MissionTableManager::Instance().SetLoadSuccessCallback([]() {
    // 在这里构建自定义的跨表数据结构。
});
```

---

## 9. 热更新

### 工作原理

1. `ReloadTables()` 调用 `LoadTables()`，重建所有 Snapshot。
2. 每个 `Load()` 创建全新的 `Snapshot`（新的 idMap、新的索引、新的数据）。
3. 旧 Snapshot 被原子替换（`unique_ptr` swap）。
4. `LoadSuccess()` 回调在所有表加载完毕后触发。
5. 全局 `loadSuccessCallback` 最后触发。
6. 版本号递增。

### 安全性

| 操作 | 效果 |
|------|------|
| 增加行 | 新 Snapshot 包含新行 + 更新的索引 |
| 删除行 | 新 Snapshot 不包含已删行 |
| 修改行 | 新 Snapshot 包含修改后的数据 |
| FK 查询 | 无状态，始终查询当前 Snapshot |
| idx 查询 | 索引在 Load() 中重建，返回新数据的 const 引用 |
| 反向 FK | 索引在 Load() 中重建，返回新数据的 const 引用 |

### 注意事项

`ReloadTables()` 之后，任何持有的旧 Snapshot 的**原始指针**或 **string_view** 都将**悬垂**。重载后必须重新查询。

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

## 11. 线程安全

- **加载阶段**: 每个表在独立线程中加载（`LoadTablesAsync`），只访问自身数据，无跨表依赖。安全。
- **读取阶段**: 所有读取通过不可变 Snapshot 进行。多线程可并发读取。安全。
- **重载**: `ReloadTables()` 须在单线程（主线程/加载线程）中调用。swap 后，读者在下次访问时看到新数据。
- **FK helper**: 无状态内联函数，读取当前 Snapshot。线程安全。

---

## 速查表：命名规范

| 模式 | C++ | Go | Java |
|------|-----|-----|------|
| 单例 | `XxxTableManager::Instance()` | `XxxTableManagerInstance` | `XxxTableManager.getInstance()` |
| 按 id 查找 | `FindById(id)` | `FindById(id)` | `findById(id)` |
| FK (传行) | `GetXxxColRow(row)` | `GetXxxColRow(row)` | `getColRow(row)` |
| FK (传id) | `GetXxxColRow(id)` | `GetXxxColRowById(id)` | `getColRow(id)` |
| FK repeated (传行) | `GetXxxColRows(row)` | `GetXxxColRows(row)` | `getColRows(row)` |
| FK repeated (传id) | `GetXxxColRows(id)` | `GetXxxColRowsById(id)` | `getColRows(id)` |
| 反向 FK | `FindXxxRowsByCol(key)` | `FindXxxRowsByCol(key)` | `findRowsByCol(key)` |
| 二级索引查询 | `GetByCol(key)` | `GetByCol(key)` | `getByCol(key)` |
| 条件过滤 | `Where(pred)` | -- | -- |
| 首条匹配 | `First(pred)` | -- | -- |
| 随机一条 | `RandOne()` | -- | -- |

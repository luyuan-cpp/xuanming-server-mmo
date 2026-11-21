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

完整设计见 [配置表热更：表包、轨道指针与进程内整批切换](config-table-hot-reload.md)。

### 工作原理（两阶段整批切换）

1. **只切声明过的表**：逐表在 `data/schema/*_table.proto` 声明 `option (cfg_hot_reload) = true;`（缺省 `false`）。改动触及未声明表时进程不切换并汇报 `needs_restart`，待滚动重启生效。
2. **C++（`TableBundleManager` + `TableSet`）**：
   - `ParseSnapshotFromDir` 构造独立不可变 `std::shared_ptr<const XxxTableSnapshot>`，失败返回错误、不 `LOG_FATAL`；
   - 主 EventLoop 每轮 `queueInLoop` 分片解析 1 张表（不起新线程），装成整套 `TableSet`；
   - 在 `ScopedTableSetOverride` 作用域内执行 `TableLoadHandler::Validate()`，失败即丢弃并继续用旧表；
   - 到 `activate_at_ms` 原子 `CommitTableSet` 并触发 `OnCommitted`，旧 `TableSet` 留存 10 分钟后退役（战斗房间显式持有 `shared_ptr<const TableSet>` 直到该局结束）。
3. **Go（`shared/tablebundle` + `table.Prepare` / `table.Commit` / `table.Current`）**：
   - `table.Prepare(dir, useBinary)` 构造独立 `*table.TableSet`，校验与派生数据编译通过后由 `table.Commit(ts)` 替换各表 `atomic.Pointer` 并发布 `table.Current()`；
   - 跨多张表要求同版一致时先取 `ts := table.Current()` 再经 `ts` 查询；旧的裸赋值 `ReloadTables` 已删除。

### 安全性

| 操作 | 效果 |
|------|------|
| 增加行 | 新 `TableSet` 包含新行 + 更新的索引 |
| 删除行 | 新 `TableSet` 不包含已删行（`(cfg_ids_permanent)` 表禁止删行） |
| 修改行 | 新 `TableSet` 包含修改后的数据 |
| FK 查询 | 无状态，始终查询当前快照 |
| idx / 反向 FK 查询 | 索引在 `Prepare` 阶段重建，提交后原子切换 |

### 注意事项

**硬规则（`data/AGENTS.md`）**：业务结构体、定时器闭包与协程状态**只存 id，不存表行原始指针或 `string_view`**。C++ 10 分钟退役期仅兜底单次短 `co_await`，过了 10 分钟保存的行指针仍会悬垂。

---

## 10. Schema 选项速查

选项词表在 [cfg_options.proto](../../data/schema/cfg_options.proto)。下表均写在 schema 选项中，不写入 xlsx 表头。

| 选项 | 说明 | 示例 |
|------|------|------|
| `cfg_hot_reload` | 表级选项；是否允许运行期热更（缺省 `false`） | `option (cfg_hot_reload) = true;` |
| `cfg_ids_permanent` | 表级选项；表行 ID 只增不删（如 `Item`、`Pet`） | `option (cfg_ids_permanent) = true;` |
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

- **启动加载阶段**：C++ `LoadTablesAsync` 并行解析并在返回前全部 `join`；Go `tablebundle.LoadAtStartup` 在提供服务前同步提交首套 `TableSet`。
- **读取阶段**：所有读取通过不可变 `Snapshot` / `TableSet` 进行，无状态 const `Evaluate*` 表达式求值，支持并发读取。
- **热更提交**：C++ 全程在主 EventLoop 单线程执行（不新起线程）；Go 在后台协程 `Prepare` + 校验完成后经 `atomic.Pointer` 发布 `Commit`。
- **FK helper**：无状态内联函数，读取当前快照，线程安全。

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

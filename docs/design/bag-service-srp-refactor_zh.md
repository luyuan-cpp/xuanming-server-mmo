# Bag → BagService 单一职责重构 (2026-03-26)

> 2026-10-15：`PlayerItemBlockList` 与 `blockList` 参数已按 C004 删除，下文为 2026-03-26 历史。
> 2026-11-05：单玩家物品获取封禁以新形状实现，不是 `BagService` 的参数：状态是持久组件 `PlayerItemGainBlockComp`（`player_database.item_gain_block`，由收件箱 GM 域写），`BagService` 各入包入口内部调统一判定 `GainBlockService::IsItemGainBlocked`（全服 + 本人，命中回 `kAssetBlocked`）。见 [player-inbox-service.md](player-inbox-service.md) §2.2。

## 问题
`Bag` 类职责过多——它是一个容器，但同时还处理了：
- 全局物品封锁检查（GainBlockService）
- 单玩家 GM 封锁检查（BlockItem/UnblockItem/IsItemBlocked + blocked_config_ids_）
- 交易日志（TransactionLogSystem::LogItemCreate/LogItemDestroy）
- 异常检测（AnomalyDetector::RecordItemGain）

## 解决方案
将横切关注点提取到 `BagService`（上层编排层）。

### Bag（纯容器）— 仅负责：
- 格子/槽位管理（容量、位置映射、空间检查）
- 物品 CRUD（AddItem、RemoveItem、RemoveItems、RemoveItemByPos）
- 堆叠逻辑（可堆叠 vs 不可堆叠）
- 整理（合并部分堆叠）
- 解锁（扩展容量）

### BagService（编排层）— 处理：
- `BagService::AddItem(entity, bag, blockList, param)`：封锁检查 → Bag::AddItem → 交易日志 → 异常检测
- `BagService::RemoveItem(entity, bag, guid)`：捕获信息 → Bag::RemoveItem → 交易日志

### PlayerItemBlockList（数据）— 单玩家 GM 封锁状态：
- `Block(configId)`、`Unblock(configId)`、`IsBlocked(configId)`、`All()`
- 独立结构体，与 Bag 不耦合

## 相关文件
- `cpp/libs/modules/bag/bag_service.h` — BagService + PlayerItemBlockList 声明
- `cpp/libs/modules/bag/bag_service.cpp` — 实现
- `cpp/libs/modules/bag/bag_system.h` — 移除了封锁相关成员
- `cpp/libs/modules/bag/bag_system.cpp` — 移除了 GainBlockService/TransactionLog/AnomalyDetector
- `cpp/tests/bag_test/bag_test.cpp` — 封锁测试使用 BagService + PlayerItemBlockList
- `cpp/libs/modules/modules.vcxproj` — 添加了 bag_service.cpp/.h

## 原则
**Bag 是纯容器**（"怎么放进去"），**BagService 是策略层**（"能不能给" + 副作用）。
修改封锁规则、日志策略或异常检测，永远不需要修改 Bag 的代码。

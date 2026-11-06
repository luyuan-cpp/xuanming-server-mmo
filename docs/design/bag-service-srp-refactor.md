# Bag → BagService SRP Refactor (2026-03-26)

> 2026-10-15：`PlayerItemBlockList` 与 `blockList` 参数已按 C004 删除，下文为 2026-03-26 历史。
> 2026-11-05：单玩家物品获取封禁以新形状实现，不是 `BagService` 的参数：状态是持久组件 `PlayerItemGainBlockComp`（`player_database.item_gain_block`，由收件箱 GM 域写），`BagService` 各入包入口内部调统一判定 `GainBlockService::IsItemGainBlocked`（全服 + 本人，命中回 `kAssetBlocked`）。见 [player-inbox-service.md](player-inbox-service.md) §2.2。

## Problem
`Bag` class had too many responsibilities — it was a container but also handled:
- Global item block checks (GainBlockService)
- Per-player GM block checks (BlockItem/UnblockItem/IsItemBlocked + blocked_config_ids_)
- Transaction logging (TransactionLogSystem::LogItemCreate/LogItemDestroy)
- Anomaly detection (AnomalyDetector::RecordItemGain)

## Solution
Extracted cross-cutting concerns into `BagService` (upper-level orchestration layer).

### Bag (pure container) — only does:
- Grid/slot management (capacity, pos mapping, space checks)
- Item CRUD (AddItem, RemoveItem, RemoveItems, RemoveItemByPos)
- Stack logic (stackable vs non-stackable)
- Neaten (consolidate partial stacks)
- Unlock (expand capacity)

### BagService (orchestration) — handles:
- `BagService::AddItem(entity, bag, blockList, param)`: block check → Bag::AddItem → tx log → anomaly
- `BagService::RemoveItem(entity, bag, guid)`: capture info → Bag::RemoveItem → tx log

### PlayerItemBlockList (data) — per-player GM block state:
- `Block(configId)`, `Unblock(configId)`, `IsBlocked(configId)`, `All()`
- Separate struct, not coupled to Bag

## Files
- `cpp/libs/modules/bag/bag_service.h` — BagService + PlayerItemBlockList declarations
- `cpp/libs/modules/bag/bag_service.cpp` — implementation
- `cpp/libs/modules/bag/bag_system.h` — stripped of block members
- `cpp/libs/modules/bag/bag_system.cpp` — stripped of GainBlockService/TransactionLog/AnomalyDetector
- `cpp/tests/bag_test/bag_test.cpp` — block tests use BagService + PlayerItemBlockList
- `cpp/libs/modules/modules.vcxproj` — added bag_service.cpp/.h

## Principle
**Bag is a pure container** ("怎么放进去"), **BagService is the policy layer** ("能不能给" + side effects).
Changing block rules, logging strategy, or anomaly detection never requires modifying Bag code.

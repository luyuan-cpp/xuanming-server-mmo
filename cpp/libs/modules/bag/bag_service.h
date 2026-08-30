#pragma once

#include <unordered_set>

#include "engine/core/type_define/type_define.h"

#include "bag_system.h"

// Per-player item block list (GM feature).
// Stored separately from Bag — Bag is a pure container.
struct PlayerItemBlockList
{
	void Block(uint32_t configId);
	void Unblock(uint32_t configId);
	bool IsBlocked(uint32_t configId) const;
	const std::unordered_set<uint32_t> &All() const { return blocked_config_ids_; }

private:
	std::unordered_set<uint32_t> blocked_config_ids_;
};

// Orchestration layer for bag operations.
//
// Handles cross-cutting concerns that do NOT belong in the Bag container:
//   - Global (server-wide) item block check  (GainBlockService)
//   - Per-player GM item block check          (PlayerItemBlockList)
//   - Transaction logging                     (TransactionLogSystem)
//   - Anomaly detection                       (AnomalyDetector)
//
// Bag itself is a pure container (grid / slot / stack management only).
class BagService
{
public:
	// Orchestrated AddItem:
	//   block check → Bag::AddItem → transaction log → anomaly detection
	static uint32_t AddItem(
		entt::entity playerEntity,
		Bag &bag,
		const PlayerItemBlockList &blockList,
		const InitItemParam &param);

	// Orchestrated batch AddItems (config_id → count):
	//   transactional — all-or-nothing space check, then per-config
	//   block check → Bag::AddItem → transaction log → anomaly detection.
	//   Symmetric with Bag::RemoveItems.
	static uint32_t AddItems(
		entt::entity playerEntity,
		Bag &bag,
		const PlayerItemBlockList &blockList,
		const ItemCountMap &itemsToAdd);

	// Orchestrated batch AddItems carrying full ItemComp per piece
	//   (mail attachments mixing equipment + stackable items): preserves
	//   each piece's preassigned guid / attributes. Transactional —
	//   all-or-nothing space check, then per-piece
	//   block check → Bag::AddItem → transaction log → anomaly detection.
	static uint32_t AddItems(
		entt::entity playerEntity,
		Bag &bag,
		const PlayerItemBlockList &blockList,
		const std::vector<InitItemParam> &itemsToAdd);

	// Orchestrated RemoveItem:
	//   capture item info → Bag::RemoveItem → transaction log
	static uint32_t RemoveItem(
		entt::entity playerEntity,
		Bag &bag,
		Guid guid);

	// Orchestrated MergeAndCompact (背包整理):
	//   frozen check → Bag::MergeAndCompact → transaction log per retired instance
	//
	// **生产代码整理背包一律走这个入口,不要直接调 Bag::MergeAndCompact。**
	// 整理会退役一批 item_uuid(被合并空的堆、以及 size==0 的僵尸实例),而
	// item_uuid 正是 transaction_log 做外挂回收关联的键 —— 直接调纯容器那层,
	// 这些实例就无声消失了,追溯链在整理这一步断掉。
	//
	// **policy 刻意没有默认值。** "开背包要不要自动重排"是一条会和"跨服还原
	// 保持原位"直接冲突的产品取舍,拆分前从来没人做过这个取舍(只有重排一种
	// 行为)。现在把它变成调用点必须填的参数 —— 你没法在不表态的情况下整理背包:
	//   * 自动触发(开背包、入包后顺手整理)-> kMergeOnly,绝不挪位置
	//   * 玩家显式点"整理"                  -> kMergeAndReorder
	//
	// changedOut(可为 nullptr)透传 Bag::MergeAndCompact 的返回值:
	// 背包是否真的动过(已最优时会早退,返回 false)。
	static uint32_t MergeAndCompact(
		entt::entity playerEntity,
		Bag &bag,
		CompactPolicy policy,
		bool *changedOut = nullptr);

	// 玩家点击"整理"。
	//
	// **2026-08-27 拍板:整理是玩家的显式动作,不是开背包的副作用。**
	// 所以这是目前唯一会重排位置的入口 —— 背包 UI 上那个"整理"按钮接到这里。
	//
	// 自动触发的路径(开背包、入包后顺手收拾)应当走
	// MergeAndCompact(..., CompactPolicy::kMergeOnly):只合并堆叠、回收空实例,
	// 一格不挪。这样跨服回来"东西还在你离开时摆的地方"才成立。
	//
	// 以后若要做成"打开背包自动整理"的玩家设置,不需要改这两层中的任何一层:
	// 读设置,然后决定调这个还是调 kMergeOnly 那个。CompactPolicy 就是为这一天
	// 留的口子。
	static uint32_t SortByPlayerRequest(
		entt::entity playerEntity,
		Bag &bag,
		bool *changedOut = nullptr);
};

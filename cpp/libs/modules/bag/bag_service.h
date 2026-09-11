#pragma once

#include <unordered_set>

#include "engine/core/type_define/type_define.h"

#include "bag_system.h"
#include "proto/common/rollback/transaction_log.pb.h"

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
// 2026-09-01 起还多一件:**为腾位而被挤掉的实例要落 LogItemDestroy**。
// 临时格(kTemporary)配的是先进先出淘汰策略,满了会销毁最早进包的实例;
// `item_uuid` 是外挂回收的关联键,不留痕追溯链就在这一步断掉。Bag 是纯容器
// 不写流水,所以它把退役清单**回执**上来,由这里落 —— 形状与 MergeAndCompact
// 那条路完全一致。批量入包因此必须走 `Bag::ReserveForBatchAdd` 而不是纯预测的
// `CheckSpaceFor`,否则淘汰在批量路径上永远不会发生。
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
	//   per-config block check → Bag::ReserveForBatchAdd(全部纯预检 + 腾位)
	//   → per-config Bag::AddItem → transaction log → anomaly detection.
	//
	//   原子性口径(2026-09-01 更正):所有**会拒绝**的判断都在 ReserveForBatchAdd
	//   之前或之内完成;它返回成功之后,逐项写入不会再失败(失败即编程错误)。
	//   但注意:临时格(kTemporary)上 reserve 本身可能**已经销毁**了最早的物品来
	//   腾位 —— 那些实例已落 LogItemDestroy,且即使后续失败也不会复活。调用方不能
	//   把"返回失败"理解为"包与调用前完全一致"。
	static uint32_t AddItems(
		entt::entity playerEntity,
		Bag &bag,
		const PlayerItemBlockList &blockList,
		const ItemCountMap &itemsToAdd,
		TransactionType txType = TX_SYSTEM_GRANT);

	// Orchestrated batch AddItems carrying full ItemComp per piece
	//   (mail attachments mixing equipment + stackable items): preserves
	//   each piece's preassigned guid / attributes.
	//   per-piece block check → Bag::ReserveForBatchAdd(vector 重载:含预设 guid
	//   撞车预检)→ per-piece Bag::AddItem → transaction log → anomaly detection.
	//   原子性口径同上。
	static uint32_t AddItems(
		entt::entity playerEntity,
		Bag &bag,
		const PlayerItemBlockList &blockList,
		const std::vector<InitItemParam> &itemsToAdd,
		TransactionType txType = TX_SYSTEM_GRANT);

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

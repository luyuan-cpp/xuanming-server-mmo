#pragma once

#include <string>
#include <unordered_set>
#include <vector>

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
	// correlationId / extra 落进流水(战斗掉落传 battle_id 与来源 JSON)。
	// TX_ITEM_AWARD 的 proto 注释要求 extra 带来源,否则回滚重放无法判断这笔奖励是否仍有效。
	//
	// mutated(可为 nullptr)回答一个上面那段"原子性口径"注释点破、却没法从返回值读出来的
	// 问题:**这次失败,包到底动过没有**。返回失败有两种:一件没碰(预检拒),和
	// "临时格已经挤掉旧物、或前几个 config 已经写进去了"。通用资产通道靠它区分
	// RETRY(可重投)与 APPLIED+partial(已经改了一半,必须记账转人工补偿)——
	// 把两者混为一谈就会重复发放或永久少发(guild-phase2.md §S4 4.33)。
	// 函数开头一律先置 false。
	static uint32_t AddItems(
		entt::entity playerEntity,
		Bag &bag,
		const PlayerItemBlockList &blockList,
		const ItemCountMap &itemsToAdd,
		TransactionType txType = TX_SYSTEM_GRANT,
		uint64_t correlationId = 0,
		const std::string &extra = {},
		bool *mutated = nullptr);

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

	// Orchestrated 按数量扣除(config_id → count),**按实际持有夹紧**:
	//   frozen check → Bag::RemoveItemsClamped → 逐条回执落 TX_ITEM_DESTROY。
	//
	// 用在"账已经在别处记好、这里只负责把数量落到背包"的场景 —— 今天唯一的
	// 生产调用方是战斗结算的道具消耗:引擎在快照副本上记的账本可能比玩家此刻
	// 真实持有的多(中途丢了/被扣了),契约是"不足按 0 处理并记日志,防刷"
	// (docs/design/turn-based-battle-server.md §5.3),所以不能用全或无的语义。
	//
	// correlationId / extra 落进流水(战斗传 battle_id 与来源 JSON),否则事后
	// 查不出这瓶药是哪一场战斗喝掉的,反刷追溯链断在这里。
	// drainedOut(可为 nullptr)按抽取顺序给出被扣的 (guid, config, 数量);
	// 调用方把它的总量与请求量一比,就知道夹紧了多少、要不要告警。
	//
	// 返回:跨 zone 冻结期返回 kInvalidParameter(一件不扣);其余恒 kSuccess。
	static uint32_t RemoveItemsClamped(
		entt::entity playerEntity,
		Bag &bag,
		const ItemCountMap &itemsToRemove,
		std::vector<DrainedInstance> *drainedOut = nullptr,
		uint64_t correlationId = 0,
		const std::string &extra = {});

	// Orchestrated 按 guid 全或无扣出(聚宝斋 P2 托管):
	//   frozen check → Bag::ReserveForBatchRemove(纯预检)→ 逐个 Bag::RemoveItem
	//   (**销毁实例**)→ 逐条落流水。
	//
	// **与 RemoveItemsClamped 语义相反,不要复用它。** 那条是按 config 夹紧、恒成功
	// (战斗结算消耗);这条是按 guid 全或无,少一件就整批拒。托管的是玩家挂出去卖的
	// 那一件具体装备,夹紧等于凭空少卖一件。
	//
	// **扣出必须销毁实例,不能留 size=0 的僵尸堆。** 托管之后权威转到交易库的快照上
	// (docs/design/jubaozhai-market.md §6.1 第 1 条),玩家 blob 里若还留着同 guid 的
	// 空壳,整理时会把它回收、跨 zone 快照会把它带走,而交易库里那份仍然存在 ——
	// 同一个 item_uuid 两处都在,过户回来就是复制。所以走 Bag::RemoveItem(销毁 +
	// 释放槽位),不走 Drain 那条"只扣数量、留着实例"的路。
	//
	// **只支持 `max_stack_size == 1` 的不可叠加物品**(由 ReserveForBatchRemove 把关):
	// 可叠加物品的预设 guid 在 AddStackableItem 里会被并堆重铸,发回来对不上。
	//
	// **不拦战斗**(D48 红线):局中拦一切背包写的闸只许放在资产 RPC 入口层。下沉到
	// 这里,战斗结算自己的扣物就会被自己拦住,结算永久卡死。
	//
	// txType 进流水(托管扣出传 TX_AUCTION_SELL),correlationId 传 listing_id。
	// removedOut(可为 nullptr)按入参顺序给出销毁前抓拍的 (guid, config, size)。
	//
	// mutated(可为 nullptr)与 AddItems 的同名出参同一用途、同一纪律:**这次失败,
	// 包到底动过没有**。函数开头一律先置 false。预检拒与冻结拒都是一件不扣
	// (mutated 保持 false,调用方按 RETRY / REJECTED 记账,不记资产变更);
	// commit 段中途失败则已有实例被销毁并落了流水,置 true —— 调用方**必须**按
	// APPLIED + partial 记账转人工补偿,不许当成零改动重投(04-asset-channel.md
	// §4.33:已改动却不记账 = 重复发放或永久少发)。
	//
	// 返回:
	//   * 跨 zone 冻结期            -> kAssetFrozen(RETRY 类,一件不扣,mutated=false)
	//   * 预检不过                  -> kAssetInvalidBundle(REJECTED 类,一件不扣,mutated=false)
	//   * commit 段中途失败         -> Bag 层原始错误码(kBagDeleteItemFindGuid 等),
	//                                 **批次已半截且无回滚**,mutated=true;预检已排除
	//                                 全部数据可触发的原因,走到这里即两层状态不自洽的
	//                                 编程错误,函数会打 ERROR。不要把它当"一件不扣"。
	//   * 其余                      -> kSuccess
	static uint32_t RemoveItemsByGuid(
		entt::entity playerEntity,
		Bag &bag,
		const std::vector<Guid> &guids,
		TransactionType txType = TX_AUCTION_SELL,
		uint64_t correlationId = 0,
		std::vector<DestroyedInstance> *removedOut = nullptr,
		bool *mutated = nullptr);

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

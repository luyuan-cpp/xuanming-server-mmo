#include "bag_service.h"

#include "engine/core/error_handling/error_handling.h"
#include "engine/core/macros/return_define.h"
#include "table/proto/tip/asset_error_tip.pb.h" // kAssetFrozen / kAssetInvalidBundle — 按 guid 扣出的错误码
#include "table/proto/tip/common_error_tip.pb.h"

#include "modules/gain_block/gain_block_service.h"
#include "modules/transaction_log/anomaly_detector.h"
#include "modules/transaction_log/transaction_log_system.h"
#include "services/scene/player/system/player_lifecycle.h" // IsCrossZoneFrozen — cross-zone-readiness-audit.md §11.1

// ---------------------------------------------------------------------------
// PlayerItemBlockList
// ---------------------------------------------------------------------------

void PlayerItemBlockList::Block(uint32_t configId)
{
	blocked_config_ids_.insert(configId);
}

void PlayerItemBlockList::Unblock(uint32_t configId)
{
	blocked_config_ids_.erase(configId);
}

bool PlayerItemBlockList::IsBlocked(uint32_t configId) const
{
	return blocked_config_ids_.contains(configId);
}

// ---------------------------------------------------------------------------
// BagService
// ---------------------------------------------------------------------------

// 一条 transaction_log 记录一次"玩家获得了 N 个 config C",item_uuid 用来标识这批数量
// **主要落在哪个实例**上。Bag::AddItem 的回执按写入顺序给出本次真实写入的实例:
// 单件装备 / 单堆物品只有一个,取它即可;并堆或跨多格时取第一个作为主实例。
//
// 回执为空 = 本次没有任何实例被写入。这在成功路径上不该发生(能返回 kSuccess 就一定写了东西),
// 所以这里返回 kInvalidGuid 而不是硬编一个看起来合法的号 —— 宁可让追溯查不到,
// 也不能把别的物品的 guid 写进流水(那正是旧的"回读上一次发号"实现干的事)。
static Guid PrimaryWrittenGuid(const std::vector<Guid> &writtenGuids)
{
	return writtenGuids.empty() ? kInvalidGuid : writtenGuids.front();
}

// 为腾位而被挤掉的实例:逐条落 LogItemDestroy。
//
// 与 MergeAndCompact 那条路径同一个理由 —— `item_uuid` 是 transaction_log 做外挂
// 回收关联的键,被挤掉的实例不留痕,追溯链就在"临时格溢出"这一步无声断掉。
//
// 一个关键区别:整理退役的实例 `size` 恒为 0(数量并进了别的堆,没有消失),
// 而这里被销毁的实例**数量是真的没了**,所以 quantity 是它当时的真实 size。
//
// **无论本次入包最终成功与否都要落。** 淘汰是一次已经提交完成的销毁,
// 它跟后面那件东西放没放进去无关;只在成功分支里记,就会漏掉"腾了位但入包
// 仍然失败"那条路径上被销毁的实例 —— 那正是最需要能查的情形。
static void LogEvictedInstances(entt::entity playerEntity,
								const std::vector<DestroyedInstance> &evicted)
{
	for (const auto &item : evicted)
	{
		TransactionLogSystem::LogItemDestroy(playerEntity, item.guid, item.configId, item.size);
	}
}

uint32_t BagService::AddItem(
	entt::entity playerEntity,
	Bag &bag,
	const PlayerItemBlockList &blockList,
	const InitItemParam &param)
{
	auto configId = param.itemPBComp.config_id();

	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// Bag is part of the player's marshaled state; once a transfer is in
	// flight any item add on the source side never reaches the destination
	// and will be lost when DestroyPlayer fires on ACK. Reject early — also
	// avoids polluting transaction_log with TX_SYSTEM_GRANT entries for items
	// that effectively don't exist.
	// See docs/design/cross-zone-readiness-audit.md §11.1.
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::AddItem rejected: player frozen for cross-zone migration. "
				 << "config_id=" << configId << " player=" << bag.PlayerGuid();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// ── Global (server-wide) block check ─────────────────────────────────
	if (GainBlockService::IsGainBlocked(GainBlockService::GainType::kItem, configId))
	{
		LOG_WARN << "BagService::AddItem: item GLOBALLY blocked. config_id="
				 << configId << " player=" << bag.PlayerGuid();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// ── Per-player GM block check ────────────────────────────────────────
	if (blockList.IsBlocked(configId))
	{
		LOG_WARN << "BagService::AddItem: item blocked by GM. config_id="
				 << configId << " player=" << bag.PlayerGuid();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// ── Delegate to pure container ───────────────────────────────────────
	// writtenGuids 是 AddItem 的显式回执:本次真实写入的实例 guid。
	// 不能再回读发号器的"上一个号"—— 同一个发号器还在铸 tx_id / snapshot_id,
	// 而且并堆 / 沿用预设 guid 的路径根本不铸号,残值属于上一件物品。
	std::vector<Guid> writtenGuids;
	// evicted:临时格满了时被先进先出挤掉的实例(其余包恒为空)。
	std::vector<DestroyedInstance> evicted;
	auto result = bag.AddItem(param, &writtenGuids, &evicted);

	// 先落淘汰流水,再落本次获得的流水 —— 顺序与实际发生顺序一致(腾位在写入
	// 之前),而且它不看 result:销毁已经提交完成了。
	LogEvictedInstances(playerEntity, evicted);

	// ── Post-success: transaction log + anomaly detection ────────────────
	if (result == kSuccess)
	{
		TransactionLogSystem::LogItemCreate(
			playerEntity, PrimaryWrittenGuid(writtenGuids),
			param.itemPBComp.config_id(),
			param.itemPBComp.size(),
			TX_SYSTEM_GRANT);

		AnomalyDetector::RecordItemGain(
			playerEntity, param.itemPBComp.config_id(),
			param.itemPBComp.size());
	}

	return result;
}

uint32_t BagService::AddItems(
	entt::entity playerEntity,
	Bag &bag,
	const PlayerItemBlockList &blockList,
	const ItemCountMap &itemsToAdd,
	TransactionType txType,
	uint64_t correlationId,
	const std::string &extra,
	bool *mutated)
{
	// 先置 false:下面每条 return 路径都可能在任意一步退出,只有开头统一清零才能
	// 保证"没写过 true 就是真的没动过"。
	if (mutated != nullptr)
	{
		*mutated = false;
	}

	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// See AddItem above for rationale. Reject the whole batch early.
	// See docs/design/cross-zone-readiness-audit.md §11.1.
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::AddItems rejected: player frozen for cross-zone migration. "
				 << "player=" << bag.PlayerGuid();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// ── Block checks first (transactional: reject whole batch if any blocked) ──
	for (const auto &[configId, count] : itemsToAdd)
	{
		if (GainBlockService::IsGainBlocked(GainBlockService::GainType::kItem, configId))
		{
			LOG_WARN << "BagService::AddItems: item GLOBALLY blocked. config_id="
					 << configId << " player=" << bag.PlayerGuid();
			return PrintStackAndReturnError(kInvalidParameter);
		}

		if (blockList.IsBlocked(configId))
		{
			LOG_WARN << "BagService::AddItems: item blocked by GM. config_id="
					 << configId << " player=" << bag.PlayerGuid();
			return PrintStackAndReturnError(kInvalidParameter);
		}
	}

	// ── All-or-nothing space check before any mutation ───────────────────
	// 用 ReserveForBatchAdd 而不是 CheckSpaceFor:后者是纯预测,会抢在淘汰之前
	// 把整批拒掉,临时格的先进先出在批量路径上就永远不生效了。
	std::vector<DestroyedInstance> evicted;
	const uint32_t reserved = bag.ReserveForBatchAdd(itemsToAdd, &evicted);
	// 腾位可能已经发生(哪怕后面整批失败),销毁必须留痕。
	LogEvictedInstances(playerEntity, evicted);
	// 淘汰即"包动过了" —— 那些实例是真的没了,后面整批失败也不会复活。
	if (mutated != nullptr && !evicted.empty())
	{
		*mutated = true;
	}
	RETURN_ON_ERROR(reserved);

	// ── Apply per config: Bag::AddItem → transaction log + anomaly ───────
	for (const auto &[configId, count] : itemsToAdd)
	{
		InitItemParam param;
		param.itemPBComp.set_config_id(configId);
		param.itemPBComp.set_size(count);

		std::vector<Guid> writtenGuids;
		// 位已经腾够,这里正常不会再淘汰;真淘汰了也要留痕(说明上面的规划漏了什么)。
		std::vector<DestroyedInstance> lateEvicted;
		auto result = bag.AddItem(param, &writtenGuids, &lateEvicted);
		LogEvictedInstances(playerEntity, lateEvicted);
		// 写出了 guid = 这一项真的落进包里了;迟到的淘汰同样是改动。
		if (mutated != nullptr && (!writtenGuids.empty() || !lateEvicted.empty()))
		{
			*mutated = true;
		}
		if (result != kSuccess)
		{
			return result;
		}

		TransactionLogSystem::LogItemCreate(
			playerEntity, PrimaryWrittenGuid(writtenGuids),
			configId, count, txType, correlationId, extra);

		AnomalyDetector::RecordItemGain(playerEntity, configId, count);
	}

	return kSuccess;
}

uint32_t BagService::AddItems(
	entt::entity playerEntity,
	Bag &bag,
	const PlayerItemBlockList &blockList,
	const std::vector<InitItemParam> &itemsToAdd,
	TransactionType txType)
{
	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// See AddItem above for rationale. Reject the whole batch early.
	// See docs/design/cross-zone-readiness-audit.md §11.1.
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::AddItems rejected: player frozen for cross-zone migration. "
				 << "player=" << bag.PlayerGuid();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// ── Block checks first (transactional: reject whole batch if any blocked) ──
	for (const auto &param : itemsToAdd)
	{
		const auto configId = param.itemPBComp.config_id();
		if (GainBlockService::IsGainBlocked(GainBlockService::GainType::kItem, configId))
		{
			LOG_WARN << "BagService::AddItems: item GLOBALLY blocked. config_id="
					 << configId << " player=" << bag.PlayerGuid();
			return PrintStackAndReturnError(kInvalidParameter);
		}

		if (blockList.IsBlocked(configId))
		{
			LOG_WARN << "BagService::AddItems: item blocked by GM. config_id="
					 << configId << " player=" << bag.PlayerGuid();
			return PrintStackAndReturnError(kInvalidParameter);
		}
	}

	// ── All-or-nothing space pre-check before any mutation ───────────────
	// 走 **vector 重载**,不要自己汇总成 ItemCountMap 再调 —— 那样会丢掉"预设 guid
	// 撞车"这道预检(它需要看到每一件的 guid),邮件附件重放时临时格会先挤掉旧物、
	// 再在写到一半时撞 guid 失败。全部纯预检都在这个入口里、在腾位之前。
	std::vector<DestroyedInstance> evicted;
	const uint32_t reserved = bag.ReserveForBatchAdd(itemsToAdd, &evicted);
	LogEvictedInstances(playerEntity, evicted);
	RETURN_ON_ERROR(reserved);

	// ── Apply per piece, interleaving the transaction log ────────────────
	// 逐件添加,每件拿自己的写入回执,于是一条流水对应一件物品。
	// 回执已经覆盖"沿用预设 guid"的情形(AddNonStackableItem 对沿用与新铸一视同仁地记录),
	// 所以这里不必再单独判断 preassignedGuid —— 那个分支正是旧实现为了绕开残值不准而打的补丁。
	for (const auto &param : itemsToAdd)
	{
		std::vector<Guid> writtenGuids;
		std::vector<DestroyedInstance> lateEvicted;
		auto result = bag.AddItem(param, &writtenGuids, &lateEvicted);
		LogEvictedInstances(playerEntity, lateEvicted);
		if (result != kSuccess)
		{
			return result;
		}

		TransactionLogSystem::LogItemCreate(
			playerEntity, PrimaryWrittenGuid(writtenGuids),
			param.itemPBComp.config_id(),
			param.itemPBComp.size(),
			txType);

		AnomalyDetector::RecordItemGain(
			playerEntity, param.itemPBComp.config_id(),
			param.itemPBComp.size());
	}

	return kSuccess;
}

uint32_t BagService::RemoveItem(
	entt::entity playerEntity,
	Bag &bag,
	Guid guid)
{
	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// See AddItem above for rationale. Removing an item from the source-
	// side bag while a transfer is in flight would leave the destination
	// with a duplicate (the marshaled PlayerAllData still contains the item)
	// — worse than rejecting the remove.
	// See docs/design/cross-zone-readiness-audit.md §11.1.
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::RemoveItem rejected: player frozen for cross-zone migration. "
				 << "guid=" << guid << " player=" << bag.PlayerGuid();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// Capture item info for transaction log before destroying.
	auto *itemComp = bag.GetItemCompByGuid(guid);
	uint32_t capturedConfigId = 0;
	uint32_t capturedSize = 0;
	if (itemComp != nullptr)
	{
		capturedConfigId = itemComp->config_id();
		capturedSize = itemComp->size();
	}

	auto result = bag.RemoveItem(guid);

	if (result == kSuccess && capturedConfigId > 0)
	{
		TransactionLogSystem::LogItemDestroy(
			playerEntity, guid,
			capturedConfigId, capturedSize);
	}

	return result;
}

uint32_t BagService::RemoveItemsClamped(
	entt::entity playerEntity,
	Bag &bag,
	const ItemCountMap &itemsToRemove,
	std::vector<DrainedInstance> *drainedOut,
	uint64_t correlationId,
	const std::string &extra)
{
	// 冻结期一件都不扣:理由同 RemoveItem —— 迁移在途时源端扣数,目标端拿到的
	// 快照里那笔数量还在,等于凭空多出来。调用方必须把这个返回值当"没扣成",
	// 而不是"扣了 0 个"。
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::RemoveItemsClamped rejected: player frozen for cross-zone "
				 << "migration. player=" << bag.PlayerGuid()
				 << " configs=" << itemsToRemove.size();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	std::vector<DrainedInstance> drained;
	const auto result = bag.RemoveItemsClamped(itemsToRemove, &drained);

	// 逐条落流水:一条回执一个 item_uuid。这里不能像入包那样只记第一个 guid ——
	// 一笔消耗可能横跨好几堆,少记一条就是少一条追溯线索。
	for (const auto &entry : drained)
	{
		TransactionLogSystem::LogItemDestroy(
			playerEntity, entry.guid, entry.configId, entry.amount, correlationId, extra);
	}

	if (drainedOut != nullptr)
	{
		drainedOut->insert(drainedOut->end(), drained.begin(), drained.end());
	}
	return result;
}

uint32_t BagService::RemoveItemsByGuid(
	entt::entity playerEntity,
	Bag &bag,
	const std::vector<Guid> &guids,
	TransactionType txType,
	uint64_t correlationId,
	std::vector<DestroyedInstance> *removedOut)
{
	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// 理由同 RemoveItem:迁移在途时源端把实例销毁,目标端拿到的快照里那件东西还在,
	// 等于凭空多出来一件;而交易库那侧已经按"扣出成功"入了托管快照 —— 一件装备
	// 两处都在,过户回来就是复制。
	//
	// 这里回 kAssetFrozen 而不是兄弟函数用的 kInvalidParameter:冻结是**会自己消失**
	// 的条件,调用方(资产通道)据此回 RETRY 重投;回 kInvalidParameter 会被当成
	// 终局拒绝,玩家过图的那几百毫秒里发起的寄售就被永久拒掉了。本函数是新增的,
	// 没有既有调用点会因这个取值变化而受影响。
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::RemoveItemsByGuid rejected: player frozen for cross-zone "
				 << "migration. player=" << bag.PlayerGuid() << " guids=" << guids.size();
		return PrintStackAndReturnError(kAssetFrozen);
	}

	// ⚠ 这里**刻意不判 InBattleComp**(D48 红线)。"局中挡一切背包写"的闸只许放在
	// 资产 RPC 入口层;下沉到 BagService,战斗结算自己的扣物就会被自己拦住,结算
	// 永久卡死。局中不许寄售这条业务规则由调用方(交易会话的资产 RPC 入口)负责。

	// ── reserve:纯预检,零副作用。过了这一关,下面的销毁不会再失败 ─────────
	std::vector<DestroyedInstance> removable;
	RETURN_ON_ERROR(bag.ReserveForBatchRemove(guids, &removable));

	// ── commit:逐个销毁实例 + 当场落流水 ────────────────────────────────
	// 销毁一件就记一件,而不是全部销毁完再统一记:万一中途真的失败了(预检已经
	// 排除了所有数据可触发的原因,只剩编程错误),已经消失的那几件仍然留得下
	// item_uuid —— 那正是最需要能查的情形。
	for (const auto &item : removable)
	{
		const uint32_t result = bag.RemoveItem(item.guid);
		if (result != kSuccess)
		{
			// 预检刚刚确认过每个 guid 都在包里且互不重复,走到这里只可能是两层
			// 状态不自洽的编程错误。此刻批次已经半截,没有回滚手段,必须吼出来。
			LOG_ERROR << "BagService::RemoveItemsByGuid: RemoveItem failed for guid " << item.guid
					  << " (config " << item.configId << ") right after ReserveForBatchRemove "
					  << "succeeded (programming error); the batch is now partially removed. "
					  << "player=" << bag.PlayerGuid() << " correlation_id=" << correlationId;
			return result;
		}

		TransactionLogSystem::LogItemDestroy(
			playerEntity, item.guid, item.configId, item.size, correlationId, {}, txType);

		if (removedOut != nullptr)
		{
			removedOut->push_back(item);
		}
	}

	return kSuccess;
}

uint32_t BagService::MergeAndCompact(
	entt::entity playerEntity,
	Bag &bag,
	CompactPolicy policy,
	bool *changedOut)
{
	if (changedOut != nullptr)
	{
		*changedOut = false;
	}

	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// 整理会销毁实例、重排槽位,两样都是会被 marshal 带走的状态。传输在途时动它
	// 等于在源端造出一份与已发出快照不一致的布局,ACK 后 DestroyPlayer 一到,
	// 这次整理就白做了(更糟的是流水已经写了)。理由同 AddItem。
	// See docs/design/cross-zone-readiness-audit.md §11.1.
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::MergeAndCompact rejected: player frozen for cross-zone migration. "
				 << "player=" << bag.PlayerGuid();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// ── Delegate to pure container ───────────────────────────────────────
	std::vector<DestroyedInstance> destroyed;
	const bool changed = bag.MergeAndCompact(&destroyed, policy);
	if (changedOut != nullptr)
	{
		*changedOut = changed;
	}

	// ── Post: transaction log for every retired instance ─────────────────
	// 数量没有凭空消失(它们被并进了别的实例),但 item_uuid 本身退役了,而那
	// 正是外挂回收做关联的键。不写这一笔,追溯链就在"整理"这一步无声断掉 ——
	// 拆分前 MergeAndCompact 根本没有编排层,这些 guid 是悄悄消失的。
	for (const auto &item : destroyed)
	{
		TransactionLogSystem::LogItemDestroy(
			playerEntity, item.guid, item.configId, item.size);
	}

	return kSuccess;
}

uint32_t BagService::SortByPlayerRequest(
	entt::entity playerEntity,
	Bag &bag,
	bool *changedOut)
{
	// 玩家点了"整理"按钮 —— 这是唯一被授权重排位置的路径。
	// 编排(冻结检查 + 退役实例写流水)全在 MergeAndCompact 里,这里只负责
	// 把"这是玩家的显式意图"这件事翻译成 policy,不重复任何逻辑。
	return MergeAndCompact(playerEntity, bag, CompactPolicy::kMergeAndReorder, changedOut);
}

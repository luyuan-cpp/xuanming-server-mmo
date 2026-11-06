#include "bag_service.h"

#include <algorithm>
#include <limits>
#include <optional>
#include <tuple>
#include <type_traits>
#include <typeindex>
#include <utility>

#include "engine/core/error_handling/error_handling.h"
#include "engine/core/macros/return_define.h"
#include "table/proto/tip/asset_error_tip.pb.h" // kAssetFrozen / kAssetInvalidBundle — 按 guid 扣出的错误码
#include "table/proto/tip/bag_error_tip.pb.h"   // kBagItemNotStacked — 两包都放不下(结算日志限频)
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/item_table.h"

#include "modules/gain_block/gain_block_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "thread_context/ecs_context.h"
#include "modules/transaction_log/anomaly_detector.h"
#include "modules/transaction_log/transaction_log_system.h"
#include "services/scene/player/system/player_lifecycle.h" // IsCrossZoneFrozen — cross-zone-readiness-audit.md §11.1

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

// 发放的流水。不可叠加物品一件一条:item_uuid 是回收和追溯的键,只记第一件的话其余几件无从点名,回收也动不了它们。
// 可叠加物品的 guid 会随并堆变,记一条总量。
static void LogItemsCreated(entt::entity playerEntity, const std::vector<Guid> &writtenGuids, uint32_t configId, uint32_t count,
	TransactionType txType, uint64_t correlationId = 0, const std::string &extra = {})
{
	const auto *row = ItemTableManager::Instance().FindByIdSilent(configId).first;
	if (row != nullptr && row->max_stack_size() == 1 && writtenGuids.size() == count)
	{
		for (const auto guid : writtenGuids)
		{
			TransactionLogSystem::LogItemCreate(playerEntity, guid, configId, 1, txType, correlationId, extra);
		}
		return;
	}
	TransactionLogSystem::LogItemCreate(playerEntity, PrimaryWrittenGuid(writtenGuids), configId, count, txType, correlationId, extra);
}

// 为腾位而被挤掉的实例:逐条落 LogItemDestroy。
//
// 与 MergeAndCompact 那条路径同一个理由 —— `item_uuid` 是 transaction_log 做外挂
// 回收关联的键,被挤掉的实例不留痕,追溯链就在"临时格溢出"这一步无声断掉。
//
// 一个关键区别:整理退役的实例 `size` 恒为 0(数量并进了别的堆,没有消失),
// 而这里被销毁的实例**数量是真的没了**,所以 quantity 是它当时的真实 size。
//
// 容器成功提交后才会给出淘汰回执；拒绝的调用保持原包不变，也不产生销毁流水。
static void LogEvictedInstances(entt::entity playerEntity,
								const std::vector<DestroyedInstance> &evicted)
{
	for (const auto &item : evicted)
	{
		TransactionLogSystem::LogItemDestroy(playerEntity, item.guid, item.configId, item.size);
	}
}

// 进程级的「新铸实例初始化回调」槽位。
//
// 做成函数内静态而不是文件级变量:安装方可能在别的编译单元的静态初始化期就调
// SetItemInstanceInitializer,函数内静态保证那时槽位已经构造好。
// 不带锁 —— 约定是启动期装一次、此后只读(见 bag_service.h)。
static ItemInstanceInitializer &ItemInstanceInitializerSlot()
{
	static ItemInstanceInitializer slot;
	return slot;
}

// 传给 Bag::AddItem 的那个参数:没装就是 nullptr,Bag 侧一个分支都不用多判。
//
// 三个服务入口都把同一个安装回调显式交给容器的统一候选提交，不另造初始化循环。
static const ItemInstanceInitializer *InstalledItemInstanceInitializer()
{
	const ItemInstanceInitializer &slot = ItemInstanceInitializerSlot();
	return slot ? &slot : nullptr;
}

void BagService::SetItemInstanceInitializer(ItemInstanceInitializer initializer)
{
	ItemInstanceInitializerSlot() = std::move(initializer);
}

uint32_t BagService::AddItem(
	entt::entity playerEntity,
	Bag &bag,
	const InitItemParam &param)
{
	auto configId = param.itemPBComp.config_id();

	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// 冻结 = 归属交接在途(PlayerLifecycleSystem::StartTravelHandoff:跨 zone 传送,或同 zone
	// 跨节点换图);PlayerFrozenComp 只与 PlayerTravelHandoffComp 成对出现,已没有 Kafka
	// 迁移包 / ACK / reaper。背包随冻结那一次存盘落盘,目标节点从盘上加载;此后源端再加的
	// 物品不会再被存盘(handoff 标记写出后 SavePlayer 直接跳过),放行后随
	// DestroyDeposedPlayer 不存盘销毁。Reject early — also
	// avoids polluting transaction_log with TX_SYSTEM_GRANT entries for items
	// that effectively don't exist.
	// See docs/design/cross-zone-readiness-audit.md §11.1.
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::AddItem rejected: player frozen for cross-zone migration. "
				 << "config_id=" << configId << " player=" << bag.PlayerGuid();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// ── 物品获得封禁(全服 + 本人)───────────────────────────────────────
	// 业务拒绝,不是编程错误:回 kAssetBlocked、不打调用栈,调用方据此与「背包满」区分开,不得改投临时格。
	if (GainBlockService::IsItemGainBlocked(playerEntity, configId))
	{
		LOG_WARN << "BagService::AddItem: item gain blocked (global or this player). config_id="
				 << configId << " player=" << bag.PlayerGuid();
		return kAssetBlocked;
	}

	// ── Delegate to pure container ───────────────────────────────────────
	// writtenGuids 是 AddItem 的显式回执:本次真实写入的实例 guid。
	// 不能再回读发号器的"上一个号"—— 同一个发号器还在铸 tx_id / snapshot_id,
	// 而且并堆 / 沿用预设 guid 的路径根本不铸号,残值属于上一件物品。
	std::vector<Guid> writtenGuids;
	// evicted:配了淘汰策略的包为腾位挤掉的实例(2026-11-05 起四个固定包都满即拒,生产路径恒为空)。
	std::vector<DestroyedInstance> evicted;
	// 三个服务入口都向同一候选提交传入装备初始化回调。
	auto result = bag.AddItem(param, &writtenGuids, &evicted, InstalledItemInstanceInitializer());

	// 成功后先记淘汰、再记获得；失败没有淘汰回执，不产生资产流水。
	LogEvictedInstances(playerEntity, evicted);

	// ── Post-success: transaction log + anomaly detection ────────────────
	if (result == kSuccess)
	{
		LogItemsCreated(playerEntity, writtenGuids, param.itemPBComp.config_id(), param.itemPBComp.size(), TX_SYSTEM_GRANT);

		AnomalyDetector::RecordItemGain(
			playerEntity, param.itemPBComp.config_id(),
			param.itemPBComp.size());
	}

	return result;
}

uint32_t BagService::AddItems(
	entt::entity playerEntity,
	Bag &bag,
	const ItemCountMap &itemsToAdd,
	TransactionType txType,
	uint64_t correlationId,
	const std::string &extra)
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

	// 同一批按配置号升序预检、占格和记流水,不依赖请求哈希表的历史插入顺序。
	// 任一项被封禁(全服或本人)整批拒绝,在容器做任何规划之前:一件不发,不留半笔。
	const auto ordered = SortedByConfigId(itemsToAdd);
	for (const auto &[configId, count] : ordered)
	{
		if (GainBlockService::IsItemGainBlocked(playerEntity, configId))
		{
			LOG_WARN << "BagService::AddItems: item gain blocked (global or this player), whole batch rejected. config_id="
					 << configId << " player=" << bag.PlayerGuid();
			return kAssetBlocked;
		}
	}

    // 容器一次性提交整批，返回每项真实去处；失败不淘汰、不合堆，也不落任何资产流水。
    std::vector<DestroyedInstance> evicted;
    std::vector<GuidVector> written;
    RETURN_ON_ERROR(bag.AddItems(itemsToAdd, &evicted, &written, InstalledItemInstanceInitializer()));
    LogEvictedInstances(playerEntity, evicted);
    for (std::size_t i = 0; i < ordered.size(); ++i)
    {
        const auto &[configId, count] = ordered[i];
        LogItemsCreated(playerEntity, written[i], configId, count, txType, correlationId, extra);
        AnomalyDetector::RecordItemGain(playerEntity, configId, count);
    }

	return kSuccess;
}

namespace
{
    // 候选只借用原只读策略；不持有源Bag，也不把这两个适配器暴露给调用方。
    class BorrowedAdmission final : public IAdmissionPolicy
    {
    public:
        explicit BorrowedAdmission(const IAdmissionPolicy& source) : source_(source) {}
        bool Accepts(uint32_t config) const override { return source_.Accepts(config); }
    private:
        const IAdmissionPolicy& source_;
    };
    class BorrowedEviction final : public IEvictionPolicy
    {
    public:
        explicit BorrowedEviction(const IEvictionPolicy& source) : source_(source) {}
        GuidVector SelectVictims(std::size_t needed, const ItemStore& store, const IContainerLayout& layout) const override
        { return source_.SelectVictims(needed, store, layout); }
    private:
        const IEvictionPolicy& source_;
    };

    // 两包都放不下而整批被拒的连续次数(按需状态,docs/design/ecs-component-access-rules.md):
    //   * 只在本文件读写;不入库、不跨进程,随实体销毁(重登 / 换节点之后从 0 数起);
    //   * 唯一创建入口 NoteGrantOverflowRejected(第一次因两包都放不下而整批拒绝时);这名玩家的下一笔
    //     整批发放成功时摘掉,下一段卡住又从「首次」打起;
    //   * 只用于 metric=bag_grant_overflow_rejected 限频,以及战斗结算执行器给收件箱的 DEFER 原因分类(这一次调用里它涨了
    //     = 两包都放不下,PlayerBattleSystem::ApplyInboxSettlement),不改变任何发放 / 结算结果。
    //     状态挂在玩家实体上,没有进程内跨玩家的状态。
    struct GrantOverflowRejectStreakComp
    {
        uint32_t rejections{0};
    };

    // 两包都放不下(Bag::AddItems 静默回 kBagItemNotStacked)时记一次:按 BagService::IsGrantOverflowWarnDue 的节拍打 WARN,
    // 其余打 DEBUG。不带调用栈:这是玩家自己能造出来、能维持很久的状态,不是故障。
    void NoteGrantOverflowRejected(entt::entity player, uint64_t playerId, uint64_t correlationId, const Bag& temporary)
    {
        auto& streak = tlsEcs.actorRegistry.get_or_emplace<GrantOverflowRejectStreakComp>(player);
        if (streak.rejections < std::numeric_limits<uint32_t>::max()) ++streak.rejections;
        if (BagService::IsGrantOverflowWarnDue(streak.rejections))
        {
            LOG_WARN << "metric=bag_grant_overflow_rejected player_id=" << playerId << " correlation_id=" << correlationId
                     << " consecutive_rejections=" << streak.rejections
                     << " temporary_used=" << temporary.OccupiedGridCount() << "/" << temporary.Capacity()
                     << ";the inventory and the temporary bag both refused the whole grant (temporary bag never evicts), nothing applied;"
                     << " logged on the first rejection and then every " << BagService::kGrantOverflowWarnEveryRejections
                     << " consecutive rejections";
        }
        else
        {
            LOG_DEBUG << "[BagService] grant refused again, both bags still full: player_id=" << playerId
                      << " correlation_id=" << correlationId << " consecutive_rejections=" << streak.rejections;
        }
    }
}

uint32_t BagService::ConsumeInventoryAndGrant(entt::entity playerEntity,
    const ItemCountMap& consumed, const ItemCountMap& gained, uint64_t correlationId,
    const std::string& extra, const std::function<uint32_t()>& beforeCommit)
{
    if (!tlsEcs.actorRegistry.valid(playerEntity) || !PlayerLifecycleSystem::IsPlayerMutable(playerEntity)) return kInvalidParameter;
    const auto playerId = tlsEcs.GetPlayerId(playerEntity);
    if (playerId == 0 || tlsEcs.GetPlayer(playerId) != playerEntity) return kInvalidParameter;
    for (const auto* batch : {&consumed, &gained})
        for (const auto& [config, count] : *batch)
            if (config == 0 || count == 0 || ItemTableManager::Instance().FindByIdSilent(config).first == nullptr)
                return kInvalidParameter;
    // 物品获得封禁(全服 + 本人)在扣消耗、建候选、回调之前判:任一项被封整笔拒绝,不扣材料、不发半笔,
    // 也不会因「主包放不下」改投临时格(封禁不是容量不足)。提交前再复核一次。
    // entt::entity 用 init-capture(MSVC C3495,见 .github/copilot-instructions.md)。
    const auto firstBlockedGain = [player = playerEntity, &gained]() -> std::optional<uint32_t> {
        for (const auto& entry : gained)
            if (GainBlockService::IsItemGainBlocked(player, entry.first)) return entry.first;
        return std::nullopt;
    };
    if (const auto blocked = firstBlockedGain(); blocked.has_value()) {
        LOG_WARN << "BagService::ConsumeInventoryAndGrant: item gain blocked (global or this player), whole grant rejected"
                 << " before consuming anything. player_id=" << playerId << " config_id=" << *blocked
                 << " correlation_id=" << correlationId;
        return kAssetBlocked;
    }
    if (consumed.empty() && gained.empty()) return beforeCommit ? beforeCommit() : uint32_t{kSuccess};
    auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(playerEntity);
    if (bags == nullptr) return kInvalidParameter;
    auto& inventory = bags->bags[kInventory];
    auto& temporary = bags->bags[kTemporary];
    if (!inventory.IsLayerConsistent() || !temporary.IsLayerConsistent()) return kInvalidParameter;

    const auto fingerprint = [](const Bag& bag) {
        std::vector<std::pair<Guid, std::string>> items;
        bag.store_.ForEach([&items](Guid id, const ItemComp& item) { items.emplace_back(id, item.SerializeAsString()); });
        std::sort(items.begin(), items.end());
        std::vector<std::pair<uint32_t, Guid>> slots(bag.layout_->Slots().begin(), bag.layout_->Slots().end());
        std::sort(slots.begin(), slots.end());
        const auto* grid = dynamic_cast<const GridLayout*>(bag.layout_.get());
        return std::tuple{std::move(items), std::move(slots), bag.store_.NextAcquireSeq(), bag.Capacity(),
            bag.layout_.get(), std::type_index(typeid(*bag.layout_)), grid ? grid->Width() : 0u,
            grid ? grid->Height() : 0u, bag.layout_->FreeCells(),
            bag.ProfileId(), bag.PlayerGuid(), bag.admission_.get(), bag.eviction_.get()};
    };
    const auto inventoryBefore = fingerprint(inventory), temporaryBefore = fingerprint(temporary);
    const auto sameSource = [&] {
        if (!tlsEcs.actorRegistry.valid(playerEntity) || tlsEcs.GetPlayer(playerId) != playerEntity
            || tlsEcs.GetPlayerId(playerEntity) != playerId || !PlayerLifecycleSystem::IsPlayerMutable(playerEntity)) return false;
        const auto* current = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(playerEntity);
        return current == bags && fingerprint(current->bags[kInventory]) == inventoryBefore
            && fingerprint(current->bags[kTemporary]) == temporaryBefore;
    };
    const auto clone = [](const Bag& source, Bag& candidate) {
        candidate.store_ = source.store_;
        candidate.layout_ = source.layout_->Clone();
        candidate.admission_ = std::make_unique<BorrowedAdmission>(source.Admission());
        candidate.eviction_ = std::make_unique<BorrowedEviction>(source.Eviction());
        candidate.profileId_ = source.profileId_; candidate.playerGuid = source.playerGuid;
        return candidate.layout_ != nullptr;
    };
    Bag nextInventory, nextTemporary;
    if (!clone(inventory, nextInventory) || !clone(temporary, nextTemporary)) return kInvalidParameter;
    std::vector<DrainedInstance> drained;
    if (!consumed.empty()) {
        const auto result = nextInventory.RemoveItemsClamped(consumed, &drained);
        if (result != kSuccess) return result;
    }
    std::vector<DestroyedInstance> evicted;
    std::vector<GuidVector> written;
    if (!gained.empty()) {
        auto result = nextInventory.AddItems(gained, &evicted, &written, InstalledItemInstanceInitializer());
        if (result != kSuccess) {
            if (!sameSource()) return kInvalidParameter;
            evicted.clear(); written.clear();
            result = nextTemporary.AddItems(gained, &evicted, &written, InstalledItemInstanceInitializer());
            // 临时格满了不淘汰(2026-11-05 起,审计 cpp-economy#2):整笔在任何副作用之前拒绝,候选一起丢掉,
            // 消耗、beforeCommit(货币)与两个包都没动;来源保留(战斗结算整笔延后,玩家取回腾位后重试即应用)。
            // 两包都放不下是玩家能造出来的状态,按玩家限频打 WARN(首次,之后每 kGrantOverflowWarnEveryRejections 次);
            // 其余失败码是故障,Bag::AddItems 里已按 ERROR 打过,这里不再记。
            if (result == kBagItemNotStacked)
                NoteGrantOverflowRejected(playerEntity, playerId, correlationId, temporary);
        }
        if (result != kSuccess) return result;
        if (written.size() != gained.size()) return kInvalidParameter;
    }
    if (!sameSource()) return kInvalidParameter;
    if (firstBlockedGain().has_value()) return kAssetBlocked;
    if (beforeCommit) {
        const auto result = beforeCommit();
        if (result != kSuccess) return result;
    }
    // 原策略不移交，候选从未发布；所有可能拒绝的步骤都已完成。这里不回调、不yield。
    static_assert(std::is_nothrow_move_assignable_v<ItemStore>);
    inventory.store_ = std::move(nextInventory.store_); inventory.layout_ = std::move(nextInventory.layout_);
    temporary.store_ = std::move(nextTemporary.store_); temporary.layout_ = std::move(nextTemporary.layout_);
    for (const auto& entry : drained)
        TransactionLogSystem::LogItemDestroy(playerEntity, entry.guid, entry.configId, entry.amount, correlationId, extra);
    LogEvictedInstances(playerEntity, evicted);
    const auto ordered = SortedByConfigId(gained);
    for (std::size_t i = 0; i < ordered.size(); ++i) {
        const auto& [config, count] = ordered[i];
        LogItemsCreated(playerEntity, written[i], config, count, TX_ITEM_AWARD, correlationId, extra);
        AnomalyDetector::RecordItemGain(playerEntity, config, count);
    }
    // 这一笔整批发放成功:「连续放不下」到此为止,下一段卡住重新从首次打 WARN(GrantOverflowRejectStreakComp 的说明)。
    if (tlsEcs.actorRegistry.any_of<GrantOverflowRejectStreakComp>(playerEntity))
        tlsEcs.actorRegistry.remove<GrantOverflowRejectStreakComp>(playerEntity);
    return kSuccess;
}

uint32_t BagService::ConsecutiveGrantOverflowRejections(entt::entity playerEntity)
{
    if (!tlsEcs.actorRegistry.valid(playerEntity)) return 0;
    const auto* streak = tlsEcs.actorRegistry.try_get<GrantOverflowRejectStreakComp>(playerEntity);
    return streak != nullptr ? streak->rejections : 0;
}

uint64_t BagService::MaxNewInstancesFor(const ItemCountMap &itemsToAdd)
{
	uint64_t total = 0;
	for (const auto &[configId, count] : itemsToAdd)
	{
		const auto *row = ItemTableManager::Instance().FindByIdSilent(configId).first;
		if (row == nullptr || row->max_stack_size() == 0 || count == 0)
		{
			continue; // Bag::AddItems 会在铸号之前整批拒绝这类批次,见头文件
		}
		total += ItemStore::StacksNeededFor(count, row->max_stack_size());
	}
	return total;
}

uint32_t BagService::AddItems(
	entt::entity playerEntity,
	Bag &bag,
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

	// ── 封禁先判(全服或本人任一项命中就整批拒绝,容器一件不动)──────────
	for (const auto &param : itemsToAdd)
	{
		const auto configId = param.itemPBComp.config_id();
		if (GainBlockService::IsItemGainBlocked(playerEntity, configId))
		{
			LOG_WARN << "BagService::AddItems: item gain blocked (global or this player), whole batch rejected. config_id="
					 << configId << " player=" << bag.PlayerGuid();
			return kAssetBlocked;
		}
	}

    // 完整实例按输入顺序提交；预设身份、equip presence 与每项流水关联原样保留。
    std::vector<DestroyedInstance> evicted;
    std::vector<GuidVector> written;
    RETURN_ON_ERROR(bag.AddItems(itemsToAdd, &evicted, &written, InstalledItemInstanceInitializer()));
    LogEvictedInstances(playerEntity, evicted);
    for (std::size_t i = 0; i < itemsToAdd.size(); ++i)
    {
        const auto &item = itemsToAdd[i].itemPBComp;
        LogItemsCreated(playerEntity, written[i], item.config_id(), item.size(), txType);
        AnomalyDetector::RecordItemGain(playerEntity, item.config_id(), item.size());
    }

	return kSuccess;
}

uint32_t BagService::RemoveItem(
	entt::entity playerEntity,
	Bag &bag,
	Guid guid)
{
	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// See AddItem above for rationale. 归属交接在途时从源端背包删物品,目标节点从盘上
	// 加载到的仍是冻结那一次存盘(里面还有这件物品)—— 等于多出一件,
	// 比拒绝这次删除更糟。
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
	// 冻结期一件都不扣:理由同 RemoveItem —— 归属交接在途时源端扣数,目标节点从盘上加载到的
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

uint32_t BagService::RemoveItemsExact(
	entt::entity playerEntity,
	Bag &bag,
	const ItemCountMap &itemsToRemove,
	TransactionType txType,
	uint64_t correlationId,
	const std::string &extra)
{
	// ── 参数检查:排在冻结闸之前 ─────────────────────────────────────────
	// 空表在底层是"什么都不用扣 = 成功",调用方会把一次什么都没扣的调用当成扣过了。
	if (itemsToRemove.empty())
	{
		LOG_ERROR << "BagService::RemoveItemsExact: empty request (programming error); "
				  << "nothing removed. player=" << bag.PlayerGuid()
				  << " tx_type=" << static_cast<int>(txType)
				  << " correlation_id=" << correlationId;
		return PrintStackAndReturnError(kInvalidParameter);
	}
	// 0 数量的条目是编程错误:没有可扣的东西,却会进预检、回执与流水。底层预检
	// (ItemStore::CheckQuantities)对 0 需求恒判满足,不会替调用方拦它;入包侧为同一类问题
	// 加过前置拒绝(Bag::AddItems 的零数量预检),这里同理:整批拒,不替调用方猜它想扣什么。
	for (const auto &[configId, count] : itemsToRemove)
	{
		if (count == 0)
		{
			LOG_ERROR << "BagService::RemoveItemsExact: zero count for config " << configId
					  << " (programming error: skip the entry instead); nothing removed. player="
					  << bag.PlayerGuid() << " tx_type=" << static_cast<int>(txType)
					  << " correlation_id=" << correlationId;
			return PrintStackAndReturnError(kInvalidParameter);
		}
	}

	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// 冻结期一件都不扣,理由同 RemoveItem:归属交接在途时源端扣数,目标节点从盘上加载到的
	// 快照里那笔数量还在,等于凭空多出来。玩法入口(PlayerEquipSystem 的写入口)会先挡掉
	// 冻结,正常走不到这里;留着它是因为本函数不能假设每个调用方都记得挡。
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::RemoveItemsExact rejected: player frozen for cross-zone "
				 << "migration. player=" << bag.PlayerGuid()
				 << " configs=" << itemsToRemove.size();
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// ⚠ 这里**刻意不判 InBattleComp**(D48 红线),理由同 RemoveItemsByGuid:"局中挡一切背包写"
	// 的闸只许放在玩法入口层。强化 / 镶嵌的入口自己判
	// (docs/design/equipment-enhance-inlay.md §5 不变量 5)。

	// ── 全或无扣除:任何一种不够就整体失败,一件不扣 ─────────────────────
	// 不够时 Bag 层已经打过 ERROR 与调用栈;drained 此时保持为空,下面不会落任何流水。
	std::vector<DrainedInstance> drained;
	RETURN_ON_ERROR(bag.RemoveItems(itemsToRemove, &drained));

	// 逐条落流水,理由同 RemoveItemsClamped:一笔消耗可能横跨好几堆,少记一条就是少一条
	// 追溯线索。注意 LogItemDestroy 的 txType 在**最后**(与 LogItemCreate 的顺序不同),
	// 按 (correlationId, extra, txType) 传,别照着入包那边的顺序抄。
	//
    // 被抽光的实例及其槽位已经释放，流水仍以扣除回执中的原 guid 关联；其余格位不重排。
	for (const auto &entry : drained)
	{
		TransactionLogSystem::LogItemDestroy(
			playerEntity, entry.guid, entry.configId, entry.amount, correlationId, extra, txType);
	}

	return kSuccess;
}

uint32_t BagService::RemoveItemsByGuid(
	entt::entity playerEntity,
	Bag &bag,
	const std::vector<Guid> &guids,
	TransactionType txType,
	uint64_t correlationId,
	std::vector<DestroyedInstance> *removedOut,
	bool *mutated)
{
	// 与 AddItems 同一纪律:先一律置 false,调用方据此区分"一件没碰"(RETRY /
	// REJECTED,不记资产变更)与"已经销毁了几件"(必须记 APPLIED + partial)。
	if (mutated != nullptr)
	{
		*mutated = false;
	}

	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// 理由同 RemoveItem:归属交接在途时源端把实例销毁,目标节点从盘上加载到的快照里那件东西还在,
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
	std::size_t removedCount = 0;
	for (const auto &item : removable)
	{
		const uint32_t result = bag.RemoveItem(item.guid);
		if (result != kSuccess)
		{
			// 预检刚刚确认过每个 guid 都在包里且互不重复,走到这里只可能是两层
			// 状态不自洽的编程错误。此刻批次已经半截,没有回滚手段,必须吼出来。
			//
			// **返回的既不是 kAssetFrozen 也不是 kAssetInvalidBundle**,而 mutated 已经
			// 在前几轮置 true(第一件就失败时仍为 false = 确实一件没碰)。调用方必须
			// 靠 mutated 而不是返回值来判断要不要记账:把半截批次当成"一件不扣"去
			// RETRY,玩家那几件装备就永久消失且账本上不留痕(04-asset-channel.md §4.33)。
			LOG_ERROR << "BagService::RemoveItemsByGuid: RemoveItem failed for guid " << item.guid
					  << " (config " << item.configId << ") right after ReserveForBatchRemove "
					  << "succeeded (programming error); the batch is now partially removed. "
					  << "player=" << bag.PlayerGuid() << " correlation_id=" << correlationId
					  << " removed_before_failure=" << removedCount;
			return result;
		}

		// 实例已经没了 —— 从这一刻起这次调用就不再是"零改动"。置位必须在落流水
		// 之前,流水投递失败不会让实例复活。
		++removedCount;
		if (mutated != nullptr)
		{
			*mutated = true;
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
	// 整理会销毁实例、重排槽位,两样都是要随存盘落盘的状态。归属交接在途时动它
	// 等于在源端造出一份与盘上快照(冻结那一次存盘)不一致的布局,放行后源端实体由
	// DestroyDeposedPlayer 不存盘销毁,这次整理就白做了(更糟的是流水已经写了)。理由同 AddItem。
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

uint32_t BagService::RetrieveFromTemporary(
	entt::entity playerEntity,
	const std::vector<Guid> &guids)
{
	// ── Cross-zone Frozen check (Single Writer guarantee) ────────────────
	// 搬运改的是两个要随存盘落盘的包。归属交接在途时动它,放行后源端实体不存盘销毁,这次搬运就白做了;
	// 理由同 MergeAndCompact。玩法入口已先判过、回了玩家看得懂的码,这里是本函数不依赖调用方的那一道。
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(playerEntity))
	{
		LOG_WARN << "BagService::RetrieveFromTemporary rejected: player frozen for cross-zone migration. player_id="
				 << tlsEcs.GetPlayerId(playerEntity);
		return PrintStackAndReturnError(kInvalidParameter);
	}
	auto *bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(playerEntity);
	if (bags == nullptr)
	{
		LOG_ERROR << "BagService::RetrieveFromTemporary: player has no bags component. player_id="
				  << tlsEcs.GetPlayerId(playerEntity);
		return PrintStackAndReturnError(kInvalidParameter);
	}
	Bag &temporary = bags->bags[kTemporary];
	Bag &inventory = bags->bags[kInventory];
	if (!temporary.IsLayerConsistent() || !inventory.IsLayerConsistent())
	{
		LOG_ERROR << "BagService::RetrieveFromTemporary: bag layers are inconsistent; nothing moved. player_id="
				  << tlsEcs.GetPlayerId(playerEntity);
		return PrintStackAndReturnError(kInvalidParameter);
	}

	// ── 两包候选上全或无地搬,失败两包都不动、不落任何流水 ────────────────
	std::vector<DestroyedInstance> absorbed;
	RETURN_ON_ERROR(temporary.MoveInstancesTo(inventory, guids, &absorbed));

	// ── 提交之后:只有整堆并进别的堆的原 guid 退役,落数量 0 的销毁流水(整理同口径) ──
	// 其余实例的身份没变,只是换了个包:流水本来就不记包位置,不写一对伪造的「销毁 + 获得」。
	for (const auto &item : absorbed)
	{
		TransactionLogSystem::LogItemDestroy(playerEntity, item.guid, item.configId, item.size, 0,
											 "{\"source\":\"temporary-retrieve\"}");
	}
	LOG_INFO << "BagService::RetrieveFromTemporary: moved " << guids.size() << " instance(s) from the temporary bag"
			 << " into the inventory, " << absorbed.size() << " merged into existing stacks. player_id="
			 << tlsEcs.GetPlayerId(playerEntity);
	return kSuccess;
}

#include "bag_system.h"

#include <limits>

#include <algorithm>
#include <unordered_set>
#include <utility>
#include <vector>

#include "engine/core/error_handling/error_handling.h"
#include "engine/core/macros/return_define.h"
#include "table/proto/tip/asset_error_tip.pb.h" // kAssetInvalidBundle — 按 guid 扣出的 REJECTED 类码
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/code/item_table.h"
#include "table/code/equipslot_table.h"

std::vector<std::pair<uint32_t, uint32_t>> SortedByConfigId(const ItemCountMap &counts)
{
	std::vector<std::pair<uint32_t, uint32_t>> ordered(counts.begin(), counts.end());
	std::sort(ordered.begin(), ordered.end(), [](const auto &lhs, const auto &rhs)
			  { return lhs.first < rhs.first; });
	return ordered;
}

// Bag 是桥层。凡是只需要一层就能回答的问题,这里都不实现,直接转给
// store_(实例层)或 layout_(布局层);留在本文件里的,全是**同时需要
// 两层**才成立的玩法规则。判断一段代码该不该待在这里,就看它是不是两层都碰。

// ── 布局策略 ─────────────────────────────────────────────────────────────

void Bag::SetLayout(std::unique_ptr<IContainerLayout> layout)
{
	if (layout == nullptr)
	{
		LOG_ERROR << "Bag::SetLayout: null layout rejected, player " << PlayerGuid();
		return;
	}
	// 槽位号在不同策略下含义不同(扁平是下标、格子是 y*width+x),带着物品换策略
	// 等于让所有已持久化的 pos 突然改变意义。fail-closed:只允许空背包换布局。
	if (store_.Size() > 0)
	{
		LOG_ERROR << "Bag::SetLayout refused: bag still holds " << store_.Size()
			<< " item(s); swap the layout only on an empty bag. player " << PlayerGuid();
		return;
	}
	layout_ = std::move(layout);
}

void Bag::SetAdmission(std::unique_ptr<IAdmissionPolicy> admission)
{
	if (admission == nullptr)
	{
		LOG_ERROR << "Bag::SetAdmission: null admission policy rejected, player " << PlayerGuid();
		return;
	}
	// 与 SetLayout 不同,这里**不要求背包是空的**:准入是纯谓词,换掉它不会
	// 让任何已存物品的 pos 改变含义(换布局会 —— 槽位号在不同策略下解释不同)。
	// 换成更严的策略后,包里可能留着新策略不接受的存量物品;那是刻意的:
	// 「入包严格、还原宽容」,不能因为规则变了就丢玩家的东西。
	admission_ = std::move(admission);
}

void Bag::SetEviction(std::unique_ptr<IEvictionPolicy> eviction)
{
	if (eviction == nullptr)
	{
		LOG_ERROR << "Bag::SetEviction: null eviction policy rejected, player " << PlayerGuid();
		return;
	}
	// 同 SetAdmission:纯策略对象,不重新解释任何已存状态,所以不要求空包。
	eviction_ = std::move(eviction);
}

void Bag::SetProfile(BagProfile profile)
{
	// **整套一起换,或者一根都不换。**
	// SetLayout 对非空包会拒绝(槽位号含义会变);若只拦它、放行另外两根,就得到
	// "旧布局 + 新淘汰策略"这种半套 profile —— 一个从不淘汰的包从此会挤掉玩家的
	// 东西。所以在这里统一把关,不靠三个 setter 各自的守卫。
	if (store_.Size() > 0)
	{
		LOG_ERROR << "Bag::SetProfile refused: bag still holds " << store_.Size()
			<< " item(s); a profile can only be installed on an empty bag. player " << PlayerGuid();
		return;
	}
	if (profile.layout == nullptr || profile.admission == nullptr || profile.eviction == nullptr)
	{
		LOG_ERROR << "Bag::SetProfile refused: incomplete profile (some axis is null). player "
			<< PlayerGuid();
		return;
	}
	SetLayout(std::move(profile.layout));
	SetAdmission(std::move(profile.admission));
	SetEviction(std::move(profile.eviction));
	profileId_ = profile.id;
}

void Bag::SetCapacityForRestore(std::size_t newCapacity)
{
	if (newCapacity < layout_->OccupiedSlotCount())
	{
		LOG_ERROR << "Bag::SetCapacityForRestore: refusing to shrink capacity to " << newCapacity
			<< " below " << layout_->OccupiedSlotCount() << " occupied slot(s); keeping "
			<< layout_->Capacity() << ". player " << PlayerGuid();
		return;
	}
	layout_->Resize(newCapacity);
	AssertLayerConsistency();
}

// ── 单层直通(留在这里只是为了保持公开 API 不变)──────────────────────────

std::size_t Bag::GetTotalItemCount(uint32_t config_id) const
{
	return store_.TotalCountOf(config_id);
}

ItemComp *Bag::GetItemCompByGuid(Guid guid)
{
	return store_.Find(guid);
}

// ── 按槽位取物:布局层查 guid,实例层查内容 ───────────────────────────────

ItemComp *Bag::GetItemCompByPos(uint32_t pos)
{
	const Guid guid = layout_->At(pos);
	return guid != kInvalidGuid ? store_.Find(guid) : nullptr;
}

// ── 容量预检:plan 问实例层,reserve 问布局层 ─────────────────────────────

uint32_t Bag::PlanInstances(const ItemCountMap &itemsToAdd, ItemCountMap *instancesByConfigOut,
							std::size_t *totalInstancesOut,
							const std::unordered_set<Guid> *exclude)
{
	// plan(实例层,零副作用):可叠加物品先填满同 config 现有堆叠的空余,只有
	// 溢出部分才需要**新建实例**;不可叠加(maxStack==1)每个单位一个实例。
	//
	// 拆分前"要几个实例"和"占几格"被 GridsNeededFor 这一个名字混成了一件事;
	// 现在前者在这里算,后者由 CanReserve 回答。
	AssertCapacityInvariant();

	const ItemCountMap freeRoomByConfig = store_.MeasureFreeRoomPerConfig(itemsToAdd, exclude);

	// 逐 config 算出"要为它新建几个实例",顺带按 config 留一份明细 ——
	// 具名槽的 reserve 要按**部位**汇总,只有一个总数是答不出来的
	// (两个 config 可能共用同一个部位,例如两件不同的手镯)。
	std::size_t instancesNeeded = 0;
	ItemCountMap instancesByConfig;
	for (const auto &[configId, count] : SortedByConfigId(itemsToAdd))
	{
		LookupItemOrReturnError(configId);
		const uint32_t maxStack = itemRow->max_stack_size();
		if (maxStack == 0)
		{
			LOG_ERROR << "config error:" << configId << " player:" << PlayerGuid();
			return PrintStackAndReturnError(kInvalidTableData);
		}

		// 准入:这个包收不收这种东西。纯 config 判定,与占用无关,所以放在
		// 算实例数之前 —— 不收就没必要再算了。
		if (!admission_->Accepts(configId))
		{
			LOG_ERROR << "Bag::PlanInstances: this bag does not admit config " << configId
				<< "; refusing the whole request. player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}

		const auto roomIt = freeRoomByConfig.find(configId);
		const uint32_t freeRoom = (roomIt != freeRoomByConfig.end()) ? roomIt->second : 0;
		const uint32_t overflow = count > freeRoom ? count - freeRoom : 0;
		const std::size_t instances = ItemStore::StacksNeededFor(overflow, maxStack);
		instancesNeeded += instances;
		if (instances > 0)
		{
			instancesByConfig[configId] += static_cast<uint32_t>(instances);
		}
	}

	if (instancesByConfigOut != nullptr)
	{
		*instancesByConfigOut = std::move(instancesByConfig);
	}
	if (totalInstancesOut != nullptr)
	{
		*totalInstancesOut = instancesNeeded;
	}
	return kSuccess;
}

uint32_t Bag::CheckSpaceFor(const ItemCountMap &itemsToAdd, std::size_t *instancesNeededOut)
{
	// 只读预检(不改背包):背包能否一次性放下 itemsToAdd 的每一项?
	//
	// **纯预测,刻意不问淘汰。** 它回答的是"照现在这样放得下吗",而不是"腾一腾
	// 能不能放下" —— 后者会销毁玩家的东西,不能藏在一个名字里带 Check 的函数里。
	// 真入包路径(AddItems)走 PlanInstances + PlanEviction。
	//
	// 副作用:对会淘汰的包(临时格),这个函数会说"放不下",而实际 AddItems 能
	// 成功。这是刻意的 —— 预测就该按不破坏现状回答。
	ItemCountMap instancesByConfig;
	std::size_t instancesNeeded = 0;
	RETURN_ON_ERROR(PlanInstances(itemsToAdd, &instancesByConfig, &instancesNeeded));

	// 整批一次判定。今天 FootprintFor 恒为 1x1,所以格子数那一问一次就够;等
	// 物品真有了宽高(格子背包),CanReserve 里要改成按 config 分组、逐形状问。
	if (!CanReserve(instancesByConfig, instancesNeeded))
	{
		return PrintStackAndReturnError(kBagItemNotStacked);
	}

	if (instancesNeededOut != nullptr)
	{
		*instancesNeededOut = instancesNeeded;
	}
	return kSuccess;
}

uint32_t Bag::CheckItemsAvailable(const ItemCountMap &requiredItems) const
{
	// 纯实例层问题:有没有这些东西。跟摆在哪、还剩几格毫无关系。静默:常规的"不够"由调用方决定怎么报。
	switch (CheckQuantities(requiredItems))
	{
	case ItemStore::QuantityVerdict::kEnough:
		return kSuccess;
	case ItemStore::QuantityVerdict::kVersionSkew:
		return kFeatureUnavailable;
	case ItemStore::QuantityVerdict::kInsufficient:
		break;
	}
	return kBagInsufficientItems;
}

uint32_t Bag::RemoveItems(const ItemCountMap &itemsToRemove,
						  std::vector<DrainedInstance> *drainedOut)
{
	// 事务语义:先确认每种物品都够扣,任何一种不够就整体失败,绝不做部分删除。
	// 调用方应先用 CheckQuantities 静默预检;走到这里还不够说明调用方漏了预检,打调用栈。
	if (const uint32_t availability = CheckItemsAvailable(itemsToRemove); availability != kSuccess)
	{
		return PrintStackAndReturnError(availability);
	}

	// 数量扣光后由桥层立即成对释放实例与槽位，后续整理无需再回收零堆。
	for (const auto &[configId, count] : SortedByConfigId(itemsToRemove))
	{
		DrainOneConfig(configId, count, drainedOut);
	}
	AssertLayerConsistency();
	return kSuccess;
}

uint32_t Bag::ReserveForBatchRemove(const std::vector<Guid> &guids,
								   std::vector<DestroyedInstance> *removableOut)
{
	// 纯预检:本函数**不得**修改 store_ / layout_ 的任何状态。先把结果攒在本地,
	// 全部通过了才写回 removableOut —— 否则调用方会拿到一份"半截清单",而那份
	// 清单正是它用来落流水的依据。
	if (guids.empty())
	{
		LOG_ERROR << "Bag::ReserveForBatchRemove: empty guid list; player " << PlayerGuid();
		return PrintStackAndReturnError(kAssetInvalidBundle);
	}

	std::unordered_set<Guid> seen;
	seen.reserve(guids.size());
	std::vector<DestroyedInstance> removable;
	removable.reserve(guids.size());

	for (const Guid guid : guids)
	{
		// ① 同一 guid 不能报两次。放过它的话,提交段第二次 RemoveItem 会失败,
		//    而第一件已经销毁 —— 全或无当场破掉。
		if (!seen.insert(guid).second)
		{
			LOG_ERROR << "Bag::ReserveForBatchRemove: duplicate guid " << guid
					  << " in the request; refusing the whole batch. player " << PlayerGuid();
			return PrintStackAndReturnError(kAssetInvalidBundle);
		}

		// ② guid 必须在**本包**里。不在就是终局拒绝:玩家早就不持有它了,重试不会变好。
		const ItemComp *item = GetItemCompByGuid(guid);
		if (item == nullptr)
		{
			LOG_ERROR << "Bag::ReserveForBatchRemove: guid " << guid
					  << " is not in this bag; refusing the whole batch. player " << PlayerGuid();
			return PrintStackAndReturnError(kAssetInvalidBundle);
		}

		// ③ 配置表必须查得到。查不到时 max_stack_size 无从判断,只能 fail-closed。
		const auto *itemRow = ItemTableManager::Instance().FindByIdSilent(item->config_id()).first;
		if (itemRow == nullptr)
		{
			LOG_ERROR << "Bag::ReserveForBatchRemove: guid " << guid << " has config "
					  << item->config_id() << " missing from the Item table; refusing the whole "
					  << "batch. player " << PlayerGuid();
			return PrintStackAndReturnError(kAssetInvalidBundle);
		}

		// ④ 只收不可叠加物品。可叠加物品的 guid 不是稳定身份 —— 可叠加入包
		//    会把预设 guid 并进既有堆后重铸,发回来的 guid 指向的可能是别的堆。
		if (itemRow->max_stack_size() != 1)
		{
			LOG_ERROR << "Bag::ReserveForBatchRemove: guid " << guid << " (config "
					  << item->config_id() << ") is stackable (max_stack_size="
					  << itemRow->max_stack_size() << "); by-guid removal only supports "
					  << "non-stackable items. player " << PlayerGuid();
			return PrintStackAndReturnError(kAssetInvalidBundle);
		}

		// ⑤ 不可叠加实例的 size 恒为 1。正常扣空已立即移除，仍见 0 或 >1
		//    都说明数据损坏，不能当成“一件装备”托管出去。
		if (item->size() != 1)
		{
			LOG_ERROR << "Bag::ReserveForBatchRemove: guid " << guid << " (config "
					  << item->config_id() << ") has size=" << item->size()
					  << ", expected exactly 1 for a non-stackable instance; refusing the whole "
					  << "batch. player " << PlayerGuid();
			return PrintStackAndReturnError(kAssetInvalidBundle);
		}

		removable.push_back(DestroyedInstance{guid, item->config_id(), item->size()});
	}

	if (removableOut != nullptr)
	{
		removableOut->insert(removableOut->end(), removable.begin(), removable.end());
	}
	return kSuccess;
}

uint32_t Bag::RemoveItemsClamped(const ItemCountMap &itemsToRemove,
								 std::vector<DrainedInstance> *drainedOut)
{
	// 与上面唯一的差别:不做 CheckItemsAvailable。每个 config 扣多少由实例层
	// 按实际持有夹紧,扣不满不算失败 —— 调用方比对请求量与回执总量就知道差了多少。
	for (const auto &[configId, count] : SortedByConfigId(itemsToRemove))
	{
		DrainOneConfig(configId, count, drainedOut);
	}
	AssertLayerConsistency();
	return kSuccess;
}

void Bag::DrainOneConfig(uint32_t configId, uint32_t count,
                         std::vector<DrainedInstance> *drainedOut)
{
    if (count == 0)
    {
        return;
    }
    const ItemStore::DrainRank bySlot = [this](Guid guid)
        { return static_cast<uint64_t>(layout_->SlotOf(guid)); };
    std::vector<ItemStore::StackDrain> drains;
    store_.DrainStacks(configId, count, bySlot, &drains);
    for (const auto &drain : drains)
    {
        if (drainedOut != nullptr)
        {
            drainedOut->push_back({drain.guid, configId, drain.amount});
        }
        if (const auto *item = store_.Find(drain.guid); item != nullptr && item->size() == 0)
        {
            DestroyItem(drain.guid);
        }
    }
}

uint32_t Bag::RemoveItemByPos(const RemoveItemByPosParam &param)
{
	// 按"槽位"精确删除指定物品的若干数量。调用方给出 pos/guid/config/size,
	// 四者全部对得上才会扣减 —— 一连串卫语句,任何一项不一致就立刻返回对应
	// 错误,绝不误删别的格子。全部是 O(1) 哈希查找,无遍历。
	//
	// 这条守卫链正好是桥层职责的写照:前两项问布局层(槽位在不在、上面是谁),
	// 后两项问实例层(它是什么、够不够扣)。

	if (param.size == 0) // 删除数量必须 > 0(size 是无符号,== 0 即"没要求删任何东西")
	{
		return PrintStackAndReturnError(kBagDelItemSize);
	}

	const Guid guidAtPos = layout_->At(param.pos); // 1) 这个槽位有东西吗(布局层)
	if (guidAtPos == kInvalidGuid)
	{
		return PrintStackAndReturnError(kBagDelItemPos);
	}

	if (guidAtPos != param.item_guid) // 2) 槽位上的实例是否就是调用方说的那个
	{
		return PrintStackAndReturnError(kBagDelItemGuid);
	}

	auto *item = store_.Find(param.item_guid); // 3) 该实例在仓库里真的存在吗(实例层)
	if (item == nullptr)
	{
		return PrintStackAndReturnError(kBagDelItemFindItem);
	}

	if (item->config_id() != param.item_config_id) // 4) config 是否一致(防张冠李戴)
	{
		return PrintStackAndReturnError(kBagDelItemConfig);
	}

	if (item->size() < param.size) // 5) 这一堆的数量是否够扣
	{
		return PrintStackAndReturnError(kBagItemDeletionSizeMismatch);
	}

	item->set_size(item->size() - param.size);
	if (item->size() == 0)
	{
		DestroyItem(param.item_guid);
	}
	AssertLayerConsistency();
	return kSuccess;
}

// ── 整理:实例层合并 + 布局层重排 ─────────────────────────────────────────

GuidVector Bag::DesiredOrder() const
{
	// 期望顺序 (config 升序, size 降序):同种聚拢、组内满堆在前、零头落到该组
	// 末尾,得到 [config A: 满..满 零头][config B: 满..满 零头]...
	//
	// 真正发生重排时,同 config / size 再按实例序打破平局;已整齐的布局仍保留玩家摆法。
	// 这是**实例层数据**决定的顺序,布局层只负责按它落位 —— 于是"按什么排"
	// 是一条可以随时改的玩法规则,不必碰任何一层。
	std::vector<std::pair<Guid, const ItemComp *>> ordered;
	ordered.reserve(store_.Size());
	store_.ForEach([&ordered](Guid guid, const ItemComp &item)
				   { ordered.emplace_back(guid, &item); });

	std::sort(ordered.begin(), ordered.end(), [](const auto &a, const auto &b)
			  {
				  if (a.second->config_id() != b.second->config_id())
				  {
					  return a.second->config_id() < b.second->config_id(); // 同种聚拢
				  }
				  if (a.second->size() != b.second->size())
				  {
					  return a.second->size() > b.second->size(); // 满堆在前、零头在后
				  }
				  return ItemStore::AcquiredBefore(*a.second, *b.second);
			  });

	GuidVector order;
	order.reserve(ordered.size());
	for (const auto &key : ordered)
	{
		order.push_back(key.first);
	}
	return order;
}

bool Bag::IsOrderedByConfigThenSize() const
{
	// 按 pos 顺序读出,相邻两格必须满足"config 不减;同 config 时 size 不增"。
	//
	// 刻意用相邻比较,而不是"跟 DesiredOrder() 的结果比对是否相等":
	// (config, size) 完全相同的两个实例之间顺序本就是任意的,拿排序结果去比会把
	// 本已整齐的布局判成需要整理,反而把玩家摆好的东西挪走。
	const auto &slots = layout_->Slots();

	bool hasPrev = false;
	uint32_t prevConfig = 0;
	uint32_t prevSize = 0;
	for (uint32_t pos = 0; pos < slots.size(); ++pos)
	{
		auto posIt = slots.find(pos);
		if (posIt == slots.end())
		{
			return false; // 不连续(IsCompact 应已挡住,这里双保险)
		}
		const auto *item = store_.Find(posIt->second);
		if (item == nullptr)
		{
			return false; // 两层对不上 -> 交给整理重建
		}
		const uint32_t curConfig = item->config_id();
		const uint32_t curSize = item->size();
		if (hasPrev)
		{
			if (curConfig < prevConfig)
			{
				return false; // config 没升序 -> 同种没聚拢
			}
			if (curConfig == prevConfig && curSize > prevSize)
			{
				return false; // 同 config 内 size 没降序 -> 满堆排在零头后面
			}
		}
		prevConfig = curConfig;
		prevSize = curSize;
		hasPrev = true;
	}
	return true;
}

bool Bag::MergeAndCompact(std::vector<DestroyedInstance> *destroyedOut, CompactPolicy policy)
{
	// 背包整理 = 两件属于不同层的事,桥层只负责编排:
	//   实例层:把同一种可叠加物品散落各处的"零头"合并成尽量少的满堆,并回收
	//           所有 size==0 的空实例;
	//   布局层:把剩下的实例重新紧凑铺开,消除中间空洞。
	// 拆分前这两件事挤在一个函数体里,既 set_size 又重排 posToGuid。
	// 会不会重排,由两件事共同决定:调用方的意图(policy),以及布局策略答不答应
	// (具名装备槽与格子背包都不允许被重排)。任何一方说不,就只合并不挪位置。
	const bool willReorder =
		policy == CompactPolicy::kMergeAndReorder && layout_->SupportsCompaction();

	// 早退(热路径):自动整理的路径每次开背包都会走一遍,绝大多数情况下背包
	// 早已整理好、无需再动。先做一遍只读检查,若已是最优就直接返回 —— 既省掉
	// 后面的销毁/重建开销,更关键的是"不打乱玩家已经摆好的布局"。
	const bool nothingToMerge = !store_.HasMergeablePartials();
	const bool nothingToReclaim = !store_.HasEmptyInstances();
	const bool layoutAlreadyFine =
		!willReorder || (layout_->IsCompact() && IsOrderedByConfigThenSize());
	if (nothingToMerge && nothingToReclaim && layoutAlreadyFine)
	{
		return false;
	}

	// ① 合并同种未满堆(纯实例层)。被合并空的堆 size 变 0,连同下面②一起回收。
	store_.MergePartialStacks();

	// ② 按实例序回收①刚清空的可叠加堆；扣空已当场释放，加载也跳过旧档零堆。
	//    销毁前抓拍 (guid, config, size) 交给调用方:整理会退役一批 item_uuid,
	//    而那是 transaction_log 的关联键。Bag 是纯容器不写流水,只负责报告。
	for (const Guid guid : store_.CollectEmptyInstances())
	{
		if (destroyedOut != nullptr)
		{
			if (const auto *item = store_.Find(guid); item != nullptr)
			{
				destroyedOut->push_back(
					DestroyedInstance{guid, item->config_id(), item->size()});
			}
		}
		DestroyItem(guid); // 两层成对销毁,这是桥层的活
	}

	// ③ 让布局层按期望顺序重铺。kMergeOnly、或者不支持重排的策略(格子背包 /
	//    具名装备槽),到这里就只完成了合并与回收,位置一格不动 —— 那正是
	//    "东西还在你离开时摆的地方"这条意图需要的。
	if (willReorder)
	{
		layout_->ApplyOrder(DesiredOrder());
	}
	AssertLayerConsistency();
	return true;
}

// ── 入包:plan -> reserve -> commit ───────────────────────────────────────

// 对一件**新铸**的不可叠加实例调一次初始化回调(装备掷属性),契约见 item_system.h。
//
// 判据只有 `!piece.has_equip()` 这一条:带着装备数据来的既有实例(穿脱搬运回流、邮件
// 附件)已经初始化过,再调就是重掷(docs/design/equipment-attributes.md §5 不变量 1)。
//
// 调用点在 reserve 之后、这一件进 ItemStore 之前,所以回调**不许失败**;它也不许动
// 容器负责的字段。后一条不靠约定:调用前抓拍、调用后钉回去 —— 回调若改了 config_id,
// 这一件就会带着没做过 reserve / 准入 / 部位判定的 config 进包;改了 item_id 则绕过
// 入口那道预设 guid 撞车预检。钉回去之后回调再怎么写错,坏的也只是它自己那段实例数据。
static void InitializeNewInstance(const ItemInstanceInitializer *instanceInitializer,
								  ItemComp &piece, Guid playerGuid)
{
	if (instanceInitializer == nullptr || !*instanceInitializer || piece.has_equip())
	{
		return;
	}
	const uint64_t itemId = piece.item_id();
	const uint32_t configId = piece.config_id();
	const uint32_t size = piece.size();
	const uint64_t acquireSeq = piece.acquire_seq();

	(*instanceInitializer)(piece);

	if (piece.item_id() != itemId || piece.config_id() != configId || piece.size() != size ||
		piece.acquire_seq() != acquireSeq)
	{
		LOG_ERROR << "Bag: item instance initializer touched container-owned fields (item_id "
			<< itemId << " -> " << piece.item_id() << ", config_id " << configId << " -> "
			<< piece.config_id() << ", size " << size << " -> " << piece.size()
			<< ", acquire_seq " << acquireSeq << " -> " << piece.acquire_seq()
			<< "); restoring them. The initializer may only write instance data. player "
			<< playerGuid;
		piece.set_item_id(itemId);
		piece.set_config_id(configId);
		piece.set_size(size);
		piece.set_acquire_seq(acquireSeq);
	}
}

uint32_t Bag::AddItem(const InitItemParam &param, std::vector<Guid> *writtenGuidsOut,
					  std::vector<DestroyedInstance> *evictedOut,
					  const ItemInstanceInitializer *instanceInitializer)
{
	std::vector<GuidVector> written;
	const auto result = AddItems(std::vector<InitItemParam>{param}, evictedOut, &written,
								 instanceInitializer);
	if (result == kSuccess && writtenGuidsOut != nullptr && !written.empty())
	{
		writtenGuidsOut->insert(writtenGuidsOut->end(), written.front().begin(), written.front().end());
	}
	return result == kBagItemNotStacked ? kBagAddItemBagFull : result;
}

uint32_t Bag::AddItems(const ItemCountMap &itemsToAdd,
					   std::vector<DestroyedInstance> *evictedOut,
					   std::vector<GuidVector> *writtenByEntryOut,
					   const ItemInstanceInitializer *instanceInitializer)
{
	std::vector<InitItemParam> entries;
	entries.reserve(itemsToAdd.size());
	for (const auto &[configId, count] : SortedByConfigId(itemsToAdd))
	{
		InitItemParam param;
		param.itemPBComp.set_config_id(configId);
		param.itemPBComp.set_size(count);
		entries.push_back(std::move(param));
	}
	return AddItems(entries, evictedOut, writtenByEntryOut, instanceInitializer);
}

uint32_t Bag::AddItems(const std::vector<InitItemParam> &itemsToAdd,
					   std::vector<DestroyedInstance> *evictedOut,
					   std::vector<GuidVector> *writtenByEntryOut,
					   const ItemInstanceInitializer *instanceInitializer)
{
	// 所有入口共用一次提交。按需铸出的号允许作废，玩家实例、槽位和回执只能整批发布。
	if (itemsToAdd.empty())
	{
		return kSuccess;
	}
	ItemCountMap stackUnits;
	ItemCountMap separateInstances;
	std::unordered_set<Guid> preassigned;
	for (const auto &param : itemsToAdd)
	{
		const auto &item = param.itemPBComp;
		if (item.config_id() == 0 || item.size() == 0)
		{
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
		LookupItemOrReturnError(item.config_id());
		const auto maxStack = itemRow->max_stack_size();
		if (maxStack == 0)
		{
			return PrintStackAndReturnError(kInvalidTableData);
		}
		if (!admission_->Accepts(item.config_id()))
		{
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
		// 独立实例（已带实例数据，或带着本版本不认识的字段）不能合堆；单独计实例数，不拿普通材料的空余抵扣它。
		// 判据必须与提交侧 CanStack 同一个（ItemStore::IsIndependentInstance），否则规划少铸号、提交时号不够。
		const bool independent = ItemStore::IsIndependentInstance(item);
		auto &units = independent ? separateInstances[item.config_id()] : stackUnits[item.config_id()];
		const auto added = independent ? ItemStore::StacksNeededFor(item.size(), maxStack) : item.size();
		if (added > std::numeric_limits<uint32_t>::max() - units)
		{
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
		units += static_cast<uint32_t>(added);
		if (maxStack == 1 && item.size() == 1 && !ItemStore::IsInvalidGuid(item))
		{
			if (store_.Contains(item.item_id()) || !preassigned.insert(item.item_id()).second)
			{
				return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
			}
		}
	}

	GuidVector victims;
	std::size_t newInstanceCount = 0;
	if (!PlanEviction(stackUnits, evictedOut != nullptr, victims, &newInstanceCount, &separateInstances))
	{
		return PrintStackAndReturnError(kBagItemNotStacked);
	}

	// 先铸齐最终规划所需的号，再执行初始化回调和任何玩家状态修改。预取/失效可在铸号中途发生。
	if (newInstanceCount < preassigned.size())
	{
		LOG_ERROR << "Bag::AddItems: 规划实例数少于预设身份数，批次未修改。player=" << PlayerGuid();
		assert(false && "规划实例数少于预设身份数");
		return PrintStackAndReturnError(kInvalidTableData);
	}
	GuidVector minted;
	minted.reserve(newInstanceCount - preassigned.size());
	std::unordered_set<Guid> reserved = preassigned;
	for (std::size_t i = preassigned.size(); i < newInstanceCount; ++i)
	{
		const Guid guid = ItemStore::MintGuid();
		if (guid == 0 || guid == kInvalidGuid || store_.Contains(guid) || !reserved.insert(guid).second)
		{
			LOG_ERROR << "Bag::AddItems: item guid reservation failed; batch unchanged. player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
		minted.push_back(guid);
	}

	// 候选仓库隔离合堆、盖章和实例初始化；失败时原仓库及其入包水位保持不变。
	ItemStore candidate = store_;
	std::vector<DestroyedInstance> evicted;
	for (const auto guid : victims)
	{
		const auto *item = candidate.Find(guid);
		if (item == nullptr)
		{
			LOG_ERROR << "Bag::AddItems: 淘汰策略返回仓库外身份，批次未修改。player=" << PlayerGuid();
			assert(false && "淘汰策略返回仓库外身份");
			return PrintStackAndReturnError(kInvalidTableData);
		}
		evicted.push_back({guid, item->config_id(), item->size()});
		candidate.Erase(guid);
	}
	std::vector<std::pair<Guid, uint32_t>> newItems;
	std::vector<GuidVector> written(itemsToAdd.size());
	std::size_t nextMint = 0;
	for (std::size_t entry = 0; entry < itemsToAdd.size(); ++entry)
	{
		const auto &proto = itemsToAdd[entry].itemPBComp;
		LookupItemOrReturnError(proto.config_id());
		const auto maxStack = itemRow->max_stack_size();
		std::vector<ItemStore::StackFill> fills;
		uint32_t remaining = maxStack > 1 ? candidate.PlanStackIntoExistingStacks(proto, maxStack, fills) : proto.size();
		candidate.ApplyStackFill(fills, &written[entry]);
		while (remaining > 0)
		{
			ItemComp piece = proto;
			const auto amount = std::min(maxStack, remaining);
			piece.set_size(amount);
			if (!(maxStack == 1 && proto.size() == 1 && !ItemStore::IsInvalidGuid(proto)))
			{
				if (nextMint == minted.size())
				{
					LOG_ERROR << "Bag::AddItems: 预取身份数与规划不一致，批次未修改。player=" << PlayerGuid();
					assert(false && "预取身份数与规划不一致");
					return PrintStackAndReturnError(kInvalidTableData);
				}
				piece.set_item_id(minted[nextMint++]);
			}
			if (maxStack == 1)
			{
				InitializeNewInstance(instanceInitializer, piece, PlayerGuid());
			}
			const auto guid = piece.item_id();
			if (candidate.Insert(std::move(piece)) == nullptr)
			{
				return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
			}
			newItems.emplace_back(guid, proto.config_id());
			written[entry].push_back(guid);
			remaining -= amount;
		}
	}
	if (nextMint != minted.size())
	{
		LOG_ERROR << "Bag::AddItems: 规划与提交实例数不一致，批次未修改。player=" << PlayerGuid();
		assert(false && "规划与提交实例数不一致");
		return PrintStackAndReturnError(kInvalidTableData);
	}

	// 布局与仓库都在候选中完成，试放失败只丢弃副本，不依赖反向恢复或 Debug 断言。
	auto candidateLayout = layout_->Clone();
	if (candidateLayout == nullptr)
	{
		LOG_ERROR << "Bag::AddItems: 布局无法创建事务候选，批次未修改。player=" << PlayerGuid();
		assert(false && "布局无法创建事务候选");
		return PrintStackAndReturnError(kInvalidTableData);
	}
	for (const auto guid : victims)
	{
		candidateLayout->Remove(guid);
	}
	for (const auto &[guid, configId] : newItems)
	{
		if (PlaceInstance(guid, configId, candidateLayout.get()) == kInvalidSlot)
		{
			return PrintStackAndReturnError(kBagItemNotStacked);
		}
	}
	// 单一场景逻辑线程，无回调的提交段同时发布两层候选。
	store_ = std::move(candidate);
	layout_ = std::move(candidateLayout);
	if (evictedOut != nullptr)
	{
		evictedOut->insert(evictedOut->end(), evicted.begin(), evicted.end());
	}
	if (writtenByEntryOut != nullptr)
	{
		writtenByEntryOut->insert(writtenByEntryOut->end(), written.begin(), written.end());
	}
	AssertLayerConsistency();
	return kSuccess;
}

uint32_t Bag::RemoveItem(Guid del_guid)
{
	if (!store_.Contains(del_guid))
	{
		return PrintStackAndReturnError(kBagDeleteItemFindGuid);
	}

	DestroyItem(del_guid);
	AssertLayerConsistency();
	return kSuccess;
}

void Bag::ExpandCapacity(std::size_t sz)
{
	// 容量是布局层的属性,增量语义保持不变。
	layout_->Resize(layout_->Capacity() + sz);
	AssertLayerConsistency();
}

std::size_t Bag::GridsNeededFor(std::size_t totalSize, std::size_t maxStackSize)
{
	return ItemStore::StacksNeededFor(totalSize, maxStackSize);
}

uint32_t Bag::EquipKindFor(uint32_t configId)
{
	// 表里查不到就当"不受约束"——查不到本身会由 CheckSpaceFor / AddItem 那边
	// 的 LookupItemOrReturnError 报出来,这里不重复报错。
	const auto [itemRow, itemResult] = ItemTableManager::Instance().FindByIdSilent(configId);
	if (itemRow == nullptr)
	{
		return kInvalidSlot;
	}
	return itemRow->equip_kind();
}

// 这一行槽位**接不接**这个部位 —— 只看槽位表与当前容量,不看占用。
//
// 从下面的 IsFreeSlotForEquipKind 里拆出来,是因为穿脱编排要问的恰恰是"不看占用"的
// 那一半(Bag::SlotsAcceptingKind:该部位有哪些槽,满了要替换哪一只),而指定槽放置
// (Bag::PutInstance)要拿同一条口径校验调用方给的槽。**"接受"只许有这一处定义**,
// 否则 SlotsAcceptingKind 报出来的槽 PutInstance 可能不认。
static bool SlotRowAcceptsKind(const EquipSlotTable &slotRow, uint32_t kind,
							   const IContainerLayout &layout)
{
	// 槽位表说了算:哪个槽接受哪个部位,完全是数据。表里没出现的槽位不接受任何东西。
	if (slotRow.equip_kind() != kind)
	{
		return false;
	}
	const auto slot = static_cast<SlotId>(slotRow.id());
	// 槽位号必须落在**当前容量**内。容量是可以被快照还原改小的(见
	// FixedSlotLayout 那条"刻意不写死槽位数"的注释),此时表里靠后的槽位
	// PlaceAt 会拒。不在这里一起滤掉,reserve 就会比 commit 乐观。
	return slot < layout.Capacity();
}

// 这一行槽位,对这个部位来说是不是一个"可用的空槽"。
//
// **FindFreeSlotForKind(commit 侧)与 CountFreeSlotsForKind(reserve 侧)必须
// 共用这一条判据。** 两侧一旦各写各的,reserve 数出来的空槽就可能不是 commit
// 真能落位的那些,于是"预检通过、写到一半失败"会以新的形式复活 —— 那正是
// CanReserve 要消灭的 bug 本身。
static bool IsFreeSlotForEquipKind(const EquipSlotTable &slotRow, uint32_t kind,
								   const IContainerLayout &layout)
{
	if (!SlotRowAcceptsKind(slotRow, kind, layout))
	{
		return false;
	}
	return layout.At(static_cast<SlotId>(slotRow.id())) == kInvalidGuid; // 空着才算
}

SlotId Bag::FindFreeSlotForKind(uint32_t kind, const IContainerLayout *target) const
{
    const auto &layout = target != nullptr ? *target : *layout_;
	// 找的是**第一个空的**接受槽,不是唯一那个 —— 同一部位允许有多个槽
	// (两个手镯位 = 表里两行 equip_kind 相同)。上一版模型把部位和槽号
	// 混成了一个字段,根本表达不了这个需求。
	if (kind == 0)
	{
		return kInvalidSlot;
	}
	for (const auto &slotRow : EquipSlotTableManager::Instance().FindAll().data())
	{
		if (IsFreeSlotForEquipKind(slotRow, kind, layout))
		{
			return static_cast<SlotId>(slotRow.id());
		}
	}
	return kInvalidSlot;
}

std::size_t Bag::CountFreeSlotsForKind(uint32_t kind) const
{
	if (kind == 0)
	{
		return 0;
	}
	// 按**物理槽位**去重:槽位表若出现同一 id 的两行(脏表,Load 用 emplace 静默吞掉
	// 重复键),commit 只能落到一个槽,reserve 也只能数一次 —— 否则 reserve 又比
	// commit 乐观了,装备栏批量入包写到一半失败。
	std::unordered_set<SlotId> counted;
	for (const auto &slotRow : EquipSlotTableManager::Instance().FindAll().data())
	{
		if (IsFreeSlotForEquipKind(slotRow, kind, *layout_))
		{
			counted.insert(static_cast<SlotId>(slotRow.id()));
		}
	}
	return counted.size();
}

bool Bag::PlanEviction(const ItemCountMap &unitsToAdd, bool allowEviction,
					   GuidVector &victimOrder, std::size_t *totalInstancesOut,
					   const ItemCountMap *separateInstances)
{
	victimOrder.clear();
	const auto plan = [&](ItemCountMap &need, std::size_t &total, const std::unordered_set<Guid> *excluded = nullptr)
	{
		if (PlanInstances(unitsToAdd, &need, &total, excluded) != kSuccess)
		{
			return false;
		}
		if (separateInstances != nullptr)
		{
			for (const auto &[configId, count] : *separateInstances)
			{
				need[configId] += count;
				total += count;
			}
		}
		if (totalInstancesOut != nullptr)
		{
			*totalInstancesOut = total;
		}
		return true;
	};
	// 第一问永远是"照现在这样放得下吗" —— 放得下就什么都不做,淘汰只在真放不下时发生。
	ItemCountMap need;
	std::size_t needTotal = 0;
	if (!plan(need, needTotal))
	{
		return false; // 配置 / 准入问题;调用方在这之前已用 PlanInstances 拿过精确错误码
	}
	if (CanReserve(need, needTotal))
	{
		return true;
	}

	// 具名槽布局**一律不淘汰**。
	//
	// "为了给新手镯腾位而销毁你正戴着的手镯"不是任何游戏想要的行为;而且具名槽
	// 的位是按部位分桶的,"腾一个位"得先回答"腾哪个部位的",那是另一套账。
	// BagProfile::Equipment 已经配了 RejectWhenFull,这里是第二道锁 ——
	// 有人日后手工 SetEviction 到装备栏上,也不会把装备烧掉。
	if (layout_->HasSlotSemantics())
	{
		return false;
	}

	// **没有回执就不淘汰。** 被销毁的实例必须经 evictedOut 交给 BagService 落
	// LogItemDestroy —— item_uuid 是外挂回收的关联键。此前这只是一句"生产代码请走
	// BagService"的注释,任何直接调纯容器的调用方都能无声销毁玩家资产;现在是闸:
	// 不带回执的调用在会淘汰的包上按 RejectWhenFull 处理。
	if (!allowEviction)
	{
		LOG_ERROR << "Bag::PlanEviction: eviction would be needed but no receipt sink was "
			<< "given (evictedOut == nullptr); refusing to destroy anything. Route through "
			<< "BagService. player " << PlayerGuid();
		return false;
	}

	// 容量不是标量的布局(格子背包:空格数够却因碎片摆不进去)—— 淘汰不知道怎么
	// 腾出一块连续矩形,按 RejectWhenFull 处理。这不是编程错误,不吼。
	// 自由格布局下 1 实例恒占 1 格,到不了这里(CanFit 假 ⇔ 空格数 < 需求)。
	const std::size_t freeCells = layout_->FreeCells();
	if (freeCells >= needTotal)
	{
		return false;
	}

	// ── 先算到不动点,再销毁 ─────────────────────────────────────────────
	// 关键:被挤掉的可能正是本批打算并进去的未满堆(临时格里同种材料的旧堆恰恰
	// 最早进包)。把牺牲者当作**已不存在**重新规划,需求会变大;需求变大就多选
	// 牺牲者;直到"腾出的格子 >= 排除牺牲者后的需求"稳定下来。全程零副作用,
	// 稳定了才动手 —— 于是"腾不够就一件都不动"是结构保证,不是祈愿。
	//
	// 收敛:牺牲者集合每轮严格变大(否则退出),上界是 store_.Size() 轮。
	// 先进先出是前缀稳定的(最早 k 个 ⊂ 最早 k+1 个),并集不改变它;对不前缀
	// 稳定的未来策略,并集保证单调。
	std::unordered_set<Guid> victimSet;
	bool converged = false;
	for (std::size_t round = 0; round <= store_.Size(); ++round)
	{
		ItemCountMap needIfEvicted;
		std::size_t needIfEvictedTotal = 0;
		if (!plan(needIfEvicted, needIfEvictedTotal, &victimSet))
		{
			return false;
		}
		const std::size_t shortfall =
			needIfEvictedTotal > freeCells ? needIfEvictedTotal - freeCells : 0;
		if (shortfall <= victimSet.size())
		{
			converged = true; // 腾出的格子够了
			break;
		}

		const GuidVector picked = eviction_->SelectVictims(shortfall, store_, *layout_);
		if (picked.size() < shortfall)
		{
			// RejectWhenFull 恒走这条(返回空),等于拆分前"背包满了"的行为。
			// EvictOldestFirst 腾不够时也走这条 —— 一件都不动。
			return false;
		}
		bool grew = false;
		for (const Guid guid : picked)
		{
			if (victimSet.insert(guid).second)
			{
				victimOrder.push_back(guid);
				grew = true;
			}
		}
		if (!grew)
		{
			return false; // 策略给不出新的牺牲者却仍不够:腾不够,一件都不动
		}
	}
	if (!converged)
	{
		LOG_ERROR << "Bag::PlanEviction: eviction planning did not converge within "
			<< store_.Size() << " round(s); refusing without destroying anything. player "
			<< PlayerGuid();
		return false;
	}

	return true;
}

bool Bag::CanReserve(const ItemCountMap &instancesByConfig, std::size_t totalInstances) const
{
	AssertCapacityInvariant();

	// 第一问:格子数够不够。任何布局都要过这一关。
	if (!layout_->CanFit(totalInstances, kSingleCell))
	{
		return false;
	}

	// 自由格布局(人物背包 / 仓库 / 临时格 / 格子背包)到此为止:位置本身不
	// 表达任何东西,有空格就摆得下。**这个分叉条件与 PlaceInstance 逐字相同**,
	// 这是本函数的全部要点。
	if (!layout_->HasSlotSemantics())
	{
		return true;
	}

	// 具名槽(装备栏):容量是按**部位**分桶的,不是一个标量。
	// 必须先把需求按部位汇总、再逐部位比对空槽数 —— 逐 config 单独问会把
	// 同一批槽位重复算给两个 config(两件不同的手镯共用那两个手镯位,
	// 用例 DifferentConfigsSharingAKindShareItsSlots 钉的就是这件事)。
	ItemCountMap neededByKind;
	for (const auto &[configId, instances] : instancesByConfig)
	{
		if (instances == 0)
		{
			continue;
		}
		const uint32_t kind = EquipKindFor(configId);
		if (kind == 0)
		{
			// 没在表里声明部位 = 不是装备,压根不该进这个包。
			// 与 PlaceInstance 同一条判据,只是提前到了写入任何东西之前。
			LOG_ERROR << "Bag::CanReserve: config " << configId
				<< " declares no equip_kind but the target bag has named slots; refusing. player "
				<< PlayerGuid();
			return false;
		}
		neededByKind[kind] += instances;
	}

	for (const auto &[kind, needed] : neededByKind)
	{
		// 表里查不到的 config 会走到这里(EquipKindFor 返回 kInvalidSlot 哨兵),
		// 它匹配不到任何槽位行,空槽数恒为 0,于是同样被拒 —— 不必单列一支。
		const std::size_t freeSlots = CountFreeSlotsForKind(kind);
		if (freeSlots < needed)
		{
			LOG_ERROR << "Bag::CanReserve: equip kind " << kind << " needs " << needed
				<< " free slot(s) but only " << freeSlots << " available; refusing the whole "
				<< "batch before writing anything. player " << PlayerGuid();
			return false;
		}
	}
	return true;
}

SlotId Bag::PlaceInstance(Guid guid, uint32_t configId, IContainerLayout *target)
{
    auto &layout = target != nullptr ? *target : *layout_;
	const Footprint footprint = FootprintFor(configId);

	// 槽位没有语义的布局(人物背包 / 仓库 / 临时格 / 格子背包):位置本身不表达
	// 任何东西,first-fit 即可 —— 与拆分前完全一致。一件装备躺在人物背包里时
	// 走的就是这条路,它不该被强行塞进"装备槽 3"。
	if (!layout.HasSlotSemantics())
	{
		return layout.Place(guid, footprint);
	}

	// 具名槽布局(装备栏):物品声明**部位**,槽位表声明**每个槽接受哪个部位**。
	const uint32_t kind = EquipKindFor(configId);
	if (kind == 0)
	{
		// 没在表里声明部位 = 不是装备,压根不该进这个包。
		LOG_ERROR << "Bag::PlaceInstance: config " << configId
			<< " declares no equip_kind but the target bag has named slots; refusing. player "
			<< PlayerGuid();
		return kInvalidSlot;
	}
	const SlotId slot = FindFreeSlotForKind(kind, &layout);
	if (slot == kInvalidSlot)
	{
		// 该部位的槽全被占满了(或者槽位表压根没给这个部位开过槽)。
		LOG_ERROR << "Bag::PlaceInstance: no free slot accepts equip kind " << kind
			<< " (config " << configId << "); refusing. player " << PlayerGuid();
		return kInvalidSlot;
	}
	if (!layout.PlaceAt(guid, slot, footprint))
	{
		LOG_ERROR << "Bag::PlaceInstance: equip slot " << slot << " for config " << configId
			<< " is occupied or out of range (capacity " << layout.Capacity()
			<< "); refusing. player " << PlayerGuid();
		return kInvalidSlot;
	}
	return slot;
}

void Bag::DestroyItem(Guid guid)
{
	// 两层成对销毁 —— 少做任何一半都会留下腐化:
	//   只删实例:槽位被一个幽灵 guid 永久占着,背包会"莫名其妙满了";
	//   只删槽位:size=0 的孤儿实例仍在实例表里,后续并堆会往一个
	//             已经没有位置的实例上灌数量,单位就静默丢了。
	// 拆分前这两半写在同一个函数体里(destroy 实体 + 线扫 posToGuid),
	// 现在是桥层显式地各调一次 —— 这正是桥层存在的理由。
	store_.Erase(guid);
	layout_->Remove(guid);
}

// ── 搬运原语(同一 guid 换包:穿脱 / 包间移动)─────────────────────────────
// 契约见 bag_system.h。这里的纪律只有一条,与入包的三段同源:**所有校验先于任何修改**。
// 调用方要靠"失败 = 两层都没动"来按相反顺序回滚,任何一个会失败的判断排在第一次写入
// 之后,回滚就没有可信的起点了。

std::vector<uint32_t> Bag::SlotsAcceptingKind(uint32_t equipKind) const
{
	std::vector<uint32_t> slots;
	// 自由格布局(人物背包 / 仓库 / 临时格)的槽位号不表达任何东西,"哪些槽接受部位 N"
	// 在那里没有答案;把槽位表的行号原样报出去,调用方就会拿装备栏的槽号去指人物背包的格子。
	// equipKind == 0 = 不是装备,同样没有接受它的槽(与 FindFreeSlotForKind 一致)。
	if (equipKind == 0 || !layout_->HasSlotSemantics())
	{
		return slots;
	}
	for (const auto &slotRow : EquipSlotTableManager::Instance().FindAll().data())
	{
		if (SlotRowAcceptsKind(slotRow, equipKind, *layout_))
		{
			slots.push_back(static_cast<uint32_t>(slotRow.id()));
		}
	}
	// 升序是契约(穿上规则 = 槽号升序里第一个空槽,都占着就替换槽号最小的),不能依赖表的
	// 行序;去重是防脏表里同一 id 出现两行(理由同 CountFreeSlotsForKind)。
	std::sort(slots.begin(), slots.end());
	slots.erase(std::unique(slots.begin(), slots.end()), slots.end());
	return slots;
}

uint32_t Bag::TakeInstance(Guid guid, ItemComp &out)
{
	// ── 全部纯校验 ──────────────────────────────────────────────────────
	const ItemComp *item = store_.Find(guid);
	if (item == nullptr)
	{
		return PrintStackAndReturnError(kBagDeleteItemFindGuid); // 与 RemoveItem 同码
	}

	// 查不到表就无从判断可否叠加,fail-closed(与 ReserveForBatchRemove ③ 同一取舍)。
	LookupItemOrReturnError(item->config_id());

	// 只搬不可叠加实例:可叠加物品的 guid 不是稳定身份(并堆会重铸),整件拷走再放进
	// 另一个包还会绕过并堆,凭空多出一个未满堆。
	if (itemRow->max_stack_size() != 1)
	{
		LOG_ERROR << "Bag::TakeInstance: guid " << guid << " (config " << item->config_id()
			<< ") is stackable (max_stack_size=" << itemRow->max_stack_size()
			<< "); only non-stackable instances can be carried. player " << PlayerGuid();
		return PrintStackAndReturnError(kBagDelItemConfig);
	}
	// 不可叠加实例的 size 恒为 1。正常扣空已立即移除，仍见 0 或 >1
	// 都说明数据损坏，不能当成“一件装备”搬走。
	if (item->size() != 1)
	{
		LOG_ERROR << "Bag::TakeInstance: guid " << guid << " (config " << item->config_id()
			<< ") has size=" << item->size() << ", expected exactly 1. player " << PlayerGuid();
		return PrintStackAndReturnError(kBagItemDeletionSizeMismatch);
	}

	// ── 校验全过,现在才允许改状态 ──────────────────────────────────────
	// 先整份拷出(含 equip 与 acquire_seq)再销毁:DestroyItem 之后 item 指针即失效。
	out = *item;
	DestroyItem(guid); // 两层成对移除,与 RemoveItem 走同一个口子
	AssertLayerConsistency();
	return kSuccess;
}

uint32_t Bag::PutInstance(ItemComp item, std::optional<uint32_t> slot)
{
	// ── 全部纯校验 ──────────────────────────────────────────────────────
	if (ItemStore::IsInvalidGuid(item))
	{
		LOG_ERROR << "Bag::PutInstance: invalid guid (config " << item.config_id()
			<< "); a carried instance must keep its own identity. player " << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidGuid);
	}
	const Guid guid = item.item_id();
	const uint32_t configId = item.config_id();
	if (configId == 0 || item.size() != 1)
	{
		LOG_ERROR << "Bag::PutInstance: guid " << guid << " has config " << configId << " size "
			<< item.size() << "; expected a single non-stackable instance. player " << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
	}

	LookupItemOrReturnError(configId);
	if (itemRow->max_stack_size() == 0)
	{
		return PrintStackAndReturnError(kInvalidTableData);
	}
	// 与 TakeInstance 对称:可叠加物品走这里会绕过并堆。
	if (itemRow->max_stack_size() != 1)
	{
		LOG_ERROR << "Bag::PutInstance: guid " << guid << " (config " << configId
			<< ") is stackable (max_stack_size=" << itemRow->max_stack_size()
			<< "); only non-stackable instances can be carried. player " << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
	}

	// 准入照常问:搬运不是绕过"这个包收不收这种东西"的后门(节日包照样不收装备)。
	if (!admission_->Accepts(configId))
	{
		LOG_ERROR << "Bag::PutInstance: this bag does not admit config " << configId
			<< " (guid " << guid << "); refusing. player " << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
	}

	if (store_.Contains(guid))
	{
		LOG_ERROR << "Bag::PutInstance: guid " << guid << " (config " << configId
			<< ") already exists in this bag; refusing. player " << PlayerGuid();
		return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
	}

	if (slot.has_value())
	{
		const SlotId wanted = *slot;
		// 越界:布局层的 PlaceAt 也会拒,但那已经在实例入库之后了 —— 这里提前问。
		if (wanted == kInvalidSlot || wanted >= layout_->Capacity())
		{
			LOG_ERROR << "Bag::PutInstance: slot " << wanted << " is out of range (capacity "
				<< layout_->Capacity() << ") for guid " << guid << ". player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
		// 被占:绝不顶替。"替换"是编排层先把旧的 Take 走、再 Put 新的。
		if (layout_->At(wanted) != kInvalidGuid)
		{
			LOG_ERROR << "Bag::PutInstance: slot " << wanted << " is occupied by guid "
				<< layout_->At(wanted) << "; refusing to place guid " << guid << ". player "
				<< PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemBagFull);
		}
		// 部位:只有具名槽布局才问,且与 SlotsAcceptingKind 是同一条口径(它报出来的槽
		// 这里一定认)。自由格布局的格子放什么都行 —— 分叉条件与 PlaceInstance 相同。
		if (layout_->HasSlotSemantics())
		{
			const uint32_t kind = EquipKindFor(configId);
			const std::vector<uint32_t> accepting = SlotsAcceptingKind(kind);
			if (std::find(accepting.begin(), accepting.end(), wanted) == accepting.end())
			{
				LOG_ERROR << "Bag::PutInstance: slot " << wanted << " does not accept equip kind "
					<< kind << " (config " << configId << ", guid " << guid << "); refusing. player "
					<< PlayerGuid();
				return PrintStackAndReturnError(kBagAddItemInvalidParam);
			}
		}
	}
	else
	{
		// 自动找位的 reserve:CanReserve 与下面 commit 用的 PlaceInstance 在同一个条件上
		// 分叉(HasSlotSemantics),它说能放,PlaceInstance 就一定落得了位。
		// **刻意不走 PlanEviction**:搬运绝不为了腾位销毁东西,临时格也一样。
		if (!CanReserve({{configId, uint32_t{1}}}, 1))
		{
			return PrintStackAndReturnError(kBagAddItemBagFull);
		}
	}

	// ── 校验全过,现在才允许改状态 ──────────────────────────────────────
	// 入包序号是"本包内的先后",不随实例搬家:ItemStore::Insert 对非 0 值会原样保留并把
	// 水位抬到它之上,带着源包的序号进来,先进先出就按别的包的时间线挤人了。
	item.set_acquire_seq(0);

	if (store_.Insert(std::move(item)) == nullptr)
	{
		// 上面刚查过 Contains,走到这里只可能是实例层索引与实体不同步(编程错误)。
		// Insert 失败自己会回滚半建好的实体,两层仍然没动。
		LOG_ERROR << "Bag::PutInstance: ItemStore refused guid " << guid << " right after the "
			<< "Contains check passed (programming error). player " << PlayerGuid();
		return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
	}

	const bool placed = slot.has_value()
							? layout_->PlaceAt(guid, *slot, FootprintFor(configId))
							: PlaceInstance(guid, configId) != kInvalidSlot;
	if (!placed)
	{
		// 预检已排除全部数据可触发的原因;到这里说明预检与布局层不一致,是真 bug。
		// 回滚这一件 —— 绝不留下没有槽位的孤儿实例(同其他入包分支)。
		store_.Erase(guid);
		LOG_ERROR << "Bag::PutInstance: layout refused to place guid " << guid << " (config "
			<< configId << ", capacity " << layout_->Capacity() << ", occupied "
			<< layout_->OccupiedSlotCount() << ") after the pre-check passed (programming error). "
			<< "player " << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemBagFull);
	}

	AssertLayerConsistency();
	return kSuccess;
}

// ── 快照还原(cross-zone / rollback / 持久化)────────────────────────────
// 详见 cpp/libs/services/scene/player/system/bag_marshal.{h,cpp} 与
// docs/design/cross-zone-readiness-audit.md §3.2 件 1。
//
// 这条路径刻意绕开 AddItem 的异常检测 / transaction_log / gain-block 校验:
// 物品在源服首次入包时就已经验过一遍,这里重放会重复计数并写出重复的
// TX_SYSTEM_GRANT 记录。

void Bag::ResetFromSnapshot()
{
	// 两层一起清空。容量刻意不动 —— SetCapacityForRestore 会在
	// InsertItemForRestore 之前从快照里设好。
	store_.Clear();
	layout_->Clear();
	AssertLayerConsistency();
}

void Bag::InsertItemForRestore(Guid guid, uint32_t configId, uint32_t stackSize, uint32_t pos,
							   uint64_t acquireSeq)
{
	// 位置参数重载:只带得动身份 + 数量 + 入包序号。拼成 ItemComp 后转调整份重载,
	// 校验与落位只有那一份 —— 两个重载各写一遍,日后必然漂移。
	ItemComp proto;
	proto.set_item_id(guid);
	proto.set_config_id(configId);
	proto.set_size(stackSize);
	// 0 交给 ItemStore::Insert 按重放顺序补盖(旧存档);非 0 原样保留并抬高水位。
	proto.set_acquire_seq(acquireSeq);
	InsertItemForRestore(std::move(proto), pos);
}

void Bag::RestoreItems(std::vector<std::pair<ItemComp, uint32_t>> items)
{
	for (const auto &[item, pos] : items)
	{
		if (item.size() > 0)
		{
			store_.RaiseAcquireSeq(item.acquire_seq());
		}
	}
	for (auto &[item, pos] : items)
	{
		InsertItemForRestore(std::move(item), pos);
	}
}

void Bag::InsertItemForRestore(ItemComp item, uint32_t pos)
{
	// 零数量不代表资产，也不应占槽或消耗旧档补章序号。
	if (item.size() == 0)
	{
		return;
	}
	// 身份与模板先抄出来:item 下面会被 move 进实例层。
	const Guid guid = item.item_id();
	const uint32_t configId = item.config_id();

	if (guid == kInvalidGuid)
	{
		LOG_ERROR << "Bag::InsertItemForRestore: refusing invalid guid (configId=" << configId << ")";
		return;
	}
	if (store_.Contains(guid))
	{
		// Duplicate guid in snapshot — log and skip. Indicates upstream
		// data corruption; better to drop one copy than crash.
		LOG_ERROR << "Bag::InsertItemForRestore: duplicate guid=" << guid
				  << " configId=" << configId << " in snapshot, skipping second copy";
		return;
	}

	// 整份原样进实例层,走 ItemStore::Insert 这唯一的创建口径:
	//   * equip 段连同 presence 一起保留 —— has_equip() 是"已初始化过"的唯一判据,
	//     还原时丢了它,这件装备下次经过任何入包路径都会被当成新铸的再掷一次;
	//   * acquire_seq 为 0 由 Insert 按重放顺序补盖(旧存档),非 0 原样保留并抬高水位;
	//   * **不调 ItemInstanceInitializer** —— 还原的不是新铸实例。
	if (store_.Insert(std::move(item)) == nullptr)
	{
		LOG_ERROR << "Bag::InsertItemForRestore: duplicate guid=" << guid
				  << " configId=" << configId << ", skipping";
		return;
	}

	const Footprint footprint = FootprintFor(configId);
	bool placed = false;

	// ① 具名槽布局(装备栏):落到哪个槽由**配置数据**说了算,快照 pos 只有在配置
	//    今天仍然认可它时才算数。
	//
	//    (a) 快照 pos 这个槽**仍然接受这件东西的部位**(槽位表 + 当前容量,与
	//        SlotsAcceptingKind / PutInstance 的指定槽是同一条口径)且空着 -> 原样落回。
	//        同一部位可以有多个槽(两个手镯位),"戴在哪一只上"是编排层经 PutInstance
	//        的指定槽选定的,必须撑得过存档往返。此前这里直接取"该部位第一个空槽",
	//        而存档是按实例层的遍历序写出的(不保证按槽号):单独戴在 1 号槽的那只重登
	//        后会跑到 0 号槽,两只都戴着时每往返一次互换一次 —— 穿上规则"都占着就替换
	//        槽号最小的那只"换掉的是哪一只,也就跟着重登漂。
	//    (b) 否则(策划把这件装备从"头"改到了"胸"、槽位表改了、容量缩了、或与另一件
	//        已还原的撞位)-> 该部位的第一个空槽。老存档落到新槽位,这是"口径在数据里"
	//        的直接后果。
	if (layout_->HasSlotSemantics())
	{
		// 安静地试:还原路径不该因为配置查不到就刷错误日志,后面还有兜底。
		// (EquipKindFor 查不到表返回哨兵,下面两问对哨兵都只会答"没有"。)
		const uint32_t kind = EquipKindFor(configId);
		SlotId slot = kInvalidSlot;
		if (layout_->At(pos) == kInvalidGuid)
		{
			const std::vector<uint32_t> accepting = SlotsAcceptingKind(kind);
			if (std::find(accepting.begin(), accepting.end(), pos) != accepting.end())
			{
				slot = pos;
			}
		}
		if (slot == kInvalidSlot)
		{
			slot = FindFreeSlotForKind(kind);
		}
		if (slot != kInvalidSlot)
		{
			placed = layout_->PlaceAt(guid, slot, footprint);
		}
	}

	// ② 还原路径**绝不因为配置查不到就丢东西**。
	//
	//    这里与 AddItem 那条路径的严格程度是刻意不同的:活玩法往装备栏里塞一个
	//    没声明槽位的东西,该拒(PlaceInstance 会拒);但还原时配置查不到 ——
	//    表被裁过、这件是新版本的装备、或者干脆是脏数据 —— 把玩家的装备丢掉是
	//    远比"放错格子"更坏的结果。原则仍是 (a) 那条:**位置可以变,物品不能丢**。
	//
	//    退路一:忠实复现快照里的位置(非具名槽布局的正常路径,也是具名槽下
	//    配置查不到时的兜底)。这里**不要**自动 MergeAndCompact,玩家期望东西
	//    还在他离开时摆的地方。
	if (!placed && pos != kInvalidU32Id)
	{
		placed = layout_->PlaceAt(guid, pos, footprint);
	}

	//    退路二:快照里的 pos 也用不了(没记录、越界、或与另一件已还原的物品
	//    撞位)。拆分前这里是一句无校验的 `posToGuid[pos] = guid` —— 越界的落进
	//    一个永远扫不到的幽灵槽位,撞位的把前一件顶成没有槽位的孤儿。现在退化
	//    为自动选位。
	if (!placed)
	{
		if (const SlotId relocated = layout_->Place(guid, footprint); relocated != kInvalidSlot)
		{
			LOG_ERROR << "Bag::InsertItemForRestore: unusable snapshot pos=" << pos
					  << " for guid=" << guid << " configId=" << configId
					  << " (capacity=" << layout_->Capacity() << "); relocated to slot "
					  << relocated << ". player " << PlayerGuid();
			placed = true;
		}
	}

	if (placed)
	{
		AssertLayerConsistency();
		return;
	}

	// 三条路都走不通 = 快照里的物品比容量还多,属于上游数据损坏。
	// 宁可丢这一件并大声报错,也不能留下一个没有槽位的实例 —— 那会让实例数与
	// 槽位数永久对不上,之后每一次容量判定都是错的。
	store_.Erase(guid);
	LOG_ERROR << "Bag::InsertItemForRestore: could not place guid=" << guid
			  << " configId=" << configId << " (capacity=" << layout_->Capacity()
			  << ", occupied=" << layout_->OccupiedSlotCount()
			  << "), DROPPING it. player " << PlayerGuid();
}

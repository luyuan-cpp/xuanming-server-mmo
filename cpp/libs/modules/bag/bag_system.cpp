#include "bag_system.h"

#include <algorithm>
#include <unordered_set>
#include <utility>
#include <vector>

#include "engine/core/error_handling/error_handling.h"
#include "engine/core/macros/return_define.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/code/item_table.h"
#include "table/code/equipslot_table.h"

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

entt::entity Bag::GetItemByGuid(Guid guid)
{
	return store_.EntityOf(guid);
}

// ── 按槽位取物:布局层查 guid,实例层查内容 ───────────────────────────────

ItemComp *Bag::GetItemCompByPos(uint32_t pos)
{
	const Guid guid = layout_->At(pos);
	return guid != kInvalidGuid ? store_.Find(guid) : nullptr;
}

entt::entity Bag::GetItemByPos(uint32_t pos)
{
	const Guid guid = layout_->At(pos);
	return guid != kInvalidGuid ? store_.EntityOf(guid) : entt::null;
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
	for (const auto &[configId, count] : itemsToAdd)
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
	// 真入包路径(AddItems)走 PlanInstances + ReserveOrEvict。
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

uint32_t Bag::ReserveForBatchAdd(const ItemCountMap &itemsToAdd,
								 std::vector<DestroyedInstance> *evictedOut)
{
	// ── 全部零副作用预检,必须在腾位之前 ─────────────────────────────────
	// ① 零数量条目。CheckSpaceFor 对它宽容(预测,用例 CheckSpaceForEmptyRequest
	//    要求如此),入包不宽容:逐件 AddItem 会拒它,拖到那时才拒就是半批 ——
	//    而且拒不拒取决于 unordered_map 的哈希序,是个 heisenbug。
	for (const auto &[configId, count] : itemsToAdd)
	{
		if (count == 0)
		{
			LOG_ERROR << "Bag::ReserveForBatchAdd: config " << configId
				<< " requested with count 0; refusing the whole batch before touching anything. "
				<< "player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
	}

	// ② 配置表 / 准入 / 实例数。
	ItemCountMap plannedByConfig;
	std::size_t plannedInstances = 0;
	RETURN_ON_ERROR(PlanInstances(itemsToAdd, &plannedByConfig, &plannedInstances));

	// ③ 铸号。这个重载的调用方从不预设 guid,"要新建实例"就等价于"要铸号"。
	//    plannedInstances == 0(纯并堆)时不会触发腾位(CanReserve 恒真),所以不必
	//    担心"腾位后需求从 0 变正却铸不了号"。
	if (plannedInstances > 0 && !ItemStore::CanMintGuid())
	{
		LOG_ERROR << "Bag::ReserveForBatchAdd: item guid generator unavailable (fenced or "
			<< "uninitialised), refusing the whole batch of " << itemsToAdd.size()
			<< " config(s) needing " << plannedInstances << " new instance(s) for player "
			<< PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
	}

	// ── 预检全过,现在才允许改状态 ──────────────────────────────────────
	return ReserveBatch(itemsToAdd, nullptr, evictedOut);
}

uint32_t Bag::ReserveForBatchAdd(const std::vector<InitItemParam> &itemsToAdd,
								 std::vector<DestroyedInstance> *evictedOut)
{
	// 带完整 ItemComp 的批量(邮件附件等)。四道预检全在腾位之前;顺序与
	// ItemCountMap 重载一致,只是多了"预设 guid 撞车"这一道。

	// ① 零数量 + 汇总成 config -> 单位数。
	ItemCountMap requiredSpace;
	for (const auto &param : itemsToAdd)
	{
		if (param.itemPBComp.size() == 0)
		{
			LOG_ERROR << "Bag::ReserveForBatchAdd: piece of config " << param.itemPBComp.config_id()
				<< " has size 0; refusing the whole batch before touching anything. player "
				<< PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
		requiredSpace[param.itemPBComp.config_id()] += param.itemPBComp.size();
	}

	// ② 配置表 / 准入 / 实例数。PlanInstances 对装备(maxStack==1)同样正确 ——
	//    每个单位各占一个实例、无现有堆叠可填。
	ItemCountMap plannedByConfig;
	std::size_t instancesNeeded = 0;
	RETURN_ON_ERROR(PlanInstances(requiredSpace, &plannedByConfig, &instancesNeeded));

	// ③ 预设 guid 不能撞车 —— 既不能撞背包里已有的,也不能在这一批内部自己撞自己。
	//    邮件附件正是携带预设 guid 的典型场景,上游造数据出问题(重放 / 重复领取)时,
	//    不预检就会变成"前几件已进包、第 N 件才发现",留下半批;在会淘汰的包上更糟:
	//    先挤掉旧物、再失败。
	std::unordered_set<Guid> batchGuids;
	std::size_t preassignedInstances = 0;
	for (const auto &param : itemsToAdd)
	{
		const auto &proto = param.itemPBComp;
		LookupItemOrReturnError(proto.config_id());

		// 只有"不可叠加 + 恰好一件 + guid 合法"这一种情形会沿用预设 guid,
		// 其余一律铸号(判据与 AddNonStackableItem 里完全一致)。
		if (itemRow->max_stack_size() != 1 || proto.size() != 1 || ItemStore::IsInvalidGuid(proto))
		{
			continue;
		}
		const Guid guid = proto.item_id();
		if (store_.Contains(guid) || !batchGuids.insert(guid).second)
		{
			LOG_ERROR << "Bag::ReserveForBatchAdd: preassigned guid " << guid << " (config "
				<< proto.config_id() << ") collides with an item already in the bag or with "
				<< "another entry in the same batch; refusing the whole batch. player "
				<< PlayerGuid();
			return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
		}
		++preassignedInstances;
	}

	// ④ 铸号。除去沿用预设 guid 的那些,剩下要新建的实例都得铸号。
	if (!ItemStore::CanMintGuid())
	{
		if (instancesNeeded > preassignedInstances)
		{
			LOG_ERROR << "Bag::ReserveForBatchAdd: item guid generator unavailable (fenced or "
				<< "uninitialised), refusing the whole batch of " << itemsToAdd.size()
				<< " piece(s) needing " << (instancesNeeded - preassignedInstances)
				<< " minted instance(s) for player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
		// 发号器不可用时**连腾位也不做**:腾位可能挤掉本批打算并进去的堆,让原本
		// 纯并堆的那部分变成要铸号的溢出 —— 那就成了"先销毁、再铸不了号"。
		// 全部沿用预设 guid 且现状放得下的批次(邮件附件的典型形状)仍然放行。
		if (!CanReserve(plannedByConfig, instancesNeeded))
		{
			LOG_ERROR << "Bag::ReserveForBatchAdd: generator unavailable and the batch does not "
				<< "fit without eviction; refusing rather than evicting (post-eviction replan "
				<< "could require minting). player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
	}

	// ── 预检全过,现在才允许改状态 ──────────────────────────────────────
	return ReserveBatch(requiredSpace, nullptr, evictedOut);
}

uint32_t Bag::ReserveBatch(const ItemCountMap &itemsToAdd, std::size_t *totalInstancesOut,
						   std::vector<DestroyedInstance> *evictedOut)
{
	// 先用 PlanInstances 拿**精确错误码**(配置表 / 准入);ReserveOrEvict 只回答 bool。
	ItemCountMap instancesByConfig;
	std::size_t instancesNeeded = 0;
	RETURN_ON_ERROR(PlanInstances(itemsToAdd, &instancesByConfig, &instancesNeeded));

	// 摆得下直接过;摆不下就先算到不动点再腾位;腾不够一件都不动。
	if (!ReserveOrEvict(itemsToAdd, evictedOut))
	{
		return PrintStackAndReturnError(kBagItemNotStacked);
	}

	if (totalInstancesOut != nullptr)
	{
		// 腾位后需求可能已变(被挤掉的堆不再能并入),重算一次给调用方。
		instancesByConfig.clear();
		instancesNeeded = 0;
		RETURN_ON_ERROR(PlanInstances(itemsToAdd, &instancesByConfig, &instancesNeeded));
		*totalInstancesOut = instancesNeeded;
	}
	return kSuccess;
}

uint32_t Bag::CheckItemsAvailable(const ItemCountMap &requiredItems)
{
	// 纯实例层问题:有没有这些东西。跟摆在哪、还剩几格毫无关系。
	return store_.HasAll(requiredItems) ? kSuccess
										: PrintStackAndReturnError(kBagInsufficientItems);
}

uint32_t Bag::RemoveItems(const ItemCountMap &itemsToRemove)
{
	// 事务语义:先确认每种物品都够扣,任何一种不够就整体失败,绝不做部分删除。
	RETURN_ON_ERROR(CheckItemsAvailable(itemsToRemove));

	// 扣数量是纯实例层操作:被抽光的堆 size 变 0 但实例保留,槽位也就跟着留着。
	// 这与 RemoveItem(guid) 的"彻底销毁 + 释放槽位"是两回事。
	for (const auto &[configId, count] : itemsToRemove)
	{
		store_.DrainStacks(configId, count);
	}
	AssertLayerConsistency();
	return kSuccess;
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

	item->set_size(item->size() - param.size); // 校验全通过,扣减(扣光则 size=0,槽位保留)
	AssertLayerConsistency();
	return kSuccess;
}

// ── 整理:实例层合并 + 布局层重排 ─────────────────────────────────────────

GuidVector Bag::DesiredOrder() const
{
	// 期望顺序 (config 升序, size 降序):同种聚拢、组内满堆在前、零头落到该组
	// 末尾,得到 [config A: 满..满 零头][config B: 满..满 零头]...
	//
	// 这是**实例层数据**决定的顺序,布局层只负责按它落位 —— 于是"按什么排"
	// 是一条可以随时改的玩法规则,不必碰任何一层。
	struct OrderKey
	{
		Guid guid;
		uint32_t configId;
		uint32_t size;
	};

	std::vector<OrderKey> ordered;
	ordered.reserve(store_.Size());
	store_.ForEach([&ordered](Guid guid, const ItemComp &item)
				   { ordered.emplace_back(OrderKey{guid, item.config_id(), item.size()}); });

	std::sort(ordered.begin(), ordered.end(), [](const OrderKey &a, const OrderKey &b)
			  {
				  if (a.configId != b.configId)
				  {
					  return a.configId < b.configId; // 同种聚拢
				  }
				  return a.size > b.size;             // 满堆在前、零头在后
			  });

	GuidVector order;
	order.reserve(ordered.size());
	for (const auto &key : ordered)
	{
		order.push_back(key.guid);
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

	// ② 回收所有 size==0 的实例 —— 一次扫描,不区分来路:
	//      * ① 刚清空的可叠加堆;
	//      * 可叠加物品被 RemoveItems 抽光留下的空堆;
	//      * **不可叠加物品被 RemoveItemByPos 扣到 0 的僵尸** —— 这一类
	//        MergePartialStacks 永远碰不到(它跳过 max_stack_size() <= 1),
	//        拆分前没有任何路径回收得了,会永久占着一个格子。
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

uint32_t Bag::AddNonStackableItem(ItemComp itemProto, std::vector<Guid> *writtenGuidsOut,
								  std::vector<DestroyedInstance> *evictedOut)
{
	// 不可叠加物品(装备等):每一件都是一个独立实例,size 即"要放几件"。
	// 统一处理:把 itemProto 拆成 pieceCount 件、每件 size=1 单独入包。
	//   * 单件 (pieceCount==1):若调用方预先指定了 guid 就沿用,否则铸一个新的。
	//   * 多件 (pieceCount>1):每件都必须有各自唯一的 guid,所以一律铸新。
	const uint32_t pieceCount = itemProto.size(); // 要放入的件数

	// 只有"单件"才允许沿用调用方预设的 guid;多件无法共用一个 guid,必须各自铸新。
	const bool honorPreassignedGuid = (pieceCount == 1);

	// ── 纯预检必须全部跑在 ReserveOrEvict 之前 ──────────────────────────
	// 本次会铸号(非"单件沿用预设 guid")而发号器已不可用:在写入任何一件之前整体拒绝。
	// 见 ItemStore::CanMintGuid 注释——放行会把哨兵 guid 当 item_id 持久化。
	//
	// **顺序是硬要求,不是风格。** ReserveOrEvict 会为了腾位真的销毁实例;
	// 任何一个零副作用的预检排在它后面,就会出现"已经挤掉了玩家最早那件东西,
	// 然后整批拒绝"—— 东西白丢了。这条纪律与三段式同源:所有可能失败且不改
	// 状态的判断,一律排在唯一会改状态的那一步之前。
	const bool willMint = !honorPreassignedGuid || ItemStore::IsInvalidGuid(itemProto);
	if (willMint && !ItemStore::CanMintGuid())
	{
		LOG_ERROR << "AddNonStackableItem: item guid generator unavailable (fenced or uninitialised), "
			<< "refusing to add config " << itemProto.config_id() << " x" << pieceCount
			<< " for player " << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
	}

	// 预设 guid 撞包内已有实例 —— 零成本的纯预检,同样必须在腾位之前。
	// AddItems(vector) 的批量入口对整批做过同样的事;单件路径此前漏了这一道,
	// 于是同一预设 guid 的发放被重放时,临时格先挤掉旧物、再在 Insert 处撞 guid 失败。
	if (honorPreassignedGuid && !ItemStore::IsInvalidGuid(itemProto) &&
		store_.Contains(itemProto.item_id()))
	{
		LOG_ERROR << "AddNonStackableItem: preassigned guid " << itemProto.item_id()
			<< " (config " << itemProto.config_id() << ") already exists in this bag; refusing "
			<< "before touching anything. player " << PlayerGuid();
		return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
	}

	// reserve —— 拆分前这里是 items.size() 与 capacity 的减法,拆分后一度是
	// IsSpaceInsufficient()(只数格子)。两者对具名槽都偏乐观:装备栏"还有 8 格"
	// 不代表"还能再穿一只手镯"。CanReserve 会按部位问。
	//
	// 摆不下时按淘汰策略腾位(临时格 = 先进先出;其余包 = RejectWhenFull,
	// 等价于拆分前那句 `// TODO: overflow to temp bag or mail` 底下的直接拒绝)。
	// 入参是单位数;不可叠加物品 1 单位 = 1 实例,腾完也不会并堆,不需要重新规划。
	if (!ReserveOrEvict({{itemProto.config_id(), pieceCount}}, evictedOut))
	{
		return PrintStackAndReturnError(kBagAddItemBagFull);
	}

	for (uint32_t i = 0; i < pieceCount; ++i) // 逐件创建并入包
	{
		ItemComp piece = itemProto; // 复制一份作为这一件
		piece.set_size(1);          // 不可叠加:每件固定 size=1

		// 沿用预设 guid 仅限单件且 guid 合法;其余情况一律铸新 guid。
		if (!honorPreassignedGuid || ItemStore::IsInvalidGuid(piece))
		{
			piece.set_item_id(ItemStore::MintGuid());
			// 入口门(willMint && !CanMintGuid)已挡掉可预见的失败;这里是最后防线,
			// 铸出哨兵值绝不能落进实例仓库。tls 单线程下理论不可达,达了就是新 bug。
			if (ItemStore::IsInvalidGuid(piece))
			{
				LOG_ERROR << "AddNonStackableItem: minted an invalid guid mid-loop (piece " << i
					<< "), player " << PlayerGuid();
				return PrintStackAndReturnError(kBagAddItemInvalidParam);
			}
		}
		const auto guid = piece.item_id();

		// commit:实例层建实例,布局层给位置。两步都在同一次迭代里完成,
		// 保证不会出现"有实例没位置"或"有位置没实例"的中间态。
		if (store_.Insert(std::move(piece)) == nullptr)
		{
			LOG_ERROR << "AddNonStackableItem: duplicate guid " << guid << " player " << PlayerGuid();
			return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
		}

		// 放置失败必须回滚这一件 —— 否则实例进了仓库却没有槽位,变成一件查不到、
		// 删不掉、还占着容量的孤儿,而且零日志。入口的 IsSpaceInsufficient 已经挡掉
		// 了可预见的失败,所以走到这里说明容量判定与布局层不一致,是真 bug,要吼。
		if (PlaceInstance(guid, itemProto.config_id()) == kInvalidSlot)
		{
			store_.Erase(guid);
			LOG_ERROR << "AddNonStackableItem: layout refused to place guid " << guid
				<< " (config " << itemProto.config_id() << ", piece " << i << "/" << pieceCount
				<< ", capacity " << layout_->Capacity() << ", occupied " << layout_->OccupiedSlotCount()
				<< ") player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemBagFull);
		}

		// 回执:无论 guid 是新铸的还是沿用调用方预设的,这一件都是本次真实写入的实例。
		if (writtenGuidsOut != nullptr)
		{
			writtenGuidsOut->push_back(guid);
		}
	}
	AssertLayerConsistency();
	return kSuccess;
}

uint32_t Bag::SpillIntoNewInstances(ItemComp proto, uint32_t maxStackSize,
									uint32_t remaining, std::size_t newInstanceCount,
									std::vector<Guid> *writtenGuidsOut)
{
	for (std::size_t i = 0; i < newInstanceCount; ++i)
	{
		ItemComp piece = proto;
		piece.set_item_id(ItemStore::MintGuid());
		// 入口门(AddStackableItem 在 ApplyStackFill 前已查 CanMintGuid)挡掉可预见的
		// 失败;这里是最后防线,哨兵 guid 绝不能落进实例仓库。
		if (ItemStore::IsInvalidGuid(piece))
		{
			LOG_ERROR << "SpillIntoNewInstances: minted an invalid guid mid-loop (instance " << i
				<< "), player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}
		const uint32_t put = maxStackSize < remaining ? maxStackSize : remaining;
		piece.set_size(put);
		remaining -= put;
		const auto guid = piece.item_id();

		if (store_.Insert(std::move(piece)) == nullptr)
		{
			LOG_ERROR << "AddStackableItem: duplicate guid " << guid << " player " << PlayerGuid();
			return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
		}

		// 同 AddNonStackableItem:放置失败回滚这一堆,绝不留下没有槽位的孤儿实例。
		if (PlaceInstance(guid, proto.config_id()) == kInvalidSlot)
		{
			store_.Erase(guid);
			LOG_ERROR << "SpillIntoNewInstances: layout refused to place guid " << guid
				<< " (config " << proto.config_id() << ", instance " << i << "/" << newInstanceCount
				<< ", capacity " << layout_->Capacity() << ", occupied " << layout_->OccupiedSlotCount()
				<< ") player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemBagFull);
		}

		if (writtenGuidsOut != nullptr)
		{
			writtenGuidsOut->push_back(guid);
		}
	}
	AssertLayerConsistency();
	return kSuccess;
}

uint32_t Bag::AddStackableItem(ItemComp itemProto, uint32_t maxStackSize,
							   std::vector<Guid> *writtenGuidsOut,
							   std::vector<DestroyedInstance> *evictedOut)
{
	// 严格三段,顺序不能反 —— 一旦先改了堆才发现放不下,就会留下半完成的脏状态。
	//
	// plan(实例层,零副作用):进来的数量先尽量填进同 config 的既有堆叠,
	//                          算出还剩多少必须新建实例。
	std::vector<ItemStore::StackFill> fillPlan;
	uint32_t remaining = store_.PlanStackIntoExistingStacks(itemProto, maxStackSize, fillPlan);

	// reserve:那些新实例摆不摆得下;摆不下就问淘汰策略能不能腾。
	std::size_t newInstanceCount = 0;
	if (remaining > 0)
	{
		newInstanceCount = ItemStore::StacksNeededFor(remaining, maxStackSize);

		// ── 纯预检排在 ReserveOrEvict 之前 ─────────────────────────────
		// 溢出到新实例要铸号。发号器不可用时必须**在改动任何状态之前**拒绝:
		// 既不能先灌旧堆(违反三段),更不能先为了腾位销毁实例然后再失败
		// —— 那会把玩家最早那件东西白白弄丢。
		//
		// 这里刻意**保守**:万一腾位之后重新规划发现全并得进去、根本不用铸号,
		// 我们也已经拒绝了。宁可少收一次,不可错杀一件。
		if (!ItemStore::CanMintGuid())
		{
			LOG_ERROR << "AddStackableItem: item guid generator unavailable (fenced or uninitialised), "
				<< "refusing to add config " << itemProto.config_id() << " x" << itemProto.size()
				<< " for player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
		}

		// reserve(可腾位):入参是**单位数**,ReserveOrEvict 自己反复规划、先算到不动点
		// 再销毁 —— 它返回 true 就意味着"腾位之后的需求"一定放得下,包括被挤掉的
		// 恰是本次打算并进去的那个未满堆的情形。摆得下时它什么都不做。
		if (!ReserveOrEvict({{itemProto.config_id(), itemProto.size()}}, evictedOut))
		{
			return PrintStackAndReturnError(kBagAddItemBagFull);
		}

		// **腾位之后必须重新规划。** 淘汰可能刚好销毁了本次打算并进去的那个未满堆
		// (临时格里同 config 的旧堆恰恰是最早进包的),于是上面那份 fillPlan 里的
		// entity 已经失效。不重算就会拿着悬空的 entity 去 ApplyStackFill。
		// 无淘汰时重算结果与第一次相同 —— 一次遍历,换一个不靠调用方记性的保证。
		fillPlan.clear();
		remaining = store_.PlanStackIntoExistingStacks(itemProto, maxStackSize, fillPlan);
		newInstanceCount = remaining > 0 ? ItemStore::StacksNeededFor(remaining, maxStackSize) : 0;
		if (remaining > 0 &&
			!CanReserve({{itemProto.config_id(), static_cast<uint32_t>(newInstanceCount)}},
						newInstanceCount))
		{
			// ReserveOrEvict 的不动点已经保证放得下;这里为假只可能是策略 / 布局不自洽
			// 的编程错误。东西可能已经销毁、回不来了,必须吼。
			LOG_ERROR << "AddStackableItem: cannot reserve " << newInstanceCount
				<< " instance(s) for config " << itemProto.config_id()
				<< " right after ReserveOrEvict succeeded; policy and layout disagree "
				<< "(programming error). player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemBagFull);
		}
	}

	// commit:先灌既有堆(纯实例层,不占新格子)……
	store_.ApplyStackFill(fillPlan, writtenGuidsOut);

	if (remaining == 0)
	{
		return kSuccess;
	}
	// ……再把溢出量铺进新实例并各自要一个位置(两层)。
	return SpillIntoNewInstances(std::move(itemProto), maxStackSize, remaining, newInstanceCount,
								 writtenGuidsOut);
}

uint32_t Bag::AddItem(const InitItemParam &initItemParam, std::vector<Guid> *writtenGuidsOut,
					  std::vector<DestroyedInstance> *evictedOut)
{
	auto itemProto = initItemParam.itemPBComp;
	// config_id / size 都是无符号:== 0 即"没指定物品"或"数量为 0",均属非法入参。
	if (itemProto.config_id() == 0 || itemProto.size() == 0)
	{
		LOG_ERROR << "bag add item player:" << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
	}

	LookupItemOrReturnError(itemProto.config_id());

	if (itemRow->max_stack_size() == 0)
	{
		return PrintStackAndReturnError(kInvalidTableData);
	}

	// 准入:这个包收不收这种东西(节日包只收节日道具之类)。纯 config 判定,
	// 零副作用,放在写入之前 —— 三段纪律的 reserve 段。
	// 放在 AddItem 而不是两个 AddXxxItem 里,是因为它与堆不堆叠无关,
	// 而这里是两条路径唯一的共同入口。
	if (!admission_->Accepts(itemProto.config_id()))
	{
		LOG_ERROR << "Bag::AddItem: this bag does not admit config " << itemProto.config_id()
			<< "; refusing. player " << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
	}

	if (itemRow->max_stack_size() == 1)
	{
		return AddNonStackableItem(std::move(itemProto), writtenGuidsOut, evictedOut);
	}

	return AddStackableItem(std::move(itemProto), itemRow->max_stack_size(), writtenGuidsOut,
							evictedOut);
}

uint32_t Bag::AddItems(const ItemCountMap &itemsToAdd,
					   std::vector<DestroyedInstance> *evictedOut)
{
	// 事务语义:与 RemoveItems 完全对称。任何一项不满足就整体失败,
	// 绝不做"前几种已进包、后一种失败"的部分添加。
	//
	// 全部纯预检(零数量 / 配置表 / 准入 / 铸号)与腾位都在 ReserveForBatchAdd 里,
	// 且预检严格排在腾位之前。**这里不再自己做一遍** —— 两份预检会漂移:BagService
	// 有另一套批量循环,第一版就因为它不经过这里而漏掉了预检,发号器被 fence 时
	// 临时格先挤掉旧物、再整批失败。预检并进 reserve 入口之后,谁调都一样。
	RETURN_ON_ERROR(ReserveForBatchAdd(itemsToAdd, evictedOut));

	// 位已腾够、预检全过,逐个按配置发放。每个 config 复用单个 AddItem 的完整逻辑
	// (堆叠/非堆叠分发、要位置等),主函数只表达"批量发放"的意图。
	for (const auto &[configId, count] : itemsToAdd)
	{
		InitItemParam param;
		param.itemPBComp.set_config_id(configId);
		param.itemPBComp.set_size(count);
		// evictedOut 继续透传:ReserveBatch 已经腾够了位,这里正常不会再淘汰,
		// 真淘汰了也必须留痕(那说明 ReserveBatch 的规划漏了什么,要能查)。
		RETURN_ON_ERROR(AddItem(param, nullptr, evictedOut));
	}
	return kSuccess;
}

uint32_t Bag::AddItems(const std::vector<InitItemParam> &itemsToAdd,
					   std::vector<DestroyedInstance> *evictedOut)
{
	// 带完整 ItemComp 的批量发放(邮件附件等):列表里可同时混有装备和普通物品。
	// 与 ItemCountMap 版的区别:每个 InitItemParam 携带各自完整的 ItemComp
	//   (预设 guid、强化等级、随机词条、绑定状态……),逐件原样写入,绝不丢属性。

	// 事务语义:四道预检(零数量 / 配置表与准入 / 预设 guid 撞车 / 铸号)全过、
	// 且位已腾够之后才开始写入;任何一道不过都整体失败,绝不做"前几件已进包、
	// 后一件失败"的部分添加。邮件附件是这条路径的主要用户,半批发放会让调用方
	// 以为整批失败而重发,变成复制道具。
	//
	// 预检与腾位都在 ReserveForBatchAdd(vector 重载)里,且预检严格排在腾位之前。
	// **这里不再自己做一遍**(理由见 ItemCountMap 重载)。
	//
	// 剩下唯一还能中途失败的是"布局层说能放却放不下",那是编程错误,由
	// AssertLayerConsistency 兜底,不是数据能触发的情形。
	RETURN_ON_ERROR(ReserveForBatchAdd(itemsToAdd, evictedOut));

	// 逐件发放,复用单个 AddItem 的完整逻辑(装备走非堆叠、普通物品走堆叠)。
	for (const auto &param : itemsToAdd)
	{
		RETURN_ON_ERROR(AddItem(param, nullptr, evictedOut));
	}
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

// 这一行槽位,对这个部位来说是不是一个"可用的空槽"。
//
// **FindFreeSlotForKind(commit 侧)与 CountFreeSlotsForKind(reserve 侧)必须
// 共用这一条判据。** 两侧一旦各写各的,reserve 数出来的空槽就可能不是 commit
// 真能落位的那些,于是"预检通过、写到一半失败"会以新的形式复活 —— 那正是
// CanReserve 要消灭的 bug 本身。
static bool IsFreeSlotForEquipKind(const EquipSlotTable &slotRow, uint32_t kind,
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
	if (slot >= layout.Capacity())
	{
		return false;
	}
	return layout.At(slot) == kInvalidGuid; // 空着才算
}

SlotId Bag::FindFreeSlotForKind(uint32_t kind) const
{
	// 找的是**第一个空的**接受槽,不是唯一那个 —— 同一部位允许有多个槽
	// (两个手镯位 = 表里两行 equip_kind 相同)。上一版模型把部位和槽号
	// 混成了一个字段,根本表达不了这个需求。
	if (kind == 0)
	{
		return kInvalidSlot;
	}
	for (const auto &slotRow : EquipSlotTableManager::Instance().FindAll().data())
	{
		if (IsFreeSlotForEquipKind(slotRow, kind, *layout_))
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

bool Bag::ReserveOrEvict(const ItemCountMap &unitsToAdd,
						 std::vector<DestroyedInstance> *evictedOut)
{
	// 第一问永远是"照现在这样放得下吗" —— 放得下就什么都不做,淘汰只在真放不下时发生。
	ItemCountMap need;
	std::size_t needTotal = 0;
	if (PlanInstances(unitsToAdd, &need, &needTotal) != kSuccess)
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
	if (evictedOut == nullptr)
	{
		LOG_ERROR << "Bag::ReserveOrEvict: eviction would be needed but no receipt sink was "
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
	GuidVector victimOrder; // 保留策略给出的顺序,回执按它落流水
	bool converged = false;
	for (std::size_t round = 0; round <= store_.Size(); ++round)
	{
		ItemCountMap needIfEvicted;
		std::size_t needIfEvictedTotal = 0;
		if (PlanInstances(unitsToAdd, &needIfEvicted, &needIfEvictedTotal, &victimSet) != kSuccess)
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
		LOG_ERROR << "Bag::ReserveOrEvict: eviction planning did not converge within "
			<< store_.Size() << " round(s); refusing without destroying anything. player "
			<< PlayerGuid();
		return false;
	}

	// commit(腾位):这是一次**独立且完整**的提交,发生在本批写入任何东西之前。
	// 于是三段纪律仍然成立:要么腾完再放,要么一件不动。
	for (const Guid guid : victimOrder)
	{
		// 销毁前抓拍 —— item_uuid 是 transaction_log 做外挂回收的关联键,
		// 由 BagService 落 LogItemDestroy。形状与 MergeAndCompact 的回执一致。
		if (const auto *item = store_.Find(guid); item != nullptr)
		{
			evictedOut->push_back(DestroyedInstance{guid, item->config_id(), item->size()});
		}
		DestroyItem(guid); // 两层成对销毁,这是桥层的活
	}
	AssertLayerConsistency();

	// 收尾核对:不动点已经保证放得下。这里为 false 只可能是策略 / 布局不自洽的
	// 编程错误(例如策略返回了不在 store 里的 guid)。东西已经销毁、回不来,必须吼。
	ItemCountMap after;
	std::size_t afterTotal = 0;
	if (PlanInstances(unitsToAdd, &after, &afterTotal) != kSuccess ||
		!CanReserve(after, afterTotal))
	{
		LOG_ERROR << "Bag::ReserveOrEvict: evicted " << victimOrder.size()
			<< " instance(s) but still cannot reserve " << afterTotal
			<< "; eviction policy and layout disagree (programming error). player "
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

SlotId Bag::PlaceInstance(Guid guid, uint32_t configId)
{
	const Footprint footprint = FootprintFor(configId);

	// 槽位没有语义的布局(人物背包 / 仓库 / 临时格 / 格子背包):位置本身不表达
	// 任何东西,first-fit 即可 —— 与拆分前完全一致。一件装备躺在人物背包里时
	// 走的就是这条路,它不该被强行塞进"装备槽 3"。
	if (!layout_->HasSlotSemantics())
	{
		return layout_->Place(guid, footprint);
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
	const SlotId slot = FindFreeSlotForKind(kind);
	if (slot == kInvalidSlot)
	{
		// 该部位的槽全被占满了(或者槽位表压根没给这个部位开过槽)。
		LOG_ERROR << "Bag::PlaceInstance: no free slot accepts equip kind " << kind
			<< " (config " << configId << "); refusing. player " << PlayerGuid();
		return kInvalidSlot;
	}
	if (!layout_->PlaceAt(guid, slot, footprint))
	{
		LOG_ERROR << "Bag::PlaceInstance: equip slot " << slot << " for config " << configId
			<< " is occupied or out of range (capacity " << layout_->Capacity()
			<< "); refusing. player " << PlayerGuid();
		return kInvalidSlot;
	}
	return slot;
}

void Bag::DestroyItem(Guid guid)
{
	// 两层成对销毁 —— 少做任何一半都会留下腐化:
	//   只删实例:槽位被一个幽灵 guid 永久占着,背包会"莫名其妙满了";
	//   只删槽位:size=0 的孤儿实例仍在 view<ItemComp> 里,后续并堆会往一个
	//             已经没有位置的实例上灌数量,单位就静默丢了。
	// 拆分前这两半写在同一个函数体里(destroy 实体 + 线扫 posToGuid),
	// 现在是桥层显式地各调一次 —— 这正是桥层存在的理由。
	store_.Erase(guid);
	layout_->Remove(guid);
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

	// Build the ItemComp the same shape AddItem would, then route through
	// the single ItemStore::Insert chokepoint so the store stays consistent.
	ItemComp proto;
	proto.set_item_id(guid);
	proto.set_config_id(configId);
	proto.set_size(stackSize);
	// 0 交给 ItemStore::Insert 按重放顺序补盖(旧存档);非 0 原样保留并抬高水位。
	proto.set_acquire_seq(acquireSeq);

	if (store_.Insert(std::move(proto)) == nullptr)
	{
		LOG_ERROR << "Bag::InsertItemForRestore: duplicate guid=" << guid
				  << " configId=" << configId << ", skipping";
		return;
	}

	const Footprint footprint = FootprintFor(configId);
	bool placed = false;

	// ① 具名槽布局(装备栏):优先按**配置数据**落位。策划把某件装备从"头"改到
	//    "胸"之后,老存档还原就该落到新槽位 —— 这是"口径在数据里"的直接后果。
	if (layout_->HasSlotSemantics())
	{
		// 安静地试:还原路径不该因为配置查不到就刷错误日志,后面还有兜底。
		if (const SlotId slot = FindFreeSlotForKind(EquipKindFor(configId));
			slot != kInvalidSlot)
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

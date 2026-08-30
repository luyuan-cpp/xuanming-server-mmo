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

uint32_t Bag::CheckSpaceFor(const ItemCountMap &itemsToAdd, std::size_t *instancesNeededOut)
{
	// 只读预检(不改背包):背包能否一次性放下 itemsToAdd 的每一项?
	//
	// plan(实例层):可叠加物品先填满同 config 现有堆叠的空余,只有溢出部分才
	//                需要**新建实例**;不可叠加(maxStack==1)每个单位一个实例。
	// reserve(布局层):这些新实例摆不摆得下。
	//
	// 拆分前这两步都用 Bag 自己的 items/capacity 算,"要几个实例"和"占几格"
	// 被 GridsNeededFor 这一个名字混成了一件事。
	AssertCapacityInvariant();

	const ItemCountMap freeRoomByConfig = store_.MeasureFreeRoomPerConfig(itemsToAdd);

	std::size_t instancesNeeded = 0;
	for (const auto &[configId, count] : itemsToAdd)
	{
		LookupItemOrReturnError(configId);
		const uint32_t maxStack = itemRow->max_stack_size();
		if (maxStack == 0)
		{
			LOG_ERROR << "config error:" << configId << " player:" << PlayerGuid();
			return PrintStackAndReturnError(kInvalidTableData);
		}

		const auto roomIt = freeRoomByConfig.find(configId);
		const uint32_t freeRoom = (roomIt != freeRoomByConfig.end()) ? roomIt->second : 0;
		const uint32_t overflow = count > freeRoom ? count - freeRoom : 0;
		instancesNeeded += ItemStore::StacksNeededFor(overflow, maxStack);
	}

	// 整批一次判定。今天 FootprintFor 恒为 1x1,所以一次 CanFit 就够;等物品
	// 真有了宽高(格子背包),这里要改成按 config 分组、逐形状递交给布局层。
	if (!layout_->CanFit(instancesNeeded, kSingleCell))
	{
		return PrintStackAndReturnError(kBagItemNotStacked);
	}

	if (instancesNeededOut != nullptr)
	{
		*instancesNeededOut = instancesNeeded;
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

uint32_t Bag::AddNonStackableItem(ItemComp itemProto, std::vector<Guid> *writtenGuidsOut)
{
	// 不可叠加物品(装备等):每一件都是一个独立实例,size 即"要放几件"。
	// 统一处理:把 itemProto 拆成 pieceCount 件、每件 size=1 单独入包。
	//   * 单件 (pieceCount==1):若调用方预先指定了 guid 就沿用,否则铸一个新的。
	//   * 多件 (pieceCount>1):每件都必须有各自唯一的 guid,所以一律铸新。
	const uint32_t pieceCount = itemProto.size(); // 要放入的件数

	// reserve:布局层判定 —— 拆分前这里是 items.size() 与 capacity 的减法。
	if (IsSpaceInsufficient(pieceCount))
	{
		// TODO: overflow to temp bag or mail
		return PrintStackAndReturnError(kBagAddItemBagFull);
	}

	// 只有"单件"才允许沿用调用方预设的 guid;多件无法共用一个 guid,必须各自铸新。
	const bool honorPreassignedGuid = (pieceCount == 1);

	// 本次会铸号(非"单件沿用预设 guid")而发号器已不可用:在写入任何一件之前整体拒绝。
	// 见 ItemStore::CanMintGuid 注释——放行会把哨兵 guid 当 item_id 持久化。
	const bool willMint = !honorPreassignedGuid || ItemStore::IsInvalidGuid(itemProto);
	if (willMint && !ItemStore::CanMintGuid())
	{
		LOG_ERROR << "AddNonStackableItem: item guid generator unavailable (fenced or uninitialised), "
			<< "refusing to add config " << itemProto.config_id() << " x" << pieceCount
			<< " for player " << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
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
							   std::vector<Guid> *writtenGuidsOut)
{
	// 严格三段,顺序不能反 —— 一旦先改了堆才发现放不下,就会留下半完成的脏状态。
	//
	// plan(实例层,零副作用):进来的数量先尽量填进同 config 的既有堆叠,
	//                          算出还剩多少必须新建实例。
	std::vector<ItemStore::StackFill> fillPlan;
	const uint32_t remaining = store_.PlanStackIntoExistingStacks(itemProto, maxStackSize, fillPlan);

	// reserve(布局层,零副作用):那些新实例摆不摆得下。
	std::size_t newInstanceCount = 0;
	if (remaining > 0)
	{
		newInstanceCount = ItemStore::StacksNeededFor(remaining, maxStackSize);
		if (IsSpaceInsufficient(newInstanceCount))
		{
			return PrintStackAndReturnError(kBagAddItemBagFull);
		}
		// 溢出到新实例要铸号。发号器不可用时必须**在 ApplyStackFill 之前**拒绝 ——
		// 一旦先灌了旧堆才在 SpillIntoNewInstances 里失败,就违反了三段纪律,
		// 留下半完成的脏状态。见 ItemStore::CanMintGuid 注释。
		if (!ItemStore::CanMintGuid())
		{
			LOG_ERROR << "AddStackableItem: item guid generator unavailable (fenced or uninitialised), "
				<< "refusing to add config " << itemProto.config_id() << " x" << itemProto.size()
				<< " for player " << PlayerGuid();
			return PrintStackAndReturnError(kBagAddItemInvalidParam);
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

uint32_t Bag::AddItem(const InitItemParam &initItemParam, std::vector<Guid> *writtenGuidsOut)
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

	if (itemRow->max_stack_size() == 1)
	{
		return AddNonStackableItem(std::move(itemProto), writtenGuidsOut);
	}

	return AddStackableItem(std::move(itemProto), itemRow->max_stack_size(), writtenGuidsOut);
}

uint32_t Bag::AddItems(const ItemCountMap &itemsToAdd)
{
	// 事务语义:与 RemoveItems 完全对称。任何一项不满足就整体失败,
	// 绝不做"前几种已进包、后一种失败"的部分添加。
	//
	// 光有 CheckSpaceFor 不够 —— 它只挡容量与配置表。**发号器**是第二条会让
	// 循环中途失败的路径:如果第一种物品纯并堆(不铸号)成功了、第二种要溢出到
	// 新实例却发现发号器被 fence,就留下了半批。所以这里在写入任何东西之前
	// 把两件事一起预检掉。
	std::size_t instancesNeeded = 0;
	RETURN_ON_ERROR(CheckSpaceFor(itemsToAdd, &instancesNeeded));

	// 这个重载的调用方从不预设 guid,所以"要新建实例"就等价于"要铸号"。
	if (instancesNeeded > 0 && !ItemStore::CanMintGuid())
	{
		LOG_ERROR << "Bag::AddItems: item guid generator unavailable (fenced or uninitialised), "
			<< "refusing the whole batch of " << itemsToAdd.size() << " config(s) needing "
			<< instancesNeeded << " new instance(s) for player " << PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
	}

	// 校验通过后逐个按配置发放。每个 config 复用单个 AddItem 的完整逻辑
	// (堆叠/非堆叠分发、要位置等),主函数只表达"批量发放"的意图。
	for (const auto &[configId, count] : itemsToAdd)
	{
		InitItemParam param;
		param.itemPBComp.set_config_id(configId);
		param.itemPBComp.set_size(count);
		RETURN_ON_ERROR(AddItem(param));
	}
	return kSuccess;
}

uint32_t Bag::AddItems(const std::vector<InitItemParam> &itemsToAdd)
{
	// 带完整 ItemComp 的批量发放(邮件附件等):列表里可同时混有装备和普通物品。
	// 与 ItemCountMap 版的区别:每个 InitItemParam 携带各自完整的 ItemComp
	//   (预设 guid、强化等级、随机词条、绑定状态……),逐件原样写入,绝不丢属性。

	// 事务语义:三道预检全过之后才开始写入,任何一道不过都整体失败,
	// 绝不做"前几件已进包、后一件失败"的部分添加。
	//
	// 这三道**必须都在这里**,不能靠逐件 AddItem 各自把关 —— 那样第 N 件才发现
	// 问题时,前 N-1 件已经写进去了。邮件附件是这条路径的主要用户,半批发放会
	// 让调用方以为整批失败而重发,变成复制道具。
	//
	// 剩下唯一还能中途失败的是"布局层说能放却放不下",那是编程错误,由
	// AssertLayerConsistency 兜底,不是数据能触发的情形。

	// 预检一:容量与配置表。汇总成 config->总数一次性校验。
	// CheckSpaceFor 对装备(maxStack==1)同样正确 —— 每个单位各占一个实例、
	// 无现有堆叠可填。
	ItemCountMap requiredSpace;
	for (const auto &param : itemsToAdd)
	{
		requiredSpace[param.itemPBComp.config_id()] += param.itemPBComp.size();
	}
	std::size_t instancesNeeded = 0;
	RETURN_ON_ERROR(CheckSpaceFor(requiredSpace, &instancesNeeded));

	// 预检二:预设 guid 不能撞车 —— 既不能撞背包里已有的,也不能在这一批内部
	// 自己撞自己。邮件附件正是携带预设 guid 的典型场景,上游造数据出问题时,
	// 不预检就会变成"前几件已进包、第 N 件才发现",留下半批。
	std::unordered_set<Guid> batchGuids;
	std::size_t preassignedInstances = 0;
	for (const auto &param : itemsToAdd)
	{
		const auto &proto = param.itemPBComp;
		LookupItemOrReturnError(proto.config_id());

		// 只有"不可叠加 + 恰好一件 + guid 合法"这一种情形会沿用预设 guid,
		// 其余一律铸号(判据与 AddNonStackableItem 里完全一致)。
		if (itemRow->max_stack_size() != 1 || proto.size() != 1 ||
			ItemStore::IsInvalidGuid(proto))
		{
			continue;
		}

		const Guid guid = proto.item_id();
		if (store_.Contains(guid) || !batchGuids.insert(guid).second)
		{
			LOG_ERROR << "Bag::AddItems: preassigned guid " << guid << " (config "
				<< proto.config_id() << ") collides with an item already in the bag or with "
				<< "another entry in the same batch; refusing the whole batch. player "
				<< PlayerGuid();
			return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
		}
		++preassignedInstances;
	}

	// 预检三:除去那些沿用预设 guid 的,剩下要新建的实例都得铸号。发号器被
	// fence 时必须在写入任何一件之前整体拒绝 —— 否则"前几件纯并堆成功、后一件
	// 要铸号却失败"同样留下半批。
	if (instancesNeeded > preassignedInstances && !ItemStore::CanMintGuid())
	{
		LOG_ERROR << "Bag::AddItems: item guid generator unavailable (fenced or uninitialised), "
			<< "refusing the whole batch of " << itemsToAdd.size() << " piece(s) needing "
			<< (instancesNeeded - preassignedInstances) << " minted instance(s) for player "
			<< PlayerGuid();
		return PrintStackAndReturnError(kBagAddItemInvalidParam);
	}

	// 校验通过后逐件发放,复用单个 AddItem 的完整逻辑(装备走非堆叠、普通物品走堆叠)。
	for (const auto &param : itemsToAdd)
	{
		RETURN_ON_ERROR(AddItem(param));
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

SlotId Bag::FindFreeSlotForKind(uint32_t kind) const
{
	// 槽位表说了算:哪个槽接受哪个部位,完全是数据。表里没出现的槽位不接受任何东西。
	//
	// 找的是**第一个空的**接受槽,不是唯一那个 —— 同一部位允许有多个槽
	// (两个手镯位 = 表里两行 equip_kind 相同)。上一版模型把部位和槽号
	// 混成了一个字段,根本表达不了这个需求。
	if (kind == 0)
	{
		return kInvalidSlot;
	}
	for (const auto &slotRow : EquipSlotTableManager::Instance().FindAll().data())
	{
		if (slotRow.equip_kind() != kind)
		{
			continue;
		}
		const auto slot = static_cast<SlotId>(slotRow.id());
		if (layout_->At(slot) != kInvalidGuid)
		{
			continue; // 这个部位的这一格已经穿着东西了,看下一格
		}
		return slot;
	}
	return kInvalidSlot;
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

void Bag::InsertItemForRestore(Guid guid, uint32_t configId, uint32_t stackSize, uint32_t pos)
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

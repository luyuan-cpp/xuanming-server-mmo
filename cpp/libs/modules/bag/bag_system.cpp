#include "bag_system.h"

#include <algorithm>
#include <vector>

#include "engine/core/error_handling/error_handling.h"
#include "engine/core/macros/return_define.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "core/utils/id/snow_flake.h"
#include "table/code/item_table.h"
#include "core/utils/defer/defer.h"
#include <thread_context/snow_flake_manager.h>

std::size_t Bag::GetTotalItemCount(uint32_t config_id) const
{
	std::size_t totalSize = 0;
	for (const auto &[entity, item] : itemRegistry.view<ItemComp>().each())
	{
		if (item.config_id() == config_id)
		{
			totalSize += item.size();
		}
	}
	return totalSize;
}

ItemComp *Bag::GetItemCompByGuid(Guid guid)
{
	auto guidIt = items.find(guid);
	return (guidIt != items.end()) ? itemRegistry.try_get<ItemComp>(guidIt->second) : nullptr;
}

ItemComp *Bag::GetItemCompByPos(uint32_t pos)
{
	auto posIt = posToGuid.find(pos);
	return (posIt != posToGuid.end()) ? GetItemCompByGuid(posIt->second) : nullptr;
}

entt::entity Bag::GetItemByGuid(Guid guid)
{
	auto guidIt = items.find(guid);
	return (guidIt != items.end()) ? guidIt->second : entt::null;
}

entt::entity Bag::GetItemByPos(uint32_t pos)
{
	auto posIt = posToGuid.find(pos);
	return (posIt != posToGuid.end()) ? GetItemByGuid(posIt->second) : entt::null;
}

ItemCountMap Bag::MeasureFreeRoomPerConfig(const ItemCountMap &itemsToAdd) const
{
	ItemCountMap freeRoomByConfig;
	for (const auto &[entity, item] : itemRegistry.view<ItemComp>().each())
	{
		if (!itemsToAdd.contains(item.config_id()))
		{
			continue;
		}
		LookupItemOrContinue(item.config_id());
		const uint32_t maxStack = itemRow->max_stack_size();
		// `<` 而非 `!=`:防止脏数据"超满堆叠"在做减法时无符号下溢成巨大值。
		if (item.size() < maxStack)
		{
			freeRoomByConfig[item.config_id()] += maxStack - item.size();
		}
	}
	return freeRoomByConfig;
}

uint32_t Bag::CheckSpaceFor(const ItemCountMap &itemsToAdd)
{
	// 只读容量预检(不改背包):背包能否一次性放下 itemsToAdd 的每一项?
	// 可叠加物品先填满同 config 现有堆叠的空余,只有溢出部分才占新格子;
	// 不可叠加物品(maxStack==1)每个单位都占一格。
	// 做法:先量出现有堆叠的空余,再算整批请求总共还需多少新格子,最后比一次空格数。
	const ItemCountMap freeRoomByConfig = MeasureFreeRoomPerConfig(itemsToAdd);

	std::size_t gridsNeeded = 0;
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
		gridsNeeded += GridsNeededFor(overflow, maxStack);
	}

	if (gridsNeeded > EmptyGridCount())
	{
		return PrintStackAndReturnError(kBagItemNotStacked);
	}
	return kSuccess;
}

uint32_t Bag::CheckItemsAvailable(const ItemCountMap &requiredItems)
{
	// 只读检查:背包里是否拥有 requiredItems 要求的每一项 (config_id -> 需求数量)。
	// 思路:把需求拷一份,遍历背包逐个堆叠去抵扣,需求全部清零即满足。
	auto itemsToCheck = requiredItems; // 拷贝一份需求,边遍历边扣减(不污染入参)

	for (const auto &[entity, item] : itemRegistry.view<ItemComp>().each()) // 遍历背包内每个物品实体
	{
		auto it = itemsToCheck.find(item.config_id()); // 这个 config 在需求清单里吗
		if (it == itemsToCheck.end())
		{
			continue; // 不是需要的物品,跳过
		}
		if (item.size() >= it->second)
		{
			itemsToCheck.erase(it); // 这一堆就够抵该 config 的全部剩余需求 -> 该项满足
			continue;
		}
		it->second -= item.size(); // 这一堆只够抵一部分 -> 扣减后继续找后面的同 config 堆叠
	}

	// 清单空 => 所有需求都被满足;否则返回物品不足错误。
	return itemsToCheck.empty() ? kSuccess : PrintStackAndReturnError(kBagInsufficientItems);
}

// 把某个 config 需要扣除的数量从它的若干堆叠里抽走。
// 同一种物品的数量可能分散在多个堆叠里(例如 2.5 个满堆),所以要逐堆抽,
// 直到抽够 count 为止。被抽光的堆叠 size 变 0 但格子保留(items/posToGuid 不删),
// 后续 AddStackableItem 可以再填回去——这与 RemoveItem(guid) 的"彻底删格"不同。
// 前置条件:调用方已用 CheckItemsAvailable 确认库存足够,这里不再校验。
void Bag::DrainItemStacks(uint32_t configId, uint32_t count)
{
	for (const auto &[entity, item] : itemRegistry.view<ItemComp>().each()) // 遍历背包堆叠
	{
		if (count == 0)
		{
			break; // 这种物品已经抽够了
		}
		if (item.config_id() != configId)
		{
			continue; // 不是目标物品,跳过
		}
		const uint32_t take = count < item.size() ? count : item.size(); // 这一堆最多能抽多少(需求与库存取小)
		item.set_size(item.size() - take);                               // 抽走(抽光则 size=0,留作空格)
		count -= take;                                                   // 更新还需抽取的数量
	}
}

uint32_t Bag::RemoveItems(const ItemCountMap &itemsToRemove)
{
	// 事务语义:先确认每种物品都够扣,任何一种不够就整体失败,绝不做部分删除。
	RETURN_ON_ERROR(CheckItemsAvailable(itemsToRemove));

	// 主流程到这里就一句话:每种物品要扣多少,就从它的堆叠里抽走多少。
	// "跨多堆抽取"的细节被收进 DrainItemStacks,主函数只表达意图。
	for (const auto &[configId, count] : itemsToRemove)
	{
		DrainItemStacks(configId, count);
	}
	return kSuccess;
}

uint32_t Bag::RemoveItemByPos(const RemoveItemByPosParam &param)
{
	// 按"格子位置"精确删除指定物品的若干数量。调用方给出 pos/guid/config/size,
	// 必须四者全部对得上才会扣减——这是一连串卫语句(guard),任何一项不一致就
	// 立刻返回对应错误,绝不误删别的格子。全部是 O(1) 哈希查找,无遍历。

	if (param.size == 0) // 删除数量必须 > 0(size 是无符号,== 0 即"没要求删任何东西")
	{
		return PrintStackAndReturnError(kBagDelItemSize);
	}

	auto posIt = posToGuid.find(param.pos); // 1) 这个格子位置存在吗
	if (posIt == posToGuid.end())
	{
		return PrintStackAndReturnError(kBagDelItemPos);
	}

	if (posIt->second != param.item_guid) // 2) 该位置上的物品 guid 是否就是调用方说的那个
	{
		return PrintStackAndReturnError(kBagDelItemGuid);
	}

	auto guidIt = items.find(param.item_guid); // 3) 该 guid 在物品表里真的存在吗
	if (guidIt == items.end())
	{
		return PrintStackAndReturnError(kBagDelItemFindItem);
	}

	auto &item = itemRegistry.get<ItemComp>(guidIt->second); // 取出该物品组件
	if (item.config_id() != param.item_config_id) // 4) 物品的 config 是否与调用方一致(防张冠李戴)
	{
		return PrintStackAndReturnError(kBagDelItemConfig);
	}

	if (item.size() < param.size) // 5) 这一堆的数量是否够扣
	{
		return PrintStackAndReturnError(kBagItemDeletionSizeMismatch);
	}

	item.set_size(item.size() - param.size); // 校验全通过,扣减数量(扣光则 size=0,格子保留作空格)
	return kSuccess;
}

bool Bag::IsAlreadyMergedAndCompact() const
{
	// 只读判断"是否还需要整理",不改任何状态。三件事都满足才算已最优:
	//   ① 没有可合并的零散堆:每个可叠加 config 至多只有 1 个未满堆。
	//      同种物品一旦出现 >=2 个未满堆(一高一低 / 两个半堆),就能合并出
	//      更少的格子,必须整理。这一条独立于格子是否紧凑。
	//   ② 格子已紧凑:posToGuid 的键正好是 0,1,...,n-1,没有中间空洞。
	//   ③ 排布已是 (config 升序, size 降序):同种物品聚在一起、组内满堆在前、
	//      零头落到该组末尾。按 pos 0..n-1 顺序读出,相邻两格必须满足
	//      "config 不减;同 config 时 size 不增"。任何一处违反(同种没聚拢、
	//      或组内满堆排在零头后面),就算位置已连续也要整理。

	// ① 检查是否存在"同 config 出现 >=2 个未满堆"。
	std::unordered_map<uint32_t, uint32_t> partialCountByConfig;
	for (auto &&[entity, item] : itemRegistry.view<ItemComp>().each())
	{
		LookupItemOrContinue(item.config_id());
		if (itemRow->max_stack_size() <= 1)
		{
			continue; // 不可叠加:不参与合并
		}
		if (item.size() >= itemRow->max_stack_size())
		{
			continue; // 满堆:不参与合并
		}
		if (++partialCountByConfig[item.config_id()] >= 2)
		{
			return false; // 同种有 >=2 个零散堆,可合并 -> 需要整理
		}
	}

	// ② 检查格子是否紧凑:键唯一且数量 == size,只要每个键都 < size,
	//    就等价于键集合恰为 {0,1,...,size-1}。
	for (const auto &[pos, guid] : posToGuid)
	{
		if (pos >= posToGuid.size())
		{
			return false; // 存在空洞 -> 需要整理
		}
	}

	// ③ 按 pos 顺序检查 (config 升序, size 降序)。
	bool hasPrev = false;
	uint32_t prevConfig = 0;
	uint32_t prevSize = 0;
	for (uint32_t pos = 0; pos < posToGuid.size(); ++pos)
	{
		auto posIt = posToGuid.find(pos);
		if (posIt == posToGuid.end())
		{
			return false; // 不连续(②应已挡住,这里双保险)
		}
		auto guidIt = items.find(posIt->second);
		if (guidIt == items.end())
		{
			return false; // 容器不一致 -> 交给整理重建
		}
		const auto &item = itemRegistry.get<ItemComp>(guidIt->second);
		const uint32_t curConfig = item.config_id();
		const uint32_t curSize = item.size();
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

	return true; // 无可合并的堆、格子已紧凑,且已是 (config 升序, size 降序) 布局
}

bool Bag::MergeAndCompact()
{
	// 背包整理:把同一种可叠加物品散落各格的"零头"合并成尽量少的满堆,
	// 再把所有物品的格子位置重新紧凑排布(0,1,2,...),消除中间空洞。
	// 三步:① 按 config 把"未满的可叠加堆"分组 ② 每组合并、多余的清空待删
	//       ③ 删空堆 + 重建 posToGuid 布局。

	// 早退(热路径):本函数每次玩家打开背包都会被调一次,绝大多数情况下背包
	// 早已整理好、无需再动。先做一遍只读 O(物品数) 检查,若已是最优布局就直接
	// 返回 —— 既省掉后面的销毁/重建开销,更关键的是"不打乱玩家已经摆好的布局"
	// (后面的重建会清空 posToGuid 按 items 哈希序重排,会让本已整齐的物品换位)。
	if (IsAlreadyMergedAndCompact())
	{
		return false;
	}

	// ① 分组 —— 把同 config_id 的"未满可叠加堆"归到一组。
	// 直接用 config_id 做哈希分组,O(物品数)。CanStack 本质就是比 config_id,
	// 所以这里完全等价,且避免了原来"逐物品线性扫已有组"的 O(物品数 × 组数) 开销。
	std::unordered_map<uint32_t, EntityVector> groupsByConfig; // config_id -> 同种未满堆的实体列表
	for (auto &&[entity, item] : itemRegistry.view<ItemComp>().each()) // 遍历背包每个物品
	{
		LookupItemOrContinue(item.config_id());        // 查物品表(查不到跳过),注入 itemRow
		if (itemRow->max_stack_size() <= 1)
		{
			continue; // 不可叠加:无可合并,跳过
		}
		if (item.size() >= itemRow->max_stack_size())
		{
			continue; // 已经是满堆:无需参与合并,跳过
		}
		groupsByConfig[item.config_id()].emplace_back(entity); // 未满堆,按 config 入组
	}

	// ② 合并每组 —— 把组内总数量重新分配:前几格填满,最后一格放余数,其余清 0。
	GuidVector emptiedItemGuids; // 被清空(size=0)、待删除的物品 guid
	for (auto &[configId, group] : groupsByConfig) // 遍历每个 config 分组
	{
		LookupItemOrContinue(configId);            // 注入 itemRow,拿该 config 的堆叠上限
		const uint32_t maxStack = itemRow->max_stack_size();

		// 先求出该 config 这些未满堆里的总数量。
		uint32_t totalStackSize = 0;
		for (auto &entity : group)
		{
			totalStackSize += itemRegistry.get<ItemComp>(entity).size();
		}

		// 前面的格子逐个填满 maxStack,直到剩余量能塞进一格为止。
		std::size_t index = 0;
		for (; index < group.size(); ++index)
		{
			auto &currentItem = itemRegistry.get<ItemComp>(group[index]);
			if (totalStackSize <= maxStack)
			{
				currentItem.set_size(totalStackSize); // 余量一格放下,合并完成
				++index;                              // 这一格已用,后面的都要清空
				break;
			}
			currentItem.set_size(maxStack);           // 这一格填满
			totalStackSize -= maxStack;               // 扣掉已分配的量
		}

		// 合并后多出来的格子全部清空,记录 guid 等待删除。
		for (; index < group.size(); ++index)
		{
			auto &currentItem = itemRegistry.get<ItemComp>(group[index]);
			currentItem.set_size(0);
			emptiedItemGuids.emplace_back(currentItem.item_id());
		}
	}

	// ③ 删空堆,再把剩余物品紧凑重排到 pos 0,1,2,...
	for (auto &guid : emptiedItemGuids)
	{
		DestroyItem(guid); // 释放实体 + items + posToGuid 槽位
	}

	// 重建位置布局:把剩余物品按 (config 升序, size 降序) 排序后依次铺到
	// pos 0,1,2,... —— 同种物品聚在一起、组内满堆在前、零头落到该组末尾,
	// 得到 [config A: 满..满 零头][config B: 满..满 零头]... 的布局。
	// 先快照成数组再 std::sort,O(物品数 × log 物品数);比对每个物品调
	// AllocateGridSlot(每次 O(容量) 找空位 → O(物品数 × 容量))更省。
	// 剩余物品数必然 <= 容量(整理只会减少占用),所以下标不会越界。
	struct OrderKey
	{
		Guid guid;
		uint32_t configId;
		uint32_t size;
	};
	std::vector<OrderKey> ordered;
	ordered.reserve(items.size());
	for (auto &[guid, entity] : items)
	{
		const auto &item = itemRegistry.get<ItemComp>(entity);
		ordered.emplace_back(OrderKey{guid, item.config_id(), item.size()});
	}
	std::sort(ordered.begin(), ordered.end(), [](const OrderKey &a, const OrderKey &b)
			  {
				  if (a.configId != b.configId)
				  {
					  return a.configId < b.configId; // 同种聚拢
				  }
				  return a.size > b.size;             // 满堆在前、零头在后
			  });

	posToGuid.clear();
	uint32_t nextPos = 0;
	for (const auto &key : ordered)
	{
		posToGuid[nextPos++] = key.guid;
	}
	return true;
}

uint32_t Bag::AddNonStackableItem(ItemComp itemProto, std::vector<Guid> *writtenGuidsOut)
{
	// 不可叠加物品(装备等):每一件都独占一格,size 即"要放几件"。
	// 统一处理:把 itemProto 拆成 pieceCount 件、每件 size=1 单独入格。
	//   * 单件 (pieceCount==1):若调用方预先指定了 guid 就沿用,否则铸一个新的。
	//   * 多件 (pieceCount>1):每件都必须有各自唯一的 guid,所以一律铸新。
	const uint32_t pieceCount = itemProto.size(); // 要放入的件数

	if (IsSpaceInsufficient(pieceCount)) // 空格不够放下这么多件
	{
		// TODO: overflow to temp bag or mail
		return PrintStackAndReturnError(kBagAddItemBagFull);
	}

	// 只有"单件"才允许沿用调用方预设的 guid;多件无法共用一个 guid,必须各自铸新。
	const bool honorPreassignedGuid = (pieceCount == 1);

	for (uint32_t i = 0; i < pieceCount; ++i) // 逐件创建并入格
	{
		ItemComp piece = itemProto; // 复制一份作为这一件
		piece.set_size(1);          // 不可叠加:每件固定 size=1

		// 沿用预设 guid 仅限单件且 guid 合法;其余情况一律铸新 guid。
		if (!honorPreassignedGuid || IsInvalidItemGuid(piece))
		{
			piece.set_item_id(GenerateItemGuid());
		}
		const auto guid = piece.item_id();

		// 经 InsertItemEntity 单一入口写入,保证 items/itemRegistry 同步;
		// 返回 nullptr 表示 guid 撞车,回滚并报错。
		if (InsertItemEntity(std::move(piece)) == nullptr)
		{
			LOG_ERROR << "AddNonStackableItem: duplicate guid " << guid << " player " << PlayerGuid();
			return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
		}

		AllocateGridSlot(guid); // 给这一件分配一个空格子

		// 回执:无论 guid 是新铸的还是沿用调用方预设的,这一件都是本次真实写入的实例。
		if (writtenGuidsOut != nullptr)
		{
			writtenGuidsOut->push_back(guid);
		}
	}
	return kSuccess;
}

uint32_t Bag::PlanStackIntoExistingStacks(const ItemComp &proto, uint32_t maxStackSize,
										  std::vector<StackFill> &outFillPlan) const
{
	uint32_t remaining = proto.size();
	for (auto &&[entity, item] : itemRegistry.view<ItemComp>().each())
	{
		if (remaining == 0)
		{
			break;
		}
		if (!CanStack(item, proto))
		{
			continue;
		}
		if (item.size() > maxStackSize)
		{
			// 脏数据:堆叠数已超上限。跳过,否则 maxStackSize - size 会下溢成巨大值。
			LOG_ERROR << "AddStackableItem: item.size() " << item.size() << " > maxStackSize " << maxStackSize << " player " << PlayerGuid();
			continue;
		}
		const uint32_t room = maxStackSize - item.size();
		if (room == 0)
		{
			continue;
		}
		const uint32_t fill = remaining < room ? remaining : room;
		outFillPlan.emplace_back(StackFill{entity, fill});
		remaining -= fill;
	}
	return remaining;
}

void Bag::ApplyStackFill(const std::vector<StackFill> &fillPlan,
						 std::vector<Guid> *writtenGuidsOut)
{
	for (const auto &plan : fillPlan)
	{
		auto &item = itemRegistry.get<ItemComp>(plan.entity);
		item.set_size(item.size() + plan.amount);

		// 回执:并入既有堆没有铸新号,但这一堆确实是本次数量的去处,
		// 流水按它记录才能追溯到"东西进了哪个实例"。
		if (writtenGuidsOut != nullptr)
		{
			writtenGuidsOut->push_back(item.item_id());
		}
	}
}

uint32_t Bag::SpillIntoNewGrids(ItemComp proto, uint32_t maxStackSize,
								uint32_t remaining, std::size_t newGridCount,
								std::vector<Guid> *writtenGuidsOut)
{
	for (std::size_t i = 0; i < newGridCount; ++i)
	{
		ItemComp piece = proto;
		piece.set_item_id(GenerateItemGuid());
		const uint32_t put = maxStackSize < remaining ? maxStackSize : remaining;
		piece.set_size(put);
		remaining -= put;
		const auto guid = piece.item_id();

		if (InsertItemEntity(std::move(piece)) == nullptr)
		{
			LOG_ERROR << "AddStackableItem: duplicate guid " << guid << " player " << PlayerGuid();
			return PrintStackAndReturnError(kBagDeleteItemAlreadyHasGuid);
		}
		AllocateGridSlot(guid);

		if (writtenGuidsOut != nullptr)
		{
			writtenGuidsOut->push_back(guid);
		}
	}
	return kSuccess;
}

uint32_t Bag::AddStackableItem(ItemComp itemProto, uint32_t maxStackSize,
							   std::vector<Guid> *writtenGuidsOut)
{
	// 事务性:先把"怎么放"全部规划好(PlanStackIntoExistingStacks 不改背包),
	// 确认放得下后再真正写入(ApplyStackFill / SpillIntoNewGrids)。
	// 顺序不能反——一旦先改了堆才发现放不下,就会留下半完成的脏状态。
	std::vector<StackFill> fillPlan;
	const uint32_t remaining = PlanStackIntoExistingStacks(itemProto, maxStackSize, fillPlan);

	std::size_t newGridCount = 0;
	if (remaining > 0)
	{
		newGridCount = GridsNeededFor(remaining, maxStackSize);
		if (IsSpaceInsufficient(newGridCount))
		{
			return PrintStackAndReturnError(kBagAddItemBagFull);
		}
	}

	ApplyStackFill(fillPlan, writtenGuidsOut);

	if (remaining == 0)
	{
		return kSuccess;
	}
	return SpillIntoNewGrids(std::move(itemProto), maxStackSize, remaining, newGridCount,
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
	// 事务语义:与 RemoveItems 完全对称。先用 CheckSpaceFor 整体校验,
	// 任何一种物品放不下就整体失败,绝不做"前几种已进包、后一种失败"的部分添加。
	RETURN_ON_ERROR(CheckSpaceFor(itemsToAdd));

	// 校验通过后逐个按配置发放。每个 config 复用单个 AddItem 的完整逻辑
	// (堆叠/非堆叠分发、铺格子等),主函数只表达"批量发放"的意图。
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

	// 事务前置:先把整批的格子需求汇总成 config->总数,用 CheckSpaceFor 一次性校验。
	// CheckSpaceFor 对装备(maxStack==1)同样正确——每个单位各占一格、无现有堆叠可填。
	// 任何一项放不下就整体失败,绝不做"前几件已进包、后一件失败"的部分添加。
	ItemCountMap requiredSpace;
	for (const auto &param : itemsToAdd)
	{
		requiredSpace[param.itemPBComp.config_id()] += param.itemPBComp.size();
	}
	RETURN_ON_ERROR(CheckSpaceFor(requiredSpace));

	// 校验通过后逐件发放,复用单个 AddItem 的完整逻辑(装备走非堆叠、普通物品走堆叠)。
	for (const auto &param : itemsToAdd)
	{
		RETURN_ON_ERROR(AddItem(param));
	}
	return kSuccess;
}

uint32_t Bag::RemoveItem(Guid del_guid)
{
	if (!items.contains(del_guid))
	{
		return PrintStackAndReturnError(kBagDeleteItemFindGuid);
	}

	// DestroyItem frees the entity, the items entry, and the posToGuid slot.
	DestroyItem(del_guid);
	return kSuccess;
}

void Bag::ExpandCapacity(std::size_t sz)
{
	capacity += sz;
}

Guid Bag::GenerateItemGuid()
{
	return tlsSnowflakeManager.GenerateItemGuid();
}

bool Bag::IsInvalidItemGuid(const ItemComp &item) const
{
	return item.item_id() == kInvalidGuid || item.item_id() <= 0;
}

ItemComp *Bag::InsertItemEntity(ItemComp proto)
{
	auto entity = itemRegistry.create();
	auto &stored = itemRegistry.emplace<ItemComp>(entity, std::move(proto));

	auto [it, inserted] = items.emplace(stored.item_id(), entity);
	if (!inserted)
	{
		// Duplicate guid — roll the entity back so items and itemRegistry
		// stay perfectly in sync (the half-created entity must not leak).
		itemRegistry.destroy(entity);
		return nullptr;
	}
	return &stored;
}

void Bag::DestroyItem(Guid guid)
{
	auto guidIt = items.find(guid);
	if (guidIt == items.end())
	{
		return;
	}
	// Destroy the backing ECS entity too — otherwise the size-0 component
	// lingers in itemRegistry as an orphan that view<ItemComp> still sees,
	// letting a later AddStackableItem stack into an item no longer in
	// items (units would be silently lost).
	if (itemRegistry.valid(guidIt->second))
	{
		itemRegistry.destroy(guidIt->second);
	}
	items.erase(guidIt);

	// Free its grid slot too (posToGuid maps pos->guid, so scan for the guid).
	for (auto posIt = posToGuid.begin(); posIt != posToGuid.end(); ++posIt)
	{
		if (posIt->second == guid)
		{
			posToGuid.erase(posIt);
			break;
		}
	}
}

void Bag::ClearAllItems()
{
	for (const auto &[guid, entity] : items)
	{
		if (itemRegistry.valid(entity))
		{
			itemRegistry.destroy(entity);
		}
	}
	items.clear();
	posToGuid.clear();
}

std::size_t Bag::GridsNeededFor(std::size_t totalSize, std::size_t maxStackSize)
{
	return (totalSize + maxStackSize - 1) / maxStackSize;
}

uint32_t Bag::AllocateGridSlot(Guid guid)
{
	const auto gridSize = Capacity();
	for (uint32_t i = 0; i < gridSize; ++i)
	{
		if (posToGuid.contains(i))
		{
			continue;
		}
		posToGuid.emplace(i, guid);
		return i;
	}
	return kInvalidU32Id;
}

bool Bag::CanStack(const ItemComp &leftItem, const ItemComp &rightItem)
{
	return leftItem.config_id() == rightItem.config_id();
}

// ── Snapshot/migration restore ────────────────────────────────────────
// Wholesale replace bag contents from a snapshot — used by
// bag_marshal::Unmarshal when receiving a migrating player or
// restoring a rollback snapshot. NOT for gameplay use.
//
// Skips AddItem's anomaly detection / transaction_log / gain-block
// checks because the items were already validated when originally
// added in the source zone (or pre-snapshot state). Re-running those
// checks here would double-count anomaly events and emit duplicate
// TX_SYSTEM_GRANT records.
//
// See cpp/libs/services/scene/player/system/bag_marshal.{h,cpp} for
// the proto bridge and docs/design/cross-zone-readiness-audit.md §3.2
// 件 1 for the cross-zone migration design that motivated this.
void Bag::ResetFromSnapshot()
{
	// ClearAllItems drops every entity + items + posToGuid entry.
	// capacity deliberately left alone — SetCapacityForRestore sets it
	// from BagAllData.capacities before InsertItemForRestore calls fire.
	ClearAllItems();
}

void Bag::InsertItemForRestore(Guid guid, uint32_t configId, uint32_t stackSize, uint32_t pos)
{
	if (guid == kInvalidGuid)
	{
		LOG_ERROR << "Bag::InsertItemForRestore: refusing invalid guid (configId=" << configId << ")";
		return;
	}
	if (items.find(guid) != items.end())
	{
		// Duplicate guid in snapshot — log and skip. Indicates upstream
		// data corruption; better to drop one copy than crash.
		LOG_ERROR << "Bag::InsertItemForRestore: duplicate guid=" << guid
				  << " configId=" << configId << " in snapshot, skipping second copy";
		return;
	}

	// Build the ItemComp the same shape AddItem would, then route through
	// the single InsertItemEntity chokepoint so items/itemRegistry stay
	// in sync.
	ItemComp proto;
	proto.set_item_id(guid);
	proto.set_config_id(configId);
	proto.set_size(stackSize);

	if (InsertItemEntity(std::move(proto)) == nullptr)
	{
		LOG_ERROR << "Bag::InsertItemForRestore: duplicate guid=" << guid
				  << " configId=" << configId << ", skipping";
		return;
	}
	// Position is persisted from the source-side bag layout. Don't
	// auto-MergeAndCompact — the player expects items to be where they left them.
	if (pos != kInvalidU32Id)
	{
		posToGuid[pos] = guid;
	}
}

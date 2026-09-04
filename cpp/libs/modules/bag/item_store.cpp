#include "item_store.h"

#include <unordered_map>

#include "engine/core/error_handling/error_handling.h"
#include "table/code/item_table.h"
#include <thread_context/snow_flake_manager.h>

// ── 身份与生命期 ─────────────────────────────────────────────────────────

entt::entity ItemStore::EntityOf(Guid guid) const
{
    auto it = guidToEntity_.find(guid);
    return it != guidToEntity_.end() ? it->second : entt::null;
}

ItemComp *ItemStore::Find(Guid guid)
{
    auto it = guidToEntity_.find(guid);
    return it != guidToEntity_.end() ? registry_.try_get<ItemComp>(it->second) : nullptr;
}

const ItemComp *ItemStore::Find(Guid guid) const
{
    auto it = guidToEntity_.find(guid);
    return it != guidToEntity_.end() ? registry_.try_get<ItemComp>(it->second) : nullptr;
}

ItemComp *ItemStore::Insert(ItemComp proto)
{
    // 入包序号在这里、也只在这里盖章 —— Insert 是本仓库唯一的创建口径。
    //
    //   acquire_seq == 0  新实例 / 旧存档没盖过章 -> 盖当前水位;
    //   acquire_seq  > 0  快照带回来的章 -> 原样保留,并把水位抬到它之上。
    //
    // 于是"店里的每个实例都有非零、且同包内单调"这条不变量由结构保证:
    // 淘汰策略可以直接比大小,不必再考虑 0 与 guid 混排(两个数域混排会让
    // 旧存档的物品永远排在最后,先进先出就挤错人)。
    if (proto.acquire_seq() == 0)
    {
        proto.set_acquire_seq(nextAcquireSeq_++);
    }
    else if (proto.acquire_seq() >= nextAcquireSeq_)
    {
        nextAcquireSeq_ = proto.acquire_seq() + 1;
    }

    auto entity = registry_.create();
    auto &stored = registry_.emplace<ItemComp>(entity, std::move(proto));

    auto [it, inserted] = guidToEntity_.emplace(stored.item_id(), entity);
    if (!inserted)
    {
        // guid 撞车 —— 把半建好的实体回滚掉,让 guidToEntity_ 与 registry_
        // 严丝合缝地同步(孤儿实体绝不能泄漏出去)。
        registry_.destroy(entity);
        return nullptr;
    }
    return &stored;
}

bool ItemStore::Erase(Guid guid)
{
    auto it = guidToEntity_.find(guid);
    if (it == guidToEntity_.end())
    {
        return false;
    }
    // 实体也要销毁 —— 否则那个 size=0 的组件会作为孤儿留在 registry_ 里,
    // view<ItemComp> 仍然看得见它,后续并堆会往一个已经不在索引里的实例上
    // 灌数量,单位就静默丢了。
    if (registry_.valid(it->second))
    {
        registry_.destroy(it->second);
    }
    guidToEntity_.erase(it);
    return true;
}

void ItemStore::Clear()
{
    for (const auto &[guid, entity] : guidToEntity_)
    {
        if (registry_.valid(entity))
        {
            registry_.destroy(entity);
        }
    }
    guidToEntity_.clear();
    // 水位跟着清空一起复位:ResetFromSnapshot 之后紧跟着的就是按快照顺序重放,
    // 那批实例会带着自己的章回来(或没章、由这里重新按顺序盖)。不复位的话,
    // 一个反复还原的实体水位会单调涨到没有意义的大数。
    nextAcquireSeq_ = 1;
}

std::size_t ItemStore::TotalCountOf(uint32_t configId) const
{
    std::size_t totalSize = 0;
    for (const auto &[entity, item] : registry_.view<ItemComp>().each())
    {
        if (item.config_id() == configId)
        {
            totalSize += item.size();
        }
    }
    return totalSize;
}

bool ItemStore::HasAll(const ItemCountMap &required) const
{
    // 只读检查:是否拥有 required 要求的每一项 (config_id -> 需求数量)。
    // 思路:把需求拷一份,遍历实例逐个堆叠去抵扣,需求全部清零即满足。
    auto itemsToCheck = required;  // 拷一份,边遍历边扣减(不污染入参)

    for (const auto &[entity, item] : registry_.view<ItemComp>().each())
    {
        auto it = itemsToCheck.find(item.config_id());  // 这个 config 在需求清单里吗
        if (it == itemsToCheck.end())
        {
            continue;  // 不是需要的物品,跳过
        }
        if (item.size() >= it->second)
        {
            itemsToCheck.erase(it);  // 这一堆就够抵该 config 的全部剩余需求
            continue;
        }
        it->second -= item.size();  // 只够抵一部分 -> 扣减后继续找后面的同 config 堆叠
    }

    return itemsToCheck.empty();
}

// ── 堆叠语义 ─────────────────────────────────────────────────────────────

ItemCountMap ItemStore::MeasureFreeRoomPerConfig(const ItemCountMap &wanted,
                                                 const std::unordered_set<Guid> *exclude) const
{
    ItemCountMap freeRoomByConfig;
    for (const auto &[entity, item] : registry_.view<ItemComp>().each())
    {
        if (!wanted.contains(item.config_id()))
        {
            continue;
        }
        // 被排除的实例视同不存在 —— 它的空余不能算给这一批。
        if (exclude != nullptr && exclude->contains(static_cast<Guid>(item.item_id())))
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

uint32_t ItemStore::PlanStackIntoExistingStacks(const ItemComp &proto, uint32_t maxStackSize,
                                                std::vector<StackFill> &outFillPlan,
                                                const std::unordered_set<Guid> *exclude) const
{
    uint32_t remaining = proto.size();
    for (auto &&[entity, item] : registry_.view<ItemComp>().each())
    {
        if (remaining == 0)
        {
            break;
        }
        if (!CanStack(item, proto))
        {
            continue;
        }
        if (exclude != nullptr && exclude->contains(static_cast<Guid>(item.item_id())))
        {
            continue; // 视同不存在,见头文件
        }
        if (item.size() > maxStackSize)
        {
            // 脏数据:堆叠数已超上限。跳过,否则 maxStackSize - size 会下溢成巨大值。
            LOG_ERROR << "ItemStore::PlanStackIntoExistingStacks: item.size() " << item.size()
                      << " > maxStackSize " << maxStackSize << " config " << item.config_id();
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

void ItemStore::ApplyStackFill(const std::vector<StackFill> &fillPlan,
                               std::vector<Guid> *writtenGuidsOut)
{
    for (const auto &plan : fillPlan)
    {
        // 防御:计划里的实体在 plan 与 apply 之间被销毁(唯一的销毁点是淘汰腾位,
        // 桥层腾位后必定重新规划,所以正常到不了这里)。到了就是编程错误 ——
        // 但 entt 对失效实体 get 在 Release 下是 UB,宁可丢这一份数量并吼,
        // 也不能拿悬空实体去解引用。
        if (!registry_.valid(plan.entity))
        {
            LOG_ERROR << "ItemStore::ApplyStackFill: fill plan references a destroyed entity; "
                      << plan.amount << " unit(s) NOT applied. The plan must be recomputed after "
                      << "any eviction — this is a bridge-layer ordering bug.";
            continue;
        }
        auto &item = registry_.get<ItemComp>(plan.entity);
        item.set_size(item.size() + plan.amount);

        // 回执:并入既有堆没有铸新号,但这一堆确实是本次数量的去处,
        // 流水按它记录才能追溯到"东西进了哪个实例"。
        if (writtenGuidsOut != nullptr)
        {
            writtenGuidsOut->push_back(item.item_id());
        }
    }
}

void ItemStore::DrainStacks(uint32_t configId, uint32_t count)
{
    for (const auto &[entity, item] : registry_.view<ItemComp>().each())  // 遍历所有堆叠
    {
        if (count == 0)
        {
            break;  // 这种物品已经抽够了
        }
        if (item.config_id() != configId)
        {
            continue;  // 不是目标物品,跳过
        }
        const uint32_t take = count < item.size() ? count : item.size();  // 需求与库存取小
        item.set_size(item.size() - take);                                // 抽走(抽光则 size=0)
        count -= take;                                                    // 更新还需抽取的数量
    }
}

bool ItemStore::HasMergeablePartials() const
{
    // 同一个 config 一旦出现 >= 2 个未满堆(一高一低 / 两个半堆),就能合并出
    // 更少的实例。这一条完全独立于槽位紧不紧凑 —— 它是纯粹的实例层判据。
    std::unordered_map<uint32_t, uint32_t> partialCountByConfig;
    for (auto &&[entity, item] : registry_.view<ItemComp>().each())
    {
        LookupItemOrContinue(item.config_id());
        if (itemRow->max_stack_size() <= 1)
        {
            continue;  // 不可叠加:不参与合并
        }
        if (item.size() >= itemRow->max_stack_size())
        {
            continue;  // 满堆:不参与合并
        }
        if (++partialCountByConfig[item.config_id()] >= 2)
        {
            return true;
        }
    }
    return false;
}

void ItemStore::MergePartialStacks()
{
    // ① 分组 —— 把同 config_id 的"未满可叠加堆"归到一组。
    // 直接用 config_id 做哈希分组,O(实例数)。CanStack 本质就是比 config_id,
    // 所以这里完全等价,且避免了"逐物品线性扫已有组"的 O(实例数 × 组数) 开销。
    std::unordered_map<uint32_t, EntityVector> groupsByConfig;
    for (auto &&[entity, item] : registry_.view<ItemComp>().each())
    {
        LookupItemOrContinue(item.config_id());  // 查物品表(查不到跳过),注入 itemRow
        if (itemRow->max_stack_size() <= 1)
        {
            continue;  // 不可叠加:无可合并,跳过
        }
        if (item.size() >= itemRow->max_stack_size())
        {
            continue;  // 已经是满堆:无需参与合并,跳过
        }
        groupsByConfig[item.config_id()].emplace_back(entity);  // 未满堆,按 config 入组
    }

    // ② 合并每组 —— 把组内总数量重新分配:前几个填满,最后一个放余数,其余清 0。
    // 清成 0 的那些不在这里销毁也不单独记名:它们已经是 size==0,由调用方紧接着的
    // CollectEmptyInstances 一并扫出来交给桥层两层成对销毁。
    for (auto &[configId, group] : groupsByConfig)
    {
        LookupItemOrContinue(configId);  // 注入 itemRow,拿该 config 的堆叠上限
        const uint32_t maxStack = itemRow->max_stack_size();

        // 先求出该 config 这些未满堆里的总数量。
        uint32_t totalStackSize = 0;
        for (auto &entity : group)
        {
            totalStackSize += registry_.get<ItemComp>(entity).size();
        }

        // 前面的实例逐个填满 maxStack,直到剩余量能塞进一个为止。
        std::size_t index = 0;
        for (; index < group.size(); ++index)
        {
            auto &currentItem = registry_.get<ItemComp>(group[index]);
            if (totalStackSize <= maxStack)
            {
                currentItem.set_size(totalStackSize);  // 余量一堆放下,合并完成
                ++index;                               // 这一个已用,后面的都要清空
                break;
            }
            currentItem.set_size(maxStack);  // 这一个填满
            totalStackSize -= maxStack;      // 扣掉已分配的量
        }

        // 合并后多出来的实例全部清空,交给 CollectEmptyInstances 统一回收。
        for (; index < group.size(); ++index)
        {
            registry_.get<ItemComp>(group[index]).set_size(0);
        }
    }
}

bool ItemStore::HasEmptyInstances() const
{
    for (const auto &[entity, item] : registry_.view<ItemComp>().each())
    {
        if (item.size() == 0)
        {
            return true;
        }
    }
    return false;
}

GuidVector ItemStore::CollectEmptyInstances() const
{
    GuidVector empties;
    for (const auto &[entity, item] : registry_.view<ItemComp>().each())
    {
        if (item.size() == 0)
        {
            empties.emplace_back(static_cast<Guid>(item.item_id()));
        }
    }
    return empties;
}

bool ItemStore::CanStack(const ItemComp &leftItem, const ItemComp &rightItem)
{
    return leftItem.config_id() == rightItem.config_id();
}

std::size_t ItemStore::StacksNeededFor(std::size_t totalSize, std::size_t maxStackSize)
{
    return (totalSize + maxStackSize - 1) / maxStackSize;
}

// ── guid 发号(实例身份)───────────────────────────────────────────────

Guid ItemStore::MintGuid()
{
    return tlsSnowflakeManager.GenerateItemGuid();
}

bool ItemStore::CanMintGuid()
{
    // 发号器被 fence(失去 node_id 所有权)或本线程从未 OnNodeStart 时,
    // GenerateItemGuid() 只会返回 kInvalidGuid。铸号型写入必须在**改动任何状态
    // 之前**用它把整个操作拒掉:哨兵值一旦被 set 进 item_id 并 Insert,就是一件
    // guid 为哨兵的物品被持久化进玩家 blob(数据完整性破坏),且第二件同样的
    // 插入会撞 DuplicateGuid,报错方向完全误导排障。
    // tls 单线程,本调用与后续铸号之间状态不会被并发翻转,先查后铸没有 TOCTOU。
    return tlsSnowflakeManager.IsInitialized() && !tlsSnowflakeManager.IsFenced();
}

bool ItemStore::IsInvalidGuid(const ItemComp &item)
{
    return item.item_id() == kInvalidGuid || item.item_id() <= 0;
}

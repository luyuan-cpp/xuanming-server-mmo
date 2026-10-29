#include "item_store.h"

#include <algorithm>
#include <cassert>
#include <map>
#include <unordered_map>

#include "engine/core/error_handling/error_handling.h"
#include "engine/core/utils/proto/unknown_field_carry.h"
#include "table/code/item_table.h"

#include <chrono>

#include "modules/id_segment/guid_segment_registry.h"

// ── 身份与生命期 ─────────────────────────────────────────────────────────

ItemComp *ItemStore::Find(Guid guid)
{
    auto it = items_.find(guid);
    if (it == items_.end())
    {
        return nullptr;
    }
    assert(it->first == it->second.item_id());
    return &it->second;
}

const ItemComp *ItemStore::Find(Guid guid) const
{
    auto it = items_.find(guid);
    if (it == items_.end())
    {
        return nullptr;
    }
    assert(it->first == it->second.item_id());
    return &it->second;
}

ItemComp *ItemStore::Insert(ItemComp proto)
{
    const Guid guid = proto.item_id();
    if (items_.contains(guid))
    {
        return nullptr; // 重复请求不占用入包序号,也不抬高水位。
    }
    // 入包序号在这里、也只在这里盖章 —— Insert 是本仓库唯一的创建口径。
    //
    //   acquire_seq == 0  新实例 / 旧存档没盖过章 -> 盖当前水位;
    //   acquire_seq  > 0  快照带回来的章 -> 原样保留,并把水位抬到它之上。
    //
    // 加载边界先调用 RaiseAcquireSeq 扫全包水位，避免零章条目与后到的既有序号冲突。
    if (proto.acquire_seq() == 0)
    {
        proto.set_acquire_seq(nextAcquireSeq_++);
    }
    else if (proto.acquire_seq() >= nextAcquireSeq_)
    {
        nextAcquireSeq_ = proto.acquire_seq() + 1;
    }

    auto [it, inserted] = items_.emplace(guid, std::move(proto));
    assert(inserted); // 单线程所有权,上面的查重与插入之间没有回调。
    return &it->second;
}

void ItemStore::RaiseAcquireSeq(uint64_t sequence)
{
    if (sequence >= nextAcquireSeq_)
    {
        nextAcquireSeq_ = sequence + 1;
    }
}

bool ItemStore::Erase(Guid guid)
{
    return items_.erase(guid) != 0;
}

void ItemStore::Clear()
{
    items_.clear();
    // 水位跟着清空一起复位:ResetFromSnapshot 之后紧跟着的就是按快照顺序重放,
    // 那批实例会带着自己的章回来(或没章、由这里重新按顺序盖)。不复位的话,
    // 一个反复还原的实体水位会单调涨到没有意义的大数。
    nextAcquireSeq_ = 1;
}

bool ItemStore::AcquiredBefore(const ItemComp &lhs, const ItemComp &rhs)
{
    return lhs.acquire_seq() != rhs.acquire_seq() ? lhs.acquire_seq() < rhs.acquire_seq()
                                                   : lhs.item_id() < rhs.item_id();
}

std::vector<const ItemStore::Items::value_type *> ItemStore::InAcquireOrder() const
{
    std::vector<const Items::value_type *> ordered;
    ordered.reserve(items_.size());
    for (const auto &entry : items_)
    {
        assert(entry.first == entry.second.item_id());
        ordered.push_back(&entry);
    }
    std::sort(ordered.begin(), ordered.end(), [](const auto *lhs, const auto *rhs)
              { return AcquiredBefore(lhs->second, rhs->second); });
    return ordered;
}

std::vector<ItemStore::Items::value_type *> ItemStore::InAcquireOrder()
{
    std::vector<Items::value_type *> ordered;
    ordered.reserve(items_.size());
    for (auto &entry : items_)
    {
        assert(entry.first == entry.second.item_id());
        ordered.push_back(&entry);
    }
    std::sort(ordered.begin(), ordered.end(), [](const auto *lhs, const auto *rhs)
              { return AcquiredBefore(lhs->second, rhs->second); });
    return ordered;
}

bool ItemStore::IsCountableByConfig(const ItemComp &item)
{
    return !unknown_field_carry::CarriesNewerVersionFields(item);
}

std::size_t ItemStore::TotalCountOf(uint32_t configId) const
{
    std::size_t totalSize = 0;
    // 总量与遍历顺序无关。
    for (const auto &[guid, item] : items_)
    {
        if (item.config_id() == configId && IsCountableByConfig(item))
        {
            totalSize += item.size();
        }
    }
    return totalSize;
}

ItemStore::QuantityVerdict ItemStore::CheckQuantities(const ItemCountMap &required) const
{
    // 一次遍历,按需求里的 config 分别累计可计单位与"扣下"的单位(带更新版本字段的)。与遍历顺序无关。
    struct Held
    {
        uint64_t countable{0};
        uint64_t withheld{0};
    };
    std::unordered_map<uint32_t, Held> heldByConfig;
    for (const auto &[guid, item] : items_)
    {
        if (!required.contains(item.config_id()))
        {
            continue;
        }
        auto &held = heldByConfig[item.config_id()];
        (IsCountableByConfig(item) ? held.countable : held.withheld) += item.size();
    }

    bool versionSkew = false;
    for (const auto &[configId, need] : required)
    {
        if (need == 0)
        {
            continue;
        }
        const auto it = heldByConfig.find(configId);
        const Held held = it == heldByConfig.end() ? Held{} : it->second;
        if (held.countable + held.withheld < need)
        {
            return QuantityVerdict::kInsufficient; // 任何版本都扣不出来:确定不足,优先于版本错位
        }
        if (held.countable < need)
        {
            versionSkew = true;
        }
    }
    return versionSkew ? QuantityVerdict::kVersionSkew : QuantityVerdict::kEnough;
}

bool ItemStore::HasAll(const ItemCountMap &required) const
{
    return CheckQuantities(required) == QuantityVerdict::kEnough;
}

// ── 堆叠语义 ─────────────────────────────────────────────────────────────

ItemCountMap ItemStore::MeasureFreeRoomPerConfig(const ItemCountMap &wanted,
                                                 const std::unordered_set<Guid> *exclude) const
{
    ItemCountMap freeRoomByConfig;
    // 空余总量与遍历顺序无关；Bag 已在扣空时释放实例，加载也跳过旧档零堆。
    for (const auto &[guid, item] : items_)
    {
        if (!wanted.contains(item.config_id()))
        {
            continue;
        }
        // 被排除的实例视同不存在 —— 它的空余不能算给这一批。
        if (exclude != nullptr && exclude->contains(guid))
        {
            continue;
        }
        // 独立实例的堆不收任何并入(见 CanStack)。这里是 reserve 侧的估算,不跟着跳过的话
        // 它会把这种堆的空余算进去,而 commit 侧的 PlanStackIntoExistingStacks 并不往里填 ——
        // reserve 比 commit 乐观,就是"预检通过、写到一半放不下"。
        if (IsIndependentInstance(item))
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
    for (const auto *entry : InAcquireOrder())
    {
        const auto &[guid, item] = *entry;
        if (remaining == 0)
        {
            break;
        }
        if (!CanStack(item, proto))
        {
            continue;
        }
        if (exclude != nullptr && exclude->contains(guid))
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
        outFillPlan.emplace_back(StackFill{guid, fill});
        remaining -= fill;
    }
    return remaining;
}

void ItemStore::ApplyStackFill(const std::vector<StackFill> &fillPlan,
                               std::vector<Guid> *writtenGuidsOut)
{
    for (const auto &plan : fillPlan)
    {
        // 淘汰后桥层必须重新规划。找不到 guid 是顺序错误,不得凭空补回已销毁实例。
        auto *item = Find(plan.guid);
        if (item == nullptr)
        {
            LOG_ERROR << "ItemStore::ApplyStackFill: 计划引用已销毁的 guid=" << plan.guid
                      << ", 未写入数量=" << plan.amount << "; 淘汰后必须重新规划";
            continue;
        }
        item->set_size(item->size() + plan.amount);

        // 回执:并入既有堆没有铸新号,但这一堆确实是本次数量的去处,
        // 流水按它记录才能追溯到"东西进了哪个实例"。
        if (writtenGuidsOut != nullptr)
        {
            writtenGuidsOut->push_back(plan.guid);
        }
    }
}

uint32_t ItemStore::DrainStacks(uint32_t configId, uint32_t count,
                                const DrainRank &drainRank,
                                std::vector<StackDrain> *drainedOut)
{
    // 每个候选只查一次 rank,比较器不调用外部逻辑。空 rank 时仅按实例序。
    std::vector<std::pair<uint64_t, Items::value_type *>> ordered;
    for (auto &entry : items_)
    {
        assert(entry.first == entry.second.item_id());
        // 带更新版本字段的堆不当普通单位扣(IsCountableByConfig,与 TotalCountOf / CheckQuantities 同口径)。
        if (entry.second.config_id() == configId && entry.second.size() > 0 && IsCountableByConfig(entry.second))
        {
            ordered.emplace_back(drainRank ? drainRank(entry.first) : uint64_t{0}, &entry);
        }
    }
    std::sort(ordered.begin(), ordered.end(), [](const auto &lhs, const auto &rhs)
              { return lhs.first != rhs.first ? lhs.first < rhs.first
                                               : AcquiredBefore(lhs.second->second, rhs.second->second); });
    const uint32_t requested = count;
    for (const auto &[rank, entry] : ordered)
    {
        if (count == 0)
        {
            break;  // 这种物品已经抽够了
        }
        auto &item = entry->second;
        const uint32_t take = count < item.size() ? count : item.size();  // 需求与库存取小
        if (take == 0)
        {
            continue;  // size==0 的僵尸堆:抽不出东西,也不该进回执
        }
        item.set_size(item.size() - take);                                // 抽走(抽光则 size=0)
        count -= take;                                                    // 更新还需抽取的数量
        if (drainedOut != nullptr)
        {
            drainedOut->push_back(StackDrain{entry->first, take});
        }
    }
    return requested - count;  // 实扣量:库存不足时 < requested
}

bool ItemStore::HasMergeablePartials() const
{
    // 同一个 config 一旦出现 >= 2 个未满堆(一高一低 / 两个半堆),就能合并出
    // 更少的实例。这一条完全独立于槽位紧不紧凑 —— 它是纯粹的实例层判据。
    std::unordered_map<uint32_t, uint32_t> partialCountByConfig;
    for (const auto &[guid, item] : items_)
    {
        LookupItemOrContinue(item.config_id());
        if (!IsMergeCandidate(item, itemRow->max_stack_size()))
        {
            continue;
        }
        if (++partialCountByConfig[item.config_id()] >= 2)
        {
            return true;
        }
    }
    return false;
}

bool ItemStore::IsMergeCandidate(const ItemComp &item, uint32_t maxStack)
{
    return !IsIndependentInstance(item) && maxStack > 1 && item.size() > 0 && item.size() < maxStack;
}

bool ItemStore::IsIndependentInstance(const ItemComp &item)
{
    return item.has_equip() || unknown_field_carry::CarriesNewerVersionFields(item);
}

void ItemStore::MergePartialStacks()
{
    // 按实例序入组,最早的非空堆先填满并留下。空堆仅交给回收,装备不参与并堆。
    std::map<uint32_t, std::vector<ItemComp *>> groupsByConfig;
    for (auto *entry : InAcquireOrder())
    {
        auto &item = entry->second;
        LookupItemOrContinue(item.config_id());  // 查物品表(查不到跳过),注入 itemRow
        if (!IsMergeCandidate(item, itemRow->max_stack_size()))
        {
            continue;
        }
        groupsByConfig[item.config_id()].push_back(&item);
    }

    // ② 合并每组 —— 把组内总数量重新分配:前几个填满,最后一个放余数,其余清 0。
    // 清成 0 的那些不在这里销毁也不单独记名:它们已经是 size==0,由调用方紧接着的
    // CollectEmptyInstances 一并扫出来交给桥层两层成对销毁。
    for (auto &[configId, group] : groupsByConfig)
    {
        LookupItemOrContinue(configId);  // 注入 itemRow,拿该 config 的堆叠上限
        const uint32_t maxStack = itemRow->max_stack_size();

        // 先求出该 config 这些未满堆里的总数量。
        uint64_t totalStackSize = 0;
        for (const auto *item : group)
        {
            totalStackSize += item->size();
        }

        // 前面的实例逐个填满 maxStack,直到剩余量能塞进一个为止。
        std::size_t index = 0;
        for (; index < group.size(); ++index)
        {
            auto &currentItem = *group[index];
            if (totalStackSize <= maxStack)
            {
                currentItem.set_size(static_cast<uint32_t>(totalStackSize)); // 已确认余量不超过单堆上限。
                ++index;                               // 这一个已用,后面的都要清空
                break;
            }
            currentItem.set_size(maxStack);  // 这一个填满
            totalStackSize -= maxStack;      // 扣掉已分配的量
        }

        // 合并后多出来的实例全部清空,交给 CollectEmptyInstances 统一回收。
        for (; index < group.size(); ++index)
        {
            group[index]->set_size(0);
        }
    }
}

bool ItemStore::HasEmptyInstances() const
{
    // 是否存在空堆与遍历顺序无关。
    for (const auto &[guid, item] : items_)
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
    for (const auto *entry : InAcquireOrder())
    {
        if (entry->second.size() == 0)
        {
            empties.push_back(entry->first);
        }
    }
    return empties;
}

bool ItemStore::CanStack(const ItemComp &leftItem, const ItemComp &rightItem)
{
    // 任一方带实例数据(equip 段)就不许并堆:并堆只加数量,两份不同的属性并成一堆之后
    // 只剩下既有那一份。正常情况下到不了这里 —— 装备必须 max_stack_size == 1,根本不走
    // 堆叠路径(docs/design/equipment-attributes.md §5 不变量 7,由玩法层启动时逐行校验);
    // 这是表被误配成可堆叠时的兜底。判的是 presence 而不是"有几条属性":
    // has_equip() 且 0 条属性同样是一件已经初始化过的独立实例。
    // 带着本版本不认识字段的实例同样不并(头文件 CanStack 注释,混版本设计 §4.4)。
    if (IsIndependentInstance(leftItem) || IsIndependentInstance(rightItem))
    {
        return false;
    }
    return leftItem.config_id() == rightItem.config_id();
}

std::size_t ItemStore::StacksNeededFor(std::size_t totalSize, std::size_t maxStackSize)
{
    return (totalSize + maxStackSize - 1) / maxStackSize;
}

// ── guid 发号(实例身份)───────────────────────────────────────────────
//
// 唯一的发号源是 item 种类的号段客户端(docs/design/node-id-overhaul-plan-20260908.md §6 / §7.5):
// scene 经 DataService.AllocateIdSegment 从全局库 id_segment 表领 [lo, hi),这里只从手里的段发号。
// **没有回退**(§7.5 第 1 条:scene 已退出 snowflake 槽位协议,没有槽就没有可回退的发号器):
// 两段都空且续段未到 → kInvalidGuid,调用方 fail-closed(背包整批拒绝、零写入)。
// tx_id / snapshot_id 各自有自己的实例(GuidKind::kTxLog / kSnapshot),互不影响。

namespace
{
    GuidSegmentClient &ItemSegment()
    {
        return tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    }

    // 发号源掉线时 MintGuid 会被批量入包连续调用,ERROR 按 1/s 节流,
    // 否则一次 400 件的发放就是 400 行同样的日志。tls 单线程,不需要原子量。
    bool ShouldLogMintFailure()
    {
        using Clock = std::chrono::steady_clock;
        thread_local Clock::time_point lastLogged{};
        const auto now = Clock::now();
        if (lastLogged != Clock::time_point{} && now - lastLogged < std::chrono::seconds(1))
        {
            return false;
        }
        lastLogged = now;
        return true;
    }
} // namespace

Guid ItemStore::MintGuid()
{
    Guid guid = kInvalidGuid;
    if (ItemSegment().TryNext(guid))
    {
        return guid;
    }
    if (ShouldLogMintFailure())
    {
        LOG_ERROR << "[idsegment] no id source for item guid: segment not enabled, exhausted or first range not "
                  << "fetched yet, refill not ready; returning kInvalidGuid (fail-closed, no fallback by design) "
                  << ItemSegment().Describe();
    }
    return kInvalidGuid;
}

bool ItemStore::IsInvalidGuid(const ItemComp &item)
{
    return item.item_id() == kInvalidGuid || item.item_id() <= 0;
}

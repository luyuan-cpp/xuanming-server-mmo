#include "container_layout.h"

#include <optional>
#include "engine/core/error_handling/error_handling.h"

// ═════════════════════════════════════════════════════════════════════════
// FlatLayout
// ═════════════════════════════════════════════════════════════════════════

std::size_t FlatLayout::FreeCells() const
{
    // 钳到 0 而不是直接相减:PlaceAt 不做越界校验(见下),理论上可能出现
    // 槽位数超过容量的脏状态,无符号相减会下溢成天文数字并让"背包永远不满"。
    return capacity > slotToGuid.size() ? capacity - slotToGuid.size() : 0;
}

bool FlatLayout::CanFit(std::size_t count, Footprint /*footprint*/) const
{
    // 扁平布局忽略形状:一个实例恒占一格。
    return FreeCells() >= count;
}

SlotId FlatLayout::Place(Guid guid, Footprint /*footprint*/)
{
    // 幂等前置:同一个 guid 不可能同时占两格。拆分前 AllocateGridSlot 没有这道
    // 前置(它只往 posToGuid 里 emplace),因为调用方一定先 InsertItemEntity
    // 成功、guid 必然是新的。现在多了反向索引,重复放置会让正反索引对不上,
    // 所以先摘干净再放。可达路径上这是个空操作。
    Remove(guid);

    // first-fit 扫 0..capacity-1,与拆分前 Bag::AllocateGridSlot 逐字同构
    // (含"满了返回 kInvalidU32Id 且不写入任何状态")。
    for (SlotId slot = 0; slot < static_cast<SlotId>(capacity); ++slot)
    {
        if (slotToGuid.contains(slot))
        {
            continue;
        }
        slotToGuid.emplace(slot, guid);
        guidToSlot.emplace(guid, slot);
        return slot;
    }
    return kInvalidSlot;
}

bool FlatLayout::PlaceAt(Guid guid, SlotId slot, Footprint /*footprint*/)
{
    if (slot == kInvalidSlot)
    {
        return false;
    }

    // fail-closed:越界的槽位一律拒绝。
    //
    // 拆分前这里是一句无校验的 `posToGuid[pos] = guid`,一份 pos=999 / capacity=50
    // 的脏快照会让物品落在一个**永远扫不到**的幽灵槽位上:Place 只扫 0..capacity-1
    // 看不见它,FreeCells 也数不到它,于是那件东西既占不到格子也删不掉。
    // 现在把"这个位置合不合法"还给布局层自己判断,由桥层决定放不下时怎么办
    // (Bag::InsertItemForRestore 会改为自动选位)。
    if (slot >= static_cast<SlotId>(capacity))
    {
        return false;
    }

    // 同样 fail-closed:槽位已被**别人**占着就拒绝,绝不静默顶替。
    // 拆分前是后写者覆盖先写者,被顶掉的那件物品仍留在 items 里却没有位置 ——
    // 一个查不到、删不掉、还占着容量的孤儿。
    auto existing = slotToGuid.find(slot);
    if (existing != slotToGuid.end() && existing->second != guid)
    {
        return false;
    }

    // 同一个 guid 若已在别的槽位,先释放旧槽位,否则旧槽位会被永久占着。
    auto previous = guidToSlot.find(guid);
    if (previous != guidToSlot.end() && previous->second != slot)
    {
        slotToGuid.erase(previous->second);
    }

    slotToGuid[slot] = guid;
    guidToSlot[guid] = slot;
    return true;
}

void FlatLayout::Remove(Guid guid)
{
    auto it = guidToSlot.find(guid);
    if (it == guidToSlot.end())
    {
        return;
    }
    // O(1)。拆分前这里是遍历 posToGuid 比对 value 的线性扫描,因为当时根本
    // 没有反向索引 —— 布局没被当成一层东西,自然也不会有自己的索引。
    slotToGuid.erase(it->second);
    guidToSlot.erase(it);
}

void FlatLayout::Clear()
{
    slotToGuid.clear();
    guidToSlot.clear();
}

SlotId FlatLayout::SlotOf(Guid guid) const
{
    auto it = guidToSlot.find(guid);
    return it != guidToSlot.end() ? it->second : kInvalidSlot;
}

Guid FlatLayout::At(SlotId slot) const
{
    auto it = slotToGuid.find(slot);
    return it != slotToGuid.end() ? it->second : kInvalidGuid;
}

bool FlatLayout::IsCompact() const
{
    // 键唯一且个数 == size,所以"每个键都 < size"等价于键集合恰为 {0..size-1}。
    // 与拆分前 Bag::IsAlreadyMergedAndCompact 的第 ② 条判据逐字同构。
    for (const auto &[slot, guid] : slotToGuid)
    {
        if (slot >= slotToGuid.size())
        {
            return false;
        }
    }
    return true;
}

void FlatLayout::ApplyOrder(const std::vector<Guid> &order)
{
    // 按给定顺序铺到 0,1,2,...。顺序是桥层算好的,这里不做任何排序判断。
    slotToGuid.clear();
    guidToSlot.clear();
    SlotId next = 0;
    for (const Guid guid : order)
    {
        slotToGuid[next] = guid;
        guidToSlot[guid] = next;
        ++next;
    }
}

// ═════════════════════════════════════════════════════════════════════════
// FixedSlotLayout
// ═════════════════════════════════════════════════════════════════════════

void FixedSlotLayout::ApplyOrder(const std::vector<Guid> &order)
{
    // SupportsCompaction() 返回 false,桥层不该走到这里。真走到了是编程错误:
    // 重排会把头盔挪到鞋子的槽位上。
    LOG_ERROR << "FixedSlotLayout::ApplyOrder called on a named-slot layout that does not "
              << "support compaction (order size=" << order.size() << "); ignored";
}

// ═════════════════════════════════════════════════════════════════════════
// GridLayout
// ═════════════════════════════════════════════════════════════════════════

GridLayout::GridLayout(uint32_t width, uint32_t height)
    : width(width), height(height)
{
    cells.assign(Capacity(), kInvalidGuid);
}

bool GridLayout::RectFree(const std::vector<Guid> &cells, uint32_t x, uint32_t y,
                          Footprint footprint) const
{
    if (footprint.width == 0 || footprint.height == 0)
    {
        return false;
    }
    // 先判越界再判占用:宽高相加用 uint64 免得溢出绕回来变成"没越界"。
    if (static_cast<uint64_t>(x) + footprint.width > width ||
        static_cast<uint64_t>(y) + footprint.height > height)
    {
        return false;
    }
    for (uint32_t dy = 0; dy < footprint.height; ++dy)
    {
        for (uint32_t dx = 0; dx < footprint.width; ++dx)
        {
            if (cells[static_cast<std::size_t>(y + dy) * width + (x + dx)] != kInvalidGuid)
            {
                return false;
            }
        }
    }
    return true;
}

SlotId GridLayout::FindFreeRect(const std::vector<Guid> &cells, Footprint footprint) const
{
    for (uint32_t y = 0; y < height; ++y)
    {
        for (uint32_t x = 0; x < width; ++x)
        {
            if (RectFree(cells, x, y, footprint))
            {
                return y * width + x;
            }
        }
    }
    return kInvalidSlot;
}

void GridLayout::Stamp(std::vector<Guid> &cells, SlotId anchor, Footprint footprint,
                       Guid guid) const
{
    const uint32_t x0 = anchor % width;
    const uint32_t y0 = anchor / width;
    for (uint32_t dy = 0; dy < footprint.height; ++dy)
    {
        for (uint32_t dx = 0; dx < footprint.width; ++dx)
        {
            cells[static_cast<std::size_t>(y0 + dy) * width + (x0 + dx)] = guid;
        }
    }
}

void GridLayout::Resize(std::size_t newCapacity)
{
    if (width == 0)
    {
        LOG_ERROR << "GridLayout::Resize on a zero-width grid, ignored";
        return;
    }

    // 按整行增减:容量向上取整到整行。
    const std::size_t newHeight = (newCapacity + width - 1) / width;

    if (newHeight < height)
    {
        // 缩容:被砍掉的行必须完全空着。布局层无权决定"哪件东西该被挤掉",
        // 那是桥层 / 玩法的决策,这里只做 fail-closed。
        for (std::size_t idx = newHeight * width; idx < cells.size(); ++idx)
        {
            if (cells[idx] != kInvalidGuid)
            {
                LOG_ERROR << "GridLayout::Resize refused: shrinking to " << newCapacity
                          << " would evict an occupied cell at index " << idx;
                return;
            }
        }
    }

    height = static_cast<uint32_t>(newHeight);
    cells.resize(Capacity(), kInvalidGuid);
}

std::size_t GridLayout::FreeCells() const
{
    std::size_t free = 0;
    for (const Guid occupant : cells)
    {
        if (occupant == kInvalidGuid)
        {
            ++free;
        }
    }
    return free;
}

bool GridLayout::CanFit(std::size_t count, Footprint footprint) const
{
    if (count == 0)
    {
        return true;
    }
    // 格子布局的"放不放得下"不能用面积算 —— 碎片化会让总空格够、却摆不进去。
    // 只能在一份影子占用图上真的试摆一遍。这正是扁平布局那句
    // `空格数 >= count` 在格子下失效的地方。
    std::vector<Guid> scratch = cells;
    constexpr Guid kScratchOccupied{1};  // 影子图上只区分"空 / 非空"
    for (std::size_t i = 0; i < count; ++i)
    {
        const SlotId anchor = FindFreeRect(scratch, footprint);
        if (anchor == kInvalidSlot)
        {
            return false;
        }
        Stamp(scratch, anchor, footprint, kScratchOccupied);
    }
    return true;
}

SlotId GridLayout::Place(Guid guid, Footprint footprint)
{
    Remove(guid);  // 同 FlatLayout::Place:同一 guid 不可能同时占两块
    const SlotId anchor = FindFreeRect(cells, footprint);
    if (anchor == kInvalidSlot)
    {
        return kInvalidSlot;
    }
    Stamp(cells, anchor, footprint, guid);
    placed[guid] = Placement{anchor, footprint};
    anchorToGuid[anchor] = guid;
    return anchor;
}

bool GridLayout::PlaceAt(Guid guid, SlotId slot, Footprint footprint)
{
    if (slot == kInvalidSlot || width == 0 || slot >= Capacity())
    {
        return false;
    }
    // 失败保持 fail-closed：先记住原位置，目标区域被占时还原原占位，不丢失物品位置。
    std::optional<Placement> previous;
    if (auto it = placed.find(guid); it != placed.end())
    {
        previous = it->second;
        Remove(guid);
    }

    const uint32_t x = slot % width;
    const uint32_t y = slot / width;
    if (!RectFree(cells, x, y, footprint))
    {
        if (previous)
        {
            Stamp(cells, previous->anchor, previous->footprint, guid);
            placed[guid] = *previous;
            anchorToGuid[previous->anchor] = guid;
        }
        return false;
    }
    Stamp(cells, slot, footprint, guid);
    placed[guid] = Placement{slot, footprint};
    anchorToGuid[slot] = guid;
    return true;
}

void GridLayout::Remove(Guid guid)
{
    auto it = placed.find(guid);
    if (it == placed.end())
    {
        return;
    }
    Stamp(cells, it->second.anchor, it->second.footprint, kInvalidGuid);
    anchorToGuid.erase(it->second.anchor);
    placed.erase(it);
}

void GridLayout::Clear()
{
    cells.assign(Capacity(), kInvalidGuid);
    placed.clear();
    anchorToGuid.clear();
}

SlotId GridLayout::SlotOf(Guid guid) const
{
    auto it = placed.find(guid);
    return it != placed.end() ? it->second.anchor : kInvalidSlot;
}

Guid GridLayout::At(SlotId slot) const
{
    // 命中的是"覆盖该单元的实例",不只是锚点 —— 点在一件 2x3 装备的任意
    // 一格上都应该拿到它。
    if (slot >= cells.size())
    {
        return kInvalidGuid;
    }
    return cells[slot];
}

void GridLayout::ApplyOrder(const std::vector<Guid> &order)
{
    // SupportsCompaction() 返回 false,桥层不该走到这里。真走到了是编程错误:
    // 重排会把玩家手摆的格子布局全打乱。
    LOG_ERROR << "GridLayout::ApplyOrder called on a layout that does not support compaction "
              << "(order size=" << order.size() << "); ignored";
}

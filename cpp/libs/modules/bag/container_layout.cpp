#include "container_layout.h"

#include "engine/core/error_handling/error_handling.h"

// ═════════════════════════════════════════════════════════════════════════
// FlatLayout
// ═════════════════════════════════════════════════════════════════════════

std::size_t FlatLayout::FreeCells() const
{
    // 钳到 0 而不是直接相减:PlaceAt 不做越界校验(见下),理论上可能出现
    // 槽位数超过容量的脏状态,无符号相减会下溢成天文数字并让"背包永远不满"。
    return capacity_ > slotToGuid_.size() ? capacity_ - slotToGuid_.size() : 0;
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
    for (SlotId slot = 0; slot < static_cast<SlotId>(capacity_); ++slot)
    {
        if (slotToGuid_.contains(slot))
        {
            continue;
        }
        slotToGuid_.emplace(slot, guid);
        guidToSlot_.emplace(guid, slot);
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
    if (slot >= static_cast<SlotId>(capacity_))
    {
        return false;
    }

    // 同样 fail-closed:槽位已被**别人**占着就拒绝,绝不静默顶替。
    // 拆分前是后写者覆盖先写者,被顶掉的那件物品仍留在 items 里却没有位置 ——
    // 一个查不到、删不掉、还占着容量的孤儿。
    auto existing = slotToGuid_.find(slot);
    if (existing != slotToGuid_.end() && existing->second != guid)
    {
        return false;
    }

    // 同一个 guid 若已在别的槽位,先释放旧槽位,否则旧槽位会被永久占着。
    auto previous = guidToSlot_.find(guid);
    if (previous != guidToSlot_.end() && previous->second != slot)
    {
        slotToGuid_.erase(previous->second);
    }

    slotToGuid_[slot] = guid;
    guidToSlot_[guid] = slot;
    return true;
}

void FlatLayout::Remove(Guid guid)
{
    auto it = guidToSlot_.find(guid);
    if (it == guidToSlot_.end())
    {
        return;
    }
    // O(1)。拆分前这里是遍历 posToGuid 比对 value 的线性扫描,因为当时根本
    // 没有反向索引 —— 布局没被当成一层东西,自然也不会有自己的索引。
    slotToGuid_.erase(it->second);
    guidToSlot_.erase(it);
}

void FlatLayout::Clear()
{
    slotToGuid_.clear();
    guidToSlot_.clear();
}

SlotId FlatLayout::SlotOf(Guid guid) const
{
    auto it = guidToSlot_.find(guid);
    return it != guidToSlot_.end() ? it->second : kInvalidSlot;
}

Guid FlatLayout::At(SlotId slot) const
{
    auto it = slotToGuid_.find(slot);
    return it != slotToGuid_.end() ? it->second : kInvalidGuid;
}

bool FlatLayout::IsCompact() const
{
    // 键唯一且个数 == size,所以"每个键都 < size"等价于键集合恰为 {0..size-1}。
    // 与拆分前 Bag::IsAlreadyMergedAndCompact 的第 ② 条判据逐字同构。
    for (const auto &[slot, guid] : slotToGuid_)
    {
        if (slot >= slotToGuid_.size())
        {
            return false;
        }
    }
    return true;
}

void FlatLayout::ApplyOrder(const std::vector<Guid> &order)
{
    // 按给定顺序铺到 0,1,2,...。顺序是桥层算好的,这里不做任何排序判断。
    slotToGuid_.clear();
    guidToSlot_.clear();
    SlotId next = 0;
    for (const Guid guid : order)
    {
        slotToGuid_[next] = guid;
        guidToSlot_[guid] = next;
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
    : width_(width), height_(height)
{
    cells_.assign(Capacity(), kInvalidGuid);
}

bool GridLayout::RectFree(const std::vector<Guid> &cells, uint32_t x, uint32_t y,
                          Footprint footprint) const
{
    if (footprint.width == 0 || footprint.height == 0)
    {
        return false;
    }
    // 先判越界再判占用:宽高相加用 uint64 免得溢出绕回来变成"没越界"。
    if (static_cast<uint64_t>(x) + footprint.width > width_ ||
        static_cast<uint64_t>(y) + footprint.height > height_)
    {
        return false;
    }
    for (uint32_t dy = 0; dy < footprint.height; ++dy)
    {
        for (uint32_t dx = 0; dx < footprint.width; ++dx)
        {
            if (cells[static_cast<std::size_t>(y + dy) * width_ + (x + dx)] != kInvalidGuid)
            {
                return false;
            }
        }
    }
    return true;
}

SlotId GridLayout::FindFreeRect(const std::vector<Guid> &cells, Footprint footprint) const
{
    for (uint32_t y = 0; y < height_; ++y)
    {
        for (uint32_t x = 0; x < width_; ++x)
        {
            if (RectFree(cells, x, y, footprint))
            {
                return y * width_ + x;
            }
        }
    }
    return kInvalidSlot;
}

void GridLayout::Stamp(std::vector<Guid> &cells, SlotId anchor, Footprint footprint,
                       Guid guid) const
{
    const uint32_t x0 = anchor % width_;
    const uint32_t y0 = anchor / width_;
    for (uint32_t dy = 0; dy < footprint.height; ++dy)
    {
        for (uint32_t dx = 0; dx < footprint.width; ++dx)
        {
            cells[static_cast<std::size_t>(y0 + dy) * width_ + (x0 + dx)] = guid;
        }
    }
}

void GridLayout::Resize(std::size_t newCapacity)
{
    if (width_ == 0)
    {
        LOG_ERROR << "GridLayout::Resize on a zero-width grid, ignored";
        return;
    }

    // 按整行增减:容量向上取整到整行。
    const std::size_t newHeight = (newCapacity + width_ - 1) / width_;

    if (newHeight < height_)
    {
        // 缩容:被砍掉的行必须完全空着。布局层无权决定"哪件东西该被挤掉",
        // 那是桥层 / 玩法的决策,这里只做 fail-closed。
        for (std::size_t idx = newHeight * width_; idx < cells_.size(); ++idx)
        {
            if (cells_[idx] != kInvalidGuid)
            {
                LOG_ERROR << "GridLayout::Resize refused: shrinking to " << newCapacity
                          << " would evict an occupied cell at index " << idx;
                return;
            }
        }
    }

    height_ = static_cast<uint32_t>(newHeight);
    cells_.resize(Capacity(), kInvalidGuid);
}

std::size_t GridLayout::FreeCells() const
{
    std::size_t free = 0;
    for (const Guid occupant : cells_)
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
    std::vector<Guid> scratch = cells_;
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
    const SlotId anchor = FindFreeRect(cells_, footprint);
    if (anchor == kInvalidSlot)
    {
        return kInvalidSlot;
    }
    Stamp(cells_, anchor, footprint, guid);
    placed_[guid] = Placement{anchor, footprint};
    anchorToGuid_[anchor] = guid;
    return anchor;
}

bool GridLayout::PlaceAt(Guid guid, SlotId slot, Footprint footprint)
{
    if (slot == kInvalidSlot || width_ == 0 || slot >= Capacity())
    {
        return false;
    }
    // 若这个 guid 已经摆在别处,先摘掉再判 —— 否则它自己的格子会把自己挡住。
    Remove(guid);

    const uint32_t x = slot % width_;
    const uint32_t y = slot / width_;
    if (!RectFree(cells_, x, y, footprint))
    {
        return false;
    }
    Stamp(cells_, slot, footprint, guid);
    placed_[guid] = Placement{slot, footprint};
    anchorToGuid_[slot] = guid;
    return true;
}

void GridLayout::Remove(Guid guid)
{
    auto it = placed_.find(guid);
    if (it == placed_.end())
    {
        return;
    }
    Stamp(cells_, it->second.anchor, it->second.footprint, kInvalidGuid);
    anchorToGuid_.erase(it->second.anchor);
    placed_.erase(it);
}

void GridLayout::Clear()
{
    cells_.assign(Capacity(), kInvalidGuid);
    placed_.clear();
    anchorToGuid_.clear();
}

SlotId GridLayout::SlotOf(Guid guid) const
{
    auto it = placed_.find(guid);
    return it != placed_.end() ? it->second.anchor : kInvalidSlot;
}

Guid GridLayout::At(SlotId slot) const
{
    // 命中的是"覆盖该单元的实例",不只是锚点 —— 点在一件 2x3 装备的任意
    // 一格上都应该拿到它。
    if (slot >= cells_.size())
    {
        return kInvalidGuid;
    }
    return cells_[slot];
}

void GridLayout::ApplyOrder(const std::vector<Guid> &order)
{
    // SupportsCompaction() 返回 false,桥层不该走到这里。真走到了是编程错误:
    // 重排会把玩家手摆的格子布局全打乱。
    LOG_ERROR << "GridLayout::ApplyOrder called on a layout that does not support compaction "
              << "(order size=" << order.size() << "); ignored";
}

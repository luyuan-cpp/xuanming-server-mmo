#include <gtest/gtest.h>

#include "engine/core/type_define/type_define.h"
#include "engine/thread_context/snow_flake_manager.h"

#include "table/code/item_table.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/bag_service.h"
#include "modules/gain_block/gain_block_service.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "../test_config_helper.h"

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

uint32_t MaxStack(uint32_t configId)
{
    return ItemTableManager::Instance().FindById(configId).first->max_stack_size();
}

InitItemParam MakeItem(uint32_t configId, uint32_t size)
{
    InitItemParam p;
    p.itemPBComp.set_config_id(configId);
    p.itemPBComp.set_size(size);
    return p;
}

InitItemParam MakeItem(uint32_t configId)
{
    return MakeItem(configId, MaxStack(configId));
}

/// AddItem 并返回本次写入的**最后一个**实例 guid(即最新占用的那一格)。
///
/// 取代旧的 Bag::LastGeneratedItemGuid():那是回读发号器里的"上一个号"的残值通道,
/// 已随生产代码一起删除 —— 同一发号器还铸 tx_id / snapshot_id 会覆盖它,
/// 且并堆、沿用预设 guid 这些不铸号的路径读到的是上一件物品的号。
Guid AddItemAndGetLastWritten(Bag &bag, const InitItemParam &item)
{
    std::vector<Guid> written;
    EXPECT_EQ(kSuccess, bag.AddItem(item, &written));
    EXPECT_FALSE(written.empty()) << "successful AddItem must report at least one written instance";
    return written.empty() ? kInvalidGuid : written.back();
}

/// Verify the last added item is at `pos` with expected config and size.
/// `guid` 来自 AddItem 的写入回执,断言"回执说写了 X"与"背包里 pos 处就是 X"一致。
void VerifyLastAdded(Bag &bag, uint32_t pos, uint32_t configId, uint32_t size, Guid guid)
{
    auto *byPos = bag.GetItemCompByPos(pos);
    auto *byGuid = bag.GetItemCompByGuid(guid);
    ASSERT_NE(nullptr, byPos);
    ASSERT_NE(nullptr, byGuid);
    EXPECT_EQ(configId, byPos->config_id());
    EXPECT_EQ(size, byPos->size());
    EXPECT_EQ(configId, byGuid->config_id());
    EXPECT_EQ(size, byGuid->size());
    EXPECT_EQ(guid, byPos->item_id());
    EXPECT_EQ(guid, byGuid->item_id());
    EXPECT_EQ(pos, bag.GetItemPosByGuid(guid));
}

RemoveItemByPosParam MakeRemoveParam(Bag &bag, uint32_t pos, uint32_t configId, uint32_t removeSize = 1)
{
    RemoveItemByPosParam dp;
    dp.pos = pos;
    dp.item_guid = bag.GetItemCompByPos(pos)->item_id();
    dp.item_config_id = configId;
    dp.size = removeSize;
    return dp;
}

// Config IDs (matches item_table.json)
constexpr uint32_t kNonStack1 = 1; // non-stackable
constexpr uint32_t kNonStack2 = 2; // non-stackable
constexpr uint32_t kStack9 = 9;    // stackable
constexpr uint32_t kStack10 = 10;  // stackable
constexpr uint32_t kStack11 = 11;  // stackable

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

TEST(BagTest, NullItem)
{
    Bag bag;
    EXPECT_EQ(nullptr, bag.GetItemCompByGuid(0));
}

TEST(BagTest, AddNewGridItem)
{
    Bag bag;
    auto item = MakeItem(kNonStack1);
    const Guid written = AddItemAndGetLastWritten(bag, item);
    EXPECT_EQ(1, bag.OccupiedGridCount());
    EXPECT_EQ(1, bag.GridSlotCount());
    VerifyLastAdded(bag, 0, kNonStack1, item.itemPBComp.size(), written);
}

TEST(BagTest, AddNewGridItemFull)
{
    Bag bag;
    auto item = MakeItem(kNonStack1);

    // Fill initial capacity
    for (uint32_t i = 0; i < (uint32_t)kDefaultCapacity; i++)
    {
        const Guid written = AddItemAndGetLastWritten(bag, item);
        EXPECT_EQ(i + 1, bag.OccupiedGridCount());
        EXPECT_EQ(i + 1, bag.GridSlotCount());
        VerifyLastAdded(bag, i, kNonStack1, item.itemPBComp.size(), written);
    }
    EXPECT_EQ(kDefaultCapacity, bag.OccupiedGridCount());
    EXPECT_EQ(kDefaultCapacity, bag.GridSlotCount());

    // Full — adding one more fails
    EXPECT_EQ(kBagAddItemBagFull, bag.AddItem(item));

    // ExpandCapacity more slots and fill again
    bag.ExpandCapacity(kDefaultCapacity);
    for (uint32_t i = 0; i < (uint32_t)kDefaultCapacity; i++)
    {
        const Guid written = AddItemAndGetLastWritten(bag, item);
        uint32_t idx = i + (uint32_t)kDefaultCapacity;
        EXPECT_EQ(idx + 1, bag.OccupiedGridCount());
        EXPECT_EQ(idx + 1, bag.GridSlotCount());
        VerifyLastAdded(bag, idx, kNonStack1, item.itemPBComp.size(), written);
    }
    EXPECT_EQ(kDefaultCapacity * 2, bag.OccupiedGridCount());
    EXPECT_EQ(kDefaultCapacity * 2, bag.GridSlotCount());
}

TEST(BagTest, Add10CanStack10CanNotStack)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);

    // Fill all initial slots with non-stackable items
    auto nonStack = MakeItem(kNonStack1, MaxStack(kNonStack1) * kDefaultCapacity);
    EXPECT_EQ(kSuccess, bag.AddItem(nonStack));

    // Fill unlocked slots with stackable items
    auto stackable = MakeItem(kStack10, MaxStack(kStack10) * kDefaultCapacity);
    EXPECT_EQ(kSuccess, bag.AddItem(stackable));

    EXPECT_EQ(kDefaultCapacity * 2, bag.OccupiedGridCount());
    EXPECT_EQ(kDefaultCapacity * 2, bag.GridSlotCount());
}

TEST(BagTest, AddStackItem12121212)
{
    Bag bag;
    auto maxStack = MaxStack(kStack9);
    auto halfStack = maxStack / 2;
    uint32_t totalSize = 0;

    for (uint32_t i = 0; i < (uint32_t)kDefaultCapacity * 2; i++)
    {
        uint32_t addSize = (i % 2 == 0) ? halfStack : maxStack;
        auto item = MakeItem(kStack9, addSize);
        totalSize += addSize;

        std::vector<Guid> written;
        auto ret = bag.AddItem(item, &written);
        if (ret != kSuccess)
            break;
        EXPECT_EQ(kSuccess, ret);
        ASSERT_FALSE(written.empty());
        // 回执的最后一个 = 本次数量最终落到的那一格(并堆时是被并入的既有堆,
        // 溢出时是新开的那一格),正是下面按 lastIdx 断言的实例。
        const Guid lastWritten = written.back();

        auto gridSize = Bag::GridsNeededFor(totalSize, maxStack);
        uint32_t lastIdx = uint32_t(gridSize - 1);

        EXPECT_EQ(gridSize, bag.OccupiedGridCount());
        EXPECT_EQ(gridSize, bag.GridSlotCount());

        if (totalSize % maxStack == 0)
        {
            EXPECT_EQ(maxStack, bag.GetItemCompByPos(lastIdx / 2)->size());
        }
        else
        {
            EXPECT_EQ(halfStack, (uint32_t)bag.GetItemCompByPos(lastIdx)->size());
            EXPECT_EQ(halfStack, bag.GetItemCompByGuid(lastWritten)->size());
        }

        EXPECT_EQ(kStack9, bag.GetItemCompByGuid(lastWritten)->config_id());
        EXPECT_EQ(lastIdx, bag.GetItemPosByGuid(lastWritten));
        EXPECT_EQ(lastWritten, bag.GetItemCompByPos(lastIdx)->item_id());
        EXPECT_EQ(lastWritten, bag.GetItemCompByGuid(lastWritten)->item_id());
    }

    EXPECT_EQ(kDefaultCapacity, bag.OccupiedGridCount());
    EXPECT_EQ(kDefaultCapacity, bag.GridSlotCount());
}

TEST(BagTest, AddStackItemUnlock)
{
    Bag bag;
    auto item = MakeItem(kStack10);

    // Fill initial capacity with full stacks
    for (uint32_t i = 0; i < (uint32_t)kDefaultCapacity; i++)
    {
        const Guid written = AddItemAndGetLastWritten(bag, item);
        EXPECT_EQ(i + 1, bag.OccupiedGridCount());
        EXPECT_EQ(i + 1, bag.GridSlotCount());
        VerifyLastAdded(bag, i, kStack10, item.itemPBComp.size(), written);
    }
    EXPECT_EQ(kDefaultCapacity, bag.OccupiedGridCount());
    EXPECT_EQ(kDefaultCapacity, bag.GridSlotCount());

    // Full — unlock and fill again
    EXPECT_EQ(kBagAddItemBagFull, bag.AddItem(item));
    bag.ExpandCapacity(kDefaultCapacity);
    for (uint32_t i = 0; i < (uint32_t)kDefaultCapacity; i++)
    {
        const Guid written = AddItemAndGetLastWritten(bag, item);
        uint32_t idx = i + (uint32_t)kDefaultCapacity;
        EXPECT_EQ(idx + 1, bag.OccupiedGridCount());
        EXPECT_EQ(idx + 1, bag.GridSlotCount());
        VerifyLastAdded(bag, idx, kStack10, item.itemPBComp.size(), written);
    }
    EXPECT_EQ(kDefaultCapacity * 2, bag.OccupiedGridCount());
    EXPECT_EQ(kDefaultCapacity * 2, bag.GridSlotCount());
}

TEST(BagTest, AdequateSizeAddItemCannotStackItemFull)
{
    Bag bag;
    ItemCountMap needed{{kNonStack1, (uint32_t)kDefaultCapacity + 1}};
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(needed));

    needed[kNonStack1] = (uint32_t)kDefaultCapacity;
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(needed));
}

TEST(BagTest, AdequateSizeAddItemmixtureFull)
{
    Bag bag;
    auto maxStack10 = MaxStack(kStack10);
    ItemCountMap needed{
        {kNonStack1, 1},
        {kStack10, maxStack10 * (uint32_t)kDefaultCapacity}};

    // 1 non-stackable + 10 full stacks > capacity
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(needed));

    // 1 non-stackable + 9 full stacks = capacity
    needed[kStack10] = (uint32_t)(kDefaultCapacity - 1) * maxStack10;
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(needed));

    // Occupy one slot with a non-stackable item
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));

    // Now 9 remaining slots — 1 non-stackable + 9 stacks won't fit
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(needed));

    // 1 non-stackable + 8 stacks = 9 slots needed
    needed[kStack10] = (uint32_t)(kDefaultCapacity - 2) * maxStack10;
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(needed));

    // Occupy one more slot with a stackable item
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));

    // 8 remaining — 1 + 8 won't fit
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(needed));

    // 1 + 7 = 8 slots needed
    needed[kStack10] = (uint32_t)(kDefaultCapacity - 3) * maxStack10;
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(needed));

    // Add a partially-filled stack (max - 100)
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, maxStack10 - 100)));
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(needed));
}

// CheckSpaceFor: empty request always fits, even on a full bag.
TEST(BagTest, CheckSpaceForEmptyRequest)
{
    Bag bag;
    // Fill every slot so EmptyGridCount() == 0.
    for (uint32_t i = 0; i < (uint32_t)kDefaultCapacity; i++)
    {
        EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));
    }
    EXPECT_EQ(kDefaultCapacity, bag.OccupiedGridCount());

    ItemCountMap empty;
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(empty));

    // A zero-count entry must not consume a grid either (no false "full").
    ItemCountMap zeroCount{{kNonStack1, 0}, {kStack10, 0}};
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(zeroCount));
}

// CheckSpaceFor: a single config requested in BOTH stackable and non-stackable
// flavours via one map is impossible (one config has one max_stack_size), but a
// request CAN mix several configs of different kinds. Verify the new-grid math
// for a pure stackable request that packs across multiple grids.
TEST(BagTest, CheckSpaceForStackablePacking)
{
    Bag bag; // kDefaultCapacity (10) empty grids
    const auto maxStack = MaxStack(kStack10);

    // Exactly 10 full stacks -> 10 grids -> fits.
    ItemCountMap exactly{{kStack10, maxStack * (uint32_t)kDefaultCapacity}};
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(exactly));

    // One unit more -> needs an 11th grid -> fails.
    ItemCountMap oneOver{{kStack10, maxStack * (uint32_t)kDefaultCapacity + 1}};
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(oneOver));

    // A partial last grid still counts as a whole grid: maxStack + 1 -> 2 grids.
    ItemCountMap partial{{kStack10, maxStack + 1}};
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(partial));
}

// CheckSpaceFor: stackable demand first soaks into the free room of existing
// partial stacks before charging new grids.
TEST(BagTest, CheckSpaceForSoaksExistingStacks)
{
    Bag bag; // 10 empty grids
    const auto maxStack = MaxStack(kStack10);

    // Put one partial stack in the bag: occupies 1 grid, leaves (maxStack - 1)
    // units of room inside it. 9 empty grids remain.
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, 1)));
    EXPECT_EQ(1, bag.OccupiedGridCount());

    // Requesting (maxStack - 1) units fits entirely inside the existing stack:
    // 0 new grids needed.
    ItemCountMap intoExisting{{kStack10, maxStack - 1}};
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(intoExisting));

    // Free room (maxStack - 1) + 9 empty grids * maxStack is the true ceiling.
    // Ask for exactly that -> fits.
    const uint32_t ceiling = (maxStack - 1) + 9 * maxStack;
    ItemCountMap atCeiling{{kStack10, ceiling}};
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(atCeiling));

    // One unit over the ceiling -> fails.
    ItemCountMap overCeiling{{kStack10, ceiling + 1}};
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(overCeiling));
}

// CheckSpaceFor: a request mixing non-stackable + stackable configs, against a
// bag that already holds a partial stack to soak into.
TEST(BagTest, CheckSpaceForMixedWithExistingStack)
{
    Bag bag; // 10 empty grids
    const auto maxStack = MaxStack(kStack10);

    // One partial stack of kStack10 (1 grid used, maxStack-1 room). 9 grids free.
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, 1)));

    // Request: 9 non-stackable (9 grids) + (maxStack - 1) stackable (soaks into
    // the existing stack, 0 new grids) = 9 grids needed == 9 free. Fits exactly.
    ItemCountMap fits{
        {kNonStack1, 9},
        {kStack10, maxStack - 1}};
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(fits));

    // Bump the stackable demand by maxStack units -> needs 1 extra grid -> 10 > 9.
    ItemCountMap overflow{
        {kNonStack1, 9},
        {kStack10, maxStack - 1 + maxStack}};
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(overflow));
}

// CheckSpaceFor: multiple DISTINCT non-stackable configs each cost one grid per
// unit and are summed together.
TEST(BagTest, CheckSpaceForMultipleNonStackable)
{
    Bag bag; // 10 empty grids
    ItemCountMap fits{{kNonStack1, 4}, {kNonStack2, 6}}; // 4 + 6 = 10
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(fits));

    ItemCountMap overflow{{kNonStack1, 5}, {kNonStack2, 6}}; // 5 + 6 = 11 > 10
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(overflow));
}

// CheckSpaceFor: a full stack contributes ZERO free room (the `< maxStack`
// guard), so the request can only use empty grids.
TEST(BagTest, CheckSpaceForFullStackNoRoom)
{
    Bag bag; // 10 empty grids
    const auto maxStack = MaxStack(kStack10);

    // One FULL stack -> 1 grid used, 0 room inside it. 9 grids free.
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    EXPECT_EQ(1, bag.OccupiedGridCount());

    // 9 full stacks fit in the 9 remaining grids.
    ItemCountMap fits{{kStack10, maxStack * 9}};
    EXPECT_EQ(kSuccess, bag.CheckSpaceFor(fits));

    // 9 full stacks + 1 unit needs a 10th new grid -> only 9 free -> fails.
    ItemCountMap overflow{{kStack10, maxStack * 9 + 1}};
    EXPECT_EQ(kBagItemNotStacked, bag.CheckSpaceFor(overflow));
}

TEST(BagTest, AdequateItem)
{
    Bag bag;
    bag.ExpandCapacity(20);
    auto maxStack10 = MaxStack(kStack10);
    auto maxStack11 = MaxStack(kStack11);

    // Empty bag — no items
    ItemCountMap required{{kStack10, 1}};
    EXPECT_EQ(kBagInsufficientItems, bag.CheckItemsAvailable(required));

    // Add one full stack of id 10
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    EXPECT_EQ(kSuccess, bag.CheckItemsAvailable(required));

    required[kStack10] = maxStack10 / 2;
    EXPECT_EQ(kSuccess, bag.CheckItemsAvailable(required));

    // Also require 1 non-stackable — not in bag yet
    required.emplace(kNonStack1, 1);
    EXPECT_EQ(kBagInsufficientItems, bag.CheckItemsAvailable(required));

    // Add a non-stackable item
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));
    EXPECT_EQ(kSuccess, bag.CheckItemsAvailable(required));

    // Require exactly 1 full stack of id 10
    required[kStack10] = maxStack10;
    EXPECT_EQ(kSuccess, bag.CheckItemsAvailable(required));

    // Require more than available
    required[kStack10] = maxStack10 + 1;
    EXPECT_EQ(kBagInsufficientItems, bag.CheckItemsAvailable(required));

    required[kStack10] = maxStack10 * 3;
    EXPECT_EQ(kBagInsufficientItems, bag.CheckItemsAvailable(required));

    // Add second stack of id 10 — still not enough for 3 stacks
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    EXPECT_EQ(kBagInsufficientItems, bag.CheckItemsAvailable(required));

    // Add id 11 — doesn't help with id 10 requirement
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11, maxStack11 * 3)));
    EXPECT_EQ(kBagInsufficientItems, bag.CheckItemsAvailable(required));

    // Third stack of id 10 — now have 3 stacks
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    EXPECT_EQ(kSuccess, bag.CheckItemsAvailable(required));

    // Also require 3 stacks of id 11 — we added 3 stacks above
    required[kStack11] = maxStack11 * 3;
    EXPECT_EQ(kSuccess, bag.CheckItemsAvailable(required));
}

TEST(BagTest, DelItem)
{
    Bag bag;
    bag.ExpandCapacity(20);
    auto maxStack10 = MaxStack(kStack10);
    auto maxStack11 = MaxStack(kStack11);

    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, maxStack10 * 2)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack2)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11)));

    // Remove 1 unit of id 10
    ItemCountMap toRemove{{kStack10, 1}};
    EXPECT_EQ(kSuccess, bag.RemoveItems(toRemove));
    EXPECT_EQ(maxStack10 * 2 - 1, bag.GetTotalItemCount(kStack10));
    EXPECT_EQ(MaxStack(kNonStack1), bag.GetTotalItemCount(kNonStack1));
    EXPECT_EQ(MaxStack(kNonStack2), bag.GetTotalItemCount(kNonStack2));
    EXPECT_EQ(maxStack11, bag.GetTotalItemCount(kStack11));

    // Remove everything remaining
    toRemove[kStack10] = maxStack10 * 2 - 1;
    toRemove[kNonStack1] = MaxStack(kNonStack1);
    toRemove[kNonStack2] = MaxStack(kNonStack2);
    toRemove[kStack11] = maxStack11;
    EXPECT_EQ(kSuccess, bag.RemoveItems(toRemove));
    EXPECT_EQ(0, bag.GetTotalItemCount(kStack10));
    EXPECT_EQ(0, bag.GetTotalItemCount(kNonStack1));
    EXPECT_EQ(0, bag.GetTotalItemCount(kNonStack2));
    EXPECT_EQ(0, bag.GetTotalItemCount(kStack11));

    // Grid slots remain (empty)
    EXPECT_EQ(5, bag.OccupiedGridCount());
    EXPECT_EQ(5, bag.GridSlotCount());
    for (uint32_t p : {0u, 1u, 2u, 3u, 4u})
    {
        EXPECT_TRUE(bag.GridSlots().find(p) != bag.GridSlots().end());
    }
}

TEST(BagTest, Del)
{
    Bag bag;
    const Guid written = AddItemAndGetLastWritten(bag, MakeItem(kNonStack1));
    EXPECT_EQ(1, bag.OccupiedGridCount());
    EXPECT_EQ(1, bag.GridSlotCount());

    EXPECT_EQ(kSuccess, bag.RemoveItem(written));
    EXPECT_EQ(0, bag.OccupiedGridCount());
    EXPECT_EQ(0, bag.GridSlotCount());
}

TEST(BagTest, RemoveItemByPos)
{
    Bag bag;
    auto maxStack = MaxStack(kStack10);
    // 取本次写入的实例 guid 用 AddItem 的显式回执。
    // 旧写法是 Bag::LastGeneratedItemGuid() 回读发号器残值,该通道已删除
    // (同一发号器还铸 tx_id / snapshot_id 会覆盖它,且不铸号的路径读到的是上一件物品)。
    std::vector<Guid> written;
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10), &written));
    ASSERT_EQ(1u, written.size()) << "single stackable add must report exactly one instance";
    const Guid addedGuid = written.front();
    EXPECT_EQ(1, bag.OccupiedGridCount());
    EXPECT_EQ(1, bag.GridSlotCount());

    // Missing fields → various errors
    RemoveItemByPosParam dp;
    EXPECT_EQ(kBagDelItemPos, bag.RemoveItemByPos(dp));

    dp.pos = 0;
    EXPECT_EQ(kBagDelItemGuid, bag.RemoveItemByPos(dp));

    dp.item_guid = addedGuid;
    EXPECT_EQ(kBagDelItemConfig, bag.RemoveItemByPos(dp));

    // Remove 1 unit
    dp.item_config_id = kStack10;
    EXPECT_EQ(kSuccess, bag.RemoveItemByPos(dp));
    EXPECT_EQ(maxStack - 1, bag.GetTotalItemCount(kStack10));

    // Remove the rest
    dp.size = maxStack - 1;
    EXPECT_EQ(kSuccess, bag.RemoveItemByPos(dp));
    EXPECT_EQ(0, bag.GetTotalItemCount(kStack10));
    EXPECT_EQ(1, bag.OccupiedGridCount());
    EXPECT_EQ(1, bag.GridSlotCount());
    EXPECT_EQ(0, bag.GetItemCompByPos(0)->size());
    EXPECT_EQ(0, bag.GetItemCompByGuid(addedGuid)->size());
}

/// Helper: fill `count` slots of `configId`, then reduce each to 1 unit.
void FillAndReduceToOne(Bag &bag, uint32_t configId, uint32_t startPos, uint32_t count)
{
    auto maxStack = MaxStack(configId);
    for (uint32_t i = startPos; i < startPos + count; ++i)
    {
        auto dp = MakeRemoveParam(bag, i, configId, maxStack - 1);
        EXPECT_EQ(kSuccess, bag.RemoveItemByPos(dp));
    }
}

TEST(BagTest, Neaten1)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);

    // Fill both halves with full stacks
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, MaxStack(kStack10) * kDefaultCapacity)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11, MaxStack(kStack11) * kDefaultCapacity)));

    // Reduce every slot to 1 unit
    FillAndReduceToOne(bag, kStack10, 0, (uint32_t)kDefaultCapacity);
    FillAndReduceToOne(bag, kStack11, (uint32_t)kDefaultCapacity, (uint32_t)kDefaultCapacity);

    for (uint32_t i = 0; i < (uint32_t)bag.GridSlotCount(); ++i)
        EXPECT_EQ(1, bag.GetItemCompByPos(i)->size());

    bag.MergeAndCompact();

    // 10 units of each config consolidate into 1 slot each (size = 10)
    EXPECT_EQ(2, bag.OccupiedGridCount());
    EXPECT_EQ(2, bag.GridSlotCount());
    for (auto &[pos, guid] : bag.GridSlots())
        EXPECT_EQ(kDefaultCapacity, (std::size_t)bag.GetItemCompByPos(bag.GetItemPosByGuid(guid))->size());
    EXPECT_EQ(kDefaultCapacity, bag.GetTotalItemCount(kStack10));
    EXPECT_EQ(kDefaultCapacity, bag.GetTotalItemCount(kStack11));
}

// 关键回归:位置已经是连续的 0,1(没有空洞),但同一 config 是"一高一低"两个
// 未满堆。早退判据必须识别出"前面没满"仍需整理,绝不能因为位置紧凑就跳过。
TEST(BagTest, MergeAndCompactMergesHighLowStacksEvenWhenContiguous)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);

    // 两个满堆 -> pos 0,1,位置天然连续无空洞
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, MaxStack(kStack10) * 2)));
    EXPECT_EQ(2, bag.OccupiedGridCount());

    // 各削到 1 单位 -> [1,1] 一高一低(两个未满堆),位置仍是 0,1 连续
    FillAndReduceToOne(bag, kStack10, 0, 2);
    EXPECT_EQ(1, bag.GetItemCompByPos(0)->size());
    EXPECT_EQ(1, bag.GetItemCompByPos(1)->size());

    // 位置紧凑但前面没满 —— 必须整理。若早退误判为"已最优"则此处不会合并。
    bag.MergeAndCompact();

    EXPECT_EQ(1, bag.OccupiedGridCount());
    EXPECT_EQ(1, bag.GridSlotCount());
    EXPECT_EQ(2, bag.GetItemCompByPos(0)->size()); // 1+1 合成一个满前堆
    EXPECT_EQ(2, bag.GetTotalItemCount(kStack10));
}

// 已最优(满堆在前 + 至多一个未满堆 + 位置连续)时早退:整理应是无操作,
// 既不改 size 也不挪动 guid 的位置。
TEST(BagTest, MergeAndCompactIsNoOpWhenAlreadyOptimal)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);

    const auto maxStack10 = MaxStack(kStack10);
    // [满, 满, 余3] 的 kStack10:前面是满堆,仅末尾一个未满堆 -> 已最优
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, maxStack10 * 2 + 3)));
    // 另一种物品一个满堆
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11)));
    EXPECT_EQ(4, bag.OccupiedGridCount());

    // 记录整理前每个格子的 guid 与 size
    const auto slotCount = (uint32_t)bag.GridSlotCount();
    std::vector<Guid> beforeGuid(slotCount);
    std::vector<uint32_t> beforeSize(slotCount);
    for (uint32_t i = 0; i < slotCount; ++i)
    {
        auto *item = bag.GetItemCompByPos(i);
        ASSERT_NE(nullptr, item);
        beforeGuid[i] = item->item_id();
        beforeSize[i] = item->size();
    }

    bag.MergeAndCompact(); // 已最优 -> 早退,什么都不该变

    EXPECT_EQ(4, bag.OccupiedGridCount());
    EXPECT_EQ(slotCount, (uint32_t)bag.GridSlotCount());
    for (uint32_t i = 0; i < slotCount; ++i)
    {
        auto *item = bag.GetItemCompByPos(i);
        ASSERT_NE(nullptr, item);
        EXPECT_EQ(beforeGuid[i], item->item_id()); // guid 留在原位
        EXPECT_EQ(beforeSize[i], item->size());    // size 未变
    }
}

// 关键回归:同一 config 的布局为 [未满, 满](未满堆排在满堆前面),位置已连续、
// 该 config 也只有 1 个未满堆。整理必须把满堆挪到组内前面、未满堆落到组内末尾
// (同种内满堆在前不变量)。
TEST(BagTest, MergeAndCompactPutsFullBeforePartialWithinConfig)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);

    const auto maxStack10 = MaxStack(kStack10);
    // 同种 kStack10 两个满堆 [满, 满],再把 pos0 削成 1 单位 -> [未满, 满]。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, maxStack10 * 2)));
    EXPECT_EQ(2, bag.OccupiedGridCount());
    auto dp = MakeRemoveParam(bag, 0, kStack10, maxStack10 - 1);
    EXPECT_EQ(kSuccess, bag.RemoveItemByPos(dp));

    const auto partialGuid = bag.GetItemCompByPos(0)->item_id(); // 未满堆
    const auto fullGuid = bag.GetItemCompByPos(1)->item_id();    // 满堆
    EXPECT_EQ(1, bag.GetItemCompByPos(0)->size());
    EXPECT_EQ(maxStack10, bag.GetItemCompByPos(1)->size());

    bag.MergeAndCompact(); // [未满, 满] -> 同种内满堆挪到前面

    EXPECT_EQ(2, bag.OccupiedGridCount());
    EXPECT_EQ(2, bag.GridSlotCount());
    // pos0 现在是满堆,pos1 是未满堆。
    EXPECT_EQ(fullGuid, bag.GetItemCompByPos(0)->item_id());
    EXPECT_EQ(maxStack10, bag.GetItemCompByPos(0)->size());
    EXPECT_EQ(partialGuid, bag.GetItemCompByPos(1)->item_id());
    EXPECT_EQ(1, bag.GetItemCompByPos(1)->size());
    // 数量守恒。
    EXPECT_EQ(maxStack10 + 1, bag.GetTotalItemCount(kStack10));
}

// 关键回归:不同 config 的满堆按 config 逆序排列 [config11, config10],
// 整理必须按 config 升序把同种聚拢 -> [config10, config11]。
TEST(BagTest, MergeAndCompactGroupsAndSortsByConfigAscending)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);

    // pos0 放 kStack11 满堆,pos1 放 kStack10 满堆 -> config 逆序布局。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    EXPECT_EQ(2, bag.OccupiedGridCount());
    EXPECT_EQ(kStack11, bag.GetItemCompByPos(0)->config_id());
    EXPECT_EQ(kStack10, bag.GetItemCompByPos(1)->config_id());

    bag.MergeAndCompact(); // [config11, config10] -> 按 config 升序聚拢

    EXPECT_EQ(2, bag.OccupiedGridCount());
    EXPECT_EQ(2, bag.GridSlotCount());
    // 同种聚拢:config 小的在前。
    EXPECT_EQ(kStack10, bag.GetItemCompByPos(0)->config_id());
    EXPECT_EQ(kStack11, bag.GetItemCompByPos(1)->config_id());
    EXPECT_EQ(MaxStack(kStack10), bag.GetItemCompByPos(0)->size());
    EXPECT_EQ(MaxStack(kStack11), bag.GetItemCompByPos(1)->size());
    // 数量守恒。
    EXPECT_EQ(MaxStack(kStack10), bag.GetTotalItemCount(kStack10));
    EXPECT_EQ(MaxStack(kStack11), bag.GetTotalItemCount(kStack11));
}

// 综合端到端:三种 config 各有"一高一低"两个未满堆,且 config 按降序摆放
// (11,10,9)。整理必须同时做到:① 同种内合并出 [满, 零头] ② 按 config 升序
// 把同种聚拢 ③ 组内满堆在前。最终布局应是
// [9满, 9零头, 10满, 10零头, 11满, 11零头]。
TEST(BagTest, MergeAndCompactGroupsMergesAndOrdersMultipleConfigs)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);

    const auto max9 = MaxStack(kStack9);
    const auto max10 = MaxStack(kStack10);
    const auto max11 = MaxStack(kStack11);

    // 按 config 降序铺:11 占 pos0,1;10 占 pos2,3;9 占 pos4,5。各两个满堆。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11, max11 * 2)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, max10 * 2)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack9, max9 * 2)));
    EXPECT_EQ(6, bag.OccupiedGridCount());

    // 把每种的第一格削成 1 单位 -> 每种都成 [1, 满] 的一高一低。
    auto dp11 = MakeRemoveParam(bag, 0, kStack11, max11 - 1);
    EXPECT_EQ(kSuccess, bag.RemoveItemByPos(dp11));
    auto dp10 = MakeRemoveParam(bag, 2, kStack10, max10 - 1);
    EXPECT_EQ(kSuccess, bag.RemoveItemByPos(dp10));
    auto dp9 = MakeRemoveParam(bag, 4, kStack9, max9 - 1);
    EXPECT_EQ(kSuccess, bag.RemoveItemByPos(dp9));

    bag.MergeAndCompact();

    // 每种合并成 [满, 1],仍占 2 格,共 6 格。
    EXPECT_EQ(6, bag.OccupiedGridCount());
    EXPECT_EQ(6, bag.GridSlotCount());

    // 期望布局:config 升序聚拢、组内满堆在前。
    EXPECT_EQ(kStack9, bag.GetItemCompByPos(0)->config_id());
    EXPECT_EQ(max9, bag.GetItemCompByPos(0)->size());
    EXPECT_EQ(kStack9, bag.GetItemCompByPos(1)->config_id());
    EXPECT_EQ(1, bag.GetItemCompByPos(1)->size());

    EXPECT_EQ(kStack10, bag.GetItemCompByPos(2)->config_id());
    EXPECT_EQ(max10, bag.GetItemCompByPos(2)->size());
    EXPECT_EQ(kStack10, bag.GetItemCompByPos(3)->config_id());
    EXPECT_EQ(1, bag.GetItemCompByPos(3)->size());

    EXPECT_EQ(kStack11, bag.GetItemCompByPos(4)->config_id());
    EXPECT_EQ(max11, bag.GetItemCompByPos(4)->size());
    EXPECT_EQ(kStack11, bag.GetItemCompByPos(5)->config_id());
    EXPECT_EQ(1, bag.GetItemCompByPos(5)->size());

    // 数量守恒:每种 = 满堆 + 1。
    EXPECT_EQ(max9 + 1, bag.GetTotalItemCount(kStack9));
    EXPECT_EQ(max10 + 1, bag.GetTotalItemCount(kStack10));
    EXPECT_EQ(max11 + 1, bag.GetTotalItemCount(kStack11));

    // 幂等:已最优,再整理一次应是无操作(早退)。
    std::vector<Guid> beforeGuid(6);
    for (uint32_t i = 0; i < 6; ++i)
        beforeGuid[i] = bag.GetItemCompByPos(i)->item_id();
    bag.MergeAndCompact();
    for (uint32_t i = 0; i < 6; ++i)
        EXPECT_EQ(beforeGuid[i], bag.GetItemCompByPos(i)->item_id());
}

TEST(BagTest, Neaten400)
{
    Bag bag;
    constexpr std::size_t kSlots = 400;
    constexpr std::size_t kHalf = kSlots / 2;
    bag.ExpandCapacity(kSlots);

    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, MaxStack(kStack10) * (uint32_t)kHalf)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11, MaxStack(kStack11) * (uint32_t)kHalf)));

    FillAndReduceToOne(bag, kStack10, 0, (uint32_t)kHalf);
    FillAndReduceToOne(bag, kStack11, (uint32_t)kHalf, (uint32_t)kHalf);

    for (uint32_t i = 0; i < (uint32_t)bag.GridSlotCount(); ++i)
        EXPECT_EQ(1, bag.GetItemCompByPos(i)->size());

    bag.MergeAndCompact();

    // kHalf units of each config consolidate into 1 slot each (size = kHalf)
    EXPECT_EQ(2, bag.OccupiedGridCount());
    EXPECT_EQ(2, bag.GridSlotCount());
    for (auto &[pos, guid] : bag.GridSlots())
        EXPECT_EQ(kHalf, (std::size_t)bag.GetItemCompByPos(bag.GetItemPosByGuid(guid))->size());
    EXPECT_EQ(kHalf, bag.GetTotalItemCount(kStack10));
    EXPECT_EQ(kHalf, bag.GetTotalItemCount(kStack11));
}

TEST(BagTest, Neaten400_1)
{
    Bag bag;
    constexpr std::size_t kSlots = 400;
    constexpr std::size_t kHalf = kSlots / 2;
    constexpr std::size_t kQuarter = kSlots / 4;
    constexpr std::size_t kMaxStack = 999;
    bag.ExpandCapacity(kSlots);

    // Fill 200 slots of each item type
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, MaxStack(kStack10) * (uint32_t)kHalf)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11, MaxStack(kStack11) * (uint32_t)kHalf)));

    // Reduce only the first quarter of each item to 1 unit
    FillAndReduceToOne(bag, kStack10, 0, (uint32_t)kQuarter);
    FillAndReduceToOne(bag, kStack11, (uint32_t)kHalf, (uint32_t)kQuarter);

    // Layout: [0..99]=1, [100..199]=999, [200..299]=1, [300..399]=999
    for (uint32_t i = 0; i < (uint32_t)bag.GridSlotCount(); ++i)
    {
        if (i < kQuarter)
            EXPECT_EQ(1, bag.GetItemCompByPos(i)->size());
        else if (i < kHalf)
            EXPECT_EQ(kMaxStack, bag.GetItemCompByPos(i)->size());
        else if (i < kHalf + kQuarter)
            EXPECT_EQ(1, bag.GetItemCompByPos(i)->size());
        else
            EXPECT_EQ(kMaxStack, bag.GetItemCompByPos(i)->size());
    }

    bag.MergeAndCompact();

    // 200 full stacks (999) + 2 consolidated partial stacks (kQuarter units each)
    EXPECT_EQ(kHalf + 2, bag.OccupiedGridCount());
    EXPECT_EQ(kHalf + 2, bag.GridSlotCount());

    UInt32Set pos999, posPartial;
    for (uint32_t i = 0; i < (uint32_t)bag.GridSlotCount(); ++i)
    {
        auto sz = bag.GetItemCompByPos(i)->size();
        if (sz == kMaxStack)
            pos999.emplace(i);
        else if (sz == kQuarter)
            posPartial.emplace(i);
    }
    EXPECT_EQ(kHalf, pos999.size());
    EXPECT_EQ(2, posPartial.size());
    EXPECT_EQ(kHalf / 2 * kMaxStack + kQuarter, bag.GetTotalItemCount(kStack10));
    EXPECT_EQ(kHalf / 2 * kMaxStack + kQuarter, bag.GetTotalItemCount(kStack11));
}

TEST(BagTest, NeatenCanNotStack)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);

    // First half: stackable, second half: non-stackable
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, MaxStack(kStack10) * kDefaultCapacity)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1, MaxStack(kNonStack1) * kDefaultCapacity)));

    // Reduce stackable slots to 1 unit each
    FillAndReduceToOne(bag, kStack10, 0, (uint32_t)kDefaultCapacity);

    for (uint32_t i = 0; i < (uint32_t)bag.GridSlotCount(); ++i)
        EXPECT_EQ(1, bag.GetItemCompByPos(i)->size());

    bag.MergeAndCompact();

    // 1 consolidated stack + kDefaultCapacity non-stackable slots
    EXPECT_EQ(kDefaultCapacity + 1, bag.OccupiedGridCount());
    EXPECT_EQ(kDefaultCapacity + 1, bag.GridSlotCount());
    for (auto &[pos, guid] : bag.GridSlots())
    {
        auto sz = (std::size_t)bag.GetItemCompByPos(bag.GetItemPosByGuid(guid))->size();
        if (sz != kDefaultCapacity)
            EXPECT_EQ(1, sz);
    }
    EXPECT_EQ(kDefaultCapacity, bag.GetTotalItemCount(kStack10));
    EXPECT_EQ(kDefaultCapacity, bag.GetTotalItemCount(kNonStack1));
}

// ---------------------------------------------------------------------------
// BlockItem / UnblockItem tests (via BagService + PlayerItemBlockList)
// ---------------------------------------------------------------------------

TEST(BagTest, BlockItemPreventsAdd)
{
    Bag bag;
    bag.ExpandCapacity(kBagMaxCapacity);
    PlayerItemBlockList blockList;

    // Block the non-stackable item config
    blockList.Block(kNonStack1);
    EXPECT_TRUE(blockList.IsBlocked(kNonStack1));

    // BagService::AddItem should fail for blocked config
    EXPECT_NE(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kNonStack1)));
    EXPECT_EQ(0, bag.OccupiedGridCount());

    // Other items unaffected
    EXPECT_EQ(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kStack10, 1)));
    EXPECT_EQ(1, bag.OccupiedGridCount());
}

TEST(BagTest, UnblockItemAllowsAdd)
{
    Bag bag;
    bag.ExpandCapacity(kBagMaxCapacity);
    PlayerItemBlockList blockList;

    blockList.Block(kNonStack1);
    EXPECT_NE(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kNonStack1)));

    blockList.Unblock(kNonStack1);
    EXPECT_FALSE(blockList.IsBlocked(kNonStack1));
    EXPECT_EQ(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kNonStack1)));
    EXPECT_EQ(1, bag.OccupiedGridCount());
}

TEST(BagTest, BlockItemIdempotent)
{
    PlayerItemBlockList blockList;
    blockList.Block(kNonStack1);
    blockList.Block(kNonStack1); // duplicate — should not crash
    EXPECT_TRUE(blockList.IsBlocked(kNonStack1));

    blockList.Unblock(kNonStack1);
    EXPECT_FALSE(blockList.IsBlocked(kNonStack1));
}

TEST(BagTest, BlockedItemsQuery)
{
    PlayerItemBlockList blockList;
    EXPECT_TRUE(blockList.All().empty());

    blockList.Block(kNonStack1);
    blockList.Block(kStack10);
    EXPECT_EQ(2, blockList.All().size());
    EXPECT_TRUE(blockList.All().contains(kNonStack1));
    EXPECT_TRUE(blockList.All().contains(kStack10));
}

// ===========================================================================
// GainBlockService — server-wide (global) block tests
// ===========================================================================

TEST(GainBlockServiceTest, GlobalItemBlockPreventsAdd)
{
    GainBlockService::ClearAllGlobalBlocks();
    Bag bag;
    bag.ExpandCapacity(kBagMaxCapacity);
    PlayerItemBlockList blockList;

    // Before global block, item can be added
    EXPECT_EQ(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kNonStack1)));
    EXPECT_EQ(1, bag.OccupiedGridCount());

    // Global block on kStack10
    GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, kStack10);
    EXPECT_TRUE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, kStack10));

    // AddItem should fail for globally blocked item
    EXPECT_NE(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kStack10, 1)));
    EXPECT_EQ(1, bag.OccupiedGridCount()); // unchanged

    // Non-blocked items still work
    EXPECT_EQ(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kNonStack2)));
    EXPECT_EQ(2, bag.OccupiedGridCount());

    GainBlockService::ClearAllGlobalBlocks();
}

TEST(GainBlockServiceTest, GlobalUnblockAllowsAdd)
{
    GainBlockService::ClearAllGlobalBlocks();
    Bag bag;
    bag.ExpandCapacity(kBagMaxCapacity);
    PlayerItemBlockList blockList;

    GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, kNonStack1);
    EXPECT_NE(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kNonStack1)));

    GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, kNonStack1);
    EXPECT_FALSE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, kNonStack1));
    EXPECT_EQ(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kNonStack1)));

    GainBlockService::ClearAllGlobalBlocks();
}

TEST(GainBlockServiceTest, GlobalBlockIdempotent)
{
    GainBlockService::ClearAllGlobalBlocks();

    GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, kNonStack1);
    GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, kNonStack1);
    EXPECT_TRUE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, kNonStack1));
    EXPECT_EQ(1, GainBlockService::GlobalBlockedIds(GainBlockService::GainType::kItem).size());

    GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, kNonStack1);
    EXPECT_FALSE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, kNonStack1));

    GainBlockService::ClearAllGlobalBlocks();
}

TEST(GainBlockServiceTest, ClearAllGlobalBlocks)
{
    GainBlockService::ClearAllGlobalBlocks();

    GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, kNonStack1);
    GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, kStack10);
    GainBlockService::BlockGlobal(GainBlockService::GainType::kCurrency, 0);
    GainBlockService::BlockGlobal(GainBlockService::GainType::kCurrency, 1);

    EXPECT_EQ(2, GainBlockService::GlobalBlockedIds(GainBlockService::GainType::kItem).size());
    EXPECT_EQ(2, GainBlockService::GlobalBlockedIds(GainBlockService::GainType::kCurrency).size());

    GainBlockService::ClearAllGlobalBlocks();

    EXPECT_TRUE(GainBlockService::GlobalBlockedIds(GainBlockService::GainType::kItem).empty());
    EXPECT_TRUE(GainBlockService::GlobalBlockedIds(GainBlockService::GainType::kCurrency).empty());
}

TEST(GainBlockServiceTest, GlobalCurrencyBlockCheck)
{
    GainBlockService::ClearAllGlobalBlocks();

    EXPECT_FALSE(GainBlockService::IsGainBlocked(GainBlockService::GainType::kCurrency, 0));

    GainBlockService::BlockGlobal(GainBlockService::GainType::kCurrency, 0);
    EXPECT_TRUE(GainBlockService::IsGainBlocked(GainBlockService::GainType::kCurrency, 0));
    EXPECT_FALSE(GainBlockService::IsGainBlocked(GainBlockService::GainType::kCurrency, 1));

    GainBlockService::ClearAllGlobalBlocks();
}

TEST(GainBlockServiceTest, GlobalAndPerPlayerBlockCombo)
{
    GainBlockService::ClearAllGlobalBlocks();
    Bag bag;
    bag.ExpandCapacity(kBagMaxCapacity);
    PlayerItemBlockList blockList;

    // Per-player block on kNonStack1, global block on kStack10
    blockList.Block(kNonStack1);
    GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, kStack10);

    EXPECT_NE(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kNonStack1)));  // per-player blocked
    EXPECT_NE(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kStack10, 1))); // globally blocked
    EXPECT_EQ(kSuccess, BagService::AddItem(entt::null, bag, blockList, MakeItem(kNonStack2)));  // neither blocked
    EXPECT_EQ(1, bag.OccupiedGridCount());

    GainBlockService::ClearAllGlobalBlocks();
}

int main(int argc, char **argv)
{
    if (!test_config::FindAndLoadTestConfig(argc, argv))
        return 1;
    // 生产线程由 EtcdService 在节点身份分配成功后初始化发号器；单测没有该启动链，
    // 必须显式给当前测试线程一个非零节点号，不能依赖未初始化时的保留 node_id=0。
    tlsSnowflakeManager.OnNodeStart(1);
    ItemTableManager::Instance().Load();
    testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}

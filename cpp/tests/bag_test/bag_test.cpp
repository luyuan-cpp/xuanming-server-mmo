#include <gtest/gtest.h>

#include "engine/core/type_define/type_define.h"
#include "engine/thread_context/snow_flake_manager.h"

#include "table/code/item_table.h"
#include "table/code/equipslot_table.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
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

// ---------------------------------------------------------------------------
// 实例层 / 布局层拆分回归
//
// 下面两组用例钉住的是**那条缝本身**,不是某个玩法:
//   * GridLayoutTest    —— 直接构造一个 GridLayout。目前没有任何背包用它,
//     测试就是它的使用者;它证明"加一种布局策略,ItemStore 与 Bag 一行都不用改"。
//   * BagLayoutSwapTest —— 给一个空 Bag 换上格子布局,证明 Bag 这座桥真的只
//     隔着 IContainerLayout 接口跟布局层说话,没有偷偷依赖扁平语义。
//
// 详见 docs/design/bag-instance-layout-split.md。
// ---------------------------------------------------------------------------

TEST(GridLayoutTest, PlacementCoversEveryCellNotJustTheAnchor)
{
    GridLayout grid(4, 3); // 4 列 x 3 行 = 12 格
    EXPECT_EQ(12u, grid.Capacity());
    EXPECT_EQ(12u, grid.FreeCells());

    // 一件 2x2 的东西 first-fit 落在左上角,锚点 = 0。
    const SlotId anchor = grid.Place(1001, Footprint{2, 2});
    EXPECT_EQ(0u, anchor);
    EXPECT_EQ(8u, grid.FreeCells()); // 12 - 4
    EXPECT_EQ(1u, grid.OccupiedSlotCount());

    // 它盖住的每一格都能反查到它,不只是锚点 —— 点在一件 2x2 装备的任意一角
    // 都该拿到同一件东西。扁平布局根本没有"一件东西盖住多格"这个概念。
    for (SlotId slot : {0u, 1u, 4u, 5u})
    {
        EXPECT_EQ(1001u, grid.At(slot)) << "slot " << slot;
    }
    EXPECT_EQ(kInvalidGuid, grid.At(2));
    EXPECT_EQ(0u, grid.SlotOf(1001));
}

TEST(GridLayoutTest, FreeCellCountIsNotAnAnswerToCanFit)
{
    // 这一条正是"扁平那句 `空格数 >= count` 在格子下必然失效"的判据 ——
    // 也是 IContainerLayout 必须把 CanFit 做成虚函数、而不是让桥层自己拿
    // FreeCells() 做减法的原因。
    GridLayout grid(3, 3); // 9 格

    // 沿对角线摆 3 个 1x1(锚点 0 / 4 / 8),剩 6 个空格,但没有任何一块连续的 2x2。
    EXPECT_TRUE(grid.PlaceAt(2001, 0, kSingleCell));
    EXPECT_TRUE(grid.PlaceAt(2002, 4, kSingleCell));
    EXPECT_TRUE(grid.PlaceAt(2003, 8, kSingleCell));
    EXPECT_EQ(6u, grid.FreeCells());

    EXPECT_FALSE(grid.CanFit(1, Footprint{2, 2})); // 空格够,却摆不进去
    EXPECT_TRUE(grid.CanFit(6, kSingleCell));      // 换成 1x1 就放得下
    EXPECT_FALSE(grid.CanFit(7, kSingleCell));
}

TEST(GridLayoutTest, RemoveFreesEveryCoveredCell)
{
    GridLayout grid(4, 2); // 8 格
    ASSERT_EQ(0u, grid.Place(3001, Footprint{2, 2}));
    EXPECT_EQ(4u, grid.FreeCells());

    grid.Remove(3001);
    EXPECT_EQ(8u, grid.FreeCells());
    EXPECT_EQ(0u, grid.OccupiedSlotCount());
    EXPECT_EQ(kInvalidSlot, grid.SlotOf(3001));
    EXPECT_EQ(kInvalidGuid, grid.At(0));
}

TEST(GridLayoutTest, PlaceAtRejectsOverlapAndOutOfBounds)
{
    GridLayout grid(4, 4);
    EXPECT_TRUE(grid.PlaceAt(4001, 5, Footprint{2, 2})); // (1,1) 起的 2x2 -> 槽位 5/6/9/10
    EXPECT_EQ(5u, grid.SlotOf(4001));

    EXPECT_FALSE(grid.PlaceAt(4002, 0, Footprint{2, 2})); // (0,0) 起的 2x2 与上面在槽位 5 重叠
    EXPECT_EQ(kInvalidSlot, grid.SlotOf(4002));

    EXPECT_FALSE(grid.PlaceAt(4003, 3, Footprint{2, 1})); // x=3 再往右放 2 宽 -> 越右边界
    EXPECT_FALSE(grid.PlaceAt(4004, 999, kSingleCell));   // 槽位号本身越界
}

TEST(GridLayoutTest, ResizeGrowsByRowsAndRefusesToEvict)
{
    GridLayout grid(4, 2); // 8 格
    ASSERT_TRUE(grid.PlaceAt(5001, 4, kSingleCell)); // 第 2 行第 1 格
    EXPECT_EQ(8u, grid.Capacity());

    grid.Resize(12); // 长到 3 行
    EXPECT_EQ(12u, grid.Capacity());
    EXPECT_EQ(3u, grid.Height());
    EXPECT_EQ(5001u, grid.At(4)); // 原有物品原地不动

    grid.Resize(4); // 想砍回 1 行 —— 第 2 行还有东西,布局层无权挤掉它,必须拒绝
    EXPECT_EQ(12u, grid.Capacity());
    EXPECT_EQ(5001u, grid.At(4));

    grid.Remove(5001);
    grid.Resize(4); // 空了就允许
    EXPECT_EQ(4u, grid.Capacity());
}

TEST(BagLayoutSwapTest, SetLayoutRefusedWhileBagHoldsItems)
{
    Bag bag;
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    const std::size_t capacityBefore = bag.Capacity();

    bag.SetLayout(std::make_unique<GridLayout>(4, 4));

    // 拒绝:槽位号在两种策略下含义不同(扁平是下标、格子是 y*width+x),
    // 带着物品换布局等于让所有已持久化的 pos 突然改变意义。
    EXPECT_EQ(capacityBefore, bag.Capacity());
    EXPECT_EQ(1u, bag.OccupiedGridCount());
}

TEST(BagLayoutSwapTest, GridBackedBagStillMergesPartialStacks)
{
    // 换布局不影响实例层:堆叠合并照常工作。
    Bag bag;
    bag.SetLayout(std::make_unique<GridLayout>(4, 4));
    ASSERT_EQ(16u, bag.Capacity());

    const auto maxStack = MaxStack(kStack10);
    ASSERT_GE(maxStack, 2u);

    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, maxStack * 2)));
    EXPECT_EQ(2u, bag.OccupiedGridCount());
    FillAndReduceToOne(bag, kStack10, 0, 2); // 削成一高一低两个未满堆

    EXPECT_TRUE(bag.MergeAndCompact());

    EXPECT_EQ(1u, bag.OccupiedGridCount()); // 两个零散堆并成一个
    EXPECT_EQ(1u, bag.GridSlotCount());
    EXPECT_EQ(2u, bag.GetTotalItemCount(kStack10));
}

TEST(BagLayoutSwapTest, GridBackedBagDoesNotReorderOnMergeAndCompact)
{
    // 换布局改变的是布局行为:格子背包 SupportsCompaction()==false,
    // 整理只合并堆叠、绝不挪位置 —— 玩家手摆的格子布局就是布局本身。
    Bag bag;
    bag.SetLayout(std::make_unique<GridLayout>(4, 4));

    // 按 config 降序铺:11 在锚点 0,10 在锚点 1。同样的排布在扁平布局下
    // 一定会被重排成升序(见 MergeAndCompactGroupsAndSortsByConfigAscending)。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(1));
    ASSERT_EQ(kStack11, bag.GetItemCompByPos(0)->config_id());
    ASSERT_EQ(kStack10, bag.GetItemCompByPos(1)->config_id());

    // 没有可合并的零散堆 + 布局不支持重排 -> 早退,整理是无操作。
    EXPECT_FALSE(bag.MergeAndCompact());

    EXPECT_EQ(kStack11, bag.GetItemCompByPos(0)->config_id());
    EXPECT_EQ(kStack10, bag.GetItemCompByPos(1)->config_id());
}

// ---------------------------------------------------------------------------
// 遗留缺陷修复的回归用例
//
// 下面每一条都钉住一个**拆分前就存在、拆分时刻意没动、现在修掉**的缺陷。
// 对照 docs/design/bag-instance-layout-split.md §11。
// ---------------------------------------------------------------------------

// §11(a):快照里的 pos 不可用时,拆分前是一句无校验的 posToGuid[pos] = guid,
// 越界的会落进一个永远扫不到的幽灵槽位。现在布局层 fail-closed,桥层退化为自动选位。
TEST(BagRestoreTest, OutOfRangeSnapshotPosIsRelocatedNotLost)
{
    Bag bag;
    bag.SetCapacityForRestore(5);
    bag.InsertItemForRestore(/*guid=*/9001, /*configId=*/kStack10, /*stackSize=*/3, /*pos=*/99);

    // 物品还在,而且**两层对得上**——拆分前它会既查不到又删不掉。
    EXPECT_EQ(1u, bag.OccupiedGridCount());
    EXPECT_EQ(1u, bag.GridSlotCount());

    const uint32_t slot = bag.GetItemPosByGuid(9001);
    ASSERT_NE(kInvalidU32Id, slot);
    EXPECT_LT(slot, 5u) << "重新安置后必须落在合法容量范围内";
    ASSERT_NE(nullptr, bag.GetItemCompByPos(slot));
    EXPECT_EQ(3u, bag.GetItemCompByPos(slot)->size());
}

// §11(a) 的另一半:两条快照记录撞同一个 pos。拆分前后写者直接覆盖,
// 先到的那件被顶成"在 items 里但没有槽位"的孤儿。
TEST(BagRestoreTest, CollidingSnapshotPosDoesNotOrphanTheFirstItem)
{
    Bag bag;
    bag.SetCapacityForRestore(5);
    bag.InsertItemForRestore(9101, kStack10, 1, 2);
    bag.InsertItemForRestore(9102, kStack10, 1, 2); // 撞位

    EXPECT_EQ(2u, bag.OccupiedGridCount());
    EXPECT_EQ(2u, bag.GridSlotCount());

    EXPECT_EQ(2u, bag.GetItemPosByGuid(9101)) << "先到的守住原位";
    const uint32_t second = bag.GetItemPosByGuid(9102);
    ASSERT_NE(kInvalidU32Id, second) << "后到的必须也有位置,不能变成孤儿";
    EXPECT_NE(2u, second);
}

// §11(a) 的边界:快照里的物品比容量还多。位置可以变,但实在放不下时宁可丢掉
// 并报错,也不能留下没有槽位的实例——那会让实例数与槽位数永久对不上。
TEST(BagRestoreTest, OverCapacitySnapshotDropsRatherThanBreakTheInvariant)
{
    Bag bag;
    bag.SetCapacityForRestore(2);
    bag.InsertItemForRestore(9201, kStack10, 1, 0);
    bag.InsertItemForRestore(9202, kStack10, 1, 1);
    bag.InsertItemForRestore(9203, kStack10, 1, 2); // 放不下

    EXPECT_EQ(2u, bag.OccupiedGridCount());
    EXPECT_EQ(2u, bag.GridSlotCount());
    EXPECT_EQ(nullptr, bag.GetItemCompByGuid(9203));
}

// §11(a) 在布局层这一侧的判据:PlaceAt 必须诚实拒绝,不能静默顶替。
TEST(FlatLayoutTest, PlaceAtRejectsOutOfRangeAndOccupiedSlots)
{
    FlatLayout flat(4); // 合法槽位 0..3

    EXPECT_TRUE(flat.PlaceAt(11, 3, kSingleCell));

    EXPECT_FALSE(flat.PlaceAt(12, 4, kSingleCell)) << "越界必须拒绝";
    EXPECT_EQ(kInvalidSlot, flat.SlotOf(12));

    EXPECT_FALSE(flat.PlaceAt(13, 3, kSingleCell)) << "已被别人占的槽位必须拒绝";
    EXPECT_EQ(kInvalidSlot, flat.SlotOf(13));

    EXPECT_EQ(11u, flat.At(3)) << "原主不能被顶掉";
    EXPECT_EQ(1u, flat.OccupiedSlotCount());
}

// §11(i):不可叠加物品被 RemoveItemByPos 扣到 0 之后,MergePartialStacks 永远
// 碰不到它(那里跳过 max_stack_size() <= 1),于是它永久占着一个格子。
TEST(BagTest, MergeAndCompactReclaimsDrainedNonStackableSlot)
{
    ASSERT_EQ(1u, MaxStack(kNonStack1)) << "用例前提:该 config 不可叠加";

    Bag bag;
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));
    EXPECT_EQ(1u, bag.OccupiedGridCount());

    auto dp = MakeRemoveParam(bag, 0, kNonStack1, 1);
    EXPECT_EQ(kSuccess, bag.RemoveItemByPos(dp));

    // 扣光后实例与槽位都还在——这是 RemoveItemByPos 的既有语义,没有改。
    EXPECT_EQ(1u, bag.OccupiedGridCount());
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    EXPECT_EQ(0u, bag.GetItemCompByPos(0)->size());

    std::vector<DestroyedInstance> destroyed;
    EXPECT_TRUE(bag.MergeAndCompact(&destroyed));

    EXPECT_EQ(0u, bag.OccupiedGridCount()) << "整理必须回收 size==0 的僵尸";
    EXPECT_EQ(0u, bag.GridSlotCount());
    ASSERT_EQ(1u, destroyed.size());
    EXPECT_EQ(kNonStack1, destroyed.front().configId);
    EXPECT_EQ(0u, destroyed.front().size);
}

// §11(g):整理会退役一批 item_uuid,而那是 transaction_log 做外挂回收关联的键。
// Bag 是纯容器不写流水,但必须**报告**退役了哪些,否则追溯链在整理这一步无声断掉。
TEST(BagTest, MergeAndCompactReportsRetiredInstances)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);
    const auto maxStack = MaxStack(kStack10);
    ASSERT_GE(maxStack, 2u);

    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, maxStack * 2)));
    FillAndReduceToOne(bag, kStack10, 0, 2); // [1,1] 两个未满堆

    std::vector<DestroyedInstance> destroyed;
    EXPECT_TRUE(bag.MergeAndCompact(&destroyed));

    EXPECT_EQ(1u, bag.OccupiedGridCount());
    EXPECT_EQ(2u, bag.GetTotalItemCount(kStack10)) << "数量守恒,只是实例少了一个";
    ASSERT_EQ(1u, destroyed.size());
    EXPECT_EQ(kStack10, destroyed.front().configId);
    EXPECT_NE(kInvalidGuid, destroyed.front().guid);
}

// 回执参数是可选的:老调用方(以及本文件里原有的十个 MergeAndCompact 用例)
// 不传也必须照常工作。
TEST(BagTest, MergeAndCompactWithoutReceiptStillWorks)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, MaxStack(kStack10) * 2)));
    FillAndReduceToOne(bag, kStack10, 0, 2);
    EXPECT_TRUE(bag.MergeAndCompact());
    EXPECT_EQ(1u, bag.OccupiedGridCount());
}

// §11(b):playerGuid 此前全仓没有写入点,所有 player= 日志打的都是哨兵。
TEST(BagTest, PlayerGuidIsSettableForLogging)
{
    Bag bag;
    EXPECT_EQ(kInvalidGuid, bag.PlayerGuid()) << "默认仍是哨兵";
    bag.SetPlayerGuid(123456789ULL);
    EXPECT_EQ(123456789ULL, bag.PlayerGuid());
}

// ---------------------------------------------------------------------------
// §11.2 两条"需要产品决策"的收口
//
// (d) 装备栏改用具名槽布局:不再被"整理"重排。槽位分类(哪件装备进哪个槽)
//     仍然等策划,但**危害本身**已经消掉了。
// (h) 自动整理 vs 还原位置:变成调用点必须表态的 CompactPolicy。
// ---------------------------------------------------------------------------

// §11.2(d):四个背包各自的布局与起始容量。拆分前这张表只是注释,
// 代码里四个包清一色 kDefaultCapacity(10 格)且全是扁平布局。
TEST(PlayerBagsCompTest, EachBagTypeGetsItsDocumentedLayoutAndCapacity)
{
    PlayerBagsComp comp;

    EXPECT_EQ(kBagMaxCapacity, comp.bags[kInventory].Capacity());
    EXPECT_EQ(kWarehouseMaxCapacity, comp.bags[kWarehouse].Capacity());
    EXPECT_EQ(kEquipmentCapacity, comp.bags[kEquipment].Capacity());
    EXPECT_EQ(kTempBagMaxCapacity, comp.bags[kTemporary].Capacity());

    // 只有装备栏不参与自动重排。
    EXPECT_TRUE(comp.bags[kInventory].Layout().SupportsCompaction());
    EXPECT_TRUE(comp.bags[kWarehouse].Layout().SupportsCompaction());
    EXPECT_FALSE(comp.bags[kEquipment].Layout().SupportsCompaction());
    EXPECT_TRUE(comp.bags[kTemporary].Layout().SupportsCompaction());
}

// §11.2(d):**部位与槽位口径全在配置数据里,代码零硬编。**
//
//   CfgItem.equip_kind   这件东西属于哪个部位(0 = 不是装备)
//   CfgEquipSlot         每个槽位接受哪个部位
//
// 下面用例里的示例数据(data/Item.xlsx 与 data/EquipSlot.xlsx):
//   item 1 / item 2 -> 部位 1
//   槽位 0 -> 部位 1、槽位 1 -> 部位 1、槽位 2 -> 部位 2
// **部位 1 有两个槽** —— 这就是"能带两只手镯"的形状。

// 头号用例:同一部位的两件装备可以同时穿戴,第三件才被拒。
// 上一版模型(equip_slot = 只能进第 N 号槽)根本表达不了这件事:
// 两件同部位的东西会抢同一个槽号,第二件必然被拒。
TEST(FixedSlotLayoutTest, TwoItemsOfTheSameKindOccupyTwoSlots)
{
    Bag bag;
    bag.SetLayout(std::make_unique<FixedSlotLayout>(kEquipmentCapacity));

    // 两件**同一部位**的装备(想象成两只手镯)。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));
    EXPECT_EQ(2u, bag.OccupiedGridCount()) << "同部位有两个槽,两件都该穿得上";

    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(1));
    EXPECT_EQ(kNonStack1, bag.GetItemCompByPos(0)->config_id());
    EXPECT_EQ(kNonStack1, bag.GetItemCompByPos(1)->config_id());

    // 第三件同部位的就没位置了 —— 槽位数由表决定,不是代码。
    EXPECT_NE(kSuccess, bag.AddItem(MakeItem(kNonStack1)));
    EXPECT_EQ(2u, bag.OccupiedGridCount());
    EXPECT_TRUE(bag.IsLayerConsistent());
}

// 不同 config、同一部位,一样各占一个槽。
TEST(FixedSlotLayoutTest, DifferentConfigsSharingAKindShareItsSlots)
{
    Bag bag;
    bag.SetLayout(std::make_unique<FixedSlotLayout>(kEquipmentCapacity));

    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack2)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));

    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(1));
    EXPECT_EQ(kNonStack2, bag.GetItemCompByPos(0)->config_id());
    EXPECT_EQ(kNonStack1, bag.GetItemCompByPos(1)->config_id());
    EXPECT_EQ(nullptr, bag.GetItemCompByPos(2)) << "部位 2 的槽没人穿,就该空着";
}

// 没在表里声明部位的东西(equip_kind = 0)压根不该进装备栏。
TEST(FixedSlotLayoutTest, RejectsItemsThatDeclareNoKind)
{
    Bag bag;
    bag.SetLayout(std::make_unique<FixedSlotLayout>(kEquipmentCapacity));

    EXPECT_NE(kSuccess, bag.AddItem(MakeItem(kStack10, 1))); // 普通物品,equip_kind = 0
    EXPECT_EQ(0u, bag.OccupiedGridCount()) << "拒绝要干净,不能留下半个实例";
    EXPECT_TRUE(bag.IsLayerConsistent());
}

// 同一件装备躺在**人物背包**(扁平布局)里时不受部位约束 —— 背包的下标
// 不表达任何语义,它就该 first-fit 落到 0 号位。
TEST(FixedSlotLayoutTest, FlatBagIgnoresEquipKind)
{
    Bag bag; // 默认 FlatLayout
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack2)));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    EXPECT_EQ(kNonStack2, bag.GetItemCompByPos(0)->config_id())
        << "背包里的装备仍是 first-fit,不该被塞进装备槽";
}

// §11.2(d) 的实际危害:装备栏被"整理"重排。
TEST(FixedSlotLayoutTest, NeverReordersWornGear)
{
    Bag bag;
    bag.SetLayout(std::make_unique<FixedSlotLayout>(kEquipmentCapacity));

    // config 降序穿:kNonStack2 先占 0 号槽,kNonStack1 占 1 号槽。
    // 同样的排布在扁平布局下一定会被整理成 config 升序。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack2)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));

    EXPECT_FALSE(bag.MergeAndCompact()) << "没得合并 + 不许重排 -> 早退";

    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(1));
    EXPECT_EQ(kNonStack2, bag.GetItemCompByPos(0)->config_id())
        << "具名槽是契约:整理绝不能把头盔挪到鞋子的位置";
    EXPECT_EQ(kNonStack1, bag.GetItemCompByPos(1)->config_id());
}

// 具名槽**刻意没有**把槽位数写死:一份 capacities[kEquipment]=20 的存量快照
// 撞上写死的 10,还原时会静默丢装备。这条用例拦住那个"优化"。
TEST(FixedSlotLayoutTest, CapacityStaysRestorableSoSnapshotsDoNotLoseGear)
{
    Bag bag;
    bag.SetLayout(std::make_unique<FixedSlotLayout>(kEquipmentCapacity));
    ASSERT_EQ(kEquipmentCapacity, bag.Capacity());

    bag.SetCapacityForRestore(20);
    EXPECT_EQ(20u, bag.Capacity()) << "槽位数必须能被快照还原,不能被代码写死";
    bag.SetCapacityForRestore(4);
    EXPECT_EQ(4u, bag.Capacity());
}

// 还原到装备栏时以**配置**为准,快照里的 pos 只是历史记录 ——
// 策划把某个部位的槽位改了之后,老存档还原就该落到新槽位。
TEST(FixedSlotLayoutTest, RestoreFollowsConfigNotTheSnapshotPos)
{
    Bag bag;
    bag.SetLayout(std::make_unique<FixedSlotLayout>(kEquipmentCapacity));

    // 快照说它在 7 号槽,但 kNonStack1 是部位 1,该部位的第一个空槽是 0。
    bag.InsertItemForRestore(/*guid=*/7101, /*configId=*/kNonStack1, /*stackSize=*/1, /*pos=*/7);

    EXPECT_EQ(0u, bag.GetItemPosByGuid(7101)) << "以配置为准,不是以快照为准";
    EXPECT_EQ(nullptr, bag.GetItemCompByPos(7));
    EXPECT_EQ(1u, bag.OccupiedGridCount());
    EXPECT_TRUE(bag.IsLayerConsistent());
}

// 还原路径与入包路径的**严格程度刻意不同**。
//
// 入包严格:往装备栏塞没声明部位的东西,拒。
// 还原宽容:配置里查不到部位 —— 表被裁过、这件是新版本装备、或者干脆是脏数据
// —— 把玩家的装备丢掉,远比"放错格子"更坏。原则仍是 §11(a):
// **位置可以变,物品不能丢**。这条 bug 是 cross_zone_test 抓到的。
TEST(FixedSlotLayoutTest, RestoreKeepsGearWhoseConfigDeclaresNoKind)
{
    Bag bag;
    bag.SetLayout(std::make_unique<FixedSlotLayout>(kEquipmentCapacity));

    constexpr uint32_t kUnknownConfig = 987654; // 表里根本没有这个 config
    bag.InsertItemForRestore(/*guid=*/7201, kUnknownConfig, /*stackSize=*/1, /*pos=*/5);

    EXPECT_EQ(1u, bag.OccupiedGridCount()) << "查不到配置也绝不能丢玩家的东西";
    EXPECT_EQ(5u, bag.GetItemPosByGuid(7201)) << "退回快照里的位置";
    EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST(CompactPolicyTest, MergeOnlyMergesWithoutTouchingPositions)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);
    const auto max10 = MaxStack(kStack10);
    ASSERT_GE(max10, 2u);

    // config 降序摆放:11 占 pos0,10 占 pos1/pos2;再把 10 削成两个未满堆。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, max10 * 2)));
    FillAndReduceToOne(bag, kStack10, 1, 2);
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    const Guid guidAtZero = bag.GetItemCompByPos(0)->item_id();

    std::vector<DestroyedInstance> destroyed;
    EXPECT_TRUE(bag.MergeAndCompact(&destroyed, CompactPolicy::kMergeOnly));

    // 合并确实发生了:两个零头并成一个,退役掉一个实例。
    ASSERT_EQ(1u, destroyed.size());
    EXPECT_EQ(2u, bag.GetTotalItemCount(kStack10));

    // 但 pos0 上那件东西一动没动 —— 位置不是整理的副作用。
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    EXPECT_EQ(guidAtZero, bag.GetItemCompByPos(0)->item_id());
    EXPECT_EQ(kStack11, bag.GetItemCompByPos(0)->config_id());
}

// §11.2(h):kMergeAndReorder —— 玩家显式点"整理"才重排。
TEST(CompactPolicyTest, MergeAndReorderSortsByConfigAscending)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    ASSERT_EQ(kStack11, bag.GetItemCompByPos(0)->config_id());

    EXPECT_TRUE(bag.MergeAndCompact(nullptr, CompactPolicy::kMergeAndReorder));

    EXPECT_EQ(kStack10, bag.GetItemCompByPos(0)->config_id()) << "显式整理才重排";
    EXPECT_EQ(kStack11, bag.GetItemCompByPos(1)->config_id());
}

// 同一份布局,两种 policy 给出两种结果 —— 这就是"取舍变成参数"的意思。
TEST(CompactPolicyTest, SameBagTwoPoliciesTwoOutcomes)
{
    const auto build = [](Bag &bag) {
        bag.ExpandCapacity(kDefaultCapacity);
        EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11)));
        EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    };

    Bag kept;
    build(kept);
    EXPECT_FALSE(kept.MergeAndCompact(nullptr, CompactPolicy::kMergeOnly))
        << "没得合并也没得回收 -> 早退,布局原封不动";
    EXPECT_EQ(kStack11, kept.GetItemCompByPos(0)->config_id());

    Bag sorted;
    build(sorted);
    EXPECT_TRUE(sorted.MergeAndCompact(nullptr, CompactPolicy::kMergeAndReorder));
    EXPECT_EQ(kStack10, sorted.GetItemCompByPos(0)->config_id());
}

// ---------------------------------------------------------------------------
// 补上变异分析暴露出的两条"没有用例保护"的修复
//
// 做完 §11 的修复之后,我把每条修复逐个 revert 回去看对应用例会不会变红。
// 结果发现 (e) 和 (f) **revert 掉之后所有用例仍然全绿** —— 它们是防御性路径,
// 在正常调用序列下不可达,于是没有任何用例能证明它们有效。
// 下面两组就是补这个洞的。
// ---------------------------------------------------------------------------

// §11(e) 的可测性:一个"说谎"的布局 —— CanFit 恒说放得下,Place 恒失败。
//
// 生产里不存在这种布局。它存在的意义是把那条防御路径变成**可触发**的,
// 而这恰恰是把布局抽成接口才换来的能力:拆分前那段逻辑焊死在 Bag 内部,
// 除非改产品代码,否则从外部根本没办法制造"容量判定说行、放置却失败"的局面。
class LyingLayout final : public FlatLayout
{
public:
    explicit LyingLayout(std::size_t capacity) : FlatLayout(capacity) {}

    bool CanFit(std::size_t, Footprint) const override { return true; }
    SlotId Place(Guid, Footprint) override { return kInvalidSlot; }
};

// §11(e):放置失败必须回滚实例,绝不能留下"进了仓库却没有槽位"的孤儿。
TEST(BagPlacementFailureTest, NonStackableRollsBackWhenLayoutRefuses)
{
    Bag bag;
    bag.SetLayout(std::make_unique<LyingLayout>(kDefaultCapacity));

    EXPECT_EQ(kBagAddItemBagFull, bag.AddItem(MakeItem(kNonStack1)));

    EXPECT_EQ(0u, bag.OccupiedGridCount()) << "失败的这一件必须被回滚掉";
    EXPECT_EQ(0u, bag.GridSlotCount());
    EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST(BagPlacementFailureTest, StackableSpillRollsBackWhenLayoutRefuses)
{
    Bag bag;
    bag.SetLayout(std::make_unique<LyingLayout>(kDefaultCapacity));

    EXPECT_EQ(kBagAddItemBagFull, bag.AddItem(MakeItem(kStack10, 1)));

    EXPECT_EQ(0u, bag.OccupiedGridCount());
    EXPECT_EQ(0u, bag.GridSlotCount());
    EXPECT_TRUE(bag.IsLayerConsistent());
}

// §11(f):跨层不变量。断言在 Release 下会被编译掉,所以谓词要能被用例直接验。
// 这条用例串起**每一个公开写入方法**,每步之后都验一次。
TEST(BagTest, LayerConsistencyHoldsAcrossEveryMutator)
{
    Bag bag;
    EXPECT_TRUE(bag.IsLayerConsistent()) << "空背包";

    bag.ExpandCapacity(kDefaultCapacity);
    EXPECT_TRUE(bag.IsLayerConsistent()) << "ExpandCapacity";

    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, MaxStack(kStack10) * 2)));
    EXPECT_TRUE(bag.IsLayerConsistent()) << "AddItem(可叠加,溢出到新实例)";

    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));
    EXPECT_TRUE(bag.IsLayerConsistent()) << "AddItem(不可叠加)";

    ItemCountMap toAdd{{kStack11, MaxStack(kStack11)}};
    EXPECT_EQ(kSuccess, bag.AddItems(toAdd));
    EXPECT_TRUE(bag.IsLayerConsistent()) << "AddItems(ItemCountMap)";

    ItemCountMap toRemove{{kStack10, 1}};
    EXPECT_EQ(kSuccess, bag.RemoveItems(toRemove));
    EXPECT_TRUE(bag.IsLayerConsistent()) << "RemoveItems";

    auto dp = MakeRemoveParam(bag, 0, kStack10, 1);
    EXPECT_EQ(kSuccess, bag.RemoveItemByPos(dp));
    EXPECT_TRUE(bag.IsLayerConsistent()) << "RemoveItemByPos";

    const Guid someGuid = bag.GetItemCompByPos(0)->item_id();
    EXPECT_EQ(kSuccess, bag.RemoveItem(someGuid));
    EXPECT_TRUE(bag.IsLayerConsistent()) << "RemoveItem";

    bag.MergeAndCompact();
    EXPECT_TRUE(bag.IsLayerConsistent()) << "MergeAndCompact";

    bag.ResetFromSnapshot();
    EXPECT_TRUE(bag.IsLayerConsistent()) << "ResetFromSnapshot";

    bag.SetCapacityForRestore(4);
    EXPECT_TRUE(bag.IsLayerConsistent()) << "SetCapacityForRestore";

    bag.InsertItemForRestore(8001, kStack10, 2, 99); // 越界 -> 重新安置
    EXPECT_TRUE(bag.IsLayerConsistent()) << "InsertItemForRestore(越界 pos)";

    bag.InsertItemForRestore(8002, kStack10, 2, 0);
    bag.InsertItemForRestore(8003, kStack10, 2, 0); // 撞位 -> 重新安置
    EXPECT_TRUE(bag.IsLayerConsistent()) << "InsertItemForRestore(撞位)";

    bag.InsertItemForRestore(8004, kStack10, 2, 1);
    bag.InsertItemForRestore(8005, kStack10, 2, 2); // 第 5 件,容量只有 4 -> 丢弃
    EXPECT_TRUE(bag.IsLayerConsistent()) << "InsertItemForRestore(超容量丢弃)";
    EXPECT_EQ(4u, bag.OccupiedGridCount());
}

// §11(f) 的**负向**用例。
//
// 变异测试抓到的洞:原来只有 `EXPECT_TRUE(bag.IsLayerConsistent())` 这种正向断言,
// 把谓词改成 `return true;` 之后全套用例照样绿 —— 那是同义反复,只能证明"没报错",
// 证明不了"报得出错"。必须有一条用例让它返回 false。
//
// SetCapacityForRestore 是唯一不做容量校验、也不带 AssertLayerConsistency 的入口
// (marshal 在 ResetFromSnapshot 之后、插入物品之前调它,那一刻背包本来就是空的),
// 所以它是从外部制造"容量 < 已占槽位"的唯一途径。
TEST(BagTest, LayerConsistencyPredicateActuallyDetectsBreakage)
{
    Bag bag;
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1)));
    ASSERT_TRUE(bag.IsLayerConsistent());
    ASSERT_EQ(1u, bag.GridSlotCount());

    bag.SetCapacityForRestore(0); // 容量 0,却还占着 1 个槽位

    EXPECT_FALSE(bag.IsLayerConsistent())
        << "谓词必须真的发现得了不一致,否则 AssertLayerConsistency 只是个摆设";
}

// 产品决策钉桩:新建角色的人物背包 = 100 格(2026-08-27 用户拍板)。
//
// 上面那条用例断言的是 `kBagMaxCapacity` 这个**常量**,它只能证明"构造函数用了
// 那个常量",证明不了"那个常量还是当初拍板的数"。有人把 kBagMaxCapacity 从 100
// 改成别的,上面那条照样绿。所以这里单独钉死字面量。
//
// 拆分前这个数其实是 10 —— player_bags_comp.h 里那张 100/200/10/200 的容量表
// 一直只是注释,没有任何代码兑现它,四个包清一色 kDefaultCapacity。
// 详见 docs/design/bag-instance-layout-split.md §11.2(d)。
//
// 要改这个数,是一次 gameplay 变更:改常量 + 改这条用例 + 知会策划。
TEST(PlayerBagsCompTest, NewCharacterInventoryIsOneHundredSlots)
{
    EXPECT_EQ(100u, kBagMaxCapacity) << "新角色人物背包格数是拍过板的,不是随手可调的常量";

    PlayerBagsComp comp;
    EXPECT_EQ(100u, comp.bags[kInventory].Capacity());
}

// ---------------------------------------------------------------------------
// §13:批量入包的事务性(此前是"注释比代码强",现在把话兑现)
//
// 拆分前 AddItems 只有 CheckSpaceFor 一道预检 —— 它挡容量和配置表,挡不住
// 发号器被 fence、也挡不住预设 guid 撞车。于是"第一件纯并堆成功、第二件要
// 铸号却失败"就留下半批。邮件附件是这条路径的主要用户,半批发放会让调用方
// 以为整批失败而重发,变成复制道具。
// ---------------------------------------------------------------------------

// 预设 guid 撞背包里已有的 -> 整批拒绝,一件都不许写进去。
TEST(BagBatchAtomicityTest, PreassignedGuidCollidingWithExistingItemRejectsWholeBatch)
{
    Bag bag;
    bag.ExpandCapacity(kBagMaxCapacity);

    std::vector<Guid> written;
    ASSERT_EQ(kSuccess, bag.AddItem(MakeItem(kNonStack1), &written));
    ASSERT_EQ(1u, written.size());
    const Guid existing = written.front();
    ASSERT_EQ(1u, bag.OccupiedGridCount());

    // 第一件全新、第二件的 guid 撞已有的那件。
    InitItemParam fresh = MakeItem(kNonStack2, 1);
    fresh.itemPBComp.set_item_id(778899);
    InitItemParam clash = MakeItem(kNonStack1, 1);
    clash.itemPBComp.set_item_id(existing);

    EXPECT_NE(kSuccess, bag.AddItems(std::vector<InitItemParam>{fresh, clash}));

    // 关键:第一件也不能进去。
    EXPECT_EQ(1u, bag.OccupiedGridCount()) << "撞车必须整批拒绝,不能留下半批";
    EXPECT_EQ(nullptr, bag.GetItemCompByGuid(778899));
    EXPECT_TRUE(bag.IsLayerConsistent());
}

// 同一批里两件用了同一个预设 guid -> 整批拒绝。
TEST(BagBatchAtomicityTest, DuplicateGuidWithinTheBatchRejectsWholeBatch)
{
    Bag bag;
    bag.ExpandCapacity(kBagMaxCapacity);

    InitItemParam a = MakeItem(kNonStack1, 1);
    a.itemPBComp.set_item_id(556677);
    InitItemParam b = MakeItem(kNonStack2, 1);
    b.itemPBComp.set_item_id(556677); // 同一个 guid

    EXPECT_NE(kSuccess, bag.AddItems(std::vector<InitItemParam>{a, b}));
    EXPECT_EQ(0u, bag.OccupiedGridCount());
    EXPECT_TRUE(bag.IsLayerConsistent());
}

// 全部携带各自合法的预设 guid(邮件附件的典型形状)-> 不需要铸号,正常放行。
// 这条用例同时钉住"别把铸号预检写成无条件拒绝"——那样会误伤这个场景。
TEST(BagBatchAtomicityTest, AllPreassignedGuidsNeedNoMintingAndSucceed)
{
    Bag bag;
    bag.ExpandCapacity(kBagMaxCapacity);

    InitItemParam a = MakeItem(kNonStack1, 1);
    a.itemPBComp.set_item_id(331);
    InitItemParam b = MakeItem(kNonStack2, 1);
    b.itemPBComp.set_item_id(332);

    EXPECT_EQ(kSuccess, bag.AddItems(std::vector<InitItemParam>{a, b}));
    EXPECT_EQ(2u, bag.OccupiedGridCount());
    ASSERT_NE(nullptr, bag.GetItemCompByGuid(331));
    ASSERT_NE(nullptr, bag.GetItemCompByGuid(332));
    EXPECT_TRUE(bag.IsLayerConsistent());
}

// §11.2(h) 的落点:玩家点击"整理"是唯一会重排位置的入口(2026-08-27 拍板)。
TEST(BagServiceSortTest, SortByPlayerRequestReorders)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);

    // config 降序摆放,扁平布局下"整理"应当把它排成升序。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    ASSERT_EQ(kStack11, bag.GetItemCompByPos(0)->config_id());

    bool changed = false;
    EXPECT_EQ(kSuccess, BagService::SortByPlayerRequest(entt::null, bag, &changed));
    EXPECT_TRUE(changed);

    EXPECT_EQ(kStack10, bag.GetItemCompByPos(0)->config_id());
    EXPECT_EQ(kStack11, bag.GetItemCompByPos(1)->config_id());
}

// 与之对照:自动触发的档位只合并、不挪位置。两条并排放,是为了让"点击才重排"
// 这个决定在用例层面一眼可见。
TEST(BagServiceSortTest, AutoTidyPathDoesNotReorder)
{
    Bag bag;
    bag.ExpandCapacity(kDefaultCapacity);
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack11)));
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10)));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));
    ASSERT_EQ(kStack11, bag.GetItemCompByPos(0)->config_id());

    EXPECT_EQ(kSuccess,
              BagService::MergeAndCompact(entt::null, bag, CompactPolicy::kMergeOnly));

    EXPECT_EQ(kStack11, bag.GetItemCompByPos(0)->config_id())
        << "自动路径不许挪位置:跨服回来东西得还在原地";
    EXPECT_EQ(kStack10, bag.GetItemCompByPos(1)->config_id());
}

// ⚠️ 本套件必须保持在**文件最后**:Fence() 是单向的,tlsSnowflakeManager 是进程级 tls 单例,
// fence 之后本线程再也铸不出合法 guid —— 在它后面声明的任何铸号型用例都会被连坐挂掉。
// (gtest 默认按声明序执行;请勿对本文件开 --gtest_shuffle。)
//
// 钉住的契约:发号器被 fence(失去 node_id 所有权)后,铸号型入包必须在**改动任何背包状态
// 之前**整体拒绝 —— 绝不能把 kInvalidGuid 哨兵当 item_id 写进背包持久化;
// 而不需要铸号的路径(纯并堆)不受影响。修复前:AddItem 会静默插入一件 guid=0 的物品。
TEST(BagFencedGeneratorTest, MintingPathsFailClosedWhileMergeStillWorks)
{
    // 堆叠上限从表读(kStack10 的 10 是 config_id,不是上限),所有数量按它推导,不硬编码。
    const uint32_t maxStack = MaxStack(kStack10);
    ASSERT_GE(maxStack, 4u) << "用例前提:该物品堆叠上限至少 4";

    Bag bag;
    bag.ExpandCapacity(kBagMaxCapacity);

    // 铺底:一堆差 3 满(留出"能并 2 还剩 1 空位"的余量)。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, maxStack - 3)));
    EXPECT_EQ(1, bag.OccupiedGridCount());

    tlsSnowflakeManager.Fence();

    // ① 纯并堆不铸号:fence 后必须照常成功(门的作用域只限"会铸号"的路径)。并 2,还剩 1 空位。
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, 2)));
    EXPECT_EQ(1, bag.OccupiedGridCount());

    // ② 并 1 + 溢 1 要铸号:必须整体失败,且旧堆**一点都不能动**——
    //    若先灌了旧堆再失败,就留下"满堆 + 丢 1 个"的半写脏状态。
    //    判据:失败后再并 1 个仍能成功 ⇒ 旧堆刚才没被灌,空位还在。
    EXPECT_EQ(kBagAddItemInvalidParam, bag.AddItem(MakeItem(kStack10, 2)));
    EXPECT_EQ(1, bag.OccupiedGridCount());
    EXPECT_EQ(kSuccess, bag.AddItem(MakeItem(kStack10, 1))); // 恰好装满旧堆
    EXPECT_EQ(1, bag.OccupiedGridCount());

    // ③ 不可堆叠必铸号:整体失败,零写入。
    EXPECT_EQ(kBagAddItemInvalidParam, bag.AddItem(MakeItem(kNonStack1, 1)));
    EXPECT_EQ(1, bag.OccupiedGridCount());
}

// 必须排在上面那条之后:Fence() 是单向的,本组用例依赖它已经被拉下。
//
// 钉住的契约:发号器不可用时,**批量入包整批拒绝**,一件都不许落地。
// 关键在于批次里混了"不铸号就能完成"的项和"必须铸号"的项 —— 逐件把关时,
// 前者会先成功写进去,等轮到后者才失败,于是留下半批。邮件附件是这条路径的
// 主要用户,半批发放会让调用方以为整批失败而重发,变成复制道具。
//
// 铺底用 InsertItemForRestore:它用调用方给的 guid,不铸号,所以 fence 之后
// 仍然可用 —— 否则 fence 之后根本没法把背包摆成需要的初始状态。
TEST(BagFencedGeneratorTest, VectorBatchRejectsWholeBatchWhenAnyPieceNeedsMinting)
{
    ASSERT_TRUE(tlsSnowflakeManager.IsFenced()) << "本用例依赖前一条用例已经 Fence()";

    const uint32_t maxStack10 = MaxStack(kStack10);
    ASSERT_GE(maxStack10, 4u) << "用例前提:该物品堆叠上限至少 4";

    Bag bag;
    bag.SetCapacityForRestore(kBagMaxCapacity);
    bag.InsertItemForRestore(910001, kStack10, maxStack10 - 2, 0);
    ASSERT_EQ(1u, bag.OccupiedGridCount());
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));

    // vector 重载按下标顺序处理,所以这里的"第一件能并堆、第二件要铸号"是
    // 确定性的:没有预检的话,第一件一定会先被写进旧堆。
    std::vector<InitItemParam> batch{MakeItem(kStack10, 2), MakeItem(kStack11, 1)};
    EXPECT_NE(kSuccess, bag.AddItems(batch));

    EXPECT_EQ(maxStack10 - 2, bag.GetItemCompByPos(0)->size())
        << "第一件不能被灌进旧堆 —— 整批拒绝意味着一件都没写";
    EXPECT_EQ(1u, bag.OccupiedGridCount());
    EXPECT_EQ(0u, bag.GetTotalItemCount(kStack11));
    EXPECT_TRUE(bag.IsLayerConsistent());
}

// ItemCountMap 重载的同一条契约。
//
// 注意它与上面那条的区别:ItemCountMap 是 unordered_map,**遍历顺序不确定**,
// 所以"没有预检时会不会留下半批"取决于哈希序 —— 那正是必须有整批预检的理由:
// 否则同一份数据在不同构建下表现不一样,是个 heisenbug。有了预检,下面的断言
// 才是确定成立的。
TEST(BagFencedGeneratorTest, CountMapBatchRejectsWholeBatchWhenMintingUnavailable)
{
    ASSERT_TRUE(tlsSnowflakeManager.IsFenced());

    const uint32_t maxStack10 = MaxStack(kStack10);
    ASSERT_GE(maxStack10, 4u);

    Bag bag;
    bag.SetCapacityForRestore(kBagMaxCapacity);
    bag.InsertItemForRestore(920001, kStack10, maxStack10 - 2, 0);
    ASSERT_NE(nullptr, bag.GetItemCompByPos(0));

    ItemCountMap batch{{kStack10, 2}, {kStack11, 1}};
    EXPECT_NE(kSuccess, bag.AddItems(batch));

    EXPECT_EQ(maxStack10 - 2, bag.GetItemCompByPos(0)->size());
    EXPECT_EQ(1u, bag.OccupiedGridCount());
    EXPECT_EQ(0u, bag.GetTotalItemCount(kStack11));
    EXPECT_TRUE(bag.IsLayerConsistent());
}

int main(int argc, char **argv)
{
    if (!test_config::FindAndLoadTestConfig(argc, argv))
        return 1;
    // 生产线程由 EtcdService 在节点身份分配成功后初始化发号器；单测没有该启动链，
    // 必须显式给当前测试线程一个非零节点号，不能依赖未初始化时的保留 node_id=0。
    tlsSnowflakeManager.OnNodeStart(1);
    ItemTableManager::Instance().Load();
    // 装备栏的槽位口径在这张表里(哪个槽接受哪个部位)。生产侧由 all_table.cpp
    // 自动注册加载;单测没有那条启动链,必须自己加载,否则 FindAll() 为空、
    // 任何装备都放不进具名槽。
    EquipSlotTableManager::Instance().Load();
    testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}

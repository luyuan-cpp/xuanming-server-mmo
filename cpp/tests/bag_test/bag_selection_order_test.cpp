// 背包选择顺序是客户端可见行为:同一份实例和布局不能因哈希历史或删除历史而选中不同物品。
// 每例独立装号段并恢复基线,不依赖其他测试文件的注册与执行顺序。
#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <string>
#include <tuple>
#include <unordered_map>
#include <utility>
#include <vector>

#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/item_store.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"

namespace
{
constexpr uint32_t kStackConfig = 10;
constexpr Guid kGuidA = (uint64_t{1} << 60) + 9501;
constexpr Guid kGuidB = (uint64_t{1} << 60) + 9502;
constexpr Guid kGuidC = (uint64_t{1} << 60) + 9503;
constexpr Guid kGuidD = (uint64_t{1} << 60) + 9504;

bool ArmItemSegment(uint64_t lo, uint64_t hi)
{
    auto &client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options;
    options.mixIds = false;
    options.kindName = "item";
    options.initialStep = 100000;
    if (!client.Enable(options, [](const std::string &, uint32_t) { return true; },
                       [](GuidSegmentClient::TimerKind, double, std::function<void()>) {},
                       [] { return 0.0; }))
    {
        return false;
    }
    client.Warm();
    client.OnResponse(0, lo, hi);
    return client.IsReady();
}

ItemComp MakeInstance(Guid guid, uint32_t size, uint64_t sequence)
{
    ItemComp item;
    item.set_item_id(guid);
    item.set_config_id(kStackConfig);
    item.set_size(size);
    item.set_acquire_seq(sequence);
    return item;
}

InitItemParam MakeGrant(uint32_t configId, uint32_t size)
{
    InitItemParam param;
    param.itemPBComp.set_config_id(configId);
    param.itemPBComp.set_size(size);
    return param;
}

GuidVector VisitedGuids(const Bag &bag)
{
    GuidVector guids;
    bag.ForEachItem([&guids](Guid guid, const ItemComp &) { guids.push_back(guid); });
    return guids;
}

uint32_t StackSize(Bag &bag, Guid guid)
{
    const auto *item = bag.GetItemCompByGuid(guid);
    EXPECT_NE(nullptr, item);
    return item != nullptr ? item->size() : 0;
}

Guid GuidAt(Bag &bag, uint32_t slot)
{
    const auto *item = bag.GetItemCompByPos(slot);
    EXPECT_NE(nullptr, item);
    return item != nullptr ? item->item_id() : kInvalidGuid;
}

using Snapshot = std::vector<std::tuple<Guid, uint32_t, uint32_t, uint32_t, uint64_t>>;

Snapshot Capture(const Bag &bag)
{
    Snapshot result;
    bag.ForEachItem([&](Guid guid, const ItemComp &item)
                    { result.emplace_back(guid, item.config_id(), item.size(),
                                          bag.GetItemPosByGuid(guid), item.acquire_seq()); });
    return result;
}

class BagSelectionOrderTest : public testing::Test
{
protected:
    void SetUp() override
    {
        ItemTableManager::Instance().Load();
        EquipSlotTableManager::Instance().Load();
        ASSERT_TRUE(ArmItemSegment(95000000, 96000000));
        BagService::SetItemInstanceInitializer({});
        GainBlockService::ClearAllGlobalBlocks();
        for (const uint32_t configId : {10u, 11u, 15u})
        {
            const auto *row = ItemTableManager::Instance().FindByIdSilent(configId).first;
            ASSERT_NE(nullptr, row) << configId;
            ASSERT_GE(row->max_stack_size(), 10u) << configId;
        }
        const auto *plain = ItemTableManager::Instance().FindByIdSilent(3).first;
        ASSERT_NE(nullptr, plain);
        ASSERT_EQ(1u, plain->max_stack_size());
    }

    void TearDown() override
    {
        BagService::SetItemInstanceInitializer({});
        GainBlockService::ClearAllGlobalBlocks();
        EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
    }

    static uint32_t MaxStack(uint32_t configId = kStackConfig)
    {
        return ItemTableManager::Instance().FindByIdSilent(configId).first->max_stack_size();
    }

    static void PlaceDrainFixture(Bag &bag)
    {
        // 槽位序、实例序、插入序互不相同,不能碰巧沿用容器遍历序。
        bag.InsertItemForRestore(kGuidA, kStackConfig, 5, 2, 1);
        bag.InsertItemForRestore(kGuidB, kStackConfig, 5, 0, 3);
        bag.InsertItemForRestore(kGuidC, kStackConfig, 5, 1, 2);
    }

    static void ExpectLowestSlotsDrained(Bag &bag, const std::vector<DrainedInstance> &drained)
    {
        ASSERT_EQ(2u, drained.size());
        EXPECT_EQ(kGuidB, drained[0].guid);
        EXPECT_EQ(kStackConfig, drained[0].configId);
        EXPECT_EQ(5u, drained[0].amount);
        EXPECT_EQ(kGuidC, drained[1].guid);
        EXPECT_EQ(kStackConfig, drained[1].configId);
        EXPECT_EQ(2u, drained[1].amount);
        EXPECT_EQ(5u, StackSize(bag, kGuidA));
        EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidB));
        EXPECT_EQ(kInvalidSlot, bag.GetItemPosByGuid(kGuidB));
        EXPECT_EQ(2u, bag.OccupiedGridCount());
        EXPECT_EQ(nullptr, bag.GetItemCompByPos(0));
        EXPECT_EQ(3u, StackSize(bag, kGuidC));
        EXPECT_EQ(1u, bag.GetItemPosByGuid(kGuidC));
        EXPECT_EQ(2u, bag.GetItemPosByGuid(kGuidA));
        EXPECT_TRUE(bag.IsLayerConsistent());
    }
};

TEST_F(BagSelectionOrderTest, ForEachVisitsInAcquireOrderThenGuid)
{
    Bag bag;
    bag.InsertItemForRestore(kGuidA, kStackConfig, 1, 0, 30);
    bag.InsertItemForRestore(kGuidB, kStackConfig, 1, 1, 10);
    bag.InsertItemForRestore(kGuidD, kStackConfig, 1, 2, 20);
    bag.InsertItemForRestore(kGuidC, kStackConfig, 1, 3, 20);
    EXPECT_EQ((GuidVector{kGuidB, kGuidC, kGuidD, kGuidA}), VisitedGuids(bag));

    // 删除再插入不会把无关实例的遍历顺序改成删除历史的函数。
    ASSERT_EQ(kSuccess, bag.RemoveItem(kGuidD));
    bag.InsertItemForRestore(kGuidD, kStackConfig, 1, 2, 20);
    EXPECT_EQ((GuidVector{kGuidB, kGuidC, kGuidD, kGuidA}), VisitedGuids(bag));
}

TEST_F(BagSelectionOrderTest, StackFillGoesToEarliestAcquiredPartialFirst)
{
    Bag bag;
    const uint32_t maxStack = MaxStack();
    bag.InsertItemForRestore(kGuidA, kStackConfig, maxStack - 2, 0, 30);
    bag.InsertItemForRestore(kGuidD, kStackConfig, maxStack - 2, 2, 10);
    bag.InsertItemForRestore(kGuidB, kStackConfig, maxStack - 2, 1, 20);
    std::vector<Guid> written;
    ASSERT_EQ(kSuccess, bag.AddItem(MakeGrant(kStackConfig, 2), &written));
    EXPECT_EQ((GuidVector{kGuidD}), written);
    EXPECT_EQ(maxStack, StackSize(bag, kGuidD));

    written.clear();
    ASSERT_EQ(kSuccess, bag.AddItem(MakeGrant(kStackConfig, 3), &written));
    EXPECT_EQ((GuidVector{kGuidB, kGuidA}), written);
    EXPECT_EQ(maxStack, StackSize(bag, kGuidB));
    EXPECT_EQ(maxStack - 1, StackSize(bag, kGuidA));
    EXPECT_EQ(3u, bag.OccupiedGridCount());
}

TEST_F(BagSelectionOrderTest, ClampedDrainTakesLowestSlotFirst)
{
    Bag bag;
    PlaceDrainFixture(bag);
    std::vector<DrainedInstance> drained;
    ASSERT_EQ(kSuccess, bag.RemoveItemsClamped({{kStackConfig, 7}}, &drained));
    ExpectLowestSlotsDrained(bag, drained);
}

TEST_F(BagSelectionOrderTest, ExactDrainTakesLowestSlotFirst)
{
    Bag bag;
    PlaceDrainFixture(bag);
    std::vector<DrainedInstance> drained;
    ASSERT_EQ(kSuccess, bag.RemoveItems({{kStackConfig, 7}}, &drained));
    ExpectLowestSlotsDrained(bag, drained);
}

TEST_F(BagSelectionOrderTest, DrainWithoutReceiptUsesTheSameSlotOrder)
{
    Bag bag;
    PlaceDrainFixture(bag);
    ASSERT_EQ(kSuccess, bag.RemoveItems({{kStackConfig, 7}}));
    EXPECT_EQ(5u, StackSize(bag, kGuidA));
    EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidB));
    EXPECT_EQ(kInvalidSlot, bag.GetItemPosByGuid(kGuidB));
    EXPECT_EQ(2u, bag.OccupiedGridCount());
    EXPECT_EQ(3u, StackSize(bag, kGuidC));
    EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST_F(BagSelectionOrderTest, DrainSkipsEmptyStackWithoutReceipt)
{
    Bag bag;
    bag.InsertItemForRestore(kGuidA, kStackConfig, 1, 0, 1);
    // 加载已拒绝零堆；这里专门注入内存损坏，验证抽取不把空堆写入回执。
    ASSERT_NE(nullptr, bag.GetItemCompByGuid(kGuidA));
    bag.GetItemCompByGuid(kGuidA)->set_size(0);
    bag.InsertItemForRestore(kGuidB, kStackConfig, 5, 1, 2);
    std::vector<DrainedInstance> drained;
    ASSERT_EQ(kSuccess, bag.RemoveItemsClamped({{kStackConfig, 3}}, &drained));
    ASSERT_EQ(1u, drained.size());
    EXPECT_EQ(kGuidB, drained[0].guid);
    EXPECT_EQ(3u, drained[0].amount);
    EXPECT_EQ(0u, StackSize(bag, kGuidA));
    EXPECT_EQ(2u, StackSize(bag, kGuidB));
}

TEST_F(BagSelectionOrderTest, MergeKeepsEarliestAcquiredStackAndRetiresLaterOnes)
{
    Bag bag;
    bag.InsertItemForRestore(kGuidB, kStackConfig, 4, 1, 1);
    bag.InsertItemForRestore(kGuidA, kStackConfig, 3, 0, 5);
    std::vector<DestroyedInstance> destroyed;
    ASSERT_TRUE(bag.MergeAndCompact(&destroyed, CompactPolicy::kMergeOnly));
    EXPECT_EQ(7u, StackSize(bag, kGuidB));
    EXPECT_EQ(1u, bag.GetItemPosByGuid(kGuidB));
    EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidA));
    ASSERT_EQ(1u, destroyed.size());
    EXPECT_EQ(kGuidA, destroyed[0].guid);
    EXPECT_EQ(kStackConfig, destroyed[0].configId);
    EXPECT_EQ(0u, destroyed[0].size);
    EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST_F(BagSelectionOrderTest, MergeNeverPicksAnEmptyStackAsSurvivor)
{
    Bag bag;
    bag.InsertItemForRestore(kGuidC, kStackConfig, 4, 2, 3);
    bag.InsertItemForRestore(kGuidB, kStackConfig, 3, 1, 2);
    bag.InsertItemForRestore(kGuidA, kStackConfig, 1, 0, 1);
    // 负向夹具绕过加载的零堆过滤，保留对整理幸存者选择与空堆回收的覆盖。
    ASSERT_NE(nullptr, bag.GetItemCompByGuid(kGuidA));
    bag.GetItemCompByGuid(kGuidA)->set_size(0);
    std::vector<DestroyedInstance> destroyed;
    ASSERT_TRUE(bag.MergeAndCompact(&destroyed, CompactPolicy::kMergeOnly));
    EXPECT_EQ(7u, StackSize(bag, kGuidB));
    EXPECT_EQ(1u, bag.GetItemPosByGuid(kGuidB));
    ASSERT_EQ(2u, destroyed.size());
    EXPECT_EQ(kGuidA, destroyed[0].guid);
    EXPECT_EQ(kGuidC, destroyed[1].guid);
    EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidA));
    EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidC));
}

TEST_F(BagSelectionOrderTest, ReorderBreaksTiesByAcquireOrder)
{
    Bag bag;
    bag.InsertItemForRestore(kGuidD, 11, MaxStack(11), 0, 4);
    bag.InsertItemForRestore(kGuidA, kStackConfig, MaxStack(), 1, 3);
    bag.InsertItemForRestore(kGuidC, kStackConfig, MaxStack(), 2, 1);
    bag.InsertItemForRestore(kGuidB, kStackConfig, MaxStack(), 3, 2);
    ASSERT_TRUE(bag.MergeAndCompact(nullptr, CompactPolicy::kMergeAndReorder));
    EXPECT_EQ(kGuidC, GuidAt(bag, 0));
    EXPECT_EQ(kGuidB, GuidAt(bag, 1));
    EXPECT_EQ(kGuidA, GuidAt(bag, 2));
    EXPECT_EQ(kGuidD, GuidAt(bag, 3));
}

TEST_F(BagSelectionOrderTest, AlreadyOrderedLayoutKeepsItsExistingTiePositions)
{
    Bag bag;
    bag.InsertItemForRestore(kGuidA, kStackConfig, MaxStack(), 0, 3);
    bag.InsertItemForRestore(kGuidB, kStackConfig, MaxStack(), 1, 1);
    EXPECT_FALSE(bag.MergeAndCompact(nullptr, CompactPolicy::kMergeAndReorder));
    EXPECT_EQ(kGuidA, GuidAt(bag, 0));
    EXPECT_EQ(kGuidB, GuidAt(bag, 1));
}

TEST_F(BagSelectionOrderTest, CountMapBatchPlacesConfigsInAscendingConfigOrder)
{
    ItemCountMap requested;
    requested.emplace(15, MaxStack(15));
    requested.emplace(11, MaxStack(11));
    requested.emplace(10, MaxStack(10));
    for (const bool throughService : {false, true})
    {
        SCOPED_TRACE(throughService);
        Bag bag;
        const auto result = throughService
            ? BagService::AddItems(entt::null, bag, requested)
            : bag.AddItems(requested);
        ASSERT_EQ(kSuccess, result);
        for (const auto &[slot, configId] : std::vector<std::pair<uint32_t, uint32_t>>{{0, 10}, {1, 11}, {2, 15}})
        {
            const auto *item = bag.GetItemCompByPos(slot);
            ASSERT_NE(nullptr, item);
            EXPECT_EQ(configId, item->config_id());
        }
    }
}

TEST_F(BagSelectionOrderTest, BatchDrainReportsConfigsBeforeSlotOrder)
{
    Bag bag;
    bag.InsertItemForRestore(kGuidA, 15, 5, 0, 1);
    bag.InsertItemForRestore(kGuidB, 11, 5, 1, 2);
    bag.InsertItemForRestore(kGuidC, 10, 5, 2, 3);
    std::vector<DrainedInstance> drained;
    ASSERT_EQ(kSuccess, bag.RemoveItemsClamped({{15, 1}, {11, 1}, {10, 1}}, &drained));
    ASSERT_EQ(3u, drained.size());
    EXPECT_EQ(kGuidC, drained[0].guid);
    EXPECT_EQ(kGuidB, drained[1].guid);
    EXPECT_EQ(kGuidA, drained[2].guid);
}

TEST_F(BagSelectionOrderTest, EvictionTieOnEqualSeqEvictsSmallerGuidFirst)
{
    Bag bag;
    bag.SetProfile(BagProfile::Temporary(2));
    bag.InsertItemForRestore(kGuidB, 3, 1, 0, 5);
    bag.InsertItemForRestore(kGuidA, 3, 1, 1, 5);
    std::vector<DestroyedInstance> evicted;
    ASSERT_EQ(kSuccess, bag.AddItem(MakeGrant(3, 1), nullptr, &evicted));
    ASSERT_EQ(1u, evicted.size());
    EXPECT_EQ(kGuidA, evicted[0].guid);
    EXPECT_NE(nullptr, bag.GetItemCompByGuid(kGuidB));
}

TEST_F(BagSelectionOrderTest, SnapshotReplayKeepsIterationOrder)
{
    Bag source;
    source.InsertItemForRestore(kGuidA, kStackConfig, 1, 0, 30);
    source.InsertItemForRestore(kGuidC, kStackConfig, 2, 2, 10);
    source.InsertItemForRestore(kGuidB, 11, 3, 1, 20);
    const Snapshot snapshot = Capture(source);
    Bag restored;
    for (const auto &[guid, config, size, position, sequence] : snapshot)
    {
        restored.InsertItemForRestore(guid, config, size, position, sequence);
    }
    EXPECT_EQ(snapshot, Capture(restored));
    EXPECT_EQ(3u, restored.OccupiedGridCount());
}

TEST(ItemStoreTest, MergeNeverPicksAnEmptyStackAsSurvivor)
{
    ItemTableManager::Instance().Load();
    ItemStore store;
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidA, 0, 1)));
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidB, 3, 2)));
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidC, 4, 3)));
    store.MergePartialStacks();
    EXPECT_EQ(7u, store.Find(kGuidB)->size());
    EXPECT_EQ(0u, store.Find(kGuidA)->size());
    EXPECT_EQ(0u, store.Find(kGuidC)->size());
    EXPECT_EQ((GuidVector{kGuidA, kGuidC}), store.CollectEmptyInstances());
}

TEST(ItemStoreTest, DuplicateInsertDoesNotAdvanceWatermark)
{
    ItemStore store;
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidA, 2, 7)));
    const auto watermark = store.NextAcquireSeq();
    EXPECT_EQ(nullptr, store.Insert(MakeInstance(kGuidA, 99, 1000)));
    EXPECT_EQ(watermark, store.NextAcquireSeq());
    EXPECT_EQ(nullptr, store.Insert(MakeInstance(kGuidA, 99, 0)));
    EXPECT_EQ(watermark, store.NextAcquireSeq());
    ASSERT_NE(nullptr, store.Find(kGuidA));
    EXPECT_EQ(2u, store.Find(kGuidA)->size());
    EXPECT_EQ(7u, store.Find(kGuidA)->acquire_seq());
}

TEST(ItemStoreTest, ApplyStackFillSkipsDestroyedGuid)
{
    ItemStore store;
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidA, 2, 1)));
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidB, 3, 2)));
    ASSERT_TRUE(store.Erase(kGuidB));
    std::vector<Guid> written;
    store.ApplyStackFill({{kGuidB, 3}}, &written);
    EXPECT_TRUE(written.empty());
    EXPECT_EQ(1u, store.Size());
    ASSERT_NE(nullptr, store.Find(kGuidA));
    EXPECT_EQ(2u, store.Find(kGuidA)->size());
}

TEST(ItemStoreTest, DrainRankIsReadOncePerNonemptyCandidate)
{
    ItemStore store;
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidA, 5, 1)));
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidB, 5, 3)));
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidC, 5, 2)));
    ASSERT_NE(nullptr, store.Insert(MakeInstance(kGuidD, 0, 4)));
    std::unordered_map<Guid, uint32_t> rankReads;
    const ItemStore::DrainRank byRank = [&](Guid guid)
    {
        ++rankReads[guid];
        return guid == kGuidB ? uint64_t{0} : uint64_t{1};
    };
    std::vector<ItemStore::StackDrain> drained;
    EXPECT_EQ(7u, store.DrainStacks(kStackConfig, 7, byRank, &drained));
    ASSERT_EQ(2u, drained.size());
    EXPECT_EQ(kGuidB, drained[0].guid);
    EXPECT_EQ(kGuidA, drained[1].guid);
    EXPECT_EQ(1u, rankReads[kGuidA]);
    EXPECT_EQ(1u, rankReads[kGuidB]);
    EXPECT_EQ(1u, rankReads[kGuidC]);
    EXPECT_EQ(0u, rankReads.count(kGuidD));
}

TEST(ItemStoreTest, ConfigOrderUsesUnsignedKeys)
{
    const ItemCountMap counts{{UINT32_MAX, 1}, {1, 2}, {uint32_t{1} << 31, 3}};
    EXPECT_EQ((std::vector<std::pair<uint32_t, uint32_t>>{{1, 2}, {uint32_t{1} << 31, 3}, {UINT32_MAX, 1}}),
              SortedByConfigId(counts));
}
} // 匿名命名空间

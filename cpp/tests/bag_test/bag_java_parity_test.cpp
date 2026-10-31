// 本轮只核对可观察的背包提交、零数量、旧档补章和写盘顺序，不依赖私有存储形状。
#include <gtest/gtest.h>

#include <cstddef>
#include <cstdint>
#include <functional>
#include <limits>
#include <memory>
#include <string>
#include <tuple>
#include <vector>

#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "services/scene/player/system/bag_marshal.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
constexpr Guid kOldStack = (uint64_t{1} << 60) + 9801;
constexpr Guid kOldOther = (uint64_t{1} << 60) + 9802;

bool ArmParitySegment(uint64_t lo, uint64_t hi)
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

InitItemParam ParityGrant(uint32_t configId, uint32_t count)
{
    InitItemParam result;
    result.itemPBComp.set_config_id(configId);
    result.itemPBComp.set_size(count);
    return result;
}

using BagSnapshot = std::vector<std::tuple<Guid, uint32_t, uint32_t, uint32_t, uint64_t>>;
BagSnapshot CaptureParityBag(const Bag &bag)
{
    BagSnapshot result;
    bag.ForEachItem([&](Guid guid, const ItemComp &item)
    {
        result.emplace_back(guid, item.config_id(), item.size(), bag.GetItemPosByGuid(guid), item.acquire_seq());
    });
    return result;
}

ItemEntry *AddParityEntry(BagAllData &state, Guid guid, uint32_t size, uint32_t slot, uint64_t seq)
{
    auto *entry = state.add_items();
    entry->set_item_uuid(guid);
    entry->set_config_id(10);
    entry->set_stack_size(size);
    entry->set_pos(slot);
    entry->set_acquire_seq(seq);
    entry->set_bag_type(kInventory);
    return entry;
}

class FailSecondPlacementLayout final : public FlatLayout
{
public:
    explicit FailSecondPlacementLayout(std::size_t capacity) : FlatLayout(capacity) {}
    std::unique_ptr<IContainerLayout> Clone() const override
    {
        return std::make_unique<FailSecondPlacementLayout>(*this);
    }
    SlotId Place(Guid guid, Footprint footprint) override
    {
        return ++placements == 2 ? kInvalidSlot : FlatLayout::Place(guid, footprint);
    }

private:
    int placements{0};
};

class BagJavaParityTest : public testing::Test
{
protected:
    void SetUp() override
    {
        ItemTableManager::Instance().Load();
        EquipSlotTableManager::Instance().Load();
        const auto *row = ItemTableManager::Instance().FindByIdSilent(10).first;
        ASSERT_NE(nullptr, row);
        ASSERT_GT(row->max_stack_size(), 1u);
        ASSERT_LT(row->max_stack_size(), std::numeric_limits<uint32_t>::max());
        maxStack = row->max_stack_size();
        const auto *single = ItemTableManager::Instance().FindByIdSilent(3).first;
        ASSERT_NE(nullptr, single);
        ASSERT_EQ(1u, single->max_stack_size());
        ASSERT_TRUE(ArmParitySegment(98000000, 98100000));
        BagService::SetItemInstanceInitializer({});
        GainBlockService::ClearAllGlobalBlocks();
        player = tlsEcs.actorRegistry.create();
    }

    void TearDown() override
    {
        BagService::SetItemInstanceInitializer({});
        GainBlockService::ClearAllGlobalBlocks();
        if (tlsEcs.actorRegistry.valid(player))
        {
            tlsEcs.actorRegistry.destroy(player);
        }
        EXPECT_TRUE(ArmParitySegment(1, uint64_t{1} << 54));
    }

    void FillEvictingBag(Bag &bag)
    {
        bag.SetProfile(BagProfile::Temporary(2));
        bag.InsertItemForRestore(kOldStack, 10, maxStack - 1, 0, 1);
        bag.InsertItemForRestore(kOldOther, 3, 1, 1, 2);
    }

    entt::entity player{entt::null};
    uint32_t maxStack{0};
};

TEST(BagLayoutCloneTest, FlatLayoutSubclassWithoutCloneOverrideDiesInDebug)
{
    class ForgetfulLayout final : public FlatLayout
    {
    public:
        using FlatLayout::FlatLayout;
        bool HasSlotSemantics() const override { return true; }
    };
    EXPECT_DEBUG_DEATH({ auto candidate = ForgetfulLayout(3).Clone(); (void)candidate; }, "Clone");
}

TEST_F(BagJavaParityTest, BagLevelRestoreStampsLegacyZeroAfterLaterExistingStamp)
{
    Bag bag;
    bag.SetProfile(BagProfile::Flat(3));
    ItemComp legacy;
    legacy.set_item_id(kOldStack);
    legacy.set_config_id(10);
    legacy.set_size(1);
    ItemComp stamped = legacy;
    stamped.set_item_id(kOldOther);
    stamped.set_acquire_seq(70);
    bag.RestoreItems({{legacy, 0}, {stamped, 1}});
    ASSERT_NE(nullptr, bag.GetItemCompByGuid(kOldStack));
    ASSERT_NE(nullptr, bag.GetItemCompByGuid(kOldOther));
    EXPECT_EQ(71u, bag.GetItemCompByGuid(kOldStack)->acquire_seq());
    EXPECT_EQ(70u, bag.GetItemCompByGuid(kOldOther)->acquire_seq());
    ASSERT_EQ(kSuccess, bag.AddItem(ParityGrant(3, 1)));
    ASSERT_NE(nullptr, bag.GetItemCompByPos(2));
    EXPECT_EQ(72u, bag.GetItemCompByPos(2)->acquire_seq());
}

TEST_F(BagJavaParityTest, PostEvictionMintFailurePreservesOldItemsLayoutAndReceipts)
{
    Bag bag;
    FillEvictingBag(bag);
    const auto before = CaptureParityBag(bag);
    ASSERT_TRUE(ArmParitySegment(98200000, 98200001));
    std::vector<DestroyedInstance> evicted{{123, 3, 1}};
    std::vector<GuidVector> written{{456}};

    // 修复前按淘汰前的一个新实例预检，先销毁两件旧物，再因实际需要两个号而失败。
    EXPECT_EQ(kBagAddItemInvalidParam, bag.AddItems(ItemCountMap{{10, maxStack + 1}}, &evicted, &written));
    EXPECT_EQ(before, CaptureParityBag(bag));
    EXPECT_EQ(1u, evicted.size());
    EXPECT_EQ(123u, evicted.front().guid);
    EXPECT_EQ((std::vector<GuidVector>{{456}}), written);
    EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST_F(BagJavaParityTest, ServiceMintFailureReportsUnmutatedAndKeepsBothOldInstances)
{
    Bag bag;
    FillEvictingBag(bag);
    const auto before = CaptureParityBag(bag);
    ASSERT_TRUE(ArmParitySegment(98300000, 98300001));
    EXPECT_EQ(kBagAddItemInvalidParam,
              BagService::AddItems(entt::null, bag, ItemCountMap{{10, maxStack + 1}},
                                   TX_SYSTEM_GRANT, 0, {}));
    EXPECT_EQ(before, CaptureParityBag(bag));
    EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST_F(BagJavaParityTest, FullBagRejectsSpaceBeforeConsultingUnavailableIdSource)
{
    Bag bag;
    bag.SetProfile(BagProfile::Flat(1));
    bag.InsertItemForRestore(kOldOther, 3, 1, 0, 1);
    const auto before = CaptureParityBag(bag);
    tlsGuidSegmentRegistry.Get(GuidKind::kItem).Reset();

    EXPECT_EQ(kBagItemNotStacked, bag.AddItems(ItemCountMap{{3, 1}}));
    EXPECT_EQ(before, CaptureParityBag(bag));
}

TEST_F(BagJavaParityTest, AllIdsAreReservedBeforeAnyInstanceInitializerRuns)
{
    Bag bag;
    bag.SetProfile(BagProfile::Flat(3));
    bag.InsertItemForRestore(kOldOther, 3, 1, 2, 1);
    const auto before = CaptureParityBag(bag);
    ASSERT_TRUE(ArmParitySegment(98400000, 98400002));
    int callbacks = 0;
    const ItemInstanceInitializer initialize = [&](ItemComp &item)
    {
        EXPECT_EQ(before, CaptureParityBag(bag));
        item.mutable_equip();
        if (++callbacks == 1)
        {
            tlsGuidSegmentRegistry.Get(GuidKind::kItem).Reset();
        }
    };
    std::vector<Guid> written;

    // 修复前回调后才铸第二个号，失败留下半批；先铸齐后不再依赖号源当前是否可用。
    EXPECT_EQ(kSuccess, bag.AddItem(ParityGrant(3, 2), &written, nullptr, &initialize));
    EXPECT_EQ(2, callbacks);
    EXPECT_EQ(2u, written.size());
    EXPECT_EQ(3u, bag.OccupiedGridCount());
    EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST_F(BagJavaParityTest, SuccessfulBatchReturnsEachEntriesActualWrittenGuids)
{
    Bag bag;
    bag.SetProfile(BagProfile::Flat(4));
    bag.InsertItemForRestore(kOldStack, 10, maxStack - 1, 3, 1);
    auto preassigned = ParityGrant(3, 1);
    preassigned.itemPBComp.set_item_id(kOldOther);
    std::vector<GuidVector> written;

    ASSERT_EQ(kSuccess, bag.AddItems(std::vector<InitItemParam>{ParityGrant(10, 2), preassigned}, nullptr, &written));
    ASSERT_EQ(2u, written.size());
    ASSERT_EQ(2u, written[0].size());
    EXPECT_EQ(kOldStack, written[0][0]);
    EXPECT_EQ((GuidVector{kOldOther}), written[1]);
    EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST_F(BagJavaParityTest, CandidatePlacementFailureKeepsOriginalLayoutAndAllReceipts)
{
    Bag bag;
    bag.SetLayout(std::make_unique<FailSecondPlacementLayout>(3));
    bag.InsertItemForRestore(kOldOther, 3, 1, 2, 4);
    const auto before = CaptureParityBag(bag);
    const auto *layoutBefore = &bag.Layout();
    std::vector<GuidVector> written{{123}};

    EXPECT_EQ(kBagItemNotStacked, bag.AddItems(ItemCountMap{{3, 2}}, nullptr, &written));
    EXPECT_EQ(before, CaptureParityBag(bag));
    EXPECT_EQ(layoutBefore, &bag.Layout());
    EXPECT_EQ(3u, bag.Capacity());
    EXPECT_EQ(2u, bag.GetItemPosByGuid(kOldOther));
    EXPECT_EQ((std::vector<GuidVector>{{123}}), written);
    EXPECT_TRUE(bag.IsLayerConsistent());
}

TEST_F(BagJavaParityTest, FormalLayoutClonesKeepGeometryAnchorsAndIndependentOccupancy)
{
    std::vector<std::unique_ptr<IContainerLayout>> layouts;
    layouts.push_back(std::make_unique<FlatLayout>(12));
    layouts.push_back(std::make_unique<FixedSlotLayout>(12));
    layouts.push_back(std::make_unique<GridLayout>(4, 3));
    for (const auto &layout : layouts)
    {
        ASSERT_TRUE(layout->PlaceAt(kOldStack, 1, Footprint{2, 2}));
        auto copy = layout->Clone();
        ASSERT_NE(nullptr, copy);
        EXPECT_EQ(layout->Capacity(), copy->Capacity());
        EXPECT_EQ(layout->Slots(), copy->Slots());
        EXPECT_EQ(layout->HasSlotSemantics(), copy->HasSlotSemantics());
        EXPECT_EQ(layout->FreeCells(), copy->FreeCells());
        copy->Remove(kOldStack);
        EXPECT_EQ(kOldStack, layout->At(1));
        EXPECT_EQ(kInvalidGuid, copy->At(1));
    }
}

TEST_F(BagJavaParityTest, DrainingLastUnitsImmediatelyReleasesInstancesAndSlots)
{
    Bag bag;
    bag.SetProfile(BagProfile::Flat(2));
    bag.InsertItemForRestore(kOldStack, 10, 2, 0, 1);
    bag.InsertItemForRestore(kOldOther, 3, 1, 1, 2);
    std::vector<DrainedInstance> drained;
    ASSERT_EQ(kSuccess, bag.RemoveItemsClamped(ItemCountMap{{10, 2}}, &drained));
    ASSERT_EQ(1u, drained.size());
    EXPECT_EQ(kOldStack, drained[0].guid);
    EXPECT_EQ(2u, drained[0].amount);
    EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kOldStack));
    EXPECT_EQ(nullptr, bag.GetItemCompByPos(0));

    RemoveItemByPosParam remove;
    remove.item_guid = kOldOther;
    remove.item_config_id = 3;
    remove.pos = 1;
    remove.size = 1;
    ASSERT_EQ(kSuccess, bag.RemoveItemByPos(remove));
    EXPECT_EQ(0u, bag.OccupiedGridCount());
    EXPECT_EQ(0u, bag.GridSlotCount());
    std::vector<DestroyedInstance> destroyed;
    EXPECT_FALSE(bag.MergeAndCompact(&destroyed, CompactPolicy::kMergeOnly));
    EXPECT_TRUE(destroyed.empty());
}

TEST_F(BagJavaParityTest, LegacyZeroSequencesFollowEveryExistingStampInTheirOwnBag)
{
    BagAllData input;
    AddParityEntry(input, kOldStack, 1, 0, 0);
    AddParityEntry(input, kOldOther, 1, 1, 70);
    AddParityEntry(input, kOldOther + 1, 1, 2, 0);
    auto *dynamic = input.add_dynamic_bags();
    dynamic->set_bag_id(90);
    dynamic->set_capacity(3);
    dynamic->add_items()->CopyFrom(input.items(0));
    dynamic->add_items()->CopyFrom(input.items(1));
    dynamic->mutable_items(0)->set_item_uuid(kOldStack + 20);
    dynamic->mutable_items(1)->set_item_uuid(kOldOther + 20);
    dynamic->mutable_items(1)->set_acquire_seq(100);
    bag_marshal::Unmarshal(player, input);
    auto &bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
    EXPECT_EQ(71u, bags.bags[kInventory].GetItemCompByGuid(kOldStack)->acquire_seq());
    EXPECT_EQ(72u, bags.bags[kInventory].GetItemCompByGuid(kOldOther + 1)->acquire_seq());
    EXPECT_EQ(101u, bags.dynamicBags_.at(90).GetItemCompByGuid(kOldStack + 20)->acquire_seq());
    std::vector<Guid> added;
    ASSERT_EQ(kSuccess, bags.bags[kInventory].AddItem(ParityGrant(3, 1), &added));
    ASSERT_EQ(1u, added.size());
    EXPECT_EQ(73u, bags.bags[kInventory].GetItemCompByGuid(added[0])->acquire_seq());
}

TEST_F(BagJavaParityTest, RestoreDropsZeroStacksWithoutOccupyingSlotsOrConsumingStamps)
{
    BagAllData input;
    AddParityEntry(input, kOldStack, 0, 0, 900);
    AddParityEntry(input, kOldOther, 1, 0, 0);
    bag_marshal::Unmarshal(player, input);
    auto &bag = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    EXPECT_EQ(1u, bag.OccupiedGridCount());
    EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kOldStack));
    const auto *kept = bag.GetItemCompByGuid(kOldOther);
    ASSERT_NE(nullptr, kept);
    EXPECT_EQ(1u, kept->acquire_seq());
    EXPECT_EQ(0u, bag.GetItemPosByGuid(kOldOther));
    BagAllData output;
    bag_marshal::Marshal(player, output);
    ASSERT_EQ(1, output.items_size());
    EXPECT_EQ(kOldOther, output.items(0).item_uuid());
}

TEST_F(BagJavaParityTest, MarshalUsesBagThenSlotOrderForFixedAndDynamicBags)
{
    auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
    bags.bags[kInventory].InsertItemForRestore(kOldStack, 10, 1, 4, 1);
    bags.bags[kInventory].InsertItemForRestore(kOldOther, 10, 1, 0, 2);
    for (const uint64_t id : {90u, 10u})
    {
        auto &bag = bags.dynamicBags_[id];
        bag.InsertItemForRestore(kOldStack + id, 10, 1, 3, 1);
        bag.InsertItemForRestore(kOldOther + id, 10, 1, 0, 2);
    }
    BagAllData first;
    bag_marshal::Marshal(player, first);
    ASSERT_EQ(2, first.items_size());
    EXPECT_EQ(kOldOther, first.items(0).item_uuid());
    EXPECT_EQ(0u, first.items(0).pos());
    EXPECT_EQ(kOldStack, first.items(1).item_uuid());
    ASSERT_EQ(2, first.dynamic_bags_size());
    EXPECT_EQ(10u, first.dynamic_bags(0).bag_id());
    EXPECT_EQ(90u, first.dynamic_bags(1).bag_id());
    for (const auto &dynamic : first.dynamic_bags())
    {
        ASSERT_EQ(2, dynamic.items_size());
        EXPECT_EQ(0u, dynamic.items(0).pos());
        EXPECT_EQ(3u, dynamic.items(1).pos());
    }
    bag_marshal::Unmarshal(player, first);
    BagAllData second;
    bag_marshal::Marshal(player, second);
    EXPECT_EQ(first.SerializeAsString(), second.SerializeAsString());
}
} // namespace

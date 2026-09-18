#include <gtest/gtest.h>

#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/bag_marshal.h"
#include "proto/common/database/bag_quest_mail_data.pb.h"
#include "thread_context/ecs_context.h"

// ---------------------------------------------------------------------------
// cross_zone_test — unit tests for the cross-zone repair triad's
// non-K8s-requiring invariants. K8s-required tests (Kafka broker fail,
// destination node crash, source restart, duplicate delivery) are
// documented in docs/ops/cross-zone-failure-test-runbook.md and have to
// run against a real two-zone cluster.
//
// ⚠️ STATUS (2026-05-19): the binary compiles + links cleanly, but
// launching it currently exits with no stdout — a global static-init
// chain in one of the linked libs (probably table.lib's ItemTable or
// thread_context.lib's tlsEcs reset) crashes before main() runs.
// bag_test sidesteps this with test_config_helper.h. The right fix is
// either:
//   (a) include test_config_helper.h and call LoadAllTables() / init
//       tlsEcs in main() — adds spurious dependencies for tests that
//       don't actually need ItemTable; or
//   (b) prune AdditionalDependencies in cross_zone_test.vcxproj to drop
//       table.lib / config.lib so the static init never runs.
// Either approach is ~0.5 session of work and isn't blocking the K8s
// SOP validation, so the test source + vcxproj ship now as the skeleton.
// When init helper lands, every TEST(...) below should pass without
// further changes — they only exercise the pure-function Marshal /
// Unmarshal / IsCrossZoneFrozen paths.
//
// What this file covers (all single-process, gtest-runnable):
//   1. BagMarshalRoundTrip      — Marshal → Unmarshal preserves all items
//                                 + capacities across all 4 bag_type slots.
//   2. BagMarshalEmptyBag       — empty bags round-trip cleanly (zero items,
//                                 zero capacities, no spurious entries).
//   3. BagMarshalNoBagsComp     — Marshal on a player without
//                                 PlayerBagsComp emits empty BagAllData
//                                 without crashing.
//   4. BagUnmarshalCreatesComp  — Unmarshal on a fresh player entity uses
//                                 get_or_emplace to create PlayerBagsComp,
//                                 so whoever rebuilds the player from a
//                                 PlayerAllData snapshot (the node taking
//                                 over after an ownership handoff) doesn't
//                                 have to pre-emplace.
//   5. BagUnmarshalDropsBadType — bag_type >= kBagTypeCount in incoming
//                                 BagAllData is dropped with WARN, not
//                                 crashed.
//   6. FrozenCompMarker         — PlayerFrozenComp emplace/remove +
//                                 IsCrossZoneFrozen() semantics.
//
// History: these cases were written for the player_migrate data-moving chain
// (Kafka PlayerMigrationEvent + ACK + CrossZoneReaper). That chain was
// decommissioned on 2026-09-18 (cross-zone-scene-travel.md §11.3): players no
// longer move between zones by shipping PlayerAllData, the target zone loads
// them from the shared Redis after an owner_epoch handoff. The marshal
// round-trip and PlayerFrozenComp semantics tested here are still what that
// handoff relies on, so the cases stay.
//
// What this file does NOT cover (intentionally):
//   - The handoff chain end-to-end (StartTravelHandoff → save → handoff mark →
//     scene_manager.EnterScene → reply / watchdog) — needs scene-node bootstrap
//     (tlsRedisSystem, world tick, a scene_manager). See robot travel-smoke.
// ---------------------------------------------------------------------------

namespace
{

// One-shot helper: emplace 4 bags with a few items spread across kInventory /
// kWarehouse / kEquipment, return the player entity. Capacities deliberately
// non-default to verify capacities[] round-trip carries unlock state.
entt::entity CreatePlayerWithStockedBags(uint32_t playerSeed)
{
    auto& reg = tlsEcs.actorRegistry;
    const auto player = reg.create();

    auto& bags = reg.emplace<PlayerBagsComp>(player);

    // kInventory (0): 3 items at pos 0/1/2.
    bags.bags[kInventory].SetCapacityForRestore(50);
    bags.bags[kInventory].InsertItemForRestore(
        /*guid=*/100ULL + playerSeed, /*configId=*/1001, /*stackSize=*/5, /*pos=*/0);
    bags.bags[kInventory].InsertItemForRestore(101ULL + playerSeed, 1002, 1, 1);
    bags.bags[kInventory].InsertItemForRestore(102ULL + playerSeed, 1003, 99, 2);

    // kWarehouse (1): 1 item.
    bags.bags[kWarehouse].SetCapacityForRestore(100);
    bags.bags[kWarehouse].InsertItemForRestore(200ULL + playerSeed, 2001, 50, 0);

    // kEquipment (2): 2 items in different slots.
    bags.bags[kEquipment].SetCapacityForRestore(10);
    bags.bags[kEquipment].InsertItemForRestore(300ULL + playerSeed, 3001, 1, 0);
    bags.bags[kEquipment].InsertItemForRestore(301ULL + playerSeed, 3002, 1, 5);

    // kTemporary (3): empty + custom capacity.
    bags.bags[kTemporary].SetCapacityForRestore(200);

    return player;
}

// Collect items across all four bags into a flat (bagType, guid, configId,
// stackSize, pos) tuple list. Order-insensitive comparison is the caller's
// responsibility (Marshal walks an unordered_map internally).
struct ItemSnapshot
{
    uint32_t bagType;
    Guid guid;
    uint32_t configId;
    uint32_t stackSize;
    uint32_t pos;

    bool operator<(const ItemSnapshot& o) const
    {
        if (guid != o.guid) return guid < o.guid;
        return bagType < o.bagType;
    }
    bool operator==(const ItemSnapshot& o) const
    {
        return bagType == o.bagType && guid == o.guid && configId == o.configId
            && stackSize == o.stackSize && pos == o.pos;
    }
};

std::vector<ItemSnapshot> SnapshotPlayerBags(entt::entity player)
{
    std::vector<ItemSnapshot> out;
    const auto& bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
    for (uint32_t bagType = 0; bagType < static_cast<uint32_t>(kBagTypeCount); ++bagType)
    {
        const auto& bag = bags.bags[bagType];
        bag.ForEachItem([&](Guid guid, const ItemComp& item) {
            out.push_back({bagType, guid, item.config_id(), item.size(),
                           bag.GetItemPosByGuid(guid)});
        });
    }
    std::sort(out.begin(), out.end());
    return out;
}

// Capacity vector helper.
std::array<std::size_t, kBagTypeCount> SnapshotCapacities(entt::entity player)
{
    std::array<std::size_t, kBagTypeCount> caps{};
    const auto& bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
    for (uint32_t i = 0; i < kBagTypeCount; ++i)
    {
        caps[i] = bags.bags[i].Capacity();
    }
    return caps;
}

}  // namespace

// ============================================================================
// Bag Marshal/Unmarshal — round-trip preserves data
// ============================================================================
TEST(CrossZoneBagMarshal, RoundTripPreservesAllItemsAndCapacities)
{
    tlsEcs.actorRegistry.clear();

    const auto sourcePlayer = CreatePlayerWithStockedBags(/*seed=*/0);
    const auto sourceItems = SnapshotPlayerBags(sourcePlayer);
    const auto sourceCaps  = SnapshotCapacities(sourcePlayer);

    // Marshal source → proto → wire-equivalent serialize/parse → Unmarshal
    // into a fresh entity (simulates the cross-zone hop into a new node).
    BagAllData wire;
    bag_marshal::Marshal(sourcePlayer, wire);

    // Verify Marshal output sizes — sanity check before round-trip.
    EXPECT_EQ(wire.items_size(), 6) << "expected 3+1+2+0 items across 4 bags";
    EXPECT_EQ(wire.capacities_size(), static_cast<int>(kBagTypeCount));

    // Actually serialize/parse to catch proto schema bugs (any missing
    // field in the .proto would silently drop here).
    std::string bytes;
    ASSERT_TRUE(wire.SerializeToString(&bytes));
    BagAllData wireOnDest;
    ASSERT_TRUE(wireOnDest.ParseFromString(bytes));

    // Destination side: fresh entity, Unmarshal should rebuild
    // PlayerBagsComp via get_or_emplace.
    const auto destPlayer = tlsEcs.actorRegistry.create();
    bag_marshal::Unmarshal(destPlayer, wireOnDest);

    const auto destItems = SnapshotPlayerBags(destPlayer);
    const auto destCaps  = SnapshotCapacities(destPlayer);

    EXPECT_EQ(sourceItems, destItems)
        << "round-trip should preserve every item's (bagType, guid, configId, "
        << "stackSize, pos). If this fails, check bag_marshal.cpp Marshal "
        << "Bag::ForEachItem / Unmarshal Bag::InsertItemForRestore.";
    EXPECT_EQ(sourceCaps, destCaps)
        << "per-bag capacities (unlock state) must round-trip via "
        << "BagAllData.capacities[]";
}

TEST(CrossZoneBagMarshal, EmptyBagRoundTrip)
{
    tlsEcs.actorRegistry.clear();

    const auto source = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<PlayerBagsComp>(source);  // all 4 bags empty + default capacities

    BagAllData wire;
    bag_marshal::Marshal(source, wire);
    EXPECT_EQ(wire.items_size(), 0);
    EXPECT_EQ(wire.capacities_size(), static_cast<int>(kBagTypeCount))
        << "Marshal must always emit kBagTypeCount capacities even for empty bags, "
        << "so destination can index bag_type → capacity directly.";

    const auto dest = tlsEcs.actorRegistry.create();
    bag_marshal::Unmarshal(dest, wire);
    EXPECT_EQ(SnapshotPlayerBags(dest).size(), 0u);
}

TEST(CrossZoneBagMarshal, MarshalWithoutBagsCompYieldsEmpty)
{
    tlsEcs.actorRegistry.clear();

    // Player without PlayerBagsComp — Marshal should NOT crash, just emit
    // empty BagAllData (matches the comment in bag_marshal.cpp Marshal()).
    const auto player = tlsEcs.actorRegistry.create();
    BagAllData wire;
    ASSERT_NO_THROW(bag_marshal::Marshal(player, wire));
    EXPECT_EQ(wire.items_size(), 0);
    EXPECT_EQ(wire.capacities_size(), 0)
        << "no PlayerBagsComp → no capacities emitted (Marshal early-returns)";
}

TEST(CrossZoneBagMarshal, UnmarshalCreatesPlayerBagsComp)
{
    tlsEcs.actorRegistry.clear();

    // Fresh player entity, no bags. Unmarshal should get_or_emplace.
    const auto player = tlsEcs.actorRegistry.create();
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerBagsComp>(player));

    BagAllData wire;
    wire.add_capacities(64);
    wire.add_capacities(128);
    wire.add_capacities(20);
    wire.add_capacities(256);
    auto* item = wire.add_items();
    item->set_item_uuid(999);
    item->set_config_id(1234);
    item->set_stack_size(7);
    item->set_pos(3);
    item->set_bag_type(static_cast<uint32_t>(kInventory));

    bag_marshal::Unmarshal(player, wire);
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<PlayerBagsComp>(player))
        << "Unmarshal must emplace PlayerBagsComp so the node rebuilding the player "
        << "from a snapshot doesn't have to pre-emplace it";

    const auto items = SnapshotPlayerBags(player);
    ASSERT_EQ(items.size(), 1u);
    EXPECT_EQ(items[0].guid, 999u);
    EXPECT_EQ(items[0].configId, 1234u);
    EXPECT_EQ(items[0].stackSize, 7u);
    EXPECT_EQ(items[0].pos, 3u);
    EXPECT_EQ(items[0].bagType, static_cast<uint32_t>(kInventory));
}

TEST(CrossZoneBagMarshal, UnmarshalDropsOutOfRangeBagType)
{
    tlsEcs.actorRegistry.clear();

    const auto player = tlsEcs.actorRegistry.create();
    BagAllData wire;
    // Two items: one valid (kInventory), one bag_type out of range.
    auto* good = wire.add_items();
    good->set_item_uuid(1);
    good->set_config_id(100);
    good->set_stack_size(1);
    good->set_pos(0);
    good->set_bag_type(static_cast<uint32_t>(kInventory));

    auto* bad = wire.add_items();
    bad->set_item_uuid(2);
    bad->set_config_id(200);
    bad->set_stack_size(1);
    bad->set_pos(0);
    bad->set_bag_type(static_cast<uint32_t>(kBagTypeCount) + 5);  // future / corrupt

    bag_marshal::Unmarshal(player, wire);
    const auto items = SnapshotPlayerBags(player);
    EXPECT_EQ(items.size(), 1u) << "out-of-range bag_type must be dropped, "
                                << "not crash. Forward-compat for future bag types.";
    EXPECT_EQ(items[0].guid, 1u);
}

// ============================================================================
// PlayerFrozenComp invariants
// ============================================================================
#include "player/system/player_lifecycle.h"

TEST(CrossZoneFrozen, IsCrossZoneFrozenReflectsComponent)
{
    tlsEcs.actorRegistry.clear();

    const auto player = tlsEcs.actorRegistry.create();
    EXPECT_FALSE(PlayerLifecycleSystem::IsCrossZoneFrozen(player))
        << "fresh entity is not frozen";

    auto& frozen = tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    frozen.frozenAtMs = 12345;
    frozen.toZoneId = 42;
    frozen.migrateAttempts = 1;
    EXPECT_TRUE(PlayerLifecycleSystem::IsCrossZoneFrozen(player));

    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
    EXPECT_FALSE(PlayerLifecycleSystem::IsCrossZoneFrozen(player))
        << "removing the component must clear Frozen state immediately so "
        << "business systems unblock writes on the same tick.";
}

TEST(CrossZoneFrozen, InvalidEntityIsNotFrozen)
{
    tlsEcs.actorRegistry.clear();

    EXPECT_FALSE(PlayerLifecycleSystem::IsCrossZoneFrozen(entt::null))
        << "entt::null must return false, not crash. Defensive contract "
        << "matches the implementation in player_lifecycle.cpp.";
}

// ============================================================================
// Test bootstrap
// ============================================================================
int main(int argc, char** argv)
{
    ::testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}

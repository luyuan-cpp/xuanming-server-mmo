#include <gtest/gtest.h>
#include <google/protobuf/util/message_differencer.h>
#include <vector>
#include "hexagons_grid.h"
#include "infra/agones/agones_gameserver_lifecycle.h"
#include "core/node/system/node/muduo_grpc_channel_slot.h"
#include "muduo/net/EventLoop.h"
#include "muduo_grpc/channel.h"
#include "actor/actor_net_id.h"
#include "core/system/id_generator.h"
#include "engine/core/type_define/player_guid_comp.h"

#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/bag_marshal.h"
#include "player/system/role_admission_system.h"
#include "proto/common/database/bag_quest_mail_data.pb.h"
#include "thread_context/ecs_context.h"
#include "network/node_utils.h"
#include "node/system/discovery/service_node_snapshot.h"
#include "spatial/system/aoi.h"
#include "spatial/comp/grid_comp.h"
#include "player/system/player_scene.h"
#include "proto/scene/scene.pb.h"
#include "world/scene_lifecycle.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/scene_error_tip.pb.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "entt/src/entt/signal/sigh.hpp"

// 测试宿主不构造 Node,由同一个只读门面安装身份;每例清零,不依赖执行顺序。
NodeInfo &TestLocalNodeInfo()
{
    static NodeInfo info;
    return info;
}

namespace
{
constexpr uint64_t kFixtureAdmissionGeneration = 1;
constexpr uint64_t kFixtureOwnerRevision = 7;
void SetFixtureAdmission(PlayerEnterContext& context);
class LocalNodeInfoResetListener : public ::testing::EmptyTestEventListener
{
    void OnTestStart(const ::testing::TestInfo &) override
    {
        TestLocalNodeInfo().Clear();
        tlsIdGeneratorManager.SetNodeId(94);
        actor_net_id::Install(tlsEcs.actorRegistry);
        RoleAdmissionSystem::Dependencies dependencies;
        for (const Guid player : {Guid{950200001}, Guid{950200002}})
        {
            auto& identity = dependencies.confirmedAdmissions.emplace_back();
            identity.set_player_id(player);
            identity.set_generation(kFixtureAdmissionGeneration);
            identity.set_owner_revision(kFixtureOwnerRevision);
            identity.set_scene_instance_id("explicit-test-scene");
        }
        admission_ = std::make_unique<RoleAdmissionSystem::ScopedDependencies>(std::move(dependencies));
    }
    void OnTestEnd(const ::testing::TestInfo &) override { admission_.reset(); }
    std::unique_ptr<RoleAdmissionSystem::ScopedDependencies> admission_;
};
}

// ---------------------------------------------------------------------------
// cross_zone_test — single-process unit tests for the pieces the ownership
// handoff (cross-zone travel / same-zone cross-node scene change,
// docs/design/cross-zone-scene-travel.md) relies on: bag marshal round-trip,
// PlayerFrozenComp semantics, and the handoff-mark withdrawal queue.
// (The "cross-zone repair triad" this file was originally written for — the
// player_migrate data-moving chain — is gone, see History below.)
// Multi-node failure drills live in docs/ops/cross-zone-failure-test-runbook.md
// and need a real two-zone cluster.
//
// STATUS: the 2026-05-19 note that used to sit here ("binary exits with no
// stdout before main()") is obsolete. Commit 01e23eb05 (2026-09-05) fixed the
// vcxproj (hiredisd.lib / absl lib dir / battle.lib), registered the project in
// game.sln with Build.0, added it to tools/scripts/run_cpp_tests.ps1 (which
// also syncs the zlibd / rdkafka DLLs — a missing DLL makes a test exe exit
// silently with 0xC0000135, the likely cause of the old symptom) and recorded
// the project green. It has NOT been re-run since the
// cross-zone travel code (phase 1/2/3) landed — that is pending Codex.
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
//   5. BagUnmarshalCarriesBadType — bag_type >= kBagTypeCount in incoming
//                                 BagAllData stays out of the runtime bags
//                                 and is carried verbatim back to the
//                                 snapshot (mixed-version §12, batch 5),
//                                 not dropped, not crashed.
//   6. FrozenCompMarker         — PlayerFrozenComp emplace/remove +
//                                 IsCrossZoneFrozen() semantics.
//   7. HandoffMarkWithdrawQueue — the pending-withdrawal table for handoff
//                                 marks (handoff_mark_withdraw.h): dedup,
//                                 retry pacing, deadline expiry, exact-value
//                                 confirm, capacity eviction, and that the Lua
//                                 stays a *conditional* delete. Pure container,
//                                 time is injected; the Redis glue around it
//                                 (WithdrawHandoffMark) is not covered here.
//   8. TravelOutcomeReset       — travel_outcome (player_lifecycle.h): when a
//                                 granted-without-reply handoff must tip + kick
//                                 the client (cross-zone + failed/no-reply only)
//                                 and the "location is this handoff's awaiting
//                                 placement" predicate. Pure functions; the MGET
//                                 glue in ResolveTravelOutcome is not covered.
//   9. ExitPersist              — Z1 exit-save convergence (§12.6.3 step 1): the
//                                 exit-intent predicates plus ECS-level regressions
//                                 that drive HandlePlayerAsyncSaved /
//                                 HandleExitGameNode in a host without Redis.
//   10. ExitRelease             — A1′ release mark / A2′ inherited-mark clear
//                                 (§12.6.3 steps 2/3): pure decisions, the Lua
//                                 fragments that matter, and the no-Redis ECS seams.
//   11. EnterSceneReplyRoute    — EnterScene replies are routed by the echoed
//                                 correlation_id (enter_scene_reply::Classify /
//                                 IsSuspiciousUnmatched), plus ECS-level cases that
//                                 drive PlayerLifecycleSystem::DispatchEnterSceneReply:
//                                 a foreign reply never becomes handoff evidence,
//                                 the §11.2 "18 starts a same-zone handoff" chain
//                                 still works, and an old scene_manager (no echo,
//                                 corr 0) falls back to today's player_id matching.
//   12. TravelFreezeCap         — handoff freeze hard cap + dispatch window
//                                 (travel_freeze_cap.h): the budget relations the
//                                 static_asserts guard, DecideFreezeCap / dispatch
//                                 window / monotonic re-check pure functions, plus
//                                 ECS-level seams in a host without Redis that drive
//                                 EnforceTravelFreezeCaps (unfreeze only when the
//                                 handoff mark was never sent, otherwise tip + kick +
//                                 destroy without persisting) and the set_mark-stage
//                                 gate in BeginTravelHandoff; plus the handoff-mark SET
//                                 ERROR reply only aborting its own generation
//                                 (HandleTravelMarkWriteRejected).
//   13. TravelOwnership         — GO-2 source-side verdict (§12.8 decision table
//                                 B4–B9): travel_outcome::ClassifyOwnership (unchanged
//                                 epoch wins, only this handoff's rollback receipt with a
//                                 full cross-check is adopted, the returned-after-grant
//                                 counterexample is never adopted) and the shape of
//                                 kLuaJudgeTravelOutcome (#!lua, MGET only, deletes only
//                                 its own lineage). The script's Redis behaviour is
//                                 covered by the Go cross-language golden test
//                                 go/scene_manager/internal/logic/owner_epoch_crosslang_test.go.
//   14. RelocateConfirm         — CPP-3 pending-confirmation table for drain /
//                                 evacuation relocates (relocate_confirm.h): ticket
//                                 voiding, claiming replies / transport failures by
//                                 correlation_id, placement verdicts (zone / node / scene
//                                 only), the settle rule, the six-way credential decision,
//                                 "kick blind only when none of our requests can have
//                                 written a placement", and the Table's phase transitions /
//                                 due classification with injected time; plus ECS-level
//                                 seams in a host without scene_manager / gate / Redis
//                                 (not sent → verify → kick, reentry reconcile, ticket
//                                 voiding, the local re-check before a kick).

//   15. BagEquipInstanceData    — the equipment instance data (ItemEntry.equip,
//                                 docs/design/equipment-attributes.md §3.1) rides the
//                                 saved PlayerAllData across a hop: attribute rows and
//                                 the has_equip() presence both survive, in the
//                                 player_database record and in the legacy top-level
//                                 mirror alike.
//   16. EnterFence              — scene enter session fence (login_generation,
//                                 docs/design/scene-enter-session-fence.md §5):
//                                 the enter_fence::Decide table cell by cell and
//                                 DecideAll's merge rule, plus ECS-level seams that
//                                 drive PlayerLifecycleSystem::HandleEnterGameNode in a
//                                 host without Redis (stale cross-session enters are
//                                 rejected on the entity / pending / relocate-entry
//                                 paths, same-session merges, owned-only SessionMap
//                                 erasure, the bind-time re-check in EnterScene).
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
//   - Freeze cap (section 12) pieces the no-Redis / no-EventLoop host cannot reach
//     (code review + runbook scenario H only):
//       * the enter_scene-stage dispatch-window gate in RequestTravelEnterScene
//         (only reachable from the handoff SET's OK callback);
//       * the StartTravelHandoff fast-path forced save (HasUnsettledPlayerSave needs
//         a real player-data Redis client, it is always false here);
//       * the "unsettled save → defer destroy" branch of
//         ConcludeHandoffAfterMarkSent (same reason);
//       * RunAtMonotonic re-arming on an early muduo wake-up (needs an EventLoop).
//   - GO-2 (section 13) pieces that need a Redis reply: the judge-script callback
//     (JudgeTravelOutcomeReply: adopt → unfreeze → forced save, receipt anomaly →
//     ConcludeHandoffAfterMarkSent) and the conditional evacuation mark write in
//     DispatchEmergencyRelocate — runbook B4a / B4b / B5 and the evacuation drill only.
//   - Relocate confirmation (section 14) pieces the host cannot reach — the send
//     needs a running EventLoop with a live scene_manager behind the muduo_grpc
//     channel, and the verify needs a zone Redis, so these are
//     covered by the pure-function / Table cases and runbook scenario V only:
//       * anything past kAwaitingReply at the ECS level;
//       * the kVerify branches of the correlated reply claim and the transport-failure
//         claim (only the reply claim calls Table::MarkCompletion; a transport failure
//         is "outcome unknown" and deliberately leaves completionSeen false);
//       * the "previous relocate's own request is still unsettled" source of
//         earlierEnterSceneMayReply for an entry that was actually sent (the host only
//         reaches it through an earlier-flagged, never-sent previous entry), and that
//         source's settleAt bound (registration reads the real monotonic clock, there is
//         no injection point to get past the previous entry's settleAt);
//       * the glue that keeps the ticket on a same-session reentry while the whole node is
//         evacuating (tlsEmergencyRelocating has no reset hook; the decision itself is the
//         pure CancelsTicketOnReentry, covered by RelocateConfirmTicket.*);
//       * HandleRelocateVerifyReply, including the glue that drops stale verify replies;
//       * the conditional credential write;
//       * the kSent path of PushToSessionViaGate.
//   - Enter fence (section 16) pieces outside the scene library: the node-level
//     handlers (SceneHandler::PlayerEnterGameNode's AsyncLoad, UpdateSessionDetail's
//     fence call) and the [EnterFence] 30s summary line in RedisSystem.
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

// 本版本不认识的包类型(新版本加的包)的条目与容量:不进任何运行时包、不崩,整份原样携带,存盘原样写回
// (docs/design/mixed-version-persist-field-protection.md §12,第 5 批)。以前这里直接丢弃 —— 新版本加了包类型后
// 回滚 scene,旧版本一存盘,新包里的物品就全没了。
TEST(CrossZoneBagMarshal, UnmarshalCarriesOutOfRangeBagTypeVerbatim)
{
    tlsEcs.actorRegistry.clear();

    const auto player = tlsEcs.actorRegistry.create();
    BagAllData wire;
    for (uint32_t bagType = 0; bagType < static_cast<uint32_t>(kBagTypeCount); ++bagType)
    {
        wire.add_capacities(20 + bagType);
    }
    wire.add_capacities(77); // 下标 kBagTypeCount:新版本加的第一个包
    wire.add_capacities(88);
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
    bad->set_stack_size(3);
    bad->set_pos(5);
    bad->set_bag_type(static_cast<uint32_t>(kBagTypeCount) + 5);  // future / corrupt
    bad->mutable_instance()->set_item_id(2);
    bad->mutable_instance()->set_config_id(200);
    bad->mutable_instance()->set_size(3);
    bad->mutable_instance()->mutable_unknown_fields()->AddVarint(412345, 9); // 新版本的物品属性
    const ItemEntry badCopy = *bad;

    bag_marshal::Unmarshal(player, wire);
    const auto items = SnapshotPlayerBags(player);
    ASSERT_EQ(items.size(), 1u) << "out-of-range bag_type must not enter any runtime bag, and must not crash";
    EXPECT_EQ(items[0].guid, 1u);
    const auto& bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
    ASSERT_EQ(1u, bags.unsupportedBagTypes.items.size());
    EXPECT_EQ(std::vector<uint32_t>({77u, 88u}), bags.unsupportedBagTypes.capacities);

    BagAllData saved;
    bag_marshal::Marshal(player, saved);
    ASSERT_EQ(static_cast<int>(kBagTypeCount) + 2, saved.capacities_size());
    EXPECT_EQ(77u, saved.capacities(static_cast<int>(kBagTypeCount)));
    EXPECT_EQ(88u, saved.capacities(static_cast<int>(kBagTypeCount) + 1));
    ASSERT_EQ(2, saved.items_size());
    EXPECT_EQ(1u, saved.items(0).item_uuid());
    EXPECT_TRUE(google::protobuf::util::MessageDifferencer::Equals(badCopy, saved.items(1)))
        << "the unknown bag type's entry must be written back verbatim, unknown fields included";
    EXPECT_EQ(saved.items(1).SerializeAsString(), badCopy.SerializeAsString());

    // 重新还原(回档到已有实体):载荷里没有越界记录时,上一次携带的不得残留。
    BagAllData withoutUnsupported;
    *withoutUnsupported.add_items() = wire.items(0);
    bag_marshal::Unmarshal(player, withoutUnsupported);
    EXPECT_TRUE(bags.unsupportedBagTypes.items.empty());
    EXPECT_TRUE(bags.unsupportedBagTypes.capacities.empty());
    BagAllData resaved;
    bag_marshal::Marshal(player, resaved);
    EXPECT_EQ(static_cast<int>(kBagTypeCount), resaved.capacities_size());
    EXPECT_EQ(1, resaved.items_size());
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
// 7. HandoffMarkWithdrawQueue —— handoff 标记待撤回表(纯容器,时间由用例注入)
// ============================================================================
#include <chrono>
#include <optional>
#include <string>

#include "player/system/handoff_mark_withdraw.h"

namespace
{
    namespace hmw = handoff_mark_withdraw;

    constexpr uint64_t kWithdrawPlayerA = 100000000000001ull;
    constexpr uint64_t kWithdrawPlayerB = 100000000000002ull;
    constexpr std::chrono::seconds kWithdrawTtl{305};

    // 任意固定起点:用例只关心相对时间,不读真实时钟。
    hmw::Clock::time_point WithdrawT0()
    {
        return hmw::Clock::time_point{} + std::chrono::hours(1);
    }
} // namespace

TEST(HandoffMarkWithdrawQueue, AddThenHasPlayer)
{
    hmw::Queue queue;
    std::optional<hmw::Entry> evicted = hmw::Entry{}; // 预置非空:Add 没淘汰时必须把它清掉
    EXPECT_TRUE(queue.Add(kWithdrawPlayerA, "7:100", WithdrawT0(), kWithdrawTtl, evicted));
    EXPECT_FALSE(evicted.has_value());
    EXPECT_EQ(queue.size(), 1u);
    EXPECT_TRUE(queue.HasPlayer(kWithdrawPlayerA));
    EXPECT_FALSE(queue.HasPlayer(kWithdrawPlayerB)) << "闸口按 player_id 判,不能误伤别的玩家";
}

TEST(HandoffMarkWithdrawQueue, DuplicateAddKeepsOriginalDeadline)
{
    hmw::Queue queue;
    std::optional<hmw::Entry> evicted;
    ASSERT_TRUE(queue.Add(kWithdrawPlayerA, "7:100", WithdrawT0(), kWithdrawTtl, evicted));
    EXPECT_FALSE(queue.Add(kWithdrawPlayerA, "7:100", WithdrawT0() + std::chrono::seconds(100), kWithdrawTtl, evicted));
    std::size_t expired = 0;
    const auto due = queue.CollectDue(WithdrawT0() + kWithdrawTtl, false, expired);
    EXPECT_EQ(0u, expired); ASSERT_EQ(1u, due.size());
    EXPECT_EQ(1u, queue.size()) << "资格不能因标记TTL到期消账";
}

TEST(HandoffMarkWithdrawQueue, CollectDueRespectsNextAttempt)
{
    hmw::Queue queue;
    std::optional<hmw::Entry> evicted;
    ASSERT_TRUE(queue.Add(kWithdrawPlayerA, "7:100", WithdrawT0(), kWithdrawTtl, evicted));

    std::size_t expired = 0;
    auto due = queue.CollectDue(WithdrawT0(), /*force=*/false, expired);
    ASSERT_EQ(due.size(), 1u) << "新登记的条目立刻到期,第一次撤回由它发出";
    EXPECT_EQ(due[0].playerId, kWithdrawPlayerA);
    EXPECT_EQ(due[0].markValue, "7:100");
    EXPECT_EQ(due[0].attempts, 1u) << "首发 = 1:调用方只在首发失败时打 ERROR";
    EXPECT_EQ(expired, 0u);

    due = queue.CollectDue(WithdrawT0() + hmw::kRetryInterval - std::chrono::seconds(1), /*force=*/false, expired);
    EXPECT_TRUE(due.empty()) << "重试间隔之内不重发";

    due = queue.CollectDue(WithdrawT0() + hmw::kRetryInterval - std::chrono::seconds(1), /*force=*/true, expired);
    ASSERT_EQ(due.size(), 1u) << "重连时 force 忽略重试间隔";
    EXPECT_EQ(due[0].attempts, 2u) << "间隔内被跳过的那一轮不计次,真正取走才加一";

    EXPECT_EQ(queue.size(), 1u) << "CollectDue 只取副本,销账只能靠 Confirm";
    EXPECT_TRUE(queue.HasPlayer(kWithdrawPlayerA));
}

TEST(HandoffMarkWithdrawQueue, UnconfirmedRevocationNeverExpiresByMarkTTL)
{
    hmw::Queue queue;
    std::optional<hmw::Entry> evicted;
    ASSERT_TRUE(queue.Add(kWithdrawPlayerA, "7:100", WithdrawT0(), kWithdrawTtl, evicted));
    std::size_t expired = 99;
    const auto due = queue.CollectDue(WithdrawT0() + kWithdrawTtl + std::chrono::seconds(1), true, expired);
    EXPECT_EQ(0u, expired); ASSERT_EQ(1u, due.size());
    EXPECT_EQ(1u, queue.size()); EXPECT_TRUE(queue.HasPlayer(kWithdrawPlayerA));
}

TEST(HandoffMarkWithdrawQueue, ConfirmRemovesOnlyExactValue)
{
    hmw::Queue queue;
    std::optional<hmw::Entry> evicted;
    ASSERT_TRUE(queue.Add(kWithdrawPlayerA, "7:100", WithdrawT0(), kWithdrawTtl, evicted));
    ASSERT_TRUE(queue.Add(kWithdrawPlayerA, "7:200", WithdrawT0(), kWithdrawTtl, evicted));

    EXPECT_FALSE(queue.Confirm(kWithdrawPlayerA, "7:300")) << "不在表里的标记销不了账";
    EXPECT_FALSE(queue.Confirm(kWithdrawPlayerB, "7:100")) << "别的玩家的同值标记也不行";
    EXPECT_TRUE(queue.Confirm(kWithdrawPlayerA, "7:100"));
    EXPECT_EQ(queue.size(), 1u);
    EXPECT_TRUE(queue.HasPlayer(kWithdrawPlayerA)) << "\"7:200\" 还没确认,闸口不能放开";
    EXPECT_FALSE(queue.Confirm(kWithdrawPlayerA, "7:100")) << "重复应答的第二次销账是 no-op";

    EXPECT_TRUE(queue.Confirm(kWithdrawPlayerA, "7:200"));
    EXPECT_FALSE(queue.HasPlayer(kWithdrawPlayerA));
}

TEST(HandoffMarkWithdrawQueue, CapacityRejectsNewEntryWithoutEvictingUnconfirmedRevocation)
{
    hmw::Queue queue;
    std::optional<hmw::Entry> evicted;
    for (std::size_t i=0;i<hmw::kMaxPending;++i)
        ASSERT_TRUE(queue.Add(i+1,"7:100",WithdrawT0(),kWithdrawTtl,evicted));
    EXPECT_FALSE(queue.Add(hmw::kMaxPending+1,"7:100",WithdrawT0(),kWithdrawTtl,evicted));
    EXPECT_FALSE(evicted.has_value()); EXPECT_EQ(hmw::kMaxPending,queue.size());
    EXPECT_TRUE(queue.HasPlayer(1)); EXPECT_FALSE(queue.HasPlayer(hmw::kMaxPending+1));
}

TEST(HandoffMarkWithdrawQueue, LuaIsConditionalDelete)
{
    const std::string lua = hmw::kLuaDelIfEqual;
    EXPECT_NE(lua.find("raw~=ARGV[1] and raw~=ARGV[2] and raw~=ARGV[3]"), std::string::npos);
    EXPECT_LT(lua.find("redis.call('HSET'"), lua.find("redis.call('DEL'")) << "mark缺席也先撤资格";
    EXPECT_NE(lua.find("sameholder(held,next)"), std::string::npos);
    EXPECT_EQ(lua.find("tonumber"), std::string::npos);
}

// ============================================================================
// TravelOutcomeReset — 交接已被放行却没收到放行应答时,要不要在销毁实体之前给客户端
// 发失败 tip + 踢线 34(player_lifecycle.h travel_outcome)。纯函数,不需要 Redis / 场景宿主;
// 运行期那道"实体没在退出"的条件和 MGET 胶水不在这里覆盖,要靠真实环境故障注入验证
// (docs/design/cross-zone-scene-travel.md §12)。
// ============================================================================
namespace
{
namespace to = travel_outcome;

constexpr to::Evidence kAllEvidence[] = {
    to::Evidence::kSucceeded,
    to::Evidence::kFailed,
    to::Evidence::kNoReply,
    to::Evidence::kMarkWriteUnknown,
    to::Evidence::kAnomalous,
};
static_assert(std::size(kAllEvidence) == to::kEvidenceCount, "新增证据种类时补进这张表,并补判据用例");

constexpr uint32_t kTargetZone = 2;
constexpr uint64_t kGrantedEpoch = 8;
} // namespace

TEST(TravelOutcomeReset, CrossZoneFailedOrNoReplyResetsClient)
{
    EXPECT_TRUE(to::ShouldResetClientOnGrant(/*crossZone=*/true, to::Evidence::kFailed))
        << "显式失败应答但 epoch 已变:客户端没拿到重定向,不踢就挂在哑连接上";
    EXPECT_TRUE(to::ShouldResetClientOnGrant(/*crossZone=*/true, to::Evidence::kNoReply))
        << "看门狗到期(含 zrpc 超时)是 S3L1-1 的主触发形态";
}

TEST(TravelOutcomeReset, CrossZoneOtherEvidenceDoesNotReset)
{
    EXPECT_FALSE(to::ShouldResetClientOnGrant(/*crossZone=*/true, to::Evidence::kSucceeded));
    EXPECT_FALSE(to::ShouldResetClientOnGrant(/*crossZone=*/true, to::Evidence::kMarkWriteUnknown))
        << "EnterScene 根本没发出去,epoch 的推进不是本次交接造成的";
    EXPECT_FALSE(to::ShouldResetClientOnGrant(/*crossZone=*/true, to::Evidence::kAnomalous))
        << "协议异常应答说明不了客户端没拿到结果";
}

TEST(TravelOutcomeReset, SameZoneNeverResets)
{
    // 同 zone 放行靠 RoutePlayerEvent 改绑同一个会话,"没收到应答"时路由往往已经到了,踢线会断掉合法会话。
    for (const auto evidence : kAllEvidence)
    {
        EXPECT_FALSE(to::ShouldResetClientOnGrant(/*crossZone=*/false, evidence))
            << "evidence=" << to::EvidenceName(evidence);
    }
}

TEST(TravelOutcomeReset, EvidenceNamesCoverEveryValue)
{
    for (const auto evidence : kAllEvidence)
    {
        EXPECT_STRNE(to::EvidenceName(evidence), "?");
    }
    EXPECT_STREQ(to::EvidenceName(to::Evidence::kCount), "?") << "越界不能读出数组外";
}

TEST(TravelOutcomeReset, AwaitingPlacementOfThisHandoff)
{
    EXPECT_TRUE(to::IsAwaitingPlacementOfHandoff(/*locationNodeEmpty=*/true, kTargetZone, kGrantedEpoch,
                                                 kTargetZone, kGrantedEpoch))
        << "第一条腿写下的等待落点:无节点、目标 zone、location 里的 epoch 就是当前 owner_epoch";
}

TEST(TravelOutcomeReset, NotAwaitingPlacementOfThisHandoff)
{
    EXPECT_FALSE(to::IsAwaitingPlacementOfHandoff(/*locationNodeEmpty=*/false, kTargetZone, kGrantedEpoch,
                                                  kTargetZone, kGrantedEpoch))
        << "已落到某个节点上(第二条腿已落地,或同会话被别的请求改绑):不踢";
    EXPECT_FALSE(to::IsAwaitingPlacementOfHandoff(/*locationNodeEmpty=*/true, /*locationZoneId=*/1, kGrantedEpoch,
                                                  kTargetZone, kGrantedEpoch))
        << "等待落点在别的 zone:不是本次交接写的";
    EXPECT_FALSE(to::IsAwaitingPlacementOfHandoff(/*locationNodeEmpty=*/true, kTargetZone, kGrantedEpoch - 1,
                                                  kTargetZone, kGrantedEpoch))
        << "location 的 epoch 与同一时刻读到的 owner_epoch 不一致:epoch 是被别的请求推进的";
    EXPECT_FALSE(to::IsAwaitingPlacementOfHandoff(/*locationNodeEmpty=*/true, kTargetZone, /*locationOwnerEpoch=*/0,
                                                  kTargetZone, /*redisOwnerEpoch=*/0))
        << "epoch 为 0(从未铸造)不可能是放行后的等待落点,fail-closed";
    EXPECT_FALSE(to::IsAwaitingPlacementOfHandoff(/*locationNodeEmpty=*/true, /*locationZoneId=*/0, kGrantedEpoch,
                                                  /*targetZoneId=*/0, kGrantedEpoch))
        << "目标 zone 为 0 是非法交接,不据此踢线";
}

TEST(TravelOutcomeReset, RecordedEvidenceOverridesWatchdogNoReply)
{
    // 应答已到、因 Redis 不可用没能裁决,首次挂的 kNoReply 看门狗(不取消)先到期:
    // 裁决必须用组件上记下的应答证据,不能被盖成超时。
    for (const auto recorded : kAllEvidence)
    {
        EXPECT_EQ(to::EffectiveEvidence(/*hasRecorded=*/true, static_cast<uint8_t>(recorded), to::Evidence::kNoReply),
                  recorded)
            << "recorded=" << to::EvidenceName(recorded);
    }
    // 同 zone 成功被盖成 kNoReply 会补假失败 tip;跨 zone 协议异常被盖成 kNoReply 会被踢线。
    EXPECT_FALSE(to::ShouldResetClientOnGrant(
        /*crossZone=*/true,
        to::EffectiveEvidence(true, static_cast<uint8_t>(to::Evidence::kAnomalous), to::Evidence::kNoReply)));
}

TEST(TravelOutcomeReset, NoRecordedEvidenceKeepsIncoming)
{
    for (const auto incoming : kAllEvidence)
    {
        EXPECT_EQ(to::EffectiveEvidence(/*hasRecorded=*/false, /*recorded=*/0, incoming), incoming)
            << "incoming=" << to::EvidenceName(incoming);
    }
    EXPECT_EQ(to::EffectiveEvidence(/*hasRecorded=*/true, static_cast<uint8_t>(to::kEvidenceCount),
                                    to::Evidence::kNoReply),
              to::Evidence::kNoReply)
        << "记下的底层值越界视同没记,不得转换成非法枚举";
}

// ============================================================================
// 9. ExitPersist —— Z1 修复(cross-zone-scene-travel.md §12.6.3 第一步):
//    退出意图组件的纯判据 + HandlePlayerAsyncSaved 退出分支的 ECS 级回归(M15)
// ============================================================================
#include "services/scene/player/system/player_exit_intent.h" // 与 player_lifecycle.h 同一写法
#include "player/comp/player_ownership_comp.h"                  // PlayerOwnerEpochComp
#include "proto/common/component/actor_comp.pb.h"            // Velocity
#include <limits>
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/system/player_data_loader.h"
#include "player/system/player_persist_marshal.h"
#include "player/comp/player_persist_carry_comp.h"
#include "player/comp/player_persist_writer_comp.h" // 严格屏障写方令牌(混版本持久字段保护 §5.2)
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/mission/comp/mission_comp.h"
#include "proto/common/component/asset_op_ledger_comp.pb.h"
#include "proto/common/component/battle_settlement_ledger_comp.pb.h"
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/common/component/player_mystery_merchant_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"         // UnregisterPlayer
#include "proto/common/component/player_network_comp.pb.h" // PlayerSessionSnapshotComp
#include "type_alias/player_session_type_alias.h"          // SessionMap
#include "muduo/base/Logging.h"                            // Logger::setOutput:数 WARN 条数
#include <cstdio>
#include <string_view>

namespace
{
constexpr SessionId kSessionAtExit = 131073; // 取自 Z1 实跑日志里被误判的退出会话号
constexpr SessionId kNewerSession = 131074;

constexpr ExitCause kAllExitCauses[] = {
    ExitCause::kUnspecified,
    ExitCause::kClientDisconnect,
    ExitCause::kNodeShutdown,
    ExitCause::kSceneDrain,
    ExitCause::kIdentityConflict,
    ExitCause::kReleasedByTransfer,
    ExitCause::kRoleTradeFrozen,
};
static_assert(std::size(kAllExitCauses) == player_exit::kExitCauseCount, "新增退出原因时补进这张表,并补 Merge 用例");
} // namespace

TEST(ExitPersistSession, SameSessionIsNotSuperseding)
{
    // Z1 的回归用例:HandleExitGameNode 不解绑会话,退出会话本身一直留在 SessionMap 里、映射到本玩家。
    // 旧判据把它当成"重连已取代退出",实体成了僵尸。
    EXPECT_FALSE(player_exit::IsSupersedingSession(kSessionAtExit, kSessionAtExit, /*currentMappedToPlayer=*/true));
}

TEST(ExitPersistSession, NewerMappedSessionIsSuperseding)
{
    EXPECT_TRUE(player_exit::IsSupersedingSession(kNewerSession, kSessionAtExit, /*currentMappedToPlayer=*/true));
    EXPECT_TRUE(player_exit::IsSupersedingSession(kNewerSession, kInvalidSessionId, true))
        << "退出时没有会话、之后绑上了一个映射到本玩家的会话:算取代";
}

TEST(ExitPersistSession, UnmappedOrUnboundIsNotSuperseding)
{
    EXPECT_FALSE(player_exit::IsSupersedingSession(kNewerSession, kSessionAtExit, /*currentMappedToPlayer=*/false))
        << "SessionMap 里没有 / 映射到别的玩家:不是本玩家的活会话";
    EXPECT_FALSE(player_exit::IsSupersedingSession(kInvalidSessionId, kSessionAtExit, true));
    EXPECT_FALSE(player_exit::IsSupersedingSession(0, kSessionAtExit, true)) << "0 同样表示没有会话";
}

TEST(ExitPersistCause, NamesCoverEveryValue)
{
    for (const auto cause : kAllExitCauses)
    {
        EXPECT_STRNE(player_exit::ExitCauseName(cause), "?");
    }
    EXPECT_STREQ(player_exit::ExitCauseName(ExitCause::kCount), "?") << "越界不能读出数组外";
}

TEST(ExitPersistCause, MergeSuppressesOnlyForIdentityConflictUnspecifiedOrRoleTradeFrozen)
{
    for (const auto first : kAllExitCauses)
    {
        for (const auto incoming : kAllExitCauses)
        {
            PlayerExitIntentComp intent;
            intent.cause = first;
            player_exit::MergeExitCause(intent, incoming, /*devBypassSuppressesTransfer=*/false);
            const bool expectSuppressed =
                incoming == ExitCause::kIdentityConflict || incoming == ExitCause::kUnspecified ||
                incoming == ExitCause::kRoleTradeFrozen;
            EXPECT_EQ(intent.releaseMarkSuppressed, expectSuppressed)
                << "first=" << player_exit::ExitCauseName(first) << " incoming=" << player_exit::ExitCauseName(incoming);
            EXPECT_EQ(intent.cause, first) << "合并不改第一次的原因";
        }
    }
}

TEST(ExitPersistCause, ReleasedByTransferSuppressesOnlyUnderDevBypass)
{
    PlayerExitIntentComp production;
    production.cause = ExitCause::kClientDisconnect;
    player_exit::MergeExitCause(production, ExitCause::kReleasedByTransfer, /*devBypassSuppressesTransfer=*/false);
    EXPECT_FALSE(production.releaseMarkSuppressed) << "生产口径不压制,由 A1′ 的 owner_epoch 条件把关(M6)";

    PlayerExitIntentComp devBypass;
    devBypass.cause = ExitCause::kClientDisconnect;
    player_exit::MergeExitCause(devBypass, ExitCause::kReleasedByTransfer, /*devBypassSuppressesTransfer=*/true);
    EXPECT_TRUE(devBypass.releaseMarkSuppressed);
}

TEST(ExitPersistCause, RoleTradeFrozenSuppressesReleaseAndPreservesFirstCause)
{
    for (const bool devBypass : {false, true})
    {
        for (const auto first : kAllExitCauses)
        {
            PlayerExitIntentComp intent;
            intent.cause = first;
            player_exit::MergeExitCause(intent, ExitCause::kRoleTradeFrozen, devBypass);
            ASSERT_TRUE(intent.releaseMarkSuppressed);
            EXPECT_EQ(intent.suppressedBy, ExitCause::kRoleTradeFrozen);
            EXPECT_EQ(intent.cause, first) << "交易冻结不能改写第一次退出的原因";
            EXPECT_FALSE(intent.clientDisconnected) << "交易冻结不是客户端断线";

            player_exit::MergeExitCause(intent, ExitCause::kIdentityConflict, devBypass);
            player_exit::MergeExitCause(intent, ExitCause::kClientDisconnect, devBypass);
            EXPECT_TRUE(intent.releaseMarkSuppressed) << "后续退出不能恢复冻结角色的释放标记";
            EXPECT_EQ(intent.suppressedBy, ExitCause::kRoleTradeFrozen) << "保留第一次压制来源";
            EXPECT_EQ(intent.cause, first);
            EXPECT_TRUE(intent.clientDisconnected) << "后续真实断线仍须记录";
        }
    }

    PlayerExitIntentComp alreadySuppressed;
    alreadySuppressed.cause = ExitCause::kClientDisconnect;
    player_exit::MergeExitCause(alreadySuppressed, ExitCause::kIdentityConflict, false);
    player_exit::MergeExitCause(alreadySuppressed, ExitCause::kRoleTradeFrozen, false);
    EXPECT_TRUE(alreadySuppressed.releaseMarkSuppressed);
    EXPECT_EQ(alreadySuppressed.suppressedBy, ExitCause::kIdentityConflict);
    EXPECT_EQ(alreadySuppressed.cause, ExitCause::kClientDisconnect);
}

TEST(ExitPersistCause, SuppressionIsSticky)
{
    PlayerExitIntentComp intent;
    intent.cause = ExitCause::kClientDisconnect;
    player_exit::MergeExitCause(intent, ExitCause::kIdentityConflict, false);
    ASSERT_TRUE(intent.releaseMarkSuppressed);
    player_exit::MergeExitCause(intent, ExitCause::kNodeShutdown, false);
    EXPECT_TRUE(intent.releaseMarkSuppressed) << "置位后不再清";
    EXPECT_TRUE(player_exit::ShouldSuppressReleaseMarkOnMerge(ExitCause::kCount, false)) << "越界按来源不明处理";
}

TEST(ExitPersistDecision, FinishOnlyWhenCurrentAndSettled)
{
    using D = player_exit::AfterPersistDecision;
    constexpr uint8_t kMax = player_exit::kMaxExitResaveRounds;
    EXPECT_EQ(player_exit::DecideAfterPersist(true, false, 0, kMax), D::kFinish);
    EXPECT_EQ(player_exit::DecideAfterPersist(true, false, kMax, kMax), D::kFinish)
        << "超限之后的落地只要收敛了照样收尾";
    EXPECT_EQ(player_exit::DecideAfterPersist(false, false, 0, kMax), D::kResave);
    EXPECT_EQ(player_exit::DecideAfterPersist(true, true, 0, kMax), D::kResave) << "还有未落地的存盘不能销毁(M4)";
    EXPECT_EQ(player_exit::DecideAfterPersist(false, false, static_cast<uint8_t>(kMax - 1), kMax), D::kResave);
    EXPECT_EQ(player_exit::DecideAfterPersist(false, false, kMax, kMax), D::kRetainExhausted);
    EXPECT_EQ(player_exit::DecideAfterPersist(true, true, kMax, kMax), D::kRetainExhausted);
}

TEST(ExitPersistDecision, RekickOnlyWhenExhaustedAndSettled)
{
    constexpr uint8_t kMax = player_exit::kMaxExitResaveRounds;
    EXPECT_TRUE(player_exit::ShouldRekickExhaustedExit(kMax, kMax, /*hasUnsettledSave=*/false));
    EXPECT_FALSE(player_exit::ShouldRekickExhaustedExit(kMax, kMax, true)) << "有在途存盘就等它落地";
    EXPECT_FALSE(player_exit::ShouldRekickExhaustedExit(0, kMax, false)) << "没到上限 = 退出分支自己的重存还在途";
    EXPECT_FALSE(player_exit::ShouldRekickExhaustedExit(static_cast<uint8_t>(kMax - 1), kMax, false));
}

TEST(ExitPersistReentry, DeposedOnlyWhenEpochJumpsByAtLeastTwo)
{
    EXPECT_FALSE(player_exit::IsDeposedOnReentry(5, 0)) << "上游未填,无从判断";
    EXPECT_FALSE(player_exit::IsDeposedOnReentry(5, 4)) << "乱序的旧路由";
    EXPECT_FALSE(player_exit::IsDeposedOnReentry(5, 5)) << "同节点重连不铸造";
    EXPECT_FALSE(player_exit::IsDeposedOnReentry(5, 6)) << "落回本节点这一次自己铸的,内存仍是真身";
    EXPECT_TRUE(player_exit::IsDeposedOnReentry(5, 7)) << "别处落点铸一次 + 回到本节点再铸一次";
    EXPECT_FALSE(player_exit::IsDeposedOnReentry(0, 1)) << "缓存 0 = 未知,无从判断";
    EXPECT_FALSE(player_exit::IsDeposedOnReentry(0, 5))
        << "缓存 0 = 未知(兼容窗口经旧 gate 首登),Redis 真实值可能早已是 5:不能拿 0 当基数判被废黜";
    EXPECT_FALSE(player_exit::IsDeposedOnReentry((std::numeric_limits<uint64_t>::max)(), 1))
        << "不新就不废黜,减法不得回绕";
}

TEST(ExitPersistPayload, ProbeFieldsAreIgnoredBusinessFieldsAreNot)
{
    PlayerAllData current;
    current.mutable_player_database_data()->set_player_id(7);
    current.mutable_player_database_1_data()->set_player_id(7);
    current.mutable_player_database_data()->mutable_level_component()->set_level(10);

    PlayerAllData landed = current;
    auto* probe = landed.mutable_player_database_data()->mutable_stress_test_probe();
    probe->set_test_seq(42);
    probe->set_test_sig(std::string(16, '\x5a'));
    landed.mutable_player_database_1_data()->mutable_stress_test_probe()->set_test_seq(43);
    EXPECT_TRUE(player_exit::IsPersistedPayloadCurrent(landed, current))
        << "压测探针只打在写盘的那份上,不剔除的话 STRESS_TEST_PROBE 构建退出永不收敛";

    current.mutable_player_database_data()->mutable_level_component()->set_level(11);
    EXPECT_FALSE(player_exit::IsPersistedPayloadCurrent(landed, current)) << "业务字段不同必须判不一致";
}

// ── ECS 级回归(M15):直接驱动 PlayerLifecycleSystem::HandlePlayerAsyncSaved ──
// 宿主里没有初始化 RedisSystem(GetPlayerDataRedis() 为空):"未落地存盘"查询如实返回 false,
// 且下面的用例都不会走到重存(SavePlayerToRedis 需要真的 Redis 客户端)。
namespace
{
constexpr Guid kExitPlayerId = 950200001;

struct ScopedPersistedPlayerCleanup
{
    ~ScopedPersistedPlayerCleanup()
    {
        tlsEcs.Clear();
        SessionMap().clear();
    }
};

// 本宿主不加载配表,在测试创建点显式构造"加载已完成"的持久组件集合。
// 不让只读 marshal 为夹具补组件,同时避免把属性/任务加载副作用引入退出接缝测试。
void CreatePersistedPlayerComps(entt::entity player)
{
    auto& registry = tlsEcs.actorRegistry;
    registry.emplace<Transform>(player);
    registry.emplace<PlayerUint64Comp>(player);
    registry.emplace<PlayerSkillListComp>(player);
    registry.emplace<PlayerUint32Comp>(player);
    registry.emplace<BaseAttributesComp>(player);
    registry.emplace<LevelComp>(player);
    registry.emplace<PlayerAttributeComp>(player);
    registry.emplace<PlayerPetComp>(player);
    registry.emplace<PlayerProfileComp>(player);
    registry.emplace<PlayerBagsComp>(player);
    registry.emplace<MissionsContainerComp>(player);
    registry.emplace<PlayerAssetOpLedgerComp>(player);
    registry.emplace<BattleSettlementLedgerComp>(player);
    registry.emplace<PlayerInboxLedgerComp>(player);
    registry.emplace<PlayerMysteryMerchantComp>(player);
    registry.emplace<CurrencyComp>(player);
    registry.emplace<PlayerCurrencyComp>(player);
    // 两表顶层未知字段的载体(混版本持久字段保护 §4.2 L1),生产里由两张表的 Unmarshal 创建。
    registry.emplace<PlayerDatabaseCarryComp>(player);
    registry.emplace<PlayerDatabase1CarryComp>(player);
}

// 构造一个"退出存盘在途"的玩家:当前绑定 currentSession(已写进 SessionMap、映射到本玩家)、
// 挂 UnregisterPlayer + 退出意图(sessionAtExit)。logout_initiated_ms 留 0 = 跳过租约告警。
entt::entity MakeExitingPlayer(SessionId sessionAtExit, SessionId currentSession, bool withIntent = true)
{
    tlsEcs.Clear();
    SessionMap().clear(); // 随 tlsEcs.Clear() 一起清理;此处显式强调测试的空会话前置条件。
    const auto player = tlsEcs.actorRegistry.create();
    CreatePersistedPlayerComps(player);
    tlsEcs.playerList.emplace(kExitPlayerId, player);
    tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kExitPlayerId);
    auto& session = tlsEcs.actorRegistry.emplace<PlayerSessionSnapshotComp>(player);
    session.set_gate_session_id(currentSession);
    session.set_player_id(kExitPlayerId);
    SessionMap().insert_or_assign(currentSession, kExitPlayerId);
    tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
    if (withIntent)
    {
        PlayerExitIntentComp intent;
        intent.sessionAtExit = sessionAtExit;
        intent.cause = ExitCause::kClientDisconnect;
        tlsEcs.actorRegistry.emplace<PlayerExitIntentComp>(player, intent);
    }
    return player;
}

// 合法的新号父快照与login种子一致；让生命周期测试到达各自的继承/会话围栏。
PlayerAllData IdentifiedPlayerLoadData(Guid playerId)
{
    PlayerAllData data;
    data.mutable_player_database_data()->set_player_id(playerId);
    data.mutable_player_database_1_data()->set_player_id(playerId);
    return data;
}

// 与 SavePlayerToRedis 写盘时同一个形状:走同一个统一入口(拼装 + 两张子表的 player_id + 持久 schema 世代)。
bool MarshalAsSaved(entt::entity player, PlayerAllData& data)
{
    return player_persist::MarshalPlayerForPersist(player, kExitPlayerId, data);
}
} // namespace

TEST(ExitPersistEcs, ExitSessionStillMappedIsDestroyed)
{
    // Z1:退出会话仍在 SessionMap 里、映射到本玩家(HandleExitGameNode 不解绑会话),落地内容就是当前内存。
    // 修复前这里判"被取代"、摘标记后 return,实体成僵尸。
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(player, landed));

    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player)) << "真写盘的正常断线退出必须销毁实体";
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u) << "退出会话的映射随收尾一起摘掉";
}

TEST(ExitPersistEcs, LandedPayloadWithProbeStillConverges)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(player, landed));
    landed.mutable_player_database_data()->mutable_stress_test_probe()->set_test_seq(1);
    landed.mutable_player_database_1_data()->mutable_stress_test_probe()->set_test_seq(1);

    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
}

TEST(ExitPersistEcs, NewerSessionKeepsEntityAndClearsExitMarkers)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kNewerSession);
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(player, landed));

    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "被更新的会话取代:保留实体";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerExitIntentComp>(player)) << "与 UnregisterPlayer 成对摘";
    const auto* snapshot = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
    ASSERT_NE(snapshot, nullptr) << "这份字节确实落了盘,快照照常更新";
    EXPECT_TRUE(snapshot->HasSnapshot());
}

namespace
{
// 数 muduo 日志里 "save outran reconnect lease" 出现的条数(帮会值班 LogQL 按这段原文计事件数,§12.6.9)。
int g_leaseOverrunWarnLines = 0;
int g_handoffPrecheckRefusedLines = 0;
int g_handoffMarkComponentRefusedLines = 0;
int g_handoffMarkRedisDisconnectedLines = 0;

void CountHandoffPrecheckLines(const char* msg, int len)
{
    const std::string_view line(msg, static_cast<std::size_t>(len));
    if (line.find("handoff precheck refused") != std::string_view::npos)
    {
        ++g_handoffPrecheckRefusedLines;
    }
    if (line.find(
            "persisted component missing before writing the handoff mark") != std::string_view::npos)
    {
        ++g_handoffMarkComponentRefusedLines;
    }
    if (line.find("zone redis not connected") != std::string_view::npos)
    {
        ++g_handoffMarkRedisDisconnectedLines;
    }
    std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
}

void CountLeaseOverrunWarn(const char* msg, int len)
{
    if (std::string_view(msg, static_cast<std::size_t>(len)).find("save outran reconnect lease") !=
        std::string_view::npos)
    {
        ++g_leaseOverrunWarnLines;
    }
    std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
}

void WriteLogToStdout(const char* msg, int len)
{
    std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
}
} // namespace

TEST(ExitPersistEcs, LeaseOverrunWarnIsLoggedOncePerExit)
{
    // 复审 liveness-minor:一次退出可能落地多次,"save outran reconnect lease" 只许每次退出打一条(§12.6.9)。
    // 走"超限保留"分支:不碰 Redis、实体与意图组件都留着,同一次退出可以连续落地两次。
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    tlsEcs.actorRegistry.get<UnregisterPlayer>(player).set_logout_initiated_ms(1); // 远早于租约
    tlsEcs.actorRegistry.get<PlayerExitIntentComp>(player).resaveRounds = player_exit::kMaxExitResaveRounds;
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(player, landed));
    landed.mutable_player_database_data()->mutable_level_component()->set_level(999); // 不收敛

    g_leaseOverrunWarnLines = 0;
    muduo::Logger::setOutput(CountLeaseOverrunWarn);
    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);
    const int afterFirst = g_leaseOverrunWarnLines;
    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed); // 同一次退出的第二次落地
    const int afterSecond = g_leaseOverrunWarnLines;
    muduo::Logger::setOutput(WriteLogToStdout);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "不收敛且已到上限:保留实体";
    const auto* intent = tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(player);
    ASSERT_NE(intent, nullptr);
    EXPECT_TRUE(intent->leaseOverrunWarned);
    EXPECT_EQ(afterFirst, 1) << "第一次超租约落地打一条,原文不变";
    EXPECT_EQ(afterSecond, 1) << "同一次退出的后续落地不再打";
}

TEST(ExitPersistEcs, MissingIntentFailsClosed)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit, /*withIntent=*/false);
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(player, landed));

    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "意图缺失不销毁(M9 fail-closed)";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player));
}

// ── 退出入口(HandleExitGameNode)与重连取消(CancelExitOnReconnect)的接缝 ──
// 宿主里没有 Redis:只能走不写盘的路径,所以用"快照 = 当前 marshal"让 SavePlayerToRedis 走快路径(返回 false)。
namespace
{
// 一个在线(未退出)的玩家:绑定 session、写进 SessionMap。
entt::entity MakeOnlinePlayer(SessionId session)
{
    tlsEcs.Clear();
    SessionMap().clear();
    const auto player = tlsEcs.actorRegistry.create();
    CreatePersistedPlayerComps(player);
    tlsEcs.playerList.emplace(kExitPlayerId, player);
    tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kExitPlayerId);
    auto& snapshot = tlsEcs.actorRegistry.emplace<PlayerSessionSnapshotComp>(player);
    snapshot.set_gate_session_id(session);
    snapshot.set_player_id(kExitPlayerId);
    SessionMap().insert_or_assign(session, kExitPlayerId);
    return player;
}

// 让下一次 SavePlayerToRedis 判"盘上已是同一份"(快路径,不碰 Redis)。
bool MarkPersisted(entt::entity player)
{
    PlayerAllData saved;
    if (!MarshalAsSaved(player, saved))
    {
        return false;
    }
    auto& snapshot = tlsEcs.actorRegistry.emplace_or_replace<PlayerLastPersistedSnapshotComp>(player);
    const auto* owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
    if (owner == nullptr || owner->epoch == 0)
    {
        snapshot.Replace(saved); // 旧无围栏宿主保留原兼容合同。
        return true;
    }
    // 严格宿主显式模拟原不可变Redis提交：不能用仅Replace的业务字节冒同E/q资格，更不伪SQL ACK。
    const auto identity = RoleAdmissionSystem::ConfirmedIdentity(kExitPlayerId);
    if (!identity || identity->scene_instance_id().empty()) return false;
    auto& admission = tlsEcs.actorRegistry.get_or_emplace<PlayerRoleAdmissionComp>(player,
        identity->generation(), identity->owner_revision());
    if (admission.generation != identity->generation() || admission.ownerRevision != identity->owner_revision()) return false;
    saved.set_last_save_seq(1);
    const auto source = std::make_shared<const PlayerAllData>(saved);
    const auto committed = RoleAdmissionSystem::CaptureSnapshotCommit(player, source, owner->epoch, 1);
    if (!committed) return false;
    committed(); // 与正式Redis成功hook相同，先钩子，再同一个payload的saved回调。
    snapshot.Replace(*source);
    return snapshot.committed_owner_epoch == owner->epoch && snapshot.committed_save_seq == 1;
}
} // namespace

TEST(ExitPersistEcs, StopMotionForExitZeroesKinematics)
{
    tlsEcs.Clear();
    const auto player = tlsEcs.actorRegistry.create();
    auto& velocity = tlsEcs.actorRegistry.emplace<Velocity>(player);
    velocity.set_x(3.0);
    velocity.set_y(-1.5);
    velocity.set_z(0.25);
    PlayerLifecycleSystem::StopMotionForExit(player);

    const auto& v = tlsEcs.actorRegistry.get<Velocity>(player);
    EXPECT_EQ(v.x(), 0.0);
    EXPECT_EQ(v.y(), 0.0);
    EXPECT_EQ(v.z(), 0.0);
    const auto bare = tlsEcs.actorRegistry.create();
    PlayerLifecycleSystem::StopMotionForExit(bare);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<Velocity>(bare)) << "只改已有组件,不 emplace";
}

TEST(ExitPersistEcs, FirstExitOnFastPathFinishesInline)
{
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kClientDisconnect);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player)) << "盘上已是当前内存、没有未落地存盘:同步收尾";
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
}

TEST(ExitPersistEcs, ReexitMergesCauseWithoutFinishing)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kIdentityConflict);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "轮次没到上限 = 退出存盘仍在途,再来一次退出不插手";
    const auto& intent = tlsEcs.actorRegistry.get<PlayerExitIntentComp>(player);
    EXPECT_EQ(intent.cause, ExitCause::kClientDisconnect) << "原因保持第一次的";
    EXPECT_TRUE(intent.releaseMarkSuppressed);
    EXPECT_EQ(intent.sessionAtExit, kSessionAtExit) << "不重挂,不改退出那一刻的会话";
}

TEST(ExitPersistEcs, ReexitAfterExhaustedRekicksAndConverges)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    tlsEcs.actorRegistry.get<PlayerExitIntentComp>(player).resaveRounds = player_exit::kMaxExitResaveRounds;
    ASSERT_TRUE(MarkPersisted(player));

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kNodeShutdown);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player))
        << "超限保留的实体再收到退出请求:补发的存盘判盘上已是同一份、且没有未落地存盘 → 收尾(停机时的最后出口)";
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
}

// 永久拒存不能收敛:按负责人决定不存盘丢弃,不让重连复用无法再保存的实体。
TEST(ExitPersistEcs, RejectedExitIsDiscardedNotReused)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    const auto before = exit_release_stats::Read();
    const auto marksBefore = PlayerLifecycleSystem::ExitReleaseMarksInFlight();
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kClientDisconnect);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), entt::entity{entt::null});
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
    EXPECT_EQ(exit_release_stats::Read().decisions, before.decisions) << "不进入 A1′ 释放标记判定";
    EXPECT_EQ(PlayerLifecycleSystem::ExitReleaseMarksInFlight(), marksBefore);
}

TEST(ExitPersistEcs, MissingPersistedComponentDiscardsWithoutReleaseMark)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    const auto before = exit_release_stats::Read();
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kClientDisconnect);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), entt::entity{entt::null});
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
    EXPECT_EQ(exit_release_stats::Read().decisions, before.decisions);
}

TEST(ExitPersistEcs, ExhaustedRekickWithMissingComponentDiscardsWithoutReleaseMark)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    const auto before = exit_release_stats::Read();
    tlsEcs.actorRegistry.get<PlayerExitIntentComp>(player).resaveRounds = player_exit::kMaxExitResaveRounds;
    tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player);

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kIdentityConflict);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), entt::entity{entt::null});
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
    EXPECT_EQ(exit_release_stats::Read().decisions, before.decisions);
}

TEST(ExitPersistEcs, LandedCallbackWithMissingComponentDiscardsWithoutReleaseMark)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(player, landed));
    const auto before = exit_release_stats::Read();
    tlsEcs.actorRegistry.remove<BattleSettlementLedgerComp>(player);

    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), entt::entity{entt::null});
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
    EXPECT_EQ(exit_release_stats::Read().decisions, before.decisions);
}

TEST(ExitPersistEcs, StaleEpochResaveRejectedDiscardsWithoutReleaseMark)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    const auto before = exit_release_stats::Read();
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 7;
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);

    PlayerLifecycleSystem::HandlePlayerSaveRejected(kExitPlayerId, "player-save-test", "6", RedisSaveOwnerRejection{});

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), entt::entity{entt::null});
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
    EXPECT_EQ(exit_release_stats::Read().decisions, before.decisions);
}

TEST(ExitPersistEcs, IssuedHandoffWithMissingPersistedComponentDiscardsWithoutReleaseMark)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    const auto before = exit_release_stats::Read();
    const auto travelBefore = travel_handoff_stats::Read();
    auto& travel = tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
    travel.requestedAtMs = 123;
    travel.targetZoneId = 8;
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kClientDisconnect);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), entt::entity{entt::null});
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
    EXPECT_EQ(exit_release_stats::Read().decisions, before.decisions);
    EXPECT_EQ(travel_handoff_stats::Read().exitWins, travelBefore.exitWins)
        << "不走退出优先撤标记或改派链";
}

TEST(ExitPersistEcs, StaleEpochRejectOnOnlinePlayerKeepsEntity)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 7;
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);

    PlayerLifecycleSystem::HandlePlayerSaveRejected(kExitPlayerId, "player-save-test", "6", RedisSaveOwnerRejection{});

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), player);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 1u);
}

// ── 严格屏障覆盖世代校验 C3 的结局分派(docs/design/mixed-version-persist-field-protection.md §5.2、§10)──────────

// enforce 拒写发生在退出存盘上:不会再有落地回调,必须不存盘收尾,否则退出永远收不了尾。
TEST(ExitPersistEcs, SchemaRejectOnExitingPlayerDiscardsWithoutReleaseMark)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    const auto before = exit_release_stats::Read();
    const auto schemaBefore = persist_schema_stats::Read();

    PlayerLifecycleSystem::HandlePlayerSaveSchemaRejected(kExitPlayerId, "player-save-test", "6");

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), entt::entity{entt::null});
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
    EXPECT_EQ(exit_release_stats::Read().decisions, before.decisions) << "不写释放标记、不发改派";
    EXPECT_EQ(persist_schema_stats::Read().regressionRejected, schemaBefore.regressionRejected + 1);
}

// 即使标记未发，旧登记/SET也可能仍在网络中；必须确认撤销原资格才解冻，断连不能猜已取消。
TEST(ExitPersistEcs, SchemaRejectBeforeMarkWaitsForOriginalQualificationRevocation)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    auto& holder = tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player);
    holder.epoch = 7;
    holder.handoffFence = "7|explicit-test-scene|1|" + std::string(32, 'a') + "|H|" + std::string(32, 'b');
    ASSERT_TRUE(MarkPersisted(player));
    const auto travelBefore = travel_handoff_stats::Read();
    auto& travel = tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
    travel.holderFence = holder.handoffFence; // 本次冻结时缓存的原资格，不从回调fresh补取。
    travel.requestedAtMs = 0;
    travel.targetZoneId = 8;
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);

    PlayerLifecycleSystem::HandlePlayerSaveSchemaRejected(kExitPlayerId, "player-save-test", "6");

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player)) << "原资格撤销未知，不能因SET尚未发送解冻";
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), player);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 1u);
    EXPECT_EQ(travel_handoff_stats::Read().aborted, travelBefore.aborted);
    const auto& pending = tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(player);
    ASSERT_FALSE(pending.cancelFence.empty());
    const auto originalCancel = pending.cancelFence;
    const auto candidate = handoff_sql_barrier::ParseFence(originalCancel);
    ASSERT_TRUE(candidate.has_value());
    EXPECT_EQ(candidate->state, handoff_sql_barrier::FenceState::kHeld);
    EXPECT_EQ(pending.holderFence, holder.handoffFence);
    EXPECT_NE(originalCancel, holder.handoffFence);
    PlayerLifecycleSystem::HandlePlayerSaveSchemaRejected(kExitPlayerId, "player-save-test", "6");
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(player).cancelFence, originalCancel)
        << "撤销未知重试固定原候选，不换nonce、不借新资格";
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
}

// 在线:保留实体与会话,本次不落盘;不能按"已废黜"销毁(那是 owner CAS 拒绝的语义)。
TEST(ExitPersistEcs, SchemaRejectOnOnlinePlayerKeepsEntity)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    const auto ownerBefore = owner_epoch_stats::Read();

    PlayerLifecycleSystem::HandlePlayerSaveSchemaRejected(kExitPlayerId, "player-save-test", "6");

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), player);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 1u);
    EXPECT_EQ(owner_epoch_stats::Read().staleOwnerWriteRejected, ownerBefore.staleOwnerWriteRejected)
        << "C3 拒写不计入双主证据";
}

// warn 照写只计数,不改实体状态;同一写方之后每次写入都会再返回 2(证据留在 Redis,防回包丢失),按写方令牌只记一次;
// 令牌对不上(另一个加载实体留下的在途写)照常计数;实体已不在时两种回调都是空操作。
TEST(ExitPersistEcs, SchemaWarnOnlyCountsAndGonePlayerIsNoop)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    // 令牌由存盘写入口懒建;能返回 2 的实体必然已有。这里在测试创建点显式挂上。
    tlsEcs.actorRegistry.emplace_or_replace<PlayerPersistWriterComp>(player).token = "writer-of-this-entity";
    const auto schemaBefore = persist_schema_stats::Read();

    PlayerLifecycleSystem::HandlePlayerSaveSchemaWarned(kExitPlayerId, "player-save-test", "writer-of-this-entity");

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "warn 已照写,退出照常等落地回调收尾";
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player));
    EXPECT_EQ(persist_schema_stats::Read().regressionWarned, schemaBefore.regressionWarned + 1);

    PlayerLifecycleSystem::HandlePlayerSaveSchemaWarned(kExitPlayerId, "player-save-test", "writer-of-this-entity");
    EXPECT_EQ(persist_schema_stats::Read().regressionWarned, schemaBefore.regressionWarned + 1)
        << "同一写方后续写入带回的旧证据只记一次";
    PlayerLifecycleSystem::HandlePlayerSaveSchemaWarned(kExitPlayerId, "player-save-test", "writer-of-another-entity");
    EXPECT_EQ(persist_schema_stats::Read().regressionWarned, schemaBefore.regressionWarned + 2)
        << "另一个加载实体的在途写照常计数,不被当前实体的去重标记吞掉";
    EXPECT_TRUE(tlsEcs.actorRegistry.get<PlayerPersistWriterComp>(player).regressionWarnReported);

    constexpr Guid kGonePlayerId = 987650001;
    ASSERT_EQ(tlsEcs.GetPlayer(kGonePlayerId), entt::entity{entt::null});
    PlayerLifecycleSystem::HandlePlayerSaveSchemaRejected(kGonePlayerId, "player-save-test", "6");
    EXPECT_EQ(persist_schema_stats::Read().regressionRejected, schemaBefore.regressionRejected + 1);
}

// 写方令牌"一个加载实体一个、生命期内不变"(设计 §5.2):读路径不建;同一实体两次取相同;换 owner 代次(E→E+1 复用实体)
// 不变;同一玩家重新加载成新实体(entt 实体号可能被回收复用)必须不同;形状满足严格 Lua 的 ARGV[11] 校验。
TEST(ExitPersistEcs, PersistWriterTokenIsPerLoadedEntity)
{
    ScopedPersistedPlayerCleanup cleanup;
    const auto first = MakeOnlinePlayer(kSessionAtExit);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerPersistWriterComp>(first)) << "令牌只在存盘写入口懒建";
    const std::string tokenA = player_persist::WriterTokenForSave(first);
    EXPECT_TRUE(IsValidRedisSaveSchemaWriter(tokenA)) << tokenA;
    EXPECT_EQ(tokenA, player_persist::WriterTokenForSave(first));
    tlsEcs.actorRegistry.emplace_or_replace<PlayerOwnerEpochComp>(first).epoch = 7;
    tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(first).epoch = 8;
    EXPECT_EQ(tokenA, player_persist::WriterTokenForSave(first)) << "换 owner 代次不换令牌,本实体的后续写入才能跳过 C3";

    // MakeOnlinePlayer 先 tlsEcs.Clear() 再建实体:同一玩家重新加载,实体号可能复用。
    const auto second = MakeOnlinePlayer(kSessionAtExit);
    const std::string tokenB = player_persist::WriterTokenForSave(second);
    EXPECT_TRUE(IsValidRedisSaveSchemaWriter(tokenB)) << tokenB;
    EXPECT_NE(tokenA, tokenB) << "新加载的实体必须拿新令牌,否则会跳过它接手别人数据时那一次比较";
}

TEST(ExitPersistEcs, CancelExitOnReconnectRemovesBothMarkers)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);

    PlayerLifecycleSystem::CancelExitOnReconnect(player);

    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerExitIntentComp>(player)) << "与 UnregisterPlayer 成对摘";

    // 成对约定被破坏、只剩意图组件时同样收干净。
    tlsEcs.actorRegistry.emplace<PlayerExitIntentComp>(player);
    PlayerLifecycleSystem::CancelExitOnReconnect(player);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerExitIntentComp>(player));
}

TEST(ExitPersistEcs, ReentryDiscardsExitingEntityOnlyWhenDeposed)
{
    {
        const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
        EXPECT_FALSE(PlayerLifecycleSystem::DiscardDeposedEntityOnReentry(player, 6))
            << "恰好 +1:落回本节点这一次自己铸的,照常复用";
        EXPECT_TRUE(tlsEcs.actorRegistry.valid(player));
        EXPECT_TRUE(PlayerLifecycleSystem::DiscardDeposedEntityOnReentry(player, 7));
        EXPECT_FALSE(tlsEcs.actorRegistry.valid(player)) << "中间有别的持有者:不存盘销毁,调用方重载";
        EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    }
    {
        // 复审 ownership-minor:不带 UnregisterPlayer 的活僵尸(意图缺失分支保留的 / gate 崩溃没代发 ExitGame)
        // 同样不得在中间有别的持有者时被复用。
        const auto player = MakeOnlinePlayer(kNewerSession);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
        EXPECT_FALSE(PlayerLifecycleSystem::DiscardDeposedEntityOnReentry(player, 6))
            << "活实体恰好 +1:落回本节点这一次自己铸的,照常复用";
        EXPECT_FALSE(PlayerLifecycleSystem::DiscardDeposedEntityOnReentry(player, 5)) << "同节点重连不铸造";
        EXPECT_TRUE(tlsEcs.actorRegistry.valid(player));
        EXPECT_TRUE(PlayerLifecycleSystem::DiscardDeposedEntityOnReentry(player, 7))
            << "活僵尸遇到 ≥2 跳:中间有别的持有者,不存盘销毁";
        EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
        EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    }
    {
        const auto player = MakeOnlinePlayer(kNewerSession);
        EXPECT_FALSE(PlayerLifecycleSystem::DiscardDeposedEntityOnReentry(player, 9))
            << "缓存 epoch 缺失(按 0 = 未知):无从判断,照常复用";
        EXPECT_TRUE(tlsEcs.actorRegistry.valid(player));
    }
}

// ============================================================================
// 10. ExitRelease —— 断线释放标记 A1′ 与载入时清标记 A2′(cross-zone-scene-travel.md §12.6.3 第二步 / 第三步):
//     exit_release_mark.h 的纯判定 + 两段 Lua 的关键片段 + 无 Redis 宿主里的 ECS 级接缝(M15)
// ============================================================================
#include "services/scene/player/system/exit_release_mark.h"

namespace erm = exit_release_mark;

namespace
{
// 一份"全部条件满足"的输入:原因 kClientDisconnect、未压制、没消费票据、没有在途交接、epoch 非 0、身份有效、开关开。
erm::ExitReleaseFacts WritableFacts()
{
    erm::ExitReleaseFacts facts;
    facts.featureEnabled = true;
    facts.entityValid = true;
    facts.hasIntent = true;
    facts.cause = ExitCause::kClientDisconnect;
    facts.ownerEpoch = 7;
    facts.identityValid = true;
    return facts;
}
} // namespace

TEST(ExitReleaseDecision, WritesForTheThreeReleaseCauses)
{
    for (const auto cause : {ExitCause::kClientDisconnect, ExitCause::kNodeShutdown, ExitCause::kSceneDrain})
    {
        auto facts = WritableFacts();
        facts.cause = cause;
        EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kWrite)
            << "cause=" << player_exit::ExitCauseName(cause);
    }
}

TEST(ExitReleaseDecision, ReleaseIdentityUnspecifiedCausesDoNotWrite)
{
    for (const auto cause : {ExitCause::kReleasedByTransfer, ExitCause::kIdentityConflict, ExitCause::kUnspecified,
                             ExitCause::kCount})
    {
        auto facts = WritableFacts();
        facts.cause = cause;
        EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipCause)
            << "cause=" << player_exit::ExitCauseName(cause);
    }
}

TEST(ExitReleaseDecision, SuppressionIsCountedByItsFirstReason)
{
    auto facts = WritableFacts();
    facts.releaseMarkSuppressed = true;
    facts.suppressedBy = ExitCause::kReleasedByTransfer;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipSuppressedRelease);
    facts.suppressedBy = ExitCause::kIdentityConflict;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipSuppressedIdentity);
    facts.suppressedBy = ExitCause::kUnspecified;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipSuppressedUnspecified);
    facts.suppressedBy = ExitCause::kCount;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipSuppressedUnspecified)
        << "压制位置了却没记原因:按来源不明,仍然不写";
}

TEST(ExitReleaseDecision, MergeRecordsTheFirstSuppressor)
{
    PlayerExitIntentComp intent;
    intent.cause = ExitCause::kClientDisconnect;
    player_exit::MergeExitCause(intent, ExitCause::kNodeShutdown, false);
    EXPECT_EQ(intent.suppressedBy, ExitCause::kCount) << "不压制的合并不记原因";
    player_exit::MergeExitCause(intent, ExitCause::kReleasedByTransfer, /*devBypassSuppressesTransfer=*/true);
    player_exit::MergeExitCause(intent, ExitCause::kIdentityConflict, false);
    EXPECT_TRUE(intent.releaseMarkSuppressed);
    EXPECT_EQ(intent.suppressedBy, ExitCause::kReleasedByTransfer) << "记第一次触发压制的原因";
}

TEST(ExitReleaseDecision, ConsumedRelocateTicketDoesNotWrite)
{
    auto facts = WritableFacts();
    facts.cause = ExitCause::kSceneDrain;
    facts.relocateTicketConsumed = true;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipRelocate)
        << "票据被消费:这次的标记归改派负责(条件写,可能没写成;没生效要踢线时由待确认表兜底补写),A1′ 不得覆盖";
}

TEST(ExitReleaseDecision, EpochZeroOrInvalidEntityOrMissingIntentDoesNotWrite)
{
    auto facts = WritableFacts();
    facts.ownerEpoch = 0;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipEpochUnknown);

    facts = WritableFacts();
    facts.entityValid = false;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipEntityInvalid);

    facts = WritableFacts();
    facts.hasIntent = false;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipIntentMissing);
}

TEST(ExitReleaseDecision, ExitWinsOverIssuedHandoffDoesNotWrite)
{
    auto facts = WritableFacts();
    facts.handoffMarkInflight = true;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipHandoffInflight)
        << "M11:新标记会被在途的交接 EnterScene 用掉";
}

TEST(ExitReleaseDecision, UnconfirmedIdentityDoesNotWrite)
{
    auto facts = WritableFacts();
    facts.identityValid = false;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipIdentityConflict) << "M3";
}

TEST(ExitReleaseDecision, DisabledSwitchDoesNotWrite)
{
    auto facts = WritableFacts();
    facts.featureEnabled = false;
    EXPECT_EQ(erm::DecideExitReleaseMark(facts), erm::ExitReleaseDecision::kSkipDisabled) << "M14 回滚开关";
}

TEST(ExitReleaseDecision, SwitchParsing)
{
    EXPECT_TRUE(erm::ParseSwitch(nullptr, true)) << "未设置取默认值";
    EXPECT_FALSE(erm::ParseSwitch("", false));
    EXPECT_FALSE(erm::ParseSwitch("0", true));
    EXPECT_FALSE(erm::ParseSwitch("off", true));
    EXPECT_FALSE(erm::ParseSwitch("false", true));
    EXPECT_TRUE(erm::ParseSwitch("1", false));
    EXPECT_TRUE(erm::ParseSwitch("on", false));
    EXPECT_TRUE(erm::ParseSwitch("true", false));
    EXPECT_TRUE(erm::ParseSwitch("garbage", true)) << "不认识的值取默认值";
}

TEST(ExitReleaseDecision, NamesCoverEveryValue)
{
    for (std::size_t i = 0; i < erm::kExitReleaseDecisionCount; ++i)
    {
        EXPECT_STRNE(erm::ExitReleaseDecisionName(static_cast<erm::ExitReleaseDecision>(i)), "?");
    }
    EXPECT_STREQ(erm::ExitReleaseDecisionName(erm::ExitReleaseDecision::kCount), "?");
    for (std::size_t i = 0; i < erm::kInheritClearResultCount; ++i)
    {
        EXPECT_STRNE(erm::InheritClearResultName(static_cast<erm::InheritClearResult>(i)), "?");
    }
    EXPECT_STREQ(erm::InheritClearResultName(erm::InheritClearResult::kCount), "?");
}

TEST(ExitReleaseMarkWrite, ReplyClassification)
{
    using R = erm::MarkWriteResult;
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kInteger, 1), R::kWritten);
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kInteger, 0), R::kEpochMoved);
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kInteger, -2), R::kSuperseded)
        << "原资格已被轮换(handoff_fence),与 owner_epoch 已推进分开计";
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kInteger, -1), R::kFailed);
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kInteger, 2), R::kFailed);
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kNone, 1), R::kFailed) << "空应答:结果未知,计失败";
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kError, 0), R::kFailed);
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kOther, 1), R::kFailed);
}

TEST(ExitReleaseMarkWrite, LuaChecksOwnerEpochBeforeSet)
{
    const std::string lua = erm::kLuaWriteIfOwnerEpoch;
    EXPECT_LT(lua.find("redis.call('GET',KEYS[1])~=ARGV[1]"), lua.find("redis.call('HSET'"));
    EXPECT_LT(lua.find("if raw~=ARGV[4] then return -2 end"), lua.find("redis.call('SET'"));
    EXPECT_NE(lua.find("sameholder(held,next)"), std::string::npos);
    EXPECT_NE(lua.find("held.k~='H'"), std::string::npos) << "A1′ / A2′ / 改派只消费缓存的原 H";
    EXPECT_NE(lua.find("'EX',ARGV[3]"), std::string::npos);
    EXPECT_EQ(lua.find("tonumber"), std::string::npos);
}

// 会话结束凭证的 CAS(location-release-binding.md §3.3,2026-10-23):原件来自 HGET 观察,H / F / R 都认,但同样先比 owner、
// 再比原件整字节,原件被轮换时 -2、一个字节都不写。行为(含迟到帧)在 CurrencyTest 真 Redis 与 Go 金样里验。
TEST(ExitReleaseMarkWrite, CredentialLuaCasOnObservedOriginalBeforeSet)
{
    const std::string lua = handoff_sql_barrier::kCredentialMarkLua;
    EXPECT_LT(lua.find("redis.call('GET',KEYS[1])~=ARGV[1]"), lua.find("redis.call('HSET'"));
    EXPECT_LT(lua.find("if raw~=ARGV[4] then return -2 end"), lua.find("redis.call('HSET'"));
    EXPECT_LT(lua.find("if raw~=ARGV[4] then return -2 end"), lua.find("redis.call('SET'"));
    EXPECT_NE(lua.find("sameholder(observed,next)"), std::string::npos) << "只能把观察到的那份资格转成 F,不得换持有者";
    EXPECT_NE(lua.find("next.k~='F'"), std::string::npos);
    EXPECT_EQ(lua.find("observed.k"), std::string::npos) << "原件状态 H / F / R 都认";
    EXPECT_EQ(lua.find("tonumber"), std::string::npos);
}

TEST(ExitReleaseInheritClear, DelayedClearOnlyConfirmsOriginalRegisteredHolder)
{
    const std::string lua = erm::kLuaInheritClear;
    EXPECT_NE(lua.find("redis.call('GET',KEYS[1])~=ARGV[1] or raw~=ARGV[2]"), std::string::npos);
    EXPECT_EQ(lua.find("redis.call('DEL'"), std::string::npos) << "登记已清mark，迟到A2仅核原H";
    EXPECT_NE(handoff_sql_barrier::kRegisterFenceLua.find("cmp(candidate.n,old.n)<=0"), std::string::npos);
    EXPECT_EQ(lua.find("tonumber"), std::string::npos);
}

TEST(ExitReleaseInheritClear, UnknownEpochCannotBorrowCurrentHolder)
{
    EXPECT_STREQ(erm::kLuaInheritClearUnknownEpoch, "return -1")
        << "无原owner/holder不得借当前Redis身份清标记";
}

TEST(ExitReleaseInheritClear, ReplyClassification)
{
    using R = erm::InheritClearResult;
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kInteger, -1), R::kEpochMismatch);
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kInteger, 0), R::kAbsent);
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kInteger, 1), R::kDeletedOlder);
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kInteger, 2), R::kDeletedCurrent);
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kInteger, 3), R::kKeptNewer);
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kInteger, 4), R::kDeletedMalformed);
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kInteger, 5), R::kReplyError);
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kError, 0), R::kReplyError);
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kOther, 0), R::kReplyError);
    EXPECT_EQ(erm::ClassifyInheritClearReply(erm::ReplyShape::kNone, 0), R::kReplyLost);

    for (const auto confirmed : {R::kAbsent, R::kDeletedOlder, R::kDeletedCurrent, R::kKeptNewer, R::kDeletedMalformed})
    {
        EXPECT_TRUE(erm::IsInheritClearConfirmed(confirmed)) << erm::InheritClearResultName(confirmed);
        EXPECT_FALSE(erm::IsInheritClearFailure(confirmed));
    }
    EXPECT_FALSE(erm::IsInheritClearConfirmed(R::kEpochMismatch)) << "-1 不是确认";
    EXPECT_FALSE(erm::IsInheritClearFailure(R::kEpochMismatch)) << "-1 也不是失败:不补发";
    EXPECT_TRUE(erm::IsInheritClearFailure(R::kReplyError));
    EXPECT_TRUE(erm::IsInheritClearFailure(R::kReplyLost));

    EXPECT_TRUE(erm::MayHaveDeletedCurrent(R::kDeletedCurrent));
    EXPECT_TRUE(erm::MayHaveDeletedCurrent(R::kReplyLost)) << "空应答结果未知,放弃时按删过处理(M7)";
    EXPECT_FALSE(erm::MayHaveDeletedCurrent(R::kReplyError)) << "ERROR = Redis 明确没执行";
    EXPECT_FALSE(erm::MayHaveDeletedCurrent(R::kDeletedOlder));
    EXPECT_FALSE(erm::MayHaveDeletedCurrent(R::kEpochMismatch));
}

TEST(ExitReleaseInheritClear, GateHasThreeStates)
{
    using P = erm::InheritClearPhase;
    using G = erm::InheritGateDecision;
    // 旧版路由(ctxEpoch 0)不再直接放行(复审 ownership-major):同样要等针对 0 的那一次清理确认。
    EXPECT_EQ(erm::DecideInheritGate(0, 0, P::kNone), G::kRefuse) << "旧版路由也没发过清理:fail-closed";
    EXPECT_EQ(erm::DecideInheritGate(0, 0, P::kConfirmed), G::kProceed);
    EXPECT_EQ(erm::DecideInheritGate(0, 0, P::kInFlight), G::kWait);
    EXPECT_EQ(erm::DecideInheritGate(0, 5, P::kConfirmed), G::kRefuse) << "针对 5 的确认不能冒充针对 0 的";
    EXPECT_EQ(erm::DecideInheritGate(5, 0, P::kConfirmed), G::kRefuse) << "针对 0 的确认不能冒充针对 5 的";
    EXPECT_EQ(erm::DecideInheritGate(5, 5, P::kConfirmed), G::kProceed);
    EXPECT_EQ(erm::DecideInheritGate(5, 5, P::kInFlight), G::kWait) << "EVAL 在途:暂存载入结果";
    EXPECT_EQ(erm::DecideInheritGate(5, 5, P::kRetryWait), G::kWait) << "失败待重发:暂存载入结果";
    EXPECT_EQ(erm::DecideInheritGate(5, 5, P::kNone), G::kRefuse) << "没发过:fail-closed";
    EXPECT_EQ(erm::DecideInheritGate(6, 5, P::kConfirmed), G::kRefuse)
        << "只认本 ctx.ownerEpoch 的那一次,别的 epoch 的确认不能冒充(M2)";
}

TEST(ExitReleaseInheritClear, RetryIsBoundedByAttemptsAndDeadline)
{
    using namespace std::chrono_literals;
    EXPECT_FALSE(erm::IsInheritClearExhausted(1, 0ms));
    EXPECT_FALSE(erm::IsInheritClearExhausted(erm::kInheritClearMaxAttempts - 1, 4999ms));
    EXPECT_TRUE(erm::IsInheritClearExhausted(erm::kInheritClearMaxAttempts, 0ms)) << "次数上限";
    EXPECT_TRUE(erm::IsInheritClearExhausted(1, erm::kInheritClearDeadline)) << "截止时刻";
    EXPECT_EQ(erm::InheritClearRetryDelay(1), 250ms);
    EXPECT_EQ(erm::InheritClearRetryDelay(2), 500ms);
    EXPECT_EQ(erm::InheritClearRetryDelay(3), 1000ms);
    EXPECT_EQ(erm::InheritClearRetryDelay(9), 1000ms) << "退避封顶";
}

TEST(ExitReleaseInheritClear, AbandonedLateReplyNeverRewritesOverANewerHolder)
{
    // 复审 ownership-major:已放弃那一轮(L1)的 A2′ 晚到应答删掉了 epoch N 的标记,而同节点更新一轮(L2,
    // 同为 epoch N、不铸造)已在载入或已建实体。当场补写会给 L2 留下对它有效的"已落盘、不再持有"标记。
    using D = erm::AbandonedRewriteDecision;
    EXPECT_EQ(erm::DecideAbandonedRewrite(/*nodeHoldsPlayer=*/false, /*newerLoadPending=*/false), D::kRewriteNow);
    EXPECT_EQ(erm::DecideAbandonedRewrite(false, true), D::kHandToNewerLoad)
        << "L2 还在载入:补写移交给 L2(L2 建出实体即作废,L2 也放弃时才补写)";
    EXPECT_EQ(erm::DecideAbandonedRewrite(true, false), D::kDropNodeHolds) << "L2 已建实体:本节点就是持有者";
    EXPECT_EQ(erm::DecideAbandonedRewrite(true, true), D::kDropNodeHolds) << "有实体一律不补写";
}

// ── ECS 级接缝(M15):宿主里没有 zone Redis(tlsRedis.GetZoneRedis() 为空)──
namespace
{
constexpr Guid kReleasePlayerId = 950200002;

// 在待入场表里放一条"新载入"上下文(session 0:不预登记会话、不回 tip,只看闸门)。
void RegisterPendingEnter(uint64_t ownerEpoch)
{
    tlsEcs.Clear();
    SessionMap().clear();
    auto& pending = PlayerLifecycleSystem::GetPendingEnterMap();
    pending.clear();
    PlayerEnterContext ctx;
    ctx.ownerEpoch = ownerEpoch;
    SetFixtureAdmission(ctx);
    pending[kReleasePlayerId] = ctx;
}
} // namespace

TEST(ExitReleaseEcs, ConvergedExitWithoutRedisCountsFailedAndLeavesNothingInFlight)
{
    // M15:无 Redis 宿主里 A1′ 判定为写后必须走"写不成"分支、不增加在途计数;实体照常销毁。
    PlayerLifecycleSystem::SetNodeIdentityProbe([] { return true; });
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(player, landed));
    const auto before = exit_release_stats::Read();

    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    const auto after = exit_release_stats::Read();
    PlayerLifecycleSystem::SetNodeIdentityProbe({});
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    const auto attempted = static_cast<std::size_t>(erm::ExitReleaseDecision::kWrite);
    EXPECT_EQ(after.decisions[attempted], before.decisions[attempted] + 1) << "条件都满足,判定为写";
    EXPECT_EQ(after.failed, before.failed + 1) << "zone Redis 不可用:计 failed,不重试";
    EXPECT_EQ(PlayerLifecycleSystem::ExitReleaseMarksInFlight(), 0u);
}

TEST(ExitReleaseEcs, UnconfirmedIdentityOnExitSkipsTheMark)
{
    PlayerLifecycleSystem::SetNodeIdentityProbe({}); // 未注入 = 身份不确认
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(player, landed));
    const auto before = exit_release_stats::Read();

    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    const auto after = exit_release_stats::Read();
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    const auto skip = static_cast<std::size_t>(erm::ExitReleaseDecision::kSkipIdentityConflict);
    EXPECT_EQ(after.decisions[skip], before.decisions[skip] + 1);
    EXPECT_EQ(after.failed, before.failed) << "不写就不会有失败";
}

TEST(ExitReleaseEcs, LoadWithoutClearForItsEpochIsRefused)
{
    // 闸门 fail-closed:ctx.ownerEpoch 非 0 却没有针对它的 A2′(没调 BeginInheritedMarkClear)→ 拒建实体。
    RegisterPendingEnter(/*ownerEpoch=*/5);
    const auto before = exit_release_stats::Read();

    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kReleasePlayerId, IdentifiedPlayerLoadData(kReleasePlayerId));

    EXPECT_EQ(PlayerLifecycleSystem::GetPendingEnterMap().count(kReleasePlayerId), 0u) << "待入场条目已擦掉";
    EXPECT_EQ(tlsEcs.playerList.count(kReleasePlayerId), 0u) << "没有建实体";
    EXPECT_EQ(exit_release_stats::Read().inheritRefused, before.inheritRefused + 1);
}

TEST(ExitReleaseEcs, RouteWithoutOwnerEpochStillWaitsForTheClear)
{
    // 复审 ownership-major:旧版路由(owner_epoch 0)以前跳过 A2′、闸门直接放行,上一任的 A1′ 标记因此存活。
    RegisterPendingEnter(/*ownerEpoch=*/0);
    const auto before = exit_release_stats::Read();
    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kReleasePlayerId, IdentifiedPlayerLoadData(kReleasePlayerId));
    EXPECT_EQ(PlayerLifecycleSystem::GetPendingEnterMap().count(kReleasePlayerId), 0u) << "没发过清理:拒建实体";
    EXPECT_EQ(tlsEcs.playerList.count(kReleasePlayerId), 0u);
    EXPECT_EQ(exit_release_stats::Read().inheritRefused, before.inheritRefused + 1);

    RegisterPendingEnter(/*ownerEpoch=*/0);
    PlayerLifecycleSystem::BeginInheritedMarkClear(kReleasePlayerId);
    auto& pending = PlayerLifecycleSystem::GetPendingEnterMap();
    ASSERT_EQ(pending.count(kReleasePlayerId), 1u);
    const auto& state = pending.at(kReleasePlayerId).inheritClear;
    EXPECT_EQ(state.targetEpoch, 0u) << "针对\"路由不带 owner_epoch\"的那一种清理";
    EXPECT_EQ(state.phase, erm::InheritClearPhase::kRetryWait) << "照样发了(zone Redis 不可用 = 失败,等重发)";
    EXPECT_EQ(state.attemptsSent, 1u);
    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kReleasePlayerId, IdentifiedPlayerLoadData(kReleasePlayerId));
    ASSERT_EQ(pending.count(kReleasePlayerId), 1u) << "等清理期间保留载入结果,不建实体";
    EXPECT_NE(pending.at(kReleasePlayerId).inheritClear.stashedLoad, nullptr);
    EXPECT_EQ(tlsEcs.playerList.count(kReleasePlayerId), 0u);
}

TEST(ExitReleaseEcs, FailingClearKeepsTheLoadThenRefusesWhenExhausted)
{
    RegisterPendingEnter(/*ownerEpoch=*/5);
    PlayerLifecycleSystem::BeginInheritedMarkClear(kReleasePlayerId);
    {
        auto& pending = PlayerLifecycleSystem::GetPendingEnterMap();
        ASSERT_EQ(pending.count(kReleasePlayerId), 1u) << "首发失败不拒绝";
        const auto& state = pending.at(kReleasePlayerId).inheritClear;
        EXPECT_NE(state.lifecycle, 0u);
        EXPECT_EQ(state.targetEpoch, 5u);
        EXPECT_EQ(state.phase, erm::InheritClearPhase::kRetryWait) << "zone Redis 不可用 = 失败,等重发";
        EXPECT_EQ(state.attemptsSent, 1u);
    }

    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kReleasePlayerId, IdentifiedPlayerLoadData(kReleasePlayerId));
    {
        auto& pending = PlayerLifecycleSystem::GetPendingEnterMap();
        ASSERT_EQ(pending.count(kReleasePlayerId), 1u) << "等重发期间保留载入结果(M10)";
        EXPECT_NE(pending.at(kReleasePlayerId).inheritClear.stashedLoad, nullptr);
        EXPECT_EQ(tlsEcs.playerList.count(kReleasePlayerId), 0u);
    }

    const auto before = exit_release_stats::Read();
    // 重连触发的补发忽略退避;每次都失败,第 kInheritClearMaxAttempts 次失败后拒绝。
    for (uint32_t i = 1; i < erm::kInheritClearMaxAttempts; ++i)
    {
        PlayerLifecycleSystem::RetryInheritedMarkClears(/*reconnected=*/true);
    }
    EXPECT_EQ(PlayerLifecycleSystem::GetPendingEnterMap().count(kReleasePlayerId), 0u) << "重发耗尽:拒建实体";
    EXPECT_EQ(tlsEcs.playerList.count(kReleasePlayerId), 0u);
    EXPECT_EQ(exit_release_stats::Read().inheritRefused, before.inheritRefused + 1);
}

// ============================================================================
// 11. EnterSceneReply —— EnterScene 应答按 correlation_id 分发(player_lifecycle.h 的 enter_scene_reply、
//     PlayerLifecycleSystem::DispatchEnterSceneReply):纯函数 Classify / IsSuspiciousUnmatched,
//     加无 Redis 宿主里的 ECS 级接缝 —— 外来应答不得变成交接证据、§11.2 的 18 链不断、
//     旧版 scene_manager(不回显,号为 0)退回今天按 player_id 的行为。
// ============================================================================
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "network/node_utils.h" // 节点身份由测试宿主安装,不属于线程 ECS 上下文
#include "time/system/time.h"   // TimeSystem::NowMillisecondsUTC:在途换图组件的 sentAtMs
#include "thread_context/node_context_manager.h" // 假 scene_manager 节点(StartTravelHandoff 的 SM 预检)
#include "core/network/grpc_client_runtime.h"       // 假 scene_manager 节点的通道:MuduoGrpcChannelPtr / muduo_grpc::Channel / EventLoop

namespace esr = enter_scene_reply;

TEST(EnterSceneReplyRoute, HandoffTagMatch)
{
    EXPECT_EQ(esr::Classify(42, true, 42, false, 0), esr::Route::kTravelHandoff);
}

TEST(EnterSceneReplyRoute, ForeignTagDuringHandoffIsUnmatched)
{
    EXPECT_EQ(esr::Classify(41, true, 42, false, 0), esr::Route::kUnmatched) << "上一代 / 别的请求的号";
    EXPECT_EQ(esr::Classify(7, true, 0, false, 0), esr::Route::kUnmatched)
        << "交接的 EnterScene 还没发(号 0):任何应答都不可能是它的";
}

TEST(EnterSceneReplyRoute, SceneChangeTagDuringHandoffIsDropped)
{
    EXPECT_EQ(esr::Classify(9, true, 42, true, 9), esr::Route::kSceneChangeDuringHandoff)
        << "按构造不可达的共存:丢弃,不走普通换图分支(否则交接中会补一条失败 tip)";
}

TEST(EnterSceneReplyRoute, UncorrelatedDuringHandoffFallsBackOnlyAfterSend)
{
    EXPECT_EQ(esr::Classify(0, true, 42, false, 0), esr::Route::kLegacyTravelHandoff) << "旧版 SM:退回按 player_id";
    EXPECT_EQ(esr::Classify(0, true, 42, true, 9), esr::Route::kLegacyTravelHandoff);
    EXPECT_EQ(esr::Classify(0, true, 0, false, 0), esr::Route::kUnmatched)
        << "旧版 SM 下也要求交接的 EnterScene 已经发出:SET 在途窗口里的应答不是它的";
}

TEST(EnterSceneReplyRoute, SceneChangeTagMatch)
{
    EXPECT_EQ(esr::Classify(9, false, 0, true, 9), esr::Route::kSceneChange);
    EXPECT_EQ(esr::Classify(8, false, 0, true, 9), esr::Route::kUnmatched) << "上一条换图的迟到应答";
    EXPECT_EQ(esr::Classify(5, false, 0, true, 0), esr::Route::kUnmatched);
}

TEST(EnterSceneReplyRoute, UncorrelatedSceneChangeFallsBack)
{
    EXPECT_EQ(esr::Classify(0, false, 0, true, 9), esr::Route::kLegacySceneChange) << "旧版 SM:§11.2 的 18 链不断";
}

TEST(EnterSceneReplyRoute, NoWaiter)
{
    EXPECT_EQ(esr::Classify(0, false, 0, false, 0), esr::Route::kNoWaiter);
    EXPECT_EQ(esr::Classify(5, false, 0, false, 0), esr::Route::kNoWaiter);
}

TEST(EnterSceneReplyRoute, SuspiciousUnmatched)
{
    EXPECT_FALSE(esr::IsSuspiciousUnmatched(esr::Route::kUnmatched, false, false, false))
        << "路由先到之后才到的纯成功迟到应答是常态,不计数";
    EXPECT_TRUE(esr::IsSuspiciousUnmatched(esr::Route::kUnmatched, false, true, false)) << "拒绝";
    EXPECT_TRUE(esr::IsSuspiciousUnmatched(esr::Route::kUnmatched, false, false, true)) << "重定向";
    EXPECT_TRUE(esr::IsSuspiciousUnmatched(esr::Route::kUnmatched, true, false, false)) << "发生在交接期间";
    EXPECT_TRUE(esr::IsSuspiciousUnmatched(esr::Route::kSceneChangeDuringHandoff, false, false, false));
    EXPECT_FALSE(esr::IsSuspiciousUnmatched(esr::Route::kNoWaiter, true, true, true));
    EXPECT_FALSE(esr::IsSuspiciousUnmatched(esr::Route::kTravelHandoff, true, true, true));
}

TEST(EnterSceneReplyRoute, RouteNamesCoverEveryValue)
{
    for (std::size_t i = 0; i < esr::kRouteCount; ++i)
    {
        EXPECT_STRNE(esr::RouteName(static_cast<esr::Route>(i)), "?") << "route=" << i;
    }
    EXPECT_STREQ(esr::RouteName(esr::Route::kCount), "?") << "越界不能读出数组外";
}

// ── ECS 级接缝:直接驱动 PlayerLifecycleSystem::DispatchEnterSceneReply ──
// 宿主里没有 zone Redis(tlsRedis.GetZoneRedis() 为空),除 ScopedFakeSceneManagerNode 存活期间(夹具自带 EventLoop)外也没有 EventLoop:ResolveTravelOutcome 判不清就保持冻结、
// 计 verify_rearmed,看门狗挂不上(只打 WARN)。所以"应答进了交接裁决"的可观察结果是证据被记下 + verify_rearmed +1。
// 例外:用了 ScopedFakeSceneManagerNode 的用例,夹具在测试线程自建 EventLoop,期间统计 / 冻结上限定时器会挂到它上面;
// 控制它们的 static thread_local armed 置位后不复位,之后的用例即使有 loop 也不会再挂。现有用例都不依赖这些定时器。
namespace
{
constexpr uint32_t kReplyTestZone = 7;
constexpr uint32_t kReplyTargetZone = 8;
constexpr uint32_t kSmErrSomeRejection = 5; // 任意非 18 的 scene_manager 错误码

// 在线玩家 + 本节点 zone + "盘上已是同一份"(SavePlayerToRedis 走快路径,不碰 Redis)。
// zone 由每例开始前的监听器清零,与 tlsEcs.Clear() 无关;这里显式设置本用例身份。
// StartTravelHandoff 对 targetZoneId == 0 直接拒绝。
bool MakeReplyTestPlayer(entt::entity& player)
{
    player = MakeOnlinePlayer(kSessionAtExit);
    auto& original = tlsEcs.actorRegistry.emplace_or_replace<PlayerOwnerEpochComp>(player);
    original.epoch = 7;
    original.handoffFence = "7|explicit-test-scene|1|"+std::string(32,'a')+"|H|"+std::string(32,'b');
    TestLocalNodeInfo().set_zone_id(kReplyTestZone);
    return MarkPersisted(player);
}

// 交接在途:handoff 标记的 SET 已发(requestedAtMs 非 0)。correlationId 0 = 交接的 EnterScene 还没发。
// markEpoch 0:本用例不关心撤回,AbortTravelHandoff 也就不登记待撤回表。
void AttachHandoff(entt::entity player, uint64_t correlationId)
{
    auto& travel = tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
    travel.targetZoneId = kReplyTargetZone;
    travel.requestedAtMs = 1000;
    travel.markEpoch = 0;
    travel.enterSceneCorrelationId = correlationId;
    auto& frozen = tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    frozen.frozenAtMs = 1000;
    frozen.toZoneId = kReplyTargetZone;
}

void AttachSceneChange(entt::entity player, uint64_t sceneId, bool playerRequested, uint64_t correlationId)
{
    auto& pending = tlsEcs.actorRegistry.emplace<PlayerSceneChangeInFlightComp>(player);
    pending.sceneId = sceneId;
    pending.sceneConfigId = 0;
    pending.sentAtMs = TimeSystem::NowMillisecondsUTC();
    pending.playerRequested = playerRequested;
    pending.correlationId = correlationId;
}

// StartTravelHandoff 在冻结之前预检 scene_manager 是否可达(冻结上限:SET 之后才发现没有 SM 只能销毁 + 踢线,
// 所以要在改状态之前挡住)。夹具给 SM 实体挂一条真实的 muduo_grpc 通道(目标端口不监听),满足生产筛选契约
// (HasUsableGrpcClient);这些用例在 epoch / Redis 检查处终止,不发真实 RPC。
// 通道只能在其 EventLoop 线程上使用,本宿主又没有 EventLoop,所以夹具在测试线程上自带一个(一个线程同时只能有一个
// 夹具),平时不转,只在析构时转到拆除排完:选点的 GetState(true) 已经发起过一次连接,~Channel 把连接拆除排进队列,
// muduo Connector 拆除途中还会再排一跳(resetChannel)。两跳 queueInLoop 后再 quit(AGENTS.md §11.7),
// 保证它们都在 loop 析构前跑完。夹具存活期间当前线程「有 EventLoop」,交接统计 / 冻结上限扫描会挂到它上面,随它销毁。
class ScopedFakeSceneManagerNode
{
public:
    ScopedFakeSceneManagerNode()
    {
        auto& registry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
        entity_ = registry.create();
        auto channel = muduo_grpc::Channel::Create(loop_, "127.0.0.1", 1);
        EXPECT_NE(channel, nullptr);
        registry.emplace<MuduoGrpcChannelPtr>(entity_, std::move(channel));
        registry.emplace<NodeInfo>(entity_);
    }
    ~ScopedFakeSceneManagerNode()
    {
        auto& registry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
        const auto* channel = registry.try_get<MuduoGrpcChannelPtr>(entity_);
        EXPECT_TRUE(channel != nullptr && *channel != nullptr && (*channel)->outstandingCalls() == 0)
            << "夹具不得发起真实 RPC";
        registry.destroy(entity_);
        loop_.queueInLoop([&loop = loop_] { loop.queueInLoop([&loop] { loop.quit(); }); });
        loop_.loop();
    }
    ScopedFakeSceneManagerNode(const ScopedFakeSceneManagerNode&) = delete;
    ScopedFakeSceneManagerNode& operator=(const ScopedFakeSceneManagerNode&) = delete;

private:
    // 先于实体构造、最后析构:通道与它排进队列的拆除都要在 loop 还在时处理掉。
    muduo::net::EventLoop loop_;
    entt::entity entity_{entt::null};
};

::scene_manager::EnterSceneResponse MakeReply(Guid playerId, uint32_t errorCode, uint64_t correlationId,
                                              bool withRedirect = false)
{
    ::scene_manager::EnterSceneResponse resp;
    resp.set_player_id(playerId);
    resp.set_error_code(errorCode);
    resp.set_correlation_id(correlationId);
    if (withRedirect)
    {
        resp.mutable_redirect()->set_target_gate_ip("10.2.0.8");
        resp.mutable_redirect()->set_target_gate_port(7001);
    }
    return resp;
}

// 既有用例显式模拟旧的直接分发入口;新的捕获身份用例直接传入 playerId / requestTag。
void DeliverEnterSceneReply(const ::scene_manager::EnterSceneResponse &response)
{
    PlayerLifecycleSystem::DispatchEnterSceneReply(response.player_id(), response.correlation_id(), response);
}
} // namespace

TEST(TravelPersistEcs, IncompletePlayerNeverStartsHandoff)
{
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    ScopedFakeSceneManagerNode sceneManager;
    const auto saved = tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player).snapshot->SerializeAsString();
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);
    tlsEcs.actorRegistry.emplace<PlayerMirrorCreateInFlightComp>(player).token = 103;
    const auto before = travel_handoff_stats::Read();

    g_handoffPrecheckRefusedLines = 0;
    muduo::Logger::setOutput(CountHandoffPrecheckLines);
    const auto result = PlayerLifecycleSystem::StartTravelHandoff(player, kReplyTargetZone, 0, 0);
    muduo::Logger::setOutput(WriteLogToStdout);

    EXPECT_EQ(g_handoffPrecheckRefusedLines, 1);
    EXPECT_NE(result, PlayerLifecycleSystem::kTravelAccepted);
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp, PlayerFrozenComp>(player)));
    EXPECT_EQ(travel_handoff_stats::Read().started, before.started);
    EXPECT_EQ(travel_handoff_stats::Read().aborted, before.aborted);
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player).token, 103u)
        << "交接尚未提交,预检失败不得取消旧镜像";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerAssetOpLedgerComp>(player));
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player).snapshot->SerializeAsString(), saved);
}

// 刚落地的原件不重marshal已缺组件；Redis断连使撤销资格未知，必须保留冻结及原稳定撤销候选。
TEST(TravelPersistEcs, LandedCallbackWithMissingComponentWaitsForQualificationRevocation)
{
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    ASSERT_EQ(tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch, 7u); // 保留MakeReplyTestPlayer捕获的原H资格。
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(player, landed));
    auto& travel = tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
    travel.targetZoneId = kReplyTargetZone;
    travel.holderFence = tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).handoffFence;
    travel.frozenAtSteady = std::chrono::steady_clock::now();
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    tlsEcs.actorRegistry.remove<BattleSettlementLedgerComp>(player);
    const auto before = travel_handoff_stats::Read();

    g_handoffMarkComponentRefusedLines = 0;
    g_handoffMarkRedisDisconnectedLines = 0;
    muduo::Logger::setOutput(CountHandoffPrecheckLines);
    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);
    muduo::Logger::setOutput(WriteLogToStdout);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_TRUE((tlsEcs.actorRegistry.all_of<PlayerTravelHandoffComp, PlayerFrozenComp>(player)));
    EXPECT_EQ(travel_handoff_stats::Read().aborted, before.aborted);
    EXPECT_EQ(g_handoffMarkComponentRefusedLines, 0);
    EXPECT_EQ(g_handoffMarkRedisDisconnectedLines, 0); // 尚未确认撤销，不可打印“已中止/解冻”。
    const auto& pending = tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(player);
    EXPECT_EQ(pending.holderFence, tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).handoffFence);
    EXPECT_FALSE(pending.cancelFence.empty());
    EXPECT_FALSE(pending.cancellationPending); // 未连接，没有接管任何Redis回调。
    EXPECT_EQ(travel_handoff_stats::Read().handoffFastpathForced, before.handoffFastpathForced);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<BattleSettlementLedgerComp>(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), player);
}

TEST(EnterSceneReplyEcs, ForeignTaggedReplyDuringHandoffLeavesEvidenceUntouched)
{
    // 今天的缺陷:交接在途时任何一条 EnterScene 应答都照单全收 —— 污染证据、DEL 撤回标记、跨 zone 还可能踢线。
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachHandoff(player, /*correlationId=*/42);
    const auto before = travel_handoff_stats::Read();

    DeliverEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 41));
    DeliverEnterSceneReply(MakeReply(kExitPlayerId, 0, 41));
    DeliverEnterSceneReply(MakeReply(kExitPlayerId, 0, 41, /*withRedirect=*/true));

    const auto after = travel_handoff_stats::Read();
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "外来 redirect 只记录、不销毁";
    const auto* travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
    ASSERT_NE(travel, nullptr);
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_FALSE(travel->hasRecordedEvidence) << "外来应答不得记成交接证据";
    EXPECT_EQ(after.aborted, before.aborted);
    EXPECT_EQ(after.verifyRearmed, before.verifyRearmed) << "没有进 ResolveTravelOutcome";
    EXPECT_EQ(after.granted, before.granted);
    EXPECT_EQ(after.replyUnmatched, before.replyUnmatched + 3) << "交接期间的外来应答全算可疑";
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated);
}

TEST(EnterSceneReplyEcs, ReplyBeforeHandoffSendIsIgnoredEvenWhenUncorrelated)
{
    // requestedAtMs 在 SET 发出前就已置位:今天 SET 在途期间到达的外来应答会被吃成交接证据。改后丢弃。
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachHandoff(player, /*correlationId=*/0);
    const auto before = travel_handoff_stats::Read();

    DeliverEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 0));

    const auto after = travel_handoff_stats::Read();
    const auto* travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
    ASSERT_NE(travel, nullptr);
    EXPECT_FALSE(travel->hasRecordedEvidence);
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated + 1);
    EXPECT_EQ(after.replyUnmatched, before.replyUnmatched + 1);
}

TEST(EnterSceneReplyEcs, OwnReplyReachesTravelOutcome)
{
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachHandoff(player, /*correlationId=*/42);
    const auto before = travel_handoff_stats::Read();

    DeliverEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 42));

    const auto after = travel_handoff_stats::Read();
    const auto* travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
    ASSERT_NE(travel, nullptr) << "Redis 不可用判不清:保持交接,不解冻";
    EXPECT_TRUE(travel->hasRecordedEvidence);
    EXPECT_EQ(travel->recordedEvidence, static_cast<uint8_t>(travel_outcome::Evidence::kFailed));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_EQ(after.verifyRearmed, before.verifyRearmed + 1);
    EXPECT_EQ(after.replyUnmatched, before.replyUnmatched);
}

TEST(EnterSceneReplyEcs, UncorrelatedReplyAfterSendFallsBackToLegacy)
{
    // 旧版 SM 不回显:交接的 EnterScene 已发,就按 player_id 退回今天的行为(不 fail-closed)。
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachHandoff(player, /*correlationId=*/42);
    const auto before = travel_handoff_stats::Read();

    DeliverEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 0));

    const auto after = travel_handoff_stats::Read();
    const auto* travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
    ASSERT_NE(travel, nullptr);
    EXPECT_TRUE(travel->hasRecordedEvidence);
    EXPECT_EQ(travel->recordedEvidence, static_cast<uint8_t>(travel_outcome::Evidence::kFailed));
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated + 1);
    EXPECT_EQ(after.verifyRearmed, before.verifyRearmed + 1);
}

TEST(EnterSceneReplyEcs, SceneChange18StillStartsSameZoneHandoff)
{
    // §11.2 回归:普通换图被 18 暂拒 → 用记下的目标起同 zone 交接。对号的依据从 player_id 换成了关联号。
    const ScopedFakeSceneManagerNode sceneManager; // StartTravelHandoff 的 SM 预检
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/true, /*correlationId=*/9);
    const auto before = travel_handoff_stats::Read();

    DeliverEnterSceneReply(
        MakeReply(kExitPlayerId, PlayerLifecycleSystem::kSmErrHandoffPending, 10));
    const auto afterForeign = travel_handoff_stats::Read();
    EXPECT_EQ(afterForeign.started, before.started) << "别的请求的 18 不起交接";
    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
    ASSERT_NE(pending, nullptr) << "对不上号的应答不得摘掉在途记录";
    EXPECT_EQ(pending->correlationId, 9u);
    EXPECT_EQ(afterForeign.replyUnmatched, before.replyUnmatched + 1);

    DeliverEnterSceneReply(
        MakeReply(kExitPlayerId, PlayerLifecycleSystem::kSmErrHandoffPending, 9));
    const auto after = travel_handoff_stats::Read();
    EXPECT_EQ(after.started, before.started + 1) << "号匹配的 18 起同 zone 交接";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    // 宿主里没有 owner_epoch(按 0):BeginTravelHandoff 就地 Abort,交接与冻结成对摘掉。
    EXPECT_EQ(after.aborted, before.aborted);
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
}

TEST(EnterSceneReplyEcs, LegacySceneChange18StillStartsHandoff)
{
    // 旧版 SM 不回显号:§11.2 的 18 链照样接得上(按 player_id 退回)。
    const ScopedFakeSceneManagerNode sceneManager; // StartTravelHandoff 的 SM 预检
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/true, /*correlationId=*/9);
    const auto before = travel_handoff_stats::Read();

    DeliverEnterSceneReply(
        MakeReply(kExitPlayerId, PlayerLifecycleSystem::kSmErrHandoffPending, 0));

    const auto after = travel_handoff_stats::Read();
    EXPECT_EQ(after.started, before.started + 1);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated + 1);
}

TEST(EnterSceneReplyEcs, StaleFollowReplyDoesNotConsumeNewerRequest)
{
    // 过了 TTL 才到的上一条请求的应答,今天会摘掉新请求的在途记录。
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachSceneChange(player, /*sceneId=*/2, /*playerRequested=*/false, /*correlationId=*/20);
    const auto before = travel_handoff_stats::Read();

    DeliverEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 19));
    {
        const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
        ASSERT_NE(pending, nullptr);
        EXPECT_EQ(pending->sceneId, 2u);
        EXPECT_EQ(pending->correlationId, 20u);
    }
    const auto afterRejected = travel_handoff_stats::Read();
    EXPECT_EQ(afterRejected.replyUnmatched, before.replyUnmatched + 1) << "对不上号的拒绝算可疑";

    DeliverEnterSceneReply(MakeReply(kExitPlayerId, 0, 19));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    EXPECT_EQ(travel_handoff_stats::Read().replyUnmatched, afterRejected.replyUnmatched)
        << "路由先到之后的纯成功迟到应答是常态,不计数";

    DeliverEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 20));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player)) << "自己的应答摘组件";
    EXPECT_EQ(travel_handoff_stats::Read().started, before.started) << "跟随被拒只记日志,不起交接";
}

TEST(EnterSceneReplyEcs, MismatchedCorrelationEchoDoesNotClaimCapturedRequest)
{
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    constexpr uint64_t requestTag = 91;
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/false, requestTag);
    const auto before = travel_handoff_stats::Read();
    const auto response = MakeReply(kExitPlayerId, kSmErrSomeRejection, requestTag + 1);

    PlayerLifecycleSystem::DispatchEnterSceneReply(kExitPlayerId, requestTag, response);

    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
    ASSERT_NE(pending, nullptr) << "回显冲突不得摘掉捕获请求的等待者";
    EXPECT_EQ(pending->correlationId, requestTag);
    const auto after = travel_handoff_stats::Read();
    EXPECT_EQ(after.replyUnmatched, before.replyUnmatched + 1);
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated);
}

TEST(EnterSceneReplyEcs, MismatchedPlayerEchoIsRejectedEvenWithoutCorrelationEcho)
{
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    constexpr uint64_t requestTag = 91;
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/false, requestTag);
    const auto before = travel_handoff_stats::Read();
    const auto response = MakeReply(kExitPlayerId + 1, kSmErrSomeRejection, /*correlationId=*/0);

    PlayerLifecycleSystem::DispatchEnterSceneReply(kExitPlayerId, requestTag, response);

    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
    ASSERT_NE(pending, nullptr) << "回显冲突不得摘掉捕获请求的等待者";
    EXPECT_EQ(pending->correlationId, requestTag);
    const auto after = travel_handoff_stats::Read();
    EXPECT_EQ(after.replyUnmatched, before.replyUnmatched + 1);
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated);
}

TEST(EnterSceneReplyEcs, CapturedIdentityClaimsReplyWithoutEcho)
{
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    constexpr uint64_t requestTag = 93;
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/false, requestTag);
    const auto before = travel_handoff_stats::Read();
    const auto response = MakeReply(/*playerId=*/0, kSmErrSomeRejection, /*correlationId=*/0);

    PlayerLifecycleSystem::DispatchEnterSceneReply(kExitPlayerId, requestTag, response);

    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player))
        << "旧版 SM 不回显时仍按捕获的身份与号收尾";
    const auto after = travel_handoff_stats::Read();
    EXPECT_EQ(after.replyUnmatched, before.replyUnmatched);
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated) << "捕获号非 0,不走兼容退路";
}

TEST(EnterSceneReplyEcs, NoWaiterAndGonePlayerAreSilent)
{
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    const auto before = travel_handoff_stats::Read();

    DeliverEnterSceneReply(MakeReply(kExitPlayerId, 0, 5));
    DeliverEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 5));
    // 不在本节点的玩家。
    DeliverEnterSceneReply(MakeReply(kExitPlayerId + 1, kSmErrSomeRejection, 6));
    // 调用方没有带上捕获的 playerId:属于调用方 bug,在计数之前直接返回。
    DeliverEnterSceneReply(MakeReply(0, kSmErrSomeRejection, 0));

    const auto after = travel_handoff_stats::Read();
    EXPECT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(after.replyUnmatched, before.replyUnmatched);
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated)
        << "捕获的 playerId 为 0 时在计 reply_uncorrelated 之前就返回";
}

// ----------------------------------------------------------------------------
// EnterScene 传输失败(PlayerLifecycleSystem::DispatchEnterSceneTransportFailure,
// docs/design/grpc-muduo-transport.md §16.5)。修复前生成的 gRPC 客户端对非 OK 只打日志,
// 这些路径根本走不到:普通换图的槽位只能等 TTL 过期,客户端收不到任何提示。
// ----------------------------------------------------------------------------
#include "node/system/grpc_call_deadline.h" // 在途换图 TTL 从 SceneManager 的 deadline 派生

namespace
{
void DeliverEnterSceneTransportFailure(Guid playerId, uint64_t correlationId, const std::string &reason)
{
    PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(playerId, correlationId, reason);
}

constexpr char kTransportFailureReason[] = "SceneManager.EnterScene code=4 msg=Deadline Exceeded";
} // namespace

TEST(EnterSceneTransportFailureEcs, HandoffFailureIsNotEvidence)
{
    // 结果未知:scene_manager 可能已放行。不解冻、不记证据、不提前核实,留给应答看门狗 / 冻结上限。
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachHandoff(player, /*correlationId=*/42);
    const auto before = travel_handoff_stats::Read();

    DeliverEnterSceneTransportFailure(kExitPlayerId, 42, kTransportFailureReason);

    const auto after = travel_handoff_stats::Read();
    const auto* travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
    ASSERT_NE(travel, nullptr);
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_FALSE(travel->hasRecordedEvidence) << "传输失败不是 scene_manager 的拒绝";
    EXPECT_EQ(after.aborted, before.aborted);
    EXPECT_EQ(after.verifyRearmed, before.verifyRearmed) << "不提前进 ResolveTravelOutcome";
    EXPECT_EQ(after.granted, before.granted);
}

TEST(EnterSceneTransportFailureEcs, OwnSceneChangeFailureReleasesTheSlot)
{
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/true, /*correlationId=*/9);
    const auto before = travel_handoff_stats::Read();

    // 别的请求(被顶替的)的失败不动当前等待者。
    DeliverEnterSceneTransportFailure(kExitPlayerId, 8, kTransportFailureReason);
    {
        const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
        ASSERT_NE(pending, nullptr);
        EXPECT_EQ(pending->correlationId, 9u);
    }

    DeliverEnterSceneTransportFailure(kExitPlayerId, 9, kTransportFailureReason);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player)) << "自己的失败摘槽位";
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "玩家可以立刻重试";
    const auto after = travel_handoff_stats::Read();
    EXPECT_EQ(after.started, before.started) << "传输失败不是 18,不起交接";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
}

TEST(EnterSceneTransportFailureEcs, FollowFailureReleasesTheSlotToo)
{
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachSceneChange(player, /*sceneId=*/2, /*playerRequested=*/false, /*correlationId=*/20);

    DeliverEnterSceneTransportFailure(kExitPlayerId, 20, kTransportFailureReason);

    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
}

TEST(EnterSceneTransportFailureEcs, UncorrelatedFailureNeverFallsBackToPlayerId)
{
    // 应答号为 0 会退回按 player_id(旧版 SM);失败号为 0 只能是绕过统一出口的发送,不能照搬那条退路。
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachSceneChange(player, /*sceneId=*/2, /*playerRequested=*/true, /*correlationId=*/20);

    DeliverEnterSceneTransportFailure(kExitPlayerId, 0, kTransportFailureReason);
    // 不在本节点的玩家:静默。
    DeliverEnterSceneTransportFailure(kExitPlayerId + 1, 21, kTransportFailureReason);

    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
    ASSERT_NE(pending, nullptr);
    EXPECT_EQ(pending->correlationId, 20u);
}

TEST(EnterSceneTransportFailureEcs, InFlightTtlCoversTheSceneManagerDeadline)
{
    // 上游比下游宽:deadline 到期时失败通知才到,槽位必须还在;TTL 只兜"完成通知永远不来"。
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    const uint64_t deadlineMs =
        static_cast<uint64_t>(grpc_call_deadline::Get(eNodeType::SceneManagerNodeService).count());
    AttachSceneChange(player, /*sceneId=*/2, /*playerRequested=*/true, /*correlationId=*/30);
    auto& pending = tlsEcs.actorRegistry.get<PlayerSceneChangeInFlightComp>(player);
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();

    pending.sentAtMs = nowMs - deadlineMs;
    EXPECT_TRUE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "deadline 刚到时槽位不得已过期";

    pending.sentAtMs = nowMs - deadlineMs - 2000;
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "deadline + 余量之后放下一条请求";
}

// ============================================================================
// 12. TravelFreezeCap —— 交接冻结硬上限 + 晚发闸 +「标记已发出」统一收口(travel_freeze_cap.h、
//     PlayerLifecycleSystem::EnforceTravelFreezeCaps / BeginTravelHandoff):纯常量关系与纯判定,
//     加无 Redis、无 EventLoop 宿主里的 ECS 级接缝。时间一律注入(单调时钟的相对偏移),不读真实墙钟。
// ============================================================================
#include "services/scene/player/system/travel_freeze_cap.h"

namespace
{
namespace tfc = travel_freeze_cap;
using namespace std::chrono_literals;

constexpr Guid kCapPlayerId = 950300001;
constexpr SessionId kCapSession = 131301;
constexpr uint32_t kCapSelfZone = 1;
// 注入的"冻结起点":离时钟纪元 1 小时,保证与"未打点"(纪元)区分开,且减去任何用例里的偏移都不为负。
const tfc::Clock::time_point kT0 = tfc::Clock::time_point{} + std::chrono::hours(1);

// 一个交接在途的在线玩家:本节点 zone = kCapSelfZone,会话活着(SessionMap 映射到本玩家),交接组件与冻结组件
// 成对挂。requestedAtMs 0 = handoff 标记的 SET 还没发出;非 0 = 已发出。frozenAt = 单调冻结起点(纪元 = 未打点)。
entt::entity MakeFrozenHandoffPlayer(uint32_t targetZoneId, uint64_t requestedAtMs, uint64_t markEpoch,
                                     tfc::Clock::time_point frozenAt)
{
    tlsEcs.Clear();
    SessionMap().clear();
    TestLocalNodeInfo().set_zone_id(kCapSelfZone); // 身份由本例安装的对象持有,与 ECS Clear 无关。
    const auto player = tlsEcs.actorRegistry.create();
    CreatePersistedPlayerComps(player);
    tlsEcs.playerList.emplace(kCapPlayerId, player);
    tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kCapPlayerId);
    auto& session = tlsEcs.actorRegistry.emplace<PlayerSessionSnapshotComp>(player);
    session.set_gate_session_id(kCapSession);
    session.set_player_id(kCapPlayerId);
    SessionMap().insert_or_assign(kCapSession, kCapPlayerId);
    // 逐字段赋值(PlayerTravelHandoffComp 的约定)。
    auto& travel = tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
    travel.targetZoneId = targetZoneId;
    travel.requestedAtMs = requestedAtMs;
    travel.markEpoch = markEpoch;
    travel.frozenAtSteady = frozenAt;
    auto& frozen = tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    frozen.frozenAtMs = 1000; // 固定值,不读墙钟(AGENTS §11.4);只影响 frozen_ms 统计
    frozen.toZoneId = targetZoneId;
    return player;
}

travel_handoff_stats::Snapshot Stats()
{
    return travel_handoff_stats::Read();
}

// 本项新增的 8 个计数都没动。
void ExpectNoFreezeCapCounterMoved(const travel_handoff_stats::Snapshot& before,
                                   const travel_handoff_stats::Snapshot& after)
{
    EXPECT_EQ(after.freezeCapReached, before.freezeCapReached);
    EXPECT_EQ(after.dispatchWindowClosed, before.dispatchWindowClosed);
    EXPECT_EQ(after.markSentDestroyed, before.markSentDestroyed);
    EXPECT_EQ(after.markSentClientReset, before.markSentClientReset);
    EXPECT_EQ(after.destroyDeferredUnsettledSave, before.destroyDeferredUnsettledSave);
    EXPECT_EQ(after.freezeUnstamped, before.freezeUnstamped);
    EXPECT_EQ(after.watchdogEarlyFire, before.watchdogEarlyFire);
    EXPECT_EQ(after.handoffFastpathForced, before.handoffFastpathForced);
}
} // namespace

TEST(TravelFreezeCap, BudgetsKeepVerifiedPathInsideCap)
{
    // 与头文件的 static_assert 重复:把设计意图留在测试里,改常量时这里先红。
    EXPECT_TRUE(tfc::kSaveBudget <= tfc::kDispatchWindow) << "存盘看门狗先于晚发窗口关闭到期";
    EXPECT_TRUE(tfc::kDispatchWindow + tfc::kReplyBudget + tfc::kVerifyMargin <= tfc::kFreezeCap)
        << "窗口内发出的 EnterScene,其应答看门狗 + 一次核实在上限前做完";
    EXPECT_TRUE(tfc::kEarlyFireTolerance <= tfc::kVerifyMargin);
    EXPECT_TRUE(tfc::kFreezeCap + tfc::kSweepInterval < tfc::kClientAcceptedHandoffBudget)
        << "服务端最迟出结论早于客户端遮罩超时";
    EXPECT_TRUE(tfc::kDispatchWindow == 35s);
    EXPECT_TRUE(tfc::kFreezeCap == 70s);
}

TEST(TravelFreezeCap, NotDueBeforeCap)
{
    EXPECT_EQ(tfc::DecideFreezeCap(tfc::kFreezeCap - 1ms, true), tfc::FreezeCapAction::kNone);
    EXPECT_EQ(tfc::DecideFreezeCap(tfc::kFreezeCap - 1ms, false), tfc::FreezeCapAction::kNone);
}

TEST(TravelFreezeCap, UnfreezeOnlyWhenMarkNeverSent)
{
    EXPECT_FALSE(tfc::IsHandoffMarkSent(0));
    EXPECT_TRUE(tfc::IsHandoffMarkSent(123)) << "只看 requestedAtMs(在 SET 的 command() 之前写)";
    EXPECT_EQ(tfc::DecideFreezeCap(tfc::kFreezeCap, false), tfc::FreezeCapAction::kUnfreeze)
        << "恰好等于上限即到期";
}

TEST(TravelFreezeCap, DestroyOnceMarkSent)
{
    EXPECT_EQ(tfc::DecideFreezeCap(tfc::kFreezeCap, true), tfc::FreezeCapAction::kDestroy);
    EXPECT_EQ(tfc::DecideFreezeCap(tfc::kFreezeCap + 1h, true), tfc::FreezeCapAction::kDestroy)
        << "标记已发出后永不解冻";
}

TEST(TravelFreezeCap, DispatchWindowClosesBeforeReplyBudgetRunsOut)
{
    EXPECT_TRUE(tfc::IsDispatchWindowOpen(tfc::Clock::duration::zero()));
    EXPECT_TRUE(tfc::IsDispatchWindowOpen(tfc::kDispatchWindow - 1ms));
    EXPECT_FALSE(tfc::IsDispatchWindowOpen(tfc::kDispatchWindow));
    EXPECT_FALSE(tfc::IsDispatchWindowOpen(tfc::kFreezeCap));
    EXPECT_TRUE(tfc::kDispatchWindow + tfc::kReplyBudget < tfc::kFreezeCap);
}

TEST(TravelFreezeCap, NamesCoverEveryValue)
{
    for (const auto action : {tfc::FreezeCapAction::kNone, tfc::FreezeCapAction::kUnfreeze,
                              tfc::FreezeCapAction::kDestroy})
    {
        EXPECT_STRNE(tfc::FreezeCapActionName(action), "?") << "action=" << static_cast<uint32_t>(action);
    }
    for (const auto site : {tfc::MarkSentSite::kFreezeCap, tfc::MarkSentSite::kDispatchWindow,
                            tfc::MarkSentSite::kNoGateSession, tfc::MarkSentSite::kNoSceneManager,
                            tfc::MarkSentSite::kReceiptAnomaly})
    {
        EXPECT_STRNE(tfc::MarkSentSiteName(site), "?") << "site=" << static_cast<uint32_t>(site);
    }
    EXPECT_STREQ(tfc::FreezeCapActionName(tfc::FreezeCapAction::kCount), "?") << "越界不能读出数组外";
    EXPECT_STREQ(tfc::MarkSentSiteName(tfc::MarkSentSite::kCount), "?");
    EXPECT_STREQ(tfc::MarkSentSiteName(tfc::MarkSentSite::kFreezeCap), "travel_freeze_cap")
        << "runbook 按这段原文 grep(也是 DestroyDeposedPlayer 的 reasonTag)";
    EXPECT_STREQ(tfc::MarkSentSiteName(tfc::MarkSentSite::kReceiptAnomaly), "travel_receipt_anomaly")
        << "GO-2 判定表 B6 的收口点,runbook 同样按原文 grep";
}

TEST(TravelFreezeCap, RemainingUntilNeverNegative)
{
    const tfc::Clock::time_point due = tfc::Clock::time_point{} + 1h;
    EXPECT_TRUE(tfc::RemainingUntil(due, due - 3s) == 3s);
    EXPECT_TRUE(tfc::RemainingUntil(due, due) == tfc::Clock::duration::zero());
    EXPECT_TRUE(tfc::RemainingUntil(due, due + 1s) == tfc::Clock::duration::zero()) << "已过期按 0,不返回负值";
}

TEST(TravelFreezeCap, EarlyFireWithinToleranceRunsInsteadOfRearming)
{
    const tfc::Clock::time_point due = tfc::Clock::time_point{} + 1h;
    EXPECT_FALSE(tfc::ShouldRearmEarlyFire(due, due - 999ms)) << "亚秒级的时钟差不计数、直接执行";
    EXPECT_FALSE(tfc::ShouldRearmEarlyFire(due, due - 1s)) << "恰好等于容差不重挂";
    EXPECT_TRUE(tfc::ShouldRearmEarlyFire(due, due - 1001ms)) << "墙钟前跳 ≥1s:按剩余时间重挂";
    EXPECT_FALSE(tfc::ShouldRearmEarlyFire(due, due));
    EXPECT_FALSE(tfc::ShouldRearmEarlyFire(due, due + 1s));
}

// ── ECS 级接缝:直接驱动 PlayerLifecycleSystem::EnforceTravelFreezeCaps / BeginTravelHandoff ──
// 宿主里没有 zone Redis、没有玩家数据 Redis 客户端(HasUnsettledPlayerSave 恒为 false、真写盘会解引用空指针)、
// 没有 EventLoop、gate 注册表为空(发 tip / 34 只打 ERROR 就返回)。"会话活着 → 决定踢线"的可观察结果是
// mark_sent_client_reset 计数。

TEST(TravelFreezeCapEcs, NotYetDueLeavesHandoffAlone)
{
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/123, /*markEpoch=*/7, kT0);
    const auto before = Stats();

    PlayerLifecycleSystem::EnforceTravelFreezeCaps(kT0 + tfc::kFreezeCap - 1ms);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    ExpectNoFreezeCapCounterMoved(before, Stats());
}

TEST(TravelFreezeCapEcs, UnstampedFreezeIsStampedNotExpired)
{
    // 漏打点(frozenAtSteady 为纪元)只会晚处置,不会下一拍就被销毁:第一次扫描就地补记为当前时刻。
    const auto player =
        MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/123, /*markEpoch=*/7, tfc::Clock::time_point{});
    const auto before = Stats();

    PlayerLifecycleSystem::EnforceTravelFreezeCaps(kT0);
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    const auto* travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
    ASSERT_NE(travel, nullptr);
    EXPECT_TRUE(travel->frozenAtSteady == kT0) << "补记为扫描时刻";
    EXPECT_EQ(Stats().freezeUnstamped, before.freezeUnstamped + 1);

    PlayerLifecycleSystem::EnforceTravelFreezeCaps(kT0 + tfc::kFreezeCap - 1ms);
    EXPECT_TRUE(tlsEcs.actorRegistry.valid(player)) << "从补记时刻起算,还没到上限";

    PlayerLifecycleSystem::EnforceTravelFreezeCaps(kT0 + tfc::kFreezeCap);
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(Stats().markSentDestroyed, before.markSentDestroyed + 1);
    EXPECT_EQ(Stats().freezeUnstamped, before.freezeUnstamped + 1) << "只补记一次";
}

TEST(TravelFreezeCapEcs, UnconfirmedRevocationAtCapDestroysWithoutPersisting)
{
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/0, /*markEpoch=*/0, kT0);
    const auto before = Stats();

    PlayerLifecycleSystem::EnforceTravelFreezeCaps(kT0 + tfc::kFreezeCap);

    const auto after = Stats();
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player)) << "撤资格未确认，硬上限不存盘销毁而不解冻";
    EXPECT_EQ(after.freezeCapReached, before.freezeCapReached + 1);
    EXPECT_EQ(after.aborted, before.aborted + 1);
    EXPECT_EQ(after.markSentDestroyed, before.markSentDestroyed);
}

TEST(TravelFreezeCapEcs, MarkSentDestroysWithoutPersistingAndResetsClient)
{
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/123, /*markEpoch=*/7, kT0);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 7;
    const auto before = Stats();

    PlayerLifecycleSystem::EnforceTravelFreezeCaps(kT0 + tfc::kFreezeCap);

    const auto after = Stats();
    // 宿主里真写盘会解引用空的玩家数据 Redis 客户端:走到这里没崩,本身就证明没有存盘。
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player)) << "标记已发出:永不解冻,不存盘销毁";
    EXPECT_EQ(tlsEcs.playerList.count(kCapPlayerId), 0u);
    EXPECT_EQ(SessionMap().count(kCapSession), 0u);
    EXPECT_EQ(after.freezeCapReached, before.freezeCapReached + 1);
    EXPECT_EQ(after.markSentDestroyed, before.markSentDestroyed + 1);
    EXPECT_EQ(after.markSentClientReset, before.markSentClientReset + 1) << "会话活着:销毁前发 tip + 踢线 34";
    EXPECT_EQ(after.granted, before.granted);
    EXPECT_EQ(after.aborted, before.aborted);
    EXPECT_EQ(after.destroyDeferredUnsettledSave, before.destroyDeferredUnsettledSave);

    // 上限处置不往待撤回表里加条目:标记留作释放标记(A1′ 同义),同一玩家立刻可以再发起。
    const auto reentered = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<PlayerGuidComp>(reentered, kCapPlayerId);
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(reentered));
}

TEST(TravelFreezeCapEcs, SameZoneMarkSentAlsoResetsClient)
{
    // 有意为之:到上限时 epoch 状态未知,宁可多重登一次,也不留哑连接。
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone, /*requestedAtMs=*/123, /*markEpoch=*/7, kT0);
    const auto before = Stats();

    PlayerLifecycleSystem::EnforceTravelFreezeCaps(kT0 + tfc::kFreezeCap);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(Stats().markSentClientReset, before.markSentClientReset + 1);
}

TEST(TravelFreezeCapEcs, ExitingEntityPastCapIsDestroyedWithoutKick)
{
    // 退出中的实体不再被排除(骨架没有例外);客户端已断开,不踢线。
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/123, /*markEpoch=*/7, kT0);
    tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
    PlayerExitIntentComp intent;
    intent.sessionAtExit = kCapSession;
    tlsEcs.actorRegistry.emplace<PlayerExitIntentComp>(player, intent);
    const auto before = Stats();

    PlayerLifecycleSystem::EnforceTravelFreezeCaps(kT0 + tfc::kFreezeCap);

    const auto after = Stats();
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(after.markSentDestroyed, before.markSentDestroyed + 1);
    EXPECT_EQ(after.markSentClientReset, before.markSentClientReset) << "退出中不踢";
}

TEST(TravelFreezeCapEcs, PastDispatchWindowRequiresRevocationBeforeUnfreeze)
{
    // 关键证据是 dispatch_window_closed:没有这道闸时代码会走到"zone redis 未连接"分支,那条分支同样让 aborted +1,
    // 但不会动 dispatch_window_closed。时间只用 steady 时钟的相对偏移,余量 1s,结果确定。
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/0, /*markEpoch=*/0,
                                                tfc::Clock::now() - (tfc::kDispatchWindow + 1s));
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 7;
    const auto before = Stats();

    PlayerLifecycleSystem::BeginTravelHandoff(kCapPlayerId);

    const auto after = Stats();
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_EQ(after.dispatchWindowClosed, before.dispatchWindowClosed + 1);
    EXPECT_EQ(after.aborted, before.aborted);
    EXPECT_EQ(after.sqlBarrierExpired, before.sqlBarrierExpired) << "没等过 SQL ACK,不算这一类";
    EXPECT_TRUE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "原资格未确认撤销，不解冻";
}

TEST(TravelFreezeCapEcs, BeginTravelHandoffPastWindowWhileWaitingForSqlAcksCountsSqlBarrierExpiry)
{
    // 交接 SQL 屏障:标记脚本因两张子表 SQL ACK 没追平而回 0、每 250ms 重试,直到 35s 晚发闸。窗口在等待中关闭时,
    // 除了既有的 dispatch_window_closed,还要计 sql_barrier_expired,把"等 SQL ACK 超时"与"存盘落地太晚"分开。
    // 等待计数只能由 Redis 回调写(宿主里没有 zone Redis),这里直接在组件上置 3,模拟已经等过三轮。
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/0, /*markEpoch=*/0,
                                                tfc::Clock::now() - (tfc::kDispatchWindow + 1s));
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 7;
    tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(player).sqlBarrierWaits = 3;
    const auto before = Stats();

    PlayerLifecycleSystem::BeginTravelHandoff(kCapPlayerId);

    const auto after = Stats();
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "无Redis撤资格确认，仍保持冻结";
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_EQ(after.dispatchWindowClosed, before.dispatchWindowClosed + 1);
    EXPECT_EQ(after.sqlBarrierExpired, before.sqlBarrierExpired + 1);
    EXPECT_EQ(after.aborted, before.aborted);
    EXPECT_EQ(after.sqlBarrierWaited, before.sqlBarrierWaited) << "等待计数只在回调里第一次等待时加,这里不动";
    EXPECT_TRUE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "原资格未确认撤销，不解冻";
}

TEST(TravelFreezeCapEcs, BeginTravelHandoffInsideWindowIsNotRefusedByTheGate)
{
    // 对照组:窗口内这道闸不生效,且排在 epoch 检查之前 —— 没有 owner_epoch 时走原有的"epoch unknown"分支 Abort。
    const auto player =
        MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/0, /*markEpoch=*/0, tfc::Clock::now());
    const auto before = Stats();

    PlayerLifecycleSystem::BeginTravelHandoff(kCapPlayerId);

    const auto after = Stats();
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_EQ(after.aborted, before.aborted);
    EXPECT_EQ(after.dispatchWindowClosed, before.dispatchWindowClosed);
}

TEST(TravelFreezeCapEcs, LateMarkSetErrorOfOlderGenerationLeavesNewHandoffFrozen)
{
    // 冻结上限审查留下的问题:SET 回调的 ERROR 分支只在代际匹配时清 markEpoch,随后的 Abort 却不看代际。
    // 旧一代被上限销毁后同节点重登、又发起了新一代(SET 已发出:requestedAtMs = 456,markEpoch = 7),旧一代(123)
    // 的 ERROR 应答迟到时不得动新一代:不解冻(骨架 I2 / I3),也不按新一代的 markEpoch 登记撤回。
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/456, /*markEpoch=*/7, kT0);
    const auto before = Stats();

    PlayerLifecycleSystem::HandleTravelMarkWriteRejected(kCapPlayerId, /*requestedAtMs=*/123, "READONLY");

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    const auto* travel = tlsEcs.actorRegistry.try_get<PlayerTravelHandoffComp>(player);
    ASSERT_NE(travel, nullptr) << "旧一代的 ERROR 应答不得解冻新一代";
    EXPECT_EQ(travel->requestedAtMs, uint64_t{456});
    EXPECT_EQ(travel->markEpoch, uint64_t{7}) << "也不得清新一代的 markEpoch";
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_EQ(Stats().aborted, before.aborted);
    EXPECT_EQ(Stats().resolvedInPlace, before.resolvedInPlace);

    // 对照:本代的 ERROR 应答照旧解冻 + 回 tip;标记确定不存在,不登记撤回。
    PlayerLifecycleSystem::HandleTravelMarkWriteRejected(kCapPlayerId, /*requestedAtMs=*/456, "READONLY");

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_EQ(Stats().aborted, before.aborted);
    EXPECT_TRUE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "mark缺席也必须先撤资格";

    // 实体已不在时同样 no-op(不崩、不计数)。
    tlsEcs.Clear();
    PlayerLifecycleSystem::HandleTravelMarkWriteRejected(kCapPlayerId, /*requestedAtMs=*/456, "READONLY");
    EXPECT_EQ(Stats().aborted, before.aborted);
}

// ============================================================================
// 13. TravelOwnership —— GO-2 根治的源端判定(player_lifecycle.h 的 travel_outcome::ClassifyOwnership 与
// kLuaJudgeTravelOutcome;cross-zone-scene-travel.md §12.8 判定表 B4–B9)。纯函数,不需要 Redis / 场景宿主。
// 取证脚本在 Redis 上的行为(只删本族标记、读到回执、与 A2′ / 回滚 Lua 的互斥)由 Go 侧跨语言金样
// go/scene_manager/internal/logic/owner_epoch_crosslang_test.go 在 miniredis 上覆盖;下面用栈构造的正常 Redis
// 应答补测 B5 缺组件时的拒绝采纳。成功采纳后的真实落盘仍靠 runbook B4a / B4b 实跑。
// ============================================================================
#include "proto/scene_manager/storage.pb.h"
#include "player/system/dirty_save_stats.h"

// 严格限定为取证应答入口:借用调用期间有效的 reply,不暴露保存/销毁等私有操作。
class PlayerTravelOutcomeTestAccess
{
public:
    static void JudgeReply(Guid playerId, uint64_t requestedAtMs, uint64_t cachedEpoch,
                           const redisReply& reply)
    {
        PlayerLifecycleSystem::JudgeTravelOutcomeReply(playerId, requestedAtMs, cachedEpoch,
            "B5 missing persisted component regression", travel_outcome::Evidence::kNoReply, &reply);
    }

    // AbortTravelHandoff 是私有的:存盘看门狗 / 晚发闸 / 冻结上限都经它收口,这里直接驱动这个共同出口。
    static void Abort(Guid playerId, const char* reason)
    {
        PlayerLifecycleSystem::AbortTravelHandoff(playerId, reason);
    }
};

TEST(TravelFreezeCapEcs, SaveWatchdogAbortWhileWaitingForSqlAcksCountsSqlBarrierExpiry)
{
    // 主路径:交接存盘在途(kQueued)时挂 30s 存盘看门狗,它先于 35s 晚发闸到期。两次 SQL ACK 重试之间 requestedAtMs 为 0,
    // 看门狗照常 AbortTravelHandoff —— 归因在这个共同出口做,同样要计 sql_barrier_expired。
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/0, /*markEpoch=*/0, kT0);
    tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(player).sqlBarrierWaits = 2;
    const auto before = Stats();

    PlayerTravelOutcomeTestAccess::Abort(kCapPlayerId, "handoff save did not land in time");

    const auto after = Stats();
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_EQ(after.sqlBarrierExpired, before.sqlBarrierExpired + 1);
    EXPECT_EQ(after.aborted, before.aborted);
    EXPECT_EQ(after.dispatchWindowClosed, before.dispatchWindowClosed) << "不是晚发闸收的口";
}

TEST(TravelFreezeCapEcs, AbortWithMarkWriteInFlightIsNotSqlBarrierExpiry)
{
    // 对照组:标记脚本在途(requestedAtMs != 0)时中止,不是"在等 SQL ACK";没等过(sqlBarrierWaits == 0)也不算。
    // markEpoch 保持 0,Abort 不登记撤回,宿主里不需要 zone Redis。
    const auto inFlight = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/123, /*markEpoch=*/0, kT0);
    tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(inFlight).sqlBarrierWaits = 2;
    const auto before = Stats();

    PlayerTravelOutcomeTestAccess::Abort(kCapPlayerId, "test abort while the mark write is in flight");

    const auto after = Stats();
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(inFlight));
    EXPECT_EQ(after.sqlBarrierExpired, before.sqlBarrierExpired);
    EXPECT_EQ(after.aborted, before.aborted);
}


namespace
{
constexpr to::Ownership kAllOwnership[] = {
    to::Ownership::kUnchanged,       to::Ownership::kRolledBackToSelf, to::Ownership::kReceiptAnomaly,
    to::Ownership::kReturnedToSelf,  to::Ownership::kMovedElsewhere,   to::Ownership::kLocationUnknown,
};
static_assert(std::size(kAllOwnership) == to::kOwnershipCount, "新增结论时补进这张表,并补判定用例");

// B5 的标准形态:缓存 5、标记 "5:t"、回滚之后 7,location 指回本节点本 zone 且记 7,回执是本次标记原文。
constexpr to::OwnershipFacts RolledBackToSelfFacts()
{
    to::OwnershipFacts facts;
    facts.cachedEpoch = 5;
    facts.markEpoch = 5;
    facts.redisEpoch = 7;
    facts.locationParsed = true;
    facts.locationOnSelf = true;
    facts.locationOwnerEpoch = 7;
    facts.receiptIsMine = true;
    return facts;
}

int g_rollbackMissingPersistedLines = 0;

void CountRollbackMissingPersisted(const char* message, int length)
{
    if (std::string_view(message, static_cast<std::size_t>(length)).find(
            "[ZoneTravel][RollbackAdopt] persisted component missing for player ") != std::string_view::npos)
    {
        ++g_rollbackMissingPersistedLines;
    }
    std::fwrite(message, 1, static_cast<std::size_t>(length), stdout);
}
} // namespace

TEST(TravelOwnership, UnchangedEpochWinsRegardlessOfLocation)
{
    // B4:epoch 未变最先判。取证脚本已原子删掉本族标记,"未变"= 此前没放行、此后放不出去 —— location 与回执怎样都不影响。
    for (const bool parsed : {false, true})
    {
        for (const bool onSelf : {false, true})
        {
            for (const bool receipt : {false, true})
            {
                to::OwnershipFacts facts;
                facts.cachedEpoch = 5;
                facts.markEpoch = 5;
                facts.redisEpoch = 5;
                facts.locationParsed = parsed;
                facts.locationOnSelf = parsed && onSelf;
                facts.locationOwnerEpoch = 7;
                facts.receiptIsMine = parsed && receipt;
                EXPECT_EQ(to::ClassifyOwnership(facts), to::Ownership::kUnchanged)
                    << "parsed=" << parsed << " onSelf=" << onSelf << " receipt=" << receipt;
            }
        }
    }
}

TEST(TravelOwnership, ReceiptOfThisHandoffIsAdopted)
{
    static_assert(to::ClassifyOwnership(RolledBackToSelfFacts()) == to::Ownership::kRolledBackToSelf,
                  "判定必须能在编译期求值(纯函数)");
    EXPECT_EQ(to::ClassifyOwnership(RolledBackToSelfFacts()), to::Ownership::kRolledBackToSelf)
        << "回执是本次原文,epoch 恰好前进两格且与 location 一致,location 指回本节点:采纳";
}

// 回执、回复形状、玩家与交接代际都有效,必须进入真实 B5。缺组件应在采纳 epoch / 解冻之前拒绝。
// 去掉生产 B5 的完整性检查后,epoch 会变成 7、交接和冻结会被摘掉,下面的断言即失败;
// 强制保存仍会被保存提交点的缺组件检查拒绝,所以修前红不依赖空 Redis 客户端崩溃。
TEST(TravelOwnershipEcs, RollbackWithMissingPersistedComponentKeepsFrozenState)
{
    ScopedPersistedPlayerCleanup cleanup;
    constexpr uint64_t kCachedEpoch = 5;
    constexpr uint64_t kRollbackEpoch = 7;
    constexpr uint64_t kRequestedAtMs = 456;
    constexpr uint32_t kSelfNodeId = 31;
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, kRequestedAtMs, kCachedEpoch, kT0);
    TestLocalNodeInfo().set_node_id(kSelfNodeId);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = kCachedEpoch;

    PlayerAllData persisted;
    // 与写盘同一个形状(统一入口,带持久 schema 世代),落盘快照才与生产的 marshal 结果可比。
    ASSERT_TRUE(player_persist::MarshalPlayerForPersist(player, kCapPlayerId, persisted));
    tlsEcs.actorRegistry.emplace<PlayerLastPersistedSnapshotComp>(player).Replace(persisted);
    const auto persistedBytes = persisted.SerializeAsString();
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);

    storage::PlayerLocation location;
    location.set_zone_id(kCapSelfZone);
    location.set_node_id(std::to_string(kSelfNodeId));
    location.set_owner_epoch(kRollbackEpoch);
    location.set_rollback_receipt(player_ownership::HandoffRedisValue(kCachedEpoch, kRequestedAtMs));
    std::string locationBytes;
    ASSERT_TRUE(location.SerializeToString(&locationBytes));

    // hiredis 回复只在本次同步调用中借用这些栈对象和字符串,不经过连接、不交给 freeReplyObject。
    char epochText[] = "7";
    redisReply epochReply{};
    epochReply.type = REDIS_REPLY_STRING;
    epochReply.str = epochText;
    epochReply.len = sizeof(epochText) - 1;
    redisReply locationReply{};
    locationReply.type = REDIS_REPLY_STRING;
    locationReply.str = locationBytes.data();
    locationReply.len = locationBytes.size();
    redisReply* elements[] = {&epochReply, &locationReply};
    redisReply reply{};
    reply.type = REDIS_REPLY_ARRAY;
    reply.elements = std::size(elements);
    reply.element = elements;

    storage::PlayerLocation parsedLocation;
    ASSERT_TRUE(player_ownership::ParsePlayerLocationElement(&locationReply, parsedLocation));
    auto facts = RolledBackToSelfFacts();
    facts.locationParsed = true;
    facts.locationOnSelf = parsedLocation.zone_id() == GetZoneId() &&
        parsedLocation.node_id() == std::to_string(GetNodeInfo().node_id());
    facts.locationOwnerEpoch = parsedLocation.owner_epoch();
    facts.receiptIsMine = parsedLocation.rollback_receipt() ==
        player_ownership::HandoffRedisValue(kCachedEpoch, kRequestedAtMs);
    ASSERT_EQ(to::ClassifyOwnership(facts), to::Ownership::kRolledBackToSelf);
    ASSERT_EQ(tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(player).requestedAtMs, kRequestedAtMs);
    const auto before = travel_handoff_stats::Read();
    const auto savesBefore = dirty_save_stats::Read();
    g_rollbackMissingPersistedLines = 0;
    muduo::Logger::setOutput(CountRollbackMissingPersisted);
    PlayerTravelOutcomeTestAccess::JudgeReply(kCapPlayerId, kRequestedAtMs, kCachedEpoch, reply);
    muduo::Logger::setOutput(WriteLogToStdout);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    ASSERT_TRUE((tlsEcs.actorRegistry.all_of<PlayerTravelHandoffComp, PlayerFrozenComp, PlayerOwnerEpochComp>(player)));
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch, kCachedEpoch);
    const auto& travel = tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(player);
    EXPECT_EQ(travel.requestedAtMs, kRequestedAtMs);
    EXPECT_EQ(travel.markEpoch, kCachedEpoch);
    EXPECT_EQ(travel_handoff_stats::Read().rolledBackAdopted, before.rolledBackAdopted);
    EXPECT_EQ(travel_handoff_stats::Read().aborted, before.aborted);
    EXPECT_EQ(travel_handoff_stats::Read().verifyRearmed, before.verifyRearmed);
    EXPECT_EQ(dirty_save_stats::Read().total, savesBefore.total);
    EXPECT_EQ(g_rollbackMissingPersistedLines, 1) << "必须命中 B5 缺组件拒绝,不能因坏回复或代际不符伪绿";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerAssetOpLedgerComp>(player));
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerLastPersistedSnapshotComp>(player).snapshot->SerializeAsString(), persistedBytes);
}

TEST(TravelOwnership, ReturnedAfterGrantIsNotMistakenForRollback)
{
    // 反例原样(§12.8 第五节):先放行给 B(6),A 的应答丢失;B 又以 "6:u" 交接回 A(7,location 写回 A 的新字节,
    // 没有回执)。与上一条数值完全相同,只差回执:永远不采纳,销毁(不回档、不双持有)。
    auto facts = RolledBackToSelfFacts();
    facts.receiptIsMine = false;
    EXPECT_EQ(to::ClassifyOwnership(facts), to::Ownership::kReturnedToSelf);
    EXPECT_NE(to::ClassifyOwnership(facts), to::Ownership::kRolledBackToSelf);
}

TEST(TravelOwnership, ReceiptWithFailedCrossCheckIsAnomaly)
{
    struct Case
    {
        std::string_view name;
        to::OwnershipFacts facts;
    };
    std::vector<Case> cases;
    {
        auto facts = RolledBackToSelfFacts();
        facts.redisEpoch = 6; // 只前进一格(不是 bump 回滚的形态)
        facts.locationOwnerEpoch = 6;
        cases.push_back({"redis=6", facts});
    }
    {
        auto facts = RolledBackToSelfFacts();
        facts.redisEpoch = 8; // 回滚之后又前进过
        facts.locationOwnerEpoch = 8;
        cases.push_back({"redis=8", facts});
    }
    {
        auto facts = RolledBackToSelfFacts();
        facts.locationOwnerEpoch = 6; // location 记的 epoch 与键不一致(键被淘汰后补种 / 写坏)
        cases.push_back({"locEpoch=6", facts});
    }
    {
        auto facts = RolledBackToSelfFacts();
        facts.locationOnSelf = false; // location 不指回本节点本 zone
        cases.push_back({"onSelf=false", facts});
    }
    {
        auto facts = RolledBackToSelfFacts();
        facts.markEpoch = 4; // 写标记用的 epoch 不是缓存值
        cases.push_back({"markEpoch=4", facts});
    }
    {
        auto facts = RolledBackToSelfFacts();
        facts.markEpoch = 0; // SET 没发出去过,不可能有本次回执
        cases.push_back({"markEpoch=0", facts});
    }
    for (const auto &c : cases)
    {
        EXPECT_EQ(to::ClassifyOwnership(c.facts), to::Ownership::kReceiptAnomaly) << c.name;
    }
}

TEST(TravelOwnership, MovedElsewhereUnknownAndEvictedKey)
{
    to::OwnershipFacts moved;
    moved.cachedEpoch = 5;
    moved.markEpoch = 5;
    moved.redisEpoch = 6;
    moved.locationParsed = true;
    moved.locationOnSelf = false;
    moved.locationOwnerEpoch = 6;
    EXPECT_EQ(to::ClassifyOwnership(moved), to::Ownership::kMovedElsewhere) << "B8:已放行到别处";

    auto unknown = moved;
    unknown.locationParsed = false;
    unknown.locationOnSelf = false;
    EXPECT_EQ(to::ClassifyOwnership(unknown), to::Ownership::kLocationUnknown) << "B9:location 缺失 / 写坏";

    auto evicted = moved;
    evicted.redisEpoch = 0; // owner_epoch 键被淘汰读成 0
    evicted.locationOnSelf = true;
    evicted.locationOwnerEpoch = 5;
    EXPECT_EQ(to::ClassifyOwnership(evicted), to::Ownership::kReturnedToSelf) << "B7:键被淘汰,不采纳";

    auto evictedWithReceipt = evicted;
    evictedWithReceipt.receiptIsMine = true;
    EXPECT_EQ(to::ClassifyOwnership(evictedWithReceipt), to::Ownership::kReceiptAnomaly)
        << "B6:回执对上但 epoch 读成 0,交叉校验不成立";

    for (const auto &facts : {moved, unknown, evicted, evictedWithReceipt})
    {
        EXPECT_NE(to::ClassifyOwnership(facts), to::Ownership::kRolledBackToSelf);
    }
}

TEST(TravelOwnership, NameTableCoversEveryVerdict)
{
    for (const auto ownership : kAllOwnership)
    {
        const std::string name = to::OwnershipName(ownership);
        EXPECT_FALSE(name.empty()) << "ownership=" << static_cast<uint32_t>(ownership);
        EXPECT_NE(name, "?") << "ownership=" << static_cast<uint32_t>(ownership);
    }
    EXPECT_STREQ(to::OwnershipName(to::Ownership::kCount), "?") << "越界不能读出数组外";
}

TEST(TravelOwnership, JudgeRevokesOriginalHolderBeforeReadingOwnership)
{
    const std::string lua = to::kLuaJudgeTravelOutcome;
    EXPECT_LT(lua.find("if not types3()"), lua.find("redis.call('HSET'"));
    EXPECT_LT(lua.find("raw~=ARGV[1] and raw~=ARGV[2] and raw~=ARGV[3]"), lua.find("redis.call('HSET'"));
    EXPECT_LT(lua.find("redis.call('HSET'"), lua.find("redis.call('DEL'"));
    EXPECT_LT(lua.find("redis.call('DEL'"), lua.find("return {owner"));
    EXPECT_EQ(lua.find("DEL',KEYS[1]"), std::string::npos) << "不删owner";
    EXPECT_EQ(lua.find("DEL',location"), std::string::npos) << "不删location";
}

// ============================================================================
// 14. RelocateConfirm —— 疏散 / 排空改派的待确认表(relocate_confirm.h;cross-zone-scene-travel.md
//     「CPP-3 疏散 / 排空改派的待确认表」):票据作废、应答 / 传输失败按关联号认领、核实读数的裁决、settle 规则、
//     凭证补写的六值判定、无法核实时的盲踢规则,以及 Table 的阶段迁移与到期归类。纯函数与纯容器的用例时间一律注入
//     (单调时钟的相对偏移,复用第 12 节的 kT0),不读任何时钟。节尾是无 scene_manager / gate / Redis 宿主里的
//     ECS 级接缝(RelocateConfirmEcs.*):登记与进场 Reconcile 走真实的单调时钟,扫描时刻以它为基准加偏移注入。
// ============================================================================
#include "services/scene/player/system/relocate_confirm.h"
#include "modules/scene/comp/scene_comp.h"      // SceneEntityComp:ECS 用例把玩家放进源场景
#include <proto/scene/scene_info.pb.h>          // SceneInfoComp
#include <algorithm>                            // std::max(本节直接用到,不靠 relocate_confirm.h 传递包含)
#include <ostream>                              // 下面给枚举写的 PrintTo
#include <utility>                              // std::pair

// 让 gtest 在断言失败时打出枚举的名字,而不是「1-byte object <02>」。relocate_confirm.h 刻意不含 <ostream>,
// 所以写在测试里;放进枚举所在的命名空间,gtest 靠 ADL 找到它。只影响失败时的输出,不影响任何断言的真假。
namespace relocate_confirm
{
inline void PrintTo(Phase value, std::ostream *os) { *os << PhaseName(value); }
inline void PrintTo(Evidence value, std::ostream *os) { *os << EvidenceName(value); }
inline void PrintTo(Expectation value, std::ostream *os) { *os << ExpectationName(value); }
inline void PrintTo(Verdict value, std::ostream *os) { *os << VerdictName(value); }
inline void PrintTo(VerifyAction value, std::ostream *os) { *os << VerifyActionName(value); }
inline void PrintTo(LandingAction value, std::ostream *os) { *os << LandingActionName(value); }
inline void PrintTo(TicketDecision value, std::ostream *os) { *os << TicketDecisionName(value); }
inline void PrintTo(Outcome value, std::ostream *os) { *os << OutcomeName(value); }
inline void PrintTo(GatePushResult value, std::ostream *os) { *os << GatePushResultName(value); }
inline void PrintTo(MarkWrite value, std::ostream *os) { *os << MarkWriteName(value); }
inline void PrintTo(ClaimAction value, std::ostream *os) { *os << ClaimActionName(value); }
inline void PrintTo(CredentialAction value, std::ostream *os) { *os << CredentialActionName(value); }
} // namespace relocate_confirm

namespace
{
namespace rc = relocate_confirm;

constexpr uint64_t kRelocatePlayerA = 950400001;
constexpr uint64_t kRelocatePlayerB = 950400002;
constexpr uint64_t kRelocatePlayerC = 950400003;
constexpr uint32_t kRelocateSelfZone = 3;
constexpr uint64_t kRelocateSourceScene = 77;

constexpr rc::Phase kAllRelocatePhases[] = {
    rc::Phase::kMarkWriting,
    rc::Phase::kAwaitingReply,
    rc::Phase::kVerifying,
    rc::Phase::kLanding,
};
static_assert(std::size(kAllRelocatePhases) == rc::kPhaseCount, "新增阶段时补进这张表,并补认领 / 受理判据的用例");

constexpr rc::Evidence kAllRelocateEvidence[] = {
    rc::Evidence::kNotSent,          rc::Evidence::kReplyRejected,    rc::Evidence::kReplySucceeded,
    rc::Evidence::kNoReply,          rc::Evidence::kTransportFailed,  rc::Evidence::kLandingAbandoned,
    rc::Evidence::kLandingTimeout,
};
static_assert(std::size(kAllRelocateEvidence) == rc::kEvidenceCount, "新增证据时补进这张表,并补三个踢线 / 读时机判定的用例");

constexpr rc::Verdict kAllRelocateVerdicts[] = {
    rc::Verdict::kAbsent,          rc::Verdict::kUnchanged,     rc::Verdict::kMovedHere,
    rc::Verdict::kMovedElsewhere,  rc::Verdict::kIndeterminate,
};
static_assert(std::size(kAllRelocateVerdicts) == rc::kVerdictCount, "新增读数结论时补进这张表,并补裁决 / 凭证用例");

// (playerId, seq):CollectDue 三个列表里的元素。
using RelocateKey = std::pair<uint64_t, uint64_t>;

// 当前配置下的预算:C++ 发往 scene_manager 的调用 deadline 10000ms → settle 15s、等完成通知 30s。
constexpr rc::Budgets RelocateBudgets()
{
    return rc::BudgetsFor(10000ms);
}

rc::Registration MakeRelocateRegistration(uint64_t playerId, bool earlierEnterSceneMayReply = false)
{
    rc::Registration registration;
    registration.playerId = playerId;
    registration.sessionId = kSessionAtExit;
    registration.gateNodeId = 1;
    registration.gateInstanceId = "gate-uuid";
    registration.ownerEpoch = 5;
    registration.sourceSceneId = kRelocateSourceScene;
    registration.earlierEnterSceneMayReply = earlierEnterSceneMayReply;
    return registration;
}

// 登记一条并返回表内指针。Table 用例里没有结清动作,指针在用例内一直有效(节点式容器,再登记别人也不失效)。
rc::Entry *AddRelocateEntry(rc::Table &table, uint64_t playerId, rc::Clock::time_point now,
                            bool earlierEnterSceneMayReply = false)
{
    std::optional<rc::Entry> superseded;
    const uint64_t seq =
        table.Add(MakeRelocateRegistration(playerId, earlierEnterSceneMayReply), now, RelocateBudgets(), superseded);
    EXPECT_NE(seq, 0u);
    EXPECT_FALSE(superseded.has_value());
    return table.Find(playerId, seq);
}

bool RelocateDueIsEmpty(const rc::Table::Due &due)
{
    return due.toVerify.empty() && due.verifyExpired.empty() && due.landingToCheck.empty() &&
           due.markWriteTimedOut == 0 && due.replyTimedOut == 0;
}

// 一份「没变」的标准读数:location 在、解析成功、指向本节点本 zone 的源场景,不在疏散。用例各改一项。
constexpr rc::PlacementFacts RelocatePlacementFacts(rc::Expectation expectation)
{
    rc::PlacementFacts facts;
    facts.expectation = expectation;
    facts.present = true;
    facts.parsed = true;
    facts.locationZoneId = kRelocateSelfZone;
    facts.selfZoneId = kRelocateSelfZone;
    facts.nodeIsSelf = true;
    facts.locationSceneId = kRelocateSourceScene;
    facts.sourceSceneId = kRelocateSourceScene;
    facts.evacuating = false;
    return facts;
}

// 记法:(读数结论, 读到的 owner_epoch, 已有同代标记, 本节点可能持有, 身份已确认, dev 旁路)。
constexpr rc::CredentialAction DecideRelocateCredential(rc::Verdict verdict, uint64_t redisOwnerEpoch,
                                                        bool markCarriesEpoch, bool localHolderPossible,
                                                        bool identityConfirmed, bool devUnsafeCrossNode)
{
    rc::CredentialFacts facts;
    facts.verdict = verdict;
    facts.redisOwnerEpoch = redisOwnerEpoch;
    facts.markCarriesEpoch = markCarriesEpoch;
    facts.localHolderPossible = localHolderPossible;
    facts.identityConfirmed = identityConfirmed;
    facts.devUnsafeCrossNode = devUnsafeCrossNode;
    return rc::DecideCredential(facts);
}

template <typename Enum>
void ExpectRelocateNamesCoverEveryValue(const char *enumName, std::size_t count, const char *(*nameOf)(Enum))
{
    for (std::size_t i = 0; i < count; ++i)
    {
        const std::string name = nameOf(static_cast<Enum>(i));
        EXPECT_FALSE(name.empty()) << enumName << "=" << i;
        EXPECT_NE(name, "?") << enumName << "=" << i;
    }
    EXPECT_STREQ(nameOf(static_cast<Enum>(count)), "?") << enumName << ":越界不能读出数组外";
}
} // namespace

// ── 票据作废、更早请求、认领 ──

TEST(RelocateConfirmTicket, ClientGoneOrReplacedSessionVoidsTheTicket)
{
    // 记法:(clientDisconnected, currentSessionBound, currentIsTicketSession)。
    static_assert(rc::DecideTicket(false, true, true) == rc::TicketDecision::kDispatch,
                  "判定必须能在编译期求值(纯函数)");
    EXPECT_EQ(rc::DecideTicket(true, true, true), rc::TicketDecision::kVoidClientGone);
    EXPECT_EQ(rc::DecideTicket(true, false, false), rc::TicketDecision::kVoidClientGone)
        << "clientDisconnected 优先:会话已死,给它改派只会把 location 留在一个从未载入该玩家的节点上";
    EXPECT_EQ(rc::DecideTicket(false, true, false), rc::TicketDecision::kVoidSessionReplaced);
    EXPECT_EQ(rc::DecideTicket(false, true, true), rc::TicketDecision::kDispatch);
    EXPECT_EQ(rc::DecideTicket(false, false, false), rc::TicketDecision::kDispatch)
        << "快照已无会话不是「换了一条会话」的证据:仍按票据派发";
}

TEST(RelocateConfirmTicket, ReentryCancelsTheTicketExceptForTheSameSessionWhileEvacuating)
{
    // 记法:(evacuating, reentrySessionBound, reentryIsTicketSession, exitSawClientDisconnect)。进场路由把玩家带回
    // 本节点时,还没派发的票据作废不作废。
    static_assert(rc::CancelsTicketOnReentry(false, true, true, false), "判定必须能在编译期求值(纯函数)");
    // 平时一律作废:那次退出不会再正常收尾,留着票据只会在下一次、会话已换的退出里被误消费。
    for (const bool bound : {false, true})
    {
        for (const bool sameSession : {false, true})
        {
            for (const bool clientGone : {false, true})
            {
                EXPECT_TRUE(rc::CancelsTicketOnReentry(false, bound, sameSession, clientGone))
                    << "bound=" << bound << " same_session=" << sameSession << " client_gone=" << clientGone;
            }
        }
    }
    // 整节点疏散中,只有「票据那同一条有效会话、且被打断的那次退出没记过客户端断线」的进场留着票据:进场若取消了
    // 他的退出,他留在将死的节点上,而疏散只发一轮票,删掉的话既不改派也不踢。
    EXPECT_FALSE(rc::CancelsTicketOnReentry(true, true, true, false));
    EXPECT_TRUE(rc::CancelsTicketOnReentry(true, true, false, false)) << "另一条有效会话:票据里的旧会话已死";
    // 会话 0 / 无效会话必须在这里作废:进场会把实体的会话快照清成无效,而 DecideTicket 把「没有会话」当成
    // 「不是换了会话的证据」照发 —— 留着就会按票据里的旧会话改派。
    EXPECT_TRUE(rc::CancelsTicketOnReentry(true, false, false, false));
    EXPECT_TRUE(rc::CancelsTicketOnReentry(true, false, true, false)) << "会话无效时「相等」没有意义,同样作废";
    EXPECT_EQ(rc::DecideTicket(false, false, false), rc::TicketDecision::kDispatch) << "上一条注释依赖的前提";
    // 那次退出已经记过客户端断线:取消退出会把意图组件上的断线位一起摘掉,下一次退出就不知道这条会话死过了,
    // 留着票据等于给一条判过「已断线」的会话改派 —— 作废。
    EXPECT_TRUE(rc::CancelsTicketOnReentry(true, true, true, true));
    EXPECT_EQ(rc::DecideTicket(true, true, true), rc::TicketDecision::kVoidClientGone)
        << "断线位若还在,票据判定自己就会作废;这里兜的是它被摘掉之后";
}

TEST(RelocateConfirmEarlier, InFlightSceneChangeCountsOnlyInsideSettleWindow)
{
    constexpr uint64_t kSettleWindowMs = 15000;
    EXPECT_FALSE(rc::SceneChangeReplyMayArrive(false, 1000, 0, kSettleWindowMs)) << "没有在途换图";
    EXPECT_TRUE(rc::SceneChangeReplyMayArrive(true, 20000, 5001, kSettleWindowMs)) << "发出 14999ms";
    EXPECT_FALSE(rc::SceneChangeReplyMayArrive(true, 20000, 5000, kSettleWindowMs))
        << "发出 15000ms,恰好等于窗口:scene_manager 不会再处理它";
    EXPECT_TRUE(rc::SceneChangeReplyMayArrive(true, 100, 200, kSettleWindowMs)) << "墙钟回拨按「可能」处理(只会多等)";
    EXPECT_TRUE(rc::SceneChangeReplyMayArrive(true, 20000, 1, 25000)) << "deadline 调大、窗口变宽后仍算在途";
}

TEST(RelocateConfirmReply, OnlyAwaitingReplyTriggersVerification)
{
    for (const auto phase : kAllRelocatePhases)
    {
        EXPECT_EQ(rc::VerifiesOnReply(phase), phase == rc::Phase::kAwaitingReply) << rc::PhaseName(phase);
    }
    EXPECT_EQ(rc::EvidenceForReply(0), rc::Evidence::kReplySucceeded);
    // 非 0 一律只是「被拒」,不区分码:推路由失败(7,没有具名常量,字面量抄自
    // go/scene_manager/internal/constants/errors.go 的 ErrKafkaRoute)、缺 gate 实例号(前置拒绝)、归属查不到、
    // 归属 zone 合服中(合服围栏)。
    for (const uint32_t code : {7u, PlayerLifecycleSystem::kSmErrInvalidGateID,
                                PlayerLifecycleSystem::kSmErrHomeZoneUnavailable,
                                PlayerLifecycleSystem::kSmErrHomeZoneMerging})
    {
        EXPECT_EQ(rc::EvidenceForReply(code), rc::Evidence::kReplyRejected) << "code=" << code;
    }
    EXPECT_EQ(rc::EvidenceForReply(PlayerLifecycleSystem::kSmErrHandoffPending), rc::Evidence::kReplyRejected);
}

TEST(RelocateConfirmClaim, ReplyIsClaimedOnlyByItsOwnCorrelationId)
{
    // 记法:(有条目, 条目记下的号, 条目阶段, 应答回显的号)。
    using rc::ClaimAction;
    using rc::Phase;
    EXPECT_EQ(rc::DecideReplyClaim(false, 41, Phase::kAwaitingReply, 41), ClaimAction::kNotMine) << "没有条目";
    EXPECT_EQ(rc::DecideReplyClaim(false, 0, Phase::kAwaitingReply, 0), ClaimAction::kNotMine);
    EXPECT_EQ(rc::DecideReplyClaim(true, 41, Phase::kAwaitingReply, 41), ClaimAction::kVerify);
    EXPECT_EQ(rc::DecideReplyClaim(true, 41, Phase::kAwaitingReply, 42), ClaimAction::kNotMine)
        << "别的请求(更早的换图 / 交接)的应答";
    EXPECT_EQ(rc::DecideReplyClaim(true, 0, Phase::kMarkWriting, 42), ClaimAction::kNotMine)
        << "自己的请求还没发:任何带号应答都不是它的";
    EXPECT_EQ(rc::DecideReplyClaim(true, 0, Phase::kVerifying, 42), ClaimAction::kNotMine) << "没发出去(kNotSent)的条目同理";
    EXPECT_EQ(rc::DecideReplyClaim(true, 41, Phase::kVerifying, 41), ClaimAction::kIgnore)
        << "是自己的,但已有证据在核实(例如传输失败先到)";
    EXPECT_EQ(rc::DecideReplyClaim(true, 41, Phase::kLanding, 41), ClaimAction::kIgnore) << "路由已先到,由落地路径定案";
    // 旧版 scene_manager 不回显(号为 0):退回按 player_id。
    EXPECT_EQ(rc::DecideReplyClaim(true, 41, Phase::kAwaitingReply, 0), ClaimAction::kVerify);
    EXPECT_EQ(rc::DecideReplyClaim(true, 0, Phase::kMarkWriting, 0), ClaimAction::kIgnore);
}

TEST(RelocateConfirmClaim, TransportFailureIsClaimedOnlyByExactCorrelationId)
{
    using rc::ClaimAction;
    using rc::Phase;
    EXPECT_EQ(rc::DecideTransportFailureClaim(true, 41, Phase::kAwaitingReply, 41), ClaimAction::kVerify);
    EXPECT_EQ(rc::DecideTransportFailureClaim(true, 41, Phase::kAwaitingReply, 0), ClaimAction::kNotMine)
        << "失败号为 0 只能来自绕过统一出口的发送:不退回 player_id";
    EXPECT_EQ(rc::DecideTransportFailureClaim(true, 41, Phase::kAwaitingReply, 42), ClaimAction::kNotMine);
    EXPECT_EQ(rc::DecideTransportFailureClaim(true, 0, Phase::kMarkWriting, 0), ClaimAction::kNotMine)
        << "两边都是 0 也不算对上号";
    EXPECT_EQ(rc::DecideTransportFailureClaim(true, 41, Phase::kLanding, 41), ClaimAction::kIgnore);
    EXPECT_EQ(rc::DecideTransportFailureClaim(true, 41, Phase::kVerifying, 41), ClaimAction::kIgnore);
    EXPECT_EQ(rc::DecideTransportFailureClaim(false, 41, Phase::kAwaitingReply, 41), ClaimAction::kNotMine);
}

// ── 核实读数的裁决 ──

TEST(RelocateConfirmPlacement, AtSourceVerdicts)
{
    static_assert(rc::ClassifyPlacement(RelocatePlacementFacts(rc::Expectation::kAtSource)) == rc::Verdict::kUnchanged,
                  "判定必须能在编译期求值(纯函数)");
    const auto base = RelocatePlacementFacts(rc::Expectation::kAtSource);
    EXPECT_EQ(rc::ClassifyPlacement(base), rc::Verdict::kUnchanged) << "仍指向本节点上的源场景:改派没生效";

    auto absent = base;
    absent.present = false;
    absent.parsed = false;
    EXPECT_EQ(rc::ClassifyPlacement(absent), rc::Verdict::kAbsent);

    auto garbled = base;
    garbled.parsed = false;
    EXPECT_EQ(rc::ClassifyPlacement(garbled), rc::Verdict::kIndeterminate)
        << "location 在但解析失败:不能当成不存在(那一侧可踢)";

    auto otherZone = base;
    otherZone.locationZoneId = kRelocateSelfZone + 1;
    EXPECT_EQ(rc::ClassifyPlacement(otherZone), rc::Verdict::kMovedElsewhere) << "别的 zone 里的同号节点";

    auto otherNode = base;
    otherNode.nodeIsSelf = false;
    EXPECT_EQ(rc::ClassifyPlacement(otherNode), rc::Verdict::kMovedElsewhere);

    auto otherScene = base;
    otherScene.locationSceneId = kRelocateSourceScene + 1;
    EXPECT_EQ(rc::ClassifyPlacement(otherScene), rc::Verdict::kMovedHere) << "落在本节点的另一个场景:等进场路由";
    otherScene.evacuating = true;
    EXPECT_EQ(rc::ClassifyPlacement(otherScene), rc::Verdict::kMovedElsewhere) << "将死节点不等路由";

    auto evacuatingUnchanged = base;
    evacuatingUnchanged.evacuating = true;
    EXPECT_EQ(rc::ClassifyPlacement(evacuatingUnchanged), rc::Verdict::kUnchanged) << "场景相同时与疏散无关";

    auto unknownSource = base;
    unknownSource.sourceSceneId = 0;
    EXPECT_EQ(rc::ClassifyPlacement(unknownSource), rc::Verdict::kIndeterminate) << "源场景未知:不踢";
    unknownSource.locationSceneId = 0;
    EXPECT_EQ(rc::ClassifyPlacement(unknownSource), rc::Verdict::kIndeterminate) << "两个 0 相等也不算「没变」";

    EXPECT_EQ(rc::ClassifyPlacement(rc::PlacementFacts{}), rc::Verdict::kIndeterminate)
        << "漏填的输入落在不踢的一侧(不是可踢的 absent)";
}

TEST(RelocateConfirmPlacement, AtThisNodeVerdicts)
{
    auto facts = RelocatePlacementFacts(rc::Expectation::kAtThisNode);
    for (const uint64_t scene : {kRelocateSourceScene, kRelocateSourceScene + 1, uint64_t{0}})
    {
        facts.locationSceneId = scene;
        EXPECT_EQ(rc::ClassifyPlacement(facts), rc::Verdict::kUnchanged) << "落地核实不比场景 scene=" << scene;
    }
    facts.sourceSceneId = 0;
    EXPECT_EQ(rc::ClassifyPlacement(facts), rc::Verdict::kUnchanged) << "也不需要知道源场景";

    auto evacuating = facts;
    evacuating.evacuating = true;
    EXPECT_EQ(rc::ClassifyPlacement(evacuating), rc::Verdict::kIndeterminate)
        << "疏散中本节点号可能已被新进程复用:指向这个号什么也证明不了";

    auto otherNode = facts;
    otherNode.nodeIsSelf = false;
    EXPECT_EQ(rc::ClassifyPlacement(otherNode), rc::Verdict::kMovedElsewhere);

    auto absent = facts;
    absent.present = false;
    absent.parsed = false;
    EXPECT_EQ(rc::ClassifyPlacement(absent), rc::Verdict::kAbsent);
}

TEST(RelocateConfirmPlacement, UnknownZoneIsIndeterminateNotSelf)
{
    // zone 0 证明不了物理节点身份:当成本 zone 会把别的 zone 的同号节点读成本节点,随后的条件补写等于给那边的活持有者
    // 发放行证。与归属取证(JudgeTravelOutcomeReply)、scene_manager 的 samePhysicalNode 同向。
    for (const auto expectation : {rc::Expectation::kAtSource, rc::Expectation::kAtThisNode})
    {
        auto legacyLocation = RelocatePlacementFacts(expectation);
        legacyLocation.locationZoneId = 0;
        EXPECT_EQ(rc::ClassifyPlacement(legacyLocation), rc::Verdict::kIndeterminate)
            << rc::ExpectationName(expectation);

        auto selfUnknown = RelocatePlacementFacts(expectation);
        selfUnknown.selfZoneId = 0;
        EXPECT_EQ(rc::ClassifyPlacement(selfUnknown), rc::Verdict::kIndeterminate) << rc::ExpectationName(expectation);

        auto bothUnknown = legacyLocation;
        bothUnknown.selfZoneId = 0;
        EXPECT_EQ(rc::ClassifyPlacement(bothUnknown), rc::Verdict::kIndeterminate)
            << "两边都是 0 也不算同一个 zone " << rc::ExpectationName(expectation);

        auto otherNode = legacyLocation;
        otherNode.nodeIsSelf = false;
        EXPECT_EQ(rc::ClassifyPlacement(otherNode), rc::Verdict::kMovedElsewhere)
            << "节点号都不是本节点时用不着 zone " << rc::ExpectationName(expectation);
    }
}

TEST(RelocateConfirmPlacement, CredentialFollowsTheEpochReadAndNeverWritesForALocalHolderOrDevBypass)
{
    using rc::CredentialAction;
    using rc::Verdict;
    // 记法:(读数结论, 读到的 owner_epoch, 已有同代标记, 本节点可能持有, 身份已确认, dev 旁路)。
    // 读到的 7 可以不是派发时缓存的 5:回滚把 epoch 推到了 E+2,补写按读到的值。
    static_assert(DecideRelocateCredential(Verdict::kUnchanged, 7, false, false, true, false) ==
                      CredentialAction::kAttempt,
                  "判定必须能在编译期求值(纯函数)");
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 7, false, false, true, false), CredentialAction::kAttempt);
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 7, true, false, true, false),
              CredentialAction::kKeepExisting)
        << "已有 scene_manager 认得的同代标记:不重复写";
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 7, true, true, false, true),
              CredentialAction::kKeepExisting)
        << "不需要写的时候不看后面的闸";
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 7, false, true, true, false),
              CredentialAction::kSkipLocalHolder)
        << "本节点有实体或载入在途:绝不写(补写会排在那次载入的标记清理之后)";
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 7, false, false, false, false),
              CredentialAction::kSkipIdentity);
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 7, false, false, true, true),
              CredentialAction::kSkipDevBypass);
    // 顺序:本地持有 → 身份 → dev 旁路。
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 7, false, true, false, true),
              CredentialAction::kSkipLocalHolder);
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 7, false, false, false, true),
              CredentialAction::kSkipIdentity);

    // owner_epoch 键丢失 / 写坏读成 0:写 "0:now" 没有意义。
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 0, false, false, true, false), CredentialAction::kNone);
    EXPECT_EQ(DecideRelocateCredential(Verdict::kUnchanged, 0, true, false, true, false), CredentialAction::kNone);
    // 只有「没变」才有凭证可言。
    for (const auto verdict : kAllRelocateVerdicts)
    {
        if (verdict == Verdict::kUnchanged)
        {
            continue;
        }
        EXPECT_EQ(DecideRelocateCredential(verdict, 7, false, false, true, false), CredentialAction::kNone)
            << rc::VerdictName(verdict);
        EXPECT_EQ(DecideRelocateCredential(verdict, 7, true, false, true, false), CredentialAction::kNone)
            << rc::VerdictName(verdict);
    }
    EXPECT_EQ(rc::DecideCredential(rc::CredentialFacts{}), CredentialAction::kNone) << "漏填的输入落在不写的一侧";
}

TEST(RelocateConfirmPlacement, MarkMustBeDigitsColonDigitsLikeTheSceneManagerParser)
{
    static_assert(rc::MarkCarriesEpoch("7:1700000000000", 7), "判定必须能在编译期求值(纯函数)");
    static_assert(rc::ParseOwnerEpoch("007") == 7, "解析必须能在编译期求值(纯函数)");

    // scene_manager 认得的标记:整串「数字:数字」(ownerepoch.ParseHandoff / kLuaInheritClear 的 '^(%d+):%d+$')。
    EXPECT_TRUE(rc::MarkCarriesEpoch("7:1700000000000", 7));
    EXPECT_TRUE(rc::MarkCarriesEpoch("7:0", 7));
    EXPECT_TRUE(rc::MarkCarriesEpoch("007:1", 7)) << "前导零:按数值比代际";
    EXPECT_TRUE(rc::MarkCarriesEpoch("7:9223372036854775807", 7)) << "后缀恰为 int64 上限(Go 侧用 ParseInt 解析它)";
    EXPECT_TRUE(rc::MarkCarriesEpoch("18446744073709551615:1", (std::numeric_limits<uint64_t>::max)()));

    // 后缀写坏:换手门会按「未落盘」回 18,所以按没有标记处理,让条件补写覆盖它。
    for (const char *mark : {"7:x", "7:", "7:1:2", "7:-1", "7: 1", "7:99999999999999999999", "7:9223372036854775808"})
    {
        EXPECT_FALSE(rc::MarkCarriesEpoch(mark, 7)) << "mark=[" << mark << "]";
    }
    // 前缀不对 / 不是数字 / 溢出(21 位)/ 没有冒号。
    for (const char *mark : {"5:1", "", "7", "7a:1", " 7:1", "123456789012345678901:1"})
    {
        EXPECT_FALSE(rc::MarkCarriesEpoch(mark, 7)) << "mark=[" << mark << "]";
    }
    EXPECT_FALSE(rc::MarkCarriesEpoch(":1", 0)) << "前缀为空不等于 0";

    EXPECT_EQ(rc::ParseOwnerEpoch("7"), 7u);
    EXPECT_EQ(rc::ParseOwnerEpoch("007"), 7u);
    EXPECT_EQ(rc::ParseOwnerEpoch("18446744073709551615"), (std::numeric_limits<uint64_t>::max)());
    // 不是完整的十进制串一律按 0(凭证判定里 0 = 不写),不能把 "7a" 截成 7。
    for (const char *text : {"", "7a", "-1", " 7", "18446744073709551616"})
    {
        EXPECT_EQ(rc::ParseOwnerEpoch(text), 0u) << "text=[" << text << "]";
    }
}

TEST(RelocateConfirmScript, ReadPlacementIsAShebangScriptThatOnlyReads)
{
    const std::string lua = rc::kLuaReadPlacement;
    EXPECT_EQ(lua.rfind("#!lua\n", 0), 0u)
        << "必须以 #!lua 开头:只读副本 / MISCONF / OOM 下整体被拒,不会从切主后的旧主读到滞后的「没变」而误踢";
    EXPECT_EQ(lua.find("no-writes"), std::string::npos) << "不得声明 no-writes:那会让只读副本上照样读成功";
    EXPECT_NE(lua.find("redis.call('MGET', KEYS[1], KEYS[2], KEYS[3])"), std::string::npos)
        << "一次读回 owner_epoch、location、handoff 三个键";
    EXPECT_EQ(lua.find("'GET'"), std::string::npos) << "只用 MGET:GET 遇到类型不对的键会抛 WRONGTYPE";
    EXPECT_EQ(lua.find("DEL"), std::string::npos) << "只读:删标记会毁掉被踢玩家重登要出示的凭证";
    EXPECT_EQ(lua.find("SET"), std::string::npos) << "只读:补写走另一条按 owner_epoch 的条件写";
}

// ── settle 规则、踢线时机、无法核实时的处置 ──

TEST(RelocateConfirmKick, EarlyKickOnlyForProvablyOwnRejectionOrNotSent)
{
    // 记法:(证据, 还有请求结局未定)。
    EXPECT_TRUE(rc::MayKickBeforeSettle(rc::Evidence::kReplyRejected, false)) << "自己的拒绝:亚秒级踢";
    EXPECT_FALSE(rc::MayKickBeforeSettle(rc::Evidence::kReplyRejected, true))
        << "更早的请求可能凭改派刚写的标记被放行:等到 settle 再读一次";
    EXPECT_FALSE(rc::MayKickBeforeSettle(rc::Evidence::kReplySucceeded, false))
        << "成功却读到没变:可能是放回了同一个场景、路由还在路上";
    EXPECT_TRUE(rc::MayKickBeforeSettle(rc::Evidence::kNotSent, false));
    EXPECT_FALSE(rc::MayKickBeforeSettle(rc::Evidence::kNotSent, true));
    EXPECT_TRUE(rc::MayKickBeforeSettle(rc::Evidence::kLandingAbandoned, false));
    EXPECT_TRUE(rc::MayKickBeforeSettle(rc::Evidence::kLandingTimeout, false));
    EXPECT_FALSE(rc::MayKickBeforeSettle(rc::Evidence::kLandingTimeout, true));
    EXPECT_FALSE(rc::MayKickBeforeSettle(rc::Evidence::kTransportFailed, false)) << "结果未知";
    EXPECT_FALSE(rc::MayKickBeforeSettle(rc::Evidence::kTransportFailed, true));
    for (const auto evidence : kAllRelocateEvidence)
    {
        EXPECT_FALSE(rc::MayKickBeforeSettle(evidence, true)) << rc::EvidenceName(evidence);
    }
}

TEST(RelocateConfirmKick, DecideAfterVerifyTable)
{
    for (const bool kickAllowedNow : {false, true})
    {
        EXPECT_EQ(rc::DecideAfterVerify(rc::Verdict::kMovedElsewhere, kickAllowedNow), rc::VerifyAction::kResolveMoved)
            << "allowed=" << kickAllowedNow;
        EXPECT_EQ(rc::DecideAfterVerify(rc::Verdict::kMovedHere, kickAllowedNow), rc::VerifyAction::kAwaitLanding)
            << "allowed=" << kickAllowedNow;
        EXPECT_EQ(rc::DecideAfterVerify(rc::Verdict::kIndeterminate, kickAllowedNow),
                  rc::VerifyAction::kResolveIndeterminate)
            << "allowed=" << kickAllowedNow;
        EXPECT_EQ(rc::DecideAfterVerify(rc::Verdict::kCount, kickAllowedNow), rc::VerifyAction::kResolveIndeterminate)
            << "越界的结论按判不清处理,不踢 allowed=" << kickAllowedNow;
    }
    for (const auto verdict : {rc::Verdict::kUnchanged, rc::Verdict::kAbsent})
    {
        EXPECT_EQ(rc::DecideAfterVerify(verdict, true), rc::VerifyAction::kKick) << rc::VerdictName(verdict);
        EXPECT_EQ(rc::DecideAfterVerify(verdict, false), rc::VerifyAction::kWaitSettle) << rc::VerdictName(verdict);
    }
}

TEST(RelocateConfirmKick, UnknownOutcomeAndEarlierRequestsAreReadOnlyAfterSettle)
{
    // 记法:(证据, 还有请求结局未定)。settle 之前 scene_manager 可能正处在「铸造 → 推路由 / 失败回滚」窗口里,
    // 此刻读到「在别处」会被结清为不踢,随后的回滚把 location 写回源场景。
    EXPECT_TRUE(rc::MustReadAfterSettle(rc::Evidence::kTransportFailed, false)) << "结果未知:窗口属于本次改派";
    EXPECT_TRUE(rc::MustReadAfterSettle(rc::Evidence::kTransportFailed, true));
    for (const auto evidence : kAllRelocateEvidence)
    {
        if (evidence == rc::Evidence::kTransportFailed)
        {
            continue;
        }
        EXPECT_FALSE(rc::MustReadAfterSettle(evidence, false)) << rc::EvidenceName(evidence) << ":没有请求结局未定时立即读";
        EXPECT_TRUE(rc::MustReadAfterSettle(evidence, true)) << rc::EvidenceName(evidence) << ":窗口属于那条结局未定的请求";
    }
}

TEST(RelocateConfirmKick, UnverifiableKickOnlyWhenSceneManagerCannotHaveGrantedOurs)
{
    // 记法:(证据, 还有请求结局未定, 拒绝发生在任何落点写之前)。本节点读不到 ≠ scene_manager 写不了。
    // ① 没发出去 / 落地两类:本节点替他发的请求里没有一条可能写过落点。
    EXPECT_TRUE(rc::ShouldKickUnverified(rc::Evidence::kNotSent, false, false));
    EXPECT_TRUE(rc::ShouldKickUnverified(rc::Evidence::kLandingAbandoned, false, false));
    EXPECT_TRUE(rc::ShouldKickUnverified(rc::Evidence::kLandingTimeout, false, false));
    // ② 被拒:只有拒绝码证明拒绝发生在落点写之前才踢。
    EXPECT_TRUE(rc::ShouldKickUnverified(rc::Evidence::kReplyRejected, false, true));
    EXPECT_FALSE(rc::ShouldKickUnverified(rc::Evidence::kReplyRejected, false, false))
        << "推路由失败那一类:回滚没成时等于已放行到别处,盲踢会打到合法会话";
    // ③ 成功 / 等不到完成通知 / 结果未知:可能已放行并把同一条会话改绑到别处;第三个入参对它们不起作用。
    EXPECT_FALSE(rc::ShouldKickUnverified(rc::Evidence::kReplySucceeded, false, true));
    EXPECT_FALSE(rc::ShouldKickUnverified(rc::Evidence::kNoReply, false, true));
    EXPECT_FALSE(rc::ShouldKickUnverified(rc::Evidence::kTransportFailed, false, true));
    // ④ 还有请求结局未定时一律不盲踢(含没发出去的)。
    for (const auto evidence : kAllRelocateEvidence)
    {
        EXPECT_FALSE(rc::ShouldKickUnverified(evidence, true, true)) << rc::EvidenceName(evidence);
        EXPECT_FALSE(rc::ShouldKickUnverified(evidence, true, false)) << rc::EvidenceName(evidence);
    }

    // ⑤ 第三个入参的来源:PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite 的白名单 —— 这九个码在 scene_manager 里的
    //    每一个出处都早于本请求的落点写。数值手抄自 go/scene_manager/internal/constants/errors.go(两边没有共享的生成物),
    //    钉在这里:那边改号而 player_lifecycle.h 没跟,盲踢就会打到已放行的会话。18 引用既有常量,不再写字面量。
    struct PinnedSmCode
    {
        uint32_t value;
        uint32_t expected;
        std::string_view goName;
    };
    const PinnedSmCode pinnedCodes[] = {
        {PlayerLifecycleSystem::kSmErrNoAvailableNode, 1, "ErrNoAvailableNode"},
        {PlayerLifecycleSystem::kSmErrInvalidNodeID, 4, "ErrInvalidNodeID"},
        {PlayerLifecycleSystem::kSmErrInvalidGateID, 5, "ErrInvalidGateID"},
        {PlayerLifecycleSystem::kSmErrRedis, 8, "ErrRedis"},
        {PlayerLifecycleSystem::kSmErrSceneReentryBarrier, 17, "ErrSceneReentryBarrier"},
        {PlayerLifecycleSystem::kSmErrHomeZoneUnavailable, 20, "ErrHomeZoneUnavailable"},
        {PlayerLifecycleSystem::kSmErrHomeZoneMerging, 21, "ErrHomeZoneMerging"},
        // 25 的唯一出处是落点会话身份判定 decidePlacementIdentity,早于一切预占与写(scene-enter-chain-residuals.md §5.2 ⑤)。
        {PlayerLifecycleSystem::kSmErrPlacementSessionSuperseded, 25, "ErrPlacementSessionSuperseded"},
    };
    for (const auto &pinned : pinnedCodes)
    {
        EXPECT_EQ(pinned.value, pinned.expected) << pinned.goName;
        EXPECT_TRUE(PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite(pinned.value)) << pinned.goName;
    }
    EXPECT_TRUE(PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite(PlayerLifecycleSystem::kSmErrHandoffPending))
        << "18 的两个出处都没写落点:换手门只读;落点 Lua 发现标记已不是预检那一份时什么都不改";
    // 不在白名单里的码(没有具名常量,字面量同样抄自 errors.go):0 不是拒绝;3 ErrUpdateLocation(也可能是落点写入报错而
    // 结果未知)、7 ErrKafkaRoute(也可能出在落点已写之后,回滚成没成都回 7)、19 ErrOwnerEpochConflict(也可能是不凭标记的
    // 铸造被重发、首发其实已写)各有一个「落点可能已写」的出处;2 ErrSceneLookupFailed、6 ErrEncodeEvent 没逐个核过出处;
    // 99 是未知码。拿不准的一律不算 —— 漏判只会少踢。
    for (const uint32_t code : {0u, 3u, 7u, 19u, 2u, 6u, 99u})
    {
        EXPECT_FALSE(PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite(code)) << "code=" << code;
    }
    static_assert(PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite(PlayerLifecycleSystem::kSmErrHomeZoneMerging) &&
                      !PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite(0),
                  "白名单判定必须能在编译期求值(纯函数)");

    // ⑥ 组合反例:被 7 / 19 拒掉之后又读不到 Redis,不踢(结清为 gave_up_unverified)。
    EXPECT_FALSE(rc::ShouldKickUnverified(rc::Evidence::kReplyRejected, false,
                                          PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite(7)))
        << "推路由失败:回滚没成(所凭标记已被目标节点删掉)时玩家其实已在别处";
    EXPECT_FALSE(rc::ShouldKickUnverified(rc::Evidence::kReplyRejected, false,
                                          PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite(19)));
    EXPECT_TRUE(rc::ShouldKickUnverified(
        rc::Evidence::kReplyRejected, false,
        PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite(PlayerLifecycleSystem::kSmErrHomeZoneMerging)))
        << "合服围栏的拒绝发生在任何落点写之前";
}

TEST(RelocateConfirmKick, UnsettledRequestDefersTheReadAndForbidsEarlyOrBlindKicks)
{
    // ① 记法:(更早请求可能还会回应答, 本次改派的关联号, 完成通知已认领)。
    EXPECT_FALSE(rc::RequestOutcomeUnsettled(false, 0, false)) << "没发出去,也没有更早请求";
    EXPECT_TRUE(rc::RequestOutcomeUnsettled(true, 0, false));
    EXPECT_TRUE(rc::RequestOutcomeUnsettled(false, 41, false)) << "已发出、完成通知未到:自己的请求可能还在排队";
    EXPECT_FALSE(rc::RequestOutcomeUnsettled(false, 41, true));
    EXPECT_TRUE(rc::RequestOutcomeUnsettled(true, 41, true)) << "更早请求的窗口不因本次完成而关上";

    // ② 条目上的同一个量:发出后为真,完成通知被认领后为假;MarkCompletion 不动阶段与时间点。
    rc::Table table;
    rc::Entry *entry = AddRelocateEntry(table, kRelocatePlayerA, kT0);
    ASSERT_NE(entry, nullptr);
    EXPECT_FALSE(rc::RequestOutcomeUnsettled(*entry));
    rc::Table::MarkSent(*entry, kT0 + 1s, 41, RelocateBudgets());
    EXPECT_TRUE(rc::RequestOutcomeUnsettled(*entry));
    const rc::Entry beforeCompletion = *entry;
    rc::Table::MarkCompletion(*entry, 21);
    EXPECT_FALSE(rc::RequestOutcomeUnsettled(*entry));
    EXPECT_TRUE(entry->completionSeen);
    EXPECT_EQ(entry->replyErrorCode, 21u);
    EXPECT_EQ(entry->phase, beforeCompletion.phase);
    EXPECT_TRUE(entry->sentAt == beforeCompletion.sentAt);
    EXPECT_TRUE(entry->deadline == beforeCompletion.deadline);
    EXPECT_TRUE(entry->settleAt == beforeCompletion.settleAt);
    EXPECT_TRUE(entry->nextVerifyAt == beforeCompletion.nextVerifyAt);
    // 只认第一次:一次调用只完成一次,第二次按构造不可达;真到了也不改写已记下的拒绝码(盲踢白名单与日志都看它)。
    rc::Table::MarkCompletion(*entry, 0);
    EXPECT_TRUE(entry->completionSeen);
    EXPECT_EQ(entry->replyErrorCode, 21u);

    rc::Entry *withEarlier = AddRelocateEntry(table, kRelocatePlayerC, kT0, /*earlierEnterSceneMayReply=*/true);
    ASSERT_NE(withEarlier, nullptr);
    EXPECT_TRUE(rc::RequestOutcomeUnsettled(*withEarlier));
    rc::Table::MarkSent(*withEarlier, kT0 + 1s, 43, RelocateBudgets());
    rc::Table::MarkCompletion(*withEarlier, 0);
    EXPECT_TRUE(rc::RequestOutcomeUnsettled(*withEarlier)) << "登记时就在途的更早请求一直算到 settle";

    // ③ 完成通知未到时条目已被别的路由提前转进落地,随后载入被放弃:落地核实也得等 settle、不许提前踢、读不到不盲踢。
    rc::Entry *landed = AddRelocateEntry(table, kRelocatePlayerB, kT0);
    ASSERT_NE(landed, nullptr);
    rc::Table::MarkSent(*landed, kT0 + 1s, 42, RelocateBudgets());
    rc::Table::BeginLanding(*landed, kT0 + 2s, /*reentered=*/true, /*loadPending=*/false);
    rc::Table::BeginVerify(*landed, kT0 + 3s, rc::Evidence::kLandingAbandoned, rc::Expectation::kAtThisNode);
    EXPECT_TRUE(rc::MustReadAfterSettle(rc::Evidence::kLandingAbandoned, rc::RequestOutcomeUnsettled(*landed)));
    EXPECT_FALSE(rc::MayKickBeforeSettle(rc::Evidence::kLandingAbandoned, rc::RequestOutcomeUnsettled(*landed)));
    EXPECT_FALSE(rc::ShouldKickUnverified(rc::Evidence::kLandingAbandoned, rc::RequestOutcomeUnsettled(*landed), false));
    rc::Table::WaitForSettle(*landed);
    EXPECT_TRUE(landed->nextVerifyAt == kT0 + 16s) << "settleAt 从发送时刻起算";

    rc::Table::MarkCompletion(*landed, 0);
    EXPECT_FALSE(rc::MustReadAfterSettle(rc::Evidence::kLandingAbandoned, rc::RequestOutcomeUnsettled(*landed)));
    EXPECT_TRUE(rc::MayKickBeforeSettle(rc::Evidence::kLandingAbandoned, rc::RequestOutcomeUnsettled(*landed)));
    EXPECT_TRUE(rc::ShouldKickUnverified(rc::Evidence::kLandingAbandoned, rc::RequestOutcomeUnsettled(*landed), false));
}

TEST(RelocateConfirmLanding, DecideLandingTable)
{
    // 记法:(实体有效, 实体会话 == 票据会话, 进场路由到过, 载入在途, 已过截止)。
    using rc::LandingAction;
    for (const bool reentered : {false, true})
    {
        for (const bool loadPending : {false, true})
        {
            for (const bool pastDeadline : {false, true})
            {
                EXPECT_EQ(rc::DecideLanding(true, true, reentered, loadPending, pastDeadline),
                          LandingAction::kResolveLanded)
                    << "实体已建出时其余入参不起作用";
                EXPECT_EQ(rc::DecideLanding(true, false, reentered, loadPending, pastDeadline),
                          LandingAction::kResolveSuperseded)
                    << "会话换了 = 玩家已重登,不踢";
            }
        }
    }
    EXPECT_EQ(rc::DecideLanding(false, false, true, false, false), LandingAction::kVerifyAbandoned)
        << "路由到过、载入却已不在途:被放弃了";
    EXPECT_EQ(rc::DecideLanding(false, false, false, false, true), LandingAction::kVerifyTimeout);
    EXPECT_EQ(rc::DecideLanding(false, false, false, true, true), LandingAction::kVerifyTimeout);
    EXPECT_EQ(rc::DecideLanding(false, false, true, true, true), LandingAction::kVerifyTimeout) << "载入卡过了截止";
    EXPECT_EQ(rc::DecideLanding(false, false, true, true, false), LandingAction::kWait);
    EXPECT_EQ(rc::DecideLanding(false, false, false, false, false), LandingAction::kWait)
        << "路由还没到时不能把「没有载入在途」当成载入被放弃";
}

// ── Table:阶段迁移与到期归类 ──

TEST(RelocateConfirmTable, AddStartsInMarkWritingWithBudgets)
{
    rc::Table table;
    std::optional<rc::Entry> superseded = rc::Entry{}; // 预置非空:没有替换时 Add 必须把它清掉
    const uint64_t seqA = table.Add(MakeRelocateRegistration(kRelocatePlayerA), kT0, RelocateBudgets(), superseded);
    ASSERT_GE(seqA, 1u) << "seq 从 1 起,0 留给「没被跟踪」";
    EXPECT_FALSE(superseded.has_value());

    const rc::Entry *entry = table.FindPlayer(kRelocatePlayerA);
    ASSERT_NE(entry, nullptr);
    EXPECT_EQ(entry->seq, seqA);
    EXPECT_EQ(entry->phase, rc::Phase::kMarkWriting);
    EXPECT_TRUE(entry->createdAt == kT0);
    EXPECT_TRUE(entry->deadline == kT0 + 30s) << "写标记回调的预算";
    EXPECT_TRUE(entry->settleAt == kT0 + 15s);
    EXPECT_EQ(entry->correlationId, 0u) << "0 = 改派还没发出";
    EXPECT_EQ(entry->markWrite, rc::MarkWrite::kNotAttempted);
    EXPECT_FALSE(entry->completionSeen);
    EXPECT_EQ(entry->replyErrorCode, 0u);
    EXPECT_FALSE(entry->reentered);
    EXPECT_FALSE(entry->verifyInFlight);
    EXPECT_FALSE(entry->retryPending);
    EXPECT_EQ(entry->verifyGen, 0u);
    // 登记的事实原样带进条目。
    EXPECT_EQ(entry->playerId, kRelocatePlayerA);
    EXPECT_EQ(entry->sessionId, kSessionAtExit);
    EXPECT_EQ(entry->gateNodeId, 1u);
    EXPECT_EQ(entry->gateInstanceId, "gate-uuid");
    EXPECT_EQ(entry->ownerEpoch, 5u);
    EXPECT_EQ(entry->sourceSceneId, kRelocateSourceScene);
    EXPECT_TRUE(entry->markValue.empty());
    EXPECT_FALSE(entry->earlierEnterSceneMayReply);

    const uint64_t seqB =
        table.Add(MakeRelocateRegistration(kRelocatePlayerB), kT0 + 1s, rc::BudgetsFor(20000ms), superseded);
    EXPECT_GT(seqB, seqA) << "seq 表内单调";
    const rc::Entry *wider = table.Find(kRelocatePlayerB, seqB);
    ASSERT_NE(wider, nullptr);
    EXPECT_TRUE(wider->settleAt == kT0 + 1s + 25s) << "settle 窗口跟着调用方给的预算走(deadline 20s + 5s)";
    EXPECT_TRUE(wider->deadline == kT0 + 1s + 30s) << "写标记的预算与 gRPC deadline 无关";

    EXPECT_EQ(table.size(), 2u);
    EXPECT_EQ(table.Find(kRelocatePlayerA, seqB), nullptr) << "seq 对不上就找不到:过期回调不会作用到别的条目";
    EXPECT_EQ(table.Find(kRelocatePlayerA, 0), nullptr);
    EXPECT_EQ(table.FindPlayer(kRelocatePlayerC), nullptr);
    const auto counts = table.CountByPhase();
    EXPECT_EQ(counts[static_cast<std::size_t>(rc::Phase::kMarkWriting)], 2u);
    EXPECT_EQ(counts[static_cast<std::size_t>(rc::Phase::kVerifying)], 0u);
    const auto snapshot = table.Snapshot();
    ASSERT_EQ(snapshot.size(), 2u);
    EXPECT_EQ(snapshot[0].playerId, kRelocatePlayerA) << "副本按登记顺序(seq)排好";
    EXPECT_EQ(snapshot[1].playerId, kRelocatePlayerB);
}

TEST(RelocateConfirmTable, FullTableRefusesInsteadOfEvicting)
{
    rc::Table table(2);
    std::optional<rc::Entry> superseded;
    ASSERT_NE(table.Add(MakeRelocateRegistration(kRelocatePlayerA), kT0, RelocateBudgets(), superseded), 0u);
    ASSERT_NE(table.Add(MakeRelocateRegistration(kRelocatePlayerB), kT0, RelocateBudgets(), superseded), 0u);

    EXPECT_EQ(table.Add(MakeRelocateRegistration(kRelocatePlayerC), kT0 + 1s, RelocateBudgets(), superseded), 0u)
        << "表满返回 0:调用方计数后按旧行为发出、不跟踪";
    EXPECT_FALSE(superseded.has_value());
    EXPECT_EQ(table.size(), 2u);
    EXPECT_NE(table.FindPlayer(kRelocatePlayerA), nullptr) << "容量满时不淘汰在途改派(淘汰 = 放弃对他的确认)";
    EXPECT_NE(table.FindPlayer(kRelocatePlayerB), nullptr);
    EXPECT_EQ(table.FindPlayer(kRelocatePlayerC), nullptr);

    EXPECT_NE(table.Add(MakeRelocateRegistration(kRelocatePlayerA), kT0 + 2s, RelocateBudgets(), superseded), 0u)
        << "同玩家的再次登记是替换,不占新位置";
    EXPECT_TRUE(superseded.has_value());
    EXPECT_EQ(table.size(), 2u);
}

TEST(RelocateConfirmTable, SecondAddForSamePlayerSupersedesTheOldEntry)
{
    rc::Table table;
    std::optional<rc::Entry> superseded;
    const uint64_t oldSeq = table.Add(MakeRelocateRegistration(kRelocatePlayerA), kT0, RelocateBudgets(), superseded);
    ASSERT_NE(oldSeq, 0u);
    ASSERT_FALSE(superseded.has_value());

    const uint64_t newSeq =
        table.Add(MakeRelocateRegistration(kRelocatePlayerA), kT0 + 5s, RelocateBudgets(), superseded);
    ASSERT_NE(newSeq, 0u);
    ASSERT_TRUE(superseded.has_value()) << "旧条目要带出来:调用方按 superseded 结清并记日志";
    EXPECT_EQ(superseded->seq, oldSeq);
    EXPECT_EQ(superseded->playerId, kRelocatePlayerA);
    EXPECT_NE(newSeq, oldSeq);
    EXPECT_EQ(table.Find(kRelocatePlayerA, oldSeq), nullptr) << "旧 seq 的回调从此对不上号";
    const rc::Entry *current = table.Find(kRelocatePlayerA, newSeq);
    ASSERT_NE(current, nullptr);
    EXPECT_TRUE(current->createdAt == kT0 + 5s);
    EXPECT_EQ(table.size(), 1u);

    const auto taken = table.Take(kRelocatePlayerA);
    ASSERT_TRUE(taken.has_value());
    EXPECT_EQ(taken->seq, newSeq);
    EXPECT_EQ(table.size(), 0u);
    EXPECT_FALSE(table.Take(kRelocatePlayerA).has_value()) << "取走之后拿不到第二次:结清 / 踢线不会重入";
}

TEST(RelocateConfirmTable, MarkWritingPastDeadlineVerifiesAsNotSent)
{
    rc::Table table;
    rc::Entry *entry = AddRelocateEntry(table, kRelocatePlayerA, kT0);
    ASSERT_NE(entry, nullptr);
    const uint64_t seq = entry->seq;

    EXPECT_TRUE(RelocateDueIsEmpty(table.CollectDue(kT0 + 30s - 1ms, /*reconnected=*/false)));
    EXPECT_EQ(entry->phase, rc::Phase::kMarkWriting);

    const auto due = table.CollectDue(kT0 + 30s, /*reconnected=*/false);
    ASSERT_EQ(due.toVerify.size(), 1u) << "写标记的回调一直不来(半开连接):按没发出去转核实";
    EXPECT_EQ(due.toVerify[0], RelocateKey(kRelocatePlayerA, seq));
    EXPECT_EQ(due.markWriteTimedOut, 1u);
    EXPECT_EQ(due.replyTimedOut, 0u);
    EXPECT_TRUE(due.verifyExpired.empty());
    EXPECT_TRUE(due.landingToCheck.empty());
    EXPECT_EQ(entry->phase, rc::Phase::kVerifying);
    EXPECT_EQ(entry->evidence, rc::Evidence::kNotSent);
    EXPECT_EQ(entry->expectation, rc::Expectation::kAtSource);
    EXPECT_TRUE(entry->deadline == kT0 + 30s + 10s);
    EXPECT_FALSE(entry->verifyInFlight);
}

TEST(RelocateConfirmTable, SendRestartsReplyAndSettleClocks)
{
    rc::Table table;
    rc::Entry *entry = AddRelocateEntry(table, kRelocatePlayerA, kT0);
    ASSERT_NE(entry, nullptr);
    const uint64_t seq = entry->seq;

    rc::Table::MarkSent(*entry, kT0 + 3s, 41, RelocateBudgets());
    EXPECT_EQ(entry->phase, rc::Phase::kAwaitingReply);
    EXPECT_EQ(entry->correlationId, 41u);
    EXPECT_TRUE(entry->sentAt == kT0 + 3s);
    EXPECT_TRUE(entry->deadline == kT0 + 33s);
    EXPECT_TRUE(entry->settleAt == kT0 + 18s) << "settle 从发送时刻重新起算";
    EXPECT_FALSE(entry->completionSeen);

    EXPECT_TRUE(RelocateDueIsEmpty(table.CollectDue(kT0 + 30s, /*reconnected=*/false))) << "写标记的截止不再生效";

    const auto due = table.CollectDue(kT0 + 33s, /*reconnected=*/false);
    ASSERT_EQ(due.toVerify.size(), 1u) << "完成通知永远不来:按 no_reply 转核实";
    EXPECT_EQ(due.toVerify[0], RelocateKey(kRelocatePlayerA, seq));
    EXPECT_EQ(due.replyTimedOut, 1u);
    EXPECT_EQ(due.markWriteTimedOut, 0u);
    EXPECT_EQ(entry->phase, rc::Phase::kVerifying);
    EXPECT_EQ(entry->evidence, rc::Evidence::kNoReply);
    EXPECT_EQ(entry->expectation, rc::Expectation::kAtSource);
    EXPECT_TRUE(entry->deadline == kT0 + 33s + 10s);

    // deadline 调大时两只钟一起变宽。
    rc::Entry *wider = AddRelocateEntry(table, kRelocatePlayerB, kT0);
    ASSERT_NE(wider, nullptr);
    rc::Table::MarkSent(*wider, kT0 + 3s, 42, rc::BudgetsFor(40000ms));
    EXPECT_TRUE(wider->settleAt == kT0 + 3s + 45s);
    EXPECT_TRUE(wider->deadline == kT0 + 3s + 50s);
}

TEST(RelocateConfirmTable, VerifyPacingSettleAndDeadlineAreMutuallyExclusive)
{
    rc::Table table;
    rc::Entry *entry = AddRelocateEntry(table, kRelocatePlayerA, kT0); // settleAt = kT0 + 15s
    ASSERT_NE(entry, nullptr);
    const uint64_t seq = entry->seq;
    const auto t = kT0 + 1s;

    // 1. 核实在途:不重发,重连也不重发。
    rc::Table::BeginVerify(*entry, t, rc::Evidence::kNotSent, rc::Expectation::kAtSource);
    EXPECT_TRUE(entry->deadline == t + 10s);
    EXPECT_EQ(rc::Table::MarkVerifySent(*entry), 1u);
    EXPECT_TRUE(entry->verifyInFlight);
    EXPECT_TRUE(RelocateDueIsEmpty(table.CollectDue(t + 2s, /*reconnected=*/true)));

    // 2. 这次没读到:1s 后重试;重连可以提前。
    rc::Table::MarkVerifyDeferred(*entry, t + 2s);
    EXPECT_TRUE(entry->nextVerifyAt == t + 3s);
    EXPECT_TRUE(entry->retryPending);
    EXPECT_FALSE(entry->verifyInFlight);
    EXPECT_TRUE(RelocateDueIsEmpty(table.CollectDue(t + 2500ms, /*reconnected=*/false)));
    {
        const auto due = table.CollectDue(t + 2500ms, /*reconnected=*/true);
        ASSERT_EQ(due.toVerify.size(), 1u) << "重连:上次没读到的条目立刻重发";
        EXPECT_EQ(due.toVerify[0], RelocateKey(kRelocatePlayerA, seq));
        EXPECT_TRUE(due.verifyExpired.empty());
    }

    // 3. 读到没变但还不许踢:等到 settleAt 再读;重连不绕过 settle。
    EXPECT_EQ(rc::Table::MarkVerifySent(*entry), 2u);
    EXPECT_FALSE(entry->retryPending);
    const auto deadlineBeforeWait = entry->deadline;
    rc::Table::WaitForSettle(*entry);
    EXPECT_TRUE(entry->nextVerifyAt == entry->settleAt);
    EXPECT_TRUE(entry->deadline == (std::max)(deadlineBeforeWait, entry->settleAt + rc::kVerifyBudget));
    EXPECT_TRUE(entry->deadline == kT0 + 25s) << "本轮核实的截止顺延到 settle 之后一整轮";
    EXPECT_FALSE(entry->verifyInFlight);
    EXPECT_FALSE(entry->retryPending);
    EXPECT_TRUE(RelocateDueIsEmpty(table.CollectDue(entry->settleAt - 1ms, /*reconnected=*/true)))
        << "重连只对「上次没读到」的条目生效,不绕过 settle 等待";
    {
        const auto due = table.CollectDue(entry->settleAt, /*reconnected=*/false);
        ASSERT_EQ(due.toVerify.size(), 1u) << "到 settleAt 再读一次";
        EXPECT_TRUE(due.verifyExpired.empty());
    }

    // 4. 到期只进 verifyExpired,不同时进 toVerify;在途也照样到期(黑洞连接不会回空应答)。
    {
        const auto due = table.CollectDue(entry->deadline, /*reconnected=*/true);
        ASSERT_EQ(due.verifyExpired.size(), 1u);
        EXPECT_EQ(due.verifyExpired[0], RelocateKey(kRelocatePlayerA, seq));
        EXPECT_TRUE(due.toVerify.empty());
        EXPECT_TRUE(due.landingToCheck.empty());
    }
    rc::Table::MarkVerifySent(*entry);
    {
        const auto due = table.CollectDue(entry->deadline, /*reconnected=*/false);
        EXPECT_EQ(due.verifyExpired.size(), 1u);
        EXPECT_TRUE(due.toVerify.empty());
    }
    EXPECT_EQ(table.size(), 1u) << "CollectDue 不结清条目:由调用方踢线 / 放弃时取走";
}

TEST(RelocateConfirmTable, TransportFailureWaitsForSettleBeforeTheFirstRead)
{
    rc::Table table;
    rc::Entry *entry = AddRelocateEntry(table, kRelocatePlayerA, kT0);
    ASSERT_NE(entry, nullptr);
    const uint64_t seq = entry->seq;

    rc::Table::MarkSent(*entry, kT0 + 1s, 41, RelocateBudgets());
    // 传输失败被认领:粘合层**不**调 MarkCompletion(结果未知不是结局),只换证据转核实。
    rc::Table::BeginVerify(*entry, kT0 + 2s, rc::Evidence::kTransportFailed, rc::Expectation::kAtSource);
    EXPECT_FALSE(entry->completionSeen);
    EXPECT_TRUE(rc::RequestOutcomeUnsettled(*entry)) << "没拿到带应答体的完成通知:本次请求的结局一直算未定";
    ASSERT_TRUE(rc::MustReadAfterSettle(entry->evidence, rc::RequestOutcomeUnsettled(*entry))) << "结果未知:等 settle";
    // 证据本身就够:即使把「结局未定」这一项拿掉,传输失败照样等 settle、不提前踢、读不到不盲踢。
    EXPECT_TRUE(rc::MustReadAfterSettle(rc::Evidence::kTransportFailed, /*requestUnsettled=*/false));
    EXPECT_FALSE(rc::MayKickBeforeSettle(rc::Evidence::kTransportFailed, /*requestUnsettled=*/false));
    EXPECT_FALSE(rc::ShouldKickUnverified(rc::Evidence::kTransportFailed, /*requestUnsettled=*/false, true));
    rc::Table::WaitForSettle(*entry); // 还没读过就可以等
    EXPECT_TRUE(entry->nextVerifyAt == kT0 + 16s) << "settleAt = 发送时刻 + 15s";
    EXPECT_TRUE(entry->deadline == kT0 + 26s);
    EXPECT_FALSE(entry->retryPending);
    EXPECT_FALSE(entry->verifyInFlight);
    EXPECT_EQ(entry->verifyGen, 0u) << "一次都没读过";

    EXPECT_TRUE(RelocateDueIsEmpty(table.CollectDue(kT0 + 16s - 1ms, /*reconnected=*/true))) << "重连也不提前读";
    const auto due = table.CollectDue(kT0 + 16s, /*reconnected=*/false);
    ASSERT_EQ(due.toVerify.size(), 1u);
    EXPECT_EQ(due.toVerify[0], RelocateKey(kRelocatePlayerA, seq));
    EXPECT_TRUE(due.verifyExpired.empty());
}

TEST(RelocateConfirmTable, LandingEntriesAreReportedAndVerifyGenerationAdvances)
{
    rc::Table table;
    rc::Entry *entry = AddRelocateEntry(table, kRelocatePlayerA, kT0);
    ASSERT_NE(entry, nullptr);
    const uint64_t seq = entry->seq;
    rc::Table::MarkSent(*entry, kT0 + 1s, 41, RelocateBudgets());
    const auto t = kT0 + 2s;

    // ① 受理判据:核实在途时进场路由回到本节点,条目转去落地。
    rc::Table::BeginVerify(*entry, t, rc::Evidence::kReplySucceeded, rc::Expectation::kAtSource);
    const uint32_t gen = rc::Table::MarkVerifySent(*entry);
    EXPECT_TRUE(rc::AcceptsVerifyReply(*entry, gen));
    EXPECT_FALSE(rc::AcceptsVerifyReply(*entry, gen + 1));
    rc::Table::BeginLanding(*entry, t, /*reentered=*/true, /*loadPending=*/false);
    EXPECT_EQ(entry->phase, rc::Phase::kLanding);
    EXPECT_FALSE(entry->verifyInFlight);
    EXPECT_FALSE(entry->retryPending);
    EXPECT_TRUE(entry->reentered);
    EXPECT_TRUE(entry->deadline == t + 60s);
    EXPECT_EQ(entry->verifyGen, gen) << "转去落地不动代际";
    EXPECT_FALSE(rc::AcceptsVerifyReply(*entry, gen)) << "在途的旧核实应答从此作废";

    {
        const auto due = table.CollectDue(t + 1s, /*reconnected=*/false);
        ASSERT_EQ(due.landingToCheck.size(), 1u) << "落地条目每拍都报:实体建出来没有由调用方看";
        EXPECT_EQ(due.landingToCheck[0], RelocateKey(kRelocatePlayerA, seq));
        EXPECT_TRUE(due.toVerify.empty());
        EXPECT_TRUE(due.verifyExpired.empty());
    }
    {
        const auto due = table.CollectDue(t + 60s + 1h, /*reconnected=*/true);
        EXPECT_EQ(due.landingToCheck.size(), 1u) << "过了截止也只是报出来:超时由 DecideLanding 判";
        EXPECT_TRUE(due.toVerify.empty());
        EXPECT_TRUE(due.verifyExpired.empty());
    }

    // ② 幂等:同一条路由可能到两次(gate 侧补发)。
    rc::Table::BeginLanding(*entry, t + 5s, /*reentered=*/true, /*loadPending=*/true);
    EXPECT_TRUE(entry->deadline == t + 60s) << "同一次载入的重复路由:截止不动";
    rc::Table::BeginLanding(*entry, t + 5s, /*reentered=*/true, /*loadPending=*/false);
    EXPECT_TRUE(entry->deadline == t + 65s)
        << "上一次载入已结束 = 真正的第二次进场:截止重新起算,否则新载入会撞上第一次的截止被判超时";
    EXPECT_TRUE(entry->reentered);

    // ① 续:落地被放弃后新一轮核实,代际前进,旧应答可以被识别。
    rc::Table::BeginVerify(*entry, t + 70s, rc::Evidence::kLandingAbandoned, rc::Expectation::kAtThisNode);
    const uint32_t gen2 = rc::Table::MarkVerifySent(*entry);
    EXPECT_EQ(gen2, gen + 1);
    EXPECT_FALSE(rc::AcceptsVerifyReply(*entry, gen));
    EXPECT_TRUE(rc::AcceptsVerifyReply(*entry, gen2));

    // ③ 核实读到「落在本节点的另一个场景」:路由还没到(reentered 为假)。
    rc::Entry *waiting = AddRelocateEntry(table, kRelocatePlayerB, kT0);
    ASSERT_NE(waiting, nullptr);
    rc::Table::BeginVerify(*waiting, t, rc::Evidence::kReplySucceeded, rc::Expectation::kAtSource);
    rc::Table::BeginLanding(*waiting, t, /*reentered=*/false, /*loadPending=*/false);
    EXPECT_EQ(waiting->phase, rc::Phase::kLanding);
    EXPECT_FALSE(waiting->reentered);
    EXPECT_TRUE(waiting->deadline == t + 60s);
    rc::Table::BeginLanding(*waiting, t + 5s, /*reentered=*/true, /*loadPending=*/true);
    EXPECT_TRUE(waiting->reentered) << "路由到了";
    EXPECT_TRUE(waiting->deadline == t + 65s) << "从路由到达起重新给足载入时间";
    rc::Table::BeginLanding(*waiting, t + 9s, /*reentered=*/false, /*loadPending=*/false);
    EXPECT_TRUE(waiting->reentered) << "reentered 只增不减";
    EXPECT_TRUE(waiting->deadline == t + 65s) << "已在落地、入参 reentered 为假:什么都不改";
}

TEST(RelocateConfirmTable, StaleVerifyReplyIsRejectedAfterTheEntryMovedOn)
{
    rc::Table table;
    rc::Entry *entry = AddRelocateEntry(table, kRelocatePlayerA, kT0);
    ASSERT_NE(entry, nullptr);
    rc::Table::MarkSent(*entry, kT0 + 1s, 41, RelocateBudgets());

    // 时序一:核实在途时同会话的进场路由到达,载入在途。旧应答读到的「仍指向源场景」不得踢掉正在本节点载入的合法会话。
    rc::Table::BeginVerify(*entry, kT0 + 2s, rc::Evidence::kReplyRejected, rc::Expectation::kAtSource);
    const uint32_t gen = rc::Table::MarkVerifySent(*entry);
    ASSERT_TRUE(rc::AcceptsVerifyReply(*entry, gen));
    rc::Table::BeginLanding(*entry, kT0 + 3s, /*reentered=*/true, /*loadPending=*/true);
    EXPECT_FALSE(rc::AcceptsVerifyReply(*entry, gen));

    // 时序二:落地被放弃、新一轮核实已开始,但新命令还没发(等 settle / Redis 推迟)。旧应答的代际仍对得上,
    // 却是按上一轮的期望(kAtSource)读的,不能当成这一轮(kAtThisNode)的读数。
    rc::Table::BeginVerify(*entry, kT0 + 4s, rc::Evidence::kLandingAbandoned, rc::Expectation::kAtThisNode);
    EXPECT_EQ(entry->verifyGen, gen) << "只有 MarkVerifySent 自增代际";
    EXPECT_FALSE(entry->verifyInFlight);
    EXPECT_FALSE(rc::AcceptsVerifyReply(*entry, gen));
    rc::Table::WaitForSettle(*entry);
    EXPECT_FALSE(rc::AcceptsVerifyReply(*entry, gen));
    rc::Table::MarkVerifyDeferred(*entry, kT0 + 5s);
    EXPECT_FALSE(rc::AcceptsVerifyReply(*entry, gen)) << "过期的空应答同样不得再去推迟这一轮";
    const uint32_t gen2 = rc::Table::MarkVerifySent(*entry);
    EXPECT_EQ(gen2, gen + 1);
    EXPECT_FALSE(rc::AcceptsVerifyReply(*entry, gen));
    EXPECT_TRUE(rc::AcceptsVerifyReply(*entry, gen2));

    // 不在核实阶段的条目一律不受理,哪怕在途位与代际都对得上。
    for (const auto phase : kAllRelocatePhases)
    {
        rc::Entry probe;
        probe.phase = phase;
        probe.verifyInFlight = true;
        probe.verifyGen = 3;
        EXPECT_EQ(rc::AcceptsVerifyReply(probe, 3), phase == rc::Phase::kVerifying) << rc::PhaseName(phase);
    }
}

// ── 预算与名表 ──

TEST(RelocateConfirmBudgets, Derivations)
{
    static_assert(rc::BudgetsFor(10000ms).settleWindow == 15s, "预算派生必须能在编译期求值(纯函数)");
    const rc::Budgets atCurrentConfig{15s, 30s};
    EXPECT_TRUE(rc::BudgetsFor(10000ms) == atCurrentConfig) << "deadline 10s + Kafka 写 5s = 15s;兜底 30s";
    EXPECT_TRUE(rc::BudgetsFor(0ms) == atCurrentConfig) << "deadline 被配小时两个下限不跟着缩";
    const rc::Budgets atTwentySeconds{25s, 30s};
    EXPECT_TRUE(rc::BudgetsFor(20000ms) == atTwentySeconds);
    const rc::Budgets atFortySeconds{45s, 50s};
    EXPECT_TRUE(rc::BudgetsFor(40000ms) == atFortySeconds) << "settle 越过兜底下限后,等完成通知的预算跟着抬";
    EXPECT_TRUE(rc::Budgets{} == atCurrentConfig) << "默认预算 = 两个下限";

    for (const auto deadline : {0ms, 10000ms, 20000ms, 40000ms})
    {
        const auto budgets = rc::BudgetsFor(deadline);
        EXPECT_TRUE(budgets.replyWait > budgets.settleWindow)
            << "no_reply 出现时一定已过 settleAt deadline_ms=" << deadline.count();
        EXPECT_TRUE(budgets.settleWindow > deadline) << "deadline_ms=" << deadline.count();
    }

    EXPECT_TRUE(rc::kLandingBudget > 31500ms + 5000ms) << "AsyncLoad 6 次退避约 31.5s + 载入前清继承标记 5s";
    EXPECT_TRUE(rc::kVerifyRetryInterval < rc::kVerifyBudget);
    EXPECT_TRUE(rc::kReplyWaitBudget == 30s);
    EXPECT_TRUE(rc::kMarkWriteBudget == 30s);
    EXPECT_TRUE(rc::kSmSettleWindow == 15s);
    EXPECT_TRUE(rc::kSettleAfterCallDeadline == 5s)
        << "= scene_manager_service.yaml 的 KafkaWriteTimeoutSeconds:调大那边必须同步这里";
    EXPECT_TRUE(rc::kSmSettleWindow < rc::kReplyWaitBudget);
}

TEST(RelocateConfirmBudgets, SettleWindowCoversTheSceneManagerCallDeadline)
{
    // 上游比下游宽:deadline 到期之后 scene_manager 的 handler 最多再跑一个 Kafka 写超时,settle 窗口必须盖住它。
    // 宿主里没有 Apply 过配置,取到的是内置默认 10000ms。
    const auto deadline = grpc_call_deadline::Get(eNodeType::SceneManagerNodeService);
    const auto budgets = rc::BudgetsFor(deadline);
    EXPECT_TRUE(budgets.settleWindow >= deadline + rc::kSettleAfterCallDeadline) << "deadline_ms=" << deadline.count();
    EXPECT_TRUE(budgets.replyWait > budgets.settleWindow);
    // 上面两条对任何 deadline 都由 BudgetsFor 的定义保证;下面两条不经 BudgetsFor,钉的是「当前配置」本身:
    // 宿主取到的默认 deadline 就是 10000ms(base_deploy_config.yaml 的 GrpcClient.CallDeadlineMs.SceneManagerNodeService
    // 与它一致;给宿主 Apply 过别的配置时改这里),并且 settle 的下限常量对它够用 —— 不够用说明有人调大了 deadline 而
    // 下限没跟上(BudgetsFor 仍会派生出更宽的窗口,但这个常量的注释与文档就过期了)。
    EXPECT_EQ(deadline.count(), 10000);
    EXPECT_TRUE(rc::kSmSettleWindow >= deadline + rc::kSettleAfterCallDeadline);
}

TEST(RelocateConfirmNames, CoverEveryValue)
{
    ExpectRelocateNamesCoverEveryValue<rc::Phase>("Phase", rc::kPhaseCount, rc::PhaseName);
    ExpectRelocateNamesCoverEveryValue<rc::Evidence>("Evidence", rc::kEvidenceCount, rc::EvidenceName);
    ExpectRelocateNamesCoverEveryValue<rc::Expectation>("Expectation", rc::kExpectationCount, rc::ExpectationName);
    ExpectRelocateNamesCoverEveryValue<rc::Verdict>("Verdict", rc::kVerdictCount, rc::VerdictName);
    ExpectRelocateNamesCoverEveryValue<rc::VerifyAction>("VerifyAction", rc::kVerifyActionCount, rc::VerifyActionName);
    ExpectRelocateNamesCoverEveryValue<rc::LandingAction>("LandingAction", rc::kLandingActionCount,
                                                         rc::LandingActionName);
    ExpectRelocateNamesCoverEveryValue<rc::TicketDecision>("TicketDecision", rc::kTicketDecisionCount,
                                                          rc::TicketDecisionName);
    ExpectRelocateNamesCoverEveryValue<rc::Outcome>("Outcome", rc::kOutcomeCount, rc::OutcomeName);
    ExpectRelocateNamesCoverEveryValue<rc::GatePushResult>("GatePushResult", rc::kGatePushResultCount,
                                                          rc::GatePushResultName);
    ExpectRelocateNamesCoverEveryValue<rc::MarkWrite>("MarkWrite", rc::kMarkWriteCount, rc::MarkWriteName);
    ExpectRelocateNamesCoverEveryValue<rc::ClaimAction>("ClaimAction", rc::kClaimActionCount, rc::ClaimActionName);
    ExpectRelocateNamesCoverEveryValue<rc::CredentialAction>("CredentialAction", rc::kCredentialActionCount,
                                                            rc::CredentialActionName);

    // [RelocateConfirm] 汇总行的 key 与逐条日志的取值:runbook 按原文 grep,改名要同步 runbook。
    const char *const outcomeNames[] = {
        "granted",       "moved_elsewhere", "landed_here",     "superseded",
        "indeterminate", "kick_verified",   "kick_unverified", "gave_up_unverified",
    };
    ASSERT_EQ(std::size(outcomeNames), rc::kOutcomeCount);
    for (std::size_t i = 0; i < rc::kOutcomeCount; ++i)
    {
        EXPECT_STREQ(rc::OutcomeName(static_cast<rc::Outcome>(i)), outcomeNames[i]) << "outcome=" << i;
    }
    const char *const credentialNames[] = {
        "none", "keep_existing", "skip_local_holder", "skip_identity", "skip_dev_bypass", "attempted",
    };
    ASSERT_EQ(std::size(credentialNames), rc::kCredentialActionCount);
    for (std::size_t i = 0; i < rc::kCredentialActionCount; ++i)
    {
        EXPECT_STREQ(rc::CredentialActionName(static_cast<rc::CredentialAction>(i)), credentialNames[i])
            << "credential=" << i;
    }
    const char *const pushNames[] = {"sent", "gate_gone", "gate_replaced"};
    ASSERT_EQ(std::size(pushNames), rc::kGatePushResultCount);
    for (std::size_t i = 0; i < rc::kGatePushResultCount; ++i)
    {
        EXPECT_STREQ(rc::GatePushResultName(static_cast<rc::GatePushResult>(i)), pushNames[i]) << "push=" << i;
    }
    EXPECT_STREQ(rc::EvidenceName(rc::Evidence::kTransportFailed), "transport_failed");
    EXPECT_STREQ(rc::TicketDecisionName(rc::TicketDecision::kVoidClientGone), "void_client_gone");
    EXPECT_STREQ(rc::kVerdictUnread, "unread") << "没有读数的结清 / 踢线在日志里的 verdict= 取值";
}

// ── 退出意图组件上给改派用的两个事实(player_exit_intent.h)──

TEST(RelocateConfirmIntent, ClientDisconnectIsStickyOnMerge)
{
    PlayerExitIntentComp intent;
    intent.cause = ExitCause::kSceneDrain;
    EXPECT_FALSE(intent.clientDisconnected) << "默认 = 会话还活着";
    EXPECT_EQ(intent.sceneIdAtExit, 0u) << "默认 = 源场景未知(核实一律判不清、不踢)";

    player_exit::MergeExitCause(intent, ExitCause::kClientDisconnect, /*devBypassSuppressesTransfer=*/false);
    EXPECT_TRUE(intent.clientDisconnected) << "排空发起的退出之后客户端才断线:会话已死,改派票据要作废";
    EXPECT_FALSE(intent.releaseMarkSuppressed) << "客户端断线不参与压制规则";
    EXPECT_EQ(intent.cause, ExitCause::kSceneDrain) << "原因保持第一次的";

    player_exit::MergeExitCause(intent, ExitCause::kNodeShutdown, false);
    EXPECT_TRUE(intent.clientDisconnected) << "置位后不再清";

    for (const auto incoming : kAllExitCauses)
    {
        if (incoming == ExitCause::kClientDisconnect)
        {
            continue;
        }
        PlayerExitIntentComp fresh;
        fresh.cause = ExitCause::kSceneDrain;
        player_exit::MergeExitCause(fresh, incoming, false);
        EXPECT_FALSE(fresh.clientDisconnected) << "incoming=" << player_exit::ExitCauseName(incoming);
    }
}

TEST(RelocateConfirmIntent, HandoffEnterSceneMayBeInFlightCoversBothExitWinsBranches)
{
    // A1′(M11)与改派的「更早请求可能还会被放行」共用这一个判据,两处「退出优先」分支都要覆盖到。
    PlayerExitIntentComp issued;
    issued.travelHandoffMarkIssued = true;
    const PlayerExitIntentComp plain{};
    EXPECT_TRUE(player_exit::HandoffEnterSceneMayBeInFlight(true, nullptr))
        << "FinishExitAfterPersist 自己的「退出优先」分支:组件已摘,只剩局部变量";
    EXPECT_TRUE(player_exit::HandoffEnterSceneMayBeInFlight(false, &issued))
        << "HandlePlayerAsyncSaved 的「退出优先」分支把它记在意图组件上";
    EXPECT_TRUE(player_exit::HandoffEnterSceneMayBeInFlight(true, &plain));
    EXPECT_FALSE(player_exit::HandoffEnterSceneMayBeInFlight(false, &plain));
    EXPECT_FALSE(player_exit::HandoffEnterSceneMayBeInFlight(false, nullptr));
}

// ── ECS 级接缝:直接驱动排空(BeginSceneDrain)、退出收尾、进场 Reconcile、定时扫描与两个 Dispatch ──
// 宿主里没有 scene_manager(GetSceneManagerEntity 返回 null:改派没发出去)、没有 gate(按会话推送 = push_gate_gone)、
// 没有 zone Redis(改派标记不写;核实读发不出去 = verify_deferred)。所以这里走得到的是:登记 → 没发出去 → 核实推迟 →
// 读不到时的处置,以及落回本节点的各条路径;kAwaitingReply 之后的路径只有上面的纯函数 / Table 用例(见文件头)。
// 本节不构造 ScopedFakeSceneManagerNode:需要明确走"没有可派发节点"的分支,不能意外发起真实 RPC。
// 时间:登记与 Reconcile 走真实的单调时钟,所以 SweepRelocateConfirms 的时刻以「现在」为基准加偏移注入;断言只依赖
// 偏移量之间的关系(各预算都是秒级以上),不依赖用例本身跑了多久。
namespace
{
template <typename Enum>
constexpr std::size_t RelocateIndex(Enum value)
{
    return static_cast<std::size_t>(value);
}

relocate_confirm_stats::Snapshot RelocateStats()
{
    return relocate_confirm_stats::Read();
}

// 逐项比对两份计数快照,失败时指出是哪一项。用法:expected = 基线,只改预期会动的项 —— 其余项不动也一并断言了。
void ExpectRelocateStats(const relocate_confirm_stats::Snapshot &actual,
                         const relocate_confirm_stats::Snapshot &expected)
{
    for (std::size_t i = 0; i < rc::kTicketDecisionCount; ++i)
    {
        EXPECT_EQ(actual.ticketDecisions[i], expected.ticketDecisions[i]) << rc::kTicketDecisionNames[i];
    }
    EXPECT_EQ(actual.untrackedOverflow, expected.untrackedOverflow) << "untracked_overflow";
    EXPECT_EQ(actual.cancelledOnReentry, expected.cancelledOnReentry) << "cancelled_on_reentry";
    EXPECT_EQ(actual.ticketsDroppedDeposed, expected.ticketsDroppedDeposed) << "ticket_dropped_deposed";
    EXPECT_EQ(actual.replyVerified, expected.replyVerified) << "reply_verified";
    EXPECT_EQ(actual.replyIgnored, expected.replyIgnored) << "reply_ignored";
    EXPECT_EQ(actual.transportFailed, expected.transportFailed) << "transport_failed";
    EXPECT_EQ(actual.notSent, expected.notSent) << "not_sent";
    EXPECT_EQ(actual.markWriteTimeout, expected.markWriteTimeout) << "mark_write_timeout";
    EXPECT_EQ(actual.replyTimeout, expected.replyTimeout) << "reply_timeout";
    EXPECT_EQ(actual.verifyDeferred, expected.verifyDeferred) << "verify_deferred";
    EXPECT_EQ(actual.settleWaits, expected.settleWaits) << "settle_waits";
    for (std::size_t i = 0; i < rc::kOutcomeCount; ++i)
    {
        EXPECT_EQ(actual.outcomes[i], expected.outcomes[i]) << rc::kOutcomeNames[i];
    }
    for (std::size_t i = 0; i < rc::kGatePushResultCount; ++i)
    {
        EXPECT_EQ(actual.push[i], expected.push[i]) << "push_" << rc::kGatePushResultNames[i];
    }
    for (std::size_t i = 0; i < rc::kCredentialActionCount; ++i)
    {
        EXPECT_EQ(actual.credentialActions[i], expected.credentialActions[i])
            << "credential_" << rc::kCredentialActionNames[i];
    }
    EXPECT_EQ(actual.credentialWritten, expected.credentialWritten) << "credential_written";
    EXPECT_EQ(actual.credentialEpochMoved, expected.credentialEpochMoved) << "credential_epoch_moved";
    EXPECT_EQ(actual.credentialFailed, expected.credentialFailed) << "credential_failed";
    EXPECT_EQ(actual.credentialSuperseded, expected.credentialSuperseded) << "credential_superseded";
    // 兜底:以后给 Snapshot 加了字段而上面没跟着逐项列出时,这一条会红(上面各项都绿、只有它红 = 漏列了新字段)。
    EXPECT_TRUE(actual == expected) << "relocate_confirm_stats::Snapshot 有 ExpectRelocateStats 没逐项列出的字段";
}

// 作用域内把节点身份探针置为「确认有效」;离开作用域(含 ASSERT 失败提前返回)一律复位成「未注入 = 不确认」。
// 探针是 thread_local 的,而 gtest 的用例都跑在同一条线程上:不复位会改变后面所有走 A1′ 的用例的判定
// (--gtest_shuffle 下尤其难查)。
struct ScopedConfirmedNodeIdentity
{
    ScopedConfirmedNodeIdentity() { PlayerLifecycleSystem::SetNodeIdentityProbe([] { return true; }); }
    ~ScopedConfirmedNodeIdentity() { PlayerLifecycleSystem::SetNodeIdentityProbe({}); }
    ScopedConfirmedNodeIdentity(const ScopedConfirmedNodeIdentity &) = delete;
    ScopedConfirmedNodeIdentity &operator=(const ScopedConfirmedNodeIdentity &) = delete;
};

// 待确认表、改派票据表、待入场表都是 thread_local,不随 tlsEcs.Clear() 清。每条 ECS 用例开头和结尾各调一次:本节的
// 用例都用 kExitPlayerId,前一条留下的条目会让下一条的登记把它计成 superseded;前一条在发票之后失败(ASSERT 提前
// 返回)会把票据留给下一条。计数基线一律在开头这一次**之后**取(第 ① 步会动 cancelled_on_reentry 与 superseded)。
void DrainRelocateConfirms()
{
    // 本节会往待入场表里放 kExitPlayerId 的条目(模拟载入在途),别留给下一条。
    PlayerLifecycleSystem::GetPendingEnterMap().erase(kExitPlayerId);
    // ① 会话 0 的进场:删掉残留票据,并把残留条目按 superseded 结清。票据表没有公开访问器,这是唯一能清它的入口。
    //    前提:进程不在整节点疏散中。本文件没有用例调 BeginEmergencyRelocateAll(那个标志没有复位入口);
    //    会话 0 的进场在疏散中也照样作废票据,但疏散态本身清不掉 —— 以后要加这类用例,得先给它配一个单测复位口。
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, 0);
    // ② 别的玩家的残留条目(本节不产生,防御):第一拍把各阶段都推到「核实读不到」,第二拍让那一轮核实到期。
    const auto farFuture = rc::Clock::now() + std::chrono::hours(1);
    PlayerLifecycleSystem::SweepRelocateConfirms(farFuture, /*reconnected=*/false);
    PlayerLifecycleSystem::SweepRelocateConfirms(farFuture + rc::kVerifyBudget + 1s, /*reconnected=*/false);
    // ③
    ASSERT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
}

// 把玩家放进一个场景:场景挂 SceneInfoComp,玩家挂唯一的成员事实 SceneEntityComp。
// BeginSceneDrain 查询玩家绑定,HandleExitGameNode 从场景抄 sceneIdAtExit。返回场景实体。
// 必须在 MakeOnlinePlayer / MakeExitingPlayer 之后调:它们里面的 tlsEcs.Clear() 会清 sceneRegistry。
SceneEntity PlaceInScene(entt::entity player, uint64_t sceneId)
{
    const auto scene = tlsEcs.sceneRegistry.create();
    tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_id(sceneId);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(player).sceneEntity = scene;
    return scene;
}

// 登记一条「没发出去」的改派:在线玩家(会话 kSessionAtExit、缓存 owner_epoch 5)站在 kRelocateSourceScene 里,盘上已是
// 当前内存(退出走快路径、同步收敛),对他所在的场景做单场景排空。返回后实体已销毁、会话映射已摘,待确认表里有他的
// 一条 kVerifying / kNotSent 条目:没有 scene_manager → 没发出去 → 立即核实;没有 Redis → 核实推迟。
// 计数只动三项:dispatch、not_sent、verify_deferred 各 +1。
// 以上是「表里没有他的旧条目」时的结果。表里已有他的一条旧条目时再调:旧条目被顶掉,多计一项 superseded;
// 旧条目名下若还有结局未定的请求,新条目带上「更早请求可能被放行」,首次核实改为等 settle
// (settle_waits +1 而不是 verify_deferred +1)。
void RegisterNotSentRelocate()
{
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
    const auto scene = PlaceInScene(player, kRelocateSourceScene);
    ASSERT_TRUE(MarkPersisted(player)); // 最后做:快照要与退出时再 marshal 出来的那一份逐字节相同
    ASSERT_EQ(PlayerLifecycleSystem::BeginSceneDrain(scene), 1u);
    ASSERT_FALSE(tlsEcs.actorRegistry.valid(player)) << "退出同步收敛,实体已销毁";
    ASSERT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 1u);
}

// 一个「退出存盘在途」的玩家,站在源场景里:第一次退出原因 = 单场景排空,会话 kSessionAtExit 仍映射到本玩家,
// 缓存 owner_epoch 5。对返回的场景做 BeginSceneDrain 会给他发一张改派票据(已在退出中:只合并原因,实体与票据都留着),
// 之后用 HandlePlayerAsyncSaved(MarshalAsSaved) 让退出收敛。
struct RelocateExitingPlayer
{
    entt::entity player{entt::null};
    SceneEntity scene{entt::null};
};

RelocateExitingPlayer MakeRelocateExitingPlayer(bool travelHandoffMarkIssued = false)
{
    RelocateExitingPlayer made;
    made.player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    auto &intent = tlsEcs.actorRegistry.get<PlayerExitIntentComp>(made.player);
    intent.cause = ExitCause::kSceneDrain;
    intent.travelHandoffMarkIssued = travelHandoffMarkIssued;
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(made.player).epoch = 5;
    made.scene = PlaceInScene(made.player, kRelocateSourceScene);
    return made;
}
} // namespace

// 不建立场景成员反向索引。旧排空实现只查那份索引,此夹具会返回 0;修复后只查玩家绑定。
TEST(RelocateConfirmEcs, BeginSceneDrainOnlyCountsResidentsOfThatScene)
{
    ScopedPersistedPlayerCleanup cleanup;
    tlsEcs.Clear();
    SessionMap().clear();
    constexpr Guid targetId = kExitPlayerId + 71;
    constexpr Guid otherId = kExitPlayerId + 72;
    struct TicketCleanup
    {
        Guid targetId;
        Guid otherId;
        ~TicketCleanup()
        {
            PlayerLifecycleSystem::ReconcileRelocateOnReentry(targetId, 0);
            PlayerLifecycleSystem::ReconcileRelocateOnReentry(otherId, 0);
        }
    } ticketCleanup{targetId, otherId};
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(targetId, 0);
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(otherId, 0);

    // 已退出中且未耗尽重存次数:真实排空只登记票据、合并原因,不触碰 Redis 或网络。
    const auto makeResident = [](Guid id, SessionId session)
    {
        const auto player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<Player>(player);
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, id);
        auto& snapshot = tlsEcs.actorRegistry.emplace<PlayerSessionSnapshotComp>(player);
        snapshot.set_player_id(id);
        snapshot.set_gate_session_id(session);
        tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
        auto& intent = tlsEcs.actorRegistry.emplace<PlayerExitIntentComp>(player);
        intent.sessionAtExit = session;
        intent.cause = ExitCause::kSceneDrain;
        tlsEcs.playerList.emplace(id, player);
        SessionMap().insert_or_assign(session, id);
        return player;
    };
    const auto target = makeResident(targetId, kSessionAtExit);
    const auto other = makeResident(otherId, kNewerSession);
    const auto targetScene = PlaceInScene(target, kRelocateSourceScene);
    const auto otherScene = PlaceInScene(other, kRelocateSourceScene + 1);
    const auto cancelledBefore = RelocateStats().cancelledOnReentry;

    EXPECT_EQ(PlayerLifecycleSystem::BeginSceneDrain(targetScene), 1u);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(target));
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(other));
    EXPECT_EQ(tlsEcs.actorRegistry.get<SceneEntityComp>(target).sceneEntity, targetScene);
    EXPECT_EQ(tlsEcs.actorRegistry.get<SceneEntityComp>(other).sceneEntity, otherScene);
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(otherId, 0);
    EXPECT_EQ(RelocateStats().cancelledOnReentry, cancelledBefore) << "另一场景不能获得排空票据";
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(targetId, 0);
    EXPECT_EQ(RelocateStats().cancelledOnReentry, cancelledBefore + 1) << "只撤销目标场景玩家的票据";
}

// 必需清理走真实销毁入口,宿主没有注册 任何 AOI 离场 sink。
// 修复前只触发通知事件,玩家虽被销毁,格子和观察者仍保留陈旧句柄。
TEST(PlayerLifecycleEcs, DestroyPlayerWithoutDetachLeavesNoAoiResidue)
{
    ScopedPersistedPlayerCleanup cleanup;
    tlsEcs.Clear();
    const auto target = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<Player>(target);
    tlsEcs.actorRegistry.emplace<PlayerGuidComp>(target, kExitPlayerId);
    tlsEcs.actorRegistry.emplace<Transform>(target);
    tlsEcs.playerList.emplace(kExitPlayerId, target);
    const auto scene = PlaceInScene(target, kRelocateSourceScene);
    const auto watcher = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<Player>(watcher);
    tlsEcs.actorRegistry.emplace<PlayerGuidComp>(watcher, kExitPlayerId + 1);
    tlsEcs.actorRegistry.emplace<Transform>(watcher);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(watcher, SceneEntityComp{scene});

    AoiSystem::Update(0); // 首次真实入格创建格子表和双方兴趣表,不预挂空表。
    const auto* beforeGrids = tlsEcs.sceneRegistry.try_get<SceneGridListComp>(scene);
    ASSERT_NE(beforeGrids, nullptr);
    ASSERT_TRUE(std::any_of(beforeGrids->begin(), beforeGrids->end(), [target](const auto& entry)
        { return entry.second.entities.contains(target); }));
    const auto* beforeAoi = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(beforeAoi, nullptr);
    ASSERT_TRUE(beforeAoi->Contains(target));

    PlayerLifecycleSystem::DestroyPlayer(kExitPlayerId);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(target));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), entt::entity{entt::null});
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(watcher));
    const auto* afterAoi = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(afterAoi, nullptr);
    EXPECT_FALSE(afterAoi->Contains(target));
    const auto* afterGrids = tlsEcs.sceneRegistry.try_get<SceneGridListComp>(scene);
    ASSERT_NE(afterGrids, nullptr);
    for (const auto& [gridId, grid] : *afterGrids)
    {
        EXPECT_FALSE(grid.entities.contains(target));
    }
}




TEST(RelocateConfirmEcs, DrainWithoutSceneManagerIsVerifiedThenKicked)
{
    // 今天的缺陷:排空 / 疏散的改派发完即忘 —— 没发出去或被拒时实体已销毁、location 仍指向源场景,客户端的会话却还
    // 挂着。现在这次改派被登记下来,核实读不到(宿主没有 Redis)、而本节点替他发的请求确定没写过落点,到期后踢线。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    const auto before = RelocateStats();
    const auto releaseBefore = exit_release_stats::Read();

    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate());

    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u) << "会话映射随退出收尾摘掉;按会话踢线靠的是条目里抄下的会话号";
    const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(entry.has_value());
    EXPECT_NE(entry->seq, 0u);
    EXPECT_EQ(entry->phase, rc::Phase::kVerifying);
    EXPECT_EQ(entry->evidence, rc::Evidence::kNotSent);
    EXPECT_EQ(entry->expectation, rc::Expectation::kAtSource);
    EXPECT_EQ(entry->sessionId, kSessionAtExit);
    EXPECT_EQ(entry->ownerEpoch, 5u);
    EXPECT_EQ(entry->sourceSceneId, kRelocateSourceScene) << "源场景在 LeaveCurrentScene 之前抄进意图组件";
    EXPECT_TRUE(entry->markValue.empty()) << "宿主没有 Redis:改派标记没尝试写";
    EXPECT_EQ(entry->markWrite, rc::MarkWrite::kNotAttempted);
    EXPECT_FALSE(entry->earlierEnterSceneMayReply) << "逐字段显式登记:没有更早的请求在途(缺省值是保守的 true)";
    EXPECT_EQ(entry->correlationId, 0u) << "没发出去";
    EXPECT_FALSE(entry->completionSeen);
    EXPECT_FALSE(entry->verifyInFlight);
    EXPECT_TRUE(entry->retryPending) << "核实读发不出去,等 1s 节拍重试";

    auto expected = before;
    ++expected.ticketDecisions[RelocateIndex(rc::TicketDecision::kDispatch)];
    ++expected.notSent;
    ++expected.verifyDeferred;
    ExpectRelocateStats(RelocateStats(), expected);
    const auto releaseAfter = exit_release_stats::Read();
    const auto skipRelocate = static_cast<std::size_t>(erm::ExitReleaseDecision::kSkipRelocate);
    EXPECT_EQ(releaseAfter.decisions[skipRelocate], releaseBefore.decisions[skipRelocate] + 1)
        << "票据被消费:这次的标记归改派负责,A1′ 不写";

    // 一轮核实到期仍读不到:没发出去 + 没有请求结局未定 → 踢(读不到,所以不补写凭证);宿主没有 gate,推不出去。
    PlayerLifecycleSystem::SweepRelocateConfirms(rc::Clock::now() + rc::kVerifyBudget + 1s, /*reconnected=*/false);

    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
    ++expected.outcomes[RelocateIndex(rc::Outcome::kKickUnverified)];
    ++expected.push[RelocateIndex(rc::GatePushResult::kGateGone)];
    ++expected.credentialActions[RelocateIndex(rc::CredentialAction::kNone)];
    ExpectRelocateStats(RelocateStats(), expected);
    EXPECT_EQ(PlayerLifecycleSystem::ExitReleaseMarksInFlight(), 0u) << "没有读数就没有凭证补写";

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, OnlyUncorrelatedRepliesAreSwallowedWhileVerifyingANotSentRelocate)
{
    // 认领按关联号:条目没发出去(号为 0)时,任何带号的应答都不是它的 —— 那是更早请求(换图 / 交接)的应答,
    // 不能吞。只有旧版 scene_manager 不回显号(0)时才退回按 player_id 认领,而且认领了也只是忽略,不改条目。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate());
    const auto before = RelocateStats();
    const auto travelBefore = travel_handoff_stats::Read();

    // ① 带非 0 号:不被认领(落回原来的 "entity is gone" 日志),不计任何 relocate_confirm_stats。7 = ErrKafkaRoute。
    DeliverEnterSceneReply(MakeReply(kExitPlayerId, 0, 77));
    DeliverEnterSceneReply(MakeReply(kExitPlayerId, 7, 77));
    ExpectRelocateStats(RelocateStats(), before);

    // ② 号为 0(旧版 scene_manager):按 player_id 认领后忽略 —— 条目已在核实,结论由核实给。
    DeliverEnterSceneReply(MakeReply(kExitPlayerId, 0, 0));
    DeliverEnterSceneReply(MakeReply(kExitPlayerId, 7, 0));
    auto expected = before;
    expected.replyIgnored += 2;
    ExpectRelocateStats(RelocateStats(), expected); // ③ reply_verified、outcomes、push 全程不变
    EXPECT_EQ(travel_handoff_stats::Read().replyUncorrelated, travelBefore.replyUncorrelated + 2)
        << "号为 0 的应答在更前面由既有代码计 reply_uncorrelated";

    const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(entry.has_value()) << "应答从不直接定案:成功不结清,拒绝不踢";
    EXPECT_EQ(entry->phase, rc::Phase::kVerifying);
    EXPECT_EQ(entry->evidence, rc::Evidence::kNotSent);
    EXPECT_FALSE(entry->completionSeen) << "退路认领归属不精确,不记「完成通知已到」";
    EXPECT_EQ(entry->replyErrorCode, 0u);

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, TransportFailureForANotSentRelocateIsNotClaimed)
{
    // 传输失败只按号精确认领,没有 player_id 退路:条目没发出去(号为 0)时什么失败都不是它的。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate());
    const auto before = RelocateStats();

    DeliverEnterSceneTransportFailure(kExitPlayerId, 77, kTransportFailureReason);
    // 号为 0 的失败在分发入口就被丢弃(只能来自绕过统一出口的发送)。
    DeliverEnterSceneTransportFailure(kExitPlayerId, 0, kTransportFailureReason);

    ExpectRelocateStats(RelocateStats(), before); // transport_failed、reply_ignored 都不动
    const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(entry.has_value());
    EXPECT_EQ(entry->phase, rc::Phase::kVerifying);
    EXPECT_EQ(entry->evidence, rc::Evidence::kNotSent) << "证据没被换成「结果未知」";
    EXPECT_FALSE(entry->completionSeen);

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, ReentryOnSameSessionWaitsForTheLoadThenResolves)
{
    // scene_manager 把他放回了本节点(更早的请求、或放回正在排空的同一个场景):进场路由带着同一条会话到达,
    // 条目转入「落地等载入」,实体建出来就按 landed_here 结清,不踢。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate());
    const auto before = RelocateStats();
    auto &pendingEnter = PlayerLifecycleSystem::GetPendingEnterMap();

    // 1. 进场 Reconcile(PlayerEnterGameNode 守护段最前面,此时还没登记待入场条目)。
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, kSessionAtExit);
    {
        const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
        ASSERT_TRUE(entry.has_value());
        EXPECT_EQ(entry->phase, rc::Phase::kLanding);
        EXPECT_TRUE(entry->reentered);
        EXPECT_FALSE(entry->retryPending) << "转入落地时作废还在排队的核实重试";
    }

    // 2. 载入在途(待入场表里有他):每拍只是等。
    pendingEnter[kExitPlayerId] = PlayerEnterContext{};
    PlayerLifecycleSystem::SweepRelocateConfirms(rc::Clock::now() + 1s, /*reconnected=*/false);
    {
        const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
        ASSERT_TRUE(entry.has_value());
        EXPECT_EQ(entry->phase, rc::Phase::kLanding);
    }

    // 3. 实体建出来了(会话就是票据那一条)。此刻到达的应答 —— 哪怕号为 0 —— 不得被待确认表按 player_id 吞掉:
    //    认领挂在「实体已不在」分支里,实体有效时应答照旧走普通分发(落到 kNoWaiter)。号为 0 是最强的反例。
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    DeliverEnterSceneReply(MakeReply(kExitPlayerId, 0, 0));
    ExpectRelocateStats(RelocateStats(), before); // reply_ignored / reply_verified 都不动
    {
        const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
        ASSERT_TRUE(entry.has_value());
        EXPECT_EQ(entry->phase, rc::Phase::kLanding);
    }

    // 4. 载入结束,下一拍看到实体:结清,不踢、不推 gate。
    pendingEnter.erase(kExitPlayerId);
    PlayerLifecycleSystem::SweepRelocateConfirms(rc::Clock::now() + 2s, /*reconnected=*/false);
    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
    auto expected = before;
    ++expected.outcomes[RelocateIndex(rc::Outcome::kLandedHere)];
    ExpectRelocateStats(RelocateStats(), expected);
    EXPECT_TRUE(tlsEcs.actorRegistry.valid(player)) << "结清不碰实体";

    // 5. 清理。
    tlsEcs.Clear();
    SessionMap().clear();
    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, AbandonedLandingIsVerifiedThenKicked)
{
    // 进场路由到过本节点,载入却被放弃(继承标记核对不过 / 载入失败 / 会话在载入中取消):那几条出口只回 tip、不踢线。
    // 待确认表看到「路由到过、载入已不在途、实体没建出来」→ 核实 → 读不到而本节点替他发的请求都没写过落点 → 踢。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate());
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, kSessionAtExit);
    const auto before = RelocateStats();

    // 待入场表里没有他 = 载入已被放弃。
    const auto t1 = rc::Clock::now() + 1s;
    PlayerLifecycleSystem::SweepRelocateConfirms(t1, /*reconnected=*/false);
    {
        const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
        ASSERT_TRUE(entry.has_value());
        EXPECT_EQ(entry->phase, rc::Phase::kVerifying);
        EXPECT_EQ(entry->evidence, rc::Evidence::kLandingAbandoned);
        EXPECT_EQ(entry->expectation, rc::Expectation::kAtThisNode) << "落地核实不比场景:仍指向本节点就算没变";
        EXPECT_TRUE(entry->deadline == t1 + rc::kVerifyBudget) << "这一轮核实从注入的时刻起算";
    }
    auto expected = before;
    ++expected.verifyDeferred; // 宿主没有 Redis
    ExpectRelocateStats(RelocateStats(), expected);

    PlayerLifecycleSystem::SweepRelocateConfirms(t1 + rc::kVerifyBudget + 1s, /*reconnected=*/false);
    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
    ++expected.outcomes[RelocateIndex(rc::Outcome::kKickUnverified)];
    ++expected.push[RelocateIndex(rc::GatePushResult::kGateGone)];
    ++expected.credentialActions[RelocateIndex(rc::CredentialAction::kNone)];
    ExpectRelocateStats(RelocateStats(), expected);

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, KickIsWithheldWhenTheEntityIsBackOnThisNode)
{
    // 踢线前的本地复核:走到「该踢了」的那一刻,本节点上却有他的有效实体(载入卡过落地预算才建出来、或又一条
    // 路由把他带了回来)—— 踢下去打掉的是一条正在本节点上的合法会话。不踢、不推 gate、不计凭证判定。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate());
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, kSessionAtExit);
    const auto t1 = rc::Clock::now() + 1s;
    PlayerLifecycleSystem::SweepRelocateConfirms(t1, /*reconnected=*/false);
    {
        const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
        ASSERT_TRUE(entry.has_value());
        ASSERT_EQ(entry->phase, rc::Phase::kVerifying);
        ASSERT_EQ(entry->evidence, rc::Evidence::kLandingAbandoned);
    }
    const auto before = RelocateStats();

    // 手工建出实体(会话就是票据那一条),不经 Reconcile:条目仍在核实阶段。
    const auto player = MakeOnlinePlayer(kSessionAtExit);

    PlayerLifecycleSystem::SweepRelocateConfirms(t1 + rc::kVerifyBudget + 1s, /*reconnected=*/false);

    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
    auto expected = before;
    ++expected.outcomes[RelocateIndex(rc::Outcome::kLandedHere)];
    ExpectRelocateStats(RelocateStats(), expected); // kick_unverified、push、credential_* 都不动
    EXPECT_TRUE(tlsEcs.actorRegistry.valid(player));

    tlsEcs.Clear();
    SessionMap().clear();
    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, ReentryOnAnotherSessionSupersedesWithoutKick)
{
    // 进场带来的是另一条会话:玩家已经重登,票据里的旧会话已死。按 superseded 结清,不踢新会话。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate());
    const auto before = RelocateStats();

    PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, kNewerSession);

    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
    auto expected = before;
    ++expected.outcomes[RelocateIndex(rc::Outcome::kSuperseded)];
    ExpectRelocateStats(RelocateStats(), expected); // push 不动

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, ClientDisconnectDuringDrainVoidsTheTicket)
{
    // 排空发了票、退出还没收敛时客户端断线了:会话已死,给它改派只会让 location 停在一个从未载入该玩家的节点上。
    // 票据作废、不登记、不发改派;A1′ 按第一次退出原因(单场景排空)接手写断线释放标记。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    const ScopedConfirmedNodeIdentity identityConfirmed; // 下面有 ASSERT:用守卫保证探针一定被复位
    const auto made = MakeRelocateExitingPlayer();
    const auto before = RelocateStats();
    const auto releaseBefore = exit_release_stats::Read();

    // 1. 发票:已在退出中,只合并原因;实体与票据都留着。
    EXPECT_EQ(PlayerLifecycleSystem::BeginSceneDrain(made.scene), 1u);
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(made.player));
    // 2. 走真实的合并路径:客户端断线粘性置位,第一次的原因不变。
    PlayerLifecycleSystem::HandleExitGameNode(made.player, ExitCause::kClientDisconnect);
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(made.player));
    {
        const auto &intent = tlsEcs.actorRegistry.get<PlayerExitIntentComp>(made.player);
        EXPECT_TRUE(intent.clientDisconnected);
        EXPECT_EQ(intent.cause, ExitCause::kSceneDrain);
    }
    // 3. 退出存盘落地、收敛收尾。
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(made.player, landed));
    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    const auto releaseAfter = exit_release_stats::Read();
    // 需要「身份确认」的那一步(A1′ 的判定)已经跑完,这里就复位,让后面的收尾在默认的「未确认」下跑;
    // 守卫的析构会再复位一次(ASSERT 提前返回时只有它),重复无害。
    PlayerLifecycleSystem::SetNodeIdentityProbe({});
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(made.player));
    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
    auto expected = before;
    ++expected.ticketDecisions[RelocateIndex(rc::TicketDecision::kVoidClientGone)];
    ExpectRelocateStats(RelocateStats(), expected); // dispatch 不动
    const auto skipRelocate = static_cast<std::size_t>(erm::ExitReleaseDecision::kSkipRelocate);
    const auto attempted = static_cast<std::size_t>(erm::ExitReleaseDecision::kWrite);
    EXPECT_EQ(releaseAfter.decisions[skipRelocate], releaseBefore.decisions[skipRelocate]) << "票据没被消费";
    EXPECT_EQ(releaseAfter.decisions[attempted], releaseBefore.decisions[attempted] + 1) << "A1′ 接手";
    EXPECT_EQ(releaseAfter.failed, releaseBefore.failed + 1) << "宿主没有 Redis:判定为写、写不成";

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, ReentryCancelsAPendingTicket)
{
    // 票据还没派发,进场路由就把他带回了本节点(这次进场要么取消退出、要么废黜实体后重载):票据作废。
    // 不作废的话,它会一直留到下一次、会话已换的退出里被误消费。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    const auto made = MakeRelocateExitingPlayer();
    const auto before = RelocateStats();

    EXPECT_EQ(PlayerLifecycleSystem::BeginSceneDrain(made.scene), 1u);
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(made.player));

    PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, kSessionAtExit);
    auto expected = before;
    ++expected.cancelledOnReentry;
    ExpectRelocateStats(RelocateStats(), expected);

    // 这里不模拟「重连取消退出」,直接让那次退出收敛:已经没有票据可消费,不发改派、不登记。
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(made.player, landed));
    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(made.player));
    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
    ExpectRelocateStats(RelocateStats(), expected); // ticketDecisions 各项都不动

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, DeposedEntityDropsItsTicketWithoutRelocating)
{
    // 票据的第二个删除点:实体带着没派发的票据被按「已废黜」销毁(这里是进场时发现中间有过别的持有者)。
    // 不发改派、不登记、不踢,只留计数 —— 这条路不经 DispatchEmergencyRelocate。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    const auto made = MakeRelocateExitingPlayer(); // 缓存 owner_epoch 5
    const auto before = RelocateStats();
    EXPECT_EQ(PlayerLifecycleSystem::BeginSceneDrain(made.scene), 1u);
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(made.player));

    EXPECT_TRUE(PlayerLifecycleSystem::DiscardDeposedEntityOnReentry(made.player, 7)) << "epoch 跳了 2:中间有别的持有者";

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(made.player));
    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
    auto expected = before;
    ++expected.ticketsDroppedDeposed;
    ExpectRelocateStats(RelocateStats(), expected); // ticketDecisions、push 都不动

    // 票据确实已删:再来一次进场 Reconcile 没有票据可作废。
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, kSessionAtExit);
    ExpectRelocateStats(RelocateStats(), expected);

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, EarlierHandoffEnterSceneIsRecordedOnTheEntry)
{
    // 退出优先作废过一次已写标记的交接:那条交接 EnterScene 可能还在 scene_manager 排队,而且可以凭改派刚写的同代标记
    // 过门。登记时把它记在条目上 —— 此后 settle 之前不读、不提前踢(读到的可能是那条请求「铸造 → 回滚」窗口的中间态)。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    const auto made = MakeRelocateExitingPlayer(/*travelHandoffMarkIssued=*/true);
    const auto before = RelocateStats();

    EXPECT_EQ(PlayerLifecycleSystem::BeginSceneDrain(made.scene), 1u);
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(made.player, landed));
    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(made.player));
    const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(entry.has_value());
    EXPECT_TRUE(entry->earlierEnterSceneMayReply);
    EXPECT_TRUE(rc::RequestOutcomeUnsettled(*entry));
    EXPECT_EQ(entry->phase, rc::Phase::kVerifying);
    EXPECT_EQ(entry->evidence, rc::Evidence::kNotSent) << "宿主没有 scene_manager";
    // settle 窗口从 scene_manager 的调用 deadline 派生;宿主里没有 Apply 过配置,deadline 是内置默认 10000ms → 15s。
    const auto budgets = rc::BudgetsFor(grpc_call_deadline::Get(eNodeType::SceneManagerNodeService));
    EXPECT_TRUE(budgets.settleWindow == rc::kSmSettleWindow);
    EXPECT_TRUE(entry->settleAt - entry->createdAt == budgets.settleWindow) << "没发出去:settle 仍从登记时刻起算";
    EXPECT_TRUE(entry->nextVerifyAt == entry->settleAt) << "首次核实排到 settleAt";
    EXPECT_TRUE(entry->deadline == entry->settleAt + rc::kVerifyBudget) << "settle 之后还有一整轮核实";
    EXPECT_FALSE(entry->retryPending) << "不是「没读到等重试」:重连不绕过 settle 等待";

    auto expected = before;
    ++expected.ticketDecisions[RelocateIndex(rc::TicketDecision::kDispatch)];
    ++expected.notSent;
    ++expected.settleWaits; // 没有发核实:verify_deferred 不动
    ExpectRelocateStats(RelocateStats(), expected);

    EXPECT_FALSE(rc::MayKickBeforeSettle(rc::Evidence::kNotSent, rc::RequestOutcomeUnsettled(*entry)));
    PlayerLifecycleSystem::SweepRelocateConfirms(rc::Clock::now() + 1s, /*reconnected=*/true);
    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 1u) << "settle 之前一拍(含重连)什么都不做";
    ExpectRelocateStats(RelocateStats(), expected);

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, InFlightSceneChangeIsRecordedAsAnEarlierRequest)
{
    // 「更早请求」的第二个来源:退出时实体上还挂着一条在途的普通换图。它发出不满一个 settle 窗口时,scene_manager
    // 可能还会处理它(而且可以凭改派刚写的同代标记过门),登记时要记在条目上;超过一个 settle 窗口的不算。
    // 这条用例钉的是 FinishExitAfterPersist 里传给 SceneChangeReplyMayArrive 的实参接线(现在时刻 / 发出时刻 / 窗口,
    // 都是毫秒):写反或单位取错,纯函数用例发现不了。三段都读真实墙钟,余量是秒级(15s 窗口对 0s / 16s / 5s):
    // ① ② 抓得住实参写反、窗口偏大(例如取成纳秒);③ 的 5s 落在 15s 之内、15ms 之外,抓得住窗口按秒传成 15。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());

    // ① 刚发出的换图(AttachSceneChange 把发出时刻记成现在):在窗口内 → 记为「更早请求可能被放行」,首次核实排到 settle。
    {
        const auto player = MakeOnlinePlayer(kSessionAtExit);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
        AttachSceneChange(player, 777, /*playerRequested=*/true, 9);
        const auto scene = PlaceInScene(player, kRelocateSourceScene);
        ASSERT_TRUE(MarkPersisted(player));
        const auto before = RelocateStats();

        ASSERT_EQ(PlayerLifecycleSystem::BeginSceneDrain(scene), 1u);
        ASSERT_FALSE(tlsEcs.actorRegistry.valid(player));
        const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
        ASSERT_TRUE(entry.has_value());
        EXPECT_TRUE(entry->earlierEnterSceneMayReply);
        EXPECT_TRUE(entry->nextVerifyAt == entry->settleAt) << "首次核实排到 settleAt";
        auto expected = before;
        ++expected.ticketDecisions[RelocateIndex(rc::TicketDecision::kDispatch)];
        ++expected.notSent;
        ++expected.settleWaits; // 没有发核实:verify_deferred 不动
        ExpectRelocateStats(RelocateStats(), expected);
    }
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());

    // ② 对照:同一条换图发出已有 16s(超过 15s 的 settle 窗口)→ 不算,核实立即发(宿主没有 Redis,记推迟)。
    {
        const auto player = MakeOnlinePlayer(kSessionAtExit);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
        AttachSceneChange(player, 777, /*playerRequested=*/true, 9);
        tlsEcs.actorRegistry.get<PlayerSceneChangeInFlightComp>(player).sentAtMs = TimeSystem::NowMillisecondsUTC() - 16000;
        const auto scene = PlaceInScene(player, kRelocateSourceScene);
        ASSERT_TRUE(MarkPersisted(player));
        const auto before = RelocateStats();

        ASSERT_EQ(PlayerLifecycleSystem::BeginSceneDrain(scene), 1u);
        ASSERT_FALSE(tlsEcs.actorRegistry.valid(player));
        const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
        ASSERT_TRUE(entry.has_value());
        EXPECT_FALSE(entry->earlierEnterSceneMayReply);
        EXPECT_TRUE(entry->retryPending) << "核实读发不出去,等 1s 节拍重试";
        auto expected = before;
        ++expected.ticketDecisions[RelocateIndex(rc::TicketDecision::kDispatch)];
        ++expected.notSent;
        ++expected.verifyDeferred;
        ExpectRelocateStats(RelocateStats(), expected);
    }
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());

    // ③ 发出已有 5s:仍在 15s 的窗口内 → 算。窗口若被按秒传进去(15 而不是 15000),5000ms 就会被判成「已过窗口」。
    {
        const auto player = MakeOnlinePlayer(kSessionAtExit);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
        AttachSceneChange(player, 777, /*playerRequested=*/true, 9);
        tlsEcs.actorRegistry.get<PlayerSceneChangeInFlightComp>(player).sentAtMs = TimeSystem::NowMillisecondsUTC() - 5000;
        const auto scene = PlaceInScene(player, kRelocateSourceScene);
        ASSERT_TRUE(MarkPersisted(player));
        const auto before = RelocateStats();

        ASSERT_EQ(PlayerLifecycleSystem::BeginSceneDrain(scene), 1u);
        ASSERT_FALSE(tlsEcs.actorRegistry.valid(player));
        const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
        ASSERT_TRUE(entry.has_value());
        EXPECT_TRUE(entry->earlierEnterSceneMayReply);
        auto expected = before;
        ++expected.ticketDecisions[RelocateIndex(rc::TicketDecision::kDispatch)];
        ++expected.notSent;
        ++expected.settleWaits;
        ExpectRelocateStats(RelocateStats(), expected);
    }

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, ReplacedSessionVoidsTheTicket)
{
    // 排空发了票、退出还没收敛时,实体上的会话快照已经换成另一条:票据里抄的旧会话不是他当前的会话了,给旧会话
    // 改派没有意义。票据作废、不登记、不发改派。钉的是 DispatchEmergencyRelocate 里「当前会话有效」与「当前会话就是
    // 票据那一条」两个实参的接线(此前粘合层只走过 dispatch 与 void_client_gone 两个取值)。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    const auto made = MakeRelocateExitingPlayer();
    const auto before = RelocateStats();

    EXPECT_EQ(PlayerLifecycleSystem::BeginSceneDrain(made.scene), 1u);
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(made.player));
    // 只改实体上的会话快照、不写 SessionMap:退出分支的「被取代」判定要求新会话已映射到本玩家,这里不成立,
    // 退出照常收敛;走到票据判定时就是「当前会话有效,但不是票据那一条」。
    tlsEcs.actorRegistry.get<PlayerSessionSnapshotComp>(made.player).set_gate_session_id(kNewerSession);

    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(made.player, landed));
    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(made.player));
    EXPECT_EQ(PlayerLifecycleSystem::RelocateConfirmsPending(), 0u);
    auto expected = before;
    ++expected.ticketDecisions[RelocateIndex(rc::TicketDecision::kVoidSessionReplaced)];
    ExpectRelocateStats(RelocateStats(), expected); // dispatch、not_sent 都不动

    SessionMap().clear(); // 旧会话的映射没有随「已换过的快照」摘掉,别留给下一条
    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, NextRelocateSupersedesALandingEntryWithoutKick)
{
    // 同一玩家上一次改派的条目还在等载入(他刚被路由带回本节点、1s 节拍还没来得及结清)就又被排空:旧条目按
    // superseded 结清(不踢:玩家本人就在本节点上,正要被再次改派),新条目照常登记。上一条没有结局未定的请求
    // (它根本没发出去),所以不往新条目上带「更早请求」。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());
    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate());
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, kSessionAtExit);
    const auto first = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(first.has_value());
    ASSERT_EQ(first->phase, rc::Phase::kLanding);
    ASSERT_FALSE(rc::RequestOutcomeUnsettled(*first));
    const auto before = RelocateStats();

    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate()); // 第二次排空;它内部断言待确认表里仍只有一条

    const auto second = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(second.has_value());
    EXPECT_NE(second->seq, first->seq) << "是新登记的一条";
    EXPECT_EQ(second->phase, rc::Phase::kVerifying);
    EXPECT_EQ(second->evidence, rc::Evidence::kNotSent);
    EXPECT_FALSE(second->reentered);
    EXPECT_FALSE(second->earlierEnterSceneMayReply);
    auto expected = before;
    ++expected.ticketDecisions[RelocateIndex(rc::TicketDecision::kDispatch)];
    ++expected.notSent;
    ++expected.verifyDeferred;
    ++expected.outcomes[RelocateIndex(rc::Outcome::kSuperseded)];
    ExpectRelocateStats(RelocateStats(), expected); // 不踢:kick_*、push 都不动

    DrainRelocateConfirms();
}

TEST(RelocateConfirmEcs, UnsettledPreviousRelocateIsCarriedIntoTheNextOne)
{
    // 「更早请求」的第三个来源:同一玩家上一次改派的条目还没收口、它名下还有结局未定的请求、又没过它的 settle ——
    // 那条请求仍可能被 scene_manager 处理,而且可以凭这次改派刚写的同代标记过门。新条目要把它记下来:
    // 被瞬时拒绝时不立即踢,首次核实排到 settle。
    // 宿主里发不出 EnterScene,造不出「上一条自己的请求在途」;这里用「上一条登记时就带着更早请求」来走同一行判据
    // (RequestOutcomeUnsettled(上一条) 且还没过它的 settleAt)。两次登记之间只隔毫秒,settle 窗口是 15s。
    ASSERT_NO_FATAL_FAILURE(DrainRelocateConfirms());

    // 第一次改派:退出优先作废过一次已写标记的交接 → 条目带「更早请求可能被放行」。
    const auto made = MakeRelocateExitingPlayer(/*travelHandoffMarkIssued=*/true);
    EXPECT_EQ(PlayerLifecycleSystem::BeginSceneDrain(made.scene), 1u);
    PlayerAllData landed;
    ASSERT_TRUE(MarshalAsSaved(made.player, landed));
    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);
    ASSERT_FALSE(tlsEcs.actorRegistry.valid(made.player));
    // 同一条会话被路由带回本节点:条目转入等载入,还没被 1s 节拍结清。
    PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, kSessionAtExit);
    const auto first = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(first.has_value());
    ASSERT_EQ(first->phase, rc::Phase::kLanding);
    ASSERT_TRUE(rc::RequestOutcomeUnsettled(*first));
    const auto before = RelocateStats();

    // 第二次排空:这一次退出本身没有任何更早的请求(没有交接、没有在途换图)。
    ASSERT_NO_FATAL_FAILURE(RegisterNotSentRelocate());

    const auto second = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(second.has_value());
    EXPECT_NE(second->seq, first->seq);
    EXPECT_TRUE(second->earlierEnterSceneMayReply) << "上一条名下结局未定的请求带进了新条目";
    EXPECT_EQ(second->phase, rc::Phase::kVerifying);
    EXPECT_EQ(second->evidence, rc::Evidence::kNotSent);
    EXPECT_TRUE(second->nextVerifyAt == second->settleAt) << "首次核实排到 settleAt,不立即读";
    EXPECT_FALSE(second->retryPending);
    auto expected = before;
    ++expected.ticketDecisions[RelocateIndex(rc::TicketDecision::kDispatch)];
    ++expected.notSent;
    ++expected.settleWaits; // 没有发核实:verify_deferred 不动
    ++expected.outcomes[RelocateIndex(rc::Outcome::kSuperseded)];
    ExpectRelocateStats(RelocateStats(), expected);

    DrainRelocateConfirms();
}

// ============================================================================
// DBTask topic 世代号(player-storage-placement.md §7 / §12 A14)
// ============================================================================
// CZ-2 让存盘按 home_zone 选 topic;这里钉住 topic 名里的世代号规则。C++ scene 是生产者、go/db 是
// 唯一的消费者,两边规则一旦漂移不报任何错,存盘静默写进没人消费的 topic —— 所以期望值逐条抄自
// go/db/internal/config 与 go/login/internal/config 的 DbTaskTopicForGeneration,不从 C++ 实现反推。
#include "player/constants/player.h"

TEST(DbTaskTopicGeneration, FirstGenerationKeepsLegacyNameWithoutSuffix)
{
    // 第一代的名字在引入世代号前就已上线,不能补 _g1(与审计 topic 第一代就带 _g1 刻意不同)。
    EXPECT_EQ(GetDbTaskTopic(1, 1), "db_task_zone_1");
    EXPECT_EQ(GetDbTaskTopic(42, 1), "db_task_zone_42");
}

TEST(DbTaskTopicGeneration, ZeroMeansUnconfiguredAndEqualsFirstGeneration)
{
    // proto3 没有 presence:yaml 不写 DbTaskTopicGeneration 就是 0,必须与第一代同名,存量部署不改也能起。
    EXPECT_EQ(GetDbTaskTopic(3, 0), GetDbTaskTopic(3, 1));
    EXPECT_EQ(GetDbTaskTopic(3, 0), "db_task_zone_3");
}

TEST(DbTaskTopicGeneration, LaterGenerationsAppendSuffix)
{
    EXPECT_EQ(GetDbTaskTopic(1, 2), "db_task_zone_1_g2");
    EXPECT_EQ(GetDbTaskTopic(7, 10), "db_task_zone_7_g10");
    EXPECT_EQ(GetDbTaskTopic(4294967295u, 4294967295u), "db_task_zone_4294967295_g4294967295");
}

TEST(DbTaskTopicGeneration, GenerationDoesNotChangeZoneRouting)
{
    // 世代号只改名字后缀,不改按 zone 分 topic 的路由:不同 zone 同世代必然不同名。
    EXPECT_NE(GetDbTaskTopic(1, 2), GetDbTaskTopic(2, 2));
    EXPECT_NE(GetDbTaskTopic(1, 1), GetDbTaskTopic(1, 2));
}

// 15. BagEquipInstanceData —— 装备实例数据(ItemEntry.equip,docs/design/equipment-attributes.md §3.1)
// 随存盘的 PlayerAllData 过一跳不丢。目标节点是从共享 Redis 里的 PlayerAllData 载入玩家的,所以这里
// 走的是「marshal 成 PlayerAllData → 整条序列化 / 解析 → 从记录里的 bag_component 还原」。
//
// 还原这一步用 bag_marshal::Unmarshal 而不是 PlayerAllDataMessageFieldsUnmarshal:本测试宿主不加载
// 任何配置表,整条 unmarshal 会连带跑属性 / 宝宝的载入初始化,那不是这条用例要钉的东西
// (整条入口的往返在 bag_test 的 PlayerFeaturePersistenceTest 里,那边加载了表)。
// 配置号沿用本文件上方的 3001 —— 表里查不到,装备栏按「还原宽容」退回快照里的槽位。
// ============================================================================
TEST(CrossZoneBagMarshal, EquipInstanceDataSurvivesThePlayerAllDataHop)
{
    tlsEcs.actorRegistry.clear();

    constexpr Guid kWornGuid = 9300001;
    constexpr Guid kSpareGuid = 9300002;
    constexpr Guid kPotionGuid = 9300003;

    const auto source = tlsEcs.actorRegistry.create();
    CreatePersistedPlayerComps(source);
    {
        auto& bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(source);

        // 穿在身上的一件:两条属性,四个字段各取不同的值。
        ItemComp worn;
        worn.set_item_id(kWornGuid);
        worn.set_config_id(3001);
        worn.set_size(1);
        auto* blue = worn.mutable_equip()->add_affixes();
        blue->set_attr_id(8);
        blue->set_tier(1);
        blue->set_value(77);
        blue->set_seq(0);
        auto* pink = worn.mutable_equip()->add_affixes();
        pink->set_attr_id(16);
        pink->set_tier(2);
        pink->set_value(6);
        pink->set_seq(1);
        // 二期(强化 + 镶嵌):强化等级与两颗宝石。孔位号故意不等于下标 —— 位置是字段,不是 repeated 的顺序。
        worn.mutable_equip()->set_enhance_level(7);
        auto* gemInSocket1 = worn.mutable_equip()->add_gems();
        gemInSocket1->set_socket(1);
        gemInSocket1->set_gem_config_id(2101);
        auto* gemInSocket0 = worn.mutable_equip()->add_gems();
        gemInSocket0->set_socket(0);
        gemInSocket0->set_gem_config_id(2302);
        bags.bags[kEquipment].RestoreItems({{worn, 5}});

        // 背包里的一件:已初始化但一条都没掷出,只有 presence。
        ItemComp spare;
        spare.set_item_id(kSpareGuid);
        spare.set_config_id(3002);
        spare.set_size(1);
        spare.mutable_equip();
        bags.bags[kInventory].RestoreItems({{spare, 2}});

        // 普通物品:没有 equip 段。
        ItemComp potion;
        potion.set_item_id(kPotionGuid);
        potion.set_config_id(1001);
        potion.set_size(7);
        bags.bags[kInventory].RestoreItems({{potion, 3}});
    }

    PlayerAllData saved;
    ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(source, saved));
    std::string bytes;
    ASSERT_TRUE(saved.SerializeToString(&bytes));
    PlayerAllData landed;
    ASSERT_TRUE(landed.ParseFromString(bytes));

    // 数据库记录与顶层兼容镜像是两份拷贝,哪一份丢了 equip 都会在某条读路径上重掷。
    const auto expectCopyCarriesEquip = [&](const BagAllData& copy, const char* which) {
        ASSERT_EQ(copy.items_size(), 3) << which;
        int withEquip = 0;
        for (const auto& entry : copy.items())
        {
            if (entry.item_uuid() == kWornGuid)
            {
                ASSERT_TRUE(entry.has_equip()) << which;
                ASSERT_EQ(entry.equip().affixes_size(), 2) << which;
                EXPECT_EQ(entry.equip().affixes(0).attr_id(), 8u) << which;
                EXPECT_EQ(entry.equip().affixes(0).tier(), 1u) << which;
                EXPECT_EQ(entry.equip().affixes(0).value(), 77u) << which;
                EXPECT_EQ(entry.equip().affixes(0).seq(), 0u) << which;
                EXPECT_EQ(entry.equip().affixes(1).attr_id(), 16u) << which;
                EXPECT_EQ(entry.equip().affixes(1).tier(), 2u) << which;
                EXPECT_EQ(entry.equip().affixes(1).value(), 6u) << which;
                EXPECT_EQ(entry.equip().affixes(1).seq(), 1u) << which;
                EXPECT_EQ(entry.equip().enhance_level(), 7u) << which;
                ASSERT_EQ(entry.equip().gems_size(), 2) << which;
                EXPECT_EQ(entry.equip().gems(0).socket(), 1u) << which;
                EXPECT_EQ(entry.equip().gems(0).gem_config_id(), 2101u) << which;
                EXPECT_EQ(entry.equip().gems(1).socket(), 0u) << which;
                EXPECT_EQ(entry.equip().gems(1).gem_config_id(), 2302u) << which;
            }
            else if (entry.item_uuid() == kSpareGuid)
            {
                EXPECT_TRUE(entry.has_equip()) << which << ": zero rows is still 'already rolled'";
                EXPECT_EQ(entry.equip().affixes_size(), 0) << which;
                EXPECT_EQ(entry.equip().enhance_level(), 0u) << which;
                EXPECT_EQ(entry.equip().gems_size(), 0) << which;
            }
            else
            {
                EXPECT_FALSE(entry.has_equip()) << which << ": a plain item must not grow an equip section";
            }
            withEquip += entry.has_equip() ? 1 : 0;
        }
        EXPECT_EQ(withEquip, 2) << which;
    };
    ASSERT_TRUE(landed.player_database_data().has_bag_component());
    ASSERT_NO_FATAL_FAILURE(
        expectCopyCarriesEquip(landed.player_database_data().bag_component(), "player_database.bag_component"));
    ASSERT_NO_FATAL_FAILURE(expectCopyCarriesEquip(landed.bag_data(), "PlayerAllData.bag_data"));

    const auto dest = tlsEcs.actorRegistry.create();
    bag_marshal::Unmarshal(dest, landed.player_database_data().bag_component());
    auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(dest);
    ASSERT_NE(bags, nullptr);

    const ItemComp* worn = bags->bags[kEquipment].GetItemCompByGuid(kWornGuid);
    const ItemComp* spare = bags->bags[kInventory].GetItemCompByGuid(kSpareGuid);
    const ItemComp* potion = bags->bags[kInventory].GetItemCompByGuid(kPotionGuid);
    ASSERT_NE(worn, nullptr);
    ASSERT_NE(spare, nullptr);
    ASSERT_NE(potion, nullptr);

    ASSERT_TRUE(worn->has_equip());
    ASSERT_EQ(worn->equip().affixes_size(), 2);
    EXPECT_EQ(worn->equip().affixes(0).attr_id(), 8u);
    EXPECT_EQ(worn->equip().affixes(0).tier(), 1u);
    EXPECT_EQ(worn->equip().affixes(0).value(), 77u);
    EXPECT_EQ(worn->equip().affixes(0).seq(), 0u);
    EXPECT_EQ(worn->equip().affixes(1).attr_id(), 16u);
    EXPECT_EQ(worn->equip().affixes(1).tier(), 2u);
    EXPECT_EQ(worn->equip().affixes(1).value(), 6u);
    EXPECT_EQ(worn->equip().affixes(1).seq(), 1u);
    EXPECT_EQ(worn->equip().enhance_level(), 7u) << "enhance level must survive the hop";
    ASSERT_EQ(worn->equip().gems_size(), 2) << "inlaid gems must survive the hop";
    EXPECT_EQ(worn->equip().gems(0).socket(), 1u);
    EXPECT_EQ(worn->equip().gems(0).gem_config_id(), 2101u);
    EXPECT_EQ(worn->equip().gems(1).socket(), 0u);
    EXPECT_EQ(worn->equip().gems(1).gem_config_id(), 2302u);
    EXPECT_EQ(bags->bags[kEquipment].GetItemPosByGuid(kWornGuid), 5u) << "worn gear keeps its slot";

    EXPECT_TRUE(spare->has_equip());
    EXPECT_EQ(spare->equip().affixes_size(), 0);
    EXPECT_FALSE(potion->has_equip());
    EXPECT_EQ(potion->size(), 7u);

    for (const auto& bag : bags->bags)
    {
        EXPECT_TRUE(bag.IsLayerConsistent());
    }
}

// ============================================================================
// 16. EnterFence —— 入场会话栅栏:登录代次(docs/design/scene-enter-session-fence.md §5、§10)。
//     EnterFence.*    enter_fence.h 的纯判定:Decide 判定表逐格、DecideAll 的合并规则、名表与放行集合。
//     EnterFenceEcs.* 无 Redis 宿主里直接驱动 PlayerLifecycleSystem::HandleEnterGameNode(AsyncLoad 留在节点层,
//                     这里不发):陈旧的跨会话入场在实体 / 待入场 / 改派待确认条目三条路径上都被拒、且不碰任何状态;
//                     同一会话合并;SessionMap 只擦属于自己的映射;EnterScene 的绑定时复核。
//     缺陷复现形状(§1):同一角色两台设备落在同一个 scene 节点,同节点顶号两次路由的 owner_epoch 相同(都取 5),
//     先登录那一方(A)的入场晚于顶号方(B)到达。修复前 A 的入场会擦掉 B 的会话、把会话绑回 sA。
// ============================================================================
#include "services/scene/player/system/enter_fence.h"
#include "proto/common/component/player_login_comp.pb.h" // PlayerEnterGameStateComp、LOGIN_*

// 让 gtest 在断言失败时打出结论名,而不是「1-byte object <04>」。只影响失败输出。
namespace enter_fence
{
inline void PrintTo(Verdict value, std::ostream *os) { *os << VerdictName(value); }
} // namespace enter_fence

namespace
{
namespace ef = enter_fence;

constexpr SessionId kFenceSessionOld = 262144; // 更早的一次登录(A 的入场从没被处理过,本节点上留着的是它)
constexpr SessionId kFenceSessionA = 262145;   // 先登录、已被踢的设备
constexpr SessionId kFenceSessionB = 262146;   // 顶号的设备
// login 铸的代次带毫秒时间下限(设计 §3.2),取一个毫秒时间戳量级的值;只比大小。
constexpr uint64_t kGenOld = 1760400000000;
constexpr uint64_t kGenA = kGenOld + 1;
constexpr uint64_t kGenB = kGenOld + 2;
constexpr uint64_t kFenceOwnerEpoch = 5;       // 同节点顶号不铸造 epoch:各次路由都带同一个值
constexpr uint64_t kFenceScene = 88;
constexpr Guid kFenceOtherPlayerId = 950500001; // 与本玩家先后占同一会话号的另一名玩家

constexpr ef::Verdict kAllEnterVerdicts[] = {
    ef::Verdict::kAdmitFresh,  ef::Verdict::kAdmitSameSession, ef::Verdict::kAdmitNewer,
    ef::Verdict::kAdmitLegacy, ef::Verdict::kRejectStale,      ef::Verdict::kRejectConflict,
    ef::Verdict::kRejectUnversioned,
};
static_assert(std::size(kAllEnterVerdicts) == ef::kVerdictCount, "新增结论时补进这张表,并补判定表用例");

constexpr ef::Identity FenceIdentity(SessionId session, uint64_t generation)
{
    return ef::Identity{true, session, generation};
}

constexpr ef::Identity kNoIdentity{};

uint64_t FenceCount(ef::Verdict verdict)
{
    return enter_fence_stats::Read().verdicts[static_cast<std::size_t>(verdict)];
}
} // namespace

TEST(EnterFence, DecideTableEveryCell)
{
    struct Row
    {
        ef::Identity current;
        SessionId requestSession;
        uint64_t requestGeneration;
        ef::Verdict expected;
        std::string_view why;
    };
    const Row rows[] = {
        // 不存在:任意请求放行。
        {kNoIdentity, kFenceSessionA, 0, ef::Verdict::kAdmitFresh, "no identity, unversioned request"},
        {kNoIdentity, kFenceSessionA, kGenA, ef::Verdict::kAdmitFresh, "no identity, versioned request"},
        // 同一会话(有效):任意代次放行。
        {FenceIdentity(kFenceSessionB, kGenB), kFenceSessionB, kGenB, ef::Verdict::kAdmitSameSession, "same session, equal"},
        {FenceIdentity(kFenceSessionB, kGenB), kFenceSessionB, kGenA, ef::Verdict::kAdmitSameSession, "same session, lower"},
        {FenceIdentity(kFenceSessionB, kGenB), kFenceSessionB, 0, ef::Verdict::kAdmitSameSession, "same session, unversioned"},
        {FenceIdentity(kFenceSessionB, 0), kFenceSessionB, kGenA, ef::Verdict::kAdmitSameSession, "same session, current 0"},
        // 当前代次 0。
        {FenceIdentity(kFenceSessionA, 0), kFenceSessionB, 0, ef::Verdict::kAdmitLegacy, "both unversioned"},
        {FenceIdentity(kFenceSessionA, 0), kFenceSessionB, kGenB, ef::Verdict::kAdmitNewer, "current unversioned"},
        // 当前代次非 0。
        {FenceIdentity(kFenceSessionB, kGenB), kFenceSessionA, 0, ef::Verdict::kRejectUnversioned, "request unversioned"},
        {FenceIdentity(kFenceSessionA, kGenA), kFenceSessionB, kGenB, ef::Verdict::kAdmitNewer, "request newer"},
        {FenceIdentity(kFenceSessionB, kGenB), kFenceSessionA, kGenA, ef::Verdict::kRejectStale, "request older"},
        {FenceIdentity(kFenceSessionB, kGenB), kFenceSessionA, kGenB, ef::Verdict::kRejectConflict, "equal, other session"},
        {FenceIdentity(kInvalidSessionId, kGenB), kFenceSessionA, kGenB, ef::Verdict::kAdmitSameSession,
         "equal, current has no session (invalid)"},
        {FenceIdentity(0, kGenB), kFenceSessionA, kGenB, ef::Verdict::kAdmitSameSession, "equal, current has no session (0)"},
        // 会话 0 / kInvalidSessionId 不是会话:两边都没有会话时按代次裁决,不走"同一会话"。
        {FenceIdentity(0, kGenB), 0, kGenA, ef::Verdict::kRejectStale, "session 0 is not a session"},
        {FenceIdentity(kInvalidSessionId, 0), kInvalidSessionId, 0, ef::Verdict::kAdmitLegacy, "invalid is not a session"},
    };
    for (const Row &row : rows)
    {
        EXPECT_EQ(ef::Decide(row.current, row.requestSession, row.requestGeneration), row.expected) << row.why;
    }
}

TEST(EnterFence, AdmitsOnlyTheFourAdmitVerdicts)
{
    for (const auto verdict : kAllEnterVerdicts)
    {
        const bool admit = verdict == ef::Verdict::kAdmitFresh || verdict == ef::Verdict::kAdmitSameSession ||
                           verdict == ef::Verdict::kAdmitNewer || verdict == ef::Verdict::kAdmitLegacy;
        EXPECT_EQ(ef::Admits(verdict), admit) << ef::VerdictName(verdict);
    }
    EXPECT_FALSE(ef::Admits(ef::Verdict::kCount)) << "越界按拒绝处理(fail-closed)";
}

TEST(EnterFence, NamesCoverEveryValue)
{
    for (const auto verdict : kAllEnterVerdicts)
    {
        EXPECT_STRNE(ef::VerdictName(verdict), "?");
    }
    EXPECT_STREQ(ef::VerdictName(ef::Verdict::kCount), "?") << "越界不能读出数组外";
    EXPECT_STREQ(ef::IdentitySourceName(ef::IdentitySource::kEntity), "entity");
    EXPECT_STREQ(ef::IdentitySourceName(ef::IdentitySource::kRelocateEntry), "relocate_entry");
    EXPECT_STREQ(ef::IdentitySourceName(ef::IdentitySource::kCount), "?");
}

TEST(EnterFence, DecideAllStopsAtTheFirstRejection)
{
    // 实体放行(更新)、待入场拒(陈旧)、条目拒(冲突):返回第一处拒绝的结论。
    const ef::Identity mixed[] = {FenceIdentity(kFenceSessionOld, kGenOld), FenceIdentity(kFenceSessionB, kGenB),
                                  FenceIdentity(kFenceSessionOld, kGenA)};
    EXPECT_EQ(ef::DecideAll(mixed, kFenceSessionA, kGenA), ef::Verdict::kRejectStale);

    // 改派待确认条目那一格(§5.1 第 3 处):实体与待入场都不在,只有条目记着他 —— 照样挡下陈旧的异会话入场。
    const ef::Identity onlyEntry[] = {kNoIdentity, kNoIdentity, FenceIdentity(kFenceSessionB, kGenB)};
    EXPECT_EQ(ef::DecideAll(onlyEntry, kFenceSessionA, kGenA), ef::Verdict::kRejectStale);
    EXPECT_EQ(ef::DecideAll(onlyEntry, kFenceSessionA, kGenB + 1), ef::Verdict::kAdmitNewer);
}

TEST(EnterFence, DecideAllReturnsTheFirstPresentVerdictWhenAllAdmit)
{
    const ef::Identity none[] = {kNoIdentity, kNoIdentity, kNoIdentity};
    EXPECT_EQ(ef::DecideAll(none, kFenceSessionA, kGenA), ef::Verdict::kAdmitFresh) << "一处都没有";
    EXPECT_EQ(ef::DecideAll(std::span<const ef::Identity>{}, kFenceSessionA, kGenA), ef::Verdict::kAdmitFresh);

    // 实体不在:不存在的那一处不得盖掉真正比过的结论。
    const ef::Identity pendingFirst[] = {kNoIdentity, FenceIdentity(kFenceSessionA, 0), FenceIdentity(kFenceSessionB, kGenOld)};
    EXPECT_EQ(ef::DecideAll(pendingFirst, kFenceSessionB, kGenB), ef::Verdict::kAdmitNewer)
        << "待入场那一处(当前代次 0、请求带代次)先比";

    const ef::Identity entityFirst[] = {FenceIdentity(kFenceSessionA, 0), FenceIdentity(kFenceSessionB, kGenA), kNoIdentity};
    EXPECT_EQ(ef::DecideAll(entityFirst, kFenceSessionB, kGenB), ef::Verdict::kAdmitNewer)
        << "两处都放行(更新 / 同一会话):返回第一处的结论";
    EXPECT_EQ(ef::DecideAll(entityFirst, kFenceSessionB, 0), ef::Verdict::kAdmitLegacy)
        << "实体放行(双方无代次)、待入场同一会话放行:第一处是 legacy,计数靠它";
}

// ── ECS 级接缝:无 Redis 宿主里驱动 HandleEnterGameNode ──
// 本节都用 kExitPlayerId(MakeOnlinePlayer / MakeExitingPlayer 写死的那一名)。待入场表、改派待确认表、SessionMap 都是
// thread_local,不随用例复位:每条用例开头与结尾各调一次 ResetFenceHost。
// 新载入分支在宿主里:A2′ 发不出去(zone Redis 为空)→ 记失败、等重发,待入场条目保留;AsyncLoad 在节点层,不发。
// 被拒时给请求会话推的 tip 在宿主里推不出去(没有 gate),只留 WARN。
namespace
{
void ResetFenceHost()
{
    tlsEcs.Clear();
    SessionMap().clear();
    DrainRelocateConfirms(); // 同时擦掉 kExitPlayerId 的待入场条目
}

// 本文件直接测旧生命周期接口；授权由每例 scope 的明确 SQL 确认原件提供。
// 只补独立的 admission 世代与归属修订，不改变原 login_generation=0 等回归含义。
void SetFixtureAdmission(PlayerEnterContext& context)
{
    context.roleAdmissionGeneration = kFixtureAdmissionGeneration;
    context.ownerRevision = kFixtureOwnerRevision;
}

// 一次真实可入场请求。旧夹具用不存在的 scene_id=0 验会话绑定,掩盖了拒入发生在副作用之后的问题。
PlayerEnterContext FenceEnter(SessionId session, uint64_t generation, uint32_t enterGsType = LOGIN_REPLACE,
                              uint64_t sceneId = kFenceScene)
{
    if (PlayerSceneSystem::FindSceneById(sceneId) == entt::null)
    {
        const auto scene = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_id(sceneId);
    }
    PlayerEnterContext ctx;
    ctx.enterInfo.set_session_id(session);
    ctx.enterInfo.set_enter_gs_type(enterGsType);
    ctx.enterInfo.set_scene_id(sceneId);
    ctx.ownerEpoch = kFenceOwnerEpoch;
    ctx.loginGeneration = generation;
    SetFixtureAdmission(ctx);
    return ctx;
}

PlayerLifecycleSystem::EnterGameNodeOutcome EnterGameNode(const PlayerEnterContext &ctx)
{
    return PlayerLifecycleSystem::HandleEnterGameNode(kExitPlayerId, ctx);
}

// 本节点上一个在线实体:session 已绑定、映射到本玩家,缓存 owner_epoch 与路由相同。generation 非 0 时挂登录身份
// (session, generation);0 = 不挂(旧版实体,栅栏退回快照会话、代次 0)。
entt::entity MakeFencedPlayer(SessionId session, uint64_t generation)
{
    const auto player = MakeOnlinePlayer(session);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = kFenceOwnerEpoch;
    if (generation != 0)
    {
        auto &identity = tlsEcs.actorRegistry.emplace<PlayerLoginIdentityComp>(player);
        identity.sessionId = session;
        identity.loginGeneration = generation;
    }
    return player;
}

// 断言实体的当前绑定:SessionMap、会话快照、登录身份三处一致地指向 (session, generation)。
void ExpectBoundTo(entt::entity player, SessionId session, uint64_t generation)
{
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    ASSERT_EQ(SessionMap().count(session), 1u) << "session " << session;
    EXPECT_EQ(SessionMap().at(session), kExitPlayerId);
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerSessionSnapshotComp>(player).gate_session_id(), session);
    const auto *identity = tlsEcs.actorRegistry.try_get<PlayerLoginIdentityComp>(player);
    ASSERT_NE(identity, nullptr);
    EXPECT_EQ(identity->sessionId, session);
    EXPECT_EQ(identity->loginGeneration, generation);
}

const PlayerEnterContext *PendingEnter()
{
    const auto &pending = PlayerLifecycleSystem::GetPendingEnterMap();
    const auto it = pending.find(kExitPlayerId);
    return it == pending.end() ? nullptr : &it->second;
}



} // namespace

namespace
{
class SceneEntryPermissionTest : public testing::Test
{
protected:
    void SetUp() override
    {
        player = MakeOnlinePlayer(kSessionAtExit);
        auto& registry = tlsEcs.actorRegistry;
        registry.emplace<Player>(player);
        ASSERT_NE(0, actor_net_id::Assign(registry, player));
        source = PlaceInScene(player, kRelocateSourceScene);
        target = tlsEcs.sceneRegistry.create();
        auto& info = tlsEcs.sceneRegistry.emplace<SceneInfoComp>(target);
        info.set_scene_id(kRelocateSourceScene + 1);
        watcher = registry.create();
        registry.emplace<Player>(watcher);
        registry.emplace<PlayerGuidComp>(watcher, kExitPlayerId + 1);
        registry.emplace<Transform>(watcher);
        registry.emplace<SceneEntityComp>(watcher, SceneEntityComp{source});
        ASSERT_NE(0, actor_net_id::Assign(registry, watcher));
        AoiSystem::Update(0);
        ASSERT_TRUE(registry.all_of<Hex>(player));
        const auto* interests = registry.try_get<AoiListComp>(watcher);
        ASSERT_NE(nullptr, interests);
        ASSERT_TRUE(interests->Contains(player));
    }
    void ExpectStillInSource()
    {
        ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
        ASSERT_TRUE((tlsEcs.actorRegistry.all_of<SceneEntityComp, Hex>(player)));
        EXPECT_EQ(source, tlsEcs.actorRegistry.get<SceneEntityComp>(player).sceneEntity);
        const auto* interests = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
        ASSERT_NE(nullptr, interests);
        EXPECT_TRUE(interests->Contains(player));
        const auto* grids = tlsEcs.sceneRegistry.try_get<SceneGridListComp>(source);
        ASSERT_NE(nullptr, grids);
        EXPECT_TRUE(std::any_of(grids->begin(), grids->end(), [this](const auto& entry)
            { return entry.second.entities.contains(player); }));
    }
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    entt::entity watcher{entt::null};
    SceneEntity source{entt::null};
    SceneEntity target{entt::null};
};


} // namespace

// 没有注册 AOI 离场业务 sink,仍必须清理旧格子和观察关系,只保留新场景的唯一绑定。
TEST_F(SceneEntryPermissionTest, PublicSceneSwitchesWithoutBusinessSinkAndCleansOldAoi)
{
    EXPECT_EQ(kSuccess, PlayerSceneSystem::CheckEnterScene(player, target));
    ASSERT_TRUE(PlayerSceneSystem::HandleEnterScene(player, target));
    EXPECT_EQ(target, tlsEcs.actorRegistry.get<SceneEntityComp>(player).sceneEntity);
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<Hex>(player));
    const auto* interests = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(nullptr, interests);
    EXPECT_FALSE(interests->Contains(player));
    const auto* grids = tlsEcs.sceneRegistry.try_get<SceneGridListComp>(source);
    ASSERT_NE(nullptr, grids);
    for (const auto& [id, grid] : *grids) EXPECT_FALSE(grid.entities.contains(player));
    EXPECT_EQ(kExitPlayerId, SessionMap().at(kSessionAtExit));
}

TEST_F(SceneEntryPermissionTest, LeaveCurrentSceneClearsBindingHexAndWatchers)
{
    const auto sessionsBefore = SessionMap();
    const auto expectLeft = [this]
    {
        EXPECT_FALSE((tlsEcs.actorRegistry.any_of<SceneEntityComp, Hex>(player)));
        const auto* interests = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
        ASSERT_NE(nullptr, interests);
        EXPECT_FALSE(interests->Contains(player));
        const auto* grids = tlsEcs.sceneRegistry.try_get<SceneGridListComp>(source);
        ASSERT_NE(nullptr, grids);
        for (const auto& [id, grid] : *grids) EXPECT_FALSE(grid.entities.contains(player));
    };

    PlayerSceneSystem::LeaveCurrentScene(player);
    ASSERT_NO_FATAL_FAILURE(expectLeft());
    const auto watcherCount = tlsEcs.actorRegistry.get<AoiListComp>(watcher).entries.size();
    const auto gridCount = tlsEcs.sceneRegistry.get<SceneGridListComp>(source).size();

    PlayerSceneSystem::LeaveCurrentScene(player);
    ASSERT_NO_FATAL_FAILURE(expectLeft());
    EXPECT_EQ(watcherCount, tlsEcs.actorRegistry.get<AoiListComp>(watcher).entries.size());
    EXPECT_EQ(gridCount, tlsEcs.sceneRegistry.get<SceneGridListComp>(source).size());
    EXPECT_TRUE(SessionMap() == sessionsBefore);
    EXPECT_EQ(player, tlsEcs.GetPlayer(kExitPlayerId));
}

TEST_F(SceneEntryPermissionTest, NonCreatorJoinsMirrorById)
{
    auto& info = tlsEcs.sceneRegistry.get<SceneInfoComp>(target);
    info.set_mirror_config_id(1);
    (*info.mutable_creators())[kExitPlayerId + 99] = true;
    EXPECT_EQ(kSuccess, PlayerSceneSystem::CheckEnterScene(player, target));
    ASSERT_TRUE(PlayerSceneSystem::HandleEnterScene(player, target));
    EXPECT_EQ(target, tlsEcs.actorRegistry.get<SceneEntityComp>(player).sceneEntity);
}

TEST_F(SceneEntryPermissionTest, EnterGameNodeAdmitsTeammateIntoLeaderMirror)
{
    auto& info = tlsEcs.sceneRegistry.get<SceneInfoComp>(target);
    info.set_mirror_config_id(1);
    (*info.mutable_creators())[kExitPlayerId + 99] = true;
    PlayerEnterContext context;
    context.enterInfo.set_scene_id(kRelocateSourceScene + 1);
    context.enterInfo.set_session_id(kSessionAtExit);
    context.enterInfo.set_enter_gs_type(LOGIN_RECONNECT);
    SetFixtureAdmission(context);
    tlsEcs.actorRegistry.emplace<PlayerMirrorCreateInFlightComp>(player).token = 101;

    EXPECT_EQ(PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting,
              PlayerLifecycleSystem::HandleEnterGameNode(kExitPlayerId, context));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerMirrorCreateInFlightComp>(player))
        << "权威落点成功永久取代旧镜像意图";

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    ASSERT_TRUE(tlsEcs.actorRegistry.all_of<SceneEntityComp>(player));
    EXPECT_EQ(target, tlsEcs.actorRegistry.get<SceneEntityComp>(player).sceneEntity);
    ASSERT_TRUE(SessionMap().contains(kSessionAtExit));
    EXPECT_EQ(kExitPlayerId, SessionMap().at(kSessionAtExit));
}

TEST_F(SceneEntryPermissionTest, MissingAndZeroGuidRejectedWithoutMutation)
{
    const auto sessionsBefore = SessionMap();
    const auto snapshotBefore = tlsEcs.actorRegistry.get<PlayerSessionSnapshotComp>(player).SerializeAsString();
    for (const bool hasZeroGuid : {false, true})
    {
        SCOPED_TRACE(hasZeroGuid);
        tlsEcs.actorRegistry.remove<PlayerGuidComp>(player);
        if (hasZeroGuid)
            tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{0});
        EXPECT_EQ(kInvalidParameter, PlayerSceneSystem::CheckEnterScene(player, target));
        EXPECT_FALSE(PlayerSceneSystem::HandleEnterScene(player, target));
        ASSERT_NO_FATAL_FAILURE(ExpectStillInSource());
        EXPECT_TRUE(SessionMap() == sessionsBefore);
        EXPECT_EQ(snapshotBefore, tlsEcs.actorRegistry.get<PlayerSessionSnapshotComp>(player).SerializeAsString());
    }
}

TEST_F(SceneEntryPermissionTest, LifecycleRejectsBeforeSessionOwnershipAndExitSideEffects)
{
    tlsEcs.actorRegistry.emplace<PlayerHomeZoneComp>(player).homeZoneId = 3;
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
    tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
    tlsEcs.actorRegistry.emplace<PlayerExitIntentComp>(player).sessionAtExit = kSessionAtExit;
    tlsEcs.actorRegistry.emplace<PlayerMirrorCreateInFlightComp>(player).token = 102;
    PlayerEnterContext context;
    context.enterInfo.set_scene_id(kRelocateSourceScene + 2);
    context.enterInfo.set_session_id(kNewerSession);
    context.enterInfo.set_enter_gs_type(LOGIN_RECONNECT);
    context.loginGeneration = 10;
    SetFixtureAdmission(context);
    context.homeZoneId = 4;
    context.ownerEpoch = 6;
    const auto sessionsBefore = SessionMap();
    const auto snapshotBefore = tlsEcs.actorRegistry.get<PlayerSessionSnapshotComp>(player).SerializeAsString();

    EXPECT_EQ(kEnterSceneNotFound, PlayerSceneSystem::CheckEnterScene(
                  player, PlayerSceneSystem::FindSceneById(context.enterInfo.scene_id())));
    EXPECT_FALSE(PlayerLifecycleSystem::EnterScene(player, context));
    EXPECT_EQ(PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected,
              PlayerLifecycleSystem::HandleEnterGameNode(kExitPlayerId, context));

    ASSERT_NO_FATAL_FAILURE(ExpectStillInSource());
    EXPECT_TRUE(SessionMap() == sessionsBefore);
    EXPECT_EQ(snapshotBefore, tlsEcs.actorRegistry.get<PlayerSessionSnapshotComp>(player).SerializeAsString());
    EXPECT_EQ(3u, tlsEcs.actorRegistry.get<PlayerHomeZoneComp>(player).homeZoneId);
    EXPECT_EQ(5u, tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch);
    EXPECT_TRUE((tlsEcs.actorRegistry.all_of<UnregisterPlayer, PlayerExitIntentComp>(player)));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<PlayerLoginIdentityComp, PlayerEnterGameStateComp>(player)));
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player).token, 102u)
        << "权威落点预检被拒,不能提前取消旧镜像";
}

namespace
{
class SceneLifecycleTest : public testing::Test
{
protected:
    struct OwnedSceneCleanup
    {
        std::vector<SceneEntity> scenes;
        ~OwnedSceneCleanup()
        {
            // StartDisabled 不清单元集合;只归还本例创建过的 key,断言提前返回也不会污染后例。
            for (const auto scene : scenes)
            {
                agones::GameServerLifecycle::Instance().OnUnitDestroyed(entt::to_integral(scene));
                if (tlsEcs.sceneRegistry.valid(scene)) tlsEcs.sceneRegistry.destroy(scene);
            }
        }
    };

    void SetUp() override
    {
        tlsEcs.Clear();
        PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, 0);
        auto& lifecycle = agones::GameServerLifecycle::Instance();
        lifecycle.StartDisabled();
        unitsBefore = lifecycle.UnitCount();
    }
    void TearDown() override
    {
        PlayerLifecycleSystem::ReconcileRelocateOnReentry(kExitPlayerId, 0);
    }
    CreateSceneRequest Request() const
    {
        CreateSceneRequest request;
        request.set_config_id(1);
        request.set_scene_id(4242);
        request.set_mirror_config_id(2);
        request.set_dungeon_config_id(3);
        request.add_creator_ids(kExitPlayerId);
        request.add_creator_ids(kExitPlayerId + 1);
        return request;
    }
    void CreateAtEntry(const CreateSceneRequest& request, CreateSceneResponse& response)
    {
        // 组合正式入口的按号幂等契约,场景域只负责未命中后的真实创建。
        if (const auto existing = PlayerSceneSystem::FindSceneById(request.scene_id()); existing != entt::null)
        {
            response.mutable_scene_info()->CopyFrom(tlsEcs.sceneRegistry.get<SceneInfoComp>(existing));
            return;
        }
        response.mutable_scene_info()->CopyFrom(SceneLifecycle::CreateScene(request));
        ownedScenes.scenes.push_back(PlayerSceneSystem::FindSceneById(request.scene_id()));
    }

    ScopedPersistedPlayerCleanup ecsCleanup;
    OwnedSceneCleanup ownedScenes;
    int unitsBefore{0};
};
} // namespace

TEST_F(SceneLifecycleTest, RepeatedSceneIdReturnsSameInfoAndCountsOneUnitWithoutEventSinks)
{
    auto request = Request();
    CreateSceneResponse first;
    CreateAtEntry(request, first);
    ASSERT_TRUE(first.has_scene_info());
    EXPECT_EQ(4242u, first.scene_info().scene_id());
    EXPECT_EQ(1u, first.scene_info().scene_config_id());
    EXPECT_EQ(2u, first.scene_info().mirror_config_id());
    EXPECT_EQ(3u, first.scene_info().dungeon_config_id());
    EXPECT_EQ(2, first.scene_info().creators_size());
    EXPECT_TRUE(first.scene_info().creators().contains(kExitPlayerId));
    EXPECT_TRUE(first.scene_info().creators().contains(kExitPlayerId + 1));

    request.set_config_id(9);
    request.clear_creator_ids();
    CreateSceneResponse second;
    CreateAtEntry(request, second);

    EXPECT_TRUE(google::protobuf::util::MessageDifferencer::Equals(first, second));
    EXPECT_EQ(1u, tlsEcs.sceneRegistry.view<SceneInfoComp>().size());
    EXPECT_EQ(unitsBefore + 1, agones::GameServerLifecycle::Instance().UnitCount());
}

TEST_F(SceneLifecycleTest, ResidentDrainKeepsSceneAndUnitUntilResidentLeaves)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    tlsEcs.actorRegistry.get<PlayerExitIntentComp>(player).cause = ExitCause::kSceneDrain;
    CreateSceneResponse response;
    CreateAtEntry(Request(), response);
    const auto scene = PlayerSceneSystem::FindSceneById(4242);
    ASSERT_NE(SceneEntity{entt::null}, scene);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(player).sceneEntity = scene;

    EXPECT_EQ(1u, SceneLifecycle::DestroyScene(scene));

    EXPECT_TRUE(tlsEcs.sceneRegistry.valid(scene));
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(scene, tlsEcs.actorRegistry.get<SceneEntityComp>(player).sceneEntity);
    EXPECT_EQ(scene, PlayerSceneSystem::FindSceneById(4242));
    EXPECT_EQ(unitsBefore + 1, agones::GameServerLifecycle::Instance().UnitCount());
}

TEST_F(SceneLifecycleTest, DisconnectedExitingResidentKeepsSceneUntilBindingIsRemoved)
{
    // 已断线的退出中玩家不会登记改派票据,但残留绑定仍须阻止场景销毁。
    const auto player = MakeExitingPlayer(kSessionAtExit, kInvalidSessionId);
    SessionMap().clear();
    CreateSceneResponse response;
    CreateAtEntry(Request(), response);
    const auto scene = PlayerSceneSystem::FindSceneById(4242);
    ASSERT_NE(SceneEntity{entt::null}, scene);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(player).sceneEntity = scene;

    EXPECT_EQ(1u, SceneLifecycle::DestroyScene(scene));

    ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(scene, tlsEcs.actorRegistry.get<SceneEntityComp>(player).sceneEntity);
    EXPECT_EQ(scene, PlayerSceneSystem::FindSceneById(4242));
    EXPECT_EQ(unitsBefore + 1, agones::GameServerLifecycle::Instance().UnitCount());

    PlayerSceneSystem::LeaveCurrentScene(player);
    EXPECT_EQ(0u, SceneLifecycle::DestroyScene(scene));
    EXPECT_FALSE(tlsEcs.sceneRegistry.valid(scene));
    EXPECT_EQ(SceneEntity{entt::null}, PlayerSceneSystem::FindSceneById(4242));
    EXPECT_EQ(unitsBefore, agones::GameServerLifecycle::Instance().UnitCount());
}

TEST_F(SceneLifecycleTest, MissingAndInvalidGuidBindingsKeepSceneUntilBothAreRemoved)
{
    CreateSceneResponse response;
    CreateAtEntry(Request(), response);
    const auto scene = PlayerSceneSystem::FindSceneById(4242);
    ASSERT_NE(SceneEntity{entt::null}, scene);
    auto& registry = tlsEcs.actorRegistry;
    const auto missingGuid = registry.create();
    registry.emplace<Player>(missingGuid);
    registry.emplace<SceneEntityComp>(missingGuid, SceneEntityComp{scene});
    const auto invalidGuid = registry.create();
    registry.emplace<Player>(invalidGuid);
    registry.emplace<PlayerGuidComp>(invalidGuid, kInvalidGuid);
    registry.emplace<SceneEntityComp>(invalidGuid, SceneEntityComp{scene});

    // 身份不完整会阻止改派,不能据此推断场景已经没有绑定者。
    EXPECT_EQ(2u, SceneLifecycle::DestroyScene(scene));
    ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
    ASSERT_TRUE(registry.all_of<SceneEntityComp>(missingGuid));
    ASSERT_TRUE(registry.all_of<SceneEntityComp>(invalidGuid));
    EXPECT_EQ(scene, registry.get<SceneEntityComp>(missingGuid).sceneEntity);
    EXPECT_EQ(scene, registry.get<SceneEntityComp>(invalidGuid).sceneEntity);
    EXPECT_EQ(unitsBefore + 1, agones::GameServerLifecycle::Instance().UnitCount());

    PlayerSceneSystem::LeaveCurrentScene(missingGuid);
    EXPECT_EQ(1u, SceneLifecycle::DestroyScene(scene));
    ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
    EXPECT_EQ(scene, PlayerSceneSystem::FindSceneById(4242));
    EXPECT_EQ(unitsBefore + 1, agones::GameServerLifecycle::Instance().UnitCount());

    PlayerSceneSystem::LeaveCurrentScene(invalidGuid);
    EXPECT_EQ(0u, SceneLifecycle::DestroyScene(scene));
    EXPECT_FALSE(tlsEcs.sceneRegistry.valid(scene));
    EXPECT_EQ(SceneEntity{entt::null}, PlayerSceneSystem::FindSceneById(4242));
    EXPECT_EQ(unitsBefore, agones::GameServerLifecycle::Instance().UnitCount());
}

TEST_F(SceneLifecycleTest, DestroyedSceneHandleCannotDestroyReusedSlotOrChangeUnitCount)
{
    CreateSceneResponse first;
    CreateAtEntry(Request(), first);
    const auto destroyed = PlayerSceneSystem::FindSceneById(4242);
    ASSERT_NE(SceneEntity{entt::null}, destroyed);
    ASSERT_EQ(0u, SceneLifecycle::DestroyScene(destroyed));
    ASSERT_FALSE(tlsEcs.sceneRegistry.valid(destroyed));
    EXPECT_EQ(unitsBefore, agones::GameServerLifecycle::Instance().UnitCount());

    auto request = Request();
    request.set_scene_id(4243);
    CreateSceneResponse second;
    CreateAtEntry(request, second);
    const auto replacement = PlayerSceneSystem::FindSceneById(4243);
    ASSERT_NE(SceneEntity{entt::null}, replacement);
    // 复用实体槽位但版本不同:旧句柄不能越过版本围栏删除新场景。
    ASSERT_EQ(entt::to_entity(destroyed), entt::to_entity(replacement));
    ASSERT_NE(destroyed, replacement);

    EXPECT_EQ(0u, SceneLifecycle::DestroyScene(destroyed));
    EXPECT_FALSE(tlsEcs.sceneRegistry.valid(destroyed));
    ASSERT_TRUE(tlsEcs.sceneRegistry.valid(replacement));
    EXPECT_EQ(replacement, PlayerSceneSystem::FindSceneById(4243));
    EXPECT_EQ(4243u, tlsEcs.sceneRegistry.get<SceneInfoComp>(replacement).scene_id());
    EXPECT_EQ(unitsBefore + 1, agones::GameServerLifecycle::Instance().UnitCount());

    EXPECT_EQ(0u, SceneLifecycle::DestroyScene(replacement));
    EXPECT_FALSE(tlsEcs.sceneRegistry.valid(replacement));
    EXPECT_EQ(unitsBefore, agones::GameServerLifecycle::Instance().UnitCount());
}

TEST_F(SceneLifecycleTest, EmptySceneDestroyRemovesLookupAndRestoresUnitCount)
{
    CreateSceneResponse response;
    CreateAtEntry(Request(), response);
    const auto scene = PlayerSceneSystem::FindSceneById(4242);
    ASSERT_NE(SceneEntity{entt::null}, scene);
    EXPECT_EQ(unitsBefore + 1, agones::GameServerLifecycle::Instance().UnitCount());

    EXPECT_EQ(0u, SceneLifecycle::DestroyScene(scene));

    EXPECT_FALSE(tlsEcs.sceneRegistry.valid(scene));
    EXPECT_EQ(SceneEntity{entt::null}, PlayerSceneSystem::FindSceneById(4242));
    EXPECT_EQ(unitsBefore, agones::GameServerLifecycle::Instance().UnitCount());
}







TEST(EnterFenceEcs, StaleEnterAfterTakeoverIsRejected)
{
    // §1 的实体路径:本节点上是更早一次登录的实体;B 顶号的入场先到并绑定,A(已被踢)的入场后到。
    // 两次路由的 owner_epoch 都是 5 —— 只靠 epoch 区分不了。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeFencedPlayer(kFenceSessionOld, kGenOld);

    const uint64_t newerBefore = FenceCount(ef::Verdict::kAdmitNewer);
    ASSERT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenB)), PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting);
    EXPECT_EQ(FenceCount(ef::Verdict::kAdmitNewer), newerBefore + 1);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(SessionMap().count(kFenceSessionOld), 0u) << "顶号擦掉了上一次登录的映射(属于本玩家)";

    const uint64_t staleBefore = FenceCount(ef::Verdict::kRejectStale);
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenA)), PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected);

    EXPECT_EQ(FenceCount(ef::Verdict::kRejectStale), staleBefore + 1);
    // 映射、快照、身份仍指向 B。
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u)
        << "没有 sA 映射:gateA 紧随其后代发的 ExitGame(sA) 在 \"session id not found\" 就结束,碰不到 B 的实体";
    EXPECT_EQ(PendingEnter(), nullptr) << "被拒的请求不登记待入场上下文";
    EXPECT_EQ(PlayerLifecycleSystem::GetPendingEnterMap().count(kExitPlayerId), 0u);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, StaleEnterDuringLoadKeepsPending)
{
    // §1 的载入中变体:更早一次登录的载入在途,B 覆盖了待入场上下文,A 的旧入场才到。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    ASSERT_EQ(EnterGameNode(FenceEnter(kFenceSessionOld, kGenOld, LOGIN_FIRST)),
              PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    ASSERT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenB, LOGIN_REPLACE)),
              PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    const PlayerEnterContext *pending = PendingEnter();
    ASSERT_NE(pending, nullptr);
    const uint64_t lifecycle = pending->inheritClear.lifecycle;
    EXPECT_NE(lifecycle, 0u);
    EXPECT_EQ(SessionMap().count(kFenceSessionOld), 0u) << "覆盖时擦掉上一次载入的预登记(属于本玩家)";

    const uint64_t staleBefore = FenceCount(ef::Verdict::kRejectStale);
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenA, LOGIN_REPLACE)),
              PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected);

    EXPECT_EQ(FenceCount(ef::Verdict::kRejectStale), staleBefore + 1);
    pending = PendingEnter();
    ASSERT_NE(pending, nullptr) << "待入场上下文仍在";
    EXPECT_EQ(pending->enterInfo.session_id(), kFenceSessionB);
    EXPECT_EQ(pending->loginGeneration, kGenB);
    EXPECT_EQ(pending->enterInfo.enter_gs_type(), static_cast<uint32_t>(LOGIN_REPLACE));
    EXPECT_EQ(pending->inheritClear.lifecycle, lifecycle) << "仍是同一次载入";
    ASSERT_EQ(SessionMap().count(kFenceSessionB), 1u) << "B 的预登记还在";
    EXPECT_EQ(SessionMap().at(kFenceSessionB), kExitPlayerId);
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, LoadCallbackBindsNewestAdmitted)
{
    // A 的载入在途时 B 覆盖了待入场上下文;载入回调(A 那一次发起的同一个载入)建出的实体绑的是 B。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    ASSERT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenA, LOGIN_FIRST)),
              PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    ASSERT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenB, LOGIN_REPLACE)),
              PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);
    {
        // 宿主里 A2′ 发不出去;手工把"针对本 owner_epoch 的清理已确认"写进去,让闸门放行(kProceed)。
        auto &pending = PlayerLifecycleSystem::GetPendingEnterMap();
        ASSERT_EQ(pending.count(kExitPlayerId), 1u);
        auto &inheritClear = pending.at(kExitPlayerId).inheritClear;
        ASSERT_EQ(inheritClear.targetEpoch, kFenceOwnerEpoch);
        inheritClear.phase = erm::InheritClearPhase::kConfirmed;
    }

    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kExitPlayerId, IdentifiedPlayerLoadData(kExitPlayerId));

    EXPECT_EQ(PendingEnter(), nullptr) << "待入场上下文已消费";
    ASSERT_EQ(tlsEcs.playerList.count(kExitPlayerId), 1u) << "建出了实体";
    const auto player = tlsEcs.playerList.at(kExitPlayerId);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);

    // 实体建出来之后,A 的旧入场再到:走实体路径同样被拒。
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenA)), PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, StaleEnterDoesNotCancelExit)
{
    // §1 的退出存盘在途变体:B 正在退出(UnregisterPlayer + 退出意图),A 的旧入场到达 —— 不得取消 B 的退出。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeExitingPlayer(kFenceSessionB, kFenceSessionB);
    auto &identity = tlsEcs.actorRegistry.emplace<PlayerLoginIdentityComp>(player);
    identity.sessionId = kFenceSessionB;
    identity.loginGeneration = kGenB;

    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenA)), PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player)) << "退出没有被取消";
    const auto *intent = tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(player);
    ASSERT_NE(intent, nullptr) << "与 UnregisterPlayer 成对留着";
    EXPECT_EQ(intent->sessionAtExit, kFenceSessionB);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, NewerSessionTakesOver)
{
    // 正常顶号:代次更大的异会话入场接管实体,旧会话的映射被擦掉。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeFencedPlayer(kFenceSessionA, kGenA);
    const uint64_t newerBefore = FenceCount(ef::Verdict::kAdmitNewer);

    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenB)), PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting);

    EXPECT_EQ(FenceCount(ef::Verdict::kAdmitNewer), newerBefore + 1);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, DuplicateEnterIsIdempotent)
{
    // gate 补发同一条路由(同一会话、同一代次):两次都放行,登录态与绑定不变。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeFencedPlayer(kFenceSessionB, kGenB);
    const auto scene = PlaceInScene(player, kFenceScene);
    const uint64_t sameBefore = FenceCount(ef::Verdict::kAdmitSameSession);

    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenB, LOGIN_REPLACE, kFenceScene)),
              PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting);
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenB, LOGIN_REPLACE, kFenceScene)),
              PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting);

    EXPECT_EQ(FenceCount(ef::Verdict::kAdmitSameSession), sameBefore + 2);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    const auto *enterState = tlsEcs.actorRegistry.try_get<PlayerEnterGameStateComp>(player);
    ASSERT_NE(enterState, nullptr);
    EXPECT_EQ(enterState->enter_gs_type(), static_cast<uint32_t>(LOGIN_REPLACE));
    const auto *sceneComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
    ASSERT_NE(sceneComp, nullptr);
    EXPECT_EQ(sceneComp->sceneEntity, scene) << "已在目标场景:HandleEnterScene 幂等早退";

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, PendingSameSessionKeepsLoginType)
{
    // 同一会话在载入完成前先后到达"登录类型"与"0 型换图路由":合并而不是整体替换(§5.5)。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    PlayerEnterContext login = FenceEnter(kFenceSessionA, kGenA, LOGIN_FIRST);
    login.homeZoneId = 1;
    ASSERT_EQ(EnterGameNode(login), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    ASSERT_NE(PendingEnter(), nullptr);
    const uint64_t lifecycle = PendingEnter()->inheritClear.lifecycle;

    PlayerEnterContext sceneChange = FenceEnter(kFenceSessionA, kGenA, LOGIN_NONE, kFenceScene);
    sceneChange.homeZoneId = 2;
    sceneChange.ownerEpoch = kFenceOwnerEpoch + 1;
    EXPECT_EQ(EnterGameNode(sceneChange), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);

    const PlayerEnterContext *pending = PendingEnter();
    ASSERT_NE(pending, nullptr);
    EXPECT_EQ(pending->enterInfo.enter_gs_type(), static_cast<uint32_t>(LOGIN_FIRST))
        << "新值为 0 时保留旧的登录类型,否则载入后不置登录态";
    EXPECT_EQ(pending->enterInfo.session_id(), kFenceSessionA);
    EXPECT_EQ(pending->enterInfo.scene_id(), kFenceScene) << "scene_id 取新值";
    EXPECT_EQ(pending->homeZoneId, 2u) << "home_zone 取新值";
    EXPECT_EQ(pending->ownerEpoch, kFenceOwnerEpoch + 1) << "owner_epoch 取新值";
    EXPECT_EQ(pending->loginGeneration, kGenA);
    EXPECT_EQ(pending->inheritClear.lifecycle, lifecycle) << "A2′ 状态原样沿用,载入仍是同一个";
    ASSERT_EQ(SessionMap().count(kFenceSessionA), 1u) << "同一会话不擦自己的预登记";
    EXPECT_EQ(SessionMap().at(kFenceSessionA), kExitPlayerId);

    // 新值非 0 时取新值。
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenA, LOGIN_RECONNECT)),
              PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    ASSERT_NE(PendingEnter(), nullptr);
    EXPECT_EQ(PendingEnter()->enterInfo.enter_gs_type(), static_cast<uint32_t>(LOGIN_RECONNECT));

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, EqualGenerationDifferentSessionRejected)
{
    // 代次相等、会话不同:违反"一登一号",只可能是并发冲突 —— 安全失败,两边都不动。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeFencedPlayer(kFenceSessionB, kGenB);
    const uint64_t conflictBefore = FenceCount(ef::Verdict::kRejectConflict);

    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenB)), PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected);

    EXPECT_EQ(FenceCount(ef::Verdict::kRejectConflict), conflictBefore + 1);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);

    // 载入中同样:待入场上下文这一处一样拦。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    ASSERT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenB)), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenB)), PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected);
    ASSERT_NE(PendingEnter(), nullptr);
    EXPECT_EQ(PendingEnter()->enterInfo.session_id(), kFenceSessionB);
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);
    EXPECT_EQ(SessionMap().count(kFenceSessionB), 1u);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, UnversionedCannotDisplaceVersioned)
{
    // 当前有代次、请求没有(旧版 login 顶号 / 混版窗口)、会话不同:拒,不留默认放行的窗口(§6)。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeFencedPlayer(kFenceSessionB, kGenB);
    const uint64_t unversionedBefore = FenceCount(ef::Verdict::kRejectUnversioned);

    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, 0)), PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected);

    EXPECT_EQ(FenceCount(ef::Verdict::kRejectUnversioned), unversionedBefore + 1);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, LegacyBothUnversionedKeepsOldBehavior)
{
    // 双方都没有代次(上游还没铸造):沿用旧行为 —— 后到者胜,并计 admit_legacy。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeFencedPlayer(kFenceSessionA, 0); // 旧版实体:没有身份组件
    const uint64_t legacyBefore = FenceCount(ef::Verdict::kAdmitLegacy);

    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, 0)), PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, 0));
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);

    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, 0)), PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting)
        << "旧行为:晚到的那一条照样接管";
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionA, 0));
    EXPECT_EQ(SessionMap().count(kFenceSessionB), 0u);
    EXPECT_EQ(FenceCount(ef::Verdict::kAdmitLegacy), legacyBefore + 2);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, SameSessionLowerGenerationDoesNotLower)
{
    // 同一会话一律放行,但已放行的最高代次不得被拉低(实体与待入场两处)。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeFencedPlayer(kFenceSessionB, kGenB);

    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenA)), PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, 0)), PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenA)), PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected)
        << "代次没被拉低:更早的异会话入场仍被拒";

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    ASSERT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, kGenA, LOGIN_FIRST)),
              PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionA, 0, LOGIN_NONE)), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    ASSERT_NE(PendingEnter(), nullptr);
    EXPECT_EQ(PendingEnter()->loginGeneration, kGenA) << "待入场同一会话合并:代次取 max";
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionOld, kGenOld)), PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, RemovePlayerSessionOnlyErasesOwnMapping)
{
    // gate 以同一 node_id 重启后会话序号从头发:玩家 Q 拿到了本玩家 P 旧会话的同号会话、映射已改成 Q。
    // P 的任何擦除都不得擦掉 Q 的在线映射(§5.6)。
    constexpr SessionId kShared = kFenceSessionA;

    // ① RemovePlayerSession(实体):快照里的旧号已属于 Q。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    auto player = MakeFencedPlayer(kShared, kGenA);
    SessionMap()[kShared] = kFenceOtherPlayerId;
    PlayerLifecycleSystem::RemovePlayerSession(player);
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerSessionSnapshotComp>(player).gate_session_id(), kInvalidSessionId)
        << "自己的快照照样摘";
    ASSERT_EQ(SessionMap().count(kShared), 1u);
    EXPECT_EQ(SessionMap().at(kShared), kFenceOtherPlayerId) << "Q 的映射还在";

    // ② 判据本身。
    EXPECT_FALSE(PlayerLifecycleSystem::EraseSessionMappingIfOwned(kShared, kExitPlayerId));
    EXPECT_FALSE(PlayerLifecycleSystem::EraseSessionMappingIfOwned(kFenceSessionB, kExitPlayerId)) << "没有映射";
    EXPECT_TRUE(PlayerLifecycleSystem::EraseSessionMappingIfOwned(kShared, kFenceOtherPlayerId));
    EXPECT_EQ(SessionMap().count(kShared), 0u);

    // ③ P 经新会话重登通过栅栏:入口第 2 步摘旧会话时不擦 Q。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    player = MakeFencedPlayer(kShared, kGenA);
    SessionMap()[kShared] = kFenceOtherPlayerId;
    EXPECT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenB)), PlayerLifecycleSystem::EnterGameNodeOutcome::kEnteredExisting);
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    ASSERT_EQ(SessionMap().count(kShared), 1u);
    EXPECT_EQ(SessionMap().at(kShared), kFenceOtherPlayerId);

    // ④ EnterScene 第 1 步换会话时(快照里还挂着旧号)同样只擦自己的。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    player = MakeFencedPlayer(kShared, kGenA);
    SessionMap()[kShared] = kFenceOtherPlayerId;
    EXPECT_TRUE(PlayerLifecycleSystem::EnterScene(player, FenceEnter(kFenceSessionB, kGenB)));
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(SessionMap().at(kShared), kFenceOtherPlayerId);

    // ⑤ 载入在途时覆盖待入场上下文:上一次的预登记号已属于 Q,不擦。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    ASSERT_EQ(EnterGameNode(FenceEnter(kShared, kGenA)), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    SessionMap()[kShared] = kFenceOtherPlayerId;
    ASSERT_EQ(EnterGameNode(FenceEnter(kFenceSessionB, kGenB)), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    EXPECT_EQ(SessionMap().at(kShared), kFenceOtherPlayerId);
    EXPECT_EQ(SessionMap().at(kFenceSessionB), kExitPlayerId);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, AbandonedLoadOnlyErasesOwnMapping)
{
    // 载入放弃的路径(RedisError、A2′ 重发耗尽 → 拒建实体)与载入回调的"会话已取消"判据,同样只认属于自己的映射。
    using LoadFailureReason = MessageAsyncClient<Guid, PlayerAllData>::LoadFailureReason;
    constexpr SessionId kShared = kFenceSessionA;

    // ① RedisError。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    ASSERT_EQ(EnterGameNode(FenceEnter(kShared, kGenA)), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    SessionMap()[kShared] = kFenceOtherPlayerId;
    PlayerLifecycleSystem::HandlePlayerAsyncLoadFailed(kExitPlayerId, LoadFailureReason::RedisError);
    EXPECT_EQ(PendingEnter(), nullptr);
    EXPECT_EQ(SessionMap().at(kShared), kFenceOtherPlayerId);

    // ② A2′ 重发耗尽(RefuseInheritedEntry;截止拒绝走的是同一个函数)。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    ASSERT_EQ(EnterGameNode(FenceEnter(kShared, kGenA)), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    SessionMap()[kShared] = kFenceOtherPlayerId;
    for (uint32_t i = 1; i < erm::kInheritClearMaxAttempts; ++i)
    {
        PlayerLifecycleSystem::RetryInheritedMarkClears(/*reconnected=*/true);
    }
    EXPECT_EQ(PendingEnter(), nullptr) << "重发耗尽:拒建实体";
    EXPECT_EQ(SessionMap().at(kShared), kFenceOtherPlayerId);

    // ③ 载入回调:预登记号已被 Q 占用 = 这次载入作废(§5.4),不建实体、不碰 Q 的映射。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    ASSERT_EQ(EnterGameNode(FenceEnter(kShared, kGenA)), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    PlayerLifecycleSystem::GetPendingEnterMap().at(kExitPlayerId).inheritClear.phase = erm::InheritClearPhase::kConfirmed;
    SessionMap()[kShared] = kFenceOtherPlayerId;
    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kExitPlayerId, PlayerAllData{});
    EXPECT_EQ(PendingEnter(), nullptr);
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u) << "不建实体";
    EXPECT_EQ(SessionMap().at(kShared), kFenceOtherPlayerId);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, BindRecheckRejectsStaleContext)
{
    // EnterScene 的绑定时复核(§5.4):绕过入口直接拿陈旧上下文调 EnterScene,同样什么都不碰 —— 排在取消退出之前。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeExitingPlayer(kFenceSessionB, kFenceSessionB);
    auto &identity = tlsEcs.actorRegistry.emplace<PlayerLoginIdentityComp>(player);
    identity.sessionId = kFenceSessionB;
    identity.loginGeneration = kGenB;
    const auto before = enter_fence_stats::Read();

    EXPECT_FALSE(PlayerLifecycleSystem::EnterScene(player, FenceEnter(kFenceSessionA, kGenA)));

    const auto after = enter_fence_stats::Read();
    EXPECT_EQ(after.bindRejected, before.bindRejected + 1);
    EXPECT_TRUE(after.verdicts == before.verdicts) << "入口计数只由 HandleEnterGameNode 记";
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player)) << "没有取消退出";
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerExitIntentComp>(player));
    ASSERT_NO_FATAL_FAILURE(ExpectBoundTo(player, kFenceSessionB, kGenB));
    EXPECT_EQ(SessionMap().count(kFenceSessionA), 0u);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

TEST(EnterFenceEcs, StaleEnterDoesNotSupersedeRelocateEntry)
{
    // §5.1 第 3 处:排空把他改派出去、实体已销毁,本节点上只剩改派待确认条目记着他。陈旧的异会话入场不得把条目
    // 误结成 superseded(那会让本该核实后踢线的玩家不被踢);更新的入场照常接管并结清条目。
    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = kFenceOwnerEpoch;
    auto &identity = tlsEcs.actorRegistry.emplace<PlayerLoginIdentityComp>(player);
    identity.sessionId = kSessionAtExit;
    identity.loginGeneration = kGenA;
    const auto scene = PlaceInScene(player, kRelocateSourceScene);
    ASSERT_TRUE(MarkPersisted(player)); // 最后做:快照要与退出时再 marshal 出来的那一份逐字节相同
    ASSERT_EQ(PlayerLifecycleSystem::BeginSceneDrain(scene), 1u);
    ASSERT_FALSE(tlsEcs.actorRegistry.valid(player)) << "退出同步收敛,实体已销毁";
    const auto entry = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(entry.has_value());
    EXPECT_EQ(entry->sessionId, kSessionAtExit);
    EXPECT_EQ(entry->loginGeneration, kGenA) << "登记时从实体的登录身份抄下代次";

    // 只读判定不改任何状态、不计数。
    const auto before = enter_fence_stats::Read();
    EXPECT_EQ(PlayerLifecycleSystem::JudgeEnter(kExitPlayerId, kNewerSession, kGenOld), ef::Verdict::kRejectStale);
    EXPECT_TRUE(enter_fence_stats::Read() == before);

    EXPECT_EQ(EnterGameNode(FenceEnter(kNewerSession, kGenOld)), PlayerLifecycleSystem::EnterGameNodeOutcome::kRejected);
    const auto kept = PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId);
    ASSERT_TRUE(kept.has_value()) << "条目没有被误结清";
    EXPECT_EQ(kept->seq, entry->seq);
    EXPECT_EQ(kept->phase, entry->phase);
    EXPECT_EQ(PendingEnter(), nullptr);
    EXPECT_EQ(SessionMap().count(kNewerSession), 0u);
    EXPECT_EQ(enter_fence_stats::Read().verdicts[static_cast<std::size_t>(ef::Verdict::kRejectStale)],
              before.verdicts[static_cast<std::size_t>(ef::Verdict::kRejectStale)] + 1);

    EXPECT_EQ(EnterGameNode(FenceEnter(kNewerSession, kGenB)), PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired);
    EXPECT_FALSE(PlayerLifecycleSystem::PeekRelocateConfirm(kExitPlayerId).has_value())
        << "更新的异会话入场:玩家已重登,条目按 superseded 结清";
    ASSERT_NE(PendingEnter(), nullptr);
    EXPECT_EQ(PendingEnter()->enterInfo.session_id(), kNewerSession);
    EXPECT_EQ(PendingEnter()->loginGeneration, kGenB);

    ASSERT_NO_FATAL_FAILURE(ResetFenceHost());
}

// ============================================================================
// 17. MirrorCreateInFlight —— 镜像 CreateScene 在途标记(PlayerMirrorCreateInFlightComp,
//     docs/design/client-request-ordering.md §7.2 第 7 条)。
//     缺陷形状:EnterSceneC2S 的镜像分支发出 CreateScene 后不登记任何在途记录,IsSceneChangeBusy 看不见它,
//     建镜像这一个往返里客户端换图 / 跨 zone 传送 / 队伍跟随都被放行,随后镜像的自动进场再把玩家搬一次。
//     另一半:CreateScene 的业务失败应答不回显 creator_ids,旧的全局处理器找不到创建者,客户端收不到任何提示。
//     这里直接驱动 RequestEnterMirrorScene 的闸与 HandleMirrorCreateDone(每次调用回调的落点),不发真实 RPC。
// ============================================================================
#include "core/network/grpc_client_call.h"                    // GrpcCallOutcome
#include "player/system/player_tip.h"                     // 校准迟到完成不得推提示的日志观察器
#include "player/system/db_task_dispatch_system.h"          // 复用账本测试钩子,让真实退出等回执时保留实体
#include "proto/common/component/battle_comp.pb.h"       // InBattleComp

namespace
{
// 与 RequestEnterMirrorScene 登记的形状一致:令牌非 0,发送时刻 = 现在。
void AttachMirrorCreate(entt::entity player, uint64_t token)
{
    auto& inFlight = tlsEcs.actorRegistry.emplace<PlayerMirrorCreateInFlightComp>(player);
    inFlight.token = token;
    inFlight.sentAtMs = TimeSystem::NowMillisecondsUTC();
    inFlight.mirrorConfigId = 3;
    inFlight.sourceSceneId = 1001;
}

GrpcCallOutcome OkOutcome()
{
    return GrpcCallOutcome{};
}

GrpcCallOutcome DeadlineExceededOutcome()
{
    GrpcCallOutcome outcome;
    outcome.code = 4; // grpc::StatusCode::DEADLINE_EXCEEDED
    outcome.message = "Deadline Exceeded";
    return outcome;
}

::scene_manager::CreateSceneResponse MakeCreateReply(uint32_t errorCode, uint64_t sceneId)
{
    ::scene_manager::CreateSceneResponse reply;
    reply.set_error_code(errorCode);
    reply.set_scene_id(sceneId);
    return reply;
}

bool HasMirrorCreate(entt::entity player)
{
    return tlsEcs.actorRegistry.any_of<PlayerMirrorCreateInFlightComp>(player);
}

// 此宿主没有 gate:推一次 tip 会在发送边界留下 Gate session not found 的 ERROR。
// 沿用 bag_test 的 PanelPushProbe 做法,每次先校准再清零,避免把没推与观察器失效混淆。
int gMirrorTipPushTraces = 0;

void CountMirrorTipPushTrace(const char* msg, int len)
{
    if (std::string_view(msg, static_cast<std::size_t>(len)).find("Gate session not found for player with session ID") !=
        std::string_view::npos)
    {
        ++gMirrorTipPushTraces;
    }
    WriteLogToStdout(msg, len);
}

class MirrorTipPushProbe
{
public:
    MirrorTipPushProbe()
    {
        gMirrorTipPushTraces = 0;
        muduo::Logger::setOutput(CountMirrorTipPushTrace);
    }
    ~MirrorTipPushProbe() { muduo::Logger::setOutput(WriteLogToStdout); }
    MirrorTipPushProbe(const MirrorTipPushProbe&) = delete;
    MirrorTipPushProbe& operator=(const MirrorTipPushProbe&) = delete;

    int Count() const { return gMirrorTipPushTraces; }
    void Reset() { gMirrorTipPushTraces = 0; }
};

void CheckLateMirrorCompletion(const GrpcCallOutcome& outcome, const ::scene_manager::CreateSceneResponse* reply)
{
    // 两种失效时钟 × 后来的普通换图仍在途/已经结束,分别校验同一过期完成。
    for (const bool clockWentBack : {false, true})
    {
        for (const bool newerChangeInFlight : {false, true})
        {
            SCOPED_TRACE(::testing::Message() << "clock_went_back=" << clockWentBack
                                             << " newer_change_in_flight=" << newerChangeInFlight);
            const auto player = MakeOnlinePlayer(kSessionAtExit);
            AttachMirrorCreate(player, /*token=*/71);
            const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
            const uint64_t deadlineMs =
                static_cast<uint64_t>(grpc_call_deadline::Get(eNodeType::SceneManagerNodeService).count());
            tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player).sentAtMs =
                clockWentBack ? nowMs + 60000 : nowMs - deadlineMs - 2000;
            ASSERT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "失效镜像不得挡下一次普通换图";

            constexpr uint64_t newerCorrelationId = 92;
            constexpr uint64_t newerSceneId = 9002;
            AttachSceneChange(player, newerSceneId, /*playerRequested=*/true, newerCorrelationId);
            const auto newerChange = tlsEcs.actorRegistry.get<PlayerSceneChangeInFlightComp>(player);
            if (!newerChangeInFlight)
            {
                tlsEcs.actorRegistry.remove<PlayerSceneChangeInFlightComp>(player);
            }

            MirrorTipPushProbe tipPush;
            PlayerTipSystem::SendToPlayer(player, kEnterSceneFailed, {});
            ASSERT_EQ(tipPush.Count(), 1) << "校准:宿主能观察到一次提示的发送尝试";
            tipPush.Reset();

            PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, 71, outcome, reply);

            EXPECT_FALSE(HasMirrorCreate(player)) << "匹配的旧令牌即使失效也须摘掉";
            EXPECT_EQ(tipPush.Count(), 0) << "旧成功/失败均不得给后来的换图补推提示";
            const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
            if (newerChangeInFlight)
            {
                ASSERT_NE(pending, nullptr);
                EXPECT_EQ(pending->correlationId, newerChange.correlationId);
                EXPECT_EQ(pending->sceneId, newerChange.sceneId);
                EXPECT_EQ(pending->sentAtMs, newerChange.sentAtMs);
                EXPECT_EQ(pending->playerRequested, newerChange.playerRequested);
            }
            else
            {
                EXPECT_EQ(pending, nullptr) << "后来的换图已结束,旧镜像成功也不得新建自动进场槽";
            }
        }
    }
}

void CheckMirrorCompletionAfterClockRecovery(const GrpcCallOutcome& outcome,
                                             const ::scene_manager::CreateSceneResponse* reply)
{
    for (const bool newerChangeInFlight : {false, true})
    {
        SCOPED_TRACE(newerChangeInFlight);
        const ScopedFakeSceneManagerNode sceneManager; // 提供 EventLoop,析构时排空本地失败回调。
        const auto player = MakeOnlinePlayer(kSessionAtExit);
        AttachMirrorCreate(player, /*token=*/81);
        auto oldMirror = tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player);
        tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player).sentAtMs =
            TimeSystem::NowMillisecondsUTC() + 60000;
        ASSERT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "回拨放行下一次普通换图";

        // 真实发送入口,但不给目标实体挂通道:只排队一个异步本地失败,不会发真实 RPC。
        auto& smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
        const auto emptySceneManager = smRegistry.create();
        ::scene_manager::EnterSceneRequest request;
        request.set_player_id(kExitPlayerId);
        request.set_session_id(kSessionAtExit);
        request.set_scene_id(9002);
        const uint64_t correlationId =
            PlayerLifecycleSystem::RequestSceneChange(player, emptySceneManager, request);
        smRegistry.destroy(emptySceneManager); // 回调只捕获身份值,不借用此实体。
        ASSERT_FALSE(HasMirrorCreate(player)) << "普通换图提交必须永久撤销旧镜像资格";
        const auto newerChange = tlsEcs.actorRegistry.get<PlayerSceneChangeInFlightComp>(player);
        ASSERT_EQ(newerChange.correlationId, correlationId);
        ASSERT_NE(correlationId, 0u);
        if (!newerChangeInFlight)
        {
            tlsEcs.actorRegistry.remove<PlayerSceneChangeInFlightComp>(player);
        }

        // 只恢复局部旧请求的时间,不把被取消的组件重新挂回实体。
        oldMirror.sentAtMs = TimeSystem::NowMillisecondsUTC();
        ASSERT_TRUE(PlayerLifecycleSystem::IsSceneChangeRequestWithinTtl(oldMirror.sentAtMs))
            << "墙钟恢复后旧时间又落在 TTL 内,资格仍必须已经永久取消";
        MirrorTipPushProbe tipPush;
        PlayerTipSystem::SendToPlayer(player, kEnterSceneFailed, {});
        ASSERT_EQ(tipPush.Count(), 1);
        tipPush.Reset();

        PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, oldMirror.token, outcome, reply);

        EXPECT_FALSE(HasMirrorCreate(player));
        EXPECT_EQ(tipPush.Count(), 0);
        const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
        if (newerChangeInFlight)
        {
            ASSERT_NE(pending, nullptr);
            EXPECT_EQ(pending->correlationId, correlationId);
            EXPECT_EQ(pending->sceneId, newerChange.sceneId);
            EXPECT_EQ(pending->sentAtMs, newerChange.sentAtMs);
        }
        else
        {
            EXPECT_EQ(pending, nullptr) << "新换图结束后旧镜像也不得重新自动进场";
        }
    }
}
} // namespace

TEST(MirrorCreateInFlightEcs, BlocksSceneChangesUntilTtl)
{
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player));

    AttachMirrorCreate(player, /*token=*/7);
    EXPECT_TRUE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "建镜像在途:别的换图 / 传送 / 跟随都要被挡";

    // TTL 与普通换图同一取值:deadline 刚到时完成回调可能还没到,槽位必须还在;TTL 只兜"完成通知永远不来"。
    const uint64_t deadlineMs =
        static_cast<uint64_t>(grpc_call_deadline::Get(eNodeType::SceneManagerNodeService).count());
    auto& inFlight = tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player);
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
    inFlight.sentAtMs = nowMs - deadlineMs;
    EXPECT_TRUE(PlayerLifecycleSystem::IsSceneChangeBusy(player));
    inFlight.sentAtMs = nowMs - deadlineMs - 2000;
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "过了 deadline + 余量放下一条请求";
}

TEST(MirrorCreateInFlightEcs, ZoneTravelIsRefusedWhileMirrorCreateIsInFlight)
{
    // 修复前这里会越过闸进 StartTravelHandoff(宿主没有 scene_manager,回 kZoneTravelTargetBusy)。
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachMirrorCreate(player, /*token=*/51);

    EXPECT_EQ(PlayerLifecycleSystem::RequestZoneTravel(player, kReplyTargetZone, /*sceneConfigId=*/0),
              static_cast<uint32_t>(kEnterSceneChangingScene));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp, PlayerFrozenComp>(player)));
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player).token, 51u);
}

TEST(MirrorCreateInFlightEcs, RequestRefusedWhileAnotherSceneChangeIsInFlight)
{
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));

    // 普通换图在途:不发 CreateScene,也不登记建镜像。
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/true, /*correlationId=*/9);
    EXPECT_FALSE(PlayerSceneSystem::RequestEnterMirrorScene(player, /*mirrorConfigId=*/3));
    EXPECT_FALSE(HasMirrorCreate(player));

    // 另一件建镜像在途:不顶掉它的令牌(顶掉了,它的完成回调就认领不到,玩家既进不去也收不到提示)。
    tlsEcs.actorRegistry.remove<PlayerSceneChangeInFlightComp>(player);
    AttachMirrorCreate(player, /*token=*/7);
    EXPECT_FALSE(PlayerSceneSystem::RequestEnterMirrorScene(player, /*mirrorConfigId=*/3));
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player).token, 7u);
}

TEST(MirrorCreateInFlightEcs, DoneClaimsOnlyItsOwnToken)
{
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    AttachMirrorCreate(player, /*token=*/7);

    // 不在本节点的玩家、被顶替的令牌:都不动当前等待者。
    PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId + 1, 7, DeadlineExceededOutcome(), nullptr);
    PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, 6, DeadlineExceededOutcome(), nullptr);
    ASSERT_TRUE(HasMirrorCreate(player));
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player).token, 7u);
    EXPECT_TRUE(PlayerLifecycleSystem::IsSceneChangeBusy(player));

    // 自己的传输失败(结果未知):摘槽位,玩家可以立刻重试;不发自动进场。
    PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, 7, DeadlineExceededOutcome(), nullptr);
    EXPECT_FALSE(HasMirrorCreate(player));
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
}

TEST(MirrorCreateInFlightEcs, RejectedCreateReleasesTheSlot)
{
    // 业务失败应答不回显 creator_ids:旧的全局处理器找不到创建者,槽位(若有)只能等 TTL。每次调用回调按令牌认领。
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    AttachMirrorCreate(player, /*token=*/11);
    const auto rejected = MakeCreateReply(/*errorCode=*/5, /*sceneId=*/0);
    PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, 11, OkOutcome(), &rejected);
    EXPECT_FALSE(HasMirrorCreate(player));
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));

    // 成功却没给 scene_id:同样按失败收尾,不拿 0 去进场。
    AttachMirrorCreate(player, /*token=*/12);
    const auto noScene = MakeCreateReply(/*errorCode=*/0, /*sceneId=*/0);
    PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, 12, OkOutcome(), &noScene);
    EXPECT_FALSE(HasMirrorCreate(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
}

TEST(MirrorCreateInFlightEcs, CreatedWhileInBattleDoesNotAutoEnter)
{
    // 提交点复核:建镜像这一个往返里玩家可能刚进备战,战斗在途不换场景。
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    AttachMirrorCreate(player, /*token=*/21);
    tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(4242);
    const auto created = MakeCreateReply(/*errorCode=*/0, /*sceneId=*/9001);

    PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, 21, OkOutcome(), &created);

    EXPECT_FALSE(HasMirrorCreate(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player)) << "不发自动进场的 EnterScene";
}

TEST(MirrorCreateInFlightEcs, CreatedWithoutSceneManagerReleasesTheSlot)
{
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_EQ(GetSceneManagerEntity(kExitPlayerId), entt::entity{entt::null}) << "前置:宿主没有 scene_manager 节点,不会发真实 RPC";
    AttachMirrorCreate(player, /*token=*/31);
    const auto created = MakeCreateReply(/*errorCode=*/0, /*sceneId=*/9001);

    PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, 31, OkOutcome(), &created);

    EXPECT_FALSE(HasMirrorCreate(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player));
}

TEST(MirrorCreateInFlightEcs, UncorrelatedEnterSceneReplyLeavesMirrorCreateAlone)
{
    // 不复用 PlayerSceneChangeInFlightComp 的理由:旧版 scene_manager 不回显号的 EnterScene 应答按 player_id
    // 对普通换图槽。建镜像单独一个组件,这类应答找不到等待者,18 也不会拿建镜像的槽起交接。
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachMirrorCreate(player, /*token=*/41);

    DeliverEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, /*correlationId=*/0));
    DeliverEnterSceneReply(
        MakeReply(kExitPlayerId, PlayerLifecycleSystem::kSmErrHandoffPending, /*correlationId=*/0));

    ASSERT_TRUE(HasMirrorCreate(player));
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player).token, 41u);
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp, PlayerFrozenComp>(player)));
}

TEST(MirrorCreateInFlightEcs, ExpiredSuccessDoesNotDisturbNewerSceneChange)
{
    const auto created = MakeCreateReply(/*errorCode=*/0, /*sceneId=*/9001);
    CheckLateMirrorCompletion(OkOutcome(), &created);
}

TEST(MirrorCreateInFlightEcs, ExpiredBusinessFailureDoesNotDisturbNewerSceneChange)
{
    const auto rejected = MakeCreateReply(/*errorCode=*/5, /*sceneId=*/0);
    CheckLateMirrorCompletion(OkOutcome(), &rejected);
}

TEST(MirrorCreateInFlightEcs, ExpiredTransportFailureDoesNotDisturbNewerSceneChange)
{
    CheckLateMirrorCompletion(DeadlineExceededOutcome(), nullptr);
}

TEST(MirrorCreateInFlightEcs, ClockRecoveryAfterCommittedChangeCannotReviveOldSuccess)
{
    const auto created = MakeCreateReply(/*errorCode=*/0, /*sceneId=*/9001);
    CheckMirrorCompletionAfterClockRecovery(OkOutcome(), &created);
}

TEST(MirrorCreateInFlightEcs, ClockRecoveryAfterCommittedChangeCannotReviveOldBusinessFailure)
{
    const auto rejected = MakeCreateReply(/*errorCode=*/5, /*sceneId=*/0);
    CheckMirrorCompletionAfterClockRecovery(OkOutcome(), &rejected);
}

TEST(MirrorCreateInFlightEcs, ClockRecoveryAfterCommittedChangeCannotReviveOldTransportFailure)
{
    CheckMirrorCompletionAfterClockRecovery(DeadlineExceededOutcome(), nullptr);
}

TEST(MirrorCreateInFlightEcs, CommittedHandoffPermanentlyCancelsOldMirror)
{
    const ScopedFakeSceneManagerNode sceneManager;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReplyTestPlayer(player));
    AttachMirrorCreate(player, /*token=*/82);
    tlsEcs.actorRegistry.get<PlayerMirrorCreateInFlightComp>(player).sentAtMs =
        TimeSystem::NowMillisecondsUTC() + 60000;
    ASSERT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player));
    const auto before = travel_handoff_stats::Read();

    EXPECT_EQ(PlayerLifecycleSystem::StartTravelHandoff(player, kReplyTargetZone, 0, 0),
              PlayerLifecycleSystem::kTravelAccepted);

    EXPECT_EQ(travel_handoff_stats::Read().started, before.started + 1);
    EXPECT_EQ(travel_handoff_stats::Read().aborted, before.aborted)
        << "沿用无 owner_epoch 宿主的就地 Abort,旧镜像资格仍不得恢复";
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_FALSE(HasMirrorCreate(player));
    MirrorTipPushProbe tipPush;
    PlayerTipSystem::SendToPlayer(player, kEnterSceneFailed, {});
    ASSERT_EQ(tipPush.Count(), 1);
    tipPush.Reset();
    const auto created = MakeCreateReply(/*errorCode=*/0, /*sceneId=*/9001);
    PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, 82, OkOutcome(), &created);
    EXPECT_EQ(tipPush.Count(), 0);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
}

TEST(MirrorCreateInFlightEcs, CommittedExitCancelsMirrorWhileWaitingForDbAck)
{
    ScopedPersistedPlayerCleanup cleanup;
    struct LedgerCleanup
    {
        LedgerCleanup() { DbTaskDispatchSystem::ResetForTest(); }
        ~LedgerCleanup() { DbTaskDispatchSystem::ResetForTest(); }
    } ledgerCleanup;
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    auto& ledger = DbTaskDispatchSystem::LedgerForTest();
    const auto generation = ledger.BeginSave(kExitPlayerId);
    ledger.IssueTicket(kExitPlayerId, generation, db_task_dispatch::Clock::now());
    ledger.FinishSave(kExitPlayerId, generation, /*allQueued=*/true);
    AttachMirrorCreate(player, /*token=*/83);

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kNodeShutdown);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "实体还在等 DBTask 回执,旧完成确实仍可能找到它";
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player));
    ASSERT_FALSE(HasMirrorCreate(player));
    MirrorTipPushProbe tipPush;
    PlayerTipSystem::SendToPlayer(player, kEnterSceneFailed, {});
    ASSERT_EQ(tipPush.Count(), 1);
    tipPush.Reset();
    const auto created = MakeCreateReply(/*errorCode=*/0, /*sceneId=*/9001);
    PlayerSceneSystem::HandleMirrorCreateDone(kExitPlayerId, 83, OkOutcome(), &created);
    EXPECT_EQ(tipPush.Count(), 0);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
}

// ============================================================================
// Test bootstrap
// ============================================================================
TEST(LocalNodeInfoTest, SurvivesEcsClear)
{
    TestLocalNodeInfo().set_zone_id(9);
    TestLocalNodeInfo().set_node_id(31);
    tlsEcs.Clear();
    EXPECT_EQ(&GetNodeInfo(), &TestLocalNodeInfo());
    EXPECT_EQ(GetZoneId(), 9u);
    EXPECT_EQ(GetNodeInfo().node_id(), 31u);
}


TEST(LocalNodeInfoTest, UninstalledIsReadOnlyDefault)
{
    InstallLocalNodeInfo(nullptr);
    EXPECT_EQ(&GetNodeInfo(), &NodeInfo::default_instance());
    EXPECT_EQ(GetZoneId(), 0u);
    InstallLocalNodeInfo(&TestLocalNodeInfo());
    EXPECT_EQ(&GetNodeInfo(), &TestLocalNodeInfo());
}


TEST(ServiceNodeSnapshotTest, UpsertKeepsOneSnapshotPerUuid)
{
    ServiceNodeSnapshot snapshot;
    NodeInfo first;
    first.set_node_uuid("first");
    first.mutable_endpoint()->set_port(10001);
    EXPECT_FALSE(snapshot.HasNodesOfType(SceneNodeService));
    EXPECT_EQ(snapshot.Upsert(SceneNodeService, first), ServiceNodeSnapshot::UpsertResult::kAdded);
    first.mutable_endpoint()->set_port(10002);
    EXPECT_EQ(snapshot.Upsert(SceneNodeService, first), ServiceNodeSnapshot::UpsertResult::kUpdated);
    ASSERT_EQ(snapshot.NodesOfType(SceneNodeService).size(), 1);
    EXPECT_EQ(snapshot.NodesOfType(SceneNodeService).Get(0).endpoint().port(), 10002u);

    NodeInfo second;
    second.set_node_uuid("second");
    EXPECT_EQ(snapshot.Upsert(SceneNodeService, second), ServiceNodeSnapshot::UpsertResult::kAdded);
    EXPECT_EQ(snapshot.NodesOfType(SceneNodeService).size(), 2);
    EXPECT_TRUE(snapshot.HasNodesOfType(SceneNodeService));
    EXPECT_FALSE(snapshot.HasNodesOfType(GateNodeService));
}

TEST(ServiceNodeSnapshotTest, RemovePreservesOtherNodesAndTypes)
{
    ServiceNodeSnapshot snapshot;
    NodeInfo first;
    first.set_node_uuid("first");
    NodeInfo second;
    second.set_node_uuid("second");
    snapshot.Upsert(SceneNodeService, first);
    snapshot.Upsert(SceneNodeService, second);
    snapshot.Upsert(GateNodeService, first);

    EXPECT_EQ(snapshot.Remove(SceneNodeService, "first"), 1);
    EXPECT_EQ(snapshot.Remove(SceneNodeService, "first"), 0);
    ASSERT_EQ(snapshot.NodesOfType(SceneNodeService).size(), 1);
    EXPECT_EQ(snapshot.NodesOfType(SceneNodeService).Get(0).node_uuid(), "second");
    EXPECT_EQ(snapshot.NodesOfType(GateNodeService).size(), 1);
    EXPECT_EQ(snapshot.Remove(SceneNodeService, "second"), 1);
    EXPECT_FALSE(snapshot.HasNodesOfType(SceneNodeService));
}

TEST(ServiceNodeSnapshotTest, InvalidNodeTypesAreEmptyAndRejectWrites)
{
    ServiceNodeSnapshot snapshot;
    NodeInfo node;
    node.set_node_uuid("valid");
    snapshot.Upsert(SceneNodeService, node);
    for (uint32_t invalid : {static_cast<uint32_t>(eNodeType_ARRAYSIZE), UINT32_MAX})
    {
        EXPECT_TRUE(snapshot.NodesOfType(invalid).empty());
        EXPECT_FALSE(snapshot.HasNodesOfType(invalid));
        EXPECT_EQ(snapshot.Upsert(invalid, node), ServiceNodeSnapshot::UpsertResult::kRejected);
        EXPECT_EQ(snapshot.Remove(invalid, node.node_uuid()), 0);
    }
    EXPECT_EQ(snapshot.NodesOfType(SceneNodeService).size(), 1);
}

int main(int argc, char** argv)
{
    ::testing::InitGoogleTest(&argc, argv);
    InstallLocalNodeInfo(&TestLocalNodeInfo());
    ::testing::UnitTest::GetInstance()->listeners().Append(new LocalNodeInfoResetListener);
    const int result = RUN_ALL_TESTS();
    InstallLocalNodeInfo(nullptr);
    return result;
}

// ============================================================================
// 进场链余项 c / d 的 C++ 部分(docs/design/scene-enter-chain-residuals.md §4.7 / §5.7)。
// 本文件有别的会话的大量在途改动,本节只在文件末尾追加:ReleasePlayer 归属栅栏(纯判定 + 库函数接缝)、
// 发往 scene_manager 的请求盖章(enter_fence::ActingLoginGeneration)、scene_manager 私有码 25。
// 宿主里没有 Redis:会走到退出的释放类用例先 MarkPersisted,让退出走快路径同步收尾(不碰 Redis 客户端)。
// 这样不带栅栏的旧逻辑在这些用例上只会断言失败(实体被销毁),不会因空 Redis 客户端崩溃。
// ============================================================================
#include "services/scene/player/system/release_fence.h"

namespace
{
namespace rf = release_fence;

constexpr rf::Verdict kAllReleaseVerdicts[] = {
    rf::Verdict::kRelease,
    rf::Verdict::kReleaseUnversioned,
    rf::Verdict::kReleaseCacheUnknown,
    rf::Verdict::kSkipHandoffInFlight,
    rf::Verdict::kSkipStaleRelease,
    rf::Verdict::kSkipAheadOfHolder,
};
static_assert(std::size(kAllReleaseVerdicts) == rf::kVerdictCount, "新增结论时补进这张表,并补判定用例");

constexpr uint64_t kReleaseExpectedEpoch = 5; // E:scene_manager 本次落点 CAS 用过的观察值

// 静态存储期,供 constexpr 断言取地址。
constexpr PlayerLoginIdentityComp kActingIdentity{kSessionAtExit, 9};

uint64_t ReleaseVerdictCount(const release_fence_stats::Snapshot &snapshot, rf::Verdict verdict)
{
    return snapshot.verdicts[static_cast<std::size_t>(verdict)];
}

// 在线实体(kExitPlayerId,绑定 session)+ 缓存 owner_epoch(0 = 不挂组件)+ "盘上已是同一份"。
bool MakeReleaseTarget(uint64_t cachedOwnerEpoch, entt::entity &player, SessionId session = kSessionAtExit)
{
    player = MakeOnlinePlayer(session);
    if (cachedOwnerEpoch != 0)
    {
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = cachedOwnerEpoch;
    }
    return MarkPersisted(player);
}

void ReleaseFromSceneManager(uint64_t expectedOwnerEpoch)
{
    PlayerLifecycleSystem::HandleReleaseFromSceneManager(kExitPlayerId, expectedOwnerEpoch, /*targetSceneId=*/42,
                                                         /*targetNodeId=*/"node-target");
}

// 被忽略的释放零副作用:实体与会话映射都还在,没有挂上退出标记。调用方先 ASSERT 实体有效。
void ExpectEntityUntouched(entt::entity player, SessionId session)
{
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), player);
    EXPECT_EQ(SessionMap().count(session), 1u);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerExitIntentComp>(player));
}

// 被执行的释放走了 kReleasedByTransfer 退出:宿主里快路径同步收尾、实体销毁;A1′ 按"原因不在可写集合"跳过
// (released_by_transfer 不写断线释放标记),据此确认退出原因。
void ExpectReleasedByTransfer(entt::entity player, SessionId session, const exit_release_stats::Snapshot &before)
{
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    EXPECT_EQ(SessionMap().count(session), 0u);
    const auto skipCause = static_cast<std::size_t>(erm::ExitReleaseDecision::kSkipCause);
    EXPECT_EQ(exit_release_stats::Read().decisions[skipCause], before.decisions[skipCause] + 1)
        << "退出原因必须是 released_by_transfer(A1′ 按原因跳过)";
}
} // namespace

TEST(ReleaseFence, DecideTableEveryCell)
{
    constexpr uint64_t E = kReleaseExpectedEpoch;
    for (const uint64_t cached : {uint64_t{0}, E - 1, E, E + 2})
    {
        // 第 1 行:交接在途先于一切比较。
        for (const uint64_t expected : {uint64_t{0}, E})
        {
            EXPECT_EQ(rf::Decide(expected, cached, /*handoffRequested=*/true), rf::Verdict::kSkipHandoffInFlight)
                << "expected=" << expected << " cached=" << cached;
        }
        // 第 2 行:期望 0(旧版 scene_manager)按旧行为释放,先于缓存判断。
        EXPECT_EQ(rf::Decide(0, cached, false), rf::Verdict::kReleaseUnversioned) << "cached=" << cached;
    }
    EXPECT_EQ(rf::Decide(E, 0, false), rf::Verdict::kReleaseCacheUnknown) << "第 3 行:缓存 0(含组件缺失)按旧行为释放";
    EXPECT_EQ(rf::Decide(E, E, false), rf::Verdict::kRelease) << "第 4 行:缓存恰好是那一代";
    EXPECT_EQ(rf::Decide(E, E + 2, false), rf::Verdict::kSkipStaleRelease) << "第 5 行:已采纳回滚 / 回流重载,迟到的旧释放";
    EXPECT_EQ(rf::Decide(E, E - 1, false), rf::Verdict::kSkipAheadOfHolder) << "第 6 行:期望超前于缓存,生产构造不出";
    static_assert(rf::Decide(5, 5, false) == rf::Verdict::kRelease &&
                      rf::Decide(5, 7, false) == rf::Verdict::kSkipStaleRelease &&
                      rf::Decide(5, 7, true) == rf::Verdict::kSkipHandoffInFlight,
                  "判定必须能在编译期求值(纯函数)");

    // 只有前三种结论执行退出;越界按不释放(fail-closed:退出是破坏性动作)。
    for (const auto verdict : kAllReleaseVerdicts)
    {
        const bool expectRelease = verdict == rf::Verdict::kRelease || verdict == rf::Verdict::kReleaseUnversioned ||
                                   verdict == rf::Verdict::kReleaseCacheUnknown;
        EXPECT_EQ(rf::Releases(verdict), expectRelease) << rf::VerdictName(verdict);
    }
    EXPECT_FALSE(rf::Releases(rf::Verdict::kCount));
}

TEST(ReleaseFence, NamesCoverEveryValue)
{
    for (std::size_t i = 0; i < rf::kVerdictCount; ++i)
    {
        const auto verdict = kAllReleaseVerdicts[i];
        EXPECT_EQ(static_cast<std::size_t>(verdict), i) << "表按枚举序排列";
        EXPECT_STRNE(rf::VerdictName(verdict), "?");
        for (std::size_t j = 0; j < i; ++j)
        {
            EXPECT_STRNE(rf::VerdictName(kAllReleaseVerdicts[j]), rf::VerdictName(verdict)) << "汇总行 key 不得重名";
        }
    }
    EXPECT_STREQ(rf::VerdictName(rf::Verdict::kCount), "?") << "越界不能读出数组外";
    // 名字是 [ReleaseFence] 汇总行的 key(运维检索),钉住三个告警项的原文。
    EXPECT_STREQ(rf::VerdictName(rf::Verdict::kSkipStaleRelease), "skip_stale_release");
    EXPECT_STREQ(rf::VerdictName(rf::Verdict::kSkipAheadOfHolder), "skip_ahead_of_holder");
    EXPECT_STREQ(rf::VerdictName(rf::Verdict::kReleaseCacheUnknown), "release_cache_unknown");
}

TEST(ReleaseFence, SummaryEscalatesOnlyOnAnomalies)
{
    for (const auto verdict : kAllReleaseVerdicts)
    {
        const bool expectAlarming = verdict == rf::Verdict::kReleaseCacheUnknown ||
                                    verdict == rf::Verdict::kSkipStaleRelease ||
                                    verdict == rf::Verdict::kSkipAheadOfHolder;
        EXPECT_EQ(rf::Alarming(verdict), expectAlarming) << rf::VerdictName(verdict);
    }
    release_fence_stats::Snapshot previous;
    release_fence_stats::Snapshot next = previous;
    next.verdicts[static_cast<std::size_t>(rf::Verdict::kRelease)] += 3;
    next.verdicts[static_cast<std::size_t>(rf::Verdict::kReleaseUnversioned)] += 1;
    next.verdicts[static_cast<std::size_t>(rf::Verdict::kSkipHandoffInFlight)] += 2;
    next.notFound += 4;
    EXPECT_FALSE(next.AlarmingIncreaseSince(previous)) << "常态计数增长打 INFO";
    next.verdicts[static_cast<std::size_t>(rf::Verdict::kSkipStaleRelease)] += 1;
    EXPECT_TRUE(next.AlarmingIncreaseSince(previous)) << "迟到的旧释放被挡下:升 WARN";
}

TEST(ReleaseFenceEcs, LateReleaseAfterRollbackAdoptKeepsEntity)
{
    // 主时序(设计 §4.1):B5 采纳之后 —— 缓存已抬到 E+2、交接组件已摘(IsHandoffRequested 为假),迟到的释放带 E。
    // 宿主里真实 B5 成功采纳会强制写 Redis(没有客户端),所以直接构造采纳之后的实体;"先抬 epoch、后摘交接组件"
    // 的顺序由 ResolveTravelOutcome 那里的注释钉住。
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch + 2, player));
    ASSERT_FALSE(PlayerLifecycleSystem::IsHandoffRequested(player));
    const auto before = release_fence_stats::Read();

    ReleaseFromSceneManager(kReleaseExpectedEpoch);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "采纳回滚后仍持有归属的实体不得被迟到的旧释放退掉";
    ExpectEntityUntouched(player, kSessionAtExit);
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch, kReleaseExpectedEpoch + 2)
        << "只读缓存,不采纳、不改写";
    const auto after = release_fence_stats::Read();
    EXPECT_EQ(ReleaseVerdictCount(after, rf::Verdict::kSkipStaleRelease),
              ReleaseVerdictCount(before, rf::Verdict::kSkipStaleRelease) + 1);
}

TEST(ReleaseFenceEcs, LateReleaseOnReturnedEntityIgnored)
{
    // 变体 C(回流):换到别处成功(E+1)后约 6s 内又换回本节点(铸 E+2),本节点新载入的实体带着新会话与 E+2;
    // 还在重试的旧释放(期望 E)此时才到。
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch + 2, player, kNewerSession));
    auto &identity = tlsEcs.actorRegistry.emplace<PlayerLoginIdentityComp>(player);
    identity.sessionId = kNewerSession;
    identity.loginGeneration = 9;
    const auto before = release_fence_stats::Read();

    ReleaseFromSceneManager(kReleaseExpectedEpoch);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "回流后新载入的实体不得被旧释放退掉";
    ExpectEntityUntouched(player, kNewerSession);
    const auto &identityAfter = tlsEcs.actorRegistry.get<PlayerLoginIdentityComp>(player);
    EXPECT_EQ(identityAfter.sessionId, kNewerSession);
    EXPECT_EQ(identityAfter.loginGeneration, 9u);
    EXPECT_EQ(ReleaseVerdictCount(release_fence_stats::Read(), rf::Verdict::kSkipStaleRelease),
              ReleaseVerdictCount(before, rf::Verdict::kSkipStaleRelease) + 1);
}

TEST(ReleaseFenceEcs, EqualEpochReleases)
{
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player));
    const auto exitBefore = exit_release_stats::Read();
    const auto before = release_fence_stats::Read();

    ReleaseFromSceneManager(kReleaseExpectedEpoch);

    ExpectReleasedByTransfer(player, kSessionAtExit, exitBefore);
    EXPECT_EQ(ReleaseVerdictCount(release_fence_stats::Read(), rf::Verdict::kRelease),
              ReleaseVerdictCount(before, rf::Verdict::kRelease) + 1);
}

TEST(ReleaseFenceEcs, UnversionedReleaseKeepsLegacy)
{
    // 旧版 scene_manager 不带期望值(0):按旧行为释放并计数。
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player));
    const auto exitBefore = exit_release_stats::Read();
    const auto before = release_fence_stats::Read();

    ReleaseFromSceneManager(/*expectedOwnerEpoch=*/0);

    ExpectReleasedByTransfer(player, kSessionAtExit, exitBefore);
    EXPECT_EQ(ReleaseVerdictCount(release_fence_stats::Read(), rf::Verdict::kReleaseUnversioned),
              ReleaseVerdictCount(before, rf::Verdict::kReleaseUnversioned) + 1);
}

TEST(ReleaseFenceEcs, CacheUnknownReleasesAndCounts)
{
    // 实体没有 PlayerOwnerEpochComp(缓存按 0):不 fail-closed,按旧行为释放并计数。
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(/*cachedOwnerEpoch=*/0, player));
    ASSERT_FALSE(tlsEcs.actorRegistry.any_of<PlayerOwnerEpochComp>(player));
    const auto exitBefore = exit_release_stats::Read();
    const auto before = release_fence_stats::Read();

    ReleaseFromSceneManager(kReleaseExpectedEpoch);

    ExpectReleasedByTransfer(player, kSessionAtExit, exitBefore);
    EXPECT_EQ(ReleaseVerdictCount(release_fence_stats::Read(), rf::Verdict::kReleaseCacheUnknown),
              ReleaseVerdictCount(before, rf::Verdict::kReleaseCacheUnknown) + 1);
}

TEST(ReleaseFenceEcs, AheadOfHolderIgnored)
{
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player));
    const auto before = release_fence_stats::Read();

    ReleaseFromSceneManager(kReleaseExpectedEpoch + 1);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "期望超前于缓存:退出要求严格相等,忽略";
    ExpectEntityUntouched(player, kSessionAtExit);
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch, kReleaseExpectedEpoch);
    EXPECT_EQ(ReleaseVerdictCount(release_fence_stats::Read(), rf::Verdict::kSkipAheadOfHolder),
              ReleaseVerdictCount(before, rf::Verdict::kSkipAheadOfHolder) + 1);
}

TEST(ReleaseFenceEcs, HandoffInFlightStillIgnored)
{
    // 现有守卫,语义不变:交接已发起的实体不论期望值多少都不走退出。
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player));
    AttachHandoff(player, /*correlationId=*/0);
    ASSERT_TRUE(PlayerLifecycleSystem::IsHandoffRequested(player));
    const auto before = release_fence_stats::Read();

    for (const uint64_t expected : {uint64_t{0}, kReleaseExpectedEpoch, kReleaseExpectedEpoch + 2})
    {
        ReleaseFromSceneManager(expected);
        ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "expected=" << expected;
    }

    ExpectEntityUntouched(player, kSessionAtExit);
    ASSERT_TRUE((tlsEcs.actorRegistry.all_of<PlayerTravelHandoffComp, PlayerFrozenComp>(player)))
        << "交接与冻结组件原样保留,去留只由 EnterScene 应答与看门狗裁决";
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerTravelHandoffComp>(player).requestedAtMs, 1000u);
    EXPECT_EQ(ReleaseVerdictCount(release_fence_stats::Read(), rf::Verdict::kSkipHandoffInFlight),
              ReleaseVerdictCount(before, rf::Verdict::kSkipHandoffInFlight) + 3);
}

TEST(ReleaseFenceEcs, StaleReleaseOnExitingEntityDoesNotMerge)
{
    // 退出中(重存到上限、被保留)的实体缓存 E+2,迟到的旧释放带 E:不得进 HandleExitGameNode。
    // 不带栅栏时会合并原因并触发超限重踢,快路径收尾后实体被销毁。
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = kReleaseExpectedEpoch + 2;
    tlsEcs.actorRegistry.get<PlayerExitIntentComp>(player).resaveRounds = player_exit::kMaxExitResaveRounds;
    ASSERT_TRUE(MarkPersisted(player));
    const PlayerExitIntentComp intentBefore = tlsEcs.actorRegistry.get<PlayerExitIntentComp>(player);
    const auto persistBefore = exit_persist_stats::Read();
    const auto before = release_fence_stats::Read();

    ReleaseFromSceneManager(kReleaseExpectedEpoch);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "被忽略的释放不得推动退出收尾";
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player)) << "原有退出照常在途,不被摘也不被重挂";
    const auto *intent = tlsEcs.actorRegistry.try_get<PlayerExitIntentComp>(player);
    ASSERT_NE(intent, nullptr);
    EXPECT_EQ(intent->cause, intentBefore.cause);
    EXPECT_EQ(intent->releaseMarkSuppressed, intentBefore.releaseMarkSuppressed);
    EXPECT_EQ(intent->suppressedBy, intentBefore.suppressedBy);
    EXPECT_EQ(intent->resaveRounds, intentBefore.resaveRounds) << "不触发超限重踢(轮次不清零)";
    EXPECT_EQ(exit_persist_stats::Read().exhaustedRekick, persistBefore.exhaustedRekick);
    EXPECT_EQ(ReleaseVerdictCount(release_fence_stats::Read(), rf::Verdict::kSkipStaleRelease),
              ReleaseVerdictCount(before, rf::Verdict::kSkipStaleRelease) + 1);
}

TEST(ReleaseFenceEcs, PlayerNotOnThisNodeIsIdempotent)
{
    ScopedPersistedPlayerCleanup cleanup;
    tlsEcs.Clear();
    SessionMap().clear();
    const auto before = release_fence_stats::Read();

    ReleaseFromSceneManager(kReleaseExpectedEpoch);

    const auto after = release_fence_stats::Read();
    EXPECT_EQ(after.notFound, before.notFound + 1);
    EXPECT_EQ(after.verdicts, before.verdicts) << "找不到实体时不做判定";
}

TEST(ActingLoginGeneration, NoIdentityComponentYieldsZero)
{
    EXPECT_EQ(ef::ActingLoginGeneration(nullptr, kSessionAtExit), 0u);
    static_assert(ef::ActingLoginGeneration(nullptr, kSessionAtExit) == 0, "纯函数,可在编译期求值");
}

TEST(ActingLoginGeneration, SessionMismatchYieldsZero)
{
    PlayerLoginIdentityComp identity;
    identity.sessionId = kSessionAtExit;
    identity.loginGeneration = 9;
    EXPECT_EQ(ef::ActingLoginGeneration(&identity, kNewerSession), 0u) << "请求会话不是实体已放行的那一条:不冒用它的代次";
    EXPECT_EQ(ef::ActingLoginGeneration(&identity, 0), 0u);
    EXPECT_EQ(ef::ActingLoginGeneration(&identity, kInvalidSessionId), 0u);

    // 两边都"没有会话"不算同一会话(与 player_exit::IsBoundSession 同一口径)。
    PlayerLoginIdentityComp unbound;
    unbound.sessionId = kInvalidSessionId;
    unbound.loginGeneration = 9;
    EXPECT_EQ(ef::ActingLoginGeneration(&unbound, kInvalidSessionId), 0u);
    unbound.sessionId = 0;
    EXPECT_EQ(ef::ActingLoginGeneration(&unbound, 0), 0u);
}

TEST(ActingLoginGeneration, MatchingSessionYieldsGeneration)
{
    PlayerLoginIdentityComp identity;
    identity.sessionId = kSessionAtExit;
    identity.loginGeneration = 9;
    EXPECT_EQ(ef::ActingLoginGeneration(&identity, kSessionAtExit), 9u);
    identity.loginGeneration = 0;
    EXPECT_EQ(ef::ActingLoginGeneration(&identity, kSessionAtExit), 0u) << "身份没有代次(旧版 login 放行的)就如实给 0";
    static_assert(ef::ActingLoginGeneration(&kActingIdentity, kSessionAtExit) == 9, "纯函数,可在编译期求值");
}

// ===========================================================================
// 会话结束释放(ReleasePlayerRequest.session_ended)与回档证据计数
// (docs/design/location-release-binding.md §3.3、§3.7;审计 data-overwrite-rollback-audit-20261018.md §2.1)。
//
// scene_manager 的 LeaveScene 拿不到"持有者已释放"证据时请持有节点释放:持有 → 按客户端断线退出(存盘后 A1′ 写标记);
// 完全不持有 → 按 owner_epoch 条件补写凭证标记;交接在途 / 代际不等 → 不动。宿主没有 Redis:标记写一律走"写不成"
// 分支,用例只看判定与计数。
// ===========================================================================

namespace
{
constexpr rf::SessionEndedCredential kAllSessionEndedCredentials[] = {
    rf::SessionEndedCredential::kWrite,
    rf::SessionEndedCredential::kSkipUnversioned,
    rf::SessionEndedCredential::kSkipLocalHolder,
    rf::SessionEndedCredential::kSkipMarkDisabled,
    rf::SessionEndedCredential::kSkipDevBypass,
    rf::SessionEndedCredential::kSkipIdentity,
};
static_assert(std::size(kAllSessionEndedCredentials) == rf::kSessionEndedCredentialCount,
              "新增凭证结论时补进这张表,并补判定用例");

uint64_t CredentialCount(const release_fence_stats::Snapshot &snapshot, rf::SessionEndedCredential decision)
{
    return snapshot.credentialDecisions[static_cast<std::size_t>(decision)];
}

uint64_t CredentialWriteOutcomes(const release_fence_stats::Snapshot &snapshot)
{
    return snapshot.credentialWritten + snapshot.credentialEpochMoved + snapshot.credentialSuperseded +
           snapshot.credentialFailed;
}

constexpr rf::CredentialQualification kAllCredentialQualifications[] = {
    rf::CredentialQualification::kQualified,
    rf::CredentialQualification::kReadFailed,
    rf::CredentialQualification::kHolderAppeared,
    rf::CredentialQualification::kNoQualification,
    rf::CredentialQualification::kMalformed,
};
static_assert(std::size(kAllCredentialQualifications) == rf::kCredentialQualificationCount,
              "新增资格核对结论时补进这张表,并补判定用例");

uint64_t QualificationCount(const release_fence_stats::Snapshot &snapshot, rf::CredentialQualification qualification)
{
    return snapshot.credentialQualifications[static_cast<std::size_t>(qualification)];
}

// 全部条件都满足"以读到的原件写凭证"的输入,各用例只改一项。
rf::CredentialQualificationFacts QualifiedCredentialFacts()
{
    rf::CredentialQualificationFacts facts;
    facts.replyUsable = true;
    facts.stillWritable = true;
    facts.fencePresent = true;
    facts.fenceParsed = true;
    facts.fenceMatchesExpectedEpoch = true;
    facts.fenceOwnedByThisInstance = true;
    return facts;
}

// 全部条件都满足"写"的输入,各用例只改一项。
rf::SessionEndedFacts WritableSessionEndedFacts()
{
    rf::SessionEndedFacts facts;
    facts.expectedOwnerEpoch = kReleaseExpectedEpoch;
    facts.localHolderPossible = false;
    facts.markFeatureEnabled = true;
    facts.devUnsafeCrossNode = false;
    facts.identityConfirmed = true;
    return facts;
}

// 本节点完全不持有 kExitPlayerId:没有实体、没有会话映射、没有待入场条目、没有改派待确认条目。
void ClearLocalHolderOfExitPlayer()
{
    tlsEcs.Clear();
    SessionMap().clear();
    DrainRelocateConfirms();
}

// 覆盖身份(ReleasePlayerRequest.covered_*,location-release-binding.md §13)用例里的被覆盖那次登录:会话 kSessionAtExit、
// 代次 kCoveredGeneration。实体的 PlayerLoginIdentityComp 由各用例按需摆;不摆 = 实体没有登录代次。
constexpr uint64_t kCoveredGeneration = 20;

constexpr rf::SessionEndedBinding kAllSessionEndedBindings[] = {
    rf::SessionEndedBinding::kCovered,
    rf::SessionEndedBinding::kUnversionedRequest,
    rf::SessionEndedBinding::kUnversionedEntity,
    rf::SessionEndedBinding::kSkipNewerLogin,
    rf::SessionEndedBinding::kSkipConflict,
};
static_assert(std::size(kAllSessionEndedBindings) == rf::kSessionEndedBindingCount,
              "新增登录身份结论时补进这张表,并补判定用例");

uint64_t BindingCount(const release_fence_stats::Snapshot &snapshot, rf::SessionEndedBinding binding)
{
    return snapshot.bindingDecisions[static_cast<std::size_t>(binding)];
}

// 会话结束释放,覆盖身份默认是新版 scene_manager 对"离场的就是实体当前那次登录"的正常载荷。
void ReleaseEndedSession(uint64_t expectedOwnerEpoch, SessionId coveredSession = kSessionAtExit,
                         uint64_t coveredGeneration = kCoveredGeneration)
{
    PlayerLifecycleSystem::HandleSessionEndedRelease(kExitPlayerId, expectedOwnerEpoch, coveredSession, coveredGeneration);
}

// 给实体摆上"已放行的最高登录身份"(EnterScene 第 1 步 RecordLoginIdentity 写的那个组件)。
void BindLoginIdentity(entt::entity player, SessionId session, uint64_t generation)
{
    auto &identity = tlsEcs.actorRegistry.emplace_or_replace<PlayerLoginIdentityComp>(player);
    identity.sessionId = session;
    identity.loginGeneration = generation;
}

// 会话结束释放的退出走了 kClientDisconnect:宿主里快路径同步收尾、实体销毁;A1′ 判定为"写"(原因在可写集合里)。
void ExpectExitedAsClientDisconnect(entt::entity player, SessionId session, const exit_release_stats::Snapshot &before)
{
    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    EXPECT_EQ(SessionMap().count(session), 0u);
    const auto write = static_cast<std::size_t>(erm::ExitReleaseDecision::kWrite);
    EXPECT_EQ(exit_release_stats::Read().decisions[write], before.decisions[write] + 1) << "按客户端断线退出:A1′ 判定写标记";
}
} // namespace

TEST(ReleaseFence, SessionEndedCredentialDecideTable)
{
    EXPECT_EQ(rf::DecideSessionEndedCredential(WritableSessionEndedFacts()), rf::SessionEndedCredential::kWrite);

    auto facts = WritableSessionEndedFacts();
    facts.expectedOwnerEpoch = 0;
    EXPECT_EQ(rf::DecideSessionEndedCredential(facts), rf::SessionEndedCredential::kSkipUnversioned);

    facts = WritableSessionEndedFacts();
    facts.localHolderPossible = true;
    EXPECT_EQ(rf::DecideSessionEndedCredential(facts), rf::SessionEndedCredential::kSkipLocalHolder);

    facts = WritableSessionEndedFacts();
    facts.markFeatureEnabled = false;
    EXPECT_EQ(rf::DecideSessionEndedCredential(facts), rf::SessionEndedCredential::kSkipMarkDisabled);

    facts = WritableSessionEndedFacts();
    facts.devUnsafeCrossNode = true;
    EXPECT_EQ(rf::DecideSessionEndedCredential(facts), rf::SessionEndedCredential::kSkipDevBypass);

    facts = WritableSessionEndedFacts();
    facts.identityConfirmed = false;
    EXPECT_EQ(rf::DecideSessionEndedCredential(facts), rf::SessionEndedCredential::kSkipIdentity);

    // 默认值全部取"不写"的一侧:漏填任何一项都只会少写。
    EXPECT_NE(rf::DecideSessionEndedCredential(rf::SessionEndedFacts{}), rf::SessionEndedCredential::kWrite);
    static_assert(rf::DecideSessionEndedCredential(rf::SessionEndedFacts{}) == rf::SessionEndedCredential::kSkipUnversioned,
                  "判定必须能在编译期求值(纯函数)");
}

TEST(ReleaseFence, SessionEndedCredentialNamesCoverEveryValue)
{
    for (std::size_t i = 0; i < rf::kSessionEndedCredentialCount; ++i)
    {
        const auto decision = kAllSessionEndedCredentials[i];
        EXPECT_EQ(static_cast<std::size_t>(decision), i) << "表按枚举序排列";
        EXPECT_STRNE(rf::SessionEndedCredentialName(decision), "?");
        for (std::size_t j = 0; j < i; ++j)
        {
            EXPECT_STRNE(rf::SessionEndedCredentialName(kAllSessionEndedCredentials[j]),
                         rf::SessionEndedCredentialName(decision))
                << "汇总行 key 不得重名";
        }
    }
    EXPECT_STREQ(rf::SessionEndedCredentialName(rf::SessionEndedCredential::kCount), "?");
}

// 会话结束凭证的资格核对(2026-10-23,r2-lc#2 之后;location-release-binding.md §3.3):只有"读回时仍完全不持有、原件是
// 本进程实例在期望 E 下的资格"才以读到的原件做 CAS;其余每一种都不写,且各自计数,排障分得清"设计性不写"与"写失败"。
TEST(ReleaseFence, CredentialQualificationDecideTable)
{
    using Q = rf::CredentialQualification;
    EXPECT_EQ(rf::DecideCredentialQualification(QualifiedCredentialFacts()), Q::kQualified);

    auto facts = QualifiedCredentialFacts();
    facts.replyUsable = false;
    EXPECT_EQ(rf::DecideCredentialQualification(facts), Q::kReadFailed) << "读不到原件:结果未知,不写";

    facts = QualifiedCredentialFacts();
    facts.stillWritable = false;
    EXPECT_EQ(rf::DecideCredentialQualification(facts), Q::kHolderAppeared)
        << "读回时本节点又有了该玩家的内存态:原件可能就是这条新持有刚写的 H,不得消费";

    facts = QualifiedCredentialFacts();
    facts.fencePresent = false;
    facts.fenceParsed = false;
    EXPECT_EQ(rf::DecideCredentialQualification(facts), Q::kNoQualification) << "从未登记:没有可消费的资格";

    facts = QualifiedCredentialFacts();
    facts.fenceParsed = false;
    EXPECT_EQ(rf::DecideCredentialQualification(facts), Q::kMalformed);

    facts = QualifiedCredentialFacts();
    facts.fenceMatchesExpectedEpoch = false;
    EXPECT_EQ(rf::DecideCredentialQualification(facts), Q::kNoQualification) << "别的 E 的资格不归这次释放处置";

    facts = QualifiedCredentialFacts();
    facts.fenceOwnedByThisInstance = false;
    EXPECT_EQ(rf::DecideCredentialQualification(facts), Q::kNoQualification)
        << "别的节点 / 别的进程实例登记的资格不得由本节点替它释放";

    // 读失败与持有再现先于原件内容判:内存态才是"能不能消费"的前提。
    facts = QualifiedCredentialFacts();
    facts.stillWritable = false;
    facts.fenceOwnedByThisInstance = false;
    EXPECT_EQ(rf::DecideCredentialQualification(facts), Q::kHolderAppeared);

    // 默认值全部取"不写"的一侧:漏填任何一项都只会少写。
    EXPECT_NE(rf::DecideCredentialQualification(rf::CredentialQualificationFacts{}), Q::kQualified);
    static_assert(rf::DecideCredentialQualification(rf::CredentialQualificationFacts{}) == Q::kReadFailed,
                  "判定必须能在编译期求值(纯函数)");
}

TEST(ReleaseFence, CredentialQualificationNamesCoverEveryValue)
{
    for (std::size_t i = 0; i < rf::kCredentialQualificationCount; ++i)
    {
        const auto qualification = kAllCredentialQualifications[i];
        EXPECT_EQ(static_cast<std::size_t>(qualification), i) << "表按枚举序排列";
        EXPECT_STRNE(rf::CredentialQualificationName(qualification), "?");
        for (std::size_t j = 0; j < i; ++j)
        {
            EXPECT_STRNE(rf::CredentialQualificationName(kAllCredentialQualifications[j]),
                         rf::CredentialQualificationName(qualification))
                << "汇总行 key 不得重名";
        }
    }
    EXPECT_STREQ(rf::CredentialQualificationName(rf::CredentialQualification::kCount), "?");
    EXPECT_TRUE(rf::QualificationAlarming(rf::CredentialQualification::kNoQualification));
    EXPECT_TRUE(rf::QualificationAlarming(rf::CredentialQualification::kMalformed));
    EXPECT_FALSE(rf::QualificationAlarming(rf::CredentialQualification::kQualified));
    EXPECT_FALSE(rf::QualificationAlarming(rf::CredentialQualification::kHolderAppeared));
}

TEST(ReleaseFence, SummaryEscalatesOnCredentialWithoutQualification)
{
    release_fence_stats::Snapshot previous;
    release_fence_stats::Snapshot next = previous;
    ++next.credentialQualifications[static_cast<std::size_t>(rf::CredentialQualification::kQualified)];
    ++next.credentialQualifications[static_cast<std::size_t>(rf::CredentialQualification::kHolderAppeared)];
    ++next.credentialSuperseded;
    EXPECT_FALSE(next.AlarmingIncreaseSince(previous)) << "常态计数增长打 INFO";
    ++next.credentialQualifications[static_cast<std::size_t>(rf::CredentialQualification::kNoQualification)];
    EXPECT_TRUE(next.AlarmingIncreaseSince(previous)) << "活着的持有节点拿不出资格:location 可能删不掉,升 WARN";
}

TEST(ReleaseFenceEcs, SessionEndedReleaseExitsHolderAsClientDisconnect)
{
    // 持有者缓存恰好是那一代:按客户端断线退出。宿主里快路径同步收尾、实体销毁;A1′ 判定为"写"(原因在可写集合里),
    // 而不是交接释放那样按原因跳过 —— 这正是 scene_manager 下一次重试要看的那枚标记。
    ScopedPersistedPlayerCleanup cleanup;
    ScopedConfirmedNodeIdentity identity;
    DrainRelocateConfirms();
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player));
    const auto exitBefore = exit_release_stats::Read();
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
    const auto exitAfter = exit_release_stats::Read();
    const auto write = static_cast<std::size_t>(erm::ExitReleaseDecision::kWrite);
    const auto skipCause = static_cast<std::size_t>(erm::ExitReleaseDecision::kSkipCause);
    EXPECT_EQ(exitAfter.decisions[write], exitBefore.decisions[write] + 1) << "按客户端断线退出:A1′ 判定写标记";
    EXPECT_EQ(exitAfter.decisions[skipCause], exitBefore.decisions[skipCause]) << "不得按交接释放的原因跳过 A1′";
    const auto after = release_fence_stats::Read();
    EXPECT_EQ(after.sessionEnded, before.sessionEnded + 1);
    EXPECT_EQ(ReleaseVerdictCount(after, rf::Verdict::kRelease), ReleaseVerdictCount(before, rf::Verdict::kRelease) + 1);
    EXPECT_EQ(after.credentialDecisions, before.credentialDecisions) << "持有时不走凭证";
    EXPECT_EQ(BindingCount(after, rf::SessionEndedBinding::kUnversionedEntity),
              BindingCount(before, rf::SessionEndedBinding::kUnversionedEntity) + 1)
        << "夹具实体没有登录身份组件:证明不了它更新,按现行为退出";
    DrainRelocateConfirms();
}

TEST(ReleaseFenceEcs, SessionEndedReleaseOnStaleEntityIgnored)
{
    // 实体已在更新的一代(E+2):这条释放属于更早的 location,不动实体。
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch + 2, player));
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    ExpectEntityUntouched(player, kSessionAtExit);
    const auto after = release_fence_stats::Read();
    EXPECT_EQ(ReleaseVerdictCount(after, rf::Verdict::kSkipStaleRelease),
              ReleaseVerdictCount(before, rf::Verdict::kSkipStaleRelease) + 1);
    EXPECT_EQ(CredentialWriteOutcomes(after), CredentialWriteOutcomes(before)) << "有实体时绝不写凭证";
}

TEST(ReleaseFenceEcs, SessionEndedReleaseDuringHandoffIgnored)
{
    // 交接在途:去留只由 EnterScene 应答与看门狗裁决,交接的 EnterScene 会改写 location。
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player));
    AttachHandoff(player, /*correlationId=*/0);
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    ExpectEntityUntouched(player, kSessionAtExit);
    EXPECT_EQ(ReleaseVerdictCount(release_fence_stats::Read(), rf::Verdict::kSkipHandoffInFlight),
              ReleaseVerdictCount(before, rf::Verdict::kSkipHandoffInFlight) + 1);
}

TEST(ReleaseFenceEcs, SessionEndedReleaseWithoutHolderReadsQualificationBeforeCredential)
{
    // 本节点完全不持有(实体已存盘销毁、A1′ 没写成或已过 TTL):判定表判 write 之后,凭证**先读** save_barrier.handoff_fence
    // 的资格原件、核对之后才以它做 CAS(2026-10-23,r2-lc#2 之后;location-release-binding.md §3.3)。宿主里没有 zone Redis:
    // 读这一步就失败,记 credential_qualification_read_failed,不发任何标记命令、不按读到的 owner_epoch 现造资格去写
    // (那样会走到条件写、记成 credential_failed),也不是一律拒发(那样资格核对计数不会动)。
    ScopedPersistedPlayerCleanup cleanup;
    ScopedConfirmedNodeIdentity identity;
    ClearLocalHolderOfExitPlayer();
    const auto before = release_fence_stats::Read();
    const std::size_t marksInFlightBefore = PlayerLifecycleSystem::ExitReleaseMarksInFlight();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    const auto after = release_fence_stats::Read();
    EXPECT_EQ(after.sessionEnded, before.sessionEnded + 1);
    EXPECT_EQ(CredentialCount(after, rf::SessionEndedCredential::kWrite),
              CredentialCount(before, rf::SessionEndedCredential::kWrite) + 1);
    EXPECT_EQ(QualificationCount(after, rf::CredentialQualification::kReadFailed),
              QualificationCount(before, rf::CredentialQualification::kReadFailed) + 1)
        << "判 write 之后必须先读资格原件";
    for (const auto qualification : kAllCredentialQualifications)
    {
        if (qualification != rf::CredentialQualification::kReadFailed)
        {
            EXPECT_EQ(QualificationCount(after, qualification), QualificationCount(before, qualification))
                << rf::CredentialQualificationName(qualification);
        }
    }
    EXPECT_EQ(CredentialWriteOutcomes(after), CredentialWriteOutcomes(before))
        << "读不到原件就没有 CAS:written / epoch_moved / superseded / failed 都不动";
    EXPECT_EQ(PlayerLifecycleSystem::ExitReleaseMarksInFlight(), marksInFlightBefore) << "不发任何标记命令";
    EXPECT_EQ(after.verdicts, before.verdicts) << "没有实体时不做实体判定";
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u) << "凭证不建实体";
}

TEST(ReleaseFenceEcs, SessionEndedReleaseWithPendingLoadSkipsCredential)
{
    // 载入在途:载入建出实体就是持有者,放弃时 A2′ 自己补写;这里发凭证等于给即将上线的持有者发放行证。
    ScopedPersistedPlayerCleanup cleanup;
    ScopedConfirmedNodeIdentity identity;
    ClearLocalHolderOfExitPlayer();
    PlayerEnterContext ctx;
    ctx.ownerEpoch = kReleaseExpectedEpoch;
    PlayerLifecycleSystem::GetPendingEnterMap()[kExitPlayerId] = ctx;
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    const auto after = release_fence_stats::Read();
    EXPECT_EQ(CredentialCount(after, rf::SessionEndedCredential::kSkipLocalHolder),
              CredentialCount(before, rf::SessionEndedCredential::kSkipLocalHolder) + 1);
    EXPECT_EQ(CredentialWriteOutcomes(after), CredentialWriteOutcomes(before));
    EXPECT_EQ(PlayerLifecycleSystem::GetPendingEnterMap().count(kExitPlayerId), 1u) << "不碰待入场条目";
    DrainRelocateConfirms();
}

TEST(ReleaseFenceEcs, SessionEndedReleaseWithoutConfirmedIdentitySkipsCredential)
{
    // 本节点身份不确认(没注入探针 = 不确认,M3):写标记可能给接手的新进程开放行口,不写。
    ScopedPersistedPlayerCleanup cleanup;
    PlayerLifecycleSystem::SetNodeIdentityProbe({});
    ClearLocalHolderOfExitPlayer();
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    const auto after = release_fence_stats::Read();
    EXPECT_EQ(CredentialCount(after, rf::SessionEndedCredential::kSkipIdentity),
              CredentialCount(before, rf::SessionEndedCredential::kSkipIdentity) + 1);
    EXPECT_EQ(CredentialWriteOutcomes(after), CredentialWriteOutcomes(before));
}

// ── 覆盖身份:会话结束释放在归属代际放行之后再比登录身份(ReleasePlayerRequest.covered_*,location-release-binding.md §13)──

TEST(ReleaseFence, SessionEndedBindingDecideTable)
{
    constexpr uint64_t g = kCoveredGeneration;
    // 覆盖代次 0(旧版 scene_manager):按现行为,哪怕实体明显更新。
    EXPECT_EQ(rf::DecideSessionEndedBinding(kNewerSession, g + 5, 0, 0), rf::SessionEndedBinding::kUnversionedRequest);
    EXPECT_EQ(rf::DecideSessionEndedBinding(kNewerSession, g + 5, kSessionAtExit, 0),
              rf::SessionEndedBinding::kUnversionedRequest);
    // 两边都没有代次时第 1 行先于第 2 行:旧版 scene_manager 必须记到 unversioned_request(运维靠它恒 0 判断升级完成),
    // 不能因为目标实体也是旧登录就被记成 unversioned_entity 藏起来。
    EXPECT_EQ(rf::DecideSessionEndedBinding(kSessionAtExit, 0, 0, 0), rf::SessionEndedBinding::kUnversionedRequest);
    EXPECT_EQ(rf::DecideSessionEndedBinding(kInvalidSessionId, 0, kSessionAtExit, 0),
              rf::SessionEndedBinding::kUnversionedRequest);
    // 实体没有登录代次:证明不了它更新。
    EXPECT_EQ(rf::DecideSessionEndedBinding(kNewerSession, 0, kSessionAtExit, g), rf::SessionEndedBinding::kUnversionedEntity);
    EXPECT_EQ(rf::DecideSessionEndedBinding(kInvalidSessionId, 0, kSessionAtExit, g),
              rf::SessionEndedBinding::kUnversionedEntity);
    // 实体代次更大:不论会话号同不同。
    EXPECT_EQ(rf::DecideSessionEndedBinding(kNewerSession, g + 1, kSessionAtExit, g), rf::SessionEndedBinding::kSkipNewerLogin);
    EXPECT_EQ(rf::DecideSessionEndedBinding(kSessionAtExit, g + 1, kSessionAtExit, g), rf::SessionEndedBinding::kSkipNewerLogin)
        << "同会话号、更大代次 = 同一连接 LeaveGame 后的另一次登录;不沿用入场栅栏「同会话一律放行」的口径";
    EXPECT_EQ(rf::DecideSessionEndedBinding(kInvalidSessionId, g + 1, 0, g), rf::SessionEndedBinding::kSkipNewerLogin);
    // 同代:两边会话都有效且不同 → 冲突;相同,或任一侧没有会话(没得比,不当作不同)→ 覆盖。
    EXPECT_EQ(rf::DecideSessionEndedBinding(kNewerSession, g, kSessionAtExit, g), rf::SessionEndedBinding::kSkipConflict);
    EXPECT_EQ(rf::DecideSessionEndedBinding(kSessionAtExit, g, kSessionAtExit, g), rf::SessionEndedBinding::kCovered);
    EXPECT_EQ(rf::DecideSessionEndedBinding(kInvalidSessionId, g, kSessionAtExit, g), rf::SessionEndedBinding::kCovered);
    EXPECT_EQ(rf::DecideSessionEndedBinding(0, g, kSessionAtExit, g), rf::SessionEndedBinding::kCovered);
    EXPECT_EQ(rf::DecideSessionEndedBinding(kNewerSession, g, 0, g), rf::SessionEndedBinding::kCovered);
    EXPECT_EQ(rf::DecideSessionEndedBinding(kNewerSession, g, kInvalidSessionId, g), rf::SessionEndedBinding::kCovered);
    // 实体代次更小:实体服务的登录比被覆盖的还早(登录撞门时覆盖到这次登录,实体还绑着 location 上的旧会话),退出。
    EXPECT_EQ(rf::DecideSessionEndedBinding(kSessionAtExit, g - 1, kNewerSession, g), rf::SessionEndedBinding::kCovered);
    EXPECT_EQ(rf::DecideSessionEndedBinding(kSessionAtExit, g - 1, kSessionAtExit, g), rf::SessionEndedBinding::kCovered);
    static_assert(rf::DecideSessionEndedBinding(kSessionAtExit, kCoveredGeneration + 1, kSessionAtExit, kCoveredGeneration) ==
                      rf::SessionEndedBinding::kSkipNewerLogin,
                  "纯函数,可在编译期求值");
}

TEST(ReleaseFence, SessionEndedBindingNamesCoverEveryValue)
{
    for (std::size_t i = 0; i < rf::kSessionEndedBindingCount; ++i)
    {
        const auto binding = kAllSessionEndedBindings[i];
        EXPECT_EQ(static_cast<std::size_t>(binding), i) << "表按枚举序排列";
        EXPECT_STRNE(rf::SessionEndedBindingName(binding), "?");
        for (std::size_t j = 0; j < i; ++j)
        {
            EXPECT_STRNE(rf::SessionEndedBindingName(kAllSessionEndedBindings[j]), rf::SessionEndedBindingName(binding))
                << "汇总行 key 不得重名";
        }
        // 只有两项 skip 不退出,也只有它们升 WARN。
        EXPECT_EQ(rf::BindingAlarming(binding), !rf::BindingReleases(binding)) << rf::SessionEndedBindingName(binding);
    }
    EXPECT_STREQ(rf::SessionEndedBindingName(rf::SessionEndedBinding::kCount), "?") << "越界不能读出数组外";
    EXPECT_FALSE(rf::BindingReleases(rf::SessionEndedBinding::kCount)) << "越界值按不退出(fail-closed)";
    EXPECT_TRUE(rf::BindingReleases(rf::SessionEndedBinding::kCovered));
    EXPECT_TRUE(rf::BindingReleases(rf::SessionEndedBinding::kUnversionedRequest));
    EXPECT_TRUE(rf::BindingReleases(rf::SessionEndedBinding::kUnversionedEntity));
    EXPECT_FALSE(rf::BindingReleases(rf::SessionEndedBinding::kSkipNewerLogin));
    EXPECT_FALSE(rf::BindingReleases(rf::SessionEndedBinding::kSkipConflict));
    // 名字是 [ReleaseFence] 汇总行 binding_ 后缀的 key(运维检索),钉住两个告警项的原文。
    EXPECT_STREQ(rf::SessionEndedBindingName(rf::SessionEndedBinding::kSkipNewerLogin), "skip_newer_login");
    EXPECT_STREQ(rf::SessionEndedBindingName(rf::SessionEndedBinding::kSkipConflict), "skip_conflict");
}

TEST(ReleaseFence, SummaryEscalatesOnBindingSkip)
{
    release_fence_stats::Snapshot previous;
    release_fence_stats::Snapshot next = previous;
    next.bindingDecisions[static_cast<std::size_t>(rf::SessionEndedBinding::kCovered)] += 3;
    next.bindingDecisions[static_cast<std::size_t>(rf::SessionEndedBinding::kUnversionedRequest)] += 1;
    next.bindingDecisions[static_cast<std::size_t>(rf::SessionEndedBinding::kUnversionedEntity)] += 1;
    EXPECT_FALSE(next == previous) << "binding 计数有变化就要打行";
    EXPECT_FALSE(next.AlarmingIncreaseSince(previous)) << "常态计数增长打 INFO";
    next.bindingDecisions[static_cast<std::size_t>(rf::SessionEndedBinding::kSkipNewerLogin)] += 1;
    EXPECT_TRUE(next.AlarmingIncreaseSince(previous)) << "挡下迟到的会话结束释放:升 WARN";
    release_fence_stats::Snapshot conflict = previous;
    conflict.bindingDecisions[static_cast<std::size_t>(rf::SessionEndedBinding::kSkipConflict)] += 1;
    EXPECT_TRUE(conflict.AlarmingIncreaseSince(previous));
}

TEST(ReleaseFenceEcs, SessionEndedReleaseAfterSameNodeReloginKeepsNewerLogin)
{
    // §12.5 残余 1 的主时序:登录撞门的同步释放请求超时(failed)。客户端随即重登(会话 kNewerSession、更大代次),落回本节点
    // (同物理节点不铸造,仍是 E),复用实体并在 EnterScene 第 1 步记下新身份;覆盖到 (kSessionAtExit, g) 的那条释放这时才
    // 在本节点执行。代际相等照旧放行,登录身份判定挡下:实体与新会话原样保留。
    ScopedPersistedPlayerCleanup cleanup;
    DrainRelocateConfirms();
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player, kNewerSession));
    BindLoginIdentity(player, kNewerSession, kCoveredGeneration + 1);
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "迟到的会话结束释放不得退掉已绑上更新登录的实体";
    ExpectEntityUntouched(player, kNewerSession);
    const auto &identity = tlsEcs.actorRegistry.get<PlayerLoginIdentityComp>(player);
    EXPECT_EQ(identity.sessionId, kNewerSession);
    EXPECT_EQ(identity.loginGeneration, kCoveredGeneration + 1) << "只读,不改写身份";
    const auto after = release_fence_stats::Read();
    EXPECT_EQ(after.sessionEnded, before.sessionEnded + 1);
    EXPECT_EQ(ReleaseVerdictCount(after, rf::Verdict::kRelease), ReleaseVerdictCount(before, rf::Verdict::kRelease) + 1)
        << "归属代际照旧判 release:挡下它的是第二道判定";
    EXPECT_EQ(BindingCount(after, rf::SessionEndedBinding::kSkipNewerLogin),
              BindingCount(before, rf::SessionEndedBinding::kSkipNewerLogin) + 1);
    EXPECT_TRUE(after.AlarmingIncreaseSince(before)) << "30s 汇总行升 WARN";
    EXPECT_EQ(CredentialWriteOutcomes(after), CredentialWriteOutcomes(before)) << "有实体时绝不写凭证";
    DrainRelocateConfirms();
}

TEST(ReleaseFenceEcs, SessionEndedReleaseKeepsSameConnectionRelogin)
{
    // LeaveScene 的后台派发晚到:LeaveGame 之后同一连接再登录,会话号不变、代次变大,落回本节点复用实体;覆盖到离场那次
    // 登录 (kSessionAtExit, g) 的释放这时才到。只比会话号会把这次新登录当成离场会话退掉。
    ScopedPersistedPlayerCleanup cleanup;
    DrainRelocateConfirms();
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player));
    BindLoginIdentity(player, kSessionAtExit, kCoveredGeneration + 1);
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    ExpectEntityUntouched(player, kSessionAtExit);
    EXPECT_EQ(BindingCount(release_fence_stats::Read(), rf::SessionEndedBinding::kSkipNewerLogin),
              BindingCount(before, rf::SessionEndedBinding::kSkipNewerLogin) + 1);
    DrainRelocateConfirms();
}

TEST(ReleaseFenceEcs, SessionEndedReleaseExitsCoveredLogin)
{
    // 实体绑着的就是被覆盖的那次登录(登出 / 断线后 LeaveScene 的正常时序):照常按客户端断线退出。
    ScopedPersistedPlayerCleanup cleanup;
    ScopedConfirmedNodeIdentity identity;
    DrainRelocateConfirms();
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player));
    BindLoginIdentity(player, kSessionAtExit, kCoveredGeneration);
    const auto exitBefore = exit_release_stats::Read();
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    ExpectExitedAsClientDisconnect(player, kSessionAtExit, exitBefore);
    EXPECT_EQ(BindingCount(release_fence_stats::Read(), rf::SessionEndedBinding::kCovered),
              BindingCount(before, rf::SessionEndedBinding::kCovered) + 1);
    DrainRelocateConfirms();
}

TEST(ReleaseFenceEcs, SessionEndedReleaseExitsOlderLoginOnLoginCollision)
{
    // 登录撞上未释放的持有者(§12):请求覆盖到发起这次登录的会话 (kNewerSession, g),实体还绑着 location 上更早的那次
    // 登录 (kSessionAtExit, g-1)。它已被这次登录取代,照常按客户端断线退出。
    ScopedPersistedPlayerCleanup cleanup;
    ScopedConfirmedNodeIdentity identity;
    DrainRelocateConfirms();
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player));
    BindLoginIdentity(player, kSessionAtExit, kCoveredGeneration - 1);
    const auto exitBefore = exit_release_stats::Read();
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch, kNewerSession, kCoveredGeneration);

    ExpectExitedAsClientDisconnect(player, kSessionAtExit, exitBefore);
    EXPECT_EQ(BindingCount(release_fence_stats::Read(), rf::SessionEndedBinding::kCovered),
              BindingCount(before, rf::SessionEndedBinding::kCovered) + 1);
    DrainRelocateConfirms();
}

TEST(ReleaseFenceEcs, SessionEndedReleaseSameGenerationOtherSessionIgnored)
{
    // 同代异会话违反 G2(一个代次只属于一条会话):分不清哪一份该退,退出是破坏性动作,不动。
    ScopedPersistedPlayerCleanup cleanup;
    DrainRelocateConfirms();
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player, kNewerSession));
    BindLoginIdentity(player, kNewerSession, kCoveredGeneration);
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    ExpectEntityUntouched(player, kNewerSession);
    EXPECT_EQ(BindingCount(release_fence_stats::Read(), rf::SessionEndedBinding::kSkipConflict),
              BindingCount(before, rf::SessionEndedBinding::kSkipConflict) + 1);
    DrainRelocateConfirms();
}

TEST(ReleaseFenceEcs, SessionEndedReleaseWithoutCoveredIdentityKeepsLegacyBehavior)
{
    // 旧版 scene_manager 不带覆盖身份(0 / 0):只比 owner_epoch,与本节之前一致 —— 实体绑着更新的登录也照常退出。
    // 这是滚动窗口里的已知行为(scene 先于 scene_manager 发布,§13.6),全量升级后 binding_unversioned_request 应恒为 0。
    ScopedPersistedPlayerCleanup cleanup;
    ScopedConfirmedNodeIdentity identity;
    DrainRelocateConfirms();
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch, player, kNewerSession));
    BindLoginIdentity(player, kNewerSession, kCoveredGeneration + 1);
    const auto exitBefore = exit_release_stats::Read();
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch, /*coveredSession=*/0, /*coveredGeneration=*/0);

    ExpectExitedAsClientDisconnect(player, kNewerSession, exitBefore);
    EXPECT_EQ(BindingCount(release_fence_stats::Read(), rf::SessionEndedBinding::kUnversionedRequest),
              BindingCount(before, rf::SessionEndedBinding::kUnversionedRequest) + 1);
    DrainRelocateConfirms();
}

TEST(ReleaseFenceEcs, SessionEndedBindingJudgedOnlyAfterEpochReleases)
{
    // 两道判定的顺序:代际不等(或交接在途)先挡下的释放不再比登录身份,binding_* 不计 —— 汇总行里的 binding_*
    // 因此读作「归属代际已放行之后」的分布。
    ScopedPersistedPlayerCleanup cleanup;
    entt::entity player{entt::null};
    ASSERT_TRUE(MakeReleaseTarget(kReleaseExpectedEpoch + 2, player));
    BindLoginIdentity(player, kSessionAtExit, kCoveredGeneration + 1);
    const auto before = release_fence_stats::Read();

    ReleaseEndedSession(kReleaseExpectedEpoch);

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    ExpectEntityUntouched(player, kSessionAtExit);
    const auto after = release_fence_stats::Read();
    EXPECT_EQ(ReleaseVerdictCount(after, rf::Verdict::kSkipStaleRelease),
              ReleaseVerdictCount(before, rf::Verdict::kSkipStaleRelease) + 1);
    EXPECT_EQ(after.bindingDecisions, before.bindingDecisions) << "代际先挡下:不做登录身份判定";
}

TEST(OwnerEpochStats, RejectedExitingSaveCountsAsRollbackEvidence)
{
    // 退出中的实体存盘被 owner CAS 拒(被拒的就是它当前缓存的那一代 = 已被废黜):退出存盘从未落地,差额随实体丢弃。
    // 这是回档的直接证据,单独计 stale_owner_rejected_exiting(总数 stale_owner_write_rejected 照旧 +1)。
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 6;
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);
    const auto before = owner_epoch_stats::Read();

    PlayerLifecycleSystem::HandlePlayerSaveRejected(kExitPlayerId, "player-save-test", "6", RedisSaveOwnerRejection{});

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player)) << "已被废黜:不存盘销毁";
    const auto after = owner_epoch_stats::Read();
    EXPECT_EQ(after.staleOwnerWriteRejected, before.staleOwnerWriteRejected + 1);
    EXPECT_EQ(after.staleOwnerRejectedExiting, before.staleOwnerRejectedExiting + 1);
    EXPECT_EQ(after.ownerCounterBehind, before.ownerCounterBehind) << "改派不是 Redis 回退";
    EXPECT_TRUE(after.Any());
}

TEST(OwnerEpochStats, RejectedLiveSaveIsNotCountedAsExiting)
{
    // 在线(没在退出)的实体被拒:只计总数,不计"退出中从未落地"。
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 6;
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);
    const auto before = owner_epoch_stats::Read();

    PlayerLifecycleSystem::HandlePlayerSaveRejected(kExitPlayerId, "player-save-test", "6", RedisSaveOwnerRejection{});

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
    const auto after = owner_epoch_stats::Read();
    EXPECT_EQ(after.staleOwnerWriteRejected, before.staleOwnerWriteRejected + 1);
    EXPECT_EQ(after.staleOwnerRejectedExiting, before.staleOwnerRejectedExiting);
}

TEST(OwnerEpochStats, CounterBehindDestroysWithoutSavingAndIsNotCountedAsDeposal)
{
    // G4(docs/design/zone-redis-regression-guard-20261022.md §4.4):Redis 的 owner 计数器(5)比实体缓存的 epoch(6)还小 =
    // Redis 回退丢了铸造。只检测、不修复:与废黜同样不存盘销毁(不写释放标记),单独计 owner_counter_behind;
    // 成因不是双主,stale_owner_write_rejected / stale_owner_rejected_exiting 都不动。用退出中的实体:退出存盘被拒最要紧。
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 6;
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);
    const auto before = owner_epoch_stats::Read();
    const auto exitBefore = exit_release_stats::Read();

    PlayerLifecycleSystem::HandlePlayerSaveRejected(kExitPlayerId, "player-save-test", "6",
        RedisSaveOwnerRejection{RedisSaveOwnerRejectReason::kOwnerCounterBehind, "5"});

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player)) << "只丢不盖:不存盘销毁";
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), entt::entity{entt::null});
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
    EXPECT_EQ(exit_release_stats::Read().decisions, exitBefore.decisions) << "不写释放标记、不发改派";
    const auto after = owner_epoch_stats::Read();
    EXPECT_EQ(after.ownerCounterBehind, before.ownerCounterBehind + 1);
    EXPECT_EQ(after.staleOwnerWriteRejected, before.staleOwnerWriteRejected);
    EXPECT_EQ(after.staleOwnerRejectedExiting, before.staleOwnerRejectedExiting);
    EXPECT_TRUE(after.Any()) << "[OwnerEpoch] 汇总行要因它而打出";
}

TEST(OwnerEpochStats, CounterBehindOnStaleInFlightSaveResavesWithCurrentEpoch)
{
    // 被拒的是旧期望值(6),实体已缓存更新的 epoch(7):与改派下的旧代际在途写同一处置 —— 用当前 epoch 重存,由 Lua 按同一判据
    // 重新回答(仍落后就再回计数器落后,那时两值相等才销毁),这里既不计数也不销毁。本宿主没有 Redis:去掉一个持久组件让重存
    // 在组装阶段被拒,在线实体保留(同 ExitPersistEcs.StaleEpochRejectOnOnlinePlayerKeepsEntity)。
    ScopedPersistedPlayerCleanup cleanup;
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    ASSERT_TRUE(MarkPersisted(player));
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 7;
    tlsEcs.actorRegistry.remove<PlayerAssetOpLedgerComp>(player);
    const auto before = owner_epoch_stats::Read();

    PlayerLifecycleSystem::HandlePlayerSaveRejected(kExitPlayerId, "player-save-test", "6",
        RedisSaveOwnerRejection{RedisSaveOwnerRejectReason::kOwnerCounterBehind, "5"});

    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player));
    EXPECT_EQ(tlsEcs.GetPlayer(kExitPlayerId), player);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 1u);
    const auto after = owner_epoch_stats::Read();
    EXPECT_EQ(after.ownerCounterBehind, before.ownerCounterBehind) << "当前 epoch 是否也落后要等重存的回答";
    EXPECT_EQ(after.staleOwnerWriteRejected, before.staleOwnerWriteRejected);
}

TEST(HandoffFenceCodec, PreservesUint64AndRejectsUnknownHolderMasqueradingAsAdmission)
{
    using namespace handoff_sql_barrier;
    const std::string a(32,'a'), b(32,'b');
    const auto raw=EncodeFence({UINT64_MAX,"scene",UINT64_MAX,a,FenceState::kHeld,b});
    ASSERT_TRUE(raw);const auto parsed=ParseFence(*raw);ASSERT_TRUE(parsed);
    EXPECT_EQ(UINT64_MAX,parsed->ownerEpoch);EXPECT_EQ(UINT64_MAX,parsed->generation);
    EXPECT_FALSE(ParseFence("18446744073709551616|scene|1|"+a+"|H|"+b));
    EXPECT_FALSE(ParseFence("7|scene|0|"+a+"|H|"+b));
    EXPECT_FALSE(ParseFence("7||0|"+std::string(32,'0')+"|H|"+b));
    EXPECT_TRUE(ParseFence("7||0|"+std::string(32,'0')+"|R|"+b));
    EXPECT_FALSE(ParseFence("7|scene|1|"+a+"|H|"+std::string(32,'0')));
    const auto changed=RotateFence(*raw,FenceState::kRevoked);ASSERT_TRUE(changed);EXPECT_NE(*raw,*changed);
    EXPECT_EQ(parsed->generation,ParseFence(*changed)->generation);
}

TEST(HandoffMarkWithdrawQueue, OldConfirmationCannotConsumeNewHolderEvenWithSameTimestamp)
{
    hmw::Queue queue;std::optional<hmw::Entry> evicted;
    ASSERT_TRUE(queue.Add(7,"7:100",WithdrawT0(),kWithdrawTtl,evicted,"old","oldF","oldR"));
    ASSERT_TRUE(queue.Add(7,"7:100",WithdrawT0(),kWithdrawTtl,evicted,"new","newF","newR"));
    EXPECT_TRUE(queue.Confirm(7,"7:100","old"));EXPECT_EQ(1u,queue.size());
    EXPECT_FALSE(queue.Confirm(7,"7:100","old"));EXPECT_TRUE(queue.HasPlayer(7));
    EXPECT_TRUE(queue.Confirm(7,"7:100","new"));EXPECT_EQ(0u,queue.size());
}

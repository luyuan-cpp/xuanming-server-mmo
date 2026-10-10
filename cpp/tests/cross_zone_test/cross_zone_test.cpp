#include <gtest/gtest.h>

#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/bag_marshal.h"
#include "proto/common/database/bag_quest_mail_data.pb.h"
#include "thread_context/ecs_context.h"

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
//   5. BagUnmarshalDropsBadType — bag_type >= kBagTypeCount in incoming
//                                 BagAllData is dropped with WARN, not
//                                 crashed.
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
//     needs a real CompletionQueue and the verify needs a zone Redis, so these are
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
    EXPECT_FALSE(queue.Add(kWithdrawPlayerA, "7:100", WithdrawT0() + std::chrono::seconds(100), kWithdrawTtl, evicted))
        << "同一份标记重复登记不算新条目";
    EXPECT_EQ(queue.size(), 1u);

    // 原 deadline = T0 + 305s。重复登记若把它推到了 T0 + 405s,这里就不会过期。
    std::size_t expired = 0;
    const auto due = queue.CollectDue(WithdrawT0() + kWithdrawTtl, /*force=*/false, expired);
    EXPECT_EQ(expired, 1u) << "截止时刻跟的是第一次登记,不得被重复登记延长";
    EXPECT_TRUE(due.empty());
    EXPECT_EQ(queue.size(), 0u);
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

TEST(HandoffMarkWithdrawQueue, CollectDueDropsExpiredAndCounts)
{
    hmw::Queue queue;
    std::optional<hmw::Entry> evicted;
    ASSERT_TRUE(queue.Add(kWithdrawPlayerA, "7:100", WithdrawT0(), kWithdrawTtl, evicted));

    std::size_t expired = 0;
    const auto due = queue.CollectDue(WithdrawT0() + kWithdrawTtl + std::chrono::seconds(1), /*force=*/true, expired);
    EXPECT_EQ(expired, 1u);
    EXPECT_TRUE(due.empty()) << "过了截止时刻的条目不再重发(force 也不行)";
    EXPECT_EQ(queue.size(), 0u);
    EXPECT_FALSE(queue.HasPlayer(kWithdrawPlayerA)) << "放弃之后闸口必须放开,否则玩家永远换不了图";
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

TEST(HandoffMarkWithdrawQueue, CapacityEvictsEarliestDeadline)
{
    hmw::Queue queue;
    std::optional<hmw::Entry> evicted;
    // 第 i 条在 T0 + i 秒登记,deadline 严格递增:player 1 的最早。
    for (std::size_t i = 0; i < hmw::kMaxPending; ++i)
    {
        ASSERT_TRUE(queue.Add(static_cast<uint64_t>(i + 1), "7:100",
                              WithdrawT0() + std::chrono::seconds(static_cast<long long>(i)), kWithdrawTtl, evicted));
        ASSERT_FALSE(evicted.has_value());
    }
    ASSERT_EQ(queue.size(), hmw::kMaxPending);

    const uint64_t overflowPlayer = static_cast<uint64_t>(hmw::kMaxPending) + 1;
    EXPECT_TRUE(queue.Add(overflowPlayer, "7:100",
                          WithdrawT0() + std::chrono::seconds(static_cast<long long>(hmw::kMaxPending)), kWithdrawTtl,
                          evicted));
    ASSERT_TRUE(evicted.has_value()) << "表满必须让调用方知道(要记 ERROR + withdraw_expired)";
    EXPECT_EQ(evicted->playerId, 1u) << "被淘汰的条目要带出来:日志得能定位到是哪个玩家";
    EXPECT_EQ(evicted->markValue, "7:100");
    EXPECT_EQ(queue.size(), hmw::kMaxPending);
    EXPECT_FALSE(queue.HasPlayer(1)) << "被淘汰的是 deadline 最早的那一条";
    EXPECT_TRUE(queue.HasPlayer(2));
    EXPECT_TRUE(queue.HasPlayer(overflowPlayer));
}

TEST(HandoffMarkWithdrawQueue, LuaIsConditionalDelete)
{
    // 撤回会被延迟重发,期间同一玩家可能已经写了新标记;退回无条件 DEL 会误删新标记。
    const std::string lua = hmw::kLuaDelIfEqual;
    EXPECT_NE(lua.find("GET"), std::string::npos);
    EXPECT_NE(lua.find("== ARGV[1]"), std::string::npos);
    EXPECT_NE(lua.find("DEL"), std::string::npos);
    EXPECT_LT(lua.find("GET"), lua.find("DEL")) << "先比对、后删除";
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
#include "proto/common/component/actor_comp.pb.h"            // Velocity / Acceleration
#include <limits>
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/system/player_data_loader.h"
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

TEST(ExitPersistCause, MergeSuppressesOnlyForIdentityConflictOrUnspecified)
{
    for (const auto first : kAllExitCauses)
    {
        for (const auto incoming : kAllExitCauses)
        {
            PlayerExitIntentComp intent;
            intent.cause = first;
            player_exit::MergeExitCause(intent, incoming, /*devBypassSuppressesTransfer=*/false);
            const bool expectSuppressed =
                incoming == ExitCause::kIdentityConflict || incoming == ExitCause::kUnspecified;
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
    EXPECT_FALSE(player_exit::IsDeposedOnReentry(std::numeric_limits<uint64_t>::max(), 1))
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

// 构造一个"退出存盘在途"的玩家:当前绑定 currentSession(已写进 SessionMap、映射到本玩家)、
// 挂 UnregisterPlayer + 退出意图(sessionAtExit)。logout_initiated_ms 留 0 = 跳过租约告警。
entt::entity MakeExitingPlayer(SessionId sessionAtExit, SessionId currentSession, bool withIntent = true)
{
    tlsEcs.Clear();
    SessionMap().clear(); // SessionMap 不随 tlsEcs.Clear() 清,不清的话用例之间互相依赖执行顺序
    const auto player = tlsEcs.actorRegistry.create();
    tlsEcs.playerList.emplace(kExitPlayerId, player);
    tlsEcs.actorRegistry.emplace<Guid>(player, kExitPlayerId);
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

// 与 SavePlayerToRedis 写盘时同一个形状:marshal + 两张子表的 player_id。
PlayerAllData MarshalAsSaved(entt::entity player)
{
    PlayerAllData data;
    PlayerAllDataMessageFieldsMarshal(player, data);
    data.mutable_player_database_data()->set_player_id(kExitPlayerId);
    data.mutable_player_database_1_data()->set_player_id(kExitPlayerId);
    return data;
}
} // namespace

TEST(ExitPersistEcs, ExitSessionStillMappedIsDestroyed)
{
    // Z1:退出会话仍在 SessionMap 里、映射到本玩家(HandleExitGameNode 不解绑会话),落地内容就是当前内存。
    // 修复前这里判"被取代"、摘标记后 return,实体成僵尸。
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    PlayerAllData landed = MarshalAsSaved(player);

    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player)) << "真写盘的正常断线退出必须销毁实体";
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u) << "退出会话的映射随收尾一起摘掉";
}

TEST(ExitPersistEcs, LandedPayloadWithProbeStillConverges)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    PlayerAllData landed = MarshalAsSaved(player);
    landed.mutable_player_database_data()->mutable_stress_test_probe()->set_test_seq(1);
    landed.mutable_player_database_1_data()->mutable_stress_test_probe()->set_test_seq(1);

    PlayerLifecycleSystem::HandlePlayerAsyncSaved(kExitPlayerId, landed);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player));
}

TEST(ExitPersistEcs, NewerSessionKeepsEntityAndClearsExitMarkers)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kNewerSession);
    PlayerAllData landed = MarshalAsSaved(player);

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
    PlayerAllData landed = MarshalAsSaved(player);
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
    PlayerAllData landed = MarshalAsSaved(player);

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
    tlsEcs.playerList.emplace(kExitPlayerId, player);
    tlsEcs.actorRegistry.emplace<Guid>(player, kExitPlayerId);
    auto& snapshot = tlsEcs.actorRegistry.emplace<PlayerSessionSnapshotComp>(player);
    snapshot.set_gate_session_id(session);
    snapshot.set_player_id(kExitPlayerId);
    SessionMap().insert_or_assign(session, kExitPlayerId);
    return player;
}

// 让下一次 SavePlayerToRedis 判"盘上已是同一份"(快路径,不碰 Redis)。
void MarkPersisted(entt::entity player)
{
    tlsEcs.actorRegistry.emplace_or_replace<PlayerLastPersistedSnapshotComp>(player).Replace(MarshalAsSaved(player));
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
    auto& acceleration = tlsEcs.actorRegistry.emplace<Acceleration>(player);
    acceleration.set_x(1.0);

    PlayerLifecycleSystem::StopMotionForExit(player);

    const auto& v = tlsEcs.actorRegistry.get<Velocity>(player);
    EXPECT_EQ(v.x(), 0.0);
    EXPECT_EQ(v.y(), 0.0);
    EXPECT_EQ(v.z(), 0.0);
    EXPECT_EQ(tlsEcs.actorRegistry.get<Acceleration>(player).x(), 0.0)
        << "MovementAccelerationSystem 积分 (Velocity + Acceleration),只清速度不够";

    const auto bare = tlsEcs.actorRegistry.create();
    PlayerLifecycleSystem::StopMotionForExit(bare);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<Velocity>(bare)) << "只改已有组件,不 emplace";
}

TEST(ExitPersistEcs, FirstExitOnFastPathFinishesInline)
{
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    MarkPersisted(player);

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kClientDisconnect);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player)) << "盘上已是当前内存、没有未落地存盘:同步收尾";
    EXPECT_EQ(tlsEcs.playerList.count(kExitPlayerId), 0u);
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
}

TEST(ExitPersistEcs, ReexitMergesCauseWithoutFinishing)
{
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    MarkPersisted(player);

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
    MarkPersisted(player);

    PlayerLifecycleSystem::HandleExitGameNode(player, ExitCause::kNodeShutdown);

    EXPECT_FALSE(tlsEcs.actorRegistry.valid(player))
        << "超限保留的实体再收到退出请求:补发的存盘判盘上已是同一份、且没有未落地存盘 → 收尾(停机时的最后出口)";
    EXPECT_EQ(SessionMap().count(kSessionAtExit), 0u);
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
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kInteger, 2), R::kFailed);
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kNone, 1), R::kFailed) << "空应答:结果未知,计失败";
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kError, 0), R::kFailed);
    EXPECT_EQ(erm::ClassifyMarkWriteReply(erm::ReplyShape::kOther, 1), R::kFailed);
}

TEST(ExitReleaseMarkWrite, LuaChecksOwnerEpochBeforeSet)
{
    const std::string lua = erm::kLuaWriteIfOwnerEpoch;
    const auto compare = lua.find("redis.call('GET', KEYS[1]) ~= ARGV[1]");
    ASSERT_NE(compare, std::string::npos) << "先比 owner_epoch";
    const auto set = lua.find("redis.call('SET', KEYS[2]");
    ASSERT_NE(set, std::string::npos);
    EXPECT_LT(compare, set) << "比对在写之前";
    EXPECT_LT(lua.find("return 0"), set) << "核对不过直接返回 0、不写";
    EXPECT_NE(lua.find("'EX', ARGV[3]"), std::string::npos) << "标记必须带 TTL";
}

TEST(ExitReleaseInheritClear, LuaComparesOwnerEpochFirstAndOnlyDeletesUpToN)
{
    const std::string lua = erm::kLuaInheritClear;
    const auto compare = lua.find("if cur ~= ARGV[1] then return -1 end");
    ASSERT_NE(compare, std::string::npos) << "先比 owner_epoch,核对不过返回 -1";
    EXPECT_NE(lua.find("if cur == false then cur = '0' end"), std::string::npos) << "owner_epoch 缺键按 0";
    const auto firstDel = lua.find("DEL");
    ASSERT_NE(firstDel, std::string::npos);
    EXPECT_LT(compare, firstDel) << "核对不过时一个标记都不删(M2)";
    const auto keepNewer = lua.find("then return 3 end");
    const auto lastDel = lua.rfind("redis.call('DEL', KEYS[2])");
    ASSERT_NE(keepNewer, std::string::npos);
    EXPECT_LT(keepNewer, lastDel) << "代际比 N 新的标记先返回 3、不删:只删 ≤N";
    EXPECT_EQ(lua.find("tonumber"), std::string::npos) << "uint64 代际不能过 tonumber(2^53 以上丢精度)";
}

TEST(ExitReleaseInheritClear, UnknownEpochLuaNeverRefusesAndOnlyDeletesUpToCurrent)
{
    // 复审 ownership-major:路由不带 owner_epoch 时照样清,以 Redis 里当前的 owner_epoch 代替 N。
    const std::string lua = erm::kLuaInheritClearUnknownEpoch;
    EXPECT_EQ(lua.find("return -1"), std::string::npos) << "无从核对归属:不返回 -1";
    EXPECT_EQ(lua.find("ARGV"), std::string::npos) << "没有 N 可传";
    EXPECT_NE(lua.find("if cur == false then cur = '0' end"), std::string::npos) << "owner_epoch 缺键按 0";
    const auto keepNewer = lua.find("then return 3 end");
    const auto lastDel = lua.rfind("redis.call('DEL', KEYS[2])");
    ASSERT_NE(keepNewer, std::string::npos);
    ASSERT_NE(lastDel, std::string::npos);
    EXPECT_LT(keepNewer, lastDel) << "代际比当前新的标记先返回 3、不删:只删 ≤ 当前代际";
    EXPECT_EQ(lua.find("tonumber"), std::string::npos) << "uint64 代际不能过 tonumber";
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
    pending[kReleasePlayerId] = ctx;
}
} // namespace

TEST(ExitReleaseEcs, ConvergedExitWithoutRedisCountsFailedAndLeavesNothingInFlight)
{
    // M15:无 Redis 宿主里 A1′ 判定为写后必须走"写不成"分支、不增加在途计数;实体照常销毁。
    PlayerLifecycleSystem::SetNodeIdentityProbe([] { return true; });
    const auto player = MakeExitingPlayer(kSessionAtExit, kSessionAtExit);
    tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 5;
    PlayerAllData landed = MarshalAsSaved(player);
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
    PlayerAllData landed = MarshalAsSaved(player);
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

    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kReleasePlayerId, PlayerAllData{});

    EXPECT_EQ(PlayerLifecycleSystem::GetPendingEnterMap().count(kReleasePlayerId), 0u) << "待入场条目已擦掉";
    EXPECT_EQ(tlsEcs.playerList.count(kReleasePlayerId), 0u) << "没有建实体";
    EXPECT_EQ(exit_release_stats::Read().inheritRefused, before.inheritRefused + 1);
}

TEST(ExitReleaseEcs, RouteWithoutOwnerEpochStillWaitsForTheClear)
{
    // 复审 ownership-major:旧版路由(owner_epoch 0)以前跳过 A2′、闸门直接放行,上一任的 A1′ 标记因此存活。
    RegisterPendingEnter(/*ownerEpoch=*/0);
    const auto before = exit_release_stats::Read();
    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kReleasePlayerId, PlayerAllData{});
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
    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kReleasePlayerId, PlayerAllData{});
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

    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kReleasePlayerId, PlayerAllData{});
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
#include "network/node_utils.h" // GetNodeInfo:tlsEcs.Clear() 会清 globalRegistry,zone 要在每个用例里重设
#include "time/system/time.h"   // TimeSystem::NowMillisecondsUTC:在途换图组件的 sentAtMs
#include "thread_context/node_context_manager.h" // 假 scene_manager 节点(StartTravelHandoff 的 SM 预检)

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
// 宿主里没有 zone Redis(tlsRedis.GetZoneRedis() 为空)也没有 EventLoop:ResolveTravelOutcome 判不清就保持冻结、
// 计 verify_rearmed,看门狗挂不上(只打 WARN)。所以"应答进了交接裁决"的可观察结果是证据被记下 + verify_rearmed +1。
namespace
{
constexpr uint32_t kReplyTestZone = 7;
constexpr uint32_t kReplyTargetZone = 8;
constexpr uint32_t kSmErrSomeRejection = 5; // 任意非 18 的 scene_manager 错误码

// 在线玩家 + 本节点 zone + "盘上已是同一份"(SavePlayerToRedis 走快路径,不碰 Redis)。
// zone 必须在 MakeOnlinePlayer 之后设:它里面的 tlsEcs.Clear() 会清 globalRegistry,GetZoneId() 回到 0,
// 而 StartTravelHandoff 对 targetZoneId == 0 直接拒绝。
entt::entity MakeReplyTestPlayer()
{
    const auto player = MakeOnlinePlayer(kSessionAtExit);
    GetNodeInfo().set_zone_id(kReplyTestZone);
    MarkPersisted(player);
    return player;
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
// 所以要在改状态之前挡住)。宿主里没有 SM 节点,凡是要真正起交接的用例都得临时登记一个假的:只挂 NodeInfo,
// 够 GetSceneManagerEntity 挑中即可。作用域结束即销毁 —— 假实体上没有 gRPC 客户端,别的用例若走到真的发送点,
// 不能让它被挑中。这些用例都在 BeginTravelHandoff 的 epoch / Redis 检查处就地 Abort,走不到发送点。
class ScopedFakeSceneManagerNode
{
public:
    ScopedFakeSceneManagerNode()
    {
        auto& registry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
        entity_ = registry.create();
        registry.emplace<NodeInfo>(entity_);
    }
    ~ScopedFakeSceneManagerNode()
    {
        tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService).destroy(entity_);
    }
    ScopedFakeSceneManagerNode(const ScopedFakeSceneManagerNode&) = delete;
    ScopedFakeSceneManagerNode& operator=(const ScopedFakeSceneManagerNode&) = delete;

private:
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
} // namespace

TEST(EnterSceneReplyEcs, ForeignTaggedReplyDuringHandoffLeavesEvidenceUntouched)
{
    // 今天的缺陷:交接在途时任何一条 EnterScene 应答都照单全收 —— 污染证据、DEL 撤回标记、跨 zone 还可能踢线。
    const auto player = MakeReplyTestPlayer();
    AttachHandoff(player, /*correlationId=*/42);
    const auto before = travel_handoff_stats::Read();

    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 41));
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, 0, 41));
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, 0, 41, /*withRedirect=*/true));

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
    const auto player = MakeReplyTestPlayer();
    AttachHandoff(player, /*correlationId=*/0);
    const auto before = travel_handoff_stats::Read();

    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 0));

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
    const auto player = MakeReplyTestPlayer();
    AttachHandoff(player, /*correlationId=*/42);
    const auto before = travel_handoff_stats::Read();

    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 42));

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
    const auto player = MakeReplyTestPlayer();
    AttachHandoff(player, /*correlationId=*/42);
    const auto before = travel_handoff_stats::Read();

    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 0));

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
    const auto player = MakeReplyTestPlayer();
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/true, /*correlationId=*/9);
    const auto before = travel_handoff_stats::Read();

    PlayerLifecycleSystem::DispatchEnterSceneReply(
        MakeReply(kExitPlayerId, PlayerLifecycleSystem::kSmErrHandoffPending, 10));
    const auto afterForeign = travel_handoff_stats::Read();
    EXPECT_EQ(afterForeign.started, before.started) << "别的请求的 18 不起交接";
    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
    ASSERT_NE(pending, nullptr) << "对不上号的应答不得摘掉在途记录";
    EXPECT_EQ(pending->correlationId, 9u);
    EXPECT_EQ(afterForeign.replyUnmatched, before.replyUnmatched + 1);

    PlayerLifecycleSystem::DispatchEnterSceneReply(
        MakeReply(kExitPlayerId, PlayerLifecycleSystem::kSmErrHandoffPending, 9));
    const auto after = travel_handoff_stats::Read();
    EXPECT_EQ(after.started, before.started + 1) << "号匹配的 18 起同 zone 交接";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    // 宿主里没有 owner_epoch(按 0):BeginTravelHandoff 就地 Abort,交接与冻结成对摘掉。
    EXPECT_EQ(after.aborted, before.aborted + 1);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
}

TEST(EnterSceneReplyEcs, LegacySceneChange18StillStartsHandoff)
{
    // 旧版 SM 不回显号:§11.2 的 18 链照样接得上(按 player_id 退回)。
    const ScopedFakeSceneManagerNode sceneManager; // StartTravelHandoff 的 SM 预检
    const auto player = MakeReplyTestPlayer();
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/true, /*correlationId=*/9);
    const auto before = travel_handoff_stats::Read();

    PlayerLifecycleSystem::DispatchEnterSceneReply(
        MakeReply(kExitPlayerId, PlayerLifecycleSystem::kSmErrHandoffPending, 0));

    const auto after = travel_handoff_stats::Read();
    EXPECT_EQ(after.started, before.started + 1);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated + 1);
}

TEST(EnterSceneReplyEcs, StaleFollowReplyDoesNotConsumeNewerRequest)
{
    // 过了 TTL 才到的上一条请求的应答,今天会摘掉新请求的在途记录。
    const auto player = MakeReplyTestPlayer();
    AttachSceneChange(player, /*sceneId=*/2, /*playerRequested=*/false, /*correlationId=*/20);
    const auto before = travel_handoff_stats::Read();

    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 19));
    {
        const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
        ASSERT_NE(pending, nullptr);
        EXPECT_EQ(pending->sceneId, 2u);
        EXPECT_EQ(pending->correlationId, 20u);
    }
    const auto afterRejected = travel_handoff_stats::Read();
    EXPECT_EQ(afterRejected.replyUnmatched, before.replyUnmatched + 1) << "对不上号的拒绝算可疑";

    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, 0, 19));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    EXPECT_EQ(travel_handoff_stats::Read().replyUnmatched, afterRejected.replyUnmatched)
        << "路由先到之后的纯成功迟到应答是常态,不计数";

    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 20));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player)) << "自己的应答摘组件";
    EXPECT_EQ(travel_handoff_stats::Read().started, before.started) << "跟随被拒只记日志,不起交接";
}

TEST(EnterSceneReplyEcs, NoWaiterAndGonePlayerAreSilent)
{
    const auto player = MakeReplyTestPlayer();
    const auto before = travel_handoff_stats::Read();

    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, 0, 5));
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, kSmErrSomeRejection, 5));
    // 不在本节点的玩家。
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId + 1, kSmErrSomeRejection, 6));
    // 更老的 SM:连 player_id 都没有。
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(0, kSmErrSomeRejection, 0));

    const auto after = travel_handoff_stats::Read();
    EXPECT_TRUE(tlsEcs.actorRegistry.valid(player));
    EXPECT_EQ(after.replyUnmatched, before.replyUnmatched);
    EXPECT_EQ(after.replyUncorrelated, before.replyUncorrelated)
        << "player_id 为 0 的应答在计 reply_uncorrelated 之前就返回";
}

// ----------------------------------------------------------------------------
// EnterScene 传输失败(PlayerLifecycleSystem::DispatchEnterSceneTransportFailure,
// docs/design/grpc-client-deadline-failure-callback.md §5 #1 / #2)。修复前生成的 gRPC 客户端对非 OK 只打日志,
// 这些路径根本走不到:普通换图的槽位只能等 TTL 过期,客户端收不到任何提示。
// ----------------------------------------------------------------------------
#include "node/system/grpc_call_deadline.h" // 在途换图 TTL 从 SceneManager 的 deadline 派生

namespace
{
::scene_manager::EnterSceneRequest MakeFailedRequest(Guid playerId, uint64_t correlationId)
{
    ::scene_manager::EnterSceneRequest req;
    req.set_player_id(playerId);
    req.set_correlation_id(correlationId);
    return req;
}

constexpr char kTransportFailureReason[] = "SceneManager.EnterScene code=4 msg=Deadline Exceeded";
} // namespace

TEST(EnterSceneTransportFailureEcs, HandoffFailureIsNotEvidence)
{
    // 结果未知:scene_manager 可能已放行。不解冻、不记证据、不提前核实,留给应答看门狗 / 冻结上限。
    const auto player = MakeReplyTestPlayer();
    AttachHandoff(player, /*correlationId=*/42);
    const auto before = travel_handoff_stats::Read();

    PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(MakeFailedRequest(kExitPlayerId, 42),
                                                              kTransportFailureReason);

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
    const auto player = MakeReplyTestPlayer();
    AttachSceneChange(player, /*sceneId=*/777, /*playerRequested=*/true, /*correlationId=*/9);
    const auto before = travel_handoff_stats::Read();

    // 别的请求(被顶替的)的失败不动当前等待者。
    PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(MakeFailedRequest(kExitPlayerId, 8),
                                                              kTransportFailureReason);
    {
        const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
        ASSERT_NE(pending, nullptr);
        EXPECT_EQ(pending->correlationId, 9u);
    }

    PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(MakeFailedRequest(kExitPlayerId, 9),
                                                              kTransportFailureReason);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player)) << "自己的失败摘槽位";
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "玩家可以立刻重试";
    const auto after = travel_handoff_stats::Read();
    EXPECT_EQ(after.started, before.started) << "传输失败不是 18,不起交接";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
}

TEST(EnterSceneTransportFailureEcs, FollowFailureReleasesTheSlotToo)
{
    const auto player = MakeReplyTestPlayer();
    AttachSceneChange(player, /*sceneId=*/2, /*playerRequested=*/false, /*correlationId=*/20);

    PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(MakeFailedRequest(kExitPlayerId, 20),
                                                              kTransportFailureReason);

    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
}

TEST(EnterSceneTransportFailureEcs, UncorrelatedFailureNeverFallsBackToPlayerId)
{
    // 应答号为 0 会退回按 player_id(旧版 SM);失败号为 0 只能是绕过统一出口的发送,不能照搬那条退路。
    const auto player = MakeReplyTestPlayer();
    AttachSceneChange(player, /*sceneId=*/2, /*playerRequested=*/true, /*correlationId=*/20);

    PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(MakeFailedRequest(kExitPlayerId, 0),
                                                              kTransportFailureReason);
    // 不在本节点的玩家:静默。
    PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(MakeFailedRequest(kExitPlayerId + 1, 21),
                                                              kTransportFailureReason);

    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
    ASSERT_NE(pending, nullptr);
    EXPECT_EQ(pending->correlationId, 20u);
}

TEST(EnterSceneTransportFailureEcs, InFlightTtlCoversTheSceneManagerDeadline)
{
    // 上游比下游宽:deadline 到期时失败通知才到,槽位必须还在;TTL 只兜"完成通知永远不来"。
    const auto player = MakeReplyTestPlayer();
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
    GetNodeInfo().set_zone_id(kCapSelfZone); // 必须在 Clear 之后:Clear 会清 globalRegistry,GetZoneId() 回到 0
    const auto player = tlsEcs.actorRegistry.create();
    tlsEcs.playerList.emplace(kCapPlayerId, player);
    tlsEcs.actorRegistry.emplace<Guid>(player, kCapPlayerId);
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

TEST(TravelFreezeCapEcs, MarkNeverSentUnfreezesWithoutWithdrawal)
{
    const auto player = MakeFrozenHandoffPlayer(kCapSelfZone + 1, /*requestedAtMs=*/0, /*markEpoch=*/0, kT0);
    const auto before = Stats();

    PlayerLifecycleSystem::EnforceTravelFreezeCaps(kT0 + tfc::kFreezeCap);

    const auto after = Stats();
    ASSERT_TRUE(tlsEcs.actorRegistry.valid(player)) << "SET 没发出:不可能已被放行,解冻而不是销毁";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_EQ(after.freezeCapReached, before.freezeCapReached + 1);
    EXPECT_EQ(after.aborted, before.aborted + 1);
    EXPECT_EQ(after.markSentDestroyed, before.markSentDestroyed);
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "没发过标记,不登记撤回";
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
    tlsEcs.actorRegistry.emplace<Guid>(reentered, kCapPlayerId);
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

TEST(TravelFreezeCapEcs, BeginTravelHandoffPastDispatchWindowAbortsWithoutMark)
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
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_EQ(after.dispatchWindowClosed, before.dispatchWindowClosed + 1);
    EXPECT_EQ(after.aborted, before.aborted + 1);
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "SET 没发出,不登记撤回";
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
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_EQ(after.aborted, before.aborted + 1);
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
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player));
    EXPECT_EQ(Stats().aborted, before.aborted + 1);
    EXPECT_FALSE(PlayerLifecycleSystem::IsSceneChangeBusy(player)) << "markEpoch 已清 0:不登记撤回";

    // 实体已不在时同样 no-op(不崩、不计数)。
    tlsEcs.Clear();
    PlayerLifecycleSystem::HandleTravelMarkWriteRejected(kCapPlayerId, /*requestedAtMs=*/456, "READONLY");
    EXPECT_EQ(Stats().aborted, before.aborted + 1);
}

// ============================================================================
// 13. TravelOwnership —— GO-2 根治的源端判定(player_lifecycle.h 的 travel_outcome::ClassifyOwnership 与
// kLuaJudgeTravelOutcome;cross-zone-scene-travel.md §12.8 判定表 B4–B9)。纯函数,不需要 Redis / 场景宿主。
// 取证脚本在 Redis 上的行为(只删本族标记、读到回执、与 A2′ / 回滚 Lua 的互斥)由 Go 侧跨语言金样
// go/scene_manager/internal/logic/owner_epoch_crosslang_test.go 在 miniredis 上覆盖;采纳路径的 ECS 级行为
// (采纳 → 解冻 → 强制存盘)要 Redis 应答才走得到,只能靠 runbook B4a / B4b 实跑。
// ============================================================================
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
        const char *name;
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

TEST(TravelOwnership, JudgeScriptKeepsShebangAndMget)
{
    const std::string lua = to::kLuaJudgeTravelOutcome;
    EXPECT_EQ(lua.rfind("#!lua\n", 0), 0u)
        << "必须以 #!lua 开头:只读副本 / MISCONF / OOM 下整体被拒,不会出现删除失败而读取成功";
    EXPECT_EQ(lua.find("no-writes"), std::string::npos) << "不得声明 no-writes:那会让只读副本上照样读成功";
    EXPECT_EQ(lua.find("'GET'"), std::string::npos) << "只用 MGET:GET 遇到类型不对的键会抛 WRONGTYPE,取证永远出不来";
    EXPECT_EQ(lua.find("'SET'"), std::string::npos) << "取证只删不写";
    const auto del = lua.find("redis.call('DEL', KEYS[1])");
    const auto read = lua.find("return redis.call('MGET', KEYS[2], KEYS[3])");
    ASSERT_NE(del, std::string::npos);
    ASSERT_NE(read, std::string::npos);
    EXPECT_LT(del, read) << "先删本族标记,再在同一段脚本里读 epoch 与 location";
    EXPECT_NE(lua.find("string.match(v, '^%d+:(%d+)$') == ARGV[1]"), std::string::npos)
        << "只删后缀(saved_at_ms)与本次 requestedAtMs 逐字节相同的那一族,别人的标记不碰";
    EXPECT_EQ(lua.find("DEL', KEYS[2]"), std::string::npos) << "不删 owner_epoch";
    EXPECT_EQ(lua.find("DEL', KEYS[3]"), std::string::npos) << "不删 location";
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
#include "modules/scene/comp/scene_node_comp.h" // ScenePlayers
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
    // 记法:(evacuating, reentrySessionBound, reentryIsTicketSession)。进场路由把玩家带回本节点时,还没派发的票据
    // 作废不作废。
    static_assert(rc::CancelsTicketOnReentry(false, true, true), "判定必须能在编译期求值(纯函数)");
    // 平时一律作废:那次退出不会再正常收尾,留着票据只会在下一次、会话已换的退出里被误消费。
    for (const bool bound : {false, true})
    {
        for (const bool sameSession : {false, true})
        {
            EXPECT_TRUE(rc::CancelsTicketOnReentry(false, bound, sameSession))
                << "bound=" << bound << " same_session=" << sameSession;
        }
    }
    // 整节点疏散中,只有「票据那同一条有效会话」的进场留着票据:进场若取消了他的退出,他留在将死的节点上,而疏散
    // 只发一轮票,删掉的话既不改派也不踢。
    EXPECT_FALSE(rc::CancelsTicketOnReentry(true, true, true));
    EXPECT_TRUE(rc::CancelsTicketOnReentry(true, true, false)) << "另一条有效会话:票据里的旧会话已死";
    // 会话 0 / 无效会话必须在这里作废:进场会把实体的会话快照清成无效,而 DecideTicket 把「没有会话」当成
    // 「不是换了会话的证据」照发 —— 留着就会按票据里的旧会话改派。
    EXPECT_TRUE(rc::CancelsTicketOnReentry(true, false, false));
    EXPECT_TRUE(rc::CancelsTicketOnReentry(true, false, true)) << "会话无效时「相等」没有意义,同样作废";
    EXPECT_EQ(rc::DecideTicket(false, false, false), rc::TicketDecision::kDispatch) << "上一条注释依赖的前提";
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

    // ⑤ 第三个入参的来源:PlayerLifecycleSystem::SmRejectedBeforeAnyPlacementWrite 的白名单 —— 这八个码在 scene_manager 里的
    //    每一个出处都早于本请求的落点写。数值手抄自 go/scene_manager/internal/constants/errors.go(两边没有共享的生成物),
    //    钉在这里:那边改号而 player_lifecycle.h 没跟,盲踢就会打到已放行的会话。18 引用既有常量,不再写字面量。
    struct PinnedSmCode
    {
        uint32_t value;
        uint32_t expected;
        const char *goName;
    };
    const PinnedSmCode pinnedCodes[] = {
        {PlayerLifecycleSystem::kSmErrNoAvailableNode, 1, "ErrNoAvailableNode"},
        {PlayerLifecycleSystem::kSmErrInvalidNodeID, 4, "ErrInvalidNodeID"},
        {PlayerLifecycleSystem::kSmErrInvalidGateID, 5, "ErrInvalidGateID"},
        {PlayerLifecycleSystem::kSmErrRedis, 8, "ErrRedis"},
        {PlayerLifecycleSystem::kSmErrSceneReentryBarrier, 17, "ErrSceneReentryBarrier"},
        {PlayerLifecycleSystem::kSmErrHomeZoneUnavailable, 20, "ErrHomeZoneUnavailable"},
        {PlayerLifecycleSystem::kSmErrHomeZoneMerging, 21, "ErrHomeZoneMerging"},
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
// 不得在这些用例里构造 ScopedFakeSceneManagerNode:假节点只挂 NodeInfo,会被 GetSceneManagerEntity 挑中,真正的发送点
// 随后对它取 CompletionQueue 而断言。
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
    // 兜底:以后给 Snapshot 加了字段而上面没跟着逐项列出时,这一条会红(上面各项都绿、只有它红 = 漏列了新字段)。
    EXPECT_TRUE(actual == expected) << "relocate_confirm_stats::Snapshot 有 ExpectRelocateStats 没逐项列出的字段";
}

// 作用域内把节点身份探针置为「确认有效」;离开作用域(含 ASSERT 失败提前返回)一律复位成「未注入 = 不确认」。
// 探针是进程级的,不复位会改变后面所有走 A1′ 的用例的判定(--gtest_shuffle 下尤其难查)。
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

// 把玩家放进一个场景:sceneRegistry 里建场景实体(SceneInfoComp.scene_id + ScenePlayers),玩家挂 SceneEntityComp ——
// BeginSceneDrain(遍历 ScenePlayers)与 HandleExitGameNode(抄 sceneIdAtExit)要看到的最小形状。返回场景实体。
// 必须在 MakeOnlinePlayer / MakeExitingPlayer 之后调:它们里面的 tlsEcs.Clear() 会清 sceneRegistry。
entt::entity PlaceInScene(entt::entity player, uint64_t sceneId)
{
    const auto scene = tlsEcs.sceneRegistry.create();
    tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_id(sceneId);
    tlsEcs.sceneRegistry.emplace<ScenePlayers>(scene).insert(player);
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
    MarkPersisted(player); // 最后做:快照要与退出时再 marshal 出来的那一份逐字节相同
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
    entt::entity scene{entt::null};
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
    EXPECT_EQ(entry->sourceSceneId, kRelocateSourceScene) << "源场景在 DetachFromScene 之前抄进意图组件";
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
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, 0, 77));
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, 7, 77));
    ExpectRelocateStats(RelocateStats(), before);

    // ② 号为 0(旧版 scene_manager):按 player_id 认领后忽略 —— 条目已在核实,结论由核实给。
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, 0, 0));
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, 7, 0));
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

    PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(MakeFailedRequest(kExitPlayerId, 77),
                                                              kTransportFailureReason);
    // 号为 0 的失败在分发入口就被丢弃(只能来自绕过统一出口的发送)。
    PlayerLifecycleSystem::DispatchEnterSceneTransportFailure(MakeFailedRequest(kExitPlayerId, 0),
                                                              kTransportFailureReason);

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
    PlayerLifecycleSystem::DispatchEnterSceneReply(MakeReply(kExitPlayerId, 0, 0));
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
    PlayerAllData landed = MarshalAsSaved(made.player);
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
    PlayerAllData landed = MarshalAsSaved(made.player);
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
    PlayerAllData landed = MarshalAsSaved(made.player);
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
        MarkPersisted(player);
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
        MarkPersisted(player);
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
        MarkPersisted(player);
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

    PlayerAllData landed = MarshalAsSaved(made.player);
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
    PlayerAllData landed = MarshalAsSaved(made.player);
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
// 还原这一步用 bag_marshal::Unmarshal 而不是 PlayerAllDataMessageFieldsUnMarshal:本测试宿主不加载
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
    {
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(source);

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
        bags.bags[kEquipment].InsertItemForRestore(worn, 5);

        // 背包里的一件:已初始化但一条都没掷出,只有 presence。
        ItemComp spare;
        spare.set_item_id(kSpareGuid);
        spare.set_config_id(3002);
        spare.set_size(1);
        spare.mutable_equip();
        bags.bags[kInventory].InsertItemForRestore(spare, 2);

        // 普通物品:没有 equip 段。
        ItemComp potion;
        potion.set_item_id(kPotionGuid);
        potion.set_config_id(1001);
        potion.set_size(7);
        bags.bags[kInventory].InsertItemForRestore(potion, 3);
    }

    PlayerAllData saved;
    PlayerAllDataMessageFieldsMarshal(source, saved);
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
            }
            else if (entry.item_uuid() == kSpareGuid)
            {
                EXPECT_TRUE(entry.has_equip()) << which << ": zero rows is still 'already rolled'";
                EXPECT_EQ(entry.equip().affixes_size(), 0) << which;
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
// Test bootstrap
// ============================================================================
int main(int argc, char** argv)
{
    ::testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}

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
// Test bootstrap
// ============================================================================
int main(int argc, char** argv)
{
    ::testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}

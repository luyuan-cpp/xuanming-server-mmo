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
        << "改派自己写了标记,A1′ 不得覆盖";
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
// Test bootstrap
// ============================================================================
int main(int argc, char** argv)
{
    ::testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}

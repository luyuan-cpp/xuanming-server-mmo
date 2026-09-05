// aoi_system_test.cpp — AOI (Area of Interest) system tests
//
// Verifies:
//   1. Hex-grid coordinate mapping and neighbor lookup
//   2. Entity-to-grid assignment after random movement
//   3. Round-trip movement across all six hex neighbors
//   4. Entity enter/leave visibility via AoiListComp
//   5. Priority tags, capacity limits, pin/unpin
//   6. Dynamic capacity (client-reported + server pressure)
//   7. Per-scene priority policies (open-world, dungeon, PvP)

#include "spatial/system/aoi.h"
#include <gtest/gtest.h>
#include "hexagons_grid.h"
#include "spatial/comp/grid_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/constants/aoi_priority.h"
#include "spatial/system/grid.h"
#include "spatial/system/interest.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/team_comp.pb.h"
#include "proto/common/event/scene_event.pb.h"
#include "modules/scene/comp/scene_comp.h"

#include "engine/core/type_define/type_define.h"
#include "engine/core/utils/random/random.h"
#include "thread_context/redis_manager.h"
#include <thread_context/ecs_context.h>
#include <proto/scene/player_scene.pb.h>

extern const Point kDefaultSize(20.0, 20.0);
extern const Point kOrigin(0.0, 0.0);
extern const auto kHexLayout = Layout(layout_flat, kDefaultSize, kOrigin);

// ---------------------------------------------------------------------------
// Helper: register / unregister global message components used by AoiSystem
// ---------------------------------------------------------------------------

static void RegisterGlobalMessageComponents()
{
    tlsEcs.globalRegistry.emplace<ActorCreateS2C>(tlsEcs.GlobalEntity());
    tlsEcs.globalRegistry.emplace<ActorDestroyS2C>(tlsEcs.GlobalEntity());
    tlsEcs.globalRegistry.emplace<ActorListCreateS2C>(tlsEcs.GlobalEntity());
    tlsEcs.globalRegistry.emplace<ActorListDestroyS2C>(tlsEcs.GlobalEntity());
}

static void UnregisterGlobalMessageComponents()
{
    tlsEcs.globalRegistry.remove<ActorCreateS2C>(tlsEcs.GlobalEntity());
    tlsEcs.globalRegistry.remove<ActorDestroyS2C>(tlsEcs.GlobalEntity());
    tlsEcs.globalRegistry.remove<ActorListCreateS2C>(tlsEcs.GlobalEntity());
    tlsEcs.globalRegistry.remove<ActorListDestroyS2C>(tlsEcs.GlobalEntity());
}

// Free helper: check whether |observer| has |target| in its AoiListComp.
static bool IsInAoiList(entt::entity observer, entt::entity target)
{
    const auto* aoiList = tlsEcs.actorRegistry.try_get<AoiListComp>(observer);
    return aoiList && aoiList->Contains(target);
}

// ---------------------------------------------------------------------------
// Fixture: Grid assignment, neighbor lookup, player movement
// ---------------------------------------------------------------------------

class AoiGridTest : public ::testing::Test
{
protected:
    void SetUp() override { RegisterGlobalMessageComponents(); }

    void TearDown() override
    {
        UnregisterGlobalMessageComponents();
        tlsEcs.sceneRegistry.clear();
        tlsEcs.actorRegistry.clear();
    }
};

// Verify that a world-space coordinate maps to the expected hex grid id.
TEST_F(AoiGridTest, CoordinateMapsToCorrectGridId)
{
    Vector3 location;
    location.set_x(10);
    location.set_y(20);

    const auto gridId = GridSystem::GetGridId(location);
    const auto expectedGridId = GridSystem::GetGridId(hex_round(pixel_to_hex(kHexLayout, Point(10, 20))));
    EXPECT_EQ(gridId, expectedGridId);
}

// A hex cell must have exactly 6 neighbors, each matching hex_neighbor().
TEST_F(AoiGridTest, HexHasSixCorrectNeighbors)
{
    const Hex centerHex{0, 0, 0};
    GridSet neighborGridIds;
    GridSystem::GetNeighborGridIds(centerHex, neighborGridIds);

    EXPECT_EQ(neighborGridIds.size(), 6);

    for (int direction = 0; direction < 6; ++direction)
    {
        auto expectedId = GridSystem::GetGridId(hex_neighbor(centerHex, direction));
        EXPECT_TRUE(neighborGridIds.contains(expectedId))
            << "Missing neighbor in direction " << direction;
    }
}

// Spawn 10 players at random positions; after each Update the entity must
// appear in the grid cell that corresponds to its world position.
TEST_F(AoiGridTest, RandomSpawnAssignsCorrectGrid)
{
    auto sceneEntity = tlsEcs.sceneRegistry.create();
    auto& sceneGridList = tlsEcs.sceneRegistry.get_or_emplace<SceneGridListComp>(sceneEntity);

    const SceneEntityComp sceneComp{sceneEntity};
    std::unordered_map<absl::uint128, uint32_t, absl::Hash<absl::uint128>> entityCountPerGrid;

    for (uint32_t i = 0; i < 10; ++i)
    {
        auto playerEntity = tlsEcs.actorRegistry.create();
        auto& transform = tlsEcs.actorRegistry.get_or_emplace<Transform>(playerEntity);
        transform.mutable_location()->set_x(tlsRandom.RandReal<double>(0, 1000));
        transform.mutable_location()->set_y(tlsRandom.RandReal<double>(0, 1000));
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(playerEntity, sceneComp);

        AoiSystem::Update(0.1);

        auto gridId = GridSystem::GetGridId(transform.location());
        ++entityCountPerGrid[gridId];
        EXPECT_TRUE(sceneGridList[gridId].entities.contains(playerEntity));
        EXPECT_EQ(sceneGridList[gridId].entities.size(), entityCountPerGrid[gridId]);
    }

    for (auto&& [scene, gridList] : tlsEcs.sceneRegistry.view<SceneGridListComp>().each())
    {
        for (const auto& [_, grid] : gridList)
        {
            EXPECT_FALSE(grid.entities.empty());
        }
    }

    GridSystem::UpdateLogGridSize();
}

// Move a single player from the origin hex to each of its 6 neighbors and
// back, verifying grid membership after every move.
TEST_F(AoiGridTest, RoundTripThroughAllSixNeighbors)
{
    auto sceneEntity = tlsEcs.sceneRegistry.create();
    auto& sceneGridList = tlsEcs.sceneRegistry.emplace<SceneGridListComp>(sceneEntity);

    auto playerEntity = tlsEcs.actorRegistry.create();
    auto& transform = tlsEcs.actorRegistry.emplace<Transform>(playerEntity);
    transform.mutable_location()->set_x(0);
    transform.mutable_location()->set_y(0);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(playerEntity, SceneEntityComp{sceneEntity});

    AoiSystem::Update(0.1);

    const Hex originHex = hex_round(pixel_to_hex(kHexLayout, Point(0, 0)));
    const auto originGridId = GridSystem::GetGridId(originHex);
    EXPECT_TRUE(sceneGridList[originGridId].entities.contains(playerEntity));

    for (int direction = 0; direction < 6; ++direction)
    {
        // Move to neighbor
        const Hex neighborHex = hex_neighbor(originHex, direction);
        const Point neighborPixel = hex_to_pixel(kHexLayout, neighborHex);
        transform.mutable_location()->set_x(neighborPixel.x);
        transform.mutable_location()->set_y(neighborPixel.y);
        AoiSystem::Update(0.1);

        auto neighborGridId = GridSystem::GetGridId(neighborHex);
        EXPECT_TRUE(sceneGridList[neighborGridId].entities.contains(playerEntity))
            << "Player not in neighbor grid (direction " << direction << ")";
        EXPECT_FALSE(sceneGridList[originGridId].entities.contains(playerEntity))
            << "Player still in origin grid after moving (direction " << direction << ")";

        // Move back to origin
        transform.mutable_location()->set_x(0);
        transform.mutable_location()->set_y(0);
        AoiSystem::Update(0.1);
        EXPECT_TRUE(sceneGridList[originGridId].entities.contains(playerEntity));
        EXPECT_FALSE(sceneGridList[neighborGridId].entities.contains(playerEntity));
    }

    // After all round-trips only one grid cell should contain the player.
    std::size_t totalEntities = 0;
    for (auto&& [scene, gridList] : tlsEcs.sceneRegistry.view<SceneGridListComp>().each())
    {
        for (const auto& [_, grid] : gridList)
        {
            if (grid.entities.empty()) continue;
            EXPECT_EQ(grid.entities.size(), 1);
            totalEntities += grid.entities.size();
        }
    }
    EXPECT_EQ(totalEntities, 1);
}

// ---------------------------------------------------------------------------
// Fixture: Entity enter / leave visibility (verified via AoiListComp)
// ---------------------------------------------------------------------------

class AoiVisibilityTest : public ::testing::Test
{
protected:
    entt::entity entity1 = entt::null;
    entt::entity entity2 = entt::null;
    entt::entity sceneEntity = entt::null;

    void SetUp() override
    {
        RegisterGlobalMessageComponents();

        sceneEntity = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneGridListComp>(sceneEntity);

        const SceneEntityComp sceneComp{sceneEntity};

        entity1 = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<Transform>(entity1);            // origin (0,0,0)
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(entity1, sceneComp);

        entity2 = tlsEcs.actorRegistry.create();
        auto& t2 = tlsEcs.actorRegistry.emplace<Transform>(entity2);
        t2.mutable_location()->set_x(100);                           // far away initially
        t2.mutable_location()->set_y(100);
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(entity2, sceneComp);
    }

    void TearDown() override
    {
        UnregisterGlobalMessageComponents();
        tlsEcs.actorRegistry.clear();
        tlsEcs.sceneRegistry.clear();
    }
};

// When two entities are within kMaxViewRadius (10 units) after Update,
// each should appear in the other's AoiListComp.
TEST_F(AoiVisibilityTest, EntitiesWithinRangeEnterEachOthersAoiList)
{
    // Move entity2 close to entity1 (distance 5 < kMaxViewRadius 10)
    auto& location2 = *tlsEcs.actorRegistry.get<Transform>(entity2).mutable_location();
    location2.set_x(5);
    location2.set_y(0);

    AoiSystem::Update(0.0);

    EXPECT_TRUE(IsInAoiList(entity1, entity2))
        << "entity2 should be in entity1's AoiList after entering view range";
    EXPECT_TRUE(IsInAoiList(entity2, entity1))
        << "entity1 should be in entity2's AoiList after entering view range";
}

// Two entities at the same position see each other, then after one leaves
// the scene via BeforeLeaveSceneHandler it is removed from the other's AoiList.
TEST_F(AoiVisibilityTest, EntityRemovedFromAoiListAfterLeavingScene)
{
    // Place both at origin so they see each other.
    auto& location2 = *tlsEcs.actorRegistry.get<Transform>(entity2).mutable_location();
    location2.set_x(0);
    location2.set_y(0);

    AoiSystem::Update(0.0);

    // Precondition: mutual visibility established.
    ASSERT_TRUE(IsInAoiList(entity1, entity2));
    ASSERT_TRUE(IsInAoiList(entity2, entity1));

    // entity2 leaves the scene.
    BeforeLeaveScene leaveEvent;
    leaveEvent.set_entity(entt::to_integral(entity2));
    AoiSystem::BeforeLeaveSceneHandler(leaveEvent);

    // entity1 should no longer list entity2.
    EXPECT_FALSE(IsInAoiList(entity1, entity2))
        << "entity2 should be removed from entity1's AoiList after leaving scene";
}

// Entities far apart (distance > kMaxViewRadius) should NOT appear in
// each other's AoiList.
TEST_F(AoiVisibilityTest, EntitiesOutOfRangeAreNotVisible)
{
    // entity2 is already at (100,100), distance ≈ 141 >> kMaxViewRadius.
    AoiSystem::Update(0.0);

    EXPECT_FALSE(IsInAoiList(entity1, entity2))
        << "entity2 should NOT be in entity1's AoiList when out of range";
    EXPECT_FALSE(IsInAoiList(entity2, entity1))
        << "entity1 should NOT be in entity2's AoiList when out of range";
}

// ---------------------------------------------------------------------------
// Fixture: Priority, capacity, pin/unpin, stealth
// ---------------------------------------------------------------------------

class AoiPriorityTest : public ::testing::Test
{
protected:
    entt::entity sceneEntity = entt::null;

    void SetUp() override
    {
        RegisterGlobalMessageComponents();
        sceneEntity = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneGridListComp>(sceneEntity);
    }

    void TearDown() override
    {
        UnregisterGlobalMessageComponents();
        tlsEcs.actorRegistry.clear();
        tlsEcs.sceneRegistry.clear();
    }

    // Create an entity at a given position in the scene.
    entt::entity SpawnAt(double x, double y)
    {
        auto e = tlsEcs.actorRegistry.create();
        auto& t = tlsEcs.actorRegistry.emplace<Transform>(e);
        t.mutable_location()->set_x(x);
        t.mutable_location()->set_y(y);
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(e, SceneEntityComp{sceneEntity});
        return e;
    }
};

// InterestSystem::AddAoiEntity with priority upgrades existing entries.
TEST_F(AoiPriorityTest, PriorityUpgradeOnDuplicateAdd)
{
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(5, 0);

    InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kNormal);
    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(comp, nullptr);
    EXPECT_EQ(comp->entries.at(target).priority, AoiPriority::kNormal);

    // Re-add with higher priority — should upgrade.
    InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kTeammate);
    EXPECT_EQ(comp->entries.at(target).priority, AoiPriority::kTeammate);
}

// When the interest list is at capacity, a lower-priority entity is evicted
// to make room for a higher-priority one.
TEST_F(AoiPriorityTest, CapacityEvictsLowestPriority)
{
    auto watcher = SpawnAt(0, 0);

    // Fill to default capacity with kNormal entities.
    for (std::size_t i = 0; i < kAoiListCapacityDefault; ++i) {
        auto filler = tlsEcs.actorRegistry.create();
        InterestSystem::AddAoiEntity(watcher, filler, AoiPriority::kNormal);
    }

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(comp, nullptr);
    EXPECT_EQ(comp->Size(), kAoiListCapacityDefault);

    // Try to add another kNormal entity — should fail (same priority, no eviction).
    auto lowEntity = tlsEcs.actorRegistry.create();
    EXPECT_FALSE(InterestSystem::AddAoiEntity(watcher, lowEntity, AoiPriority::kNormal));
    EXPECT_EQ(comp->Size(), kAoiListCapacityDefault);

    // Add a kTeammate entity — should succeed by evicting a kNormal one.
    auto teammateEntity = tlsEcs.actorRegistry.create();
    EXPECT_TRUE(InterestSystem::AddAoiEntity(watcher, teammateEntity, AoiPriority::kTeammate));
    EXPECT_EQ(comp->Size(), kAoiListCapacityDefault);
    EXPECT_TRUE(comp->Contains(teammateEntity));
}

// PinAoiEntity adds at kPinned priority.
TEST_F(AoiPriorityTest, PinAddsAtPinnedPriority)
{
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(100, 100);  // Far away — would not be added by grid scan.

    EXPECT_TRUE(InterestSystem::PinAoiEntity(watcher, target));
    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(comp, nullptr);
    EXPECT_TRUE(comp->Contains(target));
    EXPECT_EQ(comp->entries.at(target).priority, AoiPriority::kPinned);
}

// UnpinAoiEntity downgrades pinned entry to kNormal.
TEST_F(AoiPriorityTest, UnpinDowngradesToNormal)
{
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(100, 100);

    InterestSystem::PinAoiEntity(watcher, target);
    InterestSystem::UnpinAoiEntity(watcher, target);

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(comp, nullptr);
    // Still present (not removed yet), but priority downgraded.
    EXPECT_TRUE(comp->Contains(target));
    EXPECT_EQ(comp->entries.at(target).priority, AoiPriority::kNormal);
}

// Pinned entries are NOT removed when the observer moves to a different grid.
TEST_F(AoiPriorityTest, PinnedEntitySurvivesGridLeave)
{
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(5, 0);  // Close — will enter via grid.

    AoiSystem::Update(0.0);

    // Now pin the target.
    InterestSystem::PinAoiEntity(watcher, target);

    // Move watcher far away so target falls out of grid range.
    auto& t = tlsEcs.actorRegistry.get<Transform>(watcher);
    t.mutable_location()->set_x(500);
    t.mutable_location()->set_y(500);
    AoiSystem::Update(0.0);

    // Target should still be in watcher's list because it's pinned.
    EXPECT_TRUE(IsInAoiList(watcher, target));
}

// Teammates get kTeammate priority when both have the same TeamId.
TEST_F(AoiPriorityTest, TeammatesGetHigherPriority)
{
    auto player1 = SpawnAt(0, 0);
    auto player2 = SpawnAt(5, 0);

    // Assign same team.
    auto& team1 = tlsEcs.actorRegistry.emplace<TeamId>(player1);
    team1.set_team_id(42);
    auto& team2 = tlsEcs.actorRegistry.emplace<TeamId>(player2);
    team2.set_team_id(42);

    AoiSystem::Update(0.0);

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(player1);
    ASSERT_NE(comp, nullptr);
    ASSERT_TRUE(comp->Contains(player2));
    EXPECT_EQ(comp->entries.at(player2).priority, AoiPriority::kTeammate);
}

// ---------------------------------------------------------------------------
// Fixture: Dynamic capacity (client-reported + server pressure)
// ---------------------------------------------------------------------------

class AoiDynamicCapacityTest : public ::testing::Test
{
protected:
    entt::entity sceneEntity = entt::null;

    void SetUp() override
    {
        RegisterGlobalMessageComponents();
        sceneEntity = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneGridListComp>(sceneEntity);
    }

    void TearDown() override
    {
        UnregisterGlobalMessageComponents();
        tlsEcs.actorRegistry.clear();
        tlsEcs.sceneRegistry.clear();
    }

    entt::entity SpawnAt(double x, double y)
    {
        auto e = tlsEcs.actorRegistry.create();
        auto& t = tlsEcs.actorRegistry.emplace<Transform>(e);
        t.mutable_location()->set_x(x);
        t.mutable_location()->set_y(y);
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(e, SceneEntityComp{sceneEntity});
        return e;
    }
};

// Without any capacity components, GetEffectiveCapacity returns the default.
TEST_F(AoiDynamicCapacityTest, DefaultCapacityWhenNoComponents)
{
    auto watcher = SpawnAt(0, 0);
    EXPECT_EQ(InterestSystem::GetEffectiveCapacity(watcher), kAoiListCapacityDefault);
}

// Client-reported capacity is clamped and respected.
TEST_F(AoiDynamicCapacityTest, ClientReportedCapacityIsRespected)
{
    auto watcher = SpawnAt(0, 0);
    auto& clientCap = tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(watcher);
    clientCap.clientDesiredCount = 50;

    EXPECT_EQ(InterestSystem::GetEffectiveCapacity(watcher), 50u);
}

// Client capacity below minimum is clamped to kAoiListCapacityMin.
TEST_F(AoiDynamicCapacityTest, ClientCapacityClampedToMin)
{
    auto watcher = SpawnAt(0, 0);
    auto& clientCap = tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(watcher);
    clientCap.clientDesiredCount = 5; // below kAoiListCapacityMin (20)

    EXPECT_EQ(InterestSystem::GetEffectiveCapacity(watcher), kAoiListCapacityMin);
}

// Client capacity above maximum is clamped to kAoiListCapacityMax.
TEST_F(AoiDynamicCapacityTest, ClientCapacityClampedToMax)
{
    auto watcher = SpawnAt(0, 0);
    auto& clientCap = tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(watcher);
    clientCap.clientDesiredCount = 999;

    EXPECT_EQ(InterestSystem::GetEffectiveCapacity(watcher), kAoiListCapacityMax);
}

// Server pressure reduces the effective capacity.
// GetEffectiveCapacity() 取 min(客户端上限, 服务器上限);没挂 AoiClientCapacityComp 时
// 客户端上限是 kAoiListCapacityDefault(100),会先于服务器压力成为约束项。
// 所以要验"服务器压力起作用",必须把客户端上限抬到 max,否则测的其实是那个默认值。
TEST_F(AoiDynamicCapacityTest, ServerPressureReducesCapacity)
{
    auto watcher = SpawnAt(0, 0);
    auto& clientCap = tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(watcher);
    clientCap.clientDesiredCount = kAoiListCapacityMax;

    auto& pressure = tlsEcs.sceneRegistry.emplace<ScenePressureComp>(sceneEntity);
    pressure.pressureFactor = 0.5; // half pressure → midpoint between max and min

    // Expected: max - 0.5 * (max - min) = 200 - 0.5 * 180 = 110
    EXPECT_EQ(InterestSystem::GetEffectiveCapacity(watcher), 110u);
}

// 没有 AoiClientCapacityComp 时,默认客户端上限(100)比压力算出的服务器上限(110)更紧,
// 结果取默认值 —— 这条把上面那个用例踩过的坑单独钉住。
TEST_F(AoiDynamicCapacityTest, DefaultClientCapacityWinsWhenTighterThanServerCap)
{
    auto watcher = SpawnAt(0, 0);
    auto& pressure = tlsEcs.sceneRegistry.emplace<ScenePressureComp>(sceneEntity);
    pressure.pressureFactor = 0.5;

    EXPECT_EQ(InterestSystem::GetEffectiveCapacity(watcher), kAoiListCapacityDefault);
}

// Full pressure yields minimum capacity.
TEST_F(AoiDynamicCapacityTest, FullPressureYieldsMinCapacity)
{
    auto watcher = SpawnAt(0, 0);

    auto& pressure = tlsEcs.sceneRegistry.emplace<ScenePressureComp>(sceneEntity);
    pressure.pressureFactor = 1.0;

    EXPECT_EQ(InterestSystem::GetEffectiveCapacity(watcher), kAoiListCapacityMin);
}

// Effective capacity = min(client, server-pressure-adjusted).
TEST_F(AoiDynamicCapacityTest, EffectiveCapacityIsMinOfClientAndServer)
{
    auto watcher = SpawnAt(0, 0);

    // Client wants 80, server pressure yields ~110 → effective = 80
    auto& clientCap = tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(watcher);
    clientCap.clientDesiredCount = 80;
    auto& pressure = tlsEcs.sceneRegistry.emplace<ScenePressureComp>(sceneEntity);
    pressure.pressureFactor = 0.5; // server cap = 110

    EXPECT_EQ(InterestSystem::GetEffectiveCapacity(watcher), 80u);

    // Now client wants 150, server still 110 → effective = 110
    clientCap.clientDesiredCount = 150;
    EXPECT_EQ(InterestSystem::GetEffectiveCapacity(watcher), 110u);
}

// Dynamic capacity limits how many entities AddAoiEntity accepts.
TEST_F(AoiDynamicCapacityTest, AddAoiEntityRespectsReducedCapacity)
{
    auto watcher = SpawnAt(0, 0);

    // Set client capacity to 30.
    auto& clientCap = tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(watcher);
    clientCap.clientDesiredCount = 30;

    for (std::size_t i = 0; i < 30; ++i)
    {
        auto filler = tlsEcs.actorRegistry.create();
        EXPECT_TRUE(InterestSystem::AddAoiEntity(watcher, filler, AoiPriority::kNormal));
    }

    // 31st entity should be rejected.
    auto overflow = tlsEcs.actorRegistry.create();
    EXPECT_FALSE(InterestSystem::AddAoiEntity(watcher, overflow, AoiPriority::kNormal));

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(comp, nullptr);
    EXPECT_EQ(comp->Size(), 30u);
}

// ScenePressureComp default (no pressure) returns max capacity.
TEST_F(AoiDynamicCapacityTest, ZeroPressureYieldsMaxCapacity)
{
    ScenePressureComp comp;
    comp.pressureFactor = 0.0;
    EXPECT_EQ(comp.GetServerCapacity(), kAoiListCapacityMax);
}

// ---------------------------------------------------------------------------
// Fixture: Per-scene priority policy
// ---------------------------------------------------------------------------

class AoiPriorityPolicyTest : public ::testing::Test
{
protected:
    entt::entity sceneEntity = entt::null;

    void SetUp() override
    {
        RegisterGlobalMessageComponents();
        sceneEntity = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneGridListComp>(sceneEntity);
    }

    void TearDown() override
    {
        UnregisterGlobalMessageComponents();
        tlsEcs.actorRegistry.clear();
        tlsEcs.sceneRegistry.clear();
    }

    entt::entity SpawnAt(double x, double y)
    {
        auto e = tlsEcs.actorRegistry.create();
        auto& t = tlsEcs.actorRegistry.emplace<Transform>(e);
        t.mutable_location()->set_x(x);
        t.mutable_location()->set_y(y);
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(e, SceneEntityComp{sceneEntity});
        return e;
    }

    void SetPolicy(AoiPolicyKind kind)
    {
        tlsEcs.sceneRegistry.emplace_or_replace<ScenePriorityPolicyComp>(sceneEntity,
            ScenePriorityPolicyComp{kind});
    }
};

// Default policy is kPolicyOpenWorld when no ScenePriorityPolicyComp exists.
TEST_F(AoiPriorityPolicyTest, DefaultPolicyIsOpenWorld)
{
    auto watcher = SpawnAt(0, 0);
    const auto& policy = InterestSystem::GetPriorityPolicy(watcher);
    EXPECT_EQ(policy.GetWeight(AoiPriority::kQuestNpc),
              kPolicyOpenWorld.GetWeight(AoiPriority::kQuestNpc));
}

// Open-world policy: quest NPC weight (3) > attacker weight (2).
TEST_F(AoiPriorityPolicyTest, OpenWorldQuestNpcOutranksAttacker)
{
    SetPolicy(AoiPolicyKind::kOpenWorld);
    EXPECT_GT(kPolicyOpenWorld.GetWeight(AoiPriority::kQuestNpc),
              kPolicyOpenWorld.GetWeight(AoiPriority::kAttacker));
}

// Dungeon policy: attacker weight (3) > quest NPC weight (1).
TEST_F(AoiPriorityPolicyTest, DungeonAttackerOutranksQuestNpc)
{
    SetPolicy(AoiPolicyKind::kDungeon);
    EXPECT_GT(kPolicyDungeon.GetWeight(AoiPriority::kAttacker),
              kPolicyDungeon.GetWeight(AoiPriority::kQuestNpc));
}

// Dungeon policy: boss weight (4) > attacker weight (3).
TEST_F(AoiPriorityPolicyTest, DungeonBossOutranksAttacker)
{
    SetPolicy(AoiPolicyKind::kDungeon);
    EXPECT_GT(kPolicyDungeon.GetWeight(AoiPriority::kBoss),
              kPolicyDungeon.GetWeight(AoiPriority::kAttacker));
}

// PvP arena policy: attacker weight (4) > boss weight (3).
TEST_F(AoiPriorityPolicyTest, PvpAttackerOutranksBoss)
{
    SetPolicy(AoiPolicyKind::kPvpArena);
    EXPECT_GT(kPolicyPvpArena.GetWeight(AoiPriority::kAttacker),
              kPolicyPvpArena.GetWeight(AoiPriority::kBoss));
}

// kPinned always has max weight (255) regardless of policy.
TEST_F(AoiPriorityPolicyTest, PinnedAlwaysMaxWeight)
{
    EXPECT_EQ(kPolicyOpenWorld.GetWeight(AoiPriority::kPinned), 255);
    EXPECT_EQ(kPolicyDungeon.GetWeight(AoiPriority::kPinned), 255);
    EXPECT_EQ(kPolicyPvpArena.GetWeight(AoiPriority::kPinned), 255);
}

// Under dungeon policy, adding a kBoss entry upgrades over a kQuestNpc
// via AddAoiEntity (priority upgrade path).
TEST_F(AoiPriorityPolicyTest, PolicyAffectsUpgradeDecision)
{
    SetPolicy(AoiPolicyKind::kDungeon);
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(5, 0);

    InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kQuestNpc);
    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(comp, nullptr);
    EXPECT_EQ(comp->entries.at(target).priority, AoiPriority::kQuestNpc);

    // kBoss has higher weight than kQuestNpc in dungeon policy -> upgrade succeeds.
    InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kBoss);
    EXPECT_EQ(comp->entries.at(target).priority, AoiPriority::kBoss);
}

// Under PvP policy, kAttacker outranks kBoss, so UpgradePriority from boss
// to attacker should succeed.
TEST_F(AoiPriorityPolicyTest, UpgradePriorityUsesPolicy)
{
    SetPolicy(AoiPolicyKind::kPvpArena);
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(5, 0);

    InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kBoss);
    InterestSystem::UpgradePriority(watcher, target, AoiPriority::kAttacker);

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(comp, nullptr);
    // In PvP: attacker weight (4) > boss weight (3) -> upgrade should happen.
    EXPECT_EQ(comp->entries.at(target).priority, AoiPriority::kAttacker);
}

// Under open-world policy, kAttacker (weight 2) < kQuestNpc (weight 3),
// so upgrading from kQuestNpc to kAttacker should be a no-op.
TEST_F(AoiPriorityPolicyTest, UpgradePriorityNoOpWhenWeightIsLower)
{
    SetPolicy(AoiPolicyKind::kOpenWorld);
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(5, 0);

    InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kQuestNpc);
    InterestSystem::UpgradePriority(watcher, target, AoiPriority::kAttacker);

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(comp, nullptr);
    // Attacker weight (2) < quest NPC weight (3) in open-world → no change.
    EXPECT_EQ(comp->entries.at(target).priority, AoiPriority::kQuestNpc);
}

int main(int argc, char** argv)
{
    ::testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}

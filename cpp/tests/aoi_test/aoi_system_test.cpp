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
#include "actor/actor_net_id.h"
#include "core/system/id_generator.h"
#include <gtest/gtest.h>
#include "hexagons_grid.h"
#include "spatial/comp/grid_comp.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/constants/aoi_priority.h"
#include "spatial/system/grid.h"
#include "spatial/system/interest.h"
#include "spatial/system/movement.h"
#include "combat/buff/comp/buff_comp.h"
#include "player/system/player_team.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/team_comp.pb.h"
#include "modules/scene/comp/scene_comp.h"

#include "engine/core/type_define/type_define.h"
#include "engine/core/utils/random/random.h"
#include "thread_context/redis_manager.h"
#include <thread_context/ecs_context.h>
#include <proto/scene/player_scene.pb.h>

extern const Point kDefaultSize(20.0, 20.0);
extern const Point kOrigin(0.0, 0.0);
extern const auto kHexLayout = Layout(layout_flat, kDefaultSize, kOrigin);

// AoiSystem 的 S2C 发送缓冲是 aoi.cpp 内的函数内 static thread_local,测试无需预置。

// 夹具显式模拟角色创建边界，查询和 AOI 更新不会代为补建身份。
static entt::entity CreateAoiActor()
{
    const auto actor = tlsEcs.actorRegistry.create();
    EXPECT_NE(0, actor_net_id::Assign(tlsEcs.actorRegistry, actor));
    return actor;
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
    void TearDown() override
    {
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
        auto playerEntity = CreateAoiActor();
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

    auto playerEntity = CreateAoiActor();
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

// 空场景没有入格写入时不需要格子表,每帧更新也不应预挂空表。
TEST_F(AoiGridTest, EmptySceneUpdateDoesNotCreateGridList)
{
    auto sceneEntity = tlsEcs.sceneRegistry.create();
    AoiSystem::Update(0.1);
    EXPECT_FALSE(tlsEcs.sceneRegistry.any_of<SceneGridListComp>(sceneEntity));
}

// SceneGridListComp 是按需状态:第一次真实入格必须创建,并保留后续实体的登记。
TEST_F(AoiGridTest, FirstGridEntryCreatesGridList)
{
    auto sceneEntity = tlsEcs.sceneRegistry.create();
    auto playerEntity = CreateAoiActor();
    const auto& transform = tlsEcs.actorRegistry.emplace<Transform>(playerEntity);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(playerEntity, SceneEntityComp{sceneEntity});
    EXPECT_FALSE(tlsEcs.sceneRegistry.any_of<SceneGridListComp>(sceneEntity));
    AoiSystem::Update(0.1);

    ASSERT_TRUE(tlsEcs.sceneRegistry.any_of<SceneGridListComp>(sceneEntity));
    const auto& gridList = tlsEcs.sceneRegistry.get<SceneGridListComp>(sceneEntity);
    const auto gridIt = gridList.find(GridSystem::GetGridId(transform.location()));
    ASSERT_TRUE(gridIt != gridList.end());
    EXPECT_TRUE(gridIt->second.entities.contains(playerEntity));
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<Hex>(playerEntity));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(playerEntity))
        << "独自入格没有可见对象,不应创建兴趣表";
}

// 活场景尚无格子表表示空场景,只读查询返回空,不能替它初始化。
TEST_F(AoiGridTest, SpatialQueryOnEmptySceneDoesNotCreateGridList)
{
    auto sceneEntity = tlsEcs.sceneRegistry.create();
    auto playerEntity = CreateAoiActor();
    const auto& transform = tlsEcs.actorRegistry.emplace<Transform>(playerEntity);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(playerEntity, SceneEntityComp{sceneEntity});
    tlsEcs.actorRegistry.emplace<Hex>(playerEntity, GridSystem::CalculateHexPosition(transform));

    EntityUnorderedSet nearby;
    GridSystem::GetEntitiesInGridAndNeighbors(playerEntity, nearby, true);
    GridSystem::GetEntitiesInViewAndNearby(playerEntity, nearby);

    EXPECT_FALSE(tlsEcs.sceneRegistry.any_of<SceneGridListComp>(sceneEntity));
    EXPECT_TRUE(nearby.empty());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(playerEntity));
}

TEST_F(AoiGridTest, UpdateOnDestroyedSceneDoesNotCreateGridList)
{
    const auto sceneEntity = tlsEcs.sceneRegistry.create();
    const auto playerEntity = CreateAoiActor();
    tlsEcs.actorRegistry.emplace<Transform>(playerEntity);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(playerEntity, SceneEntityComp{sceneEntity});
    tlsEcs.sceneRegistry.destroy(sceneEntity);

    AoiSystem::Update(0.1);

    EXPECT_EQ(tlsEcs.sceneRegistry.try_get<SceneGridListComp>(sceneEntity), nullptr);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<Hex>(playerEntity));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(playerEntity));
}

// F077 回归:空间查询是只读路径。场景已销毁(SceneEntityComp 里是陈旧句柄)时必须先 valid() 再查、查不到就返回空;
// 修复前 get_or_emplace 会在已销毁的场景句柄上挂出一张孤儿格子表。
TEST_F(AoiGridTest, SpatialQueryOnDestroyedSceneDoesNotCreateGridList)
{
    auto sceneEntity = tlsEcs.sceneRegistry.create();
    tlsEcs.sceneRegistry.emplace<SceneGridListComp>(sceneEntity);

    auto observer = CreateAoiActor();
    tlsEcs.actorRegistry.emplace<Transform>(observer);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(observer, SceneEntityComp{sceneEntity});
    auto neighbour = CreateAoiActor();
    tlsEcs.actorRegistry.emplace<Transform>(neighbour);
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(neighbour, SceneEntityComp{sceneEntity});
    AoiSystem::Update(0.1);
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<Hex>(observer));

    tlsEcs.sceneRegistry.destroy(sceneEntity);

    EntityUnorderedSet nearby;
    GridSystem::GetEntitiesInGridAndNeighbors(observer, nearby, true);
    GridSystem::GetEntitiesInViewAndNearby(observer, nearby);
    EXPECT_TRUE(nearby.empty());
    EXPECT_EQ(tlsEcs.sceneRegistry.try_get<SceneGridListComp>(sceneEntity), nullptr)
        << "只读查询不得在已销毁的场景句柄上挂组件";
}

// ---------------------------------------------------------------------------
// Fixture: Entity enter / leave visibility (verified via AoiListComp)
// ---------------------------------------------------------------------------

class AoiVisibilityTest : public ::testing::Test
{
protected:
    entt::entity entity1 = entt::null;
    entt::entity entity2 = entt::null;
    SceneEntity sceneEntity = entt::null;

    void SetUp() override
    {
        sceneEntity = tlsEcs.sceneRegistry.create();

        const SceneEntityComp sceneComp{sceneEntity};

        entity1 = CreateAoiActor();
        tlsEcs.actorRegistry.emplace<Transform>(entity1);            // origin (0,0,0)
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(entity1, sceneComp);

        entity2 = CreateAoiActor();
        auto& t2 = tlsEcs.actorRegistry.emplace<Transform>(entity2);
        t2.mutable_location()->set_x(100);                           // far away initially
        t2.mutable_location()->set_y(100);
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(entity2, sceneComp);
    }

    void TearDown() override
    {
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
// the scene via OnLeaveScene it is removed from the other's AoiList.
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
    AoiSystem::OnLeaveScene(entity2);

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
// AoiSystem::ResetEntityVisibility:同场景瞬移之后的可见性重建(team-system.md 文末「组队跟随 v1.1」V.2)
// ---------------------------------------------------------------------------

// 同一个六边形内的瞬移:两者同格但相距 14(> kMaxViewRadius 10)时互相不可见;
// 把 entity2 瞬移到 entity1 脚下并重置之后,下一次 Update 必须建立双向可见。
TEST_F(AoiVisibilityTest, ResetEntityVisibilityRebuildsViewAfterSameHexTeleport)
{
    auto& location2 = *tlsEcs.actorRegistry.get<Transform>(entity2).mutable_location();
    location2.set_x(14);
    location2.set_y(0);
    AoiSystem::Update(0.0);

    // 前置条件:同格、互相不可见
    ASSERT_EQ(GridSystem::GetGridId(tlsEcs.actorRegistry.get<Transform>(entity1).location()),
              GridSystem::GetGridId(location2));
    ASSERT_FALSE(IsInAoiList(entity1, entity2));
    ASSERT_FALSE(IsInAoiList(entity2, entity1));

    location2.set_x(0);
    AoiSystem::ResetEntityVisibility(entity2);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<Hex>(entity2))
        << "重置后必须摘掉 Hex,下一次 Update 才会走首次进场分支";
    AoiSystem::Update(0.0);

    EXPECT_TRUE(IsInAoiList(entity1, entity2));
    EXPECT_TRUE(IsInAoiList(entity2, entity1));
}

// 瞬移到远处:旧位置的观察者立刻不再看到它,它自己的兴趣表也清空;
// 之后的 Update 把它登记到新位置的格子里,两者仍然互相不可见。
TEST_F(AoiVisibilityTest, ResetEntityVisibilityDropsOldNeighbours)
{
    auto& location2 = *tlsEcs.actorRegistry.get<Transform>(entity2).mutable_location();
    location2.set_x(0);
    location2.set_y(0);
    AoiSystem::Update(0.0);
    ASSERT_TRUE(IsInAoiList(entity1, entity2));
    ASSERT_TRUE(IsInAoiList(entity2, entity1));

    location2.set_x(500);
    location2.set_y(500);
    AoiSystem::ResetEntityVisibility(entity2);

    EXPECT_FALSE(IsInAoiList(entity1, entity2));
    EXPECT_FALSE(IsInAoiList(entity2, entity1));

    AoiSystem::Update(0.0);

    const auto& gridList = tlsEcs.sceneRegistry.get<SceneGridListComp>(sceneEntity);
    const auto newGrid = gridList.find(GridSystem::GetGridId(location2));
    ASSERT_TRUE(newGrid != gridList.end());
    EXPECT_TRUE(newGrid->second.entities.contains(entity2));
    const auto oldGrid = gridList.find(GridSystem::GetGridId(tlsEcs.actorRegistry.get<Transform>(entity1).location()));
    ASSERT_TRUE(oldGrid != gridList.end());
    EXPECT_FALSE(oldGrid->second.entities.contains(entity2));
    EXPECT_FALSE(IsInAoiList(entity1, entity2));
    EXPECT_FALSE(IsInAoiList(entity2, entity1));
}

// kPinned 条目由 buff / skill 生命周期管理:重置既不摘"我钉住的",也不摘"钉住我的"。
TEST_F(AoiVisibilityTest, ResetEntityVisibilityKeepsPinnedEntriesOnBothSides)
{
    auto& location2 = *tlsEcs.actorRegistry.get<Transform>(entity2).mutable_location();
    location2.set_x(0);
    location2.set_y(0);
    AoiSystem::Update(0.0);
    ASSERT_TRUE(IsInAoiList(entity1, entity2));
    ASSERT_TRUE(IsInAoiList(entity2, entity1));

    ASSERT_TRUE(InterestSystem::AddAoiEntity(entity1, entity2, AoiPriority::kPinned));
    ASSERT_TRUE(InterestSystem::AddAoiEntity(entity2, entity1, AoiPriority::kPinned));

    location2.set_x(500);
    location2.set_y(500);
    AoiSystem::ResetEntityVisibility(entity2);

    EXPECT_TRUE(IsInAoiList(entity1, entity2)) << "对方钉住我的条目不能被重置摘掉";
    EXPECT_TRUE(IsInAoiList(entity2, entity1)) << "我钉住对方的条目不能被重置摘掉";
}

// 还没进过格子的实体(刚进场、首次 Update 之前):重置不崩,首次 Update 照常建立可见性。
TEST_F(AoiVisibilityTest, ResetEntityVisibilityBeforeFirstUpdateIsHarmless)
{
    auto& location2 = *tlsEcs.actorRegistry.get<Transform>(entity2).mutable_location();
    location2.set_x(3);
    location2.set_y(0);

    AoiSystem::ResetEntityVisibility(entity2);
    AoiSystem::Update(0.0);

    EXPECT_TRUE(IsInAoiList(entity1, entity2));
    EXPECT_TRUE(IsInAoiList(entity2, entity1));
}

// 兴趣表只在真正加入第一个可见对象时创建,孤身入格无需空表。
TEST_F(AoiVisibilityTest, FirstGridEntryWithoutVisibleTargetDoesNotCreateInterestList)
{
    // entity2 远在 (100,100),entity1 周围没有任何人。
    AoiSystem::Update(0.0);

    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(entity1));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(entity2));
}

// 重走首次进场分支(同场景瞬移之后)必须沿用已有兴趣表:kPinned 条目由 buff/skill 生命周期管理,
// 要跨瞬移存活,不能被一张新表覆盖。守护创建点不被改成 emplace_or_replace。
TEST_F(AoiVisibilityTest, FirstEntryAfterResetKeepsPinnedEntries)
{
    auto& location2 = *tlsEcs.actorRegistry.get<Transform>(entity2).mutable_location();
    location2.set_x(0);
    location2.set_y(0);
    AoiSystem::Update(0.0);
    ASSERT_TRUE(InterestSystem::AddAoiEntity(entity2, entity1, AoiPriority::kPinned));

    location2.set_x(500);
    location2.set_y(500);
    AoiSystem::ResetEntityVisibility(entity2);
    AoiSystem::Update(0.0);

    ASSERT_TRUE(IsInAoiList(entity2, entity1));
    EXPECT_EQ(tlsEcs.actorRegistry.get<AoiListComp>(entity2).entries.at(entity1).priority, AoiPriority::kPinned);
}

// 离场路径对场景格子表只读/摘除。表缺失时按空表处理,不得补建。
// 修复前 OnLeaveScene 对场景实体 get_or_emplace<SceneGridListComp>,会重新挂出一张空表。
TEST_F(AoiVisibilityTest, LeaveSceneDoesNotRecreateMissingGridList)
{
    AoiSystem::Update(0.0);
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<Hex>(entity1));

    tlsEcs.sceneRegistry.remove<SceneGridListComp>(sceneEntity);

    AoiSystem::OnLeaveScene(entity1);

    EXPECT_FALSE(tlsEcs.sceneRegistry.any_of<SceneGridListComp>(sceneEntity));
}

// ---------------------------------------------------------------------------
// Fixture: Priority, capacity, pin/unpin, stealth
// ---------------------------------------------------------------------------

class AoiPriorityTest : public ::testing::Test
{
protected:
    SceneEntity sceneEntity = entt::null;

    void SetUp() override
    {
        sceneEntity = tlsEcs.sceneRegistry.create();
    }

    void TearDown() override
    {
        tlsEcs.actorRegistry.clear();
        tlsEcs.sceneRegistry.clear();
    }

    // Create an entity at a given position in the scene.
    entt::entity SpawnAt(double x, double y)
    {
        auto e = CreateAoiActor();
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
        auto filler = CreateAoiActor();
        InterestSystem::AddAoiEntity(watcher, filler, AoiPriority::kNormal);
    }

    auto* comp = tlsEcs.actorRegistry.try_get<AoiListComp>(watcher);
    ASSERT_NE(comp, nullptr);
    EXPECT_EQ(comp->Size(), kAoiListCapacityDefault);

    // Try to add another kNormal entity — should fail (same priority, no eviction).
    auto lowEntity = CreateAoiActor();
    EXPECT_FALSE(InterestSystem::AddAoiEntity(watcher, lowEntity, AoiPriority::kNormal));
    EXPECT_EQ(comp->Size(), kAoiListCapacityDefault);

    // Add a kTeammate entity — should succeed by evicting a kNormal one.
    auto teammateEntity = CreateAoiActor();
    EXPECT_TRUE(InterestSystem::AddAoiEntity(watcher, teammateEntity, AoiPriority::kTeammate));
    EXPECT_EQ(comp->Size(), kAoiListCapacityDefault);
    EXPECT_TRUE(comp->Contains(teammateEntity));
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

// 直接写入口允许尚未入格的活 actor,第一次真实插入时创建兴趣表。
TEST_F(AoiPriorityTest, AddAoiEntityCreatesInterestListBeforeGridEntry)
{
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(5, 0);

    EXPECT_TRUE(InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kNormal));
    EXPECT_TRUE(IsInAoiList(watcher, target));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<Hex>(watcher));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(target));
}


TEST_F(AoiPriorityTest, AbsentInterestListReadsAndRemovalDoNotCreateIt)
{
    const auto watcher = SpawnAt(0, 0);
    const auto target = SpawnAt(5, 0);
    EXPECT_FALSE(IsInAoiList(watcher, target));
    InterestSystem::RemoveAoiEntity(watcher, target);
    InterestSystem::UpgradePriority(watcher, target, AoiPriority::kPinned);
    InterestSystem::DowngradePriority(watcher, target, AoiPriority::kPinned, AoiPriority::kNormal);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(watcher));
}

// F078 回归:已销毁的观察者必须被拒绝。修复前 get_or_emplace 不校验存活,
// 会在已销毁的句柄上挂出一张孤儿 AoiListComp 并返回 true。
TEST_F(AoiPriorityTest, AddAoiEntityRejectsDestroyedWatcher)
{
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(5, 0);
    tlsEcs.actorRegistry.destroy(watcher);

    EXPECT_FALSE(InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kNormal));
    EXPECT_EQ(tlsEcs.actorRegistry.try_get<AoiListComp>(watcher), nullptr);
}

// 已销毁的目标同样不得登记进兴趣表:否则下发给客户端的实体号会指向一个不存在的 actor。
// 修复前目标不做存活校验,会被直接插入并返回 true。
TEST_F(AoiPriorityTest, AddAoiEntityRejectsDestroyedTarget)
{
    auto watcher = SpawnAt(0, 0);
    auto target  = SpawnAt(5, 0);
    tlsEcs.actorRegistry.destroy(target);

    EXPECT_FALSE(InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kNormal));
    EXPECT_FALSE(IsInAoiList(watcher, target));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(watcher));
}

// ---------------------------------------------------------------------------
// Fixture: Dynamic capacity (client-reported + server pressure)
// ---------------------------------------------------------------------------

class AoiDynamicCapacityTest : public ::testing::Test
{
protected:
    SceneEntity sceneEntity = entt::null;

    void SetUp() override
    {
        sceneEntity = tlsEcs.sceneRegistry.create();
    }

    void TearDown() override
    {
        tlsEcs.actorRegistry.clear();
        tlsEcs.sceneRegistry.clear();
    }

    entt::entity SpawnAt(double x, double y)
    {
        auto e = CreateAoiActor();
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
        auto filler = CreateAoiActor();
        EXPECT_TRUE(InterestSystem::AddAoiEntity(watcher, filler, AoiPriority::kNormal));
    }

    // 31st entity should be rejected.
    auto overflow = CreateAoiActor();
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
    SceneEntity sceneEntity = entt::null;

    void SetUp() override
    {
        sceneEntity = tlsEcs.sceneRegistry.create();
    }

    void TearDown() override
    {
        tlsEcs.actorRegistry.clear();
        tlsEcs.sceneRegistry.clear();
    }

    entt::entity SpawnAt(double x, double y)
    {
        auto e = CreateAoiActor();
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

// ---------------------------------------------------------------------------
// Fixture: 组队成员关系变化后的队友 AOI 优先级修正(team-system.md §F.4 / §I.4)
// ---------------------------------------------------------------------------

class AoiTeammateRefreshTest : public ::testing::Test
{
protected:
    SceneEntity sceneEntity = entt::null;

    void SetUp() override
    {
        sceneEntity = tlsEcs.sceneRegistry.create();
    }

    void TearDown() override
    {
        tlsEcs.actorRegistry.clear();
        tlsEcs.sceneRegistry.clear();
    }

    entt::entity SpawnAt(double x, double y)
    {
        auto e = CreateAoiActor();
        auto& t = tlsEcs.actorRegistry.emplace<Transform>(e);
        t.mutable_location()->set_x(x);
        t.mutable_location()->set_y(y);
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(e, SceneEntityComp{sceneEntity});
        return e;
    }

    // 模拟 PlayerTeamSystem::ApplyMembership 对组件的写法:tid 非 0 写组件,tid 为 0 移除。
    static void SetTeam(entt::entity e, uint64_t teamId)
    {
        if (teamId == 0)
        {
            tlsEcs.actorRegistry.remove<TeamId>(e);
            return;
        }
        auto& team = tlsEcs.actorRegistry.emplace_or_replace<TeamId>(e);
        team.set_team_id(teamId);
    }

    static AoiPriority PriorityOf(entt::entity watcher, entt::entity target)
    {
        return tlsEcs.actorRegistry.get<AoiListComp>(watcher).entries.at(target).priority;
    }
};

// 已经互相可见之后才入队:只有入队者收到刷新,双向都要升到 kTeammate。
TEST_F(AoiTeammateRefreshTest, JoinAfterMutualVisibilityUpgradesBothDirections)
{
    auto leader = SpawnAt(0, 0);
    auto joiner = SpawnAt(5, 0);
    SetTeam(leader, 42);

    AoiSystem::Update(0.0);
    ASSERT_TRUE(IsInAoiList(leader, joiner));
    ASSERT_TRUE(IsInAoiList(joiner, leader));
    ASSERT_EQ(PriorityOf(leader, joiner), AoiPriority::kNormal);
    ASSERT_EQ(PriorityOf(joiner, leader), AoiPriority::kNormal);

    SetTeam(joiner, 42);
    PlayerTeamSystem::RefreshTeammateAoi(joiner);

    EXPECT_EQ(PriorityOf(leader, joiner), AoiPriority::kTeammate);
    EXPECT_EQ(PriorityOf(joiner, leader), AoiPriority::kTeammate);
}

// 离队:双向降回 kNormal;被 buff/skill 钉住的 kPinned 条目不受影响。
TEST_F(AoiTeammateRefreshTest, LeaveDowngradesBothDirectionsButKeepsPinned)
{
    auto leaver = SpawnAt(0, 0);
    auto mateB = SpawnAt(5, 0);
    auto mateC = SpawnAt(0, 5);
    SetTeam(leaver, 7);
    SetTeam(mateB, 7);
    SetTeam(mateC, 7);

    AoiSystem::Update(0.0);
    ASSERT_EQ(PriorityOf(leaver, mateB), AoiPriority::kTeammate);
    ASSERT_EQ(PriorityOf(mateB, leaver), AoiPriority::kTeammate);
    ASSERT_TRUE(InterestSystem::AddAoiEntity(leaver, mateC, AoiPriority::kPinned));
    ASSERT_EQ(PriorityOf(leaver, mateC), AoiPriority::kPinned);

    SetTeam(leaver, 0);
    PlayerTeamSystem::RefreshTeammateAoi(leaver);

    EXPECT_EQ(PriorityOf(leaver, mateB), AoiPriority::kNormal);
    EXPECT_EQ(PriorityOf(mateB, leaver), AoiPriority::kNormal);
    EXPECT_EQ(PriorityOf(leaver, mateC), AoiPriority::kPinned);
    EXPECT_EQ(PriorityOf(mateC, leaver), AoiPriority::kNormal);
    // 与离队者无关的队友关系保持不变
    EXPECT_EQ(PriorityOf(mateB, mateC), AoiPriority::kTeammate);
}

// 换队:旧队友降级,新队友升级。
TEST_F(AoiTeammateRefreshTest, SwitchTeamDowngradesOldAndUpgradesNew)
{
    auto mover = SpawnAt(0, 0);
    auto oldMate = SpawnAt(5, 0);
    auto newMate = SpawnAt(0, 5);
    SetTeam(mover, 1);
    SetTeam(oldMate, 1);
    SetTeam(newMate, 2);

    AoiSystem::Update(0.0);
    ASSERT_EQ(PriorityOf(mover, oldMate), AoiPriority::kTeammate);
    ASSERT_EQ(PriorityOf(mover, newMate), AoiPriority::kNormal);

    SetTeam(mover, 2);
    PlayerTeamSystem::RefreshTeammateAoi(mover);

    EXPECT_EQ(PriorityOf(mover, oldMate), AoiPriority::kNormal);
    EXPECT_EQ(PriorityOf(oldMate, mover), AoiPriority::kNormal);
    EXPECT_EQ(PriorityOf(mover, newMate), AoiPriority::kTeammate);
    EXPECT_EQ(PriorityOf(newMate, mover), AoiPriority::kTeammate);
}

// 兴趣条目不对称:我的表里没有对方(容量满被淘汰),对方表里有我。
// 只有我收到刷新,对方表中关于我的条目也必须先升到 kTeammate、离队后再降回 kNormal。
TEST_F(AoiTeammateRefreshTest, AsymmetricEntryIsFixedFromTheOtherSide)
{
    auto me = SpawnAt(0, 0);
    auto other = SpawnAt(5, 0);
    SetTeam(other, 9);

    AoiSystem::Update(0.0);
    ASSERT_TRUE(IsInAoiList(other, me));
    // 模拟"我的兴趣表已满、对方被淘汰"
    InterestSystem::RemoveAoiEntity(me, other);
    ASSERT_FALSE(IsInAoiList(me, other));

    SetTeam(me, 9);
    PlayerTeamSystem::RefreshTeammateAoi(me);
    EXPECT_EQ(PriorityOf(other, me), AoiPriority::kTeammate);
    EXPECT_FALSE(IsInAoiList(me, other)) << "刷新只修正已有条目,不负责补插";

    SetTeam(me, 0);
    PlayerTeamSystem::RefreshTeammateAoi(me);
    EXPECT_EQ(PriorityOf(other, me), AoiPriority::kNormal);
    EXPECT_FALSE(IsInAoiList(me, other));
}

// 还没进过 AOI 格子(没有 Hex)的实体:静默返回,不崩溃。
TEST_F(AoiTeammateRefreshTest, EntityWithoutGridPositionIsNoOp)
{
    auto loner = SpawnAt(0, 0);
    SetTeam(loner, 3);
    PlayerTeamSystem::RefreshTeammateAoi(loner);
    EXPECT_EQ(tlsEcs.actorRegistry.try_get<AoiListComp>(loner), nullptr);
}

// DowngradePriority 只认标签:条目不是 from 时不动。
TEST_F(AoiTeammateRefreshTest, DowngradePriorityOnlyMatchesExactTag)
{
    auto watcher = SpawnAt(0, 0);
    auto target = SpawnAt(5, 0);

    InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kAttacker);
    InterestSystem::DowngradePriority(watcher, target, AoiPriority::kTeammate, AoiPriority::kNormal);
    EXPECT_EQ(PriorityOf(watcher, target), AoiPriority::kAttacker);

    InterestSystem::DowngradePriority(watcher, target, AoiPriority::kAttacker, AoiPriority::kNormal);
    EXPECT_EQ(PriorityOf(watcher, target), AoiPriority::kNormal);
}

// ---------------------------------------------------------------------------
// 纯函数:PlayerTeamSystem::ShouldApplyMembership(team-system.md §F.2)
// ---------------------------------------------------------------------------

TEST(TeamMembershipRuleTest, OlderEpochIsIgnored)
{
    EXPECT_FALSE(PlayerTeamSystem::ShouldApplyMembership(5, true, 4));
}

TEST(TeamMembershipRuleTest, EqualEpochIsIgnored)
{
    EXPECT_FALSE(PlayerTeamSystem::ShouldApplyMembership(5, true, 5));
}

TEST(TeamMembershipRuleTest, NewerEpochIsApplied)
{
    EXPECT_TRUE(PlayerTeamSystem::ShouldApplyMembership(5, true, 6));
}

TEST(TeamMembershipRuleTest, MissingComponentAlwaysApplies)
{
    EXPECT_TRUE(PlayerTeamSystem::ShouldApplyMembership(0, false, 0));
    EXPECT_TRUE(PlayerTeamSystem::ShouldApplyMembership(0, false, 1700000000123ULL));
}

int main(int argc, char** argv)
{
    ::testing::InitGoogleTest(&argc, argv);
    tlsIdGeneratorManager.SetNodeId(92);
    actor_net_id::Install(tlsEcs.actorRegistry);
    return RUN_ALL_TESTS();
}

// ---------------------------------------------------------------------------
// 纯函数:PlayerTeamSystem::ShouldRegroup(team-system.md 文末「组队跟随 v1.1」V.2)
// ---------------------------------------------------------------------------

TEST(TeamRegroupRuleTest, InvisibleLeaderAlwaysRegroupsEvenWhenAdjacent)
{
    // 同格但从未建立可见性:脸贴脸也要瞬移,否则客户端永远看不到队长
    EXPECT_TRUE(PlayerTeamSystem::ShouldRegroup(0.0, false));
    EXPECT_TRUE(PlayerTeamSystem::ShouldRegroup(kTeamRegroupDistance, false));
}

TEST(TeamRegroupRuleTest, VisibleLeaderWithinFormationIsLeftAlone)
{
    EXPECT_FALSE(PlayerTeamSystem::ShouldRegroup(0.0, true));
    EXPECT_FALSE(PlayerTeamSystem::ShouldRegroup(5.6, true)); // 5 人纵队末位
    EXPECT_FALSE(PlayerTeamSystem::ShouldRegroup(kTeamRegroupDistance, true)); // 边界:恰好等于不触发
}

TEST(TeamRegroupRuleTest, VisibleLeaderBeyondFormationRegroups)
{
    EXPECT_TRUE(PlayerTeamSystem::ShouldRegroup(kTeamRegroupDistance + 0.01, true));
    EXPECT_TRUE(PlayerTeamSystem::ShouldRegroup(300.0, true));
}

// ---------------------------------------------------------------------------
// 纯函数:MovementSystem::JudgeReportAfterTeleport(瞬移后丢弃过期移动上报)
// ---------------------------------------------------------------------------

TEST(TeleportReportVerdictTest, FarReportBeforeDeadlineIsStale)
{
    EXPECT_EQ(MovementSystem::JudgeReportAfterTeleport(kTeleportSettleRadius + 0.01, 1000, 4000),
              TeleportReportVerdict::kStale);
    EXPECT_EQ(MovementSystem::JudgeReportAfterTeleport(250.0, 3999, 4000), TeleportReportVerdict::kStale);
}

TEST(TeleportReportVerdictTest, NearReportSettlesBeforeAndAfterDeadline)
{
    EXPECT_EQ(MovementSystem::JudgeReportAfterTeleport(0.0, 1000, 4000), TeleportReportVerdict::kSettled);
    EXPECT_EQ(MovementSystem::JudgeReportAfterTeleport(kTeleportSettleRadius, 1000, 4000),
              TeleportReportVerdict::kSettled);
    // 落位后站着不动、过了超时才发第一条上报:仍然是正常落位
    EXPECT_EQ(MovementSystem::JudgeReportAfterTeleport(0.5, 9000, 4000), TeleportReportVerdict::kSettled);
}

TEST(TeleportReportVerdictTest, FarReportAtOrAfterDeadlineExpires)
{
    EXPECT_EQ(MovementSystem::JudgeReportAfterTeleport(250.0, 4000, 4000), TeleportReportVerdict::kExpired);
    EXPECT_EQ(MovementSystem::JudgeReportAfterTeleport(250.0, 9000, 4000), TeleportReportVerdict::kExpired);
}

#include <gtest/gtest.h>

#include <cmath>
#include <utility>
#include <vector>

#include "engine/core/type_define/type_define.h"
#include "config/config.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/scene/scene_info.pb.h"
#include "spatial/constants/nav.h"
#include "spatial/manager/scene_nav.h"
#include "spatial/system/navigation.h"
#include "spatial/system/nav_query.h"
#include "spatial/system/scene_spawn.h"
#include "table/code/basescene_table.h"
#include "thread_context/ecs_context.h"

namespace {

class SceneSpawnTest : public testing::Test {
protected:
    void SetUp() override {
        BaseSceneTableManager::Instance().Load();
        previousNav = std::move(SceneNavManager::Instance().sceneNav);
        NavigationSystem::LoadNavBins();
        player = tlsEcs.actorRegistry.create();
        scene = tlsEcs.sceneRegistry.create();
    }
    void TearDown() override {
        tlsEcs.actorRegistry.destroy(player);
        tlsEcs.sceneRegistry.destroy(scene);
        SceneNavManager::Instance().sceneNav = std::move(previousNav);
    }
    void SelectScene(uint32_t id) {
        tlsEcs.sceneRegistry.get_or_emplace<SceneInfoComp>(scene).set_scene_config_id(id);
    }
    void SetPosition(double x, double y, double z) {
        auto* location = tlsEcs.actorRegistry.get_or_emplace<Transform>(player).mutable_location();
        location->set_x(x); location->set_y(y); location->set_z(z);
    }
    void ExpectPosition(const Location& expected) {
        const auto& actual = tlsEcs.actorRegistry.get<Transform>(player).location();
        EXPECT_NEAR(expected.x(), actual.x(), 0.02);
        EXPECT_NEAR(expected.y(), actual.y(), 0.02);
        EXPECT_NEAR(expected.z(), actual.z(), 0.02);
    }
    entt::entity player{entt::null};
    SceneEntity scene{entt::null};
    SceneNavMapComp previousNav;
};

TEST_F(SceneSpawnTest, EveryConfiguredSceneHasNavigationAndAnExactSpawn) {
    const auto& rows = BaseSceneTableManager::Instance().FindAll().data();
    ASSERT_GE(rows.size(), 4);
    for (const auto& row : rows) {
        SCOPED_TRACE(row.id());
        auto* nav = SceneNavManager::Instance().Get(row.id());
        ASSERT_NE(nullptr, nav);
        const Location expected = SceneSpawnSystem::DefaultSpawnFor(row.id());
        Location actual;
        ASSERT_TRUE(NavQuerySystem::SnapToMesh(*nav, expected, actual));
        EXPECT_NEAR(expected.x(), actual.x(), 0.02);
        EXPECT_NEAR(expected.y(), actual.y(), 0.02);
        EXPECT_NEAR(expected.z(), actual.z(), 0.02);
    }
}

TEST_F(SceneSpawnTest, ThreeDestinationsUseDistinctGeometry) {
    auto& nav = SceneNavManager::Instance();
    ASSERT_NE(nullptr, nav.Get(2));
    ASSERT_NE(nullptr, nav.Get(3));
    ASSERT_NE(nullptr, nav.Get(4));
    EXPECT_NE(nav.Get(2), nav.Get(3));
    EXPECT_NE(nav.Get(2), nav.Get(4));
    EXPECT_NE(nav.Get(3), nav.Get(4));
}

TEST_F(SceneSpawnTest, UnsetAndInvalidSavedLocationsUseEachDestinationSpawn) {
    for (const uint32_t id : {2u, 3u, 4u}) {
        SCOPED_TRACE(id);
        SelectScene(id);
        const auto expected = SceneSpawnSystem::DefaultSpawnFor(id);
        SetPosition(0, 0, 0);
        EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
        ExpectPosition(expected);
        SetPosition(9999, 9999, 9999);
        EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
        ExpectPosition(expected);
    }
}

TEST_F(SceneSpawnTest, ReentryPreservesLegalPositionWhileMapTravelUsesDestinationSpawn) {
    for (const uint32_t id : {2u, 3u, 4u}) {
        SCOPED_TRACE(id);
        SelectScene(id);
        const auto spawn = SceneSpawnSystem::DefaultSpawnFor(id);
        auto candidate = spawn;
        candidate.set_x(candidate.x() + 2.0);
        Location nearby;
        ASSERT_TRUE(NavQuerySystem::SnapToMesh(*SceneNavManager::Instance().Get(id), candidate, nearby));
        ASSERT_GT(std::abs(nearby.x() - spawn.x()) + std::abs(nearby.y() - spawn.y()), 0.5);
        SetPosition(nearby.x(), nearby.y(), nearby.z());
        EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
        ExpectPosition(nearby);
        EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene, true));
        ExpectPosition(spawn);
    }
}

TEST_F(SceneSpawnTest, YuexiBayLoadedMapRoutesReachBothDocksAndRelocateOldInvalidPosition) {
    SelectScene(3);
    auto* nav = SceneNavManager::Instance().Get(3);
    ASSERT_NE(nullptr, nav);
    ASSERT_TRUE(nav->versionedMask);
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(3));
    const auto spawn = SceneSpawnSystem::DefaultSpawnFor(3);
    EXPECT_NEAR(36.73125, spawn.x(), 1e-9);
    EXPECT_NEAR(20.19375, spawn.y(), 1e-9);
    EXPECT_DOUBLE_EQ(0, spawn.z());
    // 真实双节导航计划的南北码头接近点，按服务端 Z-up 坐标写入。
    for (const auto& [x,y] : {std::pair{44.4375, 59.34375}, std::pair{21.88125, 48.15}}) {
        Location dock;
        dock.set_x(x); dock.set_y(y);
        std::vector<Location> route;
        ASSERT_TRUE(NavQuerySystem::FindPath(*nav, spawn, dock, route)) << x << ',' << y;
        ASSERT_GE(route.size(), 2U);
        EXPECT_NEAR(spawn.x(), route.front().x(), .02);
        EXPECT_NEAR(spawn.y(), route.front().y(), .02);
        EXPECT_NEAR(dock.x(), route.back().x(), .02);
        EXPECT_NEAR(dock.y(), route.back().y(), .02);
        for (size_t i = 1; i < route.size(); ++i) {
            Location accepted;
            ASSERT_TRUE(NavQuerySystem::ValidateMove(*nav, route[i-1], route[i], accepted)) << i;
        }
        SetPosition(dock.x(), dock.y(), 0);
        EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
        ExpectPosition(dock);
    }
    SetPosition(147, 201, 0);
    EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
    ExpectPosition(spawn);
    Location sea, accepted;
    sea.set_x(spawn.x()); sea.set_y(75);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, sea, accepted));
    EXPECT_FALSE(NavQuerySystem::ValidateMove(*nav, spawn, sea, accepted));
}

TEST_F(SceneSpawnTest, NoNavigationStillHonorsDestinationSpawnAndPreservesReentry) {
    SceneNavManager::Instance().sceneNav.clear();
    SelectScene(4);
    const auto spawn = SceneSpawnSystem::DefaultSpawnFor(4);
    SetPosition(10, 20, 0);
    EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
    EXPECT_EQ(10, tlsEcs.actorRegistry.get<Transform>(player).location().x());
    EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene, true));
    ExpectPosition(spawn);
}

TEST_F(SceneSpawnTest, MissingSceneDoesNotCreateTransformAndUnknownIdKeepsLegacyFallback) {
    EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
    EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<Transform>(player));
    const auto fallback = SceneSpawnSystem::DefaultSpawnFor(0);
    EXPECT_EQ(kTianyongSpawnX, fallback.x());
    EXPECT_EQ(kTianyongSpawnY, fallback.y());
    EXPECT_EQ(kTianyongSpawnZ, fallback.z());
}

TEST_F(SceneSpawnTest, MissingTransformIsNotCreatedOnEntry) {
    SelectScene(2);
    ASSERT_FALSE(tlsEcs.actorRegistry.any_of<Transform>(player));

    EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
    EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<Transform>(player));
}

} // namespace

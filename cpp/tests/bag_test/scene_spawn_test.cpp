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

TEST_F(SceneSpawnTest, BiyuanWaterPalaceLoadedMapRoutesReachAllKeyAreasAndBlockWater) {
    SelectScene(5);
    auto* nav = SceneNavManager::Instance().Get(5);
    ASSERT_NE(nullptr, nav);
    ASSERT_TRUE(nav->versionedMask);
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(5));
    const auto spawn = SceneSpawnSystem::DefaultSpawnFor(5);
    EXPECT_NEAR(58.5, spawn.x(), 1e-9);
    EXPECT_NEAR(222.5, spawn.y(), 1e-9);
    EXPECT_DOUBLE_EQ(0, spawn.z());
    // 碧渊水宫双节共享导航关键地标（主殿门、鼓亭、茶亭、东南观景台、东拱桥），服务端 Z-up 坐标。
    for (const auto& [x, y] : {std::pair{186.0, 116.25}, std::pair{185.375, 261.25},
                               std::pair{53.5, 80.0}, std::pair{83.5, 277.5},
                               std::pair{124.125, 220.0}}) {
        Location goal;
        goal.set_x(x); goal.set_y(y);
        std::vector<Location> route;
        ASSERT_TRUE(NavQuerySystem::FindPath(*nav, spawn, goal, route)) << x << ',' << y;
        ASSERT_GE(route.size(), 2U);
        EXPECT_NEAR(spawn.x(), route.front().x(), .02);
        EXPECT_NEAR(spawn.y(), route.front().y(), .02);
        EXPECT_NEAR(goal.x(), route.back().x(), .02);
        EXPECT_NEAR(goal.y(), route.back().y(), .02);
        for (size_t i = 1; i < route.size(); ++i) {
            Location accepted;
            ASSERT_TRUE(NavQuerySystem::ValidateMove(*nav, route[i - 1], route[i], accepted)) << i;
        }
        SetPosition(goal.x(), goal.y(), 0);
        EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
        ExpectPosition(goal);
    }
    // 旧占位主城出生点在碧渊水宫内不可走，进图应回落到新出生点。
    SetPosition(133, 199, 0);
    EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
    ExpectPosition(spawn);
    // 上下 128px 留白、外湖水域、东廊边荷花池、茶亭屋檐、大殿屋顶与南桥分叉水岸必须全部阻挡。
    for (const auto& [x, y] : {std::pair{252.0, 160.0}, std::pair{4.0, 160.0},
                               std::pair{131.0, 25.0}, std::pair{144.75, 245.625},
                               std::pair{74.75, 80.0}, std::pair{216.0, 112.5},
                               std::pair{16.0, 242.5}}) {
        Location blocked, accepted;
        blocked.set_x(x); blocked.set_y(y);
        EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, blocked, accepted)) << x << ',' << y;
        EXPECT_FALSE(NavQuerySystem::ValidateMove(*nav, spawn, blocked, accepted)) << x << ',' << y;
    }
}

TEST_F(SceneSpawnTest, BajingIslandLoadedMapRoutesReachAllKeyAreasAndBlockSea) {
    SelectScene(2);
    auto* nav = SceneNavManager::Instance().Get(2);
    ASSERT_NE(nullptr, nav);
    ASSERT_TRUE(nav->versionedMask);
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(2));
    const auto spawn = SceneSpawnSystem::DefaultSpawnFor(2);
    EXPECT_NEAR(70.0, spawn.x(), 1e-9);
    EXPECT_NEAR(205.0, spawn.y(), 1e-9);
    EXPECT_DOUBLE_EQ(0, spawn.z());
    // 八景岛双节共享导航关键地标（中央太极广场、中央主殿前、西北桥、东南书亭前），服务端 Z-up 坐标。
    for (const auto& [x, y] : {std::pair{200.41666666666666, 166.66666666666669},
                               std::pair{240.0, 209.58333333333331},
                               std::pair{272.5, 104.16666666666667},
                               std::pair{141.66666666666666, 341.25}}) {
        Location goal;
        goal.set_x(x); goal.set_y(y);
        std::vector<Location> route;
        ASSERT_TRUE(NavQuerySystem::FindPath(*nav, spawn, goal, route)) << x << ',' << y;
        ASSERT_GE(route.size(), 2U);
        EXPECT_NEAR(spawn.x(), route.front().x(), .02);
        EXPECT_NEAR(spawn.y(), route.front().y(), .02);
        EXPECT_NEAR(goal.x(), route.back().x(), .02);
        EXPECT_NEAR(goal.y(), route.back().y(), .02);
        for (size_t i = 1; i < route.size(); ++i) {
            Location accepted;
            ASSERT_TRUE(NavQuerySystem::ValidateMove(*nav, route[i - 1], route[i], accepted)) << i;
        }
        SetPosition(goal.x(), goal.y(), 0);
        EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
        ExpectPosition(goal);
    }
    // 外海区域必须阻挡。
    Location sea, accepted;
    sea.set_x(198.33333333333334); sea.set_y(-12.5);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, sea, accepted));
    EXPECT_FALSE(NavQuerySystem::ValidateMove(*nav, spawn, sea, accepted));
}

TEST_F(SceneSpawnTest, QinglanTownLoadedMapRoutesReachAllKeyAreasAndBlockRiver) {
    SelectScene(4);
    auto* nav = SceneNavManager::Instance().Get(4);
    ASSERT_NE(nullptr, nav);
    ASSERT_TRUE(nav->versionedMask);
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(4));
    const auto spawn = SceneSpawnSystem::DefaultSpawnFor(4);
    EXPECT_NEAR(-65.1943359375, spawn.x(), 1e-9);
    EXPECT_NEAR(-83.6923828125, spawn.y(), 1e-9);
    EXPECT_DOUBLE_EQ(0, spawn.z());
    // 青岚镇双节共享导航关键地标（中央石桥、东北主院、西北木桥、东南木桥、北圃西台），服务端 Z-up 坐标。
    for (const auto& [x, y] : {std::pair{-14.0615234375, -22.7841796875},
                               std::pair{40.455078125, 82.865234375},
                               std::pair{40.0791015625, -81.4365234375},
                               std::pair{-62.9384765625, 64.4423828125},
                               std::pair{53.990234375, -45.71875}}) {
        Location goal;
        goal.set_x(x); goal.set_y(y);
        std::vector<Location> route;
        ASSERT_TRUE(NavQuerySystem::FindPath(*nav, spawn, goal, route)) << x << ',' << y;
        ASSERT_GE(route.size(), 2U);
        EXPECT_NEAR(spawn.x(), route.front().x(), .02);
        EXPECT_NEAR(spawn.y(), route.front().y(), .02);
        EXPECT_NEAR(goal.x(), route.back().x(), .02);
        EXPECT_NEAR(goal.y(), route.back().y(), .02);
        for (size_t i = 1; i < route.size(); ++i) {
            Location accepted;
            ASSERT_TRUE(NavQuerySystem::ValidateMove(*nav, route[i - 1], route[i], accepted)) << i;
        }
        SetPosition(goal.x(), goal.y(), 0);
        EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
        ExpectPosition(goal);
    }
    // 旧 lanxian_scene.bin 出生点 (180, 200) 在青岚镇有效范围外，进图应回落到新出生点。
    SetPosition(180, 200, 0);
    EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
    ExpectPosition(spawn);
    // 上下透明留白、溪流、西南牌坊屋顶、西市桌摊、北圃菜地、东北主殿屋顶必须全部阻挡。
    for (const auto& [x, y] : {std::pair{123.921875, 0.0}, std::pair{-124.22265625, 0.0},
                               std::pair{-27.97265625, -17.14453125}, std::pair{-57.298828125, -108.8828125},
                               std::pair{-11.8056640625, -63.765625}, std::pair{56.998046875, -23.912109375},
                               std::pair{68.27734375, 97.15234375}}) {
        Location blocked, accepted;
        blocked.set_x(x); blocked.set_y(y);
        EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, blocked, accepted)) << x << ',' << y;
        EXPECT_FALSE(NavQuerySystem::ValidateMove(*nav, spawn, blocked, accepted)) << x << ',' << y;
    }
}

TEST_F(SceneSpawnTest, WaterwheelCourtyardLoadedMapRoutesReachBridgeStairsAndBlockWater) {
    SelectScene(20000);
    auto* nav = SceneNavManager::Instance().Get(20000);
    ASSERT_NE(nullptr, nav);
    ASSERT_TRUE(nav->versionedMask);
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(20000));
    const auto spawn = SceneSpawnSystem::DefaultSpawnFor(20000);
    EXPECT_NEAR(140.0, spawn.x(), 1e-9);
    EXPECT_NEAR(160.0, spawn.y(), 1e-9);
    EXPECT_DOUBLE_EQ(0, spawn.z());
    // 水车庭院双节共享导航关键地标（主殿台阶顶、北上石阶顶、西铺台阶、东南小殿台阶顶、石拱桥顶、西侧临水观景台），服务端 Z-up 坐标。
    for (const auto& [x, y] : {std::pair{237.1875, 196.09375},
                               std::pair{314.53125, 126.5625},
                               std::pair{176.25, 79.6875},
                               std::pair{87.96875, 236.71875},
                               std::pair{87.1875, 148.4375},
                               std::pair{80.15625, 42.1875}}) {
        Location goal;
        goal.set_x(x); goal.set_y(y);
        std::vector<Location> route;
        ASSERT_TRUE(NavQuerySystem::FindPath(*nav, spawn, goal, route)) << x << ',' << y;
        ASSERT_GE(route.size(), 2U);
        EXPECT_NEAR(spawn.x(), route.front().x(), .02);
        EXPECT_NEAR(spawn.y(), route.front().y(), .02);
        EXPECT_NEAR(goal.x(), route.back().x(), .02);
        EXPECT_NEAR(goal.y(), route.back().y(), .02);
        for (size_t i = 1; i < route.size(); ++i) {
            Location accepted;
            ASSERT_TRUE(NavQuerySystem::ValidateMove(*nav, route[i - 1], route[i], accepted)) << i;
        }
        SetPosition(goal.x(), goal.y(), 0);
        EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
        ExpectPosition(goal);
    }
    // 越界填充边位置进图应回落到出生点 (140, 160, 0)。
    SetPosition(16.0, 160.0, 0);
    EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
    ExpectPosition(spawn);
    // 瀑布、水车、南荷塘、桥北护栏、右侧与底部填充边必须全部阻挡。
    for (const auto& [x, y] : {std::pair{273.125, 22.65625}, std::pair{261.40625, 52.34375},
                               std::pair{54.375, 146.484375}, std::pair{94.21875, 148.4375},
                               std::pair{160.0, 308.0}, std::pair{16.0, 160.0}}) {
        Location blocked, accepted;
        blocked.set_x(x); blocked.set_y(y);
        EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, blocked, accepted)) << x << ',' << y;
        EXPECT_FALSE(NavQuerySystem::ValidateMove(*nav, spawn, blocked, accepted)) << x << ',' << y;
    }
}

TEST_F(SceneSpawnTest, CanglanHarborLoadedMapRoutesReachPiersMarketAndBlockWater) {
    SelectScene(6);
    auto* nav = SceneNavManager::Instance().Get(6);
    ASSERT_NE(nullptr, nav);
    ASSERT_TRUE(nav->versionedMask);
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(6));
    const auto spawn = SceneSpawnSystem::DefaultSpawnFor(6);
    EXPECT_NEAR(129.4218017965555, spawn.x(), 1e-9);
    EXPECT_NEAR(23.110906512290242, spawn.y(), 1e-9);
    EXPECT_DOUBLE_EQ(0, spawn.z());
    // 沧澜港双节共享导航关键地标（中央石阶、中央码头、北鱼市、北码头、船厂前场），服务端 Z-up 坐标。
    for (const auto& [x, y] : {std::pair{126.221802367, 120.889067839},
                               std::pair{120.533616179, 148.266339535},
                               std::pair{242.844505022, 239.644502001},
                               std::pair{247.466325457, 288.710859205},
                               std::pair{44.4445403606, 196.266330985}}) {
        Location goal;
        goal.set_x(x); goal.set_y(y);
        std::vector<Location> route;
        ASSERT_TRUE(NavQuerySystem::FindPath(*nav, spawn, goal, route)) << x << ',' << y;
        ASSERT_GE(route.size(), 2U);
        EXPECT_NEAR(spawn.x(), route.front().x(), .02);
        EXPECT_NEAR(spawn.y(), route.front().y(), .02);
        EXPECT_NEAR(goal.x(), route.back().x(), .02);
        EXPECT_NEAR(goal.y(), route.back().y(), .02);
        for (size_t i = 1; i < route.size(); ++i) {
            Location accepted;
            ASSERT_TRUE(NavQuerySystem::ValidateMove(*nav, route[i - 1], route[i], accepted)) << i;
        }
        SetPosition(goal.x(), goal.y(), 0);
        EXPECT_FALSE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
        ExpectPosition(goal);
    }
    // 不可走水域位置进图应回落到出生点。
    SetPosition(159.99997509270906, 270.22177312672136, 0);
    EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
    ExpectPosition(spawn);
    // 水域、官署室内、右侧与底部填充边必须全部阻挡。
    for (const auto& [x, y] : {std::pair{159.99997509270906, 270.22177312672136},
                               std::pair{230.75541780441995, 195.55542048290374},
                               std::pair{99.19998592212798, 142.22179592549801},
                               std::pair{190.2217909671366, 145.0663401044905},
                               std::pair{256.84799844026566, 331.99881315231323},
                               std::pair{-15.60754930973053, 72.17365503311157}}) {
        Location blocked, accepted;
        blocked.set_x(x); blocked.set_y(y);
        EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, blocked, accepted)) << x << ',' << y;
        EXPECT_FALSE(NavQuerySystem::ValidateMove(*nav, spawn, blocked, accepted)) << x << ',' << y;
    }
}

TEST_F(SceneSpawnTest, TemporaryMainCityReusesBiyuanNavigationAndSpawn) {
    SelectScene(1);
    auto* nav1 = SceneNavManager::Instance().Get(1);
    auto* nav5 = SceneNavManager::Instance().Get(5);
    ASSERT_NE(nullptr, nav1);
    ASSERT_EQ(nav5, nav1);
    ASSERT_TRUE(nav1->versionedMask);
    const auto spawn = SceneSpawnSystem::DefaultSpawnFor(1);
    EXPECT_NEAR(58.5, spawn.x(), 1e-9);
    EXPECT_NEAR(222.5, spawn.y(), 1e-9);
    // 旧主城出生点 (133, 199) 在碧渊水宫内不可走，进图应回落到新出生点。
    SetPosition(133, 199, 0);
    EXPECT_TRUE(SceneSpawnSystem::EnsureValidEnterLocation(player, scene));
    ExpectPosition(spawn);
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

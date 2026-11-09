#include <gtest/gtest.h>

#include <chrono>
#include <filesystem>
#include <fstream>
#include <string>

#include "config/node_config_manager.h"
#include "core/utils/encode/sha256.h"
#include "modules/scene/comp/scene_comp.h"
#include "player/system/player_scene.h"
#include "proto/scene/scene_info.pb.h"
#include "spatial/manager/scene_nav.h"
#include "spatial/system/navigation.h"
#include "spatial/system/nav_query.h"
#include "spatial/system/movement.h"
#include <limits>
#include "table/code/basescene_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
namespace fs = std::filesystem;

class FestivalNavigationTest : public testing::Test {
protected:
    virtual uint32_t SceneId() const { return 19004; }
    void SetUp() override {
        oldConfig = tlsNodeConfigManager.GetBaseDeployConfig();
        oldNav = std::move(SceneNavManager::Instance().sceneNav);
        // 从运行配置取真实已交付导航；每个失败用例只破坏自己的临时副本。
        const std::string mapPath = "data/scene_nav_bin/festival_maps/" + std::to_string(SceneId());
        source = fs::path(GetDataRootDir()) / mapPath;
        ASSERT_TRUE(fs::exists(source / "scene.bin")) << source.string();
        temporary = fs::temp_directory_path() / ("festival-nav-" + std::to_string(
            std::chrono::steady_clock::now().time_since_epoch().count()));
        folder = temporary / mapPath;
        fs::create_directories(folder);
        for (const char* name : {"scene.bin", "scene.bin.mask", "scene.bin.sha256"})
            fs::copy_file(source / name, folder / name);
        NavMask mask;
        ASSERT_TRUE(mask.Load((folder / "scene.bin.mask").string()));
        // 取实际安全格中心作为测试配置出生点，避免硬编码客户端像素坐标。
        bool found = false;
        for (int row=0;row<mask.rows && !found;++row) for(int col=0;col<mask.columns && !found;++col) {
            if (!mask.Cell(col,row)) continue;
            double x0,x1,z0,z1;
            ASSERT_TRUE(mask.CellBounds(col,row,x0,x1,z0,z1));
            spawn.set_x((z0+z1)/2); spawn.set_y((x0+x1)/2); spawn.set_z(0); found=true;
        }
        ASSERT_TRUE(found);
        BaseSceneTableData data;
        auto* row = data.add_data(); row->set_id(SceneId());
        row->set_nav_bin_file(mapPath + "/scene.bin");
        row->set_spawn_x(spawn.x()); row->set_spawn_y(spawn.y()); row->set_spawn_z(0);
        std::ofstream out(temporary / "basescene.pb", std::ios::binary);
        ASSERT_TRUE(data.SerializeToOstream(&out)); out.close();
        auto& config = tlsNodeConfigManager.GetBaseDeployConfig();
        config.set_data_root_directory(temporary.generic_string() + "/");
        config.set_table_data_directory(temporary.generic_string() + "/");
        config.set_table_data_format("binary");
        BaseSceneTableManager::Instance().Load();
    }
    void TearDown() override {
        SceneNavManager::Instance().sceneNav = std::move(oldNav);
        tlsNodeConfigManager.GetBaseDeployConfig() = oldConfig;
        BaseSceneTableManager::Instance().Load();
        if (!temporary.empty()) fs::remove_all(temporary);
    }
    void LoadAndExpectRejected() {
        NavigationSystem::LoadNavBins();
        EXPECT_EQ(nullptr, SceneNavManager::Instance().Get(19004));
        EXPECT_TRUE(NavQuerySystem::RequiresNavigation(19004));
        const auto scene = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_config_id(19004);
        EXPECT_EQ(kServiceUnavailable, PlayerSceneSystem::CheckEnterScene(Guid{42}, scene));
        const auto actor = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(actor).sceneEntity = scene;
        EXPECT_TRUE(NavQuerySystem::MissingRequiredNavigation(actor));
        auto* position = tlsEcs.actorRegistry.emplace<Transform>(actor).mutable_location();
        position->set_x(spawn.x()); position->set_y(spawn.y());
        const Vector3 target = *position;
        EXPECT_FALSE(MovementSystem::TeleportWithinScene(actor,target,Rotation(),kTeleportReasonTeamRegroup));
        tlsEcs.actorRegistry.destroy(actor);
        tlsEcs.sceneRegistry.destroy(scene);
    }
    BaseDeployConfig oldConfig;
    SceneNavMapComp oldNav;
    fs::path source, temporary, folder;
    Location spawn;
};

TEST_F(FestivalNavigationTest, RealLoaderRegisters48CellMapAndRejectsOutsideMovement) {
    NavigationSystem::LoadNavBins();
    auto* nav = SceneNavManager::Instance().Get(19004);
    ASSERT_NE(nullptr, nav);
    ASSERT_TRUE(nav->strictMask);
    int count=0;
    for(int row=0;row<nav->mask.rows;++row) for(int col=0;col<nav->mask.columns;++col)
        count += nav->mask.Cell(col,row) ? 1 : 0;
    EXPECT_EQ(48,count);
    Location accepted;
    EXPECT_TRUE(NavQuerySystem::ValidateMove(*nav,spawn,spawn,accepted));
    Location outside=spawn;
    outside.set_y(nav->mask.effectiveX + nav->mask.effectiveWidth);
    EXPECT_FALSE(NavQuerySystem::ValidateMove(*nav,spawn,outside,accepted));
    EXPECT_DOUBLE_EQ(spawn.x(),accepted.x());
    EXPECT_DOUBLE_EQ(spawn.y(),accepted.y());
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav,outside,accepted));
}

TEST_F(FestivalNavigationTest, MissingNavBinRejectsRegistrationAndEntry) {
    fs::remove(folder / "scene.bin"); LoadAndExpectRejected();
}
TEST_F(FestivalNavigationTest, FeetHeightToleranceNormalizesGroundButRejectsAirborneAndNaN) {
    NavigationSystem::LoadNavBins();
    auto* nav=SceneNavManager::Instance().Get(19004);
    ASSERT_NE(nullptr,nav);
    for(double feet : {-0.05,0.007,0.15}) {
        Location target=spawn,accepted; target.set_z(feet);
        EXPECT_TRUE(NavQuerySystem::ValidateMove(*nav,spawn,target,accepted));
        EXPECT_DOUBLE_EQ(0,accepted.z());
    }
    for(double height : {-0.201,0.201,std::numeric_limits<double>::quiet_NaN()}) {
        Location target=spawn,accepted; target.set_z(height);
        EXPECT_FALSE(NavQuerySystem::ValidateMove(*nav,spawn,target,accepted));
        EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav,target,accepted));
    }
}

class FestivalDisconnectedNavigationTest : public FestivalNavigationTest {
protected:
    uint32_t SceneId() const override { return 10000; }
};

TEST_F(FestivalDisconnectedNavigationTest, TeamTeleportCannotBridgeDisconnectedPlatforms) {
    NavigationSystem::LoadNavBins();
    auto* nav=SceneNavManager::Instance().Get(SceneId());
    ASSERT_NE(nullptr,nav);
    Location other;
    bool found=false;
    const int startCol=static_cast<int>((spawn.y()-nav->mask.minX)/nav->mask.cellSize);
    const int startRow=nav->mask.rows-1-static_cast<int>((spawn.x()-nav->mask.minZ)/nav->mask.cellSize);
    std::vector<bool> visited(nav->mask.columns*nav->mask.rows,false);
    std::vector<int> queue{startRow*nav->mask.columns+startCol}; visited[queue.front()]=true;
    for(size_t q=0;q<queue.size();++q) {
        const int col=queue[q]%nav->mask.columns,row=queue[q]/nav->mask.columns;
        const int dx[]={-1,1,0,0},dy[]={0,0,-1,1};
        for(int n=0;n<4;++n) if(nav->mask.Cell(col+dx[n],row+dy[n])) {
            const int next=(row+dy[n])*nav->mask.columns+col+dx[n];
            if(!visited[next]) {visited[next]=true;queue.push_back(next);}
        }
    }
    for(int row=0;row<nav->mask.rows && !found;++row) for(int col=0;col<nav->mask.columns && !found;++col) {
        if(!nav->mask.Cell(col,row) || visited[row*nav->mask.columns+col]) continue;
        double x0,x1,z0,z1;
        if(!nav->mask.CellBounds(col,row,x0,x1,z0,z1)) continue;
        Location candidate; candidate.set_x((z0+z1)/2);candidate.set_y((x0+x1)/2);
        std::vector<Location> path;
        EXPECT_FALSE(NavQuerySystem::FindPath(*nav,spawn,candidate,path)); other=candidate;found=true;
    }
    ASSERT_TRUE(found);
    const auto scene=tlsEcs.sceneRegistry.create();
    const auto actor=tlsEcs.actorRegistry.create();
    tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_config_id(SceneId());
    tlsEcs.actorRegistry.emplace<SceneEntityComp>(actor).sceneEntity=scene;
    auto* p=tlsEcs.actorRegistry.emplace<Transform>(actor).mutable_location();
    p->set_x(spawn.x());p->set_y(spawn.y());p->set_z(0);
    Vector3 target;target.set_x(other.x());target.set_y(other.y());
    EXPECT_FALSE(MovementSystem::TeleportWithinScene(actor,target,Rotation(),kTeleportReasonTeamRegroup));
    EXPECT_DOUBLE_EQ(spawn.x(),p->x()); EXPECT_DOUBLE_EQ(spawn.y(),p->y());
    tlsEcs.actorRegistry.destroy(actor);tlsEcs.sceneRegistry.destroy(scene);
}
TEST_F(FestivalNavigationTest, MissingExactMaskRejectsRegistrationAndEntry) {
    fs::remove(folder / "scene.bin.mask"); LoadAndExpectRejected();
}
TEST_F(FestivalNavigationTest, MissingChecksumsRejectsRegistrationAndEntry) {
    fs::remove(folder / "scene.bin.sha256"); LoadAndExpectRejected();
}
TEST_F(FestivalNavigationTest, CorruptNavBinIsRejectedBeforeDetourParse) {
    std::ofstream out(folder / "scene.bin",std::ios::binary | std::ios::trunc); out << "MSET"; out.close();
    LoadAndExpectRejected();
}
TEST_F(FestivalNavigationTest, CorruptExactMaskRejectsRegistrationAndEntry) {
    std::ofstream out(folder / "scene.bin.mask",std::ios::app); out << "unexpected"; out.close();
    LoadAndExpectRejected();
}
} // namespace

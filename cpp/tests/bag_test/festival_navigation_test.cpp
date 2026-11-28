#include <gtest/gtest.h>

#include <chrono>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <sstream>
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
    virtual uint32_t SourceSceneId() const { return SceneId(); }
    virtual std::string MapPath() const {
        return "data/scene_nav_bin/festival_maps/" + std::to_string(SceneId());
    }
    virtual fs::path SourceFolder() const {
        return fs::path(GetDataRootDir()) / "data/scene_nav_bin/festival_maps" / std::to_string(SourceSceneId());
    }
    void SetUp() override {
        oldConfig = tlsNodeConfigManager.GetBaseDeployConfig();
        oldNav = std::move(SceneNavManager::Instance().sceneNav);
        // 从运行配置取真实已交付导航；每个失败用例只破坏自己的临时副本。
        const std::string mapPath = MapPath();
        source = SourceFolder();
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
        EXPECT_EQ(nullptr, SceneNavManager::Instance().Get(SceneId()));
        EXPECT_TRUE(NavQuerySystem::RequiresNavigation(SceneId()));
        const auto scene = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_config_id(SceneId());
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

class CurrentVillageNavigationTest : public FestivalNavigationTest {
protected:
    uint32_t SceneId() const override { return 3; }
    uint32_t SourceSceneId() const override { return 19004; }
    std::string MapPath() const override {
        return "data/scene_nav_bin/current_maps/village/yuexi-bay-20261115";
    }
};

TEST_F(CurrentVillageNavigationTest, VersionedMapRegistersItsVerifiedExactMask) {
    NavigationSystem::LoadNavBins();
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(SceneId()));
    auto* nav = SceneNavManager::Instance().Get(SceneId());
    ASSERT_NE(nullptr, nav);
    EXPECT_TRUE(nav->strictMask);
    Location accepted;
    EXPECT_TRUE(NavQuerySystem::SnapToMesh(*nav, spawn, accepted));
}

TEST_F(CurrentVillageNavigationTest, MissingChecksumRejectsVersionedSceneBeforeEntry) {
    fs::remove(folder / "scene.bin.sha256");
    LoadAndExpectRejected();
}

class CurrentBiyuanNavigationTest : public FestivalNavigationTest {
protected:
    uint32_t SceneId() const override { return 5; }
    std::string MapPath() const override {
        return "data/scene_nav_bin/current_maps/biyuan-water-palace/biyuan-body-20261123";
    }
    fs::path SourceFolder() const override {
        return fs::path(GetDataRootDir()) / MapPath();
    }
};

TEST_F(CurrentBiyuanNavigationTest, VersionedBiyuanBodyRegistersExactMaskAndRejectsPadding) {
    NavigationSystem::LoadNavBins();
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(SceneId()));
    auto* nav = SceneNavManager::Instance().Get(SceneId());
    ASSERT_NE(nullptr, nav);
    EXPECT_TRUE(nav->strictMask);
    EXPECT_TRUE(nav->versionedMask);
    EXPECT_EQ(320, nav->mask.columns);
    EXPECT_EQ(256, nav->mask.rows);
    EXPECT_DOUBLE_EQ(8.0, nav->mask.effectiveZ);
    EXPECT_DOUBLE_EQ(240.0, nav->mask.effectiveHeight);
    int count = 0;
    for (int row = 0; row < nav->mask.rows; ++row) {
        for (int col = 0; col < nav->mask.columns; ++col) {
            count += nav->mask.Cell(col, row) ? 1 : 0;
        }
    }
    EXPECT_EQ(9093, count);
    Location accepted;
    EXPECT_TRUE(NavQuerySystem::SnapToMesh(*nav, spawn, accepted));
    Location topPadding;
    topPadding.set_x(252.0); topPadding.set_y(160.0); topPadding.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, topPadding, accepted));
    Location bottomPadding;
    bottomPadding.set_x(4.0); bottomPadding.set_y(160.0); bottomPadding.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, bottomPadding, accepted));
}

TEST_F(CurrentBiyuanNavigationTest, MissingChecksumRejectsBiyuanBeforeEntry) {
    fs::remove(folder / "scene.bin.sha256");
    LoadAndExpectRejected();
}

class CurrentBajingNavigationTest : public FestivalNavigationTest {
protected:
    uint32_t SceneId() const override { return 2; }
    std::string MapPath() const override {
        return "data/scene_nav_bin/current_maps/island/bajingbody20261114";
    }
    fs::path SourceFolder() const override {
        return fs::path(GetDataRootDir()) / MapPath();
    }
};

TEST_F(CurrentBajingNavigationTest, VersionedBajingBodyRegistersExactMaskAndBlocksSea) {
    NavigationSystem::LoadNavBins();
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(SceneId()));
    auto* nav = SceneNavManager::Instance().Get(SceneId());
    ASSERT_NE(nullptr, nav);
    EXPECT_TRUE(nav->strictMask);
    EXPECT_TRUE(nav->versionedMask);
    EXPECT_EQ(587, nav->mask.columns);
    EXPECT_EQ(427, nav->mask.rows);
    int count = 0;
    for (int row = 0; row < nav->mask.rows; ++row) {
        for (int col = 0; col < nav->mask.columns; ++col) {
            count += nav->mask.Cell(col, row) ? 1 : 0;
        }
    }
    EXPECT_EQ(24016, count);
    Location accepted;
    EXPECT_TRUE(NavQuerySystem::SnapToMesh(*nav, spawn, accepted));
    Location westSea;
    westSea.set_x(198.33333333333334); westSea.set_y(-12.5); westSea.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, westSea, accepted));
}

TEST_F(CurrentBajingNavigationTest, MissingChecksumRejectsBajingBeforeEntry) {
    fs::remove(folder / "scene.bin.sha256");
    LoadAndExpectRejected();
}

class CurrentQinglanNavigationTest : public FestivalNavigationTest {
protected:
    uint32_t SceneId() const override { return 4; }
    std::string MapPath() const override {
        return "data/scene_nav_bin/current_maps/qinglan-town/qinglan-20261127-v002";
    }
    fs::path SourceFolder() const override {
        return fs::path(GetDataRootDir()) / MapPath();
    }
};

TEST_F(CurrentQinglanNavigationTest, VersionedQinglanTownRegistersExactMaskAndBlocksPaddingAndRiver) {
    NavigationSystem::LoadNavBins();
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(SceneId()));
    auto* nav = SceneNavManager::Instance().Get(SceneId());
    ASSERT_NE(nullptr, nav);
    EXPECT_TRUE(nav->strictMask);
    EXPECT_TRUE(nav->versionedMask);
    EXPECT_EQ(308, nav->mask.columns);
    EXPECT_EQ(308, nav->mask.rows);
    EXPECT_DOUBLE_EQ(-95.197265625, nav->mask.effectiveZ);
    EXPECT_DOUBLE_EQ(190.39453125, nav->mask.effectiveHeight);
    int count = 0;
    for (int row = 0; row < nav->mask.rows; ++row) {
        for (int col = 0; col < nav->mask.columns; ++col) {
            count += nav->mask.Cell(col, row) ? 1 : 0;
        }
    }
    EXPECT_EQ(6389, count);
    Location accepted;
    EXPECT_TRUE(NavQuerySystem::SnapToMesh(*nav, spawn, accepted));
    Location topPadding;
    topPadding.set_x(123.921875); topPadding.set_y(0.0); topPadding.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, topPadding, accepted));
    Location bottomPadding;
    bottomPadding.set_x(-124.22265625); bottomPadding.set_y(0.0); bottomPadding.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, bottomPadding, accepted));
    Location river;
    river.set_x(-27.97265625); river.set_y(-17.14453125); river.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, river, accepted));
}

TEST_F(CurrentQinglanNavigationTest, MissingChecksumRejectsQinglanBeforeEntry) {
    fs::remove(folder / "scene.bin.sha256");
    LoadAndExpectRejected();
}

class CurrentWaterwheelNavigationTest : public FestivalNavigationTest {
protected:
    uint32_t SceneId() const override { return 20000; }
    std::string MapPath() const override {
        return "data/scene_nav_bin/current_maps/waterwheel-courtyard/waterwheel-hd8-20261128";
    }
    fs::path SourceFolder() const override {
        return fs::path(GetDataRootDir()) / MapPath();
    }
};

TEST_F(CurrentWaterwheelNavigationTest, VersionedWaterwheelCourtyardRegistersExactMaskAndBlocksPaddingAndWaterfall) {
    NavigationSystem::LoadNavBins();
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(SceneId()));
    auto* nav = SceneNavManager::Instance().Get(SceneId());
    ASSERT_NE(nullptr, nav);
    EXPECT_TRUE(nav->strictMask);
    EXPECT_TRUE(nav->versionedMask);
    EXPECT_EQ(320, nav->mask.columns);
    EXPECT_EQ(320, nav->mask.rows);
    EXPECT_DOUBLE_EQ(0.0, nav->mask.effectiveX);
    EXPECT_DOUBLE_EQ(32.578125, nav->mask.effectiveZ);
    EXPECT_DOUBLE_EQ(297.1484375, nav->mask.effectiveWidth);
    EXPECT_DOUBLE_EQ(287.421875, nav->mask.effectiveHeight);
    int count = 0;
    for (int row = 0; row < nav->mask.rows; ++row) {
        for (int col = 0; col < nav->mask.columns; ++col) {
            count += nav->mask.Cell(col, row) ? 1 : 0;
        }
    }
    EXPECT_EQ(15357, count);
    Location accepted;
    EXPECT_TRUE(NavQuerySystem::SnapToMesh(*nav, spawn, accepted));
    Location rightPadding;
    rightPadding.set_x(160.0); rightPadding.set_y(308.0); rightPadding.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, rightPadding, accepted));
    Location bottomPadding;
    bottomPadding.set_x(16.0); bottomPadding.set_y(160.0); bottomPadding.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, bottomPadding, accepted));
    Location waterfall;
    waterfall.set_x(273.125); waterfall.set_y(22.65625); waterfall.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, waterfall, accepted));
}

TEST_F(CurrentWaterwheelNavigationTest, MissingChecksumRejectsWaterwheelBeforeEntry) {
    fs::remove(folder / "scene.bin.sha256");
    LoadAndExpectRejected();
}

class CurrentCanglanHarborNavigationTest : public FestivalNavigationTest {
protected:
    uint32_t SceneId() const override { return 6; }
    std::string MapPath() const override {
        return "data/scene_nav_bin/current_maps/canglan-harbor/canglan-harbor-20261128-v001";
    }
    fs::path SourceFolder() const override {
        return fs::path(GetDataRootDir()) / MapPath();
    }
};

TEST_F(CurrentCanglanHarborNavigationTest, VersionedCanglanHarborRegistersExactMaskAndBlocksPaddingAndWater) {
    NavigationSystem::LoadNavBins();
    ASSERT_TRUE(NavQuerySystem::RequiresNavigation(SceneId()));
    auto* nav = SceneNavManager::Instance().Get(SceneId());
    ASSERT_NE(nullptr, nav);
    EXPECT_TRUE(nav->strictMask);
    EXPECT_TRUE(nav->versionedMask);
    EXPECT_EQ(247, nav->mask.columns);
    EXPECT_EQ(247, nav->mask.rows);
    EXPECT_DOUBLE_EQ(0.0, nav->mask.effectiveX);
    EXPECT_GE(nav->mask.effectiveZ, 0.0);
    EXPECT_LE(nav->mask.effectiveWidth, 320.0);
    EXPECT_LE(nav->mask.effectiveHeight, 320.0);
    int count = 0;
    for (int row = 0; row < nav->mask.rows; ++row) {
        for (int col = 0; col < nav->mask.columns; ++col) {
            count += nav->mask.Cell(col, row) ? 1 : 0;
        }
    }
    EXPECT_EQ(2429, count);
    Location accepted;
    EXPECT_TRUE(NavQuerySystem::SnapToMesh(*nav, spawn, accepted));
    Location rightPadding;
    rightPadding.set_x(256.84799844026566); rightPadding.set_y(331.99881315231323); rightPadding.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, rightPadding, accepted));
    Location bottomPadding;
    bottomPadding.set_x(-15.60754930973053); bottomPadding.set_y(72.17365503311157); bottomPadding.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, bottomPadding, accepted));
    Location lagoonWater;
    lagoonWater.set_x(159.99997509270906); lagoonWater.set_y(270.22177312672136); lagoonWater.set_z(0.0);
    EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, lagoonWater, accepted));
}

TEST_F(CurrentCanglanHarborNavigationTest, MissingChecksumRejectsCanglanHarborBeforeEntry) {
    fs::remove(folder / "scene.bin.sha256");
    LoadAndExpectRejected();
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

class AuthoredNavigationTest : public FestivalNavigationTest {
protected:
    uint32_t SceneId() const override { return 26000; }
    uint32_t SourceSceneId() const override { return 19004; }
    std::string MapPath() const override { return "data/scene_nav_bin/authored_maps/26000"; }
    void SetUp() override {
        FestivalNavigationTest::SetUp();
        ASSERT_FALSE(HasFatalFailure());
        NavMask originalBounds;
        ASSERT_TRUE(originalBounds.Load((folder / "scene.bin.mask").string()));
        fs::remove(folder / "scene.bin.mask");
        std::ostringstream text;
        text << std::setprecision(17) << "ANAV 1\n" << originalBounds.effectiveX << ' '
             << originalBounds.effectiveZ << ' ' << originalBounds.effectiveWidth << ' '
             << originalBounds.effectiveHeight << " 0\n";
        WriteContract(text.str());
    }
    void WriteContract(const std::string& text) {
        std::ofstream contract(folder / "scene.bin.authored", std::ios::binary | std::ios::trunc);
        contract << text; contract.close();
        Resign();
    }
    void Resign() {
        auto hash = [](const fs::path& path) {
            std::ifstream in(path, std::ios::binary); std::ostringstream data; data << in.rdbuf();
            return Sha256::HashToHex(data.str());
        };
        std::ofstream checksums(folder / "scene.bin.sha256", std::ios::trunc);
        checksums << hash(folder / "scene.bin") << '\n' << hash(folder / "scene.bin.authored") << '\n';
    }
};

TEST_F(AuthoredNavigationTest, RealLoaderRegistersRequiredNavWithoutLegacyMask) {
    NavigationSystem::LoadNavBins();
    auto* nav = SceneNavManager::Instance().Get(SceneId());
    ASSERT_NE(nullptr, nav);
    EXPECT_FALSE(nav->strictMask);
    EXPECT_TRUE(NavQuerySystem::RequiresNavigation(SceneId()));
    Location accepted;
    EXPECT_TRUE(NavQuerySystem::ValidateMove(*nav, spawn, spawn, accepted));
    EXPECT_DOUBLE_EQ(0, accepted.z());
}
TEST_F(AuthoredNavigationTest, MissingContractRejectsRegistrationAndEntry) {
    fs::remove(folder / "scene.bin.authored"); LoadAndExpectRejected();
}
TEST_F(AuthoredNavigationTest, MissingChecksumsRejectsRegistrationAndEntry) {
    fs::remove(folder / "scene.bin.sha256"); LoadAndExpectRejected();
}
TEST_F(AuthoredNavigationTest, CorruptNavIsRejectedBeforeDetourParse) {
    std::ofstream out(folder / "scene.bin", std::ios::binary | std::ios::trunc); out << "bad"; out.close();
    LoadAndExpectRejected();
}
TEST_F(AuthoredNavigationTest, CorruptContractChecksumIsRejected) {
    std::ofstream out(folder / "scene.bin.authored", std::ios::app); out << "unexpected"; out.close();
    LoadAndExpectRejected();
}
TEST_F(AuthoredNavigationTest, ResidualLegacyMaskIsRejected) {
    fs::copy_file(source / "scene.bin.mask", folder / "scene.bin.mask"); LoadAndExpectRejected();
}
TEST_F(AuthoredNavigationTest, InvalidSignedContractCannotChangeGroundOrIgnoreTrailingGarbage) {
    for (const char* text : {"ANAV 2\n0 0 300 240 0\n", "ANAV 1\n0 0 0 240 0\n",
                             "ANAV 1\n0 0 300 240 0.2\n", "ANAV 1\n0 0 300 240\n",
                             "ANAV 1\n0 0 300 240 0 trailing\n", "ANAV 1\nnan 0 300 240 0\n"}) {
        WriteContract(text); LoadAndExpectRejected();
    }
}
TEST_F(AuthoredNavigationTest, OriginalGroundToleranceRejectsThreeCentimeterOrRaisedFootPoint) {
    NavigationSystem::LoadNavBins();
    auto* nav = SceneNavManager::Instance().Get(SceneId());
    ASSERT_NE(nullptr, nav);
    for (double height : {-0.03, 0.03, 0.2, std::numeric_limits<double>::quiet_NaN()}) {
        Location target = spawn, accepted; target.set_z(height);
        EXPECT_FALSE(NavQuerySystem::SnapToMesh(*nav, target, accepted));
        EXPECT_FALSE(NavQuerySystem::ValidateMove(*nav, spawn, target, accepted));
    }
}
TEST_F(AuthoredNavigationTest, HalfOpenBoundsRejectExactEastEdgeAndOutside) {
    // 把有效东边界放在原出生点上；即使附近有同一多边形，也必须拒绝吸附和注册。
    std::ostringstream text;
    text << std::setprecision(17) << "ANAV 1\n0 0 " << spawn.y() << " 300 0\n";
    WriteContract(text.str()); LoadAndExpectRejected();
}
} // namespace

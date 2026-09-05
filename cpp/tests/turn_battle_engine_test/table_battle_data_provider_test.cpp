#include <gtest/gtest.h>

#include <filesystem>
#include <system_error>
#include <vector>

#include "../test_config_helper.h"
#include "data/table_battle_data_provider.h"
#include "table/code/class_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/monster_table.h"

// TableBattleDataProvider 对「真实导出表」(generated/tables/*.pb)的契约测试,
// 设计文档 §15(PVE 数据化)。2026-09-02「开局秒杀」事故的根因就是 Monster 表只有 id 列、
// Class 表没有 init_* 列 —— 引擎侧回退常量把空表掩盖过去了,只有对真表断言才拦得住。
// 覆盖:副本怪物组 → 怪物属性/奖励齐备 → 组内引用完整性 → 职业初始属性 → 各职业初值一致。
//
// 需要能定位 bin/etc 配置(TableDataDirectory 相对 bin 解析)。从仓库根目录、bin 目录
// 或 build/cpp/tests 下运行都能找到:逐级往上找含 bin/etc/base_deploy_config.yaml 的目录。
// **找不到就判红,不跳过** —— 这组用例的全部价值就是「真的读了真表」,
// 静默 SKIP 会让空表事故照样放行,而退出码仍是 0(仓库其余 6 个测试工程同样是找不到配置即失败)。

namespace {

// 逐级上溯定位运行时目录并 chdir 进去;成功后 cwd 下有 etc/base_deploy_config.yaml。
bool ChdirToRuntimeDir() {
    namespace fs = std::filesystem;
    std::error_code ec;
    auto dir = fs::current_path(ec);
    if (ec) {
        return false;
    }
    for (int depth = 0; depth < 8; ++depth) {
        if (fs::exists(dir / "etc" / "base_deploy_config.yaml")) {
            fs::current_path(dir, ec);
            return !ec;
        }
        if (fs::exists(dir / "bin" / "etc" / "base_deploy_config.yaml")) {
            fs::current_path(dir / "bin", ec);
            return !ec;
        }
        if (!dir.has_parent_path() || dir.parent_path() == dir) {
            break;
        }
        dir = dir.parent_path();
    }
    return false;
}

class TableBattleDataProviderTest : public ::testing::Test {
protected:
    static void SetUpTestSuite() {
        std::error_code ec;
        originalDir = std::filesystem::current_path(ec);
        if (!ChdirToRuntimeDir()) {
            return;
        }
        // 配置定位复用各测试工程共用的 helper(cwd 已是运行时目录,不需要 argv)
        tablesLoaded = test_config::FindAndLoadTestConfig(0, nullptr);
        if (!tablesLoaded) {
            return;
        }
        ClassTableManager::Instance().Load();
        DungeonTableManager::Instance().Load();
        MonsterTableManager::Instance().Load();
    }

    static void TearDownTestSuite() {
        // cwd 是进程级状态,别把后续测试带跑偏
        std::error_code ec;
        std::filesystem::current_path(originalDir, ec);
    }

    void SetUp() override {
        // 不用 GTEST_SKIP:跳过 = 绿色但没跑,正是这组用例要防的事故形态。
        // 注意断言放在 SetUp 而非 SetUpTestSuite:同一 exe 里另外几十个用例不依赖配表,
        // 在 main() 里硬失败会误杀它们,fixture 内失败只影响这三条。
        ASSERT_TRUE(tablesLoaded)
            << "未能定位运行时目录(需要 etc/base_deploy_config.yaml 与 etc/game_config.yaml 同时存在)。"
               "真实配表契约测试必须真跑 —— 静默跳过会让 Monster 表退回只有 id 列、"
               "Class 表缺 init_* 列这类事故(2026-09-02 开局秒杀)照样放行。";
    }

    static std::filesystem::path originalDir;
    static bool tablesLoaded;
};

std::filesystem::path TableBattleDataProviderTest::originalDir;
bool TableBattleDataProviderTest::tablesLoaded = false;

}  // namespace

// 副本怪物组与 data/Dungeon.xlsx 的 monster 列一致(0 值补位不能进组);未配置副本返回空
TEST_F(TableBattleDataProviderTest, DungeonMonsterGroupsMatchExportedTable) {
    turnbattle::TableBattleDataProvider provider;
    EXPECT_EQ(provider.GetDungeonMonsterIds(1), (std::vector<uint32_t>{1, 2}));
    EXPECT_EQ(provider.GetDungeonMonsterIds(2), (std::vector<uint32_t>{6, 7}));
    EXPECT_EQ(provider.GetDungeonMonsterIds(3), (std::vector<uint32_t>{11, 12, 16}));
    EXPECT_TRUE(provider.GetDungeonMonsterIds(999).empty());
}

// 每个配置怪物都要有能打的属性与奖励;副本引用的怪物必须存在
TEST_F(TableBattleDataProviderTest, EveryConfiguredMonsterHasCombatStatsAndRewards) {
    turnbattle::TableBattleDataProvider provider;
    const auto& monsters = MonsterTableManager::Instance().FindAll().data();
    ASSERT_GT(monsters.size(), 0) << "Monster 表为空 = PVE 全靠引擎回退常量";
    for (const auto& row : monsters) {
        const auto* found = provider.FindMonster(row.id());
        ASSERT_NE(found, nullptr) << "monster id=" << row.id();
        EXPECT_GT(found->health(), 0u) << "0 血怪物开局即死(秒杀事故) id=" << row.id();
        EXPECT_GT(found->strength(), 0u) << "0 力量怪物打不出伤害 id=" << row.id();
        EXPECT_GT(found->speed(), 0u) << "0 速度怪物破坏出手序 id=" << row.id();
        EXPECT_GT(found->exp_reward(), 0u) << "无经验奖励 id=" << row.id();
        EXPECT_GT(found->gold_reward(), 0u) << "无金币奖励 id=" << row.id();
    }

    ASSERT_GT(DungeonTableManager::Instance().FindAll().data().size(), 0) << "Dungeon 表为空 = 没有可打的副本";
    for (const auto& dungeon : DungeonTableManager::Instance().FindAll().data()) {
        const auto group = provider.GetDungeonMonsterIds(dungeon.id());
        EXPECT_FALSE(group.empty()) << "dungeon " << dungeon.id() << " 没配怪物组";
        for (const uint32_t monsterId : group) {
            EXPECT_NE(provider.FindMonster(monsterId), nullptr)
                << "dungeon " << dungeon.id() << " 引用了不存在的怪物 " << monsterId;
        }
    }

    EXPECT_EQ(provider.FindMonster(0), nullptr);
    EXPECT_EQ(provider.FindMonster(999999), nullptr);
}

// 职业表首行有正的初始属性(登录初始化 / 阵亡复活 / 二级属性重算都读它)
TEST_F(TableBattleDataProviderTest, ClassTableProvidesPositiveInitialAttributes) {
    const auto& rows = ClassTableManager::Instance().FindAll().data();
    ASSERT_GT(rows.size(), 0) << "Class 表为空 = 新号属性全 0";
    const auto& cls = rows.Get(0);
    EXPECT_GT(cls.init_health(), 0u);
    EXPECT_GT(cls.init_mana(), 0u);
    EXPECT_GT(cls.init_strength(), 0u);
    EXPECT_GT(cls.init_speed(), 0u);
}

// 生产代码(player_database_loader.cpp 与 player_attribute.cpp 的 ResolveClassRow)
// 在 class_id 打通前一律取首行,前提是「各职业初值相同」。
// 一旦策划给某个职业配了不同初值,这个前提就悄悄破了:玩家看到的初始属性与自己的职业无关。
// 这条断言把前提钉死 —— 要么保持一致,要么先把 class_id 打通再改表。
TEST_F(TableBattleDataProviderTest, AllClassRowsShareSameInitialAttributes) {
    const auto& rows = ClassTableManager::Instance().FindAll().data();
    ASSERT_GT(rows.size(), 0);
    const auto& first = rows.Get(0);
    for (const auto& row : rows) {
        EXPECT_EQ(row.init_health(), first.init_health()) << "class id=" << row.id();
        EXPECT_EQ(row.init_mana(), first.init_mana()) << "class id=" << row.id();
        EXPECT_EQ(row.init_strength(), first.init_strength()) << "class id=" << row.id();
        EXPECT_EQ(row.init_armor(), first.init_armor()) << "class id=" << row.id();
        EXPECT_EQ(row.init_resistance(), first.init_resistance()) << "class id=" << row.id();
        EXPECT_EQ(row.init_critchance(), first.init_critchance()) << "class id=" << row.id();
        EXPECT_EQ(row.init_speed(), first.init_speed()) << "class id=" << row.id();
    }
}

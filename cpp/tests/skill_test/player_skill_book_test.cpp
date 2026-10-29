#include "gtest/gtest.h"

#include <cstdint>
#include <vector>

#include <google/protobuf/unknown_field_set.h>

#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/player_level_rules.h"
#include "player/system/player_skill.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "services/battle/system/class_skill_rules.h"
#include "table/code/skill_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include <thread_context/ecs_context.h>

// PlayerSkillSystem 的技能书 / 修习 / 加载整理 / 战斗快照 / 被动加成(docs/design/class-skills.md §3~§7)。
// 读真表(generated/tables:Skill / Class / Buff 由 skill_test.cpp 的 main 加载),技能 id 跟着 data/Skill.xlsx 走。
// 只挂技能书需要的组件:属性重算缺输入时会记 ERROR 并跳过,不影响这里断言的技能与货币状态。

namespace {

constexpr uint32_t kPoZhenJingFeng = 1101;   // 破军 主动 解锁 1(初始)
constexpr uint32_t kHengGeLieYue = 1102;     // 破军 主动 解锁 10
constexpr uint32_t kTieGuZhuHun = 1106;      // 破军 被动 防御 8%,解锁 1(初始)
constexpr uint32_t kXuanFuLuoYao = 1201;     // 玄霄 主动 解锁 1(初始)
constexpr uint32_t kLingTaiNingYuan = 1206;  // 玄霄 被动 解锁 1(初始)
constexpr uint32_t kTingFengShiXi = 1406;    // 逐风 被动 暴击 5 个百分点,解锁 1
constexpr uint32_t kLegacySkill = 1;         // 旧通用占位技能

class PlayerSkillBookTest : public ::testing::Test {
protected:
    void TearDown() override {
        for (const auto player : players) {
            if (tlsEcs.actorRegistry.valid(player)) {
                tlsEcs.actorRegistry.destroy(player);
            }
        }
    }

    entt::entity NewPlayer(uint32_t classId, uint32_t level, uint64_t gold) {
        const auto player = tlsEcs.actorRegistry.create();
        players.push_back(player);
        tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player).set_class_(classId);
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(level);
        tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        if (gold > 0) {
            EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, static_cast<int64_t>(gold)));
        }
        return player;
    }

    // 返回刚加的条目,给要在条目上挂未知字段 / 内部 id 的用例用;列表被整体替换(SanitizeSkillList 剔除了条目)后失效
    static PlayerSkillComp* Own(entt::entity player, uint32_t skillTableId, uint32_t rank) {
        auto* entry = tlsEcs.actorRegistry.get<PlayerSkillListComp>(player).add_skill_list();
        entry->set_skill_table_id(skillTableId);
        entry->set_rank(rank);
        return entry;
    }

    static const SkillTable* Row(uint32_t skillTableId) {
        return SkillTableManager::Instance().FindByIdSilent(skillTableId).first;
    }

    static std::vector<uint32_t> OwnedIds(entt::entity player) {
        std::vector<uint32_t> ids;
        for (const auto& skill : tlsEcs.actorRegistry.get<PlayerSkillListComp>(player).skill_list()) {
            ids.push_back(skill.skill_table_id());
        }
        return ids;
    }

    static uint32_t StoredRank(entt::entity player, uint32_t skillTableId) {
        for (const auto& skill : tlsEcs.actorRegistry.get<PlayerSkillListComp>(player).skill_list()) {
            if (skill.skill_table_id() == skillTableId) {
                return skill.rank();
            }
        }
        return 0;
    }

    static UpgradeSkillResponse Upgrade(entt::entity player, uint32_t skillTableId, uint32_t expectedRank,
                                        uint32_t count) {
        UpgradeSkillRequest request;
        request.set_skill_table_id(skillTableId);
        request.set_expected_rank(expectedRank);
        request.set_count(count);
        UpgradeSkillResponse response;
        PlayerSkillSystem::UpgradeSkill(player, request, response);
        return response;
    }

    static const SkillCatalogEntry* FindEntry(const ListSkillsResponse& book, uint32_t skillTableId) {
        for (const auto& entry : book.catalog()) {
            if (entry.skill_table_id() == skillTableId) {
                return &entry;
            }
        }
        return nullptr;
    }

    std::vector<entt::entity> players;
};

}  // namespace

// 旧实现遍历所有职业的种子,每个号都拿到 1 / 2 / 13;现在只按本职业 Class.skill 发首个主动 + 首个被动(1 重)
TEST_F(PlayerSkillBookTest, RegisterPlayerSeedsOnlyCurrentClassInitialSkills) {
    const auto player = NewPlayer(2, 1, 0);
    PlayerSkillSystem::RegisterPlayer(player);
    EXPECT_EQ(OwnedIds(player), (std::vector<uint32_t>{kXuanFuLuoYao, kLingTaiNingYuan}));
    EXPECT_EQ(StoredRank(player, kXuanFuLuoYao), 1u);
    EXPECT_EQ(StoredRank(player, kLingTaiNingYuan), 1u);

    PlayerSkillSystem::RegisterPlayer(player);  // 幂等
    EXPECT_EQ(OwnedIds(player).size(), 2u);
}

// 加载整理只剔除与版本无关的坏记录(ID 0、重复 ID 保留首条),其余原样携带(混版本设计 §12.1 D5 / D6):
// 旧通用技能写着的 rank、超过本版本最高重的重数、别的职业技能、查不到表的技能都不改写,生效与否 / 生效几重
// 由读路径按本版本的表判定;rank 0 的本职业初始种子即"未学",原地补回 1 重、位置不变
TEST_F(PlayerSkillBookTest, InitializeOnLoadDropsOnlyZeroAndDuplicateIdsAndCarriesTheRest) {
    const auto* hengGe = Row(kHengGeLieYue);
    ASSERT_NE(hengGe, nullptr);
    ASSERT_EQ(classskill::RankLimit(30, hengGe->unlock_level(), hengGe->max_rank()), hengGe->max_rank())
        << "夹具前提:30 级已能修满 1102";
    const uint32_t aboveMax = hengGe->max_rank() + 5;

    const auto player = NewPlayer(1, 30, 0);
    Own(player, kLegacySkill, 5);          // 旧通用技能:rank 读写都忽略,原样携带
    Own(player, kXuanFuLuoYao, 3);         // 别的职业
    Own(player, kPoZhenJingFeng, 0);       // rank 0 的本职业初始种子:未学,原地补 1 重
    Own(player, kHengGeLieYue, aboveMax);  // 超过本版本最高重:不压回,按上限生效
    Own(player, kHengGeLieYue, 2);         // 重复:剔除,保留首条
    Own(player, 999999, 1);                // 表里没有
    Own(player, 0, 4);                     // ID 0:剔除

    PlayerSkillSystem::InitializeOnLoad(player);

    EXPECT_EQ(OwnedIds(player),
              (std::vector<uint32_t>{kLegacySkill, kXuanFuLuoYao, kPoZhenJingFeng, kHengGeLieYue, 999999, kTieGuZhuHun}));
    EXPECT_EQ(StoredRank(player, kLegacySkill), 5u);         // 不归零
    EXPECT_EQ(StoredRank(player, kXuanFuLuoYao), 3u);        // 保留,不生效
    EXPECT_EQ(StoredRank(player, kPoZhenJingFeng), 1u);      // 原地补 1 重,位置不变
    EXPECT_EQ(StoredRank(player, kHengGeLieYue), aboveMax);  // 首条的存储重数原样保留
    EXPECT_EQ(StoredRank(player, 999999), 1u);               // 未知技能及等级保留但不生效
    EXPECT_EQ(StoredRank(player, kTieGuZhuHun), 1u);         // 缺的初始被动照常补在末尾
    EXPECT_FALSE(PlayerSkillSystem::HasSkill(player, 999999));
    EXPECT_FALSE(PlayerSkillSystem::HasSkill(player, kLegacySkill));
    EXPECT_FALSE(PlayerSkillSystem::HasSkill(player, kXuanFuLuoYao));
    EXPECT_TRUE(PlayerSkillSystem::HasSkill(player, kHengGeLieYue));
    EXPECT_TRUE(PlayerSkillSystem::HasSkill(player, kPoZhenJingFeng));
    EXPECT_EQ(PlayerSkillSystem::EffectiveRank(player, kHengGeLieYue), hengGe->max_rank());
    EXPECT_EQ(PlayerSkillSystem::EffectiveRank(player, kXuanFuLuoYao), 0u);
    EXPECT_EQ(PlayerSkillSystem::EffectiveRank(player, kLegacySkill), 0u);

    ListSkillsResponse book;
    ASSERT_EQ(PlayerSkillSystem::BuildSkillBook(player, book), kSuccess);
    ASSERT_TRUE(book.has_skill_list());
    std::vector<uint32_t> usable;
    for (const auto& skill : book.skill_list().skill_list()) {
        usable.push_back(skill.skill_table_id());
    }
    EXPECT_EQ(usable, (std::vector<uint32_t>{kPoZhenJingFeng, kHengGeLieYue, kTieGuZhuHun}));
    ASSERT_EQ(book.catalog_size(), 8);
    for (int index = 0; index < book.catalog_size(); ++index) {
        EXPECT_EQ(book.catalog(index).skill_table_id(), static_cast<uint32_t>(1101 + index));
        EXPECT_EQ(book.catalog(index).class_id(), 1u);
    }
    // 目录 rank 是存储重数(UpgradeSkill.expected_rank 的口径);高于上限时按满重阻断
    const auto* hengGeView = FindEntry(book, kHengGeLieYue);
    ASSERT_NE(hengGeView, nullptr);
    EXPECT_EQ(hengGeView->rank(), aboveMax);
    EXPECT_EQ(hengGeView->next_rank_cost(), 0u);
    EXPECT_EQ(hengGeView->upgrade_block_tip_id(), static_cast<uint32_t>(kSkillMaxRankReached));
    EXPECT_EQ(book.class_id(), 1u);
    EXPECT_EQ(book.player_level(), 30u);

    // 再加载一次什么都不变(同一实例前后比较)
    const auto settled = tlsEcs.actorRegistry.get<PlayerSkillListComp>(player).SerializeAsString();
    PlayerSkillSystem::InitializeOnLoad(player);
    EXPECT_EQ(settled, tlsEcs.actorRegistry.get<PlayerSkillListComp>(player).SerializeAsString());
}

// 还没有职业技能的职业(5..9)继续用旧通用技能
TEST_F(PlayerSkillBookTest, ClassWithoutClassSkillsKeepsLegacySkills) {
    const auto player = NewPlayer(5, 10, 0);
    Own(player, 1, 0);
    Own(player, 2, 0);
    PlayerSkillSystem::InitializeOnLoad(player);
    EXPECT_TRUE(PlayerSkillSystem::HasSkill(player, 1));
    EXPECT_TRUE(PlayerSkillSystem::HasSkill(player, 2));
    ListSkillsResponse book;
    ASSERT_EQ(PlayerSkillSystem::BuildSkillBook(player, book), kSuccess);
    EXPECT_EQ(book.catalog_size(), 0);
    EXPECT_EQ(book.skill_list().skill_list_size(), 2);
}

TEST_F(PlayerSkillBookTest, UpgradeLearnsSkillAndChargesExactCost) {
    const auto player = NewPlayer(1, 30, 100000);
    const auto response = Upgrade(player, kHengGeLieYue, 0, 1);
    EXPECT_FALSE(response.has_error_message());
    EXPECT_EQ(response.upgraded_count(), 1u);
    EXPECT_EQ(response.stop_tip_id(), 0u);
    EXPECT_EQ(StoredRank(player, kHengGeLieYue), 1u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 100000u - classskill::UpgradeCost(1, 10));

    ASSERT_TRUE(response.has_state());
    EXPECT_EQ(response.state().currency_balance(), CurrencySystem::GetBalance(player, kCurrencyGold));
    const auto* entry = FindEntry(response.state(), kHengGeLieYue);
    ASSERT_NE(entry, nullptr);
    EXPECT_EQ(entry->rank(), 1u);
    EXPECT_EQ(entry->rank_limit(), 10u);
    EXPECT_EQ(entry->next_rank_cost(), classskill::UpgradeCost(2, 10));
    EXPECT_EQ(entry->upgrade_block_tip_id(), 0u);
    EXPECT_FALSE(entry->passive());
    EXPECT_EQ(entry->icon_key(), "UI/Skills/1102");
}

// 被动修习后立刻重算属性(本夹具不挂属性组件,重算记 ERROR 跳过),等级照常落下
TEST_F(PlayerSkillBookTest, UpgradingPassiveRaisesRank) {
    const auto player = NewPlayer(1, 30, 100000);
    Own(player, kTieGuZhuHun, 1);
    const auto response = Upgrade(player, kTieGuZhuHun, 1, 1);
    EXPECT_FALSE(response.has_error_message());
    EXPECT_EQ(StoredRank(player, kTieGuZhuHun), 2u);
    const auto* entry = FindEntry(response.state(), kTieGuZhuHun);
    ASSERT_NE(entry, nullptr);
    EXPECT_TRUE(entry->passive());
    EXPECT_EQ(entry->rank(), 2u);
}

TEST_F(PlayerSkillBookTest, TenUpgradesStopAtLevelLimitAndReportWhy) {
    const auto player = NewPlayer(1, 13, 100000);  // 1102 解锁 10:13 级上限 = 1 + 3 / 2 = 2
    const auto response = Upgrade(player, kHengGeLieYue, 0, 10);
    EXPECT_FALSE(response.has_error_message());
    EXPECT_EQ(response.upgraded_count(), 2u);
    EXPECT_EQ(response.stop_tip_id(), static_cast<uint32_t>(kSkillLevelNotEnough));
    EXPECT_EQ(StoredRank(player, kHengGeLieYue), 2u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold),
              100000u - classskill::UpgradeCost(1, 10) - classskill::UpgradeCost(2, 10));
    const auto* entry = FindEntry(response.state(), kHengGeLieYue);
    ASSERT_NE(entry, nullptr);
    EXPECT_EQ(entry->upgrade_block_tip_id(), static_cast<uint32_t>(kSkillLevelNotEnough));
}

TEST_F(PlayerSkillBookTest, TenUpgradesStopWhenGoldRunsOut) {
    const uint64_t gold = classskill::UpgradeCost(1, 10) + classskill::UpgradeCost(2, 10) + 50;
    const auto player = NewPlayer(1, 30, gold);
    const auto response = Upgrade(player, kHengGeLieYue, 0, 10);
    EXPECT_FALSE(response.has_error_message());
    EXPECT_EQ(response.upgraded_count(), 2u);
    EXPECT_EQ(response.stop_tip_id(), static_cast<uint32_t>(kSkillGoldNotEnough));
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 50u);
}

// 整笔拒绝:一分钱不扣、等级不动,回包仍带完整技能书
TEST_F(PlayerSkillBookTest, RejectedRequestsNeverChargeOrChangeRank) {
    const auto player = NewPlayer(1, 30, 100000);
    const struct {
        uint32_t skill;
        uint32_t expected;
        uint32_t count;
        uint32_t tip;
    } cases[] = {
        {kHengGeLieYue, 0, 2, kSkillUpgradeCountInvalid},
        {kHengGeLieYue, 0, 0, kSkillUpgradeCountInvalid},
        {kXuanFuLuoYao, 0, 1, kSkillNotInClass},
        {kLegacySkill, 0, 1, kSkillNotInClass},
        {999999, 0, 1, kInvalidTableId},
        {kHengGeLieYue, 3, 1, kSkillRankChanged},
    };
    for (const auto& item : cases) {
        const auto response = Upgrade(player, item.skill, item.expected, item.count);
        ASSERT_TRUE(response.has_error_message()) << "skill " << item.skill;
        EXPECT_EQ(response.error_message().id(), item.tip) << "skill " << item.skill;
        EXPECT_EQ(response.upgraded_count(), 0u);
        EXPECT_TRUE(response.has_state());
    }
    EXPECT_EQ(StoredRank(player, kHengGeLieYue), 0u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 100000u);
}

// 同一个旧请求重放:第二次 expected_rank 已经对不上,不会再扣一次
TEST_F(PlayerSkillBookTest, ReplayedRequestIsChargedOnlyOnce) {
    const auto player = NewPlayer(1, 30, 100000);
    ASSERT_EQ(Upgrade(player, kHengGeLieYue, 0, 1).upgraded_count(), 1u);
    const auto replay = Upgrade(player, kHengGeLieYue, 0, 1);
    ASSERT_TRUE(replay.has_error_message());
    EXPECT_EQ(replay.error_message().id(), static_cast<uint32_t>(kSkillRankChanged));
    EXPECT_EQ(StoredRank(player, kHengGeLieYue), 1u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 100000u - classskill::UpgradeCost(1, 10));
}

TEST_F(PlayerSkillBookTest, NoGoldOrMaxRankRejectsWithReason) {
    const auto broke = NewPlayer(1, 30, 0);
    auto response = Upgrade(broke, kHengGeLieYue, 0, 1);
    ASSERT_TRUE(response.has_error_message());
    EXPECT_EQ(response.error_message().id(), static_cast<uint32_t>(kSkillGoldNotEnough));

    const auto maxed = NewPlayer(1, 30, 100000);
    Own(maxed, kHengGeLieYue, 10);
    response = Upgrade(maxed, kHengGeLieYue, 10, 1);
    ASSERT_TRUE(response.has_error_message());
    EXPECT_EQ(response.error_message().id(), static_cast<uint32_t>(kSkillMaxRankReached));
    EXPECT_EQ(CurrencySystem::GetBalance(maxed, kCurrencyGold), 100000u);

    const auto young = NewPlayer(1, 9, 100000);  // 1102 要 10 级
    response = Upgrade(young, kHengGeLieYue, 0, 1);
    ASSERT_TRUE(response.has_error_message());
    EXPECT_EQ(response.error_message().id(), static_cast<uint32_t>(kSkillLevelNotEnough));
}

// 战斗在途 / 跨 zone 冻结:持久化数据不写,技能书标出原因
TEST_F(PlayerSkillBookTest, BattleOrFreezeWindowRejectsWithoutCharging) {
    const auto player = NewPlayer(1, 30, 100000);
    tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    auto response = Upgrade(player, kHengGeLieYue, 0, 1);
    ASSERT_TRUE(response.has_error_message());
    EXPECT_EQ(response.error_message().id(), static_cast<uint32_t>(kSkillUpgradeBusy));
    const auto* entry = FindEntry(response.state(), kHengGeLieYue);
    ASSERT_NE(entry, nullptr);
    EXPECT_EQ(entry->upgrade_block_tip_id(), static_cast<uint32_t>(kSkillUpgradeBusy));
    tlsEcs.actorRegistry.remove<InBattleComp>(player);

    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    response = Upgrade(player, kHengGeLieYue, 0, 1);
    ASSERT_TRUE(response.has_error_message());
    EXPECT_EQ(response.error_message().id(), static_cast<uint32_t>(kSkillUpgradeBusy));
    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);

    EXPECT_EQ(StoredRank(player, kHengGeLieYue), 0u);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 100000u);
    EXPECT_EQ(Upgrade(player, kHengGeLieYue, 0, 1).upgraded_count(), 1u);  // 窗口过去就能修
}

// 战斗快照:可施放列表只有生效的主动技能,等级表含被动;不生效的条目一律不带
TEST_F(PlayerSkillBookTest, BattleSnapshotCarriesRanksAndCastableIdsOnly) {
    const auto player = NewPlayer(1, 30, 0);
    Own(player, kPoZhenJingFeng, 3);
    Own(player, kTieGuZhuHun, 2);
    Own(player, kLegacySkill, 0);       // 已退役
    Own(player, kXuanFuLuoYao, 1);      // 别的职业
    BattlePlayerSnapshot snapshot;
    PlayerSkillSystem::FillBattleSnapshot(player, snapshot);
    ASSERT_EQ(snapshot.skill_table_ids_size(), 1);
    EXPECT_EQ(snapshot.skill_table_ids(0), kPoZhenJingFeng);
    ASSERT_EQ(snapshot.skill_ranks().size(), 2u);
    EXPECT_EQ(snapshot.skill_ranks().at(kPoZhenJingFeng), 3u);
    EXPECT_EQ(snapshot.skill_ranks().at(kTieGuZhuHun), 2u);
}

// 被动加成按生效等级算:存档 10 重但角色 1 级时只按 1 重给
TEST_F(PlayerSkillBookTest, PassiveBonusUsesEffectiveRank) {
    const auto veteran = NewPlayer(1, 30, 0);
    Own(veteran, kTieGuZhuHun, 10);
    EXPECT_DOUBLE_EQ(PlayerSkillSystem::CollectPassiveBonus(veteran).defense,
                     0.08 * classskill::PassiveRankFactor(10));

    const auto rookie = NewPlayer(1, 1, 0);
    Own(rookie, kTieGuZhuHun, 10);
    EXPECT_DOUBLE_EQ(PlayerSkillSystem::CollectPassiveBonus(rookie).defense, 0.08);

    const auto scout = NewPlayer(4, 1, 0);
    Own(scout, kTingFengShiXi, 1);
    EXPECT_DOUBLE_EQ(PlayerSkillSystem::CollectPassiveBonus(scout).critPercentPoints, 5.0);
    EXPECT_DOUBLE_EQ(PlayerSkillSystem::CollectPassiveBonus(scout).defense, 0.0);
}

// ---------------------------------------------------------------------------
// 混版本:加载与重算不改写持久值(docs/design/mixed-version-persist-field-protection.md §12.1 D5 / D6)
// ---------------------------------------------------------------------------

// 新版本调大 max_rank、玩家修到更高重后回滚 scene:本版本加载不压回存储重数,读路径一律按本版本上限生效。
// 升回新版本后玩家修过的重数还在
TEST_F(PlayerSkillBookTest, RollbackRankAboveTableMaxIsStoredButEffectiveAtMax) {
    const auto* active = Row(kPoZhenJingFeng);
    const auto* passive = Row(kTieGuZhuHun);
    ASSERT_NE(active, nullptr);
    ASSERT_NE(passive, nullptr);
    ASSERT_EQ(classskill::RankLimit(30, active->unlock_level(), active->max_rank()), active->max_rank());
    ASSERT_EQ(classskill::RankLimit(30, passive->unlock_level(), passive->max_rank()), passive->max_rank());
    const uint32_t activeStored = active->max_rank() + 3;
    const uint32_t passiveStored = passive->max_rank() + 7;

    const auto player = NewPlayer(1, 30, 0);
    Own(player, kPoZhenJingFeng, activeStored);
    Own(player, kTieGuZhuHun, passiveStored);

    PlayerSkillSystem::InitializeOnLoad(player);

    EXPECT_EQ(OwnedIds(player), (std::vector<uint32_t>{kPoZhenJingFeng, kTieGuZhuHun}));  // 种子都已学,不补
    EXPECT_EQ(StoredRank(player, kPoZhenJingFeng), activeStored);
    EXPECT_EQ(StoredRank(player, kTieGuZhuHun), passiveStored);
    EXPECT_EQ(PlayerSkillSystem::EffectiveRank(player, kPoZhenJingFeng), active->max_rank());
    EXPECT_EQ(PlayerSkillSystem::EffectiveRank(player, kTieGuZhuHun), passive->max_rank());

    BattlePlayerSnapshot snapshot;
    PlayerSkillSystem::FillBattleSnapshot(player, snapshot);
    ASSERT_EQ(snapshot.skill_ranks().size(), 2u);
    EXPECT_EQ(snapshot.skill_ranks().at(kPoZhenJingFeng), active->max_rank());
    EXPECT_EQ(snapshot.skill_ranks().at(kTieGuZhuHun), passive->max_rank());
    EXPECT_DOUBLE_EQ(PlayerSkillSystem::CollectPassiveBonus(player).defense,
                     passive->passive_percent() * classskill::PassiveRankFactor(passive->max_rank()));
}

// 存储重数高于本版本上限时修习一步不走:回满重、一分钱不扣、存储重数不动(不压回,也不会算 rank + 1 回绕)
TEST_F(PlayerSkillBookTest, UpgradeRefusesRankAboveTableMaxWithoutCharging) {
    const auto* row = Row(kHengGeLieYue);
    ASSERT_NE(row, nullptr);
    const uint32_t aboveMax = row->max_rank() + 5;
    const auto player = NewPlayer(1, 30, 100000);
    Own(player, kHengGeLieYue, aboveMax);

    for (const uint32_t count : {1u, 10u}) {
        const auto response = Upgrade(player, kHengGeLieYue, aboveMax, count);
        ASSERT_TRUE(response.has_error_message()) << "count " << count;
        EXPECT_EQ(response.error_message().id(), static_cast<uint32_t>(kSkillMaxRankReached)) << "count " << count;
        EXPECT_EQ(response.upgraded_count(), 0u);
        ASSERT_TRUE(response.has_state());
        const auto* view = FindEntry(response.state(), kHengGeLieYue);
        ASSERT_NE(view, nullptr);
        EXPECT_EQ(view->rank(), aboveMax);
        EXPECT_EQ(view->upgrade_block_tip_id(), static_cast<uint32_t>(kSkillMaxRankReached));
    }
    EXPECT_EQ(StoredRank(player, kHengGeLieYue), aboveMax);
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold), 100000u);
}

// rank 0 的职业技能条目与未学同义(例如新版本把通用技能改成职业技能,条目带着 rank 0 回来):
// 加载不删,读路径都当没学;之后修习原地复用这一条,不会出现两条同 id
TEST_F(PlayerSkillBookTest, RankZeroClassSkillIsUnlearnedButCarried) {
    const auto* row = Row(kHengGeLieYue);
    ASSERT_NE(row, nullptr);
    const auto player = NewPlayer(1, 30, 100000);
    Own(player, kHengGeLieYue, 0)->mutable_unknown_fields()->AddVarint(412346, 9);  // 1102 不是初始种子

    PlayerSkillSystem::InitializeOnLoad(player);

    ASSERT_EQ(OwnedIds(player), (std::vector<uint32_t>{kHengGeLieYue, kPoZhenJingFeng, kTieGuZhuHun}));
    EXPECT_EQ(StoredRank(player, kHengGeLieYue), 0u);
    EXPECT_FALSE(PlayerSkillSystem::HasSkill(player, kHengGeLieYue));
    EXPECT_EQ(PlayerSkillSystem::EffectiveRank(player, kHengGeLieYue), 0u);
    BattlePlayerSnapshot snapshot;
    PlayerSkillSystem::FillBattleSnapshot(player, snapshot);
    EXPECT_EQ(snapshot.skill_ranks().count(kHengGeLieYue), 0u);
    for (const auto castable : snapshot.skill_table_ids()) {
        EXPECT_NE(castable, kHengGeLieYue);
    }

    ListSkillsResponse book;
    ASSERT_EQ(PlayerSkillSystem::BuildSkillBook(player, book), kSuccess);
    for (const auto& skill : book.skill_list().skill_list()) {
        EXPECT_NE(skill.skill_table_id(), kHengGeLieYue) << "未学的条目不进已拥有列表";
    }
    const auto* view = FindEntry(book, kHengGeLieYue);
    ASSERT_NE(view, nullptr);
    EXPECT_EQ(view->rank(), 0u);
    EXPECT_EQ(view->upgrade_block_tip_id(), 0u);

    const auto response = Upgrade(player, kHengGeLieYue, 0, 1);
    EXPECT_FALSE(response.has_error_message());
    EXPECT_EQ(response.upgraded_count(), 1u);
    EXPECT_EQ(OwnedIds(player), (std::vector<uint32_t>{kHengGeLieYue, kPoZhenJingFeng, kTieGuZhuHun}))
        << "复用 rank 0 的条目,不另加一条";
    EXPECT_EQ(StoredRank(player, kHengGeLieYue), 1u);
    EXPECT_EQ(tlsEcs.actorRegistry.get<PlayerSkillListComp>(player).skill_list(0).unknown_fields().field_count(), 1)
        << "原地改等级,条目上的未知字段还在";
    EXPECT_EQ(CurrencySystem::GetBalance(player, kCurrencyGold),
              100000u - classskill::UpgradeCost(1, row->unlock_level()));
}

// rank 0 的本职业初始种子是"未学":加载时原地补 1 重(复用条目,位置与条目上的未知字段不动);再跑一遍不变
TEST_F(PlayerSkillBookTest, RankZeroClassSeedIsRegrantedInPlace) {
    const auto player = NewPlayer(1, 1, 0);
    Own(player, kTieGuZhuHun, 0)->mutable_unknown_fields()->AddVarint(412346, 9);

    PlayerSkillSystem::InitializeOnLoad(player);

    EXPECT_EQ(OwnedIds(player), (std::vector<uint32_t>{kTieGuZhuHun, kPoZhenJingFeng}));
    EXPECT_EQ(StoredRank(player, kTieGuZhuHun), 1u);
    EXPECT_EQ(StoredRank(player, kPoZhenJingFeng), 1u);
    EXPECT_TRUE(PlayerSkillSystem::HasSkill(player, kTieGuZhuHun));
    {
        const auto& list = tlsEcs.actorRegistry.get<PlayerSkillListComp>(player);
        ASSERT_EQ(list.skill_list(0).unknown_fields().field_count(), 1);
        EXPECT_EQ(list.skill_list(0).unknown_fields().field(0).number(), 412346);
    }

    const auto settled = tlsEcs.actorRegistry.get<PlayerSkillListComp>(player).SerializeAsString();
    PlayerSkillSystem::InitializeOnLoad(player);
    PlayerSkillSystem::RegisterPlayer(player);
    EXPECT_EQ(settled, tlsEcs.actorRegistry.get<PlayerSkillListComp>(player).SerializeAsString()) << "幂等";
}

// 旧通用技能不分等级:存档里写着的 rank 原样携带(可能是新版本改了归属后写的),读路径全都忽略它 ——
// 技能书回包里 rank 为 0,快照不带等级
TEST_F(PlayerSkillBookTest, LegacySkillRankIsCarriedAndIgnored) {
    const auto player = NewPlayer(5, 10, 0);  // 职业 5 还没有职业技能,种子是旧通用技能 1
    Own(player, kLegacySkill, 7);

    PlayerSkillSystem::InitializeOnLoad(player);

    EXPECT_EQ(OwnedIds(player), (std::vector<uint32_t>{kLegacySkill}));
    EXPECT_EQ(StoredRank(player, kLegacySkill), 7u);
    EXPECT_TRUE(PlayerSkillSystem::HasSkill(player, kLegacySkill));
    EXPECT_EQ(PlayerSkillSystem::EffectiveRank(player, kLegacySkill), 0u);

    ListSkillsResponse book;
    ASSERT_EQ(PlayerSkillSystem::BuildSkillBook(player, book), kSuccess);
    ASSERT_EQ(book.skill_list().skill_list_size(), 1);
    EXPECT_EQ(book.skill_list().skill_list(0).skill_table_id(), kLegacySkill);
    EXPECT_EQ(book.skill_list().skill_list(0).rank(), 0u);

    BattlePlayerSnapshot snapshot;
    PlayerSkillSystem::FillBattleSnapshot(player, snapshot);
    EXPECT_TRUE(snapshot.skill_ranks().empty());
}

// 已拥有列表是显式投影:存档条目上的内部 id 与新版本加的未知字段都不下发,存档条目本身不动
TEST_F(PlayerSkillBookTest, OwnedListIsAnExplicitProjection) {
    constexpr uint64_t kStoredEntryId = (uint64_t{1} << 60) + 851;
    const auto player = NewPlayer(1, 30, 0);
    auto* stored = Own(player, kHengGeLieYue, 3);
    stored->set_id(kStoredEntryId);
    stored->mutable_unknown_fields()->AddVarint(412346, 9);

    ListSkillsResponse book;
    ASSERT_EQ(PlayerSkillSystem::BuildSkillBook(player, book), kSuccess);
    ASSERT_EQ(book.skill_list().skill_list_size(), 1);
    const auto& owned = book.skill_list().skill_list(0);
    EXPECT_EQ(owned.skill_table_id(), kHengGeLieYue);
    EXPECT_EQ(owned.rank(), 3u);
    EXPECT_EQ(owned.id(), 0u);
    EXPECT_EQ(owned.unknown_fields().field_count(), 0);

    const auto& list = tlsEcs.actorRegistry.get<PlayerSkillListComp>(player);
    ASSERT_EQ(list.skill_list_size(), 1);
    EXPECT_EQ(list.skill_list(0).id(), kStoredEntryId);
    EXPECT_EQ(list.skill_list(0).unknown_fields().field_count(), 1);
}

// 存储等级高于本版本 kMaxLevel(新版本调高上限后回滚):技能书按 CappedLevel 报等级、算每重上限,
// LevelComp 的存储值不改写;存储等级 0(新号加载时)按 1 级算
TEST_F(PlayerSkillBookTest, StoredLevelAboveCapReportsCappedPlayerLevel) {
    const uint32_t storedLevel = playerlevel::kMaxLevel + 15;
    const auto veteran = NewPlayer(1, storedLevel, 0);
    ListSkillsResponse book;
    ASSERT_EQ(PlayerSkillSystem::BuildSkillBook(veteran, book), kSuccess);
    EXPECT_EQ(book.player_level(), playerlevel::kMaxLevel);
    ASSERT_GT(book.catalog_size(), 0);
    for (const auto& view : book.catalog()) {
        EXPECT_EQ(view.rank_limit(),
                  classskill::RankLimit(playerlevel::kMaxLevel, view.unlock_level(), view.max_rank()))
            << "skill " << view.skill_table_id();
    }
    EXPECT_EQ(tlsEcs.actorRegistry.get<LevelComp>(veteran).level(), storedLevel);

    const auto fresh = NewPlayer(1, 0, 0);
    ASSERT_EQ(PlayerSkillSystem::BuildSkillBook(fresh, book), kSuccess);
    EXPECT_EQ(book.player_level(), 1u);
    EXPECT_EQ(tlsEcs.actorRegistry.get<LevelComp>(fresh).level(), 0u);
}

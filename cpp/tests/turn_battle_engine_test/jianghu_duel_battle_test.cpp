// 天墉生死状一期回合战斗引擎单测：
//   * 默认 30 回合上限
//   * 打满回合上限双方存活权威判平(BATTLE_OUTCOME_DRAW)
//   * BuildSettlement 原样回显 JianghuDuelBattleContext

#include <gtest/gtest.h>

#include <cstdint>
#include <memory>

#include "memory_battle_data_provider.h"
#include "system/turn_battle_engine.h"

namespace {

using turnbattle::TurnBattleEngine;

BattleAction DefendAction() {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_DEFEND);
    return action;
}

CreateBattleRequest MakeJianghuDuelRequest(uint32_t roundLimitOverride) {
    CreateBattleRequest req;
    req.set_battle_id(99001);
    req.set_battle_config_id(0);
    req.set_match_mode(turnbattle::kMatchModeJianghuDuel);
    req.set_seed(1234567);

    auto* jh = req.mutable_jianghu_duel_context();
    jh->set_duel_invite_id(5001);
    jh->set_writ_tier(2);
    jh->set_challenger_captain_id(1001);
    jh->set_challenger_captain_name("QingFeng");
    jh->set_target_captain_id(2001);
    jh->set_target_captain_name("MoYuan");
    jh->set_side_a_guild_id(801);
    jh->set_side_a_guild_name("QingYun");
    jh->set_side_b_guild_id(802);
    jh->set_side_b_guild_name("CangLan");
    jh->set_round_limit_override(roundLimitOverride);
    jh->set_team_size(1);

    for (const uint64_t pid : {uint64_t{1001}, uint64_t{2001}}) {
        auto* p = req.add_players();
        p->set_player_id(pid);
        p->set_team_index(pid == 1001 ? 0 : 1);
        p->set_player_name(pid == 1001 ? "QingFeng" : "MoYuan");
        p->set_level(50);
        p->set_max_health(5000);
        p->set_max_mana(1000);
        p->mutable_base_attributes()->set_health(5000);
        p->mutable_base_attributes()->set_mana(1000);
        p->mutable_base_attributes()->set_speed(200);
        p->set_physical_attack(300);
        p->set_magic_attack(300);
        p->set_defense(100);
    }
    return req;
}

TEST(JianghuDuelBattleTest, DefaultsToThirtyRounds) {
    auto provider = std::make_shared<MemoryBattleDataProvider>();
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(MakeJianghuDuelRequest(0)));
    EXPECT_TRUE(engine.IsJianghuDuelBattle());
    EXPECT_EQ(turnbattle::kJianghuDuelDefaultMaxRounds, engine.MaxRounds());
}

TEST(JianghuDuelBattleTest, RoundLimitForcesDrawAndEchoesContext) {
    auto provider = std::make_shared<MemoryBattleDataProvider>();
    TurnBattleEngine engine(provider);
    ASSERT_TRUE(engine.Initialize(MakeJianghuDuelRequest(2)));
    EXPECT_EQ(2u, engine.MaxRounds());

    for (uint32_t r = 0; r < 2; ++r) {
        ASSERT_EQ(BATTLE_OUTCOME_ONGOING, engine.Outcome());
        (void)engine.SubmitAction(1001, DefendAction());
        (void)engine.SubmitAction(2001, DefendAction());
        (void)engine.ResolveCurrentRound();
    }

    EXPECT_EQ(BATTLE_OUTCOME_DRAW, engine.Outcome());

    const auto settleA = engine.BuildSettlement(1001);
    EXPECT_EQ(BATTLE_OUTCOME_DRAW, settleA.outcome());
    EXPECT_EQ(2u, settleA.total_rounds());
    ASSERT_TRUE(settleA.has_jianghu_duel());
    EXPECT_EQ(5001u, settleA.jianghu_duel().duel_invite_id());
    EXPECT_EQ(2u, settleA.jianghu_duel().writ_tier());
    EXPECT_EQ(1001u, settleA.jianghu_duel().challenger_captain_id());
    EXPECT_EQ(2001u, settleA.jianghu_duel().target_captain_id());
    EXPECT_EQ(801u, settleA.jianghu_duel().side_a_guild_id());
    EXPECT_EQ(802u, settleA.jianghu_duel().side_b_guild_id());
}

}  // namespace

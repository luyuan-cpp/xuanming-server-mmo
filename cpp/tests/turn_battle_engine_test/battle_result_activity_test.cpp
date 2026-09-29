#include <gtest/gtest.h>

#include <cstdint>
#include <limits>
#include <vector>

#include "services/battle/system/battle_result_activity.h"

// 活动局结果纯函数(battle_result_activity.h)的单元测试,docs/design/guild-phase2/06-activities.md §6.42。
// 覆盖:上下文回显 / 不回显、逃跑与阵亡名单升序去重、重发判定顺序、跨语言持久记录键契约。
// Redis / Kafka 编排在 battle_room_manager.cpp,不在本工程里测(与 settlement_outbox.h 同一拆法)。

using turnbattle::ActivityResultKey;
using turnbattle::ActivityResultRetryAction;
using turnbattle::ClassifyActivityResultRetry;
using turnbattle::FillBattleResultActivityFields;
using turnbattle::IsActivityBattle;

namespace
{
    ::BattleActivityContext MakeGuildTrialContext()
    {
        ::BattleActivityContext activity;
        activity.set_kind(::BATTLE_ACTIVITY_KIND_GUILD_TRIAL);
        activity.set_guild_id(7000000000001ULL);
        activity.set_activity_id(3);
        activity.set_period_key(20260929);
        activity.set_initiator_player_id(9216);
        activity.set_guild_period_key(20260929);
        return activity;
    }

    std::vector<uint64_t> FledOf(const contracts::kafka::BattleResultEvent &event)
    {
        return std::vector<uint64_t>(event.fled_player_ids().begin(), event.fled_player_ids().end());
    }

    std::vector<uint64_t> DeadOf(const contracts::kafka::BattleResultEvent &event)
    {
        return std::vector<uint64_t>(event.dead_player_ids().begin(), event.dead_player_ids().end());
    }
} // namespace

// kind == NONE:即使其它字段非 0 也不写上下文(普通对局),空名单写出来也是空。
TEST(BattleResultActivity, NoneKindLeavesContextUnset)
{
    ::BattleActivityContext activity;
    activity.set_guild_id(123); // kind 仍是 NONE:battle 只按 kind 判断,不看其余字段
    ASSERT_FALSE(IsActivityBattle(activity));

    contracts::kafka::BattleResultEvent event;
    event.set_battle_id(42);
    FillBattleResultActivityFields(activity, {}, {}, event);

    EXPECT_FALSE(event.has_activity_context());
    EXPECT_EQ(event.fled_player_ids_size(), 0);
    EXPECT_EQ(event.dead_player_ids_size(), 0);
    EXPECT_EQ(event.battle_id(), 42u); // 既有字段不被改动
}

// 同道历练:6 个字段逐一原样回显。
TEST(BattleResultActivity, GuildTrialContextEchoed)
{
    const auto activity = MakeGuildTrialContext();
    ASSERT_TRUE(IsActivityBattle(activity));

    contracts::kafka::BattleResultEvent event;
    FillBattleResultActivityFields(activity, {}, {}, event);

    ASSERT_TRUE(event.has_activity_context());
    const auto &echoed = event.activity_context();
    EXPECT_EQ(echoed.kind(), ::BATTLE_ACTIVITY_KIND_GUILD_TRIAL);
    EXPECT_EQ(echoed.guild_id(), 7000000000001ULL);
    EXPECT_EQ(echoed.activity_id(), 3u);
    EXPECT_EQ(echoed.period_key(), 20260929u);
    EXPECT_EQ(echoed.initiator_player_id(), 9216u);
    EXPECT_EQ(echoed.guild_period_key(), 20260929u);
}

// 名单升序去重;普通对局(kind NONE)也照样写名单。
TEST(BattleResultActivity, FledAndDeadSortedDeduplicated)
{
    contracts::kafka::BattleResultEvent event;
    FillBattleResultActivityFields(::BattleActivityContext{}, {9, 3, 9}, {5, 5}, event);

    EXPECT_FALSE(event.has_activity_context());
    EXPECT_EQ(FledOf(event), (std::vector<uint64_t>{3, 9}));
    EXPECT_EQ(DeadOf(event), (std::vector<uint64_t>{5}));
}

// U2(阵亡也得奖)只改 guild 的过滤口径:battle 仍如实回显阵亡名单,逃跑且阵亡的人两份名单都在。
TEST(BattleResultActivity, DeadListStillEchoedForGuildTrial)
{
    contracts::kafka::BattleResultEvent event;
    FillBattleResultActivityFields(MakeGuildTrialContext(), {11}, {12, 11}, event);

    ASSERT_TRUE(event.has_activity_context());
    EXPECT_EQ(FledOf(event), (std::vector<uint64_t>{11}));
    EXPECT_EQ(DeadOf(event), (std::vector<uint64_t>{11, 12}));
}

// proto3 开放枚举:未知 kind 值按活动对局处理(宁可多落一次库,不能把真实活动结果当普通对局)。
TEST(BattleResultActivity, UnknownKindTreatedAsActivity)
{
    auto activity = MakeGuildTrialContext();
    activity.set_kind(static_cast<::eBattleActivityKind>(7));
    ASSERT_TRUE(IsActivityBattle(activity));

    contracts::kafka::BattleResultEvent event;
    FillBattleResultActivityFields(activity, {}, {}, event);
    ASSERT_TRUE(event.has_activity_context());
    EXPECT_EQ(static_cast<int>(event.activity_context().kind()), 7);
}

// 判定顺序:已销账 > 次数用尽 > 重发。
TEST(BattleResultActivity, ClassifyRetryOrder)
{
    EXPECT_EQ(ClassifyActivityResultRetry(false, 99, 30), ActivityResultRetryAction::kDone);
    EXPECT_EQ(ClassifyActivityResultRetry(false, 30, 30), ActivityResultRetryAction::kDone);
    EXPECT_EQ(ClassifyActivityResultRetry(true, 30, 30), ActivityResultRetryAction::kExhausted);
    EXPECT_EQ(ClassifyActivityResultRetry(true, 29, 30), ActivityResultRetryAction::kResend);
    EXPECT_EQ(ClassifyActivityResultRetry(true, 0, 30), ActivityResultRetryAction::kResend);
}

// 跨语言键契约:guild(go/guild/internal/data/trial_result_record.go)按同一字面量读 / 删。
// 改这里的期望值 = 改契约,必须同改 Go 侧常量与其单测。
TEST(BattleResultActivity, ActivityResultKeyMatchesGuildContract)
{
    EXPECT_EQ(ActivityResultKey(123), "battle:activity_result:123");
    EXPECT_EQ(ActivityResultKey(std::numeric_limits<uint64_t>::max()),
              "battle:activity_result:18446744073709551615");
    EXPECT_EQ(turnbattle::kActivityResultTtlSec, 604800u);
    EXPECT_EQ(turnbattle::kActivityResultRetryIntervalSec, 10u);
    EXPECT_EQ(turnbattle::kActivityResultRetryMaxAttempts, 30u);
}

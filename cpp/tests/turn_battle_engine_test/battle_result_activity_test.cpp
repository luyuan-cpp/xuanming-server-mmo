#include <gtest/gtest.h>
#include <google/protobuf/unknown_field_set.h>

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <limits>
#include <map>
#include <optional>
#include <set>
#include <string>
#include <vector>

#include "services/battle/settlement/settlement_outbox.h"
#include "services/battle/system/battle_result_activity.h"
#include "services/battle/system/result_commit.h"

// 活动局结果纯函数(battle_result_activity.h)的单元测试,docs/design/guild-phase2/06-activities.md §6.42。
// 覆盖:上下文回显 / 不回显、逃跑与阵亡名单升序去重、重发判定顺序、跨语言持久记录键契约。
// Redis / Kafka 编排在 battle_room_manager.cpp,不在本工程里测(与 settlement_outbox.h 同一拆法);
// 其中落库轮次的回复汇总抽成了纯接缝 turnbattle::StagingRoundTally(result_commit.h),在这里测。

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

namespace
{
    // 落库前置条件要覆盖的成员(与 MakeSettlementSourceResult 的两名成员一致)。
    std::set<uint64_t> StagingMembers() { return {92, 93}; }

    // 结果发送的前置条件是成员的个人结算已全部写进 pending(审计 social-match-battle#2)。
    // 只测发送 / 回执状态机的用例先把这一步走完。
    void MarkPersonalSettlementsDurable(turnbattle::ResultCommit& commit)
    {
        const auto instant = turnbattle::ResultCommit::Clock::time_point{};
        const auto attempt = commit.BeginStaging(instant);
        ASSERT_NE(attempt, 0u);
        ASSERT_TRUE(commit.FinishStaging(attempt, commit.UnstagedMembers(), instant));
    }
}

TEST(BattleResultCommit, UnknownCannotReleaseTerminalOrChangeOriginalBytes)
{
    turnbattle::ResultCommit commit("original-result-with-first-finished-at", StagingMembers());
    MarkPersonalSettlementsDurable(commit);
    const auto start = turnbattle::ResultCommit::Clock::time_point{};
    ASSERT_TRUE(commit.Begin(7, start));
    EXPECT_FALSE(commit.Confirmed());
    EXPECT_FALSE(commit.Begin(8, start));
    EXPECT_FALSE(commit.Deliver(99, true, start));
    EXPECT_FALSE(commit.Deliver(7, false, start));
    EXPECT_FALSE(commit.CanSend(start));
    EXPECT_TRUE(commit.CanSend(start + std::chrono::seconds(1)));
    ASSERT_TRUE(commit.Begin(8, start + std::chrono::seconds(1)));
    EXPECT_FALSE(commit.Deliver(7, true, start + std::chrono::seconds(1)));
    EXPECT_TRUE(commit.Deliver(8, true, start + std::chrono::seconds(1)));
    EXPECT_TRUE(commit.Confirmed());
    EXPECT_FALSE(commit.Deliver(8, true, start + std::chrono::seconds(2)));
    EXPECT_FALSE(commit.Begin(9, start + std::chrono::seconds(90)));
    EXPECT_EQ(commit.Payload(), "original-result-with-first-finished-at");
}

TEST(BattleResultCommit, FailedAdmissionUsesMonotonicBoundedBackoff)
{
    turnbattle::ResultCommit commit("same-bytes", StagingMembers());
    MarkPersonalSettlementsDurable(commit);
    auto current = turnbattle::ResultCommit::Clock::time_point{};
    const uint32_t delays[]{1,2,4,8,16,30,30};
    uint64_t correlation = 0;
    for (const auto seconds : delays)
    {
        ASSERT_TRUE(commit.Begin(++correlation, current));
        EXPECT_FALSE(commit.Deliver(correlation, false, current));
        EXPECT_FALSE(commit.CanSend(current + std::chrono::seconds(seconds) - std::chrono::milliseconds(1)));
        current += std::chrono::seconds(seconds);
        EXPECT_TRUE(commit.CanSend(current));
        EXPECT_FALSE(commit.Confirmed());
    }
}

TEST(BattleResultCommit, LiveRoomKeepsCreationReceiptBeyondDeadlineWhileResultIsUnknown)
{
    EXPECT_FALSE(turnbattle::CreationReceiptExpired(360000, 900000, true));
    EXPECT_TRUE(turnbattle::CreationReceiptExpired(360000, 900000, false));
    EXPECT_FALSE(turnbattle::CreationReceiptExpired(360000, 360000, false));
}

namespace
{
    contracts::kafka::BattleResultEvent MakeSettlementSourceResult()
    {
        contracts::kafka::BattleResultEvent event;
        event.set_battle_id(851);
        event.set_outcome(::BATTLE_OUTCOME_DRAW);
        event.set_total_rounds(7);
        event.set_finished_at_ms(123456);
        auto* team = event.add_teams();
        team->set_team_index(0);
        team->add_player_ids(92);
        team->add_player_ids(93);
        return event;
    }

    std::vector<::BattleSettlementData> MakeSettlementSources()
    {
        ::BattleSettlementData first;
        first.set_battle_id(851);
        first.set_player_id(92);
        first.set_health(27);
        first.set_mana(19);
        first.set_exp_gain(123);
        first.set_gold_gain(456);
        first.set_total_rounds(7);
        first.set_is_dead(true);
        first.set_fled(true);
        auto* consumed = first.add_items_consumed();
        consumed->set_item_table_id(1001);
        consumed->set_count(3);
        auto* gained = first.add_items_gained();
        gained->set_item_table_id(1002);
        gained->set_count(4);
        auto* pet = first.add_pets();
        pet->set_pet_id(923);
        pet->set_health(0);
        pet->set_mana(9);
        pet->set_is_dead(true);
        auto* defeated = first.add_defeated_monsters();
        defeated->set_monster_config_id(101);
        defeated->set_count(2);
        ::BattleSettlementData second;
        second.set_player_id(93);
        second.set_total_rounds(7);
        return {second, first};
    }
}

TEST(BattleResultCommit, FullSourcePreservesAllAssetsAndFixedBytesAfterUnknownAck)
{
    auto event = MakeSettlementSourceResult();
    auto source = MakeSettlementSources();
    auto expected = source.at(1);
    expected.set_outcome(::BATTLE_OUTCOME_DRAW);
    ASSERT_TRUE(turnbattle::FreezeBattleSettlementSource(event, source));
    ASSERT_EQ(event.settlements_size(), 2);
    EXPECT_EQ(event.settlement_format_version(), 1u);
    EXPECT_EQ(event.settlements(0).SerializeAsString(), expected.SerializeAsString());
    EXPECT_EQ(event.settlements(1).player_id(), 93u);
    EXPECT_EQ(event.settlements(1).battle_id(), 851u);
    EXPECT_EQ(event.settlements(1).outcome(), ::BATTLE_OUTCOME_DRAW);
    turnbattle::ResultCommit commit(event.SerializeAsString(), StagingMembers());
    MarkPersonalSettlementsDurable(commit);
    source.at(1).set_gold_gain(99999);
    event.mutable_settlements(0)->clear_items_gained();
    const auto instant = turnbattle::ResultCommit::Clock::time_point{};
    ASSERT_TRUE(commit.Begin(1, instant));
    EXPECT_FALSE(commit.Deliver(1, false, instant));
    ASSERT_TRUE(commit.Begin(2, instant + std::chrono::seconds(1)));
    ASSERT_TRUE(commit.Deliver(2, true, instant + std::chrono::seconds(1)));
    contracts::kafka::BattleResultEvent restored;
    ASSERT_TRUE(restored.ParseFromString(commit.Payload()));
    EXPECT_EQ(restored.settlements(0).SerializeAsString(), expected.SerializeAsString());
    EXPECT_EQ(restored.finished_at_ms(), 123456u);
}

TEST(BattleResultCommit, InvalidSourceFailsBeforeChangingTheResult)
{
    const auto original = MakeSettlementSourceResult();
    const auto sources = MakeSettlementSources();
    auto event = original;
    auto invalid = sources;
    invalid.pop_back();
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(event, invalid));
    EXPECT_EQ(event.SerializeAsString(), original.SerializeAsString());
    invalid = sources;
    invalid.at(0).set_player_id(92);
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(event, invalid));
    invalid = sources;
    invalid.at(0).set_player_id(0);
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(event, invalid));
    invalid = sources;
    invalid.at(0).set_player_team_index(1);
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(event, invalid));
    invalid = sources;
    invalid.at(0).set_battle_id(852);
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(event, invalid));
    invalid = sources;
    invalid.at(0).set_total_rounds(8);
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(event, invalid));
    EXPECT_EQ(event.SerializeAsString(), original.SerializeAsString());
    auto duplicateTeam = original;
    duplicateTeam.add_teams()->set_team_index(0);
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(duplicateTeam, sources));
    auto emptyTeam = original;
    emptyTeam.add_teams()->set_team_index(1);
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(emptyTeam, sources));
    ASSERT_TRUE(turnbattle::FreezeBattleSettlementSource(event, sources));
    const auto once = event.SerializeAsString();
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(event, sources));
    EXPECT_EQ(event.SerializeAsString(), once);
}

TEST(BattleResultCommit, OversizedCompleteSourceNeverPublishesATruncatedRecord)
{
    auto event = MakeSettlementSourceResult();
    event.GetReflection()->MutableUnknownFields(&event)->AddLengthDelimited(99,
        std::string(turnbattle::kMaxCompleteResultBytes, 'x'));
    const auto before = event.SerializeAsString();
    EXPECT_FALSE(turnbattle::FreezeBattleSettlementSource(event, MakeSettlementSources()));
    EXPECT_EQ(event.SerializeAsString(), before);
    EXPECT_EQ(event.settlement_format_version(), 0u);
}

// 审计 social-match-battle#2(data-overwrite-rollback-audit-20261018.md §2.4)。修复前,个人结算的 pending
// 只在 Kafka all ACK 之后才由 FinishBattle 写入。battle 进程在回执处理前停机(AbortAllRooms 先停提交再清房,
// 框架随后 flush producer)或崩溃时,结果已经到了 broker,评分与帮会历练照常入账,个人结算却永不派发。
// 修复后,结果第一次发往 broker 的前置条件是全部成员的个人结算已写进 pending:落库没完成之前不能发;
// 失败的一轮按有界退避重来;过期轮次的迟到回复不能放行。
TEST(BattleResultCommit, ResultIsNeverSentBeforePersonalSettlementsAreDurable)
{
    turnbattle::ResultCommit commit("result-with-personal-settlements", StagingMembers());
    const auto start = turnbattle::ResultCommit::Clock::time_point{};
    EXPECT_FALSE(commit.PersonalSettlementsDurable());
    EXPECT_FALSE(commit.CanSend(start));
    EXPECT_FALSE(commit.Begin(1, start));  // 修复前这里就能发出

    const auto first = commit.BeginStaging(start);
    ASSERT_NE(first, 0u);
    EXPECT_EQ(commit.BeginStaging(start), 0u);  // 同一时刻最多一轮在途
    EXPECT_FALSE(commit.FinishStaging(first, {}, start));  // 有成员没写进去
    EXPECT_FALSE(commit.CanSend(start + std::chrono::hours(1)));
    EXPECT_EQ(commit.BeginStaging(start + std::chrono::seconds(1) - std::chrono::milliseconds(1)), 0u);

    const auto second = commit.BeginStaging(start + std::chrono::seconds(1));
    ASSERT_NE(second, 0u);
    EXPECT_NE(second, first);
    EXPECT_FALSE(commit.FinishStaging(first, StagingMembers(), start + std::chrono::seconds(1)));  // 过期轮次
    EXPECT_FALSE(commit.PersonalSettlementsDurable());
    EXPECT_FALSE(commit.Begin(1, start + std::chrono::seconds(1)));

    EXPECT_TRUE(commit.FinishStaging(second, StagingMembers(), start + std::chrono::seconds(1)));
    EXPECT_TRUE(commit.PersonalSettlementsDurable());
    EXPECT_EQ(commit.BeginStaging(start + std::chrono::seconds(2)), 0u);  // 已落库不再重写
    EXPECT_FALSE(commit.FinishStaging(second, StagingMembers(), start + std::chrono::seconds(2)));
    EXPECT_TRUE(commit.Begin(1, start + std::chrono::seconds(1)));
    EXPECT_EQ(commit.Payload(), "result-with-personal-settlements");
}

// 落库失败的退避与投递同一口径:1、2、4…30 秒封顶,单调不减。
TEST(BattleResultCommit, FailedStagingUsesMonotonicBoundedBackoff)
{
    turnbattle::ResultCommit commit("same-bytes", StagingMembers());
    auto current = turnbattle::ResultCommit::Clock::time_point{};
    const uint32_t delays[]{1, 2, 4, 8, 16, 30, 30};
    // 这里只看快速阶段退避；低频阶段与总跨度分别由后续用例验证。
    ASSERT_LT(std::size(delays), static_cast<std::size_t>(turnbattle::kFastStagingRounds));
    for (const auto seconds : delays)
    {
        const auto attempt = commit.BeginStaging(current);
        ASSERT_NE(attempt, 0u);
        EXPECT_FALSE(commit.FinishStaging(attempt, {}, current));
        EXPECT_EQ(commit.BeginStaging(current + std::chrono::seconds(seconds) - std::chrono::milliseconds(1)), 0u);
        current += std::chrono::seconds(seconds);
        EXPECT_FALSE(commit.CanSend(current));
    }
}

// 审计 r2-battle#1(data-overwrite-rollback-audit-r2-20261019.md §3.3)。修复前,一轮里只要有一名成员没拿到确认,
// 下一轮就对全部成员再写一遍 pending:第 1 轮已写进去、随后被 scene 应用并销账(删 pending 与锁、摘账本)的成员,
// pending 复活,下次登录重复发奖。修复后按成员记结论,后续轮次只写没落定的成员。
TEST(BattleResultCommit, StagingRetryRewritesOnlyMembersWithoutConfirmation)
{
    turnbattle::ResultCommit commit("result", StagingMembers());
    const auto start = turnbattle::ResultCommit::Clock::time_point{};
    const auto first = commit.BeginStaging(start);
    ASSERT_NE(first, 0u);
    EXPECT_EQ(commit.UnstagedMembers(), (std::set<uint64_t>{92, 93}));
    // A(92)写进去;B(93)回 ERROR / 空应答 / 命令没发出
    EXPECT_FALSE(commit.FinishStaging(first, {92}, start));
    EXPECT_EQ(commit.UnstagedMembers(), (std::set<uint64_t>{93}));  // 修复前第 2 轮会把 92 一起重写
    EXPECT_FALSE(commit.CanSend(start + std::chrono::hours(1)));

    const auto second = commit.BeginStaging(start + std::chrono::seconds(1));
    ASSERT_NE(second, 0u);
    EXPECT_EQ(commit.UnstagedMembers(), (std::set<uint64_t>{93}));
    // 已落定成员重复上报、陌生 id 都不改变结论
    EXPECT_FALSE(commit.FinishStaging(second, {92, 777}, start + std::chrono::seconds(1)));
    EXPECT_EQ(commit.UnstagedMembers(), (std::set<uint64_t>{93}));
    EXPECT_FALSE(commit.PersonalSettlementsDurable());

    const auto third = commit.BeginStaging(start + std::chrono::seconds(3));  // 第 2 次失败后退避 2s
    ASSERT_NE(third, 0u);
    EXPECT_TRUE(commit.FinishStaging(third, {93}, start + std::chrono::seconds(3)));
    EXPECT_TRUE(commit.PersonalSettlementsDurable());
    EXPECT_TRUE(commit.UnstagedMembers().empty());
    EXPECT_TRUE(commit.CanSend(start + std::chrono::seconds(3)));
}

// 快速阶段失败不会直接丢弃落库；第24轮之后转为5分钟一次，仍只重写未确认成员。
TEST(BattleResultCommit, StagingContinuesWithSlowRetriesAfterFastRounds)
{
    turnbattle::ResultCommit commit("result", StagingMembers());
    auto current = turnbattle::ResultCommit::Clock::time_point{};
    EXPECT_EQ(turnbattle::kFastStagingRounds, 24u);
    for (uint32_t round = 1; round <= turnbattle::kFastStagingRounds; ++round)
    {
        EXPECT_FALSE(commit.ExhaustStagingIfOverdue(current));
        const auto attempt = commit.BeginStaging(current);
        ASSERT_NE(attempt, 0u) << "round " << round;
        EXPECT_FALSE(commit.FinishStaging(attempt, round == 1 ? std::set<uint64_t>{92} : std::set<uint64_t>{}, current));
        if (round < turnbattle::kFastStagingRounds) current += std::chrono::seconds(30);
    }
    EXPECT_EQ(commit.StagingFailures(), turnbattle::kFastStagingRounds);
    EXPECT_FALSE(commit.StagingExhausted());
    EXPECT_EQ(commit.UnstagedMembers(), (std::set<uint64_t>{93}));
    EXPECT_EQ(commit.BeginStaging(current + turnbattle::kSlowStagingRetryInterval - std::chrono::milliseconds(1)), 0u);
    current += turnbattle::kSlowStagingRetryInterval;
    const auto slowAttempt = commit.BeginStaging(current);
    ASSERT_NE(slowAttempt, 0u);
    EXPECT_FALSE(commit.FinishStaging(slowAttempt, {}, current));
    EXPECT_EQ(commit.StagingFailures(), turnbattle::kFastStagingRounds + 1);
    EXPECT_FALSE(commit.StagingExhausted());
    EXPECT_EQ(commit.UnstagedMembers(), (std::set<uint64_t>{93}));
    EXPECT_FALSE(commit.PersonalSettlementsDurable());
    EXPECT_FALSE(commit.CanSend(current));
}

// 审计r2-battle#4的现行契约：6小时总跨度到期才停止，终局仍未知，已确认成员不得复活。
TEST(BattleResultCommit, StagingStopsAtTotalSpanWithoutReleasingResult)
{
    turnbattle::ResultCommit commit("result", StagingMembers());
    const auto start = turnbattle::ResultCommit::Clock::time_point{};
    const auto attempt = commit.BeginStaging(start);
    ASSERT_NE(attempt, 0u);
    EXPECT_FALSE(commit.FinishStaging(attempt, {92}, start));
    EXPECT_FALSE(commit.ExhaustStagingIfOverdue(start + turnbattle::kMaxStagingSpan - std::chrono::milliseconds(1)));
    EXPECT_EQ(commit.BeginStaging(start + turnbattle::kMaxStagingSpan), 0u);
    EXPECT_FALSE(commit.StagingExhausted());  // 开轮拒绝不代替一次性耗尽通知。
    EXPECT_TRUE(commit.ExhaustStagingIfOverdue(start + turnbattle::kMaxStagingSpan));
    EXPECT_TRUE(commit.StagingExhausted());
    EXPECT_FALSE(commit.ExhaustStagingIfOverdue(start + turnbattle::kMaxStagingSpan + std::chrono::milliseconds(1)));
    EXPECT_EQ(commit.StagingFailures(), 1u);
    EXPECT_EQ(commit.UnstagedMembers(), (std::set<uint64_t>{93}));
    const auto later = start + std::chrono::hours(24);
    EXPECT_EQ(commit.BeginStaging(later), 0u);
    EXPECT_FALSE(commit.PersonalSettlementsDurable());
    EXPECT_FALSE(commit.CanSend(later));
    EXPECT_FALSE(commit.Begin(1, later));
}

// Redis一直不可用时调用方只问跨度、不发落库轮；单调截止仍必须生效，无需真实等待。
TEST(BattleResultCommit, StagingSpanExpiresWithoutOpeningRedisRounds)
{
    turnbattle::ResultCommit commit("result", StagingMembers());
    const auto start = turnbattle::ResultCommit::Clock::time_point{};
    EXPECT_FALSE(commit.ExhaustStagingIfOverdue(start));
    EXPECT_FALSE(commit.ExhaustStagingIfOverdue(start + turnbattle::kMaxStagingSpan - std::chrono::milliseconds(1)));
    EXPECT_TRUE(commit.ExhaustStagingIfOverdue(start + turnbattle::kMaxStagingSpan));
    EXPECT_EQ(commit.StagingFailures(), 0u);
    EXPECT_EQ(commit.UnstagedMembers(), StagingMembers());
    EXPECT_EQ(commit.BeginStaging(start + turnbattle::kMaxStagingSpan), 0u);
    EXPECT_FALSE(commit.PersonalSettlementsDurable());
    EXPECT_FALSE(commit.CanSend(start + turnbattle::kMaxStagingSpan));
    EXPECT_FALSE(commit.Begin(1, start + turnbattle::kMaxStagingSpan));
}

// 审计 r2-battle#4 的有界性(2026-10-22)。按生产的驱动顺序(StagePersonalSettlements:每拍先判跨度、再开轮)
// 一直失败下去:6 小时里恰好开出快速 24 轮 + 低频 70 轮,第 6 小时整那一拍判用尽且只判一次,之后再不开轮。
// 推导:快速阶段 23 次等待 = 1+2+4+8+16 + 18×30 = 571s;低频第 k 轮在 571s + 300s×k 开出,
// 要早于 21600s 得 k ≤ 70。退避都是整秒,驱动按 1s 一拍走,不读墙钟。
TEST(BattleResultCommit, StagingRoundsStayBoundedAcrossTheWholeSpan)
{
    turnbattle::ResultCommit commit("result", StagingMembers());
    const auto start = turnbattle::ResultCommit::Clock::time_point{};
    const std::chrono::seconds span(turnbattle::kMaxStagingSpan);
    uint32_t rounds = 0;
    std::optional<std::chrono::seconds> exhaustedAt;
    for (std::chrono::seconds elapsed{0}; elapsed <= span + std::chrono::hours(1); elapsed += std::chrono::seconds(1))
    {
        const auto now = start + elapsed;
        if (commit.ExhaustStagingIfOverdue(now))
        {
            ASSERT_FALSE(exhaustedAt.has_value()) << "exhaustion reported twice";
            exhaustedAt = elapsed;
            continue;
        }
        const auto attempt = commit.BeginStaging(now);
        if (attempt == 0) continue;
        ASSERT_FALSE(exhaustedAt.has_value()) << "round opened after exhaustion at " << elapsed.count() << "s";
        ++rounds;
        if (rounds == turnbattle::kFastStagingRounds) EXPECT_EQ(elapsed.count(), 571);
        EXPECT_FALSE(commit.FinishStaging(attempt, {}, now));
    }
    ASSERT_TRUE(exhaustedAt.has_value());
    EXPECT_EQ(exhaustedAt->count(), span.count());
    EXPECT_EQ(rounds, turnbattle::kFastStagingRounds + 70u);
    EXPECT_EQ(commit.StagingFailures(), rounds);
    EXPECT_TRUE(commit.StagingExhausted());
    EXPECT_EQ(commit.UnstagedMembers(), StagingMembers());
    EXPECT_FALSE(commit.CanSend(start + span + std::chrono::hours(1)));
}

// 有轮次在途时不判用尽:它的回复到齐后可能恰好落定。到点时落定 → 放行、不再判用尽;没落定 → 下一次调用判用尽。
TEST(BattleResultCommit, StagingSpanWaitsForTheInFlightRound)
{
    const auto start = turnbattle::ResultCommit::Clock::time_point{};
    const auto deadline = start + turnbattle::kMaxStagingSpan;
    {
        turnbattle::ResultCommit commit("result", StagingMembers());
        EXPECT_FALSE(commit.ExhaustStagingIfOverdue(start));
        const auto attempt = commit.BeginStaging(deadline - std::chrono::milliseconds(1));
        ASSERT_NE(attempt, 0u);
        EXPECT_FALSE(commit.ExhaustStagingIfOverdue(deadline));  // 在途
        EXPECT_TRUE(commit.FinishStaging(attempt, StagingMembers(), deadline));
        EXPECT_FALSE(commit.ExhaustStagingIfOverdue(deadline + std::chrono::seconds(1)));
        EXPECT_FALSE(commit.StagingExhausted());
        EXPECT_TRUE(commit.CanSend(deadline));
    }
    {
        turnbattle::ResultCommit commit("result", StagingMembers());
        EXPECT_FALSE(commit.ExhaustStagingIfOverdue(start));
        const auto attempt = commit.BeginStaging(deadline - std::chrono::milliseconds(1));
        ASSERT_NE(attempt, 0u);
        EXPECT_FALSE(commit.ExhaustStagingIfOverdue(deadline));
        EXPECT_FALSE(commit.FinishStaging(attempt, {92}, deadline));
        EXPECT_TRUE(commit.ExhaustStagingIfOverdue(deadline));
        EXPECT_EQ(commit.UnstagedMembers(), (std::set<uint64_t>{93}));
        EXPECT_FALSE(commit.CanSend(deadline + std::chrono::hours(1)));
    }
}

// 落库标记(settlement_outbox.h 的 kStagedSettlementMembersKeyFmt)要活过整个落库阶段,一轮开出时它才一定还在;
// 留 4 倍余量(推导见 result_commit.h 的 kMaxStagingSpan)。与 battle_room_manager.cpp 的 static_assert 同一关系,
// 放进测试工程是为了改任一常量时这里也先红。
TEST(BattleResultCommit, StagingSpanLeavesStagedMarkerTtlMargin)
{
    const long long markerTtlSec = battle_settlement::kStagedSettlementMembersTtlSec;
    const long long spanSec = std::chrono::seconds(turnbattle::kMaxStagingSpan).count();
    EXPECT_LE(spanSec * 4, markerTtlSec);
    // 快速阶段整段(第 24 轮约在 571s 开出)与至少一轮低频重试都落在跨度之内。
    EXPECT_LT(571 + std::chrono::seconds(turnbattle::kSlowStagingRetryInterval).count(), spanSec);
}

// 2026-10-22 评审补(r2-battle#4 should_fix):滞留房间周期提醒按 ClassifyStuckTerminal 数房间表。
// 原先只数落库用尽的旧协议房间;v1 SQL 终局未确认与占位房间同样排不空,却不在提醒里,值班第 2 步
// 「等 node_rooms 降到滞留数」永远等不到。改回只看 StagingExhausted 时,下面 sql / placeholder 两条断言转红。
TEST(BattleStuckTerminal, ClassifiesEveryRoomThatCannotDrain)
{
    using turnbattle::ClassifyStuckTerminal;
    using turnbattle::StuckTerminal;
    const auto start = turnbattle::ResultCommit::Clock::time_point{};

    // 没有终局提交在途(普通在打的房间 / 本地模式):不算滞留。
    EXPECT_EQ(ClassifyStuckTerminal(nullptr, false), StuckTerminal::kNone);

    // v1:SQL 终局重试到上限或原件无效。v1 房间不建 ResultCommit。
    EXPECT_EQ(ClassifyStuckTerminal(nullptr, true), StuckTerminal::kSqlOutcomeUnconfirmed);

    // 占位:BeginResultCommit 序列化失败 / 成员对不上时的空载荷、无待写成员,永不放行。
    const turnbattle::ResultCommit placeholder(std::string{});
    EXPECT_FALSE(placeholder.PersonalSettlementsDurable());
    EXPECT_FALSE(placeholder.StagingExhausted());
    EXPECT_EQ(ClassifyStuckTerminal(&placeholder, false), StuckTerminal::kPlaceholder);

    // 正常落库中、已落定待发:都还会自己走完,不算滞留。
    turnbattle::ResultCommit staging("result", StagingMembers());
    EXPECT_EQ(ClassifyStuckTerminal(&staging, false), StuckTerminal::kNone);
    {
        turnbattle::ResultCommit staged("result", StagingMembers());
        const auto attempt = staged.BeginStaging(start);
        ASSERT_NE(attempt, 0u);
        ASSERT_TRUE(staged.FinishStaging(attempt, StagingMembers(), start));
        EXPECT_EQ(ClassifyStuckTerminal(&staged, false), StuckTerminal::kNone);
    }

    // 落库到总跨度上限。
    EXPECT_FALSE(staging.ExhaustStagingIfOverdue(start));
    ASSERT_TRUE(staging.ExhaustStagingIfOverdue(start + turnbattle::kMaxStagingSpan));
    EXPECT_EQ(ClassifyStuckTerminal(&staging, false), StuckTerminal::kStagingExhausted);

    // 日志字段名(值班按名字区分三类)。
    EXPECT_STREQ(turnbattle::StuckTerminalName(StuckTerminal::kStagingExhausted), "staging_exhausted");
    EXPECT_STREQ(turnbattle::StuckTerminalName(StuckTerminal::kSqlOutcomeUnconfirmed), "sql_unconfirmed");
    EXPECT_STREQ(turnbattle::StuckTerminalName(StuckTerminal::kPlaceholder), "placeholder");
}

// 2026-10-23 修复(审计 r2-battle#2):停机时只有「v1 终局已冻结、还没拿到 SQL 决定、也没判用尽」的房间要在排空窗口内
// 再提交一轮,AbortAllRooms(立即续发)、SqlOutcomesFlushed(排空谓词)、StopSqlOutcomes(未落定留痕)三处都按它判。
// 修复前停机直接装停止闸、清空待提交集合,这类房间再没有提交机会,只能等 match 截止后判 ABORT。
// 还在打的 v1 房间没有终局可交、旧协议房间不走 SQL 终局:二者都不能让停机多等。
TEST(BattleShutdownOutcomeFlush, OnlyFrozenUnconfirmedV1OutcomesAreFlushed)
{
    using turnbattle::NeedsShutdownOutcomeFlush;
    // 参数顺序:outcomeProtocol, outcomePending, outcomeComplete, outcomeExhausted
    EXPECT_TRUE(NeedsShutdownOutcomeFlush(1, true, false, false));   // 已算出终局、等 SQL 决定:补提交

    EXPECT_FALSE(NeedsShutdownOutcomeFlush(1, false, false, false)); // v1 还在打:作废,match 截止后判 ABORT
    EXPECT_FALSE(NeedsShutdownOutcomeFlush(1, true, true, false));   // 已拿到 COMPLETE:回复处理已收尾
    EXPECT_FALSE(NeedsShutdownOutcomeFlush(1, true, false, true));   // 已判用尽(跨度上限 / 原件无效):交 match 恢复裁决
    EXPECT_FALSE(NeedsShutdownOutcomeFlush(0, true, false, false));  // 旧协议:不走 SQL 终局
    EXPECT_FALSE(NeedsShutdownOutcomeFlush(2, true, false, false));  // 未知协议版本(建房时已拒):fail-closed 不提交
}

// 一轮落库的回复汇总(battle_room_manager.cpp StagePersonalSettlements 的编排接缝)。
// 每名成员只记第一次回复;不在本轮成员里的 id 忽略;全部成员回复过的那一次恰好返回 true。
TEST(BattleStagingRoundTally, RecordsFirstReplyPerMemberAndCompletesOnce)
{
    turnbattle::StagingRoundTally tally({92, 93, 94});
    EXPECT_FALSE(tally.Record(92, true));
    EXPECT_FALSE(tally.Record(92, false));  // 重复回复不改第一次的结论
    EXPECT_FALSE(tally.Record(777, true));  // 陌生 id
    EXPECT_FALSE(tally.Record(93, false));
    EXPECT_TRUE(tally.Record(94, true));    // 最后一名到齐,收口
    EXPECT_FALSE(tally.Record(94, true));   // 到齐之后的迟到回复不再触发收口
    EXPECT_FALSE(tally.Record(93, true));
    EXPECT_EQ(tally.Settled(), (std::set<uint64_t>{92, 94}));
    EXPECT_EQ(tally.Members(), (std::set<uint64_t>{92, 93, 94}));
}

// 按生产编排把 StagingRoundTally 与 ResultCommit 串起来(StagePersonalSettlements → onReply → OnPersonalSettlementsStaged)。
// 审计 r2-battle#1:每轮只对开轮时的 UnstagedMembers 发命令,修复前第 2 轮会把已落定的 92 一起重写。
// 本轮最后一名成员同步失败(命令没发出)时当场收口:遍历的是 tally 自己那份成员,不受 FinishStaging 改写待写集合影响。
// 全部落定的那一轮才放行(调用方随即 DEL 本局落库标记),此后不再开轮。
TEST(BattleStagingRoundTally, DrivesRetryRoundsOverOnlyUnsettledMembers)
{
    turnbattle::ResultCommit commit("result", {92, 93, 94});
    auto now = turnbattle::ResultCommit::Clock::time_point{};
    // 第 1 轮:92 写入,93 回 ERROR,94 命令没发出(同步失败,就地记未落定)。
    // 第 2 轮:93 回 2(上一轮其实执行过),94 写入。
    const std::vector<std::map<uint64_t, bool>> replies{{{92, true}, {93, false}, {94, false}},
                                                        {{93, true}, {94, true}}};
    std::vector<std::set<uint64_t>> commandedPerRound;
    std::vector<bool> releasedPerRound;
    for (const auto& roundReplies : replies)
    {
        const auto attempt = commit.BeginStaging(now);
        ASSERT_NE(attempt, 0u);
        turnbattle::StagingRoundTally tally(commit.UnstagedMembers());
        std::set<uint64_t> commanded;
        uint32_t finishCalls = 0;
        bool released = false;
        for (const auto member : tally.Members())
        {
            commanded.insert(member);
            if (!tally.Record(member, roundReplies.at(member))) continue;
            ++finishCalls;
            released = commit.FinishStaging(attempt, tally.Settled(), now);
        }
        EXPECT_EQ(finishCalls, 1u);
        commandedPerRound.push_back(std::move(commanded));
        releasedPerRound.push_back(released);
        now += std::chrono::seconds(1);  // 第 1 次失败后退避 1s
    }
    EXPECT_EQ(commandedPerRound[0], (std::set<uint64_t>{92, 93, 94}));
    EXPECT_EQ(commandedPerRound[1], (std::set<uint64_t>{93, 94}));
    EXPECT_FALSE(releasedPerRound[0]);
    EXPECT_TRUE(releasedPerRound[1]);
    EXPECT_TRUE(commit.PersonalSettlementsDurable());
    EXPECT_EQ(commit.BeginStaging(now), 0u);  // DEL 标记之后不会再有本局的落库命令
}

// 过期轮次与重复回调不改状态:不移出成员、不计失败、不放行。
TEST(BattleResultCommit, StaleStagingReplyChangesNothing)
{
    turnbattle::ResultCommit commit("result", StagingMembers());
    const auto start = turnbattle::ResultCommit::Clock::time_point{};
    const auto first = commit.BeginStaging(start);
    ASSERT_NE(first, 0u);
    EXPECT_FALSE(commit.FinishStaging(first + 1, StagingMembers(), start));  // 编号不符
    EXPECT_FALSE(commit.FinishStaging(0, StagingMembers(), start));
    EXPECT_EQ(commit.UnstagedMembers(), StagingMembers());
    EXPECT_EQ(commit.StagingFailures(), 0u);
    EXPECT_EQ(commit.BeginStaging(start), 0u);  // 本轮仍在途

    EXPECT_FALSE(commit.FinishStaging(first, {}, start));
    EXPECT_EQ(commit.StagingFailures(), 1u);
    // 第 1 轮已收口,它的回复再来一次也不改状态
    EXPECT_FALSE(commit.FinishStaging(first, StagingMembers(), start + std::chrono::seconds(1)));
    EXPECT_EQ(commit.UnstagedMembers(), StagingMembers());
    EXPECT_EQ(commit.StagingFailures(), 1u);
    EXPECT_FALSE(commit.PersonalSettlementsDurable());

    const auto second = commit.BeginStaging(start + std::chrono::seconds(1));
    ASSERT_NE(second, 0u);
    EXPECT_NE(second, first);
}

// 没有成员的提交(序列化失败、成员对不上时的「终局未知」占位)永远不放行。
TEST(BattleResultCommit, CommitWithoutStagingMembersNeverReleases)
{
    turnbattle::ResultCommit commit(std::string{});
    const auto start = turnbattle::ResultCommit::Clock::time_point{};
    EXPECT_EQ(commit.BeginStaging(start), 0u);
    EXPECT_FALSE(commit.PersonalSettlementsDurable());
    EXPECT_FALSE(commit.CanSend(start + std::chrono::hours(1)));
    EXPECT_FALSE(commit.Begin(1, start + std::chrono::hours(1)));
}

// 落库的字节与回执后派发的字节是同一份:都取自冻结后的结果原件,逐人一条 BattleSettlementEvent。
TEST(BattleResultCommit, PersonalSettlementPayloadsAreTheFrozenSourceBytes)
{
    auto event = MakeSettlementSourceResult();
    ASSERT_TRUE(turnbattle::FreezeBattleSettlementSource(event, MakeSettlementSources()));

    std::map<uint64_t, std::string> payloads;
    ASSERT_TRUE(turnbattle::BuildPersonalSettlementPayloads(event, payloads));
    ASSERT_EQ(payloads.size(), 2u);
    for (const auto& settlement : event.settlements())
    {
        const auto it = payloads.find(settlement.player_id());
        ASSERT_NE(it, payloads.end());
        ::BattleSettlementEvent parsed;
        ASSERT_TRUE(parsed.ParseFromString(it->second));
        EXPECT_EQ(parsed.settlement().SerializeAsString(), settlement.SerializeAsString());
        EXPECT_EQ(parsed.settlement().battle_id(), 851u);
        EXPECT_EQ(parsed.settlement().outcome(), ::BATTLE_OUTCOME_DRAW);
    }
    // 同一份原件反复计算,字节不变(重试落库与回执后派发不会出现两个版本)。
    std::map<uint64_t, std::string> again;
    ASSERT_TRUE(turnbattle::BuildPersonalSettlementPayloads(event, again));
    EXPECT_EQ(again, payloads);
}

// 原件不是 v1 完整来源、成员重复、battle_id 或 outcome 与原件不符时拒绝,且不改出参。
TEST(BattleResultCommit, PersonalSettlementPayloadsRejectIncompleteSource)
{
    const std::map<uint64_t, std::string> sentinel{{1, "keep"}};
    auto out = sentinel;
    EXPECT_FALSE(turnbattle::BuildPersonalSettlementPayloads(MakeSettlementSourceResult(), out));
    EXPECT_EQ(out, sentinel);

    auto frozen = MakeSettlementSourceResult();
    ASSERT_TRUE(turnbattle::FreezeBattleSettlementSource(frozen, MakeSettlementSources()));

    auto duplicate = frozen;
    *duplicate.add_settlements() = frozen.settlements(0);
    EXPECT_FALSE(turnbattle::BuildPersonalSettlementPayloads(duplicate, out));
    auto wrongBattle = frozen;
    wrongBattle.mutable_settlements(1)->set_battle_id(852);
    EXPECT_FALSE(turnbattle::BuildPersonalSettlementPayloads(wrongBattle, out));
    auto wrongOutcome = frozen;
    wrongOutcome.mutable_settlements(0)->set_outcome(::BATTLE_OUTCOME_SIDE_A_WIN);
    EXPECT_FALSE(turnbattle::BuildPersonalSettlementPayloads(wrongOutcome, out));
    auto missingPlayer = frozen;
    missingPlayer.mutable_settlements(1)->set_player_id(0);
    EXPECT_FALSE(turnbattle::BuildPersonalSettlementPayloads(missingPlayer, out));
    EXPECT_EQ(out, sentinel);
}

#include <gtest/gtest.h>
#include <google/protobuf/unknown_field_set.h>

#include <chrono>
#include <cstdint>
#include <string>
#include <vector>

#include "services/battle/system/battle_result_activity.h"
#include "services/battle/system/result_commit.h"

// 活动局结果组装(battle_result_activity.h)与终局原件(result_commit.h)的纯函数单元测试,
// docs/design/guild-phase2/06-activities.md §6.42。覆盖:上下文回显 / 不回显、逃跑与阵亡名单升序去重、
// 完整来源冻结、停机补提交判据、SQL 终局重放节奏。提交 match SQL 的编排在 battle_room_manager.cpp,不在本工程里测。

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

// proto3 开放枚举:未知 kind 值按活动对局处理(宁可多回显一次上下文,不能把真实活动结果当普通对局)。
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

// 只有终局协议 v1;Go(gather.go)与 Java 建房时填同一个值。
static_assert(turnbattle::kOutcomeProtocolVersion == 1);

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

TEST(BattleResultCommit, FullSourcePreservesAllAssetsAndFixedBytes)
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
    // 原件一经序列化就固定:重放 CommitBattleOutcome 只发这份字节,之后改动手里的来源影响不到它。
    const auto payload = event.SerializeAsString();
    source.at(1).set_gold_gain(99999);
    event.mutable_settlements(0)->clear_items_gained();
    contracts::kafka::BattleResultEvent restored;
    ASSERT_TRUE(restored.ParseFromString(payload));
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

// 2026-10-23 修复(审计 r2-battle#2):停机时只有「终局已冻结、还没拿到 SQL 决定、也没判用尽」的房间要在排空窗口内
// 再提交一轮,AbortAllRooms(立即续发)、SqlOutcomesFlushed(排空谓词)、StopSqlOutcomes(未落定留痕)三处都按它判。
// 修复前停机直接装停止闸、清空待提交集合,这类房间再没有提交机会,只能等 match 截止后判 ABORT。
// 还在打的房间没有终局可交,不能让停机多等。
TEST(BattleShutdownOutcomeFlush, OnlyFrozenUnconfirmedOutcomesAreFlushed)
{
    using turnbattle::NeedsShutdownOutcomeFlush;
    // 参数顺序:outcomePending, outcomeComplete, outcomeExhausted
    EXPECT_TRUE(NeedsShutdownOutcomeFlush(true, false, false));   // 已算出终局、等 SQL 决定:补提交

    EXPECT_FALSE(NeedsShutdownOutcomeFlush(false, false, false)); // 还在打:作废,match 截止后判 ABORT
    EXPECT_FALSE(NeedsShutdownOutcomeFlush(true, true, false));   // 已拿到 COMPLETE:回复处理已收尾
    EXPECT_FALSE(NeedsShutdownOutcomeFlush(true, false, true));   // 已判用尽(跨度上限 / 原件无效):交 match 恢复裁决
}

// 2026-10-30:v1 房间的 SQL 终局重放到总跨度上限之后不再「判用尽就停」。修复前越界即移出待提交集合、此后不再重放,
// 而房间只在拿到 COMPLETE / ABORT 时才删,于是永远留在节点上、节点排不空。现在越界只置 outcomeExhausted(告警、计入滞留),
// 重放改为每分钟一次,直到 Match 给出决定(同一原件按持久决定幂等回放,到期 ABORT 由 Match 自己落);越界之后再过
// kSqlOutcomeSlowReplaySpan 仍无决定才本地删房(重试有上限)。
TEST(BattleSqlOutcomeReplay, SqlOutcomeKeepsSlowReplayAfterSpanExhaustion)
{
    using turnbattle::SqlOutcomeReplayAbandoned;
    using turnbattle::SqlOutcomeRetryInterval;
    using turnbattle::SqlOutcomeSpanExceeded;
    const std::chrono::steady_clock::duration span = turnbattle::kSqlOutcomeFastReplaySpan;
    const std::chrono::steady_clock::duration giveUp = span + turnbattle::kSqlOutcomeSlowReplaySpan;
    const auto oneMs = std::chrono::milliseconds(1);

    // 跨度之内:快速重放,每 5 秒一次。
    EXPECT_FALSE(SqlOutcomeSpanExceeded(std::chrono::steady_clock::duration::zero()));
    EXPECT_FALSE(SqlOutcomeSpanExceeded(span - oneMs));
    EXPECT_EQ(5, SqlOutcomeRetryInterval(false).count());

    // 越界:判 exhausted(告警、计入滞留),但不放弃 —— 继续慢速重放,每分钟一次。
    EXPECT_TRUE(SqlOutcomeSpanExceeded(span));
    EXPECT_FALSE(SqlOutcomeReplayAbandoned(span));
    EXPECT_FALSE(SqlOutcomeReplayAbandoned(giveUp - oneMs));
    EXPECT_EQ(60, SqlOutcomeRetryInterval(true).count());
    // 越界之后到放弃之前还能慢速重放上千轮(24h / 60s),足够等到 Match 的到期 ABORT 或 COMPLETE 回放。
    EXPECT_GE(turnbattle::kSqlOutcomeSlowReplaySpan / SqlOutcomeRetryInterval(true), 1000);

    // 越界之后再过 kSqlOutcomeSlowReplaySpan 仍无决定:本地删房,不再重放。
    EXPECT_TRUE(SqlOutcomeReplayAbandoned(giveUp));
    EXPECT_TRUE(SqlOutcomeSpanExceeded(giveUp));

    // 越界改变的只是重放节奏:停机补提交照旧只看 outcomeExhausted。
    EXPECT_FALSE(turnbattle::NeedsShutdownOutcomeFlush(true, false, true));
}

#include <gtest/gtest.h>

#include <optional>
#include <random>
#include <string>

#include "system/battle_replay_recorder.h"

// 战报回放录制器单测(docs/design/battle-report-replay.md §6.1、§17):
// 直接构造观众版帧喂录制器,冻结后解压解析,与输入逐字段核对;覆盖实时字段清理、截断、强制判平、超上限丢弃与 gzip 往返。

namespace {

using turnbattle::BattleReplayHeader;
using turnbattle::BattleReplayRecorder;

BattleReplayHeader Header()
{
    return BattleReplayHeader{9001, 3, 17, 1700000000000ull, "fingerprint-v1"};
}

::BattleStateS2C State(const uint32_t roundIndex)
{
    ::BattleStateS2C state;
    state.set_battle_id(9001);
    state.set_round_index(roundIndex);
    state.set_action_deadline_ms(1700000006000ull); // 实时字段:回放里必须清零
    state.add_pending_actor_ids(11);                 // 实时字段:回放里必须清空
    for (uint64_t id : {11ull, 22ull})
    {
        auto* actor = state.add_actors();
        actor->set_actor_id(id);
        actor->set_name(id == 11 ? "甲" : "乙");
        actor->set_team_index(id == 11 ? 0 : 1);
        actor->set_max_health(1000);
        actor->mutable_attributes()->set_health(1000 - roundIndex * 10);
    }
    return state;
}

::TurnResultS2C Round(const uint32_t roundIndex)
{
    ::TurnResultS2C turn;
    turn.set_battle_id(9001);
    turn.set_round_index(roundIndex);
    auto* event = turn.add_events();
    event->set_event_type(::BATTLE_EVENT_DAMAGE);
    event->set_source_id(11);
    event->set_target_id(22);
    event->set_value(10);
    event->set_target_health_after(1000 - roundIndex * 10);
    turn.add_action_order(11);
    turn.add_action_order(22);
    *turn.mutable_state() = State(roundIndex);
    return turn;
}

::BattleReplay Decode(const ::BattleReplayEnvelope& envelope)
{
    EXPECT_EQ(turnbattle::kBattleReplayFormatVersion, envelope.format_version());
    EXPECT_EQ(::BATTLE_REPLAY_CODEC_GZIP, envelope.codec());
    const auto raw = turnbattle::GzipDecompress(envelope.payload(), turnbattle::kMaxBattleReplayRawBytes);
    EXPECT_TRUE(raw.has_value());
    ::BattleReplay replay;
    if (raw)
    {
        EXPECT_EQ(envelope.raw_bytes(), raw->size());
        EXPECT_TRUE(replay.ParseFromString(*raw));
    }
    return replay;
}

// 指定字符集里随机取字符:压缩比有界(64 个字符约 6 bit/字符),用来造「压缩后仍超限」的输入。
std::string RandomText(const std::size_t size)
{
    static constexpr char kAlphabet[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    std::mt19937 generator(20261106u);
    std::uniform_int_distribution<int> pick(0, 63);
    std::string text(size, 'A');
    for (auto& c : text)
    {
        c = kAlphabet[pick(generator)];
    }
    return text;
}

} // namespace

TEST(BattleReplayRecorder, FreezeRoundTripsHeaderStartAndRounds)
{
    BattleReplayRecorder recorder(Header(), State(1));
    recorder.AddRound(Round(1));
    recorder.AddRound(Round(2));
    const auto envelope = recorder.Freeze(::BATTLE_OUTCOME_SIDE_A_WIN, 2, 1700000012000ull);
    ASSERT_TRUE(envelope.has_value());

    const auto replay = Decode(*envelope);
    EXPECT_EQ(9001u, replay.battle_id());
    EXPECT_EQ(3u, replay.match_mode());
    EXPECT_EQ(17u, replay.battle_config_id());
    EXPECT_EQ(1700000000000ull, replay.started_at_ms());
    EXPECT_EQ(1700000012000ull, replay.finished_at_ms());
    EXPECT_EQ("fingerprint-v1", replay.table_fingerprint());
    EXPECT_EQ(::BATTLE_OUTCOME_SIDE_A_WIN, replay.outcome());
    EXPECT_EQ(::BATTLE_REPLAY_END_FINISHED, replay.end_reason());
    EXPECT_EQ(2u, replay.total_rounds());
    EXPECT_FALSE(replay.truncated());
    EXPECT_FALSE(replay.has_final_state());

    EXPECT_EQ(2, replay.start_state().actors_size());
    EXPECT_EQ(0u, replay.start_state().action_deadline_ms());
    EXPECT_EQ(0, replay.start_state().pending_actor_ids_size());
    ASSERT_EQ(2, replay.rounds_size());
    for (int i = 0; i < replay.rounds_size(); ++i)
    {
        const auto& round = replay.rounds(i);
        EXPECT_EQ(static_cast<uint32_t>(i + 1), round.round_index());
        EXPECT_EQ(0u, round.state().action_deadline_ms()) << "实时字段已清";
        EXPECT_EQ(0, round.state().pending_actor_ids_size()) << "实时字段已清";
        ASSERT_EQ(1, round.events_size());
        EXPECT_EQ(Round(i + 1).events(0).SerializeAsString(), round.events(0).SerializeAsString()) << "事件逐字段相同";
        EXPECT_EQ(2, round.action_order_size());
    }
}

TEST(BattleReplayRecorder, DeadlineDrawKeepsFinalStateAndReason)
{
    BattleReplayRecorder recorder(Header(), State(1));
    recorder.AddRound(Round(1));
    recorder.MarkDeadlineDraw(State(2));
    const auto envelope = recorder.Freeze(::BATTLE_OUTCOME_DRAW, 1, 1700000300000ull);
    ASSERT_TRUE(envelope.has_value());
    const auto replay = Decode(*envelope);
    EXPECT_EQ(::BATTLE_REPLAY_END_DEADLINE_DRAW, replay.end_reason());
    EXPECT_EQ(::BATTLE_OUTCOME_DRAW, replay.outcome());
    ASSERT_TRUE(replay.has_final_state());
    EXPECT_EQ(2u, replay.final_state().round_index());
    EXPECT_EQ(0u, replay.final_state().action_deadline_ms());
    EXPECT_EQ(1, replay.rounds_size());
}

TEST(BattleReplayRecorder, StopsRecordingPastBudgetAndMarksTruncated)
{
    BattleReplayRecorder recorder(Header(), State(1));
    // 每回合约 64 KiB(可高度压缩的重复文本),预算约 1 MiB:十几回合后停录。
    auto big = Round(1);
    big.mutable_state()->mutable_actors(0)->set_name(std::string(64u << 10, 'x'));
    int added = 0;
    for (; added < 64 && !recorder.Truncated(); ++added)
    {
        recorder.AddRound(big);
    }
    ASSERT_TRUE(recorder.Truncated());
    EXPECT_LE(recorder.RecordedBytes(), turnbattle::kBattleReplayRecordBudgetBytes);
    const int recorded = recorder.RecordedRounds();
    EXPECT_LT(recorded, added) << "超预算的那一回合没录";
    recorder.AddRound(Round(2)); // 截断之后不再追加
    EXPECT_EQ(recorded, recorder.RecordedRounds());

    const auto envelope = recorder.Freeze(::BATTLE_OUTCOME_SIDE_B_WIN, static_cast<uint32_t>(added), 1700000100000ull);
    ASSERT_TRUE(envelope.has_value()) << "重复文本压缩后远小于 512 KiB";
    EXPECT_LE(envelope->raw_bytes(), turnbattle::kMaxBattleReplayRawBytes);
    const auto replay = Decode(*envelope);
    EXPECT_TRUE(replay.truncated());
    EXPECT_EQ(recorded, replay.rounds_size());
    EXPECT_EQ(static_cast<uint32_t>(added), replay.total_rounds()) << "total_rounds 是引擎结算过的回合数,不受截断影响";
}

TEST(BattleReplayRecorder, DropsWhenCompressedPayloadExceedsLimit)
{
    auto start = State(1);
    // 约 900 KiB 的随机文本:原始字节在上限内,压缩后仍超过 512 KiB。
    start.mutable_actors(0)->set_name(RandomText(900u << 10));
    BattleReplayRecorder recorder(Header(), start);
    EXPECT_FALSE(recorder.Freeze(::BATTLE_OUTCOME_SIDE_A_WIN, 0, 1).has_value());
}

TEST(BattleReplayRecorder, FreezesOnlyOnce)
{
    BattleReplayRecorder recorder(Header(), State(1));
    ASSERT_TRUE(recorder.Freeze(::BATTLE_OUTCOME_SIDE_A_WIN, 0, 1).has_value());
    EXPECT_EQ(0, recorder.RecordedRounds()) << "冻结后释放已录的帧";
    recorder.AddRound(Round(1));
    recorder.MarkDeadlineDraw(State(2));
    EXPECT_EQ(0, recorder.RecordedRounds());
    EXPECT_FALSE(recorder.Freeze(::BATTLE_OUTCOME_SIDE_A_WIN, 0, 1).has_value());
}

TEST(BattleReplayGzip, RoundTripsAndRejectsOversizedOrCorruptInput)
{
    const std::string raw = RandomText(200000) + std::string(100000, 'z');
    const auto compressed = turnbattle::GzipCompress(raw, turnbattle::kBattleReplayGzipLevel);
    ASSERT_TRUE(compressed.has_value());
    ASSERT_GE(compressed->size(), 18u);
    EXPECT_EQ('\x1f', (*compressed)[0]) << "gzip 魔数";
    EXPECT_EQ('\x8b', (*compressed)[1]);

    const auto restored = turnbattle::GzipDecompress(*compressed, raw.size());
    ASSERT_TRUE(restored.has_value());
    EXPECT_EQ(raw, *restored);

    EXPECT_FALSE(turnbattle::GzipDecompress(*compressed, raw.size() - 1).has_value()) << "解出超过上限即失败(防解压炸弹)";
    EXPECT_FALSE(turnbattle::GzipDecompress(compressed->substr(0, compressed->size() / 2), raw.size()).has_value()) << "截断";
    auto corrupt = *compressed;
    corrupt[corrupt.size() - 1] = static_cast<char>(corrupt[corrupt.size() - 1] ^ 0x5a); // 改坏 gzip 尾部的长度校验
    EXPECT_FALSE(turnbattle::GzipDecompress(corrupt, raw.size()).has_value()) << "尾部校验不符";

    const auto empty = turnbattle::GzipCompress(std::string{}, turnbattle::kBattleReplayGzipLevel);
    ASSERT_TRUE(empty.has_value());
    const auto emptyRestored = turnbattle::GzipDecompress(*empty, 0);
    ASSERT_TRUE(emptyRestored.has_value());
    EXPECT_TRUE(emptyRestored->empty());
}

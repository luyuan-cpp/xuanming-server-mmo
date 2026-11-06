#include "system/battle_replay_recorder.h"

#include <chrono>
#include <limits>
#include <utility>

#include <zlib.h>

#include "muduo/base/Logging.h"

namespace turnbattle {

namespace {

// gzip 头尾:zlib 的 windowBits 取 15 + 16 时写 / 读 RFC 1952 格式,而不是 zlib 格式(RFC 1950)。
constexpr int kGzipWindowBits = 15 + 16;
constexpr int kDeflateMemLevel = 8;

// 只对实时有用的字段:回放里没有「行动截止」和「还没出手的人」。
void ClearLiveOnlyFields(::BattleStateS2C& state)
{
    state.set_action_deadline_ms(0);
    state.clear_pending_actor_ids();
}

} // namespace

BattleReplayRecorder::BattleReplayRecorder(const BattleReplayHeader& header, const ::BattleStateS2C& spectatorStartState)
{
    replay_.set_battle_id(header.battleId);
    replay_.set_match_mode(header.matchMode);
    replay_.set_battle_config_id(header.battleConfigId);
    replay_.set_started_at_ms(header.startedAtMs);
    replay_.set_table_fingerprint(header.tableFingerprint);
    auto* start = replay_.mutable_start_state();
    *start = spectatorStartState;
    ClearLiveOnlyFields(*start);
    recordedBytes_ = start->ByteSizeLong();
}

void BattleReplayRecorder::AddRound(const ::TurnResultS2C& spectatorTurn)
{
    if (frozen_ || replay_.truncated())
    {
        return;
    }
    ::TurnResultS2C round = spectatorTurn;
    ClearLiveOnlyFields(*round.mutable_state());
    const std::size_t bytes = round.ByteSizeLong();
    if (recordedBytes_ + bytes > kBattleReplayRecordBudgetBytes)
    {
        // 截断只影响极长、单位极多的局:后面的回合不录,结局照 outcome 显示(设计 BR-5)。
        replay_.set_truncated(true);
        LOG_WARN << "metric=battle_replay_truncated battle_id=" << replay_.battle_id()
                 << " recorded_rounds=" << replay_.rounds_size() << " recorded_bytes=" << recordedBytes_
                 << " next_round_bytes=" << bytes;
        return;
    }
    *replay_.add_rounds() = std::move(round);
    recordedBytes_ += bytes;
}

void BattleReplayRecorder::MarkDeadlineDraw(const ::BattleStateS2C& spectatorState)
{
    if (frozen_)
    {
        return;
    }
    auto* finalState = replay_.mutable_final_state();
    *finalState = spectatorState;
    ClearLiveOnlyFields(*finalState);
    replay_.set_end_reason(::BATTLE_REPLAY_END_DEADLINE_DRAW);
}

std::optional<::BattleReplayEnvelope> BattleReplayRecorder::Freeze(const ::eBattleOutcome outcome, const uint32_t totalRounds,
                                                                   const uint64_t finishedAtMs)
{
    if (frozen_)
    {
        return std::nullopt;
    }
    frozen_ = true;
    replay_.set_outcome(outcome);
    replay_.set_total_rounds(totalRounds);
    replay_.set_finished_at_ms(finishedAtMs);
    if (replay_.end_reason() == ::BATTLE_REPLAY_END_UNSPECIFIED)
    {
        replay_.set_end_reason(::BATTLE_REPLAY_END_FINISHED);
    }
    const uint64_t battleId = replay_.battle_id();
    const int rounds = replay_.rounds_size();
    const bool truncated = replay_.truncated();

    std::string raw;
    const bool serialized = replay_.SerializeToString(&raw);
    replay_.Clear(); // 不论成败都释放已录的帧:房间之后还要等 match 的持久决定,不再需要它们
    recordedBytes_ = 0;
    if (!serialized || raw.empty())
    {
        LOG_ERROR << "metric=battle_replay_dropped reason=serialize_failed battle_id=" << battleId;
        return std::nullopt;
    }
    if (raw.size() > kMaxBattleReplayRawBytes)
    {
        LOG_WARN << "metric=battle_replay_dropped reason=too_large battle_id=" << battleId << " raw_bytes=" << raw.size();
        return std::nullopt;
    }

    const auto began = std::chrono::steady_clock::now();
    auto compressed = GzipCompress(raw, kBattleReplayGzipLevel);
    const auto elapsedUs = std::chrono::duration_cast<std::chrono::microseconds>(std::chrono::steady_clock::now() - began).count();
    if (!compressed)
    {
        LOG_ERROR << "metric=battle_replay_dropped reason=compress_failed battle_id=" << battleId << " raw_bytes=" << raw.size();
        return std::nullopt;
    }
    if (compressed->size() > kMaxBattleReplayPayloadBytes)
    {
        LOG_WARN << "metric=battle_replay_dropped reason=too_large battle_id=" << battleId << " raw_bytes=" << raw.size()
                 << " payload_bytes=" << compressed->size();
        return std::nullopt;
    }

    ::BattleReplayEnvelope envelope;
    envelope.set_format_version(kBattleReplayFormatVersion);
    envelope.set_codec(::BATTLE_REPLAY_CODEC_GZIP);
    envelope.set_raw_bytes(static_cast<uint32_t>(raw.size()));
    envelope.set_payload(std::move(*compressed));
    LOG_INFO << "metric=battle_replay_frozen battle_id=" << battleId << " rounds=" << rounds << " truncated=" << truncated
             << " raw_bytes=" << raw.size() << " payload_bytes=" << envelope.payload().size() << " compress_us=" << elapsedUs;
    return envelope;
}

std::optional<std::string> GzipCompress(const std::string& raw, const int level)
{
    if (raw.size() > std::numeric_limits<uInt>::max())
    {
        return std::nullopt;
    }
    z_stream stream{};
    if (deflateInit2(&stream, level, Z_DEFLATED, kGzipWindowBits, kDeflateMemLevel, Z_DEFAULT_STRATEGY) != Z_OK)
    {
        return std::nullopt;
    }
    // deflateBound 已把 gzip 头尾算进去,一次 Z_FINISH 就能写完。
    std::string out(deflateBound(&stream, static_cast<uLong>(raw.size())), '\0');
    stream.next_in = reinterpret_cast<Bytef*>(const_cast<char*>(raw.data()));
    stream.avail_in = static_cast<uInt>(raw.size());
    stream.next_out = reinterpret_cast<Bytef*>(out.data());
    stream.avail_out = static_cast<uInt>(out.size());
    const int rc = deflate(&stream, Z_FINISH);
    const uLong produced = stream.total_out;
    deflateEnd(&stream);
    if (rc != Z_STREAM_END)
    {
        return std::nullopt;
    }
    out.resize(produced);
    return out;
}

std::optional<std::string> GzipDecompress(const std::string& compressed, const std::size_t maxBytes)
{
    if (compressed.size() > std::numeric_limits<uInt>::max())
    {
        return std::nullopt;
    }
    z_stream stream{};
    if (inflateInit2(&stream, kGzipWindowBits) != Z_OK)
    {
        return std::nullopt;
    }
    stream.next_in = reinterpret_cast<Bytef*>(const_cast<char*>(compressed.data()));
    stream.avail_in = static_cast<uInt>(compressed.size());
    std::string out;
    char buffer[16384];
    int rc = Z_OK;
    while (rc != Z_STREAM_END)
    {
        stream.next_out = reinterpret_cast<Bytef*>(buffer);
        stream.avail_out = sizeof(buffer);
        rc = inflate(&stream, Z_NO_FLUSH);
        // Z_BUF_ERROR = 输入用完却没到流尾(数据被截断);其余非 OK 值都是损坏。
        if (rc != Z_OK && rc != Z_STREAM_END)
        {
            inflateEnd(&stream);
            return std::nullopt;
        }
        out.append(buffer, sizeof(buffer) - stream.avail_out);
        if (out.size() > maxBytes)
        {
            inflateEnd(&stream);
            return std::nullopt;
        }
    }
    inflateEnd(&stream);
    return out;
}

} // namespace turnbattle

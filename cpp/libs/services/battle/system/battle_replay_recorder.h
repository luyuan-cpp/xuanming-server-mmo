#pragma once

#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>

#include "proto/battle/battle_data.pb.h"
#include "proto/battle/player_battle.pb.h"

// 战报回放录制(docs/design/battle-report-replay.md §6)。
//
// battle 节点在 record_replay 为真的房间里持有一个录制器:开战时构造,每回合追加一帧,强制判平时记最后状态,
// 终局冻结时序列化并 gzip,结果随 CommitBattleOutcomeRequest.replay 提交 match。
// 输入必须是观众版帧:节点先过 RedactStateForViewer(state, 0) 再交进来,本模块不做脱敏(脱敏规则只在节点一处)。
// 只在 loop 线程调用;任何失败都不抛异常、不影响战斗,结果只是这一局没有回放。
namespace turnbattle {

// 回放外壳的版本与上限(BR-5)。match 侧 go/match/internal/authority/battle_report.go 同值,改一边要改两边。
inline constexpr uint32_t kBattleReplayFormatVersion = 1;
inline constexpr std::size_t kMaxBattleReplayRawBytes = std::size_t{1} << 20;      // 解码后 BattleReplay 的上限
inline constexpr std::size_t kMaxBattleReplayPayloadBytes = std::size_t{512} << 10; // 压缩后的上限
// 录制预算比原始上限少留 16 KiB:头部字段与 repeated 元素的 tag / 长度前缀不计入帧大小,序列化后仍要落在上限内。
inline constexpr std::size_t kBattleReplayRecordBudgetBytes = kMaxBattleReplayRawBytes - (std::size_t{16} << 10);
// gzip 压缩级别:1 = zlib 的 Z_BEST_SPEED。冻结在 loop 线程上做,先取最快档,按实测再定(设计 §9)。
inline constexpr int kBattleReplayGzipLevel = 1;

// 整场不变的头部字段,开战时就能定下来。
struct BattleReplayHeader
{
    uint64_t battleId = 0;
    uint32_t matchMode = 0;
    uint32_t battleConfigId = 0;
    uint64_t startedAtMs = 0;    // CreateBattleRequest.created_at_ms
    std::string tableFingerprint; // 本节点的战斗配表指纹
};

class BattleReplayRecorder
{
public:
    // 开战时构造:记下头部字段和开战状态(观众版)。
    BattleReplayRecorder(const BattleReplayHeader& header, const ::BattleStateS2C& spectatorStartState);

    // 追加一回合(观众版)。累计字节超过录制预算后不再追加,并标 truncated;冻结之后调用无效。
    void AddRound(const ::TurnResultS2C& spectatorTurn);

    // 整场期限到、节点强制判平时调用:记下强制结束那一刻的状态(观众版)和结束原因。
    void MarkDeadlineDraw(const ::BattleStateS2C& spectatorState);

    // 冻结:填结局字段,序列化,gzip。成功返回外壳;序列化或压缩失败、超过任一上限时返回空并打 metric 日志。
    // 无论成败,冻结后都释放已录的帧,之后再调用返回空。
    std::optional<::BattleReplayEnvelope> Freeze(::eBattleOutcome outcome, uint32_t totalRounds, uint64_t finishedAtMs);

    bool Truncated() const { return replay_.truncated(); }
    std::size_t RecordedBytes() const { return recordedBytes_; }
    int RecordedRounds() const { return replay_.rounds_size(); }

private:
    ::BattleReplay replay_;
    std::size_t recordedBytes_ = 0;
    bool frozen_ = false;
};

// gzip(RFC 1952)压缩。失败返回空。
std::optional<std::string> GzipCompress(const std::string& raw, int level);

// gzip 解压。解出的字节超过 maxBytes、数据不完整或损坏都返回空。测试与诊断用。
std::optional<std::string> GzipDecompress(const std::string& compressed, std::size_t maxBytes);

} // namespace turnbattle

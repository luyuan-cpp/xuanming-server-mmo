#pragma once

#include <algorithm>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <map>
#include <optional>
#include <set>
#include <string>
#include <utility>
#include <vector>

#include "proto/common/event/battle_event.pb.h"
#include "proto/contracts/kafka/match_event.pb.h"

namespace turnbattle
{
// 小于Kafka默认单消息约1MB，给键和协议开销留空间；两版消费者同一上限。
inline constexpr std::size_t kMaxCompleteResultBytes = 960 * 1024;

// 从首次引擎结算固定完整来源。先校验全部成员，失败不修改event；强制DRAW统一盖章。
// 仅归档来源，不能以这个格式版本代替玩家归属、源存档版本或资产SQL收据。
inline bool FreezeBattleSettlementSource(contracts::kafka::BattleResultEvent& event,
                                        std::vector<::BattleSettlementData> settlements)
{
    if (event.battle_id() == 0 || !::eBattleOutcome_IsValid(event.outcome()) ||
        event.outcome() == ::BATTLE_OUTCOME_ONGOING || settlements.empty() ||
        event.settlement_format_version() != 0 || event.settlements_size() != 0)
        return false;
    std::map<uint64_t, uint32_t> members;
    std::set<uint32_t> teams;
    for (const auto& team : event.teams())
    {
        if (team.player_ids().empty() || !teams.insert(team.team_index()).second) return false;
        for (const auto playerId : team.player_ids())
            if (playerId == 0 || !members.emplace(playerId, team.team_index()).second) return false;
    }
    if (members.size() != settlements.size()) return false;
    std::sort(settlements.begin(), settlements.end(), [](const auto& a, const auto& b)
    { return a.player_id() < b.player_id(); });
    uint64_t previous = 0;
    for (const auto& settlement : settlements)
    {
        const auto member = members.find(settlement.player_id());
        if (member == members.end() || settlement.player_id() == previous ||
            member->second != settlement.player_team_index() ||
            settlement.total_rounds() != event.total_rounds() ||
            (settlement.battle_id() != 0 && settlement.battle_id() != event.battle_id())) return false;
        previous = settlement.player_id();
    }
    auto frozen = event;
    for (auto& settlement : settlements)
    {
        settlement.set_battle_id(event.battle_id());
        settlement.set_outcome(event.outcome());
        *frozen.add_settlements() = std::move(settlement);
    }
    frozen.set_settlement_format_version(1);
    if (frozen.ByteSizeLong() > kMaxCompleteResultBytes) return false;
    event = std::move(frozen);
    return true;
}

// 冻结后的结果原件里，每名成员的 R07 个人结算字节(BattleSettlementEvent)，按 player_id 索引。
// 结果第一次发往 broker 之前用它写 pending，回执之后的派发也只用这同一份字节(审计 social-match-battle#2)。
// 回执之后不得再改写 pending：那时 scene 可能已应用并销账，账本条目随销账摘除；改写会让 pending 复活，
// 玩家下次登录就会再发一次奖。
// 原件不是 v1 完整来源、没有成员、成员缺 player_id 或重复、battle_id / outcome 与原件不符、序列化失败时，
// 返回 false 且不改 out。
inline bool BuildPersonalSettlementPayloads(const contracts::kafka::BattleResultEvent& committed,
                                            std::map<uint64_t, std::string>& out)
{
    if (committed.settlement_format_version() != 1 || committed.settlements_size() == 0) return false;
    std::map<uint64_t, std::string> payloads;
    for (const auto& settlement : committed.settlements())
    {
        if (settlement.player_id() == 0 || settlement.battle_id() != committed.battle_id() ||
            settlement.outcome() != committed.outcome()) return false;
        ::BattleSettlementEvent event;
        *event.mutable_settlement() = settlement;
        std::string payload;
        if (!event.SerializeToString(&payload) ||
            !payloads.emplace(settlement.player_id(), std::move(payload)).second) return false;
    }
    out = std::move(payloads);
    return true;
}

inline constexpr bool CreationReceiptExpired(uint64_t expiryMs,uint64_t nowMs,bool roomExists)
{
    return expiryMs < nowMs && !roomExists;
}

// 个人结算落库的重试节奏与总跨度(审计 r2-battle#4,AGENTS §11.3 重试必须有超时、上限与退避)。
// 2026-10-22 起分两个阶段，由总跨度兜底(原来 24 轮用尽即永久停住，只能人工删 Pod 收尾)：
//   - 快速阶段：前 kFastStagingRounds 轮按与投递相同的 1→30s 退避。只数真正开出去的轮(Redis 未连上时
//     调用方不开轮)。前 5 次失败共等 31s，之后每轮 30s；第 24 轮在第 1 轮之后约 571s(约 9.5 分钟)开出，
//     远大于一次主从切换；
//   - 低频阶段：快速阶段用完仍没落定，多半是持续性故障(OOM、NOAUTH/ACL、键类型错)，改为每
//     kSlowStagingRetryInterval 一轮。故障排除后结果自动发出，不必人工处置。重写是安全的：每轮只写没落定的成员，
//     已写过的由本局落库标记挡住(settlement_outbox.h 的 kStagePendingSettlementOnceScript 返回 2)；
//   - 总跨度：从第一次问起落库(BeginStaging / ExhaustStagingIfOverdue 的第一次调用)起满 kMaxStagingSpan 即判用尽，
//     不再开轮。按单调时钟计，与 Redis 连不连得上无关，所以「标记还在」不再取决于连通性。
// 跨度上限的依据是落库标记的寿命：某成员上一轮结论未知时，下一轮只能靠标记判出「已写过」；标记的 TTL
// (kStagedSettlementMembersTtlSec,24 小时)从它最后一次写入算起，而写入不早于跨度起点，所以只要跨度远小于 TTL，
// 任何一轮开出时标记必然还在。取 6 小时，留 4 倍余量(Redis 主机墙钟跳变、命令在输入缓冲里滞留)，
// battle_room_manager.cpp 有 static_assert 守住这个关系，单测同样断言。
// 6 小时里低频阶段约 70 轮，每轮只写没落定的成员(不超过一局人数)，对 Redis 的压力可以忽略。
// 用尽后终局保持未知，CanSend 恒为假：放弃落库直接发结果会退回 social-match-battle#2 的原缺陷。
inline constexpr uint32_t kFastStagingRounds = 24;
inline constexpr std::chrono::minutes kSlowStagingRetryInterval{5};
inline constexpr std::chrono::hours kMaxStagingSpan{6};
static_assert(kSlowStagingRetryInterval < kMaxStagingSpan, "the slow staging phase must get at least one round");

// 单逻辑线程所有。载荷一经计算不可改；只有本次关联号的 all ACK 能放行终局。
//
// 发送的前置条件(审计 social-match-battle#2)：载荷里全部成员的个人结算已写进 R07 pending。
// 在这之前 CanSend 恒为假。这样结果一旦可能到达 broker，battle 进程无论在回执前停机还是崩溃，
// 个人结算都已有耐久副本，由 scene 登录钩子 / 冻结到期补读应用，不依赖本进程活到回执。
// 落库按轮进行：BeginStaging 发号，调用方对 UnstagedMembers() 各写一次，回复到齐后 FinishStaging 一次。
// 按成员记结论(审计 r2-battle#1)：拿到确定结论的成员移出待写集合，后续轮次只重写其余成员。
// 已写进去的成员 scene 可能随时应用并销账(删 pending 与锁、摘账本)，再写一次 pending 就会复活，
// 玩家下次登录重复领奖。结论未知的成员(空应答 / ERROR / 命令没发出)下一轮照写，
// 「命令其实执行过」由落库脚本自己判出来(settlement_outbox.h 的 kStagePendingSettlementOnceScript)。
// 失败先快后慢地重来，总跨度 kMaxStagingSpan 到了即判用尽；用尽后终局保持未知，CanSend 恒为假。
// 过期轮次(attempt 不符)的迟到回复不改状态。
class ResultCommit
{
public:
    using Clock = std::chrono::steady_clock;
    // stagingMembers：落库前置条件要覆盖的成员(确认模式 = 全部参战者)。为空时永远不放行(fail-closed)，
    // 只给序列化失败、成员对不上这类「终局未知」的占位用。
    explicit ResultCommit(std::string payload, std::set<uint64_t> stagingMembers = {})
        : payload_(std::move(payload)), unstaged_(std::move(stagingMembers)) {}
    const std::string& Payload() const { return payload_; }
    bool Confirmed() const { return confirmed_; }
    bool InFlight() const { return inFlight_ != 0; }
    uint32_t Failures() const { return failures_; }
    bool PersonalSettlementsDurable() const { return staged_; }
    // 还没拿到确定结论的成员。本轮在途期间(BeginStaging 之后、FinishStaging 之前)不变。
    const std::set<uint64_t>& UnstagedMembers() const { return unstaged_; }
    uint32_t StagingFailures() const { return stagingFailures_; }
    // 落库已到总跨度上限(kMaxStagingSpan)而没落定：不再开新轮，结果不发，终局保持未知。
    bool StagingExhausted() const { return stagingExhausted_; }
    // 调用方每次驱动落库都先调它，Redis 连不上时也调：总跨度与连通性无关，Redis 一直断着也会到点判用尽、出告警。
    // 跨度从第一次调用本函数或 BeginStaging 时传入的 now 起算。到上限、还没落定、且没有在途轮次时判用尽，
    // 只在判定的那一次返回 true(调用方据此告警、停止驱动，恰好一次)。有轮次在途时不判：它的回复到齐后
    // FinishStaging 可能恰好落定，下一次调用再判。
    bool ExhaustStagingIfOverdue(Clock::time_point now)
    {
        AnchorStagingSpan(now);
        if (staged_ || stagingExhausted_ || stagingAttempt_ != 0 || !StagingSpanReached(now)) return false;
        stagingExhausted_ = true;
        return true;
    }
    // 开一轮落库，返回本轮编号；已落库、已用尽、总跨度已到、没有待写成员、上一轮还在途或仍在退避期时返回 0。
    // 总跨度到了只拒绝开轮、不改状态：用尽由 ExhaustStagingIfOverdue 判，告警因此只出一次；
    // 调用方漏调它时，这里仍保证跨度之外不会再开轮。
    uint64_t BeginStaging(Clock::time_point now)
    {
        AnchorStagingSpan(now);
        if (staged_ || stagingExhausted_ || unstaged_.empty() || stagingAttempt_ != 0 || now < stagingRetryAt_ ||
            StagingSpanReached(now))
            return 0;
        stagingAttempt_ = ++stagingSeq_;
        return stagingAttempt_;
    }
    // 本轮全部成员的落库回复到齐后调用一次。settledMembers = 本轮拿到确定结论(本次写入 / 本局早先已写 /
    // 已被别的局取代)的成员，只把它们移出待写集合；不在待写集合里的 id 忽略。按值接收：调用方传
    // UnstagedMembers() 本身时也不会边遍历边删。返回 true 表示全部成员已落定，此后允许发送。
    bool FinishStaging(uint64_t attempt, std::set<uint64_t> settledMembers, Clock::time_point now)
    {
        if (attempt == 0 || attempt != stagingAttempt_ || staged_) return false;
        stagingAttempt_ = 0;
        for (const auto member : settledMembers) unstaged_.erase(member);
        if (unstaged_.empty()) { staged_ = true; return true; }
        if (stagingFailures_ != UINT32_MAX) ++stagingFailures_;
        stagingRetryAt_ = now + StagingRetryDelayAfter(stagingFailures_);
        return false;
    }
    bool CanSend(Clock::time_point now) const { return staged_ && !confirmed_ && !InFlight() && now >= retryAt_; }
    bool Begin(uint64_t correlation, Clock::time_point now)
    {
        if (correlation == 0 || !CanSend(now)) return false;
        inFlight_ = correlation;
        return true;
    }
    bool Deliver(uint64_t correlation, bool allAcknowledged, Clock::time_point now)
    {
        if (correlation == 0 || correlation != inFlight_ || confirmed_) return false;
        inFlight_ = 0;
        if (allAcknowledged) { confirmed_ = true; return true; }
        const auto delay = BackoffAfter(failures_);
        if (failures_ != UINT32_MAX) ++failures_;
        retryAt_ = now + delay;
        return false;
    }
private:
    static std::chrono::seconds BackoffAfter(uint32_t failures)
    {
        return std::chrono::seconds(std::min(1u << std::min(failures, uint32_t{5}), uint32_t{30}));
    }
    // 第 failedRounds 次(≥ 1)落库失败之后，到下一轮的等待：快速阶段 1、2、4…30 秒封顶，
    // 快速阶段的 kFastStagingRounds 轮全部失败之后固定为低频间隔(推导见 kFastStagingRounds 的说明)。
    static Clock::duration StagingRetryDelayAfter(uint32_t failedRounds)
    {
        if (failedRounds < kFastStagingRounds) return BackoffAfter(failedRounds - 1);
        return kSlowStagingRetryInterval;
    }
    void AnchorStagingSpan(Clock::time_point now)
    {
        if (!stagingStartedAt_) stagingStartedAt_ = now;
    }
    bool StagingSpanReached(Clock::time_point now) const
    {
        return stagingStartedAt_ && now - *stagingStartedAt_ >= kMaxStagingSpan;
    }

    std::string payload_;
    uint64_t inFlight_ = 0;
    uint32_t failures_ = 0;
    bool confirmed_ = false;
    Clock::time_point retryAt_{};
    bool staged_ = false;
    std::set<uint64_t> unstaged_;
    bool stagingExhausted_ = false;
    uint64_t stagingAttempt_ = 0;
    uint64_t stagingSeq_ = 0;
    uint32_t stagingFailures_ = 0;
    Clock::time_point stagingRetryAt_{};
    // 总跨度的起点(单调时钟，调用方传入，不读墙钟)；第一次问起落库时定下，之后不变。
    std::optional<Clock::time_point> stagingStartedAt_;
};

// 终局卡在本节点、节点因此排不空的房间按原因分类(2026-10-22 评审补，r2-battle#4)。
// 三类都不会自然消失，只能等停机 AbortAllRooms 清房，值班排空时要一起等；
// battle_room_manager.cpp 的 ReportExhaustedStagingRooms 每 60s 按本函数数一遍房间表。
enum class StuckTerminal : uint8_t
{
    kNone,
    // 旧协议确认模式：个人结算落库到总跨度上限(ResultCommit::StagingExhausted)，结果不发。
    kStagingExhausted,
    // 终局协议 v1：CommitBattleOutcome 到总跨度上限仍没拿到 SQL 终局，或终局原件生成失败(房间的 outcomeExhausted)。
    kSqlOutcomeUnconfirmed,
    // 旧协议确认模式：结果序列化失败 / 个人结算与成员对不上的占位(载荷为空、没有待写成员，永不放行)。
    kPlaceholder,
};

// commit = 房间的 ResultCommit(没有时传 nullptr)；sqlOutcomeExhausted = v1 终局已重试到上限或原件无效。
// 两种协议互斥(v1 房间不建 ResultCommit)，先判 v1 只为让结论唯一。
inline StuckTerminal ClassifyStuckTerminal(const ResultCommit* commit, bool sqlOutcomeExhausted)
{
    if (sqlOutcomeExhausted) return StuckTerminal::kSqlOutcomeUnconfirmed;
    if (commit == nullptr) return StuckTerminal::kNone;
    // 正常的结果载荷是序列化后的 BattleResultEvent(battle_id 非 0)，不可能为空；空载荷只来自占位。
    if (commit->Payload().empty()) return StuckTerminal::kPlaceholder;
    if (commit->StagingExhausted()) return StuckTerminal::kStagingExhausted;
    return StuckTerminal::kNone;
}

inline const char* StuckTerminalName(StuckTerminal stuck)
{
    switch (stuck)
    {
    case StuckTerminal::kNone: return "none";
    case StuckTerminal::kStagingExhausted: return "staging_exhausted";
    case StuckTerminal::kSqlOutcomeUnconfirmed: return "sql_unconfirmed";
    case StuckTerminal::kPlaceholder: return "placeholder";
    }
    return "unknown";
}

// 停机时这间房是否还要在排空窗口内把终局再提交一轮(2026-10-23 修复，审计 r2-battle#2)。
// 全球局一律走终局协议 v1：整局终局与全员个人结算由 match 在同一个 SQL 事务里提交(CommitBattleOutcome → COMPLETE)，
// 之后由 match 的持久发件箱补发结果与个人结算；没有 COMPLETE 的局到权威截止后由 match 原子判 ABORT，全员作废。
// 两种终态都覆盖整局成员，停机不会留下「部分成员已结算」的局；停机能做的只剩一件事：已经算出、还没拿到 SQL 决定的终局
// 不要白白作废，退出前再提交。
// 只有这一种房间要补提交：终局原件已冻结(outcomePending)、还没拿到 COMPLETE / ABORT、也没判用尽(总跨度上限 / 原件无效，
// 那时终局交给 match 恢复裁决)。旧协议房间、还在打的 v1 房间都不在此列：后者没有终局可交，按作废处理(match 截止后判 ABORT)。
inline bool NeedsShutdownOutcomeFlush(uint32_t outcomeProtocol, bool outcomePending, bool outcomeComplete,
                                      bool outcomeExhausted)
{
    return outcomeProtocol == 1 && outcomePending && !outcomeComplete && !outcomeExhausted;
}

// 一轮落库的回复汇总(battle_room_manager.cpp StagePersonalSettlements 每轮一份，回调共享)。
// 本轮成员 = 开轮那一刻的 ResultCommit::UnstagedMembers()，整轮不变。每名成员只记第一次回复，
// 不在本轮成员里的 id 忽略；全部成员都回复过的那一次 Record 返回 true(恰好一次)，调用方随即把
// Settled() 交给 ResultCommit::FinishStaging。单逻辑线程所有，不加锁。
class StagingRoundTally
{
public:
    explicit StagingRoundTally(std::set<uint64_t> members) : members_(std::move(members)) {}
    // Record 不改它：调用方可以一边遍历它发命令，一边在同步失败时当场 Record。
    const std::set<uint64_t>& Members() const { return members_; }
    // settled = 该成员拿到确定结论(settlement_outbox.h 的 IsStageSettled)。
    bool Record(uint64_t member, bool settled)
    {
        if (!members_.contains(member) || !replied_.insert(member).second) return false;
        if (settled) settled_.insert(member);
        return replied_.size() == members_.size();
    }
    const std::set<uint64_t>& Settled() const { return settled_; }
private:
    std::set<uint64_t> members_;
    std::set<uint64_t> replied_;
    std::set<uint64_t> settled_;
};
}

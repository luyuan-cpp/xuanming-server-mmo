#pragma once

#include <algorithm>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <map>
#include <set>
#include <utility>
#include <vector>

#include "proto/battle/battle_data.pb.h"
#include "proto/contracts/kafka/match_event.pb.h"

namespace turnbattle
{
// 本节点唯一支持的终局协议版本:battle 把终局提交 match SQL(CommitBattleOutcome),match 发件箱投 player_inbox,
// scene 经 ApplyInboxSettlement 应用。其它版本的创建请求一律拒绝(player-data-tidb-scaling-plan.md §5.4)。
inline constexpr uint32_t kOutcomeProtocolVersion = 1;

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

inline constexpr bool CreationReceiptExpired(uint64_t expiryMs,uint64_t nowMs,bool roomExists)
{
    return expiryMs < nowMs && !roomExists;
}

// 停机时这间房是否还要在排空窗口内把终局再提交一轮(2026-10-23 修复，审计 r2-battle#2)。
// 整局终局与全员个人结算由 match 在同一个 SQL 事务里提交(CommitBattleOutcome → COMPLETE)，
// 之后由 match 的持久发件箱补发结果与个人结算；没有 COMPLETE 的局到权威截止后由 match 原子判 ABORT，全员作废。
// 两种终态都覆盖整局成员，停机不会留下「部分成员已结算」的局；停机能做的只剩一件事：已经算出、还没拿到 SQL 决定的终局
// 不要白白作废，退出前再提交。
// 只有这一种房间要补提交：终局原件已冻结(outcomePending)、还没拿到 COMPLETE / ABORT、也没判用尽(总跨度上限 / 原件无效，
// 那时终局交给 match 恢复裁决)。还在打的房间没有终局可交，按作废处理(match 截止后判 ABORT)。
inline bool NeedsShutdownOutcomeFlush(bool outcomePending, bool outcomeComplete, bool outcomeExhausted)
{
    return outcomePending && !outcomeComplete && !outcomeExhausted;
}

// SQL 终局(CommitBattleOutcome)重放节奏(2026-10-30,battle_room_manager.cpp 的 PumpSqlOutcomes 按它判)。
// 原先从终局冻结(outcomeSince)起满快速跨度即判用尽、移出待提交集合、此后不再重放;可房间只在拿到 COMPLETE / ABORT
// 时才删,于是这间房永远留在节点上(每 60s 报一次 sql_unconfirmed),节点排不空。现在分三段:
//   - 快速重放:kSqlOutcomeFastReplaySpan 之内每 kSqlOutcomeRetryInterval 重放一次同一原件;
//   - 越界(SqlOutcomeSpanExceeded):照旧置 outcomeExhausted、打一次 ERROR、计入滞留房间,但**不停**,改为每
//     kSqlOutcomeSlowRetryInterval 重放一次。Match 对同一原件按持久决定幂等回放,到权威截止后的 ABORT 也由 Match 自己落,
//     所以慢速重放迟早拿到 COMPLETE / ABORT,房间照常收尾;
//   - 放弃(SqlOutcomeReplayAbandoned):越界之后再过 kSqlOutcomeSlowReplaySpan 仍无决定(Match 长期不可达),本地删房并打 ERROR
//     (AGENTS §11.3 重试必须有上限)。本地删房不是 ABORT,不发布任何终局:终局仍由 Match 凭持久原件恢复裁决。
// 停机补提交(NeedsShutdownOutcomeFlush)与滞留提醒照旧只看 outcomeExhausted,不受这里影响。
inline constexpr std::chrono::hours kSqlOutcomeFastReplaySpan{6};
inline constexpr std::chrono::seconds kSqlOutcomeRetryInterval{5};
inline constexpr std::chrono::seconds kSqlOutcomeSlowRetryInterval{60};
inline constexpr std::chrono::hours kSqlOutcomeSlowReplaySpan{24};
static_assert(kSqlOutcomeRetryInterval < kSqlOutcomeSlowRetryInterval, "越界之后的重放只能更慢");
static_assert(kSqlOutcomeSlowRetryInterval < kSqlOutcomeSlowReplaySpan, "放弃之前至少还要慢速重放一轮");

// 从终局冻结起已过 sinceFrozen(单调时钟,调用方传入),是否已越过快速重放的总跨度。
inline constexpr bool SqlOutcomeSpanExceeded(std::chrono::steady_clock::duration sinceFrozen)
{
    return sinceFrozen >= kSqlOutcomeFastReplaySpan;
}

// 越界之后的慢速重放是否也到了上限:到了就本地删房、不再重放。
inline constexpr bool SqlOutcomeReplayAbandoned(std::chrono::steady_clock::duration sinceFrozen)
{
    return sinceFrozen >= kSqlOutcomeFastReplaySpan + kSqlOutcomeSlowReplaySpan;
}

// 下一次重放的间隔:越界(outcomeExhausted)之后每分钟一次,之前每 5 秒一次。
inline constexpr std::chrono::seconds SqlOutcomeRetryInterval(bool outcomeExhausted)
{
    return outcomeExhausted ? kSqlOutcomeSlowRetryInterval : kSqlOutcomeRetryInterval;
}
}

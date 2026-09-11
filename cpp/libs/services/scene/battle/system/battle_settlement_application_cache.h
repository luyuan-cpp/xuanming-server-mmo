#pragma once

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <map>
#include <utility>

namespace battle_settlement {

// 只保护同一 scene loop 的结算应用；不持有实体，正常下线重建后仍能识别同一局。
// 保留期取 pending 的 7 天，上限约束进程内存；满时淘汰最早已完成项，绝不淘汰正在
// 应用的项。超过保留期/容量或进程重启后仍依赖 Redis 条件销账，不是持久化 exactly-once。
class SettlementApplicationCache {
public:
    using Clock = std::chrono::steady_clock;
    enum class Result { Applied, Duplicate, InFlight, Failed, Full };

    explicit SettlementApplicationCache(std::size_t capacity = 131072,
        Clock::duration retention = std::chrono::hours(24 * 7))
        : capacity_(capacity), retention_(retention) {}

    // 回调返回 false 必须发生在不可重复的副作用之前；调用者保留 pending 供重投。
    // Apply 是同步 loop 内操作，处理中标记同时挡住领域事件触发的重入。
    template<class ApplyFn>
    Result Apply(uint64_t playerId, uint64_t battleId, Clock::time_point now, ApplyFn&& apply) {
        if (playerId == 0 || battleId == 0) return Result::Failed;
        Prune(now);
        const Key key{playerId, battleId};
        if (const auto current = entries_.find(key); current != entries_.end()) {
            return current->second ? Result::Duplicate : Result::InFlight;
        }
        while (entries_.size() >= capacity_ && !completed_.empty()) {
            entries_.erase(completed_.front().first);
            completed_.pop_front();
        }
        if (entries_.size() >= capacity_) return Result::Full;
        entries_.emplace(key, false);
        try {
            if (!std::forward<ApplyFn>(apply)()) {
                entries_.erase(key);
                return Result::Failed;
            }
        } catch (...) {
            entries_.erase(key);
            throw;
        }
        entries_.at(key) = true;
        completed_.emplace_back(key, now + retention_);
        return Result::Applied;
    }

    std::size_t Size() const { return entries_.size(); }

private:
    using Key = std::pair<uint64_t, uint64_t>;
    void Prune(Clock::time_point now) {
        while (!completed_.empty() && completed_.front().second <= now) {
            entries_.erase(completed_.front().first);
            completed_.pop_front();
        }
    }

    std::size_t capacity_;
    Clock::duration retention_;
    std::map<Key, bool> entries_; // false=应用中，true=已完成
    std::deque<std::pair<Key, Clock::time_point>> completed_;
};

} // namespace battle_settlement

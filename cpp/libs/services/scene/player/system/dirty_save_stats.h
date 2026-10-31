#pragma once

#include <atomic>
#include <cstdint>

// Dirty-save statistics for the proto-compare fast-path in SavePlayerToRedis.
//
// Background: SavePlayerToRedis (player_lifecycle.cpp) compares the freshly
// marshaled PlayerAllData against PlayerLastPersistedSnapshotComp via
// dirty_save::IsEqual; on a match the entire save (one player_store commit)
// is skipped. This is a sizable write-traffic win during idle /
// AFK / move-only gameplay where most of the proto is unchanged between
// periodic 300s save ticks.
//
// We need numbers, not vibes — these counters expose the skip rate so we
// can prove the optimization fires under stress. Two counters:
//   * total   — every SavePlayerToRedis call that reaches the fast-path
//               check (i.e. valid entity, marshaled OK; pre-condition
//               failures don't count)
//   * skipped — calls that returned early because IsEqual matched the
//               last persisted snapshot
//
// Skip ratio = skipped / total.
//
// save_marshal_refused 单独统计 SavePlayerToRedisImpl 因缺少有效 Guid 或持久组件,
// 无法组装完整存盘消息而拒绝的次数。它不计入 total/skipped,不计相等跳过、交接跳过,
// 也不计已经失效的实体,不带任何玩家维度。进程启动以来累计,由同一 30s 定时器汇总。
//
// Why not Prometheus: cpp scene processes don't currently expose a
// metrics HTTP endpoint (Round 16 added :9150 / :9160 only on the go
// SceneManager + db services). Rather than dragging in prometheus-cpp
// just for two counters, we piggyback on the existing 30s
// LogQueueSnapshot timer in RedisSystem and emit one LOG_INFO line
// (parsed by stress_summarize.ps1).
//
// std::atomic<uint64_t> with relaxed ordering: counters are
// fire-and-forget; readers are the LOG sink which can tolerate a
// tearing read on 32-bit (we don't run there) and a slightly stale
// view (logs print every 30s — a single sample miss is invisible).
namespace dirty_save_stats {

inline std::atomic<uint64_t>& Total()
{
    static std::atomic<uint64_t> g_total{0};
    return g_total;
}

inline std::atomic<uint64_t>& Skipped()
{
    static std::atomic<uint64_t> g_skipped{0};
    return g_skipped;
}

inline std::atomic<uint64_t>& MarshalRefused()
{
    static std::atomic<uint64_t> g_marshalRefused{0};
    return g_marshalRefused;
}

// Convenience wrappers so call sites read like prose and we don't have
// to repeat memory_order_relaxed everywhere. The save-path overhead of
// a relaxed inc is a single LOCK XADD on x86 — well below the cost of
// the proto compare itself, and dwarfed by the avoided Redis + Kafka
// round trip when we hit the skip branch.
inline void IncTotal()   { Total().fetch_add(1, std::memory_order_relaxed); }
inline void IncSkipped() { Skipped().fetch_add(1, std::memory_order_relaxed); }
inline void IncMarshalRefused() { MarshalRefused().fetch_add(1, std::memory_order_relaxed); }

// Read-only snapshot. All fields use relaxed loads — we accept a tiny
// inconsistency between the two reads for cheaper reads; the only
// consumer is a periodic LOG_INFO and a small lag is invisible.
struct Snapshot
{
    uint64_t total;
    uint64_t skipped;
    uint64_t marshalRefused;

    // Skip percentage rounded to one decimal, returned as integer
    // tenths-of-a-percent so the LOG path doesn't drag in floating
    // point formatting. e.g. 723 == 72.3%.
    uint32_t SkipPctTenths() const
    {
        if (total == 0) { return 0; }
        // 1000 * skipped / total without overflow as long as
        // skipped < 2^54 — billions of saves still fit comfortably.
        return static_cast<uint32_t>((skipped * 1000ULL) / total);
    }
};

inline Snapshot Read()
{
    return Snapshot{
        Total().load(std::memory_order_relaxed),
        Skipped().load(std::memory_order_relaxed),
        MarshalRefused().load(std::memory_order_relaxed)};
}

} // namespace dirty_save_stats

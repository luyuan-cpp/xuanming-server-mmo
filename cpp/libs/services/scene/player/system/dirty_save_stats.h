#pragma once

#include <atomic>
#include <cstdint>

// 存盘尝试和「一行都没变」跳过的计数。
//
// 现在的判脏:SavePlayerImpl(player_lifecycle.cpp)拼出整份 PlayerAllData,SubmitPlayerSave 按行投影并确定性序列化,
// PlayerStoreClient 逐行和上一次确认提交的字节比较,只发变了的行;一行都没变、也不用释放写权或补代次时不发请求。
// 规格见 docs/design/player-save-direct-commit.md,开销讨论见 docs/design/player-data-tidb-scaling-plan.md §4.6。
//
//   total           走到判脏的存盘尝试:实体有效、拼数据成功、不在交接中
//   skipped         一行都没变、没有发请求的次数(SubmitResult::kAlreadyPersisted)
//   piggybackDeferred 定时存盘只遇到位置这类只顺带的行有变化、推迟写入的次数(计入 total,不计入 skipped)
//   marshalRefused  拒存次数:缺有效 Guid 或持久组件、拼不出完整数据(这类不计入 total),
//                   以及按行投影失败(发生在计入 total 之后)
//
// 跳过率 = skipped / total。进程启动以来累计,不带玩家维度。
// 读者:RedisSystem 每 30 秒的 [DirtySave] / [PlayerSave] 汇总日志(stress_summarize.ps1 解析),以及 /metrics 的
// scene_player_save_events_total(player_save_metrics.cpp)。各段耗时在 save_timing_stats.h。
//
// relaxed 原子:计数只增不减,读者容忍两次读取之间差一两次。
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

// 定时存盘只遇到只顺带的行(位置)有变化、把这一笔推迟的次数(SaveResult::kDeferred,
// player-save-direct-commit.md §1.1)。计入 total,不计入 skipped:位置变了,只是晚点写。
inline std::atomic<uint64_t>& PiggybackDeferred()
{
    static std::atomic<uint64_t> g_piggybackDeferred{0};
    return g_piggybackDeferred;
}

// 调用点不必每处都写 memory_order_relaxed。
inline void IncTotal()   { Total().fetch_add(1, std::memory_order_relaxed); }
inline void IncSkipped() { Skipped().fetch_add(1, std::memory_order_relaxed); }
inline void IncMarshalRefused() { MarshalRefused().fetch_add(1, std::memory_order_relaxed); }
inline void IncPiggybackDeferred() { PiggybackDeferred().fetch_add(1, std::memory_order_relaxed); }

// 只读快照,各项分别 relaxed 读。
struct Snapshot
{
    uint64_t total;
    uint64_t skipped;
    uint64_t marshalRefused;
    uint64_t piggybackDeferred;

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
        MarshalRefused().load(std::memory_order_relaxed),
        PiggybackDeferred().load(std::memory_order_relaxed)};
}

} // namespace dirty_save_stats

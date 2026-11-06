#pragma once

#include <array>
#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <string_view>

// 玩家存盘的分段耗时(docs/design/player-data-tidb-scaling-plan.md §4.6「下一步」)。
//
// 为什么要:判脏和存盘的开销排序(写库 > 序列化 > 比字节)是经验判断,没有实测。§4.6 那张表里的优化
// (周期存盘摊到多个 tick、大行加变化提示、confirmed 换哈希、拆资产行)都要看这里的数据再定。
//
// 分段:
//   marshal       ECS → PlayerAllData(MarshalPlayerForSave),每次存盘尝试一次
//   project       按行投影并确定性序列化(player_store_rows::ProjectPlayer)
//   compare       逐行和上一次确认提交的字节比较(PlayerStoreClient::DirtyKinds);排队的快照在真正发出时才比。
//                 定时存盘的推迟判断(DeferPeriodicSave)也是一次比较,同样计入;没推迟时 Start 再比一次,所以次数多于存盘次数
//   submit        交给存盘客户端:比较、组请求、发出;已有在途存盘时只是记下这份快照
//   periodic_tick 周期存盘定时器的单次回调:这一秒轮到的全部玩家,含遍历在线表
//
// 读者:RedisSystem 每 30 秒的 [SaveTiming] 汇总日志(窗口值),以及 /metrics(player_save_metrics.h,进程累计值)。
// 计数只在 scene 逻辑线程上写,用 relaxed 原子,与其他统计同一写法;玩家号不进任何维度。
// 计时用单调时钟,结果只进指标,不参与任何判定。
namespace save_timing_stats
{
    using Clock = std::chrono::steady_clock;

    enum class Stage : uint8_t
    {
        kMarshal,
        kProject,
        kCompare,
        kSubmit,
        kPeriodicTick,
        kCount,
    };

    inline constexpr std::size_t kStageCount = static_cast<std::size_t>(Stage::kCount);

    // 汇总日志的 key 前缀与指标的 stage 标签共用这张表;改名要同步改 tools/scripts/stress_summarize.ps1。
    inline constexpr std::array<std::string_view, kStageCount> kStageNames{
        "marshal", "project", "compare", "submit", "periodic_tick"};
    static_assert(kStageNames.size() == kStageCount);

    // §4.6 的判据初值:周期回调单次超过它,就把这一秒的玩家摊到多个 tick。
    inline constexpr std::chrono::milliseconds kPeriodicTickBudget{10};

    struct StageCounters
    {
        std::atomic<uint64_t> count{0};
        std::atomic<uint64_t> totalNs{0};
        std::atomic<uint64_t> maxNs{0};       // 进程启动以来
        std::atomic<uint64_t> windowMaxNs{0}; // 上一次 TakeWindowMax 之后
    };

    struct Counters
    {
        std::array<StageCounters, kStageCount> stages;
        std::atomic<uint64_t> periodicTickOverBudget{0};
    };

    inline Counters &Global()
    {
        static Counters counters;
        return counters;
    }

    inline void RaiseMax(std::atomic<uint64_t> &slot, uint64_t value)
    {
        uint64_t current = slot.load(std::memory_order_relaxed);
        while (value > current && !slot.compare_exchange_weak(current, value, std::memory_order_relaxed))
        {
        }
    }

    inline void Observe(Stage stage, Clock::duration elapsed)
    {
        const auto ns = std::chrono::duration_cast<std::chrono::nanoseconds>(elapsed).count();
        const uint64_t value = ns > 0 ? static_cast<uint64_t>(ns) : 0;
        Counters &counters = Global();
        StageCounters &slot = counters.stages[static_cast<std::size_t>(stage)];
        slot.count.fetch_add(1, std::memory_order_relaxed);
        slot.totalNs.fetch_add(value, std::memory_order_relaxed);
        RaiseMax(slot.maxNs, value);
        RaiseMax(slot.windowMaxNs, value);
        if (stage == Stage::kPeriodicTick && elapsed > kPeriodicTickBudget)
        {
            counters.periodicTickOverBudget.fetch_add(1, std::memory_order_relaxed);
        }
    }

    // 调用点的写法:start = Clock::now(); 做事; ObserveSince(段, start)。
    inline void ObserveSince(Stage stage, Clock::time_point start)
    {
        Observe(stage, Clock::now() - start);
    }

    struct StageSnapshot
    {
        uint64_t count{0};
        uint64_t totalNs{0};
        uint64_t maxNs{0};
    };

    struct Snapshot
    {
        std::array<StageSnapshot, kStageCount> stages{};
        uint64_t periodicTickOverBudget{0};
    };

    // 各项分别 relaxed 读,彼此之间可能差一两次观测;读者只有汇总日志和抓取,这点误差看不出来。
    inline Snapshot Read()
    {
        Snapshot snapshot;
        const Counters &counters = Global();
        for (std::size_t i = 0; i < kStageCount; ++i)
        {
            snapshot.stages[i].count = counters.stages[i].count.load(std::memory_order_relaxed);
            snapshot.stages[i].totalNs = counters.stages[i].totalNs.load(std::memory_order_relaxed);
            snapshot.stages[i].maxNs = counters.stages[i].maxNs.load(std::memory_order_relaxed);
        }
        snapshot.periodicTickOverBudget = counters.periodicTickOverBudget.load(std::memory_order_relaxed);
        return snapshot;
    }

    using WindowMax = std::array<uint64_t, kStageCount>;

    // 取走并清零各段的窗口最大值(纳秒)。只许 [SaveTiming] 汇总日志一个调用者:多一个读者,窗口就被拆散了。
    inline WindowMax TakeWindowMax()
    {
        WindowMax window{};
        Counters &counters = Global();
        for (std::size_t i = 0; i < kStageCount; ++i)
        {
            window[i] = counters.stages[i].windowMaxNs.exchange(0, std::memory_order_relaxed);
        }
        return window;
    }
} // namespace save_timing_stats

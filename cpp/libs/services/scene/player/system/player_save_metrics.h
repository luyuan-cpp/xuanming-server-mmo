#pragma once

#include <string>

#include "player/system/save_timing_stats.h"

// 玩家存盘统计的两个出口:30 秒汇总日志里的 [SaveTiming] 一行,和 /metrics 的采集回调。
// 计数本身在 save_timing_stats.h(各段耗时)和 dirty_save_stats.h(尝试、无变化跳过、拒存),这里只读。
namespace player_save_metrics
{
    // [SaveTiming] 汇总行:各段在 last → now 这个窗口里的次数、平均耗时、最大耗时(微秒,一位小数),
    // 以及周期回调超过预算的次数。窗口里一次观测都没有时返回空串,调用方不打。
    // key 的名字和顺序是 tools/scripts/stress_summarize.ps1 的解析契约:新 key 只追加在末尾。
    std::string FormatSaveTimingLine(const save_timing_stats::Snapshot &now, const save_timing_stats::Snapshot &last,
                                     const save_timing_stats::WindowMax &windowMaxNs);
} // namespace player_save_metrics

// 把存盘统计挂到 Prometheus 出口(设了 MMORPG_METRICS_PORT 才有出口,见 metrics_http_exporter.h)。
// 重复登记被注册表按名字拒绝,只打一行 ERROR。
void RegisterPlayerSaveMetrics();

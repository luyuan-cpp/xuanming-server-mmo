#include "aoi_load.h"

#include <algorithm>
#include <atomic>
#include <cmath>
#include <cstdint>

#include "engine/core/metrics/metrics_registry.h"
#include "muduo/base/Logging.h"
#include "spatial/constants/aoi_priority.h"

namespace
{
// /metrics 的读数:模拟线程写,出口线程读,relaxed 足够(各自独立的读数,差一次可以接受)。
std::atomic<uint64_t> gStepUtilizationPercent{0};
std::atomic<uint64_t> gPressurePercent{0};
std::atomic<uint64_t> gTrimmedEntries{0};

void CollectAoiLoad(prometheus_text::Writer& writer)
{
    writer.BeginFamily("scene_aoi_load_step_utilization_percent",
                       "Smoothed simulation step time as a percentage of the fixed step on this scene node.",
                       prometheus_text::MetricType::kGauge);
    writer.Sample({}, gStepUtilizationPercent.load(std::memory_order_relaxed));
    writer.BeginFamily("scene_aoi_load_pressure_percent",
                       "AOI pressure applied to interest capacity: 0 means none, 100 means capacity at its floor.",
                       prometheus_text::MetricType::kGauge);
    writer.Sample({}, gPressurePercent.load(std::memory_order_relaxed));
    writer.BeginFamily("scene_aoi_load_trimmed_entries_total",
                       "Interest entries trimmed since process start because AOI capacity shrank under pressure.",
                       prometheus_text::MetricType::kCounter);
    writer.Sample({}, gTrimmedEntries.load(std::memory_order_relaxed));
}
} // namespace

bool AoiLoadMonitor::Observe(double stepSeconds, double fixedDeltaSeconds)
{
    if (!(fixedDeltaSeconds > 0) || !std::isfinite(stepSeconds))
    {
        return false;
    }
    const double utilization = std::max(0.0, stepSeconds / fixedDeltaSeconds);
    smoothedUtilization_ += kAoiLoadSmoothing * (utilization - smoothedUtilization_);
    gStepUtilizationPercent.store(static_cast<uint64_t>(std::lround(smoothedUtilization_ * 100)),
                                  std::memory_order_relaxed);

    // 步长累加有浮点误差(0.05 加 20 次不一定正好是 1.0),留一点余量,保证每 1 秒的第 20 步决定。
    constexpr double kTimeEpsilonSeconds = 1e-9;
    secondsSinceDecision_ += fixedDeltaSeconds;
    if (secondsSinceDecision_ + kTimeEpsilonSeconds < kAoiLoadDecisionIntervalSeconds)
    {
        return false;
    }
    secondsSinceDecision_ = 0;

    const double raw = std::clamp((smoothedUtilization_ - kAoiPressureLowUtilization) /
                                      (kAoiPressureHighUtilization - kAoiPressureLowUtilization),
                                  0.0, 1.0);
    const double scaled = raw * kAoiPressureLevels;
    // 防抖:偏离当前档位超过 kAoiPressureHysteresisLevels 档才换,边界附近的小抖动不会让容量来回变。
    if (std::abs(scaled - level_) > kAoiPressureHysteresisLevels)
    {
        const int previous = level_;
        level_ = static_cast<int>(std::lround(scaled));
        gPressurePercent.store(static_cast<uint64_t>(level_) * 100 / kAoiPressureLevels, std::memory_order_relaxed);
        LOG_INFO << "AOI pressure level changed: " << previous << " -> " << level_ << " of " << kAoiPressureLevels
                 << ", smoothed step utilization " << smoothedUtilization_;
    }
    return true;
}

double AoiLoadMonitor::PressureFactor() const
{
    return static_cast<double>(level_) / kAoiPressureLevels;
}

void RegisterAoiLoadMetrics()
{
    if (!GlobalMetricsRegistry().Register("scene_aoi_load", &CollectAoiLoad))
    {
        LOG_ERROR << "[Metrics] collector scene_aoi_load is already registered";
    }
}

void aoi_load_stats::RecordTrimmedEntries(std::size_t count)
{
    if (count > 0)
    {
        gTrimmedEntries.fetch_add(count, std::memory_order_relaxed);
    }
}

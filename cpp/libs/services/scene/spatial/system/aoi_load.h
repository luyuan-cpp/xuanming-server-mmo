#pragma once

#include <cstddef>

// 服务器压力监控(AOI 负载调节)。
//
// 每个模拟步报一次这一步的耗时:利用率 = 耗时 / 步长,按指数平均平滑后映射成压力因子 [0, 1],
// 量化成 kAoiPressureLevels 档,档位边界附近留防抖(常量见 spatial/constants/aoi_priority.h)。
// 压力由 AoiSystem::ApplyServerPressure 写进各场景的 ScenePressureComp,InterestSystem 据此把关注容量
// 从 kAoiListCapacityMax 线性调到 kAoiListCapacityMin:线程忙不过来时每个观察者少显示、少同步几个,
// 先保住模拟步不超时;超出的条目按权重裁掉,压力回落后补回。
//
// 只看模拟步耗时(MSPT 那一类信号),事件循环里处理 RPC 的耗时不在内;要不要并入,等压测数据再定。
// 不碰 ECS:World::Update 为每个场景线程持有一份,测试直接构造。读数顺带写进 /metrics 的 relaxed 计数。
class AoiLoadMonitor
{
public:
    // 报一个模拟步的耗时与步长(秒)。返回 true 表示到了做决定的时候(每 kAoiLoadDecisionIntervalSeconds 一次),
    // 调用方要把 PressureFactor() 写进各场景;档位没变时写入是无操作。步长不为正或耗时不是有限数时忽略。
    bool Observe(double stepSeconds, double fixedDeltaSeconds);

    // 当前压力因子,0、1 / kAoiPressureLevels、…、1 之一;0 表示不受压。
    [[nodiscard]] double PressureFactor() const;

    // 平滑后的利用率(耗时 / 步长),给日志与指标用。
    [[nodiscard]] double SmoothedUtilization() const { return smoothedUtilization_; }

private:
    double smoothedUtilization_ = 0;
    double secondsSinceDecision_ = 0;
    int level_ = 0;
};

// 把压力监控的读数挂到 /metrics(scene_aoi_load_*)。scene 节点启动时调一次。
void RegisterAoiLoadMetrics();

namespace aoi_load_stats
{
// 记关注表因容量收缩被裁掉的条目数(AoiSystem::ApplyServerPressure 调)。
void RecordTrimmedEntries(std::size_t count);
} // namespace aoi_load_stats

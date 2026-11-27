#pragma once

#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <string_view>

#include <muduo/net/TimerId.h>

namespace muduo
{
    namespace net
    {
        class EventLoop;
    }
}

namespace prometheus_text
{
    class Writer;
}

struct RedisClusterStats;

// 两个集群句柄(tlsPlayerRedis / tlsControlRedis)路由层的累计计数,两个出口读同一份、key 序列同一份(redis.cpp 的
// VisitRedisClusterStats):
//   - 每 30s 一行 [PlayerRedis] / [ControlRedis],有变化才打;运维手册(docs/ops/redis-player-cluster-runbook.md)按原文判读,
//     prefix 之后的 key=value 写法不要改;
//   - Prometheus(2026-11-27 起):scene_redis_cluster_events_total{client="player"|"control",event=<同一组 key>},
//     每个 client × event 进程一启动就以 0 导出。两个句柄只在 scene 上建连,所以只有 scene 登记。
// 设计见 docs/design/redis-player-store-cluster.md §5.6「可观测」。
std::string FormatRedisClusterStats(std::string_view prefix, const RedisClusterStats &stats);
void WriteRedisClusterMetrics(prometheus_text::Writer &writer, const RedisClusterStats &player, const RedisClusterStats &control);
// 登记进 GlobalMetricsRegistry(采集器名 scene_redis_cluster),采集时在 scene 逻辑线程上读两个句柄的 stats()。
// scene 启动时调一次;重复调用不会重复登记,只打一行 ERROR。
void RegisterRedisClusterMetrics();

class RedisSystem
{
public:
    RedisSystem() = default;
    ~RedisSystem();

    RedisSystem(const RedisSystem&) = delete;
    RedisSystem& operator=(const RedisSystem&) = delete;
    RedisSystem(RedisSystem &&) = delete;
    RedisSystem &operator=(RedisSystem &&) = delete;

    void Initialize(muduo::net::EventLoop *loop);

    // 进入停机 drain:只停止会制造新存盘的周期任务,保留 Redis 重连与重试定时器,
    // 让交接标记的撤回与改派核实仍能走完。
    void BeginShutdown();

    // 取消全部定时器。幂等;必须在 EventLoop 析构前调用。
    void Shutdown();

private:
    // Reference-wrapper instead of raw EventLoop* to satisfy the project's
    // no-raw-pointer-member lint (cpp/plugin). Set by Initialize(), unset means
    // Initialize() never ran, so Shutdown() is a no-op.
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop;
    muduo::net::TimerId retryTimerId;
    muduo::net::TimerId snapshotTimerId;
    muduo::net::TimerId periodicSaveTimerId;
    bool retryTimerActive = false;
    bool snapshotTimerActive = false;
    bool periodicSaveTimerActive = false;
    bool shutdownBegun = false;
};


extern thread_local RedisSystem tlsRedisSystem;

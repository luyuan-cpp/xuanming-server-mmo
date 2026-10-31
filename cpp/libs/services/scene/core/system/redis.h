#pragma once

#include <functional>
#include <memory>
#include <optional>

#include <muduo/net/TimerId.h>

namespace muduo
{
    namespace net
    {
        class EventLoop;
    }
}

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
    std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop_;
    muduo::net::TimerId retryTimerId_;
    muduo::net::TimerId snapshotTimerId_;
    muduo::net::TimerId periodicSaveTimerId_;
    bool retryTimerActive_ = false;
    bool snapshotTimerActive_ = false;
    bool periodicSaveTimerActive_ = false;
    bool shutdownBegun_ = false;
};


extern thread_local RedisSystem tlsRedisSystem;

#pragma once

#include <functional>
#include <memory>
#include <optional>

#include "engine/infra/storage/redis_client/redis_client.h"
#include "proto/common/database/player_cache.pb.h"
#include <muduo/net/TimerId.h>

using PlayerDataRedis = std::unique_ptr<MessageAsyncClient<Guid, PlayerAllData>>;

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

    PlayerDataRedis& GetPlayerDataRedis() {
		return playerRedis;
	}

    void Initialize(muduo::net::EventLoop *loop);

    // 进入停机 drain:只停止会制造新存盘的周期任务,保留 Redis 重连与重试定时器,
    // 让已经发起的玩家退出存盘仍有机会收到 ACK。
    void BeginShutdown();

    // 取消全部定时器并释放 playerRedis。幂等;必须在 EventLoop 析构前调用。
    void Shutdown();

private:
	PlayerDataRedis playerRedis;
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

#include "redis.h"

#include <cstdlib>

#include "muduo/net/EventLoop.h"

#include "player/system/dirty_save_stats.h"
#include "player/system/player_lifecycle.h"
#include "thread_context/ecs_context.h"
#include "thread_context/redis_manager.h"

thread_local RedisSystem tlsRedisSystem;

using namespace muduo;
using namespace muduo::net;

void RedisSystem::Initialize(muduo::net::EventLoop* loop)
{
	shutdownBegun_ = false;
    loop_ = std::ref(*loop);
    playerRedis = std::make_unique<PlayerDataRedis::element_type>(tlsRedis.GetZoneRedis());
    playerRedis->SetLoadCallback(PlayerLifecycleSystem::HandlePlayerAsyncLoaded);
    playerRedis->SetLoadFailedCallback(PlayerLifecycleSystem::HandlePlayerAsyncLoadFailed);
    playerRedis->SetSaveCallback(PlayerLifecycleSystem::HandlePlayerAsyncSaved);
    playerRedis->SetSaveFailedCallback(PlayerLifecycleSystem::HandlePlayerAsyncSaveFailed);

    tlsRedis.SetReconnectCallback([this]()
                                  {
        LOG_INFO << "Redis reconnected, retrying pending player loads";
        if (playerRedis)
        {
            playerRedis->OnReconnected();
        } });

    // Periodically drain NIL-pending load retries and pending save retries
    // whose backoff has elapsed. MUST NOT touch in-flight loading_queue_ —
    // only pending queues — otherwise load_callback_ can fire twice per key.
    static constexpr double kRetryIntervalSec = 1.0;
    retryTimerId_ = loop->runEvery(kRetryIntervalSec, [this]()
                                   {
        if (playerRedis)
        {
            playerRedis->RetryDuePending();
        } });
    retryTimerActive_ = true;

    // Periodically log a snapshot of internal queue sizes so operators can spot
    // a Redis stall (rising pending_loads / pending_saves) without enabling
    // per-call DEBUG logs. No output when all queues are empty.
    static constexpr double kQueueSnapshotIntervalSec = 30.0;
    snapshotTimerId_ = loop->runEvery(kQueueSnapshotIntervalSec, [this]()
                                      {
        if (playerRedis)
        {
            playerRedis->LogQueueSnapshot("RedisSystem");
        }
        // Piggyback on the same 30s tick so a fresh timer isn't needed
        // just for two counters. Format is parsed by
        // tools/scripts/stress_summarize.ps1 — keep the prefix and the
        // key=value layout stable, or update the parser.
        const auto stats = dirty_save_stats::Read();
        if (stats.total > 0)
        {
            const uint32_t pctTenths = stats.SkipPctTenths();
            LOG_INFO << "[DirtySave] total=" << stats.total
                     << " skipped=" << stats.skipped
                     << " skip_pct=" << (pctTenths / 10) << "."
                     << (pctTenths % 10) << "%";
        } });
    snapshotTimerActive_ = true;

    // Periodically save all online players to Redis to bound the data-loss
    // window if the scene node crashes. Default 300s; set
    // SCENE_PLAYER_SAVE_INTERVAL_SECONDS=0 to disable. Each tick scans
    // tlsEcs.playerList and calls SavePlayerToRedis for every player.
    // Cost grows linearly with online count; the default interval is sized
    // to keep amortized Redis/Kafka load modest (e.g. 10k players over 300s
    // = ~33 saves/sec).
    int periodicSaveSec = 300;
    if (const char* env = std::getenv("SCENE_PLAYER_SAVE_INTERVAL_SECONDS"))
    {
        const int parsed = std::atoi(env);
        if (parsed >= 0)
        {
            periodicSaveSec = parsed;
        }
    }
    if (periodicSaveSec > 0)
    {
        const double interval = static_cast<double>(periodicSaveSec);
        periodicSaveTimerId_ = loop->runEvery(interval, []()
                                              {
            if (tlsEcs.playerList.empty())
            {
                return;
            }
            const std::size_t before = tlsEcs.playerList.size();
            for (const auto& [playerId, player] : tlsEcs.playerList)
            {
                if (!tlsEcs.actorRegistry.valid(player))
                {
                    continue;
                }
                PlayerLifecycleSystem::SavePlayerToRedis(player);
            }
            LOG_INFO << "[RedisSystem] Periodic save scanned " << before << " online players"; });
        periodicSaveTimerActive_ = true;
        LOG_INFO << "[RedisSystem] Periodic player save enabled, interval=" << periodicSaveSec << "s";
    }
    else
    {
        LOG_INFO << "[RedisSystem] Periodic player save disabled (SCENE_PLAYER_SAVE_INTERVAL_SECONDS=0)";
    }
}

void RedisSystem::BeginShutdown()
{
	if (shutdownBegun_)
	{
		return;
	}
	shutdownBegun_ = true;

	// 停掉会制造新写入的周期全量存盘。retry timer 与 RedisManager 的重连
	// 必须继续活到 barrier 结束,否则 Redis 短暂抖动会必然拖到超时。
	if (loop_.has_value() && periodicSaveTimerActive_)
	{
		loop_->get().cancel(periodicSaveTimerId_);
		periodicSaveTimerActive_ = false;
	}
}

void RedisSystem::Shutdown()
{
	BeginShutdown();

	// 先取消定时器与 RedisManager 回调,再释放 MessageAsyncClient,避免后续
	// 重连回调命中已经销毁的 this。
    if (loop_.has_value())
    {
        muduo::net::EventLoop &loop = loop_->get();
        if (retryTimerActive_)
        {
            loop.cancel(retryTimerId_);
            retryTimerActive_ = false;
        }
        if (snapshotTimerActive_)
        {
            loop.cancel(snapshotTimerId_);
            snapshotTimerActive_ = false;
        }
    }
	tlsRedis.SetReconnectCallback({});
    playerRedis.reset();
	loop_.reset();
}

RedisSystem::~RedisSystem()
{
    Shutdown();
}

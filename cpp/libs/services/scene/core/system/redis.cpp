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
    // SCENE_PLAYER_SAVE_INTERVAL_SECONDS=0 to disable.
    //
    // 分摊而不是一口气扫完:定时器每秒跑一次,只处理
    // playerId % interval == 当前槽位 的玩家 —— 每个玩家每个周期仍然被存
    // 恰好一次,但单次回调的工作量是 N/interval 而不是 N。
    // 旧实现每 interval 秒在**单个回调里**全量遍历 playerList 逐个
    // SavePlayerToRedis(每次都是整份 PlayerAllData 的 marshal + 脏比较),
    // 回调跑在游戏 tick 同一个 EventLoop 线程上:几千在线就是每 300 秒一次
    // 数百毫秒级的全服停顿,World::Update 的固定步长累加器 clamp 在 1s,
    // 超过即直接丢模拟时间。注释里 "10k players over 300s = ~33 saves/sec"
    // 的摊销口径,现在才真的成立。
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
        const auto slotCount = static_cast<uint64_t>(periodicSaveSec);
        periodicSaveTimerId_ = loop->runEvery(1.0, [slotCount, slot = uint64_t{0}]() mutable
                                              {
            const uint64_t currentSlot = slot;
            slot = (slot + 1) % slotCount;

            if (tlsEcs.playerList.empty())
            {
                return;
            }
            std::size_t saved = 0;
            for (const auto& [playerId, player] : tlsEcs.playerList)
            {
                if (playerId % slotCount != currentSlot)
                {
                    continue;
                }
                if (!tlsEcs.actorRegistry.valid(player))
                {
                    continue;
                }
                PlayerLifecycleSystem::SavePlayerToRedis(player);
                ++saved;
            }
            // 只在整个周期的最后一个槽位打一条汇总,避免每秒刷日志。
            if (currentSlot == slotCount - 1 && saved > 0)
            {
                LOG_INFO << "[RedisSystem] Periodic save slot " << currentSlot
                         << " saved " << saved << " players (online=" << tlsEcs.playerList.size() << ")";
            } });
        periodicSaveTimerActive_ = true;
        LOG_INFO << "[RedisSystem] Periodic player save enabled, interval=" << periodicSaveSec
                 << "s (sharded per-second by playerId)";
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

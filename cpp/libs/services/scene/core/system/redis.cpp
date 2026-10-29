#include "redis.h"

#include "muduo/net/EventLoop.h"

#include "player/system/db_task_dispatch_system.h"
#include "player/system/dirty_save_stats.h"
#include "player/system/player_lifecycle.h"
#include "player/system/release_fence.h" // [ReleaseFence] 汇总行
#include "player/system/save_seq.h"      // [SaveSeq] 汇总行
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
    // owner_epoch CAS 拒绝(Lua 比对 player:{id}:owner_epoch 不等)与"存盘失败"是两种语义:
    // 失败要重试、拒绝是终态(本节点已被废黜,再试也是拒)。分开接线,别让废黜走进重试队列。
    playerRedis->SetSaveRejectedCallback(PlayerLifecycleSystem::HandlePlayerSaveRejected);
    // 严格屏障覆盖世代校验 C3(混版本持久字段保护 §5.2)的两种结局:warn 照写只计数;enforce 拒写按实体状态分派。
    // 都不是"已被废黜",不能接到上面那条回调上。
    playerRedis->SetSaveSchemaWarnedCallback(PlayerLifecycleSystem::HandlePlayerSaveSchemaWarned);
    playerRedis->SetSaveSchemaRejectedCallback(PlayerLifecycleSystem::HandlePlayerSaveSchemaRejected);
    // DBTask 投递记账:登记本线程 KafkaProducer 的逐条回执监听器,DBTask 落定后交给生命周期(下线收尾的回执闸门)。
    // 必须早于任何存盘(存盘只可能发生在玩家进场之后)。函数指针,不绑任何对象。
    DbTaskDispatchSystem::Initialize(&PlayerLifecycleSystem::OnDbTaskDispatchSettled);

    tlsRedis.SetReconnectCallback([this]()
                                  {
        LOG_INFO << "Redis reconnected, retrying pending player loads";
        // 先补发没确认的 handoff 标记撤回,再重发玩家加载。撤回与玩家加载走同一条连接
        // (playerRedis 持有的就是 tlsRedis.GetZoneRedis() 这个 HiredisPtr 的引用),Redis 按序执行:
        // 撤回只要被执行,就一定排在随后重发的加载之前。但这只是顺序上的尽力 —— 撤回可能被 Redis
        // 以 ERROR 拒掉(只读副本 / 正在加载数据集)而加载照样成功,所以正确性**不**靠这条顺序,
        // 靠的是 PlayerLifecycleSystem::IsSceneChangeBusy:撤回没确认之前**本节点**不替该玩家发 EnterScene。
        // (这道闸管不到不经本节点的跨节点落点,例如玩家断线后重登落到别的节点;那条路在撤回确认前
        // 仍只有标记 TTL 兜底,见 handoff_mark_withdraw.h 的契约说明。)
        PlayerLifecycleSystem::RetryPendingHandoffWithdrawals(/*reconnected=*/true);
        // 断线期间失败、等重发的 A2′(载入前"先核归属再删"继承来的 handoff 标记)也立刻补发,排在重发的
        // 玩家加载之前(同一条连接 FIFO)。闸门只认 A2′ 的应答,不靠这条顺序(见 exit_release_mark.h)。
        PlayerLifecycleSystem::RetryInheritedMarkClears(/*reconnected=*/true);
        // 疏散 / 排空改派的待确认表(relocate_confirm.h):断线期间没读到、在等重试的核实立刻重发(不绕过 settle 等待),
        // 排在重发的玩家加载之前。核实读与随后的凭证补写走这条不重放的连接,是为了补写不被推迟到任意时刻(R7);
        // 正确性**不**靠这条 FIFO(2026-10-23,r2-lc#2):补写在 Lua 里原子复核 owner_epoch 与条目缓存的原 H
        // (save_barrier.handoff_fence),之后的登记已换掉原件时返回 -2、不写。表空时直接返回。
        PlayerLifecycleSystem::SweepRelocateConfirms(relocate_confirm::Clock::now(), /*reconnected=*/true);
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
        // A2′ 的有上限重发与截止拒绝(单调时钟,见 exit_release_mark.h)搭这个 1s 节拍,排在加载 / 存盘重试之前。
        // 待入场表为空时直接返回。静态函数,不引入新的绑定。
        PlayerLifecycleSystem::RetryInheritedMarkClears(/*reconnected=*/false);
        if (playerRedis)
        {
            playerRedis->RetryDuePending();
        }
        // 没确认的 handoff 标记撤回也搭这个 1s 节拍重试(表为空时直接返回;条目自带 5s 重试间隔
        // 与"标记 TTL + 余量"的截止时刻,见 handoff_mark_withdraw.h)。静态函数,不引入新的绑定。
        PlayerLifecycleSystem::RetryPendingHandoffWithdrawals(/*reconnected=*/false);
        // 改派待确认表的各阶段截止、核实的首发 / 重发、落地条目的检查,与撤回表、A2′ 用同一个 1s 节拍(单调时钟的
        // 时刻在这里取一次、显式传入)。表空时直接返回。静态函数,不引入新的绑定。
        // BeginShutdown 只取消周期存盘,本定时器在停机 drain 期间仍在跑:停机谓词等待确认表清空靠的就是它。
        PlayerLifecycleSystem::SweepRelocateConfirms(relocate_confirm::Clock::now(), /*reconnected=*/false); });
    retryTimerActive_ = true;

    // DBTask 投递回执的驱动点(DbTaskDispatchSystem::Pump):poll() 派发回执 → 330s 票据超时兜底 → 把落定的玩家交给
    // PlayerLifecycleSystem::OnDbTaskDispatchSettled。回执只在本线程的 poll / flush / send 里派发,空闲节点上没有 send()
    // 来驱动,下线收尾会一直等;50ms 让收尾正常只多 ≤ 50ms。BeginShutdown **不**取消它:停机 drain 期间被保留 / 等回执的
    // 退出实体要靠它推完(停机谓词按 IsSaveInFlight 等这些实体)。取消在 StopDbTaskDispatch(Node 的拆除前钩子,
    // producer 最后一次 flush 之后)。lambda 无捕获,不绑 this。
    static constexpr double kDbTaskDispatchPumpIntervalSec = 0.05;
    dbTaskPumpTimerId_ = loop->runEvery(kDbTaskDispatchPumpIntervalSec, []
                                        { DbTaskDispatchSystem::Pump(); });
    dbTaskPumpTimerActive_ = true;

    // Periodically log a snapshot of internal queue sizes so operators can spot
    // a Redis stall (rising pending_loads / pending_saves) without enabling
    // per-call DEBUG logs. No output when all queues are empty.
    static constexpr double kQueueSnapshotIntervalSec = 30.0;
    // lastEnterFence:上一次打出 [EnterFence] 时的计数(只在本定时器里读写,逻辑线程上;初值全 0 = 第一次有计数就打)。
    // lastDbTaskDispatch:上一次打出 [DbTaskDispatch] 时的计数(同上,只在本定时器里读写)。
    // lastReleaseFence:上一次打出 [ReleaseFence] 时的计数(同上,只在本定时器里读写)。
    snapshotTimerId_ = loop->runEvery(kQueueSnapshotIntervalSec, [this, lastEnterFence = enter_fence_stats::Snapshot{},
                                                                  lastDbTaskDispatch = db_task_dispatch_stats::Snapshot{},
                                                                  lastReleaseFence = release_fence_stats::Snapshot{}]() mutable
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
        }
        // 拒存与相等跳过分开汇总,保持 [DirtySave] 的既有解析格式和跳过率分母不变。
        // 只统计缺有效 Guid/持久组件造成的完整消息组装失败,累计值不带玩家维度。
        if (stats.marshalRefused > 0)
        {
            LOG_WARN << "[PlayerSave] save_marshal_refused=" << stats.marshalRefused;
        }
        // 归属健康计数单独一行、单独前缀,不动 [DirtySave] 的解析格式。
        // 各项在链路升级完成后都应恒 0;任一非 0 都是要查的信号(含义见 owner_epoch_stats)。
        // owner_counter_behind(Redis 回退丢了铸造,zone-redis-regression-guard-20261022.md §4.4)追加在最后,不动前几项的顺序。
        const auto ownerStats = owner_epoch_stats::Read();
        if (ownerStats.Any())
        {
            // 字段名与顺序取 owner_epoch_stats::kFields(与 Prometheus 导出同一份),输出与逐项拼写时逐字相同。
            std::string line = "[OwnerEpoch]";
            for (const auto &field : owner_epoch_stats::kFields)
            {
                line += ' ';
                line += field.key;
                line += '=';
                line += std::to_string(ownerStats.*field.value);
            }
            LOG_WARN << line;
        }
        // 混版本持久字段保护 C3 的结局计数(persist_schema_stats),单独一行、单独前缀,不动上面几行的解析格式。
        // 两项正常都应恒 0;非 0 时逐次的 metric=persist_schema_regression 日志里有玩家号与 key。
        const auto schemaStats = persist_schema_stats::Read();
        if (schemaStats.Any())
        {
            LOG_WARN << "[PersistSchema] regression_warned=" << schemaStats.regressionWarned
                     << " regression_rejected=" << schemaStats.regressionRejected;
        }
        // 存盘序号发号器的异常计数(save_seq.h 的 save_seq_stats;db-write-fencing-mysql.md §7.2)。单独一行、单独前缀,
        // 不动上面几行的解析格式。key=value 平铺、进程启动以来的累计值;三项正常都应恒 0,任一非 0 才打,级别 WARN
        // (有坏数据或 scene 主机时钟超前,含义见 save_seq_stats)。玩家号不进这一行。
        const auto saveSeqStats = save_seq_stats::Read();
        if (saveSeqStats.Any())
        {
            std::string line = "[SaveSeq]";
            for (std::size_t i = 0; i < saveSeqStats.values.size(); ++i)
            {
                line += ' ';
                line += save_seq_stats::kCounterNames[i];
                line += '=';
                line += std::to_string(saveSeqStats.values[i]);
            }
            LOG_WARN << line;
        }
        // 入场会话栅栏的结论计数(enter_fence.h 的 enter_fence_stats;scene-enter-session-fence.md §5.7)。单独一行、单独前缀,
        // 不动上面两行的解析格式。key=value 平铺、进程启动以来的累计值,有变化才打;这一拍里拒绝类计数有增长时升 WARN
        // (挡下了跨会话的陈旧入场,或混版窗口里的无代次顶号),否则 INFO(admit_legacy 在 login 全量铸造后应恒为 0)。
        const auto fenceStats = enter_fence_stats::Read();
        if (fenceStats != lastEnterFence)
        {
            std::string line = "[EnterFence]";
            for (std::size_t i = 0; i < fenceStats.verdicts.size(); ++i)
            {
                line += ' ';
                line += enter_fence::kVerdictNames[i];
                line += '=';
                line += std::to_string(fenceStats.verdicts[i]);
            }
            line += " bind_rejected=" + std::to_string(fenceStats.bindRejected);
            line += " session_update_rejected=" + std::to_string(fenceStats.sessionUpdateRejected);
            if (fenceStats.Rejected() > lastEnterFence.Rejected())
            {
                LOG_WARN << line;
            }
            else
            {
                LOG_INFO << line;
            }
            lastEnterFence = fenceStats;
        }
        // ReleasePlayer 归属栅栏的结论计数(release_fence.h 的 release_fence_stats;scene-enter-chain-residuals.md §4.4)。
        // 单独一行、单独前缀,不动上面几行的解析格式。key=value 平铺、进程启动以来的累计值,有变化才打;这一拍里
        // skip_stale_release / skip_ahead_of_holder / release_cache_unknown 有增长时升 WARN(release_fence::Alarming),否则 INFO。
        // release_unversioned 在 scene_manager 全量升级后应恒为 0;not_found 是成功交接后的常态。
        const auto releaseFenceStats = release_fence_stats::Read();
        if (releaseFenceStats != lastReleaseFence)
        {
            std::string line = "[ReleaseFence]";
            for (std::size_t i = 0; i < releaseFenceStats.verdicts.size(); ++i)
            {
                line += ' ';
                line += release_fence::kVerdictNames[i];
                line += '=';
                line += std::to_string(releaseFenceStats.verdicts[i]);
            }
            line += " not_found=" + std::to_string(releaseFenceStats.notFound);
            // 会话结束释放(docs/design/location-release-binding.md §3.3):总数、凭证判定分布与条件写结果,追加在原有 key 之后。
            line += " session_ended=" + std::to_string(releaseFenceStats.sessionEnded);
            for (std::size_t i = 0; i < releaseFenceStats.credentialDecisions.size(); ++i)
            {
                line += " credential_";
                line += release_fence::kSessionEndedCredentialNames[i];
                line += '=';
                line += std::to_string(releaseFenceStats.credentialDecisions[i]);
            }
            line += " credential_written=" + std::to_string(releaseFenceStats.credentialWritten);
            line += " credential_epoch_moved=" + std::to_string(releaseFenceStats.credentialEpochMoved);
            line += " credential_failed=" + std::to_string(releaseFenceStats.credentialFailed);
            // 会话结束释放的登录身份判定(location-release-binding.md §13),追加在最后。binding_skip_newer_login /
            // binding_skip_conflict 有增长时升 WARN(release_fence::BindingAlarming)。
            for (std::size_t i = 0; i < releaseFenceStats.bindingDecisions.size(); ++i)
            {
                line += " binding_";
                line += release_fence::kSessionEndedBindingNames[i];
                line += '=';
                line += std::to_string(releaseFenceStats.bindingDecisions[i]);
            }
            // 2026-10-23 追加在行尾(原有 key 的位置不变):凭证 CAS 返回 -2 的次数,与判 write 之后读资格原件的核对分布
            // (location-release-binding.md §3.3)。no_qualification / malformed 有增长时升 WARN(release_fence::QualificationAlarming)。
            line += " credential_superseded=" + std::to_string(releaseFenceStats.credentialSuperseded);
            for (std::size_t i = 0; i < releaseFenceStats.credentialQualifications.size(); ++i)
            {
                line += " credential_qualification_";
                line += release_fence::kCredentialQualificationNames[i];
                line += '=';
                line += std::to_string(releaseFenceStats.credentialQualifications[i]);
            }
            if (releaseFenceStats.AlarmingIncreaseSince(lastReleaseFence))
            {
                LOG_WARN << line;
            }
            else
            {
                LOG_INFO << line;
            }
            lastReleaseFence = releaseFenceStats;
        }
        // DBTask 投递记账(db_task_dispatch_system.h 的 db_task_dispatch_stats)。单独一行、单独前缀,不动上面几行的
        // 解析格式。key=value 平铺、进程启动以来的累计值;计数有变化或还有未落定玩家时才打。这一拍里出现「MySQL 可能
        // 落后」的增长(AlarmingIncreaseSince)升 WARN,否则 INFO。玩家号不进这一行。
        const auto dispatchStats = db_task_dispatch_stats::Read();
        const std::size_t unsettledPlayers = DbTaskDispatchSystem::UnsettledPlayerCount();
        if (dispatchStats != lastDbTaskDispatch || unsettledPlayers > 0)
        {
            std::string line = "[DbTaskDispatch]";
            for (std::size_t i = 0; i < dispatchStats.values.size(); ++i)
            {
                line += ' ';
                line += db_task_dispatch_stats::kCounterNames[i];
                line += '=';
                line += std::to_string(dispatchStats.values[i]);
            }
            line += " unsettled_players=" + std::to_string(unsettledPlayers);
            line += " in_flight_tickets=" + std::to_string(DbTaskDispatchSystem::InFlightTickets());
            if (dispatchStats.AlarmingIncreaseSince(lastDbTaskDispatch))
            {
                LOG_WARN << line;
            }
            else
            {
                LOG_INFO << line;
            }
            lastDbTaskDispatch = dispatchStats;
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

void RedisSystem::StopDbTaskDispatch()
{
	// loop_ 为空 = Initialize 没跑过(监听器也没登记)或已 Shutdown(已收过账):什么都不做。这也保证析构里的第二次
	// Shutdown 不会在 thread_local 析构期间再碰 KafkaProducer。
	if (!loop_.has_value())
	{
		return;
	}
	// 先停 Pump 再注销监听器:之后不会再有 Pump 把落定的玩家交给下线收尾(Node 拆除前钩子之后的让轮里,Redis 已释放、
	// etcd 已注销,那时推收尾只会制造写失败与告警噪声)。未落定的玩家由 DbTaskDispatchSystem::Shutdown 打一行 ERROR 留证;
	// 它自身幂等,第二次调用不再打。
	if (dbTaskPumpTimerActive_)
	{
		loop_->get().cancel(dbTaskPumpTimerId_);
		dbTaskPumpTimerActive_ = false;
	}
	DbTaskDispatchSystem::Shutdown();
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
        // 正常停机时 Node 的拆除前钩子已经调过(那时日志还活着),这里是幂等兜底:钩子没挂上 / 没走到拆除时才真正生效。
        StopDbTaskDispatch();
    }
	tlsRedis.SetReconnectCallback({});
    playerRedis.reset();
	loop_.reset();
}

RedisSystem::~RedisSystem()
{
    Shutdown();
}

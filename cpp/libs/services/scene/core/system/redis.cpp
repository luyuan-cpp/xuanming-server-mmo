#include "redis.h"

#include <chrono>
#include <cstdlib>
#include <string>
#include <utility>

#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"

#include <node_config_manager.h>

#include "infra/storage/redis_cluster/redis_cluster_client.h"
#include "player/system/dirty_save_stats.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_store_client.h"
#include "player/system/release_fence.h" // [ReleaseFence] 汇总行
#include "player/system/save_seq.h"      // [SaveSeq] 汇总行
#include "thread_context/ecs_context.h"

thread_local RedisSystem tlsRedisSystem;

using namespace muduo;
using namespace muduo::net;

namespace
{
	// 部署配置 → 路由层参数(docs/design/redis-player-store-cluster.md §5.6)。配置错误写进 error 返回 false,调用方拒绝启动;
	// error 不含口令。主机名只在这里解析一次:路由层只认 IP 字面量,运行期不做阻塞 DNS。解析失败同样拒绝启动,
	// 进程重启会重新解析;带着连不上的种子起来,玩家域就永远不可用。
	bool BuildPlayerRedisOptions(const GameConfig::PlayerRedisConfig& config, RedisClusterOptions& options, std::string& error)
	{
		if (config.mode() == "standalone")
		{
			options.mode = RedisClusterMode::kStandalone;
		}
		else if (config.mode() == "cluster")
		{
			options.mode = RedisClusterMode::kCluster;
		}
		else
		{
			error = "player_redis.mode must be standalone or cluster, got '" + config.mode() + "'";
			return false;
		}
		if (config.seeds().empty() || (options.mode == RedisClusterMode::kStandalone && config.seeds().size() != 1))
		{
			error = "player_redis.seeds has " + std::to_string(config.seeds().size()) + " entries, mode " + config.mode() +
					(options.mode == RedisClusterMode::kStandalone ? " needs exactly one" : " needs at least one");
			return false;
		}
		for (const std::string& seed : config.seeds())
		{
			const std::optional<redis_cluster::HostPort> parts = redis_cluster::SplitHostPort(seed);
			if (!parts)
			{
				error = "player_redis seed '" + seed + "' is not host:port";
				return false;
			}
			std::string ip(parts->host);
			if (!redis_cluster::IsIpLiteral(ip))
			{
				muduo::net::InetAddress resolved(parts->port);
				if (!muduo::net::InetAddress::resolve(ip, &resolved))
				{
					error = "player_redis seed '" + seed + "' could not be resolved";
					return false;
				}
				ip = resolved.toIp();
			}
			options.seeds.push_back(redis_cluster::NodeEndpoint{std::move(ip), parts->port});
		}
		options.connect.password = config.password();
		// 玩家域只接受带 hash tag 的键(§5.1):控制域的键发到这个句柄当场拒绝,单实例开发时也一样。
		options.hashTagPolicy = redis_cluster::HashTagPolicy::kAllKeys;
		return true;
	}

	// 只打模式、种子和开关,不打口令。
	void LogPlayerRedisOptions(const RedisClusterOptions& options)
	{
		std::string seeds;
		for (const redis_cluster::NodeEndpoint& seed : options.seeds)
		{
			if (!seeds.empty())
			{
				seeds += ',';
			}
			seeds += seed.ToString();
		}
		LOG_INFO << "Player Redis: mode=" << (options.mode == RedisClusterMode::kCluster ? "cluster" : "standalone")
				 << " seeds=" << seeds << " auth=" << (options.connect.password.empty() ? "off" : "on");
	}

	std::string FormatPlayerRedisStats(const RedisClusterStats& stats)
	{
		return "[PlayerRedis] rejected_by_guard=" + std::to_string(stats.rejectedByGuard) +
			   " rejected_not_connected=" + std::to_string(stats.rejectedNotConnected) +
			   " rejected_queue_full=" + std::to_string(stats.rejectedQueueFull) +
			   " moved=" + std::to_string(stats.movedRedirects) +
			   " ask=" + std::to_string(stats.askRedirects) +
			   " backoff_retries=" + std::to_string(stats.backoffRetries) +
			   " budget_exhausted=" + std::to_string(stats.budgetExhausted) +
			   " null_replies=" + std::to_string(stats.nullReplies) +
			   " aborted=" + std::to_string(stats.abortedCommands) +
			   " topology_applied=" + std::to_string(stats.topologyApplied) +
			   " topology_refresh_failures=" + std::to_string(stats.topologyRefreshFailures);
	}
} // namespace

void RedisSystem::Initialize(muduo::net::EventLoop* loop)
{
	// 存盘的启动校验放在任何玩家进场之前:字段漏标分组会静默丢数据,密钥不对则每一笔存盘都被拒。
	std::string playerStoreError;
	if (!ValidatePlayerStoreStartup(playerStoreError))
	{
		LOG_FATAL << "[PlayerStore] refused to start: " << playerStoreError;
	}
	shutdownBegun_ = false;
    loop_ = std::ref(*loop);

	RedisClusterOptions playerRedisOptions;
	std::string playerRedisError;
	if (!BuildPlayerRedisOptions(tlsNodeConfigManager.GetGameConfig().player_redis(), playerRedisOptions, playerRedisError))
	{
		LOG_FATAL << "[PlayerRedis] refused to start: " << playerRedisError;
	}
	LogPlayerRedisOptions(playerRedisOptions);

	// 任一主节点完成握手(首次与重连)都会进来,这时表里的条目不一定落在这个节点上。补发是幂等的:撤回是条件写,
	// A2′ 与核实只重发没有在途的条目。主节点很多时一次切主或网络恢复会连着进来很多次,所以限成每秒一轮,
	// 没赶上的由 1s 定时器接着发,正确性不靠这里抢的这点时间。路由层回调前会拷一份函数对象,限速状态放在 shared_ptr 里才留得住。
    tlsPlayerRedis.SetReconnectCallback([lastSweep = std::make_shared<std::chrono::steady_clock::time_point>()](const redis_cluster::NodeEndpoint& node)
                                  {
        const auto now = std::chrono::steady_clock::now();
        if (now - *lastSweep < std::chrono::seconds(1))
        {
            return;
        }
        *lastSweep = now;
        LOG_INFO << "Player Redis node " << node.ToString() << " ready, retrying pending handoff marks";
        // 补发没确认的 handoff 标记撤回。撤回可能以空应答告终(只读副本 / 正在加载数据集,路由层预算耗尽),
        // 所以正确性**不**靠它,靠的是 PlayerLifecycleSystem::IsSceneChangeBusy:撤回没确认之前**本节点**不替该玩家发 EnterScene。
        // (这道闸管不到不经本节点的跨节点落点,例如玩家断线后重登落到别的节点;那条路在撤回确认前
        // 仍只有标记 TTL 兜底,见 handoff_mark_withdraw.h 的契约说明。)
        PlayerLifecycleSystem::RetryPendingHandoffWithdrawals(/*reconnected=*/true);
        // 断线期间失败、等重发的 A2′(载入前"先核归属再删"继承来的 handoff 标记)也立刻补发。闸门只认 A2′ 的应答(见 exit_release_mark.h)。
        PlayerLifecycleSystem::RetryInheritedMarkClears(/*reconnected=*/true);
        // 疏散 / 排空改派的待确认表(relocate_confirm.h):断线期间没读到、在等重试的核实立刻重发(不绕过 settle 等待)。
        // 核实读与随后的凭证补写走不重放的句柄,是为了补写不被推迟到任意时刻(R7);
        // 正确性**不**靠命令顺序(2026-10-23,r2-lc#2):补写在 Lua 里原子复核 owner_epoch 与条目缓存的原 H
        // (handoff_fence),之后的登记已换掉原件时返回 -2、不写。表空时直接返回。
        PlayerLifecycleSystem::SweepRelocateConfirms(relocate_confirm::Clock::now(), /*reconnected=*/true); });
    tlsPlayerRedis.Connect(loop, std::move(playerRedisOptions));

    // 协调用途的 Redis 写(继承标记清理、handoff 标记撤回、改派核实)的 1s 重试节拍。
    static constexpr double kRetryIntervalSec = 1.0;
    retryTimerId_ = loop->runEvery(kRetryIntervalSec, []()
                                   {
        // A2′ 的有上限重发与截止拒绝(单调时钟,见 exit_release_mark.h)搭这个 1s 节拍。
        // 待入场表为空时直接返回。静态函数,不引入新的绑定。
        PlayerLifecycleSystem::RetryInheritedMarkClears(/*reconnected=*/false);
        // 没确认的 handoff 标记撤回也搭这个 1s 节拍重试(表为空时直接返回;条目自带 5s 重试间隔
        // 与"标记 TTL + 余量"的截止时刻,见 handoff_mark_withdraw.h)。静态函数,不引入新的绑定。
        PlayerLifecycleSystem::RetryPendingHandoffWithdrawals(/*reconnected=*/false);
        // 改派待确认表的各阶段截止、核实的首发 / 重发、落地条目的检查,与撤回表、A2′ 用同一个 1s 节拍(单调时钟的
        // 时刻在这里取一次、显式传入)。表空时直接返回。静态函数,不引入新的绑定。
        // BeginShutdown 只取消周期存盘,本定时器在停机 drain 期间仍在跑:停机谓词等待确认表清空靠的就是它。
        PlayerLifecycleSystem::SweepRelocateConfirms(relocate_confirm::Clock::now(), /*reconnected=*/false); });
    retryTimerActive_ = true;

    // 每 30 秒打一轮计数快照。
    static constexpr double kQueueSnapshotIntervalSec = 30.0;
    // lastEnterFence:上一次打出 [EnterFence] 时的计数(只在本定时器里读写,逻辑线程上;初值全 0 = 第一次有计数就打)。
    // lastReleaseFence:上一次打出 [ReleaseFence] 时的计数(同上,只在本定时器里读写)。
    // lastPlayerRedis:上一次打出 [PlayerRedis] 时的计数(同上)。
    snapshotTimerId_ = loop->runEvery(kQueueSnapshotIntervalSec, [lastEnterFence = enter_fence_stats::Snapshot{},
                                                                  lastReleaseFence = release_fence_stats::Snapshot{},
                                                                  lastPlayerRedis = RedisClusterStats{}]() mutable
                                      {
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
        // 覆盖世代校验拒写的计数(persist_schema_stats),单独一行、单独前缀,不动上面几行的解析格式。
        // 正常应恒 0;非 0 时逐次的 metric=persist_schema_regression 日志里有玩家号与 key。
        const auto schemaStats = persist_schema_stats::Read();
        if (schemaStats.Any())
        {
            LOG_WARN << "[PersistSchema] regression_rejected=" << schemaStats.regressionRejected;
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
        // 玩家域路由层的累计计数(redis-player-store-cluster.md §8.1 观察项),有变化才打。单独一行、单独前缀,不动上面几行的解析格式。
        // 这一拍里拒绝类或预算耗尽有增长时升 WARN:这些命令没有执行,调用方走的是各自的失败路径。重定向是迁移时的常态,只打 INFO。
        const RedisClusterStats playerRedisStats = tlsPlayerRedis.stats();
        if (playerRedisStats != lastPlayerRedis)
        {
            const bool alarming = playerRedisStats.rejectedByGuard > lastPlayerRedis.rejectedByGuard ||
                                  playerRedisStats.rejectedQueueFull > lastPlayerRedis.rejectedQueueFull ||
                                  playerRedisStats.budgetExhausted > lastPlayerRedis.budgetExhausted;
            if (alarming)
            {
                LOG_WARN << FormatPlayerRedisStats(playerRedisStats);
            }
            else
            {
                LOG_INFO << FormatPlayerRedisStats(playerRedisStats);
            }
            lastPlayerRedis = playerRedisStats;
        } });
    snapshotTimerActive_ = true;

    // 周期存盘,限定 scene 崩溃时的丢失窗口。缺省 60 秒;
    // SCENE_PLAYER_SAVE_INTERVAL_SECONDS=0 关闭。
    //
    // 分摊而不是一口气扫完:定时器每秒跑一次,只处理
    // playerId % interval == 当前槽位 的玩家 —— 每个玩家每个周期仍然被存
    // 恰好一次,但单次回调的工作量是 N/interval 而不是 N。
    // 旧实现每 interval 秒在**单个回调里**全量遍历 playerList 逐个
    // SavePlayer(每次都是整份 PlayerAllData 的 marshal + 脏比较),
    // 回调跑在游戏 tick 同一个 EventLoop 线程上:几千在线就是每 300 秒一次
    // 数百毫秒级的全服停顿,World::Update 的固定步长累加器 clamp 在 1s,
    // 超过即直接丢模拟时间。注释里 "10k players over 300s = ~33 saves/sec"
    // 的摊销口径,现在才真的成立。
    // 只写有变化的行,没有变化不发请求,所以周期可以短(总体方案 §4,初值,压测后定)。环境变量优先。
    int periodicSaveSec = 60;
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
                PlayerLifecycleSystem::SavePlayer(player);
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

	// 先取消定时器,再清玩家域句柄的回调。只在 Initialize 跑过、还没 Shutdown 时碰句柄:析构走到这里时
	// tlsPlayerRedis 可能已先析构(它比 tlsRedisSystem 晚构造)。连接本身由 Node 的收尾与 scene 入口的兜底 Shutdown 释放。
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
        tlsPlayerRedis.SetReconnectCallback({});
    }
	loop_.reset();
}

RedisSystem::~RedisSystem()
{
    Shutdown();
}

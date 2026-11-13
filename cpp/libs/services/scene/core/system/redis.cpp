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
#include "player/system/player_save_metrics.h" // [SaveTiming] 汇总行
#include "player/system/player_store_client.h"
#include "player/system/release_fence.h" // [ReleaseFence] 汇总行
#include "player/system/save_seq.h"      // [SaveSeq] 汇总行
#include "player/system/save_timing_stats.h"
#include "engine/core/time/system/time.h" // 托管在场心跳按墙钟秒错开(RefreshHostedPresence)
#include "thread_context/ecs_context.h"
#include "thread_context/redis_manager.h"

thread_local RedisSystem tlsRedisSystem;

using namespace muduo;
using namespace muduo::net;

namespace
{
	// 部署配置的一段(player_redis / control_redis)→ 路由层的模式、种子与口令(docs/design/redis-player-store-cluster.md §5.6)。
	// 键策略不在这里定,由调用方按句柄所属的域设置。配置错误写进 error 返回 false,调用方拒绝启动;error 不含口令。
	// 主机名只在这里解析一次:路由层只认 IP 字面量,运行期不做阻塞 DNS。解析走 ResolveRedisAddress(失败按 1s 间隔重试 10 次,
	// 仍失败就致命退出,infra-reconnect-overview.md §4 R9):带着连不上的种子起来,这个句柄就永远不可用,进程重启会重新解析。
	bool BuildRedisClusterOptions(const char* section, const GameConfig::RedisClusterConfig& config, RedisClusterOptions& options,
								  std::string& error)
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
			error = std::string(section) + ".mode must be standalone or cluster, got '" + config.mode() + "'";
			return false;
		}
		if (config.seeds().empty() || (options.mode == RedisClusterMode::kStandalone && config.seeds().size() != 1))
		{
			error = std::string(section) + ".seeds has " + std::to_string(config.seeds().size()) + " entries, mode " + config.mode() +
					(options.mode == RedisClusterMode::kStandalone ? " needs exactly one" : " needs at least one");
			return false;
		}
		for (const std::string& seed : config.seeds())
		{
			const std::optional<redis_cluster::HostPort> parts = redis_cluster::SplitHostPort(seed);
			if (!parts)
			{
				error = std::string(section) + " seed '" + seed + "' is not host:port";
				return false;
			}
			std::string ip(parts->host);
			if (!redis_cluster::IsIpLiteral(ip))
			{
				ip = ResolveRedisAddress(ip, parts->port).toIp();
			}
			options.seeds.push_back(redis_cluster::NodeEndpoint{std::move(ip), parts->port});
		}
		options.connect.password = config.password();
		return true;
	}

	// 只打模式、种子和开关,不打口令。label 是日志里的句柄名("Player Redis" / "Control Redis")。
	void LogRedisClusterOptions(const char* label, const RedisClusterOptions& options)
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
		LOG_INFO << label << ": mode=" << (options.mode == RedisClusterMode::kCluster ? "cluster" : "standalone")
				 << " seeds=" << seeds << " auth=" << (options.connect.password.empty() ? "off" : "on");
	}

	// prefix 是行首标签([PlayerRedis] / [ControlRedis]),其后的 key=value 写法由 tools/scripts/stress_summarize.ps1 解析,改了要同步改脚本。
	std::string FormatRedisClusterStats(const char* prefix, const RedisClusterStats& stats)
	{
		return std::string(prefix) + " rejected_by_guard=" + std::to_string(stats.rejectedByGuard) +
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

	// 有变化才打一行,并把 last 更新成这次的计数。这一拍里拒绝类或预算耗尽有增长时升 WARN:这些命令没有执行,
	// 调用方走的是各自的失败路径。重定向是迁移时的常态,只打 INFO。
	void LogRedisClusterStatsIfChanged(const char* prefix, const RedisClusterStats& current, RedisClusterStats& last)
	{
		if (current == last)
		{
			return;
		}
		const bool alarming = current.rejectedByGuard > last.rejectedByGuard || current.rejectedQueueFull > last.rejectedQueueFull ||
							  current.budgetExhausted > last.budgetExhausted;
		if (alarming)
		{
			LOG_WARN << FormatRedisClusterStats(prefix, current);
		}
		else
		{
			LOG_INFO << FormatRedisClusterStats(prefix, current);
		}
		last = current;
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
	if (!BuildRedisClusterOptions("player_redis", tlsNodeConfigManager.GetGameConfig().player_redis(), playerRedisOptions,
								  playerRedisError))
	{
		LOG_FATAL << "[PlayerRedis] refused to start: " << playerRedisError;
	}
	// 玩家域只接受带 hash tag 的键(§5.1):控制域的键发到这个句柄当场拒绝,单实例开发时也一样。
	playerRedisOptions.hashTagPolicy = redis_cluster::HashTagPolicy::kAllKeys;
	// 控制集群的键也带 tag(全部 Redis 上 Cluster §3.1),只看「有没有 tag」拦不住它们,再按 tag 的形状拒绝。
	playerRedisOptions.keyDomain = redis_cluster::KeyDomain::kPlayer;
	LogRedisClusterOptions("Player Redis", playerRedisOptions);

	// 刻意不挂重连回调(docs/design/infra-reconnect-overview.md §4 R14;负责人 2026-11-05 定:重连成功不做业务逻辑)。
	// 这里原来在任一主节点握手完成时补发 handoff 撤回、A2′ 与改派核实,并且忽略各条目的重试间隔。补发本来就是幂等的,
	// 正确性靠的是下面的 1s 定时器、各条目自己的重试间隔与截止:
	//   - 撤回没确认之前本节点不替该玩家发 EnterScene(PlayerLifecycleSystem::IsSceneChangeBusy);
	//   - A2′ 的闸门只认 A2′ 的应答(exit_release_mark.h);
	//   - 改派核实的补写在 Lua 里原子复核 owner_epoch 与原 H(handoff_fence)。
	// 去掉回调的代价只是:Redis 恢复后,条目要等到各自的下一次重试时刻才重发,而不是立刻抢发。
	// 下面三个函数原来用来区分「重连回调 / 定时器」的 reconnected 参数(重连时忽略重试间隔)已于 2026-11-11 删掉,
	// 到期只看各条目自己的重试时刻。
    tlsPlayerRedis.Connect(loop, std::move(playerRedisOptions));

	// 控制集群(docs/design/redis-cluster-all-domains-20261110.md §4.2、§5.2):scene_manager 写的读模型(世界分线目录、神秘商人)、
	// 场景归属对账的 MGET、资产待办唤醒的 SPUBLISH。原来的两个单实例句柄(核心 zoneredis 上的唤醒、本 zone 组控制域实例上的
	// 读模型)合成这一个。Cluster 只有 0 号库,不需要库号。同样不挂重连回调:四处用法都是一次性的,丢了等下一拍或退回原退避(R14 口径)。
	{
		RedisClusterOptions controlRedisOptions;
		std::string controlRedisError;
		if (!BuildRedisClusterOptions("control_redis", tlsNodeConfigManager.GetGameConfig().control_redis(), controlRedisOptions,
									  controlRedisError))
		{
			LOG_FATAL << "[ControlRedis] refused to start: " << controlRedisError;
		}
		// 单键可以不带 tag;带 tag 只能是登记过的控制族;多键必须同一个 tag(碰巧同 slot 也拒绝)。玩家域的 tag 发过来当场拒绝。
		controlRedisOptions.hashTagPolicy = redis_cluster::HashTagPolicy::kMultiKeyOnly;
		controlRedisOptions.keyDomain = redis_cluster::KeyDomain::kControl;
		LogRedisClusterOptions("Control Redis", controlRedisOptions);
		tlsControlRedis.Connect(loop, std::move(controlRedisOptions));
	}

    // 协调用途的 Redis 写(继承标记清理、handoff 标记撤回、改派核实)的 1s 重试节拍。
    static constexpr double kRetryIntervalSec = 1.0;
    retryTimerId_ = loop->runEvery(kRetryIntervalSec, []()
                                   {
        // A2′ 的有上限重发与截止拒绝(单调时钟,见 exit_release_mark.h)搭这个 1s 节拍,时刻在这里取、显式传入。
        // 待入场表为空时直接返回。静态函数,不引入新的绑定。
        PlayerLifecycleSystem::RetryInheritedMarkClears(exit_release_mark::Clock::now());
        // 没确认的 handoff 标记撤回也搭这个 1s 节拍重试(表为空时直接返回;条目自带 5s 重试间隔,没有截止,
        // 重试到确认为止,见 handoff_mark_withdraw.h)。静态函数,不引入新的绑定。
        PlayerLifecycleSystem::RetryPendingHandoffWithdrawals();
        // 改派待确认表的各阶段截止、核实的首发 / 重发、落地条目的检查,与撤回表、A2′ 用同一个 1s 节拍(单调时钟的
        // 时刻在这里取一次、显式传入)。表空时直接返回。静态函数,不引入新的绑定。
        // BeginShutdown 只取消周期存盘,本定时器在停机 drain 期间仍在跑:停机谓词等待确认表清空靠的就是它。
        PlayerLifecycleSystem::SweepRelocateConfirms(relocate_confirm::Clock::now());
        // 托管落点的看护(目标节点没把人建起来时的有界重试,hosted_placement_retry.h)同一个节拍。表空时直接返回;
        // 节点疏散 / 计划排空 / 停机期间它自己不再读、不再重试。静态函数,不引入新的绑定。
        PlayerLifecycleSystem::SweepHostedPlacementRetries(hosted_placement_retry::Clock::now());
        // 离线托管角色的在场心跳与托管归位信息(hosted_presence.h,player-connection-lost-hold.md §4.5)同一个节拍:
        // 每名托管角色 20 秒一次,按玩家号错开。停机 drain 期间照常续:带着托管改派票据退出中、改派待确认的角色
        // 节点还在对他负责,不让 player_locator 在改派有结论之前就替他归位。静态函数,不引入新的绑定。
        PlayerLifecycleSystem::RefreshHostedPresence(TimeSystem::NowSecondsUTC()); });
    retryTimerActive_ = true;

    // 每 30 秒打一轮计数快照。
    static constexpr double kQueueSnapshotIntervalSec = 30.0;
    // lastEnterFence:上一次打出 [EnterFence] 时的计数(只在本定时器里读写,逻辑线程上;初值全 0 = 第一次有计数就打)。
    // lastReleaseFence:上一次打出 [ReleaseFence] 时的计数(同上,只在本定时器里读写)。
    // lastPlayerRedis / lastControlRedis:上一次打出 [PlayerRedis] / [ControlRedis] 时的计数(同上)。
    // lastSaveTiming:上一次打 [SaveTiming] 时的存盘分段耗时累计值(同上),这一行打的是两次之间的窗口值。
    snapshotTimerId_ = loop->runEvery(kQueueSnapshotIntervalSec, [lastEnterFence = enter_fence_stats::Snapshot{},
                                                                  lastReleaseFence = release_fence_stats::Snapshot{},
                                                                  lastPlayerRedis = RedisClusterStats{},
                                                                  lastControlRedis = RedisClusterStats{},
                                                                  lastSaveTiming = save_timing_stats::Snapshot{}]() mutable
                                      {
        // 格式由 tools/scripts/stress_summarize.ps1 解析:前缀和 key=value 的写法不要改,改了要同步改脚本。
        const auto stats = dirty_save_stats::Read();
        if (stats.total > 0)
        {
            const uint32_t pctTenths = stats.SkipPctTenths();
            // piggyback_deferred 追加在行尾:只有位置变了、定时存盘推迟的次数,计入 total、不计入 skipped。
            LOG_INFO << "[DirtySave] total=" << stats.total
                     << " skipped=" << stats.skipped
                     << " skip_pct=" << (pctTenths / 10) << "."
                     << (pctTenths % 10) << "%"
                     << " piggyback_deferred=" << stats.piggybackDeferred;
        }
        // 存盘分段耗时(save_timing_stats.h):这 30 秒里各段的次数、平均与最大耗时。窗口最大值只在这里取走清零。
        // 周期回调这一窗口里有超过预算的就升 WARN:那是 §4.6 说的「摊到多个 tick」的信号。没有存盘就不打。
        const auto saveTiming = save_timing_stats::Read();
        const auto saveTimingLine =
            player_save_metrics::FormatSaveTimingLine(saveTiming, lastSaveTiming, save_timing_stats::TakeWindowMax());
        if (!saveTimingLine.empty())
        {
            if (saveTiming.periodicTickOverBudget > lastSaveTiming.periodicTickOverBudget)
            {
                LOG_WARN << saveTimingLine;
            }
            else
            {
                LOG_INFO << saveTimingLine;
            }
        }
        lastSaveTiming = saveTiming;
        // 拒存与相等跳过分开汇总,保持 [DirtySave] 的既有解析格式和跳过率分母不变。
        // 只统计缺有效 Guid/持久组件造成的完整消息组装失败,累计值不带玩家维度。
        if (stats.marshalRefused > 0)
        {
            LOG_WARN << "[PlayerSave] save_marshal_refused=" << stats.marshalRefused;
        }
        // 归属健康计数单独一行、单独前缀,不动 [DirtySave] 的解析格式。
        // 各项在链路升级完成后都应恒 0;任一非 0 都是要查的信号(含义见 owner_epoch_stats)。
        // 存盘重构(玩家数据只存 TiDB)之后只剩两项,Redis 存档路径的 home_zone_unknown / owner_epoch_unknown / owner_counter_behind 已随路径删除。
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
        // 存盘最终被确定性拒绝的计数(player_store_refused_stats;player-save-direct-commit.md §4)。单独一行、单独前缀,
        // 不动上面几行的解析格式。key=value 平铺、进程启动以来的累计值;正常都应恒 0,任一非 0 才打,级别 WARN。
        // 与 Prometheus 的 scene_player_store_save_refused_total{result} 共用 kCounterNames;玩家号只在逐次的 metric=player_store_save_refused 日志里。
        const auto refusedStats = player_store_refused_stats::Read();
        if (refusedStats.Any())
        {
            std::string line = "[PlayerStoreRefused]";
            for (std::size_t i = 0; i < refusedStats.values.size(); ++i)
            {
                line += ' ';
                line += player_store_refused_stats::kCounterNames[i];
                line += '=';
                line += std::to_string(refusedStats.values[i]);
            }
            LOG_WARN << line;
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
            // 2026-11-12 追加在行尾:带 keep_held 的会话结束释放(托管归位撞门)遇到本节点持有、按约定不动的次数。
            line += " session_ended_kept_held=" + std::to_string(releaseFenceStats.sessionEndedKeptHeld);
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
        // 两个集群句柄路由层的累计计数(redis-player-store-cluster.md §8.1 观察项),有变化才打。各自单独一行、单独前缀,
        // 不动上面几行的解析格式。
        LogRedisClusterStatsIfChanged("[PlayerRedis]", tlsPlayerRedis.stats(), lastPlayerRedis);
        LogRedisClusterStatsIfChanged("[ControlRedis]", tlsControlRedis.stats(), lastControlRedis); });
    snapshotTimerActive_ = true;

    // 周期存盘,限定 scene 崩溃时的丢失窗口。缺省 60 秒;
    // SCENE_PLAYER_SAVE_INTERVAL_SECONDS=0 关闭。
    //
    // 分摊而不是一口气扫完:定时器每 kPeriodicSaveTickSec 跑一次,一个周期分成 interval / kPeriodicSaveTickSec 个槽,
    // 每次只处理 playerId % 槽数 == 当前槽 的玩家。每个玩家每个周期仍然恰好存一次,单次回调的工作量是
    // 在线人数 / 槽数(60 秒周期、1 万在线约 17 人)。回调和游戏 tick 在同一个 EventLoop 线程上,一次做太多就是一次卡顿;
    // 原来每秒一个槽,同样的人数一次要做约 167 人(player-data-tidb-scaling-plan.md §4.6「tick 尖刺」,2026-11-11 定案按
    // 更细的槽摊开,不等压测)。单次回调仍超过 save_timing_stats::kPeriodicTickBudget 时 [SaveTiming] 升 WARN。
    // 只写有变化的行,没有变化不发请求,所以周期可以短(总体方案 §4)。环境变量优先。
    // 走 SavePlayerPeriodic:只有位置变了就推迟这一笔,位置随下一笔有别的行要写的存盘、下线等最终存盘
    // 或兜底间隔一起写(player-save-direct-commit.md §1.1)。别的入口要的是落盘,不走这里。
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
        // 每秒 10 个槽。槽数随周期变,周期 1 秒时也有 10 个槽,不会出现 0。
        static constexpr uint64_t kPeriodicSaveTicksPerSecond = 10;
        static constexpr double kPeriodicSaveTickSec = 1.0 / static_cast<double>(kPeriodicSaveTicksPerSecond);
        const uint64_t slotCount = static_cast<uint64_t>(periodicSaveSec) * kPeriodicSaveTicksPerSecond;
        // cycleSaved:本周期已经发起的定时存盘次数,周期最后一个槽打完汇总后清零(只在本定时器里读写)。
        periodicSaveTimerId_ = loop->runEvery(kPeriodicSaveTickSec, [slotCount, slot = uint64_t{0},
                                                                     cycleSaved = std::size_t{0}]() mutable
                                              {
            const uint64_t currentSlot = slot;
            slot = (slot + 1) % slotCount;

            if (tlsEcs.playerList.empty())
            {
                cycleSaved = 0;
                return;
            }
            // 单次回调的总耗时进 periodic_tick 段,含遍历整张在线表;超过 kPeriodicTickBudget 另计一次。
            const auto tickStart = save_timing_stats::Clock::now();
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
                PlayerLifecycleSystem::SavePlayerPeriodic(player);
                ++saved;
            }
            save_timing_stats::ObserveSince(save_timing_stats::Stage::kPeriodicTick, tickStart);
            cycleSaved += saved;
            // 每个周期只在最后一个槽打一条汇总,避免刷日志。数的是发起次数,推迟和无变化的也算。
            if (currentSlot == slotCount - 1)
            {
                if (cycleSaved > 0)
                {
                    LOG_INFO << "[RedisSystem] Periodic save cycle attempted " << cycleSaved
                             << " saves (online=" << tlsEcs.playerList.size() << ")";
                }
                cycleSaved = 0;
            } });
        periodicSaveTimerActive_ = true;
        LOG_INFO << "[RedisSystem] Periodic player save enabled, interval=" << periodicSaveSec
                 << "s (" << slotCount << " slots by playerId, one slot every " << kPeriodicSaveTickSec << "s)";
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

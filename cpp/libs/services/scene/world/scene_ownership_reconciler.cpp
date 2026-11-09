#include "world/scene_ownership_reconciler.h"

#include <algorithm>
#include <chrono>
#include <cstddef>
#include <string>
#include <utility>

#include <hiredis/hiredis.h>

#include "muduo/base/Logging.h"

#include "engine/core/metrics/metrics_registry.h"
#include "network/node_utils.h"
#include "proto/common/base/node.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/redis_manager.h"
#include "world/scene_lifecycle.h"

namespace scene_ownership
{
namespace
{
// 核对状态号的发号器(进程内单调;0 保留给「未登记」)。与 tlsEcs 同属 scene 的 EventLoop 线程。
thread_local std::uint64_t tlsNextTrackingId = 0;
thread_local Stats tlsStats;

void CountObservation(Observation observation, std::uint64_t count = 1)
{
	tlsStats.observations[static_cast<std::size_t>(observation)] += count;
}

// 判定 kRelease 的实体。第一遍只推进核对状态并记下它们,第二遍再销毁:销毁会改 sceneRegistry,不在第一遍里做。
struct Release
{
	PendingCheck check;
	Observation observation{Observation::kUnknown};
	std::string owner;
	std::uint8_t streak{0};
	std::int64_t trackedSeconds{0};
};

// 本节点 node_id 的十进制串,与 scene_manager 写进 scene:{id}:node 的串同一种写法(strconv.FormatUint)。
std::string SelfNodeId()
{
	return std::to_string(GetNodeInfo().node_id());
}
} // namespace

std::vector<PendingCheck> CollectChecks(Clock::time_point now)
{
	auto &registry = tlsEcs.sceneRegistry;
	// 先固化实体列表,再登记组件:不在遍历 SceneInfoComp 视图的同时往这批实体上加组件。
	std::vector<std::pair<SceneEntity, std::uint64_t>> scenes;
	for (const auto &[scene, info] : registry.view<SceneInfoComp>().each())
	{
		if (info.scene_id() != 0)
		{
			scenes.emplace_back(scene, info.scene_id());
		}
	}

	std::vector<PendingCheck> checks;
	checks.reserve(scenes.size());
	for (const auto &[scene, sceneId] : scenes)
	{
		auto *check = registry.try_get<SceneOwnershipCheckComp>(scene);
		if (check == nullptr)
		{
			// 此处延迟创建(按需状态的唯一创建入口):第一次核对到这个实体,宽限期从现在算起。
			check = &registry.emplace<SceneOwnershipCheckComp>(scene);
			check->trackingId = ++tlsNextTrackingId;
			check->state.graceStartedAt = now;
		}
		checks.push_back(PendingCheck{scene, sceneId, check->trackingId});
	}
	return checks;
}

void RestartCheck(SceneEntity scene)
{
	if (tlsEcs.sceneRegistry.valid(scene))
	{
		tlsEcs.sceneRegistry.remove<SceneOwnershipCheckComp>(scene);
	}
}

std::vector<OwnerReading> ReadingsOf(const redisReply *reply, std::size_t sceneCount)
{
	std::vector<OwnerReading> readings(sceneCount); // 缺省 kUnreadable
	if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || reply->elements != sceneCount)
	{
		return readings;
	}
	for (std::size_t i = 0; i < sceneCount; ++i)
	{
		const redisReply *element = reply->element[i];
		if (element == nullptr)
		{
			continue;
		}
		if (element->type == REDIS_REPLY_NIL)
		{
			readings[i].kind = OwnerReading::Kind::kMissing;
		}
		else if (element->type == REDIS_REPLY_STRING)
		{
			readings[i].kind = OwnerReading::Kind::kValue;
			readings[i].value.assign(element->str, element->len);
		}
	}
	return readings;
}

ApplyOutcome ApplyReadings(const std::vector<PendingCheck> &checks, const std::vector<OwnerReading> &readings,
						   std::string_view selfNodeId, Clock::time_point issuedAt)
{
	ApplyOutcome outcome;
	if (readings.size() != checks.size())
	{
		LOG_ERROR << "[SceneOwnership] " << readings.size() << " owner reading(s) for " << checks.size()
				  << " scene(s); batch discarded";
		CountObservation(Observation::kUnknown, checks.size());
		return outcome;
	}

	auto &registry = tlsEcs.sceneRegistry;
	std::vector<Release> releases;
	for (std::size_t i = 0; i < checks.size(); ++i)
	{
		const PendingCheck &check = checks[i];
		// 读的途中实体被销毁(scene_manager 的 DestroyScene 先到),或核对状态被摘掉重建(CreateScene 又命中了它,
		// 可能正是一次迁回本节点的预建):这次读数作废。
		if (!registry.valid(check.scene))
		{
			continue;
		}
		auto *tracked = registry.try_get<SceneOwnershipCheckComp>(check.scene);
		const auto *info = registry.try_get<SceneInfoComp>(check.scene);
		if (tracked == nullptr || tracked->trackingId != check.trackingId || info == nullptr ||
			info->scene_id() != check.sceneId)
		{
			continue;
		}

		const Observation observation = Classify(readings[i], selfNodeId);
		CountObservation(observation);
		if (Advance(tracked->state, observation, issuedAt) != Decision::kRelease)
		{
			// 排空途中映射又回到本节点(scene_manager 把它迁了回来):不再排空,已改派走的住户由 scene_manager 重新落点。
			if (observation == Observation::kSelf && tracked->releaseStarted)
			{
				tracked->releaseStarted = false;
				LOG_INFO << "[SceneOwnership] scene " << check.sceneId
						 << " is mapped to this node again; stopped releasing it";
			}
			continue;
		}
		releases.push_back(Release{
			check, observation, readings[i].value, tracked->state.notMineStreak,
			std::chrono::duration_cast<std::chrono::seconds>(issuedAt - tracked->state.graceStartedAt).count()});
	}

	for (const Release &release : releases)
	{
		// 前一个实体的排空可能同步改了别的实体,逐个重新核对。
		if (!registry.valid(release.check.scene))
		{
			continue;
		}
		auto *tracked = registry.try_get<SceneOwnershipCheckComp>(release.check.scene);
		if (tracked == nullptr || tracked->trackingId != release.check.trackingId)
		{
			continue;
		}
		const bool firstRelease = !tracked->releaseStarted;
		tracked->releaseStarted = true;
		if (firstRelease)
		{
			++outcome.releaseStarted;
			++tlsStats.releaseStarted;
			// muduo 的 LogStream 不接受 string_view,转成 string。
			LOG_WARN << "[SceneOwnership] scene " << release.check.sceneId
					 << " is no longer mapped to this node: observed=" << std::string(ObservationName(release.observation))
					 << " owner=\"" << release.owner << "\" self_node=" << std::string(selfNodeId)
					 << " streak=" << static_cast<unsigned>(release.streak)
					 << " tracked_seconds=" << release.trackedSeconds
					 << "; relocating its residents and destroying the local entity";
		}
		// 与 scene_manager 发来的 DestroyScene 同一条路径:先排空住户(存盘后改派回大世界),没有绑定了才销毁。
		const std::size_t remainingBindings = SceneLifecycle::DestroyScene(release.check.scene);
		// tracked 可能已随实体销毁,此后不再访问。
		if (remainingBindings == 0)
		{
			++outcome.destroyed;
			++tlsStats.destroyed;
			LOG_INFO << "[SceneOwnership] destroyed unmapped scene " << release.check.sceneId;
		}
		else if (firstRelease)
		{
			LOG_WARN << "[SceneOwnership] scene " << release.check.sceneId << " kept until its " << remainingBindings
					 << " scene binding(s) leave; retried every sweep while it stays unmapped";
		}
	}
	return outcome;
}

const Stats &ReadStats()
{
	return tlsStats;
}

void ResetStatsForTest()
{
	tlsStats = Stats{};
}
} // namespace scene_ownership

namespace
{
void CollectSceneOwnership(prometheus_text::Writer &writer)
{
	const auto &stats = scene_ownership::ReadStats();
	writer.BeginFamily("scene_ownership_sweeps_total",
					   "Scene ownership reconcile rounds on this scene node since process start, by outcome.",
					   prometheus_text::MetricType::kCounter);
	for (std::size_t i = 0; i < stats.sweeps.size(); ++i)
	{
		writer.Sample({{"outcome", scene_ownership::kSweepOutcomeNames[i]}}, stats.sweeps[i]);
	}
	writer.BeginFamily("scene_ownership_observations_total",
					   "Owner readings of local scenes (scene:{id}:node in the zone control Redis), by result.",
					   prometheus_text::MetricType::kCounter);
	for (std::size_t i = 0; i < stats.observations.size(); ++i)
	{
		writer.Sample({{"result", scene_ownership::kObservationNames[i]}}, stats.observations[i]);
	}
	writer.BeginFamily("scene_ownership_releases_total",
					   "Local scenes found no longer mapped to this node: drain started, and entities destroyed. "
					   "Any increase means this node kept scenes the control plane had moved or torn down.",
					   prometheus_text::MetricType::kCounter);
	writer.Sample({{"stage", "drain_started"}}, stats.releaseStarted);
	writer.Sample({{"stage", "destroyed"}}, stats.destroyed);

	std::uint64_t releasing = 0;
	for (const auto &[scene, check] : tlsEcs.sceneRegistry.view<SceneOwnershipCheckComp>().each())
	{
		(void)scene;
		if (check.releaseStarted)
		{
			++releasing;
		}
	}
	writer.BeginFamily("scene_ownership_releasing_scenes",
					   "Local scenes already found unmapped and still waiting for their residents to leave.",
					   prometheus_text::MetricType::kGauge);
	writer.Sample({}, releasing);
}
} // namespace

void RegisterSceneOwnershipMetrics()
{
	if (!GlobalMetricsRegistry().Register("scene_ownership", &CollectSceneOwnership))
	{
		LOG_ERROR << "[Metrics] collector scene_ownership is already registered";
	}
}

// 一次 Start 到 Stop 之间的状态。定时器与 Redis 回调只捕获它的 weak_ptr:Stop 之后 lock 失败,回调直接返回。
struct SceneOwnershipReconciler::Session
{
	IdentityProbe identityConfirmed;
	// 已发出、应答还没回来的 MGET 条数。大于 0 时下一轮不发:Redis 卡住时不叠加读,
	// 断线时 hiredis 对在途命令逐个回空应答,这里会归零。
	std::size_t readsInFlight{0};

	bool IdentityConfirmed() const { return identityConfirmed && identityConfirmed(); }
};

SceneOwnershipReconciler::~SceneOwnershipReconciler()
{
	Stop();
}

void SceneOwnershipReconciler::Start(IdentityProbe identityConfirmed)
{
	Stop();
	session_ = std::make_shared<Session>();
	session_->identityConfirmed = std::move(identityConfirmed);
	const bool armed = timer_.RunEvery(static_cast<double>(scene_ownership::kSweepInterval.count()),
									   [weak = std::weak_ptr<Session>(session_)]
									   {
										   if (const auto session = weak.lock())
										   {
											   Sweep(session);
										   }
									   });
	if (!armed)
	{
		LOG_ERROR << "[SceneOwnership] reconcile timer was not armed (no EventLoop on this thread?); "
				  << "scenes moved away while this node was out of etcd will not be released";
		return;
	}
	LOG_INFO << "[SceneOwnership] reconcile started: interval=" << scene_ownership::kSweepInterval.count()
			 << "s confirmations=" << static_cast<unsigned>(scene_ownership::kNotMineConfirmations)
			 << " grace=" << scene_ownership::kCreateGrace.count() << "s";
}

void SceneOwnershipReconciler::Stop()
{
	timer_.Cancel();
	if (session_)
	{
		session_.reset();
		LOG_INFO << "[SceneOwnership] reconcile stopped";
	}
}

scene_ownership::SweepOutcome SceneOwnershipReconciler::SweepOnce()
{
	if (!session_)
	{
		return scene_ownership::SweepOutcome::kIdentityUnconfirmed;
	}
	return Sweep(session_);
}

bool SceneOwnershipReconciler::SendOwnerRead(hiredis::Hiredis &redis, const std::shared_ptr<Session> &session,
											 std::vector<scene_ownership::PendingCheck> batch,
											 const std::string &selfNodeId, scene_ownership::Clock::time_point issuedAt)
{
	// MGET scene:{id}:node ...:键名按长度传参,不经格式串。
	std::vector<std::string> keys;
	keys.reserve(batch.size());
	for (const auto &check : batch)
	{
		keys.push_back(scene_ownership::SceneNodeKey(check.sceneId));
	}
	static constexpr char kMget[] = "MGET";
	std::vector<const char *> argv;
	std::vector<std::size_t> argvLen;
	argv.reserve(keys.size() + 1);
	argvLen.reserve(keys.size() + 1);
	argv.push_back(kMget);
	argvLen.push_back(sizeof(kMget) - 1);
	for (const auto &key : keys)
	{
		argv.push_back(key.data());
		argvLen.push_back(key.size());
	}

	char *formatted = nullptr;
	const auto length = ::redisFormatCommandArgv(&formatted, static_cast<int>(argv.size()), argv.data(), argvLen.data());
	if (length < 0 || formatted == nullptr)
	{
		return false;
	}
	// 回调只按值捕获:Session 的 weak_ptr、这一批的实体句柄与核对状态号、发读时的节点号与时刻(AGENTS.md §11.7)。
	// MGET 不改数据:任何结局(含断线时的空应答)都不在维护前沿留下 UNKNOWN。
	const int ret = redis.formattedCommand(
		[weak = std::weak_ptr<Session>(session), batch = std::move(batch), selfNodeId,
		 issuedAt](hiredis::Hiredis *, redisReply *reply)
		{
			const auto live = weak.lock();
			if (!live)
			{
				return; // Stop 之后到达:不再动任何实体
			}
			if (live->readsInFlight > 0)
			{
				--live->readsInFlight;
			}
			// 应答回来时身份仍须确认有效、仍是同一个节点号,否则这批读数不当证据。
			if (!live->IdentityConfirmed() || scene_ownership::SelfNodeId() != selfNodeId)
			{
				scene_ownership::CountObservation(scene_ownership::Observation::kUnknown, batch.size());
				return;
			}
			scene_ownership::ApplyReadings(batch, scene_ownership::ReadingsOf(reply, batch.size()), selfNodeId, issuedAt);
		},
		formatted, static_cast<std::size_t>(length), &hiredis::Hiredis::hasNoSideEffect);
	::redisFreeCommand(formatted);
	return ret == REDIS_OK;
}

scene_ownership::SweepOutcome SceneOwnershipReconciler::Sweep(const std::shared_ptr<Session> &session)
{
	using scene_ownership::SweepOutcome;
	const auto finish = [](SweepOutcome outcome)
	{
		++scene_ownership::tlsStats.sweeps[static_cast<std::size_t>(outcome)];
		return outcome;
	};

	// 身份不确认时不核对:失联期间本节点分不清「etcd 整体故障」与「只有我连不上」,排空要走的改派链路也被身份闸挡着。
	if (!session->IdentityConfirmed())
	{
		return finish(SweepOutcome::kIdentityUnconfirmed);
	}
	auto &redis = tlsSceneControlRedis.GetConnection();
	if (!redis || !redis->connected())
	{
		return finish(SweepOutcome::kRedisUnavailable);
	}
	if (session->readsInFlight > 0)
	{
		return finish(SweepOutcome::kPreviousInFlight);
	}

	const auto issuedAt = scene_ownership::Clock::now();
	std::vector<scene_ownership::PendingCheck> checks = scene_ownership::CollectChecks(issuedAt);
	if (checks.empty())
	{
		return finish(SweepOutcome::kNoScenes);
	}
	const std::string selfNodeId = scene_ownership::SelfNodeId();

	bool anyNotSent = false;
	for (std::size_t begin = 0; begin < checks.size(); begin += scene_ownership::kMaxScenesPerRead)
	{
		const std::size_t end = std::min(checks.size(), begin + scene_ownership::kMaxScenesPerRead);
		std::vector<scene_ownership::PendingCheck> batch(checks.begin() + static_cast<std::ptrdiff_t>(begin),
														 checks.begin() + static_cast<std::ptrdiff_t>(end));
		const std::size_t batchSize = batch.size();
		if (SendOwnerRead(*redis, session, std::move(batch), selfNodeId, issuedAt))
		{
			++session->readsInFlight;
			continue;
		}
		anyNotSent = true;
		scene_ownership::CountObservation(scene_ownership::Observation::kUnknown, batchSize);
	}
	if (anyNotSent)
	{
		LOG_WARN << "[SceneOwnership] owner read not accepted by hiredis for some scenes; retrying next sweep";
		return finish(SweepOutcome::kSendFailed);
	}
	return finish(SweepOutcome::kSent);
}

#include "world/scene_ownership_reconciler.h"

#include <algorithm>
#include <chrono>
#include <cstddef>
#include <string>
#include <unordered_map>
#include <utility>

#include <hiredis/hiredis.h>

#include "muduo/base/Logging.h"

#include "engine/core/metrics/metrics_registry.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/scene/comp/scene_comp.h"
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

// 每个本地场景实体上绑定的玩家数,与 BeginSceneDrain 改派的范围同一个口径(SceneEntityComp + PlayerGuidComp)。
std::unordered_map<SceneEntity, std::size_t> CountResidents()
{
	std::unordered_map<SceneEntity, std::size_t> counts;
	for (const auto entity : tlsEcs.actorRegistry.view<SceneEntityComp, PlayerGuidComp>())
	{
		++counts[tlsEcs.actorRegistry.get<SceneEntityComp>(entity).sceneEntity];
	}
	return counts;
}

OwnerReading ReadingOf(const redisReply *element)
{
	OwnerReading reading;
	if (element == nullptr)
	{
		return reading;
	}
	if (element->type == REDIS_REPLY_NIL)
	{
		reading.kind = OwnerReading::Kind::kMissing;
	}
	else if (element->type == REDIS_REPLY_STRING)
	{
		reading.kind = OwnerReading::Kind::kValue;
		reading.value.assign(element->str, element->len);
	}
	return reading;
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

BatchReadings ReadBatch(const redisReply *reply, std::size_t sceneCount)
{
	BatchReadings readings;
	readings.owners.resize(sceneCount); // 缺省 kUnreadable
	if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || reply->elements != sceneCount + 1)
	{
		return readings;
	}
	readings.holder = ReadingOf(reply->element[0]);
	for (std::size_t i = 0; i < sceneCount; ++i)
	{
		readings.owners[i] = ReadingOf(reply->element[i + 1]);
	}
	return readings;
}

ApplyOutcome ApplyReadings(const std::vector<PendingCheck> &checks, const std::vector<OwnerReading> &owners,
						   std::string_view selfNodeId, SlotHolder slotHolder, Clock::time_point issuedAt,
						   ReleaseBudget &budget)
{
	ApplyOutcome outcome;
	if (owners.size() != checks.size())
	{
		LOG_ERROR << "[SceneOwnership] " << owners.size() << " owner reading(s) for " << checks.size()
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

		// 本节点号已被别的进程接走时,映射里的号指的是那个进程,一律不归本进程。
		const Observation observation =
			slotHolder == SlotHolder::kAnotherProcess ? Observation::kSuperseded : Classify(owners[i], selfNodeId);
		CountObservation(observation);
		if (Advance(tracked->state, observation, issuedAt) != Decision::kRelease)
		{
			// 映射又回到本节点(scene_manager 把它迁了回来):不再排空、不再等预算,已改派走的住户由 scene_manager 重新落点。
			if (observation == Observation::kSelf && (tracked->releaseStarted || tracked->releaseDeferred))
			{
				tracked->releaseStarted = false;
				tracked->releaseDeferred = false;
				LOG_INFO << "[SceneOwnership] scene " << check.sceneId
						 << " is mapped to this node again; stopped releasing it";
			}
			continue;
		}
		releases.push_back(Release{
			check, observation, owners[i].value, tracked->state.notMineStreak,
			std::chrono::duration_cast<std::chrono::seconds>(issuedAt - tracked->state.graceStartedAt).count()});
	}

	// 第二遍:已在排空的照常重调 DestroyScene(住户早已登记改派,不占预算);还没开始的按住户预算开始(§4.7)。
	std::unordered_map<SceneEntity, std::size_t> residents;
	bool residentsCounted = false;
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
		if (firstRelease)
		{
			if (!residentsCounted)
			{
				residents = CountResidents();
				residentsCounted = true;
			}
			const auto found = residents.find(release.check.scene);
			const std::size_t residentCount = found != residents.end() ? found->second : 0;
			if (!budget.TryStart(residentCount))
			{
				tracked->releaseDeferred = true;
				++outcome.deferred;
				continue;
			}
			tracked->releaseDeferred = false;
			tracked->releaseStarted = true;
			++outcome.releaseStarted;
			++tlsStats.releaseStarted;
			// muduo 的 LogStream 不接受 string_view,转成 string。
			LOG_WARN << "[SceneOwnership] scene " << release.check.sceneId
					 << " is no longer owned by this process: observed=" << std::string(ObservationName(release.observation))
					 << " owner=\"" << release.owner << "\" self_node=" << std::string(selfNodeId)
					 << " streak=" << static_cast<unsigned>(release.streak)
					 << " tracked_seconds=" << release.trackedSeconds << " residents=" << residentCount
					 << "; relocating its residents and destroying the local entity";
		}
		// 与 scene_manager 发来的 DestroyScene 同一条路径:先排空住户(存盘后改派回大世界),没有绑定了才销毁。
		const std::size_t remainingBindings = SceneLifecycle::DestroyScene(release.check.scene);
		// tracked 可能已随实体销毁,此后不再访问。
		if (remainingBindings == 0)
		{
			++outcome.destroyed;
			++tlsStats.destroyed;
			LOG_INFO << "[SceneOwnership] destroyed unowned scene " << release.check.sceneId;
		}
		else if (firstRelease)
		{
			LOG_WARN << "[SceneOwnership] scene " << release.check.sceneId << " kept until its " << remainingBindings
					 << " scene binding(s) leave; retried every sweep while it stays unowned";
		}
	}
	if (outcome.deferred > 0)
	{
		LOG_INFO << "[SceneOwnership] " << outcome.deferred
				 << " unowned scene(s) wait for the relocation budget; " << budget.remainingResidents
				 << " relocation(s) left this sweep";
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
					   "Ownership readings of local scenes (scene:{id}:node and this node id's holder record in the zone "
					   "control Redis), by result.",
					   prometheus_text::MetricType::kCounter);
	for (std::size_t i = 0; i < stats.observations.size(); ++i)
	{
		writer.Sample({{"result", scene_ownership::kObservationNames[i]}}, stats.observations[i]);
	}
	writer.BeginFamily("scene_ownership_releases_total",
					   "Local scenes found no longer owned by this process: drain started, and entities destroyed. "
					   "Any increase means this node kept scenes the control plane had moved, torn down, or handed to "
					   "the process that took over this node id.",
					   prometheus_text::MetricType::kCounter);
	writer.Sample({{"stage", "drain_started"}}, stats.releaseStarted);
	writer.Sample({{"stage", "destroyed"}}, stats.destroyed);

	std::uint64_t tracked = 0;
	std::uint64_t releasing = 0;
	std::uint64_t deferred = 0;
	for (const auto &[scene, check] : tlsEcs.sceneRegistry.view<SceneOwnershipCheckComp>().each())
	{
		(void)scene;
		++tracked;
		if (check.releaseStarted)
		{
			++releasing;
		}
		if (check.releaseDeferred)
		{
			++deferred;
		}
	}
	writer.BeginFamily("scene_ownership_tracked_scenes",
					   "Local scenes currently tracked by the ownership reconcile.",
					   prometheus_text::MetricType::kGauge);
	writer.Sample({}, tracked);
	writer.BeginFamily("scene_ownership_releasing_scenes",
					   "Local scenes already being released and still waiting for their residents to leave.",
					   prometheus_text::MetricType::kGauge);
	writer.Sample({}, releasing);
	writer.BeginFamily("scene_ownership_release_deferred_scenes",
					   "Local scenes found unowned but not released yet because this sweep's relocation budget ran out.",
					   prometheus_text::MetricType::kGauge);
	writer.Sample({}, deferred);
	writer.BeginFamily("scene_ownership_superseded",
					   "1 when the latest holder record shows this node id taken over by a newer registration of "
					   "another process.",
					   prometheus_text::MetricType::kGauge);
	writer.Sample({}, stats.superseded ? 1u : 0u);
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
	// 跨轮记着本进程自己那次登记的修订号,用来判断持有者记录是不是更新的别人(§4.6)。
	scene_ownership::HolderTracker holder;

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
				  << "scenes the control plane moved away from this node will not be released";
		return;
	}
	LOG_INFO << "[SceneOwnership] reconcile started: interval=" << scene_ownership::kSweepInterval.count()
			 << "s confirmations=" << static_cast<unsigned>(scene_ownership::kNotMineConfirmations)
			 << " grace=" << scene_ownership::kCreateGrace.count()
			 << "s relocations_per_sweep=" << scene_ownership::kMaxRelocationsPerSweep;
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

std::optional<scene_ownership::SweepOutcome> SceneOwnershipReconciler::SweepOnce()
{
	if (!session_)
	{
		return std::nullopt;
	}
	return Sweep(session_);
}

bool SceneOwnershipReconciler::SendOwnerRead(hiredis::Hiredis &redis, const std::shared_ptr<Session> &session,
											 std::vector<scene_ownership::PendingCheck> batch,
											 const std::string &holderKey, const std::string &selfNodeId,
											 const std::string &selfUuid, scene_ownership::Clock::time_point issuedAt,
											 const std::shared_ptr<scene_ownership::ReleaseBudget> &budget)
{
	// MGET <本节点号持有者> scene:{id}:node ...:键名按长度传参,不经格式串。
	std::vector<std::string> keys;
	keys.reserve(batch.size() + 1);
	keys.push_back(holderKey);
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
	// 回调只按值捕获:Session 的 weak_ptr、这一批的实体句柄与核对状态号、发读时的节点号 / uuid / 时刻、本轮预算
	// (AGENTS.md §11.7)。MGET 不改数据:任何结局(含断线时的空应答)都不在维护前沿留下 UNKNOWN。
	const int ret = redis.formattedCommand(
		[weak = std::weak_ptr<Session>(session), batch = std::move(batch), selfNodeId, selfUuid, issuedAt,
		 budget](hiredis::Hiredis *, redisReply *reply)
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
			// 应答回来时仍须是发读时的节点号,否则这批读数比的是别的号。
			if (scene_ownership::SelfNodeId() != selfNodeId)
			{
				scene_ownership::CountObservation(scene_ownership::Observation::kUnknown, batch.size());
				return;
			}
			const auto readings = scene_ownership::ReadBatch(reply, batch.size());
			const auto slotHolder = live->holder.Observe(readings.holder, selfUuid, live->IdentityConfirmed());
			// 读不到持有者记录时不改上一次的判定(指标与日志只跟着有信息的读数走)。
			if (readings.holder.kind != scene_ownership::OwnerReading::Kind::kUnreadable)
			{
				const bool superseded = slotHolder == scene_ownership::SlotHolder::kAnotherProcess;
				if (superseded != scene_ownership::tlsStats.superseded)
				{
					if (superseded)
					{
						LOG_ERROR << "[SceneOwnership] node id " << selfNodeId << " is held by a newer registration of another "
								  << "process (holder=\"" << readings.holder.value << "\", this process uuid=" << selfUuid
								  << " registration revision=" << live->holder.ownRevision
								  << "); releasing every local scene";
					}
					else
					{
						LOG_WARN << "[SceneOwnership] node id " << selfNodeId << " is no longer reported as taken over "
								 << "(holder=\"" << readings.holder.value << "\")";
					}
				}
				scene_ownership::tlsStats.superseded = superseded;
			}
			scene_ownership::ApplyReadings(batch, readings.owners, selfNodeId, slotHolder, issuedAt, *budget);
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

	// 不看本节点的 etcd 身份:「映射不是本节点号」与「本节点号被更新的登记接走」都不依赖 etcd 就成立。etcd 整体故障时
	// scene_manager 不会判任何节点死亡、映射不变,读到的都是本节点,什么也不做;只有本节点单边失联、被判死之后,映射才会变。
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
	const std::string selfUuid = GetNodeInfo().node_uuid();
	const std::string holderKey = scene_ownership::NodeHolderKey(GetZoneId(), selfNodeId);
	// 本轮全部批次共用一份住户预算;应答按发出顺序回来,先回来的批次先用。
	const auto budget = std::make_shared<scene_ownership::ReleaseBudget>();

	bool anyNotSent = false;
	for (std::size_t begin = 0; begin < checks.size(); begin += scene_ownership::kMaxScenesPerRead)
	{
		const std::size_t end = std::min(checks.size(), begin + scene_ownership::kMaxScenesPerRead);
		std::vector<scene_ownership::PendingCheck> batch(checks.begin() + static_cast<std::ptrdiff_t>(begin),
														 checks.begin() + static_cast<std::ptrdiff_t>(end));
		const std::size_t batchSize = batch.size();
		if (SendOwnerRead(*redis, session, std::move(batch), holderKey, selfNodeId, selfUuid, issuedAt, budget))
		{
			++session->readsInFlight;
			continue;
		}
		anyNotSent = true;
		scene_ownership::CountObservation(scene_ownership::Observation::kUnknown, batchSize);
	}
	if (anyNotSent)
	{
		LOG_WARN << "[SceneOwnership] ownership read not accepted by hiredis for some scenes; retrying next sweep";
		return finish(SweepOutcome::kSendFailed);
	}
	return finish(SweepOutcome::kSent);
}

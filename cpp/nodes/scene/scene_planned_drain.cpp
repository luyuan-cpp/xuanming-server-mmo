#include "scene_planned_drain.h"

#include <cstdlib>
#include <vector>

#include "muduo/base/Logging.h"
#include "infra/agones/agones_gameserver_lifecycle.h"
#include "node/system/node/node.h"
#include "node/system/node/node_admission_publisher.h"
#include "player/system/player_lifecycle.h"
#include "world/scene_planned_drain_rules.h"
#include "proto/common/base/common.pb.h"

namespace
{
	// 疏散限速的节拍。令牌按经过的时间补,节拍只决定放行的颗粒度。
	constexpr double kTickIntervalSec = 0.1;
	// admission 对账、自疏散排队、同类检查与进度日志的节拍。
	constexpr std::chrono::seconds kSlowTickInterval{1};
	// 每秒看一眼 Agones 排空标签(lifecycle 自己每 5 秒读一次标签,这里只读它的判定,不发 HTTP)。
	constexpr double kAgonesLabelPollIntervalSec = 1.0;
} // namespace

void ScenePlannedDrain::Begin(Node &node, const std::function<void()> &closeCreation)
{
	if (started_)
	{
		return;
	}
	started_ = true;
	agonesLabelTimer_.Cancel();

	// 先摘流:不再建新场景(scene_manager 收到 UNAVAILABLE 后回滚),并发布 admission = CLOSED 让它不再往这里放东西。
	closeCreation();

	const auto budget = node.PlannedDrainBudget();
	const auto selfEvacuateAfter =
		scene_planned_drain::SelfEvacuateAfter(budget, std::getenv(scene_planned_drain::kSelfEvacuateAfterEnvVar));
	const auto relocationsPerSecond =
		scene_planned_drain::RelocationsPerSecond(std::getenv(scene_planned_drain::kRelocationsPerSecondEnvVar));
	const auto now = std::chrono::steady_clock::now();
	selfEvacuateAt_ = now + selfEvacuateAfter;
	lastSecondTick_ = now;

	PlayerLifecycleSystem::BeginPlannedDrain(relocationsPerSecond);
	LOG_INFO << "[PlannedDrain] scene drain started: players=" << PlayerLifecycleSystem::LocalPlayerCount()
			 << " budget_s=" << budget.count() << " self_evacuate_after_s=" << selfEvacuateAfter.count()
			 << " trigger=" << (triggeredByAgonesLabel_ ? "agones_drain_label" : "sigterm");

	ReconcileAdmission(node);
	if (!HasOpenEvacuationTarget(node))
	{
		noTargetSince_ = now;
	}
	tickTimer_.RunEvery(kTickIntervalSec, [this, &node]
						{ Tick(node); });
}

bool ScenePlannedDrain::IsComplete(Node &node)
{
	if (!started_)
	{
		return false;
	}
	if (PlayerLifecycleSystem::IsPlannedDrainSettled())
	{
		LogProgress(node);
		return true;
	}
	if (noTargetSince_.has_value() &&
		std::chrono::steady_clock::now() - *noTargetSince_ >= scene_planned_drain::kNoTargetGrace)
	{
		if (!noTargetLogged_)
		{
			noTargetLogged_ = true;
			LOG_WARN << "[PlannedDrain] no open world-hosting scene node in this zone for "
					 << scene_planned_drain::kNoTargetGrace.count()
					 << "s; remaining players go through the normal shutdown save. players="
					 << PlayerLifecycleSystem::LocalPlayerCount();
		}
		return true;
	}
	return false;
}

void ScenePlannedDrain::Stop()
{
	tickTimer_.Cancel();
	agonesLabelTimer_.Cancel();
}

void ScenePlannedDrain::WatchAgonesDrainLabel(Node &node)
{
	agonesLabelTimer_.RunEvery(kAgonesLabelPollIntervalSec, [this, &node]
							   {
		if (started_ || node.PlannedDrainStarted())
		{
			agonesLabelTimer_.Cancel();
			return;
		}
		if (!agones::GameServerLifecycle::Instance().IsDraining())
		{
			return;
		}
		triggeredByAgonesLabel_ = true;
		agonesLabelTimer_.Cancel();
		LOG_WARN << "[PlannedDrain] Agones drain label observed; starting a planned drain";
		node.RequestPlannedStop(); });
}

void ScenePlannedDrain::Tick(Node &node)
{
	const auto now = std::chrono::steady_clock::now();
	PlayerLifecycleSystem::PumpPlannedEvacuations(now);
	if (now - lastSecondTick_ < kSlowTickInterval)
	{
		return;
	}
	lastSecondTick_ = now;

	ReconcileAdmission(node);

	// 自疏散:过了这个时刻还在本节点上的人(副本没打完、scene_manager 没赶上迁频道、排空开始后才路由进来的)
	// 由节点自己排进疏散队列;之后每秒再扫一次,接住晚到的。
	if (now >= selfEvacuateAt_)
	{
		const std::size_t queued = PlayerLifecycleSystem::QueueAllPlayersForPlannedDrain();
		if (!selfEvacuationStarted_)
		{
			selfEvacuationStarted_ = true;
			LOG_WARN << "[PlannedDrain] self-evacuation started: queued=" << queued
					 << " players=" << PlayerLifecycleSystem::LocalPlayerCount();
		}
		else if (queued > 0)
		{
			LOG_INFO << "[PlannedDrain] self-evacuation queued late arrivals: " << queued;
		}
	}

	if (HasOpenEvacuationTarget(node))
	{
		noTargetSince_.reset();
	}
	else if (!noTargetSince_.has_value())
	{
		noTargetSince_ = now;
	}

	LogProgress(node);
}

void ScenePlannedDrain::ReconcileAdmission(Node &node)
{
	const auto result = node_admission::Reconcile(node, node_admission::Advertised::kClosed, advertisePolicy_);
	if (result.action == node_admission::AdvertiseAction::kNone)
	{
		return;
	}
	const char *observed = result.observed ? node_admission::ToString(*result.observed) : "absent";
	if (result.action == node_admission::AdvertiseAction::kResend)
	{
		LOG_WARN << "[PlannedDrain] admission readback still stale; re-advertising closed to etcd: observed=" << observed;
	}
	else
	{
		LOG_INFO << "[PlannedDrain] admission advertised to etcd: admission=closed observed=" << observed;
	}
}

void ScenePlannedDrain::LogProgress(Node &)
{
	const std::size_t players = PlayerLifecycleSystem::LocalPlayerCount();
	const std::size_t queued = PlayerLifecycleSystem::PlannedEvacuationsQueued();
	if (players == lastLoggedPlayers_ && queued == lastLoggedQueued_)
	{
		return;
	}
	lastLoggedPlayers_ = players;
	lastLoggedQueued_ = queued;
	LOG_INFO << "[PlannedDrain] progress: players=" << players << " queued=" << queued
			 << " relocate_confirms=" << PlayerLifecycleSystem::RelocateConfirmsPending();
}

bool ScenePlannedDrain::HasOpenEvacuationTarget(Node &node) const
{
	// 发现快照里本 zone 的同类(scene 为同类自检与选号 watch 本 zone 的 scene 前缀)。只看 zone 与自报的 admission、角色:
	// 这是「还有没有地方可去」的提示,不判活 —— 节点从 etcd 摘除时会从快照里消失。
	const auto zoneId = node.GetNodeInfo().zone_id();
	std::vector<scene_planned_drain::Peer> peers;
	for (const auto &registered : node.GetServiceDiscoveryManager().ServiceNodes().NodesOfType(node.GetNodeType()))
	{
		if (registered.zone_id() != zoneId)
		{
			continue;
		}
		scene_planned_drain::Peer peer;
		peer.self = node.IsCurrentNode(registered);
		peer.closed = registered.admission() == NODE_ADMISSION_CLOSED;
		peer.sceneNodeType = registered.scene_node_type();
		peers.push_back(peer);
	}
	return scene_planned_drain::HasOpenWorldPeer(peers);
}

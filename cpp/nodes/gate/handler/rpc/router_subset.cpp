#include "router_subset.h"

#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <random>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

#include "client_request_lane.h"
#include "router_subset_policy.h"

#include "core/network/grpc_client_runtime.h"
#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"
#include "node/system/grpc_call_deadline.h"
#include "node/system/node/node.h"
#include "node/system/node/node_connector.h"
#include "node/system/node/node_util.h"
#include "node/system/node/release_track.h"
#include "thread_context/node_context_manager.h"
#include "time/comp/timer_task_comp.h"

namespace gate_router_subset
{
	namespace
	{
		constexpr uint32_t kRouterType = eNodeType::ClientRpcRouterNodeService;

		// 延后关闭的扫描间隔。扫描定时器只在有待关连接时运行。
		constexpr double kCloseSweepIntervalSeconds = 1.0;

		ChannelHealth HealthOf(muduo_grpc::Channel &channel)
		{
			switch (channel.GetState(false))
			{
			case muduo_grpc::ConnectivityState::kReady:
				return ChannelHealth::kReady;
			case muduo_grpc::ConnectivityState::kIdle:
			case muduo_grpc::ConnectivityState::kConnecting:
				return ChannelHealth::kConnectable;
			default:
				return ChannelHealth::kOther;
			}
		}

		std::string JoinForLog(const std::vector<std::string> &uuids)
		{
			std::string joined;
			for (const auto &uuid : uuids)
			{
				if (!joined.empty())
				{
					joined += ",";
				}
				joined += uuid;
			}
			return joined.empty() ? "(none)" : joined;
		}
	} // namespace

	// 一台 gate 的路由服子集状态。由 GateRuntimeContext 持有(比 Node 活得久),Node 上的准入 / 观察者回调与
	// 扫描定时器都只弱持有它(AGENTS.md §11.7)。只在 gate 的 loop 线程使用。
	class Subsetter : public std::enable_shared_from_this<Subsetter>
	{
	public:
		Subsetter(std::size_t subsetSize, uint32_t zoneId, release_track::Track selfTrack)
			: subsetSize_(subsetSize), zoneId_(zoneId), selfTrack_(selfTrack), random_(std::random_device{}())
		{
		}
		Subsetter(const Subsetter &) = delete;
		Subsetter &operator=(const Subsetter &) = delete;

		// 建连准入:在当前子集里。
		bool Admits(const NodeInfo &router)
		{
			Refresh();
			return SubsetContains(subset_, router.node_uuid());
		}

		// 发现快照里路由服的成员变了:按 PlanReconcile 建连、排延后关闭、撤销关闭。
		void Reconcile()
		{
			Refresh();
			const auto &discoveredNodes = gNode->GetServiceDiscoveryManager().ServiceNodes().NodesOfType(kRouterType);
			auto &registry = tlsNodeContextManager.GetRegistry(kRouterType);

			std::vector<std::string_view> discovered;
			discovered.reserve(static_cast<std::size_t>(discoveredNodes.size()));
			for (const auto &router : discoveredNodes)
			{
				discovered.push_back(router.node_uuid());
			}
			std::vector<std::string_view> connected;
			for (const auto &[entity, router] : registry.view<NodeInfo>().each())
			{
				connected.push_back(router.node_uuid());
			}
			std::vector<std::string_view> pendingClose;
			pendingClose.reserve(closeDeadlines_.size());
			for (const auto &[uuid, deadline] : closeDeadlines_)
			{
				pendingClose.push_back(uuid);
			}
			// plan 里是拷贝出来的 uuid:下面建连会往注册表里加实体、撤销会改 closeDeadlines_,借用的视图到那时可能失效。
			const auto plan = PlanReconcile(subset_, discovered, connected, pendingClose);

			for (const auto &uuid : plan.cancelClose)
			{
				closeDeadlines_.erase(uuid);
			}
			// 服务没启动时不建:StartRpcServer 里的 ConnectAllNodes 会按准入建。
			if (gNode->IsServiceStarted())
			{
				for (const auto &uuid : plan.connect)
				{
					for (const auto &router : discoveredNodes)
					{
						if (router.node_uuid() != uuid || !gNode->AdmitsConnection(router))
						{
							continue;
						}
						LOG_INFO << "Connecting to a router that entered this zone's subset, uuid=" << uuid;
						NodeConnector::ConnectToNode(router);
						break;
					}
				}
			}
			const auto now = std::chrono::steady_clock::now();
			for (const auto &uuid : plan.scheduleClose)
			{
				ScheduleClose(uuid, now);
			}
		}

		std::optional<entt::entity> Pick()
		{
			Refresh();
			auto &registry = tlsNodeContextManager.GetRegistry(kRouterType);
			std::vector<entt::entity> candidates;
			for (const auto &[entity, router] : registry.view<NodeInfo>().each())
			{
				// 认不出的轨道既不能当 stable(stable 会话会被送进它),也不能当 canary;发现时已记过错误日志。
				if (!SubsetContains(subset_, router.node_uuid()) || !release_track::Parse(router.release_track()) ||
					!HasUsableGrpcClient(registry, entity))
				{
					continue;
				}
				candidates.push_back(entity);
			}
			candidates = release_track::Select(selfTrack_, candidates, [&registry](entt::entity entity)
			{
				return release_track::Parse(registry.get<NodeInfo>(entity).release_track())
					.value_or(release_track::Track::kStable);
			});

			std::vector<ChannelHealth> health;
			health.reserve(candidates.size());
			for (const auto entity : candidates)
			{
				health.push_back(HealthOf(*FindGrpcChannel(registry, entity)));
			}
			const auto picked = PickByHealth(health, random_());
			if (!picked)
			{
				return std::nullopt;
			}
			return candidates[*picked];
		}

	private:
		// 发现快照的代数变了才重算子集;选点每次都要问子集,快照不变时只比一次代数。
		void Refresh()
		{
			const auto &snapshot = gNode->GetServiceDiscoveryManager().ServiceNodes();
			const uint64_t generation = snapshot.Generation(kRouterType);
			if (computedGeneration_ == generation)
			{
				return;
			}
			computedGeneration_ = generation;

			const auto &routers = snapshot.NodesOfType(kRouterType);
			std::vector<RouterMember> members;
			members.reserve(static_cast<std::size_t>(routers.size()));
			for (const auto &router : routers)
			{
				members.push_back(RouterMember{router.node_uuid(), release_track::Parse(router.release_track())});
			}
			auto computed = ComputeSubset(zoneId_, selfTrack_, members, subsetSize_);
			if (computed == subset_)
			{
				return;
			}
			subset_ = std::move(computed);
			LOG_INFO << "Router subset changed: zone_id=" << zoneId_
					 << ", release_track=" << std::string(release_track::Name(selfTrack_))
					 << ", subset_size=" << subsetSize_ << ", discovered=" << routers.size()
					 << ", members=" << JoinForLog(subset_);
		}

		// 掉出子集:立即不再被挑(Pick 只看子集),过「调用期限 + lane 在途过期余量」再关。到那时 lane 里发往它的请求
		// 都已收到回包或被过期认领;万一还在途,~Channel 以 UNAVAILABLE 完成,lane 按恰好一个回执认领。已排过的不顺延。
		void ScheduleClose(const std::string &uuid, std::chrono::steady_clock::time_point now)
		{
			if (closeDeadlines_.contains(uuid))
			{
				return;
			}
			const auto delay = grpc_call_deadline::Get(kRouterType) + gate_lane::kInflightExpiryMargin;
			closeDeadlines_.emplace(uuid, now + delay);
			LOG_INFO << "Router left this zone's subset; no longer picked, closing in " << delay.count()
					 << "ms, uuid=" << uuid;
			if (!closeSweepTimer_.IsActive())
			{
				closeSweepTimer_.RunEvery(kCloseSweepIntervalSeconds, [weak = weak_from_this()]
				{
					if (const auto self = weak.lock())
					{
						self->SweepCloses();
					}
				});
			}
		}

		void SweepCloses()
		{
			Refresh();
			const auto now = std::chrono::steady_clock::now();
			for (auto it = closeDeadlines_.begin(); it != closeDeadlines_.end();)
			{
				if (SubsetContains(subset_, it->first))
				{
					it = closeDeadlines_.erase(it); // 到期前回到了子集:保留连接
					continue;
				}
				if (now < it->second)
				{
					++it;
					continue;
				}
				// 排到下一轮拆,拆之前引擎再判一次准入。连接已经没了(节点摘除先到)时引擎什么也不做。
				gNode->DropUnadmittedConnection(kRouterType, it->first);
				it = closeDeadlines_.erase(it);
			}
			if (closeDeadlines_.empty())
			{
				closeSweepTimer_.Cancel();
			}
		}

		const std::size_t subsetSize_;
		const uint32_t zoneId_;
		const release_track::Track selfTrack_;
		std::optional<uint64_t> computedGeneration_;
		// ComputeSubset 的结果:升序、无重复,用 SubsetContains 查。
		std::vector<std::string> subset_;
		// 掉出子集、等着关的连接:uuid -> 到期时刻(单调时钟)。
		std::unordered_map<std::string, std::chrono::steady_clock::time_point> closeDeadlines_;
		TimerTaskComp closeSweepTimer_;
		std::mt19937_64 random_;
	};

	namespace
	{
		// PickRouter 的入口找安装好的那一份;只在 loop 线程读写。
		thread_local std::weak_ptr<Subsetter> tlsInstalled;
	} // namespace

	std::shared_ptr<Subsetter> Install(Node &node)
	{
		const char *raw = std::getenv(kSubsetSizeEnv);
		const auto subsetSize = ParseSubsetSize(raw);
		if (!subsetSize)
		{
			LOG_FATAL << kSubsetSizeEnv << "='" << raw << "' is invalid: expected a decimal integer in ["
					  << kMinSubsetSize << ", " << kMaxSubsetSize << "], or unset for the default " << kDefaultSubsetSize;
		}
		const auto &self = node.GetNodeInfo();
		// 本节点轨道在 Node::InitRpcServer 校验过,非法值已拒启。
		const auto selfTrack = release_track::Parse(self.release_track()).value_or(release_track::Track::kStable);
		auto subsetter = std::make_shared<Subsetter>(*subsetSize, self.zone_id(), selfTrack);
		tlsInstalled = subsetter;

		const std::weak_ptr<Subsetter> weak = subsetter;
		node.SetConnectionAdmission(kRouterType, [weak](const NodeInfo &router)
		{
			const auto installed = weak.lock();
			return installed != nullptr && installed->Admits(router);
		});
		node.SetDiscoveryObserver(kRouterType, [weak]
		{
			if (const auto installed = weak.lock())
			{
				installed->Reconcile();
			}
		});
		LOG_INFO << "Gate router subset installed: zone_id=" << self.zone_id()
				 << ", release_track=" << std::string(release_track::Name(selfTrack)) << ", subset_size=" << *subsetSize
				 << " (" << kSubsetSizeEnv << ", default " << kDefaultSubsetSize << ")";
		return subsetter;
	}

	std::optional<entt::entity> PickRouter()
	{
		const auto subsetter = tlsInstalled.lock();
		return subsetter != nullptr ? subsetter->Pick() : std::nullopt;
	}
} // namespace gate_router_subset

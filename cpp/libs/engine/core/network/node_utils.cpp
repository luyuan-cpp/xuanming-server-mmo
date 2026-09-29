#include "node_utils.h"
#include <cstddef>
#include <cstdint>
#include <memory>
#include <grpcpp/channel.h>
#include "muduo/base/Logging.h"
#include <thread_context/ecs_context.h>
#include <thread_context/node_context_manager.h>

NodeInfo &GetNodeInfo()
{
	return tlsEcs.globalRegistry.get_or_emplace<NodeInfo>(tlsEcs.GlobalEntity());
}

uint32_t GetZoneId()
{
	return GetNodeInfo().zone_id();
}

namespace
{
	// 为什么分三档,而不是像 PickReadyDataServiceNode 那样只认 READY:
	//
	// - 只认 READY 会在两种正常情形下一个都挑不到:
	//   1) 通道创建后是 IDLE。ConnectToGrpcNode 只建通道不拨号,第一次调用(或 GetState(true))才去连;
	//   2) 连着的通道 30 分钟没有调用会回 IDLE。GrpcChannelCache 没设 GRPC_ARG_CLIENT_IDLE_TIMEOUT_MS,
	//      gRPC 默认 30 分钟;keepalive ping 不算调用,挡不住它。
	//   号段传输只认 READY 没事,因为它的调用方按退避重试;SceneManager 的调用方挑不到就立即失败、
	//   给玩家回提示,不能拿"刚启动 / 夜里没人换图"当失败。
	// - TRANSIENT_FAILURE 永不选:实例崩溃后连接断开,通道先回 IDLE;这里的 GetState(true) 把它踢去重连,
	//   对端拒连就进 TRANSIENT_FAILURE,并且 pick_first 在退避重连期间一直停在 TRANSIENT_FAILURE、不会
	//   回报 CONNECTING,直到真的连上(third_party/grpc pick_first.cc)。这个状态下的调用立即以 UNAVAILABLE
	//   失败(默认不 wait_for_ready),挑它等于必败。etcd 租约 60s 到期前死实例仍在注册表里,这正是要挡掉的窗口。
	// - 残余窗口:实例刚崩、还没被踢过时它显示 IDLE。只有在一个 READY 都没有时它才可能被挑中,那一次请求
	//   以 UNAVAILABLE 走失败处理器(grpc-client-deadline-failure-callback.md §5),之后它就是 TRANSIENT_FAILURE。
	//   对端主机整个消失(没有 RST)时,重连要等到连接超时,这段时间它停在 CONNECTING,同样排在 READY 之后。
	enum class ChannelTier : uint8_t
	{
		kPreferred, // READY
		kFallback,  // IDLE / CONNECTING
		kUnusable,  // TRANSIENT_FAILURE / SHUTDOWN
	};

	ChannelTier ClassifyChannel(grpc_connectivity_state state)
	{
		switch (state)
		{
		case GRPC_CHANNEL_READY:
			return ChannelTier::kPreferred;
		case GRPC_CHANNEL_IDLE:
		case GRPC_CHANNEL_CONNECTING:
			return ChannelTier::kFallback;
		case GRPC_CHANNEL_TRANSIENT_FAILURE:
		case GRPC_CHANNEL_SHUTDOWN:
		default:
			return ChannelTier::kUnusable;
		}
	}

	// 取模前先把 playerId 打散(splitmix64 的终混步)。PlayerId 是 bwmarrin 布局
	// (41 位毫秒 | 13 位 node | 9 位 step),低 9 位是毫秒内序号、每个新毫秒归零,低频建角时几乎全是 0。
	// 直接 playerId % 实例数,实例数为 2 / 4 / 8 时几乎所有玩家都压在第 0 个实例上(旧实现就是这样)。
	uint64_t SpreadPlayerId(Guid playerId)
	{
		uint64_t z = playerId + 0x9e3779b97f4a7c15ull;
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9ull;
		z = (z ^ (z >> 27)) * 0x94d049bb133111ebull;
		return z ^ (z >> 31);
	}

	entt::entity PickWithinTier(const std::vector<scene_manager_selector::Candidate> &candidates, ChannelTier tier,
								Guid playerId)
	{
		std::size_t tierSize = 0;
		for (const auto &candidate : candidates)
		{
			if (ClassifyChannel(candidate.channelState) == tier)
			{
				++tierSize;
			}
		}
		if (tierSize == 0)
		{
			return entt::null;
		}

		auto remaining = SpreadPlayerId(playerId) % tierSize;
		for (const auto &candidate : candidates)
		{
			if (ClassifyChannel(candidate.channelState) != tier)
			{
				continue;
			}
			if (remaining == 0)
			{
				return candidate.entity;
			}
			--remaining;
		}
		return entt::null;
	}
}

namespace scene_manager_selector
{
	// 同档取模而不是按玩家粘滞(rendezvous 哈希):go/scene_manager 里进程内的状态全都按场景节点记
	// (knownNodes / nodeGoneObservedAt / pendingDeadNodes / deferredNodeDetaches / nodeConnCache),
	// 按玩家的状态(request_id 去重、owner_epoch、交接标记)都在 Redis 里,各实例共享。节点维度的本地观察
	// 本来就各副本各记一份,设计上已按"本副本没看到就退回 Redis 的 death_at"兜底,与玩家落在哪个实例无关。
	// 所以候选集变化时"大多数玩家换实例"没有正确性代价,不值得多一层哈希。
	// 顺序上也没有损失:即使同一实例,gRPC 服务端也是每个调用一个 goroutine,本来就不保证两次请求的先后。
	// 若日后 scene_manager 加了按玩家驻留在进程内存里的状态,这里要改成 rendezvous 哈希。
	entt::entity Pick(const std::vector<Candidate> &candidates, Guid playerId)
	{
		if (const auto picked = PickWithinTier(candidates, ChannelTier::kPreferred, playerId); picked != entt::null)
		{
			return picked;
		}
		return PickWithinTier(candidates, ChannelTier::kFallback, playerId);
	}
}

entt::entity GetSceneManagerEntity(Guid playerId)
{
	auto &smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);

	// 只看挂了通道的实体。ConnectToGrpcNode 在同一个函数里依次挂通道、InitGrpcNode(CompletionQueue 与
	// SceneManager stub)、NodeInfo,所以"有通道"就等于发送函数要 get 的 stub / CompletionQueue 都在;
	// 没有通道的实体(今天只有单测的假节点)挑中后一发送就会断言,不能要。
	// 实例数是个位数,每次现拼一张候选表;调用频率跟着玩家换图走,不是热路径。
	std::vector<scene_manager_selector::Candidate> candidates;
	for (const auto entity : smRegistry.view<NodeInfo, std::shared_ptr<grpc::Channel>>())
	{
		const auto &channel = smRegistry.get<std::shared_ptr<grpc::Channel>>(entity);
		const auto state = channel ? channel->GetState(/*try_to_connect=*/true) : GRPC_CHANNEL_SHUTDOWN;
		candidates.push_back(scene_manager_selector::Candidate{entity, state});
	}

	const auto picked = scene_manager_selector::Pick(candidates, playerId);
	if (picked == entt::null && !candidates.empty())
	{
		// 注册表里有实例、但全都不可达:与"一个都没注册"分开记,排障时一眼能分清是发现问题还是连接问题。
		// 调用方还会各记一行自己的上下文。
		std::size_t transientFailure = 0;
		std::size_t shutdown = 0;
		for (const auto &candidate : candidates)
		{
			if (candidate.channelState == GRPC_CHANNEL_TRANSIENT_FAILURE)
			{
				++transientFailure;
			}
			else if (candidate.channelState == GRPC_CHANNEL_SHUTDOWN)
			{
				++shutdown;
			}
		}
		LOG_WARN << "[SceneManagerSelect] " << candidates.size()
				 << " SceneManager instance(s) registered but none reachable: transient_failure=" << transientFailure
				 << " shutdown=" << shutdown;
	}
	return picked;
}

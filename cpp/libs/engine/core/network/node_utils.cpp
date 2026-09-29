#include "node_utils.h"
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <memory>
#include <optional>
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
	// 分四级挑,前一级有实例就不看后一级(枚举顺序即挑选顺序):
	//
	//   ①  READY
	//   ②  IDLE / CONNECTING
	//   ③a 挂了通道、状态为 TRANSIENT_FAILURE / SHUTDOWN
	//   ③b 没挂通道
	//
	// 底线:注册表里只要有一个 SceneManager 实体就一定挑出一个,与旧实现(playerId % 已注册实例数)一样,
	// 只在注册表为空时返回 null。分级只做"有更好的选择时避开坏通道",不新增 null 窗口 —— 调用方拿到 null
	// 走的是立即失败分支,它们比"挑一个坏实例、让那次调用失败"更伤:跨 zone 交接在 handoff 标记 SET 已发出后
	// 拿不到 SceneManager 只能 ConcludeHandoffAfterMarkSent(销毁 + 踢线重登);而挑一个坏实例,那次调用以
	// UNAVAILABLE 进失败处理器(grpc-client-deadline-failure-callback.md §5)或由 30s 看门狗收尾,
	// 这正是改造前的行为。
	//
	// 为什么 ② 算可用,而不是像 PickReadyDataServiceNode 那样只认 READY:
	// - 只认 READY 会在两种正常情形下一个都挑不到:
	//   1) 通道创建后是 IDLE。ConnectToGrpcNode 只建通道不拨号,第一次调用(或 GetState(true))才去连;
	//   2) 连着的通道 30 分钟没有调用会回 IDLE。GrpcChannelCache 没设 GRPC_ARG_CLIENT_IDLE_TIMEOUT_MS,
	//      gRPC 默认 30 分钟;keepalive ping 不算调用,挡不住它。
	//   号段传输只认 READY 没事,因为它的调用方按退避重试;SceneManager 的调用方不能拿"刚启动 / 夜里没人换图"
	//   当失败。
	//
	// 为什么 ③a 排在 ② 之后:实例崩溃后连接断开,通道先回 IDLE;这里的 GetState(true) 把它踢去重连,对端拒连
	// 就进 TRANSIENT_FAILURE,并且 pick_first 在退避重连期间一直停在 TRANSIENT_FAILURE、不会回报 CONNECTING,
	// 直到真的连上(third_party/grpc pick_first.cc)。这个状态下的调用立即以 UNAVAILABLE 失败(默认不
	// wait_for_ready),有 ①② 时挑它等于白白失败;etcd 租约 60s 到期前死实例仍在注册表里,这正是要避开的窗口。
	// 只有 ①② 全空时才兜底挑它,结果与改造前相同。
	//
	// 为什么 ③b 放在最末级:正常路径下通道先于 NodeInfo 挂上(ConnectToGrpcNode 先 emplace 通道、InitGrpcNode
	// 挂 CompletionQueue 与 stub,最后才 emplace NodeInfo),经 gRPC 发现的实体不会缺通道。但生成的 IsTcpNodeType
	// 白名单含 SceneManagerNodeService(cpp/generated/proto_helpers/proto_util.cpp),任何 TCP 对端握手时声明
	// node_type=SceneManager,NodeHandshakeManager(registration_manager.cpp)就会在本注册表里建一个只有
	// NodeInfo + RpcSession 的实体;之后同 uuid 的 gRPC 发现在 ConnectToGrpcNode 里按"already registered"
	// 提前返回,永远补不上通道。这种实体被挑中后,生成的 SendSceneManager* 要 registry.get<grpc::CompletionQueue>
	// / get<SceneManagerStubPtr>,取不到组件:Debug 断言,Release 未定义行为。所以它只在 ①②③a 全空时才会被选,
	// 保留它只为与旧实现"有实体就挑得到"兼容,以及单测的假节点(cross_zone_test 的 ScopedFakeSceneManagerNode
	// 只挂 NodeInfo)。不把它记成 SHUTDOWN 与 ③a 同级:同级取模会让它在"有 TF 实例可兜底"时也可能被挑中。
	//
	// 残余窗口:实例刚崩、还没被踢过时它显示 IDLE。只有在一个 READY 都没有时它才可能被挑中,那一次请求
	// 以 UNAVAILABLE 走失败处理器,之后它就是 TRANSIENT_FAILURE。对端主机整个消失(没有 RST)时,重连要等到
	// 连接超时,这段时间它停在 CONNECTING,同样排在 READY 之后。
	enum class SelectionTier : uint8_t
	{
		kReady,               // ①
		kConnectable,         // ②
		kLastResortFailed,    // ③a
		kLastResortNoChannel, // ③b
		kCount,
	};
	constexpr std::size_t kSelectionTierCount = static_cast<std::size_t>(SelectionTier::kCount);

	// 只用于兜底告警里标出挑中的是哪一级。顺序必须与 SelectionTier 逐项对应(static_assert 只拦数量不一致)。
	constexpr const char *kTierNames[] = {
		"ready",
		"idle_or_connecting",
		"failed_channel",
		"no_channel",
	};
	static_assert(std::size(kTierNames) == kSelectionTierCount,
				  "kTierNames 必须与 SelectionTier 一一对应:加了枚举项就得加名字");

	SelectionTier ClassifyCandidate(const scene_manager_selector::Candidate &candidate)
	{
		if (!candidate.channelState)
		{
			return SelectionTier::kLastResortNoChannel;
		}
		switch (*candidate.channelState)
		{
		case GRPC_CHANNEL_READY:
			return SelectionTier::kReady;
		case GRPC_CHANNEL_IDLE:
		case GRPC_CHANNEL_CONNECTING:
			return SelectionTier::kConnectable;
		case GRPC_CHANNEL_TRANSIENT_FAILURE:
		case GRPC_CHANNEL_SHUTDOWN:
		default:
			return SelectionTier::kLastResortFailed;
		}
	}

	const char *TierName(SelectionTier tier)
	{
		return kTierNames[static_cast<std::size_t>(tier)];
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

	entt::entity PickWithinTier(const std::vector<scene_manager_selector::Candidate> &candidates, SelectionTier tier,
								Guid playerId)
	{
		std::size_t tierSize = 0;
		for (const auto &candidate : candidates)
		{
			if (ClassifyCandidate(candidate) == tier)
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
			if (ClassifyCandidate(candidate) != tier)
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

	// 落到 ③a / ③b 时的告警:说明此刻没有任何能用的 SceneManager 通道,本次调用多半以 UNAVAILABLE 失败
	// (挑中 ③b 则会在发送处断言,见上面分级说明)。全挂期间每次换图都会走到这里,所以要节流:
	// 每 10s 最多一行,期间压掉的次数带在下一行里,排障时仍能看出频率。
	//
	// 线程模型:GetSceneManagerEntity 读的是 thread_local 的 tlsNodeContextManager,只在拥有它的节点主循环线程上
	// 调用(调用方都是 scene 主循环里的换图 / 跨 zone 交接 / 组队跟随 / RPC 应答处理;gRPC 应答也是主循环上的
	// 定时器轮询 CompletionQueue 后分发的,见 etcd_service.cpp)。节流状态与注册表同样做成 thread_local,
	// 不需要锁;每个进程只有这一个线程调它,"每线程 10s 一行"就是"每进程 10s 一行"。
	// 用 steady_clock:节流间隔不能受墙钟跳变影响。
	constexpr auto kLastResortWarnInterval = std::chrono::seconds(10);

	void WarnLastResortPick(const std::vector<scene_manager_selector::Candidate> &candidates, entt::entity picked,
							SelectionTier pickedTier)
	{
		struct WarnThrottle
		{
			std::optional<std::chrono::steady_clock::time_point> lastWarnAt;
			uint64_t suppressedSinceLastWarn{0};
		};
		static thread_local WarnThrottle throttle;

		const auto now = std::chrono::steady_clock::now();
		if (throttle.lastWarnAt && now - *throttle.lastWarnAt < kLastResortWarnInterval)
		{
			++throttle.suppressedSinceLastWarn;
			return;
		}

		// 走到兜底级说明 ①② 都是空的,READY / IDLE / CONNECTING 计数必为 0,不再打印。
		std::size_t transientFailure = 0;
		std::size_t shutdown = 0;
		std::size_t noChannel = 0;
		for (const auto &candidate : candidates)
		{
			if (!candidate.channelState)
			{
				++noChannel;
			}
			else if (*candidate.channelState == GRPC_CHANNEL_TRANSIENT_FAILURE)
			{
				++transientFailure;
			}
			else if (*candidate.channelState == GRPC_CHANNEL_SHUTDOWN)
			{
				++shutdown;
			}
		}
		LOG_WARN << "[SceneManagerSelect] no READY / IDLE / CONNECTING SceneManager instance, last-resort pick:"
				 << " registered=" << candidates.size()
				 << " transient_failure=" << transientFailure
				 << " shutdown=" << shutdown
				 << " no_channel=" << noChannel
				 << " picked_entity=" << entt::to_integral(picked)
				 << " picked_tier=" << TierName(pickedTier)
				 << " suppressed_since_last_warn=" << throttle.suppressedSinceLastWarn;
		throttle.lastWarnAt = now;
		throttle.suppressedSinceLastWarn = 0;
	}
}

namespace scene_manager_selector
{
	// 同级取模而不是按玩家粘滞(rendezvous 哈希):go/scene_manager 里进程内的状态全都按场景节点记
	// (knownNodes / nodeGoneObservedAt / pendingDeadNodes / deferredNodeDetaches / nodeConnCache),
	// 按玩家的状态(request_id 去重、owner_epoch、交接标记)都在 Redis 里,各实例共享。节点维度的本地观察
	// 本来就各副本各记一份,设计上已按"本副本没看到就退回 Redis 的 death_at"兜底,与玩家落在哪个实例无关。
	// 所以候选集变化时"大多数玩家换实例"没有正确性代价,不值得多一层哈希。
	// 顺序上也没有损失:即使同一实例,gRPC 服务端也是每个调用一个 goroutine,本来就不保证两次请求的先后。
	// 若日后 scene_manager 加了按玩家驻留在进程内存里的状态,这里要改成 rendezvous 哈希。
	entt::entity Pick(const std::vector<Candidate> &candidates, Guid playerId)
	{
		// 按枚举顺序逐级挑,前一级有实例就不看后一级。
		for (std::size_t tierIndex = 0; tierIndex < kSelectionTierCount; ++tierIndex)
		{
			const auto tier = static_cast<SelectionTier>(tierIndex);
			if (const auto picked = PickWithinTier(candidates, tier, playerId); picked != entt::null)
			{
				return picked;
			}
		}
		// 四级覆盖了所有候选,只有空表会走到这里。
		return entt::null;
	}
}

entt::entity GetSceneManagerEntity(Guid playerId)
{
	auto &smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);

	// 候选 = 所有挂了 NodeInfo 的 SceneManager 实体,与旧实现(playerId % view<NodeInfo>().size())同一个集合,
	// 所以"注册表非空就一定挑得出"与旧实现一致。没挂通道的实体也算进来,只是排在最末级(见上面分级说明)。
	// 挂了通道组件但指针为空的,与没挂同样发不出去,一并记成"没有通道"。
	// 实例数是个位数,每次现拼一张候选表;调用频率跟着玩家换图走,不是热路径。
	std::vector<scene_manager_selector::Candidate> candidates;
	for (const auto entity : smRegistry.view<NodeInfo>())
	{
		scene_manager_selector::Candidate candidate{entity};
		if (const auto *channel = smRegistry.try_get<std::shared_ptr<grpc::Channel>>(entity);
			channel != nullptr && *channel != nullptr)
		{
			candidate.channelState = (*channel)->GetState(/*try_to_connect=*/true);
		}
		candidates.push_back(candidate);
	}

	const auto picked = scene_manager_selector::Pick(candidates, playerId);
	if (picked == entt::null)
	{
		// 一个 SceneManager 都没注册。调用方各记一行自己的上下文,这里不重复记。
		return entt::null;
	}

	for (const auto &candidate : candidates)
	{
		if (candidate.entity != picked)
		{
			continue;
		}
		if (const auto tier = ClassifyCandidate(candidate);
			tier == SelectionTier::kLastResortFailed || tier == SelectionTier::kLastResortNoChannel)
		{
			WarnLastResortPick(candidates, picked, tier);
		}
		break;
	}
	return picked;
}

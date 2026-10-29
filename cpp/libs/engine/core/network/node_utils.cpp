#include "node_utils.h"
#include "grpc_client_runtime.h"
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <memory>
#include <optional>
#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"
#include <thread_context/node_context_manager.h>

namespace
{
const NodeInfo *localNodeInfo = nullptr;
}

void InstallLocalNodeInfo(const NodeInfo *info)
{
	localNodeInfo = info;
}

const NodeInfo &GetNodeInfo()
{
	return localNodeInfo != nullptr ? *localNodeInfo : NodeInfo::default_instance();
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
	// 底线:注册表里只要有一个挂着未关闭通道的 SceneManager 实例(发现即建通道,建不成不留实体,生产上就是每个已发现的
	// 实例)就一定挑出一个,与旧实现(playerId % 已注册实例数)一样,只在没有这样的实例时返回 null。
	// 分级只做"有更好的选择时避开坏通道",不新增 null 窗口 —— 调用方拿到 null 走的是立即失败分支,它们比
	// "挑一个坏实例、让那次调用失败"更伤:跨 zone 交接在 handoff 标记 SET 已发出后
	// 拿不到 SceneManager 只能 ConcludeHandoffAfterMarkSent(销毁 + 踢线重登);而挑一个坏实例,那次调用以
	// UNAVAILABLE 完成每次调用回调(grpc-muduo-transport.md §16.1),或由 30s 看门狗收尾,
	// 这正是改造前的行为。
	//
	// 状态取自 muduo_grpc::Channel(docs/design/grpc-muduo-transport.md §5):IDLE = 没有连接;CONNECTING = 正在建 TCP、
	// 等对端首个 SETTINGS,单次尝试上限 connect_timeout(20s);READY = 收到对端 SETTINGS;TRANSIENT_FAILURE = 本次尝试
	// 失败、按 1s × 1.6(±20%)封顶 120s 退避,期间新调用立即失败;SHUTDOWN = 通道已析构。
	//
	// 为什么 ② 算可用,而不是像 PickReadyDataServiceNode 那样只认 READY:
	// - 只认 READY 会在两种正常情形下一个都挑不到:
	//   1) 刚发现的实例。ConnectToGrpcNode 建通道即 GetState(true) 预连,收到对端 SETTINGS 之前一直是 CONNECTING;
	//   2) 连接没了、通道回 IDLE。READY 连接断开(含收到对端 GOAWAY 后退役)都回 IDLE,下一次调用或 GetState(true)
	//      才重连。go-zero 服务端 MaxConnectionIdle = 5min,没有在途调用满 5 分钟的连接会被 GOAWAY 关掉(设计 §4.5),
	//      所以一阵子没人换图后各实例都是 IDLE。这个 IDLE 来自服务端:通道自己没有空闲超时,业务通道也不发周期
	//      keepalive(§4.4),而 keepalive 本来也挡不住 MaxConnectionIdle(它只看在途调用)。
	//   号段传输只认 READY 没事,因为它的调用方按退避重试;SceneManager 的调用方不能拿"刚启动 / 夜里没人换图"
	//   当失败。
	//
	// 为什么 ③a 排在 ② 之后:实例崩溃后连接断开,通道回 IDLE;这里的 GetState(true) 把它踢去重连(返回时已是
	// CONNECTING)。对端拒连后通道进 TRANSIENT_FAILURE,退避期间后台重连也一直报 TRANSIENT_FAILURE、不回报
	// CONNECTING,直到真的连上(muduo_grpc/channel.cpp 的 OnBackoffTimer,与官方 pick_first 相同)。这个状态下的调用
	// 立即以 UNAVAILABLE 失败(不 wait_for_ready),有 ①② 时挑它等于白白失败;etcd 租约 60s 到期前死实例仍在注册表里,
	// 这正是要避开的窗口。只有 ①② 全空时才兜底挑它,结果与改造前相同。
	// SHUTDOWN 只在通道析构时出现,生产适配层又先经 HasUsableGrpcClient 滤掉,实际到不了这里;纯选择器仍把它归入 ③a,
	// 五态全覆盖。
	//
	// 纯选择器仍能描述无通道输入，供独立排序用例使用；生产适配层只传有完整传输的候选。
	// 无通道实体与真实连接暂时不可达不是同一状态：前者立即拒绝，后者仍走原有失败回调。
	// 入站握手同时拒绝纯 gRPC 类型，防止其按 uuid 抢占出站实体。
	//
	// 残余窗口:
	// - 实例刚崩、还没被踢过时它显示 IDLE,被踢起后到拒连报上来之前显示 CONNECTING,都在 ②。有 READY 实例就轮不到它;
	//   一个 READY 都没有时(例如空闲 GOAWAY 后刚被踢起来)它可能被挑中,那次调用排队等连接。拒连经 muduo 的连接失败
	//   回调(LOCAL DELTA,设计 §14.3)报给 Http2Transport,下一轮关闭,通道随即进 TRANSIENT_FAILURE、排队调用以
	//   UNAVAILABLE 完成每次调用回调,与官方 pick_first 相同。该回调在 Windows 上对拒连报哪些 poll 事件尚未实测(§14.3);
	//   万一没报上来,拒连会一直显示 CONNECTING 直到 connect_timeout(20s),表现同下一条。
	// - 对端主机整个消失(没有 RST)时,要等 connect_timeout(20s;系统 SYN 超时更早就以它为准),这段时间它停在
	//   CONNECTING,同样排在 READY 之后。一个 READY 都没有时被挑中的调用排队等连接,自己的截止时间先到就以
	//   DEADLINE_EXCEEDED 结束(SceneManager 的 deadline 10s,短于 20s,通常是这种),连接超时先到才以 UNAVAILABLE
	//   结束,都以非 OK 完成每次调用回调、同属"结果未知"(PlayerLifecycleSystem::DispatchEnterSceneTransportFailure 只记录 code,不按它分支)。
	//   官方通道的单次连接上限同为 20s,这一条与改造前相同。
	// - 对端主机在连接 READY 时静默消失:业务通道没有周期 ping,要等某个调用截止、且这条连接自该调用发出后一帧未收,
	//   才发 PING 探测(§4.4),keepalive_timeout(20s)内无 ACK 才断开回 IDLE;此前它一直是 READY、照常分到玩家。
	//   旧官方通道靠 30s 周期 ping 在无调用时也能发现,新通道必须有调用超时才会发现;etcd 租约 60s 到期摘除兜底。
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
		case muduo_grpc::ConnectivityState::kReady:
			return SelectionTier::kReady;
		case muduo_grpc::ConnectivityState::kIdle:
		case muduo_grpc::ConnectivityState::kConnecting:
			return SelectionTier::kConnectable;
		case muduo_grpc::ConnectivityState::kTransientFailure:
		case muduo_grpc::ConnectivityState::kShutdown:
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
	// (生产适配层不传无通道候选,③b 只出现在纯选择器用例里;万一对无通道实体发送,生成层同样本地以 UNAVAILABLE
	// 异步失败)。全挂期间每次换图都会走到这里,所以要节流:每 10s 最多一行,期间压掉的次数带在下一行里,
	// 排障时仍能看出频率。
	//
	// 线程模型:GetSceneManagerEntity 读的是 thread_local 的 tlsNodeContextManager,只在拥有它的节点主循环线程上
	// 调用(调用方都是 scene 主循环里的换图 / 跨 zone 交接 / 组队跟随 / RPC 应答处理;gRPC 应答由节点实体上的
	// muduo_grpc 通道在节点 EventLoop 上经 queueInLoop 投递,同样在这个线程)。节流状态与注册表同样做成 thread_local,
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
			else if (*candidate.channelState == muduo_grpc::ConnectivityState::kTransientFailure)
			{
				++transientFailure;
			}
			else if (*candidate.channelState == muduo_grpc::ConnectivityState::kShutdown)
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

	// 仅把真正拥有传输、且尚未关闭的实例交给选择器。无通道的握手实体不代表服务可用。
	// 实例数是个位数,每次现拼一张候选表;调用频率跟着玩家换图走,不是热路径。
	std::vector<scene_manager_selector::Candidate> candidates;
	for (const auto entity : smRegistry.view<NodeInfo>())
	{
		if (!HasUsableGrpcClient(smRegistry, entity))
		{
			continue;
		}
		const auto channel = FindGrpcChannel(smRegistry, entity);
		if (!channel)
		{
			continue;
		}
		scene_manager_selector::Candidate candidate{entity};
		candidate.channelState = channel->GetState(/*tryToConnect=*/true);
		candidates.push_back(candidate);
	}

	const auto picked = scene_manager_selector::Pick(candidates, playerId);
	if (picked == entt::null)
	{
		// 没有具备完整传输的 SceneManager。调用方记录自己的上下文。
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

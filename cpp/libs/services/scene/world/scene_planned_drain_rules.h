#pragma once

// scene 计划排空的纯规则(docs/design/scene-node-planned-drain.md §4.5、§4.7):配置解析、疏散限速、提前结束判定。
// 不依赖 entt / muduo / protobuf,单测见 cpp/tests/cross_zone_test/planned_drain_test.cpp。
// 编排在 cpp/nodes/scene/scene_planned_drain.cpp;疏散本身在 PlayerLifecycleSystem(BeginSceneDrain / PumpPlannedEvacuations)。

#include <algorithm>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <string_view>

namespace scene_planned_drain
{
// 自疏散时刻(从计划排空开始算,秒):过了它节点自己疏散还没走的人 —— 副本没打完、scene_manager 没赶上迁频道。
// 不设 = 预算的一半;大于预算按预算算(到预算就停机,再晚没有意义)。
inline constexpr const char *kSelfEvacuateAfterEnvVar = "SCENE_PLANNED_DRAIN_SELF_EVACUATE_AFTER_SECONDS";
// 计划排空期间每秒最多发起多少次疏散。不设 = kDefaultRelocationsPerSecond;0 = 不限。
inline constexpr const char *kRelocationsPerSecondEnvVar = "SCENE_PLANNED_DRAIN_RELOCATIONS_PER_SECOND";

// 进程缺省预算:没注入 MMORPG_PLANNED_DRAIN_BUDGET_SECONDS 时用(本机开发、自己起进程)。按原 scene 宽限期 60 秒减 25 秒。
inline constexpr std::chrono::seconds kDefaultBudget{35};
// 限速初值:没有压测基线,按「一台节点几千人在十几秒内搬完、后端不被打满」估,待压测校准(设计 §4.7)。
inline constexpr uint32_t kDefaultRelocationsPerSecond = 200;
// 连续这么久看不到能接住疏散的同类就提前结束:整 zone 停机、或全部同类都在排空时等下去只是空耗宽限期。
// 3 秒覆盖新 Pod 注册与 admission 改写经 etcd watch 传到本节点的延迟(毫秒到亚秒级)。
inline constexpr std::chrono::seconds kNoTargetGrace{3};

// 十进制无符号整数;空串、非数字、带符号或空白、超过 9 位一律 nullopt(部署只写整数,别的写法都是配错)。
inline std::optional<uint64_t> ParseUnsigned(const char *raw)
{
	if (raw == nullptr || raw[0] == '\0')
	{
		return std::nullopt;
	}
	const std::string_view text(raw);
	if (text.size() > 9)
	{
		return std::nullopt;
	}
	uint64_t value = 0;
	for (const char c : text)
	{
		if (c < '0' || c > '9')
		{
			return std::nullopt;
		}
		value = value * 10 + static_cast<uint64_t>(c - '0');
	}
	return value;
}

// 自疏散时刻。raw 合法就用它(不超过预算),否则预算的一半。
inline std::chrono::seconds SelfEvacuateAfter(const std::chrono::seconds budget, const char *raw)
{
	if (const auto parsed = ParseUnsigned(raw))
	{
		return (std::min)(std::chrono::seconds{static_cast<std::chrono::seconds::rep>(*parsed)}, budget);
	}
	return budget / 2;
}

// 每秒疏散上限。raw 合法就用它(0 = 不限),否则缺省值。
inline uint32_t RelocationsPerSecond(const char *raw)
{
	if (const auto parsed = ParseUnsigned(raw))
	{
		return static_cast<uint32_t>((std::min)(*parsed, static_cast<uint64_t>(UINT32_MAX)));
	}
	return kDefaultRelocationsPerSecond;
}

// 主世界类节点(与 go/scene_manager constants.IsWorldHostingType 同一口径):主世界 0、跨服主世界 2。
// 疏散的落点最终都在主世界里(原频道、同图频道、默认大世界),所以只有它们算「接得住」。
constexpr bool IsWorldHostingSceneNodeType(const uint32_t sceneNodeType)
{
	return sceneNodeType == 0 || sceneNodeType == 2;
}

// 发现快照里一台本 zone 的 scene 节点,在「能不能接住疏散」这件事上的视图。
struct Peer
{
	bool self{false};		   // 就是本进程
	bool closed{false};		   // 自报 admission = CLOSED(排空中)
	uint32_t sceneNodeType{0}; // NodeInfo.scene_node_type
};

// 有没有别的、开放的主世界类同类。
template <typename Range>
bool HasOpenWorldPeer(const Range &peers)
{
	for (const Peer &peer : peers)
	{
		if (!peer.self && !peer.closed && IsWorldHostingSceneNodeType(peer.sceneNodeType))
		{
			return true;
		}
	}
	return false;
}

// 疏散限速:令牌桶。每秒补 perSecond 个,最多攒 1 秒的量;perSecond = 0 = 不限。
// 只在 scene 逻辑线程上用,不加锁;时间由调用方传入(单调时钟,单测可注入)。
class Pacer
{
public:
	Pacer() = default;
	explicit Pacer(const uint32_t perSecond) : perSecond_(perSecond) {}

	// 本拍可以发起几个(不超过 waiting)。第一次调用只起算,不放行。
	std::size_t Take(const std::chrono::steady_clock::time_point now, const std::size_t waiting)
	{
		if (perSecond_ == 0)
		{
			return waiting;
		}
		if (last_.has_value() && now > *last_)
		{
			const double elapsed = std::chrono::duration<double>(now - *last_).count();
			tokens_ = (std::min)(static_cast<double>(perSecond_), tokens_ + elapsed * static_cast<double>(perSecond_));
		}
		last_ = now;
		const auto whole = static_cast<std::size_t>(tokens_);
		const std::size_t taken = (std::min)(whole, waiting);
		tokens_ -= static_cast<double>(taken);
		return taken;
	}

private:
	uint32_t perSecond_{0};
	double tokens_{0.0};
	std::optional<std::chrono::steady_clock::time_point> last_;
};

} // namespace scene_planned_drain

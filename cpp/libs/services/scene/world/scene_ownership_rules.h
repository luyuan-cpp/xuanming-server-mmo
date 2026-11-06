#pragma once

// scene 节点按控制面的期望状态核对本地场景归属:纯规则(缺口清单第 33 条)。
// 设计见 docs/design/scene-node-ownership-reconcile.md。本头不碰 ECS、不碰 Redis,单测直接喂读数。
//
// 期望状态的唯一事实源是 scene_manager 写在本 zone 控制域 Redis 里的 scene:{id}:node(值 = 属主 node_id 的十进制串)。
// 节点只读它,不写控制域任何键(control-plane-zone-scoping-20261105.md §4.3:zone z 的控制域键只由 zone z 的
// scene_manager 读写)。读到的属主不是本节点,而且满足下面两条,才把本地实体排空并销毁:
//   1. 连续 kNotMineConfirmations 次读到「不是本节点」(读失败不算证据,不改连击数);
//   2. 判定所用的那次读,发出时刻距宽限起点已满 kCreateGrace。宽限起点 = 本节点第一次核对到这个实体,
//      或最近一次 CreateScene 按 scene_id 命中它(SceneLifecycle::ReuseExistingScene 把核对状态整个摘掉、从头算)。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <string>
#include <string_view>

namespace scene_ownership
{
using Clock = std::chrono::steady_clock;

// 多久核对一次。失联超过屏障又回来的节点,重新注册之后约 kSweepInterval × kNotMineConfirmations 开始排空。
// 每轮每个节点发 ceil(本地场景数 / kMaxScenesPerRead) 条 MGET:一个控制域实例每秒要读的键数 = 同组全部场景数 / kSweepInterval。
inline constexpr std::chrono::seconds kSweepInterval{10};

// 连续几次读到「不归本节点」才动手。一次读数可能落在 scene_manager 两步写入的中间(改派的 CAS、拆除的认领),
// 两次相隔一个核对周期的读数都说不是,才当成稳定的期望状态。
inline constexpr std::uint8_t kNotMineConfirmations = 2;

// 宽限期。scene_manager 改派世界频道是「先建后改」:先在新节点 CreateScene,返回后立刻 CAS scene:{id}:node
// (rebalance、懒改派、init 的死节点改派、resolveScene 的死节点自愈四条路径都是这样;CAS 落空时 scene_manager 按那次
// 尝试的放置代次回收,docs/design/world-channel-placement-generation.md)。这段窗口里新节点上的实体已经建好,映射还指着
// 旧节点。窗口上界 = CreateScene 时限(init 5s,其余是请求 ctx)+ CAS + 落空回收 3s,宽限期取它的数倍。宽限期只拖慢
// 「刚建好就成了孤儿」的实体的回收;第 33 条的实体在节点失联前就建好了,早已过了宽限期。
inline constexpr std::chrono::seconds kCreateGrace{60};

// 一条 MGET 最多带几个场景。场景多时分批,单条命令的执行时间有上界,不长时间占住控制域实例。
inline constexpr std::size_t kMaxScenesPerRead = 256;

// scene_manager 写的场景属主键。跨语言契约:与 go/scene_manager/internal/logic/createscenelogic.go 的
// SceneNodeKeyFmt("scene:%d:node")是同一个键,两边改名必须同批改。
inline std::string SceneNodeKey(std::uint64_t sceneId)
{
	return "scene:" + std::to_string(sceneId) + ":node";
}

// 一个场景的 scene:{id}:node 读数。
struct OwnerReading
{
	enum class Kind : std::uint8_t
	{
		kValue,      // 读到一个属主串
		kMissing,    // 键不存在
		kUnreadable, // 整条命令失败、断线空应答、应答形状不对
	};
	Kind kind{Kind::kUnreadable};
	std::string value; // kind == kValue 时是属主串
};

// 一次读数说明了什么。
enum class Observation : std::uint8_t
{
	kSelf,      // 映射指向本节点
	kOtherNode, // 映射指向别的节点(被改派走了)
	kNoMapping, // 映射不存在(被拆除,或者从来没登记成功)
	kUnknown,   // 读不到,不当证据
	kCount
};

// 指标与日志用的名字,下标与 Observation 一一对应。长度由初始化列表推导,加了枚举项没加名字时 static_assert 失败。
inline constexpr std::string_view kObservationNames[] = {
	"self",       // Observation::kSelf
	"other_node", // Observation::kOtherNode
	"no_mapping", // Observation::kNoMapping
	"unknown",    // Observation::kUnknown
};
static_assert(std::size(kObservationNames) == static_cast<std::size_t>(Observation::kCount),
			  "kObservationNames 必须与 Observation 一一对应");

inline std::string_view ObservationName(Observation observation)
{
	const auto index = static_cast<std::size_t>(observation);
	return index < std::size(kObservationNames) ? kObservationNames[index] : std::string_view("invalid");
}

// selfNodeId 是本节点 node_id 的十进制串,与 scene_manager 写进映射的串同一种写法(strconv.FormatUint)。
// 不知道自己是谁(空串)时什么都判不了,一律 kUnknown。scene_manager 把空串当成没有属主,这里同样按 kNoMapping。
inline Observation Classify(const OwnerReading &reading, std::string_view selfNodeId)
{
	if (selfNodeId.empty())
	{
		return Observation::kUnknown;
	}
	switch (reading.kind)
	{
	case OwnerReading::Kind::kMissing:
		return Observation::kNoMapping;
	case OwnerReading::Kind::kValue:
		if (reading.value.empty())
		{
			return Observation::kNoMapping;
		}
		return reading.value == selfNodeId ? Observation::kSelf : Observation::kOtherNode;
	case OwnerReading::Kind::kUnreadable:
		break;
	}
	return Observation::kUnknown;
}

// 一个本地场景实体的核对状态。
struct CheckState
{
	Clock::time_point graceStartedAt{}; // 宽限起点,见文件头
	std::uint8_t notMineStreak{0};      // 连续读到「不归本节点」的次数,封顶 kNotMineConfirmations
};

enum class Decision : std::uint8_t
{
	kKeep,    // 留着(归本节点,或证据还不够)
	kRelease, // 不归本节点:排空并销毁本地实体
};

// 用一次读数推进状态。issuedAt 是这次读发出的时刻:读到的是发出之后某一刻的映射,按发出时刻判宽限更保守。
// kUnknown 不改连击数、不动手:读不到不是「不归本节点」的证据,也不推翻之前的证据。
// 判定为 kRelease 之后,只要后续读数仍不归本节点,每次都回 kRelease(销毁是幂等的,住户排空之前实体会留着)。
inline Decision Advance(CheckState &state, Observation observation, Clock::time_point issuedAt)
{
	switch (observation)
	{
	case Observation::kSelf:
		state.notMineStreak = 0;
		return Decision::kKeep;
	case Observation::kOtherNode:
	case Observation::kNoMapping:
		if (state.notMineStreak < kNotMineConfirmations)
		{
			++state.notMineStreak;
		}
		break;
	case Observation::kUnknown:
	case Observation::kCount:
		return Decision::kKeep;
	}
	const bool confirmed = state.notMineStreak >= kNotMineConfirmations;
	const bool graceOver = issuedAt - state.graceStartedAt >= kCreateGrace;
	return confirmed && graceOver ? Decision::kRelease : Decision::kKeep;
}
} // namespace scene_ownership

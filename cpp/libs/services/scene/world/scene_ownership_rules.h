#pragma once

// scene 节点按控制面的期望状态核对本地场景归属:纯规则(缺口清单第 33 条)。
// 设计见 docs/design/scene-node-ownership-reconcile.md。本头不碰 ECS、不碰 Redis,单测直接喂读数。
//
// 期望状态的事实源是 scene_manager 写在本 zone 控制域 Redis 里的两把键,节点只读、不写控制域任何键
// (control-plane-zone-scoping-20261105.md §4.3:zone z 的控制域键只由 zone z 的 scene_manager 读写):
//   - scene:{id}:node:场景的属主 node_id(十进制串);
//   - node:zone:{z}:{n}:holder:本节点号最近一次登记的进程("<etcd ModRevision>:<node_uuid>",§4.6)。
// 一个本地实体「不归本节点」= 映射不是本节点号,或者本节点号已被更新的登记接走(那时映射里的号指的是别的进程)。
// 满足下面两条才排空并销毁:
//   1. 连续 kNotMineConfirmations 次读到「不归本节点」(读失败不算证据,不改连击数);
//   2. 判定所用的那次读,发出时刻距宽限起点已满 kCreateGrace。宽限起点 = 本节点第一次核对到这个实体,
//      或最近一次 CreateScene 按 scene_id 命中它(SceneLifecycle::ReuseExistingScene 把核对状态整个摘掉、从头算)。
// 这两条都不看本节点的 etcd 身份:「映射不是我」不依赖 etcd 就成立,所以与 etcd 失联期间照样收敛(§4.5)。
// 开始排空按本轮的住户预算限速(kMaxRelocationsPerSweep),控制域丢数据时全 zone 也不会同一时刻一起改派(§4.7)。

#include <algorithm>
#include <charconv>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <optional>
#include <string>
#include <string_view>
#include <system_error>

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

// 本节点每轮核对最多新开始改派多少名住户(§4.7)。改派一名住户 = 一次退出存盘、一次交接标记、一次 EnterScene、
// 一次目标节点的认领载入。按住户而不按场景限:一个满员世界频道就有几百人。没有住户的场景不占预算,随时销毁。
// 本轮第一个有住户的场景即使超出预算也放行,保证进度。取值是保守的初值,没有压测基线,待按压测里 EnterScene 与
// go/db 的吞吐复核。
inline constexpr std::size_t kMaxRelocationsPerSweep = 200;

// scene_manager 写的场景属主键。跨语言契约:与 go/scene_manager/internal/logic/createscenelogic.go 的
// SceneNodeKeyFmt("scene:%d:node")是同一个键,两边改名必须同批改。
inline std::string SceneNodeKey(std::uint64_t sceneId)
{
	return "scene:" + std::to_string(sceneId) + ":node";
}

// scene_manager 写的节点号持有者键。跨语言契约:与 go/scene_manager/internal/logic/node_holder.go 的
// NodeHolderKeyFmt("node:zone:%d:%s:holder")是同一个键,两边改名必须同批改。
inline std::string NodeHolderKey(std::uint32_t zoneId, std::string_view nodeId)
{
	return "node:zone:" + std::to_string(zoneId) + ":" + std::string(nodeId) + ":holder";
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
	kSelf,       // 映射指向本节点
	kOtherNode,  // 映射指向别的节点(被改派走了)
	kNoMapping,  // 映射不存在(被拆除,或者从来没登记成功)
	kSuperseded, // 本节点号已被更新的登记接走:映射里的号指的是那个进程,本地实体全都不归本进程(§4.6)
	kUnknown,    // 读不到,不当证据
	kCount
};

// 指标与日志用的名字,下标与 Observation 一一对应。长度由初始化列表推导,加了枚举项没加名字时 static_assert 失败。
inline constexpr std::string_view kObservationNames[] = {
	"self",       // Observation::kSelf
	"other_node", // Observation::kOtherNode
	"no_mapping", // Observation::kNoMapping
	"superseded", // Observation::kSuperseded
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

// 本节点号此刻由谁持有(§4.6)。
enum class SlotHolder : std::uint8_t
{
	kThisProcessOrUnknown, // 持有者是本进程,或者证明不了别人更新:按映射判
	kAnotherProcess,       // 本节点号已被更新的登记接走:本地实体一律不归本进程
};

// 节点号持有者记录 "<etcd ModRevision>:<node_uuid>"。
struct HolderRecord
{
	std::uint64_t revision{0};
	std::string uuid;
};

// 解析持有者记录。修订号必须是不带符号的十进制、大于 0,uuid 非空;否则当作坏记录(nullopt)。
inline std::optional<HolderRecord> ParseHolderRecord(std::string_view value)
{
	const auto sep = value.find(':');
	if (sep == std::string_view::npos || sep == 0 || sep + 1 >= value.size())
	{
		return std::nullopt;
	}
	HolderRecord record;
	const char *begin = value.data();
	const char *end = value.data() + sep;
	const auto [ptr, ec] = std::from_chars(begin, end, record.revision);
	if (ec != std::errc() || ptr != end || record.revision == 0)
	{
		return std::nullopt;
	}
	record.uuid.assign(value.substr(sep + 1));
	return record;
}

// 判定本节点号是不是已被别的进程接走,跨多次读数记着本进程自己那次登记的修订号。
//
// 判据:持有者不是本进程,而且它的修订号比本进程那次登记更新。etcd 修订号全局单调,同一个号的新进程只能在本进程的分配键
// 过期之后登记,所以「别人、更新」就是证明。下面两种情况一律按映射判(kThisProcessOrUnknown),只会少拆、不会多拆:
//   - 本节点 etcd 身份确认有效:本进程此刻持有分配键,别的记录只能是登记之前留下的旧值;
//   - 还没见过本进程自己的记录(ownRevision == 0):比不出谁更新。scene_manager 处理本进程的 PUT 之后就会有。
// 读不到、记录不存在、记录损坏:没有信息。
struct HolderTracker
{
	std::uint64_t ownRevision{0}; // 见到过的本进程登记里最大的修订号;0 = 还没见到

	SlotHolder Observe(const OwnerReading &reading, std::string_view selfUuid, bool identityConfirmed)
	{
		if (reading.kind != OwnerReading::Kind::kValue || selfUuid.empty())
		{
			return SlotHolder::kThisProcessOrUnknown;
		}
		const auto record = ParseHolderRecord(reading.value);
		if (!record.has_value())
		{
			return SlotHolder::kThisProcessOrUnknown;
		}
		if (record->uuid == selfUuid)
		{
			ownRevision = std::max(ownRevision, record->revision);
			return SlotHolder::kThisProcessOrUnknown;
		}
		if (identityConfirmed || ownRevision == 0 || record->revision <= ownRevision)
		{
			return SlotHolder::kThisProcessOrUnknown;
		}
		return SlotHolder::kAnotherProcess;
	}
};

// 本轮的改派预算(§4.7)。一轮核对的全部批次共用一份。
struct ReleaseBudget
{
	std::size_t remainingResidents{kMaxRelocationsPerSweep};
	bool startedResidentScene{false}; // 本轮已经开始排空过有住户的场景

	// 这个场景本轮能不能开始排空;能的话扣掉它的住户数。没有住户的随时可以;有住户的在预算内可以;
	// 本轮第一个有住户的场景超出预算也放行(一个场景的住户比预算多时,不能永远排不动)。
	bool TryStart(std::size_t residents)
	{
		if (residents == 0)
		{
			return true;
		}
		if (startedResidentScene && residents > remainingResidents)
		{
			return false;
		}
		remainingResidents -= std::min(residents, remainingResidents);
		startedResidentScene = true;
		return true;
	}
};

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
	case Observation::kSuperseded:
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

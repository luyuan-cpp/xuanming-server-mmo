#pragma once

// battle 对外发布的可分配状态(R6,docs/design/global-match-hierarchical-pools.md §5.1):
// 把「此刻接不接新房间」写进 etcd 里本节点的 NodeInfo.admission(proto/common/base/common.proto 字段 14),
// match 选点时排除 CLOSED 的节点(go/match/internal/battleplace)。这只是提示:建房准入闸(battle_admission_gate.h)
// 与 Agones 分配许可仍是唯一的准入判定;提示过时最多让 match 白拨一次(battle_not_allocatable,无副作用,match 熔断该实例)。
//
// 为什么只在变化时写、并按回读对账,而不是周期写:
//   * 周期写的写放大与读量随进程数平方增长:每次写都推给全部 watcher(docs/handoff/server-gap-audit-20261101.md 第 4 条)。
//     这里稳态一次都不写;
//   * etcd 的 Put 失败是静默的:生成的客户端在 RPC 失败时不回调,EtcdService::OnPut 也只打日志。「写过」不等于「etcd 里是这个值」。
//     所以「已发布状态」取 watch 回读的本节点登记(发现快照里同 uuid 的 NodeInfo;battle 本来就 watch 自己的前缀做劫持检测
//     与注册自检,框架的注册健康监控也靠它),不一致才写;同一个值写出 kResendAfter 后回读仍不一致就补写。丢掉的写会自愈。
//
// 什么时候不写,只把期望值留在内存的 NodeInfo 里:
//   * 回读里还看不到自己的键:首次发布还没回来,或已失租。此刻 Put 会抢在注册链的 PutIfAbsent 之前把服务键建出来,
//     那条 CAS 失败后框架会重新分配 node_id(etcd_service.cpp OnTxnFailed)。首发与重注册都整份发布内存里的 NodeInfo,
//     带上的就是最新期望值,不需要另写;
//   * 注册链不空闲:租约未确认有效、正在重注册,或有在途的注册 CAS(EtcdManager::PendingTxnKey 非空)。理由同上。
//
// 与 battle_admission_gate.h 同一条纪律:不依赖 muduo / protobuf / 引擎,不打日志,单测独立编译
// (tests/battle_admission_gate_test.cpp)。线程模型:不加锁,只在 loop 线程调用(main.cpp 的对账定时器与关闸当场)。

#include <chrono>
#include <cstdint>
#include <optional>

#include "battle_admission_gate.h"

namespace battle_admission
{

// 发布出去的可分配状态,与 proto NodeAdmission 一一对应(映射在 main.cpp;本头不依赖 protobuf)。
enum class Advertised : uint8_t
{
	// 没有上报。不会是期望值,只会出现在回读里(不认识本字段的旧版本写的值,或更新版本才有的取值)。
	kUnspecified,
	// 接新房间(仍须过建房准入闸与 Agones 分配许可)。
	kOpen,
	// 不接新房间:启动未完成、Agones 排空或未就绪、停机或维护关闸之后。
	kClosed,
};

constexpr const char *ToString(const Advertised advertised)
{
	switch (advertised)
	{
	case Advertised::kUnspecified:
		return "unspecified";
	case Advertised::kOpen:
		return "open";
	case Advertised::kClosed:
		return "closed";
	}
	return "unknown";
}

// 本节点此刻是否接新房间:建房准入闸开着,且 Agones 会放行分配许可。
// lifecycleGrantsPermits 由调用方按 GameServerLifecycle 判定(BattleNodeImpl::LifecycleGrantsPermits),本头不依赖 Agones。
constexpr bool AcceptsNewRooms(const AdmissionPhase phase, const bool lifecycleGrantsPermits)
{
	return phase == AdmissionPhase::kOpen && lifecycleGrantsPermits;
}

// 期望发布的状态。只会是 kOpen / kClosed。
constexpr Advertised DesiredAdvertised(const AdmissionPhase phase, const bool lifecycleGrantsPermits)
{
	return AcceptsNewRooms(phase, lifecycleGrantsPermits) ? Advertised::kOpen : Advertised::kClosed;
}

// 一拍对账的决定。
enum class AdvertiseAction : uint8_t
{
	// 不写:已一致、回读里还看不到自己的键、注册链不空闲,或刚写过还在等回读。
	kNone,
	// 期望值与回读不一致,且这个值还没写过(或上次写的是别的值):写一次。
	kWrite,
	// 同一个值写出 kResendAfter 之后回读仍不一致:补写。上一次多半丢了(etcd 抖动、传输流重建),也可能是 watch 回读滞后;
	// 两种情况重写同一个值都无害。
	kResend,
};

// 可分配状态的发布决策(只存「上次写了什么、什么时候写的」)。由调用方每拍喂入期望值、回读与注册链状态,
// 返回 kWrite / kResend 时调用方写一次 etcd(EtcdManager::UpdateNodeInfo)。
class AdvertisePolicy
{
public:
	// 写出之后等待回读的时长;超过仍不一致就补写。远大于 watch 回读的正常延迟(毫秒级),
	// 也限制了 etcd 不可用期间的补写频率(每台节点每 5 秒至多一次,且只在不一致时)。
	static constexpr std::chrono::seconds kResendAfter{5};

	// desired:本拍期望(DesiredAdvertised);observed:etcd 回读,nullopt = 回读里还看不到自己的键;
	// registrationIdle:注册链空闲(租约确认有效、不在重注册、没有在途的注册 CAS);now:单调时钟。
	AdvertiseAction Decide(const Advertised desired, const std::optional<Advertised> observed, const bool registrationIdle,
						   const std::chrono::steady_clock::time_point now)
	{
		if (!observed.has_value() || *observed == desired || !registrationIdle)
		{
			return AdvertiseAction::kNone;
		}
		const bool resend = lastSent_ == desired;
		if (resend && now - lastSentAt_ < kResendAfter)
		{
			return AdvertiseAction::kNone;
		}
		lastSent_ = desired;
		lastSentAt_ = now;
		return resend ? AdvertiseAction::kResend : AdvertiseAction::kWrite;
	}

private:
	std::optional<Advertised> lastSent_;
	std::chrono::steady_clock::time_point lastSentAt_{};
};

} // namespace battle_admission

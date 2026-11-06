#pragma once

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>

#include "engine/core/type_define/type_define.h" // SessionId / kInvalidSessionId
#include "player_exit_intent.h"                  // player_exit::IsBoundSession

// 断线保留(docs/design/player-connection-lost-hold.md)—— 组件 + 纯常量与纯判定,不含 entt / redis / muduo,
// 不读任何时钟,可单测(cpp/tests/cross_zone_test 的 ConnectionLostHold*)。风格同 travel_freeze_cap.h / player_exit_intent.h。
// 粘合代码在 player_lifecycle.cpp:HandleClientConnectionLost(唯一写入口)、ResumeConnectionLostHold(EnterScene 接回)、
// ExpireConnectionLostHolds(1s 扫描)、HandleExitGameNode(任何退出都取代保留)。
//
// ── 做什么 ──
// 客户端连接意外断开时(gate 代发 ExitGame,reason = EXIT_GAME_REASON_DISCONNECTED),scene 不再立刻存盘销毁:
// 角色停在原地(清速度、广播停步,仍在场景与 AOI 里),留 kHoldWindow。窗口内同一角色的新会话进场
// (login 判 ShortReconnect / ReplaceLogin,scene_manager 按 location 派回原场景)直接接回这个实体,
// EnterScene 按"换会话"先离场再完整入场,新客户端拿到入场通知、自身与当前邻居;到期还没人接回,
// 按客户端断线退出,与原来断线即退出走同一条路(存盘、写断线释放标记、销毁)。
// 主动登出(LOGOUT)、租约到期关假死连接(LEASE_EXPIRED)、旧版 gate(UNSPECIFIED)仍立即退出。
//
// ── 时序(static_assert 守着,改任何一个常量都要重新过一遍)──
//   kLoginReconnectLease 30s 是 login 的断线租约(go/login sessionmanager.SetSessionDisconnecting 的
//   LeaseTtlSeconds;**两边必须同步改**):租约内同账号重登才判 ShortReconnect。租约到期后 player_locator 的
//   LeaseMonitor 调 LeaveScene;此时 location 的持有者若还没释放,scene_manager 只能先派 ReleasePlayer(session_ended)、
//   回"等持有者释放",这名玩家的清理要再等一个 claim 超时(约 30s)才完成,期间重登回"清理中"。
//   kHoldWindow 25 + kSweepInterval 1 + kReleaseMargin 4 ≤ 30:保留在租约到期之前结束,退出存盘有余量落地、写出断线
//   释放标记,租约到期时 LeaveScene 照常拿到释放证据当场删 location —— 窗口外的重登不比原来多等。
//   窗口与租约之间(25s~30s)到达的重连照常判 ShortReconnect:退出还在存盘就由 EnterScene 第 0 步取消退出、复用实体;
//   已销毁就按原来的路径从盘上重载。两种都正确,只是后一种慢一点。
//
// ── 保留期间 ──
//   * 实体照常周期存盘;进入保留时另请求一次关键进度存盘(与原来断线即存盘的落盘时机对齐,节点崩溃时不多丢)。
//   * 场景对旧会话的推送照发,gate 按会话 + 玩家身份栅栏丢弃(会话早已摘除);不改 AOI。
//   * 别的退出(租约到期的会话结束释放、排空 / 疏散、停机、交易封存)照常进来:HandleExitGameNode 摘掉保留,
//     并在退出意图上记 clientDisconnected(会话已死,改派票据作废)。
//   * 不做"重连成功"业务钩子:接回只是 EnterScene 的换会话入场,与 11-05 定的「连接层不做业务逻辑」一致。
namespace connection_lost_hold
{
	using Clock = std::chrono::steady_clock;

	// login 断线租约(镜像 go/login/internal/logic/pkg/sessionmanager/session_manager.go 的 LeaseTtlSeconds: 30)。
	inline constexpr std::chrono::seconds kLoginReconnectLease{30};

	// 断线后角色留在原地的时长(单调时钟,从 scene 收到断线通知起算)。
	inline constexpr std::chrono::seconds kHoldWindow{25};

	// 到期扫描周期(PlayerLifecycleSystem::ExpireConnectionLostHolds 的 1s 定时器)。
	inline constexpr std::chrono::seconds kSweepInterval{1};

	// 保留到期到租约到期之间留给退出存盘落地、写断线释放标记的余量。
	inline constexpr std::chrono::seconds kReleaseMargin{4};

	static_assert(kHoldWindow > std::chrono::seconds::zero(), "保留窗口必须为正");
	static_assert(kHoldWindow + kSweepInterval + kReleaseMargin <= kLoginReconnectLease,
				  "保留必须在 login 断线租约到期之前结束并留出退出存盘的余量,否则窗口外的重登要多等一个 claim 超时");

	// 收到断线通知时怎么处置。底层类型 uint8_t 数的是处置种类数(现 3 种)。纯内存使用,不进协议、不落库。
	enum class Decision : uint8_t
	{
		kHold,        // 停在原地,开始计时
		kKeepHolding, // 已在保留中(同一次断线的重复通知):不重新计时,窗口不因重复通知延长
		kExitNow,     // 立即按客户端断线退出(加保留之前的行为)

		kCount
	};

	constexpr std::size_t kDecisionCount = static_cast<std::size_t>(Decision::kCount);

	inline constexpr const char *kDecisionNames[] = {
		"hold",
		"keep_holding",
		"exit_now",
	};
	static_assert(std::size(kDecisionNames) == kDecisionCount, "kDecisionNames 必须与 Decision 一一对应");

	constexpr const char *DecisionName(Decision decision)
	{
		const auto index = static_cast<std::size_t>(decision);
		return index < kDecisionCount ? kDecisionNames[index] : "?";
	}

	// 为什么不保留、立即退出。只给日志与计数用;kNone = 可以保留。底层类型 uint8_t 数的是原因个数(现 4 个)。
	enum class ExitNowReason : uint8_t
	{
		kNone,
		kAlreadyExiting,  // 已在退出中(UnregisterPlayer):交给退出流程按 MergeExitCause 合并
		kHandoffInFlight, // 归属交接已发起(PlayerTravelHandoffComp.requestedAtMs != 0):去留由交接的应答与看门狗裁决
		kNodeEvacuating,  // 本节点正在整节点疏散(BeginEmergencyRelocateAll 之后):不在将死的节点上留人
		kNoBoundSession,  // 实体没有绑定会话:没有"断开的那条会话"可记,无从判断谁来接回

		kCount
	};

	constexpr std::size_t kExitNowReasonCount = static_cast<std::size_t>(ExitNowReason::kCount);

	inline constexpr const char *kExitNowReasonNames[] = {
		"none",
		"already_exiting",
		"handoff_in_flight",
		"node_evacuating",
		"no_bound_session",
	};
	static_assert(std::size(kExitNowReasonNames) == kExitNowReasonCount,
				  "kExitNowReasonNames 必须与 ExitNowReason 一一对应");

	constexpr const char *ExitNowReasonName(ExitNowReason reason)
	{
		const auto index = static_cast<std::size_t>(reason);
		return index < kExitNowReasonCount ? kExitNowReasonNames[index] : "?";
	}

	// 判定输入,全部由调用方从实体与节点状态读好传入(纯函数不碰 registry)。
	struct Facts
	{
		bool exiting{false};         // 实体挂着 UnregisterPlayer
		bool handoffInFlight{false}; // PlayerLifecycleSystem::IsHandoffRequested
		bool nodeEvacuating{false};  // 整节点疏散已开始
		bool sessionBound{false};    // 实体当前绑着会话(player_exit::IsBoundSession(快照会话))
		bool alreadyHeld{false};     // 实体已挂着 PlayerConnectionLostComp
	};

	// 不能保留的原因,按优先级取第一个。已在退出中排第一:退出流程有自己的合并规则,不能被保留打断。
	constexpr ExitNowReason ExitNowReasonOf(const Facts &facts)
	{
		if (facts.exiting)
		{
			return ExitNowReason::kAlreadyExiting;
		}
		if (facts.handoffInFlight)
		{
			return ExitNowReason::kHandoffInFlight;
		}
		if (facts.nodeEvacuating)
		{
			return ExitNowReason::kNodeEvacuating;
		}
		if (!facts.sessionBound)
		{
			return ExitNowReason::kNoBoundSession;
		}
		return ExitNowReason::kNone;
	}

	constexpr Decision Decide(const Facts &facts)
	{
		if (ExitNowReasonOf(facts) != ExitNowReason::kNone)
		{
			return Decision::kExitNow;
		}
		return facts.alreadyHeld ? Decision::kKeepHolding : Decision::kHold;
	}

	// 保留了 heldFor 之后是否到期。恰好等于窗口即到期。时钟不会倒退(steady_clock),负值按没到期处理。
	constexpr bool IsExpired(Clock::duration heldFor)
	{
		return heldFor >= kHoldWindow;
	}

	// 一次进场是不是"新会话接回":绑上的是一个有效会话,而且不是断开的那一条。断开的那条会话 gate 已经摘掉,
	// 它不会再带来进场;万一迟到的同会话进场到达,也不算接回,保留照常到期。
	constexpr bool IsResumedBy(SessionId incoming, SessionId lost)
	{
		return player_exit::IsBoundSession(incoming) && incoming != lost;
	}
} // namespace connection_lost_hold

// 断线保留中的角色(按需状态,docs/design/ecs-component-access-rules.md):没有这个组件 = 连接正常,或不在保留中。
// 挂:只在 PlayerLifecycleSystem::HandleClientConnectionLost(判定为 kHold 时,先 valid)。
// 摘:EnterScene 接回(ResumeConnectionLostHold)、HandleExitGameNode 发起任何原因的退出时;实体销毁随之消失。
// 读:try_get / any_of,缺失按"不在保留中"处理。纯内存:不进 PlayerAllData、不序列化、不跨进程。
struct PlayerConnectionLostComp
{
	// scene 收到断线通知的时刻(单调时钟)。到期按 connection_lost_hold::IsExpired(now - lostAt) 判。
	connection_lost_hold::Clock::time_point lostAt{};
	// 断开的那条会话(进入保留时实体快照里的会话)。接回判据见 connection_lost_hold::IsResumedBy。
	SessionId sessionId{kInvalidSessionId};
};

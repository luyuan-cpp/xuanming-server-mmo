#pragma once

#include <charconv>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <string_view>
#include <system_error>

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
		// 实体当前绑着会话(player_exit::IsBoundSession(快照会话)),且有 Guid。进入保留时会话已被摘掉,
		// 所以已在保留中的实体这一项恒为假 —— Decide 先看 alreadyHeld,不让重复通知落进 kNoBoundSession。
		bool sessionBound{false};
		bool alreadyHeld{false};     // 实体已挂着 PlayerConnectionLostComp
	};

	// 不能保留的原因,按优先级取第一个。已在退出中排第一:退出流程有自己的合并规则,不能被保留打断。
	// 只在 Decide 判 kExitNow 时用来打日志;对已在保留中的实体没有意义。
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

	// 顺序:已在退出中 → 立即退出(交给退出流程合并原因);已在保留中 → 保持原计时(重复通知不延长窗口,也不因
	// 会话已摘掉被当成"没有会话"退出);其余按 ExitNowReasonOf 判能不能保留。保留中的实体不会同时在退出中
	// (任何退出发起时都会摘掉保留),也发起不了交接(没有活会话),所以 alreadyHeld 排在这两项的哪一侧结论都一样。
	constexpr Decision Decide(const Facts &facts)
	{
		if (facts.exiting)
		{
			return Decision::kExitNow;
		}
		if (facts.alreadyHeld)
		{
			return Decision::kKeepHolding;
		}
		return ExitNowReasonOf(facts) == ExitNowReason::kNone ? Decision::kHold : Decision::kExitNow;
	}

	// 保留了 heldFor 之后是否到期。恰好等于窗口即到期。时钟不会倒退(steady_clock),负值按没到期处理。
	constexpr bool IsExpired(Clock::duration heldFor)
	{
		return heldFor >= kHoldWindow;
	}

	// 一次进场是不是"新的一次登录来接回":绑上的是一个有效会话,而且不是断开的那次登录 —— 会话号不同,或会话号相同而
	// 登录代次比断开时大。后一种是 gate 以同一 node_id 重启后会话序号从头发,新登录拿回了断开的那个号:只比会话号的话
	// 摘不掉保留,25 秒后保留到期,正在玩的玩家被按断线退出。判据与 EnterScene 第 1 步的"换会话"同一口径。
	// 断开的那条会话 gate 已经摘掉,它不会再带来进场;万一断开的那次登录迟到的进场到达(号相同、代次不大于断开时),
	// 也不算接回,保留照常到期。请求代次 0 = 上游没带,比不出新旧,同号时同样不算。
	constexpr bool IsResumedBy(SessionId incoming, uint64_t incomingLoginGeneration, SessionId lost,
							   uint64_t lostLoginGeneration)
	{
		return player_exit::IsBoundSession(incoming) &&
			   (incoming != lost || incomingLoginGeneration > lostLoginGeneration);
	}

	// ── 挂机月卡:离线托管(docs/design/player-connection-lost-hold.md §4.1)──
	// 开通了挂机(挂机月卡有效)的玩家断线后不按 kHoldWindow 退出,一直保留到重连、月卡到期或被别的退出取代;
	// 托管期间角色留在场景与 AOI 里,组队跟随照常把他带着走(team-follow-server-trail.md §3.6)。
	// 月卡事实源:玩家域 Redis 键 player:afk_pass:{<player_id>},值为到期的 Unix 秒(购买 / 开通流程写入,
	// 与 go/shared/redisslot AfkPassKey 同形)。player_locator 的租约监视在月卡有效期间每 5 分钟给断线会话续租
	// (go/player_locator/internal/logic/leasemonitor.go afkPassState),所以托管期间 login 的断线租约一直在,
	// 不受上面 kLoginReconnectLease 那条 static_assert 的约束;它只在看到月卡失效之后才让租约到期。

	// scene 侧已知的月卡状态。底层类型 uint8_t 数的是状态种类数(现 3 种)。
	enum class AfkPass : uint8_t
	{
		kUnknown,  // 还没查到结果(没查、在途或查询失败):按普通保留窗口计,窗口内每次扫描重查
		kInactive, // 确认不能托管:键不存在、值坏了、已经到期或剩下不到 kHostingExitLead
		kActive,   // 月卡有效,到期时刻是 PlayerConnectionLostComp::afkPassExpiresAtSec

		kCount
	};

	// 托管在月卡到期之前提前结束的余量,与普通保留同一口径(扫描周期 + 退出存盘余量):player_locator 只在月卡失效
	// 之后才让断线租约到期,提前这么多退出,租约到期时退出存盘已经落地、断线释放标记已写,LeaveScene 当场拿到证据。
	inline constexpr std::chrono::seconds kHostingExitLead = kSweepInterval + kReleaseMargin;

	// 月卡已知的到期时刻过去这么久仍确认不了续费(Redis 一直读不成),按到期处理、照常退出:
	// 查询失败时先保持托管(与 player_locator 查不出来时推迟清理同口径),但不让托管无限期挂着。
	inline constexpr std::chrono::seconds kHostingUnconfirmedGrace{30};

	// 解析一次读成功的月卡值。found = false 表示键不存在。值是十进制的到期 Unix 秒;解析不了按没有月卡
	// (与 player_locator 同口径)。剩余时间不超过 kHostingExitLead 也按不能托管:马上就该退了,不必开始或继续。
	// 返回 kActive 时 expiresAtSec 为到期时刻,其余为 0。
	inline AfkPass ParseAfkPass(bool found, std::string_view value, uint64_t nowUnixSec, uint64_t &expiresAtSec)
	{
		expiresAtSec = 0;
		if (!found || value.empty())
		{
			return AfkPass::kInactive;
		}
		uint64_t parsed = 0;
		const char *first = value.data();
		const char *last = first + value.size();
		const auto [end, error] = std::from_chars(first, last, parsed);
		if (error != std::errc{} || end != last)
		{
			return AfkPass::kInactive;
		}
		if (nowUnixSec + static_cast<uint64_t>(kHostingExitLead.count()) >= parsed)
		{
			return AfkPass::kInactive;
		}
		expiresAtSec = parsed;
		return AfkPass::kActive;
	}

	// 每次到期扫描对一个保留中的实体怎么处置。底层类型 uint8_t 数的是处置种类数(现 3 种)。
	enum class HoldVerdict : uint8_t
	{
		kKeep,  // 继续保留(普通窗口没到期,或托管中)
		kProbe, // 继续保留,并(再)查一次月卡
		kExit,  // 按客户端断线退出

		kCount
	};

	// 托管中、且离月卡到期还远(与下面 JudgeHold 判"托管继续"同一个条件):服务器可以替这名没有客户端的角色换场景
	// (组队跟随的托管落点,docs/design/player-connection-lost-hold.md §4.2)。还不知道有没有月卡的普通保留不算 ——
	// 它的 25 秒窗口建立在 login 断线租约 30 秒之上(上面的 static_assert),搬到别的节点会在那里重新开一个窗口、拖过租约;
	// 托管期间 player_locator 一直续租,没有这个顾虑。快到期的也不算:马上就该退了。
	constexpr bool IsHosting(bool hostingStarted, AfkPass pass, uint64_t passExpiresAtSec, uint64_t nowUnixSec)
	{
		return hostingStarted && pass == AfkPass::kActive &&
			   nowUnixSec + static_cast<uint64_t>(kHostingExitLead.count()) < passExpiresAtSec;
	}

	// 托管中、想跟队长却没接在队长身后的队员,服务器多久替他重新走一遍跟随检查(team-follow-server-trail.md §3.7)。
	// 客户端在线时它自己做这件事:没被驱动就每 5 秒重新声明一次跟随(客户端 TeamRegroupPolicy.RetrySeconds)。托管中没有
	// 客户端,由服务器兜底。正常路径(队长离开本节点、队长回到本节点的扇出、战斗结束、托管开始)都是当场触发,这里只补
	// 漏掉的信号,周期放宽到 15 秒:每次是一次组队读加至多两次 Redis GET,只对没接上的托管队员发。
	inline constexpr std::chrono::seconds kHostedFollowRecheckPeriod{15};

	// 这一秒要不要替这名托管中的角色重新检查一次跟随。按玩家号错开(玩家号是打散过的发号器值,取模均匀),同一节点上的
	// 托管角色不在同一秒集中发读;每名玩家每个周期恰好一秒到期。nowUnixSec 是墙钟 Unix 秒,由 1 秒扫描传入;扫描
	// 偶尔跳过一秒时那一轮顺延到下一个周期,只晚不错。
	constexpr bool IsHostedFollowRecheckDue(uint64_t playerId, uint64_t nowUnixSec)
	{
		return (playerId + nowUnixSec) % static_cast<uint64_t>(kHostedFollowRecheckPeriod.count()) == 0;
	}

	// 判定只看传入的事实:heldFor 是单调时钟的保留时长,nowUnixSec 是墙钟 Unix 秒(与月卡到期时刻同一口径)。
	//   托管中:离到期还远 → 保留;快到期(剩下不到 kHostingExitLead)→ 查一次是不是续了(ParseAfkPass 按续没续
	//           改成 kActive / kInactive);到期后 kHostingUnconfirmedGrace 仍确认不了 → 退出。
	//   不能托管 / 还不知道:按普通窗口,到期退出;还不知道且没有查询在途 → 窗口内再查一次。
	constexpr HoldVerdict JudgeHold(Clock::duration heldFor, AfkPass pass, uint64_t passExpiresAtSec,
									uint64_t nowUnixSec, bool probeInFlight)
	{
		if (pass == AfkPass::kActive)
		{
			if (nowUnixSec + static_cast<uint64_t>(kHostingExitLead.count()) < passExpiresAtSec)
			{
				return HoldVerdict::kKeep;
			}
			if (nowUnixSec >= passExpiresAtSec + static_cast<uint64_t>(kHostingUnconfirmedGrace.count()))
			{
				return HoldVerdict::kExit;
			}
			return probeInFlight ? HoldVerdict::kKeep : HoldVerdict::kProbe;
		}
		if (IsExpired(heldFor))
		{
			return HoldVerdict::kExit;
		}
		return pass == AfkPass::kUnknown && !probeInFlight ? HoldVerdict::kProbe : HoldVerdict::kKeep;
	}
} // namespace connection_lost_hold

// 断线保留中的角色(按需状态,docs/design/ecs-component-access-rules.md):没有这个组件 = 连接正常,或不在保留中。
// 挂:PlayerLifecycleSystem::HandleClientConnectionLost(判定为 kHold 时,先 valid);托管落点到了本节点、EnterScene 入场之后
//     (EnterHostedHold,player-connection-lost-hold.md §4.2:角色从别的节点搬来,在本节点从没有过活会话,按同一次断线重建)。
// 摘:EnterScene 接回(ResumeConnectionLostHold)、HandleExitGameNode 发起任何原因的退出时;实体销毁随之消失。
// 读:try_get / any_of,缺失按"不在保留中"处理。纯内存:不进 PlayerAllData、不序列化、不跨进程。
struct PlayerConnectionLostComp
{
	// scene 收到断线通知的时刻(单调时钟)。到期按 connection_lost_hold::IsExpired(now - lostAt) 判。
	connection_lost_hold::Clock::time_point lostAt{};
	// 断开的那条会话(进入保留时实体快照里的会话)。接回判据见 connection_lost_hold::IsResumedBy。
	SessionId sessionId{kInvalidSessionId};
	// 断开的那次登录的代次(进入保留时实体登录身份 PlayerLoginIdentityComp 的代次,没有登录身份时为 0)。
	// 重启后的 gate 会复用会话号,单凭会话号认不出拿回旧号的新登录,见 IsResumedBy。
	uint64_t loginGeneration{0};
	// ── 挂机月卡(离线托管,见 connection_lost_hold::JudgeHold)。只由 AfkHostingSystem 写(查询发出与回包)。──
	connection_lost_hold::AfkPass afkPass{connection_lost_hold::AfkPass::kUnknown};
	uint64_t afkPassExpiresAtSec{0}; // afkPass == kActive 时有效:月卡到期的 Unix 秒(Redis 原值)
	bool afkProbeInFlight{false};    // 有一次月卡查询在途:不重复发
	bool hostingStarted{false};      // 已按"托管开始"处理过(由服务器开启组队跟随只做一次)
};

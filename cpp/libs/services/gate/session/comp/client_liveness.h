#pragma once

// gate 客户端连接的存活判定(docs/design/gate-client-heartbeat.md)。纯逻辑:不做 I/O,不读时钟,时刻全由调用方传入。
//
// 一条客户端连接有两道期限,都由调用方 forceClose 收尾,之后照常走断开回调:
//   1. 握手期限:建连之后 handshakeTimeout 内必须验签。判据是「距建连的时长 + 仍未验签」,不看期间有没有发字节
//      (按「距上一次活动」算的话,对端隔几秒发一个字节就能永远占着槽位)。
//   2. 空闲超时:gate 在验签成功的回包里通告心跳参数,从这一刻起这条连接被「跟踪」:连续 idleTimeout 收不到它的
//      任何一帧就判它已死。客户端按通告每 interval 发一条心跳,gate 每条都回。
//
// 本文件只依赖 STL,不认识连接、会话表、protobuf,所以单测可以脱离 gate 进程编译
// (cpp/nodes/gate/tests/client_liveness_test.cpp)。装配与副作用(回包、通告、关连接、计数、日志)在
// cpp/nodes/gate/handler/rpc/client_heartbeat.cpp。
//
// 线程模型:gate 只有一个 EventLoop,会话表与本文件的状态都只在 loop 线程上读写,不加锁。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <queue>
#include <vector>

namespace gate_liveness
{

using SteadyTime = std::chrono::steady_clock::time_point;

struct ClientLivenessPolicy
{
	std::chrono::milliseconds handshakeTimeout; // 建连之后这么久还没验签就关
	std::chrono::milliseconds interval;         // 客户端发心跳的间隔;经验签回包通告给客户端
	std::chrono::milliseconds idleTimeout;      // 跟踪中的连接连续这么久收不到任何一帧就判死;同样通告给客户端
	std::chrono::milliseconds checkPeriod;      // 扫描节拍
	uint32_t maxHeartbeatsPerInterval;          // 一个 interval 窗口里最多应答几条;超出的不回,由调用方计非法包
};

// 合法条件:
//   - 时长都为正;节拍不长于心跳间隔,也短于握手期限(期限至少能被检查到一次);
//   - 判死阈值至少容下一个丢失的心跳;
//   - 每个窗口至少能应答两条(断网恢复后,TCP 会把积压的几条心跳一次送到);
//   - 通告给客户端的毫秒值要放得进 uint32。
constexpr bool IsValidLivenessPolicy(const ClientLivenessPolicy &policy)
{
	return policy.handshakeTimeout.count() > 0 && policy.interval.count() > 0 && policy.idleTimeout.count() > 0 &&
		   policy.checkPeriod.count() > 0 && policy.checkPeriod <= policy.interval &&
		   policy.checkPeriod < policy.handshakeTimeout && 2 * policy.interval < policy.idleTimeout &&
		   policy.idleTimeout.count() <= static_cast<std::chrono::milliseconds::rep>(UINT32_MAX) &&
		   policy.maxHeartbeatsPerInterval >= 2;
}

// gate 唯一的一份。心跳的两个值经验签回包通告,客户端照通告执行,以后要调只改这里;Java 版取同值。推导见设计 §4:
//   handshakeTimeout 10s:合法客户端建连后立即发验签,毫秒级完成;移动网络首个数据段丢一两次、重传也在几秒内。
//                    与 battle 直连的握手期限(battle_client_edge.h kHandshakeTimeoutSec)同值。
//   interval    10s:2 万连接的 gate 每秒约 2000 条心跳与回包;远短于运营商 NAT 的 TCP 空闲回收。
//   idleTimeout 30s = 3 × interval:正常节奏下要连着丢两个心跳、第三个也没准时到才判死;
//               能容下约 15s 的移动网络断网(TCP 重传指数退避,恢复后数据最长再隔十几秒才到)。
//               比节点链路的 15s 宽:客户端在公网与移动网络上,抖动大得多。
//   checkPeriod 1s:精度 ±1s;判死只会晚、不会早。
//   maxHeartbeatsPerInterval 3:容下断网恢复后一次到达的积压心跳;正常节奏与两倍速都碰不到。
inline constexpr ClientLivenessPolicy kClientLiveness{
	.handshakeTimeout = std::chrono::seconds(10),
	.interval = std::chrono::seconds(10),
	.idleTimeout = std::chrono::seconds(30),
	.checkPeriod = std::chrono::seconds(1),
	.maxHeartbeatsPerInterval = 3,
};
static_assert(IsValidLivenessPolicy(kClientLiveness));
static_assert(kClientLiveness.idleTimeout == 3 * kClientLiveness.interval,
			  "idleTimeout tolerates two lost heartbeats; see docs/design/gate-client-heartbeat.md section 4");

// 每会话状态,挂在 SessionInfo::liveness。纯内存,会话销毁即随之消失。验签与否看 SessionInfo::verified,这里不另存一份。
struct ClientLiveness
{
	// 正在按空闲超时判死:验签成功的回包已发出(或者没发验签请求就来了心跳)。只置真不复位,随会话结束。
	bool tracked{false};
	// 最近一次收到本连接任意一帧的时刻。只在跟踪中维护:开始跟踪时写入,之后每帧刷新。
	// 未跟踪时没人读它,调用方也就不必为它读时钟。
	SteadyTime lastInboundAt{};
	// 心跳频率窗口:窗口里第一条心跳的时刻,与本窗口已应答的条数。
	SteadyTime heartbeatWindowStart{};
	uint32_t heartbeatsInWindow{0};

	// 开始跟踪,从 now 起计静默。返回是否是这一次开始的:已在跟踪返回 false,调用方据此只登记一次检查点。
	bool StartTracking(const SteadyTime now)
	{
		if (tracked)
		{
			return false;
		}
		tracked = true;
		lastInboundAt = now;
		return true;
	}

	// 收到本连接的一帧(任何类型,含被拒绝的)。未跟踪时什么都不做。
	void NoteInbound(const SteadyTime now)
	{
		if (tracked)
		{
			lastInboundAt = now;
		}
	}
};

enum class HeartbeatAction : uint8_t
{
	kRejectUnverified,       // 验签之前发来:调用方断开连接
	kExcess,                 // 本窗口已应答满:不回,调用方计一次非法包
	kAnswer,                 // 回包
	kAnswerAndStartTracking, // 回包;这条连接此前没在跟踪,调用方随即登记空闲检查点(LivenessSweeper::Track)
};

// 收到一条心跳时调用。正常流程里验签成功时已开始跟踪,这里返回 kAnswer;kAnswerAndStartTracking 只出现在
// 客户端没发验签请求就发心跳的时候(dev 免验签模式下会话建连即已验证)。
inline HeartbeatAction AdmitHeartbeat(ClientLiveness &liveness, const bool verified, const SteadyTime now,
									  const ClientLivenessPolicy &policy)
{
	if (!verified)
	{
		return HeartbeatAction::kRejectUnverified;
	}
	// 窗口从窗口里第一条心跳算起,满 interval 换新窗口。正常节奏下每条心跳各开一个窗口。
	if (liveness.heartbeatsInWindow == 0 || now - liveness.heartbeatWindowStart >= policy.interval)
	{
		liveness.heartbeatWindowStart = now;
		liveness.heartbeatsInWindow = 0;
	}
	if (liveness.heartbeatsInWindow >= policy.maxHeartbeatsPerInterval)
	{
		return HeartbeatAction::kExcess;
	}
	++liveness.heartbeatsInWindow;
	return liveness.StartTracking(now) ? HeartbeatAction::kAnswerAndStartTracking : HeartbeatAction::kAnswer;
}

enum class IdleVerdict : uint8_t
{
	kAlive,    // 距上一帧不足 idleTimeout
	kDeferred, // 已满,但本次扫描迟到(本端 loop 卡过),约一个节拍后再判
	kDead,     // 已满:调用方 forceClose
};

// 恰好满 idleTimeout 即判死;差一点也算活着。
inline IdleVerdict JudgeIdle(const SteadyTime lastInboundAt, const SteadyTime now, const bool lateCheck,
							 const std::chrono::milliseconds idleTimeout)
{
	if (now - lastInboundAt < idleTimeout)
	{
		return IdleVerdict::kAlive;
	}
	return lateCheck ? IdleVerdict::kDeferred : IdleVerdict::kDead;
}

// 检查点堆:按到期时刻排序的 (到期时刻, session_id, 会话代次, 种类)。一条连接至多同时有两个检查点:
//   - 握手检查点:建连时登记,在建连 + handshakeTimeout 到期;到期时已验签就作废;
//   - 空闲检查点:开始跟踪时登记,之后每条跟踪中的连接恰好一个。到期时距上一帧不足 idleTimeout 就按
//     「上一帧 + idleTimeout」重新登记,已满就判死且不再登记,所以同一条连接只会被判死一次。
// 收包路径只刷新 ClientLiveness::lastInboundAt,不碰堆;每条连接大约每 idleTimeout 被检查一次,不按节拍全量遍历会话表。
// 会话关闭时不用摘检查点:到期时按 (session_id, 会话代次) 找不到就丢弃,session_id 被新连接复用也挡得住。
class LivenessSweeper
{
public:
	using Duration = SteadyTime::duration;

	enum class CheckKind : uint8_t
	{
		kHandshake,
		kIdle,
	};

	// lookup 返回的会话视图。verified 取自 SessionInfo::verified,liveness 指向 SessionInfo::liveness。
	struct SessionView
	{
		bool verified{false};
		const ClientLiveness *liveness{nullptr};
	};

	struct SweepResult
	{
		Duration checkGap{};           // 距上一次扫描的时长;首轮为 0
		bool late{false};              // 本次扫描迟到(checkGap > 2 × checkPeriod):这一轮只推迟,不关
		uint64_t stale{0};             // 丢弃的检查点:会话已关闭 / 代次不符 / 握手期限到时已验签 / 空闲检查点的会话没在跟踪
		uint64_t deferred{0};          // 期限已到但本次扫描迟到,改在约一个节拍后再判
		uint64_t handshakeTimeouts{0}; // 建连满 handshakeTimeout 仍未验签,已回调 onHandshakeTimeout
		uint64_t idleClosed{0};        // 跟踪中、静默满 idleTimeout,已回调 onIdle
	};

	explicit LivenessSweeper(const ClientLivenessPolicy &policy) : policy_(policy) {}

	// 建连时调用(会话还没验签时):建连 + handshakeTimeout 到期仍未验签就关。
	void ExpectHandshake(const uint32_t sessionId, const uint64_t incarnation, const SteadyTime connectedAt)
	{
		Schedule(connectedAt + policy_.handshakeTimeout, sessionId, incarnation, CheckKind::kHandshake);
	}

	// 开始跟踪一条连接,第一个空闲检查点在 lastInboundAt + idleTimeout。每条连接只调一次:
	// ClientLiveness::StartTracking 返回 true 的时候。
	void Track(const uint32_t sessionId, const uint64_t incarnation, const SteadyTime lastInboundAt)
	{
		Schedule(lastInboundAt + policy_.idleTimeout, sessionId, incarnation, CheckKind::kIdle);
	}

	// 取出全部到期(due <= now)的检查点逐个判定。
	// lookup(sessionId, incarnation) 返回 std::optional<SessionView>:会话已关闭或代次不符时为空。
	// onHandshakeTimeout(sessionId, incarnation)、onIdle(sessionId, incarnation, silentFor) 由调用方关连接。
	// 它们不能同步删会话(forceClose 是排队关闭,满足这一条)。
	template <typename Lookup, typename OnHandshakeTimeout, typename OnIdle>
	SweepResult Sweep(const SteadyTime now, Lookup &&lookup, OnHandshakeTimeout &&onHandshakeTimeout, OnIdle &&onIdle)
	{
		SweepResult result;
		// 距上一次扫描超过两个节拍,说明本端 loop 卡过:客户端的帧可能还在接收缓冲里没读
		// (Linux muduo 的 timerfd 与连接 fd 在同一次 epoll_wait 里返回、先后不定)。这一轮不关任何连接,
		// 改在约一个节拍后、积压的输入已经读过时再判。首轮扫描没有参照,不算迟到。
		if (lastSweepAt_)
		{
			result.checkGap = now - *lastSweepAt_;
			result.late = result.checkGap > 2 * policy_.checkPeriod;
		}
		lastSweepAt_ = now;
		// 新登记的检查点到期时刻都晚于 now,本轮不会再被取出。
		while (!checkpoints_.empty() && checkpoints_.top().due <= now)
		{
			const Checkpoint checkpoint = checkpoints_.top();
			checkpoints_.pop();
			const std::optional<SessionView> view = lookup(checkpoint.sessionId, checkpoint.incarnation);
			if (!view)
			{
				++result.stale;
				continue;
			}
			if (checkpoint.kind == CheckKind::kHandshake)
			{
				if (view->verified)
				{
					++result.stale; // 握手已完成,期限作废;验签成功时已另登记空闲检查点
					continue;
				}
				if (result.late)
				{
					++result.deferred;
					Schedule(now + policy_.checkPeriod, checkpoint.sessionId, checkpoint.incarnation, CheckKind::kHandshake);
					continue;
				}
				++result.handshakeTimeouts;
				onHandshakeTimeout(checkpoint.sessionId, checkpoint.incarnation);
				continue;
			}
			const ClientLiveness *liveness = view->liveness;
			if (liveness == nullptr || !liveness->tracked)
			{
				++result.stale;
				continue;
			}
			switch (JudgeIdle(liveness->lastInboundAt, now, result.late, policy_.idleTimeout))
			{
			case IdleVerdict::kAlive:
				Schedule(liveness->lastInboundAt + policy_.idleTimeout, checkpoint.sessionId, checkpoint.incarnation,
						 CheckKind::kIdle);
				break;
			case IdleVerdict::kDeferred:
				++result.deferred;
				Schedule(now + policy_.checkPeriod, checkpoint.sessionId, checkpoint.incarnation, CheckKind::kIdle);
				break;
			case IdleVerdict::kDead:
				++result.idleClosed;
				onIdle(checkpoint.sessionId, checkpoint.incarnation, Duration(now - liveness->lastInboundAt));
				break;
			}
		}
		return result;
	}

	// 堆里的检查点数,含已关闭会话、已验签连接尚未到期的那些。只用于观测(汇总日志),不参与判定。
	std::size_t PendingChecks() const { return checkpoints_.size(); }

	const ClientLivenessPolicy &policy() const { return policy_; }

private:
	struct Checkpoint
	{
		SteadyTime due;
		uint32_t sessionId;
		uint64_t incarnation;
		CheckKind kind;
	};
	// std::priority_queue 是大根堆,比较器反过来得到「最早到期的在堆顶」。
	struct DueLater
	{
		bool operator()(const Checkpoint &lhs, const Checkpoint &rhs) const { return lhs.due > rhs.due; }
	};

	void Schedule(const SteadyTime due, const uint32_t sessionId, const uint64_t incarnation, const CheckKind kind)
	{
		checkpoints_.push(Checkpoint{due, sessionId, incarnation, kind});
	}

	ClientLivenessPolicy policy_;
	std::priority_queue<Checkpoint, std::vector<Checkpoint>, DueLater> checkpoints_;
	std::optional<SteadyTime> lastSweepAt_;
};

} // namespace gate_liveness

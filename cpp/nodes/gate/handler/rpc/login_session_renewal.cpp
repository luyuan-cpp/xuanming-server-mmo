#include "login_session_renewal.h"

#include <algorithm>
#include <queue>
#include <vector>

#include "muduo/base/Logging.h"
#include "session/manager/session_manager.h"

namespace gate_login_renewal
{

namespace
{

// 一条到期登记。连接断开后不摘除:到期时按会话号与会话代次核对,对不上就丢弃。
// 一条会话同一时刻至多有一条登记(状态为 kQueued 时),发出之后到应答回来之前没有。
struct DueEntry
{
	SteadyTime dueAt{};
	SessionId sessionId{0};
	uint64_t incarnation{0};
};

// 最早到期的在堆顶。
struct DueLater
{
	bool operator()(const DueEntry &lhs, const DueEntry &rhs) const { return lhs.dueAt > rhs.dueAt; }
};

// 每隔这么久检查一次汇总行,累计计数有变化才打。
constexpr std::chrono::seconds kSummaryInterval{60};

thread_local std::priority_queue<DueEntry, std::vector<DueEntry>, DueLater> tlsDue;
thread_local uint64_t tlsInFlight = 0;
thread_local Counters tlsCounters;
thread_local Counters tlsLastLoggedCounters;
thread_local SteadyTime tlsNextSummaryAt{};
thread_local RequestSender tlsSender = nullptr;

SessionInfo *FindSession(const SessionId sessionId, const uint64_t incarnation)
{
	auto &sessions = tlsSessionManager.sessions();
	const auto it = sessions.find(sessionId);
	if (it == sessions.end() || it->second.incarnation != incarnation)
	{
		return nullptr;
	}
	return &it->second;
}

// 排一次续租。调用方负责把会话状态置成 kQueued。
void Enqueue(SessionInfo &session, const SessionId sessionId, const SteadyTime dueAt)
{
	session.loginRenewal = LoginRenewalState::kQueued;
	tlsDue.push(DueEntry{dueAt, sessionId, session.incarnation});
}

void LogSummaryIfDue(const SteadyTime now)
{
	if (now < tlsNextSummaryAt)
	{
		return;
	}
	tlsNextSummaryAt = now + kSummaryInterval;
	if (tlsCounters == tlsLastLoggedCounters)
	{
		return;
	}
	const Counters &c = tlsCounters;
	const bool failuresGrew = c.failed != tlsLastLoggedCounters.failed || c.notSent != tlsLastLoggedCounters.notSent;
	tlsLastLoggedCounters = tlsCounters;
	LOG_INFO << "[LoginRenewal] summary scheduled=" << c.scheduled << " requested=" << c.requested
			 << " renewed=" << c.renewed << " gone=" << c.gone << " rearmed=" << c.rearmed << " failed=" << c.failed
			 << " not_sent=" << c.notSent << " stale_reply=" << c.staleReply << " dropped=" << c.dropped
			 << " pending=" << tlsDue.size() << " in_flight=" << tlsInFlight;
	if (failuresGrew)
	{
		// 续租失败不影响在线玩家,但持续失败超过登录态寿命之后,这些连接不再计入设备数上限,值得看一眼 login 与路由服。
		LOG_WARN << "[LoginRenewal] renewals are failing and will be retried; login sessions keep counting down until one succeeds:"
				 << " failed=" << c.failed << " not_sent=" << c.notSent;
	}
}

void OnReply(const SessionId sessionId, const uint64_t incarnation, const RequestReply &reply, const SteadyTime now)
{
	if (tlsInFlight > 0)
	{
		--tlsInFlight;
	}
	SessionInfo *session = FindSession(sessionId, incarnation);
	if (session == nullptr)
	{
		// 连接已断开,或会话号已被新连接复用:这次应答不属于眼前这条连接。
		++tlsCounters.staleReply;
		return;
	}
	if (!reply.delivered)
	{
		// 结果未知。续租可以原样重试;在途期间是否重新登录过已经无所谓,下一次应答就是当时的真实状态。
		++tlsCounters.failed;
		Enqueue(*session, sessionId, now + kRetryBackoff);
		return;
	}
	if (reply.response.renewed())
	{
		++tlsCounters.renewed;
		Enqueue(*session, sessionId, now + NextInterval(reply.response.renew_after_seconds(), sessionId));
		return;
	}
	if (session->loginRenewal == LoginRenewalState::kInFlightRelogged)
	{
		// 请求在途期间这条连接又登录成功过(例如 LeaveGame 之后重新登录):这次「没有可续的记录」可能是在新的登录之前
		// 处理的,而两个应答到达 gate 的先后没有保证。马上再问一次,不能就此停下。
		++tlsCounters.rearmed;
		Enqueue(*session, sessionId, now);
		return;
	}
	// login 已没有这条连接的记录(离开游戏之后、已按断线处理、记录已过期):停止,直到这条连接再次登录成功。
	++tlsCounters.gone;
	session->loginRenewal = LoginRenewalState::kIdle;
}

} // namespace

void SetRequestSender(const RequestSender sender)
{
	tlsSender = sender;
}

void NoteLoginSucceeded(const SessionId sessionId, SessionInfo &session, const SteadyTime now)
{
	switch (session.loginRenewal)
	{
	case LoginRenewalState::kIdle:
		// 立刻到期、下一拍发出:第一次续租同时向 login 取间隔,gate 不需要知道登录态的寿命。
		++tlsCounters.scheduled;
		Enqueue(session, sessionId, now);
		return;
	case LoginRenewalState::kInFlight:
		session.loginRenewal = LoginRenewalState::kInFlightRelogged;
		return;
	case LoginRenewalState::kQueued:
	case LoginRenewalState::kInFlightRelogged:
		return; // 已经排着:到期时问到的就是这次登录之后的状态
	}
}

void Tick(const SteadyTime now)
{
	LogSummaryIfDue(now);
	if (tlsSender == nullptr)
	{
		return;
	}

	uint32_t budget = kMaxRequestsPerTick;
	while (budget > 0 && tlsInFlight < kMaxInFlight && !tlsDue.empty() && tlsDue.top().dueAt <= now)
	{
		const DueEntry entry = tlsDue.top();
		tlsDue.pop();
		SessionInfo *session = FindSession(entry.sessionId, entry.incarnation);
		if (session == nullptr || session->loginRenewal != LoginRenewalState::kQueued)
		{
			// 连接已断开,或会话号已被新连接复用。状态不是 kQueued 的登记不该存在,同样丢弃,不发请求。
			++tlsCounters.dropped;
			continue;
		}

		// 先记在途再发:出口可能同步调回(例如路由模式签名失败时直接回失败),回调里要看到一致的状态。
		session->loginRenewal = LoginRenewalState::kInFlight;
		++tlsInFlight;
		--budget;
		const bool sent = tlsSender(entry.sessionId, *session,
			[id = entry.sessionId, incarnation = entry.incarnation](const RequestReply &reply, SteadyTime receivedAt)
			{ OnReply(id, incarnation, reply, receivedAt); });
		if (!sent)
		{
			// 没发出:回调不会来,在这里收回在途并退避。出口只返回、不改会话表,session 仍有效。
			--tlsInFlight;
			++tlsCounters.notSent;
			Enqueue(*session, entry.sessionId, now + kRetryBackoff);
			continue;
		}
		++tlsCounters.requested;
	}
}

std::chrono::seconds NextInterval(const uint32_t renewAfterSeconds, const SessionId sessionId)
{
	const std::chrono::seconds base = std::clamp(std::chrono::seconds(renewAfterSeconds), kMinInterval, kMaxInterval);
	// 提前量只取决于会话号:同一条连接每个周期的间隔相同,不同连接的间隔不同,成批登录的连接几个周期之后自然错开。
	const uint64_t spread = static_cast<uint64_t>(base.count() / 5);
	return base - std::chrono::seconds(static_cast<std::chrono::seconds::rep>(sessionId % (spread + 1)));
}

const Counters &GetCounters()
{
	return tlsCounters;
}

std::size_t PendingRenewals()
{
	return tlsDue.size();
}

uint64_t InFlight()
{
	return tlsInFlight;
}

void ResetForTesting()
{
	tlsDue = {};
	tlsInFlight = 0;
	tlsCounters = Counters{};
	tlsLastLoggedCounters = Counters{};
	tlsNextSummaryAt = SteadyTime{};
}

} // namespace gate_login_renewal

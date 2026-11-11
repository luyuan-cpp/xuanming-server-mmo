#include "drain_migration.h"

#include <deque>

#include "login_admission.h"
#include "muduo/base/Logging.h"
#include "muduo/net/TcpConnection.h"
#include "session/manager/session_manager.h"

namespace gate_drain_migration
{

namespace
{

// 许可。只有 login 的排空判定会发,每个 login 副本每拍一条;续期就是把截止往后推。
struct LeaseState
{
	bool active{false};
	SteadyTime deadline{};
	uint32_t ratePerSecond{0};
	int64_t drainMarkedAt{0};
};

// 一条滞留关闭的登记。kRedirectLinger 是常量,先登记的先到期,队列按到期时刻有序。
struct LingerEntry
{
	SessionId sessionId{0};
	uint64_t incarnation{0};
	SteadyTime closeAt{};
};

// 迁移进行中时,每隔这么久打一行汇总。
constexpr std::chrono::seconds kSummaryInterval{10};

thread_local LeaseState tlsLease;
thread_local uint64_t tlsInFlight = 0;
thread_local std::deque<LingerEntry> tlsLinger;
thread_local Counters tlsCounters;
thread_local SteadyTime tlsNextSummaryAt{};
thread_local RequestSender tlsSender = nullptr;
thread_local RedirectPusher tlsPusher = nullptr;

void LogSummary(const char *stage)
{
	LOG_INFO << "[DrainMigration] " << stage << ": drain_marked_at=" << tlsLease.drainMarkedAt
			 << " rate_per_second=" << tlsLease.ratePerSecond << " in_flight=" << tlsInFlight
			 << " requested=" << tlsCounters.requested << " offered=" << tlsCounters.offered
			 << " no_target=" << tlsCounters.noTarget << " failed=" << tlsCounters.failed
			 << " not_sent=" << tlsCounters.notSent << " stale_reply=" << tlsCounters.staleReply
			 << " push_dropped=" << tlsCounters.pushDropped << " linger_closed=" << tlsCounters.lingerClosed;
}

void StopLease(const char *reason)
{
	if (!tlsLease.active)
	{
		return;
	}
	tlsLease.active = false;
	++tlsCounters.leasesStopped;
	LOG_INFO << "[DrainMigration] stopped: reason=" << reason;
	LogSummary("totals at stop");
}

// 推过 124、满 kRedirectLinger 仍连着的会话:关连接,之后照常走断开回调(断线保留)。
// 会话已断开或号已被新连接复用(代次不同)的登记直接丢弃。forceClose 排到下一轮执行,这里不会改会话表。
void SweepLinger(SteadyTime now)
{
	auto &sessions = tlsSessionManager.sessions();
	while (!tlsLinger.empty() && tlsLinger.front().closeAt <= now)
	{
		const LingerEntry entry = tlsLinger.front();
		tlsLinger.pop_front();
		const auto it = sessions.find(entry.sessionId);
		if (it == sessions.end() || it->second.incarnation != entry.incarnation)
		{
			continue;
		}
		if (const auto conn = it->second.conn.lock(); conn && conn->connected())
		{
			++tlsCounters.lingerClosed;
			LOG_INFO << "[DrainMigration] redirected session still connected after "
					 << std::chrono::duration_cast<std::chrono::seconds>(kRedirectLinger).count()
					 << "s; closing it, session_id=" << entry.sessionId;
			conn->forceClose();
		}
	}
}

void OnReply(SessionId sessionId, uint64_t incarnation, Guid expectedPlayer, Candidate expectedKind,
			 const RequestReply &reply, SteadyTime now)
{
	if (tlsInFlight > 0)
	{
		--tlsInFlight;
	}
	auto &sessions = tlsSessionManager.sessions();
	const auto it = sessions.find(sessionId);
	if (it == sessions.end() || it->second.incarnation != incarnation)
	{
		// 会话已断开,或号已被新连接复用:这张票不属于眼前这条连接。
		++tlsCounters.staleReply;
		return;
	}
	SessionInfo &session = it->second;
	session.drainMigrationInFlight = false;

	if (!reply.delivered)
	{
		++tlsCounters.failed;
		session.drainMigrationRetryAt = now + kRetryBackoff;
		return;
	}
	switch (reply.response.result())
	{
	case loginpb::GATE_MIGRATION_RESULT_OK:
		break;
	case loginpb::GATE_MIGRATION_RESULT_NOT_DRAINING:
		// login 不认为本 gate 在排空(标记已删、或本实例不在它的目录里):迟到的许可不能把人迁走。
		++tlsCounters.notDraining;
		StopLease("login replied NOT_DRAINING");
		return;
	case loginpb::GATE_MIGRATION_RESULT_DISABLED:
		++tlsCounters.disabled;
		StopLease("login replied DISABLED");
		return;
	case loginpb::GATE_MIGRATION_RESULT_NO_TARGET:
		// 同 zone 没有合格的目标(login 不回落):这条连接留在原地,稍后再试。
		++tlsCounters.noTarget;
		session.drainMigrationRetryAt = now + kRetryBackoff;
		return;
	default:
		++tlsCounters.failed;
		session.drainMigrationRetryAt = now + kRetryBackoff;
		return;
	}

	// 复核:应答在路上时,会话可能已被别的 124 改派、换了角色或进了切场景。状态变了就不推这张票,
	// 下一拍按新状态重新判(游戏中的票据绑定的是请求时的角色)。
	if (Classify(session, now) != expectedKind ||
		(expectedKind == Candidate::kInGame && session.playerId != expectedPlayer))
	{
		++tlsCounters.staleReply;
		return;
	}

	RedirectToGateNotify notify;
	notify.set_target_ip(reply.response.target_gate_ip());
	notify.set_target_port(reply.response.target_gate_port());
	notify.set_token_payload(reply.response.token_payload());
	notify.set_token_signature(reply.response.token_signature());
	notify.set_token_deadline(reply.response.token_deadline());
	if (tlsPusher != nullptr && tlsPusher(sessionId, expectedPlayer, notify, now) == PushOutcome::kPushed)
	{
		++tlsCounters.offered;
		return;
	}
	// 没推出去(连接已释放、身份不符,或没装出口):票作废,这条会话照常退避,不在下一拍立刻再要一张。
	// 推送出口只写连接、不改会话表,session 仍有效。
	++tlsCounters.pushDropped;
	session.drainMigrationRetryAt = now + kRetryBackoff;
}

} // namespace

Candidate Classify(const SessionInfo &session, SteadyTime now)
{
	if (!session.verified || !session.loginSucceeded || session.routerLane.Redirected() || session.redirectedAt.has_value())
	{
		return Candidate::kSkip;
	}
	// 路由 lane 还有在途或排队的请求时不迁,留到下一拍(client-request-ordering.md O9,2026-11-10 补):推 124 会让排队项不再发出
	// (逐条回 1003),排在慢请求后面的 LeaveGame 这类请求就白发了;在途那条的回包也还没回来。应答到达时 OnReply 按本函数复核,
	// 要票期间 lane 变忙同样不推。代价只是这条连接晚一拍迁:正常会话的 lane 绝大多数时刻是空的。
	if (session.routerLane.Depth() != 0)
	{
		return Candidate::kSkip;
	}
	if (gate_login_admission::HasBoundRole(session))
	{
		// 进场途中(路由已到、还没交给 scene)不迁:新连接上的重连会和这次进场交错。
		if (session.sceneId == 0 || session.pendingSceneEntry.has_value())
		{
			return Candidate::kSkip;
		}
		return Candidate::kInGame;
	}
	if (session.lastEnterGameAt.has_value() && now - *session.lastEnterGameAt < kEnterGameSettle)
	{
		return Candidate::kSkip;
	}
	return Candidate::kRoleSelect;
}

void SetRequestSender(RequestSender sender)
{
	tlsSender = sender;
}

void SetRedirectPusher(RedirectPusher pusher)
{
	tlsPusher = pusher;
}

void OnLease(const contracts::kafka::GateDrainMigrationEvent &event, SteadyTime now)
{
	++tlsCounters.leases;
	if (event.rate_per_second() == 0 || event.lease_ms() == 0)
	{
		StopLease("lease with zero rate or zero duration");
		return;
	}
	const bool starting = !tlsLease.active || tlsLease.drainMarkedAt != event.drain_marked_at();
	tlsLease.active = true;
	tlsLease.deadline = now + std::chrono::milliseconds(event.lease_ms());
	tlsLease.ratePerSecond = event.rate_per_second();
	tlsLease.drainMarkedAt = event.drain_marked_at();
	if (starting)
	{
		tlsNextSummaryAt = now + kSummaryInterval;
		LOG_INFO << "[DrainMigration] started: drain_marked_at=" << tlsLease.drainMarkedAt
				 << " rate_per_second=" << tlsLease.ratePerSecond << " lease_ms=" << event.lease_ms()
				 << " sessions=" << tlsSessionManager.sessions().size();
	}
}

void Tick(SteadyTime now)
{
	SweepLinger(now);
	if (!tlsLease.active)
	{
		return;
	}
	if (now >= tlsLease.deadline)
	{
		// login 不再续期:排空已取消、已排空,或迁移开关关了。
		StopLease("lease expired");
		return;
	}
	if (now >= tlsNextSummaryAt)
	{
		tlsNextSummaryAt = now + kSummaryInterval;
		LogSummary("progress");
	}
	if (tlsSender == nullptr)
	{
		return;
	}

	const uint64_t inFlightCap = 2ull * tlsLease.ratePerSecond;
	uint32_t budget = tlsLease.ratePerSecond;
	for (auto &[sessionId, session] : tlsSessionManager.sessions())
	{
		if (budget == 0 || tlsInFlight >= inFlightCap)
		{
			break;
		}
		if (session.drainMigrationInFlight || now < session.drainMigrationRetryAt)
		{
			continue;
		}
		const Candidate kind = Classify(session, now);
		if (kind == Candidate::kSkip)
		{
			continue;
		}
		const Guid expectedPlayer = kind == Candidate::kInGame ? session.playerId : 0;

		// 先记在途再发:出口可能同步调回(例如没有通道时直接回失败),回调里要看到一致的状态。
		session.drainMigrationInFlight = true;
		++tlsInFlight;
		--budget;
		// 结构化绑定用 init-capture 拷一份(MSVC 对直接捕获结构化绑定的支持不稳,见 copilot-instructions 的 C3495 一节)。
		const bool sent = tlsSender(sessionId, session,
			[id = sessionId, incarnation = session.incarnation, expectedPlayer, kind](const RequestReply &reply,
																					SteadyTime receivedAt)
			{ OnReply(id, incarnation, expectedPlayer, kind, reply, receivedAt); });
		if (!sent)
		{
			// 没发出:回调不会来,在这里收回在途状态并退避。出口只返回、不改会话表,session 仍有效。
			session.drainMigrationInFlight = false;
			--tlsInFlight;
			session.drainMigrationRetryAt = now + kRetryBackoff;
			++tlsCounters.notSent;
			continue;
		}
		++tlsCounters.requested;
	}
}

void NoteEnterGameForwarded(SessionInfo &session, SteadyTime now)
{
	session.lastEnterGameAt = now;
}

void NoteRedirected(SessionId sessionId, SessionInfo &session, SteadyTime now)
{
	if (session.redirectedAt.has_value())
	{
		return;
	}
	session.redirectedAt = now;
	tlsLinger.push_back(LingerEntry{sessionId, session.incarnation, now + kRedirectLinger});
}

bool LeaseActive(SteadyTime now)
{
	return tlsLease.active && now < tlsLease.deadline;
}

const Counters &GetCounters()
{
	return tlsCounters;
}

void ResetForTesting()
{
	tlsLease = LeaseState{};
	tlsInFlight = 0;
	tlsLinger.clear();
	tlsCounters = Counters{};
	tlsNextSummaryAt = SteadyTime{};
}

} // namespace gate_drain_migration

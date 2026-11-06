#include "client_heartbeat.h"

#include <optional>

#include "gate_codec.h"
#include "message_limiter/illegal_packet_counter.h"
#include "muduo/base/Logging.h"
#include "session/manager/session_manager.h"

namespace gate_client_heartbeat
{
namespace
{

using gate_liveness::HeartbeatAction;
using gate_liveness::LivenessSweeper;
using gate_liveness::SteadyTime;

constexpr std::chrono::seconds kSummaryInterval{30};

thread_local LivenessSweeper tlsSweeper{gate_liveness::kClientLiveness};
thread_local Counters tlsCounters;
// 汇总行:上一次检查的时刻,与上一次打出时的计数(计数没变就不打)。
thread_local std::optional<SteadyTime> tlsLastSummaryAt;
thread_local Counters tlsLastLoggedCounters;

// 与 GateClientSessionHandler::GetSessionId 同一份上下文,但不抛异常、不打日志:
// 这里的调用频率由公网客户端决定。没有会话号的连接取 kInvalidSessionId,找不到会话。
SessionId SessionIdOf(const muduo::net::TcpConnectionPtr &conn)
{
	const auto *sessionId = boost::any_cast<SessionId>(&conn->getContext());
	return sessionId != nullptr ? *sessionId : kInvalidSessionId;
}

// 采样日志:reason 是进程内常量,不含客户端输入。频率由公网客户端决定,每 1024 次留一条趋势证据。
// total 是该原因已累计的次数(含本次)。
void LogSampled(const char *reason, const SessionId sessionId, const uint64_t total)
{
	if ((total & 0x3FF) == 1)
	{
		LOG_WARN << "[ClientHeartbeat] connection rejected (sampled), reason=" << reason
				 << ", latest_session_id=" << sessionId << ", rejected_total=" << total;
	}
}

SessionInfo *FindSession(const uint32_t sessionId, const uint64_t incarnation)
{
	auto &sessions = tlsSessionManager.sessions();
	const auto it = sessions.find(sessionId);
	if (it == sessions.end() || it->second.incarnation != incarnation)
	{
		return nullptr;
	}
	return &it->second;
}

void ForceClose(SessionInfo &session)
{
	if (const auto conn = session.conn.lock())
	{
		conn->forceClose();
	}
}

// 握手期限到了还没验签:只关连接。没验签的会话没有角色、没向 login 发过登录,断开回调只删会话。
void CloseHandshakeTimeout(const uint32_t sessionId, const uint64_t incarnation)
{
	SessionInfo *session = FindSession(sessionId, incarnation);
	if (session == nullptr)
	{
		return; // 同一次扫描里刚核对过,不会走到这里
	}
	++tlsCounters.handshakeTimeouts;
	LogSampled("handshake_timeout", sessionId, tlsCounters.handshakeTimeouts);
	ForceClose(*session);
}

// 静默满阈值:只关连接。断开回调随后照常代发 ExitGame(DISCONNECTED)、通知 login、清 lane 与观战订阅、删会话。
void CloseIdleSession(const uint32_t sessionId, const uint64_t incarnation, const SteadyTime::duration silentFor)
{
	SessionInfo *session = FindSession(sessionId, incarnation);
	if (session == nullptr)
	{
		return; // 同一次扫描里刚核对过,不会走到这里
	}
	++tlsCounters.idleClosed;
	const auto silentMs = std::chrono::duration_cast<std::chrono::milliseconds>(silentFor).count();
	const auto timeoutMs = gate_liveness::kClientLiveness.idleTimeout.count();
	// 客户端掉线是正常事件,不是服务端故障,打 INFO。绑定了角色的连接逐条打(排查「这个玩家为什么掉线」要用),
	// 与断开日志同一口径;未绑定角色的连接数量由公网流量决定,采样。
	const bool boundPlayer = session->playerId != 0 && session->playerId != kInvalidGuid;
	if (boundPlayer)
	{
		LOG_INFO << "[ClientHeartbeat] idle timeout, forcing close: session_id=" << sessionId
				 << ", player_id=" << session->playerId << ", silent_ms=" << silentMs
				 << ", heartbeat_timeout_ms=" << timeoutMs << ", idle_closed_total=" << tlsCounters.idleClosed;
	}
	else
	{
		static uint64_t unboundClosedCount = 0;
		if ((unboundClosedCount++ & 0x3FF) == 0)
		{
			LOG_INFO << "[ClientHeartbeat] idle timeout on unbound sessions (sampled), forcing close: latest_session_id="
					 << sessionId << ", silent_ms=" << silentMs << ", heartbeat_timeout_ms=" << timeoutMs
					 << ", unbound_closed_total=" << unboundClosedCount
					 << ", idle_closed_total=" << tlsCounters.idleClosed;
		}
	}
	ForceClose(*session);
}

void StartTracking(const SessionId sessionId, SessionInfo &session, const SteadyTime now)
{
	if (session.liveness.StartTracking(now))
	{
		++tlsCounters.tracked;
		tlsSweeper.Track(sessionId, session.incarnation, now);
	}
}

void LogSummaryIfDue(const SteadyTime now)
{
	if (tlsLastSummaryAt && now - *tlsLastSummaryAt < kSummaryInterval)
	{
		return;
	}
	tlsLastSummaryAt = now;
	if (tlsCounters == tlsLastLoggedCounters)
	{
		return;
	}
	tlsLastLoggedCounters = tlsCounters;
	const Counters &c = tlsCounters;
	// 字段名是与 tools/scripts/stress_summarize.ps1 之间的契约,改名要两边一起改。
	LOG_INFO << "[ClientHeartbeat] summary answered=" << c.answered << " excess=" << c.excess
			 << " before_verify=" << c.beforeVerify << " tracked=" << c.tracked << " idle_closed=" << c.idleClosed
			 << " handshake_timeouts=" << c.handshakeTimeouts << " deferred=" << c.deferred
			 << " pending_checks=" << tlsSweeper.PendingChecks();
}

} // namespace

void ExpectHandshake(const SessionId sessionId, const SessionInfo &session, const SteadyTime now)
{
	tlsSweeper.ExpectHandshake(sessionId, session.incarnation, now);
}

void OnTokenVerified(const SessionId sessionId, SessionInfo &session, ClientTokenVerifyResponse &response,
					 const SteadyTime now)
{
	const auto &policy = gate_liveness::kClientLiveness;
	response.set_heartbeat_interval_ms(static_cast<uint32_t>(policy.interval.count()));
	response.set_heartbeat_timeout_ms(static_cast<uint32_t>(policy.idleTimeout.count()));
	// 回包里告诉了客户端「每 10s 一条心跳」,从这一刻起就按 30s 判死。客户端在收到回包后立即发第一条心跳。
	StartTracking(sessionId, session, now);
}

void OnHeartbeatRequest(const muduo::net::TcpConnectionPtr &conn, const std::shared_ptr<ClientHeartbeatRequest> &request,
						muduo::Timestamp)
{
	// muduo 的 receiveTime 是墙钟,不能用来算超时;这里取单调时钟。
	HandleHeartbeat(conn, *request, std::chrono::steady_clock::now());
}

void HandleHeartbeat(const muduo::net::TcpConnectionPtr &conn, const ClientHeartbeatRequest &request, const SteadyTime now)
{
	const SessionId sessionId = SessionIdOf(conn);
	auto &sessions = tlsSessionManager.sessions();
	const auto sessionIt = sessions.find(sessionId);
	if (sessionIt == sessions.end())
	{
		// 与 ClientRequest 找不到会话同一处置(client_session_handler.cpp DispatchClientRpcMessage)。
		static uint64_t missingSessionCount = 0;
		LogSampled("heartbeat_missing_session", sessionId, ++missingSessionCount);
		conn->forceClose();
		return;
	}
	SessionInfo &session = sessionIt->second;
	session.liveness.NoteInbound(now);

	switch (gate_liveness::AdmitHeartbeat(session.liveness, session.verified, now, gate_liveness::kClientLiveness))
	{
	case HeartbeatAction::kRejectUnverified:
		// 与验签之前发 ClientRequest 同一处置:验签之前除了验签请求,什么都不收。
		++tlsCounters.beforeVerify;
		LogSampled("heartbeat_before_token_verify", sessionId, tlsCounters.beforeVerify);
		conn->forceClose();
		return;
	case HeartbeatAction::kExcess:
		// 正常客户端每 10s 一条,断网恢复后积压的几条也在窗口上限之内。超出的不回,与超限、坏号、限流同一个非法包计数。
		++tlsCounters.excess;
		LogSampled("heartbeat_over_rate", sessionId, tlsCounters.excess);
		if (IllegalPacketCounter::RegisterAndShouldKill(session.illegalPacketCount))
		{
			LOG_WARN << "Session illegal-packet threshold exceeded (heartbeat over rate) -- forceClose."
					 << " count=" << session.illegalPacketCount << " session_id=" << sessionId;
			conn->forceClose();
		}
		return;
	case HeartbeatAction::kAnswerAndStartTracking:
		// 没发验签请求就来了心跳(dev 免验签模式下会话建连即已验证):从这一条起按空闲超时判死。
		++tlsCounters.tracked;
		tlsSweeper.Track(sessionId, session.incarnation, now);
		break;
	case HeartbeatAction::kAnswer:
		break;
	}

	ClientHeartbeatResponse response;
	response.set_seq(request.seq());
	GetGateCodec().send(conn, response);
	++tlsCounters.answered;
}

void SweepLiveness(const SteadyTime now)
{
	const auto result = tlsSweeper.Sweep(
		now,
		[](const uint32_t sessionId, const uint64_t incarnation) -> std::optional<LivenessSweeper::SessionView>
		{
			const SessionInfo *session = FindSession(sessionId, incarnation);
			if (session == nullptr)
			{
				return std::nullopt;
			}
			return LivenessSweeper::SessionView{.verified = session->verified, .liveness = session->liveness};
		},
		[](const uint32_t sessionId, const uint64_t incarnation) { CloseHandshakeTimeout(sessionId, incarnation); },
		[](const uint32_t sessionId, const uint64_t incarnation, const SteadyTime::duration silentFor)
		{ CloseIdleSession(sessionId, incarnation, silentFor); });

	if (result.deferred > 0)
	{
		// gate 的 loop 卡过 2s 以上才会走到这里,本身就是该看的服务端问题。一次卡顿只打一行,不按连接打。
		tlsCounters.deferred += result.deferred;
		LOG_WARN << "[ClientHeartbeat] liveness verdicts deferred, local loop stalled: check_gap_ms="
				 << std::chrono::duration_cast<std::chrono::milliseconds>(result.checkGap).count()
				 << ", deferred=" << result.deferred << ", deferred_total=" << tlsCounters.deferred;
	}
	LogSummaryIfDue(now);
}

const Counters &GetCounters()
{
	return tlsCounters;
}

std::size_t PendingChecks()
{
	return tlsSweeper.PendingChecks();
}

} // namespace gate_client_heartbeat

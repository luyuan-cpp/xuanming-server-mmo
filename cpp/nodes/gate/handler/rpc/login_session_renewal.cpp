#include "login_session_renewal.h"

#include <algorithm>
#include <queue>
#include <unordered_map>
#include <utility>
#include <vector>

#include "metrics/metrics_registry.h"
#include "muduo/base/Logging.h"
#include "session/manager/session_manager.h"

namespace gate_login_renewal
{

static_assert(kMaxPendingDisconnects == SessionIdGenerator::kSeqMask,
			  "the pending disconnect cap is one gate process's session id space (see the header)");

namespace
{

// 一条到期登记。续租:连接断开后不摘除,到期时按会话号与会话代次核对,对不上就丢弃;一条会话同一时刻至多有一条登记
// (状态为 kQueued 时),发出之后到应答回来之前没有。断线通知:按会话代次找待送达表,规则见 PendingDisconnect。
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

// 一条待送达的断线通知。在表里的通知要么在途(inFlight),要么在 tlsDisconnectDue 里恰好有一条登记;login 确认才移出。
struct PendingDisconnect
{
	DisconnectNotice notice;
	SteadyTime closedAt{};
	uint32_t attempts{0}; // 已发起的次数,含没发出的
	bool inFlight{false};
	bool overdueLogged{false};
};

// 每隔这么久检查一次汇总行,累计计数有变化才打。
constexpr std::chrono::seconds kSummaryInterval{60};

thread_local std::priority_queue<DueEntry, std::vector<DueEntry>, DueLater> tlsDue;
thread_local uint64_t tlsInFlight = 0;
// 本拍还能发起的请求数,续租与断线通知共用;Tick 开头重置,两拍之间当场发出的断线通知也从里面扣。
thread_local uint32_t tlsBudgetLeft = kMaxRequestsPerTick;
// 会话代次 → 待送达的断线通知。代次在 gate 进程内单调、不复用,会话号复用之后新旧两条连接的通知也不会互相覆盖。
thread_local std::unordered_map<uint64_t, PendingDisconnect> tlsDisconnects;
thread_local std::priority_queue<DueEntry, std::vector<DueEntry>, DueLater> tlsDisconnectDue;
thread_local Counters tlsCounters;
thread_local Counters tlsLastLoggedCounters;
thread_local SteadyTime tlsNextSummaryAt{};
thread_local RequestSender tlsSender = nullptr;
thread_local DisconnectSender tlsDisconnectSender = nullptr;

int64_t MillisSince(const SteadyTime since, const SteadyTime now)
{
	return std::chrono::duration_cast<std::chrono::milliseconds>(now - since).count();
}

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
			 << " pending=" << tlsDue.size() << " in_flight=" << tlsInFlight
			 << " disconnect_queued=" << c.disconnectQueued << " disconnect_sent=" << c.disconnectSent
			 << " disconnect_retried=" << c.disconnectRetried << " disconnect_delivered=" << c.disconnectDelivered
			 << " disconnect_failed=" << c.disconnectFailed << " disconnect_not_sent=" << c.disconnectNotSent
			 << " disconnect_overdue=" << c.disconnectOverdue << " disconnect_dropped=" << c.disconnectDropped
			 << " disconnect_pending=" << tlsDisconnects.size();
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

// 这次没成(调用失败、login 回错误、没发出):按退避排下一次。
void ScheduleDisconnectRetry(const PendingDisconnect &pending, const SteadyTime now)
{
	tlsDisconnectDue.push(DueEntry{now + DisconnectRetryBackoff(pending.attempts, pending.notice.incarnation),
								   pending.notice.sessionId, pending.notice.incarnation});
}

// 待送达满 kDisconnectOverdue 的通知打一次 ERROR。在每次发起之前查:一直失败的通知最慢一个封顶间隔(30 秒出头)之内打出来,
// 积压在预算后面、一次都还没轮上的通知轮到它时打出来。日志量受每拍预算约束,每秒至多 kMaxRequestsPerTick 行。
void NoteIfOverdue(PendingDisconnect &pending, const SteadyTime now)
{
	if (pending.overdueLogged || now - pending.closedAt < kDisconnectOverdue)
	{
		return;
	}
	pending.overdueLogged = true;
	++tlsCounters.disconnectOverdue;
	LOG_ERROR << "[LoginDisconnect] Disconnect not confirmed by login " << MillisSince(pending.closedAt, now)
			  << " ms after the connection closed; the player stays ONLINE in player_locator until it lands, still retrying:"
			  << " session_id=" << pending.notice.sessionId << " player_id=" << pending.notice.playerId
			  << " incarnation=" << pending.notice.incarnation << " attempts=" << pending.attempts;
}

void OnDisconnectReply(const uint64_t incarnation, const DisconnectReply &reply, const SteadyTime now)
{
	if (tlsInFlight > 0)
	{
		--tlsInFlight;
	}
	const auto it = tlsDisconnects.find(incarnation);
	if (it == tlsDisconnects.end() || !it->second.inFlight)
	{
		// 每次发送恰好回调一次,在途期间不会再发同一条,正常走不到这里。出口违约(重复回调)时不把同一条通知排两次。
		return;
	}
	PendingDisconnect &pending = it->second;
	pending.inFlight = false;
	if (!reply.delivered)
	{
		// 结果未知也原样重发:login 与 player_locator 对重放幂等(见头文件)。
		++tlsCounters.disconnectFailed;
		ScheduleDisconnectRetry(pending, now);
		return;
	}
	++tlsCounters.disconnectDelivered;
	if (pending.attempts > 1)
	{
		LOG_INFO << "[LoginDisconnect] Disconnect confirmed by login after retries: session_id=" << pending.notice.sessionId
				 << " player_id=" << pending.notice.playerId << " incarnation=" << incarnation
				 << " attempts=" << pending.attempts << " pending_ms=" << MillisSince(pending.closedAt, now);
	}
	tlsDisconnects.erase(it);
}

// 发一次。调用方保证:出口已装上、本拍预算与在途上限有余量、这条通知不在途。
void SendDisconnect(PendingDisconnect &pending, const SteadyTime now)
{
	NoteIfOverdue(pending, now);
	// 先记在途再发:出口可能同步调回(路由模式签名失败时直接回失败),回调里要看到一致的状态。
	pending.inFlight = true;
	++pending.attempts;
	++tlsInFlight;
	--tlsBudgetLeft;
	const bool isRetry = pending.attempts > 1;
	const uint64_t incarnation = pending.notice.incarnation;
	const bool sent = tlsDisconnectSender(pending.notice,
		[incarnation](const DisconnectReply &reply, SteadyTime receivedAt) { OnDisconnectReply(incarnation, reply, receivedAt); });
	if (!sent)
	{
		// 没发出:回调不会来,pending 仍有效。收回在途,按退避重排(留在表里,不丢)。
		--tlsInFlight;
		pending.inFlight = false;
		++tlsCounters.disconnectNotSent;
		ScheduleDisconnectRetry(pending, now);
		return;
	}
	// 发出之后不再碰 pending:同步回调可能已经处置过它。
	++tlsCounters.disconnectSent;
	if (isRetry)
	{
		++tlsCounters.disconnectRetried;
	}
}

void SendDueDisconnects(const SteadyTime now)
{
	while (tlsBudgetLeft > 0 && tlsInFlight < kMaxInFlight && !tlsDisconnectDue.empty() &&
		   tlsDisconnectDue.top().dueAt <= now)
	{
		const uint64_t incarnation = tlsDisconnectDue.top().incarnation;
		tlsDisconnectDue.pop();
		const auto it = tlsDisconnects.find(incarnation);
		if (it == tlsDisconnects.end() || it->second.inFlight)
		{
			continue; // 每条不在途的通知恰好一条登记,正常走不到这里
		}
		// 没成时重排的到期时刻至少晚 0.8 秒,这一拍不会再取到同一条。
		SendDisconnect(it->second, now);
	}
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

void SetDisconnectSender(const DisconnectSender sender)
{
	tlsDisconnectSender = sender;
}

void NoteConnectionClosed(DisconnectNotice notice, const SteadyTime now)
{
	const uint64_t incarnation = notice.incarnation;
	if (tlsDisconnects.contains(incarnation))
	{
		return; // 同一条连接只关一次;重复调用不另记一条
	}
	if (tlsDisconnects.size() >= kMaxPendingDisconnects)
	{
		++tlsCounters.disconnectDropped;
		LOG_ERROR << "[LoginDisconnect] Pending Disconnect table is full; this notice is dropped and the player stays ONLINE"
				  << " in player_locator until relogin or manual remediation (release-checklist F.B-1): session_id="
				  << notice.sessionId << " player_id=" << notice.playerId << " incarnation=" << incarnation
				  << " cap=" << kMaxPendingDisconnects;
		return;
	}
	PendingDisconnect &pending = tlsDisconnects[incarnation];
	pending.notice = std::move(notice);
	pending.closedAt = now;
	++tlsCounters.disconnectQueued;
	if (tlsDisconnectSender != nullptr && tlsBudgetLeft > 0 && tlsInFlight < kMaxInFlight)
	{
		SendDisconnect(pending, now);
		return;
	}
	// 本拍预算或在途上限用完了(成批断线),或出口还没装上:排到下一拍,到期时刻就是现在。
	tlsDisconnectDue.push(DueEntry{now, pending.notice.sessionId, incarnation});
}

void Tick(const SteadyTime now)
{
	LogSummaryIfDue(now);
	tlsBudgetLeft = kMaxRequestsPerTick;
	// 断线通知先发:它每晚一秒,玩家就多挂一秒 ONLINE;续租的间隔留着寿命三分之二的余量。
	if (tlsDisconnectSender != nullptr)
	{
		SendDueDisconnects(now);
	}
	if (tlsSender == nullptr)
	{
		return;
	}

	while (tlsBudgetLeft > 0 && tlsInFlight < kMaxInFlight && !tlsDue.empty() && tlsDue.top().dueAt <= now)
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
		--tlsBudgetLeft;
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

std::chrono::milliseconds DisconnectRetryBackoff(const uint32_t attempts, const uint64_t incarnation)
{
	// 1、2、4、8、16 秒,之后封顶 30 秒。翻倍次数先夹住,attempts 再大也不溢出。
	const uint32_t doublings = std::min<uint32_t>(attempts > 0 ? attempts - 1 : 0, 5);
	const std::chrono::milliseconds base = std::min(kDisconnectRetryBase * (1 << doublings), kDisconnectRetryCap);
	// 抖动系数 800‰ ~ 1200‰,由会话代次与次数混出来(splitmix64 的混合步骤)。
	uint64_t mixed = incarnation * 0x9E3779B97F4A7C15ull + attempts;
	mixed = (mixed ^ (mixed >> 30)) * 0xBF58476D1CE4E5B9ull;
	mixed = (mixed ^ (mixed >> 27)) * 0x94D049BB133111EBull;
	mixed ^= mixed >> 31;
	const auto permille = static_cast<std::chrono::milliseconds::rep>(800 + mixed % 401);
	return base * permille / 1000;
}

const Counters &GetCounters()
{
	return tlsCounters;
}

std::size_t PendingRenewals()
{
	return tlsDue.size();
}

std::size_t PendingDisconnects()
{
	return tlsDisconnects.size();
}

uint64_t InFlight()
{
	return tlsInFlight;
}

void CollectMetrics(prometheus_text::Writer &writer)
{
	using prometheus_text::MetricType;
	const Counters &c = tlsCounters;

	writer.BeginFamily("gate_login_disconnect_pending",
					   "Disconnect notices for closed client connections that login has not confirmed yet, in flight included.",
					   MetricType::kGauge);
	writer.Sample({}, tlsDisconnects.size());

	writer.BeginFamily("gate_login_disconnect_total",
					   "Disconnect notices to login since process start. queued: connections closed; sent: requests, resends "
					   "included; retried: resends; delivered: confirmed by login; failed / not_sent: attempts that did not "
					   "land; overdue: notices still unconfirmed a minute after close; dropped: refused because the table was full.",
					   MetricType::kCounter);
	writer.Sample({{"result", "queued"}}, c.disconnectQueued);
	writer.Sample({{"result", "sent"}}, c.disconnectSent);
	writer.Sample({{"result", "retried"}}, c.disconnectRetried);
	writer.Sample({{"result", "delivered"}}, c.disconnectDelivered);
	writer.Sample({{"result", "failed"}}, c.disconnectFailed);
	writer.Sample({{"result", "not_sent"}}, c.disconnectNotSent);
	writer.Sample({{"result", "overdue"}}, c.disconnectOverdue);
	writer.Sample({{"result", "dropped"}}, c.disconnectDropped);
}

void RegisterMetrics()
{
	if (!GlobalMetricsRegistry().Register("gate_login_disconnect", &CollectMetrics))
	{
		LOG_ERROR << "[Metrics] collector gate_login_disconnect is already registered";
	}
}

void ResetForTesting()
{
	tlsDue = {};
	tlsInFlight = 0;
	tlsBudgetLeft = kMaxRequestsPerTick;
	tlsDisconnects.clear();
	tlsDisconnectDue = {};
	tlsCounters = Counters{};
	tlsLastLoggedCounters = Counters{};
	tlsNextSummaryAt = SteadyTime{};
}

} // namespace gate_login_renewal

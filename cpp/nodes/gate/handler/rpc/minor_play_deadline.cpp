#include "minor_play_deadline.h"

#include <queue>
#include <vector>

#include "muduo/base/Logging.h"
#include "session/manager/session_manager.h"

namespace gate_minor_deadline
{
namespace
{

// 一条到期登记。连接断开后不摘除,到期时按会话号、会话代次与截止时刻核对,对不上就丢弃。
struct DueEntry
{
	uint64_t closeAtMs{0};
	uint64_t deadlineMs{0};
	SessionId sessionId{0};
	uint64_t incarnation{0};
};

// 最早到期的在堆顶。
struct DueLater
{
	bool operator()(const DueEntry &lhs, const DueEntry &rhs) const { return lhs.closeAtMs > rhs.closeAtMs; }
};

thread_local std::priority_queue<DueEntry, std::vector<DueEntry>, DueLater> tlsDue;
thread_local Counters tlsCounters;
thread_local CloseFn tlsCloser = nullptr;

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

} // namespace

void SetCloser(const CloseFn closer)
{
	tlsCloser = closer;
}

void NoteLoginSucceeded(const SessionId sessionId, SessionInfo &session, const uint64_t playDeadlineMs)
{
	// 旧的到期登记留在堆里:到期时截止时刻对不上,自然丢弃。
	session.minorPlayDeadlineMs = playDeadlineMs;
	if (playDeadlineMs == 0)
	{
		return;
	}
	++tlsCounters.armed;
	const uint64_t closeAtMs = playDeadlineMs > kCloseLeadMs ? playDeadlineMs - kCloseLeadMs : 0;
	tlsDue.push(DueEntry{closeAtMs, playDeadlineMs, sessionId, session.incarnation});
}

void Tick(const uint64_t nowUnixMs)
{
	uint64_t closedNow = 0;
	while (!tlsDue.empty() && tlsDue.top().closeAtMs <= nowUnixMs)
	{
		const DueEntry due = tlsDue.top();
		tlsDue.pop();
		SessionInfo *session = FindSession(due.sessionId, due.incarnation);
		if (session == nullptr || session->minorPlayDeadlineMs != due.deadlineMs)
		{
			continue; // 连接已断开,或之后又登录过、截止时刻变了
		}
		// 清掉:同一条连接重复登录留下的几条登记,只关一次。
		session->minorPlayDeadlineMs = 0;
		++closedNow;
		if (tlsCloser != nullptr)
		{
			tlsCloser(*session);
		}
	}
	if (closedNow > 0)
	{
		tlsCounters.closed += closedNow;
		// 每晚 21:00 前一拍里会关很多条,只打一行;单条连接的去向看它自己的断开日志。
		LOG_INFO << "[MinorDeadline] minor play time is over, closed connections=" << closedNow
				 << " closed_total=" << tlsCounters.closed << " pending=" << tlsDue.size();
	}
}

const Counters &GetCounters()
{
	return tlsCounters;
}

std::size_t Pending()
{
	return tlsDue.size();
}

void ResetForTesting()
{
	tlsDue = {};
	tlsCounters = {};
	tlsCloser = nullptr;
}

} // namespace gate_minor_deadline

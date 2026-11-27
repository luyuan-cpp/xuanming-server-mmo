#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <unordered_map>

#include "session/comp/session_info_comp.h"
#include "network/network_utils.h"

using GateSessionMap = std::unordered_map<SessionId, SessionInfo>;

class SessionManager
{
public:
	SessionManager() = default;

	SessionManager(const SessionManager&) = delete;
	SessionManager& operator=(const SessionManager&) = delete;

	GateSessionMap& sessions() { return sessionList; }
	SessionIdGenerator& session_id_gen() { return sessionIdGen; }

	// 一张 gate 票同一时刻只开一条连接(docs/design/login-queue-2026-05.md §5「一张票只开一条连接」)。
	// ticketKey 是票据签名(每张票不同)。验签通过时调用:这张票已经被另一条还活着的连接用着就返回 false,调用方拒绝这一条;
	// 记着的持有者已经不在(会话已删,或 session_id 被新连接复用、代次对不上)时由这一条接手。
	// 票据在有效期里不是一次性的,不拦的话一张票能开出任意多条已验签连接,每条都算进 gate 人数,挤掉排队的名额。
	bool ClaimTicket(const std::string &ticketKey, const SessionId sessionId, const uint64_t incarnation)
	{
		const auto [it, inserted] = ticketHolders.try_emplace(ticketKey, TicketHolder{sessionId, incarnation});
		if (inserted || (it->second.sessionId == sessionId && it->second.incarnation == incarnation))
		{
			return true;
		}
		const auto holderIt = sessionList.find(it->second.sessionId);
		if (holderIt != sessionList.end() && holderIt->second.incarnation == it->second.incarnation)
		{
			return false;
		}
		it->second = TicketHolder{sessionId, incarnation};
		return true;
	}

	// 会话删除之前调用(GateClientSessionHandler::HandleConnectionDisconnection):只摘这条连接自己持有的那一条,
	// 别的连接后来接手的不动。ticketKey 为空(没验过票,如 dev 免验签)时什么都不做。
	void ReleaseTicket(const std::string &ticketKey, const SessionId sessionId, const uint64_t incarnation)
	{
		if (ticketKey.empty())
		{
			return;
		}
		const auto it = ticketHolders.find(ticketKey);
		if (it != ticketHolders.end() && it->second.sessionId == sessionId && it->second.incarnation == incarnation)
		{
			ticketHolders.erase(it);
		}
	}

	// 占着票据的连接数,只作观测与单测。
	std::size_t TicketHolderCount() const { return ticketHolders.size(); }

private:
	struct TicketHolder
	{
		SessionId sessionId{};
		uint64_t incarnation{};
	};

	GateSessionMap sessionList;
	SessionIdGenerator sessionIdGen;
	// 票据签名 → 用它验签的那条连接。条目随会话删除摘掉,条数不超过会话数。只在 gate 的 EventLoop 线程上读写。
	std::unordered_map<std::string, TicketHolder> ticketHolders;
};

extern thread_local SessionManager tlsSessionManager;

#pragma once

#include <cstddef>
#include <cstdint>
#include <map>
#include <string>
#include <unordered_map>

#include "session/comp/pre_auth_connection.h"
#include "session/comp/session_info_comp.h"
#include "network/network_utils.h"

using GateSessionMap = std::unordered_map<SessionId, SessionInfo>;
// 未验签连接表(pre_auth_connection.h):键是代次,建连时单调分配,所以按键有序就是按建连先后有序,begin() 最老。
using PreAuthConnectionMap = std::map<uint64_t, PreAuthConnection>;

class SessionManager
{
public:
	SessionManager() = default;

	SessionManager(const SessionManager&) = delete;
	SessionManager& operator=(const SessionManager&) = delete;

	GateSessionMap& sessions() { return sessionList; }
	SessionIdGenerator& session_id_gen() { return sessionIdGen; }
	// 建连到验签成功之前的连接只在这张表里,不在会话表里(docs/design/gate-connection-admission-control.md P2-4)。
	PreAuthConnectionMap& preAuthConnections() { return preAuthConnectionList; }
	// 这台 gate 持有的客户端连接数 = 会话数 + 未验签连接数。总量上限(GateMaxConnections)按它判:
	// 未验签连接不在会话表里,同样占着 fd 与连接对象。
	std::size_t ClientConnectionCount() const { return sessionList.size() + preAuthConnectionList.size(); }

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
	// 只在 gate 的 EventLoop 线程上读写:建连时插入,验签成功、断开、握手超时、承压淘汰时摘掉。
	PreAuthConnectionMap preAuthConnectionList;
	// 票据签名 → 用它验签的那条连接。条目随会话删除摘掉,条数不超过会话数。只在 gate 的 EventLoop 线程上读写。
	std::unordered_map<std::string, TicketHolder> ticketHolders;
};

extern thread_local SessionManager tlsSessionManager;

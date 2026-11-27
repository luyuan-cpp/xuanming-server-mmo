#pragma once

#include <chrono>
#include <cstdint>
#include <memory>
#include <optional>

#include "muduo/net/TcpConnection.h"

// 未验签连接(docs/design/gate-connection-admission-control.md P2-4「票据先行」)。
//
// 建连到验签成功之前,gate 只为一条客户端连接留这一条记录:不发 session_id、不建 SessionInfo,
// 会话表与 session_id 发号器都挡在票据之后。记录在 SessionManager::preAuthConnections() 里,键是连接的代次
// (gate_lane::NextIncarnation(),进程内单调、从不复用),所以表按键有序就是按建连先后有序,begin() 最老。
// 验签成功时摘掉记录、发号建会话,会话沿用这个代次;断开、握手超时、承压淘汰时摘掉记录。
// 读写都在 gate 的 EventLoop 线程上(gate_connection_admission / gate_client_heartbeat / 断开回调)。
struct PreAuthConnection
{
	// weak(AGENTS.md §11.7):连接由 TcpServer 拥有,这里只是索引。断开回调同步摘记录,稳态下 lock() 必成功。
	std::weak_ptr<muduo::net::TcpConnection> conn;
	// 建连时刻(单调时钟):握手期限、承压淘汰的年龄、握手耗时都从它算。
	std::chrono::steady_clock::time_point connectedAt;
};

// 未验签连接在 TcpConnection 上下文里放的值:它在未验签表里的键(代次)。验签成功时上下文换成 session_id。
struct PreAuthConnectionKey
{
	uint64_t incarnation{0};
};

// 这条连接还没验签时返回它在未验签表里的键;已经有会话(上下文是 session_id)、或准入时被拒(kInvalidSessionId)时为空。
// 只读上下文,不查表:记录可能已被淘汰或握手超时摘掉,而连接还在关的路上。
inline std::optional<uint64_t> PreAuthIncarnationOf(const muduo::net::TcpConnectionPtr &conn)
{
	const auto *key = boost::any_cast<PreAuthConnectionKey>(&conn->getContext());
	if (key == nullptr)
	{
		return std::nullopt;
	}
	return key->incarnation;
}

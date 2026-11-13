#pragma once

// gate 单测共用的回环 TCP 夹具:本进程内一对 TcpServer / TcpClient 建出的一条真实连接。
//
// 给「被测代码要 lock() 真实的 TcpConnection 再写 socket 或关连接」的用例用:服务端那一头挂进会话表,
// 客户端那一头按到达顺序收下全部字节,供用例比对。做法与取舍出自 gate_push_delivery_test.cpp 里的同名夹具
// (那份与 client_heartbeat_test.cpp 各带一份副本,这里没有动它们);之后的新用例从这里取,不再复制。
//
// 只能在测试线程上用:EventLoop 归创建它的线程,下面的函数都在同一线程上驱动它。
// 头文件里的函数都是 inline,同一个测试工程里多个用例文件包含不会重复定义。

#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <utility>

#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"

// 借空闲回环端口用(FindFreeLoopbackPort)。放在 muduo 头之后:Windows 上 muduo 头已按正确顺序引入过 WinSock。
#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#else
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>
#endif

namespace gate_test
{

// 只作活性上限,不作判据:用例的结论都来自 done() 是否成立。
inline constexpr double kLoopLivenessSec = 5.0;
// 驱动期间检查完成条件的间隔。Windows 版 poll 固定 100ms 超时、返回后才扫定时器,空闲时实际粒度约 100ms,只影响用例耗时。
inline constexpr double kLoopPollIntervalSec = 0.005;

// 向 OS 借一个空闲回环端口(muduo 的 TcpServer 不暴露实际绑定端口)。失败返回 0。
inline uint16_t FindFreeLoopbackPort()
{
#ifdef _WIN32
	const SOCKET s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
	if (s == INVALID_SOCKET)
	{
		return 0;
	}
	int len = sizeof(sockaddr_in);
#else
	const int s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
	if (s < 0)
	{
		return 0;
	}
	socklen_t len = sizeof(sockaddr_in);
#endif
	sockaddr_in addr{};
	addr.sin_family = AF_INET;
	addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	addr.sin_port = 0;
	uint16_t port = 0;
	if (::bind(s, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)) == 0 &&
		::getsockname(s, reinterpret_cast<sockaddr *>(&addr), &len) == 0)
	{
		port = ntohs(addr.sin_port);
	}
#ifdef _WIN32
	::closesocket(s);
#else
	::close(s);
#endif
	return port;
}

// 在测试线程上驱动 loop,直到 done() 为真或活性上限到期,返回 done() 是否成立。条件满足后经两跳
// queueInLoop 再 quit(同 Node 关机,AGENTS.md §11.7):此前排进队列的 connectDestroyed 等按 FIFO 先跑完。
// 定时器回调与排队的两跳 functor 只捕获 loop 自身与 weak_ptr<状态>,返回前两只定时器都已取消;
// 活性上限与第一跳同轮触发时第二跳会留在队列里,本次驱动返回后状态已析构,它在下次驱动里 lock 失败即不 quit。
inline bool DriveLoopUntil(muduo::net::EventLoop &loop, std::function<bool()> done)
{
	struct DriveState
	{
		std::function<bool()> done;
		bool quitting = false;
	};
	const auto state = std::make_shared<DriveState>();
	state->done = std::move(done);
	auto *const loopPtr = &loop;
	const auto checkTimer = loop.runEvery(kLoopPollIntervalSec, [loopPtr, weak = std::weak_ptr<DriveState>(state)] {
		const auto current = weak.lock();
		if (!current || current->quitting || !current->done())
		{
			return;
		}
		current->quitting = true;
		loopPtr->queueInLoop([loopPtr, weak] {
			loopPtr->queueInLoop([loopPtr, weak] {
				if (weak.lock())
				{
					loopPtr->quit();
				}
			});
		});
	});
	const auto livenessTimer = loop.runAfter(kLoopLivenessSec, [loopPtr] { loopPtr->quit(); });
	loop.loop();
	loop.cancel(checkTimer);
	loop.cancel(livenessTimer);
	return state->done();
}

// 一条真实的回环 TCP 连接。连接回调只捕获 weak_ptr<Probe>,本类只存服务端连接的 weak(AGENTS.md §11.7)。
// 拆除:客户端 disconnect(半关写端)→ 服务端读到 EOF 关闭 → 客户端读到 EOF 关闭;两端都 DOWN 且
// connectDestroyed 排空后才析构 TcpClient / TcpServer —— 二者把裸 this 绑进连接的 close 回调,必须活到
// 连接关完;连接析构时也必须已是 kDisconnected(TcpConnection.cc:71)。
class LoopbackConnection
{
public:
	explicit LoopbackConnection(muduo::net::EventLoop &loop) : loop_(loop) {}
	~LoopbackConnection() { Close(); }
	LoopbackConnection(const LoopbackConnection &) = delete;
	LoopbackConnection &operator=(const LoopbackConnection &) = delete;

	// 建连并等两端都 UP;借不到端口或活性上限内没连上返回 false。
	bool Open(const std::string &name)
	{
		const uint16_t port = FindFreeLoopbackPort();
		if (port == 0)
		{
			return false;
		}
		server_ = std::make_unique<muduo::net::TcpServer>(
			&loop_, muduo::net::InetAddress(port, /*loopbackOnly=*/true), name + "_server");
		server_->setConnectionCallback([weak = std::weak_ptr<Probe>(probe_)](const muduo::net::TcpConnectionPtr &conn) {
			const auto probe = weak.lock();
			if (!probe)
			{
				return;
			}
			if (conn->connected())
			{
				probe->serverSide = conn;
				probe->serverUp = true;
			}
			else
			{
				probe->serverDown = true;
			}
		});
		server_->start();
		client_ = std::make_unique<muduo::net::TcpClient>(
			&loop_, muduo::net::InetAddress("127.0.0.1", port), name + "_client");
		client_->setConnectionCallback([weak = std::weak_ptr<Probe>(probe_)](const muduo::net::TcpConnectionPtr &conn) {
			const auto probe = weak.lock();
			if (!probe)
			{
				return;
			}
			if (conn->connected())
			{
				probe->clientUp = true;
			}
			else
			{
				probe->clientDown = true;
			}
		});
		// 客户端侧按到达顺序收下全部字节,供用例比对服务端写出的帧。
		client_->setMessageCallback([weak = std::weak_ptr<Probe>(probe_)](const muduo::net::TcpConnectionPtr &,
																		  muduo::net::Buffer *buffer, muduo::Timestamp) {
			const auto probe = weak.lock();
			if (!probe)
			{
				buffer->retrieveAll();
				return;
			}
			probe->clientReceived.append(buffer->retrieveAllAsString());
		});
		client_->connect();
		return DriveLoopUntil(loop_, [probe = probe_] { return probe->serverUp && probe->clientUp; });
	}

	// 服务端连接,挂进会话表用。调用方不要长期持有返回的强引用:muduo 关连接不关 fd、要等连接析构,
	// 强引用不放手客户端就等不到 EOF。
	muduo::net::TcpConnectionPtr ServerSide() const { return probe_->serverSide.lock(); }
	bool ServerSideConnected() const
	{
		const auto serverSide = probe_->serverSide.lock();
		return serverSide && serverSide->connected();
	}
	// 两端是否都已断开(服务端关连接之后,客户端读到 EOF)。
	bool BothSidesDown() const { return probe_->serverDown && probe_->clientDown; }
	// 客户端侧至今收到的全部字节。只在测试线程(即 loop 线程)读。
	const std::string &ClientReceived() const { return probe_->clientReceived; }

private:
	struct Probe
	{
		std::weak_ptr<muduo::net::TcpConnection> serverSide;
		bool serverUp = false;
		bool serverDown = false;
		bool clientUp = false;
		bool clientDown = false;
		std::string clientReceived;
	};

	void Close()
	{
		if (!client_ && !server_)
		{
			return;
		}
		if (client_)
		{
			client_->disconnect();
		}
		const bool closed = DriveLoopUntil(loop_, [probe = probe_] {
			return (!probe->serverUp || probe->serverDown) && (!probe->clientUp || probe->clientDown);
		});
		if (!closed)
		{
			// 活性上限内没关完:此时析构会让连接带着非 kDisconnected 状态析构(TcpConnection.cc:71),
			// 或让连接的 close 回调打到已析构的 TcpServer / TcpClient。宁可泄漏这对测试端点并报失败。
			ADD_FAILURE() << "loopback connection did not close within liveness bound";
			(void)client_.release();
			(void)server_.release();
			return;
		}
		client_.reset();
		server_.reset();
		// 再转一圈:TcpClient 析构排队的 Connector 收尾等 functor 在本用例内跑完。
		(void)DriveLoopUntil(loop_, [] { return true; });
	}

	muduo::net::EventLoop &loop_;
	const std::shared_ptr<Probe> probe_ = std::make_shared<Probe>();
	std::unique_ptr<muduo::net::TcpServer> server_;
	std::unique_ptr<muduo::net::TcpClient> client_;
};

} // namespace gate_test

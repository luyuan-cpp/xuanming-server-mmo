// gate 客户端连接存活检查装配层的单测(handler/rpc/client_heartbeat.{h,cpp},docs/design/gate-client-heartbeat.md):
// 握手期限、验签回包通告与开始跟踪、心跳应答、空闲超时。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj,被测源码直接编进测试
// (同 gate_push_delivery_test.cpp)。向 tlsSessionManager 放会话,会话的连接是真实的回环 TCP:被测函数要 lock()
// 真实的 TcpConnection 回包或 forceClose,结果在客户端侧收字节比对、看两端是否断开。
// 「没有写」不靠等待判定:核对时先往这条连接直接写一帧哨兵,TCP 保序,收到哨兵时它前面的字节就是全部结果。
//
// 被测状态是进程级 thread_local(检查点堆、计数、上一次扫描的时刻),用例之间不重置:
//   - 计数只看自 SetUp 以来的增量;
//   - 每个用例的合成时刻基点比上一个晚一天。SetUp 先在基点扫一次,前面用例留下的检查点全部按「会话已不在」丢弃,
//     之后按 1 秒节拍的扫描也都不算迟到;
//   - 会话代次在进程内单调分配,前面用例留下的检查点不会认错会话。
// 合成时刻与真实的 steady_clock 不可比,所以用例不调读真实时钟的 NoteInboundFrame / OnHeartbeatRequest,
// 「收到一帧」直接写 ClientLiveness::NoteInbound(两者只差读不读时钟)。
//
// 生产上 HandleConnectionEstablished 调 ExpectHandshake、DispatchTokenVerify 的成功回包调 OnTokenVerified;用例按同一顺序直接调它们。
// 没覆盖的一层:GateClientSessionHandler 注册回调、两个收包入口调 NoteInboundFrame、上面两处调用点、main.cpp 装扫描定时器。
// client_session_handler.cpp 与 main.cpp 拖着节点注册表、登录与场景入口,进不了这个工程,靠核对调用点与 robot 的 heartbeat-idle-smoke。

#include <gtest/gtest.h>

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <utility>
#include <vector>

#include "gate_codec.h"
#include "handler/rpc/client_heartbeat.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"
#include "proto/common/base/message.pb.h"
#include "session/manager/session_manager.h"

// 借空闲回环端口用(FindFreeLoopbackPort)。放在项目头之后:Windows 上 muduo 头已按正确顺序引入过 WinSock。
#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#else
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>
#endif

namespace
{
	using gate_client_heartbeat::Counters;
	using gate_liveness::kClientLiveness;
	using gate_liveness::SteadyTime;
	using std::chrono::seconds;

	constexpr SessionId kSession = 2001;
	constexpr Guid kPlayer = 100000000000101ull;
	constexpr uint32_t kSentinelMessageId = 4243;

	// 只作活性上限,不作判据(同 rpc_channel_loopback_test 的 kLiveness)。
	constexpr double kLoopLivenessSec = 5.0;
	// 驱动期间检查完成条件的间隔。Windows 版 poll 固定 100ms 超时、返回后才扫定时器,空闲时实际粒度约 100ms,只影响用例耗时。
	constexpr double kLoopPollIntervalSec = 0.005;

	// gate codec 写到线上的一帧(长度、类型名、载荷、校验和)。
	std::string FrameBytes(const google::protobuf::Message &message)
	{
		muduo::net::Buffer buffer;
		ProtobufCodec::fillEmptyBuffer(&buffer, message);
		return buffer.retrieveAllAsString();
	}

	std::string HeartbeatReplyBytes(const uint64_t seq)
	{
		ClientHeartbeatResponse reply;
		reply.set_seq(seq);
		return FrameBytes(reply);
	}

	// 向 OS 借一个空闲回环端口(muduo 的 TcpServer 不暴露实际绑定端口)。失败返回 0。
	uint16_t FindFreeLoopbackPort()
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
	// queueInLoop 再 quit(同 Node 关机,AGENTS.md §11.7):此前排进队列的 forceCloseInLoop、connectDestroyed 等按 FIFO 先跑完。
	// 定时器回调与排队的两跳 functor 只捕获 loop 自身与 weak_ptr<状态>,返回前两只定时器都已取消;
	// 活性上限与第一跳同轮触发时第二跳会留在队列里,本次驱动返回后状态已析构,它在下次驱动里 lock 失败即不 quit。
	bool DriveLoopUntil(muduo::net::EventLoop &loop, std::function<bool()> done)
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

	// 一条真实的回环 TCP 连接(同 gate_push_delivery_test.cpp,另加两端是否已断开的读数)。
	// 连接回调只捕获 weak_ptr<Probe>,本类只存服务端连接的 weak(AGENTS.md §11.7)。
	// ServerSide() 每次 lock() 出一个临时强引用,调用方不要长期持有:强引用会让服务端 fd 不关,拆除时客户端等不到 EOF。
	// 拆除:客户端 disconnect(半关写端)→ 服务端读到 EOF 关闭 → 客户端读到 EOF 关闭;两端都 DOWN 且
	// connectDestroyed 排空后才析构 TcpClient / TcpServer —— 二者把裸 this 绑进连接的 close 回调,必须活到
	// 连接关完;连接析构时也必须已是 kDisconnected(TcpConnection.cc:71)。被测代码先 forceClose 的连接,拆除时两端已 DOWN。
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

		muduo::net::TcpConnectionPtr ServerSide() const { return probe_->serverSide.lock(); }
		// 客户端侧至今收到的全部字节。只在测试线程(即 loop_ 线程)读。
		const std::string &ClientReceived() const { return probe_->clientReceived; }
		bool ServerDown() const { return probe_->serverDown; }
		bool ClientDown() const { return probe_->clientDown; }

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

	// 回包经 GetGateCodec() 发送。这里的回调不会被调用:用例只写不解码。进程内只留一份,不会留下悬空的全局指针。
	ProtobufCodec &SharedCodec()
	{
		static ProtobufCodec codec([](const muduo::net::TcpConnectionPtr &, const MessagePtr &, muduo::Timestamp) {});
		return codec;
	}

	// 会话代次:进程内单调,前面用例留在检查点堆里的条目不会认错本用例的会话。
	uint64_t NextTestIncarnation()
	{
		static uint64_t last = 1ull << 40;
		return ++last;
	}

	class GateClientHeartbeat : public testing::Test
	{
	protected:
		void SetUp() override
		{
			loop_ = std::make_unique<muduo::net::EventLoop>();
			InitGateCodec(SharedCodec());
			tlsSessionManager.sessions().clear();
			static int caseIndex = 0;
			base_ = SteadyTime{} + std::chrono::hours(24) * (++caseIndex);
			// 丢弃前面用例留下的检查点(会话都已清掉),并让本用例之后的 1 秒节拍不算迟到。
			gate_client_heartbeat::SweepLiveness(base_);
			baseline_ = gate_client_heartbeat::GetCounters();
		}

		void TearDown() override
		{
			tlsSessionManager.sessions().clear();
			connections_.clear(); // 拆连接要驱动 loop,必须先于 loop_ 析构
			loop_.reset();
		}

		// 建一条回环连接并放进会话表。连不上时这里报失败,之后的核对也会失败,不会误过。
		LoopbackConnection &AddSession(const SessionId sessionId, const bool verified)
		{
			auto connection = std::make_unique<LoopbackConnection>(*loop_);
			EXPECT_TRUE(connection->Open("gate_heartbeat_" + std::to_string(sessionId)));
			const auto serverSide = connection->ServerSide();
			if (serverSide)
			{
				serverSide->setContext(sessionId); // 同 HandleConnectionEstablished:连接上下文里是 session_id
			}
			tlsSessionManager.sessions().erase(sessionId); // 同号重放时从一个全新的会话开始
			SessionInfo &session = tlsSessionManager.sessions()[sessionId];
			session.conn = serverSide;
			session.verified = verified;
			session.playerId = kPlayer;
			session.incarnation = NextTestIncarnation();
			connections_.push_back(std::move(connection));
			return *connections_.back();
		}

		SessionInfo &Session(const SessionId sessionId) { return tlsSessionManager.sessions().at(sessionId); }

		void Heartbeat(const LoopbackConnection &connection, const uint64_t seq, const SteadyTime now)
		{
			const auto serverSide = connection.ServerSide();
			ASSERT_NE(serverSide, nullptr);
			ClientHeartbeatRequest request;
			request.set_seq(seq);
			gate_client_heartbeat::HandleHeartbeat(serverSide, request, now);
		}

		// 验签成功的回包(生产上是 DispatchTokenVerify 的 sendReply(true, …)):通告心跳参数,开始跟踪。返回回包。
		ClientTokenVerifyResponse VerifySucceeded(const SessionId sessionId, const SteadyTime now)
		{
			SessionInfo &session = Session(sessionId);
			session.verified = true;
			ClientTokenVerifyResponse response;
			response.set_success(true);
			gate_client_heartbeat::OnTokenVerified(sessionId, session, response, now);
			return response;
		}

		// 按 1 秒节拍从 from 扫到 to(两端都含),模拟准时的定时器。
		static void SweepEachSecond(const SteadyTime from, const SteadyTime to)
		{
			for (SteadyTime t = from; t <= to; t += kClientLiveness.checkPeriod)
			{
				gate_client_heartbeat::SweepLiveness(t);
			}
		}

		// 这条连接至今写出的字节必须恰好是 expected。每条连接只核对一次:核对本身会写一帧哨兵。
		void ExpectReceived(const LoopbackConnection &connection, const std::string &expected)
		{
			const auto serverSide = connection.ServerSide();
			ASSERT_NE(serverSide, nullptr);
			MessageContent sentinel;
			sentinel.set_message_id(kSentinelMessageId);
			sentinel.set_serialized_message("sentinel");
			const std::string all = expected + FrameBytes(sentinel);
			GetGateCodec().send(serverSide, sentinel);
			ASSERT_TRUE(DriveLoopUntil(*loop_, [&connection, &all] { return connection.ClientReceived().size() >= all.size(); }));
			EXPECT_EQ(connection.ClientReceived(), all);
		}

		// 被测代码 forceClose 之后,两端都应看到断开。
		void ExpectClosed(const LoopbackConnection &connection)
		{
			EXPECT_TRUE(DriveLoopUntil(*loop_, [&connection] { return connection.ServerDown() && connection.ClientDown(); }))
				<< "the gate side should have force-closed the connection";
		}

		// 转一圈 loop,让可能已排队的 forceCloseInLoop 跑完,再确认服务端没有断开。
		void ExpectStillOpen(const LoopbackConnection &connection)
		{
			(void)DriveLoopUntil(*loop_, [] { return true; });
			EXPECT_FALSE(connection.ServerDown());
			EXPECT_FALSE(connection.ClientDown());
		}

		// 自 SetUp 以来的计数增量。
		Counters Delta() const
		{
			const Counters &now = gate_client_heartbeat::GetCounters();
			Counters delta;
			delta.answered = now.answered - baseline_.answered;
			delta.excess = now.excess - baseline_.excess;
			delta.beforeVerify = now.beforeVerify - baseline_.beforeVerify;
			delta.tracked = now.tracked - baseline_.tracked;
			delta.idleClosed = now.idleClosed - baseline_.idleClosed;
			delta.handshakeTimeouts = now.handshakeTimeouts - baseline_.handshakeTimeouts;
			delta.deferred = now.deferred - baseline_.deferred;
			return delta;
		}

		SteadyTime base_{};

	private:
		std::unique_ptr<muduo::net::EventLoop> loop_;
		std::vector<std::unique_ptr<LoopbackConnection>> connections_;
		Counters baseline_{};
	};
} // namespace

// ---- 验签回包:通告与开始跟踪 ----

TEST_F(GateClientHeartbeat, TokenVerifiedAdvertisesHeartbeatAndStartsTrackingOnce)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	const ClientTokenVerifyResponse response = VerifySucceeded(kSession, base_ + seconds(1));
	EXPECT_TRUE(response.success());
	EXPECT_EQ(response.heartbeat_interval_ms(), 10000u);
	EXPECT_EQ(response.heartbeat_timeout_ms(), 30000u);
	EXPECT_TRUE(Session(kSession).liveness.tracked);

	// 已验证会话重发验签:照样通告,不重复登记。
	const ClientTokenVerifyResponse again = VerifySucceeded(kSession, base_ + seconds(2));
	EXPECT_EQ(again.heartbeat_interval_ms(), 10000u);
	EXPECT_EQ(Delta(), (Counters{.tracked = 1}));
	ExpectStillOpen(connection);
}

// ---- 收心跳 ----

TEST_F(GateClientHeartbeat, HeartbeatIsEchoedWithItsSeq)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	VerifySucceeded(kSession, base_ + seconds(1));
	Heartbeat(connection, 7, base_ + seconds(1));
	Heartbeat(connection, 8, base_ + seconds(11));

	ExpectReceived(connection, HeartbeatReplyBytes(7) + HeartbeatReplyBytes(8));
	EXPECT_EQ(Delta(), (Counters{.answered = 2, .tracked = 1}));
	EXPECT_EQ(Session(kSession).illegalPacketCount, 0u);
}

// dev 免验签:会话建连即已验证,客户端没发验签请求就来了心跳。从这一条起跟踪。
TEST_F(GateClientHeartbeat, HeartbeatWithoutAVerifyReplyStartsTracking)
{
	const auto &connection = AddSession(kSession, /*verified=*/true);
	Heartbeat(connection, 1, base_ + seconds(1));

	EXPECT_TRUE(Session(kSession).liveness.tracked);
	ExpectReceived(connection, HeartbeatReplyBytes(1));
	EXPECT_EQ(Delta(), (Counters{.answered = 1, .tracked = 1}));
}

// 与验签前发 ClientRequest 同一处置:不回包,直接断开。
TEST_F(GateClientHeartbeat, HeartbeatBeforeTokenVerifyClosesTheConnection)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	Heartbeat(connection, 1, base_ + seconds(1));

	ExpectClosed(connection);
	EXPECT_TRUE(connection.ClientReceived().empty());
	EXPECT_FALSE(Session(kSession).liveness.tracked);
	EXPECT_EQ(Delta(), (Counters{.beforeVerify = 1}));
}

// 一个窗口里第 4 条不回,计一次非法包;连接不断(阈值缺省 50)。
TEST_F(GateClientHeartbeat, FourthHeartbeatInOneWindowIsNotAnsweredAndCountsAsIllegal)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	const SteadyTime burst = base_ + seconds(1);
	VerifySucceeded(kSession, burst);
	for (uint64_t seq = 1; seq <= 4; ++seq)
	{
		Heartbeat(connection, seq, burst);
	}

	ExpectReceived(connection, HeartbeatReplyBytes(1) + HeartbeatReplyBytes(2) + HeartbeatReplyBytes(3));
	EXPECT_EQ(Session(kSession).illegalPacketCount, 1u);
	EXPECT_EQ(Delta(), (Counters{.answered = 3, .excess = 1, .tracked = 1}));
	ExpectStillOpen(connection);
}

// ---- 空闲超时 ----

// 验签之后一帧都不发(没实现心跳的客户端):从验签回包那一刻起满 30s 关,只关一次。
TEST_F(GateClientHeartbeat, VerifiedClientThatNeverHeartbeatsIsClosedOnceAfterTheIdleTimeout)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	const SteadyTime start = base_ + seconds(1);
	VerifySucceeded(kSession, start);

	SweepEachSecond(start, start + kClientLiveness.idleTimeout - seconds(1));
	EXPECT_EQ(Delta().idleClosed, 0u);
	ExpectStillOpen(connection);

	gate_client_heartbeat::SweepLiveness(start + kClientLiveness.idleTimeout);
	EXPECT_EQ(Delta().idleClosed, 1u);
	ExpectClosed(connection);

	// 测试里没有断开回调去删会话;之后的扫描也不会再关一次。
	SweepEachSecond(start + kClientLiveness.idleTimeout + seconds(1), start + 2 * kClientLiveness.idleTimeout);
	EXPECT_EQ(Delta(), (Counters{.tracked = 1, .idleClosed = 1}));
}

TEST_F(GateClientHeartbeat, InboundFramePostponesTheIdleClose)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	const SteadyTime start = base_ + seconds(1);
	VerifySucceeded(kSession, start);

	SweepEachSecond(start, start + seconds(20));
	// 收到一帧(生产上是三个收包入口里的 NoteInboundFrame)。
	Session(kSession).liveness.NoteInbound(start + seconds(20));
	SweepEachSecond(start + seconds(21), start + seconds(49));
	EXPECT_EQ(Delta().idleClosed, 0u);
	ExpectStillOpen(connection);

	gate_client_heartbeat::SweepLiveness(start + seconds(50));
	EXPECT_EQ(Delta().idleClosed, 1u);
	ExpectClosed(connection);
}

TEST_F(GateClientHeartbeat, RegularHeartbeatsKeepTheConnectionOpen)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	const SteadyTime start = base_ + seconds(1);
	VerifySucceeded(kSession, start);
	std::string expected;
	uint64_t seq = 0;
	for (SteadyTime t = start; t <= start + 4 * kClientLiveness.idleTimeout; t += kClientLiveness.checkPeriod)
	{
		if ((t - start) % kClientLiveness.interval == SteadyTime::duration::zero())
		{
			Heartbeat(connection, ++seq, t);
			expected += HeartbeatReplyBytes(seq);
		}
		gate_client_heartbeat::SweepLiveness(t);
	}

	ExpectReceived(connection, expected);
	EXPECT_EQ(Delta(), (Counters{.answered = seq, .tracked = 1}));
	ExpectStillOpen(connection);
}

// dev 免验签、客户端既没发验签请求也没发心跳:gate 从没通告过心跳参数,不跟踪,静默再久也不关。
TEST_F(GateClientHeartbeat, DevBypassSessionThatNeverAskedToVerifyIsNotTracked)
{
	const auto &connection = AddSession(kSession, /*verified=*/true);
	SweepEachSecond(base_ + seconds(1), base_ + 3 * kClientLiveness.idleTimeout);

	EXPECT_FALSE(Session(kSession).liveness.tracked);
	EXPECT_EQ(Delta(), Counters{});
	ExpectStillOpen(connection);
}

// 会话关闭后 session_id 被一条新连接复用:旧连接留下的检查点按代次丢弃,不会关掉新连接。
TEST_F(GateClientHeartbeat, StaleCheckpointDoesNotCloseANewConnectionWithTheSameSessionId)
{
	const auto &oldConnection = AddSession(kSession, /*verified=*/false);
	const SteadyTime start = base_ + seconds(1);
	VerifySucceeded(kSession, start);
	tlsSessionManager.sessions().erase(kSession); // 断开回调删了会话

	const auto &newConnection = AddSession(kSession, /*verified=*/true); // 新代次,没验签请求、没心跳,不跟踪
	SweepEachSecond(start, start + kClientLiveness.idleTimeout + seconds(2));

	EXPECT_EQ(Delta().idleClosed, 0u);
	ExpectStillOpen(newConnection);
	ExpectStillOpen(oldConnection);
}

// gate 的 loop 卡过(距上次扫描超过两个节拍):这一轮只推迟并计数,下一次准时的扫描才关。
TEST_F(GateClientHeartbeat, StalledLoopDefersTheVerdictToTheNextOnTimeSweep)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	const SteadyTime start = base_ + seconds(1);
	VerifySucceeded(kSession, start);
	SweepEachSecond(start, start + kClientLiveness.idleTimeout - seconds(1));

	const SteadyTime stalledUntil = start + kClientLiveness.idleTimeout + seconds(3);
	gate_client_heartbeat::SweepLiveness(stalledUntil);
	EXPECT_EQ(Delta().deferred, 1u);
	EXPECT_EQ(Delta().idleClosed, 0u);
	ExpectStillOpen(connection);

	gate_client_heartbeat::SweepLiveness(stalledUntil + seconds(1));
	EXPECT_EQ(Delta().idleClosed, 1u);
	ExpectClosed(connection);
}

// ---- 握手期限 ----

// 建连 10s 仍未验签:关。没验签的会话没有角色,断开回调只删会话。
TEST_F(GateClientHeartbeat, UnverifiedConnectionIsClosedAtTheHandshakeDeadline)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	const SteadyTime connectedAt = base_ + seconds(1);
	gate_client_heartbeat::ExpectHandshake(kSession, Session(kSession), connectedAt);

	SweepEachSecond(connectedAt, connectedAt + kClientLiveness.handshakeTimeout - seconds(1));
	EXPECT_EQ(Delta().handshakeTimeouts, 0u);
	ExpectStillOpen(connection);

	gate_client_heartbeat::SweepLiveness(connectedAt + kClientLiveness.handshakeTimeout);
	EXPECT_EQ(Delta(), (Counters{.handshakeTimeouts = 1}));
	ExpectClosed(connection);
}

// 期限之内验签:期限作废,之后靠心跳保活。
TEST_F(GateClientHeartbeat, VerifyingBeforeTheHandshakeDeadlineKeepsTheConnection)
{
	const auto &connection = AddSession(kSession, /*verified=*/false);
	const SteadyTime connectedAt = base_ + seconds(1);
	gate_client_heartbeat::ExpectHandshake(kSession, Session(kSession), connectedAt);
	const SteadyTime verifiedAt = connectedAt + seconds(2);
	VerifySucceeded(kSession, verifiedAt);
	Heartbeat(connection, 1, verifiedAt);
	Heartbeat(connection, 2, verifiedAt + kClientLiveness.interval);

	SweepEachSecond(connectedAt, connectedAt + 2 * kClientLiveness.handshakeTimeout);
	EXPECT_EQ(Delta(), (Counters{.answered = 2, .tracked = 1}));
	ExpectStillOpen(connection);
}

// gate 推送收口 gate_push 的单测(handler/gate_push_delivery.h,docs/design/routing-identity-audit-20260908.md R13)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj,被测源码直接编进测试。
// 向 tlsSessionManager 放会话,会话的连接是真实的回环 TCP(夹具做法同 cpp/nodes/battle/tests/battle_observer_contract_test.cpp):
// 被测函数要 lock() 真实的 TcpConnection 再写 socket,结果在客户端侧收字节比对。
// 「没有写」不靠等待判定:核对时先往这条连接直接写一帧哨兵,TCP 保序,收到哨兵时它前面的字节就是全部结果。
// 计数是进程累计的 thread_local,用例只看自 SetUp 以来的增量,不依赖执行顺序。
//
// 没覆盖的一层:GateHandler(gate_service_handler.cpp)与 GateEventHandler(gate_event_handler.cpp)把消息转给这些函数的那一行。
// 两个文件拖着节点注册表、登录与场景入口,进不了这个工程;它们各只有一次调用,靠核对调用点与 robot 冒烟。

#include <gtest/gtest.h>

#include <array>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <utility>
#include <vector>

#include "gate_codec.h"
#include "handler/gate_push_delivery.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"
#include "network/broadcast_target_codec.h"
#include "proto/common/base/message.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/gate/gate_service.pb.h"
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
	using gate_push::DeliveryCounters;
	using gate_push::Path;

	constexpr SessionId kSession = 1001;
	constexpr SessionId kOtherSession = 1002;
	constexpr SessionId kThirdSession = 1003;
	constexpr Guid kPlayerA = 100000000000001ull;
	constexpr Guid kPlayerB = 100000000000002ull;
	constexpr uint64_t kScene = 7001;
	constexpr uint64_t kOtherScene = 7002;
	constexpr uint32_t kMessageId = 4242;
	constexpr uint32_t kSentinelMessageId = 4243;

	// 只作活性上限,不作判据(同 rpc_channel_loopback_test 的 kLiveness)。
	constexpr double kLoopLivenessSec = 5.0;
	// 驱动期间检查完成条件的间隔。Windows 版 poll 固定 100ms 超时、返回后才扫定时器,空闲时实际粒度约 100ms,只影响用例耗时。
	constexpr double kLoopPollIntervalSec = 0.005;

	MessageContent Content(const std::string &body)
	{
		MessageContent content;
		content.set_message_id(kMessageId);
		content.set_serialized_message(body);
		return content;
	}

	// 客户端在这条连接上应该收到的字节。单播经 gate codec、广播经 EncodedFrame,两者是同一种编码。
	std::string Bytes(const MessageContent &content)
	{
		const gate_push::EncodedFrame frame(content);
		return std::string(frame.data(), frame.size());
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
	// queueInLoop 再 quit(同 Node 关机,AGENTS.md §11.7):此前排进队列的 connectDestroyed 等按 FIFO 先跑完。
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

	// 一条真实的回环 TCP 连接。连接回调只捕获 weak_ptr<Probe>,本类只存服务端连接的 weak(AGENTS.md §11.7)。
	// ServerSide() 每次 lock() 出一个临时强引用,调用方不要长期持有:强引用会让服务端 fd 不关,拆除时客户端等不到 EOF。
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

		muduo::net::TcpConnectionPtr ServerSide() const { return probe_->serverSide.lock(); }
		// 客户端侧至今收到的全部字节。只在测试线程(即 loop_ 线程)读。
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

	// 单播经 GetGateCodec() 发送。这里的回调不会被调用:用例只写不解码。进程内只留一份,不会留下悬空的全局指针。
	ProtobufCodec &SharedCodec()
	{
		static ProtobufCodec codec([](const muduo::net::TcpConnectionPtr &, const MessagePtr &, muduo::Timestamp) {});
		return codec;
	}

	class GatePushDelivery : public testing::Test
	{
	protected:
		void SetUp() override
		{
			loop_ = std::make_unique<muduo::net::EventLoop>();
			InitGateCodec(SharedCodec());
			tlsSessionManager.sessions().clear();
			baseline_[Index(Path::kRpc)] = gate_push::Counters(Path::kRpc);
			baseline_[Index(Path::kKafka)] = gate_push::Counters(Path::kKafka);
		}

		void TearDown() override
		{
			tlsSessionManager.sessions().clear();
			connections_.clear(); // 拆连接要驱动 loop,必须先于 loop_ 析构
			loop_.reset();
		}

		// 建一条回环连接并放进会话表。连不上时这里报失败,之后的 ExpectReceived 也会失败,不会误过。
		LoopbackConnection &AddSession(const SessionId sessionId, const Guid playerId, const uint64_t sceneId = 0)
		{
			auto connection = std::make_unique<LoopbackConnection>(*loop_);
			EXPECT_TRUE(connection->Open("gate_push_" + std::to_string(sessionId)));
			SessionInfo &session = tlsSessionManager.sessions()[sessionId];
			session.playerId = playerId;
			session.sceneId = sceneId;
			session.conn = connection->ServerSide();
			connections_.push_back(std::move(connection));
			return *connections_.back();
		}

		// 这条连接至今写出的字节必须恰好是 expected。每条连接只核对一次:核对本身会写一帧哨兵。
		void ExpectReceived(const LoopbackConnection &connection, const std::string &expected)
		{
			const auto serverSide = connection.ServerSide();
			ASSERT_NE(serverSide, nullptr);
			MessageContent sentinel = Content("sentinel");
			sentinel.set_message_id(kSentinelMessageId);
			const std::string all = expected + Bytes(sentinel);
			gate_push::EncodedFrame(sentinel).SendTo(serverSide);
			ASSERT_TRUE(DriveLoopUntil(*loop_, [&connection, &all] { return connection.ClientReceived().size() >= all.size(); }));
			EXPECT_EQ(connection.ClientReceived(), all);
		}

		// 这条入口自 SetUp 以来的计数增量必须恰好是 expected(没写的项为 0)。
		void ExpectCounted(const Path path, const DeliveryCounters &expected) const
		{
			const DeliveryCounters &now = gate_push::Counters(path);
			const DeliveryCounters &base = baseline_[Index(path)];
			EXPECT_EQ(now.delivered - base.delivered, expected.delivered);
			EXPECT_EQ(now.deliveredUnfenced - base.deliveredUnfenced, expected.deliveredUnfenced);
			EXPECT_EQ(now.droppedFence - base.droppedFence, expected.droppedFence);
			EXPECT_EQ(now.sessionNotFound - base.sessionNotFound, expected.sessionNotFound);
			EXPECT_EQ(now.noConnection - base.noConnection, expected.noConnection);
			EXPECT_EQ(now.invalidMessage - base.invalidMessage, expected.invalidMessage);
			EXPECT_EQ(now.droppedScene0 - base.droppedScene0, expected.droppedScene0);
		}

	private:
		static std::size_t Index(const Path path) { return static_cast<std::size_t>(path); }

		std::unique_ptr<muduo::net::EventLoop> loop_;
		std::vector<std::unique_ptr<LoopbackConnection>> connections_;
		std::array<DeliveryCounters, static_cast<std::size_t>(Path::kCount)> baseline_{};
	};
} // namespace

// ---- 单播:身份栅栏三态(R13)----

// 两条入口走同一份判定,计数按入口分开。
TEST_F(GatePushDelivery, UnicastDeliversOnMatchDropsOnMismatchAndPassesZeroUnfenced)
{
	const auto &connection = AddSession(kSession, kPlayerA);
	const MessageContent matched = Content("matched");
	const MessageContent mismatched = Content("mismatched");
	const MessageContent unfenced = Content("unfenced");

	std::string expected;
	for (const Path path : {Path::kRpc, Path::kKafka})
	{
		gate_push::DeliverToSession(path, kSession, kPlayerA, &matched);
		gate_push::DeliverToSession(path, kSession, kPlayerB, &mismatched);
		gate_push::DeliverToSession(path, kSession, 0, &unfenced);
		expected += Bytes(matched) + Bytes(unfenced);
		ExpectCounted(path, {.delivered = 1, .deliveredUnfenced = 1, .droppedFence = 1});
	}
	ExpectReceived(connection, expected);
}

// 还没 Bind 的会话:带真实玩家的推送必须丢(gate 重启后继任者把旧 session_id 发给一条刚连上的新连接,这正是 R13 的失败形态);
// 只有显式的 0 能到,login 在 Bind 之前的登录失败提示就走这一位。
TEST_F(GatePushDelivery, UnboundSessionOnlyAcceptsTheExplicitZeroTarget)
{
	const auto &connection = AddSession(kSession, kInvalidGuid);
	const MessageContent named = Content("named");
	const MessageContent loginFailureTip = Content("login-failure-tip");

	gate_push::DeliverToSession(Path::kKafka, kSession, kPlayerA, &named);
	gate_push::DeliverToSession(Path::kKafka, kSession, 0, &loginFailureTip);

	ExpectReceived(connection, Bytes(loginFailureTip));
	ExpectCounted(Path::kKafka, {.deliveredUnfenced = 1, .droppedFence = 1});
}

TEST_F(GatePushDelivery, MissingSessionAndReleasedConnectionAreOnlyCounted)
{
	const MessageContent content = Content("nobody");
	gate_push::DeliverToSession(Path::kKafka, kSession, kPlayerA, &content);

	tlsSessionManager.sessions()[kOtherSession].playerId = kPlayerA; // conn 是空的 weak_ptr
	gate_push::DeliverToSession(Path::kKafka, kOtherSession, kPlayerA, &content);

	ExpectCounted(Path::kKafka, {.sessionNotFound = 1, .noConnection = 1});
}

// ---- 缺 message_content:照发就是给客户端推一个空帧 ----

TEST_F(GatePushDelivery, MissingMessageContentIsRejectedForEveryKindOfPush)
{
	const auto &connection = AddSession(kSession, kPlayerA, kScene);

	const contracts::kafka::PushToPlayerEvent push;
	gate_push::DeliverToSession(Path::kKafka, kSession, kPlayerA, gate_push::ContentOf(push));

	const contracts::kafka::BroadcastToSceneEvent scene;
	gate_push::DeliverToScene(Path::kKafka, kScene, gate_push::ContentOf(scene));

	const contracts::kafka::BroadcastToAllEvent all;
	gate_push::DeliverToAll(Path::kKafka, gate_push::ContentOf(all));

	contracts::kafka::BroadcastToPlayersEvent targeted;
	targeted.add_session_list(kSession);
	targeted.add_player_list(kPlayerA);
	gate_push::DeliverToTargets(Path::kKafka, targeted);

	ExpectReceived(connection, "");
	ExpectCounted(Path::kKafka, {.invalidMessage = 4});
}

// ---- 指名广播 ----

// 每个目标各过一次栅栏;RPC 请求与 Kafka 事件是同一份实现。
TEST_F(GatePushDelivery, TargetedBroadcastFencesEveryTargetOnBothEntries)
{
	const auto &bound = AddSession(kSession, kPlayerA);
	const auto &otherPlayer = AddSession(kOtherSession, kPlayerB);
	const auto &unbound = AddSession(kThirdSession, kInvalidGuid);
	const MessageContent content = Content("targeted");
	const std::vector<broadcast_targets::Target> targets{
		{kSession, kPlayerA},
		{kOtherSession, kPlayerA}, // 这个会话现在绑的是 kPlayerB
		{kThirdSession, 0},
	};

	BroadcastToPlayersRequest request;
	*request.mutable_message_content() = content;
	auto requestTargets = targets;
	broadcast_targets::Encode(request, requestTargets);
	gate_push::DeliverToTargets(Path::kRpc, request);

	contracts::kafka::BroadcastToPlayersEvent event;
	*event.mutable_message_content() = content;
	auto eventTargets = targets;
	broadcast_targets::Encode(event, eventTargets);
	gate_push::DeliverToTargets(Path::kKafka, event);

	const std::string twice = Bytes(content) + Bytes(content);
	ExpectReceived(bound, twice);
	ExpectReceived(otherPlayer, "");
	ExpectReceived(unbound, twice);
	ExpectCounted(Path::kRpc, {.delivered = 1, .deliveredUnfenced = 1, .droppedFence = 1});
	ExpectCounted(Path::kKafka, {.delivered = 1, .deliveredUnfenced = 1, .droppedFence = 1});
}

// player_list 与目标数不齐时无法按位置对应,逐项比对会把每个人的身份比成别人的。整条拒绝,排在前面、本来对得上的目标也不发。
TEST_F(GatePushDelivery, MisalignedPlayerListRejectsTheWholeBroadcast)
{
	const auto &connection = AddSession(kSession, kPlayerA);
	contracts::kafka::BroadcastToPlayersEvent event;
	*event.mutable_message_content() = Content("misaligned");
	event.add_session_list(kSession);
	event.add_session_list(kOtherSession);
	event.add_player_list(kPlayerA);

	gate_push::DeliverToTargets(Path::kKafka, event);

	ExpectReceived(connection, "");
	ExpectCounted(Path::kKafka, {.invalidMessage = 1});
}

// ---- 场景 / 全服广播:没有目标玩家可比对,只推已绑定玩家的会话 ----

TEST_F(GatePushDelivery, SceneBroadcastReachesOnlyBoundSessionsOfThatScene)
{
	const auto &inScene = AddSession(kSession, kPlayerA, kScene);
	const auto &otherScene = AddSession(kOtherSession, kPlayerB, kOtherScene);
	const auto &unbound = AddSession(kThirdSession, kInvalidGuid, kScene);
	const MessageContent content = Content("scene");

	gate_push::DeliverToScene(Path::kKafka, kScene, &content);

	ExpectReceived(inScene, Bytes(content));
	ExpectReceived(otherScene, "");
	ExpectReceived(unbound, "");
	ExpectCounted(Path::kKafka, {.delivered = 1});
}

// 没进场景的会话 sceneId 就是 0,按 0 匹配会把消息推给所有还没进场景的连接。
TEST_F(GatePushDelivery, SceneZeroIsRejectedEvenThoughSessionsOutsideAnySceneHaveSceneZero)
{
	const auto &outsideAnyScene = AddSession(kSession, kPlayerA, 0);
	const MessageContent content = Content("scene-zero");

	gate_push::DeliverToScene(Path::kKafka, 0, &content);

	ExpectReceived(outsideAnyScene, "");
	ExpectCounted(Path::kKafka, {.droppedScene0 = 1});
}

TEST_F(GatePushDelivery, BroadcastToAllSkipsSessionsNotBoundToAPlayer)
{
	const auto &bound = AddSession(kSession, kPlayerA);
	const auto &unbound = AddSession(kOtherSession, kInvalidGuid);
	// playerId 为 0 同样按未绑定处理。这个会话没有连接:若没被跳过,会计成 noConnection。
	tlsSessionManager.sessions()[kThirdSession].playerId = 0;
	const MessageContent content = Content("all");

	gate_push::DeliverToAll(Path::kKafka, &content);

	ExpectReceived(bound, Bytes(content));
	ExpectReceived(unbound, "");
	ExpectCounted(Path::kKafka, {.delivered = 1});
}

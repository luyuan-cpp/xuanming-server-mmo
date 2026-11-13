// 推 msg 124 的收口 gate_redirect::PushRedirect 的单测(handler/gate_redirect_delivery.h,
// docs/design/gate-drain-session-migration.md §4.3)。跨 zone 传送与 gate 排空迁移共用这一个函数。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj,被测源码直接编进测试。
// 会话的连接是真实的回环 TCP(loopback_connection.h),结果在客户端侧收字节比对。字节的先后正是这里要钉的:
// 排队请求的回执必须先于 124 写出 —— 客户端收到 124 就换连接,排在它后面的回执再也读不到,只能干等请求超时。
// 路由 lane 用真实的装配层(client_request_lane.cpp),发送函数注入替身,用来在会话上排出「一条在途、若干排队」。
// 「没有写」不靠等待判定:核对时先往这条连接直接写一帧哨兵,TCP 保序,收到哨兵时它前面的字节就是全部结果。
//
// 没覆盖的一层:GateEventHandler 把 RedirectToGateEvent 转给本函数的那一行,以及「推过 124 的会话被踢时不推提示」
// (都在 gate_event_handler.cpp,拖着节点注册表与场景入口,进不了这个工程);靠核对调用点与 robot 冒烟。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "gate_codec.h"
#include "handler/gate_push_delivery.h"
#include "handler/gate_redirect_delivery.h"
#include "handler/rpc/client_request_lane.h"
#include "handler/rpc/drain_migration.h"
#include "handler/rpc/session_details_builder.h"
#include "loopback_connection.h"
#include "proto/common/base/message.pb.h"
#include "proto/scene/client_player_common.pb.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "session/manager/session_manager.h"
#include "table/proto/tip/common_error_tip.pb.h"

namespace
{
	using gate_drain_migration::PushOutcome;
	using gate_drain_migration::SteadyTime;
	using gate_test::DriveLoopUntil;
	using gate_test::LoopbackConnection;
	using std::chrono::seconds;

	constexpr SessionId kSession = 5151;
	constexpr Guid kPlayerA = 100000000000011ull;
	constexpr Guid kPlayerB = 100000000000012ull;
	// 不需要登录闸的在线域请求号。lane 不按号查注册表,取不与 login 号冲突的值即可(同 client_request_lane_test.cpp)。
	constexpr uint32_t kQueryMessageId = 900001;
	constexpr uint32_t kQuery2MessageId = 900002;
	constexpr uint32_t kSentinelMessageId = 900099;

	// 单调时钟上的任意起点。被测代码只把它记进会话、与滞留期限比较先后。
	constexpr SteadyTime kT0 = SteadyTime{} + std::chrono::hours(1);

	// 路由 lane 的发送替身:一律报已发出,第一条请求于是停在在途,后面的排队。
	gate_lane::RouterSendResult AlwaysSent(const ClientRequest &, const std::string &)
	{
		return gate_lane::RouterSendResult::kSent;
	}

	// 推送经 GetGateCodec() 发送。这里的回调不会被调用:用例只写不解码。进程内只留一份,不会留下悬空的全局指针。
	ProtobufCodec &SharedCodec()
	{
		static ProtobufCodec codec([](const muduo::net::TcpConnectionPtr &, const MessagePtr &, muduo::Timestamp) {});
		return codec;
	}

	RedirectToGateNotify Notify(const std::string &targetIp = "10.0.0.7")
	{
		RedirectToGateNotify notify;
		notify.set_target_ip(targetIp);
		notify.set_target_port(7101);
		notify.set_token_payload("payload");
		notify.set_token_signature("signature");
		notify.set_token_deadline(1793000300);
		return notify;
	}

	// 客户端在这条连接上应该收到的字节(gate codec 的客户端帧;广播用的 EncodedFrame 是同一种编码)。
	std::string Bytes(const MessageContent &content)
	{
		const gate_push::EncodedFrame frame(content);
		return std::string(frame.data(), frame.size());
	}

	std::string RedirectBytes(const RedirectToGateNotify &notify)
	{
		MessageContent content;
		content.set_message_id(SceneClientPlayerCommonRedirectToGateMessageId);
		content.set_serialized_message(notify.SerializeAsString());
		return Bytes(content);
	}

	// 排队请求的回执:带请求 id 与原消息号的 1003 信封(gate_lane::SendEnvelope 的形状)。
	std::string RejectedBytes(uint64_t clientRequestId, uint32_t messageId)
	{
		MessageContent envelope;
		envelope.set_id(clientRequestId);
		envelope.set_message_id(messageId);
		envelope.mutable_error_message()->set_id(kServiceUnavailable);
		return Bytes(envelope);
	}

	class GateRedirectDelivery : public testing::Test
	{
	protected:
		void SetUp() override
		{
			loop_ = std::make_unique<muduo::net::EventLoop>();
			InitGateCodec(SharedCodec());
			ResetState();
			gate_lane::SetRouterSender(&AlwaysSent);
		}

		void TearDown() override
		{
			ResetState();
			connections_.clear(); // 拆连接要驱动 loop,必须先于 loop_ 析构
			loop_.reset();
		}

		// 建一条回环连接并挂成会话:验过票、登录成功、经认证绑定了 playerId(kInvalidGuid = 还在选角界面)。
		// 连不上时这里报失败,之后的 ExpectReceived 也会失败,不会误过。
		LoopbackConnection &AddSession(Guid playerId)
		{
			auto connection = std::make_unique<LoopbackConnection>(*loop_);
			EXPECT_TRUE(connection->Open("gate_redirect"));
			SessionInfo &session = tlsSessionManager.sessions()[kSession];
			session.verified = true;
			session.loginSucceeded = true;
			session.incarnation = gate_lane::NextIncarnation();
			session.playerId = playerId;
			session.authenticatedAccount = "redirect-test-account";
			session.authenticatedOwnerRevision = 7;
			session.conn = connection->ServerSide();
			connections_.push_back(std::move(connection));
			return *connections_.back();
		}

		static SessionInfo &Session()
		{
			return tlsSessionManager.sessions().at(kSession);
		}

		// 客户端发来一条经路由 lane 的请求:身份按收包时刻的会话构造(与 HandleRouterForward 同一个构造点)。
		static void Receive(uint64_t clientRequestId, uint32_t messageId)
		{
			SessionInfo &session = Session();
			auto request = std::make_shared<ClientRequest>();
			request->set_id(clientRequestId);
			request->set_message_id(messageId);
			gate_lane::Admit(kSession, session, request,
							 gate_session_details::Build(kSession, session.playerId, 1, "redirect-test-gate", &session));
		}

		// 这条连接至今写出的字节必须恰好是 expected。每条连接只核对一次:核对本身会写一帧哨兵。
		void ExpectReceived(const LoopbackConnection &connection, const std::string &expected)
		{
			const auto serverSide = connection.ServerSide();
			ASSERT_NE(serverSide, nullptr);
			MessageContent sentinel;
			sentinel.set_message_id(kSentinelMessageId);
			const std::string all = expected + Bytes(sentinel);
			gate_push::EncodedFrame(sentinel).SendTo(serverSide);
			ASSERT_TRUE(DriveLoopUntil(*loop_, [&connection, &all] { return connection.ClientReceived().size() >= all.size(); }));
			EXPECT_EQ(connection.ClientReceived(), all);
		}

		muduo::net::EventLoop &Loop() { return *loop_; }

	private:
		static void ResetState()
		{
			tlsSessionManager.sessions().clear();
			gate_lane::ResetForTesting();
			gate_drain_migration::ResetForTesting();
		}

		std::unique_ptr<muduo::net::EventLoop> loop_;
		std::vector<std::unique_ptr<LoopbackConnection>> connections_;
	};
} // namespace

// 排队的路由请求逐条回 1003,而且先于 124 写出;在途那一条不动。之后会话标成已重定向,滞留关闭从这次推送起算。
TEST_F(GateRedirectDelivery, QueuedRequestsAreAnsweredBeforeTheRedirectFrame)
{
	LoopbackConnection &connection = AddSession(kPlayerA);
	Receive(1, kQueryMessageId);  // 发出,在途
	Receive(2, kQuery2MessageId); // 排队
	Receive(3, kQueryMessageId);  // 排队
	ASSERT_EQ(Session().routerLane.QueuedCount(), 2u);
	const RedirectToGateNotify notify = Notify();

	EXPECT_EQ(gate_redirect::PushRedirect(kSession, kPlayerA, notify, kT0), PushOutcome::kPushed);

	ExpectReceived(connection, RejectedBytes(2, kQuery2MessageId) + RejectedBytes(3, kQueryMessageId) + RedirectBytes(notify));
	const SessionInfo &session = Session();
	EXPECT_TRUE(session.routerLane.Redirected());
	EXPECT_EQ(session.routerLane.QueuedCount(), 0u);
	EXPECT_FALSE(session.routerLane.Idle()) << "the in-flight request keeps its slot: its reply is still delivered";
	ASSERT_TRUE(session.redirectedAt.has_value());
	EXPECT_EQ(*session.redirectedAt, kT0);
	EXPECT_EQ(gate_lane::GetCounters().droppedOnRedirect, 2u);
}

// 身份栅栏(R13):票据绑定的玩家不是会话当前的角色,不推,也不动会话 —— 排队的请求照常留着。
// session_id 序号回绕或 gate node_id 复用之后,这张票本来会推给另一名玩家。
TEST_F(GateRedirectDelivery, TicketForAnotherPlayerIsDroppedWithoutTouchingTheSession)
{
	LoopbackConnection &connection = AddSession(kPlayerA);
	Receive(1, kQueryMessageId);
	Receive(2, kQuery2MessageId);

	EXPECT_EQ(gate_redirect::PushRedirect(kSession, kPlayerB, Notify(), kT0), PushOutcome::kIdentityMismatch);

	ExpectReceived(connection, "");
	const SessionInfo &session = Session();
	EXPECT_FALSE(session.routerLane.Redirected());
	EXPECT_EQ(session.routerLane.QueuedCount(), 1u);
	EXPECT_FALSE(session.redirectedAt.has_value());
}

// 还在选角界面的会话没有绑定角色:绑定了玩家的票据必须丢(那不是给这条连接的),只有不绑玩家的票据
// (排空迁移给选角界面连接签的,ticketPlayerId = 0)能推。
TEST_F(GateRedirectDelivery, UnboundSessionTakesOnlyAnUnboundTicket)
{
	LoopbackConnection &connection = AddSession(kInvalidGuid);
	const RedirectToGateNotify notify = Notify();

	EXPECT_EQ(gate_redirect::PushRedirect(kSession, kPlayerA, notify, kT0), PushOutcome::kIdentityMismatch);
	EXPECT_EQ(gate_redirect::PushRedirect(kSession, 0, notify, kT0), PushOutcome::kPushed);

	ExpectReceived(connection, RedirectBytes(notify));
}

// 会话已不在,或会话还在而连接已释放:如实返回,不改任何状态。
TEST_F(GateRedirectDelivery, MissingSessionOrReleasedConnectionChangesNothing)
{
	EXPECT_EQ(gate_redirect::PushRedirect(kSession, kPlayerA, Notify(), kT0), PushOutcome::kSessionGone);

	SessionInfo &session = tlsSessionManager.sessions()[kSession]; // conn 是空的 weak_ptr
	session.playerId = kPlayerA;
	EXPECT_EQ(gate_redirect::PushRedirect(kSession, kPlayerA, Notify(), kT0), PushOutcome::kConnectionGone);
	EXPECT_FALSE(session.routerLane.Redirected());
	EXPECT_FALSE(session.redirectedAt.has_value());
}

// 同一条连接上后到的重定向(排空迁移的票之后又来一张跨 zone 传送的票)照样推出去,由客户端决定跟哪一张;
// 滞留关闭仍从第一次推送起算,不因为后一张票往后拖。
TEST_F(GateRedirectDelivery, LaterRedirectIsStillDeliveredAndKeepsTheFirstLingerStart)
{
	LoopbackConnection &connection = AddSession(kPlayerA);
	const RedirectToGateNotify first = Notify("10.0.0.7");
	const RedirectToGateNotify second = Notify("10.0.0.8");

	EXPECT_EQ(gate_redirect::PushRedirect(kSession, kPlayerA, first, kT0), PushOutcome::kPushed);
	EXPECT_EQ(gate_redirect::PushRedirect(kSession, kPlayerA, second, kT0 + seconds(3)), PushOutcome::kPushed);

	ExpectReceived(connection, RedirectBytes(first) + RedirectBytes(second));
	ASSERT_TRUE(Session().redirectedAt.has_value());
	EXPECT_EQ(*Session().redirectedAt, kT0);
}

// 真实的推送出口登记了滞留关闭:推过 124 满 kRedirectLinger 仍连着,排空迁移的那一拍把连接关掉。
TEST_F(GateRedirectDelivery, PushedSessionIsClosedByTheLingerSweep)
{
	LoopbackConnection &connection = AddSession(kPlayerA);
	ASSERT_EQ(gate_redirect::PushRedirect(kSession, kPlayerA, Notify(), kT0), PushOutcome::kPushed);

	gate_drain_migration::Tick(kT0 + gate_drain_migration::kRedirectLinger - seconds(1));
	EXPECT_TRUE(connection.ServerSideConnected());

	gate_drain_migration::Tick(kT0 + gate_drain_migration::kRedirectLinger);
	EXPECT_EQ(gate_drain_migration::GetCounters().lingerClosed, 1u);
	EXPECT_TRUE(DriveLoopUntil(Loop(), [&connection] { return connection.BothSidesDown(); }));
}

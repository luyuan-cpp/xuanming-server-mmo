// gate 客户端入口两道 login 相关闸的接线用例(handler/rpc/client_entry_admission.h,2026-11-12 数据审计遗留)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj,被测源码 client_entry_admission.cpp
// 与路由 lane 的装配层 client_request_lane.cpp 直接编进测试。会话的连接是真实的回环 TCP(夹具在 loopback_connection.h):
// 回执信封经 gate codec 真写进 socket,在客户端侧收字节比对;「没有写」不靠等待判定 —— 核对时先往这条连接写一帧哨兵,
// TCP 保序,收到哨兵时它前面的字节就是全部结果。计数是进程累计的 thread_local,SetUp 里 ResetForTesting 清零。
//
// 钉的两处接线(纯判定的用例在 cpp/tests/routing_identity_test/gate_client_entry_admission_test.cpp):
//   1. 客户端自己发 Disconnect(58):计一次非法包、不回任何包、不往下转发(调用方随即返回,不进任何 lane);到阈值断连;
//   2. 已绑定角色时 EnterGame 另一个角色:回带请求 id 与原消息号的信封(kLoginInProgress,2005),不往下转发、不执行会话状态变化 ——
//      路由模式走真实的 lane 与真实的发出前钩子;直连模式走收包即发出的那一道。同一角色照常放行。
// 没覆盖的一层:DispatchClientRpcMessage 调这几个函数的那几行、构造函数里装发出钩子的那一行(client_session_handler.cpp
// 拖着节点注册表、登录与场景入口,进不了这个工程),靠核对调用点。

#include <gtest/gtest.h>

#include <cstdint>
#include <memory>
#include <string>

#include "gate_codec.h"
#include "handler/rpc/client_entry_admission.h"
#include "handler/rpc/client_request_lane.h"
#include "handler/rpc/session_details_builder.h"
#include "message_limiter/illegal_packet_counter.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/session.pb.h"
#include "proto/login/login.pb.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "session/manager/session_manager.h"
#include "table/proto/tip/login_error_tip.pb.h"
#include "loopback_connection.h"

namespace
{
	using gate_test::DriveLoopUntil;
	using gate_test::LoopbackConnection;

	constexpr SessionId kSession = 5101;
	constexpr Guid kBoundRole = 200000000000101ull;
	constexpr Guid kOtherRole = 200000000000102ull;
	const std::string kAccount = "entry-wiring-account";
	constexpr uint64_t kOwnerRevision = 9;
	constexpr uint32_t kSentinelMessageId = 4244;

	// gate codec 写到线上的一帧(长度、类型名、载荷、校验和)。
	std::string FrameBytes(const google::protobuf::Message &message)
	{
		muduo::net::Buffer buffer;
		ProtobufCodec::fillEmptyBuffer(&buffer, message);
		return buffer.retrieveAllAsString();
	}

	// 带请求 id 与原消息号的错误信封(gate_lane::SendEnvelope 的形状)。
	std::string EnvelopeBytes(const uint64_t clientRequestId, const uint32_t messageId, const uint32_t tipId)
	{
		MessageContent envelope;
		envelope.set_id(clientRequestId);
		envelope.set_message_id(messageId);
		envelope.mutable_error_message()->set_id(tipId);
		return FrameBytes(envelope);
	}

	std::shared_ptr<ClientRequest> EnterGameRequest(const uint64_t clientRequestId, const Guid role)
	{
		loginpb::EnterGameRequest body;
		body.set_player_id(role);
		body.set_request_id("entry-wiring-enter");
		auto request = std::make_shared<ClientRequest>();
		request->set_id(clientRequestId);
		request->set_message_id(ClientPlayerLoginEnterGameMessageId);
		request->set_body(body.SerializeAsString());
		return request;
	}

	// 回执经 GetGateCodec() 发送。这里的回调不会被调用:用例只写不解码。进程内只留一份,不会留下悬空的全局指针。
	ProtobufCodec &SharedCodec()
	{
		static ProtobufCodec codec([](const muduo::net::TcpConnectionPtr &, const MessagePtr &, muduo::Timestamp) {});
		return codec;
	}

	// 路由服的发送替身:只数发出了几次 —— 「往下转发了没有」的判据。
	int &RouterSends()
	{
		static int sends = 0;
		return sends;
	}

	gate_lane::RouterSendResult CountingRouterSend(const ClientRequest &, const std::string &)
	{
		++RouterSends();
		return gate_lane::RouterSendResult::kSent;
	}

	// 发出后钩子的替身:只数调了几次(真实的 ApplyLoginTransition 拖着场景入口,进不了这个工程)。被拒的请求不该走到它。
	int &AfterSends()
	{
		static int calls = 0;
		return calls;
	}

	void CountingAfterSend(SessionId, SessionInfo &, const ClientRequest &)
	{
		++AfterSends();
	}

	class GateClientEntryWiring : public testing::Test
	{
	protected:
		void SetUp() override
		{
			loop_ = std::make_unique<muduo::net::EventLoop>();
			InitGateCodec(SharedCodec());
			gate_lane::ResetForTesting();
			tlsSessionManager.sessions().clear();
			RouterSends() = 0;
			AfterSends() = 0;
			gate_lane::SetRouterSender(&CountingRouterSend);
			// 发出前钩子装的是生产那一个(client_session_handler.cpp 构造时装的同一个函数),发出后钩子换成计数替身。
			gate_lane::SetSendHooks(&gate_client_entry::LoginTransitionRejectTip, &CountingAfterSend);

			connection_ = std::make_unique<LoopbackConnection>(*loop_);
			ASSERT_TRUE(connection_->Open("gate_entry_wiring"));
			const auto serverSide = connection_->ServerSide();
			ASSERT_NE(serverSide, nullptr);
			serverSide->setContext(kSession); // 同 HandleConnectionEstablished:连接上下文里是 session_id

			// 已进游戏:验过票、本连接 Login 成功、经认证绑定了角色(字段同 client_request_lane_test.cpp 的夹具)。
			SessionInfo &session = tlsSessionManager.sessions()[kSession];
			session.conn = serverSide;
			session.verified = true;
			session.loginSucceeded = true;
			session.incarnation = gate_lane::NextIncarnation();
			session.playerId = kBoundRole;
			session.authenticatedAccount = kAccount;
			session.authenticatedOwnerRevision = kOwnerRevision;
		}

		void TearDown() override
		{
			gate_lane::ResetForTesting();
			tlsSessionManager.sessions().clear();
			connection_.reset(); // 拆连接要驱动 loop,必须先于 loop_ 析构
			loop_.reset();
		}

		static SessionInfo &Session() { return tlsSessionManager.sessions().at(kSession); }

		// 路由模式的收包:身份按收包时刻的会话构造(与 HandleRouterForward 同一个构造点),交给路由 lane。
		static void AdmitThroughRouterLane(const std::shared_ptr<ClientRequest> &request)
		{
			SessionInfo &session = Session();
			gate_lane::Admit(kSession, session, request,
							 gate_session_details::Build(kSession, session.playerId, 1, "entry-wiring-gate", &session));
		}

		// 这条连接至今写出的字节必须恰好是 expected。每条连接只核对一次:核对本身会写一帧哨兵。
		void ExpectReceived(const std::string &expected)
		{
			const auto serverSide = connection_->ServerSide();
			ASSERT_NE(serverSide, nullptr);
			MessageContent sentinel;
			sentinel.set_message_id(kSentinelMessageId);
			sentinel.set_serialized_message("sentinel");
			const std::string all = expected + FrameBytes(sentinel);
			GetGateCodec().send(serverSide, sentinel);
			ASSERT_TRUE(DriveLoopUntil(*loop_, [this, &all] { return connection_->ClientReceived().size() >= all.size(); }));
			EXPECT_EQ(connection_->ClientReceived(), all);
		}

		std::unique_ptr<muduo::net::EventLoop> loop_;
		std::unique_ptr<LoopbackConnection> connection_;
	};
} // namespace

// 客户端自己发 58(Disconnect,只能由 gate 合成):计一次非法包,不回任何包,不往下转发(返回 true = 调用方随即返回);连接照常开着。
// 排空迁移的要票请求同样只由 gate 合成,同一处置。对照:白名单里的号放行,不计数。
TEST_F(GateClientEntryWiring, ClientSentDisconnectCountsAsIllegalRepliesNothingAndIsNotForwarded)
{
	SessionInfo &session = Session();
	const uint32_t illegalBefore = session.illegalPacketCount;

	EXPECT_TRUE(gate_client_entry::RejectUnauthorizedMessageId(kSession, session, connection_->ServerSide(),
															  ClientPlayerLoginDisconnectMessageId))
		<< "调用方随即返回,不往下转发";
	EXPECT_EQ(illegalBefore + 1, session.illegalPacketCount) << "计一次非法包";
	EXPECT_TRUE(gate_client_entry::RejectUnauthorizedMessageId(kSession, session, connection_->ServerSide(),
															  ClientPlayerLoginRequestGateMigrationMessageId));
	EXPECT_EQ(illegalBefore + 2, session.illegalPacketCount);

	EXPECT_FALSE(gate_client_entry::RejectUnauthorizedMessageId(kSession, session, connection_->ServerSide(),
															   ClientPlayerLoginEnterGameMessageId))
		<< "白名单里的号放行";
	EXPECT_EQ(illegalBefore + 2, session.illegalPacketCount) << "放行不计数";

	EXPECT_EQ(0u, gate_lane::GetCounters().admitted) << "没进任何 lane";
	EXPECT_EQ(0, RouterSends());
	ExpectReceived("");
	EXPECT_TRUE(connection_->ServerSideConnected()) << "没到阈值,不断连";
}

// 非法包到阈值(IllegalPacketCounter,缺省 50)时这一条照样不回包,并把连接关掉。
TEST_F(GateClientEntryWiring, ClientSentDisconnectAtTheIllegalPacketThresholdClosesTheConnection)
{
	const uint32_t threshold = IllegalPacketCounter::ThresholdFromEnv();
	if (threshold == 0)
	{
		GTEST_SKIP() << "GATE_ILLEGAL_PACKET_THRESHOLD=0 关掉了断连";
	}
	SessionInfo &session = Session();
	session.illegalPacketCount = threshold - 1;

	EXPECT_TRUE(gate_client_entry::RejectUnauthorizedMessageId(kSession, session, connection_->ServerSide(),
															  ClientPlayerLoginDisconnectMessageId));
	EXPECT_EQ(threshold, session.illegalPacketCount);
	EXPECT_TRUE(DriveLoopUntil(*loop_, [this] { return connection_->BothSidesDown(); })) << "到阈值 forceClose";
	EXPECT_TRUE(connection_->ClientReceived().empty()) << "断连之前也不回包";
}

// 路由模式:已绑定角色、没 LeaveGame 就 EnterGame 另一个角色。真实的发出前钩子在轮到它发出时拒绝:
// 回带请求 id 与原消息号的 kLoginInProgress(2005)信封,路由服一次都没收到,发出后钩子(会话状态变化)没跑,lane 空闲。
// 对照:同一角色重发 EnterGame 照常发出。
TEST_F(GateClientEntryWiring, RoleSwitchThroughTheRouterLaneRepliesTheRequestIdEnvelopeAndIsNotForwarded)
{
	ASSERT_EQ(2005u, static_cast<uint32_t>(kLoginInProgress)) << "客户端按 id 查文案,这个码是契约";

	AdmitThroughRouterLane(EnterGameRequest(/*clientRequestId=*/31, kOtherRole));
	EXPECT_EQ(0, RouterSends()) << "不往下转发";
	EXPECT_EQ(0, AfterSends()) << "被拒的请求不执行会话状态变化";
	EXPECT_EQ(1u, gate_lane::GetCounters().rejectedBeforeSend);
	EXPECT_TRUE(Session().routerLane.Idle());
	EXPECT_EQ(kBoundRole, Session().playerId) << "绑定没变";

	AdmitThroughRouterLane(EnterGameRequest(/*clientRequestId=*/32, kBoundRole));
	EXPECT_EQ(1, RouterSends()) << "同一角色重发照常发出";
	EXPECT_EQ(1, AfterSends());

	ExpectReceived(EnvelopeBytes(31, ClientPlayerLoginEnterGameMessageId, kLoginInProgress));
}

// 直连模式(收包即发出):同一道闸在收包点判,被拒时回同样的信封并返回 true —— 调用方随即返回,不转发、不执行会话状态变化。
// 同一角色返回 false、什么都不发,由调用方照常转发。
TEST_F(GateClientEntryWiring, RoleSwitchInDirectModeRepliesTheRequestIdEnvelopeAndIsNotForwarded)
{
	SessionInfo &session = Session();
	EXPECT_TRUE(gate_client_entry::RejectLoginTransitionBeforeDirectSend(kSession, session, *EnterGameRequest(41, kOtherRole)))
		<< "调用方随即返回,不转发";
	EXPECT_FALSE(gate_client_entry::RejectLoginTransitionBeforeDirectSend(kSession, session, *EnterGameRequest(42, kBoundRole)))
		<< "同一角色放行";
	EXPECT_EQ(0u, session.illegalPacketCount) << "换角闸不计非法包";
	EXPECT_EQ(0u, gate_lane::GetCounters().admitted) << "直连模式不经路由 lane";
	EXPECT_EQ(0, RouterSends());

	ExpectReceived(EnvelopeBytes(41, ClientPlayerLoginEnterGameMessageId, kLoginInProgress));
	EXPECT_TRUE(connection_->ServerSideConnected()) << "换角闸不断连";
}

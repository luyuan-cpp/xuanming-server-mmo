// 路由 lane 装配层的单测(handler/rpc/client_request_lane.h,docs/design/client-request-ordering.md §4.4–§4.6、§4.10)。
//
// 不进 gate.vcxproj,登记在本工程(routing_identity_test.vcxproj),被测源码 client_request_lane.cpp 直接编进测试。
// 发送函数与两个发出钩子注入替身:发送替身按用例给定的结果依次返回并记下发出的 SessionDetails;发出后钩子在 LeaveGame 时
// 照真实钩子(client_session_handler.cpp ApplyLoginTransition)对身份的影响调 ResetSceneRoute,不代发 ExitGame(那要节点注册表)。
// 会话不挂连接:回执信封在 SendEnvelope 里因连接为空不发,用例按计数与 lane 状态核对走了哪条分支。
// 纯逻辑(排队、单飞、认领恰好一次)的用例在 cpp/nodes/gate/tests/router_lane_test.cpp,同样编进本工程。
//
// 回归对象(2026-11-10 盘点):
//   F1 发送函数报「调用没构造出来」时当场回执、不登记在途,同会话下一条立刻发出(原先报「已发出」,队头卡约 9 s);
//   F2 重定向交出全部排队项逐条回执(原先只清不回执);
//   F3 LeaveGame 的会话状态变化在它真正发出时按收包顺序生效,没发出就不生效;之后受理的请求按离开之后的身份取快照;
//   F5 发出前身份复核失配计 rejected_stale_identity(原先不计数)。

#include <gtest/gtest.h>

#include <cstddef>
#include <cstdint>
#include <deque>
#include <memory>
#include <string>
#include <vector>

#include "handler/event/scene_route_helper.h"
#include "handler/rpc/client_request_lane.h"
#include "handler/rpc/session_details_builder.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/session.pb.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "session/manager/session_manager.h"
#include "table/proto/tip/login_error_tip.pb.h"

namespace
{
	using gate_lane::GetCounters;
	using gate_lane::RouterSendResult;

	constexpr Guid kPlayer = 100000000000077ull;
	const std::string kAccount = "lane-test-account";
	constexpr uint64_t kOwnerRevision = 7;
	// 不需要登录闸的在线域请求号。lane 不按号查注册表,替身也不查,取一个不与 login 号冲突的值即可。
	constexpr uint32_t kQueryMessageId = 900001;
	constexpr uint32_t kQuery2MessageId = 900002;

	struct SentCall
	{
		uint64_t clientRequestId{0};
		uint32_t messageId{0};
		SessionDetails details;
	};

	struct FakeRouter
	{
		std::deque<RouterSendResult> results; // 依次返回;用完之后一律 kSent
		std::vector<SentCall> sent;
		uint32_t rejectMessageId{0}; // 发出前钩子对这个号回 rejectTip
		uint32_t rejectTip{0};
		std::vector<uint32_t> afterSend; // 发出后钩子见到的消息号,按调用顺序
	};

	FakeRouter &Router()
	{
		static FakeRouter router;
		return router;
	}

	RouterSendResult FakeSend(const ClientRequest &request, const std::string &sessionDetailsBin)
	{
		FakeRouter &router = Router();
		RouterSendResult result = RouterSendResult::kSent;
		if (!router.results.empty())
		{
			result = router.results.front();
			router.results.pop_front();
		}
		if (result == RouterSendResult::kSent)
		{
			SentCall call;
			call.clientRequestId = request.id();
			call.messageId = request.message_id();
			EXPECT_TRUE(call.details.ParseFromString(sessionDetailsBin));
			router.sent.push_back(std::move(call));
		}
		return result;
	}

	uint32_t FakeBeforeSend(SessionId, const SessionInfo &, const ClientRequest &request)
	{
		const FakeRouter &router = Router();
		return request.message_id() == router.rejectMessageId ? router.rejectTip : 0;
	}

	void FakeAfterSend(SessionId, SessionInfo &session, const ClientRequest &request)
	{
		Router().afterSend.push_back(request.message_id());
		if (request.message_id() == ClientPlayerLoginLeaveGameMessageId)
		{
			gate_scene_route::ResetSceneRoute(session);
		}
	}

	class ClientRequestLane : public testing::Test
	{
	protected:
		static constexpr SessionId kSession = 4242;

		void SetUp() override
		{
			gate_lane::ResetForTesting();
			tlsSessionManager.sessions().clear();
			Router() = FakeRouter{};
			gate_lane::SetRouterSender(&FakeSend);
			gate_lane::SetSendHooks(&FakeBeforeSend, &FakeAfterSend);

			// 已进游戏:验过票、本连接 Login 成功、经认证绑定了角色。
			SessionInfo &session = tlsSessionManager.sessions()[kSession];
			session.verified = true;
			session.loginSucceeded = true;
			session.incarnation = gate_lane::NextIncarnation();
			session.playerId = kPlayer;
			session.authenticatedAccount = kAccount;
			session.authenticatedOwnerRevision = kOwnerRevision;
		}

		void TearDown() override
		{
			gate_lane::ResetForTesting();
			tlsSessionManager.sessions().clear();
			Router() = FakeRouter{};
		}

		static SessionInfo &Session()
		{
			return tlsSessionManager.sessions().at(kSession);
		}

		// 客户端发来一条请求:身份按收包时刻的会话构造(与 HandleRouterForward 同一个构造点)。
		static void Receive(uint64_t clientRequestId, uint32_t messageId)
		{
			SessionInfo &session = Session();
			auto request = std::make_shared<ClientRequest>();
			request->set_id(clientRequestId);
			request->set_message_id(messageId);
			gate_lane::Admit(kSession, session, request,
							 gate_session_details::Build(kSession, session.playerId, 1, "lane-test-gate", &session));
		}

		// 路由服对第 index 条发出的调用回成功信封(路由服原值回显会话头)。
		static void ReplyTo(std::size_t index)
		{
			ASSERT_LT(index, Router().sent.size());
			const SentCall &call = Router().sent[index];
			MessageContent reply;
			reply.set_id(call.clientRequestId);
			reply.set_message_id(call.messageId);
			ASSERT_TRUE(gate_lane::OnRouterReply(call.details, reply));
		}
	};

	// F1:调用没构造出来(签名失败等)时通道没被碰过、不会有任何完成。lane 当场回执(1003),不登记在途,
	// 同会话的下一条立刻发出;计 call_not_built,不混进 no_router。
	TEST_F(ClientRequestLane, CallNotBuiltIsAnsweredAtOnceAndTheNextRequestIsNotBlocked)
	{
		Router().results = {RouterSendResult::kCallNotBuilt};
		Receive(1, kQueryMessageId);
		EXPECT_TRUE(Router().sent.empty());
		EXPECT_TRUE(Session().routerLane.Idle()) << "没发出的那条不能占着在途槽";
		EXPECT_EQ(GetCounters().callNotBuilt, 1u);
		EXPECT_EQ(GetCounters().noRouter, 0u);

		Receive(2, kQuery2MessageId);
		ASSERT_EQ(Router().sent.size(), 1u) << "下一条不用等本地截止(约 9 s)";
		EXPECT_EQ(Router().sent[0].clientRequestId, 2u);
		EXPECT_EQ(GetCounters().sent, 1u);
		EXPECT_EQ(GetCounters().expired, 0u);
	}

	TEST_F(ClientRequestLane, NoRouterIsCountedSeparatelyFromCallNotBuilt)
	{
		Router().results = {RouterSendResult::kNoRouter};
		Receive(1, kQueryMessageId);
		EXPECT_TRUE(Session().routerLane.Idle());
		EXPECT_EQ(GetCounters().noRouter, 1u);
		EXPECT_EQ(GetCounters().callNotBuilt, 0u);
	}

	// F5:排队期间会话的认证绑定变了(换角 / 离场),入队快照不能再签发。拒绝要计数,[lane] 汇总才对得上账。
	TEST_F(ClientRequestLane, StaleIdentityRejectionIsCounted)
	{
		Receive(1, kQueryMessageId);
		Receive(2, kQuery2MessageId);
		ASSERT_EQ(Router().sent.size(), 1u);

		Session().authenticatedAccount = "another-account";
		ReplyTo(0);
		EXPECT_EQ(Router().sent.size(), 1u) << "失配的那条不发出";
		EXPECT_EQ(GetCounters().rejectedStaleIdentity, 1u);
		EXPECT_TRUE(Session().routerLane.Idle());
	}

	// F3:LeaveGame 排在一条慢请求后面。它的会话状态变化(真实钩子里是代发 ExitGame、清路由与原账号)要等它真正发出才生效:
	// 排在它前面的请求先按原身份执行;收到它之后受理的 Login 按离开之后的身份取快照,发出前复核不会误判失配。
	TEST_F(ClientRequestLane, LeaveGameTakesEffectWhenSentInReceiptOrder)
	{
		Receive(1, kQueryMessageId);
		Receive(2, ClientPlayerLoginLeaveGameMessageId);
		ASSERT_EQ(Router().sent.size(), 1u);
		EXPECT_TRUE(gate_lane::HasQueuedRequest(Session(), ClientPlayerLoginLeaveGameMessageId));
		EXPECT_EQ(Session().authenticatedAccount, kAccount) << "收包时不清身份";
		EXPECT_FALSE(Session().sceneRouteResetPending) << "收包时不重置 scene 路由";

		Receive(3, ClientPlayerLoginLoginMessageId);

		ReplyTo(0);
		ASSERT_EQ(Router().sent.size(), 2u);
		EXPECT_EQ(Router().sent[1].messageId, ClientPlayerLoginLeaveGameMessageId);
		EXPECT_EQ(Router().sent[1].details.authenticated_account(), kAccount) << "LeaveGame 自己按原身份发出";
		EXPECT_EQ(Router().afterSend.back(), ClientPlayerLoginLeaveGameMessageId);
		EXPECT_TRUE(Session().authenticatedAccount.empty()) << "发出那一刻才清原账号";
		EXPECT_TRUE(Session().sceneRouteResetPending);
		EXPECT_FALSE(gate_lane::HasQueuedRequest(Session(), ClientPlayerLoginLeaveGameMessageId));

		ReplyTo(1);
		ASSERT_EQ(Router().sent.size(), 3u) << "登出之后紧跟的 Login 照常发出";
		EXPECT_EQ(Router().sent[2].messageId, ClientPlayerLoginLoginMessageId);
		EXPECT_TRUE(Router().sent[2].details.authenticated_account().empty()) << "快照按离开之后的身份取";
		EXPECT_EQ(Router().sent[2].details.owner_revision(), 0u);
		EXPECT_EQ(GetCounters().rejectedStaleIdentity, 0u);
	}

	// F3:LeaveGame 没发出(这里是没有路由服)就回「确定未执行」,会话状态一点不变,与回执一致。
	TEST_F(ClientRequestLane, LeaveGameNotSentLeavesTheSessionUntouched)
	{
		Receive(1, kQueryMessageId);
		Receive(2, ClientPlayerLoginLeaveGameMessageId);
		Router().results = {RouterSendResult::kNoRouter};

		ReplyTo(0);
		EXPECT_EQ(Router().sent.size(), 1u);
		EXPECT_EQ(GetCounters().noRouter, 1u);
		EXPECT_EQ(Router().afterSend.size(), 1u) << "只有第一条发出过,LeaveGame 的钩子没跑";
		EXPECT_EQ(Session().authenticatedAccount, kAccount);
		EXPECT_FALSE(Session().sceneRouteResetPending);
	}

	// F2:重定向时排队项不再发出,全部交出逐条回执、计 dropped_on_redirect;排在里面的 LeaveGame 的会话状态变化不会发生。
	// 在途那一条照常认领,之后不再放行;新请求回 1003。
	TEST_F(ClientRequestLane, RedirectAnswersEveryQueuedRequestAndKeepsTheInFlightOne)
	{
		Receive(1, kQueryMessageId);
		Receive(2, kQuery2MessageId);
		Receive(3, ClientPlayerLoginLeaveGameMessageId);

		gate_lane::OnSessionRedirected(Session());
		EXPECT_EQ(GetCounters().droppedOnRedirect, 2u);
		EXPECT_EQ(Session().routerLane.QueuedCount(), 0u);
		EXPECT_FALSE(Session().routerLane.Idle()) << "在途那一条还在";

		ReplyTo(0);
		EXPECT_EQ(GetCounters().replied, 1u);
		EXPECT_EQ(Router().sent.size(), 1u) << "被交出的排队项不再发出";
		EXPECT_EQ(Session().authenticatedAccount, kAccount) << "没发出的 LeaveGame 不生效";

		Receive(4, kQueryMessageId);
		EXPECT_EQ(GetCounters().rejectedRedirected, 1u);
	}

	// 发出前钩子拒绝(EnterGame 换角闸等):不发出、不跑发出后钩子,计 rejected_before_send。
	TEST_F(ClientRequestLane, BeforeSendRejectionIsAnsweredWithoutSending)
	{
		Router().rejectMessageId = ClientPlayerLoginEnterGameMessageId;
		Router().rejectTip = kLoginInProgress;
		Receive(1, ClientPlayerLoginEnterGameMessageId);
		EXPECT_TRUE(Router().sent.empty());
		EXPECT_TRUE(Router().afterSend.empty());
		EXPECT_EQ(GetCounters().rejectedBeforeSend, 1u);
		EXPECT_TRUE(Session().routerLane.Idle());
	}
} // namespace

// 场景 lane 装配层的单测(handler/rpc/client_scene_lane.h,docs/design/client-scene-request-handoff-hold.md §4)。
//
// 不进 gate.vcxproj,登记在本工程(routing_identity_test.vcxproj),被测源码 client_scene_lane.cpp 直接编进测试。
// 发送函数注入替身:按用例给定的结果依次返回,并记下真正发出去的消息。时刻由用例传入,不读真实时钟。
// 会话不挂连接:回执信封与下发的回包因连接为空不发,用例按计数、发出记录与 lane 状态核对走了哪条分支。
// 纯逻辑(排队、单飞、放回队头、判丢)的用例在 cpp/nodes/gate/tests/scene_lane_test.cpp,同样编进本工程。
//
// 回归对象(2026-11-12):玩家换场景节点的窗口里,发往 scene 的客户端请求原先一律回 1003、由客户端自己重发;
// 现在 scene 答复「确定未执行、稍后重投」的请求留在 gate,按收包顺序重投到玩家当时所在的节点。
//   H1 被答复「稍后重投」的请求放回队头,到点重投;排在它后面的请求不越过它;
//   H2 连续换几次节点(连续几次「稍后重投」)请求一直留着,最终恰好受理一次;
//   H3 等待超过上限就回执、不再等,后面的请求继续往下走;
//   H4 RPC0 失败(结果未知)绝不重投;
//   H5 会话关闭、session_id 被新连接复用之后,旧会话留下的重投登记不会落到新会话上。

#include <gtest/gtest.h>

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <memory>
#include <vector>

#include "handler/rpc/client_request_lane.h"
#include "handler/rpc/client_scene_lane.h"
#include "network/rpc_status.h"
#include "proto/common/base/message.pb.h"
#include "proto/scene/scene.pb.h"
#include "session/manager/session_manager.h"

namespace
{
	using gate_scene_lane::CallSeq;
	using gate_scene_lane::GetCounters;
	using gate_scene_lane::HoldingSessionCount;
	using gate_scene_lane::SceneSendResult;
	using gate_scene_lane::SteadyTime;
	using std::chrono::milliseconds;

	// lane 不按号查注册表,替身也不查,取几个互不相同的值即可。
	constexpr uint32_t kUseItemMessageId = 910001;
	constexpr uint32_t kBuyMessageId = 910002;
	constexpr uint32_t kMoveMessageId = 910003;

	constexpr milliseconds kCallTimeout{12000};

	struct SentToScene
	{
		uint64_t clientRequestId{0};
		uint32_t messageId{0};
		bool oneWay{false};
		CallSeq callSeq{0};
	};

	struct FakeScene
	{
		std::deque<SceneSendResult> results; // 依次返回;用完之后请求-响应回 kSent、单向回 kSentOneWay
		std::vector<SentToScene> sent;       // 真正发出去的(kSent / kSentOneWay),按发出顺序
		uint32_t attempts{0};                // 发送函数被调用的次数(含没发出去的)
	};

	FakeScene &Scene()
	{
		static FakeScene scene;
		return scene;
	}

	SceneSendResult FakeSend(SessionId, const SessionInfo &, const ClientRequest &request, bool oneWay, CallSeq callSeq)
	{
		FakeScene &scene = Scene();
		++scene.attempts;
		SceneSendResult result = oneWay ? SceneSendResult::kSentOneWay : SceneSendResult::kSent;
		if (!scene.results.empty())
		{
			result = scene.results.front();
			scene.results.pop_front();
		}
		if (result == SceneSendResult::kSent || result == SceneSendResult::kSentOneWay)
		{
			scene.sent.push_back(SentToScene{request.id(), request.message_id(), oneWay, callSeq});
		}
		return result;
	}

	SteadyTime At(int ms)
	{
		return SteadyTime{} + milliseconds(ms);
	}

	class ClientSceneLane : public testing::Test
	{
	protected:
		static constexpr SessionId kSession = 4343;

		void SetUp() override
		{
			gate_scene_lane::ResetForTesting();
			tlsSessionManager.sessions().clear();
			Scene() = FakeScene{};
			gate_scene_lane::SetSceneSender(&FakeSend, kCallTimeout);

			SessionInfo &session = tlsSessionManager.sessions()[kSession];
			session.verified = true;
			session.incarnation = gate_lane::NextIncarnation();
		}

		void TearDown() override
		{
			gate_scene_lane::ResetForTesting();
			tlsSessionManager.sessions().clear();
			Scene() = FakeScene{};
		}

		static SessionInfo &Session()
		{
			return tlsSessionManager.sessions().at(kSession);
		}

		// 客户端发来一条发往 scene 的消息。
		static void ClientSends(uint64_t requestId, uint32_t messageId, bool oneWay, SteadyTime now)
		{
			auto request = std::make_shared<ClientRequest>();
			request->set_id(requestId);
			request->set_message_id(messageId);
			gate_scene_lane::Admit(kSession, Session(), request, oneWay, now);
		}

		// scene 受理了当前在途的那一条,回了内容。
		static void SceneAccepts(SteadyTime now)
		{
			ProcessClientPlayerMessageResponse response;
			response.mutable_message_content()->set_message_id(kUseItemMessageId);
			gate_scene_lane::OnSceneCallDone(kSession, Session().incarnation, Session().sceneLane.InFlightSeq(), RpcStatus::Ok(),
											 &response, now);
		}

		// scene 对当前在途的那一条答复「确定未执行、稍后重投」(玩家归属正在交接,或还没在目标节点就绪;
		// 哪些请求会得到这个答复由 scene 的白名单决定,见 cross_zone_test 的 player_handoff_gate_test.cpp)。
		static void SceneAnswersRetryLater(SteadyTime now)
		{
			ProcessClientPlayerMessageResponse response;
			response.set_dispatch_verdict(CLIENT_MESSAGE_DISPATCH_VERDICT_RETRY_LATER);
			gate_scene_lane::OnSceneCallDone(kSession, Session().incarnation, Session().sceneLane.InFlightSeq(), RpcStatus::Ok(),
											 &response, now);
		}

		// 当前在途的那一次 RPC0 调用失败(失败时应答为空)。
		static void SceneCallFails(RpcStatusCode code, SteadyTime now)
		{
			gate_scene_lane::OnSceneCallDone(kSession, Session().incarnation, Session().sceneLane.InFlightSeq(), RpcStatus{code},
											 nullptr, now);
		}
	};

	TEST_F(ClientSceneLane, RequestsAreSentOneAtATimeInReceiptOrder)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		ClientSends(2, kBuyMessageId, false, At(1));
		ASSERT_EQ(Scene().sent.size(), 1u);
		EXPECT_EQ(Scene().sent[0].clientRequestId, 1u);

		SceneAccepts(At(5));
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_EQ(Scene().sent[1].clientRequestId, 2u);
		SceneAccepts(At(9));

		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(GetCounters().admitted, 2u);
		EXPECT_EQ(GetCounters().sent, 2u);
		EXPECT_EQ(GetCounters().replied, 2u);
	}

	TEST_F(ClientSceneLane, OneWayGoesStraightOutWhenTheLaneIsEmpty)
	{
		ClientSends(0, kMoveMessageId, true, At(0));
		ASSERT_EQ(Scene().sent.size(), 1u);
		EXPECT_TRUE(Scene().sent[0].oneWay);
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(GetCounters().oneWayDirect, 1u);
		EXPECT_EQ(GetCounters().admitted, 0u);
	}

	TEST_F(ClientSceneLane, OneWayQueuesBehindAnInFlightRequest)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		ClientSends(0, kMoveMessageId, true, At(1));
		// 移动排在请求后面,不能抢到它前面。
		ASSERT_EQ(Scene().sent.size(), 1u);

		SceneAccepts(At(5));
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_TRUE(Scene().sent[1].oneWay);
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(GetCounters().sentOneWay, 1u);
	}

	// H1
	TEST_F(ClientSceneLane, RetryLaterHoldsTheRequestAndRetriesItBeforeLaterOnes)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		ClientSends(2, kBuyMessageId, false, At(1));
		ASSERT_EQ(Scene().sent.size(), 1u);

		SceneAnswersRetryLater(At(10));
		// 放回队头:此刻什么都不发,排在后面的 2 号请求不越过 1 号。
		EXPECT_EQ(Scene().sent.size(), 1u);
		EXPECT_EQ(HoldingSessionCount(), 1u);
		EXPECT_EQ(GetCounters().held, 1u);
		EXPECT_EQ(GetCounters().retryVerdicts, 1u);

		// 重投间隔没到:这一拍不发。
		gate_scene_lane::Tick(At(100));
		EXPECT_EQ(Scene().sent.size(), 1u);

		// 到点:先重投 1 号(换新的调用序号),仍然不发 2 号。
		gate_scene_lane::Tick(At(250));
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_EQ(Scene().sent[1].clientRequestId, 1u);
		EXPECT_NE(Scene().sent[1].callSeq, Scene().sent[0].callSeq);
		EXPECT_EQ(HoldingSessionCount(), 0u);

		// 新节点受理了 1 号,2 号才发出。
		SceneAccepts(At(260));
		ASSERT_EQ(Scene().sent.size(), 3u);
		EXPECT_EQ(Scene().sent[2].clientRequestId, 2u);
		SceneAccepts(At(265));

		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(GetCounters().replied, 2u);
		EXPECT_EQ(GetCounters().expiredOnHold, 0u);
	}

	// H2
	TEST_F(ClientSceneLane, ConsecutiveHandoffsKeepTheRequestUntilItIsAccepted)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		// A → B:旧节点答复稍后重投;B → C:落到 B 上又赶上下一次交接;第三次还没就绪;第四次被受理。
		SceneAnswersRetryLater(At(10));
		gate_scene_lane::Tick(At(250));
		SceneAnswersRetryLater(At(260));
		gate_scene_lane::Tick(At(500));
		SceneAnswersRetryLater(At(510));
		gate_scene_lane::Tick(At(750));
		SceneAccepts(At(760));

		ASSERT_EQ(Scene().sent.size(), 4u);
		for (const auto &sent : Scene().sent)
		{
			EXPECT_EQ(sent.clientRequestId, 1u);
		}
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);
		EXPECT_EQ(GetCounters().held, 1u);
		EXPECT_EQ(GetCounters().retryVerdicts, 3u);
		EXPECT_EQ(GetCounters().replied, 1u);
		EXPECT_EQ(GetCounters().expiredOnHold, 0u);
	}

	// H3
	TEST_F(ClientSceneLane, HoldGivesUpAfterTheWaitLimitAndLetsTheNextRequestThrough)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		SceneAnswersRetryLater(At(10));
		// 新消息进来也会带动一次放行(重投时刻已过):先重投 1 号,2 号排在它后面。
		ClientSends(2, kBuyMessageId, false, At(1500));
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_EQ(Scene().sent[1].clientRequestId, 1u);

		// 1 号又被答复稍后重投,而它从收包起已经等了 2100 ms(上限 2000):不再等,回执;2 号只等了 600 ms,照常发出。
		SceneAnswersRetryLater(At(2100));
		ASSERT_EQ(Scene().sent.size(), 3u);
		EXPECT_EQ(Scene().sent[2].clientRequestId, 2u);
		EXPECT_EQ(GetCounters().expiredOnHold, 1u);
		EXPECT_EQ(GetCounters().held, 1u);
		EXPECT_EQ(HoldingSessionCount(), 0u);
	}

	// H3:等重投的那一条在定时那一拍发现已经超过上限。
	TEST_F(ClientSceneLane, HeldRequestExpiresAtTheTickAfterTheWaitLimit)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		SceneAnswersRetryLater(At(10));
		gate_scene_lane::Tick(At(2100));
		EXPECT_EQ(Scene().sent.size(), 1u); // 没有再发
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);
		EXPECT_EQ(GetCounters().expiredOnHold, 1u);
	}

	// H3:答复本身来得太晚(例如前一次投递在 scene 那边排了很久)。
	TEST_F(ClientSceneLane, RetryLaterArrivingAfterTheWaitLimitIsNotHeld)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		SceneAnswersRetryLater(At(2500));
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);
		EXPECT_EQ(GetCounters().expiredOnHold, 1u);
		EXPECT_EQ(GetCounters().held, 0u);
		EXPECT_EQ(Scene().sent.size(), 1u);
	}

	TEST_F(ClientSceneLane, HoldDropsOneWayMessagesInsteadOfDeliveringThemLate)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		ClientSends(0, kMoveMessageId, true, At(1)); // 排在在途请求后面
		SceneAnswersRetryLater(At(10));
		EXPECT_EQ(GetCounters().droppedOneWay, 1u); // 排着的那条移动被清掉

		const uint32_t attemptsBefore = Scene().attempts;
		ClientSends(0, kMoveMessageId, true, At(20)); // 等重投期间新来的移动
		EXPECT_EQ(GetCounters().droppedOneWay, 2u);
		EXPECT_EQ(Scene().attempts, attemptsBefore);
		EXPECT_EQ(Session().sceneLane.QueuedCount(), 1u); // 只剩等重投的那条请求
	}

	// H4
	TEST_F(ClientSceneLane, RpcFailureIsAnsweredOnceAndNeverRetried)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		SceneCallFails(RpcStatusCode::kTimeout, At(12000));
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);
		EXPECT_EQ(GetCounters().failed, 1u);

		gate_scene_lane::Tick(At(12250));
		gate_scene_lane::Tick(At(12500));
		EXPECT_EQ(Scene().sent.size(), 1u);
		EXPECT_EQ(Scene().attempts, 1u);
	}

	TEST_F(ClientSceneLane, OneWayWithoutASceneIsAnsweredLikeBefore)
	{
		// 单向消息在会话没有 scene 节点时同样回 1003(改造前「Scene not ready」一支就是这样)。
		Scene().results = {SceneSendResult::kNoScene};
		ClientSends(0, kMoveMessageId, true, At(0));
		EXPECT_TRUE(Scene().sent.empty());
		EXPECT_EQ(GetCounters().noScene, 1u);
		EXPECT_EQ(GetCounters().oneWayDirect, 0u);
		EXPECT_TRUE(Session().sceneLane.Empty());
	}

	TEST_F(ClientSceneLane, NoSceneIsAnsweredAtOnceWithoutHolding)
	{
		Scene().results = {SceneSendResult::kNoScene};
		ClientSends(1, kUseItemMessageId, false, At(0));
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);
		EXPECT_EQ(GetCounters().noScene, 1u);
		EXPECT_EQ(GetCounters().held, 0u);
	}

	TEST_F(ClientSceneLane, CompletionWithAStaleCallSeqIsDroppedAsLate)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		const CallSeq inFlight = Session().sceneLane.InFlightSeq();
		ASSERT_NE(inFlight, gate_scene_lane::kNoCallSeq);

		ProcessClientPlayerMessageResponse response;
		response.mutable_message_content()->set_message_id(kUseItemMessageId);
		gate_scene_lane::OnSceneCallDone(kSession, Session().incarnation, inFlight + 999, RpcStatus::Ok(), &response, At(5));
		EXPECT_EQ(GetCounters().lateDropped, 1u);
		EXPECT_EQ(GetCounters().replied, 0u);
		EXPECT_EQ(Session().sceneLane.InFlightSeq(), inFlight);
	}

	TEST_F(ClientSceneLane, LostCompletionIsFailedOnTheNextAdmitAndItsLateCallbackIsIgnored)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		const CallSeq lostSeq = Session().sceneLane.InFlightSeq();

		// RPC0 超时(12000)+ 余量(1000)之后仍没有完成回调:下一条消息进来时判丢,它自己照常发出。
		ClientSends(2, kBuyMessageId, false, At(13000));
		EXPECT_EQ(GetCounters().inflightLost, 1u);
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_EQ(Scene().sent[1].clientRequestId, 2u);

		// 1 号的回调这时才到:序号对不上,丢弃,不会把 2 号当成它来完成。
		ProcessClientPlayerMessageResponse response;
		response.mutable_message_content()->set_message_id(kUseItemMessageId);
		gate_scene_lane::OnSceneCallDone(kSession, Session().incarnation, lostSeq, RpcStatus::Ok(), &response, At(13010));
		EXPECT_EQ(GetCounters().lateDropped, 1u);
		EXPECT_EQ(GetCounters().replied, 0u);
		EXPECT_NE(Session().sceneLane.InFlightSeq(), gate_scene_lane::kNoCallSeq);
	}

	TEST_F(ClientSceneLane, CompletionAfterTheSessionClosedIsIgnored)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		const CallSeq inFlight = Session().sceneLane.InFlightSeq();
		const uint64_t incarnation = Session().incarnation;
		tlsSessionManager.sessions().erase(kSession);

		ProcessClientPlayerMessageResponse response;
		response.mutable_message_content()->set_message_id(kUseItemMessageId);
		gate_scene_lane::OnSceneCallDone(kSession, incarnation, inFlight, RpcStatus::Ok(), &response, At(5));
		EXPECT_EQ(GetCounters().replied, 0u);
		EXPECT_EQ(GetCounters().lateDropped, 0u);
	}

	// H5
	TEST_F(ClientSceneLane, RetryRegistrationOfAClosedSessionDoesNotLeakIntoANewSessionWithTheSameId)
	{
		ClientSends(1, kUseItemMessageId, false, At(0));
		SceneAnswersRetryLater(At(10));
		ASSERT_EQ(HoldingSessionCount(), 1u);

		// 旧连接断开,同一个 session_id 被新连接复用(新的会话代次,空的 lane)。
		tlsSessionManager.sessions().erase(kSession);
		SessionInfo &fresh = tlsSessionManager.sessions()[kSession];
		fresh.verified = true;
		fresh.incarnation = gate_lane::NextIncarnation();

		const uint32_t attemptsBefore = Scene().attempts;
		gate_scene_lane::Tick(At(250));
		EXPECT_EQ(HoldingSessionCount(), 0u);
		EXPECT_EQ(Scene().attempts, attemptsBefore);
		EXPECT_TRUE(fresh.sceneLane.Empty());
	}
} // namespace

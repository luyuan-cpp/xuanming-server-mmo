// 场景 lane 装配层的单测(handler/rpc/client_scene_lane.h,docs/design/client-scene-request-handoff-hold.md §4)。
//
// 不进 gate.vcxproj,登记在本工程(routing_identity_test.vcxproj),被测源码 client_scene_lane.cpp 直接编进测试。
// 发送函数注入替身:按用例给定的结果依次返回,并记下真正发出去的消息。时刻由用例传入,不读真实时钟。
// 会话不挂连接:回执信封与下发的回包因连接为空不发,用例按计数、发出记录与 lane 状态核对走了哪条分支。
// 纯逻辑(排队、单飞、放回队头)的用例在 cpp/nodes/gate/tests/scene_lane_test.cpp,同样编进本工程。
//
// 回归对象(2026-11-12):玩家换场景节点的窗口里,发往 scene 的客户端请求原先一律回 1003、由客户端自己重发;
// 现在 scene 答复「确定未执行、稍后重投」的请求留在 gate,按收包顺序重投到玩家当时所在的节点。
//   H1 被答复「稍后重投」的请求放回队头,到点重投;排在它后面的请求不越过它;
//   H2 连续换几次节点(连续几次「稍后重投」)请求一直留着,最终恰好受理一次;
//   H3 等待超过上限就回执、不再等,后面的请求继续往下走;
//   H4 RPC0 失败(结果未知)绝不重投;
//   H5 会话关闭、session_id 被新连接复用之后,旧会话留下的重投登记不会落到新会话上;
//   H6 完成回调丢失时由定时扫描判丢,会话的后续请求不会永远卡住;会话先关闭的调用不被当成丢失;
//   H7 会话被重定向到别的 gate 时,还没被受理的请求当场回执,不再重投;在途那一条的应答照常下发;
//   O1 单向消息(移动等)按收包顺序上连接:不越过还没发出的请求,也不被在途请求挡住;等重投期间不等被扣住的那条;
//   R1 排着的时候换过场景或换过节点的消息,发出时在信封上标 received_on_previous_route(scene 据此不在新场景里执行
//      含义绑在旧场景上的消息);没换过的不标。

#include <gtest/gtest.h>

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <memory>
#include <vector>

#include "entt/src/entt/entity/entity.hpp"
#include "handler/rpc/client_request_lane.h"
#include "handler/rpc/client_scene_lane.h"
#include "network/rpc_status.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/node.pb.h"
#include "proto/scene/scene.pb.h"
#include "session/manager/session_manager.h"

namespace
{
	using gate_scene_lane::CallSeq;
	using gate_scene_lane::GetCounters;
	using gate_scene_lane::HoldingSessionCount;
	using gate_scene_lane::InFlightCount;
	using gate_scene_lane::SceneSendResult;
	using gate_scene_lane::SteadyTime;
	using std::chrono::milliseconds;

	// lane 不按号查注册表,替身也不查,取几个互不相同的值即可。
	constexpr uint32_t kEquipMessageId = 910001;
	constexpr uint32_t kMissionMessageId = 910002;
	constexpr uint32_t kMoveMessageId = 910003;

	// 与 gate 的实际取值相同(kGateToSceneClientCallTimeout);本地判丢截止 = 发出时刻 + 本值 + kInflightLostMargin。
	constexpr milliseconds kCallTimeout{12000};

	struct SentToScene
	{
		uint64_t clientRequestId{0};
		uint32_t messageId{0};
		bool oneWay{false};
		CallSeq callSeq{0};
		bool receivedOnPreviousRoute{false};
	};

	struct FakeScene
	{
		std::deque<SceneSendResult> results; // 依次返回;用完之后一律 kSent
		std::vector<SentToScene> sent;       // 真正发出去的(kSent),按发出顺序
		uint32_t attempts{0};                // 发送函数被调用的次数(含没发出去的)
	};

	FakeScene &Scene()
	{
		static FakeScene scene;
		return scene;
	}

	SceneSendResult FakeSend(SessionId, const SessionInfo &, const ClientRequest &request,
							 const gate_scene_lane::SceneSendArgs &args)
	{
		FakeScene &scene = Scene();
		++scene.attempts;
		SceneSendResult result = SceneSendResult::kSent;
		if (!scene.results.empty())
		{
			result = scene.results.front();
			scene.results.pop_front();
		}
		if (result == SceneSendResult::kSent)
		{
			scene.sent.push_back(
				SentToScene{request.id(), request.message_id(), args.oneWay, args.callSeq, args.receivedOnPreviousRoute});
		}
		return result;
	}

	SteadyTime At(int ms)
	{
		return SteadyTime{} + milliseconds(ms);
	}

	// 发出记录里第 index 条的消息号,越界返回 0(让断言给出可读的失败信息,而不是越界访问)。
	uint32_t SentMessageAt(std::size_t index)
	{
		return index < Scene().sent.size() ? Scene().sent[index].messageId : 0;
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

		// 客户端发来一条发往 scene 的请求-响应消息。
		static void ClientSends(uint64_t requestId, uint32_t messageId, SteadyTime now)
		{
			auto request = std::make_shared<ClientRequest>();
			request->set_id(requestId);
			request->set_message_id(messageId);
			gate_scene_lane::Admit(kSession, Session(), request, /*oneWay=*/false, now);
		}

		// 客户端发来一条单向消息(移动等,请求号为 0)。
		static void ClientMoves(SteadyTime now)
		{
			auto request = std::make_shared<ClientRequest>();
			request->set_id(0);
			request->set_message_id(kMoveMessageId);
			gate_scene_lane::Admit(kSession, Session(), request, /*oneWay=*/true, now);
		}

		// scene 受理了当前在途的那一条,回了内容。
		static void SceneAccepts(SteadyTime now)
		{
			ProcessClientPlayerMessageResponse response;
			response.mutable_message_content()->set_message_id(kEquipMessageId);
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
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1));
		ASSERT_EQ(Scene().sent.size(), 1u);
		EXPECT_EQ(Scene().sent[0].clientRequestId, 1u);
		EXPECT_EQ(InFlightCount(), 1u);

		SceneAccepts(At(5));
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_EQ(Scene().sent[1].clientRequestId, 2u);
		SceneAccepts(At(9));

		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(InFlightCount(), 0u);
		EXPECT_EQ(GetCounters().admitted, 2u);
		EXPECT_EQ(GetCounters().sent, 2u);
		EXPECT_EQ(GetCounters().replied, 2u);
	}

	// O1
	TEST_F(ClientSceneLane, OneWayGoesStraightOutWhileARequestIsInFlight)
	{
		ClientSends(1, kEquipMessageId, At(0));
		ClientMoves(At(1));
		// 在途请求挡不住移动:它已经在连接上排在移动前面,scene 按连接顺序执行。
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_TRUE(Scene().sent[1].oneWay);
		EXPECT_EQ(GetCounters().oneWayDirect, 1u);
		EXPECT_EQ(GetCounters().oneWaySent, 1u);
		EXPECT_EQ(GetCounters().oneWayQueued, 0u);
	}

	// O1
	TEST_F(ClientSceneLane, OneWayWaitsBehindAnUnsentRequestAndFollowsItOut)
	{
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1)); // 排在 1 号后面,还没发出
		ClientMoves(At(2));                       // 不能越过 2 号
		ASSERT_EQ(Scene().sent.size(), 1u);
		EXPECT_EQ(GetCounters().oneWayQueued, 1u);

		// 1 号受理:2 号发出,紧跟在它后面的移动马上跟着发出(不等 2 号完成)。
		SceneAccepts(At(5));
		ASSERT_EQ(Scene().sent.size(), 3u);
		EXPECT_EQ(SentMessageAt(1), kMissionMessageId);
		EXPECT_EQ(SentMessageAt(2), kMoveMessageId);
		EXPECT_EQ(GetCounters().oneWaySent, 1u);
	}

	// H1
	TEST_F(ClientSceneLane, RetryLaterHoldsTheRequestAndRetriesItBeforeLaterOnes)
	{
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1));
		ASSERT_EQ(Scene().sent.size(), 1u);

		SceneAnswersRetryLater(At(10));
		// 放回队头:此刻什么都不发,排在后面的 2 号请求不越过 1 号。
		EXPECT_EQ(Scene().sent.size(), 1u);
		EXPECT_EQ(HoldingSessionCount(), 1u);
		EXPECT_EQ(InFlightCount(), 0u);
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
		ClientSends(1, kEquipMessageId, At(0));
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

	// O1:等重投期间移动不等被扣住的那条请求(它只读写玩家自身状态,与移动谁先谁后不改变结果)。
	TEST_F(ClientSceneLane, HoldSendsQueuedOneWayMessagesAtOnceAndLetsNewOnesThrough)
	{
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1));
		ClientMoves(At(2)); // 排在 2 号后面
		ASSERT_EQ(Scene().sent.size(), 1u);

		SceneAnswersRetryLater(At(10));
		// 排着的移动马上发出,2 号仍然留在 1 号后面。
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_EQ(SentMessageAt(1), kMoveMessageId);
		EXPECT_EQ(Session().sceneLane.QueuedCount(), 2u);

		// 等重投期间新来的移动也直接发出。
		ClientMoves(At(50));
		ASSERT_EQ(Scene().sent.size(), 3u);
		EXPECT_EQ(SentMessageAt(2), kMoveMessageId);
		EXPECT_EQ(GetCounters().oneWaySent, 2u);
		EXPECT_EQ(GetCounters().oneWayDropped, 0u);

		// 重投、受理之后照常轮到 2 号。
		gate_scene_lane::Tick(At(250));
		SceneAccepts(At(260));
		ASSERT_EQ(Scene().sent.size(), 5u);
		EXPECT_EQ(Scene().sent[3].clientRequestId, 1u);
		EXPECT_EQ(Scene().sent[4].clientRequestId, 2u);
	}

	// H3
	TEST_F(ClientSceneLane, HoldGivesUpAfterTheWaitLimitAndLetsTheNextRequestThrough)
	{
		ClientSends(1, kEquipMessageId, At(0));
		SceneAnswersRetryLater(At(10));
		// 新请求进来也会带动一次放行(重投时刻已过):先重投 1 号,2 号排在它后面。
		ClientSends(2, kMissionMessageId, At(1500));
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
		ClientSends(1, kEquipMessageId, At(0));
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
		ClientSends(1, kEquipMessageId, At(0));
		SceneAnswersRetryLater(At(2500));
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);
		EXPECT_EQ(GetCounters().expiredOnHold, 1u);
		EXPECT_EQ(GetCounters().held, 0u);
		EXPECT_EQ(Scene().sent.size(), 1u);
	}

	TEST_F(ClientSceneLane, MessagesQueuedTooLongBehindASlowRequestAreNotSent)
	{
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(10));
		ClientMoves(At(20));
		// 1 号的应答 3 秒后才回来:2 号与移动都已经排了将近 3 秒(上限 2000),不再发出。
		// 2 号回带请求 id 的 1003(确定未执行);过时的移动直接丢。
		SceneAccepts(At(3000));
		EXPECT_EQ(Scene().sent.size(), 1u);
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(GetCounters().expiredInQueue, 1u);
		EXPECT_EQ(GetCounters().oneWayDropped, 1u);
		EXPECT_EQ(GetCounters().expiredOnHold, 0u);
	}

	// H4
	TEST_F(ClientSceneLane, RpcFailureIsAnsweredOnceAndNeverRetried)
	{
		ClientSends(1, kEquipMessageId, At(0));
		SceneCallFails(RpcStatusCode::kTimeout, At(12000));
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);
		EXPECT_EQ(InFlightCount(), 0u);
		EXPECT_EQ(GetCounters().failed, 1u);

		gate_scene_lane::Tick(At(12250));
		gate_scene_lane::Tick(At(13500));
		EXPECT_EQ(Scene().sent.size(), 1u);
		EXPECT_EQ(Scene().attempts, 1u);
		EXPECT_EQ(GetCounters().inflightLost, 0u);
	}

	TEST_F(ClientSceneLane, NoSceneIsAnsweredAtOnceWithoutHolding)
	{
		Scene().results = {SceneSendResult::kNoScene};
		ClientSends(1, kEquipMessageId, At(0));
		EXPECT_TRUE(Scene().sent.empty());
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);
		EXPECT_EQ(InFlightCount(), 0u);
		EXPECT_EQ(GetCounters().noScene, 1u);
		EXPECT_EQ(GetCounters().held, 0u);
	}

	TEST_F(ClientSceneLane, OneWayWithoutASceneIsAnsweredLikeBefore)
	{
		// 单向消息在会话没有 scene 节点时同样回 1003(改造前「Scene not ready」一支就是这样)。
		Scene().results = {SceneSendResult::kNoScene};
		ClientMoves(At(0));
		EXPECT_TRUE(Scene().sent.empty());
		EXPECT_EQ(GetCounters().noScene, 1u);
		EXPECT_EQ(GetCounters().oneWaySent, 0u);
		EXPECT_TRUE(Session().sceneLane.Empty());
	}

	TEST_F(ClientSceneLane, FullLaneRejectsTheNewRequestAndKeepsTheQueuedOnes)
	{
		// 1 条在途 + 63 条排队 = 上限 64。
		for (uint64_t id = 1; id <= gate_scene_lane::kMaxSceneLaneDepth; ++id)
		{
			ClientSends(id, kEquipMessageId, At(0));
		}
		ASSERT_EQ(Session().sceneLane.Depth(), gate_scene_lane::kMaxSceneLaneDepth);
		EXPECT_EQ(GetCounters().rejectedFull, 0u);

		ClientSends(999, kEquipMessageId, At(1));
		EXPECT_EQ(GetCounters().rejectedFull, 1u);
		ClientMoves(At(1)); // 排不进去的移动直接丢
		EXPECT_EQ(GetCounters().oneWayDropped, 1u);
		EXPECT_EQ(Session().sceneLane.Depth(), gate_scene_lane::kMaxSceneLaneDepth);
		EXPECT_EQ(GetCounters().admitted, gate_scene_lane::kMaxSceneLaneDepth);

		// 排着的请求不受影响:上一条受理后照常轮到下一条。
		SceneAccepts(At(2));
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_EQ(Scene().sent[1].clientRequestId, 2u);
	}

	TEST_F(ClientSceneLane, CompletionWithAStaleCallSeqIsDroppedAsLate)
	{
		ClientSends(1, kEquipMessageId, At(0));
		const CallSeq inFlight = Session().sceneLane.InFlightSeq();
		ASSERT_NE(inFlight, gate_scene_lane::kNoCallSeq);

		ProcessClientPlayerMessageResponse response;
		response.mutable_message_content()->set_message_id(kEquipMessageId);
		gate_scene_lane::OnSceneCallDone(kSession, Session().incarnation, inFlight + 999, RpcStatus::Ok(), &response, At(5));
		EXPECT_EQ(GetCounters().lateDropped, 1u);
		EXPECT_EQ(GetCounters().replied, 0u);
		EXPECT_EQ(Session().sceneLane.InFlightSeq(), inFlight);
	}

	// H6
	TEST_F(ClientSceneLane, LostCompletionIsFailedByTheTickAndItsLateCallbackIsIgnored)
	{
		ClientSends(1, kEquipMessageId, At(0));
		const CallSeq lostSeq = Session().sceneLane.InFlightSeq();

		// RPC0 超时(12000)+ 余量(1000)之前不判丢。
		gate_scene_lane::Tick(At(12999));
		EXPECT_EQ(GetCounters().inflightLost, 0u);
		EXPECT_EQ(Session().sceneLane.InFlightSeq(), lostSeq);

		// 到了截止仍没有完成回调:定时扫描判丢,客户端不必再发消息来触发。
		gate_scene_lane::Tick(At(13000));
		EXPECT_EQ(GetCounters().inflightLost, 1u);
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(InFlightCount(), 0u);

		// 会话的后续请求照常往下走。
		ClientSends(2, kMissionMessageId, At(13001));
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_EQ(Scene().sent[1].clientRequestId, 2u);

		// 1 号的回调这时才到:序号对不上,丢弃,不会把 2 号当成它来完成,也不会出现第二个回执。
		ProcessClientPlayerMessageResponse response;
		response.mutable_message_content()->set_message_id(kEquipMessageId);
		gate_scene_lane::OnSceneCallDone(kSession, Session().incarnation, lostSeq, RpcStatus::Ok(), &response, At(13010));
		EXPECT_EQ(GetCounters().lateDropped, 1u);
		EXPECT_EQ(GetCounters().replied, 0u);
		EXPECT_NE(Session().sceneLane.InFlightSeq(), gate_scene_lane::kNoCallSeq);
	}

	// H6:会话先关闭的调用由它迟到的完成回调注销,不被定时扫描当成丢失。
	TEST_F(ClientSceneLane, CompletionAfterTheSessionClosedIsIgnoredAndNotCountedAsLost)
	{
		ClientSends(1, kEquipMessageId, At(0));
		const CallSeq inFlight = Session().sceneLane.InFlightSeq();
		const uint64_t incarnation = Session().incarnation;
		ASSERT_EQ(InFlightCount(), 1u);
		tlsSessionManager.sessions().erase(kSession);

		ProcessClientPlayerMessageResponse response;
		response.mutable_message_content()->set_message_id(kEquipMessageId);
		gate_scene_lane::OnSceneCallDone(kSession, incarnation, inFlight, RpcStatus::Ok(), &response, At(5));
		EXPECT_EQ(GetCounters().replied, 0u);
		EXPECT_EQ(GetCounters().lateDropped, 0u);
		EXPECT_EQ(InFlightCount(), 0u);

		gate_scene_lane::Tick(At(20000));
		EXPECT_EQ(GetCounters().inflightLost, 0u);
	}

	// H7:跨 zone 传送,会话被重定向到别的 gate(gate_redirect_delivery.cpp 在推 msg 124 之前调 OnSessionRedirected)。
	TEST_F(ClientSceneLane, RedirectAnswersHeldAndQueuedRequestsAtOnce)
	{
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1));
		ClientMoves(At(2));             // 排在 2 号后面
		SceneAnswersRetryLater(At(10)); // 1 号在等重投,2 号排在后面,移动已经发出
		ASSERT_EQ(HoldingSessionCount(), 1u);

		gate_scene_lane::OnSessionRedirected(kSession, Session());
		EXPECT_EQ(GetCounters().droppedOnRedirect, 2u);
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);

		// 之后不再重投:玩家正在离开,这些请求由客户端到新 gate 上重发。
		const uint32_t attemptsBefore = Scene().attempts;
		gate_scene_lane::Tick(At(250));
		EXPECT_EQ(Scene().attempts, attemptsBefore);
	}

	TEST_F(ClientSceneLane, RedirectDropsQueuedOneWayMessagesSilently)
	{
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1));
		ClientMoves(At(2)); // 排在还没发出的 2 号后面

		gate_scene_lane::OnSessionRedirected(kSession, Session());
		EXPECT_EQ(GetCounters().droppedOnRedirect, 1u); // 只有 2 号回执
		EXPECT_EQ(GetCounters().oneWayDropped, 1u);
		// 在途的 1 号不动。
		EXPECT_NE(Session().sceneLane.InFlightSeq(), gate_scene_lane::kNoCallSeq);
	}

	// H7:重定向时在途的那一条不动,它的应答照常下发;若被答复「稍后重投」也不再等,当场回执。
	TEST_F(ClientSceneLane, InFlightRequestSurvivesRedirectButIsNoLongerHeld)
	{
		ClientSends(1, kEquipMessageId, At(0));
		gate_scene_lane::OnSessionRedirected(kSession, Session());
		Session().redirectedAt = At(5); // 推完 124 之后由 gate_drain_migration::NoteRedirected 置位
		EXPECT_EQ(GetCounters().droppedOnRedirect, 0u);
		ASSERT_NE(Session().sceneLane.InFlightSeq(), gate_scene_lane::kNoCallSeq);

		SceneAnswersRetryLater(At(10));
		EXPECT_EQ(GetCounters().droppedOnRedirect, 1u);
		EXPECT_EQ(GetCounters().held, 0u);
		EXPECT_TRUE(Session().sceneLane.Empty());
		EXPECT_EQ(HoldingSessionCount(), 0u);
	}

	TEST_F(ClientSceneLane, ReplyToTheInFlightRequestIsStillDeliveredAfterRedirect)
	{
		ClientSends(1, kEquipMessageId, At(0));
		gate_scene_lane::OnSessionRedirected(kSession, Session());
		Session().redirectedAt = At(5);

		SceneAccepts(At(10));
		EXPECT_EQ(GetCounters().replied, 1u);
		EXPECT_EQ(GetCounters().droppedOnRedirect, 0u);
		EXPECT_TRUE(Session().sceneLane.Empty());
	}

	// R1:没换过路由的消息不标。
	TEST_F(ClientSceneLane, MessagesSentOnTheSameRouteAreNotMarked)
	{
		Session().sceneId = 7001;
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1));
		ClientMoves(At(2));
		SceneAccepts(At(5));
		ASSERT_EQ(Scene().sent.size(), 3u);
		for (const auto &sent : Scene().sent)
		{
			EXPECT_FALSE(sent.receivedOnPreviousRoute) << sent.messageId;
		}
		EXPECT_EQ(GetCounters().sentOnNewRoute, 0u);
	}

	// R1:被扣住的请求重投时,会话已经换到新场景:标出来(它在白名单里,scene 照常执行);
	// 排在它后面、换场景之后才发出的请求同样标出来,由 scene 决定执不执行。
	TEST_F(ClientSceneLane, RequestsSentAfterASceneChangeAreMarked)
	{
		Session().sceneId = 7001;
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1));
		SceneAnswersRetryLater(At(10));

		Session().sceneId = 7002; // 路由切到新场景(RoutePlayerEvent 提交)
		ClientSends(3, kMissionMessageId, At(20)); // 换场景之后才收到的请求

		gate_scene_lane::Tick(At(250));
		SceneAccepts(At(260));
		SceneAccepts(At(270));
		ASSERT_EQ(Scene().sent.size(), 4u);
		EXPECT_FALSE(Scene().sent[0].receivedOnPreviousRoute) << "first attempt on the original route";
		EXPECT_TRUE(Scene().sent[1].receivedOnPreviousRoute) << "the held request replayed after the scene change";
		EXPECT_TRUE(Scene().sent[2].receivedOnPreviousRoute) << "request 2 was received before the scene change";
		EXPECT_FALSE(Scene().sent[3].receivedOnPreviousRoute) << "request 3 was received after the scene change";
		EXPECT_EQ(GetCounters().sentOnNewRoute, 2u);
	}

	// R1:换节点(scene 节点实体变了)同样算换路由;排着的移动跟着请求发出时也标出来。
	TEST_F(ClientSceneLane, QueuedOneWayCrossingANodeChangeIsMarked)
	{
		Session().sceneId = 7001;
		Session().SetNodeEntity(common::base::SceneNodeService, entt::entity{3});
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1));
		ClientMoves(At(2)); // 排在 2 号后面

		Session().SetNodeEntity(common::base::SceneNodeService, entt::entity{4}); // 同一场景实例搬到了别的节点
		SceneAccepts(At(5));
		ASSERT_EQ(Scene().sent.size(), 3u);
		EXPECT_TRUE(Scene().sent[1].receivedOnPreviousRoute);
		EXPECT_TRUE(Scene().sent[2].oneWay);
		EXPECT_TRUE(Scene().sent[2].receivedOnPreviousRoute);
	}

	// R1:等重投时马上发出的那批单向消息,按各自收到时的路由判。
	TEST_F(ClientSceneLane, OneWayFlushedAtHoldIsMarkedByItsOwnReceiptRoute)
	{
		Session().sceneId = 7001;
		ClientSends(1, kEquipMessageId, At(0));
		ClientSends(2, kMissionMessageId, At(1));
		ClientMoves(At(2)); // 排在 2 号后面,收到时还在 7001
		Session().sceneId = 7002;
		SceneAnswersRetryLater(At(10)); // 扣住 1 号,排着的移动马上发出
		ASSERT_EQ(Scene().sent.size(), 2u);
		EXPECT_TRUE(Scene().sent[1].oneWay);
		EXPECT_TRUE(Scene().sent[1].receivedOnPreviousRoute);

		ClientMoves(At(20)); // 换场景之后收到、等重投期间直发的移动:不标
		ASSERT_EQ(Scene().sent.size(), 3u);
		EXPECT_FALSE(Scene().sent[2].receivedOnPreviousRoute);
	}

	// H5
	TEST_F(ClientSceneLane, RetryRegistrationOfAClosedSessionDoesNotLeakIntoANewSessionWithTheSameId)
	{
		ClientSends(1, kEquipMessageId, At(0));
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

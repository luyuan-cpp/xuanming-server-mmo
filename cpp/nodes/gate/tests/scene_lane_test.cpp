// 会话场景 lane 纯逻辑单测(docs/design/client-scene-request-handoff-hold.md §4)。
//
// 不进 gate.vcxproj 的 ClCompile 列表,不被编进 gate 可执行文件;登记在
// cpp/tests/routing_identity_test/routing_identity_test.vcxproj,随该工程跑(run_cpp_tests.ps1 -Filter routing_identity_test)。
// 装配层(client_scene_lane.cpp)的用例在同工程的 client_scene_lane_test.cpp。
// scene_lane.h 只依赖 STL,这份测试也能脱离引擎单独编译:
//
//   GT=third_party/googletest/googletest
//   g++ -std=c++23 -I cpp/libs/services/gate/session/comp -I "$GT/include" -I "$GT"
//       cpp/nodes/gate/tests/scene_lane_test.cpp
//       "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc"
//       -lpthread -o /tmp/scene_lane_test
//   /tmp/scene_lane_test
//
// (上面四行是同一条命令,换行只为可读;实际执行时接成一行。)

#include <gtest/gtest.h>

#include <chrono>
#include <cstddef>
#include <optional>
#include <string>
#include <utility>
#include <vector>

#include "scene_lane.h"

namespace
{

using gate_scene_lane::AdmitResult;
using gate_scene_lane::CallSeq;
using gate_scene_lane::kNoCallSeq;
using gate_scene_lane::SceneLane;
using gate_scene_lane::SteadyTime;
using gate_scene_lane::WaitExceeded;
using std::chrono::milliseconds;

struct TestItem
{
	std::string name;
	bool oneWay{false};
};

constexpr std::size_t kDepth = 64;

SteadyTime At(int ms)
{
	return SteadyTime{} + milliseconds(ms);
}

TestItem Request(std::string name)
{
	return TestItem{std::move(name), false};
}

TestItem OneWay(std::string name)
{
	return TestItem{std::move(name), true};
}

// 取出队头的名字;取不到返回空串。不做 MarkSent,由用例自己决定这一条怎么处置。
std::string TakeName(SceneLane<TestItem> &lane, SteadyTime now)
{
	auto item = lane.TakeNextIfReady(now);
	return item ? item->name : std::string{};
}

// 取出队头并作为请求-响应消息发出;返回取到的名字,取不到返回空串。
std::string SendNext(SceneLane<TestItem> &lane, CallSeq seq, SteadyTime now)
{
	auto item = lane.TakeNextIfReady(now);
	if (!item)
	{
		return {};
	}
	std::string name = item->name;
	EXPECT_TRUE(lane.MarkSent(std::move(*item), seq, now + milliseconds(13000)));
	return name;
}

TEST(SceneLane, SendsRequestsInReceiptOrderOneAtATime)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("b"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("c"), kDepth), AdmitResult::kQueued);

	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	// a 在途期间不出队。
	EXPECT_EQ(TakeName(lane, At(1)), "");
	EXPECT_EQ(lane.InFlightSeq(), 1u);
	EXPECT_EQ(lane.Depth(), 3u);

	auto done = lane.Complete(1);
	ASSERT_TRUE(done.has_value());
	EXPECT_EQ(done->name, "a");
	EXPECT_EQ(SendNext(lane, 2, At(2)), "b");
	ASSERT_TRUE(lane.Complete(2).has_value());
	EXPECT_EQ(SendNext(lane, 3, At(3)), "c");
	ASSERT_TRUE(lane.Complete(3).has_value());
	EXPECT_TRUE(lane.Empty());
}

TEST(SceneLane, OneWayItemsKeepReceiptOrderButDoNotOccupyInFlight)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(OneWay("move1"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("use_item"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(OneWay("move2"), kDepth), AdmitResult::kQueued);

	// 单向消息取出即算处置完,不 MarkSent,紧接着能取下一条。
	EXPECT_EQ(TakeName(lane, At(0)), "move1");
	EXPECT_EQ(SendNext(lane, 7, At(0)), "use_item");
	// 排在请求后面的单向消息不能抢到它前面:请求在途期间不出队。
	EXPECT_EQ(TakeName(lane, At(1)), "");
	ASSERT_TRUE(lane.Complete(7).has_value());
	EXPECT_EQ(TakeName(lane, At(2)), "move2");
	EXPECT_TRUE(lane.Empty());
}

TEST(SceneLane, HeldItemGoesBackToHeadAndNothingIsSentBeforeRetryTime)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("b"), kDepth), AdmitResult::kQueued);

	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	auto notExecuted = lane.Complete(1);
	ASSERT_TRUE(notExecuted.has_value());
	EXPECT_EQ(lane.HoldForRetry(std::move(*notExecuted), At(200)), 0u);
	EXPECT_TRUE(lane.Holding());
	EXPECT_FALSE(lane.Empty());

	// 重投时刻之前什么都不发:b 不能越过 a。
	EXPECT_EQ(TakeName(lane, At(199)), "");
	EXPECT_TRUE(lane.Holding());
	// 到点:先重投 a,再轮到 b。
	EXPECT_EQ(SendNext(lane, 2, At(200)), "a");
	EXPECT_FALSE(lane.Holding());
	EXPECT_EQ(TakeName(lane, At(201)), "");
	ASSERT_TRUE(lane.Complete(2).has_value());
	EXPECT_EQ(SendNext(lane, 3, At(202)), "b");
}

// 回归对象:玩家连续换几次场景节点(A → B → C),同一条请求连续几次被答复「稍后重投」。
// 队列一直留在 gate 的会话上,最终执行顺序仍是收包顺序,每条恰好被受理一次。
TEST(SceneLane, ReceiptOrderSurvivesRepeatedHoldsAcrossConsecutiveHandoffs)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("b"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("c"), kDepth), AdmitResult::kQueued);

	std::vector<std::string> executed;
	CallSeq seq = 0;
	int nowMs = 0;
	// 每条请求前两次投递都被答复「稍后重投」(第一次交接、紧接着第二次交接),第三次才被受理。
	for (int accepted = 0; accepted < 3; ++accepted)
	{
		for (int attempt = 0; attempt < 3; ++attempt)
		{
			auto item = lane.TakeNextIfReady(At(nowMs));
			ASSERT_TRUE(item.has_value());
			std::string name = item->name;
			ASSERT_TRUE(lane.MarkSent(std::move(*item), ++seq, At(nowMs + 13000)));
			auto done = lane.Complete(seq);
			ASSERT_TRUE(done.has_value());
			if (attempt < 2)
			{
				lane.HoldForRetry(std::move(*done), At(nowMs + 200));
				// 等重投期间后面的请求一条都不发。
				EXPECT_FALSE(lane.TakeNextIfReady(At(nowMs + 100)).has_value());
				nowMs += 200;
			}
			else
			{
				executed.push_back(name);
			}
		}
	}
	EXPECT_EQ(executed, (std::vector<std::string>{"a", "b", "c"}));
	EXPECT_TRUE(lane.Empty());
}

TEST(SceneLane, HoldPurgesQueuedOneWayItemsAndRefusesNewOnes)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	// a 在途时排进来的消息。
	ASSERT_EQ(lane.Admit(OneWay("move1"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("b"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(OneWay("move2"), kDepth), AdmitResult::kQueued);

	auto notExecuted = lane.Complete(1);
	ASSERT_TRUE(notExecuted.has_value());
	// 玩家在换节点:排着的两条移动已经没有意义,清掉;请求保留。
	EXPECT_EQ(lane.HoldForRetry(std::move(*notExecuted), At(200)), 2u);
	EXPECT_EQ(lane.QueuedCount(), 2u);

	// 等重投期间:新的单向消息不入队,新的请求照常排在后面。
	EXPECT_EQ(lane.Admit(OneWay("move3"), kDepth), AdmitResult::kDroppedOneWayOnHold);
	EXPECT_EQ(lane.Admit(Request("c"), kDepth), AdmitResult::kQueued);
	EXPECT_EQ(lane.QueuedCount(), 3u);

	EXPECT_EQ(SendNext(lane, 2, At(200)), "a");
	ASSERT_TRUE(lane.Complete(2).has_value());
	EXPECT_EQ(SendNext(lane, 3, At(201)), "b");
	ASSERT_TRUE(lane.Complete(3).has_value());
	EXPECT_EQ(SendNext(lane, 4, At(202)), "c");
}

TEST(SceneLane, OneWayIsAcceptedAgainOnceTheHeldItemIsRetried)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	auto item = lane.TakeNextIfReady(At(0));
	ASSERT_TRUE(item.has_value());
	lane.HoldForRetry(std::move(*item), At(200));
	EXPECT_EQ(lane.Admit(OneWay("move1"), kDepth), AdmitResult::kDroppedOneWayOnHold);

	// 重投已经发出(在途):不再是「等重投」,单向消息重新排在它后面。
	EXPECT_EQ(SendNext(lane, 1, At(200)), "a");
	EXPECT_FALSE(lane.Holding());
	EXPECT_EQ(lane.Admit(OneWay("move2"), kDepth), AdmitResult::kQueued);
	ASSERT_TRUE(lane.Complete(1).has_value());
	EXPECT_EQ(TakeName(lane, At(201)), "move2");
}

TEST(SceneLane, CompleteOnlyClaimsTheCurrentInFlightSeq)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	EXPECT_FALSE(lane.Complete(1).has_value()); // 没有在途
	EXPECT_EQ(SendNext(lane, 5, At(0)), "a");
	EXPECT_FALSE(lane.Complete(kNoCallSeq).has_value());
	EXPECT_FALSE(lane.Complete(4).has_value()); // 串号
	EXPECT_EQ(lane.InFlightSeq(), 5u);
	ASSERT_TRUE(lane.Complete(5).has_value());
	EXPECT_FALSE(lane.Complete(5).has_value()); // 重复完成
}

TEST(SceneLane, MarkSentRefusesWhenBusyOrWithoutSeq)
{
	SceneLane<TestItem> lane;
	EXPECT_FALSE(lane.MarkSent(Request("x"), kNoCallSeq, At(13000)));
	EXPECT_TRUE(lane.Empty());
	EXPECT_TRUE(lane.MarkSent(Request("a"), 1, At(13000)));
	EXPECT_FALSE(lane.MarkSent(Request("b"), 2, At(13000)));
	EXPECT_EQ(lane.InFlightSeq(), 1u);
	auto done = lane.Complete(1);
	ASSERT_TRUE(done.has_value());
	EXPECT_EQ(done->name, "a");
}

// 回归对象:完成回调万一丢了,这条会话的 scene 请求不能永远卡在它后面。
TEST(SceneLane, LostInFlightItemIsHandedBackOnceAndLateCompletionIsIgnored)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("b"), kDepth), AdmitResult::kQueued);
	auto item = lane.TakeNextIfReady(At(0));
	ASSERT_TRUE(item.has_value());
	ASSERT_TRUE(lane.MarkSent(std::move(*item), 1, At(13000)));

	EXPECT_FALSE(lane.ExpireInFlightIfLost(At(12999)).has_value());
	auto lost = lane.ExpireInFlightIfLost(At(13000));
	ASSERT_TRUE(lost.has_value());
	EXPECT_EQ(lost->name, "a");
	EXPECT_FALSE(lane.ExpireInFlightIfLost(At(20000)).has_value()); // 只交还一次
	// 迟到的完成对不上号,不会再被认领(不会出现第二个回执)。
	EXPECT_FALSE(lane.Complete(1).has_value());
	// 后面的请求继续往下走。
	EXPECT_EQ(SendNext(lane, 2, At(13001)), "b");
}

TEST(SceneLane, DepthLimitCountsInFlightAndQueued)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), 2), AdmitResult::kQueued);
	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	ASSERT_EQ(lane.Admit(Request("b"), 2), AdmitResult::kQueued);
	EXPECT_EQ(lane.Admit(Request("c"), 2), AdmitResult::kRejectedFull);
	EXPECT_EQ(lane.Admit(OneWay("move"), 2), AdmitResult::kRejectedFull);
	ASSERT_TRUE(lane.Complete(1).has_value());
	EXPECT_EQ(lane.Admit(Request("c"), 2), AdmitResult::kQueued);
}

TEST(SceneLane, WaitExceededIsStrictlyAfterTheLimit)
{
	EXPECT_FALSE(WaitExceeded(At(0), At(2000), milliseconds(2000)));
	EXPECT_TRUE(WaitExceeded(At(0), At(2001), milliseconds(2000)));
}

} // namespace

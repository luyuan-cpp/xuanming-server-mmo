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

TestItem Request(std::string name)
{
	return TestItem{std::move(name), false};
}

TestItem OneWay(std::string name)
{
	return TestItem{std::move(name), true};
}

constexpr std::size_t kDepth = 64;

SteadyTime At(int ms)
{
	return SteadyTime{} + milliseconds(ms);
}

// 取出队头的名字;取不到返回空串。取出的条目被丢弃(单向消息取出即算发出)。
std::string TakeName(SceneLane<TestItem> &lane, SteadyTime now)
{
	auto item = lane.TakeNextIfReady(now);
	return item ? item->name : std::string{};
}

// 取出队头的请求并发出;返回取到的名字,取不到返回空串。
std::string SendNext(SceneLane<TestItem> &lane, CallSeq seq, SteadyTime now)
{
	auto item = lane.TakeNextIfReady(now);
	if (!item)
	{
		return {};
	}
	std::string name = item->name;
	EXPECT_FALSE(item->oneWay) << name;
	EXPECT_TRUE(lane.MarkSent(std::move(*item), seq));
	return name;
}

std::vector<std::string> Names(const std::vector<TestItem> &items)
{
	std::vector<std::string> names;
	for (const auto &item : items)
	{
		names.push_back(item.name);
	}
	return names;
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

TEST(SceneLane, OneWayIsNotBlockedByAnInFlightRequest)
{
	// 在途那一条已经在连接上排在前面:单向消息不用等它完成,scene 按连接顺序执行,先后仍是收包顺序。
	SceneLane<TestItem> lane;
	EXPECT_TRUE(lane.OneWayMaySkipQueue());
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	EXPECT_FALSE(lane.OneWayMaySkipQueue()) << "a 还没发出,移动不能越过它";
	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	EXPECT_TRUE(lane.OneWayMaySkipQueue());
}

TEST(SceneLane, OneWayWaitsBehindAnUnsentRequestAndGoesRightAfterIt)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("r1"), kDepth), AdmitResult::kQueued);
	EXPECT_EQ(SendNext(lane, 1, At(0)), "r1");
	ASSERT_EQ(lane.Admit(Request("r2"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(OneWay("move"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("r3"), kDepth), AdmitResult::kQueued);

	// r1 在途:r2 不能发,排在 r2 后面的移动也不能越过它。
	EXPECT_EQ(TakeName(lane, At(1)), "");

	ASSERT_TRUE(lane.Complete(1).has_value());
	EXPECT_EQ(SendNext(lane, 2, At(2)), "r2");
	// r2 已经发出:紧跟在它后面的移动可以马上发,r3 要等 r2 完成。
	EXPECT_EQ(TakeName(lane, At(2)), "move");
	EXPECT_EQ(TakeName(lane, At(2)), "");
	ASSERT_TRUE(lane.Complete(2).has_value());
	EXPECT_EQ(SendNext(lane, 3, At(3)), "r3");
}

TEST(SceneLane, HeldItemGoesBackToHeadAndNoRequestIsSentBeforeRetryTime)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("b"), kDepth), AdmitResult::kQueued);

	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	auto notExecuted = lane.Complete(1);
	ASSERT_TRUE(notExecuted.has_value());
	EXPECT_TRUE(lane.HoldForRetry(std::move(*notExecuted), At(200)).empty());
	EXPECT_TRUE(lane.Holding());
	EXPECT_FALSE(lane.Empty());
	EXPECT_EQ(lane.QueuedCount(), 2u);

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
			ASSERT_TRUE(lane.MarkSent(std::move(*item), ++seq));
			auto done = lane.Complete(seq);
			ASSERT_TRUE(done.has_value());
			if (attempt < 2)
			{
				EXPECT_TRUE(lane.HoldForRetry(std::move(*done), At(nowMs + 200)).empty());
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

TEST(SceneLane, HoldHandsBackQueuedOneWayItemsInOrderAndKeepsQueuedRequests)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("r1"), kDepth), AdmitResult::kQueued);
	EXPECT_EQ(SendNext(lane, 1, At(0)), "r1");
	// r1 在途时依次收到:移动、请求、移动。
	ASSERT_EQ(lane.Admit(OneWay("m1"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(Request("r2"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(OneWay("m2"), kDepth), AdmitResult::kQueued);

	auto notExecuted = lane.Complete(1);
	ASSERT_TRUE(notExecuted.has_value());
	// 玩家在换节点:两条移动交还给调用方马上发(按收包顺序),r2 留在 r1 后面。
	EXPECT_EQ(Names(lane.HoldForRetry(std::move(*notExecuted), At(200))), (std::vector<std::string>{"m1", "m2"}));
	EXPECT_EQ(lane.QueuedCount(), 2u);
	// 等重投期间新到的移动也不再等 r1。
	EXPECT_TRUE(lane.OneWayMaySkipQueue());

	EXPECT_EQ(SendNext(lane, 2, At(200)), "r1");
	ASSERT_TRUE(lane.Complete(2).has_value());
	EXPECT_EQ(SendNext(lane, 3, At(201)), "r2");
}

TEST(SceneLane, RequestsAdmittedDuringAHoldQueueBehindTheHeldOne)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	auto notExecuted = lane.Complete(1);
	ASSERT_TRUE(notExecuted.has_value());
	EXPECT_TRUE(lane.HoldForRetry(std::move(*notExecuted), At(200)).empty());

	// 玩家在换节点期间又发来的请求:排在等重投的那条后面。
	ASSERT_EQ(lane.Admit(Request("b"), kDepth), AdmitResult::kQueued);
	EXPECT_EQ(TakeName(lane, At(100)), "");

	EXPECT_EQ(SendNext(lane, 2, At(200)), "a");
	ASSERT_TRUE(lane.Complete(2).has_value());
	EXPECT_EQ(SendNext(lane, 3, At(201)), "b");
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
	EXPECT_FALSE(lane.Complete(5).has_value()); // 重复完成:每个请求恰好一个回执
}

TEST(SceneLane, RetriedItemIsClaimedOnlyByItsNewSeq)
{
	// 每次发出都换新号:上一次投递的迟到完成不会把这一次重投当成自己来认领。
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	auto notExecuted = lane.Complete(1);
	ASSERT_TRUE(notExecuted.has_value());
	EXPECT_TRUE(lane.HoldForRetry(std::move(*notExecuted), At(200)).empty());
	EXPECT_EQ(SendNext(lane, 2, At(200)), "a");

	EXPECT_FALSE(lane.Complete(1).has_value());
	EXPECT_EQ(lane.InFlightSeq(), 2u);
	ASSERT_TRUE(lane.Complete(2).has_value());
}

TEST(SceneLane, MarkSentRefusesWhenBusyOrWithoutSeq)
{
	SceneLane<TestItem> lane;
	EXPECT_FALSE(lane.MarkSent(Request("x"), kNoCallSeq));
	EXPECT_TRUE(lane.Empty());
	EXPECT_TRUE(lane.MarkSent(Request("a"), 1));
	EXPECT_FALSE(lane.MarkSent(Request("b"), 2));
	EXPECT_EQ(lane.InFlightSeq(), 1u);
	auto done = lane.Complete(1);
	ASSERT_TRUE(done.has_value());
	EXPECT_EQ(done->name, "a");
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

// 会话转去别的 gate:还没被受理的消息(等重投的那条 + 排在后面的)全部交还,由调用方逐条处置。
TEST(SceneLane, TakeAllQueuedHandsBackEverythingNotInFlightAndEndsTheHold)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	auto notExecuted = lane.Complete(1);
	ASSERT_TRUE(notExecuted.has_value());
	EXPECT_TRUE(lane.HoldForRetry(std::move(*notExecuted), At(200)).empty());
	ASSERT_EQ(lane.Admit(Request("b"), kDepth), AdmitResult::kQueued);

	const auto dropped = lane.TakeAllQueued();
	ASSERT_EQ(dropped.size(), 2u);
	EXPECT_EQ(dropped[0].name, "a");
	EXPECT_EQ(dropped[1].name, "b");
	EXPECT_FALSE(lane.Holding());
	EXPECT_TRUE(lane.Empty());
	// 之后再没有东西可重投。
	EXPECT_EQ(TakeName(lane, At(200)), "");
}

TEST(SceneLane, TakeAllQueuedLeavesTheInFlightItemToBeCompleted)
{
	SceneLane<TestItem> lane;
	ASSERT_EQ(lane.Admit(Request("a"), kDepth), AdmitResult::kQueued);
	EXPECT_EQ(SendNext(lane, 1, At(0)), "a");
	ASSERT_EQ(lane.Admit(Request("b"), kDepth), AdmitResult::kQueued);
	ASSERT_EQ(lane.Admit(OneWay("move"), kDepth), AdmitResult::kQueued);

	const auto dropped = lane.TakeAllQueued();
	ASSERT_EQ(dropped.size(), 2u);
	EXPECT_EQ(dropped[0].name, "b");
	EXPECT_EQ(dropped[1].name, "move");
	// 在途那一条的应答照常认领。
	EXPECT_EQ(lane.InFlightSeq(), 1u);
	auto done = lane.Complete(1);
	ASSERT_TRUE(done.has_value());
	EXPECT_EQ(done->name, "a");
	EXPECT_TRUE(lane.Empty());
}

TEST(SceneLane, WaitExceededIsStrictlyAfterTheLimit)
{
	EXPECT_FALSE(WaitExceeded(At(0), At(2000), milliseconds(2000)));
	EXPECT_TRUE(WaitExceeded(At(0), At(2001), milliseconds(2000)));
}

} // namespace

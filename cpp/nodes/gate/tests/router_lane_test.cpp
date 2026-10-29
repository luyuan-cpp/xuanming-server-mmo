// 会话路由 lane 纯逻辑单测(docs/design/client-request-ordering.md §4)。
//
// 与 gate_security_test.cpp 同一做法:不进 gate.vcxproj 的 ClCompile 列表,不被编进 gate 可执行文件,独立跑。
// router_lane.h 只依赖 STL(InflightIndex 在 engine/core 的 utils/data_structures/inflight_index.h,同样只依赖 STL),
// 所以这份测试不需要 muduo / protobuf / 引擎库,只多一个 include 目录。
//
// 跑法(仓库根目录,带 g++ 的环境;gtest 用仓库里已经 vendored 的那份):
//
//   GT=third_party/googletest/googletest
//   g++ -std=c++23 -I cpp/libs/services/gate/session/comp -I cpp/libs/engine/core -I "$GT/include" -I "$GT"
//       cpp/nodes/gate/tests/router_lane_test.cpp
//       "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc"
//       -lpthread -o /tmp/router_lane_test
//   /tmp/router_lane_test
//
// (上面四行是同一条命令,换行只为可读;实际执行时接成一行。)

#include <gtest/gtest.h>

#include <chrono>
#include <string>

#include "router_lane.h"

namespace
{

using gate_lane::AdmitResult;
using gate_lane::CallSeq;
using gate_lane::InflightIndex;
using gate_lane::kNoCallSeq;
using gate_lane::QueueWaitExceeded;
using gate_lane::RouterLane;
using gate_lane::SteadyTime;
using std::chrono::milliseconds;

struct TestItem
{
	CallSeq seq{kNoCallSeq};
	std::string name;
};

struct TestCall
{
	SteadyTime expiresAt;
	std::string name;
};

constexpr std::size_t kDepth = 32;

SteadyTime At(int ms)
{
	return SteadyTime{} + milliseconds(ms);
}

// 取出队头并标记发出,返回取到的条目名;空闲但队列空时返回空串。
std::string SendNext(RouterLane<TestItem> &lane)
{
	auto item = lane.TakeNextIfIdle();
	if (!item)
	{
		return {};
	}
	EXPECT_TRUE(lane.MarkSent(item->seq));
	return item->name;
}

TEST(RouterLane, SendsInReceiptOrderOneAtATime)
{
	RouterLane<TestItem> lane;
	ASSERT_EQ(AdmitResult::kQueued, lane.Admit({1, "join"}, kDepth));
	ASSERT_EQ(AdmitResult::kQueued, lane.Admit({2, "cancel"}, kDepth));

	EXPECT_EQ("join", SendNext(lane));
	EXPECT_FALSE(lane.Idle());
	EXPECT_FALSE(lane.TakeNextIfIdle().has_value()) << "在途期间不得发出下一条(这正是要修的乱序)";

	EXPECT_TRUE(lane.Complete(1));
	EXPECT_EQ("cancel", SendNext(lane)) << "上一条完成之后才按收包顺序发下一条";
	EXPECT_TRUE(lane.Complete(2));
	EXPECT_TRUE(lane.Idle());
	EXPECT_EQ(0u, lane.Depth());
}

TEST(RouterLane, DepthLimitCountsInFlightAndQueued)
{
	RouterLane<TestItem> lane;
	ASSERT_EQ(AdmitResult::kQueued, lane.Admit({1, "a"}, 2));
	ASSERT_EQ("a", SendNext(lane));
	ASSERT_EQ(AdmitResult::kQueued, lane.Admit({2, "b"}, 2));
	EXPECT_EQ(2u, lane.Depth());
	EXPECT_EQ(AdmitResult::kRejectedFull, lane.Admit({3, "c"}, 2)) << "排队 + 在途达到上限就拒绝";
	EXPECT_EQ(1u, lane.QueuedCount()) << "被拒的那条不入队";
}

TEST(RouterLane, CompleteIgnoresLateDuplicateAndForeignSequences)
{
	RouterLane<TestItem> lane;
	lane.Admit({5, "a"}, kDepth);
	lane.Admit({6, "b"}, kDepth);
	ASSERT_EQ("a", SendNext(lane));

	EXPECT_FALSE(lane.Complete(kNoCallSeq)) << "0 号不是 lane 的调用";
	EXPECT_FALSE(lane.Complete(6)) << "还没发出的序号不能认领";
	EXPECT_FALSE(lane.Complete(99)) << "别的会话 / 复用 session_id 带来的序号不能认领";
	EXPECT_FALSE(lane.Idle());

	EXPECT_TRUE(lane.Complete(5));
	EXPECT_FALSE(lane.Complete(5)) << "同一序号只认领一次";
	EXPECT_TRUE(lane.Idle());
}

TEST(RouterLane, MarkSentRejectsWhenBusyOrZero)
{
	RouterLane<TestItem> lane;
	EXPECT_FALSE(lane.MarkSent(kNoCallSeq));
	EXPECT_TRUE(lane.MarkSent(1));
	EXPECT_FALSE(lane.MarkSent(2)) << "同一时刻至多一条在途";
	EXPECT_EQ(1u, lane.InFlightSeq());
}

TEST(RouterLane, NotSentItemLetsCallerContinueWithNext)
{
	// 队头因排队超时 / 无路由服而没发出时,调用方继续取下一条,lane 仍空闲。
	RouterLane<TestItem> lane;
	lane.Admit({1, "stale"}, kDepth);
	lane.Admit({2, "fresh"}, kDepth);
	auto first = lane.TakeNextIfIdle();
	ASSERT_TRUE(first.has_value());
	EXPECT_EQ("stale", first->name);
	EXPECT_TRUE(lane.Idle());
	EXPECT_EQ("fresh", SendNext(lane));
}

TEST(RouterLane, RedirectDropsQueuedRejectsNewButKeepsInFlight)
{
	RouterLane<TestItem> lane;
	lane.Admit({1, "a"}, kDepth);
	ASSERT_EQ("a", SendNext(lane));
	lane.Admit({2, "b"}, kDepth);
	lane.Admit({3, "c"}, kDepth);

	EXPECT_EQ(2u, lane.MarkRedirected());
	EXPECT_TRUE(lane.Redirected());
	EXPECT_EQ(AdmitResult::kRejectedRedirected, lane.Admit({4, "d"}, kDepth));
	EXPECT_EQ(1u, lane.InFlightSeq()) << "在途那一条照常认领";
	EXPECT_TRUE(lane.Complete(1));
	EXPECT_FALSE(lane.TakeNextIfIdle().has_value()) << "排队项已作废,不再发出";
}

TEST(RouterLane, DropQueuedOnDisconnectKeepsInFlightSequence)
{
	RouterLane<TestItem> lane;
	lane.Admit({1, "a"}, kDepth);
	ASSERT_EQ("a", SendNext(lane));
	lane.Admit({2, "b"}, kDepth);
	EXPECT_EQ(1u, lane.DropQueued());
	EXPECT_EQ(1u, lane.InFlightSeq()) << "断线时调用方要拿到在途序号去注销在途索引";
}

TEST(InflightIndex, InsertRejectsZeroAndDuplicate)
{
	InflightIndex<TestCall> index;
	EXPECT_FALSE(index.Insert(kNoCallSeq, {At(100)}));
	EXPECT_TRUE(index.Insert(7, {At(100)}));
	EXPECT_FALSE(index.Insert(7, {At(200)}));
	EXPECT_EQ(1u, index.Size());
}

TEST(InflightIndex, ExtractIsExactlyOnce)
{
	InflightIndex<TestCall> index;
	ASSERT_TRUE(index.Insert(7, {At(100), "a"}));
	auto first = index.Extract(7);
	ASSERT_TRUE(first.has_value());
	EXPECT_EQ("a", first->name);
	EXPECT_FALSE(index.Extract(7).has_value()) << "成功回包与本地截止只能有一个生效";
	EXPECT_FALSE(index.Erase(7));
}

TEST(InflightIndex, ExtractExpiredInDeadlineOrderAndSkipsCompleted)
{
	InflightIndex<TestCall> index;
	index.Insert(1, {At(300), "late"});
	index.Insert(2, {At(100), "early"});
	index.Insert(3, {At(200), "completed"});
	ASSERT_TRUE(index.Extract(3).has_value()) << "已认领的调用留在堆里,弹出时必须跳过";

	auto expired = index.ExtractExpired(At(250));
	ASSERT_EQ(1u, expired.size());
	EXPECT_EQ(2u, expired[0].first);
	EXPECT_EQ("early", expired[0].second.name);

	EXPECT_TRUE(index.ExtractExpired(At(299)).empty());
	auto rest = index.ExtractExpired(At(300));
	ASSERT_EQ(1u, rest.size()) << "截止时刻等于 now 也算到期";
	EXPECT_EQ(1u, rest[0].first);
	EXPECT_EQ(0u, index.Size());
	EXPECT_EQ(0u, index.ExpiryHeapSize());
}

TEST(QueueWait, ExceededOnlyStrictlyAfterLimit)
{
	EXPECT_FALSE(QueueWaitExceeded(At(0), At(5000), milliseconds(5000))) << "等号边界不算超时";
	EXPECT_TRUE(QueueWaitExceeded(At(0), At(5001), milliseconds(5000)));
}

} // namespace

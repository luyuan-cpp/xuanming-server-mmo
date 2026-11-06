#include <gtest/gtest.h>

#include "core/async/coroutine_callback.h"
#include "core/async/loop_task_scope.h"
#include "../../nodes/scene/id_segment_request_table.h"

#include <boost/cobalt/task.hpp>

namespace
{
using Table = id_segment_detail::RequestTable;

struct FetchProbe
{
	GuidSegmentClient client;
	Table table;
	std::function<void(uint64_t)> pending;
	int submissions = 0;
};

boost::cobalt::task<void> AwaitFakeFetch(EventLoop& loop, FetchProbe& probe, uint64_t sequence)
{
	auto lo = co_await loop_async::AwaitCallback<uint64_t>(loop, [&probe](auto done) {
		probe.pending = std::move(done);
		return true;
	}, 0);
	if (lo && probe.table.Take(sequence)) probe.client.OnResponse(0, *lo, *lo + 1);
}

void EnableProbe(FetchProbe& probe, EventLoop& loop, LoopTaskScope& scope)
{
	GuidSegmentClient::Options options;
	options.kindName = "item";
	options.initialStep = 1;
	options.mixIds = false;
	ASSERT_TRUE(probe.client.Enable(options, [&](const std::string&, uint32_t) {
		if (!scope.IsAccepting()) return false;
		++probe.submissions;
		return scope.Start(AwaitFakeFetch(loop, probe, probe.table.Reserve(GuidKind::kItem, 0.0)), "test guid fetch");
	}, [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; }));
}
}

TEST(GuidSegmentTransportContractTest, TimeoutKeepsOriginalKindDuringLateWindow)
{
	Table table;
	const auto seq = table.Reserve(GuidKind::kItem, 5.0);
	EXPECT_EQ(table.BeforeSend(7.9, 3.0).admission, Table::Admission::kBusy);
	EXPECT_EQ(table.BeforeSend(8.0, 3.0).admission, Table::Admission::kOpenedLateWindow);
	EXPECT_EQ(table.BeforeSend(17.9, 3.0).admission, Table::Admission::kBusy);
	const auto late = table.Take(seq);
	ASSERT_TRUE(late);
	EXPECT_TRUE(late->late);
	EXPECT_EQ(late->kind, GuidKind::kItem);
	EXPECT_FALSE(table.Take(seq));
	EXPECT_EQ(table.BeforeSend(18.0, 3.0).admission, Table::Admission::kReady);
}

TEST(GuidSegmentTransportContractTest, ExpiredReplyCannotBeAppliedToTheNextKind)
{
	Table table;
	const auto old = table.Reserve(GuidKind::kItem, 5.0);
	EXPECT_EQ(table.BeforeSend(8.0, 3.0).admission, Table::Admission::kOpenedLateWindow);
	EXPECT_EQ(table.BeforeSend(18.0, 3.0).admission, Table::Admission::kExpiredLateWindow);
	const auto current = table.Reserve(GuidKind::kTxLog, 18.0);
	EXPECT_FALSE(table.Take(old));
	const auto completion = table.Take(current);
	ASSERT_TRUE(completion);
	EXPECT_FALSE(completion->late);
	EXPECT_EQ(completion->kind, GuidKind::kTxLog);
}

TEST(GuidSegmentTransportContractTest, ClosedScopeDeliversAlreadyIssuedRangeAfterClientShutdown)
{
	EventLoop loop;
	LoopTaskScope scope(loop);
	FetchProbe probe;
	EnableProbe(probe, loop, scope);
	probe.client.Warm();
	ASSERT_TRUE(probe.pending);
	scope.Close();
	probe.client.Shutdown();
	EXPECT_FALSE(scope.IsAccepting());
	probe.pending(77);
	Guid minted{};
	EXPECT_TRUE(probe.client.TryNext(minted));
	EXPECT_EQ(minted, 77u);
	EXPECT_FALSE(probe.client.TryNext(minted));
	EXPECT_EQ(probe.submissions, 1);
}

TEST(GuidSegmentTransportContractTest, StoppedScopeDropsLateCompletionAfterOwnerDestruction)
{
	EventLoop loop;
	LoopTaskScope scope(loop);
	auto probe = std::make_unique<FetchProbe>();
	EnableProbe(*probe, loop, scope);
	probe->client.Warm();
	ASSERT_TRUE(probe->pending);
	auto late = probe->pending;
	scope.Stop();
	probe.reset();
	late(77);
	EXPECT_FALSE(scope.IsAccepting());
}

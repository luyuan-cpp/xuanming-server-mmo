#include <gtest/gtest.h>

#include <functional>
#include <string>
#include <utility>
#include <vector>

#include "core/async/coroutine_callback.h"
#include "core/async/loop_task_scope.h"
#include "../../nodes/scene/id_segment_request_table.h"
#include "../../nodes/scene/id_segment_transport.h"

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

// ── 生产传输(nodes/scene/id_segment_transport.h)────────────────────────────────────────────
// 只把选路 / 发送和单调时钟换成假的;单飞、迟到窗口、按请求编号认领、Close / Stop 都走 Scene 节点用的同一份代码,
// 号段客户端也是注册表里的真实实例。2026-11-11 之前这些只在上面的复刻实现里测过,生产传输本身没有任何用例。
namespace
{
using ProductionTransport = id_segment_detail::Transport;

class IdSegmentProductionTransportTest : public testing::Test
{
protected:
	struct Sent
	{
		ProductionTransport::Request request;
		std::function<void(ProductionTransport::FetchResult)> done;
	};

	void SetUp() override
	{
		transport.Init(loop,
			[this]() -> ProductionTransport::Fetcher {
				if (!nodeReady) return {};
				return [this](const ProductionTransport::Request& request,
					std::function<void(ProductionTransport::FetchResult)> done) {
					sent.push_back({request, std::move(done)});
				};
			},
			[this] { return now; });
		ASSERT_NO_FATAL_FAILURE(Enable(GuidKind::kItem, "item"));
		ASSERT_NO_FATAL_FAILURE(Enable(GuidKind::kTxLog, "txlog"));
	}

	void TearDown() override
	{
		transport.Stop();
		tlsGuidSegmentRegistry.Get(GuidKind::kItem).Reset();
		tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Reset();
		retries.clear();
		sent.clear();
		loop.queueInLoop([this] { loop.quit(); });
		loop.loop();
	}

	void Enable(GuidKind kind, const char* name)
	{
		auto& client = tlsGuidSegmentRegistry.Get(kind);
		client.Reset();
		GuidSegmentClient::Options options;
		options.kindName = name;
		options.bizTag = name;
		options.initialStep = 10;
		options.fetchTimeoutSec = 3.0;
		options.mixIds = false;
		ASSERT_TRUE(client.Enable(options,
			[this, kind](const std::string&, uint32_t step) { return transport.Send(kind, step); },
			// 领段超时不排:用例自己拨时钟驱动迟到窗口。退避重试收下来,由用例手动触发。
			[this, kind](GuidSegmentClient::TimerKind timer, double, std::function<void()> fn) {
				if (timer == GuidSegmentClient::TimerKind::kRetry) retries.push_back({kind, std::move(fn)});
			},
			[this] { return now; }));
	}

	// 触发这一种最早排上的退避重试。没有可触发的返回 false。
	bool FireRetry(GuidKind kind)
	{
		for (auto it = retries.begin(); it != retries.end(); ++it)
		{
			if (it->first != kind) continue;
			auto fn = std::move(it->second);
			retries.erase(it);
			fn();
			return true;
		}
		return false;
	}

	static ProductionTransport::FetchResult Granted(uint64_t lo, uint64_t hi)
	{
		::data_service::AllocateIdSegmentResponse reply;
		reply.set_lo(lo);
		reply.set_hi(hi);
		return {GrpcCallOutcome{}, reply};
	}

	static GuidSegmentClient& Client(GuidKind kind) { return tlsGuidSegmentRegistry.Get(kind); }

	muduo::net::EventLoop loop;
	ProductionTransport transport;
	std::vector<Sent> sent;
	std::vector<std::pair<GuidKind, std::function<void()>>> retries;
	bool nodeReady = true;
	double now = 100.0;
};
} // namespace

// 单飞:在途时另一种发不出去、按退避等;结果按请求编号交给发出它的那一种,路径空出后另一种照常发出。
TEST_F(IdSegmentProductionTransportTest, GrantedRangeGoesToTheKindThatAskedWhileOtherKindsWait)
{
	Client(GuidKind::kItem).Warm();
	ASSERT_EQ(sent.size(), 1u);
	EXPECT_EQ(sent[0].request.biz_tag(), "item");
	EXPECT_EQ(sent[0].request.step(), 10u);
	Client(GuidKind::kTxLog).Warm();
	EXPECT_EQ(sent.size(), 1u);

	sent[0].done(Granted(500, 510));
	Guid minted{};
	ASSERT_TRUE(Client(GuidKind::kItem).TryNext(minted));
	EXPECT_EQ(minted, 500u);
	EXPECT_FALSE(Client(GuidKind::kTxLog).IsReady());

	ASSERT_TRUE(FireRetry(GuidKind::kTxLog));
	ASSERT_EQ(sent.size(), 2u);
	EXPECT_EQ(sent[1].request.biz_tag(), "txlog");
}

// fetchTimeout 到期后共享路径守 10 秒迟到窗口:窗口内谁都不发,迟到的结果仍交给原来那一种,认领之后路径空出。
TEST_F(IdSegmentProductionTransportTest, LateWindowHoldsThePathAndTheLateReplyStillReachesItsKind)
{
	Client(GuidKind::kItem).Warm();
	ASSERT_EQ(sent.size(), 1u);
	now += 3.0;
	Client(GuidKind::kTxLog).Warm();
	now += 9.9;
	ASSERT_TRUE(FireRetry(GuidKind::kTxLog));
	EXPECT_EQ(sent.size(), 1u);

	sent[0].done(Granted(700, 710));
	Guid minted{};
	ASSERT_TRUE(Client(GuidKind::kItem).TryNext(minted));
	EXPECT_EQ(minted, 700u);
	EXPECT_EQ(transport.OrphanResponses(), 0u);

	ASSERT_TRUE(FireRetry(GuidKind::kTxLog));
	ASSERT_EQ(sent.size(), 2u);
	EXPECT_EQ(sent[1].request.biz_tag(), "txlog");
}

// 窗口过期之后才到的结果是孤儿:丢弃并计数,不会落到接着发出请求的另一种身上。
TEST_F(IdSegmentProductionTransportTest, ReplyAfterTheLateWindowIsAnOrphanAndCannotReachTheNextKind)
{
	Client(GuidKind::kItem).Warm();
	ASSERT_EQ(sent.size(), 1u);
	now += 3.0;
	Client(GuidKind::kTxLog).Warm();
	now += 10.0;
	ASSERT_TRUE(FireRetry(GuidKind::kTxLog));
	ASSERT_EQ(sent.size(), 2u);
	EXPECT_EQ(sent[1].request.biz_tag(), "txlog");

	sent[0].done(Granted(700, 710));
	EXPECT_EQ(transport.OrphanResponses(), 1u);
	EXPECT_FALSE(Client(GuidKind::kTxLog).IsReady());

	sent[1].done(Granted(900, 910));
	Guid minted{};
	ASSERT_TRUE(Client(GuidKind::kTxLog).TryNext(minted));
	EXPECT_EQ(minted, 900u);
}

// 传输失败只报给发出请求的那一种:它按退避排重试,别的种类不受牵连。
TEST_F(IdSegmentProductionTransportTest, TransportFailureIsReportedOnlyToTheKindThatAsked)
{
	Client(GuidKind::kItem).Warm();
	ASSERT_EQ(sent.size(), 1u);
	ASSERT_TRUE(retries.empty());
	sent[0].done({GrpcCallOutcome{kGrpcStatusUnavailable, "connection reset"}, std::nullopt});
	ASSERT_EQ(retries.size(), 1u);
	EXPECT_EQ(retries.front().first, GuidKind::kItem);
	EXPECT_FALSE(Client(GuidKind::kItem).IsReady());
}

// 挑不到 READY 节点时当场回 false,不占请求编号:节点一就绪,任何一种都能立即发出。
TEST_F(IdSegmentProductionTransportTest, NoReadyNodeDoesNotReserveTheSharedPath)
{
	nodeReady = false;
	EXPECT_FALSE(transport.Send(GuidKind::kItem, 10));
	EXPECT_TRUE(sent.empty());
	nodeReady = true;
	EXPECT_TRUE(transport.Send(GuidKind::kTxLog, 10));
	ASSERT_EQ(sent.size(), 1u);
	EXPECT_EQ(sent[0].request.biz_tag(), "txlog");
}

// Close 只封新请求:已发出的结果照常交付,停机退出存盘仍能用到这一段;Close 之后谁都发不出新请求。
TEST_F(IdSegmentProductionTransportTest, CloseStillDeliversTheResultAlreadyInFlight)
{
	Client(GuidKind::kItem).Warm();
	ASSERT_EQ(sent.size(), 1u);
	transport.Close();
	Client(GuidKind::kItem).Shutdown();
	Client(GuidKind::kTxLog).Warm();
	EXPECT_EQ(sent.size(), 1u);

	sent[0].done(Granted(300, 301));
	Guid minted{};
	ASSERT_TRUE(Client(GuidKind::kItem).TryNext(minted));
	EXPECT_EQ(minted, 300u);
}

// Stop 结束本地等待并清表:晚到的结果碰不到任何状态,也不计孤儿;之后不再发送。
TEST_F(IdSegmentProductionTransportTest, StopDropsTheLateResultAndRefusesNewSends)
{
	Client(GuidKind::kItem).Warm();
	ASSERT_EQ(sent.size(), 1u);
	auto late = std::move(sent[0].done);
	transport.Stop();
	late(Granted(300, 301));
	EXPECT_FALSE(Client(GuidKind::kItem).IsReady());
	EXPECT_EQ(transport.OrphanResponses(), 0u);
	EXPECT_FALSE(transport.Send(GuidKind::kTxLog, 10));
}

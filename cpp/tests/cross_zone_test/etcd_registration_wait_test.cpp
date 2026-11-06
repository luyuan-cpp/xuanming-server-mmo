#include <gtest/gtest.h>

#include "core/async/loop_task_scope.h"
#include "core/node/system/etcd/etcd_registration_wait.h"
#include "core/node/system/etcd/etcd_service.h"

#include <boost/cobalt/task.hpp>
#include <string>
#include <vector>

// 注册任务测试用可控节点接缝。链接完整 core 时 Node 引用两处节点可执行文件入口，但本测试不启动 Node。
void InitPlayerService() {}
void InitServiceHandler() {}

struct EtcdRegistrationTestAccess
{
	struct Probe
	{
		EtcdRegistrationCompletion<etcdserverpb::LeaseGrantResponse> lease;
		EtcdRegistrationCompletion<etcdserverpb::TxnResponse> txn;
		std::vector<std::string> calls;
		std::function<void()> duringLeaseSubmit;

		void Grant(int64_t id)
		{
			ASSERT_TRUE(lease);
			etcdserverpb::LeaseGrantResponse response;
			response.set_id(id);
			response.set_ttl(30);
			std::exchange(lease, {})(std::move(response));
		}

		void CompleteClaim(bool won)
		{
			ASSERT_TRUE(txn);
			etcdserverpb::TxnResponse response;
			response.set_succeeded(won);
			std::exchange(txn, {})(std::move(response));
		}
	};

	static void Configure(EtcdService& service, EventLoop& loop, Probe& probe)
	{
		using Stage = EtcdService::RegistrationStage;
		const auto keyOf = [](Stage stage) {
			return std::string(stage == Stage::kPort ? "port" : (stage == Stage::kAllocation ? "node" : "publish"));
		};
		service.ConfigureRegistration(loop, {
			[&probe](auto done) {
				probe.calls.push_back("lease");
				probe.lease = std::move(done);
				if (probe.duringLeaseSubmit) probe.duringLeaseSubmit();
			},
			[&probe] { probe.calls.push_back("keepalive"); },
			[&probe] { probe.calls.push_back("select-port"); return true; },
			[&probe] { probe.calls.push_back("select-node"); },
			[&probe] { probe.calls.push_back("lost-node"); },
			keyOf,
			[&probe, keyOf](Stage stage, bool recovering, bool unknown, auto done) {
				probe.calls.push_back(keyOf(stage) + (recovering ? "-recover" : "") + (unknown ? "-unknown" : ""));
				probe.txn = std::move(done);
			},
			[&probe] { probe.calls.push_back("prepare-rpc"); },
			[&probe] { probe.calls.push_back("fence"); },
		});
	}

	static void Start(EtcdService& service) { service.RequestNodeLease(); }
};

namespace
{
using Reply = std::optional<int>;
using Completion = std::function<void(Reply)>;

boost::cobalt::task<void> AwaitEtcdRetry(EventLoop& loop, int& committed)
{
	TimerTaskComp timer;
	Completion old;
	auto first = co_await etcd_registration::AwaitReply<int>(loop, timer, 0.0, [&](Completion done) {
		old = std::move(done);
		old(std::nullopt); // 明确传输失败不能当成 CAS 已拒绝。
	});
	timer.Cancel();
	EXPECT_TRUE(first && !*first);

	auto second = co_await etcd_registration::AwaitReply<int>(loop, timer, 1.0, [&](Completion done) {
		loop.queueInLoop([&committed, old, done = std::move(done)] {
			old(11); // 此时下一次请求已在途。不能把 11 当成它的回包。
			EXPECT_EQ(committed, 0);
			done(22);
		});
	});
	timer.Cancel();
	if (second && *second) committed = **second;
	loop.quit();
}

boost::cobalt::task<void> AwaitEtcdStop(EventLoop& loop, Completion& saved, int& delivered)
{
	TimerTaskComp timer;
	auto result = co_await etcd_registration::AwaitReply<int>(loop, timer, 1.0, [&](Completion done) {
		saved = std::move(done);
	});
	EXPECT_FALSE(result);
	if (result && *result) ++delivered;
}
}

TEST(EtcdRegistrationWaitTest, LateReplyCannotCommitTheNextAttempt)
{
	EventLoop loop;
	LoopTaskScope scope(loop);
	int committed = 0;
	ASSERT_TRUE(scope.Start(AwaitEtcdRetry(loop, committed), "etcd registration test"));
	loop.runAfter(3.0, [&] { ADD_FAILURE() << "未收到第二次 CAS 完成"; loop.quit(); });
	loop.loop();
	EXPECT_EQ(committed, 22);
}

TEST(EtcdRegistrationWaitTest, ShutdownDropsLateReply)
{
	EventLoop loop;
	LoopTaskScope scope(loop);
	Completion saved;
	int delivered = 0;
	ASSERT_TRUE(scope.Start(AwaitEtcdStop(loop, saved, delivered), "etcd registration test"));
	loop.runAfter(0.0, [&] {
		scope.Stop();
		EXPECT_TRUE(saved);
		if (saved) saved(33);
		loop.quit();
	});
	loop.runAfter(3.0, [&] { ADD_FAILURE() << "本地停止没有完成"; loop.quit(); });
	loop.loop();
	EXPECT_EQ(delivered, 0);
}

TEST(EtcdRegistrationWaitTest, ActualRegistrationPublishesOnlyAfterTheListenerIsReady)
{
	EventLoop loop;
	EtcdRegistrationTestAccess::Probe probe;
	EtcdService service;
	EtcdRegistrationTestAccess::Configure(service, loop, probe);
	EtcdRegistrationTestAccess::Start(service);
	ASSERT_EQ(probe.calls, (std::vector<std::string>{"lease"}));
	probe.Grant(41);
	ASSERT_EQ(probe.calls, (std::vector<std::string>{"lease", "keepalive", "select-port", "port"}));
	probe.CompleteClaim(true);
	EXPECT_EQ(probe.calls.back(), "node");
	probe.CompleteClaim(true);
	EXPECT_EQ(probe.calls.back(), "prepare-rpc");
	EXPECT_FALSE(probe.txn);
	EXPECT_TRUE(service.RegistrationInFlight());
	service.PublishDiscoveryAfterGrpcReady();
	EXPECT_EQ(probe.calls.back(), "publish");
	probe.CompleteClaim(true);
	EXPECT_FALSE(service.RegistrationInFlight());
}

TEST(EtcdRegistrationWaitTest, LostLeaseDuringPreparedStartupKeepsIdentityAndWaitsForListener)
{
	EventLoop loop;
	EtcdRegistrationTestAccess::Probe probe;
	EtcdService service;
	EtcdRegistrationTestAccess::Configure(service, loop, probe);
	EtcdRegistrationTestAccess::Start(service);
	probe.Grant(41);
	probe.CompleteClaim(true);
	probe.CompleteClaim(true);
	ASSERT_EQ(probe.calls.back(), "prepare-rpc");
	service.RequestReRegistration("test lease lost");
	ASSERT_EQ(probe.calls.back(), "lease");
	probe.Grant(42);
	EXPECT_EQ(probe.calls.back(), "port-recover");
	probe.CompleteClaim(true);
	EXPECT_EQ(probe.calls.back(), "node-recover");
	probe.CompleteClaim(true);
	EXPECT_FALSE(probe.txn);
	service.PublishDiscoveryAfterGrpcReady();
	EXPECT_EQ(probe.calls.back(), "publish-recover");
	probe.CompleteClaim(true);
	EXPECT_FALSE(service.RegistrationInFlight());
}

TEST(EtcdRegistrationWaitTest, ReRegistrationConflictFencesInsteadOfPublishing)
{
	EventLoop loop;
	EtcdRegistrationTestAccess::Probe probe;
	EtcdService service;
	EtcdRegistrationTestAccess::Configure(service, loop, probe);
	service.RequestReRegistration("test already running");
	probe.Grant(41);
	probe.CompleteClaim(true);
	EXPECT_EQ(probe.calls.back(), "node-recover");
	probe.CompleteClaim(false);
	EXPECT_EQ(probe.calls.back(), "fence");
	EXPECT_FALSE(probe.txn);
}

TEST(EtcdRegistrationWaitTest, SuccessfulLeaseThenStopDuringSubmissionDoesNotStartKeepAlive)
{
	EventLoop loop;
	EtcdRegistrationTestAccess::Probe probe;
	EtcdService service;
	EtcdRegistrationTestAccess::Configure(service, loop, probe);
	probe.duringLeaseSubmit = [&] {
		probe.Grant(41);
		service.StopRegistrationRetries();
	};
	EtcdRegistrationTestAccess::Start(service);
	EXPECT_EQ(probe.calls, (std::vector<std::string>{"lease"}));
	EXPECT_EQ(service.GetLeaseId(), 0);
	EXPECT_FALSE(service.RegistrationInFlight());
	EXPECT_FALSE(probe.txn);
}

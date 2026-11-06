#include <gtest/gtest.h>

#include "core/async/loop_task_scope.h"
#include "core/node/system/etcd/etcd_registration_wait.h"

#include <boost/cobalt/task.hpp>

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
	loop.queueInLoop([&] {
		scope.Stop();
		EXPECT_TRUE(saved);
		if (saved) saved(33);
		loop.quit();
	});
	loop.loop();
	EXPECT_EQ(delivered, 0);
}

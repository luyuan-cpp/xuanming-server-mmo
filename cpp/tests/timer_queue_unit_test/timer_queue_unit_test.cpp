#include <gtest/gtest.h>

#include "entt/src/entt/entity/registry.hpp"
#include "muduo/base/Timestamp.h"
#include "muduo/net/EventLoopThread.h"
// 内部头,只为直接测时钟换算与 Windows 的 nextTimeoutMs。
#include "muduo/net/TimerQueue.h"

#include <atomic>
#include <cassert>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <functional>
#include <limits>
#include <memory>
#include <utility>
#include <vector>

#include "combat/skill/comp/skill_comp.h"
#include "time/comp/timer_task_comp.h"

using namespace muduo;
using namespace muduo::net;

namespace {
// 正间隔小于 1us,首次到期的微秒增量为 0,保证与过去时刻的一次性定时器同批到期。
// 保留重复属性而不依赖 sleep 或机器调度速度制造测试窗口。
constexpr double kImmediateRepeatIntervalSeconds = 0.0000001;
} // namespace

// 保留现有直接组件及技能包装的地址稳定性;下方另测无此标记的包装也能安全搬移。
static_assert(entt::component_traits<TimerTaskComp>::in_place_delete);
static_assert(entt::component_traits<CastingTimerComp>::in_place_delete);
static_assert(entt::component_traits<RecoveryTimerComp>::in_place_delete);
static_assert(entt::component_traits<ChannelFinishTimerComp>::in_place_delete);
static_assert(entt::component_traits<ChannelIntervalTimerComp>::in_place_delete);

class GameTimerTest
{
public:
	GameTimerTest(std::atomic<int>& afterCount, std::atomic<int>& everyCount)
		: afterCount_(afterCount), everyCount_(everyCount) {}
	// 本夹具的回调绑定 this,因此宿主地址固定。
	GameTimerTest(const GameTimerTest&) = delete;
	GameTimerTest& operator=(const GameTimerTest&) = delete;
	GameTimerTest(GameTimerTest&&) = delete;
	GameTimerTest& operator=(GameTimerTest&&) = delete;

	void RunAfter()
	{
		m_Timer.RunAfter(0.01, std::bind(&GameTimerTest::AfterCallBack, this));
		m_TimerActiveTest.RunAfter(0.05, std::bind(&GameTimerTest::AfterCallBackActiveTest, this));
	}


	void RunEvery()
	{
		m_Timer.RunEvery(0.01, std::bind(&GameTimerTest::RunEveryCallBack, this));
	}

	void Cancel()
	{
		m_Timer.Cancel();
		assert(!m_Timer.IsActive());
	}

private:

	void AfterCallBack()
	{
		++afterCount_;
	}

	void AfterCallBackActiveTest()
	{
		assert(!m_Timer.IsActive());
	}


	void RunEveryCallBack()
	{
		++everyCount_;
    }
private:
	std::atomic<int>& afterCount_;
	std::atomic<int>& everyCount_;
	TimerTaskComp m_Timer;
	TimerTaskComp m_TimerActiveTest;

};

TEST(TimerQueueTest, ComponentStaysSmall)
{
	std::printf("[  SIZEOF  ] TimerTaskComp          = %zu bytes\n", sizeof(TimerTaskComp));
	std::printf("[  SIZEOF  ]   muduo::net::TimerId  = %zu bytes\n", sizeof(TimerId));
	std::printf("[  SIZEOF  ]   TimerCallback        = %zu bytes (not stored in the component)\n",
		sizeof(TimerCallback));
	std::printf("[  SIZEOF  ]   alignof              = %zu\n", alignof(TimerTaskComp));

	// 组件只持有一个 shared_ptr<State>,回调由 muduo 持有;保留原体积上界防止膨胀。
	EXPECT_LE(sizeof(TimerTaskComp),
	          sizeof(TimerId) + sizeof(std::uint64_t) + sizeof(std::shared_ptr<void>));
}

TEST(TimerQueueTest, BasicTimerOperations)
{
	std::atomic<int> afterCount{0};
	std::atomic<int> everyCount{0};

	EventLoop loop;
	GameTimerTest oneShot(afterCount, everyCount);
	oneShot.RunAfter();

	GameTimerTest periodic(afterCount, everyCount);
	periodic.RunEvery();

	loop.runAfter(0.08, [&periodic]() {
		periodic.Cancel();
	});

	loop.runAfter(0.12, [&loop]() {
		loop.quit();
	});

	loop.loop();

	EXPECT_GE(afterCount.load(), 1);
	EXPECT_GE(everyCount.load(), 1);
}

TEST(TimerQueueTest, SafeWithoutEventLoop)
{
	TimerTaskComp timer;

	timer.RunAfter(0.01, []() {});
	timer.RunEvery(0.01, []() {});
	timer.RunAt(Timestamp::now(), []() {});
	timer.Cancel();

	EXPECT_FALSE(timer.IsActive());
}

TEST(TimerTaskRegressionTest, InvalidRepeatingIntervalDoesNotArmOrReplace)
{
	EventLoop loop;
	TimerTaskComp timer;
	for (const double interval : {0.0, -1.0}) {
		timer.RunEvery(interval, [] {});
		EXPECT_FALSE(timer.IsActive());
		timer.Cancel();
	}
	int fires = 0;
	timer.RunAfter(0.0, [&] { ++fires; loop.quit(); });
	timer.RunEvery(0.0, [] {});
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
}

TEST(TimerTaskRegressionTest, CancelCleanupSeesInactiveAndCanCancelAgain)
{
	EventLoop loop;
	TimerTaskComp timer;
	bool activeInCleanup = true;
	int cleanups = 0;
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) {
		++cleanups;
		activeInCleanup = timer.IsActive();
		timer.Cancel();
	});
	timer.RunAfter(60.0, [cleanup = std::move(cleanup)] {});
	timer.Cancel();
	EXPECT_FALSE(activeInCleanup);
	EXPECT_FALSE(timer.IsActive());
	EXPECT_EQ(1, cleanups);
}

TEST(TimerTaskRegressionTest, CancelCleanupCanRearmSameComponent)
{
	EventLoop loop;
	TimerTaskComp timer;
	int fires = 0;
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) {
		timer.RunAfter(0.0, [&] { ++fires; loop.quit(); });
	});
	timer.RunAfter(60.0, [cleanup = std::move(cleanup)] {});
	timer.Cancel();
	EXPECT_TRUE(timer.IsActive());
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
	EXPECT_FALSE(timer.IsActive());
}

TEST(TimerTaskRegressionTest, ReplacementCleanupRearmWins)
{
	EventLoop loop;
	TimerTaskComp timer;
	int nestedFires = 0;
	int replacedFires = 0;
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) {
		timer.RunAfter(0.0, [&] { ++nestedFires; loop.quit(); });
	});
	timer.RunAfter(60.0, [cleanup = std::move(cleanup)] {});
	timer.RunAfter(0.0, [&] { ++replacedFires; loop.quit(); });
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, nestedFires);
	EXPECT_EQ(0, replacedFires);
}

TEST(TimerTaskRegressionTest, ReplacementCleanupCanCancelNewTimer)
{
	EventLoop loop;
	TimerTaskComp timer;
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) { timer.Cancel(); });
	timer.RunAfter(60.0, [cleanup = std::move(cleanup)] {});
	timer.RunAfter(60.0, [] {});
	EXPECT_FALSE(timer.IsActive());
}

TEST(TimerTaskRegressionTest, ReplacementCleanupCanDestroyOwner)
{
	EventLoop loop;
	auto owner = std::make_unique<TimerTaskComp>();
	int fires = 0;
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) { owner.reset(); });
	owner->RunAfter(60.0, [cleanup = std::move(cleanup)] {});
	owner->RunAt(addTime(Timestamp::now(), -2.0), [&] { ++fires; });
	loop.runAt(addTime(Timestamp::now(), -1.0), [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(nullptr, owner);
	EXPECT_EQ(0, fires);
}

TEST(TimerTaskRegressionTest, ReplacementCleanupCanMoveOwner)
{
	EventLoop loop;
	TimerTaskComp source;
	TimerTaskComp destination;
	int fires = 0;
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) { destination = std::move(source); });
	source.RunAfter(60.0, [cleanup = std::move(cleanup)] {});
	source.RunAfter(0.0, [&] { ++fires; loop.quit(); });
	EXPECT_FALSE(source.IsActive());
	EXPECT_TRUE(destination.IsActive());
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
}

TEST(TimerTaskRegressionTest, MoveAssignmentCleanupRearmWins)
{
	EventLoop loop;
	TimerTaskComp source;
	TimerTaskComp destination;
	int sourceFires = 0;
	int nestedFires = 0;
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) {
		destination.RunAfter(0.0, [&] { ++nestedFires; loop.quit(); });
	});
	destination.RunAfter(60.0, [cleanup = std::move(cleanup)] {});
	source.RunAfter(0.0, [&] { ++sourceFires; loop.quit(); });
	destination = std::move(source);
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_FALSE(source.IsActive());
	EXPECT_EQ(1, nestedFires);
	EXPECT_EQ(0, sourceFires);
}

TEST(TimerTaskRegressionTest, MoveAssignmentCleanupCanDestroySource)
{
	EventLoop loop;
	auto source = std::make_unique<TimerTaskComp>();
	TimerTaskComp destination;
	int fires = 0;
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) { source.reset(); });
	destination.RunAfter(60.0, [cleanup = std::move(cleanup)] {});
	source->RunAfter(0.0, [&] { ++fires; loop.quit(); });
	destination = std::move(*source);
	EXPECT_EQ(nullptr, source);
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
}

TEST(TimerTaskRegressionTest, InvalidDelayAndEmptyCallbackPreserveCurrentTimer)
{
	EventLoop loop;
	TimerTaskComp timer;
	int fires = 0;
	ASSERT_TRUE(timer.RunAfter(0.0, [&] { ++fires; loop.quit(); }));
	for (const double delay : {std::numeric_limits<double>::quiet_NaN(),
		std::numeric_limits<double>::infinity(), -std::numeric_limits<double>::infinity(),
		std::numeric_limits<double>::max(), -std::numeric_limits<double>::max()}) {
		EXPECT_FALSE(timer.RunAfter(delay, [] {}));
		EXPECT_FALSE(timer.RunEvery(delay, [] {}));
		EXPECT_TRUE(timer.IsActive());
	}
	EXPECT_FALSE(timer.RunEvery(0.0, [] {}));
	EXPECT_FALSE(timer.RunEvery(-1.0, [] {}));
	EXPECT_FALSE(timer.RunAfter(0.0, {}));
	EXPECT_FALSE(timer.RunEvery(1.0, {}));
	EXPECT_FALSE(timer.RunAt(Timestamp::now(), {}));
	EXPECT_TRUE(timer.IsActive());
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
}

TEST(TimerTaskRegressionTest, SchedulingWithoutLoopReportsFailure)
{
	TimerTaskComp timer;
	EXPECT_FALSE(timer.RunAfter(0.0, [] {}));
	EXPECT_FALSE(timer.RunEvery(1.0, [] {}));
	EXPECT_FALSE(timer.RunAt(Timestamp::now(), [] {}));
	EXPECT_FALSE(timer.IsActive());
}

TEST(TimerTaskRegressionTest, CancelCleanupCanDestroyOwner)
{
	EventLoop loop;
	auto owner = std::make_unique<TimerTaskComp>();
	int cleanups = 0;
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) { ++cleanups; owner.reset(); });
	owner->RunAfter(60.0, [cleanup = std::move(cleanup)] {});
	owner->Cancel();
	EXPECT_EQ(nullptr, owner);
	EXPECT_EQ(1, cleanups);
}

TEST(TimerTaskRegressionTest, RepeatingCallbackCanCancelItself)
{
	EventLoop loop;
	TimerTaskComp timer;
	int fires = 0;
	timer.RunEvery(kImmediateRepeatIntervalSeconds, [&] {
		++fires;
		timer.Cancel();
		timer.Cancel();
		loop.queueInLoop([&loop] { loop.runAfter(0.0, [&loop] { loop.quit(); }); });
	});
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
	EXPECT_FALSE(timer.IsActive());
}

TEST(TimerTaskRegressionTest, RepeatingCallbackCanDestroyOwner)
{
	EventLoop loop;
	auto owner = std::make_unique<TimerTaskComp>();
	int fires = 0;
	owner->RunEvery(kImmediateRepeatIntervalSeconds, [&] {
		++fires;
		owner.reset();
		loop.queueInLoop([&loop] { loop.runAfter(0.0, [&loop] { loop.quit(); }); });
	});
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
	EXPECT_EQ(nullptr, owner);
}

TEST(TimerTaskRegressionTest, RepeatingCallbackCanReplaceItselfWithOneShot)
{
	EventLoop loop;
	TimerTaskComp timer;
	int repeats = 0;
	int oneShots = 0;
	timer.RunEvery(kImmediateRepeatIntervalSeconds, [&] {
		++repeats;
		timer.RunAfter(0.0, [&] { ++oneShots; loop.quit(); });
	});
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, repeats);
	EXPECT_EQ(1, oneShots);
	EXPECT_FALSE(timer.IsActive());
}

TEST(TimerTaskRegressionTest, NonPositiveOneShotDelayStillFiresOnce)
{
	EventLoop loop;
	TimerTaskComp zero;
	TimerTaskComp negative;
	int fires = 0;
	auto tick = [&] { if (++fires == 2) loop.quit(); };
	EXPECT_TRUE(zero.RunAfter(0.0, tick));
	EXPECT_TRUE(negative.RunAfter(-1.0, tick));
	loop.runAfter(1.0, [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(2, fires);
	EXPECT_FALSE(zero.IsActive());
	EXPECT_FALSE(negative.IsActive());
}

TEST(TimerQueueTest, MoveAndCopyAreSafe)
{
	EventLoop loop;

	TimerTaskComp source;
	source.RunAfter(60.0, []() {});   // far enough out that it never fires here
	EXPECT_TRUE(source.IsActive());

	// 复制不共享源对象的计时所有权。
	TimerTaskComp copied(source);
	EXPECT_FALSE(copied.IsActive());

	// 移动转交已武装的状态,与组件地址无关。
	TimerTaskComp moved(std::move(source));
	EXPECT_TRUE(moved.IsActive());
	EXPECT_FALSE(source.IsActive());
}

TEST(TimerQueueTest, MovedArmedTimerFiresOnceIntoNewOwner)
{
	EventLoop loop;
	int fires = 0;
	TimerTaskComp source;
	const auto now = Timestamp::now();
	source.RunAt(addTime(now, -2.0), [&fires] { ++fires; });
	TimerTaskComp moved(std::move(source));
	// 两个时刻都在过去,同批处理且开火在退出之前,不依赖 sleep 或调度速度。
	loop.runAt(addTime(now, -1.0), [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
	EXPECT_FALSE(source.IsActive());
	EXPECT_FALSE(moved.IsActive());
}

TEST(TimerQueueTest, MoveAssignCancelsDestinationOwnTimer)
{
	EventLoop loop;
	int sourceFires = 0;
	int destinationFires = 0;
	TimerTaskComp source;
	TimerTaskComp destination;
	const auto now = Timestamp::now();
	destination.RunAt(addTime(now, -3.0), [&destinationFires] { ++destinationFires; });
	source.RunAt(addTime(now, -2.0), [&sourceFires] { ++sourceFires; });
	destination = std::move(source);
	ASSERT_TRUE(destination.IsActive());
	EXPECT_FALSE(source.IsActive());
	loop.runAt(addTime(now, -1.0), [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, sourceFires);
	EXPECT_EQ(0, destinationFires);
}

namespace {
struct NoPinTimerComp
{
	TimerTaskComp timer;
};
static_assert(!entt::component_traits<NoPinTimerComp>::in_place_delete);
} // namespace

TEST(TimerQueueTest, SwapAndPopWithoutInPlaceDeleteKeepsSurvivorTimer)
{
	EventLoop loop;
	entt::registry registry;
	const auto removed = registry.create();
	const auto survivor = registry.create();
	int removedFires = 0;
	int survivorFires = 0;
	const auto now = Timestamp::now();
	registry.emplace<NoPinTimerComp>(removed).timer.RunAt(addTime(now, -3.0), [&removedFires] { ++removedFires; });
	registry.emplace<NoPinTimerComp>(survivor).timer.RunAt(addTime(now, -2.0), [&survivorFires] { ++survivorFires; });
	registry.destroy(removed);
	ASSERT_TRUE(registry.get<NoPinTimerComp>(survivor).timer.IsActive());
	loop.runAt(addTime(now, -1.0), [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(0, removedFires);
	EXPECT_EQ(1, survivorFires);
	EXPECT_FALSE(registry.get<NoPinTimerComp>(survivor).timer.IsActive());
}

TEST(TimerQueueTest, MovedFromObjectDestructionDoesNotCancel)
{
	EventLoop loop;
	int fires = 0;
	const auto now = Timestamp::now();
	auto source = std::make_unique<TimerTaskComp>();
	source->RunAt(addTime(now, -2.0), [&fires] { ++fires; });
	TimerTaskComp destination(std::move(*source));
	source.reset();
	ASSERT_TRUE(destination.IsActive());
	loop.runAt(addTime(now, -1.0), [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
}

TEST(TimerQueueTest, CallbackCanDestroyItsOwner)
{
	EventLoop loop;
	int fires = 0;
	const auto now = Timestamp::now();
	auto owner = std::make_unique<TimerTaskComp>();
	owner->RunAt(addTime(now, -2.0), [&owner, &fires] {
		owner.reset();
		++fires;
	});
	loop.runAt(addTime(now, -1.0), [&loop] { loop.quit(); });
	loop.loop();
	EXPECT_EQ(1, fires);
	EXPECT_EQ(nullptr, owner);
}

TEST(TimerQueueTest, EnTTDeletePreservesRawTimerOfSurvivingEntity)
{
	EventLoop loop;
	entt::registry registry;

	const entt::entity removed = registry.create();
	const entt::entity survivor = registry.create();
	auto& removedTimer = registry.emplace<TimerTaskComp>(removed);
	auto& survivorTimer = registry.emplace<TimerTaskComp>(survivor);
	TimerTaskComp* const survivorAddress = &survivorTimer;

	removedTimer.RunAfter(60.0, []() {});
	survivorTimer.RunAfter(60.0, []() {});
	ASSERT_TRUE(removedTimer.IsActive());
	ASSERT_TRUE(survivorTimer.IsActive());

	// 保留直接存 TimerTaskComp 时的地址稳定契约;无原地删除的正确性由上面的包装用例覆盖。
	registry.remove<TimerTaskComp>(removed);

	auto& preserved = registry.get<TimerTaskComp>(survivor);
	EXPECT_EQ(&preserved, survivorAddress);
	EXPECT_TRUE(preserved.IsActive());
}

TEST(TimerQueueTest, EnTTDeletePreservesSkillWrapperTimerOfSurvivingEntity)
{
	EventLoop loop;
	entt::registry registry;

	const entt::entity removed = registry.create();
	const entt::entity survivor = registry.create();
	auto& removedComp = registry.emplace<CastingTimerComp>(removed);
	auto& survivorComp = registry.emplace<CastingTimerComp>(survivor);
	CastingTimerComp* const survivorAddress = &survivorComp;

	removedComp.timer.RunAfter(60.0, []() {});
	survivorComp.timer.RunAfter(60.0, []() {});
	ASSERT_TRUE(removedComp.timer.IsActive());
	ASSERT_TRUE(survivorComp.timer.IsActive());

	// 玩家退出通过 registry.destroy() 进入同一条组件删除路径。
	registry.destroy(removed);

	auto& preserved = registry.get<CastingTimerComp>(survivor);
	EXPECT_EQ(&preserved, survivorAddress);
	EXPECT_TRUE(preserved.timer.IsActive());
}

TEST(TimerQueueTest, IsActiveTracksArmingNotDeadline)
{
	EventLoop loop;

	TimerTaskComp oneShot;
	TimerTaskComp repeating;

	std::atomic<bool> activeInsideOneShot{true};
	std::atomic<bool> activeInsideRepeating{false};
	std::atomic<int> ticks{0};

	oneShot.RunAfter(0.01, [&oneShot, &activeInsideOneShot]() {
		activeInsideOneShot = oneShot.IsActive();
	});

	repeating.RunEvery(0.01, [&repeating, &activeInsideRepeating, &ticks, &loop]() {
		activeInsideRepeating = repeating.IsActive();
		if (++ticks >= 2) {
			loop.quit();
		}
	});

	loop.runAfter(5.0, [&loop]() { loop.quit(); });
	loop.loop();

	// IsActive() answers "is a timer armed", not "is the deadline still in the
	// future" -- it no longer reads muduo's Timer at all. A one-shot retires
	// itself before invoking the callback; a repeating one stays armed.
	EXPECT_FALSE(activeInsideOneShot.load());
	EXPECT_TRUE(activeInsideRepeating.load());
	EXPECT_FALSE(oneShot.IsActive());
	EXPECT_TRUE(repeating.IsActive());
}

// ---------------------------------------------------------------------------
// Regression tests for firings that arrive after their arming was superseded.
//
// TimerQueue::handleRead() copies the whole expired batch out *before* running
// any callback, and cancelInLoop() cannot pull a timer back out of that copy --
// it can only record the cancellation in cancelingTimers_. So a timer already
// in the batch always runs, no matter what an earlier callback in the same
// batch did to it.
//
// Every test below arms its timers with timestamps that are already in the
// past. getExpired() therefore collects them into a single batch, and muduo
// orders that batch by expiration, so the earlier timestamp is guaranteed to
// run first. None of this depends on wall-clock luck or wakeup latency.
// ---------------------------------------------------------------------------

TEST(TimerQueueTest, RearmFromEarlierCallbackInSameBatchIsNotOrphaned)
{
	EventLoop loop;

	std::atomic<int> originalFires{0};
	std::atomic<int> rearmedFires{0};
	std::atomic<bool> stillArmedAfterBatch{false};

	TimerTaskComp driver;
	TimerTaskComp victim;

	victim.RunAt(addTime(Timestamp::now(), -1.0), [&originalFires]() {
		++originalFires;
	});

	driver.RunAt(addTime(Timestamp::now(), -2.0), [&victim, &rearmedFires]() {
		// Re-arm the victim while its own, already-expired timer is still
		// queued behind us in this same batch.
		victim.RunAfter(0.05, [&rearmedFires]() { ++rearmedFires; });
	});

	// Runs after the batch has been fully processed, before the re-armed
	// timer is due.
	loop.runAfter(0.01, [&victim, &stillArmedAfterBatch]() {
		stillArmedAfterBatch = victim.IsActive();
	});

	loop.runAfter(0.12, [&loop]() { loop.quit(); });
	loop.loop();

	// The superseded arming must not run anything.
	EXPECT_EQ(originalFires.load(), 0);

	// The victim must still hold the handle to the timer driver armed for it.
	// Losing it here is the actual defect: OnTimer() used to clear timerId on
	// behalf of the *old* firing, orphaning the new timer so that nothing
	// could cancel it when the victim died.
	EXPECT_TRUE(stillArmedAfterBatch.load());

	// Exactly once, and at its own due time -- not immediately, as part of the
	// stale firing.
	EXPECT_EQ(rearmedFires.load(), 1);
}

TEST(TimerQueueTest, RearmedTimerIsCancelledWhenOwnerIsDestroyed)
{
	EventLoop loop;

	std::atomic<int> rearmedFires{0};

	auto victim = std::make_unique<TimerTaskComp>();
	TimerTaskComp driver;

	victim->RunAt(addTime(Timestamp::now(), -1.0), []() {});

	driver.RunAt(addTime(Timestamp::now(), -2.0), [&victim, &rearmedFires]() {
		victim->RunAfter(0.06, [&rearmedFires]() { ++rearmedFires; });
	});

	// Destroy the victim before its re-armed timer comes due. ~TimerTaskComp
	// cancels through the TimerId it still holds; if the stale firing had
	// wiped that handle there would be nothing left to cancel, the timer would
	// outlive its owner, and the callback would run against freed memory.
	// Under the pre-fix code this is a real use-after-free -- build the test
	// with ASan to see it fail hard rather than as a miscount.
	loop.runAfter(0.02, [&victim]() { victim.reset(); });
	loop.runAfter(0.14, [&loop]() { loop.quit(); });
	loop.loop();

	EXPECT_EQ(rearmedFires.load(), 0);
}

TEST(TimerQueueTest, CancelFromEarlierCallbackInSameBatchStaysSilent)
{
	EventLoop loop;

	std::atomic<int> fires{0};

	TimerTaskComp driver;
	TimerTaskComp victim;

	victim.RunAt(addTime(Timestamp::now(), -1.0), [&fires]() { ++fires; });
	driver.RunAt(addTime(Timestamp::now(), -2.0), [&victim]() { victim.Cancel(); });

	loop.runAfter(0.05, [&loop]() { loop.quit(); });
	loop.loop();

	// Behaviour lock rather than a regression catch: this already held before
	// the generation check existed, because Cancel() clears `callback` and the
	// stale firing then found nothing to call. It is asserted here so that the
	// guarantee survives any future rework of that path.
	EXPECT_EQ(fires.load(), 0);
}

TEST(TimerQueueTest, CancelledTimerInSameBatchMustNotRun)
{
	EventLoop loop;

	std::atomic<int> victimFires{0};

	// 使用裸 muduo 定时器:TimerTaskComp 的 weak State 与 generation 也会拦截失效回调,
	// 经过组件无法判断本次是否由 TimerQueue 自身跳过。
	const TimerId victimId = loop.runAt(addTime(Timestamp::now(), -1.0), [&victimFires]() {
		++victimFires;
	});

	loop.runAt(addTime(Timestamp::now(), -2.0), [&loop, victimId]() {
		loop.cancel(victimId);
	});

	loop.runAfter(0.05, [&loop]() { loop.quit(); });
	loop.loop();

	// cancel() cannot pull a timer back out of the batch handleRead() already
	// copied out; it only records it in cancelingTimers_. Upstream muduo
	// consults that set solely in reset(), to stop a *repeating* timer being
	// re-inserted -- it does not skip the pending run(). So this only holds if
	// TimerQueue::handleRead() checks cancelingTimers_ before calling run().
	//
	// See docs/design/muduo-timer-cancellation-hazards.md.
	// 守卫两端都有:Windows 在 cpp/libs/engine/muduo_windows/src/muduo/net/TimerQueue.cc,
	// Linux 在 tools/archived/muduo_linux_overlay/muduo/net/TimerQueue.cc 的 LOCAL DELTA (1)
	// (由 setup_dependencies.sh 覆盖到 third_party/muduo-linux)。Windows 使用本目录 vcxproj,
	// Linux 使用 cpp/tests/redis_lifecycle_test/CMakeLists.txt 的 timer_queue_lifecycle_test。
	EXPECT_EQ(victimFires.load(), 0);
}

// Minimal reproduction of the shape that used to crash: an object that owns a
// bare muduo timer whose callback captures `this`, and cancels it in its
// destructor -- which is the textbook-correct thing to do, and still not enough.
class TimerOwner
{
public:
	TimerOwner(EventLoop& loop, Timestamp when, std::atomic<int>& firedAfterDeath)
		: loop_(loop)
	{
		timerId_ = loop.runAt(when, [this, &firedAfterDeath]() {
			// Deliberately does NOT touch *this*. We only need to observe
			// whether the callback ran at all; dereferencing a destroyed
			// object would make the test itself undefined behaviour instead
			// of a detector for it. Real code does touch `this` -- that is
			// exactly where it crashed.
			(void)this;
			++firedAfterDeath;
		});
	}

	~TimerOwner()
	{
		loop_.cancel(timerId_);
	}

	TimerOwner(const TimerOwner&) = delete;
	TimerOwner& operator=(const TimerOwner&) = delete;

private:
	EventLoop& loop_;
	TimerId timerId_;
};

TEST(TimerQueueTest, TimerOfObjectDestroyedInSameBatchMustNotRun)
{
	EventLoop loop;

	std::atomic<int> firedAfterDeath{0};

	auto owner = std::make_unique<TimerOwner>(
		loop, addTime(Timestamp::now(), -1.0), firedAfterDeath);

	// Earlier in the very same batch: destroy the owner. ~TimerOwner cancels,
	// but by then handleRead() has already copied the batch out, so
	// cancelInLoop() can do nothing except record the cancellation.
	loop.runAt(addTime(Timestamp::now(), -2.0), [&owner]() {
		owner.reset();
	});

	loop.runAfter(0.05, [&loop]() { loop.quit(); });
	loop.loop();

	// 同批先销毁宿主,再轮到它的裸定时器。独立 weak 状态可以在业务层保护寿命,
	// 本用例专门验证直接调用 cancel 的底层契约,不借助该外层保护。
	// See docs/design/muduo-timer-cancellation-hazards.md.
	EXPECT_EQ(firedAfterDeath.load(), 0);
}

TEST(TimerQueueTest, CancelledRepeatingTimerInSameBatchMustNotRunOrRestart)
{
	EventLoop loop;
	int fires = 0;
	TimerId repeatingId;
	repeatingId = loop.runEvery(kImmediateRepeatIntervalSeconds, [&] {
		++fires;
		// 若回归失败,只观测一次额外触发,避免重复定时器忙转。
		loop.cancel(repeatingId);
	});
	loop.runAt(addTime(Timestamp::now(), -1.0), [&] {
		loop.cancel(repeatingId);
		loop.cancel(repeatingId);
	});
	loop.runAfter(0.02, [&loop] { loop.quit(); });
	loop.loop();

	EXPECT_EQ(0, fires);
}

TEST(TimerQueueTest, RepeatingTimerCanCancelItselfTwice)
{
	EventLoop loop;
	int fires = 0;
	TimerId repeatingId;
	repeatingId = loop.runEvery(kImmediateRepeatIntervalSeconds, [&] {
		++fires;
		loop.cancel(repeatingId);
		loop.cancel(repeatingId);
	});
	loop.runAfter(0.02, [&loop] { loop.quit(); });
	loop.loop();
	// 回收后再次取消同一旧句柄也必须是无操作,不能读取已释放的 Timer。
	loop.cancel(repeatingId);

	EXPECT_EQ(1, fires);
}

TEST(TimerQueueTest, CancellationCallbackCleanupCanCancelAnotherActiveTimer)
{
	EventLoop loop;
	int victimFires = 0;
	int cleanupCount = 0;
	const TimerId victimId = loop.runAfter(0.01, [&victimFires] { ++victimFires; });
	TimerId cancelledId;

	// 只在被取消 Timer 的闭包中保留一个控制块,闭包销毁时执行清理并取消另一只活动 Timer。
	// 清理动作不读取已删除对象,其可观察行为与捕获对象析构中的 cancel 相同。
	auto cleanup = std::shared_ptr<void>(nullptr, [&](void*) {
		++cleanupCount;
		loop.cancel(cancelledId);
		loop.cancel(victimId);
	});
	cancelledId = loop.runAfter(1.0, [cleanup = std::move(cleanup)] {
		(void)cleanup;
	});

	// cancel 必须先从到期队列与活动集合同时摘除,再销毁闭包,使重入取消看到一致状态。
	loop.cancel(cancelledId);
	loop.runAfter(0.02, [&loop] { loop.quit(); });
	loop.loop();

	EXPECT_EQ(1, cleanupCount);
	EXPECT_EQ(0, victimFires);
}

TEST(TimerQueueTest, EventLoopDestructionCanCancelOwnAndAnotherTimer)
{
	int cleanupCount = 0;
	int victimCleanupCount = 0;
	TimerId cancelledId;
	{
		EventLoop loop;
		auto victimCleanup = std::shared_ptr<void>(nullptr, [&](void*) {
			++victimCleanupCount;
		});
		const TimerId victimId = loop.runAfter(120.0, [cleanup = std::move(victimCleanup)] {
			(void)cleanup;
		});
		auto cleanup = std::shared_ptr<void>(nullptr, [&loop, &cancelledId, victimId, &cleanupCount](void*) {
			++cleanupCount;
			// 直接通过仍在析构栈上的 loop 取消,不依赖已清空的线程局部 EventLoop 句柄。
			loop.cancel(cancelledId);
			loop.cancel(victimId);
		});
		cancelledId = loop.runAfter(60.0, [cleanup = std::move(cleanup)] {
			(void)cleanup;
		});
		// 不运行 loop:只由 TimerQueue 析构释放尚未到期回调。先清理较早 Timer 时会重入 cancel。
	}

	EXPECT_EQ(1, cleanupCount);
	EXPECT_EQ(1, victimCleanupCount);
}

TEST(TimerQueueTest, ExpiredCallbackCleanupCanCancelRepeatingTimer)
{
	EventLoop loop;
	int fires = 0;
	int firesAfterDestruction = 0;
	int ownerDestructions = 0;
	TimerId repeatingId;

	repeatingId = loop.runEvery(kImmediateRepeatIntervalSeconds, [&]() {
		++fires;
		if (fires == 1) {
			// 先让本批 reset 完成,再安排下一批退出。旧实现重新入队的 B 到期点不晚于退出定时器,
			// 必须先被取出并执行,不会因测试线程在进入 loop 前被抢占而漏掉第二次触发。
			loop.queueInLoop([&loop] {
				loop.runAfter(0.0, [&loop] { loop.quit(); });
			});
		}
		if (ownerDestructions != 0) {
			++firesAfterDestruction;
			// 旧实现会错误地重新入队;第二次触发自行退出,避免失败用例忙转。
			loop.cancel(repeatingId);
		}
	});

	struct Owner
	{
		Owner(EventLoop& eventLoop, TimerId timerId, int& destructionCount)
			: loop(eventLoop), timer(timerId), destructions(destructionCount) {}
		~Owner()
		{
			++destructions;
			loop.cancel(timer);
		}
		EventLoop& loop;
		TimerId timer;
		int& destructions;
	};

	auto owner = std::make_shared<Owner>(loop, repeatingId, ownerDestructions);
	loop.runAt(addTime(Timestamp::now(), -1.0), [owner = std::move(owner)] {
		// 唯一所有权由这只 Timer 的闭包持有,回调返回时仍存活,reset 删除闭包时才析构。
		(void)owner;
	});
	loop.loop();

	// 两只回调都已执行,清理较早的一次性回调时取消后面的重复定时器,必须阻止其重新入队。
	// 回调只读外置计数器,不解引用已销毁宿主,使回归检测本身保持良定义。
	EXPECT_EQ(1, ownerDestructions);
	EXPECT_EQ(1, fires);
	EXPECT_EQ(0, firesAfterDestruction);
}

TEST(TimerQueueTest, RepeatingTimerKeepsFiringAcrossGenerationCheck)
{
	EventLoop loop;

	std::atomic<int> ticks{0};

	// 数够触发次数再退出,避免固定时间窗口在事件循环繁忙时把晚执行误判成漏触发。
	// 下方 5s 定时器只限制失败等待,正常情况下不通过它退出。
	TimerTaskComp timer;
	timer.RunEvery(0.01, [&ticks, &loop]() {
		if (++ticks >= 3) {
			loop.quit();
		}
	});

	loop.runAfter(5.0, [&loop]() { loop.quit(); });
	loop.loop();

	// Every firing of a repeating timer carries the same bound generation, so
	// the staleness check must not retire it after the first tick.
	EXPECT_GE(ticks.load(), 3);
}

TEST(TimerQueueTest, SelfRearmFromOwnCallbackIsHonoured)
{
	EventLoop loop;

	std::atomic<int> fires{0};

	// 同样按触发次数退出,使调度延迟不改变预期次数。
	TimerTaskComp timer;
	std::function<void()> tick = [&]() {
		if (++fires == 1) {
			timer.RunAfter(0.01, tick);
		}
		else {
			loop.quit();
		}
	};

	timer.RunAfter(0.01, tick);

	loop.runAfter(5.0, [&loop]() { loop.quit(); });
	loop.loop();

	// 重挂时 Cancel 推进代际,新武装的回调匹配新代际;旧回调返回不能覆盖新状态。
	// 正在执行的 callable 由 muduo Timer 持有,等回调返回后的 reset 才释放,因此重挂不会中途析构它。
	EXPECT_EQ(fires.load(), 2);
}

// ---------------------------------------------------------------------------
// 定时器时钟域(本仓库改动):TimerQueue 按单调时钟到期,runAt 的墙钟时刻在调用时换算到单调域。
//
// 不改系统时钟:换算本身用纯函数 TimerQueue::monotonicFromWall 测;接进 EventLoop 之后,用过去 / 将来的
// 墙钟时刻验证排序不变、与 runAfter 同域比较。墙钟回拨的端到端效果(定时器不再晚 N 秒)需要改系统时间,
// 不在单测里做。
// ---------------------------------------------------------------------------

namespace {
// 一个 2025 年附近的墙钟读数与一个开机 5s 的单调读数,只用来固定换算的两个原点。
constexpr int64_t kWallNowUs = 1'760'000'000LL * Timestamp::kMicroSecondsPerSecond;
constexpr int64_t kMonoNowUs = 5LL * Timestamp::kMicroSecondsPerSecond;
} // namespace

TEST(TimerClockTest, MonotonicFromWallKeepsOffsetFromWallNow)
{
	const Timestamp wallNow(kWallNowUs);
	const Timestamp monoNow(kMonoNowUs);

	// 换算只保留"离现在还有多久":墙钟此刻对应单调此刻,前后偏移原样带过去。
	const int64_t deltasUs[] = {0, 1, 2'500'000, -1, -3'000'000};
	for (const int64_t deltaUs : deltasUs)
	{
		const Timestamp mono = TimerQueue::monotonicFromWall(Timestamp(kWallNowUs + deltaUs), wallNow, monoNow);
		EXPECT_EQ(kMonoNowUs + deltaUs, mono.microSecondsSinceEpoch()) << "delta " << deltaUs << "us";
	}

	// 早于开机时刻的墙钟时刻换算出 <= 0 的值:单调域里这只是"早就到期",不是无效值。
	const Timestamp longAgo = TimerQueue::monotonicFromWall(Timestamp(kWallNowUs - 10'000'000), wallNow, monoNow);
	EXPECT_EQ(-5'000'000, longAgo.microSecondsSinceEpoch());
}

TEST(TimerClockTest, MonotonicFromWallPreservesOrdering)
{
	const Timestamp wallNow(kWallNowUs);
	const Timestamp monoNow(kMonoNowUs);

	// 单调递增的墙钟时刻(含相差 1us 的相邻点、远早于开机的点)换算后仍严格单调递增:
	// 同一批 runAt 在队列里的先后只取决于它们的墙钟先后。
	const int64_t deltasUs[] = {-86'400'000'000LL, -2'000'000, -1, 0, 1, 50'000, 3'600'000'000LL};
	bool first = true;
	int64_t previousUs = 0;
	for (const int64_t deltaUs : deltasUs)
	{
		const int64_t currentUs =
			TimerQueue::monotonicFromWall(Timestamp(kWallNowUs + deltaUs), wallNow, monoNow).microSecondsSinceEpoch();
		if (!first)
		{
			EXPECT_LT(previousUs, currentUs) << "delta " << deltaUs << "us";
		}
		first = false;
		previousUs = currentUs;
	}
}

TEST(TimerClockTest, MonotonicNowDoesNotGoBackwards)
{
	Timestamp previous = TimerQueue::monotonicNow();
	for (int i = 0; i < 1000; ++i)
	{
		const Timestamp current = TimerQueue::monotonicNow();
		ASSERT_LE(previous.microSecondsSinceEpoch(), current.microSecondsSinceEpoch());
		previous = current;
	}
}

TEST(TimerClockTest, RunAtPastWallTimesFireInWallOrder)
{
	EventLoop loop;
	std::vector<int> order;
	const Timestamp now = Timestamp::now();

	// 故意乱序挂:同一批里的先后只能来自换算后的到期时刻,而不是挂的先后。
	loop.runAt(addTime(now, -1.0), [&order] { order.push_back(3); });
	loop.runAt(addTime(now, -3.0), [&order] { order.push_back(1); });
	loop.runAt(addTime(now, -2.0), [&order] { order.push_back(2); });
	loop.runAt(addTime(now, -0.5), [&loop] { loop.quit(); });
	// 兜底:换算一旦漏掉,上面几个会被当成几十年后的单调时刻,永远不到期。
	loop.runAfter(5.0, [&loop] { loop.quit(); });
	loop.loop();

	EXPECT_EQ((std::vector<int>{1, 2, 3}), order);
}

TEST(TimerClockTest, RunAtLongPastWallTimeStillFires)
{
	EventLoop loop;
	int fires = 0;

	// 1970 年的墙钟时刻换算到单调域是负数。它必须照常排在最前、照常触发,之后挂着的定时器也照常触发。
	// 注意:本用例区分不出 reset() 里 valid() → !timers_.empty() 那处改动 —— 这里负值定时器触发后队首是
	// 正值;回调里新挂的 <= 0 定时器会由 addTimerInLoop 直接上弦,Windows 又不靠 timerfd。那处改动是
	// 防御性的,本用例只守"负的单调值照常排序、触发且不卡住后续定时器"。
	loop.runAt(Timestamp::fromUnixTime(1), [&fires] { ++fires; });
	loop.runAfter(0.02, [&loop] { loop.quit(); });
	loop.runAfter(5.0, [&loop] { loop.quit(); });
	loop.loop();

	EXPECT_EQ(1, fires);
}

TEST(TimerClockTest, RunAtFutureWallTimeSharesDomainWithRunAfter)
{
	EventLoop loop;
	std::vector<int> order;
	const Timestamp now = Timestamp::now();

	// runAt(墙钟时刻)与 runAfter(时长)必须落在同一个时钟域里比先后。三个到期点相隔 40ms,
	// 远大于两只表各读一次的偏差;唤醒再晚,同一批内也按到期时刻排序,所以不依赖调度速度。
	loop.runAt(addTime(now, 0.06), [&order] { order.push_back(2); });
	loop.runAfter(0.02, [&order] { order.push_back(1); });
	loop.runAt(addTime(now, 0.10), [&order, &loop] {
		order.push_back(3);
		loop.quit();
	});
	loop.runAfter(5.0, [&loop] { loop.quit(); });
	loop.loop();

	EXPECT_EQ((std::vector<int>{1, 2, 3}), order);
}

#ifdef _WIN32
TEST(TimerClockTest, NextTimeoutMsCountsDownOnMonotonicClock)
{
	// Windows 没有 timerfd,poll 的超时由 nextTimeoutMs 给出。它必须与到期时刻同在单调域:
	// 拿单调时刻去减墙钟 now 会得到一个巨大的负数,poll 永远不阻塞,loop 空转。
	EventLoop loop;
	TimerQueue queue(&loop);
	EXPECT_EQ(60000, queue.nextTimeoutMs(60000));   // 空队列:给回 fallback

	// 10s 后到期:剩余量落在 (9s, 10s];留 1s 余量,测试线程被抢占也不至于误判。
	queue.addTimer([] {}, addTime(TimerQueue::monotonicNow(), 10.0), 0.0);
	const int timeoutMs = queue.nextTimeoutMs(60000);
	EXPECT_GT(timeoutMs, 9000);
	EXPECT_LE(timeoutMs, 10000);

	EXPECT_EQ(100, queue.nextTimeoutMs(100));       // 超过 fallback 时截到 fallback

	// 已到期的不阻塞。
	queue.addTimer([] {}, addTime(TimerQueue::monotonicNow(), -1.0), 0.0);
	EXPECT_EQ(0, queue.nextTimeoutMs(60000));
}
#endif // _WIN32

int main(int argc, char **argv)
{
	testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}

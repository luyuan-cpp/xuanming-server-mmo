#include <gtest/gtest.h>

#include "entt/src/entt/entity/registry.hpp"
#include "muduo/base/Timestamp.h"
#include "muduo/net/EventLoopThread.h"

#include <atomic>
#include <cassert>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <functional>
#include <memory>
#include <utility>

#include "combat/skill/comp/skill_comp.h"
#include "time/comp/timer_task_comp.h"

using namespace muduo;
using namespace muduo::net;

// 所有直接交给 EnTT 存储的生产 TimerTaskComp 组件都必须使用稳定地址。
// 内嵌组件需要独立声明；EnTT 不会通过包装成员传播
// TimerTaskComp::in_place_delete。
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

	// TimerId(16) + generation(4) + armed(1) + 3 bytes of tail padding = 24.
	//
	// The callback used to be a member -- 64 bytes on MSVC, 32 on libstdc++ --
	// which made this 88 and 56 respectively. It now lives in the closure
	// muduo's Timer holds, so the component is the same size on both
	// toolchains and carries no std::function at all.
	//
	// Upper bound rather than equality: a smaller layout on some future ABI is
	// not a failure. Growth is, because this is a per-entity component and
	// entities carry several of them (casting / recovery / channel / buff).
	// Shrinking `generation` would not help -- alignment forces the tail to 8
	// bytes whether it holds a uint64 or a uint32 plus a bool.
	//
	// 2026-08 起多了 `aliveToken`(std::shared_ptr,一对指针 = 16 字节):同批到期的
	// 定时器里,前一个回调销毁了后一个的宿主后,后者的闭包仍会带着已释放的 this 进
	// OnTimer —— generation 住在那块内存里,读它就已经是 use-after-free;闭包只持
	// weak_ptr、开火先 lock() 才能挡住。组件注释原话:correctness over 16 bytes。
	// 所以上界 = TimerId + 8 字节尾 + 一个 shared_ptr;再长仍然红。
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

TEST(TimerQueueTest, MoveAndCopyAreSafe)
{
	EventLoop loop;

	TimerTaskComp source;
	source.RunAfter(60.0, []() {});   // far enough out that it never fires here
	EXPECT_TRUE(source.IsActive());

	// A copy never inherits the timer: the callback captures the source's
	// `this`, so sharing it would leave a dangling pointer behind.
	TimerTaskComp copied(source);
	EXPECT_FALSE(copied.IsActive());

	// A move cancels the source rather than transferring the timer, for the
	// same reason -- the destination lives at a different address.
	TimerTaskComp moved(std::move(source));
	EXPECT_FALSE(moved.IsActive());
	EXPECT_FALSE(source.IsActive());
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

	// `removed` 先插入，因此不是紧凑存储中的末尾组件。若未启用原地删除，
	// EnTT 会把 `survivor` 移入这个空位，而 TimerTaskComp 必需的移动语义
	// 会取消 survivor 的定时器。
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

	// Raw muduo timers on purpose. Going through TimerTaskComp would prove
	// nothing: its Cancel() also clears `callback`, so the firing would look
	// suppressed even if TimerQueue ran it. Only a bare timer shows whether
	// TimerQueue itself skipped the callback.
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
	// Why it matters: cancelling usually means the callback's owner is being
	// destroyed. Without that check the callback runs against freed memory.
	// This cannot be fixed above muduo -- a component-level guard would have to
	// dereference the freed object just to read its own guard flag.
	//
	// See docs/design/muduo-timer-cancellation-hazards.md. Note this currently
	// passes only on Windows: the guard lives in
	// cpp/libs/engine/muduo_windows/src/muduo/net/TimerQueue.cc and has not
	// been ported to third_party/muduo-linux.
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

	// Destroyed first, expired second, same batch -- the original crash.
	// Cancelling in the destructor does not prevent it; only TimerQueue
	// skipping the callback does. Nothing above muduo can fix this: a
	// component-level "am I stale" flag would have to be read out of the
	// freed object to be consulted at all.
	//
	// See docs/design/muduo-timer-cancellation-hazards.md.
	EXPECT_EQ(firedAfterDeath.load(), 0);
}

TEST(TimerQueueTest, RepeatingTimerKeepsFiringAcrossGenerationCheck)
{
	EventLoop loop;

	std::atomic<int> ticks{0};

	// Quit on the tick count, not on a wall-clock window. The Windows muduo
	// has no timerfd and polls on the event loop's own cadence (~107ms here),
	// so a fixed 100ms window only ever gets one poll and this looked like a
	// broken repeating timer. The runAfter below is a safety net, not the
	// expected exit path.
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

	// Same reason as above: quit once the second firing lands, not after a
	// fixed window that may be shorter than one poll cycle.
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

	// Two things are locked in here.
	//
	// 1. The generation is bumped before the callback runs, so the new arming
	//    is the one the next firing matches against. Bumping it afterwards
	//    would silently swallow the re-arm.
	//
	// 2. Re-scheduling from inside one's own callback must not destroy the
	//    callable that is currently executing. This is the test that lets
	//    OnTimer() invoke cb() directly instead of taking a defensive copy:
	//    `tick` lives in the closure held by muduo's Timer, and that Timer
	//    survives until TimerQueue::reset(), which runs after the callback
	//    returns. Back when the callback was a member of TimerTaskComp,
	//    RunAfter() -> ScheduleTimer() -> Cancel() destroyed it mid-call and
	//    the copy was mandatory. If anyone moves the callback back into the
	//    component without restoring that copy, this test is what catches it.
	EXPECT_EQ(fires.load(), 2);
}

int main(int argc, char **argv)
{
	testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}

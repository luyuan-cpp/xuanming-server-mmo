#include <gtest/gtest.h>

#include "entt/src/entt/entity/registry.hpp"
#include "muduo/net/EventLoopThread.h"

#include <atomic>
#include <cassert>
#include <chrono>
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
	EXPECT_EQ(timer.GetEndTime(), 0U);
}

TEST(TimerQueueTest, MoveAndCopyAreSafe)
{
	TimerTaskComp source;
	source.SetCallBack([]() {});

	TimerTaskComp copied(source);
	EXPECT_FALSE(copied.IsActive());
	EXPECT_EQ(copied.GetEndTime(), 0U);

	TimerTaskComp moved(std::move(source));
	EXPECT_FALSE(moved.IsActive());
	EXPECT_EQ(moved.GetEndTime(), 0U);
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

int main(int argc, char **argv)
{
	testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}
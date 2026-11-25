#include <gtest/gtest.h>

#include <iostream>
#include <unordered_map>

#include <muduo/net/EventLoop.h>
#include <muduo/net/EventLoopThread.h>

#include "time/comp/timer_task_comp.h"

#include <stdio.h>
#include <thread>
#include <chrono>

#ifdef __linux__
#include <unistd.h>
#endif // __linux__
#include <thread_context/ecs_context.h>

using namespace muduo;
using namespace muduo::net;

// ---------------------------------------------------------------------------
// 测试场景说明（定时器销毁 & 悬空回调）:
//
// 场景 A:
//   对象1 在 timer 回调中为对象2 注册新的定时回调,
//   对象2 在回调中尝试取消自己的 timer（或已被销毁）,
//   对象2 销毁,
//   对象1 设置的对象2 回调尚未取消 → 验证不会崩溃
//
// 场景 B（类似）:
//   成员1 在回调中为成员3 注册新的定时回调,
//   成员3 在回调中尝试取消自己的 timer（或已被销毁）,
//   成员3 销毁,
//   成员1 设置的成员3 回调尚未取消 → 验证不会崩溃
// ---------------------------------------------------------------------------

int cnt = 0;
EventLoop* g_loop;


class GameTimerTest
{
public:

    void RunAfter()
    {
        timer.RunAfter(0.1, std::bind(&GameTimerTest::AfterCallBack, this));
    }

    void RunEvery()
    {
        timer.RunEvery(0.1, std::bind(&GameTimerTest::RunEveryCallBack, this));
    }

    void RunAt(const Timestamp& time)
    {
        timer.RunAt(time, std::bind(&GameTimerTest::RunAtCallBack, this));
    }



private:

    void AfterCallBack()
    {
        std::cout << "AfterCallBack" << this << std::endl;
        RunAfter();
    }

    void RunEveryCallBack()
    {
        std::cout << "runEveryCallBack" << this << std::endl;
        RunEvery();
    }

    void RunAtCallBack()
    {
        std::cout << "RunAtCallBack" << this << std::endl;

    }
private:
    TimerTaskComp timer;
    TimerTaskComp timer1;
};

class RecursionTimerTest
{
public:

    void RunAt(const Timestamp& time)
    {
        timer.RunAt(time, std::bind(&RecursionTimerTest::RunAtCallBack, this));
    }

private:

    void RunAtCallBack()
    {
        std::cout << "RunAtCallBack" << this << std::endl;
        RunAt(Timestamp::fromUnixTime(time(NULL) + 5));
    }
private:
    TimerTaskComp timer;
};

using GameTimerTestPtr = std::shared_ptr<GameTimerTest>;

using GameTimerTestMap = std::unordered_map<int32_t, GameTimerTestPtr>;


class TestCoreDump {
public:

    TestCoreDump() { id = i++;  std::cout << "TestCoreCreate" << id << std::endl; }

    ~TestCoreDump() {
        std::cout << "TestCoreDump destroy ." << id << std::endl;
    }

    void TestTimer() {
        std::cout << "TestCoreDump : TestTimer." << &timer << std::endl;

        timer.RunAfter(0.1, [this]() {
            Add();
            });
    }

    void Add() {
        std::cout << "TestCoreDump : new callback executed." << &timer << std::endl;
        data.push_back(1);
    }

    std::vector<int32_t> data;

    TimerTaskComp timer;
    int32_t id{ 0 };

    static int32_t i;
};

int32_t TestCoreDump::i = 0;


struct CastingTimerCompTest
{
    TimerTaskComp timer;

    void Add() {
        std::cout << "TestCoreDump : new callback executed." << std::endl;
        data.push_back(1);
    }

    std::vector<int32_t> data;
};


// 场景: entity 在定时器回调中被销毁，接着重新创建 → 验证不崩溃
void TestScenario() {

    const entt::entity targetEntity = tlsEcs.actorRegistry.create();

    auto& t = tlsEcs.actorRegistry.emplace<CastingTimerCompTest>(targetEntity);

    auto fn = [targetEntity = targetEntity]() {
        std::cout << "CastingTimerCompTest: callback -- destroy entity and recreate." << std::endl;

        tlsEcs.actorRegistry.destroy(targetEntity);

        entt::entity newEntity = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<CastingTimerCompTest>(newEntity);
    };

    t.timer.RunEvery(0.1, fn);
}


// 场景: 对象1 在定时器回调中为对象2 注册新回调，然后对象2 被销毁
// -> 验证悬空回调不会引发崩溃
void TestScenario1() {

    using DestroyObjType = std::shared_ptr<TestCoreDump>;

    TimerTaskComp obj1;
    DestroyObjType obj2 = std::make_shared<DestroyObjType::element_type>();

    obj1.RunAfter(0.1, [&obj1, &obj2]() {
        std::cout << "Callback from obj2 executed." << obj2->id << std::endl;

        // 对象1 在回调中为对象2 注册新的定时回调
        obj2->timer.RunAfter(0.2, [&obj1, &obj2]() {
            std::cout << "obj2->timer.RunAfter" << &obj2->timer << std::endl;
            // 对象2 在回调中尝试注册自己的 timer
            obj2->TestTimer();
        });

        // 对象2 随后被销毁 (reset)
        obj1.RunAfter(0.15, [&obj2]() {
            std::cout << "reset ." << &obj2->timer << std::endl;
            obj2.reset();
        });

        // 此时对象1 设置的对象2 回调可能悬空 -> 验证不崩溃
    });
}

// 副本定时器链式调用测试：准备→结算→强退→准备 循环
class DungeonTimerTest
{
public:

    void Init()
    {
        prepareEndTimer.RunAfter(0.01, std::bind(&DungeonTimerTest::PrePare, this));
        conclusionTimer.RunAfter(0.01, std::bind(&DungeonTimerTest::DungeonFinish, this));
        durationTimer.RunAfter(0.01, std::bind(&DungeonTimerTest::ForceRetreatScene, this));

    }

    void PrePare()
    {
        std::chrono::seconds s2(1);
        std::this_thread::sleep_for(s2);
        conclusionTimer.RunAfter(0.01, std::bind(&DungeonTimerTest::DungeonFinish, this));
    }

    void DungeonFinish()
    {
        std::chrono::seconds s2(2);
        std::this_thread::sleep_for(s2);
        durationTimer.RunAfter(0.01, std::bind(&DungeonTimerTest::ForceRetreatScene, this));
    }

    void ForceRetreatScene()
    {
        std::chrono::seconds s2(1);
        std::this_thread::sleep_for(s2);
        prepareEndTimer.RunAfter(0.01, std::bind(&DungeonTimerTest::PrePare, this));

    }

    TimerTaskComp prepareEndTimer;
    TimerTaskComp conclusionTimer;
    TimerTaskComp durationTimer;

};

// ---------------------------------------------------------------------------
// 综合测试: 定时器销毁安全性（无断言，仅验证不崩溃）
// ---------------------------------------------------------------------------

TEST(TimerDestroyTest, TimerDestroyDoesNotCrash)
{
    EventLoop loop;
    g_loop = &loop;

    TestScenario();

    {
        DungeonTimerTest t;
        t.Init();
    }

    GameTimerTest a1;

    GameTimerTest a;
    a.RunAfter();

    GameTimerTest g;
    g.RunEvery();

    GameTimerTest c;
    c.RunEvery();

    // 定时器在作用域结束时被销毁 -> 验证不崩溃
    { GameTimerTest t; t.RunAfter(); }
    { GameTimerTest t; t.RunAfter(); }
    { GameTimerTest t; t.RunEvery(); }
    { GameTimerTest t; t.RunEvery(); }

    RecursionTimerTest r;
    r.RunAt(Timestamp::fromUnixTime(time(NULL) + 1));
    GameTimerTestMap v;

    {
        GameTimerTestPtr p(new GameTimerTest);
        p->RunAt(Timestamp::fromUnixTime(time(NULL) + 1));
    }

    TestScenario1();

    // 让事件循环运行 3 秒后自动退出
    loop.runAfter(3.0, [&loop]() { loop.quit(); });
    loop.loop();

    // loop 结束后清理 registry 中残留的定时器组件，
    // 避免 EventLoop 销毁后 registry 析构触发 Cancel() 空指针
    tlsEcs.actorRegistry.clear();
}

int main(int argc, char** argv)
{
    testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}

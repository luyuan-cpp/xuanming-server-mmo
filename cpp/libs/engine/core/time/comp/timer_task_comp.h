#pragma once

#include <cstdint>
#include <memory>

#include "muduo/net/Callbacks.h"
#include "muduo/net/EventLoop.h"

using muduo::Timestamp;
using muduo::net::EventLoop;
using muduo::net::TimerCallback;
using muduo::net::TimerId;

// 定时器与宿主同寿命,只在所属 EventLoop 线程调度、移动、取消和销毁。
// 移动会转移已武装的定时器。回调应捕获实体句柄或业务 ID,不能依赖可移动宿主的地址;
// 必须捕获宿主 this 的类型须显式禁止移动。闭包从不捕获 TimerTaskComp 自身地址。
class TimerTaskComp
{
public:
    // 保留直接存储本组件时的地址稳定性;移动正确性已不依赖此标记。
    static constexpr bool in_place_delete = true;

    TimerTaskComp();
    ~TimerTaskComp();

    TimerTaskComp(const TimerTaskComp &);
    TimerTaskComp &operator=(const TimerTaskComp &) = delete;

    TimerTaskComp(TimerTaskComp &&param) noexcept;
    TimerTaskComp &operator=(TimerTaskComp &&param) noexcept;

    // 返回是否接受调度。无 loop、空回调或非法时长返回 false,保留现有计时。
    // 相对时长以秒计,必须有限且在 int64 微秒范围的一半以内,为截止点运算保留余量。
    // RunAfter 允许零或负延迟;RunEvery 的间隔必须大于零。
    // 释放旧回调时允许重入取消、调度或移动;较晚的重入操作决定最终状态。
    bool RunAt(const Timestamp &time, const TimerCallback &cb);
    bool RunAfter(double delay, const TimerCallback &cb);
    bool RunEvery(double interval, const TimerCallback &cb);

    void Cancel();

    // 只表示是否已武装,不读 muduo Timer 的私有状态。跨存盘的截止时间属于业务组件。
    bool IsActive() const;

private:
    struct State
    {
        TimerId timerId;
        uint32_t generation{0};
        bool armed{false};
    };

    // 只在实际调度时创建状态。复制不继承计时,移动整体转交状态。
    template <typename ScheduleFn>
    bool ScheduleTimer(TimerCallback cb, bool repeating, ScheduleFn &&schedule);

    // 先使旧状态失效,再释放可能重入或销毁宿主的闭包;之后不再访问宿主。
    static void CancelState(std::shared_ptr<State> state);

    // 开火期间强持有 State,cb 销毁或移动宿主也不会释放当前状态。
    // generation 使已被 muduo 摘进到期批次的旧回调在取消或重新武装后失效。
    static void OnTimer(State &state, uint32_t firedGeneration, bool repeating, const TimerCallback &cb);
    std::shared_ptr<State> state_;
};

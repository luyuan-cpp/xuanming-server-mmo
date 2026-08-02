#pragma once

#include "muduo/net/Callbacks.h"
#include "muduo/net/EventLoop.h"

using muduo::Timestamp;
using muduo::net::EventLoop;
using muduo::net::TimerCallback;
using muduo::net::TimerId;

// Timer must be owned by its callback target; if B owns A's timer,
// A may be destroyed before B's timer fires.
class TimerTaskComp
{
public:
    // muduo 回调捕获本组件地址。EnTT 默认的 swap-and-pop 删除会把末尾组件
    // 移入被删除位置；由于回调仍指向源地址，移动操作必须取消源定时器。
    // 将直接存入 ECS 的 TimerTaskComp 固定为原地删除，避免删除一个实体时
    // 取消或搬移另一个实体的定时器。作为 EnTT 存储之外的普通成员使用时，
    // 此标记不影响 TimerTaskComp 的行为。
    static constexpr bool in_place_delete = true;

    TimerTaskComp();
    ~TimerTaskComp();

    TimerTaskComp(const TimerTaskComp &);
    TimerTaskComp &operator=(const TimerTaskComp &) = delete;

    TimerTaskComp(TimerTaskComp &&param) noexcept;
    TimerTaskComp &operator=(TimerTaskComp &&param) noexcept;

    void RunAt(const Timestamp &time, const TimerCallback &cb);
    void RunAfter(double delay, const TimerCallback &cb);
    void RunEvery(double interval, const TimerCallback &cb);
    void Run() const;

    void Cancel();

    bool IsActive() const;

    uint64_t GetEndTime() const;

    void SetCallBack(const TimerCallback &cb);

private:
    // Common guard + schedule logic shared by RunAt/RunAfter/RunEvery.
    template <typename ScheduleFn>
    void ScheduleTimer(const TimerCallback &cb, ScheduleFn &&schedule);

    void OnTimer();

    TimerId timerId;
    TimerCallback callback;
};

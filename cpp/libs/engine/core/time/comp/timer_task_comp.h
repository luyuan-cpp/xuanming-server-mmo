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

    void Cancel();

    // "Is a timer currently armed", nothing more. Deliberately does not expose
    // a deadline: remaining time is entity data that must survive save/load,
    // reach the client, and be restored on reconnect, and muduo's Timer can do
    // none of those. The old GetEndTime() read it out of muduo's private Timer
    // and was removed -- put the deadline on the entity's protobuf component
    // instead, which is what already gets synced and persisted.
    bool IsActive() const;

private:
    // Common guard + schedule logic shared by RunAt/RunAfter/RunEvery.
    // Takes cb by value so it can be moved into the closure handed to muduo.
    template <typename ScheduleFn>
    void ScheduleTimer(TimerCallback cb, bool repeating, ScheduleFn &&schedule);

    // Everything it needs is bound at schedule time and lives in muduo's Timer,
    // not in this component.
    //
    // `firedGeneration` lets a firing left over from a superseded arming
    // recognise itself and bail out -- see the .cpp for the use-after-free it
    // prevents. `repeating` replaces asking muduo's Timer what kind it is; we
    // scheduled it, so we already know. `cb` used to be a member: keeping it in
    // the closure instead means Cancel() cannot destroy the callable while it
    // is executing, so re-arming from inside one's own callback needs no
    // defensive copy. It also drops sizeof(TimerTaskComp) from 88 to 24 on
    // MSVC (56 to 24 on libstdc++) and removes one std::function copy per
    // firing, which was a heap allocation on Linux for any callable larger
    // than the 16-byte inline buffer.
    void OnTimer(uint32_t firedGeneration, bool repeating, const TimerCallback &cb);

    TimerId timerId;
    // Bumped on every Cancel() -- and therefore on every re-arm, since
    // ScheduleTimer() cancels first. Only ever compared for equality, so
    // wrapping is harmless. uint32 rather than uint64 so that `armed` fits in
    // the same 8 bytes of tail padding.
    uint32_t generation = 0;
    // Backs IsActive(). True from a successful arming until the one-shot fires
    // or Cancel() runs; a repeating timer stays armed.
    bool armed = false;
};

#pragma once

#include <memory>

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
    // defensive copy. It also shrank the component substantially (88 -> 24 on
    // MSVC, 56 -> 24 on libstdc++ at the time) and removes one std::function
    // copy per firing, which was a heap allocation on Linux for any callable
    // larger than the 16-byte inline buffer. (`aliveToken` has since added one
    // pointer pair back — see its comment; correctness over 16 bytes.)
    void OnTimer(uint32_t firedGeneration, bool repeating, const TimerCallback &cb);

    // 存活令牌:定时器闭包只持有它的 weak_ptr,开火时先 lock() 再碰 this。
    //
    // 为什么 generation 挡不住这一类:muduo 的 TimerQueue::handleRead 先把到期
    // 定时器整批取出(getExpired 已把它们从 activeTimers_ 摘掉),再逐个 run()。
    // 此后调 cancel() 只会记进 cancelingTimers_ —— 那只影响重复定时器要不要重挂,
    // **run() 照常发生**。于是批内前一个回调若销毁了后一个定时器的宿主,后者的
    // 闭包仍会以已释放的 this 进入 OnTimer,而 generation 本身就住在那块内存里,
    // 读它就已经是 use-after-free。
    //
    // 可达路径(2026-08-03 审计):同一实体上两个 buff 同批到期,buff1 的
    // OnBuffExpire → RemoveSubBuff → RemoveBuff → OnBuffExpire →
    // buffList.erase(buff2) 同步析构 buff2 的 BuffEntry(内含 expireTimerTaskComp),
    // 而 buff2 的定时器就在同一批里等着 run()。实体整体销毁(DestroyEntity 连带
    // 析构 BuffListComp)同理。
    //
    // 懒创建:只有真正挂过定时器的组件才付这次控制块分配,未武装的
    // TimerTaskComp(数量很大:每 buff / 每技能一个)不受影响。
    std::shared_ptr<char> aliveToken;

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

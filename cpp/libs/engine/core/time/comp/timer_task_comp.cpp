#include "timer_task_comp.h"
#include "muduo/net/TimerId.h"

// EventLoop destructor sets t_loopInThisThread = NULL before entt registry
// cleanup destroys components, so every call site must tolerate nullptr.
static inline muduo::net::EventLoop* GetThreadEventLoop() {
    return muduo::net::EventLoop::getEventLoopOfCurrentThread();
}

// ---- Lifecycle --------------------------------------------------------------

// Default ctor: timerId and callback are already value-initialized.
TimerTaskComp::TimerTaskComp() = default;

TimerTaskComp::~TimerTaskComp() {
    Cancel();
}

// Copy ctor: intentionally does NOT copy the timer. The timer callback
// captures `this`, so sharing it across instances would create a dangling
// pointer. The copy starts empty; the source keeps its timer.
TimerTaskComp::TimerTaskComp(const TimerTaskComp& /*unused*/) {}

// Move ctor: cancel the source's timer (its callback captures source's
// `this` which will be in a moved-from state). Destination starts empty.
TimerTaskComp::TimerTaskComp(TimerTaskComp&& param) noexcept {
    param.Cancel();
}

TimerTaskComp& TimerTaskComp::operator=(TimerTaskComp&& param) noexcept {
    if (this != &param) {
        Cancel();        // release our own timer first
        param.Cancel();  // then invalidate the source
    }
    return *this;
}

// ---- Scheduling -------------------------------------------------------------

template <typename ScheduleFn>
void TimerTaskComp::ScheduleTimer(TimerCallback cb, bool repeating, ScheduleFn&& schedule) {
    Cancel();  // also bumps `generation`, retiring any firing already in flight

    if (!cb) {
        return;
    }

    auto* loop = GetThreadEventLoop();
    if (loop == nullptr) {
        return;
    }

    // A lambda rather than std::bind: the bind expression is 32 bytes on
    // libstdc++, past its 16-byte inline buffer, so every scheduled timer
    // heap-allocated. Capturing the callback pushes this past the buffer again,
    // but that allocation replaces two others -- the old `callback = cb` member
    // copy and the defensive copy that used to happen on every firing.
    const uint32_t gen = generation;
    timerId = schedule(loop, [this, gen, repeating, cb = std::move(cb)]() {
        OnTimer(gen, repeating, cb);
    });
    armed = true;
}

void TimerTaskComp::RunAt(const Timestamp& time, const TimerCallback& cb) {
    ScheduleTimer(cb, /*repeating=*/false, [&](EventLoop* loop, TimerCallback bound) {
        return loop->runAt(time, std::move(bound));
    });
}

void TimerTaskComp::RunAfter(double delay, const TimerCallback& cb) {
    ScheduleTimer(cb, /*repeating=*/false, [&](EventLoop* loop, TimerCallback bound) {
        return loop->runAfter(delay, std::move(bound));
    });
}

void TimerTaskComp::RunEvery(double interval, const TimerCallback& cb) {
    ScheduleTimer(cb, /*repeating=*/true, [&](EventLoop* loop, TimerCallback bound) {
        return loop->runEvery(interval, std::move(bound));
    });
}

// ---- Execution & cancellation -----------------------------------------------

void TimerTaskComp::Cancel() {
    // Null-check required: EventLoop may already be destroyed during shutdown.
    auto* loop = GetThreadEventLoop();
    if (loop != nullptr) {
        loop->cancel(timerId);
    }
    timerId = TimerId();
    armed = false;
    // Retire the arming we just dropped. cancel() alone is not enough: if this
    // timer is already inside the batch TimerQueue::handleRead() pulled out,
    // cancelInLoop() can only note it in cancelingTimers_ -- the run() call is
    // still coming. Bumping here lets that firing detect it is obsolete.
    ++generation;
}

// ---- Query ------------------------------------------------------------------

// Answered from our own flag, never by reaching into muduo's Timer.
//
// The Timer belongs to the EventLoop's TimerQueue, not to us, and ~TimerQueue
// deletes every timer still pending *without* running its callback -- so
// OnTimer never gets to clear timerId and any pointer we kept would dangle.
// Reading expiration() off it, as this used to, was a use-after-free during
// teardown. Holding a bool instead means there is nothing to dangle.
//
// Note the semantics shifted slightly: a one-shot whose deadline has passed
// but whose callback has not run yet now reports true, where the old
// expiration-based test reported false. For the one production caller --
// "is this cast/recovery/channel phase still running?" in
// cpp/libs/services/scene/combat/skill/system/skill.cpp -- true is the more
// accurate answer: the phase has not ended until its callback says so.
//
// Deliberately NOT the place to ask "how much time is left". That is data:
// it has to survive a save/load round trip, reach the client, and come back
// after a reconnect. muduo's Timer can do none of those, which is why
// GetEndTime() was removed rather than reimplemented here -- the deadline
// belongs in the entity's own protobuf component.
bool TimerTaskComp::IsActive() const {
    return armed;
}

// ---- Internal ---------------------------------------------------------------

void TimerTaskComp::OnTimer(uint32_t firedGeneration, bool repeating, const TimerCallback& cb) {
    // A timer that was already in the expired batch fires even if it has since
    // been cancelled: handleRead() copies the batch out before running any
    // callback, and cancelInLoop() can only record the cancellation.
    //
    // That opens a use-after-free. Say entities 1 and 3 both expire in the
    // same batch, 1 first, and 1's callback re-arms 3. Entity 3's component
    // now holds the NEW TimerId, and then its OLD timer fires. Clearing
    // timerId here would throw away the only handle to the new timer -- so
    // when entity 3 is destroyed, Cancel() would cancel nothing, the new timer
    // would survive its owner, and fire into freed memory.
    //
    // The generation is bumped by every Cancel(), and ScheduleTimer() cancels
    // before arming, so a stale firing never matches. Skipping it also makes
    // Cancel() mean what it says: the callback does not run.
    if (firedGeneration != generation) {
        return;
    }

    // No need to ask muduo whether this timer repeats -- we scheduled it, so
    // we know. One-shots are deleted by TimerQueue::reset() once this batch
    // finishes, so drop the handle now rather than let it dangle.
    if (!repeating) {
        timerId = TimerId();
        armed = false;
    }

    // No defensive copy needed. `cb` lives in the closure muduo's Timer holds,
    // and that Timer is not destroyed until TimerQueue::reset(), which runs
    // after this returns -- so re-scheduling or cancelling from inside cb()
    // cannot pull the callable out from under us. When cb was a member of this
    // component, Cancel() destroyed it mid-call and a copy was mandatory.
    cb();
}

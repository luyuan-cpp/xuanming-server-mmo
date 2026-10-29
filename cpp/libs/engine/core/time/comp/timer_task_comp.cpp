#include "timer_task_comp.h"

#include <cmath>
#include <limits>
#include <utility>

#include "muduo/net/TimerId.h"

// EventLoop 可能先于线程局部 registry 销毁,取消时必须允许没有当前 loop。
static EventLoop* GetThreadEventLoop() {
    return EventLoop::getEventLoopOfCurrentThread();
}

namespace {
bool IsSupportedDelay(double seconds) {
    // addTime 会把秒转换为 int64 微秒,截止点相加和队列求差还需要整数余量。
    constexpr double maxDelaySeconds =
        static_cast<double>(std::numeric_limits<int64_t>::max() / 2) / Timestamp::kMicroSecondsPerSecond;
    return std::isfinite(seconds) && std::abs(seconds) < maxDelaySeconds;
}
} // namespace

TimerTaskComp::TimerTaskComp() = default;

TimerTaskComp::~TimerTaskComp() {
    Cancel();
}

// 复制得到未武装的新定时器,源对象继续独占原计时。
TimerTaskComp::TimerTaskComp(const TimerTaskComp& /*unused*/) {}

TimerTaskComp::TimerTaskComp(TimerTaskComp&& param) noexcept
    : state_(std::move(param.state_)) {}

TimerTaskComp& TimerTaskComp::operator=(TimerTaskComp&& param) noexcept {
    if (this != &param) {
        auto previous = std::move(state_);
        state_ = std::move(param.state_);
        // 先交接所有权;旧闭包的析构可能再次操作目标,甚至销毁源对象。
        CancelState(std::move(previous));
    }
    return *this;
}

template <typename ScheduleFn>
bool TimerTaskComp::ScheduleTimer(TimerCallback cb, bool repeating, ScheduleFn&& schedule) {
    auto* loop = GetThreadEventLoop();
    if (!cb || loop == nullptr) {
        return false;
    }
    if (!state_) {
        state_ = std::make_shared<State>();
    }

    const auto state = state_;
    const auto previousId = state->timerId;
    const bool previouslyArmed = state->armed;
    const auto gen = ++state->generation;
    state->timerId = schedule(loop, [weakState = std::weak_ptr<State>(state), gen, repeating,
                                      cb = std::move(cb)]() {
        // 先前的到期回调可能已经销毁或移动宿主;只访问保活后的独立状态。
        const auto state = weakState.lock();
        if (state) {
            OnTimer(*state, gen, repeating, cb);
        }
    });
    state->armed = true;
    // 最后才释放旧闭包:析构可以取消/替换新计时、移动或销毁宿主。
    // 此后不再访问 this,也不回写 State,让重入操作决定最终结果。
    if (previouslyArmed) {
        loop->cancel(previousId);
    }
    return true;
}

bool TimerTaskComp::RunAt(const Timestamp& time, const TimerCallback& cb) {
    return ScheduleTimer(cb, false, [&](EventLoop* loop, TimerCallback bound) {
        return loop->runAt(time, std::move(bound));
    });
}

bool TimerTaskComp::RunAfter(double delay, const TimerCallback& cb) {
    if (!IsSupportedDelay(delay)) {
        return false;
    }
    return ScheduleTimer(cb, false, [&](EventLoop* loop, TimerCallback bound) {
        return loop->runAfter(delay, std::move(bound));
    });
}

bool TimerTaskComp::RunEvery(double interval, const TimerCallback& cb) {
    if (!IsSupportedDelay(interval) || interval <= 0.0) {
        return false;
    }
    return ScheduleTimer(cb, true, [&](EventLoop* loop, TimerCallback bound) {
        return loop->runEvery(interval, std::move(bound));
    });
}

void TimerTaskComp::Cancel() {
    CancelState(state_);
}

void TimerTaskComp::CancelState(std::shared_ptr<State> state) {
    if (!state) {
        return;
    }
    const auto previousId = state->timerId;
    const bool previouslyArmed = state->armed;
    state->timerId = TimerId();
    state->armed = false;
    // cancel 无法保证移出已取出的到期批次,必须另外使旧开火失效。
    ++state->generation;
    if (auto* loop = GetThreadEventLoop(); loop != nullptr && previouslyArmed) {
        loop->cancel(previousId);
    }
}

bool TimerTaskComp::IsActive() const {
    return state_ && state_->armed;
}

void TimerTaskComp::OnTimer(State& state, uint32_t firedGeneration, bool repeating, const TimerCallback& cb) {
    if (firedGeneration != state.generation) {
        return;
    }
    if (!repeating) {
        state.timerId = TimerId();
        state.armed = false;
    }
    // cb 存在于 muduo 的闭包中,不存于 State;重入取消、重挂或宿主销毁都不会析构当前 callable。
    // cb 返回后不再访问宿主或状态,避免把新武装的计时改回旧值。
    cb();
}

#pragma once

#include <functional>
#include <optional>
#include <utility>

#include "core/async/coroutine_callback.h"
#include "core/time/comp/timer_task_comp.h"
#include "muduo/base/Logging.h"

namespace etcd_registration
{
// 注册的预算与退避都靠这个定时器。排不上只会是本线程没有 EventLoop 或时长非法,属于装配错误:
// 按“没有应答”或“退避结束”继续都不对 —— 前者让注册循环在同一个栈上空转,后者静默结束注册。直接终止。
inline void ArmOrDie(TimerTaskComp& timer, double seconds, std::function<void()> fire, const char* what)
{
	if (!timer.RunAfter(seconds, std::move(fire)))
	{
		LOG_FATAL << "etcd registration could not arm its " << what << " timer (seconds=" << seconds
				  << "): no EventLoop on this thread or an invalid duration";
	}
}

// 空外层表示本地停止，空内层表示在注册预算内没有收到可信应答。
// 提交拒绝或传输失败不冒充 CAS 拒绝，继续使用原来的注册预算。每次等待有自己的完成回调，
// 超时后旧应答无法被下一次 CAS 或租约请求领取。
template<class Reply, class Submit>
auto AwaitReply(EventLoop& loop, TimerTaskComp& timer, double budgetSeconds, Submit submit)
{
	using Result = std::optional<Reply>;
	return loop_async::AwaitCallback<Result>(loop,
		[&timer, budgetSeconds, submit = std::move(submit)](auto done) mutable {
			ArmOrDie(timer, budgetSeconds, [done] { done(std::nullopt); }, "response budget");
			submit([done = std::move(done)](Result reply) mutable {
				if (reply) done(std::move(reply));
			});
			return true;
		}, std::nullopt);
}

inline auto Delay(EventLoop& loop, TimerTaskComp& timer, double seconds)
{
	return loop_async::AwaitCallback<bool>(loop,
		[&timer, seconds](auto done) {
			ArmOrDie(timer, seconds, [done = std::move(done)] { done(true); }, "retry backoff");
			return true;
		}, false);
}
}

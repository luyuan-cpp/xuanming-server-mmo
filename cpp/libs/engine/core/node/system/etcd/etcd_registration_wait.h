#pragma once

#include <optional>
#include <utility>

#include "core/async/coroutine_callback.h"
#include "core/time/comp/timer_task_comp.h"

namespace etcd_registration
{
// 空外层表示本地停止，空内层表示在注册预算内没有收到可信应答。
// 提交拒绝或传输失败不冒充 CAS 拒绝，继续使用原来的注册预算。每次等待有自己的完成回调，
// 超时后旧应答无法被下一次 CAS 或租约请求领取。
template<class Reply, class Submit>
auto AwaitReply(EventLoop& loop, TimerTaskComp& timer, double budgetSeconds, Submit submit)
{
	using Result = std::optional<Reply>;
	return loop_async::AwaitCallback<Result>(loop,
		[&timer, budgetSeconds, submit = std::move(submit)](auto done) mutable {
			if (!timer.RunAfter(budgetSeconds, [done] { done(std::nullopt); })) return false;
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
			return timer.RunAfter(seconds, [done = std::move(done)] { done(true); });
		}, false);
}
}

#include "loop_task_scope.h"

#define BOOST_COBALT_NO_LIB 1
#include <boost/asio/bind_cancellation_slot.hpp>
#include <boost/cobalt/spawn.hpp>

#include <cassert>
#include <functional>
#include <type_traits>
#include <unordered_set>
#include <utility>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "thread_context/trace_context_tls.h"

namespace
{
	namespace asio = boost::asio;
	namespace cobalt = boost::cobalt;
	using muduo::net::EventLoop;

	// execution_context 只提供执行器属性，实际执行与投递均由 Scene 的 muduo loop 驱动。
	struct SceneExecutor
	{
		std::reference_wrapper<EventLoop> loop;
		std::reference_wrapper<asio::execution_context> context;
		bool allowInline = true;

		bool operator==(const SceneExecutor& other) const noexcept
		{
			return &loop.get() == &other.loop.get() && &context.get() == &other.context.get() &&
				allowInline == other.allowInline;
		}
		asio::execution_context& query(asio::execution::context_t) const noexcept { return context.get(); }
		asio::execution::blocking_t query(asio::execution::blocking_t) const noexcept
		{
			if (allowInline) return asio::execution::blocking.possibly;
			return asio::execution::blocking.never;
		}
		SceneExecutor require(asio::execution::blocking_t::never_t) const noexcept { return {loop, context, false}; }
		SceneExecutor require(asio::execution::blocking_t::possibly_t) const noexcept { return {loop, context, true}; }

		template<class Work>
		void execute(Work&& work) const
		{
			if (allowInline && loop.get().isInLoopThread())
			{
				std::forward<Work>(work)();
				return;
			}
			// muduo 的 Functor 可拷贝，Cobalt 的完成对象可能只可移动。
			auto pending = std::make_shared<std::decay_t<Work>>(std::forward<Work>(work));
			// 投递出去再执行的恢复会离开当前作用域：记下此刻的 trace，执行时装回(docs/design/distributed-tracing.md §5.6)。
			// 协程恢复都是一次性投递；就地执行的分支本来就在当前上下文里，不用管。
			loop.get().queueInLoop(tracing::Bind([pending] { (*pending)(); }));
		}
	};
}

struct LoopTaskScope::State
{
	EventLoop& loop;
	asio::execution_context context;
	cobalt::executor executor;
	std::unordered_set<std::shared_ptr<asio::cancellation_signal>> active;
	bool stopped = false;

	explicit State(EventLoop& loop) : loop(loop), executor(SceneExecutor{loop, context}) {}
};

LoopTaskScope::LoopTaskScope(EventLoop& loop) : state_(std::make_shared<State>(loop))
{
    loop.assertInLoopThread();
}

LoopTaskScope::~LoopTaskScope() { Stop(); }

void LoopTaskScope::Close()
{
    state_->loop.assertInLoopThread();
    state_->stopped = true;
}

void LoopTaskScope::Stop()
{
	const auto state = state_;
	state->loop.assertInLoopThread();
	state->stopped = true;
	// 根完成通知保留信号，直到 Cobalt 清除该停止槽。
	const auto pending = std::exchange(state->active, {});
	for (const auto& signal : pending) signal->emit(asio::cancellation_type::all);
}

bool LoopTaskScope::IsAccepting() const
{
	const auto state = state_;
	if (!state) return false;
	state->loop.assertInLoopThread();
	return !state->stopped;
}

bool LoopTaskScope::Start(cobalt::task<void> task, const char* name)
{
	const auto state = state_;
	if (!state) return false;
	state->loop.assertInLoopThread();
	if (state->stopped) return false;
	const auto signal = std::make_shared<asio::cancellation_signal>();
	state->active.insert(signal);
	cobalt::spawn(state->executor, std::move(task),
		asio::bind_cancellation_slot(signal->slot(), [state, signal, name](std::exception_ptr error) {
			state->loop.assertInLoopThread();
			state->active.erase(signal);
			if (error) LOG_ERROR << name << " task failed with an exception";
		}));
	return true;
}

#include "loop_task_scope.h"

#define BOOST_COBALT_NO_LIB 1
#include <boost/asio/bind_cancellation_slot.hpp>
#include <boost/asio/error.hpp>
#include <boost/cobalt/spawn.hpp>
#include <boost/system/system_error.hpp>

#include <cassert>
#include <exception>
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

	// 任务体抛出的异常由 Cobalt 收进 promise，根完成回调在这里统一处置，不吞掉。
	// 本地停止之后，同一任务的下一次 co_await 会在挂起前抛 operation_aborted(Cobalt 的 throw_if_cancelled 默认开启)：
	// 这是停止的正常结局，记一条警告。抛点之后的业务代码不再执行，所以拿到 nullopt 就该直接 co_return。
	// 其余异常按改造前回调链的结局处理：那时异常逸出 EventLoop，进程终止。这里带上任务名和 what() 经 LOG_FATAL 终止，
	// 不让半途更新的账本、计数和围栏状态继续运行(AGENTS.md §11.3：错误不得吞掉)。
	void ReportTaskFailure(const bool cancelledByStop, const char* name, const std::exception_ptr& error)
	{
		try
		{
			std::rethrow_exception(error);
		}
		catch (const boost::system::system_error& failure)
		{
			if (cancelledByStop && failure.code() == asio::error::operation_aborted)
			{
				LOG_WARN << name << " task ended by local stop while awaiting: " << failure.what();
				return;
			}
			LOG_FATAL << name << " task failed with an exception: " << failure.what();
		}
		catch (const std::exception& failure)
		{
			LOG_FATAL << name << " task failed with an exception: " << failure.what();
		}
		catch (...)
		{
			LOG_FATAL << name << " task failed with a non-standard exception";
		}
	}
}

struct LoopTaskScope::State
{
	EventLoop& loop;
	asio::execution_context context;
	cobalt::executor executor;
	std::unordered_set<std::shared_ptr<asio::cancellation_signal>> active;
	bool stopped = false;
	// Stop 发过本地取消。Close 只封准入、不取消，所以单独记：operation_aborted 只在发过取消之后才算停止的结局。
	bool cancelled = false;

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
	state->cancelled = true;
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
			if (error) ReportTaskFailure(state->cancelled, name, error);
		}));
	return true;
}

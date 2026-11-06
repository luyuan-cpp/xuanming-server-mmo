#pragma once

#include <memory>

namespace boost::cobalt { template<typename T> struct task; }
namespace muduo::net { class EventLoop; }

// 每个 Scene EventLoop 持有一个。只能在所属 loop 启动、停止和销毁。
class SceneTaskScope final
{
public:
	explicit SceneTaskScope(muduo::net::EventLoop& loop);
	~SceneTaskScope();
	SceneTaskScope(const SceneTaskScope&) = delete;
	SceneTaskScope& operator=(const SceneTaskScope&) = delete;

	// 发起前可用来跳过会触发 I/O 的同步预检。
	[[nodiscard]] static bool IsAccepting();
	// 接纳后在当前栈启动，直到任务首次挂起或结束。名称为静态存储期的日志标签。
	[[nodiscard]] static bool TryStart(boost::cobalt::task<void> task, const char* name);
	// 入场、存盘和退出释放在业务排空期间仍可推进，不能在维护 P1 与跟随一起取消。
	[[nodiscard]] static bool IsDurableAccepting();
	[[nodiscard]] static bool TryStartDurable(boost::cobalt::task<void> task, const char* name);
	// 关闭准入并取消本地等待。完成通知由 loop 异步回收。
	void Stop();
	// 只在原业务排空结束后的 BeforeTeardown，以及析构兜底使用。
	void StopAll();

private:
	struct State;
	static thread_local std::weak_ptr<State> current_;
	std::shared_ptr<State> state_;
};

#pragma once

#include <memory>

namespace boost::cobalt { template<typename T> struct task; }
namespace muduo::net { class EventLoop; }

// 任务和停止信号归现有 muduo loop。此对象不创建线程，也不等待远端完成。
class LoopTaskScope final
{
public:
    explicit LoopTaskScope(muduo::net::EventLoop& loop);
    ~LoopTaskScope();
    LoopTaskScope(const LoopTaskScope&) = delete;
    LoopTaskScope& operator=(const LoopTaskScope&) = delete;

    [[nodiscard]] bool IsAccepting() const;
    // name 必须有静态存储期，异步完成时用于日志。
    // 任务体的异常不吞：Stop 之后再 co_await 抛出的 operation_aborted 只记一条警告；其余异常带 name 和 what()
    // 经 LOG_FATAL 终止进程，与改造前回调里的异常逸出 EventLoop 是同一结局。
    [[nodiscard]] bool Start(boost::cobalt::task<void> task, const char* name);
    // 只封住准入，已有任务继续推进。持久写入仍由原业务账本判断排空。
    void Close();
    // 结束本地等待。不是撤回远端命令，也不是持久写入完成证明。
    // 正在等待的那一次得到 nullopt；同一任务此后的 co_await 会在挂起前抛 operation_aborted。
    void Stop();

private:
    struct State;
    std::shared_ptr<State> state_;
};

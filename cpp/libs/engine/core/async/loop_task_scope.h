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
    [[nodiscard]] bool Start(boost::cobalt::task<void> task, const char* name);
    // 只封住准入，已有任务继续推进。持久写入仍由原业务账本判断排空。
    void Close();
    // 结束本地等待。不是撤回远端命令，也不是持久写入完成证明。
    void Stop();

private:
    struct State;
    std::shared_ptr<State> state_;
};

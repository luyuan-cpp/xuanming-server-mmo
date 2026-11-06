#pragma once

#define BOOST_COBALT_NO_LIB 1
#include <boost/asio/dispatch.hpp>
#include <boost/cobalt/op.hpp>

#include <functional>
#include <memory>
#include <optional>
#include <utility>

#include "muduo/net/EventLoop.h"

namespace loop_async
{
// 把已有单次异步接口接入 Cobalt，不添加超时或重试。
// submit 和回调都属于 loop；true 承诺回调，false 表示未接纳且不会再回调。
// T 必须拥有结果，不能携带借用的回复指针。nullopt 专指本地停止。
template<class T, class Submit>
class Callback final : public boost::cobalt::op<std::optional<T>>
{
    using Handler = boost::cobalt::completion_handler<std::optional<T>>;
    struct Completion
    {
        muduo::net::EventLoop& loop;
        std::optional<Handler> handler;
        bool submitting = true;

        Completion(muduo::net::EventLoop& loop, Handler handler)
            : loop(loop), handler(std::move(handler)) {}

        void Finish(std::optional<T> result)
        {
            loop.assertInLoopThread();
            if (!handler) return;
            auto complete = std::move(*handler);
            handler.reset();
            auto slot = complete.get_cancellation_slot();
            if (slot.is_connected()) slot.clear();
            if (submitting)
            {
                // 交给 Cobalt 的即时完成协议，await_suspend 返回前不重入业务。
                auto executor = complete.get_immediate_executor();
                boost::asio::dispatch(executor,
                    [complete = std::move(complete), result = std::move(result)]() mutable {
                        complete(std::move(result));
                    });
                return;
            }
            complete(std::move(result));
        }
    };

public:
    Callback(muduo::net::EventLoop& loop, Submit submit, T rejected)
        : loop_(loop), submit_(std::move(submit)), rejected_(std::move(rejected)) {}

    void initiate(Handler handler) override
    {
        loop_.assertInLoopThread();
        const auto state = std::make_shared<Completion>(loop_, std::move(handler));
        completion_ = state; // 发命令之前准备完整状态，容许拒绝、完成和停止重入。
        const std::weak_ptr<Completion> weak = state;
        auto slot = state->handler->get_cancellation_slot();
        if (slot.is_connected())
            slot.assign([weak](boost::asio::cancellation_type) {
                if (const auto state = weak.lock()) state->Finish(std::nullopt);
            });
        const bool accepted = submit_(std::function<void(T)>([weak](T result) {
            if (const auto state = weak.lock()) state->Finish(std::move(result));
        }));
        if (!accepted) state->Finish(std::move(rejected_));
        state->submitting = false;
    }

private:
    muduo::net::EventLoop& loop_;
    Submit submit_;
    T rejected_;
    std::shared_ptr<Completion> completion_;
};

template<class T, class Submit>
auto AwaitCallback(muduo::net::EventLoop& loop, Submit submit, T rejected)
{
    return Callback<T, Submit>(loop, std::move(submit), std::move(rejected));
}
}

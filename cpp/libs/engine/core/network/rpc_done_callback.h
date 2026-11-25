#pragma once

// 服务端延后完成的凭证(muduo 原版服务端 `done` / muduo-protorpc 的 RpcDoneCallback,docs/design/muduo-rpc-channel.md §5)。
//
// 框架派发每个 REQUEST 时都给 handler 一个非空的 `::google::protobuf::Closure* done`:
//   * 不碰 done(既有 handler 的写法):handler 返回后框架立即回包 —— 默认同步语义不变;
//   * 同步调 done->Run():立即回包,框架随后不再回;
//   * 要等下游(A→B→C 里的 B):在 handler 返回前 `const auto reply = RpcDoneCallback::Adopt(done);`,
//     把 reply 按值捕获进下游的回调,下游完成后 B 继续自己的业务、填 `reply.response<T>()`,再 `reply.Run()`。
//     等待期间 handler 早已返回,EventLoop 照常处理别的消息。
//
// 契约:
//   * done 指针本身只在本次 CallMethod 内有效;要在返回之后完成,必须先 Adopt —— 存下裸 done 等于悬空指针。
//   * 可拷贝,所有副本共享同一个完成标记:只有第一次 Run / Fail 生效,之后的调用被吞掉并计数。
//   * 最后一个副本析构时仍未完成:回 RPC_ERROR(REPLY_DROPPED),调用方立即以 kReplyDropped 失败,
//     不必等到超时。所以 Adopt 之后的每条分支(包括下游失败、下游根本没发出去)都必须 Run 或 Fail。
//   * 只持有 weak_ptr<RpcChannel>(channel 每条连接一个):请求进来的那条连接断了,Run 静默丢弃 ——
//     迟到的回复不可能落到重连后的新连接上。绝不持有强 TcpConnectionPtr / RpcChannelPtr(AGENTS §11.7)。
//   * 只在 EventLoop 线程调用。
//   * 不提供次序保证:接管之后,同一会话后续的同步请求会先回包;需要串行请用业务在途闸。
//   * 单向请求(id == 0)也能 Adopt,但 Run 不发任何东西,析构也不发 REPLY_DROPPED。

#include <cstdint>
#include <memory>

#include <google/protobuf/descriptor.h>
#include <google/protobuf/message.h>
#include <google/protobuf/service.h>

class RpcDoneCallback
{
public:
    // 只能在 handler 的同步栈内、对框架交给本次派发的 done 调用;nullptr 或别的 Closure → LOG_FATAL(fail-fast,
    // 与 RpcController::From 同理:Release 下也必须立刻暴露)。对同一个 done 多次 Adopt 得到同一状态的副本。
    [[nodiscard]] static RpcDoneCallback Adopt(::google::protobuf::Closure* done);

    // 本次调用的响应对象(框架持有,完成前有效)。Response 与方法的响应类型不符 → LOG_FATAL。
    template <typename Response>
    Response& response() const
    {
        return static_cast<Response&>(ResponseMessage(*Response::descriptor()));
    }

    // 回 RESPONSE(muduo doneCallback 语义)。
    void Run() const;
    // 回 RPC_ERROR(HANDLER_FAILED):handler 已运行并显式失败。业务失败优先写响应里的 error_message 再 Run。
    void Fail() const;

    // 请求所在的连接是否还在(A 已断开时 B 可以提前止损,不必再调下游)。
    bool IsCallerConnected() const;
    bool IsCompleted() const;

    struct State;

private:
    explicit RpcDoneCallback(std::shared_ptr<State> state) : state(std::move(state)) {}
    ::google::protobuf::Message& ResponseMessage(const ::google::protobuf::Descriptor& expected) const;

    std::shared_ptr<State> state;
};

#pragma once

// 生成的 gRPC 服务端骨架与 muduo_grpc 之间的 protobuf 胶水(docs/design/grpc-muduo-transport.md §15.4.3、§15.8)。
//
// 一次入站 unary 调用的流程(生成的 RegisterMethods 闭包驱动):
//   Create(ServerCall, Handle<M>) → ParseRequest → <M>()(守护段 grpc.<M>.guard)→ Proceed / Reject。
//   * Proceed:ServerCall::TryStartBusiness 为真时才调用 handler(即同步的 Handle<M>),随后序列化并 FinishOk。
//     调用已被截止 / 对端取消 / 排空收口时什么都不做;重复调用没有副作用。
//   * Reject:不进 Handle*,以给定的非 OK 状态完成调用(守护段的同步拒绝、许可拒绝)。
//   * 恰好一次由 ServerCall 裁决:第一个完成源生效,之后的 Finish 一律被吞掉(计 late_finish)。
// 所有权:生成代码与守护段的异步回调持有 shared_ptr<GrpcUnaryCall>;本对象只经 ServerCall 弱引用服务端,
//   绝不持有连接或 Server 的强引用(AGENTS §11.7)。异步续跑只捕获 shared_ptr<GrpcUnaryCall> 与 weak 引用,
//   不捕获生成的 <Svc>Impl 的 this。
// 线程:只在 Server 所属的 EventLoop 线程使用(ServerCall 对非只读操作断言 loop 线程);不创建线程,不加锁。
// 没有任何 T* 字段(Directory.Build.targets 的 no-raw-pointer-member 检查,§15.2 #13):handler 用 std::function,
//   生成代码传入静态成员函数指针(&Handle<M>)或无捕获 lambda 都能构造。
//
// 刻意放在全局命名空间:生成的头文件直接写 GrpcUnaryCall<Req, Resp>(§15.8.1)。

#include <cstddef>
#include <functional>
#include <limits>
#include <memory>
#include <string>
#include <string_view>
#include <utility>

#include "muduo_grpc/server_call.h"
#include "muduo_grpc/status.h"

#include "core/tracing/player_field.h"
#include "thread_context/trace_context_tls.h"

// muduo 头放在标准库头之后(它的 Windows 适配头 #define 了 off_t / pid_t 等名字;server_call.h 还会带进 <chrono> /
// <optional> / <thread>)。
#include "muduo/base/Logging.h"

template <typename Request, typename Response>
class GrpcUnaryCall final
{
    struct PrivateTag
    {
        explicit PrivateTag() = default;
    };

public:
    // 不用函数指针字段(§15.2 #13)。参数与生成的 Handle<M> 签名一致;无应答体的方法由生成代码包一层 lambda。
    using Handler = std::function<void(const Request *request, Response *response)>;

    static std::shared_ptr<GrpcUnaryCall> Create(muduo_grpc::ServerCall call, Handler handler)
    {
        return std::make_shared<GrpcUnaryCall>(PrivateTag{}, std::move(call), std::move(handler));
    }

    // 只给 Create 用(PrivateTag 挡住外部直接构造):对象必须由 shared_ptr 持有,守护段的异步回调才能延长它的寿命。
    GrpcUnaryCall(PrivateTag, muduo_grpc::ServerCall call, Handler handler)
        : call_(std::move(call)), handler_(std::move(handler))
    {
    }

    GrpcUnaryCall(const GrpcUnaryCall &) = delete;
    GrpcUnaryCall &operator=(const GrpcUnaryCall &) = delete;
    GrpcUnaryCall(GrpcUnaryCall &&) = delete;
    GrpcUnaryCall &operator=(GrpcUnaryCall &&) = delete;

    // 解析请求体。失败时已以 INTERNAL "grpc: error unmarshalling request" 完成调用(与 grpc-go 同文案前缀),
    // 调用方直接丢弃本对象即可。bytes 只在本次调用内有效,解析结果拷进 request_。
    bool ParseRequest(std::string_view bytes)
    {
        // 会话层已把单条消息限制在 4MiB 以内,这里的 int 上限只是防御 ParseFromArray 的参数窄化。
        if (bytes.size() > static_cast<std::size_t>(std::numeric_limits<int>::max()) ||
            !request_.ParseFromArray(bytes.data(), static_cast<int>(bytes.size())))
        {
            call_.FinishError(
                muduo_grpc::Status(muduo_grpc::StatusCode::kInternal, "grpc: error unmarshalling request"));
            return false;
        }
        return true;
    }

    const Request &request() const { return request_; }
    const muduo_grpc::ServerCall &server_call() const { return call_; }
    bool IsActive() const { return call_.IsActive(); }
    bool ServerDraining() const { return call_.ServerDraining(); }

    // 不进 Handle*,以 status 完成调用。status 必须非 OK:OK 视为编程错误,由 ServerCall::FinishError
    // 改回 INTERNAL 并 LOG_ERROR。调用已完成(截止 / 取消 / 排空)时被吞掉。
    void Reject(muduo_grpc::Status status) { call_.FinishError(std::move(status)); }

    // TryStartBusiness 为真时:handler_(&request_, &response_) → SerializeToString → FinishOk
    // (序列化失败回 INTERNAL "grpc: error while marshaling";应答超过 maxSendMessageBytes 由 FinishOk 改回 RESOURCE_EXHAUSTED)。
    // 调用已被截止 / 取消 / 排空收口时什么都不做。重复调用没有副作用:业务最多执行一次。
    void Proceed()
    {
        if (businessStarted_)
        {
            return;
        }
        if (!handler_)
        {
            // 生成代码总会传入 handler;走到这里是装配缺陷。业务没有执行,所以回「未执行」的 INTERNAL。
            LOG_ERROR << "gRPC unary call has no handler, method=" << call_.method();
            call_.FinishError(muduo_grpc::Status(muduo_grpc::StatusCode::kInternal, "grpc: no handler for method"));
            return;
        }
        // 判定「有效且未到期」与标记「业务已开始」在 ServerCall 里一次完成(§15.7 第 4 条)。
        if (!call_.TryStartBusiness())
        {
            return;
        }
        // 先置位再调业务:handler 里即使再次走到 Proceed(重入)也不会执行第二次。
        businessStarted_ = true;
        {
            // 业务日志带上请求顶层的 player_id(docs/design/distributed-tracing.md §5.5),口径同 Go 的
            // tracing.PlayerFieldInterceptor;没有这个字段的请求装 0,不沿用执行线程上残留的玩家。
            // 只覆盖 handler 的同步部分;trace 由派发钩子(grpc_trace_hooks)按 traceparent 接续。
            const tracing::ScopedPlayerId player(tracing::TopLevelPlayerId(request_));
            handler_(&request_, &response_);
        }

        std::string responseBytes;
        if (!response_.SerializeToString(&responseBytes))
        {
            // proto3 下实际不会发生(§15.13);真发生时业务已执行,打 ERROR 留痕。
            LOG_ERROR << "gRPC response serialization failed after the handler ran, method=" << call_.method();
            call_.FinishError(muduo_grpc::Status(muduo_grpc::StatusCode::kInternal, "grpc: error while marshaling"));
            return;
        }
        call_.FinishOk(std::move(responseBytes));
    }

private:
    muduo_grpc::ServerCall call_;
    Handler handler_;
    Request request_;
    Response response_;
    // 本对象是否已经把调用交给业务;保证 Proceed 重复调用时业务最多执行一次(不依赖 ServerCall 的记录状态)。
    bool businessStarted_{false};
};

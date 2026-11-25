#pragma once

// 一次入站 unary 调用的完成句柄(docs/design/grpc-muduo-transport.md §15.4.3、§15.7)。
//
// 形状照 RpcDoneCallback(docs/design/muduo-rpc-channel.md §5):
//   * 可拷贝:所有副本共享一个 CallToken。token 只持 weak_ptr<Server> + callId + loop 线程 id + 只读信息,
//     绝不持有 TcpConnectionPtr,也不持有 Server 的强引用(AGENTS §11.7)。所以句柄可以按值捕获进
//     Agones 许可回调、定时器、queueInLoop 闭包;服务端先没了,句柄的操作一律静默失效、不崩。
//   * 除只读访问(id / method / peer / metadata / deadline)外,只能在 Server 所属 loop 线程调用(断言:LOG_FATAL)。
//   * 恰好一次:第一个完成源生效(handler、截止、对端 RST / 断连、排空到期、句柄全部丢弃、服务端析构);
//     之后的 Finish 一律吞掉,计入 late_finish。
//   * 最后一个副本析构时仍未完成:
//     - 在 loop 线程:回 UNAVAILABLE "server dropped the call without completing it"(业务可能已执行,
//       所以只能用「结果未知」码),计入 dropped,打限频 ERROR;
//     - 不在 loop 线程(与 loopThread 不符):只累加进程级原子计数 dropped_off_loop,绝不 lock Server
//       (lock 出来的临时强引用可能是最后一个,Server 会在错误的线程上析构)。这时无从判断调用是否已完成,
//       所以最后一个副本只要在 loop 线程之外析构就计一次(已完成的也计);正常路径上恒为 0。
//
// 本头文件不依赖 muduo:server_session_test 等只需要完整的 ServerCall 类型(构造 UnaryMethodHandler)。

#include <chrono>
#include <cstdint>
#include <memory>
#include <optional>
#include <string>
#include <thread>

#include "metadata.h"
#include "status.h"

namespace muduo_grpc
{

class Server;

namespace detail
{
struct CallToken;
} // namespace detail

class ServerCall
{
public:
    ServerCall() = default;

    // 句柄是否非空(默认构造的句柄为空;空句柄的 Finish* 只打 ERROR)。
    explicit operator bool() const { return token != nullptr; }

    // ---- 只读信息:任何线程都可调用,服务端销毁后仍然有效 ----
    uint64_t id() const;
    const std::string &method() const;   // :path
    const std::string &peer() const;     // 只用于日志
    const Metadata &metadata() const;    // 只含自定义键;-bin 已解掉线上那一层
    std::optional<std::chrono::steady_clock::time_point> deadline() const;

    // ---- 以下只在 loop 线程 ----
    // 调用记录还在(还没被任何完成源收口)。
    bool IsActive() const;
    // 已 BeginShutdown,或服务端已销毁(空句柄也视为真)。
    bool ServerDraining() const;
    // 业务开始前调用(只给 GrpcUnaryCall::Proceed 用)。
    //   记录不在 → false;
    //   已到期(steady 复核)→ 以 DEADLINE_EXCEEDED 完成后返回 false;
    //   已经开始过一次 → false(不让业务执行两次;§15.4.3 草稿之外的补充);
    //   否则标记「业务已开始」,返回 true。
    // 不检查排空:排空期间已接受的调用照常处理。
    bool TryStartBusiness() const;
    // 超过 ServerOptions::maxSendMessageBytes → 改回 RESOURCE_EXHAUSTED 并打 ERROR。
    void FinishOk(std::string responseBytes) const;
    // status.ok() 视为编程错误 → 回 INTERNAL 并打 ERROR。
    void FinishError(Status status) const;

private:
    friend class Server;
    explicit ServerCall(std::shared_ptr<detail::CallToken> token);

    std::shared_ptr<detail::CallToken> token;
};

namespace detail
{

// 句柄共享的状态。没有任何 T* 字段(Directory.Build.targets 的 no-raw-pointer-member 检查)。
// 由 Server 在派发时创建,字段在创建后只读。
struct CallToken
{
    std::weak_ptr<Server> server;
    uint64_t callId{0};
    std::thread::id loopThread;
    std::string method;
    std::string peer;
    Metadata metadata;
    std::optional<std::chrono::steady_clock::time_point> deadline;

    CallToken() = default;
    CallToken(const CallToken &) = delete;
    CallToken &operator=(const CallToken &) = delete;
    ~CallToken();   // 规则见文件头「最后一个副本析构时仍未完成」;实现在 server.cpp
};

} // namespace detail

} // namespace muduo_grpc

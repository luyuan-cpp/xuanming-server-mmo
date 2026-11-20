#pragma once

#include <cstdint>
#include <string>

#include <google/protobuf/service.h>
#include "muduo/base/Logging.h"
#include "muduo/net/TcpConnection.h"
#include "core/type_define/type_define.h"   // 本头已不用;保留以免依赖它间接带入 SessionId 的翻译单元断编

// 单次 RPC 调用的上下文对象 —— 语义对应 Go 的 ctx。
//
// 名字沿用 protobuf 的概念名 RpcController(与基类 ::google::protobuf::RpcController 同名,
// 这是有意的:它就是那个槽位的实现,brpc::Controller 也是同样的做法)。
// 因此全仓**不得** `using namespace google::protobuf`;需要基类时一律全限定写
// ::google::protobuf::RpcController(生成代码本来就是这么写的)。
//
// 生命周期:在 RpcChannel::CallServiceMethod 的栈上按次构造,Service::CallMethod 返回即析构。
// 随调用链**显式传递**(生成代码原样转发 controller;手写 handler 用 From() 收参),
// 不落任何 thread_local,不允许拷贝或存进长生命周期结构。
// 要在 handler 返回之后才完成本次调用(A→B→C 里的 B),用的不是 controller,而是 done:
// RpcDoneCallback::Adopt(done)(network/rpc_done_callback.h)。
//
// 失败:handler 同步返回前调 SetFailed,框架回 RPC_ERROR(HANDLER_FAILED),调用方得到 kRemoteFailed。
// 业务失败照旧写进响应的 error_message;SetFailed 只用于「这次调用本身不该被服务」(例如已废弃的方法)。
//
// 它取代了原先的 thread_local RpcRequestContext tlsRpc —— 其 conn 字段是一个跨派发存活、
// 从不清空的 TcpConnectionPtr 强引用,会把 muduo TcpClient::~TcpClient 的
// `connection_.use_count() == 1` 唯一性判断带偏 → forceClose 被跳过 → 连接在 kConnected
// 态被析构(TcpConnection.cc:71 assert;Release 下是 poller 里的悬垂 Channel*,use-after-free)。
// 事故记录:docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md
//
// muduo 规范同样成立:TcpConnectionPtr 只在回调期间持有;需要跨回调"记住"连接的地方
// 应当持 weak_ptr + lock()。另一条必须遵守的规则:**不要在一条消息的回调里同步释放该连接所属
// RpcClient 的最后一个引用**——回调期间 muduo 自身与本对象各持一份强引用,~TcpClient 必然看到
// use_count>1 而跳过 forceClose;节点摘除必须像现在一样经 queueInLoop 延后(node.cpp DestroyEntity 路径)。
// (原先的会话 / 下一跳路由字段全仓无读写者,随 RoutePlayerMessage 一族的退役一并删除。)
class RpcController final : public ::google::protobuf::RpcController
{
public:
    explicit RpcController(const muduo::net::TcpConnectionPtr& conn) : RpcController(conn, NoHandshakePeer()) {}
    // handshakePeerUuid 引用的是派发方 RpcChannel 上的握手身份,channel 活过本次派发(RpcChannel::CallServiceMethod)。
    RpcController(const muduo::net::TcpConnectionPtr& conn, const std::string& handshakePeerUuid)
        : conn_(conn), handshakePeerUuid_(handshakePeerUuid) {}

    RpcController(const RpcController&) = delete;
    RpcController& operator=(const RpcController&) = delete;

    // --- 本次调用所在的连接。仅在本次派发内有效,不要存起来 ---
    const muduo::net::TcpConnectionPtr& conn() const { return conn_; }

    // --- 本次调用来自哪个进程:入站连接握手时认下的对端 node_uuid(docs/design/muduo-rpc-channel.md §8.3)---
    // 空 = 这条连接没有握手成功(出站连接上的调用、测试直接构造的 ctx)。需要按对端进程认身份的 handler 用它,
    // 不用消息体自报的实例号(scene 按 (gate 实例, session_id) 认会话,docs/design/scene-enter-chain-residuals.md §14)。
    const std::string& HandshakePeerUuid() const { return handshakePeerUuid_; }

    // 手写 handler 从生成层拿到的是基类指针;全仓唯一的 CallMethod 派发点
    // (RpcChannel::CallServiceMethod)构造的就是本类型,这里集中做一次受检下转。
    // 对 nullptr / 异类 controller 一律 LOG_FATAL(而不是 assert):Release 下 NDEBUG 会
    // 抹掉 assert,若将来出现第二个派发点忘了传本类型,必须在这里立刻暴露,而不是
    // 解引用空指针崩在别处。这是有意的 fail-fast。
    static const RpcController& From(::google::protobuf::RpcController* controller)
    {
        auto* self = dynamic_cast<RpcController*>(controller);   // nullptr 输入也返回 nullptr
        if (self == nullptr)
        {
            LOG_FATAL << "CallMethod dispatched without an RpcController (controller="
                      << static_cast<const void*>(controller) << ")";
        }
        return *self;
    }

    // --- ::google::protobuf::RpcController 接口:只用 Failed / ErrorText / SetFailed;取消语义不支持 ---
    void Reset() override { failed_ = false; errorText_.clear(); }
    bool Failed() const override { return failed_; }
    std::string ErrorText() const override { return errorText_; }
    void StartCancel() override {}
    void SetFailed(const std::string& reason) override { failed_ = true; errorText_ = reason; }
    bool IsCanceled() const override { return false; }
    void NotifyOnCancel(::google::protobuf::Closure* /*callback*/) override {}

private:
    static const std::string& NoHandshakePeer()
    {
        static const std::string kNone;
        return kNone;
    }

    muduo::net::TcpConnectionPtr conn_;
    const std::string& handshakePeerUuid_;
    bool failed_{ false };
    std::string errorText_;
};

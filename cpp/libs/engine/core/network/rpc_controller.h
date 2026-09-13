#pragma once

#include <cstdint>
#include <string>

#include <google/protobuf/service.h>
#include "muduo/base/Logging.h"
#include "muduo/net/TcpConnection.h"
#include "core/type_define/type_define.h"

// 单次 RPC 调用的上下文对象 —— 语义对应 Go 的 ctx。
//
// 名字沿用 protobuf 的概念名 RpcController(与基类 ::google::protobuf::RpcController 同名,
// 这是有意的:它就是那个槽位的实现,brpc::Controller 也是同样的做法)。
// 因此全仓**不得** `using namespace google::protobuf`;需要基类时一律全限定写
// ::google::protobuf::RpcController(生成代码本来就是这么写的)。
//
// 生命周期:在 GameChannel::ProcessMessage 的栈上按次构造,Service::CallMethod 返回即析构。
// 随调用链**显式传递**(生成代码原样转发 controller;手写 handler 用 From() 收参),
// 不落任何 thread_local,不允许拷贝或存进长生命周期结构。
//
// 它取代了原先的 thread_local RpcRequestContext tlsRpc —— 其 conn 字段是一个跨派发存活、
// 从不清空的 TcpConnectionPtr 强引用,会把 muduo TcpClient::~TcpClient 的
// `connection_.use_count() == 1` 唯一性判断带偏 → forceClose 被跳过 → 连接在 kConnected
// 态被析构(TcpConnection.cc:71 assert;Release 下是 poller 里的悬垂 Channel*,use-after-free)。
// 事故记录:docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md
//
// muduo 规范同样成立:TcpConnectionPtr 只在回调期间持有;需要跨回调"记住"连接的地方
// 应当持 weak_ptr + lock()。同类的遗留(均已登记在事故文档 §7):
//   * registration_manager.cpp 握手重试定时器按值捕获出站 conn + RpcClientPtr —— 与本次
//     同形的 B 类隐患,目前只靠 entt 池销毁顺序掩盖;
//   * GameChannel::connection_ 对出站连接是强引用且 DOWN 时不清 —— 只靠 RpcClient 的
//     成员声明顺序(client_ 在 channel_ 之前)保证先释放;
//   * RpcSession::connection 恒为 TcpServer 侧入站连接(其 ctor 从 conn 的 context 取
//     GameChannelPtr,只有 RpcServer 会设置),因此对本 assert 免疫,属滞留而非崩溃。
// 另一条必须遵守的规则:**不要在一条消息的回调里同步释放该连接所属 RpcClient 的最后一个
// 引用**——回调期间 muduo 自身与本对象各持一份强引用,~TcpClient 必然看到 use_count>1 而
// 跳过 forceClose;节点摘除必须像现在一样经 queueInLoop 延后(node.cpp DestroyEntity 路径)。
class RpcController final : public ::google::protobuf::RpcController
{
public:
    explicit RpcController(const muduo::net::TcpConnectionPtr& conn) : conn_(conn) {}

    RpcController(const RpcController&) = delete;
    RpcController& operator=(const RpcController&) = delete;

    // --- 本次调用所在的连接。仅在本次派发内有效,不要存起来 ---
    const muduo::net::TcpConnectionPtr& conn() const { return conn_; }

    // --- 会话 / 下一跳路由:从旧 RpcRequestContext 折进来的 per-call 字段 ---
    // (旧的 routeData_ / routeMsgBody_ 全仓无读者且是重量级 proto 对象,不再携带;
    //  需要时在此处加,不要再开 thread_local。)
    void SetCurrentSessionId(SessionId sessionId) { currentSessionId_ = sessionId; }
    SessionId GetSessionId() const { return currentSessionId_; }

    void SetNextRouteNodeType(uint32_t nodeType) { nextRouteNodeType_ = nodeType; }
    uint32_t GetNextRouteNodeType() const { return nextRouteNodeType_; }

    void SetNextRouteNodeId(uint32_t nodeId) { nextRouteNodeId_ = nodeId; }
    uint32_t GetNextRouteNodeId() const { return nextRouteNodeId_; }

    // 手写 handler 从生成层拿到的是基类指针;全仓唯一的 CallMethod 派发点
    // (GameChannel::ProcessMessage)构造的就是本类型,这里集中做一次受检下转。
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

    // --- ::google::protobuf::RpcController 纯虚接口:本框架不用这些语义,给出最小实现 ---
    void Reset() override { failed_ = false; errorText_.clear(); }
    bool Failed() const override { return failed_; }
    std::string ErrorText() const override { return errorText_; }
    void StartCancel() override {}
    void SetFailed(const std::string& reason) override { failed_ = true; errorText_ = reason; }
    bool IsCanceled() const override { return false; }
    void NotifyOnCancel(::google::protobuf::Closure* /*callback*/) override {}

private:
    muduo::net::TcpConnectionPtr conn_;
    SessionId currentSessionId_{ kInvalidSessionId };
    uint32_t nextRouteNodeType_{ UINT32_MAX };
    uint32_t nextRouteNodeId_{ UINT32_MAX };
    bool failed_{ false };
    std::string errorText_;
};

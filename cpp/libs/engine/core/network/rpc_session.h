#pragma once

#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <utility>

#include <boost/any.hpp>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/TcpConnection.h"
#include "network/rpc_channel.h"
#include "network/rpc_channel_stats.h"

// 入站节点链路在本端的句柄:握手成功后挂在对端节点实体上(registration_manager tryRegister)。
// 通过它反向调用对端(scene → gate),走的是该入站连接的 RpcChannel。
//
// 只存 weak(AGENTS §11.7):channel 归连接所有(RpcServer 存进 conn 的 context),连接 DOWN 后
// channel 随 context 清空而析构,这里 lock 失败即视为「对端不可达」。RpcServer 的 DOWN 分支会顺手把本组件摘掉
// (NodeUtils::RemoveRpcSessionsBoundTo),那只是记录卫生,不是释放的前提。
//
// conn 必须非空,否则构造即 LOG_FATAL(fail-fast,见 LoopOf)。
class RpcSession
{
public:
    explicit RpcSession(const muduo::net::TcpConnectionPtr& conn,
        const std::string& uuid)
        : connection(conn),
          node_uuid(uuid),
          loop(LoopOf(conn, uuid))
    {
        // 只有 RpcServer 会在 context 里放 RpcChannelPtr;别的连接(或 context 为空)时 channel 为空,调用一律 kNotSent。
        if (const auto* channel = boost::any_cast<RpcChannelPtr>(&conn->getContext()); channel != nullptr)
        {
            this->channel = *channel;
        }
    }

    [[nodiscard]] bool IsConnected() const
    {
        const auto channel = this->channel.lock();
        return channel && channel->IsConnected();
    }

    // 需要应答的调用,语义见 RpcChannel::CallMethod:done 恰好一次、永不同步执行。
    void CallMethod(uint32_t messageId, const ::google::protobuf::Message& request, ClientDoneCallback done,
                    const RpcCallOptions& options = {}) const
    {
        if (const auto channel = this->channel.lock())
        {
            channel->CallMethod(messageId, request, std::move(done), options);
            return;
        }
        FailNotConnected(messageId, std::move(done));
    }

    template <typename Method, typename Done>
    void CallMethod(const typename Method::Request& request, Done&& done, const RpcCallOptions& options = {}) const
    {
        if (const auto channel = this->channel.lock())
        {
            channel->CallMethod<Method>(request, std::forward<Done>(done), options);
            return;
        }
        FailNotConnected(Method::kMessageId,
                         [typedDone = std::forward<Done>(done)](const RpcStatus& status, const MessagePtr&) mutable
                         {
                             typedDone(status, std::shared_ptr<typename Method::Response>{});
                         });
    }

    // 单向(调用方不要应答)。channel 已不在时返回 false 并计数。
    bool SendRequest(uint32_t messageId, const ::google::protobuf::Message& message) const
    {
        if (const auto channel = this->channel.lock())
        {
            return channel->SendRequest(messageId, message);
        }
        const auto n = GetRpcChannelStats().onewayRejected.fetch_add(1, std::memory_order_relaxed) + 1;
        if (ShouldLogRpcEvent(n))
        {
            LOG_WARN << "[RpcChannel] one-way request dropped: reason=session channel gone, message_id=" << messageId
                     << ", node_uuid=" << node_uuid << ", occurrences=" << n;
        }
        return false;
    }

    template <typename Method>
    bool SendRequest(const typename Method::Request& request) const
    {
        return SendRequest(Method::kMessageId, request);
    }

    // 节点关机阶段 0(NodeUtils::CloseAllRpcChannels)。
    void CloseChannel() const
    {
        if (const auto channel = this->channel.lock())
        {
            channel->Close();
        }
    }

    // weak_ptr:应用层不拥有入站连接(§8 连接生命周期约定)。RemoveRpcSessionsBoundTo 按它精确匹配。
    std::weak_ptr<muduo::net::TcpConnection> connection;

private:
    // 空连接 = 调用方的 bug(唯一生产构造点 registration_manager tryRegister 在入口已判空),构造即 LOG_FATAL。
    // 不再容忍:旧实现在连接为空时把 loop_ 留空,之后 channel 不在时 CallMethod 没有 loop 可投递 kNotSent,
    // done 被就地丢弃 —— 违反 RpcChannel::CallMethod「done 恰好一次、永不同步」的契约,调用方的失败路径永远不跑。
    static muduo::net::EventLoop& LoopOf(const muduo::net::TcpConnectionPtr& conn, const std::string& uuid)
    {
        if (!conn)
        {
            LOG_FATAL << "[RpcChannel] RpcSession requires a non-null connection: node_uuid=" << uuid;
        }
        return *conn->getLoop();
    }

    // loop_ 自构造起必有值,所以本地失败一律经 loop 以 kNotSent 投递(恰好一次、不同步执行)。
    void FailNotConnected(uint32_t messageId, ClientDoneCallback done) const
    {
        RpcChannel::FailNotSent(loop.get(), messageId, std::move(done), "session channel gone", node_uuid);
    }

    std::weak_ptr<RpcChannel> channel;
    std::string node_uuid;
    // 连接所在的 loop:channel 已不在时用它投递 kNotSent。引用不是指针(no-raw-pointer-member);
    // reference_wrapper 可拷贝 / 移动赋值,entt 存储删除时的 swap-and-pop(std::exchange 移动赋值)照常可用。
    std::reference_wrapper<muduo::net::EventLoop> loop;
};

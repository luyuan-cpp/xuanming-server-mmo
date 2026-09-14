#pragma once

#include <memory>
#include <string>
#include "muduo/base/Logging.h"
#include "muduo/net/TcpConnection.h"
#include "network/game_channel.h"

class RpcSession
{
public:
    explicit RpcSession(const muduo::net::TcpConnectionPtr& conn,
        const std::string& uuid)
		: connection(conn), 
          node_uuid(uuid),
          channel_(boost::any_cast<GameChannelPtr>(conn->getContext())) {}

    [[nodiscard]] bool IsConnected() const
    {
        const auto conn = connection.lock();
        return conn && conn->connected();
    }

    void CallRemoteMethod(uint32_t messageId, const ::google::protobuf::Message& request) const
    {
        if (!IsConnected()) {
            LOG_ERROR << "Connection is not active. Cannot call remote method.";
            return;
        }
        channel_->CallRemoteMethod(messageId, request);
    }

    void SendRequest(uint32_t messageId, const ::google::protobuf::Message& message) const
    {
        if (!IsConnected()) {
            LOG_ERROR << "Connection is not active. Cannot send request.";
            return;
        }
        channel_->SendRequest(messageId, message);
    }

    void RouteMessageToNode(uint32_t messageId, const ::google::protobuf::Message& message) const
    {
        if (!IsConnected()) {
            LOG_ERROR << "Connection is not active. Cannot route message.";
            return;
        }
        channel_->RouteMessageToNode(messageId, message);
    }

    void SendRouteResponse(uint32_t messageId, uint64_t id, const std::string& messageBytes) const
    {
        if (!IsConnected()) {
            LOG_ERROR << "Connection is not active. Cannot send route response.";
            return;
        }
        channel_->SendRouteResponse(messageId, id, messageBytes);
    }

    // weak_ptr:应用层不拥有入站连接(§8 连接生命周期约定)。对端断开后这个组件不再钉住
    // 死连接和它的 fd;RpcServer 的 DOWN 分支会顺手把组件摘掉,那只是记录卫生,不是释放的前提。
    std::weak_ptr<muduo::net::TcpConnection> connection;

private:
    GameChannelPtr channel_;
	std::string node_uuid;
	bool handshaked = false;
};




#pragma once

#include "core/network/codec/codec.h"
#include "muduo/net/EventLoop.h"
#include "network/rpc_client.h"
#include "network/codec/dispatcher.h"
#include "engine/core/type_define/type_define.h"
#include "proto/common/base/message.pb.h"

using RpcClientMessagePtr = std::shared_ptr<ClientRequest>;
using ClientTokenVerifyRequestPtr = std::shared_ptr<ClientTokenVerifyRequest>;

struct SessionInfo;
class OnNodeRemoveEvent;

class RpcClientSessionHandler : muduo::noncopyable
{
public:
    RpcClientSessionHandler(ProtobufCodec &codec, ProtobufDispatcher &dispatcher);

    ProtobufCodec &codec() const { return protobufCodec; }

    void OnConnection(const muduo::net::TcpConnectionPtr &conn);

    void SendMessageToClient(const muduo::net::TcpConnectionPtr &conn, const ::google::protobuf::Message &message) const;

    void DispatchClientRpcMessage(const muduo::net::TcpConnectionPtr &conn,
                                  const RpcClientMessagePtr &message,
                                  muduo::Timestamp);

    static SessionId GetSessionId(const muduo::net::TcpConnectionPtr &conn);

    static void SendTipToClient(const muduo::net::TcpConnectionPtr &conn, uint32_t tip_id);

    bool CheckMessageSize(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const;

    bool CheckMessageLimit(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const;

    bool ValidateClientMessage(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const;

    void DispatchTokenVerify(const muduo::net::TcpConnectionPtr &conn,
                             const ClientTokenVerifyRequestPtr &message,
                             muduo::Timestamp);

    void OnNodeRemoveEventHandler(const OnNodeRemoveEvent &pb);

private:
    static void HandleConnectionDisconnection(const muduo::net::TcpConnectionPtr &conn);

    static void HandleConnectionEstablished(const muduo::net::TcpConnectionPtr &conn);

    ProtobufCodec &protobufCodec;
    ProtobufDispatcher &messageDispatcher;
};

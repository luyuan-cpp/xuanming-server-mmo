#pragma once

#include "core/network/codec/codec.h"
#include "muduo/net/EventLoop.h"
#include "network/rpc_client.h"
#include "network/codec/dispatcher.h"
#include "engine/core/type_define/type_define.h"
#include "proto/common/base/message.pb.h"
#include "proto/scene/player_lifecycle.pb.h"
#include "entt/src/entt/signal/sigh.hpp"

using RpcClientMessagePtr = std::shared_ptr<ClientRequest>;
using ClientTokenVerifyRequestPtr = std::shared_ptr<ClientTokenVerifyRequest>;

struct SessionInfo;
struct OnNodeRemoveEvent;

class GateClientSessionHandler : muduo::noncopyable
{
public:
    GateClientSessionHandler(ProtobufCodec &codec, ProtobufDispatcher &dispatcher);

    ProtobufCodec &codec() const { return protobufCodec; }

    void OnConnection(const muduo::net::TcpConnectionPtr &conn);

    void SendMessageToClient(const muduo::net::TcpConnectionPtr &conn, const ::google::protobuf::Message &message) const;

    void DispatchClientRpcMessage(const muduo::net::TcpConnectionPtr &conn,
                                  const RpcClientMessagePtr &message,
                                  muduo::Timestamp);

    static SessionId GetSessionId(const muduo::net::TcpConnectionPtr &conn);

    static void SendTipToClient(const muduo::net::TcpConnectionPtr &conn, uint32_t tip_id);

    // 向会话当前绑定的 scene 节点代发 ExitGame(单向,带 player_id 反向身份栅栏)。断线与客户端 LeaveGame
    // (gate_scene_entry::OnClientLeaveGame,docs/design/location-release-binding.md §3.5)共用这一份。
    // reason 决定 scene 是立即退出还是把角色留在原地等重连(docs/design/player-connection-lost-hold.md §3):
    // 断线填 DISCONNECTED(租约到期关假死连接填 LEASE_EXPIRED),LeaveGame 填 LOGOUT。
    // 返回是否交给了连接;没绑定节点 / 节点已摘除 / 没有 RpcClient 时返回 false。sceneNodeId 回填节点号(只供日志,拿不到为 0)。
    static bool NotifySceneExitGame(SessionId sessionId, const SessionInfo &session, ExitGameReason reason,
                                    uint32_t &sceneNodeId);

    bool CheckMessageSize(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const;

    bool CheckMessageLimit(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const;

    bool ValidateClientMessage(SessionInfo &session, const RpcClientMessagePtr &request, const muduo::net::TcpConnectionPtr &conn) const;

    void DispatchTokenVerify(const muduo::net::TcpConnectionPtr &conn,
                             const ClientTokenVerifyRequestPtr &message,
                             muduo::Timestamp);

    void OnNodeRemoveEventHandler(const OnNodeRemoveEvent &pb);

private:
    // handler 销毁即断开带 this 的订阅,dispatcher 的寿命可以长于 handler。
    entt::scoped_connection nodeRemovedSubscription_;
    static void HandleConnectionDisconnection(const muduo::net::TcpConnectionPtr &conn);

    static void HandleConnectionEstablished(const muduo::net::TcpConnectionPtr &conn);

    ProtobufCodec &protobufCodec;
    ProtobufDispatcher &messageDispatcher;
};

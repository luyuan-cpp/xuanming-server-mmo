#pragma once

#include "muduo/base/Mutex.h"
#include "muduo/net/protobuf/ProtobufCodecLite.h"
#include "network/codec/dispatcher.h"

#include <google/protobuf/service.h>
#include <map>
#include <memory>

#include "proto/common/base/rpc_message.pb.h"

using RpcMessagePtr = std::shared_ptr<GameRpcMessage>;

// Protocol tag
constexpr char RPC_TAG[] = "RPC0";

// Wire format:
// size      : 4-byte  (N+8) total message length
// RPC_TAG   : 4-byte  fixed tag "RPC0"
// payload   : N-byte  serialized Protobuf data
// checksum  : 4-byte  Adler-32 over "RPC0" + payload
using RpcCodec = muduo::net::ProtobufCodecLiteT<GameRpcMessage, RPC_TAG>;

using muduo::net::TcpConnectionPtr;
using ProtobufService = ::google::protobuf::Service;
using ProtobufMessage = ::google::protobuf::Message;

// Temporary debug: set gDumpOversizedRpc = true to dump full oversized RPC bodies to logs/oversized_rpc.log
extern bool gDumpOversizedRpc;
void EnableOversizedRpcDump(bool enable);

class GameChannel
{
public:
    GameChannel();
    explicit GameChannel(const TcpConnectionPtr &connection);

    ~GameChannel();

    void SetConnection(const TcpConnectionPtr &connection) { connection_ = connection; }

    void SetServiceMap(const std::map<std::string, ProtobufService *> *services) { services_ = services; }

    inline bool IsValidMessageId(uint32_t messageId) const;

    ProtobufDispatcher &GetDispatcher() { return dispatcher_; }

    void CallRemoteMethod(uint32_t messageId, const ProtobufMessage &request);

    // Fire-and-forget request (no response expected)
    void SendRequest(uint32_t messageId, const ProtobufMessage &message);

    void RouteMessageToNode(uint32_t messageId, const ProtobufMessage &request);

    void SendRouteResponse(uint32_t messageId, uint64_t routeId, const std::string &responseData);

    void HandleIncomingMessage(const TcpConnectionPtr &connection,
                               muduo::net::Buffer *buffer,
                               muduo::Timestamp receiveTime);

private:
    void SendErrorResponse(const GameRpcMessage &message, GameErrorCode errorCode);

    void HandleRpcMessage(const TcpConnectionPtr &connection, const RpcMessagePtr &message, muduo::Timestamp receiveTime);
    void HandleResponseMessage(const TcpConnectionPtr &connection, const GameRpcMessage &message, muduo::Timestamp receiveTime);
    void HandleRequestMessage(const TcpConnectionPtr &connection, const GameRpcMessage &message, muduo::Timestamp receiveTime);
    void HandleNodeRouteMessage(const TcpConnectionPtr &connection, const GameRpcMessage &message, muduo::Timestamp receiveTime);
    void HandleClientRequestMessage(const TcpConnectionPtr &connection, const GameRpcMessage &message, muduo::Timestamp receiveTime);

    void SendRpcRequestMessage(GameMessageType type, uint32_t messageId, const ProtobufMessage *content);
    void SendRpcResponseMessage(GameMessageType type, uint32_t messageId, const ProtobufMessage *content);

    void ProcessMessage(const TcpConnectionPtr &conn, const GameRpcMessage &rpcMessage, muduo::Timestamp receiveTime);

    bool SerializeMessage(const ProtobufMessage &message, std::string *output) const;

    void LogMessageStatistics(const GameRpcMessage &message, uint32_t cachedSize) const;

    void StartMessageStatistics();

    void StopMessageStatistics();

    void SendGameRpcMessage(const GameRpcMessage &message);

    // 只在真正要发送时把连接锁成强引用;锁不住(连接已断、对象已被 muduo 释放)就丢弃并告警。
    TcpConnectionPtr LockConnection(const char *what) const;

    RpcCodec codec_;
    // 连接生命周期约定(docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §8):
    // 应用层不拥有 TcpConnection,只存 weak_ptr。这里改成 weak 之后:
    //  - 出站(RpcClient 持有的 channel):DOWN 后不再钉住死连接和它的 fd,不必等重连覆盖;
    //  - 入站(RpcServer 存进 conn context 的 channel):conn→context→channel→conn 的引用环不复存在,
    //    DOWN 分支清 context 只是卫生,不再是释放连接的前提;
    //  - 对 muduo TcpClient::~TcpClient 的 use_count()==1 判断永远不会造成干扰。
    std::weak_ptr<muduo::net::TcpConnection> connection_;
    const std::map<std::string, ProtobufService *> *services_ = nullptr;
    ProtobufDispatcher dispatcher_;
};

using GameChannelPtr = std::shared_ptr<GameChannel>;

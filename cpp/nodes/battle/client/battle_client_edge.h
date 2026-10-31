#pragma once

// battle 节点的客户端直连面(设计文档 turn-based-battle-server.md §18,D23-D28)。
//
// 职责:在节点自身的 TCP 端口(NodeInfo.endpoint,框架已分配并发布到 etcd)上接受
// 客户端第二条连接(集群外客户端按票据里的 NodeInfo.client_endpoint 经 hostPort / Agones
// 端口映射到达这个端口,集群外入口 D76/D81),完成票据握手,把战斗客户端消息就地派发给 BattleRoomManager,
// 并把 S2C 直接写回连接 —— 战斗流量从此零字节经过 gate。收缩后这是战斗的唯一通路
// (turn-based §22 D66/D68):gate 不再中继战斗上行,战斗帧也不再回落大厅。
//
// 线协议与 gate 的客户端面完全一致(ProtobufCodec 帧;上行 ClientRequest,下行
// MessageContent;握手首包 BattleTokenVerifyRequest ↔ BattleTokenVerifyResponse),
// 客户端两条连接可复用同一套编解码与分发。
//
// 线程模型:全部回调在 muduo 主 loop 线程(与 BattleRoomManager 同线程),无锁。
//
// 安全闸(D27,逐条镜像 gate 的 HandleConnectionEstablished / DispatchTokenVerify):
//   1. 并发连接上限 BaseDeployConfig.battle_max_connections(0 仅 dev/test 允许);
//   2. 空 battle_token_secret 的处置由 BATTLE_RUN_MODE 决定(prod 拒连);
//   3. 未验证连接 kHandshakeTimeoutSec 内没完成握手即关闭(防占槽);
//   4. 验证通过前丢弃并关闭一切非握手消息;
//   5. ClientRequest 体 ≤ kMaxClientRequestBytes;每消息号限速(MessageLimiter,与 gate
//      同一张表);消息号只放行 BattleClientPlayer 的四条客户端 RPC;非法包累计达阈值即关闭
//      (阈值与 gate 同源:IllegalPacketCounter,默认 50,GATE_ILLEGAL_PACKET_THRESHOLD 可调)。
//
// 与 gate 的关键差别:dev/test 空密钥只是**跳过签名比对**,握手包本身仍必须发 ——
// 直连没有别的途径知道这条连接属于哪场战斗、哪个玩家。

#include <cstdint>
#include <chrono>
#include <memory>
#include <optional>
#include <string>
#include <unordered_map>
#include <utility>

#include "muduo/base/noncopyable.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"

#include "message_limiter/message_limiter.h"
#include "network/codec/codec.h"
#include "network/codec/dispatcher.h"
#include "time/comp/timer_task_comp.h"

#include "proto/battle/player_battle.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/match/match_network.pb.h"

class BattleClientEdge : muduo::noncopyable, public std::enable_shared_from_this<BattleClientEdge>
{
public:
    // 未验证连接的握手期限。gate 没有这一道(靠非法包计数),直连面加上它:
    // 票据握手是客户端拿到 BattleAssignedS2C 后的第一件事,10s 远超正常 RTT。
    static constexpr double kHandshakeTimeoutSec = 10.0;
    // 与 gate CheckMessageSize 同口径:客户端合法业务消息不超过 1KB。
    static constexpr size_t kMaxClientRequestBytes = 1024;
    // battle_max_connections=0(dev/test)时的硬上限,防 fd 被吃光。
    static constexpr size_t kHardMaxConnections = 65535;

    BattleClientEdge();

    // 装到节点 TCP server 上(替换框架默认的 RpcCodec 回调)。必须在 StartRpcServer
    // 之后调用(Node::SetAfterStart 回调里),与 gate main.cpp 同一时机。
    void Install(muduo::net::TcpServer &server);

    // 下行:按 ProtobufCodec 帧写出(长度头 + 类型名 + 校验和)。
    // 绝不能裸 conn->send 序列化字节 —— 会毁掉整条连接的分帧(gate 同款教训)。
    // 走 static ProtobufCodec::fillEmptyBuffer 而不是 codec_.send:后者是非 const 成员,
    // 在 const 方法里对按值成员调用编不过(gate 能那样写是因为它持的是引用成员)。
    void Send(const muduo::net::TcpConnectionPtr &conn, const ::google::protobuf::Message &message) const;

    // 一份完整的下行帧(与 Send 写出的字节逐字节相同)。只能由 EncodeFrame 产出,SendEncoded 只收这个类型:
    // 裸序列化字节进不来(理由同 Send)。
    class EncodedFrame
    {
        friend class BattleClientEdge;
        explicit EncodedFrame(std::string bytes) : bytes_(std::move(bytes)) {}
        std::string bytes_;
    };

    // 编码一次、多条连接复用(观众帧,battle-spectate-tiered-fanout.md §10 第 3 条):同一帧发给多条直连时
    // 先 EncodeFrame 一次,再逐条 SendEncoded,序列化、分帧与校验和不再随连接数重复。
    // 前提是帧与收信连接无关;将来要按连接签名 / 加料,这条复用就作废。
    static EncodedFrame EncodeFrame(const ::google::protobuf::Message &message);
    // 经 send(StringPiece) 写出:loop 内就地写 socket,写不完的部分拷进本连接的输出缓冲,入参不被改动,
    // 同一份帧可以反复发。不能改成对共享 Buffer 调 send(Buffer*):它在 loop 内会 retrieveAll() 清空传入的
    // 缓冲,第一条连接发完,后面的连接拿到的就是空帧。
    void SendEncoded(const muduo::net::TcpConnectionPtr &conn, const EncodedFrame &frame) const;

    // 停机收尾:仍处于 connected 的直连 forceClose;已被房间管理器 shutdown() 的连接
    //(终局包还在排空)不再动它,由其自身的延迟强关收尾,保证"终局包先于 FIN"。
    void DisconnectAll(const char *reason);

    size_t ConnectionCount() const { return sessions_.size(); }

    // 并发连接的有效上限(闸 1 的判据,现读部署配置):battle_max_connections,为 0(仅 dev/test 合法)时取
    // kHardMaxConnections。「0 = 硬上限」这条规则只写在这里,节点观众席配额由它派生(BattleRoomManager::SetClientEdge)。
    size_t EffectiveMaxConnections() const;

private:
    struct DirectSession
    {
        uint64_t connId = 0;
        bool verified = false;
        uint64_t battleId = 0;
        uint64_t playerId = 0;
        ::eBattleTicketRole role = ::BATTLE_TICKET_ROLE_NONE;
        std::string spectatorOperation;
        // gate 会话 id(来自房间路由快照),回填进合成的 SessionDetails 供日志对齐
        uint32_t gateSessionId = 0;
        // 非法包累计(IllegalPacketCounter 管阈值);握手成功时清零
        uint32_t illegalPacketCount = 0;
        // 每消息号限速,与 gate SessionInfo 同款(表驱动,MessageLimiter.xlsx)
        MessageLimiter messageLimiter;
        TimerTaskComp handshakeTimer;
        std::weak_ptr<muduo::net::TcpConnection> conn;
        std::optional<match::NetworkProbeGrant> probeGrant;
        std::string probeNonce;
        uint32_t probeSamples = 0;
        uint32_t probeRttMs = 0;
        std::chrono::steady_clock::time_point probeSent;
    };

    void OnConnection(const muduo::net::TcpConnectionPtr &conn);
    void HandleEstablished(const muduo::net::TcpConnectionPtr &conn);
    void HandleClosed(const muduo::net::TcpConnectionPtr &conn);

    void OnTokenVerify(const muduo::net::TcpConnectionPtr &conn,
                       const std::shared_ptr<::BattleTokenVerifyRequest> &request,
                       muduo::Timestamp receiveTime);
    void OnProbeStart(const muduo::net::TcpConnectionPtr&, const std::shared_ptr<match::NetworkProbeStart>&, muduo::Timestamp);
    void OnProbeEcho(const muduo::net::TcpConnectionPtr&, const std::shared_ptr<match::NetworkProbeEcho>&, muduo::Timestamp);
    void SendProbeChallenge(DirectSession&, const muduo::net::TcpConnectionPtr&);
    void OnClientRequest(const muduo::net::TcpConnectionPtr &conn,
                         const std::shared_ptr<::ClientRequest> &request,
                         muduo::Timestamp receiveTime);
    void OnUnknownMessage(const muduo::net::TcpConnectionPtr &conn,
                          const std::shared_ptr<::google::protobuf::Message> &message,
                          muduo::Timestamp receiveTime);

    // 把一条已验证连接上的业务请求派发给 BattleRoomManager,并回 MessageContent。
    void DispatchVerifiedRequest(DirectSession &session,
                                 const muduo::net::TcpConnectionPtr &conn,
                                 const ::ClientRequest &request);

    // 非法包计数 + 达阈值即关;返回 true 表示连接已被关闭,调用方直接 return。
    bool RegisterIllegalPacket(DirectSession &session, const muduo::net::TcpConnectionPtr &conn,
                               const char *reason);

    void SendVerifyReply(const muduo::net::TcpConnectionPtr &conn, bool success,
                         const std::string &error, uint64_t battleId) const;
    void SendEnvelopeError(const muduo::net::TcpConnectionPtr &conn, const ::ClientRequest &request,
                           uint32_t tipId) const;

    static uint64_t ConnIdOf(const muduo::net::TcpConnectionPtr &conn);
    DirectSession *FindSession(const muduo::net::TcpConnectionPtr &conn);

    ProtobufDispatcher dispatcher_;
    ProtobufCodec codec_;
    std::unordered_map<uint64_t, DirectSession> sessions_;
    uint64_t nextConnId_ = 1;
};

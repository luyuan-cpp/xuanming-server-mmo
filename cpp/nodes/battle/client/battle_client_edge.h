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
//   1. 连接准入,与 gate 同一套做法(battle_connection_admission.h):accept 之后、TcpConnection 建出来之前判
//      关源 → 并发上限 BaseDeployConfig.battle_max_connections(0 仅 dev/test 允许)→ 新建连接速率,被拒的连接当场以
//      RST 关闭、不建任何每连接对象;连接回调里的上限判定留作第二道。连接类指标经节点的 /metrics 导出;
//   2. 空 battle_token_secret 的处置由 BATTLE_RUN_MODE 决定(prod 拒连);
//   3. 未验证连接 kHandshakeTimeoutSec 内没完成握手即关闭(防占槽);
//   4. 验证通过前丢弃并关闭一切非握手消息;
//   5. ClientRequest 体 ≤ kMaxClientRequestBytes;每消息号限速(MessageLimiter,与 gate
//      同一张表);消息号只放行 BattleClientPlayer 的四条客户端 RPC;非法包累计达阈值即关闭
//      (阈值与 gate 同源:IllegalPacketCounter,默认 50,GATE_ILLEGAL_PACKET_THRESHOLD 可调);
//   6. 应用层心跳(docs/design/gate-client-heartbeat.md §14,参数与 gate 同一份 client_liveness::kClientLiveness):
//      握手成功的应答通告心跳参数,从这一刻起连续 30s 收不到这条直连的任何一帧就 forceClose;
//      ClientHeartbeatRequest 每 10s 窗口最多回 3 条,超出的不回、计非法包;握手前的心跳同闸 4 处置。
//      关闭之后照常走断开回调(DetachDirectConnection),这里不加任何战斗逻辑。
//
// 与 gate 的关键差别:dev/test 空密钥只是**跳过签名比对**,握手包本身仍必须发 ——
// 直连没有别的途径知道这条连接属于哪场战斗、哪个玩家。

#include <cstdint>
#include <chrono>
#include <memory>
#include <optional>
#include <set>
#include <string>
#include <unordered_map>
#include <utility>

#include "muduo/base/noncopyable.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"

#include "battle_connection_admission.h"
#include "message_limiter/message_limiter.h"
#include "network/client_liveness.h"
#include "network/codec/codec.h"
#include "network/codec/dispatcher.h"
#include "time/comp/timer_task_comp.h"

#include "proto/battle/player_battle.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/match/match_network.pb.h"

class BattleClientEdge : muduo::noncopyable, public std::enable_shared_from_this<BattleClientEdge>
{
public:
    // 未验证连接的握手期限,与 gate 同一份(client_liveness::kClientLiveness.handshakeTimeout,10s):
    // 票据握手是客户端拿到 BattleAssignedS2C 后的第一件事,10s 远超正常 RTT。
    static constexpr double kHandshakeTimeoutSec =
        std::chrono::duration<double>(client_liveness::kClientLiveness.handshakeTimeout).count();
    // 与 gate CheckMessageSize 同口径:客户端合法业务消息不超过 1KB。
    static constexpr size_t kMaxClientRequestBytes = 1024;
    // battle_max_connections=0(dev/test)时的硬上限,防 fd 被吃光。
    static constexpr size_t kHardMaxConnections = 65535;

    BattleClientEdge();

    // 启动时调一次(main.cpp ConfigureClientAdmissionOrDie,Install 之前):换上正式的新建连接速率闸参数、未验签预算与
    // 按来源限速(sourceRateLimit 为空 = 关闭)。没调过时按硬上限推导(只有不 Install 的单测会这样)。
    void ConfigureAdmission(battle_connection_admission::RateLimit rateLimit,
                            battle_connection_admission::PreAuthBudget preAuthBudget,
                            std::optional<battle_connection_admission::RateLimit> sourceRateLimit)
    {
        admission.Configure(rateLimit, preAuthBudget, sourceRateLimit);
    }

    // 装到节点 TCP server 上(替换框架默认的 RpcCodec 回调,装上准入回调,登记连接类指标)。必须在 StartRpcServer
    // 之后调用(Node::SetAfterStart 回调里),与 gate main.cpp 同一时机。
    void Install(muduo::net::TcpServer &server);

    // 关源(停机 / 维护关源时调,不可撤销):之后的新直连在 accept 处以 RST 拒绝(reason = shutting_down)。
    // 已建立的直连不受影响,由 DisconnectAll 收尾。
    void CloseSource() { admission.CloseSource(); }

    // 下行:按 ProtobufCodec 帧写出(长度头 + 类型名 + 校验和)。
    // 绝不能裸 conn->send 序列化字节 —— 会毁掉整条连接的分帧(gate 同款教训)。
    // 走 static 的 ProtobufCodec::send:小游戏直连(WebSocket)时它再包一层二进制消息帧头。
    void Send(const muduo::net::TcpConnectionPtr &conn, const ::google::protobuf::Message &message) const;

    // 一份完整的下行帧(与 Send 写出的字节逐字节相同)。只能由 EncodeFrame 产出,SendEncoded 只收这个类型:
    // 裸序列化字节进不来(理由同 Send)。
    class EncodedFrame
    {
        friend class BattleClientEdge;
        explicit EncodedFrame(std::string bytes) : bytes(std::move(bytes)) {}
        std::string bytes;
        // 同一份帧加上 WebSocket 帧头后的字节,给小游戏直连用;SendEncoded 第一次遇到这类连接时才拼。
        mutable std::string webSocketBytes;
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

    size_t ConnectionCount() const { return sessions.size(); }

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
        // 建连时刻(单调时钟):握手耗时直方图从这里算。
        std::chrono::steady_clock::time_point openedAt;
        // 本端主动 shutdown() 进入半关闭的时刻(单调时钟):超过宽限期仍未退出时允许承压淘汰强关。
        std::optional<std::chrono::steady_clock::time_point> halfClosedAt;
        // 应用层心跳的存活状态(闸 6)。握手成功的应答发出时开始跟踪,只在跟踪中刷新「上一帧」时刻。
        client_liveness::ClientLiveness liveness;
        // 空闲检查:每条跟踪中的连接恰好一个定时器。到期时距上一帧不足阈值就按「上一帧 + 阈值」重新武装,
        // 本端 loop 卡过就推迟约 1s,静默满阈值就 forceClose 且不再武装。回调只持 connId,随会话销毁而取消。
        TimerTaskComp idleTimer;
        std::weak_ptr<muduo::net::TcpConnection> conn;
        std::optional<match::NetworkProbeGrant> probeGrant;
        std::string probeNonce;
        uint32_t probeSamples = 0;
        uint32_t probeRttMs = 0;
        std::chrono::steady_clock::time_point probeSent;
    };

    // 准入回调的函数体(Install 经 TcpServer::setAdmissionCallback 装上):放行返回 true;拒绝返回 false,
    // 已按原因计数并采样打日志(被拒的量就是攻击流量的量级,不能逐条写盘)。
    // 未验签预算要的池大小是 unauthenticated.size();最老一条只在池承压时才找(OldestConnectedUnauthenticated)。
    // 每次 accept 都是 O(1),不随直连数增长:准入回调排在速率闸之前,被拒的洪水也要走它,不能让它按直连数付代价。
    // 判定要求淘汰时,经最老那条会话里的 weak 连接 forceClose,会话由断开回调删。本端 loop 刚卡过时不淘汰。
    bool AdmitConnection(const muduo::net::InetAddress &peer, battle_connection_admission::SteadyTime now);
    // 承压时淘汰一条未认证直连给新连接让位:先移出池(这一刻起不再算在池里),再 forceClose;会话或连接已不在时什么都不做。
    void EvictUnauthenticated(uint64_t connId, battle_connection_admission::SteadyTime now);
    // 池里最老的一条可淘汰直连;没有返回空。仍在短宽限期内的半关闭连接跳过,超时未关闭的半关闭连接允许强关腾位。
    // now 是本次准入判定的时刻(AdmitConnection 传进来的那个),不在里面另取时钟。
    std::optional<uint64_t> OldestConnectedUnauthenticated(battle_connection_admission::SteadyTime now) const;
    // 未认证的直连数(指标与判定同一个数):既没完成票据握手、也没出示有效的网络探测授权(授权由 match 签发,OnProbeStart 验过签)。
    size_t UnauthenticatedConnectionCount() const { return unauthenticated.size(); }
    // 把连接类指标的采集回调登记进进程的指标注册表(Install 时一次)。
    void RegisterMetrics();

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
    // 闸 6:收心跳。找不到会话 / 握手前:关;本窗口已回满:不回、计非法包;其余回 ClientHeartbeatResponse{seq}。
    void OnHeartbeat(const muduo::net::TcpConnectionPtr &conn,
                     const std::shared_ptr<::ClientHeartbeatRequest> &request,
                     muduo::Timestamp receiveTime);

    // 收到本连接的一帧(握手包、ClientRequest、心跳;含随后被拒的):跟踪中的连接刷新「上一帧」。握手前不读时钟。
    static void NoteInboundFrame(DirectSession &session);
    // 握手成功的应答发出时调用:开始跟踪并武装第一次空闲检查。已在跟踪(重复握手)时什么都不做。
    void StartLivenessTracking(DirectSession &session);
    // 武装本连接的空闲检查,到期时刻 due 是单调时钟。
    void ScheduleIdleCheck(DirectSession &session, client_liveness::SteadyTime due);
    // 空闲检查到期(为 due 而设、在 now 执行):按 client_liveness::DecideIdleCheck 活着就重新武装、迟到就推迟、
    // 静默满阈值就 forceClose。会话已不在就什么都不做。
    void CheckIdle(uint64_t connId, client_liveness::SteadyTime due, client_liveness::SteadyTime now);

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
    // 这条直连是否已完成票据握手。给编解码器的验签前单帧上限用(准入设计 G10);只读、不打日志。
    bool IsVerifiedConnection(const muduo::net::TcpConnectionPtr &conn) const;

    ProtobufDispatcher dispatcher;
    ProtobufCodec codec;
    std::unordered_map<uint64_t, DirectSession> sessions;
    uint64_t nextConnId = 1;
    // 未认证直连池:还没完成票据握手、也没出示探测授权的直连的 connId。connId 单调递增、从不复用,有序集合即建连先后,
    // begin() 就是最老的。进池只在 HandleEstablished;出池在握手成功、探测授权验过、淘汰与 HandleClosed(会话表唯一的删除点),
    // erase 幂等,所以计数不会漂。
    std::set<uint64_t> unauthenticated;
    // 连接准入的状态与计数(battle_connection_admission.h)。起步按硬上限推导,main.cpp 启动时换成正式参数。
    battle_connection_admission::Admission admission{
        accept_admission::DefaultRateLimit(static_cast<uint32_t>(kHardMaxConnections))};
    // 本端 loop 刚卡过没有(client_liveness.h LoopStallWatch):Install 起一个 1 秒节拍的定时器给它打拍,
    // 未验签淘汰在卡过之后不淘汰(验签请求可能还在接收缓冲里没读),与 gate 同一口径。
    client_liveness::LoopStallWatch loopStall{client_liveness::kClientLiveness.checkPeriod};
    TimerTaskComp loopStallTimer;
};

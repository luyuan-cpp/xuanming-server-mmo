#include "battle_client_edge.h"

#include <boost/any.hpp>

#include "muduo/base/Logging.h"

#include "battle_security.h"
#include "logic/battle_room_manager.h"
#include "message_limiter/illegal_packet_counter.h"
#include "node/system/node/node.h"
#include "node_config_manager.h"
#include "time/system/time.h"

#include "proto/common/base/session.pb.h"
#include "rpc/service_metadata/player_battle_service_metadata.h"
#include "table/proto/tip/common_error_tip.pb.h"

namespace
{
    // 直连输出缓冲高水位:客户端收不动时不能让房间广播把 battle 进程内存吃光。
    // 回合制每回合一条几 KB 的包,2MB 已是几百回合的积压,达到即断连
    // (客户端凭票据重连后 GetBattleState 补拉全量,不会丢状态)。
    constexpr size_t kDirectConnHighWaterMark = 2 * 1024 * 1024;

    void OnDirectConnHighWaterMark(const muduo::net::TcpConnectionPtr &conn, size_t oldLen)
    {
        LOG_WARN << "battle 直连输出缓冲超高水位,断连: peer=" << conn->peerAddress().toIpPort()
                 << " pending=" << oldLen << " threshold=" << kDirectConnHighWaterMark;
        conn->forceClose();
    }

    // 拒绝日志限流:未验证连接的建立/拒绝次数完全由公网流量决定,不能逐条写盘。
    //
    // 按 reason **分别**计数:prod 里 handshake_timeout / at_capacity 是常态噪声,若与
    // ticket_hmac_mismatch 共用一个计数器,一轮针对签名的伪造尝试可能一条日志都不留,
    // 而 §18.6 把 reason 枚举写成了观测契约。每种原因首次必打,之后每 1024 次打一行。
    // reason 全部来自进程内常量(字面量 / TicketVerdictName),不含攻击者输入,
    // 键集合有界(十几个),用 map 不会被打爆。
    void LogRejectionSampled(const char *reason, const muduo::net::TcpConnectionPtr &conn)
    {
        static std::unordered_map<std::string, uint64_t> countsByReason;
        uint64_t &count = countsByReason[reason];
        if ((count++ & 0x3FF) == 0)
        {
            LOG_WARN << "battle 直连拒绝(采样): reason=" << reason
                     << " latest_peer=" << (conn ? conn->peerAddress().toIpPort() : std::string("<null>"))
                     << " reason_total=" << count;
        }
    }
} // namespace

BattleClientEdge::BattleClientEdge()
    : dispatcher_([this](const muduo::net::TcpConnectionPtr &conn, const MessagePtr &msg, muduo::Timestamp ts)
                  { OnUnknownMessage(conn, msg, ts); }),
      codec_([this](const muduo::net::TcpConnectionPtr &conn, const MessagePtr &msg, muduo::Timestamp ts)
             { dispatcher_.onProtobufMessage(conn, msg, ts); })
{
    dispatcher_.registerMessageCallback<::BattleTokenVerifyRequest>(
        [this](const muduo::net::TcpConnectionPtr &conn,
               const std::shared_ptr<::BattleTokenVerifyRequest> &request, muduo::Timestamp ts)
        { OnTokenVerify(conn, request, ts); });
    dispatcher_.registerMessageCallback<::ClientRequest>(
        [this](const muduo::net::TcpConnectionPtr &conn,
               const std::shared_ptr<::ClientRequest> &request, muduo::Timestamp ts)
        { OnClientRequest(conn, request, ts); });
}

void BattleClientEdge::Install(muduo::net::TcpServer &server)
{
    server.setConnectionCallback([this](const muduo::net::TcpConnectionPtr &conn)
                                 { OnConnection(conn); });
    server.setMessageCallback([this](const muduo::net::TcpConnectionPtr &conn, muduo::net::Buffer *buf,
                                     muduo::Timestamp ts)
                              { codec_.onMessage(conn, buf, ts); });
}

void BattleClientEdge::Send(const muduo::net::TcpConnectionPtr &conn,
                            const ::google::protobuf::Message &message) const
{
    if (!conn || !conn->connected())
    {
        return;
    }
    // 与 ProtobufCodec::send 同一份分帧(它的本体就是这两行),但走 static 版本:
    // 本方法是 const,codec_ 按值持有,非 const 的 codec_.send 在这里编不过。
    muduo::net::Buffer buf;
    ProtobufCodec::fillEmptyBuffer(&buf, message);
    conn->send(&buf);
}

void BattleClientEdge::DisconnectAll(const char *reason)
{
    if (sessions_.empty())
    {
        return;
    }
    LOG_INFO << "battle 直连全部断开: count=" << sessions_.size() << " reason=" << reason;
    for (auto &[connId, session] : sessions_)
    {
        session.handshakeTimer.Cancel();
        // AbortAllRooms 已对有房间的直连 shutdown()(终局包排空后 FIN)+ 延迟强关;
        // 这些连接 connected()==false,再 forceClose 会立刻关 fd,把还在用户态输出缓冲里
        // 的 SpectateEnd 字节丢掉。只强关仍 connected 的(未验证 / 空闲)连接。
        if (const auto conn = session.conn.lock(); conn && conn->connected())
        {
            conn->forceClose();
        }
    }
    // 断开回调会逐条 erase;这里不清表,让 HandleClosed 走正常解绑路径。
}

// ---- 连接生命周期 ----

uint64_t BattleClientEdge::ConnIdOf(const muduo::net::TcpConnectionPtr &conn)
{
    if (!conn || conn->getContext().empty())
    {
        return 0;
    }
    try
    {
        return boost::any_cast<uint64_t>(conn->getContext());
    }
    catch (const boost::bad_any_cast &)
    {
        return 0;
    }
}

BattleClientEdge::DirectSession *BattleClientEdge::FindSession(const muduo::net::TcpConnectionPtr &conn)
{
    const auto connId = ConnIdOf(conn);
    if (connId == 0)
    {
        return nullptr;
    }
    const auto it = sessions_.find(connId);
    return it == sessions_.end() ? nullptr : &it->second;
}

void BattleClientEdge::OnConnection(const muduo::net::TcpConnectionPtr &conn)
{
    if (conn->connected())
    {
        HandleEstablished(conn);
    }
    else
    {
        HandleClosed(conn);
    }
}

void BattleClientEdge::HandleEstablished(const muduo::net::TcpConnectionPtr &conn)
{
    const auto &deploy = gNodeConfigManager.GetBaseDeployConfig();

    // 闸 1:并发上限。必须是第一道:在分配会话之前拒掉,否则限流本身先付出它想省的内存。
    // 0 = 关闭运维阈值(仅 dev/test 合法,启动门禁已核过),但硬上限仍在。
    const size_t configuredMax = deploy.battle_max_connections();
    const size_t effectiveMax = configuredMax > 0 ? configuredMax : kHardMaxConnections;
    if (sessions_.size() >= effectiveMax)
    {
        LogRejectionSampled("at_capacity", conn);
        conn->forceClose();
        return;
    }

    // 闸 2:空密钥 + prod 一律拒连(纵深防御;启动门禁 ValidateBattleTokenSecretOrDie 已挡过一次)。
    const auto verdict = battle_security::ClassifyTokenSecret(deploy.battle_token_secret(),
                                                              battle_security::CurrentRunMode());
    if (verdict == battle_security::TokenSecretVerdict::kRefuse)
    {
        LogRejectionSampled("token_secret_not_configured", conn);
        conn->forceClose();
        return;
    }

    const uint64_t connId = nextConnId_++;
    conn->setContext(connId);
    conn->setHighWaterMarkCallback(OnDirectConnHighWaterMark, kDirectConnHighWaterMark);

    auto [it, inserted] = sessions_.try_emplace(connId);
    DirectSession &session = it->second;
    session.connId = connId;
    session.conn = conn;

    // 闸 3:握手期限。回调只持 connId,到期时重查会话;已验证则什么都不做。
    std::weak_ptr<muduo::net::TcpConnection> weakConn = conn;
    session.handshakeTimer.RunAfter(kHandshakeTimeoutSec, [this, connId, weakConn]
                                    {
        const auto found = sessions_.find(connId);
        if (found == sessions_.end() || found->second.verified)
        {
            return;
        }
        if (const auto c = weakConn.lock(); c)
        {
            LogRejectionSampled("handshake_timeout", c);
            c->forceClose();
        } });

    static uint64_t acceptedCount = 0;
    if ((acceptedCount++ & 0x3FF) == 0)
    {
        LOG_INFO << "battle 直连接入(采样): latest_conn_id=" << connId
                 << " latest_peer=" << conn->peerAddress().toIpPort()
                 << " accepted_total=" << acceptedCount
                 << " current=" << sessions_.size();
    }
}

void BattleClientEdge::HandleClosed(const muduo::net::TcpConnectionPtr &conn)
{
    const auto connId = ConnIdOf(conn);
    if (connId == 0)
    {
        // 容量 / 密钥闸拒掉的连接从未建立会话
        return;
    }
    const auto it = sessions_.find(connId);
    if (it == sessions_.end())
    {
        return;
    }
    DirectSession &session = it->second;
    session.handshakeTimer.Cancel();
    if (session.verified)
    {
        // 只解绑"仍指向本连接"的直连,晚到的旧连接 FIN 不能把重连后的新连接摘掉
        BattleRoomManager::Instance().DetachDirectConnection(session.battleId, session.playerId, conn);
        LOG_INFO << "battle 直连断开: battle_id=" << session.battleId
                 << " player_id=" << session.playerId
                 << " role=" << ::eBattleTicketRole_Name(session.role)
                 << " peer=" << conn->peerAddress().toIpPort()
                 << " remaining=" << (sessions_.size() - 1);
    }
    sessions_.erase(it);
}

// ---- 握手 ----

void BattleClientEdge::SendVerifyReply(const muduo::net::TcpConnectionPtr &conn, const bool success,
                                       const std::string &error, const uint64_t battleId) const
{
    ::BattleTokenVerifyResponse reply;
    reply.set_success(success);
    if (!error.empty())
    {
        reply.set_error(error);
    }
    reply.set_battle_id(battleId);
    Send(conn, reply);
}

void BattleClientEdge::OnTokenVerify(const muduo::net::TcpConnectionPtr &conn,
                                     const std::shared_ptr<::BattleTokenVerifyRequest> &request,
                                     muduo::Timestamp)
{
    DirectSession *session = FindSession(conn);
    if (session == nullptr)
    {
        LogRejectionSampled("verify_missing_session", conn);
        conn->forceClose();
        return;
    }

    const auto rejectAndClose = [&](const char *reason, const std::string &clientError)
    {
        LogRejectionSampled(reason, conn);
        SendVerifyReply(conn, false, clientError, 0);
        // shutdown 立刻切到 kDisconnecting(codec 停止分发),短延迟给应答一个 flush 窗口
        conn->shutdown();
        conn->forceCloseWithDelay(0.1);
    };

    if (session->verified)
    {
        // 重复握手幂等:回成功即可,不重新绑定
        SendVerifyReply(conn, true, "", session->battleId);
        return;
    }

    const auto &deploy = gNodeConfigManager.GetBaseDeployConfig();
    const auto &secret = deploy.battle_token_secret();
    const auto verdict = battle_security::ClassifyTokenSecret(secret, battle_security::CurrentRunMode());
    if (verdict == battle_security::TokenSecretVerdict::kRefuse)
    {
        rejectAndClose("token_secret_not_configured", "battle token secret not configured");
        return;
    }

    const auto &payloadBytes = request->payload();
    if (verdict == battle_security::TokenSecretVerdict::kEnforce)
    {
        const std::string signature(request->signature().begin(), request->signature().end());
        if (!battle_security::VerifyTicketSignature(secret, payloadBytes, signature))
        {
            rejectAndClose("ticket_hmac_mismatch", "invalid ticket signature");
            return;
        }
    }
    else
    {
        // dev/test 空密钥:跳过签名比对,但载荷、节点、期限、名单照常校验
        static bool sBypassWarned = false;
        if (!sBypassWarned)
        {
            sBypassWarned = true;
            LOG_WARN << "SECURITY WARNING: battle_token_secret is EMPTY and " << battle_security::kRunModeEnv
                     << "=" << battle_security::RunModeName(battle_security::CurrentRunMode())
                     << " -- ticket signatures are NOT verified. NEVER run this configuration in production.";
        }
    }

    ::BattleTicketPayload payload;
    if (!payload.ParseFromString(payloadBytes))
    {
        rejectAndClose("ticket_payload_parse_failed", "malformed ticket payload");
        return;
    }

    const battle_security::TicketFields fields{
        payload.battle_id(), payload.player_id(), payload.battle_node_id(),
        payload.battle_instance_id(), payload.expire_at_ms(), static_cast<int>(payload.role())};
    const auto ticketVerdict = battle_security::ClassifyTicketFields(
        fields, gNode->GetNodeId(), gNode->GetNodeInfo().node_uuid(), TimeSystem::NowMillisecondsUTC());
    if (ticketVerdict != battle_security::TicketVerdict::kOk)
    {
        rejectAndClose(battle_security::TicketVerdictName(ticketVerdict),
                       std::string("ticket rejected: ") + battle_security::TicketVerdictName(ticketVerdict));
        return;
    }

    // 房间仍存在且该玩家确在名单上(按角色);重连时替换旧连接。
    uint32_t gateSessionId = 0;
    if (!BattleRoomManager::Instance().AttachDirectConnection(payload.battle_id(), payload.player_id(),
                                                              payload.role(), conn, &gateSessionId))
    {
        rejectAndClose("ticket_not_in_roster", "battle not found or player not in this battle");
        return;
    }

    session->verified = true;
    session->battleId = payload.battle_id();
    session->playerId = payload.player_id();
    session->role = payload.role();
    session->gateSessionId = gateSessionId;
    IllegalPacketCounter::Reset(session->illegalPacketCount);
    session->handshakeTimer.Cancel();

    SendVerifyReply(conn, true, "", payload.battle_id());
    LOG_INFO << "battle 直连握手成功: battle_id=" << payload.battle_id()
             << " player_id=" << payload.player_id()
             << " role=" << ::eBattleTicketRole_Name(payload.role())
             << " peer=" << conn->peerAddress().toIpPort()
             << " signature_checked=" << (verdict == battle_security::TokenSecretVerdict::kEnforce);
}

// ---- 业务消息 ----

bool BattleClientEdge::RegisterIllegalPacket(DirectSession &session, const muduo::net::TcpConnectionPtr &conn,
                                             const char *reason)
{
    // 阈值与 gate 同源(默认 50,GATE_ILLEGAL_PACKET_THRESHOLD 可调,0 = 只计数不踢):
    // 压测刻意发坏包时对两个客户端面一起生效,不再各自硬编码。
    if (IllegalPacketCounter::RegisterAndShouldKill(session.illegalPacketCount))
    {
        LOG_WARN << "battle 直连非法包达阈值,断连: reason=" << reason
                 << " battle_id=" << session.battleId << " player_id=" << session.playerId
                 << " count=" << session.illegalPacketCount;
        conn->forceClose();
        return true;
    }
    return false;
}

void BattleClientEdge::SendEnvelopeError(const muduo::net::TcpConnectionPtr &conn, const ::ClientRequest &request,
                                         const uint32_t tipId) const
{
    ::MessageContent reply;
    reply.set_id(request.id());
    reply.set_message_id(request.message_id());
    reply.mutable_error_message()->set_id(tipId);
    Send(conn, reply);
}

void BattleClientEdge::OnClientRequest(const muduo::net::TcpConnectionPtr &conn,
                                       const std::shared_ptr<::ClientRequest> &request,
                                       muduo::Timestamp)
{
    DirectSession *session = FindSession(conn);
    if (session == nullptr)
    {
        conn->forceClose();
        return;
    }
    if (!session->verified)
    {
        // 闸 4:握手前一切业务消息都是不可信来源,直接关(不回应答,别给探测者放大器)
        LogRejectionSampled("request_before_verify", conn);
        conn->forceClose();
        return;
    }

    // 闸 5a:体积。超限包既回信封错误也计入非法包(gate 同款修法)。
    if (request->ByteSizeLong() > kMaxClientRequestBytes)
    {
        SendEnvelopeError(conn, *request, kMessageSizeExceeded);
        RegisterIllegalPacket(*session, conn, "oversized");
        return;
    }

    // 闸 5b:每消息号限速(gate CheckMessageLimit 同款)。持合法票据的玩家也不能用
    // 线速 GetBattleState 打满单线程的房间管理器 —— 每条都在主 loop 上做全量快照序列化。
    if (const auto err = session->messageLimiter.CanSend(request->message_id()); err != kSuccess)
    {
        SendEnvelopeError(conn, *request, err);
        RegisterIllegalPacket(*session, conn, "rate_limited");
        return;
    }

    DispatchVerifiedRequest(*session, conn, *request);
}

void BattleClientEdge::DispatchVerifiedRequest(DirectSession &session,
                                               const muduo::net::TcpConnectionPtr &conn,
                                               const ::ClientRequest &request)
{
    // 权威身份来自已验证的票据,不信请求体;session_id 回填 gate 会话号供日志对齐。
    // 这份合成的 SessionDetails 与 gate 经 gRPC 注入的形态一致,Handle* 零改动。
    ::SessionDetails sessionDetails;
    sessionDetails.set_player_id(session.playerId);
    sessionDetails.set_session_id(session.gateSessionId);

    ::MessageContent reply;
    reply.set_id(request.id());
    reply.set_message_id(request.message_id());

    const auto parseOrReject = [&](::google::protobuf::Message &target) -> bool
    {
        if (target.ParseFromString(request.body()))
        {
            return true;
        }
        SendEnvelopeError(conn, request, kInvalidParameter);
        RegisterIllegalPacket(session, conn, "body_parse_failed");
        return false;
    };

    // 闸 5c:消息号白名单 = BattleClientPlayer 的四条客户端 RPC。战斗连接不是大厅连接,
    // 任何别的消息号(登录 / 场景 / 匹配)在这里都没有意义,一律拒并计非法包。
    switch (request.message_id())
    {
    case BattleClientPlayerSubmitBattleActionMessageId:
    {
        ::SubmitBattleActionRequest req;
        if (!parseOrReject(req))
        {
            return;
        }
        ::SubmitBattleActionResponse resp;
        BattleRoomManager::Instance().HandleSubmitBattleAction(sessionDetails, req, resp);
        reply.set_serialized_message(resp.SerializeAsString());
        break;
    }
    case BattleClientPlayerGetBattleStateMessageId:
    {
        ::GetBattleStateRequest req;
        if (!parseOrReject(req))
        {
            return;
        }
        ::BattleStateS2C resp;
        BattleRoomManager::Instance().HandleGetBattleState(sessionDetails, req, resp);
        reply.set_serialized_message(resp.SerializeAsString());
        break;
    }
    case BattleClientPlayerStopWatchBattleMessageId:
    {
        ::StopWatchBattleRequest req;
        if (!parseOrReject(req))
        {
            return;
        }
        ::StopWatchBattleResponse resp;
        BattleRoomManager::Instance().HandleStopWatchBattle(sessionDetails, req, resp);
        reply.set_serialized_message(resp.SerializeAsString());
        break;
    }
    case BattleClientPlayerSetAutoBattleMessageId:
    {
        ::SetAutoBattleRequest req;
        if (!parseOrReject(req))
        {
            return;
        }
        ::SetAutoBattleResponse resp;
        BattleRoomManager::Instance().HandleSetAutoBattle(sessionDetails, req, resp);
        reply.set_serialized_message(resp.SerializeAsString());
        break;
    }
    default:
        SendEnvelopeError(conn, request, kInvalidParameter);
        RegisterIllegalPacket(session, conn, "message_id_not_allowed");
        return;
    }

    Send(conn, reply);
}

void BattleClientEdge::OnUnknownMessage(const muduo::net::TcpConnectionPtr &conn,
                                        const std::shared_ptr<::google::protobuf::Message> &message,
                                        muduo::Timestamp)
{
    // 直连面只认两种类型:握手包与 ClientRequest。别的类型名合法但不在此面的消息
    // (比如客户端把大厅那条连接的 ClientTokenVerifyRequest 发错了连接)一律关。
    // 只走采样日志:这一步在握手之前就可到达,逐条 WARN 就是按建连速率放大的日志 I/O
    //(gate client_message_processor.cpp 刚修过同类问题);类型名不进日志,采样行有 peer 够排障。
    (void)message;
    LogRejectionSampled("unknown_message_type", conn);
    conn->shutdown();
    conn->forceCloseWithDelay(0.1);
}

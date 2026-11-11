#include "battle_client_edge.h"

#include <algorithm>
#include <chrono>

#include <boost/any.hpp>
#include <openssl/rand.h>
#include "network_probe_security.h"

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
        LOG_WARN << "battle direct connection output buffer exceeded high water mark;disconnecting: peer=" << conn->peerAddress().toIpPort()
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
            LOG_WARN << "battle direct connection rejected(sampled): reason=" << reason
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
    dispatcher_.registerMessageCallback<match::NetworkProbeStart>([this](const auto& conn,const auto& request,auto ts){OnProbeStart(conn,request,ts);});
    dispatcher_.registerMessageCallback<match::NetworkProbeEcho>([this](const auto& conn,const auto& request,auto ts){OnProbeEcho(conn,request,ts);});
    dispatcher_.registerMessageCallback<::ClientHeartbeatRequest>(
        [this](const muduo::net::TcpConnectionPtr &conn,
               const std::shared_ptr<::ClientHeartbeatRequest> &request, muduo::Timestamp ts)
        { OnHeartbeat(conn, request, ts); });
}

void BattleClientEdge::Install(muduo::net::TcpServer &server)
{
    auto weak = weak_from_this();
    server.setConnectionCallback([weak](const muduo::net::TcpConnectionPtr &conn)
                                 { if(auto self=weak.lock()) self->OnConnection(conn); });
    server.setMessageCallback([weak](const muduo::net::TcpConnectionPtr &conn, muduo::net::Buffer *buf,
                                     muduo::Timestamp ts)
                              { if(auto self=weak.lock()) self->codec_.onMessage(conn, buf, ts); });
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

BattleClientEdge::EncodedFrame BattleClientEdge::EncodeFrame(const ::google::protobuf::Message &message)
{
    // 与 Send 同一份分帧;多出的一次拷贝(Buffer → string)每帧一次,不随连接数增长。
    muduo::net::Buffer buf;
    ProtobufCodec::fillEmptyBuffer(&buf, message);
    return EncodedFrame(buf.retrieveAllAsString());
}

void BattleClientEdge::SendEncoded(const muduo::net::TcpConnectionPtr &conn, const EncodedFrame &frame) const
{
    if (!conn || !conn->connected())
    {
        return;
    }
    conn->send(muduo::StringPiece(frame.bytes_));
}

size_t BattleClientEdge::EffectiveMaxConnections() const
{
    const size_t configuredMax = gNodeConfigManager.GetBaseDeployConfig().battle_max_connections();
    return configuredMax > 0 ? configuredMax : kHardMaxConnections;
}

void BattleClientEdge::DisconnectAll(const char *reason)
{
    if (sessions_.empty())
    {
        return;
    }
    LOG_INFO << "battle all direct connections disconnected: count=" << sessions_.size() << " reason=" << reason;
    for (auto &[connId, session] : sessions_)
    {
        session.handshakeTimer.Cancel();
        session.idleTimer.Cancel();
        // BeginRoomShutdown 已对有房间的直连 shutdown()(终局包排空后 FIN)+ 延迟强关;
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
    if (sessions_.size() >= EffectiveMaxConnections())
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
    const auto weakSelf=weak_from_this();
    session.handshakeTimer.RunAfter(kHandshakeTimeoutSec, [weakSelf, connId, weakConn]
                                    {
        const auto self=weakSelf.lock();if(!self)return;
        const auto found = self->sessions_.find(connId);
        if (found == self->sessions_.end() || found->second.verified)
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
        LOG_INFO << "battle direct connection accepted(sampled): latest_conn_id=" << connId
                 << " latest_peer=" << conn->peerAddress().toIpPort()
                 << " accepted_total=" << acceptedCount
                 << " current=" << sessions_.size();
    }
}

void BattleClientEdge::OnProbeStart(const muduo::net::TcpConnectionPtr& conn,
    const std::shared_ptr<match::NetworkProbeStart>& request, muduo::Timestamp)
{
    auto* s=FindSession(conn);
    if(!s||s->verified||s->probeGrant||network_probe::PhysicalRegion()==0){conn->forceClose();return;}
    const auto& signedGrant=request->grant();
    match::NetworkProbeGrant grant;
    const auto signature=network_probe::Sign(network_probe::kGrantDomain,signedGrant.payload());
    if(!token_security::ConstantTimeEquals(signature,signedGrant.signature())||!grant.ParseFromString(signedGrant.payload())||
       grant.player_id()==0||grant.plan_id().empty()||grant.session_binding().empty()||
       !gNode||grant.physical_region()!=network_probe::PhysicalRegion()||grant.target_node_id()!=gNode->GetNodeInfo().node_id()||
       grant.target_instance_id()!=gNode->GetNodeInfo().node_uuid()||grant.expires_at_ms()<=TimeSystem::NowMillisecondsUTC())
    {conn->forceClose();return;}
    s->probeGrant=std::move(grant);SendProbeChallenge(*s,conn);
}

void BattleClientEdge::SendProbeChallenge(DirectSession& s,const muduo::net::TcpConnectionPtr& conn)
{
    unsigned char nonce[32];if(RAND_bytes(nonce,sizeof(nonce))!=1){conn->forceClose();return;}
    s.probeNonce.assign(reinterpret_cast<const char*>(nonce),sizeof(nonce));
    match::NetworkProbeChallenge challenge;challenge.set_nonce(s.probeNonce);challenge.set_sample_index(s.probeSamples);
    s.probeSent=std::chrono::steady_clock::now();Send(conn,challenge);
}

void BattleClientEdge::OnProbeEcho(const muduo::net::TcpConnectionPtr& conn,
    const std::shared_ptr<match::NetworkProbeEcho>& request,muduo::Timestamp)
{
    auto* s=FindSession(conn);
    if(!s||s->verified||!s->probeGrant||request->sample_index()!=s->probeSamples||
       !token_security::ConstantTimeEquals(request->nonce(),s->probeNonce)){conn->forceClose();return;}
    const auto elapsed=std::chrono::duration_cast<std::chrono::microseconds>(std::chrono::steady_clock::now()-s->probeSent).count();
    if(elapsed<0||elapsed>3000000||s->probeGrant->expires_at_ms()<=TimeSystem::NowMillisecondsUTC()){conn->forceClose();return;}
    // 用三次的最大值，客户端调度变慢只会保守拒绝，无法填写更小RTT。
    s->probeRttMs=std::max(s->probeRttMs,static_cast<uint32_t>((elapsed+999)/1000));
    if(++s->probeSamples<3){SendProbeChallenge(*s,conn);return;}
    match::NetworkProbeReceipt receipt;*receipt.mutable_grant()=*s->probeGrant;
    receipt.set_rtt_ms(std::max(1u,s->probeRttMs));receipt.set_sample_count(3);
    receipt.set_measured_at_ms(TimeSystem::NowMillisecondsUTC());receipt.set_expires_at_ms(s->probeGrant->expires_at_ms());
    match::NetworkProbeComplete complete;auto* result=complete.mutable_receipt();result->set_payload(receipt.SerializeAsString());
    result->set_signature(network_probe::Sign(network_probe::kReceiptDomain,result->payload()));
    Send(conn,complete);s->probeGrant.reset();conn->shutdown();
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
    session.idleTimer.Cancel();
    if (session.verified)
    {
        // 只解绑"仍指向本连接"的直连,晚到的旧连接 FIN 不能把重连后的新连接摘掉
        BattleRoomManager::Instance().DetachDirectConnection(session.battleId, session.playerId, conn);
        LOG_INFO << "battle direct connection disconnected: battle_id=" << session.battleId
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
    if (success)
    {
        // 闸 6 的通告:与 gate 验签回包同一份参数。客户端收到就在本直连上按间隔发心跳;
        // 调用方随后调 StartLivenessTracking,从这一刻起按读空闲判死。失败应答不填。
        const auto &policy = client_liveness::kClientLiveness;
        reply.set_heartbeat_interval_ms(static_cast<uint32_t>(policy.interval.count()));
        reply.set_heartbeat_timeout_ms(static_cast<uint32_t>(policy.idleTimeout.count()));
    }
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
    NoteInboundFrame(*session);

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
        // 重复握手幂等:回成功即可,不重新绑定。应答照样带心跳参数;第一次握手时已开始跟踪,这里不重复武装。
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
        payload.battle_instance_id(), payload.expire_at_ms(), static_cast<int>(payload.role()), payload.spectator_operation_id()};
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
                                                              payload.role(), conn, &gateSessionId, payload.spectator_operation_id()))
    {
        rejectAndClose("ticket_not_in_roster", "battle not found or player not in this battle");
        return;
    }

    session->verified = true;
    session->battleId = payload.battle_id();
    session->playerId = payload.player_id();
    session->role = payload.role();
    session->spectatorOperation = payload.spectator_operation_id();
    session->gateSessionId = gateSessionId;
    IllegalPacketCounter::Reset(session->illegalPacketCount);
    session->handshakeTimer.Cancel();

    SendVerifyReply(conn, true, "", payload.battle_id());
    // 应答里已通告心跳参数:从这一刻起这条直连按读空闲判死(闸 6)。
    StartLivenessTracking(*session);
    LOG_INFO << "battle direct connection handshake succeeded: battle_id=" << payload.battle_id()
             << " player_id=" << payload.player_id()
             << " role=" << ::eBattleTicketRole_Name(payload.role())
             << " peer=" << conn->peerAddress().toIpPort()
             << " signature_checked=" << (verdict == battle_security::TokenSecretVerdict::kEnforce);

    // snapshot on connect(turn-based §22 D69):观众的观战首帧紧跟在握手应答之后经本直连下发。
    // 必须在 SendVerifyReply 之后 —— 客户端握手读到的第一个包必须是应答。
    BattleRoomManager::Instance().OnDirectConnectionVerified(payload.battle_id(), payload.player_id(),
                                                             payload.role());
}

// ---- 业务消息 ----

bool BattleClientEdge::RegisterIllegalPacket(DirectSession &session, const muduo::net::TcpConnectionPtr &conn,
                                             const char *reason)
{
    // 阈值与 gate 同源(默认 50,GATE_ILLEGAL_PACKET_THRESHOLD 可调,0 = 只计数不踢):
    // 压测刻意发坏包时对两个客户端面一起生效,不再各自硬编码。
    if (IllegalPacketCounter::RegisterAndShouldKill(session.illegalPacketCount))
    {
        LOG_WARN << "battle direct connection invalid packet threshold reached;disconnecting: reason=" << reason
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
    NoteInboundFrame(*session);
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
    if (const auto err = session->messageLimiter.TryConsumeRequestQuota(request->message_id()); err != kSuccess)
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
    // forceClose延后执行；已被替换/取消的旧连接在本轮仍可能有入站包，不能操作新槽位。
    if (!BattleRoomManager::Instance().OwnsDirectConnection(session.battleId, session.playerId,
            session.role, conn, session.spectatorOperation)) { conn->forceClose(); return; }
    // 直连是战斗唯一的上行入口(turn-based §22 D66):权威身份来自已验证的票据,不信请求体;
    // session_id 只是登记时的大厅会话号,仅供日志对齐。Handle* 仍收 SessionDetails(D28 零改动)。
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
    // 直连面只认握手包、ClientRequest、心跳与网络探测四类。别的类型名合法但不在此面的消息
    // (比如客户端把大厅那条连接的 ClientTokenVerifyRequest 发错了连接)一律关。
    // 只走采样日志:这一步在握手之前就可到达,逐条 WARN 就是按建连速率放大的日志 I/O
    //(gate client_session_handler.cpp 刚修过同类问题);类型名不进日志,采样行有 peer 够排障。
    (void)message;
    LogRejectionSampled("unknown_message_type", conn);
    conn->shutdown();
    conn->forceCloseWithDelay(0.1);
}

// ---- 应用层心跳与空闲超时(闸 6,docs/design/gate-client-heartbeat.md §14) ----

void BattleClientEdge::NoteInboundFrame(DirectSession &session)
{
    if (session.liveness.tracked)
    {
        session.liveness.NoteInbound(std::chrono::steady_clock::now());
    }
}

void BattleClientEdge::OnHeartbeat(const muduo::net::TcpConnectionPtr &conn,
                                   const std::shared_ptr<::ClientHeartbeatRequest> &request,
                                   muduo::Timestamp)
{
    DirectSession *session = FindSession(conn);
    if (session == nullptr)
    {
        LogRejectionSampled("heartbeat_missing_session", conn);
        conn->forceClose();
        return;
    }
    // muduo 的 receiveTime 是墙钟,不能拿来算超时;这里取单调时钟。
    const auto now = std::chrono::steady_clock::now();
    session->liveness.NoteInbound(now);

    const auto &policy = client_liveness::kClientLiveness;
    switch (client_liveness::AdmitHeartbeat(session->liveness, session->verified, now, policy))
    {
    case client_liveness::HeartbeatAction::kRejectUnverified:
        // 与闸 4 同一处置:握手前除握手包外一律关,不回应答。
        LogRejectionSampled("heartbeat_before_verify", conn);
        conn->forceClose();
        return;
    case client_liveness::HeartbeatAction::kExcess:
        // 正常客户端每 10s 一条,断网恢复后积压的几条也在窗口上限之内。超出的不回,与超限、坏号、限流同一个非法包计数。
        LogRejectionSampled("heartbeat_over_rate", conn);
        RegisterIllegalPacket(*session, conn, "heartbeat_over_rate");
        return;
    case client_liveness::HeartbeatAction::kAnswerAndStartTracking:
        // 握手成功时已开始跟踪(StartLivenessTracking),正常走不到这里;走到了也补武装空闲检查,不让这条直连漏判。
        ScheduleIdleCheck(*session, now + policy.idleTimeout);
        break;
    case client_liveness::HeartbeatAction::kAnswer:
        break;
    }

    ::ClientHeartbeatResponse reply;
    reply.set_seq(request->seq());
    Send(conn, reply);
}

void BattleClientEdge::StartLivenessTracking(DirectSession &session)
{
    const auto now = std::chrono::steady_clock::now();
    if (session.liveness.StartTracking(now))
    {
        ScheduleIdleCheck(session, now + client_liveness::kClientLiveness.idleTimeout);
    }
}

void BattleClientEdge::ScheduleIdleCheck(DirectSession &session, const client_liveness::SteadyTime due)
{
    // 回调只持 weak 自己、connId 与到期时刻,到期时重查会话(与握手期限同一写法,AGENTS §11.7);
    // 不持连接。RunAfter 先取消本会话尚未触发的那一次,所以每条连接至多一个待触发的空闲检查。
    const double delaySeconds = std::chrono::duration<double>(due - std::chrono::steady_clock::now()).count();
    const uint64_t connId = session.connId;
    const auto weakSelf = weak_from_this();
    const bool armed = session.idleTimer.RunAfter(std::max(delaySeconds, 0.0), [weakSelf, connId, due]
                                                  {
        if (const auto self = weakSelf.lock())
        {
            self->CheckIdle(connId, due, std::chrono::steady_clock::now());
        } });
    if (!armed)
    {
        // 只在没有 EventLoop 时发生(直连面总在 battle 主 loop 上):不静默放过,这条直连从此不会被判死。
        LOG_ERROR << "battle direct connection idle check not scheduled: battle_id=" << session.battleId
                  << " player_id=" << session.playerId << " conn_id=" << connId;
    }
}

void BattleClientEdge::CheckIdle(const uint64_t connId, const client_liveness::SteadyTime due,
                                 const client_liveness::SteadyTime now)
{
    const auto it = sessions_.find(connId);
    if (it == sessions_.end() || !it->second.liveness.tracked)
    {
        return;
    }
    DirectSession &session = it->second;
    const auto &policy = client_liveness::kClientLiveness;
    const auto check = client_liveness::DecideIdleCheck(session.liveness, due, now, policy);
    switch (check.verdict)
    {
    case client_liveness::IdleVerdict::kAlive:
        ScheduleIdleCheck(session, check.nextCheckAt);
        return;
    case client_liveness::IdleVerdict::kDeferred:
    {
        // 定时器比到期时刻晚了两个节拍以上,说明本端 loop 卡过:客户端的帧可能还在接收缓冲里没读。
        // 这一次不关,约 1s 后再判。这是该看的服务端问题,但卡一次会让很多连接同时走到这里,所以采样。
        static uint64_t deferredCount = 0;
        if ((deferredCount++ & 0x3FF) == 0)
        {
            LOG_WARN << "battle direct connection liveness verdict deferred, local loop stalled(sampled): late_ms="
                     << std::chrono::duration_cast<std::chrono::milliseconds>(now - due).count()
                     << " deferred_total=" << deferredCount;
        }
        ScheduleIdleCheck(session, check.nextCheckAt);
        return;
    }
    case client_liveness::IdleVerdict::kDead:
        break;
    }

    // 静默满阈值:只关连接。断开回调(HandleClosed → DetachDirectConnection)照常解绑;观众按既有规则进席位宽限,
    // 参战者的回合照常按超时自动出招,这里不加任何战斗逻辑。客户端掉线是正常事件,打 INFO;
    // 只有验证过的直连才会走到这里,条数以参战与观战人数为界,逐条打。
    LOG_INFO << "battle direct connection idle timeout;disconnecting: battle_id=" << session.battleId
             << " player_id=" << session.playerId << " role=" << ::eBattleTicketRole_Name(session.role)
             << " silent_ms="
             << std::chrono::duration_cast<std::chrono::milliseconds>(now - session.liveness.lastInboundAt).count()
             << " heartbeat_timeout_ms=" << policy.idleTimeout.count();
    // 只关仍 connected 的连接,与 DisconnectAll 同一条规则:房间收尾时已 shutdown() 的直连正在排空终局包,
    // 由它自己的延迟强关收尾,这里再 forceClose 会立刻关 fd,把输出缓冲里的终局包丢掉。
    if (const auto conn = session.conn.lock(); conn && conn->connected())
    {
        conn->forceClose();
    }
}

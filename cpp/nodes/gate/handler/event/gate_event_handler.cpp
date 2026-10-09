#include "gate_event_handler.h"
#include "thread_context/ecs_context.h"

///<<< BEGIN WRITING YOUR CODE
#include <session/manager/session_manager.h>
#include "muduo/base/Logging.h"
#include "gate_codec.h"
#include "proto/common/base/node.pb.h"
#include "proto/scene/client_player_common.pb.h"
#include "proto/scene/scene.pb.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "table/proto/tip/login_error_tip.pb.h"
#include "thread_context/node_context_manager.h"
#include "network/rpc_client.h"
#include "node/system/node/node_util.h"
#include "scene_route_helper.h"
#include "scene_entry_dispatch.h"

#include <chrono>

// 向 scene 转发进场(ForwardPlayerToScene)、欠账补发与超限收口都在 scene_entry_dispatch.cpp(CPP-2);
// 下面 RoutePlayer / BindSession 的守护段只做会话字段的写入并委托过去。
///<<< END WRITING YOUR CODE
void GateEventHandler::Register()
{
    tlsEcs.dispatcher.sink<contracts::kafka::RoutePlayerEvent>().connect<&GateEventHandler::RoutePlayerEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::KickPlayerEvent>().connect<&GateEventHandler::KickPlayerEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::PlayerDisconnectedEvent>().connect<&GateEventHandler::PlayerDisconnectedEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::PlayerLeaseExpiredEvent>().connect<&GateEventHandler::PlayerLeaseExpiredEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BindSessionEvent>().connect<&GateEventHandler::BindSessionEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::RedirectToGateEvent>().connect<&GateEventHandler::RedirectToGateEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::PushToPlayerEvent>().connect<&GateEventHandler::PushToPlayerEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BroadcastToPlayersEvent>().connect<&GateEventHandler::BroadcastToPlayersEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BroadcastToSceneEvent>().connect<&GateEventHandler::BroadcastToSceneEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BroadcastToAllEvent>().connect<&GateEventHandler::BroadcastToAllEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BindBattleEvent>().connect<&GateEventHandler::BindBattleEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::UnbindBattleEvent>().connect<&GateEventHandler::UnbindBattleEventHandler>();
}

void GateEventHandler::UnRegister()
{
    tlsEcs.dispatcher.sink<contracts::kafka::RoutePlayerEvent>().disconnect<&GateEventHandler::RoutePlayerEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::KickPlayerEvent>().disconnect<&GateEventHandler::KickPlayerEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::PlayerDisconnectedEvent>().disconnect<&GateEventHandler::PlayerDisconnectedEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::PlayerLeaseExpiredEvent>().disconnect<&GateEventHandler::PlayerLeaseExpiredEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BindSessionEvent>().disconnect<&GateEventHandler::BindSessionEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::RedirectToGateEvent>().disconnect<&GateEventHandler::RedirectToGateEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::PushToPlayerEvent>().disconnect<&GateEventHandler::PushToPlayerEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BroadcastToPlayersEvent>().disconnect<&GateEventHandler::BroadcastToPlayersEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BroadcastToSceneEvent>().disconnect<&GateEventHandler::BroadcastToSceneEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BroadcastToAllEvent>().disconnect<&GateEventHandler::BroadcastToAllEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BindBattleEvent>().disconnect<&GateEventHandler::BindBattleEventHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::UnbindBattleEvent>().disconnect<&GateEventHandler::UnbindBattleEventHandler>();
}
void GateEventHandler::RoutePlayerEventHandler(const contracts::kafka::RoutePlayerEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    const auto sessionId = event.session_id();
    const auto targetNodeId = event.target_node_id();

    auto &sessions = tlsSessionManager.sessions();
    auto it = sessions.find(sessionId);
    if (it == sessions.end())
    {
        // Normal race: player disconnected (sessions.erase) between SceneManager
        // accepting EnterScene and the RoutePlayer event landing on this gate.
        // No cleanup needed -- if the scene was never told about this player
        // (RoutePlayer never landed) there is nothing to clean up server-side.
        LOG_DEBUG << "RoutePlayer: session already gone (disconnect race), session_id=" << sessionId
                 << " player_id=" << event.player_id()
                 << " scene_node_id=" << targetNodeId
                 << " scene_id=" << event.scene_id();
        return;
    }

    auto &session = it->second;

    // Use player_id from the event if session doesn't have it yet (BindSession may not have arrived).
    if (event.player_id() != 0 && session.playerId == kInvalidGuid)
    {
        session.playerId = event.player_id();
    }

    // 归属 zone 与 epoch 属于"本次路由决策",在转发之前就无条件覆盖旧值:
    // 两次改派挨近时只有后到事件的 epoch 与 Redis 相等,旧值不能保留;转发若因节点未就绪失败,
    // 重投或晚到的 BindSession 补发时也必须带的是这份最新值,所以不能只在转发成功后才提交。
    // 节点暂时交不出去时,之后的补发也必须带这份值(补发从会话取,不再读路由事件)。
    session.homeZoneId = event.home_zone_id();
    session.ownerEpoch = event.owner_epoch();

    LOG_DEBUG << "RoutePlayer: route decision, session_id=" << sessionId
              << " target_node_id=" << targetNodeId
              << " scene_id=" << event.scene_id()
              << " home_zone_id=" << event.home_zone_id()
              << " owner_epoch=" << event.owner_epoch();

    // 路由(sceneId 与 scene 节点指向)只在转发成功后才整体提交,见 scene_route_helper.h 的
    // AttemptPendingSceneEntry;交不出去时记欠账、按单调时钟有上限地补发,超限推 kEnterSceneFailed 并踢线
    // (scene_entry_dispatch.cpp)。原先"先提交指向、转发失败后重投判成节点未变化"的已知局限随之消失,
    // 节点暂未发现也不再直接作废这条路由。
    gate_scene_entry::OnRouteDecision(sessionId, session,
                                      SceneRouteTarget{event.player_id(), targetNodeId, event.scene_id()},
                                      std::chrono::steady_clock::now());
///<<< END WRITING YOUR CODE
}
void GateEventHandler::KickPlayerEventHandler(const contracts::kafka::KickPlayerEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    const auto sessionId = event.session_id();
    auto &sessions = tlsSessionManager.sessions();
    auto it = sessions.find(sessionId);
    if (it == sessions.end())
    {
        // Expected during races between disconnect and kick command delivery.
        LOG_DEBUG << "KickPlayer: session not found, already disconnected. session_id=" << sessionId;
        return;
    }

    auto conn = it->second.conn.lock();
    if (!conn || !conn->connected())
    {
        LOG_DEBUG << "KickPlayer: connection already closed. session_id=" << sessionId;
        return;
    }

    // Send kick notification to client before closing.
    GameKickPlayerRequest kickMsg;
    kickMsg.mutable_reason()->set_id(kLoginBeKickByAnOtherAccount);
    MessageContent mc;
    mc.set_message_id(SceneClientPlayerCommonKickPlayerMessageId);
    mc.set_serialized_message(kickMsg.SerializeAsString());
    GetGateCodec().send(conn, mc);

    conn->shutdown();
    LOG_INFO << "KickPlayer: kicked session_id=" << sessionId
             << " player_id=" << it->second.playerId;
///<<< END WRITING YOUR CODE
}
void GateEventHandler::PlayerDisconnectedEventHandler(const contracts::kafka::PlayerDisconnectedEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}
void GateEventHandler::PlayerLeaseExpiredEventHandler(const contracts::kafka::PlayerLeaseExpiredEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    // player_locator 的 LeaseMonitor 判定该玩家的断线租约已到期(30s 内没有
    // 重连),后端会话与场景侧位置已经/即将清理。gate 这边若还挂着同一个
    // session 的 TCP 连接,那就是一条**假死连接**:后端已经不认这个会话,
    // 客户端却还占着 fd 和 SessionInfo,永远等不到任何下行 —— 必须收口。
    //
    // 这个 handler 此前是空实现,而 LeaseMonitor 把"这条通知投递成功"当作
    // ack claim 的必要副作用(至少一次投递) —— 空实现等于把 no-op 当成了
    // 必须成功的清理动作,也是老账"假死连接不会被清"(PROGRESS P2)的根因。
    //
    // 幂等性:正常情况下 TCP 断开回调早已把 session 摘掉,这里 find 不到,
    // 直接返回;只有"连接假死未触发断开回调"的场景才会真正走到 forceClose。
    const auto sessionId = event.session_id();
    auto &sessions = tlsSessionManager.sessions();
    const auto it = sessions.find(sessionId);
    if (it == sessions.end())
    {
        LOG_DEBUG << "LeaseExpired: session already gone. session_id=" << sessionId
                  << " player_id=" << event.player_id();
        return;
    }

    // 会话已被复用给别的玩家时绝不能踢(session_id 是 gate 本地发号,
    // 理论上不复用,但 fail-safe 校验便宜)。
    if (event.player_id() != 0 && it->second.playerId != kInvalidGuid &&
        it->second.playerId != event.player_id())
    {
        LOG_WARN << "LeaseExpired: session player mismatch, skip kick. session_id=" << sessionId
                 << " event_player=" << event.player_id()
                 << " session_player=" << it->second.playerId;
        return;
    }

    auto conn = it->second.conn.lock();
    if (conn)
    {
        // forceClose(而非 shutdown):对端已经假死,不能指望它配合四次挥手。
        // 关闭会触发正常的 TCP 断开回调,由它统一走会话摘除 + Disconnect 通知,
        // 不在这里重复清理。
        LOG_INFO << "LeaseExpired: force closing zombie connection. session_id=" << sessionId
                 << " player_id=" << event.player_id();
        conn->forceClose();
        return;
    }

    // 无 conn 的残留会话:断开回调永远不会来,只能就地摘除。
    LOG_INFO << "LeaseExpired: removing connectionless session. session_id=" << sessionId
             << " player_id=" << event.player_id();
    sessions.erase(it);
///<<< END WRITING YOUR CODE
}
void GateEventHandler::BindSessionEventHandler(const contracts::kafka::BindSessionEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    const auto sessionId = event.session_id();
    const auto playerId = event.player_id();
    const auto enterGsType = event.enter_gs_type();

    auto &sessions = tlsSessionManager.sessions();
    auto it = sessions.find(sessionId);
    if (it == sessions.end())
    {
        // Normal race: player disconnected before BindSession landed.
        LOG_WARN << "BindSession: session already gone (disconnect race), session_id=" << sessionId
                 << " player_id=" << playerId;
        return;
    }

    it->second.playerId = playerId;
    it->second.sessionVersion = event.session_version();

    // 登录类型先落进会话,再交给 CPP-2 的欠账逻辑(scene_entry_dispatch.cpp OnLoginTypeBound):
    // 以前只在"已提交节点指向 && sceneId != 0"时按已提交的指向立即补发,转发失败就只把登录类型留在会话里;
    // 指向随后被节点摘除置无效、或因同 uuid 重注册而悬空时,登录类型会永远挂着。现在按最近一次路由的
    // node_id 重新解析,建一笔有上限的欠账(超限推 kEnterSceneFailed 并踢线);还没收到路由时什么都不做,
    // 等路由到达时由 RoutePlayer 一并转发。归属 zone / epoch 仍取会话里 RoutePlayer 先前存下的那份。
    if (enterGsType != 0)
    {
        it->second.pendingEnterGsType = enterGsType;
        gate_scene_entry::OnLoginTypeBound(sessionId, it->second, std::chrono::steady_clock::now());
    }

    LOG_DEBUG << "BindSession: bound session_id=" << sessionId
              << " player_id=" << playerId
              << " enter_gs_type=" << enterGsType;
    ///<<< END WRITING YOUR CODE
}
void GateEventHandler::RedirectToGateEventHandler(const contracts::kafka::RedirectToGateEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    const auto sessionId = event.session_id();
    auto &sessions = tlsSessionManager.sessions();
    auto it = sessions.find(sessionId);
    if (it == sessions.end())
    {
        // Normal race: player disconnected before RedirectToGate landed.
        LOG_WARN << "RedirectToGate: session already gone (disconnect race), session_id=" << sessionId
                 << " player_id=" << event.player_id();
        return;
    }

    auto conn = it->second.conn.lock();
    if (!conn || !conn->connected())
    {
        LOG_DEBUG << "RedirectToGate: connection already closed, session_id=" << sessionId;
        return;
    }

    // Build client-facing redirect message.
    RedirectToGateNotify notify;
    notify.set_target_ip(event.target_gate_ip());
    notify.set_target_port(event.target_gate_port());
    notify.set_token_payload(event.token_payload().data(), event.token_payload().size());
    notify.set_token_signature(event.token_signature().data(), event.token_signature().size());
    notify.set_token_deadline(event.token_deadline());

    MessageContent mc;
    mc.set_message_id(SceneClientPlayerCommonRedirectToGateMessageId);
    mc.set_serialized_message(notify.SerializeAsString());
    GetGateCodec().send(conn, mc);

    LOG_INFO << "RedirectToGate: sent redirect to player " << event.player_id()
             << " session_id=" << sessionId
             << " target=" << event.target_gate_ip() << ":" << event.target_gate_port();
///<<< END WRITING YOUR CODE
}
void GateEventHandler::PushToPlayerEventHandler(const contracts::kafka::PushToPlayerEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
auto sessionIt = tlsSessionManager.sessions().find(event.session_id());
if (sessionIt == tlsSessionManager.sessions().end())
{
    // Normal race: player disconnected before this push landed.
    LOG_WARN << "PushToPlayer: session already gone (disconnect race), session_id=" << event.session_id();
    return;
}
// 会话不持有连接；发送前锁定，连接已释放时丢弃本次推送。
auto conn = sessionIt->second.conn.lock();
if (!conn)
{
    LOG_ERROR << "PushToPlayer: session has no connection, session_id=" << event.session_id();
    return;
}
GetGateCodec().send(conn, event.message_content());
///<<< END WRITING YOUR CODE
}
void GateEventHandler::BroadcastToPlayersEventHandler(const contracts::kafka::BroadcastToPlayersEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
auto sendToSession = [&](uint32_t sessionId)
{
    auto sessionIt = tlsSessionManager.sessions().find(sessionId);
    if (sessionIt == tlsSessionManager.sessions().end())
        return;
    auto conn = sessionIt->second.conn.lock();
    if (!conn)
        return;
    GetGateCodec().send(conn, event.message_content());
};

if (!event.session_bitmap().empty())
{
    const uint32_t base = event.session_bitmap_base();
    const auto &bitmap = event.session_bitmap();
    for (size_t i = 0; i < bitmap.size(); ++i)
    {
        const uint8_t byte = static_cast<uint8_t>(bitmap[i]);
        for (int bit = 0; bit < 8; ++bit)
        {
            if (byte & (1 << bit))
            {
                sendToSession(base + static_cast<uint32_t>(i * 8 + bit));
            }
        }
    }
}
else
{
    for (auto sessionId : event.session_list())
    {
        sendToSession(sessionId);
    }
}
///<<< END WRITING YOUR CODE
}
void GateEventHandler::BroadcastToSceneEventHandler(const contracts::kafka::BroadcastToSceneEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}
void GateEventHandler::BroadcastToAllEventHandler(const contracts::kafka::BroadcastToAllEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}
void GateEventHandler::BindBattleEventHandler(const contracts::kafka::BindBattleEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}
void GateEventHandler::UnbindBattleEventHandler(const contracts::kafka::UnbindBattleEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}

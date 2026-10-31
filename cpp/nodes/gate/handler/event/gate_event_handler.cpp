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
#include "handler/rpc/client_request_lane.h"
#include "handler/gate_push_delivery.h"
#include "spectate/gate_spectate_client.h"
#include <session/system/session_identity_fence.h>

#include <chrono>

// 向 scene 转发进场(ForwardPlayerToScene)、欠账补发与超限收口都在 scene_entry_dispatch.cpp(CPP-2);
// 下面 RoutePlayer / BindSession 的守护段只经 scene_route_helper.h 的纯函数写会话字段,再委托过去。

namespace
{
    // 旧版 login 的踢线不带 player_id(ClassifyKick 判 kKickUnfenced):按 session_id 踢(旧行为)。
    // 迁移窗口里每条踢线都成立,只在本线程首次记一行 INFO,看到它 = login 灰度尚未完成
    // (docs/design/scene-enter-chain-residuals.md §2.5)。做法同 handler/gate_push_delivery.cpp 的 LogFenceUnfencedOnce;
    // 标记用 thread_local,与 tlsSessionManager 同一线程归属,不引入跨线程共享状态。
    void LogKickUnfencedOnce(uint32_t sessionId)
    {
        thread_local bool logged = false;
        if (logged)
        {
            return;
        }
        logged = true;
        LOG_INFO << "KickPlayer: received a kick without player_id (legacy login); kicking by session_id only. "
                 << "Kick identity fence is inactive for such events; this is logged once per thread. session_id="
                 << sessionId << " See docs/design/scene-enter-chain-residuals.md section 2.";
    }
}
///<<< END WRITING YOUR CODE
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

    // 客户端 LeaveGame 之后、下一次登录进场之前到达的、属于 LeaveGame 之前那条登录链的路由:在写任何会话字段之前丢弃
    // (scene_route_helper.h IsRouteFromBeforeLeaveGame,docs/design/location-release-binding.md §3.5 R5)。
    // 不丢的话它会重新填上 lastSceneRoute / ownerEpoch,下一次 BindSession 按这条旧路由、带旧 epoch 补发。
    // 丢弃时把它的代次记进被离开的那条链,同一条旧链在客户端再次 EnterGame 之后才到的 Bind 仍被认出(第二轮审计 r2-gate#1)。
    if (gate_scene_route::IsRouteFromBeforeLeaveGame(session, event.player_id(), event.login_generation()))
    {
        LOG_INFO << "RoutePlayer: dropped a route issued before the client left the game, session_id=" << sessionId
                 << " player_id=" << event.player_id()
                 << " scene_node_id=" << targetNodeId
                 << " scene_id=" << event.scene_id()
                 << " owner_epoch=" << event.owner_epoch()
                 << " route_login_generation=" << event.login_generation()
                 << " enter_game_sent_after_leave=" << session.enterGameSentAfterLeave
                 << " login_generation_at_leave=" << session.loginGenerationAtReset;
        gate_scene_route::NoteDroppedFromBeforeLeaveGame(session, event.player_id(), event.login_generation());
        return;
    }

    // 身份字段在转发之前写进会话(scene_route_helper.h ApplyRouteIdentity):转发、欠账重试与晚到 BindSession
    // 的补发都从会话取,不再读路由事件。玩家只在会话未绑角色时采用(BindSession 可能尚未到达);归属 zone 与
    // epoch 属于"本次路由决策",无条件覆盖,两次改派挨近时旧值不能保留;登录代次只对本角色的路由取 max,
    // 不据此丢弃路由(scene-enter-session-fence.md §4)。
    gate_scene_route::ApplyRouteIdentity(session, event.player_id(), event.home_zone_id(), event.owner_epoch(),
                                         event.login_generation(), event.owner_revision());

    LOG_DEBUG << "RoutePlayer: route decision, session_id=" << sessionId
              << " target_node_id=" << targetNodeId
              << " scene_id=" << event.scene_id()
              << " home_zone_id=" << event.home_zone_id()
              << " owner_epoch=" << event.owner_epoch()
              << " login_generation=" << event.login_generation()
              << " session_login_generation=" << session.loginGeneration;

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

    // 踢线身份栅栏(session_identity_fence.h ClassifyKick;scene-enter-chain-residuals.md §2.2 ④):
    // 只踢当前绑定该玩家、且没承载更新登录的会话。纯判定,在任何副作用之前做;只读 loginGeneration,不写
    // (写入点仍只有 scene_route_helper.h 的两个 helper)。
    const auto &session = it->second;
    const auto verdict = gate_session_fence::ClassifyKick(session.playerId, session.loginGeneration,
                                                          event.player_id(), event.login_generation());
    switch (verdict)
    {
    case gate_session_fence::KickVerdict::kSkipOtherPlayer:
    case gate_session_fence::KickVerdict::kSkipNewerLogin:
        LOG_WARN << "KickPlayer: identity fence skipped kick. session_id=" << sessionId
                 << " session_player_id=" << session.playerId
                 << " event_player_id=" << event.player_id()
                 << " session_login_generation=" << session.loginGeneration
                 << " kicker_login_generation=" << event.login_generation()
                 << " verdict=" << gate_session_fence::KickVerdictName(verdict);
        return;
    case gate_session_fence::KickVerdict::kKickUnfenced:
        LogKickUnfencedOnce(sessionId);
        break;
    case gate_session_fence::KickVerdict::kKick:
        break;
    }

    auto conn = session.conn.lock();
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
             << " player_id=" << session.playerId
             << " event_player_id=" << event.player_id()
             << " session_login_generation=" << session.loginGeneration
             << " kicker_login_generation=" << event.login_generation()
             << " verdict=" << gate_session_fence::KickVerdictName(verdict);
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
    // 与正常断开同一收尾:先注销路由 lane 的在途调用、丢弃排队项,摘掉观战订阅,再摘会话(client-request-ordering.md §4.6)。
    gate_lane::OnSessionClosing(it->second);
    gate_spectate::OnSessionClosed(sessionId);
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

    auto &session = it->second;

    // 客户端 LeaveGame 之后到达的、属于 LeaveGame 之前那条登录链的 Bind(login 后台链在 LeaveGame 之前就发出、还在 Kafka 里):
    // 在写任何会话字段之前丢弃(scene_route_helper.h IsBindFromBeforeLeaveGame,location-release-binding.md §3.5 R5,
    // 第二轮审计 r2-gate#1)。不丢的话它把登录类型挂回会话、把代次抬到旧链的值,旧链随后到达的路由就被当成新登录的路由,
    // 带着登录类型转发到旧节点。丢弃时把代次记进被离开的那条链,同一条旧链之后才到的路由仍被认出。
    if (gate_scene_route::IsBindFromBeforeLeaveGame(session, playerId, event.login_generation()))
    {
        LOG_INFO << "BindSession: dropped a bind issued before the client left the game, session_id=" << sessionId
                 << " player_id=" << playerId
                 << " enter_gs_type=" << enterGsType
                 << " bind_login_generation=" << event.login_generation()
                 << " enter_game_sent_after_leave=" << session.enterGameSentAfterLeave
                 << " login_generation_at_leave=" << session.loginGenerationAtReset;
        gate_scene_route::NoteDroppedFromBeforeLeaveGame(session, playerId, event.login_generation());
        return;
    }

    // 角色、会话版本号、登录代次与登录类型先落进会话(scene_route_helper.h ApplyLoginBinding),再交给 CPP-2
    // 的欠账逻辑(scene_entry_dispatch.cpp OnLoginTypeBound)。代次必须在它之前写:它会同步按最近一次路由补发,
    // 盖章从会话取值;同连接重登时 Bind 先于本次路由到达,这次补发若带上一次登录的代次,中间被别的设备顶过号时
    // 会被 scene 判陈旧(scene-enter-session-fence.md §4)。
    // 以前只在"已提交节点指向 && sceneId != 0"时按已提交的指向立即补发,转发失败就只把登录类型留在会话里;
    // 指向随后被节点摘除置无效、或因同 uuid 重注册而悬空时,登录类型会永远挂着。现在按最近一次路由的
    // node_id 重新解析,建一笔有上限的欠账(超限推 kEnterSceneFailed 并踢线);还没收到路由时什么都不做,
    // 等路由到达时由 RoutePlayer 一并转发。归属 zone / epoch 仍取会话里 RoutePlayer 先前存下的那份。
    if (gate_scene_route::ApplyLoginBinding(session, playerId, event.session_version(), event.login_generation(),
                                            enterGsType))
    {
        // 不会 erase 会话(AttemptAndSettle 的约定),下面的日志仍可读 session。
        gate_scene_entry::OnLoginTypeBound(sessionId, session, std::chrono::steady_clock::now());
    }

    LOG_DEBUG << "BindSession: bound session_id=" << sessionId
              << " player_id=" << playerId
              << " enter_gs_type=" << enterGsType
              << " login_generation=" << event.login_generation()
              << " session_login_generation=" << session.loginGeneration;
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

    // 路由 lane(docs/design/client-request-ordering.md O9):会话转去别的 gate,丢弃还没发出的排队项,
    // 之后到达的路由类请求回带请求 id 的 1003;在途那一条的回包照常下发。不这样做,旧连接上排着的请求会继续发出,
    // 与目标 zone 新 gate 上的新会话请求交错执行。与 Java 版 ClientDispatcher.enterRedirected 一致。
    gate_lane::OnSessionRedirected(it->second);

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
    // 查会话、lock 连接、身份栅栏(R13,目标玩家 = event.player_id)与写 socket,
    // 与 RPC 版 SendMessageToPlayer 同一份实现(handler/gate_push_delivery.h)。
    gate_push::DeliverToSession(gate_push::Path::kKafka, event.session_id(), event.player_id(),
                                gate_push::ContentOf(event));
///<<< END WRITING YOUR CODE
}
void GateEventHandler::BroadcastToPlayersEventHandler(const contracts::kafka::BroadcastToPlayersEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    // (session_id, player_id) 按 broadcast_target_codec.h 的顺序契约解码、整条校验(player_list 不齐整条拒绝)、
    // 逐个目标过身份栅栏(R13)、编码一次后复用字节扇出,与 RPC 版 BroadcastToPlayers 同一份实现。
    gate_push::DeliverToTargets(gate_push::Path::kKafka, event);
///<<< END WRITING YOUR CODE
}
void GateEventHandler::BroadcastToSceneEventHandler(const contracts::kafka::BroadcastToSceneEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    // 只推本场景内已绑定玩家的会话,scene_id 为 0 整条拒绝;与 RPC 版 BroadcastToScene 同一份实现。
    gate_push::DeliverToScene(gate_push::Path::kKafka, event.scene_id(), gate_push::ContentOf(event));
///<<< END WRITING YOUR CODE
}
void GateEventHandler::BroadcastToAllEventHandler(const contracts::kafka::BroadcastToAllEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    // 只推已绑定玩家的会话(未登录的连接不收游戏广播);与 RPC 版 BroadcastToAll 同一份实现。
    gate_push::DeliverToAll(gate_push::Path::kKafka, gate_push::ContentOf(event));
///<<< END WRITING YOUR CODE
}

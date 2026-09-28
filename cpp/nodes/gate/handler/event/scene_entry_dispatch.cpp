#include "scene_entry_dispatch.h"

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <type_traits>
#include <unordered_set>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "session/manager/session_manager.h"
#include "scene_route_helper.h"
#include "handler/rpc/client_message_processor.h"
#include "gate_codec.h"
#include "proto/scene/scene.pb.h"
#include "proto/scene/client_player_common.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/common.pb.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "table/proto/tip/scene_error_tip.pb.h"
#include "thread_context/node_context_manager.h"
#include "network/rpc_client.h"
#include "node/system/node/node_util.h"

static_assert(std::is_same_v<gate_scene_entry::Clock, SceneEntryClock>,
              "scene_entry_dispatch.h 的 Clock 必须与欠账记录用的 SceneEntryClock 是同一个单调时钟");

namespace
{
using gate_scene_entry::Clock;

// 有欠账的会话索引。扫描只遍历它而不是整张会话表:正常情况下它是空的,定时器每 250ms 只做一次 empty()。
// 它只是索引,真相在 SessionInfo::pendingSceneEntry:会话 erase(断线)或 session_id 被复用时,
// 扫描 find 不到欠账就把索引项删掉,不需要断线回调来配合。thread_local:只由 gate 主 EventLoop 线程读写。
thread_local std::unordered_set<SessionId> tlsSessionsWithPendingEntry;

// 只走日志的计数(C++ 还没接 Prometheus)。都是进程累计值,不以 player_id 做任何维度。
struct SceneEntryCounters
{
    uint64_t deferred{0};      // 首次尝试没发出去、进入补发的路由决策数
    uint64_t recovered{0};     // 补发后成功交付
    uint64_t gaveUp{0};        // 超限放弃(已推 3023 + 34 并关连接)
    uint64_t superseded{0};    // 欠账未结清时被新路由决策整体替换
    uint64_t sessionClosed{0}; // 客户端连接已不可用或会话已消失,欠账随之撤销
    uint64_t playerChanged{0}; // 欠账属于另一名角色,撤销(不踢线)
    uint64_t linksReady{0};    // scene 链路握手就绪次数

    bool operator==(const SceneEntryCounters &) const = default;
};

thread_local SceneEntryCounters tlsCounters;
thread_local SceneEntryCounters tlsLastSummaryCounters;
thread_local std::size_t tlsLastSummaryPending{0};
thread_local Clock::time_point tlsNextSummaryAt{};

// 30s 汇总:故障期间排障以汇总行里的 pending / gave_up 为准,逐条的延后 / 恢复日志只采样。
constexpr std::chrono::seconds kSummaryInterval{30};

// 1/64 采样,第一条必打。延后与恢复的量级等于故障期间的进场数,不能逐条写。
constexpr uint64_t kSampleMask = 0x3F;
thread_local uint64_t tlsDeferredLogSeq{0};
thread_local uint64_t tlsRecoveredLogSeq{0};
thread_local uint64_t tlsPlayerChangedLogSeq{0};

bool ShouldLogSampled(uint64_t &seq)
{
    return (seq++ & kSampleMask) == 0;
}

int64_t ElapsedMs(const Clock::time_point from, const Clock::time_point to)
{
    return std::chrono::duration_cast<std::chrono::milliseconds>(to - from).count();
}

// 向 scene 转发 PlayerEnterGameNode,如实返回"有没有真的交出去"。
// Gate is the bridge: it holds the TCP session and routes the RPC to the correct Scene node.
//
// 必须在调用 CallRemoteMethod **之前**判定链路:
//   - RpcClient::CallRemoteMethod 在未连接时只打一条 ERROR 就静默丢弃,调用方拿不到失败;
//   - TCP 连上后 registration_manager 要 RunAfter(0.5) 才发 NodeHandshake,scene 收到握手才为本 gate 挂
//     RpcSession;在那之前发出的进场,scene 会照常载入玩家,但 NotifyEnterScene 等推送找不到 RpcSession
//     会被丢掉(player_message_utils.cpp)。所以要求"已连上,且在**当前**连接上握过手"。
// 这些都是"本地确定没发出去",调用方可以安全地补发(I1);一旦返回 kSent 就不再重发。
//
// playerId / homeZoneId / ownerEpoch 一律从会话取:路由到达时已把最新决策写进会话,BindSession 晚到的补发
// 读不到路由事件,只能从会话取。sceneId 显式传入:会话的 sceneId 只在转发成功后才提交,调用时还是旧图。
SceneForwardResult ForwardPlayerToScene(const SessionId sessionId, const uint32_t enterGsType,
                                        const uint64_t sceneNodeEntityId, const SessionInfo &session,
                                        const uint64_t sceneId)
{
    // LOGIN_NONE(0) 用于已登录角色换图;要不要转发、以什么类型转发由 ApplyRoute 决定。
    auto &sceneRegistry = tlsNodeContextManager.GetRegistry(SceneNodeService);
    const entt::entity sceneEntity{sceneNodeEntityId};
    if (!sceneRegistry.valid(sceneEntity))
    {
        return SceneForwardResult::kNodeNotFound;
    }

    const auto *rpcClient = sceneRegistry.try_get<RpcClientPtr>(sceneEntity);
    if (!rpcClient || !*rpcClient)
    {
        return SceneForwardResult::kNoRpcClient;
    }

    // 局部强引用只活到本函数返回,不外泄(AGENTS.md §11.7)。
    const auto currentConn = (*rpcClient)->GetConnection();
    const auto *handshake = sceneRegistry.try_get<gate_scene_entry::SceneLinkHandshakeComp>(sceneEntity);
    const auto handshakenConn = handshake ? handshake->conn.lock() : muduo::net::TcpConnectionPtr{};
    if (const auto notReady = gate_scene_route::ClassifySceneLink((*rpcClient)->connected(),
                                                                   currentConn.get(),
                                                                   currentConn && currentConn->connected(),
                                                                   handshakenConn.get()))
    {
        return *notReady;
    }

    const auto playerId = session.playerId;

    PlayerEnterGameNodeRequest req;
    req.set_player_id(playerId);
    req.set_session_id(sessionId);
    req.set_enter_gs_type(enterGsType);
    req.set_scene_id(sceneId);
    // 归属 zone 与 epoch 原样透传(cross-zone-scene-travel.md CZ-3 / CZ-4):gate 不做任何校验或补默认值,
    // 0 也照发——它是 scene 侧"兼容窗口/未知"的信号,gate 擅自填进程 zone 会把 fail-closed 变成静默落错库。
    req.set_home_zone_id(session.homeZoneId);
    req.set_owner_epoch(session.ownerEpoch);

    (*rpcClient)->CallRemoteMethod(ScenePlayerEnterGameNodeMessageId, req);

    LOG_DEBUG << "ForwardPlayerToScene: sent PlayerEnterGameNode to scene_node=" << sceneNodeEntityId
              << " player=" << playerId << " session=" << sessionId
              << " scene_id=" << sceneId << " enter_gs_type=" << enterGsType
              << " home_zone_id=" << session.homeZoneId << " owner_epoch=" << session.ownerEpoch;
    return SceneForwardResult::kSent;
}

// 按业务 node_id 解析 gate 上的 scene 节点实体号;每次尝试都重新解析,不缓存实体号(I3)。
std::optional<uint64_t> ResolveSceneNodeEntity(const uint32_t nodeId)
{
    const auto entity = NodeUtils::FindNodeEntityByNodeId(SceneNodeService, nodeId);
    if (!entity)
    {
        return std::nullopt;
    }
    return entt::to_integral(*entity);
}

// 放弃收口:只做三件事 —— 推 tip、踢线、关连接(I6)。
// ExitGame 与 login 断线租约仍走断线回调:会话的指向在失败窗口里没被改写(I5),ExitGame 落到上一次成功
// 提交的节点,与"路由从未到达 gate 时断线"是同一条既有路径;那边怎么处置由它自己的退出 / 冻结逻辑决定,
// gate 不做任何假设,本函数也不引入新的解冻或销毁入口。
void GiveUpSceneEntry(const SessionId sessionId, SessionInfo &session, const Clock::time_point now)
{
    if (!session.pendingSceneEntry.has_value())
    {
        return;
    }
    const PendingSceneEntry entry = *session.pendingSceneEntry;
    session.pendingSceneEntry.reset();
    ++tlsCounters.gaveUp;

    // 每个会话一条、不采样:这是玩家被踢的唯一服务端证据。量级等于故障期间的进场数。
    LOG_ERROR << "[SceneEntry] giving up session_id=" << sessionId
              << " player_id=" << session.playerId
              << " entry_player_id=" << entry.target.playerId
              << " target_node_id=" << entry.target.nodeId
              << " scene_id=" << entry.target.sceneId
              << " attempts=" << entry.attempts
              << " first_failure=" << SceneForwardResultName(entry.firstFailure)
              << " last_failure=" << SceneForwardResultName(entry.lastFailure)
              << " elapsed_ms=" << ElapsedMs(entry.routedAt, now)
              << ",推送 kEnterSceneFailed 并踢线、关闭会话";

    const auto conn = session.conn.lock();
    if (!conn || !conn->connected())
    {
        return;
    }

    // 与"进场没成 = 3023"契约一致(login 显式拒绝、scene 转发后拒绝都用同一个码),客户端走同一条收口路径:
    // 等进场期间 IsEnterFailureTip 命中提前收口,34 的 reason 由 DescribeKickReason 判为传送失败码。
    // 三条消息走同一条 TCP、同一线程,天然有序:tip → 34 → 关写端。
    RpcClientSessionHandler::SendTipToClient(conn, kEnterSceneFailed);

    GameKickPlayerRequest kickMsg;
    kickMsg.mutable_reason()->set_id(kEnterSceneFailed);
    MessageContent mc;
    mc.set_message_id(SceneClientPlayerCommonKickPlayerMessageId);
    mc.set_serialized_message(kickMsg.SerializeAsString());
    GetGateCodec().send(conn, mc);

    // shutdown 只关写端,等发送缓冲排空后才真正 shutdownWrite,不走 handleClose,所以不会在本调用栈里
    // erase 会话;forceCloseWithDelay 是 muduo 的弱回调,兜住不配合挥手的客户端。
    conn->shutdown();
    conn->forceCloseWithDelay(gate_scene_entry::kGiveUpForceCloseDelaySeconds);
}

// 对会话上的欠账做一次尝试并按结果收口。不会 erase 会话(见 GiveUpSceneEntry 的说明),
// 所以调用方手里的会话引用在返回后仍然有效。
void AttemptAndSettle(const SessionId sessionId, SessionInfo &session, const Clock::time_point now)
{
    if (!session.pendingSceneEntry.has_value())
    {
        tlsSessionsWithPendingEntry.erase(sessionId);
        return;
    }

    // 客户端连接不可用(断开中、被顶号 kick 的 shutdown、LeaseExpired 的 forceClose、我们自己放弃之后):
    // 撤销欠账,不转发、不提交。对每一次尝试都生效,包括路由刚到时那一次 —— 给一条正在关闭的连接转发进场
    // 只会制造一个马上要退出的实体,还可能把在用实体从新会话手里抢走(scene 侧 PlayerEnterGameNode
    // 在任何 epoch 判定之前先摘会话)。
    const auto clientConn = session.conn.lock();
    if (!clientConn || !clientConn->connected())
    {
        session.pendingSceneEntry.reset();
        tlsSessionsWithPendingEntry.erase(sessionId);
        ++tlsCounters.sessionClosed;
        return;
    }

    // 尝试成功后欠账会被清掉,日志要用的字段先拷一份。
    const PendingSceneEntry before = *session.pendingSceneEntry;

    const auto outcome = gate_scene_route::AttemptPendingSceneEntry(
        session, now, ResolveSceneNodeEntity,
        [&](const uint32_t enterType, const uint64_t nodeEntityId, const uint64_t sceneId)
        {
            return ForwardPlayerToScene(sessionId, enterType, nodeEntityId, session, sceneId);
        });

    switch (outcome)
    {
    case gate_scene_route::SceneEntryAttempt::kApplied:
        tlsSessionsWithPendingEntry.erase(sessionId);
        if (before.attempts > 0)
        {
            ++tlsCounters.recovered;
            if (ShouldLogSampled(tlsRecoveredLogSeq))
            {
                LOG_INFO << "[SceneEntry] forward recovered session_id=" << sessionId
                         << " player_id=" << session.playerId
                         << " target_node_id=" << before.target.nodeId
                         << " scene_id=" << before.target.sceneId
                         << " attempts=" << (before.attempts + 1)
                         << " last_failure=" << SceneForwardResultName(before.lastFailure)
                         << " elapsed_ms=" << ElapsedMs(before.routedAt, now)
                         << " recovered_total=" << tlsCounters.recovered << " (sampled 1/64)";
            }
        }
        return;

    case gate_scene_route::SceneEntryAttempt::kRetryLater:
        tlsSessionsWithPendingEntry.insert(sessionId);
        if (before.attempts == 0)
        {
            ++tlsCounters.deferred;
            if (ShouldLogSampled(tlsDeferredLogSeq))
            {
                const auto &entry = *session.pendingSceneEntry;
                LOG_WARN << "[SceneEntry] forward deferred session_id=" << sessionId
                         << " player_id=" << session.playerId
                         << " target_node_id=" << entry.target.nodeId
                         << " scene_id=" << entry.target.sceneId
                         << " failure=" << SceneForwardResultName(entry.lastFailure)
                         << " retry_in_ms=" << ElapsedMs(now, entry.nextAttemptAt)
                         << " deferred_total=" << tlsCounters.deferred << " (sampled 1/64)";
            }
        }
        return;

    case gate_scene_route::SceneEntryAttempt::kGiveUp:
        tlsSessionsWithPendingEntry.erase(sessionId);
        GiveUpSceneEntry(sessionId, session, now);
        return;

    case gate_scene_route::SceneEntryAttempt::kPlayerMismatch:
        // 欠账属于另一名角色(同一条连接上换过角色):撤销欠账、不踢线,连接属于当前角色。若这条连接之后
        // 真的换回路由所属的角色,它的 BindSession 会凭 lastSceneRoute 重建欠账;当前角色则等它自己的路由。
        tlsSessionsWithPendingEntry.erase(sessionId);
        session.pendingSceneEntry.reset();
        ++tlsCounters.playerChanged;
        if (ShouldLogSampled(tlsPlayerChangedLogSeq))
        {
            LOG_WARN << "[SceneEntry] route belongs to another player, entry cancelled session_id=" << sessionId
                     << " session_player_id=" << session.playerId
                     << " entry_player_id=" << before.target.playerId
                     << " target_node_id=" << before.target.nodeId
                     << " scene_id=" << before.target.sceneId
                     << " player_changed_total=" << tlsCounters.playerChanged << " (sampled 1/64)";
        }
        return;
    }
}

// 计数或积压有变化才打:空闲的 gate 不刷日志。
void MaybeLogSummary(const Clock::time_point now)
{
    if (now < tlsNextSummaryAt)
    {
        return;
    }
    tlsNextSummaryAt = now + kSummaryInterval;

    const auto pending = tlsSessionsWithPendingEntry.size();
    if (tlsCounters == tlsLastSummaryCounters && pending == tlsLastSummaryPending)
    {
        return;
    }
    tlsLastSummaryCounters = tlsCounters;
    tlsLastSummaryPending = pending;

    LOG_INFO << "[SceneEntry] deferred=" << tlsCounters.deferred
             << " recovered=" << tlsCounters.recovered
             << " gave_up=" << tlsCounters.gaveUp
             << " superseded=" << tlsCounters.superseded
             << " session_closed=" << tlsCounters.sessionClosed
             << " player_changed=" << tlsCounters.playerChanged
             << " links_ready=" << tlsCounters.linksReady
             << " pending=" << pending;
}
} // namespace

namespace gate_scene_entry
{
void OnRouteDecision(const SessionId sessionId, SessionInfo &session, const SceneRouteTarget &target,
                     const Clock::time_point now)
{
    session.lastSceneRoute = target;
    if (session.pendingSceneEntry.has_value())
    {
        ++tlsCounters.superseded;
    }
    // 以最近一次路由决策为准,整体替换并重新计时。
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(target, now);
    AttemptAndSettle(sessionId, session, now);
}

void OnLoginTypeBound(const SessionId sessionId, SessionInfo &session, const Clock::time_point now)
{
    if (session.pendingEnterGsType == 0)
    {
        return;
    }
    if (!session.pendingSceneEntry.has_value())
    {
        auto entry = gate_scene_route::EntryForLateLoginBinding(session, now);
        if (!entry.has_value())
        {
            // 还没收到本角色的路由:登录类型留在会话里,等路由到达时由 OnRouteDecision 一并转发。
            return;
        }
        session.pendingSceneEntry = std::move(*entry);
    }
    AttemptAndSettle(sessionId, session, now);
}

void OnSceneLinkHandshaken(const muduo::net::TcpConnectionPtr &conn, const std::string &nodeUuid,
                           const Clock::time_point now)
{
    const auto nodeEntity = NodeUtils::FindNodeEntityByUuid(SceneNodeService, nodeUuid);
    if (!nodeEntity)
    {
        LOG_WARN << "[SceneEntry] handshake reply for unknown scene node, ignored uuid=" << nodeUuid;
        return;
    }

    auto &sceneRegistry = tlsNodeContextManager.GetRegistry(SceneNodeService);
    // 应答所走的连接必须就是该节点 RpcClient 的**当前**连接:迟到的旧连接应答不能给新连接盖章,
    // 否则新连接会在 scene 还没为它挂 RpcSession 时被当成就绪。
    const auto *rpcClient = sceneRegistry.try_get<RpcClientPtr>(*nodeEntity);
    if (!conn || !rpcClient || !*rpcClient || (*rpcClient)->GetConnection() != conn)
    {
        LOG_WARN << "[SceneEntry] handshake reply not on the node's current link, ignored uuid=" << nodeUuid;
        return;
    }

    // 握手应答路径,不是逐帧路径;重连后的再次握手就是要覆盖旧章,所以用 emplace_or_replace。
    sceneRegistry.emplace_or_replace<gate_scene_entry::SceneLinkHandshakeComp>(
        *nodeEntity, gate_scene_entry::SceneLinkHandshakeComp{conn});
    ++tlsCounters.linksReady;

    const auto *nodeInfo = sceneRegistry.try_get<NodeInfo>(*nodeEntity);
    LOG_INFO << "[SceneEntry] scene link ready node_id=" << (nodeInfo ? nodeInfo->node_id() : 0u)
             << " uuid=" << nodeUuid;
    if (!nodeInfo)
    {
        return;
    }

    // 以该节点为目标的欠账提前到下一轮扫描(≤250ms)补发;不在应答处理器里同步转发。
    const auto nodeId = nodeInfo->node_id();
    auto &sessions = tlsSessionManager.sessions();
    for (const auto sessionId : tlsSessionsWithPendingEntry)
    {
        const auto it = sessions.find(sessionId);
        if (it == sessions.end() || !it->second.pendingSceneEntry.has_value())
        {
            continue;
        }
        auto &entry = *it->second.pendingSceneEntry;
        if (entry.target.nodeId == nodeId && entry.nextAttemptAt > now)
        {
            entry.nextAttemptAt = now;
        }
    }
}

void RetryDueSceneEntries(const Clock::time_point now)
{
    MaybeLogSummary(now);
    if (tlsSessionsWithPendingEntry.empty())
    {
        return;
    }

    // 拷快照再逐个重新 find:AttemptAndSettle 会增删索引项。
    const std::vector<SessionId> snapshot(tlsSessionsWithPendingEntry.begin(), tlsSessionsWithPendingEntry.end());
    auto &sessions = tlsSessionManager.sessions();
    for (const auto sessionId : snapshot)
    {
        const auto it = sessions.find(sessionId);
        if (it == sessions.end() || !it->second.pendingSceneEntry.has_value())
        {
            // 会话已 erase(断线)或 session_id 已被新会话复用:欠账随旧会话一起没了,这里只清索引并计入
            // session_closed,让汇总行的 pending 与计数对得上。
            tlsSessionsWithPendingEntry.erase(sessionId);
            ++tlsCounters.sessionClosed;
            continue;
        }
        if (it->second.pendingSceneEntry->nextAttemptAt > now)
        {
            continue;
        }
        AttemptAndSettle(sessionId, it->second, now);
    }
}
} // namespace gate_scene_entry

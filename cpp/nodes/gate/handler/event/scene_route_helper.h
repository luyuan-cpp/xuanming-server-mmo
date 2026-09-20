#pragma once

#include <cstdint>
#include <optional>

#include "proto/common/base/node.pb.h"
#include "services/gate/session/comp/session_info_comp.h"

namespace gate_scene_route
{
// 本次路由相对会话原先的 scene 节点指向有没有换节点。ApplyRoute 是不碰节点注册表的纯逻辑,自己无从
// 知道,由 RebindSceneNode 在改写会话节点指向的同时得出。用枚举而不是 bool,调用点读得出传的是什么。
enum class SceneNodeChange : uint8_t
{
    kUnchanged,
    kChanged,
};

// 把会话的 scene 节点指向改写为 targetNodeEntityId,并返回相对旧指向有没有换节点。
// "先比较、后覆盖"收在一个函数里:覆盖之后就分不出"同节点的重复路由"与"同 scene_id 换了节点",
// 调用方不可能再把顺序写反。比的是 gate 上的节点实体号而不是 node_id:同 node_id 的进程重启后是新实体,
// 那边同样没有该玩家。
//
// 旧指向无效一律算 kChanged,不区分"从未绑定"与"绑定过、但节点已被摘除":scene 节点下线时
// OnNodeRemoveEventHandler / ResolveSessionTargetNode(client_message_processor.cpp)会把指向置为
// kInvalidEntityId,随后 node_gone 迁频道(沿用原 scene_id)或同 node_id 重启后的改派必须重新进场,
// 若把无效指向当"未变化",这条路由会被当成重复事件吞掉。首登(从未收到过路由)不会因此误转发:
// ApplyRoute 要求 session.sceneId != 0。节点以新实体重新注册、而旧进程仍持有玩家实体时会多转发一次
// enterType=0:scene 侧 PlayerEnterGameNode 对已有实体走 EnterScene 复用路径(player_lifecycle.cpp)——
// session 重绑同号、HandleEnterScene 对"已在目标场景"幂等早退、3.2 交接就地收尾有 epoch 复核、
// 0 不置登录态也不触发登录事件。唯一的状态副作用是 3.1 会顺带摘掉 PlayerSceneChangeInFlightComp,
// 即提前放开一次在途换图的"切换中"闸;静态读下来迟到应答找不到在途组件是静默 no-op,未经运行验证。
inline SceneNodeChange RebindSceneNode(SessionInfo& session, const uint64_t targetNodeEntityId)
{
    const auto change = session.GetEntityId(common::base::SceneNodeService) != targetNodeEntityId
                            ? SceneNodeChange::kChanged
                            : SceneNodeChange::kUnchanged;
    session.SetEntityId(common::base::SceneNodeService, targetNodeEntityId);
    return change;
}

// LOGIN_NONE(0) 在换图时仍表示必须通知 scene,因此用 optional 区分无需入场。
// 发送前置检查失败时不消费路由/登录类型。下面第 1、2 支的判定只读会话自身状态,同事件重投仍可恢复;
// 第 3 支靠调用方传入的 nodeChange:调用方若在转发之前就已提交节点指向(RoutePlayerEventHandler 经
// RebindSceneNode 正是如此),重投时拿到的是 kUnchanged,恢复不了——见 gate_event_handler.cpp 的已知局限。
//
// 要不要向 scene 转发进场,三个条件任一成立即转发:
//   1. pendingEnterGsType 非 0:登录入场,以该登录类型转发;
//   2. scene_id 变了:换图 / 换线,以 LOGIN_NONE(0) 转发;
//   3. scene_id 没变但**节点变了**:同样以 LOGIN_NONE(0) 转发。
// 三者都不成立才是重复事件,不转发(幂等)。
//
// 第 3 条不能省:同一个 scene_id 会出现在不同节点上。scene_manager 的 world rebalance
// (go/scene_manager/internal/logic/world_rebalance.go 的 migrateWorldChannel)迁频道时沿用原 scene_id,
// 只改写 scene->node 映射,随后对旧节点 DestroyScene。规划读到"空频道"与改写映射之间若有玩家被预占
// 进旧节点,旧节点排空时会存盘、写 handoff 标记、销毁本地实体并重新请求 EnterScene,scene_manager 可能
// 挑回已迁到新节点的同一个 scene_id。此时只按 scene_id 判定会把这条路由当成重复事件吞掉:会话指向了
// 新节点,新节点上却没有该玩家的实体,旧节点上的也已销毁,玩家在线但哪里都不存在,只能重登。
// 目标节点上无实体时,enterType=0 的进场请求走异步加载后 EnterScene(scene_handler.cpp PlayerEnterGameNode),
// 0 只表示"不置登录态、不触发 PlayerLoginEvent",与跨节点换图是同一条路径。
//
// 2、3 两条都要求 session.sceneId != 0:会话从未收到过路由时没有"已入场的玩家"可言,仍等登录类型
// 到达后按第 1 条入场。注意 sceneId != 0 只表示"此前收到过路由",不保证已入场:路由先于 BindSession
// 到达时下面也会提交 sceneId 而不转发。此时若再来一条换了节点(或换了 scene_id)的路由,会先以 0 转发
// 一次,登录类型随后由 BindSession 补发;scene 侧对这个顺序安全(pending 进场上下文取最新一份,
// 后到的登录类型仍会置登录态并触发登录事件)。
template <typename ForwardEntry>
bool ApplyRoute(SessionInfo& session, const uint64_t targetSceneId, const SceneNodeChange nodeChange,
                ForwardEntry&& forward)
{
    if (targetSceneId == 0) return false;
    std::optional<uint32_t> enterType;
    if (session.pendingEnterGsType != 0)
        enterType = session.pendingEnterGsType;
    else if (session.sceneId != 0 &&
             (session.sceneId != targetSceneId || nodeChange == SceneNodeChange::kChanged))
        enterType = 0;
    if (enterType.has_value() && !forward(*enterType)) return false;
    session.sceneId = targetSceneId;
    if (enterType.has_value()) session.pendingEnterGsType = 0;
    return true;
}
}

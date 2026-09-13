#pragma once

#include <optional>

#include "services/gate/session/comp/session_info_comp.h"

namespace gate_scene_route
{
// LOGIN_NONE(0) 在换图时仍表示必须通知 scene,因此用 optional 区分无需入场。
// 发送前置检查失败时不消费路由/登录类型,同事件重投仍可恢复。
template <typename ForwardEntry>
bool ApplyRoute(SessionInfo& session, const uint64_t targetSceneId, ForwardEntry&& forward)
{
    if (targetSceneId == 0) return false;
    std::optional<uint32_t> enterType;
    if (session.pendingEnterGsType != 0)
        enterType = session.pendingEnterGsType;
    else if (session.sceneId != 0 && session.sceneId != targetSceneId)
        enterType = 0;
    if (enterType.has_value() && !forward(*enterType)) return false;
    session.sceneId = targetSceneId;
    if (enterType.has_value()) session.pendingEnterGsType = 0;
    return true;
}
}

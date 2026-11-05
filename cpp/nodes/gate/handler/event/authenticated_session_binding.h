#pragma once

#include "proto/contracts/kafka/gate_event.pb.h"
#include "services/gate/session/comp/session_info_comp.h"

namespace gate_authenticated_session
{
// 仅 gate 主 EventLoop 调用。外层先核 Kafka target_instance_id 及 LeaveGame 栅栏。
// 账号字节原样保留；新连接代次、缺失字段、旧登录及同代身份冲突均不能建立认证态。
inline bool Bind(SessionInfo& session, const contracts::kafka::BindSessionEvent& event)
{
    if (event.session_incarnation() == 0 || event.session_incarnation() != session.incarnation ||
        event.authenticated_account().empty() || event.authenticated_account().size() > 256 ||
        event.owner_revision() == 0 || event.player_id() == 0 || event.player_id() == kInvalidGuid ||
        event.login_generation() == 0 || !session.loginSucceeded)
    {
        return false;
    }
    if (session.playerId == event.player_id())
    {
        if (event.login_generation() < session.loginGeneration)
        {
            return false;
        }
        if (event.login_generation() == session.loginGeneration && !session.authenticatedAccount.empty() &&
            (session.authenticatedAccount != event.authenticated_account() ||
             session.authenticatedOwnerRevision != event.owner_revision()))
        {
            return false;
        }
    }
    session.authenticatedAccount = event.authenticated_account();
    session.authenticatedOwnerRevision = event.owner_revision();
    return true;
}
} // namespace gate_authenticated_session

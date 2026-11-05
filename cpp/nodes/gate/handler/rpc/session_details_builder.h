#pragma once

// gate 递给 Go 服务的会话身份 SessionDetails(gRPC metadata x-session-detail-bin)的唯一构造点(纯函数)。
// 契约:docs/design/client-access-band-routing.md D60 / F8 / K9。
//
// 调用方(client_session_handler.cpp 的 BuildSessionDetails 薄包装,补上 gNode 的节点号与实例 uuid):
//   - 直连转发 HandleGrpcNodeMessage(login / match);
//   - 路由转发 HandleRouterForward → gate_lane::Admit(lane 在此之上只写会话代次与 gate_call_seq);
//   - 断线通知 HandleConnectionDisconnection(直连发 login.Disconnect,路由合成 58 号走 Forward)。
// 三条路径共用这一份,任何身份规则只在这里改:曾经直连是内联手写的一份,只改其中一个 = 一种模式下 login 永远读到 0。
//
// player_id 归一:会话尚未绑定角色时 SessionInfo.playerId 是 kInvalidGuid(UINT64_MAX),而 Go 各服务、路由服都只认
// 0 = 未绑角色。以前只有路由 lane 归一,直连模式原样带出 UINT64_MAX,未绑角色的会话会被当成玩家 18446744073709551615
// 照常执行业务。
//
// 纯函数、零状态,单独成文件是为了在 routing_identity_test 里直接测,不把 gNode、会话表、RPC 注册链拖进测试工程
// (与 login_admission.h 同一做法)。

#include <cstdint>
#include <string>

#include "engine/core/type_define/type_define.h"
#include "proto/common/base/session.pb.h"
#include "services/gate/session/comp/session_info_comp.h"

namespace gate_session_details
{

// kInvalidGuid → 0,其余原样。幂等:已归一的值再归一不变,lane 复用本函数做纵深防御不会改写真实角色。
inline Guid NormalizePlayerId(Guid playerId)
{
	return playerId == kInvalidGuid ? 0 : playerId;
}

// 排队请求只能继续使用受理时的原身份，离场、换角或换连接之后不可再签发它。
// 未携带在线账号的登录/选角请求由各自准入负责，不能在此补上后来才获得的身份。
inline bool MatchesAuthenticatedBinding(const SessionDetails& admitted, const SessionInfo& session)
{
	return admitted.authenticated_account().empty() ||
		(admitted.session_incarnation() == session.incarnation && admitted.player_id() == session.playerId &&
		 admitted.authenticated_account() == session.authenticatedAccount &&
		 admitted.owner_revision() == session.authenticatedOwnerRevision);
}

// playerId:会话当前绑定的角色,直接传 SessionInfo.playerId 即可(未绑定的 kInvalidGuid 在这里归一成 0;
// proto3 隐式存在,0 与不设置在线上是同一形状)。
// gateNodeId / gateInstanceId:gate 自身的节点号与启动 uuid(Kafka 路由与防僵尸)。
// session 非空时带上重定向票据的绑定信息(CZ-8)、会话代次(client-request-ordering.md §4.8:login 按
// (gate_instance_id, 会话代次) 认登录态与会话栅栏)与原归属修订。现有调用方都传会话。
inline SessionDetails Build(SessionId sessionId, Guid playerId, uint32_t gateNodeId, const std::string &gateInstanceId,
							const SessionInfo *session)
{
	SessionDetails sessionDetails;
	sessionDetails.set_session_id(sessionId);
	sessionDetails.set_player_id(NormalizePlayerId(playerId));
	sessionDetails.set_gate_node_id(gateNodeId);
	sessionDetails.set_gate_instance_id(gateInstanceId);
	if (session != nullptr)
	{
		sessionDetails.set_ticket_player_id(session->ticketPlayerId);
		sessionDetails.set_ticket_target_zone_id(session->ticketTargetZoneId);
		sessionDetails.set_session_incarnation(session->incarnation);
		sessionDetails.set_owner_revision(session->ownerRevision);
		if (!session->authenticatedAccount.empty())
		{
			sessionDetails.set_authenticated_account(session->authenticatedAccount);
			sessionDetails.set_owner_revision(session->authenticatedOwnerRevision);
		}
	}
	return sessionDetails;
}

} // namespace gate_session_details

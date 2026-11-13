#include "client_entry_admission.h"

#include "client_request_lane.h"
#include "login_admission.h"

#include "message_limiter/illegal_packet_counter.h"
#include "muduo/base/Logging.h"
#include "proto/common/base/message.pb.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "services/gate/session/comp/session_info_comp.h"

namespace gate_client_entry
{

bool RejectUnauthorizedMessageId(const SessionId sessionId, SessionInfo &session, const muduo::net::TcpConnectionPtr &conn,
								 const uint32_t messageId)
{
	// 只能由 gate 合成的号(Disconnect 等)单独判,处置与未授权号相同,不依赖生成的白名单是否已经把它摘掉。
	if (messageId < gRpcMethodRegistry.size() && IsClientMessageId(messageId) &&
		!gate_login_admission::IsGateSynthesizedOnly(messageId))
	{
		return false;
	}
	LOG_DEBUG << "Invalid or unauthorized message ID: " << messageId << ", session_id: " << sessionId;
	if (IllegalPacketCounter::RegisterAndShouldKill(session.illegalPacketCount))
	{
		LOG_WARN << "Session illegal-packet threshold exceeded (bad message_id) — forceClose."
				 << " count=" << session.illegalPacketCount << " message_id=" << messageId;
		conn->forceClose();
	}
	return true;
}

uint32_t LoginTransitionRejectTip(const SessionId sessionId, const SessionInfo &session, const ClientRequest &request)
{
	if (request.message_id() != ClientPlayerLoginEnterGameMessageId)
	{
		return 0;
	}
	const uint32_t rejectTip = gate_login_admission::EnterGameRoleSwitchRejectTip(session, request.body());
	if (rejectTip != 0)
	{
		static uint64_t roleSwitchRejectedCount = 0;
		if ((roleSwitchRejectedCount++ & 0x3FF) == 0)
		{
			LOG_WARN << "EnterGame rejected: session is already bound to a role (sampled)"
					 << " latest_session_id=" << sessionId << " latest_tip=" << rejectTip
					 << " rejected_total=" << roleSwitchRejectedCount;
		}
	}
	return rejectTip;
}

bool RejectLoginTransitionBeforeDirectSend(const SessionId sessionId, SessionInfo &session, const ClientRequest &request)
{
	const uint32_t rejectTip = LoginTransitionRejectTip(sessionId, session, request);
	if (rejectTip == 0)
	{
		return false;
	}
	gate_lane::SendEnvelope(session, request.id(), request.message_id(), rejectTip);
	return true;
}

} // namespace gate_client_entry

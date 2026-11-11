#include "gate_redirect_delivery.h"

#include "gate_codec.h"
#include "handler/rpc/client_request_lane.h"
#include "proto/common/base/message.pb.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "session/manager/session_manager.h"
#include "session/system/session_identity_fence.h"

namespace gate_redirect
{

gate_drain_migration::PushOutcome PushRedirect(SessionId sessionId, Guid ticketPlayerId, const RedirectToGateNotify &notify,
	gate_drain_migration::SteadyTime now)
{
	using gate_drain_migration::PushOutcome;

	auto &sessions = tlsSessionManager.sessions();
	const auto it = sessions.find(sessionId);
	if (it == sessions.end())
	{
		return PushOutcome::kSessionGone;
	}
	SessionInfo &session = it->second;
	if (gate_session_fence::ClassifyPush(session.playerId, ticketPlayerId) == gate_session_fence::PushVerdict::kDrop)
	{
		return PushOutcome::kIdentityMismatch;
	}
	const auto conn = session.conn.lock();
	if (!conn || !conn->connected())
	{
		return PushOutcome::kConnectionGone;
	}

	// 会话转去别的 gate:丢弃还没发出的路由排队项,之后到达的路由类请求回带请求 id 的 1003;在途那一条的回包照常下发。
	// 不这样做,旧连接上排着的请求会继续发出,与新 gate 上新会话的请求交错执行。与 Java 版 ClientDispatcher.enterRedirected 一致。
	gate_lane::OnSessionRedirected(session);

	MessageContent content;
	content.set_message_id(SceneClientPlayerCommonRedirectToGateMessageId);
	content.set_serialized_message(notify.SerializeAsString());
	GetGateCodec().send(conn, content);

	gate_drain_migration::NoteRedirected(sessionId, session, now);
	return PushOutcome::kPushed;
}

} // namespace gate_redirect

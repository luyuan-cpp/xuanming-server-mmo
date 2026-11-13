#include "gate_redirect_delivery.h"

#include "gate_codec.h"
#include "handler/rpc/client_request_lane.h"
#include "handler/rpc/client_scene_lane.h"
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

	// 会话转去别的 gate:还没发出的路由排队项不再发出,逐条回带请求 id 的 1003(确定未执行,到新 gate 重试安全),之后到达的
	// 路由类请求同样回 1003;在途那一条的回包照常下发。
	// 不这样做,旧连接上排着的请求会继续发出,与新 gate 上新会话的请求交错执行。
	// 必须排在下面推 124 之前:回执与 124 走同一条连接、同一线程顺序写出,客户端先收齐回执再换连接;排在 124 之后的回执
	// 客户端可能已经读不到,只能等 15 s 请求超时。
	// Java 版同形(2026-11-11 两版同批定案,O4 / O6 / O9):ClientDispatcher.enterRedirected 先逐条回排队项再推 124,
	// 之后到达的请求由 onRequest 回带 id 的 1003。这里置的重定向标记也覆盖不经 lane 的请求:scene TCP 与 gate 本地观战
	// 在 client_session_handler.cpp 的 RejectedBehindQueuedLeaveGame 里见到已重定向即回带 id 的 1003(两种模式都生效);
	// 只有直连模式的 gRPC 请求(HandleGrpcNodeMessage)按 O7 维持现状、不在 gate 拦截。
	gate_lane::OnSessionRedirected(session);
	// 场景 lane 同理(docs/design/client-scene-request-handoff-hold.md §4.6):跨 zone 传送时玩家正在离开原场景节点,
	// 还排着或在等重投的 scene 请求等不到受理,同样先逐条回 1003 再推 124。
	gate_scene_lane::OnSessionRedirected(sessionId, session);

	MessageContent content;
	content.set_message_id(SceneClientPlayerCommonRedirectToGateMessageId);
	content.set_serialized_message(notify.SerializeAsString());
	GetGateCodec().send(conn, content);

	gate_drain_migration::NoteRedirected(sessionId, session, now);
	return PushOutcome::kPushed;
}

} // namespace gate_redirect

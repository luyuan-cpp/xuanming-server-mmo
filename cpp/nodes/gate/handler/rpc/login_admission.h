#pragma once

// gate 侧「本连接已成功 Login」闸的纯判定(docs/design/client-request-ordering.md §7.2 第 2 条;
// docs/design/data-overwrite-rollback-audit-20261018.md §2.3)。
//
// 背景:login 的登录态原先只按 uint32 session_id 建键。session_id = node_id<<17 | 进程内序号,gate 被 kill -9 后以同一
// node_id 重启、序号从 0 开始,新连接会撞上旧进程同号连接 30min TTL 内仍有效的登录态;不先 Login、直接发 CreatePlayer /
// EnterGame,就能在别人的账号下建角、进别人的角色。主修在 login(登录态改按 gate_instance_id + 会话代次建键);
// 这里是 gate 侧的第二道:CreatePlayer / EnterGame / LeaveGame 发出前,要求本连接已收到过成功的 Login 回包。
// 第二道在滚动升级时不可少:新 gate 与旧 login 并存时,旧 login 仍按 session_id 读登录态,只有 gate 挡得住。
//
// 判定时刻是「发出」而不是「收包」:路由 lane 允许客户端不等回包连发 Login → EnterGame(client-request-ordering.md §4,
// ordering-smoke O7),EnterGame 要等 Login 完成之后才发出,那时 Login 的回包已经处理完。直连模式没有 lane,收包即发出,
// 连发的 EnterGame 在 Login 回包之前到达就会被拒(直连本来就不保序;真实客户端要先从 Login 回包的角色列表拿 player_id)。
//
// 同一文件还放着客户端入口的另外两道 login 相关判定,调用点都在 client_session_handler.cpp:IsGateSynthesizedOnly 在
// DispatchClientRpcMessage 收包时判(两种模式共用);EnterGameRoleSwitchRejectTip 随 EnterGame 发出时判(LoginTransitionRejectTip:
// 直连模式收包即发出,路由模式由 lane 的发出钩子在轮到它时判,排在前面的 LeaveGame 那时已经生效)。
//
// 纯函数、零状态,单独成文件是为了在 routing_identity_test 里直接测,不把会话表、连接、RPC 注册链拖进测试工程。

#include <cstdint>
#include <string>

#include "proto/common/base/message.pb.h"
#include "proto/login/login.pb.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "services/gate/session/comp/session_info_comp.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/login_error_tip.pb.h"

namespace gate_login_admission
{

// 发出前要求本连接已成功 Login 的方法。Login 本身、RefreshToken(凭 refresh token 自证)不在其中;Disconnect 也不在,
// 它只由 gate 合成、客户端发不进来(见 IsGateSynthesizedOnly)。login 侧的 CreatePlayer / EnterGame 读的就是本连接的登录态;
// LeaveGame 在 login 侧不读登录态(gate 会为活着的连接续租登录态,但续租连续失败超过 SessionExpireMin 时它仍会过期,
// 读了会误拒正常登出),只能在 gate 这一层要求。
inline bool RequiresSuccessfulLogin(uint32_t messageId)
{
	return messageId == ClientPlayerLoginCreatePlayerMessageId ||
		   messageId == ClientPlayerLoginEnterGameMessageId ||
		   messageId == ClientPlayerLoginLeaveGameMessageId;
}

// LoginResponse 表示成功:没有 error_message,或 error_message.id == 0(login 成功时不填;填了 0 也按成功)。
inline bool IsLoginResponseSuccess(const loginpb::LoginResponse &response)
{
	return !response.has_error_message() || response.error_message().id() == 0;
}

// 一条 Login 回包是否表示成功。两种转发模式的回包形状不同:
//   路由模式:路由服回 MessageContent —— 信封 error_message 非 0(路由服或下游拒绝)即失败,否则解 body;
//   直连模式:生成的 gRPC 客户端回的就是 LoginResponse。
// body 解析失败或其他回包类型一律按失败(fail-closed):宁可让客户端重登,也不能凭一个看不懂的回包放行。
inline bool IsLoginReplySuccess(const ::google::protobuf::Message &reply)
{
	if (reply.GetDescriptor() == MessageContent::descriptor())
	{
		const auto &content = static_cast<const MessageContent &>(reply);
		if (content.has_error_message() && content.error_message().id() != 0)
		{
			return false;
		}
		loginpb::LoginResponse response;
		if (!response.ParseFromString(content.serialized_message()))
		{
			return false;
		}
		return IsLoginResponseSuccess(response);
	}
	if (reply.GetDescriptor() == loginpb::LoginResponse::descriptor())
	{
		return IsLoginResponseSuccess(static_cast<const loginpb::LoginResponse &>(reply));
	}
	return false;
}

// 只能由 gate 合成、客户端不得发起的消息号(proto 都标 CLIENT_RPC_SERVER_ONLY),不经客户端入口发出:
//   - Disconnect:gate 在连接关闭时自己合成(HandleConnectionDisconnection 直接发给 login 或路由服),login 据此关闭会话栅栏、
//     清登录态、给绑定的角色挂断线租约。客户端自己发等于在连接还活着时把 login 侧的会话状态切成「已断线」,gate 却还当它在线。
//   - RequestGateMigration:gate 排空时自己合成(drain_migration.cpp 经 SendGateMigrationRequest 发),应答只给 gate 用来推 124
//     (docs/design/gate-drain-session-migration.md §4.3)。客户端自己发就能绕开 gate 的迁移限速与选人条件,直接拿到一张换 gate 的票。
//   - RenewSession:gate 为仍然连着的连接定期合成(login_session_renewal.cpp 经 SendLoginSessionRenewRequest 发),应答只给 gate
//     用来排下一次续租(docs/design/gate-online-identity.md §10.4)。续租的节奏由 gate 按 login 给的间隔控制,不接受客户端自己发。
//
// 客户端入口按「未知 / 未授权消息号」拒绝它们(计非法包、不回包)。这道判定手写、不依赖生成物:生成的上行白名单
// IsClientMessageId 按服务收号,它们所在的 ClientPlayerLogin 标了 OptionIsClientProtocolService,曾经一直在白名单里;
// 生成器按方法形态收口之后(docs/design/client-rpc-callback.md §7 R4)白名单本身也不再收它们,这里照样拦,
// 生成物没重新生成或生成器回退时同样挡得住。
inline bool IsGateSynthesizedOnly(uint32_t messageId)
{
	return messageId == ClientPlayerLoginDisconnectMessageId ||
		   messageId == ClientPlayerLoginRequestGateMigrationMessageId ||
		   messageId == ClientPlayerLoginRenewSessionMessageId;
}

// 会话当前绑定的角色是否有效:BindSession(或先到的路由)把角色写进会话之后、客户端 LeaveGame 之前。
// LeaveGame 之后 gate 不清 playerId(换角前的旧角色留着,用来认出旧登录链迟到的 Bind / 路由),但 ResetSceneRoute 置了
// sceneRouteResetPending,这段时间不算绑定:客户端在选角,下一次 EnterGame 可以是任何角色。
// 同一角色重进时 sceneRouteResetPending 要等进场真正交给 scene 才清(scene_route_helper.h ApplyRoute),这段时间同样不算,
// 由 login 会话栅栏判(它在发 Bind 之前已经绑定)。
inline bool HasBoundRole(const SessionInfo &session)
{
	return session.playerId != kInvalidGuid && session.playerId != 0 && !session.sceneRouteResetPending;
}

// 客户端 EnterGame 的换角闸:会话已绑定角色、请求的却是另一个角色时不转发。纵深防御:主判定在 login 会话栅栏
// (go/login/internal/logic/pkg/sessionfence CheckEnterGame / BindPlayer),这一道不依赖 login 侧记录的寿命,
// gate 的会话与连接同寿命(docs/design/data-overwrite-rollback-audit-20261018.md §4 login 第 2 条)。
// 没 LeaveGame 就换角,一条连接会绑上两个角色:前一个角色停在 ONLINE、断线时也收不到断线租约。
//
// 返回 0 = 放行;非 0 = 不转发,回带请求 id 的信封,值是 tip:
//   - 会话已绑定角色、请求的是另一个角色 → kLoginInProgress(login 栅栏拒绝换角回的同一个码);
//   - 会话已绑定角色、请求体解析不出 → kRequestMessageParseError(读不出角色就证明不了是同一个,fail-closed;
//     直连模式解析失败回的也是这个码)。
// 会话没有绑定角色时不解析请求体、一律放行,由 login 判定。
inline uint32_t EnterGameRoleSwitchRejectTip(const SessionInfo &session, const std::string &requestBody)
{
	if (!HasBoundRole(session))
	{
		return 0;
	}
	loginpb::EnterGameRequest request;
	if (!request.ParseFromString(requestBody))
	{
		return kRequestMessageParseError;
	}
	if (request.player_id() == session.playerId)
	{
		return 0;
	}
	return kLoginInProgress;
}

} // namespace gate_login_admission

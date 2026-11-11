#include <gtest/gtest.h>

#include <cstdint>
#include <string>

#include "../../nodes/gate/handler/rpc/login_admission.h"
#include "../../nodes/gate/handler/event/authenticated_session_binding.h"
#include "../../nodes/gate/handler/event/scene_route_helper.h"

// ---------------------------------------------------------------------------
// gate 客户端入口的两道 login 相关判定(cpp/nodes/gate/handler/rpc/login_admission.h,调用点在
// client_session_handler.cpp 的 DispatchClientRpcMessage)。
//
// 1. Disconnect(58)只能由 gate 合成:它在生成的上行白名单 IsClientMessageId 里(ClientPlayerLogin 标了
//    OptionIsClientProtocolService),客户端自己发 58 就能在连接还活着时让 login 关栅栏、清登录态、挂断线租约
//    (第二轮审计附带观察)。入口改为按未授权号拒绝,判定不依赖生成物。
// 2. 换角闸:会话已绑定角色、没 LeaveGame 就 EnterGame 另一个角色时不转发(第一轮审计 §4 login 第 2 条的纵深防御;
//    login 会话栅栏的绑定随 TTL 过期时,同一连接曾能绑上第二个角色)。
//
// 会话状态都经生产的写入口构造(gate_authenticated_session::Bind、ApplyLoginBinding、ApplyRouteIdentity、ResetSceneRoute),
// 不手写 SessionInfo 字段,写入口的语义变了这里跟着暴露。
// ---------------------------------------------------------------------------

namespace
{
	constexpr Guid kBoundRole = 200000000000001ull;
	constexpr Guid kOtherRole = 200000000000002ull;
	constexpr Guid kThirdRole = 200000000000003ull;
	constexpr uint64_t kTestIncarnation = 31;
	constexpr uint64_t kBindLoginGeneration = 40;

	std::string EnterGameBody(Guid playerId)
	{
		loginpb::EnterGameRequest request;
		request.set_player_id(playerId);
		request.set_request_id("enter-request");
		return request.SerializeAsString();
	}

	SessionInfo LoggedInSession()
	{
		SessionInfo session;
		session.incarnation = kTestIncarnation;
		session.loginSucceeded = true;
		return session;
	}

	// login 后台链绑定之后发来的 BindSession 落到会话上(BindSessionEventHandler 的两步)。
	void ApplyBind(SessionInfo &session, Guid playerId, uint64_t loginGeneration)
	{
		contracts::kafka::BindSessionEvent bind;
		bind.set_player_id(playerId);
		bind.set_session_incarnation(session.incarnation);
		bind.set_authenticated_account("acct-entry-admission");
		bind.set_owner_revision(3);
		bind.set_login_generation(loginGeneration);
		ASSERT_TRUE(gate_authenticated_session::Bind(session, bind));
		gate_scene_route::ApplyLoginBinding(session, playerId, /*sessionVersion=*/1, loginGeneration, /*enterGsType=*/1);
	}

	SessionInfo BoundSession()
	{
		SessionInfo session = LoggedInSession();
		ApplyBind(session, kBoundRole, kBindLoginGeneration);
		return session;
	}
} // namespace

// 客户端发来的 58 在入口按未授权号拒绝;gate 自己合成的那条不经客户端入口,不受影响。
TEST(GateSynthesizedOnly, DisconnectIsRejectedAtClientEntry)
{
	EXPECT_TRUE(gate_login_admission::IsGateSynthesizedOnly(ClientPlayerLoginDisconnectMessageId));
}

// 排空迁移的要票请求同样只由 gate 合成:客户端自己发就能绕开迁移限速拿到换 gate 的票。
TEST(GateSynthesizedOnly, GateMigrationRequestIsRejectedAtClientEntry)
{
	EXPECT_TRUE(gate_login_admission::IsGateSynthesizedOnly(ClientPlayerLoginRequestGateMigrationMessageId));
}

// 客户端能发起的 login 方法照常进入口。
TEST(GateSynthesizedOnly, ClientInitiatedLoginMethodsAreNotAffected)
{
	for (const uint32_t messageId : {ClientPlayerLoginLoginMessageId,
									 ClientPlayerLoginCreatePlayerMessageId,
									 ClientPlayerLoginEnterGameMessageId,
									 ClientPlayerLoginLeaveGameMessageId,
									 ClientPlayerLoginRefreshTokenMessageId})
	{
		EXPECT_FALSE(gate_login_admission::IsGateSynthesizedOnly(messageId)) << "message_id=" << messageId;
	}
}

// 还没绑定角色(Login 之后、第一次 BindSession 之前):不解析请求体,一律交给 login 判。
TEST(EnterGameRoleSwitch, UnboundSessionIsForwardedWithoutParsing)
{
	const SessionInfo session = LoggedInSession();
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(kOtherRole)), 0u);
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, std::string("\x0a\x05\x01", 3)), 0u);
}

// 已绑定角色、没 LeaveGame 就要进另一个角色:拒绝,回 login 拒绝换角时的同一个码。
TEST(EnterGameRoleSwitch, BoundSessionRejectsAnotherRole)
{
	const SessionInfo session = BoundSession();
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(kOtherRole)),
			  static_cast<uint32_t>(kLoginInProgress));
	// 不带角色(0)同样不是已绑定的那个。
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(0)),
			  static_cast<uint32_t>(kLoginInProgress));
}

// 同一角色重进(重试、断线重连之外的同连接再进)照常转发,由 login 判。
TEST(EnterGameRoleSwitch, BoundSessionForwardsTheSameRole)
{
	const SessionInfo session = BoundSession();
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(kBoundRole)), 0u);
}

// 已绑定角色时读不出请求的角色:证明不了是同一个,fail-closed。
TEST(EnterGameRoleSwitch, UnreadableRequestOnBoundSessionIsRejected)
{
	const SessionInfo session = BoundSession();
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, std::string("\x0a\x05\x01", 3)),
			  static_cast<uint32_t>(kRequestMessageParseError));
}

// 角色先由路由采用(路由先于 BindSession 到达):同样算已绑定。
TEST(EnterGameRoleSwitch, RouteAdoptedRoleCountsAsBound)
{
	SessionInfo session = LoggedInSession();
	gate_scene_route::ApplyRouteIdentity(session, kBoundRole, /*homeZoneId=*/1, /*ownerEpoch=*/5, kBindLoginGeneration);
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(kOtherRole)),
			  static_cast<uint32_t>(kLoginInProgress));
}

// LeaveGame(回选角)之后:gate 不清 playerId,但这段时间不算绑定,下一次可以进任何角色。
TEST(EnterGameRoleSwitch, LeaveGameReleasesTheRole)
{
	SessionInfo session = BoundSession();
	gate_scene_route::ResetSceneRoute(session);
	ASSERT_EQ(session.playerId, kBoundRole) << "前提:LeaveGame 之后 gate 保留旧角色";
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(kOtherRole)), 0u);
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(kBoundRole)), 0u);
}

// 换角之后新角色的 BindSession 到达:新角色成了绑定的角色,再换第三个角色又要先 LeaveGame。
TEST(EnterGameRoleSwitch, RoleBoundAfterLeaveBecomesTheGuardedRole)
{
	SessionInfo session = BoundSession();
	gate_scene_route::ResetSceneRoute(session);
	gate_scene_route::NoteEnterGameRequested(session);
	ApplyBind(session, kOtherRole, /*loginGeneration=*/7);

	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(kOtherRole)), 0u);
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(kThirdRole)),
			  static_cast<uint32_t>(kLoginInProgress));
	EXPECT_EQ(gate_login_admission::EnterGameRoleSwitchRejectTip(session, EnterGameBody(kBoundRole)),
			  static_cast<uint32_t>(kLoginInProgress))
		<< "换回 LeaveGame 之前的角色同样要先 LeaveGame";
}

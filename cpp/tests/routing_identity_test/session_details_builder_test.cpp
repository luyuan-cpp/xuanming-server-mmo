#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <utility>
#include <vector>

#include "../../nodes/gate/handler/rpc/session_details_builder.h"
#include "../../nodes/gate/handler/event/authenticated_session_binding.h"
#include "../../nodes/gate/handler/event/scene_route_helper.h"
#include "core/security/caller_auth.h"
#include "core/utils/encode/base64.h"
#include "session/system/session.h"

TEST(GateAuthenticatedIdentity, MetadataDecoderPreservesAuthenticatedAccount)
{
    SessionDetails original;
    original.set_session_id(17);
    original.set_gate_instance_id("gate-original");
    original.set_session_incarnation(19);
    original.set_player_id(100);
    original.set_owner_revision(3);
    original.set_authenticated_account("original-account");
    // 经实际 session.lib 解码接缝，覆盖新协议与调用库必须一起重编的边界。
    const auto decoded = ParseSessionDetailsMetadataValue(Base64Encode(original.SerializeAsString()));
    ASSERT_NE(decoded, nullptr);
    EXPECT_EQ(decoded->SerializeAsString(), original.SerializeAsString());
}

TEST(GateAuthenticatedIdentity, BindingRejectsReusedConnectionAndRouteCannotPromoteIdentity)
{
    SessionInfo session;
    session.incarnation = 19;
    session.loginSucceeded = true;
    contracts::kafka::BindSessionEvent bind;
    bind.set_player_id(100);
    bind.set_session_incarnation(19);
    bind.set_authenticated_account(" 原账号 ");
    bind.set_owner_revision(3);
    bind.set_login_generation(40);
    ASSERT_TRUE(gate_authenticated_session::Bind(session, bind));
    gate_scene_route::ApplyLoginBinding(session, 100, 2, 40, 1);
    gate_scene_route::ApplyRouteIdentity(session, 100, 1, 20, 40, 9);
    auto detail = gate_session_details::Build(7, 100, 1, "original-gate", &session);
    EXPECT_EQ(detail.authenticated_account(), " 原账号 ");
    EXPECT_EQ(detail.owner_revision(), 3);
    EXPECT_TRUE(gate_session_details::MatchesAuthenticatedBinding(detail, session));
    bind.set_login_generation(39);
    EXPECT_FALSE(gate_authenticated_session::Bind(session, bind));
    bind.set_login_generation(40);
    bind.set_authenticated_account("新账号");
    EXPECT_FALSE(gate_authenticated_session::Bind(session, bind));
    SessionInfo restarted;
    restarted.incarnation = 20;
    restarted.loginSucceeded = true;
    EXPECT_FALSE(gate_authenticated_session::Bind(restarted, bind));
    EXPECT_TRUE(restarted.authenticatedAccount.empty());
    gate_scene_route::ResetSceneRoute(session);
    EXPECT_FALSE(gate_session_details::MatchesAuthenticatedBinding(detail, session));
    detail = gate_session_details::Build(7, 100, 1, "original-gate", &session);
    EXPECT_TRUE(detail.authenticated_account().empty());
    EXPECT_EQ(detail.owner_revision(), 0);
}

// 跨语言契约:与 go/shared/callerauth 的 TestCanonicalV2Vector 断言同一条字面量。
TEST(GateAuthenticatedIdentity, CanonicalMatchesGoVector)
{
    const std::string subject("\0\1\xff\n", 4);
    EXPECT_EQ(caller_auth::Canonical("/login.ClientPlayerLogin/Login", subject, 1762300000000LL,
        "00112233445566778899aabbccddeeff"),
        "mmorpg-gate-identity/v2\n"
        "gate\n"
        "/login.ClientPlayerLogin/Login\n"
        "a95803d91f2a2eb57d42e2a799bb0d42e2acc37c22aa9d433a4173e5db794f28\n"
        "1762300000000\n"
        "00112233445566778899aabbccddeeff");
}

// RFC 8032 §7.1 TEST 1:钉住「种子 → 公钥」与签名原语,Go 侧 TestEd25519Rfc8032Vector 用同一组向量。
namespace
{
constexpr char kRfc8032Test1Seed[] = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60";
constexpr char kRfc8032Test1PublicKey[] = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a";
constexpr char kRfc8032Test1EmptySignature[] =
    "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b";
}

TEST(GateAuthenticatedIdentity, Ed25519MatchesRfc8032Vector)
{
    const auto key = caller_auth::ParseSeedHex(kRfc8032Test1Seed);
    ASSERT_NE(key, nullptr);
    const auto publicKey = caller_auth::PublicKeyBytes(key.get());
    ASSERT_EQ(publicKey.size(), caller_auth::kPublicKeyBytes);
    EXPECT_EQ(token_security::BytesToHex(reinterpret_cast<const unsigned char*>(publicKey.data()),
        static_cast<unsigned int>(publicKey.size())), kRfc8032Test1PublicKey);
    EXPECT_EQ(caller_auth::SignHex(key.get(), ""), kRfc8032Test1EmptySignature);
}

TEST(GateAuthenticatedIdentity, SignerRefusesUnpairedOrMalformedKeys)
{
    const std::string other(64, 'a');
    EXPECT_FALSE(caller_auth::InitProcessSigner(nullptr, kRfc8032Test1PublicKey).empty());
    EXPECT_FALSE(caller_auth::InitProcessSigner("short", kRfc8032Test1PublicKey).empty());
    EXPECT_FALSE(caller_auth::InitProcessSigner(kRfc8032Test1Seed, nullptr).empty());
    // 公钥列表里没有自己这一把:签出的名验证方一律验不过,启动时就拒绝。
    EXPECT_FALSE(caller_auth::InitProcessSigner(kRfc8032Test1Seed, other.c_str()).empty());
    // 列表格式不对(空项、重复)时验证方会拒启,gate 同样拒启。
    EXPECT_FALSE(caller_auth::InitProcessSigner(kRfc8032Test1Seed,
        (std::string(kRfc8032Test1PublicKey) + ",").c_str()).empty());
    EXPECT_FALSE(caller_auth::InitProcessSigner(kRfc8032Test1Seed,
        (std::string(kRfc8032Test1PublicKey) + "," + kRfc8032Test1PublicKey).c_str()).empty());
    // 轮换期新旧公钥并列,只要含有自己这一把就放行;成功后 Append 能挂上完整签名头。
    EXPECT_TRUE(caller_auth::InitProcessSigner(kRfc8032Test1Seed,
        (other + "," + kRfc8032Test1PublicKey).c_str()).empty());
    std::vector<std::pair<std::string, std::string>> metadata;
    ASSERT_TRUE(caller_auth::Append(metadata, "/login.ClientPlayerLogin/Login", std::string("\0\1", 2)));
    ASSERT_EQ(metadata.size(), 4u);
    EXPECT_EQ(metadata[0].first, "x-caller-id");
    EXPECT_EQ(metadata[3].first, "x-caller-sig");
    EXPECT_EQ(metadata[3].second.size(), caller_auth::kSignatureBytes * 2);
    EXPECT_FALSE(caller_auth::Append(metadata, "no-leading-slash", ""));
    caller_auth::ProcessSigningKey().reset();
}

// ---------------------------------------------------------------------------
// session_details_builder_test —— gate 递给 Go 服务的会话头(session_details_builder.h 的 Build / NormalizePlayerId)。
//
// 回归对象是 docs/design/client-access-band-routing.md D60 / F8 / K9:会话未绑角色时 SessionInfo.playerId 是 kInvalidGuid(UINT64_MAX),以前只有路由 lane 归一成 0,
// 直连模式原样带出,Go 侧被当成玩家 18446744073709551615。现在三个调用点(直连转发、路由转发、断线通知)都传会话原值、
// 由 Build 在构造点归一。各用例按调用点的实参形状构造,再走一次线上往返,断言 Go 侧解出来的值。
//
// 路由转发多一步:gate_lane::Admit 在 Build 的结果上复用 NormalizePlayerId 再归一一次,并写 gate_call_seq。Admit 在 gate
// 节点工程里,不进本测试工程;这里只固定它依赖的性质 —— 归一幂等、不改写已绑定的角色。BuildSessionDetails 本身只是
// 补上 gNode 节点号与实例 uuid 的薄包装,不在本文件覆盖。
// ---------------------------------------------------------------------------

namespace
{
	using gate_session_details::Build;
	using gate_session_details::NormalizePlayerId;

	constexpr uint32_t kGateNodeId = 3;
	const std::string kGateInstanceId = "gate-instance-uuid-for-test";
	constexpr SessionId kSessionId = (kGateNodeId << 17) | 5;
	constexpr Guid kBoundPlayer = 1234567890123ULL;
	constexpr uint64_t kIncarnation = 11;
	constexpr uint64_t kGateCallSeq = 9;

	// Go 侧拿到的是 metadata 里的字节,不是 gate 进程里的对象。
	SessionDetails OverTheWire(const SessionDetails &sent)
	{
		SessionDetails received;
		EXPECT_TRUE(received.ParseFromString(sent.SerializeAsString()));
		return received;
	}

	// 刚连上、已验票、还没绑角色的会话(Login 之后、EnterGame 的 BindSession 到达之前都是这个形状)。
	SessionInfo UnboundSession()
	{
		SessionInfo session;
		session.incarnation = kIncarnation;
		return session;
	}

	SessionInfo BoundSession()
	{
		SessionInfo session = UnboundSession();
		session.playerId = kBoundPlayer;
		return session;
	}

	// 直连转发(HandleGrpcNodeMessage)与路由转发(HandleRouterForward)的实参:会话原值 + 会话指针。
	SessionDetails BuildForForward(const SessionInfo &session)
	{
		return Build(kSessionId, session.playerId, kGateNodeId, kGateInstanceId, &session);
	}

	// 路由转发在 Build 之后由 lane 写入的部分(client_request_lane.cpp 的 Admit)。
	SessionDetails StampedByLane(SessionDetails details, const SessionInfo &session)
	{
		details.set_player_id(NormalizePlayerId(details.player_id()));
		details.set_session_incarnation(session.incarnation);
		details.set_gate_call_seq(kGateCallSeq);
		return details;
	}
} // namespace

// 前提:会话缺省就是未绑角色的哨兵。缺省值一旦改掉,下面「未绑角色」的用例就不再覆盖直连模式的旧缺陷。
TEST(SessionDetailsBuilder, FreshSessionStartsWithUnboundSentinel)
{
	EXPECT_EQ(SessionInfo{}.playerId, kInvalidGuid);
}

TEST(SessionDetailsBuilder, NormalizeMapsOnlyTheUnboundSentinelToZero)
{
	EXPECT_EQ(NormalizePlayerId(kInvalidGuid), 0u);
	EXPECT_EQ(NormalizePlayerId(0), 0u);
	EXPECT_EQ(NormalizePlayerId(kBoundPlayer), kBoundPlayer);
	EXPECT_EQ(NormalizePlayerId(kInvalidGuid - 1), kInvalidGuid - 1);
}

// lane 在 Build 的结果上再归一一次:必须幂等,否则「构造点归一 + lane 纵深防御」会互相改写。
TEST(SessionDetailsBuilder, NormalizeIsIdempotent)
{
	for (const Guid playerId : {Guid{0}, kInvalidGuid, kBoundPlayer})
	{
		EXPECT_EQ(NormalizePlayerId(NormalizePlayerId(playerId)), NormalizePlayerId(playerId)) << playerId;
	}
}

// 直连模式(GATE_CLIENT_RPC_ROUTER 未设或为 0):HandleGrpcNodeMessage 发 login / match 的会话头。
// 修复前这里带出的是 UINT64_MAX。
TEST(SessionDetailsBuilder, DirectForwardOfUnboundSessionCarriesZeroPlayerId)
{
	const SessionInfo session = UnboundSession();
	const SessionDetails wire = OverTheWire(BuildForForward(session));
	EXPECT_EQ(wire.player_id(), 0u);
	EXPECT_EQ(wire.session_id(), kSessionId);
	EXPECT_EQ(wire.session_incarnation(), kIncarnation);
	EXPECT_EQ(wire.gate_call_seq(), 0u); // 直连不归 lane 管
}

// 路由模式:HandleRouterForward 构造 → lane 准入时再归一、写调用序号 → 路由服原值透传给目标服务。
TEST(SessionDetailsBuilder, RouterForwardOfUnboundSessionCarriesZeroPlayerId)
{
	const SessionInfo session = UnboundSession();
	const SessionDetails built = BuildForForward(session);
	EXPECT_EQ(built.player_id(), 0u); // 构造点已归一,不再只靠 lane
	const SessionDetails wire = OverTheWire(StampedByLane(built, session));
	EXPECT_EQ(wire.player_id(), 0u);
	EXPECT_EQ(wire.session_incarnation(), kIncarnation);
	EXPECT_EQ(wire.gate_call_seq(), kGateCallSeq);
}

// 断线通知(两种模式共用):调用方同样直接传会话原值,不再在调用点手写「已绑定才带 player_id」。
// login 据 player_id == 0 在 markPlayerSessionDisconnecting 门禁早退,只清 session 状态。
TEST(SessionDetailsBuilder, DisconnectNoticeOfUnboundSessionCarriesZeroPlayerId)
{
	SessionInfo session = UnboundSession();
	session.loginStarted = true; // 发过 Login、还没 BindSession:要通知 login 的那一类
	const SessionDetails wire = OverTheWire(Build(kSessionId, session.playerId, kGateNodeId, kGateInstanceId, &session));
	EXPECT_EQ(wire.player_id(), 0u);
	EXPECT_EQ(wire.session_incarnation(), kIncarnation); // login 会话栅栏按代次关闭
}

// 已绑定角色的会话:三条路径都原样带出,归一不得误伤真实角色。
TEST(SessionDetailsBuilder, BoundSessionKeepsPlayerIdOnEveryPath)
{
	const SessionInfo session = BoundSession();
	EXPECT_EQ(OverTheWire(BuildForForward(session)).player_id(), kBoundPlayer);
	EXPECT_EQ(OverTheWire(StampedByLane(BuildForForward(session), session)).player_id(), kBoundPlayer);
	EXPECT_EQ(OverTheWire(Build(kSessionId, session.playerId, kGateNodeId, kGateInstanceId, &session)).player_id(),
			  kBoundPlayer);
}

// 归一只动 player_id:会话上的其余身份字段照常带出。
TEST(SessionDetailsBuilder, CopiesGateAndSessionIdentityFields)
{
	SessionInfo session = UnboundSession();
	session.ticketPlayerId = 777;
	session.ticketTargetZoneId = 2;
	session.ownerRevision = 5;
	const SessionDetails wire = OverTheWire(BuildForForward(session));
	EXPECT_EQ(wire.session_id(), kSessionId);
	EXPECT_EQ(wire.gate_node_id(), kGateNodeId);
	EXPECT_EQ(wire.gate_instance_id(), kGateInstanceId);
	EXPECT_EQ(wire.ticket_player_id(), 777u);
	EXPECT_EQ(wire.ticket_target_zone_id(), 2u);
	EXPECT_EQ(wire.session_incarnation(), kIncarnation);
	EXPECT_EQ(wire.owner_revision(), 5u);
}

// 不带会话时只填 gate 与 session_id;player_id 同样归一。
TEST(SessionDetailsBuilder, WithoutSessionLeavesSessionScopedFieldsZero)
{
	const SessionDetails wire = OverTheWire(Build(kSessionId, kInvalidGuid, kGateNodeId, kGateInstanceId, nullptr));
	EXPECT_EQ(wire.player_id(), 0u);
	EXPECT_EQ(wire.session_id(), kSessionId);
	EXPECT_EQ(wire.gate_node_id(), kGateNodeId);
	EXPECT_EQ(wire.gate_instance_id(), kGateInstanceId);
	EXPECT_EQ(wire.ticket_player_id(), 0u);
	EXPECT_EQ(wire.session_incarnation(), 0u);
	EXPECT_EQ(wire.owner_revision(), 0u);
}

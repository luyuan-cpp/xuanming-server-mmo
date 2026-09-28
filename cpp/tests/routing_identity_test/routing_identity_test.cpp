#include <gtest/gtest.h>
#include "../../nodes/gate/handler/event/scene_route_helper.h"

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <iterator>
#include <optional>
#include <set>
#include <string>
#include <tuple>
#include <vector>

#include "network/broadcast_target_codec.h"
#include "node/system/node/node_util.h"
#include "services/battle/settlement/settlement_outbox.h"
#include "services/gate/session/system/session_identity_fence.h"
#include "thread_context/node_context_manager.h"

#include "proto/common/base/common.pb.h" // NodeInfo(无 package,全局命名空间)
#include "proto/common/base/message.pb.h"
#include "proto/gate/gate_service.pb.h"

// ---------------------------------------------------------------------------
// routing_identity_test —— 路由身份对抗审计(docs/design/routing-identity-audit-20260908.md)
// 四条缺陷修复的回归测试。
//
// 覆盖:
//   R13 gate 侧身份栅栏:目标玩家与会话当前绑定不符的推送必须被丢弃;
//   R13 广播的 (session_id, player_id) 顺序契约:编解码往返必须逐项对齐;
//   R03 node_id 不是 entt 实体整数:必须走 FindNodeEntity*,裸转换会命中错误节点;
//   R07 结算发件箱的重投判定:销账即停、无目标即等、次数用尽要响。
//
// 这四条的共同点是**编译器看不见、集成测试也未必碰得到**:错投/错位/misroute 都不会
// 抛异常,只会把数据送到错误的地方。所以判定逻辑全部被拆成纯函数,由本文件直接盯住。
// ---------------------------------------------------------------------------

// ===========================================================================
// R13-A:gate 写 socket 之前的身份栅栏
// ===========================================================================

namespace
{
	constexpr uint64_t kPlayerA = 100000000000001ull;
	constexpr uint64_t kPlayerB = 100000000000002ull;
	constexpr uint64_t kUnbound = UINT64_MAX; // SessionInfo::playerId 的默认值 kInvalidGuid
} // namespace

// 正常路径:会话当前就属于这个玩家。
TEST(GateIdentityFence, DeliversPushAddressedToTheSessionOwner)
{
	EXPECT_EQ(gate_session_fence::ClassifyPush(kPlayerA, kPlayerA),
			  gate_session_fence::PushVerdict::kDeliver);
}

// R13 的核心用例:gate G(node_id=3)重启,继任者 G' 仍拿 3 号并从头发 session 号。
// scene 用旧 session_id 反查(高位=3)命中 G',把 A 的结算推向 G' 上同号的 session ——
// 而那个 session 现在属于 B。必须丢。
TEST(GateIdentityFence, DropsPushWhenRecycledGateNodeIdLandsOnAnotherPlayersSession)
{
	EXPECT_EQ(gate_session_fence::ClassifyPush(/*sessionBoundPlayerId=*/kPlayerB,
											   /*targetPlayerId=*/kPlayerA),
			  gate_session_fence::PushVerdict::kDrop);
}

// R14:session_id 的 17 位序号在**单个 gate 实例内**累计 131071 条连接后回绕。
// 这时 gate 的实例 uuid 没变,只带 uuid 的代次栅栏一点用没有 —— 唯一挡得住的是 player_id。
// (对判定函数而言这与上一条同形:同号 session 已易主。这条用例存在的意义是把
//  "uuid 栅栏挡不住、player_id 栅栏挡得住"这个结论钉在测试里。)
TEST(GateIdentityFence, DropsPushWhenSessionSequenceWrappedWithinOneGateIncarnation)
{
	EXPECT_EQ(gate_session_fence::ClassifyPush(kPlayerB, kPlayerA),
			  gate_session_fence::PushVerdict::kDrop);
}

// 会话还没绑定玩家(刚连上、尚未登录)时收到一条指名推送:同样丢。
// 放行等于把战斗结算写进一个陌生人的 socket —— 那正是 R13 的失败形态本身。
TEST(GateIdentityFence, DropsPushToASessionThatHasNoBoundPlayerYet)
{
	EXPECT_EQ(gate_session_fence::ClassifyPush(kUnbound, kPlayerA),
			  gate_session_fence::PushVerdict::kDrop);
	EXPECT_EQ(gate_session_fence::ClassifyPush(0, kPlayerA),
			  gate_session_fence::PushVerdict::kDrop);
}

// 灰度兼容位:老发送方不带 player_id(0)必须放行,否则升级顺序一反就是全服推送静默消失。
// 但它与"校验通过"必须可区分,调用方据此只打一行 INFO 观测迁移进度。
TEST(GateIdentityFence, LegacySenderWithoutPlayerIdIsDeliveredButFlaggedUnfenced)
{
	EXPECT_EQ(gate_session_fence::ClassifyPush(kPlayerA, 0),
			  gate_session_fence::PushVerdict::kDeliverUnfenced);
	EXPECT_EQ(gate_session_fence::ClassifyPush(kUnbound, 0),
			  gate_session_fence::PushVerdict::kDeliverUnfenced);
}

// ===========================================================================
// R13-B:广播的 (session_id, player_id) 顺序契约
// ===========================================================================

namespace
{
	struct Delivered
	{
		uint32_t sessionId = 0;
		uint64_t playerId = 0;
	};

	std::vector<Delivered> Decode(const BroadcastToPlayersRequest &request)
	{
		std::vector<Delivered> out;
		broadcast_targets::ForEach(request,
								   [&out](uint32_t sessionId, uint64_t playerId)
								   { out.push_back({sessionId, playerId}); });
		return out;
	}
} // namespace

// list 形态:发送方给的顺序是乱的,编码后必须按 session_id 升序,且 player 逐项跟着走。
TEST(BroadcastTargetCodec, ListEncodingRoundTripsPairsInSessionOrder)
{
	std::vector<broadcast_targets::Target> targets{
		{300, kPlayerA}, {100, kPlayerB}, {200, 777}};

	BroadcastToPlayersRequest request;
	broadcast_targets::Encode(request, targets);

	ASSERT_TRUE(request.session_bitmap().empty()) << "3 个目标不该走 bitmap";
	const auto decoded = Decode(request);
	ASSERT_EQ(decoded.size(), 3u);
	EXPECT_EQ(decoded[0].sessionId, 100u);
	EXPECT_EQ(decoded[0].playerId, kPlayerB);
	EXPECT_EQ(decoded[1].sessionId, 200u);
	EXPECT_EQ(decoded[1].playerId, 777u);
	EXPECT_EQ(decoded[2].sessionId, 300u);
	EXPECT_EQ(decoded[2].playerId, kPlayerA);
}

// bitmap 形态:gate 只能按 bit 序遍历,两端唯一能对齐的顺序就是 session_id 升序。
// 这条用例是整个顺序契约的支点 —— 错开一格的表现是每个人的身份都被比对成别人的。
TEST(BroadcastTargetCodec, BitmapEncodingKeepsPlayerIdsAlignedWithBitOrder)
{
	std::vector<broadcast_targets::Target> targets;
	// 64 个密集 session,倒序喂进去,确保编码器自己排序而不是依赖调用方。
	for (int i = 63; i >= 0; --i)
	{
		targets.push_back({static_cast<uint32_t>(1000 + i), 5000000000ull + static_cast<uint64_t>(i)});
	}

	BroadcastToPlayersRequest request;
	broadcast_targets::Encode(request, targets);

	ASSERT_FALSE(request.session_bitmap().empty()) << "64 个密集 session 应当走 bitmap";
	const auto decoded = Decode(request);
	ASSERT_EQ(decoded.size(), 64u);
	for (size_t i = 0; i < decoded.size(); ++i)
	{
		EXPECT_EQ(decoded[i].sessionId, 1000u + static_cast<uint32_t>(i));
		EXPECT_EQ(decoded[i].playerId, 5000000000ull + static_cast<uint64_t>(i))
			<< "第 " << i << " 项错位 —— 身份会被比对成别人的";
	}
}

// 重复 session 必须在编码阶段去重:bitmap 只会置一位,而 player_list 若多一项,
// 之后每一项都错位。EntityVector 形态的调用方(AOI 列表)不保证不重复。
TEST(BroadcastTargetCodec, DuplicateSessionsAreCollapsedSoAlignmentSurvives)
{
	std::vector<broadcast_targets::Target> targets{
		{500, kPlayerA}, {400, kPlayerB}, {500, kPlayerA}};

	BroadcastToPlayersRequest request;
	broadcast_targets::Encode(request, targets);

	const auto decoded = Decode(request);
	ASSERT_EQ(decoded.size(), 2u);
	EXPECT_EQ(decoded[0].sessionId, 400u);
	EXPECT_EQ(decoded[0].playerId, kPlayerB);
	EXPECT_EQ(decoded[1].sessionId, 500u);
	EXPECT_EQ(decoded[1].playerId, kPlayerA);
	EXPECT_EQ(request.player_list_size(), 2);
}

// 老发送方:只有 session,没有 player_list。解码必须一律给 0(= 兼容位放行),
// 绝不能越界读。
TEST(BroadcastTargetCodec, LegacyRequestWithoutPlayerListDecodesToZeros)
{
	BroadcastToPlayersRequest request;
	request.add_session_list(11);
	request.add_session_list(22);

	const auto decoded = Decode(request);
	ASSERT_EQ(decoded.size(), 2u);
	EXPECT_EQ(decoded[0].playerId, 0u);
	EXPECT_EQ(decoded[1].playerId, 0u);
	// 兼容位必须被栅栏判成"放行但未校验",而不是丢弃。
	EXPECT_EQ(gate_session_fence::ClassifyPush(kPlayerA, decoded[0].playerId),
			  gate_session_fence::PushVerdict::kDeliverUnfenced);
}

// 端到端:编码 → 解码 → 栅栏判定。一个会话在广播在途期间易主,只有那一条被丢,
// 其余照常投递(这是"丢一条,不是丢整批"的行为保证)。
TEST(BroadcastTargetCodec, OnlyTheHijackedSessionIsDroppedByTheFence)
{
	std::vector<broadcast_targets::Target> targets{{10, kPlayerA}, {20, kPlayerB}};
	BroadcastToPlayersRequest request;
	broadcast_targets::Encode(request, targets);

	// gate 侧当前:两条 session 都绑定给 B —— session 10 在广播在途期间被
	// "node_id 复用后的新连接"接管了。目标写着 A 的那一条必须被丢。
	int delivered = 0;
	int dropped = 0;
	broadcast_targets::ForEach(
		request,
		[&](uint32_t /*sessionId*/, uint64_t targetPlayerId)
		{
			const auto verdict = gate_session_fence::ClassifyPush(kPlayerB, targetPlayerId);
			if (verdict == gate_session_fence::PushVerdict::kDrop)
			{
				++dropped;
			}
			else
			{
				++delivered;
			}
		});
	EXPECT_EQ(dropped, 1) << "session 10 的目标是 A、当前主人是 B,必须丢";
	EXPECT_EQ(delivered, 1) << "session 20 的目标与主人一致,必须投";
}

// NodeMessageHeader 的兼容位:未设置时读出来必须是 0(proto3 隐式存在),
// 也就是"老发送方"这一档。这条把 wire 形状与栅栏的三态对上。
TEST(NodeMessageHeaderFence, UnsetTargetPlayerIdReadsAsLegacyZero)
{
	NodeRouteMessageRequest request;
	request.mutable_header()->set_session_id(5);
	EXPECT_EQ(request.header().target_player_id(), 0u);
	EXPECT_EQ(gate_session_fence::ClassifyPush(kPlayerA, request.header().target_player_id()),
			  gate_session_fence::PushVerdict::kDeliverUnfenced);

	request.mutable_header()->set_target_player_id(kPlayerA);
	EXPECT_EQ(gate_session_fence::ClassifyPush(kPlayerA, request.header().target_player_id()),
			  gate_session_fence::PushVerdict::kDeliver);
	EXPECT_EQ(gate_session_fence::ClassifyPush(kPlayerB, request.header().target_player_id()),
			  gate_session_fence::PushVerdict::kDrop);
}

// ===========================================================================
// R03:node_id 不是 entt 实体整数
// ===========================================================================

namespace
{
	// 造一个"实体槽位号与 node_id 必然不相等"的注册表:先 create 若干实体再
	// destroy 掉前面几个,EnTT 复用槽位并抬高版本号,于是 to_integral(entity)
	// 与 node_id 彻底脱钩 —— 这正是 uuid 主键重构之后线上的真实形状。
	void BuildNodeRegistry(entt::registry &registry, uint32_t zoneId,
						   const std::vector<uint32_t> &nodeIds)
	{
		registry.clear();
		std::vector<entt::entity> scratch;
		for (int i = 0; i < 4; ++i)
		{
			scratch.push_back(registry.create());
		}
		for (auto entity : scratch)
		{
			registry.destroy(entity);
		}
		for (const auto nodeId : nodeIds)
		{
			const auto entity = registry.create();
			auto &info = registry.emplace<NodeInfo>(entity);
			info.set_node_id(nodeId);
			info.set_zone_id(zoneId);
		}
	}
} // namespace

TEST(NodeEntityLookup, NodeIdIsNotAnEntityHandleAndMustBeResolved)
{
	constexpr uint32_t kZone = 1;
	auto &registry = tlsNodeContextManager.GetRegistry(eNodeType::SceneNodeService);
	// node_id 故意取小值,和实体槽位号的取值域重叠 —— 裸转换"看起来能跑"正是因为这个重叠。
	BuildNodeRegistry(registry, kZone, {1, 2, 3});

	const auto resolved = NodeUtils::FindNodeEntityByZoneAndNodeId(
		eNodeType::SceneNodeService, kZone, /*nodeId=*/3);
	ASSERT_TRUE(resolved.has_value()) << "注册表里确实有 node_id=3 的节点";
	ASSERT_TRUE(registry.valid(*resolved));
	EXPECT_EQ(registry.get<NodeInfo>(*resolved).node_id(), 3u);

	// 关键断言:旧写法 `entt::entity{node_id}` 与解析结果不是同一个实体。
	// 它要么无效(消息静默丢失),要么有效但属于**另一个节点**(消息路由到错误的节点)。
	const entt::entity naive{static_cast<entt::id_type>(3)};
	EXPECT_NE(naive, *resolved)
		<< "裸 entt::entity{node_id} 恰好等于解析结果 —— 本用例的前提被破坏,"
		   "请调整 BuildNodeRegistry 让槽位号与 node_id 脱钩";
	if (registry.valid(naive))
	{
		const auto *naiveInfo = registry.try_get<NodeInfo>(naive);
		ASSERT_NE(naiveInfo, nullptr);
		EXPECT_NE(naiveInfo->node_id(), 3u) << "裸转换命中了另一个节点 —— 这就是错投向量本身";
	}

	registry.clear();
}

// zone 维度不能省:zone-scoped 类型的 node_id 只在 zone 内不歧义。
TEST(NodeEntityLookup, ZoneScopedLookupDoesNotAliasAcrossZones)
{
	auto &registry = tlsNodeContextManager.GetRegistry(eNodeType::SceneNodeService);
	registry.clear();
	const auto zone1Node = registry.create();
	auto &info1 = registry.emplace<NodeInfo>(zone1Node);
	info1.set_node_id(7);
	info1.set_zone_id(1);
	const auto zone2Node = registry.create();
	auto &info2 = registry.emplace<NodeInfo>(zone2Node);
	info2.set_node_id(7);
	info2.set_zone_id(2);

	const auto inZone2 =
		NodeUtils::FindNodeEntityByZoneAndNodeId(eNodeType::SceneNodeService, 2, 7);
	ASSERT_TRUE(inZone2.has_value());
	EXPECT_EQ(*inZone2, zone2Node);
	EXPECT_NE(*inZone2, zone1Node);

	EXPECT_FALSE(
		NodeUtils::FindNodeEntityByZoneAndNodeId(eNodeType::SceneNodeService, 3, 7).has_value());

	registry.clear();
}

// ===========================================================================
// R07:结算发件箱的重投判定
// ===========================================================================

namespace
{
	battle_settlement::RetryInput MakeRetryInput(bool stillOurs, bool located, uint32_t attempts,
												 uint32_t maxAttempts = 12)
	{
		battle_settlement::RetryInput input;
		input.pendingRecordStillOurs = stillOurs;
		input.locationResolved = located;
		input.attemptsSoFar = attempts;
		input.maxAttempts = maxAttempts;
		return input;
	}
} // namespace

// scene 应用结算后条件销账 —— 伴生 id 键不再指向本局,battle 停止重投。
TEST(SettlementOutbox, StopsRetryingOnceSceneHasSettledTheRecord)
{
	EXPECT_EQ(battle_settlement::ClassifyRetry(MakeRetryInput(/*stillOurs=*/false, true, 0)),
			  battle_settlement::RetryAction::kDone);
}

// R07 的核心用例:scene 进程中途重启,首投被继任者按代次校验丢掉。
// 记录仍未销账、玩家当前所在的 scene 能解析出来 -> 必须重投(且目标是重新解析出来的那个)。
TEST(SettlementOutbox, ResendsAgainstAReResolvedTargetWhenStillUnacknowledged)
{
	EXPECT_EQ(battle_settlement::ClassifyRetry(MakeRetryInput(/*stillOurs=*/true,
															  /*located=*/true, 0)),
			  battle_settlement::RetryAction::kResend);
	EXPECT_EQ(battle_settlement::ClassifyRetry(MakeRetryInput(true, true, 11, 12)),
			  battle_settlement::RetryAction::kResend);
}

// 玩家此刻不在任何场景里(离线 / 还没落场景):没有目标可投。
// 不是失败 —— 持久记录仍在,scene 的登录钩子会补应用。
TEST(SettlementOutbox, WaitsInsteadOfBlindResendWhenPlayerLocationIsUnknown)
{
	EXPECT_EQ(battle_settlement::ClassifyRetry(MakeRetryInput(true, /*located=*/false, 3)),
			  battle_settlement::RetryAction::kSkipNoTarget);
}

// 次数用尽:摘条目 + 响一声。持久记录保留(TTL 7 天)。
TEST(SettlementOutbox, ExhaustionIsLoudButNotDataLoss)
{
	EXPECT_EQ(battle_settlement::ClassifyRetry(MakeRetryInput(true, true, 12, 12)),
			  battle_settlement::RetryAction::kExhausted);
	EXPECT_EQ(battle_settlement::ClassifyRetry(MakeRetryInput(true, false, 99, 12)),
			  battle_settlement::RetryAction::kExhausted);
}

// 判定顺序:最后一轮恰好读到"已销账"时应当算成功收尾,而不是打一条假的"未送达"警报。
TEST(SettlementOutbox, AcknowledgementWinsOverExhaustionOnTheLastRound)
{
	EXPECT_EQ(battle_settlement::ClassifyRetry(MakeRetryInput(/*stillOurs=*/false,
															  /*located=*/false, 12, 12)),
			  battle_settlement::RetryAction::kDone);
}

// 两个 key 必须是不同的 key,且都按 player_id 取值:blob 与伴生 id 撞成同一个 key
// 会让"条件销账"变成"读 blob 当 battle_id 比",永远不匹配 -> 永远不销账。
TEST(SettlementOutbox, PendingRecordKeysAreDistinctAndPlayerScoped)
{
	const std::string blobKey = battle_settlement::kPendingSettlementKeyFmt;
	const std::string idKey = battle_settlement::kPendingSettlementIdKeyFmt;
	EXPECT_NE(blobKey, idKey);
	EXPECT_NE(blobKey.find("%llu"), std::string::npos);
	EXPECT_NE(idKey.find("%llu"), std::string::npos);
	// 伴生键必须是 blob 键的独立命名空间,不能是它的前缀(否则 SCAN/DEL 会互相波及)。
	EXPECT_EQ(idKey.rfind("battle:settlement:pending:id:", 0), 0u);
	EXPECT_NE(blobKey.rfind("battle:settlement:pending:id:", 0), 0u);
}

// 覆盖真实入场所需的会话状态和转发副作用,不依赖网络或全局节点注册。
using gate_scene_route::SceneNodeChange;

TEST(SceneRouteEntry, LoginThenTravelForwardsWithoutRepeatingLogin)
{
    SessionInfo session;
    session.pendingEnterGsType = 1;
    std::vector<uint32_t> forwarded;
    auto send = [&](uint32_t type) { forwarded.push_back(type); return true; };
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ(0u, session.pendingEnterGsType);
    for (const uint64_t scene : {2001u, 3001u, 4001u, 1001u})
    {
        ASSERT_TRUE(gate_scene_route::ApplyRoute(session, scene, SceneNodeChange::kUnchanged, send));
        EXPECT_EQ(scene, session.sceneId);
    }
    EXPECT_EQ((std::vector<uint32_t>{1, 0, 0, 0, 0}), forwarded);
}

// 同 scene、同节点才是重复事件;节点变了的情形见 SameSceneOnAnotherNodeStillNeedsEntry。
TEST(SceneRouteEntry, DuplicateRouteDoesNotEnterAgain)
{
    SessionInfo session;
    session.sceneId = 2001;
    int sends = 0;
    EXPECT_TRUE(gate_scene_route::ApplyRoute(session, 2001, SceneNodeChange::kUnchanged,
                                             [&](uint32_t) { ++sends; return true; }));
    EXPECT_EQ(0, sends);
    EXPECT_EQ(2001u, session.sceneId);
}

// world rebalance 迁频道沿用原 scene_id(world_rebalance.go migrateWorldChannel):旧节点排空后玩家被
// 改派回"同一个 scene_id、另一个节点"。只看 scene_id 会把它当重复事件吞掉,新节点上永远没有该玩家的实体。
TEST(SceneRouteEntry, SameSceneOnAnotherNodeStillNeedsEntry)
{
    SessionInfo session;
    session.sceneId = 2001;
    std::vector<uint32_t> forwarded;
    auto send = [&](uint32_t type) { forwarded.push_back(type); return true; };
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 2001, SceneNodeChange::kChanged, send));
    EXPECT_EQ((std::vector<uint32_t>{0}), forwarded);
    EXPECT_EQ(2001u, session.sceneId);
    EXPECT_EQ(0u, session.pendingEnterGsType);
    // 同一条路由再到一次时节点指向已是新节点,回到重复事件语义。
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 2001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ((std::vector<uint32_t>{0}), forwarded);
}

// 登录类型优先于"节点变了"推出的 LOGIN_NONE:顶号 / 重连落到另一节点的同一场景时,
// scene 必须拿到真实登录类型才会置登录态、触发登录后置逻辑。
TEST(SceneRouteEntry, NodeChangeWithPendingLoginForwardsPendingType)
{
    SessionInfo session;
    session.sceneId = 2001;
    session.pendingEnterGsType = 3;
    std::vector<uint32_t> forwarded;
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 2001, SceneNodeChange::kChanged,
                                             [&](uint32_t type) { forwarded.push_back(type); return true; }));
    EXPECT_EQ((std::vector<uint32_t>{3}), forwarded);
    EXPECT_EQ(0u, session.pendingEnterGsType);
    EXPECT_EQ(2001u, session.sceneId);
}

// 同 scene_id 换节点(无登录类型)的转发失败要如实返回 false,调用方带着 kChanged 再来一次时仍会补发。
// 这里只验证 ApplyRoute 自身不消费状态;真实调用方 AttemptPendingSceneEntry(scene_route_helper.h)只在
// 转发成功后才提交节点指向,重试时仍拿到 kChanged(见 SceneEntryAttempt.SameSceneOnAnotherNodeIsResentAfterForwardFailure)。
TEST(SceneRouteEntry, NodeChangeForwardFailureCanRetry)
{
    SessionInfo session;
    session.sceneId = 2001;
    std::vector<uint32_t> attempted;
    EXPECT_FALSE(gate_scene_route::ApplyRoute(session, 2001, SceneNodeChange::kChanged,
                                              [&](uint32_t type) { attempted.push_back(type); return false; }));
    EXPECT_EQ((std::vector<uint32_t>{0}), attempted);
    EXPECT_EQ(0u, session.pendingEnterGsType);
    EXPECT_TRUE(gate_scene_route::ApplyRoute(session, 2001, SceneNodeChange::kChanged,
                                             [&](uint32_t type) { attempted.push_back(type); return true; }));
    EXPECT_EQ((std::vector<uint32_t>{0, 0}), attempted);
    EXPECT_EQ(2001u, session.sceneId);
}

// 钉住原语组合的事实:先 RebindSceneNode 后转发时,节点指向在转发之前就已提交,转发失败后同一路由再来时
// 判成 kUnchanged,同 scene_id 换节点这一支恢复不了。生产路径已改为"成功后才提交指向",见
// SceneEntryAttempt.SameSceneOnAnotherNodeIsResentAfterForwardFailure。不要把 RebindSceneNode 接回转发之前。
TEST(SceneRouteEntry, NodeChangeForwardFailureIsNotRecoveredByRedelivery)
{
    SessionInfo session;
    session.sceneId = 2001;
    session.SetEntityId(eNodeType::SceneNodeService, 7);
    std::vector<uint32_t> attempted;
    const auto first = gate_scene_route::RebindSceneNode(session, 9);
    ASSERT_EQ(SceneNodeChange::kChanged, first);
    EXPECT_FALSE(gate_scene_route::ApplyRoute(session, 2001, first,
                                              [&](uint32_t type) { attempted.push_back(type); return false; }));
    EXPECT_EQ((std::vector<uint32_t>{0}), attempted);
    // 同一事件重投:指向已是 9,判成未变化,不再转发。
    const auto redelivered = gate_scene_route::RebindSceneNode(session, 9);
    EXPECT_EQ(SceneNodeChange::kUnchanged, redelivered);
    EXPECT_TRUE(gate_scene_route::ApplyRoute(session, 2001, redelivered,
                                             [&](uint32_t type) { attempted.push_back(type); return true; }));
    EXPECT_EQ((std::vector<uint32_t>{0}), attempted);
}

// 路由先于 BindSession 到达时 sceneId 已提交但尚未入场;此时再来一条"同 scene_id、换了节点"的路由,
// 会先以 0 转发一次,登录类型随后照常补发(scene 侧对这个顺序安全,见 scene_route_helper.h)。
TEST(SceneRouteEntry, NodeChangeAfterRouteButBeforeLoginForwardsNoneThenLogin)
{
    SessionInfo session;
    std::vector<uint32_t> forwarded;
    auto send = [&](uint32_t type) { forwarded.push_back(type); return true; };
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kChanged, send));
    EXPECT_TRUE(forwarded.empty());
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kChanged, send));
    EXPECT_EQ((std::vector<uint32_t>{0}), forwarded);
    session.pendingEnterGsType = 2;
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ((std::vector<uint32_t>{0, 2}), forwarded);
    EXPECT_EQ(0u, session.pendingEnterGsType);
}

// RebindSceneNode:先比较后覆盖。把比较挪到覆盖之后,结果会恒为 kUnchanged,下面几条会红。
TEST(SceneRouteEntry, RebindSceneNodeReportsChangeAgainstPreviousBinding)
{
    SessionInfo session;
    // 从未绑定:旧指向无效,算换了节点(首登是否转发由 ApplyRoute 的 sceneId != 0 把关)。
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::RebindSceneNode(session, 7));
    EXPECT_EQ(7u, session.GetEntityId(eNodeType::SceneNodeService));
    // 同实体:重复路由。
    EXPECT_EQ(SceneNodeChange::kUnchanged, gate_scene_route::RebindSceneNode(session, 7));
    // 不同实体:换了节点,指向被改写。
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::RebindSceneNode(session, 9));
    EXPECT_EQ(9u, session.GetEntityId(eNodeType::SceneNodeService));
    EXPECT_EQ(SceneNodeChange::kUnchanged, gate_scene_route::RebindSceneNode(session, 9));
}

// scene 节点被摘除时 gate 会把会话指向置为 kInvalidEntityId(OnNodeRemoveEventHandler)。
// 之后无论改派到新节点,还是同一进程重启后恰好复用了同一个实体号,都必须算换了节点、重新进场。
TEST(SceneRouteEntry, RebindSceneNodeAfterNodeRemovalCountsAsChanged)
{
    SessionInfo session;
    session.sceneId = 2001;
    session.SetEntityId(eNodeType::SceneNodeService, 7);
    session.SetEntityId(eNodeType::SceneNodeService, SessionInfo::kInvalidEntityId);
    const auto change = gate_scene_route::RebindSceneNode(session, 7);
    EXPECT_EQ(SceneNodeChange::kChanged, change);
    EXPECT_TRUE(session.HasEntityId(eNodeType::SceneNodeService));
    std::vector<uint32_t> forwarded;
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 2001, change,
                                             [&](uint32_t type) { forwarded.push_back(type); return true; }));
    EXPECT_EQ((std::vector<uint32_t>{0}), forwarded);
}

// 别的节点类型的指向不参与判定,也不被改写。
TEST(SceneRouteEntry, RebindSceneNodeIgnoresOtherNodeTypes)
{
    SessionInfo session;
    session.SetEntityId(eNodeType::BattleNodeService, 7);
    session.SetEntityId(eNodeType::SceneNodeService, 7);
    EXPECT_EQ(SceneNodeChange::kUnchanged, gate_scene_route::RebindSceneNode(session, 7));
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::RebindSceneNode(session, 8));
    EXPECT_EQ(7u, session.GetEntityId(eNodeType::BattleNodeService));
}

TEST(SceneRouteEntry, RouteBeforeLoginBindingWaitsForLogin)
{
    SessionInfo session;
    std::vector<uint32_t> forwarded;
    auto send = [&](uint32_t type) { forwarded.push_back(type); return true; };
    EXPECT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_TRUE(forwarded.empty());
    session.pendingEnterGsType = 2;
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ((std::vector<uint32_t>{2}), forwarded);
    EXPECT_EQ(0u, session.pendingEnterGsType);
}

// 会话还没进过任何场景(sceneId 为 0)时,节点变化不单独触发入场:仍等登录类型到达。
TEST(SceneRouteEntry, NodeChangeBeforeFirstEntryWaitsForLogin)
{
    SessionInfo session;
    int sends = 0;
    EXPECT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kChanged,
                                             [&](uint32_t) { ++sends; return true; }));
    EXPECT_EQ(0, sends);
    EXPECT_EQ(1001u, session.sceneId);
}

TEST(SceneRouteEntry, AnotherInstanceOfSameMapStillNeedsEntry)
{
    SessionInfo session;
    session.sceneId = 1001;
    std::vector<uint32_t> forwarded;
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1002, SceneNodeChange::kUnchanged,
                                             [&](uint32_t type) { forwarded.push_back(type); return true; }));
    EXPECT_EQ((std::vector<uint32_t>{0}), forwarded);
}

TEST(SceneRouteEntry, MissingDestinationDoesNotConsumeLoginOrClearRoute)
{
    SessionInfo session;
    session.sceneId = 1001;
    session.pendingEnterGsType = 1;
    EXPECT_FALSE(gate_scene_route::ApplyRoute(session, 0, SceneNodeChange::kChanged,
                                              [](uint32_t) { ADD_FAILURE(); return true; }));
    EXPECT_EQ(1u, session.pendingEnterGsType);
    EXPECT_EQ(1001u, session.sceneId);
}

TEST(SceneRouteEntry, MissingRpcClientCanRetrySameTravelEvent)
{
    SessionInfo session;
    session.sceneId = 1001;
    EXPECT_FALSE(gate_scene_route::ApplyRoute(session, 2001, SceneNodeChange::kUnchanged,
                                              [](uint32_t) { return false; }));
    EXPECT_EQ(1001u, session.sceneId);
    int sends = 0;
    EXPECT_TRUE(gate_scene_route::ApplyRoute(session, 2001, SceneNodeChange::kUnchanged,
                                             [&](uint32_t type) { EXPECT_EQ(0u, type); ++sends; return true; }));
    EXPECT_EQ(1, sends);
    EXPECT_EQ(2001u, session.sceneId);
}

TEST(SceneRouteEntry, MissingRpcClientDoesNotConsumePendingLogin)
{
    SessionInfo session;
    session.pendingEnterGsType = 1;
    EXPECT_FALSE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged,
                                              [](uint32_t) { return false; }));
    EXPECT_EQ(0u, session.sceneId);
    EXPECT_EQ(1u, session.pendingEnterGsType);
    EXPECT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged,
                                             [](uint32_t type) { EXPECT_EQ(1u, type); return true; }));
    EXPECT_EQ(0u, session.pendingEnterGsType);
}

// CompareSceneNode 只比较、不改写:生产路径靠它在转发之前判"换没换节点",指向要等转发成功才提交。
TEST(SceneRouteEntry, CompareSceneNodeReportsChangeWithoutRebinding)
{
    SessionInfo session;
    session.SetEntityId(eNodeType::SceneNodeService, 7);
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::CompareSceneNode(session, 9));
    EXPECT_EQ(7u, session.GetEntityId(eNodeType::SceneNodeService));
    EXPECT_EQ(SceneNodeChange::kUnchanged, gate_scene_route::CompareSceneNode(session, 7));

    // 节点被摘除后指向置无效:再路由回同一个实体号也算换了节点。
    session.SetEntityId(eNodeType::SceneNodeService, SessionInfo::kInvalidEntityId);
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::CompareSceneNode(session, 7));

    // 别的节点类型的指向不参与判定,也不被改写。
    SessionInfo battleOnly;
    battleOnly.SetEntityId(eNodeType::BattleNodeService, 7);
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::CompareSceneNode(battleOnly, 7));
    EXPECT_EQ(7u, battleOnly.GetEntityId(eNodeType::BattleNodeService));
    EXPECT_FALSE(battleOnly.HasEntityId(eNodeType::SceneNodeService));
}

// ===========================================================================
// CPP-2:gate 侧进场转发补发(cross-zone-scene-travel.md "CPP-2 gate 侧进场转发补发")
//
// 被测的全是 scene_route_helper.h 里的纯函数:节点解析与转发由用例注入,时间一律用构造出来的
// time_point(不读真实时钟),"连接"用局部变量的地址充当。注册表、muduo 连接与定时器那一层
// (scene_entry_dispatch.cpp)不在单测范围,由联调场景 SE / SE2 覆盖。
// ===========================================================================

namespace
{
    using gate_scene_route::SceneEntryAttempt;
    using gate_scene_route::SceneEntryRetryVerdict;

    // 任意固定起点,离时钟纪元足够远,避免"减到纪元之前"掩盖错误。
    constexpr SceneEntryClock::time_point kT0 = SceneEntryClock::time_point{} + std::chrono::hours{1};

    std::chrono::milliseconds::rep MsSinceT0(const SceneEntryClock::time_point t)
    {
        return std::chrono::duration_cast<std::chrono::milliseconds>(t - kT0).count();
    }

    // (enterType, nodeEntityId, sceneId)
    using ForwardCall = std::tuple<uint32_t, uint64_t, uint64_t>;

    // 按真实扫描的节奏驱动一笔欠账直到放弃:每轮 now = nextAttemptAt、++attempts、记一次同样的失败。
    // 返回每一轮尝试的时刻(相对 kT0 的毫秒);过程中顺带核对 nextAttemptAt 严格递增且不越过截止。
    std::vector<std::chrono::milliseconds::rep> DriveUntilGiveUp(PendingSceneEntry &entry,
                                                                  const SceneForwardResult failure)
    {
        std::vector<std::chrono::milliseconds::rep> attemptAtMs;
        for (int guard = 0; guard < 100; ++guard)
        {
            const auto now = entry.nextAttemptAt;
            ++entry.attempts;
            attemptAtMs.push_back(MsSinceT0(now));
            if (gate_scene_route::RecordSceneEntryFailure(entry, failure, now) == SceneEntryRetryVerdict::kGiveUp)
            {
                return attemptAtMs;
            }
            EXPECT_GT(MsSinceT0(entry.nextAttemptAt), MsSinceT0(now));
            EXPECT_LE(MsSinceT0(entry.nextAttemptAt), MsSinceT0(entry.deadline));
        }
        ADD_FAILURE() << "欠账在 100 轮内没有放弃";
        return attemptAtMs;
    }
} // namespace

// --- SceneEntryRetry:预算、退避与放弃判定 -------------------------------------------------

TEST(SceneEntryRetry, FreshRouteIsDueImmediatelyWithFullBudget)
{
    const auto entry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 2001}, kT0);
    EXPECT_EQ(0u, entry.attempts);
    EXPECT_EQ(0, MsSinceT0(entry.routedAt));
    EXPECT_EQ(0, MsSinceT0(entry.nextAttemptAt));
    EXPECT_EQ(gate_scene_route::kSceneEntryRetryBudget.count(), MsSinceT0(entry.deadline));
    EXPECT_EQ(42u, entry.target.playerId);
    EXPECT_EQ(7u, entry.target.nodeId);
    EXPECT_EQ(2001u, entry.target.sceneId);
    EXPECT_EQ(SceneForwardResult::kSent, entry.firstFailure);
    EXPECT_EQ(SceneForwardResult::kSent, entry.lastFailure);
}

TEST(SceneEntryRetry, BackoffDoublesFromQuarterSecondAndCapsAtTwoSeconds)
{
    EXPECT_EQ(250, gate_scene_route::SceneEntryBackoff(0).count());
    EXPECT_EQ(250, gate_scene_route::SceneEntryBackoff(1).count());
    EXPECT_EQ(500, gate_scene_route::SceneEntryBackoff(2).count());
    EXPECT_EQ(1000, gate_scene_route::SceneEntryBackoff(3).count());
    EXPECT_EQ(2000, gate_scene_route::SceneEntryBackoff(4).count());
    EXPECT_EQ(2000, gate_scene_route::SceneEntryBackoff(5).count());
    // 大值不溢出、不空转。
    EXPECT_EQ(2000, gate_scene_route::SceneEntryBackoff(1000).count());
}

// 只有"本地确定没发出去"的失败才续期(I1);kSent 不是失败,kInvalidRoute 再等也不会变对。
TEST(SceneEntryRetry, OnlyLocallyUnsentFailuresAreRetryable)
{
    EXPECT_TRUE(gate_scene_route::IsRetryableForwardResult(SceneForwardResult::kNodeNotFound));
    EXPECT_TRUE(gate_scene_route::IsRetryableForwardResult(SceneForwardResult::kNoRpcClient));
    EXPECT_TRUE(gate_scene_route::IsRetryableForwardResult(SceneForwardResult::kNotConnected));
    EXPECT_TRUE(gate_scene_route::IsRetryableForwardResult(SceneForwardResult::kHandshakePending));
    EXPECT_FALSE(gate_scene_route::IsRetryableForwardResult(SceneForwardResult::kSent));
    EXPECT_FALSE(gate_scene_route::IsRetryableForwardResult(SceneForwardResult::kInvalidRoute));
}

// 20s 预算、250ms 起翻倍封顶 2s:最后一次尝试恰好落在截止时刻,之后放弃。
TEST(SceneEntryRetry, GivesUpExactlyAtDeadlineAfterBoundedAttempts)
{
    auto entry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 2001}, kT0);
    const auto attemptAtMs = DriveUntilGiveUp(entry, SceneForwardResult::kNotConnected);
    EXPECT_EQ((std::vector<std::chrono::milliseconds::rep>{0, 250, 750, 1750, 3750, 5750, 7750, 9750, 11750,
                                                           13750, 15750, 17750, 19750, 20000}),
              attemptAtMs);
    ASSERT_EQ(14u, attemptAtMs.size());
    EXPECT_EQ(20000, attemptAtMs.back());
    EXPECT_LE(entry.attempts, gate_scene_route::kSceneEntryMaxAttempts);
    EXPECT_EQ(SceneForwardResult::kNotConnected, entry.lastFailure);
}

// 连上但没握手与没连上同预算:握手在连上 0.5s 后才发,重连窗口相同。
TEST(SceneEntryRetry, HandshakePendingGetsTheSameBudgetAsNotConnected)
{
    auto entry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 2001}, kT0);
    const auto attemptAtMs = DriveUntilGiveUp(entry, SceneForwardResult::kHandshakePending);
    ASSERT_EQ(14u, attemptAtMs.size());
    EXPECT_EQ(20000, attemptAtMs.back());
    EXPECT_EQ(SceneForwardResult::kHandshakePending, entry.lastFailure);
}

// 节点找不到只给 3s 发现预算,截止收紧到 routedAt + 3s,最后一次正好落在 3s。
TEST(SceneEntryRetry, UndiscoveredNodeGivesUpAtDiscoveryBudget)
{
    auto entry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 2001}, kT0);
    const auto attemptAtMs = DriveUntilGiveUp(entry, SceneForwardResult::kNodeNotFound);
    EXPECT_EQ((std::vector<std::chrono::milliseconds::rep>{0, 250, 750, 1750, 3000}), attemptAtMs);
    for (const auto atMs : attemptAtMs)
    {
        EXPECT_LE(atMs, gate_scene_route::kSceneEntryNodeDiscoveryBudget.count());
    }
    EXPECT_LE(MsSinceT0(entry.nextAttemptAt), gate_scene_route::kSceneEntryNodeDiscoveryBudget.count());
    EXPECT_EQ(SceneForwardResult::kNodeNotFound, entry.lastFailure);
}

// 先连不上、后来节点被摘除:在已超过发现预算的那次尝试上立即放弃,两端原因都留下。
TEST(SceneEntryRetry, NodeVanishingAfterDiscoveryBudgetGivesUpImmediately)
{
    auto entry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 2001}, kT0);
    entry.attempts = 1;
    EXPECT_EQ(SceneEntryRetryVerdict::kRetry,
              gate_scene_route::RecordSceneEntryFailure(entry, SceneForwardResult::kNotConnected, kT0));
    entry.attempts = 5;
    EXPECT_EQ(SceneEntryRetryVerdict::kGiveUp,
              gate_scene_route::RecordSceneEntryFailure(entry, SceneForwardResult::kNodeNotFound,
                                                        kT0 + std::chrono::seconds{5}));
    EXPECT_EQ(SceneForwardResult::kNotConnected, entry.firstFailure);
    EXPECT_EQ(SceneForwardResult::kNodeNotFound, entry.lastFailure);
}

// 次数上限是纵深防御:截止之前也会放弃。
TEST(SceneEntryRetry, AttemptCapGivesUpBeforeDeadline)
{
    auto entry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 2001}, kT0);
    entry.attempts = gate_scene_route::kSceneEntryMaxAttempts;
    EXPECT_EQ(SceneEntryRetryVerdict::kGiveUp,
              gate_scene_route::RecordSceneEntryFailure(entry, SceneForwardResult::kNotConnected,
                                                        kT0 + std::chrono::seconds{1}));
}

TEST(SceneEntryRetry, InvalidRouteIsGivenUpWithoutRetry)
{
    auto entry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 0}, kT0);
    entry.attempts = 1;
    EXPECT_EQ(SceneEntryRetryVerdict::kGiveUp,
              gate_scene_route::RecordSceneEntryFailure(entry, SceneForwardResult::kInvalidRoute, kT0));
    EXPECT_EQ(0, MsSinceT0(entry.nextAttemptAt));
    EXPECT_EQ(SceneForwardResult::kInvalidRoute, entry.lastFailure);
}

// 放弃日志要同时回答"一开始为什么没发出去"与"最后卡在哪一步"。
TEST(SceneEntryRetry, FirstAndLastFailureAreBothKept)
{
    auto entry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 2001}, kT0);
    entry.attempts = 1;
    ASSERT_EQ(SceneEntryRetryVerdict::kRetry,
              gate_scene_route::RecordSceneEntryFailure(entry, SceneForwardResult::kHandshakePending, kT0));
    entry.attempts = 2;
    ASSERT_EQ(SceneEntryRetryVerdict::kRetry,
              gate_scene_route::RecordSceneEntryFailure(entry, SceneForwardResult::kNotConnected,
                                                        entry.nextAttemptAt));
    EXPECT_EQ(SceneForwardResult::kHandshakePending, entry.firstFailure);
    EXPECT_EQ(SceneForwardResult::kNotConnected, entry.lastFailure);
}

// 名表与枚举一一对应(static_assert 只管长度,这里再管非空、不重复与越界兜底)。
TEST(SceneEntryRetry, ForwardResultNamesCoverEveryValue)
{
    ASSERT_EQ(static_cast<std::size_t>(SceneForwardResult::kCount), std::size(kSceneForwardResultNames));
    std::set<std::string> seen;
    for (const char *name : kSceneForwardResultNames)
    {
        ASSERT_NE(nullptr, name);
        EXPECT_GT(std::strlen(name), 0u);
        EXPECT_TRUE(seen.insert(name).second) << "重复的名字: " << name;
    }
    EXPECT_STREQ("handshake_pending", SceneForwardResultName(SceneForwardResult::kHandshakePending));
    EXPECT_STREQ("unknown", SceneForwardResultName(SceneForwardResult::kCount));
}

// --- SceneLinkReady:"已连上"不等于"可交付",还要在当前连接上握过手 --------------------------

TEST(SceneLinkReady, NotConnectedWhenClientOrConnectionIsDown)
{
    int a = 0;
    // 即使印章恰好等于当前连接,任一"没连上"都优先判 not_connected。
    const auto clientDown = gate_scene_route::ClassifySceneLink(false, &a, true, &a);
    ASSERT_TRUE(clientDown.has_value());
    EXPECT_EQ(SceneForwardResult::kNotConnected, *clientDown);

    const auto noConnection = gate_scene_route::ClassifySceneLink(true, nullptr, false, &a);
    ASSERT_TRUE(noConnection.has_value());
    EXPECT_EQ(SceneForwardResult::kNotConnected, *noConnection);

    const auto connectionClosing = gate_scene_route::ClassifySceneLink(true, &a, false, &a);
    ASSERT_TRUE(connectionClosing.has_value());
    EXPECT_EQ(SceneForwardResult::kNotConnected, *connectionClosing);
}

// 刚连上(或刚重连)还没握手:scene 还没为本 gate 挂 RpcSession,进场通知会被丢。
TEST(SceneLinkReady, ReconnectedLinkNotReadyUntilHandshakeOnSameConnection)
{
    int a = 0;
    const auto verdict = gate_scene_route::ClassifySceneLink(true, &a, true, nullptr);
    ASSERT_TRUE(verdict.has_value());
    EXPECT_EQ(SceneForwardResult::kHandshakePending, *verdict);
}

// 旧连接上的握手章不算数:重连后必须在新连接上重新握手。
TEST(SceneLinkReady, HandshakeOnOldConnectionDoesNotCount)
{
    int a = 0;
    int b = 0;
    const auto verdict = gate_scene_route::ClassifySceneLink(true, &b, true, &a);
    ASSERT_TRUE(verdict.has_value());
    EXPECT_EQ(SceneForwardResult::kHandshakePending, *verdict);
}

TEST(SceneLinkReady, ReadyWhenHandshakenOnCurrentConnection)
{
    int a = 0;
    EXPECT_FALSE(gate_scene_route::ClassifySceneLink(true, &a, true, &a).has_value());
}

// --- SceneEntryAttempt:比较、转发、成功后才提交 --------------------------------------------

// 节点还没被 gate 发现:登录类型与路由都不消费;发现之后以登录类型恰好转发一次。
TEST(SceneEntryAttempt, UndiscoveredNodeRetriesThenForwardsLoginOnce)
{
    SessionInfo session;
    session.pendingEnterGsType = 1;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 1001}, kT0);

    std::optional<uint64_t> resolvedEntity;
    std::vector<ForwardCall> calls;
    auto resolve = [&](const uint32_t nodeId)
    {
        EXPECT_EQ(7u, nodeId);
        return resolvedEntity;
    };
    auto forward = [&](const uint32_t enterType, const uint64_t nodeEntity, const uint64_t sceneId)
    {
        calls.emplace_back(enterType, nodeEntity, sceneId);
        return SceneForwardResult::kSent;
    };

    EXPECT_EQ(SceneEntryAttempt::kRetryLater,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ(0u, session.sceneId);
    EXPECT_EQ(1u, session.pendingEnterGsType);
    EXPECT_FALSE(session.HasEntityId(eNodeType::SceneNodeService));
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(1u, session.pendingSceneEntry->attempts);
    EXPECT_EQ(SceneForwardResult::kNodeNotFound, session.pendingSceneEntry->lastFailure);
    EXPECT_TRUE(calls.empty());

    resolvedEntity = 55;
    const auto secondAt = session.pendingSceneEntry->nextAttemptAt;
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, secondAt, resolve, forward));
    EXPECT_EQ((std::vector<ForwardCall>{ForwardCall{1u, 55u, 1001u}}), calls);
    EXPECT_EQ(1001u, session.sceneId);
    EXPECT_EQ(0u, session.pendingEnterGsType);
    EXPECT_FALSE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(55u, session.GetEntityId(eNodeType::SceneNodeService));

    // 欠账已清:再尝试是 no-op,不会重发。
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, secondAt + std::chrono::seconds{1}, resolve,
                                                         forward));
    EXPECT_EQ(1u, calls.size());
}

// 没连上 / 没握手期间登录类型一直留着;链路就绪后恰好发一次,之后绝不重发(I1)。
TEST(SceneEntryAttempt, NotConnectedKeepsLoginTypeAndSendsExactlyOnceAfterReconnect)
{
    SessionInfo session;
    session.pendingEnterGsType = 2;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 3, 1001}, kT0);

    const std::vector<SceneForwardResult> script{SceneForwardResult::kNotConnected,
                                                 SceneForwardResult::kHandshakePending,
                                                 SceneForwardResult::kSent};
    std::vector<uint32_t> forwardedTypes;
    auto resolve = [](uint32_t) { return std::optional<uint64_t>{9}; };
    auto forward = [&](const uint32_t enterType, uint64_t, uint64_t)
    {
        forwardedTypes.push_back(enterType);
        if (forwardedTypes.size() > script.size())
        {
            ADD_FAILURE() << "转发次数超出脚本";
            return SceneForwardResult::kSent;
        }
        return script[forwardedTypes.size() - 1];
    };

    auto now = kT0;
    EXPECT_EQ(SceneEntryAttempt::kRetryLater,
              gate_scene_route::AttemptPendingSceneEntry(session, now, resolve, forward));
    EXPECT_EQ(2u, session.pendingEnterGsType);
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    now = session.pendingSceneEntry->nextAttemptAt;
    EXPECT_EQ(SceneEntryAttempt::kRetryLater,
              gate_scene_route::AttemptPendingSceneEntry(session, now, resolve, forward));
    EXPECT_EQ(2u, session.pendingEnterGsType);
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    now = session.pendingSceneEntry->nextAttemptAt;
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, now, resolve, forward));
    EXPECT_EQ((std::vector<uint32_t>{2, 2, 2}), forwardedTypes);

    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, now, resolve, forward));
    EXPECT_EQ(3u, forwardedTypes.size());
}

// 原来那条已知局限的修复:同 scene_id 换节点,首次转发失败后重试仍判成换了节点并补发 0。
TEST(SceneEntryAttempt, SameSceneOnAnotherNodeIsResentAfterForwardFailure)
{
    SessionInfo session;
    session.sceneId = 2001;
    session.SetEntityId(eNodeType::SceneNodeService, 7);
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 9, 2001}, kT0);

    std::vector<uint32_t> forwardedTypes;
    auto resolve = [](uint32_t) { return std::optional<uint64_t>{9}; };
    auto forward = [&](const uint32_t enterType, uint64_t, uint64_t)
    {
        forwardedTypes.push_back(enterType);
        return forwardedTypes.size() == 1 ? SceneForwardResult::kNotConnected : SceneForwardResult::kSent;
    };

    EXPECT_EQ(SceneEntryAttempt::kRetryLater,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ(7u, session.GetEntityId(eNodeType::SceneNodeService)); // 没提交
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, session.pendingSceneEntry->nextAttemptAt, resolve,
                                                         forward));
    EXPECT_EQ((std::vector<uint32_t>{0, 0}), forwardedTypes);
    EXPECT_EQ(9u, session.GetEntityId(eNodeType::SceneNodeService));
}

// 失败窗口内 sceneId 与指向都不动:断线 ExitGame 与客户端消息仍发往旧节点(I5)。
TEST(SceneEntryAttempt, FailedAttemptsNeverCommitTheNewRoute)
{
    SessionInfo session;
    session.playerId = 42;
    session.sceneId = 2001;
    session.SetEntityId(eNodeType::SceneNodeService, 7);
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 9, 3001}, kT0);

    const std::vector<SceneForwardResult> script{SceneForwardResult::kHandshakePending,
                                                 SceneForwardResult::kNotConnected,
                                                 SceneForwardResult::kSent};
    std::vector<uint32_t> forwardedTypes;
    auto resolve = [](uint32_t) { return std::optional<uint64_t>{9}; };
    auto forward = [&](const uint32_t enterType, uint64_t, uint64_t)
    {
        forwardedTypes.push_back(enterType);
        if (forwardedTypes.size() > script.size())
        {
            ADD_FAILURE() << "转发次数超出脚本";
            return SceneForwardResult::kSent;
        }
        return script[forwardedTypes.size() - 1];
    };

    auto now = kT0;
    for (int i = 0; i < 2; ++i)
    {
        EXPECT_EQ(SceneEntryAttempt::kRetryLater,
                  gate_scene_route::AttemptPendingSceneEntry(session, now, resolve, forward));
        EXPECT_EQ(2001u, session.sceneId);
        EXPECT_EQ(7u, session.GetEntityId(eNodeType::SceneNodeService));
        ASSERT_TRUE(session.pendingSceneEntry.has_value());
        now = session.pendingSceneEntry->nextAttemptAt;
    }
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, now, resolve, forward));
    EXPECT_EQ(3001u, session.sceneId);
    EXPECT_EQ(9u, session.GetEntityId(eNodeType::SceneNodeService));
    EXPECT_EQ((std::vector<uint32_t>{0, 0, 0}), forwardedTypes);
}

// 同节点、同 scene、无登录类型:本来就不需要转发,欠账直接结清。
TEST(SceneEntryAttempt, DuplicateRouteNeedsNoForwardAndClearsDebt)
{
    SessionInfo session;
    session.sceneId = 2001;
    session.SetEntityId(eNodeType::SceneNodeService, 9);
    session.pendingEnterGsType = 0;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 9, 2001}, kT0);

    auto resolve = [](uint32_t) { return std::optional<uint64_t>{9}; };
    auto forward = [](uint32_t, uint64_t, uint64_t)
    {
        ADD_FAILURE() << "重复路由不应转发";
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_FALSE(session.pendingSceneEntry.has_value());
}

// 到截止仍交不出去:放弃,但欠账留给调用方写日志;登录类型与指向都没被消费或改写。
TEST(SceneEntryAttempt, GivesUpAtDeadlineAndLeavesDebtForCallerToLog)
{
    SessionInfo session;
    session.sceneId = 2001;
    session.SetEntityId(eNodeType::SceneNodeService, 7);
    session.pendingEnterGsType = 1;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 9, 3001}, kT0);

    auto resolve = [](uint32_t) { return std::optional<uint64_t>{9}; };
    auto forward = [](uint32_t, uint64_t, uint64_t) { return SceneForwardResult::kNotConnected; };
    EXPECT_EQ(SceneEntryAttempt::kGiveUp,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0 + gate_scene_route::kSceneEntryRetryBudget,
                                                         resolve, forward));
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(SceneForwardResult::kNotConnected, session.pendingSceneEntry->lastFailure);
    EXPECT_EQ(1u, session.pendingEnterGsType);
    EXPECT_EQ(7u, session.GetEntityId(eNodeType::SceneNodeService));
    EXPECT_EQ(2001u, session.sceneId);
}

// scene_id 为 0 的路由以前被静默忽略,现在 fail-closed:不解析、不转发、不动会话,直接放弃。
TEST(SceneEntryAttempt, ZeroSceneIdIsGivenUpWithoutTouchingSession)
{
    SessionInfo session;
    session.sceneId = 1001;
    session.SetEntityId(eNodeType::SceneNodeService, 7);
    session.pendingEnterGsType = 1;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 9, 0}, kT0);

    auto resolve = [](uint32_t)
    {
        ADD_FAILURE() << "无效路由不应解析节点";
        return std::optional<uint64_t>{};
    };
    auto forward = [](uint32_t, uint64_t, uint64_t)
    {
        ADD_FAILURE() << "无效路由不应转发";
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kGiveUp,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(SceneForwardResult::kInvalidRoute, session.pendingSceneEntry->lastFailure);
    EXPECT_EQ(1001u, session.sceneId);
    EXPECT_EQ(7u, session.GetEntityId(eNodeType::SceneNodeService));
    EXPECT_EQ(1u, session.pendingEnterGsType);
}

// 同一条连接上已换成另一名角色:绝不以错的角色补发,也不动会话,由调用方撤销欠账。
TEST(SceneEntryAttempt, PlayerMismatchCancelsWithoutForwarding)
{
    SessionInfo session;
    session.playerId = 43;
    session.sceneId = 1001;
    session.SetEntityId(eNodeType::SceneNodeService, 7);
    session.pendingEnterGsType = 1;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 9, 2001}, kT0);

    auto resolve = [](uint32_t)
    {
        ADD_FAILURE() << "角色不符不应解析节点";
        return std::optional<uint64_t>{9};
    };
    auto forward = [](uint32_t, uint64_t, uint64_t)
    {
        ADD_FAILURE() << "角色不符不应转发";
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kPlayerMismatch,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(0u, session.pendingSceneEntry->attempts);
    EXPECT_EQ(1001u, session.sceneId);
    EXPECT_EQ(7u, session.GetEntityId(eNodeType::SceneNodeService));
    EXPECT_EQ(1u, session.pendingEnterGsType);
}

// 路由事件没带玩家(0 = 未知)时不设防,照常转发。
TEST(SceneEntryAttempt, UnknownRoutedPlayerIsNotFenced)
{
    SessionInfo session;
    session.playerId = 43;
    session.pendingEnterGsType = 1;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 9, 2001}, kT0);

    int sends = 0;
    auto resolve = [](uint32_t) { return std::optional<uint64_t>{9}; };
    auto forward = [&](uint32_t, uint64_t, uint64_t)
    {
        ++sends;
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ(1, sends);
}

// --- SceneEntryLateLogin:BindSession 晚到时按最近一次路由的 node_id 建欠账 --------------------

TEST(SceneEntryLateLogin, LoginBoundBeforeAnyRouteWaits)
{
    SessionInfo session;
    session.pendingEnterGsType = 1;
    EXPECT_FALSE(gate_scene_route::EntryForLateLoginBinding(session, kT0).has_value());
}

// 路由已提交、之后节点被摘除(指向被置无效):以前登录类型会永远挂着;现在建欠账,3s 发现预算后放弃。
TEST(SceneEntryLateLogin, LoginBoundAfterCommittedRouteWhoseNodeWasRemovedRetriesThenGivesUp)
{
    SessionInfo session;
    session.sceneId = 1001;
    session.SetEntityId(eNodeType::SceneNodeService, SessionInfo::kInvalidEntityId);
    session.lastSceneRoute = SceneRouteTarget{42, 7, 1001};
    session.playerId = 42;
    session.pendingEnterGsType = 1;

    auto entry = gate_scene_route::EntryForLateLoginBinding(session, kT0);
    ASSERT_TRUE(entry.has_value());
    session.pendingSceneEntry = *entry;

    auto resolve = [](uint32_t) { return std::optional<uint64_t>{}; };
    auto forward = [](uint32_t, uint64_t, uint64_t)
    {
        ADD_FAILURE() << "节点找不到时不应转发";
        return SceneForwardResult::kSent;
    };
    auto outcome = SceneEntryAttempt::kRetryLater;
    auto now = kT0;
    for (int guard = 0; guard < 100 && outcome == SceneEntryAttempt::kRetryLater; ++guard)
    {
        ASSERT_TRUE(session.pendingSceneEntry.has_value());
        now = session.pendingSceneEntry->nextAttemptAt;
        outcome = gate_scene_route::AttemptPendingSceneEntry(session, now, resolve, forward);
    }
    EXPECT_EQ(SceneEntryAttempt::kGiveUp, outcome);
    EXPECT_EQ(gate_scene_route::kSceneEntryNodeDiscoveryBudget.count(), MsSinceT0(now));
    EXPECT_EQ(1u, session.pendingEnterGsType);
}

// 同 uuid 重注册留下悬空实体号(HasEntityId 仍为 true):按 node_id 重新解析到新实体,以登录类型转发。
TEST(SceneEntryLateLogin, LoginBoundAfterRouteToReRegisteredNodeResolvesByNodeId)
{
    SessionInfo session;
    session.SetEntityId(eNodeType::SceneNodeService, 7); // 已销毁的旧实体号
    ASSERT_TRUE(session.HasEntityId(eNodeType::SceneNodeService));
    session.sceneId = 1001;
    session.lastSceneRoute = SceneRouteTarget{42, 3, 1001};
    session.playerId = 42;
    session.pendingEnterGsType = 1;

    auto entry = gate_scene_route::EntryForLateLoginBinding(session, kT0);
    ASSERT_TRUE(entry.has_value());
    session.pendingSceneEntry = *entry;

    std::vector<ForwardCall> calls;
    auto resolve = [](const uint32_t nodeId)
    {
        EXPECT_EQ(3u, nodeId);
        return std::optional<uint64_t>{12};
    };
    auto forward = [&](const uint32_t enterType, const uint64_t nodeEntity, const uint64_t sceneId)
    {
        calls.emplace_back(enterType, nodeEntity, sceneId);
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ((std::vector<ForwardCall>{ForwardCall{1u, 12u, 1001u}}), calls);
    EXPECT_EQ(12u, session.GetEntityId(eNodeType::SceneNodeService));
    EXPECT_EQ(0u, session.pendingEnterGsType);
}

// 最近一次路由属于另一名角色:不拿它建欠账,等本角色自己的路由。
TEST(SceneEntryLateLogin, LoginBoundToAnotherCharacterWaitsForItsOwnRoute)
{
    SessionInfo session;
    session.lastSceneRoute = SceneRouteTarget{42, 7, 1001};
    session.playerId = 43;
    session.pendingEnterGsType = 1;
    EXPECT_FALSE(gate_scene_route::EntryForLateLoginBinding(session, kT0).has_value());
}

// 已有欠账时不另建第二笔(调用方直接对现有欠账立即尝试,ApplyRoute 会优先带上登录类型)。
TEST(SceneEntryLateLogin, LoginBoundWhileDebtPendingDoesNotBuildASecondEntry)
{
    SessionInfo session;
    session.playerId = 42;
    session.pendingEnterGsType = 1;
    session.lastSceneRoute = SceneRouteTarget{42, 7, 1001};
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 7, 1001}, kT0);
    EXPECT_FALSE(gate_scene_route::EntryForLateLoginBinding(session, kT0).has_value());
}

int main(int argc, char **argv)
{
	testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}

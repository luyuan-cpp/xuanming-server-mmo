#include <gtest/gtest.h>

#include <cstdint>
#include <string>
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

int main(int argc, char **argv)
{
	testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}

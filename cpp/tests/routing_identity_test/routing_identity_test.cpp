#include <gtest/gtest.h>
#include "../../nodes/gate/handler/event/scene_route_helper.h"

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <iterator>
#include <limits>
#include <memory>
#include <optional>
#include <set>
#include <string>
#include <string_view>
#include <tuple>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "network/broadcast_target_codec.h"
#include "node/system/node/node_util.h"
#include "services/battle/settlement/settlement_outbox.h"
#include "services/battle/settlement/settlement_reply_shape.h"
#include "services/gate/session/system/session_identity_fence.h"
#include "thread_context/node_context_manager.h"

#include "proto/common/base/common.pb.h" // NodeInfo(无 package,全局命名空间)
#include "proto/common/base/message.pb.h"
#include "proto/gate/gate_service.pb.h"
#include "../../nodes/gate/handler/rpc/login_admission.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/login_error_tip.pb.h"

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
// 对端带来的 node_type 越界(docs/design/ecs-entt-misuse-audit-20261015.md F093 / C007)
// ===========================================================================
//
// 审计时 gate RoutePlayerMessage、scene RoutePlayerStringMsg、NodeHandshakeManager 的握手应答处理
// 都拿对端带来的 node_type 选 registry(2026-10-15 起这三处都已不再这样做:Route 一族已在批 7 从 proto 删除
// (41/74/78/81 留墓碑,见 docs/design/muduo-rpc-channel.md §20),
// 握手应答按本端发起时的 nodeType 选 registry,见 docs/design/muduo-rpc-channel.md §8.3;
// 下面的用例锁的是 NodeContextManager 本身的边界,不依赖这些入口)。以前 NodeContextManager::GetRegistry 只有 assert,
// Release 下 NDEBUG 抹掉它,越界读写 registries_ 之外的内存。
// 现在分两类:外部输入走 TryGetRegistry(越界返回 nullptr,由入口拒绝);GetRegistry 只给可信内部调用方,
// 越界是编程错误,任何构建下都 LOG_FATAL。
// 修复前:TryGetRegistry / IsValidNodeType 不存在,第一个用例编译不过;死亡测试见下方各自的说明。

namespace
{
	// 对端可能发来的非法 node_type:刚好越界一格、越界很多、int32 上限、uint32 上限。
	constexpr uint32_t kForeignNodeTypes[] = {kNodeTypeCount, kNodeTypeCount + 1, 0x7fffffffu, UINT32_MAX};
} // namespace

TEST(NodeContextManagerBounds, TryGetRegistryRejectsOutOfRangeNodeType)
{
	// 用独立实例,不碰 tlsNodeContextManager,用例之间互不影响。
	const auto manager = std::make_unique<NodeContextManager>();
	for (const auto nodeType : kForeignNodeTypes)
	{
		EXPECT_FALSE(NodeContextManager::IsValidNodeType(nodeType)) << "node_type=" << nodeType;
		EXPECT_EQ(manager->TryGetRegistry(nodeType), nullptr) << "node_type=" << nodeType;
	}
	// 合法值必须拿到与 GetRegistry 同一个 registry,入口改用 TryGetRegistry 后行为不变。
	for (uint32_t nodeType = 0; nodeType < kNodeTypeCount; ++nodeType)
	{
		EXPECT_TRUE(NodeContextManager::IsValidNodeType(nodeType)) << "node_type=" << nodeType;
		EXPECT_EQ(manager->TryGetRegistry(nodeType), &manager->GetRegistry(nodeType)) << "node_type=" << nodeType;
	}
}

#if GTEST_HAS_DEATH_TEST

namespace
{
	// muduo 默认把日志写到 stdout,死亡测试只比对子进程的 stderr。在死亡语句里(只在子进程执行)把日志
	// 改到 stderr,才能断言子进程死于 NodeContextManager 自己的越界检查,而不是死于别的原因。
	void RouteMuduoLogToStderr()
	{
		muduo::Logger::setOutput([](const char *msg, int len) {
			std::fwrite(msg, 1, static_cast<std::size_t>(len), stderr);
			std::fflush(stderr);
		});
	}
} // namespace

// GetRegistry 只给可信内部调用方用:越界是编程错误,任何构建下都必须当场终止,
// 不能返回替身 registry 让调用方带着它照常往下跑(建 RpcClient、发起连接这类副作用照样会发生)。
// NodeUtils 的节点查找内部走 GetRegistry,同样不接受未校验的 node_type:这正是对端输入的入口
// 必须先 TryGetRegistry、再做查找的原因。
// 修复前为红:Release 下 NDEBUG 抹掉 assert,越界读写 registries_ 之外的内存,子进程不死;
// Debug 下子进程死于 assert,但 assert 文本里没有「NodeContextManager::<函数名>:」,正则不匹配。
TEST(NodeContextManagerBoundsDeathTest, OutOfRangeNodeTypeIsFatalInEveryBuild)
{
	const auto manager = std::make_unique<NodeContextManager>();
	for (const auto nodeType : kForeignNodeTypes)
	{
		EXPECT_DEATH({ RouteMuduoLogToStderr(); (void)manager->GetRegistry(nodeType); },
					 "NodeContextManager::GetRegistry: nodeType=")
			<< "node_type=" << nodeType;
	}
	EXPECT_DEATH({ RouteMuduoLogToStderr(); (void)NodeUtils::FindNodeEntityByZoneAndNodeId(kNodeTypeCount, 1, 1); },
				 "NodeContextManager::GetRegistry: nodeType=");
}

#endif // GTEST_HAS_DEATH_TEST

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

// ===========================================================================
// 2026-10-16:zone Redis 空应答 / ERROR 不是业务结论
// ===========================================================================
//
// hiredis 在连接断开或被释放时(主切换掐连接、存活探测摘连接、停机)把在途命令的回调以空应答调用一次。
// 下面每条用例对应一个曾把空应答当结论的调用点,改坏任意一条判定都会在这里红。

namespace
{
	using battle_settlement::ReplyShape;

	redisReply MakeReply(const int type, const char *text = nullptr)
	{
		redisReply reply{};
		reply.type = type;
		if (text != nullptr)
		{
			reply.str = const_cast<char *>(text);
			reply.len = std::strlen(text);
		}
		return reply;
	}
} // namespace

// 适配层:空应答必须映射成 kNoReply,绝不能与 NIL 混为一谈 —— 后者是「键不存在」这个结论。
TEST(SettlementReplyShape, NullReplyIsNoReplyNotNil)
{
	EXPECT_EQ(battle_settlement::ReplyShapeOf(nullptr), ReplyShape::kNoReply);
	EXPECT_TRUE(battle_settlement::ReplyText(nullptr).empty());

	auto nil = MakeReply(REDIS_REPLY_NIL);
	auto str = MakeReply(REDIS_REPLY_STRING, "701");
	auto status = MakeReply(REDIS_REPLY_STATUS, "OK");
	auto integer = MakeReply(REDIS_REPLY_INTEGER);
	auto array = MakeReply(REDIS_REPLY_ARRAY);
	auto error = MakeReply(REDIS_REPLY_ERROR, "LOADING Redis is loading the dataset in memory");
	auto dbl = MakeReply(REDIS_REPLY_DOUBLE);
	EXPECT_EQ(battle_settlement::ReplyShapeOf(&nil), ReplyShape::kNil);
	EXPECT_EQ(battle_settlement::ReplyShapeOf(&str), ReplyShape::kString);
	EXPECT_EQ(battle_settlement::ReplyShapeOf(&status), ReplyShape::kStatus);
	EXPECT_EQ(battle_settlement::ReplyShapeOf(&integer), ReplyShape::kInteger);
	EXPECT_EQ(battle_settlement::ReplyShapeOf(&array), ReplyShape::kArray);
	EXPECT_EQ(battle_settlement::ReplyShapeOf(&error), ReplyShape::kError);
	EXPECT_EQ(battle_settlement::ReplyShapeOf(&dbl), ReplyShape::kOther);

	EXPECT_EQ(battle_settlement::ReplyText(&str), std::string_view("701"));
	EXPECT_EQ(battle_settlement::ReplyText(&error), std::string_view("LOADING Redis is loading the dataset in memory"));
	EXPECT_TRUE(battle_settlement::ReplyText(&nil).empty());
	EXPECT_TRUE(battle_settlement::ReplyText(&integer).empty());
}

// 缺陷 1(高):发件箱第一跳把空应答 / ERROR 当成「已销账」并删条目。一次断线,在线玩家就收不到重投。
TEST(SettlementOutbox, AckProbeWithoutUsableReplyIsUndeterminedNotAcknowledged)
{
	for (const auto shape : {ReplyShape::kNoReply, ReplyShape::kError, ReplyShape::kStatus, ReplyShape::kInteger,
							 ReplyShape::kArray, ReplyShape::kOther})
	{
		EXPECT_EQ(battle_settlement::ClassifyAckProbe(shape, "", 701), battle_settlement::AckProbeVerdict::kUndetermined)
			<< battle_settlement::ReplyShapeName(shape);
		// 即使文本恰好等于本局 id,形状不对也不能下结论。
		EXPECT_EQ(battle_settlement::ClassifyAckProbe(shape, "701", 701), battle_settlement::AckProbeVerdict::kUndetermined)
			<< battle_settlement::ReplyShapeName(shape);
	}
}

// 只有 NIL,或 STRING 且不等于本局,才算销账;STRING 且等于本局才进第二跳。比较是十进制字符串逐字节。
TEST(SettlementOutbox, AckProbeOnlyNilOrForeignIdCountsAsAcknowledged)
{
	using battle_settlement::AckProbeVerdict;
	EXPECT_EQ(battle_settlement::ClassifyAckProbe(ReplyShape::kNil, "", 701), AckProbeVerdict::kAcknowledged);
	EXPECT_EQ(battle_settlement::ClassifyAckProbe(ReplyShape::kString, "702", 701), AckProbeVerdict::kAcknowledged);
	EXPECT_EQ(battle_settlement::ClassifyAckProbe(ReplyShape::kString, "0701", 701), AckProbeVerdict::kAcknowledged);
	EXPECT_EQ(battle_settlement::ClassifyAckProbe(ReplyShape::kString, "", 701), AckProbeVerdict::kAcknowledged);
	EXPECT_EQ(battle_settlement::ClassifyAckProbe(ReplyShape::kString, "701", 701), AckProbeVerdict::kStillOurs);
	EXPECT_EQ(battle_settlement::ClassifyAckProbe(ReplyShape::kString, "18446744073709551615",
												  std::numeric_limits<uint64_t>::max()),
			  AckProbeVerdict::kStillOurs);
}

// 调用点实际走的是「适配 + 判定」这条链:空应答进来,结论必须是 undetermined。
TEST(SettlementOutbox, AckProbeChainTreatsHiredisNullCallbackAsUndetermined)
{
	using battle_settlement::AckProbeVerdict;
	const redisReply *disconnected = nullptr;
	EXPECT_EQ(battle_settlement::ClassifyAckProbe(battle_settlement::ReplyShapeOf(disconnected),
												  battle_settlement::ReplyText(disconnected), 701),
			  AckProbeVerdict::kUndetermined);
	auto ours = MakeReply(REDIS_REPLY_STRING, "701");
	EXPECT_EQ(battle_settlement::ClassifyAckProbe(battle_settlement::ReplyShapeOf(&ours),
												  battle_settlement::ReplyText(&ours), 701),
			  AckProbeVerdict::kStillOurs);
	auto noAuth = MakeReply(REDIS_REPLY_ERROR, "NOAUTH Authentication required.");
	EXPECT_EQ(battle_settlement::ClassifyAckProbe(battle_settlement::ReplyShapeOf(&noAuth),
												  battle_settlement::ReplyText(&noAuth), 701),
			  AckProbeVerdict::kUndetermined);
}

// 缺陷 2(中):第二跳读位置拿到空应答 / ERROR 时走 kSkipNoTarget 并 ++attempts,白烧一轮;
// 耗尽时还会误记成 reason=no_target。没问到就不能进 ClassifyRetry。
TEST(SettlementOutbox, LocationReadWithoutUsableReplyIsNotADecision)
{
	EXPECT_FALSE(battle_settlement::IsLocationReplyDecisive(ReplyShape::kNoReply));
	EXPECT_FALSE(battle_settlement::IsLocationReplyDecisive(ReplyShape::kError));
	EXPECT_FALSE(battle_settlement::IsLocationReplyDecisive(ReplyShape::kInteger));
	EXPECT_FALSE(battle_settlement::IsLocationReplyDecisive(ReplyShape::kOther));
	// NIL = 玩家不在任何场景(真的无目标);STRING = 有位置记录(内容解析失败仍按无目标)。
	EXPECT_TRUE(battle_settlement::IsLocationReplyDecisive(ReplyShape::kNil));
	EXPECT_TRUE(battle_settlement::IsLocationReplyDecisive(ReplyShape::kString));
}

// 「没问到」不消耗重投次数,但单独有上限:持续 ERROR 时不设上限,发件箱随对局数无界增长、永远不响 undelivered。
TEST(SettlementOutbox, UndeterminedProbeRoundsAreBoundedSeparately)
{
	using battle_settlement::UndeterminedProbeAction;
	EXPECT_EQ(battle_settlement::ClassifyUndeterminedProbeRound(1, 60), UndeterminedProbeAction::kWaitNextRound);
	EXPECT_EQ(battle_settlement::ClassifyUndeterminedProbeRound(59, 60), UndeterminedProbeAction::kWaitNextRound);
	EXPECT_EQ(battle_settlement::ClassifyUndeterminedProbeRound(60, 60), UndeterminedProbeAction::kGiveUp);
	EXPECT_EQ(battle_settlement::ClassifyUndeterminedProbeRound(61, 60), UndeterminedProbeAction::kGiveUp);
	// 上限配成 0 是调用方缺陷:立即放弃,保证有界。
	EXPECT_EQ(battle_settlement::ClassifyUndeterminedProbeRound(1, 0), UndeterminedProbeAction::kGiveUp);
	EXPECT_STREQ(battle_settlement::UndeliveredReasonName(battle_settlement::UndeliveredReason::kProbeUndetermined),
				 "probe_undetermined");
}

// 缺陷 5 / 6(中 / 中低):落库应答没确认时只投一次、不进发件箱。没确认 ≠ 没落库,结论只能是 kUnconfirmed,
// 调用点据此照样登记发件箱(结算 EVAL 成功是整数,活动局 SET 成功是状态 OK)。
TEST(SettlementOutbox, PersistReplyWithoutConfirmationIsUnconfirmedNotFailed)
{
	using battle_settlement::PersistVerdict;
	EXPECT_EQ(battle_settlement::ClassifyPersistReply(ReplyShape::kInteger, ReplyShape::kInteger), PersistVerdict::kStored);
	EXPECT_EQ(battle_settlement::ClassifyPersistReply(ReplyShape::kStatus, ReplyShape::kStatus), PersistVerdict::kStored);
	for (const auto actual : {ReplyShape::kNoReply, ReplyShape::kError, ReplyShape::kNil, ReplyShape::kString,
							  ReplyShape::kArray, ReplyShape::kOther})
	{
		EXPECT_EQ(battle_settlement::ClassifyPersistReply(actual, ReplyShape::kInteger), PersistVerdict::kUnconfirmed)
			<< battle_settlement::ReplyShapeName(actual);
	}
	EXPECT_EQ(battle_settlement::ClassifyPersistReply(ReplyShape::kInteger, ReplyShape::kStatus), PersistVerdict::kUnconfirmed);
	// 防呆:空应答永远不是「成功形状」,哪怕调用方把期望形状写错成 kNoReply。
	EXPECT_EQ(battle_settlement::ClassifyPersistReply(ReplyShape::kNoReply, ReplyShape::kNoReply), PersistVerdict::kUnconfirmed);
}

// 缺陷 3(高):scene 登录钩子把空应答与 NIL 一起当「没有挂起结算」,转去按锁重建 —— 这次登录漏补发奖励。
TEST(SettlementLoginHook, PendingReadWithoutUsableReplyIsRecheckNotNoPending)
{
	using battle_settlement::LoginPendingRead;
	EXPECT_EQ(battle_settlement::ClassifyLoginPendingRead(ReplyShape::kNil), LoginPendingRead::kNoPending);
	EXPECT_EQ(battle_settlement::ClassifyLoginPendingRead(ReplyShape::kString), LoginPendingRead::kPending);
	for (const auto shape : {ReplyShape::kNoReply, ReplyShape::kError, ReplyShape::kStatus, ReplyShape::kInteger,
							 ReplyShape::kArray, ReplyShape::kOther})
	{
		EXPECT_EQ(battle_settlement::ClassifyLoginPendingRead(shape), LoginPendingRead::kRecheck)
			<< battle_settlement::ReplyShapeName(shape);
	}
	EXPECT_EQ(battle_settlement::ClassifyLoginPendingRead(battle_settlement::ReplyShapeOf(nullptr)),
			  LoginPendingRead::kRecheck);
}

// 缺陷 4(中低):登录重建读锁把空应答当「无锁」,战斗闸失效。空应答进复查;ERROR 按无锁是既有取舍(保留)。
TEST(SettlementLoginHook, LockReadNoReplyIsRecheckWhileErrorKeepsLegacyNoLock)
{
	using battle_settlement::LoginLockRead;
	EXPECT_EQ(battle_settlement::ClassifyLoginLockRead(ReplyShape::kNoReply), LoginLockRead::kRecheck);
	EXPECT_EQ(battle_settlement::ClassifyLoginLockRead(ReplyShape::kArray), LoginLockRead::kLock);
	EXPECT_EQ(battle_settlement::ClassifyLoginLockRead(ReplyShape::kNil), LoginLockRead::kNoLock);
	EXPECT_EQ(battle_settlement::ClassifyLoginLockRead(ReplyShape::kError), LoginLockRead::kNoLock);
	EXPECT_EQ(battle_settlement::ClassifyLoginLockRead(ReplyShape::kOther), LoginLockRead::kNoLock);
}

// 2026-10-19(第二、三轮评审 P3):reaper 判作废的局,删锁没落地时锁还指向它;按锁重建的入口必须认出它、不重建。
// 读到它的路径有三条(冻结到期补读没读到挂的复查、补应用另一局挂起结算之后的重建、登录挂的复查赶上冻结到期),
// 裁决放在重建入口,一处盖住。
TEST(SettlementLoginHook, LockRebuildSkipsTheBattleThisEntityAlreadyExpired)
{
	using battle_settlement::LockRebuildDecision;
	// 锁指向刚作废的 X,账本里没有 X:不重建(调用方补一次条件删锁)。
	EXPECT_EQ(battle_settlement::ClassifyLockRebuild(701, 701, false), LockRebuildDecision::kSkipExpired);
	// 锁指向另一局(真在打的下一局 / 别处的战斗):照常重建。
	EXPECT_EQ(battle_settlement::ClassifyLockRebuild(702, 701, false), LockRebuildDecision::kRebuild);
	// 本实体没有作废局(expiredBattleId 为 0):照常重建。
	EXPECT_EQ(battle_settlement::ClassifyLockRebuild(701, 0, false), LockRebuildDecision::kRebuild);
	// 槽位为 0 是「没有作废局」,不是一个能匹配的 battle_id:锁值碰巧也读成 0 时(解析异常的上游已拦,这里纵深防御)
	// 仍按重建处理,不能因为 0 == 0 判成「已作废」去补删锁。去掉 expiredBattleId != 0 这道守卫,这一条就会红。
	EXPECT_EQ(battle_settlement::ClassifyLockRebuild(0, 0, false), LockRebuildDecision::kRebuild);
}

// 顺序:已应用 > 已作废。作废之后删锁没落地,迟到的结算会按锁匹配应用;锁要留到它落盘销账才删,
// 这时只能推进销账,补删锁会提前放开排队。
TEST(SettlementLoginHook, LockRebuildAppliedBeatsExpired)
{
	using battle_settlement::LockRebuildDecision;
	EXPECT_EQ(battle_settlement::ClassifyLockRebuild(701, 701, true), LockRebuildDecision::kAdvanceAck);
	EXPECT_EQ(battle_settlement::ClassifyLockRebuild(701, 0, true), LockRebuildDecision::kAdvanceAck);
	EXPECT_EQ(battle_settlement::ClassifyLockRebuild(702, 701, true), LockRebuildDecision::kAdvanceAck);
}

// reaper 节拍对一个复查标记的处理,全部 8 种状态:退出中 > 交接中 > Redis 不可用 > 重跑。
// bag_test 里 Redis 恒不可用,分不清「交接中保留」与「Redis 不可用保留」,这里逐格盯住。
TEST(SettlementLoginHook, RecheckEntryDecisionCoversEveryState)
{
	using battle_settlement::LoginRecheckStep;
	const auto classify = [](const bool exiting, const bool handoff, const bool redisReady)
	{
		battle_settlement::LoginRecheckEntryState state;
		state.exitingOrNoIdentity = exiting;
		state.travelHandoffOrFrozen = handoff;
		state.redisReady = redisReady;
		return battle_settlement::ClassifyLoginRecheckEntry(state);
	};
	// 退出中:不论其它条件一律摘标记、不重跑。
	for (const bool handoff : {false, true})
	{
		for (const bool redisReady : {false, true})
		{
			EXPECT_EQ(classify(true, handoff, redisReady), LoginRecheckStep::kDrop)
				<< "handoff=" << handoff << " redisReady=" << redisReady;
		}
	}
	// 交接中:Redis 可用也保留,不能在即将销毁的源实体上重跑。
	EXPECT_EQ(classify(false, true, true), LoginRecheckStep::kKeep);
	EXPECT_EQ(classify(false, true, false), LoginRecheckStep::kKeep);
	// Redis 不可用:保留,恢复后再跑。
	EXPECT_EQ(classify(false, false, false), LoginRecheckStep::kKeep);
	// 只有这一格真正重跑。
	EXPECT_EQ(classify(false, false, true), LoginRecheckStep::kRun);
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
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7});
    std::vector<uint32_t> attempted;
    const auto first = gate_scene_route::RebindSceneNode(session, entt::entity{9});
    ASSERT_EQ(SceneNodeChange::kChanged, first);
    EXPECT_FALSE(gate_scene_route::ApplyRoute(session, 2001, first,
                                              [&](uint32_t type) { attempted.push_back(type); return false; }));
    EXPECT_EQ((std::vector<uint32_t>{0}), attempted);
    // 同一事件重投:指向已是 9,判成未变化,不再转发。
    const auto redelivered = gate_scene_route::RebindSceneNode(session, entt::entity{9});
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
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::RebindSceneNode(session, entt::entity{7}));
    EXPECT_EQ(entt::entity{7}, session.GetNodeEntity(eNodeType::SceneNodeService));
    // 同实体:重复路由。
    EXPECT_EQ(SceneNodeChange::kUnchanged, gate_scene_route::RebindSceneNode(session, entt::entity{7}));
    // 不同实体:换了节点,指向被改写。
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::RebindSceneNode(session, entt::entity{9}));
    EXPECT_EQ(entt::entity{9}, session.GetNodeEntity(eNodeType::SceneNodeService));
    EXPECT_EQ(SceneNodeChange::kUnchanged, gate_scene_route::RebindSceneNode(session, entt::entity{9}));
}

// scene 节点被摘除时 gate 会把会话指向置为 entt::null(OnNodeRemoveEventHandler)。
// 之后无论改派到新节点,还是同一进程重启后恰好复用了同一个实体号,都必须算换了节点、重新进场。
TEST(SceneRouteEntry, RebindSceneNodeAfterNodeRemovalCountsAsChanged)
{
    SessionInfo session;
    session.sceneId = 2001;
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7});
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::null);
    const auto change = gate_scene_route::RebindSceneNode(session, entt::entity{7});
    EXPECT_EQ(SceneNodeChange::kChanged, change);
    EXPECT_TRUE(session.HasNodeEntity(eNodeType::SceneNodeService));
    std::vector<uint32_t> forwarded;
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 2001, change,
                                             [&](uint32_t type) { forwarded.push_back(type); return true; }));
    EXPECT_EQ((std::vector<uint32_t>{0}), forwarded);
}

// 类型契约固定用例：F033 为纯类型重构，改造前 64 位存储同样判 kChanged；守护将来收窄为 index/uint32 的回退，无修复前红。
TEST(SceneRouteEntry, RecycledNodeIndexWithAnotherVersionCountsAsChanged)
{
    entt::registry nodes;
    const auto previous = nodes.create();
    SessionInfo session;
    session.SetNodeEntity(eNodeType::SceneNodeService, previous);
    nodes.destroy(previous);
    const auto replacement = nodes.create();

    ASSERT_EQ(entt::entt_traits<entt::entity>::to_entity(previous),
              entt::entt_traits<entt::entity>::to_entity(replacement));
    ASSERT_NE(previous, replacement);
    EXPECT_FALSE(nodes.valid(session.GetNodeEntity(eNodeType::SceneNodeService)));
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::RebindSceneNode(session, replacement));
    EXPECT_EQ(replacement, session.GetNodeEntity(eNodeType::SceneNodeService));
}

// 别的节点类型的指向不参与判定,也不被改写。
TEST(SceneRouteEntry, RebindSceneNodeIgnoresOtherNodeTypes)
{
    SessionInfo session;
    session.SetNodeEntity(eNodeType::BattleNodeService, entt::entity{7});
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7});
    EXPECT_EQ(SceneNodeChange::kUnchanged, gate_scene_route::RebindSceneNode(session, entt::entity{7}));
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::RebindSceneNode(session, entt::entity{8}));
    EXPECT_EQ(entt::entity{7}, session.GetNodeEntity(eNodeType::BattleNodeService));
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
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7});
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::CompareSceneNode(session, entt::entity{9}));
    EXPECT_EQ(entt::entity{7}, session.GetNodeEntity(eNodeType::SceneNodeService));
    EXPECT_EQ(SceneNodeChange::kUnchanged, gate_scene_route::CompareSceneNode(session, entt::entity{7}));

    // 节点被摘除后指向置无效:再路由回同一个实体号也算换了节点。
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::null);
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::CompareSceneNode(session, entt::entity{7}));

    // 别的节点类型的指向不参与判定,也不被改写。
    SessionInfo battleOnly;
    battleOnly.SetNodeEntity(eNodeType::BattleNodeService, entt::entity{7});
    EXPECT_EQ(SceneNodeChange::kChanged, gate_scene_route::CompareSceneNode(battleOnly, entt::entity{7}));
    EXPECT_EQ(entt::entity{7}, battleOnly.GetNodeEntity(eNodeType::BattleNodeService));
    EXPECT_FALSE(battleOnly.HasNodeEntity(eNodeType::SceneNodeService));
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
    using ForwardCall = std::tuple<uint32_t, entt::entity, uint64_t>;

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

    std::optional<entt::entity> resolvedEntity;
    std::vector<ForwardCall> calls;
    auto resolve = [&](const uint32_t nodeId)
    {
        EXPECT_EQ(7u, nodeId);
        return resolvedEntity;
    };
    auto forward = [&](const uint32_t enterType, const entt::entity nodeEntity, const uint64_t sceneId)
    {
        calls.emplace_back(enterType, nodeEntity, sceneId);
        return SceneForwardResult::kSent;
    };

    EXPECT_EQ(SceneEntryAttempt::kRetryLater,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ(0u, session.sceneId);
    EXPECT_EQ(1u, session.pendingEnterGsType);
    EXPECT_FALSE(session.HasNodeEntity(eNodeType::SceneNodeService));
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(1u, session.pendingSceneEntry->attempts);
    EXPECT_EQ(SceneForwardResult::kNodeNotFound, session.pendingSceneEntry->lastFailure);
    EXPECT_TRUE(calls.empty());

    resolvedEntity = entt::entity{55};
    const auto secondAt = session.pendingSceneEntry->nextAttemptAt;
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, secondAt, resolve, forward));
    EXPECT_EQ((std::vector<ForwardCall>{ForwardCall{1u, entt::entity{55}, 1001u}}), calls);
    EXPECT_EQ(1001u, session.sceneId);
    EXPECT_EQ(0u, session.pendingEnterGsType);
    EXPECT_FALSE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(entt::entity{55}, session.GetNodeEntity(eNodeType::SceneNodeService));

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
    auto resolve = [](uint32_t) { return std::optional<entt::entity>{entt::entity{9}}; };
    auto forward = [&](const uint32_t enterType, entt::entity, uint64_t)
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
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7});
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 9, 2001}, kT0);

    std::vector<uint32_t> forwardedTypes;
    auto resolve = [](uint32_t) { return std::optional<entt::entity>{entt::entity{9}}; };
    auto forward = [&](const uint32_t enterType, entt::entity, uint64_t)
    {
        forwardedTypes.push_back(enterType);
        return forwardedTypes.size() == 1 ? SceneForwardResult::kNotConnected : SceneForwardResult::kSent;
    };

    EXPECT_EQ(SceneEntryAttempt::kRetryLater,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ(entt::entity{7}, session.GetNodeEntity(eNodeType::SceneNodeService)); // 没提交
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, session.pendingSceneEntry->nextAttemptAt, resolve,
                                                         forward));
    EXPECT_EQ((std::vector<uint32_t>{0, 0}), forwardedTypes);
    EXPECT_EQ(entt::entity{9}, session.GetNodeEntity(eNodeType::SceneNodeService));
}

// 失败窗口内 sceneId 与指向都不动:断线 ExitGame 与客户端消息仍发往旧节点(I5)。
TEST(SceneEntryAttempt, FailedAttemptsNeverCommitTheNewRoute)
{
    SessionInfo session;
    session.playerId = 42;
    session.sceneId = 2001;
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7});
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 9, 3001}, kT0);

    const std::vector<SceneForwardResult> script{SceneForwardResult::kHandshakePending,
                                                 SceneForwardResult::kNotConnected,
                                                 SceneForwardResult::kSent};
    std::vector<uint32_t> forwardedTypes;
    auto resolve = [](uint32_t) { return std::optional<entt::entity>{entt::entity{9}}; };
    auto forward = [&](const uint32_t enterType, entt::entity, uint64_t)
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
        EXPECT_EQ(entt::entity{7}, session.GetNodeEntity(eNodeType::SceneNodeService));
        ASSERT_TRUE(session.pendingSceneEntry.has_value());
        now = session.pendingSceneEntry->nextAttemptAt;
    }
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, now, resolve, forward));
    EXPECT_EQ(3001u, session.sceneId);
    EXPECT_EQ(entt::entity{9}, session.GetNodeEntity(eNodeType::SceneNodeService));
    EXPECT_EQ((std::vector<uint32_t>{0, 0, 0}), forwardedTypes);
}

// 同节点、同 scene、无登录类型:本来就不需要转发,欠账直接结清。
TEST(SceneEntryAttempt, DuplicateRouteNeedsNoForwardAndClearsDebt)
{
    SessionInfo session;
    session.sceneId = 2001;
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{9});
    session.pendingEnterGsType = 0;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 9, 2001}, kT0);

    auto resolve = [](uint32_t) { return std::optional<entt::entity>{entt::entity{9}}; };
    auto forward = [](uint32_t, entt::entity, uint64_t)
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
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7});
    session.pendingEnterGsType = 1;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 9, 3001}, kT0);

    auto resolve = [](uint32_t) { return std::optional<entt::entity>{entt::entity{9}}; };
    auto forward = [](uint32_t, entt::entity, uint64_t) { return SceneForwardResult::kNotConnected; };
    EXPECT_EQ(SceneEntryAttempt::kGiveUp,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0 + gate_scene_route::kSceneEntryRetryBudget,
                                                         resolve, forward));
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(SceneForwardResult::kNotConnected, session.pendingSceneEntry->lastFailure);
    EXPECT_EQ(1u, session.pendingEnterGsType);
    EXPECT_EQ(entt::entity{7}, session.GetNodeEntity(eNodeType::SceneNodeService));
    EXPECT_EQ(2001u, session.sceneId);
}

// scene_id 为 0 的路由以前被静默忽略,现在 fail-closed:不解析、不转发、不动会话,直接放弃。
TEST(SceneEntryAttempt, ZeroSceneIdIsGivenUpWithoutTouchingSession)
{
    SessionInfo session;
    session.sceneId = 1001;
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7});
    session.pendingEnterGsType = 1;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{0, 9, 0}, kT0);

    auto resolve = [](uint32_t)
    {
        ADD_FAILURE() << "无效路由不应解析节点";
        return std::optional<entt::entity>{};
    };
    auto forward = [](uint32_t, entt::entity, uint64_t)
    {
        ADD_FAILURE() << "无效路由不应转发";
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kGiveUp,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(SceneForwardResult::kInvalidRoute, session.pendingSceneEntry->lastFailure);
    EXPECT_EQ(1001u, session.sceneId);
    EXPECT_EQ(entt::entity{7}, session.GetNodeEntity(eNodeType::SceneNodeService));
    EXPECT_EQ(1u, session.pendingEnterGsType);
}

// 同一条连接上已换成另一名角色:绝不以错的角色补发,也不动会话,由调用方撤销欠账。
TEST(SceneEntryAttempt, PlayerMismatchCancelsWithoutForwarding)
{
    SessionInfo session;
    session.playerId = 43;
    session.sceneId = 1001;
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7});
    session.pendingEnterGsType = 1;
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 9, 2001}, kT0);

    auto resolve = [](uint32_t)
    {
        ADD_FAILURE() << "角色不符不应解析节点";
        return std::optional<entt::entity>{entt::entity{9}};
    };
    auto forward = [](uint32_t, entt::entity, uint64_t)
    {
        ADD_FAILURE() << "角色不符不应转发";
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kPlayerMismatch,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    ASSERT_TRUE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(0u, session.pendingSceneEntry->attempts);
    EXPECT_EQ(1001u, session.sceneId);
    EXPECT_EQ(entt::entity{7}, session.GetNodeEntity(eNodeType::SceneNodeService));
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
    auto resolve = [](uint32_t) { return std::optional<entt::entity>{entt::entity{9}}; };
    auto forward = [&](uint32_t, entt::entity, uint64_t)
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
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::null);
    session.lastSceneRoute = SceneRouteTarget{42, 7, 1001};
    session.playerId = 42;
    session.pendingEnterGsType = 1;

    auto entry = gate_scene_route::EntryForLateLoginBinding(session, kT0);
    ASSERT_TRUE(entry.has_value());
    session.pendingSceneEntry = *entry;

    auto resolve = [](uint32_t) { return std::optional<entt::entity>{}; };
    auto forward = [](uint32_t, entt::entity, uint64_t)
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

// 同 uuid 重注册留下悬空实体号(HasNodeEntity 仍为 true):按 node_id 重新解析到新实体,以登录类型转发。
TEST(SceneEntryLateLogin, LoginBoundAfterRouteToReRegisteredNodeResolvesByNodeId)
{
    SessionInfo session;
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{7}); // 已销毁的旧实体号
    ASSERT_TRUE(session.HasNodeEntity(eNodeType::SceneNodeService));
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
        return std::optional<entt::entity>{entt::entity{12}};
    };
    auto forward = [&](const uint32_t enterType, const entt::entity nodeEntity, const uint64_t sceneId)
    {
        calls.emplace_back(enterType, nodeEntity, sceneId);
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ((std::vector<ForwardCall>{ForwardCall{1u, entt::entity{12}, 1001u}}), calls);
    EXPECT_EQ(entt::entity{12}, session.GetNodeEntity(eNodeType::SceneNodeService));
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

// ===========================================================================
// 同节点新 epoch 的路由与 LeaveGame 重置(docs/design/location-release-binding.md §3.5;审计
// data-overwrite-rollback-audit-20261018.md §2.1 load-handover#0)。
//
// scene_manager 在 location 缺失后按首次落点铸了新 epoch、又派回同一节点同一频道时,只按场景与节点判的 ApplyRoute 会把这条
// 路由当重复事件吞掉,持有节点的实体永远停在旧 epoch,下一次写盘被 owner CAS 拒绝、按废黜销毁(回档)。第 4 条:scene 此前被
// 告知过非 0 的 epoch、而路由的 epoch 更大时,以 0 再转发一次。客户端 LeaveGame 时 gate 先代发 ExitGame、再 ResetSceneRoute,
// 之后的进场只由新路由驱动,不按旧路由补发。转发器在转发那一刻读会话上的 ownerEpoch,与生产 ForwardPlayerToScene 同形。
// ===========================================================================

namespace
{
    constexpr uint64_t kEpochBeforeRelogin = 7;
    constexpr uint64_t kEpochAfterRelogin = 8; // location 被删后首次落点铸出的下一代
    constexpr uint64_t kReloginGeneration = 1790000009000ull;
    constexpr uint32_t kReloginEnterType = 2; // LOGIN_REPLACE

    // (enterType, 转发那一刻会话上的 ownerEpoch)
    using EpochStampedForward = std::pair<uint32_t, uint64_t>;

    // 已在节点实体 12(node_id 3)的 scene 1001 里、scene 已被告知 kEpochBeforeRelogin 的会话。
    SessionInfo SessionInGameAtEpoch()
    {
        SessionInfo session;
        session.playerId = 42;
        session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{12});
        session.sceneId = 1001;
        session.lastSceneRoute = SceneRouteTarget{42, 3, 1001};
        session.homeZoneId = 1;
        session.ownerEpoch = kEpochBeforeRelogin;
        session.forwardedOwnerEpoch = kEpochBeforeRelogin;
        session.loginGeneration = kReloginGeneration - 1;
        return session;
    }
} // namespace

TEST(SceneRouteEntry, SameNodeNewerEpochForwardsOnce)
{
    SessionInfo session = SessionInGameAtEpoch();
    std::vector<EpochStampedForward> forwarded;
    auto send = [&](uint32_t type) { forwarded.emplace_back(type, session.ownerEpoch); return true; };

    session.ownerEpoch = kEpochAfterRelogin; // ApplyRouteIdentity 已把新路由的 epoch 写进会话
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ((std::vector<EpochStampedForward>{{0u, kEpochAfterRelogin}}), forwarded)
        << "同场景同节点,但归属换了代:必须让持有节点知道新 epoch";
    EXPECT_EQ(kEpochAfterRelogin, session.forwardedOwnerEpoch);

    // 同一条路由重投:scene 已拿到这一代,回到重复事件语义。
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ(1u, forwarded.size());
}

TEST(SceneRouteEntry, SameNodeSameOrOlderEpochStaysDeduped)
{
    SessionInfo session = SessionInGameAtEpoch();
    int sends = 0;
    auto send = [&](uint32_t) { ++sends; return true; };

    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ(0, sends) << "同一代:重复事件";

    session.ownerEpoch = kEpochBeforeRelogin - 1; // 乱序到达的旧路由
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ(0, sends) << "epoch 只增不减,迟到的旧路由不再转发";
    EXPECT_EQ(kEpochBeforeRelogin, session.forwardedOwnerEpoch);
}

TEST(SceneRouteEntry, ZeroEpochRouteNeverForwardsByEpoch)
{
    // 旧版 scene_manager 不铸造(0):不触发第 4 条。
    SessionInfo session = SessionInGameAtEpoch();
    int sends = 0;
    auto send = [&](uint32_t) { ++sends; return true; };
    session.ownerEpoch = 0;
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ(0, sends);

    // scene 从没被告知过 epoch(路由先于登录类型到达、还没转发过):下一次转发会带上会话当前值,不为此多发一次 0 型进场。
    SessionInfo fresh;
    fresh.sceneId = 1001;
    fresh.ownerEpoch = kEpochAfterRelogin;
    ASSERT_TRUE(gate_scene_route::ApplyRoute(fresh, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ(0, sends);
    EXPECT_EQ(0u, fresh.forwardedOwnerEpoch);
}

TEST(SceneRouteEntry, OwnerRevisionKeepsOriginalRouteIdentity)
{
    SessionInfo session = SessionInGameAtEpoch();
    session.ownerRevision = 19;
    session.forwardedOwnerRevision = 19;

    // 迟到的卖家路由必须保留原修订号,交给scene/SQL拒绝,不能借用较新的买家身份。
    gate_scene_route::ApplyRouteIdentity(session, 42, 1, kEpochBeforeRelogin, kReloginGeneration, 18);
    EXPECT_EQ(18u, session.ownerRevision);
    uint64_t sentRevision = 0;
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged,
        [&](uint32_t type) { EXPECT_EQ(0u, type); sentRevision = session.ownerRevision; return true; }));
    EXPECT_EQ(18u, sentRevision);
    EXPECT_EQ(18u, session.forwardedOwnerRevision);
}

TEST(SceneRouteEntry, OwnerRevisionForwardFailureKeepsRetryAndThenDeduplicates)
{
    SessionInfo session = SessionInGameAtEpoch();
    session.ownerRevision = 20;
    session.forwardedOwnerRevision = 19;
    int sends = 0;
    EXPECT_FALSE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged,
        [&](uint32_t) { ++sends; return false; }));
    EXPECT_EQ(19u, session.forwardedOwnerRevision);

    auto send = [&](uint32_t type) { EXPECT_EQ(0u, type); ++sends; return true; };
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ(20u, session.forwardedOwnerRevision);
    ASSERT_TRUE(gate_scene_route::ApplyRoute(session, 1001, SceneNodeChange::kUnchanged, send));
    EXPECT_EQ(2, sends);
}

TEST(SceneRouteEntry, LeaveGameClearsBothOwnerRevisionCopies)
{
    SessionInfo session = SessionInGameAtEpoch();
    session.ownerRevision = 20;
    session.forwardedOwnerRevision = 19;
    gate_scene_route::ResetSceneRoute(session);
    EXPECT_EQ(0u, session.ownerRevision);
    EXPECT_EQ(0u, session.forwardedOwnerRevision);
}

// 审计用例「gate 在 Bind 先到、按旧路由补发之后又收到同节点新 epoch 路由」:不经 LeaveGame 重置的残留路径(例如 LeaveGame
// 之外的同连接重登)。补发带旧 epoch;随后新 epoch 的路由同场景同节点,第 4 条判更大 → 以 0 再转发,持有节点抬到新一代。
TEST(SceneRouteEntry, BindReplayThenSameNodeNewerEpochIsForwarded)
{
    SessionInfo session = SessionInGameAtEpoch();
    std::vector<EpochStampedForward> calls;
    auto resolve = [](const uint32_t nodeId)
    {
        EXPECT_EQ(3u, nodeId);
        return std::optional<entt::entity>{entt::entity{12}};
    };
    auto forward = [&](const uint32_t enterType, const entt::entity, const uint64_t)
    {
        calls.emplace_back(enterType, session.ownerEpoch);
        return SceneForwardResult::kSent;
    };

    // BindSession 先到(OnLoginTypeBound 的调用序):按 lastSceneRoute 补发,带的是会话里旧路由的 epoch。
    ASSERT_TRUE(gate_scene_route::ApplyLoginBinding(session, 42, 2, kReloginGeneration, kReloginEnterType));
    auto entry = gate_scene_route::EntryForLateLoginBinding(session, kT0);
    ASSERT_TRUE(entry.has_value());
    session.pendingSceneEntry = *entry;
    EXPECT_EQ(SceneEntryAttempt::kApplied, gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    ASSERT_EQ((std::vector<EpochStampedForward>{{kReloginEnterType, kEpochBeforeRelogin}}), calls);

    // 随后新路由到达:同场景同节点、epoch 更大(OnRouteDecision 的调用序)。
    gate_scene_route::ApplyRouteIdentity(session, 42, 1, kEpochAfterRelogin, kReloginGeneration);
    session.lastSceneRoute = SceneRouteTarget{42, 3, 1001};
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 3, 1001}, kT0);
    EXPECT_EQ(SceneEntryAttempt::kApplied, gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ((std::vector<EpochStampedForward>{{kReloginEnterType, kEpochBeforeRelogin}, {0u, kEpochAfterRelogin}}), calls)
        << "旧代码在这里判重复事件吞掉,持有节点停在旧 epoch";
    EXPECT_EQ(kEpochAfterRelogin, session.forwardedOwnerEpoch);
    EXPECT_FALSE(session.pendingSceneEntry.has_value());
}

// 审计用例「LeaveGame 后不断线就重进」:gate 收到 LeaveGame 时清空路由状态(先代发 ExitGame,由 OnClientLeaveGame 负责);
// 之后同连接的 Bind 先到也不按旧路由补发,等新路由到达再带着登录类型与新 epoch 一起转发。
TEST(SceneRouteEntry, LeaveGameResetThenRelogForwardsFreshRouteOnly)
{
    SessionInfo session = SessionInGameAtEpoch();
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 3, 1001}, kT0);

    gate_scene_route::ResetSceneRoute(session);
    EXPECT_FALSE(session.HasNodeEntity(eNodeType::SceneNodeService));
    EXPECT_EQ(0u, session.sceneId);
    EXPECT_EQ(0u, session.pendingEnterGsType);
    EXPECT_FALSE(session.pendingSceneEntry.has_value());
    EXPECT_EQ(0u, session.lastSceneRoute.sceneId);
    EXPECT_EQ(0u, session.ownerEpoch);
    EXPECT_EQ(0u, session.homeZoneId);
    EXPECT_EQ(0u, session.forwardedOwnerEpoch);
    EXPECT_EQ(42u, session.playerId) << "角色绑定照旧保留,下一次 BindSession 按既有规则处理";
    EXPECT_EQ(kReloginGeneration - 1, session.loginGeneration) << "代次只抬高,不随 LeaveGame 清零";
    EXPECT_TRUE(session.sceneRouteResetPending);
    EXPECT_FALSE(session.enterGameSentAfterLeave);
    EXPECT_EQ(42u, session.playerIdAtReset) << "被离开的那条登录链属于会话当时的角色";
    EXPECT_EQ(kReloginGeneration - 1, session.loginGenerationAtReset);

    std::vector<EpochStampedForward> calls;
    auto resolve = [](const uint32_t) { return std::optional<entt::entity>{entt::entity{12}}; };
    auto forward = [&](const uint32_t enterType, const entt::entity, const uint64_t)
    {
        calls.emplace_back(enterType, session.ownerEpoch);
        return SceneForwardResult::kSent;
    };

    // 客户端在同一连接上再次 EnterGame(gate 收包时记下),之后重登的 BindSession 先到:没有旧路由可补发。
    gate_scene_route::NoteEnterGameRequested(session);
    ASSERT_FALSE(gate_scene_route::IsBindFromBeforeLeaveGame(session, 42, kReloginGeneration));
    ASSERT_TRUE(gate_scene_route::ApplyLoginBinding(session, 42, 3, kReloginGeneration, kReloginEnterType));
    EXPECT_FALSE(gate_scene_route::EntryForLateLoginBinding(session, kT0).has_value())
        << "不得按 LeaveGame 之前的旧路由(旧 epoch)补发";
    EXPECT_TRUE(calls.empty());

    // 新路由到达(location 删掉后首次落点铸了下一代,恰好又派回同一节点同一频道)。
    gate_scene_route::ApplyRouteIdentity(session, 42, 1, kEpochAfterRelogin, kReloginGeneration);
    session.lastSceneRoute = SceneRouteTarget{42, 3, 1001};
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 3, 1001}, kT0);
    EXPECT_EQ(SceneEntryAttempt::kApplied, gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ((std::vector<EpochStampedForward>{{kReloginEnterType, kEpochAfterRelogin}}), calls);
    EXPECT_EQ(1001u, session.sceneId);
    EXPECT_EQ(entt::entity{12}, session.GetNodeEntity(eNodeType::SceneNodeService));
    EXPECT_EQ(kEpochAfterRelogin, session.forwardedOwnerEpoch);
    EXPECT_EQ(0u, session.pendingEnterGsType);
    EXPECT_FALSE(session.sceneRouteResetPending) << "LeaveGame 之后的第一次进场已交给 scene,恢复常规路由处理";
}

// LeaveGame 重置之后、重登之前迟到的旧路由(已在 Kafka 里的跨节点换图,或路由模式下仍在 lane 里的 EnterGame 发出的路由):
// 属于 LeaveGame 之前那条登录链,在路由入口丢弃,不得重新填上 lastSceneRoute —— 否则下一次 BindSession 会按它、带旧 epoch
// 补发(静态评审复核成立的缺陷,location-release-binding.md §3.5 R5)。scene 自发的路由(代次 0)同样丢。
TEST(SceneRouteEntry, LateOldRouteAfterLeaveGameResetIsDroppedAndNotReplayedOnBind)
{
    SessionInfo session = SessionInGameAtEpoch();
    gate_scene_route::ResetSceneRoute(session);
    ASSERT_TRUE(session.sceneRouteResetPending);
    EXPECT_EQ(kReloginGeneration - 1, session.loginGenerationAtReset);

    // 客户端再次 EnterGame 之前:任何路由都只可能来自旧链(第二轮审计 r2-gate#1),代次再新、别的角色的也丢。
    EXPECT_TRUE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 42, kReloginGeneration));
    EXPECT_TRUE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 43, kReloginGeneration - 1));
    gate_scene_route::NoteEnterGameRequested(session);

    // 之后按代次认:旧链的路由(代次不大于重置时的值)与 scene 自发的路由(0)都认作 LeaveGame 之前的;新登录的路由不是。
    EXPECT_TRUE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 42, kReloginGeneration - 1));
    EXPECT_TRUE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 42, kReloginGeneration - 2));
    EXPECT_TRUE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 42, 0));
    EXPECT_TRUE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 0, kReloginGeneration - 1)) << "路由没带玩家时按会话的玩家判";
    EXPECT_FALSE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 42, kReloginGeneration));
    EXPECT_FALSE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 43, kReloginGeneration - 1))
        << "别的角色的路由由玩家围栏处理,不在这里判";

    // 生产入口(RoutePlayerEventHandler)判为旧路由即返回,会话字段一个都不写:lastSceneRoute 仍是空的,
    // 重登的 BindSession 先到时没有旧路由可补发。
    ASSERT_TRUE(gate_scene_route::ApplyLoginBinding(session, 42, 3, kReloginGeneration, kReloginEnterType));
    EXPECT_TRUE(session.sceneRouteResetPending) << "同一角色、带代次的新登录:仍按代次拦旧路由,直到真正进场";
    EXPECT_FALSE(gate_scene_route::EntryForLateLoginBinding(session, kT0).has_value());
    EXPECT_TRUE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 42, kReloginGeneration - 1))
        << "Bind 之后迟到的旧路由照样丢:否则它会带着新登录类型被转发到旧节点";

    // 新登录的路由到达,带登录类型与新 epoch 转发一次。
    std::vector<EpochStampedForward> calls;
    auto resolve = [](const uint32_t) { return std::optional<entt::entity>{entt::entity{12}}; };
    auto forward = [&](const uint32_t enterType, const entt::entity, const uint64_t)
    {
        calls.emplace_back(enterType, session.ownerEpoch);
        return SceneForwardResult::kSent;
    };
    ASSERT_FALSE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 42, kReloginGeneration));
    gate_scene_route::ApplyRouteIdentity(session, 42, 1, kEpochAfterRelogin, kReloginGeneration);
    session.lastSceneRoute = SceneRouteTarget{42, 3, 1001};
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 3, 1001}, kT0);
    EXPECT_EQ(SceneEntryAttempt::kApplied, gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ((std::vector<EpochStampedForward>{{kReloginEnterType, kEpochAfterRelogin}}), calls);
    EXPECT_FALSE(session.sceneRouteResetPending);
    EXPECT_FALSE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 42, 0))
        << "进场之后 scene 自发的换图路由(代次 0)恢复常规处理,不能被吞掉";
}

// 新登录的路由先于它的 BindSession 到达(重排):代次大于重置时的值,照常收下并记进 lastSceneRoute,Bind 到达后按它补发。
TEST(SceneRouteEntry, NewLoginRouteBeforeBindAfterLeaveGameStillReplays)
{
    SessionInfo session = SessionInGameAtEpoch();
    gate_scene_route::ResetSceneRoute(session);
    gate_scene_route::NoteEnterGameRequested(session); // 新登录的路由只可能在客户端再次 EnterGame 之后出现
    std::vector<EpochStampedForward> calls;
    auto resolve = [](const uint32_t) { return std::optional<entt::entity>{entt::entity{12}}; };
    auto forward = [&](const uint32_t enterType, const entt::entity, const uint64_t)
    {
        calls.emplace_back(enterType, session.ownerEpoch);
        return SceneForwardResult::kSent;
    };

    ASSERT_FALSE(gate_scene_route::IsRouteFromBeforeLeaveGame(session, 42, kReloginGeneration));
    gate_scene_route::ApplyRouteIdentity(session, 42, 1, kEpochAfterRelogin, kReloginGeneration);
    session.lastSceneRoute = SceneRouteTarget{42, 3, 1001};
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{42, 3, 1001}, kT0);
    EXPECT_EQ(SceneEntryAttempt::kApplied, gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_TRUE(calls.empty()) << "还没有登录类型:只提交、不转发";

    ASSERT_TRUE(gate_scene_route::ApplyLoginBinding(session, 42, 3, kReloginGeneration, kReloginEnterType));
    auto entry = gate_scene_route::EntryForLateLoginBinding(session, kT0);
    ASSERT_TRUE(entry.has_value());
    session.pendingSceneEntry = *entry;
    EXPECT_EQ(SceneEntryAttempt::kApplied, gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ((std::vector<EpochStampedForward>{{kReloginEnterType, kEpochAfterRelogin}}), calls);
    EXPECT_FALSE(session.sceneRouteResetPending);
}

// LeaveGame 之后换了角色,或旧版 login 的 Bind 不带代次:代次判据失效,退回常规处理(不拦任何路由)。
TEST(SceneRouteEntry, LeaveGameRouteFilterYieldsToCharacterSwitchAndLegacyBind)
{
    SessionInfo switched = SessionInGameAtEpoch();
    gate_scene_route::ResetSceneRoute(switched);
    gate_scene_route::NoteEnterGameRequested(switched);
    ASSERT_FALSE(gate_scene_route::IsBindFromBeforeLeaveGame(switched, 77, kReloginGeneration - 100))
        << "别的角色的 Bind:代次不可比,不按被离开的那条链判";
    ASSERT_TRUE(gate_scene_route::ApplyLoginBinding(switched, 77, 1, kReloginGeneration - 100, kReloginEnterType));
    EXPECT_FALSE(switched.sceneRouteResetPending) << "另一个角色的代次序列与重置时记下的值无关";
    EXPECT_FALSE(gate_scene_route::IsRouteFromBeforeLeaveGame(switched, 77, kReloginGeneration - 100));

    SessionInfo legacy = SessionInGameAtEpoch();
    gate_scene_route::ResetSceneRoute(legacy);
    gate_scene_route::NoteEnterGameRequested(legacy);
    ASSERT_FALSE(gate_scene_route::IsBindFromBeforeLeaveGame(legacy, 42, /*loginGeneration=*/0))
        << "旧版 login 的 Bind 不带代次:不丢,交给 ApplyLoginBinding 退回常规处理";
    ASSERT_TRUE(gate_scene_route::ApplyLoginBinding(legacy, 42, 3, /*loginGeneration=*/0, kReloginEnterType));
    EXPECT_FALSE(legacy.sceneRouteResetPending) << "旧版 login 不铸代次:分不出新旧,不拦";
    EXPECT_FALSE(gate_scene_route::IsRouteFromBeforeLeaveGame(legacy, 42, 0));
}

// ---------------------------------------------------------------------------
// 第二轮审计 r2-gate#1(data-overwrite-rollback-audit-r2-20261019.md §3.1):LeaveGame 先于本次登录的 Bind 到达 gate。
// 下面的 GateEventReplay 按生产入口的调用序复现 RoutePlayerEventHandler / BindSessionEventHandler(gate_event_handler.cpp)
// 与 OnRouteDecision / OnLoginTypeBound(scene_entry_dispatch.cpp)里的纯逻辑:先判旧链(丢弃时记代次),再写会话、建欠账、尝试转发。
// 节点 X = node_id 3(实体 12),节点 Y = node_id 5(实体 15)。
// ---------------------------------------------------------------------------

namespace
{
    constexpr uint32_t kNodeX = 3;
    constexpr uint32_t kNodeY = 5;
    constexpr entt::entity kNodeXEntity{12};
    constexpr entt::entity kNodeYEntity{15};

    // (enterType, 目标节点实体, 转发那一刻会话上的 ownerEpoch)
    using NodeStampedForward = std::tuple<uint32_t, entt::entity, uint64_t>;

    struct GateEventReplay
    {
        SessionInfo session;
        std::vector<NodeStampedForward> forwards;

        // 返回 true = 按 LeaveGame 之前的旧链丢弃。
        bool DeliverBind(const Guid playerId, const uint64_t loginGeneration, const uint32_t enterGsType)
        {
            if (gate_scene_route::IsBindFromBeforeLeaveGame(session, playerId, loginGeneration))
            {
                gate_scene_route::NoteDroppedFromBeforeLeaveGame(session, playerId, loginGeneration);
                return true;
            }
            if (gate_scene_route::ApplyLoginBinding(session, playerId, 3, loginGeneration, enterGsType))
            {
                if (!session.pendingSceneEntry.has_value())
                {
                    auto entry = gate_scene_route::EntryForLateLoginBinding(session, kT0);
                    if (!entry.has_value())
                    {
                        return false;
                    }
                    session.pendingSceneEntry = *entry;
                }
                Attempt();
            }
            return false;
        }

        bool DeliverRoute(const Guid playerId, const uint64_t loginGeneration, const uint64_t ownerEpoch,
                          const uint32_t nodeId, const uint64_t sceneId)
        {
            if (gate_scene_route::IsRouteFromBeforeLeaveGame(session, playerId, loginGeneration))
            {
                gate_scene_route::NoteDroppedFromBeforeLeaveGame(session, playerId, loginGeneration);
                return true;
            }
            gate_scene_route::ApplyRouteIdentity(session, playerId, 1, ownerEpoch, loginGeneration);
            const SceneRouteTarget target{playerId, nodeId, sceneId};
            session.lastSceneRoute = target;
            session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(target, kT0);
            Attempt();
            return false;
        }

        void Attempt()
        {
            auto resolve = [](const uint32_t nodeId)
            {
                return std::optional<entt::entity>{nodeId == kNodeX ? kNodeXEntity : kNodeYEntity};
            };
            auto forward = [this](const uint32_t enterType, const entt::entity node, const uint64_t)
            {
                forwards.emplace_back(enterType, node, session.ownerEpoch);
                return SceneForwardResult::kSent;
            };
            gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward);
        }
    };

    constexpr uint64_t kLeftChainGeneration = kReloginGeneration - 1; // gE:LeaveGame 时 Bind 还在 Kafka 里的那次登录
    constexpr uint64_t kNextLoginGeneration = kReloginGeneration;     // gF:同一连接上的下一次登录
} // namespace

// 审计原文的步骤:① EnterGame 已受理、Bind(gE) 还在 Kafka 里,客户端发 LeaveGame,gate 收包即重置,记下的是旧代次。② Bind(gE)
// 到达、随后路由(gE → X、epoch E)到达。旧实现只按代次认旧链:gE 大于重置时的值,两条都被当成新登录收下,登录类型挂回会话、
// 旧路由被转发到 X;③ session_ended 释放把 X 上的实体退掉、location 删掉;④ 同一连接再 EnterGame(gF),Bind 先到就按这条旧路由
// 带 E 补发到 X,而 scene_manager 铸了 E+1 落到 Y。修复后:客户端再次 EnterGame 之前到达的 Bind / 路由一律是旧链,丢弃。
TEST(SceneRouteEntry, OldChainBindArrivingAfterLeaveGameIsDroppedAndNeverReplayed)
{
    GateEventReplay gate; // 本连接第一次登录:Bind 还没到,gate 还不知道角色,代次为 0
    gate_scene_route::NoteEnterGameRequested(gate.session); // ① EnterGame(gE) 收包
    gate_scene_route::ResetSceneRoute(gate.session);        // ① LeaveGame 收包
    EXPECT_EQ(0u, gate.session.loginGenerationAtReset) << "重置时还没见过 gE:单靠代次拦不住旧链";

    // ② 旧链的 Bind 与路由:客户端还没再次 EnterGame,一律丢弃,会话一个字段都不写。
    EXPECT_TRUE(gate.DeliverBind(42, kLeftChainGeneration, kReloginEnterType));
    EXPECT_TRUE(gate.DeliverRoute(42, kLeftChainGeneration, kEpochBeforeRelogin, kNodeX, 1001));
    EXPECT_TRUE(gate.forwards.empty()) << "旧实现在这里把旧路由连同登录类型转发到 X";
    EXPECT_EQ(0u, gate.session.lastSceneRoute.sceneId);
    EXPECT_EQ(0u, gate.session.pendingEnterGsType);
    EXPECT_EQ(0u, gate.session.ownerEpoch);
    EXPECT_EQ(kInvalidGuid, gate.session.playerId) << "丢弃的 Bind 不绑角色";
    EXPECT_TRUE(gate.session.sceneRouteResetPending);
    EXPECT_EQ(42u, gate.session.playerIdAtReset) << "被离开的那条链的角色取自第一条被丢弃的事件";
    EXPECT_EQ(kLeftChainGeneration, gate.session.loginGenerationAtReset);

    // ④ 同一连接再次 EnterGame(gF):Bind 先到,没有旧路由可补发;新路由(Y、E+1)到达后带登录类型转发一次。
    gate_scene_route::NoteEnterGameRequested(gate.session);
    EXPECT_FALSE(gate.DeliverBind(42, kNextLoginGeneration, kReloginEnterType));
    EXPECT_TRUE(gate.forwards.empty()) << "不得按旧路由带旧 epoch 补发到 X";
    EXPECT_FALSE(gate.DeliverRoute(42, kNextLoginGeneration, kEpochAfterRelogin, kNodeY, 2002));
    EXPECT_EQ((std::vector<NodeStampedForward>{{kReloginEnterType, kNodeYEntity, kEpochAfterRelogin}}), gate.forwards);
    EXPECT_FALSE(gate.session.sceneRouteResetPending);
}

// 同一条旧链的 Bind 与路由是两条独立事件,路由要等 scene_manager 处理完才发、常常更晚:Bind 在客户端再次 EnterGame 之前被丢、
// 路由在之后才到时,丢 Bind 时记下的代次仍把它认出来(两种到达顺序:新登录的 Bind 之前 / 之后)。
TEST(SceneRouteEntry, OldChainRouteArrivingAfterNextEnterGameIsStillDropped)
{
    for (const bool newBindFirst : {false, true})
    {
        SCOPED_TRACE(newBindFirst ? "新登录的 Bind 先于旧链路由" : "旧链路由先于新登录的 Bind");
        GateEventReplay gate;
        gate.session = SessionInGameAtEpoch(); // 这条连接此前已在游戏里(代次 gE-1)
        gate.session.loginGeneration = kLeftChainGeneration - 1;
        gate_scene_route::NoteEnterGameRequested(gate.session); // 同连接重登 EnterGame(gE)
        gate_scene_route::ResetSceneRoute(gate.session);        // LeaveGame:Bind(gE) 还在路上
        ASSERT_TRUE(gate.DeliverBind(42, kLeftChainGeneration, kReloginEnterType));
        EXPECT_EQ(kLeftChainGeneration, gate.session.loginGenerationAtReset) << "丢弃的旧链 Bind 把代次抬上去";

        gate_scene_route::NoteEnterGameRequested(gate.session); // EnterGame(gF)
        if (newBindFirst)
        {
            EXPECT_FALSE(gate.DeliverBind(42, kNextLoginGeneration, kReloginEnterType));
        }
        EXPECT_TRUE(gate.DeliverRoute(42, kLeftChainGeneration, kEpochBeforeRelogin, kNodeX, 1001))
            << "代次不大于被离开的那条链:旧链";
        if (!newBindFirst)
        {
            EXPECT_FALSE(gate.DeliverBind(42, kNextLoginGeneration, kReloginEnterType));
        }
        EXPECT_TRUE(gate.forwards.empty());
        EXPECT_FALSE(gate.DeliverRoute(42, kNextLoginGeneration, kEpochAfterRelogin, kNodeY, 2002));
        EXPECT_EQ((std::vector<NodeStampedForward>{{kReloginEnterType, kNodeYEntity, kEpochAfterRelogin}}), gate.forwards);
    }
}

// 新登录的 Bind 已到(会话代次 = gF)之后,代次更小的同角色路由属于更早的一次登录,即使丢 Bind 时没能记下它的代次也认得出。
TEST(SceneRouteEntry, RouteOlderThanNewLoginBindIsDroppedWhileResetPending)
{
    GateEventReplay gate;
    gate.session = SessionInGameAtEpoch();
    gate_scene_route::ResetSceneRoute(gate.session); // 记下会话当时的代次 kReloginGeneration - 1
    gate_scene_route::NoteEnterGameRequested(gate.session);
    ASSERT_FALSE(gate.DeliverBind(42, kNextLoginGeneration + 10, kReloginEnterType));
    EXPECT_TRUE(gate.DeliverRoute(42, kNextLoginGeneration, kEpochBeforeRelogin, kNodeX, 1001))
        << "比会话已见过的本角色登录旧:旧链";
    EXPECT_FALSE(gate.DeliverRoute(43, kNextLoginGeneration, kEpochBeforeRelogin, kNodeX, 1001))
        << "别的角色的代次不可比:不在这里判(由玩家围栏挡住)";
}

// 第一次登录被离开(重置时 gate 还不知道角色),下一次换了角色、而且新角色的代次更小:不能拿旧角色的代次去拦新角色。
TEST(SceneRouteEntry, LeftChainGenerationDoesNotBlockAnotherCharacter)
{
    GateEventReplay gate;
    gate_scene_route::NoteEnterGameRequested(gate.session);
    gate_scene_route::ResetSceneRoute(gate.session);
    ASSERT_TRUE(gate.DeliverBind(42, kLeftChainGeneration, kReloginEnterType)); // 角色 42 的旧链

    gate_scene_route::NoteEnterGameRequested(gate.session);
    constexpr uint64_t kOtherCharacterGeneration = kLeftChainGeneration - 500;
    EXPECT_FALSE(gate.DeliverBind(77, kOtherCharacterGeneration, kReloginEnterType));
    EXPECT_TRUE(gate.DeliverRoute(42, kLeftChainGeneration, kEpochBeforeRelogin, kNodeX, 1001))
        << "角色 42 旧链迟到的路由照样丢";
    EXPECT_FALSE(gate.DeliverRoute(77, kOtherCharacterGeneration, kEpochAfterRelogin, kNodeY, 2002));
    EXPECT_EQ((std::vector<NodeStampedForward>{{kReloginEnterType, kNodeYEntity, kEpochAfterRelogin}}), gate.forwards);
}

// 选角界面重复点 LeaveGame:第二次重置不能把丢弃旧链事件时抬上去的代次冲回会话上更低的值。
TEST(SceneRouteEntry, RepeatedLeaveGameKeepsRaisedLeftChainGeneration)
{
    GateEventReplay gate;
    gate.session = SessionInGameAtEpoch();
    gate_scene_route::NoteEnterGameRequested(gate.session);
    gate_scene_route::ResetSceneRoute(gate.session);
    ASSERT_TRUE(gate.DeliverBind(42, kNextLoginGeneration, kReloginEnterType));
    ASSERT_EQ(kNextLoginGeneration, gate.session.loginGenerationAtReset);

    gate_scene_route::ResetSceneRoute(gate.session); // 再点一次 LeaveGame
    EXPECT_EQ(kNextLoginGeneration, gate.session.loginGenerationAtReset);
    EXPECT_EQ(42u, gate.session.playerIdAtReset);

    gate_scene_route::NoteEnterGameRequested(gate.session);
    EXPECT_TRUE(gate.DeliverRoute(42, kNextLoginGeneration, kEpochBeforeRelogin, kNodeX, 1001));
    EXPECT_TRUE(gate.forwards.empty());
}

// ===========================================================================
// 登录代次盖章(docs/design/scene-enter-session-fence.md §4):gate 只抬高、不丢弃。
//
// 会话上记的是"本会话、本角色见过的最高代次",ForwardPlayerToScene 转发时从会话取值盖章。
// 被测的是 scene_route_helper.h 的纯函数;"Bind 先到、按旧路由补发"用 EntryForLateLoginBinding +
// AttemptPendingSceneEntry 复现 OnLoginTypeBound 的调用序,转发器在转发那一刻读会话上的代次,
// 与生产转发器从会话取值同形。判新旧是 scene 栅栏的事,不在本组范围(cross_zone_test 的 EnterFence*)。
// ===========================================================================

namespace
{
    using gate_scene_route::PlayerChange;

    // 两条登录链铸出的代次(毫秒下限量级):上一次登录 < 本次登录。
    constexpr uint64_t kPreviousLoginGeneration = 1790000000000ull;
    constexpr uint64_t kNewLoginGeneration = 1790000005000ull;
    constexpr uint32_t kLoginReplace = 2; // LOGIN_REPLACE(player_login_comp.proto 的 EnterGsType)

    // (enterType, 转发那一刻会话上的代次)
    using StampedForward = std::pair<uint32_t, uint64_t>;
} // namespace

// 同一角色只抬高:迟到的低代次、旧版 login 与 scene 自发路由带的 0,都不能把会话拉低。
TEST(LoginGeneration, MergeWithinSamePlayerOnlyRaises)
{
    EXPECT_EQ(kNewLoginGeneration, gate_scene_route::MergeLoginGeneration(kPreviousLoginGeneration, kNewLoginGeneration,
                                                                          PlayerChange::kSamePlayer));
    EXPECT_EQ(kNewLoginGeneration, gate_scene_route::MergeLoginGeneration(kNewLoginGeneration, kPreviousLoginGeneration,
                                                                          PlayerChange::kSamePlayer));
    EXPECT_EQ(kNewLoginGeneration,
              gate_scene_route::MergeLoginGeneration(kNewLoginGeneration, 0, PlayerChange::kSamePlayer));
}

// 换了角色以新角色的值为准,哪怕更小:上一个角色的代次与新角色无关。
TEST(LoginGeneration, MergeAcrossPlayersTakesIncoming)
{
    EXPECT_EQ(kPreviousLoginGeneration,
              gate_scene_route::MergeLoginGeneration(kNewLoginGeneration, kPreviousLoginGeneration,
                                                     PlayerChange::kAnotherPlayer));
    EXPECT_EQ(0u, gate_scene_route::MergeLoginGeneration(kNewLoginGeneration, 0, PlayerChange::kAnotherPlayer));
}

// 同一条连接上换了角色:上一个角色更高的代次不能沿用,否则新角色的入场盖着别人的章。
TEST(LoginGeneration, BindingAnotherPlayerResetsToIncomingGeneration)
{
    SessionInfo session;
    session.playerId = kPlayerA;
    session.sessionVersion = 3;
    session.loginGeneration = kNewLoginGeneration;

    EXPECT_TRUE(gate_scene_route::ApplyLoginBinding(session, kPlayerB, 1, kPreviousLoginGeneration, kLoginReplace));
    EXPECT_EQ(kPlayerB, session.playerId);
    EXPECT_EQ(1u, session.sessionVersion);
    EXPECT_EQ(kPreviousLoginGeneration, session.loginGeneration);
    EXPECT_EQ(kLoginReplace, session.pendingEnterGsType);
}

// 同一角色只抬高;不带代次的 Bind(旧版 login 的空 payload)不把已知代次抹成 0。
// 会话版本号照旧无条件写入,enterGsType 为 0 时不动待入场的登录类型、也不要求调用方补发。
TEST(LoginGeneration, BindingSamePlayerOnlyRaises)
{
    SessionInfo session;
    session.playerId = kPlayerA;
    session.loginGeneration = kPreviousLoginGeneration;

    EXPECT_TRUE(gate_scene_route::ApplyLoginBinding(session, kPlayerA, 4, kNewLoginGeneration, kLoginReplace));
    EXPECT_EQ(kNewLoginGeneration, session.loginGeneration);
    EXPECT_EQ(4u, session.sessionVersion);

    EXPECT_FALSE(gate_scene_route::ApplyLoginBinding(session, kPlayerA, 5, 0, 0));
    EXPECT_EQ(kNewLoginGeneration, session.loginGeneration);
    EXPECT_EQ(5u, session.sessionVersion);
    EXPECT_EQ(kLoginReplace, session.pendingEnterGsType);
}

// 会话还没绑角色(kInvalidGuid)按同一角色处理:不带玩家的路由先记下的代次不被 Bind 拉低。
TEST(LoginGeneration, BindingUnboundSessionCountsAsSamePlayer)
{
    SessionInfo session;
    gate_scene_route::ApplyRouteIdentity(session, /*routePlayerId=*/0, 1, 5, kNewLoginGeneration);
    ASSERT_EQ(kInvalidGuid, session.playerId);

    EXPECT_TRUE(gate_scene_route::ApplyLoginBinding(session, kPlayerA, 1, kPreviousLoginGeneration, kLoginReplace));
    EXPECT_EQ(kPlayerA, session.playerId);
    EXPECT_EQ(kNewLoginGeneration, session.loginGeneration);
}

// 设计 §4"为什么 Bind 也要带":在进过场的连接上重登,Bind 先于本次路由到达,gate 立即按上一次的
// lastSceneRoute 补发并消费登录类型;随后本次路由同场景同节点,判重复、不再转发。所以那次补发在转发的那一刻
// 就必须已带本次登录的代次 —— ApplyLoginBinding 先于 OnLoginTypeBound 写会话。若顺序反了,下面盖的是上一代,
// 中间被别的设备顶过号时 scene 会判陈旧,合法重登进不了场。
TEST(LoginGeneration, LateBindingResendCarriesNewGenerationAtForwardTime)
{
    SessionInfo session;
    session.playerId = kPlayerA;
    session.loginGeneration = kPreviousLoginGeneration;
    session.sceneId = 1001;
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{9});
    session.lastSceneRoute = SceneRouteTarget{kPlayerA, 7, 1001};

    ASSERT_TRUE(gate_scene_route::ApplyLoginBinding(session, kPlayerA, 2, kNewLoginGeneration, kLoginReplace));

    // OnLoginTypeBound 的纯逻辑:没有欠账时按 lastSceneRoute 建一笔并立即尝试。
    auto entry = gate_scene_route::EntryForLateLoginBinding(session, kT0);
    ASSERT_TRUE(entry.has_value());
    session.pendingSceneEntry = *entry;

    std::vector<StampedForward> stamped;
    auto resolve = [](uint32_t) { return std::optional<entt::entity>{entt::entity{9}}; };
    auto forward = [&](const uint32_t enterType, entt::entity, uint64_t)
    {
        stamped.emplace_back(enterType, session.loginGeneration);
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ((std::vector<StampedForward>{StampedForward{kLoginReplace, kNewLoginGeneration}}), stamped);

    // 本次登录的路由随后才到:同场景同节点判重复、不再转发;代次取 max,不回退。
    gate_scene_route::ApplyRouteIdentity(session, kPlayerA, 1, 5, kNewLoginGeneration);
    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(SceneRouteTarget{kPlayerA, 7, 1001}, kT0);
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ(1u, stamped.size());
    EXPECT_EQ(kNewLoginGeneration, session.loginGeneration);
}

// 路由属于别的角色(同连接已换角色,这条路由随后被玩家围栏撤销):代次与玩家都不动;
// 归属 zone 与 epoch 仍按原语义无条件覆盖。
TEST(LoginGeneration, ForeignRouteLeavesGenerationAlone)
{
    SessionInfo session;
    session.playerId = kPlayerB;
    session.loginGeneration = kPreviousLoginGeneration;
    session.homeZoneId = 1;
    session.ownerEpoch = 5;

    gate_scene_route::ApplyRouteIdentity(session, kPlayerA, 2, 9, kNewLoginGeneration);
    EXPECT_EQ(kPlayerB, session.playerId);
    EXPECT_EQ(kPreviousLoginGeneration, session.loginGeneration);
    EXPECT_EQ(2u, session.homeZoneId);
    EXPECT_EQ(9u, session.ownerEpoch);
}

// 本角色的路由:代次取 max;homeZoneId / ownerEpoch 是"本次路由决策",照旧整体覆盖(可以变小)。
// 不带玩家(0)的路由按本角色处理。
TEST(LoginGeneration, OwnRouteRaisesGenerationAndStillOverwritesZoneAndEpoch)
{
    SessionInfo session;
    session.playerId = kPlayerA;
    session.loginGeneration = kPreviousLoginGeneration;

    gate_scene_route::ApplyRouteIdentity(session, kPlayerA, 2, 9, kNewLoginGeneration);
    EXPECT_EQ(kNewLoginGeneration, session.loginGeneration);
    EXPECT_EQ(2u, session.homeZoneId);
    EXPECT_EQ(9u, session.ownerEpoch);

    gate_scene_route::ApplyRouteIdentity(session, kPlayerA, 3, 4, kPreviousLoginGeneration);
    EXPECT_EQ(kNewLoginGeneration, session.loginGeneration);
    EXPECT_EQ(3u, session.homeZoneId);
    EXPECT_EQ(4u, session.ownerEpoch);

    gate_scene_route::ApplyRouteIdentity(session, 0, 3, 4, kNewLoginGeneration + 1);
    EXPECT_EQ(kPlayerA, session.playerId);
    EXPECT_EQ(kNewLoginGeneration + 1, session.loginGeneration);
}

// 会话还没绑角色时采用路由的玩家与代次(BindSession 可能尚未到达);已绑角色后不改写玩家(原语义)。
TEST(LoginGeneration, RouteOnUnboundSessionAdoptsPlayerAndGeneration)
{
    SessionInfo session;
    gate_scene_route::ApplyRouteIdentity(session, kPlayerA, 2, 9, kNewLoginGeneration);
    EXPECT_EQ(kPlayerA, session.playerId);
    EXPECT_EQ(kNewLoginGeneration, session.loginGeneration);

    gate_scene_route::ApplyRouteIdentity(session, kPlayerB, 2, 9, kNewLoginGeneration + 1);
    EXPECT_EQ(kPlayerA, session.playerId);
    EXPECT_EQ(kNewLoginGeneration, session.loginGeneration);
}

// 只抬高、不丢弃(设计 §4"为什么 gate 不据此丢路由"):一次失败的重登已把会话代次抬高,之后同连接上
// scene 自发的换图路由带 0,仍照常转发,盖的是会话上的较高值。
TEST(LoginGeneration, LowerGenerationRouteIsStillForwarded)
{
    SessionInfo session;
    session.playerId = kPlayerA;
    session.loginGeneration = kNewLoginGeneration;
    session.sceneId = 1001;
    session.SetNodeEntity(eNodeType::SceneNodeService, entt::entity{9});

    const SceneRouteTarget travel{kPlayerA, 7, 2001};
    gate_scene_route::ApplyRouteIdentity(session, travel.playerId, 1, 5, /*loginGeneration=*/0);
    EXPECT_EQ(kNewLoginGeneration, session.loginGeneration);

    session.pendingSceneEntry = gate_scene_route::BeginPendingSceneEntry(travel, kT0);
    std::vector<StampedForward> stamped;
    auto resolve = [](uint32_t) { return std::optional<entt::entity>{entt::entity{9}}; };
    auto forward = [&](const uint32_t enterType, entt::entity, uint64_t)
    {
        stamped.emplace_back(enterType, session.loginGeneration);
        return SceneForwardResult::kSent;
    };
    EXPECT_EQ(SceneEntryAttempt::kApplied,
              gate_scene_route::AttemptPendingSceneEntry(session, kT0, resolve, forward));
    EXPECT_EQ((std::vector<StampedForward>{StampedForward{0u, kNewLoginGeneration}}), stamped);
    EXPECT_EQ(2001u, session.sceneId);
}

// ===========================================================================
// 踢线身份栅栏 ClassifyKick(docs/design/scene-enter-chain-residuals.md §2.2 ④,余项 a)
// ===========================================================================
//
// login 在 ReplaceLogin 提交之后经 KickPlayerEvent{session_id, player_id, login_generation} 踢旧会话。
// 先过 ClassifyPush(玩家维度,未绑定 fail-closed),再比代次:会话已见过不小于踢线者的代次时不踢。

namespace
{
    constexpr uint64_t kKickerGeneration = 1'760'000'000'200ull;
} // namespace

TEST(KickFence, MatchingPlayerKicks)
{
    EXPECT_EQ(gate_session_fence::KickVerdict::kKick,
              gate_session_fence::ClassifyKick(kPlayerA, kKickerGeneration - 1, kPlayerA, kKickerGeneration));
}

// T3:会话号回绕 / node_id 复用后同号会话已属于别的玩家,旧 gate 只按 session_id 会踢到它。
TEST(KickFence, OtherPlayerSkips)
{
    EXPECT_EQ(gate_session_fence::KickVerdict::kSkipOtherPlayer,
              gate_session_fence::ClassifyKick(kPlayerB, 0, kPlayerA, kKickerGeneration));
}

// 未绑定玩家的会话 fail-closed,与 ClassifyPush 的 R13 口径一致(kInvalidGuid 与 0 都算未绑定)。
TEST(KickFence, UnboundSessionSkips)
{
    EXPECT_EQ(gate_session_fence::KickVerdict::kSkipOtherPlayer,
              gate_session_fence::ClassifyKick(kUnbound, 0, kPlayerA, kKickerGeneration));
    EXPECT_EQ(gate_session_fence::KickVerdict::kSkipOtherPlayer,
              gate_session_fence::ClassifyKick(0, 0, kPlayerA, kKickerGeneration));
}

// 旧版 login 的踢线不带 player_id:按 session_id 踢(旧行为),不比代次。
TEST(KickFence, LegacyKickWithoutPlayerIsUnfenced)
{
    EXPECT_EQ(gate_session_fence::KickVerdict::kKickUnfenced,
              gate_session_fence::ClassifyKick(kPlayerB, kKickerGeneration + 5, 0, 0));
    EXPECT_EQ(gate_session_fence::KickVerdict::kKickUnfenced,
              gate_session_fence::ClassifyKick(kUnbound, 0, 0, kKickerGeneration));
}

// T4:同一连接上更新的重登已经 Bind(会话代次更大),迟到的旧踢线不得踢掉它。等号只会出现在同一会话上,判不踢是安全方向。
TEST(KickFence, SessionHostingNewerOrEqualLoginSkips)
{
    EXPECT_EQ(gate_session_fence::KickVerdict::kSkipNewerLogin,
              gate_session_fence::ClassifyKick(kPlayerA, kKickerGeneration + 1, kPlayerA, kKickerGeneration));
    EXPECT_EQ(gate_session_fence::KickVerdict::kSkipNewerLogin,
              gate_session_fence::ClassifyKick(kPlayerA, kKickerGeneration, kPlayerA, kKickerGeneration));
}

TEST(KickFence, OlderSessionGenerationKicks)
{
    EXPECT_EQ(gate_session_fence::KickVerdict::kKick,
              gate_session_fence::ClassifyKick(kPlayerA, 1, kPlayerA, kKickerGeneration));
}

// 踢线者代次为 0(只带 player_id 的版本):不比代次,玩家相符即踢。
TEST(KickFence, ZeroKickerGenerationSkipsComparison)
{
    EXPECT_EQ(gate_session_fence::KickVerdict::kKick,
              gate_session_fence::ClassifyKick(kPlayerA, kKickerGeneration, kPlayerA, 0));
}

// 会话没见过代次(旧版 login 的 Bind):0 ≥ gen 不成立,照踢。
TEST(KickFence, LegacySessionGenerationZeroKicks)
{
    EXPECT_EQ(gate_session_fence::KickVerdict::kKick,
              gate_session_fence::ClassifyKick(kPlayerA, 0, kPlayerA, kKickerGeneration));
}

// 不变量 4:loginGeneration 只经 ApplyLoginBinding / ApplyRouteIdentity 写入。ClassifyKick 按值读会话字段,
// 判完之后会话的代次与玩家都不变,也不会被「抬高到踢线者的代次」。
TEST(KickFence, ClassifyDoesNotTouchSessionGeneration)
{
    SessionInfo session;
    session.playerId = kPlayerA;
    session.loginGeneration = kKickerGeneration - 1;

    const auto kicked = gate_session_fence::ClassifyKick(session.playerId, session.loginGeneration,
                                                         kPlayerA, kKickerGeneration);
    EXPECT_EQ(gate_session_fence::KickVerdict::kKick, kicked);
    EXPECT_EQ(kKickerGeneration - 1, session.loginGeneration);
    EXPECT_EQ(kPlayerA, session.playerId);

    session.loginGeneration = kKickerGeneration + 1;
    const auto skipped = gate_session_fence::ClassifyKick(session.playerId, session.loginGeneration,
                                                          kPlayerA, kKickerGeneration);
    EXPECT_EQ(gate_session_fence::KickVerdict::kSkipNewerLogin, skipped);
    EXPECT_EQ(kKickerGeneration + 1, session.loginGeneration);
}

// 日志名互不相同,也不落进兜底的 "unknown"。
TEST(KickFence, VerdictNamesAreDistinct)
{
    const std::set<std::string> names{
        gate_session_fence::KickVerdictName(gate_session_fence::KickVerdict::kKick),
        gate_session_fence::KickVerdictName(gate_session_fence::KickVerdict::kKickUnfenced),
        gate_session_fence::KickVerdictName(gate_session_fence::KickVerdict::kSkipOtherPlayer),
        gate_session_fence::KickVerdictName(gate_session_fence::KickVerdict::kSkipNewerLogin),
    };
    EXPECT_EQ(4u, names.size());
    EXPECT_EQ(0u, names.count("unknown"));
}

// ===========================================================================
// scene 推来的踢线(34)来源栅栏 ClassifySceneKickSource(cross-zone-scene-travel.md §13.2「上限到期一律踢线」残余)
// ===========================================================================
//
// 同 zone 交接其实已放行、会话已改绑到目标节点时,源 scene 收口推来的 34 不得断掉这条合法会话:
// 34 只认会话当前绑定的那个 scene 进程。任一侧认不出时照发 —— 34 是让客户端脱困的出口。

namespace
{
    constexpr std::string_view kSourceSceneInstance = "11111111-1111-4111-8111-111111111111";
    constexpr std::string_view kTargetSceneInstance = "22222222-2222-4222-8222-222222222222";
} // namespace

TEST(SceneKickSourceFence, KickFromBoundSceneIsDelivered)
{
    EXPECT_EQ(gate_session_fence::SceneKickSourceVerdict::kDeliver,
              gate_session_fence::ClassifySceneKickSource(kSourceSceneInstance, kSourceSceneInstance));
}

// 会话已被路由改绑到目标进程,迟到的源进程 34 丢弃。
TEST(SceneKickSourceFence, KickFromSceneTheSessionLeftIsDropped)
{
    EXPECT_EQ(gate_session_fence::SceneKickSourceVerdict::kDropNotBoundScene,
              gate_session_fence::ClassifySceneKickSource(kTargetSceneInstance, kSourceSceneInstance));
}

// 会话还没绑 scene / 绑定的实体已失效(空)、或认不出发送方(空):照发,与旧行为相同。
TEST(SceneKickSourceFence, UnverifiableSourceIsStillDelivered)
{
    EXPECT_EQ(gate_session_fence::SceneKickSourceVerdict::kDeliverUnverified,
              gate_session_fence::ClassifySceneKickSource("", kSourceSceneInstance));
    EXPECT_EQ(gate_session_fence::SceneKickSourceVerdict::kDeliverUnverified,
              gate_session_fence::ClassifySceneKickSource(kTargetSceneInstance, ""));
    EXPECT_EQ(gate_session_fence::SceneKickSourceVerdict::kDeliverUnverified,
              gate_session_fence::ClassifySceneKickSource("", ""));
}

// ===========================================================================
// 未登录闸(docs/design/data-overwrite-rollback-audit-20261018.md §2.3、client-request-ordering.md §7.2 第 2 条)。
//
// gate 被 kill -9 后以同一 node_id 重启、session 序号从 0 开始,新连接撞上旧进程同号连接在 login 留下的登录态;
// 跳过 Login 直接发 CreatePlayer / EnterGame 就能用别人的账号。login 已改按 (gate_instance_id, 会话代次) 认登录态,
// gate 这一层要求三种方法发出前本连接收到过成功的 Login 回包。被测的是 handler/rpc/login_admission.h 的纯判定;
// 发出时机(路由 lane 在 Pump、直连在收包)与置位点(lane 认领 / 直连回包分支)由 robot 冒烟覆盖。
// ===========================================================================

namespace
{
	MessageContent RoutedLoginReply(const loginpb::LoginResponse &body)
	{
		MessageContent content;
		content.set_id(7);
		content.set_message_id(ClientPlayerLoginLoginMessageId);
		content.set_serialized_message(body.SerializeAsString());
		return content;
	}

	loginpb::LoginResponse LoginFailedWith(uint32_t tipId)
	{
		loginpb::LoginResponse response;
		response.mutable_error_message()->set_id(tipId);
		return response;
	}

	loginpb::LoginResponse LoginSucceededWithOneRole()
	{
		loginpb::LoginResponse response;
		response.add_players()->mutable_player()->set_player_id(kPlayerA);
		return response;
	}
} // namespace

// 撞号的新连接与任何新连接一样从「未登录」开始:Login 回包之前,建角 / 进游戏 / 离开游戏都不放行。
TEST(LoginAdmission, FreshSessionAfterGateRestartIsNotLoggedIn)
{
	SessionInfo session;
	EXPECT_FALSE(session.loginSucceeded);
	for (const uint32_t messageId : {ClientPlayerLoginCreatePlayerMessageId,
									 ClientPlayerLoginEnterGameMessageId,
									 ClientPlayerLoginLeaveGameMessageId})
	{
		EXPECT_TRUE(gate_login_admission::RequiresSuccessfulLogin(messageId)) << "message_id=" << messageId;
	}
}

// Login 本身、凭 refresh token 自证的 RefreshToken、gate 合成的 Disconnect、HTTP 侧的 AssignGate 都不要求先登录。
TEST(LoginAdmission, LoginItselfAndSelfAuthenticatedMethodsAreNotGated)
{
	EXPECT_FALSE(gate_login_admission::RequiresSuccessfulLogin(ClientPlayerLoginLoginMessageId));
	EXPECT_FALSE(gate_login_admission::RequiresSuccessfulLogin(ClientPlayerLoginRefreshTokenMessageId));
	EXPECT_FALSE(gate_login_admission::RequiresSuccessfulLogin(ClientPlayerLoginDisconnectMessageId));
	EXPECT_FALSE(gate_login_admission::RequiresSuccessfulLogin(LoginPreGateAssignGateMessageId));
}

// 路由模式正常登录:信封无错、body 无错即成功;body 为空(新账号没有角色、access_token 重登不发新 token)同样成功。
TEST(LoginAdmission, RoutedSuccessfulLoginUnlocksSession)
{
	EXPECT_TRUE(gate_login_admission::IsLoginReplySuccess(RoutedLoginReply(LoginSucceededWithOneRole())));
	EXPECT_TRUE(gate_login_admission::IsLoginReplySuccess(RoutedLoginReply(loginpb::LoginResponse{})));
	EXPECT_TRUE(gate_login_admission::IsLoginReplySuccess(RoutedLoginReply(LoginFailedWith(0))))
		<< "error_message 填了 0 也按成功";
}

// 路由模式失败的 Login 不放行:body 里的业务拒绝(含 login 写了登录态之后才判出的设备数超限)、路由服信封错误。
TEST(LoginAdmission, RoutedFailedLoginKeepsSessionLocked)
{
	EXPECT_FALSE(gate_login_admission::IsLoginReplySuccess(RoutedLoginReply(LoginFailedWith(kLoginAccountNotFound))));
	EXPECT_FALSE(gate_login_admission::IsLoginReplySuccess(RoutedLoginReply(LoginFailedWith(kTooManyDevices))));

	// 信封错误时 body 再像成功也不算:那是路由服 / 下游的拒绝,login 没有写本连接的登录态。
	MessageContent envelope = RoutedLoginReply(LoginSucceededWithOneRole());
	envelope.mutable_error_message()->set_id(kServiceUnavailable);
	EXPECT_FALSE(gate_login_admission::IsLoginReplySuccess(envelope));
}

// 看不懂的 body 按失败(fail-closed):宁可让客户端重登,也不凭坏包放行。
TEST(LoginAdmission, UnparsableRoutedLoginBodyKeepsSessionLocked)
{
	MessageContent content;
	content.set_message_id(ClientPlayerLoginLoginMessageId);
	content.set_serialized_message(std::string("\x0a\x05\x01", 3)); // 字段 1 声明 5 字节、只给 1 字节
	EXPECT_FALSE(gate_login_admission::IsLoginReplySuccess(content));
}

// 直连模式回的是 LoginResponse 本身;别的回包类型(哪怕本身「成功」)都不能把会话判成已登录。
TEST(LoginAdmission, DirectLoginResponseAndForeignReplyTypes)
{
	EXPECT_TRUE(gate_login_admission::IsLoginReplySuccess(LoginSucceededWithOneRole()));
	EXPECT_FALSE(gate_login_admission::IsLoginReplySuccess(LoginFailedWith(kLoginSessionNotFound)));

	loginpb::EnterGameResponse enterOk;
	enterOk.mutable_error_message()->set_id(0);
	EXPECT_FALSE(gate_login_admission::IsLoginReplySuccess(enterOk));
}

int main(int argc, char **argv)
{
	testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}

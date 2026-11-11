// 每类节点的发现集合、选号读全服分配清单、发现快照的读守卫(docs/design/connection-fanout-subsetting-20261106.md §3.4)。
//
// 只测纯函数与纯数据结构:NodeUtils::PlanDiscoveryScopes、node_id_selection、ServiceNodeSnapshot、EtcdHelper::TxnConflictListing。
// 没覆盖的一层:Node 构造时的集合校验与 DependencyGate 的拒启(LOG_FATAL)、EtcdService 按精确键 Range / watch、
// 首次 StartRpcServer 开始看自己的键、分配键 CAS 失败时把清单交给分配器。它们要真 etcd 或整个 Node,
// 靠冒烟核对(设计稿 §7):battle 的日志里只有 Match 前缀与自己那条键的 watch,多 zone 下新节点不再逐个撞号。

#include <cstdint>
#include <optional>
#include <set>
#include <string>
#include <unordered_set>
#include <vector>

#include <gtest/gtest.h>

#include "node/system/discovery/service_node_snapshot.h"
#include "node/system/etcd/etcd_helper.h"
#include "node/system/node/node_id_selection.h"
#include "node/system/node/node_util.h"
#include "proto/common/base/node.pb.h"

namespace
{
	// 本机 bin/etc/base_deploy_config.yaml 的 13 个前缀,顺序同配置。
	const std::vector<std::string> kLocalConfigured{
		"SceneNodeService.rpc",       "GateNodeService.rpc",        "LoginNodeService.rpc",
		"FriendNodeService.rpc",      "GuildNodeService.rpc",       "SceneManagerNodeService.rpc",
		"BattleNodeService.rpc",      "ClientRpcRouterNodeService.rpc", "MatchNodeService.rpc",
		"DataServiceNodeService.rpc", "PlayerInboxNodeService.rpc", "PlayerStoreNodeService.rpc",
		"TeamNodeService.rpc",
	};

	// tools/scripts/k8s_deploy.ps1 的 node-config 模板的 11 个前缀(没有 Friend、Guild),顺序同模板。
	const std::vector<std::string> kK8sConfigured{
		"SceneNodeService.rpc",       "GateNodeService.rpc",        "LoginNodeService.rpc",
		"SceneManagerNodeService.rpc", "BattleNodeService.rpc",     "ClientRpcRouterNodeService.rpc",
		"MatchNodeService.rpc",       "DataServiceNodeService.rpc", "PlayerInboxNodeService.rpc",
		"PlayerStoreNodeService.rpc", "TeamNodeService.rpc",
	};

	std::string InZone(const std::string &prefix, uint32_t zoneId)
	{
		return prefix + "/zone/" + std::to_string(zoneId) + "/";
	}

	// 与三类节点 main.cpp 里写的发现集合一致(改那边要同步改这里)。
	const std::set<uint32_t> kSceneDiscovery{SceneManagerNodeService, DataServiceNodeService, PlayerInboxNodeService,
											 PlayerStoreNodeService,  TeamNodeService,        MatchNodeService,
											 GateNodeService,         SceneNodeService};
	const std::set<uint32_t> kGateRouterModeDiscovery{SceneNodeService, ClientRpcRouterNodeService, GateNodeService};
	const std::set<uint32_t> kGateDirectModeDiscovery{SceneNodeService, LoginNodeService, SceneManagerNodeService,
													  MatchNodeService, GateNodeService};
	const std::set<uint32_t> kBattleDiscovery{MatchNodeService};

	std::vector<uint32_t> gUndeclaredReads;
	void RecordUndeclaredRead(uint32_t nodeType)
	{
		gUndeclaredReads.push_back(nodeType);
	}
} // namespace

// ---- 每类节点实际看的前缀 ----

TEST(DiscoverySetTest, SceneWatchesOnlyTheTypesItReads)
{
	const auto plan = NodeUtils::PlanDiscoveryScopes(kLocalConfigured, kSceneDiscovery, 7);
	const std::vector<std::string> expected{
		InZone("SceneNodeService.rpc", 7),        InZone("GateNodeService.rpc", 7),
		InZone("SceneManagerNodeService.rpc", 7), "MatchNodeService.rpc",
		"DataServiceNodeService.rpc",             "PlayerInboxNodeService.rpc",
		InZone("PlayerStoreNodeService.rpc", 7),  "TeamNodeService.rpc",
	};
	EXPECT_EQ(plan.scopes, expected);
	// 加 battle、路由服、login 实例不再增加 scene 的 Range 与 watch 事件。
	EXPECT_EQ(plan.skippedPrefixes, (std::vector<std::string>{"LoginNodeService.rpc", "FriendNodeService.rpc",
															  "GuildNodeService.rpc", "BattleNodeService.rpc",
															  "ClientRpcRouterNodeService.rpc"}));
	EXPECT_TRUE(plan.missingTypes.empty());
}

TEST(DiscoverySetTest, RouterModeGateWatchesSceneRoutersAndItsOwnZoneGates)
{
	const auto plan = NodeUtils::PlanDiscoveryScopes(kLocalConfigured, kGateRouterModeDiscovery, 3);
	EXPECT_EQ(plan.scopes, (std::vector<std::string>{InZone("SceneNodeService.rpc", 3), InZone("GateNodeService.rpc", 3),
													 "ClientRpcRouterNodeService.rpc"}));
	EXPECT_EQ(plan.skippedPrefixes.size(), kLocalConfigured.size() - 3);
	EXPECT_TRUE(plan.missingTypes.empty());
}

TEST(DiscoverySetTest, DirectModeGateKeepsItsDirectTargets)
{
	const auto plan = NodeUtils::PlanDiscoveryScopes(kLocalConfigured, kGateDirectModeDiscovery, 3);
	EXPECT_EQ(plan.scopes, (std::vector<std::string>{InZone("SceneNodeService.rpc", 3), InZone("GateNodeService.rpc", 3),
													 InZone("LoginNodeService.rpc", 3),
													 InZone("SceneManagerNodeService.rpc", 3), "MatchNodeService.rpc"}));
}

TEST(DiscoverySetTest, BattleNoLongerWatchesOtherBattles)
{
	// 自身类型 Battle 不在集合里:battle 之间的 B² 项消失,本节点自己的键另由 ServiceDiscoveryManager 精确地看。
	for (const auto *configured : {&kLocalConfigured, &kK8sConfigured})
	{
		const auto plan = NodeUtils::PlanDiscoveryScopes(*configured, kBattleDiscovery, 1);
		EXPECT_EQ(plan.scopes, std::vector<std::string>{"MatchNodeService.rpc"});
		EXPECT_EQ(plan.skippedPrefixes.size(), configured->size() - 1);
	}
}

TEST(DiscoverySetTest, DeclaredButUnconfiguredTypesAreReported)
{
	// K8s 模板没有 Friend:声明了也发现不到,规划里列出来,启动日志打 WARN。
	const std::set<uint32_t> declared{MatchNodeService, FriendNodeService};
	const auto plan = NodeUtils::PlanDiscoveryScopes(kK8sConfigured, declared, 1);
	EXPECT_EQ(plan.scopes, std::vector<std::string>{"MatchNodeService.rpc"});
	EXPECT_EQ(plan.missingTypes, std::vector<uint32_t>{FriendNodeService});
}

TEST(DiscoverySetTest, DuplicateAndUnknownPrefixesAreHandled)
{
	const std::vector<std::string> configured{"MatchNodeService.rpc", "MatchNodeService.rpc", "XyzNodeService.rpc",
											  "MatchNodeService.rpc/zone/1/"};
	const auto plan = NodeUtils::PlanDiscoveryScopes(configured, kBattleDiscovery, 1);
	EXPECT_EQ(plan.scopes, std::vector<std::string>{"MatchNodeService.rpc"});
	EXPECT_EQ(plan.skippedPrefixes, (std::vector<std::string>{"XyzNodeService.rpc", "MatchNodeService.rpc/zone/1/"}));
}

TEST(DiscoverySetTest, PrefixTypeParsingIsExact)
{
	EXPECT_EQ(NodeUtils::NodeTypeOfDiscoveryPrefix("MatchNodeService.rpc"), static_cast<uint32_t>(MatchNodeService));
	EXPECT_FALSE(NodeUtils::NodeTypeOfDiscoveryPrefix("MatchNodeService").has_value());
	EXPECT_FALSE(NodeUtils::NodeTypeOfDiscoveryPrefix(".rpc").has_value());
	EXPECT_FALSE(NodeUtils::NodeTypeOfDiscoveryPrefix("XMatchNodeService.rpc").has_value());
	EXPECT_FALSE(NodeUtils::NodeTypeOfDiscoveryPrefix("").has_value());
}

// ---- 选号 ----

TEST(DiscoverySetTest, AllocatedIdsParseOnlyTheirOwnPrefix)
{
	using node_id_selection::ParseAllocatedId;
	const std::string prefix = "SceneNodeService.rpc/allocated/node_type/2/node_id/";
	EXPECT_EQ(ParseAllocatedId(prefix, prefix + "17", 131071), 17u);
	EXPECT_EQ(ParseAllocatedId(prefix, prefix + "131071", 131071), 131071u);
	for (const std::string suffix : {"", "0", "131072", "1a", "-1", "+1", " 1", "1 "})
	{
		EXPECT_FALSE(ParseAllocatedId(prefix, prefix + suffix, 131071).has_value()) << suffix;
	}
	EXPECT_FALSE(ParseAllocatedId(prefix, "GateNodeService.rpc/allocated/node_type/3/node_id/17", 131071).has_value());
}

TEST(DiscoverySetTest, SmallestFreeIdSkipsEveryKnownHolder)
{
	using node_id_selection::SmallestFreeId;
	EXPECT_EQ(SmallestFreeId({}, 10), 1u);
	EXPECT_EQ(SmallestFreeId({1, 2, 3, 5}, 10), 4u);
	EXPECT_FALSE(SmallestFreeId({1, 2, 3}, 3).has_value());

	// 多 zone:本 zone 的快照只有 1 号。第一次选号挑 2,撞上别的 zone 的节点;CAS 失败读回全服清单 {1..6, 8},
	// 第二次直接落到 7,不再一个号一个号地撞(每撞一次要等 1 秒)。
	std::unordered_set<uint32_t> used{1};
	EXPECT_EQ(SmallestFreeId(used, 131071), 2u);
	for (const uint32_t allocated : {1u, 2u, 3u, 4u, 5u, 6u, 8u})
	{
		used.insert(allocated);
	}
	EXPECT_EQ(SmallestFreeId(used, 131071), 7u);
}

TEST(DiscoverySetTest, ConflictListingComesOnlyFromAFailedListingClaim)
{
	etcdserverpb::TxnResponse failed;
	failed.set_succeeded(false);
	auto *listing = failed.add_responses()->mutable_response_range();
	listing->add_kvs()->set_key("SceneNodeService.rpc/allocated/node_type/2/node_id/1");
	listing->add_kvs()->set_key("SceneNodeService.rpc/allocated/node_type/2/node_id/2");
	const auto *found = EtcdHelper::TxnConflictListing(failed);
	ASSERT_NE(found, nullptr);
	EXPECT_EQ(found->kvs_size(), 2);

	etcdserverpb::TxnResponse won;
	won.set_succeeded(true);
	won.add_responses()->mutable_response_put();
	EXPECT_EQ(EtcdHelper::TxnConflictListing(won), nullptr);

	// 重注册用的嵌套申领(BuildClaimIfAbsentOrOwnedTxn)失败时是一个内层 txn,不是清单。
	etcdserverpb::TxnResponse nested;
	nested.set_succeeded(false);
	nested.add_responses()->mutable_response_txn();
	EXPECT_EQ(EtcdHelper::TxnConflictListing(nested), nullptr);
}

// ---- 发现快照 ----

TEST(DiscoverySetTest, SnapshotReportsUndeclaredReadsOncePerType)
{
	gUndeclaredReads.clear();
	ServiceNodeSnapshot snapshot;
	ServiceNodeSnapshot::ReadableTypes readable{};
	readable[MatchNodeService] = true;
	readable[BattleNodeService] = true; // 自身类型
	snapshot.DeclareReadableTypes(readable, &RecordUndeclaredRead);

	EXPECT_TRUE(snapshot.NodesOfType(MatchNodeService).empty());
	EXPECT_FALSE(snapshot.HasNodesOfType(BattleNodeService));
	EXPECT_TRUE(gUndeclaredReads.empty());

	EXPECT_FALSE(snapshot.HasNodesOfType(SceneNodeService));
	EXPECT_TRUE(snapshot.NodesOfType(SceneNodeService).empty());
	EXPECT_EQ(gUndeclaredReads, std::vector<uint32_t>{SceneNodeService});

	// 越界的类型照旧返回空,不报。
	EXPECT_TRUE(snapshot.NodesOfType(100000).empty());
	EXPECT_EQ(gUndeclaredReads.size(), 1u);
}

TEST(DiscoverySetTest, SnapshotGenerationCountsMembershipChanges)
{
	ServiceNodeSnapshot snapshot;
	NodeInfo router;
	router.set_node_type(ClientRpcRouterNodeService);
	router.set_node_uuid("router-1");

	EXPECT_EQ(snapshot.Generation(ClientRpcRouterNodeService), 0u);
	EXPECT_EQ(snapshot.Upsert(ClientRpcRouterNodeService, router), ServiceNodeSnapshot::UpsertResult::kAdded);
	EXPECT_EQ(snapshot.Generation(ClientRpcRouterNodeService), 1u);
	EXPECT_EQ(snapshot.Upsert(ClientRpcRouterNodeService, router), ServiceNodeSnapshot::UpsertResult::kUpdated);
	EXPECT_EQ(snapshot.Generation(ClientRpcRouterNodeService), 2u);
	EXPECT_EQ(snapshot.Remove(ClientRpcRouterNodeService, "missing"), 0);
	EXPECT_EQ(snapshot.Generation(ClientRpcRouterNodeService), 2u);
	EXPECT_EQ(snapshot.Remove(ClientRpcRouterNodeService, "router-1"), 1);
	EXPECT_EQ(snapshot.Generation(ClientRpcRouterNodeService), 3u);
	// 别的类型不受影响。
	EXPECT_EQ(snapshot.Generation(MatchNodeService), 0u);
}

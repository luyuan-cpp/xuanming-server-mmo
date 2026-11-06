// 发现前缀按 zone 收窄与兜底重拉节奏(docs/design/control-plane-zone-scoping-20261105.md §3.1)。
#include <string>

#include <gtest/gtest.h>

#include "node/system/discovery/discovery_resync_policy.h"
#include "node/system/node/node_util.h"
#include "proto/common/base/node.pb.h"

namespace
{
	std::string ConfiguredPrefix(eNodeType type)
	{
		return eNodeType_Name(type) + ".rpc";
	}

	// EtcdManager::MakeNodeEtcdPrefix 的键形(etcd_manager.cpp):<Svc>.rpc/zone/<z>/node_type/<t>/node_id/。
	std::string RegisteredKeyPrefix(eNodeType type, uint32_t zoneId)
	{
		return ConfiguredPrefix(type) + "/zone/" + std::to_string(zoneId) + "/node_type/" +
			   std::to_string(static_cast<uint32_t>(type)) + "/node_id/";
	}
} // namespace

TEST(DiscoveryZoneScopeTest, ZoneScopedTypesWatchOnlyTheirOwnZone)
{
	for (const auto type : {eNodeType::GateNodeService, eNodeType::SceneNodeService, eNodeType::LoginNodeService,
							eNodeType::PlayerLocatorNodeService, eNodeType::PlayerStoreNodeService,
							eNodeType::SceneManagerNodeService})
	{
		SCOPED_TRACE(eNodeType_Name(type));
		EXPECT_TRUE(NodeUtils::IsZoneScopedNodeType(type));
		const auto prefix = NodeUtils::DiscoveryPrefixFor(ConfiguredPrefix(type), 7);
		EXPECT_EQ(prefix, ConfiguredPrefix(type) + "/zone/7/");
		// 本 zone 注册的键都在收窄后的前缀下。
		EXPECT_EQ(RegisteredKeyPrefix(type, 7).rfind(prefix, 0), 0u);
		// 别的 zone 不在:zone 7 的前缀不能前缀匹配到 zone 70 / 17 的键。
		EXPECT_NE(RegisteredKeyPrefix(type, 70).rfind(prefix, 0), 0u);
		EXPECT_NE(RegisteredKeyPrefix(type, 17).rfind(prefix, 0), 0u);
		// 分配键不在发现前缀里(容量模型 A1「分配键移出发现前缀」)。
		const auto allocationKey = ConfiguredPrefix(type) + "/allocated/node_type/" +
								   std::to_string(static_cast<uint32_t>(type)) + "/node_id/3";
		EXPECT_NE(allocationKey.rfind(prefix, 0), 0u);
	}
}

TEST(DiscoveryZoneScopeTest, GlobalPoolTypesKeepTheWholePrefix)
{
	for (const auto type : {eNodeType::DataServiceNodeService, eNodeType::MatchNodeService,
							eNodeType::BattleNodeService, eNodeType::ClientRpcRouterNodeService,
							eNodeType::PlayerInboxNodeService, eNodeType::TeamNodeService,
							eNodeType::FriendNodeService, eNodeType::GuildNodeService})
	{
		SCOPED_TRACE(eNodeType_Name(type));
		EXPECT_FALSE(NodeUtils::IsZoneScopedNodeType(type));
		EXPECT_EQ(NodeUtils::DiscoveryPrefixFor(ConfiguredPrefix(type), 7), ConfiguredPrefix(type));
	}
}

TEST(DiscoveryZoneScopeTest, UnrecognisedPrefixesAreLeftAlone)
{
	// 只认精确的 "<eNodeType 名>.rpc",不做子串匹配。
	EXPECT_EQ(NodeUtils::DiscoveryPrefixFor("", 7), "");
	EXPECT_EQ(NodeUtils::DiscoveryPrefixFor(".rpc", 7), ".rpc");
	EXPECT_EQ(NodeUtils::DiscoveryPrefixFor("SceneNodeService", 7), "SceneNodeService");
	EXPECT_EQ(NodeUtils::DiscoveryPrefixFor("UnknownNodeService.rpc", 7), "UnknownNodeService.rpc");
	EXPECT_EQ(NodeUtils::DiscoveryPrefixFor("XSceneNodeService.rpc", 7), "XSceneNodeService.rpc");
	EXPECT_EQ(NodeUtils::DiscoveryPrefixFor("SceneNodeService.rpc/zone/1/", 7), "SceneNodeService.rpc/zone/1/");
}

TEST(DiscoveryZoneScopeTest, ResyncDelayStaysWithinTheJitterBand)
{
	using namespace discovery_resync;
	EXPECT_DOUBLE_EQ(ResyncDelaySeconds(0.0), kResyncIntervalSeconds * (1.0 - kResyncJitterRatio));
	EXPECT_DOUBLE_EQ(ResyncDelaySeconds(0.5), kResyncIntervalSeconds);
	EXPECT_DOUBLE_EQ(ResyncDelaySeconds(1.0), kResyncIntervalSeconds * (1.0 + kResyncJitterRatio));
	// 随机源出错时钳回区间,不给出 0 或负延迟。
	EXPECT_DOUBLE_EQ(ResyncDelaySeconds(-3.0), ResyncDelaySeconds(0.0));
	EXPECT_DOUBLE_EQ(ResyncDelaySeconds(9.0), ResyncDelaySeconds(1.0));
	EXPECT_GE(ResyncDelaySeconds(0.0), 30.0); // 远长于原来的 2 秒
}

TEST(DiscoveryZoneScopeTest, CanceledWatchRetryBacksOffAndCaps)
{
	using namespace discovery_resync;
	double backoff = 0.0;
	backoff = NextCancelRetrySeconds(backoff);
	EXPECT_DOUBLE_EQ(backoff, kCancelRetryMinSeconds);
	backoff = NextCancelRetrySeconds(backoff);
	EXPECT_DOUBLE_EQ(backoff, 2.0);
	for (int i = 0; i < 10; ++i)
	{
		backoff = NextCancelRetrySeconds(backoff);
	}
	EXPECT_DOUBLE_EQ(backoff, kCancelRetryMaxSeconds);
}

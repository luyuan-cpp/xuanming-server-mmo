// gate 路由服子集的纯计算单测(handler/rpc/router_subset_policy.h、core/node/system/discovery/rendezvous_subset.h,
// docs/design/connection-fanout-subsetting-20261106.md §3.1、§3.2)。
//
// 不进 gate.vcxproj,登记在 cpp/tests/routing_identity_test/routing_identity_test.vcxproj。只测纯函数:
// 打分、HRW 取前 k、分布与增减时的移动量、轨道分组、环境变量解析、对账计划、选点分级。
// 没覆盖的一层:router_subset.cpp 把这些接到 Node 的准入 / 发现观察者 / 拆连接缝、注册表与通道上,以及 main.cpp 的安装,
// 它们拖着节点注册表与 gRPC 通道,进不了这个工程,靠核对调用点与冒烟(3 个 zone、3 台路由服、k = 2,
// 看每台 gate 只连 2 台路由服)。

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <iterator>
#include <map>
#include <optional>
#include <set>
#include <string>
#include <string_view>
#include <vector>

#include "handler/rpc/router_subset_policy.h"

namespace
{
	using gate_router_subset::ChannelHealth;
	using gate_router_subset::ComputeSubset;
	using gate_router_subset::PickByHealth;
	using gate_router_subset::PlanReconcile;
	using gate_router_subset::RouterMember;
	using release_track::Track;

	// 形如真实 node_uuid 的确定性串。
	std::string RouterUuid(int index)
	{
		std::string digits = std::to_string(index);
		return "00000000-0000-4000-8000-" + std::string(12 - digits.size(), '0') + digits;
	}

	std::vector<std::string> RouterUuids(int count, int firstIndex = 1)
	{
		std::vector<std::string> uuids;
		for (int index = firstIndex; index < firstIndex + count; ++index)
		{
			uuids.push_back(RouterUuid(index));
		}
		return uuids;
	}

	// 返回的成员借用 uuids 里的字符串:uuids 必须是具名变量,活到用完这些成员为止(不能传临时量)。
	std::vector<RouterMember> Members(const std::vector<std::string> &uuids, std::optional<Track> track = Track::kStable)
	{
		std::vector<RouterMember> members;
		for (const auto &uuid : uuids)
		{
			members.push_back(RouterMember{uuid, track});
		}
		return members;
	}

	std::set<std::string> AsSet(const std::vector<std::string> &uuids)
	{
		return {uuids.begin(), uuids.end()};
	}

	std::vector<std::string_view> Views(const std::vector<std::string> &uuids)
	{
		return {uuids.begin(), uuids.end()};
	}
} // namespace

// ---- 打分 ----

TEST(RouterSubsetTest, Fnv1aMatchesTheReferenceVectors)
{
	// FNV 官方测试向量:实现错一位,子集就会在 Windows / Linux 之间或改版前后悄悄变。
	EXPECT_EQ(rendezvous_subset::Fnv1a64(""), 0xcbf29ce484222325ull);
	EXPECT_EQ(rendezvous_subset::Fnv1a64("a"), 0xaf63dc4c8601ec8cull);
	EXPECT_EQ(rendezvous_subset::Fnv1a64("foobar"), 0x85944171f73967e8ull);
}

TEST(RouterSubsetTest, ScoreIsPinned)
{
	// 钉住「key + '/' + 成员 id,再终混」这套规则。改规则会让滚动期间新旧进程选出不同的子集,必须是有意的。
	EXPECT_EQ(rendezvous_subset::Score("7", "uuid-a"), 0x8b723b9c489f2a3eull);
	EXPECT_EQ(rendezvous_subset::Score("7", "uuid-b"), 0x81832b909a05ce97ull);
	EXPECT_EQ(rendezvous_subset::Score("7", "uuid-a"),
			  rendezvous_subset::Mix64(rendezvous_subset::Fnv1a64("7/uuid-a")));
}

// ---- HRW 取前 k ----

TEST(RouterSubsetTest, TopIndicesIsOrderedAndBounded)
{
	const auto uuids = RouterUuids(8);
	const auto views = Views(uuids);

	const auto top3 = rendezvous_subset::TopIndices("12", views, 3);
	ASSERT_EQ(top3.size(), 3u);
	for (std::size_t position = 1; position < top3.size(); ++position)
	{
		EXPECT_GE(rendezvous_subset::Score("12", views[top3[position - 1]]),
				  rendezvous_subset::Score("12", views[top3[position]]));
	}
	// 前 3 名就是全排序的前 3 名。
	const auto all = rendezvous_subset::TopIndices("12", views, 100);
	ASSERT_EQ(all.size(), views.size());
	EXPECT_TRUE(std::equal(top3.begin(), top3.end(), all.begin()));

	EXPECT_TRUE(rendezvous_subset::TopIndices("12", views, 0).empty());
	EXPECT_TRUE(rendezvous_subset::TopIndices("12", {}, 3).empty());
}

TEST(RouterSubsetTest, SubsetDoesNotDependOnMemberOrder)
{
	auto uuids = RouterUuids(10);
	const auto forward = ComputeSubset(5, Track::kStable, Members(uuids), 3);
	std::reverse(uuids.begin(), uuids.end());
	const auto reversed = ComputeSubset(5, Track::kStable, Members(uuids), 3);
	EXPECT_EQ(forward, reversed);
	EXPECT_EQ(forward.size(), 3u);
	EXPECT_TRUE(std::is_sorted(forward.begin(), forward.end()));
}

TEST(RouterSubsetTest, EveryGateConnectsToAtMostKRoutersWhateverTheFleetSize)
{
	// 第 30 条的完成标准:加路由服实例时,每台 gate 的路由服连接数不随它增长。
	for (const int routerCount : {1, 2, 3, 5, 20, 100})
	{
		SCOPED_TRACE(routerCount);
		const auto uuids = RouterUuids(routerCount);
		const auto members = Members(uuids);
		for (uint32_t zoneId = 1; zoneId <= 50; ++zoneId)
		{
			const auto subset = ComputeSubset(zoneId, Track::kStable, members, 3);
			EXPECT_EQ(subset.size(), std::min<std::size_t>(3, routerCount));
		}
	}
}

TEST(RouterSubsetTest, ZonesSpreadEvenlyOverRouters)
{
	// 1000 个 zone、10 台与 20 台路由服、k = 3:每台承担的 zone 数在均值 ±20% 以内(设计稿 §7)。
	for (const int routerCount : {10, 20})
	{
		SCOPED_TRACE(routerCount);
		const auto uuids = RouterUuids(routerCount);
		const auto members = Members(uuids);
		std::map<std::string, int> zonesPerRouter;
		for (uint32_t zoneId = 1; zoneId <= 1000; ++zoneId)
		{
			for (const auto &uuid : ComputeSubset(zoneId, Track::kStable, members, 3))
			{
				++zonesPerRouter[uuid];
			}
		}
		const double mean = 1000.0 * 3 / routerCount;
		ASSERT_EQ(zonesPerRouter.size(), uuids.size());
		for (const auto &[uuid, zones] : zonesPerRouter)
		{
			EXPECT_GE(zones, mean * 0.8) << uuid;
			EXPECT_LE(zones, mean * 1.2) << uuid;
		}
	}
}

TEST(RouterSubsetTest, AddingARouterOnlyMovesZonesThatNowRankIt)
{
	const auto before = RouterUuids(10);
	auto after = before;
	after.push_back(RouterUuid(11));
	int moved = 0;
	for (uint32_t zoneId = 1; zoneId <= 1000; ++zoneId)
	{
		const auto oldSubset = AsSet(ComputeSubset(zoneId, Track::kStable, Members(before), 3));
		const auto newSubset = AsSet(ComputeSubset(zoneId, Track::kStable, Members(after), 3));
		if (oldSubset == newSubset)
		{
			continue;
		}
		++moved;
		// 变了的 zone 只是新路由服顶掉了一台,另外两台不动。
		std::vector<std::string> gained;
		std::set_difference(newSubset.begin(), newSubset.end(), oldSubset.begin(), oldSubset.end(),
							std::back_inserter(gained));
		std::vector<std::string> lost;
		std::set_difference(oldSubset.begin(), oldSubset.end(), newSubset.begin(), newSubset.end(),
							std::back_inserter(lost));
		EXPECT_EQ(gained, std::vector<std::string>{RouterUuid(11)}) << zoneId;
		EXPECT_EQ(lost.size(), 1u) << zoneId;
	}
	// 期望约 1000 × 3 / 11 ≈ 273 个 zone 换人;远少于全部重排。
	EXPECT_GT(moved, 150);
	EXPECT_LT(moved, 400);
}

TEST(RouterSubsetTest, RemovingARouterOnlyMovesZonesThatRankedIt)
{
	const auto before = RouterUuids(10);
	std::vector<std::string> after;
	const auto removed = RouterUuid(4);
	std::copy_if(before.begin(), before.end(), std::back_inserter(after),
				 [&removed](const std::string &uuid) { return uuid != removed; });
	for (uint32_t zoneId = 1; zoneId <= 1000; ++zoneId)
	{
		const auto oldSubset = AsSet(ComputeSubset(zoneId, Track::kStable, Members(before), 3));
		const auto newSubset = AsSet(ComputeSubset(zoneId, Track::kStable, Members(after), 3));
		if (!oldSubset.contains(removed))
		{
			EXPECT_EQ(oldSubset, newSubset) << zoneId;
			continue;
		}
		// 排进过它的 zone:只少了它、补上一台,另外两台不动。
		EXPECT_FALSE(newSubset.contains(removed)) << zoneId;
		std::vector<std::string> kept;
		std::set_intersection(oldSubset.begin(), oldSubset.end(), newSubset.begin(), newSubset.end(),
							  std::back_inserter(kept));
		EXPECT_EQ(kept.size(), 2u) << zoneId;
		EXPECT_EQ(newSubset.size(), 3u) << zoneId;
	}
}

// ---- 发布轨道 ----

TEST(RouterSubsetTest, StableGateNeverSubsetsCanaryRouters)
{
	const auto stable = RouterUuids(6, 1);
	const auto canary = RouterUuids(4, 100);
	auto members = Members(stable, Track::kStable);
	for (const auto &member : Members(canary, Track::kCanary))
	{
		members.push_back(member);
	}
	const auto subset = ComputeSubset(9, Track::kStable, members, 3);
	EXPECT_EQ(subset.size(), 3u);
	for (const auto &uuid : subset)
	{
		EXPECT_TRUE(std::find(stable.begin(), stable.end(), uuid) != stable.end()) << uuid;
	}
}

TEST(RouterSubsetTest, CanaryGateKeepsTheStableFallbackConnected)
{
	// release_track::Select 在 canary 组没有可用实例时退回 stable:canary gate 两组各取前 k。
	const auto stable = RouterUuids(6, 1);
	const auto canary = RouterUuids(4, 100);
	auto members = Members(stable, Track::kStable);
	for (const auto &member : Members(canary, Track::kCanary))
	{
		members.push_back(member);
	}
	const auto subset = AsSet(ComputeSubset(9, Track::kCanary, members, 3));
	const auto stableTop = AsSet(ComputeSubset(9, Track::kStable, Members(stable, Track::kStable), 3));
	const auto canaryTop = AsSet(ComputeSubset(9, Track::kStable, Members(canary, Track::kStable), 3));
	std::set<std::string> expected = stableTop;
	expected.insert(canaryTop.begin(), canaryTop.end());
	EXPECT_EQ(subset, expected);
	EXPECT_EQ(subset.size(), 6u);
}

TEST(RouterSubsetTest, UnknownTrackRoutersAreNeverSubset)
{
	const auto stable = RouterUuids(2, 1);
	const auto unknown = RouterUuids(5, 50);
	auto members = Members(stable, Track::kStable);
	for (const auto &member : Members(unknown, std::nullopt))
	{
		members.push_back(member);
	}
	EXPECT_EQ(ComputeSubset(3, Track::kStable, members, 3), stable);
	EXPECT_EQ(ComputeSubset(3, Track::kCanary, members, 3), stable);
}

// ---- GATE_ROUTER_SUBSET_SIZE ----

TEST(RouterSubsetTest, SubsetSizeParsesOnlyPlainIntegersInRange)
{
	using gate_router_subset::ParseSubsetSize;
	EXPECT_EQ(ParseSubsetSize(nullptr), gate_router_subset::kDefaultSubsetSize);
	EXPECT_EQ(ParseSubsetSize(""), gate_router_subset::kDefaultSubsetSize);
	EXPECT_EQ(ParseSubsetSize("2"), 2u);
	EXPECT_EQ(ParseSubsetSize("3"), 3u);
	EXPECT_EQ(ParseSubsetSize("64"), 64u);
	for (const char *invalid : {"0", "1", "65", "-3", "+3", " 3", "3 ", "3.0", "three", "0x3", "99999999999999999999"})
	{
		EXPECT_FALSE(ParseSubsetSize(invalid).has_value()) << invalid;
	}
}

// ---- 对账计划 ----

TEST(RouterSubsetTest, ReconcileConnectsNewMembersAndSchedulesClosesForLeavers)
{
	const std::vector<std::string> subset{"a", "b"};
	const std::vector<std::string> discovered{"a", "b", "c"};
	const std::vector<std::string> connected{"a", "c"};
	const auto plan = PlanReconcile(subset, Views(discovered), Views(connected), {});
	EXPECT_EQ(plan.connect, std::vector<std::string>{"b"});
	EXPECT_EQ(plan.scheduleClose, std::vector<std::string>{"c"});
	EXPECT_TRUE(plan.cancelClose.empty());
}

TEST(RouterSubsetTest, ReconcileDoesNotRescheduleAPendingClose)
{
	// 已排过关闭的不再排(到期时刻不顺延),也不撤销。
	const std::vector<std::string> subset{"a", "b"};
	const std::vector<std::string> discovered{"a", "b", "c"};
	const std::vector<std::string> connected{"a", "b", "c"};
	const std::vector<std::string> pending{"c"};
	const auto plan = PlanReconcile(subset, Views(discovered), Views(connected), Views(pending));
	EXPECT_TRUE(plan.connect.empty());
	EXPECT_TRUE(plan.scheduleClose.empty());
	EXPECT_TRUE(plan.cancelClose.empty());
}

TEST(RouterSubsetTest, ReconcileCancelsClosesThatNoLongerApply)
{
	const std::vector<std::string> subset{"a", "b"};
	// b 回到了子集;d 已不在快照里(节点摘除会拆它);e 在快照里但连接已经没了。
	const std::vector<std::string> discovered{"a", "b", "e"};
	const std::vector<std::string> connected{"a", "b", "d"};
	const std::vector<std::string> pending{"b", "d", "e"};
	const auto plan = PlanReconcile(subset, Views(discovered), Views(connected), Views(pending));
	EXPECT_EQ(plan.cancelClose, (std::vector<std::string>{"b", "d", "e"}));
	// 已不在快照里的 d 不排关闭,由节点摘除拆;e 不在子集,不建连。
	EXPECT_TRUE(plan.scheduleClose.empty());
	EXPECT_TRUE(plan.connect.empty());
}

TEST(RouterSubsetTest, ReconcileLeavesRemovedRoutersToNodeRemoval)
{
	// x 已不在发现快照里(etcd 登记删了),连接还在:节点摘除已经排了销毁,这里不排延后关闭,也不建连。
	const std::vector<std::string> subset{"a"};
	const std::vector<std::string> discovered{"a"};
	const std::vector<std::string> connected{"a", "x"};
	const auto plan = PlanReconcile(subset, Views(discovered), Views(connected), {});
	EXPECT_TRUE(plan.scheduleClose.empty());
	EXPECT_TRUE(plan.connect.empty());
	EXPECT_TRUE(plan.cancelClose.empty());
}

// ---- 选点分级 ----

TEST(RouterSubsetTest, PickPrefersReadyThenConnectableThenTheRest)
{
	EXPECT_FALSE(PickByHealth({}, 0).has_value());

	const std::vector<ChannelHealth> mixed{ChannelHealth::kOther, ChannelHealth::kConnectable, ChannelHealth::kReady,
										   ChannelHealth::kOther, ChannelHealth::kReady};
	// READY 有两台(下标 2、4):随机值对 2 取模,序号沿用原顺序。
	EXPECT_EQ(PickByHealth(mixed, 0), 2u);
	EXPECT_EQ(PickByHealth(mixed, 1), 4u);
	EXPECT_EQ(PickByHealth(mixed, 2), 2u);

	const std::vector<ChannelHealth> noReady{ChannelHealth::kOther, ChannelHealth::kConnectable, ChannelHealth::kOther};
	for (uint64_t random = 0; random < 5; ++random)
	{
		EXPECT_EQ(PickByHealth(noReady, random), 1u);
	}

	// 整组都不健康时仍挑一个,让这次调用按原有失败路径收尾(不是返回「没有路由服」)。
	const std::vector<ChannelHealth> allBad{ChannelHealth::kOther, ChannelHealth::kOther};
	EXPECT_EQ(PickByHealth(allBad, 0), 0u);
	EXPECT_EQ(PickByHealth(allBad, 1), 1u);
}

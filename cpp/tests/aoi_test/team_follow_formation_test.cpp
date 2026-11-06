// 组队跟随纯逻辑层单测(docs/design/team-follow-server-trail.md)。
// 只驱动 TeamFollowTrail / TeamFollowFormation:不建 ECS、不连导航,导航可走性用桩注入。
// 时间步长与 scene 一致(20 FPS),队长移速取客户端基础移速 9 米/秒(每 tick 0.45 米)。

#include <gtest/gtest.h>

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <limits>
#include <vector>

#include "player/system/team_follow_formation.h"

namespace
{
	constexpr double kEps = 1e-9;
	constexpr double kDt = 0.05;
	constexpr double kWalkStep = 0.45; // 9 米/秒 × 0.05 秒
	constexpr uint64_t kFirstMemberId = 100;

	FollowPoint P(double x, double y)
	{
		return FollowPoint{x, y, 0.0};
	}

	// 一个已经排好队的队形:队长从 (0,0) 沿 +x 走到 (20,0),members 名队员按名次锁在各自槽位上
	// (第 k 名在队长身后 (k+1)×1.4)。
	TeamFollowFormation MakeColumnAt20(std::size_t members)
	{
		TeamFollowFormation formation(P(0.0, 0.0));
		std::vector<TeamFollowStep> steps;
		for (int i = 1; i <= 40; ++i)
		{
			formation.Tick(P(0.5 * i, 0.0), kDt, {}, steps);
		}
		for (std::size_t rank = 0; rank < members; ++rank)
		{
			TeamFollowMember member;
			member.playerId = kFirstMemberId + rank;
			member.joinSeq = static_cast<uint32_t>(rank + 1);
			member.cursorOdo = formation.SlotOdo(rank);
			EXPECT_TRUE(formation.Add(member));
		}
		return formation;
	}

	FollowPoint MemberPosition(const TeamFollowFormation& formation, uint64_t playerId)
	{
		const auto* member = formation.Find(playerId);
		EXPECT_NE(member, nullptr);
		return member != nullptr ? formation.Trail().PointAt(member->cursorOdo) : FollowPoint{};
	}

	// 队长从 from 直线走到 to,每 tick kWalkStep(最后一步可短),每步一个 Tick。返回每 tick 的结果。
	std::vector<std::vector<TeamFollowStep>> WalkLeader(TeamFollowFormation& formation, FollowPoint from, FollowPoint to)
	{
		std::vector<std::vector<TeamFollowStep>> ticks;
		const double length = FollowHorizontalDistance(from, to);
		const int count = static_cast<int>(std::ceil(length / kWalkStep - 1e-9));
		for (int i = 1; i <= count; ++i)
		{
			const double t = std::min(1.0, kWalkStep * i / length);
			std::vector<TeamFollowStep> steps;
			formation.Tick(P(from.x + (to.x - from.x) * t, from.y + (to.y - from.y) * t), kDt, {}, steps);
			ticks.push_back(steps);
		}
		return ticks;
	}

	void ExpectAt(const FollowPoint& actual, double x, double y)
	{
		EXPECT_NEAR(actual.x, x, 1e-6);
		EXPECT_NEAR(actual.y, y, 1e-6);
	}
} // namespace

// ---- 一直走的情况:同动作、同间隔 ----

TEST(TeamFollowFormationTest, StraightWalkMovesEveryMemberExactlyAsFarAsTheLeader)
{
	auto formation = MakeColumnAt20(4);
	const auto ticks = WalkLeader(formation, P(20.0, 0.0), P(29.0, 0.0));
	ASSERT_EQ(ticks.size(), 20u);
	for (const auto& steps : ticks)
	{
		ASSERT_EQ(steps.size(), 4u);
		for (const auto& step : steps)
		{
			EXPECT_FALSE(step.placed);
			EXPECT_FALSE(step.suspended);
			EXPECT_NEAR(step.moved, kWalkStep, 1e-6) << "队员每 tick 走的路必须和队长一样多";
		}
	}
	for (std::size_t rank = 0; rank < 4; ++rank)
	{
		ExpectAt(MemberPosition(formation, kFirstMemberId + rank), 29.0 - 1.4 * static_cast<double>(rank + 1), 0.0);
	}
}

TEST(TeamFollowFormationTest, MembersStopOnTheSameTickAsTheLeader)
{
	auto formation = MakeColumnAt20(4);
	WalkLeader(formation, P(20.0, 0.0), P(24.5, 0.0));
	std::vector<TeamFollowStep> steps;
	for (int i = 0; i < 5; ++i)
	{
		formation.Tick(P(24.5, 0.0), kDt, {}, steps);
		for (const auto& step : steps)
		{
			EXPECT_NEAR(step.moved, 0.0, kEps) << "队长停下的第一个 tick 队员就得停";
		}
	}
	for (std::size_t rank = 0; rank < 4; ++rank)
	{
		ExpectAt(MemberPosition(formation, kFirstMemberId + rank), 24.5 - 1.4 * static_cast<double>(rank + 1), 0.0);
	}
}

TEST(TeamFollowFormationTest, MembersStartOnTheSameTickAsTheLeader)
{
	auto formation = MakeColumnAt20(4);
	std::vector<TeamFollowStep> steps;
	formation.Tick(P(20.0, 0.0), kDt, {}, steps); // 站着
	formation.Tick(P(20.0 + kWalkStep, 0.0), kDt, {}, steps);
	ASSERT_EQ(steps.size(), 4u);
	for (const auto& step : steps)
	{
		EXPECT_NEAR(step.moved, kWalkStep, 1e-6) << "队长起步的那个 tick 队员一起起步";
	}
}

TEST(TeamFollowFormationTest, MembersTurnAtTheLeadersCornerWithoutCuttingIt)
{
	auto formation = MakeColumnAt20(4);
	const auto ticks = WalkLeader(formation, P(20.0, 0.0), P(20.0, 5.4));
	for (const auto& steps : ticks)
	{
		for (const auto& step : steps)
		{
			// 只能落在队长走过的 L 形路上:x 段 (y=0, x<=20) 或 y 段 (x=20, y>=0)。
			const bool onXLeg = std::abs(step.position.y) < 1e-6 && step.position.x <= 20.0 + 1e-6;
			const bool onYLeg = std::abs(step.position.x - 20.0) < 1e-6 && step.position.y >= -1e-6;
			EXPECT_TRUE(onXLeg || onYLeg) << "队员抄了近路:(" << step.position.x << "," << step.position.y << ")";
		}
	}
	// 沿路间距恒为 1.4:第 4 名还差 0.2 才到拐角。
	ExpectAt(MemberPosition(formation, kFirstMemberId + 0), 20.0, 4.0);
	ExpectAt(MemberPosition(formation, kFirstMemberId + 1), 20.0, 2.6);
	ExpectAt(MemberPosition(formation, kFirstMemberId + 2), 20.0, 1.2);
	ExpectAt(MemberPosition(formation, kFirstMemberId + 3), 19.8, 0.0);
}

// ---- 特例:队长原地小范围来回走 ----

TEST(TeamFollowFormationTest, LeaderShufflingBackAndForthInFrontOfTheColumnMovesNobody)
{
	auto formation = MakeColumnAt20(4);
	const std::size_t pointsBefore = formation.Trail().PointCount();
	// 沿刚走过的路往回 1.0、再走回来;往回 0.8、再走回来……共 10 个来回,始终在第一名队员(1.4)之前。
	for (int cycle = 0; cycle < 10; ++cycle)
	{
		const double back = cycle % 2 == 0 ? 1.0 : 0.8;
		for (const auto& leg : {WalkLeader(formation, P(20.0, 0.0), P(20.0 - back, 0.0)),
								WalkLeader(formation, P(20.0 - back, 0.0), P(20.0, 0.0))})
		{
			for (const auto& steps : leg)
			{
				for (const auto& step : steps)
				{
					EXPECT_NEAR(step.moved, 0.0, 1e-6) << "队长在队伍前面来回晃,队员不该动";
				}
			}
		}
	}
	for (std::size_t rank = 0; rank < 4; ++rank)
	{
		ExpectAt(MemberPosition(formation, kFirstMemberId + rank), 20.0 - 1.4 * static_cast<double>(rank + 1), 0.0);
	}
	EXPECT_LE(formation.Trail().PointCount(), pointsBefore + 3) << "来回走的回路必须被剪掉,轨迹不能越积越长";

	// 晃完往前走:队员走的是剪干净的直路,间距照旧。
	WalkLeader(formation, P(20.0, 0.0), P(23.6, 0.0));
	for (std::size_t rank = 0; rank < 4; ++rank)
	{
		ExpectAt(MemberPosition(formation, kFirstMemberId + rank), 23.6 - 1.4 * static_cast<double>(rank + 1), 0.0);
	}
}

TEST(TeamFollowFormationTest, SidewaysWigglesMoveTheColumnAtMostOnce)
{
	auto formation = MakeColumnAt20(2);
	const uint64_t first = kFirstMemberId;
	// 第一次往旁边走 1.0:看上去和真的拐弯一样,队员沿路跟上 1.0。
	double movedFirst = 0.0;
	for (const auto& steps : WalkLeader(formation, P(20.0, 0.0), P(20.0, 1.0)))
	{
		movedFirst += steps[0].moved;
	}
	EXPECT_NEAR(movedFirst, 1.0, 1e-6);

	// 之后在岔口两侧 ±1.0 来回晃:队员不再被带着走。
	double movedLater = 0.0;
	for (int cycle = 0; cycle < 5; ++cycle)
	{
		for (const auto& leg : {WalkLeader(formation, P(20.0, 1.0), P(20.0, -1.0)),
								WalkLeader(formation, P(20.0, -1.0), P(20.0, 1.0))})
		{
			for (const auto& steps : leg)
			{
				movedLater += steps[0].moved + steps[1].moved;
			}
		}
	}
	EXPECT_LT(movedLater, 0.05) << "横着来回晃不能把队伍一点点往前带";
	ExpectAt(MemberPosition(formation, first), 19.6, 0.0);
}

TEST(TeamFollowFormationTest, RealReversalKeepsOrderAndReformsBehindTheLeader)
{
	auto formation = MakeColumnAt20(4);
	// 队长掉头,沿原路往回走 10 米,穿过整支队伍。
	for (const auto& steps : WalkLeader(formation, P(20.0, 0.0), P(10.0, 0.0)))
	{
		for (const auto& step : steps)
		{
			EXPECT_GE(step.moved, 0.0);
			EXPECT_NEAR(step.position.y, 0.0, 1e-6) << "队员只能走在队长走过的路上";
		}
	}
	// 发夹折在第一名队员原来的位置(18.6):全员走到那里掉头,顺序不变,沿路间距恒定。
	for (std::size_t rank = 0; rank < 4; ++rank)
	{
		ExpectAt(MemberPosition(formation, kFirstMemberId + rank), 10.0 + 1.4 * static_cast<double>(rank + 1), 0.0);
	}
}

// ---- 加入、离开、暂停 ----

TEST(TeamFollowFormationTest, NewcomerWalksToTheEndOfTheLineAndStopsOneSpacingBehind)
{
	auto formation = MakeColumnAt20(2);
	const uint64_t newcomer = kFirstMemberId + 9;
	const std::size_t rank = formation.RankFor(3, newcomer);
	ASSERT_EQ(rank, 2u);
	// ECS 层的加入顺序:剪尾 → 寻路到队尾(这里直接给直线)→ 接在尾部 → 站在新尾部。
	formation.TrimTailForNewcomer(rank);
	ExpectAt(formation.Trail().Tail(), 20.0 - 4.2, 0.0);
	const std::vector<FollowPoint> path{P(16.0, 2.0), formation.Trail().Tail()};
	formation.Trail().PrependPath(path);
	TeamFollowMember member;
	member.playerId = newcomer;
	member.joinSeq = 3;
	member.cursorOdo = formation.Trail().TailOdo();
	ASSERT_TRUE(formation.Add(member));

	std::vector<TeamFollowStep> steps;
	for (int i = 0; i < 10; ++i)
	{
		formation.Tick(P(20.0, 0.0), kDt, {}, steps);
		EXPECT_NEAR(steps[0].moved, 0.0, kEps);
		EXPECT_NEAR(steps[1].moved, 0.0, kEps);
		EXPECT_LE(steps[2].moved, kTeamFollowCatchUpSpeed * kDt + 1e-9);
	}
	ExpectAt(MemberPosition(formation, newcomer), 15.8, 0.0);
}

TEST(TeamFollowFormationTest, NewcomerCloserThanItsSlotWaitsUntilTheLeaderWalksAway)
{
	TeamFollowFormation formation(P(0.0, 0.0));
	const uint64_t newcomer = kFirstMemberId;
	formation.TrimTailForNewcomer(0);
	const std::vector<FollowPoint> path{P(0.8, 0.3), P(0.0, 0.0)};
	formation.Trail().PrependPath(path);
	TeamFollowMember member;
	member.playerId = newcomer;
	member.cursorOdo = formation.Trail().TailOdo();
	ASSERT_TRUE(formation.Add(member));

	std::vector<TeamFollowStep> steps;
	for (int i = 0; i < 5; ++i)
	{
		formation.Tick(P(0.0, 0.0), kDt, {}, steps);
		EXPECT_NEAR(steps[0].moved, 0.0, kEps) << "已经比槽位离队长近:原地等,不往后退";
	}
	WalkLeader(formation, P(0.0, 0.0), P(-3.6, 0.0));
	ExpectAt(MemberPosition(formation, newcomer), -2.2, 0.0);
}

TEST(TeamFollowFormationTest, LeavingMemberIsReplacedByTheOneBehindAtCatchUpSpeed)
{
	auto formation = MakeColumnAt20(3);
	ASSERT_TRUE(formation.Remove(kFirstMemberId));
	std::vector<TeamFollowStep> steps;
	for (int i = 0; i < 10; ++i)
	{
		formation.Tick(P(20.0, 0.0), kDt, {}, steps);
		for (const auto& step : steps)
		{
			EXPECT_LE(step.moved, kTeamFollowCatchUpSpeed * kDt + 1e-9);
		}
	}
	ExpectAt(MemberPosition(formation, kFirstMemberId + 1), 18.6, 0.0);
	ExpectAt(MemberPosition(formation, kFirstMemberId + 2), 17.2, 0.0);
}

TEST(TeamFollowFormationTest, SuspendedMemberKeepsItsSlotAndIsPlacedAfterFallingTooFarBehind)
{
	auto formation = MakeColumnAt20(2);
	const uint64_t first = kFirstMemberId;
	const uint64_t second = kFirstMemberId + 1;
	formation.SetSuspended(first, true);
	WalkLeader(formation, P(20.0, 0.0), P(25.4, 0.0));
	// 第一名原地不动、名次照留;第二名照常保持在 2.8 处(从第一名身上穿过去)。
	ExpectAt(MemberPosition(formation, first), 18.6, 0.0);
	ExpectAt(MemberPosition(formation, second), 22.6, 0.0);

	WalkLeader(formation, P(25.4, 0.0), P(50.0, 0.0));
	ASSERT_NE(formation.Find(first), nullptr);
	EXPECT_TRUE(formation.Find(first)->needsPlacement) << "落后超过上限就不再为他保留轨迹";
	EXPECT_GE(formation.Trail().TailOdo(), formation.Trail().HeadOdo() - kTeamFollowMaxLag - 2 * kTeamFollowSpacing);

	formation.SetSuspended(first, false);
	std::vector<TeamFollowStep> steps;
	formation.Tick(P(50.0, 0.0), kDt, {}, steps);
	ASSERT_EQ(steps.size(), 2u);
	EXPECT_TRUE(steps[0].placed) << "恢复后直接放进槽位";
	ExpectAt(steps[0].position, 48.6, 0.0);
}

TEST(TeamFollowFormationTest, RanksFollowJoinSeqNotArrivalOrder)
{
	TeamFollowFormation formation(P(0.0, 0.0));
	TeamFollowMember late;
	late.playerId = 7;
	late.joinSeq = 3;
	TeamFollowMember early;
	early.playerId = 9;
	early.joinSeq = 2;
	ASSERT_TRUE(formation.Add(late));
	ASSERT_TRUE(formation.Add(early));
	EXPECT_EQ(formation.RankOf(9).value_or(99), 0u);
	EXPECT_EQ(formation.RankOf(7).value_or(99), 1u);
	EXPECT_FALSE(formation.Add(early)) << "同一个人不能进两次";
	for (uint64_t id = 20; id < 22; ++id)
	{
		TeamFollowMember extra;
		extra.playerId = id;
		extra.joinSeq = static_cast<uint32_t>(id);
		ASSERT_TRUE(formation.Add(extra));
	}
	TeamFollowMember fifth;
	fifth.playerId = 30;
	EXPECT_FALSE(formation.Add(fifth)) << "队长之外最多 4 人";
}

TEST(TeamFollowFormationTest, TrailStaysBoundedOnALongWalk)
{
	auto formation = MakeColumnAt20(4);
	FollowPoint from = P(20.0, 0.0);
	for (int leg = 0; leg < 100; ++leg)
	{
		const FollowPoint to = P(from.x + 10.0, leg % 2 == 0 ? 6.0 : 0.0);
		WalkLeader(formation, from, to);
		from = to;
	}
	// 只需要覆盖到最后一名队员(5.6)与保留历史(7.0):按每 tick 一个点,远小于这个上限。
	EXPECT_LE(formation.Trail().PointCount(), 40u);
	EXPECT_GE(formation.Trail().Length(), 4 * kTeamFollowSpacing);
}

// ---- 轨迹本身 ----

TEST(TeamFollowTrailTest, RewindIsSkippedWhenTheShortcutIsNotWalkable)
{
	TeamFollowTrail trail;
	trail.Reset(P(0.0, 0.0));
	const double lowest = std::numeric_limits<double>::lowest();
	trail.Feed(P(0.45, 0.0), lowest, {});
	trail.Feed(P(0.9, 0.0), lowest, {});
	const auto blocked = trail.Feed(P(0.6, 0.0), lowest, [](const FollowPoint&, const FollowPoint&) { return false; });
	EXPECT_EQ(blocked, TeamFollowTrail::FeedResult::kAppended) << "捷径不可走:不剪,照常接上(折成发夹)";
	EXPECT_NEAR(trail.HeadOdo(), 1.2, 1e-9);

	TeamFollowTrail other;
	other.Reset(P(0.0, 0.0));
	other.Feed(P(0.45, 0.0), lowest, {});
	other.Feed(P(0.9, 0.0), lowest, {});
	EXPECT_EQ(other.Feed(P(0.6, 0.0), lowest, {}), TeamFollowTrail::FeedResult::kRewound);
	EXPECT_NEAR(other.HeadOdo(), 0.6, 1e-9);
}

TEST(TeamFollowTrailTest, RewindNeverCutsBehindTheProtectedOdo)
{
	TeamFollowTrail trail;
	trail.Reset(P(0.0, 0.0));
	const double lowest = std::numeric_limits<double>::lowest();
	for (int i = 1; i <= 4; ++i)
	{
		trail.Feed(P(0.45 * i, 0.0), lowest, {});
	}
	// 保护到 1.2:往回走到 0.9 也只能剪到 1.2,之后接上新位置(发夹折在 1.2)。
	EXPECT_EQ(trail.Feed(P(1.35, 0.0), 1.2, {}), TeamFollowTrail::FeedResult::kRewound);
	EXPECT_EQ(trail.Feed(P(0.9, 0.0), 1.2, {}), TeamFollowTrail::FeedResult::kRewound);
	ExpectAt(trail.PointAt(1.2), 1.2, 0.0);
	ExpectAt(trail.Head(), 0.9, 0.0);
	EXPECT_NEAR(trail.HeadOdo(), 1.5, 1e-9);
}

TEST(TeamFollowTrailTest, PointAtInterpolatesAndClampsAndNearestOdoFindsTheClosestLeg)
{
	TeamFollowTrail trail;
	trail.Reset(P(0.0, 0.0));
	const double lowest = std::numeric_limits<double>::lowest();
	trail.Feed(P(2.0, 0.0), lowest, {});
	trail.Feed(P(2.0, 2.0), lowest, {});
	ExpectAt(trail.PointAt(1.0), 1.0, 0.0);
	ExpectAt(trail.PointAt(3.0), 2.0, 1.0);
	ExpectAt(trail.PointAt(-5.0), 0.0, 0.0);
	ExpectAt(trail.PointAt(99.0), 2.0, 2.0);

	const auto nearest = trail.NearestOdo(P(2.3, 1.5), 0.5);
	ASSERT_TRUE(nearest.has_value());
	EXPECT_NEAR(*nearest, 3.5, 1e-9);
	EXPECT_FALSE(trail.NearestOdo(P(5.0, 5.0), 0.5).has_value());
}

TEST(TeamFollowTrailTest, TrimTailAndPrependKeepOdoMonotonic)
{
	TeamFollowTrail trail;
	trail.Reset(P(0.0, 0.0));
	const double lowest = std::numeric_limits<double>::lowest();
	for (int i = 1; i <= 10; ++i)
	{
		trail.Feed(P(0.5 * i, 0.0), lowest, {});
	}
	trail.TrimTailTo(2.25);
	EXPECT_NEAR(trail.TailOdo(), 2.25, 1e-9);
	ExpectAt(trail.Tail(), 2.25, 0.0);
	const std::vector<FollowPoint> path{P(2.25, 3.0), P(2.25, 1.0), trail.Tail()};
	trail.PrependPath(path);
	ExpectAt(trail.Tail(), 2.25, 3.0);
	EXPECT_NEAR(trail.TailOdo(), 2.25 - 3.0, 1e-9);
	ExpectAt(trail.PointAt(trail.TailOdo() + 1.5), 2.25, 1.5);
}

// ---- 广播节流 ----

TEST(TeamFollowBroadcastTest, BroadcastsOnStartStopTurnSpeedChangeAndPeriodically)
{
	const TeamFollowBroadcastState still{false, 0.0, 0.0};
	const TeamFollowBroadcastState east{true, 9.0, 0.0};
	EXPECT_TRUE(TeamFollowShouldBroadcast(still, east, 1)) << "起步";
	EXPECT_TRUE(TeamFollowShouldBroadcast(east, still, 1)) << "停步";
	EXPECT_FALSE(TeamFollowShouldBroadcast(still, still, 100)) << "一直站着不发";
	EXPECT_FALSE(TeamFollowShouldBroadcast(east, east, 1)) << "匀速直走,外推就是对的";
	EXPECT_TRUE(TeamFollowShouldBroadcast(east, east, kTeamFollowBroadcastIntervalFrames)) << "周期校正";
	const double rad30 = 30.0 * 3.14159265358979323846 / 180.0;
	const double rad10 = 10.0 * 3.14159265358979323846 / 180.0;
	EXPECT_TRUE(TeamFollowShouldBroadcast(east, {true, 9.0 * std::cos(rad30), 9.0 * std::sin(rad30)}, 1)) << "拐弯";
	EXPECT_FALSE(TeamFollowShouldBroadcast(east, {true, 9.0 * std::cos(rad10), 9.0 * std::sin(rad10)}, 1));
	EXPECT_TRUE(TeamFollowShouldBroadcast(east, {true, 10.8, 0.0}, 1)) << "追赶时变速";
	EXPECT_FALSE(TeamFollowShouldBroadcast(east, {true, 9.5, 0.0}, 1));
}

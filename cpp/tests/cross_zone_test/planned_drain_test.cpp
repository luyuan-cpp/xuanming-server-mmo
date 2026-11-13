// scene 节点计划排空的纯规则(docs/design/scene-node-planned-drain.md §4.1、§4.4、§4.5、§4.7):
//   - node_planned_drain.h:预算环境变量的解析、每拍结论;
//   - scene_planned_drain_rules.h:自疏散时刻与限速的配置、令牌桶、「还有没有能接住疏散的同类」;
//   - relocate_target.h:疏散请求的去向提示。
// 都是纯头文件,不起 EventLoop、不碰 ECS;时间由用例注入。
//
// 变异(每次一种)应当让下列用例变红:
//   - ParseBudget 把 "0" 当成缺省值:BudgetZeroDisablesPlannedDrain 红(应急开关失效,SIGTERM 不再直接停机);
//   - Decide 先看到期再看完成:DrainedWinsOverExpiryOnTheSameTick 红;
//   - ComposeBuildLabel 丢掉修订:ComposesBuildAndPodRevision 红(同镜像滚动重启时分不出新旧两代 Pod);
//   - HasOpenWorldPeer 不排除自己或不看 closed:NoTargetWhenOnlySelfOrClosedPeers 红;
//   - Pacer 不封顶 1 秒的量:PacerCapsBurstAtOneSecondOfTokens 红;
//   - ForScene 给副本带上原地图:DungeonGoesToDefaultWorld 红(副本图不是世界地图,scene_manager 会白走一步)。

#include <gtest/gtest.h>

#include <array>
#include <chrono>
#include <string>

#include "node/system/node/node_planned_drain.h"
#include "player/system/relocate_target.h"
#include "world/scene_planned_drain_rules.h"

using namespace std::chrono_literals;

namespace
{
	using Clock = std::chrono::steady_clock;
	const Clock::time_point kT0 = Clock::time_point{} + 1000s;
} // namespace

// ── node_planned_drain.h ─────────────────────────────────────────────────────────────────────────

TEST(PlannedDrainBudget, UnsetOrEmptyUsesNodeDefault)
{
	EXPECT_EQ(planned_drain::ParseBudget(nullptr).kind, planned_drain::BudgetKind::kUseDefault);
	EXPECT_EQ(planned_drain::ParseBudget("").kind, planned_drain::BudgetKind::kUseDefault);
}

TEST(PlannedDrainBudget, BudgetZeroDisablesPlannedDrain)
{
	EXPECT_EQ(planned_drain::ParseBudget("0").kind, planned_drain::BudgetKind::kDisabled);
}

TEST(PlannedDrainBudget, WholeSecondsWithinTheCap)
{
	const auto budget = planned_drain::ParseBudget("95");
	ASSERT_EQ(budget.kind, planned_drain::BudgetKind::kSeconds);
	EXPECT_EQ(budget.seconds, 95s);
	const auto max = planned_drain::ParseBudget(std::to_string(planned_drain::kMaxBudget.count()).c_str());
	ASSERT_EQ(max.kind, planned_drain::BudgetKind::kSeconds);
	EXPECT_EQ(max.seconds, planned_drain::kMaxBudget);
}

TEST(PlannedDrainBudget, MalformedValuesAreRejected)
{
	for (const char *raw : {"-5", "+5", " 5", "5 ", "5s", "1.5", "abc", "86401", "9999999999"})
	{
		EXPECT_EQ(planned_drain::ParseBudget(raw).kind, planned_drain::BudgetKind::kInvalid) << raw;
	}
}

TEST(PlannedDrainVerdict, WaitsUntilDrainedOrExpired)
{
	const auto deadline = kT0 + 30s;
	EXPECT_EQ(planned_drain::Decide(false, kT0, deadline), planned_drain::Verdict::kWait);
	EXPECT_EQ(planned_drain::Decide(true, kT0, deadline), planned_drain::Verdict::kDrained);
	EXPECT_EQ(planned_drain::Decide(false, deadline, deadline), planned_drain::Verdict::kExpired);
}

TEST(PlannedDrainVerdict, DrainedWinsOverExpiryOnTheSameTick)
{
	const auto deadline = kT0;
	EXPECT_EQ(planned_drain::Decide(true, kT0 + 1s, deadline), planned_drain::Verdict::kDrained);
}

// NodeInfo.build 把构建标识与工作负载修订拼在一起:同一个镜像滚动重启时,新旧两代 Pod 的修订不同,选点才分得出哪一代要留下。
TEST(PlannedDrainBuildLabel, ComposesBuildAndPodRevision)
{
	EXPECT_EQ(planned_drain::ComposeBuildLabel("a1b2c3", "6d4b75cb6d"), "a1b2c3@6d4b75cb6d");
	// 同镜像、不同修订必须是两个不同的值;同镜像、同修订必须相等。
	EXPECT_NE(planned_drain::ComposeBuildLabel("a1b2c3", "6d4b75cb6d"), planned_drain::ComposeBuildLabel("a1b2c3", "7f9c8d5b44"));
	EXPECT_EQ(planned_drain::ComposeBuildLabel("a1b2c3", "6d4b75cb6d"), planned_drain::ComposeBuildLabel("a1b2c3", "6d4b75cb6d"));
}

TEST(PlannedDrainBuildLabel, MissingPartsAreLeftOut)
{
	EXPECT_EQ(planned_drain::ComposeBuildLabel("a1b2c3", ""), "a1b2c3"); // Agones Fleet:没有修订
	EXPECT_EQ(planned_drain::ComposeBuildLabel("", "6d4b75cb6d"), "6d4b75cb6d");
	EXPECT_EQ(planned_drain::ComposeBuildLabel("", ""), ""); // 本机开发:没上报,不参与构建偏好
}

// ── scene_planned_drain_rules.h ──────────────────────────────────────────────────────────────────

TEST(ScenePlannedDrainConfig, SelfEvacuateDefaultsToHalfTheBudgetAndNeverExceedsIt)
{
	EXPECT_EQ(scene_planned_drain::SelfEvacuateAfter(95s, nullptr), 47s);
	EXPECT_EQ(scene_planned_drain::SelfEvacuateAfter(95s, "10"), 10s);
	EXPECT_EQ(scene_planned_drain::SelfEvacuateAfter(95s, "600"), 95s);
	EXPECT_EQ(scene_planned_drain::SelfEvacuateAfter(95s, "0"), 0s) << "0 = 一开始就自己疏散";
	EXPECT_EQ(scene_planned_drain::SelfEvacuateAfter(95s, "ten"), 47s) << "非法值回落到缺省";
}

TEST(ScenePlannedDrainConfig, RelocationRateDefaultsAndZeroMeansUnlimited)
{
	EXPECT_EQ(scene_planned_drain::RelocationsPerSecond(nullptr), scene_planned_drain::kDefaultRelocationsPerSecond);
	EXPECT_EQ(scene_planned_drain::RelocationsPerSecond("50"), 50u);
	EXPECT_EQ(scene_planned_drain::RelocationsPerSecond("0"), 0u);
	EXPECT_EQ(scene_planned_drain::RelocationsPerSecond("-1"), scene_planned_drain::kDefaultRelocationsPerSecond);
}

TEST(ScenePlannedDrainTargets, OpenWorldHostingPeerCounts)
{
	using scene_planned_drain::Peer;
	const std::array<Peer, 3> peers{Peer{true, false, 0}, Peer{false, true, 0}, Peer{false, false, 0}};
	EXPECT_TRUE(scene_planned_drain::HasOpenWorldPeer(peers));
	const std::array<Peer, 2> crossWorld{Peer{true, false, 2}, Peer{false, false, 2}};
	EXPECT_TRUE(scene_planned_drain::HasOpenWorldPeer(crossWorld)) << "跨服主世界同样接得住";
}

TEST(ScenePlannedDrainTargets, NoTargetWhenOnlySelfOrClosedPeers)
{
	using scene_planned_drain::Peer;
	const std::array<Peer, 3> peers{Peer{true, false, 0}, Peer{false, true, 0}, Peer{false, true, 2}};
	EXPECT_FALSE(scene_planned_drain::HasOpenWorldPeer(peers));
	const std::array<Peer, 2> instanceOnly{Peer{true, false, 0}, Peer{false, false, 1}};
	EXPECT_FALSE(scene_planned_drain::HasOpenWorldPeer(instanceOnly)) << "只剩副本节点:疏散落点都在主世界,接不住";
	const std::array<Peer, 0> none{};
	EXPECT_FALSE(scene_planned_drain::HasOpenWorldPeer(none));
}

TEST(ScenePlannedDrainPacer, FirstCallOnlyStartsTheClock)
{
	scene_planned_drain::Pacer pacer(100);
	EXPECT_EQ(pacer.Take(kT0, 1000), 0u);
	EXPECT_EQ(pacer.Take(kT0 + 100ms, 1000), 10u);
	EXPECT_EQ(pacer.Take(kT0 + 200ms, 3), 3u) << "不超过排队人数";
	EXPECT_EQ(pacer.Take(kT0 + 200ms, 1000), 7u) << "上一拍没用完的令牌留着";
}

TEST(ScenePlannedDrainPacer, PacerCapsBurstAtOneSecondOfTokens)
{
	scene_planned_drain::Pacer pacer(100);
	EXPECT_EQ(pacer.Take(kT0, 1000), 0u);
	EXPECT_EQ(pacer.Take(kT0 + 10s, 1000), 100u) << "空闲再久也只攒 1 秒的量";
}

TEST(ScenePlannedDrainPacer, ZeroRateIsUnlimited)
{
	scene_planned_drain::Pacer pacer(0);
	EXPECT_EQ(pacer.Take(kT0, 1234), 1234u);
}

// ── relocate_target.h ────────────────────────────────────────────────────────────────────────────

TEST(RelocateTarget, WorldChannelKeepsChannelAndMap)
{
	const auto target = relocate_target::ForScene(555, 1001, 0, 0);
	EXPECT_EQ(target.sceneId, 555u);
	EXPECT_EQ(target.sceneConfId, 1001u);
}

TEST(RelocateTarget, MirrorGoesBackToItsMap)
{
	const auto target = relocate_target::ForScene(777, 1001, 9, 0);
	EXPECT_EQ(target.sceneId, 0u) << "镜像不迁,不按号找";
	EXPECT_EQ(target.sceneConfId, 1001u);
}

TEST(RelocateTarget, DungeonGoesToDefaultWorld)
{
	const auto target = relocate_target::ForScene(888, 3001, 0, 42);
	EXPECT_EQ(target.sceneId, 0u);
	EXPECT_EQ(target.sceneConfId, 0u);
}

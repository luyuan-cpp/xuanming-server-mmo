// scene 节点按控制面期望状态对账场景归属(缺口清单第 33 条,docs/design/scene-node-ownership-reconcile.md)。
//
// 三层:
//   1. 纯规则(scene_ownership_rules.h):读数分类、连击与宽限期;
//   2. MGET 应答适配(ReadingsOf):断线空应答、错误应答、元素个数不对一律当读不到;
//   3. ECS 级(CollectChecks / ApplyReadings / SceneLifecycle::ReuseExistingScene):判定之后真的走 SceneLifecycle::DestroyScene,
//      有绑定时实体留着、绑定走了才销毁;CreateScene 按号命中已有实体时宽限期从头算,在途的旧读数作废。
// 宿主里没有 Redis、没有 etcd:对账器的定时与发读只测门禁(身份不确认 / 控制域没连上时不核对、不登记任何组件)。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include <hiredis/hiredis.h>

#include "engine/core/metrics/metrics_registry.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/scene/scene.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "thread_context/ecs_context.h"
#include "world/scene_lifecycle.h"
#include "world/scene_ownership_reconciler.h"
#include "world/scene_ownership_rules.h"

namespace so = scene_ownership;

namespace
{
using namespace std::chrono_literals;

constexpr std::string_view kSelf = "7";

so::OwnerReading Value(std::string owner)
{
	so::OwnerReading reading;
	reading.kind = so::OwnerReading::Kind::kValue;
	reading.value = std::move(owner);
	return reading;
}

so::OwnerReading Missing()
{
	so::OwnerReading reading;
	reading.kind = so::OwnerReading::Kind::kMissing;
	return reading;
}

so::OwnerReading Unreadable()
{
	return so::OwnerReading{};
}

std::uint64_t Observed(so::Observation observation)
{
	return so::ReadStats().observations[static_cast<std::size_t>(observation)];
}

std::uint64_t Swept(so::SweepOutcome outcome)
{
	return so::ReadStats().sweeps[static_cast<std::size_t>(outcome)];
}
} // namespace

// ── 1. 纯规则 ─────────────────────────────────────────────────────────────

TEST(SceneOwnershipRules, SceneNodeKeyMatchesTheSceneManagerKey)
{
	// 跨语言契约:go/scene_manager/internal/logic/createscenelogic.go 的 SceneNodeKeyFmt = "scene:%d:node"。
	EXPECT_EQ(so::SceneNodeKey(0), "scene:0:node");
	EXPECT_EQ(so::SceneNodeKey(18446744073709551615ULL), "scene:18446744073709551615:node");
}

TEST(SceneOwnershipRules, ClassifyReadings)
{
	EXPECT_EQ(so::Classify(Value("7"), kSelf), so::Observation::kSelf);
	EXPECT_EQ(so::Classify(Value("8"), kSelf), so::Observation::kOtherNode);
	EXPECT_EQ(so::Classify(Value("70"), kSelf), so::Observation::kOtherNode) << "整串比较,不是前缀";
	EXPECT_EQ(so::Classify(Missing(), kSelf), so::Observation::kNoMapping);
	EXPECT_EQ(so::Classify(Value(""), kSelf), so::Observation::kNoMapping) << "scene_manager 把空串当成没有属主";
	EXPECT_EQ(so::Classify(Unreadable(), kSelf), so::Observation::kUnknown);
	// 不知道自己是谁就什么都判不了。
	EXPECT_EQ(so::Classify(Value("7"), ""), so::Observation::kUnknown);
	EXPECT_EQ(so::Classify(Missing(), ""), so::Observation::kUnknown);
}

TEST(SceneOwnershipRules, ObservationNamesCoverEveryValue)
{
	EXPECT_EQ(so::ObservationName(so::Observation::kSelf), "self");
	EXPECT_EQ(so::ObservationName(so::Observation::kOtherNode), "other_node");
	EXPECT_EQ(so::ObservationName(so::Observation::kNoMapping), "no_mapping");
	EXPECT_EQ(so::ObservationName(so::Observation::kUnknown), "unknown");
	EXPECT_EQ(so::ObservationName(so::Observation::kCount), "invalid");
}

TEST(SceneOwnershipRules, ReleasesOnlyAfterTwoConsecutiveNotMineReadingsPastTheGrace)
{
	const auto t0 = so::Clock::now();
	so::CheckState state;
	state.graceStartedAt = t0;

	// 宽限期内,连续两次不归本节点也不动手。
	EXPECT_EQ(so::Advance(state, so::Observation::kNoMapping, t0), so::Decision::kKeep);
	EXPECT_EQ(state.notMineStreak, 1);
	EXPECT_EQ(so::Advance(state, so::Observation::kOtherNode, t0 + so::kSweepInterval), so::Decision::kKeep);
	EXPECT_EQ(state.notMineStreak, 2);
	EXPECT_EQ(so::Advance(state, so::Observation::kOtherNode, t0 + so::kCreateGrace - 1ms), so::Decision::kKeep)
		<< "宽限期差 1ms 也不动手";
	// 发读时刻满宽限期:证据够了。
	EXPECT_EQ(so::Advance(state, so::Observation::kOtherNode, t0 + so::kCreateGrace), so::Decision::kRelease);
	// 之后仍不归本节点就一直判释放(销毁幂等,住户走完之前实体留着);连击数封顶不溢出。
	for (int i = 0; i < 300; ++i)
	{
		EXPECT_EQ(so::Advance(state, so::Observation::kNoMapping, t0 + so::kCreateGrace + std::chrono::seconds(i)),
				  so::Decision::kRelease);
	}
	EXPECT_EQ(state.notMineStreak, so::kNotMineConfirmations);
}

TEST(SceneOwnershipRules, OneNotMineReadingPastTheGraceIsNotEnough)
{
	const auto t0 = so::Clock::now();
	so::CheckState state;
	state.graceStartedAt = t0 - std::chrono::hours(1);
	EXPECT_EQ(so::Advance(state, so::Observation::kNoMapping, t0), so::Decision::kKeep);
	EXPECT_EQ(so::Advance(state, so::Observation::kNoMapping, t0 + so::kSweepInterval), so::Decision::kRelease);
}

TEST(SceneOwnershipRules, MineResetsTheStreak)
{
	const auto t0 = so::Clock::now();
	so::CheckState state;
	state.graceStartedAt = t0 - std::chrono::hours(1);
	EXPECT_EQ(so::Advance(state, so::Observation::kOtherNode, t0), so::Decision::kKeep);
	EXPECT_EQ(so::Advance(state, so::Observation::kSelf, t0 + so::kSweepInterval), so::Decision::kKeep);
	EXPECT_EQ(state.notMineStreak, 0);
	EXPECT_EQ(so::Advance(state, so::Observation::kOtherNode, t0 + 2 * so::kSweepInterval), so::Decision::kKeep)
		<< "中间读到一次归本节点,要重新攒够两次";
	EXPECT_EQ(so::Advance(state, so::Observation::kOtherNode, t0 + 3 * so::kSweepInterval), so::Decision::kRelease);
}

TEST(SceneOwnershipRules, UnreadableNeitherCountsNorClearsTheEvidence)
{
	const auto t0 = so::Clock::now();
	so::CheckState state;
	state.graceStartedAt = t0 - std::chrono::hours(1);
	// 读不到永远不动手。
	for (int i = 0; i < 10; ++i)
	{
		EXPECT_EQ(so::Advance(state, so::Observation::kUnknown, t0 + std::chrono::seconds(i)), so::Decision::kKeep);
	}
	EXPECT_EQ(state.notMineStreak, 0);
	// 也不推翻已有的证据:不归、读不到、不归 = 两次不归。
	EXPECT_EQ(so::Advance(state, so::Observation::kNoMapping, t0), so::Decision::kKeep);
	EXPECT_EQ(so::Advance(state, so::Observation::kUnknown, t0 + so::kSweepInterval), so::Decision::kKeep);
	EXPECT_EQ(state.notMineStreak, 1);
	EXPECT_EQ(so::Advance(state, so::Observation::kNoMapping, t0 + 2 * so::kSweepInterval), so::Decision::kRelease);
}

// ── 2. MGET 应答适配 ───────────────────────────────────────────────────────

TEST(SceneOwnershipReply, ElementsMapToReadingsInOrder)
{
	std::string owner = "7";
	redisReply value{};
	value.type = REDIS_REPLY_STRING;
	value.str = owner.data();
	value.len = owner.size();
	redisReply nil{};
	nil.type = REDIS_REPLY_NIL;
	redisReply integer{};
	integer.type = REDIS_REPLY_INTEGER;
	integer.integer = 7;
	redisReply *elements[] = {&value, &nil, &integer, nullptr};
	redisReply array{};
	array.type = REDIS_REPLY_ARRAY;
	array.elements = 4;
	array.element = elements;

	const auto readings = so::ReadingsOf(&array, 4);
	ASSERT_EQ(readings.size(), 4u);
	EXPECT_EQ(readings[0].kind, so::OwnerReading::Kind::kValue);
	EXPECT_EQ(readings[0].value, "7");
	EXPECT_EQ(readings[1].kind, so::OwnerReading::Kind::kMissing);
	EXPECT_EQ(readings[2].kind, so::OwnerReading::Kind::kUnreadable) << "整数不是属主串";
	EXPECT_EQ(readings[3].kind, so::OwnerReading::Kind::kUnreadable);
}

TEST(SceneOwnershipReply, BrokenRepliesAreUnreadableAsAWhole)
{
	const auto allUnreadable = [](const std::vector<so::OwnerReading> &readings, std::size_t expected)
	{
		ASSERT_EQ(readings.size(), expected);
		for (const auto &reading : readings)
		{
			EXPECT_EQ(reading.kind, so::OwnerReading::Kind::kUnreadable);
		}
	};
	// 断线:hiredis 对在途命令回空应答。
	allUnreadable(so::ReadingsOf(nullptr, 3), 3);

	std::string message = "LOADING Redis is loading the dataset in memory";
	redisReply error{};
	error.type = REDIS_REPLY_ERROR;
	error.str = message.data();
	error.len = message.size();
	allUnreadable(so::ReadingsOf(&error, 2), 2);

	redisReply nil{};
	nil.type = REDIS_REPLY_NIL;
	redisReply *elements[] = {&nil};
	redisReply shortArray{};
	shortArray.type = REDIS_REPLY_ARRAY;
	shortArray.elements = 1;
	shortArray.element = elements;
	allUnreadable(so::ReadingsOf(&shortArray, 2), 2); // 元素个数对不上:不能按位置猜
}

// ── 3. ECS 级 ─────────────────────────────────────────────────────────────

namespace
{
class SceneOwnershipEcs : public ::testing::Test
{
protected:
	void SetUp() override
	{
		tlsEcs.Clear();
		so::ResetStatsForTest();
	}
	void TearDown() override
	{
		// 用例建的场景都经 SceneLifecycle::CreateScene 记了 Agones 单元账:先拆掉绑定,再逐个销毁让账平。
		tlsEcs.actorRegistry.clear();
		std::vector<SceneEntity> scenes;
		for (const auto scene : tlsEcs.sceneRegistry.view<SceneInfoComp>())
		{
			scenes.push_back(scene);
		}
		for (const auto scene : scenes)
		{
			static_cast<void>(SceneLifecycle::DestroyScene(scene));
		}
		tlsEcs.Clear();
	}

	static SceneEntity MakeScene(std::uint64_t sceneId)
	{
		CreateSceneRequest request;
		request.set_config_id(1);
		request.set_scene_id(sceneId);
		static_cast<void>(SceneLifecycle::CreateScene(request));
		for (const auto &[scene, info] : tlsEcs.sceneRegistry.view<SceneInfoComp>().each())
		{
			if (info.scene_id() == sceneId)
			{
				return scene;
			}
		}
		return entt::null;
	}

	// 不带 PlayerGuidComp 的绑定:排空不会给它发改派票据,但它仍是一条场景绑定,DestroyScene 要等它走。
	static entt::entity BindActor(SceneEntity scene)
	{
		const auto actor = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<SceneEntityComp>(actor).sceneEntity = scene;
		return actor;
	}

	static const SceneOwnershipCheckComp *Check(SceneEntity scene)
	{
		return tlsEcs.sceneRegistry.valid(scene) ? tlsEcs.sceneRegistry.try_get<SceneOwnershipCheckComp>(scene)
												 : nullptr;
	}

	static std::vector<so::OwnerReading> Repeat(const so::OwnerReading &reading, std::size_t count)
	{
		return std::vector<so::OwnerReading>(count, reading);
	}
};
} // namespace

TEST_F(SceneOwnershipEcs, CollectChecksRegistersEachSceneOnceWithTheGraceStartingNow)
{
	const auto a = MakeScene(91001);
	const auto b = MakeScene(91002);
	const auto t0 = so::Clock::now();

	const auto first = so::CollectChecks(t0);
	ASSERT_EQ(first.size(), 2u);
	ASSERT_NE(Check(a), nullptr);
	ASSERT_NE(Check(b), nullptr);
	EXPECT_EQ(Check(a)->state.graceStartedAt, t0);
	EXPECT_NE(Check(a)->trackingId, Check(b)->trackingId);
	EXPECT_NE(Check(a)->trackingId, 0u);

	// 再收一轮:已登记的不重登,宽限起点与核对状态号都不变。
	const auto second = so::CollectChecks(t0 + so::kSweepInterval);
	ASSERT_EQ(second.size(), 2u);
	EXPECT_EQ(Check(a)->state.graceStartedAt, t0);
	for (std::size_t i = 0; i < first.size(); ++i)
	{
		EXPECT_EQ(first[i].trackingId, second[i].trackingId);
		EXPECT_EQ(first[i].sceneId, second[i].sceneId);
	}
}

TEST_F(SceneOwnershipEcs, SceneStillMappedToThisNodeIsKept)
{
	const auto scene = MakeScene(91011);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0);
	for (int round = 0; round < 10; ++round)
	{
		const auto outcome = so::ApplyReadings(checks, Repeat(Value(std::string(kSelf)), checks.size()), kSelf,
											   t0 + so::kCreateGrace + round * so::kSweepInterval);
		EXPECT_EQ(outcome.releaseStarted, 0u);
		EXPECT_EQ(outcome.destroyed, 0u);
	}
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(Check(scene)->state.notMineStreak, 0);
	EXPECT_EQ(Observed(so::Observation::kSelf), 10u);
}

TEST_F(SceneOwnershipEcs, UnmappedSceneIsDestroyedOnlyAfterTheGrace)
{
	const auto scene = MakeScene(91021);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0);

	// 第 33 条之外的另一种孤儿:刚建好就没有映射(比如回滚的 DestroyScene 早于在途的 CreateScene 到达)。
	// 宽限期内连续多次读到没有映射也不动。
	for (auto at = t0; at < t0 + so::kCreateGrace; at += so::kSweepInterval)
	{
		const auto outcome = so::ApplyReadings(checks, Repeat(Missing(), checks.size()), kSelf, at);
		EXPECT_EQ(outcome.destroyed, 0u);
	}
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));

	const auto outcome = so::ApplyReadings(checks, Repeat(Missing(), checks.size()), kSelf, t0 + so::kCreateGrace);
	EXPECT_EQ(outcome.releaseStarted, 1u);
	EXPECT_EQ(outcome.destroyed, 1u);
	EXPECT_FALSE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(so::ReadStats().releaseStarted, 1u);
	EXPECT_EQ(so::ReadStats().destroyed, 1u);
}

TEST_F(SceneOwnershipEcs, SceneMovedToAnotherNodeWhileThisNodeWasAwayIsReleased)
{
	// 第 33 条的主情形:实体早就建好(宽限期早已过去),节点回来后读到映射指向别的节点。
	const auto moved = MakeScene(91031);
	const auto kept = MakeScene(91032);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));
	ASSERT_EQ(checks.size(), 2u);
	std::vector<so::OwnerReading> readings;
	for (const auto &check : checks)
	{
		readings.push_back(check.scene == moved ? Value("12") : Value(std::string(kSelf)));
	}

	EXPECT_EQ(so::ApplyReadings(checks, readings, kSelf, t0).destroyed, 0u) << "一次不够";
	const auto outcome = so::ApplyReadings(checks, readings, kSelf, t0 + so::kSweepInterval);
	EXPECT_EQ(outcome.releaseStarted, 1u);
	EXPECT_EQ(outcome.destroyed, 1u);
	EXPECT_FALSE(tlsEcs.sceneRegistry.valid(moved));
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(kept));
	EXPECT_EQ(Check(kept)->state.notMineStreak, 0);
	EXPECT_EQ(Observed(so::Observation::kOtherNode), 2u);
	EXPECT_EQ(Observed(so::Observation::kSelf), 2u);
}

TEST_F(SceneOwnershipEcs, SceneWithBindingsIsKeptUntilTheyLeave)
{
	const auto scene = MakeScene(91041);
	const auto actor = BindActor(scene);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));

	EXPECT_EQ(so::ApplyReadings(checks, Repeat(Missing(), 1), kSelf, t0).releaseStarted, 0u);
	const auto first = so::ApplyReadings(checks, Repeat(Missing(), 1), kSelf, t0 + so::kSweepInterval);
	EXPECT_EQ(first.releaseStarted, 1u);
	EXPECT_EQ(first.destroyed, 0u) << "还有绑定,实体留着";
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_TRUE(Check(scene)->releaseStarted);

	// 下一轮仍不归本节点:再排空一次,不重复计「开始排空」。
	const auto again = so::ApplyReadings(checks, Repeat(Missing(), 1), kSelf, t0 + 2 * so::kSweepInterval);
	EXPECT_EQ(again.releaseStarted, 0u);
	EXPECT_EQ(again.destroyed, 0u);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));

	// 绑定走了,下一轮销毁。
	tlsEcs.actorRegistry.remove<SceneEntityComp>(actor);
	const auto last = so::ApplyReadings(checks, Repeat(Missing(), 1), kSelf, t0 + 3 * so::kSweepInterval);
	EXPECT_EQ(last.destroyed, 1u);
	EXPECT_FALSE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(so::ReadStats().releaseStarted, 1u);
	EXPECT_EQ(so::ReadStats().destroyed, 1u);
}

TEST_F(SceneOwnershipEcs, ReleasingSceneThatTurnsBackToThisNodeStops)
{
	const auto scene = MakeScene(91051);
	const auto actor = BindActor(scene);
	static_cast<void>(actor);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));
	static_cast<void>(so::ApplyReadings(checks, Repeat(Missing(), 1), kSelf, t0));
	ASSERT_EQ(so::ApplyReadings(checks, Repeat(Missing(), 1), kSelf, t0 + so::kSweepInterval).releaseStarted, 1u);

	// 映射回到本节点(比如 scene_manager 又把它迁了回来):连击清零、不再算「排空中」,之后读到一次不归本节点也不再动手。
	static_cast<void>(so::ApplyReadings(checks, Repeat(Value(std::string(kSelf)), 1), kSelf, t0 + 2 * so::kSweepInterval));
	EXPECT_EQ(Check(scene)->state.notMineStreak, 0);
	EXPECT_FALSE(Check(scene)->releaseStarted);
	EXPECT_EQ(so::ApplyReadings(checks, Repeat(Missing(), 1), kSelf, t0 + 3 * so::kSweepInterval).releaseStarted, 0u);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	// 再连着两次不归本节点:重新开始排空,重新计一次。
	EXPECT_EQ(so::ApplyReadings(checks, Repeat(Missing(), 1), kSelf, t0 + 4 * so::kSweepInterval).releaseStarted, 1u);
	EXPECT_EQ(so::ReadStats().releaseStarted, 2u);
}

TEST_F(SceneOwnershipEcs, ReusingTheSceneRestartsTheGraceAndVoidsReadingsInFlight)
{
	const auto scene = MakeScene(91061);
	const auto t0 = so::Clock::now();
	const auto stale = so::CollectChecks(t0 - std::chrono::hours(1));
	static_cast<void>(so::ApplyReadings(stale, Repeat(Missing(), 1), kSelf, t0));
	ASSERT_EQ(Check(scene)->state.notMineStreak, 1);

	// scene_manager「先建后改」迁回本节点:CreateScene 按号命中这份实体,映射还没 CAS 过来。
	const SceneInfoComp &info = SceneLifecycle::ReuseExistingScene(scene);
	EXPECT_EQ(info.scene_id(), 91061u);
	EXPECT_EQ(Check(scene), nullptr) << "核对状态整个摘掉";

	// 命中之前发出的读,应答在命中之后才到:作废,不重建核对状态,实体不动。
	const auto voided = so::ApplyReadings(stale, Repeat(Missing(), 1), kSelf, t0 + so::kSweepInterval);
	EXPECT_EQ(voided.releaseStarted, 0u);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(Check(scene), nullptr);

	// 下一轮重新登记:宽限期从这一轮算起。
	const auto t1 = t0 + 2 * so::kSweepInterval;
	const auto fresh = so::CollectChecks(t1);
	ASSERT_EQ(fresh.size(), 1u);
	EXPECT_NE(fresh[0].trackingId, stale[0].trackingId);
	EXPECT_EQ(Check(scene)->state.graceStartedAt, t1);

	// 旧批次的读数再晚到也作废(核对状态号对不上)。
	static_cast<void>(so::ApplyReadings(stale, Repeat(Missing(), 1), kSelf, t1 + so::kCreateGrace));
	EXPECT_EQ(Check(scene)->state.notMineStreak, 0);

	// 新批次:宽限期内不动手,满了才动手。
	static_cast<void>(so::ApplyReadings(fresh, Repeat(Missing(), 1), kSelf, t1));
	EXPECT_EQ(so::ApplyReadings(fresh, Repeat(Missing(), 1), kSelf, t1 + so::kSweepInterval).destroyed, 0u);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(so::ApplyReadings(fresh, Repeat(Missing(), 1), kSelf, t1 + so::kCreateGrace).destroyed, 1u);
	EXPECT_FALSE(tlsEcs.sceneRegistry.valid(scene));
}

TEST_F(SceneOwnershipEcs, ReadingForAnEntityDestroyedInFlightIsDiscarded)
{
	const auto scene = MakeScene(91071);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));
	// scene_manager 的 DestroyScene 先到,实体已销毁;同号的新实体(另一次 CreateScene)不受旧读数影响。
	ASSERT_EQ(SceneLifecycle::DestroyScene(scene), 0u);
	const auto recreated = MakeScene(91071);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(recreated));

	for (int round = 0; round < 3; ++round)
	{
		const auto outcome =
			so::ApplyReadings(checks, Repeat(Missing(), 1), kSelf, t0 + so::kCreateGrace + round * so::kSweepInterval);
		EXPECT_EQ(outcome.releaseStarted, 0u);
	}
	EXPECT_TRUE(tlsEcs.sceneRegistry.valid(recreated));
	EXPECT_EQ(Check(recreated), nullptr);
}

TEST_F(SceneOwnershipEcs, MalformedBatchOrUnknownSelfNeverReleases)
{
	const auto scene = MakeScene(91081);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));

	// 读数条数与场景数对不上:整批作废,计为读不到。
	for (int round = 0; round < 3; ++round)
	{
		EXPECT_EQ(so::ApplyReadings(checks, {}, kSelf, t0 + round * so::kSweepInterval).releaseStarted, 0u);
	}
	EXPECT_EQ(Observed(so::Observation::kUnknown), 3u);
	// 不知道自己的节点号。
	for (int round = 0; round < 3; ++round)
	{
		EXPECT_EQ(so::ApplyReadings(checks, Repeat(Missing(), 1), "", t0 + round * so::kSweepInterval).releaseStarted,
				  0u);
	}
	// 断线空应答。
	for (int round = 0; round < 3; ++round)
	{
		EXPECT_EQ(so::ApplyReadings(checks, so::ReadingsOf(nullptr, 1), kSelf, t0 + round * so::kSweepInterval)
					  .releaseStarted,
				  0u);
	}
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(Check(scene)->state.notMineStreak, 0);
}

TEST_F(SceneOwnershipEcs, ReconcilerDoesNotSweepWithoutAConfirmedIdentityOrControlRedis)
{
	const auto scene = MakeScene(91091);
	SceneOwnershipReconciler reconciler;
	EXPECT_EQ(reconciler.SweepOnce(), so::SweepOutcome::kIdentityUnconfirmed) << "没 Start";
	EXPECT_EQ(Swept(so::SweepOutcome::kIdentityUnconfirmed), 0u) << "没 Start 不计数";

	reconciler.Start([] { return false; });
	EXPECT_EQ(reconciler.SweepOnce(), so::SweepOutcome::kIdentityUnconfirmed);
	EXPECT_EQ(Swept(so::SweepOutcome::kIdentityUnconfirmed), 1u);
	EXPECT_EQ(Check(scene), nullptr) << "不核对就不登记核对状态";

	reconciler.Start({}); // 空探针 = 永远不确认(fail-closed)
	EXPECT_EQ(reconciler.SweepOnce(), so::SweepOutcome::kIdentityUnconfirmed);

	// 单测宿主没有连控制域 Redis。
	reconciler.Start([] { return true; });
	EXPECT_EQ(reconciler.SweepOnce(), so::SweepOutcome::kRedisUnavailable);
	EXPECT_EQ(Swept(so::SweepOutcome::kRedisUnavailable), 1u);
	EXPECT_EQ(Check(scene), nullptr);

	reconciler.Stop();
	reconciler.Stop(); // 幂等
	EXPECT_EQ(reconciler.SweepOnce(), so::SweepOutcome::kIdentityUnconfirmed);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
}

TEST_F(SceneOwnershipEcs, MetricsExposeTheCountersAndTheReleasingGauge)
{
	const auto scene = MakeScene(91101);
	BindActor(scene);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));
	static_cast<void>(so::ApplyReadings(checks, Repeat(Value("3"), 1), kSelf, t0));
	static_cast<void>(so::ApplyReadings(checks, Repeat(Value("3"), 1), kSelf, t0 + so::kSweepInterval));

	RegisterSceneOwnershipMetrics(); // 重复登记被注册表按名字拒绝,只打一行 ERROR
	const std::string text = GlobalMetricsRegistry().Render();
	EXPECT_NE(text.find("scene_ownership_sweeps_total{outcome=\"identity_unconfirmed\"}"), std::string::npos);
	EXPECT_NE(text.find("scene_ownership_observations_total{result=\"other_node\"} 2\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_releases_total{stage=\"drain_started\"} 1\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_releases_total{stage=\"destroyed\"} 0\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_releasing_scenes 1\n"), std::string::npos) << text;
}

// scene 节点按控制面期望状态对账场景归属(缺口清单第 33 条,docs/design/scene-node-ownership-reconcile.md)。
//
// 三层:
//   1. 纯规则(scene_ownership_rules.h):读数分类、连击与宽限期、节点号持有者判定、住户预算;
//   2. MGET 应答适配(ReadBatch):第一个元素是持有者,断线空应答、错误应答、元素个数不对一律当读不到;
//   3. ECS 级(CollectChecks / ApplyReadings / SceneLifecycle::ReuseExistingScene):判定之后真的走 SceneLifecycle::DestroyScene,
//      有绑定时实体留着、绑定走了才销毁;CreateScene 按号命中已有实体时宽限期从头算,在途的旧读数作废;
//      节点号被接走时映射是本节点号的场景也排空;开始排空按住户预算限速。
// 宿主里没有 Redis、没有 etcd:对账器的定时与发读只测门禁(没 Start、控制域没连上时不核对、不登记任何组件;
// 核对本身不看 etcd 身份)。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include <hiredis/hiredis.h>

#include "engine/core/metrics/metrics_registry.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/scene/comp/scene_comp.h"
#include "player/system/player_exit_intent.h"
#include "proto/common/component/player_comp.pb.h"
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
constexpr std::string_view kSelfUuid = "uuid-self";

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

TEST(SceneOwnershipRules, KeysMatchTheSceneManagerKeys)
{
	// 跨语言契约:go/scene_manager/internal/logic/createscenelogic.go 的 sceneNodeKey、node_holder.go 的 nodeHolderKey
	// 渲染出来的键逐字节相同(Go 侧 TestCrossLanguageControlKeyGoldens 钉同样的串)。tag 是 {ctl:<zone>}
	// (docs/design/redis-cluster-all-domains-20261110.md §3)。
	EXPECT_EQ(so::SceneNodeKey(3, 0), "scene:{ctl:3}:0:node");
	EXPECT_EQ(so::SceneNodeKey(3, 18446744073709551615ULL), "scene:{ctl:3}:18446744073709551615:node");
	EXPECT_EQ(so::NodeHolderKey(3, "17"), "node:{ctl:3}:17:holder");
	// 对账的一条 MGET 带持有者与本节点的场景属主:同一个 zone 的键必须同槽;不同 zone 是不同的键。
	EXPECT_EQ(redis_slot::HashSlot(so::NodeHolderKey(3, "17")), redis_slot::HashSlot(so::SceneNodeKey(3, 42)));
	EXPECT_NE(so::SceneNodeKey(3, 42), so::SceneNodeKey(4, 42));
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
	EXPECT_EQ(so::ObservationName(so::Observation::kSuperseded), "superseded");
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

TEST(SceneOwnershipRules, SupersededCountsLikeNotMine)
{
	const auto t0 = so::Clock::now();
	so::CheckState state;
	state.graceStartedAt = t0 - std::chrono::hours(1);
	EXPECT_EQ(so::Advance(state, so::Observation::kSuperseded, t0), so::Decision::kKeep);
	EXPECT_EQ(so::Advance(state, so::Observation::kSuperseded, t0 + so::kSweepInterval), so::Decision::kRelease);
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

TEST(SceneOwnershipRules, ParseHolderRecord)
{
	const auto record = so::ParseHolderRecord("100:uuid-a");
	ASSERT_TRUE(record.has_value());
	EXPECT_EQ(record->revision, 100u);
	EXPECT_EQ(record->uuid, "uuid-a");

	const auto max = so::ParseHolderRecord("18446744073709551615:u");
	ASSERT_TRUE(max.has_value());
	EXPECT_EQ(max->revision, 18446744073709551615ULL);

	const auto colonInUuid = so::ParseHolderRecord("5:a:b");
	ASSERT_TRUE(colonInUuid.has_value()) << "按第一个冒号拆";
	EXPECT_EQ(colonInUuid->uuid, "a:b");

	for (const std::string_view broken : {"", ":", "100", "100:", ":uuid", "0:uuid", "-5:uuid", "1x:uuid", "abc:uuid",
										  "18446744073709551616:u"})
	{
		EXPECT_FALSE(so::ParseHolderRecord(broken).has_value()) << "坏记录:" << broken;
	}
}

TEST(SceneOwnershipRules, HolderTrackerNeedsANewerRegistrationOfAnotherProcess)
{
	so::HolderTracker tracker;
	constexpr bool kConfirmed = true;
	constexpr bool kUnconfirmed = false;

	// 还没见过本进程自己的记录:比不出谁更新,按映射判。
	EXPECT_EQ(tracker.Observe(Value("50:uuid-other"), kSelfUuid, kUnconfirmed), so::SlotHolder::kThisProcessOrUnknown);

	// 见到自己的记录,记下修订号。
	EXPECT_EQ(tracker.Observe(Value("100:uuid-self"), kSelfUuid, kUnconfirmed), so::SlotHolder::kThisProcessOrUnknown);
	EXPECT_EQ(tracker.ownRevision, 100u);

	// 别人的、但更旧(本进程登记之前留下的):不算被接走。
	EXPECT_EQ(tracker.Observe(Value("90:uuid-old"), kSelfUuid, kUnconfirmed), so::SlotHolder::kThisProcessOrUnknown);

	// 别人的、更新,但本进程身份确认有效:本进程此刻持有分配键,那条只能是旧值。
	EXPECT_EQ(tracker.Observe(Value("150:uuid-other"), kSelfUuid, kConfirmed), so::SlotHolder::kThisProcessOrUnknown);

	// 别人的、更新,本进程身份不确认:号已被接走。
	EXPECT_EQ(tracker.Observe(Value("150:uuid-other"), kSelfUuid, kUnconfirmed), so::SlotHolder::kAnotherProcess);

	// 本进程后来又登记成功(修订号更大):之后那条旧的别人记录不再算。
	EXPECT_EQ(tracker.Observe(Value("200:uuid-self"), kSelfUuid, kConfirmed), so::SlotHolder::kThisProcessOrUnknown);
	EXPECT_EQ(tracker.ownRevision, 200u);
	EXPECT_EQ(tracker.Observe(Value("150:uuid-other"), kSelfUuid, kUnconfirmed), so::SlotHolder::kThisProcessOrUnknown);

	// etcd 换成新集群、修订号从头计,scene_manager 把本进程的记录改写成新集群里的登记:跟着用新修订号比,
	// 新集群里接走这个号的进程(修订号比它大)照样认得出。
	EXPECT_EQ(tracker.Observe(Value("30:uuid-self"), kSelfUuid, kConfirmed), so::SlotHolder::kThisProcessOrUnknown);
	EXPECT_EQ(tracker.ownRevision, 30u);
	EXPECT_EQ(tracker.Observe(Value("40:uuid-other"), kSelfUuid, kUnconfirmed), so::SlotHolder::kAnotherProcess);

	// 没有信息:键不存在、读不到、记录损坏、不知道自己的 uuid。
	EXPECT_EQ(tracker.Observe(Missing(), kSelfUuid, kUnconfirmed), so::SlotHolder::kThisProcessOrUnknown);
	EXPECT_EQ(tracker.Observe(Unreadable(), kSelfUuid, kUnconfirmed), so::SlotHolder::kThisProcessOrUnknown);
	EXPECT_EQ(tracker.Observe(Value("garbage"), kSelfUuid, kUnconfirmed), so::SlotHolder::kThisProcessOrUnknown);
	EXPECT_EQ(tracker.Observe(Value("999:uuid-other"), "", kUnconfirmed), so::SlotHolder::kThisProcessOrUnknown);
}

TEST(SceneOwnershipRules, ReleaseBudgetAdmitsEmptyScenesAndTheFirstLargeScene)
{
	so::ReleaseBudget budget;
	ASSERT_EQ(budget.remainingResidents, so::kMaxRelocationsPerSweep);
	EXPECT_TRUE(budget.TryStart(0)) << "没有住户的场景不占预算";
	EXPECT_EQ(budget.remainingResidents, so::kMaxRelocationsPerSweep);
	EXPECT_TRUE(budget.TryStart(so::kMaxRelocationsPerSweep - 50));
	EXPECT_EQ(budget.remainingResidents, 50u);
	EXPECT_FALSE(budget.TryStart(51)) << "超出剩余预算:留到下一轮";
	EXPECT_EQ(budget.remainingResidents, 50u) << "没放行就不扣";
	EXPECT_TRUE(budget.TryStart(50));
	EXPECT_EQ(budget.remainingResidents, 0u);
	EXPECT_FALSE(budget.TryStart(1));
	EXPECT_TRUE(budget.TryStart(0));

	// 一个场景的住户比整轮预算还多:本轮第一个有住户的场景照样放行,否则永远排不动。
	so::ReleaseBudget fresh;
	EXPECT_TRUE(fresh.TryStart(so::kMaxRelocationsPerSweep * 3));
	EXPECT_EQ(fresh.remainingResidents, 0u);
	EXPECT_FALSE(fresh.TryStart(1));
}

// ── 2. MGET 应答适配 ───────────────────────────────────────────────────────

TEST(SceneOwnershipReply, ElementsMapToReadingsInOrder)
{
	std::string holderValue = "100:uuid-self";
	redisReply holder{};
	holder.type = REDIS_REPLY_STRING;
	holder.str = holderValue.data();
	holder.len = holderValue.size();
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
	redisReply *elements[] = {&holder, &value, &nil, &integer, nullptr};
	redisReply array{};
	array.type = REDIS_REPLY_ARRAY;
	array.elements = 5;
	array.element = elements;

	const auto readings = so::ReadBatch(&array, 4);
	EXPECT_EQ(readings.holder.kind, so::OwnerReading::Kind::kValue);
	EXPECT_EQ(readings.holder.value, "100:uuid-self");
	ASSERT_EQ(readings.owners.size(), 4u);
	EXPECT_EQ(readings.owners[0].kind, so::OwnerReading::Kind::kValue);
	EXPECT_EQ(readings.owners[0].value, "7");
	EXPECT_EQ(readings.owners[1].kind, so::OwnerReading::Kind::kMissing);
	EXPECT_EQ(readings.owners[2].kind, so::OwnerReading::Kind::kUnreadable) << "整数不是属主串";
	EXPECT_EQ(readings.owners[3].kind, so::OwnerReading::Kind::kUnreadable);
}

TEST(SceneOwnershipReply, BrokenRepliesAreUnreadableAsAWhole)
{
	const auto allUnreadable = [](const so::BatchReadings &readings, std::size_t expected)
	{
		EXPECT_EQ(readings.holder.kind, so::OwnerReading::Kind::kUnreadable);
		ASSERT_EQ(readings.owners.size(), expected);
		for (const auto &reading : readings.owners)
		{
			EXPECT_EQ(reading.kind, so::OwnerReading::Kind::kUnreadable);
		}
	};
	// 断线:hiredis 对在途命令回空应答。
	allUnreadable(so::ReadBatch(nullptr, 3), 3);

	std::string message = "LOADING Redis is loading the dataset in memory";
	redisReply error{};
	error.type = REDIS_REPLY_ERROR;
	error.str = message.data();
	error.len = message.size();
	allUnreadable(so::ReadBatch(&error, 2), 2);

	// 元素个数不是 1 + 场景数:不能按位置猜(少了持有者那一个也一样)。
	redisReply nil{};
	nil.type = REDIS_REPLY_NIL;
	redisReply *elements[] = {&nil, &nil};
	redisReply shortArray{};
	shortArray.type = REDIS_REPLY_ARRAY;
	shortArray.elements = 2;
	shortArray.element = elements;
	allUnreadable(so::ReadBatch(&shortArray, 2), 2);
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

	// 不带 PlayerGuidComp 的绑定:排空不会给它发改派票据,也不占住户预算,但它仍是一条场景绑定,DestroyScene 要等它走。
	static entt::entity BindActor(SceneEntity scene)
	{
		const auto actor = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<SceneEntityComp>(actor).sceneEntity = scene;
		return actor;
	}

	// 已在退出中的玩家住户:占住户预算;排空只合并退出原因、不再发存盘(与 RelocateConfirmEcs 的 makeResident 同一写法,
	// 不带会话快照,所以不登记改派票据,用例之间不留 thread_local 残留)。
	static entt::entity BindExitingResident(SceneEntity scene, Guid playerId)
	{
		const auto player = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, playerId);
		tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
		tlsEcs.actorRegistry.emplace<PlayerExitIntentComp>(player).cause = ExitCause::kSceneDrain;
		tlsEcs.actorRegistry.emplace<SceneEntityComp>(player).sceneEntity = scene;
		return player;
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

	// 一次调用就是一轮核对:预算每轮从头算。
	static so::ApplyOutcome Apply(const std::vector<so::PendingCheck> &checks, const std::vector<so::OwnerReading> &owners,
								  so::Clock::time_point at,
								  so::SlotHolder slotHolder = so::SlotHolder::kThisProcessOrUnknown)
	{
		so::ReleaseBudget budget;
		return so::ApplyReadings(checks, owners, kSelf, slotHolder, at, budget);
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
		const auto outcome =
			Apply(checks, Repeat(Value(std::string(kSelf)), checks.size()), t0 + so::kCreateGrace + round * so::kSweepInterval);
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
		EXPECT_EQ(Apply(checks, Repeat(Missing(), checks.size()), at).destroyed, 0u);
	}
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));

	const auto outcome = Apply(checks, Repeat(Missing(), checks.size()), t0 + so::kCreateGrace);
	EXPECT_EQ(outcome.releaseStarted, 1u);
	EXPECT_EQ(outcome.destroyed, 1u);
	EXPECT_FALSE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(so::ReadStats().releaseStarted, 1u);
	EXPECT_EQ(so::ReadStats().destroyed, 1u);
}

TEST_F(SceneOwnershipEcs, SceneMovedToAnotherNodeWhileThisNodeWasAwayIsReleased)
{
	// 第 33 条的主情形:实体早就建好(宽限期早已过去),节点被判死后读到映射指向别的节点。
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

	EXPECT_EQ(Apply(checks, readings, t0).destroyed, 0u) << "一次不够";
	const auto outcome = Apply(checks, readings, t0 + so::kSweepInterval);
	EXPECT_EQ(outcome.releaseStarted, 1u);
	EXPECT_EQ(outcome.destroyed, 1u);
	EXPECT_FALSE(tlsEcs.sceneRegistry.valid(moved));
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(kept));
	EXPECT_EQ(Check(kept)->state.notMineStreak, 0);
	EXPECT_EQ(Observed(so::Observation::kOtherNode), 2u);
	EXPECT_EQ(Observed(so::Observation::kSelf), 2u);
}

TEST_F(SceneOwnershipEcs, NodeIdTakenOverReleasesScenesStillMappedToIt)
{
	// 本节点号被新进程接走,频道留在这个号上(映射仍是 "7"):只看映射会一直以为是自己的。
	const auto inherited = MakeScene(91111);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));
	const auto mappedToSelf = Repeat(Value(std::string(kSelf)), checks.size());

	EXPECT_EQ(Apply(checks, mappedToSelf, t0).releaseStarted, 0u) << "映射是本节点号、号也还是本进程的:留着";
	EXPECT_EQ(Apply(checks, mappedToSelf, t0 + so::kSweepInterval, so::SlotHolder::kAnotherProcess).releaseStarted, 0u)
		<< "号被接走的第一次读数:连击 1";
	const auto outcome = Apply(checks, mappedToSelf, t0 + 2 * so::kSweepInterval, so::SlotHolder::kAnotherProcess);
	EXPECT_EQ(outcome.releaseStarted, 1u);
	EXPECT_EQ(outcome.destroyed, 1u);
	EXPECT_FALSE(tlsEcs.sceneRegistry.valid(inherited));
	EXPECT_EQ(Observed(so::Observation::kSuperseded), 2u);
}

TEST_F(SceneOwnershipEcs, SceneWithBindingsIsKeptUntilTheyLeave)
{
	const auto scene = MakeScene(91041);
	const auto actor = BindActor(scene);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));

	EXPECT_EQ(Apply(checks, Repeat(Missing(), 1), t0).releaseStarted, 0u);
	const auto first = Apply(checks, Repeat(Missing(), 1), t0 + so::kSweepInterval);
	EXPECT_EQ(first.releaseStarted, 1u);
	EXPECT_EQ(first.destroyed, 0u) << "还有绑定,实体留着";
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_TRUE(Check(scene)->releaseStarted);

	// 下一轮仍不归本节点:再排空一次,不重复计「开始排空」。
	const auto again = Apply(checks, Repeat(Missing(), 1), t0 + 2 * so::kSweepInterval);
	EXPECT_EQ(again.releaseStarted, 0u);
	EXPECT_EQ(again.destroyed, 0u);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));

	// 绑定走了,下一轮销毁。
	tlsEcs.actorRegistry.remove<SceneEntityComp>(actor);
	const auto last = Apply(checks, Repeat(Missing(), 1), t0 + 3 * so::kSweepInterval);
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
	static_cast<void>(Apply(checks, Repeat(Missing(), 1), t0));
	ASSERT_EQ(Apply(checks, Repeat(Missing(), 1), t0 + so::kSweepInterval).releaseStarted, 1u);

	// 映射回到本节点(比如 scene_manager 又把它迁了回来):连击清零、不再算「排空中」,之后读到一次不归本节点也不再动手。
	static_cast<void>(Apply(checks, Repeat(Value(std::string(kSelf)), 1), t0 + 2 * so::kSweepInterval));
	EXPECT_EQ(Check(scene)->state.notMineStreak, 0);
	EXPECT_FALSE(Check(scene)->releaseStarted);
	EXPECT_EQ(Apply(checks, Repeat(Missing(), 1), t0 + 3 * so::kSweepInterval).releaseStarted, 0u);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	// 再连着两次不归本节点:重新开始排空,重新计一次。
	EXPECT_EQ(Apply(checks, Repeat(Missing(), 1), t0 + 4 * so::kSweepInterval).releaseStarted, 1u);
	EXPECT_EQ(so::ReadStats().releaseStarted, 2u);
}

TEST_F(SceneOwnershipEcs, ReleaseBudgetDefersScenesWithResidentsToLaterSweeps)
{
	// 三个不归本节点的场景:a、b 各有一名住户,empty 没有住户。本轮预算只剩 1 名:a 开始排空,b 留到下一轮,
	// empty 不占预算、照常销毁。
	const auto a = MakeScene(91121);
	const auto b = MakeScene(91122);
	const auto empty = MakeScene(91123);
	BindExitingResident(a, 9112101);
	BindExitingResident(b, 9112201);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));
	const auto unmapped = Repeat(Missing(), checks.size());
	static_cast<void>(Apply(checks, unmapped, t0));

	so::ReleaseBudget tight;
	tight.remainingResidents = 1;
	const auto first = so::ApplyReadings(checks, unmapped, kSelf, so::SlotHolder::kThisProcessOrUnknown,
										 t0 + so::kSweepInterval, tight);
	EXPECT_EQ(first.releaseStarted, 2u) << "先轮到的那个有住户的场景,加上 empty";
	EXPECT_EQ(first.deferred, 1u) << "另一个有住户的场景等下一轮";
	EXPECT_EQ(first.destroyed, 1u) << "empty 没有绑定,当场销毁";
	EXPECT_FALSE(tlsEcs.sceneRegistry.valid(empty));
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(a));
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(b));
	// 谁先轮到取决于视图的遍历顺序,不在断言里写死。
	const SceneEntity started = Check(a)->releaseStarted ? a : b;
	const SceneEntity waiting = started == a ? b : a;
	EXPECT_TRUE(Check(started)->releaseStarted);
	EXPECT_FALSE(Check(started)->releaseDeferred);
	EXPECT_FALSE(Check(waiting)->releaseStarted);
	EXPECT_TRUE(Check(waiting)->releaseDeferred);

	// 下一轮新预算:等着的那个开始排空;已在排空的照常重调、不再计「开始」。
	const auto second = Apply(checks, unmapped, t0 + 2 * so::kSweepInterval);
	EXPECT_EQ(second.releaseStarted, 1u);
	EXPECT_EQ(second.deferred, 0u);
	EXPECT_TRUE(Check(waiting)->releaseStarted);
	EXPECT_FALSE(Check(waiting)->releaseDeferred);
	EXPECT_EQ(so::ReadStats().releaseStarted, 3u);
}

TEST_F(SceneOwnershipEcs, DeferredSceneMappedBackToThisNodeIsNoLongerWaiting)
{
	const auto a = MakeScene(91131);
	const auto b = MakeScene(91132);
	BindExitingResident(a, 9113101);
	BindExitingResident(b, 9113201);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));
	const auto unmapped = Repeat(Missing(), checks.size());
	static_cast<void>(Apply(checks, unmapped, t0));
	so::ReleaseBudget tight;
	tight.remainingResidents = 1;
	ASSERT_EQ(so::ApplyReadings(checks, unmapped, kSelf, so::SlotHolder::kThisProcessOrUnknown, t0 + so::kSweepInterval,
								tight)
				  .deferred,
			  1u);
	const SceneEntity deferred = Check(a)->releaseDeferred ? a : b;

	// 等预算的那个场景又被映射回本节点:不再等、不再排空。
	std::vector<so::OwnerReading> readings;
	for (const auto &check : checks)
	{
		readings.push_back(check.scene == deferred ? Value(std::string(kSelf)) : Missing());
	}
	static_cast<void>(Apply(checks, readings, t0 + 2 * so::kSweepInterval));
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(deferred));
	EXPECT_FALSE(Check(deferred)->releaseDeferred);
	EXPECT_FALSE(Check(deferred)->releaseStarted);
}

TEST_F(SceneOwnershipEcs, ReusingTheSceneRestartsTheGraceAndVoidsReadingsInFlight)
{
	const auto scene = MakeScene(91061);
	const auto t0 = so::Clock::now();
	const auto stale = so::CollectChecks(t0 - std::chrono::hours(1));
	static_cast<void>(Apply(stale, Repeat(Missing(), 1), t0));
	ASSERT_EQ(Check(scene)->state.notMineStreak, 1);

	// scene_manager「先建后改」迁回本节点:CreateScene 按号命中这份实体,映射还没 CAS 过来。
	const SceneInfoComp &info = SceneLifecycle::ReuseExistingScene(scene);
	EXPECT_EQ(info.scene_id(), 91061u);
	EXPECT_EQ(Check(scene), nullptr) << "核对状态整个摘掉";

	// 命中之前发出的读,应答在命中之后才到:作废,不重建核对状态,实体不动。
	EXPECT_EQ(Apply(stale, Repeat(Missing(), 1), t0 + so::kSweepInterval).releaseStarted, 0u);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(Check(scene), nullptr);

	// 下一轮重新登记:宽限期从这一轮算起。
	const auto t1 = t0 + 2 * so::kSweepInterval;
	const auto fresh = so::CollectChecks(t1);
	ASSERT_EQ(fresh.size(), 1u);
	EXPECT_NE(fresh[0].trackingId, stale[0].trackingId);
	EXPECT_EQ(Check(scene)->state.graceStartedAt, t1);

	// 旧批次的读数再晚到也作废(核对状态号对不上)。
	static_cast<void>(Apply(stale, Repeat(Missing(), 1), t1 + so::kCreateGrace));
	EXPECT_EQ(Check(scene)->state.notMineStreak, 0);

	// 新批次:宽限期内不动手,满了才动手。
	static_cast<void>(Apply(fresh, Repeat(Missing(), 1), t1));
	EXPECT_EQ(Apply(fresh, Repeat(Missing(), 1), t1 + so::kSweepInterval).destroyed, 0u);
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(Apply(fresh, Repeat(Missing(), 1), t1 + so::kCreateGrace).destroyed, 1u);
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
		EXPECT_EQ(Apply(checks, Repeat(Missing(), 1), t0 + so::kCreateGrace + round * so::kSweepInterval).releaseStarted,
				  0u);
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
		EXPECT_EQ(Apply(checks, {}, t0 + round * so::kSweepInterval).releaseStarted, 0u);
	}
	EXPECT_EQ(Observed(so::Observation::kUnknown), 3u);
	// 不知道自己的节点号。
	for (int round = 0; round < 3; ++round)
	{
		so::ReleaseBudget budget;
		EXPECT_EQ(so::ApplyReadings(checks, Repeat(Missing(), 1), "", so::SlotHolder::kThisProcessOrUnknown,
									t0 + round * so::kSweepInterval, budget)
					  .releaseStarted,
				  0u);
	}
	// 断线空应答。
	for (int round = 0; round < 3; ++round)
	{
		EXPECT_EQ(Apply(checks, so::ReadBatch(nullptr, 1).owners, t0 + round * so::kSweepInterval).releaseStarted, 0u);
	}
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
	EXPECT_EQ(Check(scene)->state.notMineStreak, 0);
}

TEST_F(SceneOwnershipEcs, ReconcilerSweepsWithoutEtcdIdentityButNeedsControlRedis)
{
	const auto scene = MakeScene(91091);
	SceneOwnershipReconciler reconciler;
	EXPECT_FALSE(reconciler.SweepOnce().has_value()) << "没 Start";
	for (const auto value : so::ReadStats().sweeps)
	{
		EXPECT_EQ(value, 0u) << "没 Start 不计数";
	}

	// 身份不确认也核对(与 etcd 失联期间照样收敛);单测宿主没有连控制域 Redis,所以停在这一步。
	reconciler.Start([] { return false; });
	EXPECT_EQ(reconciler.SweepOnce(), so::SweepOutcome::kRedisUnavailable);
	EXPECT_EQ(Swept(so::SweepOutcome::kRedisUnavailable), 1u);
	EXPECT_EQ(Check(scene), nullptr) << "没发读就不登记核对状态";

	reconciler.Start({}); // 空探针:只影响持有者记录的解读,不挡核对
	EXPECT_EQ(reconciler.SweepOnce(), so::SweepOutcome::kRedisUnavailable);

	reconciler.Stop();
	reconciler.Stop(); // 幂等
	EXPECT_FALSE(reconciler.SweepOnce().has_value());
	ASSERT_TRUE(tlsEcs.sceneRegistry.valid(scene));
}

TEST_F(SceneOwnershipEcs, MetricsExposeTheCountersAndGauges)
{
	const auto releasing = MakeScene(91101);
	BindActor(releasing);
	const auto waiting = MakeScene(91102);
	BindExitingResident(waiting, 9110201);
	const auto blocker = MakeScene(91103);
	BindExitingResident(blocker, 9110301);
	const auto t0 = so::Clock::now();
	const auto checks = so::CollectChecks(t0 - std::chrono::hours(1));
	const auto moved = Repeat(Value("3"), checks.size());
	static_cast<void>(Apply(checks, moved, t0));
	so::ReleaseBudget tight;
	tight.remainingResidents = 1;
	const auto outcome =
		so::ApplyReadings(checks, moved, kSelf, so::SlotHolder::kThisProcessOrUnknown, t0 + so::kSweepInterval, tight);
	ASSERT_EQ(outcome.releaseStarted, 2u) << "releasing(没有玩家住户)与 waiting / blocker 中先轮到的那个";
	ASSERT_EQ(outcome.deferred, 1u);

	RegisterSceneOwnershipMetrics(); // 重复登记被注册表按名字拒绝,只打一行 ERROR
	const std::string text = GlobalMetricsRegistry().Render();
	EXPECT_NE(text.find("scene_ownership_sweeps_total{outcome=\"redis_unavailable\"} 0\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_observations_total{result=\"other_node\"} 6\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_observations_total{result=\"superseded\"} 0\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_releases_total{stage=\"drain_started\"} 2\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_releases_total{stage=\"destroyed\"} 0\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_tracked_scenes 3\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_releasing_scenes 2\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_release_deferred_scenes 1\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_superseded 0\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_own_holder_known 0\n"), std::string::npos) << text;
}

// 持有者读数更新两项进程级状态:读到本进程自己的记录之后 own_holder_known 为 1,被更新的另一代接走之后仍是 1;
// 被接走时 superseded 为 1;读不到时两项都不改。托管着场景而 own_holder_known 一直是 0,就是设计稿 §9 第 1 条
// 那种认不出被接走的状态(告警 SceneOwnershipOwnHolderUnknown)。
TEST_F(SceneOwnershipEcs, HolderReadingsTrackTheOwnRecordAndTheTakeover)
{
	so::HolderTracker holder;
	const auto observe = [&holder](const so::OwnerReading &reading)
	{ return so::ObserveHolder(holder, reading, kSelf, kSelfUuid, /*identityConfirmed=*/false); };

	EXPECT_EQ(observe(Missing()), so::SlotHolder::kThisProcessOrUnknown);
	EXPECT_EQ(observe(Value("20:uuid-other")), so::SlotHolder::kThisProcessOrUnknown) << "没见过自己的记录,比不出谁更新";
	EXPECT_FALSE(so::ReadStats().ownHolderKnown);
	EXPECT_FALSE(so::ReadStats().superseded);

	EXPECT_EQ(observe(Value("30:uuid-self")), so::SlotHolder::kThisProcessOrUnknown);
	EXPECT_TRUE(so::ReadStats().ownHolderKnown);
	EXPECT_FALSE(so::ReadStats().superseded);

	EXPECT_EQ(observe(Value("40:uuid-other")), so::SlotHolder::kAnotherProcess);
	EXPECT_TRUE(so::ReadStats().superseded);
	EXPECT_TRUE(so::ReadStats().ownHolderKnown) << "被接走之后仍记着自己见过的那条";

	EXPECT_EQ(observe(Unreadable()), so::SlotHolder::kThisProcessOrUnknown);
	EXPECT_TRUE(so::ReadStats().superseded) << "读不到时不改上一次的判定";
	EXPECT_TRUE(so::ReadStats().ownHolderKnown);

	RegisterSceneOwnershipMetrics(); // 重复登记被注册表按名字拒绝,只打一行 ERROR
	const std::string text = GlobalMetricsRegistry().Render();
	EXPECT_NE(text.find("scene_ownership_own_holder_known 1\n"), std::string::npos) << text;
	EXPECT_NE(text.find("scene_ownership_superseded 1\n"), std::string::npos) << text;
}

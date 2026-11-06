// 世界频道的放置代次栅栏(world/scene_placement_fence.h,docs/design/world-channel-placement-generation.md §4)。
//
// 2026-11-06 要关掉的残余在节点这一半:改派落空后回收预建实体的 DestroyScene 晚到(网络分区恢复后才送达),这期间频道已被
// 合法地改派到同一个节点。修复前节点按 scene_id 拆,拆掉的正是那次合法放置的实体。scene_manager 那一半(同一次放置带同一个
// 代次、回收先抬高水位、之后落地的放置一定比回收新)见 go/scene_manager/internal/logic/world_channel_placement_generation_test.go。
//
// 修复前红的取法(按变异,每次一种):
//   - C1 AdmitDestroy 去掉 `record.newestCreate > generation` 那条(一律放行):LateDestroyOfOlderPlacementIsIgnored、
//     MoveAwayDestroyCannotHitMoveBack、ScenesAreIndependentAndRecordsOnlyGrow 红。
//   - C2 AdmitCreate 不看 destroyedThrough(一律放行):DestroyOfSameGenerationBlocksItsLateCreate、
//     LateCreateOfReclaimedAttemptIsRejected、NeverMovedChannelThenMovedAway 红。
//   - C3 代次 0 的拆场景也按 newestCreate 判过期(去掉 AdmitDestroy 开头的 generation == 0 分支):
//     GenerationZeroKeepsLegacyBehaviour 红(退役拆除不能被代次挡住,否则换过节点的频道排空后实体拆不掉)。

#include <gtest/gtest.h>

#include <cstdint>
#include <thread>

#include "world/scene_lifecycle.h"
#include "world/scene_placement_fence.h"

namespace
{
constexpr std::uint64_t kScene = 9001;
constexpr std::uint64_t kOtherScene = 9002;
} // namespace

// 残余本身:写者 A 按代次 3 预建、改派落空,回收的拆场景卡在路上;写者 B 按代次 5 把频道合法放到本节点(复用同一份实体)。
// A 的拆除晚到:比本节点见过的建场景旧,忽略。B 的 init 补建照常放行。
TEST(ScenePlacementFence, LateDestroyOfOlderPlacementIsIgnored)
{
	ScenePlacementFence fence;
	ASSERT_TRUE(fence.AdmitCreate(kScene, 3));
	ASSERT_TRUE(fence.AdmitCreate(kScene, 5));

	EXPECT_FALSE(fence.AdmitDestroy(kScene, 3)) << "比本节点见过的建场景更旧的拆除必须忽略";
	const auto* record = fence.Find(kScene);
	ASSERT_NE(record, nullptr);
	EXPECT_EQ(record->newestCreate, 5u);
	EXPECT_EQ(record->destroyedThrough, 0u) << "忽略的拆除不记";
	EXPECT_TRUE(fence.AdmitCreate(kScene, 5)) << "属主的补建照常放行";
}

// 同一次尝试:建场景之后的回收照常拆;这次尝试晚到的重发建场景被拒,不复活。更新的放置照常建。
TEST(ScenePlacementFence, DestroyOfSameGenerationBlocksItsLateCreate)
{
	ScenePlacementFence fence;
	ASSERT_TRUE(fence.AdmitCreate(kScene, 4));
	EXPECT_TRUE(fence.AdmitDestroy(kScene, 4)) << "同代的回收拆的就是这次尝试建的实体";

	EXPECT_FALSE(fence.AdmitCreate(kScene, 4)) << "已按同代拆掉,晚到的同代建场景不得复活";
	EXPECT_FALSE(fence.AdmitCreate(kScene, 2)) << "更旧的也不得复活";
	EXPECT_TRUE(fence.AdmitCreate(kScene, 6)) << "更新的放置照常建";
}

// 回收的拆场景先于同一次尝试的建场景被处理(gRPC 建场景要先等 Agones 许可):节点上还没有实体也记下,建场景到了被拒。
TEST(ScenePlacementFence, LateCreateOfReclaimedAttemptIsRejected)
{
	ScenePlacementFence fence;
	EXPECT_TRUE(fence.AdmitDestroy(kScene, 7)) << "节点上还没有实体,拆除也放行并记下";
	const auto* record = fence.Find(kScene);
	ASSERT_NE(record, nullptr);
	EXPECT_EQ(record->destroyedThrough, 7u);

	EXPECT_FALSE(fence.AdmitCreate(kScene, 7));
	EXPECT_FALSE(fence.AdmitCreate(kScene, 0)) << "不带代次的建场景同样不得复活被拆的放置";
}

// 迁走之后迁回:迁走时发给本节点的拆除(代次 6)晚于迁回的建场景(代次 9)到达,忽略;拆除先到就照拆,之后迁回照建,
// 那次拆除重发再到也忽略。
TEST(ScenePlacementFence, MoveAwayDestroyCannotHitMoveBack)
{
	{
		ScenePlacementFence fence;
		ASSERT_TRUE(fence.AdmitCreate(kScene, 2));
		ASSERT_TRUE(fence.AdmitCreate(kScene, 9));
		EXPECT_FALSE(fence.AdmitDestroy(kScene, 6)) << "迁走的拆除晚于迁回的建场景到达";
	}
	{
		ScenePlacementFence fence;
		ASSERT_TRUE(fence.AdmitCreate(kScene, 2));
		EXPECT_TRUE(fence.AdmitDestroy(kScene, 6));
		EXPECT_TRUE(fence.AdmitCreate(kScene, 9)) << "迁回用更新的代次,照常建";
		EXPECT_FALSE(fence.AdmitDestroy(kScene, 6)) << "迁走那次拆除重发再到,忽略";
		EXPECT_EQ(fence.Find(kScene)->newestCreate, 9u);
	}
}

// 代次 0:副本与镜像只用 0,不留记录、行为不变。世界频道的退役拆除(0)不比代次、无条件执行,也不改记录:不留退役墓碑,
// 显式销毁之后被懒改派以新代次复活的频道照样建得回这个节点。
TEST(ScenePlacementFence, GenerationZeroKeepsLegacyBehaviour)
{
	ScenePlacementFence fence;
	EXPECT_TRUE(fence.AdmitCreate(kOtherScene, 0));
	EXPECT_TRUE(fence.AdmitDestroy(kOtherScene, 0));
	EXPECT_TRUE(fence.AdmitCreate(kOtherScene, 0)) << "没有记录的场景,代次 0 一律放行";
	EXPECT_EQ(fence.Size(), 0u) << "副本与镜像不占记录";

	ASSERT_TRUE(fence.AdmitCreate(kScene, 5));
	EXPECT_TRUE(fence.AdmitDestroy(kScene, 0)) << "退役拆除不比代次:换过节点的频道排空后也要拆得掉";
	const auto* record = fence.Find(kScene);
	ASSERT_NE(record, nullptr);
	EXPECT_EQ(record->newestCreate, 5u);
	EXPECT_EQ(record->destroyedThrough, 0u) << "退役拆除不改记录";
	EXPECT_TRUE(fence.AdmitCreate(kScene, 6)) << "以新代次复活的频道照样建";
}

// 从没换过节点的频道按代次 0 建,不留记录;之后按代次迁走,迁走之后晚到的代次 0 补建被拒。
TEST(ScenePlacementFence, NeverMovedChannelThenMovedAway)
{
	ScenePlacementFence fence;
	ASSERT_TRUE(fence.AdmitCreate(kScene, 0));
	EXPECT_EQ(fence.Find(kScene), nullptr);

	EXPECT_TRUE(fence.AdmitDestroy(kScene, 3)) << "迁走的拆除:本节点没按代次建过,放行";
	EXPECT_FALSE(fence.AdmitCreate(kScene, 0)) << "迁走之后晚到的补建(代次 0)不得复活";
}

// 记录按场景分开;两个最大值只升不降。
TEST(ScenePlacementFence, ScenesAreIndependentAndRecordsOnlyGrow)
{
	ScenePlacementFence fence;
	ASSERT_TRUE(fence.AdmitCreate(kScene, 8));
	EXPECT_TRUE(fence.AdmitCreate(kScene, 4)) << "没被拆过时更旧的建场景也放行(复用已有实体)";
	EXPECT_EQ(fence.Find(kScene)->newestCreate, 8u) << "不回退";

	EXPECT_TRUE(fence.AdmitDestroy(kOtherScene, 1));
	EXPECT_FALSE(fence.AdmitDestroy(kScene, 7)) << "别的场景的记录不影响这里";
	EXPECT_TRUE(fence.AdmitDestroy(kScene, 8));
	EXPECT_FALSE(fence.AdmitDestroy(kScene, 3));
	EXPECT_EQ(fence.Find(kScene)->destroyedThrough, 8u) << "不回退";
	EXPECT_EQ(fence.Size(), 2u);
}

// 栅栏按线程持有:同一线程拿到同一个实例,别的线程是另一个实例(只有节点主 EventLoop 线程建、拆场景)。
TEST(ScenePlacementFence, LifecycleHoldsOneFencePerThread)
{
	const ScenePlacementFence* const mine = &SceneLifecycle::PlacementFence();
	EXPECT_EQ(mine, &SceneLifecycle::PlacementFence());

	bool otherThreadHasItsOwn = false;
	std::thread other([mine, &otherThreadHasItsOwn] { otherThreadHasItsOwn = &SceneLifecycle::PlacementFence() != mine; });
	other.join();
	EXPECT_TRUE(otherThreadHasItsOwn);
}

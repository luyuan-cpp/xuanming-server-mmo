// 按 config 全或无扣物的用例(装备强化扣材料 / 宝石镶嵌扣宝石的编排原语,
// 契约见 docs/design/equipment-enhance-inlay.md §4.1)。
//
// 这条路夹在两个容易被"顺手合并"的兄弟之间,所以单独一组用例钉住:
//   * RemoveItemsClamped:按 config 夹紧、恒成功(战斗结算,契约就是"不足按 0");
//   * RemoveItemsByGuid :按 guid 全或无,且**销毁实例**(寄售托管);
//   * RemoveItemsExact  :按 config 全或无扣数量,扣光即成对释放实例和槽位,不整理其余布局。
//
// 有两点与 bag_remove_by_guid_test.cpp 不同,都是有意的:
//
//   1. 铺底一律走 Bag::InsertItemForRestore(预设 guid,不铸号),整组用例不碰 item 号段。
//      那个文件用会铸号的 AddItem 铺底,而 bag_test.cpp 末尾有一组用例把 item 号段置为
//      不可用且不再恢复 —— 排在它后面跑,入包那一步就全部失败(docs/PROGRESS.md 记的
//      「BagRemoveByGuidTest 6 条顺序依赖」)。不铸号就没有这个先后问题。
//
//   2. 流水只数条数。TransactionLogSystem 在单测里没有可读侧(没有 Kafka),读不到
//      tx_type / correlation_id / extra;能数的只有"落了几条" —— 每条流水恰好向 txlog
//      号段要一次 tx_id(做法同 bag_test.cpp 的 GuidSourcePolicyTest)。所以这里钉的是
//      「一条回执一条流水」「一件没扣就一条都不落」。流水里的类型 / 关联键 / extra 是不是
//      传进去的那三个值,这组用例读不到 —— 已知的覆盖上限(与 bag_test.cpp 里
//      EvictionPolicyTest 记的那条同源)。实现里那一处是七个实参写全的调用,三者类型各不
//      相同(uint64 / string / 枚举),顺序写反编译不过,这是眼下唯一的防线。
//
// 夹具自带:SetUp 自己加载 Item 表、自己启用 txlog 号段,TearDown 把 txlog 号段复位回
// 进程基线的"未启用" —— 不依赖别的文件的用例先跑或后跑。

#include <gtest/gtest.h>
#include "engine/core/type_define/player_guid_comp.h"

#include <cstdint>
#include <functional>
#include <map>
#include <string>
#include <utility>

#include "engine/core/type_define/type_define.h"

#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "player/comp/player_frozen_comp.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "table/code/item_table.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

// ── 夹具数据(generated/tables/item.json 的既有行)────────────────────────
constexpr uint32_t kMaterial = 10;      // 可叠加
constexpr uint32_t kOtherMaterial = 11; // 可叠加
constexpr uint32_t kNeverHeld = 15;     // 可叠加;用例从不把它放进包里
constexpr uint32_t kPlainNonStack = 3;  // 不可叠加,不是装备
constexpr uint32_t kEquipConfig = 1;    // 不可叠加,装备(部位 1)

// 预设 guid:铺底不铸号,所以随手取一组互不相同的数即可。
constexpr Guid kGuidA = (uint64_t{1} << 60) + 9301;
constexpr Guid kGuidB = (uint64_t{1} << 60) + 9302;
constexpr Guid kGuidC = (uint64_t{1} << 60) + 9303;
constexpr Guid kGuidD = (uint64_t{1} << 60) + 9304;
constexpr Guid kGuidE = (uint64_t{1} << 60) + 9305;

constexpr Guid kPlayerGuid = 93001;
// 流水的关联键:强化 / 镶嵌传的是被操作那件装备的 item guid(契约 §3.2)。
constexpr uint64_t kTargetEquipGuid = (uint64_t{1} << 60) + 9399;
// extra 的形状照契约 §3.2;容器编排层不解释它,原样交给流水。
const std::string kEnhanceExtra = "{\"from\":3,\"success\":true}";
const std::string kInlayExtra = "{\"socket\":0}";

constexpr uint64_t kTxLogSegmentLo = 700000;
constexpr uint64_t kTxLogSegmentHi = 800000;

// 与 bag_instance_data_test.cpp 的 ArmItemSegment 同一个做法(无 I/O 的假传输 + 直接投
// 一段号),只是种类换成 txlog:进程基线里它是"未启用",流水会在取 tx_id 那一步被静默丢掉。
bool ArmTxLogSegment(uint64_t lo, uint64_t hi)
{
	auto &client = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog);
	client.Reset();
	GuidSegmentClient::Options options;
	options.mixIds = false;
	options.kindName = "txlog";
	options.initialStep = 100000;
	if (!client.Enable(options, [](const std::string &, uint32_t) { return true; },
					   [](GuidSegmentClient::TimerKind, double, std::function<void()>) {},
					   [] { return 0.0; }))
	{
		return false;
	}
	client.Warm();
	client.OnResponse(0, lo, hi);
	return client.IsReady();
}

// 落了几条流水 = TransactionLogSystem 向 txlog 号段要过几次 tx_id(每条流水恰好要一次)。
// issued 与 unavailable 相加:哪怕号段意外没号(那条流水会被 fail-closed 丢掉),
// "要过一次"也数得到,不会把"落了却没发出去"看成"没落"。
uint64_t LogAttempts()
{
	const auto stats = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).GetStats();
	return stats.issued + stats.unavailable;
}

// 背包两层的可观察状态:guid -> (槽位, 整份实例的序列化字节)。
// "一件不扣"就拿调用前后的这份抓拍比 —— 数量、实例数、槽位、实例数据一起比。
using BagState = std::map<Guid, std::pair<uint32_t, std::string>>;

BagState Capture(const Bag &bag)
{
	BagState state;
	bag.ForEachItem([&state, &bag](Guid guid, const ItemComp &item)
					{ state[guid] = {bag.GetItemPosByGuid(guid), item.SerializeAsString()}; });
	return state;
}

void ExpectUnchanged(const BagState &before, const Bag &bag)
{
	EXPECT_EQ(before, Capture(bag)) << "a refused call must leave both layers untouched";
	EXPECT_EQ(before.size(), bag.OccupiedGridCount());
	EXPECT_EQ(before.size(), bag.GridSlotCount());
	EXPECT_TRUE(bag.IsLayerConsistent());
}

class BagRemoveExactTest : public testing::Test
{
protected:
	void SetUp() override
	{
		// 整理(BagService::MergeAndCompact)要查堆叠上限;别的夹具可能在内存里改过表行。
		ItemTableManager::Instance().Load();
		ASSERT_TRUE(ArmTxLogSegment(kTxLogSegmentLo, kTxLogSegmentHi));
		ASSERT_NO_FATAL_FAILURE(RequireFixtureRows());

		player = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kPlayerGuid);
	}

	void TearDown() override
	{
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
		// 进程级状态:txlog 号段回到 bag_test.cpp 的 main() 留下的"未启用"。
		tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Reset();
	}

	// 前置条件:下面的数量都建立在这几行夹具数据上。先断言行指针非空再解引用。
	static void RequireItemRow(uint32_t configId, bool stackable)
	{
		const auto *row = ItemTableManager::Instance().FindByIdSilent(configId).first;
		ASSERT_NE(nullptr, row) << "fixture Item row " << configId << " is missing";
		if (stackable)
		{
			// 用例里同一 config 的单堆最多放 5 个。
			ASSERT_GE(row->max_stack_size(), 5u) << "Item row " << configId;
		}
		else
		{
			ASSERT_EQ(1u, row->max_stack_size()) << "Item row " << configId;
		}
	}

	static void RequireFixtureRows()
	{
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kMaterial, /*stackable=*/true));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kOtherMaterial, /*stackable=*/true));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kNeverHeld, /*stackable=*/true));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kPlainNonStack, /*stackable=*/false));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kEquipConfig, /*stackable=*/false));
	}

	entt::entity player{entt::null};
};

// ── 成功路径:恰好扣掉请求的数量,一条回执一条流水 ──────────────────────────

TEST_F(BagRemoveExactTest, DeductsExactlyTheRequestedAmountAndLogsOncePerDrainedStack)
{
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kMaterial, /*stackSize=*/5, /*pos=*/0);
	bag.InsertItemForRestore(kGuidB, kMaterial, /*stackSize=*/3, /*pos=*/1);
	bag.InsertItemForRestore(kGuidC, kOtherMaterial, /*stackSize=*/4, /*pos=*/2); // 旁观者
	const BagState before = Capture(bag);
	const uint64_t logsBefore = LogAttempts();

	// 6 比任何一堆都多、又不超过两堆之和:无论实例层先抽哪一堆,都恰好横跨两堆。
	const ItemCountMap request{{kMaterial, 6}};
	EXPECT_EQ(kSuccess, BagService::RemoveItemsExact(player, bag, request, TX_EQUIP_ENHANCE,
													 kTargetEquipGuid, kEnhanceExtra));

	EXPECT_EQ(2u, bag.GetTotalItemCount(kMaterial)) << "exactly 6 gone, no more and no less";
	EXPECT_EQ(2u, LogAttempts() - logsBefore)
		<< "one entry per drained instance: a deduction spanning two stacks leaves two";
	EXPECT_EQ(2u, bag.OccupiedGridCount());
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidA));
	EXPECT_EQ(kInvalidU32Id, bag.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(2u, bag.GridSlotCount());
	EXPECT_EQ(nullptr, bag.GetItemCompByPos(0));
	EXPECT_EQ(1u, bag.GetItemPosByGuid(kGuidB));
	EXPECT_EQ(before.at(kGuidC), Capture(bag).at(kGuidC))
		<< "a config that was not asked for is not touched at all";
	EXPECT_TRUE(bag.IsLayerConsistent());
}

// 多种 config 一次扣:可叠加的按堆、不可叠加的非装备材料逐件(每件一个实例、一条回执)。
TEST_F(BagRemoveExactTest, DeductsEveryConfigOfAMultiConfigRequest)
{
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kMaterial, /*stackSize=*/5, /*pos=*/0);
	bag.InsertItemForRestore(kGuidB, kOtherMaterial, /*stackSize=*/4, /*pos=*/1);
	bag.InsertItemForRestore(kGuidC, kPlainNonStack, /*stackSize=*/1, /*pos=*/2);
	bag.InsertItemForRestore(kGuidD, kPlainNonStack, /*stackSize=*/1, /*pos=*/3);
	bag.InsertItemForRestore(kGuidE, kPlainNonStack, /*stackSize=*/1, /*pos=*/4);
	const uint64_t logsBefore = LogAttempts();

	const ItemCountMap request{{kMaterial, 2}, {kOtherMaterial, 4}, {kPlainNonStack, 2}};
	EXPECT_EQ(kSuccess, BagService::RemoveItemsExact(player, bag, request, TX_EQUIP_INLAY,
													 kTargetEquipGuid, kInlayExtra));

	EXPECT_EQ(3u, bag.GetTotalItemCount(kMaterial));
	EXPECT_EQ(0u, bag.GetTotalItemCount(kOtherMaterial));
	EXPECT_EQ(1u, bag.GetTotalItemCount(kPlainNonStack));
	// 1(一堆里扣 2)+ 1(另一种整堆抽光)+ 2(不可叠加的两件各一条)。
	EXPECT_EQ(4u, LogAttempts() - logsBefore);
	EXPECT_EQ(2u, bag.OccupiedGridCount());
	for (const Guid guid : {kGuidB, kGuidC, kGuidD})
	{
		EXPECT_EQ(nullptr, bag.GetItemCompByGuid(guid));
		EXPECT_EQ(kInvalidU32Id, bag.GetItemPosByGuid(guid));
	}
	EXPECT_EQ(0u, bag.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(4u, bag.GetItemPosByGuid(kGuidE));
	EXPECT_EQ(2u, bag.GridSlotCount());
	EXPECT_TRUE(bag.IsLayerConsistent());
}

// ── 全或无 ────────────────────────────────────────────────────────────────

// 任何一种不够就整批拒:一件不扣、一条流水不落。
// ItemCountMap 是 unordered_map,遍历序不确定 —— 没有整批预检的话,"先把够的那种扣了、
// 再在不够的那种上失败"会不会发生取决于哈希序,是个 heisenbug。
TEST_F(BagRemoveExactTest, AnyShortfallRejectsTheWholeBatchWithoutRemovingOrLogging)
{
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kMaterial, /*stackSize=*/5, /*pos=*/0);
	bag.InsertItemForRestore(kGuidB, kMaterial, /*stackSize=*/3, /*pos=*/1);
	bag.InsertItemForRestore(kGuidC, kOtherMaterial, /*stackSize=*/2, /*pos=*/2);
	const BagState before = Capture(bag);
	const uint64_t logsBefore = LogAttempts();

	// 第一种够,第二种差 1。
	const ItemCountMap oneShort{{kMaterial, 4}, {kOtherMaterial, 3}};
	EXPECT_EQ(kBagInsufficientItems,
			  BagService::RemoveItemsExact(player, bag, oneShort, TX_EQUIP_ENHANCE, kTargetEquipGuid,
										   kEnhanceExtra));
	ExpectUnchanged(before, bag);

	// 其中一种一个都没有。
	const ItemCountMap oneAbsent{{kMaterial, 1}, {kNeverHeld, 1}};
	EXPECT_EQ(kBagInsufficientItems,
			  BagService::RemoveItemsExact(player, bag, oneAbsent, TX_EQUIP_ENHANCE, kTargetEquipGuid,
										   kEnhanceExtra));
	ExpectUnchanged(before, bag);

	// 持有量是跨堆求和的:5 + 3 = 8,要 9 仍然差 1。
	const ItemCountMap acrossStacksShort{{kMaterial, 9}};
	EXPECT_EQ(kBagInsufficientItems,
			  BagService::RemoveItemsExact(player, bag, acrossStacksShort, TX_EQUIP_ENHANCE,
										   kTargetEquipGuid, kEnhanceExtra));
	ExpectUnchanged(before, bag);

	EXPECT_EQ(0u, LogAttempts() - logsBefore) << "nothing removed, so nothing may be logged";

	// 对照:恰好等于持有量就是够 —— 上面三次拒绝不是因为"这个包根本扣不动"。
	const ItemCountMap exactlyEverything{{kMaterial, 8}, {kOtherMaterial, 2}};
	EXPECT_EQ(kSuccess, BagService::RemoveItemsExact(player, bag, exactlyEverything, TX_EQUIP_ENHANCE,
													 kTargetEquipGuid, kEnhanceExtra));
	EXPECT_EQ(0u, bag.GetTotalItemCount(kMaterial));
	EXPECT_EQ(0u, bag.GetTotalItemCount(kOtherMaterial));
	EXPECT_EQ(3u, LogAttempts() - logsBefore) << "three instances drained, three entries";
	EXPECT_TRUE(bag.IsLayerConsistent());
}

// ── 参数检查:空表与 0 数量 ────────────────────────────────────────────────

// 空表在纯容器层是"没什么要扣 = 成功"。编排入口不能照单全收:调用方会把一次什么都
// 没扣的调用当成"扣过了"接着往下走(强化照常改等级)。
TEST_F(BagRemoveExactTest, EmptyRequestIsRejected)
{
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kMaterial, /*stackSize=*/5, /*pos=*/0);
	const BagState before = Capture(bag);
	const uint64_t logsBefore = LogAttempts();

	EXPECT_EQ(kInvalidParameter,
			  BagService::RemoveItemsExact(player, bag, ItemCountMap{}, TX_EQUIP_ENHANCE,
										   kTargetEquipGuid, kEnhanceExtra));
	ExpectUnchanged(before, bag);
	EXPECT_EQ(0u, LogAttempts() - logsBefore);
}

// 0 数量的条目,玩家**持有**该物品:纯容器层会放行(预检遍历到同 config 的实例就把这项划掉)。
TEST_F(BagRemoveExactTest, ZeroCountEntryIsRejectedWhenTheItemIsHeld)
{
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kMaterial, /*stackSize=*/5, /*pos=*/0);
	bag.InsertItemForRestore(kGuidB, kOtherMaterial, /*stackSize=*/4, /*pos=*/1);
	const BagState before = Capture(bag);
	const uint64_t logsBefore = LogAttempts();

	const ItemCountMap zeroAlone{{kMaterial, 0}};
	EXPECT_EQ(kInvalidParameter,
			  BagService::RemoveItemsExact(player, bag, zeroAlone, TX_EQUIP_ENHANCE, kTargetEquipGuid,
										   kEnhanceExtra));
	ExpectUnchanged(before, bag);

	// 混在正常条目里:整批拒,正常的那一条也不许被扣。
	const ItemCountMap zeroAmongValid{{kOtherMaterial, 2}, {kMaterial, 0}};
	EXPECT_EQ(kInvalidParameter,
			  BagService::RemoveItemsExact(player, bag, zeroAmongValid, TX_EQUIP_ENHANCE,
										   kTargetEquipGuid, kEnhanceExtra));
	ExpectUnchanged(before, bag);
	EXPECT_EQ(0u, LogAttempts() - logsBefore);
}

// 0 数量的条目,玩家**不持有**该物品:纯容器层会报"不够"(这一项没人来划掉)。
// 同一个请求在两个玩家身上一个成功一个失败 —— 入口必须给出与持有情况无关的同一个答案。
TEST_F(BagRemoveExactTest, ZeroCountEntryIsRejectedWhenTheItemIsNotHeld)
{
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kMaterial, /*stackSize=*/5, /*pos=*/0);
	const BagState before = Capture(bag);
	const uint64_t logsBefore = LogAttempts();

	const ItemCountMap zeroAlone{{kNeverHeld, 0}};
	EXPECT_EQ(kInvalidParameter,
			  BagService::RemoveItemsExact(player, bag, zeroAlone, TX_EQUIP_ENHANCE, kTargetEquipGuid,
										   kEnhanceExtra))
		<< "same answer as when the item is held: a parameter error, not 'insufficient'";
	ExpectUnchanged(before, bag);

	const ItemCountMap zeroAmongValid{{kMaterial, 2}, {kNeverHeld, 0}};
	EXPECT_EQ(kInvalidParameter,
			  BagService::RemoveItemsExact(player, bag, zeroAmongValid, TX_EQUIP_ENHANCE,
										   kTargetEquipGuid, kEnhanceExtra));
	ExpectUnchanged(before, bag);

	// 空包同理。
	Bag empty;
	EXPECT_EQ(kInvalidParameter,
			  BagService::RemoveItemsExact(player, empty, zeroAlone, TX_EQUIP_ENHANCE,
										   kTargetEquipGuid, kEnhanceExtra));
	EXPECT_EQ(0u, empty.OccupiedGridCount());
	EXPECT_EQ(0u, LogAttempts() - logsBefore);
}

// ── 闸:冻结拒,战斗不判 ──────────────────────────────────────────────────

TEST_F(BagRemoveExactTest, FrozenPlayerKeepsEverythingAndNothingIsLogged)
{
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kMaterial, /*stackSize=*/5, /*pos=*/0);
	const BagState before = Capture(bag);
	const uint64_t logsBefore = LogAttempts();
	const ItemCountMap request{{kMaterial, 2}};

	// 归属交接在途:源端扣了数,目标节点从盘上加载到的快照里那笔数量还在。
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(kInvalidParameter,
			  BagService::RemoveItemsExact(player, bag, request, TX_EQUIP_ENHANCE, kTargetEquipGuid,
										   kEnhanceExtra));
	ExpectUnchanged(before, bag);
	EXPECT_EQ(0u, LogAttempts() - logsBefore);

	// 解冻后同一个请求照常成功 —— 上面的拒绝确实来自冻结闸。
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
	EXPECT_EQ(kSuccess, BagService::RemoveItemsExact(player, bag, request, TX_EQUIP_ENHANCE,
													 kTargetEquipGuid, kEnhanceExtra));
	EXPECT_EQ(3u, bag.GetTotalItemCount(kMaterial));
	EXPECT_EQ(1u, LogAttempts() - logsBefore);
}

// D48 红线:"局中挡一切背包写"的闸只许放在玩法入口层(强化 / 镶嵌的写入口自己判)。
// 这条用例防的是有人"顺手"把战斗判断下沉进 BagService —— 那会让战斗结算自己的扣物
// 被自己拦住。
TEST_F(BagRemoveExactTest, InBattleIsNotCheckedAtThisLayer)
{
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kMaterial, /*stackSize=*/5, /*pos=*/0);
	tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(4242);

	const ItemCountMap request{{kMaterial, 2}};
	EXPECT_EQ(kSuccess, BagService::RemoveItemsExact(player, bag, request, TX_EQUIP_ENHANCE,
													 kTargetEquipGuid, kEnhanceExtra));
	EXPECT_EQ(3u, bag.GetTotalItemCount(kMaterial));
}

// ── 扣到 0:立即释放,后续整理幂等且不重复落销毁流水 ──────────────────────

TEST_F(BagRemoveExactTest, DrainedStackReleasesInstanceAndSlotBeforeIdempotentCompaction)
{
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kMaterial, /*stackSize=*/3, /*pos=*/0);
	bag.InsertItemForRestore(kGuidB, kOtherMaterial, /*stackSize=*/4, /*pos=*/1);
	const uint64_t logsBefore = LogAttempts();

	const ItemCountMap wholeStack{{kMaterial, 3}};
	EXPECT_EQ(kSuccess, BagService::RemoveItemsExact(player, bag, wholeStack, TX_EQUIP_ENHANCE,
													 kTargetEquipGuid, kEnhanceExtra));
	EXPECT_EQ(0u, bag.GetTotalItemCount(kMaterial));
	EXPECT_EQ(1u, LogAttempts() - logsBefore);

	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidA));
	EXPECT_EQ(nullptr, bag.GetItemCompByPos(0));
	EXPECT_EQ(kInvalidSlot, bag.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(1u, bag.OccupiedGridCount());
	EXPECT_EQ(1u, bag.GridSlotCount());
	EXPECT_TRUE(bag.IsLayerConsistent());

	// 已扣光的配置不算持有:再扣 1 个不足,不得新增流水。
	const ItemCountMap oneMore{{kMaterial, 1}};
	EXPECT_EQ(kBagInsufficientItems,
			  BagService::RemoveItemsExact(player, bag, oneMore, TX_EQUIP_ENHANCE, kTargetEquipGuid,
										   kEnhanceExtra));
	EXPECT_EQ(1u, LogAttempts() - logsBefore);

	// 扣除已释放实例;随后整理不应再次报告销毁或落流水。
	bool changed = false;
	EXPECT_EQ(kSuccess,
			  BagService::MergeAndCompact(player, bag, CompactPolicy::kMergeOnly, &changed));
	EXPECT_FALSE(changed);
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidA));
	EXPECT_EQ(1u, bag.OccupiedGridCount());
	EXPECT_EQ(1u, bag.GetItemPosByGuid(kGuidB)) << "kMergeOnly never moves a slot";
	EXPECT_EQ(4u, bag.GetTotalItemCount(kOtherMaterial));
	EXPECT_EQ(1u, LogAttempts() - logsBefore);
	EXPECT_TRUE(bag.IsLayerConsistent());
}

// ── 这一层没有"装备不许按 config 扣"的闸 ───────────────────────────────────

// 按 config 抽数量不查表、不看 has_equip():被点名的 config 若是装备,带属性的实例会被
// 扣光并立即释放,不会保留带强化等级与宝石的零数量实例。
//
// 这条用例钉的是"这里没有闸"这个事实,不是说这是好行为 —— 契约把闸放在调用方
// (§5 不变量 9:表校验报、写入口挡,两道都要有)。哪天在这一层加了闸,这条会红,
// 届时请把契约与写入口那两道一起核一遍,别只改这里。
TEST_F(BagRemoveExactTest, EquipInstancesAreDrainedLikeAnyOtherConfig)
{
	Bag bag;
	ItemComp forged;
	forged.set_item_id(kGuidA);
	forged.set_config_id(kEquipConfig);
	forged.set_size(1);
	forged.mutable_equip()->set_enhance_level(7);
	bag.RestoreItems({{forged, 0}});
	const uint64_t logsBefore = LogAttempts();

	const ItemCountMap request{{kEquipConfig, 1}};
	EXPECT_EQ(kSuccess, BagService::RemoveItemsExact(player, bag, request, TX_EQUIP_ENHANCE,
													 kTargetEquipGuid, kEnhanceExtra));
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidA));
	EXPECT_EQ(nullptr, bag.GetItemCompByPos(0));
	EXPECT_EQ(kInvalidSlot, bag.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(0u, bag.OccupiedGridCount());
	EXPECT_EQ(1u, LogAttempts() - logsBefore);

	bool changed = true;
	EXPECT_EQ(kSuccess, BagService::MergeAndCompact(player, bag, CompactPolicy::kMergeOnly, &changed));
	EXPECT_FALSE(changed);
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidA));
	EXPECT_EQ(0u, bag.OccupiedGridCount());
	EXPECT_EQ(1u, LogAttempts() - logsBefore) << "整理不能重复记录已消费实例的销毁";
	EXPECT_TRUE(bag.IsLayerConsistent());
}

} // namespace

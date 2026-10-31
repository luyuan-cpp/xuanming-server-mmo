// 神秘商人(docs/design/mystery-merchant.md §5)在 scene 节点这一侧的单测。
//
// 分两组:
//   * MysteryMerchantRulesTest —— mystery_merchant_rules.h 里的纯规则:折后价、总价、时间窗、
//     下一次出现时刻、距离、限购记账。不碰 ECS、不读配表。
//   * PlayerMysteryMerchantTest —— PlayerMysteryMerchantSystem 的编排:查询下发什么、购买的每一道
//     拒绝、拒绝时钱 / 背包 / 限购记录都不变、成功时三样一起变;以及记录新旧的取舍。
//
// 这里验不到的(要靠联调 / robot merchant-smoke):从 Redis 读记录、向 gate 广播公告、handler 取墙钟。
// scene_manager 那一侧「什么时候排一轮、挑哪条线、掷什么货架」在
// go/scene_manager/internal/logic/mystery_merchant_test.go。
//
// 数值一律现读配表(价格、限购都是占位建议,策划随时会改),只把夹具用到的那几行的**形状**
// (可叠加 / 不可叠加、限购是否大于 1)当作前置条件在 SetUp 里核对。
//
// 夹具自带:SetUp 自己加载用到的每一张表、自己装 item 号段、自己清神秘商人的缓存;
// TearDown 把这些都恢复 —— 不依赖别的文件的用例先跑或后跑。
// 夹具玩家不挂 Player 标签组件:出现公告的名单取的是 view<Player>,在这里恒为空,
// 所以灌记录(ApplyStateForTest)不会真的向 gate 发任何东西。

#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <limits>
#include <string>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_mystery_merchant_comp.pb.h"
#include "proto/scene/player_mystery_merchant.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/system/mystery_merchant_rules.h"
#include "services/scene/player/system/player_mystery_merchant.h"
#include "table/code/item_table.h"
#include "table/code/mysterymerchantgoods_table.h"
#include "table/code/mysterymerchantrule_table.h"
#include "table/code/mysterymerchantspot_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mystery_merchant_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

// ════════════════════════════════════════════════════════════════════════════
// 纯规则
// ════════════════════════════════════════════════════════════════════════════

::MysteryMerchantState MakeState(uint64_t roundId, uint64_t appearAtMs, uint64_t expireAtMs, uint32_t intervalSeconds)
{
	::MysteryMerchantState state;
	state.set_round_id(roundId);
	state.set_revision(1);
	state.set_appear_at_ms(appearAtMs);
	state.set_expire_at_ms(expireAtMs);
	state.set_interval_seconds(intervalSeconds);
	return state;
}

TEST(MysteryMerchantRulesTest, DiscountedPriceRoundsUpAndNeverReachesZero)
{
	EXPECT_EQ(240u, merchantrules::DiscountedPrice(400, 60));
	EXPECT_EQ(200u, merchantrules::DiscountedPrice(400, 50));
	EXPECT_EQ(160u, merchantrules::DiscountedPrice(400, 40));
	// 除不尽时向上取整:999 × 40% = 399.6 → 400。
	EXPECT_EQ(400u, merchantrules::DiscountedPrice(999, 40));
	EXPECT_EQ(1u, merchantrules::DiscountedPrice(1, 40)) << "原价 1 打 4 折仍是 1,不会白送";
	EXPECT_EQ(1u, merchantrules::DiscountedPrice(1, 1));
	EXPECT_EQ(400u, merchantrules::DiscountedPrice(400, 100));
	// 上限处不溢出:10^9 × 100 + 99 远小于 uint64 上限。
	EXPECT_EQ(merchantrules::kMaxGoodsPrice, merchantrules::DiscountedPrice(merchantrules::kMaxGoodsPrice, 100));
	EXPECT_EQ(400'000'000u, merchantrules::DiscountedPrice(merchantrules::kMaxGoodsPrice, 40));
}

TEST(MysteryMerchantRulesTest, DiscountPercentRange)
{
	EXPECT_FALSE(merchantrules::IsValidDiscountPercent(0)) << "0 = 白送,不是折扣";
	EXPECT_TRUE(merchantrules::IsValidDiscountPercent(1));
	EXPECT_TRUE(merchantrules::IsValidDiscountPercent(40));
	EXPECT_TRUE(merchantrules::IsValidDiscountPercent(100));
	EXPECT_FALSE(merchantrules::IsValidDiscountPercent(101)) << "大于 100 是涨价";
}

TEST(MysteryMerchantRulesTest, TotalCostRejectsZeroAndOverflow)
{
	int64_t total = -1;
	ASSERT_TRUE(merchantrules::TotalCost(240, 5, total));
	EXPECT_EQ(1200, total);

	EXPECT_FALSE(merchantrules::TotalCost(240, 0, total));
	EXPECT_FALSE(merchantrules::TotalCost(0, 5, total));

	const auto limit = static_cast<uint64_t>(std::numeric_limits<int64_t>::max());
	ASSERT_TRUE(merchantrules::TotalCost(limit, 1, total));
	EXPECT_EQ(std::numeric_limits<int64_t>::max(), total);
	EXPECT_FALSE(merchantrules::TotalCost(limit, 2, total)) << "超过 int64 上限必须拒绝,不能回绕";
	EXPECT_FALSE(merchantrules::TotalCost(limit / 2 + 1, 2, total));
}

TEST(MysteryMerchantRulesTest, PresenceWindowIsHalfOpen)
{
	const auto state = MakeState(100, 1'000'000, 2'800'000, 3600);
	EXPECT_FALSE(merchantrules::IsPresent(state, 999'999));
	EXPECT_TRUE(merchantrules::IsPresent(state, 1'000'000)) << "出现时刻含";
	EXPECT_TRUE(merchantrules::IsPresent(state, 2'799'999));
	EXPECT_FALSE(merchantrules::IsPresent(state, 2'800'000)) << "离开时刻不含";
}

TEST(MysteryMerchantRulesTest, NextAppearIsTheNextBoundaryWithAGraceRightAfterEachOne)
{
	const uint64_t appear = 3'600'000'000ULL; // 某个整点(毫秒)
	const uint64_t hour = 3'600'000ULL;
	const uint64_t grace = merchantrules::kNextAppearGraceMs;
	ASSERT_LT(grace, hour);
	const auto state = MakeState(appear / 1000, appear, appear + 1'800'000, 3600);

	EXPECT_EQ(appear + hour, merchantrules::NextAppearAtMs(state, appear)) << "在场时指的是下一轮";
	EXPECT_EQ(appear + hour, merchantrules::NextAppearAtMs(state, appear + grace - 1))
		<< "这一轮自己的出现时刻不享受宽限:手里的记录就是这一轮";
	EXPECT_EQ(appear + hour, merchantrules::NextAppearAtMs(state, appear + 1'800'000));
	EXPECT_EQ(appear + hour, merchantrules::NextAppearAtMs(state, appear + hour - 1));

	// 刚过整点、新一轮的记录还没读到:答「就是这个整点」(不晚于 now,客户端显示成即将出现并稍后重查),
	// 不能跳到再下一个整点 —— 否则这时进场的玩家会看到「59 分 50 秒后出现」,几秒后商人却到了。
	EXPECT_EQ(appear + hour, merchantrules::NextAppearAtMs(state, appear + hour));
	EXPECT_EQ(appear + hour, merchantrules::NextAppearAtMs(state, appear + hour + grace - 1));
	// 宽限期过了还是没有新记录(调度停了):按间隔往后推。
	EXPECT_EQ(appear + 2 * hour, merchantrules::NextAppearAtMs(state, appear + hour + grace));
	EXPECT_EQ(appear + 3 * hour, merchantrules::NextAppearAtMs(state, appear + 3 * hour + 5));
	EXPECT_EQ(appear + 4 * hour, merchantrules::NextAppearAtMs(state, appear + 3 * hour + grace + 5))
		<< "调度停了几轮也能推算出下一个整点";
	EXPECT_EQ(appear, merchantrules::NextAppearAtMs(state, appear - 5)) << "时钟比出现时刻还早:下一次就是这一轮";

	auto broken = state;
	broken.set_interval_seconds(0);
	EXPECT_EQ(0u, merchantrules::NextAppearAtMs(broken, appear)) << "间隔为 0 的坏记录:未知";
}

TEST(MysteryMerchantRulesTest, InteractRangeIsHorizontalAndInclusive)
{
	EXPECT_TRUE(merchantrules::WithinInteractRange(176, 206, 176, 206, 8));
	EXPECT_TRUE(merchantrules::WithinInteractRange(184, 206, 176, 206, 8)) << "恰好等于范围算在内";
	EXPECT_FALSE(merchantrules::WithinInteractRange(184.01, 206, 176, 206, 8));
	// 3-4-5:水平距离 5。
	EXPECT_TRUE(merchantrules::WithinInteractRange(179, 210, 176, 206, 5));
	EXPECT_FALSE(merchantrules::WithinInteractRange(179, 210, 176, 206, 4));
	EXPECT_TRUE(merchantrules::WithinInteractRange(176, 206, 176, 206, 0)) << "范围 0 时只有重合才算";
}

TEST(MysteryMerchantRulesTest, ShelfLookup)
{
	::MysteryMerchantState state;
	auto *entry = state.add_shelf();
	entry->set_goods_id(3);
	entry->set_discount_percent(50);
	entry = state.add_shelf();
	entry->set_goods_id(7);
	entry->set_discount_percent(40);

	ASSERT_NE(nullptr, merchantrules::FindShelfEntry(state, 7));
	EXPECT_EQ(40u, merchantrules::FindShelfEntry(state, 7)->discount_percent());
	EXPECT_EQ(nullptr, merchantrules::FindShelfEntry(state, 5));
}

TEST(MysteryMerchantRulesTest, PurchaseRecordIsScopedToOneRound)
{
	::PlayerMysteryMerchantComp record;
	EXPECT_EQ(0u, merchantrules::BoughtCount(record, 100, 3)) << "从未买过";

	merchantrules::RecordPurchase(record, 100, 3, 2);
	merchantrules::RecordPurchase(record, 100, 7, 1);
	merchantrules::RecordPurchase(record, 100, 3, 1);
	EXPECT_EQ(100u, record.round_id());
	EXPECT_EQ(2, record.purchases_size()) << "同一件商品累加在一条里,不重复建条目";
	EXPECT_EQ(3u, merchantrules::BoughtCount(record, 100, 3));
	EXPECT_EQ(1u, merchantrules::BoughtCount(record, 100, 7));
	EXPECT_EQ(0u, merchantrules::BoughtCount(record, 100, 9));

	// 下一轮:旧记录读出来就是 0(还没写之前记录本身不变)。
	EXPECT_EQ(0u, merchantrules::BoughtCount(record, 101, 3));
	EXPECT_EQ(2, record.purchases_size());

	// 下一轮第一次购买:整份清空再记。
	merchantrules::RecordPurchase(record, 101, 7, 4);
	EXPECT_EQ(101u, record.round_id());
	ASSERT_EQ(1, record.purchases_size());
	EXPECT_EQ(4u, merchantrules::BoughtCount(record, 101, 7));
	EXPECT_EQ(0u, merchantrules::BoughtCount(record, 101, 3));
	EXPECT_EQ(0u, merchantrules::BoughtCount(record, 100, 3)) << "上一轮的记录已经作废";
}

TEST(MysteryMerchantRulesTest, LimitCheckDoesNotWrapAround)
{
	EXPECT_FALSE(merchantrules::WouldExceedLimit(0, 5, 5));
	EXPECT_TRUE(merchantrules::WouldExceedLimit(0, 6, 5));
	EXPECT_FALSE(merchantrules::WouldExceedLimit(3, 2, 5));
	EXPECT_TRUE(merchantrules::WouldExceedLimit(3, 3, 5));
	EXPECT_TRUE(merchantrules::WouldExceedLimit(5, 1, 5));
	EXPECT_TRUE(merchantrules::WouldExceedLimit(9, 1, 5)) << "已购数比限购还大(表被调小):一律拒绝";
	EXPECT_TRUE(merchantrules::WouldExceedLimit(1, std::numeric_limits<uint32_t>::max(), 5)) << "bought + count 回绕也不能放行";
}

TEST(MysteryMerchantRulesTest, StateVersionMatchesTheWriterFormula)
{
	// round_id = 这一轮出现时刻的 Unix 秒。
	auto state = MakeState(1'757'001'600ULL, 0, 0, 3600);
	state.set_revision(2);
	// 与 go/scene_manager mystery_merchant.go 的写入脚本同一个算法:round_id × 1000 + revision。
	EXPECT_EQ(1'757'001'600'002ULL, merchantrules::StateVersion(state));
}

TEST(MysteryMerchantRulesTest, StaleRecordsAreDiscardedOnlyWhileTheCachedRoundIsRunning)
{
	const uint64_t appear = 1'757'001'600'000ULL;
	auto cached = MakeState(appear / 1000, appear, appear + 1'800'000, 3600);
	cached.set_revision(2);
	auto sameRoundEarlier = cached;
	sameRoundEarlier.set_revision(1);
	const auto earlierRound = MakeState(appear / 1000 - 3600, appear - 3'600'000, appear - 1'800'000, 3600);
	const auto nextRound = MakeState(appear / 1000 + 3600, appear + 3'600'000, appear + 5'400'000, 3600);

	// 手里那一轮还在进行:版本更小的丢弃(防 Redis 主从切换把同一轮退回更早的落位),相同或更大的采纳。
	EXPECT_TRUE(merchantrules::ShouldDiscardAsStale(sameRoundEarlier, cached, appear + 60'000));
	EXPECT_TRUE(merchantrules::ShouldDiscardAsStale(earlierRound, cached, appear + 60'000));
	EXPECT_TRUE(merchantrules::ShouldDiscardAsStale(sameRoundEarlier, cached, appear + 1'799'999));
	EXPECT_FALSE(merchantrules::ShouldDiscardAsStale(cached, cached, appear + 60'000)) << "同一份记录每 5 秒读到一次,不算旧";
	EXPECT_FALSE(merchantrules::ShouldDiscardAsStale(nextRound, cached, appear + 60'000));

	// 手里那一轮结束(离开时刻那一毫秒起):保护解除,读到什么都采纳 ——
	// scene_manager 改过出现间隔后会强制覆盖出一份版本更小的记录,没有这个出口本节点会永久拒收。
	EXPECT_FALSE(merchantrules::ShouldDiscardAsStale(sameRoundEarlier, cached, appear + 1'800'000));
	EXPECT_FALSE(merchantrules::ShouldDiscardAsStale(earlierRound, cached, appear + 1'800'000));
}

// ════════════════════════════════════════════════════════════════════════════
// 编排
// ════════════════════════════════════════════════════════════════════════════

const char *const kExportFirst =
	"tables not exported yet: run the table exporter first (data/*.xlsx -> generated tables)";

constexpr uint64_t kPlayerGuid = 818181;
constexpr uint64_t kOtherPlayerGuid = 818182;
constexpr uint64_t kSegmentLo = 1400000;
constexpr uint64_t kSegmentHi = 1500000;

constexpr uint32_t kZoneId = 1;
constexpr uint32_t kIntervalSeconds = 3600;
constexpr uint64_t kRoundId = 1757001600ULL;                 // 轮次号 = 这一轮出现时刻的 Unix 秒(一个整点)
constexpr uint64_t kNextRoundId = kRoundId + kIntervalSeconds; // 下一轮
constexpr uint64_t kAppearAtMs = kRoundId * 1000ULL;
constexpr uint64_t kStayMs = 1800000ULL;
constexpr uint64_t kNowMs = kAppearAtMs + 60000ULL; // 出现后 1 分钟
constexpr uint64_t kMerchantSceneId = 63941896849850368ULL;
constexpr uint64_t kOtherSceneId = 63941896849850369ULL;
constexpr uint32_t kSceneConfigId = 1;
constexpr uint32_t kChannelNo = 3;
constexpr double kMerchantX = 176.0;
constexpr double kMerchantY = 206.0;
constexpr uint32_t kUnknownGoods = 987654;
constexpr uint64_t kPlenty = 10'000'000ULL;

// 与 bag_instance_data_test.cpp / player_equip_test.cpp 同一个做法:无 I/O 的假传输 + 直接投一段号。
bool ArmItemSegment(uint64_t lo, uint64_t hi)
{
	auto &client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
	client.Reset();
	GuidSegmentClient::Options options;
	options.mixIds = false;
	options.kindName = "item";
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

// 把 item 号段置为「请求发出去了、永远没有应答」:需要铸号的入包会在改动背包之前整体拒绝
// (与 bag_test.cpp 的 BagSegmentNotReadyTest 同一个做法)。夹具的 TearDown 会把号段恢复成基线。
bool StarveItemSegment()
{
	auto &client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
	client.Reset();
	GuidSegmentClient::Options options;
	options.mixIds = false;
	options.kindName = "item";
	options.initialStep = 100000;
	if (!client.Enable(options, [](const std::string &, uint32_t) { return true; },
					   [](GuidSegmentClient::TimerKind, double, std::function<void()>) {},
					   [] { return 0.0; }))
	{
		return false;
	}
	client.Warm();
	return !client.IsReady();
}

const MysteryMerchantGoodsTable *GoodsRowOrNull(uint32_t goodsId)
{
	return MysteryMerchantGoodsTableManager::Instance().FindByIdSilent(goodsId).first;
}

const ItemTable *ItemRowOrNull(uint32_t configId)
{
	return ItemTableManager::Instance().FindByIdSilent(configId).first;
}

class PlayerMysteryMerchantTest : public testing::Test
{
protected:
	static void LoadTables()
	{
		ItemTableManager::Instance().Load();
		MysteryMerchantRuleTableManager::Instance().Load();
		MysteryMerchantGoodsTableManager::Instance().Load();
		MysteryMerchantSpotTableManager::Instance().Load();
	}

	void SetUp() override
	{
		LoadTables();
		ASSERT_TRUE(ArmItemSegment(kSegmentLo, kSegmentHi));
		ASSERT_NO_FATAL_FAILURE(PickFixtureGoods());
		PlayerMysteryMerchantSystem::ResetForTest();
		ApplyShelf({{stackGoods, 60}, {uniqueGoods, 40}});
		scene = NewScene(kMerchantSceneId);
		otherScene = NewScene(kOtherSceneId);
		player = NewPlayer(kPlayerGuid, scene, kMerchantX, kMerchantY, kPlenty);
	}

	void TearDown() override
	{
		for (const auto entity : players)
		{
			if (tlsEcs.actorRegistry.valid(entity))
			{
				tlsEcs.actorRegistry.destroy(entity);
			}
		}
		for (const auto entity : {scene, otherScene})
		{
			if (tlsEcs.sceneRegistry.valid(entity))
			{
				tlsEcs.sceneRegistry.destroy(entity);
			}
		}
		PlayerMysteryMerchantSystem::ResetForTest();
		// 恢复其它背包用例使用的无 I/O 号段基线(与 bag_test.cpp 的 main() 同一段范围)。
		EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
	}

	// 从商品表里挑两件夹具商品:一件可叠加且限购 >= 2(验「分两次买到上限」),一件限购恰为 1。
	// 现读配表而不是写死 id:策划调了行不该让用例红,但两种形状都找不到时要说清楚。
	void PickFixtureGoods()
	{
		ASSERT_GT(MysteryMerchantGoodsTableManager::Instance().Count(), 0u) << kExportFirst;
		ASSERT_GT(MysteryMerchantRuleTableManager::Instance().Count(), 0u) << kExportFirst;
		for (const auto &row : MysteryMerchantGoodsTableManager::Instance().FindAll().data())
		{
			const auto *item = ItemRowOrNull(row.item_id());
			if (row.weight() == 0 || item == nullptr || row.currency_type() != kCurrencyGold)
			{
				continue;
			}
			if (stackGoods == 0 && item->max_stack_size() > 1 && row.limit_count() >= 2)
			{
				stackGoods = row.id();
				stackItem = row.item_id();
			}
			else if (uniqueGoods == 0 && row.limit_count() == 1 && row.item_id() != stackItem)
			{
				uniqueGoods = row.id();
			}
		}
		ASSERT_NE(0u, stackGoods) << "MysteryMerchantGoods needs a gold-priced stackable row with limit_count >= 2";
		ASSERT_NE(0u, uniqueGoods) << "MysteryMerchantGoods needs a gold-priced row with limit_count == 1";
		ASSERT_NE(GoodsRowOrNull(stackGoods)->item_id(), GoodsRowOrNull(uniqueGoods)->item_id())
			<< "the two fixture goods must grant different items";
		interactRange = MysteryMerchantRuleTableManager::Instance().FindAll().data().begin()->interact_range();
		ASSERT_GT(interactRange, 0u);
	}

	struct ShelfSpec
	{
		uint32_t goodsId;
		uint32_t discountPercent;
	};

	static ::MysteryMerchantState BuildState(uint64_t roundId, uint32_t revision, uint64_t sceneId,
											 const std::vector<ShelfSpec> &shelf)
	{
		::MysteryMerchantState state;
		state.set_zone_id(kZoneId);
		state.set_round_id(roundId);
		state.set_revision(revision);
		state.set_scene_config_id(kSceneConfigId);
		state.set_scene_id(sceneId);
		state.set_channel_no(kChannelNo);
		state.set_spot_id(1);
		state.mutable_position()->set_x(kMerchantX);
		state.mutable_position()->set_y(kMerchantY);
		const uint64_t appearAtMs = roundId * 1000ULL; // 轮次号就是出现时刻的秒数
		state.set_appear_at_ms(appearAtMs);
		state.set_expire_at_ms(appearAtMs + kStayMs);
		state.set_interval_seconds(kIntervalSeconds);
		for (const auto &spec : shelf)
		{
			auto *entry = state.add_shelf();
			entry->set_goods_id(spec.goodsId);
			entry->set_discount_percent(spec.discountPercent);
		}
		return state;
	}

	// 把一份记录灌进缓存(等价于「刚从 Redis 读到」)。
	void ApplyShelf(const std::vector<ShelfSpec> &shelf, uint64_t roundId = kRoundId, uint32_t revision = 1,
					uint64_t sceneId = kMerchantSceneId)
	{
		const auto state = BuildState(roundId, revision, sceneId, shelf);
		PlayerMysteryMerchantSystem::ApplyStateForTest(&state, state.appear_at_ms() + 60000ULL);
	}

	SceneEntity NewScene(uint64_t sceneId)
	{
		const auto entity = tlsEcs.sceneRegistry.create();
		auto &info = tlsEcs.sceneRegistry.emplace<SceneInfoComp>(entity);
		info.set_scene_config_id(kSceneConfigId);
		info.set_scene_id(sceneId);
		return entity;
	}

	entt::entity NewPlayer(uint64_t guid, SceneEntity sceneEntity, double x, double y, uint64_t gold)
	{
		const auto entity = tlsEcs.actorRegistry.create();
		players.push_back(entity);
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, Guid{guid});
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
		for (auto &bag : bags.bags)
		{
			bag.SetPlayerGuid(Guid{guid});
		}
		auto &currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(entity);
		while (currency.values_size() < static_cast<int>(kCurrencyMax))
		{
			currency.add_values(0);
		}
		currency.set_values(static_cast<int>(kCurrencyGold), gold);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(entity);
		auto &transform = tlsEcs.actorRegistry.emplace<Transform>(entity);
		transform.mutable_location()->set_x(x);
		transform.mutable_location()->set_y(y);
		tlsEcs.actorRegistry.emplace<SceneEntityComp>(entity).sceneEntity = sceneEntity;
		// 生产侧由 PlayerDatabaseMessageFieldsUnmarshal 无条件挂上。
		tlsEcs.actorRegistry.emplace<PlayerMysteryMerchantComp>(entity);
		return entity;
	}

	uint64_t Gold(entt::entity who) const { return CurrencySystem::GetBalance(who, kCurrencyGold); }
	Bag &Inventory(entt::entity who) { return tlsEcs.actorRegistry.get<PlayerBagsComp>(who).bags[kInventory]; }
	const ::PlayerMysteryMerchantComp &Record(entt::entity who) const
	{
		return tlsEcs.actorRegistry.get<PlayerMysteryMerchantComp>(who);
	}

	uint64_t PriceOf(uint32_t goodsId, uint32_t discountPercent) const
	{
		return merchantrules::DiscountedPrice(GoodsRowOrNull(goodsId)->price(), discountPercent);
	}

	::BuyMysteryMerchantGoodsRequest Request(uint32_t goodsId, uint32_t count, uint32_t discountPercent,
											  uint64_t roundId = kRoundId) const
	{
		::BuyMysteryMerchantGoodsRequest request;
		request.set_round_id(roundId);
		request.set_goods_id(goodsId);
		request.set_count(count);
		// 报价防呆四项:与客户端从货架上抄回来的值相同(价格按折扣现算,其余三项照表)。
		const auto *row = GoodsRowOrNull(goodsId);
		request.set_expected_price(row != nullptr ? PriceOf(goodsId, discountPercent) : 1);
		if (row != nullptr)
		{
			request.set_expected_currency_type(row->currency_type());
			request.set_expected_item_config_id(row->item_id());
			request.set_expected_item_count(row->item_count());
		}
		return request;
	}

	uint32_t Buy(entt::entity who, const ::BuyMysteryMerchantGoodsRequest &request, uint64_t nowMs = kNowMs)
	{
		response.Clear();
		return PlayerMysteryMerchantSystem::Buy(who, request, nowMs, response);
	}

	// 拒绝路径的统一断言:钱、背包、限购记录都与购买前一致,应答不带成功才有的两项。
	void ExpectRejectedWithoutSideEffects(uint32_t expectedTip, entt::entity who,
										  const ::BuyMysteryMerchantGoodsRequest &request, uint64_t nowMs = kNowMs)
	{
		const uint64_t goldBefore = Gold(who);
		const std::size_t itemsBefore = Inventory(who).OccupiedGridCount();
		const std::string recordBefore = Record(who).SerializeAsString();

		EXPECT_EQ(expectedTip, Buy(who, request, nowMs));

		EXPECT_EQ(goldBefore, Gold(who)) << "rejected purchase must not charge";
		EXPECT_EQ(itemsBefore, Inventory(who).OccupiedGridCount()) << "rejected purchase must not grant";
		EXPECT_EQ(recordBefore, Record(who).SerializeAsString()) << "rejected purchase must not consume the limit";
		EXPECT_FALSE(response.has_info());
		EXPECT_FALSE(response.has_bag());
	}

	const ::MysteryMerchantGoodsInfo *FindGoods(const ::MysteryMerchantInfo &info, uint32_t goodsId) const
	{
		for (const auto &goods : info.goods())
		{
			if (goods.goods_id() == goodsId)
			{
				return &goods;
			}
		}
		return nullptr;
	}

	entt::entity player{entt::null};
	SceneEntity scene{entt::null};
	SceneEntity otherScene{entt::null};
	std::vector<entt::entity> players;
	uint32_t stackGoods = 0;
	uint32_t stackItem = 0;
	uint32_t uniqueGoods = 0;
	uint32_t interactRange = 0;
	::BuyMysteryMerchantGoodsResponse response;
};

// ── 查询 ────────────────────────────────────────────────────────────────────

TEST_F(PlayerMysteryMerchantTest, InfoOnTheMerchantLineCarriesTheShelf)
{
	::MysteryMerchantInfo info;
	PlayerMysteryMerchantSystem::FillInfo(player, kNowMs, info);

	EXPECT_TRUE(info.active());
	EXPECT_TRUE(info.here());
	EXPECT_EQ(kRoundId, info.round_id());
	EXPECT_EQ(kSceneConfigId, info.scene_config_id());
	EXPECT_EQ(kMerchantSceneId, info.scene_id());
	EXPECT_EQ(kChannelNo, info.channel_no());
	EXPECT_DOUBLE_EQ(kMerchantX, info.position().x());
	EXPECT_DOUBLE_EQ(kMerchantY, info.position().y());
	EXPECT_EQ(kAppearAtMs, info.appear_at_ms());
	EXPECT_EQ(kAppearAtMs + kStayMs, info.expire_at_ms());
	EXPECT_EQ(kAppearAtMs + 3600000ULL, info.next_appear_at_ms());
	EXPECT_EQ(kNowMs, info.server_time_ms());
	EXPECT_EQ(interactRange, info.interact_range());

	ASSERT_EQ(2, info.goods_size());
	const auto *stack = FindGoods(info, stackGoods);
	ASSERT_NE(nullptr, stack);
	const auto *row = GoodsRowOrNull(stackGoods);
	EXPECT_EQ(row->item_id(), stack->item_config_id());
	EXPECT_EQ(row->item_count(), stack->item_count());
	EXPECT_EQ(row->price(), stack->original_price());
	EXPECT_EQ(60u, stack->discount_percent());
	EXPECT_EQ(PriceOf(stackGoods, 60), stack->price());
	EXPECT_EQ(row->limit_count(), stack->limit_count());
	EXPECT_EQ(0u, stack->bought_count());
	EXPECT_EQ(static_cast<uint32_t>(kCurrencyGold), stack->currency_type());
	// 展示名:商品表有就用商品表的,否则用物品表的。
	const auto *item = ItemRowOrNull(row->item_id());
	EXPECT_EQ(row->name().empty() ? item->name() : row->name(), stack->name());
	EXPECT_EQ(item->icon_key(), stack->icon_key());

	const auto *unique = FindGoods(info, uniqueGoods);
	ASSERT_NE(nullptr, unique);
	EXPECT_EQ(40u, unique->discount_percent());
	EXPECT_EQ(PriceOf(uniqueGoods, 40), unique->price());
}

TEST_F(PlayerMysteryMerchantTest, InfoOnAnotherLineHidesTheShelf)
{
	const auto elsewhere = NewPlayer(kOtherPlayerGuid, otherScene, kMerchantX, kMerchantY, kPlenty);
	::MysteryMerchantInfo info;
	PlayerMysteryMerchantSystem::FillInfo(elsewhere, kNowMs, info);

	EXPECT_TRUE(info.active());
	EXPECT_FALSE(info.here());
	EXPECT_EQ(kChannelNo, info.channel_no()) << "不在同一条线也要知道商人在几线";
	EXPECT_EQ(kMerchantSceneId, info.scene_id());
	EXPECT_EQ(0, info.goods_size()) << "不在同一条线看不到货架";
}

TEST_F(PlayerMysteryMerchantTest, InfoAfterTheMerchantLeftOnlyTellsTheNextAppearance)
{
	::MysteryMerchantInfo info;
	const uint64_t afterLeaving = kAppearAtMs + kStayMs;
	PlayerMysteryMerchantSystem::FillInfo(player, afterLeaving, info);

	EXPECT_FALSE(info.active());
	EXPECT_FALSE(info.here());
	EXPECT_EQ(0, info.goods_size());
	EXPECT_EQ(0u, info.scene_id());
	EXPECT_EQ(kAppearAtMs + 3600000ULL, info.next_appear_at_ms());
	EXPECT_EQ(afterLeaving, info.server_time_ms());
}

TEST_F(PlayerMysteryMerchantTest, InfoWithoutAnyRecordIsInactiveAndUnknown)
{
	PlayerMysteryMerchantSystem::ApplyStateForTest(nullptr, kNowMs);
	::MysteryMerchantInfo info;
	PlayerMysteryMerchantSystem::FillInfo(player, kNowMs, info);

	EXPECT_FALSE(info.active());
	EXPECT_EQ(0u, info.next_appear_at_ms()) << "从没有过记录:下一次出现时刻未知";
	EXPECT_EQ(kNowMs, info.server_time_ms());
}

TEST_F(PlayerMysteryMerchantTest, GoodsThatCannotBeSoldAreLeftOffTheShelf)
{
	// 记录里有一件本节点的表里不存在的商品(两边表版本不一致):不上架,其它商品照常。
	ApplyShelf({{stackGoods, 60}, {kUnknownGoods, 50}}, kRoundId, 2);
	::MysteryMerchantInfo info;
	PlayerMysteryMerchantSystem::FillInfo(player, kNowMs, info);

	ASSERT_EQ(1, info.goods_size());
	EXPECT_EQ(stackGoods, info.goods(0).goods_id());
}

// ── 购买:成功 ────────────────────────────────────────────────────────────────

TEST_F(PlayerMysteryMerchantTest, BuyChargesGrantsAndRecordsTogether)
{
	const auto *row = GoodsRowOrNull(stackGoods);
	const uint64_t unitPrice = PriceOf(stackGoods, 60);
	const uint64_t goldBefore = Gold(player);

	ASSERT_EQ(kSuccess, Buy(player, Request(stackGoods, 2, 60)));

	EXPECT_EQ(goldBefore - unitPrice * 2, Gold(player));
	EXPECT_EQ(static_cast<std::size_t>(row->item_count()) * 2, Inventory(player).GetTotalItemCount(row->item_id()));
	EXPECT_EQ(kRoundId, Record(player).round_id());
	EXPECT_EQ(2u, merchantrules::BoughtCount(Record(player), kRoundId, stackGoods));

	// 应答:最新货架(已购数更新)+ 背包全量(余额已更新)。
	ASSERT_TRUE(response.has_info());
	EXPECT_TRUE(response.info().here());
	const auto *goods = FindGoods(response.info(), stackGoods);
	ASSERT_NE(nullptr, goods);
	EXPECT_EQ(2u, goods->bought_count());
	ASSERT_TRUE(response.has_bag());
	ASSERT_GT(response.bag().currency().values_size(), static_cast<int>(kCurrencyGold));
	EXPECT_EQ(goldBefore - unitPrice * 2, response.bag().currency().values(static_cast<int>(kCurrencyGold)));
}

TEST_F(PlayerMysteryMerchantTest, BuyUpToTheLimitAcrossSeveralRequestsThenRejects)
{
	const auto *row = GoodsRowOrNull(stackGoods);
	const uint32_t limit = row->limit_count();

	ASSERT_EQ(kSuccess, Buy(player, Request(stackGoods, limit - 1, 60)));
	ASSERT_EQ(kSuccess, Buy(player, Request(stackGoods, 1, 60)));
	EXPECT_EQ(limit, merchantrules::BoughtCount(Record(player), kRoundId, stackGoods));

	ExpectRejectedWithoutSideEffects(kMysteryMerchantLimitReached, player, Request(stackGoods, 1, 60));
}

TEST_F(PlayerMysteryMerchantTest, LimitIsPerPlayer)
{
	ASSERT_EQ(kSuccess, Buy(player, Request(uniqueGoods, 1, 40)));
	ExpectRejectedWithoutSideEffects(kMysteryMerchantLimitReached, player, Request(uniqueGoods, 1, 40));

	// 另一名玩家的名额不受影响。
	const auto other = NewPlayer(kOtherPlayerGuid, scene, kMerchantX, kMerchantY, kPlenty);
	EXPECT_EQ(kSuccess, Buy(other, Request(uniqueGoods, 1, 40)));
}

TEST_F(PlayerMysteryMerchantTest, LimitResetsInTheNextRound)
{
	ASSERT_EQ(kSuccess, Buy(player, Request(uniqueGoods, 1, 40)));
	ExpectRejectedWithoutSideEffects(kMysteryMerchantLimitReached, player, Request(uniqueGoods, 1, 40));

	// 下一轮(同一条线、同一件商品):名额重置。
	ApplyShelf({{uniqueGoods, 50}}, kNextRoundId);
	const uint64_t nextRoundNow = kNowMs + kIntervalSeconds * 1000ULL;
	ASSERT_EQ(kSuccess, Buy(player, Request(uniqueGoods, 1, 50, kNextRoundId), nextRoundNow));
	EXPECT_EQ(kNextRoundId, Record(player).round_id());
	EXPECT_EQ(1, Record(player).purchases_size()) << "上一轮的条目在新一轮第一次购买时整份清掉";
}

TEST_F(PlayerMysteryMerchantTest, RelocationWithinTheRoundKeepsTheLimit)
{
	ASSERT_EQ(kSuccess, Buy(player, Request(uniqueGoods, 1, 40)));

	// 同一轮内商人换到另一条线(revision 2):玩家跟过去,名额不重置。
	ApplyShelf({{stackGoods, 60}, {uniqueGoods, 40}}, kRoundId, 2, kOtherSceneId);
	tlsEcs.actorRegistry.get<SceneEntityComp>(player).sceneEntity = otherScene;
	ExpectRejectedWithoutSideEffects(kMysteryMerchantLimitReached, player, Request(uniqueGoods, 1, 40));
	EXPECT_EQ(kSuccess, Buy(player, Request(stackGoods, 1, 60))) << "没买过的商品在新线照常可买";
}

// ── 购买:拒绝 ────────────────────────────────────────────────────────────────

TEST_F(PlayerMysteryMerchantTest, RejectsInvalidCount)
{
	ExpectRejectedWithoutSideEffects(kMysteryMerchantInvalidCount, player, Request(stackGoods, 0, 60));
	ExpectRejectedWithoutSideEffects(kMysteryMerchantInvalidCount, player,
									 Request(stackGoods, merchantrules::kMaxBuyCountPerRequest + 1, 60));
	auto zeroGoods = Request(stackGoods, 1, 60);
	zeroGoods.set_goods_id(0);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantInvalidCount, player, zeroGoods);
}

TEST_F(PlayerMysteryMerchantTest, RejectsWhenMerchantIsNotPresent)
{
	// 离开时刻那一毫秒起就不能买。
	ExpectRejectedWithoutSideEffects(kMysteryMerchantNotPresent, player, Request(stackGoods, 1, 60),
									 kAppearAtMs + kStayMs);
	// 时钟还没到出现时刻。
	ExpectRejectedWithoutSideEffects(kMysteryMerchantNotPresent, player, Request(stackGoods, 1, 60), kAppearAtMs - 1);
	// 压根没有记录。
	PlayerMysteryMerchantSystem::ApplyStateForTest(nullptr, kNowMs);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantNotPresent, player, Request(stackGoods, 1, 60));
}

TEST_F(PlayerMysteryMerchantTest, RejectsStaleRoundAndUnknownGoodsAsShelfChanged)
{
	ExpectRejectedWithoutSideEffects(kMysteryMerchantShelfChanged, player,
									 Request(stackGoods, 1, 60, kRoundId - kIntervalSeconds));
	ExpectRejectedWithoutSideEffects(kMysteryMerchantShelfChanged, player, Request(kUnknownGoods, 1, 60));
}

TEST_F(PlayerMysteryMerchantTest, RejectsPriceThePlayerDidNotSee)
{
	auto request = Request(stackGoods, 1, 60);
	request.set_expected_price(request.expected_price() + 1);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantShelfChanged, player, request);
	request.set_expected_price(PriceOf(stackGoods, 40)); // 货架上这件是 6 折,不是 4 折
	ExpectRejectedWithoutSideEffects(kMysteryMerchantShelfChanged, player, request);
}

TEST_F(PlayerMysteryMerchantTest, RejectsAQuoteThatDiffersInCurrencyItemOrCount)
{
	// 价格没变、但货币 / 物品 / 每份数量变了(配表热更):同样不能按玩家没看到的内容成交。
	auto request = Request(stackGoods, 1, 60);
	request.set_expected_currency_type(request.expected_currency_type() + 1);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantShelfChanged, player, request);

	request = Request(stackGoods, 1, 60);
	request.set_expected_item_config_id(GoodsRowOrNull(uniqueGoods)->item_id());
	ExpectRejectedWithoutSideEffects(kMysteryMerchantShelfChanged, player, request);

	request = Request(stackGoods, 1, 60);
	request.set_expected_item_count(request.expected_item_count() + 1);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantShelfChanged, player, request);

	EXPECT_EQ(kSuccess, Buy(player, Request(stackGoods, 1, 60))) << "四项都对得上才成交";
}

TEST_F(PlayerMysteryMerchantTest, RejectsFromAnotherLine)
{
	const auto elsewhere = NewPlayer(kOtherPlayerGuid, otherScene, kMerchantX, kMerchantY, kPlenty);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantNotHere, elsewhere, Request(stackGoods, 1, 60));

	// 不在任何场景里(换图途中):同样拒绝。
	tlsEcs.actorRegistry.get<SceneEntityComp>(elsewhere).sceneEntity = entt::null;
	ExpectRejectedWithoutSideEffects(kMysteryMerchantNotHere, elsewhere, Request(stackGoods, 1, 60));
}

TEST_F(PlayerMysteryMerchantTest, RejectsWhenTooFarAndAcceptsAtTheEdge)
{
	auto &location = *tlsEcs.actorRegistry.get<Transform>(player).mutable_location();
	location.set_x(kMerchantX + interactRange + 0.5);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantTooFar, player, Request(stackGoods, 1, 60));

	location.set_x(kMerchantX + interactRange); // 恰好在范围边上
	EXPECT_EQ(kSuccess, Buy(player, Request(stackGoods, 1, 60)));

	// 高度差不算距离。
	location.set_z(500.0);
	EXPECT_EQ(kSuccess, Buy(player, Request(stackGoods, 1, 60)));
}

TEST_F(PlayerMysteryMerchantTest, RejectsWhenCurrencyIsNotEnough)
{
	const uint64_t unitPrice = PriceOf(stackGoods, 60);
	const auto poor = NewPlayer(kOtherPlayerGuid, scene, kMerchantX, kMerchantY, unitPrice * 2 - 1);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantCurrencyNotEnough, poor, Request(stackGoods, 2, 60));
	// 买得起的份数照常成交。
	EXPECT_EQ(kSuccess, Buy(poor, Request(stackGoods, 1, 60)));
	EXPECT_EQ(unitPrice - 1, Gold(poor));
}

TEST_F(PlayerMysteryMerchantTest, RejectsWhenBagIsFull)
{
	// 人物背包里先放一件别的物品,再把容量收到恰好占满(与 player_equip_test.cpp 的背包满用例同一做法):
	// 要买的那件是另一种物品,必须占一个新格子,于是放不下。
	auto &inventory = Inventory(player);
	ItemCountMap filler;
	filler[GoodsRowOrNull(uniqueGoods)->item_id()] = 1;
	ASSERT_EQ(kSuccess, BagService::AddItems(player, inventory, filler));
	inventory.SetCapacityForRestore(inventory.OccupiedGridCount());
	ASSERT_TRUE(inventory.IsFull());

	ExpectRejectedWithoutSideEffects(kMysteryMerchantBagFull, player, Request(stackGoods, 1, 60));
}

TEST_F(PlayerMysteryMerchantTest, RejectsWhileFrozenOrInBattle)
{
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantFrozen, player, Request(stackGoods, 1, 60));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);

	auto &inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
	inBattle.set_battle_id(4242);
	inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
	ExpectRejectedWithoutSideEffects(kMysteryMerchantInBattle, player, Request(stackGoods, 1, 60));
	tlsEcs.actorRegistry.remove<InBattleComp>(player);

	EXPECT_EQ(kSuccess, Buy(player, Request(stackGoods, 1, 60))) << "解冻、出战斗之后照常可买";
}

TEST_F(PlayerMysteryMerchantTest, RejectsGloballyBlockedItems)
{
	// 物品封禁只有全服这一种(个人封禁表已删除)。用例结束前解封,不影响其它用例。
	const auto *row = GoodsRowOrNull(stackGoods);
	ASSERT_FALSE(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, row->item_id()))
		<< "fixture item must start unblocked";
	GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, row->item_id());
	ExpectRejectedWithoutSideEffects(kMysteryMerchantItemBlocked, player, Request(stackGoods, 1, 60));

	GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, row->item_id());
	EXPECT_EQ(kSuccess, Buy(player, Request(stackGoods, 1, 60)));
}

TEST_F(PlayerMysteryMerchantTest, MissingPurchaseRecordFailsClosed)
{
	// 限购记录组件缺失(加载路径坏了):不许现造一份空的来放行。
	tlsEcs.actorRegistry.remove<PlayerMysteryMerchantComp>(player);
	const uint64_t goldBefore = Gold(player);
	EXPECT_EQ(kMysteryMerchantInternalError, Buy(player, Request(stackGoods, 1, 60)));
	EXPECT_EQ(goldBefore, Gold(player));
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerMysteryMerchantComp>(player)) << "must not emplace on the buy path";
	tlsEcs.actorRegistry.emplace<PlayerMysteryMerchantComp>(player);
}

TEST_F(PlayerMysteryMerchantTest, GrantRefusedAfterThePrecheckChargesNothing)
{
	// 预检(封禁、余额、空间)全过,但物品号段领不到号:发物在改动背包之前整体拒绝。
	// 回归:最初的实现先扣钱再发物,这里要走「退款」—— 而加钱入口会先抵扣补缴欠款、遇币种封禁还会拒绝,
	// 有欠款的玩家会钱货两空。现在先发物,所以钱根本没动过,也就不需要退。
	ASSERT_TRUE(StarveItemSegment());
	ExpectRejectedWithoutSideEffects(kMysteryMerchantInternalError, player, Request(uniqueGoods, 1, 40));
	ExpectRejectedWithoutSideEffects(kMysteryMerchantInternalError, player, Request(stackGoods, 1, 60));

	// 号段恢复之后照常可买:上面的拒绝没有留下任何半截状态。
	ASSERT_TRUE(ArmItemSegment(kSegmentLo, kSegmentHi));
	EXPECT_EQ(kSuccess, Buy(player, Request(stackGoods, 1, 60)));
	EXPECT_EQ(1u, merchantrules::BoughtCount(Record(player), kRoundId, stackGoods));
}

TEST_F(PlayerMysteryMerchantTest, OlderRecordIsAdoptedOnceTheCachedRoundIsOver)
{
	// scene_manager 在出现间隔被改过(或旧记录损坏)时会强制覆盖出一份版本号更小的记录。
	// 这里构造一份「版本更小、但停留得久、此刻仍在场」的记录,好从查询结果里看出它有没有被采纳。
	auto older = BuildState(kRoundId - kIntervalSeconds, 1, kOtherSceneId, {{stackGoods, 50}});
	older.set_expire_at_ms(kAppearAtMs + 2 * kStayMs);

	// 手里那一轮还在进行:不采纳。
	PlayerMysteryMerchantSystem::ApplyStateForTest(&older, kNowMs);
	::MysteryMerchantInfo info;
	PlayerMysteryMerchantSystem::FillInfo(player, kNowMs, info);
	EXPECT_EQ(kRoundId, info.round_id());
	EXPECT_EQ(kMerchantSceneId, info.scene_id());

	// 手里那一轮结束之后:必须采纳,否则本节点会永久拒收这份记录(只能重启)。
	const uint64_t afterCachedRound = kAppearAtMs + kStayMs + 1000ULL;
	PlayerMysteryMerchantSystem::ApplyStateForTest(&older, afterCachedRound);
	PlayerMysteryMerchantSystem::FillInfo(player, afterCachedRound, info);
	EXPECT_TRUE(info.active());
	EXPECT_EQ(kRoundId - kIntervalSeconds, info.round_id());
	EXPECT_EQ(kOtherSceneId, info.scene_id());
}

TEST_F(PlayerMysteryMerchantTest, OlderRecordNeverReplacesANewerOne)
{
	// 已经是 revision 2(换过线);迟到的 revision 1 不许把缓存退回去(这一轮还在进行)。
	ApplyShelf({{stackGoods, 60}}, kRoundId, 2, kOtherSceneId);
	ApplyShelf({{stackGoods, 60}}, kRoundId, 1, kMerchantSceneId);

	::MysteryMerchantInfo info;
	PlayerMysteryMerchantSystem::FillInfo(player, kNowMs, info);
	EXPECT_EQ(kOtherSceneId, info.scene_id());
	EXPECT_FALSE(info.here()) << "玩家还在原来那条线,商人已经不在这了";
}

} // namespace

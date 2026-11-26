// 挂机月卡(docs/design/afk-monthly-card.md)的用例:纯规则(afk_pass_rules.h)与 AfkPassSystem 的开通、GM 设定、
// 查询和 Redis 投影写出。投影经 SetProjectionSenderForTest 截获,不连 Redis。
//
// 夹具自带:SetUp 自己加载 AfkPassRule 表、自己装投影截获;TearDown 把它们恢复,不依赖别的文件的用例先跑或后跑。
// 价格、天数、上限都现读配表(策划调了数值不该让用例红),表不合法时直接报清楚。

#include <gtest/gtest.h>

#include <cstdint>
#include <limits>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/scene/player_afk_pass.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/afk_pass.h"
#include "services/scene/player/system/afk_pass_rules.h"
#include "services/scene/player/system/connection_lost_hold.h"
#include "table/code/afkpassrule_table.h"
#include "table/proto/tip/afk_pass_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
constexpr uint64_t kNow = 1'800'000'000; // 2027-01-15,远离 0 与溢出边界
constexpr uint64_t kDay = afk_pass::kSecondsPerDay;

afk_pass::Rule GoodRule()
{
	return afk_pass::Rule{30, static_cast<uint32_t>(kCurrencyDiamond), 300, 180, 72};
}
} // namespace

// ── 纯规则 ──────────────────────────────────────────────────────────────

TEST(AfkPassRulesTest, RenewalStartsFromNowWhenExpiredOrNeverOpened)
{
	EXPECT_EQ(afk_pass::RenewedExpiry(0, kNow, 30, 180), kNow + 30 * kDay) << "从未开通";
	EXPECT_EQ(afk_pass::RenewedExpiry(kNow - 1, kNow, 30, 180), kNow + 30 * kDay) << "已到期的从现在起算,过期的时间不补";
	EXPECT_EQ(afk_pass::RenewedExpiry(kNow, kNow, 30, 180), kNow + 30 * kDay) << "恰好到期";
}

TEST(AfkPassRulesTest, RenewalStacksOnTopOfTheRemainingTime)
{
	EXPECT_EQ(afk_pass::RenewedExpiry(kNow + 10 * kDay, kNow, 30, 180), kNow + 40 * kDay);
	EXPECT_EQ(afk_pass::RenewedExpiry(kNow + 1, kNow, 30, 180), kNow + 1 + 30 * kDay) << "还剩一秒也接着往后叠";
}

TEST(AfkPassRulesTest, StackLimitIsInclusiveAndNeverOverflows)
{
	EXPECT_EQ(afk_pass::RenewedExpiry(kNow + 150 * kDay, kNow, 30, 180), kNow + 180 * kDay) << "续完恰好等于上限:放行";
	EXPECT_FALSE(afk_pass::RenewedExpiry(kNow + 150 * kDay + 1, kNow, 30, 180)) << "超过上限一秒也拒绝";
	EXPECT_FALSE(afk_pass::RenewedExpiry(kNow + 200 * kDay, kNow, 30, 180)) << "上限调小之后,原到期已超上限:不能再续";
	EXPECT_FALSE(afk_pass::RenewedExpiry(std::numeric_limits<uint64_t>::max(), kNow, 30, 180))
		<< "坏数据:先比剩余,不做会溢出的加法";
	EXPECT_EQ(afk_pass::RenewedExpiry(0, kNow, 30, 30), kNow + 30 * kDay) << "上限等于一次的天数:能开、不能叠";
	EXPECT_FALSE(afk_pass::RenewedExpiry(kNow + 1, kNow, 30, 30));
}

TEST(AfkPassRulesTest, RuleBounds)
{
	EXPECT_EQ(afk_pass::RuleProblem(GoodRule()), nullptr);

	const auto expectProblem = [](afk_pass::Rule rule, const char *what) {
		EXPECT_NE(afk_pass::RuleProblem(rule), nullptr) << what;
	};
	auto rule = GoodRule();
	rule.periodDays = 0;
	expectProblem(rule, "period_days = 0");
	rule = GoodRule();
	rule.periodDays = afk_pass::kMaxPeriodDays + 1;
	expectProblem(rule, "period_days > 366");
	rule = GoodRule();
	rule.priceCurrencyType = static_cast<uint32_t>(kCurrencyMax);
	expectProblem(rule, "unknown currency");
	rule = GoodRule();
	rule.price = 0;
	expectProblem(rule, "price = 0");
	rule = GoodRule();
	rule.price = afk_pass::kMaxPrice + 1;
	expectProblem(rule, "price > 1e9");
	rule = GoodRule();
	rule.maxRemainingDays = rule.periodDays - 1;
	expectProblem(rule, "max_remaining_days < period_days");
	rule = GoodRule();
	rule.maxRemainingDays = afk_pass::kMaxRemainingDaysCap + 1;
	expectProblem(rule, "max_remaining_days > 3660");
	rule = GoodRule();
	rule.remindBeforeHours = afk_pass::kMaxRemindBeforeHours + 1;
	expectProblem(rule, "remind_before_hours > 720");

	rule = GoodRule();
	rule.maxRemainingDays = rule.periodDays;
	rule.remindBeforeHours = 0;
	EXPECT_EQ(afk_pass::RuleProblem(rule), nullptr) << "只能开不能叠、不提醒:都是合法配置";
}

TEST(AfkPassRulesTest, GmExpiryAndProjectionShape)
{
	EXPECT_EQ(afk_pass::GmExpiry(kNow, 0), 0u) << "0 = 清除,与从未开通同一个值";
	EXPECT_EQ(afk_pass::GmExpiry(kNow, 120), kNow + 120);
	EXPECT_EQ(afk_pass::GmExpiry(kNow, afk_pass::kGmMaxRemainingSeconds), kNow + 366 * kDay);

	EXPECT_TRUE(afk_pass::ProjectionIsSet(kNow + 1, kNow));
	EXPECT_FALSE(afk_pass::ProjectionIsSet(kNow, kNow)) << "到期那一秒就不再有效,与读方「now < expiry」同口径";
	EXPECT_FALSE(afk_pass::ProjectionIsSet(0, kNow));
}

// ── AfkPassSystem ───────────────────────────────────────────────────────

namespace
{
constexpr Guid kPlayerGuid = 960100001;

struct ProjectionWrite
{
	Guid playerId{0};
	uint64_t expiresAtSec{0};
	bool set{false};
};
} // namespace

class AfkPassSystemTest : public testing::Test
{
protected:
	void SetUp() override
	{
		AfkPassRuleTableManager::Instance().Load();
		ASSERT_GT(AfkPassRuleTableManager::Instance().Count(), 0u)
			<< "AfkPassRule has not been exported: run tools/scripts/afk_pass_xlsx_patch.py new-tables, then export the tables";
		const auto &row = *AfkPassRuleTableManager::Instance().FindAll().data().begin();
		rule = afk_pass::Rule{row.period_days(), row.price_currency_type(), row.price(), row.max_remaining_days(),
							  row.remind_before_hours()};
		ASSERT_EQ(afk_pass::RuleProblem(rule), nullptr) << "the first AfkPassRule row is not usable";
		currency = static_cast<CurrencyType>(rule.priceCurrencyType);
		AfkPassSystem::SetProjectionSenderForTest(
			[this](Guid playerId, uint64_t expiresAtSec, bool set) { projections.push_back({playerId, expiresAtSec, set}); });
		player = NewPlayer(kPlayerGuid, rule.price * 3);
	}

	void TearDown() override
	{
		AfkPassSystem::SetProjectionSenderForTest(nullptr);
		tlsEcs.playerList.erase(kPlayerGuid);
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
	}

	// 生产侧这几个持久组件由 PlayerDatabaseMessageFieldsUnmarshal 无条件挂上。
	entt::entity NewPlayer(Guid guid, uint64_t balance)
	{
		const auto entity = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, guid);
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(entity).epoch = 1;
		auto &values = tlsEcs.actorRegistry.emplace<CurrencyComp>(entity);
		while (values.values_size() < static_cast<int>(kCurrencyMax))
		{
			values.add_values(0);
		}
		values.set_values(static_cast<int>(currency), balance);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(entity);
		tlsEcs.actorRegistry.emplace<PlayerUint64Comp>(entity);
		tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(entity);
		tlsEcs.playerList.emplace(guid, entity);
		return entity;
	}

	::BuyAfkPassRequest Request(uint64_t key) const
	{
		::BuyAfkPassRequest request;
		request.set_client_request_id(key);
		request.set_expected_currency_type(rule.priceCurrencyType);
		request.set_expected_price(rule.price);
		return request;
	}

	uint64_t Balance() const { return CurrencySystem::GetBalance(player, currency); }
	uint64_t Expiry() const { return tlsEcs.actorRegistry.get<PlayerUint64Comp>(player).afk_pass_expires_at(); }
	void SetExpiry(uint64_t value) { tlsEcs.actorRegistry.get<PlayerUint64Comp>(player).set_afk_pass_expires_at(value); }
	uint64_t Period() const { return static_cast<uint64_t>(rule.periodDays) * kDay; }

	afk_pass::Rule rule;
	CurrencyType currency{kCurrencyDiamond};
	entt::entity player{entt::null};
	std::vector<ProjectionWrite> projections;
};

TEST_F(AfkPassSystemTest, BuyChargesOnceAndExtendsFromNow)
{
	const uint64_t before = Balance();
	::BuyAfkPassResponse response;
	ASSERT_EQ(AfkPassSystem::Buy(player, Request(7001), kNow, response), kSuccess);
	EXPECT_EQ(Balance(), before - rule.price);
	EXPECT_EQ(Expiry(), kNow + Period());
	EXPECT_FALSE(response.replayed());
	EXPECT_EQ(response.info().expires_at(), kNow + Period());
	EXPECT_EQ(response.info().server_now(), kNow);
	EXPECT_EQ(response.info().price(), rule.price);
	EXPECT_EQ(response.info().balance(), before - rule.price);
	EXPECT_TRUE(projections.empty()) << "投影只写已提交的值:开通当场不写,等存盘提交回来";
}

TEST_F(AfkPassSystemTest, ReplayWithTheSameKeyDoesNotChargeAgain)
{
	ASSERT_GE(rule.maxRemainingDays, 2 * rule.periodDays)
		<< "fixture precondition: AfkPassRule must allow stacking two periods (max_remaining_days >= 2 * period_days)";
	::BuyAfkPassResponse first;
	ASSERT_EQ(AfkPassSystem::Buy(player, Request(7002), kNow, first), kSuccess);
	const uint64_t balanceAfterFirst = Balance();

	::BuyAfkPassResponse replay;
	ASSERT_EQ(AfkPassSystem::Buy(player, Request(7002), kNow + 5, replay), kSuccess);
	EXPECT_TRUE(replay.replayed());
	EXPECT_EQ(Balance(), balanceAfterFirst) << "同一个键至多扣一次钱";
	EXPECT_EQ(Expiry(), kNow + Period()) << "也只加一次天数";
	EXPECT_EQ(replay.info().expires_at(), kNow + Period());

	auto changed = Request(7002);
	changed.set_expected_price(rule.price + 1);
	::BuyAfkPassResponse mismatch;
	EXPECT_EQ(AfkPassSystem::Buy(player, changed, kNow + 6, mismatch), kInvalidParameter) << "旧键用在了参数不同的请求上";
	EXPECT_EQ(Balance(), balanceAfterFirst);

	::BuyAfkPassResponse second;
	ASSERT_EQ(AfkPassSystem::Buy(player, Request(7003), kNow + 10, second), kSuccess) << "新键是新的一次开通";
	EXPECT_EQ(Expiry(), kNow + 2 * Period()) << "没到期就接着往后叠";
	EXPECT_EQ(Balance(), balanceAfterFirst - rule.price);
}

TEST_F(AfkPassSystemTest, RejectionsChangeNothing)
{
	const uint64_t before = Balance();
	::BuyAfkPassResponse response;
	EXPECT_EQ(AfkPassSystem::Buy(player, Request(0), kNow, response), kInvalidParameter) << "新接口的键必须非 0";

	auto stale = Request(7101);
	stale.set_expected_price(rule.price + 1);
	EXPECT_EQ(AfkPassSystem::Buy(player, stale, kNow, response), kAfkPassPriceChanged);
	auto otherCurrency = Request(7102);
	otherCurrency.set_expected_currency_type(rule.priceCurrencyType + 1);
	EXPECT_EQ(AfkPassSystem::Buy(player, otherCurrency, kNow, response), kAfkPassPriceChanged);

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(AfkPassSystem::Buy(player, Request(7103), kNow, response), kAfkPassFrozen);
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);

	tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
	EXPECT_EQ(AfkPassSystem::Buy(player, Request(7103), kNow, response), kAfkPassFrozen);
	tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(player);

	tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
	EXPECT_EQ(AfkPassSystem::Buy(player, Request(7103), kNow, response), kAfkPassFrozen);
	tlsEcs.actorRegistry.remove<UnregisterPlayer>(player);

	SetExpiry(kNow + static_cast<uint64_t>(rule.maxRemainingDays) * kDay);
	EXPECT_EQ(AfkPassSystem::Buy(player, Request(7104), kNow, response), kAfkPassStackLimit);
	EXPECT_EQ(Expiry(), kNow + static_cast<uint64_t>(rule.maxRemainingDays) * kDay);
	SetExpiry(0);

	tlsEcs.actorRegistry.get<CurrencyComp>(player).set_values(static_cast<int>(currency), rule.price - 1);
	EXPECT_EQ(AfkPassSystem::Buy(player, Request(7105), kNow, response), kAfkPassCurrencyNotEnough);
	EXPECT_EQ(Balance(), rule.price - 1);
	EXPECT_EQ(Expiry(), 0u);
	tlsEcs.actorRegistry.get<CurrencyComp>(player).set_values(static_cast<int>(currency), before);

	EXPECT_EQ(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).records_size(), 0) << "拒绝不记键";

	// 拒绝过的键没有被记下,修好条件之后同一个键照常开通。
	ASSERT_EQ(AfkPassSystem::Buy(player, Request(7105), kNow, response), kSuccess);
	EXPECT_EQ(Balance(), before - rule.price);
}

TEST_F(AfkPassSystemTest, MissingSaveFieldIsRefusedInsteadOfRecreated)
{
	tlsEcs.actorRegistry.remove<PlayerUint64Comp>(player);
	const uint64_t before = Balance();
	::BuyAfkPassResponse response;
	EXPECT_EQ(AfkPassSystem::Buy(player, Request(7201), kNow, response), kAfkPassUnavailable);
	EXPECT_EQ(Balance(), before);
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerUint64Comp>(player)) << "持久组件缺失不现造(那等于把月卡清零)";
	EXPECT_EQ(AfkPassSystem::ExpiresAt(player), 0u);
}

TEST_F(AfkPassSystemTest, GmSetsRemainingTimeAndClears)
{
	::AfkPassInfo info;
	ASSERT_EQ(AfkPassSystem::GmSet(player, 120, kNow, info), kSuccess);
	EXPECT_EQ(Expiry(), kNow + 120);
	EXPECT_EQ(info.expires_at(), kNow + 120);

	ASSERT_EQ(AfkPassSystem::GmSet(player, 60, kNow, info), kSuccess);
	EXPECT_EQ(Expiry(), kNow + 60) << "设定而不是增加:可以缩短";

	ASSERT_EQ(AfkPassSystem::GmSet(player, 0, kNow, info), kSuccess);
	EXPECT_EQ(Expiry(), 0u);

	EXPECT_EQ(AfkPassSystem::GmSet(player, afk_pass::kGmMaxRemainingSeconds + 1, kNow, info), kInvalidParameter);
	EXPECT_EQ(Expiry(), 0u);
}

TEST_F(AfkPassSystemTest, GmSetRespectsPlayerMutabilityGuard)
{
	::AfkPassInfo info;
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(AfkPassSystem::GmSet(player, 120, kNow, info), kAfkPassFrozen);
	EXPECT_EQ(Expiry(), 0u);
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);

	tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
	EXPECT_EQ(AfkPassSystem::GmSet(player, 120, kNow, info), kAfkPassFrozen);
	EXPECT_EQ(Expiry(), 0u);
	tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(player);

	tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player);
	EXPECT_EQ(AfkPassSystem::GmSet(player, 120, kNow, info), kAfkPassFrozen);
	EXPECT_EQ(Expiry(), 0u);
	tlsEcs.actorRegistry.remove<UnregisterPlayer>(player);

	tlsEcs.actorRegistry.remove<PlayerOwnerEpochComp>(player);
	EXPECT_EQ(AfkPassSystem::GmSet(player, 120, kNow, info), kAfkPassFrozen);
	EXPECT_EQ(Expiry(), 0u);
}

TEST_F(AfkPassSystemTest, ValidateTablesAcceptsExportedRule)
{
	EXPECT_TRUE(AfkPassSystem::ValidateTables());
}

TEST_F(AfkPassSystemTest, InfoCarriesPriceAndServerTime)
{
	SetExpiry(kNow + 3600);
	::AfkPassInfo info;
	AfkPassSystem::FillInfo(player, kNow, info);
	EXPECT_EQ(info.expires_at(), kNow + 3600);
	EXPECT_EQ(info.server_now(), kNow);
	EXPECT_EQ(info.price_currency_type(), rule.priceCurrencyType);
	EXPECT_EQ(info.price(), rule.price);
	EXPECT_EQ(info.period_days(), rule.periodDays);
	EXPECT_EQ(info.max_remaining_days(), rule.maxRemainingDays);
	EXPECT_EQ(info.remind_before_hours(), rule.remindBeforeHours);
	EXPECT_EQ(info.balance(), Balance());
}

TEST_F(AfkPassSystemTest, ProjectionIsRebuiltOnLoadAndFollowsCommits)
{
	// 加载:按库里的值写一次。没开通 → DEL(回档或残留的投影在这里被改正)。
	AfkPassSystem::OnPlayerLoaded(player, kNow);
	ASSERT_EQ(projections.size(), 1u);
	EXPECT_EQ(projections[0].playerId, kPlayerGuid);
	EXPECT_FALSE(projections[0].set);

	// 提交回来的值与上一次已提交的相同:不写。
	PlayerAllData committed;
	AfkPassSystem::OnSaveCommitted(kPlayerGuid, committed, kNow);
	EXPECT_EQ(projections.size(), 1u) << "月卡没变,每次存盘都写一遍是白占 Redis";

	// 开通之后的那一笔提交回来:写 SET。
	committed.mutable_player_database_data()->mutable_uint64_pb_component()->set_afk_pass_expires_at(kNow + Period());
	AfkPassSystem::OnSaveCommitted(kPlayerGuid, committed, kNow + 1);
	ASSERT_EQ(projections.size(), 2u);
	EXPECT_TRUE(projections[1].set);
	EXPECT_EQ(projections[1].expiresAtSec, kNow + Period());

	// 同一个值的下一笔提交:不写。GM 清除的那一笔:写 DEL。
	AfkPassSystem::OnSaveCommitted(kPlayerGuid, committed, kNow + 2);
	EXPECT_EQ(projections.size(), 2u);
	committed.mutable_player_database_data()->mutable_uint64_pb_component()->set_afk_pass_expires_at(0);
	AfkPassSystem::OnSaveCommitted(kPlayerGuid, committed, kNow + 3);
	ASSERT_EQ(projections.size(), 3u);
	EXPECT_FALSE(projections[2].set);
}

TEST_F(AfkPassSystemTest, ProjectionWritesOnlyCommittedValues)
{
	SetExpiry(kNow + 3600);
	AfkPassSystem::OnPlayerLoaded(player, kNow);
	ASSERT_EQ(projections.size(), 1u);
	EXPECT_TRUE(projections[0].set) << "加载时已开通:SET";
	EXPECT_EQ(projections[0].expiresAtSec, kNow + 3600);

	// 内存改了、还没提交:心跳补写的仍是已提交的值,投影不领先于库。
	SetExpiry(kNow + 7200);
	AfkPassSystem::RefreshProjection(player, kNow + 10);
	ASSERT_EQ(projections.size(), 2u);
	EXPECT_TRUE(projections[1].set);
	EXPECT_EQ(projections[1].expiresAtSec, kNow + 3600);

	// 已提交的到期之后,心跳不写(只补 SET,DEL 由提交与加载写)。
	AfkPassSystem::RefreshProjection(player, kNow + 3600);
	EXPECT_EQ(projections.size(), 2u);
}

TEST_F(AfkPassSystemTest, EntitiesNotBuiltByTheLoadChainWriteNoProjection)
{
	// 没有经过 OnPlayerLoaded(没有已提交的基线):提交回调与心跳都不写,交给下一个持有者加载时写。
	PlayerAllData committed;
	committed.mutable_player_database_data()->mutable_uint64_pb_component()->set_afk_pass_expires_at(kNow + 3600);
	AfkPassSystem::OnSaveCommitted(kPlayerGuid, committed, kNow);
	AfkPassSystem::RefreshProjection(player, kNow);
	EXPECT_TRUE(projections.empty());

	AfkPassSystem::OnSaveCommitted(kPlayerGuid + 1, committed, kNow); // 本节点没有这名玩家
	EXPECT_TRUE(projections.empty());
}

TEST_F(AfkPassSystemTest, FailedProjectionSetAndDelSelfHealOnLaterCommitOrRefresh)
{
	AfkPassSystem::OnPlayerLoaded(player, kNow);
	ASSERT_EQ(projections.size(), 1u);

	// 1) 开通提交那一次 Redis 写失败(未确认):下一次存盘即便月卡到期值未变,仍补发 SET 自愈。
	PlayerAllData committed;
	committed.mutable_player_database_data()->mutable_uint64_pb_component()->set_afk_pass_expires_at(kNow + Period());
	AfkPassSystem::SetProjectionFailForTest(true);
	AfkPassSystem::OnSaveCommitted(kPlayerGuid, committed, kNow + 1);
	ASSERT_EQ(projections.size(), 2u);
	EXPECT_TRUE(projections[1].set);

	AfkPassSystem::SetProjectionFailForTest(false);
	AfkPassSystem::OnSaveCommitted(kPlayerGuid, committed, kNow + 2);
	ASSERT_EQ(projections.size(), 3u) << "上次 SET 未确认:相同值也必须重试自愈";
	EXPECT_TRUE(projections[2].set);

	AfkPassSystem::OnSaveCommitted(kPlayerGuid, committed, kNow + 3);
	EXPECT_EQ(projections.size(), 3u) << "已确认后相同值不再重复写 Redis";

	// 2) 清除(DEL)写失败:后续 RefreshProjection 也会补发 DEL 清掉 Redis 脏键,确认后停止。
	committed.mutable_player_database_data()->mutable_uint64_pb_component()->set_afk_pass_expires_at(0);
	AfkPassSystem::SetProjectionFailForTest(true);
	AfkPassSystem::OnSaveCommitted(kPlayerGuid, committed, kNow + 4);
	ASSERT_EQ(projections.size(), 4u);
	EXPECT_FALSE(projections[3].set);

	AfkPassSystem::SetProjectionFailForTest(false);
	AfkPassSystem::RefreshProjection(player, kNow + 5);
	ASSERT_EQ(projections.size(), 5u) << "未确认的 DEL 必须通过 RefreshProjection 自愈,不能残留旧键";
	EXPECT_FALSE(projections[4].set);

	AfkPassSystem::RefreshProjection(player, kNow + 6);
	EXPECT_EQ(projections.size(), 5u) << "DEL 确认后不再重复发";
}

TEST_F(AfkPassSystemTest, SettingsAndHostedRecapLifecycleSurviveCrossNodeAndAckIdempotently)
{
	AfkPassInfo info;
	// 1. 冻结态拒改偏好设置
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(AfkPassSystem::SetSettings(player, true, kNow, info), kAfkPassFrozen);
	EXPECT_FALSE(AfkPassSystem::AutoReturnCityOnTeamEnd(player));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);

	// 2. 正常修改偏好并回填 FillInfo
	EXPECT_EQ(AfkPassSystem::SetSettings(player, true, kNow, info), kSuccess);
	EXPECT_TRUE(info.auto_return_city_on_team_end());
	EXPECT_TRUE(AfkPassSystem::AutoReturnCityOnTeamEnd(player));

	// 3. 未进入托管时的战斗结算不计入离线托管收获简报
	AfkPassSystem::RecordHostedBattleSettlement(player, true, 500, 1000, 3, kNow + 10);
	AfkPassSystem::FillInfo(player, kNow + 10, info);
	EXPECT_FALSE(info.has_hosted_recap());

	// 4. 开始托管并累计战斗收益
	auto& held = tlsEcs.actorRegistry.emplace<PlayerConnectionLostComp>(player);
	held.hostingStarted = true;
	AfkPassSystem::OnHostingStarted(player, kNow + 100);
	AfkPassSystem::RecordHostedBattleSettlement(player, true, 1200, 3500, 2, kNow + 130);
	AfkPassSystem::RecordHostedBattleSettlement(player, false, 400, 500, 1, kNow + 190);

	// 5. 跨节点托管到达再次调用 OnHostingStarted:沿用原起点与已累计收获,不归零
	AfkPassSystem::OnHostingStarted(player, kNow + 250);
	AfkPassSystem::RecordHostedBattleSettlement(player, true, 800, 2000, 4, kNow + 300);

	// 6. 重连接回:封存 ended_at 并置 pending_ack
	tlsEcs.actorRegistry.remove<PlayerConnectionLostComp>(player);
	AfkPassSystem::OnHostingEnded(player, kNow + 600, /*requestSave=*/false);
	AfkPassSystem::FillInfo(player, kNow + 605, info);
	ASSERT_TRUE(info.has_hosted_recap());
	EXPECT_EQ(info.hosted_recap().started_at(), kNow + 100);
	EXPECT_EQ(info.hosted_recap().ended_at(), kNow + 600);
	EXPECT_EQ(info.hosted_recap().battle_count(), 3u);
	EXPECT_EQ(info.hosted_recap().win_count(), 2u);
	EXPECT_EQ(info.hosted_recap().exp_gained(), 2400u);
	EXPECT_EQ(info.hosted_recap().silver_gained(), 6000u);
	EXPECT_EQ(info.hosted_recap().item_count(), 7u);
	EXPECT_TRUE(info.hosted_recap().pending_ack());

	// 7. started_at 不匹配的旧确认不误清当前简报;匹配后清除且再次确认幂等
	EXPECT_EQ(AfkPassSystem::AckHostedRecap(player, kNow + 1, kNow + 610, info), kSuccess);
	ASSERT_TRUE(info.has_hosted_recap());
	EXPECT_TRUE(info.hosted_recap().pending_ack());

	EXPECT_EQ(AfkPassSystem::AckHostedRecap(player, kNow + 100, kNow + 615, info), kSuccess);
	EXPECT_FALSE(info.has_hosted_recap());
	EXPECT_EQ(AfkPassSystem::AckHostedRecap(player, kNow + 100, kNow + 620, info), kSuccess);
	EXPECT_FALSE(info.has_hosted_recap());
}

TEST_F(AfkPassSystemTest, LightCardPopulatesLoadoutSlotsAndStage2TradableFlag)
{
	AfkPassInfo info;
	AfkPassSystem::FillInfo(player, kNow, info);
	EXPECT_EQ(info.unlocked_loadout_slots(), 3u);
	EXPECT_FALSE(info.stage2_tradable_enabled());

	::BuyAfkPassResponse bought;
	ASSERT_EQ(AfkPassSystem::Buy(player, Request(7801), kNow, bought), kSuccess);
	EXPECT_EQ(bought.info().unlocked_loadout_slots(), 6u);
	EXPECT_TRUE(bought.info().stage2_tradable_enabled());

	ASSERT_EQ(AfkPassSystem::GmSet(player, 0, kNow + 5, info), kSuccess);
	EXPECT_EQ(info.unlocked_loadout_slots(), 3u);
	EXPECT_FALSE(info.stage2_tradable_enabled());
}

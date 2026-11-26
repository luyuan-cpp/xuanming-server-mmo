// 零拒审激励视频广告系统(docs/design/rewarded-ad-system.md)单测:
//   * RewardedAdRulesTest —— 切日(UTC+8 05:00)抗回拨、平台掩码、仅发绑定银两强校验、会话时长与过期判定。
//   * PlayerRewardedAdTest —— 首审默认关闭态、平台掩码隔离、10 级门槛、完整开播->领奖(发绑定银两)、
//     too_fast / expired / invalid 会话拦截、30 秒断线重连补领、幂等重放与错键拒绝、日限 2 次 + 冷却 60 秒 + 跨日重置、封禁拦截。

#include <gtest/gtest.h>

#include <cstdint>
#include <memory>

#include "engine/core/type_define/player_guid_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "proto/common/base/currency_type.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/scene/player_rewarded_ad.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/player_rewarded_ad.h"
#include "services/scene/player/system/rewarded_ad_rules.h"
#include "table/code/rewardedadslot_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/rewarded_ad_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
constexpr uint64_t kPlayerGuid = 991201;
// 2026-11-26 04:00:00 UTC = 2026-11-26 12:00:00 UTC+8
constexpr uint64_t kNowMs = 1795579200000ULL;

std::shared_ptr<RewardedAdSlotTableSnapshot> BuildSlotSnapshot(bool enabled, uint32_t currencyType = 3,
															   uint32_t requireSsv = 0, uint32_t platformMask = 3)
{
	auto snap = std::make_shared<RewardedAdSlotTableSnapshot>();
	auto *row = snap->data.add_data();
	row->set_id(1);
	row->set_enabled(enabled ? 1u : 0u);
	row->set_min_level(10);
	row->set_daily_limit(2);
	row->set_cooldown_sec(60);
	row->set_min_watch_sec(15);
	row->set_session_ttl_sec(300);
	row->set_currency_type(currencyType);
	row->set_reward_amount(300);
	row->set_require_ssv(requireSsv);
	row->set_platform_mask(platformMask);
	snap->idMap[1] = row;
	return snap;
}

class PlayerRewardedAdTest : public ::testing::Test
{
protected:
	void SetUp() override
	{
		prevSnapshot_ = RewardedAdSlotTableManager::Instance().CurrentSnapshot();
		player_ = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player_).value = kPlayerGuid;
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player_).epoch = 1;
		tlsEcs.actorRegistry.emplace<LevelComp>(player_).set_level(10);
		tlsEcs.actorRegistry.emplace<CurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player_);
	}

	void TearDown() override
	{
		if (tlsEcs.actorRegistry.valid(player_))
		{
			tlsEcs.actorRegistry.destroy(player_);
		}
		if (prevSnapshot_)
		{
			RewardedAdSlotTableManager::Instance().InstallSnapshot(prevSnapshot_);
		}
		GainBlockService::ClearAllGlobalBlocks();
	}

	entt::entity player_{entt::null};
	std::shared_ptr<const RewardedAdSlotTableSnapshot> prevSnapshot_;
};
} // namespace

TEST(RewardedAdRulesTest, StrictBindGoldOnlyAndAntiClockRollback)
{
	rewarded_ad_rules::SlotConfig cfg;
	cfg.slotId = 1;
	cfg.enabled = true;
	cfg.minLevel = 10;
	cfg.dailyLimit = 2;
	cfg.cooldownSec = 60;
	cfg.minWatchSec = 15;
	cfg.sessionTtlSec = 300;
	cfg.currencyType = static_cast<uint32_t>(common::base::CURRENCY_TYPE_BIND_GOLD);
	cfg.rewardAmount = 300;
	cfg.platformMask = 3;

	const char *reason = nullptr;
	EXPECT_TRUE(rewarded_ad_rules::ValidateSlotConfig(cfg, reason));

	// 严禁误配为流通银两或真灵石。
	cfg.currencyType = static_cast<uint32_t>(common::base::CURRENCY_TYPE_GOLD);
	EXPECT_FALSE(rewarded_ad_rules::ValidateSlotConfig(cfg, reason));
	cfg.currencyType = static_cast<uint32_t>(common::base::CURRENCY_TYPE_DIAMOND);
	EXPECT_FALSE(rewarded_ad_rules::ValidateSlotConfig(cfg, reason));

	// 切日与抗回拨校验。
	uint64_t dayStart = 0;
	uint64_t nextDay = 0;
	ASSERT_TRUE(rewarded_ad_rules::ResolveGameDay(kNowMs, dayStart, nextDay));
	EXPECT_EQ(dayStart + rewarded_ad_rules::kDayMs, nextDay);

	RewardedAdLedger ledger;
	ledger.set_day_starts_at_ms(dayStart);
	ledger.set_claimed_today(2);
	EXPECT_EQ(2u, rewarded_ad_rules::EffectiveClaimedToday(ledger, dayStart));
	EXPECT_EQ(2u, rewarded_ad_rules::EffectiveClaimedToday(ledger, dayStart - rewarded_ad_rules::kDayMs));
	EXPECT_EQ(0u, rewarded_ad_rules::EffectiveClaimedToday(ledger, nextDay));
}

TEST_F(PlayerRewardedAdTest, DefaultDisabledAndInvalidCurrencyFailClosed)
{
	// 1. 首审默认态(enabled=0)：GetInfo 返回 enabled=false，BeginSession 拒绝。
	RewardedAdSlotTableManager::Instance().InstallSnapshot(BuildSlotSnapshot(/*enabled=*/false));
	EXPECT_TRUE(PlayerRewardedAdSystem::ValidateTables());

	GetRewardedAdInfoRequest infoReq;
	infoReq.set_client_platform(AD_CLIENT_PLATFORM_WECHAT_MINIGAME);
	GetRewardedAdInfoResponse infoResp;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::GetInfo(player_, infoReq, kNowMs, infoResp));
	ASSERT_EQ(1, infoResp.slots_size());
	EXPECT_FALSE(infoResp.slots(0).enabled());

	BeginRewardedAdSessionRequest beginReq;
	beginReq.set_slot_id(1);
	beginReq.set_client_platform(AD_CLIENT_PLATFORM_WECHAT_MINIGAME);
	BeginRewardedAdSessionResponse beginResp;
	EXPECT_EQ(kRewardedAdDisabled, PlayerRewardedAdSystem::BeginSession(player_, beginReq, kNowMs, beginResp));

	// 2. 表中误配成流通银两(0)：ValidateTables 返回 false 且运行时直接拒载。
	RewardedAdSlotTableManager::Instance().InstallSnapshot(BuildSlotSnapshot(/*enabled=*/true, /*currencyType=*/0));
	EXPECT_FALSE(PlayerRewardedAdSystem::ValidateTables());
	EXPECT_EQ(kRewardedAdUnavailable, PlayerRewardedAdSystem::BeginSession(player_, beginReq, kNowMs, beginResp));
}

TEST_F(PlayerRewardedAdTest, FullLifecycleBindGoldIdempotencyReconnectAndLimits)
{
	RewardedAdSlotTableManager::Instance().InstallSnapshot(BuildSlotSnapshot(/*enabled=*/true));
	ASSERT_TRUE(PlayerRewardedAdSystem::ValidateTables());

	// 1. 平台掩码隔离(默认 platform_mask=3 仅小游戏)：iOS/Android App 端被拒，微信/抖音放行。
	BeginRewardedAdSessionRequest beginReq;
	beginReq.set_slot_id(1);
	beginReq.set_client_platform(AD_CLIENT_PLATFORM_IOS_APP);
	BeginRewardedAdSessionResponse beginResp;
	EXPECT_EQ(kRewardedAdPlatformUnsupported, PlayerRewardedAdSystem::BeginSession(player_, beginReq, kNowMs, beginResp));

	// 2. 10 级门槛拦截白号。
	tlsEcs.actorRegistry.get<LevelComp>(player_).set_level(9);
	beginReq.set_client_platform(AD_CLIENT_PLATFORM_DOUYIN_MINIGAME);
	EXPECT_EQ(kRewardedAdLevelNotEnough, PlayerRewardedAdSystem::BeginSession(player_, beginReq, kNowMs, beginResp));
	tlsEcs.actorRegistry.get<LevelComp>(player_).set_level(10);

	// 3. 签发第 1 次会话，校验 too_fast (<15s) 拒绝，满 35 秒(模拟 30s 广告断线重连后补领)成功发 300 绑定银两。
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::BeginSession(player_, beginReq, kNowMs, beginResp));
	const uint64_t session1 = beginResp.ad_session_id();
	ASSERT_NE(0u, session1);

	ClaimRewardedAdRequest claimReq;
	claimReq.set_client_request_id(10001);
	claimReq.set_slot_id(1);
	claimReq.set_ad_session_id(session1);
	ClaimRewardedAdResponse claimResp;
	EXPECT_EQ(kRewardedAdWatchTooShort, PlayerRewardedAdSystem::Claim(player_, claimReq, kNowMs + 5000, claimResp));

	// 模拟落盘后实体重载(WALLET 组往返序列化)。
	const std::string savedLedger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player_).SerializeAsString();
	tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player_).Clear();
	ASSERT_TRUE(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player_).ParseFromString(savedLedger));

	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::Claim(player_, claimReq, kNowMs + 35000, claimResp));
	EXPECT_FALSE(claimResp.replayed());
	EXPECT_EQ(static_cast<uint32_t>(kCurrencyBindGold), claimResp.currency_type());
	EXPECT_EQ(300u, claimResp.reward_amount());
	EXPECT_EQ(300u, CurrencySystem::GetBalance(player_, kCurrencyBindGold));
	EXPECT_EQ(0u, CurrencySystem::GetBalance(player_, kCurrencyGold)); // 流通银两必须为 0

	// 4. 同幂等键重放不双发；换参数错键拒回 kInvalidParameter。
	ClaimRewardedAdResponse replayResp;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::Claim(player_, claimReq, kNowMs + 36000, replayResp));
	EXPECT_TRUE(replayResp.replayed());
	EXPECT_EQ(300u, replayResp.reward_amount());
	EXPECT_EQ(300u, CurrencySystem::GetBalance(player_, kCurrencyBindGold));

	ClaimRewardedAdRequest mismatchReq = claimReq;
	mismatchReq.set_ad_session_id(session1 + 1);
	EXPECT_EQ(kInvalidParameter, PlayerRewardedAdSystem::Claim(player_, mismatchReq, kNowMs + 36000, replayResp));

	// 5. 冷却期(60s)拦截；满冷却后完成第 2 次领取；第 3 次触发每日上限；跨日(UTC+8 05:00)重置可再领。
	EXPECT_EQ(kRewardedAdCooldown, PlayerRewardedAdSystem::BeginSession(player_, beginReq, kNowMs + 50000, beginResp));
	const uint64_t secondStartMs = kNowMs + 100000;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::BeginSession(player_, beginReq, secondStartMs, beginResp));
	const uint64_t session2 = beginResp.ad_session_id();

	ClaimRewardedAdRequest claim2;
	claim2.set_client_request_id(10002);
	claim2.set_slot_id(1);
	claim2.set_ad_session_id(session2);
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::Claim(player_, claim2, secondStartMs + 20000, claimResp));
	EXPECT_EQ(600u, CurrencySystem::GetBalance(player_, kCurrencyBindGold));
	EXPECT_EQ(2u, claimResp.slot().claimed_today());

	EXPECT_EQ(kRewardedAdDailyLimitReached,
			  PlayerRewardedAdSystem::BeginSession(player_, beginReq, secondStartMs + 120000, beginResp));

	// 跨过 UTC+8 05:00 到次日，且校验过期会话(TTL > 300s)拦截。
	const uint64_t nextDayMs = kNowMs + rewarded_ad_rules::kDayMs;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::BeginSession(player_, beginReq, nextDayMs, beginResp));
	ClaimRewardedAdRequest expiredClaim;
	expiredClaim.set_client_request_id(10003);
	expiredClaim.set_slot_id(1);
	expiredClaim.set_ad_session_id(beginResp.ad_session_id());
	EXPECT_EQ(kRewardedAdSessionExpired,
			  PlayerRewardedAdSystem::Claim(player_, expiredClaim, nextDayMs + 301000, claimResp));

	// 6. 绑定银两被封禁时在 BeginSession 即拦截，避免玩家白看广告。
	ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player_, kCurrencyBindGold));
	EXPECT_EQ(kRewardedAdCurrencyBlocked, PlayerRewardedAdSystem::BeginSession(player_, beginReq, nextDayMs + 1000, beginResp));
}

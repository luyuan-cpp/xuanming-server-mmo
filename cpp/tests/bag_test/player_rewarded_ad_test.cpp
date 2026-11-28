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
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_mystery_merchant_comp.pb.h"
#include "proto/common/component/player_season_comp.pb.h"
#include "proto/scene/player_mystery_merchant.pb.h"
#include "proto/scene/player_rewarded_ad.pb.h"
#include "proto/scene/scene_info.pb.h"
#include <modules/scene/comp/scene_comp.h>
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/player_appearance.h"
#include "services/scene/player/system/player_mystery_merchant.h"
#include "services/scene/player/system/player_rewarded_ad.h"
#include "services/scene/player/system/player_season.h"
#include "services/scene/player/system/rewarded_ad_rules.h"
#include "table/code/appearancerule_table.h"
#include "table/code/characterappearance_table.h"
#include "table/code/item_table.h"
#include "table/code/mysterymerchantgoods_table.h"
#include "table/code/rewardedadslot_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mystery_merchant_error_tip.pb.h"
#include "table/proto/tip/rewarded_ad_error_tip.pb.h"
#include "table/proto/tip/season_error_tip.pb.h"
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

	// 切周(UTC+8 周一 05:00)与抗回拨校验。
	uint64_t weekStart = 0;
	uint64_t nextWeek = 0;
	ASSERT_TRUE(rewarded_ad_rules::ResolveGameWeek(kNowMs, weekStart, nextWeek));
	EXPECT_EQ(weekStart + rewarded_ad_rules::kWeekMs, nextWeek);

	RewardedAdLedger ledger;
	ledger.set_day_starts_at_ms(weekStart);
	ledger.set_claimed_today(2);
	EXPECT_EQ(2u, rewarded_ad_rules::EffectiveClaimedToday(ledger, weekStart));
	EXPECT_EQ(2u, rewarded_ad_rules::EffectiveClaimedToday(ledger, weekStart - rewarded_ad_rules::kWeekMs));
	EXPECT_EQ(0u, rewarded_ad_rules::EffectiveClaimedToday(ledger, nextWeek));
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

	// 同一周内次日不重置（防止变成每日打卡），跨过 UTC+8 周一 05:00 到下周后才重置，并校验过期会话(TTL > 300s)拦截。
	const uint64_t nextDaySameWeekMs = kNowMs + rewarded_ad_rules::kDayMs;
	EXPECT_EQ(kRewardedAdDailyLimitReached,
			  PlayerRewardedAdSystem::BeginSession(player_, beginReq, nextDaySameWeekMs, beginResp));

	const uint64_t nextWeekMs = kNowMs + rewarded_ad_rules::kWeekMs;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::BeginSession(player_, beginReq, nextWeekMs, beginResp));
	ClaimRewardedAdRequest expiredClaim;
	expiredClaim.set_client_request_id(10003);
	expiredClaim.set_slot_id(1);
	expiredClaim.set_ad_session_id(beginResp.ad_session_id());
	EXPECT_EQ(kRewardedAdSessionExpired,
			  PlayerRewardedAdSystem::Claim(player_, expiredClaim, nextWeekMs + 301000, claimResp));

	// 6. 绑定银两被封禁时在 BeginSession 即拦截，避免玩家白看广告。
	ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player_, kCurrencyBindGold));
	EXPECT_EQ(kRewardedAdCurrencyBlocked, PlayerRewardedAdSystem::BeginSession(player_, beginReq, nextWeekMs + 1000, beginResp));
}

TEST_F(PlayerRewardedAdTest, BatchB4_MultiKindSlots_TowerReroll_WardrobeTrial_MerchantRefresh)
{
	// 1. 校验非货币槽位禁止配置 reward_amount > 0。
	rewarded_ad_rules::SlotConfig badNonCurrency;
	badNonCurrency.slotId = 2;
	badNonCurrency.dailyLimit = 7;
	badNonCurrency.minWatchSec = 15;
	badNonCurrency.sessionTtlSec = 300;
	badNonCurrency.currencyType = static_cast<uint32_t>(common::base::CURRENCY_TYPE_BIND_GOLD);
	badNonCurrency.rewardAmount = 100;
	badNonCurrency.platformMask = 3;
	badNonCurrency.rewardKind = rewarded_ad_rules::kRewardTowerBoonReroll;
	const char *reason = nullptr;
	EXPECT_FALSE(rewarded_ad_rules::ValidateSlotConfig(badNonCurrency, reason));

	// 2. 装配 Slots 2..4（测试显式开启 enabled=1；正式表全部默认 0）。
	auto snap = std::make_shared<RewardedAdSlotTableSnapshot>();
	auto addSlot = [&](uint32_t id, uint32_t minLv, uint32_t limit, uint32_t kind) {
		auto *row = snap->data.add_data();
		row->set_id(id);
		row->set_enabled(1u);
		row->set_min_level(minLv);
		row->set_daily_limit(limit);
		row->set_cooldown_sec(1);
		row->set_min_watch_sec(15);
		row->set_session_ttl_sec(300);
		row->set_currency_type(3u);
		row->set_reward_amount(0u);
		row->set_require_ssv(0u);
		row->set_platform_mask(3u);
		row->set_reward_kind(kind);
		snap->idMap[id] = row;
	};
	addSlot(2u, 20u, 7u, rewarded_ad_rules::kRewardTowerBoonReroll);
	addSlot(3u, 10u, 7u, rewarded_ad_rules::kRewardWardrobe1hTrial);
	addSlot(4u, 10u, 5u, rewarded_ad_rules::kRewardMerchantPersonalRefresh);
	RewardedAdSlotTableManager::Instance().InstallSnapshot(snap);
	ASSERT_TRUE(PlayerRewardedAdSystem::ValidateTables());
	tlsEcs.actorRegistry.get<LevelComp>(player_).set_level(25u);

	// ── Slot 2：五行幻境 V2 奇策重随（M-7 硬锁每局 run_id 最多 1 次，0 资产产出）──
	auto &seasonComp = tlsEcs.actorRegistry.emplace<PlayerSeasonComp>(player_);
	auto *tower = seasonComp.mutable_wuxing_tower();
	tower->set_run_active(true);
	tower->set_run_id(77u);
	tower->set_current_floor(1u);
	tower->set_current_node_kind(1u);
	tower->add_pending_offer_sigil_ids(601u);
	tower->add_pending_offer_sigil_ids(602u);
	tower->add_pending_offer_sigil_ids(603u);

	BeginRewardedAdSessionRequest b2;
	b2.set_slot_id(2u);
	b2.set_client_platform(AD_CLIENT_PLATFORM_WECHAT_MINIGAME);
	BeginRewardedAdSessionResponse b2Resp;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::BeginSession(player_, b2, kNowMs, b2Resp));

	ClaimRewardedAdRequest c2;
	c2.set_client_request_id(20001u);
	c2.set_slot_id(2u);
	c2.set_ad_session_id(b2Resp.ad_session_id());
	c2.set_target_param_u32(77u);
	ClaimRewardedAdResponse c2Resp;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::Claim(player_, c2, kNowMs + 20000ULL, c2Resp));
	EXPECT_EQ(rewarded_ad_rules::kRewardTowerBoonReroll, c2Resp.reward_kind());
	EXPECT_EQ(0u, c2Resp.reward_amount());
	EXPECT_EQ(0u, CurrencySystem::GetBalance(player_, kCurrencyBindGold));
	EXPECT_EQ(3, c2Resp.rerolled_sigil_ids_size());
	EXPECT_EQ(77u, tower->ad_reroll_used_run_id());

	// 同一局 run_id=77 再次重随必须被 M-7 硬闸拦截。
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::BeginSession(player_, b2, kNowMs + 30000ULL, b2Resp));
	ClaimRewardedAdRequest c2Dup;
	c2Dup.set_client_request_id(20002u);
	c2Dup.set_slot_id(2u);
	c2Dup.set_ad_session_id(b2Resp.ad_session_id());
	c2Dup.set_target_param_u32(77u);
	EXPECT_EQ(kRewardedAdDailyLimitReached, PlayerRewardedAdSystem::Claim(player_, c2Dup, kNowMs + 50000ULL, c2Resp));

	// ── Slot 3：衣柜续时 1 小时试穿（绝不置位永久拥有位图）──
	auto prevRuleSnap = AppearanceRuleTableManager::Instance().CurrentSnapshot();
	auto prevCharSnap = CharacterAppearanceTableManager::Instance().CurrentSnapshot();
	auto ruleSnap = std::make_shared<AppearanceRuleTableSnapshot>();
	auto *ruleRow = ruleSnap->data.add_data();
	ruleRow->set_id(1u);
	ruleRow->set_card_item_id(3402u);
	ruleRow->set_card_item_count(1u);
	ruleRow->set_trial_duration_seconds(3600u);
	ruleSnap->idMap[1u] = ruleRow;
	AppearanceRuleTableManager::Instance().InstallSnapshot(ruleSnap);
	auto charSnap = std::make_shared<CharacterAppearanceTableSnapshot>();
	auto *charRow = charSnap->data.add_data();
	charRow->set_id(2u);
	charRow->set_appearance_id("02");
	charRow->set_enabled(1u);
	charSnap->idMap[2u] = charRow;
	CharacterAppearanceTableManager::Instance().InstallSnapshot(charSnap);

	auto &profile = tlsEcs.actorRegistry.emplace<PlayerProfileComp>(player_);
	profile.set_appearance_id("00");
	const std::string ownedBefore = profile.appearance_owned_bits();

	BeginRewardedAdSessionRequest b3;
	b3.set_slot_id(3u);
	b3.set_client_platform(AD_CLIENT_PLATFORM_WECHAT_MINIGAME);
	BeginRewardedAdSessionResponse b3Resp;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::BeginSession(player_, b3, kNowMs + 60000ULL, b3Resp));
	ClaimRewardedAdRequest c3;
	c3.set_client_request_id(30001u);
	c3.set_slot_id(3u);
	c3.set_ad_session_id(b3Resp.ad_session_id());
	c3.set_target_param_str("02");
	ClaimRewardedAdResponse c3Resp;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::Claim(player_, c3, kNowMs + 80000ULL, c3Resp));
	EXPECT_EQ(rewarded_ad_rules::kRewardWardrobe1hTrial, c3Resp.reward_kind());
	EXPECT_EQ("02", profile.trial_appearance_id());
	EXPECT_GT(profile.trial_expires_at(), 0u);
	EXPECT_EQ(ownedBefore, profile.appearance_owned_bits());

	if (prevRuleSnap) AppearanceRuleTableManager::Instance().InstallSnapshot(prevRuleSnap);
	if (prevCharSnap) CharacterAppearanceTableManager::Instance().InstallSnapshot(prevCharSnap);

	// ── Slot 4：神秘商人个人换货（每轮限 1 次）──
	const auto sceneEnt = tlsEcs.sceneRegistry.create();
	auto &sceneInfo = tlsEcs.sceneRegistry.emplace<SceneInfoComp>(sceneEnt);
	sceneInfo.set_scene_id(9001u);
	tlsEcs.actorRegistry.emplace<SceneEntityComp>(player_).sceneEntity = sceneEnt;
	tlsEcs.actorRegistry.emplace<PlayerMysteryMerchantComp>(player_);

	MysteryMerchantState mState;
	mState.set_round_id(501u);
	mState.set_scene_id(9001u);
	mState.set_appear_at_ms(kNowMs);
	mState.set_expire_at_ms(kNowMs + 3600000ULL);
	auto *se = mState.add_shelf();
	se->set_goods_id(1u);
	se->set_discount_percent(60u);
	PlayerMysteryMerchantSystem::ApplyStateForTest(&mState, kNowMs + 90000ULL);

	BeginRewardedAdSessionRequest b4;
	b4.set_slot_id(4u);
	b4.set_client_platform(AD_CLIENT_PLATFORM_WECHAT_MINIGAME);
	BeginRewardedAdSessionResponse b4Resp;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::BeginSession(player_, b4, kNowMs + 90000ULL, b4Resp));
	ClaimRewardedAdRequest c4;
	c4.set_client_request_id(40001u);
	c4.set_slot_id(4u);
	c4.set_ad_session_id(b4Resp.ad_session_id());
	c4.set_target_param_u32(501u);
	ClaimRewardedAdResponse c4Resp;
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::Claim(player_, c4, kNowMs + 110000ULL, c4Resp));
	EXPECT_EQ(rewarded_ad_rules::kRewardMerchantPersonalRefresh, c4Resp.reward_kind());
	EXPECT_EQ(501u, tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player_).rewarded_ad().merchant_reroll_round_id());

	// 同一轮 round_id=501 再次个人换货拦截。
	ASSERT_EQ(kSuccess, PlayerRewardedAdSystem::BeginSession(player_, b4, kNowMs + 120000ULL, b4Resp));
	ClaimRewardedAdRequest c4Dup;
	c4Dup.set_client_request_id(40002u);
	c4Dup.set_slot_id(4u);
	c4Dup.set_ad_session_id(b4Resp.ad_session_id());
	c4Dup.set_target_param_u32(501u);
	EXPECT_EQ(kMysteryMerchantLimitReached, PlayerRewardedAdSystem::Claim(player_, c4Dup, kNowMs + 140000ULL, c4Resp));

	PlayerMysteryMerchantSystem::ResetForTest();
	tlsEcs.sceneRegistry.destroy(sceneEnt);
}

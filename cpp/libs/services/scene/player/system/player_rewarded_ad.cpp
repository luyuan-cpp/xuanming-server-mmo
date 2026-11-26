#include "player_rewarded_ad.h"

#include <algorithm>
#include <limits>
#include <random>
#include <string>
#include <vector>

#include <openssl/rand.h>
#include "muduo/base/Logging.h"

#include "battle/system/player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/rewarded_ad_rules.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/scene/player_rewarded_ad.pb.h"
#include "table/code/rewardedadslot_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/rewarded_ad_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
std::string PlayerLogId(entt::entity player)
{
	const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
	return std::string("player_id=") + std::to_string(guid != nullptr ? guid->value : kInvalidGuid);
}

rewarded_ad_rules::SlotConfig ToSlotConfig(const RewardedAdSlotTable &row)
{
	rewarded_ad_rules::SlotConfig cfg;
	cfg.slotId = row.id();
	cfg.enabled = row.enabled() != 0;
	cfg.minLevel = row.min_level();
	cfg.dailyLimit = row.daily_limit();
	cfg.cooldownSec = row.cooldown_sec();
	cfg.minWatchSec = row.min_watch_sec();
	cfg.sessionTtlSec = row.session_ttl_sec();
	cfg.currencyType = row.currency_type();
	cfg.rewardAmount = row.reward_amount();
	cfg.requireSsv = row.require_ssv() != 0;
	cfg.platformMask = row.platform_mask();
	return cfg;
}

bool TryResolveSlot(uint32_t slotId, rewarded_ad_rules::SlotConfig &out, const char *&reasonOut)
{
	const auto [row, err] = RewardedAdSlotTableManager::Instance().FindByIdSilent(slotId);
	if (row == nullptr)
	{
		reasonOut = "slot row not found";
		return false;
	}
	if (row->enabled() > 1 || row->require_ssv() > 1)
	{
		reasonOut = "enabled or require_ssv must be 0 or 1";
		return false;
	}
	out = ToSlotConfig(*row);
	return rewarded_ad_rules::ValidateSlotConfig(out, reasonOut);
}

bool HasRequiredComponents(entt::entity player)
{
	return tlsEcs.actorRegistry.all_of<ClientRequestLedgerComp, LevelComp, CurrencyComp, PlayerCurrencyComp>(player) &&
		   !tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player);
}

uint64_t GenerateSessionId(uint64_t avoidId)
{
	for (int attempt = 0; attempt < 8; ++attempt)
	{
		uint64_t candidate = 0;
		if (RAND_bytes(reinterpret_cast<unsigned char *>(&candidate), static_cast<int>(sizeof(candidate))) != 1)
		{
			thread_local std::mt19937_64 fallbackRng(std::random_device{}());
			candidate = fallbackRng();
		}
		// 保持 63 位正数与非 0、且不与前一次挂起会话号重复。
		candidate &= 0x7FFFFFFFFFFFFFFFULL;
		if (candidate != 0u && candidate != avoidId)
		{
			return candidate;
		}
	}
	return (avoidId == 1u) ? 2u : 1u;
}

void FillSlotView(const rewarded_ad_rules::SlotConfig &cfg, const RewardedAdLedger &adLedger,
				  uint32_t clientPlatform, uint32_t playerLevel,
				  uint64_t dayStartMs, uint64_t nextDayMs, uint64_t nowMs,
				  RewardedAdSlotView &out)
{
	out.Clear();
	out.set_slot_id(cfg.slotId);
	const bool platformOk = clientPlatform == 0u || rewarded_ad_rules::IsPlatformAllowed(clientPlatform, cfg.platformMask);
	out.set_enabled(cfg.enabled && platformOk && playerLevel >= cfg.minLevel);
	out.set_min_level(cfg.minLevel);
	out.set_daily_limit(cfg.dailyLimit);
	out.set_claimed_today(rewarded_ad_rules::EffectiveClaimedToday(adLedger, dayStartMs));
	out.set_next_reset_ms(nextDayMs);
	const uint64_t cooldownUntil = rewarded_ad_rules::CooldownUntilMs(adLedger, cfg.cooldownSec);
	out.set_cooldown_until_ms(cooldownUntil > nowMs ? cooldownUntil : 0u);
	out.set_currency_type(cfg.currencyType);
	out.set_reward_amount(cfg.rewardAmount);
}

bool IsBindGoldBlocked(entt::entity player)
{
	return GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency,
											   static_cast<uint32_t>(kCurrencyBindGold)) ||
		   CurrencySystem::IsCurrencyBlocked(player, kCurrencyBindGold);
}
} // namespace

bool PlayerRewardedAdSystem::ValidateTables()
{
	bool ok = true;
	for (const auto &row : RewardedAdSlotTableManager::Instance().FindAll().data())
	{
		if (row.enabled() > 1 || row.require_ssv() > 1)
		{
			LOG_ERROR << "[RewardedAd] invalid RewardedAdSlot row: id=" << row.id()
					  << " reason=enabled_or_require_ssv_out_of_range";
			ok = false;
			continue;
		}
		const auto cfg = ToSlotConfig(row);
		const char *reason = nullptr;
		if (!rewarded_ad_rules::ValidateSlotConfig(cfg, reason))
		{
			LOG_ERROR << "[RewardedAd] invalid RewardedAdSlot row: id=" << row.id()
					  << " reason=" << (reason != nullptr ? reason : "unknown");
			ok = false;
		}
	}
	return ok;
}

uint32_t PlayerRewardedAdSystem::GetInfo(entt::entity player, const GetRewardedAdInfoRequest &request,
										 uint64_t nowMs, GetRewardedAdInfoResponse &response)
{
	response.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	if (!HasRequiredComponents(player))
	{
		return kRewardedAdUnavailable;
	}
	uint64_t dayStartMs = 0;
	uint64_t nextDayMs = 0;
	if (!rewarded_ad_rules::ResolveGameDay(nowMs, dayStartMs, nextDayMs))
	{
		return kRewardedAdUnavailable;
	}

	const auto &ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
	const uint32_t playerLevel = tlsEcs.actorRegistry.get<LevelComp>(player).level();
	response.set_server_time_ms(nowMs);
	response.set_bind_gold_balance(CurrencySystem::GetBalance(player, kCurrencyBindGold));
	response.set_gold_balance(CurrencySystem::GetBalance(player, kCurrencyGold));

	std::vector<rewarded_ad_rules::SlotConfig> validSlots;
	for (const auto &row : RewardedAdSlotTableManager::Instance().FindAll().data())
	{
		if (row.enabled() > 1 || row.require_ssv() > 1)
		{
			continue;
		}
		const auto cfg = ToSlotConfig(row);
		const char *reason = nullptr;
		if (!rewarded_ad_rules::ValidateSlotConfig(cfg, reason))
		{
			continue;
		}
		validSlots.push_back(cfg);
	}
	std::sort(validSlots.begin(), validSlots.end(),
			  [](const auto &a, const auto &b) { return a.slotId < b.slotId; });

	for (const auto &cfg : validSlots)
	{
		FillSlotView(cfg, ledger.rewarded_ad(), request.client_platform(), playerLevel,
					 dayStartMs, nextDayMs, nowMs, *response.add_slots());
	}
	return kSuccess;
}

uint32_t PlayerRewardedAdSystem::BeginSession(entt::entity player, const BeginRewardedAdSessionRequest &request,
											  uint64_t nowMs, BeginRewardedAdSessionResponse &response)
{
	response.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	if (PlayerBattleSystem::IsInBattle(player))
	{
		return kRewardedAdInBattle;
	}
	if (PlayerLifecycleSystem::IsPlayerFrozen(player) || !player_mutability::IsPlayerMutable(player))
	{
		return kRewardedAdFrozen;
	}
	if (!HasRequiredComponents(player))
	{
		return kRewardedAdUnavailable;
	}

	rewarded_ad_rules::SlotConfig cfg;
	const char *reason = nullptr;
	if (!TryResolveSlot(request.slot_id(), cfg, reason))
	{
		LOG_WARN << "[RewardedAd] metric=rewarded_ad_session_rejected reason=invalid_slot "
				 << PlayerLogId(player) << " slot_id=" << request.slot_id()
				 << " detail=" << (reason != nullptr ? reason : "unknown");
		return kRewardedAdUnavailable;
	}
	if (!cfg.enabled)
	{
		return kRewardedAdDisabled;
	}
	if (!rewarded_ad_rules::IsPlatformAllowed(request.client_platform(), cfg.platformMask))
	{
		return kRewardedAdPlatformUnsupported;
	}
	const uint32_t playerLevel = tlsEcs.actorRegistry.get<LevelComp>(player).level();
	if (playerLevel < cfg.minLevel)
	{
		return kRewardedAdLevelNotEnough;
	}

	uint64_t dayStartMs = 0;
	uint64_t nextDayMs = 0;
	if (!rewarded_ad_rules::ResolveGameDay(nowMs, dayStartMs, nextDayMs))
	{
		return kRewardedAdUnavailable;
	}

	auto &ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
	auto &adLedger = *ledger.mutable_rewarded_ad();
	const uint32_t claimedToday = rewarded_ad_rules::EffectiveClaimedToday(adLedger, dayStartMs);
	if (claimedToday >= cfg.dailyLimit)
	{
		return kRewardedAdDailyLimitReached;
	}
	const uint64_t cooldownUntil = rewarded_ad_rules::CooldownUntilMs(adLedger, cfg.cooldownSec);
	if (cooldownUntil > 0u && nowMs < cooldownUntil)
	{
		return kRewardedAdCooldown;
	}
	if (IsBindGoldBlocked(player))
	{
		return kRewardedAdCurrencyBlocked;
	}
	if (CurrencySystem::CreditOverflows(player, kCurrencyBindGold, cfg.rewardAmount))
	{
		return kRewardedAdUnavailable;
	}

	rewarded_ad_rules::AdvanceDayIfNeeded(adLedger, dayStartMs);
	const uint64_t sessionId = GenerateSessionId(adLedger.active_session_id());
	adLedger.set_active_session_id(sessionId);
	adLedger.set_active_session_issued_ms(nowMs);
	adLedger.set_active_session_slot_id(cfg.slotId);

	// 落盘 WALLET 组以抗 30 秒全屏广告暂停心跳引发的断线重连。
	PlayerLifecycleSystem::RequestKeyProgressSave(player, "rewarded_ad_session_issued");

	const uint64_t ttlMs = static_cast<uint64_t>(cfg.sessionTtlSec) * 1000ULL;
	const uint64_t expiresAt = nowMs > std::numeric_limits<uint64_t>::max() - ttlMs
								   ? std::numeric_limits<uint64_t>::max()
								   : (nowMs + ttlMs);
	response.set_ad_session_id(sessionId);
	response.set_expires_at_ms(expiresAt);

	LOG_INFO << "[RewardedAd] metric=rewarded_ad_session_issued " << PlayerLogId(player)
			 << " slot_id=" << cfg.slotId << " platform=" << request.client_platform()
			 << " ad_session_id=" << sessionId;
	return kSuccess;
}

uint32_t PlayerRewardedAdSystem::Claim(entt::entity player, const ClaimRewardedAdRequest &request,
									   uint64_t nowMs, ClaimRewardedAdResponse &response)
{
	response.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	const uint64_t clientRequestId = request.client_request_id();
	if (clientRequestId == 0u)
	{
		return kInvalidParameter;
	}

	// 1. 幂等查重必须排在一切状态校验之前（首次已成功，重放直接回首次结果）。
	const std::string fingerprint = ClientRequestFingerprint(request);
	ClientRequestResult replayedResult;
	const auto verdict = client_request_dedup::Check(
		player, clientRequestId, ClientRequestKind::kClaimRewardedAd, fingerprint, replayedResult);
	if (verdict == ClientRequestVerdict::kUnavailable)
	{
		return kServiceUnavailable;
	}
	if (verdict == ClientRequestVerdict::kMismatch)
	{
		return kInvalidParameter;
	}
	if (verdict == ClientRequestVerdict::kReplay)
	{
		response.set_currency_type(replayedResult.rewardedAdCurrencyType);
		response.set_reward_amount(replayedResult.rewardedAdAmount);
		response.set_currency_balance(CurrencySystem::GetBalance(player, kCurrencyBindGold));
		response.set_gold_balance(CurrencySystem::GetBalance(player, kCurrencyGold));
		response.set_replayed(true);
		uint64_t dayStartMs = 0;
		uint64_t nextDayMs = 0;
		rewarded_ad_rules::SlotConfig cfg;
		const char *reason = nullptr;
		if (HasRequiredComponents(player) &&
			rewarded_ad_rules::ResolveGameDay(nowMs, dayStartMs, nextDayMs) &&
			TryResolveSlot(request.slot_id(), cfg, reason))
		{
			const auto &ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
			const uint32_t level = tlsEcs.actorRegistry.get<LevelComp>(player).level();
			FillSlotView(cfg, ledger.rewarded_ad(), 0u, level, dayStartMs, nextDayMs, nowMs,
						 *response.mutable_slot());
		}
		return kSuccess;
	}

	// 2. 运行状态与必需组件校验。
	if (PlayerBattleSystem::IsInBattle(player))
	{
		return kRewardedAdInBattle;
	}
	if (PlayerLifecycleSystem::IsPlayerFrozen(player) || !player_mutability::IsPlayerMutable(player))
	{
		return kRewardedAdFrozen;
	}
	if (!HasRequiredComponents(player))
	{
		return kRewardedAdUnavailable;
	}

	// 3. 配表、等级、日限与冷却校验。
	rewarded_ad_rules::SlotConfig cfg;
	const char *reason = nullptr;
	if (!TryResolveSlot(request.slot_id(), cfg, reason))
	{
		return kRewardedAdUnavailable;
	}
	if (!cfg.enabled)
	{
		return kRewardedAdDisabled;
	}
	const uint32_t playerLevel = tlsEcs.actorRegistry.get<LevelComp>(player).level();
	if (playerLevel < cfg.minLevel)
	{
		return kRewardedAdLevelNotEnough;
	}

	uint64_t dayStartMs = 0;
	uint64_t nextDayMs = 0;
	if (!rewarded_ad_rules::ResolveGameDay(nowMs, dayStartMs, nextDayMs))
	{
		return kRewardedAdUnavailable;
	}

	auto &ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
	const auto &adLedger = ledger.rewarded_ad();
	if (rewarded_ad_rules::EffectiveClaimedToday(adLedger, dayStartMs) >= cfg.dailyLimit)
	{
		return kRewardedAdDailyLimitReached;
	}
	const uint64_t cooldownUntil = rewarded_ad_rules::CooldownUntilMs(adLedger, cfg.cooldownSec);
	if (cooldownUntil > 0u && nowMs < cooldownUntil)
	{
		return kRewardedAdCooldown;
	}

	// 4. 一次性会话、最短观看时长、TTL 与可选 SSV 校验。
	const auto sessionVerdict = rewarded_ad_rules::VerifyActiveSession(
		adLedger, cfg.slotId, request.ad_session_id(), nowMs, cfg.minWatchSec, cfg.sessionTtlSec);
	if (sessionVerdict == rewarded_ad_rules::SessionVerdict::kInvalid)
	{
		LOG_WARN << "[RewardedAd] metric=rewarded_ad_claim_rejected reason=session_invalid "
				 << PlayerLogId(player) << " slot_id=" << cfg.slotId
				 << " ad_session_id=" << request.ad_session_id();
		return kRewardedAdSessionInvalid;
	}
	if (sessionVerdict == rewarded_ad_rules::SessionVerdict::kExpired)
	{
		LOG_WARN << "[RewardedAd] metric=rewarded_ad_claim_rejected reason=session_expired "
				 << PlayerLogId(player) << " slot_id=" << cfg.slotId
				 << " ad_session_id=" << request.ad_session_id();
		return kRewardedAdSessionExpired;
	}
	if (sessionVerdict == rewarded_ad_rules::SessionVerdict::kTooShort)
	{
		LOG_WARN << "[RewardedAd] metric=rewarded_ad_claim_rejected reason=too_fast "
				 << PlayerLogId(player) << " slot_id=" << cfg.slotId
				 << " ad_session_id=" << request.ad_session_id();
		return kRewardedAdWatchTooShort;
	}
	if (cfg.requireSsv && request.ssv_proof().empty())
	{
		return kRewardedAdSsvRequired;
	}

	// 5. 产出封禁与溢出预检。
	if (IsBindGoldBlocked(player))
	{
		return kRewardedAdCurrencyBlocked;
	}
	if (CurrencySystem::CreditOverflows(player, kCurrencyBindGold, cfg.rewardAmount))
	{
		return kRewardedAdUnavailable;
	}

	// 6. 候选账本预演（不通过绝不发钱），随后同帧原子入账 + 落账 + 关键进度存盘。
	ClientRequestResult applied;
	applied.rewardedAdCurrencyType = static_cast<uint32_t>(kCurrencyBindGold);
	applied.rewardedAdAmount = cfg.rewardAmount;

	ClientRequestLedgerComp candidateLedger = ledger;
	auto &candidateAd = *candidateLedger.mutable_rewarded_ad();
	rewarded_ad_rules::AdvanceDayIfNeeded(candidateAd, dayStartMs);
	candidateAd.set_claimed_today(candidateAd.claimed_today() + 1u);
	candidateAd.set_last_claim_at_ms(nowMs);
	if (candidateAd.total_claimed_lifetime() < std::numeric_limits<uint64_t>::max())
	{
		candidateAd.set_total_claimed_lifetime(candidateAd.total_claimed_lifetime() + 1u);
	}
	candidateAd.set_active_session_id(0u);
	candidateAd.set_active_session_issued_ms(0u);
	candidateAd.set_active_session_slot_id(0u);

	if (!client_request_ledger::Remember(candidateLedger, clientRequestId, ClientRequestKind::kClaimRewardedAd,
										 fingerprint, applied) ||
		!client_request_ledger::Validate(candidateLedger).empty())
	{
		return kServiceUnavailable;
	}

	const uint32_t addErr = CurrencySystem::AddCurrency(
		player, kCurrencyBindGold, static_cast<int64_t>(cfg.rewardAmount),
		TX_SYSTEM_GRANT, request.ad_session_id());
	if (addErr != kSuccess)
	{
		if (addErr == kAssetBlocked)
		{
			return kRewardedAdCurrencyBlocked;
		}
		if (addErr == kAssetFrozen)
		{
			return kRewardedAdFrozen;
		}
		LOG_ERROR << "[RewardedAd] AddCurrency failed after precheck " << PlayerLogId(player)
				  << " slot_id=" << cfg.slotId << " amount=" << cfg.rewardAmount << " err=" << addErr;
		return kRewardedAdUnavailable;
	}

	ledger.Swap(&candidateLedger);
	PlayerLifecycleSystem::RequestKeyProgressSave(player, "rewarded_ad_claimed");

	response.set_currency_type(static_cast<uint32_t>(kCurrencyBindGold));
	response.set_reward_amount(cfg.rewardAmount);
	response.set_currency_balance(CurrencySystem::GetBalance(player, kCurrencyBindGold));
	response.set_gold_balance(CurrencySystem::GetBalance(player, kCurrencyGold));
	response.set_replayed(false);
	FillSlotView(cfg, ledger.rewarded_ad(), 0u, playerLevel, dayStartMs, nextDayMs, nowMs,
				 *response.mutable_slot());

	LOG_INFO << "[RewardedAd] metric=rewarded_ad_claimed " << PlayerLogId(player)
			 << " slot_id=" << cfg.slotId << " currency_type=" << static_cast<uint32_t>(kCurrencyBindGold)
			 << " reward_amount=" << cfg.rewardAmount
			 << " claimed_today=" << ledger.rewarded_ad().claimed_today()
			 << " client_request_id=" << clientRequestId;
	return kSuccess;
}

#pragma once

// 激励视频广告纯规则层(docs/design/rewarded-ad-system.md):
// 零 ECS、零网络、零时钟源依赖,与 Java 独立服 RewardedAdRules 逐项同构,便于单测全覆盖。

#include <cstdint>
#include <limits>

#include "proto/common/base/currency_type.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"

namespace rewarded_ad_rules
{
inline constexpr uint64_t kDayMs = 86400000ULL;
inline constexpr uint64_t kShiftMs = 10800000ULL; // UTC+8 05:00 切日(同 player_equipment_material_reward.cpp)
inline constexpr uint32_t kMaxDailyLimit = 20u;
inline constexpr uint32_t kMaxSessionTtlSec = 3600u;
inline constexpr uint64_t kMaxRewardAmount = 100000ULL;
inline constexpr uint32_t kAllPlatformMask = 0x0Fu; // bits 0..3: 微信小游戏/抖音小游戏/iOS/Android

struct SlotConfig
{
	uint32_t slotId{0};
	bool enabled{false};
	uint32_t minLevel{0};
	uint32_t dailyLimit{0};
	uint32_t cooldownSec{0};
	uint32_t minWatchSec{0};
	uint32_t sessionTtlSec{0};
	uint32_t currencyType{0};
	uint64_t rewardAmount{0};
	bool requireSsv{false};
	uint32_t platformMask{0};
};

enum class SessionVerdict : uint8_t
{
	kOk = 0,
	kInvalid = 1,
	kTooShort = 2,
	kExpired = 3,
};

// 解析 nowMs 所属游戏日区间 [dayStartOut, nextDayOut),切点 UTC+8 每日 05:00。
inline bool ResolveGameDay(uint64_t nowMs, uint64_t &dayStartOut, uint64_t &nextDayOut)
{
	if (nowMs > std::numeric_limits<uint64_t>::max() - kShiftMs - kDayMs)
	{
		return false;
	}
	const uint64_t shifted = nowMs + kShiftMs;
	const uint64_t dayIndex = shifted / kDayMs;
	dayStartOut = dayIndex == 0 ? 0ULL : (dayIndex * kDayMs - kShiftMs);
	nextDayOut = (dayIndex + 1ULL) * kDayMs - kShiftMs;
	return true;
}

inline uint32_t PlatformBit(uint32_t clientPlatform)
{
	if (clientPlatform < 1u || clientPlatform > 4u)
	{
		return 0u;
	}
	return 1u << (clientPlatform - 1u);
}

inline bool IsPlatformAllowed(uint32_t clientPlatform, uint32_t platformMask)
{
	const uint32_t bit = PlatformBit(clientPlatform);
	return bit != 0u && (platformMask & bit) != 0u;
}

// 强校验配置表行:激励视频只允许发放物理不可交易的 CURRENCY_TYPE_BIND_GOLD(3),
// 严禁误配成流通银两(0)或真灵石(1)。
inline bool ValidateSlotConfig(const SlotConfig &cfg, const char *&reasonOut)
{
	if (cfg.slotId == 0u)
	{
		reasonOut = "slot id is 0";
		return false;
	}
	if (cfg.dailyLimit == 0u || cfg.dailyLimit > kMaxDailyLimit)
	{
		reasonOut = "daily_limit out of range 1..20";
		return false;
	}
	if (cfg.minWatchSec == 0u || cfg.sessionTtlSec == 0u ||
		cfg.minWatchSec > cfg.sessionTtlSec || cfg.sessionTtlSec > kMaxSessionTtlSec)
	{
		reasonOut = "invalid min_watch_sec or session_ttl_sec";
		return false;
	}
	if (cfg.currencyType != static_cast<uint32_t>(common::base::CURRENCY_TYPE_BIND_GOLD))
	{
		reasonOut = "rewarded ad slot must grant CURRENCY_TYPE_BIND_GOLD (3) only";
		return false;
	}
	if (cfg.rewardAmount == 0u || cfg.rewardAmount > kMaxRewardAmount)
	{
		reasonOut = "reward_amount out of range 1..100000";
		return false;
	}
	if ((cfg.platformMask & ~kAllPlatformMask) != 0u)
	{
		reasonOut = "platform_mask has unknown bits";
		return false;
	}
	reasonOut = nullptr;
	return true;
}

// 抗时钟回拨的今日已领次数查询:只有当 currentDayStartMs 严格前进才视为新的一天。
inline uint32_t EffectiveClaimedToday(const RewardedAdLedger &ledger, uint64_t currentDayStartMs)
{
	if (currentDayStartMs > ledger.day_starts_at_ms())
	{
		return 0u;
	}
	return ledger.claimed_today();
}

// 推进日账本(仅前进不清倒退)。
inline void AdvanceDayIfNeeded(RewardedAdLedger &ledger, uint64_t currentDayStartMs)
{
	if (currentDayStartMs > ledger.day_starts_at_ms())
	{
		ledger.set_day_starts_at_ms(currentDayStartMs);
		ledger.set_claimed_today(0u);
	}
}

inline uint64_t CooldownUntilMs(const RewardedAdLedger &ledger, uint32_t cooldownSec)
{
	if (ledger.last_claim_at_ms() == 0u || cooldownSec == 0u)
	{
		return 0u;
	}
	const uint64_t deltaMs = static_cast<uint64_t>(cooldownSec) * 1000ULL;
	if (ledger.last_claim_at_ms() > std::numeric_limits<uint64_t>::max() - deltaMs)
	{
		return std::numeric_limits<uint64_t>::max();
	}
	return ledger.last_claim_at_ms() + deltaMs;
}

inline SessionVerdict VerifyActiveSession(const RewardedAdLedger &ledger, uint32_t slotId,
										  uint64_t adSessionId, uint64_t nowMs,
										  uint32_t minWatchSec, uint32_t sessionTtlSec)
{
	if (adSessionId == 0u || ledger.active_session_id() != adSessionId ||
		ledger.active_session_slot_id() != slotId || ledger.active_session_issued_ms() == 0u)
	{
		return SessionVerdict::kInvalid;
	}
	if (nowMs < ledger.active_session_issued_ms())
	{
		return SessionVerdict::kTooShort;
	}
	const uint64_t elapsedMs = nowMs - ledger.active_session_issued_ms();
	if (elapsedMs > static_cast<uint64_t>(sessionTtlSec) * 1000ULL)
	{
		return SessionVerdict::kExpired;
	}
	if (elapsedMs < static_cast<uint64_t>(minWatchSec) * 1000ULL)
	{
		return SessionVerdict::kTooShort;
	}
	return SessionVerdict::kOk;
}
} // namespace rewarded_ad_rules

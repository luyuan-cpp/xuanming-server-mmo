#pragma once

// 挂机月卡的纯规则(docs/design/afk-monthly-card.md §4):不碰 ECS、不读时钟、不发命令,可单测(cpp/tests/bag_test/afk_pass_test.cpp)。
// 粘合在 afk_pass.cpp(AfkPassSystem)。所有时间都是墙钟 Unix 秒,由调用方传入。

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

#include "modules/currency/constants/currency.h" // IsKnownCurrencyType

namespace afk_pass
{
	inline constexpr uint64_t kSecondsPerDay = 86400;

	// 规则表 AfkPassRule 的取值范围(与 data/schema/afkpassrule_table.proto、tools/scripts/afk_pass_xlsx_patch.py 的自检一致)。
	inline constexpr uint32_t kMaxPeriodDays = 366;
	inline constexpr uint64_t kMaxPrice = 1'000'000'000;
	inline constexpr uint32_t kMaxRemainingDaysCap = 3660;
	inline constexpr uint32_t kMaxRemindBeforeHours = 720;
	inline constexpr uint32_t kMaxBonusItemCount = 99;
	inline constexpr std::size_t kMaxBonusFrameKeyLength = 64;

	// GM 设定剩余时长的上限(GmSetAfkPassRequest.remaining_seconds):366 天。
	inline constexpr uint32_t kGmMaxRemainingSeconds = 366u * 86400u;

	// 规则表单套餐行里本功能用到的列,按值拷出来:表行指针不出函数(热更时旧快照当场析构,data/AGENTS.md)。
	struct Rule
	{
		uint32_t periodDays{0};
		uint32_t priceCurrencyType{0};
		uint64_t price{0};
		uint32_t maxRemainingDays{0};
		uint32_t remindBeforeHours{0};
		uint32_t planId{1};
		uint32_t bonusItemId{0};
		uint32_t bonusItemCount{0};
		std::string bonusFrameKey;
		std::string planName;
		std::string iconKey;
		std::string desc;
	};

	inline bool IsValidBonusFrameKey(std::string_view key)
	{
		return key.empty() ||
			   (key.size() > 6 && key.size() <= kMaxBonusFrameKeyLength && key.substr(0, 6) == "frame_");
	}

	// 不合法时返回原因(只进日志),合法返回 nullptr。不合法的规则一律不能开通(kAfkPassUnavailable)。
	inline const char *RuleProblem(const Rule &rule)
	{
		if (rule.planId == 0)
		{
			return "id must be non-zero";
		}
		if (rule.periodDays == 0 || rule.periodDays > kMaxPeriodDays)
		{
			return "period_days is not in 1..366";
		}
		if (!IsKnownCurrencyType(rule.priceCurrencyType))
		{
			return "price_currency_type is not a known CurrencyType";
		}
		if (rule.price == 0 || rule.price > kMaxPrice)
		{
			return "price is not in 1..1000000000";
		}
		if (rule.maxRemainingDays < rule.periodDays || rule.maxRemainingDays > kMaxRemainingDaysCap)
		{
			return "max_remaining_days is not in period_days..3660";
		}
		if (rule.remindBeforeHours > kMaxRemindBeforeHours)
		{
			return "remind_before_hours is not in 0..720";
		}
		if (rule.bonusItemId != 0 && (rule.bonusItemCount == 0 || rule.bonusItemCount > kMaxBonusItemCount))
		{
			return "bonus_item_count is not in 1..99";
		}
		if (rule.bonusItemId == 0 && rule.bonusItemCount != 0)
		{
			return "bonus_item_count requires bonus_item_id";
		}
		if (!IsValidBonusFrameKey(rule.bonusFrameKey))
		{
			return "bonus_frame_key must be empty or start with frame_";
		}
		return nullptr;
	}

	// 开通 / 续费之后的到期:max(现在, 原到期) + period —— 已到期或从未开通的从现在起算,没到期的接着往后叠。
	// 续费之后剩余超过 maxRemainingDays 天返回 nullopt(调用方回 kAfkPassStackLimit);恰好等于上限放行。
	// 原到期本身已经超过上限(上限被调小,或坏数据)同样返回 nullopt:先比剩余再相加,不做会溢出的加法。
	constexpr std::optional<uint64_t> RenewedExpiry(uint64_t currentExpiresAt, uint64_t nowUnixSec, uint32_t periodDays,
													uint32_t maxRemainingDays)
	{
		const uint64_t remaining = currentExpiresAt > nowUnixSec ? currentExpiresAt - nowUnixSec : 0;
		const uint64_t period = static_cast<uint64_t>(periodDays) * kSecondsPerDay;
		const uint64_t cap = static_cast<uint64_t>(maxRemainingDays) * kSecondsPerDay;
		if (remaining > cap || cap - remaining < period)
		{
			return std::nullopt;
		}
		return nowUnixSec + remaining + period;
	}

	// GM 设定:到期 = 现在 + remainingSeconds;0 = 清除(到期写 0,与「从未开通」同一个值)。
	constexpr uint64_t GmExpiry(uint64_t nowUnixSec, uint32_t remainingSeconds)
	{
		return remainingSeconds == 0 ? 0 : nowUnixSec + remainingSeconds;
	}

	// Redis 投影怎么写(设计 §5.1):到期在将来 → SET … EXAT 到期(到点自动消失);0 或已过 → DEL。
	constexpr bool ProjectionIsSet(uint64_t expiresAt, uint64_t nowUnixSec)
	{
		return expiresAt > nowUnixSec;
	}

	// 仙途轻享卡纯便利方案槽位扩展（零战力加成，免费 2 格 + 轻享卡额外 +2 格 = 4 格，叠加按月租用最高 7 格）。
	inline constexpr uint32_t kBaseLoadoutSlots = 2;
	inline constexpr uint32_t kLightCardBonusLoadoutSlots = 2;
	inline constexpr uint32_t kLightCardLoadoutSlots = kBaseLoadoutSlots + kLightCardBonusLoadoutSlots;

	constexpr bool IsActive(uint64_t expiresAt, uint64_t nowUnixSec)
	{
		return expiresAt > nowUnixSec;
	}

	constexpr uint32_t UnlockedLoadoutSlots(bool active)
	{
		return active ? kLightCardLoadoutSlots : kBaseLoadoutSlots;
	}

	// 仙途轻享卡前期体感与市场便利权益（零每日打卡、零战力加成）。
	inline constexpr uint32_t kBaseMarketListingLimit = 10;
	inline constexpr uint32_t kLightCardMarketListingLimit = 20;
	inline constexpr uint32_t kBaseStallDurationHours = 48;
	inline constexpr uint32_t kLightCardStallDurationHours = 96;
	inline constexpr uint32_t kLightCardWeeklyFreeTrialQuota = 3;

	constexpr uint32_t MarketListingLimit(bool active)
	{
		return active ? kLightCardMarketListingLimit : kBaseMarketListingLimit;
	}

	constexpr uint32_t StallDurationHours(bool active)
	{
		return active ? kLightCardStallDurationHours : kBaseStallDurationHours;
	}

	constexpr uint32_t WeeklyFreeTrialQuota(bool active)
	{
		return active ? kLightCardWeeklyFreeTrialQuota : 0u;
	}
} // namespace afk_pass

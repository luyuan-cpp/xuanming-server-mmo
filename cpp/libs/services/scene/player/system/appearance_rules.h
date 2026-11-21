#pragma once

// 角色形象切换与一小时试用的纯规则函数(docs/design/character-appearance-switch.md)。
// 不碰 ECS、不读表管理器、不取时钟,供 player_appearance.cpp 与单测共用。

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

#include "engine/core/utils/bit_index/bit_index_util.h"
#include "table/code/bit_index/characterappearance_table_id_bit_index.h"

namespace player_appearance
{
	inline constexpr uint32_t kMinTrialDurationSeconds = 60;
	inline constexpr uint32_t kMaxTrialDurationSeconds = 86'400; // 24 小时上界,防配表误配
	inline constexpr uint32_t kMaxCardItemCount = 99;
	inline constexpr uint64_t kMaxMallPriceFen = 1'000'000; // 1 万元上界,防配表误配

	struct Rule
	{
		uint32_t cardItemId{0};
		uint32_t cardItemCount{0};
		uint32_t trialDurationSeconds{0};
		uint64_t mallSkuId{0};
		uint64_t mallPriceFen{0};
	};

	// 校验单行规则表取值。合法返回 nullptr,否则返回英文原因(只进日志)。
	inline const char *RuleProblem(const Rule &rule)
	{
		if (rule.cardItemId == 0)
		{
			return "card_item_id must be non-zero";
		}
		if (rule.cardItemCount == 0 || rule.cardItemCount > kMaxCardItemCount)
		{
			return "card_item_count out of range";
		}
		if (rule.trialDurationSeconds < kMinTrialDurationSeconds ||
			rule.trialDurationSeconds > kMaxTrialDurationSeconds)
		{
			return "trial_duration_seconds out of range";
		}
		if (rule.mallPriceFen > kMaxMallPriceFen)
		{
			return "mall_price_fen out of range";
		}
		return nullptr;
	}

	// 判断当前是否处于有效试用期。到期时刻 <= now 视为已过期。
	inline bool IsTrialActive(std::string_view trialAppearanceId, uint64_t trialExpiresAt, uint64_t nowUnixSec)
	{
		return !trialAppearanceId.empty() && trialExpiresAt > nowUnixSec;
	}

	// 计算此刻实际对外展示的外观 ID:试用有效取试用形象,否则取永久形象。
	inline std::string_view EffectiveAppearance(std::string_view permanentAppearanceId,
												std::string_view trialAppearanceId,
												uint64_t trialExpiresAt,
												uint64_t nowUnixSec)
	{
		if (IsTrialActive(trialAppearanceId, trialExpiresAt, nowUnixSec))
		{
			return trialAppearanceId;
		}
		return permanentAppearanceId;
	}

	// 按 CharacterAppearance 主键查询稳定位索引。表主键未登记映射返回 std::nullopt。
	inline std::optional<uint32_t> TrialBitIndex(uint32_t configId)
	{
		return GetBitIndex(CharacterAppearanceBitMap, configId);
	}

	// 查询该形象是否已被试用过(紧凑位图,低位在前;未映射或越界按未试用返回 false)。
	inline bool IsTrialUsed(std::string_view trialUsedBits, uint32_t configId)
	{
		const auto idx = TrialBitIndex(configId);
		return idx.has_value() && TestPackedBit(trialUsedBits, *idx);
	}

	// 在紧凑位图中置位该形象的试用标记(保留其他高位未知 bit 不变)。未映射返回 false。
	inline bool MarkTrialUsed(std::string &trialUsedBits, uint32_t configId)
	{
		const auto idx = TrialBitIndex(configId);
		if (!idx.has_value())
		{
			return false;
		}
		SetPackedBit(trialUsedBits, *idx);
		return true;
	}

	// 目录外观同步失败时的有界指数退避:2s, 4s, 8s, 16s, 30s 封顶。
	inline uint64_t DirectorySyncRetryDelayMs(uint32_t attempt)
	{
		if (attempt <= 1)
		{
			return 2'000;
		}
		if (attempt == 2)
		{
			return 4'000;
		}
		if (attempt == 3)
		{
			return 8'000;
		}
		if (attempt == 4)
		{
			return 16'000;
		}
		return 30'000;
	}
} // namespace player_appearance

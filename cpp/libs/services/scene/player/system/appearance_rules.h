#pragma once

// 角色形象切换与一小时试用的纯规则函数(docs/design/character-appearance-switch.md)。
// 不碰 ECS、不读表管理器、不取时钟,供 player_appearance.cpp 与单测共用。

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

#include "engine/core/utils/bit_index/bit_index_util.h"
#include "table/code/bit_index/characterappearance_table_id_bit_index.h"
#include "table/code/bit_index/petcosmetic_table_id_bit_index.h"
#include "table/code/bit_index/skillvfxcosmetic_table_id_bit_index.h"

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
	inline std::optional<uint32_t> AppearanceBitIndex(uint32_t configId)
	{
		return GetBitIndex(CharacterAppearanceBitMap, configId);
	}

	inline std::optional<uint32_t> TrialBitIndex(uint32_t configId)
	{
		return AppearanceBitIndex(configId);
	}

	// 查询紧凑位图指定主键是否置位(低位在前;未映射或越界返回 false)。
	inline bool IsBitMarked(std::string_view bits, uint32_t configId)
	{
		const auto idx = AppearanceBitIndex(configId);
		return idx.has_value() && TestPackedBit(bits, *idx);
	}

	// 在紧凑位图中置位指定主键(保留其他高位未知 bit 不变)。未映射返回 false。
	inline bool MarkBit(std::string &bits, uint32_t configId)
	{
		const auto idx = AppearanceBitIndex(configId);
		if (!idx.has_value())
		{
			return false;
		}
		SetPackedBit(bits, *idx);
		return true;
	}

	// 查询该形象是否已被试用过(紧凑位图,低位在前;未映射或越界按未试用返回 false)。
	inline bool IsTrialUsed(std::string_view trialUsedBits, uint32_t configId)
	{
		return IsBitMarked(trialUsedBits, configId);
	}

	// 在紧凑位图中置位该形象的试用标记(保留其他高位未知 bit 不变)。未映射返回 false。
	inline bool MarkTrialUsed(std::string &trialUsedBits, uint32_t configId)
	{
		return MarkBit(trialUsedBits, configId);
	}

	// 查询玩家是否永久拥有该形象:显式 owned_bits 置位,或等于当前永久 appearance_id(旧角色零迁移兼容)。
	inline bool IsOwned(std::string_view ownedBits,
						uint32_t configId,
						std::string_view permanentAppearanceId,
						std::string_view candidateAppearanceId)
	{
		if (!candidateAppearanceId.empty() && candidateAppearanceId == permanentAppearanceId)
		{
			return true;
		}
		return IsBitMarked(ownedBits, configId);
	}

	// 在永久拥有位图中置位指定主键(保留其他高位未知 bit 不变)。未映射返回 false。
	inline bool MarkOwned(std::string &ownedBits, uint32_t configId)
	{
		return MarkBit(ownedBits, configId);
	}

	enum class SwitchPlan : uint8_t
	{
		kFreeSwitch,
		kCardUnlock,
		kRefuseNotOwned,
		kRefuseNotCardUnlockable,
	};

	// 决定一次切换请求走免费切换、扣卡买断解锁，还是 fail-closed 拒绝。
	inline SwitchPlan PlanSwitch(bool alreadyOwned, bool expectFreeSwitch, bool cardUnlockable)
	{
		if (alreadyOwned)
		{
			return SwitchPlan::kFreeSwitch;
		}
		if (expectFreeSwitch)
		{
			return SwitchPlan::kRefuseNotOwned;
		}
		if (!cardUnlockable)
		{
			return SwitchPlan::kRefuseNotCardUnlockable;
		}
		return SwitchPlan::kCardUnlock;
	}

	// 系列图鉴是否集齐(纯外观点亮与合篇解锁判定,零数值属性)。空系列(totalCount==0)永不算集齐。
	inline bool IsSeriesCompleted(uint32_t ownedCount, uint32_t totalCount)
	{
		return totalCount > 0 && ownedCount >= totalCount;
	}

	inline bool IsBlankText(std::string_view value)
	{
		return value.find_first_not_of(" \t\r\n") == std::string_view::npos;
	}

	// 人物志字段必须全空(暂无人物志)或四项核心字段(档案名/小传/篇名/正文)同时非空,禁止半残行上线。
	inline bool BiographyFieldsValid(std::string_view archiveName,
									 std::string_view nickname,
									 std::string_view biographySummary,
									 std::string_view storyTitle,
									 std::string_view storyBody)
	{
		const bool hasArchive = !IsBlankText(archiveName);
		const bool hasNickname = !IsBlankText(nickname);
		const bool hasSummary = !IsBlankText(biographySummary);
		const bool hasTitle = !IsBlankText(storyTitle);
		const bool hasBody = !IsBlankText(storyBody);
		const bool hasAny = hasArchive || hasNickname || hasSummary || hasTitle || hasBody;
		if (!hasAny)
		{
			return true;
		}
		return hasArchive && hasSummary && hasTitle && hasBody;
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

	// ── 灵宠幻化与技能特效纯规则（纯表现零数值） ──

	inline std::optional<uint32_t> PetCosmeticBitIndex(uint32_t configId)
	{
		return GetBitIndex(PetCosmeticBitMap, configId);
	}

	inline bool IsPetCosmeticOwned(std::string_view bits, uint32_t configId)
	{
		const auto idx = PetCosmeticBitIndex(configId);
		return idx.has_value() && TestPackedBit(bits, *idx);
	}

	inline bool MarkPetCosmeticOwned(std::string &bits, uint32_t configId)
	{
		const auto idx = PetCosmeticBitIndex(configId);
		if (!idx.has_value())
		{
			return false;
		}
		SetPackedBit(bits, *idx);
		return true;
	}

	inline std::optional<uint32_t> SkillVfxBitIndex(uint32_t configId)
	{
		return GetBitIndex(SkillVfxCosmeticBitMap, configId);
	}

	inline bool IsSkillVfxOwned(std::string_view bits, uint32_t configId)
	{
		const auto idx = SkillVfxBitIndex(configId);
		return idx.has_value() && TestPackedBit(bits, *idx);
	}

	inline bool MarkSkillVfxOwned(std::string &bits, uint32_t configId)
	{
		const auto idx = SkillVfxBitIndex(configId);
		if (!idx.has_value())
		{
			return false;
		}
		SetPackedBit(bits, *idx);
		return true;
	}

	// 灵宠幻化物种适配：targetPetTableId == 0 表示通用，所有灵宠均可幻化。
	inline bool IsPetSpeciesCompatible(uint32_t targetPetTableId, uint32_t petTableId)
	{
		return targetPetTableId == 0 || targetPetTableId == petTableId;
	}

	// 特效色调配置：空串表示原色，非空必须为严格 #RRGGBB 十六进制。
	inline bool IsValidTintHex(std::string_view value)
	{
		if (value.empty())
		{
			return true;
		}
		if (value.size() != 7 || value[0] != '#')
		{
			return false;
		}
		for (std::size_t i = 1; i < value.size(); ++i)
		{
			const char c = value[i];
			const bool isDigit = (c >= '0' && c <= '9');
			const bool isUpper = (c >= 'A' && c <= 'F');
			const bool isLower = (c >= 'a' && c <= 'f');
			if (!isDigit && !isUpper && !isLower)
			{
				return false;
			}
		}
		return true;
	}
} // namespace player_appearance

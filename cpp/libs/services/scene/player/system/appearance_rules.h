#pragma once

// 角色形象切换与一小时试用的纯规则函数(docs/design/character-appearance-switch.md)。
// 不碰 ECS、不读表管理器、不取时钟,供 player_appearance.cpp 与单测共用。

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

#include "engine/core/utils/bit_index/bit_index_util.h"
#include "table/code/bit_index/characterappearance_table_id_bit_index.h"
#include "table/code/bit_index/cosmeticshowcase_table_id_bit_index.h"
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

	// 支付撤单连坐回收时清除指定角色形象拥有位(保留其他位与未知高位)。
	inline bool ClearOwned(std::string &ownedBits, uint32_t configId)
	{
		const auto idx = AppearanceBitIndex(configId);
		if (!idx.has_value())
		{
			return false;
		}
		ClearPackedBit(ownedBits, *idx);
		return true;
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

	// ── Steam 式按缺口 8.5 折系列补齐包纯规则 ──
	inline constexpr uint32_t kBundleDiscountBpsBase = 10'000u;
	inline constexpr uint32_t kMinBundleDiscountBps = 5'000u;
	inline constexpr uint32_t kMaxBundleDiscountBps = 9'999u;
	inline constexpr uint32_t kMaxBundleUnitPriceDiamond = 1'000'000u;

	inline bool BundlePricingValid(uint32_t discountBps, uint32_t unitPriceDiamond)
	{
		if (discountBps == 0)
		{
			return true;
		}
		return discountBps >= kMinBundleDiscountBps && discountBps <= kMaxBundleDiscountBps &&
			   unitPriceDiamond > 0 && unitPriceDiamond <= kMaxBundleUnitPriceDiamond;
	}

	// 缺口 >= 2 整册打包，或总成员 >= 2 且已拥有 >= 1 时仅差 1 人同样享 Steam 式补齐折价。
	inline bool IsSeriesBundleEligible(uint32_t discountBps,
									   uint32_t unitPriceDiamond,
									   uint32_t totalCount,
									   uint32_t ownedCount,
									   uint32_t missingUnlockableCount)
	{
		if (discountBps == 0 || unitPriceDiamond == 0 || missingUnlockableCount == 0)
		{
			return false;
		}
		return missingUnlockableCount >= 2u || (totalCount >= 2u && ownedCount >= 1u);
	}

	// 整数向上取整，绝不出现 0 灵石漏洞。
	inline uint64_t ComputeSeriesBundleDiscountPrice(uint32_t missingCount,
													 uint32_t unitPriceDiamond,
													 uint32_t discountBps)
	{
		if (missingCount == 0 || unitPriceDiamond == 0 || discountBps == 0)
		{
			return 0ULL;
		}
		const uint64_t original = static_cast<uint64_t>(missingCount) * static_cast<uint64_t>(unitPriceDiamond);
		return (original * static_cast<uint64_t>(discountBps) + (kBundleDiscountBpsBase - 1u)) / kBundleDiscountBpsBase;
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

	inline bool ClearPetCosmeticOwned(std::string &bits, uint32_t configId)
	{
		const auto idx = PetCosmeticBitIndex(configId);
		if (!idx.has_value())
		{
			return false;
		}
		ClearPackedBit(bits, *idx);
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

	inline bool ClearSkillVfxOwned(std::string &bits, uint32_t configId)
	{
		const auto idx = SkillVfxBitIndex(configId);
		if (!idx.has_value())
		{
			return false;
		}
		ClearPackedBit(bits, *idx);
		return true;
	}

	// ── 批次 B2：排面装饰、全城同乐社交耗材与好友纯外观/手记赠礼纯规则 ──
	inline constexpr uint32_t kShowcaseSlotFormation = 1u;
	inline constexpr uint32_t kShowcaseSlotEntranceVictory = 2u;
	inline constexpr uint32_t kShowcaseSlotChatBubble = 3u;
	inline constexpr uint32_t kShowcaseSlotShareCard = 4u;
	inline constexpr uint32_t kShowcaseSlotProfileCard = 5u;

	inline bool IsValidShowcaseSlot(uint32_t slotKind)
	{
		return slotKind >= kShowcaseSlotFormation && slotKind <= kShowcaseSlotProfileCard;
	}

	inline std::optional<uint32_t> ShowcaseCosmeticBitIndex(uint32_t configId)
	{
		return GetBitIndex(CosmeticShowcaseBitMap, configId);
	}

	inline bool IsShowcaseCosmeticOwned(std::string_view bits, uint32_t configId)
	{
		const auto idx = ShowcaseCosmeticBitIndex(configId);
		return idx.has_value() && TestPackedBit(bits, *idx);
	}

	inline bool MarkShowcaseCosmeticOwned(std::string &bits, uint32_t configId)
	{
		const auto idx = ShowcaseCosmeticBitIndex(configId);
		if (!idx.has_value())
		{
			return false;
		}
		SetPackedBit(bits, *idx);
		return true;
	}

	inline bool ClearShowcaseCosmeticOwned(std::string &bits, uint32_t configId)
	{
		const auto idx = ShowcaseCosmeticBitIndex(configId);
		if (!idx.has_value())
		{
			return false;
		}
		ClearPackedBit(bits, *idx);
		return true;
	}

	// 主城全城同乐社交气氛道具规则（仅发 CURRENCY_TYPE_BIND_GOLD=3，周上限 3000，同分线并发上限 5）。
	inline constexpr uint32_t kCelebrationItemLantern = 3418u;
	inline constexpr uint32_t kCelebrationItemFirework = 3419u;
	inline constexpr uint32_t kCelebrationItemTeaTable = 3420u;
	inline constexpr uint64_t kCelebrationDurationSec = 120ULL;
	inline constexpr uint32_t kCelebrationMaxActivePerScene = 5u;
	inline constexpr uint32_t kCelebrationMinLevel = 10u;
	inline constexpr uint32_t kCelebrationWeeklyBindGoldCap = 3000u;

	inline uint32_t CelebrationBindGoldReward(uint32_t itemConfigId)
	{
		switch (itemConfigId)
		{
		case kCelebrationItemLantern:
			return 150u;
		case kCelebrationItemFirework:
			return 300u;
		case kCelebrationItemTeaTable:
			return 400u;
		default:
			return 0u;
		}
	}

	inline bool IsSafeCitySceneConfig(uint32_t sceneConfigId)
	{
		return sceneConfigId == 1u || sceneConfigId == 4u;
	}

	// 好友纯绑定外观/手记典藏赠礼规则（严守 7 天好友无拉黑 + 每周 5 笔 + 仅扣真实灵石）。
	inline constexpr uint64_t kCosmeticGiftMinFriendDurationMs = 7ULL * 86'400ULL * 1000ULL;
	inline constexpr uint32_t kCosmeticGiftWeeklyLimit = 5u;
	inline constexpr uint64_t kCosmeticGiftCharacterDiamondPrice = 100ULL;
	inline constexpr uint64_t kCosmeticGiftPetDiamondPrice = 120ULL;
	inline constexpr uint64_t kCosmeticGiftSkillVfxDiamondPrice = 180ULL;
	inline constexpr uint64_t kCosmeticGiftShowcaseDiamondPrice = 80ULL;

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

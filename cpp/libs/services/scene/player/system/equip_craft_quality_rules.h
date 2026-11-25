#pragma once

#include <algorithm>
#include <array>
#include <cstdint>

namespace equipcraft {

inline constexpr uint32_t kPolishSandConfigId = 3403;
inline constexpr uint32_t kPolishSandCount = 1;
inline constexpr uint64_t kPolishGoldCost = 20;

inline constexpr uint32_t kReforgeSandConfigId = 3403;
inline constexpr uint32_t kReforgeSandCount = 1;
inline constexpr uint64_t kReforgeGoldCost = 20;

inline constexpr uint32_t kGuaranteeCharmConfigId = 3405;
inline constexpr uint32_t kScrollFragmentConfigId = 3406;
inline constexpr uint32_t kRareScrollPageConfigId = 3407;
inline constexpr uint32_t kRareScrollInkConfigId = 3408;

inline constexpr uint32_t kMinQualityTier = 1;
inline constexpr uint32_t kMaxQualityTier = 5;
inline constexpr uint32_t kPolishPityLimit = 8;
inline constexpr uint32_t kSpeedAffixEffect = 4; // G02 速度词条豁免品相随机，固定满值。
inline constexpr uint32_t kQualityWeightTotalBps = 10000;

inline constexpr uint32_t kMasteryApprentice = 1;
inline constexpr uint32_t kMasteryArtisan = 2;
inline constexpr uint32_t kMasteryMaster = 3;
inline constexpr uint32_t kArtisanExpThreshold = 20;
inline constexpr uint32_t kMasterExpThreshold = 60;

inline constexpr uint64_t kProxyFeeMinGold = 50;
inline constexpr uint64_t kProxyFeeMaxGold = 100;
inline constexpr uint64_t kProxyTaxBps = 500; // 5% 主城代打摊位银两税销毁。

inline constexpr uint32_t kScrollBonusFragmentsCount = 2;
inline constexpr uint32_t kMaxRareSkillRatioBps = 1000; // 永久技能稀世占比硬红线 <= 10%。

inline constexpr bool IsPolishExempt(uint32_t effect) {
    return effect == kSpeedAffixEffect;
}

inline constexpr uint32_t EffectiveQualityTier(uint32_t storedTier, bool exempt) {
    if (exempt || storedTier == 0) {
        return kMaxQualityTier; // 旧存档 quality_tier==0 按满值 5 档兼容。
    }
    return std::clamp(storedTier, kMinQualityTier, kMaxQualityTier);
}

inline constexpr uint32_t DefaultQualityRatioPct(uint32_t tier) {
    switch (tier) {
    case 1: return 80;
    case 2: return 85;
    case 3: return 90;
    case 4: return 95;
    default: return 100;
    }
}

inline constexpr const char* DefaultQualityName(uint32_t tier) {
    switch (tier) {
    case 1: return "凡品";
    case 2: return "良品";
    case 3: return "上品";
    case 4: return "珍品";
    default: return "极品";
    }
}

inline constexpr uint32_t ScaleValueByQuality(uint32_t maxValueBps, uint32_t ratioPct) {
    const uint32_t clamped = std::clamp(ratioPct, 1u, 100u);
    return static_cast<uint32_t>((uint64_t{maxValueBps} * clamped) / 100u);
}

inline constexpr uint32_t RollQualityTier(const std::array<uint32_t, kMaxQualityTier>& weightsBps,
                                          uint32_t rollBps) {
    uint32_t acc = 0;
    for (uint32_t i = 0; i < kMaxQualityTier; ++i) {
        acc += weightsBps[i];
        if (rollBps < acc) {
            return i + 1;
        }
    }
    return kMaxQualityTier;
}

struct PolishResult {
    uint32_t newTier{kMaxQualityTier};
    uint32_t newPityCount{0};
    bool guaranteedByPity{false};
};

// 只升不降 + 累计第 8 次必达极品（或定星保底符直接必达极品）。
inline constexpr PolishResult ResolvePolish(uint32_t currentTier, uint32_t currentPity,
                                            uint32_t rolledTier, bool useGuaranteeCharm) {
    const uint32_t baseTier = std::clamp(currentTier, kMinQualityTier, kMaxQualityTier);
    const uint32_t rolled = std::clamp(rolledTier, kMinQualityTier, kMaxQualityTier);
    if (useGuaranteeCharm || currentPity + 1 >= kPolishPityLimit) {
        return {kMaxQualityTier, 0, true};
    }
    const uint32_t nextTier = std::max(baseTier, rolled);
    if (nextTier >= kMaxQualityTier) {
        return {kMaxQualityTier, 0, false};
    }
    return {nextTier, currentPity + 1, false};
}

inline constexpr uint32_t MasteryTierForExp(uint32_t exp) {
    if (exp >= kMasterExpThreshold) return kMasteryMaster;
    if (exp >= kArtisanExpThreshold) return kMasteryArtisan;
    return kMasteryApprentice;
}

inline constexpr uint32_t MasteryNextExp(uint32_t exp) {
    if (exp >= kMasterExpThreshold) return 0;
    if (exp >= kArtisanExpThreshold) return kMasterExpThreshold;
    return kArtisanExpThreshold;
}

inline constexpr const char* MasteryTierName(uint32_t tier) {
    switch (tier) {
    case kMasteryMaster: return "宗师";
    case kMasteryArtisan: return "巧匠";
    default: return "学徒";
    }
}

inline constexpr uint32_t ForgeTopQualityRateBps(uint32_t tier) {
    switch (tier) {
    case kMasteryMaster: return 1000;
    case kMasteryArtisan: return 700;
    default: return 500;
    }
}

inline constexpr uint32_t ScrollBonusFragmentRateBps(uint32_t tier) {
    switch (tier) {
    case kMasteryMaster: return 1000;
    case kMasteryArtisan: return 500;
    default: return 0;
    }
}

inline constexpr bool ValidProxyFee(uint64_t grossFee) {
    return grossFee >= kProxyFeeMinGold && grossFee <= kProxyFeeMaxGold;
}

inline constexpr uint64_t ProxyTaxGold(uint64_t grossFee) {
    return grossFee == 0 ? 0 : (grossFee * kProxyTaxBps + 9999u) / 10000u;
}

inline constexpr uint64_t ProxyNetGold(uint64_t grossFee) {
    const uint64_t tax = ProxyTaxGold(grossFee);
    return grossFee > tax ? grossFee - tax : 0;
}

} // namespace equipcraft

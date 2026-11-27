#pragma once

#include <algorithm>
#include <array>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

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
inline constexpr uint32_t kUnsealTalismanConfigId = 3409;   // 解封剥离符：清空绑定养成，仅恢复纯 T 原胚。
inline constexpr uint32_t kInheritTalismanConfigId = 3410;  // 流派副套继承符：无损继承强化等级至同部位副套（目标转绑定）。
inline constexpr uint32_t kBoundArtifactVoucherConfigId = 3404;
inline constexpr uint32_t kSealedSkillBookConfigId = 3414;  // 封卷道籍：书架内可交易常规技能书封卷入包实体道具（不可堆叠、可流通）。

// 策划口径永久仅自用绑定道具（商店 / 邮件 / 系统入包均强制 bound = true，禁止经任何发奖通道洗成可交易）。
inline constexpr bool IsAlwaysBoundItem(uint32_t configId) {
    return configId == kBoundArtifactVoucherConfigId ||
           configId == kGuaranteeCharmConfigId ||
           configId == kRareScrollInkConfigId ||
           configId == kUnsealTalismanConfigId ||
           configId == kInheritTalismanConfigId;
}

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

// V1 装备原地无损升档（基础装 profile=1 / 首饰 profile=2）：纯银两消耗，每跨一档（10级）按档计费，0 打造材料，不污染可交易状态。
inline constexpr uint64_t kUpgradeTierBaseGoldPerStep = 100;
inline constexpr uint64_t kUpgradeTierAccessoryGoldPerStep = 60;

inline constexpr bool CanUpgradeTier(uint32_t sourceProfile, uint32_t sourceKind, uint32_t sourceLevel,
                                     uint32_t sourceClass, uint32_t targetProfile, uint32_t targetKind,
                                     uint32_t targetLevel, uint32_t targetClass) {
    if (sourceProfile != 1u && sourceProfile != 2u) return false;
    if (targetProfile != sourceProfile || targetKind != sourceKind || targetClass != sourceClass) return false;
    return targetLevel > sourceLevel;
}

inline constexpr uint64_t UpgradeTierGoldCost(uint32_t equipProfile, uint32_t sourceLevel, uint32_t targetLevel) {
    if ((equipProfile != 1u && equipProfile != 2u) || targetLevel <= sourceLevel) return 0u;
    const uint32_t srcTier = sourceLevel / 10u;
    const uint32_t dstTier = targetLevel / 10u;
    const uint64_t steps = dstTier > srcTier ? static_cast<uint64_t>(dstTier - srcTier) : 1ull;
    const uint64_t perStep = equipProfile == 1u ? kUpgradeTierBaseGoldPerStep : kUpgradeTierAccessoryGoldPerStep;
    return steps * perStep;
}

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

// ── V2 名匠江湖口碑、预设招牌枚举与 6 档零属性雅号印款 ──
inline constexpr uint32_t kMaxMasterShowcaseTag = 4;
inline constexpr uint32_t kCraftTitleMaxId = 6;
inline constexpr uint32_t kCraftTitleBailianSupremeReq = 5;
inline constexpr uint32_t kCraftTitleTiangongSupremeReq = 20;
inline constexpr uint32_t kCraftTitleWanjuanRareBooksReq = 3;
inline constexpr uint32_t kCraftTitleWanjuanOrdersReq = 15;

inline constexpr const char* MasterShowcaseTagName(uint32_t tag) {
    switch (tag) {
    case 1: return "专攻极品铸器";
    case 2: return "专攻典籍制卷";
    case 3: return "平价快单接单";
    case 4: return "双绝天工名匠";
    default: return "综合宗师";
    }
}

inline constexpr const char* CraftTitleName(uint32_t titleId) {
    switch (titleId) {
    case 1: return "铸器宗师";
    case 2: return "百炼宗师";
    case 3: return "天工神匠";
    case 4: return "制卷宗师";
    case 5: return "万卷书圣";
    case 6: return "双绝天工";
    default: return "";
    }
}

inline constexpr bool IsCraftTitleUnlocked(uint32_t titleId, uint32_t forgeExp, uint32_t scrollExp,
                                           uint32_t supremeForgedCount, uint32_t rareBooksCraftedCount,
                                           uint32_t proxyOrdersCompleted) {
    const bool forgeMaster = MasteryTierForExp(forgeExp) >= kMasteryMaster;
    const bool scrollMaster = MasteryTierForExp(scrollExp) >= kMasteryMaster;
    switch (titleId) {
    case 1: return forgeMaster;
    case 2: return forgeMaster && supremeForgedCount >= kCraftTitleBailianSupremeReq;
    case 3: return forgeMaster && supremeForgedCount >= kCraftTitleTiangongSupremeReq;
    case 4: return scrollMaster;
    case 5: return scrollMaster &&
                   (rareBooksCraftedCount >= kCraftTitleWanjuanRareBooksReq ||
                    proxyOrdersCompleted >= kCraftTitleWanjuanOrdersReq);
    case 6: return forgeMaster && scrollMaster;
    default: return false;
    }
}

inline std::vector<uint32_t> UnlockedCraftTitleIds(uint32_t forgeExp, uint32_t scrollExp,
                                                   uint32_t supremeForgedCount, uint32_t rareBooksCraftedCount,
                                                   uint32_t proxyOrdersCompleted) {
    std::vector<uint32_t> out;
    for (uint32_t id = 1; id <= kCraftTitleMaxId; ++id) {
        if (IsCraftTitleUnlocked(id, forgeExp, scrollExp, supremeForgedCount, rareBooksCraftedCount,
                                 proxyOrdersCompleted)) {
            out.push_back(id);
        }
    }
    return out;
}

inline constexpr uint32_t UnlockedCraftTitleMask(uint32_t forgeExp, uint32_t scrollExp,
                                                 uint32_t supremeForgedCount, uint32_t rareBooksCraftedCount,
                                                 uint32_t proxyOrdersCompleted) {
    uint32_t mask = 0u;
    for (uint32_t id = 1; id <= kCraftTitleMaxId; ++id) {
        if (IsCraftTitleUnlocked(id, forgeExp, scrollExp, supremeForgedCount, rareBooksCraftedCount,
                                 proxyOrdersCompleted)) {
            mask |= (1u << (id - 1u));
        }
    }
    return mask;
}

inline constexpr uint32_t NewlyUnlockedCraftTitleMask(uint32_t beforeMask, uint32_t afterMask) {
    return afterMask & ~beforeMask;
}

inline constexpr uint32_t kHotRankTopLimit = 3;

inline constexpr uint64_t HotRankScore(uint32_t recent24hOrders, uint32_t recent24hSupreme) {
    return uint64_t{recent24hSupreme} * 3ull + uint64_t{recent24hOrders};
}

inline std::string FormatMasterSignature(uint32_t equippedTitleId, std::string_view displayName) {
    const char* title = CraftTitleName(equippedTitleId);
    if (title != nullptr && title[0] != '\0') {
        return std::string(title) + "·" + std::string(displayName);
    }
    return std::string(displayName);
}

// ── V3 工坊智能定档连磨 ──
inline constexpr uint32_t kMaxBatchPolishAttempts = kPolishPityLimit;

// ── V4 典籍藏经阁里程碑位图与批量安全拆页 ──
inline constexpr uint32_t kScrollMilestoneHalfTier = 1;
inline constexpr uint32_t kScrollMilestoneFullTier = 2;
inline constexpr uint32_t kScrollMilestoneHalfRewardFragments = 20;
inline constexpr uint32_t kScrollMilestoneFullRewardFragments = 40;
inline constexpr uint32_t kMaxBatchDismantleCount = 20;

// 未学书架(PlayerCraftComp.skill_books)同时至多存的本数(r2-craft#2):存档里的集合要有上界(每行 1 MiB、每次整行重写)。
// 100 = 95 项永久技能各一本还有余量;满了先研习、拆页或封卷。加书的三处(制书、开卷、揭榜送书)都在副作用之前判。
inline constexpr uint32_t kMaxBookshelfBooks = 100;

inline constexpr uint32_t ScrollHalfThreshold(uint32_t totalSkills) {
    return totalSkills == 0 ? 0u : (totalSkills + 1u) / 2u;
}

inline constexpr uint32_t ScrollMilestoneBitIndex(uint32_t scrollId, uint32_t tier) {
    if (scrollId == 0 || (tier != kScrollMilestoneHalfTier && tier != kScrollMilestoneFullTier)) {
        return 0u;
    }
    return (scrollId - 1u) * 2u + (tier - 1u);
}

inline constexpr uint32_t ScrollMilestoneRewardFragments(uint32_t tier) {
    if (tier == kScrollMilestoneHalfTier) return kScrollMilestoneHalfRewardFragments;
    if (tier == kScrollMilestoneFullTier) return kScrollMilestoneFullRewardFragments;
    return 0u;
}

inline bool HasBit(std::string_view bits, uint32_t bitIndex) {
    const std::size_t byteIdx = bitIndex / 8u;
    if (byteIdx >= bits.size()) return false;
    const auto byteVal = static_cast<unsigned char>(bits[byteIdx]);
    return (byteVal & static_cast<unsigned char>(1u << (bitIndex % 8u))) != 0u;
}

inline void SetBit(std::string& bits, uint32_t bitIndex) {
    const std::size_t byteIdx = bitIndex / 8u;
    if (byteIdx >= bits.size()) {
        bits.resize(byteIdx + 1u, '\0');
    }
    auto byteVal = static_cast<unsigned char>(bits[byteIdx]);
    byteVal = static_cast<unsigned char>(byteVal | static_cast<unsigned char>(1u << (bitIndex % 8u)));
    bits[byteIdx] = static_cast<char>(byteVal);
    while (!bits.empty() && bits.back() == '\0') {
        bits.pop_back();
    }
}

// ── V5 西市常规技能书悬赏求购板 ──
inline constexpr uint64_t kWantedOrderMinGold = 100;
inline constexpr uint64_t kWantedOrderMaxGold = 50000;
inline constexpr uint32_t kMaxActiveWantedOrders = 3;

inline constexpr bool ValidWantedOrderGold(uint64_t rewardGold) {
    return rewardGold >= kWantedOrderMinGold && rewardGold <= kWantedOrderMaxGold;
}

} // namespace equipcraft

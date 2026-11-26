#pragma once

#include <algorithm>
#include <cstdint>

namespace turnbattle {
namespace bossaffix {

// 首领试炼周常五行词缀纯规则（零依赖、零时钟、全确定性；与 Java BossTrialWeeklyAffixRules / Unity 逐项一致）。
// 仅在真实首领试炼（IsBossTrialKind(boss_trial_kind) && !hasTowerContext）生效，
// 与既有 10/15 份装备材料奖励池完全共享，零新增肝度、零新增付费点。
inline constexpr uint32_t kAffixNone  = 0u;
inline constexpr uint32_t kAffixMetal = 1u; // 金·锋芒破阵周：破绽暴击+15%/主目标+10% | 敌方开局8%金甲护盾
inline constexpr uint32_t kAffixWood  = 2u; // 木·灵脉涤尘周：驱散/净化回12%法力+全队增伤10% | 首领R2/5/8末回4%血
inline constexpr uint32_t kAffixWater = 3u; // 水·蓄势聚灵周：蓄力/毒火增伤20%且耗蓝-20% | 首领偶数回合末+1层蓄势
inline constexpr uint32_t kAffixFire  = 4u; // 火·烈焰连携周：灵宠直攻/追击+18%+破绽灼伤3%(封顶1200) | 首领<=60%提前二阶段
inline constexpr uint32_t kAffixEarth = 5u; // 土·守御反震周：化劲破绽赐全队8%护盾+下回合全伤+12% | 首领慢重劈基础+12%
inline constexpr uint32_t kMaxAffixId = 5u;

// UTC+8 周一 05:00（= UTC 周日 21:00）切周基准，与全服材料周历 FIRST_MONDAY_05_UTC8_MS 逐毫秒同源。
inline constexpr uint64_t kDayMs = 86'400'000ULL;
inline constexpr uint64_t kWeekMs = 7ULL * kDayMs;
inline constexpr uint64_t kFirstMonday05Utc8Ms = 3ULL * kDayMs + 21ULL * 3'600'000ULL;

// 5 条周常五行词缀定点数标尺（整数百分比/数值，零跨平台浮点漂移）。
inline constexpr uint32_t kMetalExposedCritBonusPct = 15u;
inline constexpr uint32_t kMetalExposedPrimaryBonusPct = 10u;
inline constexpr uint64_t kMetalEnemyOpeningShieldPct = 8ULL;

inline constexpr uint64_t kWoodDispelManaRegenPct = 12ULL;
inline constexpr uint32_t kWoodTeamDamageBonusPct = 10u;
inline constexpr uint64_t kWoodBossRegenHpPct = 4ULL;

inline constexpr uint32_t kWaterChargeOrPeriodicDamageBonusPct = 20u;
inline constexpr uint64_t kWaterManaKeepPct = 80ULL;

inline constexpr uint32_t kFirePetOrFollowUpDamageBonusPct = 18u;
inline constexpr uint64_t kFireExposedBurnMaxHpPct = 3ULL;
inline constexpr uint64_t kFireExposedBurnCap = 1200ULL;
inline constexpr uint64_t kFireBossPhaseTwoHpPct = 60ULL;

inline constexpr uint64_t kEarthParryTeamShieldPct = 8ULL;
inline constexpr uint32_t kEarthParryNextRoundDamageBonusPct = 12u;
inline constexpr double kEarthBossCleaveMultiplier = 1.12;

constexpr bool IsValidOverride(uint32_t affixId) {
    return affixId <= kMaxAffixId;
}

constexpr uint32_t AffixIdForTimestamp(uint64_t nowMs) {
    if (nowMs < kFirstMonday05Utc8Ms) {
        return kAffixNone;
    }
    const uint64_t weekIndex = (nowMs - kFirstMonday05Utc8Ms) / kWeekMs;
    return static_cast<uint32_t>((weekIndex % kMaxAffixId) + 1ULL);
}

constexpr uint32_t ResolveForBattle(bool isBossTrial, bool hasTowerContext,
                                    uint64_t createdAtMs, uint32_t explicitAffixId) {
    if (!isBossTrial || hasTowerContext) {
        return kAffixNone;
    }
    if (explicitAffixId >= 1u && explicitAffixId <= kMaxAffixId) {
        return explicitAffixId;
    }
    return AffixIdForTimestamp(createdAtMs);
}

constexpr bool IsWoodRegenRound(uint32_t roundIndex) {
    return roundIndex >= 2u && ((roundIndex - 2u) % 3u) == 0u;
}

constexpr bool IsWaterMomentumRound(uint32_t roundIndex) {
    return roundIndex >= 2u && (roundIndex % 2u) == 0u;
}

constexpr uint64_t DiscountWaterManaCost(uint64_t originalCost) {
    if (originalCost == 0ULL) {
        return 0ULL;
    }
    const uint64_t discounted = (originalCost * kWaterManaKeepPct) / 100ULL;
    return discounted > 0ULL ? discounted : 1ULL;
}

// 溢出安全的气血比例门槛判定：health / maxHealth <= pct / 100。
constexpr bool IsHealthAtOrBelowPct(uint64_t health, uint64_t maxHealth, uint64_t pct) {
    if (maxHealth == 0ULL) {
        return false;
    }
    const uint64_t threshold = (maxHealth / 100ULL) * pct + ((maxHealth % 100ULL) * pct) / 100ULL;
    return health <= threshold;
}

} // namespace bossaffix
} // namespace turnbattle

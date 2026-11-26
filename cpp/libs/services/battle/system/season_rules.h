#pragma once

#include <algorithm>
#include <cstdint>
#include <limits>
#include "class_skill_rules.h"
#include "equipment_battle_rules.h"

// 赛季技能与单槽赛季印（方案 A）的纯规则：不查表、不碰 ECS、不取墙钟、不打日志。
// Scene 与 Battle 共用同一份校验口径，避免双进程分叉（docs/design/season-skills-and-seal-design.md）。
namespace seasonrules {

// 硬安全上限：即便配置表误填高数值，赛季印纯面板轻属性也绝不允许超过 5%（500 万分比）。
inline constexpr uint32_t kHardStatCapBps = 500;
inline constexpr uint32_t kHardMaxProgressPoints = 1000;
inline constexpr uint64_t kHardMaxCatchupPricePerPoint = 1000000000ULL;
inline constexpr uint32_t kMaxValidCurrencyType = 2; // 0 银两 / 1 灵石 / 2 绑定灵石

struct RuleParams {
    uint32_t seasonId{0};
    uint64_t startsAtMs{0};
    uint64_t endsAtMs{0};
    bool enabled{false};
    uint32_t sealMaxPoints{0};
    uint32_t pveWinPoints{0};
    uint32_t pvpWinPoints{0};
    uint32_t pvpOtherPoints{0};
    uint32_t maxHealthBpsCap{0};
    uint32_t defenseBpsCap{0};
    uint32_t catchupCurrencyType{0};
    uint64_t catchupPricePerPoint{0};
    uint32_t defaultSealId{0};
};

struct CatchupPlan {
    uint32_t pointsToBuy{0};
    uint64_t totalPrice{0};
};

// 赛季印单局结算增量（用于近期战果持久落盘与 BattleEndS2C 实时跳条展示）。
struct BattleCreditDelta {
    uint32_t seasonId{0};
    uint32_t pointsAdded{0};
    uint32_t progressAfter{0};
    uint32_t maxPoints{0};
    bool honorNewlyUnlocked{false};
};

// 赛季全员对称周环境词缀（零养成门槛、敌我对称、每周自动轮换；0 表示无词缀）。
inline constexpr uint32_t kWeeklyAffixNone = 0;
inline constexpr uint32_t kWeeklyAffixShieldOverload = 1;  // 护盾过载周：双方技能护盾量 +15%
inline constexpr uint32_t kWeeklyAffixSpiritRecovery = 2;  // 灵息充盈周：双方普通防御额外回复 3% 最大法力
inline constexpr uint32_t kWeeklyAffixOpeningGuard = 3;    // 先机守势周：前 2 回合双方受到直伤 -8%
inline constexpr uint32_t kWeeklyAffixSharpTempo = 4;      // 锋芒竞速周：双方攻击无护盾目标直伤 +5%
inline constexpr uint32_t kWeeklyAffixCount = 4;

inline constexpr uint32_t kWeeklyAffixShieldBonusPct = 15;
inline constexpr uint32_t kWeeklyAffixDefendManaBps = 300;
inline constexpr uint32_t kWeeklyAffixOpeningGuardReducePct = 8;
inline constexpr uint32_t kWeeklyAffixOpeningGuardRounds = 2;
inline constexpr uint32_t kWeeklyAffixSharpTempoBonusPct = 5;

inline bool IsValidWeeklyAffixId(uint32_t affixId) {
    return affixId >= 1u && affixId <= kWeeklyAffixCount;
}

// 计算当前权威赛季周次（1 起）：有明确开季时间时按 7 天递增；首测常开（startsAtMs == 0）对齐 UTC+8 周一 05:00。
inline uint32_t ComputeSeasonWeekIndex(uint64_t startsAtMs, uint64_t nowMs) {
    constexpr uint64_t kMsPerWeek = 7ULL * 24ULL * 3600ULL * 1000ULL;
    if (startsAtMs > 0) {
        if (nowMs <= startsAtMs) {
            return 1u;
        }
        return static_cast<uint32_t>((nowMs - startsAtMs) / kMsPerWeek) + 1u;
    }
    constexpr uint64_t kMonday05CstOffsetMs = (3ULL * 24ULL - 3ULL) * 3600ULL * 1000ULL;
    return static_cast<uint32_t>((nowMs + kMonday05CstOffsetMs) / kMsPerWeek) + 1u;
}

// 由赛季编号与周次确定性轮换全员对称周词缀（1..4）；任一为 0 返回 0。
inline uint32_t WeeklyAffixForWeek(uint32_t seasonId, uint32_t weekIndex) {
    if (seasonId == 0 || weekIndex == 0) {
        return kWeeklyAffixNone;
    }
    return ((seasonId - 1u + weekIndex - 1u) % kWeeklyAffixCount) + 1u;
}

inline uint32_t WeeklyAffixAt(uint32_t seasonId, uint64_t startsAtMs, uint64_t nowMs) {
    if (seasonId == 0) {
        return kWeeklyAffixNone;
    }
    return WeeklyAffixForWeek(seasonId, ComputeSeasonWeekIndex(startsAtMs, nowMs));
}

// 推荐搭配该周词缀的赛季印效果类型（用于面板高亮，不限制自由切换）。
inline bool IsSealEffectRecommendedForAffix(uint32_t affixId, uint32_t effectId) {
    switch (affixId) {
    case kWeeklyAffixShieldOverload:
        return effectId == 1u || effectId == 2u; // 物理/法术破盾印
    case kWeeklyAffixSpiritRecovery:
        return effectId == 7u || effectId == 6u; // 防御回蓝印 / 固阵守印
    case kWeeklyAffixOpeningGuard:
        return effectId == 6u || effectId == 7u; // 前两回合减伤印 / 蓄息印
    case kWeeklyAffixSharpTempo:
        return effectId == 1u || effectId == 2u; // 破盾后迅速打穿
    default:
        return false;
    }
}

inline uint32_t ShieldOverloadBonusPct(uint32_t affixId) {
    return affixId == kWeeklyAffixShieldOverload ? kWeeklyAffixShieldBonusPct : 0u;
}

inline uint32_t DefendBonusManaRegenBps(uint32_t affixId) {
    return affixId == kWeeklyAffixSpiritRecovery ? kWeeklyAffixDefendManaBps : 0u;
}

inline uint32_t OpeningGuardDamageReducePct(uint32_t affixId, uint32_t roundIndex) {
    return (affixId == kWeeklyAffixOpeningGuard && roundIndex >= 1u && roundIndex <= kWeeklyAffixOpeningGuardRounds)
        ? kWeeklyAffixOpeningGuardReducePct
        : 0u;
}

inline uint32_t SharpTempoUnshieldedDamageBonusPct(uint32_t affixId, bool targetHasShield) {
    return (affixId == kWeeklyAffixSharpTempo && !targetHasShield) ? kWeeklyAffixSharpTempoBonusPct : 0u;
}

inline bool IsWindowActive(uint64_t startsAtMs, uint64_t endsAtMs, bool enabled, uint64_t nowMs) {
    if (!enabled) {
        return false;
    }
    if (endsAtMs != 0 && endsAtMs <= startsAtMs) {
        return false;
    }
    if (nowMs < startsAtMs) {
        return false;
    }
    return endsAtMs == 0 || nowMs < endsAtMs;
}

inline bool WindowsOverlap(uint64_t startA, uint64_t endA, uint64_t startB, uint64_t endB) {
    const bool aBeforeB = (endA != 0 && endA <= startB);
    const bool bBeforeA = (endB != 0 && endB <= startA);
    return !aBeforeB && !bBeforeA;
}

inline const char* ValidateRule(const RuleParams& rule) {
    if (rule.seasonId == 0) {
        return "season_id must be non-zero";
    }
    if (rule.endsAtMs != 0 && rule.endsAtMs <= rule.startsAtMs) {
        return "ends_at_ms must be 0 or greater than starts_at_ms";
    }
    if (rule.sealMaxPoints == 0 || rule.sealMaxPoints > kHardMaxProgressPoints) {
        return "seal_max_points out of range [1, 1000]";
    }
    if (rule.maxHealthBpsCap > kHardStatCapBps || rule.defenseBpsCap > kHardStatCapBps) {
        return "season stat cap exceeds hard limit 500 bps";
    }
    if (rule.catchupCurrencyType > kMaxValidCurrencyType) {
        return "catchup_currency_type invalid";
    }
    if (rule.catchupPricePerPoint > kHardMaxCatchupPricePerPoint) {
        return "catchup_price_per_point exceeds limit";
    }
    if (rule.defaultSealId == 0) {
        return "default_seal_id must be non-zero";
    }
    return nullptr;
}

inline bool ValidSealEffect(uint32_t seasonId, uint32_t sealId, uint32_t effectId,
                            uint32_t bonusBps, uint32_t capBps, uint32_t perRoundLimit,
                            uint32_t battleLimit, uint32_t activeRounds, uint32_t healthThresholdBps) {
    if (seasonId == 0 || sealId == 0) {
        return false;
    }
    return equipbattle::ValidArtifactDefinition(effectId, bonusBps, capBps, perRoundLimit,
                                                battleLimit, activeRounds, healthThresholdBps);
}

// 进度点线性映射到轻属性万分比：0 点 = 0，满点 = capBps，向下取整且绝不超 capBps。
inline uint32_t ScaledBonusBps(uint32_t progressPoints, uint32_t maxPoints, uint32_t capBps) {
    if (maxPoints == 0 || capBps == 0 || progressPoints == 0) {
        return 0;
    }
    const uint32_t cappedCap = std::min(capBps, kHardStatCapBps);
    const uint32_t clampedPoints = std::min(progressPoints, maxPoints);
    return static_cast<uint32_t>((static_cast<uint64_t>(cappedCap) * clampedPoints) / maxPoints);
}

// 技能是否允许在当前赛季生效：season_id == 0 为常驻永久技能；否则必须严格匹配当前活跃赛季。
inline bool IsSkillActiveInSeason(uint32_t skillSeasonId, uint32_t activeSeasonId) {
    return skillSeasonId == 0 || (activeSeasonId != 0 && skillSeasonId == activeSeasonId);
}

// 赛季限定技能免费自动生效重数：随角色生效等级直达当前上限，不消耗银两修习。
inline uint32_t EffectiveSeasonalSkillRank(uint32_t actorLevel, uint32_t unlockLevel, uint32_t maxRank) {
    return classskill::RankLimit(actorLevel, unlockLevel, maxRank);
}

// 计算追赶点数与总价；requestedPoints == 0 表示一键补满剩余点数。
inline bool ComputeCatchupPlan(uint32_t currentPoints, uint32_t maxPoints,
                               uint32_t requestedPoints, uint64_t pricePerPoint,
                               CatchupPlan& out) {
    out = CatchupPlan{};
    if (maxPoints == 0 || currentPoints >= maxPoints || pricePerPoint == 0) {
        return false;
    }
    const uint32_t remaining = maxPoints - currentPoints;
    const uint32_t buy = requestedPoints == 0 ? remaining : requestedPoints;
    if (buy == 0 || buy > remaining) {
        return false;
    }
    if (pricePerPoint > std::numeric_limits<uint64_t>::max() / buy) {
        return false;
    }
    out.pointsToBuy = buy;
    out.totalPrice = pricePerPoint * buy;
    return true;
}

} // namespace seasonrules

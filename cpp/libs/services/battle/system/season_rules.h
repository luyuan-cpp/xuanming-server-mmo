#pragma once

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <string>
#include <vector>
#include "class_skill_rules.h"
#include "equipment_battle_rules.h"

// 赛季技能与单槽赛季印（方案 A / V4 十季无限循环与表驱动共鸣）的纯规则：不查表、不碰 ECS、不取墙钟、不打日志。
// Scene 与 Battle 共用同一份校验口径，避免双进程分叉（docs/planning/season-ten-seasons-infinite-cycle-v4.md）。
namespace seasonrules {

// 硬安全上限：即便配置表误填高数值，赛季印纯面板轻属性也绝不允许超过 5%（500 万分比）。
inline constexpr uint32_t kHardStatCapBps = 500;
inline constexpr uint32_t kHardMaxProgressPoints = 1000;
inline constexpr uint64_t kHardMaxCatchupPricePerPoint = 1000000000ULL;
inline constexpr uint32_t kMaxValidCurrencyType = 2; // 0 银两 / 1 灵石 / 2 绑定灵石

// 无限期号自动循环排期硬边界（天）。
inline constexpr uint32_t kMinCycleDurationDays = 7;
inline constexpr uint32_t kMaxCycleDurationDays = 180;
inline constexpr uint64_t kMsPerDay = 24ULL * 3600ULL * 1000ULL;

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
    uint32_t cycleOrder{0};
    uint32_t cycleDurationDays{0};
};

struct ResolvedSeason {
    bool active{false};
    bool invalidConfig{false};
    uint32_t seasonId{0};
    uint32_t seasonTerm{0};
    uint32_t cycleIndex{0};
    uint32_t cyclePoolSize{0}; // 0 = 固定单季模式；> 0 = 自动无限循环池大小
    uint64_t termStartMs{0};
    uint64_t termEndMs{0};     // 0 = 首测无截止的固定单季
    std::size_t ruleIndex{0};
};

struct CatchupPlan {
    uint32_t pointsToBuy{0};
    uint64_t totalPrice{0};
};

// 赛季印单局结算增量（用于近期战果持久落盘与 BattleEndS2C 实时跳条展示，含满级后往季徽章追忆溢出）。
struct BattleCreditDelta {
    uint32_t seasonId{0};
    uint32_t pointsAdded{0};
    uint32_t progressAfter{0};
    uint32_t maxPoints{0};
    bool honorNewlyUnlocked{false};
    uint32_t retroHonorSeasonId{0};
    uint32_t retroHonorPointsAdded{0};
    uint32_t retroHonorProgressAfter{0};
    uint32_t retroHonorMaxPoints{0};
    bool retroHonorNewlyUnlocked{false};
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

// 计算当前权威赛季周次（1 起）：有明确开季/开届时间时按 7 天递增；首测常开（startsAtMs == 0）对齐 UTC+8 周一 05:00。
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

// 由赛季/届号与周次确定性轮换全员对称周词缀（1..4）；任一为 0 返回 0。
inline uint32_t WeeklyAffixForWeek(uint32_t seasonOrTermId, uint32_t weekIndex) {
    if (seasonOrTermId == 0 || weekIndex == 0) {
        return kWeeklyAffixNone;
    }
    return ((seasonOrTermId - 1u + weekIndex - 1u) % kWeeklyAffixCount) + 1u;
}

inline uint32_t WeeklyAffixAt(uint32_t seasonOrTermId, uint64_t startsAtMs, uint64_t nowMs) {
    if (seasonOrTermId == 0) {
        return kWeeklyAffixNone;
    }
    return WeeklyAffixForWeek(seasonOrTermId, ComputeSeasonWeekIndex(startsAtMs, nowMs));
}

// 推荐搭配该周词缀的赛季印效果类型（用于面板高亮，不限制自由切换）。
inline bool IsSealEffectRecommendedForAffix(uint32_t affixId, uint32_t effectId) {
    switch (affixId) {
    case kWeeklyAffixShieldOverload:
        return effectId == 1u || effectId == 2u || effectId == 5u; // 物理/法术破盾印或首盾增容
    case kWeeklyAffixSpiritRecovery:
        return effectId == 7u || effectId == 6u || effectId == 4u; // 防御回蓝 / 守印 / 急救增疗
    case kWeeklyAffixOpeningGuard:
        return effectId == 6u || effectId == 7u || effectId == 5u; // 前两回合减伤 / 蓄息 / 首盾
    case kWeeklyAffixSharpTempo:
        return effectId == 1u || effectId == 2u || effectId == 4u; // 破盾后迅速打穿或急救反打
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

// 赛季玩法 V4：8 种表驱动「技印共鸣」原语（零面板属性、零速度控制，硬上限校验）。
inline constexpr uint32_t kResonanceNone = 0;
inline constexpr uint32_t kResonanceSharpManaRefund = 1;     // 1/2 碎盾返蓝 param% (<= 40)
inline constexpr uint32_t kResonanceBreakExtendDebuff = 2;   // 1/2 破盾延同招 Modifier 减益 +param 回合 (<= 1)
inline constexpr uint32_t kResonanceGuardShieldBoost = 3;    // 6 减伤后次轮施放绑定技护盾 +param% (<= 15)
inline constexpr uint32_t kResonanceDefendManaDiscount = 4;  // 7 防御回蓝后次轮施放绑定技耗蓝 -param% (<= 50)
inline constexpr uint32_t kResonanceEmergencyHealRefund = 5; // 4 急救增疗后返还实付耗蓝 param% (<= 35)
inline constexpr uint32_t kResonanceFirstShieldMana = 6;     // 5 首盾增容后回复入战最大法力 param 万分比 (<= 300)
inline constexpr uint32_t kResonanceGuardHealBoost = 7;      // 6 减伤后次轮施放绑定技治疗 +param% (<= 15)
inline constexpr uint32_t kResonanceDefendShieldBoost = 8;   // 7 防御回蓝后次轮施放绑定技护盾 +param% (<= 12)
inline constexpr uint32_t kResonanceEventBase = 100;

// BATTLE_EVENT_ARTIFACT（artifact_item_id=0, season_seal_id>0）共鸣子事件号 101..108，与基础印效果 1/2/4/5/6/7 正交。
inline constexpr uint32_t kResonanceEventSharpManaRefund = 101;
inline constexpr uint32_t kResonanceEventMysticExtendDebuff = 102;
inline constexpr uint32_t kResonanceEventGuardShieldBoost = 103;
inline constexpr uint32_t kResonanceEventBreathHalfMana = 104;
inline constexpr uint32_t kResonanceEventEmergencyHealRefund = 105;
inline constexpr uint32_t kResonanceEventFirstShieldMana = 106;
inline constexpr uint32_t kResonanceEventGuardHealBoost = 107;
inline constexpr uint32_t kResonanceEventDefendShieldBoost = 108;

// V3 兼容别名（保留供旧测试引用）。
inline constexpr uint32_t kSeason1Id = 1;
inline constexpr uint32_t kSealPozhenRuiyin = 101;
inline constexpr uint32_t kSealPozhenXuanyin = 102;
inline constexpr uint32_t kSealGuzhenShouyin = 103;
inline constexpr uint32_t kSealJuxiYuanyin = 104;
inline constexpr uint32_t kSkillPozhenLiefeng = 15001;
inline constexpr uint32_t kSkillPozhenLinglei = 15002;
inline constexpr uint32_t kSkillGuzhenLingzhang = 15003;
inline constexpr uint32_t kSkillPozhenXushi = 15004;
inline constexpr uint32_t kResonanceSharpShatterManaRefundPct = 30;
inline constexpr uint32_t kResonanceMysticDebuffBonusRounds = 1;
inline constexpr uint32_t kResonanceGuardShieldBonusPct = 10;

// 向上取整百分比返蓝（溢出安全），与 Java / Python 逐位一致。
inline uint64_t ComputeManaPctCeil(uint64_t paidMana, uint32_t pct) {
    if (paidMana == 0 || pct == 0) {
        return 0;
    }
    const uint64_t boundedPct = std::min(pct, 100u);
    const uint64_t q = paidMana / 100ULL;
    const uint64_t r = paidMana % 100ULL;
    return q * boundedPct + (r * boundedPct + 99ULL) / 100ULL;
}

// 百分比省蓝后实付耗蓝（向下取整，discountPct=50 时与 baseCost/2 逐位一致）。
inline uint64_t ComputeManaDiscounted(uint64_t baseCost, uint32_t discountPct) {
    if (baseCost == 0) {
        return 0;
    }
    const uint64_t keepPct = discountPct >= 100u ? 0ULL : static_cast<uint64_t>(100u - discountPct);
    const uint64_t q = baseCost / 100ULL;
    const uint64_t r = baseCost % 100ULL;
    return q * keepPct + (r * keepPct) / 100ULL;
}

inline uint64_t ResonanceSharpManaRefund(uint64_t paidMana) {
    return ComputeManaPctCeil(paidMana, kResonanceSharpShatterManaRefundPct);
}

inline uint64_t ResonanceHalfManaCost(uint64_t baseCost) {
    return ComputeManaDiscounted(baseCost, 50u);
}

// 校验表驱动共鸣原语与硬上限；resonanceKind == 0 表示关闭共鸣（支持零代码热更一键降级）。
inline bool ValidSealResonance(uint32_t effectId, uint32_t resonanceSkillId,
                               uint32_t resonanceKind, uint32_t resonanceParam) {
    if (resonanceKind == kResonanceNone) {
        return true;
    }
    if (resonanceSkillId == 0u || resonanceParam == 0u) {
        return false;
    }
    switch (resonanceKind) {
    case kResonanceSharpManaRefund:
        return (effectId == 1u || effectId == 2u) && resonanceParam <= 40u;
    case kResonanceBreakExtendDebuff:
        return (effectId == 1u || effectId == 2u) && resonanceParam <= 1u;
    case kResonanceGuardShieldBoost:
        return effectId == 6u && resonanceParam <= 15u;
    case kResonanceDefendManaDiscount:
        return effectId == 7u && resonanceParam <= 50u;
    case kResonanceEmergencyHealRefund:
        return effectId == 4u && resonanceParam <= 35u;
    case kResonanceFirstShieldMana:
        return effectId == 5u && resonanceParam <= 300u;
    case kResonanceGuardHealBoost:
        return effectId == 6u && resonanceParam <= 15u;
    case kResonanceDefendShieldBoost:
        return effectId == 7u && resonanceParam <= 12u;
    default:
        return false;
    }
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
    if (rule.cycleDurationDays != 0 &&
        (rule.cycleDurationDays < kMinCycleDurationDays || rule.cycleDurationDays > kMaxCycleDurationDays)) {
        return "cycle_duration_days must be 0 or in [7, 180]";
    }
    return nullptr;
}

// 赛季印严禁使用 effect_id = 3（控命）与 8（控抗），仅开放 1/2/4/5/6/7 六类零速度零控率轻机制。
inline bool ValidSealEffect(uint32_t seasonId, uint32_t sealId, uint32_t effectId,
                            uint32_t bonusBps, uint32_t capBps, uint32_t perRoundLimit,
                            uint32_t battleLimit, uint32_t activeRounds, uint32_t healthThresholdBps) {
    if (seasonId == 0 || sealId == 0 || effectId == 3u || effectId == 8u) {
        return false;
    }
    return equipbattle::ValidArtifactDefinition(effectId, bonusBps, capBps, perRoundLimit,
                                                battleLimit, activeRounds, healthThresholdBps);
}

inline bool ValidSealDefinition(uint32_t seasonId, uint32_t sealId, uint32_t effectId,
                                uint32_t bonusBps, uint32_t capBps, uint32_t perRoundLimit,
                                uint32_t battleLimit, uint32_t activeRounds, uint32_t healthThresholdBps,
                                uint32_t resonanceSkillId, uint32_t resonanceKind, uint32_t resonanceParam) {
    return ValidSealEffect(seasonId, sealId, effectId, bonusBps, capBps, perRoundLimit,
                           battleLimit, activeRounds, healthThresholdBps) &&
           ValidSealResonance(effectId, resonanceSkillId, resonanceKind, resonanceParam);
}

// 校验整张 SeasonRule 排期（固定单季窗模式 vs 无限期号自动循环模式）；非法返回英文错误描述，合法返回 nullptr。
inline const char* ValidateSchedule(const std::vector<RuleParams>& rules) {
    bool anyCycle = false;
    for (const auto& rule : rules) {
        if (const char* err = ValidateRule(rule); err != nullptr) {
            return err;
        }
        if (rule.cycleDurationDays > 0u) {
            anyCycle = true;
        }
    }
    if (!anyCycle) {
        for (std::size_t i = 0; i < rules.size(); ++i) {
            if (!rules[i].enabled) {
                continue;
            }
            for (std::size_t j = i + 1; j < rules.size(); ++j) {
                if (!rules[j].enabled) {
                    continue;
                }
                if (WindowsOverlap(rules[i].startsAtMs, rules[i].endsAtMs,
                                   rules[j].startsAtMs, rules[j].endsAtMs)) {
                    return "overlapping enabled seasons in fixed-window mode";
                }
            }
        }
        return nullptr;
    }

    // 自动无限循环模式 fail-closed 校验。
    std::vector<const RuleParams*> pool;
    uint32_t sharedDuration = 0u;
    for (const auto& rule : rules) {
        if (rule.cycleOrder == 0u) {
            if (rule.enabled || rule.cycleDurationDays != 0u) {
                return "non-pool rule cannot be enabled or set cycle_duration_days in cycle mode";
            }
            continue;
        }
        if (!rule.enabled) {
            return "cycle pool rule must be enabled";
        }
        if (rule.cycleDurationDays < kMinCycleDurationDays || rule.cycleDurationDays > kMaxCycleDurationDays) {
            return "cycle pool rule cycle_duration_days out of range [7, 180]";
        }
        if (sharedDuration == 0u) {
            sharedDuration = rule.cycleDurationDays;
        } else if (sharedDuration != rule.cycleDurationDays) {
            return "cycle pool rules must share identical cycle_duration_days";
        }
        pool.push_back(&rule);
    }
    if (pool.empty()) {
        return "cycle mode requires at least one rule with cycle_order > 0";
    }
    std::sort(pool.begin(), pool.end(), [](const RuleParams* lhs, const RuleParams* rhs) {
        return lhs->cycleOrder < rhs->cycleOrder;
    });
    for (std::size_t i = 0; i < pool.size(); ++i) {
        if (pool[i]->cycleOrder != static_cast<uint32_t>(i + 1u)) {
            return "cycle_order must form contiguous 1..N sequence without duplicates";
        }
    }
    if (pool[0]->startsAtMs == 0u) {
        return "cycle mode requires pool[0].starts_at_ms > 0 as anchor timestamp";
    }
    return nullptr;
}

// 确定性解析当前时刻活跃赛季（支持固定单季与无限期号自动循环；非法配置 fail-closed 返回 invalidConfig=true）。
inline ResolvedSeason ResolveActiveSeason(const std::vector<RuleParams>& rules, uint64_t nowMs) {
    ResolvedSeason out;
    if (rules.empty()) {
        return out;
    }
    if (ValidateSchedule(rules) != nullptr) {
        out.invalidConfig = true;
        return out;
    }
    bool anyCycle = false;
    for (const auto& r : rules) {
        if (r.cycleDurationDays > 0u) {
            anyCycle = true;
            break;
        }
    }
    if (!anyCycle) {
        for (std::size_t i = 0; i < rules.size(); ++i) {
            const auto& r = rules[i];
            if (!IsWindowActive(r.startsAtMs, r.endsAtMs, r.enabled, nowMs)) {
                continue;
            }
            out.active = true;
            out.seasonId = r.seasonId;
            out.seasonTerm = 1u;
            out.cycleIndex = 1u;
            out.cyclePoolSize = 0u;
            out.termStartMs = r.startsAtMs;
            out.termEndMs = r.endsAtMs;
            out.ruleIndex = i;
            return out;
        }
        return out;
    }

    std::vector<std::pair<uint32_t, std::size_t>> pool;
    for (std::size_t i = 0; i < rules.size(); ++i) {
        if (rules[i].cycleOrder > 0u) {
            pool.emplace_back(rules[i].cycleOrder, i);
        }
    }
    std::sort(pool.begin(), pool.end());
    const auto& anchorRule = rules[pool[0].second];
    const uint64_t anchorMs = anchorRule.startsAtMs;
    if (nowMs < anchorMs) {
        return out;
    }
    const uint64_t periodMs = static_cast<uint64_t>(anchorRule.cycleDurationDays) * kMsPerDay;
    const uint64_t elapsedIdx = (nowMs - anchorMs) / periodMs;
    const uint64_t poolSize = static_cast<uint64_t>(pool.size());
    const std::size_t pickedRuleIndex = pool[static_cast<std::size_t>(elapsedIdx % poolSize)].second;
    const uint64_t term64 = elapsedIdx + 1ULL;
    const uint64_t cycle64 = (elapsedIdx / poolSize) + 1ULL;

    out.active = true;
    out.seasonId = rules[pickedRuleIndex].seasonId;
    out.seasonTerm = term64 > std::numeric_limits<uint32_t>::max()
        ? std::numeric_limits<uint32_t>::max()
        : static_cast<uint32_t>(term64);
    out.cycleIndex = cycle64 > std::numeric_limits<uint32_t>::max()
        ? std::numeric_limits<uint32_t>::max()
        : static_cast<uint32_t>(cycle64);
    out.cyclePoolSize = static_cast<uint32_t>(pool.size());
    out.termStartMs = anchorMs + elapsedIdx * periodMs;
    out.termEndMs = out.termStartMs + periodMs;
    out.ruleIndex = pickedRuleIndex;
    return out;
}

// 永久纪念徽章动态位图读写纯函数（word = (season_id - 1) / 64, bit = (season_id - 1) % 64，支持无限赛季扩容）。
template <typename Words>
inline bool IsHonorBitSet(const Words& words, uint32_t seasonId) {
    if (seasonId == 0u) {
        return false;
    }
    const std::size_t wordIdx = static_cast<std::size_t>(seasonId - 1u) / 64u;
    const uint32_t bit = (seasonId - 1u) % 64u;
    if (wordIdx >= static_cast<std::size_t>(words.size())) {
        return false;
    }
    return (words[static_cast<int>(wordIdx)] & (1ULL << bit)) != 0ULL;
}

inline bool SetHonorBitVec(std::vector<uint64_t>& words, uint32_t seasonId) {
    if (seasonId == 0u) {
        return false;
    }
    const std::size_t wordIdx = static_cast<std::size_t>(seasonId - 1u) / 64u;
    const uint32_t bit = (seasonId - 1u) % 64u;
    if (words.size() <= wordIdx) {
        words.resize(wordIdx + 1u, 0ULL);
    }
    const uint64_t mask = 1ULL << bit;
    const bool newlySet = (words[wordIdx] & mask) == 0ULL;
    words[wordIdx] |= mask;
    return newlySet;
}

template <typename RepeatedU64>
inline bool SetHonorBitRepeated(RepeatedU64* words, uint32_t seasonId) {
    if (words == nullptr || seasonId == 0u) {
        return false;
    }
    const std::size_t wordIdx = static_cast<std::size_t>(seasonId - 1u) / 64u;
    const uint32_t bit = (seasonId - 1u) % 64u;
    while (static_cast<std::size_t>(words->size()) <= wordIdx) {
        words->Add(0ULL);
    }
    const int idx = static_cast<int>(wordIdx);
    const uint64_t mask = 1ULL << bit;
    const uint64_t prev = words->Get(idx);
    const bool newlySet = (prev & mask) == 0ULL;
    words->Set(idx, prev | mask);
    return newlySet;
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

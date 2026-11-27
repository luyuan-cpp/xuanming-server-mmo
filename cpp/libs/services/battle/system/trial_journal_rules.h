#pragma once

#include <algorithm>
#include <cstdint>
#include <vector>

#include "proto/battle/battle_data.pb.h"

// 五行试炼志 V2 纯判定与常量（Scene 与 Battle 共享，零 ECS 依赖，可直接单测）。
// 设计依据：docs/design/long-term-progression-v2.md。
namespace trialjournal {

// 仅自用横向玩具固定物品模板 ID（与 data/Item.xlsx 3403/3404 保持一致）。
inline constexpr uint32_t kBoundReforgeSandItemId = 3403;
inline constexpr uint32_t kBoundArtifactVoucherItemId = 3404;

// 八件 V1 法宝效果范围（EquipArtifact.id 1..8）及最多佩戴槽数。
inline constexpr uint32_t kMinArtifactEffectId = 1;
inline constexpr uint32_t kMaxArtifactEffectId = 8;
inline constexpr uint32_t kMaxArtifactSlots = 2;
inline constexpr uint32_t kMaxCoopTeammates = 4;

// 卷册编号（与 TrialJournalEntry.chapter_id / TrialJournalMilestone.scope_chapter_id 对齐）。
inline constexpr uint32_t kScopeAllPermanent = 0;
inline constexpr uint32_t kChapterBreakthrough = 1; // 卷一 · 破阵试炼
inline constexpr uint32_t kChapterThreeRealms = 2;  // 卷二 · 灵障玄毒
inline constexpr uint32_t kChapterSeasonal = 3;     // 卷三 · 季游奇谈
inline constexpr uint32_t kChapterWildRealms = 4;   // 卷四 · 三境明雷

// 五行试炼志 V3 自选试炼誓约位图（bit 0..3，仅常驻 PVE 首领试炼可勾选，零额外材料产出）。
inline constexpr uint32_t kVowNone = 0u;
inline constexpr uint32_t kVowBanControl = 1u << 0;     // 禁控硬破
inline constexpr uint32_t kVowSingleArtifact = 1u << 1; // 单宝轻装
inline constexpr uint32_t kVowNoPet = 1u << 2;          // 无宠孤剑
inline constexpr uint32_t kVowTightRounds = 1u << 3;    // 紧缩回合
inline constexpr uint32_t kVowMaskAll =
    kVowBanControl | kVowSingleArtifact | kVowNoPet | kVowTightRounds;

inline constexpr bool IsValidVowMask(uint32_t mask) {
    return (mask & ~kVowMaskAll) == 0u;
}

inline constexpr uint32_t CountVows(uint32_t mask) {
    const uint32_t valid = mask & kVowMaskAll;
    uint32_t count = 0u;
    for (uint32_t bit = 0u; bit < 4u; ++bit) {
        if ((valid & (1u << bit)) != 0u) {
            ++count;
        }
    }
    return count;
}

inline constexpr uint32_t TightenRoundLimit(uint32_t baseRounds) {
    if (baseRounds == 0u) {
        return 0u;
    }
    const uint32_t scaled = (baseRounds * 7u + 9u) / 10u; // ceil(baseRounds * 0.7)
    if (baseRounds <= 6u) {
        return baseRounds;
    }
    return scaled < 6u ? 6u : scaled;
}

// 准入模式（TrialJournalEntry.unlock_mode）。
inline constexpr uint32_t kUnlockModeOpen = 0;
inline constexpr uint32_t kUnlockModeRequireAnyNormalBreakthroughClear = 1;

// 战法印定位分类（TrialJournalSeal.seal_slot_kind）。
enum class SealSlotKind : uint32_t {
    kUnspecified = 0,
    kClear = 1,  // 通 / 破阵
    kGuard = 2,  // 守 / 无损
    kTactic = 3, // 巧 / 机制解题
    kSpeed = 4,  // 速 / 速破
};

// 战法印权威判定条件（TrialJournalSeal.condition_type）。
enum class SealConditionType : uint32_t {
    kUnspecified = 0,
    kClear = 1,                    // 有效通关
    kSpeedClear = 2,               // 有效通关且 total_rounds <= condition_param
    kFlawlessSelfAndPet = 3,       // 有效通关且本人及本人出战宝宝全程未曾倒地
    kNoBossGuardHeal = 4,          // 有效通关且回春护卫对首领有效治疗总量为 0
    kBreakGuardStance = 5,         // 有效通关且驱散过护势或己方物法双系均造成有效直伤
    kWindDefend = 6,               // 卷二八仙岛：零风击倒地且满足回合上限（若配置）
    kBellHarmony = 7,              // 卷二碧渊水宫：阳钟物理 + 阴鼓法术均击伤并通关
    kTideFinish = 8,               // 卷二月汐湾：退潮窗口终结并通关
    kGuardRescued = 9,             // 卷三护卫救援：击败首领并保住回春护卫
    kPolarityWin = 10,             // 卷三物法易势：有效胜出
    kArtifactShieldBreak = 11,     // 卷三破障锋芒：合格完赛且触发破盾产生 >0 削盾
    kAntiControlOrSeasonSkill = 12,// 卷三明心应劫：合格完赛且触发清心符/净化/当季免费赛季技能
    kDualGuardBreak = 13,          // 卷一困难·双卫同阵连环双破：驱散护势(或双系击伤)且零有效回春(或断疗反噬)并胜出
    kPerfectCleaveParry = 14,      // 首领试炼 V2 化劲宗师：本人及宝宝被重劈点名>=1次且100%合法化解并胜出
    kSeasonSkillCast = 15,         // 卷三 V4 赛季技法：合格完赛且本人直接施放当季免费赛季技能
    kSeasonResonance = 16          // 卷三 V4 技印共鸣：合格完赛且本人触发当季 R1..R8 技印共鸣（param=0 任意，非 0 按位掩码过滤）
};

inline bool IsPlayerVictory(eBattleOutcome outcome, uint32_t playerTeamIndex) {
    return (outcome == BATTLE_OUTCOME_SIDE_A_WIN && playerTeamIndex == 0) ||
           (outcome == BATTLE_OUTCOME_SIDE_B_WIN && playerTeamIndex == 1);
}

inline bool IsCompletedNonVoidOutcome(eBattleOutcome outcome) {
    return outcome == BATTLE_OUTCOME_SIDE_A_WIN ||
           outcome == BATTLE_OUTCOME_SIDE_B_WIN ||
           outcome == BATTLE_OUTCOME_DRAW;
}

inline bool IsValidTrialSettlement(const BattleSettlementData& settlement) {
    if (settlement.battle_id() == 0 || settlement.player_id() == 0 || settlement.fled() ||
        settlement.total_rounds() == 0 || settlement.player_team_index() > 1 ||
        !IsCompletedNonVoidOutcome(settlement.outcome()) || !settlement.has_trial_facts()) {
        return false;
    }
    const auto& facts = settlement.trial_facts();
    return facts.journal_entry_id() != 0 && facts.trial_rule_version() != 0 &&
           facts.initial_human_count() >= 1 && facts.initial_human_count() <= 5;
}

inline bool IsSoloTrial(const BattleTrialJournalFacts& facts) {
    return facts.initial_human_count() == 1;
}

inline bool IsCoopTrial(const BattleTrialJournalFacts& facts) {
    return facts.initial_human_count() >= 2 && facts.initial_human_count() <= 5;
}

inline std::vector<uint32_t> NormalizeArtifactEffectIds(const google::protobuf::RepeatedField<uint32_t>& raw) {
    std::vector<uint32_t> out;
    for (const uint32_t effectId : raw) {
        if (effectId >= kMinArtifactEffectId && effectId <= kMaxArtifactEffectId) {
            out.push_back(effectId);
        }
    }
    std::sort(out.begin(), out.end());
    if (out.size() > kMaxArtifactSlots) {
        out.resize(kMaxArtifactSlots);
    }
    return out;
}

// 校验一条战法印是否在本次已验证的终局结算中达成。
inline bool EvaluateSealCondition(uint32_t conditionType,
                                  uint32_t conditionParam,
                                  const BattleSettlementData& settlement) {
    if (!IsValidTrialSettlement(settlement)) {
        return false;
    }
    const auto& facts = settlement.trial_facts();
    const bool won = IsPlayerVictory(settlement.outcome(), settlement.player_team_index());
    switch (static_cast<SealConditionType>(conditionType)) {
    case SealConditionType::kClear:
        return won;
    case SealConditionType::kSpeedClear:
        return won && conditionParam > 0 && settlement.total_rounds() <= conditionParam;
    case SealConditionType::kFlawlessSelfAndPet:
        return won && !facts.self_or_pet_ever_died();
    case SealConditionType::kNoBossGuardHeal:
        return won && facts.boss_guard_heal_total() == 0;
    case SealConditionType::kBreakGuardStance:
        return won && (facts.guard_stance_dispelled() ||
                       (facts.team_dealt_physical_direct_damage() && facts.team_dealt_magic_direct_damage()));
    case SealConditionType::kWindDefend:
        return won && !facts.boss_wind_hit_unprotected_death() &&
               (conditionParam == 0 || settlement.total_rounds() <= conditionParam);
    case SealConditionType::kBellHarmony:
        return won && facts.dealt_yang_bell_physical_damage() && facts.dealt_yin_drum_magic_damage();
    case SealConditionType::kTideFinish:
        return won && facts.finished_in_ebb_tide();
    case SealConditionType::kGuardRescued:
        return won && facts.guard_survived();
    case SealConditionType::kPolarityWin:
        return won;
    case SealConditionType::kArtifactShieldBreak:
        return (won || settlement.participated()) && facts.self_triggered_shield_break();
    case SealConditionType::kAntiControlOrSeasonSkill:
        return (won || settlement.participated()) && facts.self_triggered_anti_control_or_season_skill();
    case SealConditionType::kDualGuardBreak:
        return won &&
               (facts.guard_stance_dispelled() ||
                (facts.team_dealt_physical_direct_damage() && facts.team_dealt_magic_direct_damage())) &&
               (facts.boss_guard_heal_total() == 0 || facts.heal_interrupt_backlash_count() > 0);
    case SealConditionType::kPerfectCleaveParry:
        return won && facts.all_self_cleaves_parried();
    case SealConditionType::kSeasonSkillCast:
        return (won || settlement.participated()) && facts.self_cast_season_skill();
    case SealConditionType::kSeasonResonance:
        return (won || settlement.participated()) &&
               (conditionParam == 0u
                    ? facts.self_triggered_season_resonance()
                    : ((facts.self_season_resonance_mask() & conditionParam) != 0u));
    default:
        return false;
    }
}

inline bool IsValidConditionType(uint32_t conditionType) {
    return conditionType >= static_cast<uint32_t>(SealConditionType::kClear) &&
           conditionType <= static_cast<uint32_t>(SealConditionType::kSeasonResonance);
}

inline bool IsValidSealSlotKind(uint32_t slotKind) {
    return slotKind >= static_cast<uint32_t>(SealSlotKind::kClear) &&
           slotKind <= static_cast<uint32_t>(SealSlotKind::kSpeed);
}

} // namespace trialjournal

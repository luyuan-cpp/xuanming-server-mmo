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
inline constexpr uint32_t kChapterThreeRealms = 2;  // 卷二 · 三境明雷
inline constexpr uint32_t kChapterSeasonal = 3;     // 卷三 · 季游奇谈

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
    kAntiControlOrSeasonSkill = 12 // 卷三明心应劫：合格完赛且触发清心符/净化/当季免费赛季技能
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
    default:
        return false;
    }
}

inline bool IsValidConditionType(uint32_t conditionType) {
    return conditionType >= static_cast<uint32_t>(SealConditionType::kClear) &&
           conditionType <= static_cast<uint32_t>(SealConditionType::kAntiControlOrSeasonSkill);
}

inline bool IsValidSealSlotKind(uint32_t slotKind) {
    return slotKind >= static_cast<uint32_t>(SealSlotKind::kClear) &&
           slotKind <= static_cast<uint32_t>(SealSlotKind::kSpeed);
}

} // namespace trialjournal

#pragma once

#include <algorithm>
#include <cstdint>

#include "proto/battle/battle_data.pb.h"

namespace turnbattle {

// 万象残局坊（UGC 回合解谜工坊 V1）引擎侧共享规则与白名单常量。
// 与 Go `go/match/internal/puzzle/spec.go`、Java `TacticalPuzzleRules`、Unity `TacticalPuzzleModels` 四端逐项同源。
inline constexpr uint32_t kTacticalPuzzleWorkshopDungeonId = 314u;

inline constexpr uint32_t kTacticalPuzzleMinRoundLimit = 1u;
inline constexpr uint32_t kTacticalPuzzleMaxRoundLimit = 6u;

inline constexpr uint32_t kTacticalPuzzleMinCharPresetId = 101u;
inline constexpr uint32_t kTacticalPuzzleMaxCharPresetId = 103u;
inline constexpr uint32_t kTacticalPuzzleMinPetPresetId = 201u;
inline constexpr uint32_t kTacticalPuzzleMaxPetPresetId = 203u;

inline constexpr uint32_t kTacticalPuzzleMinMonsterId = 311u;
inline constexpr uint32_t kTacticalPuzzleMaxMonsterId = 316u;
inline constexpr uint32_t kTacticalPuzzleMinEnemies = 1u;
inline constexpr uint32_t kTacticalPuzzleMaxEnemies = 3u;

inline constexpr uint32_t kTacticalPuzzleMaxFormationId = 5u;

inline constexpr uint32_t kTacticalPuzzlePlayerMinHpPct = 15u;
inline constexpr uint32_t kTacticalPuzzlePlayerMaxHpPct = 100u;
inline constexpr uint32_t kTacticalPuzzlePlayerMinMpPct = 20u;
inline constexpr uint32_t kTacticalPuzzlePlayerMaxMpPct = 100u;
inline constexpr uint32_t kTacticalPuzzlePlayerMaxShieldPct = 50u;
inline constexpr uint32_t kTacticalPuzzleMaxPlayerDebuffKind = 3u;

inline constexpr uint32_t kTacticalPuzzleEnemyMinHpPct = 10u;
inline constexpr uint32_t kTacticalPuzzleEnemyMaxHpPct = 150u;
inline constexpr uint32_t kTacticalPuzzleEnemyMaxShieldPct = 60u;
inline constexpr int32_t kTacticalPuzzleMinSpeedDeltaPct = -30;
inline constexpr int32_t kTacticalPuzzleMaxSpeedDeltaPct = 30;
inline constexpr uint32_t kTacticalPuzzleMaxEnemyBuffKind = 4u;
inline constexpr uint32_t kTacticalPuzzleMaxLeylineElement = 5u;

constexpr bool IsTacticalPuzzlePresetSkill(uint32_t charPresetId, uint32_t petPresetId, uint32_t skillId) {
    if (skillId == 0u) {
        return true;
    }
    switch (charPresetId) {
        case 101u:
            if (skillId == 10002u || skillId == 10039u || skillId == 10025u || skillId == 10035u) return true;
            break;
        case 102u:
            if (skillId == 10006u || skillId == 10020u || skillId == 10034u || skillId == 10041u) return true;
            break;
        case 103u:
            if (skillId == 10010u || skillId == 10033u || skillId == 10040u || skillId == 10026u) return true;
            break;
        default:
            break;
    }
    switch (petPresetId) {
        case 201u:
            return skillId == 10002u || skillId == 10018u || skillId == 10016u || skillId == 13005u;
        case 202u:
            return skillId == 10006u || skillId == 10019u || skillId == 10034u || skillId == 13006u;
        case 203u:
            return skillId == 10014u || skillId == 10010u || skillId == 11006u || skillId == 11005u;
        default:
            return false;
    }
}

// 既有 Buff.json 权威条目（零新增 Buff 行，天然支持护盾吸收/破盾/驱散/结算）
inline constexpr uint32_t kTacticalPuzzleShieldBuffId = 100400u;        // 通用护盾（battle_op=9）
inline constexpr uint32_t kTacticalPuzzleDebuffAtkDownBuffId = 100031u; // 卸力：物攻 -20%
inline constexpr uint32_t kTacticalPuzzleDebuffDefDownBuffId = 130051u; // 破甲：防御 -20%
inline constexpr uint32_t kTacticalPuzzleDebuffPoisonBuffId = 100190u;  // 玄毒：每回合持续毒伤
inline constexpr uint32_t kTacticalPuzzleBuffDefUpBuffId = 100140u;     // 固守：防御 +25%
inline constexpr uint32_t kTacticalPuzzleBuffSpdUpBuffId = 100160u;     // 疾风：速度 +25%
inline constexpr uint32_t kTacticalPuzzleBuffDmgUpBuffId = 100250u;     // 狂怒：伤害 +20%
inline constexpr uint32_t kTacticalPuzzleBuffReflectBuffId = 110070u;   // 反震：物理反伤 25%
inline constexpr uint32_t kTacticalPuzzlePoisonMaxHpPct = 8u;           // 玄毒开局固定每跳 8% 最大气血

constexpr uint32_t TacticalPuzzlePlayerDebuffTableId(uint32_t kind) {
    switch (kind) {
        case 1u: return kTacticalPuzzleDebuffAtkDownBuffId;
        case 2u: return kTacticalPuzzleDebuffDefDownBuffId;
        case 3u: return kTacticalPuzzleDebuffPoisonBuffId;
        default: return 0u;
    }
}

constexpr uint32_t TacticalPuzzleEnemyBuffTableId(uint32_t kind) {
    switch (kind) {
        case 1u: return kTacticalPuzzleBuffDefUpBuffId;
        case 2u: return kTacticalPuzzleBuffSpdUpBuffId;
        case 3u: return kTacticalPuzzleBuffDmgUpBuffId;
        case 4u: return kTacticalPuzzleBuffReflectBuffId;
        default: return 0u;
    }
}

inline uint64_t TacticalPuzzleScalePct(uint64_t baseValue, uint32_t pct) {
    return std::max<uint64_t>(1ULL, (baseValue * static_cast<uint64_t>(pct)) / 100ULL);
}

inline uint64_t TacticalPuzzleApplySpeedDelta(uint64_t baseSpeed, int32_t deltaPct) {
    const int32_t clamped = std::clamp(deltaPct, kTacticalPuzzleMinSpeedDeltaPct, kTacticalPuzzleMaxSpeedDeltaPct);
    const uint64_t factor = static_cast<uint64_t>(100 + clamped);
    return std::max<uint64_t>(1ULL, (baseSpeed * factor) / 100ULL);
}

inline bool IsValidTacticalPuzzleSpec(const ::TacticalPuzzleSpec& spec) {
    if (spec.round_limit() < kTacticalPuzzleMinRoundLimit ||
        spec.round_limit() > kTacticalPuzzleMaxRoundLimit) {
        return false;
    }
    if (spec.character_preset_id() < kTacticalPuzzleMinCharPresetId ||
        spec.character_preset_id() > kTacticalPuzzleMaxCharPresetId) {
        return false;
    }
    const uint32_t petPreset = spec.pet_preset_id();
    if (petPreset != 0u &&
        (petPreset < kTacticalPuzzleMinPetPresetId || petPreset > kTacticalPuzzleMaxPetPresetId)) {
        return false;
    }
    if (spec.formation_id() > kTacticalPuzzleMaxFormationId) {
        return false;
    }
    if (spec.player_initial_hp_pct() < kTacticalPuzzlePlayerMinHpPct ||
        spec.player_initial_hp_pct() > kTacticalPuzzlePlayerMaxHpPct) {
        return false;
    }
    if (spec.player_initial_mp_pct() < kTacticalPuzzlePlayerMinMpPct ||
        spec.player_initial_mp_pct() > kTacticalPuzzlePlayerMaxMpPct) {
        return false;
    }
    if (spec.player_initial_shield_pct() > kTacticalPuzzlePlayerMaxShieldPct) {
        return false;
    }
    if (spec.player_initial_debuff_kind() > kTacticalPuzzleMaxPlayerDebuffKind) {
        return false;
    }
    const int leylineVal = static_cast<int>(spec.leyline_element());
    if (leylineVal < 0 || leylineVal > static_cast<int>(kTacticalPuzzleMaxLeylineElement)) {
        return false;
    }
    if (!IsTacticalPuzzlePresetSkill(spec.character_preset_id(), spec.pet_preset_id(), spec.banned_skill_id())) {
        return false;
    }
    if (spec.enemies_size() < static_cast<int>(kTacticalPuzzleMinEnemies) ||
        spec.enemies_size() > static_cast<int>(kTacticalPuzzleMaxEnemies)) {
        return false;
    }
    for (const auto& enemy : spec.enemies()) {
        if (enemy.monster_table_id() < kTacticalPuzzleMinMonsterId ||
            enemy.monster_table_id() > kTacticalPuzzleMaxMonsterId) {
            return false;
        }
        if (enemy.initial_hp_pct() < kTacticalPuzzleEnemyMinHpPct ||
            enemy.initial_hp_pct() > kTacticalPuzzleEnemyMaxHpPct) {
            return false;
        }
        if (enemy.initial_shield_pct() > kTacticalPuzzleEnemyMaxShieldPct) {
            return false;
        }
        if (enemy.speed_delta_pct() < kTacticalPuzzleMinSpeedDeltaPct ||
            enemy.speed_delta_pct() > kTacticalPuzzleMaxSpeedDeltaPct) {
            return false;
        }
        if (enemy.initial_buff_kind() > kTacticalPuzzleMaxEnemyBuffKind) {
            return false;
        }
    }
    return true;
}

inline bool IsValidTacticalPuzzleContext(const ::TacticalPuzzleBattleContext& ctx) {
    return ctx.puzzle_id() != 0u &&
           ctx.spec_revision() != 0u &&
           ctx.has_spec() &&
           IsValidTacticalPuzzleSpec(ctx.spec());
}

}  // namespace turnbattle

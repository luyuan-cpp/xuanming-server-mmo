#pragma once

#include <algorithm>
#include <cstdint>

#include "combat_damage_rules.h"
#include "proto/battle/battle_data.pb.h"
#include "services/battle/constants/battle_effect_constants.h"

// 宝宝独立已保存条目贡献。只读战斗快照，不读取主人装备；伤害加成不改写攻击面板。
namespace turnbattle::petcultivation {

inline bool ValidSnapshot(const PetCultivationEffects& effects) {
    if (effects.physical_damage_bps() > 10000u || effects.magic_damage_bps() > 10000u ||
        effects.outgoing_heal_bps() > 10000u || effects.shield_capacity_bps() > 10000u ||
        effects.poison_damage_bps() > 10000u || effects.retreat_hit_bps() > 10000u ||
        effects.retreat_resist_bps() > 10000u) {
        return false;
    }
    for (const auto& [stateKind, value] : effects.ailment_hit_bps()) {
        if (stateKind < 1u || stateKind > 7u || value > 10000u) {
            return false;
        }
    }
    for (const auto& [stateKind, value] : effects.ailment_resist_bps()) {
        if (stateKind < 1u || stateKind > 7u || value > 10000u) {
            return false;
        }
    }
    return true;
}

inline double Rate(uint32_t bps) {
    return std::min(bps, 10000u) / 10000.0;
}

inline bool IsPet(const BattleActorState& actor) {
    return actor.actor_type() == BATTLE_ACTOR_TYPE_PET;
}

inline double DamageBonus(const BattleActorState& actor, uint32_t damageType,
                          BattleDamageOrigin origin, eBattleHitKind hitKind) {
    if (!IsPet(actor) || origin != BattleDamageOrigin::Direct || hitKind == BATTLE_HIT_COUNTER) {
        return 0.0;
    }
    // 使用正式伤害类型常量；SkillTable 的法术类型缺省值为 0。
    if (damageType == combatdamage::kPhysicalDamage) {
        return Rate(actor.pet_cultivation().physical_damage_bps());
    }
    if (damageType == combatdamage::kMagicDamage) {
        return Rate(actor.pet_cultivation().magic_damage_bps());
    }
    return 0.0;
}

inline double HealBonus(const BattleActorState& actor) {
    return IsPet(actor) ? Rate(actor.pet_cultivation().outgoing_heal_bps()) : 0.0;
}

inline double ShieldBonus(const BattleActorState& actor) {
    return IsPet(actor) ? Rate(actor.pet_cultivation().shield_capacity_bps()) : 0.0;
}

inline double PoisonBonus(const BattleActorState& actor) {
    return IsPet(actor) ? Rate(actor.pet_cultivation().poison_damage_bps()) : 0.0;
}

inline double HitBps(const BattleActorState& actor, uint32_t stateKind) {
    if (!IsPet(actor)) {
        return 0.0;
    }
    const auto it = actor.pet_cultivation().ailment_hit_bps().find(stateKind);
    return it == actor.pet_cultivation().ailment_hit_bps().end() ? 0.0 : std::min(it->second, 10000u);
}

inline double ResistBps(const BattleActorState& actor, uint32_t stateKind) {
    if (!IsPet(actor)) {
        return 0.0;
    }
    const auto it = actor.pet_cultivation().ailment_resist_bps().find(stateKind);
    return it == actor.pet_cultivation().ailment_resist_bps().end() ? 0.0 : std::min(it->second, 10000u);
}

inline double RetreatHit(const BattleActorState& actor) {
    return IsPet(actor) ? Rate(actor.pet_cultivation().retreat_hit_bps()) : 0.0;
}

inline double RetreatResist(const BattleActorState& actor) {
    return IsPet(actor) ? Rate(actor.pet_cultivation().retreat_resist_bps()) : 0.0;
}

}  // namespace turnbattle::petcultivation

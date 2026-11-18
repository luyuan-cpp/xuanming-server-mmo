#pragma once
#include <algorithm>
#include <cmath>
#include "proto/battle/battle_data.pb.h"
#include "combat_damage_rules.h"
#include "services/battle/constants/battle_effect_constants.h"

// 宝宝独立已保存条目贡献。只读快照，不读取主人装备；伤害不改攻击面板。
namespace turnbattle::petcultivation {
inline double Rate(uint32_t bps) { return std::min(bps, 10000u) / 10000.0; }
inline bool IsPet(const BattleActorState& a) { return a.actor_type() == BATTLE_ACTOR_TYPE_PET; }
inline double DamageBonus(const BattleActorState& a,uint32_t damageType,BattleDamageOrigin origin,eBattleHitKind hit) {
    if(!IsPet(a) || origin!=BattleDamageOrigin::Direct || hit==BATTLE_HIT_COUNTER) return 0;
    // 使用正式伤害类型常量；SkillTable 的法术是缺省值0。
    return damageType==combatdamage::kPhysicalDamage?Rate(a.pet_cultivation().physical_damage_bps()):damageType==combatdamage::kMagicDamage?Rate(a.pet_cultivation().magic_damage_bps()):0;
}
inline double HealBonus(const BattleActorState& a) {return IsPet(a)?Rate(a.pet_cultivation().outgoing_heal_bps()):0;}
inline double ShieldBonus(const BattleActorState& a) {return IsPet(a)?Rate(a.pet_cultivation().shield_capacity_bps()):0;}
inline double PoisonBonus(const BattleActorState& a) {return IsPet(a)?Rate(a.pet_cultivation().poison_damage_bps()):0;}
inline double HitBps(const BattleActorState& a,uint32_t state) {
    if(!IsPet(a))return 0;const auto it=a.pet_cultivation().ailment_hit_bps().find(state);
    return it==a.pet_cultivation().ailment_hit_bps().end()?0:std::min(it->second,10000u);
}
inline double ResistBps(const BattleActorState& a,uint32_t state) {
    if(!IsPet(a))return 0;const auto it=a.pet_cultivation().ailment_resist_bps().find(state);
    return it==a.pet_cultivation().ailment_resist_bps().end()?0:std::min(it->second,10000u);
}
inline double RetreatHit(const BattleActorState& a){return IsPet(a)?Rate(a.pet_cultivation().retreat_hit_bps()):0;}
inline double RetreatResist(const BattleActorState& a){return IsPet(a)?Rate(a.pet_cultivation().retreat_resist_bps()):0;}
}

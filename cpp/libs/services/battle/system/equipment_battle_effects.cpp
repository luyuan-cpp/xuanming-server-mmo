#include "system/turn_battle_engine.h"
#include "system/equipment_battle_rules.h"
#include "system/combat_damage_rules.h"

#include <algorithm>
#include <cmath>
#include <set>

namespace turnbattle {
namespace {
uint32_t Value(const google::protobuf::Map<uint32_t, uint32_t>& values, uint32_t key) {
    const auto found = values.find(key);
    return found == values.end() ? 0u : found->second;
}
bool Bounded(const google::protobuf::Map<uint32_t, uint32_t>& values, uint32_t keys, uint32_t maximum) {
    for (const auto& [key, value] : values) if (key == 0 || key > keys || value > maximum) return false;
    return true;
}
bool IsAilment(uint32_t state) { return state >= 1 && state <= 7; }
}

bool TurnBattleEngine::InitializeEquipment(BattleActorState& actor, const EquipmentEffects& effects) {
    if (effects.physical_attack_bps() > 3200 || effects.magic_attack_bps() > 3200 ||
        effects.max_health_bps() > 800 || effects.speed_bps() > 200 ||
        effects.outgoing_heal_bps() > 3200 || effects.shield_capacity_bps() > 3200 ||
        effects.physical_crit_bps() > 800 || effects.magic_crit_bps() > 800 ||
        effects.physical_resist_bps() > 600 || effects.magic_resist_bps() > 600 ||
        !Bounded(effects.ailment_hit_bps(), 7, 1600) || !Bounded(effects.ailment_resist_bps(), 7, 1600) ||
        !Bounded(effects.mana_reduction_bps(), 12, 1200) || effects.artifacts_size() > 2) return false;
    std::set<uint64_t> instances;
    for (const auto& source : effects.artifacts()) {
        if (source.item_id() == 0 || source.item_id() >> 63 || !instances.insert(source.item_id()).second ||
            source.effect_id() < 1 || source.effect_id() > 8 || source.bonus_bps() == 0 ||
            source.bonus_bps() > 10000 || source.cap_bps() > 10000 || source.health_threshold_bps() > 10000 ||
            source.per_round_limit() > 1 || source.battle_limit() > 3 || source.active_rounds() > 2) return false;
        if (source.effect_id() != 6 && source.per_round_limit() == 0) return false;
        if ((source.effect_id() == 3 || source.effect_id() >= 6) && source.battle_limit() == 0) return false;
        if (source.effect_id() == 6 && (source.cap_bps() == 0 || source.active_rounds() == 0)) return false;
        if (source.effect_id() == 4 && source.health_threshold_bps() == 0) return false;
    }
    *actor.mutable_equipment_effects() = effects;
    auto* sources = actor.mutable_equipment_effects()->mutable_artifacts();
    std::sort(sources->begin(), sources->end(), [](const auto& left, const auto& right) { return left.item_id() < right.item_id(); });
    for (const auto& source : *sources) actor.add_artifact_states()->set_item_id(source.item_id());
    actor.set_entry_max_health(actor.max_health());
    actor.set_entry_max_mana(actor.max_mana());
    return true;
}

uint32_t TurnBattleEngine::MainEquipmentAilment(const SkillTable& skill) const {
    // effect[] 顺序就是技能效果判定顺序。仅技能自身的主障碍消费回响，子触发不冒充普通施法。
    for (const auto id : skill.effect()) {
        const auto* row = dataProvider->FindBuff(id);
        if (row != nullptr && IsAilment(row->state_kind()) &&
            (row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Control) ||
             row->battle_op() == static_cast<uint32_t>(BattleEffectOp::PeriodicDamage))) return row->state_kind();
    }
    return 0;
}

uint32_t TurnBattleEngine::EquipmentManaCategory(const SkillTable& skill) const {
    if (skill.battle_model() != 1) return 0;
    for (const auto id : skill.effect()) {
        const auto* row = dataProvider->FindBuff(id);
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Damage))
            return skill.damage_type() == combatdamage::kPhysicalDamage ? 1u : 2u;
    }
    bool restoreMana = false;
    for (const auto id : skill.effect()) {
        const auto* row = dataProvider->FindBuff(id);
        if (row == nullptr) continue;
        switch (static_cast<BattleEffectOp>(row->battle_op())) {
        case BattleEffectOp::Heal: case BattleEffectOp::ReviveMark: return 3;
        case BattleEffectOp::Shield: return 4;
        case BattleEffectOp::Control:
            if (IsAilment(row->state_kind())) return row->state_kind() + 4;
            break;
        case BattleEffectOp::PeriodicDamage:
            if (row->state_kind() == static_cast<uint32_t>(BattleStateKind::Poison)) return 11;
            break;
        case BattleEffectOp::ConsumePeriodic: case BattleEffectOp::ExtendPeriodic: return 11;
        case BattleEffectOp::RestoreMana: restoreMana = true; break;
        default: break;
        }
    }
    return restoreMana ? 0 : 12;
}

std::vector<int> TurnBattleEngine::AvailableArtifacts(const BattleActorState& actor, uint32_t effect) const {
    std::vector<int> available;
    if (actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || !IsActorActive(actor)) return available;
    const auto& sources = actor.equipment_effects().artifacts();
    for (int i = 0; i < sources.size() && i < actor.artifact_states_size(); ++i) {
        const auto& source = sources.Get(i); const auto& state = actor.artifact_states(i);
        if (source.effect_id() != effect || (source.active_rounds() > 0 && roundIndex > source.active_rounds()) ||
            (source.battle_limit() > 0 && state.used_count() >= source.battle_limit()) ||
            (source.per_round_limit() > 0 && state.last_trigger_round() == roundIndex &&
                state.round_trigger_count() >= source.per_round_limit())) continue;
        available.push_back(i);
    }
    return available;
}

void TurnBattleEngine::UseArtifact(BattleActorState& actor, int index, uint64_t targetId,
                                    uint64_t value, TurnResultS2C& result) {
    const auto& source = actor.equipment_effects().artifacts(index);
    auto* state = actor.mutable_artifact_states(index);
    state->set_used_count(state->used_count() + 1);
    state->set_round_trigger_count(state->last_trigger_round() == roundIndex ? state->round_trigger_count() + 1 : 1);
    state->set_last_trigger_round(roundIndex);
    auto* event = AppendEvent(result, BATTLE_EVENT_ARTIFACT, actor.actor_id(), targetId);
    event->set_artifact_item_id(source.item_id()); event->set_artifact_effect_id(source.effect_id());
    event->set_value(value); event->set_success(true);
}

void TurnBattleEngine::EquipmentDefend(BattleActorState& actor, TurnResultS2C& result) {
    const uint64_t before = actor.attributes().mana();
    const uint64_t missing = actor.max_mana() > before ? actor.max_mana() - before : 0;
    uint64_t total = 0;
    // 每件以相同入战上限计算，不拿前件回蓝后的余额重算比例。缺口只分配实际收益。
    for (const int index : AvailableArtifacts(actor, 7)) {
        const auto& source = actor.equipment_effects().artifacts(index);
        const uint64_t gain = std::min(missing - total, equipbattle::FloorBps(actor.entry_max_mana(), source.bonus_bps()));
        if (gain == 0) continue;
        total += gain; UseArtifact(actor, index, actor.actor_id(), gain, result);
    }
    if (total == 0) return;
    actor.mutable_attributes()->set_mana(before + total);
    auto* event = AppendEvent(result, BATTLE_EVENT_MANA, actor.actor_id(), actor.actor_id());
    event->set_value(total); event->set_mana_delta(static_cast<int64_t>(std::min<uint64_t>(total, INT64_MAX)));
    event->set_target_mana_after(before + total);
}

uint64_t TurnBattleEngine::EquipmentReduceDamage(BattleActorState& source, BattleActorState& target,
                                                 uint64_t damage, const EffectContext& context, TurnResultS2C& result) {
    if (damage == 0 || source.team_index() == target.team_index() ||
        (context.origin != BattleDamageOrigin::Direct && context.origin != BattleDamageOrigin::Pursuit &&
         context.origin != BattleDamageOrigin::Mirror)) return damage;
    uint64_t reduction = 0;
    for (const int index : AvailableArtifacts(target, 6)) {
        const auto& artifact = target.equipment_effects().artifacts(index);
        const uint64_t part = std::min({damage - reduction, equipbattle::FloorBps(damage, artifact.bonus_bps()),
            equipbattle::FloorBps(target.entry_max_health(), artifact.cap_bps())});
        if (part == 0) continue;
        reduction += part; UseArtifact(target, index, target.actor_id(), part, result);
    }
    return damage - reduction;
}

void TurnBattleEngine::EquipmentBreakShields(BattleActorState& source, BattleActorState& target,
                                             uint64_t damage, uint32_t type, const EffectContext& context,
                                             const std::vector<uint64_t>& shields, TurnResultS2C& result) {
    if (!IsActorActive(target) || context.origin != BattleDamageOrigin::Direct ||
        context.primaryTargetId != target.actor_id() || context.hitOrdinal != 0 || damage == 0) return;
    const uint32_t effect = type == combatdamage::kPhysicalDamage ? 1u : type == combatdamage::kMagicDamage ? 2u : 0u;
    uint32_t accumulated = 0;
    for (const int index : AvailableArtifacts(source, effect)) {
        const auto& artifact = source.equipment_effects().artifacts(index);
        const uint64_t before = equipbattle::FloorBps(damage, accumulated);
        accumulated += artifact.bonus_bps();
        uint64_t left = equipbattle::FloorBps(damage, accumulated) - before, removed = 0;
        for (const auto id : shields) {
            if (left == 0) break;
            const int buffIndex = FindBuffIndex(target, id); if (buffIndex < 0) continue;
            const auto saved = target.buffs(buffIndex);
            const uint64_t part = std::min(left, saved.remaining_capacity());
            if (part == 0) continue;
            left -= part; removed += part;
            target.mutable_buffs(buffIndex)->set_remaining_capacity(saved.remaining_capacity() - part);
            auto* event = AppendEvent(result, BATTLE_EVENT_SHIELD, source.actor_id(), target.actor_id());
            event->set_buff_table_id(saved.buff_table_id()); event->set_buff_instance_id(id);
            event->set_absorbed(part); event->set_shield_after(saved.remaining_capacity() - part);
            event->set_artifact_item_id(artifact.item_id()); event->set_artifact_effect_id(effect);
            if (part == saved.remaining_capacity()) RemoveConfiguredBuff(target, id, result);
        }
        if (removed > 0) UseArtifact(source, index, target.actor_id(), removed, result);
    }
}

double TurnBattleEngine::EquipmentOutgoingHeal(const BattleActorState& source) const {
    return OutgoingHealBonus(source) + ConfiguredModifier(source, BattleModifier::OutgoingHeal) +
        source.equipment_effects().outgoing_heal_bps() / 10000.0;
}

double TurnBattleEngine::EquipmentShield(BattleActorState& source, BattleActorState& target, double base,
                                         const EffectContext& context, std::vector<int>& contributions) const {
    if (source.actor_type() != BATTLE_ACTOR_TYPE_PLAYER) return base;
    double bonus = source.equipment_effects().shield_capacity_bps() / 10000.0 +
        ConfiguredModifier(source, BattleModifier::OutgoingShield);
    if (context.origin == BattleDamageOrigin::Direct && context.primaryTargetId == target.actor_id() && context.shieldOrdinal == 0) {
        for (const int index : AvailableArtifacts(source, 5)) {
            const uint32_t bps = source.equipment_effects().artifacts(index).bonus_bps();
            const double after = base * (1 + bonus + bps / 10000.0);
            if (std::floor(after) > std::floor(base * (1 + bonus))) contributions.push_back(index);
            bonus += bps / 10000.0;
        }
    }
    return base * std::max(0.0, 1 + bonus);
}
void TurnBattleEngine::CommitEquipmentShield(BattleActorState& source, uint64_t targetId,
                                             const std::vector<int>& contributions, TurnResultS2C& result) {
    for (const int index : contributions) UseArtifact(source, index, targetId,
        source.equipment_effects().artifacts(index).bonus_bps(), result);
}

bool TurnBattleEngine::EquipmentControlRoll(BattleActorState& caster, BattleActorState& target,
                                            const BuffTable& row, const EffectContext& context, TurnResultS2C& result) {
    const uint32_t kind = row.state_kind();
    if (!IsAilment(kind) || !std::isfinite(row.application_chance()) || row.application_chance() <= 0) return false;
    const auto* skill = dataProvider->FindSkill(context.skillId);
    const bool normal = context.origin == BattleDamageOrigin::Direct && context.primaryTargetId != 0;
    uint32_t primaryBuff = 0;
    if (skill != nullptr) for (const auto id : skill->effect()) {
        const auto* candidate = dataProvider->FindBuff(id);
        if (candidate != nullptr && IsAilment(candidate->state_kind()) &&
            (candidate->battle_op() == static_cast<uint32_t>(BattleEffectOp::Control) ||
             candidate->battle_op() == static_cast<uint32_t>(BattleEffectOp::PeriodicDamage))) { primaryBuff = id; break; }
    }
    const bool primary = normal && context.primaryTargetId == target.actor_id() && primaryBuff == row.id();
    double hit = Value(caster.equipment_effects().ailment_hit_bps(), kind);
    double resist = Value(target.equipment_effects().ailment_resist_bps(), kind);
    const auto& combat = target.combat();
    switch (static_cast<BattleStateKind>(kind)) {
    case BattleStateKind::Poison: resist += 100.0 * combat.resist_poison(); break;
    case BattleStateKind::Freeze: resist += 100.0 * combat.resist_freeze(); break;
    case BattleStateKind::Sleep: resist += 100.0 * combat.resist_sleep(); break;
    case BattleStateKind::Confusion: resist += 100.0 * combat.resist_confusion(); break;
    default: resist += 100.0 * combat.resist_forget(); break;
    }
    if (kind == 4 || kind == 5) resist += 10000 * ConfiguredModifier(target, BattleModifier::ControlResist);
    bool consumedEcho = false;
    if (primary) for (int index = 0; index < caster.artifact_states_size(); ++index) {
        auto* state = caster.mutable_artifact_states(index);
        if (state->pending_round() != roundIndex || state->pending_ailment_kind() != kind) continue;
        hit += caster.equipment_effects().artifacts(index).bonus_bps();
        state->clear_pending_ailment_kind(); state->clear_pending_round(); consumedEcho = true;
    }
    if (normal && kind <= 6) for (const int index : AvailableArtifacts(target, 8)) {
        resist += target.equipment_effects().artifacts(index).bonus_bps();
        UseArtifact(target, index, target.actor_id(), target.equipment_effects().artifacts(index).bonus_bps(), result);
    }
    const double chance = equipbattle::AilmentChance(row.application_chance(), hit, resist,
        100.0 * combat.resist_all_ailment(), 100.0 * caster.combat().ignore_ailment_resist(), kind == 7);
    const bool success = chance > 0 && Rand01() < chance;
    if (!success) {
        auto* event = AppendEvent(result, BATTLE_EVENT_RESIST, caster.actor_id(), target.actor_id());
        event->set_buff_table_id(row.id());
        if (primary && !consumedEcho) for (const int index : AvailableArtifacts(caster, 3)) {
            auto* state = caster.mutable_artifact_states(index);
            if (state->pending_round() != 0) continue;
            state->set_pending_ailment_kind(kind); state->set_pending_round(roundIndex + 1);
            UseArtifact(caster, index, caster.actor_id(), kind, result);
        }
    }
    return success;
}
} // namespace turnbattle

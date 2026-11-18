#include "system/turn_battle_engine.h"
#include "system/combat_damage_rules.h"
#include "system/equipment_battle_rules.h"
#include "system/support_effect_rules.h"
#include "system/pet_cultivation_rules.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include <algorithm>
#include <cmath>
#include <limits>

namespace turnbattle {
namespace {
uint64_t Whole(double value) {
    if (!(value > 0) || !std::isfinite(value)) return 0;
    return static_cast<long double>(value) >= std::numeric_limits<uint64_t>::max()
        ? UINT64_MAX : static_cast<uint64_t>(value);
}
uint64_t AddSafe(uint64_t a, uint64_t b) { return b > UINT64_MAX - a ? UINT64_MAX : a + b; }
bool Persistent(BattleEffectOp op) {
    switch (op) {
    case BattleEffectOp::Damage: case BattleEffectOp::Heal: case BattleEffectOp::RestoreMana:
    case BattleEffectOp::DispelDebuff: case BattleEffectOp::DispelBuff:
    case BattleEffectOp::ConsumePeriodic: case BattleEffectOp::ExtendPeriodic:
    case BattleEffectOp::DrainMana: case BattleEffectOp::RetreatPet: return false;
    default: return true;
    }
}
bool MatchesDamage(const BuffTable& row, uint32_t type) {
    return row.damage_filter() == 0 || (row.damage_filter() == 1 && type == combatdamage::kPhysicalDamage) ||
        (row.damage_filter() == 2 && type == combatdamage::kMagicDamage);
}
bool IsDirect(BattleDamageOrigin origin) {
    return origin == BattleDamageOrigin::Direct || origin == BattleDamageOrigin::Pursuit ||
        origin == BattleDamageOrigin::Mirror || origin == BattleDamageOrigin::Confusion;
}
}

bool TurnBattleEngine::ConfiguredSubjectAllowed(const BattleActorState& actor, const SkillTable& row) const {
    const uint32_t mask = actor.actor_type() == BATTLE_ACTOR_TYPE_PET ? 2u : 1u;
    return row.battle_model() == 1 && (row.caster_mask() & mask) != 0 && actor.level() >= row.unlock_level();
}
uint32_t TurnBattleEngine::ConfiguredRank(const BattleActorState& actor, const SkillTable& row) const {
    if (!ConfiguredSubjectAllowed(actor, row)) return 0;
    const auto found = actor.skill_ranks().find(row.id());
    return found == actor.skill_ranks().end() ? 0u : found->second;
}
bool TurnBattleEngine::ConfiguredTargetAllowed(const BattleActorState& actor, const BattleActorState& target,
                                                const SkillTable& row) const {
    if (target.fled() || (target.is_dead() && row.allow_dead_target() == 0)) return false;
    if (row.target_team() == 3) return target.actor_id() == actor.actor_id();
    if ((target.team_index() == actor.team_index()) != (row.target_team() == 2)) return false;
    if (row.health_cost_mode() == 2 && actor.actor_id() == target.actor_id()) return false;
    for (const auto id : row.effect()) {
        const auto* effect = dataProvider->FindBuff(id);
        if (effect != nullptr && static_cast<BattleEffectOp>(effect->battle_op()) == BattleEffectOp::RetreatPet &&
            target.actor_type() != BATTLE_ACTOR_TYPE_PET) return false;
    }
    return true;
}
uint32_t TurnBattleEngine::CheckConfiguredSkill(const BattleActorState& actor, const BattleAction& action,
                                                const SkillTable& row) const {
    if (!ConfiguredSubjectAllowed(actor, row) || ConfiguredRank(actor, row) == 0 || row.ability_kind() != 1)
        return kSkillCannotBeCastInCurrentState;
    if (const auto blocked = CheckBuff(actor, row); blocked != kSuccess) return blocked;
    if (HasConfiguredState(actor, BattleStateKind::SealActive) ||
        (HasConfiguredState(actor, BattleStateKind::SealPhysical) && (row.action_tags() & kActionTagPhysical)) ||
        (HasConfiguredState(actor, BattleStateKind::SealMagic) && (row.action_tags() & kActionTagMagic)))
        return kSkillCannotBeCastInCurrentState;
    const auto cooling = actor.skill_cooldown_rounds().find(row.id());
    if (cooling != actor.skill_cooldown_rounds().end() && cooling->second > 0) return kSkillCooldownNotReady;
    const auto used = actor.ability_use_counts().find(row.id());
    if (row.max_battle_uses() > 0 && used != actor.ability_use_counts().end() && used->second >= row.max_battle_uses())
        return kSkillCannotBeCastInCurrentState;
    if ((row.health_cost_mode() == 1 && actor.attributes().health() <= 1) ||
        SkillManaCost(actor, row) > actor.attributes().mana()) return kSkillCannotBeCastInCurrentState;
    if (action.target_id() != 0) {
        const auto* target = FindActor(action.target_id());
        return target != nullptr && ConfiguredTargetAllowed(actor, *target, row) ? kSuccess : kSkillInvalidTarget;
    }
    for (const auto& candidate : actors) if (ConfiguredTargetAllowed(actor, candidate, row)) return kSuccess;
    return kSkillInvalidTarget;
}
std::vector<uint64_t> TurnBattleEngine::SelectConfiguredTargets(const BattleActorState& actor,
                                                                const SkillTable& row, uint64_t primaryId) {
    std::vector<uint64_t> targets, candidates;
    const uint32_t count = row.target_team() == 3 ? 1u : std::max(1u,
        actor.actor_type() == BATTLE_ACTOR_TYPE_PET ? row.pet_max_targets() : row.max_targets());
    for (const auto& candidate : actors) {
        if (!ConfiguredTargetAllowed(actor, candidate, row)) continue;
        if (candidate.actor_id() == primaryId) targets.push_back(primaryId);
        else candidates.push_back(candidate.actor_id());
    }
    while (targets.size() < count && !candidates.empty()) {
        const size_t index = candidates.size() == 1 ? 0 : static_cast<size_t>(RandIndex(candidates.size()));
        targets.push_back(candidates[index]);
        candidates.erase(candidates.begin() + index);
    }
    return targets;
}
bool TurnBattleEngine::HasConfiguredBuffs(const BattleActorState& actor) const {
    for (const auto& entry : actor.buffs()) {
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && row->battle_op() != 0) return true;
    }
    return false;
}
bool TurnBattleEngine::HasConfiguredState(const BattleActorState& actor, BattleStateKind state) const {
    for (const auto& entry : actor.buffs()) {
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Control) &&
            row->state_kind() == static_cast<uint32_t>(state)) return true;
    }
    return false;
}
bool TurnBattleEngine::HasBuffTag(const BattleActorState& actor, const std::string& tag) const {
    if (tag.empty()) return true;
    for (const auto& entry : actor.buffs()) {
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && row->tag().contains(tag)) return true;
    }
    return false;
}
double TurnBattleEngine::ConfiguredModifier(const BattleActorState& actor, BattleModifier kind,
                                             SupportEffectBreakdown* sources) const {
    supportrules::ModifierSelection selected;
    for (const auto& entry : actor.buffs()) {
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row == nullptr || (row->battle_op() != static_cast<uint32_t>(BattleEffectOp::Modifier) &&
            row->battle_op() != static_cast<uint32_t>(BattleEffectOp::GrowingAura)) ||
            row->modifier_stat() != static_cast<uint32_t>(kind)) continue;
        const auto* source = FindActor(entry.caster_id());
        const bool available = !row->require_living_caster() || (source != nullptr && IsActorActive(*source));
        const double value = row->ratio() * (row->battle_op() == static_cast<uint32_t>(BattleEffectOp::GrowingAura)
            ? entry.stacks() : 1u);
        int index = -1;
        if (sources != nullptr) {
            index = sources->sources_size();
            auto* detail = sources->add_sources();
            detail->set_kind(IsConfiguredListenerBuff(entry) ? 2 : 3);
            detail->set_effect_kind(kind == BattleModifier::OutgoingShield ? 2 : 1);
            detail->set_buff_instance_id(entry.buff_id()); detail->set_skill_table_id(entry.source_skill_id());
            const auto* skill = dataProvider->FindSkill(entry.source_skill_id());
            detail->set_name(skill == nullptr ? "状态 #" + std::to_string(entry.buff_table_id()) : skill->name());
            detail->set_raw_bonus_bps(value * 10000); detail->set_remain_rounds(entry.remain_rounds());
            detail->set_infinite(row->infinite_duration() != 0);
            detail->set_reason(available ? "同类更强修正优先生效" : "要求来源仍存活，当前未生效");
        }
        if (available) selected.Consider(value, index);
    }
    if (sources != nullptr) selected.Project(*sources);
    return selected.Total();
}
double TurnBattleEngine::ConfiguredValue(const BattleActorState& caster, const BattleActorState& target,
                                          const BuffTable& row, const EffectContext& context) const {
    double basis = 0;
    switch (row.value_basis()) {
    case 1: basis = static_cast<double>(EffectivePhysicalAttack(caster)); break;
    case 2: basis = static_cast<double>(caster.magic_attack()) * std::max(0.1, 1 + ConfiguredModifier(caster, BattleModifier::MagicAttack)); break;
    case 3: basis = static_cast<double>(target.max_health()); break;
    case 4: basis = static_cast<double>(target.max_mana()); break;
    case 5: basis = static_cast<double>(context.paidHealth); break;
    case 6: basis = static_cast<double>(context.paidMana); break;
    case 7: basis = static_cast<double>(context.actualDamage); break;
    default: break;
    }
    return supportrules::BaseValue(row.flat_value(), row.per_level(), caster.level(), basis, row.ratio(), context.rank);
}
void TurnBattleEngine::ExecuteConfiguredSkill(BattleActorState& actor, const BattleAction& action,
                                               const SkillTable& row, TurnResultS2C& result,
                                               BattleDamageOrigin origin, double scale) {
    const bool normal = origin == BattleDamageOrigin::Direct;
    if (normal && CheckActionPrerequisites(actor, action) != kSuccess) return;
    auto targets = SelectConfiguredTargets(actor, row, action.target_id());
    if (origin == BattleDamageOrigin::Confusion) targets = {action.target_id()};
    if (targets.empty()) return;
    EffectContext context;
    context.skillId = row.id(); context.rank = ConfiguredRank(actor, row);
    if (origin == BattleDamageOrigin::Mirror) context.rank = std::max(1u, context.rank);
    context.origin = origin; context.scale = scale;
    context.primaryTargetId = targets.front();
    auto* event = AppendEvent(result, BATTLE_EVENT_SKILL, actor.actor_id(), targets.front());
    event->set_skill_table_id(row.id());
    event->set_hit_kind(origin == BattleDamageOrigin::Mirror ? BATTLE_HIT_MIRROR : BATTLE_HIT_NORMAL);
    if (normal || origin == BattleDamageOrigin::Confusion) {
        context.paidMana = SkillManaCost(actor, row);
        ConsumeSkillMana(actor, row, row.id(), result);
        if (row.cooldown_rounds() > 0) (*actor.mutable_skill_cooldown_rounds())[row.id()] = row.cooldown_rounds();
        ++(*actor.mutable_ability_use_counts())[row.id()];
        if (row.health_cost_mode() > 0) {
            const uint64_t after = row.health_cost_mode() == 1 ? 1 : 0;
            context.paidHealth = actor.attributes().health() - after;
            actor.mutable_attributes()->set_health(after);
            auto* paid = AppendEvent(result, BATTLE_EVENT_DAMAGE, actor.actor_id(), actor.actor_id());
            paid->set_skill_table_id(row.id()); paid->set_value(context.paidHealth);
            paid->set_damage_origin(static_cast<uint32_t>(BattleDamageOrigin::ResourceCost));
            paid->set_target_health_after(after);
            if (after == 0) HandleDeath(actor, actor.actor_id(), result);
        }
    }
    const int effectsBegin = result.events_size();
    for (size_t i = 0; i < targets.size(); ++i) {
        auto* target = FindActor(targets[i]);
        if (target == nullptr || target->fled()) continue;
        currentHitIndex = static_cast<uint32_t>(i);
        // 同一目标的条件在消费持续状态之前求定,毒火联动不能因第一项消费而丢失第二项。
        std::vector<uint32_t> effects;
        for (const auto effectId : row.effect()) {
            const auto* effect = dataProvider->FindBuff(effectId);
            if (effect == nullptr) continue;
            if (effect->battle_op() == static_cast<uint32_t>(BattleEffectOp::ConsumePeriodic) &&
                (!HasBuffTag(*target, effect->required_tag()) || !HasBuffTag(*target, effect->second_required_tag()))) continue;
            effects.push_back(effectId);
        }
        auto targetContext = context;
        for (const auto effect : effects) {
            ApplyConfiguredEffect(actor, *target, effect, targetContext, result);
            const auto* definition = dataProvider->FindBuff(effect);
            if (definition != nullptr && definition->battle_op() == static_cast<uint32_t>(BattleEffectOp::Damage)) ++targetContext.hitOrdinal;
            if (definition != nullptr && definition->battle_op() == static_cast<uint32_t>(BattleEffectOp::Heal)) ++targetContext.healOrdinal;
            if (definition != nullptr && definition->battle_op() == static_cast<uint32_t>(BattleEffectOp::Shield)) ++targetContext.shieldOrdinal;
        }
    }
    currentHitIndex = 0;
    bool hitDirectly = false;
    for (int i = effectsBegin; i < result.events_size(); ++i) {
        const auto& damage = result.events(i);
        if (damage.event_type() == BATTLE_EVENT_DAMAGE && damage.source_id() == actor.actor_id() &&
            damage.skill_table_id() == row.id() && damage.value() > 0 && damage.damage_origin() == 0) hitDirectly = true;
    }
    if (normal && hitDirectly && IsActorActive(actor)) {
        auto* primary = FindActor(targets.front());
        if (primary != nullptr) TriggerConfiguredBuffs(actor, *primary, BattleTrigger::AfterSkill,
            row.damage_type(), context, result);
    }
}

void TurnBattleEngine::ApplyConfiguredEffect(BattleActorState& caster, BattleActorState& target,
                                              uint32_t buffId, const EffectContext& context,
                                              TurnResultS2C& result, uint32_t depth) {
    if (depth > kMaxSubBuffDepth) return;
    const auto* config = dataProvider->FindBuff(buffId);
    if (config == nullptr) return;
    const BuffTable row = *config;
    const auto op = static_cast<BattleEffectOp>(row.battle_op());
    if (op == BattleEffectOp::None) { AddBuffToActor(target, buffId, caster.actor_id(), depth, result); return; }
    BattleActorState& receiver = row.effect_target() == 1 || row.effect_target() == 3 ? caster : target;
    if (receiver.fled() || (receiver.is_dead() && op != BattleEffectOp::ReviveMark)) return;
    const bool hostile = receiver.team_index() != caster.team_index();
    if (HasConfiguredState(receiver, BattleStateKind::Freeze) && hostile) return;
    double value = ConfiguredValue(caster, receiver, row, context);
    if (op == BattleEffectOp::PeriodicDamage && row.state_kind() == static_cast<uint32_t>(BattleStateKind::Poison))
        value = supportrules::ApplyBonus(value, petcultivation::PoisonBonus(caster));
    // 持续治疗的覆盖强度与每跳快照采用同一完整施放方值，避免旧印因施法者加成不同被弱覆盖。
    if (op == BattleEffectOp::ReviveMark) value *= std::max(0.0, 1 + EquipmentOutgoingHeal(caster));
    std::vector<int> shieldArtifacts;
    if (op == BattleEffectOp::Shield && row.hit_limit() == 0)
        value = EquipmentShield(caster, receiver, value, context, shieldArtifacts);
    if (!Persistent(op) && row.trigger_event() == 0) {
        switch (op) {
        case BattleEffectOp::Damage: {
            const auto* skill = dataProvider->FindSkill(context.skillId);
            if (skill == nullptr || receiver.is_dead()) break;
            bool critical = false;
            const uint64_t attack = skill->damage_type() == combatdamage::kPhysicalDamage ? EffectivePhysicalAttack(caster) :
                Whole(caster.magic_attack() * std::max(0.1, 1 + ConfiguredModifier(caster, BattleModifier::MagicAttack)));
            double amount = CalculateFinalDamage(caster, receiver,
                dataProvider->GetSkillDamage(skill->id(), caster.level()), attack, skill->attack_multiplier(),
                skill->damage_type(), critical) * classskill::DamageRankFactor(context.rank) * context.scale;
            if (!row.required_tag().empty() && HasBuffTag(receiver, row.required_tag())) amount *= 1 + row.conditional_bonus();
            auto damageContext = context; damageContext.critical = critical;
            const uint64_t dealt = ResolveConfiguredDamage(caster, receiver, amount, skill->damage_type(), damageContext,
                result, context.origin == BattleDamageOrigin::Mirror ? BATTLE_HIT_MIRROR : BATTLE_HIT_NORMAL);
            if (row.secondary_ratio() > 0 && dealt > 0 && IsActorActive(caster)) {
                const uint64_t heal = ApplyHeal(caster, dealt * row.secondary_ratio());
                auto* healing = AppendEvent(result, BATTLE_EVENT_HEAL, caster.actor_id(), caster.actor_id());
                healing->set_value(heal); healing->set_target_health_after(caster.attributes().health());
            }
            break;
        }
        case BattleEffectOp::Heal: {
            double healing = value;
            const auto* skill = dataProvider->FindSkill(context.skillId);
            if (skill != nullptr && receiver.max_health() > 0 &&
                static_cast<double>(receiver.attributes().health()) / receiver.max_health() < skill->execute_heal_threshold())
                healing *= 1 + skill->execute_heal_bonus();
            auto healingContext = context;
            healingContext.equipmentHeal = skill != nullptr && skill->battle_model() == 1;
            ResolveConfiguredHeal(caster, receiver, healing, healingContext, result); break;
        }
        case BattleEffectOp::RestoreMana: case BattleEffectOp::DrainMana: {
            const uint64_t before = receiver.attributes().mana();
            const uint64_t delta = op == BattleEffectOp::DrainMana ? std::min(before, Whole(value)) :
                std::min(receiver.max_mana() > before ? receiver.max_mana() - before : 0, Whole(value));
            receiver.mutable_attributes()->set_mana(op == BattleEffectOp::DrainMana ? before - delta : before + delta);
            auto* changed = AppendEvent(result, BATTLE_EVENT_MANA, caster.actor_id(), receiver.actor_id());
            changed->set_value(delta); changed->set_target_mana_after(receiver.attributes().mana());
            changed->set_mana_delta(op == BattleEffectOp::DrainMana ? -static_cast<int64_t>(std::min<uint64_t>(delta, INT64_MAX)) :
                static_cast<int64_t>(std::min<uint64_t>(delta, INT64_MAX)));
            break;
        }
        case BattleEffectOp::DispelDebuff: case BattleEffectOp::DispelBuff: {
            std::vector<uint64_t> ids;
            for (const auto& entry : receiver.buffs()) if (IsDebuffEntry(entry) == (op == BattleEffectOp::DispelDebuff)) {
                const auto* existing = dataProvider->FindBuff(entry.buff_table_id());
                if (existing != nullptr && !IsConfiguredListenerBuff(entry)) ids.push_back(entry.buff_id());
            }
            std::stable_sort(ids.begin(), ids.end(), [&](uint64_t left, uint64_t right) {
                const auto* a = dataProvider->FindBuff(receiver.buffs(FindBuffIndex(receiver, left)).buff_table_id());
                const auto* b = dataProvider->FindBuff(receiver.buffs(FindBuffIndex(receiver, right)).buff_table_id());
                return a != nullptr && a->battle_op() == static_cast<uint32_t>(BattleEffectOp::Control) &&
                    (b == nullptr || b->battle_op() != static_cast<uint32_t>(BattleEffectOp::Control));
            });
            const size_t limit = row.effect_count() == 0 ? ids.size() : std::min<size_t>(ids.size(), row.effect_count());
            for (size_t i = 0; i < limit; ++i) RemoveConfiguredBuff(receiver, ids[i], result);
            break;
        }
        case BattleEffectOp::ConsumePeriodic: case BattleEffectOp::ExtendPeriodic: {
            std::vector<uint64_t> ids;
            for (const auto& entry : receiver.buffs()) {
                const auto* existing = dataProvider->FindBuff(entry.buff_table_id());
                if (existing != nullptr && existing->battle_op() == static_cast<uint32_t>(BattleEffectOp::PeriodicDamage) &&
                    (existing->tag().contains(row.required_tag()) || (!row.second_required_tag().empty() && existing->tag().contains(row.second_required_tag()))))
                    ids.push_back(entry.buff_id());
            }
            for (const auto id : ids) {
                const int index = FindBuffIndex(receiver, id); if (index < 0) continue;
                const BattleBuffEntry saved = receiver.buffs(index);
                if (op == BattleEffectOp::ExtendPeriodic) {
                    if (saved.extensions_used() == 0) {
                        receiver.mutable_buffs(index)->set_remaining_ticks(saved.remaining_ticks() + std::max(1u, row.effect_count()));
                        receiver.mutable_buffs(index)->set_extensions_used(1);
                    }
                    continue;
                }
                const uint32_t count = std::min(saved.remaining_ticks(), std::max(1u, row.effect_count()));
                receiver.mutable_buffs(index)->set_remaining_ticks(saved.remaining_ticks() - count);
                auto* original = FindActor(saved.caster_id());
                EffectContext periodic = context; periodic.origin = BattleDamageOrigin::Periodic;
                periodic.instigatorId = caster.actor_id();
                periodic.skillId = saved.source_skill_id();
                const auto* periodicRow = dataProvider->FindBuff(saved.buff_table_id());
                periodic.periodicState = periodicRow == nullptr ? 0 : periodicRow->state_kind();
                for (uint32_t tick = 0; tick < count && !receiver.is_dead(); ++tick)
                    ResolveConfiguredDamage(original == nullptr ? caster : *original, receiver,
                        static_cast<double>(saved.snapshot_value()), combatdamage::kMagicDamage, periodic, result);
                const int remaining = FindBuffIndex(receiver, id);
                if (remaining >= 0 && receiver.buffs(remaining).remaining_ticks() == 0) RemoveConfiguredBuff(receiver, id, result);
            }
            break;
        }
        case BattleEffectOp::RetreatPet: {
            if (receiver.actor_type() != BATTLE_ACTOR_TYPE_PET || !hostile) break;
            const double chance = std::clamp((row.application_chance() > 0 ? row.application_chance() : 1.0) -
                ConfiguredModifier(receiver, BattleModifier::RetreatResist) + petcultivation::RetreatHit(caster) -
                petcultivation::RetreatResist(receiver), 0.0, 1.0);
            const bool success = chance >= 1 || (chance > 0 && Rand01() < chance);
            auto* event = AppendEvent(result, BATTLE_EVENT_PET_RETREAT, caster.actor_id(), receiver.actor_id());
            event->set_success(success); event->set_value(1);
            if (success) { receiver.set_fled(true); receiver.set_withdraw_until_round(roundIndex + std::max(1u, row.duration_rounds()) + 1); }
            break;
        }
        default: break;
        }
        return;
    }
    if (IsImmuneToBuff(receiver, row)) return;
    const uint32_t key = row.stack_key() == 0 ? row.id() : row.stack_key();
    const uint64_t strength = op == BattleEffectOp::Modifier || op == BattleEffectOp::GrowingAura
        ? Whole(std::abs(row.ratio()) * 1000000) : Whole(value);
    uint64_t existingId = 0;
    for (const auto& entry : receiver.buffs()) {
        const auto* existing = dataProvider->FindBuff(entry.buff_table_id());
        if (existing == nullptr || (existing->stack_key() == 0 ? existing->id() : existing->stack_key()) != key) continue;
        if (row.refresh_policy() == 0 && entry.caster_id() != caster.actor_id()) continue;
        const uint64_t oldStrength = row.refresh_policy() == 2 ? entry.remaining_capacity() : entry.snapshot_value();
        if (row.refresh_policy() != 0 && strength < oldStrength) return;
        existingId = entry.buff_id(); break;
    }
    // 覆盖拒绝与免疫均不进入概率阶段，不生成回响、不消耗清心。
    if (hostile && row.state_kind() >= 1 && row.state_kind() <= 7 &&
        (op == BattleEffectOp::Control || op == BattleEffectOp::PeriodicDamage)) {
        if (!EquipmentControlRoll(caster, receiver, row, context, result)) return;
    } else if (hostile && row.application_chance() > 0 && row.application_chance() < 1 && Rand01() >= row.application_chance()) {
        auto* resisted = AppendEvent(result, BATTLE_EVENT_RESIST, caster.actor_id(), receiver.actor_id());
        resisted->set_buff_table_id(buffId); return;
    }
    uint32_t extensions = 0;
    if (existingId != 0) {
        const int i = FindBuffIndex(receiver, existingId); if (i >= 0) extensions = receiver.buffs(i).extensions_used();
        RemoveConfiguredBuff(receiver, existingId, result);
    }
    auto* added = receiver.add_buffs(); const uint64_t instance = nextBuffInstanceId++;
    added->set_buff_id(instance); added->set_buff_table_id(row.id()); added->set_caster_id(caster.actor_id());
    added->set_source_skill_id(context.skillId); added->set_layer(1);
    added->set_snapshot_value(strength); added->set_remaining_capacity(Whole(value));
    added->set_remaining_hits(row.hit_limit()); added->set_remaining_ticks(row.tick_count());
    added->set_remain_rounds(row.duration_rounds()); added->set_applied_round(roundIndex);
    added->set_tick_round(row.tick_on_apply() ? roundIndex : roundIndex + 1);
    added->set_extensions_used(extensions);
    if (op == BattleEffectOp::ReviveMark)
        added->set_outgoing_heal_snapshot(value);
    if (op == BattleEffectOp::Modifier && row.modifier_stat() == static_cast<uint32_t>(BattleModifier::AdaptiveDamage))
        added->set_chosen_stat(receiver.physical_attack() >= receiver.magic_attack() ? 1 : 2);
    if (op == BattleEffectOp::TemporaryHealth) {
        const uint64_t extra = std::min(Whole(value), receiver.max_health());
        added->set_secondary_value(extra); receiver.set_max_health(AddSafe(receiver.max_health(), extra));
        receiver.mutable_attributes()->set_health(AddSafe(receiver.attributes().health(), extra));
        auto* event = AppendEvent(result, BATTLE_EVENT_MAX_HEALTH, caster.actor_id(), receiver.actor_id());
        event->set_target_max_health_after(receiver.max_health()); event->set_target_health_after(receiver.attributes().health());
    }
    auto* event = AppendEvent(result, BATTLE_EVENT_BUFF_ADD, caster.actor_id(), receiver.actor_id());
    event->set_buff_table_id(row.id()); event->set_buff_instance_id(instance); event->set_value(1);
    if (op == BattleEffectOp::Shield) event->set_shield_after(Whole(value));
    if (op == BattleEffectOp::Shield) CommitEquipmentShield(caster, receiver.actor_id(), shieldArtifacts, result);
    if (row.trigger_event() == 0 && op != BattleEffectOp::PursuitDamage) {
        for (const auto child : row.sub_buff()) {
            if (FindBuffIndex(receiver, instance) < 0) break;
            ApplyConfiguredEffect(caster, receiver, child, context, result, depth + 1);
        }
    }
}

uint64_t TurnBattleEngine::ResolveConfiguredHeal(BattleActorState& source, BattleActorState& target,
                                                 double amount, const EffectContext& context, TurnResultS2C& result) {
    double bonus = context.healingSnapshot ? 0 : context.equipmentHeal ? EquipmentOutgoingHeal(source) :
        ConfiguredModifier(source, BattleModifier::OutgoingHeal);
    const double received = std::max(0.0, 1 + ConfiguredModifier(target, BattleModifier::IncomingHeal));
    std::vector<std::pair<int, uint64_t>> contributions;
    if (context.equipmentHeal && context.origin == BattleDamageOrigin::Direct &&
        context.primaryTargetId == target.actor_id() && context.healOrdinal == 0) {
        for (const int index : AvailableArtifacts(source, 4)) {
            const auto& artifact = source.equipment_effects().artifacts(index);
            if (static_cast<long double>(target.attributes().health()) * 10000 >
                static_cast<long double>(target.max_health()) * artifact.health_threshold_bps()) continue;
            const uint64_t before = LandedHeal(target, amount * std::max(0.0, 1 + bonus) * received);
            bonus += artifact.bonus_bps() / 10000.0;
            const uint64_t after = LandedHeal(target, amount * std::max(0.0, 1 + bonus) * received);
            if (after > before) contributions.emplace_back(index, after - before);
        }
    }
    const uint64_t healed = ApplyHeal(target, supportrules::ApplyBonus(amount, bonus) * received);
    for (const auto& [index, gain] : contributions) UseArtifact(source, index, target.actor_id(), gain, result);
    auto* event = AppendEvent(result, BATTLE_EVENT_HEAL, source.actor_id(), target.actor_id());
    event->set_skill_table_id(context.skillId); event->set_value(healed);
    event->set_target_health_after(target.attributes().health()); return healed;
}

uint64_t TurnBattleEngine::ResolveConfiguredDamage(BattleActorState& source, BattleActorState& target,
                                                   double damage, uint32_t damageType,
                                                   const EffectContext& context, TurnResultS2C& result,
                                                   eBattleHitKind hitKind) {
    if (!IsActorActive(target) || HasConfiguredState(target, BattleStateKind::Freeze)) return 0;
    const bool direct = IsDirect(context.origin);
    if (direct) {
        const double dodge = std::clamp(ConfiguredModifier(target, BattleModifier::Evasion), 0.0, 0.75);
        if (dodge > 0 && Rand01() < dodge) {
            auto* miss = AppendEvent(result, BATTLE_EVENT_MISS, source.actor_id(), target.actor_id());
            miss->set_skill_table_id(context.skillId); miss->set_target_health_after(target.attributes().health());
            if (context.origin == BattleDamageOrigin::Direct)
                TriggerConfiguredBuffs(target, source, BattleTrigger::AfterDodge, damageType, context, result);
            return 0;
        }
        damage *= std::max(0.0, 1 + ConfiguredModifier(source, BattleModifier::OutgoingDamage));
        damage = supportrules::ApplyBonus(damage, petcultivation::DamageBonus(source, damageType, context.origin, hitKind));
        damage *= std::max(0.0, 1 + ConfiguredModifier(target, BattleModifier::IncomingDamage));
        if (damageType == combatdamage::kMagicDamage)
            damage *= std::max(0.0, 1 - ConfiguredModifier(target, BattleModifier::MagicReduction));
        if (hitKind == BATTLE_HIT_COMBO || context.hitOrdinal > 0)
            damage *= std::max(0.0, 1 - ConfiguredModifier(target, BattleModifier::LaterHitReduction));
        std::vector<uint64_t> consume;
        for (const auto& entry : source.buffs()) {
            const auto* row = dataProvider->FindBuff(entry.buff_table_id());
            if (row == nullptr || row->battle_op() != static_cast<uint32_t>(BattleEffectOp::Modifier)) continue;
            if (row->modifier_stat() == static_cast<uint32_t>(BattleModifier::AdaptiveDamage) &&
                entry.chosen_stat() == (damageType == combatdamage::kPhysicalDamage ? 1u : 2u)) damage *= 1 + row->ratio();
            if (row->modifier_stat() == static_cast<uint32_t>(BattleModifier::NextDirectDamage) &&
                context.origin == BattleDamageOrigin::Direct) {
                const auto* skill = dataProvider->FindSkill(context.skillId);
                if (skill == nullptr || (source.actor_type() == BATTLE_ACTOR_TYPE_PET ? skill->pet_max_targets() : skill->max_targets()) <= 1) {
                    damage *= 1 + row->ratio(); consume.push_back(entry.buff_id());
                }
            }
        }
        for (const auto id : consume) RemoveConfiguredBuff(source, id, result);
    }
    // 护盾处理前不能按当前生命截断:残血不应把一记致死重击缩成可被小盾全吃的伤害。
    if (ActorHasBuffOfType(target, kBuffTypeInvincibility)) damage = 0;
    if (target.is_defending() && context.origin != BattleDamageOrigin::Reflect && context.origin != BattleDamageOrigin::Redirect) damage *= 0.5;
    uint64_t remaining = combatdamage::DamageToHealth(damage, UINT64_MAX);
    uint64_t absorbed = 0;
    std::vector<std::pair<uint32_t, uint64_t>> defenses;
    if (direct) for (const auto& entry : target.buffs()) {
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && MatchesDamage(*row, damageType)) defenses.emplace_back(row->effect_priority(), entry.buff_id());
    }
    std::sort(defenses.begin(), defenses.end());
    std::vector<uint64_t> originalShields;
    for (const auto& [priority, id] : defenses) {
        (void)priority;
        const int index = FindBuffIndex(target, id); if (index < 0) continue;
        const auto* row = dataProvider->FindBuff(target.buffs(index).buff_table_id());
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Shield) && row->hit_limit() == 0)
            originalShields.push_back(id);
    }
    uint64_t shieldDamage = remaining;
    // 先完成分担，再进入本人吸收链；法宝破盾基数锁定在这两个阶段之间。
    for (uint32_t defensePhase = 0; defensePhase < 2; ++defensePhase) {
    if (defensePhase == 1) shieldDamage = remaining;
    for (const auto& [priority, id] : defenses) {
        (void)priority; const int index = FindBuffIndex(target, id); if (index < 0 || remaining == 0) continue;
        const auto saved = target.buffs(index); const auto* row = dataProvider->FindBuff(saved.buff_table_id());
        if (row == nullptr) continue;
        const auto op = static_cast<BattleEffectOp>(row->battle_op());
        if ((op == BattleEffectOp::ShareDamage) != (defensePhase == 0)) continue;
        if (op == BattleEffectOp::ShareDamage && context.origin != BattleDamageOrigin::Redirect) {
            const auto* attackSkill = dataProvider->FindSkill(context.skillId);
            if (attackSkill != nullptr && (source.actor_type() == BATTLE_ACTOR_TYPE_PET ?
                attackSkill->pet_max_targets() : attackSkill->max_targets()) > 1) continue;
            auto* protector = FindActor(saved.caster_id());
            if (protector == nullptr || protector == &target || !IsActorActive(*protector) ||
                HasConfiguredState(*protector, BattleStateKind::Freeze)) continue;
            const uint64_t budget = row->secondary_ratio() > 0 ? classskill::FloorRatio(protector->max_health(), row->secondary_ratio()) : UINT64_MAX;
            const auto used = protector->round_effect_usage().find(saved.source_skill_id());
            const uint64_t spent = used == protector->round_effect_usage().end() ? 0 : used->second;
            const uint64_t share = std::min({remaining, classskill::FloorRatio(remaining, std::clamp(row->ratio(), 0.0, 0.8)),
                budget > spent ? budget - spent : 0});
            (*protector->mutable_round_effect_usage())[saved.source_skill_id()] = AddSafe(spent, share);
            remaining -= share;
            EffectContext redirected = context; redirected.origin = BattleDamageOrigin::Redirect;
            redirected.critical = false;
            ResolveConfiguredDamage(source, *protector, static_cast<double>(share), damageType, redirected, result);
        } else if (op == BattleEffectOp::ManaShield) {
            const double conversion = row->secondary_ratio() > 0 ? row->secondary_ratio() : 1.0;
            const uint64_t cover = std::min(classskill::FloorRatio(remaining, std::clamp(row->ratio(), 0.0, 1.0)),
                Whole(target.attributes().mana() / conversion));
            const uint64_t cost = std::min(target.attributes().mana(), classskill::CeilRatio(cover, conversion));
            target.mutable_attributes()->set_mana(target.attributes().mana() - cost); remaining -= cover; absorbed += cover;
            auto* mana = AppendEvent(result, BATTLE_EVENT_MANA, target.actor_id(), target.actor_id());
            mana->set_value(cost); mana->set_mana_delta(-static_cast<int64_t>(std::min<uint64_t>(cost, INT64_MAX)));
            mana->set_target_mana_after(target.attributes().mana());
        } else if (op == BattleEffectOp::Shield) {
            const uint64_t cover = std::min(remaining, saved.remaining_capacity());
            remaining -= cover; absorbed += cover;
            target.mutable_buffs(index)->set_remaining_capacity(saved.remaining_capacity() - cover);
            if (saved.remaining_hits() > 0) target.mutable_buffs(index)->set_remaining_hits(saved.remaining_hits() - 1);
            auto* shield = AppendEvent(result, BATTLE_EVENT_SHIELD, saved.caster_id(), target.actor_id());
            shield->set_buff_table_id(saved.buff_table_id()); shield->set_buff_instance_id(id);
            shield->set_absorbed(cover); shield->set_shield_after(saved.remaining_capacity() - cover);
            if (saved.remaining_capacity() == cover || saved.remaining_hits() == 1) RemoveConfiguredBuff(target, id, result);
        }
    }
    }
    remaining = EquipmentReduceDamage(source, target, remaining, context, result);
    const uint64_t dealt = std::min(target.attributes().health(), remaining);
    target.mutable_attributes()->set_health(target.attributes().health() - dealt);
    auto* event = AppendEvent(result, BATTLE_EVENT_DAMAGE, source.actor_id(), target.actor_id());
    event->set_skill_table_id(context.skillId); event->set_value(dealt); event->set_absorbed(absorbed);
    event->set_target_health_after(target.attributes().health()); event->set_target_mana_after(target.attributes().mana());
    event->set_hit_kind(hitKind); event->set_damage_origin(static_cast<uint32_t>(context.origin));
    event->set_instigator_id(context.instigatorId);
    event->set_is_critical(context.critical);
    if (dealt > 0) {
        std::vector<uint64_t> sleeping;
        for (const auto& entry : target.buffs()) {
            const auto* row = dataProvider->FindBuff(entry.buff_table_id());
            if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Control) &&
                row->state_kind() == static_cast<uint32_t>(BattleStateKind::Sleep)) sleeping.push_back(entry.buff_id());
        }
        for (const auto id : sleeping) RemoveConfiguredBuff(target, id, result);
    }
    if (target.attributes().health() == 0) HandleDeath(target, source.actor_id(), result);
    EquipmentBreakShields(source, target, shieldDamage, damageType, context, originalShields, result);
    EffectContext triggered = context; triggered.actualDamage = dealt;
    if (dealt > 0 && context.origin == BattleDamageOrigin::Direct && IsActorActive(target))
        TriggerConfiguredBuffs(target, source, BattleTrigger::AfterTake, damageType, triggered, result);
    if (dealt > 0 && context.origin == BattleDamageOrigin::Periodic && IsActorActive(source))
        TriggerConfiguredBuffs(source, target, BattleTrigger::AfterPeriodic, damageType, triggered, result);
    return dealt;
}

void TurnBattleEngine::TriggerConfiguredBuffs(BattleActorState& owner, BattleActorState& other,
                                               BattleTrigger trigger, uint32_t damageType,
                                               const EffectContext& context, TurnResultS2C& result) {
    if (!IsActorActive(owner) || (context.origin != BattleDamageOrigin::Direct &&
        trigger != BattleTrigger::AfterPeriodic)) return;
    std::vector<uint64_t> ids;
    for (const auto& entry : owner.buffs()) ids.push_back(entry.buff_id());
    for (const auto id : ids) {
        int index = FindBuffIndex(owner, id); if (index < 0 || !IsActorActive(owner)) continue;
        const auto saved = owner.buffs(index);
        const auto* definition = dataProvider->FindBuff(saved.buff_table_id());
        if (definition == nullptr || definition->battle_op() == 0 ||
            definition->trigger_event() != static_cast<uint32_t>(trigger) || !MatchesDamage(*definition, damageType)) continue;
        const BuffTable row = *definition;
        const bool periodicTag = trigger == BattleTrigger::AfterPeriodic && !row.required_tag().empty();
        if (periodicTag && !((row.required_tag() == "Poison" && context.periodicState == static_cast<uint32_t>(BattleStateKind::Poison)) ||
            (row.required_tag() == "Burn" && context.periodicState == static_cast<uint32_t>(BattleStateKind::Burn)))) continue;
        if (!periodicTag && !row.required_tag().empty() && !HasBuffTag(other, row.required_tag()) &&
            (row.second_required_tag().empty() || !HasBuffTag(other, row.second_required_tag()))) continue;
        if (row.per_round_limit() > 0 && saved.last_trigger_round() == roundIndex && saved.trigger_count() >= row.per_round_limit()) continue;
        if (row.trigger_chance() > 0 && row.trigger_chance() < 1 && Rand01() >= row.trigger_chance()) continue;
        owner.mutable_buffs(index)->set_trigger_count(saved.last_trigger_round() == roundIndex ? saved.trigger_count() + 1 : 1);
        owner.mutable_buffs(index)->set_last_trigger_round(roundIndex);
        EffectContext triggered = context; triggered.skillId = saved.source_skill_id();
        const auto* sourceSkill = dataProvider->FindSkill(saved.source_skill_id());
        triggered.rank = sourceSkill == nullptr ? 1 : ConfiguredRank(owner, *sourceSkill);
        const auto op = static_cast<BattleEffectOp>(row.battle_op());
        switch (op) {
        case BattleEffectOp::MirrorSkill: {
            if (HasConfiguredState(owner, BattleStateKind::Freeze) || HasConfiguredState(owner, BattleStateKind::Sleep) || !IsActorActive(other)) break;
            const auto* incoming = dataProvider->FindSkill(context.skillId);
            if (incoming != nullptr && incoming->battle_model() == 1) {
                BattleAction action; action.set_action_type(BATTLE_ACTION_SKILL); action.set_skill_table_id(incoming->id()); action.set_target_id(other.actor_id());
                ExecuteConfiguredSkill(owner, action, *incoming, result, BattleDamageOrigin::Mirror, row.ratio() > 0 ? row.ratio() : 1);
            } else {
                EffectContext mirror = context; mirror.origin = BattleDamageOrigin::Mirror;
                bool critical = false;
                const double base = incoming == nullptr ? kBasicAttackBaseDamage : dataProvider->GetSkillDamage(incoming->id(), owner.level());
                const uint64_t attack = damageType == combatdamage::kPhysicalDamage ? EffectivePhysicalAttack(owner) : owner.magic_attack();
                const double amount = CalculateFinalDamage(owner, other, base, attack, incoming == nullptr ? 1 : incoming->attack_multiplier(), damageType, critical);
                mirror.critical = critical;
                ResolveConfiguredDamage(owner, other, amount * (row.ratio() > 0 ? row.ratio() : 1), damageType, mirror, result, BATTLE_HIT_MIRROR);
            }
            break;
        }
        case BattleEffectOp::ReflectDamage: {
            EffectContext reflected = triggered; reflected.origin = BattleDamageOrigin::Reflect;
            reflected.critical = false;
            ResolveConfiguredDamage(owner, other, context.actualDamage * row.secondary_ratio(), damageType, reflected, result, BATTLE_HIT_REFLECT);
            break;
        }
        case BattleEffectOp::PursuitDamage: {
            if (!IsActorActive(other)) break;
            std::vector<uint64_t> targets{other.actor_id()};
            const uint32_t count = std::max(1u, owner.actor_type() == BATTLE_ACTOR_TYPE_PET ? row.pet_target_count() : row.target_count());
            auto candidates = CollectAliveEnemyIds(owner);
            candidates.erase(std::remove(candidates.begin(), candidates.end(), other.actor_id()), candidates.end());
            while (targets.size() < count && !candidates.empty()) {
                const size_t pick = candidates.size() == 1 ? 0 : static_cast<size_t>(RandIndex(candidates.size()));
                targets.push_back(candidates[pick]); candidates.erase(candidates.begin() + pick);
            }
            triggered.origin = BattleDamageOrigin::Pursuit;
            const uint32_t type = row.damage_filter() == 2 ? combatdamage::kMagicDamage : combatdamage::kPhysicalDamage;
            for (const auto targetId : targets) {
                auto* target = FindActor(targetId); if (target == nullptr || !IsActorActive(*target)) continue;
                bool critical = false;
                const double amount = CalculateFinalDamage(owner, *target, kBasicAttackBaseDamage,
                    type == combatdamage::kPhysicalDamage ? EffectivePhysicalAttack(owner) : owner.magic_attack(),
                    1, type, critical) * (row.ratio() > 0 ? row.ratio() : 1);
                triggered.critical = critical;
                ResolveConfiguredDamage(owner, *target, amount, type, triggered, result, BATTLE_HIT_PURSUIT);
            }
            break;
        }
        case BattleEffectOp::ConditionalHeal:
            ResolveConfiguredHeal(owner, owner, ConfiguredValue(owner, owner, row, triggered), triggered, result); break;
        case BattleEffectOp::ConditionalMana: case BattleEffectOp::DrainMana: {
            auto& recipient = op == BattleEffectOp::ConditionalMana ? owner : other;
            if (!IsActorActive(recipient)) break;
            const uint64_t amount = Whole(ConfiguredValue(owner, recipient, row, triggered));
            const uint64_t before = recipient.attributes().mana();
            const uint64_t delta = op == BattleEffectOp::DrainMana ? std::min(before, amount) :
                std::min(recipient.max_mana() > before ? recipient.max_mana() - before : 0, amount);
            recipient.mutable_attributes()->set_mana(op == BattleEffectOp::DrainMana ? before - delta : before + delta);
            auto* event = AppendEvent(result, BATTLE_EVENT_MANA, owner.actor_id(), recipient.actor_id());
            event->set_value(delta); event->set_target_mana_after(recipient.attributes().mana());
            event->set_mana_delta((op == BattleEffectOp::DrainMana ? -1 : 1) * static_cast<int64_t>(std::min<uint64_t>(delta, INT64_MAX)));
            break;
        }
        case BattleEffectOp::ApplyChildren: case BattleEffectOp::EvasionCharge:
            for (const auto child : row.sub_buff()) ApplyConfiguredEffect(owner, owner, child, triggered, result, 1);
            for (const auto child : row.target_sub_buff()) ApplyConfiguredEffect(owner, other, child, triggered, result, 1);
            break;
        default: break;
        }
        index = FindBuffIndex(owner, id);
        if (index >= 0 && saved.remaining_hits() > 0) {
            if (saved.remaining_hits() == 1) RemoveConfiguredBuff(owner, id, result);
            else owner.mutable_buffs(index)->set_remaining_hits(saved.remaining_hits() - 1);
        }
    }
}

void TurnBattleEngine::InitializeConfiguredAbilities() {
    TurnResultS2C ignored;
    for (auto& actor : actors) {
        std::vector<uint32_t> ids;
        for (const auto& [id, rank] : actor.skill_ranks()) { (void)rank; ids.push_back(id); }
        std::sort(ids.begin(), ids.end());
        for (const auto id : ids) {
            const auto* row = dataProvider->FindSkill(id);
            if (row == nullptr || row->battle_model() != 1 || !ConfiguredSubjectAllowed(actor, *row)) continue;
            actor.add_runtime_skills(id);
            EffectContext context; context.skillId = id; context.rank = ConfiguredRank(actor, *row);
            auto targets = row->ability_kind() == 4 ? SelectConfiguredTargets(actor, *row,
                actor.actor_type() == BATTLE_ACTOR_TYPE_PET ? actor.owner_player_id() : actor.actor_id()) : std::vector<uint64_t>{actor.actor_id()};
            for (const auto targetId : targets) {
                auto* target = FindActor(targetId); if (target == nullptr) continue;
                for (const auto effect : row->init_effect()) ApplyConfiguredEffect(actor, *target, effect, context, ignored);
            }
        }
    }
}
void TurnBattleEngine::BeginConfiguredRound(TurnResultS2C& result) {
    for (auto& actor : actors) {
        actor.clear_round_effect_usage();
        for (auto& artifact : *actor.mutable_artifact_states()) {
            if (artifact.pending_round() != 0 && artifact.pending_round() < roundIndex) {
                artifact.clear_pending_round(); artifact.clear_pending_ailment_kind();
            }
        }
        for (int i = 0; i < actor.buffs_size(); ++i) {
            const auto saved = actor.buffs(i); const auto* row = dataProvider->FindBuff(saved.buff_table_id());
            if (row == nullptr || row->battle_op() != static_cast<uint32_t>(BattleEffectOp::GrowingAura) ||
                roundIndex < std::max(1u, row->start_round())) continue;
            const auto* caster = FindActor(saved.caster_id());
            if (row->require_living_caster() && (caster == nullptr || !IsActorActive(*caster))) continue;
            const uint32_t stacks = std::min(std::max(1u, row->stack_limit()), saved.stacks() + 1);
            actor.mutable_buffs(i)->set_stacks(stacks);
        }
    }
    (void)result;
}
void TurnBattleEngine::TickConfiguredBuffs(TurnResultS2C& result) {
    // 全场先伤害后复苏,保证插入顺序不会让同样的回生印出现不同存活裁决。
    for (uint32_t phase = 0; phase < 3; ++phase) for (auto& actor : actors) {
        if (actor.fled() || !HasConfiguredBuffs(actor)) continue;
        BeginEventGroup();
        std::vector<uint64_t> ids; for (const auto& entry : actor.buffs()) ids.push_back(entry.buff_id());
        for (const auto id : ids) {
            int index = FindBuffIndex(actor, id); if (index < 0) continue;
            const auto saved = actor.buffs(index); const auto* ptr = dataProvider->FindBuff(saved.buff_table_id());
            if (ptr == nullptr || ptr->battle_op() == 0 || saved.tick_round() > roundIndex) continue;
            const BuffTable row = *ptr; const auto op = static_cast<BattleEffectOp>(row.battle_op());
            auto* caster = FindActor(saved.caster_id());
            EffectContext context; context.skillId = saved.source_skill_id();
            if (phase == 0 && op == BattleEffectOp::PeriodicDamage && !actor.is_dead() && saved.remaining_ticks() > 0) {
                context.origin = BattleDamageOrigin::Periodic;
                context.periodicState = row.state_kind();
                ResolveConfiguredDamage(caster == nullptr ? actor : *caster, actor, static_cast<double>(saved.snapshot_value()),
                    row.damage_filter() == 1 ? combatdamage::kPhysicalDamage : combatdamage::kMagicDamage, context, result);
                index = FindBuffIndex(actor, id);
                if (index >= 0) actor.mutable_buffs(index)->set_remaining_ticks(saved.remaining_ticks() - 1);
            }
            if (phase == 1 && op == BattleEffectOp::ReviveMark && saved.remaining_ticks() > 0) {
                if (actor.is_dead()) {
                    actor.set_is_dead(false);
                    actor.mutable_attributes()->set_health(std::max<uint64_t>(1, std::min(actor.max_health(),
                        classskill::FloorRatio(actor.max_health(), row.secondary_ratio() > 0 ? row.secondary_ratio() : 0.2))));
                    auto* revive = AppendEvent(result, BATTLE_EVENT_REVIVE, saved.caster_id(), actor.actor_id());
                    revive->set_buff_instance_id(id); revive->set_buff_table_id(saved.buff_table_id());
                    revive->set_target_health_after(actor.attributes().health());
                    RemoveConfiguredBuff(actor, id, result);
                } else {
                    context.healingSnapshot = saved.has_outgoing_heal_snapshot();
                    ResolveConfiguredHeal(caster == nullptr ? actor : *caster, actor,
                        saved.has_outgoing_heal_snapshot() ? saved.outgoing_heal_snapshot() : static_cast<double>(saved.snapshot_value()), context, result);
                    index = FindBuffIndex(actor, id);
                    if (index >= 0) actor.mutable_buffs(index)->set_remaining_ticks(saved.remaining_ticks() - 1);
                }
            }
            if (phase == 2) {
                index = FindBuffIndex(actor, id); if (index < 0) continue;
                const auto remaining = actor.buffs(index);
                if ((op == BattleEffectOp::PeriodicDamage || op == BattleEffectOp::ReviveMark) && remaining.remaining_ticks() == 0) {
                    RemoveConfiguredBuff(actor, id, result); continue;
                }
                if (row.duration_rounds() > 0 && op != BattleEffectOp::PeriodicDamage && op != BattleEffectOp::ReviveMark) {
                    if (remaining.remain_rounds() <= 1) RemoveConfiguredBuff(actor, id, result);
                    else actor.mutable_buffs(index)->set_remain_rounds(remaining.remain_rounds() - 1);
                }
            }
        }
    }
}
void TurnBattleEngine::RemoveConfiguredBuff(BattleActorState& actor, uint64_t id, TurnResultS2C& result) {
    const int index = FindBuffIndex(actor, id); if (index < 0) return;
    const auto saved = actor.buffs(index); const auto* row = dataProvider->FindBuff(saved.buff_table_id());
    if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::TemporaryHealth)) {
        actor.set_max_health(actor.max_health() > saved.secondary_value() ? actor.max_health() - saved.secondary_value() : 1);
        actor.mutable_attributes()->set_health(std::min(actor.max_health(), actor.attributes().health()));
        auto* event = AppendEvent(result, BATTLE_EVENT_MAX_HEALTH, saved.caster_id(), actor.actor_id());
        event->set_target_max_health_after(actor.max_health()); event->set_target_health_after(actor.attributes().health());
    }
    actor.mutable_buffs()->DeleteSubrange(index, 1);
    auto* event = AppendEvent(result, BATTLE_EVENT_BUFF_REMOVE, saved.caster_id(), actor.actor_id());
    event->set_buff_table_id(saved.buff_table_id()); event->set_buff_instance_id(id);
}
bool TurnBattleEngine::ExecuteConfiguredControl(BattleActorState& actor, const BattleAction& action,
                                                 TurnResultS2C& result) {
    if (HasConfiguredState(actor, BattleStateKind::Freeze) || HasConfiguredState(actor, BattleStateKind::Sleep)) return true;
    if (HasConfiguredState(actor, BattleStateKind::Confusion)) {
        std::vector<uint64_t> allies;
        for (const auto& target : actors) if (target.team_index() == actor.team_index() && target.actor_id() != actor.actor_id() && IsActorActive(target)) allies.push_back(target.actor_id());
        if (allies.empty()) { ExecuteDefend(actor, result); return true; }
        auto* target = FindActor(allies[allies.size() == 1 ? 0 : static_cast<size_t>(RandIndex(allies.size()))]);
        if (target == nullptr) return true;
        for (const auto skillId : actor.skill_table_ids()) {
            const auto* row = dataProvider->FindSkill(skillId);
            if (row == nullptr || row->battle_model() != 1 || (row->action_tags() & (kActionTagPhysical | kActionTagMagic)) == 0 ||
                SkillManaCost(actor, *row) > actor.attributes().mana()) continue;
            BattleAction eligibility; eligibility.set_action_type(BATTLE_ACTION_SKILL); eligibility.set_skill_table_id(skillId);
            if (CheckConfiguredSkill(actor, eligibility, *row) != kSuccess) continue;
            if ((row->action_tags() & kActionTagPhysical) && HasConfiguredState(actor, BattleStateKind::SealPhysical)) continue;
            if ((row->action_tags() & kActionTagMagic) && HasConfiguredState(actor, BattleStateKind::SealMagic)) continue;
            BattleAction confused; confused.set_action_type(BATTLE_ACTION_SKILL); confused.set_skill_table_id(skillId); confused.set_target_id(target->actor_id());
            ExecuteConfiguredSkill(actor, confused, *row, result, BattleDamageOrigin::Confusion); return true;
        }
        if (HasConfiguredState(actor, BattleStateKind::SealPhysical)) { ExecuteDefend(actor, result); return true; }
        bool critical = false; EffectContext context; context.origin = BattleDamageOrigin::Confusion;
        AppendEvent(result, BATTLE_EVENT_ATTACK, actor.actor_id(), target->actor_id());
        const double amount = CalculateFinalDamage(actor, *target, kBasicAttackBaseDamage,
            EffectivePhysicalAttack(actor), 1, combatdamage::kPhysicalDamage, critical);
        context.critical = critical;
        ResolveConfiguredDamage(actor, *target, amount, combatdamage::kPhysicalDamage, context, result);
        return true;
    }
    return action.action_type() == BATTLE_ACTION_ATTACK && HasConfiguredState(actor, BattleStateKind::SealPhysical);
}
std::optional<BattleAction> TurnBattleEngine::SelectAutoConfiguredSkill(const BattleActorState& actor) const {
    for (const auto skillId : actor.skill_table_ids()) {
        const auto* row = dataProvider->FindSkill(skillId);
        if (row == nullptr || row->battle_model() != 1 || row->ability_kind() != 1) continue;
        for (const auto& target : actors) {
            if (!ConfiguredTargetAllowed(actor, target, *row)) continue;
            bool useful = row->target_team() == 1;
            for (const auto effectId : row->effect()) {
                const auto* effect = dataProvider->FindBuff(effectId); if (effect == nullptr) continue;
                const auto op = static_cast<BattleEffectOp>(effect->battle_op());
                if (op == BattleEffectOp::Heal) useful = target.attributes().health() * 1.0 < target.max_health() * 0.7;
                else if (op == BattleEffectOp::RestoreMana) useful = target.attributes().mana() * 1.0 < target.max_mana() * 0.4;
                else if (Persistent(op)) {
                    bool present = false; for (const auto& entry : target.buffs()) if (entry.buff_table_id() == effectId) present = true;
                    useful = useful || !present;
                }
            }
            if (!useful) continue;
            BattleAction action; action.set_action_type(BATTLE_ACTION_SKILL); action.set_skill_table_id(skillId); action.set_target_id(target.actor_id());
            if (CheckActionPrerequisites(actor, action) == kSuccess) return action;
        }
    }
    return std::nullopt;
}
uint32_t TurnBattleEngine::ValidateActorAction(uint64_t ownerPlayerId, uint64_t actorId, const BattleAction& action) const {
    const auto* actor = FindActor(actorId);
    if (actor == nullptr || (actor->actor_id() != ownerPlayerId &&
        (actor->actor_type() != BATTLE_ACTOR_TYPE_PET || actor->owner_player_id() != ownerPlayerId))) return kInvalidParameter;
    return ValidateAction(actorId, action);
}
bool TurnBattleEngine::SubmitActorAction(uint64_t ownerPlayerId, uint64_t actorId, const BattleAction& action) {
    if (!initialized || outcome != BATTLE_OUTCOME_ONGOING || ValidateActorAction(ownerPlayerId, actorId, action) != kSuccess) return false;
    pendingActions[actorId] = action;
    return AllPlayersReady();
}
}  // namespace turnbattle

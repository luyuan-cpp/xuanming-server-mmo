#include "system/turn_battle_engine.h"
#include "system/combat_damage_rules.h"
#include "system/equipment_battle_rules.h"
#include "system/season_rules.h"
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
    case BattleEffectOp::DrainMana: case BattleEffectOp::RetreatPet:
    case BattleEffectOp::CopyTargetSkill: return false;
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
        if (effect == nullptr) continue;
        const auto op = static_cast<BattleEffectOp>(effect->battle_op());
        if (op == BattleEffectOp::RetreatPet && target.actor_type() != BATTLE_ACTOR_TYPE_PET) return false;
        if (op == BattleEffectOp::CopyTargetSkill && target.is_boss()) return false;
    }
    return true;
}
bool TurnBattleEngine::HasValidConfiguredTarget(const BattleActorState& caster, const SkillTable& row) const {
    for (const auto& candidate : actors) {
        if (ConfiguredTargetAllowed(caster, candidate, row)) return true;
    }
    return false;
}
bool TurnBattleEngine::IsSkillCopyableFor(const BattleActorState& caster, const SkillTable& row) const {
    if (row.battle_model() != 1 || row.ability_kind() != 1) return false;
    if (IsSkillBanned(row.id())) return false;
    if (row.forbid_copy() != 0 || row.health_cost_mode() != 0 || row.max_battle_uses() != 0) return false;
    for (const auto effectId : row.effect()) {
        const auto* effect = dataProvider->FindBuff(effectId);
        if (effect == nullptr) continue;
        const auto op = static_cast<BattleEffectOp>(effect->battle_op());
        if (op == BattleEffectOp::CopyTargetSkill) return false;
        // 依赖已付气血/法力的转换技在零耗蓝镜像中无有效基数，结构上禁止复制。
        if (effect->value_basis() == 5 || effect->value_basis() == 6) return false;
    }
    return HasValidConfiguredTarget(caster, row);
}
bool TurnBattleEngine::HasCopyableSkillFor(const BattleActorState& caster, const BattleActorState& target) const {
    if (target.is_boss()) return false;
    for (const auto skillId : target.skill_table_ids()) {
        const auto* candidate = dataProvider->FindSkill(skillId);
        if (candidate != nullptr && IsSkillCopyableFor(caster, *candidate)) return true;
    }
    return false;
}
std::optional<uint32_t> TurnBattleEngine::SelectCopiedSkillId(const BattleActorState& caster,
                                                              const BattleActorState& target) {
    // 优先复制目标本回合选定的可复制主动技能（不依赖双方出手先后）。
    if (const auto it = pendingActions.find(target.actor_id());
        it != pendingActions.end() && it->second.action_type() == BATTLE_ACTION_SKILL) {
        const auto* planned = dataProvider->FindSkill(it->second.skill_table_id());
        if (planned != nullptr && IsSkillCopyableFor(caster, *planned)) return planned->id();
    }
    // 方案 A 兜底：目标本回合未选可复制技能时，从其已学可复制主动技能池按 ID 排序去重后确定性抽取。
    std::vector<uint32_t> candidates;
    for (const auto skillId : target.skill_table_ids()) {
        const auto* candidate = dataProvider->FindSkill(skillId);
        if (candidate != nullptr && IsSkillCopyableFor(caster, *candidate)) candidates.push_back(skillId);
    }
    std::sort(candidates.begin(), candidates.end());
    candidates.erase(std::unique(candidates.begin(), candidates.end()), candidates.end());
    if (candidates.empty()) return std::nullopt;
    const size_t pick = candidates.size() == 1 ? 0 : static_cast<size_t>(RandIndex(candidates.size()));
    return candidates[pick];
}
uint32_t TurnBattleEngine::CheckConfiguredSkill(const BattleActorState& actor, const BattleAction& action,
                                                const SkillTable& row) const {
    if (IsSkillBanned(row.id())) return kSkillBannedInBattle;
    if (!ConfiguredSubjectAllowed(actor, row) || ConfiguredRank(actor, row) == 0 || row.ability_kind() != 1)
        return kSkillCannotBeCastInCurrentState;
    if (const auto blocked = CheckBuff(actor, row); blocked != kSuccess) return blocked;
    if (HasConfiguredState(actor, BattleStateKind::SealActive) ||
        ((HasConfiguredState(actor, BattleStateKind::SealPhysical) || IsPhysicalForbidden(actor)) &&
         (row.action_tags() & kActionTagPhysical)) ||
        ((HasConfiguredState(actor, BattleStateKind::SealMagic) || IsMagicForbidden(actor)) &&
         (row.action_tags() & kActionTagMagic)))
        return kSkillCannotBeCastInCurrentState;
    if (actor.charging_skill_id() != 0) {
        for (const auto effectId : row.effect()) {
            const auto* effect = dataProvider->FindBuff(effectId);
            if (effect != nullptr && effect->battle_op() == static_cast<uint32_t>(BattleEffectOp::PrepareCast)) {
                return kSkillCannotBeCastInCurrentState;
            }
        }
    }
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
    const auto policy = static_cast<SecondaryTargetPolicy>(row.secondary_target_policy());
    if (policy == SecondaryTargetPolicy::Random) {
        while (targets.size() < count && !candidates.empty()) {
            const size_t index = candidates.size() == 1 ? 0 : static_cast<size_t>(RandIndex(candidates.size()));
            targets.push_back(candidates[index]);
            candidates.erase(candidates.begin() + index);
        }
        return targets;
    }

    // 策略模式副目标排序：保留主目标，其余名额按策略最优值选取；并列集合内用确定性 RNG 打破平局。
    const auto compareBetter = [this, policy](const BattleActorState& lhs, const BattleActorState& rhs) -> int {
        switch (policy) {
        case SecondaryTargetPolicy::HighestPhysicalAttack: {
            const uint64_t a = EffectivePhysicalAttack(lhs);
            const uint64_t b = EffectivePhysicalAttack(rhs);
            return a > b ? 1 : a < b ? -1 : 0;
        }
        case SecondaryTargetPolicy::HighestMagicAttack: {
            const uint64_t a = EffectiveMagicAttack(lhs);
            const uint64_t b = EffectiveMagicAttack(rhs);
            return a > b ? 1 : a < b ? -1 : 0;
        }
        case SecondaryTargetPolicy::LowestHealthValue: {
            const uint64_t a = lhs.attributes().health();
            const uint64_t b = rhs.attributes().health();
            return a < b ? 1 : a > b ? -1 : 0;
        }
        case SecondaryTargetPolicy::LowestHealthRatio: {
            const long double a = lhs.max_health() > 0
                ? static_cast<long double>(lhs.attributes().health()) / static_cast<long double>(lhs.max_health())
                : 1.0L;
            const long double b = rhs.max_health() > 0
                ? static_cast<long double>(rhs.attributes().health()) / static_cast<long double>(rhs.max_health())
                : 1.0L;
            if (std::abs(a - b) <= 1e-12L) return 0;
            return a < b ? 1 : -1;
        }
        case SecondaryTargetPolicy::LowestPhysicalDefense: {
            const double a = EffectiveMitigationScore(lhs, false);
            const double b = EffectiveMitigationScore(rhs, false);
            if (std::abs(a - b) <= 1e-9) return 0;
            return a < b ? 1 : -1;
        }
        case SecondaryTargetPolicy::LowestMagicDefense: {
            const double a = EffectiveMitigationScore(lhs, true);
            const double b = EffectiveMitigationScore(rhs, true);
            if (std::abs(a - b) <= 1e-9) return 0;
            return a < b ? 1 : -1;
        }
        case SecondaryTargetPolicy::LowestRoundSpeed: {
            const auto itA = roundStartSpeeds.find(lhs.actor_id());
            const auto itB = roundStartSpeeds.find(rhs.actor_id());
            const uint64_t a = itA != roundStartSpeeds.end() ? itA->second : EffectiveSpeed(lhs);
            const uint64_t b = itB != roundStartSpeeds.end() ? itB->second : EffectiveSpeed(rhs);
            return a < b ? 1 : a > b ? -1 : 0;
        }
        default:
            return 0;
        }
    };

    while (targets.size() < count && !candidates.empty()) {
        std::vector<size_t> bestIndices;
        for (size_t i = 0; i < candidates.size(); ++i) {
            const auto* candidate = FindActor(candidates[i]);
            if (candidate == nullptr) continue;
            if (bestIndices.empty()) {
                bestIndices.push_back(i);
                continue;
            }
            const auto* currentBest = FindActor(candidates[bestIndices.front()]);
            if (currentBest == nullptr) {
                bestIndices.assign(1, i);
                continue;
            }
            const int cmp = compareBetter(*candidate, *currentBest);
            if (cmp > 0) {
                bestIndices.assign(1, i);
            } else if (cmp == 0) {
                bestIndices.push_back(i);
            }
        }
        if (bestIndices.empty()) break;
        const size_t chosenPos = bestIndices.size() == 1
            ? bestIndices.front()
            : bestIndices[static_cast<size_t>(RandIndex(bestIndices.size()))];
        targets.push_back(candidates[chosenPos]);
        candidates.erase(candidates.begin() + chosenPos);
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
                                               BattleDamageOrigin origin, double scale,
                                               uint32_t rankOverride) {
    const bool normal = origin == BattleDamageOrigin::Direct;
    if (normal && CheckActionPrerequisites(actor, action) != kSuccess) return;
    auto targets = SelectConfiguredTargets(actor, row, action.target_id());
    if (origin == BattleDamageOrigin::Confusion) targets = {action.target_id()};
    if (targets.empty()) return;
    if (normal && actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && row.season_id() > 0) {
        antiControlOrSeasonSkillPlayers.insert(actor.actor_id());
        castSeasonSkillPlayers.insert(actor.actor_id());
    }
    EffectContext context;
    context.skillId = row.id();
    context.rank = rankOverride > 0 ? rankOverride : ConfiguredRank(actor, row);
    if (origin == BattleDamageOrigin::Mirror) context.rank = std::max(1u, context.rank);
    context.origin = origin; context.scale = scale;
    context.primaryTargetId = targets.front();
    const uint32_t subjectMaxTargets = actor.actor_type() == BATTLE_ACTOR_TYPE_PET
        ? (row.pet_max_targets() > 0 ? row.pet_max_targets() : row.max_targets())
        : row.max_targets();
    const bool explicitPrimarySelected = action.target_id() != 0 && targets.front() == action.target_id();
    context.primaryFocusBonus = IsCompetitivePvpBattle() && normal &&
        actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER &&
        subjectMaxTargets > 1 && explicitPrimarySelected &&
        targets.size() < subjectMaxTargets;
    context.isMultiTargetSkill =
        (actor.actor_type() == BATTLE_ACTOR_TYPE_PET ? row.pet_max_targets() : row.max_targets()) > 1u;
    auto* event = AppendEvent(result, BATTLE_EVENT_SKILL, actor.actor_id(), targets.front());
    event->set_skill_table_id(row.id());
    event->set_hit_kind(origin == BattleDamageOrigin::Mirror ? BATTLE_HIT_MIRROR : BATTLE_HIT_NORMAL);
    if (normal || origin == BattleDamageOrigin::Confusion) {
        const bool consumeBreathResonance = DefendManaDiscountResonancePct(actor, row.id()) > 0u;
        context.paidMana = SkillManaCost(actor, row);
        ConsumeSkillMana(actor, row, row.id(), result);
        if (consumeBreathResonance) {
            auto& rt = seasonSealRuntimeByActor[actor.actor_id()];
            rt.defendResonanceReadyRound = 0u;
            const uint64_t fullCost = SkillManaCost(actor, row);
            const uint64_t saved = fullCost > context.paidMana ? fullCost - context.paidMana : 0u;
            if (saved > 0u) {
                rt.contributionValue = AddSafe(rt.contributionValue, saved);
                EmitSeasonResonanceEvent(actor, actor.actor_id(),
                                         seasonrules::kResonanceEventBreathHalfMana, saved, result);
            }
        }
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
    bool castSupportEffect = false;
    bool nextRoundBoostActive = false;
    if (normal && actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER &&
        actor.equipment_effects().has_season_seal()) {
        const auto& seal = actor.equipment_effects().season_seal();
        if (seal.resonance_skill_id() == row.id() && seal.resonance_param() > 0u) {
            auto& rt = seasonSealRuntimeByActor[actor.actor_id()];
            if ((seal.resonance_kind() == seasonrules::kResonanceGuardShieldBoost ||
                 seal.resonance_kind() == seasonrules::kResonanceGuardHealBoost) &&
                !rt.guardResonanceConsumed && rt.guardResonanceReadyRound == roundIndex) {
                rt.guardResonanceConsumed = true;
                rt.guardResonanceReadyRound = 0u;
                nextRoundBoostActive = true;
                activeNextRoundBoostCasterId = actor.actor_id();
                activeNextRoundBoostKind = seal.resonance_kind();
                activeNextRoundBoostPct = seal.resonance_param();
                activeNextRoundBoostExtra = 0u;
            } else if (seal.resonance_kind() == seasonrules::kResonanceDefendShieldBoost &&
                       rt.defendResonanceReadyRound == roundIndex) {
                rt.defendResonanceReadyRound = 0u;
                nextRoundBoostActive = true;
                activeNextRoundBoostCasterId = actor.actor_id();
                activeNextRoundBoostKind = seal.resonance_kind();
                activeNextRoundBoostPct = seal.resonance_param();
                activeNextRoundBoostExtra = 0u;
            }
        }
    }
    for (size_t i = 0; i < targets.size(); ++i) {
        auto* target = FindActor(targets[i]);
        if (target == nullptr || target->fled()) continue;
        currentHitIndex = static_cast<uint32_t>(i);
        // 同一目标的条件在消费持续状态之前求定,毒火联动不能因第一项消费而丢失第二项。
        std::vector<uint32_t> effects;
        for (const auto effectId : row.effect()) {
            const auto* effect = dataProvider->FindBuff(effectId);
            if (effect == nullptr) continue;
            if (i > 0 && effect->effect_target() == 1) continue;
            if (effect->battle_op() == static_cast<uint32_t>(BattleEffectOp::ConsumePeriodic) &&
                (!HasBuffTag(*target, effect->required_tag()) || !HasBuffTag(*target, effect->second_required_tag()))) continue;
            effects.push_back(effectId);
        }
        auto targetContext = context;
        for (const auto effect : effects) {
            ApplyConfiguredEffect(actor, *target, effect, targetContext, result);
            const auto* definition = dataProvider->FindBuff(effect);
            if (definition != nullptr && definition->battle_op() == static_cast<uint32_t>(BattleEffectOp::Damage)) ++targetContext.hitOrdinal;
            if (definition != nullptr && definition->battle_op() == static_cast<uint32_t>(BattleEffectOp::Heal)) {
                ++targetContext.healOrdinal;
                castSupportEffect = true;
            }
            if (definition != nullptr && definition->battle_op() == static_cast<uint32_t>(BattleEffectOp::Shield)) {
                ++targetContext.shieldOrdinal;
                castSupportEffect = true;
            }
            if (definition != nullptr && definition->battle_op() == static_cast<uint32_t>(BattleEffectOp::ReviveMark)) {
                castSupportEffect = true;
            }
        }
        pendingMysticExtendCasterId = 0u;
        pendingMysticExtendTargetId = 0u;
        pendingMysticExtendSkillId = 0u;
        pendingMysticExtendBonusRounds = 0u;
    }
    if (nextRoundBoostActive && activeNextRoundBoostCasterId == actor.actor_id()) {
        const uint64_t extra = activeNextRoundBoostExtra;
        const uint32_t boostKind = activeNextRoundBoostKind;
        activeNextRoundBoostCasterId = 0u;
        activeNextRoundBoostKind = 0u;
        activeNextRoundBoostPct = 0u;
        activeNextRoundBoostExtra = 0u;
        if (extra > 0u && boostKind > 0u) {
            auto& rt = seasonSealRuntimeByActor[actor.actor_id()];
            rt.contributionValue = AddSafe(rt.contributionValue, extra);
            EmitSeasonResonanceEvent(actor, actor.actor_id(),
                                     seasonrules::kResonanceEventBase + boostKind, extra, result);
        }
    }
    currentHitIndex = 0;
    bool hitDirectly = false;
    bool landedDirectStrike = false;
    for (int i = effectsBegin; i < result.events_size(); ++i) {
        const auto& damage = result.events(i);
        if (damage.event_type() == BATTLE_EVENT_DAMAGE && damage.source_id() == actor.actor_id() &&
            damage.skill_table_id() == row.id() && damage.damage_origin() == 0) {
            if (damage.value() > 0) hitDirectly = true;
            if (damage.value() > 0 || damage.absorbed() > 0) landedDirectStrike = true;
        }
    }
    if (normal && landedDirectStrike && IsActorActive(actor)) {
        ConsumeOneShotAttackModifiers(actor, row.damage_type(), result);
    }
    if (normal && hitDirectly && IsActorActive(actor)) {
        auto* primary = FindActor(targets.front());
        if (primary != nullptr) TriggerConfiguredBuffs(actor, *primary, BattleTrigger::AfterSkill,
            row.damage_type(), context, result);
    }
    if (normal && castSupportEffect && IsActorActive(actor)) {
        TriggerConfiguredBuffs(actor, actor, BattleTrigger::AfterSupportCast, 0, context, result);
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
    if (context.scale != 1.0) value *= context.scale;
    if (op == BattleEffectOp::PeriodicDamage && row.state_kind() == static_cast<uint32_t>(BattleStateKind::Poison))
        value = supportrules::ApplyBonus(value, petcultivation::PoisonBonus(caster));
    // 持续治疗的覆盖强度与每跳快照采用同一完整施放方值，避免旧印因施法者加成不同被弱覆盖。
    if (op == BattleEffectOp::ReviveMark) value *= std::max(0.0, 1 + EquipmentOutgoingHeal(caster));
    std::vector<int> shieldArtifacts;
    uint64_t seasonSealShieldGain = 0u;
    // 人物装备/法宝/赛季印护盾加成仅作用于独立无次数容量盾(A40)；宝宝 504 护盾条目作用于全部有限数值护盾(含 B05/B06/O03)，不改次数与时长，法力盾(ManaShield)排除。
    if (op == BattleEffectOp::Shield && (row.hit_limit() == 0 || caster.actor_type() == BATTLE_ACTOR_TYPE_PET)) {
        value = EquipmentShield(caster, receiver, value, context, shieldArtifacts, &seasonSealShieldGain);
    }
    if (op == BattleEffectOp::Shield && ActiveTournamentLeyline() == TOURNAMENT_LEYLINE_WOOD) {
        value *= 110.0 / 100.0;
    }
    if (op == BattleEffectOp::Shield) {
        if (const uint32_t seasonShieldPct = seasonrules::ShieldOverloadBonusPct(activeSeasonWeeklyAffixId);
            seasonShieldPct > 0u) {
            value *= (100.0 + seasonShieldPct) / 100.0;
        }
        if (const uint32_t wuxingShieldPct = WuxingHealShieldBonusPct(caster); wuxingShieldPct > 0) {
            value *= (100.0 + wuxingShieldPct) / 100.0;
        }
    }
    if (op == BattleEffectOp::Shield && EscalationStacks() > 0) {
        value *= static_cast<double>(EscalationSupportPct()) / 100.0;
    }
    uint64_t nextRoundShieldGain = 0u;
    if (op == BattleEffectOp::Shield && context.origin == BattleDamageOrigin::Direct &&
        activeNextRoundBoostCasterId == caster.actor_id() &&
        (activeNextRoundBoostKind == seasonrules::kResonanceGuardShieldBoost ||
         activeNextRoundBoostKind == seasonrules::kResonanceDefendShieldBoost) &&
        activeNextRoundBoostPct > 0u) {
        const uint64_t beforeShield = Whole(value);
        value *= (100.0 + activeNextRoundBoostPct) / 100.0;
        const uint64_t afterShield = Whole(value);
        if (afterShield > beforeShield) {
            nextRoundShieldGain = afterShield - beforeShield;
        }
    }
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
            if (context.primaryFocusBonus && receiver.actor_id() == context.primaryTargetId) {
                amount *= static_cast<double>(kPvpPrimaryFocusNum) / static_cast<double>(kPvpPrimaryFocusDen);
                damageContext.isPrimaryFocus = true;
            }
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
            for (size_t i = 0; i < limit; ++i) {
                if (op == BattleEffectOp::DispelBuff && caster.team_index() == 0 &&
                    receiver.team_index() == 1 && receiver.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
                    const int buffIdx = FindBuffIndex(receiver, ids[i]);
                    if (buffIdx >= 0) {
                        const uint32_t targetBuffId = createRequest.trial_context().guard_stance_buff_id();
                        if (targetBuffId == 0 || receiver.buffs(buffIdx).buff_table_id() == targetBuffId) {
                            guardStanceDispelled = true;
                        }
                    }
                }
                RemoveConfiguredBuff(receiver, ids[i], result);
            }
            bool clearedSunder = false;
            if (op == BattleEffectOp::DispelDebuff) {
                if (sunderUntilRound.erase(receiver.actor_id()) > 0) {
                    RecordEffectiveContribution(caster);
                    clearedSunder = true;
                }
            }
            if (op == BattleEffectOp::DispelBuff && caster.team_index() != receiver.team_index()) {
                TryDispelBossStance(caster, receiver, result);
            }
            if (limit > 0) {
                RecordEffectiveContribution(caster);
            }
            if (limit > 0 || clearedSunder) {
                RecordDispelLanded(caster.actor_id(),
                                   static_cast<uint32_t>(limit) + (clearedSunder ? 1u : 0u));
                OnBossWeeklyAffixDispelTriggered(caster.actor_id(), result);
            }
            if (op == BattleEffectOp::DispelDebuff && limit > 0 &&
                caster.team_index() == receiver.team_index() &&
                caster.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
                antiControlOrSeasonSkillPlayers.insert(caster.actor_id());
            }
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
            if (success) {
                receiver.set_fled(true);
                receiver.set_withdraw_until_round(roundIndex + std::max(1u, row.duration_rounds()) + 1);
                ResetVolatileGrowingStacks(receiver);
            }
            break;
        }
        case BattleEffectOp::CopyTargetSkill: {
            if (context.origin == BattleDamageOrigin::Mirror) break;
            if (receiver.is_boss() || IsImmuneToBuff(receiver, row)) {
                auto* resisted = AppendEvent(result, BATTLE_EVENT_RESIST, caster.actor_id(), receiver.actor_id());
                resisted->set_buff_table_id(row.id());
                break;
            }
            const auto copiedSkillId = SelectCopiedSkillId(caster, receiver);
            if (!copiedSkillId.has_value()) {
                auto* resisted = AppendEvent(result, BATTLE_EVENT_RESIST, caster.actor_id(), receiver.actor_id());
                resisted->set_buff_table_id(row.id());
                break;
            }
            const auto* copiedSkill = dataProvider->FindSkill(*copiedSkillId);
            if (copiedSkill == nullptr) break;
            BattleAction copiedAction;
            copiedAction.set_action_type(BATTLE_ACTION_SKILL);
            copiedAction.set_skill_table_id(copiedSkill->id());
            if (copiedSkill->target_team() == 2 || copiedSkill->target_team() == 3) {
                // 复制到友方辅助/治疗/护盾或自身技能时，自动切回己方阵营，避免反手资敌。
                copiedAction.set_target_id(ConfiguredTargetAllowed(caster, caster, *copiedSkill) ? caster.actor_id() : 0);
            } else {
                copiedAction.set_target_id(ConfiguredTargetAllowed(caster, receiver, *copiedSkill) ? receiver.actor_id() : 0);
            }
            const double mirrorScale = row.ratio() > 0 ? row.ratio() : 1.0;
            ExecuteConfiguredSkill(caster, copiedAction, *copiedSkill, result,
                                   BattleDamageOrigin::Mirror, mirrorScale, context.rank);
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
    const bool hostileActionControl = hostile && op == BattleEffectOp::Control && row.state_kind() >= 1 && row.state_kind() <= 6;
    if (hostileActionControl) {
        if (IsControlBannedByCustomDuel()) {
            auto* resisted = AppendEvent(result, BATTLE_EVENT_RESIST, caster.actor_id(), receiver.actor_id());
            resisted->set_buff_table_id(buffId);
            return;
        }
        if (TryConsumeControlImmunity(caster, receiver, row, result)) {
            TriggerConfiguredBuffs(receiver, caster, BattleTrigger::AfterControlAttempt, 0, context, result);
            return;
        }
    }
    if (hostile && row.state_kind() >= 1 && row.state_kind() <= 7 &&
        (op == BattleEffectOp::Control || op == BattleEffectOp::PeriodicDamage)) {
        if (!EquipmentControlRoll(caster, receiver, row, context, result)) {
            if (hostileActionControl) {
                TriggerConfiguredBuffs(receiver, caster, BattleTrigger::AfterControlAttempt, 0, context, result);
            }
            return;
        }
    } else if (hostile && row.application_chance() > 0 && row.application_chance() < 1 && Rand01() >= row.application_chance()) {
        auto* resisted = AppendEvent(result, BATTLE_EVENT_RESIST, caster.actor_id(), receiver.actor_id());
        resisted->set_buff_table_id(buffId); return;
    }
    if (hostileActionControl) {
        TriggerConfiguredBuffs(receiver, caster, BattleTrigger::AfterControlAttempt, 0, context, result);
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
    uint32_t appliedRemainRounds = row.duration_rounds();
    uint32_t mysticResonanceExtendedRounds = 0u;
    if (hostile && op == BattleEffectOp::Modifier && appliedRemainRounds > 0u &&
        context.origin == BattleDamageOrigin::Direct &&
        pendingMysticExtendBonusRounds > 0u &&
        context.skillId == pendingMysticExtendSkillId &&
        caster.actor_id() == pendingMysticExtendCasterId &&
        receiver.actor_id() == pendingMysticExtendTargetId) {
        mysticResonanceExtendedRounds = pendingMysticExtendBonusRounds;
        appliedRemainRounds += mysticResonanceExtendedRounds;
        pendingMysticExtendCasterId = 0u;
        pendingMysticExtendTargetId = 0u;
        pendingMysticExtendSkillId = 0u;
        pendingMysticExtendBonusRounds = 0u;
    }
    added->set_remain_rounds(appliedRemainRounds); added->set_applied_round(roundIndex);
    added->set_tick_round(row.tick_on_apply() ? roundIndex : roundIndex + 1);
    added->set_extensions_used(extensions);
    if (op == BattleEffectOp::ReviveMark)
        added->set_outgoing_heal_snapshot(value);
    if (op == BattleEffectOp::Modifier && row.modifier_stat() == static_cast<uint32_t>(BattleModifier::AdaptiveDamage))
        added->set_chosen_stat(receiver.physical_attack() >= receiver.magic_attack() ? 1 : 2);
    if (op == BattleEffectOp::PrepareCast) {
        added->set_secondary_value(context.primaryTargetId);
        SyncChargingSkillId(receiver);
    }
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
    if (op == BattleEffectOp::Shield) {
        if (nextRoundShieldGain > 0u) {
            activeNextRoundBoostExtra = AddSafe(activeNextRoundBoostExtra, nextRoundShieldGain);
        }
        CommitEquipmentShield(caster, receiver.actor_id(), shieldArtifacts, seasonSealShieldGain, context.skillId, result);
    }
    if (mysticResonanceExtendedRounds > 0u) {
        EmitSeasonResonanceEvent(caster, receiver.actor_id(),
                                 seasonrules::kResonanceEventMysticExtendDebuff,
                                 mysticResonanceExtendedRounds, result);
    }
    if (row.trigger_event() == 0 && op != BattleEffectOp::PursuitDamage && op != BattleEffectOp::PrepareCast) {
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
    uint64_t seasonSealHealGain = 0u;
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
        if (SeasonSealAvailable(source, 4)) {
            const auto& seal = source.equipment_effects().season_seal();
            if (static_cast<long double>(target.attributes().health()) * 10000 <=
                static_cast<long double>(target.max_health()) * seal.health_threshold_bps()) {
                const uint64_t before = LandedHeal(target, amount * std::max(0.0, 1 + bonus) * received);
                bonus += seal.bonus_bps() / 10000.0;
                const uint64_t after = LandedHeal(target, amount * std::max(0.0, 1 + bonus) * received);
                if (after > before) seasonSealHealGain = after - before;
            }
        }
    }
    if (const uint32_t wuxingHealPct = WuxingHealShieldBonusPct(source); wuxingHealPct > 0) {
        amount *= (100.0 + wuxingHealPct) / 100.0;
    }
    uint64_t beforeNextRoundHealBoost = 0u;
    bool applyNextRoundHealBoost = false;
    if (context.origin == BattleDamageOrigin::Direct &&
        activeNextRoundBoostCasterId == source.actor_id() &&
        activeNextRoundBoostKind == seasonrules::kResonanceGuardHealBoost &&
        activeNextRoundBoostPct > 0u) {
        beforeNextRoundHealBoost = LandedHeal(target, supportrules::ApplyBonus(amount, bonus) * received);
        amount *= (100.0 + activeNextRoundBoostPct) / 100.0;
        applyNextRoundHealBoost = true;
    }
    const uint64_t healed = ApplyHeal(target, supportrules::ApplyBonus(amount, bonus) * received);
    if (applyNextRoundHealBoost && healed > beforeNextRoundHealBoost) {
        activeNextRoundBoostExtra = AddSafe(activeNextRoundBoostExtra, healed - beforeNextRoundHealBoost);
    }
    RecordTrialMonsterHeal(source, target, healed);
    if (healed > 0) RecordEffectiveContribution(source);
    for (const auto& [index, gain] : contributions) UseArtifact(source, index, target.actor_id(), gain, result);
    if (seasonSealHealGain > 0u && SeasonSealAvailable(source, 4)) {
        const auto& seal = source.equipment_effects().season_seal();
        UseSeasonSeal(source, target.actor_id(), seasonSealHealGain, result);
        if (seal.resonance_kind() == seasonrules::kResonanceEmergencyHealRefund &&
            seal.resonance_skill_id() == context.skillId && seal.resonance_param() > 0u) {
            const uint64_t manaBefore = source.attributes().mana();
            const uint64_t missingMana = source.max_mana() > manaBefore ? source.max_mana() - manaBefore : 0u;
            const uint64_t refund = std::min(missingMana,
                seasonrules::ComputeManaPctCeil(context.paidMana, seal.resonance_param()));
            if (refund > 0u) {
                source.mutable_attributes()->set_mana(manaBefore + refund);
                auto* manaEv = AppendEvent(result, BATTLE_EVENT_MANA, source.actor_id(), source.actor_id());
                manaEv->set_skill_table_id(context.skillId);
                manaEv->set_value(refund);
                manaEv->set_mana_delta(static_cast<int64_t>(std::min<uint64_t>(refund, INT64_MAX)));
                manaEv->set_target_mana_after(manaBefore + refund);
                EmitSeasonResonanceEvent(source, source.actor_id(),
                                         seasonrules::kResonanceEventEmergencyHealRefund, refund, result);
            }
        }
    }
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
        const double typedOutgoing = damageType == combatdamage::kPhysicalDamage
            ? ConfiguredModifier(source, BattleModifier::OutgoingPhysicalDamage)
            : ConfiguredModifier(source, BattleModifier::OutgoingMagicDamage);
        const double selfGrowingDirect = ConfiguredModifier(source, BattleModifier::SelfGrowingDirectDamage);
        const double oneShotOutgoing = context.origin == BattleDamageOrigin::Direct
            ? ConfiguredModifier(source, damageType == combatdamage::kPhysicalDamage
                ? BattleModifier::NextPhysicalAttack : BattleModifier::NextMagicAttack)
            : 0.0;
        damage *= std::max(0.0, 1 + ConfiguredModifier(source, BattleModifier::OutgoingDamage) + typedOutgoing + selfGrowingDirect + oneShotOutgoing);
        damage = supportrules::ApplyBonus(damage, petcultivation::DamageBonus(source, damageType, context.origin, hitKind));
        damage *= std::max(0.0, 1 + ConfiguredModifier(target, BattleModifier::IncomingDamage) + HitVulnerabilityBonus(target));
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
    } else if (context.origin == BattleDamageOrigin::Periodic) {
        damage *= std::max(0.0, 1.0 + ConfiguredModifier(source, BattleModifier::PeriodicDamageBonus));
    }
    // 护盾处理前不能按当前生命截断:残血不应把一记致死重击缩成可被小盾全吃的伤害。
    if (ActorHasBuffOfType(target, kBuffTypeInvincibility)) damage = 0;
    if (target.is_defending() && context.origin != BattleDamageOrigin::Reflect && context.origin != BattleDamageOrigin::Redirect) damage *= 0.5;
    if (EscalationStacks() > 0 && context.origin != BattleDamageOrigin::Redirect && context.origin != BattleDamageOrigin::ResourceCost) {
        damage *= static_cast<double>(EscalationDamagePct()) / 100.0;
    }
    uint64_t remaining = combatdamage::DamageToHealth(damage, UINT64_MAX);
    const bool isPrimaryTarget = context.primaryTargetId == 0 || target.actor_id() == context.primaryTargetId;
    bool splashReduced = false;
    remaining = ApplyEncounterDamageModifiers(source, target, remaining, damageType,
        direct && context.origin == BattleDamageOrigin::Direct && hitKind != BATTLE_HIT_COUNTER,
        result, context.isMultiTargetSkill, isPrimaryTarget, &splashReduced,
        context.isPreparedCast || context.origin == BattleDamageOrigin::Periodic,
        context.origin == BattleDamageOrigin::Pursuit);
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
    absorbed = AddSafe(absorbed, AbsorbTournamentEarthShield(target.actor_id(), remaining));
    const uint64_t dealt = std::min(target.attributes().health(), remaining);
    target.mutable_attributes()->set_health(target.attributes().health() - dealt);
    if (context.origin == BattleDamageOrigin::Direct) {
        RecordTrialDirectDamage(source, target, damageType, AddSafe(dealt, absorbed));
    }
    if (AddSafe(dealt, absorbed) > 0) {
        RecordEffectiveContribution(source);
    }
    auto* event = AppendEvent(result, BATTLE_EVENT_DAMAGE, source.actor_id(), target.actor_id());
    event->set_skill_table_id(context.skillId); event->set_value(dealt); event->set_absorbed(absorbed);
    event->set_target_health_after(target.attributes().health()); event->set_target_mana_after(target.attributes().mana());
    event->set_hit_kind(hitKind); event->set_damage_origin(static_cast<uint32_t>(context.origin));
    event->set_instigator_id(context.instigatorId);
    event->set_is_critical(context.critical);
    if (context.isPrimaryFocus) {
        event->set_is_primary_focus(true);
    }
    if (splashReduced) event->set_is_splash_reduced(true);
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
    if (AddSafe(dealt, absorbed) > 0 &&
        (context.origin == BattleDamageOrigin::Direct || context.origin == BattleDamageOrigin::Pursuit) &&
        IsActorActive(target)) {
        IncrementHitVulnerability(target);
    }
    EquipmentBreakShields(source, target, shieldDamage, damageType, context, originalShields, result);
    EffectContext triggered = context; triggered.actualDamage = dealt;
    if (dealt > 0 && context.origin == BattleDamageOrigin::Direct && hitKind != BATTLE_HIT_COUNTER) {
        ApplyWuxingPostDirectHitEffects(source, target, dealt, context.critical, result);
    }
    if (dealt > 0 && (context.origin == BattleDamageOrigin::Direct || context.origin == BattleDamageOrigin::Redirect) && IsActorActive(target))
        TriggerConfiguredBuffs(target, source, BattleTrigger::AfterTake, damageType, triggered, result);
    if (dealt > 0 && context.origin == BattleDamageOrigin::Periodic && IsActorActive(source))
        TriggerConfiguredBuffs(source, target, BattleTrigger::AfterPeriodic, damageType, triggered, result);
    return dealt;
}

void TurnBattleEngine::TriggerConfiguredBuffs(BattleActorState& owner, BattleActorState& other,
                                               BattleTrigger trigger, uint32_t damageType,
                                               const EffectContext& context, TurnResultS2C& result) {
    const bool redirectedTake = trigger == BattleTrigger::AfterTake && context.origin == BattleDamageOrigin::Redirect;
    const bool controlAttempt = trigger == BattleTrigger::AfterControlAttempt;
    if (!IsActorActive(owner) || (context.origin != BattleDamageOrigin::Direct &&
        trigger != BattleTrigger::AfterPeriodic && !redirectedTake && !controlAttempt)) return;
    std::vector<uint64_t> ids;
    for (const auto& entry : owner.buffs()) ids.push_back(entry.buff_id());
    for (const auto id : ids) {
        int index = FindBuffIndex(owner, id); if (index < 0 || !IsActorActive(owner)) continue;
        const auto saved = owner.buffs(index);
        const auto* definition = dataProvider->FindBuff(saved.buff_table_id());
        if (definition == nullptr || definition->battle_op() == 0) continue;
        const bool triggerMatches = definition->trigger_event() == static_cast<uint32_t>(trigger) ||
            (definition->trigger_event() == static_cast<uint32_t>(BattleTrigger::AfterDirectAttack) &&
             (trigger == BattleTrigger::AfterBasic || trigger == BattleTrigger::AfterSkill));
        if (!triggerMatches || !MatchesDamage(*definition, damageType)) continue;
        // A36 援护分担转伤仅允许触发 GrowingAura 叠层，绝不递归触发反击、反伤或回蓝。
        if (redirectedTake && definition->battle_op() != static_cast<uint32_t>(BattleEffectOp::GrowingAura)) continue;
        const BuffTable row = *definition;
        const bool periodicTag = trigger == BattleTrigger::AfterPeriodic && !row.required_tag().empty();
        if (periodicTag && !((row.required_tag() == "Poison" && context.periodicState == static_cast<uint32_t>(BattleStateKind::Poison)) ||
            (row.required_tag() == "Burn" && context.periodicState == static_cast<uint32_t>(BattleStateKind::Burn)))) continue;
        if (trigger == BattleTrigger::AfterPeriodic && row.required_tag().empty() &&
            context.periodicState != static_cast<uint32_t>(BattleStateKind::Poison) &&
            context.periodicState != static_cast<uint32_t>(BattleStateKind::Burn)) continue;
        if (!periodicTag && !row.required_tag().empty() && !HasBuffTag(other, row.required_tag()) &&
            (row.second_required_tag().empty() || !HasBuffTag(other, row.second_required_tag()))) continue;
        if (row.battle_op() == static_cast<uint32_t>(BattleEffectOp::GrowingAura) &&
            trigger != BattleTrigger::AfterSupportCast &&
            owner.team_index() == other.team_index()) continue;
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
            if ((damageType == combatdamage::kPhysicalDamage && IsPhysicalForbidden(owner)) ||
                (damageType == combatdamage::kMagicDamage && IsMagicForbidden(owner))) break;
            const auto* incoming = dataProvider->FindSkill(context.skillId);
            if (incoming != nullptr && (other.is_boss() || incoming->forbid_copy() != 0 || IsSkillBanned(incoming->id()))) break;
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
        case BattleEffectOp::GrowingAura: {
            const uint32_t cap = std::max(1u, row.stack_limit());
            if (saved.stacks() < cap) {
                owner.mutable_buffs(index)->set_stacks(saved.stacks() + 1);
            }
            break;
        }
        default: break;
        }
        index = FindBuffIndex(owner, id);
        if (index >= 0 && saved.remaining_hits() > 0) {
            if (saved.remaining_hits() == 1) RemoveConfiguredBuff(owner, id, result);
            else owner.mutable_buffs(index)->set_remaining_hits(saved.remaining_hits() - 1);
        }
    }
}

void TurnBattleEngine::InitializeConfiguredAbilitiesForActor(BattleActorState& actor, TurnResultS2C& result) {
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
            for (const auto effect : row->init_effect()) ApplyConfiguredEffect(actor, *target, effect, context, result);
        }
    }
}
void TurnBattleEngine::InitializeConfiguredAbilities() {
    TurnResultS2C ignored;
    for (auto& actor : actors) {
        InitializeConfiguredAbilitiesForActor(actor, ignored);
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
                row->trigger_event() != static_cast<uint32_t>(BattleTrigger::RoundStart) ||
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
    if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::PrepareCast)) {
        SyncChargingSkillId(actor);
    }
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
            if ((row->action_tags() & kActionTagPhysical) &&
                (HasConfiguredState(actor, BattleStateKind::SealPhysical) || IsPhysicalForbidden(actor))) continue;
            if ((row->action_tags() & kActionTagMagic) &&
                (HasConfiguredState(actor, BattleStateKind::SealMagic) || IsMagicForbidden(actor))) continue;
            BattleAction confused; confused.set_action_type(BATTLE_ACTION_SKILL); confused.set_skill_table_id(skillId); confused.set_target_id(target->actor_id());
            ExecuteConfiguredSkill(actor, confused, *row, result, BattleDamageOrigin::Confusion); return true;
        }
        if (HasConfiguredState(actor, BattleStateKind::SealPhysical) || IsPhysicalForbidden(actor)) {
            ExecuteDefend(actor, result);
            return true;
        }
        bool critical = false; EffectContext context; context.origin = BattleDamageOrigin::Confusion;
        AppendEvent(result, BATTLE_EVENT_ATTACK, actor.actor_id(), target->actor_id());
        const double amount = CalculateFinalDamage(actor, *target, kBasicAttackBaseDamage,
            EffectivePhysicalAttack(actor), 1, combatdamage::kPhysicalDamage, critical);
        context.critical = critical;
        ResolveConfiguredDamage(actor, *target, amount, combatdamage::kPhysicalDamage, context, result);
        return true;
    }
    return action.action_type() == BATTLE_ACTION_ATTACK &&
        (HasConfiguredState(actor, BattleStateKind::SealPhysical) || IsPhysicalForbidden(actor));
}
std::optional<BattleAction> TurnBattleEngine::SelectAutoConfiguredSkill(const BattleActorState& actor) const {
    for (const auto skillId : actor.skill_table_ids()) {
        if (IsSkillBanned(skillId)) continue;
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
                else if (op == BattleEffectOp::CopyTargetSkill) useful = !IsImmuneToBuff(target, *effect) && HasCopyableSkillFor(actor, target);
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
    if (const auto* actor = FindActor(actorId); actor != nullptr) {
        RecordManualActionForActor(*actor);
    }
    return AllPlayersReady();
}

uint64_t TurnBattleEngine::EffectiveMagicAttack(const BattleActorState& actor) const {
    const double mult = std::max(0.1, 1.0 + ConfiguredModifier(actor, BattleModifier::MagicAttack));
    return Whole(static_cast<double>(actor.magic_attack()) * mult);
}

double TurnBattleEngine::EffectiveMitigationScore(const BattleActorState& actor, bool isMagic) const {
    const uint64_t resistBonus = isMagic ? actor.combat().magic_resist() : actor.combat().physical_resist();
    const uint32_t equipResist = isMagic ? actor.equipment_effects().magic_resist_bps()
                                         : actor.equipment_effects().physical_resist_bps();
    const uint64_t legacyResist = AddSafe(actor.attributes().resistance(), resistBonus);
    const double received = equipbattle::ReceivedRatio(
        actor.attributes().armor(), EffectiveDefense(actor),
        static_cast<double>(legacyResist) * 100.0 + equipResist, actor.level());
    // 承受比例越低说明防御越厚，返回 1 - received 作为防御厚度分，便于按“最低防御”升序挑选脆皮。
    return 1.0 - std::clamp(received, 0.0, 1.0);
}

bool TurnBattleEngine::IsPhysicalForbidden(const BattleActorState& actor) const {
    return ConfiguredModifier(actor, BattleModifier::ForbidPhysical) > 0.0;
}

bool TurnBattleEngine::IsMagicForbidden(const BattleActorState& actor) const {
    return ConfiguredModifier(actor, BattleModifier::ForbidMagic) > 0.0;
}

bool TurnBattleEngine::TryConsumeControlImmunity(BattleActorState& caster, BattleActorState& target,
                                                 const BuffTable& row, TurnResultS2C& result) {
    for (int i = 0; i < target.buffs_size(); ++i) {
        const auto saved = target.buffs(i);
        const auto* guard = dataProvider->FindBuff(saved.buff_table_id());
        if (guard == nullptr || guard->battle_op() != static_cast<uint32_t>(BattleEffectOp::ControlImmunity)) {
            continue;
        }
        const uint64_t guardId = saved.buff_id();
        if (saved.remaining_hits() <= 1) {
            RemoveConfiguredBuff(target, guardId, result);
        } else {
            target.mutable_buffs(i)->set_remaining_hits(saved.remaining_hits() - 1);
        }
        if (target.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
            antiControlOrSeasonSkillPlayers.insert(target.actor_id());
        }
        auto* resisted = AppendEvent(result, BATTLE_EVENT_RESIST, caster.actor_id(), target.actor_id());
        resisted->set_buff_table_id(row.id());
        return true;
    }
    return false;
}

void TurnBattleEngine::IncrementHitVulnerability(BattleActorState& target) {
    for (int i = 0; i < target.buffs_size(); ++i) {
        const auto& saved = target.buffs(i);
        const auto* row = dataProvider->FindBuff(saved.buff_table_id());
        if (row == nullptr || row->battle_op() != static_cast<uint32_t>(BattleEffectOp::HitVulnerability)) {
            continue;
        }
        // 同一行动 group_id 对同一目标至多叠 1 层，防止连击或多段技能单次叠满。
        if (saved.last_trigger_round() == roundIndex && saved.secondary_value() == currentGroupId) {
            continue;
        }
        const uint32_t cap = std::max(1u, row->stack_limit());
        if (saved.stacks() >= cap) {
            continue;
        }
        auto* entry = target.mutable_buffs(i);
        entry->set_stacks(saved.stacks() + 1);
        entry->set_last_trigger_round(roundIndex);
        entry->set_secondary_value(currentGroupId);
    }
}

double TurnBattleEngine::HitVulnerabilityBonus(const BattleActorState& target) const {
    double bonus = 0.0;
    for (const auto& entry : target.buffs()) {
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row == nullptr || row->battle_op() != static_cast<uint32_t>(BattleEffectOp::HitVulnerability)) {
            continue;
        }
        if (entry.stacks() > 0 && std::isfinite(row->ratio())) {
            bonus += row->ratio() * static_cast<double>(entry.stacks());
        }
    }
    return bonus;
}

void TurnBattleEngine::ResetVolatileGrowingStacks(BattleActorState& actor) {
    for (int i = 0; i < actor.buffs_size(); ++i) {
        const auto* row = dataProvider->FindBuff(actor.buffs(i).buff_table_id());
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::GrowingAura) &&
            row->keep_on_death() == 0 && actor.buffs(i).stacks() > 0) {
            actor.mutable_buffs(i)->set_stacks(0);
        }
    }
}

void TurnBattleEngine::SyncChargingSkillId(BattleActorState& actor) const {
    uint32_t chargingId = 0;
    for (const auto& entry : actor.buffs()) {
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::PrepareCast)) {
            chargingId = entry.source_skill_id();
            break;
        }
    }
    actor.set_charging_skill_id(chargingId);
}

bool TurnBattleEngine::ExecutePreparedCastIfReady(BattleActorState& actor, TurnResultS2C& result) {
    int prepareIndex = -1;
    for (int i = 0; i < actor.buffs_size(); ++i) {
        const auto& entry = actor.buffs(i);
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::PrepareCast) &&
            entry.applied_round() < roundIndex) {
            prepareIndex = i;
            break;
        }
    }
    if (prepareIndex < 0) {
        return false;
    }

    const BattleBuffEntry saved = actor.buffs(prepareIndex);
    const auto* prepareRow = dataProvider->FindBuff(saved.buff_table_id());
    const auto* skillRow = dataProvider->FindSkill(saved.source_skill_id());
    const bool blocked =
        prepareRow == nullptr || skillRow == nullptr ||
        ActorHasBuffOfType(actor, kBuffTypeStun) || ActorHasBuffOfType(actor, kBuffTypeFreeze) ||
        HasConfiguredState(actor, BattleStateKind::Freeze) ||
        HasConfiguredState(actor, BattleStateKind::Sleep) ||
        HasConfiguredState(actor, BattleStateKind::Confusion) ||
        HasConfiguredState(actor, BattleStateKind::SealActive) ||
        ((skillRow->action_tags() & kActionTagPhysical) != 0 &&
         (HasConfiguredState(actor, BattleStateKind::SealPhysical) || IsPhysicalForbidden(actor))) ||
        ((skillRow->action_tags() & kActionTagMagic) != 0 &&
         (HasConfiguredState(actor, BattleStateKind::SealMagic) || IsMagicForbidden(actor)));

    // 无论释放或被打断，蓄力状态均在本回合窗口消耗清除（打断不退蓝、不退冷却）。
    RemoveConfiguredBuff(actor, saved.buff_id(), result);
    if (blocked) {
        return false;
    }

    auto targets = SelectConfiguredTargets(actor, *skillRow, saved.secondary_value());
    if (targets.empty()) {
        return true;
    }

    EffectContext context;
    context.skillId = skillRow->id();
    context.rank = std::max(1u, ConfiguredRank(actor, *skillRow));
    context.origin = BattleDamageOrigin::Direct;
    context.primaryTargetId = targets.front();
    context.isPreparedCast = true;
    const uint32_t subjectMaxTargets = actor.actor_type() == BATTLE_ACTOR_TYPE_PET
        ? (skillRow->pet_max_targets() > 0 ? skillRow->pet_max_targets() : skillRow->max_targets())
        : skillRow->max_targets();
    context.primaryFocusBonus = IsCompetitivePvpBattle() &&
        actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER &&
        subjectMaxTargets > 1 && saved.secondary_value() != 0 &&
        targets.front() == saved.secondary_value() && targets.size() < subjectMaxTargets;
    context.isMultiTargetSkill = subjectMaxTargets > 1u;

    auto* event = AppendEvent(result, BATTLE_EVENT_SKILL, actor.actor_id(), targets.front());
    event->set_skill_table_id(skillRow->id());
    event->set_hit_kind(BATTLE_HIT_NORMAL);

    const int effectsBegin = result.events_size();
    const auto payloadBuffs = prepareRow->sub_buff();
    for (size_t i = 0; i < targets.size(); ++i) {
        auto* target = FindActor(targets[i]);
        if (target == nullptr || target->fled()) continue;
        currentHitIndex = static_cast<uint32_t>(i);
        auto targetContext = context;
        for (const auto childBuffId : payloadBuffs) {
            ApplyConfiguredEffect(actor, *target, childBuffId, targetContext, result, 1);
            const auto* def = dataProvider->FindBuff(childBuffId);
            if (def != nullptr && def->battle_op() == static_cast<uint32_t>(BattleEffectOp::Damage)) {
                ++targetContext.hitOrdinal;
            }
        }
    }
    currentHitIndex = 0;

    bool hitDirectly = false;
    for (int i = effectsBegin; i < result.events_size(); ++i) {
        const auto& ev = result.events(i);
        if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.source_id() == actor.actor_id() &&
            ev.skill_table_id() == skillRow->id() && ev.value() > 0 && ev.damage_origin() == 0) {
            hitDirectly = true;
            break;
        }
    }
    if (hitDirectly && IsActorActive(actor)) {
        ConsumeOneShotAttackModifiers(actor, skillRow->damage_type(), result);
        auto* primary = FindActor(targets.front());
        if (primary != nullptr) {
            TriggerConfiguredBuffs(actor, *primary, BattleTrigger::AfterSkill,
                                   skillRow->damage_type(), context, result);
        }
    }
    return true;
}

void TurnBattleEngine::ConsumeOneShotAttackModifiers(BattleActorState& source, uint32_t damageType,
                                                     TurnResultS2C& result) {
    const uint32_t targetStat = damageType == combatdamage::kPhysicalDamage
        ? static_cast<uint32_t>(BattleModifier::NextPhysicalAttack)
        : static_cast<uint32_t>(BattleModifier::NextMagicAttack);
    std::vector<uint64_t> toRemove;
    for (const auto& entry : source.buffs()) {
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Modifier) &&
            row->modifier_stat() == targetStat) {
            toRemove.push_back(entry.buff_id());
        }
    }
    for (const auto buffId : toRemove) {
        RemoveConfiguredBuff(source, buffId, result);
    }
}
}  // namespace turnbattle

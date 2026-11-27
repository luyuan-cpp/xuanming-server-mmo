#include "system/turn_battle_engine.h"
#include "system/equipment_battle_rules.h"
#include "system/combat_damage_rules.h"
#include "system/season_rules.h"
#include "system/support_effect_rules.h"
#include "system/pet_cultivation_rules.h"

#include <algorithm>
#include <cmath>
#include <set>

namespace turnbattle {
namespace {
uint64_t SaturatingAddU64(uint64_t lhs, uint64_t rhs) {
    return rhs > UINT64_MAX - lhs ? UINT64_MAX : lhs + rhs;
}
uint32_t Value(const google::protobuf::Map<uint32_t, uint32_t>& values, uint32_t key) {
    const auto found = values.find(key);
    return found == values.end() ? 0u : found->second;
}
bool Bounded(const google::protobuf::Map<uint32_t, uint32_t>& values, uint32_t keys, uint32_t maximum) {
    for (const auto& [key, value] : values) if (key == 0 || key > keys || value > maximum) return false;
    return true;
}
bool IsAilment(uint32_t state) { return state >= 1 && state <= 7; }
const BattleArtifactState* ArtifactState(const BattleActorState& actor, uint64_t itemId) {
    for (const auto& state : actor.artifact_states()) if (state.item_id() == itemId) return &state;
    return nullptr;
}
BattleArtifactState* ArtifactState(BattleActorState& actor, uint64_t itemId) {
    for (auto& state : *actor.mutable_artifact_states()) if (state.item_id() == itemId) return &state;
    return nullptr;
}
}

bool TurnBattleEngine::InitializeEquipment(BattleActorState& actor, const EquipmentEffects& effects) {
    if (effects.physical_attack_bps() > equipbattle::MaximumEffectiveSourceBps(1) || effects.magic_attack_bps() > equipbattle::MaximumEffectiveSourceBps(2) ||
        effects.max_health_bps() > equipbattle::MaximumEffectiveSourceBps(3) || effects.speed_bps() > equipbattle::MaximumEffectiveSourceBps(4) ||
        effects.outgoing_heal_bps() > equipbattle::MaximumEffectiveSourceBps(5) || effects.shield_capacity_bps() > equipbattle::MaximumEffectiveSourceBps(6) ||
        effects.physical_crit_bps() > equipbattle::MaximumEffectiveSourceBps(7) || effects.magic_crit_bps() > equipbattle::MaximumEffectiveSourceBps(8) ||
        effects.physical_resist_bps() > equipbattle::MaximumEffectiveSourceBps(9) || effects.magic_resist_bps() > equipbattle::MaximumEffectiveSourceBps(10) ||
        !Bounded(effects.ailment_hit_bps(), 7, equipbattle::MaximumEffectiveSourceBps(11)) ||
        !Bounded(effects.ailment_resist_bps(), 7, equipbattle::MaximumEffectiveSourceBps(12)) ||
        !Bounded(effects.mana_reduction_bps(), 12, equipbattle::MaximumEffectiveSourceBps(13)) || effects.artifacts_size() > 2 ||
        effects.season_max_health_bps() > seasonrules::kHardStatCapBps ||
        effects.season_defense_bps() > seasonrules::kHardStatCapBps) return false;
    if (effects.has_season_seal()) {
        const auto& seal = effects.season_seal();
        if (!seasonrules::ValidSealDefinition(seal.season_id(), seal.seal_id(), seal.effect_id(),
                seal.bonus_bps(), seal.cap_bps(), seal.per_round_limit(), seal.battle_limit(),
                seal.active_rounds(), seal.health_threshold_bps(),
                seal.resonance_skill_id(), seal.resonance_kind(), seal.resonance_param())) return false;
    }
    std::set<uint64_t> instances;
    for (const auto& source : effects.artifacts()) {
        if (source.item_id() == 0 || source.item_id() >> 63 || !instances.insert(source.item_id()).second ||
            !equipbattle::ValidArtifactDefinition(source.effect_id(), source.bonus_bps(), source.cap_bps(),
                source.per_round_limit(), source.battle_limit(), source.active_rounds(), source.health_threshold_bps())) return false;
    }
    *actor.mutable_equipment_effects() = effects;
    if (!supportrules::ValidEquipmentSources(effects)) {
        actor.mutable_equipment_effects()->set_support_sources_complete(false);
        actor.mutable_equipment_effects()->clear_support_sources();
    }
    auto* sources = actor.mutable_equipment_effects()->mutable_artifacts();
    std::sort(sources->begin(), sources->end(), [](const auto& left, const auto& right) { return left.item_id() < right.item_id(); });
    for (const auto& source : *sources) actor.add_artifact_states()->set_item_id(source.item_id());
    actor.clear_season_seal_state();
    actor.set_entry_max_health(actor.max_health());
    actor.set_entry_max_mana(actor.max_mana());
    return true;
}

uint32_t TurnBattleEngine::EquipmentManaCategory(const SkillTable& skill) const {
    return equipbattle::ManaCategory(skill, [this](uint32_t id) { return dataProvider->FindBuff(id); });
}

std::vector<int> TurnBattleEngine::AvailableArtifacts(const BattleActorState& actor, uint32_t effect) const {
    std::vector<int> available;
    if (actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || !IsActorActive(actor)) return available;
    const auto& sources = actor.equipment_effects().artifacts();
    for (int i = 0; i < sources.size(); ++i) {
        const auto& source = sources.Get(i); const auto* state = ArtifactState(actor, source.item_id());
        if (state == nullptr || source.effect_id() != effect || (source.active_rounds() > 0 && roundIndex > source.active_rounds()) ||
            (source.battle_limit() > 0 && state->used_count() >= source.battle_limit()) ||
            (source.per_round_limit() > 0 && state->last_trigger_round() == roundIndex &&
                state->round_trigger_count() >= source.per_round_limit())) continue;
        available.push_back(i);
    }
    std::sort(available.begin(), available.end(), [&sources](int left, int right) {
        return sources.Get(left).item_id() < sources.Get(right).item_id();
    });
    return available;
}

void TurnBattleEngine::UseArtifact(BattleActorState& actor, int index, uint64_t targetId,
                                    uint64_t value, TurnResultS2C& result) {
    const auto& source = actor.equipment_effects().artifacts(index);
    auto* state = ArtifactState(actor, source.item_id());
    if (state == nullptr) return;
    state->set_used_count(state->used_count() + 1);
    state->set_round_trigger_count(state->last_trigger_round() == roundIndex ? state->round_trigger_count() + 1 : 1);
    state->set_last_trigger_round(roundIndex);
    auto* event = AppendEvent(result, BATTLE_EVENT_ARTIFACT, actor.actor_id(), targetId);
    event->set_artifact_item_id(source.item_id()); event->set_artifact_effect_id(source.effect_id());
    event->set_value(value); event->set_success(true);
}

bool TurnBattleEngine::SeasonSealAvailable(const BattleActorState& actor, uint32_t effect) const {
    if (actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || !IsActorActive(actor) ||
        !actor.equipment_effects().has_season_seal()) return false;
    const auto& seal = actor.equipment_effects().season_seal();
    if (seal.effect_id() != effect || (seal.active_rounds() > 0 && roundIndex > seal.active_rounds())) return false;
    const auto& state = actor.season_seal_state();
    if (seal.battle_limit() > 0 && state.used_count() >= seal.battle_limit()) return false;
    if (seal.per_round_limit() > 0 && state.last_trigger_round() == roundIndex &&
        state.round_trigger_count() >= seal.per_round_limit()) return false;
    return true;
}

void TurnBattleEngine::UseSeasonSeal(BattleActorState& actor, uint64_t targetId,
                                     uint64_t value, TurnResultS2C& result) {
    if (!actor.equipment_effects().has_season_seal()) return;
    const auto& seal = actor.equipment_effects().season_seal();
    auto* state = actor.mutable_season_seal_state();
    state->set_used_count(state->used_count() + 1);
    state->set_round_trigger_count(state->last_trigger_round() == roundIndex ? state->round_trigger_count() + 1 : 1);
    state->set_last_trigger_round(roundIndex);
    auto& runtime = seasonSealRuntimeByActor[actor.actor_id()];
    runtime.triggerCount += 1u;
    runtime.contributionValue = SaturatingAddU64(runtime.contributionValue, value);
    auto* event = AppendEvent(result, BATTLE_EVENT_ARTIFACT, actor.actor_id(), targetId);
    event->set_artifact_item_id(0);
    event->set_artifact_effect_id(seal.effect_id());
    event->set_season_seal_id(seal.seal_id());
    event->set_value(value);
    event->set_success(true);
}

bool TurnBattleEngine::HasAnyRegularShield(const BattleActorState& target) const {
    for (const auto& entry : target.buffs()) {
        if (entry.remaining_capacity() == 0) continue;
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Shield) &&
            row->hit_limit() == 0) {
            return true;
        }
    }
    return false;
}

uint32_t TurnBattleEngine::DefendManaDiscountResonancePct(const BattleActorState& actor, uint32_t skillId) const {
    if (actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER ||
        !actor.equipment_effects().has_season_seal()) {
        return 0u;
    }
    const auto& seal = actor.equipment_effects().season_seal();
    if (seal.resonance_kind() != seasonrules::kResonanceDefendManaDiscount ||
        seal.resonance_skill_id() != skillId || seal.resonance_param() == 0u) {
        return 0u;
    }
    const auto it = seasonSealRuntimeByActor.find(actor.actor_id());
    if (it != seasonSealRuntimeByActor.end() && it->second.defendResonanceReadyRound == roundIndex) {
        return seal.resonance_param();
    }
    return 0u;
}

void TurnBattleEngine::EmitSeasonResonanceEvent(BattleActorState& actor, uint64_t targetId,
                                                uint32_t resonanceEffectId, uint64_t value,
                                                TurnResultS2C& result) {
    if (!actor.equipment_effects().has_season_seal()) return;
    const auto& seal = actor.equipment_effects().season_seal();
    auto& runtime = seasonSealRuntimeByActor[actor.actor_id()];
    runtime.resonanceCount += 1u;
    if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER &&
        resonanceEffectId > seasonrules::kResonanceEventBase) {
        const uint32_t kind = resonanceEffectId - seasonrules::kResonanceEventBase;
        if (kind >= 1u && kind <= 31u) {
            seasonResonanceMaskByPlayer[actor.actor_id()] |= (1u << (kind - 1u));
        }
    }
    auto* event = AppendEvent(result, BATTLE_EVENT_ARTIFACT, actor.actor_id(), targetId);
    event->set_artifact_item_id(0);
    event->set_artifact_effect_id(resonanceEffectId);
    event->set_season_seal_id(seal.seal_id());
    event->set_value(value);
    event->set_success(true);
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
    if (SeasonSealAvailable(actor, 7) && missing > total) {
        const auto& seal = actor.equipment_effects().season_seal();
        const uint64_t gain = std::min(missing - total, equipbattle::FloorBps(actor.entry_max_mana(), seal.bonus_bps()));
        if (gain > 0) {
            total += gain;
            UseSeasonSeal(actor, actor.actor_id(), gain, result);
            if (seal.resonance_kind() == seasonrules::kResonanceDefendManaDiscount ||
                seal.resonance_kind() == seasonrules::kResonanceDefendShieldBoost) {
                seasonSealRuntimeByActor[actor.actor_id()].defendResonanceReadyRound = roundIndex + 1u;
            }
        }
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
    if (SeasonSealAvailable(target, 6) && damage > reduction) {
        const auto& seal = target.equipment_effects().season_seal();
        const uint64_t part = std::min({damage - reduction, equipbattle::FloorBps(damage, seal.bonus_bps()),
            equipbattle::FloorBps(target.entry_max_health(), seal.cap_bps())});
        if (part > 0) {
            reduction += part;
            UseSeasonSeal(target, target.actor_id(), part, result);
            if (seal.resonance_kind() == seasonrules::kResonanceGuardShieldBoost ||
                seal.resonance_kind() == seasonrules::kResonanceGuardHealBoost) {
                auto& rt = seasonSealRuntimeByActor[target.actor_id()];
                if (!rt.guardResonanceConsumed) {
                    rt.guardResonanceReadyRound = roundIndex + 1u;
                }
            }
        }
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
        if (removed > 0) {
            UseArtifact(source, index, target.actor_id(), removed, result);
            if (source.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
                shieldBreakPlayers.insert(source.actor_id());
            }
        }
    }
    if (effect != 0 && SeasonSealAvailable(source, effect)) {
        const auto& seal = source.equipment_effects().season_seal();
        const uint64_t before = equipbattle::FloorBps(damage, accumulated);
        accumulated += seal.bonus_bps();
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
            event->set_artifact_item_id(0); event->set_artifact_effect_id(effect);
            event->set_season_seal_id(seal.seal_id());
            if (part == saved.remaining_capacity()) RemoveConfiguredBuff(target, id, result);
        }
        if (removed > 0) {
            UseSeasonSeal(source, target.actor_id(), removed, result);
            if (source.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
                shieldBreakPlayers.insert(source.actor_id());
            }
            if (seal.resonance_skill_id() == context.skillId && seal.resonance_param() > 0u) {
                if (seal.resonance_kind() == seasonrules::kResonanceSharpManaRefund &&
                    !HasAnyRegularShield(target)) {
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
                                                 seasonrules::kResonanceEventSharpManaRefund, refund, result);
                    }
                } else if (seal.resonance_kind() == seasonrules::kResonanceBreakExtendDebuff) {
                    pendingMysticExtendCasterId = source.actor_id();
                    pendingMysticExtendTargetId = target.actor_id();
                    pendingMysticExtendSkillId = context.skillId;
                    pendingMysticExtendBonusRounds = seal.resonance_param();
                }
            }
        }
    }
}

double TurnBattleEngine::EquipmentOutgoingHeal(const BattleActorState& source) const {
    return OutgoingHealBonus(source) + ConfiguredModifier(source, BattleModifier::OutgoingHeal) +
        source.equipment_effects().outgoing_heal_bps() / 10000.0 + petcultivation::HealBonus(source);
}

void TurnBattleEngine::BuildSupportEffects(const BattleActorState& actor, SupportEffectSummary& output) const {
    supportrules::Begin(actor.equipment_effects(), output);
    output.set_in_battle(true); output.set_round_index(roundIndex);
    auto* healing = output.mutable_healing(); auto* shield = output.mutable_shield();
    const double legacyHealing = OutgoingHealBonus(actor, healing);
    const double healingModifier = ConfiguredModifier(actor, BattleModifier::OutgoingHeal, healing);
    const double shieldModifier = ConfiguredModifier(actor, BattleModifier::OutgoingShield, shield);
    const double healingBonus = legacyHealing + healingModifier + actor.equipment_effects().outgoing_heal_bps() / 10000.0;
    const double shieldBonus = actor.equipment_effects().shield_capacity_bps() / 10000.0 + shieldModifier;
    healing->set_total_bonus_bps(healingBonus * 10000);
    shield->set_total_bonus_bps(shieldBonus * 10000);
    bool complete = actor.equipment_effects().support_sources_complete();
    // InitPlayers 会过滤本服查不到的技能。跨版本快照仍需按入场原件识别未知的已学行。
    for (const auto& player : createRequest.players()) {
        if (player.player_id() != actor.actor_id()) continue;
        for (const auto& [id, rank] : player.skill_ranks()) {
            if (rank > 0 && dataProvider->FindSkill(id) == nullptr && !supportrules::IsRetiredSkill(id))
                complete = false;
        }
        break;
    }
    for (const auto& buff : actor.buffs()) if (dataProvider->FindBuff(buff.buff_table_id()) == nullptr) complete = false;
    for (const auto& [id, rank] : actor.skill_ranks()) {
        if (rank == 0) continue;
        const auto* skill = dataProvider->FindSkill(id);
        if (skill == nullptr) {
            if (!supportrules::IsRetiredSkill(id)) complete = false;
            continue;
        }
        if (skill->battle_model() == 1) {
            for (const auto buffId : skill->effect()) {
                if (dataProvider->FindBuff(buffId) == nullptr) complete = false;
            }
            for (const auto buffId : skill->init_effect()) {
                if (dataProvider->FindBuff(buffId) == nullptr) complete = false;
            }
        }
    }
    healing->set_complete(complete); shield->set_complete(complete);
    for (const bool isShield : {false, true}) {
        auto* part = isShield ? shield : healing;
        const auto available = AvailableArtifacts(actor, isShield ? 5 : 4);
        for (auto& source : *part->mutable_sources()) {
            if (!source.conditional()) continue;
            const bool usable = std::any_of(available.begin(), available.end(), [&](int index) {
                return actor.equipment_effects().artifacts(index).item_id() == source.item_id();
            });
            if (!usable) source.set_reason("当前次数/回合或角色状态不满足法宝条件，不计常驻总计");
        }
    }
    // 本局有效已学目录；未学的场景参考不能冒充战斗中可施放的实时预览。
    const std::map<uint32_t, uint32_t> ranks(actor.skill_ranks().begin(), actor.skill_ranks().end());
    for (const auto& [id, rank] : ranks) {
        const auto* skill = dataProvider->FindSkill(id);
        if (skill == nullptr || rank == 0 || skill->battle_model() != 1) continue;
        const uint32_t category = EquipmentManaCategory(*skill);
        const auto reduction = actor.equipment_effects().mana_reduction_bps().find(category);
        const double discount = (reduction == actor.equipment_effects().mana_reduction_bps().end() ? 0 : reduction->second) -
            ConfiguredModifier(actor, BattleModifier::ManaCost) * 10000;
        for (const auto buffId : skill->effect()) {
            const auto* buff = dataProvider->FindBuff(buffId); if (buff == nullptr) continue;
            const bool isShield = buff->battle_op() == static_cast<uint32_t>(BattleEffectOp::Shield);
            SupportSkillPreview preview;
            if (supportrules::FillPreview(*skill, *buff, actor.level(), rank, actor.max_mana(),
                isShield ? shieldBonus : healingBonus, discount, complete, preview)) {
                *output.add_previews() = preview; break;
            }
        }
    }
}

double TurnBattleEngine::EquipmentShield(BattleActorState& source, BattleActorState& target, double base,
                                         const EffectContext& context, std::vector<int>& contributions,
                                         uint64_t* outSeasonSealGain) const {
    if (outSeasonSealGain != nullptr) *outSeasonSealGain = 0u;
    if (source.actor_type() == BATTLE_ACTOR_TYPE_PET)
        return supportrules::ApplyBonus(base, petcultivation::ShieldBonus(source) + ConfiguredModifier(source, BattleModifier::OutgoingShield));
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
        if (SeasonSealAvailable(source, 5)) {
            const uint32_t bps = source.equipment_effects().season_seal().bonus_bps();
            const double beforeVal = std::max(0.0, base * (1 + bonus));
            const double afterVal = std::max(0.0, base * (1 + bonus + bps / 10000.0));
            const uint64_t beforeFloor = static_cast<uint64_t>(std::floor(beforeVal));
            const uint64_t afterFloor = static_cast<uint64_t>(std::floor(afterVal));
            if (afterFloor > beforeFloor && outSeasonSealGain != nullptr) {
                *outSeasonSealGain = afterFloor - beforeFloor;
            }
            bonus += bps / 10000.0;
        }
    }
    return supportrules::ApplyBonus(base, bonus);
}
void TurnBattleEngine::CommitEquipmentShield(BattleActorState& source, uint64_t targetId,
                                             const std::vector<int>& contributions, uint64_t seasonSealGain,
                                             uint32_t castSkillId, TurnResultS2C& result) {
    for (const int index : contributions) UseArtifact(source, index, targetId,
        source.equipment_effects().artifacts(index).bonus_bps(), result);
    if (seasonSealGain > 0u && SeasonSealAvailable(source, 5)) {
        const auto& seal = source.equipment_effects().season_seal();
        UseSeasonSeal(source, targetId, seasonSealGain, result);
        if (seal.resonance_kind() == seasonrules::kResonanceFirstShieldMana &&
            seal.resonance_skill_id() == castSkillId && seal.resonance_param() > 0u) {
            const uint64_t manaBefore = source.attributes().mana();
            const uint64_t missingMana = source.max_mana() > manaBefore ? source.max_mana() - manaBefore : 0u;
            const uint64_t gain = std::min(missingMana,
                equipbattle::FloorBps(source.entry_max_mana(), seal.resonance_param()));
            if (gain > 0u) {
                source.mutable_attributes()->set_mana(manaBefore + gain);
                auto* manaEv = AppendEvent(result, BATTLE_EVENT_MANA, source.actor_id(), source.actor_id());
                manaEv->set_skill_table_id(castSkillId);
                manaEv->set_value(gain);
                manaEv->set_mana_delta(static_cast<int64_t>(std::min<uint64_t>(gain, INT64_MAX)));
                manaEv->set_target_mana_after(manaBefore + gain);
                EmitSeasonResonanceEvent(source, source.actor_id(),
                                         seasonrules::kResonanceEventFirstShieldMana, gain, result);
            }
        }
    }
}

bool TurnBattleEngine::EquipmentControlRoll(BattleActorState& caster, BattleActorState& target,
                                            const BuffTable& row, const EffectContext& context, TurnResultS2C& result) {
    const uint32_t kind = row.state_kind();
    if (!IsAilment(kind) || !std::isfinite(row.application_chance()) || row.application_chance() <= 0) return false;
    const auto* skill = dataProvider->FindSkill(context.skillId);
    const bool normal = context.origin == BattleDamageOrigin::Direct && context.primaryTargetId != 0;
    uint32_t primaryBuff = 0;
    // 技能效果顺序显式确定唯一主障碍；同动作的次要或重复障碍不生成、消费回响。
    if (skill != nullptr) for (const auto id : skill->effect()) {
        const auto* candidate = dataProvider->FindBuff(id);
        if (candidate != nullptr && IsAilment(candidate->state_kind()) &&
            (candidate->battle_op() == static_cast<uint32_t>(BattleEffectOp::Control) ||
             candidate->battle_op() == static_cast<uint32_t>(BattleEffectOp::PeriodicDamage))) { primaryBuff = id; break; }
    }
    const bool primary = normal && context.primaryTargetId == target.actor_id() && primaryBuff == row.id();
    double hit = Value(caster.equipment_effects().ailment_hit_bps(), kind) + petcultivation::HitBps(caster, kind);
    double resist = Value(target.equipment_effects().ailment_resist_bps(), kind) + petcultivation::ResistBps(target, kind);
    const auto& combat = target.combat();
    switch (static_cast<BattleStateKind>(kind)) {
    case BattleStateKind::Poison: resist += 100.0 * combat.resist_poison(); break;
    case BattleStateKind::Freeze: resist += 100.0 * combat.resist_freeze(); break;
    case BattleStateKind::Sleep: resist += 100.0 * combat.resist_sleep(); break;
    case BattleStateKind::Confusion: resist += 100.0 * combat.resist_confusion(); break;
    default: resist += 100.0 * combat.resist_forget(); break;
    }
    if (kind == 4 || kind == 5) resist += 10000 * ConfiguredModifier(target, BattleModifier::ControlResist);
    if (kind >= 1 && kind <= 6) resist += 10000 * ConfiguredModifier(target, BattleModifier::AllControlResist);
    if (ActiveTournamentLeyline() == TOURNAMENT_LEYLINE_WATER) {
        resist += 800.0;
    }
    if (const uint32_t wuxingCtrlResist = WuxingControlResistPct(target); wuxingCtrlResist > 0) {
        resist += 100.0 * wuxingCtrlResist;
    }
    bool consumedEcho = false;
    if (primary) for (const auto& source : caster.equipment_effects().artifacts()) {
        auto* state = ArtifactState(caster, source.item_id());
        if (source.effect_id() != 3 || state == nullptr || state->pending_round() != roundIndex || state->pending_ailment_kind() != kind) continue;
        hit += source.bonus_bps();
        state->clear_pending_ailment_kind(); state->clear_pending_round(); consumedEcho = true;
    }
    if (normal && kind <= 6) for (const int index : AvailableArtifacts(target, 8)) {
        resist += target.equipment_effects().artifacts(index).bonus_bps();
        UseArtifact(target, index, target.actor_id(), target.equipment_effects().artifacts(index).bonus_bps(), result);
        if (target.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
            antiControlOrSeasonSkillPlayers.insert(target.actor_id());
        }
    }
    const double chance = equipbattle::AilmentChance(row.application_chance(), hit, resist,
        100.0 * combat.resist_all_ailment(), 100.0 * caster.combat().ignore_ailment_resist(), kind == 7);
    const bool success = chance > 0 && Rand01() < chance;
    if (!success) {
        auto* event = AppendEvent(result, BATTLE_EVENT_RESIST, caster.actor_id(), target.actor_id());
        event->set_buff_table_id(row.id());
        if (primary && !consumedEcho) for (const int index : AvailableArtifacts(caster, 3)) {
            auto* state = ArtifactState(caster, caster.equipment_effects().artifacts(index).item_id());
            if (state == nullptr || state->pending_round() != 0) continue;
            state->set_pending_ailment_kind(kind); state->set_pending_round(roundIndex + 1);
            UseArtifact(caster, index, caster.actor_id(), kind, result);
        }
    }
    return success;
}
} // namespace turnbattle

#include "player_support_effects.h"
#include <algorithm>
#include <map>
#include <vector>
#include "player/system/player_equipment_v1.h"
#include "player/system/player_skill.h"
#include "player/system/player_level_rules.h"
#include "battle/system/player_battle.h"
#include "services/battle/constants/battle_effect_constants.h"
#include "services/battle/system/support_effect_rules.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "table/code/skill_table.h"
#include "table/code/buff_table.h"
#include "thread_context/ecs_context.h"

void PlayerSupportEffectSystem::Build(entt::entity player, SupportEffectSummary& summary) {
    summary.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return;
    EquipmentEffects equipment;
    PlayerEquipmentV1System::CollectEffects(player, equipment);
    supportrules::Begin(equipment, summary);
    const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
    const auto* attributes = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player);
    const bool inputs = levelComp != nullptr && attributes != nullptr &&
        tlsEcs.actorRegistry.all_of<PlayerSkillListComp, PlayerUint32Comp>(player);
    const bool inBattle = PlayerBattleSystem::IsInBattle(player);
    summary.set_in_battle(inBattle);
    bool complete = inputs && equipment.support_sources_complete() && !inBattle;
    summary.mutable_healing()->set_complete(complete);
    summary.mutable_shield()->set_complete(complete);
    if (!inputs) return;
    for (const auto& owned : tlsEcs.actorRegistry.get<PlayerSkillListComp>(player).skill_list())
        if (owned.rank() > 0 && SkillTableManager::Instance().FindByIdSilent(owned.skill_table_id()).first == nullptr &&
            !supportrules::IsRetiredSkill(owned.skill_table_id()))
            complete = false;
    const auto level = playerlevel::CappedLevel(levelComp->level());
    BattlePlayerSnapshot snapshot;
    PlayerSkillSystem::FillBattleSnapshot(player, snapshot);
    const std::map<uint32_t, uint32_t> ranks(snapshot.skill_ranks().begin(), snapshot.skill_ranks().end());
    supportrules::ModifierSelection healing, shield, mana;
    double legacyHealing = 0;
    for (const auto& [skillId, rank] : ranks) {
        const auto* skill = SkillTableManager::Instance().FindByIdSilent(skillId).first;
        if (skill == nullptr || rank == 0) continue;
        if (classskill::ParsePassiveStat(skill->passive_stat()) == classskill::PassiveStat::kOutgoingHeal) {
            const double value = skill->passive_percent() * classskill::PassiveRankFactor(rank);
            legacyHealing += value;
            auto* source = summary.mutable_healing()->add_sources();
            source->set_effect_kind(1);
            source->set_kind(2); source->set_skill_table_id(skillId); source->set_name(skill->name());
            source->set_raw_bonus_bps(value * 10000); source->set_effective_bonus_bps(value * 10000);
            source->set_infinite(true);
        }
        if (skill->battle_model() != 1 || skill->ability_kind() != 2) continue;
        for (const auto id : skill->init_effect()) {
            const auto* buff = BuffTableManager::Instance().FindByIdSilent(id).first;
            if (buff == nullptr) { complete = false; continue; }
            if (buff->battle_op() != static_cast<uint32_t>(turnbattle::BattleEffectOp::Modifier) ||
                buff->trigger_event() != 0) continue;
            const auto modifier = static_cast<turnbattle::BattleModifier>(buff->modifier_stat());
            if (modifier == turnbattle::BattleModifier::ManaCost) { mana.Consider(buff->ratio()); continue; }
            if (modifier != turnbattle::BattleModifier::OutgoingHeal && modifier != turnbattle::BattleModifier::OutgoingShield) continue;
            auto* part = modifier == turnbattle::BattleModifier::OutgoingShield ? summary.mutable_shield() : summary.mutable_healing();
            const int index = part->sources_size();
            auto* source = part->add_sources(); source->set_kind(2); source->set_skill_table_id(skillId);
            source->set_effect_kind(modifier == turnbattle::BattleModifier::OutgoingShield ? 2 : 1);
            source->set_name(skill->name()); source->set_raw_bonus_bps(buff->ratio() * 10000);
            source->set_infinite(buff->infinite_duration() != 0); source->set_reason("同类更强修正优先生效");
            (modifier == turnbattle::BattleModifier::OutgoingShield ? shield : healing).Consider(buff->ratio(), index);
        }
    }
    healing.Project(*summary.mutable_healing()); shield.Project(*summary.mutable_shield());
    const double healingBonus = legacyHealing + healing.Total() + equipment.outgoing_heal_bps() / 10000.0;
    const double shieldBonus = equipment.shield_capacity_bps() / 10000.0 + shield.Total();
    summary.mutable_healing()->set_total_bonus_bps(healingBonus * 10000);
    summary.mutable_shield()->set_total_bonus_bps(shieldBonus * 10000);
    std::vector<const SkillTable*> freeSkills;
    for (const auto& skill : SkillTableManager::Instance().FindAll().data()) {
        if (!PlayerSkillSystem::IsFreeSkillFor(skill, 1)) continue;
        freeSkills.push_back(&skill);
        for (const auto id : skill.effect()) {
            if (BuffTableManager::Instance().FindByIdSilent(id).first == nullptr) complete = false;
        }
    }
    std::sort(freeSkills.begin(), freeSkills.end(), [](const SkillTable* lhs, const SkillTable* rhs) {
        return lhs->id() < rhs->id();
    });
    summary.mutable_healing()->set_complete(complete);
    summary.mutable_shield()->set_complete(complete);
    for (const auto* skill : freeSkills) {
        for (const auto id : skill->effect()) {
            const auto* buff = BuffTableManager::Instance().FindByIdSilent(id).first;
            if (buff == nullptr) continue;
            const bool isShield = buff->battle_op() == static_cast<uint32_t>(turnbattle::BattleEffectOp::Shield);
            const auto category = equipbattle::ManaCategory(*skill, [](uint32_t buffId) {
                return BuffTableManager::Instance().FindByIdSilent(buffId).first;
            });
            const auto reduction = equipment.mana_reduction_bps().find(category);
            const double discount = (reduction == equipment.mana_reduction_bps().end() ? 0 : reduction->second) - mana.Total() * 10000;
            const auto rank = ranks.find(skill->id());
            SupportSkillPreview preview;
            if (supportrules::FillPreview(*skill, *buff, level, rank == ranks.end() ? 0 : rank->second,
                attributes->max_mana(), isShield ? shieldBonus : healingBonus,
                discount, complete, preview)) { *summary.add_previews() = preview; break; }
        }
    }
}

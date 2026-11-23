#pragma once

#include <algorithm>
#include <array>
#include <cmath>
#include <cstdint>
#include <limits>
#include <set>
#include <sstream>
#include "class_skill_rules.h"
#include "equipment_battle_rules.h"
#include "table/proto/skill_table.pb.h"
#include "table/proto/buff_table.pb.h"
#include "proto/common/component/equipment_effects.pb.h"
#include "proto/common/component/support_effects.pb.h"

// 治疗/护盾的同一数值层，结算与只读投影共同调用；不读取ECS、不修改战斗状态。
namespace supportrules {
// 首测删行白名单也供正式目录校验读取。只用于解释缺行，不修改旧档或授予技能。
inline bool IsRetiredSkill(uint32_t skillId) {
    constexpr auto retired = std::to_array<uint32_t>({
#include "../constants/retired_skill_ids.inc"
    });
    return std::find(retired.begin(), retired.end(), skillId) != retired.end();
}

inline double BaseValue(double flat, double perLevel, uint32_t level, double basis,
                        double ratio, uint32_t rank) {
    const double value = std::max(0.0, flat + perLevel * level + basis * ratio) *
        classskill::SupportRankFactor(rank);
    return std::isfinite(value) ? value : 0.0;
}
inline double ApplyBonus(double base, double bonus) { return base * std::max(0.0, 1.0 + bonus); }
inline uint64_t Whole(double value) {
    if (!(value > 0) || !std::isfinite(value)) return 0;
    return value >= static_cast<double>(std::numeric_limits<uint64_t>::max()) ?
        std::numeric_limits<uint64_t>::max() : static_cast<uint64_t>(value);
}

// 同类修正取一个最强正项加一个最强负项；索引仅用于该次来源投影，非持久状态。
struct ModifierSelection {
    double gain = 0, loss = 0;
    int gainIndex = -1, lossIndex = -1;
    void Consider(double value, int index = -1) {
        if (!std::isfinite(value)) return;
        if (value > gain) { gain = value; gainIndex = index; }
        if (value < loss) { loss = value; lossIndex = index; }
    }
    double Total() const { return gain + loss; }
    void Project(SupportEffectBreakdown& output) const {
        for (const int index : {gainIndex, lossIndex}) if (index >= 0) {
            auto* source = output.mutable_sources(index);
            source->set_effective_bonus_bps(source->raw_bonus_bps());
            source->clear_reason();
        }
    }
};

inline void Begin(const EquipmentEffects& equipment, SupportEffectSummary& output) {
    output.Clear();
    for (const bool shield : {false, true}) {
        auto* part = shield ? output.mutable_shield() : output.mutable_healing();
        part->set_complete(equipment.support_sources_complete());
        part->set_equipment_cap_bps(equipbattle::MaximumSourceBps(shield ? 6 : 5));
        part->set_total_bonus_bps(shield ? equipment.shield_capacity_bps() : equipment.outgoing_heal_bps());
        for (const auto& source : equipment.support_sources()) {
            if (source.effect_kind() != (shield ? 2u : 1u)) continue;
            *part->add_sources() = source;
            if (source.source_cap_bps() > 0)
                part->set_equipment_cap_bps(std::min(part->equipment_cap_bps(), source.source_cap_bps()));
        }
        for (const auto& artifact : equipment.artifacts()) {
            if (artifact.effect_id() != (shield ? 5u : 4u)) continue;
            auto* source = part->add_sources();
            source->set_kind(4); source->set_item_id(artifact.item_id());
            source->set_name(artifact.name()); source->set_raw_bonus_bps(artifact.bonus_bps());
            source->set_conditional(true); source->set_effect_kind(shield ? 2 : 1);
            std::ostringstream condition;
            if (shield) condition << "主目标首个独立容量盾";
            else condition << "主目标首段即时治疗且气血≤" << artifact.health_threshold_bps() / 100.0 << "%";
            if (artifact.per_round_limit() > 0) condition << "；每回合" << artifact.per_round_limit() << "次";
            if (artifact.battle_limit() > 0) condition << "；每场" << artifact.battle_limit() << "次";
            condition << "；不计常驻总计";
            source->set_reason(condition.str());
        }
    }
}

inline bool ValidEquipmentSources(const EquipmentEffects& equipment) {
    if (!equipment.support_sources_complete()) return false;
    std::set<std::pair<uint64_t, uint32_t>> seen;
    double healing = 0, shield = 0;
    for (const auto& source : equipment.support_sources()) {
        if (source.kind() != 1 || source.item_id() == 0 || source.affix_id() == 0 ||
            !seen.emplace(source.item_id(), source.affix_id()).second ||
            (source.effect_kind() != 1 && source.effect_kind() != 2) ||
            !std::isfinite(source.raw_bonus_bps()) || !std::isfinite(source.effective_bonus_bps()) ||
            source.raw_bonus_bps() < 0 || source.effective_bonus_bps() < 0 ||
            source.effective_bonus_bps() > source.raw_bonus_bps() || source.conditional()) return false;
        (source.effect_kind() == 2 ? shield : healing) += source.effective_bonus_bps();
    }
    return healing == equipment.outgoing_heal_bps() && shield == equipment.shield_capacity_bps();
}

inline bool FillPreview(const SkillTable& skill, const BuffTable& effect, uint32_t level,
                        uint32_t effectiveRank, uint64_t maxMana, double bonus,
                        double manaReductionBps, bool complete, SupportSkillPreview& output) {
    const uint32_t kind = effect.battle_op() == 2 ? 1 : effect.battle_op() == 8 ? 3 :
        effect.battle_op() == 9 && effect.hit_limit() == 0 ? 2 : 0;
    // 依赖受益者、资源实付或伤害结果的效果没有无目标精确基础值。
    if (skill.battle_model() != 1 || kind == 0 || effect.value_basis() != 0) return false;
    output.Clear(); output.set_skill_table_id(skill.id()); output.set_effect_kind(kind);
    output.set_reference_rank(std::max(1u, effectiveRank)); output.set_learned(effectiveRank > 0);
    output.set_complete(complete);
    const double base = BaseValue(effect.flat_value(), effect.per_level(), level, 0, effect.ratio(), output.reference_rank());
    output.set_base_value(base); output.set_outgoing_bonus_bps(bonus * 10000);
    output.set_reference_value(Whole(ApplyBonus(base, bonus)));
    output.set_original_mana_cost(classskill::ManaCost(maxMana, skill.mana_percent()));
    output.set_mana_cost(equipbattle::DiscountMana(output.original_mana_cost(), manaReductionBps));
    output.set_scope(kind == 2 ? "单目标初始容量；不含条件法宝，不读取缺血或减疗" :
        kind == 3 ? "每次恢复的中立值；不含缺血封顶和目标增减疗，不含复活血量" :
        "单目标中立治疗；不含缺血封顶、目标增减疗、残血条件和法宝");
    return true;
}
} // namespace supportrules

#pragma once

#include <algorithm>
#include <cstdint>

// 伤害数值纯规则:回合引擎(turn_battle_engine.cpp)与实时技能(scene 的 combat/skill/system/skill.cpp)共用,
// 零 ECS / 零表 / 零 proto 依赖。暴击掷骰、DEFEND 减半、PVP 系数与事件落地留给各自运行时。
// 设计文档 docs/design/player-attribute-allocation.md §3.2(2026-09-13 用户批准的"防御按比例减伤"方案)。
//
//   原始伤害 = 基础伤害 × (1 + 力量 × 0.1) + 攻击 × 攻击倍率
//   受伤比例 = max(1 − 60%, 等级系数 ÷ (护甲 + 防御 + 等级系数) × (1 − 抗性%))
//   等级系数 = 360 + 120 × 目标等级(目标等级夹到 1..85;2026-09-14 防御单位 ×12,原 30 + 10 × 等级)
//   伤害     = 原始伤害 × 受伤比例
//
// 为什么不再用"攻击减防御":减法口径下防御一旦超过攻击,伤害直接归零(2026-09-10 系数上调后低级怪全体打不动人);
// 比例口径下防御收益递减,常驻减伤封顶 60%,任何命中至少打出四成。
//
// 单测:cpp/tests/turn_battle_engine_test/combat_damage_rules_test.cpp。
namespace combatdamage {

// SkillTable.damage_type:技能伤害吃哪项攻击(与 skill_type 表示的施法行为无关)
inline constexpr uint32_t kMagicDamage = 0;     // 法术:吃法伤(表缺省值,老技能行保持原行为)
inline constexpr uint32_t kPhysicalDamage = 1;  // 物理:吃物伤

// 常驻减伤(护甲 + 防御 + 抗性)上限;主动防御(DEFEND 减半)不算在内,由引擎另外处理
inline constexpr double kMaxPassiveReduction = 0.60;
// 防御单位倍率(2026-09-14 ×12,让每分配 1 点体质面板至少 +1 防御):护甲、防御、等级系数必须同乘,
// 受伤比例 K ÷ (护甲 + 防御 + K) 才保持不变(三者都是整数时逐位相同)。抗性是百分比,不乘。
// 改倍率要连带 Class.init_armor、Monster.armor、AttributeDimension 101 / 401 的 defense 系数与 kMonsterDefaultArmor。
inline constexpr double kDefenseUnitScale = 12.0;
inline constexpr double kLevelFactorBase = 30.0 * kDefenseUnitScale;      // 360
inline constexpr double kLevelFactorPerLevel = 10.0 * kDefenseUnitScale;  // 120
// 目标等级上限。镜像 scene 的 playerlevel::kMaxLevel(battle 库不 include scene 头文件),单测守住两边一致
inline constexpr uint32_t kLevelFactorMaxLevel = 85;

inline uint64_t SelectAttack(uint32_t damageType, uint64_t physicalAttack, uint64_t magicAttack) {
    if (damageType == kPhysicalDamage) return physicalAttack;
    if (damageType == kMagicDamage) return magicAttack;
    return 0;  // 未知类型不能悄悄吃到任一攻击
}

// 表里未配(proto 缺省 0)按 1 倍;负数与非有限数是坏表,不给攻击加成
inline double AttackMultiplier(double configured) {
    if (!std::isfinite(configured) || configured < 0.0) return 0.0;
    return configured == 0.0 ? 1.0 : configured;
}

inline double LevelFactor(uint32_t targetLevel) {
    const uint32_t level = std::clamp(targetLevel, uint32_t{1}, kLevelFactorMaxLevel);
    return kLevelFactorBase + kLevelFactorPerLevel * static_cast<double>(level);
}

// 受伤比例,落在 [1 − kMaxPassiveReduction, 1]
inline double ReceivedRatio(uint64_t armor, uint64_t defense, uint64_t resistancePercent, uint32_t targetLevel) {
    // 先转 double 再求和:两个 uint64 直接相加可能溢出成小数,那样几乎不减伤
    const double combinedDefense = static_cast<double>(armor) + static_cast<double>(defense);
    const double levelFactor = LevelFactor(targetLevel);
    const double resistance = std::clamp(static_cast<double>(resistancePercent) / 100.0, 0.0, 1.0);
    return std::max(1.0 - kMaxPassiveReduction,
                    levelFactor / (combinedDefense + levelFactor) * (1.0 - resistance));
}

inline double DamageBeforeCritical(double baseDamage, uint64_t legacyStrength,
                                   uint64_t attack, double attackMultiplier,
                                   uint64_t armor, uint64_t defense,
                                   uint64_t resistancePercent, uint32_t targetLevel) {
    // 基础伤害 0 = 纯辅助技能:攻击再高也不能把治疗 / 控制变成伤害
    if (!std::isfinite(baseDamage) || baseDamage <= 0.0) return 0.0;
    const double raw = baseDamage * (1.0 + static_cast<double>(legacyStrength) * 0.1)
        + static_cast<double>(attack) * AttackMultiplier(attackMultiplier);
    if (!std::isfinite(raw) || raw <= 0.0) return 0.0;
    return raw * ReceivedRatio(armor, defense, resistancePercent, targetLevel);
}

// 落到气血上的整数伤害:向上取整、不超过当前气血;NaN / 无穷 / 非正数一律 0,不让它们进整数转换
inline uint64_t DamageToHealth(double rawDamage, uint64_t health) {
    if (!std::isfinite(rawDamage) || rawDamage <= 0.0 || health == 0) return 0;
    const double rounded = std::ceil(rawDamage);
    if (rounded >= static_cast<double>(health)) return health;
    return static_cast<uint64_t>(rounded);
}

}  // namespace combatdamage

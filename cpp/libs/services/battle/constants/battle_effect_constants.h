#pragma once
#include <cstdint>

// Skill/Buff表的机制枚举。策划编号只用于追溯,从不参与运行时分支。
namespace turnbattle {
enum class BattleEffectOp : uint32_t {
    None = 0, Damage = 1, Heal = 2, RestoreMana = 3, DispelDebuff = 4,
    DispelBuff = 5, Modifier = 6, PeriodicDamage = 7, ReviveMark = 8,
    Shield = 9, ManaShield = 10, ShareDamage = 11, ReflectDamage = 12,
    Control = 13, ConsumePeriodic = 14, ExtendPeriodic = 15, DrainMana = 16,
    RetreatPet = 17, TemporaryHealth = 18, MirrorSkill = 19, PursuitDamage = 20,
    ApplyChildren = 21, ConditionalHeal = 22, ConditionalMana = 23,
    EvasionCharge = 24, GrowingAura = 25, CopyTargetSkill = 26,
    PrepareCast = 27, HitVulnerability = 28, ControlImmunity = 29, kCount
};
enum class BattleModifier : uint32_t {
    None = 0, PhysicalAttack = 1, MagicAttack = 2, Defense = 3, Speed = 4,
    OutgoingDamage = 5, IncomingHeal = 6, Evasion = 7, OutgoingHeal = 8,
    ManaCost = 9, MagicReduction = 10, RetreatResist = 11, ControlResist = 12,
    LaterHitReduction = 13, NextDirectDamage = 14, AdaptiveDamage = 15,
    IncomingDamage = 16, OutgoingShield = 17, OutgoingPhysicalDamage = 18,
    OutgoingMagicDamage = 19, SelfGrowingDefense = 20,
    NextPhysicalAttack = 21, NextMagicAttack = 22,
    ForbidPhysical = 23, ForbidMagic = 24,
    SelfGrowingDirectDamage = 25, DefensePenetration = 26,
    PeriodicDamageBonus = 27, AllControlResist = 28, kCount
};
enum class SecondaryTargetPolicy : uint32_t {
    Random = 0,
    HighestPhysicalAttack = 1,
    HighestMagicAttack = 2,
    LowestHealthValue = 3,
    LowestHealthRatio = 4,
    LowestPhysicalDefense = 5,
    LowestMagicDefense = 6,
    LowestRoundSpeed = 7,
    kCount
};
enum class BattleDamageOrigin : uint32_t {
    Direct = 0, Periodic = 1, Reflect = 2, Redirect = 3, ResourceCost = 4,
    Pursuit = 5, Mirror = 6, Confusion = 7, kCount
};
enum class BattleTrigger : uint32_t {
    Start = 0, AfterBasic = 1, AfterSkill = 2, AfterTake = 3,
    AfterPeriodic = 4, AfterDodge = 5, RoundStart = 6,
    AfterDirectAttack = 7, AfterControlAttempt = 8, AfterSupportCast = 9, kCount
};
enum class BattleStateKind : uint32_t {
    None = 0, SealActive = 1, SealMagic = 2, SealPhysical = 3,
    Confusion = 4, Sleep = 5, Freeze = 6, Poison = 7, Burn = 8, Retreat = 9, kCount
};
inline constexpr uint32_t kActionTagPhysical = 1;
inline constexpr uint32_t kActionTagMagic = 2;
inline constexpr uint32_t kActionTagHeal = 4;
inline constexpr uint32_t kActionTagSupport = 8;
inline constexpr uint32_t kActionTagControl = 16;
}  // namespace turnbattle

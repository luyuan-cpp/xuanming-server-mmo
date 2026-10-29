
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for SkillTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type SkillIdComp struct {
    Value uint32
}

type SkillRequire_targetComp struct {
    Value uint32
}

type SkillTarget_statusComp struct {
    Value uint32
}

type SkillCast_pointComp struct {
    Value float64
}

type SkillRecovery_timeComp struct {
    Value float64
}

type SkillImmediateComp struct {
    Value uint32
}

type SkillChannel_thinkComp struct {
    Value uint32
}

type SkillChannel_finishComp struct {
    Value uint32
}

type SkillThink_intervalComp struct {
    Value uint32
}

type SkillChannel_timeComp struct {
    Value uint32
}

type SkillRangeComp struct {
    Value float64
}

type SkillMax_rangeComp struct {
    Value float64
}

type SkillMin_rangeComp struct {
    Value float64
}

type SkillSelf_statusComp struct {
    Value uint32
}

type SkillRequired_statusComp struct {
    Value uint32
}

type SkillCooldown_idComp struct {
    Value uint32
}

type SkillDamageComp struct {
    Value string
}

type SkillDamage_typeComp struct {
    Value uint32
}

type SkillAttack_multiplierComp struct {
    Value float64
}

type SkillClass_idComp struct {
    Value uint32
}

type SkillNameComp struct {
    Value string
}

type SkillDescriptionComp struct {
    Value string
}

type SkillIcon_keyComp struct {
    Value string
}

type SkillCategoryComp struct {
    Value string
}

type SkillUnlock_levelComp struct {
    Value uint32
}

type SkillMax_rankComp struct {
    Value uint32
}

type SkillTarget_teamComp struct {
    Value uint32
}

type SkillMax_targetsComp struct {
    Value uint32
}

type SkillMana_percentComp struct {
    Value float64
}

type SkillCooldown_roundsComp struct {
    Value uint32
}

type SkillHeal_multiplierComp struct {
    Value float64
}

type SkillHeal_per_levelComp struct {
    Value float64
}

type SkillExecute_heal_thresholdComp struct {
    Value float64
}

type SkillExecute_heal_bonusComp struct {
    Value float64
}

type SkillRestore_mana_percentComp struct {
    Value float64
}

type SkillDispel_countComp struct {
    Value uint32
}

type SkillInitiativeComp struct {
    Value int32
}

type SkillPassive_statComp struct {
    Value string
}

type SkillPassive_percentComp struct {
    Value float64
}

type SkillBuff_percentComp struct {
    Value float64
}

type SkillSkill_typeComp struct {
    Values []uint32
}

type SkillTargeting_modeComp struct {
    Values []uint32
}

type SkillEffectComp struct {
    Values []uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeSkillIdComp(row *pb.SkillTable) SkillIdComp {
    return SkillIdComp{Value: row.Id}
}

func MakeSkillRequire_targetComp(row *pb.SkillTable) SkillRequire_targetComp {
    return SkillRequire_targetComp{Value: row.RequireTarget}
}

func MakeSkillTarget_statusComp(row *pb.SkillTable) SkillTarget_statusComp {
    return SkillTarget_statusComp{Value: row.TargetStatus}
}

func MakeSkillCast_pointComp(row *pb.SkillTable) SkillCast_pointComp {
    return SkillCast_pointComp{Value: row.CastPoint}
}

func MakeSkillRecovery_timeComp(row *pb.SkillTable) SkillRecovery_timeComp {
    return SkillRecovery_timeComp{Value: row.RecoveryTime}
}

func MakeSkillImmediateComp(row *pb.SkillTable) SkillImmediateComp {
    return SkillImmediateComp{Value: row.Immediate}
}

func MakeSkillChannel_thinkComp(row *pb.SkillTable) SkillChannel_thinkComp {
    return SkillChannel_thinkComp{Value: row.ChannelThink}
}

func MakeSkillChannel_finishComp(row *pb.SkillTable) SkillChannel_finishComp {
    return SkillChannel_finishComp{Value: row.ChannelFinish}
}

func MakeSkillThink_intervalComp(row *pb.SkillTable) SkillThink_intervalComp {
    return SkillThink_intervalComp{Value: row.ThinkInterval}
}

func MakeSkillChannel_timeComp(row *pb.SkillTable) SkillChannel_timeComp {
    return SkillChannel_timeComp{Value: row.ChannelTime}
}

func MakeSkillRangeComp(row *pb.SkillTable) SkillRangeComp {
    return SkillRangeComp{Value: row.Range}
}

func MakeSkillMax_rangeComp(row *pb.SkillTable) SkillMax_rangeComp {
    return SkillMax_rangeComp{Value: row.MaxRange}
}

func MakeSkillMin_rangeComp(row *pb.SkillTable) SkillMin_rangeComp {
    return SkillMin_rangeComp{Value: row.MinRange}
}

func MakeSkillSelf_statusComp(row *pb.SkillTable) SkillSelf_statusComp {
    return SkillSelf_statusComp{Value: row.SelfStatus}
}

func MakeSkillRequired_statusComp(row *pb.SkillTable) SkillRequired_statusComp {
    return SkillRequired_statusComp{Value: row.RequiredStatus}
}

func MakeSkillCooldown_idComp(row *pb.SkillTable) SkillCooldown_idComp {
    return SkillCooldown_idComp{Value: row.CooldownId}
}

func MakeSkillDamageComp(row *pb.SkillTable) SkillDamageComp {
    return SkillDamageComp{Value: row.Damage}
}

func MakeSkillDamage_typeComp(row *pb.SkillTable) SkillDamage_typeComp {
    return SkillDamage_typeComp{Value: row.DamageType}
}

func MakeSkillAttack_multiplierComp(row *pb.SkillTable) SkillAttack_multiplierComp {
    return SkillAttack_multiplierComp{Value: row.AttackMultiplier}
}

func MakeSkillClass_idComp(row *pb.SkillTable) SkillClass_idComp {
    return SkillClass_idComp{Value: row.ClassId}
}

func MakeSkillNameComp(row *pb.SkillTable) SkillNameComp {
    return SkillNameComp{Value: row.Name}
}

func MakeSkillDescriptionComp(row *pb.SkillTable) SkillDescriptionComp {
    return SkillDescriptionComp{Value: row.Description}
}

func MakeSkillIcon_keyComp(row *pb.SkillTable) SkillIcon_keyComp {
    return SkillIcon_keyComp{Value: row.IconKey}
}

func MakeSkillCategoryComp(row *pb.SkillTable) SkillCategoryComp {
    return SkillCategoryComp{Value: row.Category}
}

func MakeSkillUnlock_levelComp(row *pb.SkillTable) SkillUnlock_levelComp {
    return SkillUnlock_levelComp{Value: row.UnlockLevel}
}

func MakeSkillMax_rankComp(row *pb.SkillTable) SkillMax_rankComp {
    return SkillMax_rankComp{Value: row.MaxRank}
}

func MakeSkillTarget_teamComp(row *pb.SkillTable) SkillTarget_teamComp {
    return SkillTarget_teamComp{Value: row.TargetTeam}
}

func MakeSkillMax_targetsComp(row *pb.SkillTable) SkillMax_targetsComp {
    return SkillMax_targetsComp{Value: row.MaxTargets}
}

func MakeSkillMana_percentComp(row *pb.SkillTable) SkillMana_percentComp {
    return SkillMana_percentComp{Value: row.ManaPercent}
}

func MakeSkillCooldown_roundsComp(row *pb.SkillTable) SkillCooldown_roundsComp {
    return SkillCooldown_roundsComp{Value: row.CooldownRounds}
}

func MakeSkillHeal_multiplierComp(row *pb.SkillTable) SkillHeal_multiplierComp {
    return SkillHeal_multiplierComp{Value: row.HealMultiplier}
}

func MakeSkillHeal_per_levelComp(row *pb.SkillTable) SkillHeal_per_levelComp {
    return SkillHeal_per_levelComp{Value: row.HealPerLevel}
}

func MakeSkillExecute_heal_thresholdComp(row *pb.SkillTable) SkillExecute_heal_thresholdComp {
    return SkillExecute_heal_thresholdComp{Value: row.ExecuteHealThreshold}
}

func MakeSkillExecute_heal_bonusComp(row *pb.SkillTable) SkillExecute_heal_bonusComp {
    return SkillExecute_heal_bonusComp{Value: row.ExecuteHealBonus}
}

func MakeSkillRestore_mana_percentComp(row *pb.SkillTable) SkillRestore_mana_percentComp {
    return SkillRestore_mana_percentComp{Value: row.RestoreManaPercent}
}

func MakeSkillDispel_countComp(row *pb.SkillTable) SkillDispel_countComp {
    return SkillDispel_countComp{Value: row.DispelCount}
}

func MakeSkillInitiativeComp(row *pb.SkillTable) SkillInitiativeComp {
    return SkillInitiativeComp{Value: row.Initiative}
}

func MakeSkillPassive_statComp(row *pb.SkillTable) SkillPassive_statComp {
    return SkillPassive_statComp{Value: row.PassiveStat}
}

func MakeSkillPassive_percentComp(row *pb.SkillTable) SkillPassive_percentComp {
    return SkillPassive_percentComp{Value: row.PassivePercent}
}

func MakeSkillBuff_percentComp(row *pb.SkillTable) SkillBuff_percentComp {
    return SkillBuff_percentComp{Value: row.BuffPercent}
}

func MakeSkillSkill_typeComp(row *pb.SkillTable) SkillSkill_typeComp {
    return SkillSkill_typeComp{Values: row.SkillType}
}

func MakeSkillTargeting_modeComp(row *pb.SkillTable) SkillTargeting_modeComp {
    return SkillTargeting_modeComp{Values: row.TargetingMode}
}

func MakeSkillEffectComp(row *pb.SkillTable) SkillEffectComp {
    return SkillEffectComp{Values: row.Effect}
}


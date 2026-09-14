
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

func MakeSkillSkill_typeComp(row *pb.SkillTable) SkillSkill_typeComp {
    return SkillSkill_typeComp{Values: row.SkillType}
}

func MakeSkillTargeting_modeComp(row *pb.SkillTable) SkillTargeting_modeComp {
    return SkillTargeting_modeComp{Values: row.TargetingMode}
}

func MakeSkillEffectComp(row *pb.SkillTable) SkillEffectComp {
    return SkillEffectComp{Values: row.Effect}
}


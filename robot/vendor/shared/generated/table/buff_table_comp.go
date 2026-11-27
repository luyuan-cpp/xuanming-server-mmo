
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for BuffTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type BuffIdComp struct {
    Value uint32
}

type BuffNo_casterComp struct {
    Value uint32
}

type BuffBuff_typeComp struct {
    Value uint32
}

type BuffLevelComp struct {
    Value uint32
}

type BuffMax_layerComp struct {
    Value uint32
}

type BuffInfinite_durationComp struct {
    Value uint32
}

type BuffDurationComp struct {
    Value float64
}

type BuffForce_interruptComp struct {
    Value uint32
}

type BuffIntervalComp struct {
    Value float64
}

type BuffInterval_countComp struct {
    Value uint32
}

type BuffMovement_speed_boostComp struct {
    Value float64
}

type BuffMovement_speed_reductionComp struct {
    Value float64
}

type BuffHealth_regenerationComp struct {
    Value string
}

type BuffCombat_idle_secondsComp struct {
    Value float64
}

type BuffTimeComp struct {
    Value uint32
}

type BuffBonus_damageComp struct {
    Value string
}

type BuffBattle_opComp struct {
    Value uint32
}

type BuffTrigger_eventComp struct {
    Value uint32
}

type BuffEffect_targetComp struct {
    Value uint32
}

type BuffFlat_valueComp struct {
    Value float64
}

type BuffPer_levelComp struct {
    Value float64
}

type BuffRatioComp struct {
    Value float64
}

type BuffValue_basisComp struct {
    Value uint32
}

type BuffDamage_filterComp struct {
    Value uint32
}

type BuffDuration_roundsComp struct {
    Value uint32
}

type BuffTick_countComp struct {
    Value uint32
}

type BuffTrigger_chanceComp struct {
    Value float64
}

type BuffPer_round_limitComp struct {
    Value uint32
}

type BuffHit_limitComp struct {
    Value uint32
}

type BuffModifier_statComp struct {
    Value uint32
}

type BuffState_kindComp struct {
    Value uint32
}

type BuffApplication_chanceComp struct {
    Value float64
}

type BuffRequired_tagComp struct {
    Value string
}

type BuffSecond_required_tagComp struct {
    Value string
}

type BuffConditional_bonusComp struct {
    Value float64
}

type BuffSecondary_ratioComp struct {
    Value float64
}

type BuffEffect_countComp struct {
    Value uint32
}

type BuffTarget_countComp struct {
    Value uint32
}

type BuffPet_target_countComp struct {
    Value uint32
}

type BuffStack_keyComp struct {
    Value uint32
}

type BuffRefresh_policyComp struct {
    Value uint32
}

type BuffKeep_on_deathComp struct {
    Value uint32
}

type BuffEffect_priorityComp struct {
    Value uint32
}

type BuffStart_roundComp struct {
    Value uint32
}

type BuffStack_limitComp struct {
    Value uint32
}

type BuffRequire_living_casterComp struct {
    Value uint32
}

type BuffTick_on_applyComp struct {
    Value uint32
}

type BuffKeep_on_logoutComp struct {
    Value uint32
}

type BuffInterval_effectComp struct {
    Values []float64
}

type BuffSub_buffComp struct {
    Values []uint32
}

type BuffTarget_sub_buffComp struct {
    Values []uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeBuffIdComp(row *pb.BuffTable) BuffIdComp {
    return BuffIdComp{Value: row.Id}
}

func MakeBuffNo_casterComp(row *pb.BuffTable) BuffNo_casterComp {
    return BuffNo_casterComp{Value: row.NoCaster}
}

func MakeBuffBuff_typeComp(row *pb.BuffTable) BuffBuff_typeComp {
    return BuffBuff_typeComp{Value: row.BuffType}
}

func MakeBuffLevelComp(row *pb.BuffTable) BuffLevelComp {
    return BuffLevelComp{Value: row.Level}
}

func MakeBuffMax_layerComp(row *pb.BuffTable) BuffMax_layerComp {
    return BuffMax_layerComp{Value: row.MaxLayer}
}

func MakeBuffInfinite_durationComp(row *pb.BuffTable) BuffInfinite_durationComp {
    return BuffInfinite_durationComp{Value: row.InfiniteDuration}
}

func MakeBuffDurationComp(row *pb.BuffTable) BuffDurationComp {
    return BuffDurationComp{Value: row.Duration}
}

func MakeBuffForce_interruptComp(row *pb.BuffTable) BuffForce_interruptComp {
    return BuffForce_interruptComp{Value: row.ForceInterrupt}
}

func MakeBuffIntervalComp(row *pb.BuffTable) BuffIntervalComp {
    return BuffIntervalComp{Value: row.Interval}
}

func MakeBuffInterval_countComp(row *pb.BuffTable) BuffInterval_countComp {
    return BuffInterval_countComp{Value: row.IntervalCount}
}

func MakeBuffMovement_speed_boostComp(row *pb.BuffTable) BuffMovement_speed_boostComp {
    return BuffMovement_speed_boostComp{Value: row.MovementSpeedBoost}
}

func MakeBuffMovement_speed_reductionComp(row *pb.BuffTable) BuffMovement_speed_reductionComp {
    return BuffMovement_speed_reductionComp{Value: row.MovementSpeedReduction}
}

func MakeBuffHealth_regenerationComp(row *pb.BuffTable) BuffHealth_regenerationComp {
    return BuffHealth_regenerationComp{Value: row.HealthRegeneration}
}

func MakeBuffCombat_idle_secondsComp(row *pb.BuffTable) BuffCombat_idle_secondsComp {
    return BuffCombat_idle_secondsComp{Value: row.CombatIdleSeconds}
}

func MakeBuffTimeComp(row *pb.BuffTable) BuffTimeComp {
    return BuffTimeComp{Value: row.Time}
}

func MakeBuffBonus_damageComp(row *pb.BuffTable) BuffBonus_damageComp {
    return BuffBonus_damageComp{Value: row.BonusDamage}
}

func MakeBuffBattle_opComp(row *pb.BuffTable) BuffBattle_opComp {
    return BuffBattle_opComp{Value: row.BattleOp}
}

func MakeBuffTrigger_eventComp(row *pb.BuffTable) BuffTrigger_eventComp {
    return BuffTrigger_eventComp{Value: row.TriggerEvent}
}

func MakeBuffEffect_targetComp(row *pb.BuffTable) BuffEffect_targetComp {
    return BuffEffect_targetComp{Value: row.EffectTarget}
}

func MakeBuffFlat_valueComp(row *pb.BuffTable) BuffFlat_valueComp {
    return BuffFlat_valueComp{Value: row.FlatValue}
}

func MakeBuffPer_levelComp(row *pb.BuffTable) BuffPer_levelComp {
    return BuffPer_levelComp{Value: row.PerLevel}
}

func MakeBuffRatioComp(row *pb.BuffTable) BuffRatioComp {
    return BuffRatioComp{Value: row.Ratio}
}

func MakeBuffValue_basisComp(row *pb.BuffTable) BuffValue_basisComp {
    return BuffValue_basisComp{Value: row.ValueBasis}
}

func MakeBuffDamage_filterComp(row *pb.BuffTable) BuffDamage_filterComp {
    return BuffDamage_filterComp{Value: row.DamageFilter}
}

func MakeBuffDuration_roundsComp(row *pb.BuffTable) BuffDuration_roundsComp {
    return BuffDuration_roundsComp{Value: row.DurationRounds}
}

func MakeBuffTick_countComp(row *pb.BuffTable) BuffTick_countComp {
    return BuffTick_countComp{Value: row.TickCount}
}

func MakeBuffTrigger_chanceComp(row *pb.BuffTable) BuffTrigger_chanceComp {
    return BuffTrigger_chanceComp{Value: row.TriggerChance}
}

func MakeBuffPer_round_limitComp(row *pb.BuffTable) BuffPer_round_limitComp {
    return BuffPer_round_limitComp{Value: row.PerRoundLimit}
}

func MakeBuffHit_limitComp(row *pb.BuffTable) BuffHit_limitComp {
    return BuffHit_limitComp{Value: row.HitLimit}
}

func MakeBuffModifier_statComp(row *pb.BuffTable) BuffModifier_statComp {
    return BuffModifier_statComp{Value: row.ModifierStat}
}

func MakeBuffState_kindComp(row *pb.BuffTable) BuffState_kindComp {
    return BuffState_kindComp{Value: row.StateKind}
}

func MakeBuffApplication_chanceComp(row *pb.BuffTable) BuffApplication_chanceComp {
    return BuffApplication_chanceComp{Value: row.ApplicationChance}
}

func MakeBuffRequired_tagComp(row *pb.BuffTable) BuffRequired_tagComp {
    return BuffRequired_tagComp{Value: row.RequiredTag}
}

func MakeBuffSecond_required_tagComp(row *pb.BuffTable) BuffSecond_required_tagComp {
    return BuffSecond_required_tagComp{Value: row.SecondRequiredTag}
}

func MakeBuffConditional_bonusComp(row *pb.BuffTable) BuffConditional_bonusComp {
    return BuffConditional_bonusComp{Value: row.ConditionalBonus}
}

func MakeBuffSecondary_ratioComp(row *pb.BuffTable) BuffSecondary_ratioComp {
    return BuffSecondary_ratioComp{Value: row.SecondaryRatio}
}

func MakeBuffEffect_countComp(row *pb.BuffTable) BuffEffect_countComp {
    return BuffEffect_countComp{Value: row.EffectCount}
}

func MakeBuffTarget_countComp(row *pb.BuffTable) BuffTarget_countComp {
    return BuffTarget_countComp{Value: row.TargetCount}
}

func MakeBuffPet_target_countComp(row *pb.BuffTable) BuffPet_target_countComp {
    return BuffPet_target_countComp{Value: row.PetTargetCount}
}

func MakeBuffStack_keyComp(row *pb.BuffTable) BuffStack_keyComp {
    return BuffStack_keyComp{Value: row.StackKey}
}

func MakeBuffRefresh_policyComp(row *pb.BuffTable) BuffRefresh_policyComp {
    return BuffRefresh_policyComp{Value: row.RefreshPolicy}
}

func MakeBuffKeep_on_deathComp(row *pb.BuffTable) BuffKeep_on_deathComp {
    return BuffKeep_on_deathComp{Value: row.KeepOnDeath}
}

func MakeBuffEffect_priorityComp(row *pb.BuffTable) BuffEffect_priorityComp {
    return BuffEffect_priorityComp{Value: row.EffectPriority}
}

func MakeBuffStart_roundComp(row *pb.BuffTable) BuffStart_roundComp {
    return BuffStart_roundComp{Value: row.StartRound}
}

func MakeBuffStack_limitComp(row *pb.BuffTable) BuffStack_limitComp {
    return BuffStack_limitComp{Value: row.StackLimit}
}

func MakeBuffRequire_living_casterComp(row *pb.BuffTable) BuffRequire_living_casterComp {
    return BuffRequire_living_casterComp{Value: row.RequireLivingCaster}
}

func MakeBuffTick_on_applyComp(row *pb.BuffTable) BuffTick_on_applyComp {
    return BuffTick_on_applyComp{Value: row.TickOnApply}
}

func MakeBuffKeep_on_logoutComp(row *pb.BuffTable) BuffKeep_on_logoutComp {
    return BuffKeep_on_logoutComp{Value: row.KeepOnLogout}
}

func MakeBuffInterval_effectComp(row *pb.BuffTable) BuffInterval_effectComp {
    return BuffInterval_effectComp{Values: row.IntervalEffect}
}

func MakeBuffSub_buffComp(row *pb.BuffTable) BuffSub_buffComp {
    return BuffSub_buffComp{Values: row.SubBuff}
}

func MakeBuffTarget_sub_buffComp(row *pb.BuffTable) BuffTarget_sub_buffComp {
    return BuffTarget_sub_buffComp{Values: row.TargetSubBuff}
}


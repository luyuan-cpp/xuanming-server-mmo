
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for AttributeRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type AttributeRuleIdComp struct {
    Value uint32
}

type AttributeRuleMax_schemesComp struct {
    Value uint32
}

type AttributeRuleFree_scheme_countComp struct {
    Value uint32
}

type AttributeRuleCreate_scheme_cost_goldComp struct {
    Value uint64
}

type AttributeRuleSwitch_cooldown_secondsComp struct {
    Value uint32
}

type AttributeRuleScheme_name_max_lenComp struct {
    Value uint32
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeAttributeRuleIdComp(row *pb.AttributeRuleTable) AttributeRuleIdComp {
    return AttributeRuleIdComp{Value: row.Id}
}

func MakeAttributeRuleMax_schemesComp(row *pb.AttributeRuleTable) AttributeRuleMax_schemesComp {
    return AttributeRuleMax_schemesComp{Value: row.MaxSchemes}
}

func MakeAttributeRuleFree_scheme_countComp(row *pb.AttributeRuleTable) AttributeRuleFree_scheme_countComp {
    return AttributeRuleFree_scheme_countComp{Value: row.FreeSchemeCount}
}

func MakeAttributeRuleCreate_scheme_cost_goldComp(row *pb.AttributeRuleTable) AttributeRuleCreate_scheme_cost_goldComp {
    return AttributeRuleCreate_scheme_cost_goldComp{Value: row.CreateSchemeCostGold}
}

func MakeAttributeRuleSwitch_cooldown_secondsComp(row *pb.AttributeRuleTable) AttributeRuleSwitch_cooldown_secondsComp {
    return AttributeRuleSwitch_cooldown_secondsComp{Value: row.SwitchCooldownSeconds}
}

func MakeAttributeRuleScheme_name_max_lenComp(row *pb.AttributeRuleTable) AttributeRuleScheme_name_max_lenComp {
    return AttributeRuleScheme_name_max_lenComp{Value: row.SchemeNameMaxLen}
}



package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for BattleProgressionRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type BattleProgressionRuleIdComp struct {
    Value uint32
}

type BattleProgressionRuleWeekly_silver_soft_capComp struct {
    Value uint64
}

type BattleProgressionRuleDiminished_rate_permyriadComp struct {
    Value uint32
}

type BattleProgressionRuleWeekly_silver_hard_capComp struct {
    Value uint64
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeBattleProgressionRuleIdComp(row *pb.BattleProgressionRuleTable) BattleProgressionRuleIdComp {
    return BattleProgressionRuleIdComp{Value: row.Id}
}

func MakeBattleProgressionRuleWeekly_silver_soft_capComp(row *pb.BattleProgressionRuleTable) BattleProgressionRuleWeekly_silver_soft_capComp {
    return BattleProgressionRuleWeekly_silver_soft_capComp{Value: row.WeeklySilverSoftCap}
}

func MakeBattleProgressionRuleDiminished_rate_permyriadComp(row *pb.BattleProgressionRuleTable) BattleProgressionRuleDiminished_rate_permyriadComp {
    return BattleProgressionRuleDiminished_rate_permyriadComp{Value: row.DiminishedRatePermyriad}
}

func MakeBattleProgressionRuleWeekly_silver_hard_capComp(row *pb.BattleProgressionRuleTable) BattleProgressionRuleWeekly_silver_hard_capComp {
    return BattleProgressionRuleWeekly_silver_hard_capComp{Value: row.WeeklySilverHardCap}
}


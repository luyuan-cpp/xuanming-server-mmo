
package table

import (
    pb "shared/generated/pb/table"
)

// ============================================================
// Per-column component structs for RoleNameRuleTable
// ============================================================
// Scalar columns → value components
// Repeated columns → slice components
// ============================================================


type RoleNameRuleIdComp struct {
    Value uint32
}

type RoleNameRuleMin_charsComp struct {
    Value uint32
}

type RoleNameRuleMax_charsComp struct {
    Value uint32
}

type RoleNameRuleGenerated_prefixComp struct {
    Value string
}

type RoleNameRuleGenerated_suffix_lenComp struct {
    Value uint32
}

type RoleNameRuleMax_generate_attemptsComp struct {
    Value uint32
}

type RoleNameRuleDescComp struct {
    Value string
}


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

func MakeRoleNameRuleIdComp(row *pb.RoleNameRuleTable) RoleNameRuleIdComp {
    return RoleNameRuleIdComp{Value: row.Id}
}

func MakeRoleNameRuleMin_charsComp(row *pb.RoleNameRuleTable) RoleNameRuleMin_charsComp {
    return RoleNameRuleMin_charsComp{Value: row.MinChars}
}

func MakeRoleNameRuleMax_charsComp(row *pb.RoleNameRuleTable) RoleNameRuleMax_charsComp {
    return RoleNameRuleMax_charsComp{Value: row.MaxChars}
}

func MakeRoleNameRuleGenerated_prefixComp(row *pb.RoleNameRuleTable) RoleNameRuleGenerated_prefixComp {
    return RoleNameRuleGenerated_prefixComp{Value: row.GeneratedPrefix}
}

func MakeRoleNameRuleGenerated_suffix_lenComp(row *pb.RoleNameRuleTable) RoleNameRuleGenerated_suffix_lenComp {
    return RoleNameRuleGenerated_suffix_lenComp{Value: row.GeneratedSuffixLen}
}

func MakeRoleNameRuleMax_generate_attemptsComp(row *pb.RoleNameRuleTable) RoleNameRuleMax_generate_attemptsComp {
    return RoleNameRuleMax_generate_attemptsComp{Value: row.MaxGenerateAttempts}
}

func MakeRoleNameRuleDescComp(row *pb.RoleNameRuleTable) RoleNameRuleDescComp {
    return RoleNameRuleDescComp{Value: row.Desc}
}


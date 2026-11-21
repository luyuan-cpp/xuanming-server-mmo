
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/actoractioncombatstate_table.pb.h"

// ============================================================
// Per-column ECS components for ActorActionCombatStateTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct ActorActionCombatStateIdComp {
    uint32_t value;
};

struct ActorActionCombatStateState_idComp {
    uint32_t value;
};

struct ActorActionCombatStateState_modeComp {
    uint32_t value;
};

struct ActorActionCombatStateState_tipComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline ActorActionCombatStateIdComp MakeActorActionCombatStateIdComp(const ActorActionCombatStateTable& row) {
    return { row.id() };
}
inline ActorActionCombatStateState_idComp MakeActorActionCombatStateState_idComp(const ActorActionCombatStateTable& row) {
    return { row.state_id() };
}
inline ActorActionCombatStateState_modeComp MakeActorActionCombatStateState_modeComp(const ActorActionCombatStateTable& row) {
    return { row.state_mode() };
}
inline ActorActionCombatStateState_tipComp MakeActorActionCombatStateState_tipComp(const ActorActionCombatStateTable& row) {
    return { row.state_tip() };
}

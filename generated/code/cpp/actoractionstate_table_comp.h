
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/actoractionstate_table.pb.h"

// ============================================================
// Per-column ECS components for ActorActionStateTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct ActorActionStateIdComp {
    uint32_t value;
};

struct ActorActionStateState_idComp {
    uint32_t value;
};

struct ActorActionStateState_modeComp {
    uint32_t value;
};

struct ActorActionStateState_tipComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline ActorActionStateIdComp MakeActorActionStateIdComp(const ActorActionStateTable& row) {
    return { row.id() };
}
inline ActorActionStateState_idComp MakeActorActionStateState_idComp(const ActorActionStateTable& row) {
    return { row.state_id() };
}
inline ActorActionStateState_modeComp MakeActorActionStateState_modeComp(const ActorActionStateTable& row) {
    return { row.state_mode() };
}
inline ActorActionStateState_tipComp MakeActorActionStateState_tipComp(const ActorActionStateTable& row) {
    return { row.state_tip() };
}

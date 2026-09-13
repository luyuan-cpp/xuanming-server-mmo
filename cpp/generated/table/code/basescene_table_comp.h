
#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/basescene_table.pb.h"

// ============================================================
// Per-column ECS components for BaseSceneTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct BaseSceneIdComp {
    uint32_t value;
};

struct BaseSceneNav_bin_fileComp {
    std::string_view value;
};

struct BaseSceneSpawn_xComp {
    double value;
};

struct BaseSceneSpawn_yComp {
    double value;
};

struct BaseSceneSpawn_zComp {
    double value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline BaseSceneIdComp MakeBaseSceneIdComp(const BaseSceneTable& row) {
    return { row.id() };
}
inline BaseSceneNav_bin_fileComp MakeBaseSceneNav_bin_fileComp(const BaseSceneTable& row) {
    return { std::string_view(row.nav_bin_file()) };
}
inline BaseSceneSpawn_xComp MakeBaseSceneSpawn_xComp(const BaseSceneTable& row) {
    return { row.spawn_x() };
}
inline BaseSceneSpawn_yComp MakeBaseSceneSpawn_yComp(const BaseSceneTable& row) {
    return { row.spawn_y() };
}
inline BaseSceneSpawn_zComp MakeBaseSceneSpawn_zComp(const BaseSceneTable& row) {
    return { row.spawn_z() };
}


#pragma once
#include <cstdint>
#include <span>
#include <string_view>
#include "table/proto/skillscrollcraft_table.pb.h"

// ============================================================
// Per-column ECS components for SkillScrollCraftTable
// ============================================================
// Scalar columns → value components
// String columns → std::string_view (points into proto memory)
// Repeated columns → std::span (points into proto RepeatedField)
// ============================================================


struct SkillScrollCraftIdComp {
    uint32_t value;
};

struct SkillScrollCraftNameComp {
    std::string_view value;
};

struct SkillScrollCraftDescriptionComp {
    std::string_view value;
};

struct SkillScrollCraftRarity_tierComp {
    uint32_t value;
};

struct SkillScrollCraftFragment_item_idComp {
    uint32_t value;
};

struct SkillScrollCraftRandom_fragment_countComp {
    uint32_t value;
};

struct SkillScrollCraftRandom_gold_costComp {
    uint64_t value;
};

struct SkillScrollCraftTargeted_fragment_countComp {
    uint32_t value;
};

struct SkillScrollCraftTargeted_gold_costComp {
    uint64_t value;
};

struct SkillScrollCraftDismantle_fragment_countComp {
    uint32_t value;
};

struct SkillScrollCraftRare_scroll_item_idComp {
    uint32_t value;
};

struct SkillScrollCraftRare_scroll_countComp {
    uint32_t value;
};

struct SkillScrollCraftRare_ink_item_idComp {
    uint32_t value;
};

struct SkillScrollCraftRare_ink_countComp {
    uint32_t value;
};

struct SkillScrollCraftRare_gold_costComp {
    uint64_t value;
};

struct SkillScrollCraftEnabledComp {
    uint32_t value;
};


// ============================================================
// Factory helpers — build component from a proto row
// ============================================================

inline SkillScrollCraftIdComp MakeSkillScrollCraftIdComp(const SkillScrollCraftTable& row) {
    return { row.id() };
}
inline SkillScrollCraftNameComp MakeSkillScrollCraftNameComp(const SkillScrollCraftTable& row) {
    return { std::string_view(row.name()) };
}
inline SkillScrollCraftDescriptionComp MakeSkillScrollCraftDescriptionComp(const SkillScrollCraftTable& row) {
    return { std::string_view(row.description()) };
}
inline SkillScrollCraftRarity_tierComp MakeSkillScrollCraftRarity_tierComp(const SkillScrollCraftTable& row) {
    return { row.rarity_tier() };
}
inline SkillScrollCraftFragment_item_idComp MakeSkillScrollCraftFragment_item_idComp(const SkillScrollCraftTable& row) {
    return { row.fragment_item_id() };
}
inline SkillScrollCraftRandom_fragment_countComp MakeSkillScrollCraftRandom_fragment_countComp(const SkillScrollCraftTable& row) {
    return { row.random_fragment_count() };
}
inline SkillScrollCraftRandom_gold_costComp MakeSkillScrollCraftRandom_gold_costComp(const SkillScrollCraftTable& row) {
    return { row.random_gold_cost() };
}
inline SkillScrollCraftTargeted_fragment_countComp MakeSkillScrollCraftTargeted_fragment_countComp(const SkillScrollCraftTable& row) {
    return { row.targeted_fragment_count() };
}
inline SkillScrollCraftTargeted_gold_costComp MakeSkillScrollCraftTargeted_gold_costComp(const SkillScrollCraftTable& row) {
    return { row.targeted_gold_cost() };
}
inline SkillScrollCraftDismantle_fragment_countComp MakeSkillScrollCraftDismantle_fragment_countComp(const SkillScrollCraftTable& row) {
    return { row.dismantle_fragment_count() };
}
inline SkillScrollCraftRare_scroll_item_idComp MakeSkillScrollCraftRare_scroll_item_idComp(const SkillScrollCraftTable& row) {
    return { row.rare_scroll_item_id() };
}
inline SkillScrollCraftRare_scroll_countComp MakeSkillScrollCraftRare_scroll_countComp(const SkillScrollCraftTable& row) {
    return { row.rare_scroll_count() };
}
inline SkillScrollCraftRare_ink_item_idComp MakeSkillScrollCraftRare_ink_item_idComp(const SkillScrollCraftTable& row) {
    return { row.rare_ink_item_id() };
}
inline SkillScrollCraftRare_ink_countComp MakeSkillScrollCraftRare_ink_countComp(const SkillScrollCraftTable& row) {
    return { row.rare_ink_count() };
}
inline SkillScrollCraftRare_gold_costComp MakeSkillScrollCraftRare_gold_costComp(const SkillScrollCraftTable& row) {
    return { row.rare_gold_cost() };
}
inline SkillScrollCraftEnabledComp MakeSkillScrollCraftEnabledComp(const SkillScrollCraftTable& row) {
    return { row.enabled() };
}

#pragma once

#include <cstdint>
#include <map>
#include <string>

// 宝宝独立条目池纯规则：全量目标可免费减点重分配，未知存档键原样保留并占用预算；0 点不影响已学技能资格。
namespace petentries {

struct Rule {
    uint32_t cap = 0;
    uint32_t rate = 0;
};

using Points = std::map<uint32_t, uint32_t>;
using Rules = std::map<uint32_t, Rule>;

enum class Error : uint8_t {
    None = 0,
    Unknown = 1,
    Cap = 2,
    Budget = 3,
    Directions = 4,
};

inline Error Validate(const Points& stored, const Points& target, const Rules& rules,
                      uint32_t budget, uint32_t maxDirections) {
    uint64_t used = 0;
    uint32_t directions = 0;
    for (const auto& [id, value] : stored) {
        if (!rules.contains(id)) {
            used += value;
        }
    }
    for (const auto& [id, value] : target) {
        const auto row = rules.find(id);
        if (row == rules.end()) {
            return Error::Unknown;
        }
        if (value > row->second.cap) {
            return Error::Cap;
        }
        used += value;
        if (value > 0) {
            ++directions;
        }
    }
    if (used > budget) {
        return Error::Budget;
    }
    if (maxDirections > 0 && directions > maxDirections) {
        return Error::Directions;
    }
    return Error::None;
}

inline Points ReplaceKnown(const Points& stored, const Points& target, const Rules& rules) {
    Points result;
    for (const auto& [id, value] : stored) {
        if (!rules.contains(id)) {
            result[id] = value;
        }
    }
    for (const auto& [id, value] : target) {
        if (rules.contains(id) && value > 0) {
            result[id] = value;
        }
    }
    return result;
}

inline bool ValidRequestId(const std::string& id) {
    if (id.size() < 16 || id.size() > 64) {
        return false;
    }
    for (const auto c : id) {
        const bool ok = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
                        (c >= '0' && c <= '9') || c == '-' || c == '_';
        if (!ok) {
            return false;
        }
    }
    return true;
}

inline constexpr uint32_t kMaxPresetSlots = 3;
inline constexpr uint32_t kMaxPresetTag = 3;
inline constexpr uint32_t kKeepPresetTag = 0xFFFFFFFFu;
inline constexpr uint32_t kMaxCultivationTier = 5;

inline bool ValidPresetSlot(uint32_t slotId) {
    return slotId >= 1u && slotId <= kMaxPresetSlots;
}

inline bool ValidPresetTag(uint32_t tag) {
    return tag <= kMaxPresetTag || tag == kKeepPresetTag;
}

inline uint32_t EffectivePresetSlot(uint32_t stored) {
    return ValidPresetSlot(stored) ? stored : 1u;
}

inline uint64_t Version(uint64_t value) {
    return value == 0 ? 1 : value;
}

}  // namespace petentries

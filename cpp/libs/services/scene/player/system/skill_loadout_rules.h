#pragma once

#include <cstdint>
#include <functional>
#include <set>
#include <string>
#include <vector>

// 人物战法构筑装配纯规则（布局层，不依赖 ECS / Protobuf）：
// 免费 3 套 + 仙途轻享卡生效期扩展至 6 套流派方案，每套支持 3 被动 + 1 追击 + 1 开场，主动技全量可用，同 exclusive_group 互斥。
namespace skillloadout {

inline constexpr uint32_t kBaseMaxSchemes = 3;
inline constexpr uint32_t kMaxSchemes = 6;
inline constexpr uint32_t kMaxRoleTag = 3;
inline constexpr uint32_t kMaxPassiveSlots = 3;
inline constexpr uint32_t kMaxPursuitSlots = 1;
inline constexpr uint32_t kMaxOpeningSlots = 1;

enum class Error : uint8_t {
    None = 0,
    InvalidScheme = 1,
    InvalidRoleTag = 2,
    NotLearned = 3,
    WrongCategory = 4,
    DuplicateSkill = 5,
    SlotLimitReached = 6,
    ExclusiveConflict = 7,
};

struct SkillMeta {
    bool valid = false;
    bool learned = false;
    uint32_t abilityKind = 0;     // 2 被动 / 3 追击 / 4 开场
    uint32_t exclusiveGroup = 0;  // 0 无互斥；>0 同组不可同时装配
};

inline constexpr uint32_t UnlockedSchemes(bool lightCardActive) {
    return lightCardActive ? kMaxSchemes : kBaseMaxSchemes;
}

inline bool ValidSchemeId(uint32_t schemeId, uint32_t maxAllowed = kMaxSchemes) {
    return schemeId >= 1u && schemeId <= maxAllowed;
}

inline bool ValidRoleTag(uint32_t roleTag) {
    return roleTag <= kMaxRoleTag;
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

inline uint64_t Version(uint64_t stored) {
    return stored == 0 ? 1u : stored;
}

template <typename LookupFn>
inline Error ValidateScheme(uint32_t schemeId, uint32_t roleTag,
                            const std::vector<uint32_t>& passiveIds,
                            const std::vector<uint32_t>& pursuitIds,
                            const std::vector<uint32_t>& openingIds,
                            LookupFn&& lookup,
                            uint32_t maxAllowedSchemes = kMaxSchemes) {
    if (!ValidSchemeId(schemeId, maxAllowedSchemes)) {
        return Error::InvalidScheme;
    }
    if (!ValidRoleTag(roleTag)) {
        return Error::InvalidRoleTag;
    }
    if (passiveIds.size() > kMaxPassiveSlots ||
        pursuitIds.size() > kMaxPursuitSlots ||
        openingIds.size() > kMaxOpeningSlots) {
        return Error::SlotLimitReached;
    }

    std::set<uint32_t> seenSkills;
    std::set<uint32_t> occupiedExclusiveGroups;

    const auto checkList = [&](const std::vector<uint32_t>& ids, uint32_t expectedKind) -> Error {
        for (const uint32_t skillId : ids) {
            if (skillId == 0) {
                return Error::WrongCategory;
            }
            if (!seenSkills.insert(skillId).second) {
                return Error::DuplicateSkill;
            }
            const SkillMeta meta = lookup(skillId);
            if (!meta.valid || meta.abilityKind != expectedKind) {
                return Error::WrongCategory;
            }
            if (!meta.learned) {
                return Error::NotLearned;
            }
            if (meta.exclusiveGroup != 0 && !occupiedExclusiveGroups.insert(meta.exclusiveGroup).second) {
                return Error::ExclusiveConflict;
            }
        }
        return Error::None;
    };

    if (const auto err = checkList(passiveIds, 2u); err != Error::None) return err;
    if (const auto err = checkList(pursuitIds, 3u); err != Error::None) return err;
    if (const auto err = checkList(openingIds, 4u); err != Error::None) return err;
    return Error::None;
}

}  // namespace skillloadout

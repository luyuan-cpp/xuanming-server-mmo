#pragma once

#include <algorithm>
#include <array>
#include <cctype>
#include <cstdint>
#include <functional>
#include <limits>
#include <set>
#include <string>
#include <string_view>
#include <vector>

// 人物「战法·加点」一体化流派方案纯规则（布局层，不依赖 ECS / Protobuf / 时钟）：
// 永久免费 2 套（方案 1..2）+ 角色达 85 级满级自动永久再赠 1 套（0 氪长期纯玩共 3 套免费）+ 仙途轻享卡生效期额外 +2 套
// + 按套按月租用额外槽位（单套 30 真实灵石/30 天，共享统一到期日，最多叠 180 天），总计 7 套封顶。
// 每套含 1 开场 + 3 被动 + 1 追击 + 1:1 绑定的四维加点方案；到期只锁不删。
namespace skillloadout {

inline constexpr uint32_t kBaseFreeSchemes = 2;
inline constexpr uint32_t kMaxLevelFreeSchemeUnlockLevel = 85;
inline constexpr uint32_t kMaxLevelBonusFreeSchemes = 1;
inline constexpr uint32_t kBaseMaxSchemes = kBaseFreeSchemes;  // 兼容旧调用方别名
inline constexpr uint32_t kLightCardBonusSchemes = 2;
inline constexpr uint32_t kMaxRentedExtraSlots = 5;
inline constexpr uint32_t kMaxSchemes = 7;
inline constexpr uint32_t kMaxRoleTag = 4;  // 0默认 / 1输出 / 2控场 / 3辅助 / 4生存
inline constexpr uint32_t kMaxPassiveSlots = 3;
inline constexpr uint32_t kMaxPursuitSlots = 1;
inline constexpr uint32_t kMaxOpeningSlots = 1;
inline constexpr uint32_t kMaxNameCodePoints = 12;

inline constexpr uint64_t kRentUnitPriceDiamond = 30;
inline constexpr uint32_t kRentPeriodDays = 30;
inline constexpr uint64_t kSecondsPerDay = 86400;
inline constexpr uint64_t kRentPeriodSeconds = static_cast<uint64_t>(kRentPeriodDays) * kSecondsPerDay;
inline constexpr uint32_t kRentMaxRemainingDays = 180;
inline constexpr uint64_t kRentMaxRemainingSeconds = static_cast<uint64_t>(kRentMaxRemainingDays) * kSecondsPerDay;
inline constexpr uint32_t kRentMaxPeriodsPerCall = 6;

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

enum class RentError : uint8_t {
    None = 0,
    InvalidParam = 1,
    SlotsInvalid = 2,
    StackLimit = 3,
};

struct SkillMeta {
    bool valid = false;
    bool learned = false;
    uint32_t abilityKind = 0;     // 2 被动 / 3 追击 / 4 开场
    uint32_t exclusiveGroup = 0;  // 0 无互斥；>0 同组不可同时装配
};

struct RentPlan {
    RentError error = RentError::None;
    uint32_t newRentedSlots = 0;
    uint64_t newExpiresAtSec = 0;
    uint64_t prorateCostDiamond = 0;
    uint64_t renewCostDiamond = 0;
    uint64_t totalCostDiamond = 0;
};

inline constexpr uint32_t ActiveRentedSlots(uint32_t storedRentedSlots, uint64_t rentExpiresAtSec, uint64_t nowUnixSec) {
    if (rentExpiresAtSec <= nowUnixSec) {
        return 0u;
    }
    return storedRentedSlots > kMaxRentedExtraSlots ? kMaxRentedExtraSlots : storedRentedSlots;
}

inline constexpr uint32_t FreeSchemesForLevel(uint32_t effectiveLevel) {
    return kBaseFreeSchemes +
           (effectiveLevel >= kMaxLevelFreeSchemeUnlockLevel ? kMaxLevelBonusFreeSchemes : 0u);
}

inline constexpr uint32_t MaxRentableExtraSlots(uint32_t freeSchemes, bool lightCardActive) {
    const uint32_t base = freeSchemes + (lightCardActive ? kLightCardBonusSchemes : 0u);
    return base >= kMaxSchemes ? 0u : (kMaxSchemes - base);
}

inline constexpr uint32_t MaxRentableExtraSlots(bool lightCardActive) {
    return MaxRentableExtraSlots(kBaseFreeSchemes, lightCardActive);
}

inline constexpr uint32_t UnlockedSchemes(uint32_t freeSchemes, bool lightCardActive, uint32_t storedRentedSlots,
                                          uint64_t rentExpiresAtSec, uint64_t nowUnixSec) {
    const uint32_t total = freeSchemes +
                           (lightCardActive ? kLightCardBonusSchemes : 0u) +
                           ActiveRentedSlots(storedRentedSlots, rentExpiresAtSec, nowUnixSec);
    return total > kMaxSchemes ? kMaxSchemes : total;
}

inline constexpr uint32_t UnlockedSchemes(bool lightCardActive, uint32_t storedRentedSlots,
                                          uint64_t rentExpiresAtSec, uint64_t nowUnixSec) {
    return UnlockedSchemes(kBaseFreeSchemes, lightCardActive, storedRentedSlots, rentExpiresAtSec, nowUnixSec);
}

inline constexpr uint32_t UnlockedSchemes(bool lightCardActive) {
    return UnlockedSchemes(lightCardActive, 0u, 0u, 0u);
}

inline constexpr bool ValidSchemeId(uint32_t schemeId, uint32_t maxAllowed = kMaxSchemes) {
    return schemeId >= 1u && schemeId <= maxAllowed;
}

inline constexpr bool ValidRoleTag(uint32_t roleTag) {
    return roleTag <= kMaxRoleTag;
}

inline std::string DefaultSchemeName(uint32_t schemeId) {
    switch (schemeId) {
    case 1u: return "方案一";
    case 2u: return "方案二";
    case 3u: return "方案三";
    case 4u: return "方案四";
    case 5u: return "方案五";
    case 6u: return "方案六";
    case 7u: return "方案七";
    default: return "方案" + std::to_string(schemeId);
    }
}

inline bool ValidRequestId(std::string_view id) {
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

inline constexpr uint64_t Version(uint64_t stored) {
    return stored == 0 ? 1u : stored;
}

inline bool IsInvisibleCodePoint(uint32_t cp) {
    return cp == 0x20 || cp == 0xA0 || (cp >= 0x2000 && cp <= 0x200F) || cp == 0x2028 || cp == 0x2029 ||
           cp == 0x202F || cp == 0x205F || cp == 0x2060 || cp == 0x3000 || cp == 0xFEFF;
}

inline bool DefaultSensitiveWordFilter(std::string_view text) {
    std::string lower;
    lower.reserve(text.size());
    for (const unsigned char ch : text) {
        lower.push_back(static_cast<char>(std::tolower(ch)));
    }
    static constexpr std::array<std::string_view, 9> kBlockedTerms = {
        "官方", "客服", "管理员", "系统消息", "外挂", "代充", "私服", "gm", "admin",
    };
    for (const auto term : kBlockedTerms) {
        if (lower.find(term) != std::string::npos) {
            return false;
        }
    }
    return true;
}

// 名称合法性校验：UTF-8 1..12 字符、非全空白、无控制字符/非法序列/富文本尖括号、过敏感词检查。
inline bool ValidateSchemeName(std::string_view name,
                               const std::function<bool(std::string_view)>& sensitiveWordFilter = {}) {
    if (name.empty()) {
        return false;
    }
    uint32_t codePoints = 0;
    bool hasVisible = false;
    for (std::size_t i = 0; i < name.size();) {
        const auto c = static_cast<unsigned char>(name[i]);
        if (c < 0x20 || c == 0x7F || c == '<' || c == '>') {
            return false;
        }
        uint32_t cp = c;
        std::size_t len = 1;
        uint32_t minCp = 0;
        if (c < 0x80) {
            len = 1;
            minCp = 0;
        } else if ((c & 0xE0) == 0xC0) {
            cp = c & 0x1F;
            len = 2;
            minCp = 0x80;
        } else if ((c & 0xF0) == 0xE0) {
            cp = c & 0x0F;
            len = 3;
            minCp = 0x800;
        } else if ((c & 0xF8) == 0xF0) {
            cp = c & 0x07;
            len = 4;
            minCp = 0x10000;
        } else {
            return false;
        }
        if (i + len > name.size()) {
            return false;
        }
        for (std::size_t k = 1; k < len; ++k) {
            const auto cc = static_cast<unsigned char>(name[i + k]);
            if ((cc & 0xC0) != 0x80) {
                return false;
            }
            cp = (cp << 6) | (cc & 0x3F);
        }
        if (cp < minCp || cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF) || (cp >= 0x80 && cp <= 0x9F)) {
            return false;
        }
        i += len;
        ++codePoints;
        if (codePoints > kMaxNameCodePoints) {
            return false;
        }
        if (!IsInvisibleCodePoint(cp)) {
            hasVisible = true;
        }
    }
    if (!hasVisible) {
        return false;
    }
    if (sensitiveWordFilter) {
        return sensitiveWordFilter(name);
    }
    return DefaultSensitiveWordFilter(name);
}

// 按套按月租用 / 扩容补差价 / 续期纯规划（零溢出、向上取整防拆单套利、防溢买、180 天封顶）：
// - 租期内不可缩减已租套数（若轻享卡生效导致可租上限降至 3，允许按上限 3 续期不白花冤枉钱）；
// - 未在租期时 addPeriods 必须 >= 1；在租期内 addPeriods == 0 仅允许扩容加套（按剩余租期向上取整补差价）；
// - 新剩余租期不得超过 180 天（含边界等于 180 天允许）。
inline RentPlan PlanRent(bool lightCardActive, uint32_t storedRentedSlots, uint64_t currentExpiresAtSec,
                         uint64_t nowUnixSec, uint32_t targetRentedSlots, uint32_t addPeriods,
                         uint32_t freeSchemes = kBaseFreeSchemes) {
    RentPlan plan;
    const uint32_t maxRentable = MaxRentableExtraSlots(freeSchemes, lightCardActive);
    const uint32_t activeRented = ActiveRentedSlots(storedRentedSlots, currentExpiresAtSec, nowUnixSec);
    const bool rentActive = activeRented > 0u && currentExpiresAtSec > nowUnixSec;
    const uint64_t remainingSec = rentActive ? (currentExpiresAtSec - nowUnixSec) : 0u;

    if (targetRentedSlots < 1u || targetRentedSlots > maxRentable) {
        plan.error = RentError::SlotsInvalid;
        return plan;
    }
    const uint32_t minAllowedInTerm = std::min(activeRented, maxRentable);
    if (rentActive && targetRentedSlots < minAllowedInTerm) {
        plan.error = RentError::SlotsInvalid;
        return plan;
    }
    if (addPeriods > kRentMaxPeriodsPerCall) {
        plan.error = RentError::StackLimit;
        return plan;
    }
    if (!rentActive && addPeriods == 0u) {
        plan.error = RentError::InvalidParam;
        return plan;
    }
    if (rentActive && addPeriods == 0u && targetRentedSlots <= activeRented) {
        plan.error = RentError::InvalidParam;
        return plan;
    }

    const uint64_t addSec = static_cast<uint64_t>(addPeriods) * kRentPeriodSeconds;
    if (addPeriods > 0u) {
        if (remainingSec > kRentMaxRemainingSeconds || kRentMaxRemainingSeconds - remainingSec < addSec) {
            plan.error = RentError::StackLimit;
            return plan;
        }
        if (nowUnixSec > std::numeric_limits<uint64_t>::max() - remainingSec - addSec) {
            plan.error = RentError::StackLimit;
            return plan;
        }
    }

    const uint32_t addedSlots = (rentActive && targetRentedSlots > activeRented) ? (targetRentedSlots - activeRented) : 0u;
    uint64_t prorateCost = 0u;
    if (addedSlots > 0u && remainingSec > 0u) {
        const uint64_t perPeriod = static_cast<uint64_t>(addedSlots) * kRentUnitPriceDiamond;
        const uint64_t fullPeriods = remainingSec / kRentPeriodSeconds;
        const uint64_t remSec = remainingSec % kRentPeriodSeconds;
        prorateCost = fullPeriods * perPeriod +
                      (remSec == 0u ? 0u : (perPeriod * remSec + kRentPeriodSeconds - 1u) / kRentPeriodSeconds);
        if (prorateCost == 0u) {
            prorateCost = 1u;
        }
    }
    const uint64_t renewCost = static_cast<uint64_t>(targetRentedSlots) * kRentUnitPriceDiamond *
                               static_cast<uint64_t>(addPeriods);
    plan.newRentedSlots = targetRentedSlots;
    plan.newExpiresAtSec = addPeriods > 0u ? (nowUnixSec + remainingSec + addSec) : currentExpiresAtSec;
    plan.prorateCostDiamond = prorateCost;
    plan.renewCostDiamond = renewCost;
    plan.totalCostDiamond = prorateCost + renewCost;
    return plan;
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

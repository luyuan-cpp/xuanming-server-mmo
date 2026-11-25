#pragma once

// 瑞兽迎春限时活动纯规则（零 ECS / 零网络依赖，供生产与单测共用）。
// 设计契约见 docs/design/battle-festival-beast-implementation-contract.md。

#include <algorithm>
#include <cctype>
#include <cstdint>
#include <string_view>
#include <vector>

#include "proto/scene/player_activity.pb.h"
#include "table/proto/dungeon_table.pb.h"
#include "table/proto/festivalbeastactivity_table.pb.h"
#include "table/proto/festivalbeasttier_table.pb.h"
#include "table/proto/pet_table.pb.h"

namespace festival_beast_rules {

inline constexpr uint64_t kDayMs = 86400000ULL;
inline constexpr uint64_t kRequiredOpenDurationMs = 14ULL * kDayMs;
inline constexpr uint32_t kDefaultShowcaseDays = 7;
inline constexpr uint32_t kRequiredMinLevel = 1;
inline constexpr uint32_t kRequiredMaxLevel = 120;
inline constexpr uint32_t kFestivalBeastBossTrialKind = 7;
inline constexpr uint64_t kLaunchStaleTimeoutMs = 150000ULL;

// 解析标准 36 字符 UUID（xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx，拒绝全 0）。
inline bool ParseUuid128(std::string_view text, uint64_t& highOut, uint64_t& lowOut) {
    highOut = 0;
    lowOut = 0;
    if (text.size() != 36) return false;
    uint64_t high = 0;
    uint64_t low = 0;
    size_t hexCount = 0;
    for (size_t i = 0; i < text.size(); ++i) {
        const char ch = text[i];
        if (i == 8 || i == 13 || i == 18 || i == 23) {
            if (ch != '-') return false;
            continue;
        }
        uint64_t nibble = 0;
        if (ch >= '0' && ch <= '9') {
            nibble = static_cast<uint64_t>(ch - '0');
        } else if (ch >= 'a' && ch <= 'f') {
            nibble = static_cast<uint64_t>(10 + (ch - 'a'));
        } else if (ch >= 'A' && ch <= 'F') {
            nibble = static_cast<uint64_t>(10 + (ch - 'A'));
        } else {
            return false;
        }
        if (hexCount < 16) {
            high = (high << 4) | nibble;
        } else {
            low = (low << 4) | nibble;
        }
        ++hexCount;
    }
    if (hexCount != 32 || (high == 0 && low == 0)) return false;
    highOut = high;
    lowOut = low;
    return true;
}

inline uint64_t ClaimShowcaseEndAtMs(uint64_t effectiveEndMs, uint32_t showcaseDays) {
    if (effectiveEndMs == 0) return 0;
    const uint64_t days = showcaseDays > 0 ? showcaseDays : kDefaultShowcaseDays;
    return effectiveEndMs + days * kDayMs;
}

// 服务端 UTC 毫秒裁定活动阶段；左闭右开 [start, end)，结束后集中领奖展示 [end, end + showcaseDays)。
inline FestivalBeastPhase ResolvePhase(bool enabled, uint64_t startMs, uint64_t endMs,
                                       uint32_t showcaseDays, uint64_t nowMs) {
    if (!enabled || startMs == 0 || endMs <= startMs) {
        return FESTIVAL_BEAST_PHASE_UNSCHEDULED;
    }
    if (nowMs < startMs) {
        return FESTIVAL_BEAST_PHASE_UPCOMING;
    }
    if (nowMs < endMs) {
        return FESTIVAL_BEAST_PHASE_OPEN;
    }
    if (nowMs < ClaimShowcaseEndAtMs(endMs, showcaseDays)) {
        return FESTIVAL_BEAST_PHASE_CLAIM_SHOWCASE;
    }
    return FESTIVAL_BEAST_PHASE_HISTORY;
}

// 正式神兽表行和资源引用完整性门禁：缺失或不齐时禁止开放发放。
inline bool IsValidRewardPetRow(const PetTable* row) {
    if (row == nullptr || row->id() == 0 || row->name().empty() || row->model_id() == 0) {
        return false;
    }
    if (row->acquisition_tier() != 2 || row->cultivation_tier() != 4 ||
        row->transfer_policy() != 1 || row->exclusive_gift_slots() == 0 ||
        !row->fixed_initial_aptitude()) {
        return false;
    }
    if (row->aptitude_min_size() < 4 || row->aptitude_max_size() < 4) {
        return false;
    }
    for (int i = 0; i < 4; ++i) {
        if (row->aptitude_min(i) == 0 || row->aptitude_min(i) != row->aptitude_max(i)) {
            return false;
        }
    }
    return true;
}

// 等级档完整性门禁：非空、无重叠、无空洞、连续覆盖 1..120 级，且数值合法。
inline bool ValidateTiers(const std::vector<FestivalBeastTierTable>& tiers) {
    if (tiers.empty()) return false;
    std::vector<FestivalBeastTierTable> sorted = tiers;
    std::sort(sorted.begin(), sorted.end(), [](const FestivalBeastTierTable& a, const FestivalBeastTierTable& b) {
        return a.min_level() != b.min_level() ? a.min_level() < b.min_level() : a.id() < b.id();
    });
    uint32_t expectedMin = kRequiredMinLevel;
    for (const auto& tier : sorted) {
        if (tier.id() == 0 || tier.min_level() != expectedMin || tier.max_level() < tier.min_level()) {
            return false;
        }
        if (tier.boss_health() == 0 || tier.boss_physical_attack() == 0 ||
            tier.boss_speed() == 0 || tier.boss_resistance() > 100) {
            return false;
        }
        if (tier.max_level() == UINT32_MAX) return false;
        expectedMin = tier.max_level() + 1;
    }
    return expectedMin > kRequiredMaxLevel;
}

inline const FestivalBeastTierTable* FindTierForLevel(const std::vector<FestivalBeastTierTable>& tiers,
                                                      uint32_t ownerLevel) {
    const uint32_t clamped = std::max(kRequiredMinLevel, ownerLevel);
    for (const auto& tier : tiers) {
        if (clamped >= tier.min_level() && clamped <= tier.max_level()) {
            return &tier;
        }
    }
    return nullptr;
}

inline bool ValidateActivityRow(const FestivalBeastActivityTable& row,
                                const DungeonTable* dungeon,
                                const PetTable* rewardPet,
                                const std::vector<FestivalBeastTierTable>& tiers) {
    if (row.id() == 0 || row.config_revision() == 0 || row.activity_name().empty() ||
        row.boss_name().empty() || row.banner_asset_key().empty() || row.icon_key().empty() ||
        row.reward_identity().empty() || row.reward_definition_version() == 0 ||
        row.required_wins() == 0 || row.round_limit() == 0 || row.cycle_rounds() == 0 ||
        row.first_heavy_round() == 0 || row.first_heavy_round() > row.cycle_rounds() ||
        row.telegraph_lead_rounds() == 0 || row.telegraph_lead_rounds() >= row.first_heavy_round() ||
        row.heavy_damage_percent() == 0) {
        return false;
    }
    if (dungeon == nullptr || dungeon->id() != row.battle_config_id() ||
        dungeon->boss_trial_kind() != kFestivalBeastBossTrialKind ||
        dungeon->max_team_size() != 1 || dungeon->allow_pet_capture() != 0 ||
        dungeon->monster_size() == 0) {
        return false;
    }
    uint32_t nonZeroMonsters = 0;
    for (const uint32_t mid : dungeon->monster()) {
        if (mid != 0) ++nonZeroMonsters;
    }
    if (nonZeroMonsters != 1) return false;
    if (!IsValidRewardPetRow(rewardPet) || rewardPet->id() != row.reward_pet_table_id()) {
        return false;
    }
    if (!ValidateTiers(tiers)) {
        return false;
    }
    if (row.enabled()) {
        if (row.effective_start_at_ms() == 0 || row.effective_end_at_ms() <= row.effective_start_at_ms()) {
            return false;
        }
        // 正式单期必须严格为 14 个完整 24 小时。
        if (row.effective_end_at_ms() - row.effective_start_at_ms() != kRequiredOpenDurationMs) {
            return false;
        }
    }
    return true;
}

}  // namespace festival_beast_rules

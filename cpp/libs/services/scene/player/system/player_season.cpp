#include "player_season.h"

#include <algorithm>
#include <limits>
#include <string>
#include <vector>

#include <muduo/base/Logging.h>

#include "battle/system/player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_attribute.h"
#include "player/system/player_equipment_material_reward.h"
#include "player/system/player_level_rules.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/player_skill.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/equipment_effects.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_season_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/scene/player_season.pb.h"
#include "services/battle/constants/wuxing_expansion_rules.h"
#include "services/battle/system/season_rules.h"
#include "table/code/seasonrule_table.h"
#include "table/code/seasonseal_table.h"
#include "table/code/skill_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/season_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {

PlayerSeasonSystem::PersistFn gSeasonPersistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;

constexpr std::size_t kMaxTowerLeaderboardSize = 20u;
constexpr std::size_t kMaxBookmarkTextBytes = 48u;
constexpr int kMaxBookmarkSkills = 12;
constexpr int kMaxBookmarkSigils = 10;

uint32_t gTowerLeaderboardWeekKey = 0u;
std::vector<WuxingTowerLeaderboardEntry> gTowerWeeklyLeaderboard;

uint32_t ComputeWuxingTowerWeekKey(uint64_t nowMs) {
    // UTC+8 周一 05:00 换周：1970-01-01 00:00 UTC 为周四 08:00 UTC+8，偏移 +69h 对齐周一 05:00。
    constexpr uint64_t kMsPerWeek = 7ULL * 24ULL * 3600ULL * 1000ULL;
    constexpr uint64_t kMonday05CstOffsetMs = (3ULL * 24ULL - 3ULL) * 3600ULL * 1000ULL;
    return static_cast<uint32_t>((nowMs + kMonday05CstOffsetMs) / kMsPerWeek) + 1u;
}

std::string ClampUtf8Prefix(const std::string& raw, std::size_t maxBytes) {
    if (raw.size() <= maxBytes) {
        return raw;
    }
    std::size_t cut = maxBytes;
    while (cut > 0 && (static_cast<unsigned char>(raw[cut]) & 0xC0u) == 0x80u) {
        --cut;
    }
    return raw.substr(0, cut);
}

void CopyBookmarkToView(const SavedBattleBuildBookmark& src, SavedBattleBuildBookmarkView& dst) {
    dst.set_bookmark_id(src.bookmark_id());
    dst.set_title(src.title());
    dst.set_source_battle_id(src.source_battle_id());
    dst.set_source_player_id(src.source_player_id());
    dst.set_source_player_name(src.source_player_name());
    dst.set_class_id(src.class_id());
    dst.set_formation_id(src.formation_id());
    dst.set_season_seal_id(src.season_seal_id());
    dst.set_active_pet_table_id(src.active_pet_table_id());
    dst.set_active_pet_name(src.active_pet_name());
    dst.clear_skill_table_ids();
    for (const uint32_t id : src.skill_table_ids()) {
        dst.add_skill_table_ids(id);
    }
    dst.clear_wuxing_sigil_ids();
    for (const uint32_t id : src.wuxing_sigil_ids()) {
        dst.add_wuxing_sigil_ids(id);
    }
    dst.set_saved_at_ms(src.saved_at_ms());
}

void FillTowerState(entt::entity player, uint64_t nowMs, WuxingTowerStateSnapshot& out) {
    out.Clear();
    const uint32_t weekKey = ComputeWuxingTowerWeekKey(nowMs);
    out.set_week_key(weekKey);
    out.set_max_floors(turnbattle::wuxing::kTowerMaxFloors);

    uint32_t floor = 1u;
    if (tlsEcs.actorRegistry.valid(player)) {
        if (const auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
            seasonComp != nullptr && seasonComp->has_wuxing_tower() &&
            seasonComp->wuxing_tower().week_key() == weekKey) {
            const auto& tower = seasonComp->wuxing_tower();
            out.set_run_active(tower.run_active());
            out.set_run_id(tower.run_id());
            floor = (tower.current_floor() >= 1u && tower.current_floor() <= turnbattle::wuxing::kTowerMaxFloors)
                ? tower.current_floor()
                : 1u;
            out.set_run_total_rounds(tower.run_total_rounds());
            for (const uint32_t id : tower.active_sigil_ids()) {
                out.add_active_sigil_ids(id);
            }
            for (const uint32_t id : tower.pending_offer_sigil_ids()) {
                out.add_pending_offer_sigil_ids(id);
            }
            out.set_weekly_cleared_mask(tower.weekly_cleared_mask());
            out.set_weekly_best_floor(tower.weekly_best_floor());
            out.set_weekly_best_rounds(tower.weekly_best_rounds());
            out.set_revision(tower.revision());
        }
    }
    out.set_current_floor(floor);
    out.set_current_floor_dungeon_id(turnbattle::wuxing::TowerFloorDungeonId(floor));
    out.set_current_floor_leyline_element(turnbattle::wuxing::TowerFloorLeylineElement(weekKey, floor));

    if (gTowerLeaderboardWeekKey == weekKey) {
        for (std::size_t i = 0; i < gTowerWeeklyLeaderboard.size(); ++i) {
            auto* entry = out.add_weekly_leaderboard();
            *entry = gTowerWeeklyLeaderboard[i];
            entry->set_rank(static_cast<uint32_t>(i + 1u));
        }
    }
}

void UpsertTowerLeaderboard(uint32_t weekKey, uint64_t playerId, const std::string& playerName,
                            uint32_t bestFloor, uint32_t bestRounds, uint32_t formationId,
                            const google::protobuf::RepeatedField<uint32_t>& sigils) {
    if (playerId == 0 || bestFloor == 0) {
        return;
    }
    if (gTowerLeaderboardWeekKey != weekKey) {
        gTowerLeaderboardWeekKey = weekKey;
        gTowerWeeklyLeaderboard.clear();
    }
    WuxingTowerLeaderboardEntry updated;
    updated.set_player_id(playerId);
    updated.set_player_name(playerName);
    updated.set_best_floor(bestFloor);
    updated.set_best_rounds(bestRounds);
    updated.set_formation_id(formationId);
    for (const uint32_t id : sigils) {
        updated.add_sigil_ids(id);
    }

    auto it = std::find_if(gTowerWeeklyLeaderboard.begin(), gTowerWeeklyLeaderboard.end(),
                           [playerId](const WuxingTowerLeaderboardEntry& e) { return e.player_id() == playerId; });
    if (it != gTowerWeeklyLeaderboard.end()) {
        *it = std::move(updated);
    } else {
        gTowerWeeklyLeaderboard.emplace_back(std::move(updated));
    }

    std::sort(gTowerWeeklyLeaderboard.begin(), gTowerWeeklyLeaderboard.end(),
              [](const WuxingTowerLeaderboardEntry& lhs, const WuxingTowerLeaderboardEntry& rhs) {
                  if (lhs.best_floor() != rhs.best_floor()) {
                      return lhs.best_floor() > rhs.best_floor();
                  }
                  const uint32_t lr = lhs.best_rounds() == 0u ? std::numeric_limits<uint32_t>::max() : lhs.best_rounds();
                  const uint32_t rr = rhs.best_rounds() == 0u ? std::numeric_limits<uint32_t>::max() : rhs.best_rounds();
                  if (lr != rr) {
                      return lr < rr;
                  }
                  return lhs.player_id() < rhs.player_id();
              });
    if (gTowerWeeklyLeaderboard.size() > kMaxTowerLeaderboardSize) {
        gTowerWeeklyLeaderboard.resize(kMaxTowerLeaderboardSize);
    }
}

seasonrules::RuleParams ToParams(const SeasonRuleTable& row) {
    seasonrules::RuleParams params;
    params.seasonId = row.id();
    params.startsAtMs = row.starts_at_ms();
    params.endsAtMs = row.ends_at_ms();
    params.enabled = row.enabled();
    params.sealMaxPoints = row.seal_max_points();
    params.pveWinPoints = row.pve_win_points();
    params.pvpWinPoints = row.pvp_win_points();
    params.pvpOtherPoints = row.pvp_other_points();
    params.maxHealthBpsCap = row.max_health_bps_cap();
    params.defenseBpsCap = row.defense_bps_cap();
    params.catchupCurrencyType = row.catchup_currency_type();
    params.catchupPricePerPoint = row.catchup_price_per_point();
    params.defaultSealId = row.default_seal_id();
    params.cycleOrder = row.cycle_order();
    params.cycleDurationDays = row.cycle_duration_days();
    return params;
}

bool IsValidSealRow(const SeasonSealTable& seal) {
    return seasonrules::ValidSealDefinition(seal.season_id(), seal.id(), seal.effect_id(),
                                            seal.bonus_bps(), seal.cap_bps(), seal.per_round_limit(),
                                            seal.battle_limit(), seal.active_rounds(), seal.health_threshold_bps(),
                                            seal.resonance_skill_id(), seal.resonance_kind(), seal.resonance_param());
}

const SeasonSealTable* FindValidSealForSeason(uint32_t seasonId, uint32_t sealId) {
    if (seasonId == 0 || sealId == 0) {
        return nullptr;
    }
    const auto* row = SeasonSealTableManager::Instance().FindByIdSilent(sealId).first;
    if (row == nullptr || row->season_id() != seasonId || !IsValidSealRow(*row)) {
        return nullptr;
    }
    return row;
}

struct ActiveSeasonResolution {
    uint32_t ruleId{0};
    seasonrules::ResolvedSeason resolved;

    const SeasonRuleTable* Rule() const {
        return ruleId == 0u ? nullptr : SeasonRuleTableManager::Instance().FindByIdSilent(ruleId).first;
    }
};

// 解析当前时刻唯一有效的活跃赛季（支持固定单季窗与无限期号自动循环；非法配置 fail-closed 返回空）。
ActiveSeasonResolution ResolveCurrentSeason(uint64_t nowMs) {
    ActiveSeasonResolution out;
    const auto& rows = SeasonRuleTableManager::Instance().FindAll().data();
    if (rows.empty()) {
        return out;
    }
    std::vector<seasonrules::RuleParams> params;
    params.reserve(static_cast<std::size_t>(rows.size()));
    for (const auto& row : rows) {
        params.push_back(ToParams(row));
    }
    const auto resolved = seasonrules::ResolveActiveSeason(params, nowMs);
    if (!resolved.active || resolved.invalidConfig || resolved.ruleIndex >= static_cast<std::size_t>(rows.size())) {
        return out;
    }
    const auto& activeRow = rows[static_cast<int>(resolved.ruleIndex)];
    if (FindValidSealForSeason(activeRow.id(), activeRow.default_seal_id()) == nullptr) {
        return out;
    }
    out.ruleId = activeRow.id();
    out.resolved = resolved;
    return out;
}

const PlayerSeasonSealRecord* FindRecord(const PlayerSeasonComp& comp, uint32_t seasonId) {
    for (const auto& record : comp.records()) {
        if (record.season_id() == seasonId) {
            return &record;
        }
    }
    return nullptr;
}

PlayerSeasonSealRecord* FindOrAddRecord(PlayerSeasonComp& comp, uint32_t seasonId) {
    for (auto& record : *comp.mutable_records()) {
        if (record.season_id() == seasonId) {
            return &record;
        }
    }
    auto* added = comp.add_records();
    added->set_season_id(seasonId);
    return added;
}

bool IsSeasonHonorUnlocked(const PlayerSeasonComp& comp, uint32_t seasonId) {
    if (seasonId == 0u) {
        return false;
    }
    if (seasonrules::IsHonorBitSet(comp.unlocked_honor_words(), seasonId)) {
        return true;
    }
    const auto* rule = SeasonRuleTableManager::Instance().FindByIdSilent(seasonId).first;
    if (rule == nullptr || rule->seal_max_points() == 0u) {
        return false;
    }
    const auto* rec = FindRecord(comp, seasonId);
    return rec != nullptr && rec->progress_points() >= rule->seal_max_points();
}

void SyncLegacyHonorsToBitmap(PlayerSeasonComp& comp) {
    for (const auto& rec : comp.records()) {
        if (rec.season_id() == 0u) {
            continue;
        }
        const auto* rule = SeasonRuleTableManager::Instance().FindByIdSilent(rec.season_id()).first;
        if (rule != nullptr && rule->seal_max_points() > 0u && rec.progress_points() >= rule->seal_max_points()) {
            seasonrules::SetHonorBitRepeated(comp.mutable_unlocked_honor_words(), rec.season_id());
            comp.mutable_retro_honor_progress()->erase(rec.season_id());
            if (comp.retro_honor_target_season_id() == rec.season_id()) {
                comp.set_retro_honor_target_season_id(0u);
            }
        }
    }
}

struct EffectiveProgress {
    uint32_t progress{0};
    uint32_t battlePoints{0};
    uint32_t catchupPoints{0};
};

EffectiveProgress ReadEffectiveTermProgress(const PlayerSeasonSealRecord* rec,
                                            uint32_t currentTerm, uint32_t maxPoints) {
    EffectiveProgress out;
    if (rec == nullptr || maxPoints == 0u) {
        return out;
    }
    const bool sameTerm = (rec->last_term() == currentTerm) || (rec->last_term() == 0u && currentTerm == 1u);
    if (!sameTerm) {
        return out;
    }
    out.progress = std::min(rec->progress_points(), maxPoints);
    out.battlePoints = std::min(rec->battle_points(), out.progress);
    out.catchupPoints = std::min(rec->catchup_points(), out.progress);
    return out;
}

PlayerSeasonSealRecord* SyncRecordForTerm(PlayerSeasonComp& comp, const SeasonRuleTable& rule, uint32_t currentTerm) {
    SyncLegacyHonorsToBitmap(comp);
    auto* record = FindOrAddRecord(comp, rule.id());
    if (record->last_term() == 0u && currentTerm == 1u) {
        record->set_last_term(1u);
    } else if (record->last_term() != currentTerm) {
        record->set_progress_points(0u);
        record->set_battle_points(0u);
        record->set_catchup_points(0u);
        record->set_last_term(currentTerm);
    }
    return record;
}

std::vector<uint32_t> AllConfiguredSeasonIdsAsc() {
    std::vector<uint32_t> ids;
    for (const auto& row : SeasonRuleTableManager::Instance().FindAll().data()) {
        if (row.id() != 0u && row.seal_max_points() > 0u) {
            ids.push_back(row.id());
        }
    }
    std::sort(ids.begin(), ids.end());
    ids.erase(std::unique(ids.begin(), ids.end()), ids.end());
    return ids;
}

uint32_t ResolveRetroHonorTarget(const PlayerSeasonComp& comp, uint32_t activeSeasonId,
                                 bool* outExplicit = nullptr) {
    if (outExplicit != nullptr) {
        *outExplicit = false;
    }
    const uint32_t explicitId = comp.retro_honor_target_season_id();
    if (explicitId != 0u && explicitId != activeSeasonId) {
        const auto* rule = SeasonRuleTableManager::Instance().FindByIdSilent(explicitId).first;
        if (rule != nullptr && rule->seal_max_points() > 0u && !IsSeasonHonorUnlocked(comp, explicitId)) {
            if (outExplicit != nullptr) {
                *outExplicit = true;
            }
            return explicitId;
        }
    }
    for (const uint32_t sid : AllConfiguredSeasonIdsAsc()) {
        if (sid != activeSeasonId && !IsSeasonHonorUnlocked(comp, sid)) {
            return sid;
        }
    }
    return 0u;
}

uint32_t EffectiveSealId(const SeasonRuleTable& rule, const PlayerSeasonSealRecord* record) {
    if (record != nullptr && record->equipped_seal_id() != 0 &&
        FindValidSealForSeason(rule.id(), record->equipped_seal_id()) != nullptr) {
        return record->equipped_seal_id();
    }
    return rule.default_seal_id();
}

uint32_t CheckWritable(entt::entity player) {
    if (!player_mutability::IsPlayerMutable(player)) {
        return kSeasonFrozen;
    }
    if (PlayerBattleSystem::IsInBattle(player)) {
        return kSeasonInBattle;
    }
    return kSuccess;
}

uint32_t BattlePointsFor(const SeasonRuleTable& rule, const BattleSettlementData& settlement) {
    const bool playerWon =
        (settlement.outcome() == BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 0) ||
        (settlement.outcome() == BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 1);
    switch (settlement.match_mode()) {
    case match::MATCH_MODE_PVE_SOLO:
    case match::MATCH_MODE_PVE_TEAM:
        return playerWon ? rule.pve_win_points() : 0u;
    case match::MATCH_MODE_1V1:
    case match::MATCH_MODE_5V5:
    case match::MATCH_MODE_PVP_TEAM:
        return playerWon ? rule.pvp_win_points() : rule.pvp_other_points();
    default:
        return 0u;
    }
}

void FillHonors(const PlayerSeasonComp& seasonComp, uint32_t activeSeasonId,
                uint32_t activeTerm, uint32_t retroTargetSeasonId, SeasonStateSnapshot& out) {
    // 永久零数值纪念印记与称谓：全量返回已配置赛季（按 id 升序），支持十季荣誉墙与一键切换往季追忆目标。
    for (const uint32_t sid : AllConfiguredSeasonIdsAsc()) {
        const auto* rule = SeasonRuleTableManager::Instance().FindByIdSilent(sid).first;
        if (rule == nullptr || rule->seal_max_points() == 0u) {
            continue;
        }
        const bool unlocked = IsSeasonHonorUnlocked(seasonComp, sid);
        const bool isCurrent = sid == activeSeasonId;
        uint32_t retroPts = 0u;
        if (!unlocked) {
            if (const auto it = seasonComp.retro_honor_progress().find(sid);
                it != seasonComp.retro_honor_progress().end()) {
                retroPts = std::min(it->second, rule->seal_max_points());
            }
        }
        uint32_t progress = 0u;
        if (unlocked) {
            progress = rule->seal_max_points();
        } else if (isCurrent) {
            progress = ReadEffectiveTermProgress(FindRecord(seasonComp, sid), activeTerm, rule->seal_max_points()).progress;
        } else {
            progress = retroPts;
        }
        auto* honor = out.add_honors();
        honor->set_season_id(sid);
        honor->set_season_name(rule->name());
        honor->set_honor_title_name(rule->honor_title_name());
        honor->set_honor_badge_icon_key(rule->honor_badge_icon_key());
        honor->set_progress_points(progress);
        honor->set_max_points(rule->seal_max_points());
        honor->set_unlocked(unlocked);
        honor->set_is_current_season(isCurrent);
        honor->set_is_retro_target(!unlocked && sid == retroTargetSeasonId);
        honor->set_retro_progress_points(retroPts);
    }
}

void FillWeeklyAffix(const ActiveSeasonResolution& active, uint64_t nowMs,
                     const std::vector<const SeasonSealTable*>& validSeals,
                     SeasonWeeklyAffixView& out) {
    out.Clear();
    if (active.ruleId == 0u) {
        return;
    }
    const uint32_t affixSeed = active.resolved.cyclePoolSize > 0u ? active.resolved.seasonTerm : active.ruleId;
    const uint32_t weekIndex = seasonrules::ComputeSeasonWeekIndex(active.resolved.termStartMs, nowMs);
    const uint32_t affixId = seasonrules::WeeklyAffixForWeek(affixSeed, weekIndex);
    if (!seasonrules::IsValidWeeklyAffixId(affixId)) {
        return;
    }
    out.set_week_index(weekIndex);
    out.set_affix_id(affixId);
    switch (affixId) {
    case seasonrules::kWeeklyAffixShieldOverload:
        out.set_name("护盾过载周");
        out.set_description("全员对称：本季常规对局中双方技能施加的护盾量额外 +15%。");
        out.set_strategy_tip("推荐搭配破盾赛季印与破甲/护盾技能抢占节奏。");
        break;
    case seasonrules::kWeeklyAffixSpiritRecovery:
        out.set_name("灵息充盈周");
        out.set_description("全员对称：本季常规对局中双方执行普通防御额外回复 3% 最大法力。");
        out.set_strategy_tip("推荐搭配聚息援印或固阵守印打稳守蓄蓝反击。");
        break;
    case seasonrules::kWeeklyAffixOpeningGuard:
        out.set_name("先机守势周");
        out.set_description("全员对称：本季常规对局前 2 回合双方受到的直伤降低 8%。");
        out.set_strategy_tip("前两回合爆发更克制，推荐蓄势增益或固阵守印后发制人。");
        break;
    case seasonrules::kWeeklyAffixSharpTempo:
        out.set_name("锋芒竞速周");
        out.set_description("全员对称：本季常规对局中攻击无护盾目标的直伤提升 5%。");
        out.set_strategy_tip("先破盾再集火收益更高，推荐破阵锐印/玄印联动群攻技。");
        break;
    default:
        break;
    }
    for (const auto* seal : validSeals) {
        if (seal != nullptr && seasonrules::IsSealEffectRecommendedForAffix(affixId, seal->effect_id())) {
            out.add_recommended_seal_ids(seal->id());
        }
    }
}

uint32_t FillState(entt::entity player, uint64_t nowMs, SeasonStateSnapshot& out) {
    out.Clear();
    out.set_server_now_ms(nowMs);
    if (!tlsEcs.actorRegistry.valid(player)) {
        return kEntityIsNull;
    }
    const auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
    if (seasonComp == nullptr || levelComp == nullptr) {
        return kSeasonUnavailable;
    }
    // 阵法与配装收藏夹与赛季开关解耦，始终随赛季状态一并返回。
    out.set_equipped_formation_id(
        turnbattle::wuxing::IsValidFormationId(seasonComp->equipped_formation_id())
            ? seasonComp->equipped_formation_id()
            : 0u);
    for (const auto& bm : seasonComp->saved_builds()) {
        CopyBookmarkToView(bm, *out.add_saved_builds());
    }
    const auto active = ResolveCurrentSeason(nowMs);
    const auto* rule = active.Rule();
    bool retroExplicit = false;
    const uint32_t retroTargetId = ResolveRetroHonorTarget(
        *seasonComp, rule != nullptr ? rule->id() : 0u, &retroExplicit);
    out.set_retro_honor_target_season_id(retroTargetId);
    out.set_retro_honor_target_explicit(retroExplicit);
    if (rule == nullptr) {
        FillHonors(*seasonComp, 0u, 0u, retroTargetId, out);
        out.set_available(false);
        out.set_unavailable_reason("当前暂无进行中的赛季");
        return kSuccess;
    }
    FillHonors(*seasonComp, rule->id(), active.resolved.seasonTerm, retroTargetId, out);

    const auto* record = FindRecord(*seasonComp, rule->id());
    const uint32_t equippedId = EffectiveSealId(*rule, record);
    const auto eff = ReadEffectiveTermProgress(record, active.resolved.seasonTerm, rule->seal_max_points());
    const uint32_t progress = eff.progress;
    const uint32_t battlePts = eff.battlePoints;
    const uint32_t catchupPts = eff.catchupPoints;
    const uint32_t healthBps = seasonrules::ScaledBonusBps(progress, rule->seal_max_points(), rule->max_health_bps_cap());
    const uint32_t defenseBps = seasonrules::ScaledBonusBps(progress, rule->seal_max_points(), rule->defense_bps_cap());

    out.set_available(true);
    out.set_season_id(rule->id());
    out.set_season_name(rule->name());
    out.set_season_desc(rule->desc());
    out.set_starts_at_ms(active.resolved.termStartMs);
    out.set_ends_at_ms(active.resolved.termEndMs);
    out.set_season_term(active.resolved.seasonTerm);
    out.set_cycle_index(active.resolved.cycleIndex);
    out.set_cycle_pool_size(active.resolved.cyclePoolSize);

    out.set_equipped_seal_id(equippedId);
    out.set_progress_points(progress);
    out.set_max_points(rule->seal_max_points());
    out.set_battle_points(battlePts);
    out.set_catchup_points(catchupPts);
    out.set_current_max_health_bps(healthBps);
    out.set_cap_max_health_bps(rule->max_health_bps_cap());
    out.set_current_defense_bps(defenseBps);
    out.set_cap_defense_bps(rule->defense_bps_cap());
    out.set_pve_win_points(rule->pve_win_points());
    out.set_pvp_win_points(rule->pvp_win_points());
    out.set_pvp_other_points(rule->pvp_other_points());

    out.set_catchup_currency_type(rule->catchup_currency_type());
    out.set_catchup_price_per_point(rule->catchup_price_per_point());
    seasonrules::CatchupPlan fullPlan;
    const bool hasCatchup = seasonrules::ComputeCatchupPlan(
        progress, rule->seal_max_points(), 0, rule->catchup_price_per_point(), fullPlan);
    out.set_catchup_remaining_total_price(hasCatchup ? fullPlan.totalPrice : 0u);
    out.set_currency_balance(CurrencySystem::GetBalance(
        player, static_cast<CurrencyType>(rule->catchup_currency_type())));
    out.set_revision(record != nullptr ? record->revision() : 0u);

    const uint32_t writableErr = CheckWritable(player);
    out.set_can_switch_seal(writableErr == kSuccess);
    out.set_can_catchup(writableErr == kSuccess && hasCatchup);
    if (writableErr == kSeasonInBattle) {
        out.set_action_unavailable_reason("战斗中暂不能更换或追赶赛季印，请在结算后重试");
    } else if (writableErr != kSuccess) {
        out.set_action_unavailable_reason("当前角色状态暂时不能操作赛季印，请稍后再试");
    }

    // 本季可选赛季印列表（按 id 升序输出，确保跨端展示稳定）
    std::vector<const SeasonSealTable*> seals;
    for (const auto* seal : SeasonSealTableManager::Instance().GetBySeasonId(rule->id())) {
        if (seal != nullptr && IsValidSealRow(*seal)) {
            seals.push_back(seal);
        }
    }
    std::sort(seals.begin(), seals.end(), [](const SeasonSealTable* lhs, const SeasonSealTable* rhs) {
        return lhs->id() < rhs->id();
    });
    for (const auto* seal : seals) {
        auto* opt = out.add_seals();
        opt->set_seal_id(seal->id());
        opt->set_name(seal->name());
        opt->set_description(seal->description());
        opt->set_icon_key(seal->icon_key());
        opt->set_effect_id(seal->effect_id());
        opt->set_bonus_bps(seal->bonus_bps());
        opt->set_cap_bps(seal->cap_bps());
        opt->set_per_round_limit(seal->per_round_limit());
        opt->set_battle_limit(seal->battle_limit());
        opt->set_active_rounds(seal->active_rounds());
        opt->set_health_threshold_bps(seal->health_threshold_bps());
        opt->set_equipped(seal->id() == equippedId);
        opt->set_resonance_skill_id(seal->resonance_skill_id());
        opt->set_resonance_kind(seal->resonance_kind());
        opt->set_resonance_param(seal->resonance_param());
        opt->set_resonance_desc(seal->resonance_desc());
    }
    FillWeeklyAffix(active, nowMs, seals, *out.mutable_weekly_affix());

    // 本季免费限定技能列表（按 skill_table_id 升序）
    const uint32_t level = playerlevel::CappedLevel(levelComp->level());
    std::vector<const SkillTable*> seasonSkills;
    for (const auto& skill : SkillTableManager::Instance().FindAll().data()) {
        if (skill.season_id() == rule->id() && PlayerSkillSystem::IsFreeSkillFor(skill, 1)) {
            seasonSkills.push_back(&skill);
        }
    }
    std::sort(seasonSkills.begin(), seasonSkills.end(), [](const SkillTable* lhs, const SkillTable* rhs) {
        return lhs->id() < rhs->id();
    });
    for (const auto* skill : seasonSkills) {
        const uint32_t effRank = seasonrules::EffectiveSeasonalSkillRank(level, skill->unlock_level(), skill->max_rank());
        auto* entry = out.add_season_skills();
        entry->set_skill_table_id(skill->id());
        entry->set_name(skill->name());
        entry->set_description(skill->description());
        entry->set_icon_key(skill->icon_key());
        entry->set_category(skill->category());
        entry->set_target_team(skill->target_team());
        entry->set_max_targets(skill->max_targets());
        entry->set_mana_percent(skill->mana_percent());
        entry->set_cooldown_rounds(skill->cooldown_rounds());
        entry->set_unlock_level(skill->unlock_level());
        entry->set_max_rank(skill->max_rank());
        entry->set_effective_rank(effRank);
        entry->set_unlocked(effRank > 0);
        entry->set_ability_kind(skill->ability_kind());
        entry->set_caster_mask(skill->caster_mask());
        entry->set_action_tags(skill->action_tags());
    }
    return kSuccess;
}

} // namespace

bool PlayerSeasonSystem::ValidateTables() {
    const auto& rules = SeasonRuleTableManager::Instance().FindAll().data();
    const auto& seals = SeasonSealTableManager::Instance().FindAll().data();
    if (rules.empty()) {
        if (!seals.empty()) {
            LOG_ERROR << "[PlayerSeason] table validation failed: SeasonSeal has rows while SeasonRule is empty";
            return false;
        }
        return true;
    }

    std::vector<seasonrules::RuleParams> ruleParams;
    ruleParams.reserve(static_cast<std::size_t>(rules.size()));
    for (const auto& rule : rules) {
        ruleParams.push_back(ToParams(rule));
        if (FindValidSealForSeason(rule.id(), rule.default_seal_id()) == nullptr) {
            LOG_ERROR << "[PlayerSeason] table validation failed: default_seal_id=" << rule.default_seal_id()
                      << " missing or invalid for season_id=" << rule.id();
            return false;
        }
    }
    if (const char* err = seasonrules::ValidateSchedule(ruleParams); err != nullptr) {
        LOG_ERROR << "[PlayerSeason] table validation failed: schedule error=" << err;
        return false;
    }

    const bool hasSkillsLoaded = !SkillTableManager::Instance().FindAll().data().empty();
    for (const auto& seal : seals) {
        if (!SeasonRuleTableManager::Instance().Exists(seal.season_id())) {
            LOG_ERROR << "[PlayerSeason] table validation failed: seal_id=" << seal.id()
                      << " references missing season_id=" << seal.season_id();
            return false;
        }
        if (!IsValidSealRow(seal)) {
            LOG_ERROR << "[PlayerSeason] table validation failed: seal_id=" << seal.id()
                      << " has invalid effect or resonance parameters";
            return false;
        }
        if (hasSkillsLoaded && seal.resonance_kind() != seasonrules::kResonanceNone) {
            const auto* skill = SkillTableManager::Instance().FindByIdSilent(seal.resonance_skill_id()).first;
            if (skill == nullptr || skill->season_id() != seal.season_id() || skill->battle_model() != 1u) {
                LOG_ERROR << "[PlayerSeason] table validation failed: seal_id=" << seal.id()
                          << " resonance_skill_id=" << seal.resonance_skill_id()
                          << " missing or season mismatch for season_id=" << seal.season_id();
                return false;
            }
        }
    }
    return true;
}

uint32_t PlayerSeasonSystem::ActiveSeasonId(uint64_t nowMs) {
    const auto active = ResolveCurrentSeason(nowMs);
    return active.ruleId;
}

uint32_t PlayerSeasonSystem::ActiveWeeklyAffixId(uint64_t nowMs) {
    const auto active = ResolveCurrentSeason(nowMs);
    if (active.ruleId == 0u) {
        return seasonrules::kWeeklyAffixNone;
    }
    const uint32_t affixSeed = active.resolved.cyclePoolSize > 0u ? active.resolved.seasonTerm : active.ruleId;
    return seasonrules::WeeklyAffixAt(affixSeed, active.resolved.termStartMs, nowMs);
}

uint32_t PlayerSeasonSystem::EquippedFormationId(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player)) {
        return 0u;
    }
    const auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr || !turnbattle::wuxing::IsValidFormationId(seasonComp->equipped_formation_id())) {
        return 0u;
    }
    return seasonComp->equipped_formation_id();
}

bool PlayerSeasonSystem::IsEligibleBattle(const BattleSettlementData& settlement) {
    if (settlement.battle_id() == 0 || settlement.player_id() == 0 || settlement.fled() ||
        settlement.total_rounds() == 0 || settlement.player_team_index() > 1 ||
        (settlement.boss_trial_kind() >= 9u && settlement.boss_trial_kind() <= 11u) ||
        (settlement.outcome() != BATTLE_OUTCOME_SIDE_A_WIN &&
         settlement.outcome() != BATTLE_OUTCOME_SIDE_B_WIN &&
         settlement.outcome() != BATTLE_OUTCOME_DRAW)) {
        return false;
    }
    switch (settlement.match_mode()) {
    case match::MATCH_MODE_PVE_SOLO:
    case match::MATCH_MODE_PVE_TEAM:
        return (settlement.outcome() == BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 0) ||
               (settlement.outcome() == BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 1);
    case match::MATCH_MODE_1V1:
    case match::MATCH_MODE_5V5:
    case match::MATCH_MODE_PVP_TEAM: {
        if (!settlement.competitive_ranked()) {
            return true;
        }
        if (!settlement.participated()) {
            return false;
        }
        const bool playerWon =
            (settlement.outcome() == BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 0) ||
            (settlement.outcome() == BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 1);
        if (playerWon) {
            return true;
        }
        // 正式排位败局/平局复用材料总账的防挂机门槛，防止秒退/纯托管白嫖赛季印进度点。
        return settlement.total_rounds() >= PlayerEquipmentMaterialRewardSystem::kPvpMinRoundsForLoss &&
               settlement.manual_action_rounds() >= PlayerEquipmentMaterialRewardSystem::kPvpMinManualRoundsForLoss &&
               settlement.effective_combat_contribution();
    }
    default:
        return false;
    }
}

uint32_t PlayerSeasonSystem::PrepareBattleCredit(const PlayerSeasonComp& current,
                                                 const BattleSettlementData& settlement,
                                                 uint64_t nowMs,
                                                 PlayerSeasonComp& candidate,
                                                 bool& pointsIncreased,
                                                 bool* outTowerChanged,
                                                 seasonrules::BattleCreditDelta* outDelta) {
    candidate = current;
    pointsIncreased = false;
    if (outTowerChanged != nullptr) {
        *outTowerChanged = false;
    }
    if (outDelta != nullptr) {
        *outDelta = seasonrules::BattleCreditDelta{};
    }
    // 五行幻境周常爬塔结算：仅 A 方胜利且 (week_key, run_id, floor) 与进行中轮次一致时推进首通位图并抽取三选一符印。
    if (settlement.has_tower_settlement() && settlement.tower_settlement().floor() != 0u) {
        const auto& ts = settlement.tower_settlement();
        const bool won = settlement.outcome() == BATTLE_OUTCOME_SIDE_A_WIN &&
                         settlement.player_team_index() == 0u && !settlement.fled();
        if (!won || !candidate.has_wuxing_tower()) {
            return kSuccess;
        }
        auto* tower = candidate.mutable_wuxing_tower();
        if (!tower->run_active() || tower->week_key() != ts.week_key() || tower->run_id() != ts.run_id() ||
            tower->current_floor() != ts.floor() || tower->pending_offer_sigil_ids_size() != 0 ||
            ts.floor() < 1u || ts.floor() > turnbattle::wuxing::kTowerMaxFloors) {
            return kSuccess;
        }
        tower->set_weekly_cleared_mask(tower->weekly_cleared_mask() | (1u << (ts.floor() - 1u)));
        tower->set_run_total_rounds(tower->run_total_rounds() + std::max<uint32_t>(1u, ts.total_rounds()));
        if (ts.floor() > tower->weekly_best_floor()) {
            tower->set_weekly_best_floor(ts.floor());
            if (ts.floor() == turnbattle::wuxing::kTowerMaxFloors) {
                tower->set_weekly_best_rounds(tower->run_total_rounds());
            }
        } else if (ts.floor() == turnbattle::wuxing::kTowerMaxFloors &&
                   (tower->weekly_best_rounds() == 0u || tower->run_total_rounds() < tower->weekly_best_rounds())) {
            tower->set_weekly_best_rounds(tower->run_total_rounds());
        }

        if (ts.floor() >= turnbattle::wuxing::kTowerMaxFloors) {
            tower->set_run_active(false);
            tower->clear_pending_offer_sigil_ids();
        } else {
            std::vector<uint32_t> owned(tower->active_sigil_ids().begin(), tower->active_sigil_ids().end());
            const auto offer = turnbattle::wuxing::RollTowerSigilOffer(
                ts.week_key(), ts.run_id(), ts.floor(), owned);
            tower->clear_pending_offer_sigil_ids();
            for (const uint32_t id : offer) {
                tower->add_pending_offer_sigil_ids(id);
            }
        }
        if (tower->revision() < std::numeric_limits<uint64_t>::max()) {
            tower->set_revision(tower->revision() + 1u);
        }
        if (outTowerChanged != nullptr) {
            *outTowerChanged = true;
        }
        return kSuccess;
    }
    if (!IsEligibleBattle(settlement)) {
        return kSuccess;
    }
    const auto active = ResolveCurrentSeason(nowMs);
    const auto* rule = active.Rule();
    if (rule == nullptr) {
        return kSuccess;
    }
    const uint32_t gain = BattlePointsFor(*rule, settlement);
    if (gain == 0) {
        return kSuccess;
    }
    auto* record = SyncRecordForTerm(candidate, *rule, active.resolved.seasonTerm);
    if (record->equipped_seal_id() == 0) {
        record->set_equipped_seal_id(rule->default_seal_id());
    }
    const uint32_t before = std::min(record->progress_points(), rule->seal_max_points());
    const bool wasAlreadyUnlocked = IsSeasonHonorUnlocked(candidate, rule->id());
    const uint32_t usedForCurrent = before < rule->seal_max_points()
        ? std::min(gain, rule->seal_max_points() - before)
        : 0u;
    const uint32_t after = before + usedForCurrent;
    bool currentHonorNewlyUnlocked = false;
    if (usedForCurrent > 0u) {
        record->set_progress_points(after);
        record->set_battle_points(record->battle_points() + usedForCurrent);
        pointsIncreased = true;
        if (after >= rule->seal_max_points()) {
            const bool newlySet = seasonrules::SetHonorBitRepeated(candidate.mutable_unlocked_honor_words(), rule->id());
            currentHonorNewlyUnlocked = newlySet && !wasAlreadyUnlocked;
            candidate.mutable_retro_honor_progress()->erase(rule->id());
            if (candidate.retro_honor_target_season_id() == rule->id()) {
                candidate.set_retro_honor_target_season_id(0u);
            }
        }
    }
    if (outDelta != nullptr) {
        outDelta->seasonId = rule->id();
        outDelta->pointsAdded = usedForCurrent;
        outDelta->progressAfter = after;
        outDelta->maxPoints = rule->seal_max_points();
        outDelta->honorNewlyUnlocked = currentHonorNewlyUnlocked;
    }

    // 满级溢出点数自动流向「往季徽章追忆」（纯玩法免费，永不涉及灵石购买）。
    const uint32_t overflow = gain - usedForCurrent;
    if (overflow > 0u) {
        const uint32_t retroTargetId = ResolveRetroHonorTarget(candidate, rule->id());
        if (retroTargetId != 0u) {
            const auto* targetRule = SeasonRuleTableManager::Instance().FindByIdSilent(retroTargetId).first;
            if (targetRule != nullptr && targetRule->seal_max_points() > 0u) {
                const uint32_t targetMax = targetRule->seal_max_points();
                uint32_t prevRetro = 0u;
                if (const auto it = candidate.retro_honor_progress().find(retroTargetId);
                    it != candidate.retro_honor_progress().end()) {
                    prevRetro = std::min(it->second, targetMax);
                }
                const uint32_t retroAdded = prevRetro < targetMax ? std::min(overflow, targetMax - prevRetro) : 0u;
                const uint32_t retroAfter = prevRetro + retroAdded;
                bool retroUnlocked = false;
                if (retroAdded > 0u) {
                    pointsIncreased = true;
                    if (retroAfter >= targetMax) {
                        retroUnlocked = seasonrules::SetHonorBitRepeated(
                            candidate.mutable_unlocked_honor_words(), retroTargetId);
                        candidate.mutable_retro_honor_progress()->erase(retroTargetId);
                        if (candidate.retro_honor_target_season_id() == retroTargetId) {
                            candidate.set_retro_honor_target_season_id(0u);
                        }
                    } else {
                        (*candidate.mutable_retro_honor_progress())[retroTargetId] = retroAfter;
                    }
                }
                if (outDelta != nullptr && retroAdded > 0u) {
                    outDelta->retroHonorSeasonId = retroTargetId;
                    outDelta->retroHonorPointsAdded = retroAdded;
                    outDelta->retroHonorProgressAfter = retroAfter;
                    outDelta->retroHonorMaxPoints = targetMax;
                    outDelta->retroHonorNewlyUnlocked = retroUnlocked;
                }
            }
        }
    }
    return kSuccess;
}

void PlayerSeasonSystem::CollectEffects(entt::entity player, uint64_t nowMs, EquipmentEffects& effects) {
    effects.clear_season_seal();
    effects.set_season_max_health_bps(0);
    effects.set_season_defense_bps(0);
    if (!tlsEcs.actorRegistry.valid(player)) {
        return;
    }
    const auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        return;
    }
    const auto active = ResolveCurrentSeason(nowMs);
    const auto* rule = active.Rule();
    if (rule == nullptr) {
        return;
    }
    const auto* record = FindRecord(*seasonComp, rule->id());
    const uint32_t sealId = EffectiveSealId(*rule, record);
    const auto* seal = FindValidSealForSeason(rule->id(), sealId);
    if (seal == nullptr) {
        return;
    }
    const uint32_t progress = ReadEffectiveTermProgress(record, active.resolved.seasonTerm, rule->seal_max_points()).progress;
    effects.set_season_max_health_bps(
        seasonrules::ScaledBonusBps(progress, rule->seal_max_points(), rule->max_health_bps_cap()));
    effects.set_season_defense_bps(
        seasonrules::ScaledBonusBps(progress, rule->seal_max_points(), rule->defense_bps_cap()));

    auto* dst = effects.mutable_season_seal();
    dst->set_season_id(rule->id());
    dst->set_seal_id(seal->id());
    dst->set_effect_id(seal->effect_id());
    dst->set_bonus_bps(seal->bonus_bps());
    dst->set_cap_bps(seal->cap_bps());
    dst->set_per_round_limit(seal->per_round_limit());
    dst->set_battle_limit(seal->battle_limit());
    dst->set_active_rounds(seal->active_rounds());
    dst->set_health_threshold_bps(seal->health_threshold_bps());
    dst->set_name(seal->name());
    dst->set_resonance_skill_id(seal->resonance_skill_id());
    dst->set_resonance_kind(seal->resonance_kind());
    dst->set_resonance_param(seal->resonance_param());
}

void PlayerSeasonSystem::CollectAvailableSeals(uint64_t nowMs, std::vector<SeasonSealEffect>& out) {
    out.clear();
    const auto active = ResolveCurrentSeason(nowMs);
    const auto* rule = active.Rule();
    if (rule == nullptr) {
        return;
    }
    std::vector<uint32_t> sealIds;
    for (const auto& seal : SeasonSealTableManager::Instance().FindAll().data()) {
        if (seal.season_id() == rule->id() && IsValidSealRow(seal)) {
            sealIds.push_back(seal.id());
        }
    }
    std::sort(sealIds.begin(), sealIds.end());
    for (const uint32_t sealId : sealIds) {
        const auto* seal = FindValidSealForSeason(rule->id(), sealId);
        if (seal == nullptr) {
            continue;
        }
        SeasonSealEffect effect;
        effect.set_season_id(rule->id());
        effect.set_seal_id(seal->id());
        effect.set_effect_id(seal->effect_id());
        effect.set_bonus_bps(seal->bonus_bps());
        effect.set_cap_bps(seal->cap_bps());
        effect.set_per_round_limit(seal->per_round_limit());
        effect.set_battle_limit(seal->battle_limit());
        effect.set_active_rounds(seal->active_rounds());
        effect.set_health_threshold_bps(seal->health_threshold_bps());
        effect.set_name(seal->name());
        effect.set_resonance_skill_id(seal->resonance_skill_id());
        effect.set_resonance_kind(seal->resonance_kind());
        effect.set_resonance_param(seal->resonance_param());
        out.emplace_back(std::move(effect));
    }
}

uint32_t PlayerSeasonSystem::GetState(entt::entity player, uint64_t nowMs, GetSeasonStateResponse& response) {
    response.Clear();
    return FillState(player, nowMs, *response.mutable_state());
}

void PlayerSeasonSystem::EquipSeal(entt::entity player, uint64_t nowMs,
                                   const EquipSeasonSealRequest& request,
                                   EquipSeasonSealResponse& response) {
    response.Clear();
    const auto finish = [&](uint32_t err) {
        if (err != kSuccess) {
            response.mutable_error_message()->set_id(err);
        }
        if (const uint32_t stateErr = FillState(player, nowMs, *response.mutable_state()); stateErr != kSuccess) {
            response.clear_state();
            if (err == kSuccess) {
                response.mutable_error_message()->set_id(stateErr);
            }
        }
    };

    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        finish(kSeasonUnavailable);
        return;
    }
    const auto active = ResolveCurrentSeason(nowMs);
    const auto* rule = active.Rule();
    if (rule == nullptr) {
        finish(kSeasonNotOpen);
        return;
    }
    if (request.season_id() == 0 || request.season_id() != rule->id()) {
        finish(kSeasonMismatch);
        return;
    }
    if (FindValidSealForSeason(rule->id(), request.seal_id()) == nullptr) {
        finish(kSeasonSealInvalid);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        finish(writableErr);
        return;
    }
    const uint32_t currentSealId = EffectiveSealId(*rule, FindRecord(*seasonComp, rule->id()));
    if (currentSealId == request.seal_id()) {
        finish(kSeasonSealAlreadyEquipped);
        return;
    }

    auto* record = SyncRecordForTerm(*seasonComp, *rule, active.resolved.seasonTerm);
    record->set_equipped_seal_id(request.seal_id());
    if (record->revision() < std::numeric_limits<uint64_t>::max()) {
        record->set_revision(record->revision() + 1);
    }

    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    LOG_INFO << "[PlayerSeason] Equipped season seal: player_id=" << (guid != nullptr ? guid->value : 0)
             << " season_id=" << rule->id() << " seal_id=" << request.seal_id();

    PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kEquipmentChanged);
    PlayerAttributeSystem::PushPanel(player);
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "season_seal_equipped");
    finish(kSuccess);
}

void PlayerSeasonSystem::Catchup(entt::entity player, uint64_t nowMs,
                                 const CatchupSeasonSealRequest& request,
                                 CatchupSeasonSealResponse& response) {
    response.Clear();
    const auto finish = [&](uint32_t err) {
        if (err != kSuccess) {
            response.mutable_error_message()->set_id(err);
        }
        if (const uint32_t stateErr = FillState(player, nowMs, *response.mutable_state()); stateErr != kSuccess) {
            response.clear_state();
            if (err == kSuccess) {
                response.mutable_error_message()->set_id(stateErr);
            }
        }
    };

    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    const uint64_t clientRequestId = request.client_request_id();
    if (clientRequestId == 0) {
        finish(kInvalidParameter);
        return;
    }

    // 1. 幂等查账排在一切状态校验之前，保证重放始终拿到首次结果（不双扣灵石）。
    std::string fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    switch (client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kCatchupSeasonSeal, fingerprint, first)) {
    case ClientRequestVerdict::kReplay:
        response.set_replayed(true);
        response.set_added_points(first.seasonCatchupPoints);
        response.set_spent_currency(first.seasonCatchupSpent);
        finish(kSuccess);
        return;
    case ClientRequestVerdict::kMismatch:
        finish(kInvalidParameter);
        return;
    case ClientRequestVerdict::kUnavailable:
        finish(kServiceUnavailable);
        return;
    default:
        break;
    }

    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        finish(kSeasonUnavailable);
        return;
    }
    const auto active = ResolveCurrentSeason(nowMs);
    const auto* rule = active.Rule();
    if (rule == nullptr) {
        finish(kSeasonNotOpen);
        return;
    }
    if (request.season_id() == 0 || request.season_id() != rule->id()) {
        finish(kSeasonMismatch);
        return;
    }
    if (rule->catchup_price_per_point() == 0) {
        finish(kSeasonCatchupDisabled);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        finish(writableErr);
        return;
    }

    const auto* existing = FindRecord(*seasonComp, rule->id());
    const uint32_t currentPoints = ReadEffectiveTermProgress(
        existing, active.resolved.seasonTerm, rule->seal_max_points()).progress;
    if (currentPoints >= rule->seal_max_points()) {
        finish(kSeasonSealAlreadyMax);
        return;
    }

    seasonrules::CatchupPlan plan;
    if (!seasonrules::ComputeCatchupPlan(currentPoints, rule->seal_max_points(),
                                         request.points_to_buy(), rule->catchup_price_per_point(), plan)) {
        // 请求补点数超出剩余上限（可能刚结算了一局导致剩余变少）：提示刷新报价。
        finish(kSeasonCatchupPriceChanged);
        return;
    }

    // 报价防呆：货币类型与总价必须和玩家界面看到的一致，防止热更或中途战斗结算导致误扣。
    if (request.expected_currency_type() != rule->catchup_currency_type() ||
        request.expected_total_price() != plan.totalPrice) {
        finish(kSeasonCatchupPriceChanged);
        return;
    }

    const auto currencyType = static_cast<CurrencyType>(rule->catchup_currency_type());
    const auto totalCost = static_cast<int64_t>(plan.totalPrice);
    if (!CurrencySystem::CanAfford(player, currencyType, totalCost)) {
        finish(kSeasonCatchupCurrencyNotEnough);
        return;
    }

    // 2. 原子扣款 -> 推进进度 -> 记幂等回执 -> 重算面板 -> 触发资产存盘（同步帧无让出；绝不溢出至往季追忆）。
    if (const uint32_t err = CurrencySystem::DeductCurrency(
            player, currencyType, totalCost, TX_SEASON_SEAL_CATCHUP, clientRequestId);
        err != kSuccess) {
        finish(err);
        return;
    }

    auto* record = SyncRecordForTerm(*seasonComp, *rule, active.resolved.seasonTerm);
    if (record->equipped_seal_id() == 0) {
        record->set_equipped_seal_id(rule->default_seal_id());
    }
    const uint32_t afterPoints = currentPoints + plan.pointsToBuy;
    record->set_progress_points(afterPoints);
    record->set_catchup_points(record->catchup_points() + plan.pointsToBuy);
    if (afterPoints >= rule->seal_max_points()) {
        seasonrules::SetHonorBitRepeated(seasonComp->mutable_unlocked_honor_words(), rule->id());
        seasonComp->mutable_retro_honor_progress()->erase(rule->id());
        if (seasonComp->retro_honor_target_season_id() == rule->id()) {
            seasonComp->set_retro_honor_target_season_id(0u);
        }
    }
    if (record->revision() < std::numeric_limits<uint64_t>::max()) {
        record->set_revision(record->revision() + 1);
    }

    ClientRequestResult applied;
    applied.seasonCatchupPoints = plan.pointsToBuy;
    applied.seasonCatchupSpent = plan.totalPrice;
    client_request_dedup::Remember(player, clientRequestId, ClientRequestKind::kCatchupSeasonSeal,
                                   std::move(fingerprint), applied);

    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    LOG_INFO << "[PlayerSeason] Catchup succeeded: player_id=" << (guid != nullptr ? guid->value : 0)
             << " season_id=" << rule->id() << " added_points=" << plan.pointsToBuy
             << " progress=" << record->progress_points() << "/" << rule->seal_max_points()
             << " spent=" << plan.totalPrice << " currency_type=" << rule->catchup_currency_type();

    PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kEquipmentChanged);
    PlayerAttributeSystem::PushPanel(player);
    (void)gSeasonPersistFn(player);

    response.set_added_points(plan.pointsToBuy);
    response.set_spent_currency(plan.totalPrice);
    finish(kSuccess);
}

void PlayerSeasonSystem::SetRetroHonorTarget(entt::entity player, uint64_t nowMs,
                                             const SetRetroHonorTargetRequest& request,
                                             SetRetroHonorTargetResponse& response) {
    response.Clear();
    const auto finish = [&](uint32_t err) {
        if (err != kSuccess) {
            response.mutable_error_message()->set_id(err);
        }
        if (const uint32_t stateErr = FillState(player, nowMs, *response.mutable_state()); stateErr != kSuccess) {
            response.clear_state();
            if (err == kSuccess) {
                response.mutable_error_message()->set_id(stateErr);
            }
        }
    };

    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        finish(kSeasonUnavailable);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        finish(writableErr);
        return;
    }

    SyncLegacyHonorsToBitmap(*seasonComp);
    const uint32_t targetSeasonId = request.season_id();
    if (targetSeasonId == 0u) {
        seasonComp->set_retro_honor_target_season_id(0u);
        PlayerLifecycleSystem::RequestKeyProgressSave(player, "season_retro_honor_target");
        finish(kSuccess);
        return;
    }

    const auto* targetRule = SeasonRuleTableManager::Instance().FindByIdSilent(targetSeasonId).first;
    const auto active = ResolveCurrentSeason(nowMs);
    if (targetRule == nullptr || targetRule->seal_max_points() == 0u ||
        (active.ruleId != 0u && active.ruleId == targetSeasonId)) {
        finish(kSeasonHonorTargetInvalid);
        return;
    }
    if (IsSeasonHonorUnlocked(*seasonComp, targetSeasonId)) {
        finish(kSeasonHonorAlreadyUnlocked);
        return;
    }

    seasonComp->set_retro_honor_target_season_id(targetSeasonId);
    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    LOG_INFO << "[PlayerSeason] Retro honor target updated: player_id=" << (guid != nullptr ? guid->value : 0)
             << " target_season_id=" << targetSeasonId;
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "season_retro_honor_target");
    finish(kSuccess);
}

void PlayerSeasonSystem::OnTowerSettlementApplied(entt::entity player,
                                                  const PlayerSeasonComp& current,
                                                  uint64_t nowMs) {
    if (!tlsEcs.actorRegistry.valid(player) || !current.has_wuxing_tower()) {
        return;
    }
    const auto& tower = current.wuxing_tower();
    const uint32_t weekKey = ComputeWuxingTowerWeekKey(nowMs);
    if (tower.week_key() != weekKey || tower.weekly_best_floor() == 0u) {
        return;
    }
    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const auto* profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(player);
    const uint64_t playerId = guid != nullptr ? guid->value : 0ULL;
    const std::string playerName = profile != nullptr ? profile->name() : "";
    UpsertTowerLeaderboard(weekKey, playerId, playerName, tower.weekly_best_floor(),
                           tower.weekly_best_rounds(), current.equipped_formation_id(),
                           tower.active_sigil_ids());
}

uint32_t PlayerSeasonSystem::PrepareWuxingTowerContext(entt::entity player,
                                                       uint64_t nowMs,
                                                       uint32_t battleConfigId,
                                                       WuxingTowerBattleContext& outContext) {
    outContext.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) {
        return kEntityIsNull;
    }
    const auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr || !seasonComp->has_wuxing_tower()) {
        return kFeatureUnavailable;
    }
    const uint32_t weekKey = ComputeWuxingTowerWeekKey(nowMs);
    const auto& tower = seasonComp->wuxing_tower();
    if (tower.week_key() != weekKey || !tower.run_active() || tower.run_id() == 0u ||
        tower.current_floor() < 1u || tower.current_floor() > turnbattle::wuxing::kTowerMaxFloors ||
        tower.pending_offer_sigil_ids_size() != 0) {
        return kFeatureUnavailable;
    }
    if (battleConfigId != turnbattle::wuxing::TowerFloorDungeonId(tower.current_floor())) {
        return kInvalidParameter;
    }
    outContext.set_week_key(weekKey);
    outContext.set_run_id(tower.run_id());
    outContext.set_floor(tower.current_floor());
    outContext.set_leyline_element(static_cast<TournamentLeylineElement>(
        turnbattle::wuxing::TowerFloorLeylineElement(weekKey, tower.current_floor())));
    for (const uint32_t sigilId : tower.active_sigil_ids()) {
        if (turnbattle::wuxing::IsValidTowerSigilId(sigilId)) {
            outContext.add_active_sigil_ids(sigilId);
        }
    }
    return kSuccess;
}

void PlayerSeasonSystem::EquipFormation(entt::entity player, uint64_t nowMs,
                                        const EquipWuxingFormationRequest& request,
                                        EquipWuxingFormationResponse& response) {
    response.Clear();
    const auto finish = [&](uint32_t err) {
        if (err != kSuccess) {
            response.mutable_error_message()->set_id(err);
        }
        response.set_equipped_formation_id(EquippedFormationId(player));
        (void)FillState(player, nowMs, *response.mutable_state());
    };
    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        finish(kSeasonUnavailable);
        return;
    }
    if (!turnbattle::wuxing::IsValidFormationId(request.formation_id())) {
        finish(kInvalidParameter);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        finish(writableErr);
        return;
    }
    seasonComp->set_equipped_formation_id(request.formation_id());
    (void)gSeasonPersistFn(player);
    finish(kSuccess);
}

uint32_t PlayerSeasonSystem::TryEquipLinkedFormation(entt::entity player, uint32_t formationId) {
    if (!tlsEcs.actorRegistry.valid(player)) {
        return kEntityIsNull;
    }
    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        return kSeasonUnavailable;
    }
    if (formationId == 0u || !turnbattle::wuxing::IsValidFormationId(formationId)) {
        return kInvalidParameter;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        return writableErr;
    }
    if (seasonComp->equipped_formation_id() == formationId) {
        return kSuccess;
    }
    seasonComp->set_equipped_formation_id(formationId);
    return kSuccess;
}

uint32_t PlayerSeasonSystem::GetWuxingTowerState(entt::entity player, uint64_t nowMs,
                                                 GetWuxingTowerStateResponse& response) {
    response.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) {
        return kEntityIsNull;
    }
    FillTowerState(player, nowMs, *response.mutable_state());
    return kSuccess;
}

void PlayerSeasonSystem::StartWuxingTowerRun(entt::entity player, uint64_t nowMs,
                                             StartWuxingTowerRunResponse& response) {
    response.Clear();
    const auto finish = [&](uint32_t err) {
        if (err != kSuccess) {
            response.mutable_error_message()->set_id(err);
        }
        FillTowerState(player, nowMs, *response.mutable_state());
    };
    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        finish(kSeasonUnavailable);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        finish(writableErr);
        return;
    }
    const uint32_t weekKey = ComputeWuxingTowerWeekKey(nowMs);
    auto* tower = seasonComp->mutable_wuxing_tower();
    if (tower->week_key() != weekKey) {
        tower->set_week_key(weekKey);
        tower->set_weekly_cleared_mask(0u);
        tower->set_weekly_best_floor(0u);
        tower->set_weekly_best_rounds(0u);
    }
    tower->set_run_active(true);
    tower->set_run_id(tower->run_id() + 1u);
    tower->set_current_floor(1u);
    tower->set_run_total_rounds(0u);
    tower->clear_active_sigil_ids();
    tower->clear_pending_offer_sigil_ids();
    if (tower->revision() < std::numeric_limits<uint64_t>::max()) {
        tower->set_revision(tower->revision() + 1u);
    }
    (void)gSeasonPersistFn(player);
    finish(kSuccess);
}

void PlayerSeasonSystem::PickWuxingTowerSigil(entt::entity player, uint64_t nowMs,
                                              const PickWuxingTowerSigilRequest& request,
                                              PickWuxingTowerSigilResponse& response) {
    response.Clear();
    const auto finish = [&](uint32_t err) {
        if (err != kSuccess) {
            response.mutable_error_message()->set_id(err);
        }
        FillTowerState(player, nowMs, *response.mutable_state());
    };
    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        finish(kSeasonUnavailable);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        finish(writableErr);
        return;
    }
    const uint32_t weekKey = ComputeWuxingTowerWeekKey(nowMs);
    auto* tower = seasonComp->mutable_wuxing_tower();
    if (tower->week_key() != weekKey || !tower->run_active() ||
        request.expected_run_id() != tower->run_id() ||
        tower->pending_offer_sigil_ids_size() == 0 ||
        !turnbattle::wuxing::IsValidTowerSigilId(request.sigil_id())) {
        finish(kInvalidParameter);
        return;
    }
    const auto& offers = tower->pending_offer_sigil_ids();
    if (std::find(offers.begin(), offers.end(), request.sigil_id()) == offers.end()) {
        finish(kInvalidParameter);
        return;
    }
    tower->add_active_sigil_ids(request.sigil_id());
    tower->clear_pending_offer_sigil_ids();
    if (tower->current_floor() < turnbattle::wuxing::kTowerMaxFloors) {
        tower->set_current_floor(tower->current_floor() + 1u);
    }
    if (tower->revision() < std::numeric_limits<uint64_t>::max()) {
        tower->set_revision(tower->revision() + 1u);
    }
    (void)gSeasonPersistFn(player);
    finish(kSuccess);
}

void PlayerSeasonSystem::AbandonWuxingTowerRun(entt::entity player, uint64_t nowMs,
                                               const AbandonWuxingTowerRunRequest& request,
                                               AbandonWuxingTowerRunResponse& response) {
    response.Clear();
    const auto finish = [&](uint32_t err) {
        if (err != kSuccess) {
            response.mutable_error_message()->set_id(err);
        }
        FillTowerState(player, nowMs, *response.mutable_state());
    };
    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        finish(kSeasonUnavailable);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        finish(writableErr);
        return;
    }
    auto* tower = seasonComp->mutable_wuxing_tower();
    if (request.expected_run_id() != 0u && request.expected_run_id() != tower->run_id()) {
        finish(kInvalidParameter);
        return;
    }
    tower->set_run_active(false);
    tower->set_current_floor(1u);
    tower->set_run_total_rounds(0u);
    tower->clear_active_sigil_ids();
    tower->clear_pending_offer_sigil_ids();
    if (tower->revision() < std::numeric_limits<uint64_t>::max()) {
        tower->set_revision(tower->revision() + 1u);
    }
    (void)gSeasonPersistFn(player);
    finish(kSuccess);
}

void PlayerSeasonSystem::SaveBattleBuildBookmark(entt::entity player, uint64_t nowMs,
                                                 const SaveBattleBuildBookmarkRequest& request,
                                                 SaveBattleBuildBookmarkResponse& response) {
    response.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        response.mutable_error_message()->set_id(kSeasonUnavailable);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        response.mutable_error_message()->set_id(writableErr);
        return;
    }
    const auto& src = request.bookmark();
    if (!turnbattle::wuxing::IsValidFormationId(src.formation_id())) {
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    SavedBattleBuildBookmark* target = nullptr;
    uint64_t maxId = 0u;
    for (auto& existing : *seasonComp->mutable_saved_builds()) {
        maxId = std::max(maxId, existing.bookmark_id());
        if (src.bookmark_id() != 0u && existing.bookmark_id() == src.bookmark_id()) {
            target = &existing;
        }
    }
    if (target == nullptr) {
        if (static_cast<uint32_t>(seasonComp->saved_builds_size()) >= turnbattle::wuxing::kMaxSavedBuildBookmarks) {
            response.mutable_error_message()->set_id(kInvalidParameter);
            for (const auto& bm : seasonComp->saved_builds()) {
                CopyBookmarkToView(bm, *response.add_all_bookmarks());
            }
            return;
        }
        target = seasonComp->add_saved_builds();
        target->set_bookmark_id(maxId + 1u);
    }
    const std::string title = src.title().empty() ? "道友配装" : ClampUtf8Prefix(src.title(), kMaxBookmarkTextBytes);
    target->set_title(title);
    target->set_source_battle_id(src.source_battle_id());
    target->set_source_player_id(src.source_player_id());
    target->set_source_player_name(ClampUtf8Prefix(src.source_player_name(), kMaxBookmarkTextBytes));
    target->set_class_id(src.class_id());
    target->set_formation_id(src.formation_id());
    target->set_season_seal_id(src.season_seal_id());
    target->set_active_pet_table_id(src.active_pet_table_id());
    target->set_active_pet_name(ClampUtf8Prefix(src.active_pet_name(), kMaxBookmarkTextBytes));
    target->clear_skill_table_ids();
    for (int i = 0; i < src.skill_table_ids_size() && i < kMaxBookmarkSkills; ++i) {
        target->add_skill_table_ids(src.skill_table_ids(i));
    }
    target->clear_wuxing_sigil_ids();
    for (int i = 0; i < src.wuxing_sigil_ids_size() && i < kMaxBookmarkSigils; ++i) {
        if (turnbattle::wuxing::IsValidTowerSigilId(src.wuxing_sigil_ids(i))) {
            target->add_wuxing_sigil_ids(src.wuxing_sigil_ids(i));
        }
    }
    target->set_saved_at_ms(nowMs);

    (void)gSeasonPersistFn(player);
    CopyBookmarkToView(*target, *response.mutable_saved());
    for (const auto& bm : seasonComp->saved_builds()) {
        CopyBookmarkToView(bm, *response.add_all_bookmarks());
    }
}

void PlayerSeasonSystem::DeleteBattleBuildBookmark(entt::entity player, uint64_t nowMs,
                                                   const DeleteBattleBuildBookmarkRequest& request,
                                                   DeleteBattleBuildBookmarkResponse& response) {
    (void)nowMs;
    response.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    auto* seasonComp = tlsEcs.actorRegistry.try_get<PlayerSeasonComp>(player);
    if (seasonComp == nullptr) {
        response.mutable_error_message()->set_id(kSeasonUnavailable);
        return;
    }
    if (const uint32_t writableErr = CheckWritable(player); writableErr != kSuccess) {
        response.mutable_error_message()->set_id(writableErr);
        return;
    }
    auto* list = seasonComp->mutable_saved_builds();
    int foundIdx = -1;
    for (int i = 0; i < list->size(); ++i) {
        if (list->Get(i).bookmark_id() == request.bookmark_id()) {
            foundIdx = i;
            break;
        }
    }
    if (foundIdx < 0) {
        response.mutable_error_message()->set_id(kInvalidParameter);
    } else {
        list->DeleteSubrange(foundIdx, 1);
        (void)gSeasonPersistFn(player);
    }
    for (const auto& bm : seasonComp->saved_builds()) {
        CopyBookmarkToView(bm, *response.add_all_bookmarks());
    }
}

void PlayerSeasonSystem::SetPersistForTest(PersistFn fn) {
    gSeasonPersistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
}

void PlayerSeasonSystem::ResetTowerLeaderboardForTest() {
    gTowerLeaderboardWeekKey = 0u;
    gTowerWeeklyLeaderboard.clear();
}

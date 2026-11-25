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
#include "player/system/player_level_rules.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/player_skill.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/equipment_effects.pb.h"
#include "proto/common/component/player_season_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/scene/player_season.pb.h"
#include "services/battle/system/season_rules.h"
#include "table/code/seasonrule_table.h"
#include "table/code/seasonseal_table.h"
#include "table/code/skill_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/season_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {

PlayerSeasonSystem::PersistFn gSeasonPersistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;

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
    return params;
}

bool IsValidSealRow(const SeasonSealTable& seal) {
    return seasonrules::ValidSealEffect(seal.season_id(), seal.id(), seal.effect_id(),
                                        seal.bonus_bps(), seal.cap_bps(), seal.per_round_limit(),
                                        seal.battle_limit(), seal.active_rounds(), seal.health_threshold_bps());
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

// 查当前时刻唯一有效的活跃赛季；若多条启用赛季窗口重叠或配置非法则 fail-closed 返回 nullptr。
const SeasonRuleTable* FindActiveSeasonRow(uint64_t nowMs) {
    const SeasonRuleTable* active = nullptr;
    for (const auto& row : SeasonRuleTableManager::Instance().FindAll().data()) {
        if (!seasonrules::IsWindowActive(row.starts_at_ms(), row.ends_at_ms(), row.enabled(), nowMs)) {
            continue;
        }
        if (seasonrules::ValidateRule(ToParams(row)) != nullptr ||
            FindValidSealForSeason(row.id(), row.default_seal_id()) == nullptr) {
            return nullptr;
        }
        if (active != nullptr) {
            return nullptr; // 多赛季重叠：fail-closed，避免不确定归属。
        }
        active = &row;
    }
    return active;
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
    const auto* rule = FindActiveSeasonRow(nowMs);
    if (rule == nullptr) {
        out.set_available(false);
        out.set_unavailable_reason("当前暂无进行中的赛季");
        return kSuccess;
    }

    const auto* record = FindRecord(*seasonComp, rule->id());
    const uint32_t equippedId = EffectiveSealId(*rule, record);
    const uint32_t rawProgress = record != nullptr ? record->progress_points() : 0u;
    const uint32_t progress = std::min(rawProgress, rule->seal_max_points());
    const uint32_t battlePts = record != nullptr ? std::min(record->battle_points(), progress) : 0u;
    const uint32_t catchupPts = record != nullptr ? std::min(record->catchup_points(), progress) : 0u;
    const uint32_t healthBps = seasonrules::ScaledBonusBps(progress, rule->seal_max_points(), rule->max_health_bps_cap());
    const uint32_t defenseBps = seasonrules::ScaledBonusBps(progress, rule->seal_max_points(), rule->defense_bps_cap());

    out.set_available(true);
    out.set_season_id(rule->id());
    out.set_season_name(rule->name());
    out.set_season_desc(rule->desc());
    out.set_starts_at_ms(rule->starts_at_ms());
    out.set_ends_at_ms(rule->ends_at_ms());

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
    }

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

    std::vector<const SeasonRuleTable*> enabledRules;
    for (const auto& rule : rules) {
        if (const char* err = seasonrules::ValidateRule(ToParams(rule)); err != nullptr) {
            LOG_ERROR << "[PlayerSeason] table validation failed: season_id=" << rule.id() << " error=" << err;
            return false;
        }
        if (FindValidSealForSeason(rule.id(), rule.default_seal_id()) == nullptr) {
            LOG_ERROR << "[PlayerSeason] table validation failed: default_seal_id=" << rule.default_seal_id()
                      << " missing or invalid for season_id=" << rule.id();
            return false;
        }
        if (rule.enabled()) {
            enabledRules.push_back(&rule);
        }
    }

    for (std::size_t i = 0; i < enabledRules.size(); ++i) {
        for (std::size_t j = i + 1; j < enabledRules.size(); ++j) {
            if (seasonrules::WindowsOverlap(enabledRules[i]->starts_at_ms(), enabledRules[i]->ends_at_ms(),
                                            enabledRules[j]->starts_at_ms(), enabledRules[j]->ends_at_ms())) {
                LOG_ERROR << "[PlayerSeason] table validation failed: overlapping enabled seasons "
                          << enabledRules[i]->id() << " and " << enabledRules[j]->id();
                return false;
            }
        }
    }

    for (const auto& seal : seals) {
        if (!SeasonRuleTableManager::Instance().Exists(seal.season_id())) {
            LOG_ERROR << "[PlayerSeason] table validation failed: seal_id=" << seal.id()
                      << " references missing season_id=" << seal.season_id();
            return false;
        }
        if (!IsValidSealRow(seal)) {
            LOG_ERROR << "[PlayerSeason] table validation failed: seal_id=" << seal.id()
                      << " has invalid effect parameters";
            return false;
        }
    }
    return true;
}

uint32_t PlayerSeasonSystem::ActiveSeasonId(uint64_t nowMs) {
    const auto* rule = FindActiveSeasonRow(nowMs);
    return rule != nullptr ? rule->id() : 0u;
}

bool PlayerSeasonSystem::IsEligibleBattle(const BattleSettlementData& settlement) {
    if (settlement.battle_id() == 0 || settlement.player_id() == 0 || settlement.fled() ||
        settlement.total_rounds() == 0 || settlement.player_team_index() > 1 ||
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
    case match::MATCH_MODE_PVP_TEAM:
        return !settlement.competitive_ranked() || settlement.participated();
    default:
        return false;
    }
}

uint32_t PlayerSeasonSystem::PrepareBattleCredit(const PlayerSeasonComp& current,
                                                 const BattleSettlementData& settlement,
                                                 uint64_t nowMs,
                                                 PlayerSeasonComp& candidate,
                                                 bool& pointsIncreased) {
    candidate = current;
    pointsIncreased = false;
    if (!IsEligibleBattle(settlement)) {
        return kSuccess;
    }
    const auto* rule = FindActiveSeasonRow(nowMs);
    if (rule == nullptr) {
        return kSuccess;
    }
    const uint32_t gain = BattlePointsFor(*rule, settlement);
    if (gain == 0) {
        return kSuccess;
    }
    auto* record = FindOrAddRecord(candidate, rule->id());
    if (record->equipped_seal_id() == 0) {
        record->set_equipped_seal_id(rule->default_seal_id());
    }
    const uint32_t before = std::min(record->progress_points(), rule->seal_max_points());
    if (before >= rule->seal_max_points()) {
        return kSuccess;
    }
    const uint32_t added = std::min(gain, rule->seal_max_points() - before);
    record->set_progress_points(before + added);
    record->set_battle_points(record->battle_points() + added);
    pointsIncreased = added > 0;
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
    const auto* rule = FindActiveSeasonRow(nowMs);
    if (rule == nullptr) {
        return;
    }
    const auto* record = FindRecord(*seasonComp, rule->id());
    const uint32_t sealId = EffectiveSealId(*rule, record);
    const auto* seal = FindValidSealForSeason(rule->id(), sealId);
    if (seal == nullptr) {
        return;
    }
    const uint32_t progress = record != nullptr ? std::min(record->progress_points(), rule->seal_max_points()) : 0u;
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
    const auto* rule = FindActiveSeasonRow(nowMs);
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

    auto* record = FindOrAddRecord(*seasonComp, rule->id());
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
    const auto* rule = FindActiveSeasonRow(nowMs);
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
    const uint32_t currentPoints = existing != nullptr ? std::min(existing->progress_points(), rule->seal_max_points()) : 0u;
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

    // 2. 原子扣款 -> 推进进度 -> 记幂等回执 -> 重算面板 -> 触发资产存盘（同步帧无让出）。
    if (const uint32_t err = CurrencySystem::DeductCurrency(
            player, currencyType, totalCost, TX_SEASON_SEAL_CATCHUP, clientRequestId);
        err != kSuccess) {
        finish(err);
        return;
    }

    auto* record = FindOrAddRecord(*seasonComp, rule->id());
    if (record->equipped_seal_id() == 0) {
        record->set_equipped_seal_id(rule->default_seal_id());
    }
    record->set_progress_points(currentPoints + plan.pointsToBuy);
    record->set_catchup_points(record->catchup_points() + plan.pointsToBuy);
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

void PlayerSeasonSystem::SetPersistForTest(PersistFn fn) {
    gSeasonPersistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
}

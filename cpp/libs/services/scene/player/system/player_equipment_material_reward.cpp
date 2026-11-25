#include "player_equipment_material_reward.h"

#include <algorithm>
#include <chrono>
#include <limits>
#include <string>
#include <vector>
#include <google/protobuf/message.h>
#include "core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "battle/system/player_battle.h"
#include "proto/match/match_service.pb.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
using System = PlayerEquipmentMaterialRewardSystem;
System::PersistFn persistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;
struct MaterialRewardPersistRequestComp { std::chrono::steady_clock::time_point requested{}; };
constexpr uint64_t kDayMs = 86400000;
constexpr uint64_t kWeekMs = 7ULL * kDayMs;
constexpr uint64_t kShiftMs = 10800000; // UTC+8 减 05:00，即 UTC 加三小时后的自然日。
// 1970-01-04 21:00:00 UTC = 1970-01-05 05:00:00 UTC+8（首个完整周一 05:00 切点）。
constexpr uint64_t kFirstMonday05Utc8Ms = 3ULL * kDayMs + 21ULL * 3600000ULL;

struct BattleEvaluation {
    bool eligible{false};
    bool isWin{false};
    bool pvpLossIneligible{false};
    uint32_t baseUnits{0};
    uint32_t bonusUnits{0};
};

BattleEvaluation EvaluateBattle(const BattleSettlementData& settlement) {
    BattleEvaluation eval;
    if (settlement.battle_id() == 0 || settlement.player_id() == 0 || settlement.fled() ||
        settlement.total_rounds() == 0 || settlement.player_team_index() > 1 ||
        (settlement.outcome() != BATTLE_OUTCOME_SIDE_A_WIN && settlement.outcome() != BATTLE_OUTCOME_SIDE_B_WIN &&
         settlement.outcome() != BATTLE_OUTCOME_DRAW)) {
        return eval;
    }
    const bool playerWon =
        (settlement.outcome() == BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 0) ||
        (settlement.outcome() == BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 1);
    switch (settlement.match_mode()) {
    case match::MATCH_MODE_PVE_SOLO:
    case match::MATCH_MODE_PVE_TEAM:
        if (!playerWon) return eval;
        eval.eligible = true;
        eval.isWin = true;
        eval.baseUnits = System::kBattleUnits;
        eval.bonusUnits = settlement.battle_difficulty() >= 2 ? System::kBonusUnits : 0u;
        return eval;
    case match::MATCH_MODE_1V1:
    case match::MATCH_MODE_5V5:
    case match::MATCH_MODE_PVP_TEAM:
    case match::MATCH_MODE_POLARITY_5V5:
    case match::MATCH_MODE_SEASON_TOURNAMENT:
        if (settlement.competitive_ranked() && !settlement.participated()) {
            eval.pvpLossIneligible = !playerWon;
            return eval;
        }
        if (playerWon) {
            eval.eligible = true;
            eval.isWin = true;
            eval.baseUnits = System::kBattleUnits;
            eval.bonusUnits = System::kBonusUnits;
            return eval;
        }
        // PVP 平局或败局：必须满足防挂机最低参与门槛（>=3 回合、>=2 手动回合、有有效攻防/治疗/控场贡献）。
        if (settlement.total_rounds() < System::kPvpMinRoundsForLoss ||
            settlement.manual_action_rounds() < System::kPvpMinManualRoundsForLoss ||
            !settlement.effective_combat_contribution()) {
            eval.pvpLossIneligible = true;
            return eval;
        }
        eval.eligible = true;
        eval.isWin = false;
        eval.baseUnits = System::kBattleUnits;
        eval.bonusUnits = 0u;
        return eval;
    default:
        return eval;
    }
}

uint32_t CheckPlayer(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player)) return kEntityIsNull;
    const auto* identity = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
    if (identity == nullptr || identity->value == 0 || ledger == nullptr ||
        tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player) == nullptr ||
        tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
        !client_request_ledger::Validate(*ledger).empty()) return kServiceUnavailable;
    return kSuccess;
}
uint32_t CheckWritable(entt::entity player) {
    if (!PlayerLifecycleSystem::IsPlayerMutable(player)) return kEquipFrozen;
    return PlayerBattleSystem::IsInBattle(player) ? kEquipInBattle : kSuccess;
}
const ItemTable* Material(uint32_t id) {
    const auto* row = ItemTableManager::Instance().FindByIdSilent(id).first;
    return row != nullptr && row->id() == id && row->max_stack_size() != 0 &&
        row->equip_kind() == 0 && row->battle_usable() == 0 ? row : nullptr;
}
uint32_t FillSnapshot(entt::entity player, uint64_t nowMs, EquipmentMaterialRewardSnapshot& out) {
    out.Clear();
    uint64_t dayStart = 0, dayNext = 0;
    uint64_t weekStart = 0, weekNext = 0;
    if (!System::ResolveDay(nowMs, dayStart, dayNext) || !System::ResolveWeek(nowMs, weekStart, weekNext)) {
        return kInvalidParameter;
    }
    const auto* material = Material(System::kCraftingMaterial);
    const auto* stone = Material(System::kEnhancementStone);
    if (material == nullptr || stone == nullptr) return kInvalidTableData;
    const auto& state = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).material_reward();
    const uint32_t lifetimeEarned = state.migrated_to_v2() ? state.lifetime_earned() : 0u;
    const uint32_t tradableEarnedTotal = state.migrated_to_v2() ? state.tradable_earned_total() : 0u;
    const uint64_t availableUnits = state.available_units();
    const uint64_t availableTradable = state.migrated_to_v2()
        ? std::min<uint64_t>(state.available_tradable_units(), availableUnits) : 0u;
    const uint32_t batchUnits = static_cast<uint32_t>(std::min<uint64_t>(System::kMaxClaimBatchUnits, availableUnits));
    const uint32_t batchTradable = static_cast<uint32_t>(std::min<uint64_t>(batchUnits, availableTradable));
    const bool weeklyPhase = lifetimeEarned >= System::kLifetimeLimit;
    const uint32_t earnedThisWeek = (weeklyPhase && state.week_starts_at_ms() == weekStart) ? state.earned_this_week() : 0u;

    out.set_available(true);
    out.set_day_starts_at_ms(dayStart);
    out.set_next_reset_at_ms(dayNext);
    out.set_earned_today(state.day_starts_at_ms() == dayStart ? state.earned_today() : 0u);
    out.set_daily_limit(System::kDailyLimit);
    out.set_available_units(availableUnits);
    out.set_batch_units(batchUnits);
    out.set_revision(state.revision());
    out.set_lifetime_earned(lifetimeEarned);
    out.set_lifetime_limit(System::kLifetimeLimit);
    out.set_tradable_earned_total(tradableEarnedTotal);
    out.set_tradable_lifetime_limit(System::kTradableLifetimeLimit);
    out.set_available_tradable_units(availableTradable);
    out.set_batch_tradable_units(batchTradable);
    out.set_weekly_phase_active(weeklyPhase);
    out.set_week_starts_at_ms(weekStart);
    out.set_next_week_reset_at_ms(weekNext);
    out.set_earned_this_week(earnedThisWeek);
    out.set_weekly_limit(System::kWeeklyLimit);

    const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    const auto display = [&](const ItemTable& row, EquipmentCraftMaterial& value) {
        value.set_config_id(row.id()); value.set_name(row.name()); value.set_icon_key(row.icon_key());
        value.set_owned(inventory.GetTotalItemCount(row.id()));
        value.set_available(BagService::RemovableCount(player, inventory, row.id()));
    };
    display(*material, *out.mutable_crafting_material()); display(*stone, *out.mutable_enhancement_stone());
    const auto writable = CheckWritable(player);
    out.set_can_claim(out.batch_units() != 0 && writable == kSuccess &&
        state.revision() != std::numeric_limits<uint64_t>::max());
    if (out.batch_units() == 0) {
        out.set_unavailable_reason(weeklyPhase
            ? "完成主要战斗可获得本周材料自选份数（每周上限100份，全绑定）。"
            : "完成主要战斗可获得首套材料自选份数（首套总池660份，无每日限制）。");
    } else if (writable == kEquipInBattle) {
        out.set_unavailable_reason("战斗中暂不能领取，请在结算完成后重试。");
    } else if (writable != kSuccess || !out.can_claim()) {
        out.set_unavailable_reason("当前状态暂不能领取，请稍后重试。");
    }
    return kSuccess;
}
bool Durable(entt::entity player, uint64_t revision) {
    const auto* persisted = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
    const auto* identity = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const auto* owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
    if (revision == 0 || identity == nullptr || !player_mutability::HasFencedOwnership(player) ||
        owner == nullptr || persisted == nullptr || !persisted->IsCommittedForOwner(owner->epoch)) return false;
    const auto& saved = persisted->snapshot->player_database_data();
    return saved.player_id() == identity->value && saved.client_request_ledger().material_reward().revision() >= revision;
}
void RequestPersist(entt::entity player, bool force) {
    if (!PlayerLifecycleSystem::IsPlayerMutable(player)) return;
    // 仅写入口延迟创建节流状态；查询和落盘不创建持久组件。
    auto& pending = tlsEcs.actorRegistry.get_or_emplace<MaterialRewardPersistRequestComp>(player);
    const auto now = std::chrono::steady_clock::now();
    if (!force && pending.requested != std::chrono::steady_clock::time_point{} &&
        now - pending.requested < std::chrono::seconds(1)) return;
    pending.requested = now;
    (void)persistFn(player); // 调度成功不等于提交成功，必须重查确认快照。
}
void FillApplied(entt::entity player, uint64_t nowMs, const ClientRequestResult& result, bool replayed,
                 ClaimEquipmentMaterialRewardResponse& response) {
    response.set_outcome(ClaimEquipmentMaterialRewardResponse::APPLIED);
    response.set_replayed(replayed); response.set_result_revision(result.materialRewardRevision);
    response.set_current_revision(tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).material_reward().revision());
    response.set_durable(Durable(player, result.materialRewardRevision));
    auto error = FillSnapshot(player, nowMs, *response.mutable_snapshot());
    if (error == kSuccess) error = PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
    if (error != kSuccess) {
        response.clear_bag(); response.clear_snapshot(); response.mutable_error_message()->set_id(error);
    }
    if (!response.durable()) RequestPersist(player, !replayed);
}
}

bool System::ResolveDay(uint64_t nowMs, uint64_t& startMs, uint64_t& nextMs) {
    const uint64_t offset = (nowMs % kDayMs + kShiftMs) % kDayMs;
    if (nowMs < offset) return false;
    const auto start = nowMs - offset;
    if (start > std::numeric_limits<uint64_t>::max() - kDayMs) return false;
    startMs = start; nextMs = start + kDayMs;
    return true;
}
bool System::ResolveWeek(uint64_t nowMs, uint64_t& startMs, uint64_t& nextMs) {
    if (nowMs < kFirstMonday05Utc8Ms) return false;
    const uint64_t offset = (nowMs - kFirstMonday05Utc8Ms) % kWeekMs;
    const uint64_t start = nowMs - offset;
    if (start > std::numeric_limits<uint64_t>::max() - kWeekMs) return false;
    startMs = start;
    nextMs = start + kWeekMs;
    return true;
}
bool System::IsEligibleBattle(const BattleSettlementData& settlement) {
    return EvaluateBattle(settlement).eligible;
}
uint32_t System::PrepareBattleCredit(const ClientRequestLedgerComp& current,
    const BattleSettlementData& settlement, uint64_t nowMs, ClientRequestLedgerComp& candidate,
    BattleCreditDelta* deltaOut) {
    if (deltaOut != nullptr) *deltaOut = BattleCreditDelta{};
    if (!client_request_ledger::Validate(current).empty()) return kServiceUnavailable;
    candidate = current;
    const BattleEvaluation eval = EvaluateBattle(settlement);
    if (deltaOut != nullptr) deltaOut->pvpLossIneligible = eval.pvpLossIneligible;
    if (!eval.eligible) return kSuccess;

    uint64_t dayStart = 0, dayNext = 0;
    uint64_t weekStart = 0, weekNext = 0;
    if (!ResolveDay(nowMs, dayStart, dayNext) || !ResolveWeek(nowMs, weekStart, weekNext)) {
        return kInvalidParameter;
    }

    const auto& previous = current.material_reward();
    const uint32_t prevLifetime = previous.migrated_to_v2() ? previous.lifetime_earned() : 0u;
    const uint32_t prevTradableTotal = previous.migrated_to_v2() ? previous.tradable_earned_total() : 0u;
    const uint64_t prevAvailableTradable = previous.migrated_to_v2() ? previous.available_tradable_units() : 0u;
    const uint32_t grossUnits = eval.baseUnits + eval.bonusUnits;

    uint32_t addTotal = 0;
    uint32_t addTradable = 0;
    auto* state = candidate.mutable_material_reward();

    if (prevLifetime < kLifetimeLimit) {
        // 第一阶段：首套永久 660 份，无每日/每周重置。
        const uint32_t stage1Remain = kLifetimeLimit - prevLifetime;
        addTotal = std::min(grossUnits, stage1Remain);
        if (eval.isWin && addTotal >= kBattleUnits && prevTradableTotal < kTradableLifetimeLimit) {
            addTradable = 1u;
        }
        if (previous.available_units() > std::numeric_limits<uint64_t>::max() - addTotal) return kFeatureUnavailable;
        state->set_migrated_to_v2(true);
        state->set_lifetime_earned(prevLifetime + addTotal);
        state->set_tradable_earned_total(prevTradableTotal + addTradable);
        state->set_available_units(previous.available_units() + addTotal);
        state->set_available_tradable_units(prevAvailableTradable + addTradable);
    } else {
        // 第二阶段：每周平滑 100 份全绑定（UTC+8 周一 05:00 重置，防时钟回拨）。
        if (weekStart < previous.week_starts_at_ms()) return kSuccess;
        const uint32_t earnedWeek = (weekStart > previous.week_starts_at_ms()) ? 0u : previous.earned_this_week();
        if (earnedWeek >= kWeeklyLimit) return kSuccess;
        addTotal = std::min(grossUnits, kWeeklyLimit - earnedWeek);
        addTradable = 0u;
        if (previous.available_units() > std::numeric_limits<uint64_t>::max() - addTotal) return kFeatureUnavailable;
        state->set_migrated_to_v2(true);
        state->set_week_starts_at_ms(weekStart);
        state->set_earned_this_week(earnedWeek + addTotal);
        state->set_available_units(previous.available_units() + addTotal);
    }

    if (deltaOut != nullptr) {
        deltaOut->totalUnitsAdded = addTotal;
        deltaOut->tradableUnitsAdded = addTradable;
    }
    // revision 只记录成功领取。新战斗不能把一个尚未发送的领取预览误判成未知结果；
    // 批次变化仍由 Claim 的实际余额和总份数复核，完整候选在发布点比较。
    return client_request_ledger::Validate(candidate).empty() ? kSuccess : kServiceUnavailable;
}
uint32_t System::Get(entt::entity player, uint64_t nowMs, GetEquipmentMaterialRewardResponse& response) {
    response.Clear();
    if (const auto error = CheckPlayer(player); error != kSuccess) return error;
    return FillSnapshot(player, nowMs, *response.mutable_snapshot());
}
void System::Claim(entt::entity player, uint64_t nowMs, const ClaimEquipmentMaterialRewardRequest& request,
                   ClaimEquipmentMaterialRewardResponse& response) {
    response.Clear(); response.set_client_request_id(request.client_request_id());
    const auto reject = [&](uint32_t error, bool known = true) {
        response.set_outcome(known ? ClaimEquipmentMaterialRewardResponse::REJECTED : ClaimEquipmentMaterialRewardResponse::UNKNOWN);
        response.mutable_error_message()->set_id(error);
    };
    if (const auto error = CheckPlayer(player); error != kSuccess) { reject(error, false); return; }
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    response.set_current_revision(ledger.material_reward().revision());
    if (request.client_request_id() == 0 || request.GetReflection()->GetUnknownFields(request).field_count() != 0) {
        reject(kInvalidParameter); return;
    }
    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
        ClientRequestKind::kEquipmentMaterialReward, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) { FillApplied(player, nowMs, first, true, response); return; }
    if (verdict == ClientRequestVerdict::kMismatch) { reject(kInvalidParameter); return; }
    if (verdict != ClientRequestVerdict::kNew) { reject(kServiceUnavailable, false); return; }
    if (request.expected_revision() != ledger.material_reward().revision()) { reject(kFeatureUnavailable, false); return; }
    if (const auto error = FillSnapshot(player, nowMs, *response.mutable_snapshot()); error != kSuccess) { reject(error); return; }
    const uint64_t total = uint64_t{request.crafting_material_count()} + request.enhancement_stone_count();
    if (total == 0 || total != response.snapshot().batch_units()) { reject(kInvalidParameter); return; }
    if (const auto error = CheckWritable(player); error != kSuccess) { reject(error); return; }
    if (ledger.material_reward().revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) { reject(kFeatureUnavailable); return; }

    // 按本批可交易份数拆分：可交易优先分配给打造材料（3001），余量给强化石（3002）；其余全绑定，分堆发放永不混堆。
    const uint64_t availTradable = ledger.material_reward().migrated_to_v2()
        ? std::min<uint64_t>(ledger.material_reward().available_tradable_units(), ledger.material_reward().available_units())
        : 0u;
    const uint32_t batchTradable = static_cast<uint32_t>(std::min<uint64_t>(total, availTradable));
    const uint32_t craftingTradable = std::min(request.crafting_material_count(), batchTradable);
    const uint32_t craftingBound = request.crafting_material_count() - craftingTradable;
    const uint32_t stoneTradable = std::min(request.enhancement_stone_count(), batchTradable - craftingTradable);
    const uint32_t stoneBound = request.enhancement_stone_count() - stoneTradable;

    const auto before = ledger.SerializeAsString();
    ClientRequestResult applied;
    applied.materialRewardRevision = ledger.material_reward().revision() + 1;
    applied.craftingMaterialCount = request.crafting_material_count();
    applied.enhancementStoneCount = request.enhancement_stone_count();
    applied.craftingTradableCount = craftingTradable;
    applied.enhancementTradableCount = stoneTradable;
    ClientRequestLedgerComp candidate = ledger;
    auto* nextState = candidate.mutable_material_reward();
    nextState->set_migrated_to_v2(true);
    nextState->set_available_units(ledger.material_reward().available_units() - total);
    nextState->set_available_tradable_units(availTradable - batchTradable);
    nextState->set_revision(applied.materialRewardRevision);
    if (!client_request_ledger::Remember(candidate, request.client_request_id(), ClientRequestKind::kEquipmentMaterialReward,
        fingerprint, applied) || !client_request_ledger::Validate(candidate).empty()) { reject(kServiceUnavailable); return; }

    std::vector<InitItemParam> gained;
    const auto add = [&](uint32_t id, uint32_t count, bool bound) {
        if (count == 0) return;
        InitItemParam param;
        param.itemPBComp.set_config_id(id);
        param.itemPBComp.set_size(count);
        param.itemPBComp.set_item_id(kInvalidGuid);
        param.itemPBComp.set_bound(bound);
        gained.push_back(std::move(param));
    };
    add(kCraftingMaterial, craftingTradable, false);
    add(kCraftingMaterial, craftingBound, true);
    add(kEnhancementStone, stoneTradable, false);
    add(kEnhancementStone, stoneBound, true);

    std::vector<GuidVector> written;
    const auto error = BagService::ExchangeInventoryExact(player, {}, gained, {}, request.client_request_id(),
        "{\"operation\":\"equipment_material_reward\"}", written, [&](const std::vector<GuidVector>&) -> uint32_t {
            auto* current = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
            if (current == nullptr || current->SerializeAsString() != before || CheckWritable(player) != kSuccess ||
                tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player)) return kFeatureUnavailable;
            current->Swap(&candidate);
            return kSuccess;
        });
    if (error != kSuccess) { reject(error); return; }
    FillApplied(player, nowMs, applied, false, response);
}
void System::SetPersistForTest(PersistFn fn) {
    persistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
}

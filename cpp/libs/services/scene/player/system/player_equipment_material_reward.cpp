#include "player_equipment_material_reward.h"

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <limits>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>
#include <google/protobuf/message.h>
#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"
#include "core/security/service_call_auth.h"
#include "core/type_define/player_guid_comp.h"
#include "grpc_client/data_service/data_service_grpc_client.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/afk_pass.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/role_admission_system.h"
#include "battle/system/player_battle.h"
#include "proto/common/base/node.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/match/match_service.pb.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"

namespace {
using System = PlayerEquipmentMaterialRewardSystem;
System::PersistFn persistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;
struct MaterialRewardPersistRequestComp { std::chrono::steady_clock::time_point requested{}; };
constexpr const char* kDirectorySecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE";
constexpr uint64_t kDayMs = 86400000;
constexpr uint64_t kWeekMs = 7ULL * kDayMs;
constexpr uint64_t kShiftMs = 10800000; // UTC+8 减 05:00，即 UTC 加三小时后的自然日。
// 1970-01-04 21:00:00 UTC = 1970-01-05 05:00:00 UTC+8（首个完整周一 05:00 切点）。
constexpr uint64_t kFirstMonday05Utc8Ms = 3ULL * kDayMs + 21ULL * 3600000ULL;

struct AccountQuotaRuntime {
    bool loaded{false};
    bool rpcInFlight{false};
    uint32_t accountUsed{0};
    uint32_t inFlightDesired{0};
    uint32_t retryAttempts{0};
    uint64_t nextRetryAtMs{0};
};

thread_local std::unordered_map<Guid, AccountQuotaRuntime> tlsAccountQuotas;
thread_local System::AccountQuotaSyncTransport tlsAccountQuotaTransport;

uint64_t QuotaRetryDelayMs(uint32_t attempts) {
    const uint32_t shift = attempts > 0 ? std::min<uint32_t>(attempts - 1, 4u) : 0u;
    return std::min<uint64_t>(30000ULL, 2000ULL << shift);
}

entt::entity PickReadyDataServiceNode() {
    auto& registry = tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService);
    for (auto entity : registry.view<NodeInfo, MuduoGrpcChannelPtr>()) {
        const auto& channel = registry.get<MuduoGrpcChannelPtr>(entity);
        if (channel && channel->GetState(/*tryToConnect=*/true) == muduo_grpc::ConnectivityState::kReady) {
            return entity;
        }
    }
    return entt::null;
}

bool SendSyncAccountStage1TradableRpc(data_service::SyncAccountStage1TradableRequest& request, uint64_t nowMs,
                                      System::AccountQuotaSyncDone done) {
    if (tlsAccountQuotaTransport) {
        return tlsAccountQuotaTransport(request, std::move(done));
    }
    const char* secret = std::getenv(kDirectorySecretEnv);
    if (secret == nullptr || !service_call_auth::Sign(
            data_service::kDataServiceSyncAccountStage1TradableGrpcPath,
            request, *request.mutable_auth(), "scene", secret, nowMs)) {
        LOG_ERROR << "[EquipmentMaterialReward] failed to sign SyncAccountStage1Tradable: missing or invalid "
                  << kDirectorySecretEnv;
        return false;
    }
    const entt::entity node = PickReadyDataServiceNode();
    if (node == entt::null) {
        LOG_WARN << "[EquipmentMaterialReward] no ready data_service node for SyncAccountStage1Tradable: player_id="
                 << request.player_id();
        return false;
    }
    data_service::SendDataServiceSyncAccountStage1Tradable(
        tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService),
        node, request,
        [done = std::move(done)](const GrpcCallOutcome& outcome,
                                 const data_service::SyncAccountStage1TradableResponse* reply) {
            done(outcome.ok(), reply);
        });
    return true;
}

void RequestPersist(entt::entity player, bool force);
void DispatchAccountQuotaSync(Guid playerId, uint32_t desiredCharTotal, uint64_t nowMs);

void HandleAccountQuotaSyncReply(Guid playerId, uint32_t desiredCharTotal, bool callOk,
                                 const data_service::SyncAccountStage1TradableResponse* reply, uint64_t nowMs) {
    auto it = tlsAccountQuotas.find(playerId);
    if (it == tlsAccountQuotas.end()) return;
    auto& rt = it->second;
    rt.rpcInFlight = false;
    rt.inFlightDesired = 0;
    if (!callOk || reply == nullptr) {
        ++rt.retryAttempts;
        rt.nextRetryAtMs = nowMs + QuotaRetryDelayMs(rt.retryAttempts);
        LOG_WARN << "[EquipmentMaterialReward] transient SyncAccountStage1Tradable failure, will retry: player_id="
                 << playerId << " desired=" << desiredCharTotal << " attempts=" << rt.retryAttempts;
        return;
    }
    rt.retryAttempts = 0;
    rt.nextRetryAtMs = 0;
    rt.loaded = true;
    if (!reply->found()) return;

    const uint32_t used = std::min<uint32_t>(System::kTradableLifetimeLimit, reply->account_used_total());
    rt.accountUsed = std::max(rt.accountUsed, used);

    const entt::entity player = tlsEcs.GetPlayer(playerId);
    if (player == entt::null || !tlsEcs.actorRegistry.valid(player) ||
        tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player)) {
        return;
    }
    auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
    if (ledger == nullptr) return;
    auto* state = ledger->mutable_material_reward();
    if (!state->migrated_to_v2()) return;

    const uint32_t confirmedByDir = std::min<uint32_t>(reply->confirmed_char_total(), System::kTradableLifetimeLimit);
    bool ledgerChanged = false;
    // 若账号级 60 上限已被同账号其他角色（或已删角色）抢先占满，将本角色超额未确认可交易额平滑降级为绑定自用（总份数不变）。
    if (state->tradable_earned_total() > confirmedByDir &&
        (confirmedByDir < desiredCharTotal || rt.accountUsed >= System::kTradableLifetimeLimit)) {
        const uint32_t revoked = state->tradable_earned_total() - confirmedByDir;
        state->set_tradable_earned_total(confirmedByDir);
        const uint64_t availTrad = state->available_tradable_units();
        state->set_available_tradable_units(availTrad > revoked ? availTrad - revoked : 0u);
        ledgerChanged = true;
    }
    const uint32_t nextConfirmed = std::min(state->tradable_earned_total(),
                                            std::max(state->stage1_tradable_confirmed(), confirmedByDir));
    if (nextConfirmed != state->stage1_tradable_confirmed()) {
        state->set_stage1_tradable_confirmed(nextConfirmed);
        ledgerChanged = true;
    }
    if (ledgerChanged) {
        RequestPersist(player, false);
    }
    if (state->tradable_earned_total() > state->stage1_tradable_confirmed() &&
        rt.accountUsed < System::kTradableLifetimeLimit) {
        DispatchAccountQuotaSync(playerId, state->tradable_earned_total(), nowMs);
    }
}

void DispatchAccountQuotaSync(Guid playerId, uint32_t desiredCharTotal, uint64_t nowMs) {
    if (playerId == 0) return;
    auto& rt = tlsAccountQuotas[playerId];
    if (rt.rpcInFlight) return;
    desiredCharTotal = std::min<uint32_t>(desiredCharTotal, System::kTradableLifetimeLimit);
    rt.rpcInFlight = true;
    rt.inFlightDesired = desiredCharTotal;
    data_service::SyncAccountStage1TradableRequest req;
    req.set_player_id(playerId);
    req.set_desired_char_total(desiredCharTotal);
    const bool sent = SendSyncAccountStage1TradableRpc(
        req, nowMs,
        [playerId, desiredCharTotal, nowMs](bool callOk, const data_service::SyncAccountStage1TradableResponse* reply) {
            HandleAccountQuotaSyncReply(playerId, desiredCharTotal, callOk, reply, nowMs);
        });
    if (!sent) {
        rt.rpcInFlight = false;
        rt.inFlightDesired = 0;
        ++rt.retryAttempts;
        rt.nextRetryAtMs = nowMs + QuotaRetryDelayMs(rt.retryAttempts);
    }
}

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
        (settlement.boss_trial_kind() >= 9u && settlement.boss_trial_kind() <= 11u) ||
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
    case match::MATCH_MODE_GUILD_WAR_PVP:
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
    case match::MATCH_MODE_GUILD_WAR_SIEGE:
        // 守旗阵踢馆（AI 托管）：胜 +10、负 0，不触发 PVP 败局防挂机门槛。
        if (!playerWon) return eval;
        eval.eligible = true;
        eval.isWin = true;
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
    const uint32_t tradableEarnedTotal = System::KnownAccountStage1TradableUsed(player);
    const uint64_t availableUnits = state.available_units();
    const uint64_t availableTradable = state.migrated_to_v2()
        ? std::min<uint64_t>(state.available_tradable_units(), availableUnits) : 0u;
    const uint32_t batchUnits = static_cast<uint32_t>(std::min<uint64_t>(System::kMaxClaimBatchUnits, availableUnits));
    const uint32_t batchTradable = static_cast<uint32_t>(std::min<uint64_t>(batchUnits, availableTradable));
    const bool weeklyPhase = lifetimeEarned >= System::kLifetimeLimit;
    const bool lightCardActive = AfkPassSystem::ExpiresAt(player) > (nowMs / 1000ULL);
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
            ? (lightCardActive
                ? "完成主要战斗可获得本周材料自选份数（每周上限150份，仙途轻享卡生效中：胜局含1份可交易）。"
                : "完成主要战斗可获得本周材料自选份数（每周上限150份，全绑定；开通仙途轻享卡可解锁胜局可交易产出）。")
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
uint32_t System::KnownAccountStage1TradableUsed(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player)) return 0u;
    uint32_t used = 0u;
    if (const auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
        ledger != nullptr && ledger->material_reward().migrated_to_v2()) {
        used = ledger->material_reward().tradable_earned_total();
    }
    if (const auto* identity = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player); identity != nullptr) {
        if (const auto it = tlsAccountQuotas.find(identity->value); it != tlsAccountQuotas.end()) {
            used = std::max(used, it->second.accountUsed);
        }
    }
    return std::min<uint32_t>(used, kTradableLifetimeLimit);
}

uint32_t System::PrepareBattleCredit(const ClientRequestLedgerComp& current,
    const BattleSettlementData& settlement, uint64_t nowMs, ClientRequestLedgerComp& candidate,
    BattleCreditDelta* deltaOut, bool lightCardActive, uint32_t accountStage1TradableUsed) {
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
        // 第一阶段：首套永久 660 份，无每日/每周重置；阶段一可交易按账号跨角色终身 60 份硬上限控制。
        const uint32_t stage1Remain = kLifetimeLimit - prevLifetime;
        addTotal = std::min(grossUnits, stage1Remain);
        const uint32_t effectiveAccountUsed =
            std::max(prevTradableTotal, std::min<uint32_t>(accountStage1TradableUsed, kTradableLifetimeLimit));
        if (eval.isWin && addTotal >= kBattleUnits && effectiveAccountUsed < kTradableLifetimeLimit) {
            addTradable = 1u;
        }
        if (previous.available_units() > std::numeric_limits<uint64_t>::max() - addTotal) return kFeatureUnavailable;
        state->set_migrated_to_v2(true);
        state->set_lifetime_earned(prevLifetime + addTotal);
        state->set_tradable_earned_total(prevTradableTotal + addTradable);
        state->set_available_units(previous.available_units() + addTotal);
        state->set_available_tradable_units(prevAvailableTradable + addTradable);
    } else {
        // 第二阶段：每周平滑 150 份池（UTC+8 周一 05:00 重置，防时钟回拨）。
        // 零氪同样 100% 拿满绑定自用推进毕业；持有「仙途轻享卡」时每场有效胜局将其中 1 份解锁为市场可交易产出（不增加总额度/战力）。
        if (weekStart < previous.week_starts_at_ms()) return kSuccess;
        const uint32_t earnedWeek = (weekStart > previous.week_starts_at_ms()) ? 0u : previous.earned_this_week();
        if (earnedWeek >= kWeeklyLimit) return kSuccess;
        addTotal = std::min(grossUnits, kWeeklyLimit - earnedWeek);
        addTradable = (lightCardActive && eval.isWin && addTotal >= kBattleUnits) ? 1u : 0u;
        if (previous.available_units() > std::numeric_limits<uint64_t>::max() - addTotal) return kFeatureUnavailable;
        state->set_migrated_to_v2(true);
        state->set_week_starts_at_ms(weekStart);
        state->set_earned_this_week(earnedWeek + addTotal);
        state->set_available_units(previous.available_units() + addTotal);
        if (addTradable > 0u) {
            state->set_available_tradable_units(prevAvailableTradable + addTradable);
        }
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
    const auto& curReward = ledger.material_reward();
    const uint64_t availTradable = curReward.migrated_to_v2()
        ? std::min<uint64_t>(curReward.available_tradable_units(), curReward.available_units())
        : 0u;
    const uint32_t batchTradable = static_cast<uint32_t>(std::min<uint64_t>(total, availTradable));
    // 账号级阶段一 60 份硬防线（fail-closed）：正式入场玩家（或测试显式挂载同步通道）在存在未由账号目录确认的
    // 阶段一可交易份数时，禁止先铸造可交易材料，先触发异步同步并回 kServiceUnavailable，杜绝删角/双角竞态超发。
    const Guid playerId = tlsEcs.GetPlayerId(player);
    const bool enforceAccountQuota = tlsAccountQuotaTransport != nullptr ||
        RoleAdmissionSystem::ConfirmedIdentity(playerId).has_value();
    if (enforceAccountQuota && batchTradable > 0 && curReward.migrated_to_v2() &&
        curReward.tradable_earned_total() > curReward.stage1_tradable_confirmed()) {
        DispatchAccountQuotaSync(playerId, curReward.tradable_earned_total(), nowMs);
        reject(kServiceUnavailable, false);
        return;
    }
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

void System::OnPlayerLoaded(entt::entity player, uint64_t nowMs) {
    if (!tlsEcs.actorRegistry.valid(player)) return;
    const Guid playerId = tlsEcs.GetPlayerId(player);
    if (playerId == 0) return;
    const auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
    const uint32_t desired = (ledger != nullptr && ledger->material_reward().migrated_to_v2())
        ? std::min<uint32_t>(ledger->material_reward().tradable_earned_total(), kTradableLifetimeLimit)
        : 0u;
    auto& rt = tlsAccountQuotas[playerId];
    rt.accountUsed = std::max(rt.accountUsed, desired);
    if (tlsAccountQuotaTransport != nullptr || RoleAdmissionSystem::ConfirmedIdentity(playerId).has_value()) {
        DispatchAccountQuotaSync(playerId, desired, nowMs);
    }
}

void System::OnStage1TradableCredited(entt::entity player, uint64_t nowMs) {
    if (!tlsEcs.actorRegistry.valid(player)) return;
    const Guid playerId = tlsEcs.GetPlayerId(player);
    const auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
    if (playerId == 0 || ledger == nullptr || !ledger->material_reward().migrated_to_v2()) return;
    const auto& state = ledger->material_reward();
    if (state.tradable_earned_total() <= state.stage1_tradable_confirmed()) return;
    auto& rt = tlsAccountQuotas[playerId];
    rt.accountUsed = std::min<uint32_t>(kTradableLifetimeLimit, std::max(rt.accountUsed + 1u, state.tradable_earned_total()));
    if (tlsAccountQuotaTransport != nullptr || RoleAdmissionSystem::ConfirmedIdentity(playerId).has_value()) {
        DispatchAccountQuotaSync(playerId, state.tradable_earned_total(), nowMs);
    }
}

void System::OnPlayerDestroyed(Guid playerId) {
    tlsAccountQuotas.erase(playerId);
}

void System::Tick(uint64_t nowMs) {
    if (tlsAccountQuotas.empty()) return;
    std::vector<std::pair<Guid, uint32_t>> due;
    for (const auto& [playerId, rt] : tlsAccountQuotas) {
        if (rt.rpcInFlight) continue;
        if (rt.nextRetryAtMs != 0 && nowMs < rt.nextRetryAtMs) continue;
        const entt::entity player = tlsEcs.GetPlayer(playerId);
        if (player == entt::null || !tlsEcs.actorRegistry.valid(player)) continue;
        const auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
        const uint32_t desired = (ledger != nullptr && ledger->material_reward().migrated_to_v2())
            ? std::min<uint32_t>(ledger->material_reward().tradable_earned_total(), kTradableLifetimeLimit)
            : 0u;
        const uint32_t confirmed = (ledger != nullptr && ledger->material_reward().migrated_to_v2())
            ? ledger->material_reward().stage1_tradable_confirmed()
            : 0u;
        if (!rt.loaded || desired > confirmed) {
            due.emplace_back(playerId, desired);
        }
    }
    for (const auto& [playerId, desired] : due) {
        DispatchAccountQuotaSync(playerId, desired, nowMs);
    }
}

void System::SetPersistForTest(PersistFn fn) {
    persistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
}

void System::SetAccountQuotaSyncTransportForTest(AccountQuotaSyncTransport transport) {
    tlsAccountQuotaTransport = std::move(transport);
    if (!tlsAccountQuotaTransport) {
        tlsAccountQuotas.clear();
    }
}

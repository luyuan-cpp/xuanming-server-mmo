#include "player_festival_beast.h"

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <vector>

#include <muduo/base/Logging.h>

#include "battle/system/player_battle.h"
#include "core/network/grpc_client_call.h"
#include "core/security/service_call_auth.h"
#include "core/type_define/player_guid_comp.h"
#include "core/utils/random/random.h"
#include "grpc_client/match/match_internal_grpc_client.h"
#include "muduo_grpc/channel.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/festival_beast_rules.h"
#include "player/system/player_equipment_material_reward.h"
#include "player/system/player_level_rules.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/player_pet.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_festival_beast_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/match/match_internal.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "table/code/dungeon_table.h"
#include "table/code/festivalbeastactivity_table.h"
#include "table/code/festivalbeasttier_table.h"
#include "table/code/pet_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/festival_beast_error_tip.pb.h"
#include "table/proto/tip/pet_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "time/system/time.h"

namespace {

constexpr char kMatchStartActivityMethod[] = "/match.MatchInternal/StartActivityBattle";
constexpr char kSceneCaller[] = "scene";
constexpr char kSceneSecretEnv[] = "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE";
constexpr uint32_t kLaunchRpcDeadlineMs = 4000;

struct FestivalBeastPersistThrottleComp {
    std::chrono::steady_clock::time_point lastRequested{};
};

PlayerFestivalBeastSystem::PersistFn gPersistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;
PlayerFestivalBeastSystem::LaunchSenderFn gLaunchSenderFn = nullptr;

std::vector<FestivalBeastTierTable> AllTiers() {
    std::vector<FestivalBeastTierTable> result;
    for (const auto& row : FestivalBeastTierTableManager::Instance().FindAll().data()) {
        result.push_back(row);
    }
    return result;
}

const PetTable* FindPetRow(uint32_t petTableId) {
    return PetTableManager::Instance().FindByIdSilent(petTableId).first;
}

const DungeonTable* FindDungeonRow(uint32_t dungeonId) {
    return DungeonTableManager::Instance().FindByIdSilent(dungeonId).first;
}

const FestivalBeastPeriodProgress* FindPeriodProgress(const PlayerFestivalBeastComp& comp, uint32_t instanceId) {
    for (const auto& p : comp.period_progress()) {
        if (p.activity_instance_id() == instanceId) return &p;
    }
    return nullptr;
}

FestivalBeastPeriodProgress* MutablePeriodProgress(PlayerFestivalBeastComp& comp, uint32_t instanceId) {
    for (auto& p : *comp.mutable_period_progress()) {
        if (p.activity_instance_id() == instanceId) return &p;
    }
    return nullptr;
}

const FestivalBeastPermanentEligibility* FindEligibility(const PlayerFestivalBeastComp& comp,
                                                         const std::string& rewardIdentity) {
    for (const auto& e : comp.eligibilities()) {
        if (e.reward_identity() == rewardIdentity) return &e;
    }
    return nullptr;
}

const FestivalBeastClaimReceipt* FindClaimByIdentity(const PlayerFestivalBeastComp& comp,
                                                     const std::string& rewardIdentity) {
    for (const auto& r : comp.claim_receipts()) {
        if (r.reward_identity() == rewardIdentity) return &r;
    }
    return nullptr;
}

const FestivalBeastClaimReceipt* FindClaimByRequest(const PlayerFestivalBeastComp& comp,
                                                    const std::string& requestId) {
    for (const auto& r : comp.claim_receipts()) {
        if (r.request_id() == requestId) return &r;
    }
    return nullptr;
}

const FestivalBeastPermanentEligibility* FindAnyUnclaimedEligibility(const PlayerFestivalBeastComp& comp) {
    for (const auto& e : comp.eligibilities()) {
        if (FindClaimByIdentity(comp, e.reward_identity()) == nullptr) {
            return &e;
        }
    }
    return nullptr;
}

struct ActiveConfigView {
    bool found{false};
    bool multipleEnabled{false};
    bool valid{false};
    FestivalBeastActivityTable row;
    std::vector<FestivalBeastTierTable> tiers;
};

ActiveConfigView ResolveConfig(uint32_t requestedInstanceId, const PlayerFestivalBeastComp* comp) {
    ActiveConfigView view;
    view.tiers = AllTiers();
    const auto& all = FestivalBeastActivityTableManager::Instance().FindAll().data();
    int enabledCount = 0;
    const FestivalBeastActivityTable* enabledRow = nullptr;
    for (const auto& item : all) {
        if (item.enabled()) {
            ++enabledCount;
            if (enabledRow == nullptr) enabledRow = &item;
        }
    }
    if (enabledCount > 1) {
        view.multipleEnabled = true;
    }

    const FestivalBeastActivityTable* chosen = nullptr;
    if (requestedInstanceId != 0) {
        chosen = FestivalBeastActivityTableManager::Instance().FindByIdSilent(requestedInstanceId).first;
    } else if (enabledRow != nullptr) {
        chosen = enabledRow;
    } else if (comp != nullptr) {
        if (const auto* unclaimed = FindAnyUnclaimedEligibility(*comp); unclaimed != nullptr) {
            chosen = FestivalBeastActivityTableManager::Instance()
                         .FindByIdSilent(unclaimed->source_activity_instance_id())
                         .first;
        }
        if (chosen == nullptr && !comp->eligibilities().empty()) {
            chosen = FestivalBeastActivityTableManager::Instance()
                         .FindByIdSilent(comp->eligibilities(comp->eligibilities_size() - 1).source_activity_instance_id())
                         .first;
        }
        if (chosen == nullptr && all.size() > 0) {
            chosen = &all.Get(all.size() - 1);
        }
    } else if (all.size() > 0) {
        chosen = &all.Get(all.size() - 1);
    }

    if (chosen != nullptr) {
        view.found = true;
        view.row = *chosen;
        if (!view.multipleEnabled) {
            view.valid = festival_beast_rules::ValidateActivityRow(
                view.row, FindDungeonRow(view.row.battle_config_id()),
                FindPetRow(view.row.reward_pet_table_id()), view.tiers);
        }
    }
    return view;
}

bool IsClaimDurable(entt::entity player, const FestivalBeastClaimReceipt& receipt) {
    const auto* persisted = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
    const auto* identity = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const auto* owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
    if (receipt.pet_id() == 0 || receipt.reward_identity().empty() || identity == nullptr ||
        !player_mutability::HasFencedOwnership(player) || owner == nullptr ||
        persisted == nullptr || !persisted->IsCommittedForOwner(owner->epoch)) {
        return false;
    }
    const auto& saved = persisted->snapshot->player_database_data();
    if (saved.player_id() != identity->value) return false;
    bool hasReceipt = false;
    for (const auto& savedReceipt : saved.festival_beast_component().claim_receipts()) {
        if (savedReceipt.reward_identity() == receipt.reward_identity() &&
            savedReceipt.request_id() == receipt.request_id() &&
            savedReceipt.pet_id() == receipt.pet_id()) {
            hasReceipt = true;
            break;
        }
    }
    if (!hasReceipt) return false;
    for (const auto& savedPet : saved.pet_component().pets()) {
        if (savedPet.pet_id() == receipt.pet_id() &&
            savedPet.pet_table_id() == receipt.reward_pet_table_id()) {
            return true;
        }
    }
    return false;
}

void RequestAssetPersist(entt::entity player, bool force) {
    if (!PlayerLifecycleSystem::IsPlayerMutable(player)) return;
    auto& throttle = tlsEcs.actorRegistry.get_or_emplace<FestivalBeastPersistThrottleComp>(player);
    const auto now = std::chrono::steady_clock::now();
    if (!force && throttle.lastRequested != std::chrono::steady_clock::time_point{} &&
        now - throttle.lastRequested < std::chrono::seconds(1)) {
        return;
    }
    throttle.lastRequested = now;
    (void)gPersistFn(player);
}

bool HasActivePendingLaunch(const PlayerFestivalBeastComp& comp) {
    if (!comp.has_pending_launch() || comp.pending_launch().request_id().empty()) return false;
    const uint32_t st = comp.pending_launch().launch_state();
    return st == static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_LAUNCHING) ||
           st == static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_ADMITTED) ||
           st == static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_UNKNOWN);
}

void FillPendingLaunchInfo(const FestivalBeastPendingLaunch& src, FestivalBeastPendingLaunchInfo& dst) {
    dst.set_client_request_id(src.request_id());
    dst.set_activity_instance_id(src.activity_instance_id());
    dst.set_config_revision(src.config_revision());
    dst.set_locked_pet_id(src.locked_pet_id());
    dst.set_matched_tier_id(src.matched_tier_id());
    dst.set_launch_state(static_cast<FestivalBeastLaunchState>(src.launch_state()));
    dst.set_battle_id(src.battle_id());
    dst.set_requested_at_ms(src.requested_at_ms());
    dst.set_admitted_at_ms(src.admitted_at_ms());
    dst.set_reject_tip_id(src.reject_tip_id());
    dst.set_reject_reason(src.reject_reason());
}

// 校验玩家是否拥有一只存活且等级合法的出战宝宝；允许低血存活，拒绝未出战或 0 血倒地。
uint32_t ValidateActivePetForBattle(entt::entity player, uint64_t requiredLockedPetId, uint64_t& petIdOut) {
    petIdOut = 0;
    const auto* petComp = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
    const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
    if (petComp == nullptr || levelComp == nullptr) {
        return kFestivalBeastServiceUnavailable;
    }
    const uint64_t activeId = petComp->active_pet_id();
    if (activeId == 0) {
        return requiredLockedPetId != 0 ? kFestivalBeastPetChanged : kFestivalBeastPetRequired;
    }
    if (requiredLockedPetId != 0 && activeId != requiredLockedPetId) {
        return kFestivalBeastPetChanged;
    }
    const PetInstance* activePet = nullptr;
    for (const auto& p : petComp->pets()) {
        if (p.pet_id() == activeId) {
            activePet = &p;
            break;
        }
    }
    if (activePet == nullptr) {
        return requiredLockedPetId != 0 ? kFestivalBeastPetChanged : kFestivalBeastPetRequired;
    }
    const auto* row = FindPetRow(activePet->pet_table_id());
    if (row == nullptr) {
        return kFestivalBeastConfigInvalid;
    }
    const uint32_t ownerLevel = playerlevel::CappedLevel(levelComp->level());
    const uint32_t petEffectiveLevel =
        row->level_cap() > 0 ? std::min(std::max(1u, activePet->level()), row->level_cap())
                             : std::max(1u, activePet->level());
    if (ownerLevel < petEffectiveLevel) {
        return requiredLockedPetId != 0 ? kFestivalBeastPetChanged : kPetOwnerLevelNotEnough;
    }
    if (activePet->health() == 0) {
        return kFestivalBeastPetDead;
    }
    petIdOut = activeId;
    return kSuccess;
}

entt::entity PickReadyMatchNode(entt::registry& registry) {
    std::vector<entt::entity> ready;
    for (const auto node : registry.view<NodeInfo, MuduoGrpcChannelPtr>()) {
        const auto& channel = registry.get<MuduoGrpcChannelPtr>(node);
        if (channel && channel->GetState(true) == muduo_grpc::ConnectivityState::kReady) {
            ready.push_back(node);
        }
    }
    if (ready.empty()) return entt::null;
    return ready[tlsRandom.Rand<std::size_t>(0, ready.size() - 1)];
}

bool SendStartActivityToMatch(const match::StartActivityBattleRequest& unsignedReq,
                              PlayerFestivalBeastSystem::LaunchReplyFn done) {
    if (gLaunchSenderFn) {
        return gLaunchSenderFn(unsignedReq, std::move(done));
    }
    const char* secretEnv = std::getenv(kSceneSecretEnv);
    if (secretEnv == nullptr || secretEnv[0] == '\0') {
        LOG_ERROR << "[FestivalBeast] StartActivityBattle not sent: " << kSceneSecretEnv << " is not set";
        return false;
    }
    auto& registry = tlsNodeContextManager.GetRegistry(common::base::MatchNodeService);
    const auto node = PickReadyMatchNode(registry);
    if (node == entt::null) {
        LOG_WARN << "[FestivalBeast] StartActivityBattle not sent: no ready match node";
        return false;
    }
    match::StartActivityBattleRequest signedReq = unsignedReq;
    if (!service_call_auth::Sign(kMatchStartActivityMethod, signedReq, *signedReq.mutable_auth(),
                                 kSceneCaller, secretEnv, TimeSystem::NowMillisecondsUTC())) {
        LOG_ERROR << "[FestivalBeast] StartActivityBattle signing failed";
        return false;
    }
    GrpcCallOptions options;
    options.deadline = std::chrono::milliseconds(kLaunchRpcDeadlineMs);
    match::SendMatchInternalStartActivityBattle(
        registry, node, signedReq,
        [done = std::move(done)](const GrpcCallOutcome& outcome,
                                 const match::StartActivityBattleResponse* reply) {
            done(outcome.ok(), reply);
        },
        options);
    return true;
}

std::pair<uint32_t, std::string> MapMatchReject(match::ActivityBattleReject reject) {
    switch (reject) {
    case match::ACTIVITY_BATTLE_REJECT_OUTSIDE_WINDOW:
        return {kFestivalBeastNotOpen, "瑞兽迎春活动当前未开放挑战"};
    case match::ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE:
        return {kFestivalBeastInBattle, "当前已在战斗中，请在结算后重试"};
    case match::ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY:
        return {kFestivalBeastLaunchInFlight, "当前存在未完成的匹配或备战请求，请稍后再试"};
    case match::ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT:
        return {kFestivalBeastConfigInvalid, "挑战参数校验未通过，请刷新活动界面后重试"};
    default:
        return {kFestivalBeastServiceUnavailable, "活动开局服务繁忙，请稍后重试"};
    }
}

void HandleLaunchReply(uint64_t playerId, const std::string& requestId,
                       bool callOk, const match::StartActivityBattleResponse* reply) {
    const auto player = tlsEcs.GetPlayer(playerId);
    if (!tlsEcs.actorRegistry.valid(player)) {
        return;
    }
    auto* comp = tlsEcs.actorRegistry.try_get<PlayerFestivalBeastComp>(player);
    if (comp == nullptr || !comp->has_pending_launch() ||
        comp->pending_launch().request_id() != requestId) {
        return;
    }
    const uint64_t nowMs = TimeSystem::NowMillisecondsUTC();
    auto* pending = comp->mutable_pending_launch();
    if (!callOk || reply == nullptr) {
        pending->set_launch_state(static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_UNKNOWN));
        comp->set_state_revision(comp->state_revision() + 1);
        PlayerLifecycleSystem::RequestKeyProgressSave(player, "festival_beast_launch_unknown");
        return;
    }
    if (reply->reject() == match::ACTIVITY_BATTLE_REJECT_NONE && reply->battle_id() != 0) {
        pending->set_launch_state(static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_ADMITTED));
        pending->set_battle_id(reply->battle_id());
        pending->set_admitted_at_ms(reply->admitted_at_ms() != 0 ? reply->admitted_at_ms() : nowMs);
        pending->clear_reject_tip_id();
        pending->clear_reject_reason();
        comp->set_state_revision(comp->state_revision() + 1);
        PlayerLifecycleSystem::RequestKeyProgressSave(player, "festival_beast_launch_admitted");
        return;
    }
    const auto [tipId, reason] = MapMatchReject(reply->reject());
    pending->set_launch_state(static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_REJECTED));
    pending->set_battle_id(0);
    pending->set_reject_tip_id(tipId);
    pending->set_reject_reason(reason);
    comp->set_state_revision(comp->state_revision() + 1);
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "festival_beast_launch_rejected");
}

}  // namespace

void PlayerFestivalBeastSystem::BuildMaterialSummary(entt::entity player, uint64_t nowMs,
                                                     FestivalBeastMaterialSummary& out) {
    out.Clear();
    GetEquipmentMaterialRewardResponse matResp;
    if (PlayerEquipmentMaterialRewardSystem::Get(player, nowMs, matResp) != kSuccess ||
        !matResp.has_snapshot() || !matResp.snapshot().available()) {
        return;
    }
    const auto& snap = matResp.snapshot();
    out.set_available_units(snap.available_units());
    out.set_available_tradable_units(snap.available_tradable_units());
    out.set_earned_today(snap.earned_today());
    out.set_daily_limit(snap.daily_limit());
    out.set_day_starts_at_ms(snap.day_starts_at_ms());
    out.set_next_day_reset_at_ms(snap.next_reset_at_ms());
    out.set_weekly_phase_active(snap.weekly_phase_active());
    out.set_lifetime_earned(snap.lifetime_earned());
    out.set_lifetime_limit(snap.lifetime_limit());
    out.set_earned_this_week(snap.earned_this_week());
    out.set_weekly_limit(snap.weekly_limit());
    out.set_week_starts_at_ms(snap.week_starts_at_ms());
    out.set_next_week_reset_at_ms(snap.next_week_reset_at_ms());

    uint32_t earnableRemaining = 0;
    if (snap.weekly_phase_active()) {
        out.set_quota_mode(FESTIVAL_MATERIAL_QUOTA_STAGE2_WEEKLY);
        earnableRemaining = snap.weekly_limit() > snap.earned_this_week()
                                ? (snap.weekly_limit() - snap.earned_this_week())
                                : 0u;
    } else {
        out.set_quota_mode(FESTIVAL_MATERIAL_QUOTA_STAGE1_LIFETIME);
        earnableRemaining = snap.lifetime_limit() > snap.lifetime_earned()
                                ? (snap.lifetime_limit() - snap.lifetime_earned())
                                : 0u;
    }
    out.set_earnable_remaining_units(earnableRemaining);
    out.set_expected_win_units(std::min<uint32_t>(PlayerEquipmentMaterialRewardSystem::kBattleUnits, earnableRemaining));
}

void PlayerFestivalBeastSystem::AppendActivityCatalogEntry(entt::entity player, uint64_t nowMs,
                                                           GetActivityListResponse& out) {
    if (!tlsEcs.actorRegistry.valid(player)) return;
    const auto* comp = tlsEcs.actorRegistry.try_get<PlayerFestivalBeastComp>(player);
    if (comp == nullptr) return;

    GetFestivalBeastDetailRequest req;
    GetFestivalBeastDetailResponse detail;
    if (GetDetail(player, nowMs, req, detail) != kSuccess) return;
    // 未排期且玩家既没有未领永久资格也没有历史领奖记录时不出现在大厅；
    // 只要玩家有未领永久资格，即使活动结束或已进历史态也始终保留入口与红点。
    const bool hasUnclaimed = detail.has_permanent_eligibility() && !detail.reward_claimed();
    if (detail.phase() == FESTIVAL_BEAST_PHASE_UNSCHEDULED && !hasUnclaimed && !detail.reward_claimed()) {
        return;
    }

    auto* entry = out.add_activities();
    entry->set_activity_id(detail.activity_instance_id());
    entry->set_name(detail.activity_name());
    entry->set_description("单人携出战宝宝挑战" + detail.boss_name() + "，累计获胜" +
                           std::to_string(detail.required_wins()) + "次免费领取专属神兽");
    entry->set_icon_key(detail.icon_key());
    entry->set_starts_at_ms(detail.effective_start_at_ms());
    entry->set_ends_at_ms(detail.effective_end_at_ms());
    entry->set_can_participate(detail.can_start_challenge() || detail.can_claim_reward());
    entry->set_unavailable_reason(detail.can_claim_reward() ? "" : detail.start_block_reason());
    entry->set_definition_key(detail.reward_identity());
    entry->set_configuration_revision(detail.config_revision());
    entry->set_category(PLAYER_ACTIVITY_CATEGORY_LIMITED);
    entry->add_reward_types(PLAYER_ACTIVITY_REWARD_PET);
    entry->add_reward_types(PLAYER_ACTIVITY_REWARD_ITEM);
    entry->set_entry_kind(PLAYER_ACTIVITY_ENTRY_FESTIVAL_BEAST);
    entry->set_completion_count_known(true);
    entry->set_completed_count(detail.current_wins());
    entry->set_completion_limit(detail.required_wins());
    entry->set_completion_period("本期累计");
    entry->set_has_claimable_reward(detail.can_claim_reward());
    switch (detail.phase()) {
    case FESTIVAL_BEAST_PHASE_UPCOMING:
        entry->set_status(PLAYER_ACTIVITY_UPCOMING);
        break;
    case FESTIVAL_BEAST_PHASE_OPEN:
        entry->set_status(PLAYER_ACTIVITY_OPEN);
        break;
    case FESTIVAL_BEAST_PHASE_CLAIM_SHOWCASE:
    case FESTIVAL_BEAST_PHASE_HISTORY:
        entry->set_status(PLAYER_ACTIVITY_ENDED);
        break;
    default:
        entry->set_status(PLAYER_ACTIVITY_UNSCHEDULED);
        break;
    }
}

uint32_t PlayerFestivalBeastSystem::GetDetail(entt::entity player, uint64_t nowMs,
                                              const GetFestivalBeastDetailRequest& request,
                                              GetFestivalBeastDetailResponse& response) {
    response.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return kEntityIsNull;
    const auto* comp = tlsEcs.actorRegistry.try_get<PlayerFestivalBeastComp>(player);
    const auto* petComp = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
    const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
    if (comp == nullptr || petComp == nullptr || levelComp == nullptr) {
        return kFestivalBeastServiceUnavailable;
    }

    response.set_server_now_ms(nowMs);
    response.set_state_revision(comp->state_revision());
    BuildMaterialSummary(player, nowMs, *response.mutable_material_summary());

    const auto cfg = ResolveConfig(request.activity_instance_id(), comp);
    const auto* unclaimedAny = FindAnyUnclaimedEligibility(*comp);
    if (!cfg.found) {
        response.set_phase(unclaimedAny != nullptr ? FESTIVAL_BEAST_PHASE_HISTORY : FESTIVAL_BEAST_PHASE_UNSCHEDULED);
        response.set_can_start_challenge(false);
        response.set_start_block_code(FESTIVAL_BEAST_BLOCK_NOT_OPEN);
        response.set_start_block_reason("瑞兽迎春活动当前未排期");
        if (unclaimedAny != nullptr) {
            response.set_reward_identity(unclaimedAny->reward_identity());
            response.set_reward_definition_version(unclaimedAny->reward_definition_version());
            response.set_reward_pet_table_id(unclaimedAny->reward_pet_table_id());
            response.set_has_permanent_eligibility(true);
            response.set_eligibility_source_instance_id(unclaimedAny->source_activity_instance_id());
            response.set_eligibility_earned_at_ms(unclaimedAny->earned_at_ms());
            if (const auto* petRow = FindPetRow(unclaimedAny->reward_pet_table_id());
                festival_beast_rules::IsValidRewardPetRow(petRow)) {
                response.set_reward_pet_model_id(petRow->model_id());
                response.set_reward_pet_name(petRow->name());
                response.set_boss_name(petRow->name());
                response.set_activity_name("瑞兽迎春");
                const bool writable = PlayerLifecycleSystem::IsPlayerMutable(player);
                const bool inBattle = PlayerBattleSystem::IsInBattle(player);
                const bool hasSlot = PetSystem::HasExclusiveGiftSlot(*petComp, petRow->id());
                response.set_can_claim_reward(writable && !inBattle && hasSlot);
                if (!writable) {
                    response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_FROZEN);
                    response.set_claim_block_reason("当前角色状态暂时不能领取，请稍后重试");
                } else if (inBattle) {
                    response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_IN_BATTLE);
                    response.set_claim_block_reason("战斗中暂不能领取神兽，请在结算后重试");
                } else if (!hasSlot) {
                    response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_EXCLUSIVE_SLOT_OCCUPIED);
                    response.set_claim_block_reason("已拥有该专属神兽，无需重复领取");
                }
            } else {
                response.set_can_claim_reward(false);
                response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_CONFIG_INVALID);
                response.set_claim_block_reason("神兽配置或资源引用不完整，暂不可发放");
            }
        } else {
            response.set_can_claim_reward(false);
            response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_NOT_ELIGIBLE);
            response.set_claim_block_reason("尚未满足领取条件");
        }
        return kSuccess;
    }

    const auto& row = cfg.row;
    const auto phase = festival_beast_rules::ResolvePhase(
        row.enabled(), row.effective_start_at_ms(), row.effective_end_at_ms(),
        row.claim_showcase_days(), nowMs);
    response.set_phase(phase);
    response.set_activity_instance_id(row.id());
    response.set_config_revision(row.config_revision());
    response.set_activity_name(row.activity_name());
    response.set_boss_name(row.boss_name());
    response.set_banner_asset_key(row.banner_asset_key());
    response.set_icon_key(row.icon_key());
    response.set_effective_start_at_ms(row.effective_start_at_ms());
    response.set_effective_end_at_ms(row.effective_end_at_ms());
    response.set_claim_showcase_end_at_ms(
        festival_beast_rules::ClaimShowcaseEndAtMs(row.effective_end_at_ms(), row.claim_showcase_days()));
    response.set_required_wins(row.required_wins());
    response.set_round_limit(row.round_limit());
    response.set_heavy_damage_percent(row.heavy_damage_percent());

    const uint32_t ownerLevel = playerlevel::CappedLevel(levelComp->level());
    if (const auto* tier = festival_beast_rules::FindTierForLevel(cfg.tiers, ownerLevel); tier != nullptr) {
        response.set_matched_tier_id(tier->id());
    }

    if (const auto* period = FindPeriodProgress(*comp, row.id()); period != nullptr) {
        response.set_current_wins(std::min(period->wins(), row.required_wins()));
    }

    const auto* elig = FindEligibility(*comp, row.reward_identity());
    const auto* receipt = FindClaimByIdentity(*comp, row.reward_identity());
    const uint32_t effectiveRewardVersion =
        receipt != nullptr ? receipt->reward_definition_version()
                           : (elig != nullptr ? elig->reward_definition_version() : row.reward_definition_version());
    const uint32_t effectiveRewardPetId =
        receipt != nullptr ? receipt->reward_pet_table_id()
                           : (elig != nullptr ? elig->reward_pet_table_id() : row.reward_pet_table_id());
    response.set_reward_identity(row.reward_identity());
    response.set_reward_definition_version(effectiveRewardVersion);
    response.set_reward_pet_table_id(effectiveRewardPetId);
    if (const auto* petRow = FindPetRow(effectiveRewardPetId); petRow != nullptr) {
        response.set_reward_pet_model_id(petRow->model_id());
        response.set_reward_pet_name(petRow->name());
    }

    if (elig != nullptr) {
        response.set_has_permanent_eligibility(true);
        response.set_eligibility_source_instance_id(elig->source_activity_instance_id());
        response.set_eligibility_earned_at_ms(elig->earned_at_ms());
    }
    if (receipt != nullptr) {
        response.set_has_permanent_eligibility(true);
        response.set_reward_claimed(true);
        response.set_reward_claim_durable(IsClaimDurable(player, *receipt));
        response.set_claimed_pet_id(receipt->pet_id());
        response.set_claimed_at_ms(receipt->claimed_at_ms());
        response.set_claim_request_id(receipt->request_id());
    }

    if (comp->has_pending_launch() && !comp->pending_launch().request_id().empty()) {
        FillPendingLaunchInfo(comp->pending_launch(), *response.mutable_pending_launch());
    }

    // 1. 权威裁定是否可发起挑战
    uint64_t checkedPetId = 0;
    const bool writable = PlayerLifecycleSystem::IsPlayerMutable(player);
    const bool inBattle = PlayerBattleSystem::IsInBattle(player);
    const bool sceneBusy = PlayerLifecycleSystem::IsSceneChangeBusy(player);
    if (!cfg.valid) {
        response.set_can_start_challenge(false);
        response.set_start_block_code(FESTIVAL_BEAST_BLOCK_CONFIG_INVALID);
        response.set_start_block_reason("瑞兽迎春活动配置不完整，暂未开放");
    } else if (phase != FESTIVAL_BEAST_PHASE_OPEN) {
        response.set_can_start_challenge(false);
        response.set_start_block_code(FESTIVAL_BEAST_BLOCK_NOT_OPEN);
        response.set_start_block_reason(phase == FESTIVAL_BEAST_PHASE_UPCOMING
                                            ? "活动尚未开始，请等待开放时间"
                                            : "本期活动挑战已结束");
    } else if (!writable || sceneBusy) {
        response.set_can_start_challenge(false);
        response.set_start_block_code(FESTIVAL_BEAST_BLOCK_FROZEN);
        response.set_start_block_reason("当前角色状态暂时不能发起挑战，请稍后重试");
    } else if (inBattle) {
        response.set_can_start_challenge(false);
        response.set_start_block_code(FESTIVAL_BEAST_BLOCK_IN_BATTLE);
        response.set_start_block_reason("当前正在战斗中，请在结算完成后再试");
    } else if (HasActivePendingLaunch(*comp)) {
        response.set_can_start_challenge(false);
        response.set_start_block_code(FESTIVAL_BEAST_BLOCK_LAUNCH_IN_FLIGHT);
        response.set_start_block_reason("正在进入瑞兽迎春战斗或结果确认中，请稍候");
    } else if (const uint32_t petErr = ValidateActivePetForBattle(player, 0, checkedPetId); petErr != kSuccess) {
        response.set_can_start_challenge(false);
        if (petErr == kFestivalBeastPetDead) {
            response.set_start_block_code(FESTIVAL_BEAST_BLOCK_PET_DEAD);
            response.set_start_block_reason("当前出战宝宝已倒地，请先恢复气血或更换出战宝宝");
        } else {
            response.set_start_block_code(FESTIVAL_BEAST_BLOCK_PET_REQUIRED);
            response.set_start_block_reason("请先出战一只合法存活宝宝后再挑战");
        }
    } else {
        response.set_can_start_challenge(true);
        response.set_start_block_code(FESTIVAL_BEAST_BLOCK_NONE);
    }

    // 2. 权威裁定是否可领取神兽（永久资格优先，不受活动结束或历史期限制）
    if (receipt != nullptr) {
        response.set_can_claim_reward(false);
        if (response.reward_claim_durable()) {
            response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_ALREADY_CLAIMED);
            response.set_claim_block_reason("已领取该专属神兽，同一角色终身限领一只");
        } else {
            response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_CLAIM_CONFIRMING);
            response.set_claim_block_reason("正在确认神兽发放结果，请稍候或重试原请求");
        }
    } else if (elig == nullptr) {
        response.set_can_claim_reward(false);
        response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_NOT_ELIGIBLE);
        response.set_claim_block_reason("本期累计获胜 " + std::to_string(row.required_wins()) +
                                        " 次后可免费领取" + row.boss_name() + "（当前 " +
                                        std::to_string(response.current_wins()) + "/" +
                                        std::to_string(row.required_wins()) + "）");
    } else if (!festival_beast_rules::IsValidRewardPetRow(FindPetRow(effectiveRewardPetId))) {
        response.set_can_claim_reward(false);
        response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_CONFIG_INVALID);
        response.set_claim_block_reason("正式神兽配置或资源引用不完整，暂不可发放");
    } else if (!writable) {
        response.set_can_claim_reward(false);
        response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_FROZEN);
        response.set_claim_block_reason("当前角色状态暂时不能领取，请稍后重试");
    } else if (inBattle) {
        response.set_can_claim_reward(false);
        response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_IN_BATTLE);
        response.set_claim_block_reason("战斗中暂不能领取神兽，请在结算完成后重试");
    } else if (!PetSystem::HasExclusiveGiftSlot(*petComp, effectiveRewardPetId)) {
        response.set_can_claim_reward(false);
        response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_EXCLUSIVE_SLOT_OCCUPIED);
        response.set_claim_block_reason("已拥有该专属神兽，无需重复领取");
    } else {
        response.set_can_claim_reward(true);
        response.set_claim_block_code(FESTIVAL_BEAST_BLOCK_NONE);
    }
    return kSuccess;
}

uint32_t PlayerFestivalBeastSystem::StartChallenge(entt::entity player, uint64_t nowMs,
                                                   const StartFestivalBeastChallengeRequest& request,
                                                   StartFestivalBeastChallengeResponse& response) {
    response.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return kEntityIsNull;
    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    auto* comp = tlsEcs.actorRegistry.try_get<PlayerFestivalBeastComp>(player);
    const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
    if (guid == nullptr || guid->value == 0 || comp == nullptr || levelComp == nullptr) {
        return kFestivalBeastServiceUnavailable;
    }

    uint64_t reqHigh = 0;
    uint64_t reqLow = 0;
    if (!festival_beast_rules::ParseUuid128(request.client_request_id(), reqHigh, reqLow) ||
        request.activity_instance_id() == 0 || request.config_revision() == 0) {
        return kInvalidParameter;
    }
    response.set_client_request_id(request.client_request_id());

    // 1. 同请求重放：结果未知/在途时复用原请求重试，绝不另起新局。
    if (comp->has_pending_launch() && comp->pending_launch().request_id() == request.client_request_id()) {
        const auto& pending = comp->pending_launch();
        if (pending.activity_instance_id() != request.activity_instance_id() ||
            pending.config_revision() != request.config_revision()) {
            return kFestivalBeastRequestConflict;
        }
        response.set_replayed(true);
        response.set_locked_pet_id(pending.locked_pet_id());
        response.set_battle_id(pending.battle_id());
        response.set_matched_tier_id(pending.matched_tier_id());
        response.set_admitted_at_ms(pending.admitted_at_ms());
        response.set_reject_reason(pending.reject_reason());
        response.set_launch_state(static_cast<FestivalBeastLaunchState>(pending.launch_state()));

        const bool shouldResend =
            pending.launch_state() == static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_UNKNOWN) ||
            (pending.launch_state() == static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_LAUNCHING) &&
             nowMs > pending.requested_at_ms() + kLaunchRpcDeadlineMs);
        if (!shouldResend) {
            return pending.launch_state() == static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_REJECTED) &&
                           pending.reject_tip_id() != 0
                       ? pending.reject_tip_id()
                       : kSuccess;
        }
        // 结果未知重试：使用原锁定参数重投同一 (Kind=5, owner, high, low) 给 Match 幂等收敛。
        const auto cfg = ResolveConfig(pending.activity_instance_id(), comp);
        const auto* tier = festival_beast_rules::FindTierForLevel(cfg.tiers, pending.locked_owner_level());
        if (!cfg.found || !cfg.valid || tier == nullptr) {
            return kFestivalBeastServiceUnavailable;
        }
        match::StartActivityBattleRequest matchReq;
        matchReq.set_battle_config_id(pending.battle_config_id());
        matchReq.add_member_player_ids(guid->value);
        matchReq.set_request_high(reqHigh);
        matchReq.set_request_low(reqLow);
        auto* actx = matchReq.mutable_activity_context();
        actx->set_kind(BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST);
        actx->set_initiator_player_id(guid->value);
        auto* fb = actx->mutable_festival_beast();
        fb->set_activity_instance_id(cfg.row.id());
        fb->set_config_revision(cfg.row.config_revision());
        fb->set_reward_identity(cfg.row.reward_identity());
        fb->set_reward_definition_version(cfg.row.reward_definition_version());
        fb->set_reward_pet_table_id(cfg.row.reward_pet_table_id());
        fb->set_required_wins(cfg.row.required_wins());
        fb->set_locked_pet_id(pending.locked_pet_id());
        fb->set_locked_owner_level(pending.locked_owner_level());
        fb->set_tier_id(tier->id());
        fb->set_round_limit(cfg.row.round_limit());
        fb->set_cycle_rounds(cfg.row.cycle_rounds());
        fb->set_first_heavy_round(cfg.row.first_heavy_round());
        fb->set_telegraph_lead_rounds(cfg.row.telegraph_lead_rounds());
        fb->set_heavy_damage_percent(cfg.row.heavy_damage_percent());
        fb->set_boss_name(cfg.row.boss_name());
        fb->set_boss_health(tier->boss_health());
        fb->set_boss_physical_attack(tier->boss_physical_attack());
        fb->set_boss_defense(tier->boss_defense());
        fb->set_boss_resistance(tier->boss_resistance());
        fb->set_boss_speed(tier->boss_speed());
        fb->set_effective_start_at_ms(cfg.row.effective_start_at_ms());
        fb->set_effective_end_at_ms(cfg.row.effective_end_at_ms());
        fb->set_client_request_id(pending.request_id());

        const uint64_t playerId = guid->value;
        const std::string capturedReqId = pending.request_id();
        if (SendStartActivityToMatch(matchReq, [playerId, capturedReqId](bool ok, const match::StartActivityBattleResponse* rep) {
                HandleLaunchReply(playerId, capturedReqId, ok, rep);
            })) {
            comp->mutable_pending_launch()->set_launch_state(static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_LAUNCHING));
            comp->mutable_pending_launch()->set_requested_at_ms(nowMs);
            response.set_launch_state(FESTIVAL_BEAST_LAUNCH_LAUNCHING);
        }
        return kSuccess;
    }

    // 2. 已有不同请求的在途/未知开局：结果未知时禁止重复开局。
    if (HasActivePendingLaunch(*comp)) {
        return kFestivalBeastLaunchInFlight;
    }

    // 3. 校验活动配置、时间窗与玩家可写/非战斗状态。
    const auto cfg = ResolveConfig(request.activity_instance_id(), comp);
    if (!cfg.found || !cfg.valid) {
        return kFestivalBeastConfigInvalid;
    }
    if (cfg.row.config_revision() != request.config_revision()) {
        return kFestivalBeastConfigRevisionMismatch;
    }
    const auto phase = festival_beast_rules::ResolvePhase(
        cfg.row.enabled(), cfg.row.effective_start_at_ms(), cfg.row.effective_end_at_ms(),
        cfg.row.claim_showcase_days(), nowMs);
    if (phase != FESTIVAL_BEAST_PHASE_OPEN) {
        return kFestivalBeastNotOpen;
    }
    if (!PlayerLifecycleSystem::IsPlayerMutable(player) || PlayerLifecycleSystem::IsSceneChangeBusy(player)) {
        return kFestivalBeastFrozen;
    }
    if (PlayerBattleSystem::IsInBattle(player)) {
        return kFestivalBeastInBattle;
    }

    uint64_t lockedPetId = 0;
    if (const uint32_t petErr = ValidateActivePetForBattle(player, 0, lockedPetId); petErr != kSuccess) {
        return petErr;
    }

    const uint32_t ownerLevel = playerlevel::CappedLevel(levelComp->level());
    const auto* tier = festival_beast_rules::FindTierForLevel(cfg.tiers, ownerLevel);
    if (tier == nullptr) {
        return kFestivalBeastConfigInvalid;
    }

    // 4. 先持久化在途开局意图，再发 Match 异步准入（新设备无本地 UUID 也可恢复）。
    auto* pending = comp->mutable_pending_launch();
    pending->Clear();
    pending->set_request_id(request.client_request_id());
    pending->set_activity_instance_id(cfg.row.id());
    pending->set_config_revision(cfg.row.config_revision());
    pending->set_reward_identity(cfg.row.reward_identity());
    pending->set_locked_pet_id(lockedPetId);
    pending->set_locked_owner_level(ownerLevel);
    pending->set_matched_tier_id(tier->id());
    pending->set_battle_config_id(cfg.row.battle_config_id());
    pending->set_launch_state(static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_LAUNCHING));
    pending->set_requested_at_ms(nowMs);
    comp->set_state_revision(comp->state_revision() + 1);
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "festival_beast_launch_started");

    match::StartActivityBattleRequest matchReq;
    matchReq.set_battle_config_id(cfg.row.battle_config_id());
    matchReq.add_member_player_ids(guid->value);
    matchReq.set_request_high(reqHigh);
    matchReq.set_request_low(reqLow);
    auto* actx = matchReq.mutable_activity_context();
    actx->set_kind(BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST);
    actx->set_initiator_player_id(guid->value);
    auto* fb = actx->mutable_festival_beast();
    fb->set_activity_instance_id(cfg.row.id());
    fb->set_config_revision(cfg.row.config_revision());
    fb->set_reward_identity(cfg.row.reward_identity());
    fb->set_reward_definition_version(cfg.row.reward_definition_version());
    fb->set_reward_pet_table_id(cfg.row.reward_pet_table_id());
    fb->set_required_wins(cfg.row.required_wins());
    fb->set_locked_pet_id(lockedPetId);
    fb->set_locked_owner_level(ownerLevel);
    fb->set_tier_id(tier->id());
    fb->set_round_limit(cfg.row.round_limit());
    fb->set_cycle_rounds(cfg.row.cycle_rounds());
    fb->set_first_heavy_round(cfg.row.first_heavy_round());
    fb->set_telegraph_lead_rounds(cfg.row.telegraph_lead_rounds());
    fb->set_heavy_damage_percent(cfg.row.heavy_damage_percent());
    fb->set_boss_name(cfg.row.boss_name());
    fb->set_boss_health(tier->boss_health());
    fb->set_boss_physical_attack(tier->boss_physical_attack());
    fb->set_boss_defense(tier->boss_defense());
    fb->set_boss_resistance(tier->boss_resistance());
    fb->set_boss_speed(tier->boss_speed());
    fb->set_effective_start_at_ms(cfg.row.effective_start_at_ms());
    fb->set_effective_end_at_ms(cfg.row.effective_end_at_ms());
    fb->set_client_request_id(request.client_request_id());

    const uint64_t playerId = guid->value;
    const std::string capturedReqId = request.client_request_id();
    const bool sent = SendStartActivityToMatch(
        matchReq, [playerId, capturedReqId](bool ok, const match::StartActivityBattleResponse* rep) {
            HandleLaunchReply(playerId, capturedReqId, ok, rep);
        });
    if (!sent) {
        // 本地根本未发出 RPC，确定无准入副作用，直接清理 pending_launch 返回服务暂不可用。
        comp->clear_pending_launch();
        comp->set_state_revision(comp->state_revision() + 1);
        response.set_launch_state(FESTIVAL_BEAST_LAUNCH_REJECTED);
        response.set_reject_reason("活动开局服务暂不可用，请稍后重试");
        return kFestivalBeastServiceUnavailable;
    }

    // 同步注入（单测）可能已在上面回调中更新了 pending_launch，读取最新状态回包。
    if (comp->has_pending_launch() && comp->pending_launch().request_id() == capturedReqId) {
        const auto& latest = comp->pending_launch();
        response.set_launch_state(static_cast<FestivalBeastLaunchState>(latest.launch_state()));
        response.set_locked_pet_id(latest.locked_pet_id());
        response.set_battle_id(latest.battle_id());
        response.set_matched_tier_id(latest.matched_tier_id());
        response.set_admitted_at_ms(latest.admitted_at_ms());
        response.set_reject_reason(latest.reject_reason());
        if (latest.launch_state() == static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_REJECTED) &&
            latest.reject_tip_id() != 0) {
            return latest.reject_tip_id();
        }
    } else {
        response.set_launch_state(FESTIVAL_BEAST_LAUNCH_LAUNCHING);
        response.set_locked_pet_id(lockedPetId);
        response.set_matched_tier_id(tier->id());
    }
    return kSuccess;
}

uint32_t PlayerFestivalBeastSystem::ClaimReward(entt::entity player, uint64_t nowMs,
                                                const ClaimFestivalBeastRewardRequest& request,
                                                ClaimFestivalBeastRewardResponse& response) {
    response.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return kEntityIsNull;
    auto* comp = tlsEcs.actorRegistry.try_get<PlayerFestivalBeastComp>(player);
    const auto* petComp = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
    if (comp == nullptr || petComp == nullptr) {
        response.set_outcome(FESTIVAL_BEAST_CLAIM_UNKNOWN);
        return kFestivalBeastServiceUnavailable;
    }

    uint64_t reqHigh = 0;
    uint64_t reqLow = 0;
    if (!festival_beast_rules::ParseUuid128(request.client_request_id(), reqHigh, reqLow) ||
        request.reward_identity().empty() || request.reward_identity().size() > 64) {
        response.set_outcome(FESTIVAL_BEAST_CLAIM_REJECTED);
        return kInvalidParameter;
    }
    response.set_client_request_id(request.client_request_id());
    response.set_reward_identity(request.reward_identity());

    // 1. 优先按原请求 UUID 重放（历史期或配置更新后仍可确认原结果）。
    if (const auto* sameReq = FindClaimByRequest(*comp, request.client_request_id()); sameReq != nullptr) {
        if (sameReq->reward_identity() != request.reward_identity()) {
            response.set_outcome(FESTIVAL_BEAST_CLAIM_REJECTED);
            return kFestivalBeastRequestConflict;
        }
        const bool durable = IsClaimDurable(player, *sameReq);
        response.set_outcome(FESTIVAL_BEAST_CLAIM_APPLIED);
        response.set_replayed(true);
        response.set_durable(durable);
        response.set_reward_definition_version(sameReq->reward_definition_version());
        response.set_reward_pet_table_id(sameReq->reward_pet_table_id());
        response.set_pet_id(sameReq->pet_id());
        response.set_claimed_at_ms(sameReq->claimed_at_ms());
        if (!durable) RequestAssetPersist(player, false);
        return kSuccess;
    }

    // 2. 不同请求并发领取同一种 reward_identity：收敛返回已领取的同一只 pet_id，绝不铸第二只。
    if (const auto* existing = FindClaimByIdentity(*comp, request.reward_identity()); existing != nullptr) {
        const bool durable = IsClaimDurable(player, *existing);
        response.set_outcome(FESTIVAL_BEAST_CLAIM_ALREADY_CLAIMED);
        response.set_replayed(false);
        response.set_durable(durable);
        response.set_client_request_id(existing->request_id());
        response.set_reward_definition_version(existing->reward_definition_version());
        response.set_reward_pet_table_id(existing->reward_pet_table_id());
        response.set_pet_id(existing->pet_id());
        response.set_claimed_at_ms(existing->claimed_at_ms());
        if (!durable) RequestAssetPersist(player, false);
        return kSuccess;
    }

    // 3. 校验永久资格（读取达成时冻结的奖励定义与版本，换表不影响已赚取权益）。
    const auto* elig = FindEligibility(*comp, request.reward_identity());
    if (elig == nullptr) {
        response.set_outcome(FESTIVAL_BEAST_CLAIM_REJECTED);
        return kFestivalBeastNotEligible;
    }
    if (!PlayerLifecycleSystem::IsPlayerMutable(player)) {
        response.set_outcome(FESTIVAL_BEAST_CLAIM_REJECTED);
        return kFestivalBeastFrozen;
    }
    if (PlayerBattleSystem::IsInBattle(player)) {
        response.set_outcome(FESTIVAL_BEAST_CLAIM_REJECTED);
        return kFestivalBeastInBattle;
    }
    const auto* petRow = FindPetRow(elig->reward_pet_table_id());
    if (!festival_beast_rules::IsValidRewardPetRow(petRow)) {
        response.set_outcome(FESTIVAL_BEAST_CLAIM_REJECTED);
        return kFestivalBeastConfigInvalid;
    }
    if (!PetSystem::HasExclusiveGiftSlot(*petComp, elig->reward_pet_table_id())) {
        response.set_outcome(FESTIVAL_BEAST_CLAIM_REJECTED);
        return kFestivalBeastExclusiveSlotOccupied;
    }

    // 4. 先预备完整宠物实例并分配永久 pet_id；号源失败直接返回，绝不消耗资格或写入回执。
    PetInstance candidatePet;
    const uint32_t prepErr =
        PetSystem::PrepareExclusiveActivityPet(player, elig->reward_pet_table_id(), candidatePet);
    if (prepErr != kSuccess) {
        response.set_outcome(prepErr == kPetIdGenerateFailed ? FESTIVAL_BEAST_CLAIM_UNKNOWN
                                                             : FESTIVAL_BEAST_CLAIM_REJECTED);
        return prepErr;
    }

    // 5. 单帧同步原子提交：宠物实例 + 终身领取回执一起写入 GROWTH 组并触发立即资产存盘。
    const uint64_t grantedPetId = candidatePet.pet_id();
    const uint32_t frozenVersion = elig->reward_definition_version();
    const uint32_t frozenPetTableId = elig->reward_pet_table_id();
    PetSystem::CommitPreparedActivityPet(player, std::move(candidatePet));
    auto* receipt = comp->add_claim_receipts();
    receipt->set_reward_identity(request.reward_identity());
    receipt->set_request_id(request.client_request_id());
    receipt->set_reward_definition_version(frozenVersion);
    receipt->set_reward_pet_table_id(frozenPetTableId);
    receipt->set_pet_id(grantedPetId);
    receipt->set_claimed_at_ms(nowMs);
    comp->set_state_revision(comp->state_revision() + 1);

    RequestAssetPersist(player, true);
    const bool durable = IsClaimDurable(player, *receipt);
    response.set_outcome(FESTIVAL_BEAST_CLAIM_APPLIED);
    response.set_replayed(false);
    response.set_durable(durable);
    response.set_reward_definition_version(frozenVersion);
    response.set_reward_pet_table_id(frozenPetTableId);
    response.set_pet_id(grantedPetId);
    response.set_claimed_at_ms(nowMs);
    if (durable) {
        PetSystem::PushList(player);
    }
    LOG_INFO << "[FestivalBeast] Reward claimed: player_id=" << tlsEcs.GetPlayerId(player)
             << " reward_identity=" << request.reward_identity()
             << " pet_id=" << grantedPetId << " durable=" << durable;
    return kSuccess;
}

uint32_t PlayerFestivalBeastSystem::VerifyPrepareBattlePet(entt::entity player,
                                                           const FestivalBeastBattleContext& ctx) {
    if (ctx.locked_pet_id() == 0) {
        return kFestivalBeastConfigInvalid;
    }
    uint64_t checkedPetId = 0;
    const uint32_t err = ValidateActivePetForBattle(player, ctx.locked_pet_id(), checkedPetId);
    if (err != kSuccess) {
        if (auto* comp = tlsEcs.actorRegistry.try_get<PlayerFestivalBeastComp>(player);
            comp != nullptr && comp->has_pending_launch() &&
            comp->pending_launch().request_id() == ctx.client_request_id()) {
            auto* pending = comp->mutable_pending_launch();
            pending->set_launch_state(static_cast<uint32_t>(FESTIVAL_BEAST_LAUNCH_REJECTED));
            pending->set_battle_id(0);
            pending->set_reject_tip_id(err);
            pending->set_reject_reason("出战宝宝状态已变化，本次开局已取消");
            comp->set_state_revision(comp->state_revision() + 1);
        }
        return err;
    }
    return kSuccess;
}

void PlayerFestivalBeastSystem::OnBattlePrepareCancelledOrVoid(entt::entity player, uint64_t battleId,
                                                               uint64_t /*nowMs*/) {
    if (!tlsEcs.actorRegistry.valid(player) || battleId == 0) return;
    auto* comp = tlsEcs.actorRegistry.try_get<PlayerFestivalBeastComp>(player);
    if (comp == nullptr || !comp->has_pending_launch()) return;
    if (comp->pending_launch().battle_id() == battleId) {
        comp->clear_pending_launch();
        comp->set_state_revision(comp->state_revision() + 1);
        PlayerLifecycleSystem::RequestKeyProgressSave(player, "festival_beast_launch_cleared");
    }
}

uint32_t PlayerFestivalBeastSystem::PrepareBattleCredit(const PlayerFestivalBeastComp& current,
                                                        const BattleSettlementData& settlement,
                                                        uint64_t nowMs,
                                                        PlayerFestivalBeastComp& candidate,
                                                        SettlementDelta& deltaOut) {
    deltaOut = SettlementDelta{};
    candidate = current;
    if (!settlement.has_festival_beast() || settlement.festival_beast().activity_instance_id() == 0) {
        return kSuccess;
    }
    const auto& fb = settlement.festival_beast();
    if (fb.reward_identity().empty() || fb.required_wins() == 0) {
        return kInvalidParameter;
    }
    deltaOut.active = true;
    deltaOut.activityInstanceId = fb.activity_instance_id();
    deltaOut.rewardIdentity = fb.reward_identity();
    deltaOut.requiredWins = fb.required_wins();

    // 结算到达时清理匹配的在途开局状态。
    if (candidate.has_pending_launch() &&
        (candidate.pending_launch().battle_id() == settlement.battle_id() ||
         (!fb.client_request_id().empty() &&
          candidate.pending_launch().request_id() == fb.client_request_id()))) {
        candidate.clear_pending_launch();
        deltaOut.changed = true;
    }

    const auto* existingPeriod = FindPeriodProgress(candidate, fb.activity_instance_id());
    const uint32_t winsBefore =
        existingPeriod != nullptr ? std::min(existingPeriod->wins(), fb.required_wins()) : 0u;
    deltaOut.winsBefore = winsBefore;
    deltaOut.winsAfter = winsBefore;

    // 失败、逃跑、作废绝不增加胜场，也绝不清空已有进度。
    const bool playerWon =
        !settlement.fled() &&
        ((settlement.outcome() == BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 0) ||
         (settlement.outcome() == BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 1));
    if (!playerWon) {
        if (deltaOut.changed) {
            candidate.set_state_revision(candidate.state_revision() + 1);
        }
        return kSuccess;
    }

    auto* period = MutablePeriodProgress(candidate, fb.activity_instance_id());
    if (period == nullptr) {
        period = candidate.add_period_progress();
        period->set_activity_instance_id(fb.activity_instance_id());
        period->set_required_wins(fb.required_wins());
        period->set_reward_identity(fb.reward_identity());
    }
    const uint32_t winsAfter = std::min(winsBefore + 1u, fb.required_wins());
    if (winsAfter > winsBefore) {
        period->set_wins(winsAfter);
        period->set_required_wins(fb.required_wins());
        period->set_last_win_at_ms(nowMs);
        period->set_reward_identity(fb.reward_identity());
        deltaOut.winsAfter = winsAfter;
        deltaOut.changed = true;
    }

    // 达标生成永久待领资格，并冻结取得时的奖励定义与版本（复刻/改表不改已赚取权益）。
    if (winsAfter >= fb.required_wins() && FindEligibility(candidate, fb.reward_identity()) == nullptr) {
        auto* elig = candidate.add_eligibilities();
        elig->set_reward_identity(fb.reward_identity());
        elig->set_reward_definition_version(fb.reward_definition_version());
        elig->set_reward_pet_table_id(fb.reward_pet_table_id());
        elig->set_source_activity_instance_id(fb.activity_instance_id());
        elig->set_earned_at_ms(nowMs);
        elig->set_earned_battle_id(settlement.battle_id());
        deltaOut.newlyEligible = true;
        deltaOut.changed = true;
    }

    if (deltaOut.changed) {
        candidate.set_state_revision(candidate.state_revision() + 1);
    }
    return kSuccess;
}

void PlayerFestivalBeastSystem::SetPersistForTest(PersistFn fn) {
    gPersistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
}

void PlayerFestivalBeastSystem::SetLaunchSenderForTest(LaunchSenderFn fn) {
    gLaunchSenderFn = std::move(fn);
}

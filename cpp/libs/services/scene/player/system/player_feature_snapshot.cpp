#include "player_feature_snapshot.h"

#include <algorithm>
#include <limits>
#include <set>
#include <utility>
#include <vector>

#include "thread_context/ecs_context.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/condition/condition_util.h"
#include "modules/mission/comp/mission_comp.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_mission.pb.h"
#include "table/code/condition_table.h"
#include "table/code/item_table.h"
#include "table/code/mission_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mission_error_tip.pb.h"
#include "services/scene/player/system/player_mission.h"
#include "services/scene/player/system/player_activity_schedule.h"
#include "engine/core/time/system/time.h"

namespace {

bool CanSortBagType(uint32_t bagType)
{
    return bagType == kInventory || bagType == kWarehouse;
}

const MissionsComp* FindScope(const MissionsContainerComp* container, uint32_t scope)
{
    return container == nullptr ? nullptr : container->Get(scope);
}

void FillMission(entt::entity player, uint64_t nowMs, uint32_t missionId, uint32_t scope, const MissionsComp* missions,
                 const MissionTable* row, PlayerMissionInfo& out)
{
    out.set_mission_id(missionId);
    out.set_scope(scope);
    out.set_configured(row != nullptr);
    const MissionComp* active = nullptr;
    if (missions != nullptr)
    {
        const auto it = missions->GetMissionList().missions().find(missionId);
        if (it != missions->GetMissionList().missions().end()) active = &it->second;
        if (missions->IsClaimable(missionId)) out.set_status(PLAYER_MISSION_CLAIMABLE);
        else if (missions->IsComplete(missionId)) out.set_status(PLAYER_MISSION_COMPLETED);
        else if (active != nullptr)
        {
            const bool failed = active->status() == MissionComp::E_MISSION_TIME_OUT ||
                                active->status() == MissionComp::E_MISSION_FAILD;
            out.set_status(failed ? PLAYER_MISSION_FAILED : PLAYER_MISSION_ACTIVE);
        }
    }

    out.set_can_accept(false);
    out.set_can_claim(false);
    if (row == nullptr) {
        out.set_unavailable_reason("任务配置暂不可用");
        return;
    }
    const auto accept = PlayerMissionSystem::CheckAccept(player, scope, missionId, nowMs);
    const auto claim = PlayerMissionSystem::CheckClaim(player, scope, missionId);
    out.set_can_accept(out.status() == PLAYER_MISSION_NOT_ACCEPTED && accept == kSuccess);
    out.set_can_claim(out.status() == PLAYER_MISSION_CLAIMABLE && claim == kSuccess);
    if (!out.can_accept() && !out.can_claim()) {
        if (out.status() == PLAYER_MISSION_ACTIVE) out.set_unavailable_reason("完成任务目标后领取奖励");
        else if (out.status() == PLAYER_MISSION_COMPLETED) out.set_unavailable_reason("任务已完成");
        else if (out.status() == PLAYER_MISSION_FAILED) out.set_unavailable_reason("任务已失效");
        else if (out.status() == PLAYER_MISSION_CLAIMABLE) out.set_unavailable_reason("当前状态暂不可领奖，请稍后重试");
        else if (accept == kMissionTypeAlreadyExists) out.set_unavailable_reason("请先完成同类型任务");
        else if (row->mission_type() == 2) {
            PlayerActivityInfo schedule;
            PlayerActivityScheduleSystem::BuildInfo(missionId, nowMs, schedule);
            out.set_unavailable_reason(schedule.unavailable_reason());
        }
        else if (accept == kServiceUnavailable) out.set_unavailable_reason("任务所需玩法暂未开放");
        else out.set_unavailable_reason("当前条件下暂不可接取");
    }
    out.set_mission_type(row->mission_type());
    out.set_mission_sub_type(row->mission_sub_type());
    out.set_reward_id(row->reward_id());
    out.set_auto_reward(row->auto_reward() != 0);

    // 进度槽与原始目标索引一致，缺失条件不能挤占后续目标的进度。
    for (int index = 0; index < row->condition_id_size(); ++index)
    {
        const uint32_t conditionId = row->condition_id(index);
        auto* objective = out.add_objectives();
        objective->set_objective_index(static_cast<uint32_t>(index));
        objective->set_condition_id(conditionId);
        const auto [condition, lookupResult] = ConditionTableManager::Instance().FindByIdSilent(conditionId);
        if (condition == nullptr) continue;
        const uint32_t overrideTarget = index < row->target_count_size() ? row->target_count(index) : 0;
        const uint32_t target = overrideTarget > 0 ? overrideTarget : condition->target_count();
        const uint32_t progress = active != nullptr && index < active->progress_size()
            ? active->progress(index) : 0;
        objective->set_category(condition->condition_category());
        objective->set_target(target);
        objective->set_progress(progress);
        // 已完成记录没有历史计数；不伪造计数，完成位单独表达。
        objective->set_completed(out.status() == PLAYER_MISSION_COMPLETED ||
            out.status() == PLAYER_MISSION_CLAIMABLE ||
            (active != nullptr && condition_util::IsFulfilled(conditionId, progress, overrideTarget)));
    }
}

} // namespace

uint32_t PlayerBagSystem::BuildSnapshot(entt::entity player, uint32_t bagType, BagInfo& out)
{
    out.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    if (bagType >= kBagTypeCount) return kInvalidParameter;
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr) return kServiceUnavailable;
    const auto& bag = bags->bags[bagType];
    if (!bag.IsLayerConsistent() || bag.Capacity() > std::numeric_limits<uint32_t>::max())
        return kInvalidTableData;

    auto* layout = out.mutable_layout();
    layout->set_bag_type(bagType);
    layout->set_capacity(static_cast<uint32_t>(bag.Capacity()));
    layout->set_can_sort(CanSortBagType(bagType));
    bag.ForEachItem([&out, layout, &bag](Guid guid, const ItemComp& item) {
        auto* info = out.add_items();
        info->set_item_id(guid);
        info->set_config_id(item.config_id());
        info->set_count(item.size());
        const auto [row, lookupResult] = ItemTableManager::Instance().FindByIdSilent(item.config_id());
        if (row != nullptr)
        {
            info->set_max_stack(row->max_stack_size());
            info->set_equip_kind(row->equip_kind());
        }
        auto* slot = layout->add_slots();
        slot->set_slot(bag.GetItemPosByGuid(guid));
        slot->set_item_id(guid);
        const auto footprint = bag.GetItemFootprintByGuid(guid);
        slot->set_width(footprint.width);
        slot->set_height(footprint.height);
    });
    std::sort(out.mutable_items()->begin(), out.mutable_items()->end(),
        [](const BagItemInfo& left, const BagItemInfo& right) { return left.item_id() < right.item_id(); });
    std::sort(layout->mutable_slots()->begin(), layout->mutable_slots()->end(),
        [](const BagSlotInfo& left, const BagSlotInfo& right) { return left.slot() < right.slot(); });
    if (const auto* currency = tlsEcs.actorRegistry.try_get<CurrencyComp>(player))
        out.mutable_currency()->CopyFrom(*currency);
    return kSuccess;
}

uint32_t PlayerBagSystem::Sort(entt::entity player, uint32_t bagType, BagInfo& out, bool& changed)
{
    out.Clear();
    changed = false;
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    if (!CanSortBagType(bagType)) return kInvalidParameter;
    auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr) return kServiceUnavailable;
    auto& bag = bags->bags[bagType];
    if (!bag.IsLayerConsistent()) return kInvalidTableData;
    const uint32_t result = BagService::SortByPlayerRequest(player, bag, &changed);
    if (result != kSuccess) return result;
    return BuildSnapshot(player, bagType, out);
}

uint32_t PlayerMissionReadSystem::BuildList(entt::entity player, GetMissionListResponse& out)
{
    out.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    const auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player);
    std::set<std::pair<uint32_t, uint32_t>> keys;
    for (const auto& row : MissionTableManager::Instance().FindAll().data())
        keys.emplace(MissionListComp::kPlayerMission, row.id());
    if (container != nullptr)
    {
        for (const auto& [scope, missions] : container->map)
        {
            for (const auto& [id, mission] : missions.GetMissionList().missions()) keys.emplace(scope, id);
            for (const auto& [id, bitIndex] : MissionBitMap)
                if (missions.IsComplete(id) || missions.IsClaimable(id)) keys.emplace(scope, id);
            for (const auto id : missions.GetUnmappedCompletedIds()) keys.emplace(scope, id);
            for (const auto id : missions.GetUnmappedClaimableIds()) keys.emplace(scope, id);
        }
    }
    for (const auto& [scope, id] : keys)
    {
        const auto [row, lookupResult] = MissionTableManager::Instance().FindByIdSilent(id);
        FillMission(player, TimeSystem::NowMillisecondsUTC(), id, scope, FindScope(container, scope), row, *out.add_missions());
    }
    out.set_state_persistent(container != nullptr);
    return kSuccess;
}

uint32_t PlayerActivityReadSystem::BuildList(entt::entity player, uint64_t serverTimeMs,
                                           GetActivityListResponse& out)
{
    out.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    out.set_server_time_ms(serverTimeMs);
    for (const auto& row : MissionTableManager::Instance().FindAll().data())
    {
        if (row.mission_type() != 2) continue;
        auto* info = out.add_activities();
        PlayerActivityScheduleSystem::BuildInfo(row.id(), serverTimeMs, *info);
        const auto acceptance = PlayerMissionSystem::CheckAccept(
            player, MissionListComp::kPlayerMission, row.id(), serverTimeMs);
        info->set_can_participate(info->status() == PLAYER_ACTIVITY_OPEN && acceptance == kSuccess);
        if (info->status() == PLAYER_ACTIVITY_OPEN && !info->can_participate()) {
            if (acceptance == kMissionIdRepeated || acceptance == kMissionAlreadyCompleted)
                info->set_unavailable_reason("请在任务页查看活动进度");
            else if (acceptance == kMissionTypeAlreadyExists)
                info->set_unavailable_reason("请先完成同类型活动任务");
            else info->set_unavailable_reason("活动所需玩法或当前状态暂不满足参与条件");
        }
    }
    std::sort(out.mutable_activities()->begin(), out.mutable_activities()->end(),
        [](const PlayerActivityInfo& left, const PlayerActivityInfo& right) {
            return left.activity_id() < right.activity_id();
        });
    return kSuccess;
}

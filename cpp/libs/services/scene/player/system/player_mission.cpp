#include "player_mission.h"

#include <algorithm>
#include <limits>
#include <set>
#include "thread_context/ecs_context.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/condition/condition_type.h"
#include "modules/mission/comp/mission_comp.h"
#include "modules/mission/system/mission.h"
#include "proto/common/event/mission_event.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "services/scene/player/system/player_activity_schedule.h"
#include "services/scene/player/system/player_lifecycle.h"
#include "table/code/condition_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/monster_table.h"
#include "table/code/reward_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mission_error_tip.pb.h"
#include "table/proto/tip/reward_error_tip.pb.h"

namespace {
uint32_t CheckPlayer(entt::entity player, uint32_t scope) {
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    if (scope != MissionListComp::kPlayerMission) return kInvalidParameter;
    const auto* guid = tlsEcs.actorRegistry.try_get<Guid>(player);
    if (guid == nullptr || *guid == 0 || *guid == kInvalidGuid) return kInvalidParameter;
    if (PlayerLifecycleSystem::IsCrossZoneFrozen(player)) return kInvalidParameter;
    if (tlsEcs.actorRegistry.try_get<MissionsContainerComp>(player) == nullptr) return kServiceUnavailable;
    return kSuccess;
}

// 奖励表没有货币语义；只按现有物品配置发放，不自行解释编号。
uint32_t BuildRewardItems(uint32_t rewardId, ItemCountMap& items) {
    items.clear();
    if (rewardId == 0) return kSuccess;
    const auto [reward, rewardError] = RewardTableManager::Instance().FindByIdSilent(rewardId);
    if (reward == nullptr) return kInvalidTableData;
    for (const auto& entry : reward->reward()) {
        if (entry.reward_item() == 0 || entry.reward_count() == 0) return kInvalidTableData;
        const auto [item, itemError] = ItemTableManager::Instance().FindByIdSilent(entry.reward_item());
        if (item == nullptr) return kInvalidTableData;
        auto& count = items[entry.reward_item()];
        if (entry.reward_count() > std::numeric_limits<uint32_t>::max() - count) return kInvalidTableData;
        count += entry.reward_count();
    }
    return items.empty() ? kInvalidTableData : kSuccess;
}

// 击杀事实仅来自正式副本结算，怪物表有行但没有副本引用仍不可达。
// condition1 是 ANY 集合；空集合表示任意实际可达怪物。
bool HasReachableMonster(const ConditionTable& condition) {
    const auto& dungeons = DungeonTableManager::Instance();
    const auto& monsters = MonsterTableManager::Instance();
    if (dungeons.Count() == 0 || monsters.Count() == 0) return false;
    for (const auto& dungeon : dungeons.FindAll().data()) {
        for (const uint32_t monsterId : dungeon.monster()) {
            if (monsterId == 0 || !monsters.Exists(monsterId)) continue;
            if (condition.condition1().empty() ||
                std::find(condition.condition1().begin(), condition.condition1().end(), monsterId) != condition.condition1().end())
                return true;
        }
    }
    return false;
}

bool HasProgressSource(uint32_t category) {
    switch (static_cast<eConditionType>(category)) {
    case eConditionType::kConditionKillMonster:
    case eConditionType::kConditionLevelUp:
    case eConditionType::kConditionCompleteMission:
        return true;
    default:
        return false;
    }
}
} // namespace

uint32_t PlayerMissionSystem::Initialize(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player)) return kThisEntityIsInvalid;
    auto& container = tlsEcs.actorRegistry.get_or_emplace<MissionsContainerComp>(player);
    auto& missions = container.GetOrCreate(MissionListComp::kPlayerMission);
    missions.GetMutableMissionList().set_type(MissionListComp::kPlayerMission);
    return kSuccess;
}

uint32_t PlayerMissionSystem::CheckAccept(entt::entity player, uint32_t scope,
                                         uint32_t missionId, uint64_t nowMs) {
    if (const auto error = CheckPlayer(player, scope); error != kSuccess) return error;
    const auto [row, rowError] = MissionTableManager::Instance().FindByIdSilent(missionId);
    if (row == nullptr) return kInvalidTableId;
    if (!MissionBitMap.contains(missionId) || row->condition_id().empty() || row->condition_order() > 1)
        return kInvalidTableData;
    const auto& container = tlsEcs.actorRegistry.get<MissionsContainerComp>(player);
    const auto* missions = container.Get(scope);
    if (missions == nullptr) return kServiceUnavailable;
    if (missions->IsAccepted(missionId)) return kMissionIdRepeated;
    if (missions->IsComplete(missionId) || missions->IsClaimable(missionId)) return kMissionAlreadyCompleted;
    if (missions->IsMissionTypeNotRepeated() &&
        missions->GetTypeFilter().contains({row->mission_type(), row->mission_sub_type()}))
        return kMissionTypeAlreadyExists;
    for (int index = 0; index < row->condition_id_size(); ++index) {
        const auto [condition, conditionError] =
            ConditionTableManager::Instance().FindByIdSilent(row->condition_id(index));
        if (condition == nullptr) return kInvalidTableData;
        // 累计事件只支持 >= / > / == 的正向目标；<= / < 不能用累计事件可靠驱动。
        if ((condition->comparison_op() != 0 && condition->comparison_op() != 1 && condition->comparison_op() != 4) ||
            (index < row->target_count_size() && row->target_count(index) > 0
                ? row->target_count(index) : condition->target_count()) == 0)
            return kInvalidTableData;
        if (!HasProgressSource(condition->condition_category()) || condition->valid_duration() != 0)
            return kServiceUnavailable;
        // 当前事实只提供一个槽：怪物配置 ID / 当前等级 / 已完成任务 ID。
        if (!condition->condition2().empty() || !condition->condition3().empty() || !condition->condition4().empty())
            return kServiceUnavailable;
        if (condition->condition_category() == static_cast<uint32_t>(eConditionType::kConditionKillMonster) &&
            !HasReachableMonster(*condition))
            return kServiceUnavailable;
        if (condition->quantity_type() > 1 ||
            (condition->quantity_type() == 1 && condition->condition_category() != static_cast<uint32_t>(eConditionType::kConditionLevelUp)))
            return kServiceUnavailable;
        if (condition->comparison_op() == 1 &&
            (index < row->target_count_size() && row->target_count(index) > 0
                ? row->target_count(index) : condition->target_count()) == std::numeric_limits<uint32_t>::max())
            return kInvalidTableData;
    }
    ItemCountMap items;
    if (const auto error = BuildRewardItems(row->reward_id(), items); error != kSuccess) return error;
    if (row->mission_type() == 2) return PlayerActivityScheduleSystem::CheckOpen(missionId, nowMs);
    return kSuccess;
}

uint32_t PlayerMissionSystem::Accept(entt::entity player, uint32_t scope,
                                    uint32_t missionId, uint64_t nowMs) {
    if (const auto error = CheckAccept(player, scope, missionId, nowMs); error != kSuccess) return error;
    auto& missions = tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(scope);
    AcceptMissionEvent event;
    event.set_entity(entt::to_integral(player));
    event.set_mission_id(missionId);
    const auto result = MissionSystem::AcceptMission(event, missions, MissionConfig::GetSingleton());
    if (result == kSuccess) {
        (*missions.GetMutableMissionList().mutable_mission_begin_time())[missionId] = nowMs;
        if (const auto* level = tlsEcs.actorRegistry.try_get<LevelComp>(player); level != nullptr) {
            ConditionEvent currentLevel;
            currentLevel.set_entity(entt::to_integral(player));
            currentLevel.set_condition_type(static_cast<uint32_t>(eConditionType::kConditionLevelUp));
            currentLevel.add_condition_ids(level->level());
            currentLevel.set_amount(level->level());
            MissionSystem::HandleConditionEvent(currentLevel, missions, MissionConfig::GetSingleton(), missionId);
        }
        // 完成事实已持久化：晚接取依赖任务也能补齐，但只更新本次新任务。
        // 去重后每个历史任务至多贡献一次，不能借反复接取其它任务重放进度。
        const auto [row, rowError] = MissionTableManager::Instance().FindByIdSilent(missionId);
        std::set<uint32_t> relevantCompleted;
        if (row != nullptr) {
            for (const uint32_t conditionId : row->condition_id()) {
                const auto [condition, conditionError] = ConditionTableManager::Instance().FindByIdSilent(conditionId);
                if (condition == nullptr || condition->condition_category() !=
                    static_cast<uint32_t>(eConditionType::kConditionCompleteMission)) continue;
                if (condition->condition1().empty()) {
                    for (const auto& [completedId, bit] : MissionBitMap)
                        if (missions.IsComplete(completedId)) relevantCompleted.insert(completedId);
                    relevantCompleted.insert(missions.GetUnmappedCompletedIds().begin(), missions.GetUnmappedCompletedIds().end());
                } else {
                    for (const uint32_t completedId : condition->condition1())
                        if (missions.IsComplete(completedId)) relevantCompleted.insert(completedId);
                }
            }
        }
        for (const uint32_t completedId : relevantCompleted) {
            ConditionEvent completed;
            completed.set_entity(entt::to_integral(player));
            completed.set_condition_type(static_cast<uint32_t>(eConditionType::kConditionCompleteMission));
            completed.add_condition_ids(completedId);
            completed.set_amount(1);
            MissionSystem::HandleConditionEvent(completed, missions, MissionConfig::GetSingleton(), missionId);
        }
    }
    return result;
}

uint32_t PlayerMissionSystem::CheckClaim(entt::entity player, uint32_t scope, uint32_t missionId) {
    if (const auto error = CheckPlayer(player, scope); error != kSuccess) return error;
    const auto& container = tlsEcs.actorRegistry.get<MissionsContainerComp>(player);
    const auto* missions = container.Get(scope);
    if (missions == nullptr) return kServiceUnavailable;
    if (!missions->IsClaimable(missionId))
        return missions->IsComplete(missionId) ? kRewardAlreadyClaimed : kMissionIdNotInRewardList;
    // 存档中的完成位与待领位必须同时存在，不能用客户端请求修复不一致状态并发奖。
    if (!missions->IsComplete(missionId)) return kInvalidTableData;
    const auto [row, rowError] = MissionTableManager::Instance().FindByIdSilent(missionId);
    if (row == nullptr || row->reward_id() == 0) return kInvalidTableData;
    if (tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player) == nullptr) return kServiceUnavailable;
    ItemCountMap items;
    return BuildRewardItems(row->reward_id(), items);
}

uint32_t PlayerMissionSystem::ClaimReward(entt::entity player, uint32_t scope, uint32_t missionId) {
    if (const auto error = CheckClaim(player, scope, missionId); error != kSuccess) return error;
    auto& missions = tlsEcs.actorRegistry.get<MissionsContainerComp>(player).map.at(scope);
    auto& bag = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    const auto [row, rowError] = MissionTableManager::Instance().FindByIdSilent(missionId);
    if (row == nullptr) return kInvalidTableData;
    ItemCountMap items;
    if (const auto error = BuildRewardItems(row->reward_id(), items); error != kSuccess) return error;
    const PlayerItemBlockList emptyBlockList;
    const auto* blockList = tlsEcs.actorRegistry.try_get<PlayerItemBlockList>(player);
    // 人物背包不淘汰存量物品；整批预检失败时不改包，也不消耗领取权。
    const auto result = BagService::AddItems(player, bag,
        blockList == nullptr ? emptyBlockList : *blockList, items, TX_QUEST_REWARD);
    if (result != kSuccess) return result;
    // scene EventLoop 内同步提交，无异步间隙；发奖成功后才消耗领取权。
    missions.ClearClaimable(missionId);
    return kSuccess;
}

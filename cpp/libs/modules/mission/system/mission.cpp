#include "mission.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/utils/registry/actor_log.h"

#include <algorithm>
#include <ranges>
#include <limits>
#include <unordered_set>
#include <vector>

#include "muduo/base/Logging.h"
#include "engine/core/error_handling/error_handling.h"
#include "engine/core/macros/return_define.h"
#include "engine/core/macros/error_return.h"
#include "engine/core/utils/bit_index/bit_index_util.h"

#include "condition/condition_util.h"
#include "mission/comp/mission_comp.h"

#include "table/code/condition_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mission_error_tip.pb.h"
#include "proto/common/component/mission_comp.pb.h"
#include "modules/mission/mission_events.h"
#include <condition/condition_type.h>

uint32_t MissionSystem::ConsumeRewardClaim(const RewardClaimParam &param, MissionsComp &missionComp)
{
	// 先确认实体存活,再读取玩家身份组件;缺失时拒绝领取,不补默认身份。
	if (!tlsEcs.actorRegistry.valid(param.playerEntity))
	{
		LOG_ERROR << "Claim reward failed: invalid player entity = " << entt::to_integral(param.playerEntity);
		return PrintStackAndReturnError(kInvalidParameter);
	}

	const auto* owner = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(param.playerEntity);
    if (owner == nullptr) return kInvalidParameter;
    const auto playerId = owner->value;

	if (!missionComp.IsClaimable(param.missionId))
	{
		LOG_ERROR << "Claim reward failed: mission not claimable, missionId = " << param.missionId
				  << ", playerId = " << playerId;
		return PrintStackAndReturnError(kMissionIdNotInRewardList);
	}

	// 仅提交领取状态；生产发奖必须走 PlayerMissionSystem::ClaimReward。
	missionComp.ClearClaimable(param.missionId);
	LOG_INFO << "Reward claimed: missionId = " << param.missionId << ", playerId = " << playerId;
	return kSuccess;
}

uint32_t MissionSystem::CheckMissionAcceptance(const AcceptMissionEvent &acceptEvent, MissionsComp &missionComp, const IMissionConfig &config)
{
	const uint32_t missionId = acceptEvent.missionId;

	RETURN_ON_ERROR(missionComp.ValidateNotAccepted(missionId));
	RETURN_ON_ERROR(missionComp.ValidateNotCompleted(missionId));
	RETURN_IF_TRUE(!config.HasKey(missionId) || !MissionBitMap.contains(missionId), kInvalidTableId);
    for (const auto conditionId : config.GetConditionIds(missionId))
    {
        const auto [condition, error] = ConditionTableManager::Instance().FindByIdSilent(conditionId);
        RETURN_IF_TRUE(condition == nullptr, kInvalidTableData);
    }

	// When a (sub)type may only be active once, reject a second mission of that type.
	if (missionComp.IsMissionTypeNotRepeated())
	{
		const auto typeKey = std::make_pair(config.GetMissionType(missionId), config.GetMissionSubType(missionId));
		RETURN_IF_TRUE(missionComp.GetTypeFilter().count(typeKey) > 0, kMissionTypeAlreadyExists);
	}

	return kSuccess;
}

uint32_t MissionSystem::AcceptMission(const AcceptMissionEvent &acceptEvent, MissionsComp &missionComp, const IMissionConfig &config)
{
	const entt::entity playerEntity = acceptEvent.player;
	const uint32_t missionId = acceptEvent.missionId;

	if (const uint32_t checkResult = CheckMissionAcceptance(acceptEvent, missionComp, config); checkResult != kSuccess)
	{
		LOG_ERROR << "Accept mission rejected: missionId = " << missionId
				  << " " << ActorLog(playerEntity);
		return checkResult;
	}

	// Reserve this (sub)type so a duplicate of the same type cannot be accepted later.
	if (missionComp.IsMissionTypeNotRepeated())
	{
		const auto typeKey = std::make_pair(config.GetMissionType(missionId), config.GetMissionSubType(missionId));
		missionComp.GetMutableTypeFilter().emplace(typeKey);
	}

	// Create the runtime mission with one zero-initialised progress slot per condition,
	// and index it by condition category so condition events can locate it quickly.
	MissionComp mission;
	mission.set_id(missionId);
	for (const auto &conditionId : config.GetConditionIds(missionId))
	{
		LookupConditionOrContinue(conditionId);
		mission.add_progress(0);
		missionComp.GetMutableEventMissionsClassify()[conditionRow->condition_category()].emplace(missionId);
	}
	missionComp.GetMutableMissionList().mutable_missions()->insert({missionId, std::move(mission)});

	LOG_INFO << "Mission accepted: missionId = " << missionId
			 << " " << ActorLog(playerEntity);
	return kSuccess;
}

uint32_t MissionSystem::AbandonMission(const AbandonParam &param, MissionsComp &missionComp, const IMissionConfig &config)
{
	if (kMissionAlreadyCompleted == missionComp.ValidateNotCompleted(param.missionId))
	{
		return MAKE_ERROR_MSG(kMissionAlreadyCompleted,
							  "missionId=" << param.missionId
										   << " " << ActorLog(param.playerEntity));
	}

	missionComp.ClearClaimable(param.missionId);
	missionComp.GetMutableMissionList().mutable_missions()->erase(param.missionId);
	missionComp.AbandonMission(param.missionId);
	missionComp.GetMutableMissionList().mutable_mission_begin_time()->erase(param.missionId);
	UnregisterMissionIndexes(missionComp, param.missionId, config);

	LOG_INFO << "Mission abandoned: missionId = " << param.missionId
			 << " " << ActorLog(param.playerEntity);
	return kSuccess;
}

// CompleteAllMissions — Bulk-completion helper (GM / debug / admin tools).
//
// Marks every currently-accepted mission as completed in the player's
// completed-bitmap, then clears the active mission list. It does NOT:
//   - grant rewards or set claimable bits
//   - enqueue the next mission in a chain
//   - fire the kConditionCompleteMission event
//
// Intended for GM "clear all my quests" / test-harness scenarios where you
// want the bitmap state without side effects.
//
// For the normal in-game "this mission's conditions are now met" path,
// use OnMissionCompletion instead (called from HandleConditionEvent).
// The distinction is intentional — do NOT collapse the two functions.
// See todo.md #225 for the historical lesson.
void MissionSystem::CompleteAllMissions(entt::entity /*playerEntity*/, MissionsComp &missionComp)
{
	for (const auto &missionId : missionComp.GetMissionList().missions() | std::views::keys)
	{
		SetBit(MissionBitMap, missionComp.GetMutableCompletedMissions(), missionId);
	}
	missionComp.GetMutableMissionList().mutable_missions()->clear();
}

bool MissionSystem::AreAllConditionsFulfilled(const MissionComp &mission, uint32_t missionId, const IMissionConfig &config)
{
	const auto &conditionIds = config.GetConditionIds(missionId);
	const auto &targetCounts = config.GetTargetCounts(missionId);

    // 少目标的旧/损坏进度不能因为只遍历已有槽而提前完成。
    if (conditionIds.empty() || mission.progress_size() != conditionIds.size()) return false;
	const int32_t slotCount = mission.progress_size();
	for (int32_t i = 0; i < slotCount; ++i)
	{
		const uint32_t targetCount = (i < targetCounts.size()) ? targetCounts.at(i) : 0;
		if (!condition_util::IsFulfilled(conditionIds.at(i), mission.progress(i), targetCount))
		{
			return false;
		}
	}
	return true;
}

void MissionSystem::HandleConditionEvent(const ConditionEvent &conditionEvent, MissionsComp &missionComp, const IMissionConfig &config, MissionFollowUps& out, uint32_t onlyMissionId)
{
	if (conditionEvent.conditionIds.empty())
	{
		LOG_ERROR << "HandleConditionEvent: empty condition IDs for entity = " << entt::to_integral(conditionEvent.player);
		return;
	}

	// Only missions that watch this condition category can be affected by the event.
	auto &missionsByCondition = missionComp.GetMutableEventMissionsClassify();
	const auto watchersIt = missionsByCondition.find(conditionEvent.conditionType);
	if (watchersIt == missionsByCondition.end())
	{
		return;
	}

	const entt::entity playerEntity = conditionEvent.player;
	auto &missions = *missionComp.GetMutableMissionList().mutable_missions();

	std::unordered_set<uint32_t> justCompleted;
	for (const uint32_t missionId : watchersIt->second)
	{
        if (onlyMissionId != 0 && missionId != onlyMissionId) continue;
		const auto missionIt = missions.find(missionId);
		if (missionIt == missions.end())
		{
			continue;
		}

		MissionComp &mission = missionIt->second;
        if (mission.id() != missionId || !config.HasKey(missionId) || missionComp.IsComplete(missionId) ||
            mission.status() == MissionComp::E_MISSION_TIME_OUT || mission.status() == MissionComp::E_MISSION_FAILD)
            continue;
		if (!UpdateMissionProgress(conditionEvent, mission, config))
		{
			continue;
		}
		if (!AreAllConditionsFulfilled(mission, missionId, config))
		{
			continue;
		}

		mission.set_status(MissionComp::E_MISSION_COMPLETE);
		justCompleted.emplace(missionId);
		missions.erase(missionIt);
	}

	OnMissionCompletion(playerEntity, justCompleted, missionComp, config, out);
}

void MissionSystem::RemoveMissionFromConditionIndex(MissionsComp &missionComp, uint32_t missionId, const IMissionConfig &config)
{
	for (const auto &conditionId : config.GetConditionIds(missionId))
	{
		LookupConditionOrContinue(conditionId);
		missionComp.GetMutableEventMissionsClassify()[conditionRow->condition_category()].erase(missionId);
	}
}

void MissionSystem::UnregisterMissionIndexes(MissionsComp &missionComp, uint32_t missionId, const IMissionConfig &config)
{
	RemoveMissionFromConditionIndex(missionComp, missionId, config);

	const uint32_t missionSubType = config.GetMissionSubType(missionId);
	if (missionComp.IsMissionTypeNotRepeated())
	{
		missionComp.GetMutableTypeFilter().erase(
			std::make_pair(config.GetMissionType(missionId), missionSubType));
	}
}

bool MissionSystem::UpdateMissionProgress(const ConditionEvent &conditionEvent, MissionComp &mission, const IMissionConfig &config)
{
	// A condition whose table defines no slot filters matches *any* event, so an
	// event carrying no condition ids must not be allowed to advance progress.
	if (conditionEvent.conditionIds.empty())
	{
		return false;
	}

	const auto &conditionIds = config.GetConditionIds(mission.id());
	const auto &targetCounts = config.GetTargetCounts(mission.id());

    if (mission.progress_size() != conditionIds.size()) return false;
	const int32_t slotCount = mission.progress_size();
    const bool ordered = config.IsConditionOrdered(mission.id());
	bool progressed = false;
	for (int32_t i = 0; i < slotCount; ++i)
	{
		LookupConditionOrContinue(conditionIds.at(i));
		const uint32_t targetCount = (i < targetCounts.size()) ? targetCounts.at(i) : 0;
        if (ordered && condition_util::IsFulfilled(conditionIds.at(i), mission.progress(i), targetCount)) continue;
		if (UpdateProgressIfConditionMatches(conditionEvent, mission, i, conditionRow, targetCount))
		{
			progressed = true;
		}
        // 顺序目标一条事实最多推进当前一格，不能同一击杀跨过重复的后续目标。
        if (ordered) break;
	}
	return progressed;
}

bool MissionSystem::UpdateProgressIfConditionMatches(const ConditionEvent &conditionEvent, MissionComp &mission, int index, const ConditionTable *conditionTable, uint32_t targetCount)
{
	const auto currentProgress = mission.progress(index);

	// Already satisfied -- nothing left to add for this slot.
	if (condition_util::IsFulfilled(conditionTable->id(), currentProgress, targetCount))
	{
		return false;
	}
	// The event must match this condition's category and its slot filters.
	if (conditionEvent.conditionType != conditionTable->condition_category())
	{
		return false;
	}
    if (conditionTable->condition_category() == static_cast<uint32_t>(eConditionType::kConditionLevelUp))
    {
        // condition1 是等级门槛：已有 20 级的玩家接取“达到 10 级”也应满足。
        if (!conditionTable->condition1().empty() && std::none_of(
            conditionTable->condition1().begin(), conditionTable->condition1().end(),
            [&conditionEvent](uint32_t level) { return conditionEvent.amount >= level; })) return false;
        if (!conditionTable->condition2().empty() || !conditionTable->condition3().empty() || !conditionTable->condition4().empty())
            return false;
    }
    else if (!condition_util::MatchesEventSlots(conditionTable, conditionEvent.conditionIds))
    {
        return false;
    }

    const uint64_t target = targetCount > 0 ? targetCount : conditionTable->target_count();
    const bool owned = conditionTable->quantity_type() == 1 ||
        conditionTable->condition_category() == static_cast<uint32_t>(eConditionType::kConditionLevelUp);
    const uint64_t candidate = owned ? conditionEvent.amount :
        uint64_t{currentProgress} + conditionEvent.amount;
    // 累计目标饱和；严格 > 的完成值是 target+1，不能夹回 target 后又变成未完成。
    const uint64_t ceiling = conditionTable->comparison_op() == 1 ? target + 1 : target;
    const auto newProgress = static_cast<uint32_t>(std::min<uint64_t>(
        owned ? candidate : std::min(candidate, ceiling), std::numeric_limits<uint32_t>::max()));
    if (newProgress == currentProgress) return false;
    mission.set_progress(index, newProgress);
    return true;
}

// 本轮所有任务状态先落定,再由玩家任务入口按奖励、接续、完成事实的顺序驱动后续动作。
// CompleteAllMissions 只做 GM 批量完成,不收集这些业务副作用。
void MissionSystem::OnMissionCompletion(entt::entity /*playerEntity*/, const std::unordered_set<uint32_t>& completedMissions, MissionsComp& missionComp, const IMissionConfig& config, MissionFollowUps& out)
{
    for (const uint32_t missionId : completedMissions)
    {
        UnregisterMissionIndexes(missionComp, missionId, config);
        SetBit(MissionBitMap, missionComp.GetMutableCompletedMissions(), missionId);
        switch (GetRewardAction(config, missionId))
        {
        case RewardAction::kAutoGrant:
            // 发奖成功前保留领取权;满包、禁发或号段不足时仍可重试。
            missionComp.RestoreClaimable(missionId);
            out.autoGrant.push_back(missionId);
            break;
        case RewardAction::kClaimable:
            SetBit(MissionBitMap, missionComp.GetClaimableRewards(), missionId);
            break;
        default:
            break;
        }
        for (const uint32_t nextMissionId : config.GetNextMissionTableIds(missionId))
        {
            out.chainAccept.push_back(nextMissionId);
        }
        out.completed.push_back(missionId);
    }
}
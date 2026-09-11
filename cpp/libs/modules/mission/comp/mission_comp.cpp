#include "mission_comp.h"

#include "table/code/condition_table.h"
#include "core/macros/return_define.h"
#include "core/macros/error_return.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mission_error_tip.pb.h"
#include "proto/common/component/mission_comp.pb.h"
#include "proto/common/event/mission_event.pb.h"
#include <condition/condition_type.h>

MissionsComp::MissionsComp()
{
	for (uint32_t i = static_cast<uint32_t>(eConditionType::kConditionKillMonster);
	     i < static_cast<uint32_t>(eConditionType::kConditionTypeMax); ++i)
	{
		eventMissionsClassify_.emplace(i, UInt32Set{});
	}
}

std::size_t MissionsComp::CanGetRewardSize() const
{
	return claimableRewards_.count() + unmappedClaimableIds_.size();
}

void MissionsComp::AbandonMission(uint32_t missionId)
{
    SetBit(MissionBitMap, completedMissions_, missionId, false);
    unmappedCompletedIds_.erase(missionId);
}

void MissionsComp::RestoreCompleted(uint32_t missionId)
{
    if (!SetBit(MissionBitMap, completedMissions_, missionId)) unmappedCompletedIds_.insert(missionId);
}

void MissionsComp::RestoreClaimable(uint32_t missionId)
{
    if (!SetBit(MissionBitMap, claimableRewards_, missionId)) unmappedClaimableIds_.insert(missionId);
}

void MissionsComp::ClearClaimable(uint32_t missionId)
{
    SetBit(MissionBitMap, claimableRewards_, missionId, false);
    unmappedClaimableIds_.erase(missionId);
}

void MissionsComp::RebuildIndexes(const IMissionConfig& config)
{
    for (auto& [category, ids] : eventMissionsClassify_) ids.clear();
    typeFilter_.clear();
    for (const auto& [missionId, mission] : missionList_.missions())
    {
        if (!config.HasKey(missionId) || IsComplete(missionId)) continue;
        if (missionTypeNotRepeated_)
            typeFilter_.emplace(config.GetMissionType(missionId), config.GetMissionSubType(missionId));
        for (const auto conditionId : config.GetConditionIds(missionId))
        {
            const auto [condition, error] = ConditionTableManager::Instance().FindByIdSilent(conditionId);
            if (condition != nullptr) eventMissionsClassify_[condition->condition_category()].insert(missionId);
        }
    }
}

uint32_t MissionsComp::ValidateNotAccepted(uint32_t missionId) const
{
	if (missionList_.missions().find(missionId) != missionList_.missions().end())
	{
		return MAKE_ERROR_MSG(kMissionIdRepeated, "missionId=" << missionId);
	}
	return kSuccess;
}

uint32_t MissionsComp::ValidateNotCompleted(uint32_t missionId) const
{
	if (IsComplete(missionId))
	{
		return MAKE_ERROR_MSG(kMissionAlreadyCompleted, "missionId=" << missionId);
	}
	return kSuccess;
}

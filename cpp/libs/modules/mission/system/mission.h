#pragma once

#include <unordered_set>

#include "core/type_define/type_define.h"
#include "modules/mission/comp/missions_config_comp.h"
#include "core/utils/registry/game_registry.h"

class ConditionTable;

struct GetRewardParam {
	entt::entity playerEntity{ entt::null };
	uint32_t missionId{ 0 };
};

struct AbandonParam {
	entt::entity playerEntity{ entt::null };
	uint32_t missionId{ 0 };
};

class AcceptMissionEvent;
class ConditionEvent;
class MissionComp;
class MissionsComp;

/// Stateless system that drives the full mission lifecycle:
/// accept, abandon, progress, complete, and reward.
class MissionSystem {
public:
	static uint32_t GetMissionReward(const GetRewardParam& param, MissionsComp& missionComp);
	static uint32_t AcceptMission(const AcceptMissionEvent& acceptEvent, MissionsComp& missionComp, const IMissionConfig& config);
	static uint32_t AbandonMission(const AbandonParam& param, MissionsComp& missionComp, const IMissionConfig& config);
	static void CompleteAllMissions(entt::entity player, MissionsComp& missionComp);
	// onlyMissionId limits owned/history refresh to a newly accepted mission; live events use the default fan-out.
	static void HandleConditionEvent(const ConditionEvent& conditionEvent, MissionsComp& missionComp, const IMissionConfig& config, uint32_t onlyMissionId = 0);

private:
	// NOTE: CompleteAllMissions (above, public) and OnMissionCompletion (below, private)
	// look similar but are NOT interchangeable. CompleteAllMissions is GM/debug bulk-clear
	// without side effects; OnMissionCompletion runs the full reward + chain + condition
	// fan-out on the normal completion path. See mission.cpp for full contract.
	// Reference: todo.md #225.
	static uint32_t CheckMissionAcceptance(const AcceptMissionEvent& acceptEvent, MissionsComp& missionComp, const IMissionConfig& config);
	static bool UpdateMissionProgress(const ConditionEvent& conditionEvent, MissionComp& mission, const IMissionConfig& config);
	static bool UpdateProgressIfConditionMatches(const ConditionEvent& conditionEvent, MissionComp& mission, int index, const ConditionTable* conditionRow, uint32_t targetCount);
	static bool AreAllConditionsFulfilled(const MissionComp& mission, uint32_t missionId, const IMissionConfig& config);
	static void OnMissionCompletion(entt::entity player, const std::unordered_set<uint32_t>& completedMissions, MissionsComp& missionComp, const IMissionConfig& config);
	// Removes all index entries for a mission: the condition->missions lookup and the type filter.
	static void UnregisterMissionIndexes(MissionsComp& missionComp, uint32_t missionId, const IMissionConfig& config);
	// Removes a mission from the condition->missions lookup only.
	static void RemoveMissionFromConditionIndex(MissionsComp& missionComp, uint32_t missionId, const IMissionConfig& config);
};
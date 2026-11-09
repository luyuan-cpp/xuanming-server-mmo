#pragma once

#include <unordered_set>
#include <vector>

#include "modules/mission/comp/missions_config_comp.h"
#include "core/utils/registry/game_registry.h"

class ConditionTable;

struct RewardClaimParam {
	entt::entity playerEntity{ entt::null };
	uint32_t missionId{ 0 };
};

struct AbandonParam {
	entt::entity playerEntity{ entt::null };
	uint32_t missionId{ 0 };
};

struct AcceptMissionEvent;
struct ConditionEvent;
class MissionComp;
class MissionsComp;

// 先收集后续动作,待本轮所有任务状态落定后由玩家任务入口统一执行。
struct MissionFollowUps {
    std::vector<uint32_t> autoGrant;
    std::vector<uint32_t> chainAccept;
    std::vector<uint32_t> completed;
};

/// Stateless system that drives the full mission lifecycle:
/// accept, abandon, progress, complete, and reward.
class MissionSystem {
public:
	// 只校验并消耗领取权，不发放奖励；生产发奖由玩家任务入口编排。
	static uint32_t ConsumeRewardClaim(const RewardClaimParam& param, MissionsComp& missionComp);
	static uint32_t AcceptMission(const AcceptMissionEvent& acceptEvent, MissionsComp& missionComp, const IMissionConfig& config);
	static uint32_t AbandonMission(const AbandonParam& param, MissionsComp& missionComp, const IMissionConfig& config);
	static void CompleteAllMissions(entt::entity player, MissionsComp& missionComp);
	// 只结算本轮任务状态并收集后续动作,不发奖或接续;onlyMissionId 限定接取时的补算范围。
	static void HandleConditionEvent(const ConditionEvent& conditionEvent, MissionsComp& missionComp, const IMissionConfig& config, MissionFollowUps& out, uint32_t onlyMissionId = 0);

private:
	// CompleteAllMissions 仅供 GM 批量完成;正常流程必须经 OnMissionCompletion
	// 收集奖励、接续与完成事实,由玩家任务入口在状态落定后驱动。
	static uint32_t CheckMissionAcceptance(const AcceptMissionEvent& acceptEvent, MissionsComp& missionComp, const IMissionConfig& config);
	static bool UpdateMissionProgress(const ConditionEvent& conditionEvent, MissionComp& mission, const IMissionConfig& config);
	static bool UpdateProgressIfConditionMatches(const ConditionEvent& conditionEvent, MissionComp& mission, int index, const ConditionTable* conditionRow, uint32_t targetCount);
	static bool AreAllConditionsFulfilled(const MissionComp& mission, uint32_t missionId, const IMissionConfig& config);
	static void OnMissionCompletion(entt::entity player, const std::unordered_set<uint32_t>& completedMissions, MissionsComp& missionComp, const IMissionConfig& config, MissionFollowUps& out);
	// Removes all index entries for a mission: the condition->missions lookup and the type filter.
	static void UnregisterMissionIndexes(MissionsComp& missionComp, uint32_t missionId, const IMissionConfig& config);
	// Removes a mission from the condition->missions lookup only.
	static void RemoveMissionFromConditionIndex(MissionsComp& missionComp, uint32_t missionId, const IMissionConfig& config);
};

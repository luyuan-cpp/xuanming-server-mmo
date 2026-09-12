#include "mission_event_handler.h"
#include "thread_context/ecs_context.h"

///<<< BEGIN WRITING YOUR CODE
#include "modules/mission/comp/mission_comp.h"
#include "modules/mission/system/mission.h"
#include "modules/mission/comp/missions_config_comp.h"
#include "services/scene/player/system/player_mission.h"
#include "services/scene/player/system/player_lifecycle.h"
#include "core/time/system/time.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "muduo/base/Logging.h"
///<<< END WRITING YOUR CODE
void MissionEventHandler::Register()
{
    tlsEcs.dispatcher.sink<AcceptMissionEvent>().connect<&MissionEventHandler::AcceptMissionEventHandler>();
    tlsEcs.dispatcher.sink<ConditionEvent>().connect<&MissionEventHandler::ConditionEventHandler>();
    tlsEcs.dispatcher.sink<OnAcceptedMissionEvent>().connect<&MissionEventHandler::OnAcceptedMissionEventHandler>();
    tlsEcs.dispatcher.sink<OnMissionAwardEvent>().connect<&MissionEventHandler::OnMissionAwardEventHandler>();
}

void MissionEventHandler::UnRegister()
{
    tlsEcs.dispatcher.sink<AcceptMissionEvent>().disconnect<&MissionEventHandler::AcceptMissionEventHandler>();
    tlsEcs.dispatcher.sink<ConditionEvent>().disconnect<&MissionEventHandler::ConditionEventHandler>();
    tlsEcs.dispatcher.sink<OnAcceptedMissionEvent>().disconnect<&MissionEventHandler::OnAcceptedMissionEventHandler>();
    tlsEcs.dispatcher.sink<OnMissionAwardEvent>().disconnect<&MissionEventHandler::OnMissionAwardEventHandler>();
}
void MissionEventHandler::AcceptMissionEventHandler(const AcceptMissionEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    const auto entity = entt::to_entity(event.entity());
    // 自动接续也经过正式接取门禁，不能绕过活动时间和真实进度来源限制。
    const auto result = PlayerMissionSystem::Accept(entity, MissionListComp::kPlayerMission,
        event.mission_id(), TimeSystem::NowMillisecondsUTC());
    if (result != kSuccess) LOG_WARN << "Mission chain accept rejected: mission=" << event.mission_id() << " tip=" << result;
///<<< END WRITING YOUR CODE
}
void MissionEventHandler::ConditionEventHandler(const ConditionEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    const auto entity = entt::to_entity(event.entity());
    if (!tlsEcs.actorRegistry.valid(entity) || PlayerLifecycleSystem::IsCrossZoneFrozen(entity)) return;
    auto* container = tlsEcs.actorRegistry.try_get<MissionsContainerComp>(entity);
    if (container == nullptr) return;
    auto* comp = container->GetMutable(MissionListComp::kPlayerMission);
    if (comp == nullptr) return;
    MissionSystem::HandleConditionEvent(event, *comp, MissionConfig::GetSingleton());
///<<< END WRITING YOUR CODE
}
void MissionEventHandler::OnAcceptedMissionEventHandler(const OnAcceptedMissionEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    // 已有等级事实在 PlayerMissionSystem::Accept 中同步刷新；此事件不重复累计。
///<<< END WRITING YOUR CODE
}
void MissionEventHandler::OnMissionAwardEventHandler(const OnMissionAwardEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    const auto result = PlayerMissionSystem::ClaimReward(entt::to_entity(event.entity()),
        MissionListComp::kPlayerMission, event.mission_id());
    if (result != kSuccess)
        LOG_WARN << "Mission auto reward retained for retry: mission=" << event.mission_id() << " tip=" << result;
///<<< END WRITING YOUR CODE
}

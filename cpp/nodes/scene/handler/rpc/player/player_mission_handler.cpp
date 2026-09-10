
#include "player_mission_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "services/scene/player/system/player_feature_snapshot.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"
///<<< END WRITING YOUR CODE

void SceneMissionClientPlayerHandler::GetMissionList(entt::entity player,const ::GetMissionListRequest* request,
	::GetMissionListResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
const auto result = PlayerMissionReadSystem::BuildList(player, *response);
    if (result != kSuccess) tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(result);
///<<< END WRITING YOUR CODE
}
